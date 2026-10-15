#!/usr/bin/env python3
"""Sync docs/benchmarks/{hcloud-rebench-20261014,hcloud-multinode-vpc-20261014}
into website/content/docs/benchmarks/ with Hugo front matter, clean links, and
precision SVG forest/interval and absolute-latency charts generated directly
from the Markdown tables on each page.
Pass --check to verify the generated files match the committed tree and contain
no private IPv4 literals or local scratch paths.
"""
import argparse
import html
import math
from pathlib import Path
import re
import sys

REPO = Path(__file__).resolve().parent.parent
SRC_ROOT = REPO / 'docs/benchmarks'
DST_ROOT = REPO / 'website/content/docs/benchmarks'

CAMPAIGNS = [
    (
        'hcloud-multinode-vpc-20261014',
        '4-Node VPC Campaign (2026-10-14)',
        'Preregistered 4-host VPC benchmark campaign (1 client VM + 3 FDB 7.3.77 server VMs, double ssd-2, 9 storage servers): pure-Go vs CGo at 1, 8, 32, and 64 workers and cross-server USE_REMOTE_FETCH vs SCAN_AND_FETCH.',
        1,
        [
            ('phase-m1-client.md', 'Phase M1: Client (1, 8, 32, 64 Workers)', 'Pure-Go vs CGo client latency and process CPU across 1, 8, 32, and 64 closed-loop workers over a real VPC to a 3-node double ssd-2 cluster.', 1),
            ('phase-m2-remotefetch.md', 'Phase M2: Remote Fetch (3-Node VPC)', 'Record Layer USE_REMOTE_FETCH vs SCAN_AND_FETCH across 9 storage servers on a 3-node VPC cluster with verified shard placement.', 2),
        ],
    ),
    (
        'hcloud-rebench-20261014',
        'Single-Node 16-vCPU Campaign (2026-10-14)',
        'Preregistered 8-phase benchmark campaign on a dedicated 16-vCPU host comparing the pure-Go client, Record Layer, mapped remote fetch, and SQL across the RFC-258 trees.',
        2,
        [
            ('phase-a-client.md', 'Phase A: Client (1 Worker)', 'Pure-Go client vs CGo (libfdb_c 7.3.77) and vs pre-allocation pure-Go across 11 workloads at 1 closed-loop worker.', 1),
            ('phase-a2-client-8w.md', 'Phase A2: Client (8 Workers)', 'Pure-Go client vs CGo and vs pre-allocation pure-Go at 8 concurrent closed-loop workers.', 2),
            ('phase-b-recordlayer.md', 'Phase B: Record Layer', 'Record Layer benchmarks across legacy, before-allocation, and final trees.', 3),
            ('phase-c1-remotefetch.md', 'Phase C1: Remote Fetch (Forward)', 'Forward USE_REMOTE_FETCH vs SCAN_AND_FETCH across pr2 and final trees on small and two-chunk split records.', 4),
            ('phase-c2-reverse-skip.md', 'Phase C2: Remote Fetch (Reverse & Skip)', 'Reverse scans and executor Skip 3 paths with USE_REMOTE_FETCH vs SCAN_AND_FETCH.', 5),
            ('phase-c3-synthetic-rtt.md', 'Phase C3: Remote Fetch (0.5 ms Synthetic RTT)', 'Sensitivity of USE_REMOTE_FETCH vs SCAN_AND_FETCH to 0.5 ms nominal synthetic veth RTT.', 6),
            ('phase-d1-sql.md', 'Phase D1: SQL Common', 'SQL plan-cache point select across legacy, before, and final trees.', 7),
            ('phase-d2-sql-remotefetch.md', 'Phase D2: SQL Remote Fetch', 'SQL non-covering index scan with USE_REMOTE_FETCH vs SCAN_AND_FETCH.', 8),
        ],
    ),
]

CI_RE = re.compile(r'([0-9]+\.[0-9]+)\s*\[\s*([0-9]+\.[0-9]+)\s*,\s*([0-9]+\.[0-9]+)\s*\]')


def parse_ci(cell: str):
    m = CI_RE.search(cell)
    if not m:
        return None
    return float(m.group(1)), float(m.group(2)), float(m.group(3))


def ci_class(pt: float, lo: float, hi: float) -> str:
    if hi < 1.0:
        return 'ci-win'
    if lo > 1.0:
        return 'ci-reg'
    return 'ci-neutral'


def format_tick(t: float) -> str:
    if abs(t - 1.0) < 1e-6:
        return '1.0×'
    s = f'{t:.2f}'.rstrip('0').rstrip('.')
    if '.' not in s:
        s += '.0'
    return s + '×'


def verify_summary_numbers(title: str, svg_or_summary: str, raw_md: str):
    """Fail if any decimal number token in summary (other than 1.0/1.000 parity) is absent from raw_md."""
    m = re.search(r'<div class="bench-summary">(.*?)</div>', svg_or_summary, flags=re.S)
    summary = m.group(1) if m else svg_or_summary
    md_decimals = set(re.findall(r'\b\d+\.\d+\b', raw_md)) | {'1.0', '1.000'}
    for tok in re.findall(r'\b\d+\.\d+\b', summary):
        if tok not in md_decimals:
            raise ValueError(f'{title}: summary number {tok!r} not found as token in canonical markdown')


def split_label_colon(line: str):
    depth = 0
    for i, ch in enumerate(line):
        if ch == '(':
            depth += 1
        elif ch == ')' and depth > 0:
            depth -= 1
        elif ch == ':' and depth == 0:
            return line[:i], line[i + 1:]
    return None


def render_summary_html(summary: str) -> str:
    if not summary:
        return ''
    parts = [p.strip() for p in summary.split('\n') if p.strip()]
    items = []
    for p in parts:
        sp = split_label_colon(p)
        if sp is not None:
            head, tail = sp
            items.append(f'<li><strong>{html.escape(head.strip())}:</strong> {html.escape(tail.strip())}</li>')
        else:
            items.append(f'<li>{html.escape(p)}</li>')
    return '<div class="bench-summary"><ul class="bench-summary-list">' + ''.join(items) + '</ul></div>'


