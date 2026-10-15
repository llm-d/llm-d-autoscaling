import importlib.util
import json
import math
from pathlib import Path
import unittest


spec = importlib.util.spec_from_file_location(
    "benchmark_report", Path(__file__).resolve().parents[1] / "benchmark_report.py"
)
report = importlib.util.module_from_spec(spec)
spec.loader.exec_module(report)


class SnapshotValuesTest(unittest.TestCase):
    def test_nonfinite_samples_are_json_null_with_timestamps_preserved(self):
        times = [1000, 2000, 3000, 4000, 5000, 6000]
        values = [0.0, math.nan, math.inf, -math.inf, -1.5, 3.25]
        frame = report._prometheus_series_to_frame(
            "A", "test_metric", 15, {"pod": "model-0"}, times, values
        )
        encoded = json.dumps(frame, allow_nan=False)
        self.assertEqual(
            json.loads(encoded)["data"]["values"],
            [times, [0.0, None, None, None, -1.5, 3.25]],
        )
        self.assertTrue(math.isnan(values[1]))

    def test_empty_series_is_preserved(self):
        frame = report._prometheus_series_to_frame("A", "test_metric", 15, {}, [], [])
        self.assertEqual(frame["data"]["values"], [[], []])


if __name__ == "__main__":
    unittest.main()
