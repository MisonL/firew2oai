# Repository Guidelines

## Project Structure & Module Organization
`cmd/server/main.go` 是唯一可执行入口。核心代码位于 `internal/`：`config` 负责参数与环境变量，`proxy` 实现 OpenAI 兼容接口，`transport` 处理上游 HTTP 访问，`tokenauth` 和 `whitelist` 负责访问控制。测试文件与被测包同目录，命名为 `*_test.go`。部署相关文件包括 `Dockerfile`、`docker-compose.yml`，构建产物输出到 `bin/`。设计、审计和阶段性说明放入 `docs/`。

## Build, Test, and Development Commands
优先使用 `Makefile` 中的命令：

- `make build`：编译 `./cmd/server`，生成 `bin/firew2oai`。
- `make run`：先构建，再启动本地服务。
- `make test`：执行 `go test -v -race ./...`。
- `make lint`：执行 `golangci-lint run ./...`。
- `make build-all`：交叉编译 Linux、macOS、Windows 产物。
- `make docker-up`：通过 Docker Compose 启动服务；`make docker-down` 停止服务。

本地快速确认可在 `make build` 后运行 `./bin/firew2oai --help`。

## Coding Style & Naming Conventions
遵循标准 Go 风格，提交前使用 `gofmt`，保持导入有序。包名使用简短小写形式，例如 `proxy`、`transport`。导出标识符使用 `CamelCase`，非导出函数和变量使用 `camelCase`。处理器和中间件应保持单一职责，配置应显式传入，避免隐藏默认行为。日志继续使用现有 `log/slog` 结构化风格。

## Testing Guidelines
新增测试应放在被测包同目录，文件名使用 `*_test.go`，测试函数使用 `TestXxx`，基准测试使用 `BenchmarkXxx`。认证、代理转换、上游传输和 IP 过滤逻辑应覆盖成功路径与失败路径。提交 PR 前至少运行 `make test`；涉及行为或接口变化时，同时运行 `make lint`。

## Commit & Pull Request Guidelines
现有提交历史采用简短的约定式标题，例如 `feat: ...`、`docs: ...`。每个提交只处理一个明确变更。PR 描述应说明用户可见影响、列出已运行的验证命令，并明确配置项或 API 行为变化。若修改请求或响应格式，应附上示例 `curl` 或关键响应片段。

## Security & Configuration Tips
不要提交真实 API Key、令牌 JSON 或本地私密配置。开发环境优先使用 `API_KEY`、`PORT`、`CORS_ORIGINS`、`IP_WHITELIST` 等环境变量。若为测试临时放宽 CORS 或 IP 白名单，必须在 PR 中说明，便于审查时确认不会误带到生产配置。

## Codex Tooling Rules
当通过 Codex 执行仓库任务时，必须优先遵守当前会话已声明的工具与 schema，不得臆造工具名。

- 读取文件、搜索文本、查看目录时，只能使用已声明工具；若当前会话只有 `exec_command`，则统一通过 `exec_command` 执行 `sed`、`nl`、`rg`、`ls` 等只读命令。
- 调用结构化工具时，只能输出单个 JSON 对象，不得混入解释文字、Markdown 代码块或连续多个 JSON 对象。
- `exec_command` 必须使用结构化 `function_call` 形式，`arguments` 必须是 JSON 对象，且必须包含字符串字段 `cmd`。禁止改写为 `custom_tool_call`，禁止使用 `command`、`path`、`file_path` 等替代字段。
- 禁止使用未声明工具名，例如 `read_file`、`Read`、`list_files`、`run_command`、`run_terminal_cmd`。如果需要这些能力，必须改写为当前会话内可用工具。
- 收到工具错误或 `Codex adapter error` 后，先修正同一工具调用的名称和参数，再继续任务；不要向用户反问，不要把错误当成最终答案。
- 在复杂任务中，先执行必要的只读工具调用获取证据，再输出结论；禁止编造文件内容、行号或审计结论。

## Codex Usage Notes
当前仓库对 Codex 的主适配目标是 `/v1/responses`，新增协议兼容改动、回归测试和真实链路验证都以该接口为准。

- 只读型 Coding 审计任务当前已经稳定，真实写代码任务不能默认假设“所有模型都稳定可用”。
- 2026-04-20 的最新矩阵结论见 [docs/reviews/CR-CODEX-MODEL-MATRIX-2026-04-20.md](/Volumes/Work/code/firew2oai/docs/reviews/CR-CODEX-MODEL-MATRIX-2026-04-20.md)；当前应以该文档和 `README.md` 为准，不要沿用更早的口头结论。
- 走 `new-api -> firew2oai` 正式链路时，如同模型存在多渠道，必须先确认 `firew2oai` 渠道优先级最高，否则测试结果不能代表本项目适配效果。
- 与 Codex 兼容性直接相关的高风险边界仍集中在 `apply_patch`、`tool_choice`、四行收口和 finalize 收口稳定性，修改这些路径后必须补对应回归测试。
- 如果模型未按协议产出工具块、错误把自述文本写进 `FILES`/`NOTE`、或在 finalize 阶段漂移，优先视为模型能力或协议收口问题，先查 `internal/proxy/` 下的协议适配与测试，再下结论。
