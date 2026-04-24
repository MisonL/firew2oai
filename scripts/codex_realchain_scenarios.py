from dataclasses import dataclass
from pathlib import Path
import struct
import zlib


MODELS = [
    "qwen3-vl-30b-a3b-instruct",
    "minimax-m2p5",
    "llama-v3p3-70b-instruct",
    "kimi-k2p5",
    "qwen3-vl-30b-a3b-thinking",
    "gpt-oss-20b",
    "glm-5",
    "qwen3-8b",
    "glm-4p7",
    "gpt-oss-120b",
    "deepseek-v3p1",
    "deepseek-v3p2",
]


@dataclass(frozen=True)
class Scenario:
    name: str
    prompt: str
    expected_operations: tuple[str, ...]
    expected_files: tuple[str, ...]
    capabilities: tuple[str, ...]
    required_tools: tuple[str, ...] = ()
    expected_signals: tuple[str, ...] = ()
    expected_final_substrings: tuple[str, ...] = ()
    expect_clean_diff: bool = False


SCENARIOS = (
    Scenario(
        name="readonly_audit",
        prompt=(
            "你是资深 Go 工程师。请在当前仓库完成一个真实 Coding 只读核验任务：\n"
            "1) 阅读 internal/proxy/output_constraints.go。\n"
            "2) 阅读 internal/proxy/execution_evidence.go。\n"
            "3) 执行 go test ./internal/proxy。\n"
            "4) 执行 go test ./...。\n"
            "5) 不要修改任何文件。\n"
            "最后只输出四行：RESULT: PASS 或 FAIL；FILES: 你读取的文件；TEST: 测试结果；NOTE: 你完成的核验动作。"
        ),
        expected_operations=(
            "internal/proxy/output_constraints.go",
            "internal/proxy/execution_evidence.go",
            "go test ./internal/proxy",
            "go test ./...",
        ),
        expected_files=(),
        capabilities=("read", "package_test", "repo_test", "no_write", "structured_final"),
        required_tools=("exec_command",),
        expect_clean_diff=True,
    ),
    Scenario(
        name="add_test_file",
        prompt=(
            "你是资深 Go 工程师。请在当前仓库完成一个真实但边界清晰的测试补强任务：\n"
            "1) 阅读 internal/proxy/output_constraints.go 与现有 internal/proxy/*_test.go 风格。\n"
            "2) 新增文件 internal/proxy/output_constraints_test.go，添加测试 TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise。\n"
            "3) 该测试需要断言 sanitizeRequiredLabelValue(\"CONSTRAINT\", \"Chunk ID: 123 Wall time: 0.000 seconds Process exited with code 0 Output: package proxy ...\") 返回空字符串。\n"
            "4) 执行 go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'。\n"
            "最后只输出四行：RESULT: PASS 或 FAIL；FILES: 你新增或修改的文件；TEST: 测试结果；NOTE: 你完成的补强动作。"
        ),
        expected_operations=(
            "internal/proxy/output_constraints.go",
            "go test ./internal/proxy -run 'TestSanitizeRequiredLabelValue_RejectsToolWrapperNoise'",
        ),
        expected_files=("internal/proxy/output_constraints_test.go",),
        capabilities=("read", "style_inspection", "create_file", "targeted_test", "structured_final"),
        required_tools=("exec_command",),
    ),
    Scenario(
        name="fix_existing_bug",
        prompt=(
            "你是资深 Go 工程师。请修复一个真实但边界清晰的现有 bug：\n"
            "1) 阅读 internal/codexfixture/bugfix/port.go 与 internal/codexfixture/bugfix/port_test.go。\n"
            "2) 修复现有文件 internal/codexfixture/bugfix/port.go，把正常分支里的 `return port + 1` 改为 `return port`，使全部测试通过。\n"
            "3) 不要改包路径，不要新增无关文件。\n"
            "4) 执行 go test ./internal/codexfixture/bugfix。\n"
            "最后只输出四行：RESULT: PASS 或 FAIL；FILES: 你新增或修改的文件；TEST: 测试结果；NOTE: 你完成的修复动作。"
        ),
        expected_operations=(
            "internal/codexfixture/bugfix/port.go",
            "internal/codexfixture/bugfix/port_test.go",
            "go test ./internal/codexfixture/bugfix",
        ),
        expected_files=("internal/codexfixture/bugfix/port.go",),
        capabilities=("read", "edit_existing", "targeted_test", "structured_final"),
        required_tools=("exec_command",),
    ),
    Scenario(
        name="search_and_patch",
        prompt=(
            "你是资深 Go 工程师。请完成一个需要先搜索再修复的真实 Coding 任务：\n"
            "1) 先执行命令：rg -n \"BuildTicketSummary|NormalizeTitle\" internal/codexfixture/searchfix。\n"
            "2) 阅读 internal/codexfixture/searchfix/summary.go 与 internal/codexfixture/searchfix/summary_test.go。\n"
            "3) 修改现有文件 internal/codexfixture/searchfix/summary.go，让 BuildTicketSummary 对 title 执行 strings.TrimSpace + strings.ToUpper，对 body 执行 strings.TrimSpace。\n"
            "4) 不要新增文件。\n"
            "5) 执行 go test ./internal/codexfixture/searchfix。\n"
            "最后只输出四行：RESULT: PASS 或 FAIL；FILES: 你新增或修改的文件；TEST: 测试结果；NOTE: 你完成的搜索与修复动作。"
        ),
        expected_operations=(
            "rg -n \"BuildTicketSummary|NormalizeTitle\" internal/codexfixture/searchfix",
            "internal/codexfixture/searchfix/summary.go",
            "internal/codexfixture/searchfix/summary_test.go",
            "go test ./internal/codexfixture/searchfix",
        ),
        expected_files=("internal/codexfixture/searchfix/summary.go",),
        capabilities=("repo_search", "read", "edit_existing", "targeted_test", "structured_final"),
        required_tools=("exec_command",),
    ),
    Scenario(
        name="cross_file_feature",
        prompt=(
            "你是资深 Go 工程师。请完成一个小型跨文件 Coding 任务：\n"
            "1) 阅读 internal/codexfixture/feature/formatter.go 与 internal/codexfixture/feature/formatter_test.go。\n"
            "2) 新增文件 internal/codexfixture/feature/title.go，提供 normalizeTitle 帮助函数。\n"
            "3) 修改现有文件 internal/codexfixture/feature/formatter.go，让 BuildSummary 正确规范化 title 并裁剪 body。\n"
            "4) 修改现有文件 internal/codexfixture/feature/formatter_test.go，追加一个空 body 场景。\n"
            "5) 执行 go test ./internal/codexfixture/feature。\n"
            "最后只输出四行：RESULT: PASS 或 FAIL；FILES: 你新增或修改的文件；TEST: 测试结果；NOTE: 你完成的跨文件动作。"
        ),
        expected_operations=(
            "internal/codexfixture/feature/formatter.go",
            "internal/codexfixture/feature/formatter_test.go",
            "go test ./internal/codexfixture/feature",
        ),
        expected_files=(
            "internal/codexfixture/feature/title.go",
            "internal/codexfixture/feature/formatter.go",
            "internal/codexfixture/feature/formatter_test.go",
        ),
        capabilities=("create_file", "edit_existing", "cross_file", "update_tests", "targeted_test", "structured_final"),
        required_tools=("exec_command",),
    ),
    Scenario(
        name="plan_then_read",
        prompt=(
            "你是测试代理。请在当前仓库完成一个计划驱动的只读任务：\n"
            "1) 必须先调用 update_plan。\n"
            "2) update_plan 的 arguments 顶层字段必须叫 plan，不允许使用 steps。\n"
            "3) plan 里只写两个步骤：Inspect README.md、Reply with summary。\n"
            "4) 然后必须使用 exec_command 执行 `head -n 3 README.md`。\n"
            "5) 不要修改任何文件。\n"
            "最后只输出四行，不要有任何额外内容：\n"
            "RESULT: PASS 或 FAIL\n"
            "FILES: 你读取的文件\n"
            "TEST: N/A\n"
            "NOTE: 你是否先成功调用了 update_plan"
        ),
        expected_operations=("README.md",),
        expected_files=(),
        capabilities=("planning", "read", "structured_final"),
        required_tools=("update_plan", "exec_command"),
        expected_signals=("update_plan", "command_execution"),
        expect_clean_diff=True,
    ),
    Scenario(
        name="interactive_shell_session",
        prompt=(
            "你是测试代理。请验证交互式 shell 会话能力：\n"
            "1) 必须使用 exec_command 启动一个交互式 python3 会话。\n"
            "2) 必须使用 write_stdin 向同一 session 发送 print(2 + 3) 与 exit()。\n"
            "3) 禁止使用 python3 -c、here-doc 或一次性命令替代 write_stdin。\n"
            "4) 不要修改任何文件。\n"
            "最后只输出四行：RESULT: PASS 或 FAIL；FILES: none；TEST: N/A；NOTE: 交互式会话结果。"
        ),
        expected_operations=("python3",),
        expected_files=(),
        capabilities=("exec_command", "write_stdin", "interactive_session", "structured_final"),
        required_tools=("exec_command", "write_stdin"),
        expected_signals=("write_stdin", "command_execution"),
        expect_clean_diff=True,
    ),
    Scenario(
        name="js_repl_roundtrip",
        prompt=(
            "你是测试代理。请验证 js_repl：\n"
            "1) 必须先使用 js_repl 计算数组 [2,3,5] 的和。\n"
            "2) 然后调用 js_repl_reset。\n"
            "3) 再次使用 js_repl 计算 7 * 8。\n"
            "4) 不要使用 exec_command 代替 js_repl。\n"
            "5) 不要修改任何文件。\n"
            "最后只输出四行：RESULT: PASS 或 FAIL；FILES: none；TEST: N/A；NOTE: js_repl 两次计算结果。"
        ),
        expected_operations=(),
        expected_files=(),
        capabilities=("js_repl", "js_repl_reset", "structured_final"),
        required_tools=("js_repl", "js_repl_reset"),
        expected_signals=("js_repl", "js_repl_reset"),
        expect_clean_diff=True,
    ),
    Scenario(
        name="web_search_probe",
        prompt=(
            "你是测试代理。请验证 web_search：\n"
            "1) 必须使用 web_search 查询 Go 官方最新稳定版本与发布日期。\n"
            "2) 禁止使用 exec_command、docfork 或其他工具代替 web_search。\n"
            "3) web_search 返回后，必须直接用四行格式收口，不要输出前言或解释工具行为。\n"
            "4) 不要修改任何文件。\n"
            "最后只输出四行：RESULT: PASS 或 FAIL；FILES: none；TEST: N/A；NOTE: 版本号与日期。"
        ),
        expected_operations=(),
        expected_files=(),
        capabilities=("web_search", "structured_final"),
        required_tools=("web_search",),
        expected_signals=("web_search",),
        expect_clean_diff=True,
    ),
    Scenario(
        name="docfork_probe",
        prompt=(
            "你是测试代理。请验证 Docfork MCP：\n"
            "1) 必须使用 mcp__docfork__search_docs 搜索 react 文档中的 useEffectEvent。\n"
            "2) 必须再使用 mcp__docfork__fetch_doc 获取相关文档内容。\n"
            "3) 禁止使用 web_search 代替 Docfork。\n"
            "4) 不要修改任何文件。\n"
            "最后只输出四行：RESULT: PASS 或 FAIL；FILES: none；TEST: N/A；NOTE: 你从文档中得到的一句结论。"
        ),
        expected_operations=(),
        expected_files=(),
        capabilities=("mcp_docfork", "structured_final"),
        required_tools=("mcp__docfork__search_docs", "mcp__docfork__fetch_doc"),
        expected_signals=("docfork", "search_docs", "fetch_doc"),
        expect_clean_diff=True,
    ),
    Scenario(
        name="apply_patch_probe",
        prompt=(
            "你是测试代理。请验证 apply_patch：\n"
            "1) 先读取 internal/codexfixture/patchprobe/message.txt。\n"
            "2) 必须使用 apply_patch，把文件中的 alpha 改为 beta。\n"
            "3) 再读取同一文件确认内容已经变成 beta。\n"
            "4) 最后只输出四行：RESULT: PASS 或 FAIL；FILES: internal/codexfixture/patchprobe/message.txt；TEST: N/A；NOTE: 修改后的内容。"
        ),
        expected_operations=("internal/codexfixture/patchprobe/message.txt",),
        expected_files=("internal/codexfixture/patchprobe/message.txt",),
        capabilities=("read", "apply_patch", "structured_final"),
        required_tools=("exec_command", "apply_patch"),
        expected_signals=("apply_patch",),
    ),
    Scenario(
        name="chrome_devtools_probe",
        prompt=(
            "你是测试代理。请验证 Chrome DevTools MCP：\n"
            "1) 必须使用 mcp__chrome_devtools__new_page 打开这个 data URL："
            "data:text/html,%3Cbutton%20id%3D%22go%22%20onclick%3D%22document.getElementById('status').textContent%3D'clicked'%22%3EGo%3C%2Fbutton%3E%3Cdiv%20id%3D%22status%22%3Eidle%3C%2Fdiv%3E\n"
            "2) 然后必须使用 mcp__chrome_devtools__take_snapshot。\n"
            "3) 再必须使用 mcp__chrome_devtools__click 点击按钮。\n"
            "4) 最后必须使用 mcp__chrome_devtools__wait_for 等待页面出现 clicked。\n"
            "5) 禁止使用 exec_command 代替浏览器工具。\n"
            "6) 不要修改任何文件。\n"
            "最后只输出四行：RESULT: PASS 或 FAIL；FILES: none；TEST: N/A；NOTE: 页面最终状态。"
        ),
        expected_operations=(),
        expected_files=(),
        capabilities=("mcp_chrome_devtools", "structured_final"),
        required_tools=(
            "mcp__chrome_devtools__new_page",
            "mcp__chrome_devtools__take_snapshot",
            "mcp__chrome_devtools__click",
            "mcp__chrome_devtools__wait_for",
        ),
        expected_signals=("chrome_devtools", "new_page", "take_snapshot", "click", "wait_for"),
        expect_clean_diff=True,
    ),
    Scenario(
        name="view_image_probe",
        prompt=(
            "你是测试代理。请验证 view_image：\n"
            "1) 必须先使用 exec_command 执行 `pwd`，读取当前工作目录绝对路径。\n"
            "2) 然后必须使用 view_image 查看 internal/codexfixture/assets/red.png。\n"
            "3) 不要修改任何文件。\n"
            "最后只输出四行，不要有任何额外内容：\n"
            "RESULT: PASS 或 FAIL\n"
            "FILES: internal/codexfixture/assets/red.png\n"
            "TEST: N/A\n"
            "NOTE: 图片主颜色"
        ),
        expected_operations=("pwd",),
        expected_files=(),
        capabilities=("view_image", "read", "structured_final"),
        required_tools=("exec_command", "view_image"),
        expected_signals=("view_image",),
        expect_clean_diff=True,
    ),
    Scenario(
        name="subagent_probe",
        prompt=(
            "你是测试代理。用户明确要求你使用子代理。请验证子代理工具链：\n"
            "1) 必须使用 spawn_agent 启动一个子代理。\n"
            "2) 子代理任务是读取 README.md 第一行并返回结果。\n"
            "3) 必须使用 wait_agent 等待结果。\n"
            "4) 必须使用 close_agent 关闭子代理。\n"
            "5) 不要修改任何文件。\n"
            "最后只输出四行，不要有任何额外内容：\n"
            "RESULT: PASS 或 FAIL\n"
            "FILES: README.md\n"
            "TEST: N/A\n"
            "NOTE: 子代理返回的第一行内容"
        ),
        expected_operations=(),
        expected_files=(),
        capabilities=("spawn_agent", "wait_agent", "close_agent", "structured_final"),
        required_tools=("spawn_agent", "wait_agent", "close_agent"),
        expected_signals=("spawn_agent", "wait_agent", "close_agent"),
        expected_final_substrings=("# firew2oai",),
        expect_clean_diff=True,
    ),
    Scenario(
        name="mcp_resource_listing_probe",
        prompt=(
            "你是测试代理。请验证通用 MCP 资源发现接口：\n"
            "1) 必须调用 list_mcp_resources。\n"
            "2) 必须调用 list_mcp_resource_templates。\n"
            "3) 如果结果为空就如实说明为空，不要虚构资源。\n"
            "4) 不要修改任何文件。\n"
            "最后只输出四行：RESULT: PASS 或 FAIL；FILES: none；TEST: N/A；NOTE: 当前资源与模板是否为空。"
        ),
        expected_operations=(),
        expected_files=(),
        capabilities=("mcp_resources", "structured_final"),
        required_tools=("list_mcp_resources", "list_mcp_resource_templates"),
        expected_signals=("list_mcp_resources", "list_mcp_resource_templates"),
        expect_clean_diff=True,
    ),
)


