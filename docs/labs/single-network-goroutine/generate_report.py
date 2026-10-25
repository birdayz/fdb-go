#!/usr/bin/env python3
"""Generate the self-contained HTML report suite in docs/labs/single-network-goroutine/report/
and copy it to /var/tmp/netg-report/.

Reads raw benchmark JSONL and wirestats JSON files from:
  - rawhz6/   (Round 6 on netg-r6: 3-process cluster, queue ladder, linger sweep, multi-rate capped, syscalls, pprof)
  - rawhz6b/  (Round 6b on netg-r6b: 5-process split cluster, per-role server CPU, per-role wirestats)
  - rawhz4/   (Round 4 on box 1: L, A, D, D-NG, D 4Ps + wirestats + cross-box check)
  - rawhz5/   (Round 5 on box 2: L, L4, A, D, D-M, D-M 4Ps + wirestats + 1-worker golden + syscalls)
"""

import collections
import glob
import html
import json
import os
import shutil
import statistics
import subprocess

HERE = os.path.dirname(os.path.abspath(__file__))
REPORT_DIR = os.path.join(HERE, "report")
TMP_REPORT_DIR = "/var/tmp/netg-report"

ARM_LABELS = {
    "L": "L (libfdb_c, 1 net thread)",
    "L4": "L4 (libfdb_c, 4 threads / 4 DBs)",
    "A": "A (today: direct write + ack + 10/20µs)",
    "A0": "A0 (today: direct write + ack, 0µs)",
    "M": "D-M (sender writes under lock, 0µs)",
    "M4P": "D-M, 4 Ps (GOMAXPROCS=4)",
    "DNG": "D-NG (writer goroutine, mutex+slice, 0µs)",
    "DNGy": "D-NG + yield (runtime.Gosched post-write)",
    "DNGchan": "D-NG-chan (buffered channel writer, 0µs)",
    "DNGring": "D-NG-ring v2 (lock-free MPSC ring + writev)",
    "DNGa20": "D-NG-adap20 (adaptive linger 20µs)",
    "DNGa100": "D-NG-adap100 (adaptive linger 100µs)",
    "D5": "D5 (fixed 5µs linger)",
    "D": "D (C++ 10–20µs coalesce delay)",
    "D4P": "D, 4 Ps (GOMAXPROCS=4)",
    "D50": "D50 (fixed 50µs linger)",
    "D100": "D100 (fixed 100µs linger)",
    "D250": "D250 (fixed 250µs linger)",
}

ARM_SHORT = {
    "L": "L (libfdb_c)",
    "L4": "L4 (4 thr)",
    "A": "A (today)",
    "A0": "A0 (no delay)",
    "M": "D-M (sender)",
    "M4P": "D-M (4 Ps)",
    "DNG": "D-NG (0µs)",
    "DNGy": "D-NG+yield",
    "DNGchan": "D-NG-chan",
    "DNGring": "D-NG-ring v2",
    "DNGa20": "D-NG-adap20",
    "DNGa100": "D-NG-adap100",
    "D5": "D5 (5µs)",
    "D": "D (10–20µs)",
    "D4P": "D (4 Ps)",
    "D50": "D50 (50µs)",
    "D100": "D100 (100µs)",
    "D250": "D250 (250µs)",
}

ARM_COLORS = {
    "L": "#475569",
    "L4": "#94a3b8",
    "A": "#d97706",
    "A0": "#f59e0b",
    "M": "#e11d48",
    "M4P": "#fb7185",
    "DNG": "#059669",
    "DNGy": "#10b981",
    "DNGchan": "#0d9488",
    "DNGring": "#0891b2",
    "DNGa20": "#7c3aed",
    "DNGa100": "#a855f7",
    "D5": "#3b82f6",
    "D": "#2563eb",
    "D4P": "#60a5fa",
    "D50": "#1d4ed8",
    "D100": "#1e40af",
    "D250": "#1e3a8a",
}

CSS = """
:root {
  --bg: #f8fafc;
  --panel: #ffffff;
  --text: #0f172a;
  --muted: #475569;
  --border: #e2e8f0;
  --accent: #1d4ed8;
  --good: #047857;
  --good-bg: #ecfdf5;
  --warn: #b45309;
  --warn-bg: #fffbeb;
  --bad: #be123c;
  --bad-bg: #fff1f2;
  --code-bg: #f1f5f9;
}
* { box-sizing: border-box; }
body {
  margin: 0;
  font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
  font-size: 14px;
  line-height: 1.55;
  color: var(--text);
  background: var(--bg);
}
header {
  background: #0f172a;
  color: #f8fafc;
  padding: 18px 28px;
  border-bottom: 1px solid #1e293b;
}
header .title-row {
  max-width: 1440px;
  margin: 0 auto;
  display: flex;
  flex-wrap: wrap;
  justify-content: space-between;
  align-items: baseline;
  gap: 12px;
}
header h1 {
  margin: 0;
  font-size: 20px;
  font-weight: 600;
  letter-spacing: -0.01em;
}
header .subtitle {
  color: #94a3b8;
  font-size: 13px;
}
nav {
  background: #1e293b;
  padding: 0 28px;
  border-bottom: 1px solid #334155;
}
nav .nav-inner {
  max-width: 1440px;
  margin: 0 auto;
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
}
nav a {
  color: #cbd5e1;
  text-decoration: none;
  padding: 10px 14px;
  font-size: 13px;
  font-weight: 500;
  border-bottom: 2px solid transparent;
}
nav a:hover {
  color: #ffffff;
  background: rgba(255,255,255,0.05);
}
nav a.active {
  color: #ffffff;
  border-bottom-color: #38bdf8;
  background: rgba(56,189,248,0.1);
}
main {
  max-width: 1440px;
  margin: 0 auto;
  padding: 24px 28px 64px;
}
h2 {
  font-size: 18px;
  font-weight: 600;
  margin: 28px 0 10px;
  padding-bottom: 6px;
  border-bottom: 1px solid var(--border);
  color: #0f172a;
}
h3 {
  font-size: 15px;
  font-weight: 600;
  margin: 20px 0 8px;
  color: #1e293b;
}
p, li {
  color: #1e293b;
}
ul, ol {
  margin: 8px 0 14px;
  padding-left: 22px;
}
li {
  margin-bottom: 4px;
}
code, pre {
  font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", monospace;
  font-size: 12.5px;
}
code {
  background: var(--code-bg);
  padding: 1px 5px;
  border-radius: 4px;
}
pre {
  background: #0f172a;
  color: #e2e8f0;
  padding: 14px 16px;
  border-radius: 6px;
  overflow-x: auto;
  line-height: 1.45;
}
.grid-2 {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(560px, 1fr));
  gap: 18px;
  margin: 14px 0;
}
.grid-3 {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(360px, 1fr));
  gap: 16px;
  margin: 14px 0;
}
.card {
  background: var(--panel);
  border: 1px solid var(--border);
  border-radius: 8px;
  padding: 16px 18px;
  box-shadow: 0 1px 2px rgba(15, 23, 42, 0.03);
}
.card h3 {
  margin-top: 0;
}
.callout {
  border-left: 4px solid var(--accent);
  background: #eff6ff;
  padding: 12px 16px;
  border-radius: 0 6px 6px 0;
  margin: 14px 0;
}
.callout.good {
  border-left-color: var(--good);
  background: var(--good-bg);
}
.callout.warn {
  border-left-color: var(--warn);
  background: var(--warn-bg);
}
.callout.bad {
  border-left-color: var(--bad);
  background: var(--bad-bg);
}
.table-wrap {
  overflow-x: auto;
  margin: 12px 0 20px;
  background: var(--panel);
  border: 1px solid var(--border);
  border-radius: 8px;
}
table {
  width: 100%;
  border-collapse: collapse;
  font-size: 12.5px;
  font-variant-numeric: tabular-nums;
}
th, td {
  padding: 7px 10px;
  border-bottom: 1px solid var(--border);
  text-align: right;
  white-space: nowrap;
}
th:first-child, td:first-child {
  text-align: left;
  font-weight: 500;
}
td.text-left, th.text-left {
  text-align: left;
  white-space: normal;
}
thead th {
  background: #f1f5f9;
  color: #334155;
  font-weight: 600;
  border-bottom: 2px solid #cbd5e1;
}
tbody tr:hover {
  background: #f8fafc;
}
tbody tr.highlight {
  background: #f0fdf4;
}
tbody tr.highlight-warn {
  background: #fff1f2;
}
.spread {
  color: #64748b;
  font-size: 11px;
  display: block;
}
.badge {
  display: inline-block;
  padding: 1px 6px;
  border-radius: 4px;
  font-size: 11px;
  font-weight: 600;
}
.badge.pass { background: var(--good-bg); color: var(--good); }
.badge.fail { background: var(--bad-bg); color: var(--bad); }
.badge.warn { background: var(--warn-bg); color: var(--warn); }
.badge.info { background: #e0f2fe; color: #0369a1; }
.svg-wrap {
  background: var(--panel);
  border: 1px solid var(--border);
  border-radius: 8px;
  padding: 12px;
  margin: 12px 0;
  overflow-x: auto;
}
footer {
  margin-top: 48px;
  padding-top: 16px;
  border-top: 1px solid var(--border);
  color: var(--muted);
  font-size: 12px;
  display: flex;
  justify-content: space-between;
}
"""

NAV_PAGES = [
    ("index.html", "1. Executive Summary"),
    ("linger.html", "2. Linger Sweep & Per-Role Cost"),
    ("queues.html", "3. Writer Queues, Ring & D-M"),
    ("delay-vs-ack.html", "4. Coalesce Delay vs Write-Ack"),
    ("libfdbc.html", "5. libfdb_c (L vs L4) vs Go"),
    ("methodology.html", "6. Methodology & Verification"),
]


def load_jsonl(relpath):
    p = os.path.join(HERE, relpath)
    if not os.path.exists(p):
        return []
    rows = []
    with open(p) as f:
        for line in f:
            line = line.strip()
            if line:
                r = json.loads(line)
                if "server_cycles_per_tx" in r:
                    r["server_kcycles"] = r["server_cycles_per_tx"] / 1e3
                rows.append(r)
    return rows


def fmt_med_spread(vals, decimals=0, suffix="", scale=1.0):
    if not vals:
        return "-"
    sv = [v * scale for v in vals]
    m = statistics.median(sv)
    lo, hi = min(sv), max(sv)
    return (
        f"<strong>{m:,.{decimals}f}{suffix}</strong>"
        f"<span class='spread'>[{lo:,.{decimals}f}, {hi:,.{decimals}f}]</span>"
    )


def fmt_med_inline(vals, decimals=0, scale=1.0):
    if not vals:
        return "-"
    sv = [v * scale for v in vals]
    return f"{statistics.median(sv):,.{decimals}f} [{min(sv):,.{decimals}f}, {max(sv):,.{decimals}f}]"


def med_val(vals, scale=1.0):
    if not vals:
        return None
    return statistics.median([v * scale for v in vals])


def page_shell(active_file, title, body_html):
    nav_links = []
    for fname, label in NAV_PAGES:
        cls = " class='active'" if fname == active_file else ""
        nav_links.append(f"<a href='{fname}'{cls}>{html.escape(label)}</a>")
    return f"""<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{html.escape(title)} — fdbgo Transport &amp; Coalescing Lab</title>
<style>{CSS}</style>
</head>
<body>
<header>
  <div class="title-row">
    <h1>FoundationDB Pure-Go Transport Lab: Writer Queues, Coalescing Linger &amp; libfdb_c Parity</h1>
    <div class="subtitle">FDB 7.3.77 &middot; PR #991 &middot; Dedicated Hetzner ccx43 (AMD EPYC-Milan, 16 vCPUs)</div>
  </div>
</header>
<nav>
  <div class="nav-inner">
    {"".join(nav_links)}
  </div>
</nav>
<main>
{body_html}
<footer>
  <div>docs/labs/single-network-goroutine/report/{active_file}</div>
  <div>All benchmark tables report 3-round medians and [min, max] ranges unless noted.</div>
</footer>
</main>
</body>
</html>
"""


def build_cell_table(rows, workload, workers, arms_order, rate=None, wire_map=None):
    cell_rows = [
        r for r in rows
        if r["workload"] == workload and r["workers"] == workers
        and (rate is None or r.get("rate") == rate)
    ]
    if not cell_rows:
        return "<p><em>No rows for this cell.</em></p>"

    l_rows = [r for r in cell_rows if r["arm"] == "L"]
    a_rows = [r for r in cell_rows if r["arm"] == "A"]
    l_med = {k: med_val([r[k] for r in l_rows if k in r]) for k in ("ops_per_s", "client_cpu_us_per_op", "server_kcycles", "p50_us", "p99_us")}
    a_med = {k: med_val([r[k] for r in a_rows if k in r]) for k in ("ops_per_s", "client_cpu_us_per_op", "server_kcycles", "p50_us", "p99_us")}

    has_wire = bool(wire_map)
    wire_hdr = "<th>Storage f/pkt</th><th>Proxy f/pkt</th>" if has_wire else ""

    out = [
        "<div class='table-wrap'><table><thead><tr>",
        "<th>Arm</th><th>ops/s</th><th>Client CPU (µs/op)</th><th>Server (kcycles/tx)</th>",
        "<th>p50 (µs)</th><th>p99 (µs)</th><th> vs L (ops / CPU / srv / p50 / p99)</th>",
        "<th>vs A (ops / CPU / srv / p50 / p99)</th><th>Frames/write</th>",
        wire_hdr,
        "</tr></thead><tbody>",
    ]
    for arm in arms_order:
        rs = [r for r in cell_rows if r["arm"] == arm]
        if not rs:
            continue
        ops = fmt_med_spread([r["ops_per_s"] for r in rs], 0)
        cpu = fmt_med_spread([r["client_cpu_us_per_op"] for r in rs], 1)
        srv = fmt_med_spread([r["server_kcycles"] for r in rs], 1)
        p50 = fmt_med_spread([r["p50_us"] for r in rs], 0)
        p99 = fmt_med_spread([r["p99_us"] for r in rs], 0)
        fpw_vals = [r["lab_frames_per_write"] for r in rs if r.get("lab_frames_per_write")]
        fpw = fmt_med_spread(fpw_vals, 1) if fpw_vals else "-"

        def rel(base_med):
            parts = []
            for k in ("ops_per_s", "client_cpu_us_per_op", "server_kcycles", "p50_us", "p99_us"):
                mv = med_val([r[k] for r in rs if k in r])
                bm = base_med.get(k)
                parts.append(f"{mv / bm:.2f}×" if mv and bm else "-")
            return " / ".join(parts)

        tr_cls = ""
        if arm in ("DNG", "DNGa20", "D5"):
            tr_cls = " class='highlight'"
        elif arm == "M":
            tr_cls = " class='highlight-warn'"

        w_cols = ""
        if has_wire:
            w = wire_map.get(arm, {})
            ss = f"{w['ss_fps']:.1f}" if w.get("ss_fps") else "-"
            px = f"{w['px_fps']:.1f}" if w.get("px_fps") else "-"
            w_cols = f"<td>{ss}</td><td>{px}</td>"

        out.append(
            f"<tr{tr_cls}><td>{html.escape(ARM_SHORT.get(arm, arm))}</td>"
            f"<td>{ops}</td><td>{cpu}</td><td>{srv}</td><td>{p50}</td><td>{p99}</td>"
            f"<td>{rel(l_med)}</td><td>{rel(a_med)}</td><td>{fpw}</td>{w_cols}</tr>"
        )
    out.append("</tbody></table></div>")
    return "\n".join(out)


