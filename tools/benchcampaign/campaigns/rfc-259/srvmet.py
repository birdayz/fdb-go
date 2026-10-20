import glob, re, subprocess, sys, time, os, collections
L = os.environ.get('FDB_LOGS', '${FDB_PERF_HOME}/cpu-work/fdb/logs')
EV_RE = re.compile(r'<Event ([^>]*)/>')
ATTR_RE = re.compile(r'(\w+)="([^"]*)"')
CUM = {
 'GrvProxyMetrics': ['TxnRequestIn', 'TxnStartIn', 'TxnStartBatch', 'TxnDefaultPriorityStartIn', 'TxnSystemPriorityStartIn', 'TxnBatchPriorityStartIn'],
 'ProxyMetrics': ['TxnCommitIn', 'CommitBatchIn', 'Mutations', 'ConflictRanges', 'MutationBytes', 'KeyServerLocationIn'],
 'ResolverMetrics': ['ResolveBatchIn', 'ResolvedBytes', 'ResolvedReadConflictRanges', 'ResolvedWriteConflictRanges'],
 'MasterMetrics': ['GetLiveCommittedVersionRequests', 'GetCommitVersionRequests'],
 'StorageMetrics': ['QueryQueue', 'GetValueQueries', 'GetRangeQueries', 'UpdateBatches'],
 'TLogMetrics': ['NonEmptyPeeks', 'EmptyPeeks', 'BytesInput'],
}
NET = ['RunLoopIterations', 'TasksExecuted', 'ASIOEventsProcessed', 'TimersExecuted']
def events():
    """Per log directory (one per server process): sorted (time, type, attrs)."""
    out = {}
    for d in L.split(','):
        ev = []
        for f in sorted(glob.glob(d + '/trace.*.xml')):
            with open(f, errors='replace') as fh:
                for m in EV_RE.finditer(fh.read()):
                    a = dict(ATTR_RE.findall(m.group(1)))
                    t = a.get('Type')
                    if t in CUM or t == 'NetworkMetrics':
                        ev.append((float(a['Time']), t, a))
        ev.sort(key=lambda e: e[0])
        out[d] = ev
    return out
def latest(evs):
    """Per process and metric type, the cumulative counters of its latest event."""
    tot = {}
    for d, ev in evs.items():
        for t, typ, a in ev:
            if typ in CUM:
                for k in CUM[typ]: tot[d + '|' + typ + '.' + k] = int(a[k].split()[-1])
    return tot
def net_sum(evs, t0, t1):
    s = collections.Counter()
    for d, ev in evs.items():
        for t, typ, a in ev:
            if typ == 'NetworkMetrics' and t0 < t <= t1:
                for k in NET: s[k] += int(a.get(k, 0))
    return s
def measure(binary, wl, workers, n, env):
    # Every process emits each metric every 5 s. Idle 6 s on both sides of the
    # run so that the snapshots before and after bracket it in every process.
    time.sleep(6)
    t0 = time.time(); s0 = latest(events())
    subprocess.run(['./run.sh', binary, wl, str(workers), str(n), 'mettmp'], check=True, env=dict(os.environ, **env))
    time.sleep(6)
    t1 = time.time(); evs = events(); s1 = latest(evs)
    tx = n + n // 20
    r = collections.Counter()
    for k in s0:
        if k in s1: r[k.split('.')[-1]] += (s1[k] - s0[k]) / tx
    r.update({k: v / tx for k, v in net_sum(evs, t0 - 5, t1).items()})
    return r
if __name__ == '__main__':
    wl, n = sys.argv[1], int(sys.argv[2])
    for spec in sys.argv[3:]:
        name, binary, *envs = spec.split(':')
        env = dict(e.split('=') for e in envs)
        r = measure(binary, wl, int(os.environ.get('WORKERS', '32')), n, env)
        keys = sys.argv[0] and globals().get('KEYS') or ['TxnRequestIn', 'TxnStartBatch', 'GetLiveCommittedVersionRequests', 'CommitBatchIn', 'ResolveBatchIn', 'QueryQueue', 'UpdateBatches', 'NonEmptyPeeks', 'RunLoopIterations', 'TasksExecuted', 'ASIOEventsProcessed', 'TimersExecuted']
        print(f"{wl:8s} {name:8s} " + ' '.join(f"{k}={r.get(k, 0):.3f}" for k in keys), flush=True)
