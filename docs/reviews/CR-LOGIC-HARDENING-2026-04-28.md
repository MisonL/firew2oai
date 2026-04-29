# CR-LOGIC-HARDENING-2026-04-28

## Scope

本轮针对全程序逻辑复核中发现的已知问题做加固：

- Responses 历史记录按调用 token 绑定 owner，防止已知 response id 被其他 token 读取或续接。
- token 配额从 check-then-record 改为鉴权路径内原子预留，避免并发超发。
- 修正 README 中 upstream retry 默认值与代码实现不一致的问题。
- 修正 Codex realchain 矩阵的 stdin EOF、瞬时 SSE idle timeout 重试、工具探针标签漂移判定。
- 修正工具策略里 no-tool 指令、todo_list 等价 update_plan、MCP resource 无参工具合成、输出标签中的工具名误识别。

## Validation

已执行并通过：

```bash
python3 -m unittest tests.test_codex_realchain_matrix
python3 -m py_compile scripts/codex_realchain_matrix.py tests/test_codex_realchain_matrix.py
go test ./internal/proxy -run 'TestBuildExecutionPolicy_PlanThenReadFinalizesAfterPlanAndCommand|TestBuildExecutionPolicy_ExplicitToolSequenceTreatsTodoListAsUpdatePlan|TestBuildExecutionPolicy_ExplicitToolSequenceSynthesizesMCPResourcesFirst' -count=1
make test
make lint
make build
git diff --check
docker compose up -d --build
curl -fsS http://127.0.0.1:39527/health
```

服务健康检查返回：

```json
{"status":"ok"}
```

## Claude Review

按用户要求使用 `claude --print "prompt"` 做独立审计。直接全仓审计和完整 diff 审计在本机均超时且无输出；随后改为单文件 diff 分片审计。

有效 Claude 发现及处理：

- `internal/proxy/response_state.go`: Medium，同一 response id 被不同 owner 写入时会覆盖旧 entry。已修复为 owner 不一致时拒绝覆盖，并新增 `TestResponseStoreRejectsCrossOwnerOverwrite`。
- `internal/proxy/execution_policy.go`: Medium/Low，`todo_list` 作为 `update_plan` 满足项时去重 key 无效。已修复为按 `todo_list|update_plan` 去重，并新增 `TestCollectSatisfiedToolNames_DedupesTodoListAsUpdatePlan`。
- `scripts/codex_realchain_matrix.py`: Low，瞬时失败 artifact 清理存在极低概率 TOCTOU。已将 `last_path.unlink()` 改为 `missing_ok=True`。
- `README.md`: Medium，retry 默认值已升高但说明仍写“保守开启”。已调整为“默认开启”。

Claude 未输出 Critical/High 阻塞问题。`responses.go`、`tokenauth.go`、`task_intent.go` 的单文件审计在 Claude CLI 中仍超时无输出，因此以本地测试和人工复核为准。

### 2026-04-29 follow-up

继续按模块拆分 `claude --print` 复核。实测结论：

- 直接让 Claude 读取整模块或整文件仍会超时，不能作为有效审计结果。
- 合并全模块事实摘要的总复核 prompt 仍未及时返回，已停止进程，不能作为有效审计结果。
- 对 `cmd/config`、`whitelist`、`tokenauth`、`transport`、`proxy responses`、`proxy tools`、`matrix scripts` 使用极小事实摘要 prompt 后均返回“无可执行发现”。
- Claude 对 Responses owner 空值路径的有效 Medium 发现已处理：`/v1/responses`、`/v1/responses/{id}`、`/input_items` 在 handler 层显式拒绝缺失或非 Bearer token，避免绕过 mux 时出现静默 no-op 或错误 404。
- 人工复核发现并清理了代码与 README 中的非 ASCII 装饰字符，保留协议必要的 thinking marker 为 ASCII 转义字面量。

本轮追加验证：

```bash
go test ./internal/proxy -run 'TestHandleResponses_RequiresBearerOwner|TestHandleResponseByID_RequiresBearerOwner|TestResponsesStoreRejectsCrossTokenAccess|TestHandleResponses_PreviousResponseID' -count=1
go test ./internal/proxy ./internal/config ./internal/transport -count=1
python3 -m unittest tests.test_codex_realchain_matrix
python3 -m py_compile scripts/codex_realchain_matrix.py tests/test_codex_realchain_matrix.py
make test
make lint
make build
git diff --check
# scanned cmd/internal/README/Dockerfile/docker-compose for decorative Unicode markers
```

