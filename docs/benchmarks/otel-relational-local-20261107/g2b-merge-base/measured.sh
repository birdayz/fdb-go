#!/bin/bash
# measured.sh TAG OUTPREFIX CMD...: waits for quiet (load < 12), then runs CMD holding
# both machine locks; rechecks the load inside the locks and retries if it rose to 12 or more.
# Appends "TAG rc=.. quiet=[load] load start/end time user sys real at UTC" to runs.txt.
tag=$1 out=$2; shift 2
cd /var/tmp/otelcmp/g2bmb
while true; do
  q=$(./quiet.sh) || { echo "$tag quiet timeout $q" | tee -a runs.txt; exit 1; }
  flock -o /tmp/fdb-heavy-lane.lock flock -o /tmp/otelcmp/justtest.lock bash -c '
    tag=$1 out=$2 q=$3; shift 3
    s=$(cut -d" " -f1 /proc/loadavg)
    awk -v l=$s "BEGIN{exit !(l<12)}" || exit 99
    TIMEFORMAT="%U %S %R"
    { time "$@" > $out.log 2>&1 ; } 2> $out.time; rc=$?
    e=$(cut -d" " -f1 /proc/loadavg)
    echo "$tag rc=$rc quiet=[$q] load $s/$e time $(tail -1 $out.time) at $(date -u +%H:%M:%SZ)" | tee -a runs.txt
    exit 0' _ "$tag" "$out" "$q" "$@"
  rc=$?
  [ $rc = 99 ] && { sleep 10; continue; }
  exit $rc
done
