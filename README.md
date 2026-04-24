# firew2oai

firew2oai 是一个 OpenAI 兼容转换代理。它把 Fireworks 网页聊天接口转换为 OpenAI 风格的文本接口，供 Codex、New API、One API 等客户端或网关接入。

## 项目概览

- 兼容端点：`/v1/chat/completions`、`/v1/responses`、`/v1/models`
- 支持模式：流式与非流式（SSE + JSON）
- 支持能力：多轮会话、工具调用协议适配、`tool_choice` 约束、`previous_response_id` 历史恢复
- 安全与运维：API Key、配额/限流、IP 白名单、`/health`、`/metrics`
- 上游传输：Chrome 风格 TLS/HTTP 指纹，降低上游拒绝概率
- 架构说明：`docs/ARCHITECTURE.md`

## 当前验证状态

核对日期：2026-04-24  
当前以真实链路证据为准，区分 `Codex -> new-api -> firew2oai` 与 `Codex -> firew2oai` 两种口径，不再混写。

### 当前权威口径：15 维真实链路

| 项目 | 结果 |
|---|---|
| 链路 | `Codex -> new-api -> firew2oai` |
| 接口 | `wire_api=responses` |
| 场景 | `15` 个预设场景，MCP 只保留 Chrome DevTools 与 Docfork；其中 `14` 个在当前环境可执行，`1` 个因工具未声明跳过 |
| 主证据 | `/var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260424-165129/summary.tsv` |
| 严格重算 | `/var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260424-165129/summary.strict-20260424-newapi-no-cloudflare.tsv` |
| 当前结论 | `105 ok / 63 fail / 12 skip`，失败集中在交互 shell、`js_repl`、`view_image`、Chrome DevTools、subagent 5 类 probe |

按模型分梯队：

- 第一梯队，`9/14 PASS`：`deepseek-v3p1`、`deepseek-v3p2`、`glm-5`、`gpt-oss-120b`、`kimi-k2p5`、`llama-v3p3-70b-instruct`、`minimax-m2p5`、`qwen3-8b`、`qwen3-vl-30b-a3b-instruct`
- 第二梯队，`8/14 PASS`：`glm-4p7`、`gpt-oss-20b`、`qwen3-vl-30b-a3b-thinking`
- 环境跳过场景为：`apply_patch_probe`

关键结论：

- 5 个核心 Coding 场景仍保持强势：除 `qwen3-vl-30b-a3b-thinking` 的 `readonly_audit` 外，其余模型均通过 `readonly_audit`、`add_test_file`、`fix_existing_bug`、`search_and_patch`、`cross_file_feature`
- 新增的全维度弱项集中在工具历史与收口信号：`interactive_shell_session`、`js_repl_roundtrip`、`view_image_probe`、`chrome_devtools_probe`、`subagent_probe` 在本轮为主要失分项
- 当前全维度权威结论应以 `docs/reviews/CR-NEWAPI-FULL-DIMENSION-MATRIX-2026-04-24.md` 为准

### 辅助口径：直连 firew2oai

- `Codex -> firew2oai` 直连历史 17 维结果已归档到 `docs/reviews/CR-CODEX-FULL-DIMENSION-MATRIX-2026-04-24.md`
- 该文档只用于评估直连转换层能力，不作为 new-api 网关链路的最终对外口径

### 历史 coding 专项

- `docs/reviews/CR-CODEX-MODEL-MATRIX-2026-04-21.md`：`Codex -> new-api -> firew2oai` 的 5 类真实 Coding 场景模型分梯队
- `docs/reviews/CR-NEWAPI-CODING-MATRIX-2026-04-22.md`：3 个模型在 5 类 Coding 场景复测 `15/15 PASS`

## 快速开始

### 源码启动

```bash
git clone https://github.com/mison/firew2oai.git
cd firew2oai
make build
./bin/firew2oai
```

默认监听 `:39527`，默认 API Key 为 `sk-admin`。

### Docker 启动

