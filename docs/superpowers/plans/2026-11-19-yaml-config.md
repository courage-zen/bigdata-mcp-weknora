# weknora-mcp v0.2.0 YAML 配置迁移 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 workspace→KB 配置从 env vars 迁到 YAML 层次结构（`WEKNORA_CONFIG` 指向文件）：全局默认 `api_key` + 逐 workspace 覆盖 + workspace id 前缀匹配（最长胜出），v0.2.0 破坏性发布。

**Architecture:** 重写 `config.go` 的 `LoadConfig`（env 解析 → yaml 文件 + 保留 MCP_HOST/MCP_PORT/S3 的 env 读取）和 `MappingFor`（精确 → 最长前缀）。`Config`/`WorkspaceMapping` 结构体形状不变，工具层/health/启动日志零改动。`main.go` 改传 `WEKNORA_CONFIG` 路径。k8s 侧 config.yaml 进 Secret 挂载。

**Tech Stack:** Go 1.26、`go.yaml.in/yaml/v3`（已在依赖树，gopkg.in/yaml.v3 官方延续包，API 相同）、k8s（82 集群 eda-bigdata）。

**Spec:** `docs/superpowers/specs/2026-11-19-yaml-config-design.md`

**Working style:** 本 repo 沿用直接在 main 上小步提交的既有模式（本会话 339c34f/cdd0cc7 同款）；不建 worktree。

---

### Task 1: YAML schema + LoadConfig 重写（含全新 config_test.go）

**Files:**
- Modify: `config.go`（删 `parseWorkspaceMap`；重写 `LoadConfig`；`Config` 注释更新；`WorkspaceMapping` 注释更新；`MappingFor`/`AgentAllowed`/S3 系列本任务不动）
- Modify: `main.go:16`（签名变更为 `LoadConfig(path string)`，main.go 是 package main 一部分，不随改则全包编不过）
- Rewrite: `config_test.go`（整文件替换为 yaml 版测试）

- [ ] **Step 1: 重写 config_test.go（新测试套件，先写全量失败测试）**

整文件替换 `config_test.go` 为：

```go
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

// baseEnv pins the env the loader still reads (MCP_PORT default guard, S3
// empty) — yaml config goes through writeConfig, not env.
func baseEnv(t *testing.T) {
	t.Helper()
	unsetS3Env(t)
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
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./... 2>&1 | head -20`
Expected: FAIL —— 编译错误（`LoadConfig` 还是无参签名、`writeConfig` 未使用之类）。确认失败原因全部指向 config.go 未改。

- [ ] **Step 3: 重写 config.go 的 weknora 部分**

在 `config.go` 中：删除 `parseWorkspaceMap`（36-71 行整函数）；把 `LoadConfig`（84-133 行）整函数替换为下面的版本；`WorkspaceMapping`/`Config` 注释里对 env var 的引用改为 yaml 字段引用；imports 增加 `"sort"` 和 `"go.yaml.in/yaml/v3"`，删除不再使用的（检查：`strconv` 仍用于 MCP_PORT，保留）。**`splitCSV`/`AgentAllowed`/`MappingFor`/S3 系列全部不动**（splitCSV 仍被 `S3_KEY_PREFIXES` 使用，`MappingFor` 在 Task 2 改）。

在 `WorkspaceMapping` 定义前插入 yaml schema 类型：

```go
// fileConfig is the YAML schema loaded from WEKNORA_CONFIG:
//
//	weknora:
//	  base_url: http://host:28444
//	  api_key: sk-...            # global default, inherited by workspaces
//	  enable_ingest: false
//	  workspaces:
//	    - id: 11111111-2222-...  # full UUID or short prefix (MappingFor
//	                             # prefix-matches the caller's header)
//	      kb_id: 99999999-...
//	      api_key: sk-...        # optional override (cross-tenant KB)
//	      allowed_agent_ids: [prefix]
type fileConfig struct {
	WeKnora struct {
		BaseURL      string `yaml:"base_url"`
		APIKey       string `yaml:"api_key"`
		EnableIngest bool   `yaml:"enable_ingest"`
		Workspaces   []struct {
			ID              string   `yaml:"id"`
			KBID            string   `yaml:"kb_id"`
			APIKey          string   `yaml:"api_key"`
			AllowedAgentIDs []string `yaml:"allowed_agent_ids"`
		} `yaml:"workspaces"`
	} `yaml:"weknora"`
}
```

