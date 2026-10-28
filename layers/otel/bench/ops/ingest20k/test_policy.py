#!/usr/bin/env python3
"""Offline fresh-100k admission/receipt fixtures; never start a database."""
import contextlib
import copy
import hashlib
import importlib
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

HERE = Path(__file__).resolve().parent
GIB = 1024 ** 3
RUNS = {
    "prof20k0290": {"session": "prof20k0290", "container": "otel283-fdb4-prof20k0290-vxnar",
                    "volume": "otel283-vxnar-fdb4-prof20k029016-v1"},
    "prof20k0291": {"session": "prof20k0291", "container": "otel283-fdb4-prof20k0291-vxnar",
                    "volume": "otel283-vxnar-fdb4-prof20k029116-v1"},
}
PROFILE = {
    "owner": "otel283-vxnar", "profile": "fdb4-4cpu-16g-ingest20k-v1", "count": 100000,
    "batch": 32, "workers": 2, "ports": [14883, 14884, 14885, 14886],
    "image": "foundationdb/foundationdb@sha256:559b178a137bbe09272ad6e3752531aa04e2ddacab8ccfa4ffa86467db033bea",
    "total_seconds": 1200, "load_seconds": 300, "drain_seconds": 300,
    "census_seconds": 120, "oracle_seconds": 120,
}
CLASSES = ("trace", "trace_absent", "span", "span_absent", "genai_response",
           "genai_scope_resource", "genai_scope_scope", "genai_type", "genai_absent",
           "genai_operation", "genai_provider", "genai_model", "time_narrow",
           "time_empty", "time_absent", "time_broad")


def approval(run="prof20k0290", revision="1" * 40, batch=32, workers=2):
    return dict(PROFILE, **RUNS[run], run=run, batch=batch, workers=workers,
                schema="ingest20k_fresh100k_v1", approved=True,
                source_revision=revision, source_approved_revision=revision,
                binary_sha256="b" * 64, input_manifest_sha256="c" * 64,
                policy_sha256="d" * 64, policy_approved_sha256="d" * 64,
                probe_sha256="e" * 64, probe_source_sha256="f" * 64,
                quiet_thresholds_approved=True, growth_overshoot_accepted=True,
                binary="/fixture/application", source_root="/fixture/source",
                source_manifest="/fixture/manifest", probe_binary="/fixture/inode-flags",
                out="/fixture/ingest20k-" + run)


