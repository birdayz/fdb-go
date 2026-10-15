#!/usr/bin/env python3
"""Pre-timing runtime/source receipts; no builds, installs or benchmark invocation.

Runs on the VM. FDB_BENCH_ROOT (default /srv/fdb-bench), FDB_BENCH_NETNS
(default fdb-bench-client), FDB_BENCH_HOST_VETH / FDB_BENCH_PEER_VETH (defaults
fdb-bench-host / fdb-bench-peer). CPU sets, Go version and GOMAXPROCS come from
protocol.json's runtime. --expected-smoke-manifests defaults to phase A's
arms x workloads.
"""
import argparse, datetime, hashlib, json, os, pathlib, subprocess, tarfile
parser=argparse.ArgumentParser(description=__doc__)
parser.add_argument('--engine',choices=('ssd','memory'),default='ssd')
parser.add_argument('--tag',default=None)
parser.add_argument('--expected-smoke-manifests',type=int,default=None)
args=parser.parse_args()
tag=args.tag or args.engine
if not tag.replace('-','').isalnum(): raise ValueError('unsafe receipt tag')
R=pathlib.Path(os.environ.get('FDB_BENCH_ROOT','/srv/fdb-bench'))
NETNS=os.environ.get('FDB_BENCH_NETNS','fdb-bench-client')
HOST_DEV=os.environ.get('FDB_BENCH_HOST_VETH','fdb-bench-host')
PEER_DEV=os.environ.get('FDB_BENCH_PEER_VETH','fdb-bench-peer')
P=R/'provenance'
def sha(p):
    h=hashlib.sha256()
    with open(p,'rb') as f:
        for b in iter(lambda:f.read(1048576),b''): h.update(b)
    return h.hexdigest()
def cmd(*args):
    return subprocess.check_output(args,text=True,stderr=subprocess.STDOUT)
def save(name,value):
    (P/name).write_text(json.dumps(value,indent=2,sort_keys=True)+'\n')
protocol=json.loads((R/'protocol.json').read_text())
runtime=protocol['runtime']
phase_a=next(p for p in protocol['phases'] if p['id']=='A')
expected_smoke=args.expected_smoke_manifests if args.expected_smoke_manifests is not None else len(phase_a['arms'])*len(phase_a['workloads'])
artifacts=sorted((R/'sources').glob('*.tar.gz'))+sorted((R/'overlays/final').glob('*.tar.gz'))
source=dict(harness_commit=protocol['harness_revision'], production_revisions=protocol['production_revisions'],source_artifacts_sha256={str(p):sha(p) for p in artifacts})
save('source-receipt.json',source)
# Prove actual extracted source inputs match original archives plus approved overlays.
verification={}
for tree in ('legacy','before','pr2','final'):
    expected={}
    archives=[R/'sources'/f'{tree}.tar.gz',R/'overlays/final/common.tar.gz']
    if tree in ('pr2','final'): archives.append(R/'overlays/final/remote.tar.gz')
    if tree=='final': archives.append(R/'overlays/final/final-only.tar.gz')
    for archive in archives:
        with tarfile.open(archive,'r:gz') as tf:
            for member in tf:
                if member.isfile(): expected[member.name]=hashlib.sha256(tf.extractfile(member).read()).hexdigest()
    mismatches=[]
    for name,digest in expected.items():
        p=R/'trees'/tree/name
        if not p.is_file() or sha(p)!=digest: mismatches.append(name)
    verification[tree]=dict(files=len(expected),archives=[str(p) for p in archives],mismatches=mismatches)
    if mismatches: raise RuntimeError(f'actual source tree differs: {tree}: {mismatches}')
with tarfile.open(R/'overlays/final/client-harness.tar.gz','r:gz') as tf:
    members=[member for member in tf if member.isfile()]
    mismatches=[member.name for member in members if not (R/member.name).is_file() or sha(R/member.name)!=hashlib.sha256(tf.extractfile(member).read()).hexdigest()]
    verification['client-harness']=dict(files=len(members),archives=[str(R/'overlays/final/client-harness.tar.gz')],mismatches=mismatches)
    if mismatches: raise RuntimeError('actual external client harness differs: '+str(mismatches))
