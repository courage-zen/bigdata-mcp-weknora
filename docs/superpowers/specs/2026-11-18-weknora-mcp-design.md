# bigdata-mcp-weknora 设计文档

**日期**：2026-11-18
**状态**：已确认（与用户逐项过完决策）

## 背景与目标

为 multica 中的 agent 提供操作 WeKnora 知识库（KG）能力的 MCP 服务。调用链路：

```
multica agent (Claude Code)
  └─ multica-stdio-mcp-proxy (stdio, 注入 X-Multica-* 身份 headers)
       └─ HTTP POST → weknora-mcp :8000/mcp
            ├─ Bearer 认证 (MCP_AUTH_KEY)
            ├─ 提取 X-Multica-Workspace-Id → 查映射表 → (kb_id, api_key)
            ├─ agent 白名单校验（可选，前缀匹配）
            └─ tools → WeKnora 0.9.0 REST API (X-API-Key 按映射携带)
```

核心模型：**每个 multica 工作区对应一个 WeKnora 知识库（KG）**。服务不依赖 WeKnora
内置 MCP endpoint（那是进程内 service 直调 + 端点白名单机制），而是基于 WeKnora
REST API 独立实现，能力等价（调研文档 `WeKnora-0.9.0/docs/instance-api-mcp-exploration.md`
已验证映射关系）。

服务无状态：所有配置启动时加载进内存，无 DB、无文件持久化、无 token 刷新逻辑。

## 关键决策记录（用户确认）

| 决策点 | 选定方案 |
| --- | --- |
| KB 映射 | 静态映射 + 自动路由（工具不带 KB 参数） |
| WeKnora 凭证 | **每映射强制独立 API Key**（启动校验，缺失报错） |
| 工具范围 | 只读检索 + wiki + ingest 写入；**ask 不做** |
| S3 角色 | add_document 支持 s3_key 源（复用 email-mcp S3 标准） |
| ingest 启用 | 部署级开关 `WEKNORA_ENABLE_INGEST`（默认 false，关闭时不注册写入工具） |
| 调用约束 | 强制 workspace 路由（无 header/未映射 → 明确错误） |
| 超时策略 | 短超时（30s）+ 异步提交（add_document 提交即返回 document_id，不轮询索引进度） |
| add_document 源 | 三源：content（Markdown）/ url / s3_key（拉取后按后缀分流） |
| 工具命名 | 对齐 WeKnora 官方 MCP 命名 |
| 项目配套 | CI/CD + 部署、agent 白名单、文档；git committer 用 GitHub 账号 |

## WeKnora API Key 模型（源码确认）

`internal/types/tenant_api_key.go`：API Key 绑定 **WeKnora 租户**（tenant），非用户个人：

- `TenantID`：key 所属租户；平台级 key（`tenant_id=NULL`）须带 `X-Tenant-ID` 选目标租户
- `KnowledgeBaseIDs`：key 可限定 KB 范围（空 = 租户内全部 KB）
- `Capabilities`：`retrieve`（读）/ `ingest`（写）/ `chat` 等
- tenant-scoped key 的租户由 key 自带；带 `X-Tenant-ID` 且与 key 不符 → 403
  （`internal/middleware/auth.go:429-470`）

因此**我们的映射不需要租户字段**：`ws_id:kb_id` 二元组 + 每映射独立 key。想要强
隔离时，在 WeKnora 侧把 key 的 `knowledge_base_ids` 限定为映射的单个 KB——路由错误
的调用会被 WeKnora 403 掉（双保险）。

工具对 API Key capability 的要求（映射源码路由组）：

- 读类（list/search/read/wiki）：key 需 `retrieve` capability（或 full_access）
- 写类（add/update/delete）：key 需 `ingest` capability（或 full_access）
- 部署建议：映射用的 key 配 `retrieve`+`ingest`，KB 范围限定到映射的单 KB

## 配置

全部 env（对齐 email-mcp/pg-mcp 的惯例；email-mcp 的 YAML config 在本项目不需要——
没有 SMTP 这类多字段嵌套配置，纯 env 足够）：

