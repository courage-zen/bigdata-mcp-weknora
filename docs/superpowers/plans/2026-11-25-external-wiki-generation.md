# WeKnora 外部 Wiki 生成（MCP v0.3.0 + skill + runbook）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 给 weknora-mcp 加 5 个 wiki 写 REST 工具（v0.3.0），并交付等价复刻 WeKnora 内部 wiki 管线的 skill 与 82 runbook。

**Architecture:** 薄 MCP + 厚 skill（spec 方案 A）。MCP 只做"写字典"级 REST 映射——新 client 方法 + 新 handler，全部走既有 `callTool`/`resolveMapping`/`mapWeKnoraError` 模式，零业务逻辑；管线逻辑（抽取、重名合并、summary reconcile、index intro 更新）全部在本仓库 `skills/weknora-wiki-ingest/SKILL.md`（随 v0.3.0 版本化发布），由 agent 执行。前置：82 目标 KB 已清空内部 synthesis model（runbook Task 5）。

**Tech Stack:** Go（modelcontextprotocol/go-sdk）、YAML 配置、Markdown skill。

**Spec:** `docs/superpowers/specs/2026-11-25-external-wiki-generation-design.md`（含 C1-C11 REST 事实表、Part 1 工具清单、Part 2 skill Steps 0-5、计划期勘误）。**执行者必须先读 spec。**

## Global Constraints

- wiki REST 路径前缀是单数 `/api/v1/knowledgebase/{kb}/wiki/...`（无连字符——文档写入路径 `/api/v1/knowledge-bases/{id}/knowledge/...` 才有；这个不对称正是 v0.2.1 教训，新 path-lock 测试必须 pin 它）
- `wiki_move_page` 走 **PUT** `/wiki/move-page`（不是 POST——spec 计划期勘误第 1 条）
- `wiki_create_page` 撞 slug → 服务端 500（uniqueIndex `idx_kb_slug` 冲突，handler 统一 500）；409 只在 `wiki_update_page` 乐观锁路径。原样透传，不做"先查再建"
- `wiki_update_page` 只传"出现的参数"——服务端语义是缺席字段保持原值（`WikiPageUpdateRequest` 全是 `*` 指针 + omitempty）
- `page_type` 合法值：summary/entity/concept/index/synthesis/comparison；`status` 合法值：draft/published/archived（白名单校验，与 `types.IsValidWikiPageType/Status` 对齐）
- `source_refs` 条目格式 `"<knowledge_id>"` 或 `"<knowledge_id>|<title>"`（第一个 `|` 为分隔符，title 可含 `|`）；handler 只做形状校验（非空、ID 段非空）
- slug 校验复用 `validateWikiSlug`（拒 `.`/`..` 段，允许 `/` 结构段）；slug、folder_id 经 `pathSegments` 或路径参数透传
- MCP 定位：REST-only，新工具零业务逻辑、零 LLM 调用（spec 非目标）
- handler 错误码语义沿用现有：缺必填 → `errCodeMissingParam`；类型错 → `errCodeInvalidParamType`；取值非法 → `errCodeInvalidParamValue`；WeKnora 错误一律 `mapWeKnoraError`
- 测试/文档用中性示例 UUID，不出现生产 UUID（实例信息；spec 前置约束）
- git committer 保持 GitHub noreply（repo 已配好，勿动）；commit 末尾 `Co-Authored-By: Claude Code <noreply@anthropic.com>`
- WeKnora 0.9.0 源码在 `~/code/WeKnora-0.9.0`，是 REST 面的事实来源（读路径勿臆断）

## Review Focus

spec 没逐条测、但最可能咬到真实用户的五类输入/失败模式（每条已把测试 pin 到所属 task）：

1. **多段 slug 的路径安全**——`entity/../x` 这类以 `/` 为结构性前缀的越权 slug 必须在 handler 层被拒、绝不触达 client（Task 2 `TestWikiWriteSlugTraversalRejected`）
2. **update 缺席字段误清空**——`wiki_update_page` 只传 `content` 时 title/aliases 必须原样保留（服务端指针合并语义；client 侧必须只序列化出现的参数，Task 1 `TestWeKnoraClient_WikiUpdateOmitsAbsentFields`）
3. **page_type/status 非法值**——`bogus`、空串、大小写混写（`Summary`）必须在 MCP 层 `INVALID_PARAM_VALUE` 拒绝，不透传给服务端白名单（Task 2 `TestWikiCreatePage_Validation`）
4. **WeKnora 4xx/500 错误透传**——404（页不存在）、乐观锁 409、唯一索引 500 必须以 `WEKNORA_API_FAILED` + "HTTP <status>: <message>" 原样浮出（Task 2 `TestWikiWrite_WeKnoraErrorPassthrough`）
5. **skill 被错误调用于未清模型的 KB**——内部管线正在生成时跑外部 skill 会双写打架；skill Step 0 必须可判定地阻断（Task 4 skill 内容自查步骤 + Task 6 runbook 验证步骤）

