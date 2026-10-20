# PREPARED ONLY. Run on the client at the same sealed absolute paths, after collecting node telemetry for this phase.
set -euo pipefail
umask 077
R=${R:-/srv/fdb-bench}
T="$R/tooling/tools/benchcampaign/multinode"
: "${PHASE:?M2rl|M2skip|M1w1|M1w64|M1w8|M1w32}"
: "${MANIFEST_SHA256:?Acknowledged seal SHA, not a newly accepted changed hash}"
case "$PHASE" in
  M2rl|M2skip) MANIFEST="$R/plans/testingb/manifest.json"; RESULTS="$R/results/$PHASE";;
  M1w1|M1w64|M1w8|M1w32) MANIFEST="$R/plans/client/manifest.json"; RESULTS="$R/results/client/$PHASE";;
  *) echo 'REFUSED: unknown phase' >&2; exit 2;;
esac
mkdir -p "$R/reconcile/$PHASE"
python3 -B "$T/build_reconcile_input.py" \
  --manifest "$MANIFEST" --manifest-sha256 "$MANIFEST_SHA256" --phase "$PHASE" \
  --protocol "$R/protocol.json" --results "$RESULTS" --layout "$R/cluster-ssd/layout.json" \
  --client-receipt "$R/telemetry/$PHASE/client-before.json" \
  --client-receipt "$R/telemetry/$PHASE/client-after.json" \
  --node node1="$R/telemetry/$PHASE/node1" --node node2="$R/telemetry/$PHASE/node2" --node node3="$R/telemetry/$PHASE/node3" \
  --output "$R/reconcile/$PHASE/input.json"
python3 -B "$T/reconcile_telemetry.py" --input "$R/reconcile/$PHASE/input.json" --output "$R/reconcile/$PHASE/report.json"
# Exit 3 means omissions, not permission to retry/override. Preserve and report it.
