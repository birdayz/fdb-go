#!/usr/bin/env bash
# Local: assemble a frozen M1 campaign dir from a build dir and a config.
# Usage: m1-stage.sh BUILD_DIR CONFIG.json CAMPAIGN_DIR
# Fills binary_sha256 from the built binaries, generates plan/schedule, pins every input, tars it.
set -Eeuo pipefail
bd=$(cd "${1:?build dir}" && pwd); cfg=${2:?config}; cd_=${3:?campaign dir}
here=$(cd "$(dirname "$0")" && pwd)
[[ ! -e $cd_ ]] || { echo "$cd_ exists" >&2; exit 1; }
mkdir -p "$cd_"; cd_=$(cd "$cd_" && pwd)
cp -r "$bd/bin" "$bd/provenance" "$bd/binaries.sha256" "$cd_/"
cp "$here/m1.py" "$here/m1-run.sh" "$cd_/"
mkdir -p "$cd_/harness"
cp "$here"/m1-build-arms.sh "$here"/m1-make-combined.sh "$here"/m1-stage.sh "$here"/m1-bench-shim.patch \
   "$here"/m1-config.template.json "$here"/m1_selftest.py "$here"/M1-preregistration*.md "$cd_/harness/"
cp "$here"/combined-*.txt "$cd_/provenance/" 2>/dev/null || true
python3 - "$cfg" "$cd_" <<'PY'
import hashlib,json,sys
cfg=json.load(open(sys.argv[1])); d=sys.argv[2]
for a in cfg["arms"]:
    commit=open(f"{d}/provenance/{a['name']}/commit.txt").read().strip()
    assert commit==a["commit"], (a["name"],commit,a["commit"])
    a["binary_sha256"]=hashlib.sha256(open(f"{d}/{a['binary']}","rb").read()).hexdigest()
json.dump(cfg,open(d+"/config.json","w"),indent=1)
PY
python3 -B "$cd_/m1.py" plan "$cd_/config.json" "$cd_" | tee "$cd_/plan.stdout"
(cd "$cd_" && find . -type f ! -name inputs.sha256 ! -name plan.stdout -print0 | sort -z | xargs -0 sha256sum) > "$cd_/inputs.sha256"
tar -C "$(dirname "$cd_")" -czf "$cd_.tar.gz" "$(basename "$cd_")"
sha256sum "$cd_.tar.gz" | tee "$cd_.tar.gz.sha256"
sha256sum "$cd_/inputs.sha256"
