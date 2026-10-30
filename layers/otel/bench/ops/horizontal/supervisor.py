#!/usr/bin/env python3
"""One sealed USER-manager cleanup watch; prepare never activates a unit."""
import argparse
import base64
import hashlib
import json
import math
import os
from pathlib import Path
import re
import sys
import time

import driver as d
import policy as p

SCHEMA = "otel_native_managed_watch_v1"
RUNTIME_UNIT = "bruh-web.service"
HERE = Path(__file__).resolve().parent
REPO = HERE.parents[4] if len(HERE.parents) > 4 else HERE  # Remote producer imports do not use controller paths.
FILES = tuple("layers/otel/bench/ops/horizontal/" + name for name in
              ("supervisor.py", "provision.py", "policy.py", "driver.py", "diagnostic.py", "session.py", "scaling.py")) + tuple(
              "tools/benchcampaign/multinode/" + name for name in ("inventory.py", "mn_common.py", "render_cloud_init.py", "teardown_guard.py"))
PROPERTIES = ("Id", "LoadState", "ActiveState", "SubState", "FragmentPath", "DropInPaths", "MainPID", "ControlGroup",
              "InvocationID", "PartOf", "BindsTo", "Requires", "Requisite", "Conflicts", "StopWhenUnneeded", "KillMode",
              "RuntimeMaxUSec", "ExecMainStartTimestampMonotonic", "ActiveEnterTimestampMonotonic")


def validate_config(plan):
    p.require(type(plan) is dict and plan.get("schema") == "otel_native_scaling_v1", "managed watch is scaling-only")
    value = plan.get("managed_watch")
    p.require(type(value) is dict and set(value) == {"root", "output"}, "exact managed_watch root/output required")
    for name in ("root", "output"):
        path = value[name]
        p.require(type(path) is str and re.fullmatch(r"/[A-Za-z0-9_./-]+", path) and ".." not in Path(path).parts
                  and str(Path(path)) == path and not Path(path).is_relative_to(REPO), "private absolute managed watch path")
    root, output = map(Path, (value["root"], value["output"]))
    p.require(not root.is_relative_to(output) and not output.is_relative_to(root), "watch bundle and run must not overlap")
    p.require(type(plan.get("campaign")) is str and re.fullmatch(r"[a-z][a-z0-9-]{7,47}", plan["campaign"]), "watch campaign identity")
    return value


def _path(path):
    path = Path(path)
    p.require(path.is_absolute() and ".." not in path.parts and not any(x.is_symlink() for x in (path, *path.parents)), "managed watch symlink/path refused")
    return path


def _read(path):
    path = _path(path)
    p.require(path.is_file() and path.stat().st_size <= 1024 ** 2 and path.stat().st_uid == os.getuid()
              and path.stat().st_mode & 0o077 == 0, "private bounded managed watch receipt")
    value = d.loads(path.read_text())
    p.require(type(value) is dict, "managed watch receipt must be an object")
    return value


def _write(path, value, *, exclusive=False):
    path = _path(path)
    if exclusive:
        p.require(not path.exists(), "managed watch registration already exists; no lease/controller replacement")
    raw = (json.dumps(value, sort_keys=True, allow_nan=False) + "\n").encode()
    p.require(len(raw) <= 1024 ** 2, "managed watch receipt bound")
    temp = path.with_name(path.name + "." + str(os.getpid()) + ".tmp")
    with open(temp, "xb") as stream:
        os.chmod(temp, 0o600); stream.write(raw); stream.flush(); os.fsync(stream.fileno())
    os.replace(temp, path)


def _unit(plan):
    return "otel-native-watch-" + plan["campaign"] + ".service"


def _seal(manifest):
    values = {key: value for key, value in manifest.items() if key not in ("seal", "template_sha256")}
    return hashlib.sha256(json.dumps(values, sort_keys=True).encode()).hexdigest()


def _argv(manifest, root, mode):
    return [manifest["python"], "-B", str(root / "bundle/layers/otel/bench/ops/horizontal/supervisor.py"), mode, "--root", str(root), "--seal", manifest["seal"]]


