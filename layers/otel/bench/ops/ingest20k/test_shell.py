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
if os.environ.get('FAIL_PROBE') in ('all', p.name, str(p)):
    sys.exit(19)
nocow = os.environ.get('NOCOW_PROBE') in (p.name, str(p))
print(json.dumps({'path':str(p), 'flags':8388608 if nocow else 0, 'nocow':nocow}))
if nocow: sys.exit(1)
""")
        self.tool("chown", """
import json, os, sys
with open(os.environ['FIXTURE_LOG'], 'a') as f:
    f.write(json.dumps({'op':'chown', 'argv':sys.argv[1:]})+'\\n')
""")

        self.monitor = self.tool("fixture-monitor", """
import json, os
with open(os.environ['FIXTURE_LOG'], 'a') as f:
    f.write(json.dumps({'op':'monitor'})+'\\n')
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
        # Match production's conditional function call: errexit cannot mask a
        # missing explicit return inside prepare before the monitor would start.
        return subprocess.run(["/bin/bash", "-c", '''
source "$1"
monitor=$2
shift 2
bootstrap() {
    ingest20k_prepare "$@" || return
    "$monitor"
}
bootstrap "$@"
''', "fixture", str(HERE / "container-entrypoint.sh"), str(self.monitor),
                               str(root or self.root), str(self.probe), str(self.seed), owner, session, run],
                              env=env or self.env, capture_output=True, text=True, timeout=10)

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
        for port in (14883, 14886):
            for name in ("serverCheckpoints", "fetchedCheckpoints"):
                (self.root / "processes" / str(port) / name).mkdir()
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
        self.assertEqual([e["op"] for e in events], ["GET", "chown", "monitor"])
        self.assertEqual((self.root / "cluster/fdb.cluster").read_bytes(), self.seed.read_bytes())
        self.assertEqual((self.root / ".otel283-owner").read_text(),
                         "otel283-vxnar prof20k1234 prof20k1234\n")
        self.assertEqual(len(list((self.root / "processes").iterdir())), 4)
        self.assertEqual(len(list((self.root / "traces").iterdir())), 4)

    def test_bootstrap_refusals_never_write_layout(self):
        for case in ("populated", "symlink", "missing", "probe", "find", "mkdir", "mkdir-port", "cp", "chown", "owner", "session", "run"):
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
                elif case in ("mkdir", "mkdir-port", "cp", "chown"):
                    command = "mkdir" if case == "mkdir-port" else case
                    real_command = shutil.which(command, path=os.defpath)
                    self.assertIsNotNone(real_command)
                    self.tool(command, f"""
import json, os, sys
if {case!r} == 'mkdir-port' and not any(arg.endswith('/14883') for arg in sys.argv[1:]):
    os.execv({real_command!r}, [{real_command!r}] + sys.argv[1:])
with open(os.environ['FIXTURE_LOG'], 'a') as f:
    f.write(json.dumps({{'op':{case!r}}})+'\\n')
sys.exit(24)
""")
                elif case == "owner":
                    kwargs["owner"] = "wrong"
                elif case == "session":
                    kwargs["session"] = "different"
                elif case == "run":
                    kwargs["run"] = kwargs["session"] = "not-a-run"
                result = self.prepare(candidate, env=env, **kwargs)
                expected_exit = {"probe": 19, "find": 23, "mkdir": 24, "mkdir-port": 24, "cp": 24, "chown": 24}.get(case, 1)
                self.assertEqual(result.returncode, expected_exit)
                self.assertNotIn("monitor", [e["op"] for e in self.events()])
                if case == "probe":
                    self.assertEqual(list(candidate.iterdir()), [])
                if case not in ("mkdir-port", "cp", "chown"):
                    self.assertFalse((candidate / "cluster").exists())
                if case != "chown":
                    self.assertFalse((candidate / ".otel283-owner").exists())
                if case in ("find", "mkdir", "mkdir-port", "cp"):
                    (self.tools / ("mkdir" if case == "mkdir-port" else case)).unlink()
                if self.log.exists():
                    events = self.events()
                    self.assertIn(case, ("probe", "mkdir", "mkdir-port", "cp", "chown"))
                    self.assertEqual([e["op"] for e in events], ["GET"] if case == "probe" else ["GET", case])
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
        expected_dirs = {str(self.root), str(self.root / "cluster"), str(self.root / "processes"), str(self.root / "traces")}
        expected_dirs |= {str(self.root / d / str(p)) for d in ("processes", "traces") for p in range(14883, 14887)}
        expected_dirs |= {str(self.root / "processes" / str(p) / name)
                          for p in (14883, 14886) for name in ("serverCheckpoints", "fetchedCheckpoints")}
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
                 "wrong-queue-uuid", "unknown-file", "unknown-dir", "uid", "dev", "stat", "find", "probe",
                 "cluster-uid", "cluster-get", "cluster-nocow")
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
                    elif case in ("dev", "stat", "cluster-uid"):
                        real_stat = shutil.which("stat", path=os.defpath)
                        self.assertIsNotNone(real_stat)
                        self.tool("stat", f"""
import os, sys
if {case!r} == 'stat': sys.exit(22)
if {case!r} == 'cluster-uid' and sys.argv[-1].endswith('/cluster') and sys.argv[2] == '%u':
    print(os.getuid() + 1); sys.exit(0)
if {case!r} == 'dev' and sys.argv[-1].endswith('/traces') and sys.argv[2] == '%d':
    print('99999999'); sys.exit(0)
os.execv({real_stat!r}, [{real_stat!r}] + sys.argv[1:])
""")
                    elif case == "find":
                        self.tool("find", "import sys\nsys.exit(23)\n")
                    elif case == "probe":
                        env["FAIL_PROBE"] = "trace.xml"
                    elif case == "cluster-get":
                        env["FAIL_PROBE"] = "cluster"
                    elif case == "cluster-nocow":
                        env["NOCOW_PROBE"] = "cluster"
                    result = self.inventory(uid=uid, env=env)
                    self.assertNotEqual(result.returncode, 0, case)
                    self.assertNotIn('"schema":"ingest20k_inode_proof_v1"', result.stdout)
                    if case == "cluster-uid":
                        self.assertNotIn(str(self.root / "cluster"), [e["path"] for e in self.events()])
                    elif case in ("cluster-get", "cluster-nocow"):
                        self.assertEqual([e["path"] for e in self.events()], [str(self.root), str(self.root / "cluster")])
                        if case == "cluster-nocow":
                            self.assertTrue(json.loads(result.stdout.splitlines()[-1])["nocow"])
                finally:
                    self.root = original
                    for tool in ("stat", "find"):
                        if (self.tools / tool).exists():
                            (self.tools / tool).unlink()
                    if self.log.exists():
                        self.log.unlink()

    def test_checkpoint_directories_are_exact_empty_and_individually_get_verified(self):
        for port in (14883, 14886):
            for name in ("serverCheckpoints", "fetchedCheckpoints"):
                for fault in ("missing", "symlink", "file", "child-file", "child-dir", "uid", "dev", "get", "nocow", "find"):
                    with self.subTest(port=port, name=name, fault=fault), tempfile.TemporaryDirectory(dir=self.base) as temp:
                        original = self.root
                        self.root = Path(temp) / "data"
                        self.root.mkdir()
                        try:
                            self.population()
                            target = self.root / "processes" / str(port) / name
                            env = self.env.copy()
                            if fault in ("missing", "symlink", "file"):
                                target.rmdir()
                                if fault == "symlink":
                                    target.symlink_to(original, target_is_directory=True)
                                elif fault == "file":
                                    target.write_text("not a directory")
                            elif fault == "child-file":
                                (target / "unexpected").write_text("not an empty checkpoint directory")
                            elif fault == "child-dir":
                                (target / "unexpected").mkdir()
                            elif fault in ("uid", "dev"):
                                real_stat = shutil.which("stat", path=os.defpath)
                                self.assertIsNotNone(real_stat)
                                self.tool("stat", f"""
import os, sys
if sys.argv[-1] == {str(target)!r} and sys.argv[2] == {'%u' if fault == 'uid' else '%d'!r}:
    print({os.getuid() + 1 if fault == 'uid' else target.stat().st_dev + 1}); sys.exit(0)
os.execv({real_stat!r}, [{real_stat!r}] + sys.argv[1:])
""")
                            elif fault == "get":
                                env["FAIL_PROBE"] = str(target)
                            elif fault == "nocow":
                                env["NOCOW_PROBE"] = str(target)
                            elif fault == "find":
                                real_find = shutil.which("find", path=os.defpath)
                                self.assertIsNotNone(real_find)
                                self.tool("find", f"""
import os, sys
if sys.argv[1] == {str(target)!r}: sys.exit(23)
os.execv({real_find!r}, [{real_find!r}] + sys.argv[1:])
""")
                            result = self.inventory(env=env)
                            self.assertNotEqual(result.returncode, 0)
                            self.assertNotIn('"schema":"ingest20k_inode_proof_v1"', result.stdout)
                            observed = [e['path'] for e in self.events()]
                            if fault in ("get", "nocow"):
                                self.assertIn(str(target), observed)
                                self.assertEqual(observed[-1], str(target))
                                if fault == "nocow":
                                    self.assertTrue(json.loads(result.stdout.splitlines()[-1])["nocow"])
                            elif fault in ("uid", "dev", "missing", "symlink", "file", "child-file", "child-dir", "find"):
                                self.assertNotIn(str(target), observed)
                        finally:
                            self.root = original
                            for tool in ("stat", "find"):
                                if (self.tools / tool).exists():
                                    (self.tools / tool).unlink()
                            if self.log.exists():
                                self.log.unlink()
        wrong_dirs = ("processes/14884/serverCheckpoints", "processes/14885/fetchedCheckpoints",
                      "traces/14883/serverCheckpoints", "processes/14883/serverCheckpoint",
                      "processes/14886/fetchedCheckpoints-extra")
        reserved_files = tuple(f"{directory}/{name}" for directory in ("processes/14884", "processes/14885", "traces/14883")
                               for name in ("serverCheckpoints", "fetchedCheckpoints"))
        for relative, kind in [(p, "directory") for p in wrong_dirs] + [(p, "file") for p in reserved_files]:
            with self.subTest(relative=relative, kind=kind), tempfile.TemporaryDirectory(dir=self.base) as temp:
                original = self.root
                self.root = Path(temp) / "data"
                self.root.mkdir()
                try:
                    self.population()
                    extra = self.root / relative
                    if kind == "directory":
                        extra.mkdir()
                    else:
                        extra.write_text("reserved checkpoint name is not a regular inode")
                    result = self.inventory()
                    self.assertNotEqual(result.returncode, 0)
                    self.assertNotIn('"schema":"ingest20k_inode_proof_v1"', result.stdout)
                finally:
                    self.root = original
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
        ready = self.base / "ready"
        ready.mkdir()
        worker.write_text("""
import json, os, pathlib, signal, sys, time
record = sys.argv[1]
ready = pathlib.Path(sys.argv[2])
def emit(event):
    with open(record, 'a') as f:
        f.write(json.dumps({'event':event,'pid':os.getpid(),'time':time.monotonic()})+'\\n')
def term(signum, frame):
    emit('TERM')
signal.signal(signal.SIGTERM, term)
emit('setup')
if os.fork() == 0:
    emit('descendant')
    (ready / 'descendant').touch()
else:
    emit('server')
    (ready / 'server').touch()
while True:
    time.sleep(0.05)
""")
        # Arithmetic/expiry is tested above with a controlled clock. Here leave
        # startup ample room, then signal only timeout after both handlers exist:
        # timeout itself must forward TERM and escalate to KILL for descendants.
        proc = subprocess.Popen(["/bin/bash", str(HERE / "deadline.sh"), str(int(time.time()) + 180),
                                 sys.executable, str(worker), str(record), str(ready)], env=self.env,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, start_new_session=True)
        completed = False
        try:
            until = time.monotonic() + 60
            while not all((ready / name).exists() for name in ('server', 'descendant')):
                self.assertIsNone(proc.poll(), 'timeout/worker exited before readiness')
                self.assertLess(time.monotonic(), until, 'inert process readiness watchdog expired')
                time.sleep(0.05)
            proc.send_signal(signal.SIGTERM)
            stdout, stderr = proc.communicate(timeout=60)
            completed = True
        finally:
            if not completed:
                # The timeout parent may have exited while descendants hold the
                # pipes; clean only this fixture's newly created process group.
                try:
                    os.killpg(proc.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                proc.communicate(timeout=10)
        self.assertNotEqual(proc.returncode, 0, (stdout, stderr))
        rows = [json.loads(line) for line in record.read_text().splitlines()]
        self.assertEqual(sum(r['event'] == 'setup' for r in rows), 1)
        server = next(r for r in rows if r['event'] == 'server')
        descendant = next(r for r in rows if r['event'] == 'descendant')
        pids = {server['pid'], descendant['pid']}
        self.assertEqual(len(pids), 2)
        self.assertEqual({r['pid'] for r in rows if r['event'] == 'TERM'}, pids)
        # A zombie has stopped executing; never signal arbitrary or unowned PIDs.
        for pid in pids:
            stat = Path(f"/proc/{pid}/stat")
            def stopped():
                try:
                    return stat.read_text().split(') ', 1)[1].startswith('Z ')
                except FileNotFoundError:
                    return True
            until = time.monotonic() + 60
            while not stopped() and time.monotonic() < until:
                time.sleep(0.01)
            self.assertTrue(stopped(), pid)


if __name__ == "__main__":
    unittest.main()
