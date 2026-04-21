from dataclasses import dataclass
from pathlib import Path


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
    ),
)


def write_text(path: Path, content: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content, encoding="utf-8")


def prepare_fixture(worktree: Path, scenario_name: str) -> None:
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