def svg_scatter_tradeoff(title, subtitle, points, x_label, y_label, width=640, height=360):
    """points: list of (label, x_val, y_val, color)."""
    pts = [(lbl, x, y, c) for (lbl, x, y, c) in points if x is not None and y is not None]
    if not pts:
        return ""
    pad_l, pad_r, pad_t, pad_b = 64, 32, 48, 48
    pw, ph = width - pad_l - pad_r, height - pad_t - pad_b
    xs = [p[1] for p in pts]
    ys = [p[2] for p in pts]
    xmin, xmax = min(xs) * 0.92, max(xs) * 1.08
    ymin, ymax = min(ys) * 0.90, max(ys) * 1.10
    if xmax <= xmin:
        xmax = xmin + 1
    if ymax <= ymin:
        ymax = ymin + 1

    def sx(x):
        return pad_l + (x - xmin) / (xmax - xmin) * pw

    def sy(y):
        return pad_t + ph - (y - ymin) / (ymax - ymin) * ph

    elems = [
        f"<div class='svg-wrap'><svg viewBox='0 0 {width} {height}' width='100%' height='{height}' xmlns='http://www.w3.org/2000/svg'>",
        f"<text x='{pad_l}' y='22' font-size='13' font-weight='600' fill='#0f172a'>{html.escape(title)}</text>",
        f"<text x='{pad_l}' y='38' font-size='11' fill='#64748b'>{html.escape(subtitle)}</text>",
    ]
    for i in range(5):
        gy = pad_t + ph * i / 4
        vy = ymax - (ymax - ymin) * i / 4
        elems.append(f"<line x1='{pad_l}' y1='{gy:.1f}' x2='{width - pad_r}' y2='{gy:.1f}' stroke='#e2e8f0' stroke-width='1'/>")
        elems.append(f"<text x='{pad_l - 8}' y='{gy + 4:.1f}' font-size='10.5' fill='#64748b' text-anchor='end'>{vy:,.0f}</text>")
        gx = pad_l + pw * i / 4
        vx = xmin + (xmax - xmin) * i / 4
        elems.append(f"<line x1='{gx:.1f}' y1='{pad_t}' x2='{gx:.1f}' y2='{pad_t + ph}' stroke='#f1f5f9' stroke-width='1'/>")
        elems.append(f"<text x='{gx:.1f}' y='{pad_t + ph + 16}' font-size='10.5' fill='#64748b' text-anchor='middle'>{vx:,.0f}</text>")

    elems.append(f"<rect x='{pad_l}' y='{pad_t}' width='{pw}' height='{ph}' fill='none' stroke='#cbd5e1' stroke-width='1'/>")
    elems.append(f"<text x='{pad_l + pw / 2}' y='{height - 8}' font-size='11.5' font-weight='500' fill='#334155' text-anchor='middle'>{html.escape(x_label)}</text>")
    elems.append(f"<text x='16' y='{pad_t + ph / 2}' font-size='11.5' font-weight='500' fill='#334155' text-anchor='middle' transform='rotate(-90 16 {pad_t + ph / 2})'>{html.escape(y_label)}</text>")

    # Connect fixed linger curve if present in order
    linger_arms = ["D-NG (0µs)", "D5 (5µs)", "D (10–20µs)", "D50 (50µs)", "D100 (100µs)", "D250 (250µs)"]
    curve_pts = []
    for la in linger_arms:
        for lbl, x, y, _ in pts:
            if lbl == la:
                curve_pts.append((sx(x), sy(y)))
    if len(curve_pts) >= 3:
        d_str = " ".join(f"{'M' if i == 0 else 'L'}{cx:.1f},{cy:.1f}" for i, (cx, cy) in enumerate(curve_pts))
        elems.append(f"<path d='{d_str}' fill='none' stroke='#93c5fd' stroke-width='2' stroke-dasharray='4,3'/>")

    placed = []
    for lbl, x, y, color in pts:
        cx, cy = sx(x), sy(y)
        elems.append(f"<circle cx='{cx:.1f}' cy='{cy:.1f}' r='5.5' fill='{color}' stroke='#ffffff' stroke-width='1.5'/>")
        dy = -9
        for px, py in placed:
            if abs(cx - px) < 55 and abs((cy + dy) - py) < 12:
                dy = 15 if dy < 0 else -18
        placed.append((cx, cy + dy))
        anchor = "end" if cx > width - pad_r - 90 else "start"
        dx = -8 if anchor == "end" else 8
        elems.append(f"<text x='{cx + dx:.1f}' y='{cy + dy:.1f}' font-size='10.5' font-weight='600' fill='{color}' text-anchor='{anchor}'>{html.escape(lbl)}</text>")

    elems.append("</svg></div>")
    return "\n".join(elems)


def svg_grouped_bars(title, subtitle, categories, series, y_label, width=640, height=340):
    """categories: list of str; series: list of (series_name, color, [vals])."""
    if not categories or not series:
        return ""
    pad_l, pad_r, pad_t, pad_b = 60, 24, 54, 52
    pw, ph = width - pad_l - pad_r, height - pad_t - pad_b
    all_vals = [v for _, _, vals in series for v in vals if v is not None]
    ymax = max(all_vals) * 1.15 if all_vals else 1.0
    if ymax <= 0:
        ymax = 1.0

    elems = [
        f"<div class='svg-wrap'><svg viewBox='0 0 {width} {height}' width='100%' height='{height}' xmlns='http://www.w3.org/2000/svg'>",
        f"<text x='{pad_l}' y='20' font-size='13' font-weight='600' fill='#0f172a'>{html.escape(title)}</text>",
        f"<text x='{pad_l}' y='35' font-size='11' fill='#64748b'>{html.escape(subtitle)}</text>",
    ]
    # Legend
    lx = pad_l
    for sname, scolor, _ in series:
        elems.append(f"<rect x='{lx}' y='41' width='10' height='10' rx='2' fill='{scolor}'/>")
        elems.append(f"<text x='{lx + 14}' y='50' font-size='10.5' fill='#334155'>{html.escape(sname)}</text>")
        lx += 18 + len(sname) * 6.5

    for i in range(5):
        gy = pad_t + ph * i / 4
        vy = ymax * (1 - i / 4)
        elems.append(f"<line x1='{pad_l}' y1='{gy:.1f}' x2='{width - pad_r}' y2='{gy:.1f}' stroke='#e2e8f0' stroke-width='1'/>")
        elems.append(f"<text x='{pad_l - 8}' y='{gy + 4:.1f}' font-size='10.5' fill='#64748b' text-anchor='end'>{vy:,.0f}</text>")

    ncat = len(categories)
    nser = len(series)
    group_w = pw / ncat
    bar_w = min(28.0, (group_w * 0.74) / max(nser, 1))
    for ci, cat in enumerate(categories):
        gx0 = pad_l + ci * group_w + (group_w - bar_w * nser) / 2
        for si, (sname, scolor, vals) in enumerate(series):
            v = vals[ci] if ci < len(vals) and vals[ci] is not None else 0.0
            bh = (v / ymax) * ph
            bx = gx0 + si * bar_w
            by = pad_t + ph - bh
            elems.append(f"<rect x='{bx:.1f}' y='{by:.1f}' width='{bar_w - 2:.1f}' height='{max(bh, 1):.1f}' fill='{scolor}' rx='2'/>")
            if bh > 14:
                elems.append(f"<text x='{bx + (bar_w - 2) / 2:.1f}' y='{by - 4:.1f}' font-size='9.5' fill='#334155' text-anchor='middle'>{v:,.0f}</text>")
        elems.append(f"<text x='{pad_l + (ci + 0.5) * group_w:.1f}' y='{pad_t + ph + 18}' font-size='10.5' font-weight='500' fill='#334155' text-anchor='middle'>{html.escape(cat)}</text>")

    elems.append(f"<line x1='{pad_l}' y1='{pad_t + ph}' x2='{width - pad_r}' y2='{pad_t + ph}' stroke='#94a3b8' stroke-width='1'/>")
    elems.append(f"<text x='15' y='{pad_t + ph / 2}' font-size='11' font-weight='500' fill='#334155' text-anchor='middle' transform='rotate(-90 15 {pad_t + ph / 2})'>{html.escape(y_label)}</text>")
    elems.append("</svg></div>")
    return "\n".join(elems)


def svg_stacked_role_bars(title, subtitle, rows, workload, workers, arms_order, rate=None, width=680, height=360):
    cell_rows = [
        r for r in rows
        if r["workload"] == workload and r["workers"] == workers
        and (rate is None or r.get("rate") == rate)
        and "role_cycles_per_tx" in r
    ]
    if not cell_rows:
        return ""
    roles = [
        ("storage", "Storage (4753)", "#2563eb"),
        ("grv_proxy", "GRV Proxy (4754)", "#059669"),
        ("commit_proxy", "Commit Proxy (4755)", "#d97706"),
        ("transaction", "Transaction/TLog (4752)", "#7c3aed"),
        ("stateless", "Stateless/Resolver (4751)", "#64748b"),
    ]
    present_arms = [a for a in arms_order if any(r["arm"] == a for r in cell_rows)]
    if not present_arms:
        return ""

    pad_l, pad_r, pad_t, pad_b = 62, 24, 58, 56
    pw, ph = width - pad_l - pad_r, height - pad_t - pad_b

    arm_role_kc = {}
    max_tot = 1.0
    for a in present_arms:
        rs = [r for r in cell_rows if r["arm"] == a]
        d = {}
        for rk, _, _ in roles:
            vals = [r["role_cycles_per_tx"].get(rk, 0.0) / 1e3 for r in rs]
            d[rk] = max(0.0, statistics.median(vals)) if vals else 0.0
        arm_role_kc[a] = d
        max_tot = max(max_tot, sum(d.values()))
    ymax = max_tot * 1.16

    elems = [
        f"<div class='svg-wrap'><svg viewBox='0 0 {width} {height}' width='100%' height='{height}' xmlns='http://www.w3.org/2000/svg'>",
        f"<text x='{pad_l}' y='20' font-size='13' font-weight='600' fill='#0f172a'>{html.escape(title)}</text>",
        f"<text x='{pad_l}' y='35' font-size='11' fill='#64748b'>{html.escape(subtitle)}</text>",
    ]
    lx = pad_l
    for _, rlabel, rcolor in roles:
        elems.append(f"<rect x='{lx}' y='42' width='10' height='10' rx='2' fill='{rcolor}'/>")
        elems.append(f"<text x='{lx + 13}' y='51' font-size='10' fill='#334155'>{html.escape(rlabel)}</text>")
        lx += 16 + len(rlabel) * 5.8

    for i in range(5):
        gy = pad_t + ph * i / 4
        vy = ymax * (1 - i / 4)
        elems.append(f"<line x1='{pad_l}' y1='{gy:.1f}' x2='{width - pad_r}' y2='{gy:.1f}' stroke='#e2e8f0' stroke-width='1'/>")
        elems.append(f"<text x='{pad_l - 8}' y='{gy + 4:.1f}' font-size='10.5' fill='#64748b' text-anchor='end'>{vy:,.0f}</text>")

    gw = pw / len(present_arms)
    bw = min(38.0, gw * 0.62)
    for idx, a in enumerate(present_arms):
        bx = pad_l + idx * gw + (gw - bw) / 2
        y_cur = pad_t + ph
        tot = sum(arm_role_kc[a].values())
        for rk, _, rcolor in roles:
            v = arm_role_kc[a][rk]
            bh = (v / ymax) * ph
            y_cur -= bh
            if bh >= 0.5:
                elems.append(f"<rect x='{bx:.1f}' y='{y_cur:.1f}' width='{bw:.1f}' height='{bh:.1f}' fill='{rcolor}'/>")
        elems.append(f"<text x='{bx + bw / 2:.1f}' y='{y_cur - 5:.1f}' font-size='10' font-weight='600' fill='#0f172a' text-anchor='middle'>{tot:,.1f}k</text>")
        lbl = ARM_SHORT.get(a, a)
        elems.append(f"<text x='{bx + bw / 2:.1f}' y='{pad_t + ph + 16}' font-size='10' font-weight='500' fill='#334155' text-anchor='middle' transform='rotate(-18 {bx + bw / 2:.1f} {pad_t + ph + 16})'>{html.escape(lbl)}</text>")

    elems.append(f"<line x1='{pad_l}' y1='{pad_t + ph}' x2='{width - pad_r}' y2='{pad_t + ph}' stroke='#94a3b8' stroke-width='1'/>")
    elems.append(f"<text x='15' y='{pad_t + ph / 2}' font-size='11' font-weight='500' fill='#334155' text-anchor='middle' transform='rotate(-90 15 {pad_t + ph / 2})'>Server kcycles / tx</text>")
    elems.append("</svg></div>")
    return "\n".join(elems)


def load_wire_dir(rel_dir):
    p = os.path.join(HERE, rel_dir)
    out = {}
    for f in sorted(glob.glob(os.path.join(p, "r*-wirestats.json"))):
        arm = os.path.basename(f).split("-")[1]
        w = json.load(open(f))
        t_path = f.replace("-wirestats.json", "-wiretrace.json")
        t = json.load(open(t_path)) if os.path.exists(t_path) else {"kinds": {}}
        g = t.get("kinds", {}).get("grv", {})
        out[arm] = {
            "ss_fps": w.get("4753/c2s", {}).get("frames_per_segment"),
            "px_fps": w.get("4751/c2s", {}).get("frames_per_segment"),
            "grv_fps": w.get("4754/c2s", {}).get("frames_per_segment"),
            "cp_fps": w.get("4755/c2s", {}).get("frames_per_segment"),
            "tx_fps": w.get("4752/c2s", {}).get("frames_per_segment"),
            "ss_s2c_fps": w.get("4753/s2c", {}).get("frames_per_segment"),
            "cp_s2c_fps": w.get("4755/s2c", {}).get("frames_per_segment"),
            "ss_frames": w.get("4753/c2s", {}).get("frames_per_op"),
            "px_frames": w.get("4751/c2s", {}).get("frames_per_op"),
            "px_bytes": w.get("4751/c2s", {}).get("bytes_per_op"),
            "ss_bytes": w.get("4753/c2s", {}).get("bytes_per_op"),
            "grv_tpr": g["transactions"] / g["requests"] if g.get("requests") else None,
            "flows": w.get("client_flows"),
        }
    return out


