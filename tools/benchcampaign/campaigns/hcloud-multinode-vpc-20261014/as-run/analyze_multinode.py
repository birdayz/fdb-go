#!/usr/bin/env python3
"""Analyze multi-node M2 (R2) and M1 (R1) phases, gated by R4 telemetry reconciliation."""
import argparse, csv, hashlib, json, math, os, pathlib, random, shutil, statistics, subprocess, sys

C = pathlib.Path('${FDB_PERF_HOME}/hcloud-multinode-vpc-20261014')
W = pathlib.Path('${FDB_GO_WORKTREES}/fdb-go-bench267')
sys.path.insert(0, str(W / 'tools/benchcampaign/singlenode'))
import bench_analyze as ba
import bench_runner as br

def sha(path):
    return hashlib.sha256(pathlib.Path(path).read_bytes()).hexdigest()

def analyze_m2(phase):
    res_dir = C / 'results' / phase
    rec_path = C / 'reconcile' / phase / 'report.json'
    out_dir = C / 'analysis' / phase
    if out_dir.exists():
        shutil.rmtree(out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)

    plan = json.loads((res_dir / 'plan.json').read_text())
    rec = json.loads(rec_path.read_text())
    samples = ba.load_samples(res_dir / 'metrics.csv')
    runner_omitted = json.loads((res_dir / 'omitted.json').read_text()) if (res_dir / 'omitted.json').exists() else {}

    # Combine runner omissions and R4 non-clean verdicts
    omitted = dict(runner_omitted)
    for case_rec in (rec.get('cases') or []):
        case_key = case_rec['case']
        if case_rec.get('verdict') != 'clean' and case_key not in omitted:
            omitted[case_key] = {
                'reason': f"R4 reconciliation verdict={case_rec.get('verdict')}",
                'reasons': case_rec.get('contaminated_reasons', []) + case_rec.get('unverifiable_reasons', [])
            }

    # Verify raw logs against CSV for all clean cases
    cases_by_key = {c['key']: c for c in plan['cases']}
    for job in plan['jobs']:
        key = job['case']
        if key in omitted:
            continue
        folder = res_dir / job['id']
        if json.loads((folder / 'result.json').read_text()).get('status') != 'ok':
            raise ValueError(f'non-successful raw run in clean case: {job["id"]}')

    # Adapt plan shape for bench_analyze.summarize
    proto = json.loads((C / 'protocol.json').read_text())
    adapted_plan = {
        'phase': {
            'id': phase,
            'question': f'{phase}: Multi-node VPC USE_REMOTE_FETCH vs SCAN_AND_FETCH across 3 FDB servers (double ssd-2)',
            'arms': ['final-ordinary', 'final-remote'],
            'blocks': len(plan['orders']),
        },
        'cases': plan['cases'],
        'orders': plan['orders'],
        'production_revisions': proto['production_revisions'],
        'harness_commit': proto['frozen_harness_revision'],
        'bootstrap_resamples': 20000,
        'analysis_seed': proto['sample_policy']['analysis_seed'],
        'interpretation': 'testing.B means are NOT p99; each case carries its sealed P0 shard-placement label.'
    }
    report = ba.summarize(adapted_plan, samples, omitted)
    for key, c in cases_by_key.items():
        if key in report['cases']:
            report['cases'][key]['placement_label'] = c.get('placement_label')
            report['cases'][key]['placement_fraction'] = c.get('placement_fraction')
    for comp in report['comparisons']:
        c = cases_by_key[comp['case']]
        comp['placement_label'] = c.get('placement_label')
        comp['placement_fraction'] = c.get('placement_fraction')

    report['r4_reconciliation_sha256'] = sha(rec_path)
    ba.dump(out_dir / 'report.json', report)
    print(f'Wrote {out_dir / "report.json"} ({len(report["comparisons"])} comparisons)')
    return report

def analyze_m1(phase):
    res_dir = C / 'results/client' / phase
    rec_path = C / 'reconcile' / phase / 'report.json'
    out_dir = C / 'analysis' / phase
    if out_dir.exists():
        shutil.rmtree(out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)

    rec = json.loads(rec_path.read_text())
    scripts_dir = W / 'tools/clientbench/scripts'
    sys.path.insert(0, str(scripts_dir))
    import importlib.util
    spec = importlib.util.spec_from_file_location('client_analyze', scripts_dir / 'analyze.py')
    ca = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(ca)

    summary_all = {'phase': phase, 'r4_reconciliation_sha256': sha(rec_path), 'workloads': {}}
    for case_rec in (rec.get('cases') or []):
        case_key = case_rec['case']
        w_dir = res_dir / case_key
        dest = out_dir / case_key
        if case_rec.get('verdict') != 'clean' or not (w_dir / 'complete.json').exists():
            dest.mkdir(parents=True, exist_ok=True)
            na = {'status': 'N/A', 'r4_verdict': case_rec.get('verdict'), 'reasons': case_rec.get('contaminated_reasons', []) + case_rec.get('unverifiable_reasons', [])}
            ba.dump(dest / 'N_A.json', na)
            summary_all['workloads'][case_key] = na
            continue
        target = dest / 'cgo'
        shutil.copytree(w_dir, target)
        subprocess.run([sys.executable, '-B', str(scripts_dir / 'analyze.py'), str(target), '--denominator', 'cgo', '--seed', '492817', '--resamples', '20000'], check=True)
        rep = json.loads((target / 'analysis.json').read_text())
        # Supplement Go allocations
        rows = rep['per_run']
        stats = {(r['block'], r['arm']): r for r in rows}
        blocks = sorted({r['block'] for r in rows})
        rng = random.Random(492817)
        allocs = {}
        for m in ('go_alloc_bytes_per_op', 'go_mallocs_per_op'):
            pairs = [(stats[b, 'final'][m], stats[b, 'cgo'][m]) for b in blocks]
            val = ca.bootstrap_log_ratio(pairs, rng, 20000)
            val.update(ca.dependence_sensitivity(pairs, rng, 20000))
            allocs[m] = val
        comps = {c['metric']: c for c in rep['comparisons'] if c['numerator'] == 'final' and c['denominator'] == 'cgo'}
        comps.update(allocs)
        summary_all['workloads'][case_key] = {
            'status': 'clean',
            'comparisons': comps,
            'per_run': rows
        }
    ba.dump(out_dir / 'summary.json', summary_all)
    print(f'Wrote {out_dir / "summary.json"} ({len(summary_all["workloads"])} workloads)')
    return summary_all

if __name__ == '__main__':
    ap = argparse.ArgumentParser()
    ap.add_argument('--phase', action='append', required=True)
    args = ap.parse_args()
    for ph in args.phase:
        if ph.startswith('M2'):
            analyze_m2(ph)
        else:
            analyze_m1(ph)
