# PREPARED ONLY. Run on the new campaign client AFTER merged-tree builds, actual layout/runtime and capacity qualification.
# The receipt's source_artifacts_sha256 paths must be final client paths pinned by the globs below.
set -euo pipefail
umask 077
R=${R:-/srv/fdb-bench}
T="$R/tooling/tools/benchcampaign/multinode"
S="$R/frozen/tools/clientbench/scripts"
CF="$R/cluster-ssd/fdb.cluster"
NATIVE_LIBRARY=${NATIVE_LIBRARY:-/usr/lib/libfdb_c.so}
SERVER_BINARY=${SERVER_BINARY:-/usr/sbin/fdbserver}
python3 -B - "$R/protocol.json" <<'PY'
import json,re,sys
p=json.load(open(sys.argv[1]))
assert re.fullmatch('[0-9a-f]{40}',p['production_revisions']['final']), 'RFC-263 merged SHA is still TBD'
assert p['production_revisions']['before_allocation_work_after_read_parity']=='267a41434a81723b07e7150869d702636a1b91a9'
PY
python3 -B "$T/seal_multinode.py" check-orders --protocol "$R/protocol.json" --orders "$R/orders.json"
python3 -B "$T/seal_multinode.py" client-plans \
  --protocol "$R/protocol.json" --orders "$R/orders.json" \
  --scripts-dir "$S" --cluster-file "$CF" --native-library "$NATIVE_LIBRARY" --server-binary "$SERVER_BINARY" \
  --harness-archive "$R/frozen/overlays/client-harness.tar.gz" \
  --binary before="$R/bin/client-previous-go" --binary final="$R/bin/client-master-go" --binary cgo="$R/bin/client-master-cgo" \
  --adapter "$T/client_campaign_mn.py" --layout "$R/cluster-ssd/layout.json" \
  --source-receipt "$R/provenance/source-receipt.json" --vpc-cidr 10.89.20.0/24 \
  --pin /usr/bin/fdbcli --pin "$R/bin/placementprobe-cgo" \
  --pin-glob "$R/sources/*.tar.gz" --pin-glob "$R/frozen/overlays/*.tar.gz" \
  --pin-glob "$T/*.py" --pin-glob "$R/tooling/tools/benchcampaign/singlenode/*.py" --pin-glob "$S/*.py" \
  --results-dir "$R/results/client" --output-dir "$R/plans/client"
sha256sum "$R/plans/client/manifest.json"
# Require 28 campaigns, each runs=18, 0 unsealed; archive plans and receive acknowledgement on this exact SHA.
jq -e '.unsealed == [] and (.campaigns|length)==28 and all(.campaigns[]; .runs==18)' "$R/plans/client/manifest.json"
