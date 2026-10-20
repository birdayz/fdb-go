import subprocess, sys, collections, statistics
SPID = "1894562"
EV = ['task-clock','cycles','instructions','context-switches','raw_syscalls:sys_enter']
def run(binary, wl, n):
    out = subprocess.run(['perf','stat','-x,','-p',SPID,'-e',','.join(EV),'--','./run.sh',binary,wl,'32',str(n),'hwtmp'], capture_output=True, text=True).stderr
    r = {}
    for line in out.splitlines():
        f = line.split(',')
        if len(f) > 2 and f[0] and f[0][0].isdigit(): r[f[2].split(':')[-1]] = float(f[0])
    return r
wl = sys.argv[1]; N1, N2 = 10000, 50000
res = collections.defaultdict(list)
for rep in range(3):
    for arm in ['go-base', 'cgo']:
        a = run('bin/clientbench-'+arm, wl, N1); b = run('bin/clientbench-'+arm, wl, N2)
        d = (N2 - N1) * 1.05
        res[arm].append({k: (b[k] - a[k]) / d for k in a})
for arm, rows in res.items():
    m = {k: statistics.median(r[k] for r in rows) for k in rows[0]}
    print(f"{wl:8s} {arm:8s} time={m['task-clock']*1000:6.1f}us cycles={m['cycles']:8.0f} instr={m['instructions']:8.0f} GHz={m['cycles']/(m['task-clock']*1e6):.2f} IPC={m['instructions']/m['cycles']:.2f} ctxsw={m['context-switches']:.2f} syscalls={m['sys_enter']:.2f}")
