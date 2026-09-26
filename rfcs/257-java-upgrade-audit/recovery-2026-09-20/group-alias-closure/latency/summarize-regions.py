import pathlib,json,re,subprocess,hashlib
from html.parser import HTMLParser
R=pathlib.Path(__file__).parent
GO='/home/birdy/.cache/bazel/_bazel_birdy/cf0955a431539502aaf740b39f444075/external/rules_go++go_sdk+main___download_0/bin/go'
class Tables(HTMLParser):
 def __init__(self):super().__init__();self.tables=[];self.table=None;self.row=None;self.cell=None
 def handle_starttag(self,t,a):
  if t=='table':self.table=[]
  elif t=='tr':self.row=[]
  elif t in ['td','th']:self.cell=[]
 def handle_data(self,s):
  if self.cell is not None:self.cell.append(s)
 def handle_endtag(self,t):
  if t in ['td','th']:self.row.append(' '.join(''.join(self.cell).split()));self.cell=None
  elif t=='tr':self.table.append(self.row);self.row=None
  elif t=='table':self.tables.append(self.table);self.table=None
def ns(s):
 m=re.fullmatch(r'([\d.]+)(ns|µs|ms|s)',s);assert m,s;return round(float(m[1])*{'ns':1,'µs':1000,'ms':1000000,'s':1000000000}[m[2]])
regions=[];critical=[]
for side in ['baseline-1','current-2']:
 for i in range(4):
  stem=f'{side}-region-{i}';p=Tables();p.feed((R/(stem+'.html')).read_text());table=p.tables[1];head=table[0];rows=[]
  for row in table[1:]:
   assert len(row)==len(head);item={k:(v if k in ['Goroutine','Task'] else ns(v)) for k,v in zip(head,row) if k};assert item['Total']==sum(v for k,v in item.items() if k not in ['Goroutine','Task','Total']);rows.append(item)
  assert len(rows)==(2 if i==1 else 1)
  raw=subprocess.check_output([GO,'tool','pprof','-raw',str(R/(stem+'-regionblock.pprof'))],text=True,stderr=subprocess.DEVNULL);(R/(stem+'-regionblock-raw.txt')).write_text(raw)
  locations={m[1]:m[2] for line in raw.splitlines() if (m:=re.match(r'^\s*(\d+): (.*)',line))};samples=[]
  for line in raw.splitlines():
   if m:=re.match(r'^\s*(\d+)\s+(\d+): ([\d ]+)$',line):samples.append(dict(count=int(m[1]),delay_ns=int(m[2]),stack=[locations[x] for x in m[3].split()]))
  assert samples
  grvs=[s for s in samples if any('(*grvBatcher).getReadVersion ' in x for x in s['stack'])];assert len(grvs)==3*len(rows)
  assert sum(s['delay_ns'] for s in samples)==sum(r.get('Block time (select)',0)+r.get('Block time (chan receive)',0) for r in rows)
  regions.append(dict(side=side,region=i,rows=rows,grv_wait_ns=sum(s['delay_ns'] for s in grvs),grv_samples=grvs))
  for row in rows:
   g=int(row['Goroutine']);fn=R/(stem+f'-g{g}-events.json');d=json.loads(fn.read_text());es=d['traceEvents'];frames=d['stackFrames']
   def stack(n):
    out=[]
    while n:
     frame=frames[str(n)];out.append(frame['name']);n=frame.get('parent')
    return out
   for e in es:
    if e.get('tid')!=g or not any('grvBatcher).getReadVersion' in x for x in stack(e.get('esf'))) or not any('.observedStressQuery:' in x for x in stack(e.get('esf'))):continue
    blocked=e['ts']+e['dur'];wake=next((x for x in es if x.get('tid')==g and x['ph']=='t' and x['ts']>=blocked),None)
    if not wake:continue
    source=next(x for x in es if x['ph']=='s' and x.get('id')==wake['id']);wait_ns=round((source['ts']-blocked)*1000)
    # Goroutine JSON includes EXPLAIN before the query. Only admit waits whose
    # observedStressQuery frame AND delay match this region's GRV profile.
    if wait_ns<5000000 or not any(s['delay_ns']==wait_ns for s in grvs):continue
    assert any('grvBatcher).flush' in x for x in stack(source.get('sf')))
    flusher=source['tid'];runs=[x for x in es if x.get('tid')==flusher and x['ph']=='X' and blocked<=x['ts']<=source['ts']];assert runs
    reply_waits=[x for x in runs if any('client.waitReply' in s for s in stack(x.get('esf')))];assert len(reply_waits)==1
    rpc=reply_waits[0];rpc_start=rpc['ts']+rpc['dur'];reply_wake=next(x for x in es if x.get('tid')==flusher and x['ph']=='t' and x['ts']>=rpc_start);reply_source=next(x for x in es if x['ph']=='s' and x.get('id')==reply_wake['id']);assert any('transport.(*Conn).readLoop' in s for s in stack(reply_source.get('sf')))
    reader_runs=[x for x in es if x.get('tid')==reply_source['tid'] and x['ph']=='X' and x['ts']<=reply_source['ts']<=x['ts']+x['dur']];assert len(reader_runs)==1;reader=reader_runs[0];assert any('internal/poll.(*FD).Read' in s for s in stack(reader.get('sf')))
    critical.append(dict(side=side,region=i,goroutine=g,region_total_ns=row['Total'],grv_block_ns=wait_ns,before_flusher_start_ns=round((runs[0]['ts']-blocked)*1000),rpc_reply_block_ns=round((reply_source['ts']-rpc_start)*1000),rpc_resume_delay_ns=round((reply_wake['ts']-reply_source['ts'])*1000),caller_resume_delay_ns=round((wake['ts']-source['ts'])*1000),reply_reader_stack=stack(reader.get('sf')),query_block_stack=stack(e.get('esf')),flow_events=[e,source,wake,rpc,reply_source,reply_wake,reader],json_file=fn.name,json_sha256=hashlib.sha256(fn.read_bytes()).hexdigest()))
assert len(regions)==8 and len(critical)==3
result=dict(scope='Two traces (baseline-1,current-2), four region types each, five query instances each. Profile blocking is region-filtered. Related-goroutine JSON includes EXPLAIN; critical waits require an observedStressQuery stack frame and an exact delay match against the filtered GRV profile. RPC wait is client-side time until the network reader delivers a reply; server execution versus network/kernel time cannot be separated.',trees=json.loads((R/'trees.json').read_text()),regions=regions,critical_waits=critical)
(R/'region-analysis.json').write_text(json.dumps(result,indent=2)+'\n')
for x in critical:print({k:v for k,v in x.items() if k.endswith('_ns') or k in ['side','region','goroutine']})
