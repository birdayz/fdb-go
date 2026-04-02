#!/usr/bin/env bash
# Runs the FDB binding tester against our pure Go stacktester.
# Requires: running FDB cluster, Python 3 + fdb package.
#
# Usage:
#   ./run_binding_test.sh <cluster-file> [num-ops]
#
# Or via Bazel:
#   bazelisk run //cmd/fdb-stacktester:run_binding_test -- /path/to/fdb.cluster

set -euo pipefail

CLUSTER_FILE="${1:?usage: $0 <cluster-file> [num-ops]}"
NUM_OPS="${2:-100}"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

# Find the stacktester binary (built by Bazel or go build).
TESTER="${SCRIPT_DIR}/fdb-stacktester"
if [ ! -x "$TESTER" ]; then
    TESTER="$(bazelisk info bazel-bin 2>/dev/null)/cmd/fdb-stacktester/fdb-stacktester_/fdb-stacktester"
fi
if [ ! -x "$TESTER" ]; then
    echo "Build the stacktester first: bazelisk build //cmd/fdb-stacktester"
    exit 1
fi

# Find bindingtester.py in the FDB source (fetched by Bazel).
FDB_SRC="$(bazelisk info output_base 2>/dev/null)/external/foundationdb+"
BINDING_TESTER="${FDB_SRC}/bindings/bindingtester/bindingtester.py"
if [ ! -f "$BINDING_TESTER" ]; then
    echo "Cannot find bindingtester.py at: $BINDING_TESTER"
    echo "Run 'bazelisk build //...' first to fetch FDB source."
    exit 1
fi

# Set up Python venv with fdb if needed.
VENV="/tmp/fdb-binding-tester-venv"
if [ ! -f "$VENV/bin/python3" ]; then
    python3 -m venv "$VENV"
    "$VENV/bin/pip" install -q foundationdb
fi

echo "=== FDB Binding Tester ==="
echo "Cluster: $CLUSTER_FILE"
echo "Ops:     $NUM_OPS"
echo "Tester:  $TESTER"
echo "Harness: $BINDING_TESTER"
echo ""

# Run the binding tester.
# PYTHONPATH includes the bindingtester directory so imports work.
PYTHONPATH="${FDB_SRC}/bindings/bindingtester" \
FDB_CLUSTER_FILE="$CLUSTER_FILE" \
    "$VENV/bin/python3" "$BINDING_TESTER" \
    --cluster-file "$CLUSTER_FILE" \
    --test-name api \
    --num-ops "$NUM_OPS" \
    --tester-binary "$TESTER"
