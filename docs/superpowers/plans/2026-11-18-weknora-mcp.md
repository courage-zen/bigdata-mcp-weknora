# weknora-mcp Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Go MCP server bridging multica agents to WeKnora 0.9.0 knowledge bases via REST API, with static workspace→KB mapping and per-mapping API keys.

**Architecture:** Stateless Streamable-HTTP MCP server (email-mcp pattern). Each tool call extracts `X-Multica-Workspace-Id`, routes through the mapping table to a (kb_id, api_key) pair, and calls the WeKnora REST API with that key. 10 tools total: 7 read-only (list/search/read + wiki), 3 ingest behind `WEKNORA_ENABLE_INGEST`.

**Tech Stack:** Go 1.26, `github.com/modelcontextprotocol/go-sdk` v1.8.0 (mcp), `github.com/minio/minio-go/v7` v7.2.1 (S3), stdlib `net/http` (WeKnora client).

**Spec:** `docs/superpowers/specs/2026-11-18-weknora-mcp-design.md`

**Source repos for reference (read-only):**
- email-mcp pattern source: `/Users/zhengyong/code/bigdata-mcp-email/` (middleware.go, identity.go, s3.go, s3keys.go, server.go, main.go, Makefile, Dockerfile, .github/workflows/)
- WeKnora API shapes: `/Users/zhengyong/code/WeKnora-0.9.0/` (internal/handler/, internal/types/, internal/router/)

**WeKnora REST endpoints used (all under `WEKNORA_BASE_URL/api/v1`, auth `X-API-Key: <per-mapping key>`):**

| Purpose | Method + Path | Notes |
| --- | --- | --- |
| List KBs | `GET /knowledge-bases` | resp `{"success":true,"data":[KB...]}` |
| Search | `POST /knowledge-search` | body `SearchKnowledgeRequest`; resp `{"success":true,"data":[SearchResult...],"meta":{...}}` |
| List docs | `GET /knowledge-bases/{kb}/knowledge?page=&page_size=&keyword=&parse_status=&file_type=&sort_by=&sort_order=` | resp `{"success":true,"data":[Knowledge...],"total":N,"page":N,"page_size":N}` |
| Read chunks | `GET /chunks/{knowledge_id}?page=&page_size=&chunk_type=text` | resp same pagination shape; chunk_type repeatable |
| Wiki search | `GET /knowledgebase/{kb}/wiki/search?q=&limit=` | resp `{"pages":[...]}`; **note `knowledgebase` singular** |
| Wiki page | `GET /knowledgebase/{kb}/wiki/pages/{slug}` | slug is catch-all (`entity/rag` → `/pages/entity/rag`); resp `WikiPageDetail` |
| Wiki index | `GET /knowledgebase/{kb}/wiki/index?limit=` | resp `WikiIndexResponse` |
| Add manual | `POST /knowledge-bases/{kb}/manual` | body `{title, content, status, channel:"mcp"}`; resp `{"success":true,"data":Knowledge}` |
| Add from URL | `POST /knowledge-bases/{kb}/url` | body `{url, title, channel:"mcp"}`; resp same |
| Add from file | `POST /knowledge-bases/{kb}/file` | multipart: `file` + `fileName` + `channel=mcp`; resp same |
| Update doc | `PUT /knowledge/manual/{id}` | body `ManualKnowledgePayload`; resp `{"success":true,"data":Knowledge}` |
| Delete doc | `DELETE /knowledge/{id}` | resp `{"success":true,"message":"Delete task submitted","data":{"task_id":...}}` |

**WeKnora error body shape** (both forms exist): `{"error":{"code":1010,"message":"...","details":"..."}}` and bare `{"error":"Unauthorized: ..."}`. Extract `message` (or `error` string) into tool `error` field.

---

## Task 0: Project scaffold

**Files:**
- Create: `go.mod`, `Makefile`, `Dockerfile`, `.gitignore` (exists, no change)

- [ ] **Step 1: Init module and copy Makefile/Dockerfile patterns**

```bash
cd /Users/zhengyong/code/bigdata-mcp-weknora
go mod init weknora-mcp
go get github.com/modelcontextprotocol/go-sdk/mcp@v1.8.0
go get github.com/minio/minio-go/v7@v7.2.1
```

`Makefile` (same shape as email-mcp):

```make
.PHONY: build run test vet

build:
	go build -trimpath -ldflags='-s -w' -o weknora-mcp .

run: build
	./weknora-mcp

test:
	go vet ./... && go test ./...

vet:
	go vet ./...
```

`Dockerfile` (email-mcp pattern; the S3 object upload path needs CA certs + tzdata only):

```dockerfile
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/weknora-mcp .

FROM alpine:3.20
RUN adduser -D -u 10001 mcp
USER mcp
COPY --from=build /out/weknora-mcp /usr/local/bin/weknora-mcp
EXPOSE 8000
ENTRYPOINT ["weknora-mcp"]
```

- [ ] **Step 2: Verify build of empty module**

Run: `go build ./...`
Expected: no error (empty module builds)

- [ ] **Step 3: Commit**

```bash
git add go.mod go.sum Makefile Dockerfile
git commit -m "chore: module scaffold (go-sdk, minio-go deps)"
```

---

## Task 1: identity.go — multica context extraction

**Files:**
- Create: `identity.go`
- Test: `identity_test.go`

Direct copy from email-mcp (both files), package `main`. The proxy injects 8 headers we echo; `identity.go` maps them:

```go
package main

import "net/http"

// multicaHeaderToField mirrors the header set injected by
// multica-stdio-mcp-proxy (proxy.go forwardMessage) and consumed by
// bigdata-mcp-pg's AuthKeyMiddleware.
var multicaHeaderToField = map[string]string{
	"X-Multica-Workspace-Id":   "workspace_id",
	"X-Multica-Agent-Id":       "agent_id",
	"X-Multica-Task-Id":        "task_id",
	"X-Multica-User-Id":        "user_id",
	"X-Multica-User-Name":      "user_name",
	"X-Multica-User-Email":     "user_email",
	"X-Multica-Agent-Name":     "agent_name",
	"X-Multica-Workspace-Name": "workspace_name",
}

// MulticaContextFromHeaders collects the X-Multica-* identity headers into
// the multica_context object echoed in tool results. Absent or empty values
// are omitted; a nil header yields an empty map (local dev).
func MulticaContextFromHeaders(h http.Header) map[string]string {
	ctx := map[string]string{}
	if h == nil {
		return ctx
	}
	for header, field := range multicaHeaderToField {
		if v := h.Get(header); v != "" {
			ctx[field] = v
		}
	}
	return ctx
}
```

`identity_test.go` — copy email-mcp's four tests verbatim (`TestMulticaContextFromHeaders_AllPresent`, `_EmptyAndMissing`, `_NilSafe`, `_NameHeaders`) plus one for the workspace-id key specifically:

```go
func TestMulticaContextFromHeaders_WorkspaceIdKey(t *testing.T) {
	h := http.Header{}
	h.Set("X-Multica-Workspace-Id", "ws-abc")
	ctx := MulticaContextFromHeaders(h)
	if ctx["workspace_id"] != "ws-abc" {
		t.Fatalf("workspace_id = %q", ctx["workspace_id"])
	}
}
```

- [ ] **Step 1: Write identity.go + identity_test.go** (code above + email-mcp tests)
- [ ] **Step 2: Run** `go test ./identity_test.go` → Expected: PASS (5 tests)
- [ ] **Step 3: Commit** `git add identity.go identity_test.go && git commit -m "feat: multica identity context extraction"`

---

## Task 2: config.go — env config, mapping table, startup validation

**Files:**
- Create: `config.go`
- Test: `config_test.go`

Config shape (all env, no YAML — see spec):

```go
package main

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
)

// WorkspaceMapping is one multica workspace → WeKnora KB binding with its
// own API key and optional agent allowlist (prefix-match, pg-mcp style).
type WorkspaceMapping struct {
	WorkspaceID string
	KBID        string
	APIKey      string
	// AllowedAgentIDs is the optional prefix-match allowlist; empty = any
	// agent in the workspace may call.
	AllowedAgentIDs []string
}

// Config is the resolved server configuration.
type Config struct {
	BaseURL      string // WEKNORA_BASE_URL, no trailing slash
	Host         string // MCP_HOST, default 0.0.0.0
	Port         int    // MCP_PORT, default 8000
	EnableIngest bool   // WEKNORA_ENABLE_INGEST, default false
	// Mappings keyed by workspace id. Keyed lookup is the hot path of
	// every tool call.
	Mappings map[string]*WorkspaceMapping
	// S3 is nil when the trio is incomplete (s3_key source disabled).
	S3 *S3Config
}

// parseWorkspaceMap parses WEKNORA_WORKSPACE_MAP: "ws1:kb1,ws2:kb2".
// Both sides must be non-empty; duplicate ws or kb is a config error (the
// 1:1 model makes duplicates a misconfiguration).
func parseWorkspaceMap(raw string) (map[string]string, error) {
	out := map[string]string{}
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("WEKNORA_WORKSPACE_MAP is required: comma-separated workspace_id:kb_id pairs")
	}
	seenKB := map[string]string{}
	for i, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		ws, kb, ok := strings.Cut(pair, ":")
		ws, kb = strings.TrimSpace(ws), strings.TrimSpace(kb)
		if !ok || ws == "" || kb == "" {
			return nil, fmt.Errorf("WEKNORA_WORKSPACE_MAP entry %d (%q) must be workspace_id:kb_id", i+1, pair)
		}
		if _, dup := out[ws]; dup {
			return nil, fmt.Errorf("WEKNORA_WORKSPACE_MAP: duplicate workspace id %q", ws)
		}
		if prev, dup := seenKB[kb]; dup {
			return nil, fmt.Errorf("WEKNORA_WORKSPACE_MAP: kb id %q mapped to both %q and %q (1 workspace = 1 KB)", kb, prev, ws)
		}
		out[ws] = kb
		seenKB[kb] = ws
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("WEKNORA_WORKSPACE_MAP has no valid entries")
	}
	return out, nil
}

// splitCSV splits a comma-separated env var into trimmed non-empty parts.
func splitCSV(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if s := strings.TrimSpace(part); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// LoadConfig reads all env vars and validates the mapping table. A missing
// per-mapping API key is a startup error naming every missing variable at
// once (operators fix all of them in one pass).
func LoadConfig() (*Config, error) {
	cfg := &Config{
		BaseURL: strings.TrimRight(strings.TrimSpace(os.Getenv("WEKNORA_BASE_URL")), "/"),
		Host:    os.Getenv("MCP_HOST"),
		Port:    8000,
	}
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("WEKNORA_BASE_URL is required (e.g. http://your-weknora-host:28444)")
	}
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
	cfg.EnableIngest = strings.EqualFold(strings.TrimSpace(os.Getenv("WEKNORA_ENABLE_INGEST")), "true")

	wsMap, err := parseWorkspaceMap(os.Getenv("WEKNORA_WORKSPACE_MAP"))
	if err != nil {
		return nil, err
	}
	cfg.Mappings = make(map[string]*WorkspaceMapping, len(wsMap))
	var missingKeys []string
	for ws, kb := range wsMap {
		apiKey := strings.TrimSpace(os.Getenv("WEKNORA_WORKSPACE_" + ws + "_API_KEY"))
		if apiKey == "" {
			missingKeys = append(missingKeys, "WEKNORA_WORKSPACE_"+ws+"_API_KEY")
		}
		cfg.Mappings[ws] = &WorkspaceMapping{
			WorkspaceID:     ws,
			KBID:            kb,
			APIKey:          apiKey,
			AllowedAgentIDs: splitCSV(os.Getenv("WEKNORA_WORKSPACE_" + ws + "_ALLOWED_AGENT_IDS")),
		}
	}
	if len(missingKeys) > 0 {
		return nil, fmt.Errorf("per-mapping API keys are required; missing env: %s", strings.Join(missingKeys, ", "))
	}

	cfg.S3 = loadS3Config()
	return cfg, nil
}

// MappingFor returns the mapping for a workspace id.
func (c *Config) MappingFor(workspaceID string) (*WorkspaceMapping, bool) {
	m, ok := c.Mappings[workspaceID]
	return m, ok
}

// AgentAllowed applies the prefix-match allowlist: every configured entry
// is matched as a prefix of the caller's agent id (pg-mcp semantics:
// short UUID prefixes are valid config). Empty allowlist allows any agent.
func (m *WorkspaceMapping) AgentAllowed(agentID string) bool {
	if len(m.AllowedAgentIDs) == 0 {
		return true
	}
	for _, prefix := range m.AllowedAgentIDs {
		if strings.HasPrefix(agentID, prefix) {
			return true
		}
	}
	return false
}
```

`S3Config` lives here (email-mcp defines it in config.go too); `loadS3Config()` comes in Task 4 (S3) — until then a stub returns nil so this task compiles and tests pass. Append to config.go:

```go
// S3Config is the object-store config for the s3_key source of
// add_document (email-mcp S3 standard, env-only).
type S3Config struct {
	Bucket             string
	Region             string
	AccessKeyID        string
	SecretKey          string
	EndpointURL        string // AWS_ENDPOINT_URL (standard, with scheme)
	Endpoint           string // S3_ENDPOINT (legacy, host[:port] form)
	Secure             bool   // legacy S3_ENDPOINT implies http unless S3_SECURE
	UsePathStyle       bool
	PathStyleExplicit  bool
	AllowedKeyPrefixes []string
	MaxFileBytes       int64
}

// loadS3Config is implemented in the S3 task; stub until then.
func loadS3Config() *S3Config { return nil }
```

`config_test.go` — use `t.Setenv` (auto-cleanup, parallel-safe):

```go
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
```

- [ ] **Step 1: Write failing tests** (`config_test.go`, code above)
- [ ] **Step 2: Run** `go test ./config_test.go` → Expected: FAIL (LoadConfig undefined)
- [ ] **Step 3: Write config.go** (code above, with the loadS3Config stub)
- [ ] **Step 4: Run** `go test ./config_test.go` → Expected: PASS (9 tests)
- [ ] **Step 5: Commit** `git add config.go config_test.go && git commit -m "feat: env config with workspace→KB mapping validation"`

---

## Task 3: weknora.go — REST client

**Files:**
- Create: `weknora.go`
- Test: `weknora_test.go`

Client interface (mockable in tool tests) + HTTP implementation:

