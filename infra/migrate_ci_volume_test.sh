#!/bin/bash
# Exercise the data-moving core of migrate-volume.sh (rsync + convergence check +
# symlink swap) against fixtures, so the production run is not the first time
# this logic executes.
set -uo pipefail
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
fail=0
ok(){ echo "ok: $1"; }; bad(){ echo "FAIL: $1"; fail=1; }

CI=$T/mnt/ci-data ; VOL=$T/mnt/HC_Volume_1
mkdir -p "$CI"/{bazel,bazel-disk-cache,bazel-repo-cache,bazelwork,docker} "$VOL"
echo cache > "$CI/bazel-disk-cache/blob"
echo work  > "$CI/bazelwork/w"
dd if=/dev/zero of="$CI/swapfile" bs=1k count=64 status=none
ln -s "$VOL" "$CI/HC_Volume_1"          # the nested bad symlink
mkdir -p "$VOL/lost+found"
# Ownership-sensitive bit: a mode that must survive the copy.
chmod 0700 "$CI/docker"

rsync -aHAX --exclude=swapfile --exclude="HC_Volume_*" --exclude=lost+found "$CI/" "$VOL/" >/dev/null 2>&1 \
  && ok "rsync completed" || bad "rsync failed"

CHANGED=$(rsync -aHAX --dry-run --itemize-changes \
  --exclude=swapfile --exclude="HC_Volume_*" --exclude=lost+found "$CI/" "$VOL/" | grep -v '^\.' | head -5)
[ -z "$CHANGED" ] && ok "second pass transfers nothing (converged)" \
  || bad "convergence check false-positives on a good copy: $CHANGED"

[ -f "$VOL/bazel-disk-cache/blob" ] && ok "cache content copied" || bad "cache content missing"
[ ! -e "$VOL/swapfile" ] && ok "swapfile excluded (stays off the volume)" || bad "swapfile followed onto the volume"
[ ! -e "$VOL/HC_Volume_1" ] && ok "nested bad symlink not copied" || bad "nested symlink copied onto the volume"
[ "$(stat -c %a "$VOL/docker")" = "700" ] && ok "modes preserved" || bad "modes not preserved"

# The convergence check must actually FAIL on an incomplete copy, or it proves nothing.
rm -f "$VOL/bazel-disk-cache/blob"
CHANGED=$(rsync -aHAX --dry-run --itemize-changes \
  --exclude=swapfile --exclude="HC_Volume_*" --exclude=lost+found "$CI/" "$VOL/" | grep -v '^\.' | head -5)
[ -n "$CHANGED" ] && ok "convergence check detects an incomplete copy" \
  || bad "convergence check passes an INCOMPLETE copy -- it would greenlight data loss"

# Symlink swap, including the swapfile rename that must not need free space.
mv "$CI" "$T/mnt/ci-data.old" && ln -sfn "$VOL" "$CI"
[ -L "$CI" ] && ok "ci-data replaced by a symlink" || bad "ci-data is not a symlink"
[ -d "$CI/bazelwork" ] && ok "content reachable through the symlink" || bad "content unreachable"
mv "$T/mnt/ci-data.old/swapfile" "$T/swapfile-ci" && [ -f "$T/swapfile-ci" ] \
  && ok "swapfile relocated by rename (no allocation)" || bad "swapfile relocation failed"

[ "$fail" = 0 ] && echo "ALL OK" || { echo "FAILURES"; exit 1; }