def render_forest_svg(title: str, subtitle: str, groups: list, ticks: list, left_label: str, right_label: str, summary: str = '') -> str:
    """Render a log2-scaled forest plot SVG with exact 95% CI whiskers and right-aligned numeric column."""
    max_label_chars = max((len(lbl) for _, rows in groups for lbl, _ in rows), default=20)
    pad_l = max(165, int(max_label_chars * 7.8) + 26)
    pad_r = 195
    plot_w = 960
    W = pad_l + plot_w + pad_r
    row_h = 20
    sec_h = 22
    pad_t = 44
    pad_b = 30

    total_rows = sum(len(rows) for _, rows in groups)
    total_secs = sum(1 for gtitle, _ in groups if gtitle)
    H = pad_t + total_rows * row_h + total_secs * sec_h + pad_b

    tmin, tmax = min(ticks), max(ticks)
    lmin = math.log2(tmin)
    lmax = math.log2(tmax)
    tick_labels = [format_tick(t) for t in ticks]
    if len(set(tick_labels)) != len(tick_labels):
        raise ValueError(f'{title}: duplicate formatted axis tick labels {tick_labels} for {ticks}')

    def x_of(val: float) -> float:
        if not (tmin <= val <= tmax):
            raise ValueError(f'{title}: value {val} falls outside axis tick bounds [{tmin}, {tmax}]')
        return pad_l + ((math.log2(val) - lmin) / (lmax - lmin)) * plot_w

    out = [
        '<figure class="bench-figure">',
        f'<figcaption><strong>{html.escape(title)}</strong> — {html.escape(subtitle)}</figcaption>',
    ]
    if summary:
        out.append(render_summary_html(summary))
    out.append(f'<svg class="bench-svg" viewBox="0 0 {W} {H}" role="img" aria-label="{html.escape(title)}">')

    # Grid lines and top/bottom axis labels
    y_top = pad_t - 8
    y_bot = H - pad_b + 4
    for t, lbl in zip(ticks, tick_labels):
        x = x_of(t)
        is_one = abs(t - 1.0) < 1e-6
        cls = 'parity-line' if is_one else 'grid-line'
        out.append(f'<line class="{cls}" x1="{x:.1f}" y1="{y_top}" x2="{x:.1f}" y2="{y_bot}"/>')
        out.append(f'<text class="axis-text" x="{x:.1f}" y="{y_top - 6}" text-anchor="middle">{lbl}</text>')
        out.append(f'<text class="axis-text" x="{x:.1f}" y="{y_bot + 15}" text-anchor="middle">{lbl}</text>')

    x_one = x_of(1.0)
    out.append(f'<text class="axis-text" x="{x_one - 8:.1f}" y="{y_top - 19}" text-anchor="end">← {html.escape(left_label)}</text>')
    out.append(f'<text class="axis-text" x="{x_one + 8:.1f}" y="{y_top - 19}" text-anchor="start">{html.escape(right_label)} →</text>')

    y = pad_t
    for gtitle, rows in groups:
        if gtitle:
            out.append(f'<line class="section-divider" x1="8" y1="{y + 4}" x2="{W - 8}" y2="{y + 4}"/>')
            out.append(f'<text class="section-title" x="10" y="{y + 19}">{html.escape(gtitle)}</text>')
            y += sec_h
        for label, cell in rows:
            cy = y + row_h * 0.55
            out.append(f'<text class="row-label" x="{pad_l - 12}" y="{cy + 4:.1f}" text-anchor="end">{html.escape(label)}</text>')
            parsed = parse_ci(cell)
            if parsed is None:
                out.append(f'<text class="axis-text" x="{pad_l + 10}" y="{cy + 4:.1f}">{html.escape(cell)}</text>')
            else:
                pt, lo, hi = parsed
                cls = ci_class(pt, lo, hi)
                x_pt = x_of(pt)
                x_lo = x_of(lo)
                x_hi = x_of(hi)
                out.append(
                    f'<g class="{cls}">'
                    f'<title>{html.escape(label)}: {pt:.3f}× [{lo:.3f}, {hi:.3f}]</title>'
                    f'<line x1="{x_lo:.1f}" y1="{cy:.1f}" x2="{x_hi:.1f}" y2="{cy:.1f}" stroke-width="2.2"/>'
                    f'<line x1="{x_lo:.1f}" y1="{cy - 4:.1f}" x2="{x_lo:.1f}" y2="{cy + 4:.1f}" stroke-width="1.8"/>'
                    f'<line x1="{x_hi:.1f}" y1="{cy - 4:.1f}" x2="{x_hi:.1f}" y2="{cy + 4:.1f}" stroke-width="1.8"/>'
                    f'<circle cx="{x_pt:.1f}" cy="{cy:.1f}" r="3.8"/>'
                    f'</g>'
                )
                out.append(f'<text class="val-text" x="{W - pad_r + 12}" y="{cy + 4:.1f}">{pt:.3f}× [{lo:.3f}, {hi:.3f}]</text>')
            y += row_h

    out.extend([
        '</svg>',
        '<div class="bench-legend">'
        '<span><i class="bench-swatch" style="background:#0d9488"></i> 95% CI entirely &lt; 1.000 (faster / lower)</span>'
        '<span><i class="bench-swatch" style="background:#64748b"></i> 95% CI overlaps 1.000 (unresolved)</span>'
        '<span><i class="bench-swatch" style="background:#d97706"></i> 95% CI entirely &gt; 1.000 (slower / higher)</span>'
        '</div>',
        '</figure>',
    ])
    return '\n'.join(out)


