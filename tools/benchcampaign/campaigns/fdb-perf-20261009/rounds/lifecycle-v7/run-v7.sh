#!/usr/bin/env bash
set -Eeuo pipefail
p=/opt/fdbperf/artifacts/lifecycle-v7-build
scripts=/opt/fdbperf/sources/lifecycle-v7-baseline/tools/clientbench/scripts
exec > >(tee "$p/run-v7.log") 2>&1
trap 'rc=$?;printf "exit_code=%s\nfinished_utc=%s\n" "$rc" "$(date -u +%FT%TZ)" > "$p/run-v7-status.txt"' EXIT
sha256sum -c "$p/measurement-input.sha256"
sha256sum -c "$p/binaries.sha256"
sha256sum -c "$p/runtime.sha256"
# Freeze and inspect all schedules before the first timed process.
for c in baseline-batchset50 baseline-read1000 stack-batchset50 stack-read1000;do
 python3 -B "$scripts/run.py" --plan "$p/v7-$c-plan.json" --output "$p/preview-$c" --schedule-only
done
python3 -B - "$p" <<'PY'
import collections,json,sys
from pathlib import Path
p=Path(sys.argv[1]);prefixes=[]
for c in ('baseline-batchset50','baseline-read1000','stack-batchset50','stack-read1000'):
 d=p/('preview-'+c);plan=json.loads((d/'plan.json').read_text());schedule=json.loads((d/'schedule.json').read_text())
 assert len(schedule)==12 and plan['blocks']==6 and plan['campaign_wall_cap_seconds']==180
 work=c.split('-',1)[1]
 assert plan['workloads']==[dict(name=work,iterations=3000 if work=='batchset50' else 5000,warmup=200,workers=1)]
 orders=collections.Counter(tuple(r['arm'] for r in schedule if r['block']==b) for b in range(6))
 assert orders=={('go','cgo'):3,('cgo','go'):3}
 prefixes.extend(r['command'][r['command'].index('--prefix')+1] for r in schedule)
assert len(prefixes)==len(set(prefixes))==48 and {len(x.encode()) for x in prefixes}=={68}
print('V7: four preregistered two-arm six-block campaigns; 48 unique68-byte prefixes;180s independent caps.')
PY
for c in baseline-batchset50 baseline-read1000 stack-batchset50 stack-read1000;do
 out=/opt/fdbperf/artifacts/v7-$c
 for arm in baseline stack;do
  (cd "/opt/fdbperf/sources/lifecycle-v7-$arm";find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$p/before-$c-$arm.sha256"
  cmp "$p/full-$arm.sha256" "$p/before-$c-$arm.sha256"
 done
 printf 'BEGIN_V7_%s %s\n' "$c" "$(date -u +%FT%TZ)"
 timeout --signal=TERM --kill-after=10s 180s taskset --cpu-list 0-1 python3 -B "$scripts/run.py" --plan "$p/v7-$c-plan.json" --output "$out"
 printf 'END_V7_%s_TIMING %s\n' "$c" "$(date -u +%FT%TZ)"
 sha256sum -c "$p/measurement-input.sha256" > "$p/after-$c-input-check.log"
 for arm in baseline stack;do
  (cd "/opt/fdbperf/sources/lifecycle-v7-$arm";find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$p/after-$c-$arm.sha256"
  cmp "$p/full-$arm.sha256" "$p/after-$c-$arm.sha256"
 done
 taskset --cpu-list 0-1 python3 -B "$scripts/analyze.py" "$out" --denominator cgo --resamples 10000 --seed 642001 > "$out/analysis.stdout"
 printf 'V7_%s_VALIDATED %s\n' "$c" "$(date -u +%FT%TZ)"
done
printf 'ALL_V7_VALIDATED %s\n' "$(date -u +%FT%TZ)"