def generate_all():
    os.makedirs(REPORT_DIR, exist_ok=True)

    hz4_ladder = load_jsonl("rawhz4/ladder.jsonl")
    hz4_rate = load_jsonl("rawhz4/rate.jsonl")
    hz4_cross = load_jsonl("rawhz4/cross/ladder.jsonl")
    hz5_ladder = load_jsonl("rawhz5/ladder.jsonl")
    hz5_rate = load_jsonl("rawhz5/rate.jsonl")
    hz5_sys = load_jsonl("rawhz5/syscalls.jsonl")
    hz6_ladder = load_jsonl("rawhz6/ladder.jsonl")
    hz6_rate = load_jsonl("rawhz6/rate.jsonl")
    hz6_sys = load_jsonl("rawhz6/syscalls.jsonl")
    hz6_linger = load_jsonl("rawhz6/linger.jsonl")
    hz6_linger_rate = load_jsonl("rawhz6/linger_rate.jsonl")
    hz6_quick1 = load_jsonl("rawhz6/quick/ladder.jsonl")
    hz6_quick2 = load_jsonl("rawhz6/quick2/ladder.jsonl")
    hz6b_ladder = load_jsonl("rawhz6b/role_ladder.jsonl")
    hz6b_rate = load_jsonl("rawhz6b/role_rate.jsonl")

    # Merge hz6_ladder + new arms from hz6_linger so combined tables on netg-r6 have all arms
    existing_hz6_arms = {r["arm"] for r in hz6_ladder}
    hz6_combined = list(hz6_ladder) + [r for r in hz6_linger if r["arm"] not in existing_hz6_arms]

    # 1. index.html
    write_index(hz6_combined, hz6_linger, hz6_linger_rate, hz6_rate, hz5_ladder, hz6b_ladder, hz6b_rate)
    # 2. linger.html
    write_linger(hz6_linger, hz6_linger_rate, hz6b_ladder, hz6b_rate)
    # 3. queues.html
    write_queues(hz6_combined, hz6_rate, hz6_sys, hz6_quick1, hz6_quick2, hz5_ladder, hz6b_ladder)
    # 4. delay-vs-ack.html
    write_delay_vs_ack(hz6_combined, hz6_rate, hz6_sys, hz6_linger)
    # 5. libfdbc.html
    write_libfdbc(hz5_ladder, hz5_rate, hz5_sys, hz4_ladder, hz6_combined)
    # 6. methodology.html
    write_methodology(hz4_ladder, hz4_cross, hz5_ladder, hz6_ladder, hz6_linger, hz6b_ladder)

    if os.path.exists(TMP_REPORT_DIR):
        shutil.rmtree(TMP_REPORT_DIR)
    shutil.copytree(REPORT_DIR, TMP_REPORT_DIR)
    print(f"Generated 6 HTML pages in {REPORT_DIR} and copied to {TMP_REPORT_DIR}")


def make_linger_points(rows, workload, workers, x_key, y_key, arms, rate=None):
    pts = []
    for a in arms:
        rs = [
            r for r in rows
            if r["workload"] == workload and r["workers"] == workers and r["arm"] == a
            and (rate is None or r.get("rate") == rate)
        ]
        if rs:
            x = med_val([r[x_key] for r in rs if x_key in r])
            y = med_val([r[y_key] for r in rs if y_key in r])
            pts.append((ARM_SHORT.get(a, a), x, y, ARM_COLORS.get(a, "#334155")))
    return pts


def write_index(hz6_combined, hz6_linger, hz6_linger_rate, hz6_rate, hz5_ladder, hz6b_ladder, hz6b_rate):
    linger_arms = ["L", "A", "A0", "M", "DNG", "DNGy", "DNGa20", "DNGa100", "D5", "D", "D50", "D100", "D250"]
    all_r6_arms = ["L", "A", "A0", "M", "DNG", "DNGy", "DNGchan", "DNGring", "DNGa20", "DNGa100", "D5", "D", "D50", "D100", "D250"]

    chart_get64 = svg_scatter_tradeoff(
        "Closed-Loop Trade-Off: get100 / 64 workers (netg-r6)",
        "Lower-left is better: p50 latency (µs) vs server CPU (kcycles/tx)",
        make_linger_points(hz6_linger or hz6_combined, "get100", 64, "server_kcycles", "p50_us", linger_arms),
        "Server CPU (kcycles / tx)",
        "Client p50 Latency (µs)",
    )
    chart_get256 = svg_scatter_tradeoff(
        "Closed-Loop Saturation: get100 / 256 workers (netg-r6)",
        "Lower-left is better: p50 latency (µs) vs server CPU (kcycles/tx)",
        make_linger_points(hz6_linger or hz6_combined, "get100", 256, "server_kcycles", "p50_us", linger_arms),
        "Server CPU (kcycles / tx)",
        "Client p50 Latency (µs)",
    )
    chart_rate_get64 = svg_scatter_tradeoff(
        "Equal-Throughput (T4): get100 / 64 workers @ 28,000 ops/s (90% of L)",
        "At identical throughput, all Go arms beat L on p50/p99; linger controls server CPU",
        make_linger_points(hz6_linger_rate, "get100", 64, "server_kcycles", "p50_us", linger_arms, rate=28000),
        "Server CPU (kcycles / tx)",
        "Client p50 Latency (µs)",
    )
    chart_rate_set64 = svg_scatter_tradeoff(
        "Equal-Throughput (T4): set100 / 64 workers @ 17,000 ops/s (90% of L)",
        "At identical commit rate, Go arms beat L on p50/p99; server kcycles/tx within 1–12% depending on linger",
        make_linger_points(hz6_linger_rate, "set100", 64, "server_kcycles", "p50_us", linger_arms, rate=17000),
        "Server CPU (kcycles / tx)",
        "Client p50 Latency (µs)",
    )

    body = f"""
<div class="callout good">
  <strong>Headline Findings (Rounds 1–6):</strong>
  <ul>
    <li><strong>Enqueuing without write acknowledgment (<code>D1</code>: per-connection mutex+slice writer signalled on empty&rarr;non-empty) and per-transaction weak-pointer <code>time.AfterFunc</code> (<code>D4</code>) strictly dominate today's transport (<code>A</code>)</strong> across every concurrency level, reducing client CPU by 12–22%, futex calls by up to 81% at 256 workers, and latency across all workloads.</li>
    <li><strong>The 10–20 &micro;s C++ coalesce delay (<code>D</code>) is a pure latency-vs-server-CPU knob, not wire correctness:</strong> at 0 &micro;s linger (<code>D-NG</code>), Go achieves <strong>257.7k ops/s</strong> on <code>get100/256</code> (<strong>5.1&times;</strong> <code>libfdb_c</code>'s 50.3k ops/s) and 52k–80k ops/s on <code>get100/64</code>. Adding fixed linger (5&ndash;250 &micro;s) traces a smooth Pareto curve trading +10&ndash;700 &micro;s of client p50 for 10&ndash;25% lower server CPU/tx.</li>
    <li><strong>Sender-writes without a writer goroutine (<code>D-M</code> / <code>M</code>) is fastest on the happy path when sockets never fill, and matches <code>D-NG</code> on <code>netg-r6</code>, but fails <code>T1</code> and <code>T2</code> under socket backpressure:</strong> a sender that wins <code>TryLock</code> blocks inside <code>write(2)</code> when the kernel send buffer fills, ignoring <code>ctx</code> cancellation and acting as a queue-wide write acknowledgment (10 deterministic <code>-race</code> failures).</li>
    <li><strong>Writer queue structure is not the bottleneck:</strong> mutex+slice (<code>D-NG</code>), channel (<code>D-NG-chan</code>), and a fixed lock-free MPSC ring with <code>writev</code> (<code>D-NG-ring</code> v2) finish within 5% of each other at 243k&ndash;258k ops/s. Profiling shows the send-queue mutex is <strong>0.16% of CPU</strong> and <strong>1.2% of lock wait</strong>; <strong>75.8% of all lock contention</strong> is the connection reply map <code>pendingMu</code> (<code>PrepareReplyTo</code> 50.1% + <code>readLoop</code> 25.8%).</li>
  </ul>
</div>

<h2>1. Ground Truths (T1–T6) &amp; Architecture Comparison</h2>
<div class="grid-2">
  <div class="card">
    <h3>What <code>libfdb_c</code> 7.3.77 Does (C1–C5)</h3>
    <ul>
      <li><strong>C1. Send is enqueue + future:</strong> <code>Peer::send</code> appends to <code>unsent</code> and fires <code>dataToSend</code> only on the first unsent packet (<code>FlowTransport.actor.cpp:1038-1043</code>). Reads use <code>tryGetReply</code> (<code>LoadBalance.actor.h:305,312</code>); callers never wait for socket write completion.</li>
      <li><strong>C2. Single writer per connection with jittered coalesce delay:</strong> <code>connectionWriter</code> (<code>FlowTransport.actor.cpp:704-738</code>) waits <code>delayJittered(max(MIN_COALESCE_DELAY, MAX_COALESCE_DELAY - (now - lastWrite)))</code> (10/20 &micro;s, <code>flow/Knobs.cpp:211-212</code>), writes up to 128 KiB, and sleeps until <code>dataToSend</code>. Note that commit <code>a674cb4ef4</code> kept <code>delay(0)</code>, <code>delay(500e-6)</code>, and <code>yield(TaskWriteSocket)</code> commented beside it, and <code>BUGGIFY</code> sets both knobs to 0.</li>
      <li><strong>C3. Adaptive GRV batching:</strong> window is <code>min(EWMA(0.5 &times; reply_latency), 5 ms)</code> up to <code>MAX_BATCH_SIZE = 1000</code> (<code>NativeAPI.actor.cpp:7351-7365</code>).</li>
      <li><strong>C4. Opt-in transaction timeout:</strong> one <code>timebomb</code> actor runs only when <code>TIMEOUT</code> is set (<code>ReadYourWrites.actor.cpp:1567-1579</code>). Per-read RPCs arm no reply timeout (only <code>secondDelay</code> when a second storage alternative exists).</li>
      <li><strong>C5. Local cancellation:</strong> cancelling a future drops the caller's wait without mutating already-queued wire frames.</li>
    </ul>
  </div>
  <div class="card">
    <h3>What the Go Client Must Satisfy (T1–T6)</h3>
    <ul>
      <li><strong>T1 (No write ack):</strong> Callers enqueue the encoded frame and wait only on the reply channel. Write errors call <code>failConnection</code> and fail all pending replies.</li>
      <li><strong>T2 (Local <code>ctx</code>):</strong> Cancelling <code>ctx</code> ends the caller's wait immediately and never un-queues or truncates a frame, nor blocks inside a kernel <code>write(2)</code> syscall.</li>
      <li><strong>T3 (Zero-cost unset timeout):</strong> No timer is armed when transaction <code>TIMEOUT</code> is unset; one weak-pointer <code>time.AfterFunc</code> per transaction when set (<code>D4</code>).</li>
      <li><strong>T4 (Parity at equal throughput):</strong> At a rate both clients sustain on one box, p50 and p99 are no higher than <code>L</code>'s and server cycles/tx are within noise/tolerance of <code>L</code>.</li>
      <li><strong>T5 (Wire correctness):</strong> Identical payload bytes, per-connection frame ordering, whole frames, and identical request sets (verified by 1-worker tcpdump and <code>wiregolden</code>).</li>
      <li><strong>T6 (Goroutine-per-op hot path):</strong> Enqueue + single-waiter wakeup on reply delivery with pooled reply handles/channels (0 allocs on transport reply path).</li>
    </ul>
  </div>
</div>

<h3>Transport Architecture Matrix Tested in Rounds 4–6</h3>
<div class="table-wrap">
<table>
  <thead>
    <tr>
      <th>Arm</th>
      <th class="text-left">Architecture / Flag</th>
      <th class="text-left">Write Path &amp; Coalescing Policy</th>
      <th>Meets T1/T2?</th>
      <th>Passes <code>-race</code>?</th>
    </tr>
  </thead>
  <tbody>
    <tr>
      <td><code>L</code></td>
      <td class="text-left"><code>clientbench-cgo</code> (<code>libfdb_c.so</code> 7.3.77)</td>
      <td class="text-left">Single C++ <code>Net2</code> reactor thread; <code>Peer::send</code> + <code>connectionWriter</code> with 10/20 &micro;s jittered delay</td>
      <td><span class="badge info">Reference</span></td>
      <td><span class="badge info">C++</span></td>
    </tr>
    <tr>
      <td><code>L4</code></td>
      <td class="text-left"><code>libfdb_c</code> with <code>client_threads_per_version=4</code>, 4 DBs</td>
      <td class="text-left">4 independent C++ <code>Net2</code> threads and connection pools; round-robin DB handles</td>
      <td><span class="badge info">Reference (4&times; conns)</span></td>
      <td><span class="badge info">C++</span></td>
    </tr>
    <tr>
      <td><code>A</code></td>
      <td class="text-left">Today's Go transport (default)</td>
      <td class="text-left">Direct non-blocking sender write when idle + writer turns + <code>writeCh</code> handoff + write-ack wait + 10/20 &micro;s <code>hrtimer</code></td>
      <td><span class="badge warn">Waits for write-ack</span></td>
      <td><span class="badge pass">PASS</span></td>
    </tr>
    <tr>
      <td><code>A0</code></td>
      <td class="text-left">Today's Go transport with <code>-coalesce-delay=0,0</code></td>
      <td class="text-left">Same as <code>A</code> (direct write + write-ack), but with coalescing delay disabled</td>
      <td><span class="badge warn">Waits for write-ack</span></td>
      <td><span class="badge pass">PASS</span></td>
    </tr>
    <tr class="highlight-warn">
      <td><code>M</code> (<code>D-M</code>)</td>
      <td class="text-left"><code>FDBGO_NET_DESIGN=m</code></td>
      <td class="text-left">No writer goroutine: sender that wins <code>wmu.TryLock()</code> writes its frame and drains <code>pending</code>; losers append and return</td>
      <td><span class="badge fail">FAILS T1/T2 on full socket</span></td>
      <td><span class="badge fail">10 backpressure fails</span></td>
    </tr>
    <tr class="highlight">
      <td><code>D-NG</code></td>
      <td class="text-left"><code>FDBGO_NET_DESIGN=ng</code></td>
      <td class="text-left"><code>D1</code> mutex+slice queue, 1-slot signal on empty&rarr;non-empty, writer swaps slice and writes immediately (0 &micro;s linger)</td>
      <td><span class="badge pass">YES (T1 + T2)</span></td>
      <td><span class="badge pass">PASS (all targets)</span></td>
    </tr>
    <tr>
      <td><code>D-NG+yield</code></td>
      <td class="text-left"><code>FDBGO_NET_DESIGN=ngy</code></td>
      <td class="text-left"><code>D-NG</code> + one <code>runtime.Gosched()</code> after each socket <code>Write</code> before next drain</td>
      <td><span class="badge pass">YES (T1 + T2)</span></td>
      <td><span class="badge pass">PASS (all targets)</span></td>
    </tr>
    <tr>
      <td><code>D-NG-chan</code></td>
      <td class="text-left"><code>FDBGO_NET_DESIGN=ngc</code></td>
      <td class="text-left">Senders push encoded frame to buffered <code>chan chanFrame</code>; writer drains <code>1 + len(ch)</code> frames per write</td>
      <td><span class="badge pass">YES (T1 + T2)</span></td>
      <td><span class="badge pass">PASS (all targets)</span></td>
    </tr>
    <tr>
      <td><code>D-NG-ring</code> v2</td>
      <td class="text-left"><code>FDBGO_NET_DESIGN=ngr</code></td>
      <td class="text-left">Lock-free MPSC 1 MiB ring: 1 CAS claims <code>seq16|pos48</code>, encodes in-place, sets <code>slot.ready</code>; writer issues <code>writev</code></td>
      <td><span class="badge pass">YES (T1 + T2)</span></td>
      <td><span class="badge pass">PASS (all targets)</span></td>
    </tr>
    <tr class="highlight">
      <td><code>D-NG-adap20/100</code></td>
      <td class="text-left"><code>FDBGO_NET_DESIGN=nga</code> (<code>20</code> / <code>100</code> &micro;s)</td>
      <td class="text-left">Nagle-style adaptive linger: write immediately if <code>sinceLastWrite &ge; W</code>; else <code>runtime.Gosched()</code> spin until <code>lastWrite + W</code></td>
      <td><span class="badge pass">YES (T1 + T2)</span></td>
      <td><span class="badge pass">PASS (all targets)</span></td>
    </tr>
    <tr class="highlight">
      <td><code>D5 / D / D50 / D100 / D250</code></td>
      <td class="text-left"><code>FDBGO_NET_DESIGN=1</code> with <code>-coalesce-delay=L,L</code></td>
      <td class="text-left"><code>D1</code> mutex+slice writer with fixed/C2 <code>hrtimer</code> linger (5 &micro;s, 10&ndash;20 &micro;s, 50 &micro;s, 100 &micro;s, 250 &micro;s) before swapping buffer</td>
      <td><span class="badge pass">YES (T1 + T2)</span></td>
      <td><span class="badge pass">PASS (all targets)</span></td>
    </tr>
  </tbody>
</table>
</div>

<h2>2. Linger &amp; Coalescing Trade-Off Curves (Closed-Loop vs Rate-Capped)</h2>
<div class="grid-2">
  {chart_get64}
  {chart_get256}
</div>
<div class="grid-2">
  {chart_rate_get64}
  {chart_rate_set64}
</div>

<h2>3. Headline Scoreboard on <code>netg-r6</code> (3-Process Cluster, 3 Rotated Rounds)</h2>
<p>Combined round-6 closed-loop scoreboard on <code>netg-r6</code> across <code>get100</code>, <code>set100</code>, and <code>getset100</code> at 8, 64, and 256 workers. (See <a href="linger.html">Page 2: Linger Sweep</a> for full multi-rate capped and 5-process per-role breakdowns, and <a href="libfdbc.html">Page 5: libfdb_c</a> for <code>L4</code> and <code>D-M 4Ps</code>.)</p>

<h3>get100 — 8, 64, and 256 workers</h3>
<h4>get100 / 8 workers</h4>
{build_cell_table(hz6_combined, "get100", 8, all_r6_arms)}
<h4>get100 / 64 workers</h4>
{build_cell_table(hz6_combined, "get100", 64, all_r6_arms)}
<h4>get100 / 256 workers</h4>
{build_cell_table(hz6_combined, "get100", 256, all_r6_arms)}

<h3>set100 — 8, 64, and 256 workers</h3>
<h4>set100 / 8 workers</h4>
{build_cell_table(hz6_combined, "set100", 8, all_r6_arms)}
<h4>set100 / 64 workers</h4>
{build_cell_table(hz6_combined, "set100", 64, all_r6_arms)}
<h4>set100 / 256 workers</h4>
{build_cell_table(hz6_combined, "set100", 256, all_r6_arms)}

<h3>getset100 — 8, 64, and 256 workers</h3>
<h4>getset100 / 8 workers</h4>
{build_cell_table(hz6_combined, "getset100", 8, all_r6_arms)}
<h4>getset100 / 64 workers</h4>
{build_cell_table(hz6_combined, "getset100", 64, all_r6_arms)}
<h4>getset100 / 256 workers</h4>
{build_cell_table(hz6_combined, "getset100", 256, all_r6_arms)}

<h2>4. T4 Verdict at Equal Throughput &amp; Concrete RFC Recommendations</h2>
<div class="grid-2">
  <div class="card">
    <h3>T4 Verdict: Equal Throughput vs Closed Loop</h3>
    <ul>
      <li><strong>Rate-capped (equal throughput, the T4 definition across 25%, 60%, and 90% of <code>L</code>):</strong>
        <ul>
          <li><strong>At 25% and 60% of <code>L</code>'s ceiling</strong> (where <code>L</code>'s single network thread is not yet congested), <code>D-NG</code> (0 &micro;s linger) has <strong>lower or equal server CPU/tx than <code>L</code></strong>: on <code>get100/64 @ 8,000 ops/s</code>, <code>D-NG</code> is <code>175.8 vs 221.5 kc/tx</code> (<strong>0.79&times; L</strong>, p99 <code>1,948 vs 4,825 &micro;s</code> = <strong>0.40&times; L</strong>); on <code>set100/64 @ 4,800 ops/s</code>, <code>D-NG</code> is <code>426.0 vs 432.7 kc/tx</code> (<strong>0.98&times; L</strong>); on <code>set100/64 @ 11,500 ops/s</code>, <code>D-NG</code> is <code>209.1 vs 209.8 kc/tx</code> (<strong>1.00&times; L</strong>).</li>
          <li><strong>On the 5-process split cluster (<code>netg-r6b</code>) at <code>set100/64 @ 11,000 ops/s</code></strong> (equal throughput for all 11 arms), every Go arm beats <code>L</code> on total server CPU/tx: <code>D-NG</code> is <code>378.1 vs 394.5 kc/tx</code> (<strong>0.96&times; L</strong>), <code>D</code> is <code>367.2 kc/tx</code> (<strong>0.93&times; L</strong>), and <code>D50</code> is <code>355.8 kc/tx</code> (<strong>0.90&times; L</strong>).</li>
          <li><strong>At 90% of <code>L</code>'s ceiling on the 3-process cluster</strong> (<code>set100/64 @ 17,000 ops/s</code>, <code>get100/64 @ 28,000 ops/s</code>), <code>L</code>'s single network thread is ~89% saturated and queues 15&ndash;30 frames per write behind its reactor loop. Even there, every Go arm beats <code>L</code> on p50 (<strong>0.56&ndash;0.92&times; L</strong>) and p99 (<strong>0.24&ndash;0.74&times; L</strong>), while fixed linger (<code>D50</code>/<code>D100</code>) brings server CPU within 3&ndash;15% of <code>L</code>.</li>
        </ul>
      </li>
      <li><strong>Why <code>L</code> packs more frames at 28k ops/s on <code>get100/64</code>:</strong> 28,000 ops/s &times; 100 reads/op = 2.8 million read RPCs/s. At 28k ops/s, <code>L</code>'s single network thread is 89% saturated, so responses and outgoing reads back up behind one event loop and naturally batch 15&ndash;30 frames/write. Go spreads callers across 8 Ps, so each connection's queue drains rapidly unless a linger window holds it.</li>
    </ul>
  </div>
  <div class="card">
    <h3>Concrete RFC Recommendations</h3>
    <ol>
      <li><strong>Ship <code>D1</code> (per-connection mutex + byte-slice writer signalled on empty&rarr;non-empty) and <code>D4</code> (single per-transaction weak-pointer <code>time.AfterFunc</code>):</strong>
        Replaces <code>writeLoop</code>'s direct-write/writer-turn/write-ack state machine and the global <code>timeoutQueue</code> heap + <code>runtime.AddCleanup</code>. Simpler, satisfies <code>T1</code>–<code>T3</code>, passes the entire test suite under <code>-race</code>, and cuts client CPU by 12&ndash;22%.</li>
      <li><strong>Do NOT adopt <code>D-M</code> (blocking sender writes) or <code>D-NG-ring</code> (lock-free MPSC ring):</strong>
        <code>D-M</code> violates <code>T1</code>/<code>T2</code> under TCP socket backpressure, and <code>D-NG</code> achieves the same throughput safely. <code>D-NG-ring</code> v2 adds 200 lines of unsafe-like bit-packing for 0% gain because the send-queue mutex is only 0.16% of CPU.</li>
      <li><strong>Coalescing policy (<code>D2</code>):</strong>
        Make flush-on-empty (<code>D-NG</code>, 0 &micro;s linger) or a small adaptive/fixed linger (<code>D5</code> / <code>DNGa20</code>) the default, while retaining <code>WithCoalesceDelay(min, max)</code> for clusters that prioritize storage/proxy packet reduction over single-op latency.</li>
      <li><strong>Next high-leverage follow-ups identified by Round-6 profiling:</strong>
        <ul>
          <li><strong>Shard <code>Conn.pendingMu</code> (e.g. 16 or 64 token-hash shards):</strong> <code>PrepareReplyTo</code> (50.1%) + <code>readLoop</code> (25.8%) account for <strong>75.8% of all client mutex contention</strong> at 256 workers.</li>
          <li><strong>Remove or make opt-in the 5 s per-read RPC timer in <code>GetPipelined</code> / <code>waitReply</code>:</strong> C++ <code>tryGetReply</code> arms no per-read timeout; removing it will eliminate the remaining per-read timer traffic and shrink the 8-worker serial read gap against <code>L</code>.</li>
        </ul>
      </li>
    </ol>
  </div>
</div>
"""
    with open(os.path.join(REPORT_DIR, "index.html"), "w") as f:
        f.write(page_shell("index.html", "Executive Summary", body))


