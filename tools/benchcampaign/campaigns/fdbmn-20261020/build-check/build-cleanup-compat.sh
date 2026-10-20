#!/usr/bin/env bash
set -euo pipefail
umask 077
root=${FDB_PERF_HOME}/campaigns/fdbmn-20261020/build-check
export BAZELISK_HOME="$root/cache/bazelisk"
export TMPDIR="$root/tmp"
for side in old master; do
  cd "$root/trees/$side"
  find . -type f -print0 | sort -z | xargs -0 sha256sum > "$root/logs/$side-cleanup-inputs.sha256"
  set +e
  bazelisk --output_user_root="$root/cache/bazel" build \
    --repository_cache="$root/cache/repository" --disk_cache="$root/cache/disk" \
    --lockfile_mode=error --jobs=4 --local_resources=cpu=4 --local_resources=memory=8192 \
    //tools/clientbench/cmd/clientbench:clientbench_go \
    //tools/clientbench/cmd/clientbench:clientbench_cgo \
    > "$root/logs/$side-cleanup-build.log" 2>&1
  status=$?
  set -e
  printf '%s\n' "$status" > "$root/logs/$side-cleanup-build.status"
  tail -30 "$root/logs/$side-cleanup-build.log"
  sha256sum -c "$root/logs/$side-cleanup-inputs.sha256" > "$root/logs/$side-cleanup-input-verification.log"
  if (( status == 0 )); then
    for backend in go cgo; do
      path=$(bazelisk --output_user_root="$root/cache/bazel" cquery \
        --repository_cache="$root/cache/repository" --disk_cache="$root/cache/disk" \
        --lockfile_mode=error --output=files \
        "//tools/clientbench/cmd/clientbench:clientbench_$backend" \
        2>> "$root/logs/$side-cleanup-cquery.log")
      test -f "$path"
      install -m755 "$path" "$root/bin/$side/client-$backend"
    done
  fi
  bazelisk --output_user_root="$root/cache/bazel" shutdown
  printf 'RESULT %s cleanup-compat status=%s\n' "$side" "$status"
  test "$status" = 0
done
sha256sum "$root/bin/old/"* "$root/bin/master/"* > "$root/logs/cleanup-compat-binaries.sha256"
date -u +%Y-%m-%dT%H:%M:%SZ > "$root/logs/cleanup-compat-cpu-heavy-finished-at.txt"
