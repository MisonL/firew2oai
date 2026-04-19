# 架构总览（firew2oai）

## 1. 目标与主场景

firew2oai 的核心目标是把 Fireworks Chat 接口稳定转换为 OpenAI 兼容接口，服务两类主场景：

- Codex 直连 `firew2oai`（`/v1/responses` 为主）
- Codex/Claude Code 先接入 New API，再中转到 `firew2oai`

系统成功标准不是“能返回文本”，而是“多轮会话、工具调用、`tool_choice` 语义在两条链路上保持行为等价”。

## 2. 目录与模块职责

- `cmd/server/main.go`：程序入口，装配配置、路由与 HTTP 服务。
- `internal/config`：配置解析（环境变量/参数）与默认模型列表。
- `internal/proxy`：协议核心层，负责 Chat/Responses 适配、工具协议、会话状态、执行策略。
- `internal/transport`：上游 Fireworks 访问、请求转发与流式读取。
- `internal/tokenauth`：API Key、配额与限流。
- `internal/whitelist`：IP 白名单与可信代理链处理。
- `docs/reviews`：阶段性验证记录与矩阵测试证据。

## 3. 顶层设计（控制面 / 数据面 / 状态面）

- 控制面：`execution_policy.go`、`tool_protocol.go`、`output_constraints.go`
  - 定义“何时可调用工具、最终输出格式约束、失败回退边界”。
- 证据面：`execution_evidence.go`
  - 从历史输入项提取命令与输出证据，为任务完成门禁与最终文本约束提供依据。
- 数据面：`proxy.go`、`responses.go`、`chat_compat.go`、`transport.go`
  - 承载请求编排、SSE 转发、OpenAI 响应组装。
- 状态面：`response_state.go` + 运行内存状态
  - 管理 `response_id` 与多轮上下文恢复。
- 协议类型层：`responses_types.go`
  - 集中维护 Responses API 请求/响应事件结构体，避免编排逻辑和数据结构定义混杂。

## 4. 当前优化结果与非最优点

已完成优化：

- 将输出约束逻辑从 `responses.go` 拆分到 `output_constraints.go`，降低主流程文件耦合度。
- 将 Responses 类型定义拆分到 `responses_types.go`，将执行证据构建拆分到 `execution_evidence.go`。
- `.gitignore` 增加 `.cache/`、`.firecrawl/`，减少运行产物污染。

当前仍非最优点（按优先级）：

1. `internal/proxy/responses.go`、`proxy.go` 仍偏大，编排与协议细节耦合较重。
2. `response_state` 仍为进程内状态，跨实例一致性能力有限。
3. 部分策略分支在同一文件内混合，回归测试需要更细颗粒的模块边界。

## 5. 下一阶段拆分路线（面向真实使用）

1. 把 `responses.go` 再拆为 `responses_handler.go`（HTTP 编排）与 `responses_stream.go`（SSE 解析/输出）。
2. 把工具协议 prompt 构造与解析彻底下沉到 `tool_protocol` 子模块，减少跨文件隐式依赖。
3. 为 `response_state` 预留可插拔存储接口（内存/Redis），支撑多副本场景的会话连续性。

以上路线保持“先行为等价、再结构优化”的原则，避免重构引入协议回归。