def write_linger(hz6_linger, hz6_linger_rate, hz6b_ladder, hz6b_rate):
    linger_arms = ["L", "A", "M", "DNG", "DNGy", "DNGa20", "DNGa100", "D5", "D", "D50", "D100", "D250"]
    rate_arms = ["L", "A", "M", "DNG", "DNGy", "DNGa20", "DNGa100", "D5", "D", "D50", "D100"]
    role_arms = ["L", "A", "A0", "M", "DNG", "DNGy", "DNGa20", "DNGa100", "D5", "D", "D50"]

    c1 = svg_scatter_tradeoff(
        "Linger Curve on get100 / 64 workers (Closed-Loop, netg-r6)",
        "Fixed linger 0 -> 5 -> 10-20 -> 50 -> 100 -> 250 µs vs Adaptive 20/100 µs",
        make_linger_points(hz6_linger, "get100", 64, "server_kcycles", "p50_us", linger_arms),
        "Server CPU (kcycles / tx)",
        "Client p50 Latency (µs)",
    )
    c2 = svg_scatter_tradeoff(
        "Linger Curve on set100 / 64 workers (Closed-Loop, netg-r6)",
        "Commit-heavy workload: p50 latency (µs) vs server CPU (kcycles/tx)",
        make_linger_points(hz6_linger, "set100", 64, "server_kcycles", "p50_us", linger_arms),
        "Server CPU (kcycles / tx)",
        "Client p50 Latency (µs)",
    )
    c3 = svg_stacked_role_bars(
        "5-Process Split Cluster: Per-Role Server CPU on get100 / 64 (netg-r6b)",
        "Stacked kcycles/tx across storage (4753), grv_proxy (4754), commit_proxy (4755), transaction (4752), stateless (4751)",
        hz6b_ladder, "get100", 64, role_arms,
    )
    c4 = svg_stacked_role_bars(
        "5-Process Split Cluster: Per-Role Server CPU on set100 / 64 (netg-r6b)",
        "Stacked kcycles/tx showing commit_proxy + transaction + storage + stateless + grv_proxy cost",
        hz6b_ladder, "set100", 64, role_arms,
    )
    c5 = svg_stacked_role_bars(
        "5-Process Split Cluster: Rate-Capped get100 / 64 @ 28,000 ops/s (netg-r6b)",
        "At identical throughput, storage + grv_proxy cycles scale inversely with frames/packet",
        hz6b_rate, "get100", 64, role_arms, rate=28000,
    )
    c6 = svg_stacked_role_bars(
        "5-Process Split Cluster: Rate-Capped set100 / 64 @ 11,000 ops/s (netg-r6b)",
        "At identical commit rate on the 5-process cluster, server cycles/tx across all 5 roles",
        hz6b_rate, "set100", 64, role_arms, rate=11000,
    )

    # Build per-role table + wire table from netg-r6b
    def build_role_table(rows, wl, w, rate=None, wire_dir=None):
        cell_rows = [
            r for r in rows
            if r["workload"] == wl and r["workers"] == w
            and (rate is None or r.get("rate") == rate)
        ]
        if not cell_rows:
            return "<p><em>No rows.</em></p>"
        wmap = load_wire_dir(wire_dir) if wire_dir else {}
        out = [
            "<div class='table-wrap'><table><thead><tr>",
            "<th>Arm</th><th>ops/s</th><th>p50 (µs)</th><th>p99 (µs)</th><th>Total Server (kc/tx)</th>",
            "<th>Storage (kc/tx)</th><th>GRV Proxy (kc/tx)</th><th>Commit Proxy (kc/tx)</th>",
            "<th>TLog (kc/tx)</th><th>Stateless/Res (kc/tx)</th>",
            "<th>Client f/write</th><th>Wire: Storage f/pkt</th><th>Wire: GRV f/pkt</th><th>Wire: Commit f/pkt</th>",
            "</tr></thead><tbody>",
        ]
        for a in role_arms:
            rs = [r for r in cell_rows if r["arm"] == a]
            if not rs:
                continue
            ops = fmt_med_spread([r["ops_per_s"] for r in rs], 0)
            p50 = fmt_med_spread([r["p50_us"] for r in rs], 0)
            p99 = fmt_med_spread([r["p99_us"] for r in rs], 0)
            tot = fmt_med_spread([r["server_kcycles"] for r in rs], 1)

            def rcol(rk):
                vals = [r.get("role_cycles_per_tx", {}).get(rk, 0.0) / 1e3 for r in rs if "role_cycles_per_tx" in r]
                return fmt_med_spread(vals, 1) if vals else "-"

            fpw_vals = [r["lab_frames_per_write"] for r in rs if r.get("lab_frames_per_write")]
            fpw = fmt_med_spread(fpw_vals, 1) if fpw_vals else "-"
            wm = wmap.get(a, {})
            ss_w = f"{wm['ss_fps']:.1f}" if wm.get("ss_fps") else "-"
            grv_w = f"{wm['grv_fps']:.1f}" if wm.get("grv_fps") else "-"
            cp_w = f"{wm['cp_fps']:.1f}" if wm.get("cp_fps") else "-"
            out.append(
                f"<tr><td>{html.escape(ARM_SHORT.get(a, a))}</td>"
                f"<td>{ops}</td><td>{p50}</td><td>{p99}</td><td>{tot}</td>"
                f"<td>{rcol('storage')}</td><td>{rcol('grv_proxy')}</td><td>{rcol('commit_proxy')}</td>"
                f"<td>{rcol('transaction')}</td><td>{rcol('stateless')}</td>"
                f"<td>{fpw}</td><td>{ss_w}</td><td>{grv_w}</td><td>{cp_w}</td></tr>"
            )
        out.append("</tbody></table></div>")
        return "\n".join(out)

    body = f"""
<h2>1. How Coalescing Linger Moves Client Latency, Batching &amp; Server Cost</h2>
<div class="callout">
  <strong>What we swept:</strong>
  <ul>
    <li><strong>Fixed linger on <code>D1</code> (<code>FDBGO_NET_DESIGN=1</code>):</strong> <code>0 &micro;s</code> (<code>D-NG</code>), <code>5 &micro;s</code> (<code>D5</code>), <code>10&ndash;20 &micro;s</code> (<code>D</code>, the C++ <code>MIN/MAX_COALESCE_DELAY</code>), <code>50 &micro;s</code> (<code>D50</code>), <code>100 &micro;s</code> (<code>D100</code>), and <code>250 &micro;s</code> (<code>D250</code>).</li>
    <li><strong>Nagle-style adaptive linger (<code>FDBGO_NET_DESIGN=nga</code>):</strong> <code>D-NG-adap20</code> (20 &micro;s window) and <code>D-NG-adap100</code> (100 &micro;s window). If the previous write on this connection was &ge; <em>W</em> ago, the writer flushes immediately (zero delay on an idle connection); if a write just happened within <em>W</em>, the writer yields with <code>runtime.Gosched()</code> (no kernel <code>hrtimer</code> syscall) until <code>lastWrite + W</code>, coalescing bursts while avoiding timer overhead.</li>
    <li><strong>Post-write yield (<code>D-NG+yield</code>, <code>FDBGO_NET_DESIGN=ngy</code>):</strong> calls <code>runtime.Gosched()</code> once after each socket write before checking the queue again, mirroring the commented-out <code>yield(TaskWriteSocket)</code> in <code>FlowTransport.actor.cpp</code>.</li>
  </ul>
</div>

<div class="grid-2">
  {c1}
  {c2}
</div>

<h2>2. Multi-Rate Capped Sweep on <code>netg-r6</code> (25%, 60%, and 90% of <code>L</code>)</h2>
<p>To eliminate the closed-loop throughput confound, each arm was run under Poisson rate caps (<code>-rate</code>) at ~25%, ~60%, and ~90% of <code>L</code>'s closed-loop median throughput on <code>get100/64</code> (8,000 / 19,500 / 28,000 ops/s) and <code>set100/64</code> (4,800 / 11,500 / 17,000 ops/s) across 3 rotated rounds.</p>

<h3>get100 / 64 workers — Rate-Capped at 8,000 ops/s (~25% of L)</h3>
{build_cell_table(hz6_linger_rate, "get100", 64, rate_arms, rate=8000)}

<h3>get100 / 64 workers — Rate-Capped at 19,500 ops/s (~60% of L)</h3>
{build_cell_table(hz6_linger_rate, "get100", 64, rate_arms, rate=19500)}

<h3>get100 / 64 workers — Rate-Capped at 28,000 ops/s (~90% of L)</h3>
{build_cell_table(hz6_linger_rate, "get100", 64, rate_arms, rate=28000)}

<h3>set100 / 64 workers — Rate-Capped at 4,800 ops/s (~25% of L)</h3>
{build_cell_table(hz6_linger_rate, "set100", 64, rate_arms, rate=4800)}

<h3>set100 / 64 workers — Rate-Capped at 11,500 ops/s (~60% of L)</h3>
{build_cell_table(hz6_linger_rate, "set100", 64, rate_arms, rate=11500)}

<h3>set100 / 64 workers — Rate-Capped at 17,000 ops/s (~90% of L)</h3>
{build_cell_table(hz6_linger_rate, "set100", 64, rate_arms, rate=17000)}

<h2>3. Per-Role Server CPU &amp; Wire Frames/Packet (5-Process Split Cluster on <code>netg-r6b</code>)</h2>
<p>On <code>netg-r6b</code>, each FDB server role runs in its own isolated OS process pinned to dedicated CPU cores: <code>stateless</code> (resolver/master/controller, port 4751, CPU 2), <code>grv_proxy</code> (port 4754, CPU 3), <code>commit_proxy</code> (port 4755, CPU 4), <code>transaction</code> (TLog, port 4752, CPU 5), and <code>storage</code> (port 4753, CPUs 6–7). <code>perf stat --per-thread</code> and <code>tcpdump</code> capture exact per-role CPU cycles/tx and client-to-server frames/packet.</p>

<div class="grid-2">
  {c3}
  {c4}
</div>
<div class="grid-2">
  {c5}
  {c6}
</div>

<h3>get100 / 64 workers — Per-Role Server CPU &amp; Wire Batching (Closed-Loop, netg-r6b)</h3>
{build_role_table(hz6b_ladder, "get100", 64, wire_dir="rawhz6b/wire-get100-64")}

<h3>get100 / 256 workers — Per-Role Server CPU &amp; Wire Batching (Closed-Loop, netg-r6b)</h3>
{build_role_table(hz6b_ladder, "get100", 256, wire_dir="rawhz6b/wire-get100-256")}

<h3>set100 / 64 workers — Per-Role Server CPU &amp; Wire Batching (Closed-Loop, netg-r6b)</h3>
{build_role_table(hz6b_ladder, "set100", 64, wire_dir="rawhz6b/wire-set100-64")}

<h3>get100 / 64 workers — Per-Role Server CPU &amp; Wire Batching (Rate-Capped @ 28,000 ops/s, netg-r6b)</h3>
{build_role_table(hz6b_rate, "get100", 64, rate=28000, wire_dir="rawhz6b/wire-get100-64-r28000")}

<h3>set100 / 64 workers — Per-Role Server CPU &amp; Wire Batching (Rate-Capped @ 11,000 ops/s, netg-r6b)</h3>
{build_role_table(hz6b_rate, "set100", 64, rate=11000, wire_dir="rawhz6b/wire-set100-64-r11000")}

<h3>set100 / 64 workers — Per-Role Server CPU &amp; Wire Batching (Rate-Capped @ 17,000 ops/s, netg-r6b)</h3>
{build_role_table(hz6b_rate, "set100", 64, rate=17000, wire_dir="rawhz6b/wire-set100-64-r17000")}

<h2>4. Complete Closed-Loop Linger Sweep on <code>netg-r6</code> (All 9 Cells, 3 Rotated Rounds)</h2>
<h3>get100 / 8 workers</h3>
{build_cell_table(hz6_linger, "get100", 8, linger_arms)}
<h3>get100 / 64 workers</h3>
{build_cell_table(hz6_linger, "get100", 64, linger_arms)}
<h3>get100 / 256 workers</h3>
{build_cell_table(hz6_linger, "get100", 256, linger_arms)}
<h3>set100 / 8 workers</h3>
{build_cell_table(hz6_linger, "set100", 8, linger_arms)}
<h3>set100 / 64 workers</h3>
{build_cell_table(hz6_linger, "set100", 64, linger_arms)}
<h3>set100 / 256 workers</h3>
{build_cell_table(hz6_linger, "set100", 256, linger_arms)}
<h3>getset100 / 8 workers</h3>
{build_cell_table(hz6_linger, "getset100", 8, linger_arms)}
<h3>getset100 / 64 workers</h3>
{build_cell_table(hz6_linger, "getset100", 64, linger_arms)}
<h3>getset100 / 256 workers</h3>
{build_cell_table(hz6_linger, "getset100", 256, linger_arms)}
"""
    with open(os.path.join(REPORT_DIR, "linger.html"), "w") as f:
        f.write(page_shell("linger.html", "Linger Sweep & Per-Role Server Cost", body))


