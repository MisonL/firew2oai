# Codex 模型矩阵复测 2026-04-17

## 目标

验证当前 firew2oai 保留的 12 个 Fireworks 模型，在 Codex 真实复杂任务下是否具备实际可用性，而不是只验证最小文本请求或单步工具 JSON。

## 测试设计

### 轮次一：自然复杂任务

统一任务为只读代码审计，要求 Codex 基于以下文件给出中文结论：

- `README.md`
- `internal/config/config.go`
- `internal/proxy/responses.go`
- `internal/proxy/response_state.go`
- `docs/reviews/CR-CODEX-E2E-2026-04-17.md`

要求：

- 必须先用工具读取文件
- 输出 2 句摘要、2 条带文件路径和行号的风险、1 条最终结论
- 不允许修改文件

结果文件：`/tmp/codex-model-bench-20260417/summary.json`

### 轮次二：强约束复杂任务

在轮次一基础上增加硬约束：

- 只允许使用 `exec_command`
- 读取文件必须通过 `exec_command` 执行 `sed`、`nl`、`rg`、`grep`
- 严禁使用 `read_file`、`Read`、`list_files`、`run_terminal_cmd`

结果文件：`/tmp/codex-model-bench-20260417-forced-exec/summary.json`

### 轮次三：AGENTS.md 加固后复测

在仓库根 `AGENTS.md` 中补充 Codex 工具体系要求后，对代表模型再次执行轮次二任务。

结果文件：`/tmp/codex-model-bench-20260417-agents/summary.json`

## 汇总结论

- 12 个模型都能完成最小文本 Responses 请求。
- 12 个模型都能在单步、强约束、单工具测试中输出可解析的 `exec_command`。
- 但在真实复杂任务里，12 个模型全部失败，`command_execution` 均为 0。
- 加入 `AGENTS.md` 后，代表模型仍未进入真实命令执行阶段，提示层加固无实质改善。

因此，当前 firew2oai 虽已完成 Codex 协议适配，但**不具备 Codex 复杂多轮 Agent 任务可用性**。

## 轮次一结果

| 模型 | 结果 | 主要失败模式 |
| --- | --- | --- |
| qwen3-vl-30b-a3b-thinking | 未通过 | 编造 `read_file` |
| qwen3-vl-30b-a3b-instruct | 未通过 | 多 JSON 连发，解码失败 |
| qwen3-8b | 未通过 | 编造 `read_file` |
| minimax-m2p5 | 未通过 | 编造 `Read`，多 JSON 解码失败 |
| llama-v3p3-70b-instruct | 未通过 | 不调用工具，直接编造审计结论与行号 |
| kimi-k2p5 | 未通过 | 只说“我将读取相关文件”，未执行工具 |
| gpt-oss-20b | 未通过 | 空输出 |
| gpt-oss-120b | 未通过 | 空输出 |
| glm-5 | 未通过 | 编造 `read_file` |
| glm-4p7 | 未通过 | 编造 `read_file` |
| deepseek-v3p2 | 未通过 | 编造 `read_file` |
| deepseek-v3p1 | 未通过 | 编造 `read_file`，参数字段漂移 |

## 轮次二结果

| 模型 | 结果 | 主要失败模式 |
| --- | --- | --- |
| qwen3-vl-30b-a3b-thinking | 未通过 | 使用 `exec_command`，但参数是 `command`，且多 JSON 连发 |
| qwen3-vl-30b-a3b-instruct | 未通过 | 把 `exec_command` 写成多个 `custom_tool_call` |
| qwen3-8b | 未通过 | 把结构化工具错误写成 `custom_tool_call` |
| minimax-m2p5 | 未通过 | 参数名错误，多 JSON 连发 |
| llama-v3p3-70b-instruct | 未通过 | 先输出说明文字，再混入工具 JSON |
| kimi-k2p5 | 未通过 | 参数缺少 `cmd`，随后转为澄清提问 |
| gpt-oss-20b | 未通过 | 空输出 |
| gpt-oss-120b | 未通过 | 空输出 |
| glm-5 | 未通过 | 参数缺少 `cmd`，并出现 reconnect |
| glm-4p7 | 未通过 | 参数缺少 `cmd`，随后转为澄清提问 |
| deepseek-v3p2 | 未通过 | 把 `exec_command` 错写为 `custom_tool_call` |
| deepseek-v3p1 | 未通过 | 编造 `run_command`，并出现多次 reconnect |

## 轮次三结果

代表模型：`qwen3-vl-30b-a3b-thinking`、`qwen3-8b`、`kimi-k2p5`、`glm-4p7`、`deepseek-v3p2`

