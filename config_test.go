package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig writes a YAML config file into a fresh temp dir and returns
// its path. Every loader test goes through this: the loader is file-first
// with no env fallback, so fixtures must be files.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// unsetS3Env pins every S3-related variable to empty so ambient
// developer/deployment env can't leak into a test. Empty string is
// equivalent to unset for every consumer in config.go (s3Env's fallback and
// the `!= ""` / TrimSpace chains all treat "" as unset).
func unsetS3Env(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"S3_BUCKET",
		"AWS_ACCESS_KEY_ID", "S3_ACCESS_KEY",
		"AWS_SECRET_ACCESS_KEY", "S3_SECRET_KEY",
		"AWS_ENDPOINT_URL", "S3_ENDPOINT", "S3_SECURE",
		"S3_REGION", "AWS_REGION",
		"S3_USE_PATH_STYLE",
		"S3_KEY_PREFIXES",
		"S3_MAX_FILE_BYTES",
	} {
		t.Setenv(name, "")
	}
}

// baseEnv pins the env the loader still reads (MCP_HOST/MCP_PORT default
// guards, S3 empty) — yaml config goes through writeConfig, not env.
func baseEnv(t *testing.T) {
	t.Helper()
	unsetS3Env(t)
	t.Setenv("MCP_HOST", "")
	t.Setenv("MCP_PORT", "")
}

// minimalYAML is the smallest valid config: global key inherited by one
// workspace.
const minimalYAML = `weknora:
  base_url: http://weknora.test
  api_key: wk-global
  workspaces:
    - id: ws-1
      kb_id: kb-1
`

