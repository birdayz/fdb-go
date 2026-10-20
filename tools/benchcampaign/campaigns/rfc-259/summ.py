import json, sys, csv, statistics
def load(d):
    s = json.load(open(d + '/summary.json'))
    n = s['completed']
    b, a = s['stats_before'], s['stats_after']
    ident = s['stats_after_identity']; sh = s['lifecycle']['stats_after_shutdown']
    cpu = lambda x: x['process_user_cpu_us'] + x['process_system_cpu_us']
    lat = []
    with open(d + '/samples.csv') as f:
        for r in csv.DictReader(f):
            lat.append(int(r['latency_ns']))
    lat.sort()
    return dict(
        n=n,
        win_us=(cpu(a) - cpu(b)) / n,
        user_us=(a['process_user_cpu_us'] - b['process_user_cpu_us']) / n,
        sys_us=(a['process_system_cpu_us'] - b['process_system_cpu_us']) / n,
        ea_us=(cpu(sh) - cpu(ident)) / n,
        mallocs=(a['go_mallocs'] - b['go_mallocs']) / n,
        bytes=(a['go_total_alloc_bytes'] - b['go_total_alloc_bytes']) / n,
        gcs=a['go_gc_cycles'] - b['go_gc_cycles'],
        tps=n / (s['measured_wall_ns'] / 1e9),
        mean_us=statistics.fmean(lat) / 1e3,
        p99_us=lat[int(len(lat) * 0.99)] / 1e3,
    )
if __name__ == '__main__':
    print(f"{'run':38s} {'win':>6s} {'user':>6s} {'sys':>6s} {'E-A':>6s} {'mallocs':>7s} {'bytes':>7s} {'gc':>3s} {'tx/s':>7s} {'mean':>7s} {'p99':>7s}")
    for d in sys.argv[1:]:
        r = load(d)
        print(f"{d.rstrip('/').split('/')[-1]:38s} {r['win_us']:6.1f} {r['user_us']:6.1f} {r['sys_us']:6.1f} {r['ea_us']:6.1f} {r['mallocs']:7.1f} {r['bytes']:7.0f} {r['gcs']:3d} {r['tps']:7.0f} {r['mean_us']:7.0f} {r['p99_us']:7.0f}")
