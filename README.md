# weknora-mcp

MCP Server，让 multica agent 通过 WeKnora 0.9.0 REST API 操作本工作区对应的知识库（KG）。
每个 multica 工作区映射一个 WeKnora 知识库，调用自动路由。

```
multica agent → multica-stdio-mcp-proxy → weknora-mcp :8000/mcp → WeKnora REST API
```

- Streamable HTTP transport（stateless、JSON response），Bearer 认证（MCP_AUTH_KEY）
- 工作区路由：X-Multica-Workspace-Id → WEKNORA_WORKSPACE_MAP → (kb_id, 每工作区独立 API Key)
- 10 个工具：检索 4（list_knowledge_bases / search_knowledge / list_documents /
  read_document）+ wiki 3（wiki_search / wiki_read_page / wiki_index）+
  写入 3（add_document / update_document / delete_document，默认关闭）
- add_document 三源：Markdown 文本 / URL / s3_key（S3 对象按后缀分流）
- 结果统一携带 multica_context 回显；错误带 error + error_code

## 配置

| 环境变量 | 必填 | 说明 |
| --- | --- | --- |
| WEKNORA_BASE_URL | 是 | WeKnora 实例地址 |
| WEKNORA_WORKSPACE_MAP | 是 | "ws-id:kb-id" 逗号分隔 |
| WEKNORA_WORKSPACE_<ws-id>_API_KEY | 是 | 每映射独立 WeKnora API Key |
| WEKNORA_WORKSPACE_<ws-id>_ALLOWED_AGENT_IDS | 否 | agent 白名单（前缀匹配） |
| WEKNORA_ENABLE_INGEST | 否 | true 开启写入工具，默认 false |
| MCP_AUTH_KEY | 否 | Bearer key，逗号分隔；空 = 无认证 |
| MCP_HOST / MCP_PORT | 否 | 默认 0.0.0.0:8000 |

S3（env-only，add_document 的 s3_key 源）：S3_BUCKET / AWS_ACCESS_KEY_ID /
AWS_SECRET_ACCESS_KEY（三缺一 = 软禁用）+ AWS_ENDPOINT_URL / S3_REGION /
S3_USE_PATH_STYLE / S3_KEY_PREFIXES / S3_MAX_FILE_BYTES。

## multica 接入

工作区 MCP 库条目（经 multica-stdio-mcp-proxy stdio 接入；一个 weknora-mcp 实例
服务多个工作区，数据按 workspace 天然隔离）：

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

## 工具说明

工具命名对齐 WeKnora 官方 MCP，便于 agent 认知迁移；差异：参数去掉 KB 选择
（服务端按 workspace 自动路由），输出统一附 multica_context 回显。
`grep_chunks`（无 REST 等价）与 `ask`（走 LLM）不提供。

### 只读 7 个（始终注册）

| 工具 | 参数 | 说明 |
| --- | --- | --- |
| `list_knowledge_bases` | 无 | 列出本映射 key 可见的知识库 |
| `search_knowledge` | `query`*、`mode`（hybrid/semantic/keyword，默认 hybrid）、`limit`（默认 10，上限 30）、`knowledge_ids`（可选数组，限定文档） | 语义/关键词/混合检索，返回相关段落 |
| `list_documents` | `keyword`、`page`、`page_size`（默认 20，上限 100）、`parse_status`、`file_type`、`sort_by`、`sort_order` | 分页列文档 |
| `read_document` | `knowledge_id`*、`page`、`page_size`（默认 10，上限 100）、`chunk_type`（可选数组，默认 text） | 按阅读顺序读文档分块 |
| `wiki_search` | `query`*、`regex`（bool，省略自动判断）、`limit`（默认 10，上限 50） | 按标题/别名/内容搜索 wiki 页面 |
| `wiki_read_page` | `slug`* | 读整页 Markdown 与元数据；出链自动内联摘要（上限 10 个，超出标 out_links_omitted） |
| `wiki_index` | `limit`（默认 50，上限 200） | wiki 目录：intro + 按类型分组 |

### 写入 3 个（`WEKNORA_ENABLE_INGEST=true` 时注册）

| 工具 | 参数 | 说明 |
| --- | --- | --- |
| `add_document` | `title`*、`content`\|`url`\|`s3_key`（互斥，恰好其一）、`publish`（默认 true） | 三源入库，异步提交即返回 document id；s3_key 按对象后缀分流（.md → 文本，其他 → 文件上传） |
| `update_document` | `knowledge_id`*、`content`*、`title`、`publish` | 替换 Markdown 文档内容/标题 |
| `delete_document` | `knowledge_id`* | 永久删除文档及索引数据（异步任务） |

### 错误码

| error_code | 场景 |
| --- | --- |
| `WORKSPACE_NOT_MAPPED` | 无 workspace header 或不在映射表 |
| `ACCESS_DENIED` | agent 不在白名单 |
| `MISSING_PARAM` / `INVALID_PARAM_TYPE` / `INVALID_PARAM_VALUE` | 参数校验 |
| `WEKNORA_API_FAILED` | WeKnora 返回 4xx/5xx（透传其 message） |
| `WEKNORA_TIMEOUT` | WeKnora 调用超时（30s） |
| `S3_UNAVAILABLE` / `S3_INVALID_KEY` / `S3_OBJECT_NOT_FOUND` / `S3_OBJECT_TOO_LARGE` / `S3_FETCH_FAILED` | S3 源 |
| `S3_FILE_TOO_LARGE_FOR_WEKNORA` | S3 对象超过 WeKnora 50MB 上限 |
| `INTERNAL` | 兜底（当前仅 marshal 失败；传输错误归入 WEKNORA_API_FAILED） |

## 部署

```bash
docker run -d -p 8000:8000 \
  -e WEKNORA_BASE_URL=http://your-weknora-host:port \
  -e WEKNORA_WORKSPACE_MAP="ws-uuid-1:kb-uuid-1" \
  -e WEKNORA_WORKSPACE_ws-uuid-1_API_KEY=wk-xxx \
  -e MCP_AUTH_KEY=bearer-key \
  ghcr.io/courage-zen/bigdata-mcp-weknora:v0.1.0
```

镜像：GHCR 多架构（amd64/arm64）；Release 附 linux 静态二进制与 air-gapped
镜像 tar（含 checksums）。
