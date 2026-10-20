#!/usr/bin/env python3
"""Freeze the two R7 follow-up plans before observing either campaign."""
import copy
import json
from pathlib import Path
here = Path(__file__).resolve().parent
base = json.loads((here.parent / 'work-pr2-r6/r6-batchset50x1000-plan.json').read_text())
for campaign, seed, prefix in [('a', 787106, 'r7a1'), ('b', 787206, 'r7b1')]:
    p = copy.deepcopy(base)
    p['campaign'] = 'pr2-r7-' + campaign
    p['seed'] = seed
    p['purpose'] = 'Root846/perf-lead R7: two prespecified follow-ups to R6 large-value p99; reuse exact R6 old/new/CGo binaries with no builds. Retain every outcome.'
    p['budget_rationale'] = 'Exactly 2000 measured/200 warmup, six blocks/all six arm orders, independent fixed order seed for each of two additional campaigns, separate 180s caps. Never rerun to obtain significance or fit budget.'
    p['interpretation'] = 'Aggregate existing R6 large-value campaign plus both R7 campaigns: 18 paired blocks, 10000 percentile block-bootstrap replicates. Exploratory same-VM evidence conditional on these binaries/workload; campaigns have independently shuffled schedules, not independent physical hosts.'
    p['r7_analysis'] = {
        'bootstrap_seed': 787707,
        'bootstrap_resamples': 10000,
        'primary_resampling_unit': 'whole three-arm block; sample 18 block indices with replacement, reuse indices across metrics and comparisons',
        'metrics': ['p99_ns', 'p999_ns', 'p90_ns', 'p50_ns', 'process_cpu_ns_per_op'],
        'comparisons': ['new/old', 'new/cgo'],
        'quantile': 'nearest rank, sorted[ceil(p*n)-1]',
        'top_one_percent': 'exact largest ceil(0.01*n) observations per run, descending latency then ascending operation index to resolve ties',
        'neighbor_fraction': 'fraction of those top observations with at least one distinct top observation at index distance <=20; no wraparound, warmup excluded',
        'interpretation': 'clustering is descriptive and cannot identify the cause or establish periodicity; intervals are nominal, no multiplicity correction or noninferiority margin; R6 prompted the follow-up',
    }
    for arm in p['arms']:
        idx = arm['command'].index('--prefix') + 1
        arm['command'][idx] = arm['command'][idx].replace('/r6l1/', '/' + prefix + '/')
    (here / ('r7-' + campaign + '-plan.json')).write_text(json.dumps(p, indent=2) + '\n')
