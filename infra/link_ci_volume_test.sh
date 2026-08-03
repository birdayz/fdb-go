#!/bin/bash
# Tests the provisioning code that decides where CI data lives, as it is actually
# shipped: both the link-ci-volume.sh write_files entry and the bootcmd Docker
# mask/unmask decision are EXTRACTED from cloud-init.yaml and executed against
# fixtures, so this cannot drift from what the boxes run.
#
# What it pins is three incidents.
#
# 1. Provisioning attached the pool's data volumes after cloud-init's mount wait
#    expired, so /mnt/ci-data already existed as a plain root-disk directory when
#    the old `ln -sfn "$VOL" /mnt/ci-data` ran -- and ln puts the link INSIDE an
#    existing directory. The result (/mnt/ci-data/HC_Volume_<id> ->
#    /mnt/HC_Volume_<id>) is silent and looks right in a casual `ls`, so four boxes
#    ran Docker's data-root and every bazel cache on a 75GB root disk at 91-94%
#    while their 100GB volumes sat at 1%.
#
# 2. The fstab entry that makes the mount survive a reboot was authored only on the
#    path where the box LOST the attach race. A box whose volume was already
#    automounted broke out of the wait loop before reaching that code and got no
#    fstab entry at all -- and this script is PER_INSTANCE, so nothing re-ran on
#    reboot. Those boxes came back with the volume unmounted and every cache
#    silently on the root disk again. That is why the mount cases below assert
#    fstab CONTENT on BOTH race outcomes, not just the losing one.
#
# 3. bootcmd is PER_ALWAYS and runcmd is PER_INSTANCE, so masking Docker in bootcmd
#    and unmasking it in runcmd leaves every boot after the first with no container
#    runtime, on a box that still registers a runner and claims jobs. The repair has a
#    trap in it, which is why the ordering assertions below exist: bootcmd runs in
#    cloud-init's network stage, Before=sysinit.target and NOT after local-fs.target, so
#    a volume check placed THERE runs before the fstab mount and reports "absent" on every
#    boot -- the same outage, fail-closed. The decision has to live in a unit ordered
#    After=local-fs.target.
#
# Run: bash infra/link_ci_volume_test.sh
set -uo pipefail
cd "$(dirname "$0")/.."

SCRIPT=$(mktemp) ; T=$(mktemp -d) ; BIN=$(mktemp -d)
trap 'rm -rf "$SCRIPT" "$T" "$BIN"' EXIT