新 `LoadConfig`（替换旧函数，含注释）：

```go
// LoadConfig reads the YAML config file (path from the caller; empty means
// ./config.yaml), validates it fail-fast, then layers the env-only settings
// (MCP_HOST / MCP_PORT, the S3 contract — the family boundary: deployment
// plumbing stays env, weknora topology lives in the file). A missing or
// unparseable file is a startup error: there is no env fallback for the
// weknora section. Structure errors (empty base_url/workspaces, entry
// fields, duplicates) fire before the key-resolution error so operators fix
// the shape first, then the keys.
func LoadConfig(path string) (*Config, error) {
	if path == "" {
		path = "config.yaml"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var fc fileConfig
	if err := yaml.Unmarshal(data, &fc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	wk := fc.WeKnora
	baseURL := strings.TrimRight(strings.TrimSpace(wk.BaseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("config %s: weknora.base_url is required (e.g. http://your-weknora-host:28444)", path)
	}
	if len(wk.Workspaces) == 0 {
		return nil, fmt.Errorf("config %s: weknora.workspaces must have at least one entry", path)
	}

	cfg := &Config{
		BaseURL:      baseURL,
		Port:         8000,
		EnableIngest: wk.EnableIngest,
		Mappings:     make(map[string]*WorkspaceMapping, len(wk.Workspaces)),
	}

	seenKB := map[string]string{}
	var missingKeys []string
	for i, w := range wk.Workspaces {
		id := strings.TrimSpace(w.ID)
		kb := strings.TrimSpace(w.KBID)
		if id == "" || kb == "" {
			return nil, fmt.Errorf("config %s: workspaces[%d] must set both id and kb_id", path, i)
		}
		if _, dup := cfg.Mappings[id]; dup {
			return nil, fmt.Errorf("config %s: duplicate workspace id %q", path, id)
		}
		if prev, dup := seenKB[kb]; dup {
			return nil, fmt.Errorf("config %s: kb id %q mapped to both %q and %q (1 workspace = 1 KB)", path, kb, prev, id)
		}
		// Key resolution: per-workspace override, else the global default.
		// Neither is a startup error (collected and reported after the
		// structural pass).
		apiKey := strings.TrimSpace(w.APIKey)
		if apiKey == "" {
			apiKey = strings.TrimSpace(wk.APIKey)
		}
		if apiKey == "" {
			missingKeys = append(missingKeys, id)
		}
		cfg.Mappings[id] = &WorkspaceMapping{
			WorkspaceID:     id,
			KBID:            kb,
			APIKey:          apiKey,
			AllowedAgentIDs: w.AllowedAgentIDs,
		}
		seenKB[kb] = id
	}
	if len(missingKeys) > 0 {
		sort.Strings(missingKeys)
		return nil, fmt.Errorf("config %s: workspaces missing api_key (set weknora.api_key or a per-workspace api_key): %s",
			path, strings.Join(missingKeys, ", "))
	}

	cfg.Host = os.Getenv("MCP_HOST")
	if cfg.Host == "" {
		cfg.Host = "0.0.0.0"
	}
	if v := os.Getenv("MCP_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 || p > 65535 {
			return nil, fmt.Errorf("MCP_PORT: %q is not a valid port", v)
		}
		cfg.Port = p
	}
	cfg.S3 = loadS3Config()
	return cfg, nil
}
```

`Config` 结构体注释同步改（`BaseURL` ← `weknora.base_url`，`EnableIngest` ← `weknora.enable_ingest`，Mappings 注释加"keyed by configured id (full UUID or short prefix)"）。

同 commit 改 `main.go:16` 调用（否则 package main 编不过）：

```go
	cfg, err := LoadConfig(os.Getenv("WEKNORA_CONFIG"))
```

（main.go 已 import os，无需改 import。）

- [ ] **Step 4: go mod tidy 并跑测试**

Run: `go mod tidy && go test ./...`
Expected: `go mod tidy` 把 `go.yaml.in/yaml/v3` 从 `// indirect` 提为直接依赖；全部 PASS（83 基线中 config_test 的用例数变了，总量以 `go test` 无 FAIL 为准）。`tools_test.go`/`server_test.go` 的 `testEnv` 直填结构体，不受影响。

