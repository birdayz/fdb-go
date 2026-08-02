#!/bin/bash
# EXECUTES infra/migrate-ci-volume.sh against fixtures, so the production run is not the
# first time this logic runs.
#
# It used to only *paraphrase* the script: it re-typed the rsync invocations inline, with
# different flags (no --numeric-ids, no --delete), and asserted on its own copies. That
# made it worthless in the precise sense -- deleting migrate-ci-volume.sh outright left
# this suite printing ALL OK. Every case below drives the real script through its ROOT
# parameter, so a behaviour that leaves the script also leaves the suite.
#
# The kernel- and daemon-facing calls (systemctl, docker, swapon, pgrep, sudo) are stubbed
# on PATH: they cannot run here and they are not what this suite is about. rsync is REAL --
# it is the data-moving core, and the convergence guard is only meaningful against real
# transfer output.
#
# Run: bash infra/migrate_ci_volume_test.sh
set -uo pipefail
cd "$(dirname "$0")/.."
SCRIPT=$PWD/infra/migrate-ci-volume.sh

T=$(mktemp -d); BIN=$(mktemp -d); trap 'rm -rf "$T" "$BIN"' EXIT
fail=0
ok(){ echo "ok: $1"; }; bad(){ echo "FAIL: $1"; fail=1; }

[ -r "$SCRIPT" ] || { echo "FATAL: $SCRIPT is missing"; exit 1; }
command -v rsync >/dev/null || { echo "FATAL: rsync is required to test the data-moving core"; exit 1; }

# --- stubs for everything that needs a real box ---------------------------------------
mk() { cat > "$BIN/$1"; chmod +x "$BIN/$1"; }
# Nothing is running: no runner, no dockerd, no containers.
mk pgrep     <<'S'
#!/bin/sh
exit 1
S
mk systemctl <<'S'
#!/bin/sh
exit 0
S
mk sleep     <<'S'
#!/bin/sh
exit 0
S
mk docker    <<'S'
#!/bin/sh
case "$1" in
  info) echo "Docker Root Dir: /mnt/ci-data/docker" ;;
esac
exit 0
S
# Report the CI swapfile as active so the swapoff + relocate path is exercised.
mk swapon    <<'S'
#!/bin/sh
case "$*" in
  *--show=NAME*) echo "$FIXTURE_CI/swapfile" ;;
esac
exit 0
S
mk swapoff   <<'S'
#!/bin/sh
exit 0
S
# `sudo -u runner CMD ...` -> run CMD as ourselves, so the read/list/write assertions on
# the migrated tree are really performed.
mk sudo      <<'S'
#!/bin/sh
[ "$1" = "-u" ] && shift 2
exec "$@"
S

# A box mid-incident: /mnt/ci-data is a real root-disk directory holding all the caches,
# with the nested bad symlink and the runtime swapfile, and an empty attached volume.
fixture() {
  local R=$T/$1
  rm -rf "$R"
  mkdir -p "$R"/mnt/ci-data/{bazel,bazel-disk-cache,bazel-repo-cache,bazelwork,docker} "$R/mnt/HC_Volume_1"
  echo cache > "$R/mnt/ci-data/bazel-disk-cache/blob"
  echo work  > "$R/mnt/ci-data/bazelwork/w"
  dd if=/dev/zero of="$R/mnt/ci-data/swapfile" bs=1k count=64 status=none
  ln -s "$R/mnt/HC_Volume_1" "$R/mnt/ci-data/HC_Volume_1"   # the nested bad symlink
  mkdir -p "$R/mnt/HC_Volume_1/lost+found"
  chmod 0700 "$R/mnt/ci-data/docker"                        # a mode that must survive
  echo "$R"
}
migrate() {
  FIXTURE_CI="$1/mnt/ci-data" PATH="$BIN:$PATH" bash "$SCRIPT" "$1" >"$T/out" 2>&1
}

