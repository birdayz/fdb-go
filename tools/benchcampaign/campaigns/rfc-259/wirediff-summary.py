import json, statistics, sys
from collections import defaultdict
rows=[json.loads(l) for l in open(sys.argv[1])]
g=defaultdict(list)
for r in rows: g[(r['workload'],r['workers'],r['label'].split('-')[0])].append(r)
def val(r,k):
    if k=='server.cycles': return sum(r.get(f'{x}.cycles',0) for x in ('stateless','transaction','storage'))
    if k=='server.recv': return sum(r.get(f'{x}.recvfrom',0)+r.get(f'{x}.read',0) for x in ('stateless','transaction','storage'))
    if k=='server.send': return sum(r.get(f'{x}.sendmsg',0) for x in ('stateless','transaction','storage'))
    if k=='server.epoll': return sum(r.get(f'{x}.epoll_wait',0) for x in ('stateless','transaction','storage'))
    if k=='client.sendcalls': return r.get('client.write',0)+r.get('client.sendmsg',0)+r.get('client.writev',0)
    if k=='client.recvcalls': return r.get('client.read',0)+r.get('client.recvfrom',0)
    if k=='client.segs': return sum(r.get(f'client_segs_to_{p}',0) for p in ('4721','4722','4723'))
    return r.get(k,0)
keys=['server.cycles','stateless.cycles','transaction.cycles','storage.cycles','server.recv','server.send','server.epoll',
      'client.sendcalls','client.recvcalls','client.segs','client_segs_to_4721','client_segs_to_4722','client_segs_to_4723',
      'trace.GrvProxyMetrics.TxnRequestIn','trace.ProxyMetrics.CommitBatchIn','trace.ProxyMetrics.TxnCommitIn',
      'trace.StorageMetrics.QueryQueue','client.cycles','tx_per_s']
print(f'runs: {len(rows)}')
for wl,wk in sorted({(a,b) for a,b,_ in g}):
    print(f'\n== {wl}/{wk}  (median of {len(g[(wl,wk,"go")])} go, {len(g[(wl,wk,"cgo")])} cgo)')
    print(f'{"metric":38s}{"go":>12s}{"cgo":>12s}{"go/cgo":>9s}')
    for k in keys:
        a=statistics.median(val(r,k) for r in g[(wl,wk,'go')]); b=statistics.median(val(r,k) for r in g[(wl,wk,'cgo')])
        ratio=f'{a/b:9.2f}' if b else '        -'
        fmt=(lambda x:f'{x:12.0f}') if 'cycles' in k or k=='tx_per_s' else (lambda x:f'{x:12.3f}')
        print(f'{k:38s}{fmt(a)}{fmt(b)}{ratio}')
