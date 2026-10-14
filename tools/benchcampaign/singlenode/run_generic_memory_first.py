#!/usr/bin/env python3
"""Controller-only sequential execution of sealed memory-first testing.B phases.
No retries. Copies raw artifacts and runs local bench_analyze.py between phases.

--root is the controller's campaign directory (plans/generic-memory-first/
manifest.json, results/, analysis/). --ssh-key, --known-hosts and --host are
required; StrictHostKeyChecking stays on. --remote-root defaults to
FDB_BENCH_ROOT, else /srv/fdb-bench. --analyzer must be byte-identical to the
pinned VM copy (default ROOT/bench_analyze.py).
"""
import argparse,datetime,hashlib,json,os,pathlib,shlex,subprocess,sys
p=argparse.ArgumentParser(description=__doc__);p.add_argument('--root',required=True);p.add_argument('--ssh-key',required=True,help='private key for the VM (never committed)');p.add_argument('--known-hosts',required=True,help='known_hosts file pinning the VM host key');p.add_argument('--host',required=True,help='SSH destination, user@host');p.add_argument('--remote-root',default=os.environ.get('FDB_BENCH_ROOT','/srv/fdb-bench'),help='FDB_BENCH_ROOT on the VM');p.add_argument('--phase-order',default='B,C1,D1,C2,D2',help='comma-separated sealed manifest phase order');p.add_argument('--analyzer',default=None);a=p.parse_args();D=pathlib.Path(a.root).resolve()
ssh=['ssh','-i',a.ssh_key,'-o','IdentitiesOnly=yes','-o','BatchMode=yes','-o','StrictHostKeyChecking=yes','-o','UserKnownHostsFile='+a.known_hosts,a.host]
RR=a.remote_root
manifest_path=D/'plans/generic-memory-first/manifest.json';manifest=json.loads(manifest_path.read_text())
expected_order=a.phase_order.split(',')
if [x['phase'] for x in manifest['phases']]!=expected_order:raise RuntimeError('manifest phase order mismatch')
out=D/'results/generic-memory-first-driver.json'
if out.exists():raise FileExistsError('generic driver already attempted')
receipt={'manifest_sha256':hashlib.sha256(manifest_path.read_bytes()).hexdigest(),'driver_sha256':hashlib.sha256(pathlib.Path(__file__).read_bytes()).hexdigest(),'started_utc':datetime.datetime.now(datetime.timezone.utc).isoformat(),'phases':[],'no_retries':True}
def save():out.write_text(json.dumps(receipt,indent=2)+'\n')
def remote(cmd,**kw):return subprocess.run([*ssh,cmd],**kw)
save()
for item in manifest['phases']:
    ph=item['phase'];entry={'phase':ph,'plan_sha256':item['sha256'],'command':item['run'],'started_utc':datetime.datetime.now(datetime.timezone.utc).isoformat()};receipt['phases'].append(entry);save()
    print('START PHASE '+ph,flush=True)
    proc=remote(shlex.join(item['run']));entry['returncode']=proc.returncode;entry['ended_utc']=datetime.datetime.now(datetime.timezone.utc).isoformat();save()
    tar_path=D/f'results/{ph}-raw.tar.gz'
    with tar_path.open('xb') as f:copy=remote('tar -C '+shlex.quote(RR)+' -czf - '+shlex.quote('results/'+ph),stdout=f)
    if copy.returncode:raise RuntimeError('raw copy failed for '+ph)
    subprocess.run(['tar','-xzf',str(tar_path),'-C',str(D)],check=True)
    entry['raw_archive']=str(tar_path);entry['raw_archive_sha256']=hashlib.sha256(tar_path.read_bytes()).hexdigest()
    raw_dir=D/'results'/ph
    if (raw_dir/'UNEXECUTED.json').exists():
        entry['status']='unexecuted-budget';entry['unexecuted']=json.loads((raw_dir/'UNEXECUTED.json').read_text());save()
        print('UNEXECUTED PHASE '+ph,flush=True)
        continue
    if (raw_dir/'ABORTED.json').exists() and proc.returncode not in (0,2):
        entry['status']='aborted';entry['aborted']=json.loads((raw_dir/'ABORTED.json').read_text());save()
        print('ABORTED PHASE '+ph,flush=True)
        sys.exit(2)
    ana_dir=D/'analysis'/ph
    ana=subprocess.run([sys.executable,'-B',a.analyzer or str(D/'bench_analyze.py'),'--input',str(raw_dir),'--output',str(ana_dir)],capture_output=True,text=True)
    entry['analyze_returncode']=ana.returncode;entry['analyze_stdout']=ana.stdout;entry['analyze_stderr']=ana.stderr
    if ana.returncode==0:
        entry['status']='complete' if proc.returncode==0 else 'incomplete-omitted-cases-retained'
    else:
        entry['status']='analyze-failed'
    save()
    print('FINISH PHASE '+ph+' status='+entry['status']+' rc='+str(proc.returncode),flush=True)
receipt['ended_utc']=datetime.datetime.now(datetime.timezone.utc).isoformat();receipt['status']='finished';save()