def write_queues(hz6_combined, hz6_rate, hz6_sys, hz6_quick1, hz6_quick2, hz5_ladder, hz6b_ladder):
    q_arms = ["L", "A", "A0", "M", "DNG", "DNGy", "DNGchan", "DNGring"]

    cats = ["get100/64", "get100/256", "set100/64", "set100/256", "getset100/64", "getset100/256"]
    cat_specs = [("get100", 64), ("get100", 256), ("set100", 64), ("set100", 256), ("getset100", 64), ("getset100", 256)]
    series_ops = []
    for a in ["A", "M", "DNG", "DNGchan", "DNGring"]:
        vals = []
        for wl, w in cat_specs:
            rs = [r["ops_per_s"] / 1e3 for r in hz6_combined if r["workload"] == wl and r["workers"] == w and r["arm"] == a]
            vals.append(statistics.median(rs) if rs else 0.0)
        series_ops.append((ARM_SHORT[a], ARM_COLORS[a], vals))

    chart_q_ops = svg_grouped_bars(
        "Writer Queue Throughput Comparison on netg-r6 (kops/s, higher is better)",
        "A (today) vs D-M (sender writes) vs D-NG (mutex+slice) vs D-NG-chan (channel) vs D-NG-ring v2 (MPSC ring + writev)",
        cats, series_ops, "Throughput (kops/s)",
    )

    chart_ring_v1_v2 = svg_grouped_bars(
        "Lock-Free MPSC Ring: v1 (In-Order Predecessor Spin) vs v2 (Per-Slot Ready + writev)",
        "In quick1 (v1), a preempted sender between CAS-reserve and commit stalls all senders behind it at 256 workers",
        ["get100/64 (kops/s)", "get100/256 (kops/s)", "get100/256 CPU (µs/op)", "get100/256 p50 (µs)"],
        [
            ("D-NG (mutex+slice)", "#059669", [64.1, 256.9, 27.0, 933]),
            ("D-NG-ring v1 (predecessor spin)", "#be123c", [54.7, 73.1, 84.8, 3052]),
            ("D-NG-ring v2 (slot-ready + writev)", "#0891b2", [57.7, 239.0, 29.9, 991]),
        ],
        "Value (see category label)",
    )

    body = f"""
<h2>1. Writer Queue Designs Evaluated</h2>
<div class="grid-2">
  <div class="card">
    <h3>Four Queue &amp; Handoff Architectures</h3>
    <ol>
      <li><strong><code>D-NG</code> (<code>FDBGO_NET_DESIGN=ng</code>) — Mutex + Byte Slice Swap:</strong>
        Senders encode into a pooled frame buffer, lock <code>w.mu</code>, append bytes to <code>w.q</code>, and unlock. Only the sender that transitions <code>len(w.q)</code> from <code>0 &rarr; &gt;0</code> signals the 1-buffered <code>w.sig</code> channel (mirroring C++ <code>Peer::send</code>'s <code>firstUnsent</code> check). The writer swaps <code>w.q</code> with a spare buffer under <code>w.mu</code> and issues a single <code>conn.Write(buf)</code>.</li>
      <li><strong><code>D-NG-chan</code> (<code>FDBGO_NET_DESIGN=ngc</code>) — Buffered Channel Writer:</strong>
        Senders push <code>chanFrame{{bufp, frame}}</code> onto a buffered channel (<code>cap=4096</code>). <code>chanLoop</code> blocks on the first frame, drains <code>len(ch)</code> additional frames into a contiguous buffer, returns the pooled frame buffers, and calls <code>conn.Write(buf)</code>.</li>
      <li><strong><code>D-NG-ring</code> (<code>FDBGO_NET_DESIGN=ngr</code>) — Lock-Free MPSC Byte Ring + <code>writev</code>:</strong>
        A 1 MiB power-of-two ring with 4,096 slots. One 64-bit CAS on <code>reserve</code> claims <code>seq16&lt;&lt;48 | pos48</code> simultaneously. Senders encode directly into the ring slice in place and store <code>slot.ready = seq + 1</code>. <code>ringLoop</code> scans consecutive ready slots and writes them with a single <code>writev(2)</code> via <code>net.Buffers.WriteTo</code>.</li>
      <li><strong><code>D-M</code> / <code>M</code> (<code>FDBGO_NET_DESIGN=m</code>) — Sender-Writes (No Writer Goroutine):</strong>
        A sender that wins <code>wmu.TryLock()</code> writes its own frame directly on its caller goroutine, then drains any frames other senders appended to <code>pending</code> while the write was in flight.</li>
    </ol>
  </div>
  <div class="card">
    <h3>Why Lock-Free Ring v1 Collapsed at 73k ops/s — and Why v2 Still Doesn't Beat a Mutex</h3>
    <ul>
      <li><strong>v1 failure mode (in-order commit spin):</strong> Our first Disruptor-style MPSC ring reserved a byte span with <code>atomic.AddUint64</code> and required each sender to spin on <code>committed.Load() == start</code> before advancing <code>committed = end</code>. At 256 workers across 8 Ps (32 goroutines/P), if the OS or Go scheduler preempted a sender in the ~40 ns window between reservation and commit, <em>every subsequent sender on that connection spun in <code>runtime.procyieldAsm</code></em>. Throughput on <code>get100/256</code> collapsed from <strong>256.9k to 73.1k ops/s</strong>, client CPU tripled from <strong>27.0 to 84.8 &micro;s/op</strong>, and <code>cpu.pprof</code> showed 25.7s (12.7%) in <code>procyieldAsm</code> + 41.2s (20.3%) in <code>runtime.lock2</code>.</li>
      <li><strong>v2 fix (per-slot ready + <code>writev</code>):</strong> Packing <code>seq16&lt;&lt;48 | pos48</code> into one CAS and publishing per-slot <code>ready</code> removed all sender-to-sender waiting. Throughput jumped from <strong>73.1k to 243.0k ops/s</strong> (29.3 &micro;s/op), and raw <code>write</code> syscalls dropped to <code>0.01/op</code> (replaced by <code>writev</code>).</li>
      <li><strong>Why <code>D-NG</code> (257.7k ops/s, 27.0 &micro;s/op) still beats both:</strong> As the <code>pprof</code> mutex profile below proves, the send-queue mutex <code>w.mu</code> in <code>naSend</code> + <code>naLoop</code> accounts for only <strong>0.16% of CPU time</strong> and <strong>1.20% of lock contention</strong>! Removing a 40 ns uncontended lock around a slice append saves nothing, while ring slot management and <code>writev</code> iovec construction cost slightly more CPU than one contiguous <code>Write</code>.</li>
    </ul>
  </div>
</div>

<div class="grid-2">
  {chart_q_ops}
  {chart_ring_v1_v2}
</div>

<h2>2. Deep Dive on <code>D-M</code> (<code>M</code>): Speed vs Socket Backpressure Failure (T1/T2)</h2>
<div class="callout warn">
  <strong>Why <code>D-M</code> was built and tested:</strong> <code>D-M</code> removes the per-connection writer goroutine completely so we can measure the exact cost of having a dedicated writer goroutine vs writing on the caller's goroutine under a lock.
</div>
<div class="grid-2">
  <div class="card">
    <h3>Happy-Path Performance (When Sockets Never Fill)</h3>
    <ul>
      <li>On loopback when the kernel socket send buffer never fills, <code>write(2)</code> is a fast copy into kernel memory and never blocks.</li>
      <li>On Box 2 (<code>rawhz5</code>), <code>D-M</code> achieved <code>260.8k ops/s</code> on <code>get100/256</code> (21.3 &micro;s/op) and <code>48.5k ops/s</code> on <code>get100/64</code> (<code>55.6k ops/s</code> at <code>GOMAXPROCS=4</code>, halving client CPU from 47.5 to 24.1 &micro;s/op).</li>
      <li>On <code>netg-r6</code> (<code>rawhz6</code>, where <code>D-M</code> and <code>D-NG</code> ran side-by-side in the same rotated rounds), <strong><code>D-NG</code> matches or slightly leads <code>D-M</code></strong> across the board, proving that <strong>the dedicated writer goroutine in <code>D-NG</code> costs nothing in throughput or latency</strong> once the write-ack rendezvous and <code>hrtimer</code> are removed.</li>
    </ul>
  </div>
  <div class="card">
    <h3>Why <code>D-M</code> Fails T1 and T2 Under Backpressure</h3>
    <ul>
      <li><strong>Violates T2 (<code>ctx</code> is local):</strong> When a TCP socket send buffer fills (slow peer, network partition, or large commit burst), the caller goroutine that won <code>wmu.TryLock()</code> blocks inside the kernel <code>write(2)</code> syscall writing both its own frame and every frame queued behind it. Its <code>ctx</code> cancellation cannot interrupt the blocking write unless the whole connection deadline is poisoned.</li>
      <li><strong>Violates T1 (no write acknowledgment):</strong> The lock holder cannot return to its caller until the kernel accepts the entire drained queue — turning enqueue into a synchronous write barrier for the whole batch.</li>
      <li><strong>10 deterministic <code>-race</code> failures in <code>pkg/fdbgo/client</code> (<code>race6-m-*</code>):</strong>
        <ul>
          <li><code>TestNativeCommit_ReplyBeforeWriteAcknowledgment</code> (4 subtests): commit reply arrives before the committing sender returns from its blocked write.</li>
          <li><code>TestNativeCommit_WriteBackpressureOwnerRetirement</code> (4 subtests): native actor cannot exit while blocked in gated socket writer.</li>
          <li><code>TestFallbackStartsEndAtTransportSuspension/flush</code> &amp; <code>TestFallbackReadyBeginStartsEndBeforeConsumingError</code>: warm fallback request is serialized behind a blocked flush.</li>
        </ul>
      </li>
      <li><strong>Why curing <code>D-M</code> recreates <code>A</code>:</strong> Using non-blocking <code>write(2)</code> and handing unwritten <code>EAGAIN</code> tails to a background goroutine is literally today's <code>rawWriter</code> + <code>writeLoop</code> tail-handoff machinery in <code>A</code>.</li>
    </ul>
  </div>
</div>

<h2>3. Where Lock Contention &amp; CPU Actually Go (<code>pprof</code> CPU, Mutex &amp; Block Profiles)</h2>
<p>Profiles captured on <code>netg-r6</code> during <code>get100/256</code> (4.8M operations) and <code>get100/64</code> (1.2M operations) across <code>D-NG</code> (<code>ng</code>), <code>D-NG-chan</code> (<code>ngc</code>), and <code>D-NG-ring</code> (<code>ngr</code> v1 and v2).</p>

<div class="grid-2">
  <div class="card">
    <h3>Mutex Contention Breakdown (<code>get100/256</code>, <code>D-NG</code>, 17.27s total delay over 130.0s CPU)</h3>
    <div class="table-wrap">
      <table>
        <thead>
          <tr>
            <th>Call Site / Lock</th>
            <th>Role</th>
            <th>Mutex Delay (s)</th>
            <th>Share of Mutex Delay</th>
            <th>Share of Total CPU (130s)</th>
          </tr>
        </thead>
        <tbody>
          <tr class="highlight-warn">
            <td><code>Conn.PrepareReplyTo</code> (<code>pendingMu.Lock</code>)</td>
            <td class="text-left">Register request token in reply map before send</td>
            <td><strong>8.65 s</strong></td>
            <td><strong>50.07%</strong></td>
            <td>6.65%</td>
          </tr>
          <tr class="highlight-warn">
            <td><code>Conn.readLoop</code> (<code>pendingMu.Lock</code>)</td>
            <td class="text-left">Lookup &amp; delete request token on reply arrival</td>
            <td><strong>4.45 s</strong></td>
            <td><strong>25.77%</strong></td>
            <td>3.42%</td>
          </tr>
          <tr>
            <td><code>Transaction.ensureReadVersionFlags</code></td>
            <td class="text-left">GRV batch / transaction read-version state lock</td>
            <td>2.36 s</td>
            <td>13.68%</td>
            <td>1.82%</td>
          </tr>
          <tr>
            <td><code>ClientMetrics.observeReadLatency</code></td>
            <td class="text-left">Client metrics histogram lock</td>
            <td>0.98 s</td>
            <td>5.65%</td>
            <td>0.75%</td>
          </tr>
          <tr>
            <td><code>database.getOrDialForSend</code></td>
            <td class="text-left">Connection pool lookup</td>
            <td>0.20 s</td>
            <td>1.14%</td>
            <td>0.15%</td>
          </tr>
          <tr class="highlight">
            <td><strong><code>Conn.naSend</code> + <code>Conn.naLoop</code> (<code>w.mu</code>)</strong></td>
            <td class="text-left"><strong>D-NG send-queue mutex (append + slice swap)</strong></td>
            <td><strong>0.207 s</strong> (0.188 + 0.019)</td>
            <td><strong>1.20%</strong></td>
            <td><strong>0.16%</strong></td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
  <div class="card">
    <h3>CPU &amp; Block Profile Comparison Across Queues (<code>get100/256</code>)</h3>
    <div class="table-wrap">
      <table>
        <thead>
          <tr>
            <th>Metric / Symbol</th>
            <th><code>D-NG</code> (mutex+slice)</th>
            <th><code>D-NG-chan</code> (channel)</th>
            <th><code>D-NG-ring</code> v1 (spin)</th>
            <th><code>D-NG-ring</code> v2 (slot-ready)</th>
          </tr>
        </thead>
        <tbody>
          <tr>
            <td>Wall Duration / Total CPU</td>
            <td>18.38s / 129.99s</td>
            <td>19.64s / 142.94s</td>
            <td>28.78s / 202.78s</td>
            <td>19.91s / 141.44s</td>
          </tr>
          <tr>
            <td><code>syscall.Syscall6</code> (flat CPU)</td>
            <td>18.05s (13.89%)</td>
            <td>18.30s (12.80%)</td>
            <td>23.29s (11.49%)</td>
            <td>18.71s (13.23%)</td>
          </tr>
          <tr>
            <td><code>runtime.lock2</code> (cum CPU)</td>
            <td>4.56s (3.51%)</td>
            <td>6.15s (4.30%)</td>
            <td><strong>41.19s (20.31%)</strong></td>
            <td>5.90s (4.17%)</td>
          </tr>
          <tr>
            <td><code>runtime.procyieldAsm</code> (flat CPU)</td>
            <td>&lt;1.3s (&lt;1.0%)</td>
            <td>2.23s (1.56%)</td>
            <td><strong>25.71s (12.68%)</strong></td>
            <td>1.97s (1.39%)</td>
          </tr>
          <tr>
            <td>Send function cum CPU (<code>naSend</code>/<code>chanSend</code>/<code>ringSend</code>)</td>
            <td><strong>1.76s (1.35%)</strong></td>
            <td>3.71s (2.60%)</td>
            <td>4.06s (2.00%)</td>
            <td>2.08s (1.47%)</td>
          </tr>
          <tr>
            <td>Writer loop cum CPU (<code>naLoop</code>/<code>chanLoop</code>/<code>ringLoop</code>)</td>
            <td>15.09s (11.61%)</td>
            <td>16.64s (11.64%)</td>
            <td>21.08s (10.40%)</td>
            <td>15.92s (11.26%)</td>
          </tr>
          <tr>
            <td><code>PrepareReplyTo</code> + <code>readLoop</code> share of mutex wait</td>
            <td><strong>75.84%</strong> (50.1% + 25.8%)</td>
            <td><strong>77.61%</strong> (49.9% + 27.7%)</td>
            <td><strong>68.02%</strong> (44.4% + 23.6%)</td>
            <td>~75%</td>
          </tr>
          <tr>
            <td>Block profile: <code>runtime.selectgo</code> (waiting on reply)</td>
            <td>98.55%</td>
            <td>98.59%</td>
            <td>98.96%</td>
            <td>98.9%</td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</div>

<h2>4. Queue &amp; Sender-Write Benchmark Tables on <code>netg-r6</code> (All 9 Closed-Loop Cells)</h2>
<h3>get100 — 8, 64, and 256 workers</h3>
{build_cell_table(hz6_combined, "get100", 8, q_arms)}
{build_cell_table(hz6_combined, "get100", 64, q_arms)}
{build_cell_table(hz6_combined, "get100", 256, q_arms)}

<h3>set100 — 8, 64, and 256 workers</h3>
{build_cell_table(hz6_combined, "set100", 8, q_arms)}
{build_cell_table(hz6_combined, "set100", 64, q_arms)}
{build_cell_table(hz6_combined, "set100", 256, q_arms)}

<h3>getset100 — 8, 64, and 256 workers</h3>
{build_cell_table(hz6_combined, "getset100", 8, q_arms)}
{build_cell_table(hz6_combined, "getset100", 64, q_arms)}
{build_cell_table(hz6_combined, "getset100", 256, q_arms)}
"""
    with open(os.path.join(REPORT_DIR, "queues.html"), "w") as f:
        f.write(page_shell("queues.html", "Writer Queues, Lock-Free Ring & D-M", body))