def _template(plan, manifest, root):
    return ("[Unit]\nDescription=Bounded owned native cleanup watch\nDefaultDependencies=no\nStopWhenUnneeded=no\n\n[Service]\nType=exec\n"
            "Slice=-.slice\nKillMode=control-group\nTimeoutStopSec=5s\nRestart=on-failure\nRestartPreventExitStatus=77\nRestartSec=1s\nRuntimeMaxSec=1s\n"
            "Environment=" + json.dumps("PATH=" + manifest["path"]) + "\nExecStartPre=" + " ".join(_argv(manifest, root, "bound"))
            + "\nExecStart=" + " ".join(_argv(manifest, root, "watch")) + "\n")


def prepare(plan_path, *, clock=time.time):
    import scaling
    plan_path = _path(plan_path)
    plan = scaling.validate_plan(d.read(plan_path))
    p.verify_source(plan)
    value = validate_config(plan)
    p.require(plan["first_epoch"] <= clock() < plan["first_epoch"] + 4000, "watch preparation original admission window")
    root = _path(value["root"])
    root.mkdir(mode=0o700, parents=True, exist_ok=False)
    python = str(Path(sys.executable).resolve())
    path = os.environ.get("PATH", "/usr/bin:/bin")
    p.require(re.fullmatch(r"[A-Za-z0-9_./:-]+", path), "sealed executable search path")
    manifest = {"schema": SCHEMA, "plan_sha256": p.sha(plan_path), "approved_plan_path": str(plan_path),
                "source_root": str(REPO), "python": python, "python_sha256": p.sha(python), "path": path,
                "unit": _unit(plan), "unit_path": str(Path.home() / ".config/systemd/user" / _unit(plan)),
                "first_epoch": plan["first_epoch"], "deadline_epoch": plan["deadline_epoch"], "files": {}}
    for name in FILES:
        source, target = _path(REPO / name), _path(root / "bundle" / name)
        parent = root
        for part in target.parent.relative_to(root).parts:
            parent = parent / part
            parent.mkdir(mode=0o700, exist_ok=True)
        target.write_bytes(source.read_bytes()); os.chmod(target, 0o400)
        manifest["files"][name] = p.sha(source)
    (root / "plan.json").write_bytes(plan_path.read_bytes()); os.chmod(root / "plan.json", 0o400)
    manifest["seal"] = _seal(manifest)
    text = _template(plan, manifest, root)
    (root / manifest["unit"]).write_text(text); os.chmod(root / manifest["unit"], 0o400)
    manifest["template_sha256"] = p.sha(root / manifest["unit"])
    _write(root / "manifest.json", manifest, exclusive=True)
    os.chmod(root / "manifest.json", 0o400)
    return {"prepared": True, "activated": False, "operator_permission_required": True, "root": str(root), "unit": manifest["unit"],
            "plan_sha256": manifest["plan_sha256"], "boundary": "offline files only; no OS isolation or survival claim"}


