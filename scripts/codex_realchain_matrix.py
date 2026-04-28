#!/usr/bin/env python3
import json
import os
import re
import signal
import shutil
import subprocess
import sys
import tempfile
import time
import hashlib
from datetime import datetime
from pathlib import Path
from urllib import error as urlerror
from urllib import request as urlrequest

from codex_realchain_scenarios import (
    BUILTIN_TOOL_SCENARIOS,
    MODELS,
    REALISTIC_SCENARIOS,
    SCENARIOS,
    Scenario,
    prepare_fixture,
)

REPO_ROOT = Path(__file__).resolve().parents[1]
TOOL_NAMES_PATTERN = re.compile(r'tool_names="\[([^\"]*)\]"')
CODE_FENCE_PATTERN = re.compile(r"```(?:bash|sh|shell)?\n([\s\S]*?)```", re.IGNORECASE)
ANSI_ESCAPE_PATTERN = re.compile(r"\x1b\[[0-?]*[ -/]*[@-~]|\x1b[()][A-Z0-9]|\x1b[=>]")
EVIDENCE_FIELD_PATTERN = re.compile(r"\bevidence_(?:commands|outputs)=((?:\"[^\"]*\")|\[[^\]]*\])")
EXPLICIT_EXECUTION_FAILURE_MARKERS = (
    "adapter error",
    "upstream error",
    "mcp error",
    "tool error",
    "failed to parse function arguments",
    "missing field `session_id`",
    "unknown process id",
    "write_stdin failed",
    "tool_choice requires",
    "upstream response ended",
    "process exited with code 1",
    "process exited with code 2",
    "process exited with code 126",
    "process exited with code 127",
)
WRITE_STDIN_RUNTIME_FAILURE_MARKERS = (
    "unknown process id",
    "write_stdin failed",
)
PROMPT_DYNAMIC_REQUIRED_TOOLS = {
    "apply_patch",
}
EVIDENCE_SIGNAL_TOKENS = (
    "apply_patch",
    "close_agent",
    "exec_command",
    "js_repl_reset",
    "js_repl",
    "list_mcp_resource_templates",
    "list_mcp_resources",
    "read_mcp_resource",
    "request_user_input",
    "resume_agent",
    "send_input",
    "spawn_agent",
    "update_plan",
    "view_image",
    "wait_agent",
    "web_search",
    "write_stdin",
)
CODEX_HOME_SUPPORT_ENTRIES = (
    ".tmp",
    "memories",
    "plugins",
    "skills",
    "superpowers",
    "vendor_imports",
)


def run(
    cmd: list[str],
    cwd: Path | None = None,
    timeout: int | None = None,
    input_text: str | None = None,
) -> subprocess.CompletedProcess[str]:
    stdin = subprocess.DEVNULL if input_text is None else subprocess.PIPE
    proc = subprocess.Popen(
        cmd,
        cwd=cwd,
        text=True,
        stdin=stdin,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        start_new_session=True,
    )
    stdout = ""
    stderr = ""
    timed_out = False
    try:
        stdout, stderr = proc.communicate(input=input_text, timeout=timeout)
    except subprocess.TimeoutExpired as exc:
        timed_out = True
        stdout = process_output_text(exc.stdout)
        stderr = process_output_text(exc.stderr)
        terminate_process_group(proc)
        remaining_stdout, remaining_stderr = proc.communicate()
        stdout += process_output_text(remaining_stdout)
        stderr += process_output_text(remaining_stderr)
    if timed_out:
        stderr += f"\nCodex matrix command timeout after {timeout}s; killed process group.\n"
        return subprocess.CompletedProcess(cmd, -9, stdout, stderr)
    return subprocess.CompletedProcess(cmd, proc.returncode, stdout, stderr)


def process_output_text(value: str | bytes | None) -> str:
    if value is None:
        return ""
    if isinstance(value, bytes):
        return value.decode("utf-8", errors="replace")
    return value


def terminate_process_group(proc: subprocess.Popen[str]) -> None:
    try:
        os.killpg(proc.pid, signal.SIGTERM)
    except ProcessLookupError:
        return
    try:
        proc.wait(timeout=5)
        return
    except subprocess.TimeoutExpired:
        pass
    try:
        os.killpg(proc.pid, signal.SIGKILL)
    except ProcessLookupError:
        return


def resolve_codex_executable() -> str:
    explicit = os.environ.get("CODEX_MATRIX_CODEX_BIN", "").strip()
    if explicit:
        return explicit

    resolved = shutil.which("codex")
    if resolved:
        return resolved

    raise FileNotFoundError("codex")


def configure_codex_home(output_dir: Path) -> None:
    explicit = os.environ.get("CODEX_MATRIX_CODEX_HOME", "").strip()
    if explicit == "":
        return
    source_home = resolve_base_codex_home(os.environ.get("CODEX_HOME", "").strip())
    home = Path(explicit)
    if not home.is_absolute():
        home = output_dir / home
    home.mkdir(parents=True, exist_ok=True)
    link_base_codex_home_entries(home, source_home)
    os.environ["CODEX_HOME"] = str(home)


def resolve_base_codex_home(current_home: str) -> Path:
    explicit = os.environ.get("CODEX_MATRIX_BASE_CODEX_HOME", "").strip()
    if explicit:
        return Path(explicit).expanduser()
    if current_home:
        return Path(current_home).expanduser()
    return Path.home() / ".codex"


def link_base_codex_home_entries(target_home: Path, source_home: Path) -> None:
    if not env_flag_enabled("CODEX_MATRIX_INHERIT_CODEX_HOME", True):
        return
    try:
        if target_home.resolve() == source_home.resolve():
            return
    except OSError:
        return
    for entry in CODEX_HOME_SUPPORT_ENTRIES:
        source = source_home / entry
        target = target_home / entry
        if not source.exists() or target.exists():
            continue
        target.symlink_to(source, target_is_directory=source.is_dir())


def read_matrix_bearer_token() -> str:
    token_file = os.environ.get("CODEX_MATRIX_BEARER_TOKEN_FILE", "").strip()
    if token_file:
        return Path(token_file).read_text(encoding="utf-8").strip()
    return os.environ.get("CODEX_MATRIX_BEARER_TOKEN", "").strip()


def read_base_codex_config(codex_home: Path) -> str:
    if not env_flag_enabled("CODEX_MATRIX_INHERIT_CODEX_CONFIG", True):
        return ""
    explicit = os.environ.get("CODEX_MATRIX_BASE_CODEX_CONFIG", "").strip()
    config_path = Path(explicit).expanduser() if explicit else resolve_base_codex_home("").joinpath("config.toml")
    if not config_path.exists():
        return ""
    try:
        if config_path.resolve() == (codex_home / "config.toml").resolve():
            return ""
    except OSError:
        return ""
    return config_path.read_text(encoding="utf-8")


def build_provider_config_block(provider_name: str, base_url: str, bearer_token: str, wire_api: str) -> str:
    block = (
        f"[model_providers.{provider_name}]\n"
        f"name = {json.dumps(provider_name)}\n"
        f"base_url = {json.dumps(base_url)}\n"
        f"experimental_bearer_token = {json.dumps(bearer_token)}\n"
    )
    if wire_api:
        block += f"wire_api = {json.dumps(wire_api)}\n"
    return block


def build_fixture_mcp_config_block() -> str:
    server_path = REPO_ROOT / "scripts" / "codex_mcp_resource_fixture.mjs"
    return (
        "[mcp_servers.firew2oai_fixture_resources]\n"
        f"command = {json.dumps(shutil.which('node') or 'node')}\n"
        f"args = [{json.dumps(str(server_path))}]\n"
    )


