# PREPARED ONLY. Run on the campaign client after P0 and a full 40-hex merged SHA replaces TBD.
# Receipt variables name the selected attempt, never an attempt selected from timing results.
set -euo pipefail
umask 077
R=${R:-/srv/fdb-bench}
T="$R/tooling/tools/benchcampaign/multinode"
S="$R/frozen/tools/clientbench/scripts"
: "${PLACEMENT_SMALL:?Path to selected benchfetch/small receipt}"
: "${PLACEMENT_SPLIT:?Path to selected benchfetch/split receipt}"
: "${PLACEMENT_PLANSKIP:?Path to selected benchfetch/planskip receipt}"
python3 -B - "$R/protocol.json" <<'PY'
import json,re,sys
p=json.load(open(sys.argv[1]))
assert re.fullmatch('[0-9a-f]{40}',p['production_revisions']['final']), 'RFC-263 merged SHA is still TBD'
PY
python3 -B "$T/seal_multinode.py" check-orders --protocol "$R/protocol.json" --orders "$R/orders.json"
python3 -B "$T/seal_multinode.py" testingb-plans \
  --protocol "$R/protocol.json" --orders "$R/orders.json" --cluster-file "$R/cluster-ssd/fdb.cluster" \
  --binary fetch="$R/bin/fetch-master" --binary planskip="$R/bin/planskip-master" \
  --placement-receipt benchfetch/small="$PLACEMENT_SMALL" \
  --placement-receipt benchfetch/split="$PLACEMENT_SPLIT" \
  --placement-receipt benchfetch/planskip="$PLACEMENT_PLANSKIP" \
  --pin "$R/cluster-ssd/layout.json" --pin "$R/provenance/source-receipt.json" \
  --pin "$R/provenance/fixture-receipts.json" --pin "$R/bin/placementprobe-cgo" \
  --pin /usr/bin/fdbcli --pin /usr/sbin/fdbserver --pin /usr/lib/libfdb_c.so \
  --pin-glob "$R/provenance/p0/fixture-*.json" \
  --pin-glob "$R/sources/*.tar.gz" --pin-glob "$R/frozen/overlays/*.tar.gz" \
  --pin-glob "$T/*.py" --pin-glob "$R/tooling/tools/benchcampaign/singlenode/*.py" --pin-glob "$S/*.py" \
  --output-dir "$R/plans/testingb"
sha256sum "$R/plans/testingb/manifest.json"
# Require exactly M2rl 12 cases/144 jobs and M2skip 4 cases/48 jobs; 0 unsealed. Acknowledge exact manifest SHA.
jq -e '.unsealed == [] and (.phases|length)==2 and any(.phases[]; .phase=="M2rl" and .cases==12 and .jobs==144) and any(.phases[]; .phase=="M2skip" and .cases==4 and .jobs==48)' "$R/plans/testingb/manifest.json"
