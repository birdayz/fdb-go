#!/usr/bin/env python3
"""Native diagnostic boundaries with fake providers/processes and temporary files."""
import base64
import builtins
import copy
import json
from pathlib import Path
import tempfile
import types
import unittest
from unittest import mock

import diagnostic as n
import driver as d
import policy as p


def plan():
    value = dict(schema="otel_native_diagnostic_v1", approved=True, campaign="native-test", prefix="native-test",
                 location="hel1", server_type="ccx43", replication="single", count=100000,
                 probe_count=1000000, probe_seconds=120, batch=32, workers=4, gomaxprocs=4,
                 source_revision="a"*40, first_epoch=1000, deadline_epoch=8200,
                 controller_cidr="192.0.2.1/32", hcloud="/private/hcloud", controller_public_key="/private/key.pub",
                 controller_private_key="/private/key", source_root="/private/source",
                 storage_options=dict(record_compression="brotli-4", posting_value_format="tuple",
                                      bulk_index_conflicts=True, hash_bytes=8))
    for key in p.ASSETS:
        value[key] = "/private/"+key
        value["input_manifest_sha256" if key == "source_manifest" else key+"_sha256"] = "b"*64
    return value


def sampler_grant(role="node1", storage_process_count=1):
    root = "/srv/otel-horizontal/native-test"
    ports = (4500, 4501, 4502, 4503, 4504, *range(4510, 4510+storage_process_count)) if role == "node1" else ()
    processes = []
    for port in ports:
        args = [root+"/bin/fdbserver", "--public-address", "10.93.0.11:"+str(port),
                "--cluster-file", root+"/arms/diagnostic/fdb.cluster", "--logdir", root+"/arms/diagnostic/logs",
                "--datadir", root+"/arms/diagnostic/data/"+str(port), "--loggroup", "native-test"]
        processes.append(dict(port=port, address="10.93.0.11:"+str(port), args=args,
                              unit_name="oh-native-test-diagnostic-"+str(port)+".service"))
    value = dict(schema="otel_native_sampler_v1", role=role, node_id=101 if role == "node1" else 102,
                token="a"*64, producer_grant_sha256="b"*64, binary_sha256="c"*64, fdbserver_sha256="d"*64,
                first_epoch=1000, deadline_epoch=8200, campaign="native-test", prefix="native-test",
                root=root, work=root+"/diagnostics/prof20k9000-"+role,
                arm_root=root+"/arms/diagnostic", binary=root+"/bin/otel-demo", run="prof20k9000",
                processes=processes)
    if storage_process_count == 2:
        value['storage_process_count'] = 2
    return value


def process(pid, start, sequence=0, args=None):
    return dict(pid=pid, starttime_ticks=start, utime_ticks=100+sequence,
                stime_ticks=50+sequence, blkio_ticks=sequence, state="S", comm="native",
                io=dict(read_bytes=100+sequence, write_bytes=200+sequence,
                        syscr=10+sequence, syscw=20+sequence, cancelled_write_bytes=0),
                threads={str(pid): dict(pid=pid, starttime_ticks=start, utime_ticks=100+sequence,
                                      stime_ticks=50+sequence, state="S", wchan="io_schedule")},
                exe=(args or ["/srv/otel-horizontal/native-test/bin/otel-demo"])[0],
                cmdline=args or ["/srv/otel-horizontal/native-test/bin/otel-demo", "--run", "prof20k9000"])


def frame(grant, sequence):
    native = {spec["unit_name"]: process(200+i, 5000+i, sequence, spec["args"])
              for i, spec in enumerate(grant["processes"])}
    kind = "fdb" if grant["role"] == "node1" else "client"
    return dict(schema="otel_native_frame_v1", node_id=grant["node_id"], role=grant["role"], run=grant["run"],
                token=grant["token"], producer_grant_sha256=grant["producer_grant_sha256"], sequence=sequence,
                phase="baseline" if sequence < 2 else "load", epoch=1000+sequence,
                monotonic_ns=(sequence+1)*1000000000, boot_id="11111111-2222-3333-4444-555555555555", clock_ticks=100,
                cgroup=dict(path="/sys/fs/cgroup/horizontal-"+kind+".slice",
                    limits={"cpu.max": "400000 100000", "cpuset.cpus.effective": "0-3", "memory.max": "17179869184",
                            "memory.swap.max": "0", "memory.current": "1000", "memory.events": "oom 0\noom_kill 0"},
                    cpu={key: 1000+sequence for key in ("usage_usec", "user_usec", "system_usec", "nr_periods",
                                                       "nr_throttled", "throttled_usec")},
                    io={"8:1": dict(rbytes=1000+sequence, wbytes=2000+sequence, rios=10+sequence, wios=20+sequence)},
                    io_raw="8:1 rbytes=1000 wbytes=2000 rios=10 wios=20\n"),
                device=dict(filesystem_device="8:1", devices=["8:1"],
                    counters={"8:1": dict(name="sda1", raw="literal-diskstats", **{key: 100+sequence for key in n.DISK_FIELDS})}),
                native=native, producer=process(900, 9000, sequence) if grant["role"] == "client1" and sequence >= 2 else None,
                pressure={key: "some avg10=0.00 avg60=0.00 avg300=0.00 total=0\n" for key in ("cpu", "memory", "io")})


def profile_fixture(storage_process_count=1):
    grants = {role: sampler_grant(role, storage_process_count) for role in ("node1", "client1")}
    frames = {role: [frame(grant, i) for i in range(12)] for role, grant in grants.items()}
    receipt = dict(started=True, pid=900, starttime_ticks=9000, node_id=grants['client1']['node_id'],
                   run=grants['client1']['run'], token=grants['client1']['token'],
                   grant_sha256=grants['client1']['producer_grant_sha256'])
    activity = [dict(storage={"node1": dict(storage_id="storage-native", input_bytes=value)}) for value in (10, 20, 30)]
    if storage_process_count == 2:
        activity = [dict(storage_processes={"10.93.0.11:"+str(port): dict(node='node1', storage_id='storage-'+str(port),
                         input_bytes=value, durable_bytes=value-1, stored_bytes=20) for port in (4510, 4511)})
                    for value in (10, 20, 30)]
    return frames, grants, receipt, activity