# Extract a write_files script body (6-space-indented block under `content: |`).
# cloud-init.yaml is a Terraform template, so a shell expansion the template must not
# interpolate is written escaped ($${VOL:-}) and reaches the box as ${VOL:-}. Apply the
# same unescaping templatefile does, or this test runs a script that differs from the
# deployed one in exactly the characters that decide whether it works.
extract_file() {
  awk -v want="  - path: $1" '
    $0 == want { at = 1; next }
    at && !inb && $0 == "    content: |" { inb = 1; next }
    inb {
      if ($0 != "" && $0 !~ /^      /) exit
      print substr($0, 7)
    }
  ' infra/cloud-init.yaml | sed -e 's/\$\${/${/g' -e 's/%%{/%{/g'
}

extract_file /usr/local/bin/link-ci-volume.sh > "$SCRIPT"
[ -s "$SCRIPT" ] || { echo "FATAL: extracted an empty script"; exit 1; }
# Sanity-check the EXTRACTION only, against things every version of the script
# has. Asserting on the guard's own text here would make a removed guard look
# like a broken extractor instead of failing the behavioural checks below.
grep -q '^#!/bin/bash' "$SCRIPT" || { echo "FATAL: extraction looks wrong (no shebang)"; exit 1; }
grep -q 'HC_Volume' "$SCRIPT" || { echo "FATAL: extraction looks wrong (no volume glob)"; exit 1; }
chmod +x "$SCRIPT"

GATE=$(mktemp); trap 'rm -rf "$SCRIPT" "$T" "$BIN" "$GATE"' EXIT
extract_file /usr/local/bin/ci-docker-gate.sh > "$GATE"
[ -s "$GATE" ] || { echo "FATAL: extracted an empty ci-docker-gate.sh"; exit 1; }
chmod +x "$GATE"

# The single bootcmd item, unescaped the same way.
BOOTCMD=$(awk '
  $0 == "bootcmd:" { at = 1; next }
  at && $0 ~ /^  - / { sub(/^  - /, ""); print; exit }
' infra/cloud-init.yaml | sed -e "s/^'//" -e "s/'$//" -e 's/\$\${/${/g')
[ -n "$BOOTCMD" ] || { echo "FATAL: extracted an empty bootcmd"; exit 1; }

# systemctl is not runnable here and must not be: what matters is WHICH decision the
# bootcmd reaches, so record the call instead of performing it.
cat > "$BIN/systemctl" <<'STUB'
#!/bin/sh
echo "$*" >> "$SYSTEMCTL_LOG"
STUB
chmod +x "$BIN/systemctl"

fail=0
ok()   { echo "ok: $1"; }
bad()  { echo "FAIL: $1"; fail=1; }
# A fixture box: the volume is automounted at /mnt/HC_Volume_999 (the race outcome the
# majority of boxes actually get) with its by-id device node and a real /etc/fstab.
fresh() {
  rm -rf "$T/$1"
  mkdir -p "$T/$1/mnt/HC_Volume_999" "$T/$1/dev/disk/by-id" "$T/$1/etc"
  : > "$T/$1/dev/disk/by-id/scsi-0HC_Volume_999"
  : > "$T/$1/etc/fstab"
  echo "$T/$1"
}
run()  { "$SCRIPT" "$1" >/dev/null 2>&1; }
# The exact line a box must end up with. by-id, because /dev/sdX renumbers across boots
# and a renumbered `nofail` entry silently does not mount; nofail, because a missing
# volume must not drop the box to an emergency shell; x-systemd.before, because `nofail`
# is precisely what REMOVES the mount unit's Before=local-fs.target (systemd.mount(5):
# a local mount gains that dependency "unless one or more mount options among nofail
# [...] is set"). Without it the gate's After=local-fs.target is ordered against nothing
# and the reboot path can mask Docker on a box whose volume was merely still mounting.
FSTAB_LINE="/dev/disk/by-id/scsi-0HC_Volume_999 /mnt/HC_Volume_999 ext4 discard,nofail,defaults,x-systemd.before=ci-docker-gate.service 0 0"

# 1. Virgin box: no /mnt/ci-data yet -> the symlink is created.
R=$(fresh a)
run "$R" && [ -L "$R/mnt/ci-data" ] && ok "virgin box creates the symlink" \
  || bad "virgin box did not create the symlink"

# 2. Idempotence: cloud-init re-runs on reboot; a correct link must be a no-op.
run "$R" && [ "$(readlink "$R/mnt/ci-data")" = "$R/mnt/HC_Volume_999" ] \
  && ok "re-run is idempotent" || bad "re-run broke an already-correct link"

# 3. The pre-volume race: an EMPTY /mnt/ci-data is adopted, not nested into.
R=$(fresh b); mkdir -p "$R/mnt/ci-data"
run "$R" && [ -L "$R/mnt/ci-data" ] && ok "empty dir is replaced by the symlink" \
  || bad "empty dir was not adopted"

# 4. THE INCIDENT: a NON-EMPTY /mnt/ci-data on the root disk must abort loudly,
#    must not nest the link, and must not touch the existing data.
R=$(fresh c); mkdir -p "$R/mnt/ci-data/docker"
if run "$R"; then bad "non-empty dir did not abort"; else ok "non-empty dir aborts"; fi
[ -e "$R/mnt/ci-data/HC_Volume_999" ] && bad "nested the link inside ci-data (the original bug)" \
  || ok "no nested link created"
[ -d "$R/mnt/ci-data/docker" ] && ok "existing data left intact" \
  || bad "aborted run destroyed existing data"
# Capture first: the script exits non-zero here, and under pipefail a pipeline
# would report that exit rather than grep's verdict.
msg=$("$SCRIPT" "$R" 2>&1 || true)
case "$msg" in
  *NON-EMPTY*) ok "abort names the problem" ;;
  *) bad "abort message does not name the problem: $msg" ;;
esac

# 5. A link pointing somewhere else is a misconfiguration, not something to
#    silently rewrite underneath whatever is using it.
R=$(fresh d); ln -s "$R/mnt/elsewhere" "$R/mnt/ci-data"
if run "$R"; then bad "wrong symlink did not abort"; else ok "wrong symlink aborts"; fi

# 6. The volume root ends up 0755. A traverse-only (0711) ancestor makes
#    actions/runner refuse to start: it validates its work directory by walking
#    the ancestor chain and LISTING each level. That cost four of five slots.
R=$(fresh e); chmod 0711 "$R/mnt/HC_Volume_999"
run "$R"
perm=$(stat -c %a "$R/mnt/HC_Volume_999")
[ "$perm" = "755" ] && ok "volume root forced to 0755" \
  || bad "volume root left at $perm; a non-listable ancestor breaks every runner"

# 7. The subdirs Docker's data-root and bazel need exist afterwards.
[ -d "$R/mnt/ci-data/docker" ] && [ -d "$R/mnt/ci-data/bazel" ] \
  && ok "docker/bazel subdirs created" || bad "docker/bazel subdirs missing"

# --- The mount must survive a reboot, on BOTH outcomes of the attach race ---

# 8. Race WON by Hetzner's attach-time automount (the majority case: the volume is
#    already mounted when the wait loop first looks, so the loop breaks immediately).
#    Automount writes no fstab entry and this script does not re-run on reboot, so
#    without an entry the box comes back with the volume gone and every cache back on
#    the root disk, silently. This is the case that shipped broken.
R=$(fresh f)
run "$R"
if [ "$(cat "$R/etc/fstab")" = "$FSTAB_LINE" ]; then
  ok "already-mounted volume still gets its fstab entry"
else
  bad "already-mounted volume got fstab [$(cat "$R/etc/fstab")], want [$FSTAB_LINE]"
fi

# 9. Race LOST (volume attached but not yet mounted): only the by-id device exists, so
#    the loop has to discover it, create the mountpoint and author fstab itself.
R=$(fresh g); rm -rf "$R/mnt/HC_Volume_999"
run "$R"
if [ "$(cat "$R/etc/fstab")" = "$FSTAB_LINE" ]; then
  ok "self-discovered volume gets its fstab entry"
else
  bad "self-discovered volume got fstab [$(cat "$R/etc/fstab")], want [$FSTAB_LINE]"
fi

# 10. Idempotent: re-running must not accumulate duplicate fstab entries.
run "$R"; run "$R"
n=$(grep -c 'HC_Volume_999' "$R/etc/fstab")
[ "$n" = 1 ] && ok "fstab entry is written exactly once" \
  || bad "fstab has $n HC_Volume_999 entries after three runs"

# 11. An existing entry for the same mountpoint is left alone rather than duplicated
#     with different options (a second, conflicting line is how a box mounts the wrong
#     thing at boot).
R=$(fresh h); echo "/dev/sdb /mnt/HC_Volume_999 ext4 defaults 0 0" > "$R/etc/fstab"
run "$R"
[ "$(wc -l < "$R/etc/fstab")" = 1 ] && ok "pre-existing fstab entry not duplicated" \
  || bad "pre-existing fstab entry duplicated: $(cat "$R/etc/fstab")"

# 11b. THE MAJORITY CASE, and the one an empty-fstab fixture cannot express. Hetzner's
#      automount writes this entry itself, byte-for-byte in the by-id format the script
#      writes -- verified on a real boot: the entry was already there, so the write path
#      was skipped and the box came up with `discard,nofail,defaults` and no ordering at
#      all. The ordering option therefore has to be applied to whatever entry is present,
#      not only to the one this script authors. Other fstab lines must be untouched.
R=$(fresh i2)
{ echo "# comment"; echo "/dev/disk/by-uuid/deadbeef / ext4 defaults 0 1"
  echo "/dev/disk/by-id/scsi-0HC_Volume_999 /mnt/HC_Volume_999 ext4 discard,nofail,defaults 0 0"
  echo "/swapfile none swap sw 0 0"; } > "$R/etc/fstab"
run "$R"
if grep -q "^/dev/disk/by-id/scsi-0HC_Volume_999 /mnt/HC_Volume_999 ext4 discard,nofail,defaults,x-systemd.before=ci-docker-gate.service 0 0$" "$R/etc/fstab"; then
  ok "an entry Hetzner already wrote gains the gate ordering option"
else
  bad "Hetzner-authored entry left unordered: [$(grep HC_Volume_999 "$R/etc/fstab")]"
fi
[ "$(grep -c HC_Volume_999 "$R/etc/fstab")" = 1 ] && ok "upgrading in place does not duplicate the entry" \
  || bad "upgrade duplicated the entry: $(cat "$R/etc/fstab")"
grep -q "^/dev/disk/by-uuid/deadbeef / ext4 defaults 0 1$" "$R/etc/fstab" \
  && grep -q "^/swapfile none swap sw 0 0$" "$R/etc/fstab" \
  && grep -q "^# comment$" "$R/etc/fstab" \
  && ok "other fstab lines are untouched" || bad "rewrite damaged unrelated fstab lines: $(cat "$R/etc/fstab")"
run "$R"; run "$R"
[ "$(grep -c 'x-systemd.before' "$R/etc/fstab")" = 1 ] && ok "the option is added exactly once" \
  || bad "option accumulated: $(grep HC_Volume_999 "$R/etc/fstab")"

# 12. No by-id device for the mounted volume: the mount CANNOT be persisted, and a box
#     that provisions anyway is a box that silently reverts to the root disk on its next
#     reboot. Fail loud instead, the same way an unmounted volume already does.
R=$(fresh i); rm -f "$R/dev/disk/by-id/scsi-0HC_Volume_999"
msg=$("$SCRIPT" "$R" 2>&1 || true)
if run "$R"; then bad "missing by-id device did not abort"; else ok "missing by-id device aborts"; fi
case "$msg" in
  *unpersisted*) ok "abort explains the reboot consequence" ;;
  *) bad "abort message does not explain the consequence: $msg" ;;
esac

# --- the Docker gate: the mask/unmask decision, re-evaluated on EVERY boot ---

gate() {
  SYSTEMCTL_LOG="$T/systemctl.log"; : > "$SYSTEMCTL_LOG"
  SYSTEMCTL_LOG="$SYSTEMCTL_LOG" PATH="$BIN:$PATH" "$GATE" "$1"
  cat "$SYSTEMCTL_LOG"
}

# 13. bootcmd must mask UNCONDITIONALLY. It is the only hook before the CONFIG-stage
#     package install, and a conditional there would be evaluated before local-fs.target
#     has mounted anything -- see the header. So the volume must not appear in it.
case "$BOOTCMD" in
  "systemctl mask docker.service docker.socket") ok "bootcmd masks unconditionally" ;;
  *) bad "bootcmd is not the unconditional mask: $BOOTCMD" ;;