def load_bundle(plan, *, check_source=True):
    root = _path(validate_config(plan)["root"])
    p.require(root.is_dir() and root.stat().st_uid == os.getuid() and root.stat().st_mode & 0o077 == 0, "private managed watch root")
    manifest = _read(root / "manifest.json")
    sealed = _read(root / "plan.json")
    p.require(manifest.get("schema") == SCHEMA and sealed == plan and json.dumps(sealed, sort_keys=True) == json.dumps(plan, sort_keys=True)
              and p.sha(root / "plan.json") == manifest.get("plan_sha256")
              and p.sha(_path(manifest["approved_plan_path"])) == manifest["plan_sha256"]
              and manifest.get("first_epoch") == plan["first_epoch"] and manifest.get("deadline_epoch") == plan["deadline_epoch"]
              and type(plan["first_epoch"]) is int and type(plan["deadline_epoch"]) is int
              and plan["deadline_epoch"] == plan["first_epoch"] + 7200, "immutable managed watch plan/absolute lease changed")
    p.require(manifest.get("seal") == _seal(manifest), "managed watch bootstrap seal changed")
    p.require(manifest.get("unit") == _unit(plan) and manifest.get("unit_path") == str(Path.home() / ".config/systemd/user" / _unit(plan))
              and manifest.get("python") == str(Path(sys.executable).resolve()) and p.sha(manifest["python"]) == manifest.get("python_sha256")
              and type(manifest.get("files")) is dict and set(manifest["files"]) == set(FILES), "sealed managed watch bundle identity")
    for name, digest in manifest["files"].items():
        p.require(p.hexstr(digest) and p.sha(_path(root / "bundle" / name)) == digest, "managed watch helper bundle changed")
        if check_source:
            p.require(p.sha(_path(Path(manifest["source_root"]) / name)) == digest, "managed watch source helper changed")
    p.require((root / manifest["unit"]).read_text() == _template(plan, manifest, root)
              and p.sha(_path(root / manifest["unit"])) == manifest.get("template_sha256"), "managed watch template changed")
    if check_source:
        for name in p.ASSETS:
            expected = plan["input_manifest_sha256" if name == "source_manifest" else name + "_sha256"]
            p.require(p.sha(_path(plan[name])) == expected, "managed watch sealed application asset changed")
        p.verify_source(plan)
    return manifest, root


def process_identity(pid, proc_root="/proc"):
    p.require(type(pid) is int and pid > 0, "positive managed watch PID")
    base = Path(proc_root); path = base / str(pid)
    raw = (path / "stat").read_text(); head, _, tail = raw.rpartition(") ")
    fields = tail.split()
    p.require(head.startswith(str(pid) + " (") and len(fields) >= 20, "managed watch process stat")
    argv = (path / "cmdline").read_bytes().rstrip(b"\0").decode().split("\0")
    cg = (path / "cgroup").read_text().splitlines()
    p.require(len(cg) == 1 and cg[0].startswith("0::/") and ".." not in Path(cg[0][3:]).parts, "unified process cgroup required")
    uid = [line.split()[1:] for line in (path / "status").read_text().splitlines() if line.startswith("Uid:")]
    p.require(len(uid) == 1 and len(uid[0]) == 4 and all(x == str(os.getuid()) for x in uid[0]), "managed watch process UID mismatch")
    result = {"pid": pid, "ppid": int(fields[1]), "pgid": int(fields[2]), "starttime_ticks": int(fields[19]),
              "argv": argv, "exe": os.readlink(path / "exe"), "cgroup": cg[0][3:], "uid": os.getuid(),
              "boot_id": (base / "sys/kernel/random/boot_id").read_text().strip()}
    p.require(all(p.integer(result[k], 1) for k in ("ppid", "pgid", "starttime_ticks")) and argv and argv[0]
              and result["exe"].startswith("/") and not result["exe"].endswith(" (deleted)")
              and re.fullmatch(r"[0-9a-f-]{36}", result["boot_id"]), "complete managed watch process identity")
    return result


def _command(args, runner=None):
    if runner:
        rc, stdout, stderr = runner(args, timeout=2)
        p.require(rc == 0 and type(stdout) is str and len(stdout.encode()) + len(stderr.encode()) <= 65536, "user manager command unavailable")
        return stdout
    import diagnostic
    value = diagnostic.bounded_output(args, 65536, timeout=2)
    p.require(value["exit"] == 0 and not value["truncated"] and not value["timed_out"], "user manager command unavailable")
    return base64.b64decode(value["raw_base64"]).decode()


def manager_unit(unit, *, runner=None):
    p.require(type(unit) is str and re.fullmatch(r"[a-z][a-z0-9-]*\.service", unit), "exact managed USER unit name")
    raw = _command(["/usr/bin/systemctl", "--user", "show", unit, *["--property=" + key for key in PROPERTIES]], runner)
    props = {}
    for line in raw.splitlines():
        key, sep, value = line.partition("=")
        p.require(sep and key in PROPERTIES and key not in props, "ambiguous user manager properties")
        props[key] = value
    p.require(set(props) == set(PROPERTIES) and props["Id"] == unit and props["LoadState"] == "loaded", "exact loaded USER unit required")
    return props


