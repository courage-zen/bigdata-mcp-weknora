# weknora-mcp YAML 配置迁移设计

**日期**：2026-11-19
**状态**：已与用户对齐（对话中逐节确认）
**目标版本**：v0.2.0（破坏性配置变更）

## 背景与动机

v0.1.x 用 env vars 配置 workspace→KB 映射：`WEKNORA_WORKSPACE_MAP="ws-uuid:kb-uuid,..."`
加每映射 `WEKNORA_WORKSPACE_<ws-uuid>_API_KEY`。三个问题：

1. **不可读**：UUID 拼进变量名，多工作区时配置面目全非；yaml 层次结构天生表达这个模型
2. **无全局默认 key**：单租户部署（一个 MCP 实例服务一个 WeKnora 租户，每 workspace
   对应租户内一个 KB）时逐映射 key 是纯开销——同租户 KB 本来就在同一信任域，MCP
   层的 workspace→KB 路由才是真正的隔离边界，不依赖 key scope
3. **workspace 匹配只支持精确全 UUID**：multica 传全 UUID，但配置惯例（pg-mcp
   `ALLOWED_WORKSPACE_IDS`）用前 8 位短前缀；待办已久

**破坏性变更决策**：不保留旧 env 解析路径。唯一部署方是自己的 82 集群，公开 repo
发布仅一天无存量用户；双解析路径 = 双倍测试维护，干净切换。

## 配置模型

### 文件结构与边界

`WEKNORA_CONFIG` 环境变量指向配置文件（空 = `./config.yaml`；文件缺失 = 启动错误，
无任何 env 兜底）。

```yaml
weknora:
  base_url: http://your-weknora-host:28444   # 必填，自动去尾斜杠
  api_key: sk-...                             # 全局默认 key（可选，见解析规则）
  enable_ingest: false                        # 实例级写入工具开关，默认 false

  workspaces:                                 # ≥1 条，必填
    - id: 11111111-2222-3333-4444-555555555555  # 全 UUID 或短前缀（如 11111111）均可
      kb_id: 99999999-8888-7777-6666-555555555555  # 必填
      # api_key: 省略 → 继承全局；映射跨租户 KB 时才需要覆盖
      # allowed_agent_ids: [agent-uuid-prefix]     # 可选，前缀匹配（语义不变）
```

**yaml 只管 weknora 层**；以下继续走 env（对齐 email-mcp 家族边界）：
`MCP_AUTH_KEY`、`MCP_HOST`、`MCP_PORT`、S3 系列（`S3_BUCKET`/`AWS_*` 等，
add_document 的 s3_key 源用）。

### 解析与校验规则（fail-fast）

1. **key 解析**：workspace 生效 key = 自身 `api_key`，缺省继承全局 `weknora.api_key`。
   **两者皆无 → 启动错误**，一次性列出所有缺 key 的 workspace id（沿用"一轮修完"错误
   风格）。注：全局 key 也缺 + 无 workspaces → 直接报 workspaces 错误（校验顺序：
   结构错误优先于 key 错误）
2. **id 匹配**（`MappingFor` 重写）：
   - 先精确匹配（配置 id == X-Multica-Workspace-Id header 值）
   - 无精确命中 → 收集"配置 id 是 header 值的前缀"的候选，**最长前缀胜出**（确定性；
     配置 `11111111` 匹配全 UUID header；配置全 UUID 等价精确匹配——两种写法都合法）
3. **载入校验**（各自明确报错，错误信息含文件路径与条目定位）：
   - `base_url` 空
   - `workspaces` 空列表
   - 条目 `id` 或 `kb_id` 空
   - `id` 重复（含"全 UUID 与其前缀同时出现"以外的任意重复——短前缀与全 UUID 并存
     是合法的不同粒度，精确匹配自然优先，无需特判）
   - 同一 `kb_id` 映射到多个 workspace（保留 1:1 约束：workspace 隔离语义的根基，
     共享 KB 等真有需求再放）
   - yaml 语法错误（`parse <path>: <err>`）
4. **`enable_ingest`** 保持实例级。MCP 工具注册是全局的（tools/list 对所有 workspace
   相同），per-workspace 开关需要按调用方过滤工具列表/调用拒绝，另议（WeKnora key
   的 capability 已提供后端层逐工作区写权限，全局开 ingest 时可用它兜底）

### 代码结构

- `Config.Mappings map[string]*WorkspaceMapping` **形状不变**（key = 配置 id），
  `WorkspaceMapping` 字段不变（WorkspaceID/KBID/APIKey/AllowedAgentIDs）——只重写
  `LoadConfig`（env 解析 → yaml 解析 + env 定位文件）和 `MappingFor`（加前缀逻辑）
- `tools.go`、health、启动日志、S3 加载、bearer 认证全部零改动
- 新依赖 `gopkg.in/yaml.v3`（email-mcp 同款）
- `parseWorkspaceMap`/`splitCSV`（config.go 内）删除；`AgentAllowed` 不动

### k8s 布局（82 集群）

- **configmap** `weknora-mcp-config`：只剩 `WEKNORA_CONFIG=/app/config/config.yaml`
  + `MCP_HOST`/`MCP_PORT`
- **secret** `weknora-mcp-secret`：`MCP_AUTH_KEY`（env）+ 新增 `config.yaml` 文件键
  （volume 挂载 `/app/config`；对齐 email-mcp 的 config-file 挂载方式，只是来源从
  ConfigMap 换 Secret——文件里有 `sk-` api_key，敏感）
- deployment：加 volume/volumeMounts（email-mcp deployment 同款）

## 测试

- `config_test.go` 重写：临时目录写 yaml fixture。用例：
  - 继承：workspace 无 key + 全局 key → 生效 key = 全局
  - 覆盖：workspace 有 key → 生效 key = 自身
  - 缺 key：全局无 + workspace 无 → 报错列出 workspace id
  - 结构校验：base_url 空 / workspaces 空 / 条目字段空 / id 重复 / kb 重复 / 坏 yaml
  - `MappingFor` 前缀匹配：精确优先、最长前缀胜出、无命中 false
  - enable_ingest 默认 false / true 解析
- `tools_test.go`/`server_test.go`：`testEnv` 构造的 Mappings 是 Go 结构体直填，
  预期零改动或极小改动（只有 `MappingFor` 行为变化相关用例需核对）
- 全量 `go test ./...` 通过（当前 83 个测试为基线）

## 发布

1. 代码 + 测试 + README（配置章节重写：env 表拆"配置文件"+"环境变量"两节，docker
   run 示例从 `-e` 改挂文件）
2. tag v0.2.0 → GHCR release（现有 CI 链路）
3. 镜像同步 <internal-registry>/eda-bigdata/weknora-mcp:v0.2.0-arm64
4. 82 部署清单更新（`.k8s/` 本地副本 + 82 `~/config/weknora-mcp/`）→ 滚动 →
   集群内端到端验证（health/tools-list/list_knowledge_bases/前缀 header 路由）
5. how-to-config.md 同步重写

## 非目标（明确不做）

- env 兼容层（无存量用户，干净切换）
- per-workspace enable_ingest（另议）
- 多 WeKnora 实例（per-workspace base_url；单实例 YAGNI）
- 共享 KB（1:1 约束保留）
