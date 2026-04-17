# firew2oai CSE 审计文档

## Control Contract v2

| 维度 | 内容 |
|---|---|
| **Primary Setpoint** | 所有已发现问题的误差归零：编译、安全、数据完整性、可靠性、项目工程规范 |
| **Acceptance** | 5 平台编译 clean、go vet clean、0 linter errors、单元测试覆盖核心逻辑、Dockerfile 可构建 |
| **Guardrail Metrics** | 不引入第三方依赖、不破坏 OpenAI 兼容性协议、不增大二进制体积超过 2x |
| **Sampling Plan** | go build + go vet × 5 平台；go test -race -cover；make docker-build |
| **Recovery Target** | git stash/reset 即可回退 |
| **Rollback Trigger** | 编译失败或测试 panic |
| **Constraints** | 纯标准库、Go 1.25+、OpenAI API 兼容 |
| **Boundary** | 仅修改 Go 源码 + Makefile + Dockerfile + 配置文件 |

## 发现问题总计: 21 个

### P0 (5 个) — 安全 / 数据完整性
1. bin/ 目录 ~35MB 二进制已提交到 git
2. 零测试覆盖（无 *_test.go）
3. extractIP 信任 X-Forwarded-For 导致 SSRF/限流绕过
4. non-stream 模式 scanner.Err() 后直接 return 可能丢弃已有内容
5. json.Marshal 错误被静默忽略（writeJSON/sseChunk/writeError）

### P1 (7 个) — 可靠性 / 正确性
6. Limiter.cleanupLoop goroutine 泄漏（无 Stop 机制）
7. responseWriter 不实现 http.Flusher（SSE 流式写入中断）
8. ValidModel 线性扫描 O(n)
9. non-stream 空结果边界条件
10. generateRequestID 忽略 rand.Read error
11. Config.Load 静默忽略无效环境变量
12. 空目录 scripts/ 和 internal/middleware/ 已提交

### P2 (9 个) — 工程规范 / 可维护性
13-21: README 与实现不一致、Dockerfile 版本硬编码、docker-compose 不完整等

## 修复状态
- [x] Batch 1: P0 — bin/清理、测试覆盖(53+ tests)、TrustedProxyCount、scanner.Err()保留内容、json.Marshal错误处理
- [x] Batch 2: P1 — rateLimiter.Stop()、responseWriter.Unwrap()、ValidModel O(1)、空结果处理、rand.Read fallback、env校验、空目录清理
- [x] Batch 3: P2 — README同步、Dockerfile多阶段、docker-compose完整、.env.example、.dockerignore、LICENSE、非root容器、日志限制
- [x] L0/L1/L2 验证 — go vet clean、go test -race 53+ tests pass、5平台交叉编译clean

## 额外优化（代码审查）
- [x] W1: flag.Parse 与 go test 冲突 — 分离为 ApplyFlags()
- [x] W2: Authenticate() 伪恒定时间比较 — 移除，使用 map 查找
- [x] W3: SetGlobalRateLimit() 死代码 — 删除
- [x] W4: /v1/models 不限制 HTTP 方法 — 限制 GET
- [x] W5: cogito-671b-v2-p1 已下线 — 从 AvailableModels 移除
- [x] W6: temperature/max_tokens 透传 — 添加到 FireworksRequest
- [x] C1: Trusted Proxy 配置 — 添加 TrustedProxyCount
- [x] C2: JSON 注入修复 — 使用 authErrorResponse 结构体
- [x] C3: 错误信息脱敏 — 不暴露 err.Error() 给客户端
- [x] C4: WriteTimeout — 基于 TIMEOUT 配置
- [x] S1: ValidModel map — O(1) 查找
- [x] S2: 日志增强 — token 指纹
- [x] S3: UA 配置化 — CHROME_USER_AGENT 环境变量
- [x] S4: SSE 解析重构 — scanSSEEvents() 消除重复
- [x] D1: LICENSE — MIT
- [x] D2: .dockerignore
- [x] D3: .env.example
- [x] D4: Docker 非 root — appuser
- [x] D5: Docker 日志限制 — 10MB × 3

## 2026-04-17 修复记录

### 问题发现
- **kimi 模型 502 错误**: Fireworks 上游对部分 kimi 模型返回 404
- **原因**: Fireworks 平台已下架 kimi-k2-thinking、kimi-k2-instruct-0905
- **状态**: kimi-k2p5 间歇性恢复可用，minimax-m2p1 新发现不可用
- **更新**: README 已更新可用模型列表

### 当前可用模型（11个）
✅ qwen3-vl-30b-a3b-thinking, qwen3-vl-30b-a3b-instruct, qwen3-8b  
✅ minimax-m2p5, llama-v3p3-70b-instruct, kimi-k2p5（间歇性）  
✅ gpt-oss-20b, gpt-oss-120b, glm-5, glm-4p7  
✅ deepseek-v3p2, deepseek-v3p1  

### 不可用模型（4个）
❌ minimax-m2p1, kimi-k2-thinking, kimi-k2-instruct-0905, cogito-671b-v2-p1

### 架构优化（CSE 弹性设计）
- **Authorization 转发**: transport 层现在正确转发客户端 Authorization header 到上游
- **弹性 SSE 处理**: 上游返回内容但没有 `done` 事件时，仍返回 200 而非 502
- **错误事件处理**: 正确识别并处理 Fireworks 返回的 `type=error` SSE 事件

### 兼容性验证
- ✅ 直连模式（Codex / OpenAI 客户端）
- ✅ 中转模式（New API / One API）
- ✅ 流式 + 非流式响应
- ✅ 11 个可用模型通过测试
