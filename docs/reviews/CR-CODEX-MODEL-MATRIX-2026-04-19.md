# CR-CODEX-MODEL-MATRIX-2026-04-19

日期：2026-04-19  
范围：Codex 直连 `firew2oai` 与 `new-api -> firew2oai` 中转，`wire_api=responses`。  
模型：`minimax-m2p5`、`kimi-k2p5`、`glm-5`、`glm-4p7`、`deepseek-v3p2`、`deepseek-v3p1`。

## 本轮变更

1. `responses` 增加 `EXECUTION_EVIDENCE` 提示块（基于历史工具调用与输出摘要）。  
2. 增加最终输出硬约束：  
   - 当任务声明 `只输出` + 标签（如 `RESULT/README/TOOLP`）时，缺失标签返回 `Codex adapter error`。  
   - 拦截 `<function_calls>`、`<invoke ...>` 等伪工具控制标记泄漏。  
3. 流式/非流式在“无内容无 done”场景增加一次重试。  

## 证据目录

- 本轮矩阵：`/private/tmp/codex-iter-matrix-20260419-005535-postopt2`  
- 汇总：`/private/tmp/codex-iter-matrix-20260419-005535-postopt2/summary_classified.tsv`  
- 上轮基线：`/private/tmp/codex-iter-matrix-20260418-230952-postpatch/summary_classified.tsv`

## 结果（12 样本）

| 指标 | 上轮 | 本轮 |
|---|---:|---:|
| `commands_ok` | 12/12 | 7/12 |
| `turn.completed` | 12/12 | 7/12 |
| `format_ok` | 2/12 | 1/12 |
| `adapter_error` | 0/12 | 6/12 |

明细结论：

- 直连 6/6 都执行了三条命令并返回 `turn.completed`，但 6/6 被最终门禁改写为 `Codex adapter error`（标签缺失或伪工具标记泄漏）。  
- 中转仅 `glm-5` 完成闭环并满足三行格式；其余 5 个模型均在 Codex 客户端侧出现 `Reconnecting... high demand`，最终 `turn.failed`。  

## new-api 路由复核

Postgres `logs` 近 20 分钟统计：

- `deepseek-v3p1/deepseek-v3p2/glm-4p7/kimi-k2p5/minimax-m2p5` 仅命中 `channel_id=106`。  
- `glm-5` 命中 `channel_id=106`，并出现少量 `channel_id=72/75` 记录（重试链路）。  

## 结论

本轮优化提升了“错误显式化与可诊断性”，但没有提升复杂任务最终可用性；当前仍不具备作为 Codex 复杂多轮 Agent 默认通道的生产可用性。
