#!/usr/bin/env python3
"""Self-check with no outside data: build a synthetic llm-d-benchmark directory, draw it
through every input path, and check the outputs. Exit 0 on success.

    python3 -I selftest.py
"""
import json
import os
import re
import subprocess
import sys
import tempfile
import xml.dom.minidom

HERE = os.path.dirname(os.path.abspath(__file__))
CLI = os.path.join(HERE, "load_vs_supply.py")


def run(*a, ok=True):
    r = subprocess.run([sys.executable, "-B", "-I", CLI, *a], capture_output=True, text=True)
    assert (r.returncode == 0) == ok, f"{a}: rc={r.returncode}\n{r.stderr}"
    return r.stderr


def labels(path):
    return re.findall(r">([^<]+)</text>", open(path).read())


def main():
    with tempfile.TemporaryDirectory() as d:
        mock = os.path.join(d, "mock")
        subprocess.run([sys.executable, "-B", "-I", os.path.join(HERE, "make_mock_llmd_benchmark_run.py"), mock],
                       check=True)
        o = lambda n: os.path.join(d, n)  # noqa: E731
        base = ["--loop", "open", "--title", "MOCK DATA selftest"]
        # results dir; trigger from a Prometheus range result, threshold from the ScaledObject
        run(mock, *base, "--scaledobject", f"{mock}/scaledobject.yaml", "--trigger", f"{mock}/trigger.json",
            "--out", o("a.svg"))
        xml.dom.minidom.parse(o("a.svg"))
        lab = " ".join(labels(o("a.svg")))
        assert "Offered input tokens/s" in lab and "Offered output tokens/s" in lab
        assert "What KEDA read" in lab and "ceil(value ÷ 3500)" in lab and "Ready" in lab
        # raw units only: the offered-load CSV equals the client tokens per bin, no scaling
        recs = json.load(open(f"{mock}/per_request_lifecycle_metrics.json"))
        total_in = sum(r["info"]["request_metrics"]["text"]["input_tokens"] for r in recs)
        rows = [r.split(",") for r in open(o("a.csv")).read().splitlines()[1:]]
        assert abs(sum(float(v) for n, t, v, *_ in rows if n == "offered_in") * 30 - total_in) < 1e-6 * total_in + 1
        # SLO attainment: per bin, good / every request sent (errors are misses). Check the bins add
        # back up to the records: sum(attainment x requests in bin) == requests meeting every SLO
        run(mock, *base, "--load", "req", "--slo", "ttft=1", "--slo", "tpot=0.05", "--attainment-target", "0.95",
            "--out", o("s.svg"))
        lab = " ".join(labels(o("s.svg")))
        assert "SLO attainment" in lab and "TTFT ≤ 1 s, TPOT ≤ 50 ms" in lab and "objective 95%" in lab, lab
        rows = [r.split(",") for r in open(o("s.csv")).read().splitlines()[1:]]
        n = {t: float(v) * 30 for k, t, v, *_ in rows if k == "offered_req"}
        att = {t: float(v) for k, t, v, *_ in rows if k == "attainment"}
        good = sum(1 for r in recs if r["error"] is None and r["computed_metrics"]["time_to_first_token"] <= 1
                   and r["computed_metrics"]["time_per_output_token"] <= 0.05)
        errors = sum(1 for r in recs if r["error"] is not None)
        assert errors > 0 and abs(sum(att[t] * n[t] for t in att) - good) < 0.5, (good, errors)
        assert min(att.values()) < 0.5 and max(att.values()) == 1.0
        # a bad SLO key is refused
        run(mock, *base, "--slo", "itl=0.1", "--out", o("s2.svg"), ok=False)
        # two triggers, one panel each, explicit thresholds and names
        run(mock, *base, "--trigger", f"{mock}/trigger.json=3500:work", "--trigger", f"{mock}/trigger.json=7000:other",
            "--out", o("m.svg"))
        assert "trigger work " in " ".join(labels(o("m.svg"))) and "trigger other " in " ".join(labels(o("m.svg")))
        # several ScaledObjects in one file: refuse to guess which; pick by name or by scaleTargetRef
        multi = f"{mock}/scaledobjects-multi.yaml"
        trig2 = ["--trigger", f"{mock}/trigger.json:work", "--trigger", f"{mock}/kv.json:kv"]
        run(mock, *base, "--scaledobject", multi, *trig2, "--out", o("n.svg"), ok=False)
        err = run(mock, *base, "--scaledobject", multi, "--scaledobject-name", "mock", *trig2, "--out", o("n.svg"))
        lab = " ".join(labels(o("n.svg")))
        # each trigger gets its own threshold and its own metricType's rule
        assert "ceil(value ÷ 3500)" in lab and "Value: HPA asks ceil(current replicas × value ÷ 0.8)" in lab, lab
        assert "threshold 3500 AverageValue (from ScaledObject mock)" in err
        # --deployment picks the ScaledObject whose scaleTargetRef is that Deployment
        err = run(mock, *base, "--deployment", "mock-decode", "--scaledobject", multi, *trig2, "--out", o("n2.svg"))
        assert "from ScaledObject mock)" in err
        # a trigger name the ScaledObject does not have: drawn, no threshold, and a note
        err = run(mock, *base, "--scaledobject", multi, "--scaledobject-name", "mock",
                  "--trigger", f"{mock}/trigger.json:nope", "--out", o("u.svg"))
        assert "no trigger named 'nope'" in err
        # scalingModifiers: the note says the HPA reads the composite; :composite takes its target
        err = run(mock, *base, "--scaledobject", multi, "--scaledobject-name", "mock-composite",
                  "--trigger", f"{mock}/trigger.json:composite", "--out", o("c2.svg"))
        assert "scalingModifiers" in err and "ceil(value ÷ 3500)" in " ".join(labels(o("c2.svg")))
        # HPA desired replicas: from a Prometheus range result, or from autoscaler_timeseries.json
        err = run(mock, *base, "--hpa-desired", f"{mock}/hpa_desired.json", "--out", o("d.svg"))
        assert "HPA desired" in " ".join(labels(o("d.svg"))) and "kube_horizontalpodautoscaler" in err
        assert "HPA desired    : not available" in run(mock, *base, "--out", o("d0.svg"))
        import shutil
        shutil.copy(f"{mock}.extra/autoscaler_timeseries.json", f"{mock}/metrics/processed/")
        err = run(mock, *base, "--out", o("d2.svg"))
        assert "autoscaler_timeseries.json (keda-hpa-mock)" in err
        os.remove(f"{mock}/metrics/processed/autoscaler_timeseries.json")
        # no trigger: no green panel, and the CLI says why
        assert "not available" in run(mock, *base, "--out", o("b.svg"))
        assert "What KEDA read" not in " ".join(labels(o("b.svg")))
        # request-rate panel
        run(mock, *base, "--load", "req", "--out", o("e.svg"))
        assert "Offered requests/s" in " ".join(labels(o("e.svg")))
        # closed loop: plotted, but labelled as not the load
        err = run(mock, "--loop", "closed", "--out", o("y.svg"))
        assert "not the load" in err and "not the load" in " ".join(labels(o("y.svg")))
        # the load generator's pacing must be declared
        run(mock, "--out", o("z.svg"), ok=False)
        # two pools in one stack: refuse to sum them; the role picks the panel
        ts = f"{mock}/metrics/processed/replica_status_timeseries.json"
        data = json.load(open(ts))
        for s in data["snapshots"]:
            s["controllers"].append(dict(s["controllers"][0], name="mock-prefill", role="prefill"))
        json.dump(data, open(ts, "w"))
        run(mock, *base, "--out", o("g.svg"), ok=False)
        run(mock, *base, "--role", "decode", "--out", o("g.svg"))
        lab = " ".join(labels(o("g.svg")))
        assert "Offered output tokens/s" in lab and "Offered input tokens/s" not in lab
        run(mock, *base, "--role", "prefill", "--out", o("h.svg"))
        lab = " ".join(labels(o("h.svg")))
        assert "Offered input tokens/s" in lab and "Offered output tokens/s" not in lab
    print("selftest ok")


if __name__ == "__main__":
    main()
