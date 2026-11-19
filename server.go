package main

import (
	"fmt"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// version is the server version reported in MCP server info. Overridden at
// build time: -ldflags="-X main.version=$(git describe --tags)"; bare
// `go build` / `go test` carry "dev".
var version = "dev"

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

	server := mcp.NewServer(&mcp.Implementation{Name: "weknora-mcp", Version: version}, nil)
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
