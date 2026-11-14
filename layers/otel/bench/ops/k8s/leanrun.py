#!/usr/bin/env python3
"""Kubernetes benchmark runner for the OTel span-writer comparison (Go own client, Go C client, Rust).

Provisions a Hetzner Cloud lease (k3s + Cilium + the FoundationDB Kubernetes operator, the stack of the reference run
k8s-cilium4), runs the measured waves against fresh clusters, and tears everything down by label.  Standard library only.
Every provider / ssh / systemd command goes through Ctx.run(), which in --dry-run prints the command and does not execute it.
One plain JSON-lines log per campaign: <state>/log.jsonl.  State directory: $LEAN_STATE_DIR or config "state_dir", default ./runs.

Sub-commands:  cost | prepare-assets | up | sweep | down | status | selftest | watchdog (internal)
`up` creates billable servers and needs --yes; `cost`, `prepare-assets`, `selftest` and everything with --dry-run touch nothing
on the provider.  See README.md for the runbook.
"""
import argparse
import concurrent.futures as cf
import datetime
from decimal import Decimal
import hashlib
import json
import math
import os
import shlex
import subprocess
import sys
import threading
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import condition  # noqa: E402
FDB_OPERATOR_VERSION = "v2.38.0"
FDB_VERSION = "7.3.77"
K3S_VERSION = "v1.31.5+k3s1"
CILIUM_CLI = "v0.20.2"
NETWORK_CIDR = "10.99.0.0/24"
ROLES = ("node1", "node2", "node3", "client1", "client2", "client3")
PRIVATE_IP = {"node%d" % i: "10.99.0.%d" % (10 + i) for i in range(1, 7)}
PRIVATE_IP.update({"client%d" % i: "10.99.0.%d" % (20 + i) for i in range(1, 7)})
REMOTE = "/srv/lean"
CLIENT_SHA256 = "617572467897d0360f76ffc33f24b58090ee691e3273750d2ff334cc71aae3fa"
POINT2_WORKLOAD = ('{"kind":"collector_queue","producers":8,"producer_batch_spans":1,"consumers":8,'
                   '"queue_capacity_spans":10000,"batch_min_spans":64,"batch_max_spans":256,"flush_ms":200,'
                   '"block_on_overflow":true,"export_timeout_ms":120000,"retry_enabled":true,"retry_initial_ms":500,'
                   '"retry_max_ms":10000,"retry_elapsed_ms":300000}')
ROTATIONS = {1: ("go_pure", "go_native", "rust_native"),
             2: ("go_native", "rust_native", "go_pure"),
             3: ("rust_native", "go_pure", "go_native")}
KINDS = ("server", "firewall", "ssh-key", "placement-group", "primary-ip", "volume", "network")
TIMEOUTS = {"corpus": 600, "setup": 300, "verify": 1800}
VERIFICATION_NONE = {"query_oracle": "none", "query_results_file": None, "k": 100, "max_candidates": 20000,
                     "max_bytes": 4 << 20}
VERIFICATION_ORACLE = {"query_oracle": "source_1600_v1", "k": 100, "max_candidates": 20000, "max_bytes": 4 << 20}


class Stop(Exception):
    pass


class CondFail(Stop):
    """Conditioning failed (storage process died, timeout, digest/state mismatch): the run is repeated unconditioned."""


def now_iso():
    return datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


# ------------------------------------------------------------------------------------------ context
class Ctx:
    """Config + state dir + the single place where commands are executed (or only printed)."""

    def __init__(self, cfg, dry):
        self.cfg, self.dry = cfg, dry
        self.campaign = cfg["campaign"]
        self.state = Path(os.environ.get("LEAN_STATE_DIR") or cfg.get("state_dir") or HERE / "runs") / self.campaign
        self.state.mkdir(parents=True, exist_ok=True, mode=0o700)
        self.lock = threading.Lock()
        self.label = "campaign=" + self.campaign
        self.hcloud = cfg["hcloud"]
        self.key = self.state / "controller-key"
        self.known_hosts = self.state / "known_hosts"
        self.roles = tuple(cfg.get("roles", ROLES))
        self.clients = [r for r in self.roles if r.startswith("client")]
        self.nodes = [r for r in self.roles if r.startswith("node")]
        self.colocate = bool(cfg.get("colocate_clients"))      # generators run ON the database nodes: client<k> is an alias of node<k>
        self.real_roles = tuple(r for r in self.roles if not (self.colocate and r.startswith("client")))
        if self.colocate:
            for r in self.clients:
                PRIVATE_IP[r] = PRIVATE_IP["node" + r[len("client"):]]
        self.nonce = "%x" % int(time.time())  # run names must never repeat, also when a sweep is retried in the same campaign
        self.sj = self.state / "state.json"
        self.s = json.loads(self.sj.read_text()) if self.sj.exists() else {}

    # -- state and log
    def save(self):
        if not self.dry:
            tmp = self.sj.with_suffix(".tmp")
            tmp.write_text(json.dumps(self.s, indent=1, sort_keys=True))
            os.chmod(tmp, 0o600)
            tmp.replace(self.sj)

    def log(self, event, **kw):
        rec = {"t": now_iso(), "event": event, **kw}
        with self.lock:
            if not self.dry:
                with open(self.state / "log.jsonl", "a") as f:
                    f.write(json.dumps(rec, sort_keys=True) + "\n")

    def say(self, *a):
        print(*a, flush=True)

    # -- command execution
    def run(self, argv, *, timeout=120, check=True, stdin=None, fake="", read_only=False, quiet=False):
        """Run (or, in --dry-run, only print) one command.  Returns (rc, stdout, stderr)."""
        shown = " ".join(shlex.quote(a) for a in argv)
        if self.dry:
            self.say("DRY  " + ("[read-only] " if read_only else "") + shown[:600] + (" ..." if len(shown) > 600 else ""))
            return 0, fake, ""
        t0 = time.time()
        for attempt in range(8):
            p = subprocess.run(argv, input=stdin, capture_output=True, text=True, timeout=timeout)
            # sshd refuses bursts of new connections (MaxStartups): the command did NOT run, so retrying is safe
            if argv[0] in ("ssh", "scp") and p.returncode == 255 and any(m in p.stderr for m in ("kex_exchange_identification", "Connection reset by peer", "Connection refused", "banner exchange")):
                time.sleep(1.0 + 1.5 * attempt + (hash(shown) % 100) / 100.0)
                continue
            break
        self.log("cmd", argv=argv if len(shown) < 4000 else [argv[0], "..."], rc=p.returncode, secs=round(time.time() - t0, 2),
                 stderr=p.stderr[-400:] if p.returncode else "")
        if check and p.returncode != 0:
            raise Stop("command failed (%d): %s\n%s" % (p.returncode, shown[:300], p.stderr[-800:]))
        return p.returncode, p.stdout, p.stderr

    def hc(self, *args, check=True, fake="", read_only=False, timeout=120):
        return self.run([self.hcloud, *args], check=check, fake=fake, read_only=read_only, timeout=timeout)

    def hc_list(self, kind, label=None):
        _, out, _ = self.hc(kind, "list", "-l", label or self.label, "-o", "json", read_only=True, fake="[]")
        out = out.strip()
        return json.loads(out) if out and out != "null" else []

    def ssh_opts(self):
        return ["-i", str(self.key), "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10",
                "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + str(self.known_hosts),
                "-o", "ServerAliveInterval=15", "-o", "LogLevel=ERROR"]

    def ip(self, role):
        return self.s.get("hosts", {}).get(role, {}).get("ip") or "<%s-public-ip>" % role

    def ssh_argv(self, role, remote_argv):
        if self.cfg.get("local_mode"):  # local rehearsal of the drivers: "remote" commands run on this machine
            return ["bash", "-c", shlex.join(remote_argv)]
        return ["ssh", *self.ssh_opts(), "root@" + self.ip(role), shlex.join(remote_argv)]

    def ssh(self, role, remote_argv, *, timeout=120, check=True, fake=""):
        return self.run(self.ssh_argv(role, remote_argv), timeout=timeout, check=check, fake=fake)

    def scp_to(self, role, local, remote, timeout=900):
        self.run(["scp", *self.ssh_opts(), str(local), "root@%s:%s" % (self.ip(role), remote)], timeout=timeout)


# ------------------------------------------------------------------------------------------ prices / cost
def load_prices(cfg, ctx=None):
    """Hourly gross EUR for the two server types and the IPv4: from the saved response of `hcloud api /pricing` (config pricing_file)
    or a live GET (config live_pricing)."""
    path = Path(cfg.get("pricing_file") or "pricing.json")
    if cfg.get("live_pricing") and ctx is not None and not ctx.dry:
        _, raw, _ = ctx.hc("api", "/pricing", "--method", "GET", read_only=True)
        doc = json.loads(raw)
    else:
        doc = json.loads(path.read_text())
    pr = doc["pricing"]
    if pr["currency"] != "EUR":
        raise Stop("pricing currency is not EUR")
    loc = cfg["location"]

    def server(name):
        t = [x for x in pr["server_types"] if x["name"] == name]
        p = [x for x in t[0]["prices"] if x["location"] == loc]
        return Decimal(p[0]["price_hourly"]["gross"])
    ip = [x for x in pr["primary_ips"] if x["type"] == "ipv4"][0]
    ipv4 = Decimal([x for x in ip["prices"] if x["location"] == loc][0]["price_hourly"]["gross"])
    return {"node": server(cfg["node_type"]), "client": server(cfg["client_type"]), "ipv4": ipv4,
            "source": str(path) if not cfg.get("live_pricing") else "live GET /pricing"}


def cost_table(cfg, ctx=None):
    pr = load_prices(cfg, ctx)
    hours_deadline = Decimal(cfg["deadline_minutes"]) / 60
    # Hourly billing: every started hour is billed.  Allow a cleanup margin after the deadline.
    margin_min = cfg.get("cleanup_margin_minutes", 10)
    billed = math.ceil((cfg["deadline_minutes"] + margin_min) / 60)
    per_node = pr["node"] + pr["ipv4"]
    per_client = pr["client"] + pr["ipv4"]
    roles = cfg.get("roles", list(ROLES))
    n_nodes, n_clients = len([r for r in roles if r.startswith("node")]), len([r for r in roles if r.startswith("client")])
    hourly = n_nodes * per_node + n_clients * per_client
    worst = hourly * billed
    rows = [("servers", "%d x %s (nodes) + %d x %s (clients), %s" % (n_nodes, cfg["node_type"], n_clients, cfg["client_type"], cfg["location"])),
            ("price per node-hour", "%s EUR (server %s + IPv4 %s, gross)" % (per_node, pr["node"], pr["ipv4"])),
            ("price per client-hour", "%s EUR (server %s + IPv4 %s, gross)" % (per_client, pr["client"], pr["ipv4"])),
            ("all six, per hour", "%s EUR" % hourly.quantize(Decimal("0.000001"))),
            ("deadline (watchdog deletes)", "%d min after the first server is created (soft stop for new runs: %d min)" % (cfg["deadline_minutes"], cfg["soft_cutoff_minutes"])),
            ("billed hours, worst case", "%d (deadline %d min + %d min cleanup margin, rounded UP per started hour)" % (billed, cfg["deadline_minutes"], margin_min)),
            ("worst-case cost", "%s EUR" % worst.quantize(Decimal("0.000001"))),
            ("traffic", "included traffic per server is far above this run's use (private network); reserve 0 EUR; public counters recorded"),
            ("cap given on the command line", "%s EUR" % cfg["cap_eur"]),
            ("price source", pr["source"])]
    return worst, rows, billed


