#!/usr/bin/env python3
"""Render interactive SVG line charts & tables for a single-node campaign.

--campaign-dir DIR holds protocol.json, state.json and analysis/; the page is
written to --output-dir (default DIR/dashboard)/index.html. Workloads, block and
iteration counts, versions and revisions come from protocol.json. --vcpus and
--machine describe the VM; they default from protocol.json's "cloud" record.
"""
import argparse
import csv
import datetime as dt
import json
import math
from pathlib import Path

# Set by main() from the command line.
D = None
OUT_DIR = None

def load_client_phase(phase, workloads):
    items = []
    for w in workloads:
        abs_path = D / f'analysis/client/{phase}/{w}/cgo/absolute.csv'
        sum_path = D / f'analysis/client/{phase}/{w}/summary.json'
        if not abs_path.exists() or not sum_path.exists():
            continue
        rows = list(csv.DictReader(abs_path.open()))
        by_arm = {r['arm']: r for r in rows}
        summary = json.loads(sum_path.read_text())
        cgo_comp = summary['denominators']['cgo']
        bef_comp = summary['denominators']['before']
        items.append({
            'workload': w,
            'final_mean_us': float(by_arm['final']['mean_ns']) / 1000.0,
            'cgo_mean_us': float(by_arm['cgo']['mean_ns']) / 1000.0,
            'before_mean_us': float(by_arm['before']['mean_ns']) / 1000.0,
            'final_p50_us': float(by_arm['final']['p50_ns']) / 1000.0,
            'cgo_p50_us': float(by_arm['cgo']['p50_ns']) / 1000.0,
            'before_p50_us': float(by_arm['before']['p50_ns']) / 1000.0,
            'final_p99_us': float(by_arm['final']['p99_ns']) / 1000.0,
            'cgo_p99_us': float(by_arm['cgo']['p99_ns']) / 1000.0,
            'before_p99_us': float(by_arm['before']['p99_ns']) / 1000.0,
            'final_cpu_us': float(by_arm['final']['shutdown_process_cpu_ns_per_op']) / 1000.0,
            'cgo_cpu_us': float(by_arm['cgo']['shutdown_process_cpu_ns_per_op']) / 1000.0,
            'before_cpu_us': float(by_arm['before']['shutdown_process_cpu_ns_per_op']) / 1000.0,
            'ratio_cgo_mean': cgo_comp['mean_ns']['ratio'],
            'ratio_cgo_mean_lo': cgo_comp['mean_ns']['ci95_low'],
            'ratio_cgo_mean_hi': cgo_comp['mean_ns']['ci95_high'],
            'ratio_cgo_p99': cgo_comp['p99_ns']['ratio'],
            'ratio_cgo_p99_lo': cgo_comp['p99_ns']['ci95_low'],
            'ratio_cgo_p99_hi': cgo_comp['p99_ns']['ci95_high'],
            'ratio_cgo_cpu': cgo_comp['shutdown_process_cpu_ns_per_op']['ratio'],
            'ratio_cgo_cpu_lo': cgo_comp['shutdown_process_cpu_ns_per_op']['ci95_low'],
            'ratio_cgo_cpu_hi': cgo_comp['shutdown_process_cpu_ns_per_op']['ci95_high'],
            'ratio_bef_mean': bef_comp['mean_ns']['ratio'],
            'ratio_bef_mean_lo': bef_comp['mean_ns']['ci95_low'],
            'ratio_bef_mean_hi': bef_comp['mean_ns']['ci95_high'],
            'ratio_bef_p99': bef_comp['p99_ns']['ratio'],
            'ratio_bef_p99_lo': bef_comp['p99_ns']['ci95_low'],
            'ratio_bef_p99_hi': bef_comp['p99_ns']['ci95_high'],
            'ratio_bef_alloc_b': bef_comp['go_alloc_bytes_per_op']['ratio'],
            'ratio_bef_mallocs': bef_comp['go_mallocs_per_op']['ratio'],
        })
    return items

def load_generic_phases():
    reports = {}
    for ph in ['B', 'C1', 'D1', 'C2', 'D2', 'C3']:
        rp = D / f'analysis/{ph}/report.json'
        if rp.exists():
            reports[ph] = json.loads(rp.read_text())
    return reports