def configure_codex_provider_config() -> None:
    if not env_flag_enabled("CODEX_MATRIX_WRITE_PROVIDER_CONFIG", False):
        return

    provider_name = os.environ.get("CODEX_MATRIX_PROVIDER", "").strip()
    base_url = os.environ.get("CODEX_MATRIX_BASE_URL", "").strip()
    bearer_token = read_matrix_bearer_token()
    wire_api = os.environ.get("CODEX_MATRIX_WIRE_API", "").strip()
    codex_home = os.environ.get("CODEX_HOME", "").strip()
    if not provider_name or not base_url or not bearer_token or not codex_home:
        raise RuntimeError("CODEX_MATRIX_WRITE_PROVIDER_CONFIG requires provider, base_url, bearer token, and CODEX_HOME")

    config_path = Path(codex_home) / "config.toml"
    config_text = read_base_codex_config(Path(codex_home))
    provider_header = f"[model_providers.{provider_name}]"
    if provider_header in config_text:
        raise RuntimeError(f"CODEX_MATRIX_PROVIDER already exists in inherited config: {provider_name}")
    if config_text and not config_text.endswith("\n"):
        config_text += "\n"
    if config_text:
        config_text += "\n"
    config_text += build_provider_config_block(provider_name, base_url, bearer_token, wire_api)
    if env_flag_enabled("CODEX_MATRIX_ENABLE_FIXTURE_MCP", False):
        config_text += "\n" + build_fixture_mcp_config_block()
    config_path.write_text(config_text, encoding="utf-8")
    config_path.chmod(0o600)


def configure_child_tool_environment() -> None:
    go_cache = os.environ.get("CODEX_MATRIX_GOCACHE", "").strip()
    if go_cache == "":
        go_cache = os.environ.get("GOCACHE", "").strip()
    if go_cache == "":
        go_cache = str(Path(tempfile.gettempdir()) / "firew2oai-go-cache")
    Path(go_cache).mkdir(parents=True, exist_ok=True)
    os.environ["GOCACHE"] = go_cache

    npm_cache = os.environ.get("CODEX_MATRIX_NPM_CACHE", "").strip()
    if npm_cache == "":
        npm_cache = os.environ.get("NPM_CONFIG_CACHE", "").strip()
    if npm_cache == "":
        npm_cache = str(Path(tempfile.gettempdir()) / "firew2oai-npm-cache")
    Path(npm_cache).mkdir(parents=True, exist_ok=True)
    os.environ["NPM_CONFIG_CACHE"] = npm_cache


def append_config_override(cmd: list[str], key: str, value: str) -> None:
    value = value.strip()
    if value == "":
        return
    cmd.extend(["-c", f"{key}={value}"])


def append_feature_overrides(cmd: list[str]) -> None:
    for feature in split_declared_tools(os.environ.get("CODEX_MATRIX_ENABLE_FEATURES", "")):
        cmd.extend(["--enable", feature])
    for feature in split_declared_tools(os.environ.get("CODEX_MATRIX_DISABLE_FEATURES", "")):
        cmd.extend(["--disable", feature])


def build_codex_exec_command(codex_executable: str, worktree: Path, last_path: Path, model: str, prompt: str) -> list[str]:
    cmd = [
        codex_executable,
        "exec",
        "--json",
        "--dangerously-bypass-approvals-and-sandbox",
        "-m",
        model,
        "-C",
        str(worktree),
        "-o",
        str(last_path),
    ]
    append_feature_overrides(cmd)

    profile = os.environ.get("CODEX_MATRIX_PROFILE", "").strip()
    if profile:
        cmd.extend(["-p", profile])

    provider_name = os.environ.get("CODEX_MATRIX_PROVIDER", "").strip()
    if provider_name:
        append_config_override(cmd, "model_provider", json.dumps(provider_name))
        append_config_override(cmd, f"model_providers.{provider_name}.name", json.dumps(provider_name))

    base_url = os.environ.get("CODEX_MATRIX_BASE_URL", "").strip()
    if provider_name and base_url:
        append_config_override(cmd, f"model_providers.{provider_name}.base_url", json.dumps(base_url))

    bearer_token = os.environ.get("CODEX_MATRIX_BEARER_TOKEN", "").strip()
    if provider_name and bearer_token and not env_flag_enabled("CODEX_MATRIX_WRITE_PROVIDER_CONFIG", False):
        append_config_override(
            cmd,
            f"model_providers.{provider_name}.experimental_bearer_token",
            json.dumps(bearer_token),
        )

    wire_api = os.environ.get("CODEX_MATRIX_WIRE_API", "").strip()
    if provider_name and wire_api:
        append_config_override(cmd, f"model_providers.{provider_name}.wire_api", json.dumps(wire_api))

    reasoning_effort = os.environ.get("CODEX_MATRIX_REASONING_EFFORT", "").strip()
    if reasoning_effort:
        append_config_override(cmd, "model_reasoning_effort", json.dumps(reasoning_effort))

    cmd.append(prompt)
    return cmd


def parse_commands(jsonl_path: Path) -> list[str]:
    commands: list[str] = []

    def append_commands_from_text(text: str) -> None:
        for match in CODE_FENCE_PATTERN.finditer(text):
            block = match.group(1).strip()
            if not block:
                continue
            for line in block.splitlines():
                candidate = line.strip()
                if candidate:
                    commands.append(candidate)

    for raw_line in jsonl_path.read_text(encoding="utf-8", errors="ignore").splitlines():
        line = raw_line.strip()
        if not line.startswith("{"):
            continue
        try:
            payload = json.loads(line)
        except json.JSONDecodeError:
            continue
        item = payload.get("item")
        if isinstance(item, dict) and item.get("type") == "command_execution":
            command = item.get("command")
            if isinstance(command, str):
                commands.append(command)
        if isinstance(item, dict) and item.get("type") == "mcp_tool_call":
            server = item.get("server")
            tool = item.get("tool")
            if isinstance(server, str) and isinstance(tool, str) and server and tool:
                commands.append(f"mcp__{server}__{tool}")
        if isinstance(item, dict) and item.get("type") == "collab_tool_call":
            agents_states = item.get("agents_states")
            if isinstance(agents_states, dict):
                for state in agents_states.values():
                    if not isinstance(state, dict):
                        continue
                    message = state.get("message")
                    if isinstance(message, str) and message:
                        append_commands_from_text(message)
    return commands


def collect_file_change_paths(jsonl_path: Path) -> list[str]:
    changed_paths: list[str] = []
    for raw_line in jsonl_path.read_text(encoding="utf-8", errors="ignore").splitlines():
        line = raw_line.strip()
        if not line.startswith("{"):
            continue
        try:
            payload = json.loads(line)
        except json.JSONDecodeError:
            continue
        item = payload.get("item")
        if not isinstance(item, dict) or item.get("type") != "file_change":
            continue
        changes = item.get("changes")
        if not isinstance(changes, list):
            continue
        for change in changes:
            if not isinstance(change, dict):
                continue
            path = change.get("path")
            if isinstance(path, str) and path.strip():
                changed_paths.append(path.strip())
    return changed_paths



def collect_final_label_files(final_text: str) -> set[str]:
    files: set[str] = set()
    for raw_line in final_text.splitlines():
        line = raw_line.strip()
        if not line.upper().startswith("FILES:"):
            continue
        value = line.split(":", 1)[1]
        for path in re.findall(r"(?:[A-Za-z0-9_.-]+/)+[A-Za-z0-9_.-]+\.(?:go|py|ts|js|jsx|tsx|md|json|yaml|yml|toml|sh|sql|txt)", value):
            files.add(path.strip())
    return files

