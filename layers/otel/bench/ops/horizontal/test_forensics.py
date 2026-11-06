#!/usr/bin/env python3
"""Bounded native evidence after process exit, without subprocesses or host effects."""
import base64
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
from test_diagnostic import process, sampler_grant


def fixture(root):
    grant = sampler_grant(storage_process_count=2)
    previous = grant["root"]; owned = str(Path(root)/"owned")
    for key in ("root", "arm_root", "work", "binary"):
        grant[key] = grant[key].replace(previous, owned)
    for spec in grant["processes"]:
        spec["args"] = [arg.replace(previous, owned) for arg in spec["args"]]
    binary = Path(owned)/"bin"/"fdbserver"; binary.parent.mkdir(parents=True)
    binary.write_bytes(b"sealed native binary\n"); grant["fdbserver_sha256"] = p.sha(binary)
    grant["storage_ids"] = {"10.93.0.11:4510": "storage-4510", "10.93.0.11:4511": "storage-4511"}
    logs = Path(grant["arm_root"])/"logs"; logs.mkdir(parents=True)
    proc_root = Path(root)/"proc"; proc_root.mkdir()
    boot = proc_root/"sys"/"kernel"/"random"/"boot_id"; boot.parent.mkdir(parents=True)
    boot.write_text("11111111-2222-3333-4444-555555555555\n")
    bindings, snapshots, paths = {}, {}, {}
    for index, spec in enumerate(grant["processes"]):
        pid, start = 6000+index, 9000+index
        snapshots[pid] = process(pid, start, args=list(spec["args"]))
        bindings[spec["unit_name"]] = {"pid": pid, "starttime_ticks": start, "port": spec["port"],
                                       "address": spec["address"], "args": list(spec["args"])}
        fd = proc_root/str(pid)/"fd"; fd.mkdir(parents=True)
        if spec["port"] in (4504, 4510, 4511):
            path = logs/("trace.10.93.0.11."+str(spec["port"])+".1790000000.Ab12CD.1.1.json")
            event = {"Type": "UnfilteredNativeEvent", "Machine": spec["address"], "LogGroup": "native-test",
                     "ThreadID": "not-the-process-pid", "Payload": "x"*80}
            path.write_text((json.dumps(event)+"\n")*8)
            (fd/"7").symlink_to(path)
            paths[spec["port"]] = path
    return grant, bindings, snapshots, paths, proc_root


GRANT_SHA = "e"*64
SEALED_EPOCH = 1100
NOW_EPOCH = 1200


def packet(raw, maximum, *, exit_code=0, truncated=False, timed_out=False):
    retained = raw[:maximum]
    return {"exit": exit_code, "raw_base64": base64.b64encode(retained).decode(),
            "bytes_read": len(retained), "truncated": truncated or len(retained) < len(raw), "timed_out": timed_out}


def properties(unit, bound, *, dead=False):
    return {"Id": unit, "FragmentPath": "/etc/systemd/system/"+unit,
            "InvocationID": hashlib.sha256(unit.encode()).hexdigest()[:32],
            "ControlGroup": "" if dead else "/horizontal.slice/horizontal-fdb.slice/"+unit,
            "ExecStart": " ".join(bound["args"]), "MainPID": "0" if dead else str(bound["pid"]),
            "ExecMainPID": str(bound["pid"]), "ActiveState": "failed" if dead else "active",
            "SubState": "failed" if dead else "running", "Result": "signal" if dead else "success",
            "ExecMainCode": "2" if dead else "0", "ExecMainStatus": "9" if dead else "0",
            "ExecMainStartTimestampMonotonic": "1000000", "ExecMainExitTimestampMonotonic": "1100000" if dead else "0",
            "NRestarts": "0"}


def command_output(bindings, calls, *, dead=False, change_properties=None, change_event=None,
                   journal_exit=0, journal_truncated=False, journal_timed_out=False):
    def output(args, max_bytes, timeout=5):
        calls.append((list(args), max_bytes, timeout))
        if args[:2] == ["systemctl", "show"]:
            unit = args[2]; props = properties(unit, bindings[unit], dead=dead)
            if change_properties:
                change_properties(props)
            return packet("".join(key+"="+value+"\n" for key, value in props.items()).encode(), max_bytes)
        if args[0] == "journalctl":
            unit = next(arg.split("=", 1)[1] for arg in args if arg.startswith("--unit="))
            event = {"__REALTIME_TIMESTAMP": "1120000000", "_SYSTEMD_UNIT": unit,
                     "_SYSTEMD_INVOCATION_ID": properties(unit, bindings[unit])["InvocationID"],
                     "MESSAGE": "literal retained native unit event", "PRIORITY": "3"}
            if change_event:
                change_event(event)
            return packet((json.dumps(event)+"\n").encode(), max_bytes, exit_code=journal_exit,
                          truncated=journal_truncated, timed_out=journal_timed_out)
        raise AssertionError("unexpected forensic command: "+repr(args))
    return output