| 模型 | 加 AGENTS 前 | 加 AGENTS 后 | 结论 |
| --- | --- | --- | --- |
| qwen3-vl-30b-a3b-thinking | 多 JSON + `command` 字段错误 | 基本相同 | 无改善 |
| qwen3-8b | `custom_tool_call` 误用 | 基本相同 | 无改善 |
| kimi-k2p5 | 看到缺 `cmd` 后转为解释 | 基本相同 | 无改善 |
| glm-4p7 | 缺 `cmd` 后追问用户 | 改为脑补 `respond` | 无改善 |
| deepseek-v3p2 | `custom_tool_call` 误用 | 改为脑补 `run_terminal_cmd` | 无改善 |

## 失败模式归纳

按出现频率排序：

1. 脑补未声明工具名：`read_file`、`Read`、`run_command`、`run_terminal_cmd`、`respond`
2. 工具 schema 错误：把结构化 `exec_command` 写成 `custom_tool_call`
3. 参数不符合要求：缺少 `cmd`，或错误使用 `command`、`path`、`file_path`
4. 一次输出多个 JSON 工具调用，导致解码失败
5. 直接空输出
6. 收到工具错误后向用户解释或反问，而不是修正同一工具调用
7. 少量模型出现 stream reconnect，说明长链路稳定性也存在问题

## 补充复测：双读取强约束基准

2026-04-17 又补跑一轮更接近真实 Codex 的统一任务：

- 工具只允许 `exec_command`
- 必须先读取两个位置：
  - `sed -n '1,5p' README.md`
  - `sed -n '170,260p' internal/proxy/tool_protocol.go`
- 之后再输出 3 行中文结论

旧服务实例 `:39527` 的 12 模型复测结果：

- 6 / 12 至少执行了第一条真实 `command_execution`
- 0 / 12 完成“两次读取 + 最终收束回答”闭环
- 分类如下：
  - `adapter_error_before_tool`: 5
  - `partial_tool_then_adapter_error`: 2
  - `partial_or_final_text_after_tool`: 4
  - `empty_or_no_final`: 1

对应模型分布：

| 模型 | 命令执行数 | 结果 | 主要现象 |
| --- | --- | --- | --- |
| qwen3-vl-30b-a3b-thinking | 0 | 未通过 | 大段解释后夹带工具 JSON，`tool call JSON decode failed` |
| qwen3-vl-30b-a3b-instruct | 0 | 未通过 | 两个连续 legacy `function_call` JSON，旧解析器解码失败 |
| qwen3-8b | 0 | 未通过 | 两个连续 legacy `function_call` JSON，旧解析器解码失败 |
| minimax-m2p5 | 0 | 未通过 | 两个连续 legacy `function_call` JSON，旧解析器解码失败 |
| llama-v3p3-70b-instruct | 0 | 未通过 | Markdown 说明 + fenced JSON，旧解析器解码失败 |
| kimi-k2p5 | 1 | 未通过 | 完成第一步读取后转成泛化介绍，未继续第二步 |
| gpt-oss-20b | 0 | 未通过 | 空输出 |
| gpt-oss-120b | 1 | 未通过 | 完成第一步读取后直接复述文件内容，未继续第二步 |
| glm-5 | 1 | 未通过 | 第一轮读取后转成解释缺少 `cmd`，未自主修正 |
| glm-4p7 | 1 | 未通过 | 第一轮读取后直接泛化说明项目，不再继续 |
| deepseek-v3p2 | 1 | 未通过 | 第一轮读取后脑补 `run_terminal_cmd` |
| deepseek-v3p1 | 1 | 未通过 | 第一轮读取后脑补 `list_files` |

## 适配层增量优化

针对上面 3 个高频失败样式：

- `qwen3-vl-30b-a3b-instruct`
- `qwen3-8b`
- `minimax-m2p5`

本轮补了一项收紧边界后的解析增强：

- legacy 工具协议现在支持“纯连续 JSON 对象序列”解析
- 只接受连续 JSON 对象 + 空白分隔
- 不接受夹杂 Markdown、解释文字或尾随正文
- 对缺少 `type` 的普通 JSON 仍保持纯文本，不回退成工具调用

新增单元测试已覆盖：

- 连续两个 legacy `function_call` JSON 可解析为 2 个调用
- Markdown fenced JSON 之间夹杂非空白内容时，仍显式报解码错误

补丁后在临时实例 `:39531` 上做代表模型复测：