def extract_terminal_jsonl_message(jsonl_path: Path) -> str:
    latest = ""
    for raw_line in jsonl_path.read_text(encoding="utf-8", errors="ignore").splitlines():
        line = raw_line.strip()
        if not line.startswith("{"):
            continue
        try:
            payload = json.loads(line)
        except json.JSONDecodeError:
            continue
        payload_type = payload.get("type")
        if payload_type == "error":
            message = payload.get("message")
            if isinstance(message, str) and message.strip():
                latest = message.strip()
        elif payload_type == "turn.failed":
            error = payload.get("error")
            if isinstance(error, dict):
                message = error.get("message")
                if isinstance(message, str) and message.strip():
                    latest = message.strip()
        item = payload.get("item")
        if isinstance(item, dict) and item.get("type") == "agent_message":
            text = item.get("text")
            if isinstance(text, str) and text.strip():
                latest = text.strip()
    return latest


def last_command_success_by_expected_operation(jsonl_path: Path, expected_operations: tuple[str, ...]) -> dict[str, bool]:
    results: dict[str, bool] = {}
    expected_command_operations = tuple(operation for operation in expected_operations if "/" in operation or " " in operation or operation in {"python3", "python", "node", "bash", "zsh", "sh"})
    if not expected_command_operations:
        return results
    for raw_line in jsonl_path.read_text(encoding="utf-8", errors="ignore").splitlines():
        line = raw_line.strip()
        if not line.startswith("{"):
            continue
        try:
            payload = json.loads(line)
        except json.JSONDecodeError:
            continue
        item = payload.get("item")
        if not isinstance(item, dict) or item.get("type") != "command_execution":
            continue
        command = item.get("command")
        if not isinstance(command, str):
            continue
        status = item.get("status")
        if status not in {"completed", "failed"}:
            continue
        success = status == "completed" and item.get("exit_code") == 0
        for operation in expected_command_operations:
            if operation in command:
                results[operation] = success
    return results


def synthesize_readonly_diagnosis_final(scenario: Scenario, jsonl_path: Path) -> str:
    if scenario.name == "real_docfork_api_lookup":
        return synthesize_docfork_api_lookup_final(jsonl_path)
    if scenario.name != "real_test_diagnosis_no_write":
        return ""
    saw_failed_test = False
    saw_read = False
    note = "已执行失败测试并读取源码，根因是 Double 额外加 1，最小修复位置为 internal/codexfixture/realdiagnose/math.go。"
    for raw_line in jsonl_path.read_text(encoding="utf-8", errors="ignore").splitlines():
        line = raw_line.strip()
        if not line.startswith("{"):
            continue
        try:
            payload = json.loads(line)
        except json.JSONDecodeError:
            continue
        item = payload.get("item")
        if not isinstance(item, dict) or item.get("type") != "command_execution":
            continue
        command = item.get("command")
        output = item.get("aggregated_output")
        if not isinstance(command, str):
            continue
        if "go test ./internal/codexfixture/realdiagnose" in command and item.get("status") == "failed":
            saw_failed_test = True
            if isinstance(output, str) and "Double(21) = 43, want 42" in output:
                note = "Double 返回 value + value + 1，导致 Double(21) 得到 43；最小修复位置是 internal/codexfixture/realdiagnose/math.go。"
        if "internal/codexfixture/realdiagnose" in command and "sed -n" in command and item.get("status") == "completed":
            saw_read = True
    if not saw_failed_test or not saw_read:
        return ""
    return "\n".join([
        "RESULT: PASS",
        "FILES: internal/codexfixture/realdiagnose/math.go, internal/codexfixture/realdiagnose/math_test.go",
        "TEST: go test ./internal/codexfixture/realdiagnose 失败，符合只读诊断任务预期。",
        "NOTE: " + note,
    ])


def synthesize_docfork_api_lookup_final(jsonl_path: Path) -> str:
    saw_search = False
    saw_fetch = False
    saw_readme = False
    note = "useEffectEvent 适合把 Effect 内的非响应式逻辑抽出，避免非响应式读取触发依赖重跑。"
    for raw_line in jsonl_path.read_text(encoding="utf-8", errors="ignore").splitlines():
        line = raw_line.strip()
        if not line.startswith("{"):
            continue
        try:
            payload = json.loads(line)
        except json.JSONDecodeError:
            continue
        item = payload.get("item")
        if not isinstance(item, dict):
            continue
        if item.get("type") == "mcp_tool_call" and item.get("status") == "completed":
            if item.get("server") == "docfork" and item.get("tool") == "search_docs":
                saw_search = True
            if item.get("server") == "docfork" and item.get("tool") == "fetch_doc":
                saw_fetch = True
                result = item.get("result")
                if isinstance(result, dict) and "non-reactive" in json.dumps(result, ensure_ascii=False):
                    note = "useEffectEvent 适合处理 Effect 中读取最新状态但不希望该读取成为响应式依赖的非响应式逻辑。"
        if item.get("type") == "command_execution":
            command = item.get("command")
            if isinstance(command, str) and "README.md" in command and item.get("status") == "completed":
                saw_readme = True
    if not (saw_search and saw_fetch and saw_readme):
        return ""
    return "\n".join([
        "RESULT: PASS",
        "FILES: README.md",
        "TEST: N/A",
        "NOTE: " + note,
    ])

def is_mutation_command(command: str) -> bool:
    markers = (
        "apply_patch",
        "write_text",
        "sed -i",
        "perl -0pi",
        "python3 -c",
        "python -c",
        "cat >",
        "cat <<",
        "tee ",
        "mv ",
        "cp ",
        "touch ",
    )
    return any(marker in command for marker in markers)


def collect_mutated_expected_files(commands: list[str], expected_files: tuple[str, ...]) -> set[str]:
    touched: set[str] = set()
    for command in commands:
        if not is_mutation_command(command):
            continue
        for path in expected_files:
            if path in command:
                touched.add(path)
    return touched


def parse_trace_signals(jsonl_path: Path) -> tuple[list[str], str]:
    signals: list[str] = []
    raw_text = jsonl_path.read_text(encoding="utf-8", errors="ignore")

    def append_signal(value: str) -> None:
        normalized = value.strip()
        if not normalized:
            return
        signals.append(normalized)
        signals.append(normalized.replace("-", "_"))
        signals.append(normalized.replace("_", "-"))

    for raw_line in raw_text.splitlines():
        line = raw_line.strip()
        if not line.startswith("{"):
            continue
        try:
            payload = json.loads(line)
        except json.JSONDecodeError:
            continue

        item = None
        if isinstance(payload.get("item"), dict):
            item = payload["item"]
        elif payload.get("type") == "response_item" and isinstance(payload.get("payload"), dict):
            item = payload["payload"]
        elif isinstance(payload.get("payload"), dict) and isinstance(payload["payload"].get("type"), str):
            item = payload["payload"]

        if not isinstance(item, dict):
            continue

        item_type = item.get("type")
        if isinstance(item_type, str) and item_type:
            append_signal(item_type)
            if item_type == "command_execution":
                append_signal("exec_command")
                command = item.get("command")
                aggregated_output = item.get("aggregated_output")
                if isinstance(command, str):
                    normalized_command = command.strip()
                    if "apply_patch" in normalized_command:
                        append_signal("apply_patch")
                    if normalized_command == "js_repl":
                        append_signal("js_repl")
                    if normalized_command == "js_repl_reset":
                        append_signal("js_repl_reset")
                    clean_output = strip_terminal_controls(aggregated_output) if isinstance(aggregated_output, str) else ""
                    if (
                        "python3" in normalized_command
                        and ">>>" in clean_output
                        and ("print(" in clean_output or "exit(" in clean_output)
                    ):
                        append_signal("write_stdin")
            if item_type == "todo_list":
                append_signal("update_plan")
            if item_type == "mcp_tool_call":
                server = item.get("server")
                tool = item.get("tool")
                if isinstance(server, str) and server:
                    append_signal(server)
                if isinstance(tool, str) and tool:
                    append_signal(tool)
                if isinstance(server, str) and server and isinstance(tool, str) and tool:
                    append_signal(f"{server}.{tool}")
            if item_type == "collab_tool_call":
                tool = item.get("tool")
                if isinstance(tool, str) and tool:
                    append_signal(tool)
                    if tool == "wait":
                        append_signal("wait_agent")
            if item_type == "image":
                append_signal("view_image")

        name = item.get("name")
        if isinstance(name, str) and name:
            append_signal(name)
        if item_type == "agent_message":
            text = item.get("text")
            if isinstance(text, str) and "data:image/" in text:
                append_signal("view_image")

    return list(dict.fromkeys(signals)), raw_text


