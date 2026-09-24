import pathlib,json,re,statistics,hashlib,subprocess,shutil,gzip
R=pathlib.Path('/var/tmp/fdb-upgrade-recovery/runtime');L=R/'latency-comparison';S=R/'group-alias-stress';D=pathlib.Path('rfcs/257-java-upgrade-audit/recovery-2026-09-20/group-alias-closure');D.mkdir(exist_ok=True)
def artifact(p):return dict(path=str(p),sha256=hashlib.sha256(p.read_bytes()).hexdigest())
def snap():
 names=subprocess.check_output(['git','ls-files','--cached','--others','--exclude-standard','-z']).decode().split('\0');return {p:hashlib.sha256(pathlib.Path(p).read_bytes()).hexdigest() for p in sorted(set(names)) if p and pathlib.Path(p).is_file()}
# This script must run once, AFTER all measurements have completed; no source
# writes are permitted while the measurement runner holds this freeze.
assert snap()==json.loads((R/'group-alias-final-code-freeze.json').read_text())
fullfreeze=json.loads((R/'group-alias-freeze.json').read_text());currentfreeze=json.loads((R/'group-alias-final-code-freeze.json').read_text());delta=sorted(p for p in fullfreeze.keys()|currentfreeze.keys() if fullfreeze.get(p)!=currentfreeze.get(p));assert delta==['pkg/relational/sqldriver/stress/BUILD.bazel','pkg/relational/sqldriver/stress/latency_attribution_stress_test.go','pkg/relational/sqldriver/stress/stress_test.go']
assert subprocess.check_output(['git','diff','--name-only','97d67fd750c13b8399bdea82774aa26bc8c8f263','be28f57df14cf7edec617bf09216ab828698cd69'],text=True).strip()=='pkg/relational/sqldriver/stress/latency_attribution_stress_test.go'
instrumented=json.loads((L/'samples.json').read_text());assert len(instrumented)==4
for rec in instrumented:
 assert rec['explain']==instrumented[0]['explain'] and len(rec['explain'])==11
 assert [(s['sql'],s['rows']) for s in rec['attribution']]==[(s['sql'],s['rows']) for s in instrumented[0]['attribution']]
suites=json.loads((R/'group-alias-suite-results.json').read_text())
for suite in suites.values():
 for t in suite['tests']:assert hashlib.sha256(pathlib.Path(t['log']).read_bytes()).hexdigest()==t['sha256']
assert suites['group-alias-full']['targets']==93 and suites['group-alias-full']['counts']=={'run':40665,'pass':40660,'skip':5,'fail':0}
assert suites['group-alias-race']['targets']==14 and suites['group-alias-race']['counts']=={'run':21118,'pass':21118,'skip':0,'fail':0}
race=json.loads((S/'stress-race-result.json').read_text());assert race['exit']==0 and race['runs']==race['passes'] and sum(race['runs'].values())==40
just=json.loads((S/'just-test-result.json').read_text());assert just['exit']==0
records=json.loads((S/'samples.json').read_text());assert len(records)==4
rows={};plans=None
for rec in records:
 text=(S/(rec['name']+'.log')).read_text();assert rec['runs']==rec['passes'] and len(rec['runs'])==24 and rec['exit']==0 and rec['compiler'].endswith('go1.26.6')
 assert 'COUNT(*) = 1000000 (expected 1000000)' in text
 got=re.findall(r'EXPLAIN: (.*)',text);assert len(got)==11
 if plans is None:plans=got
 assert got==plans
 ts=re.findall(r'stress_test.go:\d+:\s+(.+?)\s+(\d+) rows\s+([\d.]+)(µs|ms|s)\s*$',text,re.M);assert len(ts)==22
 for name,count,duration,unit in ts:
  row=rows.setdefault(name,dict(rows=int(count),baseline_ms=[],current_ms=[]));assert row['rows']==int(count);row[rec['side']+'_ms'].append(float(duration)*{'µs':.001,'ms':1,'s':1000}[unit])
for row in rows.values():
 assert len(row['baseline_ms'])==len(row['current_ms'])==2;row['median_ratio']=statistics.median(row['current_ms'])/statistics.median(row['baseline_ms'])
