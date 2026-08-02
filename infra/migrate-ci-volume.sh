#!/bin/bash
# Migrate a pool box's /mnt/ci-data from the root disk onto its attached (and
# currently unused) Hetzner volume, then replace it with a symlink to the volume.
#
# Kept in-tree as the runbook for this class of failure, not because it runs
# routinely: link-ci-volume.sh now aborts rather than nesting the link, and its
# abort message tells the operator to run exactly this. infra/migrate_ci_volume_test.sh
# EXECUTES this script against fixtures -- rsync, the convergence guard, the symlink
# swap and the swapfile rename -- including the negative case: an incomplete copy MUST
# fail the convergence check, or the guard is decoration.
#
# usage: migrate-ci-volume.sh [ROOT]
# ROOT defaults to "" (the real filesystem); the test passes a fixture prefix. Every
# path below is relative to it, and the operations that need a real kernel (mountpoint,
# mount) are skipped under a fixture. That prefix is the whole reason the test can run
# the shipped code: the previous test re-typed the rsync invocations inline instead --
# with DIFFERENT flags, missing --numeric-ids and --delete -- and stayed green with this
# file deleted outright.
#
# Run it on a box whose slot is already out of rotation, one box at a time so
# fleet capacity never drops below four. Safe to re-run after a failure: the
# rsync resumes and nothing is deleted until the swap has been verified.
#
# How the fleet got here: provisioning attached the volumes AFTER cloud-init's
# mount wait expired, so `ln -sfn "$VOL" /mnt/ci-data` ran when /mnt/ci-data
# already existed as a real directory -- and ln put the link INSIDE it
# (/mnt/ci-data/HC_Volume_<id> -> /mnt/HC_Volume_<id>). All CI work, Docker's
# data-root and the bazel caches ran on the 75G root disk (measured at 91-97%)
# while each 100G volume sat at 1%.
set -euo pipefail

R="${1:-}"
CI="$R/mnt/ci-data"

VOL=$(ls -d "$R"/mnt/HC_Volume_* 2>/dev/null | head -1)
[ -n "$VOL" ] || { echo "FATAL: no $R/mnt/HC_Volume_* present"; exit 1; }
[ -n "$R" ] || mountpoint -q "$VOL" || { echo "FATAL: $VOL is not a mountpoint"; exit 1; }

echo "== volume: $VOL"

if [ -L "$CI" ]; then
  echo "== $CI is already a symlink -> $(readlink "$CI"); nothing to migrate"
  exit 0
fi
[ -d "$CI" ] || { echo "FATAL: $CI is neither dir nor symlink"; exit 1; }

# Refuse to run unless the box is QUIESCENT, which is a stronger condition than
# "no runner". A canceled job tears down Runner.Worker but leaves the bazel test
# processes it spawned running as orphans, still writing into
# /mnt/ci-data/bazel/_race_output_base -- measured live: the runner-process check
# passed, the copy started, and the convergence guard then caught the tree moving
# underneath it. Checking for writers directly is the honest test; the runner
# check alone is a proxy that a cancellation invalidates.
if pgrep -f "Runner[.]Worker|Runner[.]Listener" >/dev/null 2>&1; then
  echo "FATAL: a runner is active on this box; migrate only when its slot is idle"
  exit 1
