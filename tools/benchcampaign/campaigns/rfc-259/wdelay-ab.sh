#!/bin/bash
# A/B: write delay via hrtimer (default) vs nanosleep (FDBGO_WDELAY=sleep) vs off; same binary.
set -e
W=${FDB_PERF_HOME}/cpu-work
cd ${FDB_GO_WORKTREES}/fdb-go-wdelay/tools/clientbench/scripts
SRV=$(pgrep -u "$(id -u)" -x fdbserver | paste -sd,)
OUT=$W/wdelay-ab.jsonl; rm -f $OUT
for b in 0 1 2; do
 for spec in 'set100 2000 64 4000 24000' 'get100 10000 64 20000 120000' 'set100 0 32 4000 40000'; do
  read -r wl r wk s l <<<"$spec"
  for arm in timerfd flusher off; do
   extra=(); envs=()
   [[ $arm = off ]] && extra+=(--client-arg=-coalesce-delay=0,0)
   [[ $r != 0 ]] && extra+=(--client-arg=-rate=$r --client-arg=-seed=$((b+1)))
   [[ $arm = flusher ]] && extra+=(--env=FDBGO_WDELAY=flusher)
   TMPDIR=$W/tmp taskset -c 12-15 python3 servercost.py --server-pid $SRV --binary $W/bin/clientbench-go-wdelay \
     --cluster-file $W/fdb3.cluster --workload $wl --workers $wk --short $s --long $l --reps 1 \
     --label $arm-$wl-$r-b$b --out $OUT --no-syscall-count "${extra[@]}" >/dev/null
  done
 done
done
python3 - <<'PY'
import json, statistics
from collections import defaultdict
g=defaultdict(list)
for l in open('${FDB_PERF_HOME}/cpu-work/wdelay-ab.jsonl'):
    r=json.loads(l); arm, wl, rate, b = r['label'].split('-'); g[(wl,rate,arm)].append(r)
for k in sorted(g):
    rs=g[k]; m=lambda f: statistics.median(r.get(f,0) or 0 for r in rs)
    print(*k, f"server {m('server_cycles'):,.0f} client {m('client_cpu_us'):.1f}us lat {m('mean_latency_us'):,.0f}us tput {m('tx_per_s'):,.0f}")
PY
