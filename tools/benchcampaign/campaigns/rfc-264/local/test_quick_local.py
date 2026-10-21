"""Offline source tests. Run only after the owner queues validation; never contacts FDB."""
import contextlib
import copy
import csv
import json
import os
from pathlib import Path
import sys
import tempfile
import time
import unittest
from unittest import mock

import quick_local as q


HERE = Path(__file__).resolve().parent


def config():
    return json.loads((HERE / "rfc264.json").read_text())


def build_manifest():
    return {"schema": 1, "base_commit": config()["expected_base_commit"],
            "harness_patch_sha256": "a" * 64,
            "arms": {arm: {"binary": "/not-executed/" + arm, "binary_sha256": "b" * 64,
                           "source_revision": "c" * 40, "worktree": "/not-executed/tree-" + arm,
                           "prototype_patch_sha256": "d" * 64,
                           "go_version": "go version go1.26.0 linux/amd64"}
                     for arm in config()["arm_ids"]}}


class RawFixture:
    def __init__(self, root):
        self.root = Path(root)
        self.config = config()
        self.config["workers"] = [2]
        self.run = q.schedule(self.config)[0]
        self.prefix = q.campaign_prefix("local_offline_test")
        self.build = build_manifest()
        self.rows = [
            {"phase": "measure", "worker": 0, "index": 0, "latency_ns": 50,
             "attempts": 1, "error": "", "successful": "true", "completion_offset_ns": 100},
            {"phase": "measure", "worker": 1, "index": 1, "latency_ns": 100,
             "attempts": 2, "error": "temporary", "successful": "false", "completion_offset_ns": 200},
            {"phase": "measure", "worker": 0, "index": 2, "latency_ns": 150,
             "attempts": 3, "error": "", "successful": "true", "completion_offset_ns": 300},
            {"phase": "drain", "worker": 1, "index": 3, "latency_ns": 800,
             "attempts": 99, "error": "drain", "successful": "false", "completion_offset_ns": 30_000_000_010},
        ]
        arm = self.build["arms"][self.run["arm"]]
        identity = {"backend": "go", "api_version": 730, "native_library": False,
                    "executable": arm["binary"], "executable_sha256": arm["binary_sha256"]}
        metadata = {"workload": self.run["workload"], "workers": 2,
                    "duration_ns": 30_000_000_000, "warmup_duration_ns": 10_000_000_000,
                    "prefix": self.prefix + self.run["run_id"] + "/",
                    "source_revision": arm["source_revision"], "harness_id": "a" * 64}
        self.manifest = {"config": metadata, "go_version": "go1.26.0", "backend": identity}
        self.summary = {"config": copy.deepcopy(metadata), "backend": copy.deepcopy(identity),
                        "success": True, "persisted_validation": "ok", "attempts_scope": q.ATTEMPTS_SCOPE,
                        "requested_duration_ns": 30_000_000_000, "measured_wall_ns": 30_000_000_000,
                        "timer_lateness_ns": 0, "process_cpu_ns": 300, "boundary_drain_raw": 1,
                        "boundary_drain_wall_ns": 10}
        self.recount()
        self.write()

    def recount(self):
        observed = [row for row in self.rows if row["phase"] == "measure"]
        successes = [row["latency_ns"] for row in observed if row["successful"] == "true"]
        errors = [row["latency_ns"] for row in observed if row["successful"] == "false"]
        retries = sum(max(row["attempts"] - 1, 0) for row in observed)
        self.summary.update(successful=len(successes), terminal_errors=len(errors),
                            total_completed=len(observed), retries=retries,
                            successful_ops_per_second=len(successes) / 30,
                            cpu_ns_per_successful_op=300 / len(successes) if successes else None,
                            cores_busy=300 / 30_000_000_000,
                            success_p50_ns=q.percentile(successes, .5) if successes else None,
                            success_p99_ns=q.percentile(successes, .99) if successes else None,
                            error_p99_ns=q.percentile(errors, .99) if errors else None,
                            terminal_errors_per_1000_completed_ops=len(errors) * 1000 / len(observed),
                            retries_per_1000_completed_ops=retries * 1000 / len(observed))

    def write(self):
        q.save_json(self.root / "manifest.json", self.manifest)
        q.save_json(self.root / "timed-summary.json", self.summary)
        with (self.root / "timed-samples.csv").open("w", newline="") as stream:
            writer = csv.DictWriter(stream, fieldnames=sorted(q.RAW_FIELDS))
            writer.writeheader()
            writer.writerows(self.rows)

    def validate(self):
        return q.validate_run(self.root, self.config, self.run, self.prefix, self.build)


