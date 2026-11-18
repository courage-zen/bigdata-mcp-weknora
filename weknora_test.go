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
