package main

import (
	"fmt"
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