---

### Task 1: WeKnoraCaller 5 个新 client 方法 + path-lock 测试

**Files:**
- Modify: `weknora.go`（`WeKnoraCaller` 接口 `weknora.go:36-49` 加 5 个方法；实现加在 WikiIndex 之后）
- Test: `weknora_test.go`（加 `TestWeKnoraClient_WikiWritePaths`）

**Interfaces:**
- Consumes: `pathSegments(s string) string`、`doJSON(ctx, m, method, path, payload)`、`do(ctx, m, method, path, contentType, body)`（均已在 weknora.go）
- Produces（后续 task 的 handler 与 fake 全靠这些签名；Task 2 的 `fakeWeKnoraCaller` 逐个实现同名方法）:

```go
WikiCreatePage(ctx context.Context, m *WorkspaceMapping, page map[string]any) (map[string]any, error)
// POST /api/v1/knowledgebase/{kb}/wiki/pages — body = page（slug/title/content/summary/
// page_type/status/aliases/source_refs/folder_id），响应 201 的整页 JSON

WikiUpdatePage(ctx context.Context, m *WorkspaceMapping, slug string, fields map[string]any) (map[string]any, error)
// PUT /api/v1/knowledgebase/{kb}/wiki/pages/{slug} — body = fields（只含要改的字段：
// title/content/summary/page_type/status/aliases/version），响应整页 JSON

WikiDeletePage(ctx context.Context, m *WorkspaceMapping, slug string) error
// DELETE /api/v1/knowledgebase/{kb}/wiki/pages/{slug} — 204 无 body；404 → *weknoraError{404}

WikiCreateFolder(ctx context.Context, m *WorkspaceMapping, name, parentID string) (map[string]any, error)
// POST /api/v1/knowledgebase/{kb}/wiki/folders — body {name, parent_id}，响应 folder JSON

WikiMovePage(ctx context.Context, m *WorkspaceMapping, slug, folderID string) (map[string]any, error)
// PUT /api/v1/knowledgebase/{kb}/wiki/move-page — body {slug, folder_id}，响应整页 JSON
```

- [ ] **Step 1: 写失败测试 `TestWeKnoraClient_WikiWritePaths`**（weknora_test.go，紧挨 `TestWeKnoraClient_WikiPaths`）

复刻 `TestWeKnoraClient_IngestPaths` 的"记录 path+method"模式。一次 fakeWeKnora 挂五个调用、断言各自 path/method：

```go
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
```

同测试文件再加两个针对性断言测试：

```go
// 断言 update 只序列化出现的字段（缺席字段必须不出现在 body——服务端指针合并语义）
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

// 断言 create body 携带全部出现参数（含 source_refs/aliases/folder_id）
func TestWeKnoraClient_WikiCreatePageSendsAllParams(t *testing.T) {
	var gotBody map[string]any
	c, m := bodyCapture(t, &gotBody)
	if _, err := c.WikiCreatePage(context.Background(), m, map[string]any{
		"slug": "entity/rag", "title": "RAG", "content": "c", "summary": "s",
		"page_type": "entity", "status": "published",
		"aliases": []any{"Retrieval Augmented Generation"},
		"source_refs": []any{"k-1|RAG 论文"},
		"folder_id": "f-1",
	}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"slug", "title", "content", "summary", "page_type", "status", "aliases", "source_refs", "folder_id"} {
		if _, ok := gotBody[k]; !ok {
			t.Errorf("body missing %q: %v", k, gotBody)
		}
	}
}
```

`bodyCapture` 小 helper（同文件顶部，`fakeWeKnora` 旁）：httptest server + decoder 写入 `*map[string]any`，返回 client+mapping。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test -run 'TestWeKnoraClient_WikiWritePaths|TestWeKnoraClient_WikiUpdateOmitsAbsentFields|TestWeKnoraClient_WikiCreatePageSendsAllParams' ./... -v 2>&1 | head -30`
Expected: FAIL，编译错误 `c.WikiCreatePage undefined`（接口还没加方法）

- [ ] **Step 3: 接口 + 实现 + fake stubs（weknora.go、tools_test.go）**

`WeKnoraCaller` 接口加 5 个方法（签名见 Interfaces）。实现要点：
- 全部用现有 helpers：路径拼接 `"/api/v1/knowledgebase/"+pathSegments(m.KBID)+"/wiki/..."`，slug 走 `pathSegments(slug)`（多段结构保留）；body 用 `doJSON`；DELETE 用 `do`（204 无 body，读 err 即可）；响应 `map[string]any` 用 `json.Unmarshal`（对齐 `WikiReadPage`）
- `WikiCreatePage` 的 body 直接 marshal 传入的 map（不复制/过滤——形状由 handler task 保证）
- `WikiUpdatePage` 直接 marshal `fields` map
- `WikiDeletePage` 成功返回 nil；`do` 已把 ≥400 变 `*weknoraError`
- 无需新 helper、无需改 `do`/`doJSON`

**同步骤加 fake stubs**（tools_test.go——接口加了方法后 `fakeWeKnoraCaller` 必须同步实现，否则包编译失败）：结构体加字段 `wikiCreatePageCalls []map[string]any`、`wikiUpdateSlugs []string`、`wikiUpdateFields []map[string]any`、`wikiDeleteSlugs []string`、`wikiFolderName string`、`wikiFolderParent string`、`wikiMoveSlug string`、`wikiMoveFolder string`，加 5 个方法（记录调用、返回罐头值：create/update/move 返回 `map[string]any{"slug": …, "version": 2}`，folder 返回 `{"id":"f-1","name":…}`，delete 返回 nil）。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test -run 'TestWeKnoraClient_WikiWritePaths|TestWeKnoraClient_WikiUpdateOmitsAbsentFields|TestWeKnoraClient_WikiCreatePageSendsAllParams' ./... -v`
Expected: PASS

