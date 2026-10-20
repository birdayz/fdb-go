#!/usr/bin/env python3
"""Controller-only sequential execution of already-sealed C2 and D2 on memory-first."""
import argparse,datetime,hashlib,json,pathlib,shlex,subprocess,sys
p=argparse.ArgumentParser();p.add_argument('--root',required=True);a=p.parse_args();D=pathlib.Path(a.root).resolve()
ssh=['ssh','-i',str(pathlib.Path.home()/'.ssh/id_rsa_old'),'-o','IdentitiesOnly=yes','-o','BatchMode=yes','-o','StrictHostKeyChecking=yes','-o','UserKnownHostsFile='+str(D/'private/known_hosts'),'root@<vm-public-ip>']
manifest_path=D/'plans/generic-memory-first/manifest.json';manifest=json.loads(manifest_path.read_text())
phases=[x for x in manifest['phases'] if x['phase'] in ('C2','D2')]
if [x['phase'] for x in phases]!=['C2','D2']:raise RuntimeError('expected C2 and D2 in manifest')
out=D/'results/generic-memory-c2-d2-driver.json'
if out.exists():raise FileExistsError('c2-d2 driver already attempted')
receipt={'manifest_sha256':hashlib.sha256(manifest_path.read_bytes()).hexdigest(),'driver_sha256':hashlib.sha256(pathlib.Path(__file__).read_bytes()).hexdigest(),'started_utc':datetime.datetime.now(datetime.timezone.utc).isoformat(),'phases':[],'no_retries':True}
def save():out.write_text(json.dumps(receipt,indent=2)+'\n')
def remote(cmd,**kw):return subprocess.run([*ssh,cmd],**kw)
save()
for item in phases:
    ph=item['phase'];entry={'phase':ph,'plan_sha256':item['sha256'],'command':item['run'],'started_utc':datetime.datetime.now(datetime.timezone.utc).isoformat()};receipt['phases'].append(entry);save()
    print('START PHASE '+ph,flush=True)
    proc=remote(shlex.join(item['run']));entry['returncode']=proc.returncode;entry['ended_utc']=datetime.datetime.now(datetime.timezone.utc).isoformat();save()
    tar_path=D/f'results/{ph}-raw.tar.gz'
    with tar_path.open('xb') as f:copy=remote('tar -C /srv/fdb-bench -czf - '+shlex.quote('results/'+ph),stdout=f)
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
    entry['status']='complete' if proc.returncode==0 else 'incomplete-omitted-cases-retained'
    save()
    print('FINISH PHASE '+ph+' status='+entry['status']+' rc='+str(proc.returncode),flush=True)
receipt['ended_utc']=datetime.datetime.now(datetime.timezone.utc).isoformat();receipt['status']='finished';save()