class DiagnosticTest(unittest.TestCase):
    def setUp(self):
        self.run_guard = mock.patch.object(n.subprocess, "run", side_effect=AssertionError("real subprocess forbidden"))
        self.popen_guard = mock.patch.object(n.subprocess, "Popen", side_effect=AssertionError("real process forbidden"))
        self.run_guard.start(); self.popen_guard.start()
        self.addCleanup(self.run_guard.stop); self.addCleanup(self.popen_guard.stop)

    def assert_inconclusive(self, result):
        self.assertEqual(result["verdict"], "INCONCLUSIVE")
        self.assertFalse(result["accepted_throughput"])
        self.assertTrue(result["reasons"])
        self.assertNotIn("ack_rows_per_second", result)
        self.assertNotIn("throughput", result)

    def test_plan_is_two_role_diagnostic_with_1_1m_row_ceiling_not_a_comparison(self):
        value = plan(); before = copy.deepcopy(value)
        self.assertIs(n.validate_plan(value, check_assets=False), value)
        self.assertEqual(value, before)
        self.assertEqual(value["count"]+value["probe_count"], 1100000)
        self.assertEqual(n.ROLES, {"node1", "client1"})
        for key in ("topologies", "selected_topologies", "prerequisite_topologies", "measured_topologies", "measured_count"):
            self.assertNotIn(key, value)
            bad = copy.deepcopy(value); bad[key] = 1000000 if key == "measured_count" else [1]
            with self.subTest(key=key), self.assertRaises(ValueError):
                n.validate_plan(bad, check_assets=False)

    def test_storage_max_outstanding_is_optional_exact_integer_64_or_128(self):
        for value in (64, 128):
            candidate = plan(); candidate['storage_max_outstanding'] = value
            before = copy.deepcopy(candidate)
            self.assertIs(n.validate_plan(candidate, check_assets=False), candidate)
            self.assertEqual(candidate, before)
        for value in (None, True, False, 64.0, 128.0, '128', 0, 63, 65, 127, 129, [], {'max_outstanding': 128}):
            candidate = plan(); candidate['storage_max_outstanding'] = value
            with self.subTest(value=value), self.assertRaisesRegex(ValueError, 'storage_max_outstanding'):
                n.validate_plan(candidate, check_assets=False)
        candidate = plan()
        n.validate_plan(candidate, check_assets=False)
        self.assertNotIn('storage_max_outstanding', candidate)

    def test_storage_process_profiles_are_exact_and_only_three_combinations_are_allowed(self):
        for count, outstanding in ((1, 64), (1, 128), (2, 64)):
            candidate = plan(); candidate.update(storage_process_count=count, storage_max_outstanding=outstanding)
            self.assertIs(n.validate_plan(candidate, check_assets=False), candidate)
        for value in (None, True, False, 1.0, 2.0, '2', 0, 3, {}, []):
            candidate = plan(); candidate['storage_process_count'] = value
            with self.subTest(value=value), self.assertRaisesRegex(ValueError, 'storage process/I/O profile'):
                n.validate_plan(candidate, check_assets=False)
        candidate = plan(); candidate.update(storage_process_count=2, storage_max_outstanding=128)
        with self.assertRaisesRegex(ValueError, 'storage process/I/O profile'):
            n.validate_plan(candidate, check_assets=False)

    def test_two_storage_profile_requires_seven_native_processes_and_both_ss_ids_exercised(self):
        frames, grants, receipt, activity = profile_fixture(2)
        for role in n.ROLES:
            n.sampler_grant(grants[role])
        self.assertEqual(len(grants['node1']['processes']), 7)
        self.assertEqual(n.profile(frames, grants, receipt, activity, True)['verdict'], 'CAPTURED')
        address = '10.93.0.11:4511'
        for failure in ('idle', 'missing', 'reset', 'durable-reset', 'unknown', 'duplicate-id', 'changed-id'):
            changed = copy.deepcopy(activity)
            if failure == 'missing':
                del changed[-1]['storage_processes'][address]
            elif failure == 'reset':
                changed[-1]['storage_processes'][address]['input_bytes'] = 0
            elif failure == 'durable-reset':
                changed[1]['storage_processes'][address]['durable_bytes'] = 0
            elif failure == 'changed-id':
                changed[-1]['storage_processes'][address]['storage_id'] = 'replacement'
            else:
                for sample in changed:
                    value = sample['storage_processes'][address]
                    if failure == 'idle': value['input_bytes'] = 10
                    elif failure == 'unknown': value['durable_bytes'] = None
                    elif failure == 'duplicate-id': value['storage_id'] = 'storage-4510'
            with self.subTest(failure=failure):
                result = n.profile(frames, grants, receipt, changed, True)
                self.assert_inconclusive(result)
                self.assertFalse(result['positive_native_activity'])
                self.assertTrue(any('not exercised' in reason for reason in result['reasons']))
        changed = copy.deepcopy(grants); changed['node1']['processes'].pop()
        with self.assertRaisesRegex(ValueError, 'population'):
            n.sampler_grant(changed['node1'])
        self.assert_inconclusive(n.profile(frames, changed, receipt, activity, True))
        for value in (None, True, 2.0, 3):
            changed = copy.deepcopy(grants); changed['node1']['storage_process_count'] = value
            with self.subTest(value=value), self.assertRaisesRegex(ValueError, 'storage process count'):
                n.sampler_grant(changed['node1'])

    def test_plan_rejects_wrong_schema_untyped_counts_and_relaxed_common_caps(self):
        for key, bad in (("schema", "otel_horizontal_v1"), ("approved", 1), ("count", True),
                         ("count", 100001), ("probe_count", 100000), ("probe_count", 1000000.0),
                         ("probe_seconds", 121), ("probe_seconds", True), ("location", "fsn1"),
                         ("server_type", "ccx33"), ("replication", "double"), ("gomaxprocs", 8),
                         ("workers", 16), ("batch", 128), ("deadline_epoch", 8201)):
            value = plan(); value[key] = bad
            with self.subTest(key=key, bad=bad), self.assertRaises(ValueError):
                n.validate_plan(value, check_assets=False)

    def test_plan_asset_projection_keeps_hash_and_symlink_guards(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp); value = plan()
            for key in p.ASSETS:
                path = root/key; path.write_text("sealed-"+key)
                value[key] = str(path)
                value["input_manifest_sha256" if key == "source_manifest" else key+"_sha256"] = p.sha(path)
            n.validate_plan(value)
            binary = Path(value["binary"]); binary.write_text("changed")
            with self.assertRaisesRegex(ValueError, "changed asset"):
                n.validate_plan(value)
            binary.write_text("sealed-binary")
            target = root/"other"; target.write_bytes(binary.read_bytes()); binary.unlink(); binary.symlink_to(target)
            with self.assertRaisesRegex(ValueError, "changed asset"):
                n.validate_plan(value)

    def test_owned_paths_never_accept_sibling_campaign_root_escape_or_relative_input(self):
        base = "/srv/otel-horizontal/native-test"
        self.assertEqual(n.owned_path("native-test", base+"/arms/diagnostic"), Path(base+"/arms/diagnostic"))
        for bad in (base, base+"/../other/arms/x", "/srv/otel-horizontal/other-test/arms/x", "relative", None, []):
            with self.subTest(path=bad), self.assertRaises(ValueError):
                n.owned_path("native-test", bad)
        with self.assertRaises(ValueError):
            n.owned_path("../escape", base+"/arms/x")

    def test_no_symlinks_refuses_leaf_parent_base_and_resolved_escape(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp); base = root/"owned"; base.mkdir(); inside = base/"inside"; inside.mkdir()
            file = inside/"data"; file.write_text("x")
            self.assertEqual(n.no_symlinks(file, base), file)
            leaf = base/"leaf"; leaf.symlink_to(file)
            parent = base/"parent"; parent.symlink_to(inside, target_is_directory=True)
            linked = root/"linked"; linked.symlink_to(base, target_is_directory=True)
            for path, owned in ((leaf, base), (parent/"data", base), (linked/"inside"/"data", linked),
                                (base/".."/"outside", base), (root/"outside", base)):
                with self.subTest(path=path), self.assertRaises(ValueError):
                    n.no_symlinks(path, owned)

    def test_sampler_grants_bind_exact_role_population_paths_hashes_and_lifetime(self):
        for role in ("node1", "client1"):
            value = sampler_grant(role)
            self.assertIs(n.sampler_grant(value), value)
            self.assertEqual(len(value["processes"]), 6 if role == "node1" else 0)
        changes = [("node_id", True), ("node_id", 0), ("role", "node2"), ("role", "client2"),
                   ("token", "bad"), ("producer_grant_sha256", None), ("binary_sha256", "bad"),
                   ("fdbserver_sha256", "bad"), ("prefix", "../foreign"),
                   ("deadline_epoch", 8201), ("run", "other-run"),
                   ("work", "/srv/otel-horizontal/native-test/arms/foreign"),
                   ("arm_root", "/srv/otel-horizontal/native-test/diagnostics/foreign"),
                   ("binary", "/srv/otel-horizontal/other-test/bin/otel-demo")]
        for key, bad in changes:
            value = sampler_grant(); value[key] = bad
            with self.subTest(key=key), self.assertRaises(ValueError):
                n.sampler_grant(value)
        for mutation in (lambda value: value["processes"].pop(),
                         lambda value: value["processes"].append(copy.deepcopy(value["processes"][0])),
                         lambda value: value["processes"][0].update(port=True),
                         lambda value: value["processes"][0].update(unit_name="foundationdb.service"),
                         lambda value: value["processes"][0]["args"].__setitem__(0, "/foreign/fdbserver"),
                         lambda value: value["processes"][0]["args"].__setitem__(
                             value["processes"][0]["args"].index("--datadir")+1, "/foreign/data")):
            value = sampler_grant(); mutation(value)
            with self.assertRaises(ValueError):
                n.sampler_grant(value)

    def test_cumulative_counter_parser_normalizes_digits_and_rejects_ambiguous_values(self):
        self.assertEqual(n.kv_counters("usage_usec 0012\nthrottled_usec 0\n"), dict(usage_usec=12, throttled_usec=0))
        for bad in ("x -1", "x 1.0", "x NaN", "x true", "x 1\nx 2", "x", "x 1 extra"):
            with self.subTest(raw=bad), self.assertRaises(ValueError):
                n.kv_counters(bad)

    def test_proc_stat_uses_starttime_ticks_and_handles_parentheses_in_comm(self):
        fields = ["S"]+["0"]*45
        for index, value in ((11, 17), (12, 19), (19, 999), (39, 7)):
            fields[index] = str(value)
        literal = "123 (worker ) with spaces) "+" ".join(fields)+"\n"
        result = n.proc_stat(literal)
        self.assertEqual((result["pid"], result["comm"], result["utime_ticks"], result["stime_ticks"],
                          result["starttime_ticks"], result["blkio_ticks"]), (123, "worker ) with spaces", 17, 19, 999, 7))
        self.assertEqual(result["raw"], literal)
        for bad in ("not a process stat", "123 (x) S 0", literal.replace("17", "-17", 1)):
            with self.subTest(raw=bad), self.assertRaises(ValueError):
                n.proc_stat(bad)

    def test_diskstats_keeps_flush_counts_normalizes_numeric_fields_and_requires_exact_devices(self):
        numbers = list(range(1, len(n.DISK_FIELDS)+1))
        literal = "8 1 sda1 "+" ".join(map(str, numbers))
        result = n.diskstats("9 9 ignored 0\n"+literal+"\n", ["8:1"])
        self.assertEqual(set(result), {"8:1"})
        for index, key in enumerate(n.DISK_FIELDS):
            self.assertEqual(result["8:1"][key], index+1)
        self.assertEqual(result["8:1"]["raw"], literal)
        for bad in ("", literal+"\n"+literal, literal.replace(" 17", " -17"), "8 1 sda1 1 2"):
            with self.subTest(raw=bad), self.assertRaises(ValueError):
                n.diskstats(bad, ["8:1"])
        legacy = "8 1 sda1 "+" ".join("1" for _ in range(11))
        self.assertIsNone(n.diskstats(legacy, ["8:1"])["8:1"]["flush_ios"])

    def test_profile_positive_control_is_captured_never_capacity_pass_or_ack_rate(self):
        frames, grants, receipt, activity = profile_fixture()
        result = n.profile(frames, grants, receipt, activity, True)
        self.assertEqual(result["verdict"], "CAPTURED")
        self.assertTrue(result["producer_process_bound"])
        self.assertTrue(result["positive_native_activity"])
        self.assertEqual(result["populations"], {role: dict(baseline_frames=2, load_frames=10) for role in n.ROLES})
        self.assertFalse(result["accepted_throughput"])
        self.assertNotIn("ack_rows_per_second", result)
        self.assertIn("observational", result["boundary"])
        self.assertIn("not physical I/O", result["boundary"])
        self.assertEqual(n.frame_errors(frames["node1"][0], grants["node1"]), [])

    def test_profile_requires_nonempty_two_role_baselines_and_at_least_ten_load_frames(self):
        frames, grants, receipt, activity = profile_fixture()
        self.assert_inconclusive(n.profile({}, grants, receipt, activity, True))
        self.assert_inconclusive(n.profile(frames, {}, receipt, activity, True))
        for role in n.ROLES:
            for index in (0, 11):
                changed = copy.deepcopy(frames); changed[role].pop(index)
                self.assert_inconclusive(n.profile(changed, grants, receipt, activity, True))
            changed = copy.deepcopy(frames); del changed[role]
            self.assert_inconclusive(n.profile(changed, grants, receipt, activity, True))
            changed_grants = dict(grants); del changed_grants[role]
            self.assert_inconclusive(n.profile(frames, changed_grants, receipt, activity, True))

    def test_profile_rejects_frame_identity_boot_sequence_clock_and_caps_changes(self):
        frames, grants, receipt, activity = profile_fixture()
        for role in n.ROLES:
            for key, bad in (("schema", "foreign"), ("node_id", 999), ("role", "foreign"), ("token", "f"*64),
                             ("run", "prof20k9999"), ("producer_grant_sha256", "e"*64),
                             ("boot_id", "changed-boot"), ("sequence", 99), ("monotonic_ns", 1), ("epoch", float("nan"))):
                changed = copy.deepcopy(frames); changed[role][5][key] = bad
                with self.subTest(role=role, key=key):
                    self.assert_inconclusive(n.profile(changed, grants, receipt, activity, True))
            for key, bad in (("cpu.max", "max 100000"), ("cpuset.cpus.effective", "0-15"),
                             ("memory.max", "max"), ("memory.swap.max", "max")):
                changed = copy.deepcopy(frames); changed[role][5]["cgroup"]["limits"][key] = bad
                self.assert_inconclusive(n.profile(changed, grants, receipt, activity, True))

    def test_profile_rejects_cpu_native_and_disk_resets_or_missing_data(self):
        frames, grants, receipt, activity = profile_fixture()
        for role in n.ROLES:
            for key in ("usage_usec", "nr_throttled", "throttled_usec"):
                changed = copy.deepcopy(frames); changed[role][5]["cgroup"]["cpu"][key] = 0
                self.assert_inconclusive(n.profile(changed, grants, receipt, activity, True))
            for key in ("read_ios", "write_sectors", "flush_ios", "flush_ms"):
                changed = copy.deepcopy(frames); changed[role][5]["device"]["counters"]["8:1"][key] = 0
                self.assert_inconclusive(n.profile(changed, grants, receipt, activity, True))
        unit = grants["node1"]["processes"][0]["unit_name"]
        for key, bad in (("pid", 999), ("starttime_ticks", 999), ("utime_ticks", 0), ("stime_ticks", 0)):
            changed = copy.deepcopy(frames); changed["node1"][5]["native"][unit][key] = bad
            self.assert_inconclusive(n.profile(changed, grants, receipt, activity, True))
        for key in ("cgroup", "device", "native", "pressure"):
            changed = copy.deepcopy(frames); del changed["node1"][5][key]
            self.assert_inconclusive(n.profile(changed, grants, receipt, activity, True))

    def test_profile_rejects_malformed_rows_cgroup_device_native_io_and_thread_changes(self):
        frames, grants, receipt, activity = profile_fixture()
        for role in n.ROLES:
            changed = copy.deepcopy(frames); changed[role][5] = None
            self.assert_inconclusive(n.profile(changed, grants, receipt, activity, True))
            changed = copy.deepcopy(frames); changed[role][5]['cgroup']['path'] = '/foreign/slice'
            self.assert_inconclusive(n.profile(changed, grants, receipt, activity, True))
            changed = copy.deepcopy(frames); device = changed[role][5]['device']
            device['devices'] = ['8:2']; device['counters'] = {'8:2': device['counters']['8:1']}
            self.assert_inconclusive(n.profile(changed, grants, receipt, activity, True))
            changed = copy.deepcopy(frames); changed[role][5]['cgroup']['io']['8:1']['wbytes'] = 0
            self.assert_inconclusive(n.profile(changed, grants, receipt, activity, True))
        unit = grants['node1']['processes'][0]['unit_name']
        for field in ('read_bytes', 'write_bytes'):
            changed = copy.deepcopy(frames); changed['node1'][5]['native'][unit]['io'][field] = 0
            self.assert_inconclusive(n.profile(changed, grants, receipt, activity, True))
        for field in ('starttime_ticks', 'utime_ticks', 'stime_ticks'):
            changed = copy.deepcopy(frames); snapshot = changed['node1'][5]['native'][unit]
            snapshot['threads'][str(snapshot['pid'])][field] = 1
            self.assert_inconclusive(n.profile(changed, grants, receipt, activity, True))
        changed = copy.deepcopy(frames)
        for role in n.ROLES:
            changed[role][5]['device']['counters']['8:1']['in_flight'] = 0
        self.assertEqual(n.profile(changed, grants, receipt, activity, True)['verdict'], 'CAPTURED')

    def test_profile_requires_ready_release_and_actual_matching_producer_pid_starttime(self):
        frames, grants, receipt, activity = profile_fixture()
        self.assert_inconclusive(n.profile(frames, grants, receipt, activity, False))
        for bad in (None, {}, dict(receipt, started=False), dict(receipt, pid=True),
                    dict(receipt, pid=999), dict(receipt, starttime_ticks=999), dict(receipt, node_id=999),
                    dict(receipt, run='prof20k9999'), dict(receipt, token='f'*64), dict(receipt, grant_sha256='e'*64)):
            self.assert_inconclusive(n.profile(frames, grants, bad, activity, True))
        changed = copy.deepcopy(frames)
        for value in changed["client1"]:
            value["producer"] = None
        self.assert_inconclusive(n.profile(changed, grants, receipt, activity, True))

    def test_profile_native_activity_needs_stable_nonempty_identity_monotone_positive_input(self):
        frames, grants, receipt, activity = profile_fixture()
        for bad in ([], activity[:1],
                    [dict(storage={"node1": dict(storage_id="", input_bytes=x)}) for x in (10, 20)],
                    [dict(storage={"node1": dict(storage_id="id", input_bytes=x)}) for x in (10, 10)],
                    [dict(storage={"node1": dict(storage_id="id", input_bytes=x)}) for x in (10, 0, 20)]):
            self.assert_inconclusive(n.profile(frames, grants, receipt, bad, True))
        for key, bad in (("storage_id", "changed"), ("storage_id", None), ("input_bytes", None),
                         ("input_bytes", True), ("input_bytes", "20"), ("input_bytes", 20.0)):
            changed = copy.deepcopy(activity); changed[1]["storage"]["node1"][key] = bad
            self.assert_inconclusive(n.profile(frames, grants, receipt, changed, True))
        normalized = [dict(storage={"node1": dict(storage_id="stable", input_bytes=p.storage_counter(
                      dict(input_bytes=dict(counter=x)), "input_bytes"))}) for x in (10.0, 20.0)]
        self.assertEqual(n.profile(frames, grants, receipt, normalized, True)["verdict"], "CAPTURED")

    def trace_fixture(self, root):
        arm = Path(root)/'arm'; logs = arm/'logs'; logs.mkdir(parents=True)
        grant = sampler_grant()
        bindings = {spec['unit_name']: dict(pid=4000+spec['port'], starttime_ticks=7000+spec['port'],
                    address=spec['address'], port=spec['port'], args=spec['args'])
                    for spec in grant['processes']}
        paths = {}
        for port in (4504, 4510):
            paths[port] = logs/('trace.10.93.0.11.'+str(port)+'.1790000000.Ab12CD.1.1.json')
        return arm, bindings, paths

    def test_two_storage_trace_collection_covers_all_three_targets_under_one_budget(self):
        with tempfile.TemporaryDirectory() as tmp:
            arm, bindings, paths = self.trace_fixture(tmp)
            extra = sampler_grant(storage_process_count=2)['processes'][-1]
            bindings[extra['unit_name']] = dict(pid=999, starttime_ticks=9999, args=extra['args'],
                                                address=extra['address'], port=4511)
            paths[4511] = arm/'logs'/'trace.10.93.0.11.4511.1790000000.Ab12CD.1.1.json'
            for port, path in paths.items():
                line = json.dumps(dict(Type='AllEventTypes', Machine='10.93.0.11:'+str(port), LogGroup='native-test', Data='x'*80))+'\n'
                path.write_text(line*20)
            result = n.trace_tail(arm, 'native-test', bindings, budget=1536, storage_process_count=2)
            self.assertEqual(result['bytes_read'], 1536)
            self.assertEqual(result['coverage'], 'CAPTURED')
            self.assertEqual({x['address'] for x in result['files']}, {'10.93.0.11:'+str(p) for p in (4504, 4510, 4511)})
            self.assertTrue(all(x['complete_records'] > 0 and 'AllEventTypes' in x['complete_ndjson'] for x in result['files']))
            paths[4511].unlink()
            self.assertEqual(n.trace_tail(arm, 'native-test', bindings, storage_process_count=2)['coverage'], 'INCONCLUSIVE')
            paths[4511].symlink_to(paths[4510])
            with self.assertRaisesRegex(ValueError, 'symlink'):
                n.trace_tail(arm, 'native-test', bindings, storage_process_count=2)
            del bindings[extra['unit_name']]
            with self.assertRaisesRegex(ValueError, 'targets'):
                n.trace_tail(arm, 'native-test', bindings, storage_process_count=2)

    def test_literal_trace_tail_retains_all_event_types_and_maps_verified_process_not_threadid(self):
        with tempfile.TemporaryDirectory() as tmp:
            arm, bindings, paths = self.trace_fixture(tmp)
            expected = {}
            for port, path in paths.items():
                events = [dict(Type=kind, Machine='10.93.0.11:'+str(port), LogGroup='native-test',
                               ThreadID='not-a-process-pid', Severity=severity, Counter=123)
                          for kind, severity in (('StorageMetrics', '10'), ('IOStats', '10'),
                                                 ('UnregisteredNativeEvent', '20'), ('SevereEvent', '40'))]
                raw = ''.join(json.dumps(event)+'\n' for event in events)
                path.write_text(raw); expected[path.name] = (raw, events)
            result = n.trace_tail(str(arm), 'native-test', bindings)
            self.assertEqual(result['coverage'], 'CAPTURED')
            self.assertEqual(len(result['files']), 2)
            self.assertEqual(result['bytes_read'], sum(len(raw.encode()) for raw, _ in expected.values()))
            self.assertIn('not trace ThreadID', result['boundary'])
            for file in result['files']:
                raw, events = expected[file['name']]
                self.assertEqual(base64.b64decode(file['raw_base64']), raw.encode())
                self.assertEqual([json.loads(line) for line in file['complete_ndjson'].splitlines()], events)
                bound = next(value for value in bindings.values() if value['address'] == file['address'])
                self.assertEqual((file['pid'], file['starttime_ticks'], file['args']),
                                 (bound['pid'], bound['starttime_ticks'], bound['args']))
                self.assertNotEqual(str(file['pid']), events[0]['ThreadID'])

    def test_trace_budget_is_aggregate_bounded_and_missing_target_coverage_is_inconclusive(self):
        with tempfile.TemporaryDirectory() as tmp:
            arm, bindings, paths = self.trace_fixture(tmp)
            for port, path in paths.items():
                line = json.dumps(dict(Type='AllTypes', Machine='10.93.0.11:'+str(port),
                                       LogGroup='native-test', Payload='x'*200))+'\n'
                path.write_text(line*20)
            result = n.trace_tail(str(arm), 'native-test', bindings, budget=1024)
            self.assertEqual(result['bytes_read'], 1024)
            self.assertLessEqual(sum(file['bytes_read'] for file in result['files']), 1024)
            self.assertEqual(result['coverage'], 'CAPTURED')
            self.assertEqual({file['address'] for file in result['files']}, {'10.93.0.11:4504', '10.93.0.11:4510'})
            self.assertEqual(n.trace_tail(str(arm), 'native-test', bindings, budget=8)['coverage'], 'INCONCLUSIVE')
            self.assertEqual(n.trace_tail(str(arm), 'native-test', bindings, budget=0)['bytes_read'], 0)
            for bad in (True, -1, 1.0, n.TRACE_BUDGET+1):
                with self.subTest(budget=bad), self.assertRaises(ValueError):
                    n.trace_tail(str(arm), 'native-test', bindings, budget=bad)
            paths[4510].unlink()
            partial = n.trace_tail(str(arm), 'native-test', bindings)
            self.assertEqual(partial['coverage'], 'INCONCLUSIVE')

    def test_trace_selects_latest_observed_file_for_each_exact_storage_and_log_address(self):
        with tempfile.TemporaryDirectory() as tmp:
            arm, bindings, paths = self.trace_fixture(tmp)
            for port, path in paths.items():
                path.write_text(json.dumps(dict(Type='Original', Machine='10.93.0.11:'+str(port), LogGroup='native-test'))+'\n')
            old = arm/'logs'/'trace.10.93.0.11.4510.1789999999.Zz9999.1.99.json'
            old.write_text('older-file-must-not-be-selected\n')
            newest = arm/'logs'/'trace.10.93.0.11.4510.1790000001.Cd34EF.1.2.json'
            newest.write_text(json.dumps(dict(Type='Newest', Machine='10.93.0.11:4510', LogGroup='native-test'))+'\n')
            result = n.trace_tail(str(arm), 'native-test', bindings)
            self.assertEqual(result['coverage'], 'CAPTURED')
            self.assertEqual({file['name'] for file in result['files']}, {newest.name, paths[4504].name})
            self.assertIn('latest observed', result['selection'])

    def test_trace_tail_refuses_machine_loggroup_malformed_json_and_selected_symlinks(self):
        for field, bad in (('Machine', '10.93.0.12:4510'), ('Machine', None), ('LogGroup', 'other-test')):
            with self.subTest(field=field), tempfile.TemporaryDirectory() as tmp:
                arm, bindings, paths = self.trace_fixture(tmp)
                event = dict(Type='AnyEvent', Machine='10.93.0.11:4510', LogGroup='native-test'); event[field] = bad
                paths[4510].write_text(json.dumps(event)+'\n')
                with self.assertRaises(ValueError):
                    n.trace_tail(str(arm), 'native-test', bindings)
        for raw in ('not-json\n', '[]\n', '{"Machine":"a","Machine":"b"}\n'):
            with tempfile.TemporaryDirectory() as tmp:
                arm, bindings, paths = self.trace_fixture(tmp); paths[4510].write_text(raw)
                with self.assertRaises(ValueError):
                    n.trace_tail(str(arm), 'native-test', bindings)
        with tempfile.TemporaryDirectory() as tmp:
            arm, bindings, paths = self.trace_fixture(tmp)
            outside = Path(tmp)/'outside'; outside.write_text('{}\n'); paths[4510].symlink_to(outside)
            with self.assertRaisesRegex(ValueError, 'symlink'):
                n.trace_tail(str(arm), 'native-test', bindings)
        with tempfile.TemporaryDirectory() as tmp:
            arm, bindings, paths = self.trace_fixture(tmp)
            (arm/'logs').rename(arm/'real-logs'); (arm/'logs').symlink_to(arm/'real-logs', target_is_directory=True)
            with self.assertRaisesRegex(ValueError, 'symlink'):
                n.trace_tail(str(arm), 'native-test', bindings)

    def test_process_snapshot_and_native_bindings_use_exact_pid_starttime_cmdline_and_io(self):
        grant = sampler_grant()
        grant['processes'][-1]['args'] += ['--knob_max_outstanding', '128']
        snapshots = {200+i: process(200+i, 5000+i, args=spec['args']) for i, spec in enumerate(grant['processes'])}
        def command(args, timeout=5):
            unit = args[2]
            return str(200+next(i for i, spec in enumerate(grant['processes']) if spec['unit_name'] == unit))
        with mock.patch.object(n, 'command', side_effect=command), \
             mock.patch.object(p, 'sha', return_value=grant['fdbserver_sha256']), \
             mock.patch.object(n, 'process_snapshot', side_effect=lambda pid: copy.deepcopy(snapshots[pid])):
            bindings = n.native_bindings(grant)
            self.assertEqual(len(bindings), 6)
            for spec in grant['processes']:
                self.assertEqual(bindings[spec['unit_name']]['args'], spec['args'])
            snapshots[205]['cmdline'] = [*grant['processes'][-1]['args'][:-1], '64']
            with self.assertRaisesRegex(ValueError, 'command changed'):
                n.native_bindings(grant)
            snapshots[205]['cmdline'] = grant['processes'][-1]['args']
            snapshots[200]['cmdline'] = ['/foreign/bin/fdbserver']
            with self.assertRaisesRegex(ValueError, 'command changed'):
                n.native_bindings(grant)
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp); proc = root/'123'; task = proc/'task'/'123'; task.mkdir(parents=True)
            fields = ['S']+['0']*45; fields[19] = '999'; stat = '123 (worker) '+' '.join(fields)+'\n'
            (proc/'stat').write_text(stat); (task/'stat').write_text(stat); (task/'wchan').write_text('io_schedule\n')
            (proc/'io').write_text('read_bytes: 10\nwrite_bytes: 20\ncancelled_write_bytes: -1\n')
            (proc/'cmdline').write_bytes(b'/sealed/fdbserver\0--class\0storage\0')
            (proc/'exe').symlink_to('/sealed/fdbserver')
            result = n.process_snapshot(123, proc_root=root)
            self.assertEqual(result['starttime_ticks'], 999)
            self.assertEqual(result['io']['cancelled_write_bytes'], -1)
            self.assertEqual(result['threads']['123']['wchan'], 'io_schedule')
            self.assertEqual(result['cmdline'], ['/sealed/fdbserver', '--class', 'storage'])
            (proc/'io').write_text('read_bytes: -1\n')
            with self.assertRaisesRegex(ValueError, 'negative process I/O'):
                n.process_snapshot(123, proc_root=root)

    def test_sample_frame_checks_fixed_slice_caps_and_records_cpu_flush_counters(self):
        grant = sampler_grant()
        bindings = {spec['unit_name']: dict(pid=200+i, starttime_ticks=5000+i, args=spec['args'])
                    for i, spec in enumerate(grant['processes'])}
        by_pid = {200+i: process(200+i, 5000+i, args=spec['args']) for i, spec in enumerate(grant['processes'])}
        values = {'cpu.max': '400000 100000', 'cpuset.cpus.effective': '0-3', 'memory.max': '17179869184',
                  'memory.swap.max': '0', 'memory.current': '1000', 'memory.events': 'oom 0\noom_kill 0',
                  'cpu.stat': '\n'.join(key+' 100' for key in ('usage_usec', 'user_usec', 'system_usec', 'nr_periods', 'nr_throttled', 'throttled_usec')),
                  'io.stat': '8:1 rbytes=10 wbytes=20 rios=1 wios=2\n',
                  'diskstats': '8 1 sda1 '+' '.join('100' for _ in n.DISK_FIELDS), 'boot_id': 'fixed-boot'}
        def read(path, limit=65536):
            path = Path(path)
            if path.parent == Path('/proc/pressure'):
                return 'some avg10=0.00 avg60=0.00 avg300=0.00 total=0\n'
            return values[path.name]
        with mock.patch.object(n, 'command', return_value='/horizontal-fdb.slice'), \
             mock.patch.object(n, 'process_snapshot', side_effect=lambda pid: copy.deepcopy(by_pid[pid])), \
             mock.patch.object(n, 'read_small', side_effect=read), mock.patch.object(n.os, 'sysconf', return_value=100):
            result = n.sample_frame(grant, bindings, dict(filesystem_device='8:1', devices=['8:1']), 0, 'baseline')
            self.assertEqual(result['cgroup']['cpu']['nr_throttled'], 100)
            self.assertEqual(result['device']['counters']['8:1']['flush_ios'], 100)
            self.assertEqual(n.frame_errors(result, grant), [])
            for key, bad in (('cpu.max', 'max 100000'), ('cpuset.cpus.effective', '0-15'),
                             ('memory.max', 'max'), ('memory.swap.max', 'max')):
                old = values[key]; values[key] = bad
                with self.subTest(key=key), self.assertRaises((ValueError, TypeError)):
                    n.sample_frame(grant, bindings, dict(filesystem_device='8:1', devices=['8:1']), 0, 'baseline')
                values[key] = old

    def test_sample_frame_literal_missing_io_stat_fails_closed(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            values = {'cpu.max': '400000 100000', 'cpuset.cpus.effective': '0-3', 'memory.max': '17179869184',
                      'memory.swap.max': '0', 'memory.current': '1000', 'memory.events': 'oom 0\noom_kill 0',
                      'cpu.stat': '\n'.join(key+' 100' for key in ('usage_usec', 'user_usec', 'system_usec',
                                                                  'nr_periods', 'nr_throttled', 'throttled_usec'))}
            for name, value in values.items():
                (root/name).write_text(value)
            read = n.read_small
            with mock.patch.object(n, 'command', return_value='/horizontal.slice/horizontal-fdb.slice'), \
                 mock.patch.object(n, 'read_small', side_effect=lambda path, limit=65536: read(root/Path(path).name, limit)):
                with self.assertRaises(FileNotFoundError) as failure:
                    n.sample_frame(sampler_grant(), {}, dict(filesystem_device='8:1', devices=['8:1']), 0, 'baseline')
            self.assertEqual(failure.exception.filename, str(root/'io.stat'))

    def test_install_helpers_requires_owned_io_accounting_and_readable_stat_before_load(self):
        for failure in (None, 'disabled', 'foreign', 'missing'):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as tmp:
                provider = mock.Mock(); provider.copy_to_node.return_value = (0, '', '')
                campaign = n.Diagnostic(plan(), tmp, provider)
                campaign.nodes = {'node1': dict(id=101), 'client1': dict(id=102)}
                campaign.remote_harness = '/srv/otel-horizontal/native-test/harness'
                def run_on_node(plan, out, node, args, timeout):
                    if args[0] == 'sha256sum':
                        return 0, p.sha(n.HERE/'diagnostic.py')+'  diagnostic.py\n', ''
                    if '--property=IOAccounting' in args:
                        return 0, 'no\n' if failure == 'disabled' and node['id'] == 102 else 'yes\n', ''
                    if '--property=ControlGroup' in args:
                        group = '/foreign.slice' if failure == 'foreign' and node['id'] == 102 else '/horizontal.slice/'+args[2]
                        return 0, group+'\n', ''
                    if args[0] == 'cat':
                        if failure == 'missing' and node['id'] == 102:
                            return 1, '', 'cat: '+args[1]+': No such file or directory\n'
                        return 0, '', ''  # A readable accounting file may be empty before any I/O.
                    raise AssertionError(args)
                provider.run_on_node.side_effect = run_on_node
                with mock.patch.object(d.Campaign, 'install_helpers'), mock.patch.object(d, 'remaining', return_value=30):
                    if failure:
                        with self.assertRaises(ValueError):
                            campaign.install_helpers()
                        self.assertFalse((Path(tmp)/'client1-io-accounting.json').exists())
                    else:
                        campaign.install_helpers()
                        for role, kind in (('node1', 'fdb'), ('client1', 'client')):
                            unit = 'horizontal-'+kind+'.slice'
                            self.assertEqual(d.read(Path(tmp)/(role+'-io-accounting.json')),
                                             dict(node_id=campaign.nodes[role]['id'], role=role, unit=unit, IOAccounting='yes',
                                                  io_stat_path='/sys/fs/cgroup/horizontal.slice/'+unit+'/io.stat', io_stat_raw=''))
                self.assertEqual(provider.copy_to_node.call_count, 2)

    def test_sampler_frame_budget_is_checked_before_writing_payload(self):
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp); grant = sampler_grant(); grant['work'] = str(work)
            path = work/'plan.json'; path.write_text(json.dumps(grant))
            with mock.patch.object(n, 'sampler_grant', return_value=grant), mock.patch.object(d, 'identity'), \
                 mock.patch.object(n, 'no_symlinks', return_value=work), \
                 mock.patch.object(n, 'native_bindings', return_value={}), \
                 mock.patch.object(n, 'device_identity', return_value=dict(filesystem_device='8:1', devices=['8:1'])), \
                 mock.patch.object(n, 'sample_frame', return_value=frame(grant, 0)), \
                 mock.patch.object(n, 'FRAME_BUDGET', 128), mock.patch.object(n.time, 'time', return_value=1001), \
                 mock.patch.object(n.time, 'monotonic', return_value=0), mock.patch.object(n.time, 'sleep'):
                with self.assertRaisesRegex(ValueError, 'byte budget'):
                    n.sample(path)
            self.assertTrue((work/'identity.json').is_file())
            self.assertEqual((work/'frames.jsonl').stat().st_size, 0)
            self.assertEqual((work/'frames.jsonl').stat().st_mode & 0o777, 0o600)

    def diagnostic_controller(self, root):
        provider = mock.Mock(); provider.prepare.return_value = dict(nodes={'node1': dict(id=101), 'client1': dict(id=102)})
        provider.setup_cluster.return_value = dict(storage_hosts=['node1'], root='/owned/arm')
        provider.teardown.return_value = dict(passed=True)
        campaign = n.Diagnostic(plan(), Path(root)/'campaign', provider)
        campaign.install_helpers = mock.Mock(); campaign.quiet = mock.Mock(); campaign.stable = mock.Mock()
        campaign.host_samples = mock.Mock(return_value={'node1': dict(fdb_pids={'200': '5000'}), 'client1': dict(fdb_pids={})})
        return campaign, provider

    def test_controller_valid_100k_gate_precedes_single_1m_probe_and_never_accepts_ack_rate(self):
        with tempfile.TemporaryDirectory() as tmp:
            campaign, provider = self.diagnostic_controller(tmp)
            campaign.plan['storage_max_outstanding'] = 128
            gate = dict(count=100000, producers=1, healthy=True, ingest_valid=True, census_pass=True, oracle_pass=True)
            campaign.wave = mock.Mock(side_effect=[gate, dict(diagnostic_only=True, accepted_throughput=False)])
            with mock.patch.object(n, 'validate_plan'), mock.patch.object(p, 'verify_source', return_value=dict(files=1)), \
                 mock.patch.object(d, 'remaining', return_value=30):
                campaign.run()
            self.assertEqual([(call.args[1], call.args[2]) for call in campaign.wave.call_args_list],
                             [(100000, 'prerequisite'), (1000000, 'probe')])
            self.assertEqual(provider.setup_cluster.call_count, 1)
            self.assertEqual(provider.setup_cluster.call_args.args[3:], (1, 'diagnostic'))
            self.assertEqual(provider.teardown.call_count, 1)
            result = d.read(Path(tmp)/'campaign'/'diagnostic-result.json')
            self.assertEqual(result['authorized_rows'], 1100000)
            self.assertEqual(result['producers'], 1)
            self.assertEqual(result['storage_max_outstanding'], 128)
            approved = Path(tmp)/'campaign'/'approved-plan.json'
            self.assertEqual(d.read(approved), campaign.plan)
            self.assertEqual(result['approved_plan_sha256'], p.sha(approved))
            self.assertTrue(result['diagnostic_only'])
            self.assertFalse(result['accepted_throughput'])
            self.assertNotIn('ack_rows_per_second', result)

    def test_controller_failed_missing_or_untyped_100k_gate_never_runs_probe_and_always_cleans(self):
        gate = dict(count=100000, producers=1, healthy=True, ingest_valid=True, census_pass=True, oracle_pass=True)
        bad_gates = [None, {}, dict(gate, count=99999), dict(gate, count=100000.0), dict(gate, producers=True),
                     dict(gate, producers=2), dict(gate, healthy=False), dict(gate, ingest_valid=False),
                     dict(gate, census_pass=False), dict(gate, oracle_pass=False)]
        for bad in bad_gates:
            with self.subTest(gate=bad), tempfile.TemporaryDirectory() as tmp:
                campaign, provider = self.diagnostic_controller(tmp)
                campaign.wave = mock.Mock(return_value=bad)
                with mock.patch.object(n, 'validate_plan'), mock.patch.object(p, 'verify_source', return_value=dict(files=1)), \
                     mock.patch.object(d, 'remaining', return_value=30):
                    with self.assertRaisesRegex(ValueError, '100k'):
                        campaign.run()
                self.assertEqual(campaign.wave.call_count, 1)
                self.assertEqual(provider.teardown.call_count, 1)
                self.assertFalse(d.read(Path(tmp)/'campaign'/'diagnostic-result.json')['accepted_throughput'])

    def test_controller_strict_failure_is_fatal_and_cleans_without_reclassifying_as_throughput(self):
        with tempfile.TemporaryDirectory() as tmp:
            campaign, provider = self.diagnostic_controller(tmp)
            campaign.wave = mock.Mock(side_effect=ValueError('strict health failure'))
            with mock.patch.object(n, 'validate_plan'), mock.patch.object(p, 'verify_source', return_value=dict(files=1)), \
                 mock.patch.object(d, 'remaining', return_value=30):
                with self.assertRaisesRegex(ValueError, 'strict health'):
                    campaign.run()
            self.assertEqual(campaign.wave.call_count, 1)
            self.assertEqual(provider.teardown.call_count, 1)
            result = d.read(Path(tmp)/'campaign'/'diagnostic-result.json')
            self.assertIn('strict health failure', result['error'])
            self.assertFalse(result['accepted_throughput'])

    def test_source_after_is_verified_and_written_even_when_cleanup_fails_or_raises(self):
        for failure in ('false', 'exception'):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as tmp:
                campaign, provider = self.diagnostic_controller(tmp)
                campaign.wave = mock.Mock(side_effect=ValueError('strict health failure'))
                if failure == 'false': provider.teardown.return_value = dict(passed=False, external_ids_missing=[999])
                else: provider.teardown.side_effect = OSError('cleanup command failed')
                with mock.patch.object(n, 'validate_plan'), mock.patch.object(d, 'remaining', return_value=30), \
                     mock.patch.object(p, 'verify_source', side_effect=[dict(files=1), dict(files=1)]) as verify:
                    with self.assertRaisesRegex((ValueError, OSError), 'cleanup'):
                        campaign.run()
                self.assertEqual(verify.call_count, 2)
                self.assertEqual(d.read(Path(tmp)/'campaign'/'source-after.json'), dict(files=1))
                self.assertEqual(d.read(Path(tmp)/'campaign'/'diagnostic-result.json')['error'], 'strict health failure')
                if failure == 'false':
                    self.assertFalse(d.read(Path(tmp)/'campaign'/'cleanup.json')['passed'])

    def test_remote_cli_readonly_validation_does_not_import_provider_or_start_controller(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp)/'plan.json'; path.write_text(json.dumps(plan()))
            original_import = builtins.__import__
            def guarded_import(name, *args, **kwargs):
                if name == 'provision':
                    raise AssertionError('readonly CLI imported provider')
                return original_import(name, *args, **kwargs)
            with mock.patch('builtins.__import__', guarded_import), mock.patch.object(n, 'validate_plan', return_value=plan()), \
                 mock.patch.object(n, 'Diagnostic', side_effect=AssertionError('controller started')), mock.patch('builtins.print') as output:
                n.main(['run', '--plan', str(path)])
            value = json.loads(output.call_args.args[0])
            self.assertEqual(value['nodes'], 2)
            self.assertEqual(value['rows'], 1100000)
            self.assertFalse(value['provisioned'])
            self.assertFalse(value['accepted_throughput'])

    def test_minimal_remote_harness_imports_only_diagnostic_driver_policy_without_provider(self):
        # Execute imports from three temporary copies under an import guard;
        # no child Python, provider import, __main__, bytecode, or host command.
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for name in ("diagnostic", "driver", "policy"):
                (root/(name+".py")).write_bytes((Path(n.__file__).parent/(name+".py")).read_bytes())
            modules = {name: types.ModuleType(name) for name in ("diagnostic", "driver", "policy")}
            loaded = set(); original_import = builtins.__import__

            def isolated_import(name, globals=None, locals=None, fromlist=(), level=0):
                if name == "provision" or name.startswith("provision."):
                    raise AssertionError("remote harness imported provider")
                if name in modules:
                    if name not in loaded:
                        loaded.add(name)
                        module = modules[name]; module.__file__ = str(root/(name+".py"))
                        exec(compile(Path(module.__file__).read_text(), module.__file__, "exec"), module.__dict__)
                    return modules[name]
                return original_import(name, globals, locals, fromlist, level)

            with mock.patch("builtins.__import__", isolated_import):
                module = isolated_import("diagnostic")
            self.assertEqual(loaded, {"diagnostic", "driver", "policy"})
            self.assertTrue(callable(module.profile))
            self.assertEqual(set(path.name for path in root.iterdir()), {"diagnostic.py", "driver.py", "policy.py"})


if __name__ == "__main__":
    unittest.main()