class NativeEvidenceTests(unittest.TestCase):
    def setUp(self):
        for name in ("run", "Popen"):
            guard = mock.patch.object(n.subprocess, name, side_effect=AssertionError("real subprocess forbidden"))
            guard.start(); self.addCleanup(guard.stop)

    @contextmanager
    def snapshots(self, values, *, dead=()):
        def snapshot(pid, **kwargs):
            if pid in dead:
                raise FileNotFoundError("native PID exited")
            return copy.deepcopy(values[pid])
        with mock.patch.object(n, "process_snapshot", side_effect=snapshot) as probe:
            yield probe

    def seal(self, grant, bindings, snapshots, proc_root):
        calls = []
        with self.snapshots(snapshots), mock.patch.object(n, "time") as clock, \
             mock.patch.object(n, "bounded_output", side_effect=command_output(bindings, calls)):
            clock.time.return_value = SEALED_EPOCH
            identity = n.seal_native_identity(grant, bindings, proc_root=proc_root)
        identity.update(node_id=grant["node_id"], grant_sha256=GRANT_SHA, bindings=copy.deepcopy(bindings))
        return identity

    def collect(self, grant, identity, snapshots, proc_root, *, dead=(), budget=n.TRACE_BUDGET, **options):
        calls = []
        with self.snapshots(snapshots, dead=dead), \
             mock.patch.object(n, "bounded_output", side_effect=command_output(identity["bindings"], calls, dead=bool(dead), **options)):
            result = n.collect_native_evidence(grant, identity, budget, grant_sha256=GRANT_SHA,
                                               now_epoch=NOW_EPOCH, proc_root=proc_root)
        return result, calls

    def assert_refused(self, result):
        self.assertEqual(result["files"], [])
        self.assertEqual(result["coverage"], "INCONCLUSIVE")
        self.assertTrue(result["errors"])
        self.assertLessEqual(result["bytes_read"], result["budget"])

    def test_seals_exact_three_live_open_trace_files_with_identity_inode_and_prefix(self):
        with tempfile.TemporaryDirectory() as tmp:
            grant, bindings, snapshots, paths, proc_root = fixture(tmp)
            with self.snapshots(snapshots) as probe, mock.patch.object(n, "bounded_output", side_effect=AssertionError("trace seal ran command")):
                seals = n.seal_trace_files(grant, bindings, proc_root=proc_root)
            self.assertEqual(len(seals), 3)
            self.assertEqual(probe.call_count, 6)
            self.assertEqual({seal["address"] for seal in seals}, {"10.93.0.11:"+str(port) for port in paths})
            for seal in seals:
                bound = bindings[seal["unit"]]; path = Path(seal["path"]); info = path.stat()
                self.assertEqual(seal["name"], path.name)
                self.assertEqual(path, paths[bound["port"]])
                self.assertEqual((seal["device"], seal["inode"], seal["size_at_seal"]), (info.st_dev, info.st_ino, info.st_size))
                self.assertEqual((seal["pid"], seal["starttime_ticks"], seal["args"]),
                                 (bound["pid"], bound["starttime_ticks"], bound["args"]))
                prefix = path.read_bytes()[:seal["prefix_bytes"]]
                self.assertEqual(seal["prefix_sha256"], hashlib.sha256(prefix).hexdigest())
                self.assertGreater(seal["prefix_bytes"], 0)
                self.assertEqual(seal["storage_id"], grant["storage_ids"].get(bound["address"]))

    def test_missing_live_open_fd_foreign_arm_or_symlink_cannot_be_sealed(self):
        for failure in ("no-fd", "foreign-arm", "symlink"):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as tmp:
                grant, bindings, snapshots, paths, proc_root = fixture(tmp)
                bound = next(value for value in bindings.values() if value["port"] == 4510)
                fd = proc_root/str(bound["pid"])/"fd"/"7"; fd.unlink()
                if failure != "no-fd":
                    foreign = Path(tmp)/paths[4510].name; foreign.write_bytes(paths[4510].read_bytes())
                    if failure == "foreign-arm":
                        fd.symlink_to(foreign)
                    else:
                        paths[4510].unlink(); paths[4510].symlink_to(foreign); fd.symlink_to(paths[4510])
                with self.snapshots(snapshots), self.assertRaises(ValueError):
                    n.seal_trace_files(grant, bindings, proc_root=proc_root)

    def test_trace_seal_requires_matching_live_process_before_and_after_prefix_read(self):
        for field, value in (("pid", 99999), ("starttime_ticks", 1), ("cmdline", ["/foreign/fdbserver"]), ("exe", "/foreign/fdbserver")):
            with self.subTest(field=field), tempfile.TemporaryDirectory() as tmp:
                grant, bindings, snapshots, paths, proc_root = fixture(tmp)
                bound = next(value for value in bindings.values() if value["port"] == 4504)
                snapshots[bound["pid"]][field] = value
                with self.snapshots(snapshots), self.assertRaisesRegex(ValueError, "PID reuse|command changed"):
                    n.seal_trace_files(grant, bindings, proc_root=proc_root)
        with tempfile.TemporaryDirectory() as tmp:
            grant, bindings, snapshots, paths, proc_root = fixture(tmp)
            bound = next(value for value in bindings.values() if value["port"] == 4504)
            first = copy.deepcopy(snapshots[bound["pid"]]); reused = dict(first, starttime_ticks=1)
            with mock.patch.object(n, "process_snapshot", side_effect=[first, reused]), self.assertRaises(ValueError):
                n.seal_trace_files(grant, bindings, proc_root=proc_root)

    def test_native_identity_seal_binds_boot_invocations_and_accounts_all_startup_bytes(self):
        with tempfile.TemporaryDirectory() as tmp:
            grant, bindings, snapshots, paths, proc_root = fixture(tmp)
            identity = self.seal(grant, bindings, snapshots, proc_root)
            self.assertEqual(identity["boot_id"], "11111111-2222-3333-4444-555555555555")
            self.assertEqual(identity["sealed_epoch"], SEALED_EPOCH)
            self.assertEqual(set(identity["unit_seals"]), set(bindings))
            self.assertEqual(len(identity["trace_files"]), 3)
            property_bytes = sum(len("".join(key+"="+value+"\n" for key, value in properties(unit, bound).items()).encode())
                                 for unit, bound in bindings.items())
            self.assertEqual(identity["seal_bytes_read"], property_bytes+sum(seal["prefix_bytes"] for seal in identity["trace_files"]))
            for unit, seal in identity["unit_seals"].items():
                self.assertEqual(seal["invocation_id"], properties(unit, bindings[unit])["InvocationID"])

    def test_positive_unit_seal_refuses_missing_live_process_or_unknown_foreign_inactive_unit(self):
        changes = (("ActiveState", "failed"), ("MainPID", "0"), ("ExecMainPID", "99999"),
                   ("InvocationID", ""), ("ControlGroup", ""), ("FragmentPath", "/foreign/owned.service"),
                   ("command-exit", None), ("command-truncated", True), ("command-timed-out", True), ("dead", None))
        for key, value in changes:
            with self.subTest(key=key), tempfile.TemporaryDirectory() as tmp:
                grant, bindings, snapshots, paths, proc_root = fixture(tmp); calls = []
                def change(props):
                    if key in props: props[key] = value
                output = command_output(bindings, calls, change_properties=change)
                def broken(args, maximum, timeout=5):
                    result = output(args, maximum, timeout)
                    if key == "command-exit": result["exit"] = value
                    elif key == "command-truncated": result["truncated"] = value
                    elif key == "command-timed-out": result["timed_out"] = value
                    return result
                with self.snapshots(snapshots, dead=set(snapshots) if key == "dead" else ()), \
                     mock.patch.object(n, "bounded_output", side_effect=broken), mock.patch.object(n, "seal_trace_files") as traces:
                    with self.assertRaises((ValueError, FileNotFoundError)):
                        n.seal_native_identity(grant, bindings, proc_root=proc_root)
                    traces.assert_not_called()

    def test_dead_process_retains_only_sealed_raw_files_and_never_becomes_live_health(self):
        with tempfile.TemporaryDirectory() as tmp:
            grant, bindings, snapshots, paths, proc_root = fixture(tmp)
            identity = self.seal(grant, bindings, snapshots, proc_root)
            newer = paths[4510].with_name("trace.10.93.0.11.4510.1790000100.Cd34EF.1.2.json")
            newer.write_text("unsealed malformed file must not be discovered\n")
            result, calls = self.collect(grant, identity, snapshots, proc_root, dead=set(snapshots))
            self.assertTrue(result["forensic_only"])
            self.assertEqual(result["coverage"], "INCONCLUSIVE")
            self.assertEqual(set(result["identity_verification"].values()), {"DEAD_OR_MISSING"})
            self.assertEqual(result["errors"], [])
            self.assertEqual({item["name"] for item in result["files"]}, {path.name for path in paths.values()})
            for item in result["files"]:
                bound = next(value for value in bindings.values() if value["address"] == item["address"])
                self.assertEqual(base64.b64decode(item["raw_base64"]), paths[bound["port"]].read_bytes())
            self.assertEqual(result["bytes_read"], identity["seal_bytes_read"]+result["unit_forensics"]["bytes_read"]
                             +sum(seal["prefix_bytes"] for seal in identity["trace_files"])+sum(item["bytes_read"] for item in result["files"]))
            self.assertNotIn("accepted_throughput", result)
            self.assertNotIn("health_pass", result)
            self.assertTrue(all(item["status"] == "RECORDED" for item in result["unit_forensics"]["units"].values()))

    def test_live_collection_reseals_only_current_open_rotation_not_newest_unopened_name(self):
        with tempfile.TemporaryDirectory() as tmp:
            grant, bindings, snapshots, paths, proc_root = fixture(tmp)
            identity = self.seal(grant, bindings, snapshots, proc_root); before = copy.deepcopy(identity)
            opened = paths[4504].with_name("trace.10.93.0.11.4504.1790000100.Cd34EF.1.2.json")
            opened.write_bytes(paths[4504].read_bytes())
            unowned = paths[4504].with_name("trace.10.93.0.11.4504.1790000200.Xy56ZW.1.3.json")
            unowned.write_text("unopened malformed newest name\n")
            bound = next(value for value in bindings.values() if value["port"] == 4504)
            fd = proc_root/str(bound["pid"])/"fd"/"7"; fd.unlink(); fd.symlink_to(opened)
            result, calls = self.collect(grant, identity, snapshots, proc_root)
            self.assertFalse(result["forensic_only"])
            self.assertEqual(result["coverage"], "CAPTURED")
            self.assertEqual(set(result["identity_verification"].values()), {"LIVE_MATCH"})
            self.assertEqual({item["name"] for item in result["files"]}, {opened.name, paths[4510].name, paths[4511].name})
            self.assertEqual(identity, before)
            prefixes = sum(seal["prefix_bytes"] for seal in result["sealed_trace_files"])
            self.assertEqual(result["bytes_read"], identity["seal_bytes_read"]+result["unit_forensics"]["bytes_read"]
                             +2*prefixes+sum(item["bytes_read"] for item in result["files"]))

    def test_pid_reuse_changed_argv_or_executable_is_not_a_dead_process_fallback(self):
        for field, value in (("starttime_ticks", 1), ("cmdline", ["/foreign/fdbserver"]), ("exe", "/foreign/fdbserver")):
            with self.subTest(field=field), tempfile.TemporaryDirectory() as tmp:
                grant, bindings, snapshots, paths, proc_root = fixture(tmp)
                identity = self.seal(grant, bindings, snapshots, proc_root)
                snapshots[next(iter(snapshots))][field] = value
                with self.snapshots(snapshots), mock.patch.object(n, "bounded_output") as commands:
                    with self.assertRaises(ValueError):
                        n.collect_native_evidence(grant, identity, grant_sha256=GRANT_SHA, now_epoch=NOW_EPOCH, proc_root=proc_root)
                    commands.assert_not_called()

    def test_identity_grant_boot_and_binding_population_mismatches_refuse_before_commands(self):
        for failure in ("node", "grant", "boot", "missing-unit", "duplicate-pid", "args", "port", "address"):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as tmp:
                grant, bindings, snapshots, paths, proc_root = fixture(tmp)
                identity = self.seal(grant, bindings, snapshots, proc_root)
                bound = next(iter(identity["bindings"].values()))
                if failure == "node": identity["node_id"] = 102
                elif failure == "grant": identity["grant_sha256"] = "f"*64
                elif failure == "boot": (proc_root/"sys/kernel/random/boot_id").write_text("changed-boot\n")
                elif failure == "missing-unit": identity["bindings"].pop(next(iter(identity["bindings"])))
                elif failure == "duplicate-pid": list(identity["bindings"].values())[1]["pid"] = bound["pid"]
                elif failure == "args": bound["args"] = ["/foreign/fdbserver"]
                elif failure == "port": bound["port"] = 4599
                else: bound["address"] = "10.93.0.12:4500"
                with self.snapshots(snapshots), mock.patch.object(n, "bounded_output") as commands:
                    with self.assertRaises(ValueError):
                        n.collect_native_evidence(grant, identity, grant_sha256=GRANT_SHA, now_epoch=NOW_EPOCH, proc_root=proc_root)
                    commands.assert_not_called()

    def test_dead_sealed_file_refuses_foreign_path_symlink_inode_size_and_prefix_changes(self):
        for failure in ("foreign-path", "symlink", "inode", "truncate", "prefix", "receipt-pid", "receipt-args", "receipt-address"):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as tmp:
                grant, bindings, snapshots, paths, proc_root = fixture(tmp)
                identity = self.seal(grant, bindings, snapshots, proc_root)
                seal = next(item for item in identity["trace_files"] if item["address"].endswith(":4510"))
                path = paths[4510]
                if failure in ("foreign-path", "symlink"):
                    foreign = Path(tmp)/path.name; foreign.write_bytes(path.read_bytes())
                    if failure == "foreign-path": seal["path"] = str(foreign)
                    else: path.unlink(); path.symlink_to(foreign)
                elif failure == "inode":
                    raw = path.read_bytes(); path.rename(path.with_suffix(".old")); path.write_bytes(raw)
                elif failure == "truncate": path.write_bytes(b"short\n")
                elif failure == "prefix":
                    raw = path.read_bytes(); path.write_bytes(b"X"+raw[1:])
                elif failure == "receipt-pid": seal["pid"] += 1
                elif failure == "receipt-args": seal["args"] = ["/foreign/fdbserver"]
                else: seal["address"] = "10.93.0.12:4510"
                result, calls = self.collect(grant, identity, snapshots, proc_root, dead=set(snapshots))
                self.assert_refused(result)

    def test_dead_trace_storage_seal_must_match_controller_storage_ids_even_without_id_event(self):
        for failure in ("grant-id", "seal-id"):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as tmp:
                grant, bindings, snapshots, paths, proc_root = fixture(tmp)
                identity = self.seal(grant, bindings, snapshots, proc_root)
                if failure == "grant-id": grant["storage_ids"]["10.93.0.11:4510"] = "replacement"
                else:
                    next(item for item in identity["trace_files"] if item["address"].endswith(":4510"))["storage_id"] = "replacement"
                result, calls = self.collect(grant, identity, snapshots, proc_root, dead=set(snapshots))
                self.assert_refused(result)

    def test_storage_identity_events_must_match_exact_ssid_not_just_address_or_loggroup(self):
        for kind in ("StorageMetrics", "StorageServerInit", "Role"):
            with self.subTest(kind=kind), tempfile.TemporaryDirectory() as tmp:
                grant, bindings, snapshots, paths, proc_root = fixture(tmp)
                event = {"Type": kind, "Machine": "10.93.0.11:4511", "LogGroup": "native-test", "ID": "replacement"}
                if kind == "Role": event.update(As="StorageServer", Transition="Begin")
                paths[4511].write_text(json.dumps(event)+"\n")
                identity = self.seal(grant, bindings, snapshots, proc_root)
                result, calls = self.collect(grant, identity, snapshots, proc_root, dead=set(snapshots))
                self.assert_refused(result)

    def test_zero_and_small_global_budgets_bound_startup_properties_journals_prefixes_and_tails(self):
        with tempfile.TemporaryDirectory() as tmp:
            grant, bindings, snapshots, paths, proc_root = fixture(tmp)
            grant["forensic_budget"] = 0
            identity = self.seal(grant, bindings, snapshots, proc_root)
            self.assertEqual(identity["seal_bytes_read"], 0)
            self.assertEqual(identity["trace_files"], [])
            result, calls = self.collect(grant, identity, snapshots, proc_root, budget=0)
            self.assertEqual(result["bytes_read"], 0)
            self.assertEqual(result["files"], [])
            self.assertEqual(result["coverage"], "INCONCLUSIVE")
            self.assertEqual(calls, [])
        for budget in (32768, 65536, n.TRACE_BUDGET):
            with self.subTest(budget=budget), tempfile.TemporaryDirectory() as tmp:
                grant, bindings, snapshots, paths, proc_root = fixture(tmp)
                identity = self.seal(grant, bindings, snapshots, proc_root)
                result, calls = self.collect(grant, identity, snapshots, proc_root, budget=budget)
                self.assertLessEqual(result["bytes_read"], budget)
                self.assertGreaterEqual(result["bytes_read"], identity["seal_bytes_read"])
                self.assertTrue(all(0 < maximum <= min(budget, n.FORENSIC_LIMIT) for args, maximum, timeout in calls))
        with tempfile.TemporaryDirectory() as tmp:
            grant, bindings, snapshots, paths, proc_root = fixture(tmp)
            identity = self.seal(grant, bindings, snapshots, proc_root)
            for budget in (True, -1, 1.0, n.TRACE_BUDGET+1, identity["seal_bytes_read"]-1):
                with self.subTest(budget=budget), self.assertRaises(ValueError):
                    n.collect_native_evidence(grant, identity, budget, grant_sha256=GRANT_SHA, now_epoch=NOW_EPOCH, proc_root=proc_root)

    def test_unit_forensics_uses_exact_owned_units_bounded_properties_and_original_arm_journal_window(self):
        with tempfile.TemporaryDirectory() as tmp:
            grant, bindings, snapshots, paths, proc_root = fixture(tmp)
            identity = self.seal(grant, bindings, snapshots, proc_root); calls = []
            with mock.patch.object(n, "bounded_output", side_effect=command_output(bindings, calls, dead=True)):
                result = n.unit_forensics(grant, identity, 65536, now_epoch=NOW_EPOCH)
            self.assertEqual((result["start_epoch"], result["end_epoch"]), (SEALED_EPOCH, NOW_EPOCH))
            self.assertEqual(set(result["units"]), set(bindings))
            self.assertEqual(result["errors"], [])
            self.assertEqual(len(calls), 14)
            self.assertEqual(result["bytes_read"], sum(
                len(base64.b64decode(item["properties_raw_base64"]))+item["journal"]["bytes_read"]
                for item in result["units"].values()))
            for args, maximum, timeout in calls:
                self.assertTrue(all(type(arg) is str for arg in args))
                self.assertGreater(maximum, 0); self.assertLessEqual(maximum, 65536)
                self.assertGreater(timeout, 0); self.assertLessEqual(timeout, 5)
                if args[0] == "systemctl":
                    self.assertEqual(args[1], "show"); self.assertIn(args[2], bindings)
                    self.assertEqual(set(args[3:]), {"--property="+key for key in n.UNIT_PROPERTIES})
                    self.assertEqual(maximum, 4096)
                else:
                    self.assertEqual(args[0], "journalctl")
                    unit = next(arg.split("=", 1)[1] for arg in args if arg.startswith("--unit="))
                    self.assertIn(unit, bindings)
                    self.assertIn("--since=@1100.000000", args); self.assertIn("--until=@1200.000000", args)
                    self.assertIn("--lines=128", args); self.assertIn("--no-pager", args)
                    self.assertIn("--output=json", args)
                    fields = next(arg for arg in args if arg.startswith("--output-fields="))
                    self.assertNotIn("COREDUMP", fields)
            for unit, item in result["units"].items():
                self.assertEqual(item["status"], "RECORDED")
                self.assertEqual(item["properties"]["ExecMainStatus"], "9")
                self.assertEqual(item["properties"]["MainPID"], "0")
                self.assertEqual(item["journal"]["complete_records"], 1)
                event = json.loads(base64.b64decode(item["journal"]["raw_base64"]))
                self.assertEqual(event["_SYSTEMD_UNIT"], unit)
                self.assertEqual(event["_SYSTEMD_INVOCATION_ID"], identity["unit_seals"][unit]["invocation_id"])

    def test_unit_forensics_rejects_out_of_lease_time_windows_before_commands(self):
        for sealed, now in ((999, NOW_EPOCH), (1300, NOW_EPOCH), (SEALED_EPOCH, 1099),
                            (SEALED_EPOCH, 7001), (SEALED_EPOCH, 8201), (True, NOW_EPOCH), (SEALED_EPOCH, float("nan"))):
            with self.subTest(sealed=sealed, now=now), tempfile.TemporaryDirectory() as tmp:
                grant, bindings, snapshots, paths, proc_root = fixture(tmp)
                identity = self.seal(grant, bindings, snapshots, proc_root); identity["sealed_epoch"] = sealed
                with mock.patch.object(n, "bounded_output") as commands, self.assertRaises(ValueError):
                    n.unit_forensics(grant, identity, 65536, now_epoch=now)
                commands.assert_not_called()

    def test_missing_unit_seals_or_zero_budget_stays_unknown_without_commands(self):
        with tempfile.TemporaryDirectory() as tmp:
            grant, bindings, snapshots, paths, proc_root = fixture(tmp)
            identity = self.seal(grant, bindings, snapshots, proc_root)
            for budget, sealed in ((0, identity), (4095, identity), (65536, dict(identity, unit_seals={}))):
                with self.subTest(budget=budget, seals=bool(sealed["unit_seals"])), mock.patch.object(n, "bounded_output") as commands:
                    result = n.unit_forensics(grant, sealed, budget, now_epoch=NOW_EPOCH)
                commands.assert_not_called()
                self.assertEqual(result["bytes_read"], 0)
                self.assertEqual(set(result["units"]), set(bindings))
                self.assertEqual({item["status"] for item in result["units"].values()}, {"UNKNOWN"})
            for budget in (True, -1, 1.0, n.TRACE_BUDGET+1):
                with self.subTest(budget=budget), self.assertRaises(ValueError):
                    n.unit_forensics(grant, identity, budget, now_epoch=NOW_EPOCH)

    def test_foreign_unit_or_arm_binding_cannot_authorize_unit_commands(self):
        for failure in ("foreign-unit", "foreign-arm", "wrong-unit-port"):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as tmp:
                grant, bindings, snapshots, paths, proc_root = fixture(tmp)
                identity = self.seal(grant, bindings, snapshots, proc_root)
                if failure == "foreign-unit":
                    original = next(iter(identity["bindings"]))
                    identity["bindings"]["foundationdb.service"] = identity["bindings"].pop(original)
                    identity["unit_seals"]["foundationdb.service"] = identity["unit_seals"].pop(original)
                elif failure == "foreign-arm":
                    grant["arm_root"] = str(Path(tmp)/"foreign-arm")
                else:
                    next(iter(identity["bindings"].values()))["port"] = 4519
                with mock.patch.object(n, "bounded_output") as commands, self.assertRaises(ValueError):
                    n.unit_forensics(grant, identity, 65536, now_epoch=NOW_EPOCH)
                commands.assert_not_called()

    def test_unit_property_identity_invocation_pid_and_cgroup_changes_are_inconclusive(self):
        changes = (("Id", "foundationdb.service"), ("FragmentPath", "/usr/lib/systemd/system/foreign.service"),
                   ("InvocationID", "f"*32), ("InvocationID", "malformed"), ("ExecMainPID", "99999"),
                   ("MainPID", "99999"), ("ControlGroup", "/system.slice/foundationdb.service"))
        for key, value in changes:
            with self.subTest(key=key, value=value), tempfile.TemporaryDirectory() as tmp:
                grant, bindings, snapshots, paths, proc_root = fixture(tmp)
                identity = self.seal(grant, bindings, snapshots, proc_root); calls = []
                def change(props): props[key] = value
                with mock.patch.object(n, "bounded_output", side_effect=command_output(bindings, calls, dead=True, change_properties=change)):
                    result = n.unit_forensics(grant, identity, 65536, now_epoch=NOW_EPOCH)
                self.assertEqual(len(calls), 7)
                self.assertTrue(all(args[0] == "systemctl" for args, maximum, timeout in calls))
                self.assertEqual(len(result["errors"]), 7)
                self.assertEqual({item["status"] for item in result["units"].values()}, {"INCONCLUSIVE"})

    def test_journal_records_require_exact_unit_invocation_and_microsecond_window(self):
        changes = (("_SYSTEMD_UNIT", "foundationdb.service"), ("_SYSTEMD_INVOCATION_ID", "f"*32),
                   ("__REALTIME_TIMESTAMP", "1099000000"), ("__REALTIME_TIMESTAMP", "1201000000"),
                   ("__REALTIME_TIMESTAMP", 1120000000), ("__REALTIME_TIMESTAMP", "NaN"))
        for key, value in changes:
            with self.subTest(key=key, value=value), tempfile.TemporaryDirectory() as tmp:
                grant, bindings, snapshots, paths, proc_root = fixture(tmp)
                identity = self.seal(grant, bindings, snapshots, proc_root); calls = []
                def change(event): event[key] = value
                with mock.patch.object(n, "bounded_output", side_effect=command_output(bindings, calls, dead=True, change_event=change)):
                    result = n.unit_forensics(grant, identity, 65536, now_epoch=NOW_EPOCH)
                self.assertEqual(len(result["errors"]), 7)
                self.assertTrue(all(item["status"] == "INCONCLUSIVE" for item in result["units"].values()))
                self.assertTrue(all("journal" not in item for item in result["units"].values()))

    def test_pid_one_manager_journal_is_owned_only_with_exact_unit_and_optional_matching_invocation(self):
        for pid in ("1", "2"):
            with self.subTest(pid=pid), tempfile.TemporaryDirectory() as tmp:
                grant, bindings, snapshots, paths, proc_root = fixture(tmp)
                identity = self.seal(grant, bindings, snapshots, proc_root); calls = []
                def manager(event):
                    event["UNIT"] = event.pop("_SYSTEMD_UNIT")
                    event["INVOCATION_ID"] = event.pop("_SYSTEMD_INVOCATION_ID")
                    event["_PID"] = pid
                with mock.patch.object(n, "bounded_output", side_effect=command_output(bindings, calls, dead=True, change_event=manager)):
                    result = n.unit_forensics(grant, identity, 65536, now_epoch=NOW_EPOCH)
                self.assertEqual({item["status"] for item in result["units"].values()}, {"RECORDED" if pid == "1" else "INCONCLUSIVE"})
                self.assertEqual(len(result["errors"]), 0 if pid == "1" else 7)

    def test_unknown_command_exit_journal_truncation_or_timeout_never_claim_complete_unit_evidence(self):
        for options in ({"journal_exit": None}, {"journal_exit": 1}, {"journal_truncated": True}, {"journal_timed_out": True}):
            with self.subTest(options=options), tempfile.TemporaryDirectory() as tmp:
                grant, bindings, snapshots, paths, proc_root = fixture(tmp)
                identity = self.seal(grant, bindings, snapshots, proc_root)
                result, calls = self.collect(grant, identity, snapshots, proc_root, dead=set(snapshots), **options)
                self.assertTrue(result["forensic_only"])
                self.assertEqual(result["coverage"], "INCONCLUSIVE")
                self.assertTrue(all(item["status"] not in ("RECORDED", "PASS") for item in result["unit_forensics"]["units"].values()))
                self.assertLessEqual(result["bytes_read"], result["budget"])

    def test_partial_journal_tail_is_not_parsed_or_retained_but_counts_against_budget(self):
        with tempfile.TemporaryDirectory() as tmp:
            grant, bindings, snapshots, paths, proc_root = fixture(tmp)
            identity = self.seal(grant, bindings, snapshots, proc_root); calls = []
            output = command_output(bindings, calls, dead=True)
            partial = b'{"MESSAGE":"partial record'
            def with_partial(args, maximum, timeout=5):
                value = output(args, maximum, timeout)
                if args[0] == "journalctl":
                    raw = base64.b64decode(value["raw_base64"])+partial
                    return packet(raw, maximum, truncated=True)
                return value
            with mock.patch.object(n, "bounded_output", side_effect=with_partial):
                result = n.unit_forensics(grant, identity, 65536, now_epoch=NOW_EPOCH)
            for item in result["units"].values():
                journal = item["journal"]; raw = base64.b64decode(journal["raw_base64"])
                self.assertEqual(item["status"], "INCONCLUSIVE")
                self.assertEqual(journal["complete_records"], 1)
                self.assertEqual(journal["partial_tail_bytes"], len(partial))
                self.assertEqual(journal["bytes_read"], len(raw)+len(partial))
                self.assertNotIn(partial, raw)


