"""Offline execution of the shell gates; all writable paths are fixture-owned."""
import hashlib
import json
import os
from pathlib import Path
import signal
import shutil
import subprocess
import sys
import tempfile
import time
import unittest

HERE = Path(__file__).resolve().parent


class ShellTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="ingest20k-shell-")
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name)
        self.tools = self.base / "tools"
        self.tools.mkdir()
        self.log = self.base / "operations.jsonl"
        self.env = dict(os.environ, PATH=str(self.tools) + os.pathsep + os.environ["PATH"],
                        FIXTURE_LOG=str(self.log))
        self.root = self.base / "data"
        self.root.mkdir()
        self.seed = self.base / "seed"
        self.seed.write_text("fixture:fixture@127.0.0.1:14883\n")
        self.probe = self.tool("probe", """
import json, os, pathlib, sys
p = pathlib.Path(sys.argv[1])
with open(os.environ['FIXTURE_LOG'], 'a') as f:
    f.write(json.dumps({'op':'GET', 'path':str(p),
                        'children':sorted(x.name for x in p.iterdir()) if p.is_dir() else None})+'\\n')
if os.environ.get('FAIL_PROBE') in ('all', p.name):
    sys.exit(19)
print(json.dumps({'path':str(p), 'flags':0, 'nocow':False}))
""")
        self.tool("chown", """
import json, os, sys
with open(os.environ['FIXTURE_LOG'], 'a') as f:
    f.write(json.dumps({'op':'chown', 'argv':sys.argv[1:]})+'\\n')
""")

    def tool(self, name, body):
        path = self.tools / name
        path.write_text("#!" + sys.executable + "\n" + body.lstrip())
        path.chmod(0o755)
        return path

    def invoke(self, script, function, *args, env=None):
        return subprocess.run(["/bin/bash", "-c", 'source "$1"; shift; "$@"',
                               "fixture", str(HERE / script), function, *map(str, args)],
                              env=env or self.env, capture_output=True, text=True, timeout=10)

    def prepare(self, root=None, env=None, owner="otel283-vxnar", session="prof20k1234", run="prof20k1234"):
        return self.invoke("container-entrypoint.sh", "ingest20k_prepare", root or self.root,
                           self.probe, self.seed, owner, session, run, env=env)

    def inventory(self, uid=None, env=None):
        return self.invoke("inode-readonly.sh", "ingest20k_inventory", self.root,
                           self.probe, os.getuid() if uid is None else uid, env=env)

    def events(self):
        return [json.loads(line) for line in self.log.read_text().splitlines()] if self.log.exists() else []

    def population(self):
        (self.root / "cluster").mkdir()
        (self.root / "cluster/fdb.cluster").write_text("seed")
        (self.root / ".otel283-owner").write_text("fixture")
        for directory in ("processes", "traces"):
            for port in range(14883, 14887):
                (self.root / directory / str(port)).mkdir(parents=True)
        uuid = "a" * 32
        names = [f"processes/14883/storage-{'b' * 32}.redwood-v1",
                 f"processes/14886/storage-{'c' * 32}.redwood-v1",
                 f"processes/14884/log2-V_6-{uuid}.sqlite",
                 f"processes/14884/log2-V_6-{uuid}.sqlite-wal",
                 f"processes/14884/logqueue-V_6-{uuid}-0.fdq",
                 f"processes/14884/logqueue-V_6-{uuid}-1.fdq", "traces/14885/trace.xml"]
        for name in names:
            (self.root / name).write_text("inert")
        return names

    def test_empty_get_precedes_every_bootstrap_write(self):
        result = self.prepare()
        self.assertEqual(result.returncode, 0, result.stderr)
        events = self.events()
        self.assertEqual(events[0], {"op": "GET", "path": str(self.root), "children": []})
        self.assertEqual([e["op"] for e in events], ["GET", "chown"])
        self.assertEqual((self.root / "cluster/fdb.cluster").read_bytes(), self.seed.read_bytes())
        self.assertEqual((self.root / ".otel283-owner").read_text(),
                         "otel283-vxnar prof20k1234 prof20k1234\n")
        self.assertEqual(len(list((self.root / "processes").iterdir())), 4)
        self.assertEqual(len(list((self.root / "traces").iterdir())), 4)

    def test_bootstrap_refusals_never_write_layout(self):
        for case in ("populated", "symlink", "missing", "probe", "find", "mkdir", "cp", "chown", "owner", "session", "run"):
            with self.subTest(case=case):
                candidate = self.base / ("candidate-" + case)
                candidate.mkdir()
                env = self.env.copy()
                kwargs = {}
                if case == "populated":
                    (candidate / "existing").write_text("keep")
                elif case == "symlink":
                    candidate.rmdir()
                    candidate.symlink_to(self.root, target_is_directory=True)
                elif case == "missing":
                    candidate.rmdir()
                elif case == "probe":
                    env["FAIL_PROBE"] = "all"
                elif case == "find":
                    self.tool("find", "import sys\nsys.exit(23)\n")
                elif case in ("mkdir", "cp", "chown"):
                    self.tool(case, "import sys\nsys.exit(24)\n")
                elif case == "owner":
                    kwargs["owner"] = "wrong"
                elif case == "session":
                    kwargs["session"] = "different"
                elif case == "run":
                    kwargs["run"] = kwargs["session"] = "not-a-run"
                result = self.prepare(candidate, env=env, **kwargs)
                self.assertNotEqual(result.returncode, 0)
                if case not in ("cp", "chown"):
                    self.assertFalse((candidate / "cluster").exists())
                if case != "chown":
                    self.assertFalse((candidate / ".otel283-owner").exists())
                if case in ("find", "mkdir", "cp"):
                    (self.tools / case).unlink()
                if self.log.exists():
                    events = self.events()
                    self.assertIn(case, ("probe", "mkdir", "cp", "chown"))
                    self.assertEqual([e["op"] for e in events], ["GET"])
                    self.log.unlink()

    def test_inventory_reads_the_complete_population(self):
        self.population()
        before = {str(p.relative_to(self.root)): (p.stat().st_mode,
                  p.read_bytes() if p.is_file() else None) for p in self.root.rglob("*")}
        result = self.inventory()
        self.assertEqual(result.returncode, 0, result.stderr)
        rows = [json.loads(line) for line in result.stdout.splitlines()]
        summary = rows.pop()
        expected_files = {str(p) for p in self.root.rglob("*") if p.is_file()}
        expected_dirs = {str(self.root), str(self.root / "processes"), str(self.root / "traces")}
        expected_dirs |= {str(self.root / d / str(p)) for d in ("processes", "traces") for p in range(14883, 14887)}
        self.assertEqual({r["path"] for r in rows}, expected_files | expected_dirs)
        self.assertEqual(len(rows), len(expected_files | expected_dirs))
        self.assertEqual(summary, {"schema": "ingest20k_inode_proof_v1",
                                  "regular_files_verified": 9, "primary_sets_verified": 3})
        self.assertEqual({e["path"] for e in self.events()}, expected_files | expected_dirs)
        after = {str(p.relative_to(self.root)): (p.stat().st_mode,
                 p.read_bytes() if p.is_file() else None) for p in self.root.rglob("*")}
        self.assertEqual(after, before)

    def test_inventory_rejects_incomplete_unsafe_or_failed_observations(self):
        cases = ("empty", "symlink", "symlink-dir", "fifo", "missing-dir", "missing-seed", "missing-owner",
                 "missing-storage", "duplicate-storage", "bad-storage-uuid", "missing-log",
                 "duplicate-log", "bad-log-uuid", "missing-wal", "wrong-wal-uuid", "missing-queue",
                 "wrong-queue-uuid", "unknown-file", "unknown-dir", "uid", "dev", "stat", "find", "probe")
        for case in cases:
            with self.subTest(case=case), tempfile.TemporaryDirectory(dir=self.base) as temp:
                original = self.root
                self.root = Path(temp) / "data"
                self.root.mkdir()
                try:
                    names = self.population() if case != "empty" else []
                    env = self.env.copy()
                    uid = None
                    if case == "symlink":
                        (self.root / "traces/14883/link").symlink_to(self.seed)
                    elif case == "symlink-dir":
                        (self.root / "traces/14883").rmdir()
                        (self.root / "traces/14883").symlink_to(original, target_is_directory=True)
                    elif case == "fifo":
                        os.mkfifo(self.root / "traces/14883/pipe")
                    elif case == "missing-dir":
                        (self.root / "traces/14883").rmdir()
                    elif case in ("missing-seed", "missing-owner"):
                        (self.root / ("cluster/fdb.cluster" if case == "missing-seed" else ".otel283-owner")).unlink()
                    elif case in ("missing-storage", "duplicate-storage", "bad-storage-uuid"):
                        p = self.root / names[0]
                        if case == "duplicate-storage":
                            p.with_name("storage-" + "d" * 32 + ".redwood-v1").write_text("extra")
                        elif case == "bad-storage-uuid":
                            p.rename(p.with_name("storage-bad.redwood-v1"))
                        else:
                            p.unlink()
                    elif case in ("missing-log", "duplicate-log", "bad-log-uuid"):
                        p = self.root / names[2]
                        if case == "duplicate-log":
                            p.with_name("log2-V_6-" + "d" * 32 + ".sqlite").write_text("extra")
                        elif case == "bad-log-uuid":
                            p.rename(p.with_name("log2-V_6-bad.sqlite"))
                        else:
                            p.unlink()
                    elif case in ("missing-wal", "wrong-wal-uuid", "missing-queue", "wrong-queue-uuid"):
                        p = self.root / names[3 if "wal" in case else 4]
                        if case.startswith("wrong"):
                            p.rename(p.with_name(p.name.replace("a" * 32, "d" * 32)))
                        else:
                            p.unlink()
                    elif case == "unknown-file":
                        (self.root / "unexpected").write_text("no")
                    elif case == "unknown-dir":
                        (self.root / "traces/14883/nested").mkdir()
                    elif case == "uid":
                        uid = os.getuid() + 1
                    elif case in ("dev", "stat"):
                        real_stat = shutil.which("stat", path=os.defpath)
                        self.assertIsNotNone(real_stat)
                        self.tool("stat", f"""
import os, sys
if {case!r} == 'stat': sys.exit(22)
if sys.argv[-1].endswith('/traces') and sys.argv[2] == '%d':
    print('99999999'); sys.exit(0)
os.execv({real_stat!r}, [{real_stat!r}] + sys.argv[1:])
""")
                    elif case == "find":
                        self.tool("find", "import sys\nsys.exit(23)\n")
                    elif case == "probe":
                        env["FAIL_PROBE"] = "trace.xml"
                    result = self.inventory(uid=uid, env=env)
                    self.assertNotEqual(result.returncode, 0, case)
                    self.assertNotIn('"schema":"ingest20k_inode_proof_v1"', result.stdout)
                finally:
                    self.root = original
                    for tool in ("stat", "find"):
                        if (self.tools / tool).exists():
                            (self.tools / tool).unlink()
                    if self.log.exists():
                        self.log.unlink()

    def test_builder_receipts_fail_closed_without_executing_output(self):
        compiler = self.tool("fixture-cc", """
import json, os, pathlib, sys
if '--version' in sys.argv:
    print('fixture compiler 1'); sys.exit(0)
with open(os.environ['FIXTURE_LOG'], 'a') as f:
    f.write(json.dumps({'argv':sys.argv[1:]})+'\\n')
if os.environ.get('BUILD_CASE') == 'compiler':
    print('fixture compile failure', file=sys.stderr); sys.exit(17)
out = pathlib.Path(sys.argv[sys.argv.index('-o') + 1])
out.write_text('#!/bin/sh\\necho EXECUTED > "'+os.environ['EXECUTED']+'"\\nexit 99\\n')
out.chmod(0o755)
""")
        self.tool("readelf", """
import os, sys
case = os.environ.get('BUILD_CASE')
if case == 'readelf': sys.exit(18)
if case == 'invalid': print('not ELF'); sys.exit(0)
print('Class: ELF64')
if case == 'interp': print('INTERP')
if case == 'needed': print('(NEEDED) libc.so')
""")
        for case in ("static", "compiler", "readelf", "invalid", "interp", "needed", "exists"):
            with self.subTest(case=case):
                out = self.base / ("build-" + case)
                executed = self.base / ("executed-" + case)
                env = dict(self.env, CC=str(compiler), BUILD_CASE=case, EXECUTED=str(executed))
                if case == "exists":
                    out.mkdir()
                    (out / "preserve").write_text("unchanged")
                result = subprocess.run(["/bin/bash", str(HERE / "build-probe.sh"), str(out)],
                                        env=env, capture_output=True, text=True, timeout=10)
                self.assertFalse(executed.exists())
                self.assertEqual(result.returncode == 0, case == "static", result.stderr)
                if case == "exists":
                    self.assertEqual(list(out.iterdir()), [out / "preserve"])
                    continue
                self.assertEqual(int((out / "build.exit").read_text()), result.returncode)
                self.assertIn("inode_flags.c", (out / "source.sha256").read_text())
                self.assertIn(hashlib.sha256((HERE / "inode_flags.c").read_bytes()).hexdigest(),
                              (out / "source.sha256").read_text())
                self.assertEqual((out / "compiler.path").read_text().strip(), str(compiler))
                self.assertIn(hashlib.sha256(compiler.read_bytes()).hexdigest(), (out / "compiler.sha256").read_text())
                self.assertIn("fixture compiler 1", (out / "compiler.version").read_text())
                command = (out / "command.sh").read_text()
                for argument in ("-static", "-Wall", "-Wextra", "-Werror", "inode_flags.c"):
                    self.assertIn(argument, command)
                self.assertEqual(int((out / "compiler.exit").read_text()), 17 if case == "compiler" else 0)
                if case == "compiler":
                    self.assertIn("fixture compile failure", (out / "compiler.stderr").read_text())
                    self.assertFalse((out / "binary.sha256").exists())
                else:
                    digest = hashlib.sha256((out / "inode-flags").read_bytes()).hexdigest()
                    self.assertIn(digest, (out / "binary.sha256").read_text())
                    self.assertTrue((out / "binary.elf.txt").exists())
                    if case != "readelf":
                        self.assertEqual((out / "static-elf.receipt").read_text().startswith("PASS:"), case == "static")

    def test_production_main_wires_fixed_deadline_and_setup_paths(self):
        capture = self.tool("capture", "import json,sys\nprint(json.dumps(sys.argv[1:]))\n")
        outer = subprocess.run(["/bin/bash", "-c", '''
source "$1"
capture=$2
exec() { "$capture" "$@"; exit "$?"; }
ingest20k_prepare() { "$capture" unexpected-prepare "$@"; }
ingest20k_entry_main 2200
''', "fixture", str(HERE / "container-entrypoint.sh"), str(capture)],
                               env=self.env, capture_output=True, text=True, timeout=5)
        self.assertEqual(outer.returncode, 0, outer.stderr)
        self.assertEqual(json.loads(outer.stdout), ["/bin/bash", "/etc/fdb4/deadline.sh", "2200",
                         "/bin/bash", "/etc/fdb4/container-entrypoint.sh", "--inside", "2200"])
        inside = subprocess.run(["/bin/bash", "-c", '''
source "$1"
capture=$2
exec() { "$capture" "$@"; exit "$?"; }
ingest20k_prepare() { "$capture" prepare "$@"; }
ingest20k_entry_main --inside 2200
''', "fixture", str(HERE / "container-entrypoint.sh"), str(capture)],
                                env=dict(self.env, FDB4_OWNER="otel283-vxnar", FDB4_SESSION="prof20k1234", FDB4_RUN="prof20k1234"),
                                capture_output=True, text=True, timeout=5)
        self.assertEqual(inside.returncode, 0, inside.stderr)
        rows = [json.loads(line) for line in inside.stdout.splitlines()]
        self.assertEqual(rows[0], ["prepare", "/var/fdb/data", "/etc/ingest20k/inode-flags",
                         "/etc/fdb4/fdb.cluster.seed", "otel283-vxnar", "prof20k1234", "prof20k1234"])
        self.assertEqual(rows[1], ["/usr/bin/fdbmonitor", "--conffile", "/etc/fdb4/foundationdb.conf",
                         "--lockfile", "/var/run/otel283-ingest20k-monitor.pid"])

    def test_absolute_deadline_full_cap_and_consumed_budget(self):
        self.tool("date", "import os\nprint(os.environ['FIXTURE_NOW'])\n")
        inspect_parent = "import os,json; print(json.dumps(open('/proc/%d/cmdline'%os.getppid(),'rb').read().decode().split('\\0')))"
        for now, expected in ((1000, "1195s"), (1007, "1188s")):
            result = subprocess.run(["/bin/bash", str(HERE / "deadline.sh"), "2200", sys.executable,
                                     "-c", inspect_parent], env=dict(self.env, FIXTURE_NOW=str(now)),
                                    capture_output=True, text=True, timeout=5)
            self.assertEqual(result.returncode, 0, result.stderr)
            argv = json.loads(result.stdout)
            self.assertEqual(argv[:4], ["/usr/bin/timeout", "--signal=TERM", "--kill-after=5s", expected])
        for now, epoch in ((2200, "2200"), (2196, "2200"), (999, "2200"), (1000, "invalid")):
            marker = self.base / "deadline-command-ran"
            result = subprocess.run(["/bin/bash", str(HERE / "deadline.sh"), epoch, sys.executable,
                                     "-c", "from pathlib import Path; Path(__import__('sys').argv[1]).touch()", str(marker)],
                                    env=dict(self.env, FIXTURE_NOW=str(now)), capture_output=True, text=True, timeout=5)
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse(marker.exists())

    def test_deadline_contains_setup_and_term_ignoring_descendants(self):
        worker = self.base / "inert-worker.py"
        record = self.base / "processes.jsonl"
        worker.write_text("""
import json, os, signal, sys, time
record = sys.argv[1]
def emit(event):
    with open(record, 'a') as f:
        f.write(json.dumps({'event':event,'pid':os.getpid(),'time':time.monotonic()})+'\\n')
def term(signum, frame):
    emit('TERM')
signal.signal(signal.SIGTERM, term)
emit('setup')
time.sleep(2)
emit('server')
if os.fork() == 0:
    emit('descendant')
while True:
    time.sleep(0.05)
""")
        started = time.monotonic()
        proc = subprocess.Popen(["/bin/bash", str(HERE / "deadline.sh"), str(int(time.time()) + 9),
                                 sys.executable, str(worker), str(record)], env=self.env,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, start_new_session=True)
        try:
            stdout, stderr = proc.communicate(timeout=16)
        finally:
            if proc.poll() is None:
                os.killpg(proc.pid, signal.SIGKILL)
                proc.communicate(timeout=3)
        elapsed = time.monotonic() - started
        self.assertNotEqual(proc.returncode, 0, (stdout, stderr))
        self.assertLess(elapsed, 12)
        rows = [json.loads(line) for line in record.read_text().splitlines()]
        setup = next(r for r in rows if r['event'] == 'setup')
        server = next(r for r in rows if r['event'] == 'server')
        descendant = next(r for r in rows if r['event'] == 'descendant')
        self.assertGreaterEqual(server['time'] - setup['time'], 1.8)
        pids = {server['pid'], descendant['pid']}
        self.assertEqual({r['pid'] for r in rows if r['event'] == 'TERM'}, pids)
        first_term = min(r['time'] for r in rows if r['event'] == 'TERM')
        self.assertLess(first_term - server['time'], 3.5)
        self.assertGreaterEqual(elapsed - (first_term - started), 4.5)
        # A zombie has stopped executing; never signal arbitrary or unowned PIDs.
        for pid in pids:
            stat = Path(f"/proc/{pid}/stat")
            def stopped():
                try:
                    return stat.read_text().split(') ', 1)[1].startswith('Z ')
                except FileNotFoundError:
                    return True
            until = time.monotonic() + 2
            while not stopped() and time.monotonic() < until:
                time.sleep(0.01)
            self.assertTrue(stopped(), pid)


if __name__ == "__main__":
    unittest.main()
