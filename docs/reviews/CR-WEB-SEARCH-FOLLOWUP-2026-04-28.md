# CR-WEB-SEARCH-FOLLOWUP-2026-04-28

## Scope

- Investigate `web_search_challenge_blocked` on `qwen3-8b/web_search_probe`.
- Investigate `web_search_followup_unstructured` on `minimax-m2p5/web_search_probe`.
- Treat Docfork 429 separately as successful tool invocation with upstream rate limiting.

## Root Cause

- `web_search_followup_unstructured`: the follow-up model output contained a complete required label block, but it also included a preface before `RESULT:`. The server-side web search adapter only accepted required labels when they started at the first non-empty line, so it misclassified the output as unstructured.
- `web_search_challenge_blocked`: DuckDuckGo returned an anti-bot challenge to the anonymous search request. The tool call itself was emitted and observed; the backend search provider blocked the HTTP request. This is external and can recur when the current egress is challenged.
- Full matrix follow-up found one `glm-4p7/web_search_probe` run where web_search succeeded but the follow-up answer said it did not have search results. The adapter returned `web_search_followup_not_grounded` instead of finalizing from the already captured real search summary.
- Full matrix follow-up found one `glm-4p7/docfork_probe` run where Docfork `search_docs` returned `504 Gateway Timeout: upstream request timeout`. The execution policy treated the non-empty output as successful and advanced to the required `fetch_doc` step too early.

## Change

- Normalize server-side web search follow-up output with the existing required-label extractor before applying the strict top-line label check.
- Keep missing-label behavior strict: outputs without the required labels still return `web_search follow-up omitted required output labels`.
- Do not synthesize success for search provider challenge responses.
- When a web_search follow-up refuses to answer from captured results, finalize structured no-file tasks from the real captured search summary instead of returning an adapter error.
- Treat gateway timeout and upstream request timeout tool outputs as failed outputs, so Docfork retries or stops on the search step instead of incorrectly requiring `fetch_doc`.

## Validation

```bash
go test ./internal/proxy -run 'WebSearch' -count=1
```

Exit code: 0.

```bash
make test
```

Exit code: 0.

```bash
make lint
```

Exit code: 0, `0 issues`.

```bash
make build
```

Exit code: 0.

```bash
docker compose up -d --build
curl -fsS http://127.0.0.1:39527/health
```

Exit code: 0, health returned `{"status":"ok"}`.

Targeted realchain retest:

```bash
CODEX_MATRIX_INCLUDE_DIRTY_WORKSPACE=1 \
CODEX_MATRIX_MODELS='minimax-m2p5,qwen3-8b' \
CODEX_MATRIX_SCENARIOS=web_search_probe \
CODEX_MATRIX_WORKERS=1 \
CODEX_MATRIX_TIMEOUT=420 \
CODEX_MATRIX_BASE_URL=http://127.0.0.1:3000/v1 \
CODEX_MATRIX_WIRE_API=responses \
CODEX_MATRIX_BEARER_TOKEN_FILE=/tmp/firew2oai-mison-newapi-token \
python3 scripts/codex_realchain_matrix.py
```

Summary file:

`/var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260428-120505/summary.tsv`

Result:

- `minimax-m2p5/web_search_probe`: `ok`
- `qwen3-8b/web_search_probe`: `ok`

Full matrix before the follow-up fix:

```bash
CODEX_MATRIX_MODELS='qwen3-vl-30b-a3b-instruct,minimax-m2p5,llama-v3p3-70b-instruct,kimi-k2p5,qwen3-vl-30b-a3b-thinking,gpt-oss-20b,glm-5,qwen3-8b,glm-4p7,gpt-oss-120b,deepseek-v3p1,deepseek-v3p2' \
CODEX_MATRIX_WORKERS=2 \
CODEX_MATRIX_TIMEOUT=900 \
CODEX_MATRIX_BASE_URL=http://127.0.0.1:3000/v1 \
CODEX_MATRIX_WIRE_API=responses \
CODEX_MATRIX_BEARER_TOKEN_FILE=/tmp/firew2oai-mison-newapi-token \
python3 scripts/codex_realchain_matrix.py
```

Summary file:

`/var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260428-125802/summary.tsv`

Result: `173 ok / 7 fail`. Failure reasons: `docfork_rate_limited=5`, `missed_docfork_fetch_doc=1`, `web_search_followup_not_grounded=1`.

Follow-up targeted realchain retest after the gateway-timeout and ungrounded-fallback fixes:

```bash
CODEX_MATRIX_INCLUDE_DIRTY_WORKSPACE=1 \
CODEX_MATRIX_MODELS='glm-4p7' \
CODEX_MATRIX_SCENARIOS='web_search_probe,docfork_probe' \
CODEX_MATRIX_WORKERS=1 \
CODEX_MATRIX_TIMEOUT=420 \
CODEX_MATRIX_BASE_URL=http://127.0.0.1:3000/v1 \
CODEX_MATRIX_WIRE_API=responses \
CODEX_MATRIX_BEARER_TOKEN_FILE=/tmp/firew2oai-mison-newapi-token \
python3 scripts/codex_realchain_matrix.py
```