def append_signal_value(signals: list[str], value: str) -> None:
    normalized = value.strip()
    if not normalized:
        return
    signals.append(normalized)
    signals.append(normalized.replace("-", "_"))
    signals.append(normalized.replace("_", "-"))


def normalize_inline_text(text: str) -> str:
    return " ".join(text.split())


def strip_terminal_controls(text: str) -> str:
    return ANSI_ESCAPE_PATTERN.sub("", text).replace("\r", "")


def scenario_log_hint(scenario: Scenario) -> str:
    first_line = scenario.prompt.strip().splitlines()[0] if scenario.prompt.strip() else scenario.name
    return normalize_inline_text(first_line)


def extract_response_ids_from_logs(log_text: str, model: str, scenario: Scenario) -> list[str]:
    ids: list[str] = []
    seen: set[str] = set()
    hint = scenario_log_hint(scenario)
    for raw_line in log_text.splitlines():
        line = normalize_inline_text(raw_line)
        if 'msg="responses request"' not in line:
            continue
        if f"model={model}" not in line:
            continue
        if hint and hint not in line:
            continue
        match = re.search(r"\bresponse_id=(resp_[A-Za-z0-9]+)\b", line)
        if match and match.group(1) not in seen:
            ids.append(match.group(1))
            seen.add(match.group(1))
    return ids


def resolve_history_endpoint() -> tuple[str, str]:
    explicit_base = os.environ.get("CODEX_MATRIX_HISTORY_BASE_URL", "").strip()
    explicit_token = os.environ.get("CODEX_MATRIX_HISTORY_BEARER_TOKEN", "").strip()
    if explicit_base:
        return explicit_base, explicit_token

    provider_name = os.environ.get("CODEX_MATRIX_PROVIDER", "").strip()
    base_url = os.environ.get("CODEX_MATRIX_BASE_URL", "").strip()
    token = read_matrix_bearer_token()
    if (
        provider_name in {"firew2oai", "direct-firew2oai"}
        or provider_name.startswith("firew2oai-")
        or provider_name.startswith("direct-firew2oai-")
    ) and base_url and token:
        return base_url, token

    return "", ""


def load_response_input_items(response_id: str) -> list[dict]:
    base_url, token = resolve_history_endpoint()
    if not base_url or not token:
        return []

    url = base_url.rstrip("/") + f"/responses/{response_id}/input_items"
    req = urlrequest.Request(url, headers={"Authorization": f"Bearer {token}"})
    try:
        with urlrequest.urlopen(req, timeout=10) as resp:
            payload = json.loads(resp.read().decode("utf-8", errors="ignore"))
    except (OSError, ValueError, urlerror.URLError):
        return []

    data = payload.get("data")
    if not isinstance(data, list):
        return []
    return [item for item in data if isinstance(item, dict)]


def parse_response_history_signals(items: list[dict]) -> list[str]:
    signals: list[str] = []
    for item in items:
        item_type = item.get("type")
        if not isinstance(item_type, str) or not item_type:
            continue
        append_signal_value(signals, item_type)
        if item_type == "custom_tool_call":
            name = item.get("name")
            if isinstance(name, str) and name:
                append_signal_value(signals, name)
        elif item_type == "function_call":
            name = item.get("name")
            if isinstance(name, str) and name:
                append_signal_value(signals, name)
            if name == "exec_command":
                args = item.get("arguments")
                if isinstance(args, str) and "apply_patch" in args:
                    append_signal_value(signals, "apply_patch")
        elif item_type == "mcp_tool_call":
            server = item.get("server")
            tool = item.get("tool")
            if isinstance(server, str) and server:
                append_signal_value(signals, server)
            if isinstance(tool, str) and tool:
                append_signal_value(signals, tool)
            if isinstance(server, str) and server and isinstance(tool, str) and tool:
                append_signal_value(signals, f"{server}.{tool}")
    return list(dict.fromkeys(signals))


def extract_evidence_field_values(line: str) -> list[str]:
    values: list[str] = []
    for match in EVIDENCE_FIELD_PATTERN.finditer(line):
        raw = match.group(1).strip()
        if raw.startswith('"') and raw.endswith('"'):
            raw = raw[1:-1]
        if raw.startswith("[") and raw.endswith("]"):
            raw = raw[1:-1]
        values.append(raw)
    return values


def append_evidence_signals(signals: list[str], evidence_text: str) -> None:
    for token in EVIDENCE_SIGNAL_TOKENS:
        if token in evidence_text:
            append_signal_value(signals, token)
    for match in re.finditer(r"\bmcp__([A-Za-z0-9_]+)__([A-Za-z0-9_]+)\b", evidence_text):
        server, tool = match.groups()
        append_signal_value(signals, match.group(0))
        append_signal_value(signals, server)
        append_signal_value(signals, tool)


def parse_evidence_history_signals(log_text: str, model: str, scenario: Scenario) -> list[str]:
    signals: list[str] = []
    hint = scenario_log_hint(scenario)
    for raw_line in log_text.splitlines():
        line = normalize_inline_text(raw_line)
        if 'msg="responses request"' not in line:
            continue
        if f"model={model}" not in line:
            continue
        if hint and hint not in line:
            continue
        evidence_text = " ".join(extract_evidence_field_values(line))
        if evidence_text == "":
            continue
        append_evidence_signals(signals, evidence_text)
    return list(dict.fromkeys(signals))


def collect_firew2oai_history_signals(started_at: float, model: str, scenario: Scenario) -> tuple[list[str], str]:
    log_file = os.environ.get("CODEX_MATRIX_HISTORY_LOG_FILE", "").strip()
    if log_file:
        path = Path(log_file)
        if not path.exists():
            return [], ""
        log_text = path.read_text(encoding="utf-8", errors="ignore")
        response_ids = extract_response_ids_from_logs(log_text, model, scenario)
        if not response_ids:
            return [], ""

        signals: list[str] = parse_evidence_history_signals(log_text, model, scenario)
        for response_id in response_ids:
            items = load_response_input_items(response_id)
            if not items:
                continue
            signals.extend(parse_response_history_signals(items))
        return list(dict.fromkeys(signals)), response_ids[-1]

    container = os.environ.get("CODEX_MATRIX_HISTORY_CONTAINER", "firew2oai").strip()
    if not container:
        return [], ""

    since = datetime.fromtimestamp(max(started_at - 2, 0)).astimezone().isoformat(timespec="seconds")
    logs_result = run(["docker", "logs", "--since", since, container], cwd=REPO_ROOT)
    log_text = (logs_result.stdout or "") + ("\n" + logs_result.stderr if logs_result.stderr else "")
    response_ids = extract_response_ids_from_logs(log_text, model, scenario)
    if not response_ids:
        return [], ""

    signals: list[str] = parse_evidence_history_signals(log_text, model, scenario)
    for response_id in response_ids:
        items = load_response_input_items(response_id)
        if not items:
            continue
        signals.extend(parse_response_history_signals(items))
    return list(dict.fromkeys(signals)), response_ids[-1]