def describe(protocol, vcpus=None, machine=None):
    """Values the page text names; defaults reproduce the protocol's own campaign."""
    cloud = protocol.get('cloud')
    runtime = protocol['runtime']
    phase_a = next(p for p in protocol['phases'] if p['id'] == 'A')
    if vcpus is None or machine is None:
        if not cloud:
            raise ValueError('protocol.json has no cloud record; pass --vcpus and --machine')
        vcpus = cloud['dedicated_vcpus'] if vcpus is None else vcpus
        if machine is None:
            cpu = runtime['guest_topology'].split(';')[0]
            machine = f"Dedicated hcloud {cloud['type']} ({vcpus} vCPU {cpu}, {cloud['memory_gib']} GiB)"
    blocks, iterations, arms = phase_a['blocks'], phase_a['iterations'], len(phase_a['arms'])
    revisions = protocol['production_revisions']
    return dict(vcpus=vcpus, machine=machine, fdb=runtime['foundationdb'], go=runtime['go'], blocks=blocks,
                iterations=f'{iterations:,}', runs=blocks * arms, tx_per_arm=f'{blocks * iterations:,}',
                measured_tx=f"{len(phase_a['workloads']) * arms * blocks * iterations:,}",
                final=revisions['final'][:8], before=revisions['before_allocation_work_after_read_parity'][:8])