Summary file:

`/var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260428-140512/summary.tsv`

Result:

- `glm-4p7/web_search_probe`: `ok`
- `glm-4p7/docfork_probe`: `ok`

New API log ownership after `2026-04-28 12:05:00+08`:

- `token_id=5`, `user_id=1`, `username=mison`, `token_name=mison`, `count=67`

Residual process check:

- No `codex_realchain_matrix` or `codex exec` process remained after the targeted retest.

## 2026-04-28 15:10 Follow-up Full Matrix

Full matrix after commit `9915d31`:

```bash
CODEX_MATRIX_MODELS='qwen3-vl-30b-a3b-instruct,minimax-m2p5,llama-v3p3-70b-instruct,kimi-k2p5,qwen3-vl-30b-a3b-thinking,gpt-oss-20b,glm-5,qwen3-8b,glm-4p7,gpt-oss-120b,deepseek-v3p1,deepseek-v3p2' \
CODEX_MATRIX_WORKERS=2 \
CODEX_MATRIX_TIMEOUT=900 \
CODEX_MATRIX_BASE_URL=http://127.0.0.1:3000/v1 \
CODEX_MATRIX_WIRE_API=responses \
CODEX_MATRIX_BEARER_TOKEN_FILE=/tmp/firew2oai-mison-newapi-token \
python3 scripts/codex_realchain_matrix.py
```

Summary file:

`/var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260428-140920/summary.tsv`

Result: `179 ok / 1 fail`.

Failure:

- `gpt-oss-20b/docfork_probe`: `semantic_result_fail`. Docfork `search_docs` and `fetch_doc` both completed. The model marked `RESULT: FAIL` because the fetched React Compiler document title contains `Error`, not because the Docfork tool chain failed.

Fix:

- Tightened `docfork_probe` prompt so `RESULT` is based on required Docfork tool calls returning content, and document titles or content containing `Error` do not mean the probe failed.
- Added a regression assertion that the scenario prompt keeps this distinction explicit.

Targeted realchain retest after the prompt fix:

```bash
CODEX_MATRIX_INCLUDE_DIRTY_WORKSPACE=1 \
CODEX_MATRIX_MODELS='gpt-oss-20b' \
CODEX_MATRIX_SCENARIOS='docfork_probe' \
CODEX_MATRIX_WORKERS=1 \
CODEX_MATRIX_TIMEOUT=420 \
CODEX_MATRIX_BASE_URL=http://127.0.0.1:3000/v1 \
CODEX_MATRIX_WIRE_API=responses \
CODEX_MATRIX_BEARER_TOKEN_FILE=/tmp/firew2oai-mison-newapi-token \
python3 scripts/codex_realchain_matrix.py
```

Summary file:

`/var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260428-151029/summary.tsv`

Result:

- `gpt-oss-20b/docfork_probe`: `ok`

## 2026-04-28 16:25 Final Full Matrix

Full matrix after commit `af7ed1d`:

```bash
CODEX_MATRIX_MODELS='qwen3-vl-30b-a3b-instruct,minimax-m2p5,llama-v3p3-70b-instruct,kimi-k2p5,qwen3-vl-30b-a3b-thinking,gpt-oss-20b,glm-5,qwen3-8b,glm-4p7,gpt-oss-120b,deepseek-v3p1,deepseek-v3p2' \
CODEX_MATRIX_WORKERS=2 \
CODEX_MATRIX_TIMEOUT=900 \
CODEX_MATRIX_BASE_URL=http://127.0.0.1:3000/v1 \
CODEX_MATRIX_WIRE_API=responses \
CODEX_MATRIX_BEARER_TOKEN_FILE=/tmp/firew2oai-mison-newapi-token \
python3 scripts/codex_realchain_matrix.py
```

Summary file:

`/var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260428-151305/summary.tsv`

Result: `180 ok / 0 fail`.

Per-model result:

- `deepseek-v3p1`: `15/15`
- `deepseek-v3p2`: `15/15`
- `glm-4p7`: `15/15`
- `glm-5`: `15/15`
- `gpt-oss-120b`: `15/15`
- `gpt-oss-20b`: `15/15`
- `kimi-k2p5`: `15/15`
- `llama-v3p3-70b-instruct`: `15/15`
- `minimax-m2p5`: `15/15`
- `qwen3-8b`: `15/15`
- `qwen3-vl-30b-a3b-instruct`: `15/15`
- `qwen3-vl-30b-a3b-thinking`: `15/15`

New API log ownership after `2026-04-28 15:13:05+08`:

- Matrix requests: `token_id=5`, `user_id=1`, `username=mison`, `token_name=mison`, `count=1584`
- Non-matrix model test: `token_id=0`, `user_id=1`, `username=mison`, `token_name=模型测试`, `model_name=claude-opus-4-6`, `content=模型测试`, `count=1`
- No `token_id=3` records.

Residual process check:

- No `codex_realchain_matrix` or `codex exec` process remained after the final full matrix.