# --- 1. the happy path ------------------------------------------------------------------
R=$(fixture a)
VOL=$R/mnt/HC_Volume_1
# A stale file the source no longer has. --delete must remove it: content the source
# does not have must not survive on the target as a mystery.
mkdir -p "$VOL/bazel"; echo stale > "$VOL/bazel/stale-from-a-failed-attempt"
if migrate "$R"; then ok "migration succeeds"; else bad "migration failed: $(cat "$T/out")"; fi
grep -q "MIGRATION OK" "$T/out" && ok "reports MIGRATION OK" || bad "did not report MIGRATION OK"

[ -f "$VOL/bazel-disk-cache/blob" ] && ok "cache content copied" || bad "cache content missing"
[ ! -e "$VOL/swapfile" ] && ok "swapfile excluded (stays off the volume)" \
  || bad "swapfile followed onto the volume -- swap on network storage turns pressure into IO stalls"
[ ! -e "$VOL/HC_Volume_1" ] && ok "nested bad symlink not copied" || bad "nested symlink copied onto the volume"
[ ! -e "$VOL/bazel/stale-from-a-failed-attempt" ] && ok "stale target content deleted (--delete)" \
  || bad "stale file survived on the volume; --delete is not in effect"
[ "$(stat -c %a "$VOL/docker")" = "700" ] && ok "modes preserved" || bad "modes not preserved"
[ "$(stat -c %a "$VOL")" = "755" ] && ok "volume root forced to 0755" \
  || bad "volume root is $(stat -c %a "$VOL"); a non-listable ancestor breaks every runner"

[ -L "$R/mnt/ci-data" ] && ok "ci-data replaced by a symlink" || bad "ci-data is not a symlink"
[ -d "$R/mnt/ci-data/bazelwork" ] && ok "content reachable through the symlink" || bad "content unreachable"
[ -d "$R/mnt/ci-data.old" ] && ok "old tree kept for verification" || bad "old tree deleted before verification"
[ -f "$R/swapfile-ci" ] && ok "swapfile relocated to the root disk by rename" \
  || bad "swapfile not relocated"
[ ! -e "$R/mnt/ci-data.old/swapfile" ] && ok "swapfile moved, not copied (needs no free space)" \
  || bad "swapfile still in the old tree"

# --- 2. re-running a migrated box is a no-op -------------------------------------------
if migrate "$R"; then ok "re-run on a migrated box succeeds"; else bad "re-run failed: $(cat "$T/out")"; fi
grep -q "nothing to migrate" "$T/out" && ok "re-run detects the symlink and stops" \
  || bad "re-run did not short-circuit: $(cat "$T/out")"

# --- 3. the convergence guard must FAIL an incomplete copy ------------------------------
# Sabotage only the real transfer (not the --dry-run verify pass) by dropping one file, so
# the script's own guard sees a genuinely incomplete copy. Without the guard the script
# would swap the symlink over it and the missing data would be gone with ci-data.old.
# Resolve the real rsync first: the shim must exec that, not find itself on PATH.
REAL_RSYNC=$(command -v rsync)
mk rsync <<S
#!/bin/sh
for a in "\$@"; do
  [ "\$a" = "--dry-run" ] && exec "$REAL_RSYNC" "\$@"
done
exec "$REAL_RSYNC" --exclude=blob "\$@"
S
R=$(fixture b)
if migrate "$R"; then
  bad "an INCOMPLETE copy was accepted -- the convergence guard would greenlight data loss"
else
  ok "convergence guard rejects an incomplete copy"
fi
grep -q "not converged" "$T/out" && ok "abort names the convergence failure" \
  || bad "abort does not name the problem: $(cat "$T/out")"
[ ! -L "$R/mnt/ci-data" ] && ok "symlink NOT swapped over the incomplete copy" \
  || bad "symlink swapped over an incomplete copy -- this is the data-loss path"
[ -f "$R/mnt/ci-data/bazel-disk-cache/blob" ] && ok "source data untouched after the abort" \
  || bad "aborted run destroyed source data"
rm -f "$BIN/rsync"

[ "$fail" = 0 ] && echo "ALL OK" || { echo "FAILURES"; exit 1; }