def _duration(value):
    if type(value) is str and value.isdecimal():
        return int(value)
    p.require(type(value) is str, "finite user unit duration")
    pieces = re.findall(r"([0-9]+(?:\.[0-9]+)?)(us|ms|s|min|h|d)", value)
    p.require(pieces and re.sub(r"([0-9]+(?:\.[0-9]+)?)(us|ms|s|min|h|d)|\s+", "", value) == "", "finite user unit duration")
    result = sum(float(number) * {"us": 1, "ms": 1000, "s": 1000000, "min": 60000000, "h": 3600000000, "d": 86400000000}[unit] for number, unit in pieces)
    p.require(math.isfinite(result) and result > 0 and result.is_integer(), "finite positive user unit duration")
    return int(result)


def _inside(child, parent):
    return Path(child).is_relative_to(Path(parent))


def _fragment(plan, manifest, root, props):
    p.require(props["FragmentPath"] == manifest["unit_path"] and _path(props["FragmentPath"]).read_text() == _template(plan, manifest, root)
              and props["StopWhenUnneeded"] == "no" and props["KillMode"] == "control-group"
              and props["Requires"] in ("", "-.slice")
              and all(props[key] == "" for key in ("PartOf", "BindsTo", "Requisite", "Conflicts")),
              "external watch fragment or shared kill dependencies changed")
    paths = props["DropInPaths"].split()
    p.require(len(paths) <= 1, "unknown external watch override")
    if paths:
        path = _path(paths[0])
        expected = Path("/run/user") / str(os.getuid()) / "systemd/user.control" / (manifest["unit"] + ".d")
        p.require(path.parent == expected and re.fullmatch(r"50-RuntimeMax(?:Sec|USec)\.conf", path.name), "foreign external watch override")
        lines = [line.strip() for line in path.read_text().splitlines() if line.strip() and not line.lstrip().startswith("#")]
        p.require(len(lines) == 2 and lines[0] == "[Service]" and lines[1].startswith("RuntimeMaxSec=")
                  and _duration(lines[1].split("=", 1)[1]) == _duration(props["RuntimeMaxUSec"]), "foreign external watch runtime override")
    return paths


def _manager(process, proc_root):
    value = process_identity(process["ppid"], proc_root)
    p.require(value["exe"] == str(Path("/usr/lib/systemd/systemd").resolve()) and "--user" in value["argv"]
              and value["cgroup"].endswith("/init.scope"), "USER-manager parent identity required")
    return value


