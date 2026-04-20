# Codex 当前真实链路验证 2026-04-20

日期：2026-04-20  
范围：`Codex -> new-api -> firew2oai`，统一使用 `wire_api=responses`。
口径：本文件只保留最后一次测试情况，不再沉淀历史矩阵和早期复测记录。

## 测试任务

要求模型只读分析以下文件：

- `internal/proxy/output_constraints.go`
- `internal/proxy/execution_evidence.go`

随后执行两条验证命令：

- `go test ./internal/proxy`
- `go test ./...`

最终回答必须收束为四行：

- `RESULT`
- `CONSTRAINT`
- `EVIDENCE`
- `TEST`

## 测试环境

| 项目 | 值 |
|---|---|
| 链路 | `Codex -> new-api -> firew2oai` |
| 模型 | `glm-5` |
| 证据文件 | `/tmp/firew2oai-compare-newapi-glm5.jsonl` |
| 结果 | `PASS` |

## 执行结果

| 轮次 | 行为 | 结果 |
|---|---|---|
| 1 | 执行 `sed -n '1,200p' 'internal/proxy/output_constraints.go'` | 通过 |
| 2 | 执行 `sed -n '1,200p' 'internal/proxy/execution_evidence.go'` | 通过 |
| 3 | 执行 `go test ./internal/proxy` | 通过 |
| 4 | 执行 `go test ./...` | 通过 |
| 5 | 进入 `execution_stage=finalize` | 通过 |

最终输出：

```text
RESULT: PASS
CONSTRAINT: 已按要求只读分析指定文件，并完成两条测试命令。
EVIDENCE: 已执行 go test ./internal/proxy 和 go test ./...，两者均返回 ok。
TEST: 全部测试通过，指定测试用例与整体测试均返回 ok。
```

## 验证结论

- 真实链路已完成 `command_execution` 工具闭环。
- `finalize` 阶段未再误触发额外工具调用。
- 历史 `Tool result` 摘要可以被当前实现正确恢复为执行证据。
- 该记录只代表最后一次 `glm-5` 真实链路测试，不代表全模型矩阵。
