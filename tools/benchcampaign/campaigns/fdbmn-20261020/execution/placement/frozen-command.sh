#!/usr/bin/env bash
# Root4218/4221: same frozen attempt1 after added nonprotocol census hit1007.
set -euo pipefail
umask 077
: "${PRIOR_INVOCATION_SHA256:?Set unchanged original provenance/p0/invocation.json literal SHA256}"
: "${PRIOR_CONTINUITY_SHA256:?Set provenance/p0-continuation-1/continuity.json preflight literal SHA256}"
exec taskset -c 0-1 python3 -u -B /srv/fdb-bench/execution/placement/resume_p0.py \
  --execute --root /srv/fdb-bench \
  --prior-output /srv/fdb-bench/provenance/p0 \
  --prior-continuation /srv/fdb-bench/provenance/p0-continuation-1 \
  --output /srv/fdb-bench/provenance/p0-continuation-2 \
  --frozen-after-preclear-1007 \
  --ack-prior-p0-sha256 a5a3d0d405810098471a597550c1bdf47a5ae82f031a72a299b338349dde8d38 \
  --ack-current-p0-sha256 f40d150614377f9a896b95cc0973cbad8d5c8bf17bb22a3e5565f0bec2939c84 \
  --ack-failed-continuation-p0-sha256 c353d713cd527c4acb8fef44d562de98a59be809723e9d0b1e4563a352f259e9 \
  --ack-failed-continuation-resume-sha256 7a9f621b9a20fc761b57fecea2a03fb2e006943931a33687c20a28f31835544f \
  --expect-prior-invocation-sha256 "$PRIOR_INVOCATION_SHA256" \
  --expect-prior-continuity-sha256 "$PRIOR_CONTINUITY_SHA256" \
  --expect-cluster-sha256 69e1731e91d6532a01e2cf41c5ee8716d33083a379b78909a1bf5c81671bd943 \
  --expect-placementprobe-sha256 1af0b563f4169d116a7c975bb2a15e9323853d267c219ce1ddd7a75bad5387cc \
  --expect-fetch-sha256 360f03d66a4afa217db03359b8f657ee43a288eb7a42150c1c615323af1af118 \
  --expect-planskip-sha256 b2020a2044a5feba3e674d5e12a26ed76ff5b114be1bd38aa74491f5d53c29a4