- [ ] **Step 5: 全量回归 + 提交**

Run: `go vet ./... && go test ./...`
Expected: 全绿

```bash
git add weknora.go weknora_test.go tools_test.go
git commit -m "feat: weknora client wiki write methods + path-lock tests

POST/PUT/DELETE wiki pages, POST folders, PUT move-page — the five
REST writes the external wiki generation pipeline (v0.3.0) needs.
Path asymmetry vs document routes pinned (knowledgebase vs
knowledge-bases/knowledge).

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

### Task 2: 5 个 MCP 工具 handler + 校验 + fake 测试

**Files:**
- Modify: `tools.go`（RegisterTools `tools.go:135-141` 加 `addWikiWriteTools` 门控调用；handler 加在 handleWikiIndex 之后）
- Test: `tools_test.go`（fake 已在 Task 1 扩展；本 task 加 handler 测试 + `envWikiX` helpers）

**Interfaces:**
- Consumes: Task 1 的 5 个 client 方法签名；`callTool`、`resolveMapping`、`mapWeKnoraError`、`errPayload`、`successPayload`、`validateWikiSlug`、`stringSlice`（全部已在 tools.go）
- Produces: `addWikiWriteTools(server *mcp.Server, env *ServerEnv)`（RegisterTools 内 `EnableIngest` 门控）+ 5 个 handler：
  - `handleWikiCreatePage(ctx, hdr, args) []byte` — args: slug(必) title content summary page_type status aliases[] source_refs[] folder_id
  - `handleWikiUpdatePage(ctx, hdr, args) []byte` — args: slug(必) title content summary page_type status aliases[] version(可选)
  - `handleWikiDeletePage(ctx, hdr, args) []byte` — args: slug(必)
  - `handleWikiCreateFolder(ctx, hdr, args) []byte` — args: name(必) parent_id
  - `handleWikiMovePage(ctx, hdr, args) []byte` — args: slug(必) folder_id
- Produces（工具 JSON schema，对齐现有工具描述风格——描述里写清 REST 语义）:
  - `wiki_create_page`：params `slug`(必) `title` `content` `summary` `page_type` `status` `aliases[]` `source_refs[]` `folder_id`；默认 status=published
  - `wiki_update_page`：params `slug`(必) `title` `content` `summary` `page_type` `status` `aliases[]` `version`；缺席字段保持原值
  - `wiki_delete_page`：params `slug`(必)
  - `wiki_create_folder`：params `name`(必) `parent_id`
  - `wiki_move_page`：params `slug`(必) `folder_id`

- [ ] **Step 1: 写失败的 handler 测试（tools_test.go）**

`envWikiCreatePage`/`envWikiUpdatePage`/`envWikiDeletePage`/`envWikiCreateFolder`/`envWikiMovePage` helper（复刻 `envWikiReadPage` 模式）+ 测试：

```go
func TestWikiWriteTools_HappyPaths(t *testing.T) {
	fake := &fakeWeKnoraCaller{}
	env := testEnv(fake)

	// create：全部参数透传，status 缺省 published
	payload := envWikiCreatePage(env, wsHeaders(), map[string]any{
		"slug": "entity/rag", "title": "RAG", "content": "# RAG",
		"summary": "RAG 检索增强生成概览页",
		"page_type": "entity", "aliases": []any{"Retrieval Augmented Generation"},
		"source_refs": []any{"11111111-2222-3333-4444-555555555555|RAG 论文"},
		"folder_id": "f-1",
	})
	if r := decodePayload(t, payload); !r.Success {
		t.Fatalf("payload: %s", payload)
	}
	last := fake.wikiCreatePageCalls[len(fake.wikiCreatePageCalls)-1]
	for _, k := range []string{"slug", "title", "content", "summary", "page_type", "status", "aliases", "source_refs", "folder_id"} {
		if _, ok := last[k]; !ok {
			t.Errorf("create body missing %q: %v", k, last)
		}
	}
	if last["status"] != "published" {
		t.Errorf("status default = %v, want published", last["status"])
	}

	// update：只传出现的字段
	payload = envWikiUpdatePage(env, wsHeaders(), map[string]any{
		"slug": "entity/rag", "content": "# new", "version": 3.0,
	})
	if r := decodePayload(t, payload); !r.Success {
		t.Fatalf("payload: %s", payload)
	}
	if len(fake.wikiUpdateFields[0]) != 2 || fake.wikiUpdateFields[0]["version"] != 3.0 {
		t.Errorf("update fields: %v", fake.wikiUpdateFields[0])
	}

	// delete / folder / move：参数记录 + 成功
	payload = envWikiDeletePage(env, wsHeaders(), map[string]any{"slug": "entity/rag"})
	if r := decodePayload(t, payload); !r.Success || fake.wikiDeleteSlugs[0] != "entity/rag" {
		t.Fatalf("delete: %s", payload)
	}
	payload = envWikiCreateFolder(env, wsHeaders(), map[string]any{"name": "AI", "parent_id": "f-0"})
	if r := decodePayload(t, payload); !r.Success || fake.wikiFolderName != "AI" || fake.wikiFolderParent != "f-0" {
		t.Fatalf("folder: %s", payload)
	}
	payload = envWikiMovePage(env, wsHeaders(), map[string]any{"slug": "entity/rag", "folder_id": "f-1"})
	if r := decodePayload(t, payload); !r.Success || fake.wikiMoveSlug != "entity/rag" || fake.wikiMoveFolder != "f-1" {
		t.Fatalf("move: %s", payload)
	}
}