```go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const weknoraTimeout = 30 * time.Second

// weknoraError carries the HTTP status and the message extracted from a
// WeKnora error body (both body shapes handled).
type weknoraError struct {
	Status  int
	Message string
}

func (e *weknoraError) Error() string { return fmt.Sprintf("weknora api %d: %s", e.Status, e.Message) }

// WeKnoraCaller abstracts the REST calls tools make (mockable in tests).
type WeKnoraCaller interface {
	ListKnowledgeBases(ctx context.Context, m *WorkspaceMapping) ([]json.RawMessage, error)
	SearchKnowledge(ctx context.Context, m *WorkspaceMapping, body map[string]any) ([]json.RawMessage, error)
	ListDocuments(ctx context.Context, m *WorkspaceMapping, query url.Values) ([]json.RawMessage, int, error)
	ReadChunks(ctx context.Context, m *WorkspaceMapping, knowledgeID string, query url.Values) ([]json.RawMessage, int, error)
	WikiSearch(ctx context.Context, m *WorkspaceMapping, q string, limit int) ([]json.RawMessage, error)
	WikiReadPage(ctx context.Context, m *WorkspaceMapping, slug string) (map[string]any, error)
	WikiIndex(ctx context.Context, m *WorkspaceMapping, limit int) (map[string]any, error)
	AddManualDocument(ctx context.Context, m *WorkspaceMapping, title, content string, publish bool) (json.RawMessage, error)
	AddURLDocument(ctx context.Context, m *WorkspaceMapping, url, title string) (json.RawMessage, error)
	AddFileDocument(ctx context.Context, m *WorkspaceMapping, filename string, content []byte, contentType string) (json.RawMessage, error)
	UpdateDocument(ctx context.Context, m *WorkspaceMapping, knowledgeID, content, title string, publish bool) (json.RawMessage, error)
	DeleteDocument(ctx context.Context, m *WorkspaceMapping, knowledgeID string) (map[string]any, error)
}

// weknoraClient is the HTTP implementation over one shared http.Client.
type weknoraClient struct {
	base string
	hc   *http.Client
}

func NewWeKnoraClient(baseURL string) *weknoraClient {
	return &weknoraClient{
		base: strings.TrimRight(baseURL, "/"),
		hc:   &http.Client{Timeout: weknoraTimeout},
	}
}

// do performs one request with the mapping's API key and unwraps the
// response. ok shape: {"success":true,...}; wiki endpoints return the
// bare object. Errors map to *weknoraError with the extracted message.
func (w *weknoraClient) do(ctx context.Context, m *WorkspaceMapping, method, path string, contentType string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, w.base+path, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("X-API-Key", m.APIKey)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := w.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("weknora request failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20)) // 16 MiB response cap
	if err != nil {
		return nil, fmt.Errorf("read weknora response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, &weknoraError{Status: resp.StatusCode, Message: extractWeKnoraMessage(data)}
	}
	return data, nil
}

// extractWeKnoraMessage pulls the human-readable message out of both
// WeKnora error body shapes: {"error":{"message":"..."}} and {"error":"..."}.
func extractWeKnoraMessage(data []byte) string {
	var wrapped struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &wrapped) == nil && wrapped.Error.Message != "" {
		return wrapped.Error.Message
	}
	var bare struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &bare) == nil && bare.Error != "" {
		return bare.Error
	}
	return strings.TrimSpace(string(data))
}

// doJSON is do + JSON body marshalling.
func (w *weknoraClient) doJSON(ctx context.Context, m *WorkspaceMapping, method, path string, payload any) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	return w.do(ctx, m, method, path, "application/json", bytes.NewReader(raw))
}

// unwrapData extracts the "data" array of a {"success":true,"data":[...]}
// response envelope.
func unwrapData(raw []byte) ([]json.RawMessage, error) {
	var env struct {
		Success bool              `json:"success"`
		Data    []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode weknora response: %w", err)
	}
	if !env.Success {
		return nil, fmt.Errorf("weknora response success=false")
	}
	return env.Data, nil
}

// unwrapDataObject extracts the "data" object of the envelope.
func unwrapDataObject(raw []byte) (json.RawMessage, error) {
	var env struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode weknora response: %w", err)
	}
	if !env.Success {
		return nil, fmt.Errorf("weknora response success=false")
	}
	return env.Data, nil
}
```

Then the 13 interface methods, each 3-8 lines using the helpers:

```go
func (w *weknoraClient) ListKnowledgeBases(ctx context.Context, m *WorkspaceMapping) ([]json.RawMessage, error) {
	raw, err := w.do(ctx, m, http.MethodGet, "/api/v1/knowledge-bases", "", nil)
	if err != nil {
		return nil, err
	}
	return unwrapData(raw)
}

func (w *weknoraClient) SearchKnowledge(ctx context.Context, m *WorkspaceMapping, body map[string]any) ([]json.RawMessage, error) {
	raw, err := w.doJSON(ctx, m, http.MethodPost, "/api/v1/knowledge-search", body)
	if err != nil {
		return nil, err
	}
	return unwrapData(raw)
}

func (w *weknoraClient) ListDocuments(ctx context.Context, m *WorkspaceMapping, query url.Values) ([]json.RawMessage, int, error) {
	raw, err := w.do(ctx, m, http.MethodGet, "/api/v1/knowledge-bases/"+m.KBID+"/knowledge?"+query.Encode(), "", nil)
	if err != nil {
		return nil, 0, err
	}
	var env struct {
		Success bool              `json:"success"`
		Data    []json.RawMessage `json:"data"`
		Total   int               `json:"total"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || !env.Success {
		return nil, 0, fmt.Errorf("decode weknora response: %s", snippet(raw))
	}
	return env.Data, env.Total, nil
}

func (w *weknoraClient) ReadChunks(ctx context.Context, m *WorkspaceMapping, knowledgeID string, query url.Values) ([]json.RawMessage, int, error) {
	raw, err := w.do(ctx, m, http.MethodGet, "/api/v1/chunks/"+knowledgeID+"?"+query.Encode(), "", nil)
	if err != nil {
		return nil, 0, err
	}
	var env struct {
		Success bool              `json:"success"`
		Data    []json.RawMessage `json:"data"`
		Total   int               `json:"total"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || !env.Success {
		return nil, 0, fmt.Errorf("decode weknora response: %s", snippet(raw))
	}
	return env.Data, env.Total, nil
}

func (w *weknoraClient) WikiSearch(ctx context.Context, m *WorkspaceMapping, q string, limit int) ([]json.RawMessage, error) {
	raw, err := w.do(ctx, m, http.MethodGet,
		"/api/v1/knowledgebase/"+m.KBID+"/wiki/search?"+url.Values{"q": {q}, "limit": {strconv.Itoa(limit)}}.Encode(), "", nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Pages []json.RawMessage `json:"pages"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode weknora wiki search: %w", err)
	}
	return resp.Pages, nil
}

func (w *weknoraClient) WikiReadPage(ctx context.Context, m *WorkspaceMapping, slug string) (map[string]any, error) {
	// slug is a catch-all path segment: entity/rag → /pages/entity/rag
	raw, err := w.do(ctx, m, http.MethodGet, "/api/v1/knowledgebase/"+m.KBID+"/wiki/pages/"+slug, "", nil)
	if err != nil {
		return nil, err
	}
	var page map[string]any
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, fmt.Errorf("decode weknora wiki page: %w", err)
	}
	return page, nil
}

func (w *weknoraClient) WikiIndex(ctx context.Context, m *WorkspaceMapping, limit int) (map[string]any, error) {
	raw, err := w.do(ctx, m, http.MethodGet,
		"/api/v1/knowledgebase/"+m.KBID+"/wiki/index?"+url.Values{"limit": {strconv.Itoa(limit)}}.Encode(), "", nil)
	if err != nil {
		return nil, err
	}
	var idx map[string]any
	if err := json.Unmarshal(raw, &idx); err != nil {
		return nil, fmt.Errorf("decode weknora wiki index: %w", err)
	}
	return idx, nil
}

func (w *weknoraClient) AddManualDocument(ctx context.Context, m *WorkspaceMapping, title, content string, publish bool) (json.RawMessage, error) {
	raw, err := w.doJSON(ctx, m, http.MethodPost, "/api/v1/knowledge-bases/"+m.KBID+"/manual", map[string]any{
		"title":   title,
		"content": content,
		"status":  manualStatus(publish),
		"channel": "mcp",
	})
	if err != nil {
		return nil, err
	}
	return unwrapDataObject(raw)
}

func (w *weknoraClient) AddURLDocument(ctx context.Context, m *WorkspaceMapping, docURL, title string) (json.RawMessage, error) {
	body := map[string]any{"url": docURL, "channel": "mcp"}
	if title != "" {
		body["title"] = title
	}
	raw, err := w.doJSON(ctx, m, http.MethodPost, "/api/v1/knowledge-bases/"+m.KBID+"/url", body)
	if err != nil {
		return nil, err
	}
	return unwrapDataObject(raw)
}

func (w *weknoraClient) AddFileDocument(ctx context.Context, m *WorkspaceMapping, filename string, content []byte, contentType string) (json.RawMessage, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return nil, fmt.Errorf("build multipart: %w", err)
	}
	if _, err := fw.Write(content); err != nil {
		return nil, fmt.Errorf("build multipart: %w", err)
	}
	_ = mw.WriteField("fileName", filename)
	_ = mw.WriteField("channel", "mcp")
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("build multipart: %w", err)
	}
	raw, err := w.do(ctx, m, http.MethodPost, "/api/v1/knowledge-bases/"+m.KBID+"/file", mw.FormDataContentType(), &buf)
	if err != nil {
		return nil, err
	}
	return unwrapDataObject(raw)
}

func (w *weknoraClient) UpdateDocument(ctx context.Context, m *WorkspaceMapping, knowledgeID, content, title string, publish bool) (json.RawMessage, error) {
	body := map[string]any{"content": content, "status": manualStatus(publish), "channel": "mcp"}
	if title != "" {
		body["title"] = title
	}
	raw, err := w.doJSON(ctx, m, http.MethodPut, "/api/v1/knowledge/manual/"+knowledgeID, body)
	if err != nil {
		return nil, err
	}
	return unwrapDataObject(raw)
}

func (w *weknoraClient) DeleteDocument(ctx context.Context, m *WorkspaceMapping, knowledgeID string) (map[string]any, error) {
	raw, err := w.do(ctx, m, http.MethodDelete, "/api/v1/knowledge/"+knowledgeID, "", nil)
	if err != nil {
		return nil, err
	}
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode weknora response: %w", err)
	}
	return env, nil
}

// manualStatus maps publish→WeKnora manual-knowledge status.
func manualStatus(publish bool) string {
	if publish {
		return "publish"
	}
	return "draft"
}

// snippet returns up to 200 bytes of a raw response for error messages.
func snippet(raw []byte) string {
	if len(raw) > 200 {
		return string(raw[:200]) + "..."
	}
	return string(raw)
}
```

Imports for the full file: `bytes`, `context`, `encoding/json`, `fmt`, `io`, `mime/multipart`, `net/http`, `net/url`, `strconv`, `strings`, `time`.

`weknora_test.go` — httptest fake WeKnora covering header, path, body shape, error mapping, and 16 MiB cap:

```go
package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeWeKnora(t *testing.T, handler http.HandlerFunc) (*weknoraClient, *WorkspaceMapping) {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return NewWeKnoraClient(ts.URL), &WorkspaceMapping{WorkspaceID: "ws-1", KBID: "kb-1", APIKey: "wk-test"}
}

func TestWeKnoraClient_SendsAPIKeyAndPath(t *testing.T) {
	var gotPath, gotKey string
	c, m := fakeWeKnora(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey = r.URL.Path, r.Header.Get("X-API-Key")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"success":true,"data":[]}`)
	})
	if _, err := c.ListKnowledgeBases(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/v1/knowledge-bases" || gotKey != "wk-test" {
		t.Fatalf("path=%q key=%q", gotPath, gotKey)
	}
}

func TestWeKnoraClient_ErrorBodyShapes(t *testing.T) {
	c, m := fakeWeKnora(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"code":1010,"message":"Invalid login parameters","details":"x"}}`)
	})
	_, err := c.ListKnowledgeBases(context.Background(), m)
	we, ok := err.(*weknoraError)
	if !ok || we.Status != 400 || we.Message != "Invalid login parameters" {
		t.Fatalf("got %v", err)
	}

	c2, m2 := fakeWeKnora(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"Unauthorized: invalid API key"}`)
	})
	_, err = c2.ListKnowledgeBases(context.Background(), m2)
	we, ok = err.(*weknoraError)
	if !ok || we.Status != 401 || we.Message != "Unauthorized: invalid API key" {
		t.Fatalf("got %v", err)
	}
}

func TestWeKnoraClient_SearchBodyShape(t *testing.T) {
	var gotBody map[string]any
	c, m := fakeWeKnora(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		io.WriteString(w, `{"success":true,"data":[{"id":"c1"}]}`)
	})
	if _, err := c.SearchKnowledge(context.Background(), m, map[string]any{
		"query": "q", "knowledge_base_ids": []string{"kb-1"}, "match_count": 10,
	}); err != nil {
		t.Fatal(err)
	}
	if gotBody["query"] != "q" {
		t.Fatalf("body: %v", gotBody)
	}
	ids, _ := gotBody["knowledge_base_ids"].([]any)
	if len(ids) != 1 || ids[0] != "kb-1" {
		t.Fatalf("kb ids: %v", ids)
	}
}

func TestWeKnoraClient_WikiPaths(t *testing.T) {
	var paths []string
	c, m := fakeWeKnora(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/wiki/search"):
			io.WriteString(w, `{"pages":[{"slug":"entity/rag"}]}`)
		case strings.HasSuffix(r.URL.Path, "/wiki/index"):
			io.WriteString(w, `{"intro":"i","groups":[]}`)
		default: // pages catch-all
			io.WriteString(w, `{"slug":"entity/rag","title":"RAG","out_links":[]}`)
		}
	})
	if _, err := c.WikiSearch(context.Background(), m, "rag", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := c.WikiReadPage(context.Background(), m, "entity/rag"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.WikiIndex(context.Background(), m, 50); err != nil {
		t.Fatal(err)
	}
	wantPrefix := "/api/v1/knowledgebase/kb-1/wiki"
	for _, p := range paths {
		if !strings.HasPrefix(p, wantPrefix) {
			t.Errorf("wiki path %q must use singular knowledgebase prefix", p)
		}
	}
}

func TestWeKnoraClient_MultipartFileUpload(t *testing.T) {
	c, m := fakeWeKnora(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
		}
		if r.FormValue("channel") != "mcp" {
			t.Errorf("channel = %q", r.FormValue("channel"))
		}
		fh, err := r.FormFile("file")
		if err != nil {
			t.Errorf("form file: %v", err)
			return
		}
		defer fh.Close()
		b, _ := io.ReadAll(fh)
		if string(b) != "csv,data\n1,2\n" {
			t.Errorf("file content: %q", string(b))
		}
		io.WriteString(w, `{"success":true,"data":{"id":"k9"}}`)
	})
	raw, err := c.AddFileDocument(context.Background(), m, "report.csv", []byte("csv,data\n1,2\n"), "text/csv")
	if err != nil {
		t.Fatal(err)
	}
	var k struct {
		ID string `json:"id"`
	}
	json.Unmarshal(raw, &k)
	if k.ID != "k9" {
		t.Fatalf("id = %q", k.ID)
	}
}
```

