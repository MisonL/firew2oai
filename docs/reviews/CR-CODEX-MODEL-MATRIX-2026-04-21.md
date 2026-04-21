# Codex 当前真实链路验证 2026-04-21

日期：2026-04-21  
范围：`Codex -> new-api -> firew2oai`，统一使用 `wire_api=responses`。  
口径：本文件只保留当前综合结论，不再沉淀更早轮次的中间记录。

## 测试场景

本轮按真实 Coding 链路统一复核以下 5 类场景：

- `readonly_audit`：只读审计指定文件，并运行包级与仓库级测试
- `add_test_file`：新增测试文件，不改业务逻辑
- `fix_existing_bug`：在既有文件内定点修 bug
- `search_and_patch`：先仓库搜索，再定点修补
- `cross_file_feature`：跨文件新增小功能并更新测试

## 证据口径

| 项目 | 值 |
|---|---|
| 主证据 | `/var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260421-094437/summary.tsv` |
| 补测说明 | 本文件覆盖 `deepseek-v3p1`、`deepseek-v3p2`、`glm-4p7`、`gpt-oss-120b`、`qwen3-8b` 的最新实测结果 |
| 补充证据 | `/var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260420-230054/summary.reconstructed.tsv` |
| 用途 | 用于保留本轮已完成全量矩阵与后续补测的综合结论 |

## 最新分梯队结果

### 第一梯队

以下模型在 5 类真实 Coding 场景达到 `5/5 PASS`：

- `glm-5`
- `glm-4p7`
- `gpt-oss-20b`
- `gpt-oss-120b`
- `kimi-k2p5`
- `llama-v3p3-70b-instruct`
- `minimax-m2p5`
- `qwen3-vl-30b-a3b-instruct`
- `deepseek-v3p1`
- `deepseek-v3p2`

结论：

- 当前转换层已经可以稳定支撑第一梯队模型完成只读审计、补测试、修已有 bug、搜索后定点修补、跨文件小功能这 5 类任务。
- `finalize` 收口、写阶段约束和工具结果恢复在这些模型上已形成稳定闭环。

### 第二梯队

- `qwen3-8b`：`4/5 PASS`

最新已确认失败项：

- `readonly_audit`
- 失败表现：`Codex adapter error: upstream response ended without a completion signal`
- 失败证据：`/var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260421-094437/qwen3-8b__readonly_audit.last.txt`

判断：

- 当前残余问题更接近上游 completion signal 稳定性或模型长尾，而不是本轮已确认的主转换层回归。

### 第三梯队

- `qwen3-vl-30b-a3b-thinking`

当前保留结论：

- 该模型在先前全量矩阵中仍出现 completion signal 异常与长尾超时。
- 补充证据中可见 `add_test_file` 出现 `upstream response ended without a completion signal`，`cross_file_feature`、`readonly_audit`、`search_and_patch` 仍有超时。
- 现阶段不应把该模型的剩余失败直接归因为当前转换层主问题。

## 综合结论

- 当前仓库对 Codex 的主适配目标 `/v1/responses` 已能稳定支撑第一梯队模型完成真实 Coding 任务。
- 第二梯队与第三梯队的剩余问题，主要表现为上游空结束、completion signal 缺失或长尾超时。
- 评估模型可用性时，应以真实链路证据为准，不应用单轮单模型样本替代全模型矩阵。
