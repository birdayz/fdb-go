#!/bin/bash
cd /var/tmp/otelcmp/g3mb
echo "# campaign-cb start $(date -Is)" >> runs.txt
for r in 1 2; do
  if [ $r = 1 ]; then order="base head"; else order="head base"; fi
  for msg in fts off; do for arm in $order; do ./cb.sh $arm $msg $r; done; done
done
echo "# campaign-cb end $(date -Is)" >> runs.txt
