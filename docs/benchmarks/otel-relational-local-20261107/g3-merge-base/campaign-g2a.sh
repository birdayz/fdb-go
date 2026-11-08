#!/bin/bash
cd /var/tmp/otelcmp/g3mb
echo "# campaign-g2a start $(date -Is)" >> runs.txt
for r in 1 2 3; do
  if [ $r = 2 ]; then order="head base"; else order="base head"; fi
  for arm in $order; do ./g2a-ingest.sh $arm $r; done
done
for l in a b; do
  if [ $l = a ]; then order="base head"; else order="head base"; fi
  for arm in $order; do ./g2a-query.sh $arm $l; done
done
echo "# campaign-g2a end $(date -Is)" >> runs.txt
