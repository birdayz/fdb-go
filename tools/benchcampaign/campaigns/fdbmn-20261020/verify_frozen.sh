#!/usr/bin/env bash
# Check that the in-tree copies are the campaign's frozen, hash-pinned tools.
# collect_final.py is byte-identical. analyze_campaign.py differs only in its two
# default paths, written as ${FDB_PERF_HOME}; set FDB_PERF_HOME to the campaign
# controller's directory to restore and check the pinned bytes.
set -euo pipefail
cd "$(dirname "$0")/execution"
echo "8ebe4b4224c3ecf858414203cad8ff7b2256b6b55224b9d2a78fedfa99631217  collect_final.py" | sha256sum -c -
echo "1d622f864dfa0fb3785af9f08d6e609bee4cdd059edc0b34b449dc8cad5fb967  test_analyze_campaign.py" | sha256sum -c -
: "${FDB_PERF_HOME:?set to the controller directory that held the campaign}"
got=$(sed "s#\${FDB_PERF_HOME}#${FDB_PERF_HOME}#g" analyze_campaign.py | sha256sum | cut -d' ' -f1)
want=700fd3c18a319ea6738c00652846313797a4f4bfbcf50e4b3aee37e720a70289
if [ "$got" != "$want" ]; then
  echo "analyze_campaign.py: FAILED ($got)" >&2
  exit 1
fi
echo "analyze_campaign.py (paths restored): OK"
