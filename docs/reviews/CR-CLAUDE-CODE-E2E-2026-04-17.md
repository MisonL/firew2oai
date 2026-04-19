# Claude Code 端到端复测记录 2026-04-17

## 目标

复测 `Claude Code -> new-api -> firew2oai -> Fireworks` 链路在 `kimi-k2p5` 下的真实表现，区分：

- 协议是否打通
- 简单工具调用是否可用
- 复杂多轮读文件任务是否可收敛

## 环境

- `claude --version`: `2.1.112`
- `new-api`: `http://127.0.0.1:3000`
- `firew2oai`: `http://127.0.0.1:39527`
- new-api 渠道：`id=106`，`name=firew2oai-local`，`priority=100`
- 模型：`kimi-k2p5`

注意：复测时必须显式清掉本机 `ANTHROPIC_AUTH_TOKEN`，否则 Claude Code 会优先使用本机认证并产生误导性 401。

## 验证命令

最小任务：

```bash
env -u ANTHROPIC_AUTH_TOKEN \
  ANTHROPIC_BASE_URL=http://127.0.0.1:3000 \
  ANTHROPIC_API_KEY="$NEWAPI_KEY" \
  claude --bare --setting-sources local -p \
  --model kimi-k2p5 \
  --permission-mode bypassPermissions \
  --output-format stream-json \
  --verbose \
  --max-turns 2 \
  "只回答 ok"
```

简单读文件任务：

```bash
env -u ANTHROPIC_AUTH_TOKEN \
  ANTHROPIC_BASE_URL=http://127.0.0.1:3000 \
  ANTHROPIC_API_KEY="$NEWAPI_KEY" \
  claude --bare --setting-sources local -p \
  --model kimi-k2p5 \
  --permission-mode bypassPermissions \
  --output-format stream-json \
  --verbose \
  --max-turns 3 \
  "读取 /Volumes/Work/code/firew2oai/internal/proxy/chat_compat.go。只用中文一句话回答：normalizeChatTools 的作用是什么。不要读取其他文件。"
```

复杂读文件任务：

```bash
env -u ANTHROPIC_AUTH_TOKEN \
  ANTHROPIC_BASE_URL=http://127.0.0.1:3000 \
  ANTHROPIC_API_KEY="$NEWAPI_KEY" \
  claude --bare --setting-sources local -p \
  --model kimi-k2p5 \
  --permission-mode bypassPermissions \
  --output-format stream-json \
  --verbose \
  --max-turns 4 \
  "先读取 /Volumes/Work/code/firew2oai/internal/proxy/chat_compat.go 和 /Volumes/Work/code/firew2oai/internal/proxy/proxy.go，再用中文一句话说明 chat tool_calls 是如何被转换的。不要猜，必须基于读取结果回答。"
```

## 结果

| 场景 | 结果 | 事实 |
| --- | --- | --- |
| 最小文本任务 | 通过 | 1 轮完成，返回 `ok` |
| 单文件工具调用 | 通过 | 触发 `Read`，2 轮内收束并给出基于文件内容的答案 |
| 跨文件复杂工具调用 | 未通过 | 触发多次 `Read`，但 4 轮后仍停在“还需要继续读取”，没有收束成最终答案 |

## 日志证据

new-api：

- 命中 `POST /v1/messages?beta=true`
- `request_conversion=["Claude Messages","OpenAI Compatible"]`
- `channel_id=106`
- 出现 `POST /v1/messages/count_tokens?beta=true`，当前返回 `404`

firew2oai：

- 命中 `POST /v1/chat/completions`
- `model=kimi-k2p5`
- `tools_present=true`
- 复杂任务出现 `messages=2 -> 4 -> 6 -> 8` 的多轮工具回灌

## 结论

- `Claude Code -> new-api -> firew2oai` 的主请求链路已打通，基础问答可用。
- 简单单文件 `Read` 工具调用可用。
- 复杂多轮读文件任务在 `kimi-k2p5` 下仍不稳定，当前不能视为真实 Agent 任务可用。
- 若追求更完整的 Claude Code 兼容，`new-api` 仍建议补 `/v1/messages/count_tokens`。