### 服务

```
MCP_AUTH_KEY=key1,key2        # 逗号分隔多个 Bearer key；空 = 无认证（本地开发）
WEKNORA_BASE_URL=http://your-weknora-host:28444   # 必填，无默认
MCP_HOST=0.0.0.0              # 默认 0.0.0.0
MCP_PORT=8000                 # 默认 8000
WEKNORA_ENABLE_INGEST=false   # 默认 false；true 时注册写入三工具
```

### 工作区映射（核心）

```
WEKNORA_WORKSPACE_MAP="ws-uuid-1:kb-uuid-1,ws-uuid-2:kb-uuid-2"
WEKNORA_WORKSPACE_ws-uuid-1_API_KEY=wk-aaa     # 每映射必填
WEKNORA_WORKSPACE_ws-uuid-2_API_KEY=wk-bbb
# 可选 agent 白名单（逗号分隔，前缀匹配，pg-mcp 模式）
WEKNORA_WORKSPACE_ws-uuid-1_ALLOWED_AGENT_IDS=agent-uuid-prefix
```

启动校验（失败即退出）：

- `WEKNORA_BASE_URL` 非空
- `WEKNORA_WORKSPACE_MAP` 非空（至少一条映射）
- 每条映射的 workspace id、kb id 均非空且不重复（ws 重复或 kb 重复都报错——
  1:1 模型下重复即配置错误）
- 每条映射有对应 `WEKNORA_WORKSPACE_<ws>_API_KEY`，缺失报错并列出所有缺失项

注：env 变量名含 UUID（带连字符），Go `os.Getenv` 纯字符串拼接，无兼容问题。

### S3（env-only，email-mcp S3 使用标准）

三件套 `S3_BUCKET` / `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`（缺任一 → S3 软禁用，
`s3_key` 源返回 `S3_UNAVAILABLE`，服务正常启动）；`AWS_ENDPOINT_URL`（实际必需）、
`S3_REGION`→`AWS_REGION`、`S3_USE_PATH_STYLE`、`S3_KEY_PREFIXES`、`S3_MAX_FILE_BYTES`
（默认 100 MiB）。

## 工具目录（10 个）

命名与语义对齐 WeKnora 官方 MCP（`internal/mcpserver/`），便于 agent 认知迁移。
差异：参数去掉 KB 选择（服务端按 workspace 自动路由）；输出统一附 `multica_context`
回显（email-mcp 惯例）；`grep_chunks` 与 `ask` 不做（前者无 REST 等价，后者用户未选）。

### 只读 7 个（始终注册）

| 工具 | WeKnora REST | 参数 |
| --- | --- | --- |
| `list_knowledge_bases` | `GET /api/v1/knowledge-bases` | 无 |
| `search_knowledge` | `POST /api/v1/knowledge-search` | `query`*、`mode`(hybrid/semantic/keyword，默认 hybrid)、`limit`(默认 10，上限 30)、`knowledge_ids`(可选数组，限定文档) |
| `list_documents` | `GET /api/v1/knowledge-bases/{kb}/knowledge` | `keyword`、`page`、`page_size`(默认 20，上限 100)、`parse_status`、`file_type`、`sort_by`、`sort_order` |
| `read_document` | `GET /api/v1/chunks/{knowledge_id}` | `knowledge_id`*、`page`、`page_size`(默认 10，上限 100)、`chunk_type`(可选数组，默认 text) |
| `wiki_search` | `GET /api/v1/knowledgebase/{kb}/wiki/search?q=` | `query`*、`regex`(bool，省略自动判断)、`limit`(默认 10，上限 50) |
| `wiki_read_page` | `GET /api/v1/knowledgebase/{kb}/wiki/pages/{slug}` | `slug`* |
| `wiki_index` | `GET /api/v1/knowledgebase/{kb}/wiki/index` | `limit`(默认 50，上限 200) |

wiki 路由前缀注意：`/knowledgebase/`（单数、无连字符），与 `/knowledge-bases/`（管理组）不同。

