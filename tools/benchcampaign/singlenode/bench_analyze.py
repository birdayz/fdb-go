#!/usr/bin/env python3
"""Analyze frozen testing.B artifacts: paired block GMs, not p99.

--input PHASE_DIRECTORY --output NEW_DIRECTORY
Only complete, exact three-repeat arm/block populations enter ratios. Retained
historical/environmental adverse cases are N/A, never silently filtered. Seed
391706, 20000 nominal exploratory whole-block bootstrap resamples, 95% intervals.
"""
import argparse
import csv
import json
import math
from pathlib import Path
import random
import statistics
import sys

from bench_runner import METRICS, dump, number, parse_output, sha


def percentile(values, p):
    ordered = sorted(values)
    x = (len(ordered)-1)*p
    lo = math.floor(x)
    hi = math.ceil(x)
    return ordered[lo]+(ordered[hi]-ordered[lo])*(x-lo)


def paired_gm(ratios, resamples=20000, seed=391706):
    if not ratios or any(not math.isfinite(x) or x <= 0 for x in ratios):
        raise ValueError('GM requires a nonempty finite strictly positive ratio population')
    logs = [math.log(x) for x in ratios]
    rng = random.Random(seed)
    distribution = [math.exp(sum(rng.choice(logs) for _ in logs)/len(logs)) for _ in range(resamples)]
    return dict(gm=math.exp(statistics.mean(logs)), nominal_95_interval=[percentile(distribution, .025), percentile(distribution, .975)],
                blocks=len(logs), resamples=resamples, seed=seed)


def comparisons(phase):
    if phase in ('B', 'D1'):
        return [('final', 'legacy'), ('final', 'before')]
    if phase == 'C1':
        return [('pr2-remote', 'pr2-ordinary'), ('final-remote', 'final-ordinary'),
                ('final-remote', 'pr2-remote'), ('final-ordinary', 'pr2-ordinary')]
    return [('final-remote', 'final-ordinary')]


def load_samples(path):
    with open(path, newline='') as stream:
        reader = csv.DictReader(stream)
        required = {'phase', 'case', 'block', 'position', 'arm', 'repeat', 'N', *METRICS}
        if set(reader.fieldnames or []) != required:
            raise ValueError('wrong metrics CSV schema')
        rows = []
        for row in reader:
            for k in ('block', 'position', 'repeat', 'N'):
                if not row[k].isdecimal():
                    raise ValueError('malformed sample identity')
                row[k] = int(row[k])
            if row['N'] <= 0:
                raise ValueError('nonpositive iteration count')
            for metric in METRICS:
                row[metric] = None if row[metric] == '' else number(float(row[metric]), metric == 'ns/op')
            if any(row[m] is None for m in ('ns/op', 'B/op', 'allocs/op')):
                raise ValueError('missing required metric')
            rows.append(row)
        return rows


