#!/usr/bin/env bash
set -Eeuo pipefail
cd /opt/fdbperf
p=artifacts/fullstack-r16-build
bundle=fullstack-r16
test ! -e exports/$bundle.tar.gz
grep -qx exit_code=0 "$p/build-status.txt"
grep -qx exit_code=0 "$p/run-r16-status.txt"
grep -qx exit_code=0 "$p/diagnostics-status.txt"
python3 -B - <<'PY'
import hashlib
import json
from pathlib import Path
for work in ['read1000','batchset50']:
 p=Path('artifacts/fullstack-r16-'+work)
 assert not (p/'ABORTED.json').exists()
 done=json.loads((p/'complete.json').read_text());a=json.loads((p/'analysis.json').read_text());d=json.loads((p/'r16-detailed.json').read_text())
 assert done['completed']==done['scheduled']==len(a['per_run'])==len(d['per_run'])==18 and a['valid_zero_error_campaign']
 assert all(r['errors']==r['retries']==0 for r in d['per_run'])
 assert all(r['metric']!='whole_process_cpu_through_shutdown_ns_per_op' for r in d['comparisons_ci95'])
 assert len(d['comparisons_ci95'])==29
root=Path('artifacts/fullstack-r16-diagnostics')
out=Path('artifacts/fullstack-r16-diagnostics-analysis')
done=json.loads((root/'complete.json').read_text());status=json.loads((out/'status.json').read_text());a=json.loads((out/'analysis.json').read_text());inputs=json.loads((out/'inputs.json').read_text())
assert done['completed']==6 and done['unavailable_perf_processes']==0
assert status['status']=='complete' and status['no_timing_claims'] is True
assert status['raw_files']==len(inputs['files'])==len(list(root.rglob('*')))-sum(p.is_dir() for p in root.rglob('*'))
for item in inputs['files']:
 for path in [root/item['path'],out/'raw'/item['path']]:
  assert path.stat().st_size==item['size_bytes']
  assert hashlib.sha256(path.read_bytes()).hexdigest()==item['sha256'],str(path)
assert hashlib.sha256(Path(inputs['analyzer_path']).read_bytes()).hexdigest()==inputs['analyzer_sha256']
for arm in ['new','cgo']:
 sequence=a['strace_sequence'][arm]
 assert set(sequence['markers'])==set('ABCDE')
 assert all(marker['exact_signature_candidate_count']==1 for marker in sequence['markers'].values())
 assert sequence['samples']['rows']==200
 assert sequence['measured_window']['aggregate']['getrusage']['calls']==0
 assert a['perf_stat'][arm]['status']=='parsed'
 assert len(a['perf_stat'][arm]['counters'])==8
 assert all(counter['status']=='counted' for counter in a['perf_stat'][arm]['counters'].values())
print('36 timings/6 instruments complete; 29 timing comparisons per workload; all raw and copied instrument hashes verified')
PY
sha256sum -c "$p/measurement-input.sha256" > "$p/archive-input-check.log"
sha256sum -c "$p/diagnostics-input.sha256" > "$p/archive-diagnostics-input-check.log"
sha256sum -c "$p/binaries.sha256" > "$p/archive-binary-check.log"
sha256sum -c "$p/runtime.sha256" > "$p/archive-runtime-check.log"
mkdir "$p/reused-v7-build"
for name in parent-full.tar.gz parent-git-tree.txt parent-git-identity.txt harness-v7-candidate.tar.gz harness-v7.sha256 standalone-wrapper-v7.tar.gz harness-and-wrapper.sha256 frozen-v6.go.mod frozen-v6.go.sum fdb-go-range-iterator-done.patch review-candidate-sha.txt review-acks.json code-review-ack.json spec-review-ack.json build-v7.sh build.log build-status.txt standalone-cgo.log cgo.go-version-m.txt cgo.readelf-d.txt native-ldd.txt go-version.txt go-env.txt binaries.sha256 runtime.sha256;do
 cp "artifacts/lifecycle-v7-build/$name" "$p/reused-v7-build/$name"
done
ps -eo pid,ppid,comm,args > "$p/archive-processes.txt"
paths=("$p" artifacts/fullstack-r16-read1000 artifacts/fullstack-r16-batchset50 artifacts/fullstack-r16-diagnostics artifacts/fullstack-r16-diagnostics-analysis bin/clientbench-r16-old bin/clientbench-r16-new bin/clientbench-v7-cgo)
find "${paths[@]}" -type f -print0 | sort -z | xargs -0 sha256sum > exports/$bundle.files.sha256
sha256sum -c exports/$bundle.files.sha256 > exports/$bundle.prearchive-verification.log
tar -czf exports/$bundle.tar.gz "${paths[@]}" exports/$bundle.files.sha256 exports/$bundle.prearchive-verification.log
sha256sum exports/$bundle.tar.gz > exports/$bundle.tar.gz.sha256
cat exports/$bundle.tar.gz.sha256
wc -l exports/$bundle.files.sha256