`search_knowledge` 模式映射到 WeKnora `SearchKnowledgeRequest` 字段：

- hybrid（默认）：不传任何 disable 字段
- semantic：`disable_keywords_match: true`
- keyword：`disable_vector_match: true`

`wiki_read_page` 增强（对齐官方内联行为）：取页面后对 `out_links` 逐个 GET 内联其
`summary` 字段（官方 `wiki_tools.go` 行为；调研文档指出的自建差距）。上限 10 个出链
防止响应失控，超出的标记 `summary_omitted`。单页处理最多 11 次 WeKnora 请求。

### 写入 3 个（`WEKNORA_ENABLE_INGEST=true` 时注册）

| 工具 | WeKnora REST | 参数 |
| --- | --- | --- |
| `add_document` | 三源分流（见下） | `title`*、`content`\|`url`\|`s3_key`（互斥，恰好其一）、`publish`(默认 true) |
| `update_document` | `PUT /api/v1/knowledge/manual/{id}` | `knowledge_id`*、`content`*、`title`、`publish` |
| `delete_document` | `DELETE /api/v1/knowledge/{id}` | `knowledge_id`* |

**add_document 三源分流**：

1. `content` → `POST /api/v1/knowledge-bases/{kb}/manual`
   body `{title, content, status: "publish"|"draft", channel: "mcp"}`
2. `url` → `POST /api/v1/knowledge-bases/{kb}/url`
   body `{url, title, channel: "mcp"}`（WeKnora 侧做 SSRF 校验后异步抓取解析）
3. `s3_key` → 拉 S3 对象（key 验证 / 前缀白名单 / 双层大小 cap / 分层超时，全套
   复用 email-mcp `s3.go` + `s3keys.go`）→ 按对象名后缀分流：
   - `.md`（或 `.markdown`）→ 读出文本走 `/manual`（title 缺省用对象名去后缀）
   - 其他后缀 → multipart `POST /api/v1/knowledge-bases/{kb}/file`
     （`file` 字段 + `fileName` + `channel=mcp`；WeKnora 侧上限默认 50 MB，
     `S3_MAX_FILE_BYTES` 应配 ≤ 50 MB 防止提交后被拒）

所有入库均为**异步提交即返回**：响应携带 WeKnora 返回的 `knowledge_id`（document id）
与解析状态初始值，不等待索引完成（与官方 MCP add_document 行为一致）。

## 调用约束与错误处理

### 路由与访问控制（每个工具调用先过）

1. Bearer 认证（`MCP_AUTH_KEY`；`/health` 免认证）
2. `X-Multica-Workspace-Id` 必须存在且在映射表 → 否则 `WORKSPACE_NOT_MAPPED`
   （错误信息列出当前已映射的 workspace 数量与配置方法，不泄露 kb id / key）
3. 可选 agent 白名单：`WEKNORA_WORKSPACE_<ws>_ALLOWED_AGENT_IDS` 配置时，
   `X-Multica-Agent-Id` 前缀匹配任一条目才放行 → 否则 `ACCESS_DENIED`
   （pg-mcp 前缀匹配语义：配置可用短 UUID 前缀）

### 错误码（`error` 人读 + `error_code` 机器读，email-mcp 惯例）

| error_code | 场景 |
| --- | --- |
| `WORKSPACE_NOT_MAPPED` | 无 workspace header 或不在映射表 |
| `ACCESS_DENIED` | agent 不在白名单 |
| `MISSING_PARAM` / `INVALID_PARAM_TYPE` / `INVALID_PARAM_VALUE` | 参数校验 |
| `WEKNORA_API_FAILED` | WeKnora 返回 4xx/5xx（透传其 message） |
| `WEKNORA_TIMEOUT` | WeKnora 调用超时（30s） |
| `S3_UNAVAILABLE` / `S3_INVALID_KEY` / `S3_OBJECT_NOT_FOUND` / `S3_OBJECT_TOO_LARGE` / `S3_FETCH_FAILED` | S3 源（复用 email-mcp 语义） |
| `S3_FILE_TOO_LARGE_FOR_WEKNORA` | S3 拉取成功但超过 WeKnora 50MB 上限（提前校验，不浪费上传） |
| `INTERNAL` | 兜底 |