def verify_unit(plan, *, runner=None, proc_root="/proc", clock=time.time):
    manifest, root = load_bundle(plan)
    p.require(plan["first_epoch"] <= clock() < plan["deadline_epoch"], "managed watch absolute lease expired/not started")
    props = manager_unit(manifest["unit"], runner=runner)
    _fragment(plan, manifest, root, props)
    p.require(props["ActiveState"] == "active" and props["SubState"] == "running" and p.hexstr(props["InvocationID"], 32), "external watch not live")
    watch = process_identity(int(props["MainPID"]), proc_root)
    p.require(watch["exe"] == manifest["python"] and watch["argv"] == _argv(manifest, root, "watch")
              and watch["cgroup"] == props["ControlGroup"] and watch["cgroup"].endswith("/" + manifest["unit"]), "external watch live PID/argv/cgroup mismatch")
    manager = _manager(watch, proc_root)
    p.require(Path(watch["cgroup"]).parent == Path(manager["cgroup"]).parent, "watch must be directly in the USER manager root slice")
    runtime_props = manager_unit(RUNTIME_UNIT, runner=runner)
    p.require(runtime_props["ActiveState"] == "active" and runtime_props["SubState"] == "running"
              and runtime_props["KillMode"] == "control-group" and p.hexstr(runtime_props["InvocationID"], 32), "exact runtime USER unit unavailable")
    runtime = process_identity(int(runtime_props["MainPID"]), proc_root)
    p.require(runtime["cgroup"] == runtime_props["ControlGroup"] and runtime["cgroup"].endswith("/" + RUNTIME_UNIT)
              and _manager(runtime, proc_root) == manager and runtime["boot_id"] == watch["boot_id"]
              and not _inside(watch["cgroup"], runtime["cgroup"]) and not _inside(runtime["cgroup"], watch["cgroup"])
              and watch["pgid"] != runtime["pgid"], "watch shares runtime failure unit/group or manager identity changed")
    bound_receipt = _read(root / "bound.json")
    maximum = _duration(props["RuntimeMaxUSec"])
    p.require(bound_receipt.get("plan_sha256") == manifest["plan_sha256"] and bound_receipt.get("boot_id") == watch["boot_id"]
              and bound_receipt.get("invocation_id") == props["InvocationID"] and bound_receipt.get("deadline_epoch") == plan["deadline_epoch"]
              and type(bound_receipt.get("seconds")) is int and 0 < maximum <= bound_receipt["seconds"] * 1000000
              and max(_duration(props["ExecMainStartTimestampMonotonic"]), _duration(props["ActiveEnterTimestampMonotonic"]))
                  + maximum <= bound_receipt["deadline_monotonic_us"],
              "USER unit RuntimeMax would renew/exceed the original absolute lease")
    return {"watch": watch, "manager": manager, "runtime": runtime, "unit_invocation": props["InvocationID"],
            "runtime_invocation": runtime_props["InvocationID"], "bound_sha256": p.sha(root / "bound.json"),
            "plan_sha256": manifest["plan_sha256"], "manifest_sha256": p.sha(root / "manifest.json")}


def _controller(manifest, root, plan, *, proc_root):
    registration = _read(root / "controller.json")
    p.require(registration.get("manifest_sha256") == p.sha(root / "manifest.json")
              and registration.get("plan_sha256") == manifest["plan_sha256"] and registration.get("output") == plan["managed_watch"]["output"]
              and registration.get("first_epoch") == plan["first_epoch"] and registration.get("deadline_epoch") == plan["deadline_epoch"], "controller immutable lease binding changed")
    actual = process_identity(registration["controller"]["pid"], proc_root)
    p.require(actual == registration["controller"], "registered controller disappeared/reused/changed")
    return registration


def admit(plan, out, *, runner=None, proc_root="/proc", clock=time.time):
    p.require(Path(out).resolve() == Path(validate_config(plan)["output"]) / "provision", "managed watch provision output mismatch")
    p.require(plan["first_epoch"] <= clock() < plan["first_epoch"] + 6000, "original active admission window expired")
    manifest, root = load_bundle(plan)
    proof = verify_unit(plan, runner=runner, proc_root=proc_root, clock=clock)
    registration = _controller(manifest, root, plan, proc_root=proc_root)
    controller = registration["controller"]
    p.require(_inside(controller["cgroup"], proof["runtime"]["cgroup"]) and not _inside(proof["watch"]["cgroup"], controller["cgroup"])
              and controller["pgid"] != proof["watch"]["pgid"] and controller["boot_id"] == proof["watch"]["boot_id"]
              and registration["runtime"] == proof["runtime"] and registration["runtime_invocation"] == proof["runtime_invocation"], "controller/runtime failure-unit identity changed")
    ack = _read(root / "heartbeat.json")
    p.require(ack.get("schema") == SCHEMA and ack.get("healthy") is True and ack.get("proof") == proof
              and ack.get("controller_sha256") == p.sha(root / "controller.json") and p.number(ack.get("epoch"))
              and 0 <= clock() - ack["epoch"] <= 3, "stale/forged/unknown external watch heartbeat")
    p.require(clock() < plan["first_epoch"] + 6000, "original active admission window expired")
    return {"admitted": True, "plan_sha256": manifest["plan_sha256"], "unit": manifest["unit"], "epoch": clock()}