func TestLoadConfig_MinimalValid(t *testing.T) {
	baseEnv(t)
	cfg, err := LoadConfig(writeConfig(t, minimalYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != "http://weknora.test" {
		t.Errorf("BaseURL = %q", cfg.BaseURL)
	}
	if cfg.Port != 8000 || cfg.Host != "0.0.0.0" {
		t.Errorf("host/port defaults: %q %d", cfg.Host, cfg.Port)
	}
	if cfg.EnableIngest {
		t.Error("ingest must default off")
	}
	m, ok := cfg.MappingFor("ws-1")
	if !ok || m.KBID != "kb-1" || m.APIKey != "wk-global" {
		t.Fatalf("mapping: %+v ok=%v", m, ok)
	}
	if !m.AgentAllowed("any-agent") {
		t.Error("empty allowlist must allow any agent")
	}
}

func TestLoadConfig_PerWorkspaceKeyOverridesGlobal(t *testing.T) {
	baseEnv(t)
	path := writeConfig(t, `weknora:
  base_url: http://weknora.test
  api_key: wk-global
  workspaces:
    - id: ws-1
      kb_id: kb-1
      api_key: wk-own
    - id: ws-2
      kb_id: kb-2
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := cfg.MappingFor("ws-1"); m.APIKey != "wk-own" {
		t.Errorf("ws-1 key = %q, want override wk-own", m.APIKey)
	}
	if m, _ := cfg.MappingFor("ws-2"); m.APIKey != "wk-global" {
		t.Errorf("ws-2 key = %q, want inherited wk-global", m.APIKey)
	}
}

func TestLoadConfig_MissingKeyListsAllWorkspaces(t *testing.T) {
	baseEnv(t)
	// No global key; ws-2 has its own, ws-1 does not → error names ws-1 only.
	path := writeConfig(t, `weknora:
  base_url: http://weknora.test
  workspaces:
    - id: ws-1
      kb_id: kb-1
    - id: ws-2
      kb_id: kb-2
      api_key: wk-b
`)
	_, err := LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "ws-1") || strings.Contains(err.Error(), "ws-2") {
		t.Fatalf("want missing-key error naming ws-1 only, got %v", err)
	}
	// A third workspace also missing → both listed (one-pass operator fix).
	path = writeConfig(t, `weknora:
  base_url: http://weknora.test
  workspaces:
    - id: ws-1
      kb_id: kb-1
    - id: ws-3
      kb_id: kb-3
`)
	_, err = LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "ws-1") || !strings.Contains(err.Error(), "ws-3") {
		t.Fatalf("want missing-key error naming ws-1 and ws-3, got %v", err)
	}
}

func TestLoadConfig_MissingBaseURL(t *testing.T) {
	baseEnv(t)
	_, err := LoadConfig(writeConfig(t, `weknora:
  api_key: wk
  workspaces:
    - id: ws-1
      kb_id: kb-1
`))
	if err == nil || !strings.Contains(err.Error(), "base_url") {
		t.Fatalf("want base_url error, got %v", err)
	}
}

func TestLoadConfig_EmptyWorkspaces(t *testing.T) {
	baseEnv(t)
	_, err := LoadConfig(writeConfig(t, `weknora:
  base_url: http://weknora.test
  api_key: wk
`))
	if err == nil || !strings.Contains(err.Error(), "workspaces") {
		t.Fatalf("want workspaces error, got %v", err)
	}
}

func TestLoadConfig_EntryMissingFields(t *testing.T) {
	baseEnv(t)
	_, err := LoadConfig(writeConfig(t, `weknora:
  base_url: http://weknora.test
  api_key: wk
  workspaces:
    - id: ws-1
`))
	if err == nil || !strings.Contains(err.Error(), "workspaces[0]") {
		t.Fatalf("want entry error, got %v", err)
	}
}

func TestLoadConfig_DuplicateID(t *testing.T) {
	baseEnv(t)
	_, err := LoadConfig(writeConfig(t, `weknora:
  base_url: http://weknora.test
  api_key: wk
  workspaces:
    - id: ws-1
      kb_id: kb-1
    - id: ws-1
      kb_id: kb-2
`))
	if err == nil || !strings.Contains(err.Error(), "duplicate workspace id") {
		t.Fatalf("want dup-id error, got %v", err)
	}
}

func TestLoadConfig_DuplicateKB(t *testing.T) {
	baseEnv(t)
	_, err := LoadConfig(writeConfig(t, `weknora:
  base_url: http://weknora.test
  api_key: wk
  workspaces:
    - id: ws-1
      kb_id: kb-1
    - id: ws-2
      kb_id: kb-1
`))
	if err == nil || !strings.Contains(err.Error(), "1 workspace = 1 KB") {
		t.Fatalf("want dup-kb error, got %v", err)
	}
}

func TestLoadConfig_BadYAML(t *testing.T) {
	baseEnv(t)
	_, err := LoadConfig(writeConfig(t, "weknora: [unclosed\n"))
	if err == nil || !strings.Contains(err.Error(), "parse ") {
		t.Fatalf("want parse error, got %v", err)
	}
}

func TestLoadConfig_MissingFile(t *testing.T) {
	baseEnv(t)
	_, err := LoadConfig(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil || !strings.Contains(err.Error(), "read config") {
		t.Fatalf("want read error, got %v", err)
	}
}

func TestLoadConfig_DefaultPath(t *testing.T) {
	baseEnv(t)
	t.Chdir(t.TempDir()) // cwd has no config.yaml
	_, err := LoadConfig("")
	if err == nil || !strings.Contains(err.Error(), "config.yaml") || !strings.Contains(err.Error(), "WEKNORA_CONFIG") {
		t.Fatalf("want default-path read error hinting WEKNORA_CONFIG, got %v", err)
	}
}

func TestLoadConfig_BaseURLTrailingSlash(t *testing.T) {
	baseEnv(t)
	cfg, err := LoadConfig(writeConfig(t, `weknora:
  base_url: http://weknora.test/
  api_key: wk
  workspaces:
    - id: ws-1
      kb_id: kb-1
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != "http://weknora.test" {
		t.Fatalf("BaseURL = %q", cfg.BaseURL)
	}
}

func TestLoadConfig_EnableIngest(t *testing.T) {
	baseEnv(t)
	cfg, err := LoadConfig(writeConfig(t, `weknora:
  base_url: http://weknora.test
  api_key: wk
  enable_ingest: true
  workspaces:
    - id: ws-1
      kb_id: kb-1
`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.EnableIngest {
		t.Fatal("EnableIngest = false")
	}
}

func TestLoadConfig_InvalidPort(t *testing.T) {
	baseEnv(t)
	t.Setenv("MCP_PORT", "99999")
	_, err := LoadConfig(writeConfig(t, minimalYAML))
	if err == nil || !strings.Contains(err.Error(), "MCP_PORT") {
		t.Fatalf("want port error, got %v", err)
	}
}

func TestLoadConfig_AllowlistFromYAML(t *testing.T) {
	baseEnv(t)
	cfg, err := LoadConfig(writeConfig(t, `weknora:
  base_url: http://weknora.test
  api_key: wk
  workspaces:
    - id: ws-1
      kb_id: kb-1
      allowed_agent_ids: [ag-1, ag-2]
`))
	if err != nil {
		t.Fatal(err)
	}
	m, _ := cfg.MappingFor("ws-1")
	if len(m.AllowedAgentIDs) != 2 || m.AllowedAgentIDs[0] != "ag-1" || m.AllowedAgentIDs[1] != "ag-2" {
		t.Fatalf("allowlist: %v", m.AllowedAgentIDs)
	}
	// Gating under the pinned prefix-match semantics (see
	// TestAgentAllowed_PrefixMatch): "ag-1-x" passes via prefix "ag-1";
	// "ag-9" matches no configured prefix.
	if m.AgentAllowed("ag-9") {
		t.Error("allowlist gating wrong")
	}
	if !m.AgentAllowed("ag-1-x") {
		t.Error("allowlist prefix match must accept ag-1-x")
	}
}

func TestLoadConfig_AllowlistSanitized(t *testing.T) {
	baseEnv(t)
	cfg, err := LoadConfig(writeConfig(t, `weknora:
  base_url: http://weknora.test
  api_key: wk
  workspaces:
    - id: ws-1
      kb_id: kb-1
      allowed_agent_ids: ["", " ag-1 ", "ag-2"]
`))
	if err != nil {
		t.Fatal(err)
	}
	m, _ := cfg.MappingFor("ws-1")
	if len(m.AllowedAgentIDs) != 2 || m.AllowedAgentIDs[0] != "ag-1" || m.AllowedAgentIDs[1] != "ag-2" {
		t.Fatalf("allowlist after sanitize: %v", m.AllowedAgentIDs)
	}
	if !m.AgentAllowed("ag-1-x") {
		t.Error("trimmed prefix must match")
	}
	if m.AgentAllowed("ag-9") {
		t.Error("empty entry must not fail open")
	}
}

func TestLoadConfig_UnknownFieldRejected(t *testing.T) {
	baseEnv(t)
	_, err := LoadConfig(writeConfig(t, `weknora:
  base_url: http://weknora.test
  api_key: wk
  workspaces:
    - id: ws-1
      kb_id: kb-1
      allowed_agents_ids: [ag-1]
`))
	if err == nil || !strings.Contains(err.Error(), "parse ") || !strings.Contains(err.Error(), "allowed_agents_ids") {
		t.Fatalf("want parse error naming the unknown field, got %v", err)
	}
}

func TestLoadConfig_S3TrioIncomplete(t *testing.T) {
	// baseEnv pins the whole S3 env empty, so S3_BUCKET alone completes
	// part of the trio → soft disable.
	baseEnv(t)
	t.Setenv("S3_BUCKET", "b")
	cfg, err := LoadConfig(writeConfig(t, minimalYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.S3 != nil {
		t.Fatal("partial trio must soft-disable S3")
	}
}

func TestLoadConfig_S3Complete(t *testing.T) {
	baseEnv(t)
	t.Setenv("S3_BUCKET", "b")
	t.Setenv("AWS_ACCESS_KEY_ID", "ak")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "sk")
	t.Setenv("AWS_ENDPOINT_URL", "http://minio.test:9000")
	cfg, err := LoadConfig(writeConfig(t, minimalYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.S3 == nil || cfg.S3.Bucket != "b" || cfg.S3.EndpointURL != "http://minio.test:9000" {
		t.Fatalf("s3: %+v", cfg.S3)
	}
	// EndpointURL set, no explicit S3_USE_PATH_STYLE → path-style inferred on
	if !cfg.S3.UsePathStyle || cfg.S3.PathStyleExplicit {
		t.Errorf("path-style inference: UsePathStyle=%v explicit=%v, want true/false",
			cfg.S3.UsePathStyle, cfg.S3.PathStyleExplicit)
	}
	if cfg.S3.MaxFileBytes != 100*1024*1024 {
		t.Errorf("MaxFileBytes = %d", cfg.S3.MaxFileBytes)
	}
}

func TestAgentAllowed_PrefixMatch(t *testing.T) {
	m := &WorkspaceMapping{AllowedAgentIDs: []string{"abc123", "zzz"}}
	if !m.AgentAllowed("abc123-def-456") {
		t.Error("prefix match must pass")
	}
	if m.AgentAllowed("abc124-def") {
		t.Error("non-matching prefix must fail")
	}
	if m.AgentAllowed("") {
		t.Error("empty agent id must fail against a non-empty allowlist")
	}
}

// TestMappingFor_ExactAndPrefix pins the header-resolution order: exact
// match first, then the longest configured id that is a prefix of the
// caller's workspace id. Prefix matching is one-way (a configured full UUID
// never matches a shorter header).
func TestMappingFor_ExactAndPrefix(t *testing.T) {
	cfg := &Config{Mappings: map[string]*WorkspaceMapping{
		"11111111":                             {WorkspaceID: "11111111", KBID: "kb-short"},
		"11111111-2222-3333-4444-555555555555": {WorkspaceID: "11111111-2222-3333-4444-555555555555", KBID: "kb-full"},
	}}
	// Full UUID header: exact match beats the same-id prefix entry.
	m, ok := cfg.MappingFor("11111111-2222-3333-4444-555555555555")
	if !ok || m.KBID != "kb-full" {
		t.Fatalf("exact match: %+v ok=%v", m, ok)
	}
	// A different UUID sharing the 8-char prefix: only the short entry
	// matches.
	m, ok = cfg.MappingFor("11111111-ffffffff-ffff-ffff-ffffffffffff")
	if !ok || m.KBID != "kb-short" {
		t.Fatalf("prefix match: %+v ok=%v", m, ok)
	}
	// Longest prefix wins when several configured ids are prefixes.
	cfg2 := &Config{Mappings: map[string]*WorkspaceMapping{
		"1111":     {WorkspaceID: "1111", KBID: "kb-4"},
		"11111111": {WorkspaceID: "11111111", KBID: "kb-8"},
	}}
	m, ok = cfg2.MappingFor("11111111-2222")
	if !ok || m.KBID != "kb-8" {
		t.Fatalf("longest prefix: %+v ok=%v", m, ok)
	}
	// Unmapped id resolves to nothing.
	if _, ok := cfg.MappingFor("ws-other"); ok {
		t.Fatal("unmapped id must not resolve")
	}
	// One-way: a configured full UUID is never a prefix of a shorter header.
	cfg3 := &Config{Mappings: map[string]*WorkspaceMapping{
		"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee": {WorkspaceID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", KBID: "kb-x"},
	}}
	if _, ok := cfg3.MappingFor("aaaaaaaa"); ok {
		t.Fatal("config id longer than header must not match")
	}
}