save('actual-source-verification.json',verification)
pid=int((R/'cluster/server.pid').read_text())
status=json.loads(cmd('taskset','--cpu-list',runtime['housekeeping_cpus'],'/usr/bin/fdbcli','-C',str(R/'cluster/fdb.cluster'),'--exec','status json'))
if not status['client']['database_status']['healthy']: raise RuntimeError('cluster not healthy')
save('cluster-'+tag+'-sealed.json',status)
engine=(R/'cluster/engine.txt').read_text().strip()
if engine!=args.engine: raise RuntimeError('unexpected engine for phase')
info={
 'recorded_utc':datetime.datetime.now(datetime.timezone.utc).isoformat(),'actual_engine':engine,
 'hostname':cmd('hostname').strip(),'uname':cmd('uname','-a').strip(),
 'lscpu':json.loads(cmd('lscpu','-J')),
 'thread_siblings':{str(i):pathlib.Path(f'/sys/devices/system/cpu/cpu{i}/topology/thread_siblings_list').read_text().strip() for i in range(os.cpu_count())},
 'server_pid':pid,'server_affinity':cmd('taskset','-pc',str(pid)).strip(),
 'server_cmdline':pathlib.Path(f'/proc/{pid}/cmdline').read_bytes().decode().split('\0'),
 'server_status':pathlib.Path(f'/proc/{pid}/status').read_text(),
 'processes':cmd('ps','-eo','pid,ppid,psr,comm,args'),
 'network_host':json.loads(cmd('ip','-j','address','show')),
 'network_client':json.loads(cmd('ip','netns','exec',NETNS,'ip','-j','address','show')),
 'routes_client':json.loads(cmd('ip','netns','exec',NETNS,'ip','-j','route','show')),
 'qdisc_host':json.loads(cmd('tc','-s','-j','qdisc','show','dev',HOST_DEV)),
 'qdisc_client':json.loads(cmd('ip','netns','exec',NETNS,'tc','-s','-j','qdisc','show','dev',PEER_DEV)),
 'server_version':cmd('/usr/sbin/fdbserver','--version'),
 'cli_version':cmd('/usr/bin/fdbcli','--version'),
 'packages':cmd('dpkg-query','-W','foundationdb-clients','foundationdb-server'),
 'client_native_linkage':cmd('ldd',str(R/'bin/client-final-cgo')),
 'fdb_service':subprocess.run(['systemctl','is-enabled','foundationdb'],capture_output=True,text=True).stdout.strip(),
 'runtime_expected':protocol['runtime'],
 'files_sha256':{str(p):sha(p) for p in [pathlib.Path('/usr/sbin/fdbserver'),pathlib.Path('/usr/bin/fdbcli'),pathlib.Path('/usr/lib/libfdb_c.so'),pathlib.Path('/usr/local/bin/bazelisk'),R/'cluster/fdb.cluster',R/'cluster/engine.txt',R/'start_fdb_vm.sh',*sorted((R/'bin').iterdir())]},
 'diagnostic_smoke_manifests':{}
}
for manifest in sorted((R/'results/smoke-client').glob('*/manifest.json')):
    value=json.loads(manifest.read_text())
    if value['go_version']!='go'+runtime['go'] or value['gomaxprocs']!=runtime['GOMAXPROCS']: raise RuntimeError('runtime manifest mismatch')
    info['diagnostic_smoke_manifests'][str(manifest)]=value
if len(info['diagnostic_smoke_manifests'])!=expected_smoke: raise RuntimeError(f'expected all {expected_smoke} runtime diagnostic manifests')
save('runtime-'+tag+'.json',info)
print(json.dumps({'source':verification,'runtime_sha256':sha(P/('runtime-'+tag+'.json')),'source_receipt_sha256':sha(P/'source-receipt.json'),'healthy':True,'engine':engine},indent=2))
