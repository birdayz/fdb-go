"""Measured executable provenance and sealed runfiles deployment (stdlib only).

This module never builds anything or changes Bazel's normal rc configuration.
The receipt is a sealed record of an already completed, isolated measured build.
"""
import bz2
import gzip
import hashlib
import inspect
import io
import json
import lzma
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import stat
import subprocess
import tarfile
import tempfile

EXECUTABLES = ("control_pure_binary", "control_native_binary", "rust_binary")
MAX_BYTES = 512 * 1024 ** 2
MAX_FILES = 10000
RECEIPT_FIELDS = frozenset((
    "schema", "executable_asset", "binary_sha256", "source_revision", "source_manifest_sha256",
    "build_command", "compilation_mode", "configured_output_path", "developer_home", "rc_set", "build_exit_code", "readelf_command",
    "readelf_output", "readelf_exit_code", "deployment_asset", "deployment_sha256",
    "executable_relative_path", "client_library_relative_path", "files"))


def _require(condition, message):
    if not condition:
        raise ValueError(message)


def _sha(path):
    digest = hashlib.sha256()
    with open(path, "rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def _hex(value, length=64):
    return type(value) is str and re.fullmatch("[a-f0-9]{%d}" % length, value) is not None


def _absolute(value):
    _require(type(value) is str and value.startswith("/") and "\x00" not in value
             and str(Path(value)) == value and ".." not in Path(value).parts,
             "normalized absolute path required")
    return Path(value)


def _relative(value):
    _require(type(value) is str and value and "\x00" not in value and "\\" not in value
             and not value.startswith("/") and str(PurePosixPath(value)) == value
             and all(part not in (".", "..") for part in value.split("/")),
             "normalized relative path required")
    return value


def _sealed(ref, check=True):
    _require(type(ref) is dict and set(ref) == {"path", "sha256"} and _hex(ref.get("sha256")),
             "exact sealed file reference required")
    path = _absolute(ref["path"])
    if check:
        try:
            _require(path.is_file() and not path.is_symlink() and _sha(path) == ref["sha256"],
                     "sealed file hash mismatch: " + str(path))
        except OSError as exc:
            raise ValueError("sealed file unavailable: " + str(path)) from exc
    return path


def _json(text):
    """Reject JSON ambiguity before receipt identity or policy checks."""
    def object_pairs(pairs):
        result = {}
        for key, value in pairs:
            _require(key not in result, "duplicate JSON object key")
            result[key] = value
        return result

    def constant(value):
        raise ValueError("nonfinite JSON constant: " + value)

    def floating(value):
        number = float(value)
        _require(-float("inf") < number < float("inf"), "nonfinite JSON number")
        return number

    return json.loads(text, object_pairs_hook=object_pairs, parse_constant=constant, parse_float=floating)


def _manifest(files):
    _require(type(files) is list and 1 <= len(files) <= MAX_FILES, "bounded deployment file manifest required")
    result, total = {}, 0
    for item in files:
        _require(type(item) is dict and set(item) == {"path", "sha256", "bytes", "mode"}, "exact deployment file fields required")
        path = _relative(item["path"])
        _require(path not in result and _hex(item["sha256"]), "duplicate file or invalid file hash")
        _require(type(item["bytes"]) is int and 0 <= item["bytes"] <= MAX_BYTES
                 and type(item["mode"]) is int and item["mode"] in (0o644, 0o755), "bounded file size and safe mode required")
        result[path] = dict(item)
        total += item["bytes"]
    _require(total <= MAX_BYTES, "deployment exceeds 512MiB")
    _require(list(result) == sorted(result), "deployment manifest must be sorted by path")
    for path in result:
        _require(not any(str(parent) in result for parent in PurePosixPath(path).parents if str(parent) != "."),
                 "file/directory manifest collision")
    return result


class _BoundedTarInput:
    """Bound decompressed headers too, including oversized PAX/long-name records."""
    def __init__(self, path):
        self.raw = open(path, "rb")
        magic = self.raw.read(6)
        self.raw.seek(0)
        if magic.startswith(b"\x1f\x8b"):
            self.stream = gzip.GzipFile(fileobj=self.raw)
        elif magic.startswith(b"BZh"):
            self.stream = bz2.BZ2File(self.raw)
        elif magic.startswith(b"\xfd7zXZ\x00"):
            self.stream = lzma.LZMAFile(self.raw)
        else:
            self.stream = self.raw
        self.remaining = MAX_BYTES + MAX_FILES * 4096 + 1024 * 1024

    def read(self, size):
        _require(type(size) is int and 0 <= size <= self.remaining,
                 "decompressed deployment archive exceeds bound")
        block = self.stream.read(size)
        self.remaining -= len(block)
        return block

    def __enter__(self):
        return self

    def __exit__(self, *args):
        try:
            self.stream.close()
        finally:
            self.raw.close()


def _bundle(archive, expected_sha, files=None, target=None, include_directories=False):
    """Validate every tar member; optionally write regular files, never extract links."""
    manifest = _manifest(files) if files is not None else None
    archive = _absolute(archive)
    _require(_hex(expected_sha), "sealed archive hash required")
    _require(archive.is_file() and not archive.is_symlink() and archive.stat().st_size <= MAX_BYTES,
             "bounded regular deployment archive required")
    _require(_sha(archive) == expected_sha, "deployment archive hash mismatch")
    seen, actual, directories, entries, total = set(), {}, set(), 0, 0
    try:
        with _BoundedTarInput(archive) as raw, tarfile.open(fileobj=raw, mode="r|", ignore_zeros=True) as bundle:
            for member in bundle:
                entries += 1
                _require(entries <= MAX_FILES * 2, "too many archive entries")
                name = member.name.rstrip("/") if member.isdir() else member.name
                _relative(name)
                _require(name not in seen, "duplicate archive entry")
                seen.add(name)
                _require(not member.issym() and not member.islnk() and not member.issparse(), "archive links/sparse files forbidden")
                if member.isdir():
                    _require(member.mode in (0o755, 0o644) and member.size == 0, "unsafe archive directory")
                    directories.add(name)
                    if target is not None:
                        (Path(target) / name).mkdir(mode=0o755, parents=True, exist_ok=True)
                    continue
                _require(member.isreg() and (manifest is None or name in manifest), "unexpected or special archive entry")
                total += member.size
                _require(member.size >= 0 and total <= MAX_BYTES and len(actual) < MAX_FILES
                         and member.mode in (0o755, 0o644), "bounded file size and safe mode required")
                item = manifest[name] if manifest is not None else {"path": name, "bytes": member.size, "mode": member.mode}
                _require(member.size == item["bytes"] and member.mode == item["mode"], "archive file size/mode mismatch")
                digest, count = hashlib.sha256(), 0
                output = None
                if target is not None:
                    dest = Path(target) / name
                    dest.parent.mkdir(mode=0o755, parents=True, exist_ok=True)
                    output = open(dest, "xb")
                try:
                    with bundle.extractfile(member) as stream:
                        for block in iter(lambda: stream.read(1024 * 1024), b""):
                            count += len(block)
                            _require(count <= item["bytes"], "archive file exceeds manifest")
                            digest.update(block)
                            if output is not None:
                                output.write(block)
                    _require(count == item["bytes"] and (manifest is None or digest.hexdigest() == item["sha256"]), "archive file hash mismatch")
                    item = {**item, "sha256": digest.hexdigest()}
                    if output is not None:
                        output.flush()
                        os.fchmod(output.fileno(), item["mode"])
                        os.fsync(output.fileno())
                finally:
                    if output is not None:
                        output.close()
                actual[name] = dict(item)
    except (tarfile.TarError, EOFError, OSError, lzma.LZMAError) as exc:
        raise ValueError("invalid deployment archive: " + str(exc)) from exc
    _require(manifest is None or set(actual) == set(manifest), "archive omitted manifest files")
    _require(not set(actual) & directories, "file/directory archive collision")
    _manifest([actual[path] for path in sorted(actual)])
    _require(not any(str(parent) in actual for directory in directories for parent in PurePosixPath(directory).parents),
             "file/directory archive collision")
    result = [actual[path] for path in sorted(actual)]
    return (result, directories) if include_directories else result


def archive_manifest(path):
    """Return a sorted regular-file manifest after safe bounded tar inspection."""
    path = _absolute(str(path))
    _require(path.is_file() and not path.is_symlink() and path.stat().st_size <= MAX_BYTES,
             "bounded regular deployment archive required")
    return _bundle(str(path), _sha(path))


def deployment_asset(name):
    _require(name in EXECUTABLES, "unknown executable asset")
    return name + "_deployment"


def readelf_dynamic(path):
    """Inspect the already-built artifact without inherited loader/tool variables."""
    result = subprocess.run(["readelf", "-d", str(_absolute(str(path)))],
                            env={"PATH": "/usr/bin:/bin", "LC_ALL": "C"},
                            capture_output=True, text=True, timeout=30, check=False)
    _require(result.returncode == 0, "readelf -d failed: " + result.stderr)
    return result.stdout


def _argv(value):
    return type(value) is list and 1 <= len(value) <= 256 and all(type(arg) is str and arg and "\x00" not in arg for arg in value)


def optimized_build(receipt):
    """Require explicit default-optimized target configuration, never infer it from bytes."""
    command = receipt["build_command"]
    _require(_argv(command) and "build" in command, "exact Bazel build argv required")
    options = command[command.index("build") + 1:]
    modes, index = [], 0
    while index < len(options):
        option = options[index]
        if option == "--":
            break
        if option in ("-c", "--compilation_mode"):
            _require(index + 1 < len(options), "explicit opt compilation mode required")
            index += 1
            modes.append(options[index])
        elif option.startswith("--compilation_mode="):
            modes.append(option.partition("=")[2])
        elif option.startswith("-c") and not option.startswith("--"):
            modes.append(option[2:])
        index += 1
    _require(receipt.get("compilation_mode") == "opt" and modes and all(mode == "opt" for mode in modes),
             "measured builds require explicit --compilation_mode=opt (or -c opt), without conflicting modes")
    configured = _relative(receipt["configured_output_path"])
    parts = PurePosixPath(configured).parts
    _require(len(parts) >= 4 and parts[0] == "bazel-out" and parts[2] == "bin"
             and re.fullmatch(r"[A-Za-z0-9_.+-]+-opt(?:-[A-Za-z0-9_.+]+)*", parts[1]) is not None
             and not {"fastbuild", "dbg"} & set(parts[1].split("-"))
             and "/".join(parts[3:]) == _relative(receipt["executable_relative_path"]),
             "configured output must be the preserved executable under a Bazel -opt configuration")
    tuning = re.compile(r"(?i)(?:(?:^|[^A-Za-z0-9_])(?:-?C)?(?:lto|flto|thin_lto|fat_lto|pgo|target[-_]cpu|codegen[-_]units|opt[-_]level|(?:[a-z]+_)?fdo[-_](?:optimize|instrument|profile)|fauto[-_]profile|fprofile[-_](?:generate|use|instr-generate|instr-use|sample-use))\b|-(?:march|mcpu)=)")
    level = re.compile(r"(?i)(?:^|[=,\s])-(?:O(?:[0-9]+|s|g|z|fast)?|N)(?=$|[=,\s])")
    _require(not any(tuning.search(option) or level.search(option) for option in options if not option.startswith(("//", "@"))),
             "toolchain-default opt policy forbids optimization-level, LTO, PGO, target-cpu and codegen-units overrides")
    return True


def _same_path(left, right):
    if left.resolve() == right.resolve():
        return True
    try:
        return left.samefile(right)
    except OSError:
        return False


def read_receipt(plan, name):
    """Read only the small hash-sealed receipt, without ELF or archive rescans."""
    deployment = deployment_asset(name)
    executable = plan["assets"][name]
    _require(type(executable) is dict and set(executable) == {"path", "sha256", "build_receipt"},
             "executable build_receipt reference required")
    path = _sealed(executable["build_receipt"])
    _require(path.stat().st_size <= 4 * 1024 ** 2, "bounded build receipt required")
    try:
        receipt = _json(path.read_text())
    except (OSError, ValueError) as exc:
        raise ValueError("invalid build receipt") from exc
    _require(type(receipt) is dict and set(receipt) == RECEIPT_FIELDS, "exact build receipt fields required")
    _require(receipt["schema"] == "otel_paired_build_receipt_v1" and receipt["executable_asset"] == name
             and receipt["binary_sha256"] == executable["sha256"] and _hex(receipt["binary_sha256"])
             and receipt["deployment_asset"] == deployment
             and receipt["deployment_sha256"] == plan["assets"][deployment]["sha256"]
             and _hex(receipt["deployment_sha256"]), "build binary/deployment identity mismatch")
    optimized_build(receipt)
    files = _manifest(receipt["files"])
    relative = _relative(receipt["executable_relative_path"])
    _require(relative in files and files[relative]["sha256"] == executable["sha256"]
             and files[relative]["mode"] == 0o755, "deployment must preserve sealed executable")
    library = receipt["client_library_relative_path"]
    if name == "control_native_binary":
        library = _relative(library)
        _require(PurePosixPath(library).name == "libfdb_c.so" and ":" not in str(PurePosixPath(library).parent)
                 and library in files and files[library]["sha256"] == plan["assets"]["libfdb_c"]["sha256"],
                 "canonical sealed native runfile required")
    else:
        _require(library is None, "only native Go has a client library runfile")
    return receipt


def validate(plan, name, check_assets=True, inspect_elf=True):
    """Return the sealed receipt after provenance, linkage and bundle admission."""
    deployment = deployment_asset(name)
    _require(inspect_elf or not check_assets or plan.get("fake_provider") is True,
             "paid build validation cannot skip live ELF inspection")
    executable = plan["assets"][name]
    _require(type(executable) is dict and set(executable) == {"path", "sha256", "build_receipt"},
             "executable build_receipt reference required")
    binary_path = _sealed({key: executable[key] for key in ("path", "sha256")}, check_assets)
    receipt_path = _sealed(executable["build_receipt"], check_assets)
    _require(receipt_path.stat().st_size <= 4 * 1024 ** 2, "bounded build receipt required")
    try:
        receipt = _json(receipt_path.read_text())
    except (OSError, ValueError) as exc:
        raise ValueError("invalid build receipt") from exc
    _require(type(receipt) is dict and set(receipt) == RECEIPT_FIELDS, "exact build receipt fields required")
    arm = dict(zip(EXECUTABLES, ("go_pure", "go_native", "rust_native")))[name]
    source = plan["sources"][arm]
    _require(receipt["schema"] == "otel_paired_build_receipt_v1" and receipt["executable_asset"] == name
             and receipt["binary_sha256"] == executable["sha256"]
             and _hex(receipt["source_revision"], 40) and receipt["source_revision"] == source["revision"]
             and receipt["source_manifest_sha256"] == source["manifest_sha256"], "build source/binary identity mismatch")
    _sealed({"path": source["manifest"], "sha256": source["manifest_sha256"]}, check_assets)
    command = receipt["build_command"]
    _require(_argv(command) and Path(command[0]).name in ("bazel", "bazelisk") and "build" in command,
             "exact Bazel build argv required")
    build_at = command.index("build")
    _require("--nohome_rc" in command[1:build_at]
             and not any(arg.startswith("--home_rc") or (arg.startswith("--nohome_rc") and arg != "--nohome_rc") for arg in command),
             "--nohome_rc startup option required before build; --home_rc forbidden")
    _require(len(command) >= 4, "build argv must include its explicit target/options")
    _require(type(receipt["build_exit_code"]) is int and receipt["build_exit_code"] == 0, "successful build required")
    home_rc = _absolute(receipt["developer_home"]) / ".bazelrc"
    root = _absolute(source["root"])
    rc_set = receipt["rc_set"]
    _require(type(rc_set) is list and len(rc_set) <= 1000, "effective rc_set required")
    rc_paths = []
    for ref in rc_set:
        path = _sealed(ref, check_assets)
        _require(not _same_path(path, home_rc) and not any(_same_path(path, old) for old in rc_paths),
                 "home rc or duplicate rc identity forbidden")
        rc_paths.append(path)
    repo_rc = root / ".bazelrc"
    if repo_rc.exists():
        _require(any(_same_path(repo_rc, path) for path in rc_paths), "repository .bazelrc must be in effective rc_set")
    for index, arg in enumerate(command):
        if arg == "--bazelrc" or arg.startswith("--bazelrc="):
            value = command[index + 1] if arg == "--bazelrc" and index + 1 < len(command) else arg.partition("=")[2]
            _require(bool(value), "missing explicit bazelrc path")
            path = Path(value) if Path(value).is_absolute() else root / value
            _require(not _same_path(path, home_rc), "explicit home .bazelrc forbidden")
            _require(any(_same_path(path, listed) for listed in rc_paths), "explicit bazelrc missing from effective rc_set")
    readelf = receipt["readelf_command"]
    _require(_argv(readelf) and len(readelf) == 3 and Path(readelf[0]).name == "readelf"
             and readelf[1:] == ["-d", str(binary_path)]
             and type(receipt["readelf_exit_code"]) is int and receipt["readelf_exit_code"] == 0,
             "successful exact readelf -d command required")
    dynamic = receipt["readelf_output"]
    _require(type(dynamic) is str and dynamic.strip() and len(dynamic) <= 1024 ** 2, "captured readelf output required")
    for line in dynamic.splitlines():
        _require(not (re.search(r"\((?:RUNPATH|RPATH)\)", line) and "/home" in line), "developer-home RPATH/RUNPATH forbidden")
    needed = re.findall(r"\(NEEDED\).*?Shared library:\s*\[([^\]]+)\]", dynamic)
    native = name == "control_native_binary"
    _require(("libfdb_c.so" in needed) if native else not any("libfdb_c" in value for value in needed),
             "wrong libfdb_c DT_NEEDED linkage")
    if check_assets and inspect_elf:
        _require(readelf_dynamic(binary_path) == dynamic, "live readelf output differs from sealed capture")
    bundle_ref = plan["assets"][deployment]
    bundle_path = _sealed(bundle_ref, check_assets)
    _require(receipt["deployment_asset"] == deployment and receipt["deployment_sha256"] == bundle_ref["sha256"],
             "deployment identity mismatch")
    optimized_build(receipt)
    files = _manifest(receipt["files"])
    relative = _relative(receipt["executable_relative_path"])
    _require(relative in files and files[relative]["sha256"] == executable["sha256"]
             and files[relative]["mode"] == 0o755, "deployment must preserve sealed executable")
    runfiles = relative + ".runfiles/"
    has_runfiles = any(path.startswith(runfiles) for path in files)
    library = receipt["client_library_relative_path"]
    if native:
        library = _relative(library)
        _require(PurePosixPath(library).name == "libfdb_c.so" and ":" not in str(PurePosixPath(library).parent)
                 and library in files and files[library]["sha256"] == plan["assets"]["libfdb_c"]["sha256"],
                 "canonical sealed native runfile required")
        _sealed(plan["assets"]["libfdb_c"], check_assets)
    else:
        _require(library is None, "only native Go has a client library runfile")
    if check_assets:
        _, directories = _bundle(str(bundle_path), bundle_ref["sha256"], receipt["files"], include_directories=True)
        _require(has_runfiles or runfiles.rstrip("/") in directories, "executable .runfiles tree required")
    return receipt


def install_bundle(request):
    """Install verified regular files exclusively, publishing the full tree at once."""
    _require(type(request) is dict and set(request) == {"archive", "sha256", "destination", "files"}, "exact install request required")
    destination = _absolute(request["destination"])
    _require(destination.parent.is_dir() and destination.parent.resolve() == destination.parent,
             "real existing deployment parent required")
    _require(not os.path.lexists(destination), "deployment destination already exists")
    temporary = Path(tempfile.mkdtemp(prefix="." + destination.name + "-", dir=destination.parent))
    reserved = False
    try:
        _manifest(request["files"])
        files = _bundle(request["archive"], request["sha256"], request["files"], temporary)
        # Re-read the installed bytes, not just the source tar streams.
        for item in files:
            path = temporary / item["path"]
            _require(_sha(path) == item["sha256"] and path.stat().st_size == item["bytes"]
                     and stat.S_IMODE(path.stat().st_mode) == item["mode"], "installed file verification failed")
        # mkdir is the exclusive claim: rename may replace only this empty owned
        # placeholder, never a destination created by another installer.
        try:
            destination.mkdir(mode=0o700)
        except FileExistsError as exc:
            raise ValueError("deployment destination already exists") from exc
        reserved = True
        os.rename(temporary, destination)
        reserved = False
        return {"schema": "otel_paired_deployment_receipt_v1", "archive": request["archive"], "sha256": request["sha256"],
                "destination": str(destination), "files": files}
    finally:
        if reserved:
            destination.rmdir()
        if temporary.exists():
            shutil.rmtree(temporary)


def fake_readelf(name):
    deployment_asset(name)
    if name == "control_native_binary":
        return "Dynamic section at offset 0x1000 contains 1 entry:\n 0x0000000000000001 (NEEDED) Shared library: [libfdb_c.so]\n"
    return "There is no dynamic section in this file.\n"


def fake_artifacts(name, executable_asset, source, library_asset):
    """Create tiny real sealed fixtures, not runnable executables or build claims."""
    deployment = deployment_asset(name)
    binary = Path(executable_asset["path"])
    relative = "bin/" + name
    configured = Path(source["root"]) / "bazel-out/k8-opt/bin" / relative
    configured.parent.mkdir(parents=True, exist_ok=True)
    configured.write_bytes(binary.read_bytes())
    payloads = {relative: (binary.read_bytes(), 0o755),
                relative + ".runfiles/_main/MANIFEST": (b"synthetic runfiles marker\n", 0o644)}
    library = None
    if name == "control_native_binary":
        library = relative + ".runfiles/_main/lib/libfdb_c.so"
        payloads[library] = (Path(library_asset["path"]).read_bytes(), 0o755)
    files = [{"path": path, "sha256": hashlib.sha256(data).hexdigest(), "bytes": len(data), "mode": mode}
             for path, (data, mode) in sorted(payloads.items())]
    archive = binary.with_name(binary.name + "-deployment.tar")
    with tarfile.open(archive, "w", format=tarfile.USTAR_FORMAT) as bundle:
        for item in files:
            member = tarfile.TarInfo(item["path"])
            member.size, member.mode = item["bytes"], item["mode"]
            bundle.addfile(member, io.BytesIO(payloads[item["path"]][0]))
    rc = Path(source["root"]) / ".bazelrc"
    receipt = {"schema": "otel_paired_build_receipt_v1", "executable_asset": name,
               "binary_sha256": executable_asset["sha256"], "source_revision": source["revision"],
               "source_manifest_sha256": source["manifest_sha256"],
               "build_command": ["bazelisk", "--nohome_rc", "build", "--compilation_mode=opt", "//synthetic:" + name],
               "compilation_mode": "opt", "configured_output_path": "bazel-out/k8-opt/bin/" + relative,
               "developer_home": str(binary.parent / "synthetic-developer-home"),
               "rc_set": [{"path": str(rc), "sha256": _sha(rc)}] if rc.exists() else [], "build_exit_code": 0,
               "readelf_command": ["readelf", "-d", str(binary)], "readelf_output": fake_readelf(name), "readelf_exit_code": 0,
               "deployment_asset": deployment, "deployment_sha256": _sha(archive),
               "executable_relative_path": relative, "client_library_relative_path": library, "files": files}
    path = binary.with_name(binary.name + "-build-receipt.json")
    path.write_text(json.dumps(receipt, sort_keys=True) + "\n")
    return {name: {**executable_asset, "build_receipt": {"path": str(path), "sha256": _sha(path)}},
            deployment: {"path": str(archive), "sha256": _sha(archive)}}


# Exactly the same extraction/installation code runs on a host with no checkout.
INSTALL_SCRIPT = ("import bz2,gzip,hashlib,json,lzma,os,re,shutil,stat,sys,tarfile,tempfile\n"
                  "from pathlib import Path,PurePosixPath\n"
                  "MAX_BYTES=512*1024**2\nMAX_FILES=10000\n" +
                  "\n".join(inspect.getsource(function) for function in
                            (_require, _sha, _hex, _absolute, _relative, _json, _manifest, _BoundedTarInput, _bundle, install_bundle)) +
                  "\nrequest_path=_absolute(sys.argv[1])\n"
                  "_require(request_path.is_file() and not request_path.is_symlink() and request_path.stat().st_size<=4*1024**2 and _sha(request_path)==sys.argv[2], 'sealed bounded install request required')\n"
                  "print(json.dumps(install_bundle(_json(request_path.read_text())),sort_keys=True))\n")
