import importlib.util
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

_SCRIPT_PATH = Path(__file__).with_name("benchmark_report.py")
_SPEC = importlib.util.spec_from_file_location("benchmark_report", _SCRIPT_PATH)
benchmark_report = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(benchmark_report)


class _SnapshotRequest:
    """Small request-handler adapter for exercising the snapshot endpoint."""

    _handle_snapshot = benchmark_report._DashboardHandler._handle_snapshot

    def __init__(self, workspace, requested_dir):
        self.workspace = str(workspace)
        self.grafana_url = "http://localhost:3000"
        self.grafana_user = "admin"
        self.grafana_password = "admin"
        self.dashboard_uid = "test-dashboard"
        self.requested_dir = requested_dir
        self.response = None

    def _is_same_origin(self):
        return True

    def _read_json_body(self):
        return {"dir": self.requested_dir}

    def _send_json(self, code, body):
        self.response = (code, body)


class SnapshotWorkspacePathTest(unittest.TestCase):
    def test_snapshot_rejects_metadata_symlink_outside_workspace(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp) / "workspace"
            experiment = root / "session" / "results" / "exp"
            experiment.mkdir(parents=True)
            outside_metadata = Path(tmp) / "outside.yaml"
            outside_metadata.write_text(
                "harness_start: 2026-01-01T00:00:00Z\n"
                "harness_stop: 2026-01-01T00:01:00Z\n"
            )
            (experiment / "run_metadata.yaml").symlink_to(outside_metadata)
            request = _SnapshotRequest(root, "session/results/exp")

            with patch.object(benchmark_report, "_prometheus_datasource_uid") as get_datasource:
                request._handle_snapshot()

            self.assertEqual(request.response[0], 400)
            get_datasource.assert_not_called()
            self.assertIn("harness_start", outside_metadata.read_text())

    def test_snapshot_rejects_path_escape(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp) / "workspace"
            root.mkdir()
            request = _SnapshotRequest(root, "../outside")

            request._handle_snapshot()

            self.assertEqual(request.response[0], 400)

    def test_workspace_writer_rejects_symlink_output(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp) / "workspace"
            experiment = root / "session" / "results" / "exp"
            experiment.mkdir(parents=True)
            outside_file = Path(tmp) / "outside.yaml"
            outside_file.write_text("keep this content")
            (experiment / "grafana_snapshot.yaml").symlink_to(outside_file)

            with self.assertRaises(OSError):
                benchmark_report._open_workspace_file(
                    root,
                    Path("session/results/exp/grafana_snapshot.yaml"),
                    mode="w",
                )

            self.assertEqual(outside_file.read_text(), "keep this content")


if __name__ == "__main__":
    unittest.main()
