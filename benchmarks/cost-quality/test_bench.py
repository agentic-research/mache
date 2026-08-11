"""Regression tests for the cost/quality benchmark harness."""

from __future__ import annotations

import importlib.util
import subprocess
import unittest
from pathlib import Path
from unittest import mock


BENCH_PATH = Path(__file__).with_name("bench.py")
SPEC = importlib.util.spec_from_file_location("cost_quality_bench", BENCH_PATH)
assert SPEC is not None and SPEC.loader is not None
bench = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(bench)


class ParseClaudeJSONTests(unittest.TestCase):
    def test_parses_pretty_printed_object_after_log_prefix(self) -> None:
        stdout = '''startup log
{
  "result": "done",
  "usage": {
    "input_tokens": 12,
    "output_tokens": 4
  }
}
trailing log
'''

        parsed = bench.parse_claude_json(stdout)

        self.assertEqual(parsed["result"], "done")
        self.assertEqual(parsed["usage"]["input_tokens"], 12)

    def test_rejects_output_without_a_json_object(self) -> None:
        with self.assertRaisesRegex(ValueError, "JSON object"):
            bench.parse_claude_json("startup log\n{malformed}\ntrailing log")


class RunClaudeTests(unittest.TestCase):
    @mock.patch.object(bench.subprocess, "run")
    def test_invalid_success_output_becomes_an_error_result(self, run: mock.Mock) -> None:
        run.return_value = subprocess.CompletedProcess(
            args=["claude"], returncode=0, stdout="not JSON", stderr=""
        )

        result = bench.run_claude("prompt", Path("."), Path("graph.db"), "normal")

        self.assertEqual(result["stop_reason"], "error")
        self.assertEqual(result["num_turns"], 0)
        self.assertIn("invalid JSON", result["error"])


class ScoreQualityTests(unittest.TestCase):
    def test_discussion_of_error_handling_is_not_scored_as_a_failure(self) -> None:
        response = (
            "Error handling is implemented in `serve.go`. "
            "The handler returns a structured response and the fix preserves the route."
        )

        score = bench.score_quality(response, "code_explanation")

        self.assertGreater(score["total"], 0)

    def test_empty_response_is_scored_as_a_failure(self) -> None:
        self.assertEqual(bench.score_quality("", "debugging")["total"], 0)


if __name__ == "__main__":
    unittest.main()
