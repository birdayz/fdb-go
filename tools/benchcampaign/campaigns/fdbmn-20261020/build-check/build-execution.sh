#!/usr/bin/env bash
set -euo pipefail
umask 077
root=${FDB_PERF_HOME}/campaigns/fdbmn-20261020/build-check
export BAZELISK_HOME="$root/cache/bazelisk"
export TMPDIR="$root/tmp"
MASTER_REVISION=fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc bash "$root/build-frozen.sh"
test "$(cat "$root/logs/old-build.status")" = 0
test "$(cat "$root/logs/master-build.status")" = 0
cd "$root/trees/master"
find . -type f -print0 | sort -z | xargs -0 sha256sum > "$root/logs/master-placement-inputs.sha256"
set +e
bazelisk --output_user_root="$root/cache/bazel" build \
  --repository_cache="$root/cache/repository" --disk_cache="$root/cache/disk" \
  --lockfile_mode=error --jobs=4 --local_resources=cpu=4 --local_resources=memory=8192 \
  //tools/benchcampaign/multinode/placementprobe/cmd/placementprobe:placementprobe_cgo \
  > "$root/logs/master-placement-build.log" 2>&1
status=$?
set -e
printf '%s\n' "$status" > "$root/logs/master-placement-build.status"
tail -35 "$root/logs/master-placement-build.log"
sha256sum -c "$root/logs/master-placement-inputs.sha256" > "$root/logs/master-placement-input-verification.log"
if (( status == 0 )); then
  path=$(bazelisk --output_user_root="$root/cache/bazel" cquery \
    --repository_cache="$root/cache/repository" --disk_cache="$root/cache/disk" \
    --lockfile_mode=error --output=files \
    //tools/benchcampaign/multinode/placementprobe/cmd/placementprobe:placementprobe_cgo \
    2> "$root/logs/master-placement-cquery.log")
  test -f "$path"
  install -m755 "$path" "$root/bin/master/placementprobe-cgo"
  sha256sum "$root/bin/old/"* "$root/bin/master/"* > "$root/logs/execution-binaries.sha256"
fi
bazelisk --output_user_root="$root/cache/bazel" shutdown
printf 'placementprobe_exit=%s\n' "$status"
date -u +%Y-%m-%dT%H:%M:%SZ > "$root/logs/cpu-heavy-builds-finished-at.txt"
exit "$status"