- [ ] **Step 1: Write failing tests** (`weknora_test.go`, code above)
- [ ] **Step 2: Run** `go test ./weknora_test.go` → Expected: FAIL (undefined symbols)
- [ ] **Step 3: Write weknora.go** (code above)
- [ ] **Step 4: Run** `go test ./weknora_test.go` → Expected: PASS (5 tests)
- [ ] **Step 5: Commit** `git add weknora.go weknora_test.go && git commit -m "feat: weknora rest client (per-mapping key, error passthrough)"`

---

## Task 4: s3.go + s3keys.go — S3 object store

**Files:**
- Create: `s3.go`, `s3keys.go`
- Modify: `config.go` (replace loadS3Config stub)
- Test: `s3_test.go`, `s3keys_test.go`, extend `config_test.go`

Copy from email-mcp **verbatim, no code changes** — the files are already generic (the WeKnora 50 MiB cap is applied at the call site: the tools task passes `weknoraMaxFileBytes` as `Get`'s limit, which triggers `ErrAttachmentTooLarge`):

- `s3.go`: `ObjectStore` interface, `ErrObjectNotFound`/`ErrObjectTooLarge`/`ErrAttachmentTooLarge` sentinels, timeout constants, `MinioObjectStore`, `NewMinioObjectStore`, `bucketLookup`, `redactURL`, `Get`, `mapError` (source: `/Users/zhengyong/code/bigdata-mcp-email/s3.go`). Note `Get(key, limit)` treats `limit` as the **caller** cap → `ErrAttachmentTooLarge`, while `MaxFileBytes` is the **config** cap → `ErrObjectTooLarge`; the tools task maps them to different error codes.
- `s3keys.go`: `dangerousExtensions`, `hasDangerousExtension`, `invalidKeyError`, `invalidKey`, `IsInvalidKey`, `validateInputKey`, `prefixAllowed`, `errNoKeyInURL`, `resolveInputKey`, `trimScheme` (source: `/Users/zhengyong/code/bigdata-mcp-email/s3keys.go`).
- `s3_test.go`, `s3keys_test.go`: copy verbatim — the tests are already domain-agnostic (fake S3 over httptest, `fakeS3Handler`, `newTestObjectStore`, `newTestObjectStoreURL` helpers; no email/attachment-domain references).

Then replace the `config.go` stub with the real S3 env loader (adapted from email-mcp `LoadConfig` S3 section — trio soft-disable, endpoint URL, region chain, path-style inference, prefixes, max bytes):

```go
// loadS3Config resolves the S3 env contract: trio (S3_BUCKET /
// AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY) incomplete → nil (soft
// disable); complete → resolved config. Legacy names S3_ACCESS_KEY /
// S3_SECRET_KEY readable (standard wins).
func loadS3Config() *S3Config {
	bucket := strings.TrimSpace(os.Getenv("S3_BUCKET"))
	accessKeyID := strings.TrimSpace(s3Env("AWS_ACCESS_KEY_ID", "S3_ACCESS_KEY"))
	secretKey := strings.TrimSpace(s3Env("AWS_SECRET_ACCESS_KEY", "S3_SECRET_KEY"))
	if bucket == "" || accessKeyID == "" || secretKey == "" {
		if bucket != "" || accessKeyID != "" || secretKey != "" {
			var missing []string
			if bucket == "" {
				missing = append(missing, "S3_BUCKET")
			}
			if accessKeyID == "" {
				missing = append(missing, "AWS_ACCESS_KEY_ID")
			}
			if secretKey == "" {
				missing = append(missing, "AWS_SECRET_ACCESS_KEY")
			}
			log.Printf("config: s3 disabled, missing env: %s (s3_key source returns S3_UNAVAILABLE)", strings.Join(missing, ", "))
		}
		return nil
	}
	s3 := &S3Config{Bucket: bucket, AccessKeyID: accessKeyID, SecretKey: secretKey}
	s3.Region = strings.TrimSpace(s3Env("S3_REGION", ""))
	if s3.Region == "" {
		s3.Region = strings.TrimSpace(os.Getenv("AWS_REGION"))
	}
	if s3.Region == "" {
		s3.Region = "us-east-1"
	}
	if v := strings.TrimSpace(s3Env("AWS_ENDPOINT_URL", "")); v != "" {
		s3.EndpointURL = v
	} else if v := strings.TrimSpace(os.Getenv("S3_ENDPOINT")); v != "" {
		s3.Endpoint = v
		s3.Secure = parseBoolEnv("S3_SECURE", false)
	}
	s3.UsePathStyle, s3.PathStyleExplicit = parsePathStyleEnv(s3Env("S3_USE_PATH_STYLE", ""), s3.EndpointURL != "" || s3.Endpoint != "")
	s3.AllowedKeyPrefixes = splitCSV(s3Env("S3_KEY_PREFIXES", ""))
	s3.MaxFileBytes = parseBytesEnv(s3Env("S3_MAX_FILE_BYTES", ""), 100*1024*1024)
	return s3
}

// s3Env returns the standard name's value first, then the legacy name.
func s3Env(std, legacy string) string {
	if v := os.Getenv(std); v != "" {
		return v
	}
	return os.Getenv(legacy)
}

func parseBoolEnv(name string, def bool) bool {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
		log.Printf("config: %s=%q is not a valid bool, using %v", name, v, def)
	}
	return def
}

func parseBytesEnv(raw string, def int64) int64 {
	if n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil && n > 0 {
		return n
	}
	return def
}

func parsePathStyleEnv(raw string, hasEndpoint bool) (value, explicit bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes":
		return true, true
	case "0", "false", "no":
		return false, true
	}
	return hasEndpoint, false
}
```

**`S3Config` already lives in config.go** (landed in Task 2) — do NOT re-declare it in s3.go. Task 4 only replaces the `loadS3Config` nil-stub in config.go with the loader above and adds the `"log"` import to config.go's import block (the loader's log.Printf calls need it).

While in config.go/config_test.go, fold in three hardenings (review findings from Task 2, plan-inherited gaps):

1. `parseWorkspaceMap`: reject pairs with more than one colon — after `strings.Cut`, error if either side still contains `:`:

```go
		if strings.Contains(ws, ":") || strings.Contains(kb, ":") {
			return nil, fmt.Errorf("WEKNORA_WORKSPACE_MAP entry %d (%q) must be workspace_id:kb_id", i+1, pair)
		}
```

(insert immediately after the `if !ok || ws == "" || kb == ""` check)

2. `sort.Strings(missingKeys)` before the join in `LoadConfig` (reproducible error messages across map iteration order); add `"sort"` to imports.

3. Three new tests in config_test.go:

```go
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
	if m.AgentAllowed("ag-1-x") || m.AgentAllowed("ag-9") {
		t.Error("allowlist gating wrong")
	}
}
```

Also harden the negative tests against ambient env: in `TestLoadConfig_MissingBaseURL` add `t.Setenv("WEKNORA_BASE_URL", "")` at the top, and in `TestLoadConfig_EmptyMap` / `TestLoadConfig_MalformedEntry` / `TestLoadConfig_DuplicateWorkspaceAndKB` add `t.Setenv("WEKNORA_WORKSPACE_MAP", ...)` is already there — only `TestLoadConfig_MissingBaseURL` needs the explicit empty (it must be immune to an exported WEKNORA_WORKSPACE_MAP in the shell).

Extend `config_test.go`:

```go
func TestLoadConfig_S3TrioIncomplete(t *testing.T) {
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
```

- [ ] **Step 1: Copy s3.go / s3keys.go / their tests from email-mcp** (`cp` then `go build` to check; delete email-specific test references)
- [ ] **Step 2: Replace config.go stub with loadS3Config + helpers; add the 2 config tests**
- [ ] **Step 3: Run** `go test ./...` → Expected: PASS (all tests incl. copied s3 tests)
- [ ] **Step 4: Commit** `git add s3.go s3keys.go s3_test.go s3keys_test.go config.go config_test.go && git commit -m "feat: s3 object store (email-mcp standard, weknora cap)"`

---

## Task 5: tools.go — 10 MCP tools

**Files:**
- Create: `tools.go`
- Test: `tools_test.go`

ServerEnv + routing gate shared by every handler:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ServerEnv carries the dependencies shared by tool handlers.
type ServerEnv struct {
	Config  *Config
	WeKnora WeKnoraCaller
	Store   ObjectStore // nil = s3_key source unavailable
}

// Error codes carried in failed tool payloads' error_code field.
const (
	errCodeWorkspaceNotMapped  = "WORKSPACE_NOT_MAPPED"
	errCodeAccessDenied        = "ACCESS_DENIED"
	errCodeMissingParam        = "MISSING_PARAM"
	errCodeInvalidParamType    = "INVALID_PARAM_TYPE"
	errCodeInvalidParamValue   = "INVALID_PARAM_VALUE"
	errCodeWeKnoraFailed       = "WEKNORA_API_FAILED"
	errCodeWeKnoraTimeout      = "WEKNORA_TIMEOUT"
	errCodeS3Unavailable       = "S3_UNAVAILABLE"
	errCodeS3InvalidKey        = "S3_INVALID_KEY"
	errCodeS3ObjectNotFound    = "S3_OBJECT_NOT_FOUND"
	errCodeS3ObjectTooLarge    = "S3_OBJECT_TOO_LARGE"
	errCodeS3FetchFailed       = "S3_FETCH_FAILED"
	errCodeS3FileTooLargeForWK = "S3_FILE_TOO_LARGE_FOR_WEKNORA"
	errCodeInternal            = "INTERNAL"
)

// toolResult is the JSON payload every tool returns.
type toolResult struct {
	Success        bool              `json:"success"`
	Error          string            `json:"error,omitempty"`
	ErrorCode      string            `json:"error_code,omitempty"`
	MulticaContext map[string]string `json:"multica_context"`
	// one-of payload fields set per tool (json.RawMessage keeps the
	// upstream shape without re-typing every WeKnora struct)
	Data json.RawMessage `json:"data,omitempty"`
}

func errPayload(code, errMsg string, mc map[string]string) []byte {
	raw, _ := json.Marshal(toolResult{
		Success:        false,
		Error:          errMsg,
		ErrorCode:      code,
		MulticaContext: mc,
	})
	return raw
}

// resolveMapping applies the two access gates every tool shares:
// workspace must be mapped, agent must pass the mapping's allowlist.
func (env *ServerEnv) resolveMapping(mc map[string]string) (*WorkspaceMapping, []byte) {
	ws := mc["workspace_id"]
	if ws == "" {
		return nil, errPayload(errCodeWorkspaceNotMapped,
			"no workspace identity: this tool must be called through multica (X-Multica-Workspace-Id header required)", mc)
	}
	m, ok := env.Config.MappingFor(ws)
	if !ok {
		return nil, errPayload(errCodeWorkspaceNotMapped,
			fmt.Sprintf("workspace %q is not mapped to a weknora knowledge base; ask the operator to add it to WEKNORA_WORKSPACE_MAP", ws), mc)
	}
	agentID := mc["agent_id"]
	if agentID != "" && !m.AgentAllowed(agentID) {
		return nil, errPayload(errCodeAccessDenied,
			fmt.Sprintf("agent %q is not allowed to access this workspace's knowledge base", agentID), mc)
	}
	return m, nil
}

// mapWeKnoraError converts a client error into a tool payload, mapping
// timeouts and status codes onto the error-code vocabulary.
func mapWeKnoraError(err error, mc map[string]string) []byte {
	if we, ok := err.(*weknoraError); ok {
		return errPayload(errCodeWeKnoraFailed,
			fmt.Sprintf("weknora api error (HTTP %d): %s", we.Status, we.Message), mc)
	}
	if strings.Contains(err.Error(), "context deadline exceeded") {
		return errPayload(errCodeWeKnoraTimeout, "weknora api call timed out (30s)", mc)
	}
	// Transport failures (DNS, connection refused): the call to the
	// weknora API failed without an HTTP status — same caller-facing
	// meaning as an API error, and the error text (e.g. "weknora request
	// failed: dial tcp: lookup weknora.invalid: no such host") already
	// says what broke. The Task 7 release smoke depends on this mapping
	// (it greps for WEKNORA_API_FAILED against an unresolvable base URL).
	return errPayload(errCodeWeKnoraFailed, err.Error(), mc)
}

// validateKnowledgeID guards the path-bound knowledge_id parameter:
// WeKnora knowledge IDs are opaque server-generated tokens, never
// multi-segment (only wiki slugs are, and they go through WikiReadPage),
// so / or .. in an ID is an attempt to reach another endpoint under the
// workspace API key. "?" and "#" are refused too: the ID is interpolated
// into a request path before the query string, so a crafted ? could
// smuggle extra query parameters (k1?admin=1) into the call.
func validateKnowledgeID(id string) error {
	if strings.Contains(id, "/") || strings.Contains(id, "..") || strings.Contains(id, "?") || strings.Contains(id, "#") {
		return fmt.Errorf("parameter \"knowledge_id\" must be a document id, not a path")
	}
	return nil
}
```

Tool registration + handlers. Each handler: extract args → resolveMapping → call WeKnoraCaller → payload. Registration happens on a `*mcp.Server` in `RegisterTools(server *mcp.Server, env *ServerEnv)`, mirroring email-mcp's `mcp.AddTool` wiring with `req.Extra.Header` for headers and `req.Params.Arguments` for raw args (same lenient-schema philosophy: schema is descriptive; validation in handler for uniform error codes).

Tool schemas and handler logic (tools.go, continued — this is the complete file content beyond the shared code above):

```go
// RegisterTools registers all tools on the server. Write tools register
// only when ingest is enabled (deployment-level switch).
func RegisterTools(server *mcp.Server, env *ServerEnv) {
	addRetrieveTools(server, env)
	addWikiTools(server, env)
	if env.Config.EnableIngest {
		addIngestTools(server, env)
	}
}

