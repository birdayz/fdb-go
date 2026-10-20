import subprocess, sys, collections
SPID = "1894562"
EV = ['task-clock','raw_syscalls:sys_enter','syscalls:sys_enter_recvmsg','syscalls:sys_enter_recvfrom','syscalls:sys_enter_read','syscalls:sys_enter_sendmsg','syscalls:sys_enter_sendto','syscalls:sys_enter_write','syscalls:sys_enter_writev','syscalls:sys_enter_epoll_wait','syscalls:sys_enter_epoll_pwait','syscalls:sys_enter_futex']
def run(binary, wl, n):
    out = subprocess.run(['perf','stat','-x,','-p',SPID,'-e',','.join(EV),'--','./run.sh',binary,wl,'32',str(n),'margtmp'], capture_output=True, text=True).stderr
    r = {}
    for line in out.splitlines():
        f = line.split(',')
        if len(f) > 2 and f[0] and f[0][0].isdigit():
            r[f[2]] = float(f[0])
    return r
res = collections.defaultdict(list)
wl = sys.argv[1]; arms = sys.argv[2:]
N1, N2 = 10000, 50000
for rep in range(3):
    for arm in arms:
        a = run('bin/clientbench-'+arm, wl, N1); b = run('bin/clientbench-'+arm, wl, N2)
        d = (N2 - N1) * 1.05
        res[arm].append({k: (b.get(k,0) - a.get(k,0)) / d for k in EV})
for arm in arms:
    rows = res[arm]
    cpu = [r['task-clock'] * 1000 for r in rows]
    s = ' '.join(f"{k.split('enter_')[-1].replace('raw_syscalls:sys_enter','total')}={sum(r[k] for r in rows)/len(rows):.2f}" for k in EV[1:] if sum(r[k] for r in rows)/len(rows) >= 0.05)
    print(f"{wl} {arm}: server_cpu_us/tx={' '.join(f'{c:.1f}' for c in cpu)} | {s}")
