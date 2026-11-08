#!/bin/bash
# Waits until the 1-minute load is < 12 and otelg3mb's storage and log queues are < 1 MB on two polls in a row; prints "load queue".
ok=0
for i in $(seq 1 2000); do
  l=$(cut -d' ' -f1 /proc/loadavg); q=$(/var/tmp/otelcmp/g3mb/queue.sh)
  if awk -v l=$l -v q=$q 'BEGIN{exit !(l<12 && q<1000000)}'; then ok=$((ok+1)); else ok=0; fi
  [ $ok -ge 2 ] && { echo "$l $q"; exit 0; }
  sleep 5
done
echo "timeout $l $q"; exit 1
