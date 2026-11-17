#!/usr/bin/env python3
"""Render the kv-early step 2 plots from samples.csv and replicas.csv.

No third-party dependencies. Run from any directory:

    python3 plot.py

Writes p9-prefill-queue.svg, p9-prefill-kv.svg and p7-backlog-vs-replicas.svg
next to this script.

An empty Prometheus result (a blank trigger cell in samples.csv) is plotted
as 0, because KEDA's Prometheus scaler turns an empty result into 0 when
ignoreNullValues is left at its default (trap T7).
"""
import csv
import pathlib

HERE = pathlib.Path(__file__).parent
W = 760
PAD_L, PAD_R = 70, 30


def read(name):
    with open(HERE / name) as f:
        return list(csv.DictReader(f))


def num(s):
    return float(s) if s != "" else 0.0


def esc(s):
    return s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")


class Panel:
    """One plotting area with its own y scale, sharing the x range."""

    def __init__(self, top, height, t_max, y_max, ylabel, y_ticks):
        self.top, self.height, self.t_max = top, height, t_max
        self.y_max, self.ylabel, self.y_ticks = y_max, ylabel, y_ticks
        self.w = W - PAD_L - PAD_R

    def x(self, t):
        return PAD_L + t / self.t_max * self.w

    def y(self, v):
        return self.top + self.height - v / self.y_max * self.height

    def frame(self, out, x_labels):
        out.append(
            f'<line x1="{PAD_L}" y1="{self.top}" x2="{PAD_L}" y2="{self.top + self.height}" stroke="black" />'
        )
        out.append(
            f'<line x1="{PAD_L}" y1="{self.top + self.height}" x2="{PAD_L + self.w}" y2="{self.top + self.height}" stroke="black" />'
        )
        for yt in self.y_ticks:
            yy = self.y(yt)
            out.append(
                f'<line x1="{PAD_L}" y1="{yy:.1f}" x2="{PAD_L + self.w}" y2="{yy:.1f}" stroke="#ddd" />'
            )
            out.append(f'<text x="{PAD_L - 8}" y="{yy + 4:.1f}" text-anchor="end">{yt:g}</text>')
        if x_labels:
            for frac in (0, 0.25, 0.5, 0.75, 1.0):
                tt = round(self.t_max * frac)
                out.append(
                    f'<text x="{self.x(tt):.1f}" y="{self.top + self.height + 18}" text-anchor="middle">{tt}s</text>'
                )
        cy = self.top + self.height / 2
        out.append(
            f'<text x="16" y="{cy:.1f}" text-anchor="middle" transform="rotate(-90 16 {cy:.1f})">{esc(self.ylabel)}</text>'
        )

    def line(self, out, ts, vs, stroke, dash=None, points=False):
        pts = " ".join(f"{self.x(a):.1f},{self.y(b):.1f}" for a, b in zip(ts, vs))
        d = f' stroke-dasharray="{dash}"' if dash else ""
        out.append(f'<polyline points="{pts}" fill="none" stroke="{stroke}" stroke-width="2"{d} />')
        if points:
            for a, b in zip(ts, vs):
                out.append(f'<circle cx="{self.x(a):.1f}" cy="{self.y(b):.1f}" r="2.5" fill="{stroke}" />')

    def hline(self, out, v, label, dash="2,3"):
        yy = self.y(v)
        out.append(
            f'<line x1="{PAD_L}" y1="{yy:.1f}" x2="{PAD_L + self.w}" y2="{yy:.1f}" stroke="#000" stroke-width="1" stroke-dasharray="{dash}" />'
        )
        out.append(
            f'<text x="{PAD_L + self.w - 4}" y="{yy - 5:.1f}" text-anchor="end" font-size="11">{esc(label)}</text>'
        )

    def vline(self, out, t, label):
        xx = self.x(t)
        out.append(
            f'<line x1="{xx:.1f}" y1="{self.top}" x2="{xx:.1f}" y2="{self.top + self.height}" stroke="#888" stroke-dasharray="4,3" />'
        )
        out.append(
            f'<text x="{xx + 4:.1f}" y="{self.top + 13}" fill="#555" font-size="11">{esc(label)}</text>'
        )


def legend(out, entries, y0):
    """Stacked legend, one entry per row. Entries: (label, stroke, dash)."""
    for i, (label, stroke, dash) in enumerate(entries):
        y = y0 + i * 18
        d = f' stroke-dasharray="{dash}"' if dash else ""
        out.append(f'<line x1="{PAD_L}" y1="{y}" x2="{PAD_L + 30}" y2="{y}" stroke="{stroke}" stroke-width="2"{d} />')
        out.append(f'<text x="{PAD_L + 36}" y="{y + 4}">{esc(label)}</text>')


def svg(width_h, title, desc, body):
    h = width_h
    head = (
        f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {W} {h}" '
        f'font-family="Helvetica,Arial,sans-serif" font-size="12">\n'
        f"<title>{esc(title)}</title>\n<desc>{esc(desc)}</desc>\n"
        f'<rect x="0" y="0" width="{W}" height="{h}" fill="white" />\n'
    )
    return head + "\n".join(body) + "\n</svg>\n"


