# Codex Full Dimension Matrix 2026-04-24

日期：2026-04-24

范围：`Codex -> firew2oai`，`/v1/responses`，直接命中本地 `firew2oai` 服务。

最新状态：本文件只保留 2026-04-24 直连 firew2oai 历史对照。new-api 网关链路的最新权威结果已在 2026-04-28 16:25 CST 更新为 `180 ok / 0 fail`，不应再用本文梯队代表当前全链路状态。

说明：

- 本文只代表直连 `firew2oai` 的 17 维结果。
- 如果需要对外表述 `Codex -> new-api -> firew2oai` 的最终兼容性，应以 `docs/reviews/CR-NEWAPI-FULL-DIMENSION-MATRIX-2026-04-24.md` 为准。
- 当前矩阵口径已移除 Cloudflare 相关 MCP 场景，MCP 只保留 Chrome DevTools 与 Docfork；本文保留为历史直连对照。

## 证据

| 项目 | 值 |
|---|---|
| 主矩阵 | `/var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260424-115836/summary.tsv` |
| 严格重算 | `/var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260424-115836/summary.strict-20260424.tsv` |
| deepseek-v3p2 补跑 | `/var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260424-145117/summary.tsv` |
| view_image 补跑 | `/var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260424-115801/summary.tsv` |
| qwen thinking subagent 补跑 | `/var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260424-131630/summary.tsv` |
| llama subagent 补跑 | `/var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260424-121745/summary.tsv` |

本轮 17 个场景中，当前环境实际可执行 14 个。以下 3 个场景不计入模型能力：

- `cloudflare_execute_probe`：Cloudflare API MCP 未认证。
- `cloudflare_spec_probe`：Cloudflare API MCP 未认证。
- `apply_patch_probe`：当前 Codex CLI 请求工具列表未声明 `apply_patch`。

## 最新梯队

| 梯队 | 模型 | 可执行维度结果 | 主要失败 |
|---|---|---:|---|
| 第一梯队 | `deepseek-v3p1`、`glm-4p7`、`glm-5`、`gpt-oss-20b`、`kimi-k2p5`、`llama-v3p3-70b-instruct`、`minimax-m2p5`、`qwen3-8b`、`qwen3-vl-30b-a3b-instruct` | `12/14 PASS` | `chrome_devtools_probe` 工具顺序失败；`subagent_probe` 严格内容不匹配 |
| 第二梯队 | `gpt-oss-120b` | `10/14 PASS` | Chrome 工具顺序、交互 shell 语义、js_repl JSON、subagent 内容 |
| 第三梯队 | `qwen3-vl-30b-a3b-thinking` | `9/14 PASS` | Chrome 工具顺序、3 个本地超时、subagent 内容 |
| 上游不可判定 | `deepseek-v3p2` | `0/14 PASS` | Fireworks `503`、`tls: bad record MAC`、本地 900 秒超时 |

按失败类型重算：

- `127` 个可执行场景通过。
- `36` 个场景跳过，均为环境或工具声明不可用。
- `41` 个场景失败，其中 `16` 个是上游或本地长尾类：`503`、TLS bad record、timeout。
- `25` 个是模型或工具编排类：Chrome 工具顺序、subagent 最终内容不匹配、malformed tool JSON、空最终消息等。

## Coding 口径对比

只看 5 个真实 Coding 场景：

- `5/5 PASS`：`deepseek-v3p1`、`glm-4p7`、`glm-5`、`gpt-oss-120b`、`gpt-oss-20b`、`kimi-k2p5`、`llama-v3p3-70b-instruct`、`minimax-m2p5`、`qwen3-8b`、`qwen3-vl-30b-a3b-instruct`。
- `4/5 PASS`：`qwen3-vl-30b-a3b-thinking`，失败项为 `readonly_audit` 超时。
- `0/5 PASS`：`deepseek-v3p2`，失败均为上游 TLS/超时类，不能按模型语义失败归因。

仓库内已归档的 new-api 结果：

- `docs/reviews/CR-NEWAPI-CODING-MATRIX-2026-04-22.md` 记录 `deepseek-v3p1`、`qwen3-8b`、`qwen3-vl-30b-a3b-instruct` 在 `Codex -> new-api -> firew2oai` 的 5 个 Coding 场景均为 `5/5 PASS`。
- `docs/reviews/CR-CODEX-MODEL-MATRIX-2026-04-21.md` 记录大多数模型在 5 个 Coding 场景达到 `5/5 PASS`，`qwen3-vl-30b-a3b-thinking` 属于长尾不稳定。
- `docs/reviews/CR-NEWAPI-FULL-DIMENSION-MATRIX-2026-04-24.md` 是当前 `Codex -> new-api -> firew2oai` 的 15 维权威结果。
- 本轮直接链路结果与上述 coding 口径基本一致；差异主要来自新增的 MCP、图像、js_repl、子代理、交互 shell 等全维度探针，而不是 5 个核心 Coding 场景整体回退。

## 本轮优化

- `view_image` 工具输出中的 `input_image` 现在可作为显式工具序列成功信号，修复 `view_image_probe` 反复触发的问题。
- `subagent_probe` 增加严格最终内容校验，必须包含 README 第一行 `# firew2oai`，避免把命令叙述、`completed:null`、`previous_status:running` 判为通过。
- 矩阵脚本支持从本地 firew2oai 日志解析 `/responses/{id}/input_items`，用于恢复工具信号。
- 失败分类新增 `upstream_service_unavailable`、`upstream_tls_bad_record_mac`、`upstream_transport_error`、`missed_chrome_devtools_sequence`、`empty_final_after_tool`。
- MCP 评测口径已收敛为 Chrome DevTools 与 Docfork，不再纳入 Cloudflare 相关场景。

## 验证

已执行：

```bash
python3 -m unittest tests.test_codex_realchain_matrix
GOCACHE=/tmp/firew2oai-go-cache go test ./... -count=1
```

结果：

- Python 矩阵脚本单测：`46` 个通过。
- Go 全仓测试：通过。