```bash
docker compose up -d --build
```

或手动运行：

```bash
docker build -t firew2oai:latest .
docker run -d -p 39527:39527 -e API_KEY=sk-admin firew2oai:latest
```

## 配置说明

### 主要参数

| 参数 | 环境变量 | 默认值 | 说明 |
|---|---|---|---|
| `-port` | `PORT` | `39527` | 监听端口 |
| `-host` | `HOST` | `""` | 监听地址，空表示所有网卡 |
| `-api-key` | `API_KEY` | `sk-admin` | API Key 配置（见下文） |
| `-timeout` | `TIMEOUT` | `120` | 上游超时（秒） |
| `-upstream-retry-count` | `UPSTREAM_RETRY_COUNT` | `1` | 首包前瞬时失败重试次数，适用于发送失败与 `429/502/503/504` |
| `-upstream-retry-backoff-ms` | `UPSTREAM_RETRY_BACKOFF_MS` | `200` | 首包前瞬时失败重试基础退避（毫秒） |
| `-upstream-empty-retry-count` | `UPSTREAM_EMPTY_RETRY_COUNT` | `1` | 上游已返回 `200` 但在任何内容或 `done` 信号前空结束时的重试次数 |
| `-upstream-empty-retry-backoff-ms` | `UPSTREAM_EMPTY_RETRY_BACKOFF_MS` | `200` | 空流重试基础退避（毫秒） |
| `-log-level` | `LOG_LEVEL` | `info` | `debug/info/warn/error` |
| `-show-thinking` | `SHOW_THINKING` | `false` | 是否输出 thinking 内容 |
| `-cors-origins` | `CORS_ORIGINS` | `*` | 允许跨域来源 |
| `-rate-limit` | `RATE_LIMIT` | `0` | 全局每 Key 每分钟限流，0 表示关闭 |
| `-ip-whitelist` | `IP_WHITELIST` | `127.0.0.1,::1` | 允许访问 IP/CIDR |
| `-trusted-proxy-count` | `TRUSTED_PROXY_COUNT` | `0` | 信任代理层数 |

### 上游重试策略

项目当前把上游容错拆成两层，默认都保守开启，且都可通过重试次数设为 `0` 关闭：

- `UPSTREAM_RETRY_COUNT` / `UPSTREAM_RETRY_BACKOFF_MS`
  只处理首包前的基础设施级瞬时失败，包括发送失败以及 `429/502/503/504`。
- `UPSTREAM_EMPTY_RETRY_COUNT` / `UPSTREAM_EMPTY_RETRY_BACKOFF_MS`
  只处理“上游已经返回 `200`，但在任何内容或 `done` 信号前就空结束”的情况，适用于 `/v1/chat/completions` 和 `/v1/responses`。

约束边界：

- 只在尚未向客户端产出任何正文内容前重试。
- 一旦已经收到正文内容，即使上游后续中断，也不会透明重试，避免把有状态输出重复播放给客户端。
- 这两层重试都只覆盖瞬时上游扰动，不覆盖模型语义错误、协议跑偏或 finalize 收口失败。

### API Key 配置格式

1. 单 Key

```bash
./bin/firew2oai -api-key sk-admin
```

2. 多 Key（逗号分隔）

```bash
./bin/firew2oai -api-key "sk-admin,sk-user1,sk-user2"
```

3. JSON 文件（推荐）

```json
[
  {"key":"sk-admin","quota":0,"rate_limit":0},
  {"key":"sk-user1","quota":1000,"rate_limit":60}
]
```

```bash
./bin/firew2oai -api-key /path/to/tokens.json -rate-limit 30
```

4. 内联 JSON

```bash
./bin/firew2oai -api-key '[{"key":"sk-admin"},{"key":"sk-user","quota":500,"rate_limit":20}]'
```

配额与限流响应头：

- `X-Quota-Limit` / `X-Quota-Remaining`
- `X-RateLimit-Limit` / `X-RateLimit-Remaining` / `X-RateLimit-Reset`