esac
case "$BOOTCMD" in
  *ci-data*|*HC_Volume*)
    bad "bootcmd inspects the CI volume; at bootcmd time the fstab mount has not happened, so this masks docker on EVERY boot" ;;
  *) ok "bootcmd does not depend on the volume being mounted" ;;
esac

# 14. The gate that re-decides must be ordered after the mounts, or it reads the same
#     pre-mount filesystem bootcmd would have and fails closed forever.
unit=$(awk '
  $0 == "  - path: /etc/systemd/system/ci-docker-gate.service" { at = 1; next }
  at && !inb && $0 == "    content: |" { inb = 1; next }
  inb { if ($0 != "" && $0 !~ /^      /) exit; print substr($0, 7) }
' infra/cloud-init.yaml)
case "$unit" in
  *"After=local-fs.target"*) ok "gate unit is ordered after local-fs.target" ;;
  *) bad "gate unit is not ordered after local-fs.target; it would run before the volume mounts" ;;
esac
case "$unit" in
  *"WantedBy=multi-user.target"*) ok "gate unit is wanted by multi-user.target" ;;
  *) bad "gate unit has no [Install] target, so enabling it does nothing" ;;
esac
grep -q 'systemctl enable ci-docker-gate.service' infra/cloud-init.yaml \
  && ok "gate unit is enabled during provisioning" \
  || bad "gate unit is never enabled, so it never runs on a later boot"

