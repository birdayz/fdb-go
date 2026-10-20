import subprocess, sys, collections, statistics, os, json
sys.path.insert(0, '.'); import summ
SPID = "1894562"
EV = ['task-clock','cycles','instructions','raw_syscalls:sys_enter']
VARIANTS = {
  'base':     ('bin/clientbench-go-base', {}),
  'nodirect': ('bin/clientbench-go-exp', {'EXP_NODIRECT': '1'}),
  'coal10-20':('bin/clientbench-go-exp', {'EXP_NODIRECT': '1', 'EXP_COALESCE': '10,20'}),
  'grv-cpp':  ('bin/clientbench-go-exp', {'EXP_GRV': 'cpp'}),
  'grv-adapt':('bin/clientbench-go-exp', {'EXP_GRV': 'adaptive'}),
  'grv-cpp+coal':('bin/clientbench-go-exp', {'EXP_GRV': 'cpp', 'EXP_NODIRECT': '1', 'EXP_COALESCE': '10,20'}),
  'yield1':   ('bin/clientbench-go-exp', {'EXP_NODIRECT': '1', 'EXP_YIELD': '1'}),
  'yield3':   ('bin/clientbench-go-exp', {'EXP_NODIRECT': '1', 'EXP_YIELD': '3'}),
  'cpp+y1':   ('bin/clientbench-go-exp', {'EXP_NODIRECT': '1', 'EXP_YIELD': '1', 'EXP_GRV': 'cpp'}),
  'mp1-y1':   ('bin/clientbench-go-exp', {'EXP_DIRECT_MAXPENDING': '1', 'EXP_YIELD': '1'}),
  'mp1-ya':   ('bin/clientbench-go-exp', {'EXP_DIRECT_MAXPENDING': '1', 'EXP_YIELD': '-8'}),
  'ya':       ('bin/clientbench-go-exp', {'EXP_NODIRECT': '1', 'EXP_YIELD': '-8'}),
  'q20-ya':   ('bin/clientbench-go-exp', {'EXP_QUIET': '20', 'EXP_YIELD': '-8'}),
  'q50-ya':   ('bin/clientbench-go-exp', {'EXP_QUIET': '50', 'EXP_YIELD': '-8'}),
  'mp3-ya':   ('bin/clientbench-go-exp', {'EXP_DIRECT_MAXPENDING': '3', 'EXP_YIELD': '-8'}),
  'ns-coal':  ('bin/clientbench-go-exp', {'EXP_PRECISE': '1', 'EXP_NODIRECT': '1', 'EXP_COALESCE': '10,20'}),
  'ns-grv':   ('bin/clientbench-go-exp', {'EXP_PRECISE': '1', 'EXP_GRV': 'cpp'}),
  'ns-both':  ('bin/clientbench-go-exp', {'EXP_PRECISE': '1', 'EXP_GRV': 'cpp', 'EXP_NODIRECT': '1', 'EXP_COALESCE': '10,20'}),
  'hyb':      ('bin/clientbench-go-exp', {'EXP_PRECISE': '1', 'EXP_GRV': 'busy', 'EXP_QUIET': '20', 'EXP_COALESCE': '10,20'}),
  'hyb-grv':  ('bin/clientbench-go-exp', {'EXP_PRECISE': '1', 'EXP_GRV': 'busy'}),
  'hyb-w':    ('bin/clientbench-go-exp', {'EXP_PRECISE': '1', 'EXP_QUIET': '20', 'EXP_COALESCE': '10,20'}),
  'ns-both-busy': ('bin/clientbench-go-exp', {'EXP_PRECISE': '1', 'EXP_GRV': 'busy', 'EXP_NODIRECT': '1', 'EXP_COALESCE': '10,20'}),
  'busy2':    ('bin/clientbench-go-exp', {'EXP_PRECISE': '1', 'EXP_GRV': 'busy', 'EXP_BUSYWRITE': '1', 'EXP_COALESCE': '10,20'}),
  'serial2':  ('bin/clientbench-go-exp', {'EXP_PRECISE': '2', 'EXP_GRV': 'serial', 'EXP_BUSYWRITE': '1', 'EXP_COALESCE': '10,20'}),
  'cpp2':     ('bin/clientbench-go-exp', {'EXP_PRECISE': '2', 'EXP_GRV': 'cpp', 'EXP_NODIRECT': '1', 'EXP_COALESCE': '10,20'}),
  'sergrv':   ('bin/clientbench-go-exp', {'EXP_PRECISE': '2', 'EXP_GRV': 'serial'}),
  'cgo':      ('bin/clientbench-cgo', {}),
}
def run(var, wl, workers, n, out):
    binary, env = VARIANTS[var]
    e = dict(os.environ, **env)
    o = subprocess.run(['perf','stat','-x,','-p',SPID,'-e',','.join(EV),'--','./run.sh',binary,wl,str(workers),str(n),out], capture_output=True, text=True, env=e).stderr
    r = {}
    for line in o.splitlines():
        f = line.split(',')
        if len(f) > 2 and f[0] and f[0][0].isdigit(): r[f[2].split(':')[-1]] = float(f[0])
    return r
wl, workers, N1, N2 = sys.argv[1], int(sys.argv[2]), int(sys.argv[3]), int(sys.argv[4])
variants = sys.argv[5].split(',')
res = collections.defaultdict(list)
for rep in range(3):
    for var in variants:
        a = run(var, wl, workers, N1, 'exptmp'); b = run(var, wl, workers, N2, 'exptmp2')
        d = (N2 - N1) * 1.05
        r = {k: (b[k] - a[k]) / d for k in a}
        c = summ.load('exptmp2')
        r.update({'cli_cpu': c['win_us'], 'mean': c['mean_us'], 'p99': c['p99_us'], 'tps': c['tps']})
        res[var].append(r)
print(f"{wl} workers={workers}")
print(f"  {'variant':10s} {'srv_us':>7s} {'srv_cyc':>8s} {'srv_sc':>6s} {'cli_us':>7s} {'mean_us':>8s} {'p99_us':>8s} {'tx/s':>7s}")
for var in variants:
    rows = res[var]; m = {k: statistics.median(r[k] for r in rows) for k in rows[0]}
    print(f"  {var:10s} {m['task-clock']*1000:7.1f} {m['cycles']:8.0f} {m['sys_enter']:6.2f} {m['cli_cpu']:7.1f} {m['mean']:8.0f} {m['p99']:8.0f} {m['tps']:7.0f}")
