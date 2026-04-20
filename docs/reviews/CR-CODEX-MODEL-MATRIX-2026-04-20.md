# CR-CODEX-MODEL-MATRIX-2026-04-20

日期：2026-04-20  
范围：Codex 直连 `firew2oai`，以及正式 `new-api -> firew2oai` 中转，统一使用 `wire_api=responses`。  
模型：当前 `internal/config/config.go` 中启用的 12 个模型。

## 任务口径

本轮不再只看固定三步只读任务，而是拆成两类：

1. 只读 Coding 审计任务  
   统一要求：读取 `README.md`、`internal/proxy/tool_protocol.go`，执行 `go test ./internal/proxy`，最后输出四行结构化结果。
2. 真实写代码任务  
   统一要求：新增 `internal/proxy/output_constraints_test.go`，添加 `TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise`，然后执行两条 `go test`。

## 证据目录

- 只读任务：`/private/tmp/firew2oai-allmodels-latest2-20260419-233301`
- 写代码任务（旧基线）：`/private/tmp/firew2oai-writebench-allmodels-20260420-083447`
- 写代码任务（补丁后复测）：`/private/tmp/firew2oai-writebench-postfix-20260420-094500`
- 写代码任务（正式 `new-api` 中转复测）：`/private/tmp/firew2oai-writebench-newapi-prod-20260420-101500`

## 结果摘要

### 只读任务

- `12/12` 模型完成
- 每个模型都完成真实 `command_execution`
- 当前版本在只读审计链路上可用

### 写代码任务（Codex 直连 firew2oai）

| 指标 | 结果 |
|---|---:|
| 实际任务完成 | `7/12` |
| 严格四行收口 | `2/12` |
| 超时/未完成 | `5/12` |

单模型明细：

| 模型 | 实际任务 | 严格收口 | 说明 |
|---|---:|---:|---|
| `qwen3-vl-30b-a3b-thinking` | 否 | 否 | 上游/完成信号层面超时 |
| `qwen3-vl-30b-a3b-instruct` | 是 | 是 | 当前最稳定 |
| `qwen3-8b` | 否 | 否 | 写任务闭环超时 |
| `minimax-m2p5` | 是 | 否 | 任务做成，最终 `FILES/NOTE` 仍漂移 |
| `llama-v3p3-70b-instruct` | 是 | 否 | 任务做成，最终收口不稳 |
| `kimi-k2p5` | 是 | 否 | 任务做成，最终收口不稳 |
| `gpt-oss-20b` | 否 | 否 | `tool_choice requires "apply_patch", got non-tool response` |
| `gpt-oss-120b` | 否 | 否 | `tool_choice requires "apply_patch", got "exec_command"` |
| `glm-5` | 是 | 否 | 任务做成，最终 `FILES/NOTE` 仍漂移 |
| `glm-4p7` | 是 | 否 | 任务做成，最终 `FILES/NOTE` 仍漂移 |
| `deepseek-v3p2` | 是 | 是 | 当前最稳定 |
| `deepseek-v3p1` | 否 | 是 | 本轮执行波动：测试文件与最终文本已生成，但第二条 `go test` 未收完整 |

### 写代码任务（正式 `new-api -> firew2oai`）

| 指标 | 结果 |
|---|---:|
| 实际任务完成 | `8/12` |
| 严格四行收口 | `4/12` |
| 超时/未完成 | `4/12` |

单模型明细：

| 模型 | 实际任务 | 严格收口 | 说明 |
|---|---:|---:|---|
| `qwen3-vl-30b-a3b-thinking` | 否 | 否 | 仍为超时型失败 |
| `qwen3-vl-30b-a3b-instruct` | 是 | 是 | 与直连一致稳定 |
| `qwen3-8b` | 否 | 否 | 仍为超时型失败 |
| `minimax-m2p5` | 是 | 否 | 与直连一致，任务做成但最终收口不稳 |
| `llama-v3p3-70b-instruct` | 是 | 是 | 比直连更好，最终四行稳定 |
| `kimi-k2p5` | 是 | 否 | 与直连一致，任务做成但最终收口不稳 |
| `gpt-oss-20b` | 否 | 否 | `apply_patch/tool_choice` 仍失败 |
| `gpt-oss-120b` | 否 | 否 | `apply_patch/tool_choice` 仍失败 |
| `glm-5` | 是 | 否 | 与直连一致 |
| `glm-4p7` | 是 | 否 | 与直连一致 |
| `deepseek-v3p2` | 是 | 是 | 与直连一致稳定 |
| `deepseek-v3p1` | 是 | 是 | 比直连更好，消除了上一轮波动 |

### 直连与正式中转差异

| 链路 | 实际任务完成 | 严格四行收口 |
|---|---:|---:|
| 直连 `firew2oai` | `7/12` | `2/12` |
| 正式 `new-api -> firew2oai` | `8/12` | `4/12` |

本轮样本里，正式 `new-api` 中转未观察到明显劣化，反而对 `llama-v3p3-70b-instruct`、`deepseek-v3p1` 的最终收口更稳定。

## 本轮补丁影响

本轮补丁主要收敛两类适配误差：

