#!/usr/bin/env bash
set -Eeuo pipefail
id=${1:?campaign id}
round=${2:?round}
p=/opt/fdbperf/artifacts/$id-build
exec > >(tee "$p/run-$round.log") 2>&1
trap 'rc=$?;printf "exit_code=%s\nfinished_utc=%s\n" "$rc" "$(date -u +%FT%TZ)" > "$p/run-$round-status.txt"' EXIT
sha256sum -c "$p/measurement-input.sha256"
sha256sum -c "$p/binaries.sha256"
sha256sum -c "$p/runtime.sha256"
python3 -B - "$p" "$round" <<'PY'
import importlib.util,itertools,json,sys
from pathlib import Path
p=Path(sys.argv[1]);round=sys.argv[2]
spec=importlib.util.spec_from_file_location('runner','/opt/fdbperf/scripts/astra-perf-run.py');r=importlib.util.module_from_spec(spec);spec.loader.exec_module(r);r.check_builds()
prefixes=[]
for w in json.loads((p/'workloads.json').read_text()):
 out=p/('preview-'+w['name'])
 sys.argv=['astra-perf-run.py','--plan',str(p/(round+'-'+w['name']+'-plan.json')),'--output',str(out),'--schedule-only'];r.main()
 plan=json.loads((out/'plan.json').read_text());schedule=json.loads((out/'schedule.json').read_text())
 assert plan['workloads']==[w] and plan['campaign_wall_cap_seconds']==180 and len(schedule)==18
 assert {tuple(x['arm'] for x in schedule if x['block']==b) for b in range(6)}==set(itertools.permutations(['old','new','cgo']))
 prefixes.extend(x['command'][x['command'].index('--prefix')+1] for x in schedule)
 print(w['name'],w['iterations'],'/200,18runs,sixorders,180scap')
assert len(prefixes)==len(set(prefixes)) and {len(x.encode()) for x in prefixes}=={68}
PY
mapfile -t workloads < <(python3 -B -c 'import json,sys;print("\n".join(x["name"] for x in json.load(open(sys.argv[1]))))' "$p/workloads.json")
for work in "${workloads[@]}";do
 out=/opt/fdbperf/artifacts/$id-$work
 for arm in old new;do
  src=/opt/fdbperf/sources/pr6-r12-new;if [[ "$arm" == old ]];then src=/opt/fdbperf/sources/pr5-r10b-parent;fi
  (cd "$src";find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$p/precampaign-$work-$arm.sha256"
  cmp "$p/full-$arm.sha256" "$p/precampaign-$work-$arm.sha256"
 done
 printf 'BEGIN_%s_%s %s\n' "$round" "$work" "$(date -u +%FT%TZ)"
 timeout --signal=TERM --kill-after=10s 180s taskset --cpu-list 0-1 python3 -B /opt/fdbperf/scripts/astra-perf-run.py --plan "$p/$round-$work-plan.json" --output "$out"
 printf 'END_%s_%s_TIMING %s\n' "$round" "$work" "$(date -u +%FT%TZ)"
 sha256sum -c "$p/measurement-input.sha256" > "$p/after-$work-input-check.log"
 for arm in old new;do
  src=/opt/fdbperf/sources/pr6-r12-new;if [[ "$arm" == old ]];then src=/opt/fdbperf/sources/pr5-r10b-parent;fi
  (cd "$src";find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$p/postcampaign-$work-$arm.sha256"
  cmp "$p/full-$arm.sha256" "$p/postcampaign-$work-$arm.sha256"
 done
 taskset --cpu-list 0-1 python3 -B /opt/fdbperf/scripts/astra-perf-analyze.py --denominator cgo "$out" > "$out/analysis.stdout"
 taskset --cpu-list 0-1 python3 -B "$p/validate-$round.py" "$out" > "$out/validation.stdout"
 taskset --cpu-list 0-1 python3 -B "$p/report-$round.py" "$out" > "$out/$round-report.stdout"
 taskset --cpu-list 0-1 python3 -B "$p/cpu-scope.py" "$out" "$round" > "$out/cpu-scope.stdout"
 printf '%s_%s_VALIDATED %s\n' "$round" "$work" "$(date -u +%FT%TZ)"
done
printf 'ALL_%s_VALIDATED %s\n' "$round" "$(date -u +%FT%TZ)"
