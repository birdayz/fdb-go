#!/usr/bin/env python3
"""Materialize the preregistered order and measurement contracts, without running work.

--dir CAMPAIGN_DIR holds the version-1 protocol.json; it is copied to
protocol-v1.json and replaced by the sealed design, with orders.json beside it.
Phase order seeds are --order-seed-base + 100 * phase index (default 641001).
The network-sensitivity topology is recorded from FDB_BENCH_HOST_VETH_ADDR and
FDB_BENCH_PEER_VETH_ADDR (CIDR), FDB_BENCH_SERVER_ADDR (host:port), all required,
and FDB_BENCH_NETNS / FDB_BENCH_HOST_VETH / FDB_BENCH_PEER_VETH (defaults
fdb-bench-client / fdb-bench-host / fdb-bench-peer).
"""
import argparse
import itertools
import json
import os
from pathlib import Path
import random

parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
parser.add_argument('--dir', required=True, help='campaign directory containing the version-1 protocol.json')
parser.add_argument('--order-seed-base', type=int, default=641001)
args = parser.parse_args()


def required_env(name):
    value = os.environ.get(name, '')
    if not value:
        raise SystemExit(name+' is required')
    return value


HOST_ADDR = required_env('FDB_BENCH_HOST_VETH_ADDR')
PEER_ADDR = required_env('FDB_BENCH_PEER_VETH_ADDR')
SERVER = required_env('FDB_BENCH_SERVER_ADDR')
NETNS = os.environ.get('FDB_BENCH_NETNS', 'fdb-bench-client')
HOST_DEV = os.environ.get('FDB_BENCH_HOST_VETH', 'fdb-bench-host')
PEER_DEV = os.environ.get('FDB_BENCH_PEER_VETH', 'fdb-bench-peer')
ROOT = Path(args.dir).resolve()
p = json.loads((ROOT / 'protocol.json').read_text())
p['version'] = 2
p['status'] = 'design_sealed_runtime_and_harness_receipts_pending'
p['sample_policy'].update({
    'order_seed': args.order_seed_base,
    'analysis_seed': 391706,
    'bootstrap_resamples': 20000,
    'data_generation': 'No random data seed: frozen payload algorithms and exact ID ranges; arm namespaces differ but have equal byte lengths.',
    'absolute_estimand': 'Median of run-level statistics; do not pool transaction percentiles across processes.',
    'relative_estimand': 'For each case/metric, take median of the three within-process testing.B repeats per arm/block, then geometric mean of within-block numerator/denominator ratios. Client has one process per arm/block and uses that process statistic. Bootstrap resamples whole paired block log-ratios.',
    'interval_limits': 'Nominal exploratory percentile block-bootstrap 95% intervals, 20000 resamples, seed391706; six/eight independent blocks have limited resolution; no noninferiority, equivalence, production-capacity or multiplicity-adjusted claim.',
    'testing_b_scope': 'Reported ns/op and Go B/op exclude explicitly stopped-timer validation. This is not whole-process throughput or p99. Process wall time and CPU are retained separately, not relabelled measured-phase metrics.',
    'client_scope': 'Closed-loop completion latency includes retries, materialization and exact byte checks. p99 comparisons use run-level quantiles and paired blocks, not individual transactions as independent replicates. Go allocations exclude native libfdb_c heap. Lifecycle CPU scopes retain the clientbench definitions.',
})
p['runtime'].update({'fdb_memory_limit':'16GiB','fdb_storage_memory_limit':'8GiB','fdb_cache_memory':'2GiB','fdb_trace_format':'json','fdb_trace_total_limit':'256MiB'})
p['write_state'] = {
    'cluster_reset': 'Between fresh arm processes, after all clients exit, clear only this owned cluster user keyspace with fdbcli; await commit; cooldown then quiescence. Never touch system keys. A before-state snapshot and after-state metadata are retained.',
    'record_layer': 'Clear each benchmark subspace at every body invocation, including calibration, outside timer. Measured write body is fixed1000x: IDs0..999, one transaction+commit per record for SaveRecord/SaveRecordWithIndex/SaveSplitRecord. Untimed verification checks exact presence/population. No insert/overwrite mix is reported as one population.',
    'sql_insert': 'Fresh schema for each body invocation. Fixed1000x: IDs1..1000, timestamp seconds cycle0..59 and deterministic evt labels, one autocommit INSERT per iteration. Verify count1000 and endpoint IDs outside timer; schema allocation and teardown are untimed.',
    'client': 'Frozen Dataset.Seed and ValidateSeed before warmup. Set100 overwrites one prepared key per worker; batchset50 and batchset50x1000 overwrite50 distinct prepared worker-local keys per transaction. Deterministic warmup/measured payloads and exact persisted final-value validation. RYW is one noncommitted local Set+Get, not a durable transaction.',
    'client_units': {'grv':'one read-only transaction/GetReadVersion','get100/get1000/get10000':'one Transact callback/Get of the named value byte size; transaction API fast-path behavior is part of implementation','read1000':'one read-only transaction/Get of one1000-byte value, NOT1000 reads','range10/range100':'one transaction/range returning exactly10/100 8-byte values','set100':'one100-byte Set+commit','ryw':'one uncommitted local Set+Get','batchset50':'50x100-byte Sets+one commit','batchset50x1000':'50x1000-byte Sets+one commit'},
}
p['environment_policy'] = {
    'pre_arm_window_seconds':1,
    'pre_arm_thresholds': {'all_cpu_aggregate_busy':0.15,'housekeeping_aggregate_busy':0.30,'housekeeping_max_core_busy':0.80,'server_aggregate_busy':0.15,'server_max_core_busy':0.50,'client_aggregate_busy':0.05,'client_max_core_busy':0.15,'any_cpu_steal':0.01},
    'during_arm_cadence_seconds':1,
    'during_arm_receipts':'CPU/steal counters, pressure, memory, disk/network totals, benchmark process and FDB process stats; sampler and runner pinned to housekeeping CPUs'+p['runtime']['housekeeping_cpus']+'.',
    'environment_failure':'Pre-arm threshold/build/profile/install conflict aborts that case campaign before another arm. During-arm missing telemetry, OOM, server unavailability or >1% steal on a client/server CPU in a complete1-second interval marks the entire balanced block contaminated and aborts that case; retain complete and incomplete data, do not present it as clean or rerun one arm.',
    'historical_correctness_failure':'Do not patch old production to make it benchmarkable. Report that tree/case as correctness-failed/not-comparable, retain its data, omit that case ratio, and continue only other predeclared cases. A final-tree correctness failure stops measurement for investigation.',
    'restart_policy':'No automatic retry after adverse timing or an aborted campaign. Any justified environmental restart needs a separately named protocol/campaign version and retained original artifacts.',
}
p['remote_contract'] = {
    'fixture':'1000 Orders IDs0..999, price=id, quantity=id%7+1; VALUE price_idx; nil initial continuation; no store record versions; same committed fixture per process.',
    'small':'No padding, unsplit metadata, exactly1 data key-value per record.',
    'split':'100000-byte deterministic padding plus protobuf fields; split-enabled metadata; exactly2 data key-values per record verified on all1000 rows.',
    'limit1':'TupleRangeAll and ReturnedRowLimit1; expected ID0 forward,999 reverse, terminal ReturnLimitReached.',
    'rows10':'TupleRangeBetween((100),(110)); IDs100..109 forward,109..100 reverse; no row limit; terminal SourceExhausted.',
    'rows1000':'TupleRangeAll drained; IDs0..999 forward or reversed; terminal SourceExhausted.',
    'strictness':'Only SCAN_AND_FETCH and strict USE_REMOTE_FETCH are timed; no fallback. Before/after untimed audits check ordered IDs/fields/checksums/chunks; strict remote openings>0, index scans0, record loads0; ordinary remote0/index scans>0 and completed loads positive.',
    'skip':'Final executor only: Skip3 stays at the ordinary eligible leaf, while strict remote leaves use Skip0 and exactly one outer Skip. Exact row order and counter guards are mandatory; no store-level Skip masquerading as mapped fetch.',
    'sql':'Explicit method on warmed pinned connection, noncovering IDX_A EXPLAIN on measured and audited connections; audited timer reset after warmup; ordinary remote0/record loads>0; strict remote>0/index0/record loads0; measured connection has no timer.',
    'limitations':'One FDB process/storage role cannot establish cross-storage-process or cross-machine benefit. Logical remote openings are not wire RPC counts.',
}
p['network_sensitivity'] = {
    'topology':'Host '+HOST_DEV+HOST_ADDR+' -> namespace '+NETNS+'/'+PEER_DEV+PEER_ADDR+'; server'+SERVER+'; no namespace default route.',
    'server_to_client':'tc qdisc replace dev '+HOST_DEV+' root netem limit100000 delay250us',
    'client_to_server':'ip netns exec '+NETNS+' tc qdisc replace dev '+PEER_DEV+' root netem limit100000 delay250us',
    'loss_percent':0,'jitter_us':0,'reorder_percent':0,
    'verification':'Save both tc -s -j qdisc show receipts and a100-packet unloaded ping before the phase; nominal added RTT500us, report actual p50/p99 and no qdisc drops. Both arms use the identical topology and affect all external FDB traffic consistently.',
    'restore':'Delete both root qdiscs, save tc receipts and100-packet zero-delay ping proving restoration. No real WAN/multi-host performance claim.',
}

