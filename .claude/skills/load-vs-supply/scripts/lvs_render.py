"""Load-vs-supply figure: binning and the SVG drawing, in raw units only.

Standard library only. Stacked panels share one time axis; each panel has one y-scale in its
series' own unit, so no series is rescaled to fit another and nothing assumes a capacity:

    offered load      tokens/s (input, output) or requests/s clients sent, from client records
    autoscaler input  each trigger's value in its own unit, its ScaledObject threshold as a reference line
    replicas          Ready (solid) and requested (dashed)
    SLO attainment    share of requests sent in each bin that met the SLOs you give

Palette and classes: s1 blue (replicas), s2 orange (offered load), s3 green (trigger), s4 yellow
(SLO attainment), track, ref; light and dark steps in STYLE.
"""

import math

BIN = 30  # seconds per offered-load bin


def at(steps, t):
    """The last step (t_i, ...) with t_i <= t, else the first."""
    cur = steps[0]
    for s in steps:
        if s[0] <= t:
            cur = s
    return cur


def bins(reqs, t_end, key, bin_s=BIN):
    """[(bin centre, per second)] summed over requests SCHEDULED in the bin.
    key: "in" (input tokens), "out" (output tokens) or "req" (requests)."""
    out = []
    for i in range(int(math.ceil(t_end / bin_s))):
        lo = i * bin_s
        rs = [r for r in reqs if lo <= r["t"] < lo + bin_s]
        v = len(rs) if key == "req" else sum(r[key] for r in rs)
        out.append((lo + bin_s / 2, v / bin_s))
    return out


SLO_KEYS = {"ttft": "TTFT", "tpot": "TPOT", "e2e": "E2E latency"}


def attainment(reqs, t_end, slo, bin_s=BIN):
    """SLO attainment per bin: requests SCHEDULED in the bin that completed without error and met
    every SLO in slo ({"ttft": s, "tpot": s, "e2e": s}), over every request scheduled in the bin.
    An error or timeout is a miss. A completed request missing a needed latency cannot be judged
    and is left out of both counts. -> ([(bin centre, share or None)], unjudged count)."""
    out, unjudged = [], 0
    for i in range(int(math.ceil(t_end / bin_s))):
        lo = i * bin_s
        n = good = 0
        for r in reqs:
            if not lo <= r["t"] < lo + bin_s:
                continue
            if not r["ok"]:
                n += 1
                continue
            vals = [r.get(k) for k in slo]
            if any(v is None for v in vals):
                unjudged += 1
                continue
            n += 1
            good += all(v <= slo[k] for k, v in zip(slo, vals))
        out.append((lo + bin_s / 2, good / n if n else None))
    return out, unjudged


def clip(pts, t_end):
    """Step series cut at [0, t_end], carrying the last value to the edge."""
    pts = sorted(pts)
    inside = [(max(0, t), v) for t, v in pts if t <= t_end]
    before = [v for t, v in pts if t <= 0]
    if before:
        inside = [(0, before[-1])] + [(t, v) for t, v in inside if t > 0]
    return inside + [(t_end, inside[-1][1])] if inside else inside


def nice(vmax):
    """A round axis top and tick step for 0..vmax (about 4 ticks)."""
    if vmax <= 0:
        return 1, 1
    raw = vmax / 4
    mag = 10 ** math.floor(math.log10(raw))
    step = next(m * mag for m in (1, 2, 2.5, 5, 10) if m * mag >= raw)
    return step * math.ceil(vmax / step), step


def fmt(v):
    if abs(v) >= 1000:
        return f"{v / 1000:g}k"
    return f"{v:g}"


# ---------------------------------------------------------------------------
# SVG helpers.

