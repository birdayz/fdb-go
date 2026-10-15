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


def render_forest_svg(title: str, subtitle: str, groups: list, ticks: list, left_label: str, right_label: str) -> str:
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
        f'<svg class="bench-svg" viewBox="0 0 {W} {H}" role="img" aria-label="{html.escape(title)}">',
    ]

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


def render_grouped_bars_svg(title: str, subtitle: str, items: list, unit: str, max_val: float, ticks: list) -> str:
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
        f'<svg class="bench-svg" viewBox="0 0 {W} {H}" role="img" aria-label="{html.escape(title)}">',
    ]
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
            mean_groups, [0.35, 0.5, 0.7, 1.0, 1.4, 2.0], 'Pure-Go faster', 'CGo faster'
        )
        c2 = render_forest_svg(
            'Figure M1.2: Tail (p99) Latency Ratio — Final Pure-Go / CGo (log₂ scale, 95% block-bootstrap CI)',
            'Run-level p99 transaction latency resampled over paired blocks. Below 1.0× means Pure-Go has lower p99.',
            p99_groups, [0.25, 0.35, 0.5, 0.7, 1.0, 1.4, 2.0], 'Pure-Go lower p99', 'CGo lower p99'
        )
        c3 = render_forest_svg(
            'Figure M1.3: Shutdown Process CPU (E−A) Ratio — Final Pure-Go / CGo (log₂ scale, 95% CI)',
            'User+system process CPU through finalizer drain and network stop per transaction. Below 1.0× means Pure-Go uses less CPU.',
            cpu_groups, [0.5, 0.7, 1.0, 1.4, 2.0, 2.5], 'Pure-Go less CPU', 'CGo less CPU'
        )
        c4 = render_grouped_bars_svg(
            'Figure M1.4: Absolute Mean & p99 Latency at 1 Worker (M1w1) and 64 Workers (M1w64)',
            'Medians of 6 run-level statistics in µs/transaction (solid bar = mean, whisker = p99).',
            abs_items, 'µs', 12000.0, [0, 2000, 4000, 6000, 8000, 10000, 12000]
        )
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
            ns_groups, [0.15, 0.25, 0.5, 0.7, 1.0, 1.4, 2.0, 2.5], 'Remote fetch faster', 'SCAN_AND_FETCH faster'
        )
        c2 = render_forest_svg(
            'Figure M2.2: Client Go Allocations (allocs/op) Ratio — USE_REMOTE_FETCH / SCAN_AND_FETCH',
            'Below 1.0× means USE_REMOTE_FETCH allocates fewer Go heap objects on the client.',
            al_groups, [0.35, 0.5, 0.7, 1.0, 1.2], 'Remote fetch fewer allocs', 'SCAN_AND_FETCH fewer allocs'
        )
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
            groups, [0.15, 0.25, 0.5, 0.7, 1.0, 1.4], 'Pure-Go faster / lower CPU', 'CGo faster / lower CPU'
        )
        c2 = render_forest_svg(
            'Figure A.2: Allocation Reductions — Final / Before Pure-Go (3824f146 → 267a4143)',
            'Go heap bytes and mallocs per transaction (6 paired blocks). Below 1.0× means fewer allocations in Final.',
            alloc_groups, [0.7, 0.8, 0.9, 1.0, 1.1], 'Final allocates less', 'Before allocates less'
        )
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
        return render_forest_svg(
            'Figure A2.1: Single-Node 8-Worker Client Ratios (log₂ scale, 95% block-bootstrap CI)',
            '6 paired blocks × 24,000 tx/run (144,000 tx/arm/workload). Below 1.0× favors Final Pure-Go.',
            groups, [0.4, 0.6, 0.8, 1.0, 1.3, 1.7], 'Final Pure-Go lower', 'Denominator lower'
        )

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
            return render_forest_svg(
                'Figure B.1: Record Layer Allocation Optimization Ratios — Final / Before (log₂ scale, 95% CI)',
                '6 paired blocks × 3 repeats on memory engine. Below 1.0× means Final is faster or allocates less.',
                groups, [0.6, 0.7, 0.8, 0.9, 1.0, 1.1], 'Final lower', 'Before lower'
            )

    if cid == 'hcloud-rebench-20261014' and fname == 'phase-c1-remotefetch.md':
        rows = extract_md_table(raw_md, '### Remote fetch vs ordinary fetch, final tree')
        if rows:
            groups = [
                ('Mean Time (ns/op): final-remote / final-ordinary', [(r[0], r[1]) for r in rows]),
                ('Go Bytes (B/op): final-remote / final-ordinary', [(r[0], r[2]) for r in rows]),
                ('Go Allocations (allocs/op): final-remote / final-ordinary', [(r[0], r[3]) for r in rows]),
            ]
            return render_forest_svg(
                'Figure C1.1: Forward Remote Fetch vs Ordinary Scan — Final Tree (log₂ scale, 95% CI)',
                '8 Williams-square blocks × 3 repeats on single-node memory engine. Below 1.0× favors USE_REMOTE_FETCH.',
                groups, [0.3, 0.5, 0.7, 1.0, 1.3], 'USE_REMOTE_FETCH lower', 'SCAN_AND_FETCH lower'
            )

    if cid == 'hcloud-rebench-20261014' and fname in ('phase-c2-reverse-skip.md', 'phase-c3-synthetic-rtt.md', 'phase-d1-sql.md', 'phase-d2-sql-remotefetch.md'):
        rows = extract_md_table(raw_md, '## Results')
        valid = [r for r in rows if len(r) >= 4 and parse_ci(r[1])]
        if valid and fname != 'phase-d1-sql.md':
            groups = [
                ('Mean Time (ns/op): USE_REMOTE_FETCH / SCAN_AND_FETCH', [(r[0], r[1]) for r in valid]),
                ('Go Bytes (B/op): USE_REMOTE_FETCH / SCAN_AND_FETCH', [(r[0], r[2]) for r in valid]),
                ('Go Allocations (allocs/op): USE_REMOTE_FETCH / SCAN_AND_FETCH', [(r[0], r[3]) for r in valid]),
            ]
            return render_forest_svg(
                f'Figure {fname.split("-")[1].upper()}.1: Remote Fetch vs Ordinary Scan Ratios (log₂ scale, 95% CI)',
                '6 paired blocks × 3 repeats on single-node memory engine. Below 1.0× favors USE_REMOTE_FETCH.',
                groups, [0.2, 0.35, 0.5, 0.7, 1.0, 1.3], 'USE_REMOTE_FETCH lower', 'SCAN_AND_FETCH lower'
            )
        elif fname == 'phase-d1-sql.md':
            r_bef = extract_md_table(raw_md, '### Final vs before')
            r_leg = extract_md_table(raw_md, '### Final vs legacy')
            if r_bef and r_leg:
                groups = [
                    ('Final / Before (PlanCacheHit)', [('ns/op', r_bef[0][1]), ('B/op', r_bef[0][2]), ('allocs/op', r_bef[0][3])]),
                    ('Final / Legacy (PlanCacheHit)', [('ns/op', r_leg[0][1]), ('B/op', r_leg[0][2]), ('allocs/op', r_leg[0][3])]),
                ]
                return render_forest_svg(
                    'Figure D1.1: SQL FDB_PlanCacheHit Ratios (log₂ scale, 95% CI)',
                    '6 paired blocks × 3 repeats (54 samples). Below 1.0× favors Final.',
                    groups, [0.9, 0.95, 1.0, 1.05, 1.1], 'Final lower', 'Denominator lower'
                )

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
