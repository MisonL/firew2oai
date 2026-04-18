# CR-CODEX-MODEL-MATRIX-2026-04-18

日期：2026-04-18  
范围：Codex 直连 `firew2oai` 与 `new-api -> firew2oai` 中转，两条链路均使用 `wire_api=responses`。  
模型：`minimax-m2p5`、`kimi-k2p5`、`glm-5`、`glm-4p7`、`deepseek-v3p2`、`deepseek-v3p1`。

## 本轮目标

1. 验证 `execution_stage=finalize` 后是否仍会重复工具调用。  
2. 验证流式空流/异常流是否仍导致客户端持续重连。  
3. 复测 6 模型在真实三步命令任务中的完成情况。

统一任务：

1) `head -n 5 README.md`  
2) `sed -n '170,260p' internal/proxy/tool_protocol.go`  
3) `go test ./internal/proxy`  
最终只允许三行输出：`RESULT`/`README`/`TOOLP`。

## 关键改动核验

- `finalize` 阶段关闭工具暴露：`tools_present=false`、`execution_disable_tools=true`。  
- 流式空流兜底：当无 `done` 且无内容时，显式发送 `response.completed` 与 `Codex adapter error` 文本，避免客户端无限重连。

## 证据目录

- `deepseek-v3p2` 修复前复测：`/private/tmp/codex-iter-retest-20260418-224440-postfix`  
- 6 模型修复前矩阵：`/private/tmp/codex-iter-matrix-20260418-224926-postfix`  
- 6 模型修复后矩阵：`/private/tmp/codex-iter-matrix-20260418-230952-postpatch`  
- `deepseek-v3p2` 补充抽测：`/private/tmp/codex-iter-retest-20260418-230719-postfix2`

## 结果对比（12 样本）

| 指标 | 修复前 | 修复后 |
|---|---:|---:|
| `commands_ok`（三条命令都执行） | 10/12 | 12/12 |
| `turn.completed` 返回 | 10/12 | 12/12 |
| `Codex adapter error`（空流/断流） | 2/12 | 0/12 |
| 三行格式满足（`RESULT/README/TOOLP`） | 2/12 | 2/12 |

说明：修复后主要提升是“协议稳定性”，并未显著提升“最终答案格式与事实质量”。

## postpatch 模型明细（直连+中转）

- 12/12 样本都执行了三条命令并返回 `turn.completed`。  
- `deepseek-v3p2` 不再出现 `Reconnecting... stream closed before response.completed`。  
- 仍有多模型在最终消息输出“执行中描述”或格式偏离，导致复杂任务不可用。

## 补充抽测（deepseek-v3p2）

- 直连与中转在补测中都完成三条命令并返回 `turn.completed`。  
- 直连补测出现一次上游 `tls: bad record MAC`，代理返回终止态错误文本：`Codex adapter error: upstream stream failed before content: ...`。  
- 该行为符合本轮“可终止优先”目标：客户端不再进入无限重连等待。

## new-api 渠道路由复核

Postgres `logs` 复核（最近 30 分钟）显示上述 6 模型请求全部命中 `channel_id=106`，未观察到渠道漂移。

## 结论

当前结论维持：  
`firew2oai` 对 Codex 的协议层适配可用性已提升（状态机更稳定、流闭合更稳定），但复杂真实任务仍不具备生产可用性；核心瓶颈已从“协议崩溃”转为“上游模型最终收束能力不稳定”。
