package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const weknoraTimeout = 30 * time.Second

// maxResponseBytes caps how much of any WeKnora response is read; a body
// beyond it is rejected with an explicit error instead of surfacing as a
// confusing JSON decode failure on truncated data.
const maxResponseBytes = 16 << 20

// weknoraError carries the HTTP status and the message extracted from a
// WeKnora error body (both body shapes handled).
type weknoraError struct {
	Status  int
	Message string
}

func (e *weknoraError) Error() string { return fmt.Sprintf("weknora api %d: %s", e.Status, e.Message) }

// WeKnoraCaller abstracts the REST calls tools make (mockable in tests).
type WeKnoraCaller interface {
	ListKnowledgeBases(ctx context.Context, m *WorkspaceMapping) ([]json.RawMessage, error)
	SearchKnowledge(ctx context.Context, m *WorkspaceMapping, body map[string]any) ([]json.RawMessage, error)
	ListDocuments(ctx context.Context, m *WorkspaceMapping, query url.Values) ([]json.RawMessage, int, error)
	ReadChunks(ctx context.Context, m *WorkspaceMapping, knowledgeID string, query url.Values) ([]json.RawMessage, int, error)
	WikiSearch(ctx context.Context, m *WorkspaceMapping, q string, limit int) ([]json.RawMessage, error)
	WikiReadPage(ctx context.Context, m *WorkspaceMapping, slug string) (map[string]any, error)
	WikiIndex(ctx context.Context, m *WorkspaceMapping, limit int) (map[string]any, error)
	WikiCreatePage(ctx context.Context, m *WorkspaceMapping, page map[string]any) (map[string]any, error)
	WikiUpdatePage(ctx context.Context, m *WorkspaceMapping, slug string, fields map[string]any) (map[string]any, error)
	WikiDeletePage(ctx context.Context, m *WorkspaceMapping, slug string) error
	WikiCreateFolder(ctx context.Context, m *WorkspaceMapping, name, parentID string) (map[string]any, error)
	WikiMovePage(ctx context.Context, m *WorkspaceMapping, slug, folderID string) (map[string]any, error)
	AddManualDocument(ctx context.Context, m *WorkspaceMapping, title, content string, publish bool) (json.RawMessage, error)
	AddURLDocument(ctx context.Context, m *WorkspaceMapping, docURL, title string) (json.RawMessage, error)
	AddFileDocument(ctx context.Context, m *WorkspaceMapping, filename string, content []byte, contentType string) (json.RawMessage, error)
	UpdateDocument(ctx context.Context, m *WorkspaceMapping, knowledgeID, content, title string, publish bool) (json.RawMessage, error)
	DeleteDocument(ctx context.Context, m *WorkspaceMapping, knowledgeID string) (map[string]any, error)
}

var _ WeKnoraCaller = (*weknoraClient)(nil)

// weknoraClient is the HTTP implementation over one shared http.Client.
type weknoraClient struct {
	base string
	hc   *http.Client
}

func NewWeKnoraClient(baseURL string) *weknoraClient {
	return &weknoraClient{
		base: strings.TrimRight(baseURL, "/"),
		hc: &http.Client{
			Timeout: weknoraTimeout,
			// Never follow redirects: Go's client only strips Authorization
			// and Cookie headers cross-host, so a 3xx from a gateway would
			// hand the workspace API key to whatever host the redirect names.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// do performs one request with the mapping's API key and unwraps the
// response. ok shape: {"success":true,...}; wiki endpoints return the
// bare object. Errors map to *weknoraError with the extracted message.
func (w *weknoraClient) do(ctx context.Context, m *WorkspaceMapping, method, path string, contentType string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, w.base+path, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("X-API-Key", m.APIKey)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := w.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("weknora request failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read weknora response: %w", err)
	}
	if len(data) > maxResponseBytes {
		return nil, fmt.Errorf("weknora response exceeded the %d MiB cap", maxResponseBytes>>20)
	}
	if resp.StatusCode >= 400 {
		return nil, &weknoraError{Status: resp.StatusCode, Message: snippet([]byte(extractWeKnoraMessage(data)))}
	}
	return data, nil
}

// extractWeKnoraMessage pulls the human-readable message out of both
// WeKnora error body shapes: {"error":{"message":"..."}} and {"error":"..."}.
func extractWeKnoraMessage(data []byte) string {
	var wrapped struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &wrapped) == nil && wrapped.Error.Message != "" {
		return wrapped.Error.Message
	}
	var bare struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &bare) == nil && bare.Error != "" {
		return bare.Error
	}
	return strings.TrimSpace(string(data))
}