def render_grouped_bars_svg(title: str, subtitle: str, items: list, unit: str, max_val: float, ticks: list, summary: str = '') -> str:
    """Render horizontal grouped bars (e.g. Pure-Go vs CGo absolute latency with p50 bar + p99 whisker)."""
    max_label_chars = max((len(lbl) for lbl, *_ in items), default=16)
    pad_l = max(165, int(max_label_chars * 7.8) + 26)
    pad_r = 205
    plot_w = 960
    W = pad_l + plot_w + pad_r
    row_h = 28
    pad_t = 28
    pad_b = 28
    H = pad_t + len(items) * row_h + pad_b

    def x_of(v: float) -> float:
        if not (0.0 <= v <= max_val):
            raise ValueError(f'{title}: value {v} falls outside [0, {max_val}]')
        return pad_l + (v / max_val) * plot_w

    out = [
        '<figure class="bench-figure">',
        f'<figcaption><strong>{html.escape(title)}</strong> — {html.escape(subtitle)}</figcaption>',
    ]
    if summary:
        out.append(render_summary_html(summary))
    out.append(f'<svg class="bench-svg" viewBox="0 0 {W} {H}" role="img" aria-label="{html.escape(title)}">')
    y_top = pad_t - 6
    y_bot = H - pad_b + 4
    for t in ticks:
        x = x_of(t)
        out.append(f'<line class="grid-line" x1="{x:.1f}" y1="{y_top}" x2="{x:.1f}" y2="{y_bot}"/>')
        out.append(f'<text class="axis-text" x="{x:.1f}" y="{y_bot + 15}" text-anchor="middle">{t:g} {html.escape(unit)}</text>')

    y = pad_t
    for label, go_mean, go_p99, cgo_mean, cgo_p99 in items:
        out.append(f'<text class="row-label" x="{pad_l - 12}" y="{y + 18:.1f}" text-anchor="end">{html.escape(label)}</text>')
        # Pure-Go bar (top) + p99 tick
        w_go = max(1.5, x_of(go_mean) - pad_l)
        x_go99 = x_of(go_p99)
        out.append(
            f'<g class="ci-win"><title>{html.escape(label)} Pure-Go: mean {go_mean:.1f} {unit}, p99 {go_p99:.1f} {unit}</title>'
            f'<rect class="bar-go" x="{pad_l}" y="{y + 4}" width="{w_go:.1f}" height="10" rx="2"/>'
            f'<line x1="{pad_l + w_go:.1f}" y1="{y + 9}" x2="{x_go99:.1f}" y2="{y + 9}" stroke-width="1.6"/>'
            f'<line x1="{x_go99:.1f}" y1="{y + 5}" x2="{x_go99:.1f}" y2="{y + 13}" stroke-width="1.8"/></g>'
        )
        # CGo bar (bottom) + p99 tick
        w_cgo = max(1.5, x_of(cgo_mean) - pad_l)
        x_cgo99 = x_of(cgo_p99)
        out.append(
            f'<g class="ci-reg"><title>{html.escape(label)} CGo: mean {cgo_mean:.1f} {unit}, p99 {cgo_p99:.1f} {unit}</title>'
            f'<rect class="bar-cgo" x="{pad_l}" y="{y + 16}" width="{w_cgo:.1f}" height="10" rx="2"/>'
            f'<line x1="{pad_l + w_cgo:.1f}" y1="{y + 21}" x2="{x_cgo99:.1f}" y2="{y + 21}" stroke-width="1.6"/>'
            f'<line x1="{x_cgo99:.1f}" y1="{y + 17}" x2="{x_cgo99:.1f}" y2="{y + 25}" stroke-width="1.8"/></g>'
        )
        out.append(
            f'<text class="val-text" x="{W - pad_r + 10}" y="{y + 13:.1f}">Go {go_mean:.0f} / p99 {go_p99:.0f} {html.escape(unit)}</text>'
            f'<text class="axis-text" x="{W - pad_r + 10}" y="{y + 25:.1f}">CGo {cgo_mean:.0f} / p99 {cgo_p99:.0f} {html.escape(unit)}</text>'
        )
        y += row_h

    out.extend([
        '</svg>',
        '<div class="bench-legend">'
        '<span><i class="bench-swatch" style="background:#0d9488"></i> Solid bar = Pure-Go mean latency; whisker = Pure-Go p99</span>'
        '<span><i class="bench-swatch" style="background:#d97706"></i> Solid bar = CGo (libfdb_c 7.3.77) mean latency; whisker = CGo p99</span>'
        '</div>',
        '</figure>',
    ])
    return '\n'.join(out)


def extract_md_table(text: str, heading_substr: str) -> list:
    """Return data rows (as lists of stripped cell strings) of the first markdown table after heading_substr."""
    idx = text.find(heading_substr)
    if idx < 0:
        return []
    lines = text[idx:].splitlines()[1:]
    table_lines = []
    in_table = False
    for line in lines:
        s = line.strip()
        if s.startswith('|') and s.endswith('|'):
            in_table = True
            table_lines.append(s)
        elif in_table:
            break
    if len(table_lines) < 3:
        return []
    rows = []
    for tl in table_lines[2:]:
        cells = [c.strip().replace('**', '') for c in tl.strip('|').split('|')]
        rows.append(cells)
    return rows