def print_cost(ctx, cap):
    worst, rows, billed = cost_table(ctx.cfg, ctx)
    ctx.say("== COST (nothing is created by this command) ==")
    for k, v in rows:
        ctx.say("  %-30s %s" % (k, v))
    if worst > Decimal(str(cap)):
        raise Stop("REFUSED: worst-case %s EUR exceeds the cap %s EUR" % (worst, cap))
    ctx.say("  -> within cap")
    return worst


# ------------------------------------------------------------------------------------------ manifests (verbatim from the proven run)
def render_fdb_cluster_yaml(name, stage_hosts, profile="full", layout=None):
    """FoundationDBCluster for stage N.  profile=full is byte-identical to the proven run's manifest
    (checked by `selftest`); profile=small only shrinks memory/CPU/disk numbers for the cheap rehearsal."""
    if profile == "full":  # (small2 below)
        gen = dict(spp=6, g_lim=("4", "4Gi"), g_req=("100m", "256Mi"), g_pvc="16G",
                   s_params=("memory=4GiB", "cache_memory=2GiB"), s_lim=("16", "32Gi"), s_req=("500m", "2Gi"), s_pvc="64G",
                   l_params=("memory=4GiB", "cache_memory=128MiB"), l_lim=("4", "6Gi"), l_req=("200m", "1Gi"), l_pvc="32G")
    elif profile == "small":
        gen = dict(spp=2, g_lim=("1", "768Mi"), g_req=("50m", "128Mi"), g_pvc="2G",
                   s_params=("memory=768MiB", "cache_memory=256MiB"), s_lim=("2", "2Gi"), s_req=("100m", "512Mi"), s_pvc="8G",
                   l_params=("memory=512MiB", "cache_memory=64MiB"), l_lim=("1", "1Gi"), l_req=("50m", "256Mi"), l_pvc="4G")
    elif profile == "small2":
        # rehearsal 2: storage 1.25 GiB per process (2 per pod), logs 1 GiB; sized for 8 GB nodes (cx43 was unavailable)
        gen = dict(spp=2, g_lim=("1", "768Mi"), g_req=("50m", "128Mi"), g_pvc="2G",
                   s_params=("memory=1280MiB", "cache_memory=256MiB"), s_lim=("4", "3584Mi"), s_req=("100m", "1Gi"), s_pvc="16G",
                   l_params=("memory=1GiB", "cache_memory=64MiB"), l_lim=("2", "1536Mi"), l_req=("50m", "512Mi"), l_pvc="8G")
    else:
        raise Stop("unknown k8s profile " + profile)
    n_res, n_cp, n_stateless = stage_hosts, 2 * stage_hosts, 4 * stage_hosts + 1
    if layout in ("B", "C"):
        # layout B: 12 storage server processes per node (spp=12 in one pod) instead of 6; per-process memory 3 GiB (cache 1.5 GiB) instead of 4 / 2 GiB,
        # so the 12 processes stay near 36 GiB of the node's 64 GB (pod limit 44 GiB, request 8 GiB); PVC 128G for the pod.
        gen.update(spp=12, s_params=("memory=3GiB", "cache_memory=1536MiB"), s_lim=("16", "44Gi"), s_req=("1", "8Gi"), s_pvc="128G")
    if layout == "C":
        # layout C = B + twice the resolvers (2N) + twice the commit proxies (4N); the stateless class grows by the same number of processes
        n_res, n_cp = 2 * stage_hosts, 4 * stage_hosts
        n_stateless = 4 * stage_hosts + 1 + (n_res - stage_hosts) + (n_cp - 2 * stage_hosts)
    elif layout not in (None, "A", "B"):
        raise Stop("unknown layout " + str(layout))
    return f"""apiVersion: v1
kind: ServiceAccount
metadata:
  name: fdb-kubernetes
  namespace: default
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: fdb-kubernetes
  namespace: default
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get", "watch", "update", "patch", "list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: fdb-kubernetes
  namespace: default
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: fdb-kubernetes
subjects:
- kind: ServiceAccount
  name: fdb-kubernetes
  namespace: default
---
apiVersion: apps.foundationdb.org/v1beta2
kind: FoundationDBCluster
metadata:
  name: {name}
  namespace: default
spec:
  version: {FDB_VERSION}
  imageType: split
  storageServersPerPod: {gen['spp']}
  automationOptions:
    replacements:
      enabled: true
  faultDomain:
    key: kubernetes.io/hostname
  labels:
    filterOnOwnerReference: false
    matchLabels:
      foundationdb.org/fdb-cluster-name: {name}
    processClassLabels:
    - foundationdb.org/fdb-process-class
    processGroupIDLabels:
    - foundationdb.org/fdb-process-group-id
  minimumUptimeSecondsForBounce: 20
  databaseConfiguration:
    redundancy_mode: single
    storage_engine: ssd-redwood-1
    logs: {2 * stage_hosts}
    commit_proxies: {n_cp}
    grv_proxies: {stage_hosts}
    resolvers: {n_res}
  processCounts:
    storage: {stage_hosts}
    log: {2 * stage_hosts}
    stateless: {n_stateless}
    cluster_controller: 1
  processes:
    general:
      customParameters:
      - trace_format=json
      podTemplate:
        spec:
          nodeSelector:
            otel-role: fdb
          containers:
          - name: foundationdb
            resources:
              limits:
                cpu: "{gen['g_lim'][0]}"
                memory: {gen['g_lim'][1]}
              requests:
                cpu: {gen['g_req'][0]}
                memory: {gen['g_req'][1]}
            securityContext:
              runAsUser: 0
          - name: foundationdb-kubernetes-sidecar
            resources:
              limits:
                cpu: 500m
                memory: 256Mi
              requests:
                cpu: 50m
                memory: 64Mi
            securityContext:
              runAsUser: 0
          serviceAccountName: fdb-kubernetes
      volumeClaimTemplate:
        spec:
          resources:
            requests:
              storage: {gen['g_pvc']}
    storage:
      customParameters:
      - trace_format=json
      - {gen['s_params'][0]}
      - {gen['s_params'][1]}
      podTemplate:
        spec:
          nodeSelector:
            otel-role: fdb
          containers:
          - name: foundationdb
            resources:
              limits:
                cpu: "{gen['s_lim'][0]}"
                memory: {gen['s_lim'][1]}
              requests:
                cpu: {gen['s_req'][0]}
                memory: {gen['s_req'][1]}
            securityContext:
              runAsUser: 0
          - name: foundationdb-kubernetes-sidecar
            resources:
              limits:
                cpu: 500m
                memory: 256Mi
              requests:
                cpu: 50m
                memory: 64Mi
            securityContext:
              runAsUser: 0
          serviceAccountName: fdb-kubernetes
      volumeClaimTemplate:
        spec:
          resources:
            requests:
              storage: {gen['s_pvc']}
    log:
      customParameters:
      - trace_format=json
      - {gen['l_params'][0]}
      - {gen['l_params'][1]}
      podTemplate:
        spec:
          nodeSelector:
            otel-role: fdb
          containers:
          - name: foundationdb
            resources:
              limits:
                cpu: "{gen['l_lim'][0]}"
                memory: {gen['l_lim'][1]}
              requests:
                cpu: {gen['l_req'][0]}
                memory: {gen['l_req'][1]}
            securityContext:
              runAsUser: 0
          - name: foundationdb-kubernetes-sidecar
            resources:
              limits:
                cpu: 500m
                memory: 256Mi
              requests:
                cpu: 50m
                memory: 64Mi
            securityContext:
              runAsUser: 0
          serviceAccountName: fdb-kubernetes
      volumeClaimTemplate:
        spec:
          resources:
            requests:
              storage: {gen['l_pvc']}
  routing:
    defineDNSLocalityFields: false
    useDNSInClusterFile: false
  sidecarContainer:
    enableLivenessProbe: true
    enableReadinessProbe: false
  useExplicitListenAddress: true
"""


def operator_install_script():
    return f"""set -euo pipefail
mkdir -p /srv/k8s-fdb
cd /srv/k8s-fdb
curl -fsSL https://raw.githubusercontent.com/FoundationDB/fdb-kubernetes-operator/{FDB_OPERATOR_VERSION}/config/crd/bases/apps.foundationdb.org_foundationdbclusters.yaml -o crd-clusters.yaml
curl -fsSL https://raw.githubusercontent.com/FoundationDB/fdb-kubernetes-operator/{FDB_OPERATOR_VERSION}/config/crd/bases/apps.foundationdb.org_foundationdbbackups.yaml -o crd-backups.yaml
curl -fsSL https://raw.githubusercontent.com/FoundationDB/fdb-kubernetes-operator/{FDB_OPERATOR_VERSION}/config/crd/bases/apps.foundationdb.org_foundationdbrestores.yaml -o crd-restores.yaml
curl -fsSL https://raw.githubusercontent.com/FoundationDB/fdb-kubernetes-operator/{FDB_OPERATOR_VERSION}/config/samples/deployment.yaml -o operator.yaml
sed -i 's|foundationdb/fdb-kubernetes-monitor:7.3.[0-9]*|foundationdb/fdb-kubernetes-monitor:{FDB_VERSION}|g' operator.yaml
k3s kubectl apply --server-side -f crd-clusters.yaml
k3s kubectl apply --server-side -f crd-backups.yaml
k3s kubectl apply --server-side -f crd-restores.yaml
k3s kubectl apply -f operator.yaml
k3s kubectl patch deployment fdb-kubernetes-operator-controller-manager -p '{{"spec":{{"template":{{"spec":{{"nodeSelector":{{"kubernetes.io/hostname":"'"$(hostname)"'"}}}}}}}}}}'
k3s kubectl rollout status deployment/fdb-kubernetes-operator-controller-manager --timeout=180s
"""


# ------------------------------------------------------------------------------------------ jobs (byte-identical to the Stage 0 harness)
def workload_json(cfg):
    """The sealed Point-2 workload, or config.workload (verbatim string), or Point 2 with consumers/producers/queue overridden
    by config.workload_params (queue capacity scales with consumers/8 unless given)."""
    if cfg.get("workload"):
        return cfg["workload"]
    p = cfg.get("workload_params")
    if not p:
        return POINT2_WORKLOAD
    w = json.loads(POINT2_WORKLOAD)
    w.update({k: v for k, v in p.items() if k in w})
    if "queue_capacity_spans" not in p:
        w["queue_capacity_spans"] = 10000 * w["consumers"] // 8
    return json.dumps(w, separators=(",", ":"))


