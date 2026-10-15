#!/usr/bin/env python3
"""Owned-veth netem controls. Run only between timing phases; preserve receipts.

Required: FDB_BENCH_HOST_VETH_ADDR (host veth CIDR, e.g. 192.0.2.1/30; its
network must be a namespace route), FDB_BENCH_SERVER_ADDR (host:port; the host
is pinged) and FDB_BENCH_HOUSEKEEPING_CPUS (taskset list for ping). Defaults:
FDB_BENCH_NETNS=fdb-bench-client, FDB_BENCH_HOST_VETH=fdb-bench-host,
FDB_BENCH_PEER_VETH=fdb-bench-peer.
"""
import argparse,ipaddress,json,math,os,pathlib,re,statistics,subprocess
def required_env(name):
    value=os.environ.get(name,'')
    if not value: raise SystemExit(name+' is required')
    return value
R=pathlib.Path(os.environ.get('FDB_BENCH_ROOT','/srv/fdb-bench'))
NETNS=os.environ.get('FDB_BENCH_NETNS','fdb-bench-client')
HOST_DEV=os.environ.get('FDB_BENCH_HOST_VETH','fdb-bench-host')
PEER_DEV=os.environ.get('FDB_BENCH_PEER_VETH','fdb-bench-peer')
NS=['ip','netns','exec',NETNS]
# Expected netem configuration. Verification reads `tc -j` JSON, never text:
# tc prints the kernel's tick-quantized delay (e.g. 0.000249984 s as "249us"),
# so the text form does not round-trip the configured 250us.
NETEM_LIMIT=100000
NETEM_DELAY_S=250e-6
NETEM_DELAY_TOLERANCE_S=1e-6
# Every other netem option tc can emit (loss-random, loss-gilbert, loss-state,
# duplicate, reorder, corrupt, rate, slot, ...) is refused by its presence.
NETEM_ALLOWED_OPTIONS={'limit','delay','seed','ecn','gap'}
def _number(value):
    return isinstance(value,(int,float)) and not isinstance(value,bool) and math.isfinite(value)
def netem_problems(qdiscs):
    """Problems with one device's `tc -j qdisc show` list; empty when it is
    exactly one root netem qdisc with limit 100000 and a fixed 250us delay."""
    if not isinstance(qdiscs,list) or len(qdiscs)!=1 or not isinstance(qdiscs[0],dict): return ['expected exactly one qdisc']
    q=qdiscs[0];problems=[]
    if q.get('kind')!='netem': problems.append('kind is not netem')
    if q.get('root') is not True: problems.append('netem is not the root qdisc')
    o=q.get('options')
    if not isinstance(o,dict): return problems+['missing netem options']
    extra=sorted(set(o)-NETEM_ALLOWED_OPTIONS)
    if extra: problems.append('unexpected netem options: '+', '.join(extra))
    if o.get('limit')!=NETEM_LIMIT or isinstance(o.get('limit'),bool): problems.append('limit is not %d'%NETEM_LIMIT)
    d=o.get('delay')
    if not isinstance(d,dict) or not _number(d.get('delay')): problems.append('missing netem delay')
    else:
        if abs(d['delay']-NETEM_DELAY_S)>NETEM_DELAY_TOLERANCE_S: problems.append('delay %r s is not 250us'%d['delay'])
        if set(d)-{'delay','jitter','correlation'}: problems.append('unexpected delay fields')
        for k in ('jitter','correlation'):
            if k in d and (not _number(d[k]) or d[k]!=0): problems.append(k+' is nonzero')
    if 'ecn' in o and o['ecn'] is not False: problems.append('ecn is enabled')
    if 'gap' in o and (not _number(o['gap']) or o['gap']!=0): problems.append('gap is nonzero')
    return problems
def baseline_problems(qdiscs):
    """Problems with one device's `tc -j qdisc show` list; empty when it holds
    only the default root noqueue."""
    if not isinstance(qdiscs,list) or not qdiscs: return ['no qdisc reported']
    if any(not isinstance(q,dict) or q.get('kind')!='noqueue' for q in qdiscs): return ['nondefault qdisc present']
    return []

