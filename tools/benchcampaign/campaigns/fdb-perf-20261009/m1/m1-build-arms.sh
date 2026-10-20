#!/usr/bin/env bash
# M1 local build: one detached worktree + one bazel-built recordlayer_test per arm.
# Usage: m1-build-arms.sh OUT_DIR BASE_FULLSHA [NAME=FULLSHA ...]
# Arm "base" is BASE_FULLSHA. Every other arm must descend from BASE (a local combination
# commit from m1-make-combined.sh is fine). An arm whose binary already exists in OUT_DIR
# for the same commit and shim is reused, so BASE can be pre-built and NEW added later.
# All Bazel work (build + info) runs in ONE nice'd, fast.lock'ed batch per arm, gated on
# availMB >= 8000. The worktree (tracked + untracked, exact bytes) is hashed before and
# after the build and must be identical; its diff vs the commit must be exactly the shim.
set -Eeuo pipefail
out=${1:?out dir}; base=${2:?base full sha}; shift 2
repo=${FDB_GO_WORKTREES}/fdb-go
wtroot=${FDB_PERF_HOME}/worktrees
here=$(cd "$(dirname "$0")" && pwd)
shim=$here/m1-bench-shim.patch
lock=${FDB_PERF_HOME}/validation/fast.lock
sysmon=${FDB_PERF_HOME}/validation/sysmon.log
diskcache=$HOME/.cache/bazel-disk-m1
target=//pkg/recordlayer:recordlayer_test
mkdir -p "$out/bin" "$out/provenance"
full() { [[ $1 =~ ^[0-9a-f]{40}$ ]] || { echo "not a full SHA: $1" >&2; exit 2; }; }
full "$base"
arms=("base=$base" "$@")
names=()
for spec in "${arms[@]}"; do
  n=${spec%%=*}; s=${spec#*=}; full "$s"
  [[ $n =~ ^[a-z0-9_]+$ ]] || { echo "bad arm name $n" >&2; exit 2; }
  git -C "$repo" cat-file -e "$s^{commit}"
  if [[ $n != base ]]; then
    git -C "$repo" merge-base --is-ancestor "$base" "$s" || { echo "$n=$s does not descend from BASE" >&2; exit 1; }
  fi
  names+=("$n")
done
if [[ -f $out/provenance/m1-bench-shim.patch ]]; then
  cmp "$shim" "$out/provenance/m1-bench-shim.patch" || { echo "shim changed since earlier arms were built" >&2; exit 1; }
fi
cp "$shim" "$out/provenance/"
sha256sum "$shim" > "$out/provenance/shim.sha256"
pins=(go.mod go.sum MODULE.bazel MODULE.bazel.lock .bazelversion .bazelrc)

# Exact-bytes snapshot of tracked + untracked (non-ignored) files; symlinks hashed by target.
snapshot() {
  (cd "$1" && git ls-files -z --cached --others --exclude-standard | sort -zu |
    while IFS= read -r -d '' f; do
      if [[ -L $f ]]; then printf '%s  %s\n' "$(readlink "$f" | sha256sum | cut -d' ' -f1)" "L:$f"
      elif [[ -f $f ]]; then sha256sum -- "$f"
      else printf 'MISSING  %s\n' "$f"; fi
    done)
}
# The worktree must be the commit plus exactly the shim, with no untracked files.
exact_shim() {
  [[ $(git -C "$1" rev-parse HEAD) == "$2" ]] &&
  git -C "$1" diff --binary | cmp -s - "$shim" &&
  [[ -z $(git -C "$1" ls-files --others --exclude-standard) ]] &&
  [[ -z $(git -C "$1" diff --cached --name-only) ]]
}

for spec in "${arms[@]}"; do
  n=${spec%%=*}; s=${spec#*=}; wt=$wtroot/m1-$n; p=$out/provenance/$n
  if [[ -f $out/bin/recordlayer_test.$n && $(cat "$p/commit.txt" 2>/dev/null) == "$s" ]]; then
    echo "reuse $n ($s)"; continue
  fi
  if [[ -d $wt ]]; then
    if exact_shim "$wt" "$s"; then echo "existing $wt = $s + exact shim"
    elif [[ $(git -C "$wt" rev-parse HEAD) == "$s" && -z $(git -C "$wt" status --porcelain) ]]; then
      git -C "$wt" apply --check "$shim"; git -C "$wt" apply "$shim"
    else
      echo "REFUSE: $wt is not $s (+ exactly the shim); inspect/remove it by hand" >&2; exit 1
    fi
  else
    git -C "$repo" worktree add --detach "$wt" "$s"
    git -C "$wt" apply --check "$shim"; git -C "$wt" apply "$shim"
  fi
  exact_shim "$wt" "$s" || { echo "$wt is not exactly $s + shim" >&2; exit 1; }
  rm -rf "$p"; mkdir -p "$p"
  echo "$s" > "$p/commit.txt"
  git -C "$repo" diff --binary "$base" "$s" > "$p/patch-from-base.diff"
  git -C "$repo" diff --name-only "$base" "$s" > "$p/patch-files.txt"
  git -C "$repo" log --format='%H %P %s' "$base..$s" > "$p/commits.txt"
  git -C "$repo" archive --format=tar.gz -o "$p/source-$s.tar.gz" "$s"
  (cd "$wt" && sha256sum "${pins[@]}") > "$p/pins.sha256"
  snapshot "$wt" > "$p/tree-before-build.sha256"
  start=$(date -u +%FT%TZ)
  # One locked batch: availMB gate (re-checked after acquiring the lock), build, bazel-bin lookup.
  (cd "$wt" && nice -n 15 flock --close "$lock" bash -c '
      set -euo pipefail
      while :; do
        a=$(tail -1 "$1" | sed -n "s/.*availMB=\([0-9]*\).*/\1/p")
        [[ -n $a && $a -ge 8000 ]] && break
        echo "availMB=${a:-?} < 8000; waiting" >&2; sleep 30
      done
      bazelisk build --compilation_mode=fastbuild --disk_cache="$2" "$3"
      # Configured-target output (same flags as the build), not a bazel-bin alias.
      bazelisk cquery --compilation_mode=fastbuild --disk_cache="$2" --output=files "$3" 2>/dev/null | sed "s/^/CQ=/"
      bazelisk shutdown
    ' _ "$sysmon" "$diskcache" "$target") > "$p/build.log" 2>&1
  mapfile -t cq < <(sed -n 's/^CQ=//p' "$p/build.log")
  [[ ${#cq[@]} -eq 1 ]] || { echo "cquery gave ${#cq[@]} outputs: ${cq[*]}" >&2; exit 1; }
  bin=${cq[0]}; [[ $bin == /* ]] || bin=$wt/$bin
  [[ -f $bin && -x $bin ]] || { echo "configured output not an executable: $bin" >&2; exit 1; }
  echo "${cq[0]}" > "$p/configured-output.txt"
  snapshot "$wt" > "$p/tree-after-build.sha256"
  cmp "$p/tree-before-build.sha256" "$p/tree-after-build.sha256" || { echo "$wt changed during build" >&2; exit 1; }
  exact_shim "$wt" "$s" || { echo "$wt no longer exactly $s + shim" >&2; exit 1; }
  install -m 0555 "$bin" "$out/bin/recordlayer_test.$n"
  { echo "build_start=$start build_end=$(date -u +%FT%TZ)"; grep -E '^(Build label|INFO: Build completed|INFO: [0-9]+ processes)' "$p/build.log" || true
    file "$out/bin/recordlayer_test.$n"; ldd "$out/bin/recordlayer_test.$n" 2>&1 || true
    objdump -T "$out/bin/recordlayer_test.$n" | grep -o 'GLIBC_[0-9.]*' | sort -uV | tail -1
    go version "$out/bin/recordlayer_test.$n" 2>&1 || true; } > "$p/binary-info.txt"
done
# Dependency/build pins must be identical across arms (recorded either way).
for n in "${names[@]}"; do
  cmp -s "$out/provenance/base/pins.sha256" "$out/provenance/$n/pins.sha256" \
    && echo "$n pins==base" || echo "$n PINS DIFFER FROM BASE"
done | tee "$out/provenance/pins-compare.txt"
(cd "$out" && sha256sum bin/* provenance/shim.sha256 provenance/*/patch-from-base.diff) > "$out/binaries.sha256"
printf '%s\n' "${arms[@]}" > "$out/arms.txt"
echo "built: ${names[*]}"; cat "$out/binaries.sha256"
