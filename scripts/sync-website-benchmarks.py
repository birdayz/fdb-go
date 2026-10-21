#!/usr/bin/env python3
"""Sync the benchmark campaigns listed in CAMPAIGNS from docs/benchmarks/<campaign>/
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
import posixpath
import re
import sys

REPO = Path(__file__).resolve().parent.parent
SRC_ROOT = REPO / 'docs/benchmarks'
DST_ROOT = REPO / 'website/content/docs/benchmarks'

CAMPAIGNS = [
    (
        'fdbmn-20261020',
        '4-node VPC campaign, master (2026-10-20)',
        'Preregistered repeat of the 4-host VPC campaign on master fa8725f1d: master vs old Go (267a41434) vs CGo (libfdb_c 7.3.77) at 1, 8, and 64 workers, and USE_REMOTE_FETCH vs SCAN_AND_FETCH across 9 storage servers.',
        1,
        [
            ('phase-m1-client.md', 'Phase M1: Client (1, 8, 64 workers)', 'Master vs old Go vs CGo client latency, lifecycle CPU, and Go allocations at 1, 8, and 64 closed-loop workers over a real VPC to a 3-node double ssd-2 cluster.', 1),
            ('phase-m2-remotefetch.md', 'Phase M2: Remote fetch (3-node cluster)', 'Record Layer USE_REMOTE_FETCH vs SCAN_AND_FETCH on master across 9 storage servers, with the 2026-10-14 cohort for comparison.', 2),
        ],
    ),
    (
        'hcloud-multinode-vpc-20261014',
        '4-node VPC campaign (2026-10-14)',
        'Preregistered 4-host VPC benchmark campaign (1 client VM + 3 FDB 7.3.77 server VMs, double ssd-2, 9 storage servers): pure-Go vs CGo at 1, 8, 32, and 64 workers and cross-server USE_REMOTE_FETCH vs SCAN_AND_FETCH.',
        2,
        [
            ('phase-m1-client.md', 'Phase M1: Client (1, 8, 32, 64 workers)', 'Pure-Go vs CGo client latency and process CPU across 1, 8, 32, and 64 closed-loop workers over a real VPC to a 3-node double ssd-2 cluster.', 1),
            ('phase-m2-remotefetch.md', 'Phase M2: Remote fetch (3-node cluster)', 'Record Layer USE_REMOTE_FETCH vs SCAN_AND_FETCH across 9 storage servers on a 3-node VPC cluster with verified shard placement.', 2),
        ],
    ),
    (
        'hcloud-rebench-20261014',
        'Single-node 16-vCPU campaign (2026-10-14)',
        'Preregistered 8-phase benchmark campaign on a dedicated 16-vCPU host comparing the pure-Go client, Record Layer, mapped remote fetch, and SQL across the RFC-258 trees.',
        3,
        [
            ('phase-a-client.md', 'Phase A: Client (1 worker)', 'Pure-Go client vs CGo (libfdb_c 7.3.77) and vs pre-allocation pure-Go across 11 workloads at 1 closed-loop worker.', 1),
            ('phase-a2-client-8w.md', 'Phase A2: Client (8 workers)', 'Pure-Go client vs CGo and vs pre-allocation pure-Go at 8 concurrent closed-loop workers.', 2),
            ('phase-b-recordlayer.md', 'Phase B: Record Layer', 'Record Layer benchmarks across legacy, before-allocation, and final trees.', 3),
            ('phase-c1-remotefetch.md', 'Phase C1: Remote fetch (forward)', 'Forward USE_REMOTE_FETCH vs SCAN_AND_FETCH across pr2 and final trees on small and two-chunk split records.', 4),
            ('phase-c2-reverse-skip.md', 'Phase C2: Remote fetch (reverse and Skip)', 'Reverse scans and executor Skip 3 paths with USE_REMOTE_FETCH vs SCAN_AND_FETCH.', 5),
            ('phase-c3-synthetic-rtt.md', 'Phase C3: Remote fetch (0.5 ms synthetic RTT)', 'Sensitivity of USE_REMOTE_FETCH vs SCAN_AND_FETCH to 0.5 ms nominal synthetic veth RTT.', 6),
            ('phase-d1-sql.md', 'Phase D1: SQL plan-cache point select', 'SQL plan-cache point select across legacy, before, and final trees.', 7),
            ('phase-d2-sql-remotefetch.md', 'Phase D2: SQL remote fetch', 'SQL non-covering index scan with USE_REMOTE_FETCH vs SCAN_AND_FETCH.', 8),
        ],
    ),
]

CI_RE = re.compile(r'([0-9]+\.[0-9]+)\s*\[\s*([0-9]+\.[0-9]+)\s*,\s*([0-9]+\.[0-9]+)\s*\]')


def parse_ci(cell: str):
    m = CI_RE.search(cell)
    if not m:
        return None
    return float(m.group(1)), float(m.group(2)), float(m.group(3))


def ci_class(pt: float, lo: float, hi: float, higher_is_better: bool = False) -> str:
    if hi < 1.0:
        return 'ci-reg' if higher_is_better else 'ci-win'
    if lo > 1.0:
        return 'ci-win' if higher_is_better else 'ci-reg'
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
    m = re.search(r'<p class="bench-summary">(.*?)</p>', svg_or_summary, flags=re.S)
    summary = m.group(1) if m else svg_or_summary
    md_decimals = set(re.findall(r'\b\d+\.\d+\b', raw_md)) | {'1.0', '1.000'}
    for tok in re.findall(r'\b\d+\.\d+\b', summary):
        if tok not in md_decimals:
            raise ValueError(f'{title}: summary number {tok!r} not found as token in canonical markdown')


def render_summary_html(summary: str) -> str:
    if not summary:
        return ''
    return f'<p class="bench-summary">{html.escape(summary)}</p>'


def render_forest_svg(title: str, subtitle: str, groups: list, ticks: list, left_label: str, right_label: str, summary: str = '', higher_is_better: bool = False) -> str:
    """Render a log2-scaled forest plot SVG with exact 95% CI whiskers and right-aligned numeric column.

    higher_is_better colors intervals above 1.0 as favorable (e.g. throughput ratios).
    """
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
        f'<figcaption><strong>{html.escape(title)}</strong><span>{html.escape(subtitle)}</span></figcaption>',
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
                cls = ci_class(pt, lo, hi, higher_is_better)
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

    favorable, unfavorable = ('above', 'below') if higher_is_better else ('below', 'above')
    out.extend([
        '</svg>',
        '<div class="bench-legend">'
        f'<span><i class="bench-swatch" style="background:#0d9488"></i> 95% CI {favorable} 1.0×</span>'
        '<span><i class="bench-swatch" style="background:#64748b"></i> 95% CI includes 1.0× (inconclusive)</span>'
        f'<span><i class="bench-swatch" style="background:#d97706"></i> 95% CI {unfavorable} 1.0×</span>'
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
        f'<figcaption><strong>{html.escape(title)}</strong><span>{html.escape(subtitle)}</span></figcaption>',
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
        '<span><i class="bench-swatch" style="background:#0d9488"></i> Pure-Go</span>'
        '<span><i class="bench-swatch" style="background:#d97706"></i> CGo (libfdb_c 7.3.77)</span>'
        '</div>',
        '</figure>',
    ])
    return '\n'.join(out)


def render_arm_bars_svg(title: str, subtitle: str, items: list, arms: list, unit: str, max_val: float, ticks: list, summary: str = '') -> str:
    """Render horizontal bars for several arms per row: bar to p50, whisker to p99.

    items: [(label, [(p50, p99) per arm])]; arms: [(name, bar_class, stroke_class, swatch_color)].
    """
    max_label_chars = max((len(lbl) for lbl, _ in items), default=16)
    pad_l = max(165, int(max_label_chars * 7.8) + 26)
    pad_r = 230
    plot_w = 960
    W = pad_l + plot_w + pad_r
    bar_h = 9
    bar_step = 12
    row_h = len(arms) * bar_step + 8
    pad_t = 28
    pad_b = 28
    H = pad_t + len(items) * row_h + pad_b

    def x_of(v: float) -> float:
        if not (0.0 <= v <= max_val):
            raise ValueError(f'{title}: value {v} falls outside [0, {max_val}]')
        return pad_l + (v / max_val) * plot_w

    out = [
        '<figure class="bench-figure">',
        f'<figcaption><strong>{html.escape(title)}</strong><span>{html.escape(subtitle)}</span></figcaption>',
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
    for label, values in items:
        if len(values) != len(arms):
            raise ValueError(f'{title}: {label} has {len(values)} values for {len(arms)} arms')
        mid = y + (len(arms) * bar_step) / 2
        out.append(f'<text class="row-label" x="{pad_l - 12}" y="{mid + 4:.1f}" text-anchor="end">{html.escape(label)}</text>')
        for i, ((name, bar_cls, stroke_cls, _), (p50, p99)) in enumerate(zip(arms, values)):
            by = y + 4 + i * bar_step
            cy = by + bar_h / 2
            w = max(1.5, x_of(p50) - pad_l)
            x99 = x_of(p99)
            out.append(
                f'<g class="{stroke_cls}"><title>{html.escape(label)} {html.escape(name)}: p50 {p50:.1f} {unit}, p99 {p99:.1f} {unit}</title>'
                f'<rect class="{bar_cls}" x="{pad_l}" y="{by}" width="{w:.1f}" height="{bar_h}" rx="2"/>'
                f'<line x1="{pad_l + w:.1f}" y1="{cy:.1f}" x2="{x99:.1f}" y2="{cy:.1f}" stroke-width="1.6"/>'
                f'<line x1="{x99:.1f}" y1="{cy - 4:.1f}" x2="{x99:.1f}" y2="{cy + 4:.1f}" stroke-width="1.8"/></g>'
            )
            cls = 'val-text' if i == 0 else 'axis-text'
            out.append(f'<text class="{cls}" x="{W - pad_r + 10}" y="{by + bar_h:.1f}">{html.escape(name)} {p50:.0f} / p99 {p99:.0f} {html.escape(unit)}</text>')
        y += row_h

    legend = ''.join(f'<span><i class="bench-swatch" style="background:{color}"></i> {html.escape(name)}</span>' for name, _, _, color in arms)
    out.extend([
        '</svg>',
        f'<div class="bench-legend">{legend}</div>',
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


def build_section_charts(cid: str, fname: str, raw_md: str) -> list:
    """Charts injected after headings other than ## Results: [(heading line prefix, svg)]."""
    if cid == 'fdbmn-20261020' and fname == 'phase-m1-client.md':
        heading = '## Throughput (post-hoc, exploratory)'
        if heading not in raw_md:
            return []
        groups = []
        for contrast, col in (('master / old Go', 2), ('master / CGo', 3)):
            for ph, label in (('M1w1', '1 worker'), ('M1w8', '8 workers'), ('M1w64', '64 workers')):
                rows = extract_md_table(raw_md, f'#### `{ph}`: throughput')
                if not rows:
                    raise ValueError(f'{fname}: missing table #### `{ph}`: throughput')
                groups.append((f'{contrast}: {ph} ({label})', [(r[0], r[col]) for r in rows if parse_ci(r[col])]))
        c = render_forest_svg(
            'Figure M1.5: Throughput, operations per second (post-hoc)',
            'Above 1.0× means master completes more operations per second. Post-hoc, not preregistered.',
            groups, [0.4, 0.7, 1.0, 1.4, 2.0], 'Denominator more ops/s', 'Master more ops/s',
            summary='At 8 and 64 workers master completes fewer operations per second than old Go on get, range and set (0.718–0.903), and more on grv at 64 workers (1.057). Against CGo its rate is higher or the interval includes 1.0 in every cell except grv at 8 workers (0.979).',
            higher_is_better=True,
        )
        verify_summary_numbers(fname, c, raw_md)
        return [(heading, c)]
    return []


