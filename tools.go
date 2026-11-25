package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ServerEnv carries the dependencies shared by tool handlers.
type ServerEnv struct {
	Config  *Config
	WeKnora WeKnoraCaller
	Store   ObjectStore // nil = s3_key source unavailable
}

// Error codes carried in failed tool payloads' error_code field.
const (
	errCodeWorkspaceNotMapped  = "WORKSPACE_NOT_MAPPED"
	errCodeAccessDenied        = "ACCESS_DENIED"
	errCodeMissingParam        = "MISSING_PARAM"
	errCodeInvalidParamType    = "INVALID_PARAM_TYPE"
	errCodeInvalidParamValue   = "INVALID_PARAM_VALUE"
	errCodeWeKnoraFailed       = "WEKNORA_API_FAILED"
	errCodeWeKnoraTimeout      = "WEKNORA_TIMEOUT"
	errCodeS3Unavailable       = "S3_UNAVAILABLE"
	errCodeS3InvalidKey        = "S3_INVALID_KEY"
	errCodeS3ObjectNotFound    = "S3_OBJECT_NOT_FOUND"
	errCodeS3ObjectTooLarge    = "S3_OBJECT_TOO_LARGE"
	errCodeS3FetchFailed       = "S3_FETCH_FAILED"
	errCodeS3FileTooLargeForWK = "S3_FILE_TOO_LARGE_FOR_WEKNORA"
	errCodeInternal            = "INTERNAL"
)

// toolResult is the JSON payload every tool returns.
type toolResult struct {
	Success        bool              `json:"success"`
	Error          string            `json:"error,omitempty"`
	ErrorCode      string            `json:"error_code,omitempty"`
	MulticaContext map[string]string `json:"multica_context"`
	// one-of payload fields set per tool (json.RawMessage keeps the
	// upstream shape without re-typing every WeKnora struct)
	Data json.RawMessage `json:"data,omitempty"`
}

func errPayload(code, errMsg string, mc map[string]string) []byte {
	raw, _ := json.Marshal(toolResult{
		Success:        false,
		Error:          errMsg,
		ErrorCode:      code,
		MulticaContext: mc,
	})
	return raw
}

// resolveMapping applies the two access gates every tool shares:
// workspace must be mapped, agent must pass the mapping's allowlist.
func (env *ServerEnv) resolveMapping(mc map[string]string) (*WorkspaceMapping, []byte) {
	ws := mc["workspace_id"]
	if ws == "" {
		return nil, errPayload(errCodeWorkspaceNotMapped,
			"no workspace identity: this tool must be called through multica (X-Multica-Workspace-Id header required)", mc)
	}
	m, ok := env.Config.MappingFor(ws)
	if !ok {
		return nil, errPayload(errCodeWorkspaceNotMapped,
			fmt.Sprintf("workspace %q is not mapped to a weknora knowledge base; ask the operator to add it to weknora.workspaces in the config file", ws), mc)
	}
	agentID := mc["agent_id"]
	if agentID != "" && !m.AgentAllowed(agentID) {
		return nil, errPayload(errCodeAccessDenied,
			fmt.Sprintf("agent %q is not allowed to access this workspace's knowledge base", agentID), mc)
	}
	return m, nil
}

// mapWeKnoraError converts a client error into a tool payload, mapping
// timeouts, HTTP statuses and transport failures onto the error-code
// vocabulary.
func mapWeKnoraError(err error, mc map[string]string) []byte {
	if we, ok := err.(*weknoraError); ok {
		return errPayload(errCodeWeKnoraFailed,
			fmt.Sprintf("weknora api error (HTTP %d): %s", we.Status, we.Message), mc)
	}
	if strings.Contains(err.Error(), "context deadline exceeded") {
		return errPayload(errCodeWeKnoraTimeout, "weknora api call timed out (30s)", mc)
	}
	// Transport failures (DNS, connection refused): the call to the
	// weknora API failed without an HTTP status — same caller-facing
	// meaning as an API error, and the error text (e.g. "weknora request
	// failed: dial tcp: lookup weknora.invalid: no such host") already
	// says what broke.
	return errPayload(errCodeWeKnoraFailed, err.Error(), mc)
}

// validateKnowledgeID guards the path-bound knowledge_id parameter:
// WeKnora knowledge IDs are opaque server-generated tokens, never
// multi-segment (only wiki slugs are, and they go through WikiReadPage),
// so / or .. in an ID is an attempt to reach another endpoint under the
// workspace API key. "?" and "#" are refused too: the ID is interpolated
// into a request path before the query string, so a crafted ? could
// smuggle extra query parameters (k1?admin=1) into the call.
func validateKnowledgeID(id string) error {
	if strings.Contains(id, "/") || strings.Contains(id, "..") || strings.Contains(id, "?") || strings.Contains(id, "#") {
		return fmt.Errorf("parameter \"knowledge_id\" must be a document id, not a path")
	}
	return nil
}

// validateWikiSlug guards the slug of wiki_read_page: slugs are
// multi-segment identifiers, so "/" is structural — but a "." or ".."
// segment would be resolved by any normalizing proxy in front of
// WeKnora and could climb out of the /wiki/pages/{kb}/ prefix to read
// another knowledge base under this workspace's API key.
func validateWikiSlug(slug string) error {
	for _, seg := range strings.Split(slug, "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("parameter \"slug\" must be a wiki page slug, not a path")
		}
	}
	return nil
}

// RegisterTools registers all tools on the server. Write tools register
// only when ingest is enabled (deployment-level switch).
func RegisterTools(server *mcp.Server, env *ServerEnv) {
	addRetrieveTools(server, env)
	addWikiTools(server, env)
	if env.Config.EnableIngest {
		addIngestTools(server, env)
		addWikiWriteTools(server, env)
	}
}

