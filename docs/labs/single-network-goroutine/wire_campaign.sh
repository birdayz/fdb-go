#!/usr/bin/env bash
# Round 2, goal 1: per arm, a server-trace-metric window and a loopback capture
# of get100 at 64 workers. Arms rotate; each runs ROUNDS times.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../../.." && pwd)
BIN=${BIN:-/var/tmp/netg-bench/bin/clientbench-go}
CF=${CF:-/var/tmp/netg-fdb/fdb.cluster}
LOGS=${LOGS:-/var/tmp/netg-fdb}
OUT=${OUT:-$HERE/raw2/wire}
ops=
ROUNDS=${ROUNDS:-2}
PORTS=${PORTS:-4751,4752,4753}
export WIRETRACE=${WIRETRACE:-/var/tmp/netg-bench/bin/wiretrace} WIRECAPTURE_CPUS=${WIRECAPTURE_CPUS:-20-23}
mkdir -p "$OUT"
declare -A ENV=([A]="" [B]="FDBGO_NETWORK_GOROUTINE=1" [W]="FDBGO_NETWORK_GOROUTINE=writer" [S]="FDBGO_COALESCE_FROM_SENDER=1")
orders=(ABWS BWSA WSAB SABW)
declare -A ABIN=() AARGS=()
# ARMS_FILE (run_cell.py's JSON arm specs, env only) and ARMS (comma list) replace the built-in arms.
if [[ -n ${ARMS_FILE:-} ]]; then
  ENV=()
  while IFS='|' read -r name bin envs args; do ENV[$name]=$envs; ABIN[$name]=$bin; AARGS[$name]=$args; done < <(python3 -c "
import json; d=json.load(open('$ARMS_FILE'))
for k, v in d.items(): print('|'.join([k, v.get('binary', '-'), ' '.join(v.get('env', [])), ' '.join(v.get('args', []))]))")
  IFS=, read -ra list <<<"$ARMS"
  orders=("$(IFS=,; echo "${list[*]}")" "$(printf '%s\n' "${list[@]}" | tac | paste -sd,)")
fi
armbin() { local b=${ABIN[$1]:--}; [[ $b == - ]] && b=$BIN; echo "$b"; }
client() { # arm iterations outdir
  env -i PATH=/usr/bin:/bin HOME="$HOME" GOMAXPROCS=8 GOGC=100 GOMEMLIMIT=off ${ENV[$1]} \
    taskset -c "${CLIENT_CPUS:-1-8}" "$(armbin "$1")" -cluster-file "$CF" -workload "${WL:-get100}" -workers "${WORKERS:-64}" \
    -iterations "$2" -warmup 20000 -allow-writes -output "$3" ${AARGS[$1]:-}
}
quiet() { while (( $(cut -d. -f1 /proc/loadavg) > ${LOAD_MAX:-16} )); do sleep 10; done; }
for ((r=0; r<ROUNDS; r++)); do
  order=${orders[$((r % ${#orders[@]}))]}
  if [[ -n ${ARMS_FILE:-} || $order == *,* ]]; then IFS=, read -ra arms_in_order <<<"$order"; else mapfile -t arms_in_order < <(grep -o . <<<"$order"); fi
  for arm in "${arms_in_order[@]}"; do
    tag=r$r-$arm
    quiet; load=$(cut -d' ' -f1 /proc/loadavg)
    if [[ -z ${SKIP_METRICS:-} ]]; then
      # Metrics window: idle margins longer than the servers' 5 s metric interval.
      sleep 7; start=$(date +%s.%N)
      client "$arm" "${METRIC_ITERS:-2400000}" "$OUT/$tag-metrics" >/dev/null 2>&1
      end=$(date +%s.%N); sleep 7
      ops=$(python3 -c "import json;s=json.load(open('$OUT/$tag-metrics/summary.json'));print(s['completed']+s['warmup_completed'])")
      python3 "$HERE/srvmetrics.py" "$start" "$end" "$ops" "$LOGS/s/logs" "$LOGS/t/logs" "$LOGS/d/logs" > "$OUT/$tag-srvmetrics.json"
      rm -f "$OUT/$tag-metrics/samples.csv" "$OUT/$tag-metrics/warmup.csv"
    fi
    # Capture: a short run, analyzed for packets/frames per role.
    quiet
    "$REPO/tools/clientbench/scripts/wirecapture.sh" "$OUT/$tag-cap" "$PORTS" -- \
      env -i PATH=/usr/bin:/bin HOME="$HOME" GOMAXPROCS=8 GOGC=100 GOMEMLIMIT=off ${ENV[$arm]} \
      taskset -c "${CLIENT_CPUS:-1-8}" "$(armbin "$arm")" -cluster-file "$CF" -workload "${WL:-get100}" -workers "${WORKERS:-64}" \
      -iterations "${CAP_ITERS:-200000}" -warmup "${CAP_WARMUP:-20000}" -allow-writes -output "$OUT/$tag-cap/run" ${AARGS[$arm]:-} > "$OUT/$tag-wiretrace.json"
    capops=$(python3 -c "import json;s=json.load(open('$OUT/$tag-cap/run/summary.json'));print(s['completed']+s['warmup_completed'])")
    python3 "$HERE/wirestats.py" "$OUT/$tag-cap/capture.pcap" "$PORTS" "$capops" > "$OUT/$tag-wirestats.json"
    rm -f "$OUT/$tag-cap/capture.pcap" "$OUT/$tag-cap/events.csv" "$OUT/$tag-cap/run/samples.csv" "$OUT/$tag-cap/run/warmup.csv"
    echo "$tag load1=$load ops=${ops:-} capops=$capops $(date -Is)"
  done
done
