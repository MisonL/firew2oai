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
        with patch.dict(
            "os.environ",
            {"CODEX_MATRIX_CODEX_HOME": "codex-home", "CODEX_MATRIX_INHERIT_CODEX_HOME": "0"},
            clear=True,
        ):
            matrix.configure_codex_home(output_dir)

            self.assertEqual(Path(__import__("os").environ["CODEX_HOME"]), output_dir / "codex-home")
            self.assertTrue((output_dir / "codex-home").is_dir())

    def test_configure_codex_provider_config_reads_token_file(self) -> None:
        output_dir = Path(tempfile.mkdtemp(prefix="matrix-output-"))
        token_file = output_dir / "token.txt"
        token_file.write_text("sk-local\n", encoding="utf-8")
        self.addCleanup(lambda: __import__("shutil").rmtree(output_dir, ignore_errors=True))

        with patch.dict(
            "os.environ",
            {
                "CODEX_HOME": str(output_dir / "codex-home"),
                "CODEX_MATRIX_WRITE_PROVIDER_CONFIG": "1",
                "CODEX_MATRIX_PROVIDER": "newapi-local",
                "CODEX_MATRIX_BASE_URL": "http://localhost:3000/v1",
                "CODEX_MATRIX_WIRE_API": "responses",
                "CODEX_MATRIX_BEARER_TOKEN_FILE": str(token_file),
                "CODEX_MATRIX_INHERIT_CODEX_CONFIG": "0",
            },
            clear=True,
        ):
            (output_dir / "codex-home").mkdir()
            matrix.configure_codex_provider_config()

        config = (output_dir / "codex-home/config.toml").read_text(encoding="utf-8")
        self.assertIn("[model_providers.newapi-local]", config)
        self.assertIn('experimental_bearer_token = "sk-local"', config)

    def test_configure_codex_provider_config_preserves_base_tool_config(self) -> None:
        output_dir = Path(tempfile.mkdtemp(prefix="matrix-output-"))
        base_home = output_dir / "base-codex"
        base_home.mkdir()
        (base_home / "config.toml").write_text(
            '[features]\njs_repl = true\n\n[mcp_servers.docfork]\ncommand = "docfork"\n',
            encoding="utf-8",
        )
        token_file = output_dir / "token.txt"
        token_file.write_text("sk-local\n", encoding="utf-8")
        self.addCleanup(lambda: __import__("shutil").rmtree(output_dir, ignore_errors=True))

        with patch.dict(
            "os.environ",
            {
                "CODEX_HOME": str(output_dir / "codex-home"),
                "CODEX_MATRIX_WRITE_PROVIDER_CONFIG": "1",
                "CODEX_MATRIX_BASE_CODEX_HOME": str(base_home),
                "CODEX_MATRIX_PROVIDER": "newapi-local",
                "CODEX_MATRIX_BASE_URL": "http://localhost:3000/v1",
                "CODEX_MATRIX_WIRE_API": "responses",
                "CODEX_MATRIX_BEARER_TOKEN_FILE": str(token_file),
            },
            clear=True,
        ):
            (output_dir / "codex-home").mkdir()
            matrix.configure_codex_provider_config()

        config = (output_dir / "codex-home/config.toml").read_text(encoding="utf-8")
        self.assertIn("[features]", config)
        self.assertIn("[mcp_servers.docfork]", config)
        self.assertIn("[model_providers.newapi-local]", config)
        self.assertIn('experimental_bearer_token = "sk-local"', config)

    def test_configure_codex_provider_config_can_add_fixture_mcp(self) -> None:
        output_dir = Path(tempfile.mkdtemp(prefix="matrix-output-"))
        token_file = output_dir / "token.txt"
        token_file.write_text("sk-local\n", encoding="utf-8")
        self.addCleanup(lambda: __import__("shutil").rmtree(output_dir, ignore_errors=True))

        with patch.dict(
            "os.environ",
            {
                "CODEX_HOME": str(output_dir / "codex-home"),
                "CODEX_MATRIX_WRITE_PROVIDER_CONFIG": "1",
                "CODEX_MATRIX_PROVIDER": "newapi-local",
                "CODEX_MATRIX_BASE_URL": "http://localhost:3000/v1",
                "CODEX_MATRIX_WIRE_API": "responses",
                "CODEX_MATRIX_BEARER_TOKEN_FILE": str(token_file),
                "CODEX_MATRIX_INHERIT_CODEX_CONFIG": "0",
                "CODEX_MATRIX_ENABLE_FIXTURE_MCP": "1",
            },
            clear=True,
        ):
            (output_dir / "codex-home").mkdir()
            matrix.configure_codex_provider_config()

        config = (output_dir / "codex-home/config.toml").read_text(encoding="utf-8")
        self.assertIn("[mcp_servers.firew2oai_fixture_resources]", config)
        self.assertIn("codex_mcp_resource_fixture.mjs", config)

    def test_build_codex_exec_command_omits_token_when_provider_config_is_written(self) -> None:
        with patch.dict(
            "os.environ",
            {
                "CODEX_MATRIX_PROVIDER": "newapi-local",
                "CODEX_MATRIX_BASE_URL": "http://localhost:3000/v1",
                "CODEX_MATRIX_BEARER_TOKEN": "sk-local",
                "CODEX_MATRIX_WRITE_PROVIDER_CONFIG": "1",
            },
            clear=True,
        ):
            cmd = matrix.build_codex_exec_command("codex", Path("/tmp/w"), Path("/tmp/out"), "model-a", "prompt")

        self.assertNotIn("sk-local", " ".join(cmd))

    def test_build_codex_exec_command_appends_feature_overrides(self) -> None:
        with patch.dict(
            "os.environ",
            {
                "CODEX_MATRIX_ENABLE_FEATURES": "js_repl,memories",
                "CODEX_MATRIX_DISABLE_FEATURES": "apps",
            },
            clear=True,
        ):
            cmd = matrix.build_codex_exec_command("codex", Path("/tmp/w"), Path("/tmp/out"), "model-a", "prompt")

        self.assertIn("--enable", cmd)
        self.assertIn("js_repl", cmd)
        self.assertIn("memories", cmd)
        self.assertIn("--disable", cmd)
        self.assertIn("apps", cmd)

    def test_run_codex_exec_command_closes_stdin_explicitly(self) -> None:
        with patch("codex_realchain_matrix.run") as mock_run:
            mock_run.return_value = subprocess.CompletedProcess(["codex"], 0, "", "")
            matrix.run_codex_exec_command(["codex", "exec", "prompt"], timeout_s=30)

        _, kwargs = mock_run.call_args
        self.assertEqual(kwargs.get("input_text"), "")
        self.assertEqual(kwargs.get("cwd"), REPO_ROOT)
        self.assertEqual(kwargs.get("timeout"), 30)

    def test_codex_result_retryable_before_progress_accepts_sse_idle_timeout(self) -> None:
        result = subprocess.CompletedProcess(
            ["codex"],
            -9,
            '{"type":"error","message":"Reconnecting... (stream disconnected before completion: idle timeout waiting for SSE)"}\n',
            "Codex matrix command timeout after 900s",
        )

        self.assertTrue(matrix.codex_result_retryable_before_progress(result))

    def test_codex_result_retryable_before_progress_rejects_tool_progress(self) -> None:
        result = subprocess.CompletedProcess(
            ["codex"],
            -9,
            (
                '{"type":"item","item":{"type":"function_call","name":"exec_command"}}\n'
                '{"type":"error","message":"stream disconnected before completion: idle timeout waiting for SSE"}\n'
            ),
            "Codex matrix command timeout after 900s",
        )

        self.assertFalse(matrix.codex_result_retryable_before_progress(result))

    def test_run_case_retries_transient_sse_before_progress(self) -> None:
        output_dir = Path(tempfile.mkdtemp(prefix="matrix-retry-"))
        worktree = output_dir / "worktree"
        worktree.mkdir()
        self.addCleanup(lambda: __import__("shutil").rmtree(output_dir, ignore_errors=True))
        scenario = matrix.Scenario(
            name="retry_probe",
            prompt="retry prompt",
            expected_operations=(),
            expected_files=(),
            capabilities=("structured_final",),
        )
        last_path = output_dir / "model-a__retry_probe.last.txt"

        def fake_codex_run(_cmd, _timeout):
            if fake_codex_run.calls == 0:
                fake_codex_run.calls += 1
                return subprocess.CompletedProcess(
                    ["codex"],
                    -9,
                    '{"type":"error","message":"stream disconnected before completion: idle timeout waiting for SSE"}\n',
                    "Codex matrix command timeout after 900s",
                )
            fake_codex_run.calls += 1
            last_path.write_text("RESULT: PASS\nFILES: none\nTEST: N/A\nNOTE: retried", encoding="utf-8")
            return subprocess.CompletedProcess(["codex"], 0, "", "")

        fake_codex_run.calls = 0
        with patch("codex_realchain_matrix.create_worktree", return_value=worktree), \
            patch("codex_realchain_matrix.prepare_fixture"), \
            patch("codex_realchain_matrix.remove_worktree"), \
            patch("codex_realchain_matrix.collect_changed_paths", return_value=[]), \
            patch("codex_realchain_matrix.snapshot_paths", return_value={}), \
            patch("codex_realchain_matrix.build_codex_exec_command", return_value=["codex", "exec", "retry"]), \
            patch("codex_realchain_matrix.run_codex_exec_command", side_effect=fake_codex_run), \
            patch("codex_realchain_matrix.collect_firew2oai_history_signals", return_value=([], "")):
            row = matrix.run_case(output_dir, "codex", "model-a", scenario, timeout_s=30)

        self.assertEqual(row["status"], "ok")
        self.assertEqual(fake_codex_run.calls, 2)
        self.assertTrue((output_dir / "model-a__retry_probe.attempt1.jsonl").exists())
        self.assertTrue((output_dir / "model-a__retry_probe.stderr.attempt1.txt").exists())

    def test_discover_declared_tools_uses_codex_exec_stdin_wrapper(self) -> None:
        log_text = 'msg="responses request" prompt="TOOL_DISCOVERY_PROBE_123" tool_names="[exec_command js_repl]"\n'
        with patch.dict("os.environ", {"CODEX_MATRIX_HISTORY_CONTAINER": "firew2oai"}, clear=True), \
            patch("codex_realchain_matrix.time.time", return_value=123), \
            patch("codex_realchain_matrix.create_worktree", return_value=Path("/tmp/worktree")), \
            patch("codex_realchain_matrix.remove_worktree"), \
            patch("codex_realchain_matrix.build_codex_exec_command", return_value=["codex", "exec", "prompt"]), \
            patch("codex_realchain_matrix.run_codex_exec_command") as mock_codex_run, \
            patch("codex_realchain_matrix.run") as mock_run:
            mock_codex_run.return_value = subprocess.CompletedProcess(["codex"], 0, "", "")
            mock_run.return_value = subprocess.CompletedProcess(["docker"], 0, log_text, "")

            tools = matrix.discover_declared_tools(Path("/tmp/out"), "codex", "glm-4p7", timeout_s=30)

        self.assertEqual(tools, ["exec_command", "js_repl"])
        mock_codex_run.assert_called_once_with(["codex", "exec", "prompt"], 30)
        self.assertEqual(mock_run.call_count, 1)

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

    def test_select_scenarios_supports_combined_suite(self) -> None:
        with patch.dict("os.environ", {"CODEX_MATRIX_SUITE": "combined"}, clear=False):
            scenarios = matrix.select_scenarios()

        self.assertEqual(
            len(scenarios),
            len(matrix.SCENARIOS) + len(matrix.BUILTIN_TOOL_SCENARIOS) + len(matrix.REALISTIC_SCENARIOS),
        )

    def test_select_scenarios_supports_builtin_tools_suite(self) -> None:
        with patch.dict("os.environ", {"CODEX_MATRIX_SUITE": "builtin-tools"}, clear=False):
            self.assertEqual(
                [scenario.name for scenario in matrix.select_scenarios()],
                ["mcp_resource_read_probe", "subagent_resume_send_probe"],
            )

    def test_plan_then_read_uses_head_command_as_expected_operation(self) -> None:
        scenario = next(item for item in matrix.SCENARIOS if item.name == "plan_then_read")
        self.assertEqual(scenario.expected_operations, ("head -n 3 README.md",))

    def test_prepare_fixture_search_and_patch_creates_target_and_test_files(self) -> None:
        worktree = Path(tempfile.mkdtemp(prefix="matrix-fixture-search-"))
        self.addCleanup(lambda: __import__("shutil").rmtree(worktree, ignore_errors=True))

        matrix.prepare_fixture(worktree, "search_and_patch")

        self.assertTrue((worktree / "internal/codexfixture/searchfix/summary.go").is_file())
        self.assertTrue((worktree / "internal/codexfixture/searchfix/summary_test.go").is_file())

    def test_prepare_fixture_real_test_diagnosis_does_not_create_searchfix_files(self) -> None:
        worktree = Path(tempfile.mkdtemp(prefix="matrix-fixture-diagnosis-"))
        self.addCleanup(lambda: __import__("shutil").rmtree(worktree, ignore_errors=True))

        matrix.prepare_fixture(worktree, "real_test_diagnosis_no_write")

        self.assertTrue((worktree / "internal/codexfixture/realdiagnose/math.go").is_file())
        self.assertFalse((worktree / "internal/codexfixture/searchfix/summary_test.go").exists())

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

    def test_expected_operation_observed_accepts_changed_repo_path(self) -> None:
        self.assertTrue(
            matrix.expected_operation_observed(
                "internal/codexfixture/searchfix/summary.go",
                "python3 -c 'import base64; exec(...)'",
                ["internal/codexfixture/searchfix/summary.go"],
                [],
                set(),
                set(),
            )
        )

    def test_expected_operation_observed_keeps_command_operations_strict(self) -> None:
        self.assertFalse(
            matrix.expected_operation_observed(
                "go test ./internal/codexfixture/searchfix",
                "python3 -c 'import base64; exec(...)'",
                ["internal/codexfixture/searchfix/summary.go"],
                [],
                set(),
                set(),
            )
        )

    def test_parse_trace_signals_infers_write_stdin_from_interactive_python_output(self) -> None:
        with tempfile.NamedTemporaryFile("w+", suffix=".jsonl", delete=False) as fh:
            fh.write(
                json.dumps(
                    {
                        "type": "item.completed",
                        "item": {
                            "type": "command_execution",
                            "command": "/bin/zsh -lc python3",
                            "aggregated_output": (
                                "Python 3.14.3\r\n"
                                "\u001b[?2004h\u001b[?1h\u001b=\u001b[?25l>>> \u001b[?12l\u001b[?25h"
                                "\u001b[@p\u001b[@r\u001b[@i\u001b[@n\u001b[@t\u001b[@(\u001b[@2"
                                "\u001b[@ \u001b[@+\u001b[@ \u001b[@3\u001b[@)\u001b[16D\n\r"
                                "\u001b[?2004l\u001b[?1l\u001b>5\r\n"
                                "\u001b[?2004h\u001b[?1h\u001b=\u001b[?25l>>> \u001b[?12l\u001b[?25h"
                                "\u001b[@e\u001b[@x\u001b[@i\u001b[@t\u001b[@(\u001b[@)\u001b[10D\n\r"
                                "\u001b[?2004l\u001b[?1l\u001b>"
                            ),
                            "exit_code": 0,
                            "status": "completed",
                        },
                    }
                )
                + "\n"
            )
            path = Path(fh.name)
        try:
            signals, _ = matrix.parse_trace_signals(path)
            self.assertIn("write_stdin", signals)
        finally:
            path.unlink(missing_ok=True)

    def test_last_command_success_by_expected_operation_matches_interactive_python(self) -> None:
        with tempfile.NamedTemporaryFile("w+", suffix=".jsonl", delete=False) as fh:
            fh.write(
                json.dumps(
                    {
                        "type": "item.completed",
                        "item": {
                            "type": "command_execution",
                            "command": "/bin/zsh -lc python3",
                            "aggregated_output": ">>> ",
                            "exit_code": 0,
                            "status": "completed",
                        },
                    }
                )
                + "\n"
            )
            path = Path(fh.name)
        try:
            results = matrix.last_command_success_by_expected_operation(path, ("python3",))
            self.assertEqual(results, {"python3": True})
        finally:
            path.unlink(missing_ok=True)

    def test_parse_evidence_history_signals_reads_write_stdin_and_js_reset(self) -> None:
        interactive = next(item for item in matrix.SCENARIOS if item.name == "interactive_shell_session")
        js_repl = next(item for item in matrix.SCENARIOS if item.name == "js_repl_roundtrip")
        interactive_log = (
            'time=1 level=INFO msg="responses request" response_id=resp_shell '
            'model=kimi-k2p5 task_summary="你是测试代理。请验证交互式 shell 会话能力： 1) 必须使用 exec_command 启动一个交互式 python3 会话。" '
            'evidence_commands="[python3 write_stdin]" '
            'evidence_outputs="[write_stdin => success=true 5]"\n'
        )
        js_log = (
            'time=2 level=INFO msg="responses request" response_id=resp_js '
            'model=kimi-k2p5 task_summary="你是测试代理。请验证 js_repl： 1) 必须先使用 js_repl 计算数组 [2,3,5] 的和。" '
            'evidence_commands="[js_repl js_repl_reset]" '
            'evidence_outputs="[js_repl => success=true 10 js_repl_reset => js_repl kernel reset]"\n'
        )

        interactive_signals = matrix.parse_evidence_history_signals(interactive_log, "kimi-k2p5", interactive)
        js_signals = matrix.parse_evidence_history_signals(js_log, "kimi-k2p5", js_repl)

        self.assertIn("write_stdin", interactive_signals)
        self.assertIn("js_repl_reset", js_signals)

    def test_parse_trace_signals_infers_view_image_from_inline_data_url(self) -> None:
        with tempfile.NamedTemporaryFile("w+", suffix=".jsonl", delete=False) as fh:
            fh.write(
                json.dumps(
                    {
                        "type": "item.completed",
                        "item": {
                            "type": "agent_message",
                            "text": 'RESULT: PASS\nFILES: internal/codexfixture/assets/red.png\nTEST: N/A\nNOTE: [{"image_url":"data:image/png;base64,abc","type":"input_image"}]',
                        },
                    }
                )
                + "\n"
            )
            path = Path(fh.name)
        try:
            signals, _ = matrix.parse_trace_signals(path)
            self.assertIn("view_image", signals)
        finally:
            path.unlink(missing_ok=True)

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

    def test_collect_firew2oai_history_signals_falls_back_to_evidence_logs(self) -> None:
        scenario = next(item for item in matrix.SCENARIOS if item.name == "js_repl_roundtrip")
        log_text = (
            'time=1 level=INFO msg="responses request" response_id=resp_js '
            'model=qwen3-vl-30b-a3b-instruct task_summary="你是测试代理。请验证 js_repl： '
            '1) 必须先使用 js_repl 计算数组 [2,3,5] 的和。" '
            'evidence_commands="[js_repl js_repl_reset]" '
            'evidence_outputs="[js_repl => success=true 10 js_repl_reset => js_repl kernel reset js_repl => success=true 56]"\n'
        )
        with tempfile.NamedTemporaryFile("w", encoding="utf-8", delete=False) as fh:
            fh.write(log_text)
            path = Path(fh.name)
        self.addCleanup(lambda: path.unlink(missing_ok=True))

        with patch.dict("os.environ", {"CODEX_MATRIX_HISTORY_LOG_FILE": str(path)}, clear=True), patch.object(
            matrix,
            "load_response_input_items",
            return_value=[],
        ):
            signals, response_id = matrix.collect_firew2oai_history_signals(
                started_at=0,
                model="qwen3-vl-30b-a3b-instruct",
                scenario=scenario,
            )

        self.assertIn("js_repl_reset", signals)
        self.assertEqual(response_id, "resp_js")

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

    def test_classify_failure_reason_prefers_sse_idle_timeout(self) -> None:
        self.assertEqual(
            matrix.classify_failure_reason(
                exit_code="timeout",
                expected_signals=("write_stdin", "command_execution"),
                observed_signals=["exec_command", "write_stdin"],
                final_preview="Reconnecting... 2/5 (stream disconnected before completion: idle timeout waiting for SSE)",
                stderr_preview="Reading additional input from stdin...",
                labels_ok=False,
                result_pass=False,
            ),
            "upstream_sse_idle_timeout",
        )

    def test_contains_explicit_execution_failure_marks_write_stdin_failure(self) -> None:
        self.assertTrue(
            matrix.contains_explicit_execution_failure(
                "RESULT: PASS\nFILES: none\nTEST: N/A\nNOTE: write_stdin failed: Unknown process id 1"
            )
        )

    def test_non_write_stdin_scenario_ignores_stderr_only_write_stdin_warning(self) -> None:
        scenario = matrix.Scenario(
            name="cross_file_feature",
            prompt="",
            expected_operations=("go test ./internal/codexfixture/feature",),
            expected_files=("internal/codexfixture/feature/title.go",),
            capabilities=("exec_command",),
            required_tools=("exec_command",),
        )
        stderr = "ERROR codex_core::tools::router: error=write_stdin failed: Unknown process id 1"

        self.assertFalse(matrix.contains_blocking_stderr_execution_failure(stderr, scenario))

    def test_write_stdin_scenario_keeps_stderr_write_stdin_warning_blocking(self) -> None:
        scenario = matrix.Scenario(
            name="interactive_shell_session",
            prompt="",
            expected_operations=("python3",),
            expected_files=(),
            capabilities=("exec_command", "write_stdin"),
            required_tools=("exec_command", "write_stdin"),
            expected_signals=("write_stdin", "command_execution"),
        )
        stderr = "ERROR codex_core::tools::router: error=write_stdin failed: Unknown process id 1"

        self.assertTrue(matrix.contains_blocking_stderr_execution_failure(stderr, scenario))

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

    def test_classify_failure_reason_marks_web_search_no_results(self) -> None:
        self.assertEqual(
            matrix.classify_failure_reason(
                exit_code="0",
                expected_signals=("web_search",),
                observed_signals=["web_search"],
                final_preview="Codex adapter error: web search returned no results",
                stderr_preview="",
                labels_ok=False,
                result_pass=False,
            ),
            "web_search_no_results",
        )

    def test_classify_failure_reason_marks_web_search_challenge_blocked(self) -> None:
        self.assertEqual(
            matrix.classify_failure_reason(
                exit_code="0",
                expected_signals=("web_search",),
                observed_signals=["web_search"],
                final_preview="Codex adapter error: web search backend blocked request with DuckDuckGo challenge",
                stderr_preview="",
                labels_ok=False,
                result_pass=False,
            ),
            "web_search_challenge_blocked",
        )

    def test_classify_failure_reason_marks_web_search_transport_error_sanitized(self) -> None:
        self.assertEqual(
            matrix.classify_failure_reason(
                exit_code="0",
                expected_signals=("web_search",),
                observed_signals=["web_search"],
                final_preview="Codex adapter error: web search backend unavailable",
                stderr_preview="",
                labels_ok=False,
                result_pass=False,
            ),
            "web_search_transport_error",
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

    def test_tool_probe_result_label_drift_accepts_successful_web_search_signal(self) -> None:
        scenario = next(item for item in matrix.SCENARIOS if item.name == "web_search_probe")

        self.assertTrue(
            matrix.should_accept_tool_probe_result_label_drift(
                scenario=scenario,
                signals_ok=True,
                files_ok=True,
                labels_ok=True,
                result_pass=False,
                final_substrings_ok=True,
                final_text="RESULT: FAIL\nFILES: none\nTEST: N/A\nNOTE: 已按要求完成当前任务。",
            )
        )

    def test_tool_probe_result_label_drift_allows_negated_failure_words(self) -> None:
        scenario = next(item for item in matrix.SCENARIOS if item.name == "web_search_probe")

        self.assertTrue(
            matrix.should_accept_tool_probe_result_label_drift(
                scenario=scenario,
                signals_ok=True,
                files_ok=True,
                labels_ok=True,
                result_pass=False,
                final_substrings_ok=True,
                final_text="RESULT: FAIL\nFILES: none\nTEST: N/A\nNOTE: completed without error; no failures detected.",
            )
        )

    def test_tool_probe_result_label_drift_accepts_subagent_content_signal(self) -> None:
        scenario = next(item for item in matrix.SCENARIOS if item.name == "subagent_probe")

        self.assertTrue(
            matrix.should_accept_tool_probe_result_label_drift(
                scenario=scenario,
                signals_ok=True,
                files_ok=True,
                labels_ok=True,
                result_pass=False,
                final_substrings_ok=True,
                final_text=(
                    "RESULT: FAIL\n"
                    "FILES: README.md\n"
                    "TEST: N/A\n"
                    "NOTE: # firew2oai"
                ),
            )
        )

    def test_tool_probe_result_label_drift_keeps_explicit_tool_failures_failed(self) -> None:
        scenario = next(item for item in matrix.SCENARIOS if item.name == "web_search_probe")

        self.assertFalse(
            matrix.should_accept_tool_probe_result_label_drift(
                scenario=scenario,
                signals_ok=True,
                files_ok=True,
                labels_ok=True,
                result_pass=False,
                final_substrings_ok=True,
                final_text=(
                    "RESULT: FAIL\n"
                    "FILES: none\n"
                    "TEST: N/A\n"
                    "NOTE: web search backend blocked request with DuckDuckGo challenge"
                ),
            )
        )

    def test_tool_probe_result_label_drift_keeps_unnegated_error_failed(self) -> None:
        scenario = next(item for item in matrix.SCENARIOS if item.name == "web_search_probe")

        self.assertFalse(
            matrix.should_accept_tool_probe_result_label_drift(
                scenario=scenario,
                signals_ok=True,
                files_ok=True,
                labels_ok=True,
                result_pass=False,
                final_substrings_ok=True,
                final_text="RESULT: FAIL\nFILES: none\nTEST: N/A\nNOTE: tool returned an error before completion.",
            )
        )

    def test_tool_probe_result_label_drift_does_not_rewrite_artifact(self) -> None:
        output_dir = Path(tempfile.mkdtemp(prefix="matrix-result-drift-"))
        self.addCleanup(lambda: __import__("shutil").rmtree(output_dir, ignore_errors=True))
        last_path = output_dir / "case.last.txt"
        last_path.write_text("RESULT: FAIL\nFILES: none\nTEST: N/A\nNOTE: done\n", encoding="utf-8")
        scenario = next(item for item in matrix.SCENARIOS if item.name == "web_search_probe")

        self.assertTrue(
            matrix.should_accept_tool_probe_result_label_drift(
                scenario=scenario,
                signals_ok=True,
                files_ok=True,
                labels_ok=True,
                result_pass=False,
                final_substrings_ok=True,
                final_text=last_path.read_text(encoding="utf-8"),
            )
        )
        self.assertIn("RESULT: FAIL", last_path.read_text(encoding="utf-8"))

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

    def test_detect_declared_tools_intersects_explicit_list_with_runtime_observation(self) -> None:
        output_dir = Path(tempfile.mkdtemp(prefix="matrix-tools-"))
        self.addCleanup(lambda: __import__("shutil").rmtree(output_dir, ignore_errors=True))
        worktree = output_dir / "probe-worktree"

        def fake_run(cmd, cwd=None, timeout=None, input_text=None):
            if cmd[:2] == ["docker", "logs"]:
                return subprocess.CompletedProcess(
                    args=cmd,
                    returncode=0,
                    stdout='msg="responses request" prompt="TOOL_DISCOVERY_PROBE_123" tool_names="[exec_command js_repl]"\n',
                    stderr="",
                )
            return subprocess.CompletedProcess(args=cmd, returncode=0, stdout="", stderr="")

        with patch.dict(
            "os.environ",
            {
                "CODEX_MATRIX_DECLARED_TOOLS": "exec_command,apply_patch,js_repl",
                "CODEX_MATRIX_HISTORY_CONTAINER": "firew2oai",
            },
            clear=False,
        ), patch.object(matrix, "time") as mock_time, patch.object(
            matrix, "create_worktree", return_value=worktree
        ), patch.object(
            matrix, "remove_worktree"
        ), patch.object(
            matrix, "run", side_effect=fake_run
        ):
            mock_time.time.return_value = 123
            self.assertEqual(
                matrix.detect_declared_tools(output_dir, "codex", "model-a", 30),
                ["exec_command", "js_repl"],
            )

    def test_unsupported_required_tools_keeps_prompt_dynamic_apply_patch_runnable(self) -> None:
        scenario = next(item for item in matrix.SCENARIOS if item.name == "apply_patch_probe")

        self.assertEqual(
            matrix.unsupported_required_tools(scenario, {"exec_command", "js_repl"}),
            [],
        )

    def test_unsupported_required_tools_still_skips_missing_mcp_tools(self) -> None:
        scenario = next(item for item in matrix.SCENARIOS if item.name == "docfork_probe")

        self.assertEqual(
            matrix.unsupported_required_tools(scenario, {"exec_command"}),
            ["mcp__docfork__search_docs", "mcp__docfork__fetch_doc"],
        )

    def test_docfork_probe_result_is_based_on_tool_success_not_doc_title(self) -> None:
        scenario = next(item for item in matrix.SCENARIOS if item.name == "docfork_probe")

        self.assertIn("RESULT 只表示 Docfork 工具是否真实调用并返回内容", scenario.prompt)
        self.assertIn("文档标题或内容中的 Error 不代表本测试失败", scenario.prompt)

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

    def test_classify_failure_reason_marks_docfork_rate_limited(self) -> None:
        self.assertEqual(
            matrix.classify_failure_reason(
                exit_code="0",
                expected_signals=("docfork", "search_docs", "fetch_doc"),
                observed_signals=["docfork", "search_docs"],
                final_preview='RESULT: FAIL\nNOTE: 429 Too Many Requests: {"message":"Monthly rate limit exceeded"}',
                stderr_preview="",
                labels_ok=True,
                result_pass=False,
            ),
            "docfork_rate_limited",
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