// callTool wraps the email-mcp AddTool pattern: parse args map, run fn,
// double-surface the payload (structuredContent + TextContent).
func callTool(server *mcp.Server, tool *mcp.Tool, fn func(ctx context.Context, hdr http.Header, args map[string]any) []byte) {
	mcp.AddTool(server, tool, func(ctx context.Context, req *mcp.CallToolRequest, in any) (*mcp.CallToolResult, any, error) {
		var hdr map[string][]string
		if req.Extra != nil && req.Extra.Header != nil {
			hdr = req.Extra.Header
		}
		var args map[string]any
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				mc := MulticaContextFromHeaders(hdr)
				payload := errPayload(errCodeInvalidParamValue, "invalid arguments: "+err.Error(), mc)
				return &mcp.CallToolResult{
					IsError:           true,
					Content:           []mcp.Content{&mcp.TextContent{Text: string(payload)}},
					StructuredContent: json.RawMessage(payload),
				}, nil, nil
			}
		}
		payload := fn(ctx, http.Header(hdr), args)
		isErr := !jsonPayloadSuccess(payload)
		return &mcp.CallToolResult{
			IsError:           isErr,
			Content:           []mcp.Content{&mcp.TextContent{Text: string(payload)}},
			StructuredContent: json.RawMessage(payload),
		}, nil, nil
	})
}

func jsonPayloadSuccess(payload []byte) bool {
	var probe struct {
		Success bool `json:"success"`
	}
	return json.Unmarshal(payload, &probe) == nil && probe.Success
}

// addRetrieveTools registers the four read-only knowledge-base tools.
func addRetrieveTools(server *mcp.Server, env *ServerEnv) {
	callTool(server, &mcp.Tool{
		Name:        "list_knowledge_bases",
		Description: "List the knowledge bases visible to this workspace's weknora API key, with id, name and description. Other tools operate on the knowledge base this multica workspace is mapped to.",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, env.handleListKnowledgeBases)
	callTool(server, &mcp.Tool{
		Name: "search_knowledge",
		Description: "Search this workspace's weknora knowledge base and return the most relevant passages with document ids and titles. " +
			"mode hybrid (default) combines semantic and keyword retrieval; semantic ranks by meaning; keyword matches exact terms, identifiers or codes.",
		InputSchema: json.RawMessage(`{
	"type":"object",
	"properties":{
		"query":{"description":"natural-language question, topic, or exact terms to match (required)","anyOf":[{"type":"string"},{"type":"null"}]},
		"mode":{"description":"retrieval mode: hybrid (default), semantic, or keyword","anyOf":[{"type":"string"},{"type":"null"}]},
		"limit":{"description":"max passages, default 10, max 30","anyOf":[{"type":"number"},{"type":"null"}]},
		"knowledge_ids":{"description":"optional document ids to restrict the search (from list_documents or previous results)","anyOf":[{"type":"array","items":{"type":"string"}},{"type":"null"}]}
	}
}`),
	}, env.handleSearchKnowledge)
	callTool(server, &mcp.Tool{
		Name:        "list_documents",
		Description: "List the documents in this workspace's weknora knowledge base with pagination and optional filters. Returns document ids for read_document.",
		InputSchema: json.RawMessage(`{
	"type":"object",
	"properties":{
		"keyword":{"description":"optional substring to filter document titles","anyOf":[{"type":"string"},{"type":"null"}]},
		"page":{"description":"1-based page number, default 1","anyOf":[{"type":"number"},{"type":"null"}]},
		"page_size":{"description":"documents per page, default 20, max 100","anyOf":[{"type":"number"},{"type":"null"}]},
		"parse_status":{"description":"filter by parse status (e.g. completed, parsing, failed)","anyOf":[{"type":"string"},{"type":"null"}]},
		"file_type":{"description":"filter by file extension","anyOf":[{"type":"string"},{"type":"null"}]},
		"sort_by":{"description":"sort field: created_at (default), updated_at, or file_name","anyOf":[{"type":"string"},{"type":"null"}]},
		"sort_order":{"description":"asc or desc (default desc)","anyOf":[{"type":"string"},{"type":"null"}]}
	}
}`),
	}, env.handleListDocuments)
	callTool(server, &mcp.Tool{
		Name:        "read_document",
		Description: "Read a document's text chunks in reading order from this workspace's weknora knowledge base. Page through with page/page_size.",
		InputSchema: json.RawMessage(`{
	"type":"object",
	"properties":{
		"knowledge_id":{"description":"document id from list_documents or search results (required)","anyOf":[{"type":"string"},{"type":"null"}]},
		"page":{"description":"1-based page number, default 1","anyOf":[{"type":"number"},{"type":"null"}]},
		"page_size":{"description":"chunks per page, default 10, max 100","anyOf":[{"type":"number"},{"type":"null"}]},
		"chunk_type":{"description":"chunk types to return, default [\"text\"]","anyOf":[{"type":"array","items":{"type":"string"}},{"type":"null"}]}
	}
}`),
	}, env.handleReadDocument)
}

// addWikiTools registers the three wiki browsing tools.
func addWikiTools(server *mcp.Server, env *ServerEnv) {
	callTool(server, &mcp.Tool{
		Name:        "wiki_search",
		Description: "Search the wiki pages of this workspace's weknora knowledge base by title, alias and content. Returns page slugs for wiki_read_page.",
		InputSchema: json.RawMessage(`{
	"type":"object",
	"properties":{
		"query":{"description":"search terms or a regular expression (case-insensitive POSIX) (required)","anyOf":[{"type":"string"},{"type":"null"}]},
		"regex":{"description":"false forces literal match; true treats the query as regex; omit to auto-detect (invalid regex falls back to literal)","anyOf":[{"type":"boolean"},{"type":"null"}]},
		"limit":{"description":"max pages, default 10, max 50","anyOf":[{"type":"number"},{"type":"null"}]}
	}
}`),
	}, env.handleWikiSearch)
	callTool(server, &mcp.Tool{
		Name:        "wiki_read_page",
		Description: "Read one wiki page by slug, including content, metadata and out-links with inlined summaries (up to 10).",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"slug":{"description":"page slug from wiki_search or wiki_index, e.g. concept/rag (required)","anyOf":[{"type":"string"},{"type":"null"}]}}}`),
	}, env.handleWikiReadPage)
	callTool(server, &mcp.Tool{
		Name:        "wiki_index",
		Description: "Browse the wiki table of contents of this workspace's weknora knowledge base: intro plus pages grouped by type.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"limit":{"description":"max entries per page type, default 50, max 200","anyOf":[{"type":"number"},{"type":"null"}]}}}`),
	}, env.handleWikiIndex)
}

