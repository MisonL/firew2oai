# Codex Realistic Scenario Matrix

日期：2026-04-24

范围：`Codex -> new-api -> firew2oai` 真实链路下，更贴近日常 Codex 使用的评测任务集合。

## 设计目标

这组任务不是工具探针，而是模拟真实开发中常见的 Codex 工作：

- 先运行失败测试，再定位并修复 bug。
- 小范围跨文件重构，同时补充或更新测试。
- 根据代码事实同步文档。
- 只读诊断失败测试，不修改文件。
- 使用 Docfork 查官方文档，再结合仓库上下文回答。

## 场景清单

| 场景 | 类型 | 验收重点 |
|---|---|---|
| `real_debug_regression` | 调试修复 | 先复现失败，再修改 `parser.go` 并通过目标测试 |
| `real_refactor_with_tests` | 小重构 | 新增 helper 文件，修改业务代码和测试，通过目标测试 |
| `real_docs_sync` | 文档同步 | 从 Go 代码提取环境变量，更新 Markdown 表格并用 `rg` 验证 |
| `real_test_diagnosis_no_write` | 只读诊断 | 运行失败测试、阅读源码、给出根因，不产生 diff |
| `real_docfork_api_lookup` | 文档查证 | 使用 Docfork `search_docs` 和 `fetch_doc`，再结合 README 收口 |

## 推荐执行方式

最小 smoke：

```bash
CODEX_MATRIX_SUITE=realistic \
CODEX_MATRIX_MODELS=deepseek-v3p1 \
CODEX_MATRIX_WORKERS=1 \
CODEX_MATRIX_TIMEOUT=900 \
python3 scripts/codex_realchain_matrix.py
```

全模型真实使用场景矩阵：

```bash
CODEX_MATRIX_SUITE=realistic \
CODEX_MATRIX_PROVIDER=newapi-firew2oai-3000 \
CODEX_MATRIX_BASE_URL=http://localhost:3000/v1 \
CODEX_MATRIX_WIRE_API=responses \
CODEX_MATRIX_WORKERS=2 \
CODEX_MATRIX_TIMEOUT=900 \
python3 scripts/codex_realchain_matrix.py
```

如果只测某几个场景，可使用：

```bash
CODEX_MATRIX_SUITE=realistic \
CODEX_MATRIX_SCENARIOS=real_debug_regression,real_refactor_with_tests \
python3 scripts/codex_realchain_matrix.py
```

## 结果解读

这组任务更能反映模型在 Codex 中的真实工程可用性，但仍需要按链路解释结果：

- 失败可能来自模型能力，也可能来自 `new-api` 中转、`firew2oai` 协议适配或上游稳定性。
- 单轮结果只能说明当次表现；正式分梯队建议每个模型每个场景重复 `3-5` 次。
- 真实 Coding 场景应重点看成功率、目标测试是否通过、是否误改无关文件，以及最终四行收口是否可解析。
