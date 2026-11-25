---
name: weknora-wiki-ingest
description: Use when asked to add a document to the wiki, sync wiki pages from a document, generate wiki pages for a knowledge-base document, or replicate WeKnora's internal EnableIngest wiki generation from outside (external wiki generation). Reads one document's chunks, extracts entities/concepts, merges with existing pages via wiki_search, reconciles summary pages, and updates folders + the lazy index page. Requires the weknora-mcp wiki write tools (enable_ingest: true).
---

# WeKnora 外部 wiki 生成（weknora-wiki-ingest）

把一篇已入库文档的内容转化为 wiki 页面——WeKnora 内部 EnableIngest 的外部等价物。
输入：一个 `knowledge_id`。产出：summary 页 + entity/concept 页 + 目录归类 + index intro 更新。

调用工具均为 weknora-mcp 提供：`read_document` / `wiki_search` / `wiki_read_page` /
`wiki_index` / `wiki_create_page` / `wiki_update_page` / `wiki_delete_page` /
`wiki_create_folder` / `wiki_move_page`。

## Step 0：前置检查（任何一项不满足 → 停止并告知用户，不继续）

1. **内部生成必须已关闭**：目标 KB 的 `wiki_config.synthesis_model_id` 与
   `summary_model_id` 均已清空（由管理员经 WeKnora 控制台或
   `PUT /api/v1/knowledgebase/{id}` 完成，见 runbook）。本 skill 无法经 REST 查询或
   修改该配置；若不确定已清空，**停止并要求用户确认**——否则内外两条管线会打架。
2. **wiki 可读**：`wiki_index` 正常返回（它同时是后面 index 步骤的服务端懒物化入口）。
   若报错 → 停止并报告错误。
3. **文档解析完成**：`list_documents` 找到该 `knowledge_id`，其 `parse_status` 为
   `completed`。不是 completed → 停止，告知用户等解析完成后再跑。
4. **写入工具可用**：`wiki_create_page` 等写入工具在工具列表中（`enable_ingest: true`）。
   不可用 → 停止，让用户在 weknora-mcp 配置开启后重试。

## Step 1：读取文档

- `read_document(knowledge_id, chunk_type=["text", "image_ocr"], page_size=100)` 分页拉全
  （`page` 从 1 递增直到返回不足一页）。
- 拼接所有 chunk 文本，去重相邻重复；OCR 文本与正文同等对待。
- **32768 runes 上限**：拼接后超过 32768 个字符（runes）→ 截断到 32768，并记录
  “已截断”供报告。
- **10 runes 下限**：截断/去重后的真实文本不足 10 个字符 → 跳过本文档（不产出任何
  页面），报告“文档无有效文本”。

## Step 2：抽取实体与概念（你自己做，不调用工具）

从 Step 1 的全文中抽取，规则对齐 WeKnora 内部 prompts：

- **实体（entity）**：人物、组织、产品、地点、技术、事件——且被**实质性讨论**
  （≥2 次提及，或成段描述）。仅一次顺带提及的不抽。
- **概念（concept）**：主题、方法论、理论。
- 每项产出：
  - `name`：规范名（原文主要用法）
  - `slug`：`entity/<slugify(name)>` 或 `concept/<slugify(name)>`
  - `aliases[]`：文中出现的别名/缩写（可空）
  - `description`：15-40 词的索引行（wiki_search 命中它）
  - `details`：2-5 句正文要点（依据原文，不外推）

**slugify 规则**（对齐内部 slugify）：转小写；空格与下划线转 `-`；保留 CJK 字符与
`a-z0-9-`；折叠连续 `-`；最长 200 字符。中文按拼音/罗马化转写（如 张三 → zhang-san）。

## Step 3：重名判定与合并（每个候选、每个 name/alias 逐个查）

对每个候选项，用 `name` 和每个 `alias` 分别调 `wiki_search(query=<term>, limit=50)`：

1. **命中同型且标题/别名相同**（如候选 entity/RAG 命中既有页 entity/rag，标题相同或
   互为别名）→ **合并**：复用既有 slug，走 `wiki_update_page`，把本次 `details` 按
   下面的编辑规则并入既有 content。aliases 并集。**不要 `wiki_create_page`**。
