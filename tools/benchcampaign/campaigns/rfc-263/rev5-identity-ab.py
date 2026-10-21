"""Causal detector-only A/B; requires a frozen, tested identity prototype binary."""
import argparse, datetime, hashlib, importlib.util, json, pathlib, subprocess
parser=argparse.ArgumentParser(); parser.add_argument('evidence',type=pathlib.Path)
args=parser.parse_args(); P=args.evidence
spec=importlib.util.spec_from_file_location('primary',pathlib.Path(__file__).with_name('rev5-primary.py'))
c=importlib.util.module_from_spec(spec); spec.loader.exec_module(c)
c.E=P; c.RUNS=P/'runs'
c.S={'baseline':'fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc','rev5':'tree:3f800737de95d937cd1f12babc60b5588814d781','identity':'tree:'+(P/'identity-tree.txt').read_text().strip()}
expected={'baseline':'2dd943fdc9d58ff136449a181189ba5843465e86e6ae696d616a9b1bac4a439e','rev5':'d2b9f82132acd6d4c10d3a694003c0c00094181b90f648d70f38f5b7f18d965e','identity':(P/'identity-binary.sha256').read_text().split()[0]}
inputs={str(p):hashlib.sha256(p.read_bytes()).hexdigest() for p in [pathlib.Path(__file__),pathlib.Path(__file__).with_name('rev5-primary.py')]}
for arm,digest in expected.items(): assert c.sha(P/'bin'/arm)==digest
(P/'identity-ab-inputs.json').write_text(json.dumps({'scripts':inputs,'binaries':expected,'sources':c.S,'harness_id':c.H},indent=2)+'\n')
start=datetime.datetime.now(datetime.timezone.utc); cutoff=start.replace(hour=17,minute=10,second=0,microsecond=0)
assert start<cutoff
(P/'identity-ab-start-utc.txt').write_text(start.isoformat()+'\n')
rows=[]
orders=[['rev5','identity','baseline'],['baseline','identity','rev5'],['identity','rev5','baseline'],['baseline','rev5','identity'],['rev5','baseline','identity'],['identity','baseline','rev5']]
for workers,iterations in [(32,500000),(8,20000)]:
    for pair,order in enumerate(orders):
        for arm in order:
            assert datetime.datetime.now(datetime.timezone.utc)<cutoff
            phase=f'tail-ab-identity-w{workers}'; name=f'{phase}-range100-p{pair}-{arm}'
            with (c.RUNS/(name+'.uptime-before.txt')).open('w') as f: subprocess.run(['uptime'],stdout=f,check=True)
            try: rows.append(c.one(phase,'range100',pair,arm,workers=workers,iterations=iterations))
            finally:
                with (c.RUNS/(name+'.uptime-after.txt')).open('w') as f: subprocess.run(['uptime'],stdout=f,check=True)
for p,digest in inputs.items(): assert hashlib.sha256(pathlib.Path(p).read_bytes()).hexdigest()==digest
for arm,digest in expected.items(): assert c.sha(P/'bin'/arm)==digest
(P/'identity-ab-rows.json').write_text(json.dumps(rows,indent=2)+'\n')
(P/'identity-ab-finish-utc.txt').write_text(datetime.datetime.now(datetime.timezone.utc).isoformat()+'\n')
print('IDENTITY_AB_COMPLETE',len(rows),flush=True)