def build_page_charts(cid: str, fname: str, raw_md: str) -> str:
    """Build precision SVG charts directly from the exact numbers in raw_md."""
    if cid == 'hcloud-multinode-vpc-20261014' and fname == 'phase-m1-client.md':
        mean_groups, p99_groups, cpu_groups = [], [], []
        for h, label in [
            ('### `M1w1`', 'M1w1 (1 worker — 7/7 clean)'),
            ('### `M1w8`', 'M1w8 (8 workers — 5/7 clean)'),
            ('### `M1w32`', 'M1w32 (32 workers — 5/7 clean)'),
            ('### `M1w64`', 'M1w64 (64 workers — 5/7 clean)'),
        ]:
            rows = extract_md_table(raw_md, h)
            mg, pg, cg = [], [], []
            for r in rows:
                w = r[0]
                mg.append((w, r[1] if parse_ci(r[1]) else 'N/A (omitted)'))
                pg.append((w, r[3] if parse_ci(r[3]) else 'N/A (omitted)'))
                cg.append((w, r[4] if parse_ci(r[4]) else 'N/A (omitted)'))
            mean_groups.append((label, mg))
            p99_groups.append((label, pg))
            cpu_groups.append((label, cg))

        # Also extract M1w1 and M1w64 absolute values for grouped bar chart
        abs_rows = extract_md_table(raw_md, '## Absolute Values')
        by_key = {(r[0], r[1], r[2]): r for r in abs_rows if len(r) >= 7}
        abs_items = []
        for ph, w in [
            ('M1w1', 'grv'), ('M1w1', 'get100'), ('M1w1', 'get1000'), ('M1w1', 'range100'), ('M1w1', 'set100'), ('M1w1', 'batchset50'), ('M1w1', 'batchset50x1000'),
            ('M1w64', 'grv'), ('M1w64', 'get100'), ('M1w64', 'get1000'), ('M1w64', 'range100'), ('M1w64', 'set100'),
        ]:
            go_r = by_key.get((ph, w, 'final pure-Go'))
            cgo_r = by_key.get((ph, w, 'CGo'))
            if go_r and cgo_r:
                abs_items.append((f'{ph} {w}', float(go_r[4]), float(go_r[6]), float(cgo_r[4]), float(cgo_r[6])))

        c1 = render_forest_svg(
            'Figure M1.1: Mean Latency Ratio — Final Pure-Go / CGo (log₂ scale, 95% block-bootstrap CI)',
            '6 paired blocks per workload, 20,000 resamples (seed 492817). Below 1.0× means Pure-Go is faster.',
            mean_groups, [0.35, 0.5, 0.7, 1.0, 1.4, 2.0], 'Pure-Go faster', 'CGo faster',
            summary='\n'.join([
                'Wins (18/22 clean cells, 95% CI < 1.000): 0.554×–0.980× across M1w1 (7/7: 0.579×–0.954×), M1w8 (4/5: get100 0.888×, get1000 0.942×, range100 0.868×, set100 0.980×), M1w32 (3/5: get100 0.770×, get1000 0.715×, set100 0.938×), and M1w64 (4/5: get100 0.676×, get1000 0.632×, range100 0.554×, set100 0.887×).',
                'Unresolved (3/22, 95% CI spans 1.000): M1w8 grv 0.996× [0.949, 1.038], M1w32 range100 0.869× [0.753, 1.148], M1w64 grv 1.029× [0.990, 1.069].',
                'Regressions (1/22, 95% CI > 1.000): M1w32 grv 1.041× [1.007, 1.081].',
                'Omitted (6/28 planned cells): at 8, 32, and 64 workers, batchset50 exceeded the 100 MB server tLog queue gate (113.7 MB, 193.4 MB, 141.9 MB; R4 contaminated), and the phase driver stopped before batchset50x1000 (R4 unverifiable).',
            ]),
        )
        c2 = render_forest_svg(
            'Figure M1.2: Tail (p99) Latency Ratio — Final Pure-Go / CGo (log₂ scale, 95% block-bootstrap CI)',
            'Run-level p99 transaction latency resampled over paired blocks. Below 1.0× means Pure-Go has lower p99.',
            p99_groups, [0.25, 0.35, 0.5, 0.7, 1.0, 1.4, 2.0], 'Pure-Go lower p99', 'CGo lower p99',
            summary='\n'.join([
                'Wins (17/22 clean cells, 95% CI < 1.000): 0.385×–0.957× (lowest on M1w64 get1000 at 0.385× [0.336, 0.438], run-level medians 4544.4 µs pure-Go vs 11686.4 µs CGo).',
                'Unresolved (4/22, 95% CI spans 1.000): M1w1 get100 0.865× [0.734, 1.037], M1w8 grv 1.101× [0.869, 1.457], M1w32 grv 0.871× [0.754, 1.017], M1w64 grv 0.960× [0.757, 1.190].',
                'Regressions (1/22, 95% CI > 1.000): M1w32 range100 paired-block ratio 1.247× [1.048, 1.644] (run-level medians 6618.7 µs pure-Go vs 5977.7 µs CGo).',
            ]),
        )
        c3 = render_forest_svg(
            'Figure M1.3: Shutdown Process CPU (E−A) Ratio — Final Pure-Go / CGo (log₂ scale, 95% CI)',
            'User+system process CPU through finalizer drain and network stop per transaction. Below 1.0× means Pure-Go uses less CPU.',
            cpu_groups, [0.5, 0.7, 1.0, 1.4, 2.0, 2.5], 'Pure-Go less CPU', 'CGo less CPU',
            summary='\n'.join([
                '1 worker (M1w1, 7/7 clean): Pure-Go uses less E−A process CPU on all 7 workloads (0.570×–0.748×, 260.4–1290.8 µs/tx vs 451.0–1737.1 µs/tx).',
                '8, 32, and 64 workers (15 clean cells): Pure-Go uses less E−A CPU on grv at all three worker levels (0.904× at 8w, 0.765× at 32w, 0.736× at 64w), is unresolved on M1w64 range100 (1.069× [0.923, 1.180]), and uses more E−A CPU in 11 of 12 non-grv cells (1.291×–2.162×; highest on set100 at 2.142× in M1w8, 2.162× in M1w32, and 1.981× in M1w64).',
            ]),
        )
        c4 = render_grouped_bars_svg(
            'Figure M1.4: Absolute Mean & p99 Latency at 1 Worker (M1w1) and 64 Workers (M1w64)',
            'Medians of 6 run-level statistics in µs/transaction (solid bar = mean, whisker = p99).',
            abs_items, 'µs', 12000.0, [0, 2000, 4000, 6000, 8000, 10000, 12000],
            summary='\n'.join([
                'M1w1 (1 worker, 7 workloads): Pure-Go medians are 967.1–6241.4 µs mean (1477.4–9383.0 µs p99) vs CGo 1673.5–6577.5 µs mean (3133.0–10055.1 µs p99).',
                'M1w64 (64 workers, 5 clean workloads): Pure-Go medians are 2017.9–5918.8 µs mean (4237.9–9591.9 µs p99) vs CGo 1931.8–6659.4 µs mean (4015.5–11686.4 µs p99). Worker levels ran in separate phases; cross-worker comparisons are descriptive only.',
            ]),
        )
        for c in (c1, c2, c3, c4):
            verify_summary_numbers(fname, c, raw_md)
        return '\n\n'.join([c1, c2, c3, c4])

    if cid == 'hcloud-multinode-vpc-20261014' and fname == 'phase-m2-remotefetch.md':
        rl_rows = extract_md_table(raw_md, '### `M2rl`')
        sk_rows = extract_md_table(raw_md, '### `M2skip`')
        def short_place(p: str) -> str:
            return 'cross-srv' if p == 'cross-server' else p.replace('partially colocated ', '').strip('()')
        ns_groups = [
            ('M2rl: BenchmarkIndexFetch (3-node double ssd-2)', [(f"{r[0]} [{short_place(r[1])}]", r[2]) for r in rl_rows]),
            ('M2skip: BenchmarkPlanSkip (3-node double ssd-2)', [(f"{r[0]} [{short_place(r[1])}]", r[2]) for r in sk_rows]),
        ]
        al_groups = [
            ('M2rl: Client Go allocs/op Ratio', [(r[0], r[4]) for r in rl_rows]),
            ('M2skip: Client Go allocs/op Ratio', [(r[0], r[4]) for r in sk_rows]),
        ]
        c1 = render_forest_svg(
            'Figure M2.1: Mean Time (ns/op) Ratio — USE_REMOTE_FETCH / SCAN_AND_FETCH (log₂ scale, 95% CI)',
            '3-node VPC double ssd-2 cluster (9 storage servers). Below 1.0× means USE_REMOTE_FETCH is faster.',
            ns_groups, [0.15, 0.25, 0.5, 0.7, 1.0, 1.4, 2.0, 2.5], 'Remote fetch faster', 'SCAN_AND_FETCH faster',
            summary='\n'.join([
                'Wins (4/16 cases, 95% CI < 1.000): M2skip skip3-rows10-reverse 0.158× [0.155, 0.160] (4.87 ms vs 31.13 ms), M2rl split-rows10-reverse 0.854× [0.823, 0.886] (5.77 ms vs 6.81 ms, f = 0.0000), M2rl split-rows10-forward 0.873× [0.829, 0.920] (6.00 ms vs 6.83 ms, f = 0.0000), and M2rl small-limit1-forward 0.979× [0.964, 0.995] (2.62 ms vs 2.69 ms).',
                'Unresolved (2/16, 95% CI spans 1.000): M2rl small-limit1-reverse 0.985× [0.969, 1.003] and M2skip skip3-rows10-forward 1.155× [0.991, 1.345].',
                'Regressions (10/16 — 8/12 in M2rl, 2/4 in M2skip, 95% CI > 1.000): small-rows10 (1.031× fwd, 1.026× rev), split-limit1 (1.110× fwd, 1.114× rev), and all six 1,000-row scans: small-rows1000 (1.226× fwd 117.08 vs 95.10 ms, 1.255× rev 118.95 vs 96.39 ms), skip3-rows1000 (1.360× fwd 138.19 vs 98.67 ms, 1.324× rev 133.24 vs 101.06 ms), and 2-chunk split-rows1000 [f = 0.6960] (2.326× fwd 742.51 vs 316.38 ms, 2.300× rev 768.95 vs 327.62 ms).',
            ]),
        )
        c2 = render_forest_svg(
            'Figure M2.2: Client Go Allocations (allocs/op) Ratio — USE_REMOTE_FETCH / SCAN_AND_FETCH',
            'Below 1.0× means USE_REMOTE_FETCH allocates fewer Go heap objects on the client.',
            al_groups, [0.35, 0.5, 0.7, 1.0, 1.2], 'Remote fetch fewer allocs', 'SCAN_AND_FETCH fewer allocs',
            summary='\n'.join([
                'Wins (14/16 cases, 95% CI < 1.000): Client Go allocs/op drop across all 12 multi-row cases (0.398×–0.790×) and both split-limit1 cases (0.942× fwd 248 vs 263, 0.952× rev 250 vs 263).',
                'Regressions (2/16, 95% CI > 1.000): small-limit1-forward 1.040× [1.038, 1.041] (231 vs 222, +9 allocs/op) and small-limit1-reverse 1.049× [1.047, 1.052] (234 vs 223, +11 allocs/op).',
                'Note: Lower client allocations on the six 1,000-row cases (0.398×–0.529×) did not prevent their 1.226×–2.326× ns/op regressions in Figure M2.1.',
            ]),
        )
        for c in (c1, c2):
            verify_summary_numbers(fname, c, raw_md)
        return '\n\n'.join([c1, c2])

    if cid == 'hcloud-rebench-20261014' and fname == 'phase-a-client.md':
        cgo_rows = extract_md_table(raw_md, '### Final pure-Go vs CGo')
        bef_rows = extract_md_table(raw_md, '### Final vs before pure-Go')
        groups = [
            ('Mean Latency Ratio: Final Pure-Go / CGo', [(r[0], r[1]) for r in cgo_rows]),
            ('p99 Latency Ratio: Final Pure-Go / CGo', [(r[0], r[2]) for r in cgo_rows]),
            ('Shutdown Process CPU (E−A) Ratio: Final Pure-Go / CGo', [(r[0], r[3]) for r in cgo_rows]),
        ]
        alloc_groups = [
            ('Go Bytes/tx Ratio: Final / Before Pure-Go', [(r[0], r[4]) for r in bef_rows]),
            ('Go Mallocs/tx Ratio: Final / Before Pure-Go', [(r[0], r[5]) for r in bef_rows]),
        ]
        c1 = render_forest_svg(
            'Figure A.1: Single-Node 1-Worker Client Ratios — Final Pure-Go / CGo (log₂ scale, 95% CI)',
            '6 paired blocks × 3,000 tx/run (18,000 tx/arm/workload). Below 1.0× means Pure-Go is faster / lower CPU.',
            groups, [0.15, 0.25, 0.5, 0.7, 1.0, 1.4], 'Pure-Go faster / lower CPU', 'CGo faster / lower CPU',
            summary='\n'.join([
                'Mean latency (final/CGo): Pure-Go is faster with 95% CI < 1.000 on 10 of 11 workloads (0.189× on client-only ryw, 0.515× on grv, 0.613×–0.931× on reads/commits); range100 (0.846× [0.713, 1.024]) is unresolved.',
                'p99 latency (final/CGo): Lower on 8 of 11 workloads (0.245×–0.942×); range100 (0.861× [0.755, 1.008]), set100 (0.939× [0.845, 1.020]), and batchset50 (0.989× [0.913, 1.126]) are unresolved.',
                'Shutdown CPU (E−A, final/CGo): Lower on 7 of 11 workloads (0.228×–0.887×); get100 (0.948× [0.817, 1.118]), get10000 (0.959× [0.884, 1.046]), range10 (0.969× [0.834, 1.122]), and range100 (1.101× [0.968, 1.269]) are unresolved.',
            ]),
        )
        c2 = render_forest_svg(
            'Figure A.2: Allocation Reductions — Final / Before Pure-Go (3824f146 → 267a4143)',
            'Go heap bytes and mallocs per transaction (6 paired blocks). Below 1.0× means fewer allocations in Final.',
            alloc_groups, [0.7, 0.8, 0.9, 1.0, 1.1], 'Final allocates less', 'Before allocates less',
            summary='\n'.join([
                'Allocations (final/before): Go mallocs/tx drop across all 11 workloads (0.797×–0.973×, largest on range10 0.797× and range100 0.798×) and Go bytes/tx drop across all 11 (0.824×–0.998×, largest on range100 0.824× and range10 0.844×).',
                'Latency & CPU (final/before): All 11 mean-latency 95% CIs span 1.000 (0.984×–1.042×, unresolved); only range100 resolves lower p99 (0.951× [0.916, 0.998]) and lower E−A CPU (0.952× [0.937, 0.971]).',
            ]),
        )
        for c in (c1, c2):
            verify_summary_numbers(fname, c, raw_md)
        return '\n\n'.join([c1, c2])

    if cid == 'hcloud-rebench-20261014' and fname == 'phase-a2-client-8w.md':
        cgo_rows = extract_md_table(raw_md, '### Final pure-Go vs CGo (8 workers)')
        bef_rows = extract_md_table(raw_md, '### Final vs before pure-Go (8 workers)')
        groups = [
            ('Mean Latency Ratio (8w): Final Pure-Go / CGo', [(r[0], r[1]) for r in cgo_rows]),
            ('p99 Latency Ratio (8w): Final Pure-Go / CGo', [(r[0], r[2]) for r in cgo_rows]),
            ('Shutdown Process CPU (E−A) Ratio (8w): Final Pure-Go / CGo', [(r[0], r[3]) for r in cgo_rows]),
            ('Go Mallocs/tx Ratio (8w): Final / Before Pure-Go', [(r[0], r[5]) for r in bef_rows]),
        ]
        res = render_forest_svg(
            'Figure A2.1: Single-Node 8-Worker Client Ratios (log₂ scale, 95% block-bootstrap CI)',
            '6 paired blocks × 24,000 tx/run (144,000 tx/arm/workload). Below 1.0× favors Final Pure-Go.',
            groups, [0.4, 0.6, 0.8, 1.0, 1.3, 1.7], 'Final Pure-Go lower', 'Denominator lower',
            summary='\n'.join([
                'Latency (final/CGo at 8w): Mean latency is lower for pure-Go on range100 (0.663× [0.440, 0.858]) and batchset50 (0.817× [0.768, 0.851]) and unresolved on get100 (0.991× [0.928, 1.084]). p99 is lower on batchset50 (0.914× [0.816, 0.985]) and unresolved on range100 (0.881× [0.696, 1.033]) and get100 (1.015× [0.938, 1.097]).',
                'Shutdown CPU (E−A, final/CGo at 8w): Higher for pure-Go on all 3 workloads: batchset50 1.150× [1.112, 1.188], range100 1.359× [1.138, 1.559], and get100 1.638× [1.603, 1.676].',
                'Allocations (final/before at 8w): Go mallocs/tx drop on all 3 workloads (range100 0.796×, get100 0.872×, batchset50 0.973×).',
            ]),
        )
        verify_summary_numbers(fname, res, raw_md)
        return res

    if cid == 'hcloud-rebench-20261014' and fname == 'phase-b-recordlayer.md':
        rows = extract_md_table(raw_md, '### Final vs before')
        if not rows:
            rows = extract_md_table(raw_md, '## Results')
        groups = [
            ('Mean Time (ns/op): Final / Before', [(r[0], r[1]) for r in rows if len(r) >= 4]),
            ('Go Bytes (B/op): Final / Before', [(r[0], r[2]) for r in rows if len(r) >= 4]),
            ('Go Allocations (allocs/op): Final / Before', [(r[0], r[3]) for r in rows if len(r) >= 4]),
        ]
        if any(g[1] for g in groups):
            res = render_forest_svg(
                'Figure B.1: Record Layer Allocation Optimization Ratios — Final / Before (log₂ scale, 95% CI)',
                '6 paired blocks × 3 repeats on memory engine. Below 1.0× means Final is faster or allocates less.',
                groups, [0.6, 0.7, 0.8, 0.9, 1.0, 1.1], 'Final lower', 'Before lower',
                summary='\n'.join([
                    'Allocations (final/before): allocs/op drops on all 8 benchmarks (0.627×–0.888×, led by ScanIndexRecords/pipelined 0.627× and ScanIndexRecords/serial 0.651×); B/op drops on 7 of 8 (0.749×–0.910×) and is unresolved on 3-chunk SaveSplitRecord (0.998× [0.993, 1.004]).',
                    'Mean time (ns/op, final/before): Lower on 3 of 8 benchmarks (ScanIndexRecords/pipelined 0.972× [0.952, 0.995], ScanRecordsSplitVersioned 0.976× [0.957, 0.996], LoadRecord 0.978× [0.958, 0.994]) and unresolved on the other 5 (0.966×–1.015×).',
                    'Against pre-#888 legacy (see table below): ScanIndexRecords/serial is 1.477× (48% slower) and LoadRecord is 1.138× (14% slower) while ScanIndexRecords/pipelined takes 0.647× of legacy time (35% faster).',
                ]),
            )
            verify_summary_numbers(fname, res, raw_md)
            return res

    if cid == 'hcloud-rebench-20261014' and fname == 'phase-c1-remotefetch.md':
        rows = extract_md_table(raw_md, '### Remote fetch vs ordinary fetch, final tree')
        if rows:
            groups = [
                ('Mean Time (ns/op): final-remote / final-ordinary', [(r[0], r[1]) for r in rows]),
                ('Go Bytes (B/op): final-remote / final-ordinary', [(r[0], r[2]) for r in rows]),
                ('Go Allocations (allocs/op): final-remote / final-ordinary', [(r[0], r[3]) for r in rows]),
            ]
            res = render_forest_svg(
                'Figure C1.1: Forward Remote Fetch vs Ordinary Scan — Final Tree (log₂ scale, 95% CI)',
                '8 Williams-square blocks × 3 repeats on single-node memory engine. Below 1.0× favors USE_REMOTE_FETCH.',
                groups, [0.3, 0.5, 0.7, 1.0, 1.3], 'USE_REMOTE_FETCH lower', 'SCAN_AND_FETCH lower',
                summary='\n'.join([
                    'Wins (5/6 cases on ns/op, 95% CI < 1.000): small-all-1000 0.341× [0.321, 0.356] (15.3 ms vs 44.0 ms), split-limit-1-broad 0.707× [0.685, 0.735], small-exact-10 0.733× [0.724, 0.743], small-limit-1-broad 0.838× [0.795, 0.900], and split-exact-10 0.872× [0.843, 0.904].',
                    'Regressions (1/6 on ns/op, 95% CI > 1.000): 2-chunk split-all-1000 is 1.150× [1.129, 1.181] slower (188 ms vs 163 ms).',
                    'Allocations: B/op drops on all 6 cases (0.512×–0.960×); allocs/op drops on 5 of 6 (0.404×–0.942×) and increases on small-limit-1-broad (1.046× [1.046, 1.046], 229 vs 219).',
                ]),
            )
            verify_summary_numbers(fname, res, raw_md)
            return res

    if cid == 'hcloud-rebench-20261014' and fname in ('phase-c2-reverse-skip.md', 'phase-c3-synthetic-rtt.md', 'phase-d1-sql.md', 'phase-d2-sql-remotefetch.md'):
        rows = extract_md_table(raw_md, '## Results')
        valid = [r for r in rows if len(r) >= 4 and parse_ci(r[1])]
        if valid and fname != 'phase-d1-sql.md':
            groups = [
                ('Mean Time (ns/op): USE_REMOTE_FETCH / SCAN_AND_FETCH', [(r[0], r[1]) for r in valid]),
                ('Go Bytes (B/op): USE_REMOTE_FETCH / SCAN_AND_FETCH', [(r[0], r[2]) for r in valid]),
                ('Go Allocations (allocs/op): USE_REMOTE_FETCH / SCAN_AND_FETCH', [(r[0], r[3]) for r in valid]),
            ]
            summaries = {
                'phase-c2-reverse-skip.md': '\n'.join([
                    'Wins (3/4 cases on ns/op, 95% CI < 1.000): reverse-small-1000 0.337× [0.331, 0.343] (15.6 ms vs 46.5 ms), executor-skip3-small-1000 0.455× [0.445, 0.465] (21.4 ms vs 47.5 ms), and executor-skip3-small-10 0.783× [0.764, 0.800] (1.46 ms vs 1.87 ms).',
                    'Regressions (1/4 on ns/op, 95% CI > 1.000): reverse-split-1000 is 1.146× [1.130, 1.161] slower (188 ms vs 165 ms).',
                    'Allocations: Client B/op (0.506×–0.692×) and allocs/op (0.403×–0.791×) drop across all 4 cases.',
                ]),
                'phase-c3-synthetic-rtt.md': '\n'.join([
                    'Wins (1/2 cases, 0.5 ms nominal added client↔server RTT): small-all-1000 ns/op is 0.224× [0.222, 0.226] (24.0 ms vs 108.0 ms), with B/op 0.524× and allocs/op 0.439×.',
                    'Regressions (1/2 cases): 2-chunk split-all-1000 ns/op is 1.053× [1.008, 1.101] slower (276.9 ms vs 266.4 ms) despite B/op 0.692× and allocs/op 0.406×.',
                    'Scope: Single-fdbserver memory setup where index and primary records reside in one process; only client↔server traffic crosses the delayed veth link.',
                ]),
                'phase-d2-sql-remotefetch.md': '\n'.join([
                    'Wins (2/2 SQL cases on single-node memory): rows10 ns/op is 0.885× [0.873, 0.896] (2.22 ms vs 2.50 ms) and rows1000 ns/op is 0.536× [0.532, 0.538] (24.9 ms vs 46.5 ms), with lower client B/op (0.843×, 0.657×) and allocs/op (0.850×, 0.614×).',
                    'Confound flag: Both cases flagged a 5-vs-6-byte store-prefix length difference in 1 of 6 rows10 blocks and 2 of 6 rows1000 blocks (a post-hoc descriptive check on equal-prefix blocks gives 0.884 and 0.534).',
                ]),
            }
            res = render_forest_svg(
                f'Figure {fname.split("-")[1].upper()}.1: Remote Fetch vs Ordinary Scan Ratios (log₂ scale, 95% CI)',
                '6 paired blocks × 3 repeats on single-node memory engine. Below 1.0× favors USE_REMOTE_FETCH.',
                groups, [0.2, 0.35, 0.5, 0.7, 1.0, 1.3], 'USE_REMOTE_FETCH lower', 'SCAN_AND_FETCH lower',
                summary=summaries.get(fname, ''),
            )
            verify_summary_numbers(fname, res, raw_md)
            return res
        elif fname == 'phase-d1-sql.md':
            r_bef = extract_md_table(raw_md, '### Final vs before')
            r_leg = extract_md_table(raw_md, '### Final vs legacy')
            if r_bef and r_leg:
                groups = [
                    ('Final / Before (PlanCacheHit)', [('ns/op', r_bef[0][1]), ('B/op', r_bef[0][2]), ('allocs/op', r_bef[0][3])]),
                    ('Final / Legacy (PlanCacheHit)', [('ns/op', r_leg[0][1]), ('B/op', r_leg[0][2]), ('allocs/op', r_leg[0][3])]),
                ]
                res = render_forest_svg(
                    'Figure D1.1: SQL FDB_PlanCacheHit Ratios (log₂ scale, 95% CI)',
                    '6 paired blocks × 3 repeats (54 samples). Below 1.0× favors Final.',
                    groups, [0.9, 0.95, 1.0, 1.05, 1.1], 'Final lower', 'Denominator lower',
                    summary='\n'.join([
                        'Scope: Only 1 of 3 planned SQL cases (PlanCacheHit, 18 processes / 54 samples) completed; IndexScanRange stopped after block 0 on a false runner check and TimestampInsert was not run.',
                        'Results (PlanCacheHit): Final reduces allocs/op to 0.949× [0.949, 0.949] (687 → 652) and B/op to 0.966× (48,245.5 → 46,615 B vs before; 48,249.5 → 46,615 B vs legacy). ns/op is unresolved in both comparisons (1.002× [0.984, 1.021] vs before, 0.999× [0.981, 1.018] vs legacy; medians 1.153 ms vs 1.144 ms and 1.148 ms).',
                    ]),
                )
                verify_summary_numbers(fname, res, raw_md)
                return res

    return ''


