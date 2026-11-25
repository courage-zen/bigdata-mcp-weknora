# WeKnora 外部 Wiki 生成（EnableIngest 等价）— 设计

日期：2026-11-25
状态：待用户批准

## 意图与动因

WeKnora 0.9.0 的 `IndexingStrategy.WikiEnabled`（文档里口语化叫 "EnableIngest"）
在文档解析完成后触发一条内部 LLM 管线：抽取实体/概念、生成文档 summary 页、
合并进既有 wiki 页、维护目录与 index 页。82 上该 KB **已配置 synthesis
model，内部管线正在生成**。

我们要的是把这条管线**搬到 WeKnora 外部**：multica agent（Claude）本身当
抽取器 + 编辑器，通过我们 MCP 的 REST 工具读写 wiki。好处：生成质量受益于
agent 自身的推理而不是 WeKnora 配置的单个模型；生成过程对 agent 可见、可
干预；WeKnora 侧不占用 wiki worker 池。代价：每次调用都是"agent 亲自跑全
流程"，不是后台自动触发——这是选定的取舍（手动/按需触发）。

## 目标

- 给定一个已解析完成的文档（`knowledge_id`），agent 通过 MCP 工具 + skill
  等价复刻内部管线的产出：
  1. `summary/<knowledge_id>` 汇总页（slug 逐字节对齐内部约定）
  2. 实体页 `entity/<slug>`、概念页 `concept/<slug>`，带 aliases、
     SUMMARY 行、`[[slug|name]]` 内链、`source_refs` 溯源
  3. 重名（同一实体已有页面）时合并进既有 slug，不新建重复页
  4. 文档删除/更新后的收缩与再同步（全量 parity）
  5. index 页 intro 反映最新文档集
- MCP 保持"薄 REST client"定位：新增工具零业务逻辑

## 非目标 / 反目标

- **不**在 MCP 服务端内嵌 LLM 调用或整条管线（违背 REST-only 定位；且服务端
  没有模型上下文）
- **不**做 chunk 级引用（`chunk_refs`）：REST `WikiPageUpdateRequest` 不开放
  该字段，内部管线的 chunk 引用能力放弃
- **不**做批量/并发调度、重试预算、pending-ops 持久化：单文档同步流，天然
  无竞态（内部管线 6000 行里大半是这些）
- **不**追求与内部管线逐 token 一致：页面结构、slug 约定、链接语法对齐即可
- 反目标：两条管线同时跑（内外打架、页面互相覆盖）——见"前置条件"

## 约束（REST 层事实，全部已在源码核实）

| # | 事实 | 出处 |
|---|------|------|
| C1 | wiki 整组路由（**读+写都算**）要求 API key 具备 `ingest` 能力（或 full access）；写路由再叠加 RBAC。82 的 key 已是 retrieve+ingest，现有 wiki 读工具已依赖此能力 | `routes_knowledge.go:354-391` |
| C2 | 内部管线没配 synthesis model 时静默丢弃任务、文档照常完成 | `wiki_ingest.go:777 releaseIngestForUnavailableWiki` |
| C3 | `POST /wiki/pages` 接受完整 `types.WikiPage`（含 `source_refs`、`aliases`、`folder_id`、`page_type`、`status`）；`PUT` 的 `WikiPageUpdateRequest` 只有 title/content/summary/page_type/status/aliases/version——**改不了 source_refs**。create 撞已有 slug 时 repo 返回唯一索引冲突、handler 统一映射为 **500**（不是 409）；`ErrWikiPageConflict`/409 只在 update 乐观锁路径 | `wiki_page.go(handler):376-411, 501-587`；`types/wiki_page.go:386-397`；`repository/wiki_page.go:96-98` |
| C4 | 写页时服务端自动解析 `[[slug\|name]]` 维护 in/out links | `wiki_page.go(service):99,114` |
| C5 | `wiki_search`（`GET /wiki/search`，POSIX `~*`）按 title>slug>summary>content 排序、排除 archived、limit≤50 | `repository/wiki_page.go:1486-1531` |
| C6 | `slugify`：小写、空格/下划线→`-`、保留 CJK 与 `[a-z0-9-/]`、折叠连续 `-`、截 200 字符。对 UUID 是恒等映射 → `summary/<knowledge_id>` 可复现 | `wiki_ingest.go:3315` |
| C7 | summary 页标题约定 `"<doc_title> - Summary"`；页首 SUMMARY 行格式 `SUMMARY: <15-40 词>`；页型 summary/entity/concept/index/synthesis/comparison | `wiki_ingest_batch.go:2033`；`prompts_wiki.go` |
| C8 | 索引页固定 slug `index`、`page_type=index`；intro 存在 Content+Summary | `wiki_page.go(service):521` |
| C9 | `DELETE /wiki/pages/<slug>` 返回 204/404，硬删 | `wiki_page.go:773-801` |
| C10 | `POST /wiki/folders`（建）、`PUT /wiki/folders/:id`（改名/移动）、`DELETE /wiki/folders/:id`（仅空文件夹）、`POST /wiki/move-page`（slug+folder_id） | routes + handler |
| C11 | 内部管线内容上限 32768 runes、最少 10 runes 真实文本才跑抽取 | `wiki_ingest.go:42, 3364` |

