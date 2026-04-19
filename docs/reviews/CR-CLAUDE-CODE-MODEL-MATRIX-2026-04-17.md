# Claude Code 模型矩阵复测 2026-04-17

## 目标

验证当前 firew2oai 保留的 12 个 Fireworks 模型，在 `Claude Code -> new-api -> firew2oai` 链路下，给足回合数和时间后，是否具备复杂真实任务可用性。

本轮特别核对两点：

- 请求是否真的命中 `firew2oai-local` 渠道
- 模型是否能完成 `Read + Bash + 最终收束答案` 的完整工具链

## 测试配置

- 客户端：`claude 2.1.112`
- 链路：`Claude Code -> http://127.0.0.1:3000 -> channel 106 firew2oai-local -> http://127.0.0.1:39527`
- 模型范围：当前 `internal/config/config.go` 中保留的 12 个模型
- 参数：
  - `--max-turns 8`
  - 外层单模型超时 `600s`
  - `--permission-mode bypassPermissions`
  - `--output-format stream-json --verbose`

统一任务：

```text
先读取 README.md 中的“Claude Code 兼容状态”和 internal/proxy/chat_compat.go 中的 normalizeChatTools，
再执行命令 go test ./internal/proxy/... 。
最后只用中文输出三行：
1. normalizeChatTools 的作用；
2. 测试命令是否通过；
3. 基于这次实际工具执行，判断当前模型是否适合复杂 Agent 任务。
禁止猜测，禁止修改文件，必须先实际调用工具再回答。
```

原始结果目录：

- `/tmp/claude-model-bench-20260417-long/summary.json`
- `/tmp/claude-model-bench-20260417-long/raw/`

## 渠道核对

直接查询 `new-api.logs` 表，在本轮测试时间窗内，12 个模型的消费记录全部命中：

- `channel_id = 106`

未出现本轮测试模型落到其他渠道的情况。页面中同时出现的 `gpt-5.4` 等记录，属于同时间段其他请求，不应混入本轮 Fireworks 模型判断。

## 汇总结论

- 12 个模型全部命中 `firew2oai-local (106)`。
- 12 个模型里，`Read` 能力部分可触发，但 **0 个模型** 完成 `Read + Bash + 最终收束答案`。
- **0 个模型** 真正调用了 `Bash`。
- 即使放宽到 `8` 轮和 `600s`，仍然没有模型进入“复杂 Agent 任务可用”状态。

## 结果矩阵

| 模型 | 结果 | 工具情况 | 主要失败模式 |
| --- | --- | --- | --- |
| qwen3-vl-30b-a3b-thinking | 未通过 | `Read x1` | 先触发 `Read`，随后上游输出 `Assistant requested tool: Read` 这类非协议文本，导致 `tool call name is empty` |
| qwen3-vl-30b-a3b-instruct | 未通过 | 无有效工具 | 一次输出多个工具 JSON，解码失败 |
| qwen3-8b | 未通过 | `Read x1` | 只读了文件，没有执行 `Bash`，却直接脑补测试结果并下结论 |
| minimax-m2p5 | 未通过 | 无有效工具 | 多 JSON 连发，解码失败 |
| llama-v3p3-70b-instruct | 未通过 | 无有效工具 | 先输出说明文字，再混入工具 JSON，解码失败 |
| kimi-k2p5 | 未通过 | `Read x2` | 能连续读文件，但在准备继续调用时输出多段 JSON，未进入 `Bash` |
| gpt-oss-20b | 未通过 | 无有效工具 | 空输出 |
| gpt-oss-120b | 未通过 | 无有效工具 | 空输出 |
| glm-5 | 未通过 | `Read x1` | 读一轮后停在“我需要继续读取并运行测试”，未继续执行 |
| glm-4p7 | 未通过 | `Read x8` | 长回合反复读文件，直到 `max_turns`，未进入 `Bash` |
| deepseek-v3p2 | 未通过 | `Read x8` | 长回合反复读文件，直到 `max_turns`，未进入 `Bash` |
| deepseek-v3p1 | 未通过 | 无有效工具 | 多 JSON 连发，解码失败 |

## 关键观察

### 1. 给足时间不能解决根因

最慢的两个模型：

- `qwen3-vl-30b-a3b-thinking`：`207s`
- `deepseek-v3p2`：`233s`

说明问题不在“时间不够”，而在工具状态机和输出协议本身不稳定。

### 2. 失败已从“不会调用工具”演变为“只会读，不会收束”

本轮比前一轮更进一步：

- 一部分模型至少能稳定触发 `Read`
- 但仍然没有模型把任务推进到 `Bash`
- 更没有模型完成最终基于真实命令输出的三行答案

### 3. 渠道问题已排除

这轮复测的失败，不是 new-api 选错渠道导致，而是：

- 模型本身不会严格遵守 Claude Code 工具协议
- 或在多轮后不能稳定保持结构化工具输出

## 最终结论

当前结论应收紧为：

- `firew2oai-local` 渠道路由正确
- `Claude Code` 基础问答和单文件 `Read` 已部分打通
- 但在 12 个 Fireworks 模型上，复杂真实任务仍 **全部不可用**

因此，当前 firew2oai 不应被定位为 Claude Code 复杂多轮 Agent 默认通道。