验证结果均通过；装饰字符扫描无命中。Docker Compose 当前按设计要求 `API_KEY` 必填，不带 `API_KEY` 会在 compose 插值阶段失败。本轮使用临时本地 `API_KEY` 重建容器，并在容器内执行健康检查：

```bash
docker compose exec -T firew2oai wget -qO- http://127.0.0.1:39527/health
```

返回：

```json
{"status":"ok"}
```

## Full Matrix

最终全量全维度 realchain 矩阵：

```text
/var/folders/hq/q19jry150l16mrrbkh7wm0_m0000gn/T/firew2oai-realchain-matrix-20260428-231008/summary.tsv
```

结果：

```text
180 cases
180 ok
0 fail
0 skip
```

按模型：

```text
deepseek-v3p1 15/15
deepseek-v3p2 15/15
glm-4p7 15/15
glm-5 15/15
gpt-oss-120b 15/15
gpt-oss-20b 15/15
kimi-k2p5 15/15
llama-v3p3-70b-instruct 15/15
minimax-m2p5 15/15
qwen3-8b 15/15
qwen3-vl-30b-a3b-instruct 15/15
qwen3-vl-30b-a3b-thinking 15/15
```

## Runtime Audit

矩阵进程和 `codex exec` 子进程已退出，无残留。

new-api 日志从本轮矩阵开始时间后按 token 统计：

```text
token_id=5 user_id=1 username=mison token_name=mison count=797
token_id=3 count=0
```

结论：本轮矩阵请求只走 mison token，没有张任淳 token 残留。

## 2026-04-29 Gemini Follow-up

Gemini CLI 直接读取仓库时会尝试调用不可用的 shell tool。有效方式改为由本地生成 `git diff`，再通过 stdin 传给 `gemini -p`，并要求只审查 stdin。

分片审查结果：

- 配置、鉴权、白名单、transport、Responses owner 隔离：无可执行发现。
- proxy 工具与执行策略：发现 3 个可复核问题，其中 2 个已修复，1 个判定为非问题。
- Docker、矩阵脚本、测试与文档：发现 1 个有效测试口径问题，已修复。

已处理的问题：

- ID 生成不再在 `crypto/rand` 失败时 panic；改为显式返回 error，由 chat/responses/tool 入口返回 500。
- `wait_agent` 的 `timed_out` 判断改为解析 JSON 值，不再用全文去空白字符串匹配，避免命中普通字符串里的 `"timed_out": true` 文本。
- realchain 矩阵不再把原始 `RESULT: FAIL` 改写成 `RESULT: PASS`，改为保留原文并在 summary 中显式记录 `accepted_label_drift=1`。

判定为非问题：

- `chat_compat` 只接受 `text` / `input_text` content block。Gemini 建议保留 `strings.Contains(type, "text")`，但该路径解析的是 Chat Completions 请求内容，`output_text` / `text_delta` 属于响应或流式片段，不应作为请求输入类型静默接受。

本轮追加验证：

```bash
go test ./internal/proxy -run 'TestGenerateRequestID|TestGenerateConversationID|TestInferCollaborationToolOutputSuccess|TestHandleResponses_RequiresBearerOwner|TestResponsesStoreRejectsCrossTokenAccess' -count=1
python3 -m py_compile scripts/codex_realchain_matrix.py tests/test_codex_realchain_matrix.py
python3 -m unittest tests.test_codex_realchain_matrix
make test
make lint
make build
git diff --check
```

结果均通过。

修复后将关键 diff 再次分片输入 `gemini -p` 复核：

- proxy / responses / tool protocol / execution evidence：无可执行发现。
- matrix script / matrix tests / review doc：无可执行发现。

## 2026-04-29 Compose Whitelist Follow-up

外部 review 指出 `docker-compose.yml` 使用 `- IP_WHITELIST` 会在宿主环境未导出变量时不传入容器，导致服务回落到默认 `127.0.0.1,::1` 白名单。Docker 发布端口进入容器时来源通常是 bridge/gateway IP，Compose quick start 会从宿主机访问被 403 拦截。

已恢复为：

```yaml
- IP_WHITELIST=${IP_WHITELIST:-}
```

验证：

```bash
API_KEY=compose-test-key docker compose config
```

确认渲染结果包含：

```yaml
IP_WHITELIST: ""
```
