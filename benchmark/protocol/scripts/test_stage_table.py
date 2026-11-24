"""Tests for stage_table.py's per-pool counting. Run: python3 -m unittest discover -s benchmark/protocol/scripts"""
import importlib.util
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("stage_table", HERE / "stage_table.py")
stage_table = importlib.util.module_from_spec(spec)
spec.loader.exec_module(stage_table)

BLEND = "8192:256,2048:2048,256:4096"


def req(n_in):
    return {"in": n_in, "out": 256, "ok": True, "ttft": 0.5, "tpot": 0.03, "err": None, "t": 1.0}


class PoolRequests(unittest.TestCase):
    def test_every_request_without_a_floor(self):
        reqs = [req(8192), req(2048), req(256)]
        self.assertEqual(stage_table.pool_requests(reqs, None), reqs)

    def test_only_prompts_at_or_over_the_floor(self):
        reqs = [req(8192), req(2048), req(256), req(9000)]
        self.assertEqual([r["in"] for r in stage_table.pool_requests(reqs, 8192)], [8192, 9000])


class Serves(unittest.TestCase):
    def test_any_mix_at_or_over_the_floor(self):
        self.assertTrue(stage_table.serves("8192:256", 8192))
        self.assertTrue(stage_table.serves(BLEND, 8192))
        self.assertFalse(stage_table.serves("2048:2048", 8192))
        self.assertFalse(stage_table.serves("256:4096,2048:2048", 8192))

    def test_every_mix_without_a_floor(self):
        self.assertTrue(stage_table.serves("256:4096", None))


class Need(unittest.TestCase):
    MU = {"8192:256": 1.341, "2048:2048": 1.0, "256:4096": 0.678}

    def test_mix_and_blend(self):
        self.assertEqual(stage_table.need(1.357, "8192:256", self.MU, 3.972, None, 1), 2)
        self.assertEqual(stage_table.need(4.194, BLEND, self.MU, 3.972, None, 1), 2)
        self.assertEqual(stage_table.need(2.796, BLEND, self.MU, 3.972, None, 1), 1)

    def test_a_phase_the_pool_serves_none_of_needs_min_replicas(self):
        self.assertEqual(stage_table.need(1.357, "2048:2048", self.MU, 3.972, 8192, 1), 1)
        self.assertEqual(stage_table.need(1.357, "256:4096", self.MU, 3.972, 8192, 2), 2)

    def test_no_capacity_no_need(self):
        self.assertIsNone(stage_table.need(1.0, "8192:256", None, None, None, 1))
        self.assertIsNone(stage_table.need(1.0, "", self.MU, 1.165, None, 1))


class MuOverrides(unittest.TestCase):
    def test_flags_replace_the_index(self):
        mu, blend = stage_table.capacities({"mu": {"8192:256": 3.078, "2048:2048": 1.0}, "mu_blend": 1.165},
                                           ["8192:256=1.341"], 3.972)
        self.assertEqual(mu, {"8192:256": 1.341, "2048:2048": 1.0})
        self.assertEqual(blend, 3.972)

    def test_index_alone(self):
        self.assertEqual(stage_table.capacities({"mu": {"8192:256": 3.078}, "mu_blend": 1.165}, None, None),
                         ({"8192:256": 3.078}, 1.165))


if __name__ == "__main__":
    unittest.main()
