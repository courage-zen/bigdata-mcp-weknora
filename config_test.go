package main

import (
	"strings"
	"testing"
)

func baseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("WEKNORA_BASE_URL", "http://weknora.test")
}

func TestLoadConfig_MinimalValid(t *testing.T) {
	baseEnv(t)
	t.Setenv("WEKNORA_WORKSPACE_MAP", "ws-1:kb-1")
	t.Setenv("WEKNORA_WORKSPACE_ws-1_API_KEY", "wk-aaa")
	cfg, err := LoadConfig()
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
	if !ok || m.KBID != "kb-1" || m.APIKey != "wk-aaa" {
		t.Fatalf("mapping: %+v ok=%v", m, ok)
	}
	if !m.AgentAllowed("any-agent") {
		t.Error("empty allowlist must allow any agent")
	}
}

func TestLoadConfig_MissingBaseURL(t *testing.T) {
	t.Setenv("WEKNORA_BASE_URL", "")
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "WEKNORA_BASE_URL") {
		t.Fatalf("want BASE_URL error, got %v", err)
	}
}

func TestLoadConfig_EmptyMap(t *testing.T) {
	baseEnv(t)
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "WEKNORA_WORKSPACE_MAP") {
		t.Fatalf("want MAP error, got %v", err)
	}
}

func TestLoadConfig_MalformedEntry(t *testing.T) {
	baseEnv(t)
	t.Setenv("WEKNORA_WORKSPACE_MAP", "ws-1")
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "entry 1") {
		t.Fatalf("want entry error, got %v", err)
	}
}

func TestLoadConfig_DuplicateWorkspaceAndKB(t *testing.T) {
	baseEnv(t)
	t.Setenv("WEKNORA_WORKSPACE_MAP", "ws-1:kb-1,ws-1:kb-2")
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "duplicate workspace") {
		t.Fatalf("want dup-ws error, got %v", err)
	}
	t.Setenv("WEKNORA_WORKSPACE_MAP", "ws-1:kb-1,ws-2:kb-1")
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "1 workspace = 1 KB") {
		t.Fatalf("want dup-kb error, got %v", err)
	}
}

func TestLoadConfig_MissingAPIKeyListsAll(t *testing.T) {
	baseEnv(t)
	t.Setenv("WEKNORA_WORKSPACE_MAP", "ws-1:kb-1,ws-2:kb-2")
	t.Setenv("WEKNORA_WORKSPACE_ws-2_API_KEY", "wk-bbb")
	_, err := LoadConfig()
	if err == nil || !strings.Contains(err.Error(), "WEKNORA_WORKSPACE_ws-1_API_KEY") {
		t.Fatalf("want missing-key error naming ws-1, got %v", err)
	}
}

func TestLoadConfig_BaseURLTrailingSlash(t *testing.T) {
	t.Setenv("WEKNORA_BASE_URL", "http://weknora.test/")
	t.Setenv("WEKNORA_WORKSPACE_MAP", "ws-1:kb-1")
	t.Setenv("WEKNORA_WORKSPACE_ws-1_API_KEY", "wk-a")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != "http://weknora.test" {
		t.Fatalf("BaseURL = %q", cfg.BaseURL)
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

func TestLoadConfig_EnableIngest(t *testing.T) {
	baseEnv(t)
	t.Setenv("WEKNORA_WORKSPACE_MAP", "ws-1:kb-1")
	t.Setenv("WEKNORA_WORKSPACE_ws-1_API_KEY", "wk-a")
	t.Setenv("WEKNORA_ENABLE_INGEST", "true")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.EnableIngest {
		t.Fatal("EnableIngest = false")
	}
}

func TestLoadConfig_MultiColonRejected(t *testing.T) {
	baseEnv(t)
	t.Setenv("WEKNORA_WORKSPACE_MAP", "ws-1:kb-1:typo")
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "entry 1") {
		t.Fatalf("want entry error, got %v", err)
	}
}

func TestLoadConfig_InvalidPort(t *testing.T) {
	baseEnv(t)
	t.Setenv("WEKNORA_WORKSPACE_MAP", "ws-1:kb-1")
	t.Setenv("WEKNORA_WORKSPACE_ws-1_API_KEY", "wk-a")
	t.Setenv("MCP_PORT", "99999")
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "MCP_PORT") {
		t.Fatalf("want port error, got %v", err)
	}
}

func TestLoadConfig_AllowlistEnv(t *testing.T) {
	baseEnv(t)
	t.Setenv("WEKNORA_WORKSPACE_MAP", "ws-1:kb-1")
	t.Setenv("WEKNORA_WORKSPACE_ws-1_API_KEY", "wk-a")
	t.Setenv("WEKNORA_WORKSPACE_ws-1_ALLOWED_AGENT_IDS", "ag-1, ag-2")
	cfg, err := LoadConfig()
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

func TestLoadConfig_S3TrioIncomplete(t *testing.T) {
	// Pin the AWS creds empty: a developer shell may export them, and an
	// empty value reads as unset for the trio's presence checks.
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	baseEnv(t)
	t.Setenv("WEKNORA_WORKSPACE_MAP", "ws-1:kb-1")
	t.Setenv("WEKNORA_WORKSPACE_ws-1_API_KEY", "wk-a")
	t.Setenv("S3_BUCKET", "b")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.S3 != nil {
		t.Fatal("partial trio must soft-disable S3")
	}
}

func TestLoadConfig_S3Complete(t *testing.T) {
	baseEnv(t)
	t.Setenv("WEKNORA_WORKSPACE_MAP", "ws-1:kb-1")
	t.Setenv("WEKNORA_WORKSPACE_ws-1_API_KEY", "wk-a")
	t.Setenv("S3_BUCKET", "b")
	t.Setenv("AWS_ACCESS_KEY_ID", "ak")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "sk")
	t.Setenv("AWS_ENDPOINT_URL", "http://minio.test:9000")
	cfg, err := LoadConfig()
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
