#!/bin/bash
# Tests /usr/local/bin/link-ci-volume.sh as it is actually shipped: the script is
# EXTRACTED from cloud-init.yaml's write_files entry and executed against
# fixtures, so this cannot drift from what the boxes run.
#
# What it pins is one incident. Provisioning attached the pool's data volumes
# after cloud-init's mount wait expired, so /mnt/ci-data already existed as a
# plain root-disk directory when the old `ln -sfn "$VOL" /mnt/ci-data` ran --
# and ln puts the link INSIDE an existing directory. The result
# (/mnt/ci-data/HC_Volume_<id> -> /mnt/HC_Volume_<id>) is silent and looks
# right in a casual `ls`, so four boxes ran Docker's data-root and every bazel
# cache on a 75GB root disk at 91-94% while their 100GB volumes sat at 1%.
#
# Run: bash infra/link_ci_volume_test.sh
set -uo pipefail
cd "$(dirname "$0")/.."

SCRIPT=$(mktemp) ; T=$(mktemp -d)
trap 'rm -rf "$SCRIPT" "$T"' EXIT

# Extract the script body from the write_files entry (8-space indented block
# under `- path: /usr/local/bin/link-ci-volume.sh`).
node -e '
const fs = require("fs");
const lines = fs.readFileSync("infra/cloud-init.yaml", "utf8").split("\n");
const at = lines.findIndex(l => l.trim() === "- path: /usr/local/bin/link-ci-volume.sh");
if (at < 0) { console.error("write_files entry not found"); process.exit(1); }
const start = lines.findIndex((l, i) => i > at && l.trim() === "content: |");
if (start < 0) { console.error("content block not found"); process.exit(1); }
const out = [];
for (let i = start + 1; i < lines.length; i++) {
  const l = lines[i];
  if (l.trim() !== "" && !l.startsWith("      ")) break;
  out.push(l.slice(6));
}
// cloud-init.yaml is a Terraform template, so a shell expansion the template must not
// interpolate is written escaped ($${VOL:-}) and reaches the box as ${VOL:-}. Apply the
// same unescaping templatefile does, or this test runs a script that differs from the
// deployed one in exactly the characters that decide whether it works.
fs.writeFileSync(process.argv[1], out.join("\n").replace(/\$\$\{/g, "${").replace(/%%\{/g, "%{"));
' "$SCRIPT"
[ -s "$SCRIPT" ] || { echo "FATAL: extracted an empty script"; exit 1; }
# Sanity-check the EXTRACTION only, against things every version of the script
# has. Asserting on the guard's own text here would make a removed guard look
# like a broken extractor instead of failing the behavioural checks below.
grep -q '^#!/bin/bash' "$SCRIPT" || { echo "FATAL: extraction looks wrong (no shebang)"; exit 1; }
grep -q 'HC_Volume' "$SCRIPT" || { echo "FATAL: extraction looks wrong (no volume glob)"; exit 1; }
chmod +x "$SCRIPT"

fail=0
ok()   { echo "ok: $1"; }
bad()  { echo "FAIL: $1"; fail=1; }
fresh() { rm -rf "$T/$1"; mkdir -p "$T/$1/mnt/HC_Volume_999"; echo "$T/$1"; }
run()  { "$SCRIPT" "$1" >/dev/null 2>&1; }

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

[ "$fail" = 0 ] && echo "ALL OK" || { echo "FAILURES"; exit 1; }
