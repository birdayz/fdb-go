#!/usr/bin/env python3
"""Read-only final forensic snapshots of the four recorded campaign hosts.

No execution unless --execute. Uses remote.run's recorded IDs/pinned host keys.
Streams bounded relative-path tar snapshots, verifies all members/hashes without
extracting, and never includes cluster data/private SSH/cloud metadata paths.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor, as_completed
import hashlib
import io
import json
import os
from pathlib import Path, PurePosixPath
import re
import shlex
import stat
import sys
import tarfile
import time

from remote import C, R, run

ROLES = ('client', 'node1', 'node2', 'node3')
RUNTIME = {'/usr/sbin/fdbserver': '179df8ec7292ad060fb7bd60a771ff4a8dd3989a8115638356c73feea4fc2c5f',
           '/usr/bin/fdbcli': '6dfe3328389fe8ff8175ddc080e278c5b07d1d2601f97079cba1194bfb2e41fb',
           '/usr/lib/libfdb_c.so': 'e4ad7fed7b7580bf5f7b38f0a4279d37e698f69d534182e5de0b8beeef6cd1b1'}
TREES = ('provenance', 'plans', 'seals', 'diagnostic', 'results', 'reconcile', 'telemetry', 'tooling',
         'frozen', 'sources', 'bin', 'execution', 'cluster-ssd', 'work')
FILES = ('protocol.json', 'orders.json', 'cluster/fdb.cluster', 'cloud-init-done')
DENIED = {'.ssh', '.aws', '.git', '__pycache__', 'private', 'credentials', 'user-data', 'vendor-data', 'meta-data', '.env'}

# This program performs only file reads and explicitly allowlisted read-only
# host/systemd commands. It creates no remote staging directory or output files.
REMOTE_PROGRAM = r'''
import gzip,hashlib,io,json,os,pathlib,re,stat,subprocess,sys,tarfile,time
s=json.loads(sys.argv[1]); root=pathlib.Path(s['root']); role=s['role']; end=time.monotonic()+s['remote_seconds']
records={}; errors=[]; omitted=[]; hash_only={}; cache={}; commands=[]; file_count=0; trace_count=0
DENIED=set(s['denied'])
secret=re.compile(br'(?m)^-----BEGIN (?:OPENSSH |RSA |EC |DSA |ENCRYPTED )?PRIVATE KEY-----\r?$')
def remaining():
 v=end-time.monotonic()
 if v<=0: raise TimeoutError('remote collection budget expired')
 return v
def forbidden(p):
 return any(x in DENIED or x.startswith(('id_rsa','id_ed25519','ssh_host_')) for x in p.parts) or p.suffix in ('.key','.pem','.p12','.pfx','.pyc')
def checked_path(p):
 if not p.is_absolute() or forbidden(p): raise ValueError('forbidden path '+str(p))
 for q in (p,*p.parents):
  if q.is_symlink(): raise ValueError('symlink path '+str(q))
 return p
def digest_file(p, explicit_runtime=False):
 remaining()
 target=p.resolve() if explicit_runtime else checked_path(p)
 if explicit_runtime and not any(str(target).startswith(v) for v in ('/usr/bin/','/usr/sbin/','/usr/lib/')): raise ValueError('runtime target outside package paths')
 with target.open('rb') as f:
  h=hashlib.sha256()
  while True:
   remaining(); data=f.read(1048576)
   if not data:break
   h.update(data)
 cache[str(p)]=h.hexdigest()
 return h.hexdigest()
def put(name,data,source=None,extra=None):
 remaining()
 if name in records: raise ValueError('duplicate archive member '+name)
 info=tarfile.TarInfo(name);info.size=len(data);info.mode=0o600;info.mtime=int(time.time());info.uid=info.gid=0
 tar.addfile(info,io.BytesIO(data))
 records[name]=dict(size=len(data),sha256=hashlib.sha256(data).hexdigest(),source=source,**(extra or {}))
def json_put(name,value):put(name,(json.dumps(value,sort_keys=True,indent=2)+'\n').encode())
def command(label,argv,optional=False):
 remaining(); record={'label':label,'argv':argv,'started_epoch':time.time()}
 try:
  p=subprocess.run(argv,stdout=subprocess.PIPE,stderr=subprocess.PIPE,timeout=min(25,max(1,remaining())))
  out,err=p.stdout,p.stderr
  if secret.search(out) or secret.search(err):
   errors.append(label+': private key block refused in command output');out=err=b'[REDACTED private key output]'
  record.update(returncode=p.returncode,stdout=out.decode(errors='replace'),stderr=err.decode(errors='replace'))
  if p.returncode and not optional:errors.append(label+': command exit '+str(p.returncode))
 except Exception as exc:record['error']=repr(exc);errors.append(label+': '+repr(exc))
 commands.append(record);json_put('commands/'+label+'.json',record)
 return record.get('stdout','')
def trace(p):return p.is_relative_to(root/'cluster/logs') or p.is_relative_to(pathlib.Path('/var/log/foundationdb')) or p.is_relative_to(root/'traces')
def add_file(p):
 global file_count,trace_count
 remaining(); checked_path(p)
 if str(p) in seen:return
 seen.add(str(p))
 try:
  fd=os.open(p,os.O_RDONLY|os.O_NOFOLLOW)
  with os.fdopen(fd,'rb') as f:
   before=os.fstat(f.fileno())
   if not stat.S_ISREG(before.st_mode):raise ValueError('non-regular file')
   is_payload=p.is_relative_to(root/'bin') or p.suffixes[-2:]==['.tar','.gz'] or p.suffix in ('.deb','.tar','.tgz')
   if is_payload:
    digest=digest_file(p)
    retained=s['local_available'].get(str(p),{})
    if digest in retained:
     hash_only[str(p)]={'sha256':digest,'size':before.st_size,'retained_local':retained[digest]};return
   if before.st_size>536870912:raise ValueError('single file exceeds512MiB memory bound; explicit omission')
   data=f.read(before.st_size)
   if len(data)!=before.st_size:raise ValueError('file shrank during snapshot')
   if secret.search(data):raise ValueError('private key block refused before archiving')
   after=os.fstat(f.fileno());stable=(before.st_size,before.st_mtime_ns)==(after.st_size,after.st_mtime_ns)
   live=trace(p)
   if not stable and not live:errors.append(str(p)+': non-trace artifact changed during snapshot')
   put('files/'+str(p).lstrip('/'),data,str(p),{'initial_size':before.st_size,'final_size':after.st_size,
       'mtime_ns':before.st_mtime_ns,'stable':stable,'live_trace_prefix':live})
   cache[str(p)]=hashlib.sha256(data).hexdigest();file_count+=1;trace_count+=int(live)
 except Exception as exc:errors.append(str(p)+': '+repr(exc))
def tree(p,required=False):
 if not p.exists():
  if required:errors.append('required path missing: '+str(p))
  else:omitted.append({'path':str(p),'reason':'not present'})
  return
 checked_path(p)
 if p.is_file():add_file(p);return
 for base,dirs,names in os.walk(p,followlinks=False):
  remaining();base=pathlib.Path(base)
  for d in list(dirs):
   item=base/d
   if forbidden(item):dirs.remove(d);omitted.append({'path':str(item),'reason':'sensitive/cache directory excluded'})
   elif item.is_symlink():dirs.remove(d);errors.append('symlink directory refused: '+str(item))
  for name in sorted(names):
   item=base/name
   if forbidden(item):omitted.append({'path':str(item),'reason':'sensitive/cache file excluded'})
   else:add_file(item)
def allowed_pin(p):
 return p.is_relative_to(root) and p.parts[len(root.parts):len(root.parts)+2]!=('cluster','data') and not forbidden(p) or str(p) in s['runtime'] or re.fullmatch(r'/usr/bin/python[0-9.]+',str(p))
seen=set()
with gzip.GzipFile(fileobj=sys.stdout.buffer,mode='wb',compresslevel=1) as compressed, tarfile.open(fileobj=compressed,mode='w|') as tar:
 try:
  for name,digest in s['runtime'].items():
   try:
    actual=digest_file(pathlib.Path(name),True);hash_only[name]={'sha256':actual,'expected_sha256':digest,'runtime':True}
    if actual!=digest:errors.append('runtime hash mismatch: '+name)
   except Exception as exc:errors.append(name+': '+repr(exc))
  for name in s['trees']:tree(root/name,required=name in ('provenance','cluster-ssd'))
  for name in s['files']:tree(root/name,required=name in ('protocol.json','orders.json') and role=='client')
  tree(root/'cluster/logs',required=role!='client');tree(root/'traces');tree(pathlib.Path('/var/log/foundationdb'))
  for unit in s['fdb_units']:tree(pathlib.Path('/etc/systemd/system')/unit,required=role!='client')
  tree(pathlib.Path('/etc/foundationdb/foundationdb.conf'))
  listing=command('unit-discovery',['systemctl','list-units','--all','--plain','--no-legend','--no-pager'],optional=True)
  historical=command('journal-unit-discovery',['journalctl','--no-pager','--since=@'+str(s['since_epoch']),'--field=_SYSTEMD_UNIT'])
  units=set(s['fdb_units'])|{'foundationdb.service'}
  for line in (listing+'\n'+historical).splitlines():
   parts=line.split(); name=parts[0] if parts else ''
   if re.fullmatch(r'fdb-mn-(?:runner|sampler)-[A-Za-z0-9_-]+\.service',name):units.add(name)
  json_put('meta/selected-units.json',sorted(units))
  for i,unit in enumerate(sorted(units)):
   command('unit-'+str(i)+'-show',['systemctl','show','--no-pager','--property=Id,LoadState,ActiveState,SubState,MainPID,ExecMainStatus,ExecMainCode,Result,ControlGroup,ExecStart,CPUAffinity,Restart',unit],optional=True)
   command('unit-'+str(i)+'-cat',['systemctl','cat','--no-pager',unit],optional=True)
   command('unit-'+str(i)+'-journal',['journalctl','--no-pager','--since=@'+str(s['since_epoch']),'--unit='+unit,'--output=short-iso-precise'])
  for label,argv in (
   ('uname',['uname','-a']),('cpu',['lscpu','-J']),('addresses',['ip','-j','address']),('routes',['ip','-j','route']),
   ('chrony',['chronyc','tracking']),('disk',['lsblk','-J','-o','NAME,SIZE,TYPE,MODEL,ROTA,MOUNTPOINT']),
   ('processes',['ps','-eo','pid,ppid,comm']),('fdbserver-version',['/usr/sbin/fdbserver','--version']),('fdbcli-version',['/usr/bin/fdbcli','--version'])):command(label,argv)
  final_expected={};pin_sources=[]
  if role=='client':
   for mf in sorted((root/'plans').glob('*/manifest.json')):
    obj=json.loads(mf.read_text());pins=obj.get('pinned_files',{})
    if not pins:errors.append('empty final manifest pins: '+str(mf))
    for name,digest in pins.items():
     if name in final_expected and final_expected[name]!=digest:errors.append('conflicting final manifest pin: '+name)
     final_expected[name]=digest
    pin_sources.append(str(mf))
   source=root/'provenance/source-receipt.json'
   if source.is_file():
    for name,digest in json.loads(source.read_text()).get('source_artifacts_sha256',{}).items():
     if name in final_expected and final_expected[name]!=digest:errors.append('source/final pin conflict: '+name)
     final_expected[name]=digest
   else:errors.append('client source receipt missing')
  pin_report={}
  for name,expected in sorted(final_expected.items()):
   p=pathlib.Path(name)
   if not allowed_pin(p):errors.append('unsafe final pin path refused: '+name);continue
   try:
    actual=digest_file(p,str(p) in s['runtime'] or str(p).startswith('/usr/bin/python'))
    pin_report[name]={'expected':expected,'actual':actual}
    if actual!=expected:errors.append('final/source pinned input mismatch: '+name)
   except Exception as exc:errors.append('final pin '+name+': '+repr(exc))
  if role!='client' and trace_count==0:errors.append('no FDB trace files captured')
  if file_count==0:errors.append('no artifact file population captured')
  json_put('meta/pin-verification.json',{'sources':pin_sources,'pins':pin_report})
 except Exception as exc:errors.append('collector: '+repr(exc))
 manifest={'format':'fdb-final-collection/v1','role':role,'root':str(root),'collected_epoch':time.time(),
   'files':records,'hash_only':hash_only,'errors':errors,'omitted':omitted,'artifact_files':file_count,'trace_files':trace_count,
   'remote_program_sha256':s['remote_program_sha256'],'no_cluster_mutations':True,'snapshot_live_traces':True}
 raw=(json.dumps(manifest,sort_keys=True,indent=2)+'\n').encode();info=tarfile.TarInfo('meta/manifest.json');info.size=len(raw);info.mode=0o600
 tar.addfile(info,io.BytesIO(raw))
sys.exit(2 if errors else 0)
'''


class Refused(RuntimeError):
    pass


def need(condition, message):
    if not condition:
        raise Refused(message)


def sha_file(path, deadline=None):
    if deadline is not None:
        need(time.monotonic() < deadline, 'collection hashing budget expired')
    h = hashlib.sha256()
    with Path(path).open('rb') as f:
        while data := f.read(1048576):
            if deadline is not None:
                need(time.monotonic() < deadline, 'collection verification budget expired')
            h.update(data)
    return h.hexdigest()


def private_new(path):
    p = Path(path).absolute()
    for parent in (p, *p.parents):
        need(not parent.is_symlink() and not (parent / '.git').exists(), 'symlink/Git output path refused')
    p.mkdir(mode=0o700, parents=True, exist_ok=False)
    return p


def dump(path, obj):
    with Path(path).open('x') as f:
        json.dump(obj, f, indent=2, sort_keys=True)
        f.write('\n')


def member_allowed(name, units):
    need(isinstance(name, str) and name and '\\' not in name and '\x00' not in name, 'unsafe archive name')
    p = PurePosixPath(name)
    need(not p.is_absolute() and all(x not in ('', '.', '..') for x in name.split('/')) and len(name) <= 4096,
         'absolute/traversal archive member refused: ' + name)
    need(not any(x in DENIED or x.startswith(('id_rsa', 'id_ed25519', 'ssh_host_')) for x in p.parts), 'sensitive archive member refused')
    need(p.suffix not in ('.key', '.pem', '.p12', '.pfx', '.pyc'), 'sensitive/archive key suffix refused')
    if name.startswith(('commands/', 'meta/')):
        need(len(p.parts) == 2, 'nested command/meta archive member refused')
        return
    need(name.startswith('files/'), 'unknown archive namespace')
    source = '/' + '/'.join(p.parts[1:])
    allowed = any(source == R + '/' + x or source.startswith(R + '/' + x + '/') for x in TREES)
    allowed |= source in {R + '/' + x for x in FILES}
    allowed |= any(source == x or source.startswith(x + '/') for x in (R + '/cluster/logs', R + '/traces', '/var/log/foundationdb'))
    allowed |= source in {'/etc/systemd/system/' + u for u in units} | {'/etc/foundationdb/foundationdb.conf'}
    need(allowed and not source.startswith(R + '/cluster/data/'), 'out-of-scope/data archive member: ' + source)


def validate_archive(path, role, units, local_available, deadline=None):
    actual, seen, manifest = {}, set(), None
    with tarfile.open(path, mode='r|gz', ignore_zeros=True) as archive:
        for member in archive:
            if deadline is not None:
                need(time.monotonic() < deadline, 'archive verification exceeded total budget')
            member_allowed(member.name, units)
            need(member.isfile() and not member.issparse() and 0 <= member.size <= 536870912,
                 'non-regular/link/device/sparse/oversize archive member')
            need(member.name not in seen and manifest is None, 'duplicate member or data after final manifest')
            seen.add(member.name)
            stream = archive.extractfile(member)
            need(stream is not None, 'unreadable archive member')
            h, count, chunks = hashlib.sha256(), 0, []
            while data := stream.read(1048576):
                if deadline is not None:
                    need(time.monotonic() < deadline, 'archive verification exceeded total budget')
                count += len(data)
                h.update(data)
                if member.name == 'meta/manifest.json':
                    need(count <= 67108864, 'oversize collection manifest')
                    chunks.append(data)
            need(count == member.size, 'truncated archive member')
            if member.name == 'meta/manifest.json':
                manifest = json.loads(b''.join(chunks))
            else:
                actual[member.name] = dict(size=count, sha256=h.hexdigest())
    need(isinstance(manifest, dict) and manifest.get('format') == 'fdb-final-collection/v1' and
         manifest.get('role') == role and manifest.get('root') == R and manifest.get('files'), 'missing/wrong/empty archive manifest')
    need(manifest.get('remote_program_sha256') == hashlib.sha256(REMOTE_PROGRAM.encode()).hexdigest(), 'wrong remote collector identity')
    expected = manifest['files']
    artifact_count = sum(name.startswith('files/') for name in actual)
    trace_count = sum(name.startswith(('files' + R + '/cluster/logs/', 'files' + R + '/traces/', 'files/var/log/foundationdb/')) for name in actual)
    need(artifact_count > 0 and manifest.get('artifact_files') == artifact_count and manifest.get('trace_files') == trace_count,
         'artifact/trace population count disagreement')
    need(role == 'client' or trace_count > 0, 'node archive has no trace population')
    need(set(actual) == set(expected), 'archive/manifest member populations differ')
    need({source for source, record in manifest.get('hash_only', {}).items() if record.get('runtime')} == set(RUNTIME),
         'runtime hash population is incomplete')
    for name, value in actual.items():
        need(all(expected[name].get(k) == v for k, v in value.items()), 'artifact hash/size mismatch: ' + name)
    for source, record in manifest.get('hash_only', {}).items():
        if record.get('runtime'):
            need(source in RUNTIME and record.get('sha256') == RUNTIME[source], 'runtime hash-only mismatch')
        else:
            digest = record.get('sha256')
            need(digest in local_available.get(source, {}) and record.get('retained_local') == local_available[source][digest],
                 'excluded payload lacks exact retained local copy: ' + source)
            need(sha_file(record['retained_local'], deadline) == digest, 'retained local payload changed: ' + source)
    return manifest


def local_payloads(base, deadline):
    found = {}
    for prefix in ('bin', 'sources', 'frozen/overlays', 'frozen/cleanup-compat'):
        relatives = set()
        for start in (base / prefix, base / 'deploy' / prefix):
            need(time.monotonic() < deadline, 'local payload traversal exceeded total budget')
            if start.is_dir():
                for p in start.rglob('*'):
                    need(time.monotonic() < deadline, 'local payload traversal exceeded total budget')
                    if p.is_file() and not p.is_symlink():
                        relatives.add(p.relative_to(start))
        for rel in relatives:
            remote = R + '/' + prefix + '/' + str(rel)
            for start in (base / prefix, base / 'deploy' / prefix):
                p = start / rel
                if p.is_file() and not p.is_symlink():
                    found.setdefault(remote, {})[sha_file(p, deadline)] = str(p)
    aliases = {'client-previous-go': 'old/client-go', 'client-master-go': 'master/client-go',
               'client-master-cgo': 'master/client-cgo', 'fetch-master': 'master/fetch',
               'planskip-master': 'master/planskip', 'placementprobe-cgo': 'master/placementprobe-cgo'}
    for name, relative in aliases.items():
        need(time.monotonic() < deadline, 'local binary preflight exceeded total budget')
        path = base / 'build-check/bin' / relative
        if path.is_file() and not path.is_symlink():
            found.setdefault(R + '/bin/' + name, {})[sha_file(path, deadline)] = str(path)
    return found


def role_spec(role, layout, local_available, since_epoch, seconds):
    need(role in ROLES and all(p['node'] in ROLES[1:] and type(p['port']) is int and 0 < p['port'] < 65536
                              for p in layout['processes']), 'invalid role/port topology')
    units = [f'fdb-bench-{p["port"]}.service' for p in layout['processes'] if p['node'] == role]
    return dict(role=role, root=R, trees=list(TREES), files=list(FILES), denied=sorted(DENIED), runtime=RUNTIME,
                fdb_units=units, local_available=local_available, since_epoch=since_epoch,
                remote_seconds=seconds, remote_program_sha256=hashlib.sha256(REMOTE_PROGRAM.encode()).hexdigest())


def collect_role(role, spec, output, deadline, transport=run):
    folder = private_new(output / role)
    started = time.time()
    command = shlex.join(['timeout', '--signal=TERM', '--kill-after=5s', str(spec['remote_seconds'] + 5) + 's',
                         'taskset', '-c', '0-1', 'python3', '-B', '-c', REMOTE_PROGRAM, json.dumps(spec, sort_keys=True)])
    dump(folder / 'request.json', dict(role=role, spec=spec, command=command, started_epoch=started))
    report = dict(role=role, ok=False)
    try:
        need(time.monotonic() < deadline - 30, 'no SSH launched after collection budget exhausted')
        with (folder / 'artifacts.tar.gz').open('xb') as stdout, (folder / 'stderr').open('xb') as stderr:
            result = transport(role, command, stdout=stdout, stderr=stderr, timeout=max(1, deadline - time.monotonic() - 30))
        report['returncode'] = result.returncode
        manifest = validate_archive(folder / 'artifacts.tar.gz', role, spec['fdb_units'], spec['local_available'], deadline)
        dump(folder / 'manifest.json', manifest)
        report.update(ok=result.returncode == 0 and not manifest['errors'], errors=manifest['errors'],
            artifact_files=manifest['artifact_files'], trace_files=manifest['trace_files'],
            archive_sha256=sha_file(folder / 'artifacts.tar.gz', deadline), archive=str(folder / 'artifacts.tar.gz'))
    except Exception as exc:
        report['error'] = repr(exc)
    report['duration_seconds'] = time.time() - started
    dump(folder / 'complete.json', report)
    return report


def main(argv=None):
    os.umask(0o077)
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--execute', action='store_true')
    ap.add_argument('--output', type=Path, required=True)
    ap.add_argument('--seconds', type=int, default=600)
    ap.add_argument('--since-epoch', type=int, help='default campaign deadline minus8h')
    args = ap.parse_args(argv)
    if not args.execute:
        print('REFUSED: --execute required; no SSH performed', file=sys.stderr)
        return 2
    try:
        need(90 <= args.seconds <= 600, 'total collection budget must be90..600 seconds')
        start = time.monotonic()
        deadline = start + args.seconds
        base = Path(C)
        layout = json.loads((base / 'cluster-ssd/layout.json').read_text())
        state = json.loads((base / 'state.json').read_text())
        since = args.since_epoch if args.since_epoch is not None else state['deadline_epoch'] - 8 * 3600
        need(type(since) is int and since > 0, 'invalid campaign journal start')
        need(layout['root'] == R + '/cluster' and layout['vpc_cidr'] == '10.89.20.0/24', 'unexpected topology/trace/data layout')
        available = local_payloads(base, deadline)
        need(time.monotonic() < deadline - 60, 'local payload preflight consumed collection budget')
        output = private_new(args.output)
        dump(output / 'controller.json', dict(controller_sha256=sha_file(__file__, deadline), remote_program_sha256=hashlib.sha256(REMOTE_PROGRAM.encode()).hexdigest(),
            roles=ROLES, hosts={role: {key: state['hosts'][role][key] for key in ('id', 'name', 'public_ipv4')} for role in ROLES},
            since_epoch=since, deadline_monotonic=deadline, local_available=available,
            excluded=['cluster/data', 'private', 'SSH keys', 'cloud metadata'], no_remote_staging=True))
        remote_seconds = min(480, max(30, int(deadline - time.monotonic() - 60)))
        reports = []
        with ThreadPoolExecutor(max_workers=4) as pool:
            jobs = {pool.submit(collect_role, role, role_spec(role, layout, available, since, remote_seconds), output, deadline): role for role in ROLES}
            for job in as_completed(jobs):
                try:
                    reports.append(job.result())
                except Exception as exc:
                    reports.append(dict(role=jobs[job], ok=False, error=repr(exc)))
        need({r['role'] for r in reports} == set(ROLES), 'incomplete role collection population')
        summary = dict(ok=all(r['ok'] for r in reports), reports=sorted(reports, key=lambda x: x['role']),
            duration_seconds=time.monotonic() - start, no_cluster_mutations=True,
            archive_extraction=False, total_budget_seconds=args.seconds)
        dump(output / 'complete.json', summary)
        print(json.dumps(summary, sort_keys=True))
        return 0 if summary['ok'] else 2
    except (Refused, OSError, ValueError, KeyError) as exc:
        print('REFUSED/FAILED: ' + str(exc), file=sys.stderr)
        return 2


if __name__ == '__main__':
    sys.exit(main())
