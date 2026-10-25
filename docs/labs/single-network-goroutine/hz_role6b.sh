#!/usr/bin/env bash
# Round 6b: 5-process split cluster on netg-r6b (stateless 4751 on CPU 2,
# grv_proxy 4754 on CPU 3, commit_proxy 4755 on CPU 4, transaction 4752 on CPU 5,
# storage 4753 on CPUs 6-7). Per-role server CPU + wire frames/packet per role +
# cross-box check.
set -euo pipefail
cd /root/netg/repo/docs/labs/single-network-goroutine
mkdir -p rawhz6b

for pid in $(pgrep -x fdbserver); do
  cmd=$(tr '\0' ' ' < /proc/$pid/cmdline)
  case "$cmd" in
    *"--class stateless"*)    taskset -acp 2 $pid >/dev/null ;;
    *"--class grv_proxy"*)    taskset -acp 3 $pid >/dev/null ;;
    *"--class commit_proxy"*) taskset -acp 4 $pid >/dev/null ;;
    *"--class transaction"*)  taskset -acp 5 $pid >/dev/null ;;
    *"--class storage"*)      taskset -acp 6,7 $pid >/dev/null ;;
  esac
done

export NETG_MY_CPUS=2-15 CLIENT_CPUS=8-15 FOREIGN_MAX=1000 RETRIES=0 MAX_WAIT=0 BIN=/root/netg/bin/clientbench-go CF=/root/netg/fdb/fdb.cluster PIDS=$(pgrep -x fdbserver | paste -sd,)
rm -f rawhz6b/role_ladder.jsonl rawhz6b/role_rate.jsonl rawhz6b/role_ladder.log rawhz6b/role_rate.log rawhz6b/wire.log

CELLS="get100:64:80000:240000 get100:256:250000:750000 set100:64:25000:75000"
PER_ROLE=1 ARMS_FILE=$PWD/hz6b_role_arms.json OUT=$PWD/rawhz6b/role_ladder.jsonl ROUNDS=3 CELLS="$CELLS" ./ladder.sh > rawhz6b/role_ladder.log 2>&1

for spec in get100:64:42000:126000:28000 set100:64:18000:54000:11000 set100:64:25000:75000:17000; do
  IFS=: read -r wl w short long rate <<<"$spec"
  af="rawhz6b/role_rate_arms_${wl}_${rate}.json"
  python3 -c "
import json; a=json.load(open('hz6b_role_arms.json'))
for v in a.values(): v['args']=v.get('args',[])+['-rate=$rate']
json.dump(a,open('$af','w'),indent=1)"
  echo "== role rate-cap $wl/$w rate=$rate $(date -Is)" >> rawhz6b/role_rate.log
  PER_ROLE=1 ARMS_FILE="$PWD/$af" OUT=$PWD/rawhz6b/role_rate.jsonl ROUNDS=3 CELLS="$wl:$w:$short:$long" ./ladder.sh >> rawhz6b/role_rate.log 2>&1
done

ALL_ARMS=L,A,A0,M,DNG,DNGy,DNGa20,DNGa100,D5,D,D50
for spec in get100:64:100000 get100:256:300000 set100:64:40000; do
  IFS=: read -r wl w it <<<"$spec"
  PORTS=4751,4752,4753,4754,4755 CLIENT_CPUS=8-15 WL=$wl WORKERS=$w CAP_ITERS=$it SKIP_METRICS=1 \
    OUT=$PWD/rawhz6b/wire-$wl-$w LOGS=/root/netg/fdb WIRETRACE=/root/netg/bin/wiretrace WIRECAPTURE_CPUS=0-1 LOAD_MAX=1000 \
    ARMS_FILE=$PWD/hz6b_role_arms.json ARMS=$ALL_ARMS ROUNDS=1 ./wire_campaign.sh >> rawhz6b/wire.log 2>&1
done

for rate in 11000 17000; do
  PORTS=4751,4752,4753,4754,4755 CLIENT_CPUS=8-15 WL=set100 WORKERS=64 CAP_ITERS=40000 SKIP_METRICS=1 \
    OUT=$PWD/rawhz6b/wire-set100-64-r$rate LOGS=/root/netg/fdb WIRETRACE=/root/netg/bin/wiretrace WIRECAPTURE_CPUS=0-1 LOAD_MAX=1000 \
    ARMS_FILE=$PWD/rawhz6b/role_rate_arms_set100_${rate}.json ARMS=$ALL_ARMS ROUNDS=1 ./wire_campaign.sh >> rawhz6b/wire.log 2>&1
done

PORTS=4751,4752,4753,4754,4755 CLIENT_CPUS=8-15 WL=get100 WORKERS=64 CAP_ITERS=80000 SKIP_METRICS=1 \
  OUT=$PWD/rawhz6b/wire-get100-64-r28000 LOGS=/root/netg/fdb WIRETRACE=/root/netg/bin/wiretrace WIRECAPTURE_CPUS=0-1 LOAD_MAX=1000 \
  ARMS_FILE=$PWD/rawhz6b/role_rate_arms_get100_28000.json ARMS=$ALL_ARMS ROUNDS=1 ./wire_campaign.sh >> rawhz6b/wire.log 2>&1

echo role6b-done