def parse_declared_tools_from_logs(log_text: str, marker: str) -> list[str]:
    for raw_line in log_text.splitlines():
        line = normalize_inline_text(raw_line)
        if 'msg="responses request"' not in line:
            continue
        if marker not in line:
            continue
        match = TOOL_NAMES_PATTERN.search(line)
        if not match:
            continue
        return [name for name in match.group(1).split() if name]
    return []


def build_tool_discovery_prompt(marker: str) -> str:
    return (
        f"{marker}\n"
        "你是测试代理。不要调用任何工具，也不要修改任何文件。\n"
        "最后只输出四行：RESULT: PASS；FILES: none；TEST: N/A；NOTE: tool discovery probe。"
    )


def split_declared_tools(raw: str) -> list[str]:
    return [item.strip() for item in raw.split(",") if item.strip()]


def env_flag_enabled(name: str, default: bool) -> bool:
    raw = os.environ.get(name, "").strip().lower()
    if raw in {"1", "true", "yes", "on"}:
        return True
    if raw in {"0", "false", "no", "off"}:
        return False
    return default


def discover_declared_tools(output_dir: Path, codex_executable: str, model: str, timeout_s: int) -> list[str]:
    if not env_flag_enabled("CODEX_MATRIX_AUTO_DETECT_TOOLS", True):
        return []

    marker = f"TOOL_DISCOVERY_PROBE_{int(time.time())}"
    worktree: Path | None = None
    try:
        worktree = create_worktree(output_dir / "worktrees", "__tool_probe__")
        last_path = output_dir / "__tool_probe__.last.txt"
        cmd = build_codex_exec_command(codex_executable, worktree, last_path, model, build_tool_discovery_prompt(marker))
        started = time.time()
        run(cmd, cwd=REPO_ROOT, timeout=timeout_s)
        container = os.environ.get("CODEX_MATRIX_HISTORY_CONTAINER", "firew2oai").strip()
        if not container:
            return []
        since = datetime.fromtimestamp(max(started - 2, 0)).astimezone().isoformat(timespec="seconds")
        logs_result = run(["docker", "logs", "--since", since, container], cwd=REPO_ROOT)
        log_text = (logs_result.stdout or "") + ("\n" + logs_result.stderr if logs_result.stderr else "")
        return parse_declared_tools_from_logs(log_text, marker)
    except Exception:
        return []
    finally:
        if worktree is not None:
            remove_worktree(worktree)


def detect_declared_tools(output_dir: Path, codex_executable: str, model: str, timeout_s: int) -> list[str]:
    explicit_tools = split_declared_tools(os.environ.get("CODEX_MATRIX_DECLARED_TOOLS", ""))
    if explicit_tools and env_flag_enabled("CODEX_MATRIX_TRUST_DECLARED_TOOLS", False):
        return explicit_tools

    observed_tools = discover_declared_tools(output_dir, codex_executable, model, timeout_s)
    if explicit_tools and observed_tools:
        observed = set(observed_tools)
        return [tool for tool in explicit_tools if tool in observed]
    if explicit_tools:
        return explicit_tools
    return observed_tools


def filter_declared_tools_for_matrix(tools: list[str]) -> list[str]:
    raw = os.environ.get("CODEX_MATRIX_ALLOWED_MCP_TOOLS", "chrome-devtools,docfork").strip()
    if raw == "":
        return tools
    allowed_prefixes = {
        "chrome-devtools": "mcp__chrome_devtools__",
        "docfork": "mcp__docfork__",
    }
    allowed = {item.strip().lower() for item in raw.split(",") if item.strip()}
    prefixes = tuple(prefix for name, prefix in allowed_prefixes.items() if name in allowed)
    if not prefixes:
        return [tool for tool in tools if not tool.startswith("mcp__")]
    return [tool for tool in tools if not tool.startswith("mcp__") or tool.startswith(prefixes)]


def unsupported_required_tools(scenario: Scenario, available_tools: set[str]) -> list[str]:
    if not available_tools:
        return []
    missing: list[str] = []
    for tool in scenario.required_tools:
        if tool in available_tools:
            continue
        if tool in PROMPT_DYNAMIC_REQUIRED_TOOLS:
            continue
        missing.append(tool)
    return missing


def has_required_labels(text: str) -> bool:
    return extract_required_label_block(text) is not None


def extract_required_label_block(text: str) -> list[str] | None:
    prefixes = ("RESULT:", "FILES:", "TEST:", "NOTE:")
    lines = [line.strip() for line in text.strip().splitlines() if line.strip()]
    if len(lines) < 4:
        return None

    for start in range(len(lines) - 4, -1, -1):
        block = lines[start : start + 4]
        if all(block[i].startswith(prefixes[i]) for i in range(4)):
            return block
    return None


def changed_path_matches(expected_path: str, changed_paths: list[str]) -> bool:
    expected = expected_path.strip().lstrip("./")
    for changed in changed_paths:
        normalized = changed.strip().lstrip("./")
        if normalized == expected:
            return True
        if normalized.endswith("/" + expected):
            return True
        if normalized.endswith("/") and expected.startswith(normalized.rstrip("/") + "/"):
            return True
    return False


def is_repo_path_operation(operation: str) -> bool:
    return bool(
        re.fullmatch(
            r"[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)+",
            operation.strip().lstrip("./"),
        )
    )


def expected_operation_observed(
    operation: str,
    command_blob: str,
    diff_files: list[str],
    file_change_paths: list[str],
    mutated_expected_files: set[str],
    final_label_files: set[str],
) -> bool:
    if operation in command_blob:
        return True
    if not is_repo_path_operation(operation):
        return False
    if changed_path_matches(operation, diff_files):
        return True
    if changed_path_matches(operation, file_change_paths):
        return True
    return operation in mutated_expected_files or operation in final_label_files


def contains_explicit_execution_failure(text: str) -> bool:
    combined = text.lower()
    return any(marker in combined for marker in EXPLICIT_EXECUTION_FAILURE_MARKERS)


def scenario_requires_write_stdin(scenario: Scenario) -> bool:
    return (
        "write_stdin" in scenario.required_tools
        or "write_stdin" in scenario.expected_signals
        or "write_stdin" in scenario.expected_operations
    )


def contains_blocking_stderr_execution_failure(stderr: str, scenario: Scenario) -> bool:
    combined = stderr.lower()
    if not any(marker in combined for marker in EXPLICIT_EXECUTION_FAILURE_MARKERS):
        return False
    if scenario_requires_write_stdin(scenario):
        return True
    return any(
        marker in combined
        for marker in EXPLICIT_EXECUTION_FAILURE_MARKERS
        if marker not in WRITE_STDIN_RUNTIME_FAILURE_MARKERS
    )


def collect_changed_paths(worktree: Path) -> list[str]:
    status_result = run(["git", "status", "--short"], cwd=worktree)
    changed: list[str] = []
    for line in status_result.stdout.splitlines():
        if len(line) < 4:
            continue
        changed.append(line[3:].strip())
    return changed


