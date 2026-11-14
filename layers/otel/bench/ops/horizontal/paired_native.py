#!/usr/bin/env python3
"""Live paired adapter and self-contained remote helper; import has no effects.

Only the controller imports provision/driver. Remote invocations require a sealed
exclusive grant, exact metadata identity and a single JSON-argument executable.
No producer capability is emulated: absent/failed control reports refuse admission.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
import copy
from decimal import Decimal
import hashlib
import json
import os
from pathlib import Path
import re
import resource
import selectors
import stat
import subprocess
import sys
import threading
import time
import urllib.request

MAX_ARTIFACT = 256 * 1024 ** 2
MAX_LOG = 16 * 1024 ** 2
GRANT_KEYS = {"schema", "node_id", "namespace", "job", "job_sha256", "argv", "inputs",
              "active_deadline", "hard_deadline", "limits", "scope"}


def require(condition, message):
    if not condition:
        raise ValueError(message)


def loads(text):
    def pairs(items):
        value = {}
        for key, item in items:
            require(key not in value, "duplicate JSON field")
            value[key] = item
        return value
    def constant(_):
        raise ValueError("nonfinite JSON")
    def floating(text):
        value = float(text)
        require(abs(value) < float("inf"), "nonfinite JSON")
        return value
    return json.loads(text, object_pairs_hook=pairs, parse_constant=constant, parse_float=floating)


def encoded(value):
    return (json.dumps(value, sort_keys=True, allow_nan=False) + "\n").encode()


def sha(path):
    h = hashlib.sha256()
    with open(path, "rb") as stream:
        for block in iter(lambda: stream.read(1024 ** 2), b""):
            h.update(block)
    return h.hexdigest()


def clean(path):
    require(type(path) is str and path.startswith("/") and str(Path(path)) == path
            and ".." not in Path(path).parts and "\x00" not in path, "unclean absolute path")
    # Reject symlink ancestors, not just symlink leaf files.
    for parent in (Path(path), *Path(path).parents):
        require(not parent.is_symlink(), "symlink path refused")
    return Path(path)


def read(path, limit=MAX_ARTIFACT):
    path = clean(str(path))
    info = path.stat()
    require(stat.S_ISREG(info.st_mode) and info.st_nlink == 1 and info.st_size <= limit,
            "artifact type/link/size refused")
    return loads(path.read_text())


def write(path, value):
    path = clean(str(path))
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "wb") as stream:
        stream.write(encoded(value))
        stream.flush()
        os.fsync(stream.fileno())


def identity(node_id):
    require(type(node_id) is int and node_id > 0, "exact provider node ID required")
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open("http://169.254.169.254/hetzner/v1/metadata/instance-id", timeout=2) as body:
        actual = body.read(128).decode().strip()
    require(actual == str(node_id), "remote metadata ID mismatch")


def load_grant(path, *, active=True, check_inputs=True):
    path = clean(str(path))
    g = read(path, 1024 ** 2)
    require(type(g) is dict and set(g) == GRANT_KEYS and g["schema"] == "otel_paired_grant_v1", "exact typed grant required")
    identity(g["node_id"])
    job = g["job"]
    require(type(job) is dict and job.get("schema") == "otel_paired_job_v1"
            and job.get("mode") in ("setup", "ingest", "verify")
            and job.get("arm") in ("go_pure", "go_native", "rust_native"), "typed job required")
    require(type(g["namespace"]) is str and re.fullmatch(r"[a-z][a-z0-9-]{7,47}/pairs[123]-(go_pure|go_native|rust_native)-rep[12]/(prerequisite|measured)/client[123]", g["namespace"]), "owned namespace required")
    root = "/srv/otel-paired/" + g["namespace"]
    mode = job["mode"]
    require(str(path) == root + "/" + mode + "-grant.json" and job.get("output") == root + "/" + mode + ".json", "exclusive grant/output binding")
    job_path = root + "/" + ("job.json" if mode == "ingest" else mode + "-job.json")
    require(g["argv"] == [g["argv"][0], job_path] and len(g["argv"]) == 2, "one JSON argv only")
    require(g["job_sha256"] == hashlib.sha256(encoded(job)).hexdigest() and read(job_path) == job
            and sha(job_path) == g["job_sha256"], "job bytes/grant mismatch")
    require(g["scope"] == "otel-paired-" + g["job_sha256"][:24] + ".scope", "scope not bound to job")
    limits = g["limits"]
    require(type(limits) is dict and set(limits) == {"allowed_cpus", "cpu_quota_percent", "memory_max_mib", "swap_max_bytes"}
            and limits["allowed_cpus"] == "0-3" and type(limits["cpu_quota_percent"]) is int
            and limits["cpu_quota_percent"] == 400 and type(limits["memory_max_mib"]) is int
            and 1 <= limits["memory_max_mib"] <= 56 * 1024 and type(limits["swap_max_bytes"]) is int
            and limits["swap_max_bytes"] == 0, "unchanged client resource caps required")
    require(type(g["active_deadline"]) is int and type(g["hard_deadline"]) is int
            and g["active_deadline"] < g["hard_deadline"], "absolute grant deadlines required")
    if active:
        require(time.time() < g["active_deadline"], "original active deadline expired")
    inputs = g["inputs"]
    require(type(inputs) is dict and inputs and inputs.get(g["argv"][0]) == job.get("binary_sha256"), "sealed executable required")
    base = "/srv/otel-horizontal/" + g["namespace"].split("/")[0] + "/bin/"
    executable = "rust_binary" if mode == "ingest" and job["arm"] == "rust_native" else "go_binary"
    require(g["argv"][0] == base + executable, "Go control required outside Rust ingestion")
    for prefix in ("cluster", "corpus") + (("layout",) if mode != "setup" else ()):
        require(inputs.get(job.get(prefix + "_file")) == job.get(prefix + "_sha256"), "missing input seal: " + prefix)
    if job.get("client_library") is not None:
        require(job.get("client_library_sha256") == "617572467897d0360f76ffc33f24b58090ee691e3273750d2ff334cc71aae3fa"
                and inputs.get(job["client_library"]) == job["client_library_sha256"], "pinned native library required")
    outputs = [job["output"]]
    if mode == "setup":
        outputs.append(job["layout_file"])
    if mode == "verify":
        outputs.append(job["verification"]["query_results_file"])
    require(len(set(outputs)) == len(outputs) and not set(outputs) & set(inputs), "input/output alias")
    for output in outputs:
        clean(output)
        if active:
            require(not Path(output).exists(), "output already exists; no replay")
    for file, expected in inputs.items():
        clean(file)
        require(type(expected) is str and re.fullmatch("[a-f0-9]{64}", expected), "invalid input seal")
        if check_inputs:
            info = Path(file).stat()
            require(stat.S_ISREG(info.st_mode) and info.st_nlink == 1 and sha(file) == expected, "remote input SHA mismatch")
    return g


def ready_receipt(g):
    return {"namespace": g["namespace"], "job_sha256": g["job_sha256"], "node_id": g["node_id"]}


def scope_snapshot(g):
    done = subprocess.run(["systemctl", "show", g["scope"], "--property=LoadState", "--property=ActiveState",
                           "--property=ControlGroup", "--property=MainPID"], capture_output=True, text=True, timeout=5)
    props = dict(line.split("=", 1) for line in done.stdout.splitlines() if "=" in line)
    require(done.returncode == 0 or props.get("LoadState") == "not-found", "unknown exact scope")
    require(props.get("LoadState") in ("loaded", "not-found") and props.get("ActiveState") in
            ("active", "activating", "deactivating", "inactive", "failed"), "unknown scope state")
    expected = "/horizontal.slice/horizontal-client.slice/" + g["scope"]
    require(props.get("ControlGroup", "") in ("", expected), "foreign scope cgroup")
    pids = []
    group = Path("/sys/fs/cgroup" + expected)
    if group.exists():
        for file in (group / "cgroup.procs", *group.rglob("*/cgroup.procs")):
            if file.exists():
                pids += [int(x) for x in file.read_text().split()]
    processes = []
    for pid in sorted(set(pids)):
        proc = Path("/proc") / str(pid)
        try:
            args = (proc / "cmdline").read_bytes().rstrip(b"\x00").decode().split("\x00")
            start = int((proc / "stat").read_text().rsplit(")", 1)[1].split()[19])
            cgroup = (proc / "cgroup").read_text().splitlines()
            require(args == g["argv"] and cgroup == ["0::" + expected]
                    and os.readlink(proc / "exe").removesuffix(" (deleted)") == g["argv"][0], "scope PID/full command identity changed")
            processes.append({"pid": pid, "starttime_ticks": start, "argv": args, "cgroup": expected})
        except FileNotFoundError:
            continue
    return {"scope": g["scope"], "properties": props, "processes": processes}


def stop_scope(g):
    before = scope_snapshot(g)
    if before["processes"] or before["properties"]["ActiveState"] not in ("inactive", "failed"):
        subprocess.run(["systemctl", "stop", g["scope"]], check=True, capture_output=True, timeout=10)
    after = scope_snapshot(g)
    require(not after["processes"] and after["properties"]["ActiveState"] in ("inactive", "failed")
            and after["properties"].get("MainPID", "0") == "0", "exact job scope not quiescent")
    # A departed/misassigned process must not disappear from the unit census.
    residual = []
    for proc in Path("/proc").iterdir():
        if proc.name.isdigit():
            try:
                args = (proc / "cmdline").read_bytes().rstrip(b"\x00").decode().split("\x00")
                if args == g["argv"]:
                    residual.append(int(proc.name))
            except (FileNotFoundError, ProcessLookupError):
                continue
    require(not residual, "job process escaped its owned scope")
    return {"passed": True, "quiescent": True, "before": before, "after": after}


def run_job(path):
    g = load_grant(path)
    work, mode = Path(path).parent, g["job"]["mode"]
    require(read(work / (mode + "-ready.json")) == ready_receipt(g), "ready receipt changed")
    release = read(work / (mode + "-release.json"))
    require(set(release) == {"job_sha256", "release_id", "controller_ns"} and release["job_sha256"] == g["job_sha256"]
            and re.fullmatch("[a-f0-9]{64}", release["release_id"]) and type(release["controller_ns"]) is int,
            "common release not bound to grant")
    write(work / (mode + "-started.json"), {"job_sha256": g["job_sha256"], "release": release})
    limits = g["limits"]
    args = ["systemd-run", "--scope", "--quiet", "--collect", "--unit=" + g["scope"], "--slice=horizontal-client.slice",
            "--property=CPUQuota=400%", "--property=AllowedCPUs=0-3", "--property=MemoryMax=" + str(limits["memory_max_mib"] * 1024 ** 2),
            "--property=MemorySwapMax=0", "env", "-i", "PATH=/usr/bin:/bin", "HOME=/root", "GOMAXPROCS=4", *g["argv"]]
    write(work / (mode + "-command.json"), {"argv": args, "job_sha256": g["job_sha256"]})
    child, usage, code = None, None, None
    started = time.monotonic_ns()
    deadline = min(time.monotonic() + g["job"]["timeout_seconds"], time.monotonic() + g["active_deadline"] - time.time())
    streams, total, failure = [], 0, None
    try:
        child = subprocess.Popen(args, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                 env={"PATH": "/usr/bin:/bin", "HOME": "/root"})
        selector = selectors.DefaultSelector()
        for name in ("stdout", "stderr"):
            target = work / (mode + "." + name)
            fd = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
            stream = os.fdopen(fd, "wb")
            streams.append(stream)
            selector.register(getattr(child, name), selectors.EVENT_READ, stream)
        while code is None or selector.get_map():
            require(time.monotonic() < deadline and not (work / (mode + "-abort.json")).exists(), "job aborted/expired")
            for key, _ in selector.select(0.1):
                block = os.read(key.fd, 65536)
                if not block:
                    selector.unregister(key.fileobj)
                    key.fileobj.close()
                else:
                    total += len(block)
                    require(total <= MAX_LOG, "aggregate remote log bound exhausted")
                    key.data.write(block)
            if code is None:
                pid, status, usage_candidate = os.wait4(child.pid, os.WNOHANG)
                if pid:
                    usage, code = usage_candidate, os.waitstatus_to_exitcode(status)
                    child.returncode = code
        require(code == 0, "producer/control exited nonzero")
        report = read(g["job"]["output"])
        require(report.get("status") == "ok" and report.get("job_sha256") == g["job_sha256"]
                and report.get("effective_job") == g["job"], "missing/mismatched control capability report")
    except BaseException as exc:
        failure = exc
    finally:
        if child is not None and code is None:
            try:
                stop_scope(g)
            except BaseException as exc:
                failure = failure or exc
            finally:
                child.terminate()
                try:
                    code = child.wait(timeout=3)
                except subprocess.TimeoutExpired:
                    child.kill()
                    code = child.wait(timeout=3)
        for stream in streams:
            stream.flush()
            os.fsync(stream.fileno())
            stream.close()
        value = {"exit": code, "started_ns": started, "ended_ns": time.monotonic_ns(), "job_sha256": g["job_sha256"],
                 "error": None if failure is None else "job failed; retained partial artifacts", "log_bytes": total}
        if usage is not None:
            value["process_resources"] = {"scope": "argv_to_exit", "measurement_source": "wait4_systemd_scope_wrapper",
                "user_cpu_seconds": usage.ru_utime, "system_cpu_seconds": usage.ru_stime,
                "cpu_seconds": usage.ru_utime + usage.ru_stime, "peak_rss_bytes": usage.ru_maxrss * 1024}
        write(work / (mode + "-exit.json"), value)
    if failure is not None:
        raise failure
    return value


def bound_ssh_logs():
    # Kernel cap applies independently to each of the two exclusive SSH logs.
    resource.setrlimit(resource.RLIMIT_FSIZE, (MAX_LOG // 2, MAX_LOG // 2))


def remote_main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("ready", "release", "run", "abort", "stopped", "read", "collect", "identity"))
    parser.add_argument("--path", required=True)
    parser.add_argument("--node-id", type=int, required=True)
    parser.add_argument("--release-id")
    parser.add_argument("--controller-ns", type=int)
    a = parser.parse_args(argv)
    identity(a.node_id)
    path = clean(a.path)
    if a.mode == "identity":
        print(json.dumps({"node_id": a.node_id}))
        return
    if a.mode == "read":
        require(str(path).startswith(("/srv/otel-paired/", "/srv/otel-horizontal/")), "foreign receipt path")
        print(json.dumps(read(path), sort_keys=True))
        return
    g = load_grant(path, active=a.mode in ("ready", "release", "run"), check_inputs=a.mode in ("ready", "run"))
    require(g["node_id"] == a.node_id, "grant node mismatch")
    mode, work = g["job"]["mode"], path.parent
    if a.mode == "ready":
        value = ready_receipt(g)
        write(work / (mode + "-ready.json"), value)
    elif a.mode == "release":
        require(read(work / (mode + "-ready.json")) == ready_receipt(g), "ready/release mismatch")
        require(type(a.release_id) is str and re.fullmatch("[a-f0-9]{64}", a.release_id)
                and type(a.controller_ns) is int and a.controller_ns > 0, "common release required")
        value = {"job_sha256": g["job_sha256"], "release_id": a.release_id, "controller_ns": a.controller_ns}
        write(work / (mode + "-release.json"), value)
    elif a.mode == "run":
        value = run_job(path)
    elif a.mode == "abort":
        write(work / (mode + "-abort.json"), {"job_sha256": g["job_sha256"]})
        value = stop_scope(g)
    elif a.mode == "stopped":
        value = stop_scope(g)
    else:
        artifacts = {}
        sidecars = {g["job"].get("verification", {}).get("query_results_file")}
        if mode == "setup":
            sidecars.add(g["job"].get("layout_file"))
        for item in sorted(work.iterdir()):
            if item.name.startswith(mode) or item.name == "job.json" or str(item) in sidecars:
                info = item.lstat()
                require(stat.S_ISREG(info.st_mode) and info.st_nlink == 1 and info.st_size <= MAX_ARTIFACT,
                        "partial artifact type/size")
                require(sum(len(x.encode()) for x in artifacts.values()) + info.st_size <= MAX_ARTIFACT,
                        "aggregate partial artifact cap")
                artifacts[item.name] = item.read_text()
        value = {"node_id": a.node_id, "job_sha256": g["job_sha256"], "artifacts": artifacts}
    print(json.dumps(value, sort_keys=True, allow_nan=False))


class NativeBackend:
    """Real provider/process adapter. Tests inject provider, clocks and process handles.

    All mutations flow through provision's immutable watch/cost admission. Abort,
    reaping, evidence collection and teardown deliberately bypass active admission.
    """
    fake = False

    def __init__(self, plan, output, provision=None, *, clock=time.time, monotonic=time.monotonic_ns,
                 sleep=time.sleep, popen=subprocess.Popen, runner=None):
        import driver
        import paired
        import paired_cost
        import supervisor
        if provision is None:
            import provision as provider
            provision = provider
        self.d, self.q, self.cost, self.supervisor = driver, paired, paired_cost, supervisor
        self.plan, self.output, self.provision = copy.deepcopy(plan), Path(output), provision
        self.provision_out = self.output / "provision"
        self.clock, self.mono, self.sleep, self.popen, self.runner = clock, monotonic, sleep, popen, runner
        self.nodes, self.resources, self.ledger = {}, None, None
        self.pending, self.completed, self.layouts, self.deleted = {}, [], {}, set()
        self.sequence, self.quiet_sequence, self.cluster = 0, 0, None
        self.lock = threading.RLock()
        backend = self
        class Campaign(driver.Campaign):
            def command(inner, node, args, cap=30):
                with inner.sequence_lock:
                    inner.sequence += 1
                return backend.command(node, args, cap)
        self.monitor = Campaign(self.plan, self.output / "native", provision, clock=monotonic, sleep=sleep,
                                provision_out=self.provision_out, receipt_root=self.output)
        self.helper = "/srv/otel-horizontal/" + plan["campaign"] + "/harness/paired_native.py"

    def now(self):
        return int(self.clock())

    def monotonic_ns(self):
        return self.mono()

    def owned_ids(self):
        return sorted(node["id"] for node in self.nodes.values() if node["id"] not in self.deleted)

    def _same(self, plan):
        require(plan == self.plan, "immutable paired plan changed")

    def _gate(self, cap=30):
        active = self.plan["deadline_epoch"] - (900 if self.plan["stage_profile"] == "pilot" else 1200)
        require(self.clock() < active, "original active cutoff exhausted")
        if self.ledger is not None:
            self.ledger = self.cost.checkpoint(self.ledger, now_epoch=self.now(), owned_ids=self.owned_ids(),
                                               deadline_epoch=self.plan["deadline_epoch"])
            report = self.cost.assess(self.ledger, now_epoch=self.now(), owned_ids=self.owned_ids())
            total = Decimal(report["total_upper_eur"])
            require(total <= Decimal(self.plan["stage_budget_eur"])
                    and total + Decimal(self.plan["prior_cost_upper_eur"]) <= 10, "stage/shared cost budget exhausted")
        self.monitor.receipt_budget()
        return min(cap, active - self.clock())

    def _save(self, kind, value):
        with self.lock:
            self.sequence += 1
            self.q.write_receipt(self.output / "native" / ("%05d-%s.json" % (self.sequence, kind)), value, receipt_root=self.output)

    def update_cost_ledger(self, ledger):
        """Synchronize controller checkpoints without erasing provision's newer head."""
        with self.lock:
            path = self.provision_out / "provision" / "paired-cost-ledger.json"
            self.cost.assess(ledger, now_epoch=self.now(), owned_ids=self.owned_ids(), deadline_epoch=self.plan["deadline_epoch"])
            if path.exists():
                current = self.d.read(path)
                require(all(current[k] == ledger[k] for k in ("schema", "first_epoch", "deadline_epoch", "held_hosts", "pricing_seal"))
                        and [x["host_id"] for x in current["hosts"]] == [x["host_id"] for x in ledger["hosts"]],
                        "cost checkpoint ownership/scope changed")
                ledger = current if current["last_epoch"] > ledger["last_epoch"] else ledger
            self.ledger = self.cost.checkpoint(ledger, now_epoch=self.now(), owned_ids=self.owned_ids(), deadline_epoch=self.plan["deadline_epoch"])
            if path.exists():
                with self.provision._lock(path.parent):
                    self.provision._json(path, self.ledger, replace=True)

    def command(self, node, args, cap=30, *, cleanup=False):
        timeout = min(cap, max(0.001, self.plan["deadline_epoch"] - self.clock())) if cleanup else self._gate(cap)
        if cleanup:
            # Bypass run_on_node's *active* gate only for exact grant abort/read.
            argv = self.provision.ssh_argv(self.plan, self.provision_out, node, args)
            rc, stdout, stderr = self.provision._call(self.runner, argv, timeout=timeout)
        else:
            rc, stdout, stderr = self.provision.run_on_node(self.plan, self.provision_out, node, args,
                                                         runner=self.runner, timeout=timeout)
        self._save("command", {"node_id": node["id"], "argv": args, "exit": rc, "stdout": stdout,
                               "stderr": stderr, "controller_ns": self.mono()})
        require(rc == 0, "owned-node command failed; retained raw receipt")
        return stdout

    def _remote(self, node, mode, path, extra=(), *, cleanup=False):
        return loads(self.command(node, ["python3", self.helper, mode, "--path", str(path), "--node-id", str(node["id"]), *extra],
                                  cleanup=cleanup))

    def _copy(self, node, local, remote):
        rc, stdout, stderr = self.provision.copy_to_node(self.plan, self.provision_out, node, local, remote,
                                                       runner=self.runner, timeout=self._gate(120))
        self._save("copy", {"node_id": node["id"], "path": remote, "exit": rc, "stdout": stdout, "stderr": stderr})
        require(rc == 0, "sealed copy failed")
        result = self.command(node, ["sha256sum", remote])
        require(result.split()[0] == sha(local), "remote copy seal mismatch")

    def capture_pricing(self, plan):
        self._same(plan)
        quote = self.cost.validate_pricing(plan["pricing"], now_epoch=self.now(), expected_seal=plan["pricing_seal_sha256"])
        argv = [plan["hcloud"], "api", "/pricing", "--method", "GET"]
        rc, raw, stderr = self.provision._call(self.runner, argv, timeout=self._gate(20))
        self._save("pricing", {"argv": argv, "read_only": True, "exit": rc, "stdout": raw, "stderr": stderr,
                               "observed_epoch": self.now()})
        require(rc == 0 and hashlib.sha256(raw.encode()).hexdigest() == quote["provenance"]["response_sha256"],
                "live pricing response changed or unavailable")
        pricing = loads(raw)["pricing"]
        require(pricing["currency"] == "EUR" and type(pricing["vat_rate"]) is str
                and Decimal(pricing["vat_rate"]) / 100 == Decimal(quote["provenance"]["vat_rate"]),
                "provider currency/VAT changed")
        def hourly(items, key, value):
            selected = [x for x in items if x.get(key) == value]
            require(len(selected) == 1, "ambiguous live pricing item")
            selected = [x for x in selected[0]["prices"] if x.get("location") == "hel1"]
            require(len(selected) == 1, "ambiguous live pricing location")
            return selected[0]["price_hourly"]
        for kind, actual in (("server", hourly(pricing["server_types"], "name", "ccx43")),
                             ("ipv4", hourly(pricing["primary_ips"], "type", "ipv4"))):
            for field in ("net", "gross"):
                require(type(actual[field]) is str and Decimal(actual[field]) == Decimal(quote[kind + "_hourly_" + field + "_eur"]),
                        "normalized live pricing differs from approved quote")
        return quote

    def prepare(self, plan, ledger):
        self._same(plan)
        self.ledger = copy.deepcopy(ledger)
        self._gate()
        self.supervisor.register(plan, self.output, runner=self.runner, clock=self.clock, sleep=self.sleep)
        self.resources = self.provision.prepare(plan, self.provision_out, cost_ledger=ledger,
                                              runner=self.runner, clock=self.clock, sleep=self.sleep)
        self.nodes = self.resources["nodes"]
        require(set(self.nodes) == set(self.q.held_roles(plan)), "exact newly held roles required")
        for role, node in self.nodes.items():
            require(node.get("role") == role and type(node.get("id")) is int and node["id"] > 0
                    and type(node.get("created_epoch")) is int and plan["first_epoch"] <= node["created_epoch"] <= self.now(),
                    "exact provider creation receipt required")
            self.ledger = self.cost.record_creation(self.ledger, host_id=node["id"], created_epoch=node["created_epoch"], now_epoch=self.now())
        self.monitor.nodes, self.monitor.resources = self.nodes, self.resources
        self.monitor.install_helpers()
        for node in self.nodes.values():
            self._copy(node, Path(__file__).resolve(), self.helper)
            require(self._remote(node, "identity", self.helper) == {"node_id": node["id"]}, "helper identity failed")
        return copy.deepcopy(self.nodes)

    def transfer_assets(self, plan, resources):
        self._same(plan)
        require(resources == self.nodes, "transfer host set changed")
        state = self.provision_out / "provision"
        packed = self.d.read(state / "asset-transfer.json")["assets"]
        result = {}
        for role, node in resources.items():
            assets = {}
            for name, sealed in plan["assets"].items():
                item = packed[name]
                require(item["sha256"] == sealed["sha256"], "packed source seal changed")
                op = "install" if role == next(iter(resources)) else "fetch"
                receipt = self.d.read(state / (role + "-asset-" + name + "-" + op + ".json"))
                require(receipt["node_id"] == node["id"] and receipt["returncode"] == 0 and receipt["timed_out"] is False,
                        "missing per-host verified transfer")
                remote = loads(receipt["stdout"].strip().splitlines()[-1])
                require(remote == {"dest": self.remote_assets(plan)[name], "sha256": sealed["sha256"],
                                   "bytes": item["bytes"], "src_sha256": item["transfer_sha256"]}, "transfer receipt mismatch")
                assets[name] = {"sha256": remote["sha256"], "transfer_sha256": remote["src_sha256"], "bytes": remote["bytes"]}
            result[role] = {"node_id": node["id"], "assets": assets}
        self.q.validate_transfers(plan, resources, result)
        return result

    def remote_assets(self, plan):
        self._same(plan)
        return {name: "/srv/otel-horizontal/" + plan["campaign"] + "/bin/" + name for name in plan["assets"]}

    def quiet(self, plan, resources):
        self._same(plan)
        require(resources == self.nodes, "quiet host set changed")
        self._gate(100)
        self.quiet_sequence += 1
        self.monitor.out = self.output / "native" / ("quiet-%d" % self.quiet_sequence)
        self.monitor.quiet()

    def _healthy(self):
        self._gate()
        require(self.cluster is not None, "no active native cluster")
        return self.monitor.monitor(self.cluster)

    def _stable(self, *, startup=False, seconds=180):
        limit, samples = self.mono() + int(self._gate(seconds) * 10 ** 9), []
        next_sample = self.mono()
        while self.mono() < limit:
            delay = (next_sample - self.mono()) / 10 ** 9
            if delay > 0:
                self.sleep(delay)
            self._gate()
            if startup:
                self.monitor.host_samples()
                try:
                    sample = self.monitor.status(self.cluster)
                except (ValueError, KeyError, TypeError) as exc:
                    self._save("startup-not-ready", {"error": str(exc)})
                    samples = []
                    next_sample = self.mono() + 5 * 10 ** 9
                    continue
                sample = self.monitor.backlog(sample)
            else:
                sample = self._healthy()
            sample = {**sample, "healthy": True, "controller_ns": self.mono()}
            samples.append(sample)
            self._save("storage-drain", sample)
            good = lambda x: type(x.get("queue_bytes")) is int and 0 <= x["queue_bytes"] < 16777216 and type(x.get("lag_seconds")) in (int, float) and 0 <= x["lag_seconds"] < 10
            if len(samples) >= 3 and all(good(x) for x in samples[-3:]):
                require(all(b["controller_ns"] - a["controller_ns"] >= 5 * 10 ** 9 for a, b in zip(samples[-3:], samples[-2:])), "drain cadence too short")
                return samples
            next_sample = self.mono() + 5 * 10 ** 9
        raise ValueError("strict stable storage-drain deadline exhausted")

    def fresh_cluster(self, plan, resources, hosts, identity):
        self._same(plan)
        require(resources == self.nodes and not self.pending, "cluster ownership/pending job violation")
        self._gate()
        self.cluster = self.provision.setup_cluster(plan, self.provision_out, resources, hosts, identity,
                                                   runner=self.runner, clock=self.clock, sleep=self.sleep)
        self.monitor.pid_baseline = None
        observations = self.monitor.host_samples()
        self.monitor.pid_baseline = {role: sample["fdb_pids"] for role, sample in observations.items() if role.startswith("node")}
        self._stable(startup=True)
        value = {"path": self.cluster["remote_cluster_file"], "sha256": sha(self.cluster["cluster_file"])}
        self.layouts[value["path"]] = self.cluster
        return value

    def _control_job(self, job, mode):
        value = copy.deepcopy(job)
        value.update(self.q.control_identity(self.plan))
        value["mode"] = mode
        return value

    def _stage(self, node, ns, job, argv, *, mkdir=False):
        self.q.validate_job(job)
        root = "/srv/otel-paired/" + ns
        if mkdir:
            self.command(node, ["mkdir", "-p", str(Path(root).parent)])
            self.command(node, ["mkdir", "-m", "0700", root])
        mode = job["mode"]
        path = root + "/" + mode + "-grant.json"
        inputs = {remote: self.plan["assets"][name]["sha256"] for name, remote in self.remote_assets(self.plan).items()}
        for key in ("cluster", "corpus") + (("layout",) if mode != "setup" else ()):
            inputs[job[key + "_file"]] = job[key + "_sha256"]
        geometry = self.q.native_geometry(self.plan["native_geometry"])
        grant = {"schema": "otel_paired_grant_v1", "node_id": node["id"], "namespace": ns, "job": copy.deepcopy(job),
                 "job_sha256": self.q.job_seal(job), "argv": argv, "inputs": inputs,
                 "active_deadline": self.plan["deadline_epoch"] - (900 if self.plan["stage_profile"] == "pilot" else 1200),
                 "hard_deadline": self.plan["deadline_epoch"], "limits": geometry["client_slice"],
                 "scope": "otel-paired-" + self.q.job_seal(job)[:24] + ".scope"}
        directory = self.output / "native" / "jobs" / ns / mode
        self.d.write(directory / "job.json", job)
        self.d.write(directory / "grant.json", grant)
        self.command(node, ["test", "!", "-e", argv[1]])
        self.command(node, ["test", "!", "-e", path])
        self._copy(node, directory / "job.json", argv[1])
        self._copy(node, directory / "grant.json", path)
        receipt = self._remote(node, "ready", path)
        require(receipt == ready_receipt(grant), "remote ready binding mismatch")
        return {"node": node, "namespace": ns, "job": copy.deepcopy(job), "grant": grant, "path": path,
                "directory": directory, "ready": receipt}

    def setup(self, plan, cluster, namespace, count):
        self._same(plan)
        match = re.fullmatch(re.escape(plan["campaign"]) + r"/pairs([123])-(go_pure|go_native|rust_native)-rep([12])/(prerequisite|measured)/client([123])", namespace)
        require(match is not None, "setup namespace not preregistered")
        hosts, arm, repeat, phase, client = match.groups()
        require(count == (100000 if phase == "prerequisite" else 375000) and cluster["path"] in self.layouts,
                "setup corpus/cluster mismatch")
        output = "/srv/otel-paired/" + namespace + "/setup.json"
        layout = {"path": "/srv/otel-paired/" + namespace + "/layout.json", "sha256": "0" * 64}
        job = self.q._job(plan, arm, int(hosts), phase, int(client), layout, cluster, self.remote_assets(plan),
                          output, repeat=int(repeat))
        job = self._control_job(job, "setup")
        del job["layout_sha256"]
        node = self.nodes["client" + client]
        argv = [self.remote_assets(plan)["go_binary"], str(Path(output).with_name("setup-job.json"))]
        staged = self._stage(node, namespace, job, argv, mkdir=True)
        report = self._execute({int(client): staged})[int(client)]
        self._common(report, job)
        require(report.get("database_path") == "/OTEL/R_" + job["run"].upper() and report.get("tenant") == 0,
                "setup production namespace/tenant mismatch")
        baseline = report.get("baseline")
        require(type(baseline) is dict and baseline.get("record_identities") == 1
                and baseline.get("index_entries") == {"STORE_META_NAME": 1}
                and type(baseline.get("physical_record_kvs")) is int and baseline["physical_record_kvs"] >= 1
                and baseline.get("header_kvs") == 1
                and type(baseline.get("normalized_kv_sha256")) is str
                and re.fullmatch("[a-f0-9]{64}", baseline["normalized_kv_sha256"]), "setup exact metadata baseline unavailable")
        artifacts = [a for a in report.get("artifacts", []) if a.get("kind") == "layout" and a.get("path") == job["layout_file"]]
        require(len(artifacts) == 1 and artifacts[0].get("sha256") == report.get("layout_sha256")
                and re.fullmatch("[a-f0-9]{64}", report["layout_sha256"]), "sealed Go layout artifact required")
        result = self.command(node, ["sha256sum", job["layout_file"]])
        require(result.split()[0] == report["layout_sha256"], "setup layout remote SHA mismatch")
        return {"path": job["layout_file"], "sha256": report["layout_sha256"]}

    def _common(self, report, job):
        require(type(report) is dict and report.get("schema") == "otel_control_report_v1"
                and report.get("mode") == job["mode"] and report.get("arm") == job["arm"]
                and report.get("status") == "ok" and report.get("error") in (None, "")
                and report.get("effective_job") == job and report.get("job_sha256") == self.q.job_seal(job), "missing/mismatched genuine control capability")
        for key in ("binary_sha256", "source_revision", "source_manifest_sha256", "corpus_sha256", "client_library_sha256"):
            require(report.get(key) == job[key], "control executable/source/input seal mismatch")

    def ready(self, plan, node, namespace, job, argv):
        self._same(plan)
        require(node in self.nodes.values() and node.get("role", "").startswith("client"), "exact owned client required")
        require(namespace not in self.pending and all(x["namespace"] != namespace or x["job"]["mode"] != "ingest" for x in self.completed), "duplicate ingest namespace")
        expected = self.q.invocation(plan, job, "/srv/otel-paired/" + namespace + "/job.json", self.remote_assets(plan))
        require(argv == expected, "typed executable invocation mismatch")
        staged = self._stage(node, namespace, job, argv)
        self.pending[namespace] = staged
        return staged["ready"]

    def _launch(self, staged):
        self._gate()
        self.supervisor.admit(self.plan, self.provision_out, runner=self.runner, clock=self.clock)
        node = staged["node"]
        args = ["python3", self.helper, "run", "--path", staged["path"], "--node-id", str(node["id"])]
        argv = self.provision.ssh_argv(self.plan, self.provision_out, node, args)
        # Remote helper logs are bounded; SSH stdout is only a small exit receipt.
        staged["stdout"] = open(staged["directory"] / "ssh.stdout", "xb")
        staged["stderr"] = open(staged["directory"] / "ssh.stderr", "xb")
        os.chmod(staged["stdout"].name, 0o600)
        os.chmod(staged["stderr"].name, 0o600)
        staged["process"] = self.popen(argv, stdin=subprocess.DEVNULL, stdout=staged["stdout"], stderr=staged["stderr"],
                                       preexec_fn=bound_ssh_logs)

    def _execute(self, staged):
        require(staged and len({x["grant"]["job_sha256"] for x in staged.values()}) == len(staged), "nonempty unique job release required")
        released = self.mono()
        release_id = hashlib.sha256(encoded({"jobs": sorted(x["grant"]["job_sha256"] for x in staged.values()), "ns": released})).hexdigest()
        failure, result = None, {}
        try:
            def release(item):
                self._gate()
                return self._remote(item["node"], "release", item["path"], ("--release-id", release_id, "--controller-ns", str(released)))
            with ThreadPoolExecutor(max_workers=len(staged)) as executor:
                list(executor.map(release, staged.values()))
            for item in staged.values():
                self._launch(item)
            deadline = self.mono() + max(x["job"]["timeout_seconds"] + 15 for x in staged.values()) * 10 ** 9
            while any(item["process"].poll() is None for item in staged.values()):
                require(self.mono() < deadline, "common producer deadline exhausted; no retry")
                self._healthy()
                for item in staged.values():
                    require(sum(Path(item[key].name).stat().st_size for key in ("stdout", "stderr")) <= MAX_LOG, "SSH log bound exhausted")
                self.sleep(1)
            self._healthy()
            for client, item in staged.items():
                code = item["process"].wait(timeout=2)
                require(code == 0, "producer/control process failed; raw partial artifacts retained")
                result[client] = self._remote(item["node"], "read", item["job"]["output"])
                self._common(result[client], item["job"])
        except BaseException as exc:
            failure = exc
        finally:
            for item in staged.values():
                try:
                    process = item.get("process")
                    if process is not None and process.poll() is None:
                        self._remote(item["node"], "abort", item["path"], cleanup=True)
                        try:
                            process.wait(timeout=5)
                        except subprocess.TimeoutExpired:
                            process.terminate()
                            try:
                                process.wait(timeout=3)
                            except subprocess.TimeoutExpired:
                                process.kill()
                                process.wait(timeout=3)
                    proof = self._remote(item["node"], "stopped", item["path"], cleanup=True)
                    require(proof.get("passed") is True and proof.get("quiescent") is True, "exact scope stop proof missing")
                    self._save("job-quiescence", proof)
                except BaseException as exc:
                    failure = failure or exc
                finally:
                    process = item.get("process")
                    if process is not None and process.poll() is None:
                        process.terminate()
                        try:
                            process.wait(timeout=3)
                        except subprocess.TimeoutExpired:
                            process.kill()
                            process.wait(timeout=3)
                    for key in ("stdout", "stderr"):
                        if key in item:
                            item[key].close()
                    try:
                        raw = self._remote(item["node"], "collect", item["path"], cleanup=True)
                        self.d.write(item["directory"] / "raw.json", raw)
                    except BaseException as exc:
                        self._save("collection-failure", {"job_sha256": item["grant"]["job_sha256"], "error": str(exc)})
                        failure = failure or exc
                self.completed.append(item)
        if failure is not None:
            raise failure
        return result

    def release_and_wait(self, plan, ready, jobs):
        self._same(plan)
        require(set(ready) == set(jobs) and jobs, "complete wave ready population required")
        staged = {}
        for client, job in jobs.items():
            ns = ready[client]["namespace"]
            require(ns in self.pending and self.pending[ns]["job"] == job and self.pending[ns]["ready"] == ready[client]
                    and self.pending[ns]["node"] == self.nodes["client" + str(client)], "released job differs from exact ready grant")
            staged[client] = self.pending[ns]
        # Consume before *any* release action, including ambiguous SSH failures.
        for item in staged.values():
            del self.pending[item["namespace"]]
        return self._execute(staged)

    def drain(self, plan, cluster):
        self._same(plan)
        require(cluster["path"] == self.cluster["remote_cluster_file"], "drain cluster changed")
        return self._stable(seconds=300)

    def verify(self, plan, namespace, job):
        self._same(plan)
        from paired_reports import verification_job
        verify = self._control_job(verification_job(job), "verify")
        node = next((x["node"] for x in self.completed if x["namespace"] == namespace and x["job"] == job), None)
        require(node is not None, "verification has no completed ingest job")
        argv = [self.remote_assets(plan)["go_binary"], str(Path(job["output"]).with_name("verify-job.json"))]
        staged = self._stage(node, namespace, verify, argv)
        report = self._execute({1: staged})[1]
        path = verify["verification"]["query_results_file"]
        # Full sidecar, not a digest-only success assertion. Fetch via bounded helper.
        raw = self._remote(node, "collect", staged["path"])
        text = raw["artifacts"].get("queries.ndjson")
        require(type(text) is str and hashlib.sha256(text.encode()).hexdigest() == report["oracle"]["results_sha256"], "oracle raw SHA mismatch")
        queries = [loads(line) for line in text.splitlines()]
        require(len(queries) == 1600 and report["oracle"]["results_file"] == path, "complete source oracle required")
        return {"report": report, "queries": queries, "verification_job": verify,
                "ingest_job_sha256": self.q.job_seal(job), "query_raw": text}

    def quiesce(self, plan, cluster):
        self._same(plan)
        require(not self.pending and cluster["path"] == self.cluster["remote_cluster_file"], "pending/foreign arm quiescence")
        scopes = []
        for item in self.completed:
            if item["job"]["cluster_file"] == cluster["path"]:
                proof = self._remote(item["node"], "stopped", item["path"], cleanup=True)
                require(proof.get("passed") is True and proof.get("quiescent") is True, "paired scope residual")
                scopes.append(proof)
        self._gate()
        native = self.provision.stop_owned_arm(plan, self.provision_out, self.nodes, self.cluster, scopes=(),
                                               runner=self.runner, clock=self.clock, sleep=self.sleep)
        require(native.get("passed") is True and native.get("quiescent") is True, "native unit/PID residual")
        self.monitor.pid_baseline = None
        return {"passed": True, "quiescent": True, "native": native, "scopes": scopes}

    def cleanup(self, plan):
        self._same(plan)
        # Teardown remains unconditional after health/cost/watch/active-time refusal.
        state = self.provision_out / "provision"
        if not (state / "resources.json").exists():
            require(self.resources is None and not self.nodes, "missing provision ownership journal")
            return {"passed": True, "exact_baseline": True, "deletions": [], "no_provider_effects": True}
        resources = self.resources or self.d.read(state / "resources.json")
        receipt = self.provision.teardown(plan, self.provision_out, resources, runner=self.runner, clock=self.clock, sleep=self.sleep)
        require(receipt.get("passed") is True and receipt.get("inventory", {}).get("passed") is True,
                "ten-kind exact baseline teardown not verified")
        summary = receipt["inventory"]
        require(set(summary.get("kinds", {})) == set(self.provision.KINDS)
                and all(x.get("passed") is True for x in summary["kinds"].values()), "incomplete ten-kind baseline population")
        deletions = []
        ids = resources["ids"]["server"]
        require(len(ids) == len(set(ids)) and not set(ids) & set(resources["protected_ids"]["server"]), "protected/duplicate deletion ID")
        for sid in ids:
            # hcloud api raw error JSON must explicitly identify provider not_found.
            argv = [plan["hcloud"], "api", "/servers/" + str(sid), "--method", "GET"]
            rc, stdout, stderr = self.provision._call(self.runner, argv, timeout=min(10, max(0.001, plan["deadline_epoch"] - self.clock())))
            self._save("deletion-get", {"argv": argv, "exit": rc, "stdout": stdout, "stderr": stderr, "observed_epoch": self.now()})
            body = loads(stdout)
            require(rc != 0 and body == {"error": {"code": "not_found", "message": "Server not found"}},
                    "exact provider GET404 absence required; auth/timeout/list omission insufficient")
            proof = {"host_id": sid, "observed_epoch": self.now(), "source": "https://api.hetzner.cloud/v1/servers/" + str(sid), "status": "not_found"}
            proof["seal_sha256"] = self.cost.seal_receipt(proof)
            deletions.append(proof)
            self.deleted.add(sid)
        result = {"passed": True, "exact_baseline": True, "deletions": deletions, "teardown": receipt}
        if (state / "paired-cost-ledger.json").exists():
            result["cost_ledger"] = self.d.read(state / "paired-cost-ledger.json")
        return result


if __name__ == "__main__":
    try:
        remote_main()
    except (ValueError, KeyError, OSError, subprocess.SubprocessError):
        print("paired remote helper refused; inspect retained exclusive artifacts", file=sys.stderr)
        sys.exit(2)
