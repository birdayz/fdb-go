#!/usr/bin/env python3
"""Typed scaling attribution and sealed per-host evidence, with no host effects."""
import copy
from contextlib import contextmanager
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import diagnostic as n
import policy as p
import test_diagnostic as old
import test_forensics as f


def scaling_sampler(role="node2", hosts=3):
    grant = old.sampler_grant("node1" if role == "node1" else "client1")
    index = int(role[-1])
    grant.update(schema=n.SCALING_SAMPLER_SCHEMA, role=role, scaling_hosts=hosts,
                 node_id=100 + index if role.startswith("node") else 200 + index,
                 storage_process_count=3, storage_memory_profile="scaling3ss3g",
                 forensic_budget=n.TRACE_BUDGET, storage_ids={})
    grant["work"] = grant["root"] + "/diagnostics/" + grant["run"] + "-" + role
    grant["processes"] = []
    ports = list(range(4500, 4505)) if role == "node1" else []
    if role.startswith("node"):
        ports += list(range(4510, 4513))
    classes = {4500: "stateless", 4501: "commit_proxy", 4502: "grv_proxy", 4503: "resolution", 4504: "transaction"}
    for port in ports:
        storage = port >= 4510
        address = "10.93.0." + str(10 + index) + ":" + str(port)
        options = {"--cluster-file": grant["arm_root"] + "/fdb.cluster", "--public-address": address,
                   "--listen-address": address, "--class": "storage" if storage else classes[port],
                   "--datadir": grant["arm_root"] + "/data/" + str(port), "--logdir": grant["arm_root"] + "/logs",
                   "--locality-machineid": role, "--locality-zoneid": role,
                   "--memory": "3GiB" if storage else "1GiB", "--cache-memory": "1GiB" if storage else "64MiB",
                   "--trace-format": "json", "--maxlogssize": "128MiB", "--loggroup": grant["campaign"]}
        args = [grant["root"] + "/bin/fdbserver"]
        for flag, value in options.items():
            args.extend((flag, value))
        grant["processes"].append(dict(port=port, address=address, args=args,
                                       unit_name="oh-native-test-diagnostic-" + str(port) + ".service"))
        if storage:
            grant["storage_ids"][address] = format(index * 10000 + port, "016x")
    return grant


def scaling_profile(index=2, hosts=3):
    node, client = "node" + str(index), "client" + str(index)
    grants = {role: scaling_sampler(role, hosts) for role in (node, client)}
    frames = {}
    for role, grant in grants.items():
        rows = [old.frame(grant, sequence) for sequence in range(12)]
        kind = "fdb" if role == node else "client"
        for row in rows:
            row["cgroup"]["path"] = "/sys/fs/cgroup/horizontal.slice/horizontal-" + kind + ".slice"
            if role == client and row["phase"] == "load":
                row["producer"] = old.process(900, 9000, row["sequence"])
        frames[role] = rows
    receipt = dict(started=True, pid=900, starttime_ticks=9000, node_id=grants[client]["node_id"],
                   run=grants[client]["run"], token=grants[client]["token"],
                   grant_sha256=grants[client]["producer_grant_sha256"])
    activity = [dict(storage_processes={address: dict(node=node, storage_id=ssid,
                     input_bytes=value, durable_bytes=value - 1) for address, ssid in grants[node]["storage_ids"].items()})
                for value in (10, 20, 30)]
    return frames, grants, receipt, activity