## 方案取舍

- **A（选定）薄 MCP + 厚 skill**：MCP 只加"写字典"级 REST 映射；管线逻辑全部
  进 skill，由 agent 执行。符合项目 REST-only 定位；skill 迭代零部署成本。
- **B MCP 服务端整条管线**：被否——服务端无 LLM、违背定位、6000 行内部逻辑
  的竞态处理在一个外部进程里只会更糟。
- **C 保留内部管线、只换模型**：被否——不是"外部实现"，且无法干预生成过程。

## 设计

### Part 1 — MCP 新工具（本仓库，Go）

新增 `addWikiWriteTools`，仍由 `Config.EnableIngest` 门控
（`tools.go:138`）。全部走既有 `callTool`/`resolveMapping`/`mapWeKnoraError`
模式；`weknora.go` 加对应 client 方法。slug 校验复用 `validateWikiSlug`。

1. `wiki_create_page`
   `POST /api/v1/knowledgebase/{kb}/wiki/pages`
   参数：`slug`(必)、`title`、`content`、`summary`、`page_type`(summary|
   entity|concept|index|synthesis|comparison)、`aliases[]`、`source_refs[]`
   （`"knowledge_id"` 或 `"knowledge_id|title"`）、`folder_id`、`status`
   (draft|published，默认 published)
2. `wiki_update_page`
   `PUT /api/v1/knowledgebase/{kb}/wiki/pages/{slug}`
   参数：`slug`(必)、`title`/`content`/`summary`/`page_type`/`status`/
   `aliases[]`（可选字段，缺席保持原值——服务端语义）、`version`(可选乐观锁)
3. `wiki_delete_page`
   `DELETE …/wiki/pages/{slug}` → 204
4. `wiki_create_folder` `POST …/wiki/folders`：`name`、`parent_id`
5. `wiki_move_page` `PUT …/wiki/move-page`：`slug`、`folder_id`

读路径完全复用现有 `wiki_search` / `wiki_read_page` / `wiki_index` /
`read_document`。**不加**新的查询工具——重名探测（按 name+aliases 逐个
`wiki_search`）现有工具已覆盖（C5）。

工具命名/校验细节（实现约定）：
- handler 参数校验沿用现有风格：必填缺失 → `errCodeMissingParam`；类型/取值
  非法 → `errCodeInvalidParamValue`；`slug` 复用 `validateWikiSlug`，
  `page_type`/`status` 用 WeKnora 的合法值白名单；WeKnora 错误统一走
  `mapWeKnoraError`（409 冲突、404、400 语义原样透传）
- `source_refs` 条目格式在 handler 里做形状校验（非空、`|` 只按分隔符对待），
  但不做语义校验——knowledge_id 是否存在由服务端决定
- 幂等性语义跟服务端走：`wiki_create_page` 撞已有 slug → 服务端报错（uniqueIndex
  `idx_kb_slug`，当前 handler 把 service 错误统一映射为 500），原样透传即可，不做
  "先查再建"的 TOCTOU 竞态；skill 侧以 `wiki_search` 预探测为常规路径，撞车错
  误就报给用户处理
- client 方法路径常量带单测锁定（复刻 `TestWeKnoraClient_IngestPaths` 被
  v0.2.1 教训 pin 住 `/knowledge` 段的做法）

### Part 2 — Skill：`weknora-wiki-ingest`（放本仓库 `skills/weknora-wiki-ingest/`，
随 v0.3.0 一起版本化发布；消费侧部署时整体拷贝/链接到 agent 的 skills 目录）

Skill 是"管线"本身，指令按内部 5 步组织。关键对齐点（skill 内要写死）：

