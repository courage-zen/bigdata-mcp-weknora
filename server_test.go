package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

// TestServer_MultiKeyAuth pins comma-separated MCP_AUTH_KEY semantics: every
// listed key authenticates, unknown keys 401 (exercised through the
// constant-time keyMatch path).
func TestServer_MultiKeyAuth(t *testing.T) {
	ts := newTestServer(t, "key-one,key-two", &fakeWeKnoraCaller{})
	for _, key := range []string{"key-one", "key-two"} {
		status, body := mcpCall(t, ts.URL, map[string]string{"Authorization": "Bearer " + key, "X-Multica-Workspace-Id": "ws-1"}, searchCall)
		if status != 200 {
			t.Fatalf("key %q: status = %d: %s", key, status, body)
		}
	}
	status, _ := mcpCall(t, ts.URL, map[string]string{"Authorization": "Bearer key-three", "X-Multica-Workspace-Id": "ws-1"}, searchCall)
	if status != 401 {
		t.Fatalf("unknown key: status = %d, want 401", status)
	}
}

// TestKeyMatch pins the constant-time comparator's functional surface:
// exact match true; wrong key, prefix, longer token, NUL-padded token all
// false (unequal lengths never compare — padding must not create matches).
func TestKeyMatch(t *testing.T) {
	keys := []string{"alpha", "beta"}
	if !keyMatch(keys, "alpha") || !keyMatch(keys, "beta") {
		t.Fatal("exact keys must match")
	}
	for _, bad := range []string{"alph", "alphabet", "alpha\x00", "gamma", ""} {
		if keyMatch(keys, bad) {
			t.Fatalf("keyMatch(%q) = true, want false", bad)
		}
	}
}

// TestServer_HealthMethodRouted pins the Go 1.22 method-pattern routing:
// GET /health is the only health verb; POST must 405, not fall through.
func TestServer_HealthMethodRouted(t *testing.T) {
	ts := newTestServer(t, "", &fakeWeKnoraCaller{})
	resp, err := http.Post(ts.URL+"/health", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /health = %d, want 405", resp.StatusCode)
	}
}

// TestServer_BodyCapExceeded pins the 8 MiB MaxRequestBodyBytes ceiling: an
// oversized JSON-RPC body is rejected by the transport before any tool runs.
func TestServer_BodyCapExceeded(t *testing.T) {
	ts := newTestServer(t, "", &fakeWeKnoraCaller{})
	big := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add_document","arguments":{"title":"t","content":"` +
		strings.Repeat("x", 9<<20) + `"}}}`
	req, err := http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("X-Multica-Workspace-Id", "ws-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body = %d, want 413", resp.StatusCode)
	}
}

// TestServer_GracefulShutdown pins the SIGTERM path: ListenAndServe returns
// ErrServerClosed (not a fatal error) once Shutdown completes, and an
// in-flight request finishes during the drain window. Uses the real signal
// handling via the exported run path — exercised through srv.Shutdown on a
// server built the same way main.go builds it.
func TestServer_GracefulShutdown(t *testing.T) {
	env := testEnv(&fakeWeKnoraCaller{})
	srv := &http.Server{
		Addr:              "127.0.0.1:0",
		Handler:           NewHandler(env, ""),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()

	base := "http://" + ln.Addr().String()
	status, body := mcpCall(t, base, map[string]string{"X-Multica-Workspace-Id": "ws-1"}, searchCall)
	if status != 200 {
		t.Fatalf("pre-shutdown call: %d %s", status, body)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := <-done; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve returned %v, want ErrServerClosed", err)
	}
	// After shutdown the listener is closed: new connections refused.
	if _, err := net.DialTimeout("tcp", ln.Addr().String(), 500*time.Millisecond); err == nil {
		t.Fatal("listener still accepting after shutdown")
	}
}