def job_text(fields, workload=None):
    body = json.dumps(fields, separators=(",", ":"), allow_nan=False)
    if workload is None:
        return body
    json.loads(workload)
    return body[:-1] + ',"workload":' + workload + "}"


class Builds:
    """Executable identities from config.builds (swap the whole block when the final #1048 builds arrive)."""

    def __init__(self, cfg):
        self.cfg = cfg
        self.b = cfg["builds"]

    def exe_name(self, mode, arm):
        if mode == "ingest" and arm == "go_native":
            return "control_native_binary"
        if mode == "ingest" and arm == "rust_native":
            return "rust_binary"
        if mode == "verify" and self.b.get("verifier_binary"):
            return "verifier_binary"
        return "control_pure_binary"

    def sha(self, name):
        return self.b[name]["sha256"]

    def exe_path(self, name):
        return "%s/bin/%s/%s" % (REMOTE, name, self.b[name]["executable_relative_path"])

    def client_library(self, arm):
        if arm == "go_native":
            return "%s/bin/control_native_binary/%s" % (REMOTE, self.b["control_native_binary"]["client_library_relative_path"])
        if arm == "rust_native":
            return REMOTE + "/bin/rust_client/libfdb_c.so"
        return None


def make_job(cfg, builds, mode, arm, *, count, output, corpus_file, corpus_sha, live=None, timeout=None):
    name = builds.exe_name(mode, arm)
    fields = {"schema": "otel_paired_job_v1", "mode": mode, "arm": arm, "binary_sha256": builds.sha(name),
              "source_revision": cfg["source_revision"], "source_manifest_sha256": cfg["source_manifest_sha256"],
              "timeout_seconds": timeout or TIMEOUTS.get(mode), "output": output,
              "corpus_config": {"generator": "otel_fixture_v2", "seed": 283, "count": count, "tenants": 1,
                                "attributes": 8, "match_every": cfg.get("match_every", 3)},
              "expected_frames": count, "row_budget": count, "corpus_file": corpus_file}
    if mode == "corpus":
        return name, job_text(fields)
    library = builds.client_library(arm)
    fields.update({"run": live["run"], "cluster_file": live["cluster_file"], "cluster_sha256": live["cluster_sha256"],
                   "client_backend": "pure_go" if arm == "go_pure" else "native_c", "client_library": library,
                   "client_library_sha256": CLIENT_SHA256 if library else None, "corpus_sha256": corpus_sha,
                   "writer_batch_size": 64, "writer_workers": cfg.get("writer_workers", 1), "record_compression": "zlib-1",
                   "enable_message_deltas": False, "enable_message_full_text": False, "bulk_index_conflicts": False,
                   "layout_file": live["layout_file"]})
    if mode in ("ingest", "verify"):
        fields["layout_sha256"] = live["layout_sha256"]
    if mode == "verify":
        fields["verification"] = dict(live["verification"])
    return name, job_text(fields, workload_json(cfg))


def launch_env(builds, name):
    """Exactly the proven launch environment, plus GOMAXPROCS=16 for both Go variants (as in the reference run:
    no CPU cap); Rust gets nothing; the native Go binary gets the sealed client directory as loader path."""
    env = {"PATH": "/usr/bin:/bin", "HOME": "/root"}
    if name == "control_native_binary":
        lib = builds.client_library("go_native")
        env.update(GOMAXPROCS="16", LD_LIBRARY_PATH=str(Path(lib).parent))
    elif name in ("control_pure_binary", "verifier_binary"):
        env["GOMAXPROCS"] = "16"
    return env


WRAPPER = r"""
import json, os, pathlib, subprocess, sys, time
spec = json.loads(sys.argv[1]); w = pathlib.Path(spec["work"]); w.mkdir(parents=True, exist_ok=True)
(w / "ready.json").write_text('{"ready":true}\n')
deadline = time.time() + spec["barrier_s"]
while not (w / "release.json").exists():
    if time.time() > deadline: sys.exit("barrier timeout")
    time.sleep(0.01)
t0 = time.time()
r = subprocess.run(spec["argv"], env=spec["env"], cwd=str(w))
(w / "exit.json").write_text(json.dumps({"rc": r.returncode, "start": t0, "end": time.time()}))
sys.exit(r.returncode)
"""


# ------------------------------------------------------------------------------------------ provisioning
def render_cloud_init(ctx, role, hostkey_priv, hostkey_pub, authorized):
    host = "%s-%s" % (ctx.campaign, role)
    priv = "\n".join("    " + line for line in hostkey_priv.strip().splitlines())
    auth = "\n".join("  - " + a for a in authorized)
    return f"""#cloud-config
# Generated by lean-run/leanrun.py for {host}. Contains this host's own SSH host private key: keep private.
hostname: {host}
ssh_pwauth: false
disable_root: false
ssh_deletekeys: true
ssh_genkeytypes: [ed25519]
ssh_keys:
  ed25519_private: |
{priv}
  ed25519_public: {hostkey_pub.strip()}
ssh_authorized_keys:
{auth}
package_update: true
package_upgrade: false
packages: [python3, chrony, jq]
runcmd:
  - [sh, -c, 'systemctl disable --now apt-daily.timer apt-daily-upgrade.timer || true']
  - [sh, -c, 'systemctl disable --now unattended-upgrades.service || true']
  - [sh, -c, 'install -d -m 0755 /srv/fdb-bench && date -u +%Y-%m-%dT%H:%M:%SZ > /srv/fdb-bench/cloud-init-done']
"""


def gen_keys(ctx):
    """Controller key, internal fan-out key, per-host ed25519 host keys (so known_hosts is pinned, as in the proven run)."""
    d = ctx.state
    keys = {}
    if ctx.dry:
        for n in ("controller-key", "internal-key"):
            ctx.say("DRY  ssh-keygen -t ed25519 -N '' -C %s -f %s/%s" % (ctx.campaign, d, n))
        for r in ctx.roles:
            ctx.say("DRY  ssh-keygen -t ed25519 -N '' -C root@%s-%s -f %s/hostkeys/%s" % (ctx.campaign, r, d, r))
        return None
    (d / "hostkeys").mkdir(exist_ok=True, mode=0o700)
    for name, path, comment in [("controller", d / "controller-key", ctx.campaign), ("internal", d / "internal-key", ctx.campaign + "-internal")] + \
            [(r, d / "hostkeys" / r, "root@%s-%s" % (ctx.campaign, r)) for r in ctx.roles]:
        if not path.exists():
            ctx.run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", comment, "-f", str(path)])
        keys[name] = path
    return keys