def title_row(out, text):
    out.append(f'<text x="{W / 2}" y="22" text-anchor="middle" font-size="14" font-weight="bold">{esc(text)}</text>')


samples = read("samples.csv")
replicas = read("replicas.csv")
t = [float(r["t_seconds"]) for r in samples]
t_max = max(t)
load_start = next(tt for tt, r in zip(t, samples) if float(r["probe_prefill_queue"]) > 0)


def trigger_vs_probe(fname, title, desc, trig_col, probe_col, ylabel, threshold, thr_label, y_max, y_ticks, probe_label, trig_label):
    probe = [num(r[probe_col]) for r in samples]
    trig = [num(r[trig_col]) for r in samples]
    out = []
    title_row(out, title)
    legend(out, [(probe_label, "black", None), (trig_label, "#555", "6,4")], 46)
    H = 84 + 280 + 60
    p = Panel(84, 280, t_max, y_max, ylabel, y_ticks)
    p.frame(out, True)
    p.vline(out, load_start, "first non-zero sample")
    p.hline(out, threshold, thr_label)
    p.line(out, t, probe, "black", points=True)
    p.line(out, t, trig, "#555", dash="6,4")
    out.append(f'<text x="{PAD_L + p.w / 2}" y="{H - 10}" text-anchor="middle">time since first sample (s)</text>')
    (HERE / fname).write_text(svg(H, title, desc, out))


trigger_vs_probe(
    "p9-prefill-queue.svg",
    "P9: the prefill queue trigger reads 0 while the real queue grows",
    "Line chart over about 7 minutes. The prefill queue read by pod name rises "
    "from 0 to about 2970 waiting requests after load starts near 230 seconds. "
    "The ScaledObject's own query, which filters on role=\"prefill\", returns an "
    "empty result at every sample and is plotted at 0. The threshold is 20.",
    "trigger_prefill_queue",
    "probe_prefill_queue",
    "waiting requests",
    20,
    "threshold 20",
    3200,
    [0, 800, 1600, 2400, 3200],
    "prefill queue, selected by pod name (max)",
    'vllm-queue-size trigger as rendered (role="prefill"); empty, read as 0',
)

trigger_vs_probe(
    "p9-prefill-kv.svg",
    "P9: the prefill KV trigger reads 0 while KV usage passes its threshold",
    "Line chart over about 7 minutes. Prefill KV-cache usage read by pod name "
    "rises from 0.002 to 0.92 after load starts near 230 seconds, above the "
    "0.80 threshold. The ScaledObject's own query, which filters on "
    "role=\"prefill\", returns an empty result at every sample and is plotted at 0.",
    "trigger_prefill_kv",
    "probe_prefill_kv",
    "KV-cache usage (fraction)",
    0.80,
    "threshold 0.80",
    1.0,
    [0, 0.25, 0.5, 0.75, 1.0],
    "prefill KV usage, selected by pod name (max)",
    'vllm-kv-utilization trigger as rendered (role="prefill"); empty, read as 0',
)

# P7 consequence: backlog (top) against replica count (bottom).
in_window = [r for r in replicas if float(r["t_seconds"]) <= t_max]
rt = [float(r["t_seconds"]) for r in in_window]
pre = [float(r["prefill_replicas"]) for r in in_window]
queue = [num(r["probe_prefill_queue"]) for r in samples]
out = []
title_row(out, "P7: no error, no fallback, no scale-out while the backlog grows")
legend(
    out,
    [
        ("prefill queue, selected by pod name (max)", "black", None),
        ("prefill Deployment replicas (kubectl get deploy)", "#555", "8,3"),
        ("maxReplicaCount = 4", "#000", "2,3"),
    ],
    46,
)
H = 112 + 170 + 20 + 120 + 60
top = Panel(112, 170, t_max, 3200, "waiting requests", [0, 800, 1600, 2400, 3200])
bot = Panel(112 + 170 + 20, 120, t_max, 5, "replicas", [0, 1, 2, 3, 4, 5])
top.frame(out, False)
bot.frame(out, True)
top.vline(out, load_start, "first non-zero sample")
bot.vline(out, load_start, "")
top.line(out, t, queue, "black", points=True)
bot.hline(out, 4, "maxReplicaCount 4")
bot.line(out, rt, pre, "#555", dash="8,3", points=True)
out.append(f'<text x="{PAD_L + top.w / 2}" y="{H - 10}" text-anchor="middle">time since first sample (s)</text>')
(HERE / "p7-backlog-vs-replicas.svg").write_text(
    svg(
        H,
        "P7: no error, no fallback, no scale-out while the backlog grows",
        "Two stacked panels over about 7 minutes sharing the time axis. Top: the "
        "prefill queue read by pod name rises from 0 to about 2970 waiting "
        "requests. Bottom: the prefill Deployment stays at 1 replica throughout, "
        "far below maxReplicaCount 4.",
        out,
    )
)
print("wrote 3 svg files")
