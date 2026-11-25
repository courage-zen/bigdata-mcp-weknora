package main

import (
	"bytes"
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

// TestWeKnoraClient_IngestPaths pins the add-source endpoints against the
// WeKnora routes: uploads live under /knowledge-bases/:id/knowledge/{manual,
// url,file} — the /knowledge segment is part of the route (registered in
// routes_knowledge.go), NOT /knowledge-bases/:id/{manual,url,file}.
// v0.2.0 shipped the missing-segment paths and WeKnora 404'd every write.
func TestWeKnoraClient_IngestPaths(t *testing.T) {
	var gotPath, gotMethod string
	respondOK := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"success":true,"data":{"id":"k9"}}`)
	}
	c, m := fakeWeKnora(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		respondOK(w)
	})
	if _, err := c.AddManualDocument(context.Background(), m, "T", "# hi", true); err != nil {
		t.Fatal(err)
	}
	if want := "/api/v1/knowledge-bases/kb-1/knowledge/manual"; gotPath != want || gotMethod != "POST" {
		t.Fatalf("manual: path=%q method=%q, want %s %s", gotPath, gotMethod, "POST", want)
	}
	if _, err := c.AddURLDocument(context.Background(), m, "http://x.test/a", ""); err != nil {
		t.Fatal(err)
	}
	if want := "/api/v1/knowledge-bases/kb-1/knowledge/url"; gotPath != want {
		t.Fatalf("url: path=%q, want %q", gotPath, want)
	}
	if _, err := c.AddFileDocument(context.Background(), m, "a.csv", []byte("x"), "text/csv"); err != nil {
		t.Fatal(err)
	}
	if want := "/api/v1/knowledge-bases/kb-1/knowledge/file"; gotPath != want {
		t.Fatalf("file: path=%q, want %q", gotPath, want)
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

// bodyCapture captures the JSON body of any request into got and returns a
// client+mapping pair against a canned-OK server.
func bodyCapture(t *testing.T, got *map[string]any) (*weknoraClient, *WorkspaceMapping) {
	t.Helper()
	return fakeWeKnora(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(got)
		io.WriteString(w, `{"slug":"s","version":2}`)
	})
}

// TestWeKnoraClient_WikiWritePaths pins the wiki write endpoints against the
// WeKnora routes: pages live under /knowledgebase/:id/wiki/{pages,move-page,
// folders} — singular knowledgebase, and move-page is a PUT with the slug in
// the body (registered in routes_knowledge.go:354-372), unlike the document
// routes' hyphenated knowledge-bases prefix.
func TestWeKnoraClient_WikiWritePaths(t *testing.T) {
	var gotPath, gotMethod string
	respond := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}
	c, m := fakeWeKnora(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		switch {
		case strings.HasSuffix(r.URL.Path, "/wiki/folders"):
			respond(w, `{"id":"f-1","name":"AI"}`)
		case strings.HasSuffix(r.URL.Path, "/wiki/move-page"):
			respond(w, `{"slug":"entity/rag","folder_id":"f-1"}`)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default: // create/update page
			respond(w, `{"slug":"entity/rag","title":"RAG","version":2}`)
		}
	})

	if _, err := c.WikiCreatePage(context.Background(), m, map[string]any{"slug": "entity/rag", "title": "RAG", "content": "# RAG"}); err != nil {
		t.Fatal(err)
	}
	if want := "/api/v1/knowledgebase/kb-1/wiki/pages"; gotPath != want || gotMethod != "POST" {
		t.Fatalf("create: %s %s, want POST %s", gotMethod, gotPath, want)
	}

	if _, err := c.WikiUpdatePage(context.Background(), m, "entity/rag", map[string]any{"content": "# new", "version": 2.0}); err != nil {
		t.Fatal(err)
	}
	if want := "/api/v1/knowledgebase/kb-1/wiki/pages/entity/rag"; gotPath != want || gotMethod != "PUT" {
		t.Fatalf("update: %s %s, want PUT %s", gotMethod, gotPath, want)
	}

	if err := c.WikiDeletePage(context.Background(), m, "entity/rag"); err != nil {
		t.Fatal(err)
	}
	if want := "/api/v1/knowledgebase/kb-1/wiki/pages/entity/rag"; gotPath != want || gotMethod != "DELETE" {
		t.Fatalf("delete: %s %s, want DELETE %s", gotMethod, gotPath, want)
	}

	if _, err := c.WikiCreateFolder(context.Background(), m, "AI", ""); err != nil {
		t.Fatal(err)
	}
	if want := "/api/v1/knowledgebase/kb-1/wiki/folders"; gotPath != want || gotMethod != "POST" {
		t.Fatalf("folder: %s %s, want POST %s", gotMethod, gotPath, want)
	}

	if _, err := c.WikiMovePage(context.Background(), m, "entity/rag", "f-1"); err != nil {
		t.Fatal(err)
	}
	if want := "/api/v1/knowledgebase/kb-1/wiki/move-page"; gotPath != want || gotMethod != "PUT" {
		t.Fatalf("move: %s %s, want PUT %s", gotMethod, gotPath, want)
	}
}

// The server merges WikiPageUpdateRequest pointer fields: absent fields keep
// their stored value, so the client must not serialize fields the caller did
// not send — sending them (even as zero values) would clobber real data.
func TestWeKnoraClient_WikiUpdateOmitsAbsentFields(t *testing.T) {
	var gotBody map[string]any
	c, m := fakeWeKnora(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		io.WriteString(w, `{"slug":"s","version":2}`)
	})
	c.WikiUpdatePage(context.Background(), m, "s", map[string]any{"content": "# new"})
	for _, absent := range []string{"title", "summary", "status", "page_type", "aliases", "version"} {
		if _, ok := gotBody[absent]; ok {
			t.Errorf("absent field %q must not be sent: %v", absent, gotBody)
		}
	}
	if gotBody["content"] != "# new" {
		t.Errorf("content = %v", gotBody["content"])
	}
}

func TestWeKnoraClient_WikiCreatePageSendsAllParams(t *testing.T) {
	var gotBody map[string]any
	c, m := bodyCapture(t, &gotBody)
	if _, err := c.WikiCreatePage(context.Background(), m, map[string]any{
		"slug": "entity/rag", "title": "RAG", "content": "c", "summary": "s",
		"page_type": "entity", "status": "published",
		"aliases":   []any{"Retrieval Augmented Generation"},
		"source_refs": []any{"k-1|RAG 论文"},
		"folder_id":    "f-1",
	}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"slug", "title", "content", "summary", "page_type", "status", "aliases", "source_refs", "folder_id"} {
		if _, ok := gotBody[k]; !ok {
			t.Errorf("body missing %q: %v", k, gotBody)
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
		fh, hdr, err := r.FormFile("file")
		if err != nil {
			t.Errorf("form file: %v", err)
			return
		}
		defer fh.Close()
		if hdr.Filename != "report.csv" {
			t.Errorf("file part filename = %q", hdr.Filename)
		}
		if ct := hdr.Header.Get("Content-Type"); ct != "text/csv" {
			t.Errorf("file part content-type = %q", ct)
		}
		if r.FormValue("fileName") != "report.csv" {
			t.Errorf("fileName = %q", r.FormValue("fileName"))
		}
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

func TestWeKnoraClient_PathEscaping(t *testing.T) {
	var gotPath, gotQuery string
	c, m := fakeWeKnora(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		io.WriteString(w, `{"slug":"x","title":"x","out_links":[]}`)
	})
	// % in a slug previously failed request-build; ? previously leaked
	// into the query string; both must stay in the path, percent-escaped.
	if _, err := c.WikiReadPage(context.Background(), m, "entity/50%load?x=1"); err != nil {
		t.Fatal(err)
	}
	if want := "/api/v1/knowledgebase/kb-1/wiki/pages/entity/50%load?x=1"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if gotQuery != "" {
		t.Errorf("query = %q, want empty", gotQuery)
	}
	// Multi-segment slugs keep their slashes: entity/rag stays two segments.
	if _, err := c.WikiReadPage(context.Background(), m, "entity/rag"); err != nil {
		t.Fatal(err)
	}
	if want := "/api/v1/knowledgebase/kb-1/wiki/pages/entity/rag"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
}

func TestWeKnoraClient_ResponseCap(t *testing.T) {
	c, m := fakeWeKnora(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(bytes.Repeat([]byte("x"), 16<<20+1024))
	})
	_, err := c.ListKnowledgeBases(context.Background(), m)
	if err == nil || !strings.Contains(err.Error(), "16 MiB cap") {
		t.Fatalf("want cap error, got %v", err)
	}
}

func TestWeKnoraClient_CtxCanceled(t *testing.T) {
	c, m := fakeWeKnora(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"success":true,"data":[]}`)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.ListKnowledgeBases(ctx, m); err != context.Canceled {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestWeKnoraClient_SuccessFalseIsError(t *testing.T) {
	c, m := fakeWeKnora(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"success":false,"error":{"message":"kb not found"}}`)
	})
	if _, err := c.ListKnowledgeBases(context.Background(), m); err == nil || !strings.Contains(err.Error(), "success=false") {
		t.Fatalf("want success=false error, got %v", err)
	}
}

func TestWeKnoraClient_NoRedirectFollow(t *testing.T) {
	var targetHits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits++
		io.WriteString(w, `{"success":true,"data":[]}`)
	}))
	defer target.Close()
	c, m := fakeWeKnora(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL+"/api/v1/knowledge-bases")
		w.WriteHeader(http.StatusFound)
	})
	if _, err := c.ListKnowledgeBases(context.Background(), m); err == nil {
		t.Fatal("redirect body must not decode as a success envelope")
	}
	if targetHits != 0 {
		t.Fatalf("redirect was followed: %d hits on the redirect target", targetHits)
	}
}