def register(plan, out, *, runner=None, proc_root="/proc", clock=time.time, sleep=time.sleep):
    p.require(Path(out).resolve() == Path(validate_config(plan)["output"]), "managed watch controller output mismatch")
    manifest, root = load_bundle(plan)
    proof = verify_unit(plan, runner=runner, proc_root=proc_root, clock=clock)
    controller = process_identity(os.getpid(), proc_root)
    argv = controller["argv"]
    script = str(Path(manifest["source_root"]) / "layers/otel/bench/ops/horizontal/scaling.py")
    p.require(controller["exe"] == manifest["python"] and script in argv and argv[argv.index(script) + 1:] ==
              ["run", "--plan", manifest["approved_plan_path"], "--output", str(out), "--execute-approved"]
              and all(x in ("-u", "-B") for x in argv[1:argv.index(script)])
              and _inside(controller["cgroup"], proof["runtime"]["cgroup"]), "exact scaling controller command/runtime identity required")
    _write(root / "controller.json", {"manifest_sha256": p.sha(root / "manifest.json"), "plan_sha256": manifest["plan_sha256"], "output": str(out), "first_epoch": plan["first_epoch"],
           "deadline_epoch": plan["deadline_epoch"], "controller": controller, "runtime": proof["runtime"], "runtime_invocation": proof["runtime_invocation"]}, exclusive=True)
    until = min(clock() + 5, plan["first_epoch"] + 6000)
    while clock() < until:
        try:
            return admit(plan, Path(out) / "provision", runner=runner, proc_root=proc_root, clock=clock)
        except (OSError, KeyError, TypeError, ValueError):
            sleep(0.1)
    raise ValueError("external managed watch did not acknowledge registered controller")


def bound(root, *, runner=None, proc_root="/proc", clock=time.time):
    root = _path(root); plan = _read(root / "plan.json")
    manifest, root = load_bundle(plan, check_source=False)
    props = manager_unit(manifest["unit"], runner=runner)
    _fragment(plan, manifest, root, props)
    own = process_identity(os.getpid(), proc_root)
    manager = _manager(own, proc_root)
    p.require(own["argv"] == _argv(manifest, root, "bound") and own["exe"] == manifest["python"]
              and own["cgroup"] == props["ControlGroup"] and own["cgroup"].endswith("/" + manifest["unit"])
              and Path(own["cgroup"]).parent == Path(manager["cgroup"]).parent and p.hexstr(props["InvocationID"], 32),
              "bound may change only its own independent generated USER unit")
    now, mono = clock(), time.monotonic_ns() // 1000
    seconds = math.floor(plan["deadline_epoch"] - now) - 5
    p.require(plan["first_epoch"] <= now and seconds > 0, "original managed watch hard deadline expired")
    _command(["/usr/bin/systemctl", "--user", "set-property", "--runtime", manifest["unit"], "RuntimeMaxSec=" + str(seconds) + "s"], runner)
    _write(root / "bound.json", {"plan_sha256": manifest["plan_sha256"], "boot_id": own["boot_id"], "invocation_id": props["InvocationID"],
           "seconds": seconds, "epoch": now, "deadline_epoch": plan["deadline_epoch"], "deadline_monotonic_us": mono + math.floor((plan["deadline_epoch"] - now) * 1000000)})
    return {"bounded": True, "unit": manifest["unit"], "seconds": seconds, "deadline_epoch": plan["deadline_epoch"]}


