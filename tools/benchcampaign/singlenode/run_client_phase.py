#!/usr/bin/env python3
"""Controller-only sequential execution of already sealed client argv.
No retries. Copy each case's raw artifacts before admitting the next case.

--root is the controller's campaign directory (protocol.json, plans/client/
manifest.json, results/). --ssh-key, --known-hosts and --host are required;
StrictHostKeyChecking stays on. --remote-root defaults to FDB_BENCH_ROOT, else
/srv/fdb-bench.
"""
import argparse,datetime,hashlib,json,os,pathlib,shlex,subprocess,sys,time
p=argparse.ArgumentParser(description=__doc__);p.add_argument('--root',required=True);p.add_argument('--phase',choices=['A','A2'],required=True);p.add_argument('--ssh-key',required=True,help='private key for the VM (never committed)');p.add_argument('--known-hosts',required=True,help='known_hosts file pinning the VM host key');p.add_argument('--host',required=True,help='SSH destination, user@host');p.add_argument('--remote-root',default=os.environ.get('FDB_BENCH_ROOT','/srv/fdb-bench'),help='FDB_BENCH_ROOT on the VM');a=p.parse_args();D=pathlib.Path(a.root).resolve();P=a.phase
ssh=['ssh','-i',a.ssh_key,'-o','IdentitiesOnly=yes','-o','BatchMode=yes','-o','StrictHostKeyChecking=yes','-o','UserKnownHostsFile='+a.known_hosts,a.host]
RR=a.remote_root
manifest_path=D/'plans/client/manifest.json';manifest=json.loads(manifest_path.read_text());protocol=json.loads((D/'protocol.json').read_text());phase=next(x for x in protocol['phases'] if x['id']==P);cases=[x for x in manifest['campaigns'] if x['phase']==P]
if [x['workload'] for x in cases]!=phase['workloads']:raise RuntimeError('manifest does not follow complete frozen phase workload order')
receipt={'phase':P,'manifest_sha256':hashlib.sha256(manifest_path.read_bytes()).hexdigest(),'driver_sha256':hashlib.sha256(pathlib.Path(__file__).read_bytes()).hexdigest(),'started_utc':datetime.datetime.now(datetime.timezone.utc).isoformat(),'cases':[],'no_retries':True}
out=D/f'results/client-driver-{P}.json'
if out.exists():raise FileExistsError('driver already attempted this phase')
def save():out.write_text(json.dumps(receipt,indent=2)+'\n')
def remote(command,**kwargs):return subprocess.run([*ssh,command],**kwargs)
save()
for case in cases:
    name=case['workload'];entry={'workload':name,'command':case['run'],'started_utc':datetime.datetime.now(datetime.timezone.utc).isoformat()};receipt['cases'].append(entry);save()
    print('START '+P+' '+name,flush=True)
    proc=remote(shlex.join(case['run']));entry['returncode']=proc.returncode;entry['ended_utc']=datetime.datetime.now(datetime.timezone.utc).isoformat()
    state=remote('cat '+shlex.quote(RR+'/results/client/'+P+'/client-phase-state.json'),capture_output=True,text=True)
    if state.returncode:entry['status']='missing phase state; stop';save();sys.exit(2)
    current=json.loads(state.stdout);entry['state']=current['cases'].get(name);entry['final_failure']=current.get('final_failure',False);save()
    # Compression and transfer occur only between case campaigns, never in an arm.
    cmd='tar -C '+shlex.quote(RR)+' -czf - '+shlex.quote('results/client/'+P+'/'+name)+' '+shlex.quote('results/client/'+P+'/client-phase-state.json')
    tar_path=D/f'results/{P}-{name}-raw.tar.gz'
    with tar_path.open('xb') as stream:copy=remote(cmd,stdout=stream)
    if copy.returncode:raise RuntimeError('artifact copy failed; original retained on VM')
    subprocess.run(['tar','-xzf',str(tar_path),'-C',str(D)],check=True)
    entry['raw_archive_sha256']=hashlib.sha256(tar_path.read_bytes()).hexdigest();entry['raw_archive']=str(tar_path);save()
    if entry['final_failure'] or not entry['state']:
        receipt['status']='stopped; final correctness/preflight failure';save();sys.exit(2)
    print('FINISH '+P+' '+name+' '+entry['state']['status'],flush=True)
# Copy phase completion/adverse summary, without ever discarding incomplete data.
end=remote('tar -C '+shlex.quote(RR)+' -czf - '+shlex.quote('results/client/'+P),stdout=subprocess.PIPE)
if end.returncode:raise RuntimeError('phase raw copy failed')
archive=D/f'results/{P}-phase-raw.tar.gz';archive.write_bytes(end.stdout);subprocess.run(['tar','-xzf',str(archive),'-C',str(D)],check=True)
receipt['ended_utc']=datetime.datetime.now(datetime.timezone.utc).isoformat();receipt['status']='all cases attempted';receipt['all_cases_complete']=all(x['state']['status']=='complete' for x in receipt['cases']);save()
print(P+' all cases attempted; all_complete='+str(receipt['all_cases_complete']),flush=True)