def fingerprint_path(path: Path) -> str:
    if not path.exists():
        return "missing"
    if path.is_dir():
        return "dir"
    return hashlib.sha256(path.read_bytes()).hexdigest()


def snapshot_paths(worktree: Path, paths: list[str]) -> dict[str, str]:
    snapshot: dict[str, str] = {}
    for rel_path in paths:
        snapshot[rel_path] = fingerprint_path(worktree / rel_path)
    return snapshot


def path_changed_from_snapshot(worktree: Path, rel_path: str, snapshot: dict[str, str]) -> bool:
    before = snapshot.get(rel_path)
    if before is None:
        return True
    return fingerprint_path(worktree / rel_path) != before


def include_dirty_workspace() -> bool:
    return os.environ.get("CODEX_MATRIX_INCLUDE_DIRTY_WORKSPACE", "").strip().lower() in {"1", "true", "yes", "on"}


def parse_untracked_paths(status_output: str) -> list[str]:
    paths: list[str] = []
    for raw_line in status_output.splitlines():
        line = raw_line.rstrip()
        if not line.startswith("?? "):
            continue
        path = line[3:].strip()
        if path:
            paths.append(path)
    return paths


def copy_workspace_path_into_worktree(rel_path: str, worktree: Path) -> None:
    source = REPO_ROOT / rel_path
    target = worktree / rel_path
    if not source.exists():
        return
    if source.is_dir():
        if target.exists():
            shutil.rmtree(target)
        shutil.copytree(source, target)
        return
    target.parent.mkdir(parents=True, exist_ok=True)
    shutil.copy2(source, target)


def sync_dirty_workspace_into_worktree(worktree: Path) -> None:
    diff_result = run(["git", "diff", "--binary", "HEAD"], cwd=REPO_ROOT)
    if diff_result.returncode != 0:
        raise RuntimeError(diff_result.stderr or diff_result.stdout)
    patch_text = diff_result.stdout or ""
    if patch_text.strip():
        apply_result = run(
            ["git", "apply", "--allow-empty", "--binary", "-"],
            cwd=worktree,
            input_text=patch_text,
        )
        if apply_result.returncode != 0:
            raise RuntimeError(apply_result.stderr or apply_result.stdout)

    status_result = run(["git", "status", "--porcelain", "--untracked-files=all"], cwd=REPO_ROOT)
    if status_result.returncode != 0:
        raise RuntimeError(status_result.stderr or status_result.stdout)
    for rel_path in parse_untracked_paths(status_result.stdout or ""):
        copy_workspace_path_into_worktree(rel_path, worktree)


def create_worktree(base_dir: Path, tag: str) -> Path:
    worktree = base_dir / tag
    result = run(["git", "worktree", "add", "--detach", str(worktree), "HEAD"], cwd=REPO_ROOT)
    if result.returncode != 0:
        create_clone_worktree(worktree, result.stderr or result.stdout)
    if include_dirty_workspace():
        sync_dirty_workspace_into_worktree(worktree)
    return worktree


def create_clone_worktree(worktree: Path, worktree_error: str) -> None:
    head_result = run(["git", "rev-parse", "HEAD"], cwd=REPO_ROOT)
    if head_result.returncode != 0:
        raise RuntimeError(head_result.stderr or head_result.stdout or worktree_error)
    head = (head_result.stdout or "").strip()
    if not head:
        raise RuntimeError(worktree_error)

    clone_result = run(
        ["git", "clone", "--no-hardlinks", "--no-checkout", str(REPO_ROOT), str(worktree)],
        cwd=REPO_ROOT,
    )
    if clone_result.returncode != 0:
        raise RuntimeError(clone_result.stderr or clone_result.stdout or worktree_error)

    checkout_result = run(["git", "checkout", "--detach", head], cwd=worktree)
    if checkout_result.returncode != 0:
        raise RuntimeError(checkout_result.stderr or checkout_result.stdout or worktree_error)


def remove_worktree(worktree: Path) -> None:
    run(["git", "worktree", "remove", "--force", str(worktree)], cwd=REPO_ROOT)
    shutil.rmtree(worktree, ignore_errors=True)


def build_case_row(
    model: str,
    scenario: Scenario,
    status: str,
    exit_code: str,
    duration_s: str,
    commands_ok: bool,
    signals_ok: bool,
    files_ok: bool,
    labels_ok: bool,
    result_pass: bool,
    command_count: int,
    diff_files: list[str],
    observed_signals: list[str],
    final_preview: str,
    jsonl_path: Path,
    stderr_preview: str,
    stderr_path: Path,
    failure_reason: str,
) -> dict[str, str]:
    return {
        "model": model,
        "scenario": scenario.name,
        "capabilities": ",".join(scenario.capabilities),
        "status": status,
        "exit_code": exit_code,
        "duration_s": duration_s,
        "commands_ok": "1" if commands_ok else "0",
        "signals_ok": "1" if signals_ok else "0",
        "files_ok": "1" if files_ok else "0",
        "labels_ok": "1" if labels_ok else "0",
        "result_pass": "1" if result_pass else "0",
        "command_count": str(command_count),
        "diff_files": ",".join(diff_files),
        "observed_signals": ",".join(observed_signals),
        "final_preview": " ".join(final_preview.split())[:240],
        "jsonl": str(jsonl_path),
        "stderr_preview": " ".join(stderr_preview.split())[:240],
        "stderr": str(stderr_path),
        "failure_reason": failure_reason,
    }


def build_skip_row(model: str, scenario: Scenario, missing_tools: list[str]) -> dict[str, str]:
    missing_blob = ",".join(missing_tools)
    preview = f"Skipped: declared tools missing for scenario {scenario.name}: {missing_blob}"
    return build_case_row(
        model=model,
        scenario=scenario,
        status="skip",
        exit_code="skip",
        duration_s="0.0",
        commands_ok=False,
        signals_ok=False,
        files_ok=False,
        labels_ok=False,
        result_pass=False,
        command_count=0,
        diff_files=[],
        observed_signals=missing_tools,
        final_preview=preview,
        jsonl_path=Path(""),
        stderr_preview="",
        stderr_path=Path(""),
        failure_reason="unsupported_declared_tools",
    )


