"""Tests for blis_run.py's portable run records. Run: python3 -m unittest discover -s <this dir>"""
import importlib.util
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("blis_run", HERE / "blis_run.py")
blis_run = importlib.util.module_from_spec(spec)
spec.loader.exec_module(blis_run)

REPO = HERE.parents[3]  # scripts -> blis-evaluation -> skills -> .claude -> the checkout
LAB = "/home/someone/src/blis-signal-lab"


class PortablePaths(unittest.TestCase):
    def test_repo_root_is_the_checkout(self):
        self.assertEqual(blis_run.REPO, REPO)
        self.assertTrue((blis_run.REPO / "benchmark" / "protocol").is_dir())

    def test_command_names_no_local_path(self):
        cmd = f"{LAB}/blis run --scenarios {REPO}/benchmark/protocol/verdicts/x --output result.json"
        shown = blis_run.portable(cmd, LAB)
        self.assertEqual(shown, "<lab>/blis run --scenarios <repo>/benchmark/protocol/verdicts/x --output result.json")

    def test_lab_scenarios_stay_under_lab(self):
        cmd = f"{LAB}/blis run --scenarios {LAB}/testdata/kernel/aisimulate"
        self.assertEqual(blis_run.portable(cmd, LAB), "<lab>/blis run --scenarios <lab>/testdata/kernel/aisimulate")

    def test_resolve_inverts_portable(self):
        cmd = f"{LAB}/blis run --scenarios {REPO}/benchmark/protocol/verdicts/x"
        self.assertEqual(blis_run.resolve(blis_run.portable(cmd, LAB), LAB), cmd)

    def test_rerun_lab_prefers_the_flag(self):
        self.assertEqual(blis_run.rerun_lab("/other/lab", {"lab": "<lab>"}), str(Path("/other/lab").resolve()))

    def test_rerun_lab_from_an_older_record(self):
        self.assertEqual(blis_run.rerun_lab(None, {"lab": LAB}), LAB)

    def test_rerun_lab_needs_the_flag_for_a_portable_record(self):
        with self.assertRaises(SystemExit):
            blis_run.rerun_lab(None, {"lab": "<lab>"})
        with self.assertRaises(SystemExit):
            blis_run.rerun_lab(None, {})


if __name__ == "__main__":
    unittest.main()