**Step 0 前置**：KB 必须 `wiki_enabled=true` 且**内部 synthesis model 已清空
（`wiki_config.synthesis_model_id` 与 `summary_model_id` 均空）**——否则两条
管线打架。82 已配模型，上线前必须先清（用 WeKnora 控制台或
`PUT /api/v1/knowledgebase/{id}`）。

**Step 1 读取（Map 输入）**：`read_document(knowledge_id, chunk_type=["text","
image_ocr"])` 分页拉全 → 拼接、去重 → 超 32768 runes 截断；真实文本 <10
runes 则跳过（对齐 C11）。

**Step 2 抽取（agent 自己做，规则来自 `prompts_wiki.go`）**：
- 实体：人物/组织/产品/地点/技术/事件，被实质性讨论（≥2 次提及或成段描述）
- 概念：主题/方法论/理论
- 每项产出 `{name, slug: entity|concept/<slugify(name)>, aliases[], description
  (15-40 词索引行), details(2-5 句)}`
- slug 规则 = C6 的 slugify；中文用拼音/罗马化（agent 能做，内部也是靠模型）

**Step 3 重名判定（用户点名的查询需求）**：对每个候选项，用 name 和每个
alias 逐个 `wiki_search`（limit 50）：
- 命中同型（entity↔entity）且标题/别名相同 → **合并**：复用既有 slug，走
  update 路径，把新 details 并入既有内容（编辑规则照 `WikiPageModifySystem
  Prompt`：编译器不创作、无证据不写、冲突加 "Contradictions / Updates"）
- 不同型但同 slug（如 entity/foo vs concept/foo）→ slug 冲突，加后缀重生成
- 仅相似不同一 → 新建

**Step 4 summary 页**：`summary/<knowledge_id>`（C6/C7）。重灌时先
`wiki_read_page` 读旧 summary 提取"上次贡献"清单（旧页 `[[链接]]` 集合），
reconcile：
- 旧 slug ∉ 新抽取 → 该文档不再支撑该页：从页 content 删掉本 doc 贡献段；
  若页 `source_refs` 只剩本 doc（`wiki_read_page` 可见）→ 整页删除（C9）
- 旧 slug ∈ 新抽取 → 走合并 update
- summary 页本身 → 整页覆写（对齐内部行为）
注：REST 改不了 source_refs（C3），收缩后页上残留的 source_refs 条目无法手工
摘除——接受该限制，靠内容收敛表达"本 doc 不再支撑"。受此影响，"页
source_refs 只剩本 doc → 删除"的判定按**内容收缩后是否已空**执行：页 content
已被本次收缩清到近似空壳（无实质段落）即删，不依赖 source_refs 计数。

**Step 5 目录 + index**：
- 目录：`wiki_index` 拿现状 → 新页归入既有分类；无合适分类则
  `wiki_create_folder` + `wiki_move_page`（对齐内部 taxonomy 规则：复用精确
  标签、同类同级）
- index：**index 是物理 wiki 页但只存 intro**（slug=`index`、`page_type=index`，
  `Content`/`Summary` 仅含标题+2-3 句 intro；目录由 `GET /wiki/index`
  按页类型实时组装、不落库）。正确序列：
  1. `wiki_index` → `{intro, version}`——该调用走 `GetIndex`，**服务端懒物化**
     缺省 index 行（`wiki_read_page(slug="index")` 的 `GetPageBySlug` 路径
     不触发懒创建，全新 KB 上会 404，不要用它做首次读取）
  2. intro 为空/默认占位 → 首建分支：聚合本批 summary 页标题+SUMMARY 行，
     按 `WikiIndexIntroPrompt` 语义从零写 intro；否则 → 增量分支：现有
     intro + 本批 `<document_added/removed>` 变更描述，按
     `WikiIndexIntroUpdatePrompt` 语义重写（保持语气、只提显著扩范围的新
     主题、删失效引用、1 标题行+2-3 句、不生成目录）
  3. `wiki_update_page(slug="index", content=新intro, summary=新intro,
     version=第1步返回值)`——**永远不 `wiki_create_page` index**，物化交给
     服务端；写回前自查输出不含 `\n## ` 目录式段落（对齐内部
     `rebuildIndexPage` 的防御）
  4. intro 重写不理想 → 保留原 intro 跳过（对齐内部失败降级），本批其余
     页面不受影响

### 验收