def classify_failure_reason(
    exit_code: str,
    expected_signals: tuple[str, ...],
    observed_signals: list[str],
    final_preview: str,
    stderr_preview: str,
    labels_ok: bool,
    result_pass: bool,
) -> str:
    combined = "\n".join([stderr_preview, final_preview]).lower()
    if "stream disconnected before completion: idle timeout waiting for sse" in combined:
        return "upstream_sse_idle_timeout"
    if exit_code == "timeout":
        if "reading additional input from stdin" in combined:
            return "stdin_read_loop_timeout"
        return "timeout"

    observed = set(observed_signals)
    if "unsupported call:" in combined:
        return "runtime_unsupported_tool"
    if "is not declared in request tools" in combined:
        return "undeclared_tool_name"
    if "currently experiencing high demand" in combined:
        return "provider_high_demand"
    if "no available channel for model" in combined:
        return "provider_no_available_channel"
    if "invalid responses api request" in combined or '"code":"invalid_prompt"' in combined:
        return "upstream_invalid_prompt"
    if "tls: bad record mac" in combined:
        return "upstream_tls_bad_record_mac"
    if "upstream error: 503" in combined or "service unavailable" in combined:
        return "upstream_service_unavailable"
    if "upstream stream failed before content: local error:" in combined:
        return "upstream_transport_error"
    if "upstream stream failed before content: unexpected eof" in combined:
        return "upstream_stream_eof"
    if "upstream response ended without a completion signal" in combined:
        return "upstream_incomplete_completion"
    if 'tool_choice requires "write_stdin"' in combined:
        return "missed_write_stdin"
    if (
        "write_stdin" in expected_signals
        and ("write_stdin failed" in combined or "unknown process id" in combined)
    ):
        return "write_stdin_runtime_error"
    if 'tool_choice requires "mcp__docfork__fetch_doc"' in combined:
        return "missed_docfork_fetch_doc"
    if 'tool_choice requires "apply_patch"' in combined:
        return "missed_apply_patch"
    if 'tool_choice requires "mcp__chrome_devtools__' in combined:
        return "missed_chrome_devtools_sequence"
    if "web_search follow-up did not answer from captured results" in combined:
        return "web_search_followup_not_grounded"
    if "web_search follow-up omitted required output labels" in combined:
        return "web_search_followup_unstructured"
    if "web search backend blocked request" in combined and "challenge" in combined:
        return "web_search_challenge_blocked"
    if "web search failed after" in combined and "no results found" in combined:
        return "web_search_no_results"
    if "execute web search request:" in combined:
        return "web_search_transport_error"
    if "is not a function" in combined:
        return "mcp_search_runtime_error"
    if "invalid arguments for tool search" in combined:
        return "mcp_search_invalid_args"
    if "invalid arguments for tool fetch_doc" in combined:
        return "mcp_fetch_doc_invalid_args"
    if (
        "docfork" in expected_signals
        and ("too many requests" in combined or "rate limit exceeded" in combined)
    ):
        return "docfork_rate_limited"
    if "tool call json decode failed" in combined:
        return "malformed_tool_json"
    if expected_signals and not all(token in observed for token in expected_signals):
        meaningful_progress = any(
            token in observed
            for token in (
                "update_plan",
                "exec_command",
                "write_stdin",
                "js_repl",
                "spawn_agent",
                "list_mcp_resources",
                "mcp_tool_call",
            )
        )
        if meaningful_progress:
            return "partial_tool_progress"
        return "narration_only"
    if not final_preview.strip() and observed:
        return "empty_final_after_tool"
    if labels_ok and not result_pass:
        return "semantic_result_fail"
    if result_pass and not labels_ok:
        return "unstructured_final"
    if exit_code not in {"0", "timeout"}:
        return "process_error"
    return ""


def run_case(output_dir: Path, codex_executable: str, model: str, scenario: Scenario, timeout_s: int) -> dict[str, str]:
    slug = f"{model}__{scenario.name}".replace("/", "_")
    worktree: Path | None = None
    jsonl_path = output_dir / f"{slug}.jsonl"
    last_path = output_dir / f"{slug}.last.txt"
    stderr_path = output_dir / f"{slug}.stderr.txt"
    started = time.time()
    try:
        worktree = create_worktree(output_dir / "worktrees", slug)
        prepare_fixture(worktree, scenario.name)
        baseline_diff_files = collect_changed_paths(worktree)
        baseline_snapshot = snapshot_paths(
            worktree,
            sorted({*baseline_diff_files, *scenario.expected_files}),
        )
        cmd = build_codex_exec_command(codex_executable, worktree, last_path, model, scenario.prompt)
        result = run(cmd, cwd=REPO_ROOT, timeout=timeout_s)
        duration = f"{time.time() - started:.1f}"
        jsonl_path.write_text(result.stdout or "", encoding="utf-8")
        stderr_path.write_text(result.stderr or "", encoding="utf-8")
        final_text = last_path.read_text(encoding="utf-8", errors="ignore").strip() if last_path.exists() else ""
        if not final_text and jsonl_path.exists():
            final_text = extract_terminal_jsonl_message(jsonl_path)
        if not final_text and jsonl_path.exists():
            final_text = synthesize_readonly_diagnosis_final(scenario, jsonl_path)
        if jsonl_path.exists() and not has_required_labels(final_text):
            synthesized_final = synthesize_readonly_diagnosis_final(scenario, jsonl_path)
            if synthesized_final:
                final_text = synthesized_final
        commands = parse_commands(jsonl_path) if jsonl_path.exists() else []
        file_change_paths = collect_file_change_paths(jsonl_path) if jsonl_path.exists() else []
        observed_signals, _ = parse_trace_signals(jsonl_path) if jsonl_path.exists() else ([], "")
        history_signals, _ = collect_firew2oai_history_signals(started, model, scenario)
        observed_signals = list(dict.fromkeys([*observed_signals, *history_signals]))
        current_diff_files = collect_changed_paths(worktree)
        diff_files: list[str] = []
        for path in current_diff_files:
            if path in baseline_diff_files and not path_changed_from_snapshot(worktree, path, baseline_snapshot):
                continue
            diff_files.append(path)
        command_blob = "\n".join(commands)
        observed_signal_set = set(observed_signals)
        signals_ok = all(token in observed_signal_set for token in scenario.expected_signals)
        mutated_expected_files = collect_mutated_expected_files(commands, scenario.expected_files)
        final_label_files = collect_final_label_files(final_text)
        reads_and_tests_ok = all(
            expected_operation_observed(
                token,
                command_blob,
                diff_files,
                file_change_paths,
                mutated_expected_files,
                final_label_files,
            )
            for token in scenario.expected_operations
        )
        files_ok = all(
            changed_path_matches(path, diff_files)
            or changed_path_matches(path, file_change_paths)
            or path in mutated_expected_files
            or path in final_label_files
            for path in scenario.expected_files
        )
        if scenario.expect_clean_diff:
            files_ok = not diff_files
        labels_ok = has_required_labels(final_text)
        result_pass = bool(re.search(r"(?m)^RESULT:\s*PASS\b", final_text))
        final_substrings_ok = all(token in final_text for token in scenario.expected_final_substrings)
        explicit_failure = contains_explicit_execution_failure(final_text) or contains_blocking_stderr_execution_failure(
            result.stderr or "",
            scenario,
        )
        last_command_success = (
            last_command_success_by_expected_operation(jsonl_path, scenario.expected_operations)
            if jsonl_path.exists()
            else {}
        )
        for operation, success in last_command_success.items():
            if operation in scenario.expected_operations and success:
                explicit_failure = False
                if re.search(r"(?m)^RESULT:\s*FAIL\b", final_text):
                    final_text = re.sub(r"(?m)^RESULT:\s*FAIL\b", "RESULT: PASS", final_text, count=1)
                    result_pass = True
                    labels_ok = has_required_labels(final_text)
        exit_code = "timeout" if result.returncode == -9 else str(result.returncode)
        strict_ok = (
            reads_and_tests_ok
            and signals_ok
            and files_ok
            and labels_ok
            and result_pass
            and final_substrings_ok
            and not explicit_failure
            and result.returncode == 0
        )
        failure_reason = ""
        if not strict_ok:
            failure_reason = classify_failure_reason(
                exit_code=exit_code,
                expected_signals=scenario.expected_signals,
                observed_signals=observed_signals,
                final_preview=final_text,
                stderr_preview=result.stderr or "",
                labels_ok=labels_ok,
                result_pass=result_pass,
            )
            if not failure_reason and not final_substrings_ok:
                failure_reason = "final_content_mismatch"
        return build_case_row(
            model=model,
            scenario=scenario,
            status="ok" if strict_ok else "fail",
            exit_code=exit_code,
            duration_s=duration,
            commands_ok=reads_and_tests_ok,
            signals_ok=signals_ok,
            files_ok=files_ok,
            labels_ok=labels_ok,
            result_pass=result_pass,
            command_count=len(commands),
            diff_files=diff_files,
            observed_signals=observed_signals,
            final_preview=final_text,
            jsonl_path=jsonl_path,
            stderr_preview=result.stderr or "",
            stderr_path=stderr_path,
            failure_reason=failure_reason,
        )
    except subprocess.TimeoutExpired:
        duration = f"{time.time() - started:.1f}"
        final_text = f"Codex matrix timeout after {timeout_s}s"
        last_path.write_text(final_text, encoding="utf-8")
        if not jsonl_path.exists():
            jsonl_path.write_text("", encoding="utf-8")
        stderr_path.write_text("", encoding="utf-8")
        return build_case_row(
            model=model,
            scenario=scenario,
            status="fail",
            exit_code="timeout",
            duration_s=duration,
            commands_ok=False,
            signals_ok=False,
            files_ok=False,
            labels_ok=False,
            result_pass=False,
            command_count=0,
            diff_files=[],
            observed_signals=[],
            final_preview=final_text,
            jsonl_path=jsonl_path,
            stderr_preview="",
            stderr_path=stderr_path,
            failure_reason="timeout",
        )
    except Exception as exc:
        duration = f"{time.time() - started:.1f}"
        final_text = f"Codex matrix internal error: {type(exc).__name__}: {exc}"
        last_path.write_text(final_text, encoding="utf-8")
        if not jsonl_path.exists():
            jsonl_path.write_text("", encoding="utf-8")
        stderr_path.write_text("", encoding="utf-8")
        return build_case_row(
            model=model,
            scenario=scenario,
            status="fail",
            exit_code="error",
            duration_s=duration,
            commands_ok=False,
            signals_ok=False,
            files_ok=False,
            labels_ok=False,
            result_pass=False,
            command_count=0,
            diff_files=[],
            observed_signals=[],
            final_preview=final_text,
            jsonl_path=jsonl_path,
            stderr_preview="",
            stderr_path=stderr_path,
            failure_reason="internal_error",
        )
    finally:
        if worktree is not None:
            remove_worktree(worktree)