WeKnora 错误透传：4xx/5xx 响应体（`{"error":{"message":...}}` 或 `{"error":"..."}`）
提炼 message 进 `error` 字段，附 HTTP 状态码；不吞掉、不改写。

### 超时

- WeKnora API 调用：30s（所有工具统一；无长阻塞路径，ask 不做所以不需要 4 分钟级）
- S3 拉取：email-mcp 分层超时（10s connect / 60s response-header / 60s 整体）

## 项目结构（对齐 email-mcp）

```
bigdata-mcp-weknora/
  main.go          入口：加载配置、启动 HTTP 服务
  config.go        env 解析 + 映射表 + 启动校验
  middleware.go    Bearer 认证（复用 email-mcp bearerAuth，/health 免认证）
  identity.go      X-Multica-* 提取（复用 email-mcp 8-header 映射）
  weknora.go       WeKnora API 客户端（per-mapping key、30s 超时、错误透传、wiki 出链内联）
  tools.go         10 工具注册 + 参数 schema + handler 逻辑
  server.go        MCP server 装配（stateless streamable HTTP，复用 email-mcp 模式）
  s3.go            S3 ObjectStore（复用 email-mcp 实现：minio-go、Stat 预检、流式 cap）
  s3keys.go        S3 key 验证（复用 email-mcp：遍历/危险后缀/前缀白名单/URL 解析）
  各 *_test.go     平铺单测
  Dockerfile / Makefile
  .github/workflows/{build,release}.yml
  docs/superpowers/{specs,plans}/
  README.md        中文，含 multica 接入配置示例
```

依赖：`modelcontextprotocol/go-sdk`、`minio-go/v7`（与 email-mcp 同版本）。

## 测试策略

不依赖真实 WeKnora 实例（82 集群实例用于联调，不进单测）：

- `config_test.go`：env 解析、映射表校验（缺失 key、重复 ws/kb、空 map）、ingest 开关
- `identity_test.go`：header 提取（复用 email-mcp 测试思路）
- `tools_test.go`：每个工具的参数校验、错误码、路由（mock WeKnoraClient）
- `weknora_test.go`：httptest 假 WeKnora——各端点请求形状（header/body/路径）、
  错误透传、超时、wiki 出链内联（含 >10 出链截断）
- `s3_test.go` / `s3keys_test.go`：复用 email-mcp 的测试（按需适配 s3→weknora 分流）
- `server_test.go`：端到端 MCP 调用（httptest 起 server，模拟 X-Multica headers）

## 部署

- Docker 镜像（GHCR `ghcr.io/courage-zen/bigdata-mcp-weknora`）+ linux amd64/arm64
  静态二进制 + air-gapped 镜像 tar（对齐 email-mcp release 流程）
- k8s：82 集群 `eda-bigdata` namespace（调研文档验证过集群内可达 WeKnora，
  延迟 2~3ms 同机房）
- Git：repo-local `user.email = 5386698+courage-zen@users.noreply.github.com`
  （GitHub noreply；不用 chinatelecom.cn），remote
  `git@github.com:courage-zen/bigdata-mcp-weknora.git`

## 多工作区部署的 multica 侧配置

工作区 MCP 库条目（multica `ResolveAgentMcpConfig` 会把绑定的 workspace MCP 合入
agent 的 mcp_config），经 stdio proxy 接入：

```json
{
  "mcpServers": {
    "weknora-mcp": {
      "command": "multica-stdio-mcp-proxy",
      "args": [
        "--url", "http://weknora-mcp-host:8000/mcp",
        "--token", "<upstream-bearer-key>",
        "--allow-http"
      ]
    }
  }
}
```

一个 weknora-mcp 实例服务多个 multica 工作区（映射表多条）；各工作区的 agent
看到同一组工具，数据天然按 workspace 隔离。