STYLE = """<style>
  .bg { fill: #fcfcfb; }
  .ink { fill: #0b0b0b; } .ink2 { fill: #52514e; } .muted { fill: #898781; }
  .grid { stroke: #e1e0d9; stroke-width: 1; }
  .axis { stroke: #c3c2b7; stroke-width: 1; }
  .s1 { stroke: #2a78d6; } .f1 { fill: #2a78d6; }
  .s2 { stroke: #eb6834; } .f2 { fill: #eb6834; }
  .s3 { stroke: #1baf7a; } .f3 { fill: #1baf7a; }
  .s4 { stroke: #eda100; } .f4 { fill: #eda100; }
  .track { fill: #e1e0d9; }
  .ring { stroke: #fcfcfb; stroke-width: 2; }
  .ref { stroke: #898781; stroke-width: 1.5; stroke-dasharray: 5 4; }
  text { font-family: system-ui, -apple-system, "Segoe UI", Helvetica, Arial, sans-serif; }
  @media (prefers-color-scheme: dark) {
    .bg { fill: #1a1a19; }
    .ink { fill: #ffffff; } .ink2 { fill: #c3c2b7; }
    .grid { stroke: #2c2c2a; } .axis { stroke: #383835; }
    .s1 { stroke: #3987e5; } .f1 { fill: #3987e5; }
    .s2 { stroke: #d95926; } .f2 { fill: #d95926; }
    .s3 { stroke: #199e70; } .f3 { fill: #199e70; }
    .s4 { stroke: #c98500; } .f4 { fill: #c98500; }
    .track { fill: #2c2c2a; }
    .ring { stroke: #1a1a19; }
  }
</style>"""


def esc(s):
    return s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")


def svg(width, height, body, title):
    return (f'<svg xmlns="http://www.w3.org/2000/svg" width="{width}" '
            f'height="{height}" viewBox="0 0 {width} {height}" role="img" '
            f'aria-label="{esc(title)}">\n<title>{esc(title)}</title>\n{STYLE}\n'
            f'<rect class="bg" width="{width}" height="{height}" rx="8"/>\n'
            + "\n".join(body) + "\n</svg>\n")


def text(x, y, s, cls="ink", size=13, anchor="start", weight="normal"):
    return (f'<text x="{x:.1f}" y="{y:.1f}" class="{cls}" font-size="{size}" '
            f'text-anchor="{anchor}" font-weight="{weight}">{esc(s)}</text>')


def line(x1, y1, x2, y2, cls):
    return f'<line x1="{x1:.1f}" y1="{y1:.1f}" x2="{x2:.1f}" y2="{y2:.1f}" class="{cls}"/>'


def path(points, cls, width=2, dash=None):
    d = " ".join(("M" if i == 0 else "L") + f"{x:.1f},{y:.1f}"
                 for i, (x, y) in enumerate(points))
    da = f' stroke-dasharray="{dash}"' if dash else ""
    return (f'<path d="{d}" class="{cls}" fill="none" stroke-width="{width}"{da} '
            'stroke-linejoin="round" stroke-linecap="round"/>')


class Plot:
    def __init__(self, left, top, width, height, xmin, xmax, ymin, ymax):
        self.l, self.t, self.w, self.h = left, top, width, height
        self.xmin, self.xmax, self.ymin, self.ymax = xmin, xmax, ymin, ymax

    def x(self, v):
        return self.l + (v - self.xmin) / (self.xmax - self.xmin) * self.w

    def y(self, v):
        v = min(max(v, self.ymin), self.ymax)
        return self.t + self.h - (v - self.ymin) / (self.ymax - self.ymin) * self.h

    def axes(self, xticks, yticks, xlabel, ylabel):
        out = []
        for v in yticks:
            out.append(line(self.l, self.y(v), self.l + self.w, self.y(v), "grid"))
            out.append(text(self.l - 8, self.y(v) + 4, f"{v:g}", "muted", 12, "end"))
        for v in xticks:
            out.append(text(self.x(v), self.t + self.h + 18, f"{v // 60:g}", "muted", 12, "middle"))
        out.append(line(self.l, self.t + self.h, self.l + self.w, self.t + self.h, "axis"))
        out.append(text(self.l + self.w / 2, self.t + self.h + 40, xlabel, "ink2", 13, "middle"))
        out.append(text(self.l, self.t - 12, ylabel, "ink2", 13))
        return out


def steps(p, pts):
    out = [(p.x(pts[0][0]), p.y(pts[0][1]))]
    for (ta, a), (tb, b) in zip(pts, pts[1:]):
        out.append((p.x(tb), p.y(a)))
        out.append((p.x(tb), p.y(b)))
    return out


# ---------------------------------------------------------------------------
# The figure.

PANEL_H = 120     # plot height per panel
PANEL_GAP = 62    # room for the next panel's label and legend
LEFT, WIDTH = 70, 620