if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('action',choices=['enable','restore']);p.add_argument('--output',required=True);a=p.parse_args()
    LINK=str(ipaddress.ip_interface(required_env('FDB_BENCH_HOST_VETH_ADDR')).network)
    SERVER_HOST=required_env('FDB_BENCH_SERVER_ADDR').rsplit(':',1)[0]
    HOUSEKEEPING=required_env('FDB_BENCH_HOUSEKEEPING_CPUS')
    O=pathlib.Path(a.output);O.mkdir(parents=True,exist_ok=False)
    def run(label,argv,check=True):
        (O/(label+'.argv.json')).write_text(json.dumps(argv)+'\n')
        x=subprocess.run(argv,text=True,capture_output=True)
        (O/(label+'.stdout')).write_text(x.stdout);(O/(label+'.stderr')).write_text(x.stderr)
        (O/(label+'.returncode')).write_text(str(x.returncode)+'\n')
        if check and x.returncode: raise RuntimeError(label+' failed; see retained stdout/stderr')
        return x

    def qdiscs(label):
        data={}
        for name,pre,dev in [('host',[],HOST_DEV),('client',NS,PEER_DEV)]:
            text=run(label+'-'+name+'-text',pre+['tc','-s','qdisc','show','dev',dev]).stdout
            value=json.loads(run(label+'-'+name+'-json',pre+['tc','-s','-j','qdisc','show','dev',dev]).stdout)
            data[name]={'text':text,'json':value}
        return data

    def drop_count(data):
        counts=[]
        for side in data.values():
            values=re.findall(r'dropped (\d+)',side['text'])
            if not values: raise RuntimeError('missing qdisc drop counters')
            counts.extend(map(int,values))
        return sum(counts)

    def delete(label):
        return [run(label+'-'+side,pre+['tc','qdisc','del','dev',dev,'root'],check=False).returncode for side,pre,dev in [('host',[],HOST_DEV),('client',NS,PEER_DEV)]]

    # Refuse any use outside this owned namespace's isolated private link.
    routes=json.loads(run('routes',NS+['ip','-j','route','show']).stdout)
    if not routes or any(x.get('dst')=='default' for x in routes) or not any(x.get('dst')==LINK for x in routes): raise RuntimeError('unexpected client namespace routes')
    before=qdiscs('before');enabled=False;approved_restore=False
    try:
        if a.action=='enable':
            if any(q['kind']!='noqueue' for side in before.values() for q in side['json']): raise RuntimeError('baseline already has nondefault shaping')
            run('enable-host',['tc','qdisc','replace','dev',HOST_DEV,'root','netem','limit','100000','delay','250us']);enabled=True
            run('enable-client',NS+['tc','qdisc','replace','dev',PEER_DEV,'root','netem','limit','100000','delay','250us'])
        else:
            if any(q['kind'] not in ('netem','noqueue') for side in before.values() for q in side['json']): raise RuntimeError('unexpected root qdisc; refusing delete')
            approved_restore=True
            if drop_count(before): raise RuntimeError('netem drop count nonzero; restore in exception path, retain adverse receipt')
            if any(delete('restore')): raise RuntimeError('one root qdisc delete failed')
        after=qdiscs('after')
        if a.action=='enable':
            for side in after.values():
                if netem_problems(side['json']): raise RuntimeError('actual netem differs from fixed250us/limit100000: '+'; '.join(netem_problems(side['json'])))
        elif any(baseline_problems(side['json']) for side in after.values()): raise RuntimeError('baseline qdisc restoration failed')
        if drop_count(after): raise RuntimeError('qdisc drops during verification')
        ping=run('ping',NS+['taskset','--cpu-list',HOUSEKEEPING,'ping','-n','-D','-c','100','-i','0.02','-W','2',SERVER_HOST]).stdout
        rtts=[float(v) for v in re.findall(r'time=([0-9.]+) ms',ping)]
        if len(rtts)!=100 or not re.search(r'100 packets transmitted, 100 received, 0% packet loss',ping): raise RuntimeError('ping incomplete/loss; not a verified network condition')
        configured=after
        after=qdiscs('post-ping')
        if drop_count(after): raise RuntimeError('qdisc drops during ping; condition is not verified')
        if a.action=='enable' and any(netem_problems(side['json']) for side in after.values()): raise RuntimeError('netem disappeared or changed during ping')
        if a.action=='restore' and any(baseline_problems(side['json']) for side in after.values()): raise RuntimeError('baseline qdisc changed during restoration ping')
        ordered=sorted(rtts);receipt=dict(verified=True,action=a.action,one_way_delay_us=250 if a.action=='enable' else 0,nominal_added_rtt_us=500 if a.action=='enable' else 0,ping_packets=100,qdisc_drops=0,loss_percent=0,jitter_us=0,reorder_percent=0,rtt_p50_ms=statistics.median(rtts),rtt_p99_ms=ordered[math.ceil(.99*len(ordered))-1],rtt_min_ms=min(rtts),rtt_max_ms=max(rtts),rtts_ms=rtts,synthetic_private_veth_only=True,before=before,configured_before_ping=configured,after=after)
        (O/'receipt.json').write_text(json.dumps(receipt,indent=2)+'\n');print(json.dumps({k:v for k,v in receipt.items() if k not in ('before','after','rtts_ms')}))
    except BaseException as e:
        (O/'ADVERSE.json').write_text(json.dumps({'error':repr(e),'synthetic_private_veth_only':True})+'\n')
        if enabled or approved_restore:
            codes=delete('failure-restore');restored=qdiscs('failure-after-restore')
            noqueue=all(q['kind']=='noqueue' for side in restored.values() for q in side['json'])
            (O/'failure-restoration.json').write_text(json.dumps({'delete_returncodes':codes,'qdiscs_noqueue':noqueue,'rtt_restoration_verified':False,'not_a_success_receipt':True})+'\n')
        raise
