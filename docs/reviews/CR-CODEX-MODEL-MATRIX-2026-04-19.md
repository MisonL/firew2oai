# CR-CODEX-MODEL-MATRIX-2026-04-19

日期：2026-04-19  
范围：Codex 直连 `firew2oai` 与 `new-api -> firew2oai` 中转，`wire_api=responses`。  
模型：`minimax-m2p5`、`kimi-k2p5`、`glm-5`、`glm-4p7`、`deepseek-v3p2`、`deepseek-v3p1`。

## 当日迭代摘要

### 阶段 A（postopt2，01:00 左右）

- 证据：`/private/tmp/codex-iter-matrix-20260419-005535-postopt2/summary_classified.tsv`
- 结果：`commands_ok=7/12`、`turn_completed=7/12`、`format_ok=1/12`、`adapter_error=6/12`
- 现象：直连链路能执行但最终收口失败；中转链路出现 `turn.failed`

### 阶段 B（postopt6，13:40 终版）

- 证据目录：`/private/tmp/codex-iter-matrix-20260419-133613-postopt6`
- 汇总：`summary_agg.tsv`、`summary_classified.tsv`
- 聚合结果：

| 链路 | total | cmd_count_sum | commands_ok | turn_completed | format_ok | failed |
|---|---:|---:|---:|---:|---:|---:|
| direct | 6 | 18 | 6/6 | 6/6 | 6/6 | 0/6 |
| newapi | 6 | 18 | 6/6 | 6/6 | 6/6 | 0/6 |

- 单模型结果（12 样本）：
  - 每个模型在每条链路都执行 `head + sed + go test` 三步（`cmd_count=3`）
  - `adapter_error=0`、`reconnect=0`、`failed=0`

## 本轮机制口径

1. 工具协议：`AI_ACTIONS` 优先解析，legacy JSON 回退，`tool_choice` 强约束。  
2. 执行策略：`explore/execute/verify/finalize` 阶段推进，读循环抑制与下一命令注入。  
3. 收口机制：最终文本约束 + 控制标记清洗 + 空流一次重试。  

## 结论

以 6 模型样本的双链路矩阵看，2026-04-19 终版（postopt6）已达到复杂任务可用状态：工具调用、会话推进、格式收口均稳定通过。