# 15. First boot: no /mnt/ci-data yet, so Docker must stay masked -- otherwise dockerd
#     creates its data-root on the root disk, the failure the mask exists to prevent.
R=$(fresh j); rm -rf "$R/mnt/ci-data"
case "$(gate "$R")" in
  *unmask*) bad "no volume yet, but the gate unmasked docker: $(gate "$R")" ;;
  *mask*) ok "no volume: gate masks docker" ;;
  *) bad "no volume: gate did not mask docker: $(gate "$R")" ;;
esac

# 16. Later boot with the volume mounted and linked: Docker must be UNMASKED and STARTED
#     here, because runcmd is PER_INSTANCE and will not run again. This is the case that
#     made every reboot produce a runner with no container runtime.
R=$(fresh k); ln -s "$R/mnt/HC_Volume_999" "$R/mnt/ci-data"; mkdir -p "$R/mnt/ci-data/docker"
out=$(gate "$R")
case "$out" in
  unmask*) ok "volume present: gate unmasks docker" ;;
  *) bad "volume present: gate did not unmask docker: $out" ;;
esac
case "$out" in
  *"start docker.service"*) ok "gate starts docker (unmasking alone leaves it stopped for the boot)" ;;
  *) bad "gate unmasked but never started docker, so the box has no runtime this boot: $out" ;;
