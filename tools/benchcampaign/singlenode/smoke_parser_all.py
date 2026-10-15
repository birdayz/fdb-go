#!/usr/bin/env python3
"""Pre-timing real-log compatibility diagnostics. NEVER performance samples.

Runs on the VM. FDB_BENCH_ROOT (default /srv/fdb-bench) holds bench_runner.py,
protocol.json and bin/; FDB_BENCH_NETNS defaults to fdb-bench-client. Client
CPUs and GOMAXPROCS come from protocol.json's runtime.
"""
import importlib.util,json,os,pathlib,re,subprocess
R=pathlib.Path(os.environ.get('FDB_BENCH_ROOT','/srv/fdb-bench')); NETNS=os.environ.get('FDB_BENCH_NETNS','fdb-bench-client'); S=R/'results/smoke-parser-all';S.mkdir(exist_ok=False)
spec=importlib.util.spec_from_file_location('runner',R/'bench_runner.py');m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
protocol=json.loads((R/'protocol.json').read_text()); runtime=protocol['runtime']; env={'PATH':'/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin','HOME':'/root','GOMAXPROCS':str(runtime['GOMAXPROCS']),'GOGC':'100','GOMEMLIMIT':'off','GODEBUG':'','LC_ALL':'C','TZ':'UTC','FDB_BENCH_CLUSTER_FILE':str(R/'cluster/fdb.cluster')}
results=[]
for phase in ('B','C1','D1','C2','D2'):
    arms=next(p['arms'] for p in protocol['phases'] if p['id']==phase)
    for ci,c in enumerate(m.cases_for(phase)):
        for arm in arms:
            name=m.benchmark_name(c,arm);regex='/'.join('^'+re.escape(p)+'$' for p in name.split('/'))
            cmd=['ip','netns','exec',NETNS,'taskset','-c',runtime['client_cpus'],m.binary_for(c,arm),'-test.run=^$','-test.bench='+regex,'-test.count=3','-test.benchmem','-test.v','-test.benchtime='+('1000x' if c['write'] else '1x')]
            if c['binary_kind']=='rl':cmd.append('-ginkgo.skip=.*')
            label=f'{phase}-{ci:02d}-{arm}'; job={'case':c,'arm':arm,'name':name}
            (S/(label+'.command.json')).write_text(json.dumps(cmd)+'\n')
            with (S/(label+'.log')).open('x') as f:
                p=subprocess.run(cmd,env=env,stdout=f,stderr=subprocess.STDOUT,timeout=120)
            if p.returncode:raise RuntimeError(label+' nonzero '+str(p.returncode))
            text=(S/(label+'.log')).read_text();rows,audits=m.parse_output(text,job,gomaxprocs=runtime['GOMAXPROCS'])
            # Revert-prove the missing parent fixture logs are rejected.
            omitted_rejected=None
            if c['audit'] in ('fetch','planskip','sql-fetch'):
                bad='\n'.join(x for x in text.splitlines() if '"phase":"fixture"' not in x)
                try:m.parse_output(bad,job,gomaxprocs=runtime['GOMAXPROCS'])
                except m.CorrectnessFailure:omitted_rejected=True
                else:raise RuntimeError('fixture omission accepted '+label)
            results.append(dict(label=label,parsed_rows=len(rows),audits=len(audits),fixture_omission_rejected=omitted_rejected,diagnostic_only=True))
            (S/'progress.json').write_text(json.dumps(results,indent=2)+'\n')
            print('DIAGNOSTIC PARSER PASS '+label,flush=True)
(S/'complete.json').write_text(json.dumps({'diagnostic_only':True,'processes':len(results),'rows':sum(x['parsed_rows'] for x in results)},indent=2)+'\n')
