# NewAPI Full Dimension Matrix 2026-04-24

日期：2026-04-24

范围：

- 链路：`Codex -> new-api -> firew2oai`
- new-api 入口：`http://localhost:3000/v1`
- 接口：`/v1/responses`
- 场景：17 个预设场景

## 证据

| 项目 | 值 |
|---|---|
| 主矩阵 | `/var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260424-165129/summary.tsv` |
| 严格重算 | `/var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260424-165129/summary.strict-20260424-newapi.tsv` |
| provider | `newapi-firew2oai-3000` |
| workers | `2` |
| timeout | `900s` |
| declared tools | `exec_command`、`write_stdin`、`update_plan`、`view_image`、`web_search`、`js_repl`、`js_repl_reset`、`spawn_agent`、`wait_agent`、`close_agent`、`list_mcp_resources`、`list_mcp_resource_templates`、`mcp__docfork__search_docs`、`mcp__docfork__fetch_doc`、`mcp__chrome_devtools__new_page`、`mcp__chrome_devtools__take_snapshot`、`mcp__chrome_devtools__click`、`mcp__chrome_devtools__wait_for` |

本轮 17 个场景中，当前环境实际可执行 14 个。以下 3 个场景不计入模型能力：

- `apply_patch_probe`：当前 Codex CLI 请求工具列表未声明 `apply_patch`
- `cloudflare_execute_probe`：Cloudflare API MCP 未声明或未认证
- `cloudflare_spec_probe`：Cloudflare API MCP 未声明或未认证

## 总体结果

严格重算汇总：

- `105 ok`
- `63 fail`
- `36 skip`

失败类型分布：

- `partial_tool_progress`: `25`
- `missed_chrome_devtools_sequence`: `12`
- `final_content_mismatch`: `12`
- `timeout`: `10`
- `upstream_incomplete_completion`: `2`
- `web_search_followup_not_grounded`: `1`
- `malformed_tool_json`: `1`

场景级表现：

- 全模型通过：`add_test_file`、`cross_file_feature`、`docfork_probe`、`fix_existing_bug`、`plan_then_read`、`search_and_patch`
- 全模型失败：`chrome_devtools_probe`、`interactive_shell_session`、`js_repl_roundtrip`、`subagent_probe`、`view_image_probe`
- 单点失败：`readonly_audit` 仅 `qwen3-vl-30b-a3b-thinking` 失败；`web_search_probe` 仅 `glm-4p7` 失败；`mcp_resource_listing_probe` 仅 `gpt-oss-20b` 失败

## 按模型分梯队

### 第一梯队

以下模型在 14 个可执行场景中达到 `9/14 PASS`：

- `deepseek-v3p1`
- `deepseek-v3p2`
- `glm-5`
- `gpt-oss-120b`
- `kimi-k2p5`
- `llama-v3p3-70b-instruct`
- `minimax-m2p5`
- `qwen3-8b`
- `qwen3-vl-30b-a3b-instruct`

这些模型的共同失败项基本一致：

- `chrome_devtools_probe`: `missed_chrome_devtools_sequence`
- `interactive_shell_session`: `partial_tool_progress`
- `js_repl_roundtrip`: 多数为 `partial_tool_progress`
- `subagent_probe`: `final_content_mismatch`
- `view_image_probe`: 多数为 `timeout`

### 第二梯队

以下模型在 14 个可执行场景中达到 `8/14 PASS`：

- `glm-4p7`
- `gpt-oss-20b`
- `qwen3-vl-30b-a3b-thinking`

额外失分项：

- `glm-4p7`：`web_search_probe` 出现 `web_search_followup_not_grounded`
- `gpt-oss-20b`：`mcp_resource_listing_probe` 出现 `malformed_tool_json`
- `qwen3-vl-30b-a3b-thinking`：`readonly_audit` 与 `js_repl_roundtrip` 出现 `upstream_incomplete_completion`

## 对 5 个核心 Coding 场景的结论

核心 Coding 场景为：

- `readonly_audit`
- `add_test_file`
- `fix_existing_bug`
- `search_and_patch`
- `cross_file_feature`

本轮结果：

- `deepseek-v3p1`、`deepseek-v3p2`、`glm-4p7`、`glm-5`、`gpt-oss-120b`、`gpt-oss-20b`、`kimi-k2p5`、`llama-v3p3-70b-instruct`、`minimax-m2p5`、`qwen3-8b`、`qwen3-vl-30b-a3b-instruct`：`5/5 PASS`
- `qwen3-vl-30b-a3b-thinking`：`4/5 PASS`，失败项为 `readonly_audit`

因此，当前链路对核心 Coding 任务的主结论没有回退；全维度失分主要发生在交互工具、图像工具、浏览器 MCP 和 subagent 收口。

## 与直连 firew2oai 的对比

同日直连结果见 `docs/reviews/CR-CODEX-FULL-DIMENSION-MATRIX-2026-04-24.md`。

对比可见：

- 直连 `Codex -> firew2oai` 下，多数第一梯队模型可达到 `12/14 PASS`
- 经 `new-api` 中转后，多数同模型降至 `9/14 PASS`
- 下降最集中的 5 个 probe 为：
  - `interactive_shell_session`
  - `js_repl_roundtrip`
  - `view_image_probe`
  - `chrome_devtools_probe`
  - `subagent_probe`

基于这组差异，当前更像是 `new-api` 链路对工具历史、工具序列或 finalize 收口信号的保真度不足，而不是 `firew2oai` 主转换层在 5 个核心 Coding 场景上发生整体回退。

## 当前优化空间

按优先级看，当前最值得继续迭代的方向是：

1. `new-api` 中转下的工具历史保真
   - 重点核对 `exec_command -> write_stdin`
   - 重点核对 `js_repl -> js_repl_reset`
   - 目标是消除 `partial_tool_progress`

2. 图像与浏览器工具的完成态恢复
   - `view_image_probe` 目前统一表现为超时或无完整收口
   - `chrome_devtools_probe` 目前统一表现为工具顺序不满足 Codex 适配器要求

3. subagent 最终答案收口
   - 当前统一失败为 `final_content_mismatch`
   - 需要继续区分是中转丢失最终正文、模型只给状态不交付内容，还是代理层对最终内容抽取过严

4. 长尾 completion signal 稳定性
   - `qwen3-vl-30b-a3b-thinking` 已出现 `upstream_incomplete_completion`
   - 这类问题与 2026-04-21、2026-04-22 coding 专项中的长尾表现一致

## 已完成的本地验证

```bash
python3 -m unittest tests.test_codex_realchain_matrix
GOCACHE=/tmp/firew2oai-go-cache go test ./... -count=1
```

结果：

- Python 矩阵脚本单测：`46` 个通过
- Go 全仓测试：通过
