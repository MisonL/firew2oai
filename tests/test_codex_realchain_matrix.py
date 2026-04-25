import json
import subprocess
import sys
import tempfile
from pathlib import Path
from unittest import TestCase
from unittest.mock import patch


REPO_ROOT = Path(__file__).resolve().parents[1]
SCRIPTS_DIR = REPO_ROOT / "scripts"
if str(SCRIPTS_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPTS_DIR))

import codex_realchain_matrix as matrix


class FakePopen:
    def __init__(self, timeout_on_first_communicate: bool = False, wait_times_out: bool = False) -> None:
        self.pid = 12345
        self.returncode = 0
        self.communicate_input = None
        self.communicate_count = 0
        self.timeout_on_first_communicate = timeout_on_first_communicate
        self.wait_times_out = wait_times_out

    def communicate(self, input=None, timeout=None):
        self.communicate_input = input
        self.communicate_count += 1
        if self.timeout_on_first_communicate and self.communicate_count == 1:
            raise subprocess.TimeoutExpired(cmd=["codex"], timeout=timeout, output=b"partial out", stderr=b"partial err")
        return "", ""

    def wait(self, timeout=None):
        if self.wait_times_out:
            raise subprocess.TimeoutExpired(cmd=["codex"], timeout=timeout)
        self.returncode = -15
        return self.returncode