def evidence_fixture(root, role):
    grant = scaling_sampler(role)
    n.sampler_grant(grant)
    previous, owned = grant["root"], str(Path(root) / "owned")
    for key in ("root", "arm_root", "work", "binary"):
        grant[key] = grant[key].replace(previous, owned)
    for spec in grant["processes"]:
        spec["args"] = [arg.replace(previous, owned) for arg in spec["args"]]
    binary = Path(owned) / "bin/fdbserver"
    binary.parent.mkdir(parents=True)
    binary.write_bytes(b"sealed scaling native binary\n")
    grant["fdbserver_sha256"] = p.sha(binary)
    logs = Path(grant["arm_root"]) / "logs"
    logs.mkdir(parents=True)
    proc_root = Path(root) / "proc"
    boot = proc_root / "sys/kernel/random/boot_id"
    boot.parent.mkdir(parents=True)
    boot.write_text("11111111-2222-3333-4444-555555555555\n")
    bindings, snapshots, paths = {}, {}, {}
    for index, spec in enumerate(grant["processes"]):
        pid, start = 6000 + index, 9000 + index
        snapshots[pid] = old.process(pid, start, args=list(spec["args"]))
        bindings[spec["unit_name"]] = dict(pid=pid, starttime_ticks=start, port=spec["port"],
                                           address=spec["address"], args=list(spec["args"]))
        fd = proc_root / str(pid) / "fd"
        fd.mkdir(parents=True)
        if spec["port"] >= 4510 or spec["port"] == 4504:
            path = logs / ("trace." + spec["address"].replace(":", ".") + ".1790000000.Ab12CD.1.1.json")
            event = dict(Type="StorageMetrics" if spec["port"] >= 4510 else "UnfilteredNativeEvent",
                         Machine=spec["address"], LogGroup=grant["campaign"], ThreadID="not-pid", Payload="x" * 80)
            if spec["port"] >= 4510:
                event["ID"] = grant["storage_ids"][spec["address"]]
            path.write_text((json.dumps(event) + "\n") * 8)
            (fd / "7").symlink_to(path)
            paths[spec["port"]] = path
    return grant, bindings, snapshots, paths, proc_root