// callTool wraps the email-mcp AddTool pattern: parse args map, run fn,
// double-surface the payload (structuredContent + TextContent).
func callTool(server *mcp.Server, env *ServerEnv, tool *mcp.Tool, fn func(ctx context.Context, hdr http.Header, args map[string]any) []byte) {
	mcp.AddTool(server, tool, func(ctx context.Context, req *mcp.CallToolRequest, in any) (*mcp.CallToolResult, any, error) {
		var hdr map[string][]string
		if req.Extra != nil && req.Extra.Header != nil {
			hdr = req.Extra.Header
		}
		var args map[string]any
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				mc := MulticaContextFromHeaders(hdr)
				payload := errPayload(errCodeInvalidParamValue, "invalid arguments: "+err.Error(), mc)
				return &mcp.CallToolResult{
					IsError:           true,
					Content:           []mcp.Content{&mcp.TextContent{Text: string(payload)}},
					StructuredContent: json.RawMessage(payload),
				}, nil, nil
			}
		}
		payload := fn(ctx, http.Header(hdr), args)
		isErr := !jsonPayloadSuccess(payload)
		return &mcp.CallToolResult{
			IsError:           isErr,
			Content:           []mcp.Content{&mcp.TextContent{Text: string(payload)}},
			StructuredContent: json.RawMessage(payload),
		}, nil, nil
	})
}

func jsonPayloadSuccess(payload []byte) bool {
	var probe struct {
		Success bool `json:"success"`
	}
	return json.Unmarshal(payload, &probe) == nil && probe.Success
}
```

**Retrieve tools:**

```go
func addRetrieveTools(server *mcp.Server, env *ServerEnv) {
	callTool(server, env, &mcp.Tool{
		Name: "list_knowledge_bases",
		Description: "List the knowledge bases visible to this workspace's weknora API key, with id, name and description. " +
			"Other tools operate on the knowledge base this multica workspace is mapped to.",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(ctx context.Context, hdr http.Header, args map[string]any) []byte {
		mc := MulticaContextFromHeaders(hdr)
		m, fail := env.resolveMapping(mc)
		if fail != nil {
			return fail
		}
		kbs, err := env.WeKnora.ListKnowledgeBases(ctx, m)
		if err != nil {
			return mapWeKnoraError(err, mc)
		}
		return successPayload(map[string]any{"knowledge_bases": kbs}, mc)
	})

	callTool(server, env, &mcp.Tool{
		Name: "search_knowledge",
		Description: "Search this workspace's weknora knowledge base and return the most relevant passages with document ids and titles. " +
			"mode hybrid (default) combines semantic and keyword retrieval; semantic ranks by meaning; keyword matches exact terms, identifiers or codes.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"query":{"type":"string","description":"natural-language question, topic, or exact terms to match"},
				"mode":{"type":"string","enum":["hybrid","semantic","keyword"],"description":"retrieval mode, default hybrid"},
				"limit":{"type":"number","description":"max passages, default 10, max 30"},
				"knowledge_ids":{"type":"array","items":{"type":"string"},"description":"optional document ids to restrict the search (from list_documents or previous results)"}
			},
			"required":["query"]
		}`),
	}, func(ctx context.Context, hdr http.Header, args map[string]any) []byte {
		mc := MulticaContextFromHeaders(hdr)
		m, fail := env.resolveMapping(mc)
		if fail != nil {
			return fail
		}
		query, _ := args["query"].(string)
		if strings.TrimSpace(query) == "" {
			return errPayload(errCodeMissingParam, "parameter \"query\" is required", mc)
		}
		mode := "hybrid"
		if v, present := args["mode"]; present && v != nil {
			s, ok := v.(string)
			if !ok || (s != "hybrid" && s != "semantic" && s != "keyword") {
				return errPayload(errCodeInvalidParamValue, "parameter \"mode\" must be hybrid, semantic or keyword", mc)
			}
			mode = s
		}
		limit := 10
		if v, present := args["limit"]; present && v != nil {
			f, ok := v.(float64)
			if !ok || f < 1 || f > 30 {
				return errPayload(errCodeInvalidParamValue, "parameter \"limit\" must be a number between 1 and 30", mc)
			}
			limit = int(f)
		}
		var knowledgeIDs []string
		if v, present := args["knowledge_ids"]; present && v != nil {
			list, ok := v.([]any)
			if !ok {
				return errPayload(errCodeInvalidParamType, "parameter \"knowledge_ids\" must be an array of strings", mc)
			}
			for _, item := range list {
				s, _ := item.(string)
				if s = strings.TrimSpace(s); s != "" {
					knowledgeIDs = append(knowledgeIDs, s)
				}
			}
		}
		body := map[string]any{
			"query":              query,
			"knowledge_base_ids": []string{m.KBID},
			"match_count":        limit,
		}
		switch mode {
		case "semantic":
			body["disable_keywords_match"] = true
		case "keyword":
			body["disable_vector_match"] = true
		}
		if len(knowledgeIDs) > 0 {
			body["knowledge_ids"] = knowledgeIDs
		}
		results, err := env.WeKnora.SearchKnowledge(ctx, m, body)
		if err != nil {
			return mapWeKnoraError(err, mc)
		}
		return successPayload(map[string]any{"results": results}, mc)
	})

	callTool(server, env, &mcp.Tool{
		Name: "list_documents",
		Description: "List the documents in this workspace's weknora knowledge base with pagination and optional filters. Returns document ids for read_document.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"keyword":{"type":"string","description":"optional substring to filter document titles"},
				"page":{"type":"number","description":"1-based page number, default 1"},
				"page_size":{"type":"number","description":"documents per page, default 20, max 100"},
				"parse_status":{"type":"string","description":"filter by parse status (e.g. completed, parsing, failed)"},
				"file_type":{"type":"string","description":"filter by file extension"},
				"sort_by":{"type":"string","enum":["created_at","updated_at","file_name"],"description":"default created_at"},
				"sort_order":{"type":"string","enum":["asc","desc"],"description":"default desc"}
			}
		}`),
	}, func(ctx context.Context, hdr http.Header, args map[string]any) []byte {
		mc := MulticaContextFromHeaders(hdr)
		m, fail := env.resolveMapping(mc)
		if fail != nil {
			return fail
		}
		q := url.Values{}
		page, pageSize := 1, 20
		if v, present := args["page"]; present && v != nil {
			f, ok := v.(float64)
			if !ok || f < 1 {
				return errPayload(errCodeInvalidParamValue, "parameter \"page\" must be a positive number", mc)
			}
			page = int(f)
		}
		if v, present := args["page_size"]; present && v != nil {
			f, ok := v.(float64)
			if !ok || f < 1 || f > 100 {
				return errPayload(errCodeInvalidParamValue, "parameter \"page_size\" must be a number between 1 and 100", mc)
			}
			pageSize = int(f)
		}
		q.Set("page", strconv.Itoa(page))
		q.Set("page_size", strconv.Itoa(pageSize))
		for _, p := range []string{"keyword", "parse_status", "file_type", "sort_by", "sort_order"} {
			if s, _ := args[p].(string); strings.TrimSpace(s) != "" {
				q.Set(p, strings.TrimSpace(s))
			}
		}
		docs, total, err := env.WeKnora.ListDocuments(ctx, m, q)
		if err != nil {
			return mapWeKnoraError(err, mc)
		}
		return successPayload(map[string]any{
			"documents": docs, "total": total, "page": page, "page_size": pageSize,
		}, mc)
	})

	callTool(server, env, &mcp.Tool{
		Name: "read_document",
		Description: "Read a document's text chunks in reading order from this workspace's weknora knowledge base. Page through with page/page_size.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"knowledge_id":{"type":"string","description":"document id from list_documents or search results"},
				"page":{"type":"number","description":"1-based page number, default 1"},
				"page_size":{"type":"number","description":"chunks per page, default 10, max 100"},
				"chunk_type":{"type":"array","items":{"type":"string"},"description":"chunk types to return, default [\"text\"]"}
			},
			"required":["knowledge_id"]
		}`),
	}, func(ctx context.Context, hdr http.Header, args map[string]any) []byte {
		mc := MulticaContextFromHeaders(hdr)
		m, fail := env.resolveMapping(mc)
		if fail != nil {
			return fail
		}
		knowledgeID, _ := args["knowledge_id"].(string)
		if strings.TrimSpace(knowledgeID) == "" {
			return errPayload(errCodeMissingParam, "parameter \"knowledge_id\" is required", mc)
		}
		if err := validateKnowledgeID(knowledgeID); err != nil {
			return errPayload(errCodeInvalidParamValue, err.Error(), mc)
		}
		q := url.Values{}
		page, pageSize := 1, 10
		if v, present := args["page"]; present && v != nil {
			f, ok := v.(float64)
			if !ok || f < 1 {
				return errPayload(errCodeInvalidParamValue, "parameter \"page\" must be a positive number", mc)
			}
			page = int(f)
		}
		if v, present := args["page_size"]; present && v != nil {
			f, ok := v.(float64)
			if !ok || f < 1 || f > 100 {
				return errPayload(errCodeInvalidParamValue, "parameter \"page_size\" must be a number between 1 and 100", mc)
			}
			pageSize = int(f)
		}
		q.Set("page", strconv.Itoa(page))
		q.Set("page_size", strconv.Itoa(pageSize))
		types := []string{"text"}
		if v, present := args["chunk_type"]; present && v != nil {
			list, ok := v.([]any)
			if !ok {
				return errPayload(errCodeInvalidParamType, "parameter \"chunk_type\" must be an array of strings", mc)
			}
			var ts []string
			for _, item := range list {
				if s, _ := item.(string); strings.TrimSpace(s) != "" {
					ts = append(ts, strings.TrimSpace(s))
				}
			}
			if len(ts) > 0 {
				types = ts
			}
		}
		for _, ct := range types {
			q.Add("chunk_type", ct)
		}
		chunks, total, err := env.WeKnora.ReadChunks(ctx, m, knowledgeID, q)
		if err != nil {
			return mapWeKnoraError(err, mc)
		}
		return successPayload(map[string]any{
			"chunks": chunks, "total": total, "page": page, "page_size": pageSize,
		}, mc)
	})
}
```

**Wiki tools** (with out-link summary inlining for read_page, cap 10):

```go
// wikiOutLinkCap bounds the out-link summary fan-out of wiki_read_page
// (1 page + up to 10 follow-up reads, mirroring the official tool's
// inlining behaviour with a hard ceiling).
const wikiOutLinkCap = 10

func addWikiTools(server *mcp.Server, env *ServerEnv) {
	callTool(server, env, &mcp.Tool{
		Name: "wiki_search",
		Description: "Search the wiki pages of this workspace's weknora knowledge base by title, alias and content. Returns page slugs for wiki_read_page.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"query":{"type":"string","description":"search terms or a regular expression (case-insensitive POSIX)"},
				"regex":{"type":"boolean","description":"false forces literal match; true requires valid regex; omit for auto"},
				"limit":{"type":"number","description":"max pages, default 10, max 50"}
			},
			"required":["query"]
		}`),
	}, func(ctx context.Context, hdr http.Header, args map[string]any) []byte {
		mc := MulticaContextFromHeaders(hdr)
		m, fail := env.resolveMapping(mc)
		if fail != nil {
			return fail
		}
		query, _ := args["query"].(string)
		if strings.TrimSpace(query) == "" {
			return errPayload(errCodeMissingParam, "parameter \"query\" is required", mc)
		}
		limit := 10
		if v, present := args["limit"]; present && v != nil {
			f, ok := v.(float64)
			if !ok || f < 1 || f > 50 {
				return errPayload(errCodeInvalidParamValue, "parameter \"limit\" must be a number between 1 and 50", mc)
			}
			limit = int(f)
		}
		if v, present := args["regex"]; present && v != nil {
			b, ok := v.(bool)
			if !ok {
				return errPayload(errCodeInvalidParamType, "parameter \"regex\" must be a boolean", mc)
			}
			if !b {
				query = regexp.QuoteMeta(query)
			}
		}
		pages, err := env.WeKnora.WikiSearch(ctx, m, query, limit)
		if err != nil {
			return mapWeKnoraError(err, mc)
		}
		return successPayload(map[string]any{"pages": pages}, mc)
	})

	callTool(server, env, &mcp.Tool{
		Name: "wiki_read_page",
		Description: "Read one wiki page by slug, including content, metadata and out-links with inlined summaries (up to 10).",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{"slug":{"type":"string","description":"page slug from wiki_search or wiki_index, e.g. concept/rag"}},
			"required":["slug"]
		}`),
	}, func(ctx context.Context, hdr http.Header, args map[string]any) []byte {
		mc := MulticaContextFromHeaders(hdr)
		m, fail := env.resolveMapping(mc)
		if fail != nil {
			return fail
		}
		slug, _ := args["slug"].(string)
		if strings.TrimSpace(slug) == "" {
			return errPayload(errCodeMissingParam, "parameter \"slug\" is required", mc)
		}
		page, err := env.WeKnora.WikiReadPage(ctx, m, strings.TrimSpace(slug))
		if err != nil {
			return mapWeKnoraError(err, mc)
		}
		// Out-link summary inlining (official MCP behaviour parity): fetch
		// each out-linked page's summary so the agent sees context without
		// a second round trip. Cap at wikiOutLinkCap; the rest are listed
		// as slug-only.
		outLinks := stringSlice(page["out_links"])
		linked := make([]map[string]any, 0, len(outLinks))
		var omitted []string
		for i, link := range outLinks {
			if i >= wikiOutLinkCap {
				omitted = append(omitted, outLinks[i:]...)
				break
			}
			linkedPage, err := env.WeKnora.WikiReadPage(ctx, m, link)
			if err != nil {
				// a dead/dangling link is page data, not a tool failure
				linked = append(linked, map[string]any{"slug": link, "summary": "", "missing": true})
				continue
			}
			linked = append(linked, map[string]any{
				"slug":    link,
				"title":   linkedPage["title"],
				"summary": linkedPage["summary"],
			})
		}
		page["out_link_details"] = linked
		if len(omitted) > 0 {
			page["out_links_omitted"] = omitted
		}
		return successPayload(page, mc)
	})

	callTool(server, env, &mcp.Tool{
		Name: "wiki_index",
		Description: "Browse the wiki table of contents of this workspace's weknora knowledge base: intro plus pages grouped by type.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{"limit":{"type":"number","description":"max entries per page type, default 50, max 200"}}
		}`),
	}, func(ctx context.Context, hdr http.Header, args map[string]any) []byte {
		mc := MulticaContextFromHeaders(hdr)
		m, fail := env.resolveMapping(mc)
		if fail != nil {
			return fail
		}
		limit := 50
		if v, present := args["limit"]; present && v != nil {
			f, ok := v.(float64)
			if !ok || f < 1 || f > 200 {
				return errPayload(errCodeInvalidParamValue, "parameter \"limit\" must be a number between 1 and 200", mc)
			}
			limit = int(f)
		}
		idx, err := env.WeKnora.WikiIndex(ctx, m, limit)
		if err != nil {
			return mapWeKnoraError(err, mc)
		}
		return successPayload(idx, mc)
	})
}

