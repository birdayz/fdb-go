"""Frozen three-arm tail timing and separate profile populations (RFC-263 Rev5)."""
import argparse, datetime, hashlib, importlib.util, json, pathlib, shutil, subprocess
parser=argparse.ArgumentParser()
parser.add_argument('--evidence',type=pathlib.Path,required=True)
parser.add_argument('--source-evidence',type=pathlib.Path,default=pathlib.Path('/tmp/rfc263-rev5.QgGUbU'))
parser.add_argument('--mode',choices=['timing','diagnostic'],required=True)
args=parser.parse_args(); P=args.evidence; R=args.source_evidence
P.mkdir(parents=True,exist_ok=True); (P/'bin').mkdir(exist_ok=True)
spec=importlib.util.spec_from_file_location('primary',pathlib.Path(__file__).with_name('rev5-primary.py'))
c=importlib.util.module_from_spec(spec); spec.loader.exec_module(c)
c.E=P; c.RUNS=P/'runs'; c.RUNS.mkdir(exist_ok=True)
c.S={'baseline':'fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc','rev4':'tree:61d2597e0231dfbedcd898a27c078c3412efaf6a','rev5':'tree:3f800737de95d937cd1f12babc60b5588814d781'}
expected={'baseline':'2dd943fdc9d58ff136449a181189ba5843465e86e6ae696d616a9b1bac4a439e','rev4':'2f0993bd332a9d7085b1c1053659b68867286de95ad1cbe2bf91fe327f088e95','rev5':'d2b9f82132acd6d4c10d3a694003c0c00094181b90f648d70f38f5b7f18d965e'}
for arm,digest in expected.items():
    assert c.sha(R/'bin'/arm)==digest
    if not (P/'bin'/arm).exists(): shutil.copyfile(R/'bin'/arm,P/'bin'/arm)
    assert c.sha(P/'bin'/arm)==digest
    (P/'bin'/arm).chmod(0o755)
inputs={str(p):hashlib.sha256(p.read_bytes()).hexdigest() for p in [pathlib.Path(__file__),pathlib.Path(__file__).with_name('rev5-primary.py')]}
(P/(args.mode+'-inputs.json')).write_text(json.dumps({'scripts':inputs,'binaries':expected,'sources':c.S,'harness_id':c.H},indent=2)+'\n')
start=datetime.datetime.now(datetime.timezone.utc)
cutoff=start.replace(hour=17,minute=10,second=0,microsecond=0)
assert start<cutoff, 'Too late for owner window'
(P/(args.mode+'-start-utc.txt')).write_text(start.isoformat()+'\n')
orders=[['baseline','rev4','rev5'],['rev5','rev4','baseline'],['rev4','baseline','rev5'],['rev5','baseline','rev4'],['baseline','rev5','rev4'],['rev4','rev5','baseline']]
rows=[]
def run(phase,block,arm,workers,iterations,profile):
    assert datetime.datetime.now(datetime.timezone.utc)<cutoff, 'Owner window cutoff reached'
    name=f'{phase}-range100-p{block}-{arm}'
    with (c.RUNS/(name+'.uptime-before.txt')).open('w') as f: subprocess.run(['uptime'],stdout=f,check=True)
    try: rows.append(c.one(phase,'range100',block,arm,workers=workers,iterations=iterations,profile=profile))
    finally:
        with (c.RUNS/(name+'.uptime-after.txt')).open('w') as f: subprocess.run(['uptime'],stdout=f,check=True)
if args.mode=='timing':
    for workers,iterations in [(32,500000),(8,20000)]:
        for block,order in enumerate(orders):
            for arm in order: run(f'tail-timing-w{workers}',block,arm,workers,iterations,'none')
else:
    for i,profile in enumerate(['block','mutex','trace']):
        for arm in orders[i]: run('tail-diag-'+profile,0,arm,32,500000,profile)
for p,digest in inputs.items(): assert hashlib.sha256(pathlib.Path(p).read_bytes()).hexdigest()==digest
for arm,digest in expected.items(): assert c.sha(P/'bin'/arm)==digest
(P/(args.mode+'-rows.json')).write_text(json.dumps(rows,indent=2)+'\n')
(P/(args.mode+'-finish-utc.txt')).write_text(datetime.datetime.now(datetime.timezone.utc).isoformat()+'\n')
print('CAMPAIGN_COMPLETE',args.mode,len(rows),flush=True)