// addIngestTools registers the write tools (add/update/delete document);
// only called when env.Config.EnableIngest is set.
func addIngestTools(server *mcp.Server, env *ServerEnv) {
	callTool(server, &mcp.Tool{
		Name: "add_document",
		Description: "Add a document to this workspace's weknora knowledge base from Markdown text, a URL, or an S3 object (s3_key). " +
			"Parsing and indexing happen asynchronously; the response returns the new document id.",
		InputSchema: json.RawMessage(`{
	"type":"object",
	"properties":{
		"title":{"description":"document title (required)","anyOf":[{"type":"string"},{"type":"null"}]},
		"content":{"description":"Markdown content; required unless url or s3_key is given","anyOf":[{"type":"string"},{"type":"null"}]},
		"url":{"description":"web page or file URL to import; required unless content or s3_key is given","anyOf":[{"type":"string"},{"type":"null"}]},
		"s3_key":{"description":"object key (or full object-store URL) in the configured bucket; required unless content or url is given","anyOf":[{"type":"string"},{"type":"null"}]},
		"publish":{"description":"publish immediately (default true) or keep as draft","anyOf":[{"type":"boolean"},{"type":"null"}]}
	}
}`),
	}, env.handleAddDocument)
	callTool(server, &mcp.Tool{
		Name:        "update_document",
		Description: "Replace the content (and optionally the title) of a Markdown document created from text. Re-indexing happens asynchronously.",
		InputSchema: json.RawMessage(`{
	"type":"object",
	"properties":{
		"knowledge_id":{"description":"document id (required)","anyOf":[{"type":"string"},{"type":"null"}]},
		"content":{"description":"new Markdown content (required)","anyOf":[{"type":"string"},{"type":"null"}]},
		"title":{"description":"new title; keeps the current title when omitted","anyOf":[{"type":"string"},{"type":"null"}]},
		"publish":{"description":"publish (default true) or keep as draft","anyOf":[{"type":"boolean"},{"type":"null"}]}
	}
}`),
	}, env.handleUpdateDocument)
	callTool(server, &mcp.Tool{
		Name:        "delete_document",
		Description: "Permanently delete a document and its index data from this workspace's weknora knowledge base.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"knowledge_id":{"description":"document id (required)","anyOf":[{"type":"string"},{"type":"null"}]}}}`),
	}, env.handleDeleteDocument)
}

