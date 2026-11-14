"""Offline tests of the Kubernetes benchmark runner: no provider, no ssh, no network.

Everything that would touch the provider goes through a fake `hcloud` (fake_hcloud.py) or --dry-run.
"""
import contextlib
import io
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time
import unittest
from decimal import Decimal
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import condition  # noqa: E402
import leanrun as L  # noqa: E402

PRICING = {"pricing": {"currency": "EUR", "vat_rate": "19.000000",
                       "primary_ips": [{"type": "ipv4", "prices": [{"location": "hel1", "price_hourly": {"gross": "0.001000"}}]}],
                       "server_types": [{"name": "ccx43", "prices": [{"location": "hel1", "price_hourly": {"gross": "0.400000"}}]},
                                        {"name": "cpx62", "prices": [{"location": "hel1", "price_hourly": {"gross": "0.200000"}}]}]}}


def base_cfg(tmp, **extra):
    pricing = Path(tmp) / "pricing.json"
    pricing.write_text(json.dumps(PRICING))
    cfg = {"campaign": "lean-unit", "mode": "measured", "location": "hel1", "image": "ubuntu-24.04", "node_type": "ccx43", "client_type": "cpx62",
           "hcloud": str(HERE / "fake_hcloud.py"), "deadline_minutes": 110, "soft_cutoff_minutes": 95, "cleanup_margin_minutes": 10,
           "builds": {"control_pure_binary": {"sha256": "a" * 64, "executable_relative_path": "control-pure", "deployment_tar": "/x/a.tar", "deployment_sha256": "b" * 64},
                      "control_native_binary": {"sha256": "c" * 64, "executable_relative_path": "control-native", "client_library_relative_path": "lib/libfdb_c.so",
                                                "deployment_tar": "/x/c.tar", "deployment_sha256": "d" * 64},
                      "rust_binary": {"sha256": "e" * 64, "executable_relative_path": "otel-rust", "deployment_tar": "/x/e.tar", "deployment_sha256": "f" * 64}},
           "assets": {"libfdb_c": {"path": "/x/libfdb_c.so", "sha256": "7" * 64}, "fdbcli": {"path": "/x/fdbcli", "sha256": "8" * 64},
                      "corpus_1000": {"path": "/x/corpus-1000.bin", "sha256": "9" * 64}}, "warm_count": 0, "measured_count": 1000, "k8s_profile": "full", "source_revision": "0" * 40,
           "source_manifest_sha256": "1" * 64, "pricing_file": str(pricing), "controller_cidr": "192.0.2.1/32", "cap_eur": 6.0,
           "roles": ["node1", "node2", "node3", "client1", "client2", "client3"]}
    cfg.update(extra)
    return cfg


class StateDirCase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.old_env = os.environ.get("LEAN_STATE_DIR")
        os.environ["LEAN_STATE_DIR"] = str(Path(self.tmp) / "state")
        self.saved_ip = dict(L.PRIVATE_IP)

    def tearDown(self):
        if self.old_env is None:
            os.environ.pop("LEAN_STATE_DIR", None)
        else:
            os.environ["LEAN_STATE_DIR"] = self.old_env
        L.PRIVATE_IP.clear()
        L.PRIVATE_IP.update(self.saved_ip)
        shutil.rmtree(self.tmp, ignore_errors=True)


class ManifestTests(unittest.TestCase):
    def test_selftest_goldens(self):
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(L.cmd_selftest(None, None), 0)

    def test_baseline_layout_is_the_default(self):
        self.assertEqual(L.render_fdb_cluster_yaml("x", 3, "full"), L.render_fdb_cluster_yaml("x", 3, "full", "A"))

    def test_layout_b_has_12_storage_processes_and_the_same_roles(self):
        a, b = L.render_fdb_cluster_yaml("x", 3, "full"), L.render_fdb_cluster_yaml("x", 3, "full", "B")
        self.assertIn("storageServersPerPod: 6", a)
        self.assertIn("storageServersPerPod: 12", b)
        self.assertIn("memory=3GiB", b)
        for line in ("commit_proxies: 6", "resolvers: 3", "stateless: 13", "logs: 6"):
            self.assertIn(line, b)

    def test_layout_c_doubles_resolvers_and_commit_proxies_and_grows_stateless(self):
        c = L.render_fdb_cluster_yaml("x", 3, "full", "C")
        for line in ("storageServersPerPod: 12", "commit_proxies: 12", "resolvers: 6", "grv_proxies: 3", "stateless: 22"):
            self.assertIn(line, c)

    def test_unknown_layout_is_refused(self):
        with self.assertRaises(L.Stop):
            L.render_fdb_cluster_yaml("x", 3, "full", "Z")