func TestWikiWriteTools_Validation(t *testing.T) {
	env := testEnv(&fakeWeKnoraCaller{})

	// 缺必填 → MISSING_PARAM（create 无 slug、folder 无 name）
	for _, tc := range []struct{ args map[string]any }{
		{map[string]any{"title": "T"}},
		{map[string]any{"slug": ""}},
	} {
		if r := decodePayload(t, envWikiCreatePage(env, wsHeaders(), tc.args)); r.ErrorCode != "MISSING_PARAM" {
			t.Errorf("create %v: code = %q, want MISSING_PARAM", tc.args, r.ErrorCode)
		}
	}
	if r := decodePayload(t, envWikiCreateFolder(env, wsHeaders(), map[string]any{"parent_id": "f-0"})); r.ErrorCode != "MISSING_PARAM" {
		t.Errorf("folder no name: %q", r.ErrorCode)
	}

	// page_type / status 白名单 → INVALID_PARAM_VALUE（create 与 update 都测）；
	// 空串 = 缺省（不传），必须放行
	for _, bad := range []string{"bogus", "Summary"} {
		payload := envWikiCreatePage(env, wsHeaders(), map[string]any{"slug": "s", "page_type": bad})
		if r := decodePayload(t, payload); r.ErrorCode != "INVALID_PARAM_VALUE" {
			t.Errorf("page_type %q: code = %q", bad, r.ErrorCode)
		}
		payload = envWikiUpdatePage(env, wsHeaders(), map[string]any{"slug": "s", "page_type": bad})
		if r := decodePayload(t, payload); r.ErrorCode != "INVALID_PARAM_VALUE" {
			t.Errorf("update page_type %q: code = %q", bad, r.ErrorCode)
		}
	}
	if r := decodePayload(t, envWikiCreatePage(env, wsHeaders(), map[string]any{"slug": "s", "page_type": ""})); !r.Success {
		t.Errorf("empty page_type (= default) must pass")
	}	for _, bad := range []string{"bogus", "Published"} {
		payload := envWikiCreatePage(env, wsHeaders(), map[string]any{"slug": "s", "status": bad})
		if r := decodePayload(t, payload); r.ErrorCode != "INVALID_PARAM_VALUE" {
			t.Errorf("status %q: code = %q", bad, r.ErrorCode)
		}
	}

	// source_refs 形状：空串条目、"|title"（ID 段空）→ INVALID_PARAM_VALUE
	for _, refs := range []any{
		[]any{""},
		[]any{"|论文标题"},
		[]any{123},
	} {
		payload := envWikiCreatePage(env, wsHeaders(), map[string]any{"slug": "s", "source_refs": refs})
		if r := decodePayload(t, payload); r.ErrorCode != "INVALID_PARAM_VALUE" {
			t.Errorf("source_refs %v: code = %q, want INVALID_PARAM_VALUE", refs, r.ErrorCode)
		}
	}
}