def write_delay_vs_ack(hz6_combined, hz6_rate, hz6_sys, hz6_linger):
    da_arms = ["L", "A", "A0", "D", "DNG", "M", "DNGy", "DNGa20", "D5"]

    cats = ["get100/8", "get100/64", "get100/256", "set100/8", "set100/64", "set100/256", "getset100/64"]
    cat_specs = [("get100", 8), ("get100", 64), ("get100", 256), ("set100", 8), ("set100", 64), ("set100", 256), ("getset100", 64)]
    series_p50 = []
    for a in ["A", "D", "A0", "DNG", "M"]:
        vals = []
        for wl, w in cat_specs:
            rs = [r["p50_us"] for r in hz6_combined if r["workload"] == wl and r["workers"] == w and r["arm"] == a]
            vals.append(statistics.median(rs) if rs else 0.0)
        series_p50.append((ARM_SHORT[a], ARM_COLORS[a], vals))

    chart_p50 = svg_grouped_bars(
        "Separating the 10–20 µs Delay (A vs A0, D vs D-NG) from Write-Ack (A vs D, A0 vs D-NG): p50 Latency (µs)",
        "Lower is better. Delay dominates at 8 and 64 workers; removing write-ack wins at 256 workers and in client CPU/futexes.",
        cats, series_p50, "p50 Latency (µs)",
    )

    series_cpu = []
    for a in ["A", "D", "A0", "DNG", "M"]:
        vals = []
        for wl, w in cat_specs:
            rs = [r["client_cpu_us_per_op"] for r in hz6_combined if r["workload"] == wl and r["workers"] == w and r["arm"] == a]
            vals.append(statistics.median(rs) if rs else 0.0)
        series_cpu.append((ARM_SHORT[a], ARM_COLORS[a], vals))

    chart_cpu = svg_grouped_bars(
        "Client CPU per Operation (µs/op, lower is better): A vs D vs A0 vs D-NG vs D-M",
        "Removing the write-acknowledgment wait and writer-turn machinery (D, D-NG, D-M) cuts client CPU across every cell.",
        cats, series_cpu, "Client CPU (µs/op)",
    )

    # Build syscalls table
    sys_keys = [
        ("sys_enter_per_op", "Total syscalls/op"),
        ("futex_per_op", "futex/op"),
        ("epoll_pwait_per_op", "epoll_pwait/op"),
        ("read_per_op", "read/op"),
        ("write_per_op", "write/op"),
        ("nanosleep_per_op", "nanosleep/op"),
        ("context-switches_per_op", "ctx-switches/op"),
    ]
    sys_rows_html = []
    for (wl, w) in sorted({(r["workload"], r["workers"]) for r in hz6_sys}):
        for a in ["A", "A0", "D", "D5", "D50", "DNG", "M", "DNGy", "DNGa20", "DNGa100", "DNGchan", "DNGring"]:
            rs = [r for r in hz6_sys if r["workload"] == wl and r["workers"] == w and r["arm"] == a]
            if not rs:
                continue
            cols = "".join(f"<td>{fmt_med_spread([r.get(k, 0.0) for r in rs], 2)}</td>" for k, _ in sys_keys)
            sys_rows_html.append(f"<tr><td>{wl}/{w}</td><td>{html.escape(ARM_SHORT.get(a, a))}</td>{cols}</tr>")

    body = f"""
<h2>1. Isolating the Two Orthogonal Changes Between Today (<code>A</code>) and <code>D-NG</code></h2>
<p>Today's Go transport (<code>A</code>) differs from <code>D-NG</code> in two orthogonal ways:</p>
<ol>
  <li><strong>The 10–20 &micro;s coalescing delay:</strong> <code>A</code> and <code>D</code> wait <code>max(10 &micro;s, 20 &micro;s - sinceLastWrite)</code> (via Linux <code>hrtimer</code>), whereas <code>A0</code> (<code>-coalesce-delay=0,0</code>) and <code>D-NG</code> flush as soon as a frame is available.</li>
  <li><strong>The write-acknowledgment wait &amp; writer-turn machinery:</strong> In <code>A</code> and <code>A0</code>, every caller either takes a direct-write / writer-turn lock or sends a <code>writeReq</code> with an acknowledgment channel and waits for the socket write to complete before waiting for the reply. In <code>D</code> and <code>D-NG</code> (<code>D1</code>), callers append to a byte slice under a short mutex, signal the writer only on the <code>0 &rarr; 1</code> transition, and immediately wait on the reply channel.</li>
</ol>

<div class="grid-2">
  {chart_p50}
  {chart_cpu}
</div>

<div class="callout good">
  <strong>What the 2&times;2 Factorial (<code>A</code>, <code>A0</code>, <code>D</code>, <code>D-NG</code>) Proves:</strong>
  <ul>
    <li><strong>Latency at 8 &amp; 64 workers is governed by the 10–20 &micro;s delay:</strong> Removing the delay from today's transport (<code>A &rarr; A0</code>) jumps <code>get100/64</code> from 48.5k to 100.0k ops/s (p50 <code>1,262 &rarr; 618 &micro;s</code>) because each <code>get100</code> transaction performs sequential hops (GRV + read waves), paying the 10–20 &micro;s timer wait on every low-queue hop.</li>
    <li><strong>Throughput at 256 workers and client CPU/futex efficiency across all cells are governed by removing write-ack (<code>A0 &rarr; D-NG</code>):</strong>
      <ul>
        <li>On <code>get100/256</code>, <code>D-NG</code> reaches <strong>257.7k ops/s</strong> at <strong>27.0 &micro;s/op</strong> client CPU and <strong>0.06 futex/op</strong>, beating <code>A0</code> (225.7k ops/s, 32.2 &micro;s/op, 0.24 futex/op) and <code>A</code> (212.1k ops/s, 33.0 &micro;s/op, 0.31 futex/op).</li>
        <li>On <code>get100/8</code>, <code>D-NG</code> uses <strong>140.5 &micro;s/op</strong> client CPU vs <code>A0</code>'s <strong>158.7 &micro;s/op</strong> and <code>A</code>'s <strong>179.0 &micro;s/op</strong> (-21.5% client CPU).</li>
        <li>Why does <code>A0</code> beat <code>D-NG</code> specifically on closed-loop <code>get100/64</code> (100k vs 52k ops/s), while <code>D-NG+yield</code> / <code>D-NG-adap20</code> jump to 77k–80k ops/s? Because in <code>A0</code>, an idle connection writes directly on the sender's goroutine (0 goroutine hop) when no write is in flight, whereas in <code>D-NG</code> at intermediate concurrency (64 workers) the writer goroutine wakes on frame 1, writes 1–4 frames immediately, and goes back to sleep just as the next sender arrives; adding a single <code>runtime.Gosched()</code> (<code>D-NG+yield</code>) or 20 &micro;s adaptive linger (<code>DNGa20</code>) keeps the writer hot across the burst and jumps throughput to 77k–80k ops/s while cutting client CPU to 39.1 &micro;s/op (lower than <code>A0</code>'s 42.1 &micro;s/op)!</li>
      </ul>
    </li>
  </ul>
</div>

<h2>2. Client Syscalls &amp; Futexes per Operation (<code>perf stat</code> on <code>netg-r6</code>)</h2>
<div class="table-wrap">
  <table>
    <thead>
      <tr>
        <th>Cell</th><th>Arm</th><th>Total syscalls/op</th><th>futex/op</th><th>epoll_pwait/op</th>
        <th>read/op</th><th>write/op</th><th>nanosleep/op</th><th>ctx-switches/op</th>
      </tr>
    </thead>
    <tbody>
      {"".join(sys_rows_html)}
    </tbody>
  </table>
</div>

<h2>3. Historical &amp; Source Evidence on C++ <code>MIN_COALESCE_DELAY</code> / <code>MAX_COALESCE_DELAY</code></h2>
<div class="card">
  <ul>
    <li>In FoundationDB 7.3.77 (<code>fdbrpc/FlowTransport.actor.cpp:704-738</code>), <code>connectionWriter</code> is unchanged since the initial open-source commit <code>a674cb4ef4</code> (2017):
<pre>// Wait a short time for more data to accumulate
double delay = std::max(FLOW_KNOBS-&gt;MIN_COALESCE_DELAY,
                        FLOW_KNOBS-&gt;MAX_COALESCE_DELAY - (now() - lastWriteTime));
wait(delayJittered(delay, TaskWriteSocket));
// wait(delay(0, TaskWriteSocket));
// wait(delay(500e-6, TaskWriteSocket));
// wait(yield(TaskWriteSocket));</pre>
    </li>
    <li>The three commented-out lines right below <code>delayJittered</code> (<code>delay(0)</code>, <code>delay(500e-6)</code>, <code>yield(TaskWriteSocket)</code>) are the exact alternatives we benchmarked in Round 6 (<code>D-NG</code> = 0, <code>D250</code> = 250 &micro;s, <code>D-NG+yield</code> = <code>runtime.Gosched()</code>).</li>
    <li>In <code>flow/Knobs.cpp:211-212</code>, simulation runs with <code>if (randomize &amp;&amp; BUGGIFY) {{ MIN_COALESCE_DELAY = 0; MAX_COALESCE_DELAY = 0; }}</code>, confirming that 0 coalescing delay is a valid, simulation-tested operating mode.</li>
  </ul>
</div>

<h2>4. Full 9-Cell Comparison: <code>A</code> vs <code>A0</code> vs <code>D</code> vs <code>D-NG</code> vs <code>D-M</code> (<code>netg-r6</code>)</h2>
<h3>get100 — 8, 64, and 256 workers</h3>
{build_cell_table(hz6_combined, "get100", 8, da_arms)}
{build_cell_table(hz6_combined, "get100", 64, da_arms)}
{build_cell_table(hz6_combined, "get100", 256, da_arms)}

<h3>set100 — 8, 64, and 256 workers</h3>
{build_cell_table(hz6_combined, "set100", 8, da_arms)}
{build_cell_table(hz6_combined, "set100", 64, da_arms)}
{build_cell_table(hz6_combined, "set100", 256, da_arms)}

<h3>getset100 — 8, 64, and 256 workers</h3>
{build_cell_table(hz6_combined, "getset100", 8, da_arms)}
{build_cell_table(hz6_combined, "getset100", 64, da_arms)}
{build_cell_table(hz6_combined, "getset100", 256, da_arms)}
"""
    with open(os.path.join(REPORT_DIR, "delay-vs-ack.html"), "w") as f:
        f.write(page_shell("delay-vs-ack.html", "Coalesce Delay vs Write-Ack", body))