| 模型 | 补丁前 | 补丁后 | 说明 |
| --- | --- | --- | --- |
| qwen3-vl-30b-a3b-instruct | `tool call JSON decode failed` | `tool protocol allows at most 1 call(s), got 2` | 已正确识别为 2 个工具调用，但 Codex 路径默认 `parallel_tool_calls=false`，因此仍拒绝 |
| minimax-m2p5 | `tool call JSON decode failed` | `tool protocol allows at most 1 call(s), got 2` | 同上 |
| qwen3-8b | `tool call JSON decode failed` | 仍未通过 | 改为输出合法 `AI_ACTIONS_V1` 块后又附加额外项目符号，因尾随内容违反协议而不被接受 |

这说明：

- 解析层还有小块可优化空间，且本轮已经补上最明确的一块
- 但对 Codex 实际可用性的提升有限，因为 Codex 当前链路要求单轮最多一个工具调用
- 剩余主阻塞已经更多落在上游模型行为，而不是连续 JSON 解码本身

## 持续迭代：第二轮安全优化

本轮继续只做“等价表达放宽”，不改变协议语义：

1. `exec_command` 同义别名归一化扩展：
   - `run_terminal_cmd`
   - `run_command`
2. `AI_ACTIONS_V1` 控制块内部 fenced JSON 兼容
3. legacy 路径支持“前缀文本 + 多个连续 JSON 对象”

明确不做的事：

- 不接受 `AI_ACTIONS_V1` 结束标记后的尾随正文
- 不把缺少 `type` 的普通 JSON 猜成工具调用
- 不在 `parallel_tool_calls=false` 时偷偷拆分多调用

补丁后临时实例 `:39531` 代表模型复测：

| 模型 | 结果 | 说明 |
| --- | --- | --- |
| deepseek-v3p2 | 有改善，但仍未通过 | 以前在第一轮卡在 `run_terminal_cmd` 未声明；现在已连续完成两轮 `command_execution`，第三轮才偏航到 `read_file` |
| qwen3-vl-30b-a3b-instruct | 有改善，但仍未通过 | 从 decode error 进展为明确 `tool protocol allows at most 1 call(s), got 2` |
| minimax-m2p5 | 有改善，但仍未通过 | 同上，说明多对象与 AI_ACTIONS 解析已生效 |
| qwen3-8b | 无改善 | 仍输出合法控制块后再追加任务项目符号，因尾随正文违反协议而失败 |

这轮增量优化后的结论更细化为：

- 转换层还有有限优化空间，且安全收益主要来自“等价输入归一化”
- 这类优化能把部分模型从“第一步即失败”推进到“执行 1-2 轮真实工具”
- 但截至目前，仍没有任何模型完成该复杂 Codex 任务闭环

## 2026-04-18 六模型持续迭代复测

复测范围（仅用户指定 6 个模型）：

- `minimax-m2p5`
- `kimi-k2p5`
- `glm-5`
- `glm-4p7`
- `deepseek-v3p2`
- `deepseek-v3p1`

统一任务口径保持不变：

- 先读取 `README.md` 前 5 行
- 再读取 `internal/proxy/tool_protocol.go` 第 170-260 行
- 最后输出 3 行中文结论

结果文件：

- baseline：`/tmp/codex-six-models-20260418-baseline/summary.tsv`
- iter2：`/tmp/codex-six-models-20260418-iter2/summary.tsv`
- iter3：`/tmp/codex-six-models-20260418-iter3/summary.tsv`
- iter5：`/tmp/codex-six-models-20260418-iter5/summary.tsv`
- iter7（当前稳定版）：`/tmp/codex-six-models-20260418-iter7/summary.tsv`

关键变化（以 baseline 对比 iter7）：

- `adapter_error_before_tool`: `2 -> 0`
- `partial_tool_then_adapter_error`: `2 -> 0`
- 6 个模型全部进入真实 `command_execution`（`cmd_count` 范围 `1~3`）
- `glm-5`、`deepseek-v3p2` 的“参数缺失 / 未声明工具”类硬错误已在当前口径下消失

仍未解决的问题：

- 6 个模型依然没有稳定完成“按要求读取两处文件 + 三行收束结论”的闭环，最终回答常见泛化、追问或无关总结。
- 尝试过一版“任务识别启发式”（iter6）虽能拉高调用次数，但会导致 `kimi-k2p5`、`glm-5` 长循环调用，已回退，不纳入稳定结论。

## 最终结论

当前仓库中的 Codex 相关实现，应定位为：

- Responses 协议层已适配
- 工具错误已显式暴露
- 复杂真实任务不具备可用性

不建议将 firew2oai 作为 Codex 复杂多轮 Agent 或 `spawn_agent` 默认模型通道。
