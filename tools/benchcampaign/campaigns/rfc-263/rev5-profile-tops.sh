#!/usr/bin/env bash
# Diagnostic attribution only; never run during qualification timing.
set -euo pipefail
P=${1:?evidence directory}
GO=${2:?pinned Go1.26.9 binary}
mkdir -p "$P/pprof-top"
for profile in block mutex; do
  for arm in baseline rev4 rev5; do
    GOTOOLCHAIN=local "$GO" tool pprof -top -nodecount=50 "$P/bin/$arm" \
      "$P/runs/tail-diag-$profile-range100-p0-$arm/$profile.pprof" \
      > "$P/pprof-top/$profile-$arm.txt" 2>&1
    GOTOOLCHAIN=local "$GO" tool pprof -raw "$P/bin/$arm" \
      "$P/runs/tail-diag-$profile-range100-p0-$arm/$profile.pprof" \
      > "$P/pprof-top/$profile-$arm.raw.txt" 2>&1
  done
done
