import importlib.util
from pathlib import Path
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location(
    "benchmark_report", Path(__file__).resolve().parents[1] / "benchmark_report.py"
)
report = importlib.util.module_from_spec(spec)
spec.loader.exec_module(report)


class SnapshotQueriesTest(unittest.TestCase):
    def query(self):
        return report._query_range(
            "http://grafana", "prometheus", "user", "password",
            'metric{namespace="$namespace"}', "bench", 0, 60, 15,
        )

    def test_query_errors_are_not_empty_series(self):
        for response in [(503, {"error": "unavailable"}),
                         (200, {"status": "error", "error": "query failed"}),
                         (200, None)]:
            with self.subTest(response=response), patch.object(report, "_http", return_value=response):
                with self.assertRaisesRegex(RuntimeError, "query_range failed"):
                    self.query()

    def test_successful_empty_result_is_valid(self):
        with patch.object(report, "_http", return_value=(200, {"status": "success", "data": {"result": []}})):
            self.assertEqual(self.query(), [])

    def test_query_failure_prevents_snapshot_publication(self):
        dashboard = {"spec": {"elements": {"panel": {"spec": {"data": {"spec": {"queries": [
            {"spec": {"refId": "A", "query": {"spec": {"expr": "metric"}}}}
        ]}}}}}}}
        with patch.object(report, "_run_window", return_value=("bench", 0, 60, "experiment")), \
             patch.object(report, "_session_id_from_experiment_dir", return_value="session"), \
             patch.object(report, "_prometheus_datasource_uid", return_value="prometheus"), \
             patch.object(report, "_http", side_effect=[(200, dashboard), (503, {"error": "unavailable"})]) as http:
            with self.assertRaisesRegex(RuntimeError, "query_range failed"):
                report._capture_snapshot("unused", "http://grafana", "user", "password", "dashboard")
            self.assertEqual(http.call_count, 2)
            self.assertTrue(all(call.args[0] == "GET" for call in http.call_args_list))


if __name__ == "__main__":
    unittest.main()
