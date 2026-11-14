#!/usr/bin/env python3
"""Seal an already-built measured executable and deployment tar; never build/run it."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import tempfile

import paired_builds as builds


def sealed(path):
    path = Path(path).absolute()
    return {"path": str(path), "sha256": builds._sha(path)}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--executable-asset", required=True, choices=builds.EXECUTABLES)
    parser.add_argument("--binary", required=True)
    parser.add_argument("--source-root", required=True)
    parser.add_argument("--source-revision", required=True)
    parser.add_argument("--source-manifest", required=True)
    parser.add_argument("--developer-home", required=True)
    parser.add_argument("--build-command-file", required=True, help="JSON argv array for the exact completed build")
    parser.add_argument("--build-exit-code", required=True, type=int, choices=(0,), help="recorded exit status of that build")
    parser.add_argument("--rc-file", action="append", default=[], help="repeat in effective rc-set order, from --announce_rc")
    parser.add_argument("--deployment", required=True, help="binary + runfiles tar, materialized in original relative layout")
    parser.add_argument("--executable-relative-path", required=True)
    parser.add_argument("--client-library-relative-path")
    parser.add_argument("--client-library", help="pinned source artifact whose bytes must match the native Go runfile")
    parser.add_argument("--output", required=True)
    args = parser.parse_args(argv)
    name = args.executable_asset
    binary, deployment = sealed(args.binary), sealed(args.deployment)
    source = {"root": str(Path(args.source_root).absolute()), "revision": args.source_revision,
              "manifest": str(Path(args.source_manifest).absolute()), "manifest_sha256": sealed(args.source_manifest)["sha256"]}
    command = json.loads(Path(args.build_command_file).read_text())
    receipt = {"schema": "otel_paired_build_receipt_v1", "executable_asset": name,
               "binary_sha256": binary["sha256"], "source_revision": source["revision"],
               "source_manifest_sha256": source["manifest_sha256"], "build_command": command,
               "developer_home": str(Path(args.developer_home).absolute()), "rc_set": [sealed(path) for path in args.rc_file],
               "build_exit_code": args.build_exit_code, "readelf_command": ["readelf", "-d", binary["path"]],
               "readelf_output": builds.readelf_dynamic(binary["path"]), "readelf_exit_code": 0,
               "deployment_asset": builds.deployment_asset(name), "deployment_sha256": deployment["sha256"],
               "executable_relative_path": args.executable_relative_path,
               "client_library_relative_path": args.client_library_relative_path,
               "files": builds.archive_manifest(deployment["path"])}
    raw = (json.dumps(receipt, sort_keys=True, allow_nan=False) + "\n").encode()
    output = Path(args.output).absolute()
    if output.exists() or output.is_symlink():
        raise ValueError("exclusive build receipt output already exists")
    # Validate before publishing. The temporary reference has exactly the same
    # bytes/hash as the final receipt and is never an executable input itself.
    with tempfile.TemporaryDirectory(prefix=".build-receipt-", dir=output.parent) as temporary:
        candidate = Path(temporary) / "receipt.json"
        candidate.write_bytes(raw)
        os.chmod(candidate, 0o600)
        assets = {name: {**binary, "build_receipt": sealed(candidate)}, builds.deployment_asset(name): deployment}
        if args.client_library:
            assets["libfdb_c"] = sealed(args.client_library)
        arm = dict(zip(builds.EXECUTABLES, ("go_pure", "go_native", "rust_native")))[name]
        plan = {"assets": assets, "sources": {arm: source}, "fake_provider": False}
        builds.validate(plan, name)
        fd = os.open(output, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, "wb") as stream:
            stream.write(raw)
            stream.flush()
            os.fsync(stream.fileno())
    value = {name: {**binary, "build_receipt": {"path": str(output), "sha256": hashlib.sha256(raw).hexdigest()}},
             builds.deployment_asset(name): deployment}
    print(json.dumps(value, sort_keys=True))
    return value


if __name__ == "__main__":
    main()
