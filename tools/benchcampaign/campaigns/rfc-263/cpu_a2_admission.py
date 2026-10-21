"""Import-pure A2 source-equivalence and post-timing PASS receipt guards."""
from copy import deepcopy
from pathlib import Path, PurePosixPath
import math
import re

import cpu_a2_spec as spec

PYTHON_FILES = {
    'cpu_collect.py', 'cpu_analyze.py', 'cpu_guard_test.py', 'cpu_queuewait_guard_test.py',
    'rev6-cpu-queuewait.py', 'cpu_a2_spec.py', 'cpu_a2_admission.py', 'cpu_a2_analyze.py',
    'cpu_a2_guard_test.py', 'cpu_a2_power.py',
}


def allowed_change(path):
    pure = PurePosixPath(path)
    if pure.is_absolute() or '..' in pure.parts or str(pure) != path:
        return False
    if path in ('TODO.md', 'pr-body.md'):
        return True
    if path.startswith('docs/benchmarks/rfc-263/') and pure.suffix == '.md':
        return True
    if pure.parent == PurePosixPath('rfcs') and pure.name.startswith('263-') and pure.name.endswith('.md'):
        return True
    return pure.parent == PurePosixPath('tools/benchcampaign/campaigns/rfc-263') and pure.name in PYTHON_FILES


def source_equivalence(current, prior):
    spec.require(isinstance(current['files'], dict) and isinstance(prior['files'], dict) and
                 len(current['files']) > 1000 and len(prior['files']) > 1000, 'nonempty full source populations required')
    spec.require(isinstance(current['external_configs'], dict) and bool(current['external_configs']) and
                 current['external_configs'] == prior['external_configs'], 'A2 external build/user configuration drift')
    changed = sorted(path for path in set(current['files']) | set(prior['files']) if current['files'].get(path) != prior['files'].get(path))
    spec.require(bool(changed) and all(allowed_change(path) for path in changed), 'A2 source drift outside authorized doc/Python paths or empty change census')
    return dict(prior_files=len(prior['files']), current_files=len(current['files']), changed_paths=changed,
                external_configs_identical=True)


def historical_pins(frozen):
    return dict(helper_hashes=frozen['helper_hashes'], lifecycle_helper_sha256=frozen['lifecycle_helper_sha256'],
                pprof_sha256=frozen['pprof_sha256'], cluster_sha256=frozen['hashes'][str(Path(frozen['cluster']).resolve())],
                baseline_source_sha256=frozen['source_archives']['baseline']['sha256'])


def historical_inputs(current, prior):
    expected = historical_pins(prior)
    spec.require(set(current) == set(expected), 'historical A1 input pin population incomplete')
    for name, pin in expected.items():
        spec.require(current[name] == pin, 'A2 historical input pin changed: ' + name)


def passing_interval(row, n):
    spec.require(all(type(row[k]) in (int, float) and math.isfinite(row[k]) and row[k] > 0
                     for k in ('point_ratio', 'lower_ratio', 'upper_ratio')) and
                 row['lower_ratio'] <= row['point_ratio'] <= row['upper_ratio'] and
                 row['point_ratio'] < 1.02 and row['upper_ratio'] <= 1.02 and
                 row['confidence'] == .95 and row['epoch_counts'] == [n, n] and all(type(count) is int for count in row['epoch_counts']), 'A2 confirmation interval does not pass')


def confirmation(result, frozen, frozen_sha256, epoch_seals, retained):
    spec.validate_identities(frozen['identities'])
    spec.require(isinstance(frozen_sha256, str) and re.fullmatch('[0-9a-f]{64}', frozen_sha256) and set(epoch_seals) == {'1', '2'} and
                 all(isinstance(pin, str) and re.fullmatch('[0-9a-f]{64}', pin) for pin in epoch_seals.values()), 'full confirmation seals required')
    spec.require(result['timing_design'] == spec.DESIGN and result['seed'] == spec.SEED and
                 result['processes'] == 176 and result['pairs'] == 88 and result['fresh_ab_pairs'] == 80 and result['fresh_aa_pairs'] == 8 and
                 result['measured_transactions'] == 3520000 and result['warmup_transactions'] == 352000 and
                 result['target_cell'] == spec.TARGET and result['target_ab_pairs'] == 80 and
                 result['target_ab_pairs_per_epoch'] == [40, 40] and result['aa_pairs_per_role'] == 4, 'wrong A2 confirmation population')
    spec.require(result['frozen_sha256'] == frozen_sha256 and result['epoch_seals'] == epoch_seals and set(epoch_seals) == {'1', '2'},
                 'A2 confirmation frozen/epoch seal binding mismatch')
    spec.require(result['identities'] == frozen['identities'], 'A2 confirmation source/binary identity mismatch')
    for key in ('target_decision', 'decision'):
        spec.require(result[key]['verdict'] == 'PASS' and result[key]['park'] is False, 'A2 diagnostics require target and combined PASS')
    target, combined = result['target_primary_and_coprimary'], result['primary_and_coprimary']
    spec.require(set(target) == set(spec.ENDPOINTS) and set(combined) == set(spec.ENDPOINTS) and
                 all(set(combined[metric]) == spec.CELLS for metric in spec.ENDPOINTS), 'incomplete A2 confirmation bound population')
    spec.require(result['ab_pairs_by_cell'] == {cell: (80 if cell == spec.TARGET else 12) for cell in spec.CELLS}, 'confirmation sample-size matrix changed')
    meta = result['retained_a1']
    spec.require(meta['frozen_sha256'] == spec.A1_FROZEN_SHA256 and meta['results_sha256'] == spec.RESULTS_SHA256 and meta['index_sha256'] == spec.INDEX_SHA256 and
                 meta['bounds'] == 14 and meta['cells'] == 7 and meta['identities'] == retained['identities'], 'retained A1 confirmation binding mismatch')
    for metric in spec.ENDPOINTS:
        passing_interval(target[metric], 40)
        spec.require(combined[metric][spec.TARGET] == target[metric], 'target confirmation was pooled or replaced')
        for cell in spec.CELLS - {spec.TARGET}:
            passing_interval(combined[metric][cell], 6)
            spec.require(combined[metric][cell] == retained['primary_and_coprimary'][metric][cell], 'retained A1 bound replaced in confirmation')
    return deepcopy(result)
