# 82 外部 wiki 生成上线 runbook

目标：在 82 上把 WeKnora 内部 EnableIngest wiki 生成切换为外部生成（MCP v0.3.0
wiki 写工具 + `weknora-wiki-ingest` skill），两条管线不并存。

## 1. 前置状态确认

- 目标 KB `wiki_enabled=true`：控制台 wiki 页或 `GET /api/v1/knowledgebase/{kb}`
  确认（用管理员 token；scoped key 无此路由的读权限）。
- 82 的 MCP key 已具备 retrieve + ingest 能力（v0.2.1 上线时已配），无需改动。
  检查：`list_knowledge_bases`、`add_document` 用该 key 正常工作。

## 2. 停内部管线（关键安全步骤——两条管线不能同时跑）

内部 wiki 生成由 KB 的 `wiki_config.synthesis_model_id` / `summary_model_id` 驱动；
任一非空，文档解析完成后内部管线就会生成 wiki 页，与外部 skill 冲突。

**清空模型（两个都清）**，任选一条路径：

- **控制台路径（推荐）**：WeKnora 控制台 → 知识库设置 → wiki 设置 → 把
  synthesis model 与 summary model 都清空保存。
- **API 路径**：`PUT /api/v1/knowledge-bases/{kb}`，body 带
  `{"wiki_config": {"synthesis_model_id": "", "summary_model_id": ""}}`。
  **注意**：该路由对 scoped API key 要求 `manage_kbs` 能力，82 现有 key 只有
  retrieve+ingest → **用管理员 token 调用**，或临时给 key 加 `manage_kbs`
  能力、清完即撤。

**验证已停**：清完后给 KB 发一个测试文档（`add_document`），等
`list_documents` 显示 `parse_status=completed`，再看 wiki（`wiki_index`）——
不应有任何新页。若 wiki 页仍在长，模型没清干净，回到上一步。

**回滚**：恢复 `wiki_config.synthesis_model_id`（及 `summary_model_id`）即可
重新启用内部管线；外部管线停跑 skill 即停。

## 3. 部署 v0.3.0

weknora-mcp 镜像升级 `v0.2.x` → `v0.3.0`，configmap/secret 不动。部署模式参照
82 `~/config/weknora-mcp/how-to-config.md`（YAML 配置 + MCP_AUTH_KEY +
`enable_ingest: true`——82 已开，确认即可）。

升级后验证：MCP `/mcp` 起得来，tools/list 里 5 个 wiki 写工具
（`wiki_create_page` 等）在场。

## 4. 部署 skill

从本仓库 `skills/weknora-wiki-ingest/` 整体拷贝（或 symlink）到 agent 侧 skills
目录（multica agent 能发现的位置）。

**配套版本**：skill 依赖 MCP v0.3.0 的 5 个 wiki 写工具——先升 MCP 再上 skill，
别把 skill 配在旧镜像上。

## 5. 端到端验证（按序执行）

1. **单文档生成**：给一篇测试文档跑 `/weknora-wiki-ingest` → 产出
   summary 页 + entity/concept 页、aliases、页内 `[[内链]]`、source_refs 指向该
   文档。
2. **重名合并**：再灌一篇含同名实体的不同文档，跑 skill → 命中既有页走合
   并（页面内容增长），不新建重复页。
3. **收缩**：删除一篇文档后重灌另一篇（或对已删文档的 KB 变化跑 skill）→
   该文档独撑的页面收缩/删除。
4. **index**：`wiki_index` 的 intro 反映最新文档集（新文档进 intro、失效引用
   消失），且 intro 无 `## 目录` 式段落。

任何一步异常：停跑 skill，收集 `wiki_*` 工具返回的 error_code + message。

## 6. 操作参数

82 上的配置值（API key、KB id、模型 id 等）在 82 上直接改；本仓库文档/示例不
使用真实值，本地占位副本不回写覆盖 82。
