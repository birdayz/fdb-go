#!/usr/bin/env bash
# Build the clientbench binaries (before: pure Go; final: pure Go and CGo) from
# $root/trees and install them as $root/bin/client-*.
set -euo pipefail
root=${FDB_BENCH_ROOT:-/srv/fdb-bench}
jobs=${FDB_BENCH_BUILD_JOBS:-16}
mkdir -p "$root/bin" "$root/provenance" "$root/results/build"
for side in before final; do
  cd "$root/trees/$side"
  targets=(//tools/clientbench/cmd/clientbench:clientbench_go)
  if [[ $side == final ]]; then targets+=(//tools/clientbench/cmd/clientbench:clientbench_cgo); fi
  bazelisk --output_user_root="$root/cache/bazel" build --disk_cache="$root/cache/disk" --repository_cache="$root/cache/repository" --jobs="$jobs" "${targets[@]}" > "$root/results/build/client-$side.log" 2>&1
  binary=$(bazelisk --output_user_root="$root/cache/bazel" cquery --output=files //tools/clientbench/cmd/clientbench:clientbench_go 2>> "$root/results/build/client-$side.log")
  install -m755 "$binary" "$root/bin/client-$side-go"
  if [[ $side == final ]]; then
    binary=$(bazelisk --output_user_root="$root/cache/bazel" cquery --output=files //tools/clientbench/cmd/clientbench:clientbench_cgo 2>> "$root/results/build/client-$side.log")
    install -m755 "$binary" "$root/bin/client-final-cgo"
  fi
  bazelisk --output_user_root="$root/cache/bazel" shutdown
  printf 'Built client %s\n' "$side"
done
sha256sum "$root"/bin/client-* > "$root/provenance/client-binaries.sha256"
ldd "$root/bin/client-final-cgo" > "$root/provenance/client-cgo-linkage.txt"
cat "$root/provenance/client-binaries.sha256"
