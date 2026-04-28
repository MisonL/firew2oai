# Codex 内置工具覆盖复测记录

日期：2026-04-27

## 范围

本次复测目标是确认当前本地链路可以驱动 Codex CLI 内置工具与截图中的实验特性开关。

链路：

```text
codex exec -> firew2oai -> new-api http://127.0.0.1:3000/v1 -> mison token -> upstream model
```

本轮不把 Docfork 和 Chrome DevTools 计入内置工具集合。它们属于外部 MCP server，已由全量矩阵的外部工具场景覆盖。

## 实验特性

当前 `codex features list` 中与本轮相关的状态：

```text
js_repl             experimental true
memories            experimental true
external_migration  experimental true
prevent_idle_sleep  experimental true
```

其中 `js_repl` 是可调用工具，已进入矩阵验证。`memories`、`external_migration`、`prevent_idle_sleep` 是运行时特性开关，不是 Responses API 中的单独工具调用，因此通过启动参数启用后验证不破坏工具链。

## 覆盖结果

单模型内置工具覆盖矩阵：

```text
summary: /var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260427-223018/summary.tsv
model: glm-4p7
result: 9 ok / 0 fail
```

覆盖场景：

```text
plan_then_read: update_plan, exec_command
interactive_shell_session: exec_command, write_stdin
js_repl_roundtrip: js_repl, js_repl_reset
web_search_probe: web_search
apply_patch_probe: apply_patch
view_image_probe: view_image
subagent_probe: spawn_agent, wait_agent, close_agent
mcp_resource_listing_probe: list_mcp_resources, list_mcp_resource_templates
mcp_resource_read_probe: read_mcp_resource
```

额外语义探针：

```text
spawn_agent -> wait_agent -> close_agent -> resume_agent -> send_input -> wait_agent -> close_agent
result: PHASE_ONE and PHASE_TWO observed
```

说明：`resume_agent` 和 `send_input` 在复杂模型矩阵提示中不适合作为硬门禁。实测失败原因是模型没有稳定按步骤进入 `resume_agent/send_input`，不是工具运行时不可用。工具语义已通过当前 Codex 运行时的最小探针验证。

## 边界

`request_user_input` 属于交互式用户输入工具。当前会话处于 Default mode，且非交互 `codex exec` 没有可自动应答的用户输入通道，因此不作为自动化通过项。该工具应在 Plan mode 或交互式 UI 中验证。

`image_generation` 在 `codex features list` 中为 stable true，但本轮 `codex exec` 工具声明中未作为普通工具暴露。若需要验证生图，应作为 Responses 原生图像能力单独测试，不混入 Codex CLI coding 工具矩阵。

## 验证命令

```bash
python3 -m py_compile scripts/codex_realchain_matrix.py scripts/codex_realchain_scenarios.py tests/test_codex_realchain_matrix.py
python3 -m unittest tests.test_codex_realchain_matrix
node --check scripts/codex_mcp_resource_fixture.mjs
node --check scripts/docfork_fetch_capture.mjs
node --check scripts/docfork_cli_capture_pool.mjs
go test ./...
git diff --check
make lint
make build
make test
```

验证结果：

```text
python unittest: 72 tests OK
go test ./...: PASS
git diff --check: PASS
make lint: 0 issues
make build: PASS
make test: PASS
```

## new-api 日志归属

本轮 2026-04-27 22:30 CST 后的 new-api 日志归属：

```text
token_id=5
user_id=1
username=mison
token_name=mison
count=73
```

同时间段非 mison 归属记录为 0，`token_id=3` 记录为 0。
