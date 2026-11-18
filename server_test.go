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
