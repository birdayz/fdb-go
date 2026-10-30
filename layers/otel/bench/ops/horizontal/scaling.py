#!/usr/bin/env python3
"""Bounded independent-namespace weak scaling on one owned six-host lease."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
from pathlib import Path
import subprocess
import time

import diagnostic as n
import driver as d
import policy as p
import session as s

SCHEMA = "otel_native_scaling_v1"
STAGES = [[1, 1], [2, 2], [3, 3]]
AUTHORIZED_ROWS = 2850000
ROLES = {"node1", "node2", "node3", "client1", "client2", "client3"}
RECEIPT_BUDGET = 1024 ** 3
BOUNDARY = ("independent namespaces in one single-replica FDB cluster per stage; "
            "fixed six billed hosts, active pairs scale 1/2/3; no shared-corpus, HA, "
            "statistical repeatability or sustained-120-second 20k certification")


def validate_plan(plan, *, check_assets=True):
    p.require(type(plan) is dict and plan.get("schema") == SCHEMA and plan.get("approved") is True,
              "explicit native scaling approval required")
    stages = plan.get("stages")
    p.require(type(stages) is list and stages == STAGES and all(type(x) is list and len(x) == 2
              and all(type(v) is int for v in x) for x in stages), "exact ordered 1/1, 2/2, 3/3 scaling stages")
    for key, value in (("count", 100000), ("measured_count", 375000), ("measured_seconds", 120),
                       ("batch", 64), ("workers", 8), ("gomaxprocs", 4),
                       ("storage_process_count", 3), ("storage_max_outstanding", 64)):
        p.require(type(plan.get(key)) is int and plan[key] == value, "fixed scaling field: " + key)
    p.require(type(plan.get("storage_memory_profile")) is str and plan["storage_memory_profile"] == "scaling3ss3g", "fixed three-storage-process memory geometry")
    prior = plan.get("prior_rows_upper")
    p.require(type(prior) is int and prior >= 0 and prior + AUTHORIZED_ROWS <= 10000000, "scaling cumulative 10M row ceiling")
    p.require(not set(plan) & {"topologies", "selected_topologies", "prerequisite_topologies", "measured_topologies",
                              "probe_count", "probe_seconds", "cases"}, "scaling has only its exact preregistered stages")
    # Validation-only projection reuses common source/assets/hardware/lease guards.
    # It is never written or provisioned and cannot authorize extra rows.
    common = {key: value for key, value in plan.items() if key not in
              ("storage_process_count", "storage_max_outstanding", "storage_memory_profile")}
    common.update(schema="otel_horizontal_v1", topologies=[1, 2, 3], measured_topologies=[3, 2, 1], measured_count=100000)
    p.validate_plan(common, check_assets=check_assets)
    return plan


def active_roles(hosts):
    p.require(type(hosts) is int and hosts in (1, 2, 3), "typed active scaling pairs")
    return {kind + str(i) for i in range(1, hosts + 1) for kind in ("node", "client")}


def run_name(hosts, count, index):
    active_roles(hosts)
    p.require(type(count) is int and count in (100000, 375000) and type(index) is int and 1 <= index <= hosts,
              "fixed independent scaling run identity")
    return "prof20k%04d" % (9300 + (hosts - 1) * 10 + (3 if count == 375000 else 0) + index - 1)


def wave_budget(remaining, remaining_waves):
    p.require(type(remaining) is int and 0 <= remaining <= n.TRACE_BUDGET
              and type(remaining_waves) is int and 1 <= remaining_waves <= 6, "shared scaling forensic wave reservation")
    return remaining // remaining_waves


def producer_grant(value):
    p.require(type(value) is dict and value.get("schema") == "otel_native_scaling_producer_v1", "typed scaling producer grant")
    hosts, index, count = value.get("stage_hosts"), value.get("producer_index"), value.get("count")
    p.require(value.get("run") == run_name(hosts, count, index) and value.get("role") == "client" + str(index),
              "scaling producer stage/namespace binding")
    p.require(value.get("diagnostic_only") is True and type(value.get("diagnostic_seconds")) is int
              and value["diagnostic_seconds"] == (300 if count == 100000 else 120)
              and type(value.get("batch")) is int and value["batch"] == 64
              and type(value.get("workers")) is int and value["workers"] == 8, "fixed scaling producer bounds")
    # Common producer validation only; actual app argv keeps the authorized count.
    d.client_plan({**value, "schema": "otel_horizontal_producer_v1", "count": 100000})
    return value


def validate_ingest(evidence, count):
    p.require(type(count) is int and count in (100000, 375000), "fixed scaling committed row population")
    p.require(type(evidence) is dict and type(evidence.get("stats")) is dict
              and type(evidence["stats"].get("diagnostics")) is dict, "complete source diagnostics required")
    diagnostics = evidence["stats"]["diagnostics"]
    committed = diagnostics.get("committed_rows")
    p.require(type(committed) is int and committed == count, "all complete committed ACK populations required")
    p.require(p.integer(diagnostics.get("committed_batches"), 1), "nonempty committed source batches required")
    phases = diagnostics.get("phase_latencies")
    p.require(type(phases) is dict, "complete nonzero source phase diagnostics required")
    for key in ("open", "save", "commit", "run"):
        phase = phases.get(key)
        p.require(type(phase) is dict and all(p.integer(phase.get(field), 1) for field in
                  ("count", "min_nanos", "max_nanos", "total_nanos"))
                  and phase["min_nanos"] <= phase["max_nanos"] <= phase["total_nanos"]
                  and phase["count"] * phase["min_nanos"] <= phase["total_nanos"] <= phase["count"] * phase["max_nanos"],
                  "complete nonzero source phase diagnostics required: " + key)
    return True


def wave_metrics(rows, start_ns, ack_end_ns, drained_end_ns, load_samples, drain_samples):
    p.require(p.integer(rows, 1) and all(p.integer(x, 1) for x in (start_ns, ack_end_ns, drained_end_ns))
              and start_ns < ack_end_ns <= drained_end_ns, "nonempty common ACK/drained timing window")
    p.require(type(load_samples) is list and load_samples and type(drain_samples) is list and len(drain_samples) >= 3,
              "nonempty load and three strict drain observations")
    samples = load_samples + drain_samples
    p.require(all(type(x) is dict and x.get("healthy") is True and p.integer(x.get("queue_bytes"))
                  and p.number(x.get("lag_seconds")) for x in samples), "strict measured queue/lag population")
    p.require(all(x["queue_bytes"] < 16777216 and x["lag_seconds"] < 10 for x in drain_samples[-3:]),
              "final strict stable drain required")
    return {"rows": rows, "controller_started_ns": start_ns, "controller_ack_end_ns": ack_end_ns,
            "controller_drained_end_ns": drained_end_ns,
            "ack_rows_per_second": p.common_throughput(rows, start_ns, ack_end_ns),
            "drained_rows_per_second": p.common_throughput(rows, start_ns, drained_end_ns),
            "drain_seconds": (drained_end_ns - ack_end_ns) / 10 ** 9,
            "queue": {"start": samples[0]["queue_bytes"], "end": samples[-1]["queue_bytes"],
                      "max": max(x["queue_bytes"] for x in samples)},
            "lag": {"start": samples[0]["lag_seconds"], "end": samples[-1]["lag_seconds"],
                    "max": max(x["lag_seconds"] for x in samples)},
            "timing_boundary": "controller common release through last complete ACK and through final three-sample strict stable drain; includes SSH/observation overhead"}


def summarize_scaling(records):
    result = {"verdict": "INCONCLUSIVE", "near_linear": False, "efficiency_threshold": 0.90,
              "primary_metric": "drained_rows_per_second", "stages": [], "reasons": [], "boundary": BOUNDARY}
    if type(records) is not list or len(records) != 3:
        result["reasons"].append("all three preregistered stages required")
        return result
    for hosts, record in enumerate(records, 1):
        measured = record.get("measured", {}) if type(record) is dict else {}
        if (type(record) is not dict or type(record.get("hosts")) is not int or record["hosts"] != hosts
                or type(record.get("producers")) is not int or record["producers"] != hosts or record.get("status") != "PASS"
                or type(measured) is not dict or type(measured.get("count_per_producer")) is not int
                or measured["count_per_producer"] != 375000 or type(measured.get("producers")) is not int
                or measured["producers"] != hosts or type(measured.get("rows")) is not int
                or measured["rows"] != hosts * 375000 or not all(measured.get(key) is True for key in
                    ("healthy", "ingest_valid", "census_pass", "oracle_pass", "measurement_eligible"))
                or not all(p.number(measured.get(key)) and measured[key] > 0 for key in
                           ("ack_rows_per_second", "drained_rows_per_second"))
                or measured["drained_rows_per_second"] > measured["ack_rows_per_second"]):
            result["reasons"].append("stage%d lacks complete passing measured evidence" % hosts)
    if result["reasons"]:
        return result
    baseline = records[0]["measured"]
    for record in records:
        hosts, measured = record["hosts"], record["measured"]
        factor = measured["drained_rows_per_second"] / baseline["drained_rows_per_second"]
        result["stages"].append({"hosts": hosts, "scale_factor": factor, "efficiency": factor / hosts,
                                 "ack_scale_factor": measured["ack_rows_per_second"] / baseline["ack_rows_per_second"]})
    result["near_linear"] = all(x["efficiency"] >= 0.90 for x in result["stages"])
    result["verdict"] = "NEAR_LINEAR" if result["near_linear"] else "BELOW_NEAR_LINEAR"
    return result


def classify_stage_failure(error, document, layout, wave, *, fatal_guards_passed):
    if type(layout) is not dict or layout.get("schema") != SCHEMA:
        return False
    hosts = layout.get("storage_hosts")
    if type(hosts) is not list or hosts not in (["node1"], ["node1", "node2"], ["node1", "node2", "node3"]):
        return False
    return s.classify_arm_failure(error, document, layout, wave, fatal_guards_passed=fatal_guards_passed)


class ScalingArm(n.Diagnostic):
    def __init__(self, *args, stage_hosts, **kwargs):
        super().__init__(*args, **kwargs)
        self.stage_hosts = stage_hosts
        active_roles(stage_hosts)
        self.arm_id = "scaling-" + str(stage_hosts)
        self.last_wave_result = None
        self.remaining_waves = 6
        self.producer_plans = []

    def remote(self, node, mode, path):
        return ["python3", "-B", self.remote_harness + "/scaling.py", mode, "--path", str(path), "--node-id", str(node["id"])]

    def install_helpers(self):
        super().install_helpers()
        for node in self.nodes.values():
            self.copy(node, n.HERE / "session.py", self.remote_harness + "/session.py")
            self.copy(node, n.HERE / "scaling.py", self.remote_harness + "/scaling.py")
            for name in ("session.py", "scaling.py"):
                p.require(self.command(node, ["sha256sum", self.remote_harness + "/" + name]).split()[0] == p.sha(n.HERE / name),
                          "scaling remote helper seal")

    def verify_helpers(self):
        super().verify_helpers()
        for node in self.nodes.values():
            for name in ("session.py", "scaling.py"):
                p.require(self.command(node, ["sha256sum", self.remote_harness + "/" + name]).split()[0] == p.sha(n.HERE / name),
                          "scaling remote helper changed")

    def scope_bindings(self):
        result = []
        for role, grant in self.producer_plans:
            for mode in ("ingest", "audit", "query"):
                args = d.app_argv(grant, mode)
                args[args.index("--timeout") + 1] = str(grant["diagnostic_seconds"] if mode == "ingest" else 120) + "s"
                unit = "otel-native-" + grant["run"] + "-" + mode + ".scope"
                result.append({"node": role, "unit": unit, "args": args,
                               "cgroup": "/horizontal.slice/horizontal-client.slice/" + unit})
        return result

    def drain(self, layout):
        until, streak, values = time.monotonic() + d.remaining(self.plan, 300), 0, []
        while time.monotonic() < until:
            sample = self.monitor(layout)
            values.append(sample)
            streak = streak + 1 if sample["queue_bytes"] < 16777216 and sample["lag_seconds"] < 10 else 0
            if streak == 3:
                return values
            self.sleep(5)
        raise ValueError("strict native readiness/drain cap exhausted")

    def wave(self, layout, count, name):
        d.remaining(self.plan, reserve=1500)
        directory = self.out / name
        directory.mkdir(mode=0o700)
        before = self.stable(layout)
        hosts = self.stage_hosts
        roles = active_roles(hosts)
        allocation = wave_budget(self.trace_remaining, self.remaining_waves)
        quotas = {"node" + str(i): allocation // hosts + (i <= allocation % hosts) for i in range(1, hosts + 1)}
        producer_grants, local_plans, jobs, verification_jobs = {}, {}, {}, {}
        sampler_grants, samplers, frames, cursors, identity_hashes, receipts = {}, {}, {}, {}, {}, {}
        activity, load_samples, drain_samples = [before], [before], []
        release_bound, started, ack_end, drained_end = False, None, None, None
        result = {"schema": "otel_native_scaling_wave_v1", "count_per_producer": count, "count": count,
                  "producers": hosts, "rows": count * hosts, "healthy": False, "ingest_valid": False,
                  "census_pass": False, "oracle_pass": False, "measurement_eligible": False,
                  "accepted_throughput": False, "diagnostic_only": True, "forensic_allocation": allocation,
                  "forensic_node_quotas": quotas, "active_roles": sorted(roles)}
        error = None

        def poll_frames():
            for role, grant in sampler_grants.items():
                node = self.nodes[role]
                value = d.loads(self.command(node, self.diag(node, "_frames", grant["work"] + "/plan.json", "--cursor", str(cursors[role]))))
                p.require(type(value) is dict and type(value.get("frames")) is list and type(value.get("cursor")) is int
                          and value["cursor"] == cursors[role] + len(value["frames"]), "exact scaling sampler cursor")
                frames[role].extend(value["frames"])
                cursors[role] = value["cursor"]
                d.write(directory / (role + "-frames-" + str(cursors[role]) + "-" + str(self.clock()) + ".json"), value)

        try:
            for i in range(1, hosts + 1):
                role, run = "client" + str(i), run_name(hosts, count, i)
                node = self.nodes[role]
                work = self.remote_root + "/producers/" + run
                grant = {**{key: self.plan[key] for key in ("campaign", "first_epoch", "deadline_epoch", "binary_sha256", "source_revision",
                         "batch", "workers", "gomaxprocs", "storage_options")}, "schema": "otel_native_scaling_producer_v1",
                         "stage_hosts": hosts, "producer_index": i, "role": role, "node_id": node["id"], "run": run, "count": count,
                         "independent_store": True, "diagnostic_only": True, "diagnostic_seconds": 300 if count == 100000 else 120,
                         "token": hashlib.sha256((self.plan["campaign"] + run).encode()).hexdigest(),
                         "binary": self.remote_root + "/bin/otel-demo", "cluster_file": layout["remote_cluster_file"], "work": work}
                producer_grant(grant)
                self.command(node, ["mkdir", work])
                local = directory / (role + "-producer-plan.json")
                d.write(local, grant)
                self.copy(node, local, work + "/plan.json")
                p.require(self.command(node, ["sha256sum", layout["remote_cluster_file"]]).split()[0] == p.sha(layout["cluster_file"]),
                          "scaling producer cluster file changed")
                producer_grants[role], local_plans[role] = grant, local
                self.producer_plans.append((role, grant))
            for role in sorted(roles):
                index = int(role.removeprefix("node").removeprefix("client"))
                client_role = "client" + str(index)
                grant, local = producer_grants[client_role], local_plans[client_role]
                node = self.nodes[role]
                work = self.remote_root + "/diagnostics/" + grant["run"] + "/" + role
                self.command(node, ["mkdir", "-p", str(Path(work).parent)])
                self.command(node, ["mkdir", work])
                specs = [x for x in layout["processes"] if x["node"] == role]
                sample_grant = {"schema": "otel_native_scaling_sampler_v1", "scaling_hosts": hosts,
                    "storage_memory_profile": "scaling3ss3g", "storage_process_count": 3,
                    "role": role, "node_id": node["id"], "campaign": self.plan["campaign"], "prefix": self.plan["prefix"],
                    "first_epoch": self.plan["first_epoch"], "deadline_epoch": self.plan["deadline_epoch"],
                    "root": self.remote_root, "work": work, "arm_root": layout["root"], "binary": grant["binary"],
                    "binary_sha256": grant["binary_sha256"], "fdbserver_sha256": self.plan["fdbserver_sha256"],
                    "run": grant["run"], "token": grant["token"], "producer_grant_sha256": p.sha(local),
                    "forensic_budget": quotas.get(role, 0), "processes": specs,
                    "storage_ids": {spec["address"]: before["storage_processes"][spec["address"]]["storage_id"]
                                    for spec in specs if spec["process_class"] == "storage"}}
                n.sampler_grant(sample_grant)
                path = directory / (role + "-sampler-plan.json")
                d.write(path, sample_grant)
                self.copy(node, path, work + "/plan.json")
                kind = "fdb" if role.startswith("node") else "client"
                args = ["systemd-run", "--scope", "--quiet", "--collect", "--unit=otel-native-sampler-" + grant["run"] + "-" + role,
                        "--slice=horizontal-" + kind + ".slice", *self.diag(node, "_sample", work + "/plan.json")]
                samplers[role] = self.spawn(node, args, directory, role + "-sampler")
                sampler_grants[role], frames[role], cursors[role] = sample_grant, [], 0
            for role, grant in sampler_grants.items():
                ready = self.wait_file(self.nodes[role], grant["work"] + "/ready.json", job=samplers[role])
                identity = self.read_remote(self.nodes[role], grant["work"] + "/identity.json")
                identity_path = directory / (role + "-native-identity.json")
                d.write(identity_path, identity)
                identity_hashes[role] = p.sha(identity_path)
                p.require(ready == {"node_id": self.nodes[role]["id"], "token": grant["token"], "baseline_frames": 2,
                          "grant_sha256": p.sha(directory / (role + "-sampler-plan.json")), "identity_sha256": identity_hashes[role]},
                          "scaling sampler pre-release identity seal")
                d.write(directory / (role + "-sampler-ready.json"), ready)
            poll_frames()
            for role, grant in producer_grants.items():
                jobs[role] = self.spawn(self.nodes[role], self.remote(self.nodes[role], "_produce", grant["work"] + "/plan.json"), directory, role + "-producer")
            for role, grant in producer_grants.items():
                ready = self.wait_file(self.nodes[role], grant["work"] + "/ready.json", job=jobs[role])
                p.require(ready == {"node_id": self.nodes[role]["id"], "run": grant["run"], "token": grant["token"], "grant_sha256": p.sha(local_plans[role])},
                          "all scaling producers ready before common release")
                d.write(directory / (role + "-producer-ready.json"), ready)
            for role, grant in sampler_grants.items():
                self.command(self.nodes[role], self.diag(self.nodes[role], "_phase", grant["work"] + "/plan.json"))
            started = self.clock()
            with ThreadPoolExecutor(max_workers=hosts) as executor:
                list(executor.map(lambda role: self.command(self.nodes[role], self.remote(self.nodes[role], "_release", producer_grants[role]["work"] + "/plan.json")), producer_grants))
            release_bound = True
            d.write(directory / "release.json", {"controller_before_ns": started, "controller_after_ns": self.clock(),
                    "producer_grant_sha256": {role: p.sha(path) for role, path in local_plans.items()}})
            result["observation_phase"] = "load"
            limit = time.monotonic() + d.remaining(self.plan, 300 if count == 100000 else 120)
            for role, grant in producer_grants.items():
                receipts[role] = self.wait_file(self.nodes[role], grant["work"] + "/producer-started.json", 10)
            while any(job[0].poll() is None for job in jobs.values()):
                p.require(time.monotonic() < limit, "scaling common ingest observation cap")
                poll_frames()
                sample = self.monitor(layout)
                activity.append(sample)
                load_samples.append(sample)
                p.require(all(job[0].poll() is None for job in samplers.values()), "scaling sampler exited during load")
                self.sleep(0.5)
            ack_end = self.clock()
            poll_frames()
            for role, job in jobs.items():
                p.require(job[0].wait(timeout=2) == 0, "scaling producer failed")
                evidence = self.read_remote(self.nodes[role], producer_grants[role]["work"] + "/ingest.json")
                p.evidence(evidence, self.plan, producer_grants[role]["run"], count, "ingest")
                validate_ingest(evidence, count)
                d.write(directory / (role + "-ingest.json"), evidence)
            result["ingest_valid"] = True
            sample = self.monitor(layout)
            activity.append(sample)
            load_samples.append(sample)
            for role, grant in sampler_grants.items():
                self.command(self.nodes[role], self.diag(self.nodes[role], "_stop", grant["work"] + "/plan.json"))
            result["observation_phase"] = "drain"
            drain_samples = self.drain(layout)
            drained_end = self.clock()
            result["observation_phase"] = "verification"
            for role, grant in producer_grants.items():
                verification_jobs[role] = self.spawn(self.nodes[role], self.remote(self.nodes[role], "_verify", grant["work"] + "/plan.json"), directory, role + "-verification")
            until = time.monotonic() + d.remaining(self.plan, 245)
            while any(job[0].poll() is None for job in verification_jobs.values()):
                p.require(time.monotonic() < until, "scaling census/oracle cap")
                self.monitor(layout)
                self.sleep(0.5)
            for role, job in verification_jobs.items():
                p.require(job[0].wait(timeout=2) == 0, "scaling census/oracle failed")
                for mode in ("audit", "query"):
                    evidence = self.read_remote(self.nodes[role], producer_grants[role]["work"] + "/" + mode + ".json")
                    p.evidence(evidence, self.plan, producer_grants[role]["run"], count, mode)
                    d.write(directory / (role + "-" + mode + ".json"), evidence)
            self.stable(layout)
            result.update(healthy=True, census_pass=True, oracle_pass=True)
            result.update(wave_metrics(count * hosts, started, ack_end, drained_end, load_samples, drain_samples))
        except BaseException as exc:
            error = exc
            result["error"] = str(exc)
        finally:
            for role, job in list(jobs.items()) + list(verification_jobs.items()):
                if job[0].poll() is None:
                    try:
                        self.command(self.nodes[role], self.remote(self.nodes[role], "_abort", producer_grants[role]["work"] + "/plan.json"), 5)
                        job[0].wait(timeout=8)
                    except (ValueError, OSError, subprocess.SubprocessError) as exc:
                        result.setdefault("collection_errors", []).append("producer abort/reap: " + str(exc))
                        job[0].terminate()
                        try:
                            job[0].wait(timeout=3)
                        except subprocess.TimeoutExpired:
                            job[0].kill(); job[0].wait(timeout=3)
                job[1].close(); job[2].close()
            for role, grant in sampler_grants.items():
                try:
                    self.command(self.nodes[role], self.diag(self.nodes[role], "_stop", grant["work"] + "/plan.json"), 5)
                except (ValueError, OSError, subprocess.SubprocessError) as exc:
                    result.setdefault("collection_errors", []).append(str(exc))
            try:
                poll_frames()
            except (ValueError, OSError, subprocess.SubprocessError) as exc:
                result.setdefault("collection_errors", []).append(str(exc))
            result["sampler_exits"] = {}
            for role, job in samplers.items():
                try:
                    job[0].wait(timeout=5)
                except subprocess.TimeoutExpired:
                    job[0].terminate()
                    try:
                        job[0].wait(timeout=3)
                    except subprocess.TimeoutExpired:
                        job[0].kill(); job[0].wait(timeout=3)
                job[1].close(); job[2].close()
                result["sampler_exits"][role] = job[0].returncode
                if type(job[0].returncode) is not int or job[0].returncode != 0:
                    result.setdefault("collection_errors", []).append(role + ": sampler did not exit cleanly")
            for role, grant in producer_grants.items():
                try:
                    self.collect(self.nodes[role], grant, directory)
                except (ValueError, OSError, subprocess.SubprocessError) as exc:
                    result.setdefault("collection_errors", []).append(str(exc))
            for role, quota in quotas.items():
                if role not in sampler_grants:
                    continue
                charged = 0
                try:
                    p.require(role in identity_hashes, "missing pre-release scaling identity receipt")
                    grant = sampler_grants[role]
                    traces = d.loads(self.command(self.nodes[role], self.diag(self.nodes[role], "_traces", grant["work"] + "/plan.json",
                        "--budget", str(quota), "--identity-sha256", identity_hashes[role])))
                    p.require(type(traces) is dict and traces.get("schema") == "otel_native_forensic_evidence_v1"
                              and type(traces.get("node_id")) is int and traces["node_id"] == self.nodes[role]["id"]
                              and traces.get("role") == role and type(traces.get("scaling_hosts")) is int and traces["scaling_hosts"] == hosts
                              and traces.get("grant_sha256") == p.sha(directory / (role + "-sampler-plan.json")), "scaling native receipt host/grant binding")
                    p.require(type(traces.get("bytes_read")) is int and 0 <= traces["bytes_read"] <= quota, "shared scaling forensic node quota")
                    charged = traces["bytes_read"]
                    self.trace_remaining -= charged
                    d.write(directory / (role + "-native-traces.json"), traces)
                    if (traces.get("coverage") != "CAPTURED" or traces.get("forensic_only") is not False
                            or type(traces.get("errors")) is not list or traces["errors"]):
                        result.setdefault("collection_errors", []).append(role + ": native evidence is not live-verified/complete")
                except (ValueError, OSError, subprocess.SubprocessError) as exc:
                    # Unknown remote consumption is charged at its entire reservation.
                    self.trace_remaining -= quota - charged
                    result.setdefault("collection_errors", []).append(str(exc))
            result["producer_process_receipts"] = receipts
            result["producer_process_receipt"] = receipts.get("client1")
            result["release_bound"] = release_bound
            result["native_activity"] = activity
            result["load_samples"], result["drain_samples"] = load_samples, drain_samples
            pairs = {}
            for i in range(1, hosts + 1):
                node_role, client_role = "node" + str(i), "client" + str(i)
                pair_roles = {node_role, client_role}
                addresses = {x["address"] for x in layout["processes"] if x["node"] == node_role and x["process_class"] == "storage"}
                pair_activity = [{"storage_processes": {address: value for address, value in sample.get("storage_processes", {}).items() if address in addresses},
                                  "storage": {node_role: sample.get("storage", {}).get(node_role, {})}} for sample in activity]
                pairs[node_role] = n.profile({role: frames[role] for role in pair_roles if role in frames},
                    {role: sampler_grants[role] for role in pair_roles if role in sampler_grants}, receipts.get(client_role), pair_activity,
                    release_bound, node_role=node_role, client_role=client_role)
            result["profile"] = {"verdict": "CAPTURED" if len(pairs) == hosts and all(x["verdict"] == "CAPTURED" for x in pairs.values())
                                 and not result.get("collection_errors") else "INCONCLUSIVE",
                                 "producer_process_bound": all(x["producer_process_bound"] for x in pairs.values()),
                                 "positive_native_activity": all(x["positive_native_activity"] for x in pairs.values()),
                                 "pairs": pairs, "accepted_throughput": False}
            result["measurement_eligible"] = all(result[key] is True for key in ("healthy", "ingest_valid", "census_pass", "oracle_pass")) and result["profile"]["verdict"] == "CAPTURED"
            d.write(directory / "frames.json", frames)
            self.last_wave_result = result
            self.remaining_waves -= 1
            d.write(directory / "result.json", result)
        if error:
            raise error
        p.require(not result.get("collection_errors"), "scaling forensic/collection failure")
        return result

    def run_existing(self, resources, *, helpers_ready=False):
        validate_plan(self.plan)
        source = p.verify_source(self.plan)
        self.out.mkdir(mode=0o700, parents=True, exist_ok=False)
        d.write(self.out / "approved-plan.json", self.plan)
        d.write(self.out / "source-before.json", source)
        self.resources, self.nodes = resources, resources["nodes"]
        p.require(set(self.nodes) == ROLES, "exact six scaling hosts retained")
        result = {"schema": "otel_native_scaling_stage_v1", "hosts": self.stage_hosts, "producers": self.stage_hosts,
                  "diagnostic_only": True, "accepted_throughput": False, "status": "SESSION_FATAL"}
        try:
            if helpers_ready:
                self.verify_helpers()
            else:
                self.install_helpers()
            self.quiet()
            d.remaining(self.plan, reserve=2000)
            layout = self.provision.setup_cluster(self.plan, self.provision_out, self.nodes, self.stage_hosts, self.arm_id)
            self.layouts.append(layout)
            observations = self.host_samples()
            self.pid_baseline = {role: value["fdb_pids"] for role, value in observations.items() if role.startswith("node")}
            self.stable(layout, startup=True)
            gate = self.wave(layout, 100000, "prerequisite")
            require_wave(gate, self.stage_hosts, 100000)
            result["prerequisite"] = gate
            result["measured"] = self.wave(layout, 375000, "measured")
            require_wave(result["measured"], self.stage_hosts, 375000)
            result["status"] = "PASS" if result["measured"]["measurement_eligible"] else "INCONCLUSIVE"
        except BaseException as error:
            result["error"] = str(error)
            raise
        finally:
            try:
                d.write(self.out / "source-after.json", p.verify_source(self.plan))
            finally:
                d.write(self.out / "stage-result.json", result)
        return result


def require_wave(value, hosts, count):
    p.require(type(value) is dict and type(value.get("count_per_producer")) is int and value["count_per_producer"] == count
              and type(value.get("producers")) is int and value["producers"] == hosts and type(value.get("rows")) is int
              and value["rows"] == count * hosts and all(value.get(key) is True for key in
                  ("healthy", "ingest_valid", "census_pass", "oracle_pass")), "every active producer requires complete own health/census/oracle gate")


class ScalingSession:
    def __init__(self, plan, out, provision, *, arm_factory=None):
        self.plan, self.out, self.provision = plan, Path(out), provision
        self.provision_out = self.out / "provision"
        self.arm_factory = arm_factory or ScalingArm
        self.resources = None
        self.trace_remaining, self.remaining_waves = n.TRACE_BUDGET, 6
        self.observation_history = {}

    def receipt_budget(self):
        p.require(sum(x.stat().st_size for x in self.out.rglob("*") if x.is_file()) < RECEIPT_BUDGET, "scaling shared 1GiB receipt ceiling")

    def run(self):
        validate_plan(self.plan)
        source = p.verify_source(self.plan)
        self.out.mkdir(mode=0o700, parents=True, exist_ok=False)
        d.write(self.out / "approved-plan.json", self.plan)
        d.write(self.out / "source-before.json", source)
        result = {"schema": "otel_native_scaling_result_v1", "status": "SESSION_FATAL", "stages": [],
                  "all_stages_passed": False, "authorized_rows": AUTHORIZED_ROWS,
                  "cumulative_rows_upper": self.plan["prior_rows_upper"] + AUTHORIZED_ROWS,
                  "first_epoch": self.plan["first_epoch"], "deadline_epoch": self.plan["deadline_epoch"],
                  "approved_plan_sha256": p.sha(self.out / "approved-plan.json"), "held_roles": sorted(ROLES),
                  "held_host_count": 6, "boundary": BOUNDARY, "accepted_throughput": False}
        try:
            self.receipt_budget()
            d.remaining(self.plan, reserve=2000)
            self.resources = self.provision.prepare(self.plan, self.provision_out)
            nodes = self.resources["nodes"]
            p.require(set(nodes) == ROLES, "exact six owned scaling nodes")
            ids = [node["id"] for node in nodes.values()]
            owned, protected = self.resources.get("ids", {}).get("server"), self.resources.get("protected_ids", {}).get("server")
            p.require(self.resources.get("prepared") is True and self.resources.get("campaign") == self.plan["campaign"]
                      and self.resources.get("prefix") == self.plan["prefix"] and all(type(x) is int and x > 0 for x in ids)
                      and len(set(ids)) == 6 and type(owned) is list and len(owned) == 6 and set(owned) == set(ids)
                      and type(protected) is list and not set(ids) & set(protected), "six exact newly owned unprotected resource identities")
            for index, (hosts, producers) in enumerate(STAGES):
                self.receipt_budget()
                out = self.out / "stages" / ("pairs" + str(hosts))
                record = {"hosts": hosts, "producers": producers, "status": "SESSION_FATAL", "stage_result": str(out / "stage-result.json"),
                          "active_host_count": 2 * hosts, "held_host_count": 6, "active_storage_processes": 3 * hosts,
                          "active_cpu_quota": 8 * hosts, "active_memory_limit_bytes": 32 * p.GIB * hosts}
                result["stages"].append(record)
                arm = self.arm_factory(self.plan, out, self.provision, stage_hosts=hosts, provision_out=self.provision_out, receipt_root=self.out)
                arm.trace_remaining, arm.remaining_waves = self.trace_remaining, self.remaining_waves
                arm.observation_history = self.observation_history
                try:
                    try:
                        completed = arm.run_existing(self.resources, helpers_ready=index > 0)
                        p.require(type(completed) is dict and completed.get("schema") == "otel_native_scaling_stage_v1", "scaling stage result binding")
                        for phase, count in (("prerequisite", 100000), ("measured", 375000)):
                            require_wave(completed.get(phase), hosts, count)
                            record[phase] = completed[phase]
                        record["status"] = "PASS" if completed["measured"].get("measurement_eligible") is True else "INCONCLUSIVE"
                    except BaseException as error:
                        fresh_hosts = arm.host_samples()
                        fresh_source = p.verify_source(self.plan)
                        d.remaining(self.plan)
                        layout = arm.layouts[-1] if arm.layouts else {}
                        if not classify_stage_failure(error, arm.last_status_document, layout, arm.last_wave_result, fatal_guards_passed=True):
                            raise
                        record.update(status="ARM_FAILURE", error=str(error))
                        d.write(out / "arm-failure-classification.json", {"status": "ARM_FAILURE", "error": str(error), "health_pass": False,
                                "raw_status": arm.last_status_document, "fresh_host_guards": fresh_hosts, "fresh_source": fresh_source})
                    arm.host_samples()
                    p.verify_source(self.plan)
                    d.remaining(self.plan)
                    p.require(arm.layouts, "stage has no bound native layout")
                    stopped = self.provision.stop_owned_arm(self.plan, self.provision_out, nodes, arm.layouts[-1], scopes=arm.scope_bindings())
                    p.require(type(stopped) is dict and stopped.get("passed") is True and stopped.get("quiescent") is True, "exact scaling owned-arm quiescence failed")
                    d.write(out / "quiescence.json", stopped)
                except BaseException as error:
                    record.update(status="SESSION_FATAL", error=str(error))
                    raise
                finally:
                    try:
                        p.require(type(arm.trace_remaining) is int and 0 <= arm.trace_remaining <= self.trace_remaining, "scaling forensic budget reset/overflow")
                        p.require(arm.observation_history is self.observation_history, "scaling host observation history reset")
                        self.trace_remaining = arm.trace_remaining
                        # A failed gate skips its measured wave without increasing any quota.
                        self.remaining_waves = 6 - (index + 1) * 2
                        record["trace_bytes_remaining"] = self.trace_remaining
                    except BaseException as error:
                        record.update(status="SESSION_FATAL", error=str(error))
                        raise
                    finally:
                        d.write(out / "session-stage.json", record)
                self.receipt_budget()
            result["all_stages_passed"] = all(x["status"] == "PASS" for x in result["stages"])
            result["status"] = "ALL_STAGES_PASSED" if result["all_stages_passed"] else "COMPLETED_WITH_NONPASSING_STAGES"
        except BaseException as error:
            result.update(status="SESSION_FATAL", error=str(error), all_stages_passed=False)
            raise
        finally:
            try:
                if self.resources is not None:
                    cleanup = self.provision.teardown(self.plan, self.provision_out, self.resources)
                    d.write(self.out / "cleanup.json", cleanup)
                    result["cleanup_passed"] = cleanup.get("passed") is True
                    p.require(result["cleanup_passed"], "exact ten-kind baseline cleanup did not pass")
            except BaseException as error:
                result.update(status="SESSION_FATAL", error=str(error), cleanup_passed=False, all_stages_passed=False)
                raise
            finally:
                try:
                    d.write(self.out / "source-after.json", p.verify_source(self.plan))
                except BaseException as error:
                    result.update(status="SESSION_FATAL", error=str(error), all_stages_passed=False)
                    raise
                finally:
                    result["trace_bytes_remaining"] = self.trace_remaining
                    result["failed_stages"] = [x["hosts"] for x in result["stages"] if x["status"] != "PASS"]
                    result["scaling"] = summarize_scaling(result["stages"]) if result["all_stages_passed"] and result.get("cleanup_passed") is True else summarize_scaling([])
                    d.write(self.out / "scaling-result.json", result)
        return result


def produce(path, verify=False):
    grant = producer_grant(d.read(path))
    d.identity(grant["node_id"])
    work = n.no_symlinks(grant["work"], grant["root"] if "root" in grant else "/srv/otel-horizontal/" + grant["campaign"])
    p.require(p.sha(path) == p.sha(work / "plan.json"), "scaling producer grant mismatch")
    if verify:
        for mode in ("audit", "query"):
            n.run_app(grant, mode)
        return
    d.write(work / "ready.json", {"node_id": grant["node_id"], "run": grant["run"], "token": grant["token"], "grant_sha256": p.sha(path)})
    until = time.monotonic() + d.remaining(grant, 60)
    while not (work / "release.json").exists():
        p.require(time.monotonic() < until and not (work / "abort.json").exists(), "scaling barrier timeout/abort")
        time.sleep(0.05)
    p.require(d.read(work / "release.json") == {"token": grant["token"], "grant_sha256": p.sha(path)}, "scaling release binding")
    n.run_app(grant, "ingest")


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("run", "_produce", "_verify", "_release", "_abort", "_read", "_collect"))
    parser.add_argument("--plan"); parser.add_argument("--output"); parser.add_argument("--path")
    parser.add_argument("--node-id", type=int); parser.add_argument("--execute-approved", action="store_true")
    args = parser.parse_args(argv)
    if args.mode == "run":
        plan = validate_plan(d.read(args.plan))
        if not args.execute_approved:
            print(json.dumps({"validated": True, "schema": SCHEMA, "roles": sorted(ROLES), "nodes": 6, "stages": STAGES,
                "authorized_rows": AUTHORIZED_ROWS, "cumulative_rows_upper": plan["prior_rows_upper"] + AUTHORIZED_ROWS,
                "storage_process_count": 3, "storage_memory_profile": "scaling3ss3g", "primary_metric": "drained_rows_per_second",
                "provisioned": False, "boundary": BOUNDARY}, sort_keys=True))
            return
        p.require(args.output, "scaling output required")
        import provision
        result = ScalingSession(plan, args.output, provision).run()
        p.require(result["all_stages_passed"], "one or more scaling stages did not pass")
        return
    d.identity(args.node_id)
    if args.mode == "_read":
        path = Path(args.path)
        p.require(len(path.parts) >= 5 and path.parts[:3] == ("/", "srv", "otel-horizontal"), "owned scaling receipt path")
        n.owned_path(path.parts[3], str(path))
        n.no_symlinks(path, Path(*path.parts[:4]))
        print(json.dumps(d.read(path), sort_keys=True))
        return
    grant = producer_grant(d.read(args.path))
    p.require(grant["node_id"] == args.node_id, "wrong scaling producer host")
    base = "/srv/otel-horizontal/" + grant["campaign"]
    work = n.no_symlinks(grant["work"], base)
    if args.mode in ("_produce", "_verify"):
        produce(args.path, args.mode == "_verify")
    elif args.mode == "_collect":
        artifacts = {}
        for mode in ("ingest", "audit", "query"):
            for suffix in ("json", "command.json", "exit.json", "stdout", "stderr"):
                path = n.no_symlinks(work / (mode + "." + suffix), base)
                if path.exists():
                    p.require(path.is_file() and path.stat().st_size < 16 * n.MIB, "bounded scaling receipt file")
                    artifacts[path.name] = path.read_text()
        print(json.dumps({"run": grant["run"], "node_id": args.node_id, "artifacts": artifacts}, sort_keys=True))
    else:
        d.write(work / ("release.json" if args.mode == "_release" else "abort.json"), {"token": grant["token"], "grant_sha256": p.sha(args.path)})


if __name__ == "__main__":
    main()
