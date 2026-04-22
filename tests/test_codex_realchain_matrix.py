import subprocess
import sys
from pathlib import Path
from unittest import TestCase
from unittest.mock import patch


REPO_ROOT = Path(__file__).resolve().parents[1]
SCRIPTS_DIR = REPO_ROOT / "scripts"
if str(SCRIPTS_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPTS_DIR))

import codex_realchain_matrix as matrix


class RunHelperTests(TestCase):
    def test_run_closes_stdin_by_default(self) -> None:
        with patch("subprocess.run") as mock_run:
            matrix.run(["codex", "exec", "hello"], cwd=REPO_ROOT, timeout=30)

        _, kwargs = mock_run.call_args
        self.assertIs(kwargs.get("stdin"), subprocess.DEVNULL)
