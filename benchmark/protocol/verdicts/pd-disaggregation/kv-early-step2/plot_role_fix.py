#!/usr/bin/env python3
"""Render the role-fix re-run as an SVG line chart (no third-party deps).

Plots the ScaledObject's own trigger query (now selecting by pod name
instead of the absent `role` label) over the same load profile used in the
original P9 run, with vertical markers where the Deployments' replica count
actually changed. Unlike plot_p9.py, there is no separate "ground truth"
line here: the point of this run is that the trigger value itself is now a
real, non-empty reading that the autoscaler acts on.
"""
import csv
import pathlib

HERE = pathlib.Path(__file__).parent


def read_csv(name):
    with open(HERE / name) as f:
        return list(csv.DictReader(f))


trigger_rows = read_csv("p1-p9-trigger-role-fix.csv")
event_rows = read_csv("scale-events-role-fix.csv")

t = [float(r["t_seconds"]) for r in trigger_rows]
queue = [float(r["trigger_prefill_queue_pod_name_filtered"]) for r in trigger_rows]
events = [(float(r["t_seconds"]), r["event"]) for r in event_rows]

W, H = 760, 460
PAD_L, PAD_R, PAD_T, PAD_B = 70, 30, 80, 50
plot_w = W - PAD_L - PAD_R
plot_h = H - PAD_T - PAD_B

t_max = max(t + [e[0] for e in events]) * 1.03
y_max = max(queue) * 1.1 or 1.0


def x(v):
    return PAD_L + (v / t_max) * plot_w


def y(v):
    return PAD_T + plot_h - (v / y_max) * plot_h


def polyline(xs, ys, **attrs):
    pts = " ".join(f"{x(a):.1f},{y(b):.1f}" for a, b in zip(xs, ys))
    attr_str = " ".join(f'{k.replace("_","-")}="{v}"' for k, v in attrs.items())
    return f'<polyline points="{pts}" {attr_str} />'


y_ticks = [round(y_max * frac) for frac in (0, 0.25, 0.5, 0.75, 1.0)]
x_ticks = [round(t_max * frac) for frac in (0, 0.25, 0.5, 0.75, 1.0)]

svg = []
svg.append(
    f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {W} {H}" '
    f'font-family="Helvetica,Arial,sans-serif" font-size="12">'
)
svg.append(
    "<title>kv-early-role-fix: trigger responds to load and the pool scales out</title>"
)
svg.append(
    "<desc>Line chart over about 4.5 minutes. The ScaledObject's "
    "vllm-queue-size trigger, now selecting by pod name "
    "(pod=~\".*prefill.*\") instead of the absent role label, climbs from "
    "0 to over 1300 once the same load profile used in the P9 run starts. "
    "Vertical markers show when the prefill and decode Deployments' "
    "replica counts actually changed, confirming the autoscaler acted on "
    "this signal.</desc>"
)
svg.append(f'<rect x="0" y="0" width="{W}" height="{H}" fill="white" />')

svg.append(
    f'<line x1="{PAD_L}" y1="{PAD_T}" x2="{PAD_L}" y2="{PAD_T+plot_h}" stroke="black" stroke-width="1" />'
)
svg.append(
    f'<line x1="{PAD_L}" y1="{PAD_T+plot_h}" x2="{PAD_L+plot_w}" y2="{PAD_T+plot_h}" stroke="black" stroke-width="1" />'
)

for yt in y_ticks:
    yy = y(yt)
    svg.append(
        f'<line x1="{PAD_L}" y1="{yy:.1f}" x2="{PAD_L+plot_w}" y2="{yy:.1f}" '
        f'stroke="#ddd" stroke-width="1" />'
    )
    svg.append(f'<text x="{PAD_L-8}" y="{yy+4:.1f}" text-anchor="end" fill="black">{yt}</text>')

for xt in x_ticks:
    xx = x(xt)
    svg.append(f'<text x="{xx:.1f}" y="{PAD_T+plot_h+20}" text-anchor="middle" fill="black">{xt}s</text>')

# Load-start marker (first non-zero sample)
load_start = next((tt for tt, v in zip(t, queue) if v > 0), None)
if load_start is not None:
    lx = x(load_start)
    svg.append(
        f'<line x1="{lx:.1f}" y1="{PAD_T}" x2="{lx:.1f}" y2="{PAD_T+plot_h}" '
        f'stroke="#888" stroke-width="1" stroke-dasharray="4,3" />'
    )
    svg.append(f'<text x="{lx+4:.1f}" y="{PAD_T+12}" fill="#555">load starts</text>')

# Scale-event markers, stacked labels so simultaneous events don't overlap.
by_time = {}
for tt, label in events:
    by_time.setdefault(tt, []).append(label)
for i, (tt, labels) in enumerate(sorted(by_time.items())):
    ex = x(tt)
    svg.append(
        f'<line x1="{ex:.1f}" y1="{PAD_T}" x2="{ex:.1f}" y2="{PAD_T+plot_h}" '
        f'stroke="#000" stroke-width="1" stroke-dasharray="2,2" />'
    )
    for j, label in enumerate(labels):
        svg.append(
            f'<text x="{ex+4:.1f}" y="{PAD_T+28+j*14}" fill="black" font-size="11">{label}</text>'
        )

svg.append(polyline(t, queue, fill="none", stroke="black", stroke_width="2"))
# Mark the actual samples as points, since they are sparse and unevenly spaced.
for tt, qq in zip(t, queue):
    svg.append(f'<circle cx="{x(tt):.1f}" cy="{y(qq):.1f}" r="2.5" fill="black" />')

svg.append(
    f'<text x="{W/2}" y="22" text-anchor="middle" font-size="14" font-weight="bold">'
    f'kv-early-role-fix: trigger responds to load, pool scales out</text>'
)
LEGEND_Y = 50
svg.append(f'<line x1="{PAD_L}" y1="{LEGEND_Y}" x2="{PAD_L+30}" y2="{LEGEND_Y}" stroke="black" stroke-width="2" />')
svg.append(
    f'<text x="{PAD_L+36}" y="{LEGEND_Y+4}" fill="black">'
    f'vllm-queue-size trigger (pod=~".*prefill.*", max)</text>'
)

svg.append(f'<text x="{PAD_L+plot_w/2}" y="{H-8}" text-anchor="middle">time since first sample (s)</text>')
svg.append(
    f'<text x="16" y="{PAD_T+plot_h/2}" text-anchor="middle" '
    f'transform="rotate(-90 16 {PAD_T+plot_h/2})">waiting requests (trigger value)</text>'
)

svg.append("</svg>")

out_path = HERE / "role-fix-trigger-vs-replicas.svg"
out_path.write_text("\n".join(svg))
print(f"wrote {out_path}")