def provision(ctx):
    cfg, camp = ctx.cfg, ctx.campaign
    lab = ["--label", ctx.label, "--label", "deadline=%d" % ctx.s.get("deadline_epoch", 0)]
    if not cfg.get("controller_cidr") or cfg["controller_cidr"].startswith("<"):
        if not ctx.dry:
            raise Stop("config.controller_cidr (this machine's public IP as /32) is required")
    keys = gen_keys(ctx)
    ctx.say("== provision: network, placement group, firewall, ssh key, 6 servers (label %s) ==" % ctx.label)
    hc = ctx.hc
    resume = bool(os.environ.get("LEAN_RESUME")) and bool(ctx.s.get("hosts"))
    if resume:
        ctx.say("   RESUME: infrastructure and existing servers from state are kept; only missing roles are created")
    nid = ctx.s.setdefault("ids", {})
    if not resume:
        _, out, _ = hc("network", "create", "--name", camp + "-net", "--ip-range", NETWORK_CIDR, *lab, fake="network 1111 created")
        net = json.loads(ctx.run([ctx.hcloud, "network", "list", "-l", ctx.label, "-o", "json"], fake='[{"id":1111}]', read_only=True)[1] or "[]")
        nid["network"] = net[0]["id"] if net else 1111
        hc("network", "add-subnet", str(nid["network"]), "--network-zone", "eu-central", "--type", "cloud", "--ip-range", NETWORK_CIDR)
        for g in range((len(ctx.roles) + 9) // 10):          # a spread placement group holds at most 10 servers
            hc("placement-group", "create", "--name", "%s-spread-%d" % (camp, g), "--type", "spread", *lab)
        rules = [{"description": "controller SSH only", "direction": "in", "port": "22", "protocol": "tcp", "source_ips": [cfg["controller_cidr"]]},
                 {"direction": "in", "protocol": "icmp", "source_ips": [cfg["controller_cidr"]]}]
        rules_file = ctx.state / "firewall-rules.json"
        if not ctx.dry:
            rules_file.write_text(json.dumps(rules, indent=1))
        hc("firewall", "create", "--name", camp + "-fw", "--rules-file", str(rules_file), *lab)
        pub = (ctx.state / "controller-key.pub")
        hc("ssh-key", "create", "--name", camp + "-key", "--public-key-from-file", str(pub), *lab)
        for kind, key in (("placement-group", "pg"), ("firewall", "fw"), ("ssh-key", "key")):
            items = ctx.hc_list(kind)
            nid[key] = items[0]["id"] if items else {"pg": 2222, "fw": 3333, "key": 4444}[key]
        nid["pgs"] = [i["id"] for i in sorted(ctx.hc_list("placement-group"), key=lambda i: i["name"])] or [2222 + g for g in range((len(ctx.roles) + 9) // 10)]
    ctx.save()
    authorized = [(ctx.state / "controller-key.pub").read_text().strip() if not ctx.dry else "<controller public key>",
                  (ctx.state / "internal-key.pub").read_text().strip() if not ctx.dry else "<internal public key>"]
    hosts = ctx.s.setdefault("hosts", {})
    kh_lines = []
    for role in ctx.roles:
        typ = cfg["node_type"] if role.startswith("node") else cfg["client_type"]
        if ctx.colocate and role.startswith("client"):
            hosts[role] = dict(hosts["node" + role[len("client"):]], alias_of="node" + role[len("client"):])
            ctx.save()
            continue
        if resume and hosts.get(role, {}).get("id"):
            pubkey = (ctx.state / "hostkeys" / (role + ".pub")).read_text().split()[:2]
            kh_lines.append("%s %s %s" % (hosts[role]["ip"], *pubkey))
            kh_lines.append("%s %s %s" % (PRIVATE_IP[role], *pubkey))
            continue
        ud = ctx.state / "cloud-init" / (role + ".user-data")
        if not ctx.dry:
            ud.parent.mkdir(exist_ok=True, mode=0o700)
            hk = ctx.state / "hostkeys" / role
            ud.write_text(render_cloud_init(ctx, role, hk.read_text(), (ctx.state / "hostkeys" / (role + ".pub")).read_text(), authorized))
            os.chmod(ud, 0o600)
        ctx.log("create_server", role=role)
        for attempt in range(12):
            rc, _, err = hc("server", "create", "--name", "%s-%s" % (camp, role), "--type", typ, "--location", cfg["location"],
                            "--image", cfg["image"], "--ssh-key", str(nid["key"]), "--firewall", str(nid["fw"]),
                            "--placement-group", str(nid["pgs"][ctx.roles.index(role) // 10]), "--without-ipv6", "--start-after-create=false",
                            "--user-data-from-file", str(ud), *lab, "--label", "role=" + role, check=False)
            if rc == 0 or ctx.dry:
                break
            if "resource_unavailable" in err or "placement" in err:
                ctx.say("   server create %s: no capacity right now (attempt %d/12), retrying in 30 s" % (role, attempt + 1))
                time.sleep(30)
                continue
            raise Stop("server create failed: " + err[-400:])
        else:
            raise Stop("server create %s: no capacity after 12 attempts" % role)
        _, out, _ = hc("server", "describe", "%s-%s" % (camp, role), "-o", "json", read_only=True,
                       fake=json.dumps({"id": 9000 + ctx.roles.index(role), "public_net": {"ipv4": {"id": 8000 + ctx.roles.index(role), "ip": "203.0.113.%d" % (10 + ctx.roles.index(role))}}}))
        srv = json.loads(out) if out.strip() else {}
        hosts[role] = {"id": srv.get("id"), "ip": srv.get("public_net", {}).get("ipv4", {}).get("ip"),
                       "ip_id": srv.get("public_net", {}).get("ipv4", {}).get("id"), "private_ip": PRIVATE_IP[role], "type": typ}
        ctx.save()
        if hosts[role]["ip_id"]:
            hc("primary-ip", "add-label", str(hosts[role]["ip_id"]), ctx.label, check=False)
        hc("api", "/servers/%s/actions/attach_to_network" % hosts[role]["id"], "--method", "POST", "--data",
           json.dumps({"ip": PRIVATE_IP[role], "network": nid["network"]}))
        time.sleep(0 if ctx.dry else 8)  # the proven run polled the network action; a fixed pause + the poweron retry loop below is the lean equivalent
        for attempt in range(8):
            rc, _, err = hc("server", "poweron", str(hosts[role]["id"]), check=False)
            if rc == 0 or ctx.dry:
                break
            time.sleep(3)
        else:
            raise Stop("poweron failed for " + role)
        if not ctx.dry:
            pubkey = (ctx.state / "hostkeys" / (role + ".pub")).read_text().split()[:2]
            kh_lines.append("%s %s %s" % (hosts[role]["ip"], *pubkey))
            kh_lines.append("%s %s %s" % (PRIVATE_IP[role], *pubkey))
    if not ctx.dry:
        ctx.known_hosts.write_text("\n".join(kh_lines) + "\n")
    ctx.say("== waiting for ssh + cloud-init on all %d ==" % len(ctx.roles))
    for role in ctx.real_roles:
        for attempt in range(60):
            rc, _, _ = ctx.ssh(role, ["test", "-f", "/srv/fdb-bench/cloud-init-done"], check=False, timeout=30)
            if rc == 0:
                break
            time.sleep(0 if ctx.dry else 10)
        else:
            raise Stop("host %s did not come up" % role)
    ctx.log("provisioned", hosts=hosts)


# ------------------------------------------------------------------------------------------ upload
def upload(ctx):
    cfg = ctx.cfg
    A = cfg["assets"]
    ctx.say("== upload assets: controller -> client1 (public), client1 -> client2/3 (private network); fdbcli -> node1 ==")
    exes = [n for n in ("control_pure_binary", "control_native_binary", "rust_binary", "verifier_binary") if cfg["builds"].get(n)]
    counts = [] if cfg.get("corpora_on_client") else sorted({c for c in (cfg["warm_count"], cfg["measured_count"]) if c > 0})
    files = [(cfg["builds"][n]["deployment_tar"], cfg["builds"][n]["deployment_sha256"], n + ".tar") for n in exes]
    files.append((A["libfdb_c"]["path"], A["libfdb_c"]["sha256"], "libfdb_c.so"))
    for count in counts:
        files.append((A["corpus_%d" % count]["path"], A["corpus_%d" % count]["sha256"], "corpus-%d.bin" % count))
    for role in ctx.clients + ["node1"]:
        ctx.ssh(role, ["mkdir", "-p", REMOTE + "/assets", REMOTE + "/bin", REMOTE + "/corpus", REMOTE + "/runs"])
    ctx.scp_to("node1", A["fdbcli"]["path"], REMOTE + "/bin/fdbcli")
    ctx.scp_to("client1", A["fdbcli"]["path"], REMOTE + "/bin/fdbcli")
    ctx.ssh("node1", ["chmod", "755", REMOTE + "/bin/fdbcli"])
    ctx.ssh("client1", ["chmod", "755", REMOTE + "/bin/fdbcli"])
    ctx.scp_to("client1", ctx.state / "internal-key", "/root/.ssh/internal-key")
    ctx.scp_to("client1", ctx.known_hosts, "/root/.ssh/known_hosts_run")
    ctx.ssh("client1", ["chmod", "600", "/root/.ssh/internal-key"])
    for path, sha, remote_name in files:
        ctx.scp_to("client1", path, "%s/assets/%s" % (REMOTE, remote_name))
    fan = ("scp -i /root/.ssh/internal-key -o StrictHostKeyChecking=yes -o UserKnownHostsFile=/root/.ssh/known_hosts_run "
           "-o BatchMode=yes %s/assets/* root@%%s:%s/assets/" % (REMOTE, REMOTE))
    for role in ctx.clients[1:]:
        ctx.ssh("client1", ["bash", "-c", fan % PRIVATE_IP[role]], timeout=900)
    for role in ctx.clients:
        for path, sha, remote_name in files:
            out = ctx.ssh(role, ["sha256sum", "%s/assets/%s" % (REMOTE, remote_name)], fake=sha + "  x")[1]
            if not ctx.dry and out.split()[0] != sha:
                raise Stop("asset hash mismatch on %s: %s" % (role, remote_name))
        script = ["set -euo pipefail", "cd " + REMOTE]
        for n in exes:
            script.append("mkdir -p bin/%s && tar -xf assets/%s.tar -C bin/%s" % (n, n, n))
        script.append("mkdir -p bin/rust_client && install -m 0644 assets/libfdb_c.so bin/rust_client/libfdb_c.so")
        for count in counts:
            script.append("ln -sf %s/assets/corpus-%d.bin corpus/%d.bin" % (REMOTE, count, count))
        ctx.ssh(role, ["bash", "-c", "\n".join(script)], timeout=600)
        checks = [(cfg["builds"][n]["sha256"], ctx.builds.exe_path(n)) for n in exes]
        checks += [(CLIENT_SHA256, ctx.builds.client_library("go_native")), (CLIENT_SHA256, ctx.builds.client_library("rust_native"))]
        for sha, p in checks:
            out = ctx.ssh(role, ["sha256sum", p], fake=sha + "  x")[1]
            if not ctx.dry and out.split()[0] != sha:
                raise Stop("installed file hash mismatch on %s: %s" % (role, p))
    ctx.log("uploaded")


# ------------------------------------------------------------------------------------------ k3s / cilium / operator
def bootstrap(ctx):
    cfg = ctx.cfg
    ctx.say("== k3s server on node1, agents on the other five, VPC routes, Cilium, images, operator (as in k8s-cilium4) ==")
    n1 = PRIVATE_IP["node1"]
    install_server = (f"curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION='{K3S_VERSION}' sh -s - server --node-ip={n1} "
                      f"--advertise-address={n1} --flannel-backend=none --disable-network-policy --disable=traefik,servicelb "
                      f"--write-kubeconfig-mode=0644")
    ctx.ssh("node1", ["bash", "-lc", install_server], timeout=300)
    token = ctx.ssh("node1", ["cat", "/var/lib/rancher/k3s/server/node-token"], fake="K3STOKEN")[1].strip()

    def join(role):
        cmd = (f"curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION='{K3S_VERSION}' K3S_URL='https://{n1}:6443' "
               f"K3S_TOKEN='{token}' sh -s - agent --node-ip={PRIVATE_IP[role]}")
        ctx.ssh(role, ["bash", "-lc", cmd], timeout=300)
    with cf.ThreadPoolExecutor(max(1, len(ctx.real_roles) - 1)) as ex:
        list(ex.map(join, [r for r in ctx.real_roles if r != "node1"]))
    ctx.say("-- Hetzner VPC routes for the node PodCIDRs --")
    cidrs = {}
    for _ in range(60):
        out = ctx.ssh("node1", ["k3s", "kubectl", "get", "nodes", "-o", "json"], fake=json.dumps({"items": [
            {"metadata": {"name": "%s-%s" % (ctx.campaign, r)}, "spec": {"podCIDR": "10.42.%d.0/24" % i},
             "status": {"addresses": [{"type": "InternalIP", "address": PRIVATE_IP[r]}]}} for i, r in enumerate(ctx.roles)]}))[1]
        for item in json.loads(out).get("items", []):
            cidr = item.get("spec", {}).get("podCIDR", "")
            ip = next((a["address"] for a in item["status"].get("addresses", []) if a["type"] == "InternalIP"), "")
            if cidr and ip:
                cidrs[item["metadata"]["name"]] = (cidr, ip)
        if len(cidrs) == len(ctx.real_roles):
            break
        time.sleep(0 if ctx.dry else 2)
    if len(cidrs) != len(ctx.real_roles):
        raise Stop("did not get all 6 podCIDRs: %s" % cidrs)
    for name, (cidr, ip) in sorted(cidrs.items()):
        ctx.hc("network", "add-route", str(ctx.s.get("ids", {}).get("network", 1111)), "--destination", cidr, "--gateway", ip, timeout=60)
    route_cmd = ("iface=$(ip -o -4 addr show | awk '$4 ~ /^10\\.99\\.0\\./ {print $2}'); "
                 "ip route replace 10.42.0.0/16 via 10.99.0.1 dev \"$iface\"")
    for role in ctx.real_roles:
        ctx.ssh(role, ["bash", "-lc", route_cmd], timeout=60)
    cilium = ("export KUBECONFIG=/etc/rancher/k3s/k3s.yaml; "
              f"curl -fsSL https://github.com/cilium/cilium-cli/releases/download/{CILIUM_CLI}/cilium-linux-amd64.tar.gz | tar xz -C /usr/local/bin cilium; "
              "cilium install --set ipam.mode=kubernetes --set routingMode=native --set ipv4NativeRoutingCIDR=10.42.0.0/16 "
              "--set autoDirectNodeRoutes=false --set bpf.masquerade=true --set operator.replicas=1; "
              "cilium status --wait --wait-duration=300s")
    ctx.ssh("node1", ["bash", "-lc", cilium], timeout=420)
    pull = (f"for img in foundationdb/foundationdb:{FDB_VERSION} foundationdb/foundationdb-kubernetes-sidecar:{FDB_VERSION}-1 "
            f"foundationdb/fdb-kubernetes-monitor:{FDB_VERSION} foundationdb/fdb-kubernetes-operator:{FDB_OPERATOR_VERSION}; "
            "do k3s ctr images pull docker.io/$img >/dev/null; done")
    with cf.ThreadPoolExecutor(max(1, len(ctx.nodes))) as ex:
        list(ex.map(lambda r: ctx.ssh(r, ["bash", "-lc", pull], timeout=420), ctx.nodes))
    ctx.ssh("node1", ["bash", "-lc", operator_install_script()], timeout=360)
    ctx.log("bootstrapped")


# ------------------------------------------------------------------------------------------ FDB status / cluster lifecycle
def fdb_status(ctx, cluster_file):
    raw = ctx.ssh("node1", [REMOTE + "/bin/fdbcli", "-C", cluster_file, "--exec", "status json"], timeout=30,
                  fake=json.dumps({"client": {"coordinators": {"quorum_reachable": True}, "database_status": {"available": True, "healthy": True}},
                                   "cluster": {"database_available": True, "qos": {"worst_queue_bytes_storage_server": 1000, "worst_durability_lag_storage_server": {"seconds": 1.0}},
                                               "data": {"state": {"healthy": True}}, "processes": {}, "workload": {"transactions": {"committed": {"counter": 0}, "conflicted": {"counter": 0}}}}}))[1]
    doc = json.loads(raw)
    c, client = doc["cluster"], doc["client"]
    avail = bool(client.get("database_status", {}).get("available") is True and c.get("database_available") is True)
    qos = c.get("qos") or {}
    queue = qos.get("worst_queue_bytes_storage_server")
    lag = (qos.get("worst_durability_lag_storage_server") or {}).get("seconds")
    healthy = bool(avail and isinstance(queue, int) and isinstance(lag, (int, float))
                   and client["database_status"].get("healthy") and c.get("data", {}).get("state", {}).get("healthy")
                   and not c.get("messages") and not client.get("messages"))
    procs = []
    for pid, pr in (c.get("processes") or {}).items():
        mem = pr.get("memory", {})
        procs.append({"addr": pr.get("address"), "class": pr.get("class_type"), "roles": [r.get("role") for r in pr.get("roles", [])],
                      "cpu_cores": (pr.get("cpu") or {}).get("usage_cores"), "rss_bytes": mem.get("rss_bytes"), "used_bytes": mem.get("used_bytes"),
                      "unused_allocated": mem.get("unused_allocated_memory")})
    tx = (c.get("workload") or {}).get("transactions") or {}
    batch_qos = qos.get("batch_performance_limited_by") or {}
    return {"t": time.time(), "healthy": healthy, "queue_bytes": queue if isinstance(queue, int) else 0,
            "lag_seconds": lag if isinstance(lag, (int, float)) else 0.0, "available": avail,
            "committed": (tx.get("committed") or {}).get("counter"), "conflicted": (tx.get("conflicted") or {}).get("counter"),
            "limited_by": (qos.get("performance_limited_by") or {}).get("name"), "batch_limited_by": batch_qos.get("name"),
            "tps_limit": qos.get("transactions_per_second_limit"), "released_tps": (tx.get("started") or {}).get("hz"),
            "started": (tx.get("started") or {}).get("counter"), "partitions": (c.get("data") or {}).get("partitions_count"),
            "moving_in_flight": ((c.get("data") or {}).get("moving_data") or {}).get("in_flight_bytes", 0),
            "moving_in_queue": ((c.get("data") or {}).get("moving_data") or {}).get("in_queue_bytes", 0),
            "processes": procs}


def wait_stable(ctx, cluster_file, cap=300, interval=1.0, need=3, wait_dd=False):
    until, streak, first = time.time() + cap, 0, None
    while True:
        s = fdb_status(ctx, cluster_file)
        ok = s["healthy"] and s["queue_bytes"] < 16 * 1024 * 1024 and s["lag_seconds"] < 10
        if wait_dd:
            ok = ok and s["moving_in_flight"] == 0 and s["moving_in_queue"] == 0
        if ok:
            first = first or time.monotonic_ns()
            streak += 1
            if streak >= need:
                return s, first
        else:
            streak, first = 0, None
        if time.time() > until and not ctx.dry:
            raise Stop("cluster did not stabilise within %ds; last=%s" % (cap, {k: s[k] for k in ("healthy", "queue_bytes", "lag_seconds")}))
        time.sleep(0 if ctx.dry else interval)


def deploy_cluster(ctx, stage):
    cfg = ctx.cfg
    name = "otel-fdb-s%d" % stage
    ctx.say("-- stage %dx: fresh cluster %s (profile %s) --" % (stage, name, cfg["k8s_profile"]))
    ctx.ssh("node1", ["bash", "-lc", "k3s kubectl delete foundationdbcluster --all --wait=true --timeout=120s || true; "
                                    "k3s kubectl delete pvc --all --wait=true --timeout=60s || true"], timeout=240)
    for i in range(1, max(3, len(ctx.nodes)) + 1):
        host = "%s-node%d" % (ctx.campaign, i)
        if i <= stage:
            ctx.ssh("node1", ["k3s", "kubectl", "label", "node", host, "otel-role=fdb", "--overwrite"])
        else:
            ctx.ssh("node1", ["k3s", "kubectl", "label", "node", host, "otel-role-"], check=False)
    manifest = render_fdb_cluster_yaml(name, stage, cfg["k8s_profile"], cfg.get("layout"))
    local = ctx.state / ("fdb-cluster-s%d.yaml" % stage)
    if not ctx.dry:
        local.write_text(manifest)
    ctx.run(["scp", *ctx.ssh_opts(), str(local), "root@%s:/srv/k8s-fdb/fdb-cluster-s%d.yaml" % (ctx.ip("node1"), stage)])
    ctx.ssh("node1", ["k3s", "kubectl", "apply", "-f", "/srv/k8s-fdb/fdb-cluster-s%d.yaml" % stage])
    conn, deadline = None, time.time() + 600
    while True:
        out = ctx.ssh("node1", ["k3s", "kubectl", "get", "foundationdbcluster", name, "-o", "json"], timeout=30,
                      fake=json.dumps({"status": {"configured": True, "generations": {"reconciled": 1}, "connectionString": "otel_fdb_s%d:XXXX@10.42.0.9:4501" % stage,
                                                  "health": {"available": True, "healthy": True}}}))[1]
        st = json.loads(out).get("status", {})
        conn = st.get("connectionString", "")
        if st.get("configured") and st.get("health", {}).get("available") and conn:
            break
        if time.time() > deadline:
            pods = ctx.ssh("node1", ["k3s", "kubectl", "get", "pods", "-o", "wide"], check=False)[1]
            raise Stop("cluster %s did not reconcile in 10 min\n%s" % (name, pods))
        time.sleep(5)
    pods = json.loads(ctx.ssh("node1", ["k3s", "kubectl", "get", "pods", "-l", "foundationdb.org/fdb-cluster-name=" + name, "-o", "json"], timeout=30,
                              fake='{"items":[]}')[1])
    for item in pods.get("items", []):
        ip = item.get("status", {}).get("podIP", "")
        if ip:
            conn = conn.replace("%s.%s.default.svc.cluster.local" % (item["metadata"]["name"], name), ip)
    cf_path = "%s/fdb-s%d.cluster" % (REMOTE, stage)
    local_cf = ctx.state / ("fdb-s%d.cluster" % stage)
    if not ctx.dry:
        local_cf.write_text(conn + "\n")
    for role in ["node1"] + ctx.clients:
        ctx.run(["scp", *ctx.ssh_opts(), str(local_cf), "root@%s:%s" % (ctx.ip(role), cf_path)])
    wait_stable(ctx, cf_path, cap=300, interval=5.0)
    return cf_path, sha256_file(local_cf) if not ctx.dry else "0" * 64, pods


# ------------------------------------------------------------------------------------------ one wave
def stage_producers(ctx, stage):
    return ["client%d" % i for i in range(1, stage + 1)]


def write_remote(ctx, role, path, text):
    if ctx.cfg.get("local_mode"):
        Path(path).parent.mkdir(parents=True, exist_ok=True)
        Path(path).write_text(text)
        return
    ctx.run(["ssh", *ctx.ssh_opts(), "root@" + ctx.ip(role), "mkdir -p %s && cat > %s" % (shlex.quote(str(Path(path).parent)), shlex.quote(path))],
            stdin=text)


def offline_job(ctx, role, mode, arm, run_id, count, live, extra_ok=True, tag=""):
    """setup / verify: one pure Go control invocation on the client, waited for."""
    corpus = "%s/corpus/%d.bin" % (REMOTE, count)
    wdir = "%s/runs/%s" % (REMOTE, run_id)
    output = "%s/%s%s.report.json" % (wdir, mode, tag)
    name, text = make_job(ctx.cfg, ctx.builds, mode, arm, count=count, output=output, corpus_file=corpus,
                          corpus_sha=ctx.cfg["assets"]["corpus_%d" % count]["sha256"], live=live)
    job_path = "%s/%s%s.job.json" % (wdir, mode, tag)
    write_remote(ctx, role, job_path, text)
    env = launch_env(ctx.builds, name)
    envs = ["%s=%s" % kv for kv in env.items()]
    rc_exec, _, err_exec = ctx.ssh(role, ["env", "-i", *envs, ctx.builds.exe_path(name), job_path], timeout=TIMEOUTS[mode] + 60, check=False,
                                   fake="")
    rc, out, _ = ctx.ssh(role, ["cat", output], check=False, fake=json.dumps({"status": "ok", "distinct_spans": count, "layout_sha256": "0" * 64}))
    try:
        report = json.loads(out)
    except ValueError:
        report = {}
    if rc_exec != 0:
        report = {"status": "error", "error": "%s exited %d: %s | report: %s" % (mode, rc_exec, err_exec.strip()[-300:], report.get("error"))}
    return report, job_path, output


def fdbcli_exec(ctx, cluster_file, cmd, timeout=120):
    return ctx.ssh("node1", [REMOTE + "/bin/fdbcli", "-C", cluster_file, "--timeout", str(timeout), "--exec", cmd], timeout=timeout + 30,
                   fake="")[1]


def ingest_blocking(ctx, role, arm, lv, count, tag, timeout):
    """One ingest job run to completion on a client (no barrier). Used by conditioning only."""
    wdir = "%s/runs/%s" % (REMOTE, lv["run"])
    output = "%s/ingest-%s.report.json" % (wdir, tag)
    name, text = make_job(ctx.cfg, ctx.builds, "ingest", arm, count=count, output=output, corpus_file="%s/corpus/%d.bin" % (REMOTE, count),
                          corpus_sha=ctx.cfg["assets"]["corpus_%d" % count]["sha256"], live=lv, timeout=timeout)
    job_path = "%s/ingest-%s.job.json" % (wdir, tag)
    write_remote(ctx, role, job_path, text)
    envs = ["%s=%s" % kv for kv in launch_env(ctx.builds, name).items()]
    rc_exec, _, err_exec = ctx.ssh(role, ["env", "-i", *envs, ctx.builds.exe_path(name), job_path], timeout=timeout + 120, check=False)
    rc, out, _ = ctx.ssh(role, ["cat", output], check=False, fake=json.dumps({"status": "ok"}))
    try:
        rep = json.loads(out)
    except ValueError:
        rep = {}
    if rc_exec != 0:
        rep = {"status": "error", "error": "ingest exited %d: %s | report: %s" % (rc_exec, err_exec.strip()[-300:], rep.get("error"))}
    return rep


def condition_stores(ctx, stage, clients, live, count, cluster_file):
    """Pre-split every producer's real measured store with the SAME tool (Go own client, control-pure) for all variants,
    wait for data distribution to go quiet, then put each store back to exactly its fresh-setup state (see condition.py;
    proven locally in condtest/)."""
    t = {"t0": time.time()}
    snaps = {}
    for role in clients:
        lay = json.loads(ctx.ssh(role, ["cat", live[role]["layout_file"]], fake=json.dumps({"record_prefix": "15131507151f1501"}))[1])
        S = condition.store_prefix(lay)
        snaps[role] = (S, condition.parse_getrange(fdbcli_exec(ctx, cluster_file, condition.getrange_cmd(S, S + b"\xff"))) if not ctx.dry else [])
        ctx.say("   conditioning %s: store prefix %s, %d baseline keys snapshotted" % (role, S.hex(), len(snaps[role][1])))
    def one(role):
        rep = ingest_blocking(ctx, role, "go_pure", live[role], count, "condition", 600)
        if rep.get("status") != "ok":
            raise Stop("conditioning ingest failed on %s: %s" % (role, rep.get("error")))
    with cf.ThreadPoolExecutor(len(clients)) as ex:
        list(ex.map(one, clients))
    t["ingest_s"] = time.time() - t["t0"]
    t1 = time.time()
    wait_stable(ctx, cluster_file, cap=900, interval=2.0, wait_dd=True)
    t["dd_quiet_s"] = time.time() - t1
    sample = fdb_status(ctx, cluster_file)
    t["partitions"] = sample.get("partitions")
    t2 = time.time()
    for role in clients:
        S, snap = snaps[role]
        out = fdbcli_exec(ctx, cluster_file, condition.restore_cmd(S, snap) if not ctx.dry else "<restore>")
        if not ctx.dry and "Committed" not in out:
            raise Stop("restore not committed on %s: %s" % (role, out[-300:]))
    for role in clients:
        S, snap = snaps[role]
        after = condition.parse_getrange(fdbcli_exec(ctx, cluster_file, condition.getrange_cmd(S, S + b"\xff"))) if not ctx.dry else []
        if after != snap:
            raise Stop("store of %s is not back at its fresh-setup state after conditioning" % role)
    wait_stable(ctx, cluster_file, cap=600, interval=2.0)
    t["restore_and_settle_s"] = time.time() - t2
    t["total_s"] = time.time() - t["t0"]
    ctx.log("conditioned", stage=stage, **t)
    return t


def cpu_times(ctx, role):
    """Aggregate cpu line of /proc/stat on a host: (busy jiffies excl. idle/iowait, steal, total)."""
    out = ctx.ssh(role, ["bash", "-c", "head -1 /proc/stat"], fake="cpu 1 0 1 1 0 0 0 0 0 0")[1].split()
    v = [int(x) for x in out[1:]]
    total = sum(v[:8])
    return {"busy": total - v[3] - (v[4] if len(v) > 4 else 0), "steal": v[7] if len(v) > 7 else 0, "total": total}


def run_wave(ctx, stage, arm, wave, count, cluster_file, cluster_sha, rec, condition=False):
    """setup per producer, barrier-released ingest on all producers, drain, verify.  Returns wave record."""
    clients = stage_producers(ctx, stage)
    cfg = ctx.cfg
    timeout = 300 if wave == "warm" else cfg.get("measured_timeout_s", 180)
    w = {"wave": wave, "count_per_producer": count, "producers": len(clients), "clients": {}}
    live = {}
    # 1. tenant setup (outside the measured window), all producers in parallel
    def setup_one(role):
        run_id = ("%s_s%d_%s_%s_%s" % (ctx.campaign.replace("-", ""), stage, arm, wave, role)).replace("_", "")[:40]
        run_name = "lean" + hashlib.sha256((ctx.campaign + ctx.nonce + str(stage) + arm + wave + role).encode()).hexdigest()[:20]
        lv = {"run": run_name, "cluster_file": cluster_file, "cluster_sha256": cluster_sha,
              "layout_file": "%s/runs/%s/layout.json" % (REMOTE, run_name), "verification": cfg.get("verification_job", VERIFICATION_NONE)}
        report, _, _ = offline_job(ctx, role, "setup", arm, run_name, count, lv)
        if not ctx.dry:
            if report.get("status") != "ok":
                raise Stop("setup failed on %s: %s" % (role, report.get("error")))
            lv["layout_sha256"] = ctx.ssh(role, ["sha256sum", lv["layout_file"]])[1].split()[0]
            if report.get("layout_sha256") not in (None, lv["layout_sha256"]):
                raise Stop("layout hash differs from the setup report on %s" % role)
        else:
            lv["layout_sha256"] = "0" * 64
        return role, lv
    with cf.ThreadPoolExecutor(len(clients)) as ex:
        for role, lv in ex.map(setup_one, clients):
            live[role] = lv
    if wave == "measured" and condition:
        ctx.say("   conditioning (pre-split with the fixed Go own-client tool) ...")
        try:
            w["conditioning"] = condition_stores(ctx, stage, clients, live, count, cluster_file)
        except Stop as exc:
            raise CondFail(str(exc)) from exc
    # 2. ingest jobs written, barrier wrappers started over ssh
    procs = {}
    before = fdb_status(ctx, cluster_file)
    for role in clients:
        lv = live[role]
        wdir = "%s/runs/%s" % (REMOTE, lv["run"])
        output = wdir + "/ingest.report.json"
        name, text = make_job(cfg, ctx.builds, "ingest", arm, count=count, output=output, corpus_file="%s/corpus/%d.bin" % (REMOTE, count),
                              corpus_sha=cfg["assets"]["corpus_%d" % count]["sha256"], live=lv, timeout=timeout)
        job_path = wdir + "/ingest.job.json"
        write_remote(ctx, role, job_path, text)
        spec = {"work": wdir + "/barrier", "barrier_s": 120, "argv": [ctx.builds.exe_path(name), job_path], "env": launch_env(ctx.builds, name)}
        argv = ctx.ssh_argv(role, ["python3", "-B", "-c", WRAPPER, json.dumps(spec)])
        if ctx.dry:
            ctx.say("DRY  " + " ".join(shlex.quote(a) for a in argv)[:300] + " ...")
            procs[role] = None
        else:
            so, se = open(ctx.state / ("%s-%s-%s.stdout" % (wave, arm, role)), "ab"), open(ctx.state / ("%s-%s-%s.stderr" % (wave, arm, role)), "ab")
            procs[role] = subprocess.Popen(argv, stdout=so, stderr=se)
        lv["wdir"], lv["output"], lv["name"] = wdir, output, name
    for role in clients:
        for _ in range(240):
            rc, out, _ = ctx.ssh(role, ["cat", live[role]["wdir"] + "/barrier/ready.json"], check=False, fake='{"ready":true}')
            if rc == 0 and out.strip():
                break
            time.sleep(0 if ctx.dry else 0.5)
        else:
            raise Stop("producer %s never reached the barrier" % role)
    # 3. common release; 1 Hz FDB samples until every producer has exited
    cpu0 = {r: cpu_times(ctx, r) for r in clients}
    t0 = time.monotonic_ns()
    t_release = time.time()
    with cf.ThreadPoolExecutor(len(clients)) as ex:
        list(ex.map(lambda r: ctx.ssh(r, ["bash", "-c", "printf '{\"released\":true}\\n' > %s/barrier/release.json" % live[r]["wdir"]]), clients))
    samples, limit = [], time.time() + timeout + 60
    while ctx.dry or any(p.poll() is None for p in procs.values()):
        samples.append(fdb_status(ctx, cluster_file))
        if ctx.dry:
            break
        if time.time() > limit:
            raise Stop("producers still running %ds after release" % (timeout + 60))
        time.sleep(1.0)
    ack_end = time.monotonic_ns()
    t_ack_end = time.time()
    cpu1 = {r: cpu_times(ctx, r) for r in clients}
    samples.append(fdb_status(ctx, cluster_file))
    after_ingest = samples[-1]
    drained_status, first_drained = wait_stable(ctx, cluster_file, cap=300, interval=1.0)
    drained_end = max(ack_end, first_drained)
    # 4. collect producer reports
    fails = []
    for role in clients:
        lv = live[role]
        rc, out, _ = ctx.ssh(role, ["cat", lv["output"]], check=False, fake=json.dumps({"status": "ok", "ingest_wall_ns": 1, "accepted_spans": count}))
        try:
            rep = json.loads(out)
        except ValueError:
            rep = {}
        exitrc = procs[role].returncode if procs[role] is not None else 0
        w["clients"][role] = {"run": lv["run"], "exit": exitrc, "ingest_report_status": rep.get("status"), "ingest_report": rep}
        if exitrc != 0 or rep.get("status") != "ok":
            fails.append("%s: exit %s status %s %s" % (role, exitrc, rep.get("status"), rep.get("error")))
    # 5. verification (full read-back + census [+ oracle]) on each producer, in parallel, after the drain
    def verify_one(role):
        lv = live[role]
        v, _, _ = offline_job(ctx, role, "verify", arm, lv["run"], count, lv)
        if "transaction_too_old" in str(v.get("error")) and not ctx.dry:   # wait for storage lag to drop, then read back once more
            ctx.say("   read-back of %s hit transaction_too_old: waiting for the storage lag to drop, retrying once" % role)
            wait_stable(ctx, cluster_file, cap=300, interval=2.0)
            v, _, _ = offline_job(ctx, role, "verify", arm, lv["run"], count, lv, tag="-retry")
            v["retried_after_transaction_too_old"] = True
        return role, v
    if cfg.get("skip_verify"):
        for role in clients:
            w["clients"][role]["verify_report"] = {"status": "skipped", "distinct_spans": w["clients"][role]["ingest_report"].get("accepted_spans", 0)}
        w["verified"] = False
    else:
        w["verified"] = True
        with cf.ThreadPoolExecutor(len(clients)) as ex:
            for role, v in ex.map(verify_one, clients):
                w["clients"][role]["verify_report"] = v
                if v.get("status") != "ok":
                    fails.append("%s: verify %s %s" % (role, v.get("status"), v.get("error")))
    # 6. figures from verified spans only
    ok = not fails
    distinct = sum(w["clients"][r].get("verify_report", {}).get("distinct_spans", 0) for r in clients) if ok else None
    steal = {r: {"steal_pct": 100.0 * (cpu1[r]["steal"] - cpu0[r]["steal"]) / max(1, cpu1[r]["total"] - cpu0[r]["total"]),
                 "busy_pct_of_host": 100.0 * (cpu1[r]["busy"] - cpu0[r]["busy"]) / max(1, cpu1[r]["total"] - cpu0[r]["total"])} for r in clients}
    w["controller"] = {"client_host_cpu": steal, "t_release": t_release, "t_ack_end": t_ack_end, "ack_window_s": (ack_end - t0) / 1e9, "drained_window_s": (drained_end - t0) / 1e9,
                       "verified_distinct_spans": distinct,
                       "ack_spans_per_s": distinct / ((ack_end - t0) / 1e9) if ok and distinct and ack_end > t0 else None,
                       "drained_spans_per_s": distinct / ((drained_end - t0) / 1e9) if ok and distinct and drained_end > t0 else None,
                       "fdb_committed_delta": (drained_status["committed"] or 0) - (before["committed"] or 0) if before["committed"] is not None else None,
                       "fdb_conflicted_delta": (drained_status["conflicted"] or 0) - (before["conflicted"] or 0) if before["conflicted"] is not None else None,
                       "max_storage_queue_bytes": max((s["queue_bytes"] for s in samples), default=None),
                       "max_lag_seconds": max((s["lag_seconds"] for s in samples), default=None),
                       "unhealthy_samples": sum(1 for s in samples if not s["healthy"])}
    sample_file = ctx.state / "samples" / ("s%d-%s-%s-%s-%s.ndjson" % (stage, arm, wave, cfg.get("run_label") or "base", ctx.nonce))
    if not ctx.dry:
        sample_file.parent.mkdir(exist_ok=True, mode=0o700)
        with open(sample_file, "w") as f:
            for s in [before] + samples:
                f.write(json.dumps(s) + "\n")
    w["status"] = "passed" if ok else "failed"
    w["failures"] = fails
    w["fdb_sample_file"] = str(sample_file)
    return w


def collect_failure_evidence(ctx, stage, arm):
    """Before anything is torn down: pods, describe, last log lines (current and previous container) of every FDB pod."""
    if ctx.dry:
        return
    try:
        script = ("k3s kubectl get pods -o wide; k3s kubectl get events --sort-by=.lastTimestamp | tail -40; "
                  "for p in $(k3s kubectl get pods -o name | grep -E 'storage|log'); do echo \"=== $p describe\"; k3s kubectl describe $p | tail -60; "
                  "echo \"=== $p logs (foundationdb, last 60)\"; k3s kubectl logs $p -c foundationdb --tail=60 2>&1; "
                  "echo \"=== $p logs --previous\"; k3s kubectl logs $p -c foundationdb --previous --tail=60 2>&1; done; "
                  "free -m; dmesg | grep -iE 'out of memory|oom|killed process' | tail -10")
        out = ctx.ssh("node1", ["bash", "-c", script], timeout=180, check=False)[1]
        path = ctx.state / ("failure-evidence-s%d-%s-%d.txt" % (stage, arm, int(time.time())))
        path.write_text(out)
        ctx.say("   failure evidence saved: %s" % path)
    except BaseException as exc:  # never mask the original failure
        ctx.say("   could not collect failure evidence: %s" % exc)


def run_variant(ctx, stage, arm, results, condition=False, reason=None):
    cfg = ctx.cfg
    rec = {"stage": stage, "arm": arm, "label": cfg.get("run_label"), "started": now_iso(), "waves": [], "conditioned": bool(condition),
           "unconditioned_reason": reason}
    results["runs"].append(rec)
    ctx.nonce = "%x" % time.time_ns()          # a fresh run name per run: plan entries can share stage+arm (only the label differs)
    t0 = time.time()
    try:
        cluster_file, cluster_sha, pods = deploy_cluster(ctx, stage)
        rec["cluster_ready_s"] = round(time.time() - t0, 1)
        rec["pods"] = len(pods.get("items", []))
        for wave, count in [x for x in (("warm", cfg["warm_count"]), ("measured", cfg["measured_count"])) if x[1] > 0]:
            ctx.say("-- stage %dx %s: %s wave, %d spans per producer, %d producer(s) --" % (stage, arm, wave, count, stage))
            w = run_wave(ctx, stage, arm, wave, count, cluster_file, cluster_sha, rec, condition=condition)
            rec["waves"].append(w)
            ctx.log("wave", stage=stage, arm=arm, wave=wave, status=w["status"], controller=w["controller"], failures=w["failures"])
            if w["status"] != "passed":
                raise Stop("wave failed: %s" % w["failures"])
        rec["status"] = "passed"
    except BaseException as exc:
        rec["status"] = "failed"
        rec["error"] = str(exc)
        collect_failure_evidence(ctx, stage, arm)
        raise
    finally:
        rec["seconds"] = round(time.time() - t0, 1)
        rec["ended"] = now_iso()
        if not ctx.dry:
            (ctx.state / "results.json").write_text(json.dumps(results, indent=1, default=str))


def sweep(ctx, args):
    cfg = ctx.cfg
    if "first_server_epoch" not in ctx.s and not ctx.dry:
        raise Stop("no servers recorded for this campaign; run `up` first")
    first = ctx.s.get("first_server_epoch", int(time.time()))
    soft = first + cfg["soft_cutoff_minutes"] * 60
    results = {"campaign": ctx.campaign, "mode": cfg["mode"], "runs": [], "config": {k: cfg[k] for k in ("k8s_profile", "warm_count", "measured_count", "node_type", "client_type")}}
    est = cfg.get("estimated_run_seconds", 780)
    if cfg.get("plan"):   # explicit run list: [{"stage":6,"arm":"rust_native","workload_params":{...},"writer_workers":1,"label":"..."}]
        plan = [(e["stage"], e["arm"], e) for e in cfg["plan"]]
    else:
        plan = [(s, a, {}) for s in cfg.get("stage_order", [1, 2, 3]) for a in ROTATIONS[s]]
        if args.only:
            wanted = {tuple(x.split(":")) for x in args.only.split(",")}
            plan = [(s, a, e) for s, a, e in plan if (str(s), a) in wanted]
    ctx.say("== sweep: %d runs: %s ==" % (len(plan), ", ".join("%dx/%s%s" % (p[0], p[1], "/" + p[2]["label"] if p[2].get("label") else "") for p in plan)))
    for stage, arm, entry in plan:
        if not ctx.dry and time.time() + est > soft:
            ctx.say("STOP: a further run (est %ds) would pass the soft cutoff; remaining runs skipped" % est)
            results["skipped"] = [(p[0], p[1], p[2].get("label")) for p in plan if (p[0], p[1], p[2].get("label")) not in [(r["stage"], r["arm"], r.get("label")) for r in results["runs"]]]
            break
        if entry:
            for k in ("workload_params", "writer_workers", "run_label", "layout"):
                cfg.pop(k, None)
            if entry.get("layout"):
                cfg["layout"] = entry["layout"]
            if entry.get("workload_params"):
                cfg["workload_params"] = entry["workload_params"]
            if entry.get("writer_workers"):
                cfg["writer_workers"] = entry["writer_workers"]
            cfg["run_label"] = entry.get("label")
        cond = bool(cfg.get("condition")) and not getattr(ctx, "cond_disabled", False)
        reason = None if cond or not cfg.get("condition") else "conditioning switched off after two consecutive conditioning failures"
        try:
            run_variant(ctx, stage, arm, results, condition=cond, reason=reason)
            if cond:
                ctx.cond_streak = 0
        except CondFail as exc:
            ctx.say("CONDITIONING FAILED (%dx %s): %s -> repeating this run unconditioned" % (stage, arm, str(exc)[:300]))
            ctx.cond_streak = getattr(ctx, "cond_streak", 0) + 1
            if ctx.cond_streak >= 2:
                ctx.cond_disabled = True
                ctx.say("conditioning failed twice in a row: switched OFF for the rest of the sweep")
            try:
                run_variant(ctx, stage, arm, results, condition=False, reason="conditioning failed: " + str(exc)[:300])
            except Stop as exc2:
                ctx.say("RUN FAILED (%dx %s, unconditioned retry): %s" % (stage, arm, str(exc2)[:600]))
        except Stop as exc:
            ctx.say("RUN FAILED (%dx %s): %s" % (stage, arm, str(exc)[:600]))
            if not args.keep_going:
                break
    ctx.say("== sweep finished; results: %s ==" % (ctx.state / "results.json"))
    return results


# ------------------------------------------------------------------------------------------ teardown (shared with the watchdog)
def teardown(ctx, *, wait_s=600):
    """Delete everything labelled campaign=<name>, then list by label to prove nothing is left."""
    ctx.say("== teardown: delete by label %s ==" % ctx.label)
    if ctx.dry:
        for kind in ("server", "firewall", "ssh-key", "placement-group", "volume", "primary-ip", "network"):
            ctx.hc(kind, "delete", "<each-%s-id-found-by-label>" % kind)
    deadline = time.time() + wait_s
    while True:
        servers = ctx.hc_list("server")
        for s in servers:
            ctx.hc("server", "delete", str(s["id"]), check=False, timeout=60)
        if not servers or ctx.dry:
            break
        if time.time() > deadline:
            raise Stop("servers still present after %ds" % wait_s)
        time.sleep(5)
    for kind in ("firewall", "ssh-key", "placement-group", "volume", "primary-ip", "network"):
        for _ in range(12):
            items = ctx.hc_list(kind)
            for it in items:
                ctx.hc(kind, "delete", str(it["id"]), check=False, timeout=60)
            if not items or ctx.dry:
                break
            time.sleep(0 if ctx.dry else 5)
    left = {}
    for kind in KINDS:
        items = ctx.hc_list(kind)
        if items:
            left[kind] = [i["id"] for i in items]
    proof = {"t": now_iso(), "label": ctx.label, "left": left, "empty": not left}
    if not ctx.dry:
        (ctx.state / "teardown-proof.json").write_text(json.dumps(proof, indent=1))
    ctx.log("teardown", **proof)
    ctx.say("   teardown proof: %s" % ("NOTHING LEFT with label " + ctx.label if not left else "LEFT: %s" % left))
    if left:
        raise Stop("teardown incomplete: %s" % left)
    return proof


def unit_name(ctx):
    return "lean-watch-%s.service" % ctx.campaign


def arm_watchdog(ctx, deadline_epoch):
    """User systemd unit (same mechanism as the accepted probe): at the deadline it deletes by label, even if this
    controller or the whole session is gone.  Everything it needs lives in the campaign state directory (not /tmp)."""
    frozen = ctx.state / "watchdog.py"
    ctx.say("== arm watchdog %s (deletes label %s at epoch %d = %s) ==" % (unit_name(ctx), ctx.label, deadline_epoch, datetime.datetime.fromtimestamp(deadline_epoch, datetime.timezone.utc).isoformat()))
    if not ctx.dry:
        frozen.write_text(Path(__file__).read_text())
        os.chmod(frozen, 0o700)
        (ctx.state / "condition.py").write_text((HERE / "condition.py").read_text())  # the frozen copy imports it
        (ctx.state / "watchdog-config.json").write_text(json.dumps({**ctx.cfg, "deadline_epoch": deadline_epoch, "state_dir": str(ctx.state.parent)}, indent=1))
    runtime_max = max(3600, int(deadline_epoch - time.time()) + 3600)
    unit = f"""[Unit]
Description=Lean run watchdog: delete everything labelled campaign={ctx.campaign} at the deadline
DefaultDependencies=no

[Service]
Type=exec
Slice=-.slice
KillMode=control-group
Restart=on-failure
RestartSec=20s
RuntimeMaxSec={runtime_max}
ExecStart=/usr/bin/python3 -B {frozen} watchdog --config {ctx.state / 'watchdog-config.json'}
"""
    path = Path.home() / ".config/systemd/user" / unit_name(ctx)
    if ctx.dry:
        ctx.say("DRY  write %s:\n%s" % (path, "".join("      " + l + "\n" for l in unit.splitlines())))
    else:
        path.write_text(unit)
        os.chmod(path, 0o600)
    ctx.run(["systemctl", "--user", "daemon-reload"])
    ctx.run(["systemctl", "--user", "start", unit_name(ctx)])
    if not ctx.dry:
        _, out, _ = ctx.run(["systemctl", "--user", "show", unit_name(ctx), "--property=ActiveState,MainPID,ControlGroup"])
        ctx.log("watchdog_armed", show=out)
        if "ActiveState=active" not in out:
            raise Stop("watchdog did not start; refuse to create servers: " + out)
        time.sleep(6)  # a crash-looping unit is briefly active: it must still be running (and not restarted) a few seconds later
        _, out2, _ = ctx.run(["systemctl", "--user", "show", unit_name(ctx), "--property=ActiveState,SubState,NRestarts,MainPID"])
        if "ActiveState=active" not in out2 or "SubState=running" not in out2 or "NRestarts=0" not in out2:
            raise Stop("watchdog is not staying up; refuse to create servers: " + out2)


def disarm_watchdog(ctx):
    ctx.run(["systemctl", "--user", "stop", unit_name(ctx)], check=False)
    path = Path.home() / ".config/systemd/user" / unit_name(ctx)
    if ctx.dry:
        ctx.say("DRY  rm %s" % path)
    elif path.exists():
        path.unlink()
    ctx.run(["systemctl", "--user", "daemon-reload"], check=False)
    ctx.run(["systemctl", "--user", "list-units", "--all", "lean-watch-*", "--no-legend"], check=False)


def watchdog_main(args):
    cfg = json.loads(Path(args.config).read_text())
    ctx = Ctx(cfg, False)
    deadline = cfg["deadline_epoch"]
    print("watchdog: label %s, deadline epoch %d" % (ctx.label, deadline), flush=True)
    while time.time() < deadline:
        if (ctx.state / "down.done").exists():
            print("controller finished teardown; watchdog exits", flush=True)
            return 0
        time.sleep(min(30, max(1, deadline - time.time())))
    ctx.log("watchdog_fire")
    for attempt in range(40):
        try:
            teardown(ctx, wait_s=300)
            (ctx.state / "down.done").write_text(now_iso())
            return 0
        except Stop as exc:
            print("watchdog teardown attempt %d failed: %s" % (attempt, exc), flush=True)
            time.sleep(30)
    return 1


# ------------------------------------------------------------------------------------------ commands
def load_cfg(path):
    cfg = json.loads(Path(path).read_text())
    for k in ("campaign", "mode", "location", "image", "node_type", "client_type", "hcloud", "deadline_minutes", "soft_cutoff_minutes",
              "builds", "assets", "warm_count", "measured_count", "k8s_profile", "source_revision", "source_manifest_sha256"):
        if k not in cfg:
            raise Stop("config missing " + k)
    return cfg


def cmd_up(ctx, args):
    if os.environ.get("LEAN_RESUME") and ctx.s.get("hosts"):
        ctx.say("== RESUME: keeping the armed watchdog and deadline %s; no cost print (the cap was checked by the first attempt) ==" % ctx.s.get("deadline_epoch"))
    else:
        worst = print_cost(ctx, args.cap)
        if not ctx.dry and not args.yes:
            raise Stop("pass --yes to create servers (this costs money)")
        first = int(time.time())
        deadline = first + ctx.cfg["deadline_minutes"] * 60
        ctx.s["first_server_epoch"], ctx.s["deadline_epoch"] = first, deadline
        ctx.save()
        arm_watchdog(ctx, deadline)
    provision(ctx)
    upload(ctx)
    bootstrap(ctx)
    ctx.say("== up complete ==")


def cmd_down(ctx, args):
    teardown(ctx)
    if not ctx.dry:
        (ctx.state / "down.done").write_text(now_iso())
    disarm_watchdog(ctx)


def cmd_status(ctx, args):
    for kind in KINDS:
        items = ctx.hc_list(kind)
        ctx.say("%-16s %s" % (kind, [i["id"] for i in items]))
    ctx.run(["systemctl", "--user", "list-units", "--all", "lean-watch-*", "--no-legend"], check=False, read_only=True)
    if (ctx.state / "results.json").exists():
        r = json.loads((ctx.state / "results.json").read_text())
        for run in r["runs"]:
            ctx.say("%dx %-11s %s %s" % (run["stage"], run["arm"], run["status"], run.get("seconds")))


def cmd_prepare_assets(ctx, args):
    """Local only: generate the corpora for the configured counts with the sealed control-pure (Go) executable,
    reuse existing ones, verify every hash.  Touches no provider."""
    cfg = ctx.cfg
    for count in sorted({c for c in (cfg["warm_count"], cfg["measured_count"]) if c > 0}):
        a = cfg["assets"]["corpus_%d" % count]
        p = Path(a["path"])
        if p.exists() and sha256_file(p) == a["sha256"]:
            ctx.say("corpus %d ok: %s" % (count, p))
            continue
        if a["sha256"] != "TO_BE_FILLED" and p.exists():
            raise Stop("corpus %d exists with a different hash" % count)
        p.parent.mkdir(parents=True, exist_ok=True)
        job = p.parent / ("corpus-%d.job.json" % count)
        report = p.parent / ("corpus-%d.report.json" % count)
        local_builds = Builds(cfg)
        exe = Path(cfg["builds"]["control_pure_binary"]["local_executable"])
        _, text = make_job(cfg, local_builds, "corpus", "go_pure", count=count, output=str(report), corpus_file=str(p), corpus_sha=None)
        ctx.say("generating corpus %d locally with %s" % (count, exe))
        if not ctx.dry:
            job.write_text(text)
            ctx.run(["env", "-i", "PATH=/usr/bin:/bin", "GOMAXPROCS=4", str(exe), str(job)], timeout=900)
            ctx.say("  sha256 %s  (put this into config.assets.corpus_%d.sha256)" % (sha256_file(p), count))


GOLDEN_MANIFEST_SHA256 = {1: "c74a5d70b2b50cc5a409f54dcbffb7a63651093a4c2df3f395d441fda3894455",
                          2: "af7cbe49b1fcf349be3a755ca026f074255647f4bc2dce8ec79ef355dfd8302d",
                          3: "8dd34896583220a2f188187d82e16a3fa689a0837528a63fca67fe29d9a395b6"}   # byte-identical to the manifests the reference run applied
GOLDEN_JOB_SHA256 = {"setup": "14037d186ac384676abcc1ec6ef3ed8408e3957b217d24a870e3b1c17f956249",
                     "ingest": "81bd267decee48e91ddadb872727d4d1293b887cddc552dc8bab7a56bc521792",
                     "verify": "e86db7ef34c1a0a89f0041687c2d17d720ecf9bb47e910ec0e78a839e4cbc2f5"}   # Stage 0 job files for these inputs


def selftest_jobs():
    """setup/ingest/verify job texts for fixed inputs (the file format the executables were validated with)."""
    cfg = {"source_revision": "45f5c38ea9cc040189f8521148282979376e54a4", "source_manifest_sha256": "f6834b130809fc60b07dae29dfb2f7878bfa2eb63ce485b6cf8c5ca1f7b46990",
           "match_every": 3, "workload": POINT2_WORKLOAD,
           "builds": {"control_pure_binary": {"sha256": "6d661995e2da6c5aac25d2705441a2d0d600165b1de4d492527ceea3a576766d"},
                      "control_native_binary": {"sha256": "8a8d78d88dcb55b80b1a8584244f32d07777d616d1621055417b84f90e93f3c3", "client_library_relative_path": "X"},
                      "rust_binary": {"sha256": "63e4bed9f065912eb4d5f31dd7d396008f1e0c6cae10f0d7237083950495aa7f"}}}
    b = Builds(cfg)
    live = {"run": "r", "cluster_file": "/srv/lean/fdb-s3.cluster", "cluster_sha256": "0" * 64, "layout_file": "/srv/lean/runs/r/layout.json",
            "layout_sha256": "1" * 64, "verification": VERIFICATION_NONE}
    out = {}
    for mode, tmo in (("setup", 300), ("ingest", 120), ("verify", 1800)):
        _, text = make_job(cfg, b, mode, "go_pure", count=100000, output="/o/%s.json" % mode, corpus_file="/c/corpus.bin", corpus_sha="2" * 64, live=live, timeout=tmo)
        out[mode] = text
    return out


def cmd_selftest(ctx, args):
    """Offline checks that need no provider: the baseline manifests and the job files are byte-identical to the validated ones."""
    bad = 0
    for n in (1, 2, 3):
        same = hashlib.sha256(render_fdb_cluster_yaml("otel-fdb-s%d" % n, n, "full").encode()).hexdigest() == GOLDEN_MANIFEST_SHA256[n]
        bad += not same
        print("manifest stage %d vs the reference run: %s" % (n, "IDENTICAL" if same else "DIFFERENT"))
    for mode, text in selftest_jobs().items():
        same = hashlib.sha256(text.encode()).hexdigest() == GOLDEN_JOB_SHA256[mode]
        bad += not same
        print("job %-6s vs the validated job file: %s" % (mode, "BYTE-IDENTICAL" if same else "DIFFERENT"))
    return 1 if bad else 0


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("cmd", choices=["cost", "prepare-assets", "up", "sweep", "down", "status", "watchdog", "selftest"])
    ap.add_argument("--config")
    ap.add_argument("--dry-run", action="store_true", help="print every provider/ssh/systemd command, execute none")
    ap.add_argument("--cap", type=Decimal, help="refuse if the worst-case cost (EUR) exceeds this")
    ap.add_argument("--yes", action="store_true", help="required by `up`: really create servers")
    ap.add_argument("--only", help="sweep subset, e.g. 1:go_pure,2:rust_native")
    ap.add_argument("--keep-going", action="store_true")
    args = ap.parse_args(argv)
    if args.cmd == "selftest":
        return cmd_selftest(None, args)
    if args.cmd == "watchdog":
        return watchdog_main(args)
    cfg = load_cfg(args.config)
    ctx = Ctx(cfg, args.dry_run)
    ctx.builds = Builds(cfg)
    if args.cap is None:
        args.cap = Decimal(str(cfg["cap_eur"]))
    try:
        if args.cmd == "cost":
            print_cost(ctx, args.cap)
        elif args.cmd == "prepare-assets":
            cmd_prepare_assets(ctx, args)
        elif args.cmd == "up":
            cmd_up(ctx, args)
        elif args.cmd == "sweep":
            sweep(ctx, args)
        elif args.cmd == "down":
            cmd_down(ctx, args)
        elif args.cmd == "status":
            cmd_status(ctx, args)
    except Stop as exc:
        print("STOP: %s" % exc, file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