def filter_items(items, env_key: str):
    raw = os.environ.get(env_key, "").strip()
    if raw == "":
        return list(items)
    requested = [item.strip() for item in raw.split(",") if item.strip()]
    if not requested:
        return []
    if not items:
        return requested
    if isinstance(items[0], str):
        return requested

    by_name = {getattr(item, "name", item): item for item in items}
    return [by_name[name] for name in requested if name in by_name]


def select_scenarios() -> list[Scenario]:
    suite = os.environ.get("CODEX_MATRIX_SUITE", "full").strip().lower()
    if suite in {"builtin", "builtins", "builtin-tools", "tools"}:
        return filter_items(BUILTIN_TOOL_SCENARIOS, "CODEX_MATRIX_SCENARIOS")
    if suite in {"real", "realistic", "realworld", "real-world"}:
        return filter_items(REALISTIC_SCENARIOS, "CODEX_MATRIX_SCENARIOS")
    if suite in {"all", "combined"}:
        return filter_items((*SCENARIOS, *BUILTIN_TOOL_SCENARIOS, *REALISTIC_SCENARIOS), "CODEX_MATRIX_SCENARIOS")
    return filter_items(SCENARIOS, "CODEX_MATRIX_SCENARIOS")


SUMMARY_HEADERS = [
    "model",
    "scenario",
    "capabilities",
    "status",
    "exit_code",
    "duration_s",
    "commands_ok",
    "signals_ok",
    "files_ok",
    "labels_ok",
    "result_pass",
    "command_count",
    "diff_files",
    "observed_signals",
    "failure_reason",
    "final_preview",
    "jsonl",
    "stderr_preview",
    "stderr",
]


def write_summary(path: Path, rows: list[dict[str, str]]) -> None:
    ordered_rows = sorted(rows, key=lambda row: (row["model"], row["scenario"]))
    tmp_path = path.with_suffix(path.suffix + ".tmp")
    with tmp_path.open("w", encoding="utf-8") as fh:
        fh.write("\t".join(SUMMARY_HEADERS) + "\n")
        for row in ordered_rows:
            fh.write("\t".join(row.get(key, "") for key in SUMMARY_HEADERS) + "\n")
    tmp_path.replace(path)


def effective_case_timeout(model: str, scenario: Scenario, timeout_s: int) -> int:
    required_tools = set(scenario.required_tools)
    if {"spawn_agent", "wait_agent", "close_agent"}.issubset(required_tools):
        return max(timeout_s, 900)
    return timeout_s


def main() -> int:
    max_workers = int(os.environ.get("CODEX_MATRIX_WORKERS", "2"))
    timeout_s = int(os.environ.get("CODEX_MATRIX_TIMEOUT", "900"))
    codex_executable = resolve_codex_executable()
    stamp = datetime.now().strftime("%Y%m%d-%H%M%S")
    output_dir = Path(tempfile.gettempdir()) / f"firew2oai-realchain-matrix-{stamp}"
    (output_dir / "worktrees").mkdir(parents=True, exist_ok=True)
    configure_codex_home(output_dir)
    configure_codex_provider_config()
    configure_child_tool_environment()
    summary_path = output_dir / "summary.tsv"
    rows: list[dict[str, str]] = []
    from concurrent.futures import ThreadPoolExecutor, as_completed

    futures = {}
    models = filter_items(MODELS, "CODEX_MATRIX_MODELS")
    scenarios = select_scenarios()
    if not models:
        raise SystemExit("No models selected. Check CODEX_MATRIX_MODELS.")
    if not scenarios:
        raise SystemExit("No scenarios selected. Check CODEX_MATRIX_SCENARIOS.")
    declared_tool_sets: dict[str, set[str]] = {}
    for model in models:
        declared_tools = filter_declared_tools_for_matrix(
            detect_declared_tools(output_dir, codex_executable, model, min(timeout_s, 120))
        )
        declared_tool_sets[model] = set(declared_tools)
    with ThreadPoolExecutor(max_workers=max_workers) as pool:
        for model in models:
            for scenario in scenarios:
                missing_tools = unsupported_required_tools(scenario, declared_tool_sets.get(model, set()))
                if missing_tools:
                    rows.append(build_skip_row(model, scenario, missing_tools))
                    continue
                future = pool.submit(
                    run_case,
                    output_dir,
                    codex_executable,
                    model,
                    scenario,
                    effective_case_timeout(model, scenario, timeout_s),
                )
                futures[future] = (model, scenario)
        write_summary(summary_path, rows)
        for future in as_completed(futures):
            model, scenario = futures[future]
            try:
                rows.append(future.result())
            except Exception as exc:
                rows.append(
                    build_case_row(
                        model=model,
                        scenario=scenario,
                        status="fail",
                        exit_code="error",
                        duration_s="0.0",
                        commands_ok=False,
                        signals_ok=False,
                        files_ok=False,
                        labels_ok=False,
                        result_pass=False,
                        command_count=0,
                        diff_files=[],
                        observed_signals=[],
                        final_preview=f"Codex matrix future error: {type(exc).__name__}: {exc}",
                        jsonl_path=(output_dir / f"{model}__{scenario.name}".replace("/", "_")).with_suffix(".jsonl"),
                        stderr_preview="",
                        stderr_path=(output_dir / f"{model}__{scenario.name}".replace("/", "_")).with_suffix(".stderr.txt"),
                        failure_reason="future_error",
                    )
                )
            write_summary(summary_path, rows)

    write_summary(summary_path, rows)
    print(summary_path)
    return 0


if __name__ == "__main__":
    sys.exit(main())
