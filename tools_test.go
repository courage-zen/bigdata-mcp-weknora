package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeWeKnoraCaller records calls and returns canned responses.
type fakeWeKnoraCaller struct {
	lastKBID         string
	lastAPIKey       string
	searchBody       map[string]any
	lastQuery        url.Values
	readChunksID     string
	wikiSearchQ      string
	wikiSearchLimit  int
	wikiIndexLimit   int
	wikiReadSlugs    []string
	addManualTitle   string
	addManualContent string
	addManualPub     bool
	addManualErr     error
	addURLURL        string
	addFileName      string
	addFileContent   []byte
	updateID         string
	updateTitle      string
	updatePub        bool
	deleteID         string

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
	f.lastQuery = query
	return nil, 0, nil
}
func (f *fakeWeKnoraCaller) ReadChunks(ctx context.Context, m *WorkspaceMapping, id string, query url.Values) ([]json.RawMessage, int, error) {
	f.readChunksID, f.lastQuery = id, query
	return nil, 0, nil
}
func (f *fakeWeKnoraCaller) WikiSearch(ctx context.Context, m *WorkspaceMapping, q string, limit int) ([]json.RawMessage, error) {
	f.wikiSearchQ, f.wikiSearchLimit = q, limit
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
	f.wikiIndexLimit = limit
	return map[string]any{"intro": "i"}, nil
}
func (f *fakeWeKnoraCaller) AddManualDocument(ctx context.Context, m *WorkspaceMapping, title, content string, publish bool) (json.RawMessage, error) {
	f.addManualTitle, f.addManualContent, f.addManualPub = title, content, publish
	if f.addManualErr != nil {
		return nil, f.addManualErr
	}
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
	f.updateID, f.updateTitle, f.updatePub = id, title, publish
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

// envX helpers call the named handler methods directly.
func envSearchKnowledge(env *ServerEnv, hdr http.Header, args map[string]any) []byte {
	return env.handleSearchKnowledge(context.Background(), hdr, args)
}
func envWikiReadPage(env *ServerEnv, hdr http.Header, args map[string]any) []byte {
	return env.handleWikiReadPage(context.Background(), hdr, args)
}
func envWikiSearch(env *ServerEnv, hdr http.Header, args map[string]any) []byte {
	return env.handleWikiSearch(context.Background(), hdr, args)
}
func envWikiIndex(env *ServerEnv, hdr http.Header, args map[string]any) []byte {
	return env.handleWikiIndex(context.Background(), hdr, args)
}
func envDeleteDocument(env *ServerEnv, hdr http.Header, args map[string]any) []byte {
	return env.handleDeleteDocument(context.Background(), hdr, args)
}
func envAddDocument(env *ServerEnv, hdr http.Header, args map[string]any) []byte {
	return env.handleAddDocument(context.Background(), hdr, args)
}
func envListDocuments(env *ServerEnv, hdr http.Header, args map[string]any) []byte {
	return env.handleListDocuments(context.Background(), hdr, args)
}
func envReadDocument(env *ServerEnv, hdr http.Header, args map[string]any) []byte {
	return env.handleReadDocument(context.Background(), hdr, args)
}
func envUpdateDocument(env *ServerEnv, hdr http.Header, args map[string]any) []byte {
	return env.handleUpdateDocument(context.Background(), hdr, args)
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

func TestMapWeKnoraError_TransportFailure(t *testing.T) {
	// A DNS/connection failure is neither *weknoraError nor a timeout —
	// it must surface as WEKNORA_API_FAILED (the Task 7 release smoke
	// depends on this against an unresolvable WEKNORA_BASE_URL), not
	// INTERNAL.
	payload := mapWeKnoraError(errors.New("weknora request failed: dial tcp: lookup weknora.invalid: no such host"), nil)
	if r := decodePayload(t, payload); r.ErrorCode != "WEKNORA_API_FAILED" {
		t.Fatalf("code = %q, want WEKNORA_API_FAILED", r.ErrorCode)
	}
	// timeouts stay distinct
	payload = mapWeKnoraError(errors.New("context deadline exceeded"), nil)
	if r := decodePayload(t, payload); r.ErrorCode != "WEKNORA_TIMEOUT" {
		t.Fatalf("code = %q, want WEKNORA_TIMEOUT", r.ErrorCode)
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
			OutLinkDetails  []map[string]any `json:"out_link_details"`
			OutLinksOmitted []string         `json:"out_links_omitted"`
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

func TestWikiSearch_RegexModes(t *testing.T) {
	env := testEnv(&fakeWeKnoraCaller{})

	// regex omitted + compileable query → forwarded untouched
	envWikiSearch(env, wsHeaders(), map[string]any{"query": "rag pipeline"})
	if got := env.WeKnora.(*fakeWeKnoraCaller).wikiSearchQ; got != "rag pipeline" {
		t.Errorf("auto compileable: q = %q", got)
	}

	// regex omitted + invalid regex → QuoteMeta fallback (official auto-detect)
	envWikiSearch(env, wsHeaders(), map[string]any{"query": "(unclosed"})
	if got := env.WeKnora.(*fakeWeKnoraCaller).wikiSearchQ; got != regexp.QuoteMeta("(unclosed") {
		t.Errorf("auto invalid: q = %q, want QuoteMeta", got)
	}

	// regex=false → always literal
	envWikiSearch(env, wsHeaders(), map[string]any{"query": "a.b", "regex": false})
	if got := env.WeKnora.(*fakeWeKnoraCaller).wikiSearchQ; got != regexp.QuoteMeta("a.b") {
		t.Errorf("explicit false: q = %q", got)
	}

	// regex=true → forwarded untouched even when invalid (WeKnora reports it)
	envWikiSearch(env, wsHeaders(), map[string]any{"query": "(unclosed", "regex": true})
	if got := env.WeKnora.(*fakeWeKnoraCaller).wikiSearchQ; got != "(unclosed" {
		t.Errorf("explicit true: q = %q", got)
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
	if fake.addManualContent != "# hi" {
		t.Errorf("manual content = %q, want the fetched S3 object", fake.addManualContent)
	}
	store := env.Store.(*fakeObjectStore)
	if store.gotKey != "docs/a.md" || store.gotLimit != weknoraMaxFileBytes {
		t.Errorf("store.Get key=%q limit=%d", store.gotKey, store.gotLimit)
	}

	// .md with publish:false keeps the document as draft
	env.Store = &fakeObjectStore{content: []byte("# draft")}
	payload = envAddDocument(env, wsHeaders(), map[string]any{"title": "T", "s3_key": "docs/d.md", "publish": false})
	if r := decodePayload(t, payload); !r.Success {
		t.Fatalf("payload: %s", payload)
	}
	if fake.addManualPub != false {
		t.Errorf("publish = %v, want false", fake.addManualPub)
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

func TestListDocuments_ParamPassThrough(t *testing.T) {
	fake := &fakeWeKnoraCaller{}
	env := testEnv(fake)
	payload := envListDocuments(env, wsHeaders(), map[string]any{
		"keyword": "report", "page": 2.0, "page_size": 50.0, "parse_status": "completed",
	})
	if r := decodePayload(t, payload); !r.Success {
		t.Fatalf("payload: %s", payload)
	}
	if fake.lastQuery.Get("keyword") != "report" || fake.lastQuery.Get("page") != "2" ||
		fake.lastQuery.Get("page_size") != "50" || fake.lastQuery.Get("parse_status") != "completed" {
		t.Fatalf("query: %v", fake.lastQuery)
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
	types := fake.lastQuery["chunk_type"]
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

func TestAddDocument_S3UploadFailureIsWeKnoraError(t *testing.T) {
	// Store.Get succeeds, the WeKnora manual upload fails with HTTP 500:
	// must surface as WEKNORA_API_FAILED (the s3 phases are fine), not
	// S3_FETCH_FAILED.
	fake := &fakeWeKnoraCaller{addManualErr: &weknoraError{Status: 500, Message: "boom"}}
	env := testEnv(fake)
	env.Store = &fakeObjectStore{content: []byte("# hi")}
	payload := envAddDocument(env, wsHeaders(), map[string]any{"title": "T", "s3_key": "docs/a.md"})
	if r := decodePayload(t, payload); r.ErrorCode != "WEKNORA_API_FAILED" {
		t.Fatalf("code = %q, want WEKNORA_API_FAILED", r.ErrorCode)
	}
}

func TestWikiSlug_DotSegmentsRejected(t *testing.T) {
	fake := &fakeWeKnoraCaller{}
	env := testEnv(fake)
	for _, slug := range []string{"../x", "a/../b", "entity/.", "./entity/x"} {
		payload := envWikiReadPage(env, wsHeaders(), map[string]any{"slug": slug})
		if r := decodePayload(t, payload); r.ErrorCode != "INVALID_PARAM_VALUE" {
			t.Fatalf("slug %q: code = %q, want INVALID_PARAM_VALUE", slug, r.ErrorCode)
		}
	}
	if len(fake.wikiReadSlugs) != 0 {
		t.Fatalf("rejected slugs reached the client: %v", fake.wikiReadSlugs)
	}
	// structural slashes remain valid
	payload := envWikiReadPage(env, wsHeaders(), map[string]any{"slug": "entity/rag"})
	if r := decodePayload(t, payload); !r.Success {
		t.Fatalf("multi-segment slug must pass: %s", payload)
	}
}

func TestUpdateDocument_WhitespaceContentRejected(t *testing.T) {
	fake := &fakeWeKnoraCaller{}
	env := testEnv(fake)
	payload := envUpdateDocument(env, wsHeaders(), map[string]any{"knowledge_id": "k-1", "content": "   "})
	if r := decodePayload(t, payload); r.ErrorCode != "MISSING_PARAM" {
		t.Fatalf("code = %q, want MISSING_PARAM", r.ErrorCode)
	}
	if fake.updateID != "" {
		t.Fatalf("whitespace content reached the client: %q", fake.updateID)
	}
}

func TestUpdateDocument_HappyPath(t *testing.T) {
	fake := &fakeWeKnoraCaller{updateTitle: "T2", updatePub: false}
	env := testEnv(fake)
	payload := envUpdateDocument(env, wsHeaders(), map[string]any{
		"knowledge_id": "k-1", "content": "# new", "title": "T2", "publish": false,
	})
	if r := decodePayload(t, payload); !r.Success {
		t.Fatalf("payload: %s", payload)
	}
	if fake.updateID != "k-1" {
		t.Errorf("update id = %q", fake.updateID)
	}
	if fake.updateTitle != "T2" {
		t.Errorf("update title = %q", fake.updateTitle)
	}
	if fake.updatePub {
		t.Errorf("update publish = true, want false passthrough")
	}
}

func TestDeleteDocument_HappyPath(t *testing.T) {
	fake := &fakeWeKnoraCaller{}
	env := testEnv(fake)
	payload := envDeleteDocument(env, wsHeaders(), map[string]any{"knowledge_id": "k-2"})
	if r := decodePayload(t, payload); !r.Success {
		t.Fatalf("payload: %s", payload)
	}
	if fake.deleteID != "k-2" {
		t.Fatalf("delete id = %q, want k-2", fake.deleteID)
	}
}

func TestWikiSearchAndIndex_HappyPath(t *testing.T) {
	fake := &fakeWeKnoraCaller{}
	env := testEnv(fake)

	payload := envWikiSearch(env, wsHeaders(), map[string]any{"query": "hello", "limit": 25.0})
	if r := decodePayload(t, payload); !r.Success {
		t.Fatalf("search payload: %s", payload)
	}
	if fake.wikiSearchQ != "hello" {
		t.Errorf("search q = %q", fake.wikiSearchQ)
	}
	if fake.wikiSearchLimit != 25 {
		t.Errorf("search limit = %d, want 25", fake.wikiSearchLimit)
	}

	payload = envWikiIndex(env, wsHeaders(), map[string]any{"limit": 77.0})
	if r := decodePayload(t, payload); !r.Success {
		t.Fatalf("index payload: %s", payload)
	}
	if fake.wikiIndexLimit != 77 {
		t.Errorf("index limit = %d, want 77", fake.wikiIndexLimit)
	}
}

func TestReadDocument_TrimsKnowledgeID(t *testing.T) {
	fake := &fakeWeKnoraCaller{}
	env := testEnv(fake)
	payload := envReadDocument(env, wsHeaders(), map[string]any{"knowledge_id": " k-9 \n"})
	if r := decodePayload(t, payload); !r.Success {
		t.Fatalf("payload: %s", payload)
	}
	if fake.readChunksID != "k-9" {
		t.Fatalf("id = %q, want trimmed k-9", fake.readChunksID)
	}
}

func TestSchemaLeniency_SDKValidationLetsHandlerErrorsThrough(t *testing.T) {
	fake := &fakeWeKnoraCaller{}
	env := testEnv(fake)
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "v0"}, nil)
	RegisterTools(server, env)
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(r *http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	t.Cleanup(ts.Close)

	call := func(body string) string {
		req, err := http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(body))
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
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	// absent required param → handler MISSING_PARAM envelope, not an SDK
	// schema rejection (no error_code would appear)
	body := call(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_knowledge","arguments":{}}}`)
	if !strings.Contains(body, "MISSING_PARAM") {
		t.Fatalf("absent query: %s", body)
	}
	// explicit nulls on optional params pass through to the handler
	body = call(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_knowledge","arguments":{"query":"q","mode":null,"limit":null}}}`)
	if !strings.Contains(body, `"success":true`) {
		t.Fatalf("null optionals: %s", body)
	}
	// explicit null on a required-in-spirit param → handler envelope
	body = call(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_knowledge","arguments":{"query":null}}}`)
	if !strings.Contains(body, "MISSING_PARAM") {
		t.Fatalf("null query: %s", body)
	}
	// bogus enum value → handler INVALID_PARAM_VALUE envelope
	body = call(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_knowledge","arguments":{"query":"q","mode":"bogus"}}}`)
	if !strings.Contains(body, "INVALID_PARAM_VALUE") {
		t.Fatalf("bogus mode: %s", body)
	}
}