def build_html(protocol, vcpus=None, machine=None):
    text = describe(protocol, vcpus, machine)
    workloads = {p['id']: p['workloads'] for p in protocol['phases'] if p['id'] in ('A', 'A2')}
    data_a = load_client_phase('A', workloads.get('A', []))
    data_a2 = load_client_phase('A2', workloads.get('A2', []))
    generic_reports = load_generic_phases()
    state = json.loads((D / 'state.json').read_text()) if (D / 'state.json').exists() else {}
    now_utc = dt.datetime.now(dt.timezone.utc).strftime('%Y-%m-%d %H:%M:%S UTC')

    payload_json = json.dumps({
        'A': data_a,
        'A2': data_a2,
        'generic': generic_reports,
        'updated_utc': now_utc,
        'phase_status': state.get('phase', 'running')
    })

    html = f"""<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>FDB {text['vcpus']}-vCPU Rebenchmark — Pure-Go vs Go-CGo Scenarios</title>
<style>
  :root {{
    --bg: #0b0f17;
    --panel: #131a27;
    --border: #233047;
    --text: #e6edf3;
    --muted: #8b949e;
    --purego: #2dd4bf;
    --cgo: #f97316;
    --beforego: #60a5fa;
    --grid: #1e293b;
  }}
  * {{ box-sizing: border-box; }}
  body {{
    margin: 0;
    padding: 24px 32px;
    background: var(--bg);
    color: var(--text);
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
    line-height: 1.45;
  }}
  header {{
    display: flex;
    flex-wrap: wrap;
    justify-content: space-between;
    align-items: baseline;
    border-bottom: 1px solid var(--border);
    padding-bottom: 14px;
    margin-bottom: 20px;
  }}
  h1 {{ margin: 0; font-size: 22px; font-weight: 650; }}
  .subtitle {{ color: var(--muted); font-size: 13px; margin-top: 4px; }}
  .controls {{
    display: flex;
    gap: 10px;
    flex-wrap: wrap;
    margin-bottom: 18px;
    align-items: center;
  }}
  button {{
    background: var(--panel);
    color: var(--text);
    border: 1px solid var(--border);
    padding: 7px 13px;
    border-radius: 6px;
    cursor: pointer;
    font-size: 13px;
    font-weight: 500;
  }}
  button.active {{
    border-color: var(--purego);
    background: rgba(45, 212, 191, 0.14);
    color: #fff;
  }}
  .card {{
    background: var(--panel);
    border: 1px solid var(--border);
    border-radius: 10px;
    padding: 18px 20px;
    margin-bottom: 22px;
  }}
  .card h2 {{
    margin: 0 0 6px 0;
    font-size: 16px;
    font-weight: 600;
  }}
  .legend {{
    display: flex;
    gap: 20px;
    font-size: 13px;
    margin-bottom: 12px;
    align-items: center;
  }}
  .legend-item {{ display: flex; align-items: center; gap: 7px; }}
  .swatch {{ width: 22px; height: 3px; border-radius: 2px; display: inline-block; }}
  .dot {{ width: 9px; height: 9px; border-radius: 50%; display: inline-block; }}
  svg {{ width: 100%; height: auto; display: block; overflow: visible; }}
  table {{
    width: 100%;
    border-collapse: collapse;
    font-size: 13px;
    font-variant-numeric: tabular-nums;
  }}
  th, td {{
    padding: 8px 10px;
    border-bottom: 1px solid var(--border);
    text-align: right;
  }}
  th:first-child, td:first-child {{ text-align: left; font-weight: 600; }}
  th {{ color: var(--muted); font-weight: 500; background: rgba(255,255,255,0.02); }}
  tr:hover td {{ background: rgba(255,255,255,0.025); }}
  .badge-win {{ color: var(--purego); font-weight: 600; }}
  .tooltip {{
    position: fixed;
    pointer-events: none;
    background: #0f172a;
    border: 1px solid #334155;
    padding: 9px 12px;
    border-radius: 6px;
    font-size: 12px;
    color: #f8fafc;
    box-shadow: 0 8px 24px rgba(0,0,0,0.45);
    display: none;
    z-index: 1000;
  }}
</style>
</head>
<body>
<div id="tooltip" class="tooltip"></div>
<header>
  <div>
    <h1>FoundationDB {text['vcpus']}-vCPU Rebenchmark — Pure-Go vs Go-CGo</h1>
    <div class="subtitle">{text['machine']} · FDB {text['fdb']} · Go {text['go']} · {text['blocks']} counterbalanced blocks × {text['iterations']} tx/arm/workload ({text['measured_tx']} measured client tx)</div>
  </div>
  <div class="subtitle">Updated: <span id="updated">{now_utc}</span> · Status: <code id="status">{state.get('phase', 'running')}</code></div>
</header>

<div class="controls">
  <span style="color:var(--muted);font-size:13px;">Metric:</span>
  <button id="btn-mean" class="active" onclick="setMetric('mean')">Mean Latency (µs/tx)</button>
  <button id="btn-p50" onclick="setMetric('p50')">Median p50 Latency (µs/tx)</button>
  <button id="btn-p99" onclick="setMetric('p99')">Tail p99 Latency (µs/tx)</button>
  <button id="btn-cpu" onclick="setMetric('cpu')">Shutdown Process CPU E−A (µs/tx)</button>
  <span style="margin-left:16px;color:var(--muted);font-size:13px;">Y-Axis Scale:</span>
  <button id="btn-linear" class="active" onclick="setScale('linear')">Linear</button>
  <button id="btn-log" onclick="setScale('log')">Log10 (shows RYW &amp; reads clearly)</button>
  <label style="margin-left:16px;font-size:13px;color:var(--muted);cursor:pointer;">
    <input type="checkbox" id="chk-before" onchange="render()" checked> Show Before-Go ({text['before']}) reference line
  </label>
</div>

<div class="card">
  <h2 id="chart-title">Phase A Scenarios — Latency per Transaction (µs, lower is better)</h2>
  <div class="legend">
    <div class="legend-item"><span class="swatch" style="background:var(--purego)"></span><span class="dot" style="background:var(--purego)"></span> <strong>Pure-Go (final {text['final']})</strong></div>
    <div class="legend-item"><span class="swatch" style="background:var(--cgo)"></span><span class="dot" style="background:var(--cgo)"></span> <strong>Go-CGo (libfdb_c 7.3.77)</strong></div>
    <div class="legend-item" id="leg-before"><span class="swatch" style="background:var(--beforego);border-top:2px dashed var(--beforego)"></span> <span>Pure-Go Before Allocation Opts ({text['before']})</span></div>
  </div>
  <div id="chart-container"></div>
</div>

<div class="card">
  <h2>Paired-Block Ratio Line Chart — Pure-Go / Go-CGo (Nominal 95% Block-Bootstrap CI, &lt; 1.00 = Pure-Go Faster)</h2>
  <div id="ratio-chart-container"></div>
</div>

<div class="card">
  <h2>Phase A Complete Scenario Table ({text['blocks']} Paired Blocks · {text['runs']} Runs · {text['tx_per_arm']} Tx/Arm per Scenario)</h2>
  <div style="overflow-x:auto;">
    <table id="table-a">
      <thead>
        <tr>
          <th>Scenario</th>
          <th>Pure-Go Mean (µs)</th>
          <th>Go-CGo Mean (µs)</th>
          <th>Before-Go Mean (µs)</th>
          <th>Pure-Go / CGo Mean [95% CI]</th>
          <th>Pure-Go p99 (µs)</th>
          <th>Go-CGo p99 (µs)</th>
          <th>Pure-Go / CGo p99 [95% CI]</th>
          <th>Pure-Go / CGo CPU E−A [95% CI]</th>
          <th>Final / Before-Go Mean [95% CI]</th>
          <th>Final / Before-Go Go B/tx</th>
        </tr>
      </thead>
      <tbody></tbody>
    </table>
  </div>
</div>

<div id="generic-section"></div>

<script>
const DATA = {payload_json};
let currentMetric = 'mean';
let currentScale = 'linear';

function setMetric(m) {{
  currentMetric = m;
  for (const k of ['mean','p50','p99','cpu']) {{
    document.getElementById('btn-' + k).classList.toggle('active', k === m);
  }}
  render();
}}

function setScale(s) {{
  currentScale = s;
  document.getElementById('btn-linear').classList.toggle('active', s === 'linear');
  document.getElementById('btn-log').classList.toggle('active', s === 'log');
  render();
}}

function showTip(evt, html) {{
  const t = document.getElementById('tooltip');
  t.innerHTML = html;
  t.style.display = 'block';
  t.style.left = (evt.clientX + 14) + 'px';
  t.style.top = (evt.clientY + 14) + 'px';
}}
function hideTip() {{
  document.getElementById('tooltip').style.display = 'none';
}}

function renderLineChart() {{
  const rows = DATA.A;
  if (!rows || !rows.length) return;
  const showBefore = document.getElementById('chk-before').checked;
  document.getElementById('leg-before').style.display = showBefore ? 'flex' : 'none';

  const titles = {{
    mean: 'Phase A Scenarios — Mean Latency per Transaction (µs, lower is better)',
    p50: 'Phase A Scenarios — Median (p50) Latency per Transaction (µs, lower is better)',
    p99: 'Phase A Scenarios — Tail (p99) Latency per Transaction (µs, lower is better)',
    cpu: 'Phase A Scenarios — Shutdown Process CPU E−A per Transaction (µs, lower is better)'
  }};
  document.getElementById('chart-title').textContent = titles[currentMetric];

  const W = 1120, H = 410;
  const padL = 74, padR = 36, padT = 28, padB = 72;
  const plotW = W - padL - padR;
  const plotH = H - padT - padB;

  const getVal = (r, arm) => r[arm + '_' + currentMetric + '_us'];
  let allVals = [];
  rows.forEach(r => {{
    allVals.push(getVal(r, 'final'), getVal(r, 'cgo'));
    if (showBefore) allVals.push(getVal(r, 'before'));
  }});

  const minVal = currentScale === 'log' ? Math.max(1, Math.min(...allVals) * 0.8) : 0;
  const maxVal = Math.max(...allVals) * 1.10;

  const xPos = i => padL + (i / Math.max(1, rows.length - 1)) * plotW;
  const yPos = v => {{
    if (currentScale === 'log') {{
      const lmin = Math.log10(minVal), lmax = Math.log10(maxVal);
      return padT + plotH - ((Math.log10(Math.max(v, minVal)) - lmin) / (lmax - lmin)) * plotH;
    }}
    return padT + plotH - ((v - minVal) / (maxVal - minVal)) * plotH;
  }};

  let svg = `<svg viewBox="0 0 ${{W}} ${{H}}">`;

  // Y grid ticks
  let ticks = [];
  if (currentScale === 'log') {{
    for (let p = 1; p <= 10000; p *= 10) {{
      if (p >= minVal * 0.9 && p <= maxVal * 1.1) ticks.push(p);
      if (p * 2 >= minVal && p * 2 <= maxVal) ticks.push(p * 2);
      if (p * 5 >= minVal && p * 5 <= maxVal) ticks.push(p * 5);
    }}
  }} else {{
    const step = Math.pow(10, Math.floor(Math.log10(maxVal))) / 2;
    const niceStep = (maxVal / step > 8) ? step * 2 : step;
    for (let v = 0; v <= maxVal; v += niceStep) ticks.push(v);
  }}

  ticks.forEach(t => {{
    const y = yPos(t);
    svg += `<line x1="${{padL}}" y1="${{y.toFixed(1)}}" x2="${{W - padR}}" y2="${{y.toFixed(1)}}" stroke="var(--grid)" stroke-width="1"/>`;
    svg += `<text x="${{padL - 10}}" y="${{(y + 4).toFixed(1)}}" fill="var(--muted)" font-size="11" text-anchor="end">${{t >= 100 ? Math.round(t) : t.toFixed(1)}} µs</text>`;
  }});

  // X grid + labels
  rows.forEach((r, i) => {{
    const x = xPos(i);
    svg += `<line x1="${{x.toFixed(1)}}" y1="${{padT}}" x2="${{x.toFixed(1)}}" y2="${{padT + plotH}}" stroke="var(--grid)" stroke-width="1" stroke-dasharray="2,3"/>`;
    svg += `<text x="${{x.toFixed(1)}}" y="${{padT + plotH + 22}}" fill="var(--text)" font-size="12" font-weight="600" text-anchor="end" transform="rotate(-24 ${{x.toFixed(1)}} ${{padT + plotH + 22}})">${{r.workload}}</text>`;
  }});

  const makePath = (arm) => rows.map((r, i) => `${{i === 0 ? 'M' : 'L'}}${{xPos(i).toFixed(1)}},${{yPos(getVal(r, arm)).toFixed(1)}}`).join(' ');

  if (showBefore) {{
    svg += `<path d="${{makePath('before')}}" fill="none" stroke="var(--beforego)" stroke-width="2" stroke-dasharray="5,4" opacity="0.85"/>`;
  }}
  svg += `<path d="${{makePath('cgo')}}" fill="none" stroke="var(--cgo)" stroke-width="3"/>`;
  svg += `<path d="${{makePath('final')}}" fill="none" stroke="var(--purego)" stroke-width="3"/>`;

  // Points and value labels
  rows.forEach((r, i) => {{
    const x = xPos(i);
    const vf = getVal(r, 'final'), vc = getVal(r, 'cgo'), vb = getVal(r, 'before');
    const yf = yPos(vf), yc = yPos(vc), yb = yPos(vb);
    const speedup = ((1 - vf / vc) * 100).toFixed(1);

    if (showBefore) {{
      svg += `<circle cx="${{x.toFixed(1)}}" cy="${{yb.toFixed(1)}}" r="4" fill="var(--beforego)"
        onmousemove="showTip(event, '<b>${{r.workload}}</b> (Before-Go)<br>${{vb.toFixed(2)}} µs')" onmouseout="hideTip()"/>`;
    }}
    svg += `<circle cx="${{x.toFixed(1)}}" cy="${{yc.toFixed(1)}}" r="5.5" fill="var(--cgo)" stroke="#0b0f17" stroke-width="1.5"
      onmousemove="showTip(event, '<b>${{r.workload}}</b> — Go-CGo (libfdb_c)<br>Value: <b>${{vc.toFixed(2)}} µs</b><br>Pure-Go is ${{speedup}}% faster')" onmouseout="hideTip()"/>`;
    svg += `<circle cx="${{x.toFixed(1)}}" cy="${{yf.toFixed(1)}}" r="5.5" fill="var(--purego)" stroke="#0b0f17" stroke-width="1.5"
      onmousemove="showTip(event, '<b>${{r.workload}}</b> — Pure-Go (final)<br>Value: <b>${{vf.toFixed(2)}} µs</b><br>Go-CGo: ${{vc.toFixed(2)}} µs (${{speedup}}% faster)')" onmouseout="hideTip()"/>`;

    const offsetF = (yf <= yc) ? -10 : 15;
    const offsetC = (yc < yf) ? -10 : 15;
    svg += `<text x="${{x.toFixed(1)}}" y="${{(yf + offsetF).toFixed(1)}}" fill="var(--purego)" font-size="11" font-weight="600" text-anchor="middle">${{vf >= 100 ? Math.round(vf) : vf.toFixed(1)}}</text>`;
    svg += `<text x="${{x.toFixed(1)}}" y="${{(yc + offsetC).toFixed(1)}}" fill="var(--cgo)" font-size="11" font-weight="600" text-anchor="middle">${{vc >= 100 ? Math.round(vc) : vc.toFixed(1)}}</text>`;
  }});

  svg += `</svg>`;
  document.getElementById('chart-container').innerHTML = svg;
}}

function renderRatioChart() {{
  const rows = DATA.A;
  if (!rows || !rows.length) return;
  const W = 1120, H = 260;
  const padL = 74, padR = 36, padT = 22, padB = 64;
  const plotW = W - padL - padR;
  const plotH = H - padT - padB;
  const xPos = i => padL + (i / Math.max(1, rows.length - 1)) * plotW;
  const yPos = v => padT + plotH - ((v - 0.0) / 1.25) * plotH;

  let svg = `<svg viewBox="0 0 ${{W}} ${{H}}">`;
  for (const t of [0.2, 0.4, 0.6, 0.8, 1.0, 1.2]) {{
    const y = yPos(t);
    const isParity = Math.abs(t - 1.0) < 1e-6;
    svg += `<line x1="${{padL}}" y1="${{y.toFixed(1)}}" x2="${{W - padR}}" y2="${{y.toFixed(1)}}" stroke="${{isParity ? '#f43f5e' : 'var(--grid)'}}" stroke-width="${{isParity ? '1.5' : '1'}}" stroke-dasharray="${{isParity ? '4,4' : ''}}"/>`;
    svg += `<text x="${{padL - 10}}" y="${{(y + 4).toFixed(1)}}" fill="${{isParity ? '#f43f5e' : 'var(--muted)'}}" font-size="11" text-anchor="end">${{t.toFixed(2)}}x</text>`;
  }}

  const pathMean = rows.map((r, i) => `${{i === 0 ? 'M' : 'L'}}${{xPos(i).toFixed(1)}},${{yPos(r.ratio_cgo_mean).toFixed(1)}}`).join(' ');
  svg += `<path d="${{pathMean}}" fill="none" stroke="var(--purego)" stroke-width="2.5"/>`;

  rows.forEach((r, i) => {{
    const x = xPos(i);
    const y = yPos(r.ratio_cgo_mean);
    const ylo = yPos(r.ratio_cgo_mean_lo), yhi = yPos(r.ratio_cgo_mean_hi);
    svg += `<line x1="${{x.toFixed(1)}}" y1="${{ylo.toFixed(1)}}" x2="${{x.toFixed(1)}}" y2="${{yhi.toFixed(1)}}" stroke="var(--purego)" stroke-width="2" opacity="0.7"/>`;
    svg += `<line x1="${{(x-4).toFixed(1)}}" y1="${{ylo.toFixed(1)}}" x2="${{(x+4).toFixed(1)}}" y2="${{ylo.toFixed(1)}}" stroke="var(--purego)" stroke-width="2"/>`;
    svg += `<line x1="${{(x-4).toFixed(1)}}" y1="${{yhi.toFixed(1)}}" x2="${{(x+4).toFixed(1)}}" y2="${{yhi.toFixed(1)}}" stroke="var(--purego)" stroke-width="2"/>`;
    svg += `<circle cx="${{x.toFixed(1)}}" cy="${{y.toFixed(1)}}" r="5" fill="var(--purego)" stroke="#0b0f17" stroke-width="1.5"
      onmousemove="showTip(event, '<b>${{r.workload}}</b> Pure-Go / Go-CGo Mean Ratio<br><b>${{r.ratio_cgo_mean.toFixed(4)}}x</b> [${{r.ratio_cgo_mean_lo.toFixed(4)}}, ${{r.ratio_cgo_mean_hi.toFixed(4)}}]')" onmouseout="hideTip()"/>`;
    svg += `<text x="${{x.toFixed(1)}}" y="${{(y - 10).toFixed(1)}}" fill="var(--purego)" font-size="11" font-weight="600" text-anchor="middle">${{r.ratio_cgo_mean.toFixed(2)}}x</text>`;
    svg += `<text x="${{x.toFixed(1)}}" y="${{padT + plotH + 20}}" fill="var(--text)" font-size="12" font-weight="600" text-anchor="end" transform="rotate(-24 ${{x.toFixed(1)}} ${{padT + plotH + 20}})">${{r.workload}}</text>`;
  }});
  svg += `</svg>`;
  document.getElementById('ratio-chart-container').innerHTML = svg;
}}

function renderTable() {{
  const tbody = document.querySelector('#table-a tbody');
  tbody.innerHTML = DATA.A.map(r => `
    <tr>
      <td>${{r.workload}}</td>
      <td style="color:var(--purego);font-weight:600">${{r.final_mean_us.toFixed(2)}}</td>
      <td style="color:var(--cgo);font-weight:600">${{r.cgo_mean_us.toFixed(2)}}</td>
      <td>${{r.before_mean_us.toFixed(2)}}</td>
      <td class="badge-win">${{r.ratio_cgo_mean.toFixed(3)}}x [${{r.ratio_cgo_mean_lo.toFixed(3)}}, ${{r.ratio_cgo_mean_hi.toFixed(3)}}]</td>
      <td>${{r.final_p99_us.toFixed(2)}}</td>
      <td>${{r.cgo_p99_us.toFixed(2)}}</td>
      <td class="badge-win">${{r.ratio_cgo_p99.toFixed(3)}}x [${{r.ratio_cgo_p99_lo.toFixed(3)}}, ${{r.ratio_cgo_p99_hi.toFixed(3)}}]</td>
      <td class="badge-win">${{r.ratio_cgo_cpu.toFixed(3)}}x [${{r.ratio_cgo_cpu_lo.toFixed(3)}}, ${{r.ratio_cgo_cpu_hi.toFixed(3)}}]</td>
      <td>${{r.ratio_bef_mean.toFixed(3)}}x [${{r.ratio_bef_mean_lo.toFixed(3)}}, ${{r.ratio_bef_mean_hi.toFixed(3)}}]</td>
      <td>${{r.ratio_bef_alloc_b.toFixed(3)}}x (${{r.ratio_bef_mallocs.toFixed(3)}}x allocs)</td>
    </tr>
  `).join('');
}}

function renderGeneric() {{
  const container = document.getElementById('generic-section');
  const keys = Object.keys(DATA.generic || {{}});
  if (!keys.length) {{
    container.innerHTML = `<div class="card"><h2>Record Layer, Mapped Fetch &amp; SQL Phases (B, C1, D1, C2, D2)</h2><div class="subtitle">Currently running on the {text['vcpus']}-vCPU VM (memory storage engine). Refresh this page as phases complete.</div></div>`;
    return;
  }}
  let html = '';
  for (const ph of keys) {{
    const rep = DATA.generic[ph];
    const rows = (rep.comparisons || []).filter(c => c.metric === 'ns/op' || c.metric === 'B/op');
    html += `<div class="card"><h2>Phase ${{ph}} — ${{rep.question}}</h2><table><thead><tr><th>Case</th><th>Comparison</th><th>Metric</th><th>GM Ratio [95% CI]</th></tr></thead><tbody>`;
    for (const c of rows) {{
      const val = c.status === 'ok' ? `${{c.gm.toFixed(4)}}x [${{c.nominal_95_interval[0].toFixed(4)}}, ${{c.nominal_95_interval[1].toFixed(4)}}]` : 'undefined (zero)';
      html += `<tr><td>${{c.case}}</td><td>${{c.label}}</td><td>${{c.metric}}</td><td>${{val}}</td></tr>`;
    }}
    html += `</tbody></table></div>`;
  }}
  container.innerHTML = html;
}}

function render() {{
  renderLineChart();
  renderRatioChart();
  renderTable();
  renderGeneric();
}}
render();
</script>
</body>
</html>
"""
    (OUT_DIR / 'index.html').write_text(html)
    print(f"Wrote {OUT_DIR / 'index.html'}")

def main():
    global D, OUT_DIR
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('--campaign-dir', required=True)
    ap.add_argument('--output-dir', help='default: CAMPAIGN_DIR/dashboard')
    ap.add_argument('--vcpus', type=int, help='VM vCPU count shown in the title')
    ap.add_argument('--machine', help='VM description shown in the subtitle')
    args = ap.parse_args()
    D = Path(args.campaign_dir)
    OUT_DIR = Path(args.output_dir) if args.output_dir else D / 'dashboard'
    OUT_DIR.mkdir(parents=True, exist_ok=True)
    build_html(json.loads((D / 'protocol.json').read_text()), args.vcpus, args.machine)


if __name__ == '__main__':
    main()
