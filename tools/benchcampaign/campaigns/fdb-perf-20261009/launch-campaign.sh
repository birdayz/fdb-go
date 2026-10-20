#!/usr/bin/env bash
set -Eeuo pipefail
D=${FDB_PERF_HOME}/results/fdb-perf-20261009
id=${1:?campaign identifier}
round=${2:?round identifier}
p="$D/work-$id"
config=${FDB_PERF_STATE}/fdb-perf-20261009/ssh_config
(cd "$p";sha256sum "prepare-$round.py" "validate-$round.py" "run-$round.sh" "report-$round.py" "$round-read1000-plan.json" "$round-batchset50-plan.json") | sed "s#  #  /opt/fdbperf/artifacts/$id-build/#" > "$p/measurement-input.sha256"
printf '4be70960bb0122c14e3a8f9b7a94a9a3547e446647c932f5b6e6f1b1f237775e  /opt/fdbperf/scripts/astra-perf-run.py\nd157a95c9d49540b02d8c88b127f0ba5bb54487717f29c3cad2e37f83a24fc85  /opt/fdbperf/scripts/astra-perf-analyze.py\n' >> "$p/measurement-input.sha256"
scp -F "$config" "$p/measurement-input.sha256" "fdb-perf:/opt/fdbperf/artifacts/$id-build/"
ssh -F "$config" -o ConnectTimeout=10 -o ServerAliveInterval=10 -o ServerAliveCountMax=2 fdb-perf bash -s "$id" "$round" <<'SH' 2>&1 | tee "$p/measurement-preflight.log"
set -eu
p=/opt/fdbperf/artifacts/$1-build
round=$2
test ! -e "$p/run-$round-status.txt"
sha256sum -c "$p/measurement-input.sha256"
date -u
taskset -pc "$(pgrep -x fdbserver)"
taskset --cpu-list 0-1 python3 -B - "$p" "$round" <<'PY'
import importlib.util,itertools,json,sys
from pathlib import Path
spec=importlib.util.spec_from_file_location('r','/opt/fdbperf/scripts/astra-perf-run.py');r=importlib.util.module_from_spec(spec);spec.loader.exec_module(r);r.check_builds()
p=Path(sys.argv[1]);round=sys.argv[2]
for w,n in [('read1000',5000),('batchset50',2500)]:
 out=p/('preview-'+w);sys.argv=['astra-perf-run.py','--plan',str(p/f'{round}-{w}-plan.json'),'--output',str(out),'--schedule-only'];r.main()
 plan=json.loads((out/'plan.json').read_text());schedule=json.loads((out/'schedule.json').read_text());assert len(schedule)==18
 assert plan['workloads']==[dict(name=w,iterations=n,warmup=200,workers=1)]
 assert {tuple(x['arm'] for x in schedule if x['block']==b) for b in range(6)}==set(itertools.permutations(['old','new','cgo']))
 prefixes=[x['command'][x['command'].index('--prefix')+1] for x in schedule];assert len(set(prefixes))==18 and {len(x.encode()) for x in prefixes}=={68}
 assert plan['campaign_wall_cap_seconds']==180
 print(w,'prespecified18runs',n,'/200,sixorders,unique68Bprefixes,180scap')
PY
SH
ssh -F "$config" -o ConnectTimeout=10 -o ServerAliveInterval=10 -o ServerAliveCountMax=2 fdb-perf "taskset --cpu-list 0-1 bash /opt/fdbperf/artifacts/$id-build/run-$round.sh" | tee "$p/run-remote.log"