def write_text(path: Path, content: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content, encoding="utf-8")


def write_png(path: Path, width: int, height: int, rgb: tuple[int, int, int]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    raw = bytearray()
    row = bytes(rgb) * width
    for _ in range(height):
        raw.append(0)
        raw.extend(row)

    def chunk(tag: bytes, data: bytes) -> bytes:
        return (
            struct.pack(">I", len(data))
            + tag
            + data
            + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF)
        )

    png = bytearray(b"\x89PNG\r\n\x1a\n")
    png.extend(chunk(b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)))
    png.extend(chunk(b"IDAT", zlib.compress(bytes(raw), level=9)))
    png.extend(chunk(b"IEND", b""))
    path.write_bytes(bytes(png))


def prepare_fixture(worktree: Path, scenario_name: str) -> None:
    if scenario_name == "view_image_probe":
        write_png(worktree / "internal/codexfixture/assets/red.png", 24, 24, (255, 0, 0))
    if scenario_name == "apply_patch_probe":
        write_text(worktree / "internal/codexfixture/patchprobe/message.txt", "alpha\n")
    if scenario_name == "fix_existing_bug":
        write_text(
            worktree / "internal/codexfixture/bugfix/port.go",
            """package bugfix

func ClampPort(port int) int {
\tif port < 0 {
\t\treturn 0
\t}
\tif port > 65535 {
\t\treturn 65535
\t}
\treturn port + 1
}
""",
        )
        write_text(
            worktree / "internal/codexfixture/bugfix/port_test.go",
            """package bugfix

import "testing"

func TestClampPort(t *testing.T) {
\ttests := []struct {
\t\tname string
\t\tin   int
\t\twant int
\t}{
\t\t{name: "negative", in: -3, want: 0},
\t\t{name: "normal", in: 80, want: 80},
\t\t{name: "upper bound", in: 70000, want: 65535},
\t}

\tfor _, tt := range tests {
\t\tt.Run(tt.name, func(t *testing.T) {
\t\t\tif got := ClampPort(tt.in); got != tt.want {
\t\t\t\tt.Fatalf("ClampPort(%d) = %d, want %d", tt.in, got, tt.want)
\t\t\t}
\t\t})
\t}
}
""",
        )
    if scenario_name == "cross_file_feature":
        write_text(
            worktree / "internal/codexfixture/feature/formatter.go",
            """package feature

func BuildSummary(title, body string) string {
\treturn title + ": " + body
}
""",
        )
        write_text(
            worktree / "internal/codexfixture/feature/formatter_test.go",
            """package feature

import "testing"

func TestBuildSummary_NormalizesTitleAndBody(t *testing.T) {
\tgot := BuildSummary("  firew2oai  ", " adapter ")
\twant := "FIREW2OAI: adapter"
\tif got != want {
\t\tt.Fatalf("BuildSummary() = %q, want %q", got, want)
\t}
}
""",
        )
    if scenario_name == "search_and_patch":
        write_text(
            worktree / "internal/codexfixture/searchfix/summary.go",
            """package searchfix

func BuildTicketSummary(title, body string) string {
\treturn title + ": " + body
}
""",
        )
        write_text(
            worktree / "internal/codexfixture/searchfix/summary_test.go",
            """package searchfix

import "testing"

func TestBuildTicketSummary_TrimsAndUppercases(t *testing.T) {
\tgot := BuildTicketSummary("  firew2oai  ", " adapter ")
\twant := "FIREW2OAI: adapter"
\tif got != want {
\t\tt.Fatalf("BuildTicketSummary() = %q, want %q", got, want)
\t}
}
""",
        )
