#!/usr/bin/env bash
set -Eeuo pipefail
p=/opt/fdbperf/artifacts/pr9-r15-build
scripts=/opt/fdbperf/sources/lifecycle-v7-baseline/tools/clientbench/scripts
exec > >(tee "$p/run-r15.log") 2>&1
trap 'rc=$?;printf "exit_code=%s\nfinished_utc=%s\n" "$rc" "$(date -u +%FT%TZ)" > "$p/run-r15-status.txt"' EXIT
sha256sum -c "$p/measurement-input.sha256"
sha256sum -c "$p/binaries.sha256"
sha256sum -c "$p/runtime.sha256"
for work in read1000 batchset50;do
 python3 -B "$scripts/run.py" --plan "$p/r15-$work-plan.json" --output "$p/preview-$work" --schedule-only
done
python3 -B - "$p" <<'PY'
import itertools,json,sys
from pathlib import Path
p=Path(sys.argv[1]);prefixes=[]
for work,n in [('read1000',5000),('batchset50',2500)]:
 d=p/('preview-'+work);plan=json.loads((d/'plan.json').read_text());s=json.loads((d/'schedule.json').read_text())
 assert len(s)==18 and plan['blocks']==6 and plan['campaign_wall_cap_seconds']==180
 assert plan['workloads']==[dict(name=work,iterations=n,warmup=200,workers=1)]
 assert {tuple(r['arm'] for r in s if r['block']==b) for b in range(6)}==set(itertools.permutations(['old','new','cgo']))
 prefixes.extend(r['command'][r['command'].index('--prefix')+1] for r in s)
assert len(prefixes)==len(set(prefixes))==36 and {len(x.encode()) for x in prefixes}=={68}
print('R15:36fresh68-byteprefixes,twoseparate18run/sixordercampaigns,180scaps.')
PY
for work in read1000 batchset50;do
 out=/opt/fdbperf/artifacts/pr9-r15-$work
 for arm in old new;do
  (cd "/opt/fdbperf/sources/pr9-r15-$arm";find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$p/before-$work-$arm.sha256"
  cmp "$p/full-$arm.sha256" "$p/before-$work-$arm.sha256"
 done
 printf 'BEGIN_R15_%s %s\n' "$work" "$(date -u +%FT%TZ)"
 timeout --signal=TERM --kill-after=10s 180s taskset -c 0-1 python3 -B "$scripts/run.py" --plan "$p/r15-$work-plan.json" --output "$out"
 printf 'END_R15_%s_TIMING %s\n' "$work" "$(date -u +%FT%TZ)"
 sha256sum -c "$p/measurement-input.sha256" > "$p/after-$work-input-check.log"
 for arm in old new;do
  (cd "/opt/fdbperf/sources/pr9-r15-$arm";find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$p/after-$work-$arm.sha256"
  cmp "$p/full-$arm.sha256" "$p/after-$work-$arm.sha256"
 done
 taskset -c 0-1 python3 -B "$scripts/analyze.py" "$out" --denominator cgo --resamples 10000 --seed 795707 > "$out/analysis.stdout"
 taskset -c 0-1 python3 -B "$p/report-r15.py" "$out" > "$out/report-r15.stdout"
 printf 'R15_%s_VALIDATED %s\n' "$work" "$(date -u +%FT%TZ)"
done
printf 'ALL_R15_VALIDATED %s\n' "$(date -u +%FT%TZ)"
