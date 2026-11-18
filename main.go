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