fi
WRITERS=$(ls -l "$R"/proc/*/cwd 2>/dev/null | grep -c "ci-data" || true)
if [ "${WRITERS:-0}" != "0" ]; then
  echo "FATAL: $WRITERS process(es) still working inside $CI (orphans from a"
  echo "       canceled job survive their runner). Let them finish or kill them first."
  exit 1
fi
if [ "$(docker ps -q 2>/dev/null | wc -l)" != "0" ]; then
  echo "FATAL: docker containers are still running; the box is not idle"
  exit 1
fi

echo "== BEFORE"
df -h "$R/" "$VOL" | tail -3

# The extra 8G swapfile under ci-data is runtime-only (not in fstab) and must not
# follow the data onto network-attached storage: swap on a Hetzner volume turns
# memory pressure into IO stalls. Take it offline here; a root-disk replacement
# is armed at the end, once the migration has freed the space for it.
SWAP_SIZE=""
if swapon --show=NAME --noheadings | grep -qx "$CI/swapfile"; then
  SWAP_SIZE=$(stat -c %s "$CI/swapfile")
  echo "== swapoff $CI/swapfile ($SWAP_SIZE bytes)"
  swapoff "$CI/swapfile"
fi

echo "== stopping docker"
systemctl stop docker.service docker.socket || true
sleep 2
if pgrep -x dockerd >/dev/null; then
  echo "FATAL: dockerd still running after stop"
  exit 1
fi

# -aHAX --numeric-ids: hardlinks, ACLs and xattrs matter for Docker's overlayfs
# layers; numeric ids keep ownership exact without relying on name lookup.
# --delete: the volume is not guaranteed empty (a failed earlier attempt, or a
# stale tree from a previous box), and content the source no longer has must not
# survive on the target as a mystery.
# The swapfile and the nested bad symlink are deliberately left behind.
echo "== rsync ci-data -> volume"
rsync -aHAX --numeric-ids --delete --exclude=swapfile \
  --exclude="HC_Volume_*" \
  --exclude=lost+found \
  "$CI/" "$VOL/"

# A converged second pass emits only "." lines (no update needed). Anything with
# a transfer/create code in column 1 means the copy is incomplete, and swapping
# the symlink over an incomplete copy would lose data.
echo "== verify: second pass must transfer nothing"
CHANGED=$(rsync -aHAX --numeric-ids --dry-run --itemize-changes \
  --exclude=swapfile --exclude="HC_Volume_*" --exclude=lost+found \
  "$CI/" "$VOL/" | grep -v '^\.' | head -20 || true)
if [ -n "$CHANGED" ]; then
  echo "FATAL: rsync not converged, outstanding transfers:"
  echo "$CHANGED"
  exit 1
fi

# Ownership/mode contract: 0755 on the volume root so every ancestor of the
# runner's work directory is enumerable. actions/runner validates its work dir by
# walking the ancestor chain and LISTING each level; a traverse-only 0711 ancestor
# makes it refuse to start ("Fail to create and validate runner's work
# directory"), which is exactly how this fleet lost four of five slots.
chmod 0755 "$VOL"

echo "== swapping $CI -> symlink"
mv "$CI" "$CI.old"
ln -sfn "$VOL" "$CI"

# Prove the contract rather than assume it.
[ -L "$CI" ] || { echo "FATAL: $CI is not a symlink"; exit 1; }
[ "$(readlink "$CI")" = "$VOL" ] || { echo "FATAL: symlink points at $(readlink "$CI")"; exit 1; }
for d in bazel bazel-disk-cache bazel-repo-cache bazelwork docker; do
  [ -d "$CI/$d" ] || { echo "FATAL: $CI/$d missing after swap"; exit 1; }
done
sudo -u runner test -r "$CI" || { echo "FATAL: runner cannot read $CI"; exit 1; }
sudo -u runner ls "$CI" >/dev/null || { echo "FATAL: runner cannot LIST $CI"; exit 1; }
sudo -u runner test -w "$CI/bazelwork" || { echo "FATAL: runner cannot write bazelwork"; exit 1; }

echo "== starting docker"
systemctl start docker.service
sleep 3
docker info >/dev/null 2>&1 || { echo "FATAL: docker did not come back"; exit 1; }
ROOT=$(docker info 2>/dev/null | awk -F': ' '/Docker Root Dir/{print $2}')
echo "== docker root: $ROOT"
echo "== docker images:"
docker images --format '{{.Repository}}:{{.Tag}} {{.Size}}' | head -10

# Restore the swap headroom on the ROOT disk (never on the volume: swap on a
# network-attached volume turns memory pressure into IO stalls).
#
# RENAME the existing swapfile rather than allocating a new one. ci-data.old is
# still on the root disk at this point, so the freed space has NOT materialised
# yet -- a fallocate here would try to claim 8GB on a filesystem measured at 94%
# with 4.7GB free, and fail. A rename within the same filesystem costs nothing
# and preserves the already-formatted swap area, so no mkswap is needed either.
if [ -n "$SWAP_SIZE" ] && [ ! -f "$R/swapfile-ci" ]; then
  echo "== relocating CI swapfile to root ($R/swapfile-ci)"
  mv "$CI.old/swapfile" "$R/swapfile-ci"
  chmod 600 "$R/swapfile-ci"
  swapon -p -3 "$R/swapfile-ci"
fi

echo "== AFTER ($CI.old still present; delete after a job verifies)"
df -h "$R/" "$VOL" | tail -3
swapon --show
echo "== MIGRATION OK"