// doJSON is do + JSON body marshalling.
func (w *weknoraClient) doJSON(ctx context.Context, m *WorkspaceMapping, method, path string, payload any) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	return w.do(ctx, m, method, path, "application/json", bytes.NewReader(raw))
}

// unwrapData extracts the "data" array of a {"success":true,"data":[...]}
// response envelope.
func unwrapData(raw []byte) ([]json.RawMessage, error) {
	var env struct {
		Success bool              `json:"success"`
		Data    []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode weknora response: %w", err)
	}
	if !env.Success {
		return nil, fmt.Errorf("weknora response success=false: %s", snippet(raw))
	}
	return env.Data, nil
}

// unwrapDataObject extracts the "data" object of the envelope.
func unwrapDataObject(raw []byte) (json.RawMessage, error) {
	var env struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode weknora response: %w", err)
	}
	if !env.Success {
		return nil, fmt.Errorf("weknora response success=false: %s", snippet(raw))
	}
	return env.Data, nil
}

func (w *weknoraClient) ListKnowledgeBases(ctx context.Context, m *WorkspaceMapping) ([]json.RawMessage, error) {
	raw, err := w.do(ctx, m, http.MethodGet, "/api/v1/knowledge-bases", "", nil)
	if err != nil {
		return nil, err
	}
	return unwrapData(raw)
}

func (w *weknoraClient) SearchKnowledge(ctx context.Context, m *WorkspaceMapping, body map[string]any) ([]json.RawMessage, error) {
	raw, err := w.doJSON(ctx, m, http.MethodPost, "/api/v1/knowledge-search", body)
	if err != nil {
		return nil, err
	}
	return unwrapData(raw)
}