class RunHelperTests(TestCase):
    def test_run_closes_stdin_by_default(self) -> None:
        fake_proc = FakePopen()
        with patch("subprocess.Popen", return_value=fake_proc) as mock_popen:
            matrix.run(["codex", "exec", "hello"], cwd=REPO_ROOT, timeout=30)

        _, kwargs = mock_popen.call_args
        self.assertIs(kwargs.get("stdin"), subprocess.DEVNULL)
        self.assertTrue(kwargs.get("start_new_session"))

    def test_run_passes_input_text_without_explicit_stdin(self) -> None:
        fake_proc = FakePopen()
        with patch("subprocess.Popen", return_value=fake_proc) as mock_popen:
            matrix.run(["git", "apply", "-"], cwd=REPO_ROOT, timeout=30, input_text="patch")

        _, kwargs = mock_popen.call_args
        self.assertIs(kwargs.get("stdin"), subprocess.PIPE)
        self.assertEqual(fake_proc.communicate_input, "patch")

    def test_run_kills_process_group_on_timeout(self) -> None:
        fake_proc = FakePopen(timeout_on_first_communicate=True, wait_times_out=True)
        with patch("subprocess.Popen", return_value=fake_proc), patch("os.killpg") as mock_killpg:
            result = matrix.run(["codex", "exec", "slow"], cwd=REPO_ROOT, timeout=1)

        self.assertEqual(result.returncode, -9)
        self.assertIn("partial out", result.stdout)
        self.assertIn("partial err", result.stderr)
        self.assertIn("timeout after 1s", result.stderr)
        self.assertEqual(mock_killpg.call_count, 2)

    def test_resolve_codex_executable_uses_env_override(self) -> None:
        with patch.dict("os.environ", {"CODEX_MATRIX_CODEX_BIN": "/tmp/codex"}, clear=False):
            self.assertEqual(matrix.resolve_codex_executable(), "/tmp/codex")

    def test_configure_codex_home_sets_relative_home_under_output_dir(self) -> None:
        output_dir = Path(tempfile.mkdtemp(prefix="matrix-output-"))
        self.addCleanup(lambda: __import__("shutil").rmtree(output_dir, ignore_errors=True))
        with patch.dict("os.environ", {"CODEX_MATRIX_CODEX_HOME": "codex-home"}, clear=True):
            matrix.configure_codex_home(output_dir)

            self.assertEqual(Path(__import__("os").environ["CODEX_HOME"]), output_dir / "codex-home")
            self.assertTrue((output_dir / "codex-home").is_dir())

    def test_configure_child_tool_environment_sets_safe_go_cache(self) -> None:
        with patch.dict("os.environ", {}, clear=True):
            matrix.configure_child_tool_environment()

            go_cache = Path(__import__("os").environ["GOCACHE"])
            npm_cache = Path(__import__("os").environ["NPM_CONFIG_CACHE"])
            self.assertEqual(go_cache, Path(tempfile.gettempdir()) / "firew2oai-go-cache")
            self.assertEqual(npm_cache, Path(tempfile.gettempdir()) / "firew2oai-npm-cache")
            self.assertTrue(go_cache.is_dir())
            self.assertTrue(npm_cache.is_dir())

    def test_configure_child_tool_environment_honors_matrix_override(self) -> None:
        go_cache = Path(tempfile.mkdtemp(prefix="matrix-go-cache-")) / "cache"
        npm_cache = Path(tempfile.mkdtemp(prefix="matrix-npm-cache-")) / "cache"
        self.addCleanup(lambda: __import__("shutil").rmtree(go_cache.parent, ignore_errors=True))
        self.addCleanup(lambda: __import__("shutil").rmtree(npm_cache.parent, ignore_errors=True))

        with patch.dict(
            "os.environ",
            {"CODEX_MATRIX_GOCACHE": str(go_cache), "CODEX_MATRIX_NPM_CACHE": str(npm_cache)},
            clear=True,
        ):
            matrix.configure_child_tool_environment()

            self.assertEqual(Path(__import__("os").environ["GOCACHE"]), go_cache)
            self.assertEqual(Path(__import__("os").environ["NPM_CONFIG_CACHE"]), npm_cache)
            self.assertTrue(go_cache.is_dir())
            self.assertTrue(npm_cache.is_dir())

    def test_filter_items_allows_explicit_external_model_names(self) -> None:
        with patch.dict(
            "os.environ",
            {
                "CODEX_MATRIX_MODELS": "Qwen/Qwen3-8B,openai/gpt-oss-120b:free",
            },
            clear=False,
        ):
            self.assertEqual(
                matrix.filter_items(matrix.MODELS, "CODEX_MATRIX_MODELS"),
                ["Qwen/Qwen3-8B", "openai/gpt-oss-120b:free"],
            )

    def test_filter_items_keeps_requested_scenario_order(self) -> None:
        with patch.dict(
            "os.environ",
            {
                "CODEX_MATRIX_SCENARIOS": "web_search_probe,readonly_audit",
            },
            clear=False,
        ):
            self.assertEqual(
                [scenario.name for scenario in matrix.filter_items(matrix.SCENARIOS, "CODEX_MATRIX_SCENARIOS")],
                ["web_search_probe", "readonly_audit"],
            )

    def test_select_scenarios_supports_realistic_suite(self) -> None:
        with patch.dict("os.environ", {"CODEX_MATRIX_SUITE": "realistic"}, clear=False):
            self.assertEqual(
                [scenario.name for scenario in matrix.select_scenarios()],
                [
                    "real_debug_regression",
                    "real_refactor_with_tests",
                    "real_docs_sync",
                    "real_test_diagnosis_no_write",
                    "real_docfork_api_lookup",
                ],
            )

    def test_select_scenarios_filters_realistic_suite(self) -> None:
        with patch.dict(
            "os.environ",
            {"CODEX_MATRIX_SUITE": "realistic", "CODEX_MATRIX_SCENARIOS": "real_docs_sync,real_debug_regression"},
            clear=False,
        ):
            self.assertEqual(
                [scenario.name for scenario in matrix.select_scenarios()],
                ["real_docs_sync", "real_debug_regression"],
            )

    def test_effective_case_timeout_raises_subagent_probe_floor(self) -> None:
        scenario = next(item for item in matrix.SCENARIOS if item.name == "subagent_probe")
        self.assertEqual(matrix.effective_case_timeout("qwen3-vl-30b-a3b-thinking", scenario, 420), 900)

    def test_effective_case_timeout_keeps_non_subagent_timeout(self) -> None:
        scenario = next(item for item in matrix.SCENARIOS if item.name == "readonly_audit")
        self.assertEqual(matrix.effective_case_timeout("qwen3-vl-30b-a3b-thinking", scenario, 420), 420)

    def test_parse_untracked_paths_reads_porcelain_output(self) -> None:
        self.assertEqual(
            matrix.parse_untracked_paths("?? notes/new.txt\n?? internal/tmp/config.json\n M tracked.txt\n"),
            ["notes/new.txt", "internal/tmp/config.json"],
        )

    def test_create_worktree_syncs_dirty_workspace_when_enabled(self) -> None:
        base_dir = Path(tempfile.mkdtemp(prefix="matrix-worktree-base-"))
        self.addCleanup(lambda: __import__("shutil").rmtree(base_dir, ignore_errors=True))

        with patch.object(
            matrix,
            "run",
            return_value=subprocess.CompletedProcess(args=["git"], returncode=0, stdout="", stderr=""),
        ), patch.object(matrix, "sync_dirty_workspace_into_worktree") as mock_sync, patch.dict(
            "os.environ",
            {"CODEX_MATRIX_INCLUDE_DIRTY_WORKSPACE": "1"},
            clear=False,
        ):
            worktree = matrix.create_worktree(base_dir, "case-a")

        self.assertEqual(worktree, base_dir / "case-a")
        mock_sync.assert_called_once_with(base_dir / "case-a")

    def test_create_worktree_falls_back_to_clone_when_git_worktree_is_blocked(self) -> None:
        base_dir = Path(tempfile.mkdtemp(prefix="matrix-worktree-base-"))
        self.addCleanup(lambda: __import__("shutil").rmtree(base_dir, ignore_errors=True))
        calls = []

        def fake_run(cmd, cwd=None, timeout=None, input_text=None):
            calls.append(cmd)
            if cmd[:4] == ["git", "worktree", "add", "--detach"]:
                return subprocess.CompletedProcess(args=cmd, returncode=128, stdout="", stderr="Operation not permitted")
            if cmd == ["git", "rev-parse", "HEAD"]:
                return subprocess.CompletedProcess(args=cmd, returncode=0, stdout="abc123\n", stderr="")
            if cmd[:4] == ["git", "clone", "--no-hardlinks", "--no-checkout"]:
                return subprocess.CompletedProcess(args=cmd, returncode=0, stdout="", stderr="")
            if cmd[:3] == ["git", "checkout", "--detach"]:
                return subprocess.CompletedProcess(args=cmd, returncode=0, stdout="", stderr="")
            raise AssertionError(f"unexpected command: {cmd}")

        with patch.object(matrix, "run", side_effect=fake_run), patch.object(
            matrix, "sync_dirty_workspace_into_worktree"
        ) as mock_sync:
            worktree = matrix.create_worktree(base_dir, "case-a")

        self.assertEqual(worktree, base_dir / "case-a")
        self.assertIn(["git", "rev-parse", "HEAD"], calls)
        self.assertFalse(mock_sync.called)

    def test_sync_dirty_workspace_into_worktree_applies_patch_and_copies_untracked_files(self) -> None:
        repo_root = Path(tempfile.mkdtemp(prefix="matrix-repo-root-"))
        worktree = Path(tempfile.mkdtemp(prefix="matrix-worktree-"))
        self.addCleanup(lambda: __import__("shutil").rmtree(repo_root, ignore_errors=True))
        self.addCleanup(lambda: __import__("shutil").rmtree(worktree, ignore_errors=True))

        source_file = repo_root / "notes" / "new.txt"
        source_file.parent.mkdir(parents=True, exist_ok=True)
        source_file.write_text("hello from dirty workspace\n", encoding="utf-8")

        def fake_run(cmd, cwd=None, timeout=None, input_text=None):
            if cmd[:3] == ["git", "diff", "--binary"]:
                return subprocess.CompletedProcess(args=cmd, returncode=0, stdout="PATCH DATA", stderr="")
            if cmd[:4] == ["git", "status", "--porcelain", "--untracked-files=all"]:
                return subprocess.CompletedProcess(args=cmd, returncode=0, stdout="?? notes/new.txt\n", stderr="")
            if cmd[:3] == ["git", "apply", "--allow-empty"]:
                self.assertEqual(cwd, worktree)
                self.assertEqual(input_text, "PATCH DATA")
                return subprocess.CompletedProcess(args=cmd, returncode=0, stdout="", stderr="")
            raise AssertionError(f"unexpected command: {cmd}")

        with patch.object(matrix, "REPO_ROOT", repo_root), patch.object(matrix, "run", side_effect=fake_run):
            matrix.sync_dirty_workspace_into_worktree(worktree)

        self.assertEqual((worktree / "notes" / "new.txt").read_text(encoding="utf-8"), "hello from dirty workspace\n")

    def test_extract_terminal_jsonl_message_uses_turn_failed_error(self) -> None:
        with tempfile.NamedTemporaryFile("w+", suffix=".jsonl", delete=False) as fh:
            fh.write(json.dumps({"type": "error", "message": "Reconnecting... 5/5"}) + "\n")
            fh.write(
                json.dumps(
                    {
                        "type": "turn.failed",
                        "error": {"message": "We're currently experiencing high demand, which may cause temporary errors."},
                    }
                )
                + "\n"
            )
            path = Path(fh.name)
        try:
            self.assertEqual(
                matrix.extract_terminal_jsonl_message(path),
                "We're currently experiencing high demand, which may cause temporary errors.",
            )
        finally:
            path.unlink(missing_ok=True)

    def test_collect_file_change_paths_reads_jsonl(self) -> None:
        with tempfile.NamedTemporaryFile("w+", suffix=".jsonl", delete=False) as fh:
            fh.write(
                json.dumps(
                    {
                        "type": "item.completed",
                        "item": {
                            "type": "file_change",
                            "changes": [
                                {"path": "/tmp/worktree/internal/codexfixture/patchprobe/message.txt", "kind": "update"}
                            ],
                        },
                    }
                )
                + "\n"
            )
            path = Path(fh.name)
        try:
            self.assertEqual(
                matrix.collect_file_change_paths(path),
                ["/tmp/worktree/internal/codexfixture/patchprobe/message.txt"],
            )
        finally:
            path.unlink(missing_ok=True)

    def test_changed_path_matches_absolute_file_change_path(self) -> None:
        self.assertTrue(
            matrix.changed_path_matches(
                "internal/codexfixture/patchprobe/message.txt",
                ["/tmp/worktree/internal/codexfixture/patchprobe/message.txt"],
            )
        )

    def test_resolve_history_endpoint_prefers_explicit_history_env(self) -> None:
        with patch.dict(
            "os.environ",
            {
                "CODEX_MATRIX_PROVIDER": "newapi",
                "CODEX_MATRIX_BASE_URL": "http://10.0.90.200:3000/v1",
                "CODEX_MATRIX_BEARER_TOKEN": "sk-newapi",
                "CODEX_MATRIX_HISTORY_BASE_URL": "http://127.0.0.1:39527/v1",
                "CODEX_MATRIX_HISTORY_BEARER_TOKEN": "sk-admin",
            },
            clear=False,
        ):
            self.assertEqual(
                matrix.resolve_history_endpoint(),
                ("http://127.0.0.1:39527/v1", "sk-admin"),
            )

    def test_resolve_history_endpoint_does_not_fallback_to_newapi_provider(self) -> None:
        with patch.dict(
            "os.environ",
            {
                "CODEX_MATRIX_PROVIDER": "newapi",
                "CODEX_MATRIX_BASE_URL": "http://10.0.90.200:3000/v1",
                "CODEX_MATRIX_BEARER_TOKEN": "sk-newapi",
            },
            clear=True,
        ):
            self.assertEqual(matrix.resolve_history_endpoint(), ("", ""))

    def test_resolve_history_endpoint_allows_direct_firew2oai_provider(self) -> None:
        with patch.dict(
            "os.environ",
            {
                "CODEX_MATRIX_PROVIDER": "direct-firew2oai",
                "CODEX_MATRIX_BASE_URL": "http://127.0.0.1:39527/v1",
                "CODEX_MATRIX_BEARER_TOKEN": "sk-admin",
            },
            clear=True,
        ):
            self.assertEqual(
                matrix.resolve_history_endpoint(),
                ("http://127.0.0.1:39527/v1", "sk-admin"),
            )

    def test_resolve_history_endpoint_allows_named_direct_firew2oai_provider(self) -> None:
        with patch.dict(
            "os.environ",
            {
                "CODEX_MATRIX_PROVIDER": "direct-firew2oai-39544",
                "CODEX_MATRIX_BASE_URL": "http://127.0.0.1:39544/v1",
                "CODEX_MATRIX_BEARER_TOKEN": "sk-admin",
            },
            clear=True,
        ):
            self.assertEqual(
                matrix.resolve_history_endpoint(),
                ("http://127.0.0.1:39544/v1", "sk-admin"),
            )

    def test_extract_response_ids_from_logs_deduplicates(self) -> None:
        scenario = matrix.SCENARIOS[0]
        log_text = "\n".join(
            [
                'time=1 level=INFO msg="responses request" response_id=resp_aaa model=qwen3-vl-30b-a3b-instruct task_summary="你是资深 Go 工程师。请在当前仓库完成一个真实 Coding 只读核验任务： 1) 阅读 internal/proxy/output_constraints.go。"',
                'time=2 level=INFO msg="responses request" response_id=resp_aaa model=qwen3-vl-30b-a3b-instruct task_summary="你是资深 Go 工程师。请在当前仓库完成一个真实 Coding 只读核验任务： 1) 阅读 internal/proxy/output_constraints.go。"',
                'time=3 level=INFO msg="responses request" response_id=resp_bbb model=qwen3-vl-30b-a3b-instruct task_summary="你是资深 Go 工程师。请在当前仓库完成一个真实 Coding 只读核验任务： 1) 阅读 internal/proxy/output_constraints.go。"',
            ]
        )
        self.assertEqual(
            matrix.extract_response_ids_from_logs(log_text, "qwen3-vl-30b-a3b-instruct", scenario),
            ["resp_aaa", "resp_bbb"],
        )

    def test_collect_firew2oai_history_signals_merges_all_response_ids(self) -> None:
        scenario = matrix.SCENARIOS[7]
        log_text = "\n".join(
            [
                'time=1 level=INFO msg="responses request" response_id=resp_first model=qwen3-vl-30b-a3b-instruct task_summary="你是测试代理。请验证 js_repl： 1) 必须先使用 js_repl 计算数组 [2,3,5] 的和。"',
                'time=2 level=INFO msg="responses request" response_id=resp_second model=qwen3-vl-30b-a3b-instruct task_summary="你是测试代理。请验证 js_repl： 1) 必须先使用 js_repl 计算数组 [2,3,5] 的和。"',
            ]
        )

        fake_logs = subprocess.CompletedProcess(args=["docker"], returncode=0, stdout="", stderr=log_text)
        with patch.object(matrix, "run", return_value=fake_logs), patch.object(
            matrix,
            "load_response_input_items",
            side_effect=[
                [{"type": "custom_tool_call", "name": "js_repl"}],
                [{"type": "custom_tool_call", "name": "js_repl_reset"}],
            ],
        ):
            signals, response_id = matrix.collect_firew2oai_history_signals(
                started_at=0,
                model="qwen3-vl-30b-a3b-instruct",
                scenario=scenario,
            )

        self.assertIn("js_repl", signals)
        self.assertIn("js_repl_reset", signals)
        self.assertEqual(response_id, "resp_second")

    def test_collect_firew2oai_history_signals_reads_log_file(self) -> None:
        scenario = next(item for item in matrix.SCENARIOS if item.name == "view_image_probe")
        log_text = (
            'time=1 level=INFO msg="responses request" response_id=resp_img '
            'model=qwen3-vl-30b-a3b-instruct task_summary="你是测试代理。请验证 view_image： '
            '1) 必须先使用 exec_command 执行 `pwd`，读取当前工作目录绝对路径。"\n'
        )
        with tempfile.NamedTemporaryFile("w", encoding="utf-8", delete=False) as fh:
            fh.write(log_text)
            path = Path(fh.name)
        self.addCleanup(lambda: path.unlink(missing_ok=True))

        with patch.dict("os.environ", {"CODEX_MATRIX_HISTORY_LOG_FILE": str(path)}, clear=True), patch.object(
            matrix,
            "load_response_input_items",
            return_value=[
                {"type": "function_call", "name": "view_image"},
                {"type": "function_call_output", "call_id": "call_img"},
            ],
        ):
            signals, response_id = matrix.collect_firew2oai_history_signals(
                started_at=0,
                model="qwen3-vl-30b-a3b-instruct",
                scenario=scenario,
            )

        self.assertIn("view_image", signals)
        self.assertEqual(response_id, "resp_img")

    def test_classify_failure_reason_marks_missed_write_stdin(self) -> None:
        self.assertEqual(
            matrix.classify_failure_reason(
                exit_code="0",
                expected_signals=("write_stdin", "command_execution"),
                observed_signals=["exec_command"],
                final_preview='Codex adapter error: tool_choice requires "write_stdin", got "exec_command"',
                stderr_preview="",
                labels_ok=False,
                result_pass=False,
            ),
            "missed_write_stdin",
        )

    def test_classify_failure_reason_marks_write_stdin_runtime_error(self) -> None:
        self.assertEqual(
            matrix.classify_failure_reason(
                exit_code="0",
                expected_signals=("write_stdin", "command_execution"),
                observed_signals=["exec_command", "write_stdin"],
                final_preview="RESULT: PASS\nFILES: none\nTEST: N/A\nNOTE: write_stdin failed: Unknown process id 1",
                stderr_preview="",
                labels_ok=True,
                result_pass=True,
            ),
            "write_stdin_runtime_error",
        )

    def test_contains_explicit_execution_failure_marks_write_stdin_failure(self) -> None:
        self.assertTrue(
            matrix.contains_explicit_execution_failure(
                "RESULT: PASS\nFILES: none\nTEST: N/A\nNOTE: write_stdin failed: Unknown process id 1"
            )
        )

    def test_classify_failure_reason_marks_web_search_followup_not_grounded(self) -> None:
        self.assertEqual(
            matrix.classify_failure_reason(
                exit_code="0",
                expected_signals=("web_search",),
                observed_signals=["web_search"],
                final_preview="Codex adapter error: web_search follow-up did not answer from captured results",
                stderr_preview="",
                labels_ok=False,
                result_pass=False,
            ),
            "web_search_followup_not_grounded",
        )

    def test_classify_failure_reason_marks_mcp_search_invalid_args(self) -> None:
        self.assertEqual(
            matrix.classify_failure_reason(
                exit_code="0",
                expected_signals=("docfork", "search_docs"),
                observed_signals=["docfork", "search_docs"],
                final_preview="RESULT: FAIL FILES: none TEST: N/A NOTE: MCP error -32602: Input validation error: Invalid arguments for tool search",
                stderr_preview="",
                labels_ok=True,
                result_pass=False,
            ),
            "mcp_search_invalid_args",
        )

    def test_classify_failure_reason_marks_semantic_result_fail(self) -> None:
        self.assertEqual(
            matrix.classify_failure_reason(
                exit_code="0",
                expected_signals=("spawn_agent", "wait_agent", "close_agent"),
                observed_signals=["spawn_agent", "wait_agent", "close_agent"],
                final_preview="RESULT: FAIL\nFILES: README.md\nTEST: N/A\nNOTE: 未获得可解析的工具结果。",
                stderr_preview="",
                labels_ok=True,
                result_pass=False,
            ),
            "semantic_result_fail",
        )

    def test_filter_declared_tools_for_matrix_keeps_only_allowed_mcp_tools(self) -> None:
        self.assertEqual(
            matrix.filter_declared_tools_for_matrix(
                [
                    "exec_command",
                    "mcp__cloudflare_api__search",
                    "mcp__docfork__search_docs",
                    "mcp__chrome_devtools__new_page",
                ]
            ),
            ["exec_command", "mcp__docfork__search_docs", "mcp__chrome_devtools__new_page"],
        )

    def test_classify_failure_reason_ignores_unrelated_mcp_auth_stderr(self) -> None:
        self.assertEqual(
            matrix.classify_failure_reason(
                exit_code="0",
                expected_signals=("spawn_agent", "wait_agent", "close_agent"),
                observed_signals=["spawn_agent", "wait_agent", "close_agent"],
                final_preview="RESULT: FAIL\nFILES: README.md\nTEST: N/A\nNOTE: 未获得可解析的工具结果。",
                stderr_preview="AuthRequired: Missing or invalid access token",
                labels_ok=True,
                result_pass=False,
            ),
            "semantic_result_fail",
        )

    def test_classify_failure_reason_marks_provider_high_demand(self) -> None:
        self.assertEqual(
            matrix.classify_failure_reason(
                exit_code="1",
                expected_signals=("exec_command",),
                observed_signals=[],
                final_preview="We're currently experiencing high demand, which may cause temporary errors.",
                stderr_preview="",
                labels_ok=False,
                result_pass=False,
            ),
            "provider_high_demand",
        )

    def test_classify_failure_reason_marks_provider_no_available_channel(self) -> None:
        self.assertEqual(
            matrix.classify_failure_reason(
                exit_code="1",
                expected_signals=("exec_command",),
                observed_signals=[],
                final_preview="unexpected status 503 Service Unavailable: No available channel for model glm-4.7 under group default",
                stderr_preview="",
                labels_ok=False,
                result_pass=False,
            ),
            "provider_no_available_channel",
        )

    def test_classify_failure_reason_marks_upstream_service_unavailable(self) -> None:
        self.assertEqual(
            matrix.classify_failure_reason(
                exit_code="0",
                expected_signals=("list_mcp_resources", "list_mcp_resource_templates"),
                observed_signals=["message"],
                final_preview=(
                    "Codex adapter error: upstream stream failed before content: upstream error: 503, "
                    "message='Service Unavailable'"
                ),
                stderr_preview="AuthRequired: Missing or invalid access token",
                labels_ok=False,
                result_pass=False,
            ),
            "upstream_service_unavailable",
        )

    def test_classify_failure_reason_marks_upstream_tls_bad_record_mac(self) -> None:
        self.assertEqual(
            matrix.classify_failure_reason(
                exit_code="0",
                expected_signals=("exec_command",),
                observed_signals=["message"],
                final_preview="Codex adapter error: upstream stream failed before content: local error: tls: bad record MAC",
                stderr_preview="",
                labels_ok=False,
                result_pass=False,
            ),
            "upstream_tls_bad_record_mac",
        )

    def test_classify_failure_reason_marks_upstream_transport_error(self) -> None:
        self.assertEqual(
            matrix.classify_failure_reason(
                exit_code="0",
                expected_signals=("exec_command",),
                observed_signals=["message"],
                final_preview="Codex adapter error: upstream stream failed before content: local error: connection reset by peer",
                stderr_preview="",
                labels_ok=False,
                result_pass=False,
            ),
            "upstream_transport_error",
        )

    def test_classify_failure_reason_marks_upstream_invalid_prompt(self) -> None:
        self.assertEqual(
            matrix.classify_failure_reason(
                exit_code="1",
                expected_signals=("exec_command",),
                observed_signals=[],
                final_preview='{"error":{"message":"Invalid Responses API request","type":"upstream_error","param":"","code":"invalid_prompt"}}',
                stderr_preview="",
                labels_ok=False,
                result_pass=False,
            ),
            "upstream_invalid_prompt",
        )

    def test_classify_failure_reason_marks_missed_docfork_fetch_doc(self) -> None:
        self.assertEqual(
            matrix.classify_failure_reason(
                exit_code="0",
                expected_signals=("docfork", "search_docs", "fetch_doc"),
                observed_signals=["docfork", "search_docs"],
                final_preview='Codex adapter error: tool_choice requires "mcp__docfork__fetch_doc", got "mcp__docfork__search_docs"',
                stderr_preview="",
                labels_ok=False,
                result_pass=False,
            ),
            "missed_docfork_fetch_doc",
        )

    def test_classify_failure_reason_marks_missed_apply_patch(self) -> None:
        self.assertEqual(
            matrix.classify_failure_reason(
                exit_code="0",
                expected_signals=("apply_patch",),
                observed_signals=["exec_command"],
                final_preview='Codex adapter error: tool_choice requires "apply_patch", got "exec_command"',
                stderr_preview="",
                labels_ok=False,
                result_pass=False,
            ),
            "missed_apply_patch",
        )

    def test_classify_failure_reason_marks_upstream_stream_eof(self) -> None:
        self.assertEqual(
            matrix.classify_failure_reason(
                exit_code="0",
                expected_signals=("update_plan", "command_execution"),
                observed_signals=["update_plan", "command_execution"],
                final_preview="Codex adapter error: upstream stream failed before content: unexpected EOF",
                stderr_preview="",
                labels_ok=False,
                result_pass=False,
            ),
            "upstream_stream_eof",
        )

    def test_classify_failure_reason_marks_upstream_incomplete_completion(self) -> None:
        self.assertEqual(
            matrix.classify_failure_reason(
                exit_code="0",
                expected_signals=("view_image",),
                observed_signals=["message"],
                final_preview="Codex adapter error: upstream response ended without a completion signal",
                stderr_preview="",
                labels_ok=False,
                result_pass=False,
            ),
            "upstream_incomplete_completion",
        )

    def test_classify_failure_reason_marks_empty_final_after_tool(self) -> None:
        self.assertEqual(
            matrix.classify_failure_reason(
                exit_code="0",
                expected_signals=("web_search",),
                observed_signals=["web_search", "message"],
                final_preview="",
                stderr_preview="AuthRequired: Missing or invalid access token",
                labels_ok=False,
                result_pass=False,
            ),
            "empty_final_after_tool",
        )

    def test_classify_failure_reason_marks_missed_chrome_devtools_sequence(self) -> None:
        self.assertEqual(
            matrix.classify_failure_reason(
                exit_code="0",
                expected_signals=("chrome_devtools", "new_page", "take_snapshot", "click", "wait_for"),
                observed_signals=["chrome_devtools", "new_page"],
                final_preview=(
                    'Codex adapter error: tool_choice requires "mcp__chrome_devtools__click", '
                    'got "mcp__chrome_devtools__new_page"'
                ),
                stderr_preview="AuthRequired: Missing or invalid access token",
                labels_ok=False,
                result_pass=False,
            ),
            "missed_chrome_devtools_sequence",
        )

    def test_classify_failure_reason_marks_mcp_search_runtime_error(self) -> None:
        self.assertEqual(
            matrix.classify_failure_reason(
                exit_code="0",
                expected_signals=("docfork", "search_docs"),
                observed_signals=["docfork", "search_docs"],
                final_preview="RESULT: FAIL FILES: none TEST: N/A NOTE: Error: (intermediate value)(...) is not a function",
                stderr_preview="",
                labels_ok=True,
                result_pass=False,
            ),
            "mcp_search_runtime_error",
        )
