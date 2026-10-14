#!/usr/bin/env python3
"""Seal testing.B plans only between phases, against actual memory receipts.

Runs on the VM. FDB_BENCH_ROOT (default /srv/fdb-bench) holds protocol.json,
orders.json, these scripts, bin/, cluster/ and provenance/. FDB_BENCH_NETNS
(default fdb-bench-client) names the client namespace in the run commands.
--lifetime-deadline is the VM's hard ISO8601 UTC deletion deadline.
FDB_BENCH_SERVER_ADDR (required, owned server host:port) is sealed into each
run command with FDB_BENCH_ROOT and FDB_BENCH_NETNS, so an SSH driver needs no
remote environment.
"""
import argparse,hashlib,json,os,pathlib,re,subprocess,sys
R=pathlib.Path(os.environ.get('FDB_BENCH_ROOT','/srv/fdb-bench'))
NETNS=os.environ.get('FDB_BENCH_NETNS','fdb-bench-client')
SERVER=os.environ.get('FDB_BENCH_SERVER_ADDR','')
p=argparse.ArgumentParser(description=__doc__);p.add_argument('--runtime-tag',required=True);p.add_argument('--phase',action='append',required=True,choices=['B','C1','D1','C2','D2','C3']);p.add_argument('--network-receipt');p.add_argument('--output-dir',required=True);p.add_argument('--lifetime-deadline',required=True,help='hard VM lifetime deadline, ISO8601 UTC');a=p.parse_args()
if not re.fullmatch(r'[^\s@:]+:[0-9]+',SERVER):raise RuntimeError('FDB_BENCH_SERVER_ADDR=host:port of the owned server is required')
def sha(x):
    h=hashlib.sha256()
    with open(x,'rb') as f:
        for b in iter(lambda:f.read(1048576),b''):h.update(b)
    return h.hexdigest()
source_path=R/'provenance/source-receipt.json';source=json.loads(source_path.read_text());protocol=json.loads((R/'protocol.json').read_text());runtime_path=R/'provenance'/('runtime-'+a.runtime_tag+'.json');runtime=json.loads(runtime_path.read_text())
if runtime['actual_engine']!='memory' or (R/'cluster/engine.txt').read_text().strip()!='memory':raise RuntimeError('plans require actual owned memory-engine runtime')
cluster_receipt=R/'provenance'/('cluster-'+a.runtime_tag+'-sealed.json');cluster=json.loads(cluster_receipt.read_text())
if not cluster['client']['database_status']['healthy'] or not cluster['cluster']['configuration']['storage_engine'].startswith('memory'):raise RuntimeError('actual cluster receipt is not healthy memory engine')
if runtime['server_affinity'].rsplit(':',1)[-1].strip()!=protocol['runtime']['server_cpus']:raise RuntimeError('actual server affinity differs from protocol')
if 'C3' not in a.phase and any(q['kind']!='noqueue' for key in ('qdisc_host','qdisc_client') for q in runtime[key]):raise RuntimeError('unexpected network shaping before baseline phase')
if source['harness_commit']!=protocol['harness_revision'] or source['production_revisions']!=protocol['production_revisions']:raise RuntimeError('actual source receipt differs from protocol')
if any(v['mismatches'] for v in json.loads((R/'provenance/actual-source-verification.json').read_text()).values()):raise RuntimeError('source mismatch')
if a.network_receipt and a.phase!=['C3']:raise RuntimeError('network receipt only in separate C3 seal')
if 'C3' in a.phase and not a.network_receipt:raise RuntimeError('C3 needs actual pinned netem/ping receipt')
paths=[R/'protocol.json',R/'orders.json',source_path,runtime_path,cluster_receipt,*map(pathlib.Path,runtime['files_sha256']),R/'provenance/actual-source-verification.json',R/'provenance/final-harness-review.json',R/'provenance/runner-raw-compatibility.json',R/'provenance/go-buildinfo.txt',R/'provenance/bazel-version.txt',R/'provenance/server-memory-command.txt',R/'bench_runner.py',R/'bench_analyze.py',R/'tools/clientbench/scripts/run.py',R/'start_fdb_vm.sh',R/'cluster/fdb.cluster',R/'cluster/engine.txt',pathlib.Path('/usr/sbin/fdbserver'),pathlib.Path('/usr/bin/fdbcli'),pathlib.Path('/usr/lib/libfdb_c.so'),pathlib.Path(__file__).resolve(),*sorted((R/'bin').iterdir()),*map(pathlib.Path,source['source_artifacts_sha256'])]
if a.network_receipt:paths.extend([pathlib.Path(a.network_receipt),R/'network_mode.py'])
pins={str(x):sha(x) for x in sorted(set(paths))}
for x,digest in runtime['files_sha256'].items():
    if pins.get(x)!=digest:raise RuntimeError('current binary/runtime/config hash differs from actual runtime receipt '+x)
for x,digest in source['source_artifacts_sha256'].items():
    if pins[x]!=digest:raise RuntimeError('source artifact hash mismatch '+x)
O=pathlib.Path(a.output_dir);O.mkdir(parents=True,exist_ok=False);(O/'pins.json').write_text(json.dumps(pins,indent=2,sort_keys=True)+'\n');manifest={'harness_commit':source['harness_commit'],'runtime_tag':a.runtime_tag,'runtime_sha256':sha(runtime_path),'pins_sha256':sha(O/'pins.json'),'generator_sha256':sha(__file__),'phases':[]}
for phase in a.phase:
    plan=O/(phase+'.json');cmd=[sys.executable,'-B',str(R/'bench_runner.py'),'plan','--phase',phase,'--protocol',str(R/'protocol.json'),'--orders',str(R/'orders.json'),'--pins',str(O/'pins.json'),'--source-receipt',str(source_path),'--output',str(plan)]
    if a.network_receipt:cmd+=['--network-receipt',a.network_receipt]
    subprocess.run(cmd,check=True)
    value=json.loads(plan.read_text());manifest['phases'].append({'phase':phase,'plan':str(plan),'sha256':sha(plan),'jobs':len(value['jobs']),'samples':len(value['jobs'])*3,'run':['env','FDB_BENCH_ROOT='+str(R),'FDB_BENCH_NETNS='+NETNS,'FDB_BENCH_SERVER_ADDR='+SERVER,'ip','netns','exec',NETNS,'taskset','--cpu-list',protocol['runtime']['housekeeping_cpus'],sys.executable,'-B',str(R/'bench_runner.py'),'run','--plan',str(plan),'--output',str(R/'results'/phase),'--lifetime-deadline',a.lifetime_deadline]})
(O/'manifest.json').write_text(json.dumps(manifest,indent=2,sort_keys=True)+'\n');print(json.dumps(manifest,indent=2))
