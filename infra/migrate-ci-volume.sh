#!/bin/bash
# Migrate a pool box's /mnt/ci-data from the root disk onto its attached (and
# currently unused) Hetzner volume, then replace it with a symlink to the volume.
#
# Kept in-tree as the runbook for this class of failure, not because it runs
# routinely: link-ci-volume.sh now aborts rather than nesting the link, and its
# abort message tells the operator to run exactly this. infra/migrate_ci_volume_test.sh
# exercises the data-moving core (rsync, the convergence guard, the symlink swap
# and the swapfile rename) against fixtures, including the negative case -- an
# incomplete copy MUST fail the convergence check, or the guard is decoration.
#
# Run it on a box whose slot is already out of rotation, one box at a time so
# fleet capacity never drops below four.
#
# Provisioning attached the volumes AFTER cloud-init's mount wait expired, so
# cloud-init's `ln -sfn "$VOL" /mnt/ci-data` ran when /mnt/ci-data already
# existed as a real directory -- and ln put the link INSIDE it
# (/mnt/ci-data/HC_Volume_<id> -> /mnt/HC_Volume_<id>). All CI work, Docker's
# data-root and the bazel caches have been running on the 75G root disk ever
# since, with the 100G volume sitting empty.
#
# Run with the box's slot already out of rotation. Idempotent enough to re-run
# after a failure: the rsync resumes and nothing is deleted until verification.
set -euo pipefail

VOL=$(ls -d /mnt/HC_Volume_* 2>/dev/null | head -1)
[ -n "$VOL" ] || { echo "FATAL: no /mnt/HC_Volume_* present"; exit 1; }
mountpoint -q "$VOL" || { echo "FATAL: $VOL is not a mountpoint"; exit 1; }

echo "== volume: $VOL"

if [ -L /mnt/ci-data ]; then
  echo "== /mnt/ci-data is already a symlink -> $(readlink /mnt/ci-data); nothing to migrate"
  exit 0
fi
[ -d /mnt/ci-data ] || { echo "FATAL: /mnt/ci-data is neither dir nor symlink"; exit 1; }

# Refuse to run while a job is on the box.
if pgrep -f "Runner.Worker|Runner.Listener" >/dev/null 2>&1; then
  echo "FATAL: a runner is active on this box; migrate only when its slot is idle"
  exit 1
fi

echo "== BEFORE"
df -h / "$VOL" | tail -3

# The extra 8G swapfile under ci-data is runtime-only (not in fstab) and must not
# follow the data onto network-attached storage: swap on a Hetzner volume turns
# memory pressure into IO stalls. Take it offline here; a root-disk replacement
# is armed at the end, once the migration has freed the space for it.
SWAP_SIZE=""
if swapon --show=NAME --noheadings | grep -qx /mnt/ci-data/swapfile; then
  SWAP_SIZE=$(stat -c %s /mnt/ci-data/swapfile)
  echo "== swapoff /mnt/ci-data/swapfile ($SWAP_SIZE bytes)"
  swapoff /mnt/ci-data/swapfile
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
# The swapfile and the nested bad symlink are deliberately left behind.
echo "== rsync ci-data -> volume"
rsync -aHAX --numeric-ids --info=progress2 \
  --exclude=swapfile \
  --exclude="HC_Volume_*" \
  --exclude=lost+found \
  /mnt/ci-data/ "$VOL/"

# A converged second pass emits only "." lines (no update needed). Anything with
# a transfer/create code in column 1 means the copy is incomplete, and swapping
# the symlink over an incomplete copy would lose data.
echo "== verify: second pass must transfer nothing"
CHANGED=$(rsync -aHAX --numeric-ids --dry-run --itemize-changes \
  --exclude=swapfile --exclude="HC_Volume_*" --exclude=lost+found \
  /mnt/ci-data/ "$VOL/" | grep -v '^\.' | head -20 || true)
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

echo "== swapping /mnt/ci-data -> symlink"
mv /mnt/ci-data /mnt/ci-data.old
ln -sfn "$VOL" /mnt/ci-data

# Prove the contract rather than assume it.
[ -L /mnt/ci-data ] || { echo "FATAL: /mnt/ci-data is not a symlink"; exit 1; }
[ "$(readlink /mnt/ci-data)" = "$VOL" ] || { echo "FATAL: symlink points at $(readlink /mnt/ci-data)"; exit 1; }
for d in bazel bazel-disk-cache bazel-repo-cache bazelwork docker; do
  [ -d "/mnt/ci-data/$d" ] || { echo "FATAL: /mnt/ci-data/$d missing after swap"; exit 1; }
done
sudo -u runner test -r /mnt/ci-data || { echo "FATAL: runner cannot read /mnt/ci-data"; exit 1; }
sudo -u runner ls /mnt/ci-data >/dev/null || { echo "FATAL: runner cannot LIST /mnt/ci-data"; exit 1; }
sudo -u runner test -w /mnt/ci-data/bazelwork || { echo "FATAL: runner cannot write bazelwork"; exit 1; }

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
if [ -n "$SWAP_SIZE" ] && [ ! -f /swapfile-ci ]; then
  echo "== relocating CI swapfile to root (/swapfile-ci)"
  mv /mnt/ci-data.old/swapfile /swapfile-ci
  chmod 600 /swapfile-ci
  swapon -p -3 /swapfile-ci
fi

echo "== AFTER (ci-data.old still present; delete after a job verifies)"
df -h / "$VOL" | tail -3
swapon --show
echo "== MIGRATION OK"