def write_libfdbc(hz5_ladder, hz5_rate, hz5_sys, hz4_ladder, hz6_combined):
    hz5_arms = ["L", "L4", "A", "D", "M", "M4P"]
    hz4_arms = ["L", "A", "D", "DNG", "D4P"]

    cats = ["get100/8", "get100/64", "get100/256", "set100/8", "set100/64", "set100/256", "getset100/64", "getset100/256"]
    cat_specs = [("get100", 8), ("get100", 64), ("get100", 256), ("set100", 8), ("set100", 64), ("set100", 256), ("getset100", 64), ("getset100", 256)]
    series_srv = []
    for a in ["L", "L4", "A", "D", "M"]:
        vals = []
        for wl, w in cat_specs:
            rs = [r["server_kcycles"] for r in hz5_ladder if r["workload"] == wl and r["workers"] == w and r["arm"] == a]
            vals.append(statistics.median(rs) if rs else 0.0)
        series_srv.append((ARM_SHORT[a], ARM_COLORS[a], vals))

    chart_l4_srv = svg_grouped_bars(
        "Server CPU per Transaction (kcycles/tx): L vs L4 (4 Threads / 4 DBs) vs Go (Box 2 / rawhz5)",
        "L4 splits batching across 4 independent connection pools, multiplying server CPU 1.14–3.58× over L.",
        cats, series_srv, "Server CPU (kcycles/tx)",
    )

    series_ops = []
    for a in ["L", "L4", "A", "D", "M"]:
        vals = []
        for wl, w in cat_specs:
            rs = [r["ops_per_s"] / 1e3 for r in hz5_ladder if r["workload"] == wl and r["workers"] == w and r["arm"] == a]
            vals.append(statistics.median(rs) if rs else 0.0)
        series_ops.append((ARM_SHORT[a], ARM_COLORS[a], vals))

    chart_l4_ops = svg_grouped_bars(
        "Closed-Loop Throughput (kops/s): L vs L4 vs Go (Box 2 / rawhz5)",
        "L saturates one thread at 57k ops/s; L4 saturates at 107k ops/s; Go reaches 229k–261k ops/s.",
        cats, series_ops, "Throughput (kops/s)",
    )

    # Syscalls table from rawhz5
    sys_keys = [
        ("sys_enter_per_op", "Total syscalls/op"),
        ("futex_per_op", "futex/op"),
        ("epoll_pwait_per_op", "epoll_pwait/op"),
        ("read_per_op", "read/op"),
        ("write_per_op", "write/op"),
        ("nanosleep_per_op", "nanosleep/op"),
        ("context-switches_per_op", "ctx-switches/op"),
    ]
    sys5_html = []
    for (wl, w) in sorted({(r["workload"], r["workers"]) for r in hz5_sys}):
        for a in ["L", "L4", "A", "D", "M"]:
            rs = [r for r in hz5_sys if r["workload"] == wl and r["workers"] == w and r["arm"] == a]
            if not rs:
                continue
            cols = "".join(f"<td>{fmt_med_spread([r.get(k, 0.0) for r in rs], 2)}</td>" for k, _ in sys_keys)
            sys5_html.append(f"<tr><td>{wl}/{w}</td><td>{html.escape(ARM_SHORT.get(a, a))}</td>{cols}</tr>")

    body = f"""
<h2>1. Why <code>L</code> (1 Thread) and <code>L4</code> (4 Threads, 4 DBs) Behave So Differently from Go</h2>
<div class="grid-2">
  <div class="card">
    <h3><code>L</code>: Single-Thread Reactor Bottleneck &amp; Implicit Coalescing</h3>
    <ul>
      <li><code>libfdb_c</code> runs the entire C++ client stack (<code>NativeAPI</code>, <code>ReadYourWrites</code>, <code>loadBalance</code>, <code>FlowTransport</code>) on a single OS network thread using cooperative Flow actors.</li>
      <li>When 64 or 256 Go worker goroutines submit requests via cgo, every call crosses a cgo thread-safe promise queue onto that single network thread.</li>
      <li>At 256 workers on <code>get100</code>, the single <code>libfdb_c</code> network thread pins at 100% CPU (~50k–57k ops/s = 5.0M–5.7M read RPCs/s). Because requests queue behind the saturated reactor loop, each <code>connectionWriter</code> drain sweeps up <strong>31.8–64.5 storage frames per TCP packet</strong>, minimizing server packet-processing overhead at the cost of a 4.5× throughput ceiling and 4.6 ms p50 latency.</li>
    </ul>
  </div>
  <div class="card">
    <h3><code>L4</code>: Multi-Thread <code>libfdb_c</code> Splits Batching Across 4 Connection Pools</h3>
    <ul>
      <li>Setting <code>client_threads_per_version=4</code> (<code>MultiVersionTransaction.actor.cpp:3095-3097</code>) copies <code>libfdb_c.so</code> into 4 client network threads and assigns <code>FDBDatabase</code> handles round-robin (<code>:3302-3305</code>).</li>
      <li>Because each thread owns an independent <code>FlowTransport</code> and independent <code>DatabaseContext</code>, <strong>GRV batching, commit batching, and TCP socket coalescing are split 4 ways</strong> (7–8 client TCP connections instead of 2):
        <ul>
          <li>Storage frames/packet on <code>get100/64</code> drop from <strong>32.1 (<code>L</code>) to 4.2 (<code>L4</code>)</strong>.</li>
          <li><strong>Server CPU per transaction jumps to 1.14–3.58&times; <code>L</code></strong> (e.g. <code>get100/8</code>: 296.0 vs 82.4 kc/tx; <code>get100/64</code>: 62.9 vs 22.8 kc/tx; rate-capped <code>set100/64</code>: 187.8 vs 137.0 kc/tx).</li>
          <li><strong>Client CPU per op jumps to 1.15–2.73&times; <code>L</code></strong>, and client syscalls jump <strong>3.4–4.2&times;</strong> (12.4–17.4 syscalls/op vs 2.9–5.2).</li>
          <li>Even with 4 threads, <code>L4</code> still reaches only <strong>107k ops/s</strong> on <code>get100/256</code> — less than half of Go's <strong>229k–261k ops/s</strong>, which shares a single connection pool and a single GRV batcher across all 8 Ps!</li>
        </ul>
      </li>
    </ul>
  </div>
</div>

<div class="grid-2">
  {chart_l4_ops}
  {chart_l4_srv}
</div>

<h2>2. Low-Concurrency (8-Worker) Latency Analysis</h2>
<div class="card">
  <p>On <code>set100/8</code> and <code>getset100/8</code>, Go (<code>D-NG</code> and <code>D-M</code>) already beats <code>L</code> on both throughput and latency (<code>set100/8</code> p50: <code>D-NG</code> 2,801 &micro;s vs <code>L</code> 3,127 &micro;s; <code>getset100/8</code> p50: <code>D-NG</code> 3,675 &micro;s vs <code>L</code> 4,409 &micro;s). The only cell where <code>L</code> has lower p50 than <code>D-NG</code> is <code>get100/8</code> (<code>L</code> 1,219 &micro;s vs <code>D-NG</code> 1,346 &micro;s on <code>netg-r6</code>; <code>D-M 4Ps</code> reaches 1,145 &micro;s vs <code>L</code> 1,223 &micro;s on Box 2).</p>
  <p>Why? In <code>get100/8</code>, only 8 goroutines are active across 8 Ps (`client_cores_busy = 0.84`). Each transaction performs 1 GRV + 100 reads. With <code>D-NG</code>, the gap to <code>L</code> shrank from <code>+283 &micro;s</code> (<code>A</code>: 1,502 &micro;s) to <code>+127 &micro;s</code> (<code>D-NG</code>: 1,346 &micro;s) — i.e., <strong>1.27 &micro;s per read</strong>. That residual 1.27 &micro;s/read is the cost of waking a parked Go worker goroutine across Ps when `readLoop` delivers a reply plus arming/stopping the 5 s per-read timer in `GetPipelined` (which `D-M` at `GOMAXPROCS=4` eliminates by keeping goroutines on fewer Ps, beating `L` at 1,145 &micro;s vs 1,223 &micro;s).</p>
</div>

<h2>3. Client Syscalls per Operation: <code>L</code> vs <code>L4</code> vs Go (Box 2 / <code>rawhz5</code>)</h2>
<div class="table-wrap">
  <table>
    <thead>
      <tr>
        <th>Cell</th><th>Arm</th><th>Total syscalls/op</th><th>futex/op</th><th>epoll_pwait/op</th>
        <th>read/op</th><th>write/op</th><th>nanosleep/op</th><th>ctx-switches/op</th>
      </tr>
    </thead>
    <tbody>
      {"".join(sys5_html)}
    </tbody>
  </table>
</div>

<h2>4. Box 2 (<code>rawhz5</code>) Complete Tables: <code>L</code>, <code>L4</code>, <code>A</code>, <code>D</code>, <code>D-M</code>, <code>D-M 4Ps</code></h2>
<h3>get100 — 8, 64, and 256 workers</h3>
{build_cell_table(hz5_ladder, "get100", 8, hz5_arms, wire_map=load_wire_dir("rawhz5/wire-get100-8"))}
{build_cell_table(hz5_ladder, "get100", 64, hz5_arms, wire_map=load_wire_dir("rawhz5/wire-get100-64"))}
{build_cell_table(hz5_ladder, "get100", 256, hz5_arms, wire_map=load_wire_dir("rawhz5/wire-get100-256"))}

<h3>set100 — 8, 64, and 256 workers + Rate-Capped (18,000 ops/s)</h3>
{build_cell_table(hz5_ladder, "set100", 8, hz5_arms, wire_map=load_wire_dir("rawhz5/wire-set100-8"))}
{build_cell_table(hz5_ladder, "set100", 64, hz5_arms, wire_map=load_wire_dir("rawhz5/wire-set100-64"))}
{build_cell_table(hz5_ladder, "set100", 256, hz5_arms, wire_map=load_wire_dir("rawhz5/wire-set100-256"))}
<h4>set100 / 64 workers — Rate-Capped at 18,000 ops/s (Box 2 / rawhz5)</h4>
{build_cell_table(hz5_rate, "set100", 64, hz5_arms)}

<h3>getset100 — 8, 64, and 256 workers</h3>
{build_cell_table(hz5_ladder, "getset100", 8, hz5_arms, wire_map=load_wire_dir("rawhz5/wire-getset100-8"))}
{build_cell_table(hz5_ladder, "getset100", 64, hz5_arms, wire_map=load_wire_dir("rawhz5/wire-getset100-64"))}
{build_cell_table(hz5_ladder, "getset100", 256, hz5_arms, wire_map=load_wire_dir("rawhz5/wire-getset100-256"))}

<h2>5. Box 1 (<code>rawhz4</code>) Complete Tables: <code>L</code>, <code>A</code>, <code>D</code>, <code>D-NG</code>, <code>D 4Ps</code></h2>
<h3>get100 — 8, 64, and 256 workers</h3>
{build_cell_table(hz4_ladder, "get100", 8, hz4_arms, wire_map=load_wire_dir("rawhz4/wire-get100-8"))}
{build_cell_table(hz4_ladder, "get100", 64, hz4_arms, wire_map=load_wire_dir("rawhz4/wire-get100-64"))}
{build_cell_table(hz4_ladder, "get100", 256, hz4_arms, wire_map=load_wire_dir("rawhz4/wire-get100-256"))}

<h3>set100 — 8, 64, and 256 workers</h3>
{build_cell_table(hz4_ladder, "set100", 8, hz4_arms, wire_map=load_wire_dir("rawhz4/wire-set100-8"))}
{build_cell_table(hz4_ladder, "set100", 64, hz4_arms, wire_map=load_wire_dir("rawhz4/wire-set100-64"))}
{build_cell_table(hz4_ladder, "set100", 256, hz4_arms, wire_map=load_wire_dir("rawhz4/wire-set100-256"))}

<h3>getset100 — 8, 64, and 256 workers</h3>
{build_cell_table(hz4_ladder, "getset100", 8, hz4_arms, wire_map=load_wire_dir("rawhz4/wire-getset100-8"))}
{build_cell_table(hz4_ladder, "getset100", 64, hz4_arms, wire_map=load_wire_dir("rawhz4/wire-getset100-64"))}
{build_cell_table(hz4_ladder, "getset100", 256, hz4_arms, wire_map=load_wire_dir("rawhz4/wire-getset100-256"))}
"""
    with open(os.path.join(REPORT_DIR, "libfdbc.html"), "w") as f:
        f.write(page_shell("libfdbc.html", "libfdb_c (L vs L4) vs Go", body))