def capacity():
    return {"total_bytes": 1000 * GIB, "available_bytes": 260 * GIB,
            "allocated_bytes": 64 * GIB - 1, "host_receipt_bytes": GIB - 1,
            "host_available_bytes": 8 * GIB, "cgroup_bytes": 16 * GIB * 9 // 10 - 1}


def quiet():
    return {"samples": [{"epoch": i * 10, "mem_available_bytes": 24 * GIB,
                         "cpu_psi_some_avg10": 5, "memory_psi_some_avg10": 0.1,
                         "memory_psi_full_avg10": 0.01, "io_psi_full_avg10": 1,
                         "pswpin": 17, "pswpout": 23} for i in range(7)]}


def readiness():
    return {"samples": [{"epoch": i * 5, "aggregate_healthy_original": True,
                         "queue_bytes": 16777215,
                         "durability_lag_seconds": 9.999} for i in range(3)]}


def phase(run="prof20k0290", stage="load"):
    a = approval(run)
    return {**{k: a[k] for k in ("run", "session", "profile", "count", "source_revision",
                               "binary_sha256", "input_manifest_sha256", "policy_sha256")},
            "instance": "a" * 64, "started_epoch": 1000, "deadline_epoch": 2200,
            "now": 1001, "stage": stage, "stage_started_epoch": 1000,
            "stage_deadline_epoch": 1000 + {"load": 300, "drain": 300, "census": 120, "oracle": 120}[stage]}


def evidence(mode, run="prof20k0290", revision="1" * 40, batch=32, workers=2):
    raw = {"tool": "otelbench-v2", "backend": "fdb", "revision": revision,
           "mode": mode, "failures": 0,
           "config": {"run": run, "backend": "fdb", "count": 100000, "tenants": 1,
                      "attributes": 8, "seed": 283, "match_every": 100, "batch": batch,
                      "save_mode": "batch", "workers": workers, "diagnostics": True,
                      "tenant": 0, "k": 100, "max_candidates": 2000000,
                      "max_bytes": 268435456, "hash_bytes": 32, "order": "time"}}
    if mode in ("ingest", "audit"):
        raw["stats"] = {"rows": 100000, "attributes": 800000}
        if mode == "audit":
            raw["stats"]["structures"] = {name: {"keys": count} for name, count in (
                ("records", 100000), ("eq_secondary", 800000), ("span_id_v2", 100000),
                ("span_time_v2", 100000), ("owned_total", 1100002), ("eq_primary", 0))}
    else:
        raw.update(suite="all", samples=[{"iteration": i, "class": CLASSES[i % 16],
                                        "ns": i, "result_digest": format(i, "064x")}
                                       for i in range(1600)])
        raw["config"]["iterations"] = 1600
    return {"approved": approval(run, revision, batch, workers), "evidence": raw}


def bootstrap_get():
    return {"path": "/var/fdb/data", "flags": 0, "nocow": False, "compression_flag": False,
            "uid": 0, "dev": 17, "ino": 1, "nlink": 1, "size": 0, "allocated_bytes": 0,
            "mount_id": 23, "filesystem": "btrfs", "nodatasum": "NOT_EXPOSED"}


def inode_proof():
    paths = ["/var/fdb/data", "/var/fdb/data/processes", "/var/fdb/data/traces"]
    paths += [f"/var/fdb/data/{area}/{port}" for area in ("processes", "traces")
              for port in range(14883, 14887)]
    paths += ["/var/fdb/data/cluster/fdb.cluster", "/var/fdb/data/.otel283-owner",
              "/var/fdb/data/processes/14883/storage-" + "a" * 32 + ".redwood-v1",
              "/var/fdb/data/processes/14886/storage-" + "b" * 32 + ".redwood-v1"]
    paths += ["/var/fdb/data/processes/14884/" + name for name in (
        "log2-V_6-" + "c" * 32 + ".sqlite", "log2-V_6-" + "c" * 32 + ".sqlite-wal",
        "logqueue-V_6-" + "c" * 32 + "-0.fdq", "logqueue-V_6-" + "c" * 32 + "-1.fdq")]
    paths.append("/var/fdb/data/cluster")
    rows = [{"path": path, "flags": 0, "nocow": False, "compression_flag": False,
             "uid": 4059, "dev": 17, "ino": i + 1, "nlink": 1, "size": 0,
             "allocated_bytes": 0, "mount_id": 23, "filesystem": "btrfs", "nodatasum": "NOT_EXPOSED"}
            for i, path in enumerate(paths)]
    rows.append({"schema": "ingest20k_inode_proof_v1", "regular_files_verified": 8,
                 "primary_sets_verified": 3})
    return {"rows": rows}


def set_path(value, path, replacement):
    parts = path.split(".")
    for part in parts[:-1]:
        value = value[int(part)] if isinstance(value, list) else value[part]
    if isinstance(value, list):
        value[int(parts[-1])] = replacement
    else:
        value[parts[-1]] = replacement


def status():
    processes = {}
    for i, (kind, roles) in enumerate((
            ("storage", ["storage"]), ("transaction", ["log"]),
            ("stateless", ["commit_proxy", "grv_proxy", "resolver", "master", "cluster_controller"]),
            ("storage", ["storage"]))):
        processes[str(i)] = {"address": "127.0.0.1:" + str(14883 + i), "class_type": kind,
                             "roles": [{"role": role} for role in roles], "messages": [],
                             "memory": {"rss_bytes": 1000, "used_bytes": 2000, "limit_bytes": 4294967296},
                             "locality": {"machineid": "otel283-vxnar-ingest20k",
                                          "zoneid": "otel283-vxnar-ingest20k"}}
    return {"client": {"database_status": {"available": True, "healthy": True},
                       "coordinators": {"quorum_reachable": True},
                       "cluster_file": {"up_to_date": True}, "messages": []},
            "cluster": {"messages": [], "database_available": True, "data": {"state": {"healthy": True}},
                        "recovery_state": {"name": "fully_recovered"}, "degraded_processes": 0,
                        "configuration": {"storage_engine": "ssd-redwood-1", "log_engine": "ssd-2",
                                          "redundancy_mode": "single", "logs": 1, "commit_proxies": 1,
                                          "grv_proxies": 1, "resolvers": 1},
                        "qos": {"worst_queue_bytes_storage_server": 0,
                                "worst_durability_lag_storage_server": {"seconds": 0},
                                "performance_limited_by": {"name": "workload"}},
                        "latency_probe": {"transaction_start_seconds": 0.1,
                                          "immediate_priority_transaction_start_seconds": 0.2,
                                          "read_seconds": 0.3, "commit_seconds": 0.4},
                        "processes": processes}}


def startup_status(name="initializing_transaction_servers"):
    value = status()
    value["cluster"].update(connection_string="otel20k:otel20k@127.0.0.1:14883",
                            recovery_state={"name": name}, database_available=False)
    value["client"]["database_status"].update(available=False, healthy=False)
    value["cluster"]["data"]["state"]["name"] = "healthy"
    return value


class PolicyTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.work = Path(self.temp.name)
        self.bin = self.work / "bin"
        self.bin.mkdir()
        self.denied = self.work / "host-command"
        for name in ("docker", "fdbcli", "ssh", "sudo", "systemctl", "kill", "rm", "mount"):
            path = self.bin / name
            path.write_text('#!/bin/sh\nprintf "%s\\n" "$0" >> "$DENIED_LOG"\nexit 97\n')
            path.chmod(0o755)
        self.env = dict(os.environ, PATH=str(self.bin), DENIED_LOG=str(self.denied),
                        PYTHONDONTWRITEBYTECODE="1")

    def cli(self, kind, value=None, *, valid=True, raw=None):
        text = json.dumps(value) if raw is None else raw
        source = self.work / "input.json"
        source.write_text(text)
        result = subprocess.run([sys.executable, "-B", str(HERE / "policy.py"), kind, str(source)],
                                cwd=self.work, env=self.env, capture_output=True, text=True, timeout=10)
        self.assertFalse(self.denied.exists(), "pure guard invoked a host command")
        self.assertEqual(source.read_text(), text)
        self.assertEqual(sorted(p.name for p in self.work.iterdir()), ["bin", "input.json"])
        self.assertEqual(result.returncode == 0, valid, result.stdout + result.stderr)
        return json.loads(result.stdout) if valid else result

    def reject(self, kind, base, path, values):
        for value in values:
            with self.subTest(kind=kind, path=path, value=value):
                candidate = copy.deepcopy(base)
                set_path(candidate, path, value)
                self.cli(kind, candidate, valid=False)

    def test_positive_population_both_arms_independent_approved_revisions(self):
        count = 0
        for run in RUNS:
            for revision in ("1" * 40, "2" * 40):
                self.assertEqual(self.cli("approval", approval(run, revision)), {"approved": True})
                for mode in ("ingest", "audit", "query"):
                    self.assertEqual(self.cli("evidence", evidence(mode, run, revision)),
                                     {"mode": mode, "verified": True})
                    count += 1
        self.assertEqual(count, 12)
        for kind, value, expected in (
                ("capacity", capacity(), {"floor_bytes": 260 * GIB, "allocated_bytes": 64 * GIB - 1}),
                ("quiet", quiet(), {"quiet_samples": 7, "span_seconds": 60}),
                ("readiness", readiness(), {"strict_samples": 3}),
                ("backlog", {"queue_bytes": 536870913, "lag_seconds": 61, "previous_streak": 0}, {"streak": 1})):
            self.assertEqual(self.cli(kind, value), expected)

    def test_all_six_explicit_config_pairs_bind_approval_and_evidence(self):
        population = set()
        for run in RUNS:
            for batch in (32, 64):
                for workers in (2, 4, 8):
                    population.add((batch, workers))
                    approved = approval(run, "3" * 40, batch, workers)
                    self.assertEqual(self.cli("approval", approved), {"approved": True})
                    for mode in ("ingest", "audit", "query"):
                        base = evidence(mode, run, "3" * 40, batch, workers)
                        self.assertEqual(self.cli("evidence", base), {"mode": mode, "verified": True})
                        self.reject("evidence", base, "evidence.config.batch", [64 if batch == 32 else 32])
                        self.reject("evidence", base, "evidence.config.workers", [4 if workers == 2 else 2])
        self.assertEqual(population, {(32, 2), (32, 4), (32, 8), (64, 2), (64, 4), (64, 8)})
        for field, values in {
            "batch": [False, True, 0, 31, 33, 63, 65, 32.0, "32", None],
            "workers": [False, True, 0, 1, 3, 5, 9, 2.0, "2", None],
        }.items():
            self.reject("approval", approval(), field, values)
        future = approval(batch=64, workers=8)
        future.update(run="prof20k0292", session="prof20k0292",
                      container="otel283-fdb4-prof20k0292-vxnar",
                      volume="otel283-vxnar-fdb4-prof20k029216-v1", out="/fixture/ingest20k-prof20k0292")
        self.assertEqual(self.cli("approval", future), {"approved": True})
        receipt = evidence("ingest", batch=64, workers=8)
        receipt["approved"] = future
        receipt["evidence"]["config"]["run"] = "prof20k0292"
        self.assertEqual(self.cli("evidence", receipt), {"mode": "ingest", "verified": True})

    def test_literal_arm_approval_missing_typed_hash_path_and_count_guards(self):
        for run in RUNS:
            base = approval(run)
            for field, expected in base.items():
                if type(expected) is bool:
                    values = [False, 1, None, "true"]
                elif type(expected) is int:
                    values = [expected + 1, float(expected), True, None]
                elif type(expected) is list:
                    values = [[], expected[::-1], [float(port) for port in expected], None]
                else:
                    values = [None, "", "wrong", True]
                self.reject("approval", base, field, values)
                candidate = copy.deepcopy(base)
                del candidate[field]
                self.cli("approval", candidate, valid=False)
            for field in ("source_revision", "source_approved_revision"):
                self.reject("approval", base, field, ["g" * 40, "a" * 39, "a" * 41, "A" * 40])
            for field in ("binary_sha256", "input_manifest_sha256", "policy_sha256", "probe_sha256", "probe_source_sha256"):
                self.reject("approval", base, field, ["g" * 64, "a" * 63, "a" * 65, "A" * 64])
            self.reject("approval", base, "count", [10000000, 100000000])
            self.reject("approval", base, "run", ["prof20k292", "prof20k00292", "prof20k029x",
                                                    "prof20k0290\n", "../prof20k0290", "rw10m0289"])
            for field in ("binary", "source_root", "source_manifest", "probe_binary", "out"):
                self.reject("approval", base, field, ["relative/path"])

    def test_approvals_reject_other_well_formed_source_and_policy_identities(self):
        base = approval()
        for field, replacement in (("source_revision", "2" * 40),
                                   ("source_approved_revision", "2" * 40),
                                   ("policy_sha256", "e" * 64),
                                   ("policy_approved_sha256", "e" * 64)):
            self.reject("approval", base, field, [replacement])
        matching = approval(revision="2" * 40)
        matching.update(policy_sha256="e" * 64, policy_approved_sha256="e" * 64)
        self.assertEqual(self.cli("approval", matching), {"approved": True})

    def test_strict_json_vacuity_and_missing_fields(self):
        for raw in ("", "{} {}", '{"samples":[],"samples":[]}', '{"x":NaN}',
                    '{"x":Infinity}', '{"x":-Infinity}', '{"x":1e999}', "null", "[]", "true", "17"):
            self.cli("quiet", raw=raw, valid=False)
        self.cli("unknown", {}, valid=False)
        for kind, base in (("capacity", capacity()), ("quiet", quiet()), ("readiness", readiness()),
                           ("phase", phase()), ("evidence", evidence("audit")),
                           ("backlog", {"queue_bytes": 0, "lag_seconds": 0, "previous_streak": 0})):
            self.cli(kind, {}, valid=False)
            for field in base:
                candidate = copy.deepcopy(base)
                del candidate[field]
                self.cli(kind, candidate, valid=False)

    def test_capacity_threshold_rounding_unsigned_and_relationships(self):
        base = capacity()
        for field, values in {
            "total_bytes": [0, -1, True, "0", None, 10 ** 18],
            "available_bytes": [260 * GIB - 1, 1000 * GIB + 1, -1, True, "0", None],
            "allocated_bytes": [64 * GIB, GIB - 2, -1, True, "0", None],
            "host_receipt_bytes": [GIB, -1, True, "0", None],
            "host_available_bytes": [8 * GIB - 1, -1, True, "0", None],
            "cgroup_bytes": [16 * GIB * 9 // 10, -1, True, "0", None],
        }.items():
            self.reject("capacity", base, field, values)
        for field in base:
            self.reject("capacity", base, field, [0.5, 10 ** 18])
        base.update(total_bytes=1000 * GIB + 1, available_bytes=260 * GIB + 4096)
        self.assertEqual(self.cli("capacity", base)["floor_bytes"], 260 * GIB + 4096)
        base["available_bytes"] -= 1
        self.cli("capacity", base, valid=False)
        base = capacity()
        base.update(allocated_bytes=0, host_receipt_bytes=0)
        self.assertEqual(self.cli("capacity", base)["allocated_bytes"], 0)

    def test_quiet_population_interval_ram_swap_psi_and_missing_samples(self):
        for count in (0, 1, 6, 8):
            base = quiet()
            base["samples"] = (base["samples"] * 2)[:count]
            self.cli("quiet", base, valid=False)
        for field, values in {
            "epoch": [-1, True, "0", 19, 36],
            "mem_available_bytes": [24 * GIB - 1, 25 * GIB + 1, True, "0"],
            "pswpin": [18, -1, True, "17"], "pswpout": [24, -1, True, "23"],
            "cpu_psi_some_avg10": [5.001, -1, True, "0"],
            "memory_psi_some_avg10": [0.1001, -1, True, "0"],
            "memory_psi_full_avg10": [0.0101, -1, True, "0"],
            "io_psi_full_avg10": [1.001, -1, True, "0"],
        }.items():
            self.reject("quiet", quiet(), "samples.2." + field, values + [None])
            base = quiet()
            del base["samples"][2][field]
            self.cli("quiet", base, valid=False)
        base = quiet()
        for i, sample in enumerate(base["samples"]):
            sample["epoch"] = i * 15
        base["samples"][2]["mem_available_bytes"] += GIB
        self.assertEqual(self.cli("quiet", base)["span_seconds"], 90)
        for kind, factory in (("quiet", quiet), ("readiness", readiness)):
            self.reject(kind, factory(), "samples", [None, {}, "samples", True])
            self.reject(kind, factory(), "samples.1", [None, [], "sample", True])

    def quiet_policy(self):
        with mock.patch.object(sys, "path", [str(HERE), *sys.path]):
            return importlib.import_module("policy")

    def test_partial_quiet_observations_and_exact_full_proof_are_unchanged(self):
        policy = self.quiet_policy()
        for count in range(1, 8):
            samples = quiet()["samples"][:count]
            self.assertIsNone(policy.quiet_observations(samples))
        for gap in (10, 15):
            samples = quiet()["samples"]
            for i, sample in enumerate(samples):
                sample["epoch"] = i * gap
            samples[0]["mem_available_bytes"] += GIB
            self.assertIsNone(policy.quiet_observations(samples))
            self.assertEqual(policy.quiet({"samples": samples}), {"quiet_samples": 7, "span_seconds": 6 * gap})
        for count in (0, 1, 6, 8):
            samples = (quiet()["samples"] * 2)[:count]
            with self.assertRaises(ValueError) as error:
                policy.quiet({"samples": samples})
            self.assertIs(type(error.exception), ValueError)

    def test_only_valid_known_busy_quiet_facts_raise_typed_busy(self):
        policy = self.quiet_policy()
        for field, value, reason in (
                ("mem_available_bytes", 24 * GIB - 1, "below 24 GiB"),
                ("mem_available_bytes", 25 * GIB + 1, "RAM unstable"),
                ("cpu_psi_some_avg10", 5.001, "cpu_psi_some_avg10"),
                ("memory_psi_some_avg10", 0.1001, "memory_psi_some_avg10"),
                ("memory_psi_full_avg10", 0.0101, "memory_psi_full_avg10"),
                ("io_psi_full_avg10", 1.001, "io_psi_full_avg10")):
            samples = quiet()["samples"]
            samples[0][field] = value
            with self.assertRaises(policy.QuietBusy) as error:
                policy.quiet_observations(samples)
            self.assertIs(type(error.exception), policy.QuietBusy)
            self.assertIn(reason, str(error.exception))
            with self.assertRaises(policy.QuietBusy):
                policy.quiet({"samples": samples})
        for counter in ("pswpin", "pswpout"):
            samples = quiet()["samples"]
            for sample in samples[2:]:
                sample[counter] += 1
            with self.assertRaises(policy.QuietBusy) as error:
                policy.quiet_observations(samples)
            self.assertIn("swap I/O", str(error.exception))
        for count in (1, 2, 7):
            samples = quiet()["samples"][:count]
            samples[0]["mem_available_bytes"] = 24 * GIB - 1
            with self.assertRaises(policy.QuietBusy):
                policy.quiet_observations(samples)

    def test_malformed_quiet_facts_are_fatal_even_when_another_fact_is_busy(self):
        policy = self.quiet_policy()
        for value in (None, {}, [], "samples", True, tuple(quiet()["samples"]), quiet()["samples"] * 2):
            with self.assertRaises(ValueError) as error:
                policy.quiet_observations(value)
            self.assertIs(type(error.exception), ValueError)
        bad = {"epoch": [True, "0", -1, 0.5, 10 ** 18],
               "mem_available_bytes": [True, "0", -1, 0.5, 10 ** 18],
               "pswpin": [True, "17", -1, 0.5, 10 ** 18], "pswpout": [True, "23", -1, 0.5, 10 ** 18]}
        for key in ("cpu_psi_some_avg10", "memory_psi_some_avg10", "memory_psi_full_avg10", "io_psi_full_avg10"):
            bad[key] = [True, "0", -1, float("inf"), float("-inf"), float("nan")]
        for malformed_index, busy_index in ((0, 6), (6, 0)):
            for field, values in bad.items():
                for value in values + [None]:
                    samples = quiet()["samples"]
                    samples[busy_index]["mem_available_bytes"] -= 1
                    samples[malformed_index][field] = value
                    with self.assertRaises(ValueError) as error:
                        policy.quiet_observations(samples)
                    self.assertIs(type(error.exception), ValueError, (field, value))
                samples = quiet()["samples"]
                samples[busy_index]["cpu_psi_some_avg10"] += 1
                del samples[malformed_index][field]
                with self.assertRaises(ValueError) as error:
                    policy.quiet_observations(samples)
                self.assertIs(type(error.exception), ValueError)
            for malformed in (None, [], "sample", True):
                samples = quiet()["samples"]
                samples[busy_index]["mem_available_bytes"] -= 1
                samples[malformed_index] = malformed
                with self.assertRaises(ValueError) as error:
                    policy.quiet_observations(samples)
                self.assertIs(type(error.exception), ValueError)

    def test_bad_quiet_intervals_or_decreasing_either_swap_counter_never_mean_busy(self):
        policy = self.quiet_policy()
        for epoch in (0, 9, 16):
            samples = quiet()["samples"][:2]
            samples[0]["cpu_psi_some_avg10"] += 1
            samples[1]["epoch"] = epoch
            with self.assertRaises(ValueError) as error:
                policy.quiet_observations(samples)
            self.assertIs(type(error.exception), ValueError)
        for decreasing, increasing in (("pswpin", "pswpout"), ("pswpout", "pswpin")):
            samples = quiet()["samples"][:2]
            samples[0]["mem_available_bytes"] -= 1
            samples[1][decreasing] -= 1
            samples[1][increasing] += 1
            with self.assertRaises(ValueError) as error:
                policy.quiet_observations(samples)
            self.assertIs(type(error.exception), ValueError)
            self.assertIn("decreased", str(error.exception))
        samples = quiet()["samples"]
        samples[0]["mem_available_bytes"] -= 1
        samples[-1]["pswpout"] -= 1
        with self.assertRaises(ValueError) as error:
            policy.quiet_observations(samples)
        self.assertIs(type(error.exception), ValueError)

    def test_phase_every_stage_is_strict_short_and_bound_to_arm(self):
        for run in RUNS:
            for stage in ("load", "drain", "census", "oracle"):
                base = phase(run, stage)
                self.assertEqual(self.cli("phase", base), {"phase_valid": True})
                self.reject("phase", base, "stage_deadline_epoch", [base["stage_deadline_epoch"] + 1, 1000, 2201])
                self.reject("phase", base, "now", [999, base["stage_deadline_epoch"], 2201])
                base["now"] = base["stage_deadline_epoch"] - 1
                self.cli("phase", base)
            base = phase(run)
            self.reject("phase", base, "deadline_epoch", [2199, 2201, 22600])
            self.reject("phase", base, "stage_started_epoch", [999, 1002])
            outside = phase(run, "oracle")
            outside.update(stage_started_epoch=2150, stage_deadline_epoch=2270, now=2151)
            self.cli("phase", outside, valid=False)
            self.reject("phase", base, "count", [99999, 100001, 10000000, 100000000, 100000.0, True])
            for field in ("run", "session", "profile", "source_revision", "instance",
                          "binary_sha256", "input_manifest_sha256", "policy_sha256", "stage"):
                self.reject("phase", base, field, [None, "", "wrong", True])
            for field in ("started_epoch", "deadline_epoch", "now", "stage_started_epoch", "stage_deadline_epoch"):
                self.reject("phase", base, field, [None, -1, True, "1000", 1000.5, 10 ** 18])

    def test_readiness_and_backlog_thresholds_population_and_types(self):
        for count in (0, 1, 2, 4):
            base = readiness()
            base["samples"] = (base["samples"] * 2)[:count]
            self.cli("readiness", base, valid=False)
        for field, values in {
            "epoch": [0, 4, -1, True, "5"], "aggregate_healthy_original": [False, 1, "true"],
            "queue_bytes": [16777216, -1, True, "0"],
            "durability_lag_seconds": [10, -1, True, "0"],
        }.items():
            self.reject("readiness", readiness(), "samples.1." + field, values + [None])
            base = readiness()
            del base["samples"][1][field]
            self.cli("readiness", base, valid=False)
        for queue, lag, prior, expected in ((536870912, 61, 5, 0), (536870913, 60, 5, 0),
                                             (536870913, 60.001, 4, 5), (0, 0, 5, 0)):
            self.assertEqual(self.cli("backlog", {"queue_bytes": queue, "lag_seconds": lag,
                                                  "previous_streak": prior}), {"streak": expected})
        base = {"queue_bytes": 536870913, "lag_seconds": 61, "previous_streak": 5}
        self.cli("backlog", base, valid=False)
        for field in base:
            self.reject("backlog", dict(base, queue_bytes=0), field, [None, -1, True, "0"])
        self.reject("backlog", dict(base, queue_bytes=0), "previous_streak", [6, 0.5])

    def test_actual_evidence_config_source_error_and_small_population(self):
        for run in RUNS:
            for mode in ("ingest", "audit", "query"):
                base = evidence(mode, run)
                for path, values in {
                    "evidence.revision": ["2" * 40, "", None], "evidence.backend": ["memory", None],
                    "evidence.tool": ["wrong", None], "evidence.failures": [1, -1, True, 0.0, None],
                    "evidence.error": [False, 0, None, [], "failure"],
                }.items():
                    self.reject("evidence", base, path, values)
                for field, expected in base["evidence"]["config"].items():
                    values = [None, "wrong", not expected] if type(expected) is bool else [None, "wrong", True]
                    if type(expected) is int:
                        values += [expected + 1, float(expected)]
                    self.reject("evidence", base, "evidence.config." + field, values)
                if mode in ("ingest", "audit"):
                    for field, count in (("rows", 100000), ("attributes", 800000)):
                        self.reject("evidence", base, "evidence.stats." + field,
                                    [count - 1, count + 1, 10000000, True, float(count), None])
        base = evidence("audit")
        for name, value in base["evidence"]["stats"]["structures"].items():
            self.reject("evidence", base, "evidence.stats.structures." + name + ".keys",
                        [value["keys"] + 1, -1, True, float(value["keys"]), None])
            self.reject("evidence", base, "evidence.stats.structures." + name, [{}, None, []])
        base = evidence("query")
        self.assertEqual(len(base["evidence"]["samples"]), 1600)
        self.assertEqual({name: sum(s["class"] == name for s in base["evidence"]["samples"])
                          for name in CLASSES}, dict.fromkeys(CLASSES, 100))
        self.reject("evidence", base, "evidence.samples", [None, [], base["evidence"]["samples"][:-1]])
        self.reject("evidence", base, "evidence.suite", [None, "trace", ""])
        for index in (0, 15, 16, 1599):
            prefix = "evidence.samples." + str(index) + "."
            for field, values in {
                "iteration": [index + 1, float(index), True, None], "class": ["wrong", "", None],
                "ns": [-1, True, "0", None], "result_digest": ["a" * 63, "g" * 64, None],
                "error": [False, 0, None, [], "failure"],
            }.items():
                self.reject("evidence", base, prefix + field, values)
        base["evidence"]["error"] = ""
        for sample in base["evidence"]["samples"]:
            sample["error"] = ""
        self.cli("evidence", base)

    def test_initial_bootstrap_get_uses_actual_root_json_shape_and_root_uid(self):
        with mock.patch.object(sys, "path", [str(HERE), *sys.path]):
            policy = importlib.import_module("policy")
        fact = bootstrap_get()
        self.assertNotIn("type", fact)
        self.assertEqual(policy.bootstrap_get(fact), {"bootstrap_get_valid": True})
        self.assertEqual(self.cli("bootstrap_get", fact), {"bootstrap_get_valid": True})
        fact.update(flags=4, compression_flag=True)
        self.assertEqual(policy.bootstrap_get(fact), {"bootstrap_get_valid": True})
        self.assertEqual(self.cli("bootstrap_get", fact), {"bootstrap_get_valid": True})
        self.assertEqual(inode_proof()["rows"][0]["uid"], 4059, "postconfigure UID contract must remain unchanged")

    def test_initial_bootstrap_get_rejects_missing_malformed_or_non_cow_root_facts(self):
        with mock.patch.object(sys, "path", [str(HERE), *sys.path]):
            policy = importlib.import_module("policy")
        for value in (None, {}, [], "GET", True):
            with self.assertRaises(ValueError):
                policy.bootstrap_get(value)
        bad = {"path": ["/var/fdb/data/cluster", "/var/fdb/data/", "/var/fdb/data/../data", None],
               "flags": [2 ** 32, 0x00800000, 0x00800004], "uid": [1, 4059],
               "dev": [0], "ino": [0], "mount_id": [0], "nlink": [0],
               "nocow": [True, 0, "false", None], "compression_flag": [True, 0, "false", None],
               "filesystem": ["ext4", None], "nodatasum": [True, "verified", None]}
        for field in ("flags", "uid", "dev", "ino", "mount_id", "nlink", "size", "allocated_bytes"):
            bad.setdefault(field, []).extend([-1, True, 0.5, "0", 10 ** 18, None])
        for field, values in bad.items():
            for value in values:
                fact = bootstrap_get()
                fact[field] = value
                with self.assertRaises(ValueError):
                    policy.bootstrap_get(fact)
        for field in bootstrap_get():
            fact = bootstrap_get()
            del fact[field]
            with self.assertRaises(ValueError):
                policy.bootstrap_get(fact)

    def test_native_startup_progress_population_and_optional_sections(self):
        names = ("reading_coordinated_state", "locking_coordinated_state", "locking_old_transaction_servers",
                 "reading_transaction_system_state", "recruiting_transaction_servers", "initializing_transaction_servers",
                 "recovery_transaction", "writing_coordinated_state", "accepting_commits", "all_logs_recruited", "storage_recovered")
        self.assertEqual(len(names), 11)
        for name in names:
            self.assertEqual(self.cli("startup_pending", startup_status(name)),
                             {"startup_pending": True, "aggregate_healthy_original": False, "recovery_state": name})
        for healthy in (False, True):
            value = startup_status("fully_recovered")
            value["cluster"].update(database_available=True, data={"state": {"name": "initializing"}})
            value["client"]["database_status"].update(available=True, healthy=healthy)
            if healthy:
                self.assertEqual(self.cli("startup_pending", value),
                                 {"startup_pending": True, "aggregate_healthy_original": True,
                                  "recovery_state": "fully_recovered"})
            else:
                self.cli("startup_pending", value, valid=False)
        value = startup_status()
        for key in ("configuration", "data", "latency_probe", "qos"):
            del value["cluster"][key]
        value["cluster"]["processes"] = {"0": value["cluster"]["processes"]["0"]}
        value["cluster"]["processes"]["0"]["roles"] = []
        self.cli("startup_pending", value)
        for name in ("accepting_commits", "all_logs_recruited", "storage_recovered"):
            value = startup_status(name)
            value["cluster"]["database_available"] = True
            value["client"]["database_status"].update(available=True, healthy=True)
            self.cli("startup_pending", value)
            value["client"]["database_status"]["healthy"] = False
            self.cli("startup_pending", value, valid=False)

    def test_startup_pending_never_waives_unknown_warnings_bad_shapes_or_limits(self):
        base = startup_status()
        paths = {
            "cluster.recovery_state.name": ["unknown", "configuration_never_created", "fully_recovered", None],
            "cluster.connection_string": ["wrong", None],
            "client.coordinators.quorum_reachable": [False, 1, None],
            "client.cluster_file.up_to_date": [False, 1, None],
            "client.database_status.available": [True, 0, None],
            "client.database_status.healthy": [True, 0, "false", None],
            "cluster.database_available": [0, "false", None],
            "client.messages": [None, {}, [{"name": "batch_priority_transaction_start_probe_timeout"}]],
            "cluster.messages": [None, {}, [{"name": "batch_priority_transaction_start_probe_timeout"}]],
            "cluster.degraded_processes": [1, True, 0.0, None],
            "cluster.configuration": [None, [], "configuration"],
            "cluster.configuration.logs": [0, 2, True, 1.0],
            "cluster.configuration.storage_engine": ["ssd-2", None],
            "cluster.processes": [{}, None, []],
            "cluster.processes.0.address": ["127.0.0.1:14683", None],
            "cluster.processes.0.class_type": ["unset", None],
            "cluster.processes.0.locality.machineid": ["wrong", None],
            "cluster.processes.0.locality.zoneid": ["wrong", None],
            "cluster.processes.0.messages": [None, [{"name": "warning"}]],
            "cluster.processes.0.roles": [None, [{"role": ""}], [{"role": 1}]],
            "cluster.processes.0.memory.limit_bytes": [4294967296.0, True, 1, None],
            "cluster.processes.0.memory.rss_bytes": [-1, True, "0", None],
            "cluster.processes.0.memory.used_bytes": [-1, True, "0", None],
            "cluster.data.state": [{"name": "initializing", "healthy": False}, {"name": "unhealthy", "healthy": True}, None],
            "cluster.latency_probe": [None, [], {"read_seconds": 5}, {"read_seconds": True}],
            "cluster.qos": [None, [], "qos"],
            "cluster.qos.worst_queue_bytes_storage_server": [-1, True, 0.5],
            "cluster.qos.worst_durability_lag_storage_server.seconds": [-1, True, None],
        }
        for path, values in paths.items():
            if path == "cluster.data.state":
                values = values + [[], "state"]
            self.reject("startup_pending", base, path, values)
        for path in ("cluster.connection_string", "cluster.recovery_state", "cluster.processes",
                     "cluster.degraded_processes", "cluster.messages", "client.messages",
                     "client.coordinators", "client.cluster_file", "client.database_status"):
            value = startup_status()
            parts = path.split(".")
            del value[parts[0]][parts[1]]
            self.cli("startup_pending", value, valid=False)
        value = startup_status()
        value["cluster"]["processes"]["extra"] = copy.deepcopy(value["cluster"]["processes"]["0"])
        self.cli("startup_pending", value, valid=False)
        value = startup_status()
        value["cluster"]["processes"]["1"]["address"] = value["cluster"]["processes"]["0"]["address"]
        self.cli("startup_pending", value, valid=False)

    def test_clean_stop_is_separate_from_confirmed_emergency_exit(self):
        clean = {"running": False, "status": "exited", "exit_code": 0, "oom": False,
                 "restarts": 0, "shutdown_ok": True}
        self.assertEqual(self.cli("clean_stop", clean), {"clean_stop": True})
        self.assertEqual(self.cli("clean_stop", {}), {"clean_stop": False})
        for field, values in {
            "running": [True, None, 0, "false"], "status": ["dead", "running", None],
            "exit_code": [137, 1, -1, True, False, 0.0, "0", None],
            "oom": [True, None, 0, "false"], "restarts": [1, -1, True, False, 0.0, "0", None],
            "shutdown_ok": [False, None, 1, "true"],
        }.items():
            for value in values:
                candidate = dict(clean)
                candidate[field] = value
                self.assertEqual(self.cli("clean_stop", candidate), {"clean_stop": False})
            candidate = dict(clean)
            del candidate[field]
            self.assertEqual(self.cli("clean_stop", candidate), {"clean_stop": False})

    def test_get_inode_proof_positive_population_flags_and_summary(self):
        base = inode_proof()
        self.assertEqual(len(base["rows"]), 21)
        self.assertEqual(self.cli("inode_proof", base), {"inodes_verified": 20})
        base["rows"][0].update(flags=4, compression_flag=True)
        self.assertEqual(self.cli("inode_proof", base), {"inodes_verified": 20})
        extra = dict(base["rows"][0], path="/var/fdb/data/traces/14885/trace.1.json", ino=99)
        base["rows"].insert(-1, extra)
        base["rows"][-1]["regular_files_verified"] = 9
        self.assertEqual(self.cli("inode_proof", base), {"inodes_verified": 21})
        for field, values in {"schema": [None, "wrong"], "regular_files_verified": [0, 7, 9, True, 8.0, None],
                              "primary_sets_verified": [0, 2, 4, True, 3.0, None]}.items():
            self.reject("inode_proof", inode_proof(), "rows.20." + field, values)
        for value in (None, [], {}, True):
            self.reject("inode_proof", inode_proof(), "rows", [value])
        for count in (0, 1, 17, 19):
            candidate = inode_proof()
            candidate["rows"] = candidate["rows"][:count]
            self.cli("inode_proof", candidate, valid=False)

    def test_cluster_directory_get_fact_is_required_owned_and_cow(self):
        base = inode_proof()
        matches = [i for i, row in enumerate(base["rows"][:-1])
                   if row["path"] == "/var/fdb/data/cluster"]
        self.assertEqual(len(matches), 1)
        index = matches[0]
        self.assertEqual(base["rows"][index],
                         {"path": "/var/fdb/data/cluster", "flags": 0, "nocow": False,
                          "compression_flag": False, "uid": 4059, "dev": 17, "ino": index + 1,
                          "nlink": 1, "size": 0, "allocated_bytes": 0, "mount_id": 23,
                          "filesystem": "btrfs", "nodatasum": "NOT_EXPOSED"})
        self.assertEqual(self.cli("inode_proof", base), {"inodes_verified": 20})
        for field, values in {
            "uid": [0, 4058, 4060, True, 4059.0, None],
            "flags": [0x00800000, 0x00800004, -1, True, None],
            "nocow": [True, None, 0], "filesystem": ["ext4", None],
            "ino": [0, None], "mount_id": [0, 24, None],
        }.items():
            self.reject("inode_proof", base, f"rows.{index}." + field, values)
        for field in base["rows"][index]:
            candidate = inode_proof()
            del candidate["rows"][index][field]
            self.cli("inode_proof", candidate, valid=False)
        candidate = inode_proof()
        del candidate["rows"][index]
        self.cli("inode_proof", candidate, valid=False)

    def test_get_inode_proof_paths_mount_uid_uuid_and_missing_facts(self):
        base = inode_proof()
        bad = {"path": [None, "relative", "/var/fdb/data/../escape", "/var/fdb/data/processes/14683"],
               "flags": [2 ** 32, 0x00800000, -1, True, "0", None],
               "nocow": [True, 0, "false", None], "compression_flag": [True, 0, None],
               "uid": [0, 4058, 4060, True, None], "dev": [18, -1, True, None],
               "mount_id": [0, 24, True, None], "ino": [0, -1, True, None],
               "nlink": [0, -1, True, None], "size": [-1, True, None],
               "allocated_bytes": [-1, True, None], "filesystem": ["ext4", None],
               "nodatasum": [True, False, "verified", None]}
        for field, values in bad.items():
            self.reject("inode_proof", base, "rows.1." + field, values)
            candidate = inode_proof()
            del candidate["rows"][1][field]
            self.cli("inode_proof", candidate, valid=False)
        for i in range(len(base["rows"]) - 1):
            candidate = inode_proof()
            del candidate["rows"][i]
            self.cli("inode_proof", candidate, valid=False)
        candidate = inode_proof()
        candidate["rows"].insert(0, copy.deepcopy(candidate["rows"][0]))
        self.cli("inode_proof", candidate, valid=False)
        for i in (13, 14, 15, 16, 17, 18):
            candidate = inode_proof()
            candidate["rows"][i]["path"] = candidate["rows"][i]["path"].replace(
                "a" * 32 if i == 13 else "b" * 32 if i == 14 else "c" * 32, "g" * 32)
            self.cli("inode_proof", candidate, valid=False)
        candidate = inode_proof()
        candidate["rows"][16]["path"] = candidate["rows"][16]["path"].replace("c" * 32, "d" * 32)
        self.cli("inode_proof", candidate, valid=False)

    def test_imports_are_inert(self):
        for module in ("policy", "driver"):
            script = 'import sys; sys.path.insert(0,sys.argv[1]); __import__(sys.argv[2])'
            result = subprocess.run([sys.executable, "-B", "-c", script, str(HERE), module],
                                    cwd=self.work, env=self.env, capture_output=True, text=True, timeout=10)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout, "")
            self.assertEqual(sorted(p.name for p in self.work.iterdir()), ["bin"])


class StrictStatusTests(unittest.TestCase):
    setUp = PolicyTests.setUp

    def jq(self, value, *, valid=True, raw=None):
        executable = shutil.which("jq")
        self.assertIsNotNone(executable, "jq must be available; never skip")
        result = subprocess.run([executable, "-e", "-f", str(HERE / "status-check.jq")],
                                input=json.dumps(value) if raw is None else raw, cwd=self.work,
                                env=self.env, capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode == 0, valid, result.stdout + result.stderr)
        self.assertFalse(self.denied.exists())
        self.assertEqual(sorted(p.name for p in self.work.iterdir()), ["bin"])
        return json.loads(result.stdout) if valid else None

    def test_strict_positive_and_batch_warning_is_always_rejected(self):
        result = self.jq(status())
        self.assertIs(result["aggregate_healthy_original"], True)
        self.assertNotIn("health_exception_used", result)
        self.assertEqual(len(result["processes"]), 4)
        value = status()
        value["client"]["database_status"]["healthy"] = False
        value["cluster"]["messages"] = [{"name": "batch_priority_transaction_start_probe_timeout"}]
        self.jq(value, valid=False)

    def test_malformed_structural_status_population_roles_and_probes(self):
        for value in (None, {}, [], True):
            self.jq(value, valid=False)
        self.jq(status(), raw=json.dumps(status()) + "\n{}", valid=False)
        for path, values in {
            "client.database_status.available": [False, None, 1],
            "client.database_status.healthy": [False, None, 1],
            "client.messages": [None, [{"name": "warning"}]],
            "client.coordinators.quorum_reachable": [False, None, 1],
            "client.cluster_file.up_to_date": [False, None, 1],
            "cluster.database_available": [False, None, 1],
            "cluster.messages": [None, [{"name": "warning"}]],
            "cluster.degraded_processes": [1, None, "0"],
            "cluster.data.state.healthy": [False, None, 1],
            "cluster.recovery_state.name": [None, "recovering"],
            "cluster.configuration.storage_engine": ["ssd-2", None],
            "cluster.configuration.logs": [0, 2, None],
            "cluster.configuration.log_engine": ["ssd-redwood-1", None],
            "cluster.configuration.redundancy_mode": ["double", None],
            "cluster.configuration.commit_proxies": [0, 2, None],
            "cluster.configuration.grv_proxies": [0, 2, None],
            "cluster.configuration.resolvers": [0, 2, None],
            "cluster.qos.worst_queue_bytes_storage_server": [-1, True, 0.5],
            "cluster.qos.worst_durability_lag_storage_server.seconds": [-1, None, True],
            "cluster.qos.performance_limited_by.name": ["", None, 1],
            "cluster.processes": [{}, None],
            "cluster.processes.0.address": ["127.0.0.1:14683", None],
            "cluster.processes.0.locality.machineid": ["wrong", None],
            "cluster.processes.0.locality.zoneid": ["wrong", None],
            "cluster.processes.0.class_type": ["unset", None],
            "cluster.processes.0.messages": [None, [{"name": "warning"}]],
            "cluster.processes.0.memory.rss_bytes": [-1, 0.5, True, None],
            "cluster.processes.0.memory.used_bytes": [-1, 0.5, True, None],
            "cluster.processes.0.memory.limit_bytes": [1, None],
            "cluster.processes.0.roles": [[], [{"role": "log"}], [{"role": "storage"}, {"role": "resolver"}]],
        }.items():
            for replacement in values:
                value = status()
                set_path(value, path, replacement)
                self.jq(value, valid=False)
        for name in status()["cluster"]["latency_probe"]:
            for replacement in (None, -1, True, 5):
                value = status()
                value["cluster"]["latency_probe"][name] = replacement
                self.jq(value, valid=False)
        for process, entry in status()["cluster"]["processes"].items():
            for index in range(len(entry["roles"])):
                value = status()
                del value["cluster"]["processes"][process]["roles"][index]
                self.jq(value, valid=False)
        for count in (1, 3, 5):
            value = status()
            entries = list(value["cluster"]["processes"].values())
            value["cluster"]["processes"] = {str(i): entries[i % 4] for i in range(count)}
            self.jq(value, valid=False)


class DriverSafetyTests(unittest.TestCase):
    def setUp(self):
        PolicyTests.setUp(self)
        with mock.patch.object(sys, "path", [str(HERE), *sys.path]):
            self.driver = importlib.import_module("driver")
        self.driver.select_arm(approval())
        for name in ("run", "Popen"):
            patcher = mock.patch.object(self.driver.subprocess, name, side_effect=AssertionError("unmocked host process"))
            patcher.start()
            self.addCleanup(patcher.stop)

    def runtime(self, run):
        self.driver.select_arm(approval(run))
        runtime = self.driver.Runtime.__new__(self.driver.Runtime)
        runtime.out, runtime.nonce, runtime.instance = self.work, "fixture-nonce", "a" * 64
        runtime.event = mock.Mock()
        for name in ("command", "identity", "volume", "resources"):
            setattr(runtime, name, mock.Mock(side_effect=AssertionError("stop must bypass " + name)))
        return runtime

    def inspected(self, run, running=True):
        return {"id": "a" * 64, "name": "/" + RUNS[run]["container"], "running": running,
                "status": "running" if running else "exited", "oom": True, "memory": 1,
                "labels": {"otel283.owner": PROFILE["owner"], "otel283.profile": PROFILE["profile"],
                           "otel283.run": run, "otel283.session": run, "otel283.nonce": "fixture-nonce"}}

    def test_arm_selection_is_literal_and_never_retained_identity(self):
        for run in RUNS:
            for batch in (32, 64):
                for workers in (2, 4, 8):
                    self.driver.select_arm(approval(run, batch=batch, workers=workers))
                    self.assertEqual(self.driver.P,
                                     dict(PROFILE, **RUNS[run], run=run, batch=batch, workers=workers))
                    self.assertEqual(self.driver.LABELS, {"otel283.owner": PROFILE["owner"],
                        "otel283.profile": PROFILE["profile"], "otel283.run": run, "otel283.session": run})

    def test_stop_pinned_exact_id_survives_no_marker_logging_and_resource_failure(self):
        for run in RUNS:
            runtime = self.runtime(run)
            runtime.event.side_effect = OSError("receipt disk full")
            responses = [subprocess.CompletedProcess([], 0, json.dumps(self.inspected(run)), ""),
                         subprocess.CompletedProcess([], 0, "stopped", ""),
                         subprocess.CompletedProcess([], 0, json.dumps(self.inspected(run, False)), "")]
            with mock.patch.object(self.driver.subprocess, "run", side_effect=responses) as command:
                with mock.patch.object(self.driver, "read", side_effect=AssertionError("stop read missing marker")):
                    with contextlib.redirect_stderr(io.StringIO()):
                        runtime.stop()
            self.assertEqual(command.call_count, 3)
            self.assertEqual(command.call_args_list[1].args[0], ["docker", "stop", "--time", "60", "a" * 64])
            for call in (command.call_args_list[0], command.call_args_list[2]):
                self.assertEqual(call.args[0][-1], "a" * 64)
                self.assertEqual(call.args[0][:2], ["docker", "inspect"])
            for name in ("command", "identity", "volume", "resources"):
                getattr(runtime, name).assert_not_called()
            self.assertIs(runtime.event.call_args.args[0]["confirmed"], True)

    def test_stop_refuses_replacement_wrong_ownership_and_unconfirmed_exit(self):
        for run in RUNS:
            for key in ("id", "name", *self.inspected(run)["labels"]):
                runtime = self.runtime(run)
                value = self.inspected(run)
                if key in ("id", "name"):
                    value[key] = "replacement"
                else:
                    value["labels"][key] = "replacement"
                with mock.patch.object(self.driver.subprocess, "run", return_value=
                                       subprocess.CompletedProcess([], 0, json.dumps(value), "")) as command:
                    with self.assertRaises(ValueError):
                        runtime.stop()
                self.assertEqual(command.call_count, 1)
            runtime = self.runtime(run)
            with mock.patch.object(self.driver.subprocess, "run", side_effect=[
                    subprocess.CompletedProcess([], 0, json.dumps(self.inspected(run)), ""),
                    subprocess.CompletedProcess([], 0, "", ""),
                    subprocess.CompletedProcess([], 0, json.dumps(self.inspected(run)), "")]):
                with self.assertRaises(ValueError):
                    runtime.stop()

    def test_emergency_stop_returns_bad_exit_state_without_claiming_clean_or_retrying(self):
        runtime = self.runtime("prof20k0290")
        after = dict(self.inspected("prof20k0290", False), exit_code=137, oom=True, restarts=0)
        with mock.patch.object(self.driver.subprocess, "run", side_effect=[
                subprocess.CompletedProcess([], 0, json.dumps(self.inspected("prof20k0290")), ""),
                subprocess.CompletedProcess([], 0, "stopped", ""),
                subprocess.CompletedProcess([], 0, json.dumps(after), "")]) as command:
            self.assertEqual(runtime.stop(), dict(after, shutdown_ok=True, shutdown_failures=[]))
        self.assertEqual(command.call_count, 3)
        runtime.stop = mock.Mock(return_value=after)
        with mock.patch.object(self.driver.time, "sleep") as sleep:
            self.assertEqual(runtime.stop_until_confirmed(), after)
        runtime.stop.assert_called_once_with()
        sleep.assert_not_called()

    def test_no_instance_no_database_calls_and_retry_ignores_failed_receipt(self):
        runtime = self.runtime("prof20k0290")
        runtime.instance = ""
        with mock.patch.object(self.driver.subprocess, "run") as command:
            runtime.stop()
        command.assert_not_called()
        runtime.stop = mock.Mock(side_effect=[ValueError("not confirmed"), None])
        runtime.event.side_effect = OSError("full")
        with mock.patch.object(self.driver.time, "sleep") as sleep:
            with contextlib.redirect_stderr(io.StringIO()):
                runtime.stop_until_confirmed()
        self.assertEqual(runtime.stop.call_count, 2)
        sleep.assert_called_once_with(5)

    def test_sequential_guard_refuses_any_live_retained_database(self):
        runtime = self.runtime("prof20k0290")
        old = "otel283-fdb4-cow10m-vxnar"
        stopped = {name: {"id": self.driver.OLD.get(name, "f" * 64), "name": "/" + name,
                          "running": False, "status": "exited", "exit_code": 0,
                          "oom": False, "restarts": 0}
                   for name in set(self.driver.OLD) | {old}}
        runtime.command = mock.Mock(side_effect=[old + "\n", ""])
        runtime.inspect = mock.Mock(side_effect=lambda name: copy.deepcopy(stopped[name]))
        self.driver.Runtime.sequential(runtime)
        self.assertEqual({call.args[0] for call in runtime.inspect.call_args_list}, set(stopped))
        self.assertIn(mock.call(old), runtime.inspect.call_args_list)

        live = copy.deepcopy(stopped)
        live[old].update(running=True, status="running")
        runtime.command = mock.Mock(side_effect=[old + "\n", ""])
        runtime.inspect = mock.Mock(side_effect=lambda name: copy.deepcopy(live[name]))
        with self.assertRaisesRegex(ValueError, "not cleanly stopped"):
            self.driver.Runtime.sequential(runtime)
        self.assertIn(mock.call(old), runtime.inspect.call_args_list)

    @contextlib.contextmanager
    def ready_fixture(self, documents, dirname):
        runtime = self.runtime("prof20k0290")
        runtime.out = self.work / dirname
        runtime.out.mkdir()
        runtime.frozen = "d" * 64
        runtime.phase = mock.Mock(return_value=None)
        clock = {"seconds": 1000, "index": 0}
        summary = {"aggregate_healthy_original": True,
                   "queue_bytes": 0, "durability_lag_seconds": 0}

        def raw_status(*_args, **_kwargs):
            document = documents[min(clock["index"], len(documents) - 1)]
            clock["index"] += 1
            raw = document if isinstance(document, str) else json.dumps(document, sort_keys=True)
            runtime.last_stdout = runtime.out / "status.raw.json"
            runtime.last_stdout.write_text(raw)
            return raw

        def jq_result(*_args, **kwargs):
            document = json.loads(kwargs["input"])
            cluster = document["cluster"]
            strict = (cluster["recovery_state"]["name"] == "fully_recovered"
                      and cluster["data"]["state"].get("healthy") is True)
            return subprocess.CompletedProcess([], 0 if strict else 1,
                                               json.dumps(summary) if strict else "", "")

        def sleep(seconds):
            self.assertEqual(seconds, 5)
            clock["seconds"] += seconds

        runtime.bounded_exec = mock.Mock(side_effect=raw_status)
        runtime.status = lambda *, bootstrap=False: self.driver.Runtime.status(runtime, bootstrap=bootstrap)
        guard = mock.Mock()
        with contextlib.ExitStack() as stack:
            stack.enter_context(mock.patch.object(self.driver.subprocess, "run", side_effect=jq_result))
            stack.enter_context(mock.patch.object(self.driver.time, "monotonic", side_effect=lambda: clock["seconds"]))
            stack.enter_context(mock.patch.object(self.driver.time, "time", side_effect=lambda: 10000 + clock["seconds"]))
            stack.enter_context(mock.patch.object(self.driver.time, "monotonic_ns", side_effect=lambda: clock["seconds"] * 10 ** 9))
            stack.enter_context(mock.patch.object(self.driver.time, "sleep", side_effect=sleep))
            yield runtime, guard, clock

    def test_pending_then_three_strict_samples_and_pending_resets_previous_sample(self):
        sequences = [[startup_status(), status(), status(), status()],
                     [status(), startup_status(), status(), status(), status()]]
        for i, documents in enumerate(sequences):
            with self.ready_fixture(documents, "ready-" + str(i)) as (runtime, guard, clock):
                self.driver.strict_ready(runtime, guard, "bootstrap.ready", 60, bootstrap=True)
                certificate = json.loads((runtime.out / "bootstrap.ready.json").read_text())
                self.assertEqual(certificate["instance"], "a" * 64)
                self.assertEqual(certificate["policy_sha256"], "d" * 64)
                samples = certificate["samples"]
                self.assertEqual(len(samples), 3)
                self.assertEqual([sample["epoch"] for sample in samples],
                                 [11005, 11010, 11015] if i == 0 else [11010, 11015, 11020])
                raw_sha = hashlib.sha256(runtime.last_stdout.read_bytes()).hexdigest()
                for sample in samples:
                    self.assertIs(sample["aggregate_healthy_original"], True)
                    self.assertNotIn("health_exception_used", sample)
                    self.assertNotIn("startup_pending", sample)
                    self.assertEqual(sample["raw_path"], "status.raw.json")
                    self.assertEqual(sample["raw_sha256"], raw_sha)
                self.assertEqual(clock["index"], len(documents))
                self.assertEqual(guard.call_count, 2 * len(documents))
                for call in guard.call_args_list:
                    self.assertEqual(call.kwargs, {"deadline": 1060})

    def test_unknown_malformed_batch_warning_and_expired_pending_never_certify(self):
        unknown = startup_status("unknown")
        warning = startup_status()
        warning["cluster"]["messages"] = [{"name": "batch_priority_transaction_start_probe_timeout"}]
        malformed = startup_status()
        malformed["cluster"]["processes"]["0"]["memory"]["limit_bytes"] = True
        for i, document in enumerate((unknown, warning, malformed, "{")):
            with self.ready_fixture([document], "rejected-" + str(i)) as (runtime, guard, clock):
                with self.assertRaises(ValueError):
                    self.driver.strict_ready(runtime, guard, "bootstrap.ready", 60, bootstrap=True)
                self.assertEqual(clock["index"], 1)
                self.assertFalse((runtime.out / "bootstrap.ready.json").exists())
        with self.ready_fixture([startup_status()], "expired") as (runtime, guard, clock):
            with self.assertRaisesRegex(ValueError, "deadline"):
                self.driver.strict_ready(runtime, guard, "bootstrap.ready", 60, deadline=1010, bootstrap=True)
            self.assertEqual(clock["index"], 2)
            for call in guard.call_args_list:
                self.assertEqual(call.kwargs, {"deadline": 1010})
            self.assertFalse((runtime.out / "bootstrap.ready.json").exists())

    def test_ordinary_drain_and_command_failures_never_use_startup_classifier(self):
        classifier = self.driver.p.startup_pending
        with self.ready_fixture([startup_status()], "strict-drain") as (runtime, guard, _clock):
            with mock.patch.object(self.driver.p, "startup_pending", wraps=classifier) as pending:
                with self.assertRaises(ValueError):
                    self.driver.strict_ready(runtime, guard, "drain.certificate", 60, bootstrap=False)
                pending.assert_not_called()
            self.assertFalse((runtime.out / "drain.certificate.json").exists())
        with self.ready_fixture([startup_status()], "command-failed") as (runtime, guard, _clock):
            runtime.bounded_exec.side_effect = ValueError("status command failed")
            with mock.patch.object(self.driver.p, "startup_pending", wraps=classifier) as pending:
                with self.assertRaisesRegex(ValueError, "command failed"):
                    self.driver.strict_ready(runtime, guard, "bootstrap.ready", 60, bootstrap=True)
                pending.assert_not_called()
            self.assertFalse((runtime.out / "bootstrap.ready.json").exists())

if __name__ == "__main__":
    unittest.main()
