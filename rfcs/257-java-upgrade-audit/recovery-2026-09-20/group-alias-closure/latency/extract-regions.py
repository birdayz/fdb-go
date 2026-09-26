import pathlib,sys,urllib.request,urllib.parse,subprocess,json,time
from html.parser import HTMLParser
R=pathlib.Path(__file__).parent
GO='/home/birdy/.cache/bazel/_bazel_birdy/cf0955a431539502aaf740b39f444075/external/rules_go++go_sdk+main___download_0/bin/go'
side=sys.argv[1]
class Page(HTMLParser):
 def __init__(self):super().__init__();self.links=[];self.text=[]
 def handle_starttag(self,tag,attrs):
  if tag=='a' and 'href' in dict(attrs):self.links.append(dict(attrs)['href'])
 def handle_data(self,data):self.text.append(data)
def get(path):return urllib.request.urlopen('http://127.0.0.1:6067'+path,timeout=180).read()
s=get('/userregions').decode();(R/(side+'-regions.html')).write_text(s);p=Page();p.feed(s)
queries=['SELECT * FROM orders WHERE id = 0','SELECT * FROM orders WHERE id = ?','SELECT id, amount FROM orders WHERE customer_id = 0','SELECT id, customer_id, amount, status FROM orders WHERE id = ?']
for i,q in enumerate(queries):
 links=[l for l in p.links if urllib.parse.parse_qs(urllib.parse.urlparse(l).query).get('type')==['stress-query: '+q] and 'latmin' not in l];assert len(links)==1
 s=get(links[0]).decode();stem=side+'-region-'+str(i);(R/(stem+'.html')).write_text(s);p2=Page();p2.feed(s)
 (R/(stem+'.txt')).write_text('\n'.join(x.strip() for x in p2.text if x.strip()))
 profiles=[l for l in p2.links if l.startswith('/region') and 'raw=1' in l];assert len(profiles)==4
 for l in profiles:
  kind=urllib.parse.urlparse(l).path[1:];out=R/(stem+'-'+kind+'.pprof');out.write_bytes(get(l))
  for mode in ['top','traces']:
   result=subprocess.run([GO,'tool','pprof','-'+mode,str(out)],capture_output=True,text=True);assert result.returncode==0,result.stderr
   (R/(stem+'-'+kind+'-'+mode+'.txt')).write_text(result.stdout+result.stderr)
 print(stem,q,flush=True)