class BoundedOutputTests(unittest.TestCase):
    def test_zero_budget_and_invalid_bounds_never_spawn_a_command(self):
        with mock.patch.object(n.subprocess, "Popen", side_effect=AssertionError("subprocess forbidden")) as spawn:
            result = n.bounded_output(["journalctl", "--unit=owned.service"], 0)
            self.assertEqual(result["bytes_read"], 0)
            self.assertEqual(result["raw_base64"], "")
            self.assertTrue(result["skipped"])
            for maximum in (True, -1, 1.0, n.TRACE_BUDGET+1):
                with self.subTest(maximum=maximum), self.assertRaises(ValueError):
                    n.bounded_output(["journalctl", "--unit=owned.service"], maximum)
            spawn.assert_not_called()

    def test_child_output_is_read_only_to_limit_without_shell_and_closed(self):
        child = mock.Mock(); child.stdout.fileno.return_value = 17; child.poll.return_value = 0; child.wait.return_value = 0
        args = ["journalctl", "--unit=owned.service", "--no-pager"]
        with mock.patch.object(n.subprocess, "Popen", return_value=child) as spawn, \
             mock.patch.object(n.select, "select", return_value=([child.stdout], [], [])), \
             mock.patch.object(n.os, "read", return_value=b"01234") as read:
            result = n.bounded_output(args, 5)
        spawn.assert_called_once_with(args, stdout=n.subprocess.PIPE, stderr=n.subprocess.STDOUT)
        read.assert_called_once_with(17, 5)
        self.assertEqual(base64.b64decode(result["raw_base64"]), b"01234")
        self.assertEqual(result["bytes_read"], 5)
        self.assertTrue(result["truncated"])
        self.assertEqual(result["exit"], 0)
        child.stdout.close.assert_called_once_with()

    def test_timeout_and_reap_failure_terminate_then_kill_without_unbounded_read(self):
        child = mock.Mock(); child.poll.return_value = None
        child.wait.side_effect = [n.subprocess.TimeoutExpired("journalctl", 1), 137, 137]
        with mock.patch.object(n.subprocess, "Popen", return_value=child), \
             mock.patch.object(n.select, "select", return_value=([], [], [])), \
             mock.patch.object(n.os, "read", side_effect=AssertionError("read after timeout")):
            result = n.bounded_output(["journalctl", "--unit=owned.service"], 4096)
        self.assertEqual(result["bytes_read"], 0)
        self.assertTrue(result["timed_out"])
        self.assertEqual(result["exit"], 137)
        child.terminate.assert_called_once_with()
        self.assertTrue(child.kill.called)
        child.stdout.close.assert_called_once_with()


if __name__ == "__main__":
    unittest.main()