def build_page_charts(cid: str, fname: str, raw_md: str) -> str:
    """Build precision SVG charts directly from the exact numbers in raw_md."""
    if cid == 'fdbmn-20261020' and fname == 'phase-m1-client.md':
        phases = [
            ('M1w1', 'M1w1 (1 worker, 4/7 clean)'),
            ('M1w8', 'M1w8 (8 workers, 5/7 clean)'),
            ('M1w64', 'M1w64 (64 workers, 5/7 clean)'),
        ]

        def rows_of(ph: str, contrast: str, col: int) -> list:
            rows = extract_md_table(raw_md, f'#### `{ph}`: {contrast}')
            if not rows:
                raise ValueError(f'{fname}: missing table #### `{ph}`: {contrast}')
            return [(r[0], r[col] if parse_ci(r[col]) else 'N/A (excluded)') for r in rows]

        p50_old = [(label, rows_of(ph, 'master / old Go', 2)) for ph, label in phases]
        p50_cgo = [(label, rows_of(ph, 'master / CGo', 2)) for ph, label in phases]
        cpu = [(f'master / old Go: {label}', rows_of(ph, 'master / old Go', 4)) for ph, label in phases]
        cpu += [(f'master / CGo: {label}', rows_of(ph, 'master / CGo', 4)) for ph, label in phases]

        abs_rows = extract_md_table(raw_md, '## Absolute values')
        by_key = {(r[0], r[1], r[2]): r for r in abs_rows if len(r) >= 7}
        arm_names = ['master', 'old Go', 'CGo']
        abs_items = []
        for ph in ('M1w8', 'M1w64'):
            for w in ('grv', 'get100', 'get1000', 'range100', 'set100'):
                vals = []
                for a in arm_names:
                    r = by_key.get((ph, w, a))
                    if r is None:
                        raise ValueError(f'{fname}: missing absolute row {ph} {w} {a}')
                    vals.append((float(r[5]), float(r[6])))
                abs_items.append((f'{ph} {w}', vals))

        c1 = render_forest_svg(
            'Figure M1.1: p50 latency, master / old Go',
            'Below 1.0× means master has lower p50.',
            p50_old, [0.7, 1.0, 1.4, 2.0, 2.5], 'Master lower', 'Old Go lower',
            summary='At 8 and 64 workers master has higher p50 than old Go on every get, range and set workload, from 1.112 to 1.435; grv p50 is lower (0.941 and 0.923). At 1 worker master p50 is 1.049–1.091.',
        )
        c2 = render_forest_svg(
            'Figure M1.2: p50 latency, master / CGo',
            'Below 1.0× means master has lower p50 than libfdb_c 7.3.77.',
            p50_cgo, [0.5, 0.7, 1.0, 1.4, 2.0], 'Master lower', 'CGo lower',
            summary='Master p50 is below CGo at 1 worker (0.636–0.697) and at 64 workers for get100, get1000 and set100 (0.770–0.934). At 8 workers the get1000 and set100 intervals include 1.0, and grv is above it (1.025).',
        )
        c3 = render_forest_svg(
            'Figure M1.3: Lifecycle CPU (E−A) per transaction',
            'Below 1.0× means master uses less process CPU. Includes startup, warmup, validation, and shutdown.',
            cpu, [0.5, 0.7, 1.0, 1.4, 2.0], 'Master less CPU', 'Denominator less CPU',
            summary='Master uses less CPU than old Go in every 8- and 64-worker cell except range100 at 64 workers, down to 0.553 (set100, 8 workers), and more at 1 worker (1.125–1.292). Against CGo it uses more on get, range and set at 8 and 64 workers (1.037–1.354) and less on grv.',
        )
        c4 = render_arm_bars_svg(
            'Figure M1.4: p50 and p99 latency at 8 and 64 workers',
            'Bar is p50, whisker is p99, in µs per transaction (medians of six runs).',
            abs_items,
            [('master', 'bar-go', 'ci-win', '#0d9488'), ('old Go', 'bar-before', 'ci-neutral', '#64748b'), ('CGo', 'bar-cgo', 'ci-reg', '#d97706')],
            'µs', 14000.0, [0, 2000, 4000, 6000, 8000, 10000, 12000, 14000],
            summary='Each worker count ran as a separate phase with its own transaction count, so compare arms within a count.',
        )
        for c in (c1, c2, c3, c4):
            verify_summary_numbers(fname, c, raw_md)
        return '\n\n'.join([c1, c2, c3, c4])

    if cid == 'fdbmn-20261020' and fname == 'phase-m2-remotefetch.md':
        rl_rows = extract_md_table(raw_md, '### `M2rl`')
        sk_rows = extract_md_table(raw_md, '### `M2skip`')
        if not rl_rows or not sk_rows:
            raise ValueError(f'{fname}: missing M2 result tables')

        def place(p: str) -> str:
            m = re.search(r'f = ([0-9.]+)', p)
            return f'f={m.group(1)}' if m else p

        def cell(c: str) -> str:
            return c if parse_ci(c) else 'N/A (excluded)'
        ns_groups = [
            ('M2rl: BenchmarkIndexFetch', [(f'{r[0]} [{place(r[1])}]', cell(r[2])) for r in rl_rows]),
            ('M2skip: BenchmarkPlanSkip', [(f'{r[0]} [{place(r[1])}]', cell(r[2])) for r in sk_rows]),
        ]
        al_groups = [
            ('M2rl: client Go allocs/op', [(r[0], cell(r[4])) for r in rl_rows]),
            ('M2skip: client Go allocs/op', [(r[0], cell(r[4])) for r in sk_rows]),
        ]
        c1 = render_forest_svg(
            'Figure M2.1: Time per operation, USE_REMOTE_FETCH / SCAN_AND_FETCH',
            'Below 1.0× means USE_REMOTE_FETCH is faster. 3-node cluster, 9 storage servers, master.',
            ns_groups, [0.7, 1.0, 1.4, 2.0, 2.5], 'USE_REMOTE_FETCH faster', 'SCAN_AND_FETCH faster',
            summary='USE_REMOTE_FETCH is slower on every 1,000-row scan (1.152–1.220 cross-server, 2.304 on the partially colocated f=0.820 split case), on split-rows10-reverse (1.326) and on skip3-rows10-reverse (1.169). It is faster on both one-row small fetches (0.976, 0.974). Four cases are excluded by the pre-arm client CPU gate.',
        )
        c2 = render_forest_svg(
            'Figure M2.2: Client allocations per operation, USE_REMOTE_FETCH / SCAN_AND_FETCH',
            'Below 1.0× means USE_REMOTE_FETCH allocates fewer Go objects.',
            al_groups, [0.35, 0.5, 0.7, 1.0, 1.2], 'USE_REMOTE_FETCH fewer allocs', 'SCAN_AND_FETCH fewer allocs',
            summary='USE_REMOTE_FETCH allocates fewer Go objects in 10 of 12 eligible cases, down to 0.454. The one-row small fetches allocate more (1.080, 1.090).',
        )
        for c in (c1, c2):
            verify_summary_numbers(fname, c, raw_md)
        return '\n\n'.join([c1, c2])

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
        abs_rows = extract_md_table(raw_md, '## Absolute values')
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
            'Figure M1.1: Mean latency, pure-Go / CGo',
            'Below 1.0× means pure-Go is faster.',
            mean_groups, [0.35, 0.5, 0.7, 1.0, 1.4, 2.0], 'Pure-Go faster', 'CGo faster',
            summary='Pure-Go has lower mean latency in 18 of 22 cells, down to 0.554×. CGo is faster in one: grv at 32 workers (1.041×). Three cells are inconclusive. There are no results for batchset50 and batchset50x1000 at 8–64 workers: the server\'s log queue exceeded the 100 MB gate during batchset50 and the run stopped.',
        )
        c2 = render_forest_svg(
            'Figure M1.2: p99 latency, pure-Go / CGo',
            'Below 1.0× means pure-Go has lower p99.',
            p99_groups, [0.25, 0.35, 0.5, 0.7, 1.0, 1.4, 2.0], 'Pure-Go lower p99', 'CGo lower p99',
            summary='Pure-Go has lower p99 latency in 17 of 22 cells, down to 0.385× (get1000, 64 workers). CGo is lower in one: range100 at 32 workers (1.247×). Four cells are inconclusive.',
        )
        c3 = render_forest_svg(
            'Figure M1.3: Process CPU per transaction, pure-Go / CGo',
            'Below 1.0× means pure-Go uses less CPU. Includes startup, GC, and shutdown (E−A).',
            cpu_groups, [0.5, 0.7, 1.0, 1.4, 2.0, 2.5], 'Pure-Go less CPU', 'CGo less CPU',
            summary='At 1 worker, pure-Go uses less CPU on all 7 workloads. At 8–64 workers it uses more CPU in 11 of the 12 cells other than grv, up to 2.162× (set100, 32 workers). grv still uses less.',
        )
        c4 = render_grouped_bars_svg(
            'Figure M1.4: Mean and p99 latency at 1 and 64 workers',
            'Bar is the mean, whisker is p99, in µs per transaction.',
            abs_items, 'µs', 12000.0, [0, 2000, 4000, 6000, 8000, 10000, 12000],
            summary='Mean latency is about 1–7 ms per transaction for both clients at both worker counts. Each worker count ran as a separate phase, so compare within a count.',
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
            ('M2rl: client Go allocs/op', [(r[0], r[4]) for r in rl_rows]),
            ('M2skip: client Go allocs/op', [(r[0], r[4]) for r in sk_rows]),
        ]
        c1 = render_forest_svg(
            'Figure M2.1: Time per operation, USE_REMOTE_FETCH / SCAN_AND_FETCH',
            'Below 1.0× means USE_REMOTE_FETCH is faster. 3-node cluster, 9 storage servers.',
            ns_groups, [0.15, 0.25, 0.5, 0.7, 1.0, 1.4, 2.0, 2.5], 'USE_REMOTE_FETCH faster', 'SCAN_AND_FETCH faster',
            summary='SCAN_AND_FETCH is faster in 10 of 16 cases, including all six 1,000-row scans (up to 2.326×). USE_REMOTE_FETCH is faster in 4, most clearly skip3-rows10-reverse (0.158×). Two are inconclusive.',
        )
        c2 = render_forest_svg(
            'Figure M2.2: Client allocations per operation, USE_REMOTE_FETCH / SCAN_AND_FETCH',
            'Below 1.0× means USE_REMOTE_FETCH allocates fewer Go objects.',
            al_groups, [0.35, 0.5, 0.7, 1.0, 1.2], 'USE_REMOTE_FETCH fewer allocs', 'SCAN_AND_FETCH fewer allocs',
            summary='USE_REMOTE_FETCH allocates less in 14 of 16 cases, down to 0.398×. That did not make the 1,000-row scans faster (Figure M2.1).',
        )
        for c in (c1, c2):
            verify_summary_numbers(fname, c, raw_md)
        return '\n\n'.join([c1, c2])

    if cid == 'hcloud-rebench-20261014' and fname == 'phase-a-client.md':
        cgo_rows = extract_md_table(raw_md, '### Final pure-Go vs CGo')
        bef_rows = extract_md_table(raw_md, '### Final vs before pure-Go')
        groups = [
            ('Mean latency: final pure-Go / CGo', [(r[0], r[1]) for r in cgo_rows]),
            ('p99 latency: final pure-Go / CGo', [(r[0], r[2]) for r in cgo_rows]),
            ('Shutdown process CPU (E−A): final pure-Go / CGo', [(r[0], r[3]) for r in cgo_rows]),
        ]
        alloc_groups = [
            ('Go bytes/tx: final / before', [(r[0], r[4]) for r in bef_rows]),
            ('Go mallocs/tx: final / before', [(r[0], r[5]) for r in bef_rows]),
        ]
        c1 = render_forest_svg(
            'Figure A.1: Client at 1 worker, pure-Go / CGo',
            'Below 1.0× favors pure-Go. Single host.',
            groups, [0.15, 0.25, 0.5, 0.7, 1.0, 1.4], 'Pure-Go faster / lower CPU', 'CGo faster / lower CPU',
            summary='Pure-Go has lower mean latency on 10 of 11 workloads, lower p99 on 8, and lower CPU on 7. The rest are inconclusive; none favors CGo.',
        )
        c2 = render_forest_svg(
            'Figure A.2: Allocations, final / before',
            'Below 1.0× means the final tree allocates less.',
            alloc_groups, [0.7, 0.8, 0.9, 1.0, 1.1], 'Final allocates less', 'Before allocates less',
            summary='The allocation work reduced mallocs per transaction on all 11 workloads, down to 0.797×. Mean latency did not change measurably.',
        )
        for c in (c1, c2):
            verify_summary_numbers(fname, c, raw_md)
        return '\n\n'.join([c1, c2])

    if cid == 'hcloud-rebench-20261014' and fname == 'phase-a2-client-8w.md':
        cgo_rows = extract_md_table(raw_md, '### Final pure-Go vs CGo (8 workers)')
        bef_rows = extract_md_table(raw_md, '### Final vs before pure-Go (8 workers)')
        groups = [
            ('Mean latency (8 workers): final pure-Go / CGo', [(r[0], r[1]) for r in cgo_rows]),
            ('p99 latency (8 workers): final pure-Go / CGo', [(r[0], r[2]) for r in cgo_rows]),
            ('Shutdown process CPU (E−A, 8 workers): final pure-Go / CGo', [(r[0], r[3]) for r in cgo_rows]),
            ('Go mallocs/tx (8 workers): final / before', [(r[0], r[5]) for r in bef_rows]),
        ]
        res = render_forest_svg(
            'Figure A2.1: Client at 8 workers',
            'Below 1.0× favors final pure-Go. Single host.',
            groups, [0.4, 0.6, 0.8, 1.0, 1.3, 1.7], 'Final pure-Go lower', 'Denominator lower',
            summary='Pure-Go has lower mean latency on range100 and batchset50; get100 is inconclusive. It uses more CPU than CGo on all three workloads, up to 1.638×.',
        )
        verify_summary_numbers(fname, res, raw_md)
        return res

    if cid == 'hcloud-rebench-20261014' and fname == 'phase-b-recordlayer.md':
        rows = extract_md_table(raw_md, '### Final vs before')
        if not rows:
            rows = extract_md_table(raw_md, '## Results')
        groups = [
            ('Mean time (ns/op): final / before', [(r[0], r[1]) for r in rows if len(r) >= 4]),
            ('Go bytes (B/op): final / before', [(r[0], r[2]) for r in rows if len(r) >= 4]),
            ('Go allocations (allocs/op): final / before', [(r[0], r[3]) for r in rows if len(r) >= 4]),
        ]
        if any(g[1] for g in groups):
            res = render_forest_svg(
                'Figure B.1: Record Layer, final / before',
                'Below 1.0× favors the final tree. Single in-memory server.',
                groups, [0.6, 0.7, 0.8, 0.9, 1.0, 1.1], 'Final lower', 'Before lower',
                summary='Allocations per operation dropped on all 8 benchmarks, down to 0.627×. Time improved by 2–3% on 3 benchmarks and is inconclusive on the rest.',
            )
            verify_summary_numbers(fname, res, raw_md)
            return res

    if cid == 'hcloud-rebench-20261014' and fname == 'phase-c1-remotefetch.md':
        rows = extract_md_table(raw_md, '### Remote fetch vs ordinary fetch, final tree')
        if rows:
            groups = [
                ('Mean time (ns/op): final-remote / final-ordinary', [(r[0], r[1]) for r in rows]),
                ('Go bytes (B/op): final-remote / final-ordinary', [(r[0], r[2]) for r in rows]),
                ('Go allocations (allocs/op): final-remote / final-ordinary', [(r[0], r[3]) for r in rows]),
            ]
            res = render_forest_svg(
                'Figure C1.1: Forward scans, USE_REMOTE_FETCH / SCAN_AND_FETCH',
                'Below 1.0× favors USE_REMOTE_FETCH. Single in-memory server.',
                groups, [0.3, 0.5, 0.7, 1.0, 1.3], 'USE_REMOTE_FETCH lower', 'SCAN_AND_FETCH lower',
                summary='USE_REMOTE_FETCH is faster in 5 of 6 cases, down to 0.341×, and slower on split-all-1000 (1.150×).',
            )
            verify_summary_numbers(fname, res, raw_md)
            return res

    if cid == 'hcloud-rebench-20261014' and fname in ('phase-c2-reverse-skip.md', 'phase-c3-synthetic-rtt.md', 'phase-d1-sql.md', 'phase-d2-sql-remotefetch.md'):
        rows = extract_md_table(raw_md, '## Results')
        valid = [r for r in rows if len(r) >= 4 and parse_ci(r[1])]
        if valid and fname != 'phase-d1-sql.md':
            groups = [
                ('Mean time (ns/op): USE_REMOTE_FETCH / SCAN_AND_FETCH', [(r[0], r[1]) for r in valid]),
                ('Go bytes (B/op): USE_REMOTE_FETCH / SCAN_AND_FETCH', [(r[0], r[2]) for r in valid]),
                ('Go allocations (allocs/op): USE_REMOTE_FETCH / SCAN_AND_FETCH', [(r[0], r[3]) for r in valid]),
            ]
            summaries = {
                'phase-c2-reverse-skip.md': 'USE_REMOTE_FETCH is faster in 3 of 4 cases, down to 0.337×, and slower on reverse-split-1000 (1.146×).',
                'phase-c3-synthetic-rtt.md': 'With 0.5 ms added between client and server, USE_REMOTE_FETCH is 0.224× on small-all-1000 and still slower on split-all-1000 (1.053×). The index and records share one server here, so the delay applies only to client traffic.',
                'phase-d2-sql-remotefetch.md': 'Through SQL, USE_REMOTE_FETCH is faster for 10 rows (0.885×) and 1,000 rows (0.536×). A few runs had a different store-prefix length; blocks without the difference give similar ratios.',
            }
            titles = {
                'phase-c2-reverse-skip.md': 'Figure C2.1: Reverse and Skip scans, USE_REMOTE_FETCH / SCAN_AND_FETCH',
                'phase-c3-synthetic-rtt.md': 'Figure C3.1: Scans with 0.5 ms added RTT, USE_REMOTE_FETCH / SCAN_AND_FETCH',
                'phase-d2-sql-remotefetch.md': 'Figure D2.1: SQL index scans, USE_REMOTE_FETCH / SCAN_AND_FETCH',
            }
            res = render_forest_svg(
                titles[fname],
                'Below 1.0× favors USE_REMOTE_FETCH. Single in-memory server.',
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
                    ('PlanCacheHit: final / before', [('ns/op', r_bef[0][1]), ('B/op', r_bef[0][2]), ('allocs/op', r_bef[0][3])]),
                    ('PlanCacheHit: final / legacy', [('ns/op', r_leg[0][1]), ('B/op', r_leg[0][2]), ('allocs/op', r_leg[0][3])]),
                ]
                res = render_forest_svg(
                    'Figure D1.1: SQL PlanCacheHit, final vs before and legacy',
                    'Below 1.0× favors the final tree.',
                    groups, [0.9, 0.95, 1.0, 1.05, 1.1], 'Final lower', 'Denominator lower',
                    summary='Only PlanCacheHit has results; the run stopped before the other two SQL cases finished. The final tree allocates 5% fewer objects; time shows no measurable change.',
                )
                verify_summary_numbers(fname, res, raw_md)
                return res

    return ''


