#!/usr/bin/env python3
"""Freeze R18's eighteen timings and two traces before any R18 observations."""
import copy
import hashlib
import importlib.util
import json
from pathlib import Path

ROOT = Path('/opt/fdbperf/artifacts/fullstack-r18')
BUILD = Path('/opt/fdbperf/artifacts/fullstack-r18-build')
R17 = Path('/opt/fdbperf/artifacts/fullstack-r17-timing')
SCRIPTS = Path('/opt/fdbperf/sources/r17-frozen-harness/tools/clientbench/scripts')
TRACE_CALLS = ['read','write','readv','writev','pread64','pwrite64','sendmsg','recvfrom','sendto','recvmsg','recvmmsg','sendmmsg','epoll_pwait','epoll_wait','epoll_ctl','futex','nanosleep','clock_nanosleep','sched_yield','getrusage']

def sha(path):
    h=hashlib.sha256()
    with Path(path).open('rb') as f:
        for data in iter(lambda:f.read(1024*1024),b''): h.update(data)
    return h.hexdigest()

def read(path): return json.loads(path.read_text())
def dump(path,obj):
    with path.open('x') as f:f.write(json.dumps(obj,indent=2,sort_keys=True)+'\n')
def setflag(command,flag,value): command[command.index(flag)+1]=str(value)
def load(path):
    spec=importlib.util.spec_from_file_location('r18_frozen_runner',path)
    module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module);return module

assert sha(SCRIPTS/'run.py')=='7ce9e96c4e736b2df559f62b00c4eacfd4ad4b1623ca380d94e07896d96669f2'
runner=load(SCRIPTS/'run.py')
assert read(R17/'complete.json')['completed_runs']==640
assert read(Path('/opt/fdbperf/artifacts/fullstack-r17-analysis/status.json'))['status']=='complete'
assert ROOT.is_dir() and not (ROOT/'execution-manifest.json').exists()
plan=read(R17/'plans/seg000-plan.json')
for path,digest in plan['expected_sha256'].items():assert sha(path)==digest,path
new,cgo=plan['arms'];go8,go2=copy.deepcopy(new),copy.deepcopy(new)
go8['name']='go8';go2['name']='go2'
plan['arms']=[go8,go2,cgo]
for arm in plan['arms']:
    setflag(arm['command'],'--prefix','clientbench/'+'b718'+'0'*25+'/{workload:_<16}/b{block:03d}/'+arm['name']+'/')
    arm['gomaxprocs']=2 if arm['name']=='go2' else 8
plan.update(campaign='fullstack-r18',blocks=6,seed=798001,global_block_offset=0,
    workloads=[dict(name='read1000',iterations=5000,warmup=200,workers=1)],
    purpose='R18 exploratory GOMAXPROCS diagnosis; no confirmatory inference')
added=[ROOT/'R18-preregistration-v1.md',BUILD/'prepare-r18.py',BUILD/'acquire-r18.py',
    BUILD/'analyze-r18.py',R17/'execution-manifest.json',R17/'complete.json',
    Path('/usr/bin/strace'),Path('/usr/bin/taskset'),Path('/usr/bin/time')]
# Any analyzer helper must be staged before preparation and is also immutable.
added += sorted((BUILD/'analysis-support').rglob('*.py'))
plan['pinned_files']=sorted(set(plan['pinned_files'])|{str(path) for path in added})
plan['expected_sha256']={path:sha(path) for path in sorted(set(plan['pinned_files'])|{a['binary'] for a in plan['arms']})}
plan['gomaxprocs_by_arm']={a['name']:a['gomaxprocs'] for a in plan['arms']}
schedule=runner.make_schedule(plan,ROOT/'UNRESOLVED')
assert len(schedule)==18
(ROOT/'plans').mkdir()
entries=[]
for index,entry in enumerate(schedule):
    name=f'run{index:03d}'
    setflag(entry['command'],'--output',ROOT/name/entry['run_id'])
    selected=copy.deepcopy(plan)
    selected['environment']['GOMAXPROCS']=str(plan['gomaxprocs_by_arm'][entry['arm']])
    selected['selected_run_index']=index
    p=ROOT/'plans'/(name+'-plan.json');s=ROOT/'plans'/(name+'-schedule.json')
    dump(p,selected);dump(s,[entry])
    entries.append(dict(index=index,name=name,plan=str(p.relative_to(ROOT)),schedule=str(s.relative_to(ROOT)),plan_sha256=sha(p),schedule_sha256=sha(s)))
dump(ROOT/'global-plan.json',plan);dump(ROOT/'global-schedule.json',schedule)
traces=[]
for arm in ['go8','go2']:
    entry=copy.deepcopy(next(e for e in schedule if e['arm']==arm))
    folder=ROOT/'strace-sequence'/arm
    entry['run_id']='strace-sequence-'+arm;entry['block']=0;entry['position']=0
    entry['workload']['iterations']=200
    for flag,value in [('--iterations',200),('--warmup',200),('--output',folder),('--run-id',entry['run_id']),('--block',0),
                       ('--prefix','clientbench/'+'b7181'+'0'*24+'/read1000________/b000/'+arm+'/')]:
        setflag(entry['command'],flag,value)
    selected=copy.deepcopy(plan);selected['environment']['GOMAXPROCS']=str(plan['gomaxprocs_by_arm'][arm])
    selected['workloads']=[copy.deepcopy(entry['workload'])]
    p=ROOT/'plans'/('trace-'+arm+'-plan.json');s=ROOT/'plans'/('trace-'+arm+'-schedule.json')
    dump(p,selected);dump(s,[entry])
    invocation=['/usr/bin/time','-v','-o',str(folder/'process-time.txt'),'--','/usr/bin/strace','-ff','-ttt','-T','-v','-s','128',
                '-e','trace='+','.join(TRACE_CALLS),'-o',str(folder/'trace'),'--']+entry['command']
    traces.append(dict(name=arm,plan=str(p.relative_to(ROOT)),schedule=str(s.relative_to(ROOT)),plan_sha256=sha(p),schedule_sha256=sha(s),command=invocation))
prefixes=[e['command'][e['command'].index('--prefix')+1] for e in schedule]
prefixes += [read(ROOT/t['schedule'])[0]['command'][-1] for t in traces]
assert len(prefixes)==len(set(prefixes))==20 and {len(v.encode()) for v in prefixes}=={68}
orders=[tuple(e['arm'] for e in schedule if e['block']==b) for b in range(6)]
assert len(set(orders))==6
manifest=dict(schema='r18-diagnostics-v1',global_plan='global-plan.json',global_schedule='global-schedule.json',
    global_plan_sha256=sha(ROOT/'global-plan.json'),global_schedule_sha256=sha(ROOT/'global-schedule.json'),
    entries=entries,traces=traces,input_sha256=plan['expected_sha256'],
    scheduled_timing_runs=18,scheduled_trace_runs=2,timing_trace_cap_seconds=720,
    per_process_cap_seconds=180,preregistration_sha256=sha(ROOT/'R18-preregistration-v1.md'))
dump(ROOT/'execution-manifest.json',manifest)
print('Frozen 18 uninstrumented timings + 2 separate traces; execution_manifest_sha256='+sha(ROOT/'execution-manifest.json'))
