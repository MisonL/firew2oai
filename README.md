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

核对日期：2026-04-19  
验证范围：`minimax-m2p5`、`kimi-k2p5`、`glm-5`、`glm-4p7`、`deepseek-v3p2`、`deepseek-v3p1`

| 链路 | commands_ok | turn_completed | format_ok | failed |
|---|---:|---:|---:|---:|
| direct | 6/6 | 6/6 | 6/6 | 0/6 |
| newapi | 6/6 | 6/6 | 6/6 | 0/6 |

详细记录见：

- `docs/reviews/CR-CODEX-MODEL-MATRIX-2026-04-19.md`
- `docs/reviews/CR-CODEX-MODEL-MATRIX-2026-04-18.md`
- `docs/reviews/CR-CODEX-MODEL-MATRIX-2026-04-17.md`

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
| `-log-level` | `LOG_LEVEL` | `info` | `debug/info/warn/error` |
| `-show-thinking` | `SHOW_THINKING` | `false` | 是否输出 thinking 内容 |
| `-cors-origins` | `CORS_ORIGINS` | `*` | 允许跨域来源 |
| `-rate-limit` | `RATE_LIMIT` | `0` | 全局每 Key 每分钟限流，0 表示关闭 |
| `-ip-whitelist` | `IP_WHITELIST` | `127.0.0.1,::1` | 允许访问 IP/CIDR |
| `-trusted-proxy-count` | `TRUSTED_PROXY_COUNT` | `0` | 信任代理层数 |

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

说明：2026-04-19 复测中，请求命中目标渠道 `channel_id=106`。

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
