import pathlib,json,collections,re,hashlib,urllib.parse
r=pathlib.Path('/var/tmp/fdb-upgrade-recovery/runtime')
result={}
for name,expected in [('group-alias-full',93),('group-alias-race',14)]:
 events=[json.loads(l) for l in (r/(name+'-bep.jsonl')).read_text().splitlines()]; summaries=[e for e in events if 'testSummary' in e]; assert len(summaries)==expected
 out=r/(name+'-logs'); out.mkdir(exist_ok=True)
 tests=[]; counts=collections.Counter()
 for event in summaries:
  label=event['id']['testSummary']['label']; summary=event['testSummary']; assert summary['overallStatus']=='PASSED' and summary.get('numCached',0)==0
  logs=summary['passed']; assert len(logs)==1
  path=pathlib.Path(urllib.parse.unquote(urllib.parse.urlparse(logs[0]['uri']).path)); data=path.read_bytes(); text=data.decode(); dest=out/(label.replace('//','').replace('/', '__').replace(':','__')+'.log');dest.write_bytes(data)
  names={k:collections.Counter(re.findall(pattern,text,re.M)) for k,pattern in [('run',r'^\s*=== RUN\s+(\S+)'),('pass',r'^\s*--- PASS: (\S+)'),('skip',r'^\s*--- SKIP: (\S+)'),('fail',r'^\s*--- FAIL: (\S+)')]}
  cs={k:sum(v.values()) for k,v in names.items()};counts.update(cs)
  assert cs['fail']==0 and names['run']==names['pass']+names['skip'],(label,cs,names['run']-names['pass']-names['skip'],names['pass']-names['run'])
  skipped=re.findall(r'^\s*--- SKIP:.*$',text,re.M)
  tests.append(dict(label=label,counts=cs,sha256=hashlib.sha256(data).hexdigest(),log=str(dest),skips=skipped))
  if label=='//conformance:conformance_test':
   clean=re.sub(r'\x1b\[[0-9;]*m','',text)
   print(name,label,'\n'.join(l for l in clean.splitlines() if 'Ran ' in l or 'Passed |' in l))
 result[name]=dict(targets=expected,executed=expected,counts=dict(counts),tests=tests,count_scope='Go verbose markers including indented subprocess test output; RUN and PASS/SKIP name multisets reconcile per target. Ginkgo population recorded separately.')
 print(name,dict(counts));print('skips',[(t['label'],t['skips']) for t in tests if t['skips']]); assert counts['run']>0
(r/'group-alias-suite-results.json').write_text(json.dumps(result,indent=2)+'\n')