// stringSlice coerces a JSON-decoded any into []string (absent, non-array
// or non-string items yield nil).
func stringSlice(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// successPayload marshals a successful tool result.
func successPayload(data map[string]any, mc map[string]string) []byte {
	if mc == nil {
		mc = map[string]string{}
	}
	raw, err := json.Marshal(toolResult{
		Success:        true,
		MulticaContext: mc,
		Data:           mustJSON(data),
	})
	if err != nil {
		return errPayload(errCodeInternal, "marshal result: "+err.Error(), mc)
	}
	return raw
}

func mustJSON(v any) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}
```

Note: `toolResult.Data` is `json.RawMessage`, and successPayload wraps each tool's fields (`results`/`documents`/`chunks`/`pages`/page object/index object) as that raw `data` object — one envelope shape across all 10 tools.

**Ingest tools** (behind `EnableIngest`), in tools.go:

```go
// weknoraMaxFileBytes is WeKnora's upload cap (utils.GetMaxFileSizeMB
// default 50). S3 objects larger than this are rejected before upload.
const weknoraMaxFileBytes = 50 * 1024 * 1024

func addIngestTools(server *mcp.Server, env *ServerEnv) {
	callTool(server, env, &mcp.Tool{
		Name: "add_document",
		Description: "Add a document to this workspace's weknora knowledge base from Markdown text, a URL, or an S3 object (s3_key). " +
			"Parsing and indexing happen asynchronously; the response returns the new document id.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"title":{"type":"string","description":"document title (required; for s3_key defaults to the object's base name without .md)"},
				"content":{"type":"string","description":"Markdown content; required unless url or s3_key is given"},
				"url":{"type":"string","description":"web page or file URL to import; required unless content or s3_key is given"},
				"s3_key":{"type":"string","description":"object key (or full object-store URL) in the configured bucket; required unless content or url is given"},
				"publish":{"type":"boolean","description":"publish immediately (default true) or keep as draft"}
			},
			"required":["title"]
		}`),
	}, func(ctx context.Context, hdr http.Header, args map[string]any) []byte {
		mc := MulticaContextFromHeaders(hdr)
		m, fail := env.resolveMapping(mc)
		if fail != nil {
			return fail
		}
		title, _ := args["title"].(string)
		if strings.TrimSpace(title) == "" {
			return errPayload(errCodeMissingParam, "parameter \"title\" is required", mc)
		}
		publish := true
		if v, present := args["publish"]; present && v != nil {
			b, ok := v.(bool)
			if !ok {
				return errPayload(errCodeInvalidParamType, "parameter \"publish\" must be a boolean", mc)
			}
			publish = b
		}
		content, hasContent := args["content"].(string)
		if v, present := args["content"]; present && v != nil && !hasContent {
			return errPayload(errCodeInvalidParamType, "parameter \"content\" must be a string", mc)
		}
		docURL, hasURL := args["url"].(string)
		if v, present := args["url"]; present && v != nil && !hasURL {
			return errPayload(errCodeInvalidParamType, "parameter \"url\" must be a string", mc)
		}
		s3key, hasS3 := args["s3_key"].(string)
		if v, present := args["s3_key"]; present && v != nil && !hasS3 {
			return errPayload(errCodeInvalidParamType, "parameter \"s3_key\" must be a string", mc)
		}
		given := 0
		for _, b := range []bool{strings.TrimSpace(content) != "", strings.TrimSpace(docURL) != "", strings.TrimSpace(s3key) != ""} {
			if b {
				given++
			}
		}
		if given != 1 {
			return errPayload(errCodeInvalidParamValue, "give exactly one of content, url or s3_key", mc)
		}

		var (
			doc json.RawMessage
			err error
		)
		switch {
		case strings.TrimSpace(content) != "":
			doc, err = env.WeKnora.AddManualDocument(ctx, m, title, content, publish)
		case strings.TrimSpace(docURL) != "":
			doc, err = env.WeKnora.AddURLDocument(ctx, m, strings.TrimSpace(docURL), title)
		default:
			doc, err = env.addS3Document(ctx, m, title, strings.TrimSpace(s3key), publish, mc)
			if err != nil {
				return s3ErrorPayload(err, mc)
			}
		}
		if err != nil {
			return mapWeKnoraError(err, mc)
		}
		return successPayload(map[string]any{
			"document":   doc,
			"async_note": "parsing and indexing continue asynchronously; poll list_documents with parse_status to observe progress",
		}, mc)
	})

	callTool(server, env, &mcp.Tool{
		Name: "update_document",
		Description: "Replace the content (and optionally the title) of a Markdown document created from text. Re-indexing happens asynchronously.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"knowledge_id":{"type":"string","description":"document id"},
				"content":{"type":"string","description":"new Markdown content"},
				"title":{"type":"string","description":"new title; keeps the current title when omitted"},
				"publish":{"type":"boolean","description":"publish (default true) or keep as draft"}
			},
			"required":["knowledge_id","content"]
		}`),
	}, func(ctx context.Context, hdr http.Header, args map[string]any) []byte {
		mc := MulticaContextFromHeaders(hdr)
		m, fail := env.resolveMapping(mc)
		if fail != nil {
			return fail
		}
		knowledgeID, _ := args["knowledge_id"].(string)
		if strings.TrimSpace(knowledgeID) == "" {
			return errPayload(errCodeMissingParam, "parameter \"knowledge_id\" is required", mc)
		}
		if err := validateKnowledgeID(knowledgeID); err != nil {
			return errPayload(errCodeInvalidParamValue, err.Error(), mc)
		}
		content, _ := args["content"].(string)
		if content == "" {
			return errPayload(errCodeMissingParam, "parameter \"content\" is required", mc)
		}
		title, _ := args["title"].(string)
		publish := true
		if v, present := args["publish"]; present && v != nil {
			b, ok := v.(bool)
			if !ok {
				return errPayload(errCodeInvalidParamType, "parameter \"publish\" must be a boolean", mc)
			}
			publish = b
		}
		doc, err := env.WeKnora.UpdateDocument(ctx, m, knowledgeID, content, title, publish)
		if err != nil {
			return mapWeKnoraError(err, mc)
		}
		return successPayload(map[string]any{"document": doc}, mc)
	})

	callTool(server, env, &mcp.Tool{
		Name: "delete_document",
		Description: "Permanently delete a document and its index data from this workspace's weknora knowledge base.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{"knowledge_id":{"type":"string","description":"document id"}},
			"required":["knowledge_id"]
		}`),
	}, func(ctx context.Context, hdr http.Header, args map[string]any) []byte {
		mc := MulticaContextFromHeaders(hdr)
		m, fail := env.resolveMapping(mc)
		if fail != nil {
			return fail
		}
		knowledgeID, _ := args["knowledge_id"].(string)
		if strings.TrimSpace(knowledgeID) == "" {
			return errPayload(errCodeMissingParam, "parameter \"knowledge_id\" is required", mc)
		}
		if err := validateKnowledgeID(knowledgeID); err != nil {
			return errPayload(errCodeInvalidParamValue, err.Error(), mc)
		}
		resp, err := env.WeKnora.DeleteDocument(ctx, m, knowledgeID)
		if err != nil {
			return mapWeKnoraError(err, mc)
		}
		return successPayload(resp, mc)
	})
}

// addS3Document implements the s3_key source of add_document: fetch the
// object, then route by extension — .md/.markdown to the manual (text)
// endpoint, everything else to multipart file upload.
func (env *ServerEnv) addS3Document(ctx context.Context, m *WorkspaceMapping, title, s3key string, publish bool, mc map[string]string) (json.RawMessage, error) {
	if env.Store == nil {
		return nil, &s3ToolError{code: errCodeS3Unavailable, msg: "s3 attachments are not configured (set S3_BUCKET / AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY)"}
	}
	s3cfg := env.Config.S3
	if s3cfg == nil {
		s3cfg = &S3Config{}
	}
	key, err := resolveInputKey(s3key, s3cfg)
	if err != nil {
		return nil, &s3ToolError{code: errCodeS3InvalidKey, msg: fmt.Sprintf("s3_key: %v", err)}
	}
	// resolveInputKey already validated traversal, dangerous extension
	// and prefix allowlist before the fetch — no re-check needed here.
	content, err := env.Store.Get(key, weknoraMaxFileBytes)
	if err != nil {
		return nil, mapS3GetError(err, key, s3cfg.Bucket)
	}
	lower := strings.ToLower(path.Base(key))
	switch {
	case strings.HasSuffix(lower, ".md"), strings.HasSuffix(lower, ".markdown"):
		// title is required at the tool layer (handleAddDocument rejects
		// empty), so no fallback to the object name is needed.
		return env.WeKnora.AddManualDocument(ctx, m, title, string(content), publish)
	default:
		return env.WeKnora.AddFileDocument(ctx, m, path.Base(key), content, mime.TypeByExtension(path.Ext(lower)))
	}
}

// s3ToolError carries a tool error code through the s3 path.
type s3ToolError struct {
	code string
	msg  string
}

func (e *s3ToolError) Error() string { return e.msg }

// s3ErrorPayload renders an s3-path error (s3ToolError or a mapped store
// error) as a tool payload.
func s3ErrorPayload(err error, mc map[string]string) []byte {
	if ste, ok := err.(*s3ToolError); ok {
		return errPayload(ste.code, ste.msg, mc)
	}
	return errPayload(errCodeS3FetchFailed, err.Error(), mc)
}

// mapS3GetError folds the object-store sentinel errors onto tool codes,
// naming the bucket for not-found (bucket-attribution triage, email-mcp
// behaviour).
func mapS3GetError(err error, key, bucket string) error {
	switch {
	case errors.Is(err, ErrObjectNotFound):
		if bucket != "" {
			return &s3ToolError{code: errCodeS3ObjectNotFound, msg: fmt.Sprintf("s3_key: object not found in bucket %q", bucket)}
		}
		return &s3ToolError{code: errCodeS3ObjectNotFound, msg: "s3_key: object not found"}
	case errors.Is(err, ErrObjectTooLarge):
		// exceeded the configured S3_MAX_FILE_BYTES (server-level cap)
		return &s3ToolError{code: errCodeS3ObjectTooLarge, msg: "s3 object exceeds S3_MAX_FILE_BYTES"}
	case errors.Is(err, ErrAttachmentTooLarge):
		// exceeded the caller limit = the WeKnora 50 MiB upload ceiling;
		// catch it before a wasted multipart upload
		return &s3ToolError{code: errCodeS3FileTooLargeForWK,
			msg: fmt.Sprintf("s3 object exceeds the weknora upload limit (%d MiB)", weknoraMaxFileBytes>>20)}
	}
	return &s3ToolError{code: errCodeS3FetchFailed, msg: fmt.Sprintf("s3 fetch failed: %v", err)}
}
```

Full import block for tools.go: `context`, `encoding/json`, `errors`, `fmt`, `mime`, `net/http`, `net/url`, `path`, `regexp`, `strconv`, `strings`, plus `github.com/modelcontextprotocol/go-sdk/mcp`.

`tools_test.go` — mock caller + table-driven tests:

```go
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
)

// fakeWeKnoraCaller records calls and returns canned responses.
type fakeWeKnoraCaller struct {
	lastKBID       string
	lastAPIKey     string
	searchBody     map[string]any
	listDocsQuery  url.Values
	readChunksID   string
	wikiSearchQ    string
	wikiReadSlugs  []string
	addManualBody  map[string]any // recorded via title/content/publish below
	addManualTitle string
	addManualPub   bool
	addURLURL      string
	addFileName    string
	addFileContent []byte
	updateID       string
	deleteID       string

	searchResults  []json.RawMessage
	wikiPageBySlug map[string]map[string]any
}

func (f *fakeWeKnoraCaller) ListKnowledgeBases(ctx context.Context, m *WorkspaceMapping) ([]json.RawMessage, error) {
	f.lastAPIKey = m.APIKey
	return []json.RawMessage{json.RawMessage(`{"id":"kb-1","name":"bigdata"}`)}, nil
}
func (f *fakeWeKnoraCaller) SearchKnowledge(ctx context.Context, m *WorkspaceMapping, body map[string]any) ([]json.RawMessage, error) {
	f.lastKBID, f.searchBody = m.KBID, body
	return f.searchResults, nil
}
func (f *fakeWeKnoraCaller) ListDocuments(ctx context.Context, m *WorkspaceMapping, query url.Values) ([]json.RawMessage, int, error) {
	f.listDocsQuery = query
	return nil, 0, nil
}
func (f *fakeWeKnoraCaller) ReadChunks(ctx context.Context, m *WorkspaceMapping, id string, query url.Values) ([]json.RawMessage, int, error) {
	f.readChunksID, f.listDocsQuery = id, query
	return nil, 0, nil
}
func (f *fakeWeKnoraCaller) WikiSearch(ctx context.Context, m *WorkspaceMapping, q string, limit int) ([]json.RawMessage, error) {
	f.wikiSearchQ = q
	return nil, nil
}
func (f *fakeWeKnoraCaller) WikiReadPage(ctx context.Context, m *WorkspaceMapping, slug string) (map[string]any, error) {
	f.wikiReadSlugs = append(f.wikiReadSlugs, slug)
	if p, ok := f.wikiPageBySlug[slug]; ok {
		return p, nil
	}
	return map[string]any{"slug": slug, "title": slug, "summary": "s-" + slug}, nil
}
func (f *fakeWeKnoraCaller) WikiIndex(ctx context.Context, m *WorkspaceMapping, limit int) (map[string]any, error) {
	return map[string]any{"intro": "i"}, nil
}
func (f *fakeWeKnoraCaller) AddManualDocument(ctx context.Context, m *WorkspaceMapping, title, content string, publish bool) (json.RawMessage, error) {
	f.addManualTitle, f.addManualPub = title, publish
	return json.RawMessage(`{"id":"k-new"}`), nil
}
func (f *fakeWeKnoraCaller) AddURLDocument(ctx context.Context, m *WorkspaceMapping, u, title string) (json.RawMessage, error) {
	f.addURLURL = u
	return json.RawMessage(`{"id":"k-url"}`), nil
}
func (f *fakeWeKnoraCaller) AddFileDocument(ctx context.Context, m *WorkspaceMapping, filename string, content []byte, contentType string) (json.RawMessage, error) {
	f.addFileName, f.addFileContent = filename, content
	return json.RawMessage(`{"id":"k-file"}`), nil
}
func (f *fakeWeKnoraCaller) UpdateDocument(ctx context.Context, m *WorkspaceMapping, id, content, title string, publish bool) (json.RawMessage, error) {
	f.updateID = id
	return json.RawMessage(`{"id":"k-upd"}`), nil
}
func (f *fakeWeKnoraCaller) DeleteDocument(ctx context.Context, m *WorkspaceMapping, id string) (map[string]any, error) {
	f.deleteID = id
	return map[string]any{"success": true}, nil
}

func testEnv(fake WeKnoraCaller) *ServerEnv {
	return &ServerEnv{
		Config: &Config{
			BaseURL: "http://wk.test",
			Mappings: map[string]*WorkspaceMapping{
				"ws-1": {WorkspaceID: "ws-1", KBID: "kb-1", APIKey: "wk-a"},
			},
		},
		WeKnora: fake,
	}
}

func wsHeaders() http.Header {
	h := http.Header{}
	h.Set("X-Multica-Workspace-Id", "ws-1")
	return h
}

func decodePayload(t *testing.T, raw []byte) toolResult {
	t.Helper()
	var r toolResult
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("decode: %v — %s", err, raw)
	}
	return r
}

func TestResolveMapping_Gates(t *testing.T) {
	env := testEnv(&fakeWeKnoraCaller{})
	// no header
	mc := map[string]string{}
	if _, fail := env.resolveMapping(mc); fail == nil {
		t.Fatal("missing workspace must fail")
	} else {
		r := decodePayload(t, fail)
		if r.ErrorCode != "WORKSPACE_NOT_MAPPED" {
			t.Fatalf("code = %q", r.ErrorCode)
		}
	}
	// unmapped workspace
	mc = map[string]string{"workspace_id": "ws-other"}
	if _, fail := env.resolveMapping(mc); fail == nil {
		t.Fatal("unmapped workspace must fail")
	}
	// mapped, no allowlist → pass
	mc = map[string]string{"workspace_id": "ws-1", "agent_id": "ag-1"}
	m, fail := env.resolveMapping(mc)
	if fail != nil || m.KBID != "kb-1" {
		t.Fatalf("mapped must pass: %v %v", m, fail)
	}
	// allowlist denies
	env.Config.Mappings["ws-1"].AllowedAgentIDs = []string{"other"}
	mc = map[string]string{"workspace_id": "ws-1", "agent_id": "ag-1"}
	if _, fail := env.resolveMapping(mc); fail == nil {
		t.Fatal("agent not in allowlist must fail")
	} else {
		r := decodePayload(t, fail)
		if r.ErrorCode != "ACCESS_DENIED" {
			t.Fatalf("code = %q", r.ErrorCode)
		}
	}
}

func TestSearchKnowledge_ModesAndKB(t *testing.T) {
	fake := &fakeWeKnoraCaller{}
	env := testEnv(fake)

	// hybrid default: no disable flags
	payload := envSearchKnowledge(env, wsHeaders(), map[string]any{"query": "q"})
	if r := decodePayload(t, payload); !r.Success {
		t.Fatalf("payload: %s", payload)
	}
	if _, ok := fake.searchBody["disable_keywords_match"]; ok {
		t.Error("hybrid must not set disable flags")
	}
	if ids := fake.searchBody["knowledge_base_ids"].([]string); len(ids) != 1 || ids[0] != "kb-1" {
		t.Fatalf("kb routing: %v", ids)
	}

	// semantic
	envSearchKnowledge(env, wsHeaders(), map[string]any{"query": "q", "mode": "semantic"})
	if fake.searchBody["disable_keywords_match"] != true {
		t.Error("semantic must disable keywords")
	}
	// keyword
	envSearchKnowledge(env, wsHeaders(), map[string]any{"query": "q", "mode": "keyword"})
	if fake.searchBody["disable_vector_match"] != true {
		t.Error("keyword must disable vector")
	}
	// invalid mode
	payload = envSearchKnowledge(env, wsHeaders(), map[string]any{"query": "q", "mode": "bogus"})
	if r := decodePayload(t, payload); r.ErrorCode != "INVALID_PARAM_VALUE" {
		t.Fatalf("code = %q", r.ErrorCode)
	}
	// limit > 30
	payload = envSearchKnowledge(env, wsHeaders(), map[string]any{"query": "q", "limit": 99.0})
	if r := decodePayload(t, payload); r.ErrorCode != "INVALID_PARAM_VALUE" {
		t.Fatalf("code = %q", r.ErrorCode)
	}
}

func TestWikiReadPage_OutLinkInliningCap(t *testing.T) {
	fake := &fakeWeKnoraCaller{
		wikiPageBySlug: map[string]map[string]any{
			"index": {
				"slug": "index", "title": "Index", "summary": "root",
				"out_links": []any{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"},
			},
		},
	}
	env := testEnv(fake)
	payload := envWikiReadPage(env, wsHeaders(), map[string]any{"slug": "index"})
	var r struct {
		toolResult
		Data struct {
			OutLinkDetails []map[string]any `json:"out_link_details"`
			OutLinksOmitted []string        `json:"out_links_omitted"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Data.OutLinkDetails) != 10 {
		t.Fatalf("inlined = %d, want cap 10", len(r.Data.OutLinkDetails))
	}
	if len(r.Data.OutLinksOmitted) != 2 || r.Data.OutLinksOmitted[0] != "k" {
		t.Fatalf("omitted = %v", r.Data.OutLinksOmitted)
	}
	// 1 main read + 10 inlined = 11 WeKnora calls
	if len(fake.wikiReadSlugs) != 11 {
		t.Fatalf("wiki calls = %d, want 11", len(fake.wikiReadSlugs))
	}
}

