#!/usr/bin/env bash
set -euo pipefail
umask 077
root=${FDB_PERF_HOME}/campaigns/fdbmn-20261020/build-check
repo=${FDB_GO_WORKTREES}/fdb-go-flake
archive=${FDB_PERF_HOME}/hcloud-rebench-20261014
export BAZELISK_HOME="$root/cache/bazelisk"
export TMPDIR="$root/tmp"
mkdir -p "$TMPDIR"
old=267a41434a81723b07e7150869d702636a1b91a9
master=${MASTER_REVISION:-ae14b220883c4985031d5659f8b34cd7ecece8a9}
harness=d94b4c61377d173b7c6884fde426a227dab0f0bb
printf 'old=%s\nrehearsal_master=%s\nharness=%s\n' "$old" "$master" "$harness" > "$root/revisions.txt"
targets=(
  //pkg/recordlayer:recordlayer_test
  //pkg/recordlayer/benchfetch:benchfetch_test
  //pkg/recordlayer/benchfetch/planskip:planskip_test
  //pkg/relational/sqlbench:sqlbench_test
  //pkg/relational/sqlbench/fetchmethod:fetchmethod_test
  //tools/clientbench/cmd/clientbench:clientbench_go
  //tools/clientbench/cmd/clientbench:clientbench_cgo
)
names=(rl fetch planskip sql sqlfetch client-go client-cgo)
for side in old master; do
  revision=$old
  if [[ $side == master ]]; then revision=$master; fi
  tree="$root/trees/$side"
  rm -rf "$tree" "$root/bin/$side"
  mkdir -p "$tree" "$root/bin/$side"
  if [[ $side == old ]]; then
    tar -xzf "$archive/sources/final.tar.gz" -C "$tree"
  else
    git -C "$repo" archive "$revision" | tar -xf - -C "$tree"
  fi
  # Replace only benchmark sources in these external, non-git exports.
  rm -rf "$tree/tools/clientbench" "$tree/pkg/testutil/benchcluster" \
    "$tree/pkg/recordlayer/benchfetch" "$tree/pkg/relational/sqlbench"
  for overlay in common remote final-only client-harness harness-doc; do
    tar -xzf "$archive/overlays/final/$overlay.tar.gz" -C "$tree"
  done
  cd "$tree"
  find . -type f -print0 | sort -z | xargs -0 sha256sum > "$root/logs/$side-inputs.sha256"
  printf '\n=== BUILD %s %s ===\n' "$side" "$revision"
  set +e
  bazelisk --output_user_root="$root/cache/bazel" build \
    --repository_cache="$root/cache/repository" --disk_cache="$root/cache/disk" \
    --lockfile_mode=error --jobs=4 --local_resources=cpu=4 --local_resources=memory=8192 \
    "${targets[@]}" > "$root/logs/$side-build.log" 2>&1
  status=$?
  set -e
  printf '%s\n' "$status" > "$root/logs/$side-build.status"
  tail -35 "$root/logs/$side-build.log"
  sha256sum -c "$root/logs/$side-inputs.sha256" > "$root/logs/$side-input-verification.log"
  if (( status == 0 )); then
    for i in "${!targets[@]}"; do
      path=$(bazelisk --output_user_root="$root/cache/bazel" cquery \
        --repository_cache="$root/cache/repository" --disk_cache="$root/cache/disk" \
        --lockfile_mode=error --output=files "${targets[$i]}" 2>> "$root/logs/$side-cquery.log")
      test -f "$path"
      install -m755 "$path" "$root/bin/$side/${names[$i]}"
    done
    sha256sum "$root/bin/$side/"* > "$root/logs/$side-binaries.sha256"
  fi
  bazelisk --output_user_root="$root/cache/bazel" shutdown
  printf 'RESULT %s status=%s\n' "$side" "$status"
done
