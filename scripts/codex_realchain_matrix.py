#!/usr/bin/env python3
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import time
from datetime import datetime
from pathlib import Path

from codex_realchain_scenarios import MODELS, SCENARIOS, Scenario, prepare_fixture

REPO_ROOT = Path(__file__).resolve().parents[1]


def run(cmd: list[str], cwd: Path | None = None, timeout: int | None = None) -> subprocess.CompletedProcess[str]:
    return subprocess.run(cmd, cwd=cwd, text=True, capture_output=True, timeout=timeout, check=False)


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
    return commands


def has_required_labels(text: str) -> bool:
    lines = [line.strip() for line in text.strip().splitlines() if line.strip()]
    if len(lines) != 4:
        return False
    return all(lines[i].startswith(prefix) for i, prefix in enumerate(("RESULT:", "FILES:", "TEST:", "NOTE:")))


def changed_path_matches(expected_path: str, changed_paths: list[str]) -> bool:
    for changed in changed_paths:
        if changed == expected_path:
            return True
        if changed.endswith("/") and expected_path.startswith(changed):
            return True
    return False


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
    files_ok: bool,
    labels_ok: bool,
    result_pass: bool,
    command_count: int,
    diff_files: list[str],
    final_preview: str,
    jsonl_path: Path,
) -> dict[str, str]:
    return {
        "model": model,
        "scenario": scenario.name,
        "capabilities": ",".join(scenario.capabilities),
        "status": status,
        "exit_code": exit_code,
        "duration_s": duration_s,
        "commands_ok": "1" if commands_ok else "0",
        "files_ok": "1" if files_ok else "0",
        "labels_ok": "1" if labels_ok else "0",
        "result_pass": "1" if result_pass else "0",
        "command_count": str(command_count),
        "diff_files": ",".join(diff_files),
        "final_preview": " ".join(final_preview.split())[:240],
        "jsonl": str(jsonl_path),
    }


def run_case(output_dir: Path, model: str, scenario: Scenario, timeout_s: int) -> dict[str, str]:
    slug = f"{model}__{scenario.name}".replace("/", "_")
    worktree: Path | None = None
    jsonl_path = output_dir / f"{slug}.jsonl"
    last_path = output_dir / f"{slug}.last.txt"
    started = time.time()
    try:
        worktree = create_worktree(output_dir / "worktrees", slug)
        prepare_fixture(worktree, scenario.name)
        cmd = build_codex_exec_command(worktree, last_path, model, scenario.prompt)
        result = run(cmd, cwd=REPO_ROOT, timeout=timeout_s)
        duration = f"{time.time() - started:.1f}"
        jsonl_path.write_text(result.stdout or "", encoding="utf-8")
        final_text = last_path.read_text(encoding="utf-8", errors="ignore").strip() if last_path.exists() else ""
        commands = parse_commands(jsonl_path) if jsonl_path.exists() else []
        status_result = run(["git", "status", "--short"], cwd=worktree)
        diff_files = []
        for line in status_result.stdout.splitlines():
            if len(line) < 4:
                continue
            diff_files.append(line[3:].strip())
        command_blob = "\n".join(commands)
        reads_and_tests_ok = all(token in command_blob for token in scenario.expected_operations)
        files_ok = all(changed_path_matches(path, diff_files) for path in scenario.expected_files)
        if scenario.expect_clean_diff:
            files_ok = not diff_files
        labels_ok = has_required_labels(final_text)
        result_pass = bool(re.search(r"(?m)^RESULT:\s*PASS\b", final_text))
        strict_ok = reads_and_tests_ok and files_ok and labels_ok and result_pass and result.returncode == 0
        return build_case_row(
            model=model,
            scenario=scenario,
            status="ok" if strict_ok else "fail",
            exit_code=str(result.returncode),
            duration_s=duration,
            commands_ok=reads_and_tests_ok,
            files_ok=files_ok,
            labels_ok=labels_ok,
            result_pass=result_pass,
            command_count=len(commands),
            diff_files=diff_files,
            final_preview=final_text,
            jsonl_path=jsonl_path,
        )
    except subprocess.TimeoutExpired:
        duration = f"{time.time() - started:.1f}"
        final_text = f"Codex matrix timeout after {timeout_s}s"
        last_path.write_text(final_text, encoding="utf-8")
        if not jsonl_path.exists():
            jsonl_path.write_text("", encoding="utf-8")
        return build_case_row(
            model=model,
            scenario=scenario,
            status="fail",
            exit_code="timeout",
            duration_s=duration,
            commands_ok=False,
            files_ok=False,
            labels_ok=False,
            result_pass=False,
            command_count=0,
            diff_files=[],
            final_preview=final_text,
            jsonl_path=jsonl_path,
        )
    except Exception as exc:
        duration = f"{time.time() - started:.1f}"
        final_text = f"Codex matrix internal error: {type(exc).__name__}: {exc}"
        last_path.write_text(final_text, encoding="utf-8")
        if not jsonl_path.exists():
            jsonl_path.write_text("", encoding="utf-8")
        return build_case_row(
            model=model,
            scenario=scenario,
            status="fail",
            exit_code="error",
            duration_s=duration,
            commands_ok=False,
            files_ok=False,
            labels_ok=False,
            result_pass=False,
            command_count=0,
            diff_files=[],
            final_preview=final_text,
            jsonl_path=jsonl_path,
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
    with ThreadPoolExecutor(max_workers=max_workers) as pool:
        for model in models:
            for scenario in scenarios:
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
                        files_ok=False,
                        labels_ok=False,
                        result_pass=False,
                        command_count=0,
                        diff_files=[],
                        final_preview=f"Codex matrix future error: {type(exc).__name__}: {exc}",
                        jsonl_path=(output_dir / f"{model}__{scenario.name}".replace("/", "_")).with_suffix(".jsonl"),
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
        "files_ok",
        "labels_ok",
        "result_pass",
        "command_count",
        "diff_files",
        "final_preview",
        "jsonl",
    ]
    with summary_path.open("w", encoding="utf-8") as fh:
        fh.write("\t".join(headers) + "\n")
        for row in rows:
            fh.write("\t".join(row.get(key, "") for key in headers) + "\n")
    print(summary_path)
    return 0


if __name__ == "__main__":
    sys.exit(main())
