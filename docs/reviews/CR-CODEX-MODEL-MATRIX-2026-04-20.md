# CR-CODEX-MODEL-MATRIX-2026-04-20

日期：2026-04-20  
范围：Codex 直连 `firew2oai`，`wire_api=responses`。  
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

## 结果摘要

### 只读任务

- `12/12` 模型完成
- 每个模型都完成真实 `command_execution`
- 当前版本在只读审计链路上可用

### 写代码任务

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

## 本轮补丁影响

本轮补丁主要收敛两类适配误差：

1. 最终四行输出约束  
   - 新增 `FILES`、`NOTE` 的推断与噪声清洗
   - 对 handoff、自述、wrapper 文本做过滤
2. `apply_patch` 工具恢复  
   - 当尾部还有 `mode=final` 时，优先恢复前面的 mutation block

## 结论

截至 2026-04-20，`firew2oai -> Codex` 在只读 Coding 审计任务上已经稳定；但在真实写代码任务上，当前结论仍应收紧为：

- 适配层主链已打通
- 一部分“任务做成但最终收口不规整”的误差已被收进适配层
- 真实写代码场景下仍未达到全模型稳定可用
- 剩余问题已主要集中在模型自身 agent/coding 能力差异，以及少量 `apply_patch/tool_choice` 边界
