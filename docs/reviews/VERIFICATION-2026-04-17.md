# Verification 2026-04-17

## Commands

```bash
gofmt -w internal/proxy/responses.go internal/proxy/response_state.go internal/proxy/responses_test.go
go test ./internal/proxy
go test ./...
make test
go test ./internal/proxy -run 'TestHandleResponses_StreamAIActionsBlock_(RequiredToolRejectsFinalMode|ParallelToolCallsFalseRejectsMultipleCalls)'
curl -H "Authorization: Bearer <token>" http://127.0.0.1:39530/v1/models
POST /v1/chat/completions (qwen3-8b, stream=false/true)
POST /v1/responses
POST /v1/responses with previous_response_id
GET /v1/responses/{id}
GET /v1/responses/{id}/input_items
POST https://chat.fireworks.ai/chat/single for minimax-m2p1, kimi-k2-thinking, kimi-k2-instruct-0905, cogito-671b-v2-p1
```

## Results

- `gofmt` completed for `internal/proxy/responses.go`, `internal/proxy/response_state.go`, `internal/proxy/responses_test.go`.
- `go test ./internal/proxy` passed on 2026-04-17.
- `go test ./...` passed on 2026-04-17.
- `make test` passed on 2026-04-17.
- Docker image `firew2oai:fixed` was rebuilt and `firew2oai-smoke` was replaced on `39529 -> 39527`.
- Local rebuilt instance on `:39530` returned 12 models from `/v1/models`.
- `qwen3-8b` no longer leaked `<think>` tags to OpenAI-compatible output.
- `/v1/responses` now preserved multi-turn context through `previous_response_id`.
- `GET /v1/responses/{id}` and `GET /v1/responses/{id}/input_items` both returned stored state successfully.
- `/v1/responses` now stores raw request items and raw history items, not only flattened chat text.
- `response.completed.response.usage` now returns non-zero local estimates for `input_tokens`, `output_tokens`, and `total_tokens`.
- Package tests now cover streamed/non-streamed `usage`, undeclared tool rejection, and `previous_response_id + function_call_output` prompt reconstruction.
- Direct upstream checks confirmed `minimax-m2p1`, `kimi-k2-thinking`, `kimi-k2-instruct-0905`, and `cogito-671b-v2-p1` all returned Fireworks `type=error` with upstream `404 Not Found`.
- Additional Codex matrix retest is recorded in `docs/reviews/CR-CODEX-MODEL-MATRIX-2026-04-17.md`.
- Matrix result: all 12 retained models passed minimal text requests, but all 12 failed the real Codex complex-task benchmark; no model reached actual `command_execution`.
- Adding Codex tool rules to repository `AGENTS.md` did not materially improve the benchmark outcome.
- Latest targeted retest with `kimi-k2p5` is recorded in `docs/reviews/CR-CODEX-E2E-2026-04-17.md`.
- `Codex -> firew2oai` and `Codex -> new-api -> firew2oai` both failed the same way: upstream emitted two consecutive top-level `function_call` JSON objects, and the adapter returned explicit `Codex adapter error: tool call JSON decode failed`.
- Production `new-api` routing was rechecked from PostgreSQL: `channel_id=106`, `name=firew2oai-local`, `priority=100`, `status=1`.
- The latest `kimi-k2p5` retest request hit `new-api.logs.id=159893` with `channel_id=106`, so this failure was not caused by wrong-channel routing.
- A second all-model Codex retest on the two-read forced-exec benchmark showed `6/12` models reached the first real `command_execution`, but `0/12` completed the full two-read-plus-final-answer loop.
- A narrow parser improvement was added for pure consecutive legacy JSON tool-call sequences; unit tests now cover both the accepted pure sequence and the rejected markdown-separated sequence.
- Targeted retests on a patched local instance showed `qwen3-vl-30b-a3b-instruct` and `minimax-m2p5` moved from JSON decode failure to explicit `at most 1 call(s)` rejection, confirming the new parser path is active.
- Additional safe normalizations were added: `run_terminal_cmd`/`run_command` now normalize to `exec_command`, AI_ACTIONS blocks accept fenced JSON payloads, and legacy parsing now preserves prefix text before a multi-object JSON sequence.
- After the alias normalization, `deepseek-v3p2` advanced from immediate undeclared-tool failure to two real `command_execution` rounds before drifting into undeclared `read_file`.
- `qwen3-8b` still failed because it emitted a valid AI_ACTIONS block and then appended non-whitespace trailing text; the adapter intentionally continues to reject that shape.
- Tool protocol hardening now rejects structured/freeform field mismatches, enforces `tool_choice="required"` at runtime, and enforces `parallel_tool_calls=false` as a one-call limit.
- Requests that set `tool_choice` to `required` without declaring any tools now fail fast with `400 invalid_tool_choice` instead of falling through to upstream text mode.
- Named `tool_choice` prompts now use mandatory language, and `tool_choice="none"` removes tool lists plus AI_ACTIONS protocol text from upstream prompts.
- Legacy JSON parsing now requires an explicit `type`; plain JSON answers with a top-level `name` field stay plain text instead of becoming implicit tool calls.
- Responses tool stream now emits `response.output_item.added` before `response.output_item.done`, aligning the tool path with the text-path item lifecycle.
- The no-`done` stream fallback path now emits the same text-item lifecycle (`added -> content_part.added -> output_text.done -> output_item.done`) before `response.completed`.
- New stream gates confirm the adapter returns explicit `Codex adapter error` text instead of silent fallback when required-tool or max-call constraints are violated.
- 2026-04-18 再次执行 `go test ./internal/proxy` 与 `go test ./...`，均通过。
- 2026-04-18 六模型持续迭代复测（`minimax-m2p5`、`kimi-k2p5`、`glm-5`、`glm-4p7`、`deepseek-v3p2`、`deepseek-v3p1`）结果见 `/tmp/codex-six-models-20260418-iter7/summary.tsv`。
- 与 baseline 相比，`adapter_error_before_tool` 与 `partial_tool_then_adapter_error` 均降为 0，6/6 模型都出现了真实 `command_execution`。
- 当前稳定口径下仍未出现可复现的复杂任务闭环完成样本，结论仍为“协议适配可用，复杂多轮 Agent 任务不可用”。
- 一次中间实验（iter6）出现长循环工具调用（`kimi-k2p5`、`glm-5`），相关策略已回退，未进入稳定版本。

## Limits

- Codex protocol compatibility should not be interpreted as practical Codex usability for complex multi-turn Agent tasks.
