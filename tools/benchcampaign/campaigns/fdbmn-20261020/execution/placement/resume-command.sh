#!/usr/bin/env bash
# On the campaign client only, after owner/reviewer ACK and copying both scripts.
set -euo pipefail
umask 077
: "${PRIOR_INVOCATION_SHA256:?Set the literal SHA256 obtained from the preserved original provenance/p0/invocation.json preflight}"
exec taskset -c 0-1 python3 -u -B /srv/fdb-bench/execution/placement/resume_p0.py \
  --execute --root /srv/fdb-bench \
  --prior-output /srv/fdb-bench/provenance/p0 \
  --output /srv/fdb-bench/provenance/p0-continuation-1 \
  --ack-prior-p0-sha256 a5a3d0d405810098471a597550c1bdf47a5ae82f031a72a299b338349dde8d38 \
  --ack-current-p0-sha256 c353d713cd527c4acb8fef44d562de98a59be809723e9d0b1e4563a352f259e9 \
  --expect-prior-invocation-sha256 "$PRIOR_INVOCATION_SHA256" \
  --expect-cluster-sha256 69e1731e91d6532a01e2cf41c5ee8716d33083a379b78909a1bf5c81671bd943 \
  --expect-placementprobe-sha256 1af0b563f4169d116a7c975bb2a15e9323853d267c219ce1ddd7a75bad5387cc \
  --expect-fetch-sha256 360f03d66a4afa217db03359b8f657ee43a288eb7a42150c1c615323af1af118 \
  --expect-planskip-sha256 b2020a2044a5feba3e674d5e12a26ed76ff5b114be1bd38aa74491f5d53c29a4
