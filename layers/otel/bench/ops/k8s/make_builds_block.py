#!/usr/bin/env python3
"""Turn final-builds/<rev>/{control-pure,control-native,otel-rust}/ (executable + dereferenced .runfiles) into the config `builds` block.
Makes one deployment tar per executable (same layout, modes kept, no symlinks), hashes everything, writes the block into the given configs."""
import hashlib, json, subprocess, sys
from pathlib import Path
H = Path(__file__).resolve().parent
F = Path(sys.argv[1])
configs = sys.argv[2:]
def sha(p):
    h = hashlib.sha256()
    with open(p, "rb") as f:
        for c in iter(lambda: f.read(1 << 20), b""):
            h.update(c)
    return h.hexdigest()
note = (F / "build-note.txt").read_text()
rev = note.split("commit=")[1].split()[0]
assert len(rev) == 40
out = H / "assets" / ("builds-" + rev[:9]); out.mkdir(parents=True, exist_ok=True)
names = {"control_pure_binary": "control-pure", "control_native_binary": "control-native", "rust_binary": "otel-rust"}
block = {}
for key, d in names.items():
    base = F / d
    assert not any(p.is_symlink() for p in base.rglob("*")), "symlinks left in " + d
    tar = out / (key + ".tar")
    subprocess.run(["tar", "--mode=u+rwX,go+rX,go-w", "-C", str(base), "-cf", str(tar), "."], check=True)
    exe = d
    entry = {"sha256": sha(base / exe), "deployment_tar": str(tar), "deployment_sha256": sha(tar), "executable_relative_path": "./" + exe if False else exe,
             "client_library_relative_path": None}
    if key == "control_native_binary":
        libs = list(base.rglob("libfdb_c.so"))
        assert len(libs) == 1 and sha(libs[0]) == "617572467897d0360f76ffc33f24b58090ee691e3273750d2ff334cc71aae3fa"
        entry["client_library_relative_path"] = str(libs[0].relative_to(base))
    if key == "control_pure_binary":
        entry["local_executable"] = str(base / exe)
    block[key] = entry
manifest_sha = hashlib.sha256(note.encode()).hexdigest()
for c in configs:
    cfg = json.load(open(c))
    cfg["builds"] = block
    cfg["source_revision"] = rev
    cfg["source_manifest_sha256"] = manifest_sha
    json.dump(cfg, open(c, "w"), indent=1)
    print("updated", c)
print(json.dumps({k: {x: (v[x][:12] if isinstance(v[x], str) and len(v[x]) == 64 else v[x]) for x in ("sha256", "deployment_sha256", "executable_relative_path")} for k, v in block.items()}, indent=1))
print("source_revision", rev, "manifest attestation = sha256(build-note.txt)", manifest_sha[:12])