func (w *weknoraClient) ListDocuments(ctx context.Context, m *WorkspaceMapping, query url.Values) ([]json.RawMessage, int, error) {
	raw, err := w.do(ctx, m, http.MethodGet, "/api/v1/knowledge-bases/"+pathSegments(m.KBID)+"/knowledge?"+query.Encode(), "", nil)
	if err != nil {
		return nil, 0, err
	}
	var env struct {
		Success bool              `json:"success"`
		Data    []json.RawMessage `json:"data"`
		Total   int               `json:"total"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || !env.Success {
		return nil, 0, fmt.Errorf("decode weknora response: %s", snippet(raw))
	}
	return env.Data, env.Total, nil
}

func (w *weknoraClient) ReadChunks(ctx context.Context, m *WorkspaceMapping, knowledgeID string, query url.Values) ([]json.RawMessage, int, error) {
	raw, err := w.do(ctx, m, http.MethodGet, "/api/v1/chunks/"+pathSegments(knowledgeID)+"?"+query.Encode(), "", nil)
	if err != nil {
		return nil, 0, err
	}
	var env struct {
		Success bool              `json:"success"`
		Data    []json.RawMessage `json:"data"`
		Total   int               `json:"total"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || !env.Success {
		return nil, 0, fmt.Errorf("decode weknora response: %s", snippet(raw))
	}
	return env.Data, env.Total, nil
}

func (w *weknoraClient) WikiSearch(ctx context.Context, m *WorkspaceMapping, q string, limit int) ([]json.RawMessage, error) {
	raw, err := w.do(ctx, m, http.MethodGet,
		"/api/v1/knowledgebase/"+pathSegments(m.KBID)+"/wiki/search?"+url.Values{"q": {q}, "limit": {strconv.Itoa(limit)}}.Encode(), "", nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Pages []json.RawMessage `json:"pages"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode weknora wiki search: %w", err)
	}
	return resp.Pages, nil
}

func (w *weknoraClient) WikiReadPage(ctx context.Context, m *WorkspaceMapping, slug string) (map[string]any, error) {
	// slugs are multi-segment path identifiers; each segment is
	// percent-escaped, slashes preserved.
	raw, err := w.do(ctx, m, http.MethodGet, "/api/v1/knowledgebase/"+pathSegments(m.KBID)+"/wiki/pages/"+pathSegments(slug), "", nil)
	if err != nil {
		return nil, err
	}
	var page map[string]any
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, fmt.Errorf("decode weknora wiki page: %w", err)
	}
	return page, nil
}

func (w *weknoraClient) WikiIndex(ctx context.Context, m *WorkspaceMapping, limit int) (map[string]any, error) {
	raw, err := w.do(ctx, m, http.MethodGet,
		"/api/v1/knowledgebase/"+pathSegments(m.KBID)+"/wiki/index?"+url.Values{"limit": {strconv.Itoa(limit)}}.Encode(), "", nil)
	if err != nil {
		return nil, err
	}
	var idx map[string]any
	if err := json.Unmarshal(raw, &idx); err != nil {
		return nil, fmt.Errorf("decode weknora wiki index: %w", err)
	}
	return idx, nil
}

// WikiCreatePage posts a full page (slug/title/content/summary/page_type/
// status/aliases/source_refs/folder_id) — the body is the caller-built map,
// so absent fields simply stay absent.
func (w *weknoraClient) WikiCreatePage(ctx context.Context, m *WorkspaceMapping, page map[string]any) (map[string]any, error) {
	raw, err := w.doJSON(ctx, m, http.MethodPost, "/api/v1/knowledgebase/"+pathSegments(m.KBID)+"/wiki/pages", page)
	if err != nil {
		return nil, err
	}
	var created map[string]any
	if err := json.Unmarshal(raw, &created); err != nil {
		return nil, fmt.Errorf("decode weknora wiki page: %w", err)
	}
	return created, nil
}

// WikiUpdatePage merges fields onto an existing page. The server treats
// absent request fields as "keep the stored value", so fields must only be
// present when the caller sent them.
func (w *weknoraClient) WikiUpdatePage(ctx context.Context, m *WorkspaceMapping, slug string, fields map[string]any) (map[string]any, error) {
	raw, err := w.doJSON(ctx, m, http.MethodPut, "/api/v1/knowledgebase/"+pathSegments(m.KBID)+"/wiki/pages/"+pathSegments(slug), fields)
	if err != nil {
		return nil, err
	}
	var updated map[string]any
	if err := json.Unmarshal(raw, &updated); err != nil {
		return nil, fmt.Errorf("decode weknora wiki page: %w", err)
	}
	return updated, nil
}

func (w *weknoraClient) WikiDeletePage(ctx context.Context, m *WorkspaceMapping, slug string) error {
	_, err := w.do(ctx, m, http.MethodDelete, "/api/v1/knowledgebase/"+pathSegments(m.KBID)+"/wiki/pages/"+pathSegments(slug), "", nil)
	return err
}

func (w *weknoraClient) WikiCreateFolder(ctx context.Context, m *WorkspaceMapping, name, parentID string) (map[string]any, error) {
	raw, err := w.doJSON(ctx, m, http.MethodPost, "/api/v1/knowledgebase/"+pathSegments(m.KBID)+"/wiki/folders", map[string]any{
		"name":      name,
		"parent_id": parentID,
	})
	if err != nil {
		return nil, err
	}
	var folder map[string]any
	if err := json.Unmarshal(raw, &folder); err != nil {
		return nil, fmt.Errorf("decode weknora wiki folder: %w", err)
	}
	return folder, nil
}

// WikiMovePage relocates a page into a folder; folderID "" moves it to the
// wiki root. The slug travels in the body (not the path) because slugs are
// hierarchical and would collide with the catch-all /pages/{slug} route.
func (w *weknoraClient) WikiMovePage(ctx context.Context, m *WorkspaceMapping, slug, folderID string) (map[string]any, error) {
	raw, err := w.doJSON(ctx, m, http.MethodPut, "/api/v1/knowledgebase/"+pathSegments(m.KBID)+"/wiki/move-page", map[string]any{
		"slug":      slug,
		"folder_id": folderID,
	})
	if err != nil {
		return nil, err
	}
	var page map[string]any
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, fmt.Errorf("decode weknora wiki page: %w", err)
	}
	return page, nil
}

func (w *weknoraClient) AddManualDocument(ctx context.Context, m *WorkspaceMapping, title, content string, publish bool) (json.RawMessage, error) {
	raw, err := w.doJSON(ctx, m, http.MethodPost, "/api/v1/knowledge-bases/"+pathSegments(m.KBID)+"/knowledge/manual", map[string]any{
		"title":   title,
		"content": content,
		"status":  manualStatus(publish),
		"channel": "mcp",
	})
	if err != nil {
		return nil, err
	}
	return unwrapDataObject(raw)
}

func (w *weknoraClient) AddURLDocument(ctx context.Context, m *WorkspaceMapping, docURL, title string) (json.RawMessage, error) {
	body := map[string]any{"url": docURL, "channel": "mcp"}
	if title != "" {
		body["title"] = title
	}
	raw, err := w.doJSON(ctx, m, http.MethodPost, "/api/v1/knowledge-bases/"+pathSegments(m.KBID)+"/knowledge/url", body)
	if err != nil {
		return nil, err
	}
	return unwrapDataObject(raw)
}

func (w *weknoraClient) AddFileDocument(ctx context.Context, m *WorkspaceMapping, filename string, content []byte, contentType string) (json.RawMessage, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	hdr := textproto.MIMEHeader{}
	// stdlib's escaping (backslash, quote, and CR/LF as %0D/%0A) — a
	// hand-rolled version that skips CR/LF would allow header-splitting
	// from a hostile filename.
	hdr.Set("Content-Disposition", multipart.FileContentDisposition("file", filename))
	if contentType != "" {
		hdr.Set("Content-Type", contentType)
	} else {
		hdr.Set("Content-Type", "application/octet-stream")
	}
	fw, err := mw.CreatePart(hdr)
	if err != nil {
		return nil, fmt.Errorf("build multipart: %w", err)
	}
	if _, err := fw.Write(content); err != nil {
		return nil, fmt.Errorf("build multipart: %w", err)
	}
	_ = mw.WriteField("fileName", filename)
	_ = mw.WriteField("channel", "mcp")
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("build multipart: %w", err)
	}
	raw, err := w.do(ctx, m, http.MethodPost, "/api/v1/knowledge-bases/"+pathSegments(m.KBID)+"/knowledge/file", mw.FormDataContentType(), &buf)
	if err != nil {
		return nil, err
	}
	return unwrapDataObject(raw)
}

func (w *weknoraClient) UpdateDocument(ctx context.Context, m *WorkspaceMapping, knowledgeID, content, title string, publish bool) (json.RawMessage, error) {
	body := map[string]any{"content": content, "status": manualStatus(publish), "channel": "mcp"}
	if title != "" {
		body["title"] = title
	}
	raw, err := w.doJSON(ctx, m, http.MethodPut, "/api/v1/knowledge/manual/"+pathSegments(knowledgeID), body)
	if err != nil {
		return nil, err
	}
	return unwrapDataObject(raw)
}

func (w *weknoraClient) DeleteDocument(ctx context.Context, m *WorkspaceMapping, knowledgeID string) (map[string]any, error) {
	raw, err := w.do(ctx, m, http.MethodDelete, "/api/v1/knowledge/"+pathSegments(knowledgeID), "", nil)
	if err != nil {
		return nil, err
	}
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode weknora response: %w", err)
	}
	return env, nil
}

// manualStatus maps publish→WeKnora manual-knowledge status.
func manualStatus(publish bool) string {
	if publish {
		return "publish"
	}
	return "draft"
}

// snippet returns up to 200 bytes of a raw response for error messages,
// backing up to a rune boundary so multi-byte text isn't split mid-character.
func snippet(raw []byte) string {
	if len(raw) > 200 {
		cut := 200
		for cut > 0 && !utf8.RuneStart(raw[cut]) {
			cut--
		}
		return string(raw[:cut]) + "..."
	}
	return string(raw)
}

// pathSegments percent-escapes each /-separated segment of a path
// parameter while preserving the slash structure: wiki slugs are
// multi-segment identifiers (entity/rag → two escaped segments), so
// url.PathEscape alone would wrongly escape the separators too.
// knowledgeID and KBID go through the same helper for uniformity.
func pathSegments(s string) string {
	parts := strings.Split(s, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}
