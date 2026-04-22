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


def run(cmd: list[str], cwd: Path | None = None, timeout: int | None = None) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        cmd,
        cwd=cwd,
        text=True,
        capture_output=True,
        stdin=subprocess.DEVNULL,
        timeout=timeout,
        check=False,
    )


def append_config_override(cmd: list[str], key: str, value: str) -> None:
    value = value.strip()
    if value == "":
        return
    cmd.extend(["-c", f"{key}={value}"])


def build_codex_exec_command(worktree: Path, last_path: Path, model: str, prompt: str) -> list[str]:
    cmd = [
        "codex",
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
        if match:
            ids.append(match.group(1))
    return ids


def load_response_input_items(response_id: str) -> list[dict]:
    base_url = os.environ.get("CODEX_MATRIX_HISTORY_BASE_URL", "").strip()
    if not base_url:
        base_url = os.environ.get("CODEX_MATRIX_BASE_URL", "").strip()
    if not base_url:
        return []
    token = os.environ.get("CODEX_MATRIX_HISTORY_BEARER_TOKEN", "").strip()
    if not token:
        token = os.environ.get("CODEX_MATRIX_BEARER_TOKEN", "").strip()
    if not token:
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
    container = os.environ.get("CODEX_MATRIX_HISTORY_CONTAINER", "firew2oai").strip()
    if not container:
        return [], ""

    since = datetime.fromtimestamp(max(started_at - 2, 0)).astimezone().isoformat(timespec="seconds")
    logs_result = run(["docker", "logs", "--since", since, container], cwd=REPO_ROOT)
    log_text = (logs_result.stdout or "") + ("\n" + logs_result.stderr if logs_result.stderr else "")
    response_ids = extract_response_ids_from_logs(log_text, model, scenario)
    if not response_ids:
        return [], ""

    response_id = response_ids[-1]
    items = load_response_input_items(response_id)
    if not items:
        return [], response_id
    return parse_response_history_signals(items), response_id


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


def detect_declared_tools(output_dir: Path, model: str, timeout_s: int) -> list[str]:
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
        cmd = build_codex_exec_command(worktree, last_path, model, build_tool_discovery_prompt(marker))
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
    for changed in changed_paths:
        if changed == expected_path:
            return True
        if changed.endswith("/") and expected_path.startswith(changed):
            return True
    return False


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


def create_worktree(base_dir: Path, tag: str) -> Path:
    worktree = base_dir / tag
    result = run(["git", "worktree", "add", "--detach", str(worktree), "HEAD"], cwd=REPO_ROOT)
    if result.returncode != 0:
        raise RuntimeError(result.stderr or result.stdout)
    return worktree


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
    if result_pass and not labels_ok:
        return "unstructured_final"
    if exit_code not in {"0", "timeout"}:
        return "process_error"
    return ""


def run_case(output_dir: Path, model: str, scenario: Scenario, timeout_s: int) -> dict[str, str]:
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
        cmd = build_codex_exec_command(worktree, last_path, model, scenario.prompt)
        result = run(cmd, cwd=REPO_ROOT, timeout=timeout_s)
        duration = f"{time.time() - started:.1f}"
        jsonl_path.write_text(result.stdout or "", encoding="utf-8")
        stderr_path.write_text(result.stderr or "", encoding="utf-8")
        final_text = last_path.read_text(encoding="utf-8", errors="ignore").strip() if last_path.exists() else ""
        commands = parse_commands(jsonl_path) if jsonl_path.exists() else []
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
            changed_path_matches(path, diff_files) or path in mutated_expected_files for path in scenario.expected_files
        )
        if scenario.expect_clean_diff:
            files_ok = not diff_files
        labels_ok = has_required_labels(final_text)
        result_pass = bool(re.search(r"(?m)^RESULT:\s*PASS\b", final_text))
        strict_ok = reads_and_tests_ok and signals_ok and files_ok and labels_ok and result_pass and result.returncode == 0
        failure_reason = ""
        if not strict_ok:
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
    allowed = {item.strip() for item in raw.split(",") if item.strip()}
    return [item for item in items if getattr(item, "name", item) in allowed]


def main() -> int:
    max_workers = int(os.environ.get("CODEX_MATRIX_WORKERS", "2"))
    timeout_s = int(os.environ.get("CODEX_MATRIX_TIMEOUT", "900"))
    stamp = datetime.now().strftime("%Y%m%d-%H%M%S")
    output_dir = Path(tempfile.gettempdir()) / f"firew2oai-realchain-matrix-{stamp}"
    (output_dir / "worktrees").mkdir(parents=True, exist_ok=True)
    summary_path = output_dir / "summary.tsv"
    rows: list[dict[str, str]] = []
    from concurrent.futures import ThreadPoolExecutor, as_completed

    futures = {}
    models = filter_items(MODELS, "CODEX_MATRIX_MODELS")
    scenarios = filter_items(SCENARIOS, "CODEX_MATRIX_SCENARIOS")
    declared_tools = detect_declared_tools(output_dir, models[0], min(timeout_s, 120)) if models else []
    declared_tool_set = set(declared_tools)
    with ThreadPoolExecutor(max_workers=max_workers) as pool:
        for model in models:
            for scenario in scenarios:
                missing_tools = unsupported_required_tools(scenario, declared_tool_set)
                if missing_tools:
                    rows.append(build_skip_row(model, scenario, missing_tools))
                    continue
                future = pool.submit(run_case, output_dir, model, scenario, timeout_s)
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
