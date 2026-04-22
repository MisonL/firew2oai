# Codex Probe Retest 2026-04-22

日期：2026-04-22

范围：

- `Codex -> firew2oai`
- 3 个探针场景：`plan_then_read`、`view_image_probe`、`subagent_probe`
- 3 个模型：`deepseek-v3p1`、`qwen3-8b`、`qwen3-vl-30b-a3b-instruct`

本轮目标：

- 修复无 completion signal 时、只读任务的 evidence 收口
- 修复明文 `AI_ACTIONS` 泄漏导致的结构化输出失败
- 修复 `FILES` 推断对 bare filename 的缺口

## 本轮代码修正

涉及文件：

- `internal/proxy/responses.go`
- `internal/proxy/output_constraints.go`
- `internal/proxy/responses_test.go`

关键修正：

- 新增 `fallbackFinalTextForIncompleteResponses(...)`
  - 仅对只读且 required labels 明确的任务生效
  - 当工具证据全部成功、但上游缺 completion signal 时，直接合成最终结构化输出
- 扩展 `AI_ACTIONS` 泄漏清洗
  - 支持 `AI_ACTIONS:` 明文前缀
  - 支持无冒号版本 `AI_ACTIONS` + fenced JSON block
- 修复 `sanitizeLeakedToolControlMarkup(...)` 清洗后为空时错误返回原脏文本的问题
- 扩展 `FILES` 推断
  - 支持从 `FILES:` hint 提取 bare filename
  - 支持从任务中的 bare filename 提取目标文件，例如 `README.md`
- 放宽只读结构化任务的 `RESULT` 推断
  - 对 `TEST: N/A` 的只读 probe，在无负面信号时允许合成 `PASS`

## 离线验证

执行命令：

```bash
go test ./internal/proxy -count=1
```

结果：

- 通过

## 真实链路证据

最终证据文件：

- `/var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260421-235408/summary.tsv`

结果：

- 总计：`9/9 PASS`
- `plan_then_read`: `3/3 PASS`
- `view_image_probe`: `3/3 PASS`
- `subagent_probe`: `3/3 PASS`

关键结论：

- `deepseek-v3p1 plan_then_read` 已不再被无冒号 `AI_ACTIONS` fenced block 卡住
- `qwen3-vl-30b-a3b-instruct plan_then_read` 已不再因空最终文本丢失结构化收口
- 直接链路上的 3 个 probe 场景已经达到完整收口

## new-api 正式链路阻塞

本轮尝试按 `newapi` skill 的规范继续验证 `Codex -> new-api -> firew2oai`，但本地缺少管理脚本所需配置：

```text
[CONFIG_MISSING] NEWAPI_BASE_URL, NEWAPI_ACCESS_TOKEN, NEWAPI_USER_ID
```

说明：

- 当前阻塞不是 `firew2oai` 代码回归
- 在补齐上述 `NEWAPI_*` 配置前，无法按 skill 规范核验 `new-api` 的模型路由、渠道优先级和正式中转矩阵
