import json, glob, os, datetime, statistics, collections, bisect
A = '${FDB_PERF_HOME}/hcloud-multinode-vpc-20261014'
def busy(stat):
    f = stat.split('\n', 1)[0].split()[1:]
    v = list(map(int, f))
    return v[0] + v[1] + v[2] + v[5] + v[6]  # user nice system irq softirq (jiffies)
def load_node(phase, node):
    ts, bs = [], []
    with open(f'{A}/telemetry/{phase}/{node}/samples.jsonl') as fh:
        for line in fh:
            s = json.loads(line)
            ts.append(s['realtime_ns']); bs.append(busy(s['stat']))
    return ts, bs
def utc(s): return int(datetime.datetime.fromisoformat(s).timestamp() * 1e9)
out = collections.defaultdict(lambda: collections.defaultdict(list))
for phase in ['M1w1', 'M1w8', 'M1w32', 'M1w64']:
    nodes = {n: load_node(phase, n) for n in ['node1', 'node2', 'node3']}
    runs = []
    for res in glob.glob(f'{A}/results/client/{phase}/*/b*/result.json'):
        d = os.path.dirname(res); r = json.load(open(res))
        try: s = json.load(open(d + '/summary.json'))
        except Exception: continue
        if not s.get('success'): continue
        runs.append((utc(r['started_utc']), utc(r['ended_utc']), s['backend']['backend'], s['config']['workload'], s['completed'] + s['warmup_completed']))
    runs.sort()
    # idle rate: gaps between consecutive runs longer than 3 s, interior samples only
    idle = []
    for (s0, e0, *_), (s1, *_rest) in zip(runs, runs[1:]):
        for n, (ts, bs) in nodes.items():
            i = bisect.bisect_left(ts, e0 + int(0.5e9)); j = bisect.bisect_right(ts, s1 - int(0.5e9)) - 1
            if j > i and ts[j] - ts[i] > 1e9:
                idle.append((n, (bs[j] - bs[i]) / ((ts[j] - ts[i]) / 1e9)))
    idle_rate = {n: statistics.median([r for m, r in idle if m == n]) for n in nodes}
    for start, end, backend, wl, ntx in runs:
        tot = 0.0
        for n, (ts, bs) in nodes.items():
            i = bisect.bisect_right(ts, start) - 1; j = bisect.bisect_left(ts, end)
            if i < 0 or j >= len(ts): break
            tot += (bs[j] - bs[i]) - idle_rate[n] * (ts[j] - ts[i]) / 1e9
        else:
            out[(phase, wl)][backend].append(tot * 10000 / ntx)  # jiffies -> us
    print(phase, 'idle jiffies/s per node:', {n: round(v, 1) for n, v in idle_rate.items()})
print(f"{'phase':6s} {'workload':16s} {'go us/tx (6 runs)':>34s} {'cgo us/tx (6 runs)':>34s}  go/cgo(median)")
for (phase, wl), d in sorted(out.items()):
    g, c = sorted(d.get('go', [])), sorted(d.get('cgo', []))
    if g and c:
        print(f"{phase:6s} {wl:16s} {' '.join(f'{x:5.0f}' for x in g):>34s} {' '.join(f'{x:5.0f}' for x in c):>34s}  {statistics.median(g)/statistics.median(c):.2f}")