1. 最终四行输出约束  
   - 新增 `FILES`、`NOTE` 的推断与噪声清洗
   - 对 handoff、自述、wrapper 文本做过滤
2. `apply_patch` 工具恢复  
   - 当尾部还有 `mode=final` 时，优先恢复前面的 mutation block

## 2026-04-20 晚间补丁补充

当日又补了一处执行策略误差修复：

1. 测试命令成功判定收紧  
   - 对 `go test`、`pytest` 等测试命令，不再把“输出不完整但暂未显式失败”的情况视为成功
   - 仅在命中明确成功信号时，才允许把该轮测试记为成功
2. finalize 前置误判收敛  
   - 修复前：模型可能在第二条测试输出尚未完整回收时，被适配层误判为“验证完成”，随后提前进入 finalize
   - 修复后：这类场景会继续停留在 verify/execute，而不是过早结束

对应文件：

- `internal/proxy/execution_policy.go`
- `internal/proxy/execution_policy_test.go`

对应本地验证：

- `go test ./internal/proxy`
- `go test ./...`

修复后的增量结论：

- `deepseek-v3p1` 先前那类“第二条测试未收完整却提前 finalize”的问题，确认属于适配层误差，当前已消除
- 正式 `new-api -> firew2oai` 链路下，另外 10 个模型在最小多轮工具任务中全部 PASS
- 但这批 PASS 仅代表多轮工具链路已通，不应直接上升为“真实写代码任务全部可用”

## 2026-04-20 深夜第二梯队复测

在晚间补丁之后，又继续收敛了第二梯队的三类适配误差：

1. 同一句里的多条命令抽取粘连  
   - 例如 `运行 go test ./internal/proxy 和 go test ./...` 先前会被误抽成一条命令，导致任务完成判定错误
2. 模型自带 FAIL 标签污染  
   - 某些模型真实已完成任务，但最终文本仍写 `RESULT: FAIL`，旧逻辑会把这类标签本身再次当成失败证据
3. 探索阶段失败污染最终结论  
   - 例如先读一个尚不存在的目标测试文件，返回 `No such file or directory`，旧逻辑会把这类预期探索失败带入最终 `RESULT/TEST`

对应代码：

- `internal/proxy/task_intent.go`
- `internal/proxy/output_constraints.go`
- `internal/proxy/execution_policy.go`

对应新增回归测试：

- `TestExtractRequiredCommands_SplitsCompoundInlineCommands`
- `TestConstrainFinalText_OverridesModelFailLabelWhenEvidencePassed`
- `TestConstrainFinalText_IgnoresExploratoryFailureAfterRequiredCommandsPass`
- `TestBuildExecutionPolicy_ClearsMissingFileAfterSuccessfulMutation`

本地验证：

- `go test ./internal/proxy`
- `go test ./...`

### 深夜单模型真实任务复测

统一任务仍为“新增 `internal/proxy/output_constraints_test.go` 并执行两条 `go test`”。

证据目录：

- `minimax-m2p5`：`/private/tmp/firew2oai-stage4-driver-20260420-130112/minimax-m2p5.json`
- `kimi-k2p5`：`/private/tmp/firew2oai-kimi-single-rerun-20260420-131621/kimi-k2p5.json`
- `glm-5`：`/private/tmp/firew2oai-glm5-single-rerun-20260420-131447/glm-5.json`
- `glm-4p7`：`/private/tmp/firew2oai-glm4p7-single-20260420-131040/glm-4p7.json`

结果：

| 模型 | 写文件 | 定向测试 | 包测试 | 最终 PASS | 备注 |
|---|---:|---:|---:|---:|---|
| `minimax-m2p5` | 是 | 是 | 是 | 是 | 当前代码下已完成闭环 |
| `kimi-k2p5` | 是 | 是 | 是 | 是 | finalize 阶段仍可能受上游 TLS 扰动影响，但本轮重试后 PASS |
| `glm-5` | 是 | 是 | 是 | 是 | 先前“做完但 FAIL”确认是适配层误判 |
| `glm-4p7` | 是 | 是 | 是 | 是 | 当前代码下已完成闭环 |

增量结论：

- 第二梯队当前不应再简单归因为“模型自身完全不会做 Codex 真实任务”
- 至少在该统一真实写代码任务上，`minimax-m2p5`、`kimi-k2p5`、`glm-5`、`glm-4p7` 已能在当前最新代码上闭环完成
- 这一批模型当前主要剩余风险已从“适配层协议误判”转向“上游稳定性扰动”
- 本节只代表第二梯队在单一真实任务上的最新复测，不等同于重新刷新全部 12 模型总矩阵
- 因此本报告前文的全量矩阵统计仍保留原值；若要刷新总表，需要再按同口径重跑全模型

## 结论

截至 2026-04-20，`firew2oai -> Codex` 在只读 Coding 审计任务上已经稳定；而在真实写代码任务上，当前结论应收紧为：

- 适配层主链已打通
- 一部分“任务做成但最终收口不规整”的误差已被收进适配层
- 真实写代码场景下仍未达到全模型稳定可用
- 正式 `new-api` 中转链路未见明显额外损耗，本轮样本甚至略优于直连
- 剩余问题已主要集中在模型自身 agent/coding 能力差异，以及少量 `apply_patch/tool_choice` 边界
