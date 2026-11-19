package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func main() {
	cfg, err := LoadConfig(os.Getenv("WEKNORA_CONFIG"))
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

	// Graceful shutdown: SIGINT/SIGTERM stop accepting connections and give
	// in-flight requests up to 15s to finish (k8s rolling updates send
	// SIGTERM before killing the pod). The signal context exiting without
	// a second signal means shutdown timed out — exit non-zero so the
	// supervisor restarts rather than serving half-dead.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop() // restore default signal behavior: a second signal force-exits
		log.Printf("shutting down (waiting up to 15s for in-flight requests)")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("listen: %v", err)
	}
}