class ArgumentsAndSchedule(unittest.TestCase):
    def test_explicit_authorization_precedes_every_side_effect(self):
        args = q.parser().parse_args(["run", "--config", "/missing", "--output", "/fresh"])
        self.assertFalse(args.allow_run)
        with mock.patch.object(q, "read_json") as read, mock.patch.object(q, "campaign_lock") as lock:
            with self.assertRaisesRegex(q.Invalid, "explicit GO"):
                q.run_campaign(args.config, args.output, args.allow_run)
            read.assert_not_called()
            lock.assert_not_called()
        args = q.parser().parse_args(["summarize", "--campaign", "/existing"])
        self.assertEqual(args.campaign, Path("/existing"))

    def test_rfc264_72_unique_runs_rotating_cells_and_arm_order(self):
        cfg = q.validate_config(config())
        plan = q.schedule(cfg)
        self.assertEqual(len(plan), 72)
        self.assertEqual(len({item["run_id"] for item in plan}), 72)
        for block, order in enumerate(cfg["orders"]):
            current = [item for item in plan if item["block"] == block]
            self.assertEqual([item["arm"] for item in current[:3]], order)
            self.assertEqual(current[0]["cell"], block)
            for cell in range(6):
                self.assertEqual([item["arm"] for item in current if item["cell"] == cell], order)
        self.assertEqual(len({len(item["run_id"]) for item in plan}), 1)

    def test_same_driver_two_arm_four_workloads_64_runs(self):
        cfg = config()
        cfg.update(arm_ids=["a", "b"], baseline="a", orders=[["a", "b"], ["b", "a"],
                   ["b", "a"], ["a", "b"]], workloads=["grv", "get100", "set100", "getrange"],
                   intended_success_p50={})
        cfg["promotion"] = {"variant": "significant_regression", "throughput_point_max": .95,
                            "throughput_upper95_lt": 1, "cpu_per_op_point_min": 1.05,
                            "cpu_per_op_lower95_gt": 1}
        self.assertEqual(len(q.schedule(q.validate_config(cfg))), 64)

    def test_launch_flags_and_controlled_environment(self):
        cfg = config()
        with mock.patch.dict(os.environ, {"GOGC": "off", "GOMEMLIMIT": "1GiB", "GODEBUG": "bad",
                                         "GOFLAGS": "bad", "GOEXPERIMENT": "bad",
                                         "FDB_NETWORK_OPTION_TRACE_ENABLE": "bad"}):
            command, env = q.harness_command(cfg, build_manifest(), q.schedule(cfg)[0],
                                             q.campaign_prefix("local_test"), Path("/fresh/harness"))
        self.assertEqual(command[:4], ["taskset", "-c", "2-5,14-17", "/not-executed/master"])
        self.assertIn("-allow-writes", command)
        self.assertEqual(command[command.index("-duration") + 1], "30s")
        self.assertEqual(command[command.index("-warmup-duration") + 1], "10s")
        self.assertEqual({key: env[key] for key in q.CONTROLLED_ENV}, q.CONTROLLED_ENV)
        self.assertEqual(env["GOMAXPROCS"], "8")
        self.assertFalse(set(env).intersection(("GOEXPERIMENT", "GOFLAGS")))
        self.assertFalse(any(key.startswith("FDB_NETWORK_OPTION_") for key in env))
        root = Path(os.environ.get("FDB_PERF_HOME", Path.home() / ".local/share/fdb-perf"))
        self.assertEqual(q.LOCK_PATH, root / "validation/fast.lock")

    def test_prefix_ownership_and_explicit_empty_proof(self):
        prefix = q.campaign_prefix("local_abc-123")
        self.assertEqual(q.prefix_end(prefix), "clientbench/local_abc-1230")
        for invalid in ("", "../other", "x;y", "x y", "x\n"):
            with self.assertRaises(q.Invalid):
                q.campaign_prefix(invalid)
        for invalid in ("", "clientbench/", "foreign/prefix/", "clientbench/a/b/"):
            with self.assertRaises(q.Invalid):
                q.prefix_end(invalid)
        q.range_is_empty("\nRange limited to 1 keys\n\n")
        for invalid in ("", "0 results\n", "Range limited to 1 keys\n0 results\n",
                        "Range limited to 1 keys\n`x' is `v'\n1 results"):
            with self.assertRaises(q.Invalid):
                q.range_is_empty(invalid)