func TestAddDocument_Sources(t *testing.T) {
	fake := &fakeWeKnoraCaller{}
	env := testEnv(fake)

	// content source
	payload := envAddDocument(env, wsHeaders(), map[string]any{"title": "T", "content": "# hi"})
	if r := decodePayload(t, payload); !r.Success {
		t.Fatalf("payload: %s", payload)
	}
	if fake.addManualTitle != "T" {
		t.Errorf("manual title = %q", fake.addManualTitle)
	}
	// url source
	envAddDocument(env, wsHeaders(), map[string]any{"title": "T", "url": "http://x.test/a"})
	if fake.addURLURL != "http://x.test/a" {
		t.Errorf("url = %q", fake.addURLURL)
	}
	// both content+url → exactly-one error
	payload = envAddDocument(env, wsHeaders(), map[string]any{"title": "T", "content": "c", "url": "u"})
	if r := decodePayload(t, payload); r.ErrorCode != "INVALID_PARAM_VALUE" {
		t.Fatalf("code = %q", r.ErrorCode)
	}
	// s3 with no store → S3_UNAVAILABLE
	payload = envAddDocument(env, wsHeaders(), map[string]any{"title": "T", "s3_key": "ws-1/x.csv"})
	if r := decodePayload(t, payload); r.ErrorCode != "S3_UNAVAILABLE" {
		t.Fatalf("code = %q", r.ErrorCode)
	}
}

// fakeObjectStore is a canned ObjectStore for the s3_source routing test.
type fakeObjectStore struct {
	gotKey   string
	gotLimit int64
	content  []byte
	err      error
}

func (f *fakeObjectStore) Get(key string, limit int64) ([]byte, error) {
	f.gotKey, f.gotLimit = key, limit
	return f.content, f.err
}

func TestAddDocument_S3Routing(t *testing.T) {
	fake := &fakeWeKnoraCaller{}
	env := testEnv(fake)

	// .md object → manual (text) endpoint
	env.Store = &fakeObjectStore{content: []byte("# hi")}
	payload := envAddDocument(env, wsHeaders(), map[string]any{"title": "T", "s3_key": "docs/a.md"})
	if r := decodePayload(t, payload); !r.Success {
		t.Fatalf("payload: %s", payload)
	}
	if fake.addManualTitle != "T" {
		t.Errorf("manual title = %q", fake.addManualTitle)
	}
	store := env.Store.(*fakeObjectStore)
	if store.gotKey != "docs/a.md" || store.gotLimit != weknoraMaxFileBytes {
		t.Errorf("store.Get key=%q limit=%d", store.gotKey, store.gotLimit)
	}

	// .csv object → multipart file endpoint
	env.Store = &fakeObjectStore{content: []byte("csv,data\n1,2\n")}
	payload = envAddDocument(env, wsHeaders(), map[string]any{"title": "T", "s3_key": "docs/a.csv"})
	if r := decodePayload(t, payload); !r.Success {
		t.Fatalf("payload: %s", payload)
	}
	if fake.addFileName != "a.csv" {
		t.Errorf("file name = %q", fake.addFileName)
	}

	// dangerous extension → S3_INVALID_KEY (validateInputKey pre-fetch)
	env.Store = &fakeObjectStore{content: []byte("x")}
	payload = envAddDocument(env, wsHeaders(), map[string]any{"title": "T", "s3_key": "docs/a.exe"})
	if r := decodePayload(t, payload); r.ErrorCode != "S3_INVALID_KEY" {
		t.Fatalf("code = %q", r.ErrorCode)
	}

	// oversize → ErrAttachmentTooLarge → S3_FILE_TOO_LARGE_FOR_WEKNORA
	env.Store = &fakeObjectStore{err: ErrAttachmentTooLarge}
	payload = envAddDocument(env, wsHeaders(), map[string]any{"title": "T", "s3_key": "docs/big.pdf"})
	if r := decodePayload(t, payload); r.ErrorCode != "S3_FILE_TOO_LARGE_FOR_WEKNORA" {
		t.Fatalf("code = %q", r.ErrorCode)
	}

	// not found → ErrObjectNotFound → S3_OBJECT_NOT_FOUND
	env.Store = &fakeObjectStore{err: ErrObjectNotFound}
	payload = envAddDocument(env, wsHeaders(), map[string]any{"title": "T", "s3_key": "docs/gone.pdf"})
	if r := decodePayload(t, payload); r.ErrorCode != "S3_OBJECT_NOT_FOUND" {
		t.Fatalf("code = %q", r.ErrorCode)
	}
}
```

The `envX` helpers call the handler closures directly. To make that possible, extract each tool's handler closure into a named method on `*ServerEnv` (e.g. `func (env *ServerEnv) handleSearchKnowledge(ctx, hdr, args) []byte`) and have `addRetrieveTools`/`addWikiTools`/`addIngestTools` pass `env.handleSearchKnowledge` to `callTool`. Test helpers then:

```go
func envSearchKnowledge(env *ServerEnv, hdr http.Header, args map[string]any) []byte {
	return env.handleSearchKnowledge(context.Background(), hdr, args)
}
func envWikiReadPage(env *ServerEnv, hdr http.Header, args map[string]any) []byte {
	return env.handleWikiReadPage(context.Background(), hdr, args)
}
func envAddDocument(env *ServerEnv, hdr http.Header, args map[string]any) []byte {
	return env.handleAddDocument(context.Background(), hdr, args)
}
```

Add the same named-method pattern for all 10 handlers (`handleListKnowledgeBases`, `handleSearchKnowledge`, `handleListDocuments`, `handleReadDocument`, `handleWikiSearch`, `handleWikiReadPage`, `handleWikiIndex`, `handleAddDocument`, `handleUpdateDocument`, `handleDeleteDocument`).

Also add these two tests:

```go
func TestListDocuments_ParamPassThrough(t *testing.T) {
	fake := &fakeWeKnoraCaller{}
	env := testEnv(fake)
	payload := envListDocuments(env, wsHeaders(), map[string]any{
		"keyword": "report", "page": 2.0, "page_size": 50.0, "parse_status": "completed",
	})
	if r := decodePayload(t, payload); !r.Success {
		t.Fatalf("payload: %s", payload)
	}
	if fake.listDocsQuery.Get("keyword") != "report" || fake.listDocsQuery.Get("page") != "2" ||
		fake.listDocsQuery.Get("page_size") != "50" || fake.listDocsQuery.Get("parse_status") != "completed" {
		t.Fatalf("query: %v", fake.listDocsQuery)
	}
}

func TestReadDocument_ChunkTypes(t *testing.T) {
	fake := &fakeWeKnoraCaller{}
	env := testEnv(fake)
	payload := envReadDocument(env, wsHeaders(), map[string]any{
		"knowledge_id": "k-9", "chunk_type": []any{"text", "table_summary"},
	})
	if r := decodePayload(t, payload); !r.Success {
		t.Fatalf("payload: %s", payload)
	}
	if fake.readChunksID != "k-9" {
		t.Fatalf("id = %q", fake.readChunksID)
	}
	types := fake.listDocsQuery["chunk_type"]
	if len(types) != 2 || types[0] != "text" || types[1] != "table_summary" {
		t.Fatalf("chunk types: %v", types)
	}
}