func TestWikiWriteSlugTraversalRejected(t *testing.T) {
	fake := &fakeWeKnoraCaller{}
	env := testEnv(fake)
	for _, slug := range []string{"../x", "a/../b", "entity/.", "./entity/x"} {
		for _, call := range []func(env *ServerEnv, hdr http.Header, args map[string]any) []byte{
			envWikiCreatePage, envWikiUpdatePage, envWikiDeletePage, envWikiMovePage,
		} {
			args := map[string]any{"slug": slug}
			if call == envWikiCreatePage {
				args["title"] = "T"
			}
			if call == envWikiMovePage {
				args["folder_id"] = "f-1"
			}
			if r := decodePayload(t, call(env, wsHeaders(), args)); r.ErrorCode != "INVALID_PARAM_VALUE" {
				t.Fatalf("slug %q: code = %q, want INVALID_PARAM_VALUE", slug, r.ErrorCode)
			}
		}
	}
	if len(fake.wikiCreatePageCalls) != 0 || len(fake.wikiUpdateSlugs) != 0 ||
		len(fake.wikiDeleteSlugs) != 0 || fake.wikiMoveSlug != "" {
		t.Fatal("rejected slugs reached the client")
	}
	// 结构性 / 仍然合法
	payload := envWikiCreatePage(env, wsHeaders(), map[string]any{"slug": "entity/rag", "title": "T"})
	if r := decodePayload(t, payload); !r.Success {
		t.Fatalf("multi-segment slug must pass: %s", payload)
	}
}

func TestWikiWrite_WeKnoraErrorPassthrough(t *testing.T) {
	// fake 注入 *weknoraError → 统一 WEKNORA_API_FAILED + 状态/message 透传
	env := testEnv(&fakeWeKnoraCaller{
		wikiCreatePageErr: &weknoraError{Status: 500, Message: "UNIQUE constraint failed: idx_kb_slug"},
	})
	payload := envWikiCreatePage(env, wsHeaders(), map[string]any{"slug": "dup", "title": "T"})
	r := decodePayload(t, payload)
	if r.ErrorCode != "WEKNORA_API_FAILED" || !strings.Contains(r.Error, "HTTP 500") || !strings.Contains(r.Error, "idx_kb_slug") {
		t.Fatalf("passthrough: %s", payload)
	}
}

