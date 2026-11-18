package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const weknoraTimeout = 30 * time.Second

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
	AddManualDocument(ctx context.Context, m *WorkspaceMapping, title, content string, publish bool) (json.RawMessage, error)
	AddURLDocument(ctx context.Context, m *WorkspaceMapping, docURL, title string) (json.RawMessage, error)
	AddFileDocument(ctx context.Context, m *WorkspaceMapping, filename string, content []byte, contentType string) (json.RawMessage, error)
	UpdateDocument(ctx context.Context, m *WorkspaceMapping, knowledgeID, content, title string, publish bool) (json.RawMessage, error)
	DeleteDocument(ctx context.Context, m *WorkspaceMapping, knowledgeID string) (map[string]any, error)
}

// weknoraClient is the HTTP implementation over one shared http.Client.
type weknoraClient struct {
	base string
	hc   *http.Client
}

func NewWeKnoraClient(baseURL string) *weknoraClient {
	return &weknoraClient{
		base: strings.TrimRight(baseURL, "/"),
		hc:   &http.Client{Timeout: weknoraTimeout},
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
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20)) // 16 MiB response cap
	if err != nil {
		return nil, fmt.Errorf("read weknora response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, &weknoraError{Status: resp.StatusCode, Message: extractWeKnoraMessage(data)}
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
		return nil, fmt.Errorf("weknora response success=false")
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
		return nil, fmt.Errorf("weknora response success=false")
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
	raw, err := w.do(ctx, m, http.MethodGet, "/api/v1/knowledge-bases/"+m.KBID+"/knowledge?"+query.Encode(), "", nil)
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
	raw, err := w.do(ctx, m, http.MethodGet, "/api/v1/chunks/"+knowledgeID+"?"+query.Encode(), "", nil)
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
		"/api/v1/knowledgebase/"+m.KBID+"/wiki/search?"+url.Values{"q": {q}, "limit": {strconv.Itoa(limit)}}.Encode(), "", nil)
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
	// slug is a catch-all path segment: entity/rag → /pages/entity/rag
	raw, err := w.do(ctx, m, http.MethodGet, "/api/v1/knowledgebase/"+m.KBID+"/wiki/pages/"+slug, "", nil)
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
		"/api/v1/knowledgebase/"+m.KBID+"/wiki/index?"+url.Values{"limit": {strconv.Itoa(limit)}}.Encode(), "", nil)
	if err != nil {
		return nil, err
	}
	var idx map[string]any
	if err := json.Unmarshal(raw, &idx); err != nil {
		return nil, fmt.Errorf("decode weknora wiki index: %w", err)
	}
	return idx, nil
}

func (w *weknoraClient) AddManualDocument(ctx context.Context, m *WorkspaceMapping, title, content string, publish bool) (json.RawMessage, error) {
	raw, err := w.doJSON(ctx, m, http.MethodPost, "/api/v1/knowledge-bases/"+m.KBID+"/manual", map[string]any{
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
	raw, err := w.doJSON(ctx, m, http.MethodPost, "/api/v1/knowledge-bases/"+m.KBID+"/url", body)
	if err != nil {
		return nil, err
	}
	return unwrapDataObject(raw)
}

func (w *weknoraClient) AddFileDocument(ctx context.Context, m *WorkspaceMapping, filename string, content []byte, contentType string) (json.RawMessage, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
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
	raw, err := w.do(ctx, m, http.MethodPost, "/api/v1/knowledge-bases/"+m.KBID+"/file", mw.FormDataContentType(), &buf)
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
	raw, err := w.doJSON(ctx, m, http.MethodPut, "/api/v1/knowledge/manual/"+knowledgeID, body)
	if err != nil {
		return nil, err
	}
	return unwrapDataObject(raw)
}

func (w *weknoraClient) DeleteDocument(ctx context.Context, m *WorkspaceMapping, knowledgeID string) (map[string]any, error) {
	raw, err := w.do(ctx, m, http.MethodDelete, "/api/v1/knowledge/"+knowledgeID, "", nil)
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

// snippet returns up to 200 bytes of a raw response for error messages.
func snippet(raw []byte) string {
	if len(raw) > 200 {
		return string(raw[:200]) + "..."
	}
	return string(raw)
}
