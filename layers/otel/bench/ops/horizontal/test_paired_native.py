#!/usr/bin/env python3
"""Offline native-adapter regressions: fake provider/transport/processes only."""
import base64
import copy
import hashlib
import io
import json
import stat
from pathlib import Path
import subprocess
import tarfile
import tempfile
from types import SimpleNamespace
import unittest
from unittest import mock

import driver as d
import paired as q
import paired_cost as cost
import paired_native as n
import paired_reports as reports


class Process:
    def __init__(self, running=False, code=0):
        self.running, self.code = running, code
        self.terminated, self.killed, self.waited = False, False, False

    def poll(self):
        return None if self.running else self.code

    def wait(self, timeout=None):
        if self.running:
            raise subprocess.TimeoutExpired("fake", timeout)
        self.waited = True
        return self.code

    def terminate(self):
        self.terminated, self.running, self.code = True, False, -15

    def kill(self):
        self.killed, self.running, self.code = True, False, -9


class NativeFixture(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.plan = q.fake_plan(self.root / "fixtures", profile="pilot")
        self.plan["fake_provider"] = False
        self.plan["hcloud"] = "/fake/hcloud"
        self.epoch = self.plan["first_epoch"]
        self.ticks = 10 ** 9
        self.provider = mock.Mock()
        self.provider.KINDS = ("server", "primary-ip", "floating-ip", "volume", "network", "firewall", "ssh-key", "placement-group", "load-balancer", "certificate")
        self.output = self.root / "out"
        self.backend = n.NativeBackend(self.plan, self.output, self.provider, clock=lambda: self.epoch,
                                       monotonic=lambda: self.ticks, sleep=self.sleep)
        self.backend.nodes = {role: {"id": i + 1, "role": role, "created_epoch": self.epoch, "name": "fake-" + role,
                                    "host": "192.0.2." + str(i + 1)} for i, role in enumerate(q.held_roles(self.plan))}
        self.backend.monitor.nodes = self.backend.nodes
        self.cluster = {"path": "/srv/otel-horizontal/paired-fake-test/arms/pairs1-rust_native-rep1/fdb.cluster", "sha256": "a" * 64}
        self.layout = {"path": "/srv/otel-paired/layout.json", "sha256": "b" * 64}
        self.backend.cluster = {"remote_cluster_file": self.cluster["path"]}
        self.backend.layouts[self.cluster["path"]] = self.backend.cluster
        self.backend._gate = mock.Mock(return_value=300)
        self.backend.supervisor = mock.Mock()

    def sleep(self, seconds):
        self.ticks += int(seconds * 10 ** 9)
        self.epoch += int(seconds)

    def job(self, arm="rust_native"):
        ns = q.namespace(self.plan, 1, arm, "prerequisite", 1)
        job = q._job(self.plan, arm, 1, "prerequisite", 1, self.layout, self.cluster,
                     self.backend.remote_assets(self.plan), "/srv/otel-paired/" + ns + "/ingest.json")
        return ns, job

    def staged(self, arm="rust_native"):
        ns, job = self.job(arm)
        directory = self.output / "jobs" / arm
        directory.mkdir(mode=0o700, parents=True)
        grant = {"job_sha256": q.job_seal(job), "job": job}
        return {"node": self.backend.nodes["client1"], "namespace": ns, "job": job,
                "directory": directory, "path": "/fake/grant.json", "grant": grant,
                "ready": {"namespace": ns, "job_sha256": q.job_seal(job), "node_id": 2}}

    def ledger(self):
        quote = self.plan["pricing"]
        ledger = cost.admit(quote, expected_seal=quote["seal_sha256"], first_epoch=self.epoch,
                            deadline_epoch=self.plan["deadline_epoch"], now_epoch=self.epoch, held_host_count=2)
        for node in self.backend.nodes.values():
            ledger = cost.record_creation(ledger, host_id=node["id"], created_epoch=self.epoch, now_epoch=self.epoch)
        return ledger


class ProvisionAdapterTests(NativeFixture):
    def test_watch_registration_precedes_prepare_with_internal_cost_ledger(self):
        quote = self.plan["pricing"]
        ledger = cost.admit(quote, expected_seal=quote["seal_sha256"], first_epoch=self.epoch,
                            deadline_epoch=self.plan["deadline_epoch"], now_epoch=self.epoch, held_host_count=2)
        events = []
        self.backend.supervisor.register.side_effect = lambda *args, **kw: events.append("register")
        self.provider.prepare.side_effect = lambda *args, **kw: (events.append("prepare") or {"nodes": self.backend.nodes})
        with mock.patch.object(self.backend.monitor, "install_helpers"), mock.patch.object(self.backend, "_copy") as copied, \
                mock.patch.object(self.backend, "_remote", side_effect=lambda node, *args: {"node_id": node["id"]}):
            result = self.backend.prepare(self.plan, ledger)
        self.assertEqual(events, ["register", "prepare"])
        self.assertEqual(set(result), {"node1", "client1"})
        self.assertEqual(self.provider.prepare.call_args.kwargs["cost_ledger"], ledger)
        self.assertEqual(copied.call_count, 2)
        self.assertEqual(len(self.backend.ledger["hosts"]), 2)

    def test_transfer_extracts_verified_zstd_receipts_for_every_host_asset(self):
        # A valid large manifest must travel as a file, not exceed SSH argv limits.
        name = "control_native_binary"
        built = q.builds.read_receipt(self.plan, name)
        bundle_ref = self.plan["assets"][q.builds.deployment_asset(name)]
        archive = Path(bundle_ref["path"])
        with tarfile.open(archive) as source:
            payloads = {item.name: (source.extractfile(item).read(), item.mode) for item in source if item.isfile()}
        for index in range(1000):
            payloads["bin/control_native_binary.runfiles/_main/large-manifest/entry-%04d.txt" % index] = (b"fixture", 0o644)
        with tarfile.open(archive, "w") as target:
            for path, (payload, mode) in sorted(payloads.items()):
                item = tarfile.TarInfo(path); item.size = len(payload); item.mode = mode
                target.addfile(item, io.BytesIO(payload))
        bundle_ref["sha256"] = n.sha(archive)
        built.update(deployment_sha256=bundle_ref["sha256"], files=q.builds.archive_manifest(archive))
        receipt_path = Path(self.plan["assets"][name]["build_receipt"]["path"])
        receipt_path.write_text(json.dumps(built, sort_keys=True) + "\n")
        self.plan["assets"][name]["build_receipt"]["sha256"] = n.sha(receipt_path)
        self.backend.plan = copy.deepcopy(self.plan)
        with mock.patch.object(q.builds, "readelf_dynamic", return_value=q.builds.fake_readelf(name)):
            q.builds.validate(self.plan, name)
        state = self.backend.provision_out / "provision"
        state.mkdir(parents=True, mode=0o700)
        packed = {name: {"name": "libfdb_c.so" if name == "libfdb_c" else name, "sha256": value["sha256"], "transfer_sha256": "c" * 64, "bytes": 7}
                  for name, value in self.plan["assets"].items()}
        d.write(state / "asset-transfer.json", {"assets": packed})
        for index, (role, node) in enumerate(self.backend.nodes.items()):
            for name, value in packed.items():
                receipt = {"node_id": node["id"], "returncode": 0, "timed_out": False,
                    "stdout": json.dumps({"dest": "/srv/otel-horizontal/" + self.plan["campaign"] + "/bin/" + ("libfdb_c.so" if name == "libfdb_c" else name), "sha256": value["sha256"],
                                           "bytes": 7, "src_sha256": "c" * 64})}
                d.write(state / (role + "-asset-" + value["name"] + ("-install.json" if index == 0 else "-fetch.json")), receipt)
        requests = {}
        def copy_request(node, local, remote):
            requests[(node["id"], remote)] = (d.read(local), n.sha(local))
        def installed(node, args):
            self.assertEqual(args[:2], ["python3", "-c"])
            self.assertEqual(args[2], q.builds.INSTALL_SCRIPT)
            self.assertLess(len(" ".join(args).encode()), 128 * 1024)
            request, expected = requests[(node["id"], args[3])]
            self.assertEqual(args[4], expected)
            return json.dumps({"schema": "otel_paired_deployment_receipt_v1", **request})
        self.backend._copy = mock.Mock(side_effect=copy_request)
        self.backend.command = mock.Mock(side_effect=installed)
        result = self.backend.transfer_assets(self.plan, self.backend.nodes)
        self.assertEqual(self.backend.command.call_count, 6)
        self.assertEqual(self.backend._copy.call_count, 6)
        self.assertTrue(any(len(json.dumps(request).encode()) > 128 * 1024 for request, _ in requests.values()))
        self.assertTrue(all(len(value["deployments"]) == 3 for value in result.values()))
        q.validate_transfers(self.plan, self.backend.nodes, result)
        self.assertEqual(set(result), set(self.backend.nodes))
        self.assertTrue(all(set(x["assets"]) == set(self.plan["assets"]) for x in result.values()))
        missing = next(state.glob("client1-asset-*-fetch.json"))
        original = missing.read_bytes(); missing.unlink()
        with self.assertRaises(FileNotFoundError):
            self.backend.transfer_assets(self.plan, self.backend.nodes)
        self.assertEqual(self.backend._copy.call_count, 6)
        missing.write_bytes(original)
        with self.assertRaises(FileExistsError):
            self.backend.transfer_assets(self.plan, self.backend.nodes)
        self.assertEqual(self.backend._copy.call_count, 6)


class PricingTests(NativeFixture):
    def quote(self):
        quote = self.plan["pricing"]
        raw = json.dumps({"pricing": {"currency": "EUR", "vat_rate": "19.000000",
               "server_types": [{"name": "ccx43", "prices": [{"location": "hel1", "price_hourly": {
                   "net": quote["server_hourly_net_eur"], "gross": quote["server_hourly_gross_eur"]}}]}],
               "primary_ips": [{"type": "ipv4", "prices": [{"location": "hel1", "price_hourly": {
                   "net": quote["ipv4_hourly_net_eur"], "gross": quote["ipv4_hourly_gross_eur"]}}]}]}})
        quote["provenance"]["response_sha256"] = hashlib.sha256(raw.encode()).hexdigest()
        quote["seal_sha256"] = cost.seal_receipt(quote)
        self.plan["pricing_seal_sha256"] = quote["seal_sha256"]
        self.backend.plan = copy.deepcopy(self.plan)
        self.provider._call.return_value = (0, raw, "")
        return raw

    def test_read_only_current_provider_quote_not_retimestamped(self):
        self.quote()
        self.assertEqual(self.backend.capture_pricing(self.plan), self.plan["pricing"])
        argv = self.provider._call.call_args.args[1]
        self.assertEqual(argv, ["/fake/hcloud", "api", "/pricing", "--method", "GET"])
        self.assertFalse(self.provider.prepare.called)

    def test_changed_raw_currency_vat_and_normalized_prices_refuse(self):
        raw = self.quote()
        for change in (lambda x: x + " ", lambda x: x.replace('"EUR"', '"USD"'),
                       lambda x: x.replace('"19.000000"', '"20.000000"'), lambda x: x.replace('"0.3758"', '"0.9999"')):
            self.provider._call.return_value = (0, change(raw), "")
            with self.subTest(change=change), self.assertRaises(ValueError):
                self.backend.capture_pricing(self.plan)

    def test_stale_and_failed_get_refuse_before_mutation(self):
        raw = self.quote()
        self.provider._call.return_value = (1, raw, "authorization failure")
        with self.assertRaises(ValueError):
            self.backend.capture_pricing(self.plan)
        self.epoch += 3601
        self.provider._call.reset_mock()
        with self.assertRaises(cost.CostError):
            self.backend.capture_pricing(self.plan)
        self.assertFalse(self.provider._call.called)


class JobHookTests(NativeFixture):
    def test_both_go_and_rust_ingest_use_exact_one_json_argument(self):
        for arm in q.ARMS:
            ns, job = self.job(arm)
            expected = q.invocation(self.plan, job, "/srv/otel-paired/" + ns + "/job.json", self.backend.remote_assets(self.plan))
            staged = {"ready": {"namespace": ns, "node_id": 2, "job_sha256": q.job_seal(job)}}
            with mock.patch.object(self.backend, "_stage", return_value=staged) as stage:
                receipt = self.backend.ready(self.plan, self.backend.nodes["client1"], ns, job, expected)
            self.assertEqual(receipt, staged["ready"])
            self.assertEqual(stage.call_args.args[-1], expected)
            self.assertEqual(len(expected), 2)
            self.assertTrue(expected[0].endswith(q.ARM_EXECUTABLES[arm]))

    def test_ready_rejects_foreign_host_or_flag_transport(self):
        ns, job = self.job()
        for node, argv in (({"id": 77}, ["/fake", "/job.json"]),
                           (self.backend.nodes["client1"], ["/fake", "--mode", "ingest"])):
            with self.assertRaises(ValueError):
                self.backend.ready(self.plan, node, ns, job, argv)

    def test_control_job_for_every_arm_binds_go_executable_and_source(self):
        for arm in q.ARMS:
            _, job = self.job(arm)
            control = self.backend._control_job(job, "verify")
            for key, value in q.control_identity(self.plan).items():
                self.assertEqual(control[key], value)
            self.assertEqual(control["arm"], arm)
            self.assertEqual(control["run"], job["run"])
            self.assertEqual(control["corpus_sha256"], job["corpus_sha256"])

    def test_paid_host_mapping_requires_exact_stat_but_pure_control_is_unmapped(self):
        for arm in q.ARMS:
            _, job = self.job(arm)
            raw = reports.fake_ingest(job)
            self.backend._common(raw, job)
            if arm != "go_pure":
                raw["native_mapping_rule"] = "btrfs_superblock"
                with self.assertRaisesRegex(ValueError, "exact_stat"):
                    self.backend._common(raw, job)
            control = self.backend._control_job(job, "verify")
            raw = reports.fake_common(control)
            self.backend._common(raw, control)
            raw["native_library_mapped"] = True
            with self.assertRaisesRegex(ValueError, "mapping or absence"):
                self.backend._common(raw, control)

    def test_setup_derives_writer_from_namespace_but_executes_go(self):
        ns, _ = self.job()
        seen = {}
        def stage(node, namespace, job, argv, **kw):
            seen.update(job=job, argv=argv, namespace=namespace)
            return {"job": job}
        def execute(staged):
            job = staged[1]["job"]
            return {1: {**reports.fake_common({**job, "layout_sha256": "c" * 64}),
                "effective_job": job, "job_sha256": q.job_seal(job), "layout_sha256": "c" * 64,
                "database_path": "/OTEL/R_" + job["run"].upper(), "tenant": 0,
                "baseline": {"record_identities": 1, "index_entries": {"STORE_META_NAME": 1},
                             "physical_record_kvs": 1, "header_kvs": 1, "normalized_kv_sha256": "d" * 64},
                "artifacts": [{"kind": "layout", "path": job["layout_file"], "sha256": "c" * 64}]}}
        with mock.patch.object(self.backend, "_stage", side_effect=stage), mock.patch.object(self.backend, "_execute", side_effect=execute), \
                mock.patch.object(self.backend, "command", return_value="c" * 64 + "  layout.json"):
            result = self.backend.setup(self.plan, self.cluster, ns, 100000)
        self.assertEqual(seen["job"]["arm"], "rust_native")
        self.assertEqual(seen["job"]["binary_sha256"], self.plan["assets"]["control_pure_binary"]["sha256"])
        self.assertTrue(seen["argv"][0].endswith("control_pure_binary"))
        self.assertNotIn("layout_sha256", seen["job"])
        self.assertEqual(result["sha256"], "c" * 64)

    def test_verify_uses_go_pure_control_provenance_and_complete_raw_oracle(self):
        ns, job = self.job()
        wrapped = reports.fake_verify(job, control_identity=q.control_identity(self.plan))
        staged = self.staged()
        self.backend.completed = [staged]
        with mock.patch.object(self.backend, "_stage", return_value=staged) as stage, \
                mock.patch.object(self.backend, "_execute", return_value={1: wrapped["report"]}), \
                mock.patch.object(self.backend, "_remote", return_value={"artifacts": {"queries.ndjson": reports.oracle_bytes(wrapped["queries"]).decode()}}):
            result = self.backend.verify(self.plan, ns, job)
        self.assertEqual(stage.call_args.args[2]["mode"], "verify")
        self.assertTrue(stage.call_args.args[3][0].endswith("control_pure_binary"))
        self.assertTrue(q.verify_audit(result, job, ns, plan=self.plan))
        self.assertEqual(result["ingest_job_sha256"], q.job_seal(job))
        self.assertEqual(len(result["queries"]), 1600)

    def test_missing_oracle_artifact_never_becomes_capability_success(self):
        ns, job = self.job()
        staged = self.staged()
        self.backend.completed = [staged]
        with mock.patch.object(self.backend, "_stage", return_value=staged), \
                mock.patch.object(self.backend, "_execute", return_value={1: {"oracle": {"results_sha256": "a" * 64}}}), \
                mock.patch.object(self.backend, "_remote", return_value={"artifacts": {}}), self.assertRaises(ValueError):
            self.backend.verify(self.plan, ns, job)

    def test_failed_release_consumes_wave_and_cannot_retry(self):
        staged = self.staged()
        self.backend.pending[staged["namespace"]] = staged
        ready, jobs = {1: staged["ready"]}, {1: staged["job"]}
        with mock.patch.object(self.backend, "_execute", side_effect=ValueError("ambiguous release")):
            with self.assertRaisesRegex(ValueError, "ambiguous"):
                self.backend.release_and_wait(self.plan, ready, jobs)
            with self.assertRaisesRegex(ValueError, "ready grant"):
                self.backend.release_and_wait(self.plan, ready, jobs)
        self.assertEqual(self.backend.pending, {})


class ProcessTests(NativeFixture):
    def transport(self, item, mode, path, extra=(), **kw):
        if mode == "read":
            return reports.fake_ingest(self.current["job"])
        if mode == "stopped":
            return {"passed": True, "quiescent": True}
        if mode == "collect":
            return {"artifacts": {"partial.json": "{}"}}
        return {}

    def test_success_retains_raw_and_proves_reaping(self):
        self.current = self.staged("go_pure")
        process = Process()
        with mock.patch.object(self.backend, "_launch", side_effect=lambda x: x.update(process=process)), \
                mock.patch.object(self.backend, "_remote", side_effect=self.transport), mock.patch.object(self.backend, "_healthy"):
            result = self.backend._execute({1: self.current})
        self.assertEqual(result[1]["status"], "ok")
        self.assertTrue(process.waited)
        self.assertTrue((self.current["directory"] / "raw.json").exists())

    def test_health_failure_aborts_and_reaps_process_even_abort_transport_fails(self):
        self.current = self.staged()
        process = Process(running=True)
        calls = []
        def transport(node, mode, path, extra=(), **kw):
            calls.append(mode)
            if mode == "abort":
                raise ValueError("transport lost")
            return self.transport(node, mode, path, extra, **kw)
        with mock.patch.object(self.backend, "_launch", side_effect=lambda x: x.update(process=process)), \
                mock.patch.object(self.backend, "_remote", side_effect=transport), \
                mock.patch.object(self.backend, "_healthy", side_effect=ValueError("strict health failed")), \
                self.assertRaisesRegex(ValueError, "strict health"):
            self.backend._execute({1: self.current})
        self.assertTrue(process.terminated)
        self.assertTrue(process.waited)
        self.assertIn("collect", calls)
        self.assertTrue((self.current["directory"] / "raw.json").exists())

    def test_nonzero_exit_collects_failed_report_and_never_retries(self):
        self.current = self.staged()
        process = Process(code=2)
        with mock.patch.object(self.backend, "_launch", side_effect=lambda x: x.update(process=process)) as launch, \
                mock.patch.object(self.backend, "_remote", side_effect=self.transport), mock.patch.object(self.backend, "_healthy"), \
                self.assertRaisesRegex(ValueError, "process failed"):
            self.backend._execute({1: self.current})
        self.assertEqual(launch.call_count, 1)
        self.assertTrue((self.current["directory"] / "raw.json").exists())

    def test_no_report_or_invalid_report_retains_named_diagnostic_and_binary_partials(self):
        for index, (code, report) in enumerate(((2, None), (0, '{"schema":"invalid"}'), (0, '{"schema":'))):
            with self.subTest(exit=code, report=report):
                self.output = self.root / ("failure-" + str(index))
                self.current = self.staged()
                remote = self.root / ("remote-" + str(index)); remote.mkdir()
                diagnostic = b'{"error":"collector workers did not join"}\r\n'
                partial = b"\x00\xffpartial"
                (remote / "worker-diagnostic.json").write_bytes(diagnostic)
                (remote / "partial.tmp").write_bytes(partial)
                if report is not None:
                    (remote / "ingest.json").write_text(report)
                grant = dict(self.current["grant"], node_id=self.current["node"]["id"], inputs={})
                calls = []
                def transport(node, mode, path, extra=(), **kw):
                    calls.append(mode)
                    if mode == "read":
                        self.assertIsNotNone(report)
                        return n.loads(report)
                    if mode == "collect":
                        return n.collect_artifacts(grant, remote)
                    return self.transport(node, mode, path, extra, **kw)
                process = Process(code=code)
                with mock.patch.object(self.backend, "_launch", side_effect=lambda x: x.update(process=process)) as launch, \
                        mock.patch.object(self.backend, "_remote", side_effect=transport), mock.patch.object(self.backend, "_healthy"), \
                        self.assertRaises(ValueError):
                    self.backend._execute({1: self.current})
                self.assertEqual(launch.call_count, 1)
                self.assertTrue(process.waited)
                self.assertIn("stopped", calls)
                self.assertIn("collect", calls)
                self.assertEqual("read" in calls, code == 0)
                saved = d.read(self.current["directory"] / "raw.json")
                self.assertEqual(saved["artifacts"]["worker-diagnostic.json"].encode(), diagnostic)
                self.assertEqual(base64.b64decode(saved["artifacts"]["partial.tmp"]), partial)
                for name, data in (("worker-diagnostic.json", diagnostic), ("partial.tmp", partial)):
                    self.assertEqual(saved["artifact_manifest"][name]["sha256"], hashlib.sha256(data).hexdigest())
                    self.assertEqual(saved["artifact_manifest"][name]["bytes"], len(data))
                self.assertEqual(saved["artifact_manifest"]["partial.tmp"]["encoding"], "base64")
                if report is not None:
                    self.assertEqual(saved["artifacts"]["ingest.json"], report)
                    self.assertEqual(saved["artifact_manifest"]["ingest.json"]["sha256"], hashlib.sha256(report.encode()).hexdigest())

    def test_adjacent_artifact_capture_excludes_inputs_and_refuses_unsafe_or_oversize_files(self):
        work = self.root / "artifacts"; work.mkdir()
        sealed = work / "layout.json"; sealed.write_text("sealed input")
        diagnostic = work / "diagnostic.json"; diagnostic.write_text("failure")
        grant = {"node_id": 2, "job_sha256": "a" * 64, "inputs": {str(sealed): "b" * 64}}
        saved = n.collect_artifacts(grant, work)
        self.assertEqual(set(saved["artifacts"]), {"diagnostic.json"})
        with mock.patch.object(n, "MAX_ARTIFACT", 1), self.assertRaisesRegex(ValueError, "type/size"):
            n.collect_artifacts(grant, work)
        link = work / "bad-link"; link.symlink_to(diagnostic)
        with self.assertRaises(OSError):
            n.collect_artifacts(grant, work)

    def test_missing_genuine_report_refuses_even_exit_zero(self):
        self.current = self.staged()
        def transport(node, mode, path, extra=(), **kw):
            return {} if mode == "read" else self.transport(node, mode, path, extra, **kw)
        with mock.patch.object(self.backend, "_launch", side_effect=lambda x: x.update(process=Process())), \
                mock.patch.object(self.backend, "_remote", side_effect=transport), mock.patch.object(self.backend, "_healthy"), \
                self.assertRaisesRegex(ValueError, "genuine control"):
            self.backend._execute({1: self.current})


class DrainAndCleanupTests(NativeFixture):
    def test_storage_drain_is_three_observations_five_seconds_apart_not_flush(self):
        samples = [{"queue_bytes": 20000000, "lag_seconds": 11}, {"queue_bytes": 1, "lag_seconds": 1},
                   {"queue_bytes": 2, "lag_seconds": 2}, {"queue_bytes": 0, "lag_seconds": 0}]
        with mock.patch.object(self.backend, "_healthy", side_effect=samples):
            value = self.backend.drain(self.plan, self.cluster)
        self.assertEqual(len(value), 4)
        self.assertEqual([x["controller_ns"] for x in value], [10 ** 9, 6 * 10 ** 9, 11 * 10 ** 9, 16 * 10 ** 9])
        self.assertTrue(all(x["healthy"] for x in value))

    def test_drain_health_failure_is_immediate(self):
        with mock.patch.object(self.backend, "_healthy", side_effect=ValueError("unhealthy")), self.assertRaisesRegex(ValueError, "unhealthy"):
            self.backend.drain(self.plan, self.cluster)
        self.assertEqual(self.ticks, 10 ** 9)

    def cleanup_fixture(self):
        state = self.backend.provision_out / "provision"
        state.mkdir(parents=True, mode=0o700)
        resources = {"ids": {"server": [1, 2]}, "protected_ids": {"server": [99]}}
        d.write(state / "resources.json", resources)
        d.write(state / "paired-cost-ledger.json", self.ledger())
        self.provider.teardown.return_value = {"passed": True, "inventory": {"passed": True,
            "kinds": {kind: {"passed": True} for kind in self.provider.KINDS}}}
        self.provider._call.return_value = (1, '{"error":{"code":"not_found","message":"Server not found"}}', "")
        return state

    def test_cleanup_unconditional_after_cost_expiry_with_exact_id_get_proofs(self):
        self.cleanup_fixture()
        self.backend._gate.side_effect = ValueError("cost exhausted")
        self.epoch = self.plan["deadline_epoch"] - 1
        result = self.backend.cleanup(self.plan)
        self.assertTrue(result["exact_baseline"])
        self.assertEqual([x["host_id"] for x in result["deletions"]], [1, 2])
        self.assertEqual(self.backend.owned_ids(), [])
        self.assertEqual(self.provider._call.call_count, 2)
        self.assertIn("cost_ledger", result)
        self.backend._gate.assert_not_called()

    def test_cleanup_not_found_auth_timeout_or_empty_inventory_never_proof(self):
        self.cleanup_fixture()
        for response in ((0, '{}', ''), (1, '{"error":{"code":"unauthorized"}}', ''),
                         (1, '{"error":{"code":"not_found","message":"other"}}', '')):
            self.provider._call.return_value = response
            with self.subTest(response=response), self.assertRaises(ValueError):
                self.backend.cleanup(self.plan)
        self.provider.teardown.return_value = {"passed": True, "inventory": {"passed": True, "kinds": {}}}
        with self.assertRaisesRegex(ValueError, "ten-kind"):
            self.backend.cleanup(self.plan)


class RemoteBoundaryTests(NativeFixture):
    def test_composed_launcher_is_minimal_and_only_native_go_gets_sealed_loader_path(self):
        for arm in q.ARMS:
            for mode in ("ingest", "verify"):
                path, grant = self.grant(arm, mode)
                ready = n.ready_receipt(grant)
                release = {"job_sha256": grant["job_sha256"], "release_id": "a" * 64, "controller_ns": 1}
                def read(path, *args):
                    return ready if str(path).endswith("-ready.json") else release
                with mock.patch.object(n, "load_grant", return_value=grant), mock.patch.object(n, "read", side_effect=read), \
                        mock.patch.object(n, "write"), mock.patch.object(n.time, "time", return_value=self.epoch), \
                        mock.patch.dict(n.os.environ, {"LD_LIBRARY_PATH": "/home/ambient", "FDB_NETWORK_OPTION_TRACE_ENABLE": "ambient"}), \
                        mock.patch.object(n.subprocess, "Popen", side_effect=RuntimeError("captured before launch")) as popen, \
                        self.assertRaisesRegex(RuntimeError, "captured before launch"):
                    n.run_job(path)
                args = popen.call_args.args[0]
                self.assertEqual(args[-2:], grant["argv"])
                self.assertIn("-i", args)
                environment = n.launch_environment(grant["job"])
                self.assertEqual(popen.call_args.kwargs["env"], {"PATH": "/usr/bin:/bin", "HOME": "/root"})
                self.assertFalse(any("FDB_NETWORK_OPTION_" in value or "/home/ambient" in value for value in args))
                native = arm == "go_native" and mode == "ingest"
                self.assertEqual("LD_LIBRARY_PATH" in environment, native)
                self.assertEqual(any(value.startswith("LD_LIBRARY_PATH=") for value in args), native)
                if native:
                    directory = str(Path(grant["job"]["client_library"]).parent)
                    self.assertEqual(environment["LD_LIBRARY_PATH"], directory)
                    self.assertIn("LD_LIBRARY_PATH=" + directory, args)
                    self.assertIn("control_native_binary.bundle/", directory)

    def test_native_loader_refuses_wrong_filename_or_colon_directory(self):
        _, original = self.grant("go_native")
        for library in ("/srv/client/libfdb_c-copy.so", "/srv/client:other/libfdb_c.so"):
            job = dict(original["job"], client_library=library)
            with self.subTest(path=library), self.assertRaisesRegex(ValueError, "LD_LIBRARY_PATH"):
                n.launch_environment(job)

    def grant(self, arm="go_pure", mode="ingest"):
        ns, job = self.job(arm)
        if mode != "ingest":
            job = self.backend._control_job(reports.verification_job(job), mode)
        root = "/srv/otel-paired/" + ns
        argv = [self.backend.remote_assets(self.plan)[q.ARM_EXECUTABLES[arm] if mode == "ingest" else "control_pure_binary"],
                root + ("/job.json" if mode == "ingest" else "/" + mode + "-job.json")]
        job["output"] = root + "/" + mode + ".json"
        inputs = {argv[0]: job["binary_sha256"]}
        if job["client_library"] is not None:
            job["client_library_sha256"] = q.NATIVE_SHA256
            inputs[job["client_library"]] = job["client_library_sha256"]
        for key in ("cluster", "corpus", "layout"):
            inputs[job[key + "_file"]] = job[key + "_sha256"]
        seal = q.job_seal(job)
        binary = q.ARM_EXECUTABLES[arm] if mode == "ingest" else "control_pure_binary"
        deployment = q.deployment(self.plan, binary, "/srv/otel-horizontal/" + self.plan["campaign"] + "/bin")
        return root + "/" + mode + "-grant.json", {"schema": "otel_paired_grant_v1", "node_id": 2, "namespace": ns,
            "job": job, "job_sha256": seal, "argv": argv, "inputs": inputs, "deployment": deployment,
            "active_deadline": self.epoch + 100, "hard_deadline": self.epoch + 500,
            "limits": self.plan["native_geometry"]["client_slice"], "scope": "otel-paired-" + seal[:24] + ".scope"}

    def load(self, path, grant, *, check_inputs=False):
        with mock.patch.object(n, "identity"), mock.patch.object(n.time, "time", return_value=self.epoch), \
                mock.patch.object(n, "read", side_effect=lambda path, *args: grant if str(path).endswith("-grant.json") else grant["job"]), \
                mock.patch.object(n, "sha", return_value=grant["job_sha256"]), mock.patch.object(Path, "exists", return_value=False):
            return n.load_grant(path, check_inputs=check_inputs)

    def test_remote_grant_requires_metadata_and_no_extra_keys(self):
        path, grant = self.grant()
        self.assertEqual(self.load(path, grant), grant)
        grant["extra"] = True
        with self.assertRaisesRegex(ValueError, "exact typed grant"):
            self.load(path, grant)

    def test_remote_rejects_extra_argv_changed_job_or_scope(self):
        path, original = self.grant()
        for mutation in (lambda x: x["argv"].append("--mode"), lambda x: x.update(job_sha256="a" * 64),
                         lambda x: x.update(scope="foreign.scope"), lambda x: x.update(active_deadline=self.epoch - 1)):
            grant = copy.deepcopy(original)
            mutation(grant)
            with self.subTest(mutation=mutation), self.assertRaises(ValueError):
                self.load(path, grant)

    def test_native_executables_cannot_verify_even_with_resealed_job(self):
        for arm in ("go_native", "rust_native"):
            path, grant = self.grant(arm, "verify")
            binary = q.ARM_EXECUTABLES[arm]
            grant["argv"][0] = self.backend.remote_assets(self.plan)[binary]
            grant["job"]["binary_sha256"] = self.plan["assets"][binary]["sha256"]
            grant["inputs"][grant["argv"][0]] = grant["job"]["binary_sha256"]
            grant["job_sha256"] = q.job_seal(grant["job"])
            grant["scope"] = "otel-paired-" + grant["job_sha256"][:24] + ".scope"
            with self.subTest(arm=arm), self.assertRaisesRegex(ValueError, "pure Go control"):
                self.load(path, grant)

    def test_remote_ingest_requires_each_exact_arm_executable(self):
        for arm in q.ARMS:
            path, grant = self.grant(arm)
            self.assertEqual(self.load(path, grant), grant)
            for wrong in q.ARMS:
                if wrong == arm:
                    continue
                bad = copy.deepcopy(grant); binary = q.ARM_EXECUTABLES[wrong]
                bad["argv"][0] = self.backend.remote_assets(self.plan)[binary]
                bad["job"]["binary_sha256"] = self.plan["assets"][binary]["sha256"]
                bad["inputs"][bad["argv"][0]] = bad["job"]["binary_sha256"]
                bad["job_sha256"] = q.job_seal(bad["job"])
                bad["scope"] = "otel-paired-" + bad["job_sha256"][:24] + ".scope"
                with self.subTest(arm=arm, wrong=wrong), self.assertRaisesRegex(ValueError, "exact arm executable"):
                    self.load(path, bad)

    def test_remote_all_input_sha_checks_precede_ready(self):
        path, grant = self.grant()
        def digest(file):
            return grant["job_sha256"] if str(file) == grant["argv"][1] else "f" * 64
        with mock.patch.object(n, "identity"), mock.patch.object(n.time, "time", return_value=self.epoch), \
                mock.patch.object(n, "read", side_effect=lambda path, *args: grant if str(path).endswith("-grant.json") else grant["job"]), \
                mock.patch.object(n, "sha", side_effect=digest), mock.patch.object(Path, "exists", return_value=False), \
                mock.patch.object(Path, "stat", return_value=SimpleNamespace(st_mode=stat.S_IFREG | 0o600, st_nlink=1)), \
                self.assertRaisesRegex(ValueError, "remote input SHA"):
            n.load_grant(path)

    def test_duplicate_json_nonfinite_and_existing_outputs_fail_closed(self):
        for raw in ('{"a":1,"a":2}', '{"a":NaN}', '{"a":1e999}'):
            with self.subTest(raw=raw), self.assertRaises(ValueError):
                n.loads(raw)
        path = self.root / "exclusive.json"
        n.write(path, {"ok": True})
        with self.assertRaises(FileExistsError):
            n.write(path, {"ok": False})
        link = self.root / "link.json"
        link.symlink_to(path)
        with self.assertRaisesRegex(ValueError, "symlink"):
            n.read(link)


if __name__ == "__main__":
    unittest.main()
