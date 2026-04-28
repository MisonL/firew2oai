# Docfork Real Chain Fix 2026-04-25

日期：2026-04-25

范围：`Codex -> new-api -> firew2oai` 真实链路下的 `real_docfork_api_lookup` 聚焦复测。

## 背景

此前该场景在全模型矩阵中表现为 `0/12 PASS`。失败集中在 Docfork MCP 参数、AI_ACTIONS 解析容错、以及 MCP 工具序列后的执行策略推进。

## 根因

1. `mcp__docfork__search_docs` 参数归一化不足。部分模型只输出 `query: react useEffectEvent`，缺少 Docfork 必填的 `library`，导致 MCP 参数校验失败后反复重试。
2. AI_ACTIONS 结束标记容错不足。部分模型输出 `<<<END_AI_ACTIONS_V1>>}` 时会落入 legacy JSON 解析错误路径。
3. 执行策略未正确推进 MCP 后续步骤。`search_docs` 和 `fetch_doc` 完成后，策略未稳定推进到 `exec_command` 读取 `README.md`，重复 Docfork 只读工具调用未被改写到下一条必需命令。
4. 评测脚本没有把 `mcp_tool_call` 纳入操作证据，导致真实工具调用完成后仍可能被计为 `commands_ok=0`。
5. synthetic Docfork 搜索参数提取只覆盖 `搜索 react 文档中的 useEffectEvent`，未覆盖真实 realistic 场景里的 `搜索 react useEffectEvent`，导致初始轮 `tool_choice` 约束下无法构造 synthetic `search_docs`。

## 修复

- `mcp__docfork__search_docs` 支持从 `query` 推断 `library`，覆盖 `react useEffectEvent` 这类真实输出。
- AI_ACTIONS 解析兼容轻微损坏的结束标记。
- MCP history 中的 `mcp_tool_call` 纳入执行策略信号。
- 明确阅读根目录 `README.md` 时纳入必读文件集合。
- `search`、`fetch`、`get`、`snapshot`、`screenshot` 等非变更工具按只读调用处理，重复只读 MCP 调用会被改写到下一条必需 `exec_command`。
- 矩阵脚本把 MCP 工具调用计入 `commands_ok`，并为 Docfork 场景补充完整操作期望。
- synthetic Docfork 搜索支持 compact prompt，能从 `搜索 react useEffectEvent` 提取 `library=react, query=useEffectEvent`。

## 验证

本地验证：

```bash
python3 -m unittest tests.test_codex_realchain_matrix
python3 -m py_compile scripts/codex_realchain_matrix.py scripts/codex_realchain_scenarios.py
GOCACHE=/tmp/firew2oai-go-cache go test ./... -count=1
```

容器验证：

```bash
docker compose up -d --build firew2oai
docker inspect --format '{{.State.Health.Status}}' firew2oai
```

真实链路复测：

```bash
CODEX_MATRIX_SUITE=realistic \
CODEX_MATRIX_PROVIDER=newapi-firew2oai-3000 \
CODEX_MATRIX_BASE_URL=http://localhost:3000/v1 \
CODEX_MATRIX_WIRE_API=responses \
CODEX_MATRIX_HISTORY_CONTAINER=firew2oai \
CODEX_MATRIX_DECLARED_TOOLS=exec_command,mcp__docfork__search_docs,mcp__docfork__fetch_doc \
CODEX_MATRIX_WORKERS=2 \
CODEX_MATRIX_TIMEOUT=420 \
CODEX_MATRIX_SCENARIOS=real_docfork_api_lookup \
python3 scripts/codex_realchain_matrix.py
```

证据文件：

- `/var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260425-213408/summary.tsv`

## 结果

总体：`12 ok / 0 fail`。

通过模型：

- `deepseek-v3p1`
- `deepseek-v3p2`
- `glm-4p7`
- `glm-5`
- `gpt-oss-120b`
- `gpt-oss-20b`
- `kimi-k2p5`
- `llama-v3p3-70b-instruct`
- `minimax-m2p5`
- `qwen3-8b`
- `qwen3-vl-30b-a3b-instruct`
- `qwen3-vl-30b-a3b-thinking`

通过样本均完成 `mcp__docfork__search_docs -> mcp__docfork__fetch_doc -> exec_command README.md -> RESULT: PASS`。`qwen3-vl-30b-a3b-thinking` 最新全矩阵耗时 `275.8s`，仍是长尾样本，但已不再因工具循环或初始非工具叙述失败。

## 结论

本轮问题主要是适配层和评测口径缺口，不是 Docfork 工具本身不可用。修复后 Docfork 场景已从 `0/12 PASS` 收敛为 `12/12 PASS`。剩余风险不再是功能失败，而是 `qwen3-vl-30b-a3b-thinking` 的耗时长尾。

## 二次优化记录

追加观察最新失败样本后，`qwen3-vl-30b-a3b-thinking` 的主要残留模式是：`search_docs` 与 `fetch_doc` 已完成，但下一轮仍重复 Docfork MCP 调用，没有推进到 `exec_command` 读取 `README.md`。

追加修复：

- 在 verify 阶段，当显式 MCP 序列已完成且存在剩余 `NextCommand` 时，构造 synthetic `exec_command`，覆盖旧的 Docfork `tool_choice` 或重复只读 MCP 调用。
- `mcp__docfork__search_docs` 参数归一化进一步清理重复库名前缀，例如把 `library=react, query="react useEffectEvent"` 规范为 `library=react, query="useEffectEvent"`，减少冗余 query token。

追加本地验证：

```bash
python3 -m unittest tests.test_codex_realchain_matrix
python3 -m py_compile scripts/codex_realchain_matrix.py scripts/codex_realchain_scenarios.py
GOCACHE=/tmp/firew2oai-go-cache go test ./... -count=1
docker compose up -d --build firew2oai
docker inspect --format '{{.State.Health.Status}}' firew2oai
```

追加真实链路复测：

- 聚焦单模型：`qwen3-vl-30b-a3b-thinking` 通过，证据文件为 `/var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260425-212832/summary.tsv`。
- 12 模型全矩阵：`12 ok / 0 fail`，证据文件为 `/var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260425-213408/summary.tsv`。
