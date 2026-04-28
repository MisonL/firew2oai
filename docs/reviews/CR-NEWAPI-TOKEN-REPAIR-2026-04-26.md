# CR-NEWAPI-TOKEN-REPAIR-2026-04-26

## Scope

本记录覆盖 2026-04-26 误用 new-api `token_id=3` 后的修复与复核。

## Root Cause

- `tokens.id=3` 的 token 名称原为 `mison`，但归属 `user_id=2 / username=张任淳`。
- 本次矩阵运行按 token 名称和启用状态取用了该 token，没有先校验 `user_id`。

## Repair

- 已停止旧矩阵进程，确认无 `codex_realchain_matrix.py` 或 `codex exec --json` 残留。
- 已将误用窗口 `2026-04-26 20:29:34` 到 `2026-04-26 21:50:48` 的 `861` 条日志从 `user_id=2 / token_id=3` 转回 `user_id=1 / token_id=5`。
- 已同步转移 `2,040,578` quota、`9,962,492` input tokens、`49,058` output tokens。
- 已重算受影响小时桶的 `quota_data`。
- 已将 `token_id=3` 名称从 `mison` 改为 `zhangrenchun-user2`，并同步修正其历史日志 `token_name`，避免后续再次误选。
- 已修正 `user_id=2` 与 `token_id=3` 的历史账面差异，使 `used_quota` 与消费日志汇总一致，`request_count` 与 `type=2` 消费日志数量一致。

## Verification

- `token_id=3` 自误用窗口开始后：`0` 条日志，`0` quota。
- `user_id=2 / 张任淳` 自误用窗口开始后：`0` 条日志，`0` quota。
- `user_id=1 / token_id=5` 误用窗口内矩阵模型日志：`861` 条，`2,040,578` quota。
- `user_id=2.used_quota` 与其 `logs` quota 汇总差值：`0`。
- `token_id=3.used_quota` 与其 `logs` quota 汇总差值：`0`。
- `user_id=2.request_count` 与其 `type=2` 消费日志数量差值：`0`。
- `quota_data` 按 `type=2` 消费日志重算口径核对：`0` mismatch。
- `2026-04-26 21:51:00` 后旧矩阵输出目录无新增 `.jsonl`。

## Backup

- 误用窗口日志与配额修复前备份：`/tmp/firew2oai-newapi-token3-repair-20260426-215730`
- 最终 token 名称与历史账面修复前备份：`/tmp/firew2oai-newapi-token3-finalfix-20260426-223528`
