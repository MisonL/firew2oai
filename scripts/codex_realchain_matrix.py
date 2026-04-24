#!/usr/bin/env python3
import json
import os
import re
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

from codex_realchain_scenarios import MODELS, SCENARIOS, Scenario, prepare_fixture

REPO_ROOT = Path(__file__).resolve().parents[1]
TOOL_NAMES_PATTERN = re.compile(r'tool_names="\[([^\"]*)\]"')
CODE_FENCE_PATTERN = re.compile(r"```(?:bash|sh|shell)?\n([\s\S]*?)```", re.IGNORECASE)
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


def run(
    cmd: list[str],
    cwd: Path | None = None,
    timeout: int | None = None,
    input_text: str | None = None,
) -> subprocess.CompletedProcess[str]:
    kwargs = {
        "cwd": cwd,
        "text": True,
        "capture_output": True,
        "timeout": timeout,
        "check": False,
    }
    if input_text is None:
        kwargs["stdin"] = subprocess.DEVNULL
    else:
        kwargs["input"] = input_text
    return subprocess.run(cmd, **kwargs)


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
    home = Path(explicit)
    if not home.is_absolute():
        home = output_dir / home
    home.mkdir(parents=True, exist_ok=True)
    os.environ["CODEX_HOME"] = str(home)


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
    if provider_name and bearer_token:
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
    return latest


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
                if isinstance(command, str):
                    normalized_command = command.strip()
                    if normalized_command == "js_repl":
                        append_signal("js_repl")
                    if normalized_command == "js_repl_reset":
                        append_signal("js_repl_reset")
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
    token = os.environ.get("CODEX_MATRIX_BEARER_TOKEN", "").strip()
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

        signals: list[str] = []
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

    signals: list[str] = []
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


def detect_declared_tools(output_dir: Path, codex_executable: str, model: str, timeout_s: int) -> list[str]:
    explicit = os.environ.get("CODEX_MATRIX_DECLARED_TOOLS", "").strip()
    if explicit:
        return [item.strip() for item in explicit.split(",") if item.strip()]

    auto_detect = os.environ.get("CODEX_MATRIX_AUTO_DETECT_TOOLS", "1").strip().lower()
    if auto_detect in {"0", "false", "no"}:
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
    return [tool for tool in scenario.required_tools if tool not in available_tools]


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


def contains_explicit_execution_failure(text: str) -> bool:
    combined = text.lower()
    return any(marker in combined for marker in EXPLICIT_EXECUTION_FAILURE_MARKERS)


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
    if exit_code == "timeout":
        return "timeout"

    combined = "\n".join([stderr_preview, final_preview]).lower()
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
    if "write_stdin failed" in combined or "unknown process id" in combined:
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
    if "execute web search request:" in combined:
        return "web_search_transport_error"
    if "is not a function" in combined:
        return "mcp_search_runtime_error"
    if "invalid arguments for tool search" in combined:
        return "mcp_search_invalid_args"
    if "invalid arguments for tool fetch_doc" in combined:
        return "mcp_fetch_doc_invalid_args"
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
        reads_and_tests_ok = all(token in command_blob for token in scenario.expected_operations)
        observed_signal_set = set(observed_signals)
        signals_ok = all(token in observed_signal_set for token in scenario.expected_signals)
        mutated_expected_files = collect_mutated_expected_files(commands, scenario.expected_files)
        files_ok = all(
            changed_path_matches(path, diff_files)
            or changed_path_matches(path, file_change_paths)
            or path in mutated_expected_files
            for path in scenario.expected_files
        )
        if scenario.expect_clean_diff:
            files_ok = not diff_files
        labels_ok = has_required_labels(final_text)
        result_pass = bool(re.search(r"(?m)^RESULT:\s*PASS\b", final_text))
        final_substrings_ok = all(token in final_text for token in scenario.expected_final_substrings)
        explicit_failure = contains_explicit_execution_failure("\n".join([final_text, result.stderr or ""]))
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
            if not final_substrings_ok:
                failure_reason = "final_content_mismatch"
            else:
                failure_reason = classify_failure_reason(
                    exit_code=str(result.returncode),
                    expected_signals=scenario.expected_signals,
                    observed_signals=observed_signals,
                    final_preview=final_text,
                    stderr_preview=result.stderr or "",
                    labels_ok=labels_ok,
                    result_pass=result_pass,
                )
        return build_case_row(
            model=model,
            scenario=scenario,
            status="ok" if strict_ok else "fail",
            exit_code=str(result.returncode),
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
    configure_child_tool_environment()
    summary_path = output_dir / "summary.tsv"
    rows: list[dict[str, str]] = []
    from concurrent.futures import ThreadPoolExecutor, as_completed

    futures = {}
    models = filter_items(MODELS, "CODEX_MATRIX_MODELS")
    scenarios = filter_items(SCENARIOS, "CODEX_MATRIX_SCENARIOS")
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

    rows.sort(key=lambda row: (row["model"], row["scenario"]))
    headers = [
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
    with summary_path.open("w", encoding="utf-8") as fh:
        fh.write("\t".join(headers) + "\n")
        for row in rows:
            fh.write("\t".join(row.get(key, "") for key in headers) + "\n")
    print(summary_path)
    return 0


if __name__ == "__main__":
    sys.exit(main())