class ScalingForensicsTests(unittest.TestCase):
    def setUp(self):
        for name in ("run", "Popen"):
            guard = mock.patch.object(n.subprocess, name, side_effect=AssertionError("real subprocess forbidden"))
            guard.start()
            self.addCleanup(guard.stop)

    @contextmanager
    def snapshots(self, values, *, dead=()):
        def snapshot(pid, **kwargs):
            if pid in dead:
                raise FileNotFoundError("native PID exited")
            return copy.deepcopy(values[pid])
        with mock.patch.object(n, "process_snapshot", side_effect=snapshot) as probe:
            yield probe

    def seal(self, grant, bindings, snapshots, proc_root):
        with self.snapshots(snapshots), mock.patch.object(n.time, "time", return_value=f.SEALED_EPOCH), \
             mock.patch.object(n, "bounded_output", side_effect=f.command_output(bindings, [])):
            identity = n.seal_native_identity(grant, bindings, proc_root=proc_root)
        identity.update(node_id=grant["node_id"], grant_sha256=f.GRANT_SHA, bindings=copy.deepcopy(bindings))
        return identity

    def collect(self, grant, identity, snapshots, proc_root, *, dead=(), budget=None, **options):
        calls = []
        with self.snapshots(snapshots, dead=dead), mock.patch.object(n, "bounded_output", side_effect=f.command_output(
                identity["bindings"], calls, dead=bool(dead), **options)):
            result = n.collect_native_evidence(grant, identity, grant["forensic_budget"] if budget is None else budget,
                                               grant_sha256=f.GRANT_SHA, now_epoch=f.NOW_EPOCH, proc_root=proc_root)
        return result, calls

    def test_exact_typed_host_role_process_id_and_budget_populations(self):
        self.assertEqual(n.ROLES, {"node1", "client1"})
        for hosts in (1, 2, 3):
            for index in range(1, hosts + 1):
                for kind in ("node", "client"):
                    role = kind + str(index)
                    grant = scaling_sampler(role, hosts)
                    before = copy.deepcopy(grant)
                    self.assertIs(n.sampler_grant(grant), grant)
                    self.assertEqual(grant, before)
                    self.assertEqual(len(grant["processes"]), 8 if role == "node1" else 3 if kind == "node" else 0)
                    self.assertEqual(len(grant["storage_ids"]), 3 if kind == "node" else 0)
        for key, values in (("scaling_hosts", (None, True, 0, 4, 3.0, "3")),
                            ("storage_process_count", (None, True, 1, 2, 3.0)),
                            ("storage_memory_profile", (None, "capacity16g", "baseline")),
                            ("forensic_budget", (None, True, -1, n.TRACE_BUDGET + 1, 1.0)),
                            ("role", ("node4", "client4", "node0", "foreign", [])),
                            ("schema", ("otel_native_sampler_v1", "foreign"))):
            for value in values:
                grant = scaling_sampler()
                grant[key] = value
                with self.subTest(key=key, value=value), self.assertRaises(ValueError):
                    n.sampler_grant(grant)
        grant = scaling_sampler("node3", 2)
        with self.assertRaises(ValueError):
            n.sampler_grant(grant)
        for role in ("node2", "client2", "node3", "client3"):
            with self.subTest(role=role), self.assertRaises(ValueError):
                n.sampler_grant(old.sampler_grant(role))

    def test_new_schema_refuses_foreign_process_profile_paths_and_storage_ids(self):
        for failure in ("missing-ss", "log-on-node2", "client-process", "foreign-path", "duplicate-flag", "max128",
                        "memory", "cache", "locality", "class", "ssid", "ssid-size", "ssid-duplicate", "ssid-foreign", "ssid-missing"):
            grant = scaling_sampler("client2" if failure == "client-process" else "node2")
            if failure == "missing-ss":
                grant["processes"].pop()
            elif failure in ("log-on-node2", "client-process"):
                grant["processes"].append(scaling_sampler("node1")["processes"][4])
            elif failure.startswith("ssid"):
                ids = grant["storage_ids"]
                address = next(iter(ids))
                if failure == "ssid": ids[address] = "z" * 16
                elif failure == "ssid-size": ids[address] = "a" * 32
                elif failure == "ssid-duplicate": ids[address] = list(ids.values())[1]
                elif failure == "ssid-foreign": ids["10.93.0.11:4510"] = ids.pop(address)
                else: del ids[address]
            else:
                args = grant["processes"][0]["args"]
                if failure == "duplicate-flag": args.extend(("--memory", "3GiB"))
                elif failure == "max128": args.extend(("--knob_max_outstanding", "128"))
                else:
                    flag = {"foreign-path": "--logdir", "memory": "--memory", "cache": "--cache-memory",
                            "locality": "--locality-machineid", "class": "--class"}[failure]
                    args[args.index(flag) + 1] = "/foreign/arm" if failure == "foreign-path" else "foreign"
            with self.subTest(failure=failure), self.assertRaises(ValueError):
                n.sampler_grant(grant)

    def test_pair_profiles_preserve_raw_roles_page_sizes_and_all_three_independent_storage_ids(self):
        for index in (1, 2, 3):
            for hosts in range(index, 4):
                frames, grants, receipt, activity = scaling_profile(index, hosts)
                for row in frames["node" + str(index)]:
                    row["host_page_size"] = 65536
                before = copy.deepcopy(frames)
                result = n.profile(frames, grants, receipt, activity, True,
                                   node_role="node" + str(index), client_role="client" + str(index))
                self.assertEqual(result["verdict"], "CAPTURED", result)
                self.assertEqual(set(result["populations"]), set(grants))
                self.assertTrue(result["producer_process_bound"])
                self.assertTrue(result["positive_native_activity"])
                self.assertFalse(result["accepted_throughput"])
                self.assertEqual(frames, before)
        frames, grants, receipt, activity = scaling_profile()
        address = next(address for address in grants["node2"]["storage_ids"] if address.endswith(":4512"))
        for failure in ("idle", "idle-durable", "missing", "reset", "durable-reset", "changed-id", "duplicate-id", "foreign-node", "foreign-address"):
            changed = copy.deepcopy(activity)
            if failure == "missing": del changed[-1]["storage_processes"][address]
            elif failure == "reset": changed[-1]["storage_processes"][address]["input_bytes"] = 0
            elif failure == "durable-reset": changed[-1]["storage_processes"][address]["durable_bytes"] = 0
            elif failure == "changed-id": changed[-1]["storage_processes"][address]["storage_id"] = "f" * 16
            else:
                for row in changed:
                    item = row["storage_processes"][address]
                    if failure == "idle": item["input_bytes"] = 10
                    elif failure == "idle-durable": item["durable_bytes"] = 9
                    elif failure == "duplicate-id": item["storage_id"] = list(grants["node2"]["storage_ids"].values())[0]
                    elif failure == "foreign-node": item["node"] = "node3"
                    else: row["storage_processes"]["10.93.0.13:4512"] = row["storage_processes"].pop(address)
            with self.subTest(failure=failure):
                result = n.profile(frames, grants, receipt, changed, True, node_role="node2", client_role="client2")
                self.assertEqual(result["verdict"], "INCONCLUSIVE")
                self.assertFalse(result["positive_native_activity"])

    def test_pair_profiles_refuse_relabeling_schema_host_count_slice_and_producer_identity_changes(self):
        for failure in ("default-pair", "different-index", "schema", "hosts", "grant-role", "frame-role", "slice", "producer-node", "producer-start", "page-size"):
            frames, grants, receipt, activity = scaling_profile()
            pair = dict(node_role="node2", client_role="client2")
            if failure == "default-pair": pair = {}
            elif failure == "different-index": pair["client_role"] = "client3"
            elif failure == "schema": grants["client2"]["schema"] = "otel_native_sampler_v1"
            elif failure == "hosts": grants["client2"]["scaling_hosts"] = 2
            elif failure == "grant-role": grants["client2"]["role"] = "client3"
            elif failure == "frame-role": frames["node2"][0]["role"] = "node1"
            elif failure == "slice": frames["node2"][0]["cgroup"]["path"] = "/sys/fs/cgroup/horizontal.slice/horizontal-client.slice"
            elif failure == "producer-node": receipt["node_id"] += 1
            elif failure == "producer-start": receipt["starttime_ticks"] += 1
            else: frames["node2"][-1]["host_page_size"] = 65536
            with self.subTest(failure=failure):
                result = n.profile(frames, grants, receipt, activity, True, **pair)
                self.assertEqual(result["verdict"], "INCONCLUSIVE", result)
                self.assertTrue(result["reasons"])

    def test_live_and_dead_node2_node3_receipts_are_per_host_and_keep_full_identity(self):
        for role in ("node1", "node2", "node3"):
            for dead in (False, True):
                with self.subTest(role=role, dead=dead), tempfile.TemporaryDirectory() as tmp:
                    grant, bindings, snapshots, paths, proc_root = evidence_fixture(tmp, role)
                    identity = self.seal(grant, bindings, snapshots, proc_root)
                    self.assertEqual(identity["role"], role)
                    self.assertEqual(identity["sampler_schema"], n.SCALING_SAMPLER_SCHEMA)
                    self.assertEqual(identity["scaling_hosts"], 3)
                    self.assertEqual(set(identity["unit_seals"]), set(bindings))
                    self.assertEqual({seal["address"] for seal in identity["trace_files"]},
                                     {bound["address"] for bound in bindings.values() if bound["port"] in paths})
                    for seal in identity["trace_files"]:
                        bound, path = bindings[seal["unit"]], Path(seal["path"])
                        self.assertEqual((seal["pid"], seal["starttime_ticks"], seal["args"]),
                                         (bound["pid"], bound["starttime_ticks"], bound["args"]))
                        self.assertEqual((seal["device"], seal["inode"]), (path.stat().st_dev, path.stat().st_ino))
                        self.assertEqual(seal["prefix_sha256"], hashlib.sha256(path.read_bytes()[:seal["prefix_bytes"]]).hexdigest())
                        self.assertEqual(seal["storage_id"], grant["storage_ids"].get(bound["address"]))
                    before = copy.deepcopy(identity)
                    foreign = Path(grant["arm_root"]) / "logs/trace.10.93.0.99.4510.1790009999.Cd34EF.1.99.json"
                    foreign.write_text("foreign malformed evidence must never be read\n")
                    result, calls = self.collect(grant, identity, snapshots, proc_root, dead=set(snapshots) if dead else ())
                    self.assertEqual(result["forensic_only"], dead)
                    self.assertEqual((result["node_id"], result["role"], result["scaling_hosts"], result["grant_sha256"]),
                                     (grant["node_id"], role, 3, f.GRANT_SHA))
                    self.assertEqual(result["coverage"], "INCONCLUSIVE" if dead else "CAPTURED", result)
                    self.assertEqual(result["errors"], [])
                    self.assertEqual(set(result["identity_verification"].values()), {"DEAD_OR_MISSING" if dead else "LIVE_MATCH"})
                    self.assertEqual({item["name"] for item in result["files"]}, {path.name for path in paths.values()})
                    self.assertLessEqual(result["bytes_read"], grant["forensic_budget"])
                    self.assertEqual(identity, before)
                    self.assertNotIn("accepted_throughput", result)
                    for args, maximum, timeout in calls:
                        self.assertTrue(args[0] in ("systemctl", "journalctl"))
                        if args[0] == "systemctl": self.assertIn(args[2], bindings)
                        else: self.assertIn(next(arg[7:] for arg in args if arg.startswith("--unit=")), bindings)

    def test_dead_receipt_refuses_inode_prefix_path_ssid_and_foreign_units(self):
        for role in ("node2", "node3"):
            for failure in ("inode", "prefix", "path", "ssid-seal", "ssid-event", "unit", "journal"):
                with self.subTest(role=role, failure=failure), tempfile.TemporaryDirectory() as tmp:
                    grant, bindings, snapshots, paths, proc_root = evidence_fixture(tmp, role)
                    identity = self.seal(grant, bindings, snapshots, proc_root)
                    seal = identity["trace_files"][0]
                    path = Path(seal["path"])
                    options = {}
                    if failure == "inode":
                        path.rename(path.with_suffix(".old")); path.write_bytes(path.with_suffix(".old").read_bytes())
                    elif failure == "prefix":
                        raw = path.read_bytes(); path.write_bytes(b"X" + raw[1:])
                    elif failure == "path":
                        foreign = Path(tmp) / path.name; foreign.write_bytes(path.read_bytes()); seal["path"] = str(foreign)
                    elif failure == "ssid-seal": seal["storage_id"] = "f" * 16
                    elif failure == "ssid-event":
                        event = dict(Type="StorageMetrics", Machine=seal["address"], LogGroup=grant["campaign"], ID="f" * 16)
                        with path.open("a") as stream: stream.write(json.dumps(event) + "\n")
                    elif failure == "unit": options["change_properties"] = lambda props: props.update(InvocationID="f" * 32)
                    else: options["change_event"] = lambda event: event.update(_SYSTEMD_UNIT="foreign.service")
                    result, calls = self.collect(grant, identity, snapshots, proc_root, dead=set(snapshots), **options)
                    self.assertEqual(result["coverage"], "INCONCLUSIVE")
                    self.assertTrue(result["errors"])
                    self.assertLessEqual(result["bytes_read"], result["budget"])
                    if failure not in ("unit", "journal"): self.assertEqual(result["files"], [])

    def test_collect_refuses_foreign_role_schema_boot_pid_start_argv_and_over_budget_before_commands(self):
        for failure in ("role", "schema", "hosts", "grant-sha", "boot", "pid", "start", "argv", "exe", "budget", "negative-budget"):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as tmp:
                grant, bindings, snapshots, paths, proc_root = evidence_fixture(tmp, "node2")
                identity = self.seal(grant, bindings, snapshots, proc_root)
                budget = grant["forensic_budget"]
                if failure == "role": identity["role"] = "node3"
                elif failure == "schema": grant["schema"] = "foreign"
                elif failure == "hosts": identity["scaling_hosts"] = 2
                elif failure == "grant-sha": identity["grant_sha256"] = "f" * 64
                elif failure == "boot": (proc_root / "sys/kernel/random/boot_id").write_text("foreign-boot\n")
                elif failure == "budget": grant["forensic_budget"] = budget - 1
                elif failure == "negative-budget": budget = -1
                else:
                    snapshot = snapshots[next(iter(snapshots))]
                    snapshot[{"pid": "pid", "start": "starttime_ticks", "argv": "cmdline", "exe": "exe"}[failure]] = (
                        ["/foreign/fdbserver"] if failure == "argv" else "/foreign/fdbserver" if failure == "exe" else 1)
                with self.snapshots(snapshots), mock.patch.object(n, "bounded_output") as commands, self.assertRaises(ValueError):
                    n.collect_native_evidence(grant, identity, budget, grant_sha256=f.GRANT_SHA,
                                              now_epoch=f.NOW_EPOCH, proc_root=proc_root)
                commands.assert_not_called()

    def test_storage_only_trace_option_is_not_an_untyped_escape_hatch(self):
        with tempfile.TemporaryDirectory() as tmp:
            grant, bindings, snapshots, paths, proc_root = evidence_fixture(tmp, "node2")
            for changes in ({}, {"storage_process_count": 3}, {"storage_process_count": 3, "scaling_grant": dict(grant, schema="foreign")}):
                with self.subTest(changes=changes), self.assertRaises(ValueError):
                    n.trace_tail(grant["arm_root"], grant["campaign"], bindings, require_log=False, **changes)
            result = n.trace_tail(grant["arm_root"], grant["campaign"], bindings, storage_process_count=3,
                                  require_log=False, scaling_grant=grant, storage_ids=grant["storage_ids"])
            self.assertEqual(result["coverage"], "CAPTURED")
            self.assertEqual(len(result["files"]), 3)
            with self.assertRaises(ValueError):
                n.trace_tail(grant["arm_root"], grant["campaign"], bindings, storage_process_count=3,
                             scaling_grant=grant, storage_ids=grant["storage_ids"])

    def test_typed_native_and_client_slices_and_actual_page_size_are_sampled_without_relabeling(self):
        values = {"cpu.max": "400000 100000", "cpuset.cpus.effective": "0-3", "memory.max": "17179869184",
                  "memory.swap.max": "0", "memory.current": "1000", "memory.events": "oom 0\noom_kill 0",
                  "cpu.stat": "usage_usec 1\nuser_usec 1\nsystem_usec 1\nnr_periods 1\nnr_throttled 1\nthrottled_usec 1\n",
                  "io.stat": "8:1 rbytes=1 wbytes=2 rios=1 wios=1\n", "boot_id": "actual-host-boot\n"}
        def small(path, limit=65536):
            return values.get(Path(path).name, "host raw pressure\n")
        for role in ("node2", "node3", "client2", "client3"):
            grant = scaling_sampler(role)
            kind = "fdb" if role.startswith("node") else "client"
            group = "/horizontal.slice/horizontal-" + kind + ".slice"
            with mock.patch.object(n, "command", return_value=group) as command, \
                 mock.patch.object(n, "read_small", side_effect=small), \
                 mock.patch.object(n, "diskstats", return_value={"8:1": {key: 1 for key in n.DISK_FIELDS}}), \
                 mock.patch.object(n.os, "sysconf", side_effect=lambda key: 65536 if key == "SC_PAGESIZE" else 100), \
                 mock.patch.object(n, "producer_process", return_value={"positive": role}) as producer:
                row = n.sample_frame(grant, {}, {"devices": ["8:1"]}, 0, "baseline")
            self.assertEqual(row["role"], role)
            self.assertEqual(row["node_id"], grant["node_id"])
            self.assertEqual(row["host_page_size"], 65536)
            self.assertEqual(row["cgroup"]["path"], "/sys/fs/cgroup" + group)
            command.assert_called_once_with(["systemctl", "show", "horizontal-" + kind + ".slice", "--property=ControlGroup", "--value"])
            if kind == "client": producer.assert_called_once_with(grant)
            else: producer.assert_not_called()

    def test_scaling_client_process_lookup_retains_exact_pid_start_executable_and_argv(self):
        for role in ("client2", "client3"):
            grant = scaling_sampler(role)
            snapshot = old.process(9876, 12345, args=[grant["binary"], "--run", grant["run"], "--mode", "ingest"])
            with mock.patch.object(n.subprocess, "run", return_value=mock.Mock(returncode=0, stdout="/owned-producer.scope\n")) as run, \
                 mock.patch.object(Path, "read_text", return_value="9876\n"), \
                 mock.patch.object(n, "process_snapshot", return_value=snapshot):
                result = n.producer_process(grant)
            self.assertEqual((result["pid"], result["starttime_ticks"], result["exe"], result["cmdline"]),
                             (9876, 12345, grant["binary"], snapshot["cmdline"]))
            run.assert_called_once_with(["systemctl", "show", "otel-native-" + grant["run"] + "-ingest.scope",
                                         "--property=ControlGroup", "--value"], capture_output=True, text=True, timeout=5)

    def test_io_accounting_selects_native_slice_only_in_scaling_context(self):
        for role in ("node2", "node3", "client2", "client3"):
            unit = "horizontal-" + ("fdb" if role.startswith("node") else "client") + ".slice"
            campaign = object.__new__(n.Diagnostic)
            campaign.plan = dict(schema="otel_native_scaling_v1")
            campaign.out = Path("/unused")
            campaign.command = mock.Mock(side_effect=("yes", "/horizontal.slice/" + unit, "literal io.stat\n"))
            with mock.patch.object(n.d, "write") as write:
                campaign.verify_io_accounting(role, {"id": 102})
            self.assertEqual(write.call_args.args[1]["role"], role)
            self.assertEqual(write.call_args.args[1]["unit"], unit)


if __name__ == "__main__":
    unittest.main()
