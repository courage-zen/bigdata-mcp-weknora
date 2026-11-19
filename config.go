package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

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
	BaseURL      string // weknora.base_url, no trailing slash
	Host         string // MCP_HOST, default 0.0.0.0
	Port         int    // MCP_PORT, default 8000
	EnableIngest bool   // weknora.enable_ingest, default false
	// Mappings keyed by configured id (full UUID or short prefix). Keyed
	// lookup is the hot path of every tool call.
	Mappings map[string]*WorkspaceMapping
	// S3 is nil when the trio is incomplete (s3_key source disabled).
	S3 *S3Config
}

// sanitizeAllowlist trims and drops empty entries. yaml list syntax lets
// "" and whitespace-only entries through; "" would prefix-match every
// agent id — a fail-open misconfig.
func sanitizeAllowlist(raw []string) []string {
	var out []string
	for _, a := range raw {
		if s := strings.TrimSpace(a); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// splitCSV splits a comma-separated env var into trimmed non-empty parts.
func splitCSV(raw string) []string {
	return sanitizeAllowlist(strings.Split(raw, ","))
}

// LoadConfig reads the YAML config file (path from the caller; empty means
// ./config.yaml), validates it fail-fast, then layers the env-only settings
// (MCP_HOST / MCP_PORT, the S3 contract — the family boundary: deployment
// plumbing stays env, weknora topology lives in the file). A missing or
// unparseable file is a startup error: there is no env fallback for the
// weknora section. Structure errors (empty base_url/workspaces, entry
// fields, duplicates) fire before the key-resolution error so operators fix
// the shape first, then the keys.
func LoadConfig(path string) (*Config, error) {
	usedDefault := path == ""
	if usedDefault {
		path = "config.yaml"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if usedDefault {
			return nil, fmt.Errorf("read config %s (default path; set WEKNORA_CONFIG to your config file): %w", path, err)
		}
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var fc fileConfig
	// KnownFields turns field typos into startup errors (an
	// "allowed_agents_ids" typo would otherwise silently drop the allowlist
	// — fail-open). Decode returns io.EOF for an empty or comment-only
	// file; treat that as an empty config and let the required-field
	// checks below give the friendlier error.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&fc); err != nil && !errors.Is(err, io.EOF) {
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
			AllowedAgentIDs: sanitizeAllowlist(w.AllowedAgentIDs),
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
	s3.MaxFileBytes = parseBytesEnv("S3_MAX_FILE_BYTES", s3Env("S3_MAX_FILE_BYTES", ""), 100*1024*1024)
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

// parseBytesEnv parses a byte-count env var; empty falls back to def, while
// unparseable or non-positive values log and fall back to def (same loud-
// misconfig policy as parseBoolEnv — a typo silently reverting a size cap
// is a deployment surprise).
func parseBytesEnv(name, raw string, def int64) int64 {
	if n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil && n > 0 {
		return n
	}
	if v := strings.TrimSpace(raw); v != "" {
		log.Printf("config: %s=%q is not a valid positive byte count, using %d", name, v, def)
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