def write_methodology(hz4_ladder, hz4_cross, hz5_ladder, hz6_ladder, hz6_linger, hz6b_ladder):
    w_one4 = load_wire_dir("rawhz4/wire-set100-1")
    w_one5 = load_wire_dir("rawhz5/wire-set100-1")

    one_rows = []
    for box_lbl, wm in [("Box 1 (rawhz4)", w_one4), ("Box 2 (rawhz5)", w_one5)]:
        for a in ["L", "L4", "A", "D", "DNG", "M"]:
            if a in wm:
                v = wm[a]
                one_rows.append(
                    f"<tr><td>{box_lbl}</td><td>{html.escape(ARM_SHORT.get(a, a))}</td>"
                    f"<td>{v['grv_tpr']:.2f}</td><td>{v['px_frames']:.3f}</td>"
                    f"<td>{v['px_bytes']:.1f}</td><td>{v['ss_frames'] or 0:.3f}</td><td>{v['flows']}</td></tr>"
                )

    body = f"""
<h2>1. Hardware, Kernel &amp; CPU Core Pinning Topology</h2>
<div class="grid-2">
  <div class="card">
    <h3>Hetzner Dedicated vCPU Instance Specification</h3>
    <ul>
      <li><strong>Instance type:</strong> Hetzner Cloud <code>ccx43</code> (Dedicated vCPU, Ashburn <code>us-east</code> / Nuremberg).</li>
      <li><strong>Processor:</strong> AMD EPYC-Milan (8 physical cores / 16 SMT hardware threads across 1 socket, shared L3).</li>
      <li><strong>OS &amp; Kernel:</strong> Ubuntu 24.04 LTS, Linux <code>6.8.0-138-generic</code>, performance governor.</li>
      <li><strong>FoundationDB binary:</strong> Official <code>foundationdb-server_7.3.77-1_amd64.deb</code> and <code>foundationdb-clients_7.3.77-1_amd64.deb</code>:
        <ul>
          <li><code>/usr/sbin/fdbserver</code> SHA-256: <code>ebe1bfad3fd513bc81f05f0d6fb14c236e5fa586aa0de429efdb4d6c47ca1ca1</code></li>
          <li><code>/usr/lib/libfdb_c.so</code> SHA-256: <code>2b2fd4a8db996f6b4011ee35c983952f59e0de8b3ca5dd432c8dc1fa6ab2b513</code></li>
        </ul>
      </li>
      <li><strong>Client Benchmark Binaries (Round 6):</strong>
        <ul>
          <li><code>clientbench-go</code> (<code>CGO_ENABLED=0</code>, Go 1.24): <code>845b7ebf42f9ec90a5b92dbe7d3e0955d86346cc97c6569cbd655d9c77adb089</code></li>
          <li><code>clientbench-cgo</code> (<code>-tags libfdbc</code>): <code>a2cb6e76fce4b7516d5b901e69f8767067122f9f2eca8803c8c7c145cf0f4914</code></li>
        </ul>
      </li>
    </ul>
  </div>
  <div class="card">
    <h3>Strict Core Isolation Layout (<code>lscpu -e</code> Verified)</h3>
    <div class="table-wrap">
      <table>
        <thead>
          <tr>
            <th>Physical Core</th><th>Logical CPUs</th><th>3-Process Cluster (<code>netg-r6</code>)</th><th>5-Process Split Cluster (<code>netg-r6b</code>)</th>
          </tr>
        </thead>
        <tbody>
          <tr>
            <td>Core 0</td><td><code>0, 1</code></td>
            <td class="text-left">OS housekeeping, Python driver, <code>perf</code>, <code>tcpdump</code> container</td>
            <td class="text-left">OS housekeeping, Python driver, <code>perf</code>, <code>tcpdump</code> container</td>
          </tr>
          <tr>
            <td>Core 1</td><td><code>2, 3</code></td>
            <td class="text-left"><code>fdbserver :4751</code> (<code>--class stateless</code>) on CPUs <code>2,3</code></td>
            <td class="text-left"><code>:4751 stateless</code> (CPU <code>2</code>) + <code>:4754 grv_proxy</code> (CPU <code>3</code>)</td>
          </tr>
          <tr>
            <td>Core 2</td><td><code>4, 5</code></td>
            <td class="text-left"><code>fdbserver :4752</code> (<code>--class transaction</code>) on CPUs <code>4,5</code></td>
            <td class="text-left"><code>:4755 commit_proxy</code> (CPU <code>4</code>) + <code>:4752 transaction</code> (CPU <code>5</code>)</td>
          </tr>
          <tr>
            <td>Core 3</td><td><code>6, 7</code></td>
            <td class="text-left"><code>fdbserver :4753</code> (<code>--class storage</code>) on CPUs <code>6,7</code></td>
            <td class="text-left"><code>:4753 storage</code> on CPUs <code>6,7</code></td>
          </tr>
          <tr>
            <td>Cores 4–7</td><td><code>8–15</code></td>
            <td class="text-left"><code>clientbench</code> (<code>taskset -c 8-15</code>, <code>GOMAXPROCS=8</code>)</td>
            <td class="text-left"><code>clientbench</code> (<code>taskset -c 8-15</code>, <code>GOMAXPROCS=8</code>)</td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</div>

<h2>2. Differential Server-Cycle Measurement &amp; Anti-Bias Protocol</h2>
<div class="grid-2">
  <div class="card">
    <h3>Short-Minus-Long Differential (<code>run_cell.py</code>)</h3>
    <ul>
      <li>Every arm measurement in every round executes <strong>two</strong> full <code>clientbench</code> runs under <code>perf stat -p &lt;fdbserver_pids&gt; -e task-clock,cycles,instructions</code>: a <strong>short</strong> run (<em>N<sub>short</sub></em> iterations + 5% warmup) and a <strong>long</strong> run (<em>N<sub>long</sub> = 3 &times; N<sub>short</sub></em> iterations + 5% warmup).</li>
      <li>Server cycles per transaction is computed as <code>(cycles<sub>long</sub> - cycles<sub>short</sub>) / (tx<sub>long</sub> - tx<sub>short</sub>)</code>, canceling process startup, connection dial/handshake, initial key-location warmups, and fixed per-run background ticks.</li>
      <li>Client metrics (<code>ops/s</code>, <code>p50</code>, <code>p99</code>, <code>getrusage</code> user+sys CPU/op, Go heap allocs/op) come from the steady-state measurement window of the long run (after warmup).</li>
    </ul>
  </div>
  <div class="card">
    <h3>Rotated Round Ordering &amp; Foreign-CPU Guard</h3>
    <ul>
      <li><strong>Order rotation (<code>ladder.sh</code>):</strong> In round <em>r</em>, the arm list is cyclically shifted by <code>r &times; (N/2 + 1) mod N</code> and reversed on odd rounds, preventing warm-cache or storage-engine compaction drift from favoring any arm.</li>
      <li><strong>Foreign-core guard:</strong> Around every run, <code>run_cell.py</code> snapshots <code>/proc/stat</code> busy jiffies on CPUs <code>2–15</code> and subtracts the client's <code>RUSAGE_CHILDREN</code> CPU time plus the <code>fdbserver</code> PIDs' <code>/proc/&lt;pid&gt;/stat</code> jiffies. Any run where foreign processes consume above the threshold is automatically rejected and re-run.</li>
      <li><strong>Box-to-box calibration:</strong>Running identical binaries on Box 1 (`rawhz4/cross`) and Box 2 (`rawhz5`) showed that while <code>L</code> on <code>set100/64</code> is within 3% (`18.6k` vs `19.2k` ops/s, `70.4` vs `72.0` kc/tx), latency-sensitive <code>get100/64</code> varies by up to 25% across cloud hypervisor placements (`A`: `47.4k` vs `36.0k`). Therefore, <strong>all comparisons are strictly paired within the same box and rotated round</strong>.</li>
    </ul>
  </div>
</div>

<h2>3. Wire Equivalence Verification (1-Worker <code>set100</code> TCP Reassembly)</h2>
<p>To verify <strong>T5 (Wire Correctness)</strong>, <code>wirestats.py</code> reassembles every loopback TCP stream from <code>tcpdump</code> captures (checking for zero kernel packet drops and zero sequence gaps) and classifies every FDB frame by root file identifier (<code>838566 = grv</code>, <code>93948 = commit</code>, <code>8454530 = get</code>, <code>6795746 = getkeyvalues</code>). At 1 worker (no cross-goroutine batching), all single-instance arms send identical requests and bytes per transaction:</p>
<div class="table-wrap">
  <table>
    <thead>
      <tr>
        <th>Box</th><th>Arm</th><th>GRV tx / request</th><th>Proxy frames / tx</th><th>Proxy bytes / tx</th><th>Storage frames / tx</th><th>Client TCP Connections</th>
      </tr>
    </thead>
    <tbody>
      {"".join(one_rows)}
    </tbody>
  </table>
</div>

<h2>4. Race Detector (<code>-race</code>) Verification Matrix</h2>
<p>Every transport mode was run against the full <code>//pkg/fdbgo/transport:transport_test</code> and <code>//pkg/fdbgo/client/...</code> test suites under <code>bazelisk test --@io_bazel_rules_go//go/config:race</code> (logs in <code>/var/tmp/netg-bench/race*</code>):</p>
<div class="table-wrap">
  <table>
    <thead>
      <tr>
        <th>Mode / Env</th>
        <th><code>transport_test</code> (<code>-race</code>)</th>
        <th><code>client/...</code> (22 targets, <code>-race</code>)</th>
        <th class="text-left">Log Artifact &amp; Notes</th>
      </tr>
    </thead>
    <tbody>
      <tr>
        <td>Default (<code>A</code>, legacy writer)</td>
        <td><span class="badge pass">PASS</span></td>
        <td><span class="badge pass">PASS (22/22)</span></td>
        <td class="text-left"><code>race6-default.log</code></td>
      </tr>
      <tr>
        <td><code>FDBGO_NET_DESIGN=1</code> (<code>D</code>, 10/20&micro;s delay)</td>
        <td><span class="badge pass">PASS</span></td>
        <td><span class="badge pass">PASS (22/22)</span></td>
        <td class="text-left"><code>race6-d.log</code> — includes <code>TestDesignWriter_T1T2</code> for <code>d</code></td>
      </tr>
      <tr>
        <td><code>FDBGO_NET_DESIGN=ng</code> (<code>D-NG</code>, mutex+slice)</td>
        <td><span class="badge pass">PASS</span></td>
        <td><span class="badge pass">PASS (22/22)</span></td>
        <td class="text-left"><code>race6-ng.log</code> — includes <code>TestDesignWriter_T1T2</code> for <code>ng</code></td>
      </tr>
      <tr>
        <td><code>FDBGO_NET_DESIGN=ngy</code> (<code>D-NG+yield</code>)</td>
        <td><span class="badge pass">PASS</span></td>
        <td><span class="badge pass">PASS (22/22)</span></td>
        <td class="text-left"><code>race7-ngy.log</code></td>
      </tr>
      <tr>
        <td><code>FDBGO_NET_DESIGN=ngc</code> (<code>D-NG-chan</code>)</td>
        <td><span class="badge pass">PASS</span></td>
        <td><span class="badge pass">PASS (22/22)</span></td>
        <td class="text-left"><code>race7-ngc.log</code></td>
      </tr>
      <tr>
        <td><code>FDBGO_NET_DESIGN=ngr</code> (<code>D-NG-ring</code> v2)</td>
        <td><span class="badge pass">PASS</span></td>
        <td><span class="badge pass">PASS (22/22)</span></td>
        <td class="text-left"><code>race7-ngr2.log</code></td>
      </tr>
      <tr>
        <td><code>FDBGO_NET_DESIGN=nga</code> (<code>D-NG-adap20/100</code>)</td>
        <td><span class="badge pass">PASS</span></td>
        <td><span class="badge pass">PASS (22/22)</span></td>
        <td class="text-left"><code>race7-nga.log</code></td>
      </tr>
      <tr class="highlight-warn">
        <td><code>FDBGO_NET_DESIGN=m</code> (<code>D-M</code>, sender writes)</td>
        <td><span class="badge pass">PASS</span></td>
        <td><span class="badge fail">21/22 PASS, 1 FAIL (10 subtests)</span></td>
        <td class="text-left"><code>race6-m.log</code> — expected backpressure failures in <code>client_test</code> proving <code>D-M</code> violates T1/T2 when kernel socket buffer fills</td>
      </tr>
    </tbody>
  </table>
</div>

<h2>5. Raw Dataset Inventory</h2>
<div class="table-wrap">
  <table>
    <thead>
      <tr>
        <th>Directory / File</th>
        <th>Row / File Count</th>
        <th class="text-left">Description</th>
      </tr>
    </thead>
    <tbody>
      <tr>
        <td><code>rawhz6/ladder.jsonl</code></td>
        <td>{len(hz6_ladder)} rows</td>
        <td class="text-left">Round 6 on <code>netg-r6</code>: <code>L, A, A0, DNG, DNGy, DNGchan, DNGring</code> (+ <code>M</code>) &times; 9 cells &times; 3 rounds</td>
      </tr>
      <tr>
        <td><code>rawhz6/linger.jsonl</code></td>
        <td>{len(hz6_linger)} rows</td>
        <td class="text-left">Round 6 on <code>netg-r6</code>: full linger sweep (<code>L, A, M, DNG, D5, D, D50, D100, D250, DNGa20, DNGa100</code>) &times; 9 cells &times; 3 rounds</td>
      </tr>
      <tr>
        <td><code>rawhz6/linger_rate.jsonl</code> + <code>rate.jsonl</code></td>
        <td>{len(load_jsonl('rawhz6/linger_rate.jsonl')) + len(load_jsonl('rawhz6/rate.jsonl'))} rows</td>
        <td class="text-left">Multi-rate capped (25%, 60%, 90% of <code>L</code>) on <code>get100/64</code> and <code>set100/64</code> &times; 3 rounds</td>
      </tr>
      <tr>
        <td><code>rawhz6/syscalls.jsonl</code></td>
        <td>{len(load_jsonl('rawhz6/syscalls.jsonl'))} rows</td>
        <td class="text-left">Client <code>perf stat</code> syscall &amp; futex tracepoints per operation on <code>get100/64</code> and <code>get100/256</code></td>
      </tr>
      <tr>
        <td><code>rawhz6/prof/</code> &amp; <code>prof2/</code></td>
        <td>20 profiles</td>
        <td class="text-left">CPU, mutex, and block <code>pprof</code> profiles for <code>ng</code>, <code>ngc</code>, and <code>ngr</code> (v1 &amp; v2) at 64 and 256 workers</td>
      </tr>
      <tr>
        <td><code>rawhz6b/role_ladder.jsonl</code> &amp; <code>role_rate.jsonl</code></td>
        <td>{len(hz6b_ladder) + len(load_jsonl('rawhz6b/role_rate.jsonl'))} rows</td>
        <td class="text-left">5-process split cluster on <code>netg-r6b</code>: per-role server CPU cycles/tx + <code>wire-*</code> per-role <code>tcpdump</code> captures</td>
      </tr>
      <tr>
        <td><code>rawhz5/</code> &amp; <code>rawhz4/</code></td>
        <td>{len(hz5_ladder) + len(hz4_ladder)} ladder rows</td>
        <td class="text-left">Phase 4 &amp; Phase 5 datasets (<code>L, L4, A, D, D-NG, D-M, D 4Ps, D-M 4Ps</code>, wire captures, 1-worker golden, cross-box check)</td>
      </tr>
    </tbody>
  </table>
</div>
"""
    with open(os.path.join(REPORT_DIR, "methodology.html"), "w") as f:
        f.write(page_shell("methodology.html", "Methodology & Verification", body))


if __name__ == "__main__":
    generate_all()