## 接入方式

### 方式一：Codex 直连 firew2oai

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

1. 启动 firew2oai（例如 `:39527`）
2. 在 New API 或 One API 新建 OpenAI 渠道
3. 渠道地址填写 `http://your-host:39527`
4. 渠道密钥填写 firew2oai 的 API Key
5. 如果同模型有多个渠道，提高 firew2oai 渠道优先级

说明：若走 New API / One API，多渠道同模型时应提高 `firew2oai` 渠道优先级；测试时必须通过网关日志确认请求命中目标渠道。

## 协议适配逻辑

### Chat Completions

- 支持 `messages[].content` 为字符串或文本 block 数组
- 支持 `tools`、`tool_choice`、`parallel_tool_calls`
- 工具调用输出可转换为 OpenAI `tool_calls`

### Responses

- 支持 `input` 字符串与数组输入
- 支持 `previous_response_id` 多轮恢复
- 支持 `response.output` item 级历史回灌
- 返回 `usage` 的本地估算值，便于网关展示

### 工具调用协议

模型侧优先使用尾部控制块 `AI_ACTIONS_V1`，代理优先按该协议解析；若未命中，再回退 legacy JSON 解析。

工具调用示例：

```text
<<<AI_ACTIONS_V1>>>
{"mode":"tool","calls":[{"name":"exec_command","arguments":{"cmd":"pwd"}}]}
<<<END_AI_ACTIONS_V1>>>
```

最终回答示例：

```text
最终答案正文
<<<AI_ACTIONS_V1>>>
{"mode":"final"}
<<<END_AI_ACTIONS_V1>>>
```

运行时约束：

- 结构化工具必须使用 `arguments`
- 自由格式工具必须使用 `input`
- `tool_choice: "required"` 会强制工具调用
- `tool_choice: "none"` 会禁用工具并从上游 prompt 隐藏工具协议
- `parallel_tool_calls: false` 时，每轮最多一个工具调用

## 支持模型

当前启用模型（`internal/config/config.go`）：

- `qwen3-vl-30b-a3b-thinking`
- `qwen3-vl-30b-a3b-instruct`
- `qwen3-8b`
- `minimax-m2p5`
- `llama-v3p3-70b-instruct`
- `kimi-k2p5`
- `gpt-oss-20b`
- `gpt-oss-120b`
- `glm-5`
- `glm-4p7`
- `deepseek-v3p2`
- `deepseek-v3p1`

已从默认列表移除（上游 404）：

- `minimax-m2p1`
- `kimi-k2-thinking`
- `kimi-k2-instruct-0905`
- `cogito-671b-v2-p1`

## API 端点

| 端点 | 方法 | 说明 |
|---|---|---|
| `/` | GET | 服务信息 |
| `/health` | GET | 健康检查 |
| `/metrics` | GET | Prometheus 指标 |
| `/v1/models` | GET | 模型列表（需认证） |
| `/v1/chat/completions` | POST | Chat Completions（需认证） |
| `/v1/responses` | POST | Responses（需认证） |
| `/v1/responses/{id}` | GET | 查询 response（需认证） |
| `/v1/responses/{id}/input_items` | GET | 查询输入项（需认证） |

## 常用命令

| 命令 | 说明 |
|---|---|
| `make build` | 本机编译到 `bin/firew2oai` |
| `make run` | 编译后直接运行 |
| `make test` | 执行 `go test -v -race ./...` |
| `make lint` | 执行 `golangci-lint run ./...` |
| `make build-all` | 交叉编译多平台产物 |
| `make docker-up` | Docker Compose 启动 |
| `make docker-down` | Docker Compose 停止 |

## 项目结构

```text
firew2oai/
├── cmd/server/main.go
├── internal/
│   ├── config/
│   ├── proxy/
│   ├── tokenauth/
│   ├── transport/
│   └── whitelist/
├── docs/reviews/
├── Dockerfile
├── docker-compose.yml
├── Makefile
└── README.md
```

## License

MIT
