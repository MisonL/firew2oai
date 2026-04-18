# firew2oai

Fireworks.ai Chat API → OpenAI Chat Completions 文本子集转换代理

将 [Fireworks.ai](https://chat.fireworks.ai) 的网页聊天接口转换为 **OpenAI Chat Completions 文本子集** 格式，
可接入 [New API](https://github.com/Calcium-Ion/new-api)、[One API](https://github.com/songquanpeng/one-api) 等兼容 OpenAI Chat 接口的聚合网关。

## 特性

- **标准 OpenAI 兼容** — `/v1/chat/completions`、`/v1/responses` 和 `/v1/models` 接口，即插即用
- **流式 + 非流式** — 支持 Chat Completions / Responses 文本子集的 SSE streaming 和非流式响应
- **Codex 协议适配** — 已补齐 Responses 原始 item 历史、`previous_response_id`、工具回灌、Chat content blocks、Chat `tool_calls`、`usage` 估算、工具协议校验与尾部控制块解析；但实测仍不具备复杂 Agent 任务可用性
- **Thinking 模型** — 自动处理思考过程，可配置显示/隐藏（`show_thinking`）
- **Chrome TLS 指纹** — 模拟 Chrome 的 JA3 指纹，包括 TLS 1.3 密码套件顺序、曲线偏好
- **完整 HTTP 伪装** — `sec-ch-ua` Client Hints、`sec-fetch-*`、`Accept-Language`、`Origin`/`Referer` 等全量 Chrome 浏览器请求头
- **三大平台** — 支持 macOS (amd64/arm64)、Linux (amd64/arm64)、Windows (amd64) 命令行启动
- **Docker 部署** — 多阶段构建，最终镜像基于 `alpine`，体积约 15MB
- **优雅关停** — SIGINT/SIGTERM 信号处理，等待进行中的请求完成
- **健康检查** — `/health` 端点，适合 K8s/Docker 健康探针
- **可观测性指标** — `/metrics` Prometheus 文本格式，含请求量/状态码/时延/并发/goroutines
- **CORS 支持** — 可配置允许的跨域来源（默认 `*`，生产环境建议限定）
- **多 Key 认证** — 支持多 API Key，每个 Key 可独立配置额度和速率限制
- **Per-Key 额度限制** — 限制每个 Key 的总请求数，耗尽返回 403 + `X-Quota-*` 响应头
- **Per-Key 速率限制** — 令牌桶限流，标准 `X-RateLimit-*` 响应头，超出返回 429
- **IP 白名单** — 默认仅允许 `127.0.0.1, ::1` 环回地址访问，支持 CIDR 段，空值放行全部
- **Panic 恢复** — 内置 Recovery 中间件，不会因 panic 崩溃
- **IP 白名单安全** — 默认不信任任何代理头（`TrustedProxyCount=0`），防止 X-Forwarded-For 伪造绕过白名单

## 快速开始

### 从源码编译

```bash
# 克隆项目
git clone https://github.com/mison/firew2oai.git
cd firew2oai

# 编译
make build

# 运行（默认端口 39527）
./bin/firew2oai

# 查看所有选项
./bin/firew2oai --help
```

### 命令行参数

```bash
./bin/firew2oai -port 9000 -api-key your-secret-key -log-level debug -timeout 180
```

| 参数 | 环境变量 | 默认值 | 说明 |
|---|---|---|---|
| `-port` | `PORT` | `39527` | 监听端口（`1-65535`） |
| `-host` | `HOST` | `""` (所有接口) | 监听地址 |
| `-api-key` | `API_KEY` | `sk-admin` | API Key（支持多 Key、JSON 文件，详见下方） |
| `-timeout` | `TIMEOUT` | `120` | 上游请求超时（秒） |
| `-log-level` | `LOG_LEVEL` | `info` | 日志级别 (debug/info/warn/error) |
| `-show-thinking` | `SHOW_THINKING` | `false` | 显示 thinking 模型的思考过程 |
| `-cors-origins` | `CORS_ORIGINS` | `*` | 允许的跨域来源（逗号分隔，`*` 表示全部） |
| `-rate-limit` | `RATE_LIMIT` | `0`（禁用） | 全局每 Key 每分钟最大请求数（0 禁用，per-key 配置可覆盖） |
| `-ip-whitelist` | `IP_WHITELIST` | `127.0.0.1,::1` | 允许的 IP/CIDR（逗号分隔，空值放行全部） |
| `-trusted-proxy-count` | `TRUSTED_PROXY_COUNT` | `0` | 信任的反向代理数量（0 = 不信任代理头，仅用 RemoteAddr） |

### 多 Key 与额度/速率配置

`-api-key` 支持四种格式：

**1. 单 Key（默认，向后兼容）：**
```bash
./bin/firew2oai -api-key sk-admin
```

**2. 多 Key（逗号分隔）：**
```bash
./bin/firew2oai -api-key "sk-admin,sk-user1,sk-user2"
```

**3. JSON 文件路径（推荐，支持 per-key 额度和速率）：**

```json
// tokens.json
[
  {"key": "sk-admin", "quota": 0, "rate_limit": 0},
  {"key": "sk-user1", "quota": 1000, "rate_limit": 60},
  {"key": "sk-user2", "quota": 100, "rate_limit": 10}
]
```

```bash
./bin/firew2oai -api-key /path/to/tokens.json -rate-limit 30
```

**4. 内联 JSON：**
```bash
./bin/firew2oai -api-key '[{"key":"sk-admin"},{"key":"sk-user","quota":500,"rate_limit":20}]'
```

**参数说明：**
| 字段 | 说明 |
|---|---|
| `key` | API Key 字符串 |
| `quota` | 总请求额度（0 = 无限） |
| `rate_limit` | 每分钟最大请求数（0 = 使用全局 `-rate-limit` 值） |

**响应头：**
- `X-Quota-Limit` / `X-Quota-Remaining` — 额度信息（有 quota 时）
- `X-RateLimit-Limit` / `X-RateLimit-Remaining` / `X-RateLimit-Reset` — 速率信息（有 rate_limit 时）
- 额度耗尽 → 403 Forbidden
- 速率超限 → 429 Too Many Requests

### Docker 部署

```bash
# 使用 docker compose
docker compose up -d

# 或者手动构建
docker build -t firew2oai .
docker run -d -p 39527:39527 -e API_KEY=your-secret firew2oai
```

### 多平台构建

```bash
# 构建所有平台
make build-all

# 产物位于 bin/ 目录：
# bin/firew2oai-linux-amd64
# bin/firew2oai-linux-arm64
# bin/firew2oai-darwin-amd64
# bin/firew2oai-darwin-arm64
# bin/firew2oai-windows-amd64.exe
```

## 使用方式

### 方式一：Codex / 客户端直连

客户端直接连接到 firew2oai，适合本地开发或单用户场景。

```bash
# 启动 firew2oai
./bin/firew2oai -api-key sk-admin -ip-whitelist ""

# 客户端配置
Base URL: http://localhost:39527
API Key: sk-admin
```

Codex `config.toml` 示例：

```toml
model = "deepseek-v3p2"
model_provider = "firew2oai"

[model_providers.firew2oai]
name = "firew2oai"
base_url = "http://127.0.0.1:39527/v1"
experimental_bearer_token = "sk-admin"
wire_api = "responses"
```

### 方式二：New API / One API 中转

通过 API 聚合网关中转，适合多用户、多模型管理场景。

1. 部署 firew2oai（例如端口 39527）
2. 在 New API / One API 中新增渠道：
   - 类型：**OpenAI**
   - 地址：`http://your-host:39527`
   - 密钥：`sk-admin`（或你自定义的 `-api-key`）
3. 在 New API 中添加模型映射，选择上述渠道
4. 若同名模型存在多个渠道，将 firew2oai 渠道优先级调高，并确认渠道处于启用状态

**Docker 网络注意事项**：
- 如果 New API 和 firew2oai 都在 Docker 中，使用 `host.docker.internal:39527` 或容器网络
- 确保 firew2oai 的 `-ip-whitelist` 允许网关 IP 访问

Codex 经 New API 中转时，Codex 的 `base_url` 应指向 New API 的 `/v1` 地址，`wire_api` 仍使用 `responses`。New API 渠道侧保持 OpenAI 兼容渠道，渠道地址指向 firew2oai。当前生产复核中，`firew2oai-local` 为 `channel_id=106`、`priority=100`、`status=1`，最新 `kimi-k2p5` 复测确实命中该渠道。

### Codex 适配状态

代码与测试核对日期：2026-04-18。端到端复测记录见 `docs/reviews/CR-CODEX-E2E-2026-04-17.md`，模型矩阵见 `docs/reviews/CR-CODEX-MODEL-MATRIX-2026-04-17.md`。

| 场景 | 直连 firew2oai | New API 中转 | 说明 |
|---|---|---|---|
| 最小文本任务 | 通过 | 通过 | `只回答 ok` 返回 `ok` |
| Responses 流闭合 | 通过 | 通过 | 已发送 `response.created` / `response.completed` 包装事件 |
| 多轮会话恢复 | 协议级通过 | 协议级通过 | `previous_response_id` 现基于原始 Responses items 恢复，而不是仅拼接聊天文本 |
| 工具调用回灌 | 协议级通过 | 协议级通过 | 支持保存 `function_call` / `custom_tool_call` 及下一轮 `*_output` 输入项 |
| `usage` 展示 | 通过 | 通过 | `response.completed.response.usage` 现返回本地估算 token，便于 New API 展示与计费 |
| 真实复杂任务 | 不通过 | 不通过 | 最新 6 模型复测已进入真实 `command_execution`（`1~6` 次），但仍未稳定完成“双读取 + 收束答案”闭环 |
| `spawn_agent` | 不通过 | 不通过 | 仍依赖稳定工具状态机，当前上游模型在真实任务中无法持续遵守 |
| `kimi-k2p5` 最新复测 | 不通过 | 不通过 | 直连 `iter9`：`cmd_count=3`；中转 `iter4`：`cmd_count=3`；两链路都未完成“读取两处 + 三行收束”闭环 |
| 6 模型中转复测（2026-04-18） | - | 不通过 | 部署最新容器后仍未闭环；`new-api` 侧 `cmd_count` 分布：`1~6`，`partial_tool_then_adapter_error` 从 `3/6` 降到 `0/6`（证据：`/tmp/codex-six-models-20260418-newapi-iter4/summary.tsv`） |

当前只能将 Codex 接入定位为“协议级适配已完成，但不具备生产可用性”。对于读取文件、执行命令、多轮 Agent、`subagent` 等真实复杂任务，不应将 firew2oai 作为默认模型通道，也不应仅凭 HTTP 200 判定成功。

2026-04-18 持续迭代复测（6 模型：`minimax-m2p5`、`kimi-k2p5`、`glm-5`、`glm-4p7`、`deepseek-v3p2`、`deepseek-v3p1`）显示：

- 直连最新 `iter9`：`6/6` 都触发真实 `command_execution`，且 `Codex adapter error` 为 `0/6`（证据：`/tmp/codex-six-models-20260418-iter9-direct/summary.tsv`）。
- 中转最新 `iter4`：`6/6` 也都触发真实 `command_execution`，`partial_tool_then_adapter_error` 从 `iter2` 的 `3/6` 降到 `0/6`（证据：`/tmp/codex-six-models-20260418-newapi-iter4/summary.tsv`）。
- 但 6/6 仍未稳定完成复杂任务闭环，结论仍是“协议级打通，复杂 Agent 任务不可用”。
- 一次中间策略（iter6）会诱发长循环调用，已回退，不纳入稳定版本。
- 通过数据库复核，2026-04-18 的中转复测请求均命中 `channel_id=106`，因此中转失败不是渠道偏移导致。

### Codex 适配要点

- `/v1/chat/completions` 支持 `messages[].content` 为字符串或 text content blocks 数组；`cache_control` 等非文本元数据会被忽略，图片等非文本 block 会显式拒绝。
- Chat Completions 请求中的 `tools` / `tool_choice` 会被转写为文本约束；上游输出合法尾部控制块或旧版单 JSON 工具调用时，代理会转换为 OpenAI Chat `tool_calls`，流式与非流式均覆盖。
- Chat 多轮工具回灌会把 assistant `tool_calls` 与 `role=tool` 的结果整理进上游 prompt，避免只保留空 assistant 消息导致上下文断裂。
- firew2oai 会保存每次 `/v1/responses` 的原始 request items 与历史 items，后续 `previous_response_id` 直接基于 item 图恢复上下文。
- `/v1/responses/{id}/input_items` 返回原始输入项，而不是降级后的聊天文本，便于 Codex 恢复工具链状态。
- 当上游模型输出合法工具调用时，代理会转换为 `function_call` 或 `custom_tool_call` item；若工具名或参数不合法，则显式返回 `Codex adapter error`，避免静默降级。
- `tool_choice: "none"` 会禁用工具模式；有工具时会在 prompt 中追加 `AVAILABLE_TOOLS` 与 `TOOL_CHOICE` 约束。
- `response.completed` 内会包含本地估算的 `usage.input_tokens`、`usage.output_tokens`、`usage.total_tokens`，用于兼容 New API 展示。

### 工具尾部控制块协议

为降低模型正文、多个 JSON 片段与工具状态机混杂导致的解析失败，firew2oai 会优先识别回答末尾的 `AI_ACTIONS_V1` 控制块；未识别到控制块时，仍回退到旧版单 JSON 工具调用解析。

工具调用示例：

```text
<<<AI_ACTIONS_V1>>>
{"mode":"tool","calls":[{"name":"Read","arguments":{"file_path":"README.md"}},{"name":"Bash","arguments":{"command":"go test ./internal/proxy/...","description":"Run proxy tests"}}]}
<<<END_AI_ACTIONS_V1>>>
```

最终回答示例：

```text
最终答案正文。
<<<AI_ACTIONS_V1>>>
{"mode":"final"}
<<<END_AI_ACTIONS_V1>>>
```

当前本地测试已覆盖 Chat 非流式多 `tool_calls`、Responses 流式多 `function_call`、`mode=final` 控制块剥离，以及旧版单 JSON 回退。该机制只是转换层增强，仍需重新复测 Codex / Claude Code 真实复杂任务后才能更新可用性结论。

2026-04-17 最新 Codex 复测已经完成：`kimi-k2p5` 在直连 firew2oai 和经 New API 中转两条链路下都没有提升到可用状态。失败输入不是合法 `AI_ACTIONS_V1`，而是上游连续输出两个顶层旧版 `function_call` JSON，适配器按当前协议显式拒绝并返回 `Codex adapter error`。

后续又补了一项更窄的 legacy 兼容增强：当上游输出的是“纯连续 JSON 对象序列”时，代理现在可以把它解析为多个 legacy 工具调用；如果对象之间夹杂 Markdown、解释文字或尾随正文，仍会显式拒绝。需要注意，这项增强对 Codex 真实任务的提升有限，因为 Codex 当前链路默认 `parallel_tool_calls=false`，一次吐出两个调用时仍会被按协议拒绝。

随后又补了第二轮“安全归一化”优化：

- `run_terminal_cmd`、`run_command` 会归一化为 `exec_command`
- `AI_ACTIONS_V1` 控制块内部允许 fenced JSON
- legacy 路径允许“前缀文本 + 连续多个 JSON 对象”

这轮优化后，`deepseek-v3p2` 已能从第一轮真实命令执行推进到两轮真实命令执行，再在后续轮次偏航；`qwen3-vl-30b-a3b-instruct` 和 `minimax-m2p5` 则稳定推进到明确的“多调用超限”错误。也就是说，附加机制仍有有限优化空间，但当前瓶颈已经主要是上游模型不能长期稳定遵守 Codex 工具协议。

当前实现补充了以下运行时约束：

- 结构化工具调用必须使用 `arguments`，自由格式工具调用必须使用 `input`；字段错位会显式返回 `Codex adapter error`，不再静默补空值。
- `tool_choice: "required"` 不再只停留在提示词层；若请求未声明任何工具，会在入口直接返回 `400 invalid_tool_choice`；若上游最终返回 `mode=final` 或普通文本，会显式返回协议错误。
- 对象形式的 `tool_choice` 会明确要求上游必须输出指定工具的 `AI_ACTIONS` 工具块，而不是条件式提示。
- `tool_choice: "none"` 会从上游 prompt 中移除工具清单和 `AI_ACTIONS` 协议，避免控制块泄露为普通文本。
- `parallel_tool_calls: false` 会限制 `AI_ACTIONS` 中最多一个调用；超过上限会显式返回协议错误。
- Responses 工具流在工具 fallback 到文本消息时，也会补齐 `response.output_item.added`、`response.content_part.added`、`response.output_text.done`、`response.output_item.done` 顺序，与文本流状态机保持一致。

### Codex 可用性结论

- 12 个当前保留模型都能完成最小文本 Responses 请求。
- 12 个当前保留模型在单步、强约束、单工具的测试里都能生成可解析的 `exec_command`。
- 在 2026-04-18 的 6 模型持续复测中，6/6 已进入真实 `command_execution`，但仍无模型稳定完成“按要求读取并收束回答”的复杂任务闭环。
- 增加仓库级 `AGENTS.md` 工具规则与多轮提示加固后，复杂任务可用性仍未达到可生产使用标准。

### Claude Code 兼容状态

代码与测试核对日期：2026-04-17。复测记录见 `docs/reviews/CR-CLAUDE-CODE-E2E-2026-04-17.md`，长时模型矩阵见 `docs/reviews/CR-CLAUDE-CODE-MODEL-MATRIX-2026-04-17.md`。

| 场景 | 结果 | 说明 |
|---|---|---|
| 最小文本任务 | 通过 | `Claude Code -> new-api -> firew2oai -> kimi-k2p5` 可返回 `ok` |
| 单文件 `Read` 工具调用 | 通过 | 能读取 `chat_compat.go` 并基于读取结果收束回答 |
| 跨文件复杂工具调用 | 不通过 | 会进入多轮 `Read`，但不能稳定收束为最终答案 |
| `/v1/messages/count_tokens` | 不通过 | Claude Code 会额外请求该接口，当前 new-api 返回 `404`，暂未阻塞主流程 |

当前结论应限定为：Claude Code 的基础问答与简单单文件工具调用已打通，但 `kimi-k2p5` 仍不具备复杂多轮 Agent 任务可用性。

### Claude Code 全模型结论

- 12 个当前保留 Fireworks 模型，在 `Claude Code -> new-api -> firew2oai` 长时复测中，实际都命中了 `firew2oai-local (channel_id=106)`。
- 已排除“new-api 走错渠道”这一干扰项。
- 但 12 个模型里，0 个模型完成了 `Read + Bash + 最终收束答案` 的复杂任务闭环。
- 0 个模型真正执行了 `Bash`；最好的一批模型只能反复 `Read`，随后停在多 JSON 解码失败、空输出，或 `max_turns`。

因此，Claude Code 在 firew2oai 上当前只能定位为“基础问答 / 简单单文件读取部分可用”，不具备复杂真实任务可用性。

## API 端点

| 端点 | 方法 | 说明 |
|---|---|---|
| `/` | GET | 服务信息 |
| `/health` | GET | 健康检查 |
| `/metrics` | GET | Prometheus 指标（请求量/状态码/时延/并发等） |
| `/v1/models` | GET | 获取模型列表（需认证） |
| `/v1/chat/completions` | POST | 聊天补全（需认证） |
| `/v1/responses` | POST | Responses 文本子集（需认证） |

## 使用示例

### cURL

```bash
# 流式请求
curl -X POST http://localhost:39527/v1/chat/completions \
  -H "Authorization: Bearer sk-admin" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek-v3p2",
    "messages": [{"role": "user", "content": "Hello!"}],
    "stream": true
  }'

# 非流式请求
curl -X POST http://localhost:39527/v1/chat/completions \
  -H "Authorization: Bearer sk-admin" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek-v3p1",
    "messages": [{"role": "user", "content": "1+1=?"}],
    "stream": false
  }'

# 显示 thinking 模型的思考过程
curl -X POST http://localhost:39527/v1/chat/completions \
  -H "Authorization: Bearer sk-admin" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen3-vl-30b-a3b-thinking",
    "messages": [{"role": "user", "content": "解方程 x^2-5x+6=0"}],
    "stream": true,
    "show_thinking": true
  }'

# Responses 协议（非流式）
curl -X POST http://localhost:39527/v1/responses \
  -H "Authorization: Bearer sk-admin" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek-v3p2",
    "input": "只回复 ok",
    "max_output_tokens": 64
  }'
```

## 支持模型

> **可用性检测时间**: 2026-04-17 — Fireworks 上游模型可用性会动态变化，建议通过 `/v1/models` 获取当前可用列表。
>
> 说明：视觉模型标记为可用，表示其**可作为上游 `model` 选择**；当前代理仍按文本消息子集转发，尚未实现 OpenAI 视觉输入格式兼容。

| 模型 | 类型 | 状态 | 备注 |
|---|---|---|---|
| qwen3-vl-30b-a3b-thinking | Thinking (视觉) | ✅ | |
| qwen3-vl-30b-a3b-instruct | 视觉 | ✅ | |
| qwen3-8b | Thinking/通用 | ✅ | 当前上游会输出 `<think>` 块，代理已按 thinking 模型处理 |
| minimax-m2p5 | 通用 | ✅ | |
| llama-v3p3-70b-instruct | 通用 | ✅ | |
| kimi-k2p5 | 通用 | ✅ | 间歇性可用 |
| gpt-oss-20b | 通用 | ✅ | |
| gpt-oss-120b | 通用 | ✅ | |
| glm-5 | 通用 | ✅ | |
| glm-4p7 | 通用 | ✅ | |
| deepseek-v3p2 | 通用 | ✅ | |
| deepseek-v3p1 | 通用 | ✅ | |
| ~~minimax-m2p1~~ | 通用 | ❌ | Fireworks 平台已下架（404） |
| ~~kimi-k2-thinking~~ | Thinking | ❌ | Fireworks 平台已下架（404） |
| ~~kimi-k2-instruct-0905~~ | 通用 | ❌ | Fireworks 平台已下架（404） |
| ~~cogito-671b-v2-p1~~ | 通用 | ❌ | Fireworks 平台已下架（404） |

**注意**：Fireworks 平台模型可用性可能随时变化。如果遇到 502 错误，请检查上游是否返回 `type=error` / 404。

## 指纹伪装策略

本项目的核心目标是让发往 Fireworks.ai 的请求尽可能像来自真实浏览器。

### TLS 层
- Chrome 的 TLS 1.3 密码套件偏好顺序
- TLS 1.2 后备密码套件（GCM 优先，CHACHA20 在后）
- 椭圆曲线偏好：X25519 > P-256（与 Chrome 一致）

### HTTP 层
- Chrome User-Agent（随版本更新）
- `sec-ch-ua` Client Hints（品牌、平台、移动标记）
- `sec-fetch-dest` / `sec-fetch-mode` / `sec-fetch-site` 元数据
- `Origin` / `Referer` 指向 chat.fireworks.ai
- `Accept-Language` / `Accept-Encoding` 与 Chrome 一致
- 连接池参数模拟浏览器行为

## 项目结构

```
firew2oai/
├── cmd/server/main.go          # 入口：服务器启动、优雅关停、信号处理
├── internal/
│   ├── config/config.go        # 配置：环境变量 + 命令行参数
│   ├── proxy/proxy.go          # 核心：协议转换、路由、中间件
│   ├── tokenauth/tokenauth.go  # 认证：多 Key 管理、per-Key 额度和速率限制
│   ├── whitelist/whitelist.go  # 安全：IP 白名单（CIDR 支持）
│   └── transport/transport.go  # 传输：Chrome TLS 指纹、HTTP 伪装
├── Dockerfile                  # 多阶段构建（alpine 最终镜像）
├── docker-compose.yml          # Docker Compose 配置
├── Makefile                    # 构建、测试、多平台编译
├── go.mod / go.sum
├── docs/                       # 审计文档
└── README.md
```

## License

MIT