esac

# 17. Later boot where the fstab mount failed: /mnt/ci-data dangles. Fail CLOSED --
#     an unmasked Docker here refills the root disk silently, which is worse than a
#     box that visibly cannot run containers.
R=$(fresh l); ln -s "$R/mnt/HC_Volume_missing" "$R/mnt/ci-data"
case "$(gate "$R")" in
  *unmask*) bad "dangling ci-data link, but the gate unmasked docker: $(gate "$R")" ;;
  *mask*) ok "dangling ci-data link: gate masks docker" ;;
  *) bad "dangling link did not mask docker: $(gate "$R")" ;;
esac

# 18. /mnt/ci-data as a real root-disk directory with a docker dir in it is the shape
#     of the original incident, not a healthy box. It must not unmask.
R=$(fresh m); mkdir -p "$R/mnt/ci-data/docker"
case "$(gate "$R")" in
  *unmask*) bad "root-disk ci-data dir unmasked docker: $(gate "$R")" ;;
  *mask*) ok "root-disk ci-data dir: gate masks docker" ;;
  *) bad "root-disk ci-data dir did not mask docker: $(gate "$R")" ;;
esac

# --- a box that cannot run containers must LEAVE the pool, not serve from it ---

# The gate is invoked directly here (not through gate()) because what is under test is its
# EXIT STATUS, which gate() discards in favour of the systemctl log.
run_gate() {
  SYSTEMCTL_LOG="$T/systemctl.log"; : > "$SYSTEMCTL_LOG"
  SYSTEMCTL_LOG="$SYSTEMCTL_LOG" PATH="$BIN:$PATH" "$GATE" "$1" >/dev/null 2>&1
}

# 19. The fail branch must EXIT NON-ZERO. That exit is the whole mechanism behind the
#     listener's Requires=ci-docker-gate.service: a gate that masks Docker and then
#     returns 0 is a SUCCESSFUL dependency, and the listener starts beside it anyway.
#     This fixture also has no watchdog config, so it covers the unset-WATCH_UNIT path.
R=$(fresh n); ln -s "$R/mnt/HC_Volume_missing" "$R/mnt/ci-data"
if run_gate "$R"; then
  bad "gate fail branch exited 0; Requires= reads that as a pass and the listener serves jobs on a box with masked Docker"
