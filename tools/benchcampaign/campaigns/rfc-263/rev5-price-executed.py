import datetime, importlib.util, json, pathlib, shutil, subprocess, time
P=pathlib.Path('/tmp/rfc263-rev5.QgGUbU')
R=pathlib.Path('/tmp/rfc263-rev4-screen.2KEIQu')
spec=importlib.util.spec_from_file_location('primary','/tmp/rfc263-phase1.owjNRR/primary.py')
c=importlib.util.module_from_spec(spec); spec.loader.exec_module(c)
c.E=P; c.RUNS=P/'timed'; c.RUNS.mkdir(exist_ok=True)
c.S={'baseline':'fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc','rev4':'tree:61d2597e0231dfbedcd898a27c078c3412efaf6a','rev5':'tree:'+(P/'rev5-tree.txt').read_text().strip()}
for arm in ['baseline','rev4']:
    shutil.copyfile(R/'bin'/('reclaim' if arm=='rev4' else 'baseline'),P/'bin'/arm)
expected={'baseline':'2dd943fdc9d58ff136449a181189ba5843465e86e6ae696d616a9b1bac4a439e','rev4':'2f0993bd332a9d7085b1c1053659b68867286de95ad1cbe2bf91fe327f088e95','rev5':(P/'rev5-binary.sha256').read_text().split()[0]}
for arm, digest in expected.items():
    assert c.sha(P/'bin'/arm)==digest
    (P/'bin'/arm).chmod(0o755)
inputs=P/'rev5-inputs.sha256'
subprocess.run(['sha256sum','-c',str(inputs)],cwd=P/'scratch',check=True)
rows=[]
start=datetime.datetime.now(datetime.timezone.utc)
cutoff=start.replace(hour=16,minute=55,second=0,microsecond=0)
assert start<cutoff, 'Too late for quiet window'
(P/'pricing-start-utc.txt').write_text(start.isoformat()+'\n')
for phase, first, workloads, workers, iterations in [('renewal-price-w8','rev4',['get100','set100','range100'],8,20000),('renewal-original-w8','baseline',['get100','set100','range100'],8,20000),('renewal-original-range32','baseline',['range100'],32,500000)]:
    for workload in workloads:
        for pair in range(3):
            for arm in ([first,'rev5'] if pair%2==0 else ['rev5',first]):
                assert datetime.datetime.now(datetime.timezone.utc)<cutoff, 'Quiet window cutoff reached'
                run=f'{phase}-{workload}-p{pair}-{arm}'
                with (c.RUNS/(run+'.uptime-before.txt')).open('w') as f:
                    subprocess.run(['uptime'],stdout=f,check=True)
                try:
                    rows.append(c.one(phase,workload,pair,arm,workers=workers,iterations=iterations))
                finally:
                    with (c.RUNS/(run+'.uptime-after.txt')).open('w') as f:
                        subprocess.run(['uptime'],stdout=f,check=True)
for arm, digest in expected.items(): assert c.sha(P/'bin'/arm)==digest
subprocess.run(['sha256sum','-c',str(inputs)],cwd=P/'scratch',check=True)
(P/'pricing-rows.json').write_text(json.dumps(rows,indent=2)+'\n')
(P/'pricing-finish-utc.txt').write_text(datetime.datetime.now(datetime.timezone.utc).isoformat()+'\n')
print('PRICING_COMPLETE',len(rows),flush=True)
