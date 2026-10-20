#!/usr/bin/env bash
# M1 VM entry point. Usage: M1_DEADLINE_EPOCH=<unix s> m1-run.sh CAMPAIGN_DIR
# M1_DEADLINE_EPOCH (required) = absolute end of the 45-min M1 VM slot from astra-perf-measure's
# R18->M1 handoff. The runner stops before any process that could end later than
# deadline - post_reserve_s (frozen in plan.json), leaving that reserve for post-checks, cleanup,
# analysis, inventory and archive.
# CAMPAIGN_DIR holds m1.py, m1-run.sh, config/plan/schedule, bin/, provenance/, inputs.sha256.
# No builds, profiling or other experiments may run on the VM while this runs.
set -Eeuo pipefail
d=$(cd "${1:?campaign dir}" && pwd); cd "$d"
: "${M1_DEADLINE_EPOCH:?absolute VM-slot deadline required}"
export M1_DEADLINE_EPOCH
exec > >(tee -a "$d/m1-run.log") 2>&1
teepid=$!
trap 'rc=$?; printf "exit_code=%s\nfinished_utc=%s\n" "$rc" "$(date -u +%FT%TZ)" > "$d/status.txt"' EXIT
echo "BEGIN $(date -u +%FT%TZ) deadline=$(date -u -d @"$M1_DEADLINE_EPOCH" +%FT%TZ)"
sha256sum --strict -c inputs.sha256
[[ ! -e runs.jsonl ]] || { echo "runs.jsonl exists: never resume" >&2; exit 2; }
env | grep -q '^FDB_NETWORK_OPTION_' && { echo "FDB_NETWORK_OPTION_* set" >&2; exit 2; }
cf=$(python3 -c 'import json;print(json.load(open("plan.json"))["cluster_file"])')
camp=$(python3 -c 'import json;print(json.load(open("plan.json"))["campaign"])')
# Competing benchmark processes: match /proc/*/exe and full argv (comm is truncated to 15 bytes).
python3 - "$d" <<'PY'
import os,sys
from pathlib import Path
d=sys.argv[1]
mine={str(p.resolve()) for p in Path(d,"bin").iterdir()}
names=("recordlayer_test","clientbench","astra-perf-run")
hits=[]
for pid in filter(str.isdigit,os.listdir("/proc")):
    try:
        exe=os.readlink(f"/proc/{pid}/exe")
        argv=open(f"/proc/{pid}/cmdline","rb").read().split(b"\0")
    except OSError:
        continue
    a0=argv[0].decode(errors="replace") if argv and argv[0] else ""
    if exe in mine or any(n in os.path.basename(exe) or n in os.path.basename(a0) for n in names):
        hits.append((pid,exe,a0))
if hits:
    sys.exit(f"competing benchmark processes: {hits}")
PY
health() {  # $1 = pre|post: status json must be available + healthy
  timeout 30 fdbcli -C "$cf" --exec 'status json' > "identity/$1-status.json" &&
  python3 -c 'import json,sys;s=json.load(open(sys.argv[1]))["client"]["database_status"];sys.exit(0 if s["available"] and s["healthy"] else 1)' "identity/$1-status.json"
}
mkdir -p identity
{ date -u +%FT%TZ; uname -a; cat /etc/os-release; nproc; lscpu; cat /proc/cmdline
  fdbserver --version; fdbcli --version; sha256sum "$cf"; cat "$cf"; python3 --version; ldd --version | head -1
  taskset -p $$; ps -eo pid,psr,comm,args | grep -E 'fdbserver|fdbmonitor' | grep -v grep; } > identity/pre.txt 2>&1 || true
health pre || { echo "PRE_HEALTH_FAILED" >&2; exit 2; }
python3 - <<'PY'
import hashlib,json
h=lambda p:hashlib.sha256(open(p,"rb").read()).hexdigest()
json.dump({"plan_sha256":h("plan.json"),"schedule_sha256":h("schedule.json"),"inputs_sha256":h("inputs.sha256")},open("pins.json","w"),indent=1)
PY
echo "TIMING_BEGIN $(date -u +%FT%TZ)"
# Controller signals are forwarded to the runner, which kills the current child's process group.
taskset --cpu-list 0-1 python3 -B m1.py run "$d" & runner=$!
trap 'kill -TERM "$runner" 2>/dev/null' TERM INT HUP
rc=0; wait "$runner" || rc=$?
while kill -0 "$runner" 2>/dev/null; do wait "$runner" || rc=$?; done
trap - TERM INT HUP
echo "TIMING_END rc=$rc $(date -u +%FT%TZ)"
valid=1
sha256sum --strict -c inputs.sha256 > identity/post-inputs-check.log || { echo "POST_INPUTS_MISMATCH"; valid=0; }
health post || { echo "POST_HEALTH_FAILED"; valid=0; }
# Data scope: every M1 key lives under tuple-packed "m1/<campaign>/..."; cleared after timing, never during.
timeout 60 fdbcli -C "$cf" --exec "writemode on; clearrange \\x02m1/$camp/ \\x02m1/${camp}0" > identity/cleanup.log 2>&1 || echo "CLEANUP_FAILED (non-timing)"
if [[ $rc -eq 0 && $valid -eq 1 ]]; then
  taskset --cpu-list 0-1 python3 -B m1.py analyze "$d" > analysis.stdout || { echo "ANALYSIS_FAILED"; valid=0; }
else
  echo "CAMPAIGN_NOT_VALID rc=$rc valid=$valid: no inference"
fi
echo "END $(date -u +%FT%TZ) rc=$rc valid=$valid"
# Full inventory of every file in the campaign dir (inputs, identity, plan, schedule, provenance,
# bins, raw, runs, analysis, logs, status), then the archive astra verifies remotely and after download.
printf 'exit_code=%s\nvalid=%s\nfinished_utc=%s\n' "$rc" "$valid" "$(date -u +%FT%TZ)" > status.txt
trap - EXIT
exec >>"$d.final.log" 2>&1   # close the tee'd log before hashing it
wait "$teepid" || true
find . -type f ! -name ALL-FILES.sha256 -print0 | sort -z | xargs -0 sha256sum > ALL-FILES.sha256
tar -C "$(dirname "$d")" -czf "$d.results.tar.gz" "$(basename "$d")"
sha256sum "$d.results.tar.gz" > "$d.results.tar.gz.sha256"
[[ $rc -eq 0 && $valid -eq 1 ]]