// 工具注册门控：EnableIngest=false 时 5 个新工具不出现在 tools/list
func TestServer_WikiWriteToolsGating(t *testing.T) {
	// disabled
	ts := newTestServer(t, "", &fakeWeKnoraCaller{})
	_, body := mcpCall(t, ts.URL, nil, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	for _, name := range []string{"wiki_create_page", "wiki_update_page", "wiki_delete_page", "wiki_create_folder", "wiki_move_page"} {
		if strings.Contains(body, name) {
			t.Fatalf("%s must not be registered when ingest disabled", name)
		}
	}
	// enabled
	env := testEnv(&fakeWeKnoraCaller{})
	env.Config.EnableIngest = true
	ts2 := httptest.NewServer(NewHandler(env, ""))
	t.Cleanup(ts2.Close)
	_, body2 := mcpCall(t, ts2.URL, nil, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	for _, name := range []string{"wiki_create_page", "wiki_update_page", "wiki_delete_page", "wiki_create_folder", "wiki_move_page"} {
		if !strings.Contains(body2, name) {
			t.Fatalf("%s missing from tools/list when enabled", name)
		}
	}
}
```

fake 扩展（Task 1 已建结构；此处补错注入字段）：`wikiCreatePageErr error`、`wikiUpdateErr error`。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test -run 'TestWikiWrite|TestServer_WikiWriteToolsGating' ./... -v 2>&1 | head -30`
Expected: FAIL，编译错误 handler 未定义

- [ ] **Step 3: 实现 handler + 注册（tools.go）**

- `addWikiWriteTools(server, env)`：5 个 `callTool` 注册（名称/描述/schema 见 Interfaces）。描述文案对齐现有风格；`wiki_update_page` 描述里写明"缺席字段保持原值（服务端合并语义）"；`wiki_create_page` 描述里写明 slug 唯一、撞已有 slug 报错、source_refs 格式 `"<knowledge_id>|<title>"`。
- RegisterTools：`if env.Config.EnableIngest { addIngestTools(...); addWikiWriteTools(...) }`——门控与文档工具同门。
- handler 实现要点（对齐 `handleWikiReadPage`/`handleDeleteDocument` 惯用法）：
  - 每个先 `mc := MulticaContextFromHeaders(hdr)` + `resolveMapping` + slug 必填 + `validateWikiSlug`
  - `handleWikiCreatePage`：`page_type` 白名单 `summary|entity|concept|index|synthesis|comparison`（缺省不传，让服务端默认）、`status` 白名单 `draft|published|archived`（缺省 `"published"`）、`aliases`/`source_refs` 用 `stringSlice` 强转（非数组 → nil，出现即非数组时 `INVALID_PARAM_TYPE`；条目做 source_refs 形状校验：TrimSpace 后非空、第一个 `|` 前 ID 段非空）、`folder_id` 非字符串 → INVALID_PARAM_TYPE。构建 `map[string]any` 时**只放出现的参数**；`title`/`content` 按工具 schema 可选（spec Part 1 参数表无必填标记之外的必填），出现的非字符串值 → INVALID_PARAM_TYPE，空白串等同缺席。调用 `WikiCreatePage`，成功 `successPayload(page, mc)`
  - `handleWikiUpdatePage`：构建 fields map **只含出现的参数**（title/content/summary 可传非空白字符串；page_type/status 白名单；aliases stringSlice；version float64→int，非数 → INVALID_PARAM_TYPE）。注意：**不强制任何字段出现**（除 slug）——服务端允许纯 aliases 或纯 version 更新？——version 单独出现无意义但服务端接受；保持宽松，仅校验出现值的合法性
  - `handleWikiDeletePage`：slug 必填 + 校验 → `WikiDeletePage` → `successPayload(map[string]any{"deleted": true, "slug": slug}, mc)`
  - `handleWikiCreateFolder`：name 必填非空白；parent_id 可选字符串
  - `handleWikiMovePage`：slug 必填 + 校验；folder_id 可选（空串=移回根——透传空串）
  - 所有 `env.WeKnora.WikiX(...)` 错误走 `mapWeKnoraError(err, mc)`

- [ ] **Step 4: 跑测试确认通过**

Run: `go test -run 'TestWikiWrite|TestServer_WikiWriteToolsGating' ./... -v`
Expected: PASS

- [ ] **Step 5: 全量回归 + 提交**

Run: `go vet ./... && go test ./...`
Expected: 全绿

```bash
git add tools.go tools_test.go
git commit -m "feat: five wiki write MCP tools gated by enable_ingest

wiki_create_page / wiki_update_page / wiki_delete_page /
wiki_create_folder / wiki_move_page — thin REST dictionaries for the
external wiki generation skill (spec Part 1). No pipeline logic in
the server: absent-field-keeps-value update semantics, whitelist
validation, traversal-guarded slugs, WeKnora errors passed through.

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

### Task 3: README 工具表 + 版本 v0.3.0

**Files:**
- Modify: `README.md`（工具数、写入小节、错误码表）
- Modify: `go.mod`（无改动的确认；版本走 git tag，无代码改动）

**Interfaces:**
- Consumes: Task 2 的 5 个工具名与参数
- Produces: README 文档；git tag `v0.3.0`

- [ ] **Step 1: 更新 README**

- 第 12-14 行工具计数：`10 个工具` → `15 个工具：检索 4 + wiki 3 + 写入 3 + wiki 写入 5（默认关闭）`——改为准确表述：检索 4 + wiki 3 + 写入 3（文档）+ wiki 写入 5，`enable_ingest: true` 时注册后两组
- 「写入 3 个」小节（README.md:91-97）改为「写入 8 个（配置 `enable_ingest: true` 时注册）」：3 个文档工具行 + 5 个 wiki 工具行（工具、参数、说明三列；wiki 行说明写清 REST 语义：create 的 slug 唯一/409→500 透传、update 的缺席保持原值、move 的 folder_id 空=根）。**注意写准**：撞 slug 是服务端 500（唯一索引冲突）不是 409，README 不写具体状态码则只写"撞已有 slug 会报错"
- 部署小节镜像 tag `v0.2.0` → `v0.3.0`（README.md:119）
- 检查首段流量图/工具计数一致

- [ ] **Step 2: 验证文档一致性**

Run: `grep -n "个工具" README.md && grep -n "v0\." README.md | head`
Expected: 计数与 tag 一致，无 v0.2.0 残留

（v0.3.0 tag 由 Task 6 的发布流程处理，本 task 只改文档。）

- [ ] **Step 3: 提交**

```bash
git add README.md
git commit -m "docs: README for v0.3.0 (wiki write tools)

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

### Task 4: Skill — `skills/weknora-wiki-ingest/SKILL.md`（本仓库内）

**Files:**
- Create: `skills/weknora-wiki-ingest/SKILL.md`（本仓库，随 v0.3.0 版本化发布；消费侧部署时整体拷贝/链接到 agent 的 skills 目录）
- Create: `skills/weknora-wiki-ingest/REFERENCES.md`（可选，若 SKILL.md 超长则把 C1-C11 事实表+slug 规则拆过去）

**Interfaces:**
- Consumes: Task 2 的 5 个工具 + 既有 `read_document`/`wiki_search`/`wiki_read_page`/`wiki_index`
- Produces: 用户可 `/weknora-wiki-ingest` 调用的 skill（中文主文案、slug/字段名英文）；随 repo 提交（与 Task 1-3 同仓发布）

- [ ] **Step 1: 写 SKILL.md（Steps 0-5，内容按 spec Part 2）**

frontmatter（name + description 触发词要覆盖"给 wiki 加文档/同步 wiki/外部 wiki 生成/EnableIngest 等价"）。正文按 spec Part 2 的 Step 0-5 组织，逐条落实：

**Step 0 前置检查（可判定阻断）**：
- `list_knowledge_bases` 确认目标 KB 存在 → `list_documents` 拿 KB 元数据？——**不可行**，REST 无 KB 详情工具。改为：skill 文案写死操作前置（82 KB 的 `wiki_config.synthesis_model_id` 与 `summary_model_id` 均已清空，见 runbook），并给一个运行时探测：`wiki_index` 正常返回即 wiki 可读；skill 明确声明"本 skill 假设内部生成已关闭（runbook 已执行）；若不确定，停止并要求用户确认"
- 若 `knowledge_id` 的文档 `parse_status` 不是 completed → 停止，告知用户等解析完成

**Step 1 读取**：`read_document(knowledge_id, chunk_type=["text","image_ocr"])` 分页拉全 → 拼接去重 → 超过 32768 runes 截断（对齐内部 `maxContentForWiki`）；真实文本 <10 runes → 跳过并报告（对齐 `minTextContentRunes`）

**Step 2 抽取**（agent 自己做，规则来自内部 prompts_wiki.go）：实体（人物/组织/产品/地点/技术/事件，被实质性讨论：≥2 次提及或成段描述）、概念（主题/方法论/理论）；每项产出 `{name, slug: entity|concept/<slugify(name)>, aliases[], description (15-40 词), details (2-5 句)}`。slugify 规则：小写、空格/下划线→`-`、保留 CJK 与 `[a-z0-9-/]`、折叠连续 `-`、截 200 字符；中文用拼音/罗马化

**Step 3 重名判定**：对每个候选的 name 和每个 alias 逐个 `wiki_search`（limit 50）：
- 命中同型（entity↔entity）且标题/别名相同 → 合并：复用既有 slug，走 `wiki_update_page`，把新 details 并入既有 content（编辑规则：**编译器不创作、无证据不写、冲突加 "Contradictions / Updates" 段**）
- 不同型但同 slug（entity/foo vs concept/foo）→ slug 冲突，加后缀重生成（如 `entity/foo-2`）
- 仅相似不同一 → 新建
- 大 KB 多轮策略：limit=50 仍打满 → 改写 query 收窄（如加类型前缀）再搜

**Step 4 summary 页**：slug `summary/<knowledge_id>`（UUID 经 slugify 恒等，可复现）；标题 `"<doc_title> - Summary"`；页首 `SUMMARY: <15-40 词>` 行。重灌时先 `wiki_read_page` 读旧 summary 提取"上次贡献"清单（旧页 `[[链接]]` 集合），reconcile：
- 旧 slug ∉ 新抽取 → 该文档不再支撑该页：从页 content 删掉本 doc 贡献段；若收缩后页 content 已是空壳（无实质段落）→ 整页 `wiki_delete_page`（**按内容空判定，不依赖 source_refs 计数**——REST 改不了 source_refs）
- 旧 slug ∈ 新抽取 → 走合并 update
- summary 页本身 → 整页覆写（对齐内部行为）

**Step 5 目录 + index**：
- 目录：`wiki_index` 拿现状 → 新页归入既有分类（folder_id 从 index 的 folder 信息拿）；无合适分类则 `wiki_create_folder` + `wiki_move_page`
- index intro（**关键顺序，spec Step 5 修正版**）：
  1. `wiki_index` → `{intro, version}`——首次读取**必须**走 `wiki_index`（服务端懒物化；`wiki_read_page(slug="index")` 在全新 KB 会 404）
  2. intro 为空/默认占位（`"# Wiki Index\n\nThis is the index page..."` 形状）→ 首建分支：聚合本批 summary 页标题+SUMMARY 行，从零写 intro（语义对齐 `WikiIndexIntroPrompt`：1 标题行+2-3 句）；否则 → 增量分支：现有 intro + 本批 `<document_added/removed>` 变更描述，按 `WikiIndexIntroUpdatePrompt` 语义重写（保持语气、只提显著扩范围的新主题、删失效引用、1 标题行+2-3 句、**不生成目录**）
  3. 写回 `wiki_update_page(slug="index", content=新intro, summary=新intro, version=第 1 步返回值)`——**永远不 `wiki_create_page` index**；写回前自查输出不含 `\n## ` 目录式段落（对齐内部 `rebuildIndexPage` 防御）
  4. intro 重写不理想 → 保留原 intro 跳过（对齐内部失败降级），本批其余页面不受影响
- intro 重写只有 summary 页 add/remove 触发（entity/concept 页增删不触发）——**全量对齐内部行为**（见记忆 weknora-index-page-anatomy）

- [ ] **Step 2: skill 自查（结构对照 spec）**

对照 spec Part 2 逐条勾：Step 0 阻断条件、Step 1 双门限（32768/10 runes）、Step 2 抽取规则+slugify、Step 3 三分支重名判定、Step 4 reconcile 含内容空判定、Step 5 懒物化顺序与 index 永不 create。skill 文案必须让执行 agent "只能写出一种合理动作"。

- [ ] **Step 3: 提交**

skill 在本仓库内，正常 git 提交（区别于原先"写进 ~/.claude/skills 无提交"的方案）。

```bash
git add skills/weknora-wiki-ingest/
git commit -m "feat: weknora-wiki-ingest skill (external wiki generation pipeline)

Steps 0-5 per spec Part 2: prerequisite checks, chunked read with
32768/10-rune gates, extraction rules, duplicate merge via wiki_search,
summary-page reconcile with content-empty deletion, folder + lazy-index
intro update sequence.

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

### Task 5: 82 runbook

**Files:**
- Create: `docs/superpowers/runbooks/2026-11-25-external-wiki-generation-runbook.md`（本 repo 内，随 v0.3.0 一起发布）

**Interfaces:**
- Consumes: Task 3 的 v0.3.0、Task 4 的 skill
- Produces: 82 运维文档（清 synthesis model 步骤、验证步骤）

- [ ] **Step 1: 写 runbook（内容如下）**

标题「82 外部 wiki 生成上线 runbook」。分节：

1. **前置状态确认**：目标 KB `wiki_enabled=true`（`GET /api/v1/knowledgebase/{kb}` 或控制台）；82 的 MCP key 已具备 retrieve+ingest 能力（v0.2.1 时已配）
2. **停内部管线**（关键安全步骤——两条管线不能同时跑）：
   - 控制台路径：WeKnora 控制台 → 知识库设置 → wiki 设置 → 清空 synthesis model
   - API 路径：`PUT /api/v1/knowledge-bases/{kb}` body `{"wiki_config": {"synthesis_model_id": ""}}`——**注意**：该路由对 scoped API key 要求 `manage_kbs` 能力（`routes_knowledge.go:235-245`），82 现有 key 只有 retrieve+ingest → **用管理员 token / 控制台操作**，或临时给 key 加 `manage_kbs` 能力
   - 验证：清完后给 KB 发个测试文档，观察 `list_documents` parse_status=completed 且 wiki 无新页（内部管线静默丢弃，C2）——若 wiki 页仍在长，模型没清干净
   - **回滚**：恢复 `wiki_config.synthesis_model_id` 即可重新启用内部管线
3. **部署 v0.3.0**：镜像升级（configmap/secret 不动），参照 82 `~/config/weknora-mcp/how-to-config.md` 的部署模式
4. **部署 skill**：从本仓库 `skills/weknora-wiki-ingest/` 拷贝（或 symlink）到 agent 侧的 skills 目录（multica agent 能发现的位置）；skill 与 MCP v0.3.0 的 5 个 wiki 写工具是配套版本——skill 依赖这些工具，别配旧镜像
5. **端到端验证**（对应 spec 验收节）：
   - 给测试文档跑 skill → 产出 summary+entity/concept 页、aliases、内链、source_refs 正确
   - 对同名实体二次灌不同文档 → 合并不重建
   - 删除文档 → 页面收缩/删除
   - index intro 反映最新文档集
6. **操作参数**：82 值在 82 上直接改，本地占位副本不覆盖（记忆约束）

- [ ] **Step 2: 提交**

```bash
git add docs/superpowers/runbooks/2026-11-25-external-wiki-generation-runbook.md
git commit -m "docs: 82 rollout runbook for external wiki generation

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

### Task 6: 发布 v0.3.0（tag + release）

**Files:**
- 无代码改动；git tag + GitHub release（release.yml 已存在）

**Interfaces:**
- Consumes: Task 1-5 全部提交已入 main
- Produces: tag `v0.3.0` + GHCR 镜像 + release assets（静态二进制 + air-gapped tar）

- [ ] **Step 1: 打 tag**

Run: `git tag -a v0.3.0 -m "v0.3.0: wiki write tools for external wiki generation"`
Expected: tag 成功

- [ ] **Step 2: push + 触发 release**

Run: `git push origin main --tags`
Expected: release.yml 触发（CI 产出镜像+assets）；产物确认在 GitHub Releases 页

- [ ] **Step 3: 82 侧部署按 Task 5 runbook 执行（人工步骤，由用户在 82 上操作）**

确认项：镜像 v0.3.0 运行、`tools/list` 含 5 个 wiki 写工具（enable_ingest 已开）、runbook 第 2 步（清 synthesis model）已完成、端到端验证通过