- MCP：新工具单测（handler 参数校验 + client 路径拼接，沿用
  `TestWeKnoraClient_IngestPaths` 模式）；`go vet`/`go test` 绿
- 端到端（82 staging）：对一个测试文档跑 skill → 产出 summary+entity/concept
  页、aliases、内链、source_refs 正确；对同名实体二次灌不同文档 → 合并不重
  建；删除文档 → 页面收缩/删除

## 交付物

1. 本仓库：Part 1 工具 + 测试 + README 工具表更新（v0.3.0）
2. 本仓库 `skills/weknora-wiki-ingest/SKILL.md`：Part 2 完整指令
3. 82 操作 runbook（清 synthesis model、验证步骤）

## 留给实现者

- ~~`folder_id` 参数与 `validateWikiSlug` 的校验细节~~（已并入 Part 1 实现约定）
- skill 文案措辞（中文为主，保留 slug/字段名英文）
- runbook 里的具体 k8s/WeKnora 控制台操作路径

## 实现者自查记录（2026-11-25）

subagent builder-check 两次因推理网关 504 失败，降级为主对话内自查。发现并
已处置 6 项：

1. **[已修] C1 表述错误**：原文"wiki 路由要求 KB wiki_enabled=true；写路由
   还要 API key ingest 能力"混淆了两个独立门——`(kb_id)` 路由参数有效性
   （`validateWikiKB` 检查 `kb.IsWikiEnabled()`，通过 400 表达）与 API key
   能力（`apiKeyIngest` 中间件，wiki 整组读+写都要求 ingest 能力或 full
   access）。已改写为准确表述。**无实质设计影响**：82 的 key 已配 ingest，
   两个门都过。
2. **[已修] 405 路由缺失**：初稿曾提 `DELETE /wiki/folders`，实际路由是
   `DELETE /wiki/folders/:folder_id`（`routes_knowledge.go:374`）。C10 已
   写为准确路径。
3. **[已定] 幂等/竞态**：`wiki_create_page` 撞 slug 依赖服务端唯一索引报错
   （`idx_kb_slug` uniqueIndex，当前 handler 映射为 500——勘误：初稿写的 "409"
   不准确，409 只在 update 乐观锁路径），不做"先查后建"——避免 TOCTOU；已写入
   Part 1 实现约定。
4. **[已定] 错误映射**：`mapWeKnoraError` 不按状态分支——任何 `*weknoraError`
   统一 `WEKNORA_API_FAILED` 并透传状态码+message（400/404/409/500 语义原样可
   辨），无需扩展；约定已写入 Part 1。
5. **[已定] 孤页判定在 source_refs 不可写之后的口径**：Step 4 已补"按内容
   收缩后是否已空判定删除，不依赖 source_refs 计数"。
6. **[已记] `search` limit 上限 50**：C5 已含；skill Step 3 逐名探测时对
   大 KB 的多轮分页策略留给 skill 文案（limit=50 + 改写 query 收窄）。

自查结论：无 [theirs] 级问题——无需用户再决策；文档已可进入计划/实现阶段。

## 计划期勘误（2026-11-25，writing-plans recon）

写实现计划时对 REST 面再核实，修正三处事实（均已并入上文）：

1. **[已修] move-page 是 PUT 不是 POST**：路由注册 `wiki.PUT("/move-page", …)`
   （`routes_knowledge.go:355`）。Part 1 工具 5 与 C10 已更正。
2. **[已修] create 撞 slug → 500 不是 409**：CreatePage handler 对 service 错误
   统一 `http.StatusInternalServerError`（`wiki_page.go(handler):407-410`）；
   `ErrWikiPageConflict`/409 只在 update 乐观锁路径。C3、Part 1 实现约定、自查
   记录第 3 条已更正。无实质设计影响：反正原样透传，skill 侧预探测
   `wiki_search` 仍是常规路径。
3. **[已修] 错误映射表述**：`mapWeKnoraError` 无按状态分支，任何
   `*weknoraError` → `WEKNORA_API_FAILED`（状态+message 透传）。自查记录第 4
   条已更正。

另核实（无需改设计，确认可行性）：wiki 写路由的 `OwnedWikiKBOrAdmin` RBAC 守卫
对 API-key principal 直接放行（`access.CheckOwnershipOrRole`：`request.APIKey ==
true → return nil`，`ownership.go:57-59`），API key 的真实权限门是
`apiKeyIngest` capability——82 的 key 已具备，REST 层方案成立。