def summarize(plan, samples, omitted):
    if not samples and not omitted:
        raise ValueError('empty measurement population')
    phase, arms, blocks = plan['phase']['id'], plan['phase']['arms'], plan['phase']['blocks']
    cases = {c['key']: c for c in plan['cases']}
    groups = {}
    identities = set()
    for s in samples:
        if s['phase'] != phase or s['case'] not in cases or s['arm'] not in arms or not 0 <= s['block'] < blocks:
            raise ValueError('unknown sample case/arm/block/phase')
        if s['position'] != plan['orders'][s['block']].index(s['arm']) or s['repeat'] not in (0, 1, 2):
            raise ValueError('sample does not match frozen arm order/repeat')
        identity = (s['case'], s['block'], s['arm'], s['repeat'])
        if identity in identities:
            raise ValueError('duplicate sample identity')
        identities.add(identity)
        groups.setdefault(identity[:3], []).append(s)
    if not set(omitted).issubset(cases):
        raise ValueError('unknown omitted case')
    report = dict(phase=phase, question=plan['phase']['question'], production_revisions=plan['production_revisions'],
                  harness_commit=plan['harness_commit'], raw_samples=len(samples), cases={}, comparisons=[], orders=plan['orders'],
                  interpretation=plan['interpretation'], uncertainty='Nominal exploratory 95% percentile paired-block bootstrap; 6/8 blocks have limited resolution. No equivalence/noninferiority/multiplicity-adjusted claim.')
    for key in cases:
        raw = [s for s in samples if s['case'] == key]
        if key in omitted:
            report['cases'][key] = dict(status='N/A', reason=omitted[key], raw_samples_retained=len(raw))
            continue
        matrix = {(b, a): groups.get((key, b, a), []) for b in range(blocks) for a in arms}
        if any(len(v) != 3 or {r['repeat'] for r in v} != {0, 1, 2} for v in matrix.values()):
            raise ValueError('missing/nonempty three-repeat arm/block population: '+key)
        absolutes, medians = {}, {}
        for a in arms:
            absolutes[a] = {}
            for m in METRICS:
                values = [r[m] for b in range(blocks) for r in matrix[b, a]]
                if all(v is None for v in values):
                    continue
                if any(v is None for v in values):
                    raise ValueError('partially missing metric '+m)
                blockvalues = [statistics.median(r[m] for r in matrix[b, a]) for b in range(blocks)]
                medians[a, m] = blockvalues
                absolutes[a][m] = dict(median_of_process_repeat_medians=statistics.median(blockvalues),
                                       raw_repeat_median=statistics.median(values), raw_samples=len(values), blocks=blocks,
                                       minimum=min(values), maximum=max(values))
        prefix_values = {r['prefix-B'] for r in raw if r['prefix-B'] is not None}
        audit_prefixes = {r.get('audit_prefix_bytes') for r in raw if r.get('audit_prefix_bytes') is not None}
        confound = len(prefix_values | audit_prefixes) > 1
        report['cases'][key] = dict(status='ok', raw_samples=len(raw), absolutes=absolutes,
                                   physical_prefix_lengths=sorted(prefix_values | audit_prefixes), physical_prefix_confound=confound)
        for numerator, denominator in comparisons(phase):
            for metric in METRICS:
                if (numerator, metric) not in medians or (denominator, metric) not in medians:
                    continue
                n, d = medians[numerator, metric], medians[denominator, metric]
                item = dict(case=key, metric=metric, numerator=numerator, denominator=denominator,
                            label=(numerator+'/'+denominator)+(' (legacy contains parity differences)' if denominator == 'legacy' else ''),
                            raw_samples_per_arm=blocks*3, block_medians_numerator=n, block_medians_denominator=d,
                            physical_prefix_confound=confound)
                if any(x == 0 or y == 0 for x, y in zip(n, d)):
                    item.update(status='undefined-zero', reason='Absolute zero metrics retained; undefined/zero GM ratios omitted')
                else:
                    ratios = [x/y for x, y in zip(n, d)]
                    item.update(status='ok', block_ratios=ratios, **paired_gm(ratios, plan['bootstrap_resamples'], plan['analysis_seed']))
                    by_order = {}
                    for b, ratio in enumerate(ratios):
                        direction = 'numerator-first' if plan['orders'][b].index(numerator) < plan['orders'][b].index(denominator) else 'denominator-first'
                        by_order.setdefault(direction, []).append(ratio)
                    item['order_diagnostics'] = {order: dict(blocks=len(rs), gm=math.exp(statistics.mean(math.log(x) for x in rs))) for order, rs in by_order.items()}
                    item['leave_one_block_out_gm'] = [math.exp(statistics.mean(math.log(x) for i, x in enumerate(ratios) if i != drop)) for drop in range(blocks)]
                report['comparisons'].append(item)
    return report


