# NewAPI Coding Matrix 2026-04-22

日期：2026-04-22

范围：

- 链路：`Codex -> new-api -> firew2oai`
- 模型：`deepseek-v3p1`、`qwen3-8b`、`qwen3-vl-30b-a3b-instruct`
- 真实 coding 场景：
  - `readonly_audit`
  - `add_test_file`
  - `fix_existing_bug`
  - `search_and_patch`
  - `cross_file_feature`

## 结论

在修正矩阵脚本后，上述 3 个模型在 5 个真实 coding 场景达到：

- `15/15 PASS`
- 每个模型均为 `5/5 PASS`

最终证据：

- `/var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260422-103545/summary.tsv`

## 前置排查

在正式收口前，先完成了两轮排查：

1. 针对 `qwen3-vl-30b-a3b-instruct` 的 `readonly_audit`、`add_test_file` 做 targeted 复现
   - 证据：`/var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260422-091635/summary.tsv`
   - 结果：两个场景均 `PASS`
   - 结论：上一轮“大矩阵卡住”不是该模型在 coding 场景上的稳定性问题

2. 先跑一轮 `3 模型 x 17 场景` 全维度矩阵
   - 证据：`/var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260422-092055/summary.tsv`
   - 结果：
     - coding 相关的 `readonly_audit`、`add_test_file` 已稳定通过
     - `fix_existing_bug`、`search_and_patch`、`cross_file_feature` 被矩阵脚本误判为 `files_ok=0`
   - 进一步检查 `jsonl` 可见：
     - 模型实际执行了写文件命令
     - 目标测试通过
     - 最终四行结构化输出为 `RESULT: PASS`

## 根因

误差不在 `firew2oai` 主代理逻辑，而在矩阵脚本的文件变更判定口径：

- 这些 coding fixture 场景通过 `prepare_fixture(...)` 先在 worktree 中制造“待修复状态”
- 模型随后通过 `exec_command` 中的脚本命令直接改写 fixture 文件
- 仅靠 `git status` 或 worktree 结束时残留 diff，不足以稳定证明“模型未修改文件”
- 对这类场景，更可靠的证据是：
  - 写文件命令确实执行
  - 目标测试通过
  - 结构化最终输出为 `PASS`

## 本轮脚本修正

涉及文件：

- `scripts/codex_realchain_matrix.py`

修正内容：

1. 增加 baseline snapshot 逻辑
   - 为 baseline dirty path 与 `expected_files` 建立初始指纹

2. 增加 mutation command 识别
   - 对 `python3 -c`、`write_text`、`apply_patch`、`sed -i` 等写操作命令建立证据判定

3. 调整 `files_ok` 口径
   - 允许“目标文件被写命令直接修改且相关测试通过”的场景判定为通过

## 验证

脚本语法检查：

```bash
python3 -m py_compile scripts/codex_realchain_matrix.py
```

结果：

- 通过

重测命令口径：

- 固定 `CODEX_MATRIX_MODELS=deepseek-v3p1,qwen3-8b,qwen3-vl-30b-a3b-instruct`
- 固定 `CODEX_MATRIX_SCENARIOS=readonly_audit,add_test_file,fix_existing_bug,search_and_patch,cross_file_feature`
- 固定 `CODEX_MATRIX_WORKERS=1`
- 固定 `CODEX_MATRIX_TIMEOUT=300`

重测结果：

- `deepseek-v3p1`: `5/5 PASS`
- `qwen3-8b`: `5/5 PASS`
- `qwen3-vl-30b-a3b-instruct`: `5/5 PASS`

## 说明

- 本文结论只覆盖真实 coding 场景，不覆盖 `web_search`、`docfork`、`cloudflare_spec_probe`、`interactive_shell_session`、`view_image_probe` 等非 coding probe。
- 当前用户关注的是 `codex > new-api > firew2oai` 链路在真实 coding 任务上的表现；在这个口径下，本轮已完成收口。