// addWikiWriteTools registers the wiki write tools (create/update/delete
// page, create folder, move page); only called when
// env.Config.EnableIngest is set — wiki routes demand the same ingest
// capability as document writes.
func addWikiWriteTools(server *mcp.Server, env *ServerEnv) {
	callTool(server, &mcp.Tool{
		Name: "wiki_create_page",
		Description: "Create a wiki page in this workspace's weknora knowledge base. " +
			"Slugs are unique per knowledge base: creating a page whose slug already exists fails on the server. " +
			"source_refs entries have the format \"<knowledge_id>\" or \"<knowledge_id>|<document title>\".",
		InputSchema: json.RawMessage(`{
	"type":"object",
	"properties":{
		"slug":{"description":"unique page slug, e.g. entity/acme or summary/<knowledge_id> (required)","anyOf":[{"type":"string"},{"type":"null"}]},
		"title":{"description":"page title","anyOf":[{"type":"string"},{"type":"null"}]},
		"content":{"description":"markdown content; [[slug|name]] links are parsed into the page graph","anyOf":[{"type":"string"},{"type":"null"}]},
		"summary":{"description":"one-line summary for index listings","anyOf":[{"type":"string"},{"type":"null"}]},
		"page_type":{"description":"one of summary, entity, concept, index, synthesis, comparison","anyOf":[{"type":"string"},{"type":"null"}]},
		"status":{"description":"draft or published (default published)","anyOf":[{"type":"string"},{"type":"null"}]},
		"aliases":{"description":"alternate names, abbreviations or translations","anyOf":[{"type":"array","items":{"type":"string"}},{"type":"null"}]},
		"source_refs":{"description":"source documents the page summarizes, as \"<knowledge_id>\" or \"<knowledge_id>|<title>\"","anyOf":[{"type":"array","items":{"type":"string"}},{"type":"null"}]},
		"folder_id":{"description":"parent folder id; empty means wiki root","anyOf":[{"type":"string"},{"type":"null"}]}
	}
}`),
	}, env.handleWikiCreatePage)
	callTool(server, &mcp.Tool{
		Name:        "wiki_update_page",
		Description: "Update a wiki page by slug. Absent fields keep their stored value (server-side merge semantics); " +
			"version, when given, must match the page's current version or the update is rejected as a conflict.",
		InputSchema: json.RawMessage(`{
	"type":"object",
	"properties":{
		"slug":{"description":"page slug to update (required)","anyOf":[{"type":"string"},{"type":"null"}]},
		"title":{"description":"new title; keeps the current title when omitted","anyOf":[{"type":"string"},{"type":"null"}]},
		"content":{"description":"new markdown content; keeps the current content when omitted","anyOf":[{"type":"string"},{"type":"null"}]},
		"summary":{"description":"new one-line summary; keeps the current summary when omitted","anyOf":[{"type":"string"},{"type":"null"}]},
		"page_type":{"description":"one of summary, entity, concept, index, synthesis, comparison","anyOf":[{"type":"string"},{"type":"null"}]},
		"status":{"description":"one of draft, published, archived","anyOf":[{"type":"string"},{"type":"null"}]},
		"aliases":{"description":"replacement alias list (whole-list replace)","anyOf":[{"type":"array","items":{"type":"string"}},{"type":"null"}]},
		"version":{"description":"current page version for optimistic locking; 0/omitted skips the check","anyOf":[{"type":"number"},{"type":"null"}]}
	}
}`),
	}, env.handleWikiUpdatePage)
	callTool(server, &mcp.Tool{
		Name:        "wiki_delete_page",
		Description: "Permanently delete a wiki page by slug (hard delete; fails with 404 when the page does not exist).",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"slug":{"description":"page slug to delete (required)","anyOf":[{"type":"string"},{"type":"null"}]}}}`),
	}, env.handleWikiDeletePage)
	callTool(server, &mcp.Tool{
		Name:        "wiki_create_folder",
		Description: "Create a wiki folder (directory node) in this workspace's knowledge base; returns the folder with its id.",
		InputSchema: json.RawMessage(`{
	"type":"object",
	"properties":{
		"name":{"description":"folder name (required)","anyOf":[{"type":"string"},{"type":"null"}]},
		"parent_id":{"description":"parent folder id; empty means top level","anyOf":[{"type":"string"},{"type":"null"}]}
	}
}`),
	}, env.handleWikiCreateFolder)
	callTool(server, &mcp.Tool{
		Name:        "wiki_move_page",
		Description: "Move a wiki page into a folder (or to the wiki root with an empty folder_id); the folder id comes from wiki_create_folder or the wiki index.",
		InputSchema: json.RawMessage(`{
	"type":"object",
	"properties":{
		"slug":{"description":"page slug to move (required)","anyOf":[{"type":"string"},{"type":"null"}]},
		"folder_id":{"description":"target folder id; empty means wiki root","anyOf":[{"type":"string"},{"type":"null"}]}
	}
}`),
	}, env.handleWikiMovePage)
}

func (env *ServerEnv) handleListKnowledgeBases(ctx context.Context, hdr http.Header, args map[string]any) []byte {
	mc := MulticaContextFromHeaders(hdr)
	m, fail := env.resolveMapping(mc)
	if fail != nil {
		return fail
	}
	kbs, err := env.WeKnora.ListKnowledgeBases(ctx, m)
	if err != nil {
		return mapWeKnoraError(err, mc)
	}
	return successPayload(map[string]any{"knowledge_bases": kbs}, mc)
}

func (env *ServerEnv) handleSearchKnowledge(ctx context.Context, hdr http.Header, args map[string]any) []byte {
	mc := MulticaContextFromHeaders(hdr)
	m, fail := env.resolveMapping(mc)
	if fail != nil {
		return fail
	}
	query, _ := args["query"].(string)
	if strings.TrimSpace(query) == "" {
		return errPayload(errCodeMissingParam, "parameter \"query\" is required", mc)
	}
	mode := "hybrid"
	if v, present := args["mode"]; present && v != nil {
		s, ok := v.(string)
		if !ok || (s != "hybrid" && s != "semantic" && s != "keyword") {
			return errPayload(errCodeInvalidParamValue, "parameter \"mode\" must be hybrid, semantic or keyword", mc)
		}
		mode = s
	}
	limit := 10
	if v, present := args["limit"]; present && v != nil {
		f, ok := v.(float64)
		if !ok || f < 1 || f > 30 {
			return errPayload(errCodeInvalidParamValue, "parameter \"limit\" must be a number between 1 and 30", mc)
		}
		limit = int(f)
	}
	var knowledgeIDs []string
	if v, present := args["knowledge_ids"]; present && v != nil {
		list, ok := v.([]any)
		if !ok {
			return errPayload(errCodeInvalidParamType, "parameter \"knowledge_ids\" must be an array of strings", mc)
		}
		for _, item := range list {
			s, _ := item.(string)
			if s = strings.TrimSpace(s); s != "" {
				knowledgeIDs = append(knowledgeIDs, s)
			}
		}
	}
	body := map[string]any{
		"query":              query,
		"knowledge_base_ids": []string{m.KBID},
		"match_count":        limit,
	}
	switch mode {
	case "semantic":
		body["disable_keywords_match"] = true
	case "keyword":
		body["disable_vector_match"] = true
	}
	if len(knowledgeIDs) > 0 {
		body["knowledge_ids"] = knowledgeIDs
	}
	results, err := env.WeKnora.SearchKnowledge(ctx, m, body)
	if err != nil {
		return mapWeKnoraError(err, mc)
	}
	return successPayload(map[string]any{"results": results}, mc)
}

func (env *ServerEnv) handleListDocuments(ctx context.Context, hdr http.Header, args map[string]any) []byte {
	mc := MulticaContextFromHeaders(hdr)
	m, fail := env.resolveMapping(mc)
	if fail != nil {
		return fail
	}
	q := url.Values{}
	page, pageSize := 1, 20
	if v, present := args["page"]; present && v != nil {
		f, ok := v.(float64)
		if !ok || f < 1 {
			return errPayload(errCodeInvalidParamValue, "parameter \"page\" must be a positive number", mc)
		}
		page = int(f)
	}
	if v, present := args["page_size"]; present && v != nil {
		f, ok := v.(float64)
		if !ok || f < 1 || f > 100 {
			return errPayload(errCodeInvalidParamValue, "parameter \"page_size\" must be a number between 1 and 100", mc)
		}
		pageSize = int(f)
	}
	q.Set("page", strconv.Itoa(page))
	q.Set("page_size", strconv.Itoa(pageSize))
	for _, p := range []string{"keyword", "parse_status", "file_type", "sort_by", "sort_order"} {
		if s, _ := args[p].(string); strings.TrimSpace(s) != "" {
			q.Set(p, strings.TrimSpace(s))
		}
	}
	docs, total, err := env.WeKnora.ListDocuments(ctx, m, q)
	if err != nil {
		return mapWeKnoraError(err, mc)
	}
	return successPayload(map[string]any{
		"documents": docs, "total": total, "page": page, "page_size": pageSize,
	}, mc)
}

func (env *ServerEnv) handleReadDocument(ctx context.Context, hdr http.Header, args map[string]any) []byte {
	mc := MulticaContextFromHeaders(hdr)
	m, fail := env.resolveMapping(mc)
	if fail != nil {
		return fail
	}
	knowledgeID, _ := args["knowledge_id"].(string)
	if strings.TrimSpace(knowledgeID) == "" {
		return errPayload(errCodeMissingParam, "parameter \"knowledge_id\" is required", mc)
	}
	knowledgeID = strings.TrimSpace(knowledgeID)
	if err := validateKnowledgeID(knowledgeID); err != nil {
		return errPayload(errCodeInvalidParamValue, err.Error(), mc)
	}
	q := url.Values{}
	page, pageSize := 1, 10
	if v, present := args["page"]; present && v != nil {
		f, ok := v.(float64)
		if !ok || f < 1 {
			return errPayload(errCodeInvalidParamValue, "parameter \"page\" must be a positive number", mc)
		}
		page = int(f)
	}
	if v, present := args["page_size"]; present && v != nil {
		f, ok := v.(float64)
		if !ok || f < 1 || f > 100 {
			return errPayload(errCodeInvalidParamValue, "parameter \"page_size\" must be a number between 1 and 100", mc)
		}
		pageSize = int(f)
	}
	q.Set("page", strconv.Itoa(page))
	q.Set("page_size", strconv.Itoa(pageSize))
	types := []string{"text"}
	if v, present := args["chunk_type"]; present && v != nil {
		list, ok := v.([]any)
		if !ok {
			return errPayload(errCodeInvalidParamType, "parameter \"chunk_type\" must be an array of strings", mc)
		}
		var ts []string
		for _, item := range list {
			if s, _ := item.(string); strings.TrimSpace(s) != "" {
				ts = append(ts, strings.TrimSpace(s))
			}
		}
		if len(ts) > 0 {
			types = ts
		}
	}
	for _, ct := range types {
		q.Add("chunk_type", ct)
	}
	chunks, total, err := env.WeKnora.ReadChunks(ctx, m, knowledgeID, q)
	if err != nil {
		return mapWeKnoraError(err, mc)
	}
	return successPayload(map[string]any{
		"chunks": chunks, "total": total, "page": page, "page_size": pageSize,
	}, mc)
}

// wikiOutLinkCap bounds the out-link summary fan-out of wiki_read_page
// (1 page + up to 10 follow-up reads, mirroring the official tool's
// inlining behaviour with a hard ceiling).
const wikiOutLinkCap = 10

func (env *ServerEnv) handleWikiSearch(ctx context.Context, hdr http.Header, args map[string]any) []byte {
	mc := MulticaContextFromHeaders(hdr)
	m, fail := env.resolveMapping(mc)
	if fail != nil {
		return fail
	}
	query, _ := args["query"].(string)
	if strings.TrimSpace(query) == "" {
		return errPayload(errCodeMissingParam, "parameter \"query\" is required", mc)
	}
	limit := 10
	if v, present := args["limit"]; present && v != nil {
		f, ok := v.(float64)
		if !ok || f < 1 || f > 50 {
			return errPayload(errCodeInvalidParamValue, "parameter \"limit\" must be a number between 1 and 50", mc)
		}
		limit = int(f)
	}
	if v, present := args["regex"]; present && v != nil {
		b, ok := v.(bool)
		if !ok {
			return errPayload(errCodeInvalidParamType, "parameter \"regex\" must be a boolean", mc)
		}
		if !b {
			query = regexp.QuoteMeta(query)
		}
	} else if _, err := regexp.Compile("(?i)" + query); err != nil {
		// Official auto-detect semantics (WeKnora wikiSearchPattern):
		// omitted regex with a query that does not compile → literal search.
		query = regexp.QuoteMeta(query)
	}
	pages, err := env.WeKnora.WikiSearch(ctx, m, query, limit)
	if err != nil {
		return mapWeKnoraError(err, mc)
	}
	return successPayload(map[string]any{"pages": pages}, mc)
}

func (env *ServerEnv) handleWikiReadPage(ctx context.Context, hdr http.Header, args map[string]any) []byte {
	mc := MulticaContextFromHeaders(hdr)
	m, fail := env.resolveMapping(mc)
	if fail != nil {
		return fail
	}
	slug, _ := args["slug"].(string)
	if strings.TrimSpace(slug) == "" {
		return errPayload(errCodeMissingParam, "parameter \"slug\" is required", mc)
	}
	slug = strings.TrimSpace(slug)
	if err := validateWikiSlug(slug); err != nil {
		return errPayload(errCodeInvalidParamValue, err.Error(), mc)
	}
	page, err := env.WeKnora.WikiReadPage(ctx, m, slug)
	if err != nil {
		return mapWeKnoraError(err, mc)
	}
	// Out-link summary inlining (official MCP behaviour parity): fetch
	// each out-linked page's summary so the agent sees context without
	// a second round trip. Cap at wikiOutLinkCap; the rest are listed
	// as slug-only.
	outLinks := stringSlice(page["out_links"])
	linked := make([]map[string]any, 0, len(outLinks))
	var omitted []string
	for i, link := range outLinks {
		if i >= wikiOutLinkCap {
			omitted = append(omitted, outLinks[i:]...)
			break
		}
		linkedPage, err := env.WeKnora.WikiReadPage(ctx, m, link)
		if err != nil {
			// a dead/dangling link is page data, not a tool failure
			linked = append(linked, map[string]any{"slug": link, "summary": "", "missing": true})
			continue
		}
		linked = append(linked, map[string]any{
			"slug":    link,
			"title":   linkedPage["title"],
			"summary": linkedPage["summary"],
		})
	}
	page["out_link_details"] = linked
	if len(omitted) > 0 {
		page["out_links_omitted"] = omitted
	}
	return successPayload(page, mc)
}

func (env *ServerEnv) handleWikiIndex(ctx context.Context, hdr http.Header, args map[string]any) []byte {
	mc := MulticaContextFromHeaders(hdr)
	m, fail := env.resolveMapping(mc)
	if fail != nil {
		return fail
	}
	limit := 50
	if v, present := args["limit"]; present && v != nil {
		f, ok := v.(float64)
		if !ok || f < 1 || f > 200 {
			return errPayload(errCodeInvalidParamValue, "parameter \"limit\" must be a number between 1 and 200", mc)
		}
		limit = int(f)
	}
	idx, err := env.WeKnora.WikiIndex(ctx, m, limit)
	if err != nil {
		return mapWeKnoraError(err, mc)
	}
	return successPayload(idx, mc)
}

// wikiPageTypes / wikiPageStatuses mirror WeKnora's IsValidWikiPageType /
// IsValidWikiPageStatus whitelists (types/wiki_page.go) — refusing invalid
// values here keeps the server's 400 shape out of the tool's way.
var wikiPageTypes = map[string]bool{
	"summary": true, "entity": true, "concept": true, "index": true,
	"synthesis": true, "comparison": true,
}

var wikiPageStatuses = map[string]bool{
	"draft": true, "published": true, "archived": true,
}

// validateWikiSlugParam runs the shared slug guards: required, non-blank,
// traversal-free.
func validateWikiSlugParam(mc map[string]string, args map[string]any) (string, []byte) {
	slug, _ := args["slug"].(string)
	if strings.TrimSpace(slug) == "" {
		return "", errPayload(errCodeMissingParam, "parameter \"slug\" is required", mc)
	}
	slug = strings.TrimSpace(slug)
	if err := validateWikiSlug(slug); err != nil {
		return "", errPayload(errCodeInvalidParamValue, err.Error(), mc)
	}
	return slug, nil
}

// optionalStringArg pulls an optional string arg: absent, null and blank all
// count as absent; a non-string value is an INVALID_PARAM_TYPE error.
func optionalStringArg(mc map[string]string, args map[string]any, name string) (string, bool, []byte) {
	v, present := args[name]
	if !present || v == nil {
		return "", false, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", false, errPayload(errCodeInvalidParamType, fmt.Sprintf("parameter %q must be a string", name), mc)
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false, nil
	}
	return s, true, nil
}

func (env *ServerEnv) handleWikiCreatePage(ctx context.Context, hdr http.Header, args map[string]any) []byte {
	mc := MulticaContextFromHeaders(hdr)
	m, fail := env.resolveMapping(mc)
	if fail != nil {
		return fail
	}
	slug, fail := validateWikiSlugParam(mc, args)
	if fail != nil {
		return fail
	}
	page := map[string]any{"slug": slug}

	for _, name := range []string{"title", "content", "summary", "folder_id"} {
		s, present, fail := optionalStringArg(mc, args, name)
		if fail != nil {
			return fail
		}
		if present {
			page[name] = s
		}
	}

	// page_type: whitelist; absent or blank = let the server default
	if v, present := args["page_type"]; present && v != nil {
		pt, ok := v.(string)
		if !ok {
			return errPayload(errCodeInvalidParamType, "parameter \"page_type\" must be a string", mc)
		}
		if pt = strings.TrimSpace(pt); pt != "" && !wikiPageTypes[pt] {
			return errPayload(errCodeInvalidParamValue, "parameter \"page_type\" must be one of summary, entity, concept, index, synthesis, comparison", mc)
		}
		if pt != "" {
			page["page_type"] = pt
		}
	}

	// status: whitelist, absent = published (WeKnora's default)
	status := "published"
	if v, present := args["status"]; present && v != nil {
		st, ok := v.(string)
		if !ok {
			return errPayload(errCodeInvalidParamType, "parameter \"status\" must be a string", mc)
		}
		if !wikiPageStatuses[strings.TrimSpace(st)] {
			return errPayload(errCodeInvalidParamValue, "parameter \"status\" must be one of draft, published, archived", mc)
		}
		status = strings.TrimSpace(st)
	}
	page["status"] = status

	if v, present := args["aliases"]; present && v != nil {
		if _, ok := v.([]any); !ok {
			return errPayload(errCodeInvalidParamType, "parameter \"aliases\" must be an array of strings", mc)
		}
		page["aliases"] = stringSlice(v)
	}

	if v, present := args["source_refs"]; present && v != nil {
		list, ok := v.([]any)
		if !ok {
			return errPayload(errCodeInvalidParamType, "parameter \"source_refs\" must be an array of strings", mc)
		}
		refs := make([]string, 0, len(list))
		for _, item := range list {
			ref, ok := item.(string)
			if !ok {
				return errPayload(errCodeInvalidParamValue,
					fmt.Sprintf("parameter \"source_refs\" entries must be \"<knowledge_id>\" or \"<knowledge_id>|<title>\", got %v", item), mc)
			}
			refs = append(refs, strings.TrimSpace(ref))
		}
		for _, ref := range refs {
			id, _ := splitWikiSourceRef(ref)
			if id == "" {
				return errPayload(errCodeInvalidParamValue,
					fmt.Sprintf("parameter \"source_refs\" entries must be \"<knowledge_id>\" or \"<knowledge_id>|<title>\", got %q", ref), mc)
			}
		}
		page["source_refs"] = refs
	}

	created, err := env.WeKnora.WikiCreatePage(ctx, m, page)
	if err != nil {
		return mapWeKnoraError(err, mc)
	}
	return successPayload(created, mc)
}

// splitWikiSourceRef mirrors WeKnora's ParseWikiSourceRef: the first "|"
// splits the knowledge id from the optional display title.
func splitWikiSourceRef(ref string) (id, title string) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", ""
	}
	i := strings.IndexByte(ref, '|')
	if i < 0 {
		return ref, ""
	}
	return strings.TrimSpace(ref[:i]), strings.TrimSpace(ref[i+1:])
}

func (env *ServerEnv) handleWikiUpdatePage(ctx context.Context, hdr http.Header, args map[string]any) []byte {
	mc := MulticaContextFromHeaders(hdr)
	m, fail := env.resolveMapping(mc)
	if fail != nil {
		return fail
	}
	slug, fail := validateWikiSlugParam(mc, args)
	if fail != nil {
		return fail
	}
	// Only present fields go into the request body: the server treats absent
	// fields as "keep the stored value", so a field must never travel as a
	// zero value.
	fields := map[string]any{}

	for _, name := range []string{"title", "content", "summary"} {
		s, present, fail := optionalStringArg(mc, args, name)
		if fail != nil {
			return fail
		}
		if present {
			fields[name] = s
		}
	}

	if v, present := args["page_type"]; present && v != nil {
		pt, ok := v.(string)
		if !ok {
			return errPayload(errCodeInvalidParamType, "parameter \"page_type\" must be a string", mc)
		}
		// blank = absent: the field is not sent and keeps its stored value
		if pt = strings.TrimSpace(pt); pt != "" {
			if !wikiPageTypes[pt] {
				return errPayload(errCodeInvalidParamValue, "parameter \"page_type\" must be one of summary, entity, concept, index, synthesis, comparison", mc)
			}
			fields["page_type"] = pt
		}
	}

	if v, present := args["status"]; present && v != nil {
		st, ok := v.(string)
		if !ok {
			return errPayload(errCodeInvalidParamType, "parameter \"status\" must be a string", mc)
		}
		if !wikiPageStatuses[strings.TrimSpace(st)] {
			return errPayload(errCodeInvalidParamValue, "parameter \"status\" must be one of draft, published, archived", mc)
		}
		fields["status"] = strings.TrimSpace(st)
	}

	if v, present := args["aliases"]; present && v != nil {
		if _, ok := v.([]any); !ok {
			return errPayload(errCodeInvalidParamType, "parameter \"aliases\" must be an array of strings", mc)
		}
		fields["aliases"] = stringSlice(v)
	}

	if v, present := args["version"]; present && v != nil {
		f, ok := v.(float64)
		// kept as float64: json marshals whole numbers without a decimal
		// point, so the wire format matches the server's int field
		if !ok || f < 0 || f != float64(int(f)) {
			return errPayload(errCodeInvalidParamType, "parameter \"version\" must be a non-negative integer", mc)
		}
		fields["version"] = f
	}

	updated, err := env.WeKnora.WikiUpdatePage(ctx, m, slug, fields)
	if err != nil {
		return mapWeKnoraError(err, mc)
	}
	return successPayload(updated, mc)
}

func (env *ServerEnv) handleWikiDeletePage(ctx context.Context, hdr http.Header, args map[string]any) []byte {
	mc := MulticaContextFromHeaders(hdr)
	m, fail := env.resolveMapping(mc)
	if fail != nil {
		return fail
	}
	slug, fail := validateWikiSlugParam(mc, args)
	if fail != nil {
		return fail
	}
	if err := env.WeKnora.WikiDeletePage(ctx, m, slug); err != nil {
		return mapWeKnoraError(err, mc)
	}
	return successPayload(map[string]any{"deleted": true, "slug": slug}, mc)
}

func (env *ServerEnv) handleWikiCreateFolder(ctx context.Context, hdr http.Header, args map[string]any) []byte {
	mc := MulticaContextFromHeaders(hdr)
	m, fail := env.resolveMapping(mc)
	if fail != nil {
		return fail
	}
	name, _ := args["name"].(string)
	if strings.TrimSpace(name) == "" {
		return errPayload(errCodeMissingParam, "parameter \"name\" is required", mc)
	}
	parentID, present, fail := optionalStringArg(mc, args, "parent_id")
	if fail != nil {
		return fail
	}
	if !present {
		parentID = ""
	}
	folder, err := env.WeKnora.WikiCreateFolder(ctx, m, strings.TrimSpace(name), parentID)
	if err != nil {
		return mapWeKnoraError(err, mc)
	}
	return successPayload(folder, mc)
}

func (env *ServerEnv) handleWikiMovePage(ctx context.Context, hdr http.Header, args map[string]any) []byte {
	mc := MulticaContextFromHeaders(hdr)
	m, fail := env.resolveMapping(mc)
	if fail != nil {
		return fail
	}
	slug, fail := validateWikiSlugParam(mc, args)
	if fail != nil {
		return fail
	}
	// empty folder_id is meaningful: it moves the page to the wiki root
	folderID, _ := args["folder_id"].(string)
	if v, present := args["folder_id"]; present && v != nil {
		s, ok := v.(string)
		if !ok {
			return errPayload(errCodeInvalidParamType, "parameter \"folder_id\" must be a string", mc)
		}
		folderID = s
	}
	page, err := env.WeKnora.WikiMovePage(ctx, m, slug, folderID)
	if err != nil {
		return mapWeKnoraError(err, mc)
	}
	return successPayload(page, mc)
}

func (env *ServerEnv) handleAddDocument(ctx context.Context, hdr http.Header, args map[string]any) []byte {
	mc := MulticaContextFromHeaders(hdr)
	m, fail := env.resolveMapping(mc)
	if fail != nil {
		return fail
	}
	title, _ := args["title"].(string)
	if strings.TrimSpace(title) == "" {
		return errPayload(errCodeMissingParam, "parameter \"title\" is required", mc)
	}
	publish := true
	if v, present := args["publish"]; present && v != nil {
		b, ok := v.(bool)
		if !ok {
			return errPayload(errCodeInvalidParamType, "parameter \"publish\" must be a boolean", mc)
		}
		publish = b
	}
	content, hasContent := args["content"].(string)
	if v, present := args["content"]; present && v != nil && !hasContent {
		return errPayload(errCodeInvalidParamType, "parameter \"content\" must be a string", mc)
	}
	docURL, hasURL := args["url"].(string)
	if v, present := args["url"]; present && v != nil && !hasURL {
		return errPayload(errCodeInvalidParamType, "parameter \"url\" must be a string", mc)
	}
	s3key, hasS3 := args["s3_key"].(string)
	if v, present := args["s3_key"]; present && v != nil && !hasS3 {
		return errPayload(errCodeInvalidParamType, "parameter \"s3_key\" must be a string", mc)
	}
	given := 0
	for _, b := range []bool{strings.TrimSpace(content) != "", strings.TrimSpace(docURL) != "", strings.TrimSpace(s3key) != ""} {
		if b {
			given++
		}
	}
	if given != 1 {
		return errPayload(errCodeInvalidParamValue, "give exactly one of content, url or s3_key", mc)
	}

	var (
		doc json.RawMessage
		err error
	)
	switch {
	case strings.TrimSpace(content) != "":
		doc, err = env.WeKnora.AddManualDocument(ctx, m, title, content, publish)
	case strings.TrimSpace(docURL) != "":
		doc, err = env.WeKnora.AddURLDocument(ctx, m, strings.TrimSpace(docURL), title)
	default:
		doc, err = env.addS3Document(ctx, m, title, strings.TrimSpace(s3key), publish)
		if err != nil {
			if ste, ok := err.(*s3ToolError); ok {
				return errPayload(ste.code, ste.msg, mc)
			}
			// the s3 phases succeeded; this failure is on the
			// WeKnora upload side — use the WeKnora vocabulary.
			return mapWeKnoraError(err, mc)
		}
	}
	if err != nil {
		return mapWeKnoraError(err, mc)
	}
	return successPayload(map[string]any{
		"document":   doc,
		"async_note": "parsing and indexing continue asynchronously; poll list_documents with parse_status to observe progress",
	}, mc)
}

func (env *ServerEnv) handleUpdateDocument(ctx context.Context, hdr http.Header, args map[string]any) []byte {
	mc := MulticaContextFromHeaders(hdr)
	m, fail := env.resolveMapping(mc)
	if fail != nil {
		return fail
	}
	knowledgeID, _ := args["knowledge_id"].(string)
	if strings.TrimSpace(knowledgeID) == "" {
		return errPayload(errCodeMissingParam, "parameter \"knowledge_id\" is required", mc)
	}
	knowledgeID = strings.TrimSpace(knowledgeID)
	if err := validateKnowledgeID(knowledgeID); err != nil {
		return errPayload(errCodeInvalidParamValue, err.Error(), mc)
	}
	content, _ := args["content"].(string)
	if strings.TrimSpace(content) == "" {
		return errPayload(errCodeMissingParam, "parameter \"content\" is required", mc)
	}
	title, _ := args["title"].(string)
	publish := true
	if v, present := args["publish"]; present && v != nil {
		b, ok := v.(bool)
		if !ok {
			return errPayload(errCodeInvalidParamType, "parameter \"publish\" must be a boolean", mc)
		}
		publish = b
	}
	doc, err := env.WeKnora.UpdateDocument(ctx, m, knowledgeID, content, title, publish)
	if err != nil {
		return mapWeKnoraError(err, mc)
	}
	return successPayload(map[string]any{"document": doc}, mc)
}

func (env *ServerEnv) handleDeleteDocument(ctx context.Context, hdr http.Header, args map[string]any) []byte {
	mc := MulticaContextFromHeaders(hdr)
	m, fail := env.resolveMapping(mc)
	if fail != nil {
		return fail
	}
	knowledgeID, _ := args["knowledge_id"].(string)
	if strings.TrimSpace(knowledgeID) == "" {
		return errPayload(errCodeMissingParam, "parameter \"knowledge_id\" is required", mc)
	}
	knowledgeID = strings.TrimSpace(knowledgeID)
	if err := validateKnowledgeID(knowledgeID); err != nil {
		return errPayload(errCodeInvalidParamValue, err.Error(), mc)
	}
	resp, err := env.WeKnora.DeleteDocument(ctx, m, knowledgeID)
	if err != nil {
		return mapWeKnoraError(err, mc)
	}
	return successPayload(resp, mc)
}

// weknoraMaxFileBytes is WeKnora's upload cap (utils.GetMaxFileSizeMB
// default 50). S3 objects larger than this are rejected before upload.
const weknoraMaxFileBytes = 50 * 1024 * 1024

// addS3Document implements the s3_key source of add_document: fetch the
// object, then route by extension — .md/.markdown to the manual (text)
// endpoint, everything else to multipart file upload. Only the s3-side
// phases (store nil / key resolve / Get) come back wrapped in
// *s3ToolError; the WeKnora upload errors are returned as-is so
// handleAddDocument can map them with the WeKnora vocabulary.
func (env *ServerEnv) addS3Document(ctx context.Context, m *WorkspaceMapping, title, s3key string, publish bool) (json.RawMessage, error) {
	if env.Store == nil {
		return nil, &s3ToolError{code: errCodeS3Unavailable, msg: "s3 attachments are not configured (set S3_BUCKET / AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY)"}
	}
	s3cfg := env.Config.S3
	if s3cfg == nil {
		s3cfg = &S3Config{}
	}
	key, err := resolveInputKey(s3key, s3cfg)
	if err != nil {
		return nil, &s3ToolError{code: errCodeS3InvalidKey, msg: fmt.Sprintf("s3_key: %v", err)}
	}
	// resolveInputKey already validated traversal, dangerous extension
	// and prefix allowlist before the fetch — no re-check needed here.
	content, err := env.Store.Get(key, weknoraMaxFileBytes)
	if err != nil {
		return nil, mapS3GetError(err, key, s3cfg.Bucket)
	}
	lower := strings.ToLower(path.Base(key))
	switch {
	case strings.HasSuffix(lower, ".md"), strings.HasSuffix(lower, ".markdown"):
		// title is required at the tool layer (handleAddDocument rejects
		// empty), so no fallback to the object name is needed.
		return env.WeKnora.AddManualDocument(ctx, m, title, string(content), publish)
	default:
		return env.WeKnora.AddFileDocument(ctx, m, path.Base(key), content, mime.TypeByExtension(path.Ext(lower)))
	}
}

// s3ToolError carries a tool error code through the s3 path.
type s3ToolError struct {
	code string
	msg  string
}

func (e *s3ToolError) Error() string { return e.msg }

// mapS3GetError folds the object-store sentinel errors onto tool codes,
// naming the bucket for not-found (bucket-attribution triage, email-mcp
// behaviour).
func mapS3GetError(err error, key, bucket string) error {
	switch {
	case errors.Is(err, ErrObjectNotFound):
		if bucket != "" {
			return &s3ToolError{code: errCodeS3ObjectNotFound, msg: fmt.Sprintf("s3_key: object not found in bucket %q", bucket)}
		}
		return &s3ToolError{code: errCodeS3ObjectNotFound, msg: "s3_key: object not found"}
	case errors.Is(err, ErrObjectTooLarge):
		// exceeded the configured S3_MAX_FILE_BYTES (server-level cap)
		return &s3ToolError{code: errCodeS3ObjectTooLarge, msg: "s3 object exceeds S3_MAX_FILE_BYTES"}
	case errors.Is(err, ErrAttachmentTooLarge):
		// exceeded the caller limit = the WeKnora 50 MiB upload ceiling;
		// catch it before a wasted multipart upload
		return &s3ToolError{code: errCodeS3FileTooLargeForWK,
			msg: fmt.Sprintf("s3 object exceeds the weknora upload limit (%d MiB)", weknoraMaxFileBytes>>20)}
	}
	return &s3ToolError{code: errCodeS3FetchFailed, msg: fmt.Sprintf("s3 fetch failed: %v", err)}
}

// stringSlice coerces a JSON-decoded any into []string (absent, non-array
// or non-string items yield nil).
func stringSlice(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// successPayload marshals a successful tool result.
func successPayload(data map[string]any, mc map[string]string) []byte {
	if mc == nil {
		mc = map[string]string{}
	}
	raw, err := json.Marshal(toolResult{
		Success:        true,
		MulticaContext: mc,
		Data:           mustJSON(data),
	})
	if err != nil {
		return errPayload(errCodeInternal, "marshal result: "+err.Error(), mc)
	}
	return raw
}

func mustJSON(v any) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}
