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
- [ ] Batch 1: P0
- [ ] Batch 2: P1
- [ ] Batch 3: P2
- [ ] L0/L1/L2 验证