def watch(root, *, runner=None, proc_root="/proc", clock=time.time, sleep=time.sleep, teardown=None):
    root = _path(root); plan = _read(root / "plan.json")
    # Cleanup uses the sealed bundle even when original app/source admission fails.
    manifest, root = load_bundle(plan, check_source=False)
    own = process_identity(os.getpid(), proc_root)
    p.require(own["argv"] == _argv(manifest, root, "watch") and own["exe"] == manifest["python"]
              and own["cgroup"].endswith("/" + manifest["unit"]), "watch may run only in its generated OS-managed unit")
    _manager(own, proc_root)
    out = Path(plan["managed_watch"]["output"]) / "provision"
    from importlib import import_module
    provision = import_module("provision")
    cleanup = teardown or provision.teardown
    reason = None
    while clock() < plan["deadline_epoch"]:
        if reason is None:
            try:
                proof = verify_unit(plan, runner=runner, proc_root=proc_root, clock=clock)
                controller_sha = None
                if (root / "controller.json").exists():
                    registration = _controller(manifest, root, plan, proc_root=proc_root)
                    p.require(registration["runtime"] == proof["runtime"] and registration["runtime_invocation"] == proof["runtime_invocation"], "runtime invocation changed")
                    controller_sha = p.sha(root / "controller.json")
                if clock() >= plan["first_epoch"] + 6600:
                    reason = "original absolute cleanup time"
                else:
                    _write(root / "heartbeat.json", {"schema": SCHEMA, "healthy": True, "proof": proof,
                           "controller_sha256": controller_sha, "epoch": clock()})
            except (OSError, KeyError, TypeError, ValueError) as error:
                reason = "controller/supervision lost or unknown: " + str(error)
        if reason is not None:
            _write(root / "heartbeat.json", {"schema": SCHEMA, "healthy": False, "epoch": clock(), "reason": reason})
            try:
                if (provision._state(out) / "baseline").is_dir():
                    receipt = cleanup(plan, out, clock=clock, sleep=sleep)
                    if receipt.get("passed") is True and receipt.get("within_deadline") is True:
                        _write(root / "exit.json", {"passed": True, "reason": reason, "deadline_epoch": plan["deadline_epoch"], "epoch": clock()})
                        return 0
                else:
                    # No mutation can precede a complete baseline. Still prove the
                    # approved inventory without ever adopting an existing resource.
                    from types import SimpleNamespace
                    verify = provision._inventory.verify
                    dest = root / ("inventory-" + str(time.time_ns()))
                    if verify(SimpleNamespace(inventory=plan["inventory_dir"], output_dir=str(dest), hcloud=plan["hcloud"])) == 0:
                        _write(root / "exit.json", {"passed": True, "reason": reason, "deadline_epoch": plan["deadline_epoch"], "epoch": clock()})
                        return 0
            except (OSError, KeyError, TypeError, ValueError, RuntimeError) as error:
                _write(root / "cleanup-error.json", {"error": str(error), "epoch": clock()})
        sleep(min(1, max(0, plan["deadline_epoch"] - clock())))
    _write(root / "exit.json", {"passed": False, "reason": reason or "original hard deadline expired", "deadline_epoch": plan["deadline_epoch"], "epoch": clock()})
    return 77  # Permanent failure: RestartPreventExitStatus must not create a fresh lifetime.


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("prepare", "verify", "bound", "watch"))
    parser.add_argument("--plan"); parser.add_argument("--root"); parser.add_argument("--seal")
    args = parser.parse_args(argv)
    if args.mode == "prepare":
        p.require(args.plan and not args.root, "prepare requires only an approved plan")
        print(json.dumps(prepare(args.plan), sort_keys=True)); return
    p.require(args.root and not args.plan, "managed watch operation requires only its sealed root")
    plan = _read(Path(args.root) / "plan.json")
    manifest, _ = load_bundle(plan, check_source=args.mode not in ("bound", "watch"))
    if args.mode in ("bound", "watch"):
        p.require(args.seal == manifest["seal"], "OS-managed unit bootstrap seal mismatch")
        if time.time() >= plan["deadline_epoch"]:
            raise SystemExit(77)  # RestartPreventExitStatus makes expiry permanent.
    if args.mode == "bound":
        print(json.dumps(bound(args.root), sort_keys=True))
    elif args.mode == "verify":
        plan = _read(Path(args.root) / "plan.json")
        print(json.dumps(admit(plan, Path(plan["managed_watch"]["output"]) / "provision"), sort_keys=True))
    else:
        raise SystemExit(watch(args.root))


if __name__ == "__main__":
    main()