func TestKnowledgeIDTraversalRejected(t *testing.T) {
	fake := &fakeWeKnoraCaller{}
	env := testEnv(fake)
	for _, id := range []string{"../../user/settings", "k1?admin=1", "a/b"} {
		payload := envReadDocument(env, wsHeaders(), map[string]any{"knowledge_id": id})
		if r := decodePayload(t, payload); r.ErrorCode != "INVALID_PARAM_VALUE" {
			t.Fatalf("id %q: code = %q, want INVALID_PARAM_VALUE", id, r.ErrorCode)
		}
		if fake.readChunksID != "" { // rejected ids must not reach the client
			t.Fatalf("id %q reached the client", id)
		}
	}
	// update_document and delete_document share the guard
	payload := envUpdateDocument(env, wsHeaders(), map[string]any{"knowledge_id": "../x", "content": "c"})
	if r := decodePayload(t, payload); r.ErrorCode != "INVALID_PARAM_VALUE" {
		t.Fatalf("update: code = %q", r.ErrorCode)
	}
	payload = envDeleteDocument(env, wsHeaders(), map[string]any{"knowledge_id": "a/b"})
	if r := decodePayload(t, payload); r.ErrorCode != "INVALID_PARAM_VALUE" {
		t.Fatalf("delete: code = %q", r.ErrorCode)
	}
}
```

- [ ] **Step 1: Write failing tests** (`tools_test.go`, code above; run `go test . -count=1` → Expected: FAIL with undefined symbols — package mode, the single-file form doesn't compile)
- [ ] **Step 3: Write tools.go** (code above, handlers as named ServerEnv methods; add missing `envListDocuments`/`envReadDocument`/`envUpdateDocument`/`envDeleteDocument` helpers to the test file too)
- [ ] **Step 4: Run** `go test ./... -count=1` → Expected: PASS (8 tools tests; run in package mode — the single-file form doesn't compile)
- [ ] **Step 5: Commit** `git add tools.go tools_test.go && git commit -m "feat: 10 mcp tools over weknora rest (retrieve+wiki+ingest)"`

### Task 5 quality-review amendments (post-implementation)

The quality review (empirically verified against go-sdk v1.8.0) found the plan's tool schemas were NOT lenient: the SDK resolves the provided InputSchema and validates arguments BEFORE the handler runs, so `required`, `enum` and scalar-type constraints produced bare SDK rejections (no error_code, no multica_context) for exactly the mistakes the handler validations were meant to catch — and explicit nulls on optional params produced a cryptic upstream message (`validating /properties/publish: type: <invalid reflect.Value`). These amendments supersede the schema blocks and addS3Document/s3ErrorPayload code above:

**1. Schema leniency (Critical).** Every schema except `list_knowledge_bases` (already `{"type":"object"}`, no params) transforms:
- remove all `required` arrays — absent params must reach the handler and return MISSING_PARAM envelopes (add "(required)" to those descriptions instead);
- every property becomes `{"description":"...","anyOf":[{"type":"<declared>"},{"type":"null"}]}` (arrays: `{"anyOf":[{"type":"array","items":{"type":"string"}},{"type":"null"}]}`) — explicit nulls pass the SDK and the handlers' `present && v != nil` skips them;
- drop `enum` keywords — valid values move into descriptions; the handler validates `mode` (INVALID_PARAM_VALUE envelope), `sort_by`/`sort_order` pass through to WeKnora (WEKNORA_API_FAILED envelope).

The SDK still enforces declared scalar types (`limit:"10"` → SDK rejection) — the email-mcp-accepted level; INVALID_PARAM_TYPE branches stay as defense in depth. Exemplar (search_knowledge):

```json
{"type":"object","properties":{
	"query":{"description":"natural-language question, topic, or exact terms to match (required)","anyOf":[{"type":"string"},{"type":"null"}]},
	"mode":{"description":"retrieval mode: hybrid (default), semantic, or keyword","anyOf":[{"type":"string"},{"type":"null"}]},
	"limit":{"description":"max passages, default 10, max 30","anyOf":[{"type":"number"},{"type":"null"}]},
	"knowledge_ids":{"description":"optional document ids to restrict the search (from list_documents or previous results)","anyOf":[{"type":"array","items":{"type":"string"}},{"type":"null"}]}
}}
```

Property lists for the other eight: list_documents (keyword/page/page_size/parse_status/file_type/sort_by/sort_order — sort_by desc "sort field: created_at (default), updated_at, or file_name", sort_order desc "asc or desc (default desc)"); read_document (knowledge_id (required)/page/page_size/chunk_type); wiki_search (query (required)/regex/limit); wiki_read_page (slug (required)); wiki_index (limit); add_document (title (required)/content/url/s3_key/publish); update_document (knowledge_id (required)/content (required)/title/publish); delete_document (knowledge_id (required)).

**2. S3-path upload errors (Important).** addS3Document wraps only the pre-upload phases (store nil / key resolve / Get) in s3ToolError; WeKnora-side upload failures (weknoraError, timeouts) must surface through mapWeKnoraError, not S3_FETCH_FAILED. s3ErrorPayload is deleted; addS3Document drops its unused mc param. handleAddDocument's default branch:

```go
		default:
			doc, err = env.addS3Document(ctx, m, title, strings.TrimSpace(s3key), publish)
			if err != nil {
				if ste, ok := err.(*s3ToolError); ok {
					return errPayload(ste.code, ste.msg, mc)
				}
				// the s3 phases succeeded; this failure is on the
				// WeKnora upload side — use the WeKnora vocabulary.
				return mapWeKnoraError(err, mc)
			}
		}
```

**3. Wiki slug dot-segment guard (Important).** Multi-segment slugs keep "/" structural, but "."/".." segments would be resolved by any normalizing proxy in front of WeKnora and could climb out of the `/wiki/pages/{kb}/` prefix:

```go
// validateWikiSlug guards the slug of wiki_read_page: slugs are
// multi-segment identifiers, so "/" is structural — but a "." or ".."
// segment would be resolved by any normalizing proxy in front of
// WeKnora and could climb out of the /wiki/pages/{kb}/ prefix to read
// another knowledge base under this workspace's API key.
func validateWikiSlug(slug string) error {
	for _, seg := range strings.Split(slug, "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("parameter \"slug\" must be a wiki page slug, not a path")
		}
	}
	return nil
}
```

Applied in handleWikiReadPage after the empty check, on the trimmed slug.

**4. Minor fixes:** update_document rejects whitespace-only content (TrimSpace, prevents silent blank overwrite); knowledge_id is trimmed at use in read/update/delete (an opaque ID with trailing whitespace is an avoidable 404); callTool drops its unused env param (signature + 10 call sites); test fake records addManualContent (asserted in S3Routing) and addManualPub (publish:false case), dead addManualBody field removed, ReadChunks' query recording renamed lastQuery; go.mod go-sdk marker corrected to direct (surgical go mod edit, not tidy).

**5. New tests:** SDK-level leniency test over a bare mcp.Server + RegisterTools httptest harness in tools_test.go (absent query → MISSING_PARAM envelope; null optionals → success; null query → MISSING_PARAM; bogus mode → INVALID_PARAM_VALUE); S3 upload-failure test (Store.Get succeeds, AddManualDocument returns weknoraError 500 → WEKNORA_API_FAILED, not S3_FETCH_FAILED); wiki slug dot-segment test (../x, a/../b, entity/. rejected, none reach the client); whitespace-content and knowledge-id-trim tests.

---

## Task 6: middleware.go + server.go + main.go — HTTP assembly

**Files:**
- Create: `middleware.go`, `server.go`, `main.go`
- Test: `server_test.go`

`middleware.go` — verbatim from email-mcp:

```go
package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

// bearerAuth validates Authorization: Bearer <key> against a comma-separated
// key set. Only the exactly-empty key string disables auth (local dev).
// A non-empty string that yields no usable keys fails closed. /health is exempt.
func bearerAuth(keys string, next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, k := range strings.Split(keys, ",") {
		if k = strings.TrimSpace(k); k != "" {
			allowed[k] = true
		}
	}
	authDisabled := keys == ""
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authDisabled || r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		h := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(h, prefix) || !allowed[strings.TrimSpace(h[len(prefix):])] {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error":   "unauthorized",
				"message": "Invalid or missing authentication key",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}
```

`server.go` — email-mcp assembly adapted: /health reports config summary (mappings count, ingest on/off, s3 on/off):

```go
package main

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// NewHandler builds the full HTTP handler: /health + /mcp (stateless,
// JSON-response streamable transport) behind bearer auth.
func NewHandler(env *ServerEnv, authKeys string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		s3State := "not-configured"
		if env.Store != nil {
			s3State = "configured"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(
			`{"status":"ok","workspaces":%d,"ingest":%t,"s3":"%s"}`,
			len(env.Config.Mappings), env.Config.EnableIngest, s3State)))
	})

	server := mcp.NewServer(&mcp.Implementation{Name: "weknora-mcp", Version: "v0.1.0"}, nil)
	RegisterTools(server, env)

	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(
		func(r *http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{
			Stateless:    true,
			JSONResponse: true,
			// Markdown documents can be large; allow 8 MiB requests
			// (WeKnora's manual content cap is 200k runes ≈ 800 KB UTF-8;
			// headroom for JSON envelope and escaping).
			MaxRequestBodyBytes: 8 << 20,
		},
	))

	return bearerAuth(authKeys, mux)
}
```

`main.go`:

```go
package main