else
  ok "gate fail branch exits non-zero"
fi
case "$(cat "$T/systemctl.log")" in
  *"mask docker.service"*) ok "fail branch still masks docker when no WATCH_UNIT is configured" ;;
  *) bad "fail branch with no watchdog config did not mask docker" ;;
esac

# 20. ...and it takes the job listener down with it. Otherwise the box keeps claiming
#     hetzner-fdb-vm jobs it cannot run and fails them one after another, which is worse
#     for the fleet than one box visibly missing.
echo "WATCH_UNIT=actions.runner.acme-repo.box.service" > "$R/etc/runner-watchdog.conf"
run_gate "$R"
case "$(cat "$T/systemctl.log")" in
  *"stop actions.runner.acme-repo.box.service"*) ok "fail branch stops the job listener" ;;
  *) bad "fail branch left the listener running: $(cat "$T/systemctl.log")" ;;
esac

# 21. The healthy branch must still exit 0, or the Requires= added for 19 blocks the
#     listener on every boot instead of only the broken ones.
R=$(fresh o); ln -s "$R/mnt/HC_Volume_999" "$R/mnt/ci-data"; mkdir -p "$R/mnt/ci-data/docker"
run_gate "$R" && ok "gate healthy branch exits 0" \
  || bad "gate healthy branch exited non-zero; the listener's Requires= would never be satisfied"

# 22. svc.sh writes a unit that depends on nothing but network-online.target, so the
#     binding to the gate is a drop-in — and it must exist BEFORE the listener is started,
#     or first boot serves jobs ungated.
classic=$(sed -n '/if \[ "\$RUNNER_MODE" = "classic" \]/,/^    else$/p' infra/cloud-init.yaml)
[ -n "$classic" ] || { echo "FATAL: could not extract the classic runner_mode branch"; exit 1; }
case "$classic" in
  *"Requires=ci-docker-gate.service"*) ok "classic branch binds the listener to the gate" ;;
  *) bad "classic branch installs no Requires=ci-docker-gate.service drop-in" ;;
esac
dropin_at=$(printf '%s\n' "$classic" | grep -n 'ci-docker-gate.conf' | tail -1 | cut -d: -f1)
start_at=$(printf '%s\n' "$classic" | grep -n 'svc.sh start' | head -1 | cut -d: -f1)
if [ -n "$dropin_at" ] && [ -n "$start_at" ] && [ "$dropin_at" -lt "$start_at" ]; then
  ok "gate drop-in is installed before the listener is started"
else
  bad "listener started before the gate drop-in exists (drop-in $dropin_at, start $start_at)"
fi

# 23. Classic mode deliberately configures NO heartbeat arm, so the watchdog's only signal
#     is the unit being dead. The stock actions/runner publishes no heartbeat, and the one
#     local substitute — staleness of the listener's own _diag log — is silent for the
#     whole length of a job (measured on a live box: an IDLE healthy listener logs only on
#     credential refresh, ~30 min apart, while a 2h+ job produces nothing on that file at
#     all). Any threshold above the healthy idle gap is therefore also a threshold that
#     fires mid-job, which has already killed live CI twice. A heartbeat arm here needs a
#     job-in-progress guard landed with it.
#     Comments are stripped first: the branch documents the decision by name, and matching
#     the prose that explains it would make this pass for the wrong reason.
case "$(printf '%s\n' "$classic" | grep -v '^ *#')" in
  *WATCH_HEARTBEAT=*) bad "classic mode configures a heartbeat arm without a job-in-progress guard; the watchdog will restart a listener mid-job" ;;
  *) ok "classic mode configures no heartbeat arm (deliberate — see infra/README.md)" ;;
esac

[ "$fail" = 0 ] && echo "ALL OK" || { echo "FAILURES"; exit 1; }
