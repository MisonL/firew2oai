# Codex Known Issues Follow-up - 2026-04-28

## Scope

本记录覆盖 2026-04-28 对已知 Codex realchain 问题的继续分析、适配优化和复测。

主要处理范围：

- DuckDuckGo challenge 页面从 `web_search_no_results` 中拆分为 `web_search_challenge_blocked`。
- 严格工具循环模型在确定性 `exec_command`、`write_stdin`、子代理生命周期工具上直接返回真实工具调用，避免等待上游空流。
- `qwen3-vl-30b-a3b-thinking` 加入严格工具循环模型集合。
- 合成 `wait_agent` 显式使用 `timeout_ms=120000`，避免子代理刚启动即被默认短等待判为超时。

## Realchain Retest

`qwen3-vl-30b-a3b-thinking / add_test_file` 复测输出目录：

```text
/var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260428-094028
```

结果：

```text
status=ok exit_code=0 duration_s=172.4 result_pass=1
observed_signals=command_execution,command-execution,exec_command,exec-command,agent_message,agent-message
```

`qwen3-vl-30b-a3b-thinking / subagent_probe` 最终复测输出目录：

```text
/var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260428-100328
```

结果：

```text
status=ok exit_code=0 duration_s=114.9 result_pass=1
observed_signals=collab_tool_call,collab-tool-call,spawn_agent,spawn-agent,wait,wait_agent,wait-agent,close_agent,close-agent,command_execution,command-execution,exec_command,exec-command,agent_message,agent-message
```

运行期间确认 `responses direct synthetic tool call` 出现在 `spawn_agent`、`wait_agent`、`close_agent`、`exec_command` 路径上。`wait_agent` 返回成功，`close_agent` 返回成功。

## Verification

已执行并通过：

```bash
python3 -m py_compile scripts/codex_realchain_matrix.py tests/test_codex_realchain_matrix.py
python3 -m unittest tests.test_codex_realchain_matrix
go test ./internal/proxy -run 'TestHandleResponses_StreamDirectSyntheticSpawnAgentSkipsUpstream|TestShouldServeSyntheticToolCallDirect_AllowsAgentLifecycleTools|TestBuildExecutionPolicy_ExplicitToolSequenceSynthesizesWaitAgentAndCloseAgent|TestBuildExecutionPolicy_ExplicitToolSequenceAdvancesFromCompletedCollabSpawnAgent' -count=1
make test
make lint
make build
git diff --check
docker compose up -d --build
curl -fsS http://127.0.0.1:39527/health
```

结果摘要：

- Python 矩阵单测：74 tests OK。
- `make test`：`go test -v -race ./...` 通过。
- `make lint`：0 issues。
- `make build`：通过。
- `/health`：`{"status":"ok"}`。
- `docker compose ps firew2oai`：容器 healthy。
- 残留进程检查：无 `codex_realchain_matrix` 或 `codex exec` 进程。

## New API Audit

近 2 小时 `qwen3-vl-30b-a3b-thinking` 相关日志归属：

```text
token_id=5 user_id=1 username=mison token_name=mison count=52 first_cst=2026-04-28 08:50:35 last_cst=2026-04-28 10:05:41
```

近 2 小时 `token_id in (3,5)` 汇总：

```text
token_id=5 user_id=1 username=mison token_name=mison count=1246
```

未发现 `token_id=3` 记录。

## Notes

复测 stderr 中仍可见 Codex runtime 记录类噪声：

```text
failed to record rollout items: thread ... not found
```

该信息未导致矩阵失败。当前证据显示 realchain 场景状态为 `ok`，工具信号和结构化输出均满足要求。
