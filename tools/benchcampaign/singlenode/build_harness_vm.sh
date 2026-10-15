#!/usr/bin/env bash
# Build the overlaid testing.B harness binaries for every tree into $root/bin.
set -euo pipefail
root=${FDB_BENCH_ROOT:-/srv/fdb-bench}
jobs=${FDB_BENCH_BUILD_JOBS:-16}
mkdir -p "$root/bin" "$root/results/build" "$root/provenance"
copy_target() {
  local target=$1 dest=$2 path
  path=$(bazelisk --output_user_root="$root/cache/bazel" cquery --output=files "$target" 2>> "$log")
  test -f "$path"
  install -m755 "$path" "$root/bin/$dest"
}
for side in legacy before pr2 final; do
  cd "$root/trees/$side"
  log="$root/results/build/harness-$side.log"
  targets=(//pkg/recordlayer:recordlayer_test)
  if [[ $side != pr2 ]]; then targets+=(//pkg/relational/sqlbench:sqlbench_test); fi
  if [[ $side == pr2 || $side == final ]]; then targets+=(//pkg/recordlayer/benchfetch:benchfetch_test); fi
  if [[ $side == final ]]; then targets+=(//pkg/recordlayer/benchfetch/planskip:planskip_test //pkg/relational/sqlbench/fetchmethod:fetchmethod_test); fi
  bazelisk --output_user_root="$root/cache/bazel" build --disk_cache="$root/cache/disk" --repository_cache="$root/cache/repository" --jobs="$jobs" "${targets[@]}" > "$log" 2>&1
  copy_target //pkg/recordlayer:recordlayer_test "rl-$side"
  if [[ $side != pr2 ]]; then copy_target //pkg/relational/sqlbench:sqlbench_test "sql-$side"; fi
  if [[ $side == pr2 || $side == final ]]; then copy_target //pkg/recordlayer/benchfetch:benchfetch_test "fetch-$side"; fi
  if [[ $side == final ]]; then
    copy_target //pkg/recordlayer/benchfetch/planskip:planskip_test planskip-final
    copy_target //pkg/relational/sqlbench/fetchmethod:fetchmethod_test sqlfetch-final
  fi
  bazelisk --output_user_root="$root/cache/bazel" shutdown
  printf 'Built overlaid harness %s\n' "$side"
done
sha256sum "$root"/bin/* > "$root/provenance/all-binaries.sha256"
find "$root/trees/final" -path '*/external/rules_go++go_sdk+main___download_0/bin/go' -print -quit > "$root/provenance/go-sdk-location.txt"
cat "$root/provenance/all-binaries.sha256"
