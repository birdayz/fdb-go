#!/usr/bin/env python3
"""Controller-only execution of sealed Phase C3, followed by netem restore receipt."""
import argparse,datetime,hashlib,json,pathlib,shlex,subprocess,sys
p=argparse.ArgumentParser();p.add_argument('--root',required=True);a=p.parse_args();D=pathlib.Path(a.root).resolve()
ssh=['ssh','-i',str(pathlib.Path.home()/'.ssh/id_rsa_old'),'-o','IdentitiesOnly=yes','-o','BatchMode=yes','-o','StrictHostKeyChecking=yes','-o','UserKnownHostsFile='+str(D/'private/known_hosts'),'root@<vm-public-ip>']
manifest_path=D/'plans/generic-memory-c3/manifest.json';manifest=json.loads(manifest_path.read_text())
item=manifest['phases'][0]
if item['phase']!='C3':raise RuntimeError('expected C3 in manifest')
out=D/'results/generic-memory-c3-driver.json'
if out.exists():raise FileExistsError('c3 driver already attempted')
receipt={'manifest_sha256':hashlib.sha256(manifest_path.read_bytes()).hexdigest(),'driver_sha256':hashlib.sha256(pathlib.Path(__file__).read_bytes()).hexdigest(),'started_utc':datetime.datetime.now(datetime.timezone.utc).isoformat(),'phase':'C3','plan_sha256':item['sha256'],'command':item['run'],'no_retries':True}
def save():out.write_text(json.dumps(receipt,indent=2)+'\n')
def remote(cmd,**kw):return subprocess.run([*ssh,cmd],**kw)
save()
print('START PHASE C3',flush=True)
proc=remote(shlex.join(item['run']));receipt['returncode']=proc.returncode;receipt['ended_utc']=datetime.datetime.now(datetime.timezone.utc).isoformat();save()
tar_path=D/'results/C3-raw.tar.gz'
with tar_path.open('xb') as f:copy=remote('tar -C /srv/fdb-bench -czf - results/C3',stdout=f)
if copy.returncode:raise RuntimeError('raw copy failed for C3')
subprocess.run(['tar','-xzf',str(tar_path),'-C',str(D)],check=True)
receipt['raw_archive']=str(tar_path);receipt['raw_archive_sha256']=hashlib.sha256(tar_path.read_bytes()).hexdigest();save()
# Restore qdiscs and capture zero-delay restoration receipt after C3 completes.
res=remote('taskset --cpu-list 0-1 python3 -B /srv/fdb-bench/network-mode.py restore --output /srv/fdb-bench/provenance/netem-c3-restore > /srv/fdb-bench/provenance/netem-c3-restore.log && tar -C /srv/fdb-bench -cf - provenance/netem-c3-restore provenance/netem-c3-restore.log',stdout=subprocess.PIPE)
if res.returncode:raise RuntimeError('netem restore failed')
subprocess.run(['tar','-xf','-','-C',str(D)],input=res.stdout,check=True)
receipt['netem_restored']=True;receipt['status']='complete' if proc.returncode==0 else 'incomplete-or-aborted';save()
print('FINISH PHASE C3 status='+receipt['status']+' rc='+str(proc.returncode),flush=True)