import (
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

func main() {
	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	env := &ServerEnv{
		Config:  cfg,
		WeKnora: NewWeKnoraClient(cfg.BaseURL),
	}
	if cfg.S3 != nil {
		store, err := NewMinioObjectStore(cfg.S3)
		if err != nil {
			log.Fatalf("s3: %v", err)
		}
		env.Store = store
	}
	addr := cfg.Host + ":" + strconv.Itoa(cfg.Port)
	log.Printf("weknora-mcp listening on %s (weknora %s, %d workspace mappings, ingest %t)",
		addr, cfg.BaseURL, len(cfg.Mappings), cfg.EnableIngest)
	// ReadHeaderTimeout is the slowloris guard (see email-mcp main.go).
	srv := &http.Server{
		Addr:              addr,
		Handler:           NewHandler(env, os.Getenv("MCP_AUTH_KEY")),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}
```

`server_test.go` — end-to-end MCP over httptest with fake WeKnoraCaller (email-mcp pattern):

```go
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(t *testing.T, authKeys string, fake WeKnoraCaller) *httptest.Server {
	t.Helper()
	env := testEnv(fake)
	ts := httptest.NewServer(NewHandler(env, authKeys))
	t.Cleanup(ts.Close)
	return ts
}

func mcpCall(t *testing.T, base string, headers map[string]string, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest("POST", base+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

const searchCall = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_knowledge","arguments":{"query":"orders pipeline"}}}`

func TestServer_AuthGate(t *testing.T) {
	ts := newTestServer(t, "secret", &fakeWeKnoraCaller{})
	hdr := map[string]string{"X-Multica-Workspace-Id": "ws-1"}

	status, _ := mcpCall(t, ts.URL, hdr, searchCall)
	if status != 401 {
		t.Fatalf("no key: status = %d", status)
	}
	status, _ = mcpCall(t, ts.URL, map[string]string{"Authorization": "Bearer wrong", "X-Multica-Workspace-Id": "ws-1"}, searchCall)
	if status != 401 {
		t.Fatalf("wrong key: status = %d", status)
	}
	status, _ = mcpCall(t, ts.URL, map[string]string{"Authorization": "Bearer secret", "X-Multica-Workspace-Id": "ws-1"}, searchCall)
	if status != 200 {
		t.Fatalf("good key: status = %d", status)
	}
}

func TestServer_HealthExempt(t *testing.T) {
	ts := newTestServer(t, "secret", &fakeWeKnoraCaller{})
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"workspaces":1`) {
		t.Fatalf("health: %d %s", resp.StatusCode, b)
	}
}

func TestServer_SearchToolEndToEnd(t *testing.T) {
	fake := &fakeWeKnoraCaller{searchResults: []json.RawMessage{json.RawMessage(`{"id":"c1","content":"bigtable numbers"}`)}}
	ts := newTestServer(t, "", fake)
	status, body := mcpCall(t, ts.URL, map[string]string{"X-Multica-Workspace-Id": "ws-1"}, searchCall)
	if status != 200 {
		t.Fatalf("status = %d: %s", status, body)
	}
	if !strings.Contains(body, `"success":true`) || !strings.Contains(body, "bigtable numbers") {
		t.Fatalf("body: %s", body)
	}
	// unmapped workspace → tool-level error payload, HTTP 200 (MCP layer)
	status, body = mcpCall(t, ts.URL, map[string]string{"X-Multica-Workspace-Id": "ws-other"}, searchCall)
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	if !strings.Contains(body, "WORKSPACE_NOT_MAPPED") {
		t.Fatalf("body: %s", body)
	}
}

func TestServer_IngestToolsHiddenWhenDisabled(t *testing.T) {
	ts := newTestServer(t, "", &fakeWeKnoraCaller{})
	listBody := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	_, body := mcpCall(t, ts.URL, nil, listBody)
	if strings.Contains(body, "add_document") {
		t.Fatal("add_document must not be registered when ingest disabled")
	}
	if !strings.Contains(body, "search_knowledge") {
		t.Fatal("search_knowledge must always be registered")
	}
}
```

Note `TestServer_IngestToolsHiddenWhenDisabled` needs `testEnv` (Task 5) to set `EnableIngest=false` — it does (default). Add one more test flipping it on:

```go
func TestServer_IngestToolsPresentWhenEnabled(t *testing.T) {
	env := testEnv(&fakeWeKnoraCaller{})
	env.Config.EnableIngest = true
	ts := httptest.NewServer(NewHandler(env, ""))
	t.Cleanup(ts.Close)
	_, body := mcpCall(t, ts.URL, nil, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if !strings.Contains(body, "add_document") || !strings.Contains(body, "delete_document") {
		t.Fatalf("ingest tools missing: %s", body[:min(400, len(body))])
	}
}
```

- [ ] **Step 1: Write failing tests** (`server_test.go`, code above)
- [ ] **Step 2: Run** `go test . -count=1` → Expected: FAIL (NewHandler undefined; package mode — the single-file form doesn't compile)
- [ ] **Step 3: Write middleware.go, server.go, main.go** (code above)
- [ ] **Step 4: Run** `go test ./... -count=1` → Expected: PASS (all)
- [ ] **Step 5: Manual smoke** — `go build -o weknora-mcp . && WEKNORA_BASE_URL=http://weknora.test WEKNORA_WORKSPACE_MAP=ws-1:kb-1 WEKNORA_WORKSPACE_ws-1_API_KEY=wk-a ./weknora-mcp &` then `curl -s localhost:8000/health` → expect `{"status":"ok","workspaces":1,"ingest":false,"s3":"not-configured"}`; kill the process
- [ ] **Step 6: Commit** `git add middleware.go server.go main.go server_test.go && git commit -m "feat: http assembly (health, mcp streamable, bearer auth)"`

---

## Task 7: README + workflows

**Files:**
- Create: `README.md`, `.github/workflows/build.yml`, `.github/workflows/release.yml`

`README.md` (Chinese, mirroring email-mcp README structure):

```markdown
# weknora-mcp

MCP Server，让 multica agent 通过 WeKnora 0.9.0 REST API 操作本工作区对应的知识库（KG）。
每个 multica 工作区映射一个 WeKnora 知识库，调用自动路由。

```
multica agent → multica-stdio-mcp-proxy → weknora-mcp :8000/mcp → WeKnora REST API
```

- Streamable HTTP transport（stateless、JSON response），Bearer 认证（MCP_AUTH_KEY）
- 工作区路由：X-Multica-Workspace-Id → WEKNORA_WORKSPACE_MAP → (kb_id, 每工作区独立 API Key)
- 10 个工具：检索 4（list_knowledge_bases / search_knowledge / list_documents /
  read_document）+ wiki 3（wiki_search / wiki_read_page / wiki_index）+
  写入 3（add_document / update_document / delete_document，默认关闭）
- add_document 三源：Markdown 文本 / URL / s3_key（S3 对象按后缀分流）
- 结果统一携带 multica_context 回显；错误带 error + error_code

## 配置

| 环境变量 | 必填 | 说明 |
| --- | --- | --- |
| WEKNORA_BASE_URL | 是 | WeKnora 实例地址 |
| WEKNORA_WORKSPACE_MAP | 是 | "ws-id:kb-id" 逗号分隔 |
| WEKNORA_WORKSPACE_<ws-id>_API_KEY | 是 | 每映射独立 WeKnora API Key |
| WEKNORA_WORKSPACE_<ws-id>_ALLOWED_AGENT_IDS | 否 | agent 白名单（前缀匹配） |
| WEKNORA_ENABLE_INGEST | 否 | true 开启写入工具，默认 false |
| MCP_AUTH_KEY | 否 | Bearer key，逗号分隔；空 = 无认证 |
| MCP_HOST / MCP_PORT | 否 | 默认 0.0.0.0:8000 |

S3（env-only，add_document 的 s3_key 源）：S3_BUCKET / AWS_ACCESS_KEY_ID /
AWS_SECRET_ACCESS_KEY（三缺一 = 软禁用）+ AWS_ENDPOINT_URL / S3_REGION /
S3_USE_PATH_STYLE / S3_KEY_PREFIXES / S3_MAX_FILE_BYTES。

## multica 接入

工作区 MCP 库条目（经 multica-stdio-mcp-proxy stdio 接入；一个 weknora-mcp 实例
服务多个工作区，数据按 workspace 天然隔离）：

```json
{
  "mcpServers": {
    "weknora-mcp": {
      "command": "multica-stdio-mcp-proxy",
      "args": [
        "--url", "http://weknora-mcp-host:8000/mcp",
        "--token", "<upstream-bearer-key>",
        "--allow-http"
      ]
    }
  }
}
```

## 工具说明

工具命名对齐 WeKnora 官方 MCP，便于 agent 认知迁移；差异：参数去掉 KB 选择
（服务端按 workspace 自动路由），输出统一附 multica_context 回显。
`grep_chunks`（无 REST 等价）与 `ask`（走 LLM）不提供。

### 只读 7 个（始终注册）

| 工具 | 参数 | 说明 |
| --- | --- | --- |
| `list_knowledge_bases` | 无 | 列出本映射 key 可见的知识库 |
| `search_knowledge` | `query`*、`mode`（hybrid/semantic/keyword，默认 hybrid）、`limit`（默认 10，上限 30）、`knowledge_ids`（可选数组，限定文档） | 语义/关键词/混合检索，返回相关段落 |
| `list_documents` | `keyword`、`page`、`page_size`（默认 20，上限 100）、`parse_status`、`file_type`、`sort_by`、`sort_order` | 分页列文档 |
| `read_document` | `knowledge_id`*、`page`、`page_size`（默认 10，上限 100）、`chunk_type`（可选数组，默认 text） | 按阅读顺序读文档分块 |
| `wiki_search` | `query`*、`regex`（bool，省略自动判断）、`limit`（默认 10，上限 50） | 按标题/别名/内容搜索 wiki 页面 |
| `wiki_read_page` | `slug`* | 读整页 Markdown 与元数据；出链自动内联摘要（上限 10 个，超出标 out_links_omitted） |
| `wiki_index` | `limit`（默认 50，上限 200） | wiki 目录：intro + 按类型分组 |

### 写入 3 个（`WEKNORA_ENABLE_INGEST=true` 时注册）

| 工具 | 参数 | 说明 |
| --- | --- | --- |
| `add_document` | `title`*、`content`\|`url`\|`s3_key`（互斥，恰好其一）、`publish`（默认 true） | 三源入库，异步提交即返回 document id；s3_key 按对象后缀分流（.md → 文本，其他 → 文件上传） |
| `update_document` | `knowledge_id`*、`content`*、`title`、`publish` | 替换 Markdown 文档内容/标题 |
| `delete_document` | `knowledge_id`* | 永久删除文档及索引数据（异步任务） |

### 错误码

| error_code | 场景 |
| --- | --- |
| `WORKSPACE_NOT_MAPPED` | 无 workspace header 或不在映射表 |
| `ACCESS_DENIED` | agent 不在白名单 |
| `MISSING_PARAM` / `INVALID_PARAM_TYPE` / `INVALID_PARAM_VALUE` | 参数校验 |
| `WEKNORA_API_FAILED` | WeKnora 返回 4xx/5xx（透传其 message） |
| `WEKNORA_TIMEOUT` | WeKnora 调用超时（30s） |
| `S3_UNAVAILABLE` / `S3_INVALID_KEY` / `S3_OBJECT_NOT_FOUND` / `S3_OBJECT_TOO_LARGE` / `S3_FETCH_FAILED` | S3 源 |
| `S3_FILE_TOO_LARGE_FOR_WEKNORA` | S3 对象超过 WeKnora 50MB 上限 |
| `INTERNAL` | 兜底（当前仅 marshal 失败；传输错误归入 WEKNORA_API_FAILED） |

## 部署

```bash
docker run -d -p 8000:8000 \
  -e WEKNORA_BASE_URL=http://your-weknora-host:28444 \
  -e WEKNORA_WORKSPACE_MAP="ws-uuid-1:kb-uuid-1" \
  -e WEKNORA_WORKSPACE_ws-uuid-1_API_KEY=wk-xxx \
  -e MCP_AUTH_KEY=bearer-key \
  ghcr.io/courage-zen/bigdata-mcp-weknora:v0.1.0
```

镜像：GHCR 多架构（amd64/arm64）；Release 附 linux 静态二进制与 air-gapped
镜像 tar（含 checksums）。
```

`.github/workflows/build.yml`:

```yaml
name: Build

on:
  push:
    branches: [main]
  pull_request:

permissions:
  contents: read

jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: "1.26"
          cache: true
      - run: go vet ./...
      - run: go test ./...
      - run: |
          CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /dev/null .
```

`.github/workflows/release.yml` — same structure as email-mcp's (test → binaries → images → publish), with three adaptations: binary name `weknora-mcp`, image-export `local_tag` prefix `weknora-mcp`, and the smoke test boots the server with the required env and verifies health + a `search_knowledge` tools/call returns `multica_context`:

```yaml
name: Release

# Sole owner of version-tag releases: binaries, images, and checksums all
# converge in the publish job, so checksums.txt covers every asset (the
# image-export job used to race in a separate workflow and its tarballs
# missed the checksum file).

on:
  push:
    tags:
      - "v*"

permissions:
  contents: write

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: "1.26"
          cache: true
      - run: go vet ./... && go test ./...

  binaries:
    runs-on: ubuntu-latest
    needs: test
    strategy:
      matrix:
        include:
          - arch: amd64
          - arch: arm64
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: "1.26"
          cache: true
      - name: Build (linux-${{ matrix.arch }})
        env:
          GOOS: linux
          GOARCH: ${{ matrix.arch }}
          CGO_ENABLED: "0"
        run: |
          go build -trimpath -ldflags='-s -w' -o weknora-mcp .
          mkdir -p release
          tar -czf "release/weknora-mcp-linux-${{ matrix.arch }}.tar.gz" weknora-mcp
      - name: Smoke test (amd64 only)
        if: matrix.arch == 'amd64'
        run: |
          WEKNORA_BASE_URL=http://weknora.invalid \
          WEKNORA_WORKSPACE_MAP=smoke-ws:smoke-kb \
          WEKNORA_WORKSPACE_smoke-ws_API_KEY=smoke-key \
          MCP_AUTH_KEY=smoke-key MCP_PORT=8123 ./weknora-mcp > server.log 2>&1 &
          server_pid=$!
          for i in $(seq 1 30); do
            curl -s -o /dev/null http://127.0.0.1:8123/health && break
            sleep 1
          done
          response="$(curl -s -X POST http://127.0.0.1:8123/mcp \
            -H "Authorization: Bearer smoke-key" \
            -H "Content-Type: application/json" \
            -H "Accept: application/json, text/event-stream" \
            -H "X-Multica-Workspace-Id: smoke-ws" \
            -d '{"jsonrpc":"2.0","method":"tools/call","params":{"name":"search_knowledge","arguments":{"query":"smoke"}},"id":2}')"
          echo "${response}" | grep -q "WEKNORA_API_FAILED" || {
            echo "smoke call did not reach the weknora error path"; cat server.log; exit 1;
          }
          kill "${server_pid}" || true
      - uses: actions/upload-artifact@v4
        with:
          name: release-binary-linux-${{ matrix.arch }}
          path: release/*
          if-no-files-found: error

  images:
    # Build and push the multi-arch image to ghcr.io, then export per-arch
    # tar.gz as artifacts for the publish job.
    runs-on: ubuntu-latest
    needs: test
    permissions:
      contents: read
      packages: write
    env:
      REGISTRY: ghcr.io
      IMAGE_NAME: ${{ github.repository }}
    steps:
      - name: Checkout
        uses: actions/checkout@v4

      - name: Set up QEMU
        uses: docker/setup-qemu-action@v3

      - name: Set up Docker Buildx
        uses: docker/setup-buildx-action@v3

      - name: Log in to GHCR
        uses: docker/login-action@v3
        with:
          registry: ${{ env.REGISTRY }}
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}

      - name: Derive image tags
        id: meta
        uses: docker/metadata-action@v5
        with:
          images: ${{ env.REGISTRY }}/${{ env.IMAGE_NAME }}
          tags: |
            type=ref,event=tag
            type=semver,pattern={{version}}
            type=semver,pattern={{major}}.{{minor}}
            type=sha,prefix=,format=short

      - name: Build and push
        uses: docker/build-push-action@v6
        with:
          context: .
          platforms: linux/amd64,linux/arm64
          push: true
          tags: ${{ steps.meta.outputs.tags }}
          labels: ${{ steps.meta.outputs.labels }}
          cache-from: type=gha
          cache-to: type=gha,mode=max

      - name: Export images per arch
        run: |
          tag="${GITHUB_REF_NAME}"   # e.g. v1.0.0
          ref="${{ env.REGISTRY }}/${{ env.IMAGE_NAME }}:${tag}"
          mkdir -p release
          for arch in amd64 arm64; do
            # Pull the arch-specific manifest digest, tag it with a clear name,
            # then save so `docker load` preserves the repository:tag label.
            digest=$(docker buildx imagetools inspect "${ref}" --format "{{range .Manifest.Manifests}}{{if eq .Platform.Architecture \"${arch}\"}}{{.Digest}}{{end}}{{end}}")
            local_tag="weknora-mcp:${tag}-${arch}"
            docker pull "${ref}@${digest}"
            docker tag "${ref}@${digest}" "${local_tag}"
            docker save "${local_tag}" | gzip > "release/weknora-mcp-image-${tag}-linux-${arch}.tar.gz"
          done
      - uses: actions/upload-artifact@v4
        with:
          name: release-image
          path: release/*
          if-no-files-found: error

  publish:
    runs-on: ubuntu-latest
    needs: [binaries, images]
    steps:
      - uses: actions/download-artifact@v4
        with:
          path: release-assets
          pattern: release-*
          merge-multiple: true
      - name: Checksums
        working-directory: release-assets
        run: |
          rm -f checksums.txt
          find . -type f ! -name "checksums.txt" -print0 | sort -z | xargs -0 sha256sum > checksums.txt
          cat checksums.txt
      - uses: softprops/action-gh-release@v2
        with:
          files: release-assets/*
          generate-release-notes: true
```

Note: the smoke test asserts the **error path** (`WEKNORA_API_FAILED` against the unresolvable `weknora.invalid`) — it proves routing, auth and the error envelope work end-to-end without a real WeKnora.

- [ ] **Step 1: Write README.md**
- [ ] **Step 2: Copy + adapt workflows**
- [ ] **Step 3: Commit** `git add README.md .github/ && git commit -m "docs: readme and ci workflows"`

---

## Task 8: Full verification + GitHub repo

- [ ] **Step 1: Run full suite** `go vet ./... && go test ./...` → Expected: all PASS
- [ ] **Step 2: Build release binary** `make build` → `./weknora-mcp` exists
- [ ] **Step 3: Create GitHub repo and push**

```bash
gh repo create courage-zen/bigdata-mcp-weknora --public --source . --push
```

(committer 已配置 GitHub noreply email，见 spec 部署节)

- [ ] **Step 4: Tag v0.1.0 and verify release workflow**

```bash
git tag v0.1.0 && git push origin v0.1.0
```

Watch the release workflow produce binaries + image; fix any workflow issues and re-tag if needed.

- [ ] **Step 5: Final commit of any verification fixes**

```bash
git add -A && git commit -m "chore: v0.1.0 verification fixes" || true
```

---

## Self-Review Notes (already applied)

- **Task order**: S3 task (4) precedes tools task (5) — tools.go references `ObjectStore`, `resolveInputKey`, `ErrAttachmentTooLarge` etc., which must exist first for every commit to build.
- Spec coverage: 10 tools (Task 5), mapping + per-mapping keys + agent allowlist (Task 2), per-mapping API key headers (Task 3), wiki inlining cap 10 (Task 5), S3 三源 with extension routing + 50MB WeKnora cap (Tasks 4-5), ingest switch (Tasks 5, 6), error codes (Tasks 3-5), health (Task 6), CI/CD + GitHub repo + git identity (Tasks 0, 7, 8), README (Task 7). `grep_chunks`/`ask` deliberately excluded per spec.
- Type consistency: `WeKnoraCaller` interface methods match fake + client + tool call sites; `toolResult.Data` as `json.RawMessage` single envelope; `WorkspaceMapping` fields consistent across config/tools/weknora; `S3Config` defined once in config.go (Task 2), consumed by loadS3Config (Task 4) and MinioObjectStore.
- No placeholders: every step carries complete code, full workflow YAML, or exact copy instructions with source paths.
- **s3_key path mirrors email-mcp order**: `resolveInputKey` validates (traversal / dangerous extension / prefix allowlist) **before** the fetch; oversize is caught pre-upload via `Get`'s caller limit (`ErrAttachmentTooLarge` → `S3_FILE_TOO_LARGE_FOR_WEKNORA`), distinct from the config cap (`ErrObjectTooLarge` → `S3_OBJECT_TOO_LARGE`).