def transform_body(text: str, is_index: bool, cid: str = '', fname: str = '') -> str:
    # Strip the top-level H1 heading because Hugo renders `title` from front matter.
    lines = text.splitlines()
    if lines and lines[0].startswith('# '):
        lines = lines[1:]
        while lines and not lines[0].strip():
            lines.pop(0)
    body = '\n'.join(lines) + '\n'

    # Inject precision SVG charts right before the Results section on phase pages.
    if not is_index and cid and fname:
        charts_svg = build_page_charts(cid, fname, text)
        if charts_svg:
            body = re.sub(
                r'^(## Results[^\n]*)$',
                lambda m: m.group(1) + '\n\n' + charts_svg,
                body,
                count=1,
                flags=re.M,
            )

    # Rewrite relative repo links that escape docs/benchmarks/<campaign>/ to GitHub URLs.
    body = body.replace('(../campaign-harness.md)', '(https://github.com/birdayz/fdb-go/blob/master/docs/benchmarks/campaign-harness.md)')
    body = body.replace('(../../../tools/benchcampaign/singlenode/README.md)', '(https://github.com/birdayz/fdb-go/blob/master/tools/benchcampaign/singlenode/README.md)')
    body = body.replace('(../../../tools/benchcampaign/multinode/README.md)', '(https://github.com/birdayz/fdb-go/blob/master/tools/benchcampaign/multinode/README.md)')

    # Rewrite cross-campaign and sibling `.md` links to Hugo page paths.
    body = re.sub(r'\(\.\./([a-z0-9-]+)/README\.md\)', r'(/docs/benchmarks/\1/)', body)
    body = re.sub(r'\(\.\./([a-z0-9-]+)/([a-z0-9-]+)\.md\)', r'(/docs/benchmarks/\1/\2/)', body)
    body = body.replace('(README.md)', '(../)')
    if is_index:
        body = re.sub(r'\((phase-[a-z0-9-]+)\.md\)', r'(\1/)', body)
    else:
        body = re.sub(r'\((phase-[a-z0-9-]+)\.md\)', r'(../\1/)', body)
    return body