all_orders = {}
for number, phase in enumerate(p['phases']):
    pid = phase['id']
    phase['order_seed'] = args.order_seed_base + number * 100
    phase['data_seed'] = None
    phase['repeats_per_arm_process'] = 3
    phase['read_benchtime'] = '3s'
    phase['write_benchtime'] = '1000x'
    phase['warmup'] = 'Untimed committed fixture+exact audit; Go testing.B calibration retained in raw logs but not treated as a reported repeat.'
    if pid in ('A','A2'):
        phase['blocks'] = 6
        phase['repeats_per_arm_process'] = 1
        phase['iterations'] = 3000 * phase['workers']
        phase['warmup'] = 200 * phase['workers']
        phase['per_workload_campaign_wall_cap_seconds'] = 180
        phase['per_process_timeout_seconds'] = 180
        phase['read_benchtime'] = None
        phase['write_benchtime'] = None
    elif pid == 'C1':
        phase['blocks'] = 8
    else:
        phase['blocks'] = 6
    rng = random.Random(phase['order_seed'])
    arms = phase['arms']
    if len(arms) == 4:
        rows = [[0,1,3,2],[1,2,0,3],[2,3,1,0],[3,0,2,1]]
        orders = rows * 2
    else:
        orders = list(itertools.permutations(range(len(arms))))
        orders *= phase['blocks'] // len(orders)
    rng.shuffle(orders)
    actual = [[arms[i] for i in order] for order in orders]
    if len(actual) != phase['blocks']:
        raise ValueError('order population')
    all_orders[pid] = {'seed':phase['order_seed'],'orders':actual}
    phase['frozen_order_key'] = pid
    phase['nominal_observations_per_arm_per_case'] = phase['blocks'] * phase['repeats_per_arm_process']

(ROOT / 'protocol-v1.json').write_text((ROOT / 'protocol.json').read_text())
(ROOT / 'protocol.json').write_text(json.dumps(p, indent=2) + '\n')
(ROOT / 'orders.json').write_text(json.dumps(all_orders, indent=2) + '\n')
print('Design v2 and explicit arm orders written; timing still gated on runtime/harness receipts.')
