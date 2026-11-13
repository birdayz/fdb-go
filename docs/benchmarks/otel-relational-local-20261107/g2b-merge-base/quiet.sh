#!/bin/bash
# Waits until the 1-minute load is < 12 on two polls in a row; prints the load.
# (The benchmark starts its own FDB testcontainer, so there is no queue to check.)
ok=0
for i in $(seq 1 2000); do
  l=$(cut -d' ' -f1 /proc/loadavg)
  if awk -v l=$l 'BEGIN{exit !(l<12)}'; then ok=$((ok+1)); else ok=0; fi
  [ $ok -ge 2 ] && { echo "$l"; exit 0; }
  sleep 5
done
echo "timeout $l"; exit 1
