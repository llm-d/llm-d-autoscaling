#!/usr/bin/env python3
"""Render p1-p9-queue.csv as an SVG line chart (no third-party deps).

Plots the ground-truth prefill queue depth (what the real vLLM engine sees)
against the KEDA trigger's own query result, filtered by the `role` label the
scenario's ScaledObject actually uses. The trigger query returns an empty
result at every sample, which KEDA/`ignoreNullValues` reads as 0 -- so its
line is flat at 0 for the whole run regardless of the real backlog.
"""
import csv
import pathlib

HERE = pathlib.Path(__file__).parent
rows = []
with open(HERE / "p1-p9-queue.csv") as f:
    for r in csv.DictReader(f):
        rows.append(r)

t = [float(r["t_seconds"]) for r in rows]
truth = [float(r["ground_truth_prefill_queue"]) for r in rows]
# Empty Prometheus result -> KEDA/ignoreNullValues reads 0 (trap T7).
trigger = [0.0 for _ in rows]

W, H = 760, 460
# Extra top padding reserves two clear rows above the plot: the title and,
# below it, the legend -- the earlier version put both in the same 40px band
# and they overlapped when rendered.
PAD_L, PAD_R, PAD_T, PAD_B = 70, 30, 80, 50
plot_w = W - PAD_L - PAD_R
plot_h = H - PAD_T - PAD_B

t_max = max(t) or 1.0
y_max = max(truth) * 1.08 or 1.0


def x(v):
    return PAD_L + (v / t_max) * plot_w


def y(v):
    return PAD_T + plot_h - (v / y_max) * plot_h


def polyline(xs, ys, **attrs):
    pts = " ".join(f"{x(a):.1f},{y(b):.1f}" for a, b in zip(xs, ys))
    attr_str = " ".join(f'{k.replace("_","-")}="{v}"' for k, v in attrs.items())
    return f'<polyline points="{pts}" {attr_str} />'


# Axis ticks
y_ticks = [round(y_max * frac) for frac in (0, 0.25, 0.5, 0.75, 1.0)]
x_ticks = [round(t_max * frac) for frac in (0, 0.25, 0.5, 0.75, 1.0)]

svg = []
svg.append(
    f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {W} {H}" '
    f'font-family="Helvetica,Arial,sans-serif" font-size="12">'
)
svg.append(
    "<title>P9: trigger query vs. ground-truth prefill queue, kv-early</title>"
)
svg.append(
    "<desc>Line chart over about 4.5 minutes. The ground-truth prefill queue "
    "(sum(vllm:num_requests_waiting) read directly from the prefill pod) "
    "rises from 0 to over 3000 once load starts around 140 seconds. The "
    "ScaledObject's own trigger query, which additionally filters on "
    "role=\"prefill\", returns an empty Prometheus result at every sample and "
    "so is plotted flat at 0 throughout, confirming step 1's P9 prediction "
    "that the role label does not exist on these series.</desc>"
)
svg.append(f'<rect x="0" y="0" width="{W}" height="{H}" fill="white" />')

# Axes
svg.append(
    f'<line x1="{PAD_L}" y1="{PAD_T}" x2="{PAD_L}" y2="{PAD_T+plot_h}" stroke="black" stroke-width="1" />'
)
svg.append(
    f'<line x1="{PAD_L}" y1="{PAD_T+plot_h}" x2="{PAD_L+plot_w}" y2="{PAD_T+plot_h}" stroke="black" stroke-width="1" />'
)

# Gridlines + y labels
for yt in y_ticks:
    yy = y(yt)
    svg.append(
        f'<line x1="{PAD_L}" y1="{yy:.1f}" x2="{PAD_L+plot_w}" y2="{yy:.1f}" '
        f'stroke="#ddd" stroke-width="1" />'
    )
    svg.append(
        f'<text x="{PAD_L-8}" y="{yy+4:.1f}" text-anchor="end" fill="black">{yt}</text>'
    )

# x labels
for xt in x_ticks:
    xx = x(xt)
    svg.append(
        f'<text x="{xx:.1f}" y="{PAD_T+plot_h+20}" text-anchor="middle" fill="black">{xt}s</text>'
    )

# Mark when load starts (first non-zero sample, ~138s)
load_start = next((tt for tt, v in zip(t, truth) if v > 0), None)
if load_start is not None:
    lx = x(load_start)
    svg.append(
        f'<line x1="{lx:.1f}" y1="{PAD_T}" x2="{lx:.1f}" y2="{PAD_T+plot_h}" '
        f'stroke="#888" stroke-width="1" stroke-dasharray="4,3" />'
    )
    svg.append(
        f'<text x="{lx+4:.1f}" y="{PAD_T+12}" fill="#555">load starts</text>'
    )

# Data lines: solid black for ground truth, dashed gray for the trigger
# (distinguishable without color, per the grayscale-safe rule).
svg.append(polyline(t, truth, fill="none", stroke="black", stroke_width="2"))
svg.append(
    polyline(t, trigger, fill="none", stroke="#555", stroke_width="2", stroke_dasharray="6,4")
)

# Title sits in its own row near the top of the canvas.
svg.append(
    f'<text x="{W/2}" y="22" text-anchor="middle" font-size="14" font-weight="bold">'
    f'pd-disaggregation/kv-early: trigger reads 0 while the real queue grows (P9)</text>'
)

# Legend sits in its own row below the title, well clear of it, and both
# entries are placed one above the other (rather than side by side) so
# neither the legend nor the title ever collides with the plot's own
# "load starts" annotation near the top-left of the axes.
LEGEND_Y1 = 46
LEGEND_Y2 = 64
svg.append(f'<line x1="{PAD_L}" y1="{LEGEND_Y1}" x2="{PAD_L+30}" y2="{LEGEND_Y1}" stroke="black" stroke-width="2" />')
svg.append(f'<text x="{PAD_L+36}" y="{LEGEND_Y1+4}" fill="black">ground-truth prefill queue (no role filter)</text>')
svg.append(
    f'<line x1="{PAD_L}" y1="{LEGEND_Y2}" x2="{PAD_L+30}" y2="{LEGEND_Y2}" '
    f'stroke="#555" stroke-width="2" stroke-dasharray="6,4" />'
)
svg.append(f'<text x="{PAD_L+36}" y="{LEGEND_Y2+4}" fill="black">KEDA trigger value (role="prefill" filter)</text>')

# Axis titles
svg.append(f'<text x="{PAD_L+plot_w/2}" y="{H-8}" text-anchor="middle">time since first sample (s)</text>')
svg.append(
    f'<text x="16" y="{PAD_T+plot_h/2}" text-anchor="middle" '
    f'transform="rotate(-90 16 {PAD_T+plot_h/2})">waiting requests</text>'
)

svg.append("</svg>")

out_path = HERE / "p9-trigger-vs-queue.svg"
out_path.write_text("\n".join(svg))
print(f"wrote {out_path}")