def synced_pages() -> set:
    """Repository paths of the Markdown files this script turns into site pages."""
    pages = set()
    for cid, _, _, _, children in CAMPAIGNS:
        pages.add(f'docs/benchmarks/{cid}/README.md')
        pages.update(f'docs/benchmarks/{cid}/{fname}' for fname, *_ in children)
    return pages


GITHUB_BASE = 'https://github.com/birdayz/fdb-go'
LINK_RE = re.compile(r'\]\(([^)\s#]*)(#[^)\s]*)?\)')


def rewrite_repo_links(body: str, cid: str) -> str:
    """Point relative links to files that are not site pages at GitHub."""
    pages = synced_pages()

    def repl(m):
        target, anchor = m.group(1), m.group(2) or ''
        if not target or re.match(r'^[a-z]+:', target) or target.startswith('/'):
            return m.group(0)
        path = posixpath.normpath(posixpath.join(f'docs/benchmarks/{cid}', target))
        if path.startswith('..') or path in pages:
            return m.group(0)
        kind = 'tree' if target.endswith('/') or (REPO / path).is_dir() else 'blob'
        return f']({GITHUB_BASE}/{kind}/master/{path}{anchor})'

    return LINK_RE.sub(repl, body)


WEBSITE_END = '<!-- website: end of published content -->'