def panels_svg(title, subtitle, t_end, panels, phases=()):
    """Stacked panels on one time axis. panels: list of dicts

        label    y-axis label (unit and source)
        series   [{"pts": [(t, v)], "cls": "s2", "width": 1.75, "dash": None,
                   "step": False, "name": legend text}]
        refs     [(value, text)] dashed horizontal reference lines (e.g. a threshold)
        integer  True for counts (replicas): integer ticks

    Each panel gets its own y-scale from its own data and references.
    """
    top = 84
    body = [text(24, 28, title, "ink", 14, weight="600"), text(24, 46, subtitle, "muted", 12)]
    span = 300 if t_end <= 1800 else 600
    xt = list(range(0, int(t_end) + 1, span))
    for k, pn in enumerate(panels):
        vals = [v for s in pn["series"] for t, v in s["pts"] if 0 <= t <= t_end and v is not None]
        vals += [v for v, _ in pn.get("refs", ())]
        vmax = max(vals) * 1.12 if vals else 1
        if pn.get("ymax") is not None:
            ymax, step = pn["ymax"], pn.get("step", pn["ymax"] / 4)
        elif pn.get("integer"):
            ymax = max(2, math.ceil(vmax))
            step = 1 if ymax <= 8 else 2
        else:
            ymax, step = nice(vmax)
        p = Plot(LEFT, top, WIDTH, PANEL_H, 0, t_end, 0, ymax)
        body.append(text(LEFT, top - 12, pn["label"], "ink2", 13))
        # legend, right-aligned on the label line; a single-series panel's label names its series
        lx = LEFT + WIDTH
        for s in reversed(pn["series"] if len(pn["series"]) > 1 else []):
            w = 7 * len(s["name"]) + 30
            lx -= w
            da = f' stroke-dasharray="{s["dash"]}"' if s.get("dash") else ""
            body.append(f'<line x1="{lx:.1f}" y1="{top - 16}" x2="{lx + 18:.1f}" y2="{top - 16}" '
                        f'class="{s["cls"]}" stroke-width="3"{da}/>')
            body.append(text(lx + 24, top - 12, s["name"], "ink", 12))
        v = 0
        while v <= ymax + 1e-9:
            body.append(line(p.l, p.y(v), p.l + p.w, p.y(v), "grid"))
            body.append(text(p.l - 8, p.y(v) + 4, f"{v * 100:g}%" if pn.get("percent") else fmt(v), "muted", 12, "end"))
            v += step
        body.append(line(p.l, p.t + p.h, p.l + p.w, p.t + p.h, "axis"))
        for t, lab in phases:
            body.append(f'<line x1="{p.x(t):.1f}" y1="{p.t}" x2="{p.x(t):.1f}" y2="{p.t + p.h}" class="ref"/>')
            if lab and k == 0:
                body.append(text(p.x(t) + 4, p.t + 12, lab, "muted", 11))
        for rv, lab in pn.get("refs", ()):
            body.append(line(p.l, p.y(rv), p.l + p.w, p.y(rv), "ref"))
            # near the top the label would sit in the band the data most likely runs through: put it below
            ly = p.y(rv) + 14 if p.y(rv) - p.t < 18 else p.y(rv) - 6
            body.append(text(p.l + p.w - 4, ly, lab, "muted", 11, "end"))
        for s in pn["series"]:
            if s.get("step"):
                pts = clip(s["pts"], t_end)
                if pts:
                    body.append(path(steps(p, pts), s["cls"], s.get("width", 2), s.get("dash")))
                continue
            # a None value (an empty bin) breaks the line rather than bridging it
            seg = []
            for t, v in [(t, v) for t, v in s["pts"] if 0 <= t <= t_end] + [(None, None)]:
                if v is None:
                    if len(seg) > 1:
                        body.append(path(seg, s["cls"], s.get("width", 2), s.get("dash")))
                    elif seg:
                        body.append(f'<circle cx="{seg[0][0]:.1f}" cy="{seg[0][1]:.1f}" r="2" class="{s["cls"].replace("s", "f")}"/>')
                    seg = []
                else:
                    seg.append((p.x(t), p.y(v)))
        last = k == len(panels) - 1
        for t in xt:
            if last:
                body.append(text(p.x(t), p.t + p.h + 18, f"{t // 60:g}", "muted", 12, "middle"))
        if last:
            body.append(text(p.l + p.w / 2, p.t + p.h + 40, "Time since load start (minutes)", "ink2", 13, "middle"))
        top += PANEL_H + PANEL_GAP
    height = int(top - PANEL_GAP + 56)
    return svg(720, height, body, title)
