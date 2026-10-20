#!/usr/bin/env bash
# Seal R18 only after timing, tracing and analysis are all finished.
set -Eeuo pipefail
root=/opt/fdbperf/artifacts/fullstack-r18
analysis=/opt/fdbperf/artifacts/fullstack-r18-analysis
build=/opt/fdbperf/artifacts/fullstack-r18-build
stage=/opt/fdbperf/exports/fullstack-r18
archive=/opt/fdbperf/exports/fullstack-r18.tar.gz
[[ ! -e "$stage" && ! -e "$archive" ]]
python3 -B - "$root" "$analysis" "$build" "$stage" <<'PY'
import hashlib,json,os,shutil,sys
from pathlib import Path
r,a,b,s=map(Path,sys.argv[1:])
m=json.loads((r/'execution-manifest.json').read_text());c=json.loads((r/'complete.json').read_text())
assert c['completed_timing_runs']==18 and c['completed_trace_runs']==2
assert json.loads((a/'status.json').read_text())['status']=='complete'
def digest(p):
 h=hashlib.sha256()
 with p.open('rb') as f:
  for data in iter(lambda:f.read(1048576),b''):h.update(data)
 return h.hexdigest()
assert digest(r/'execution-manifest.json')==c['execution_manifest_sha256']
binaries={arm['binary'] for arm in json.loads((r/'global-plan.json').read_text())['arms']}
for task in Path('/proc').glob('[0-9]*'):
 try:exe=os.readlink(task/'exe')
 except (FileNotFoundError,PermissionError,ProcessLookupError):continue
 assert exe not in binaries,(task,exe)
s.mkdir()
for src in [r,a,b]:shutil.copytree(src,s/'artifacts'/src.name)
for path,want in m['input_sha256'].items():
 src=Path(path);assert src.is_absolute() and digest(src)==want,path
 dest=s/'inputs'/src.relative_to('/');dest.parent.mkdir(parents=True,exist_ok=True)
 shutil.copyfile(src,dest);assert digest(dest)==want,path
for path in s.rglob('*'):assert not path.is_symlink(),path
print('Copied R18 timing/tracing/analysis/build and every immutable input')
PY
(cd "$stage" && find . -type f ! -name ALL-FILES.sha256 -print0 | sort -z | xargs -0 sha256sum > ALL-FILES.sha256 && sha256sum --strict -c ALL-FILES.sha256 > /opt/fdbperf/exports/fullstack-r18-member-check.log)
tar -C /opt/fdbperf/exports -czf "$archive" fullstack-r18
sha256sum "$archive" > "$archive.sha256"
cat "$archive.sha256"
wc -l "$stage/ALL-FILES.sha256"