def published_readme(cid: str, text: str) -> str:
    """Cut a campaign README at WEBSITE_END; the site links to the full directory instead."""
    if WEBSITE_END not in text:
        return text
    head = text.split(WEBSITE_END, 1)[0].rstrip()
    url = f'{GITHUB_BASE}/tree/master/docs/benchmarks/{cid}'
    return (f'{head}\n\n## Protocol and records\n\n'
            f'The protocol, detailed analysis (`RESULTS.md`), execution records, decisions and campaign tools are in '
            f'[`docs/benchmarks/{cid}`]({url}).\n')


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

    if not is_index and cid and fname:
        for heading, svg in build_section_charts(cid, fname, text):
            pattern = r'^(' + re.escape(heading) + r'[^\n]*)$'
            if not re.search(pattern, body, flags=re.M):
                raise ValueError(f'{fname}: heading {heading!r} not found for chart')
            body = re.sub(pattern, lambda m, svg=svg: m.group(1) + '\n\n' + svg, body, count=1, flags=re.M)

    # Rewrite relative repo links that escape docs/benchmarks/<campaign>/ to GitHub URLs.
    body = body.replace('(../campaign-harness.md)', '(https://github.com/birdayz/fdb-go/blob/master/docs/benchmarks/campaign-harness.md)')
    body = body.replace('(../../../tools/benchcampaign/singlenode/README.md)', '(https://github.com/birdayz/fdb-go/blob/master/tools/benchcampaign/singlenode/README.md)')
    body = body.replace('(../../../tools/benchcampaign/multinode/README.md)', '(https://github.com/birdayz/fdb-go/blob/master/tools/benchcampaign/multinode/README.md)')

    if cid:
        body = rewrite_repo_links(body, cid)

    # Rewrite cross-campaign and sibling `.md` links to Hugo page paths.
    body = re.sub(r'\(\.\./([a-z0-9-]+)/README\.md\)', r'(/docs/benchmarks/\1/)', body)
    body = re.sub(r'\(\.\./([a-z0-9-]+)/([a-z0-9-]+)\.md\)', r'(/docs/benchmarks/\1/\2/)', body)
    body = body.replace('(README.md)', '(../)')
    if is_index:
        body = re.sub(r'\((phase-[a-z0-9-]+)\.md(#[^)\s]*)?\)', r'(\1/\2)', body)
    else:
        body = re.sub(r'\((phase-[a-z0-9-]+)\.md(#[^)\s]*)?\)', r'(../\1/\2)', body)
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

        readme_body = transform_body(published_readme(cid, (src_dir / 'README.md').read_text()), is_index=True, cid=cid, fname='README.md')
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