class CompilerComparability(unittest.TestCase):
    def test_normalized_equal_versions_are_accepted(self):
        cfg = config()
        build = build_manifest()
        for index, arm in enumerate(cfg["arm_ids"]):
            build["arms"][arm]["go_version"] = (
                "go version go1.26.10 linux/amd64" if index % 2 else "go1.26.10")
        build["arms"]["unselected"] = {"go_version": "go1.27.0"}
        with mock.patch.object(q, "read_json", return_value=build):
            self.assertEqual(q.load_build_manifest(cfg), build)

    def test_mismatched_versions_require_explicit_true(self):
        for arm in config()["arm_ids"]:
            for override in (None, False, True):
                with self.subTest(arm=arm, override=override):
                    cfg = config()
                    cfg.pop("compiler_under_test", None)
                    if override is not None:
                        cfg["compiler_under_test"] = override
                    build = build_manifest()
                    build["arms"][arm]["go_version"] = "go1.27.0"
                    with mock.patch.object(q, "read_json", return_value=build):
                        if override is True:
                            self.assertEqual(q.load_build_manifest(cfg), build)
                        else:
                            with self.assertRaisesRegex(q.Invalid, "same Go compiler version"):
                                q.load_build_manifest(cfg)

    def test_compiler_under_test_rejects_nonboolean_values(self):
        for value in (1, "true", [True], {"enabled": True}, 0, "", None):
            with self.subTest(value=value):
                cfg = config()
                cfg["compiler_under_test"] = value
                with self.assertRaisesRegex(q.Invalid, "compiler_under_test must be boolean"):
                    q.validate_config(cfg)
                with mock.patch.object(q, "read_json", return_value=build_manifest()):
                    with self.assertRaisesRegex(q.Invalid, "compiler_under_test must be boolean"):
                        q.load_build_manifest(cfg)


