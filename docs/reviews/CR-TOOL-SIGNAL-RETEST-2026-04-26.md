# Tool Signal Retest 2026-04-26

日期：2026-04-26

最新状态：本文件是 2026-04-26 的工具信号修复历史快照。后续已补齐 `apply_patch`、`js_repl`、`view_image`、subagent、web_search、Docfork 和 Chrome DevTools 场景适配，最终全量矩阵在 2026-04-28 16:25 CST 为 `180 ok / 0 fail`。

范围：

- 评分脚本：`scripts/codex_realchain_matrix.py`
- 场景定义：`scripts/codex_realchain_scenarios.py`
- 单测：`tests/test_codex_realchain_matrix.py`
- 聚焦场景：`plan_then_read`、`interactive_shell_session`、`js_repl_roundtrip`、`view_image_probe`、`subagent_probe`、`web_search_probe`

## 本轮修复

1. 修正 `plan_then_read` 的错误评测口径
   - prompt 要求 `update_plan + head -n 3 README.md`
   - 旧 `expected_operations` 错写为 Docfork 工具序列

2. 扩展操作证据判定
   - repo 内文件路径类期望操作允许从 `git diff`、`file_change` 和最终 `FILES:` 标签命中
   - 仍保持命令类操作严格匹配，不把 `go test ...` 这类命令放宽成“只要改了文件就算通过”

3. 扩展工具信号识别
   - 交互式 `python3` 会话若在 `aggregated_output` 中出现 `>>> print(...)` / `exit()`，补记 `write_stdin`
   - 最终消息中若内联 `data:image/...`，补记 `view_image`

## 本地验证

```bash
python3 -m unittest tests.test_codex_realchain_matrix
python3 -m py_compile scripts/codex_realchain_matrix.py scripts/codex_realchain_scenarios.py tests/test_codex_realchain_matrix.py
GOCACHE=/tmp/firew2oai-go-cache go test ./... -count=1
```

结果：

- Python 单测：`56/56 PASS`
- `go test ./...`：通过

## 真实链路复测

### 1. 直连 firew2oai

链路：

- `Codex -> firew2oai`
- `base_url=http://127.0.0.1:39527/v1`

证据：

- `/var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260426-085913/summary.tsv`

结果：

- `5 ok / 1 fail`
- 通过：`plan_then_read`、`interactive_shell_session`、`js_repl_roundtrip`、`view_image_probe`、`web_search_probe`
- 失败：`subagent_probe`

### 2. new-api 中转

链路：

- `Codex -> new-api -> firew2oai`
- `base_url=http://127.0.0.1:3000/v1`
- history 读取仍走 `firew2oai /responses/{id}/input_items`

证据：

- `/var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260426-090245/summary.tsv`

结果：

- `5 ok / 1 fail`
- 通过：`plan_then_read`、`interactive_shell_session`、`js_repl_roundtrip`、`view_image_probe`、`web_search_probe`
- 失败：`subagent_probe`

### 3. 全量 combined 矩阵

链路：

- `Codex -> new-api -> firew2oai`
- `base_url=http://127.0.0.1:3000/v1`
- history 读取走 `firew2oai /responses/{id}/input_items`
- 套件：`12` 个模型 x `20` 个场景

证据：

- `/var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260426-091222/summary.tsv`

结果：

- 总计：`221 ok / 19 fail`
- 失败原因：`partial_tool_progress` 12、`final_content_mismatch` 4、`narration_only` 1、`upstream_service_unavailable` 1、`semantic_result_fail` 1

按模型：

| 模型 | ok | fail |
| --- | ---: | ---: |
| `deepseek-v3p1` | 19 | 1 |
| `deepseek-v3p2` | 18 | 2 |
| `glm-4p7` | 19 | 1 |
| `glm-5` | 18 | 2 |
| `gpt-oss-120b` | 19 | 1 |
| `gpt-oss-20b` | 17 | 3 |
| `kimi-k2p5` | 19 | 1 |
| `llama-v3p3-70b-instruct` | 19 | 1 |
| `minimax-m2p5` | 18 | 2 |
| `qwen3-8b` | 19 | 1 |
| `qwen3-vl-30b-a3b-instruct` | 18 | 2 |
| `qwen3-vl-30b-a3b-thinking` | 18 | 2 |

按场景：

- `apply_patch_probe`：`0 ok / 12 fail`
- `subagent_probe`：`7 ok / 5 fail`
- `js_repl_roundtrip`：`11 ok / 1 fail`
- `real_docs_sync`：`11 ok / 1 fail`
- 其余 `16` 个场景：全部 `12 ok / 0 fail`

## 结论

- `plan_then_read` 之前的失败确认为评测脚本 bug，修复后在两条链路均通过。
- `interactive_shell_session` 的当前真实事件形态可被稳定识别，不再误判失败。
- `view_image_probe` 的当前真实事件形态可被稳定识别，不再误判失败。
- `web_search_probe` 全量 `12/12` 通过，前一轮解析失败已不再复现。
- `js_repl_roundtrip` 全量 `11/12` 通过；`gpt-oss-20b` 有 1 次未按强制工具选择调用 `js_repl`，被判为 `narration_only`。
- `apply_patch_probe` 全量 `0/12` 通过，模型普遍只报告文件名但未实际改写文件，属于真实工具执行失败。
- `subagent_probe` 全量 `7/12` 通过，失败样本仍集中在 `wait_agent` 返回命令建议或不可解析结果，而不是 README 第一行正文。
- `real_docs_sync` 有 1 次 `minimax-m2p5` 上游 Fireworks `503 Service Unavailable`，属于上游服务波动，不计为评测口径问题。