FORBIDDEN_RE = re.compile(
    r'\b(?:10\.\d{1,3}\.\d{1,3}\.\d{1,3}|172\.(?:1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3}|192\.168\.\d{1,3}\.\d{1,3}|167\.233\.\d+\.\d+|91\.98\.\d+\.\d+|2\.28\.\d+\.\d+)\b'
    r'|analysis-seed391706|/home/birdy|/srv/fdb-bench'
)


def check_text(path: Path, content: str):
    m = FORBIDDEN_RE.search(content)
    if m:
        raise SystemExit(f'{path}: forbidden literal {m.group(0)!r} in website benchmark content')


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--check', action='store_true', help='verify generated files match on-disk files without writing')
    args = ap.parse_args()

    expected = {}
    for cid, title, desc, weight, children in CAMPAIGNS:
        src_dir = SRC_ROOT / cid
        dst_dir = DST_ROOT / cid

        readme_body = transform_body((src_dir / 'README.md').read_text(), is_index=True, cid=cid, fname='README.md')
        index_md = f'---\ntitle: "{title}"\ndescription: "{desc}"\nweight: {weight}\n---\n\n{readme_body}'
        expected[dst_dir / '_index.md'] = index_md

        for fname, ctitle, cdesc, cweight in children:
            cbody = transform_body((src_dir / fname).read_text(), is_index=False, cid=cid, fname=fname)
            page_md = f'---\ntitle: "{ctitle}"\ndescription: "{cdesc}"\nweight: {cweight}\n---\n\n{cbody}'
            expected[dst_dir / fname] = page_md

    landing = DST_ROOT / '_index.md'
    if landing.is_file():
        check_text(landing, landing.read_text())
    for path, content in expected.items():
        check_text(path, content)

    if args.check:
        for path, content in expected.items():
            if not path.is_file() or path.read_text() != content:
                raise SystemExit(f'{path}: out of sync with docs/benchmarks/; run python3 scripts/sync-website-benchmarks.py')
        return

    for path, content in expected.items():
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content)


if __name__ == '__main__':
    main()