table=['| Query | Rows | Baseline ms [sample 1, sample 2] | Current ms [sample 1, sample 2] | Median ratio |','|---|---:|---:|---:|---:|']
for name,row in rows.items():
 f=lambda v:', '.join(f'{x:.3f}' for x in v)
 table.append(f"| {name} | {row['rows']:,} | {f(row['baseline_ms'])} | {f(row['current_ms'])} | {row['median_ratio']:.3f}x |")
(S/'table.md').write_text('\n'.join(table)+'\n')
assert len(re.findall('GROUP-ALIAS-PROBE ',(R/'group-alias-java-final.log').read_text()))==14
for name in ['latency-isolation-red.log','latency-isolation-green.log','group-alias-mutations.py','group-alias-mutations.json','group-alias-java-final.log','group-alias-suite-evidence.py','group-alias-suite-results.json','latency-validate-mutation.py','latency-validation-mutation.json','latency-validation-mutant.log','latency-validation-restored.log']:
 shutil.copyfile(R/name,D/name)
for p in R.glob('group-alias-mutant-*.log'):shutil.copyfile(p,D/p.name)
trace=D/'latency';trace.mkdir(exist_ok=True)
for name in ['run.py','extract-regions.py','summarize-regions.py','region-analysis.json','samples.json','trees.json']:
 shutil.copyfile(L/name,trace/name)
for p in L.glob('*-region-*.pprof'):shutil.copyfile(p,trace/p.name)
for p in L.glob('*-region-*.html'):shutil.copyfile(p,trace/p.name)
for p in L.glob('*-region-*-events.json'):
 (trace/(p.name+'.gz')).write_bytes(gzip.compress(p.read_bytes(),mtime=0))
shutil.copyfile(R/'group-alias-final-verification.py',D/'final-verification.py')
raw=[artifact(p) for p in sorted(L.iterdir()) if p.is_file() and p.suffix not in ['.index'] and 'freeze' not in p.name and 'paths.nul' not in p.name]
raw += [artifact(p) for p in sorted(S.iterdir()) if p.is_file()]
raw += [artifact(p) for p in sorted((R/'group-alias-stress-cancelled').iterdir()) if p.is_file()]
raw += [artifact(R/name) for name in ['group-alias-final-code-freeze.json','latency-isolation-red.log','latency-isolation-green.log','group-alias-full.log','group-alias-race.log','group-alias-full-bep.jsonl','group-alias-race-bep.jsonl','group-alias-freeze.json','latency-freeze.json','group-alias-red.log','group-alias-parser.log','group-alias-order-red.log','group-alias-java.log']]
proof=dict(scope='WS-A/E1 GROUP alias ownership repair, retained Java/FDB regressions and early-read latency attribution. Not whole-upgrade completion or publication/merge authorization.',head='71ccd8cf8b3fd0dbafe283e91171818e36af555e',rejected_tree='664b241c57b71829cb40687a1e26cc3dc3e145e6',verified_full_race_tree='ffdcc1138421d53d3b22971a095f619173d87d49',verified_final_code_tree='be28f57df14cf7edec617bf09216ab828698cd69',full_race_frozen_files=len(fullfreeze),instrumented_frozen_files=len(currentfreeze),post_suite_delta=delta,source_changes_during_runs=0,java_commit='fdacd162a9c8acfadc49082b89185c823ab8ae4a',suites=suites,ginkgo=dict(passed=1358,population=1477,existing_filtered=119),java_alias_cases=14,mutations=json.loads((R/'group-alias-mutations.json').read_text()),telemetry_validation=json.loads((R/'latency-validation-mutation.json').read_text()),stress_race=race,just_test=just,nominal_stress=dict(baseline_commit='e48f5b4965543cd4d99b5578356059e12d969c7c',current_tree='be28f57df14cf7edec617bf09216ab828698cd69',order='baseline1,current1,current2,baseline2',samples=records,rows=rows,explain=plans),latency=dict(trees=json.loads((L/'trees.json').read_text()),attribution='latency/region-analysis.json',samples='latency/samples.json',unchanged_client_subtree='448b10552b6b6259671c44cfbc29e3f0ac0c8cd6',baseline_overlay_restored=True),artifacts=raw)
(D/'verification.json').write_text(json.dumps(proof,indent=2)+'\n');print('Booked evidence',D,'nominal table',S/'table.md')
