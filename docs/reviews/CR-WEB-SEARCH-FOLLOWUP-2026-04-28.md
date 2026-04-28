# CR-WEB-SEARCH-FOLLOWUP-2026-04-28

## Scope

- Investigate `web_search_challenge_blocked` on `qwen3-8b/web_search_probe`.
- Investigate `web_search_followup_unstructured` on `minimax-m2p5/web_search_probe`.
- Treat Docfork 429 separately as successful tool invocation with upstream rate limiting.

## Root Cause

- `web_search_followup_unstructured`: the follow-up model output contained a complete required label block, but it also included a preface before `RESULT:`. The server-side web search adapter only accepted required labels when they started at the first non-empty line, so it misclassified the output as unstructured.
- `web_search_challenge_blocked`: DuckDuckGo returned an anti-bot challenge to the anonymous search request. The tool call itself was emitted and observed; the backend search provider blocked the HTTP request. This is external and can recur when the current egress is challenged.

## Change

- Normalize server-side web search follow-up output with the existing required-label extractor before applying the strict top-line label check.
- Keep missing-label behavior strict: outputs without the required labels still return `web_search follow-up omitted required output labels`.
- Do not synthesize success for search provider challenge responses.

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

New API log ownership after `2026-04-28 12:05:00+08`:

- `token_id=5`, `user_id=1`, `username=mison`, `token_name=mison`, `count=67`

Residual process check:

- No `codex_realchain_matrix` or `codex exec` process remained after the targeted retest.