2. **不同型但同 slug**（候选 concept/foo，既有 entity/foo）→ slug 冲突：把候选项
   slug 加后缀重生成（如 `concept/foo-2`），新建。
3. **仅相似不同一**（如 entity/RAG 与 entity/Retrieval-Augmented-Generation 是两页）
   → 新建。
4. **大 KB 多轮**：任一 `wiki_search` 返回打满 50 条 → 改写 query 收窄（加类型前缀
   `entity foo`、加限定词）再搜一轮，仍打满则按 1-3 判定。

**编辑规则**（并入既有 content 时，对齐内部 `WikiPageModifySystemPrompt`）：
- 你是编译器不是作者：只组织本次文档给出的信息，不创作、不外推
- 无证据不写：本次文档没提的既有段落保持原样
- 与既有内容冲突 → 不改写既有段落，在页尾追加 `## Contradictions / Updates` 段列出冲突点与出处

## Step 4：summary 页 reconcile

summary 页 slug：`summary/<knowledge_id>`（knowledge_id 是 UUID，slugify 恒等，可复现）。

- **新建文档**：`wiki_create_page(slug="summary/<knowledge_id>", title="<doc_title> - Summary",
  page_type="summary", source_refs=["<knowledge_id>"], content=…)`。content 页首一行
  `SUMMARY: <15-40 词>`，正文按本次抽取的页逐段小结，每段内链 `[[entity/foo]]` /
  `[[concept/foo]]`。
- **重灌文档**（该 summary 页已存在）：先 `wiki_read_page("summary/<knowledge_id>")`
  读旧页，从旧页 content 的 `[[链接]]` 集合提取“上次贡献”清单，然后 reconcile：
  - 旧 slug ∈ 新抽取 → 该页走 Step 3 的合并 update
  - 旧 slug ∉ 新抽取 → 本次文档不再支撑该页：从该页 content 删掉本 doc 贡献段
    （`wiki_update_page`）。若收缩后页 content 已是空壳（无实质段落，仅剩标题/
    SUMMARY 行）→ 整页 `wiki_delete_page`。**按内容空判定，不数 source_refs**——
    REST 改不了 source_refs，残留条目无法摘除，只能靠内容收敛表达“本 doc 不再支撑”。
  - summary 页本身 → 整页覆写（不 merge，直接 `wiki_update_page` 写新 content）

## Step 5：目录归类 + index intro 更新

### 目录

`wiki_index` 拿现状 → 新页逐页归入既有 folder；无合适 folder 则
`wiki_create_folder(name=…)` 后 `wiki_move_page(slug, folder_id)`。归类规则：复用精确
匹配的既有分类，同级同类；不为一页新建多层目录。

### index intro（顺序关键，不可调换）

1. `wiki_index` → `{intro, version}`。**首次读取必须走 `wiki_index`**——服务端在
   `GET /wiki/index` 懒物化缺省 index 行；`wiki_read_page(slug="index")` 不触发懒创建，
   全新 KB 上会 404，不要用它做首次读取。
2. 分支：
   - intro 为空或默认占位（`"# Wiki Index\n\nThis is the index page..."` 形状）→
     **首建分支**：聚合本批 summary 页标题 + SUMMARY 行，从零写 intro。
   - 否则 → **增量分支**：拿现有 intro + 本批变更描述（`<document_added/removed>`），
     重写 intro。语义对齐内部 update prompt：保持现有语气；只加显著扩范围的新主题；
     删掉失效引用；1 个标题行 + 2-3 句；**不生成目录**。
3. `wiki_update_page(slug="index", content=新intro, summary=新intro, version=第 1 步
   返回的 version)`。**永远不要 `wiki_create_page` index**——物化交给服务端。
   写回前自查：新 intro 不含 `\n## ` 目录式段落；含了 → 重写为纯 intro 再写回。
4. intro 重写不理想（拿不准、写不出比原文好的）→ 保留原 intro 跳过；本批其余页面
   不受影响。

**intro 重写触发条件**：只有 summary 页的增删触发（`<document_added/removed>`）；
entity/concept 页的增删**不触发** intro 重写。

## 收尾报告

向用户报告：读取字符数与是否截断、抽取的实体/概念数、合并/新建/删除的页数、
目录归类情况、index intro 是否更新（及原因）。