def analyze(input_dir, output_dir):
    inp, out = Path(input_dir), Path(output_dir)
    if out.exists():
        raise FileExistsError('refusing analysis overwrite')
    plan = json.loads((inp/'plan.json').read_text())
    controller = json.loads((inp/'controller.json').read_text())
    if controller['plan_sha256'] != sha(inp/'plan.json') or sha(__file__) != plan['expected_sha256'][plan['paths']['analyzer']]:
        raise ValueError('plan or analyzer differs from frozen runtime receipt')
    samples = load_samples(inp/'metrics.csv')
    omitted = json.loads((inp/'omitted.json').read_text())
    before = json.loads((inp/'pins.before.json').read_text())
    after_path = inp/'pins.after.json'
    after = json.loads((after_path if after_path.exists() else inp/'pins.finally.json').read_text())
    if before != plan['expected_sha256'] or after != before:
        raise ValueError('phase input pins changed; not eligible for clean ratios')
    for job in plan['jobs']:
        if job['case']['key'] in omitted:
            continue
        folder = inp/job['id']
        if json.loads((folder/'result.json').read_text()).get('status') != 'ok':
            raise ValueError('non-successful raw run in clean case')
        parsed, audits = parse_output((folder/'stdout.txt').read_text()+'\n'+(folder/'stderr.txt').read_text(), job,
                                      gomaxprocs=plan['env']['GOMAXPROCS'])
        csv_rows = sorted((s for s in samples if s['case'] == job['case']['key'] and s['block'] == job['block'] and s['arm'] == job['arm']), key=lambda s: s['repeat'])
        if len(csv_rows) != 3 or any(s['N'] != p['N'] or any(s[m] != p['metrics'].get(m) for m in METRICS) for s, p in zip(csv_rows, parsed)):
            raise ValueError('CSV differs from exact raw benchmark population')
        if json.loads((folder/'audits.json').read_text()) != audits:
            raise ValueError('audit artifact differs from raw log')
    # Preserve per-process physical-prefix evidence even where classic B emitted
    # no benchmark metric. Only existing emitted receipts are used.
    for s in samples:
        job = next(j for j in plan['jobs'] if j['case']['key'] == s['case'] and j['block'] == s['block'] and j['arm'] == s['arm'])
        audit_path = inp/job['id']/'audits.json'
        if audit_path.exists():
            audits = json.loads(audit_path.read_text())
            prefixes = {a['prefix_bytes'] for a in audits if a.get('case') == job['name'] and 'prefix_bytes' in a}
            if len(prefixes) > 1:
                raise ValueError('physical prefix changed within process')
            if prefixes:
                s['audit_prefix_bytes'] = next(iter(prefixes))
    if not omitted:
        receipt = json.loads((inp/'complete.json').read_text())
        if receipt['samples'] != len(samples) or receipt['completed'] != len(plan['jobs']) or not receipt['pins_unchanged']:
            raise ValueError('missing/inconsistent complete population receipt')
    report = summarize(plan, samples, omitted)
    out.mkdir(parents=True, exist_ok=False)
    dump(out/'report.json', report)
    dump(out/'analysis-provenance.json', dict(analyzer_sha256=sha(__file__), plan_sha256=sha(inp/'plan.json'),
                                            metrics_sha256=sha(inp/'metrics.csv'), nominal_resamples=20000, seed=391706))
    lines = ['# '+report['phase']+' testing.B results', '', report['question'], '', report['interpretation'], '', report['uncertainty'], '',
             '| Case | Comparison | Metric | GM ratio (nominal 95% interval) |', '|---|---|---|---|']
    for item in report['comparisons']:
        value = 'undefined (absolute zero metric)' if item['status'] != 'ok' else f"{item['gm']:.4f} ({item['nominal_95_interval'][0]:.4f}, {item['nominal_95_interval'][1]:.4f})"
        if item['physical_prefix_confound']:
            value += ' — PHYSICAL PREFIX CONFOUND'
        lines.append(f"| {item['case']} | {item['label']} | {item['metric']} | {value} |")
    lines += ['', '## Absolute metrics and raw populations', '',
              '| Case | Arm | Metric | Median of process repeat medians | Raw repeat median | Raw samples / blocks |',
              '|---|---|---|---|---|---|']
    for case, value in report['cases'].items():
        if value['status'] == 'N/A':
            lines.append('\n'+case+': N/A; retained adverse case: '+json.dumps(value['reason']))
            continue
        for arm, metrics in value['absolutes'].items():
            for metric, absolute in metrics.items():
                lines.append(f"| {case} | {arm} | {metric} | {absolute['median_of_process_repeat_medians']:.4g} | {absolute['raw_repeat_median']:.4g} | {absolute['raw_samples']} / {absolute['blocks']} |")
        if value['physical_prefix_confound']:
            lines.append('\n'+case+': PHYSICAL PREFIX CONFOUND '+str(value['physical_prefix_lengths']))
    lines += ['', '## Order diagnostics (descriptive, not another inferential gate)', '',
              '| Case | Comparison | Metric | Order | Blocks | GM ratio |', '|---|---|---|---|---|---|']
    for item in report['comparisons']:
        for order, diagnostic in item.get('order_diagnostics', {}).items():
            lines.append(f"| {item['case']} | {item['label']} | {item['metric']} | {order} | {diagnostic['blocks']} | {diagnostic['gm']:.4f} |")
    lines += ['', 'Raw samples: '+str(report['raw_samples'])+'. Complete block ratios and leave-one-block-out diagnostics are retained in report.json.']
    (out/'report.md').write_text('\n'.join(lines)+'\n')
    return report


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--input', required=True)
    p.add_argument('--output', required=True)
    a = p.parse_args()
    analyze(a.input, a.output)
    return 0


if __name__ == '__main__':
    sys.exit(main())