class RawValidation(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.fixture = RawFixture(self.temporary.name)

    def test_counts_quantiles_retry_reentries_and_drain_exclusion(self):
        result = self.fixture.validate()
        self.assertEqual(result["total_completed"], 3)
        self.assertEqual(result["terminal_errors"], 1)
        self.assertEqual(result["retries"], 3)
        self.assertEqual(result["error_latencies_ns"], [100])
        self.assertEqual(result["success_p50_ns"], 50)
        self.assertEqual(result["success_p99_ns"], 150)
        self.assertEqual(result["cpu_ns_per_successful_op"], 150)

    def test_empty_error_text_does_not_mean_success(self):
        self.fixture.rows[1]["error"] = ""
        self.fixture.write()
        self.assertEqual(self.fixture.validate()["terminal_errors"], 1)

    def test_all_errors_retained_but_comparison_has_no_population(self):
        for row in self.fixture.rows:
            row["successful"], row["error"] = "false", ""
        self.fixture.recount()
        self.fixture.write()
        result = self.fixture.validate()
        self.assertIsNone(result["success_p50_ns"])
        self.assertIsNone(result["cpu_ns_per_successful_op"])
        self.assertEqual(result["successful_ops_per_second"], 0)
        self.assertIsNone(q.paired_ratio([None] * 4, [1] * 4, 20, 1))
        self.assertFalse(q.gate_cell(config()["promotion"], None, None))

    def test_tampered_raw_and_summary_are_rejected(self):
        original = copy.deepcopy(self.fixture.summary)
        for field in ("successful", "terminal_errors", "retries", "total_completed", "success_p50_ns",
                      "success_p99_ns", "error_p99_ns", "cores_busy", "cpu_ns_per_successful_op",
                      "successful_ops_per_second", "timer_lateness_ns", "boundary_drain_raw"):
            with self.subTest(field=field):
                self.fixture.summary = copy.deepcopy(original)
                self.fixture.summary[field] += 1
                self.fixture.write()
                with self.assertRaises(q.Invalid):
                    self.fixture.validate()
        self.fixture.summary = original
        self.fixture.rows[0]["completion_offset_ns"] = 30_000_000_000
        self.fixture.write()
        with self.assertRaisesRegex(q.Invalid, "measurement after stop"):
            self.fixture.validate()

    def test_gzip_roundtrip_and_evidence_digest(self):
        before = self.fixture.validate()
        original = (self.fixture.root / "timed-samples.csv").read_bytes()
        receipt = q.compress_raw(self.fixture.root)
        self.assertFalse((self.fixture.root / "timed-samples.csv").exists())
        self.assertEqual(receipt["path"], "timed-samples.csv.gz")
        self.assertEqual(receipt["uncompressed_bytes"], len(original))
        self.assertEqual(receipt["sha256"], q.sha256_file(q.raw_path(self.fixture.root)))
        self.assertEqual(before, self.fixture.validate())
        receipt["sha256"] = "0" * 64
        q.save_json(self.fixture.root / "raw-evidence.json", receipt)
        with self.assertRaisesRegex(q.Invalid, "digest mismatch"):
            self.fixture.validate()

    def test_missing_or_malformed_compressed_receipt_rejected(self):
        q.compress_raw(self.fixture.root)
        receipt = self.fixture.root / "raw-evidence.json"
        receipt.unlink()
        with self.assertRaisesRegex(q.Invalid, "receipt missing"):
            self.fixture.validate()
        receipt.write_text("[]")
        with self.assertRaisesRegex(q.Invalid, "malformed"):
            self.fixture.validate()
        receipt.write_text("{bad json")
        with self.assertRaises(json.JSONDecodeError):
            self.fixture.validate()

    def test_duplicate_plain_and_compressed_files_rejected(self):
        q.compress_raw(self.fixture.root)
        (self.fixture.root / "timed-samples.csv").write_text("stale")
        with self.assertRaisesRegex(q.Invalid, "exactly one"):
            self.fixture.validate()

    def test_identity_and_retry_scope_rejected(self):
        for change in ("go", "binary", "source", "harness", "backend", "scope"):
            with self.subTest(change=change):
                fixture = RawFixture(self.temporary.name)
                if change == "go":
                    fixture.manifest["go_version"] = "go1.27.1"
                elif change == "binary":
                    fixture.manifest["backend"]["executable_sha256"] = "f" * 64
                elif change == "source":
                    fixture.manifest["config"]["source_revision"] = "f" * 40
                elif change == "harness":
                    fixture.summary["config"]["harness_id"] = "f" * 64
                elif change == "backend":
                    fixture.summary["backend"]["backend"] = "cgo"
                else:
                    fixture.summary["attempts_scope"] = "RPC retries"
                fixture.write()
                with self.assertRaises(q.Invalid):
                    fixture.validate()


class ClusterIdentification(unittest.TestCase):
    def test_distinct_cluster_paths_match_bytes_addresses_and_affinity(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            client_file, server_file = root / "client.cluster", root / "server.cluster"
            connection = b"local:id@127.0.0.1:4500\n"
            client_file.write_bytes(connection)
            server_file.write_bytes(connection)
            cfg = config()
            cfg["cluster_file"] = str(client_file)
            proc = root / "proc"
            (proc / "123").mkdir(parents=True)
            (proc / "123" / "comm").write_text("fdbserver\n")
            args = ["fdbserver", "-C", str(server_file), "-p", "127.0.0.1:4500"]
            (proc / "123" / "cmdline").write_bytes(b"\0".join(item.encode() for item in args) + b"\0")
            # An unrelated Docker-style server without -C must not enter this population.
            (proc / "456").mkdir()
            (proc / "456" / "comm").write_text("fdbserver\n")
            (proc / "456" / "cmdline").write_bytes(b"\0".join([b"fdbserver", b"-p", b"172.17.0.2:4500"]) + b"\0")
            status = {"client": {"database_status": {"available": True, "healthy": True}},
                      "cluster": {"database_available": True, "database_locked": False,
                                  "processes": {"p1": {"address": "127.0.0.1:4500"}}}}
            runner = mock.Mock()
            runner.run.return_value = json.dumps(status)
            with mock.patch.object(q.os, "sched_getaffinity", return_value={8}) as affinity:
                q.initial_cluster_check(cfg, runner, root, proc_root=proc)
                affinity.assert_called_once_with(123)
            record = q.read_json(root / "initial-cluster.json")
            self.assertEqual(record["servers"][0]["cluster_file"], str(server_file))
            self.assertEqual(record["servers"][0]["address"], "127.0.0.1:4500")
            self.assertEqual(record["servers"][0]["affinity"], [8])
            server_file.write_bytes(b"foreign:id@127.0.0.1:4500\n")
            with self.assertRaisesRegex(q.Invalid, "cannot identify every"):
                q.initial_cluster_check(cfg, runner, root, proc_root=proc)
            server_file.write_bytes(connection)
            with mock.patch.object(q.os, "sched_getaffinity", return_value={999}):
                with self.assertRaisesRegex(q.Invalid, "outside"):
                    q.initial_cluster_check(cfg, runner, root, proc_root=proc)
            status["cluster"]["processes"]["p1"]["address"] = "127.0.0.1:4501"
            runner.run.return_value = json.dumps(status)
            with self.assertRaisesRegex(q.Invalid, "absent from status"):
                q.initial_cluster_check(cfg, runner, root, proc_root=proc)


class StatisticsAndGates(unittest.TestCase):
    def test_pairing_is_not_ratio_of_absolute_means(self):
        candidate, baseline = [2, 6, 40, 80], [1, 3, 10, 20]
        result = q.paired_ratio(candidate, baseline, 20000, 492817)
        self.assertAlmostEqual(result["point"], 8 ** .5)
        self.assertEqual(result, q.paired_ratio(candidate, baseline, 20000, 492817))
        self.assertLessEqual(result["lower95"], result["point"])
        self.assertGreaterEqual(result["upper95"], result["point"])
        self.assertIsNone(q.paired_ratio([0, 1, 1, 1], [1] * 4, 20, 1))
        self.assertIsNone(q.paired_ratio([0] * 4, [0] * 4, 20, 1))
        with self.assertRaises(q.Invalid):
            q.paired_ratio([], [], 20, 1)

    def test_noninferiority_boundaries(self):
        gate = config()["promotion"]
        self.assertTrue(q.gate_cell(gate, {"lower95": .98}, {"upper95": 1.02}))
        self.assertFalse(q.gate_cell(gate, {"lower95": .979}, {"upper95": 1.02}))
        self.assertFalse(q.gate_cell(gate, {"lower95": .98}, {"upper95": 1.021}))

    def test_go_significant_regression_is_exact_not_noninferiority(self):
        gate = {"variant": "significant_regression", "throughput_point_max": .95,
                "throughput_upper95_lt": 1, "cpu_per_op_point_min": 1.05, "cpu_per_op_lower95_gt": 1}
        throughput = {"point": .95, "upper95": 1}
        cpu = {"point": 1.05, "lower95": 1}
        self.assertTrue(q.gate_cell(gate, throughput, cpu))
        self.assertFalse(q.gate_cell(gate, {"point": .95, "upper95": .999}, cpu))
        self.assertFalse(q.gate_cell(gate, throughput, {"point": 1.05, "lower95": 1.001}))
        self.assertTrue(q.gate_cell(gate, {"point": .951, "upper95": .99}, cpu))
        self.assertTrue(q.gate_cell(gate, throughput, {"point": 1.049, "lower95": 1.01}))


class OfflineSummary(unittest.TestCase):
    def make_campaign(self, root, candidate_errors=(5, 5, 5, 5), go_gate=False):
        cfg = config()
        cfg.update(arm_ids=["master", "p0"], baseline="master", workloads=["get100"], workers=[1],
                   orders=[["master", "p0"], ["p0", "master"], ["p0", "master"], ["master", "p0"]],
                   intended_success_p50={})
        if go_gate:
            cfg["promotion"] = {"variant": "significant_regression", "throughput_point_max": .95,
                                "throughput_upper95_lt": 1, "cpu_per_op_point_min": 1.05,
                                "cpu_per_op_lower95_gt": 1}
        prefix = q.campaign_prefix("local_offline_summary")
        plan = q.schedule(cfg)
        build = build_manifest()
        q.save_json(root / "config.json", cfg)
        q.save_json(root / "build-manifest.json", build)
        q.save_json(root / "run-plan.json", plan)
        q.save_json(root / "campaign.json", {"status": "complete", "cleanup": "verified",
                    "completed_runs": len(plan), "prefix": prefix})
        for run in plan:
            directory = root / "runs" / run["run_id"] / "harness"
            directory.mkdir(parents=True)
            fixture = RawFixture(directory)
            fixture.config, fixture.run, fixture.prefix = cfg, run, prefix
            arm = build["arms"][run["arm"]]
            for source in (fixture.manifest, fixture.summary):
                source["config"].update(workers=1, workload="get100", prefix=prefix + run["run_id"] + "/")
                source["backend"]["executable"] = arm["binary"]
            successes = run["block"] + 1
            errors = 5 if run["arm"] == "master" else candidate_errors[run["block"]]
            fixture.rows = [{"phase": "measure", "worker": 0, "index": index, "latency_ns": 100,
                             "attempts": 1, "error": "", "successful": "true",
                             "completion_offset_ns": 10000 + index} for index in range(successes)]
            fixture.rows += [{"phase": "measure", "worker": 0, "index": successes + index,
                              "latency_ns": 500 + run["block"] * 100 + index, "attempts": 2,
                              "error": "observed", "successful": "false",
                              "completion_offset_ns": 20000 + index} for index in range(errors)]
            fixture.summary.update(boundary_drain_raw=0, boundary_drain_wall_ns=0)
            fixture.recount()
            fixture.write()
            if run["block"] % 2 == 0:
                q.compress_raw(directory)
        return q.summarize_campaign(root)

    def test_report_four_groups_pooled_counts_and_pooled_error_p99(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            result = self.make_campaign(root)
            arm = result["cells"][0]["arms"]["p0"]
            self.assertEqual(arm["raw_counts"], {"terminal_errors": 20, "retries": 20, "total_completed": 30})
            self.assertAlmostEqual(arm["terminal_errors_per_1000_completed"]["absolute"], 20000 / 30)
            self.assertEqual(arm["error_p99_ns"]["absolute"], 804)
            self.assertIsNone(arm["error_p99_ns"]["ratio"], "five errors/run is not an adequate tail CI")
            report = (root / "report.md").read_text()
            self.assertEqual(report.count("| Arm | Latency | Traffic | Errors | Saturation |"), 1)
            self.assertIn("raw 20", report)
            self.assertIn("30 completed logical ops", report)

    def test_error_tail_threshold_is_per_arm_not_combined(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            result = self.make_campaign(root, candidate_errors=(4, 5, 5, 5), go_gate=True)
            self.assertEqual(result["cells"][0]["arms"]["master"]["error_p99_ns"]["absolute"], 804)
            self.assertIsNone(result["cells"][0]["arms"]["p0"]["error_p99_ns"]["absolute"])
            report = (root / "report.md").read_text()
            self.assertIn("error p99 n/a (19 errors; <20)", report)
            self.assertIn("**PASS**", report)
            self.assertNotIn("RFC264", report)
            self.assertNotIn("p50 point ratio", report)


class OwnershipAndCleanup(unittest.TestCase):
    def test_lock_is_nonblocking(self):
        with tempfile.TemporaryDirectory() as temporary:
            lock = Path(temporary) / "fast.lock"
            with q.campaign_lock(lock):
                with self.assertRaisesRegex(q.Invalid, "lock busy"):
                    with q.campaign_lock(lock):
                        self.fail("second lock acquired")

    def test_cleanup_clears_only_the_owned_prefix_and_verifies_it(self):
        prefix = q.campaign_prefix("local_onlymine")
        runner = mock.Mock()
        runner.run.return_value = "\nRange limited to 1 keys\n\n"
        with tempfile.TemporaryDirectory() as temporary:
            q.cleanup_prefix(config(), runner, prefix, Path(temporary))
        commands = [call.args[0][-1] for call in runner.run.call_args_list]
        self.assertEqual(commands, ['writemode on; clearrange "clientbench/local_onlymine/" "clientbench/local_onlymine0"',
                                    'getrange "clientbench/local_onlymine/" "clientbench/local_onlymine0" 1'])

    def test_campaign_failure_still_attempts_owned_prefix_cleanup(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            cfg = root / "config.json"
            q.save_json(cfg, config())
            runner = mock.Mock()
            runner.run.side_effect = q.Invalid("dummy timed child failed")
            with mock.patch.object(q, "campaign_lock", return_value=contextlib.nullcontext()), \
                 mock.patch.object(q, "load_build_manifest", return_value=build_manifest()), \
                 mock.patch.object(q, "binary_identities", return_value={}), \
                 mock.patch.object(q.os, "getpriority", return_value=0), \
                 mock.patch.object(q, "initial_cluster_check"), \
                 mock.patch.object(q, "verify_prefix"), \
                 mock.patch.object(q, "OwnedProcesses", return_value=runner), \
                 mock.patch.object(q, "cleanup_prefix") as cleanup:
                with self.assertRaisesRegex(q.Invalid, "dummy timed child failed"):
                    q.run_campaign(cfg, root / "fresh", True)
                cleanup.assert_called_once()
                q.prefix_end(cleanup.call_args.args[2])
            state = q.read_json(root / "fresh" / "campaign.json")
            self.assertEqual(state["status"], "failed")
            self.assertEqual(state["cleanup"], "verified")

    def test_nonzero_controller_nice_rejected_before_fdb(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            cfg = root / "config.json"
            q.save_json(cfg, config())
            with mock.patch.object(q, "campaign_lock", return_value=contextlib.nullcontext()), \
                 mock.patch.object(q, "load_build_manifest", return_value=build_manifest()), \
                 mock.patch.object(q.os, "getpriority", return_value=19), \
                 mock.patch.object(q.os, "sched_getaffinity", return_value={2, 3}), \
                 mock.patch.object(q, "initial_cluster_check") as cluster:
                with self.assertRaisesRegex(q.Invalid, "nice 0"):
                    q.run_campaign(cfg, root / "fresh", True)
                cluster.assert_not_called()
            state = q.read_json(root / "fresh" / "campaign.json")
            self.assertEqual(state["controller"]["nice"], 19)
            self.assertEqual(state["controller"]["affinity"], [2, 3])
            self.assertEqual(state["cleanup"], "not_needed")

    def test_hard_timeout_stops_only_our_dummy_process_group(self):
        # This is the only test that launches processes: plain Python sleepers, never FDB.
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            pid_file = root / "child.pid"
            child_source = "import time; time.sleep(30)"
            parent_source = ("import subprocess,sys,time; from pathlib import Path; "
                             f"p=subprocess.Popen([sys.executable,'-c',{child_source!r}]); "
                             f"Path({str(pid_file)!r}).write_text(str(p.pid)); time.sleep(30)")
            runner = q.OwnedProcesses()
            with self.assertRaisesRegex(q.Invalid, "hard timeout"):
                runner.run([sys.executable, "-c", parent_source], 1, root / "dummy.log")
            self.assertEqual(runner.active, {})
            self.assertTrue(pid_file.exists(), "positive control: child was actually started")
            child = int(pid_file.read_text())
            status = Path(f"/proc/{child}/status")
            deadline = time.monotonic() + 2
            while status.exists():
                try:
                    state = next(line for line in status.read_text().splitlines() if line.startswith("State:"))
                except FileNotFoundError:
                    break
                # A killed grandchild can briefly be an init-owned zombie, but cannot execute.
                if "Z" in state:
                    break
                self.assertLess(time.monotonic(), deadline, "owned child survived group cleanup")
                time.sleep(.01)


if __name__ == "__main__":
    unittest.main()