- [ ] **Step 5: Commit**

```bash
git add config.go config_test.go main.go go.mod go.sum
git commit -m "feat!: load weknora config from YAML (WEKNORA_CONFIG)

Breaking: WEKNORA_BASE_URL / WEKNORA_WORKSPACE_MAP /
WEKNORA_WORKSPACE_<ws>_API_KEY / WEKNORA_WORKSPACE_<ws>_ALLOWED_AGENT_IDS /
WEKNORA_ENABLE_INGEST env vars are gone. The weknora section now lives in
a YAML file (path from WEKNORA_CONFIG, default ./config.yaml): global
api_key inherited by workspaces with per-workspace override, allowed_agent_ids
per workspace, enable_ingest instance-level. MCP_HOST / MCP_PORT / S3 stay
env (family boundary with email-mcp).

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 2: MappingFor 前缀匹配

**Files:**
- Modify: `config.go:135-139`（`MappingFor`）
- Test: `config_test.go`（追加）

- [ ] **Step 1: 写失败测试（追加到 config_test.go 末尾）**

```go
// TestMappingFor_ExactAndPrefix pins the header-resolution order: exact
// match first, then the longest configured id that is a prefix of the
// caller's workspace id. Prefix matching is one-way (a configured full UUID
// never matches a shorter header).
func TestMappingFor_ExactAndPrefix(t *testing.T) {
	cfg := &Config{Mappings: map[string]*WorkspaceMapping{
		"11111111": {WorkspaceID: "11111111", KBID: "kb-short"},
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
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test -run TestMappingFor_ExactAndPrefix ./...`
Expected: FAIL（当前 `MappingFor` 纯 map 精确查找，prefix 用例全部 miss）。

- [ ] **Step 3: 替换 MappingFor 实现**

```go
// MappingFor resolves the caller's workspace id (X-Multica-Workspace-Id)
// to its mapping: exact match first, then the longest configured id that is
// a prefix of the caller's id (config may abbreviate UUIDs, e.g. "11111111"
// matches the full UUID multica sends; a configured full UUID equals the
// exact path). Prefix matching is one-way: a configured id longer than the
// header never matches.
func (c *Config) MappingFor(workspaceID string) (*WorkspaceMapping, bool) {
	if m, ok := c.Mappings[workspaceID]; ok {
		return m, true
	}
	var best *WorkspaceMapping
	for _, m := range c.Mappings {
		if strings.HasPrefix(workspaceID, m.WorkspaceID) {
			if best == nil || len(m.WorkspaceID) > len(best.WorkspaceID) {
				best = m
			}
		}
	}
	return best, best != nil
}
```

- [ ] **Step 4: 跑全部测试**

Run: `go test ./...`
Expected: 全部 PASS。

- [ ] **Step 5: Commit**

```bash
git add config.go config_test.go
git commit -m "feat: prefix-match workspace ids (longest wins)

Configured ids may be full UUIDs or short prefixes of the
X-Multica-Workspace-Id header (pg-mcp ALLOWED_WORKSPACE_IDS convention).
Exact match takes priority; among prefixes the longest wins.

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 3: 全量构建核验 + 无残留 env 引用

**Files:**
- Verify: 无残留 env 引用（main.go 调用点已随 Task 1 同 commit 改掉）

- [ ] **Step 1: 确认无残留 WEKNORA_ env 引用**

Run: `grep -rn "WEKNORA_" *.go | grep -v _test`
Expected: 仅 main.go 一处 `WEKNORA_CONFIG`。若出现 `WEKNORA_BASE_URL`/`WEKNORA_WORKSPACE` 等残留，逐处清理（README 与 `.k8s/` 在后续任务处理，不算残留）。

- [ ] **Step 2: 全量测试 + 构建 + vet**

Run: `go vet ./... && go test ./... && go build -o /tmp/weknora-mcp-v020 . && /tmp/weknora-mcp-v020 2>&1 | head -2`
Expected: vet 干净、测试全 PASS、二进制启动报 `read config config.yaml: ...` 退出（无配置文件时的 fail-fast，正是预期行为）。

（main.go 调用点已随 Task 1 同 commit 改掉，本任务无代码改动，无 commit。）

---

### Task 4: README 配置章节重写

**Files:**
- Modify: `README.md`（配置表、部署示例；行 18-32 配置节 + 行 95-102 部署节）

- [ ] **Step 1: 重写 README 配置节**

把"## 配置"整节（环境变量表 + S3 段落）替换为：

```markdown
## 配置

### 配置文件（WEKNORA_CONFIG，默认 ./config.yaml）

workspace→KB 拓扑全部在 YAML 文件里（无 env 兜底，文件缺失/不合法 = 启动失败）：

```yaml
weknora:
  base_url: http://your-weknora-host:28444   # 必填
  api_key: sk-...                             # 全局默认（单租户部署只配这个）
  enable_ingest: false                        # 写入工具开关，默认 false

  workspaces:                                 # ≥1 条
    - id: 11111111-2222-3333-4444-555555555555  # 全 UUID 或短前缀均可
      kb_id: 99999999-8888-7777-6666-555555555555
      # api_key: 省略 → 继承全局；映射跨租户 KB 时覆盖
      # allowed_agent_ids: [agent-uuid-prefix]  # 可选，前缀匹配
```

- **key 解析**：workspace 生效 key = 自身 `api_key`，缺省继承全局；两者皆无 → 启动报错并列出所有缺 key 的 workspace
- **id 匹配**：header `X-Multica-Workspace-Id` 精确匹配优先；无精确命中取最长前缀（配置 `11111111` 匹配全 UUID）
- **校验**：`base_url`/`workspaces`/条目字段空、id 重复、同一 KB 映射多个 workspace → 启动失败（1 workspace = 1 KB）

### 环境变量

| 环境变量 | 必填 | 说明 |
| --- | --- | --- |
| WEKNORA_CONFIG | 否 | 配置文件路径，默认 ./config.yaml |
| MCP_AUTH_KEY | 否 | Bearer key，逗号分隔；空 = 无认证 |
| MCP_HOST / MCP_PORT | 否 | 默认 0.0.0.0:8000 |

S3（env-only，add_document 的 s3_key 源）：S3_BUCKET / AWS_ACCESS_KEY_ID /
AWS_SECRET_ACCESS_KEY（三缺一 = 软禁用）+ AWS_ENDPOINT_URL / S3_REGION /
S3_USE_PATH_STYLE / S3_KEY_PREFIXES / S3_MAX_FILE_BYTES。
```

- [ ] **Step 2: 更新部署示例（docker run 挂文件）**

部署节的 docker run 示例替换为：

```bash
docker run -d -p 8000:8000 \
  -v $PWD/config.yaml:/app/config.yaml \
  -e WEKNORA_CONFIG=/app/config.yaml \
  -e MCP_AUTH_KEY=bearer-key \
  ghcr.io/courage-zen/bigdata-mcp-weknora:v0.2.0
```

并在镜像行之前加一句：`配置文件含 api_key（敏感），挂载时注意权限；k8s 场景放 Secret。`

- [ ] **Step 3: 核对 README 其余部分**

Run: `grep -n "WEKNORA_WORKSPACE_MAP\|WEKNORA_BASE_URL\|WEKNORA_ENABLE_INGEST\|WEKNORA_WORKSPACE_" README.md`
Expected: 无输出（全部迁移到 yaml 描述）。工具说明节提到的 `WEKNORA_ENABLE_INGEST=true` 字样若有，改为 `enable_ingest: true`。

- [ ] **Step 4: Commit**

```bash
git add README.md
git commit -m "docs: README for YAML config (v0.2.0)

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 5: 发布 v0.2.0

- [ ] **Step 1: tag + push（触发 release CI）**

```bash
git tag v0.2.0 && git push origin main v0.2.0
```

- [ ] **Step 2: 等 CI 绿 + 产物核验**

Run: `gh run watch --repo courage-zen/bigdata-mcp-weknora $(gh run list --repo courage-zen/bigdata-mcp-weknora --limit 1 --json databaseId -q '.[0].databaseId')`（后台跑，注意 2 分钟超时，分段查）
Expected: release workflow 全绿；Release 附 linux 静态二进制 + 镜像 tar + checksums；GHCR `ghcr.io/courage-zen/bigdata-mcp-weknora:v0.2.0` 可匿名拉取。

- [ ] **Step 3: 镜像同步 <internal-registry>**

先查本地 docker 是否已登录 <internal-registry>：`docker pull ghcr.io/courage-zen/bigdata-mcp-weknora:v0.2.1-arm64` 不存在——正确源 tag 是 `v0.2.0-arm64`：

```bash
docker pull ghcr.io/courage-zen/bigdata-mcp-weknora:v0.2.0-arm64 && \
docker tag ghcr.io/courage-zen/bigdata-mcp-weknora:v0.2.0-arm64 <internal-registry>/eda-bigdata/weknora-mcp:v0.2.0-arm64 && \
docker push <internal-registry>/eda-bigdata/weknora-mcp:v0.2.0-arm64
```

若未登录（push 401），请用户执行 `docker login <internal-registry>`（交互式，建议用户输入 `! docker login <internal-registry>`），或沿用 v0.1.1 的用户手动 push 流程。

---

### Task 6: 82 集群部署迁移 + 文档

**Files:**
- Rewrite: `.k8s/configmap.yaml`、`.k8s/secret.yaml`、`.k8s/deployment.yaml`（本地副本）
- Rewrite: 82 `~/config/weknora-mcp/how-to-config.md`
- Update: memory `weknora-mcp-project-scope.md`

- [ ] **Step 1: 重写 .k8s 三清单**

`.k8s/configmap.yaml`（只剩监听设置）：

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: weknora-mcp-config
  namespace: eda-bigdata
  labels:
    app: weknora-mcp
data:
  MCP_HOST: "0.0.0.0"
  MCP_PORT: "8000"
  WEKNORA_CONFIG: /app/config/config.yaml
```

`.k8s/secret.yaml`（MCP_AUTH_KEY env + config.yaml 文件键；82 上用 sed 填真值，本地留占位符）：

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: weknora-mcp-secret
  namespace: eda-bigdata
  labels:
    app: weknora-mcp
type: Opaque
stringData:
  MCP_AUTH_KEY: "REPLACE_ME_MCP_AUTH_KEY"
  # weknora 拓扑（含 sk- api_key，敏感所以进 Secret 而非 ConfigMap）。
  # id 用 8 位前缀 11111111（v0.2.0 前缀匹配），multica 传全 UUID 也能路由。
  config.yaml: |
    weknora:
      base_url: http://your-weknora-host:28444
      api_key: REPLACE_ME_WEKNORA_API_KEY
      enable_ingest: false
      workspaces:
        - id: 11111111
          kb_id: 99999999-8888-7777-6666-555555555555
```

`.k8s/deployment.yaml`：镜像改 `<internal-registry>/eda-bigdata/weknora-mcp:v0.2.0-arm64`；envFrom 保持 configmap+secret；`MCP_AUTH_KEY` 从 secretRef envFrom 自动注入（stringData 键即 env 名，config.yaml 键不会成为 env——它通过 volume 挂载）；追加 volume 块（containers 之后、restartPolicy 之前）：

```yaml
        volumeMounts:
        - name: config-file
          mountPath: /app/config
      volumes:
      - name: config-file
        secret:
          secretName: weknora-mcp-secret
```

注意：secret 里 `config.yaml` 作为文件键挂载时，`MCP_AUTH_KEY` 这个 stringData 键也会以文件形式出现在 /app/config/ 下（`MCP_AUTH_KEY` 文件）——无害但难看。若要干净，把 config.yaml 拆成独立 Secret `weknora-mcp-config-file`（对齐 email-mcp 的 email-mcp-config-file 模式），volume 引用它。**采用后者**：secret.yaml 只留 MCP_AUTH_KEY；新建 `.k8s/configfile-secret.yaml` 存 config.yaml；deployment volume 引用 `weknora-mcp-config-file`。

- [ ] **Step 2: 上传 82 并填真值**

```bash
scp .k8s/{configmap.yaml,secret.yaml,configfile-secret.yaml,deployment.yaml,service.yaml} 82:~/config/weknora-mcp/
ssh 82 'cd ~/config/weknora-mcp
KEY=$(cat /tmp/wk_apikey.txt)   # v0.1.1 部署时存的 WeKnora API key（仍有效）
sed -i "s|REPLACE_ME_WEKNORA_API_KEY|$KEY|" configfile-secret.yaml
# MCP_AUTH_KEY 沿用现有值（kubectl 里读出来原样填，不轮换）
export KUBECONFIG=~/.kube/config
kubectl -n eda-bigdata get secret weknora-mcp-secret -o jsonpath="{.data.MCP_AUTH_KEY}" | base64 -d > /tmp/mcp_auth_key.txt
sed -i "s|REPLACE_ME_MCP_AUTH_KEY|$(cat /tmp/mcp_auth_key.txt)|" secret.yaml'
```

- [ ] **Step 3: 应用 + 滚动 + 日志核验**

```bash
ssh 82 'export KUBECONFIG=~/.kube/config
kubectl apply -f ~/config/weknora-mcp/configmap.yaml -f ~/config/weknora-mcp/secret.yaml -f ~/config/weknora-mcp/configfile-secret.yaml -f ~/config/weknora-mcp/deployment.yaml
kubectl -n eda-bigdata rollout status deploy/weknora-mcp
kubectl -n eda-bigdata logs deploy/weknora-mcp --tail=1'
```

Expected: 日志 `weknora-mcp listening on 0.0.0.0:8000 (weknora http://your-weknora-host:28444, 1 workspace mappings, ingest false)`。

- [ ] **Step 4: 端到端验证（集群内 pod，含前缀路由）**

```bash
ssh 82 'export KUBECONFIG=~/.kube/config
POD=$(kubectl -n eda-bigdata get pods -l app=bigdata-mcp -o jsonpath="{.items[0].metadata.name}")
AUTH=$(kubectl -n eda-bigdata get secret weknora-mcp-secret -o jsonpath="{.data.MCP_AUTH_KEY}" | base64 -d)
# health
kubectl -n eda-bigdata exec $POD -- curl -s http://weknora-mcp.eda-bigdata.svc.<cluster-domain>:8000/health
# 全 UUID header → 8 位前缀配置命中（前缀匹配生产验证）
kubectl -n eda-bigdata exec $POD -- curl -s -X POST http://weknora-mcp.eda-bigdata.svc.<cluster-domain>:8000/mcp \
  -H "Content-Type: application/json" -H "Accept: application/json, text/event-stream" \
  -H "Authorization: Bearer $AUTH" \
  -H "X-Multica-Workspace-Id: 11111111-2222-3333-4444-555555555555" \
  -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"list_knowledge_bases\",\"arguments\":{}}}" | head -c 300'
```

Expected: health `workspaces:1`；list_knowledge_bases `success:true` 且 multica_context 回显全 UUID（证明 8 位前缀配置路由到了全 UUID header）。

- [ ] **Step 5: 重写 82 的 how-to-config.md + 本地 .k8s 副本同步**

how-to-config.md 重写要点：配置位置表（configmap/secret/configfile-secret 三者分工）、新 KB 接入流程改为"改 configfile-secret.yaml 的 config.yaml → kubectl apply → rollout restart"、env 速查表删 WEKNORA_* 加 WEKNORA_CONFIG、id 可用前缀、运维备忘补 v0.2.0 迁移记录。改完 `scp` 到 82。

- [ ] **Step 6: 更新项目 memory**

`weknora-mcp-project-scope.md` 追加：v0.2.0 yaml 配置迁移（2026-11-19）——WEKNORA_CONFIG 文件、全局 key+覆盖、前缀匹配已实现（原"前 8 位待办"完成）、82 部署三清单结构。

---

## Self-Review 记录

- **Spec 覆盖**：yaml schema（Task 1）、全局 key+覆盖（Task 1）、缺 key fail-fast 列全（Task 1）、结构校验全集（Task 1）、前缀匹配最长胜出（Task 2）、env 边界 MCP_HOST/PORT/S3/MCP_AUTH_KEY（Task 1/3）、main.go 调用点（Task 3）、README（Task 4）、发布与镜像（Task 5）、82 部署+how-to+memory（Task 6）——无缺口
- **占位符**：无 TBD/TODO；所有代码步骤含完整代码
- **类型一致**：`fileConfig` 嵌套字段名（BaseURL/APIKey/EnableIngest/Workspaces/ID/KBID/AllowedAgentIDs）与 LoadConfig 消费一致；`LoadConfig(path string)` 签名在 Task 1/2/3 一致；`writeConfig` 助手与全部用例一致
