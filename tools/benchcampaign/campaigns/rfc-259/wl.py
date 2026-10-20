import json, subprocess, sys
def snap():
    out = subprocess.run(['docker','exec','perf-fdb','fdbcli','--timeout','10','--exec','status json'], capture_output=True, text=True).stdout
    w = json.loads(out)['cluster']['workload']
    flat = {}
    for g, d in w.items():
        for k, v in d.items():
            flat[f'{g}.{k}'] = v['counter']
    return flat
binary, workload, n = sys.argv[1], sys.argv[2], int(sys.argv[3])
import time; time.sleep(12); a = snap()
subprocess.run(['./run.sh', binary, workload, '32', str(n), 'wltmp'], check=True)
time.sleep(12); b = snap()
tot = n + n // 20
keys = ['operations.read_requests', 'operations.writes', 'operations.location_requests', 'transactions.started', 'transactions.committed', 'transactions.conflicted', 'bytes.read', 'bytes.written']
print(binary.split('/')[-1], workload, ' '.join(f"{k.split('.')[-1]}={(b[k]-a[k])/tot:.2f}" for k in keys))
