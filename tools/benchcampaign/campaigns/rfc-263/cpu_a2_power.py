#!/usr/bin/env python3
"""Replay A2's power forecast from the pinned A1 pilot; never launch workloads."""
import argparse
import fcntl
import hashlib
import json
import math
import os
from pathlib import Path
import statistics

RESULTS_SHA256 = '6c700352c97e190fbc1cadf7d0a597fa6eb3f16a10bf7fb06144c9a82377deeb'
INDEX_SHA256 = '6c4cd85cd82589cc11dcc6d11721d5511b0d649a59e0b26331a8cf1b0964d8c5'
ENDPOINTS = ('window_cpu_us_per_tx', 'lifecycle_cpu_us_per_tx')
CPUS = {0, 1, 6, 7, 11, 12, 13, 18, 19, 23}


def require(ok, message):
    if not ok:
        raise ValueError(message)


def sha(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def project(results, pairs):
    from scipy import integrate, stats
    n = 40
    selected = [p for p in pairs if p['cell'] == 'grv-w8' and p['contrast'] == 'AB']
    require(len(selected) == 12 and len({p['pair'] for p in selected}) == 12, '12 distinct pilot AB pairs required')
    data, covariances = {}, []
    for epoch in (1, 2):
        rows = [p for p in selected if p['epoch'] == epoch]
        require(len(rows) == 6, 'six pilot pairs per epoch required')
        data[epoch] = [[p['logs'][name] for p in rows] for name in ENDPOINTS]
        require(all(type(x) in (int, float) and math.isfinite(x) for values in data[epoch] for x in values), 'invalid pilot log')
        covariances.append(statistics.covariance(*data[epoch]))
    variances = {name: [statistics.variance(data[e][i]) for e in (1, 2)] for i, name in enumerate(ENDPOINTS)}
    require(all(sum(v) > 0 for v in variances.values()), 'nonzero pilot variance required')
    rho = sum(covariances) / math.sqrt(math.prod(sum(variances[name]) for name in ENDPOINTS))
    require(-1 - 1e-12 <= rho <= 1 + 1e-12, 'invalid pilot correlation')
    rho = min(1, max(-1, rho))
    # Exactly linear inputs can round one ulp short of the singular boundary.
    if 1 - abs(rho) <= 4 * math.ulp(1.0):
        rho = math.copysign(1, rho)
    endpoints, limits = {}, []
    for name in ENDPOINTS:
        pilot = results['primary_and_coprimary'][name]['grv-w8']
        reported = pilot['epoch_variances_log']
        require(len(reported) == 2 and all(type(x) in (int, float) and math.isfinite(x) and x >= 0 for x in reported), 'invalid reported pilot variance')
        require(variances[name] == reported, 'pilot pair/report variance mismatch')
        v = variances[name]
        se = math.sqrt(sum(v) / n) / 2
        df = sum(v) ** 2 / sum(x * x / (n - 1) for x in v)
        critical = float(stats.t.ppf(.975, df))
        h = critical * se
        limit = math.log(1.02) / se - critical
        limits.append(limit)
        point = pilot['point_ratio']
        require(type(point) in (int, float) and math.isfinite(point) and point > 0, 'invalid pilot point')
        endpoints[name] = dict(se_log=se, df=df, critical=critical, log_half_width=h,
            ratio_half_width_at_A1_center=point * math.sinh(h), ratio_half_width_at_margin=1.02 * math.sinh(h),
            normal_plugin_power_true_1=float(stats.norm.cdf(limit)),
            welch_noncentral_t_approx_power_true_1=float(stats.nct.sf(critical, df, math.log(1.02) / se)),
            normal_plugin_power_at_A1_point=float(stats.norm.cdf((math.log(1.02) - math.log(point)) / se - critical)),
            forecast_CI_at_A1_center=[point * math.exp(-h), point * math.exp(h)])
    if rho == 1:
        joint, error = float(stats.norm.cdf(min(limits))), 0
    elif rho == -1:
        joint, error = max(0, float(stats.norm.cdf(limits[0]) - stats.norm.cdf(-limits[1]))), 0
    else:
        def integrand(x):
            return math.exp(-x * x / 2) / math.sqrt(2 * math.pi) * float(stats.norm.cdf((limits[1] - rho * x) / math.sqrt(1 - rho * rho)))
        joint, error = integrate.quad(integrand, -math.inf, limits[0], epsabs=1e-11, epsrel=1e-11)
    return dict(pairs_per_epoch=n, total_AB_pairs=2 * n, endpoints=endpoints,
        epoch_cross_covariances_log=covariances, within_epoch_pooled_correlation=rho,
        joint_normal_plugin_power_true_1=joint, quadrature_error_estimate=error,
        independent_normal_plugin_joint_for_context=math.prod(v['normal_plugin_power_true_1'] for v in endpoints.values()),
        assumptions=['Independent approximately normal paired logs within each epoch',
            'Future epoch variances and cross-covariances equal the six-pair-per-epoch A1 pilot estimates',
            'Power alternative is true ratio 1.00 for both endpoints',
            'Joint normal plug-in uses fixed pilot covariance and projected Welch critical values; not exact joint Welch power',
            'Forecast only; no sample-size extension or new measurements'])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('results', 'pairs', 'index', 'lock'):
        parser.add_argument('--' + name, type=Path, required=True)
    args = parser.parse_args()
    require(os.getpriority(os.PRIO_PROCESS, 0) == 19 and set(os.sched_getaffinity(0)) == CPUS, 'nice19/reserved affinity required')
    with args.lock.open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        buffers = {name: path.read_bytes() for name, path in (('results', args.results), ('index', args.index), ('pairs', args.pairs))}
        hashes = {name: hashlib.sha256(data).hexdigest() for name, data in buffers.items()}
        require(hashes['results'] == RESULTS_SHA256 and hashes['index'] == INDEX_SHA256, 'pinned A1 planning input changed')
        index = json.loads(buffers['index'])
        require(index.get('results.json') == RESULTS_SHA256 and index.get('pair-rows.json') == hashes['pairs'], 'pilot artifact index mismatch')
        report = project(json.loads(buffers['results']), json.loads(buffers['pairs']))
        require(all(sha(path) == hashes[name] for name, path in (('results', args.results), ('index', args.index), ('pairs', args.pairs))), 'planning input drift')
        import scipy
        report.update(input_results_sha256=hashes['results'], input_pair_rows_sha256=hashes['pairs'], input_index_sha256=hashes['index'],
                      planner_sha256=sha(__file__), scipy_version=scipy.__version__)
        print(json.dumps(report, indent=2, sort_keys=True, allow_nan=False))


if __name__ == '__main__':
    main()