class CostTests(StateDirCase):
    def test_two_billed_hours_and_cap(self):
        cfg = base_cfg(self.tmp)
        ctx = L.Ctx(cfg, True)
        with contextlib.redirect_stdout(io.StringIO()):
            worst = L.print_cost(ctx, Decimal("5.6"))
        # 3 ccx43 + 3 cpx62 with the IPv4: (0.401 * 3 + 0.201 * 3) per hour, 110 + 10 minutes = 2 started hours
        self.assertEqual(Decimal(str(worst)), (Decimal("0.401") * 3 + Decimal("0.201") * 3) * 2)
        with contextlib.redirect_stdout(io.StringIO()), self.assertRaises(L.Stop):
            L.print_cost(ctx, Decimal("1.0"))

    def test_third_hour_when_the_deadline_is_too_long(self):
        cfg = base_cfg(self.tmp, deadline_minutes=115)
        with contextlib.redirect_stdout(io.StringIO()):
            worst = L.print_cost(L.Ctx(cfg, True), Decimal("9"))
        self.assertEqual(Decimal(str(worst)), (Decimal("0.401") * 3 + Decimal("0.201") * 3) * 3)


class ColocationTests(StateDirCase):
    def test_clients_alias_the_database_nodes(self):
        roles = ["node%d" % i for i in range(1, 7)] + ["client%d" % i for i in range(1, 7)]
        ctx = L.Ctx(base_cfg(self.tmp, roles=roles, colocate_clients=True), True)
        self.assertEqual(ctx.real_roles, tuple("node%d" % i for i in range(1, 7)))
        self.assertEqual(L.PRIVATE_IP["client4"], L.PRIVATE_IP["node4"])

    def test_separate_clients_by_default(self):
        ctx = L.Ctx(base_cfg(self.tmp), True)
        self.assertEqual(len(ctx.real_roles), 6)
        self.assertNotEqual(L.PRIVATE_IP["client1"], L.PRIVATE_IP["node1"])


class DryRunUp(StateDirCase):
    def test_up_dry_run_creates_six_servers_and_arms_the_watchdog(self):
        cfgp = Path(self.tmp) / "c.json"
        cfgp.write_text(json.dumps(base_cfg(self.tmp)))
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            rc = L.main(["up", "--config", str(cfgp), "--dry-run", "--cap", "6"])
        text = out.getvalue()
        self.assertEqual(rc, 0, text[-500:])
        self.assertEqual(text.count("server create --name lean-unit-"), 6)
        self.assertIn("campaign=lean-unit", text)
        self.assertIn("arm watchdog lean-watch-lean-unit.service", text)
        self.assertNotIn("server delete", text)
        self.assertNotIn("network delete", text)


class TeardownTests(StateDirCase):
    def mk(self, i, lab="lean-unit"):
        return {"id": i, "labels": {"campaign": lab}}

    def test_teardown_deletes_by_label_only_and_proves_empty(self):
        state = Path(self.tmp) / "fake.json"
        mk = self.mk
        state.write_text(json.dumps({"server": [mk(1), mk(2), mk(99, "other")], "firewall": [mk(10), mk(98, "other")], "network": [mk(20)],
                                     "ssh-key": [mk(30)], "placement-group": [mk(40)], "primary-ip": [mk(50)], "volume": []}))
        os.environ["FAKE_HCLOUD_STATE"] = str(state)
        ctx = L.Ctx({"campaign": "lean-unit", "hcloud": str(HERE / "fake_hcloud.py")}, False)
        old = L.time.sleep
        L.time.sleep = lambda s: None
        try:
            with contextlib.redirect_stdout(io.StringIO()):
                proof = L.teardown(ctx, wait_s=60)
        finally:
            L.time.sleep = old
        left = json.loads(state.read_text())
        self.assertTrue(proof["empty"])
        self.assertEqual([i["id"] for i in left["server"]], [99])
        self.assertEqual([i["id"] for i in left["firewall"]], [98])

    def test_a_resource_that_cannot_be_deleted_fails_loudly(self):
        state = Path(self.tmp) / "fake.json"
        state.write_text(json.dumps({"server": [], "firewall": [], "network": [], "ssh-key": [self.mk(31)], "placement-group": [], "primary-ip": [], "volume": []}))
        os.environ["FAKE_HCLOUD_STATE"] = str(state)
        stuck = Path(self.tmp) / "stuck.py"
        stuck.write_text((HERE / "fake_hcloud.py").read_text().replace('else: st[kind] = [x for x in st[kind] if x["id"] != ident]', "pass"))
        os.chmod(stuck, 0o755)
        ctx = L.Ctx({"campaign": "lean-unit", "hcloud": str(stuck)}, False)
        old = L.time.sleep
        L.time.sleep = lambda s: None
        try:
            with contextlib.redirect_stdout(io.StringIO()), self.assertRaises(L.Stop):
                L.teardown(ctx, wait_s=2)
        finally:
            L.time.sleep = old

    def test_watchdog_fires_at_the_deadline(self):
        state = Path(self.tmp) / "fake.json"
        state.write_text(json.dumps({"server": [self.mk(1), self.mk(99, "other")], "firewall": [self.mk(10)], "network": [self.mk(20)], "ssh-key": [self.mk(30)],
                                     "placement-group": [], "primary-ip": [self.mk(50)], "volume": []}))
        wc = Path(self.tmp) / "wd.json"
        wc.write_text(json.dumps({"campaign": "lean-unit", "hcloud": str(HERE / "fake_hcloud.py"), "deadline_epoch": int(time.time()) + 2}))
        r = subprocess.run([sys.executable, "-B", str(HERE / "leanrun.py"), "watchdog", "--config", str(wc)], capture_output=True, text=True, timeout=120,
                           env={**os.environ, "FAKE_HCLOUD_STATE": str(state)})
        left = json.loads(state.read_text())
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual([i["id"] for i in left["server"]], [99])
        self.assertFalse(left["firewall"] or left["network"] or left["ssh-key"] or left["primary-ip"])
        self.assertTrue((Path(os.environ["LEAN_STATE_DIR"]) / "lean-unit" / "down.done").exists())


