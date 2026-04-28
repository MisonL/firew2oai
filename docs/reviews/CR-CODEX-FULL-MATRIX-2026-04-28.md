# Codex Full Matrix Regression - 2026-04-28

## Scope

本记录覆盖本轮默认全量矩阵复测，不包含 `builtin-tools` 额外套件。矩阵使用本地 `firew2oai` 到本地 `new-api` 链路，Bearer token 从 `/tmp/firew2oai-mison-newapi-token` 读取，未在命令或日志中输出。

输出目录：

```text
/var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260427-225023
```

## Preflight

已执行并通过：

```bash
python3 -m py_compile scripts/codex_realchain_matrix.py tests/test_codex_realchain_matrix.py
python3 -m unittest tests.test_codex_realchain_matrix
git diff --check
make test
make lint
make build
```

结果摘要：

- Python 单测：73 tests OK。
- `make test`：`go test -v -race ./...` 通过。
- `make lint`：0 issues。
- `make build`：通过。
- `/health`：`{"status":"ok"}`。
- mison token 调 `/v1/models`：返回 373 个模型。

## Matrix Command

```bash
CODEX_MATRIX_MODELS='qwen3-vl-30b-a3b-instruct,minimax-m2p5,llama-v3p3-70b-instruct,kimi-k2p5,qwen3-vl-30b-a3b-thinking,gpt-oss-20b,glm-5,qwen3-8b,glm-4p7,gpt-oss-120b,deepseek-v3p1,deepseek-v3p2'
CODEX_MATRIX_WORKERS=2
CODEX_MATRIX_TIMEOUT=900
CODEX_MATRIX_PROVIDER=newapi_mison_full_1777301423
CODEX_MATRIX_BASE_URL=http://127.0.0.1:3000/v1
CODEX_MATRIX_WIRE_API=responses
CODEX_MATRIX_BEARER_TOKEN_FILE=/tmp/firew2oai-mison-newapi-token
CODEX_MATRIX_CODEX_HOME=codex-home
CODEX_MATRIX_WRITE_PROVIDER_CONFIG=1
CODEX_MATRIX_ENABLE_FEATURES=js_repl,memories,external_migration,prevent_idle_sleep
python3 scripts/codex_realchain_matrix.py
```

## Results

原始结果：180 cases，160 ok，20 fail。

按最新分类口径重算失败原因：

| reason | count | interpretation |
| --- | ---: | --- |
| `docfork_rate_limited` | 12 | Docfork 匿名额度 429，工具已真实调用，服务端拒绝。 |
| `web_search_no_results` | 5 | web_search 已真实调用，搜索后端返回或解析为 no results。 |
| `upstream_sse_idle_timeout` | 2 | 上游 SSE idle timeout。 |
| `semantic_result_fail` | 1 | 模型输出 `RESULT: FAIL`，子代理链路返回了 README 内容但最终判定失败。 |

按模型原始结果：

| model | ok | fail |
| --- | ---: | ---: |
| `deepseek-v3p1` | 14 | 1 |
| `deepseek-v3p2` | 14 | 1 |
| `glm-4p7` | 14 | 1 |
| `glm-5` | 13 | 2 |
| `gpt-oss-120b` | 14 | 1 |
| `gpt-oss-20b` | 13 | 2 |
| `kimi-k2p5` | 13 | 2 |
| `llama-v3p3-70b-instruct` | 14 | 1 |
| `minimax-m2p5` | 14 | 1 |
| `qwen3-8b` | 13 | 2 |
| `qwen3-vl-30b-a3b-instruct` | 14 | 1 |
| `qwen3-vl-30b-a3b-thinking` | 10 | 5 |

## Failure Details

- Docfork：12 个模型的 `docfork_probe` 都返回 `429 Monthly rate limit exceeded`，与已知匿名额度限制一致。
- web_search：`kimi-k2p5`、`gpt-oss-20b`、`glm-5`、`qwen3-8b`、`qwen3-vl-30b-a3b-thinking` 的 `web_search_probe` 都发起了 `web_search`，query 为 `latest Go release`，最终返回 `parse web search html: no results found`。
- `qwen3-vl-30b-a3b-thinking`：`readonly_audit` 和 `view_image_probe` 为 `upstream_sse_idle_timeout`。
- `qwen3-vl-30b-a3b-thinking`：`subagent_probe` 为 `semantic_result_fail`，日志显示子代理返回了 README 内容，但模型最终输出 FAIL。

## New API Audit

全量矩阵期间与矩阵相关请求主归属：

| token_id | user_id | username | token_name | count | time range CST |
| ---: | ---: | --- | --- | ---: | --- |
| 5 | 1 | mison | mison | 1322 | 2026-04-27 22:30:15 to 2026-04-28 00:29:41 |

同一时间窗另有 15 条 `token_id=0 / token_name=模型测试 / username=mison` 日志，内容为 new-api 的模型测试记录，模型名与本次矩阵模型集合不一致，且 `content=模型测试`、`request_path=/v1/chat/completions`。未发现 `token_id=3` 记录。

残留进程检查：

```text
no codex_realchain_matrix or codex exec process
```