class ConditionTests(unittest.TestCase):
    def test_escape_round_trip(self):
        raw = bytes(range(0, 40)) + b"\\`'" + bytes([0xff, 0x00, 0x5c])
        self.assertEqual(condition.unesc(condition.esc(raw)), raw)

    def test_store_prefix_and_restore_command(self):
        layout = {"record_prefix": "15131507151f1501"}
        s = condition.store_prefix(layout)
        self.assertEqual(s, bytes.fromhex("15131507151f"))
        with self.assertRaises(ValueError):
            condition.store_prefix({"record_prefix": "1513"})
        cmd = condition.restore_cmd(s, [(s + b"\x01", b"v"), (s + b"\x02", b"")])
        self.assertTrue(cmd.startswith("writemode on; begin; clearrange "))
        self.assertTrue(cmd.endswith("commit"))
        self.assertIn('\\x15\\x13\\x15\\x07', condition.esc(b"\x15\x13\x15\x07"))
        self.assertIn('""', cmd)                              # an empty value is written as ""

    def test_getrange_parsing_and_digest(self):
        out = "Range limited to 1000 keys\n`\\x15\\x01' is `a\\x00b'\n`\\x15\\x02' is `'\n"
        items = condition.parse_getrange(out)
        self.assertEqual(items, [(b"\x15\x01", b"a\x00b"), (b"\x15\x02", b"")])
        self.assertEqual(condition.digest(items), condition.digest(list(items)))
        self.assertNotEqual(condition.digest(items), condition.digest(items[:1]))
        with self.assertRaises(ValueError):
            condition.parse_getrange("`broken line without the marker")


class ThreadSamplerTests(unittest.TestCase):
    def test_busiest_thread_is_found(self):
        code = ("import threading, time\n"
                "def burn():\n    t = time.time()\n    while time.time() - t < 6: pass\n"
                "threading.Thread(target=burn, name='net-thread').start()\ntime.sleep(7)\n")
        out = Path(tempfile.mkdtemp()) / "t.json"
        env = {**os.environ, "THREADCPU_HEAD_S": "1", "THREADCPU_TAIL_SAMPLES": "1"}
        child = subprocess.Popen([sys.executable, "-c", code])
        sampler = subprocess.Popen([sys.executable, str(HERE / "threadcpu.py"), str(out), str(child.pid)], env=env)
        try:
            self.assertEqual(sampler.wait(timeout=60), 0)
        finally:
            child.wait()
        proc = json.loads(out.read_text())["processes"][0]
        top = proc["threads"][0]
        self.assertNotEqual(top["tid"], str(proc["pid"]))        # the busy worker thread, not the idle main thread
        self.assertGreater(top["cores_mean"], 0.4)       # a burning thread; the host may be loaded
        self.assertLessEqual(top["cores_mean"], 1.2)
        self.assertGreaterEqual(proc["thread_count"], 2)


class ClimbScriptTests(StateDirCase):
    def test_launch_script_starts_all_processes_together_and_samples_threads(self):
        import climb
        cfg = base_cfg(self.tmp, assets={"corpus_1000": {"path": "/x/c.bin", "sha256": "9" * 64}}, verification_job=L.VERIFICATION_NONE)
        ctx = L.Ctx(cfg, True)
        ctx.builds = L.Builds(cfg)
        tenants = [{"idx": i, "run": "run%d" % i, "lv": {"run": "run%d" % i, "cluster_file": "/srv/lean/fdb-s3.cluster", "cluster_sha256": "0" * 64,
                                                         "layout_file": "/srv/lean/runs/run%d/layout.json" % i, "layout_sha256": "1" * 64,
                                                         "verification": L.VERIFICATION_NONE}} for i in range(3)]
        with contextlib.redirect_stdout(io.StringIO()):
            script, outputs, tpath = climb.launch_script(ctx, "rust_native", tenants, 1000, "tagx")
        self.assertEqual(script.count("& pids+=($!)"), 3)
        self.assertEqual(len(outputs), 3)
        self.assertIn("threadcpu.py", script)
        self.assertIn('"${pids[@]}"', script)
        self.assertTrue(tpath.endswith("threads-tagx.json"))
        self.assertLess(script.index("pids+=($!)"), script.index("threadcpu.py"))


if __name__ == "__main__":
    unittest.main()
