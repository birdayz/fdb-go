#!/usr/bin/env python3
"""Selected RFC263 production proofs; whole just-test lanes, never target filters.

Run from the checkout root after all source writers are stopped. Obtain explicit
owner approval for the reserved CPU/quiet window; a deadline or acquired lock is
not authorization. --deadline-utc must be future and at most two hours away.
--mode mutations first requires green nominal race AND opt lanes, then performs
one serial, reversible mutation at a time. Only no-renewal is mandatory.
Raw sources, SDK/config identities, BEP, logs and verdicts stay under --evidence.
"""
from __future__ import annotations

import argparse
import contextlib
import copy
import datetime as dt
import difflib
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import stat
import subprocess
import sys
import tarfile
import time
import traceback
import unittest
from urllib.parse import unquote, urlparse
import xml.etree.ElementTree as ET

ROOT = Path(__file__).resolve().parents[4]
CPUS = {0, 1, 6, 7, 11, 12, 13, 18, 19, 23}
READ = "pkg/fdbgo/client/read_incarnation.go"
QUEUE = "pkg/fdbgo/client/timeout_queue.go"
REGRESSION = "pkg/fdbgo/client/timeout_queue_regression_unit_test.go"
GENERATION = "pkg/fdbgo/client/timeout_generation_unit_test.go"
RESURRECTION = (
    "TestTimeoutQueueResurrectionAfterWeakNilDispatch",
    "TestTimeoutQueueResurrectionReset",
    "TestTimeoutQueueResurrectionRenewalAgainstCollectedNode/popped-before-weak-read",
    "TestTimeoutQueueResurrectionRenewalAgainstCollectedNode/immutable-ready-record",
)
CRITICAL = RESURRECTION + (
    "TestExpiredArmIsSynchronous/SetTimeout",
    "TestExpiredArmIsSynchronous/retry-publication",
    "TestExpiredArmIsSynchronous/OnError",
    "TestTimeoutQueueLogicalContinuationFIFO",
    "TestTimeoutQueueStubRepairIsNotActorResumption",
    "TestTimeoutQueueEagerSequencePublishedWithInsertion",
    "TestTimeoutQueueShardCohortFrontier",
    "TestTimeoutQueueDeferredHeadDoesNotHideEligibleWork",
    "TestTimeoutQueueStaggeredExitCallbackKeepsWake",
    "TestTimeoutPopThenRearm",
    "TestTimeoutPopThenReset/mid-turnover",
    "TestTimeoutPopThenReset/after-publication",
    "TestTimeoutRecordThenReset",
    "TestTimeoutPopThenCancel",
)
EXPECTED = {
    "no-renewal": [(name, "RESURRECTION_NODE_NOT_RENEWED") for name in RESURRECTION],
    "eager": [("TestTimeoutQueueEagerSequencePublishedWithInsertion", "EAGER_PUBLICATION_FIFO")],
    "frontier": [
        ("TestTimeoutQueueShardCohortFrontier", "SHARD_COHORT_FRONTIER"),
        ("TestTimeoutQueueDeferredHeadDoesNotHideEligibleWork", "DEFERRED_HEAD_COHORT"),
    ],
    "again": [("TestTimeoutQueueStaggeredExitCallbackKeepsWake", "STAGGERED_EXIT_LOST_WAKE")],
    "cleanup": [("TestTimeoutQueueCollectedTransactionUnlinksBeforeWake", "GC_CLEANUP_DID_NOT_UNLINK")],
    "stub-repair": [("TestTimeoutQueueStubRepairIsNotActorResumption", "STUB_REPAIR_TOOK_SEQUENCE")],
    "logical-order": [("TestTimeoutQueueLogicalContinuationFIFO", "CONTINUATION_FIFO")],
    "generation-barrier": [("TestTimeoutPopThenRearm", "TIMEOUT_QUEUE_WAIT: TIMEOUT_GENERATION_DELIVERY_PAUSE: beforeDeliver did not park")],
    "generation-turnover": [('TestTimeoutPopThenReset/mid-turnover', 'TIMEOUT_QUEUE_WAIT: stale arm delivery during mid-turnover')],
    "generation-turnover-no-defer": [('TestTimeoutPopThenReset/mid-turnover', 'TIMEOUT_QUEUE_WAIT: stale arm delivery during mid-turnover'), ('TestTimeoutPopThenReset/mid-turnover', 'TIMEOUT_QUEUE_WAIT: TURNOVER_CLEANUP_CANCEL_BLOCKED_WITHOUT_FINISH'), ('TestTimeoutPopThenReset/mid-turnover', 'TURNOVER_CLEANUP_UNPUBLISHED_WITHOUT_FINISH')],
}
RENEWAL = """\t} else if weak.Make(tx) != n.tx {
\t\t// The caller is strong: a different weak identity means resurrection.
\t\toldQueue := n.queue
\t\toldQueue.retire(n)
\t\tn = oldQueue.newNodeForShard(tx)
\t\ttx.timeoutNode = n
\t}
"""
EAGER = """\tif q.beforeEagerSchedule != nil {
\t\tq.beforeEagerSchedule(n)
\t}
\tq.mu.Lock()
\tn.seq = q.root.seq.Add(1)
"""


def digest(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def save_json(path: Path, value: object) -> None:
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")


def replace_one(text: str, old: str, new: str) -> str:
    count = text.count(old)
    if count != 1:
        raise RuntimeError(f"mutation anchor population: expected1, found{count}: {old!r}")
    return text.replace(old, new, 1)


STUB_REPAIR = "\t\t\t\t\t// A cancelled delay's physical wake only repairs its lower bound.\n\t\t\t\t\tq.pushExact(n, n.desiredWake)\n"
LOGICAL_ORDER = '\tslices.SortFunc(root.ready, func(a, b timeoutReady) int { return cmp.Compare(a.seq, b.seq) })\n'


GENERATION_BARRIER = '\t\tif n == tx.timeoutNode {\n\t\t\tclose(parked)\n'
GENERATION_BARRIER_MUTANT = '\t\tif n == tx.timeoutNode {\n\t\t\tif t.Name() != "TestTimeoutPopThenRearm" {\n\t\t\t\tclose(parked)\n\t\t\t}\n'


TURNOVER_FINISH = '\t\t\tfinish := sync.OnceFunc(token.finish)\n\t\t\tdefer finish()\n'
TURNOVER_WATCHDOG = '\t\t\tif phase == "mid-turnover" {\n\t\t\t\tt.Cleanup(func() {\n\t\t\t\t\tcancelDone := make(chan struct{})\n\t\t\t\t\tgo func() {\n\t\t\t\t\t\tdefer close(cancelDone)\n\t\t\t\t\t\ttx.Cancel()\n\t\t\t\t\t}()\n\t\t\t\t\tif !waitTimeoutQueueTest(t, cancelDone, "TURNOVER_CLEANUP_CANCEL_BLOCKED_WITHOUT_FINISH") {\n\t\t\t\t\t\ttx.readErrMu.Lock()\n\t\t\t\t\t\tstillUnpublished := tx.readReady != nil\n\t\t\t\t\t\ttx.readErrMu.Unlock()\n\t\t\t\t\t\tif stillUnpublished {\n\t\t\t\t\t\t\tt.Error("TURNOVER_CLEANUP_UNPUBLISHED_WITHOUT_FINISH")\n\t\t\t\t\t\t}\n\t\t\t\t\t\tfinish()\n\t\t\t\t\t\twaitTimeoutQueueTest(t, cancelDone, "TURNOVER_CLEANUP_CANCEL_RECOVERY")\n\t\t\t\t\t}\n\t\t\t\t})\n\t\t\t}\n'
TURNOVER_RESUME = '\t\t\tresume()\n\t\t\tawaitTimeoutQueueTest(t, done, "stale arm delivery during "+phase)\n'
TURNOVER_STUCK = '\t\t\tresume()\n\t\t\tif phase == "mid-turnover" {\n\t\t\t\tdone = nil\n\t\t\t}\n\t\t\tawaitTimeoutQueueTest(t, done, "stale arm delivery during "+phase)\n'
TURNOVER_LANES = {"mutant-generation-turnover", "mutant-generation-turnover-no-defer"}
FORBIDDEN = {"mutant-generation-turnover": ('TIMEOUT_QUEUE_WAIT: TURNOVER_CLEANUP_CANCEL_BLOCKED_WITHOUT_FINISH', 'TURNOVER_CLEANUP_UNPUBLISHED_WITHOUT_FINISH')}


def mutated(name: str, original: dict[str, bytes]) -> dict[str, bytes]:
    if name in ("generation-turnover", "generation-turnover-no-defer"):
        text = replace_one(original[GENERATION].decode(), TURNOVER_FINISH, TURNOVER_FINISH + TURNOVER_WATCHDOG)
        text = replace_one(text, TURNOVER_RESUME, TURNOVER_STUCK)
        if name == "generation-turnover-no-defer":
            text = replace_one(text, TURNOVER_FINISH, "\t\t\tfinish := sync.OnceFunc(token.finish)\n")
        return {GENERATION: text.encode()}
    if name == "generation-barrier":
        text = replace_one(original[GENERATION].decode(), GENERATION_BARRIER, GENERATION_BARRIER_MUTANT)
        return {GENERATION: text.encode()}
    if name == "no-renewal":
        text = replace_one(original[READ].decode(), RENEWAL, "\t}\n")
        text = replace_one(text, '\t"weak"\n', "")
        if "weak." in text:
            raise RuntimeError("unexpected remaining weak use after whole renewal removal")
        return {READ: text.encode()}
    text = original[QUEUE].decode()
    if name == "eager":
        text = replace_one(text, EAGER, "\tn.seq = q.root.seq.Add(1)\n" + EAGER.replace("\tn.seq = q.root.seq.Add(1)\n", ""))
    elif name == "frontier":
        text = replace_one(text, "\t\tif r.seq > frontier {", "\t\tif r.seq > root.seq.Load() {")
    elif name == "again":
        text = replace_one(text, "\tif root.running {\n\t\troot.again = true\n", "\tif root.running {\n")
    elif name == "stub-repair":
        text = replace_one(text, STUB_REPAIR, STUB_REPAIR.replace(
            "q.pushExact(", "n.seq = q.root.seq.Add(1)\n\t\t\t\t\tq.pushExact("))
    elif name == "logical-order":
        text = replace_one(text, LOGICAL_ORDER, LOGICAL_ORDER.replace("cmp.Compare(a.seq, b.seq)", "cmp.Compare(b.seq, a.seq)"))
    elif name == "cleanup":
        text = replace_one(text, "\truntime.AddCleanup(tx, func(n *timeoutNode) { n.queue.remove(n) }, n)\n", "")
    else:
        raise ValueError(f"unknown mutation: {name}")
    return {QUEUE: text.encode()}


def named_case(log: str, xml: str, name: str, outcome: str, marker: str | None, cached: bool, forbidden_markers: tuple[str, ...] = ()) -> dict[str, object]:
    """A marker in somebody else's failure, a skip, or cached red cannot prove a case."""
    runs = set(re.findall(r"^=== RUN\s+(\S+)\s*$", log, re.MULTILINE))
    results = set(re.findall(r"^\s*--- (PASS|FAIL|SKIP): (\S+)\s+\(", log, re.MULTILINE))
    cases = []
    if xml:
        for case in ET.fromstring(xml).iter("testcase"):
            if case.get("name") == name:
                failures = list(case.findall("failure")) + list(case.findall("error"))
                skipped = case.find("skipped") is not None
                xml_outcome = "SKIP" if skipped else "FAIL" if failures else "PASS"
                failure_text = "\n".join(" ".join(f.attrib.values()) + " " + "".join(f.itertext()) for f in failures)
                cases.append((xml_outcome, failure_text))
    xml_match = len(cases) == 1 and any(state == outcome and (marker is None or marker in text)
                and all(forbidden not in text for forbidden in forbidden_markers) for state, text in cases)
    proven = name in runs and (outcome, name) in results and xml_match and not (outcome == "FAIL" and cached)
    return {"name": name, "outcome": outcome, "marker": marker, "forbidden_markers": list(forbidden_markers),
            "xml_case_count": len(cases), "run_seen": name in runs,
            "result_seen": (outcome, name) in results, "xml_match": xml_match,
            "cached": cached, "proven": proven}


def turnover_population(records: list[dict[str, object]]) -> dict[str, object]:
    failures = [record for record in records if record["status"] == "FAILED"]
    statuses_ok = all(record["status"] in ("PASSED", "FAILED") for record in records)
    no_timeout = all(token not in record["log"] for record in failures
                     for token in ("panic: test timed out", "test timed out after"))
    single_actual_failure = (len(failures) == 1 and failures[0]["id"]["label"] == "//pkg/fdbgo/client:lease_alloc_unit_test"
                             and not failures[0]["cached"])
    return {"proven": statuses_ok and no_timeout and single_actual_failure,
            "all_statuses_passed_or_failed": statuses_ok, "no_global_timeout": no_timeout,
            "failures": [{"label": record["id"]["label"], "cached": record["cached"]} for record in failures]}


def finished_exit_code(finished: dict[str, object]) -> int | None:
    exit_code = finished.get("exitCode", {})
    if "code" in exit_code:
        return int(exit_code["code"])
    # Protobuf JSON omits the zero value, but only explicit SUCCESS may imply it.
    if exit_code.get("name") == "SUCCESS" and finished.get("overallSuccess") is True:
        return 0
    return None


def proof_gate(status: int, finished_code: int | None, statuses: list[str], required: list[bool], red: bool) -> tuple[bool, bool]:
    green = bool(statuses) and finished_code == 0 and status == 0 and all(s == "PASSED" for s in statuses)
    eligible = status in (1, 3) and finished_code in (1, 3) and "FAILED" in statuses if red else green
    return green, bool(required) and all(required) and eligible


def deadline_timestamp(value: str, now: dt.datetime | None = None) -> float:
    now = now or dt.datetime.now(dt.timezone.utc)
    stop = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    if stop.tzinfo is None or not 0 < (stop - now).total_seconds() <= 2 * 3600:
        raise ValueError("--deadline-utc must include timezone, be future and at most2h away")
    return stop.timestamp()


def atomic_source_write(path: Path, data: bytes, mode: int) -> None:
    temporary = path.with_name(f".{path.name}.rev6-proof-{os.getpid()}")
    created = False
    try:
        fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, mode)
        created = True
        with os.fdopen(fd, "wb") as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        temporary.chmod(mode)
        os.replace(temporary, path)
    finally:
        if created and temporary.exists():
            temporary.unlink()


class GuardTests(unittest.TestCase):
    def test_named_population_and_binding(self):
        log = "=== RUN   T\n    x.go:1: MARK\n--- FAIL: T (0.00s)\n"
        xml = '<testsuite><testcase name="T"><failure>MARK</failure></testcase></testsuite>'
        self.assertTrue(named_case(log, xml, "T", "FAIL", "MARK", False)["proven"])
        for changed in ("", log.replace("=== RUN", "=== OTHER"), log.replace("FAIL:", "PASS:")):
            self.assertFalse(named_case(changed, xml, "T", "FAIL", "MARK", False)["proven"])
        self.assertFalse(named_case(log, xml, "T", "FAIL", "MARK", True)["proven"])
        self.assertTrue(named_case(log, xml, "T", "FAIL", "MARK", False, ("ABSENT",))["proven"])
        self.assertFalse(named_case(log, xml, "T", "FAIL", "MARK", False, ("MARK",))["proven"])
        extra = '<testcase name="OTHER"><failure>FORBIDDEN</failure></testcase>'
        self.assertTrue(named_case(log, xml.replace('</testsuite>', extra + '</testsuite>'), "T", "FAIL", "MARK", False, ("FORBIDDEN",))["proven"])
        self.assertFalse(named_case(log, xml.replace("MARK", "MARK FORBIDDEN"), "T", "FAIL", "MARK", False, ("FORBIDDEN",))["proven"])
        duplicate = xml.replace('</testsuite>', '<testcase name="T"><failure>MARK</failure></testcase></testsuite>')
        self.assertFalse(named_case(log, duplicate, "T", "FAIL", "MARK", False)["proven"])
        for forbidden in FORBIDDEN["mutant-generation-turnover"]:
            self.assertFalse(named_case(log, xml.replace("MARK", "MARK " + forbidden), "T", "FAIL", "MARK", False,
                                        FORBIDDEN["mutant-generation-turnover"])["proven"])
        for changed in ("", '<testsuite/>', xml.replace('name="T"', 'name="OTHER"'), xml.replace("MARK", "OTHER")):
            self.assertFalse(named_case(log, changed, "T", "FAIL", "MARK", False)["proven"])
        with self.assertRaises(ET.ParseError):
            named_case(log, "<bad", "T", "FAIL", "MARK", False)
        passed = "=== RUN   T\n--- PASS: T (0.00s)\n"
        self.assertTrue(named_case(passed, '<testsuite><testcase name="T"/></testsuite>', "T", "PASS", None, True)["proven"])
        self.assertFalse(named_case(passed.replace("PASS", "SKIP"), '<testsuite><testcase name="T"><skipped/></testcase></testsuite>', "T", "PASS", None, False)["proven"])

    def test_verdict_vacuity_status_and_timeout(self):
        self.assertEqual(proof_gate(0, 0, ["PASSED"], [True], False), (True, True))
        population = [{"id": {"label": "//pkg/fdbgo/client:lease_alloc_unit_test"}, "status": "FAILED", "cached": False, "log": "named failure"},
                      {"id": {"label": "//pkg/other:other_test"}, "status": "PASSED", "cached": True, "log": ""}]
        self.assertTrue(turnover_population(population)["proven"])
        self.assertFalse(turnover_population([])["proven"])
        self.assertFalse(turnover_population(population + [population[0]])["proven"])
        for field, value in (("cached", True), ("status", "TIMEOUT"), ("id", {"label": "//pkg/other:other_test"}),
                             ("log", "panic: test timed out"), ("log", "test timed out after 10s")):
            changed = copy.deepcopy(population)
            changed[0][field] = value
            self.assertFalse(turnover_population(changed)["proven"])
        for status, finished, statuses, required in (
            (0, 0, [], [True]), (0, 0, ["PASSED"], []), (0, None, ["PASSED"], [True]),
            (1, 1, ["PASSED"], [True]), (0, 0, ["FAILED"], [True]), (0, 0, ["PASSED"], [False]),
        ):
            self.assertFalse(proof_gate(status, finished, statuses, required, False)[1])
        self.assertEqual(proof_gate(1, 3, ["FAILED"], [True], True), (False, True))
        self.assertEqual(proof_gate(3, 3, ["FAILED"], [True], True), (False, True))
        self.assertTrue(proof_gate(1, 1, ["FAILED", "PASSED"], [True], True)[1])
        for status, finished, statuses, required in (
            (124, 3, ["FAILED"], [True]), (1, 8, ["FAILED"], [True]), (3, 8, ["FAILED"], [True]),
            (1, 3, ["PASSED"], [True]), (3, 3, ["PASSED"], [True]),
            (1, 3, ["FAILED"], []), (1, 3, ["FAILED"], [False]), (0, 0, ["FAILED"], [True]),
        ):
            self.assertFalse(proof_gate(status, finished, statuses, required, True)[1])

    def test_finished_exit_code_zero_omission(self):
        self.assertEqual(finished_exit_code({"overallSuccess": True, "exitCode": {"name": "SUCCESS"}}), 0)
        self.assertEqual(finished_exit_code({"overallSuccess": False, "exitCode": {"name": "TESTS_FAILED", "code": 3}}), 3)
        for value in ({}, {"overallSuccess": True}, {"exitCode": {"name": "SUCCESS"}},
                      {"overallSuccess": False, "exitCode": {"name": "SUCCESS"}},
                      {"overallSuccess": True, "exitCode": {"name": "UNKNOWN"}}):
            self.assertIsNone(finished_exit_code(value))

    def test_explicit_bounded_deadline(self):
        now = dt.datetime(2026, 1, 1, tzinfo=dt.timezone.utc)
        self.assertEqual(deadline_timestamp("2026-01-01T02:00:00Z", now), now.timestamp() + 7200)
        for value in ("2026-01-01T00:00:00Z", "2026-01-01T02:00:01Z", "2026-01-01T00:10:00"):
            with self.assertRaises(ValueError):
                deadline_timestamp(value, now)

    def test_every_mutation_and_anchor_rejection(self):
        originals = {GENERATION: (GENERATION_BARRIER + TURNOVER_FINISH + TURNOVER_RESUME).encode(), READ: ('import (\n\t"weak"\n)\n' + RENEWAL).encode(), QUEUE: (
            EAGER + "\t\tif r.seq > frontier {\n" + "\tif root.running {\n\t\troot.again = true\n"
            + "\truntime.AddCleanup(tx, func(n *timeoutNode) { n.queue.remove(n) }, n)\n" + STUB_REPAIR + LOGICAL_ORDER).encode()}
        for name in EXPECTED:
            result = mutated(name, originals)
            self.assertEqual(len(result), 1)
            path, data = next(iter(result.items()))
            self.assertNotEqual(data, originals[path])
        self.assertEqual(mutated("generation-barrier", originals)[GENERATION],
                         originals[GENERATION].decode().replace(GENERATION_BARRIER, GENERATION_BARRIER_MUTANT, 1).encode())
        expected_turnover = originals[GENERATION].decode().replace(TURNOVER_FINISH, TURNOVER_FINISH + TURNOVER_WATCHDOG, 1).replace(TURNOVER_RESUME, TURNOVER_STUCK, 1)
        self.assertEqual(mutated("generation-turnover", originals)[GENERATION], expected_turnover.encode())
        self.assertEqual(mutated("generation-turnover-no-defer", originals)[GENERATION],
                         expected_turnover.replace(TURNOVER_FINISH, TURNOVER_FINISH.splitlines(True)[0], 1).encode())
        for name in ("generation-turnover", "generation-turnover-no-defer"):
            self.assertEqual(mutated(name, originals)[GENERATION].decode().count(TURNOVER_WATCHDOG), 1)
            for anchor in (TURNOVER_FINISH, TURNOVER_RESUME):
                for data in (originals[GENERATION].replace(anchor.encode(), b""), originals[GENERATION] + anchor.encode()):
                    with self.assertRaises(RuntimeError):
                        mutated(name, {GENERATION: data})
        for data in (b"", (GENERATION_BARRIER * 2).encode()):
            with self.assertRaises(RuntimeError):
                mutated("generation-barrier", {GENERATION: data})
        self.assertNotIn(b"weak", mutated("no-renewal", originals)[READ])
        self.assertTrue(mutated("eager", originals)[QUEUE].startswith(b"\tn.seq = q.root.seq.Add(1)\n\tif q.beforeEagerSchedule"))
        self.assertIn(b"r.seq > root.seq.Load()", mutated("frontier", originals)[QUEUE])
        self.assertNotIn(b"root.again = true", mutated("again", originals)[QUEUE])
        self.assertNotIn(b"AddCleanup", mutated("cleanup", originals)[QUEUE])
        self.assertIn(b"n.seq = q.root.seq.Add(1)\n\t\t\t\t\tq.pushExact(n, n.desiredWake)",
                      mutated("stub-repair", originals)[QUEUE])
        self.assertIn(b"cmp.Compare(b.seq, a.seq)", mutated("logical-order", originals)[QUEUE])
        for name, anchor in (("stub-repair", STUB_REPAIR), ("logical-order", LOGICAL_ORDER)):
            for data in (originals[QUEUE].replace(anchor.encode(), b""), originals[QUEUE] + anchor.encode()):
                with self.assertRaises(RuntimeError):
                    mutated(name, {READ: b"", QUEUE: data})
        for text in ("", "xx"):
            with self.assertRaises(RuntimeError):
                replace_one(text, "x", "y")
        with self.assertRaises(ValueError):
            mutated("unknown", originals)
        with self.assertRaises(RuntimeError):
            mutated("no-renewal", {READ: originals[READ] + b"weak.Other()", QUEUE: originals[QUEUE]})


class Campaign:
    def __init__(self, evidence: Path, deadline: float):
        self.base = evidence
        self.base.mkdir(parents=True, exist_ok=False)
        self.deadline = deadline
        self.nominal: dict[str, object] = {}
        self.info: dict[str, str] = {}
        self.sdk_identity: dict[str, object] | None = None
        self.original: dict[str, bytes] = {}
        self.expected: dict[str, object] = {}
        self.results: list[dict[str, object]] = []

    def command(self, argv: list[str], prefix: Path, bazel: bool = False) -> int:
        save_json(prefix.with_suffix(".command.json"), {"argv": argv, "cwd": str(ROOT),
                  "started": dt.datetime.now().astimezone().isoformat(), "nice": os.getpriority(os.PRIO_PROCESS, 0),
                  "affinity": sorted(os.sched_getaffinity(0))})
        actual = argv
        if bazel:
            remaining = int(self.deadline - time.time())
            if remaining <= 0:
                raise RuntimeError("Bazel prohibited at/after the explicit owner-approved deadline")
            actual = ["timeout", "--signal=TERM", "--kill-after=10s", f"{remaining}s", *argv]
        with prefix.with_suffix(".log").open("wb") as stream:
            proc = subprocess.Popen(actual, cwd=ROOT, stdout=stream, stderr=subprocess.STDOUT)
            try:
                status = proc.wait()
            except BaseException:
                proc.send_signal(signal.SIGINT)
                try:
                    proc.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    proc.terminate()
                    proc.wait(timeout=15)
                raise
        save_json(prefix.with_suffix(".exit.json"), {"exit": status, "finished": dt.datetime.now().astimezone().isoformat()})
        return status

    def git(self, *args: str) -> bytes:
        return subprocess.check_output(["git", *args], cwd=ROOT)

    def manifest(self) -> dict[str, object]:
        paths = set(self.git("ls-files", "-z", "--cached", "--others", "--exclude-standard").decode().split("\0")) - {""}
        if (ROOT / "user.bazelrc").exists():
            paths.add("user.bazelrc")
        entries = {}
        for rel in sorted(paths):
            path = ROOT / rel
            try:
                info = path.lstat()
            except FileNotFoundError:
                entries[rel] = {"kind": "missing"}
                continue
            if stat.S_ISLNK(info.st_mode):
                data, kind = os.readlink(path).encode(), "symlink"
            elif stat.S_ISREG(info.st_mode):
                data, kind = path.read_bytes(), "file"
            else:
                raise RuntimeError(f"unexpected input type: {rel}")
            entries[rel] = {"kind": kind, "mode": stat.S_IMODE(info.st_mode), "bytes": len(data), "sha256": digest(data)}
        configs = {}
        for path in (Path("/etc/bazel.bazelrc"), Path.home() / ".bazelrc"):
            configs[str(path)] = digest(path.read_bytes()) if path.is_file() else None
        return {"files": entries, "external_config_sha256": configs}

    def assert_manifest(self, expected: dict[str, object]) -> None:
        actual = self.manifest()
        if actual != expected:
            save_json(self.base / "unexpected-input-manifest.json", actual)
            raise RuntimeError("source/config drift; frozen epoch invalid (see unexpected-input-manifest.json)")

    def freeze(self) -> None:
        self.nominal = self.manifest()
        self.expected = self.nominal
        save_json(self.base / "nominal-input-tree.json", self.nominal)
        save_json(self.base / "epoch.json", {
            "mode_script": str(Path(__file__).resolve()), "script_sha256": digest(Path(__file__).read_bytes()),
            "frozen_utc": dt.datetime.now(dt.timezone.utc).isoformat(), "deadline_utc": dt.datetime.fromtimestamp(self.deadline, dt.timezone.utc).isoformat(),
            "cwd": str(ROOT), "python": sys.version, "machine": list(os.uname()),
            "nice": os.getpriority(os.PRIO_PROCESS, 0), "affinity": sorted(os.sched_getaffinity(0)),
            "environment": {key: os.environ.get(key) for key in ("PATH", "HOME", "GOMAXPROCS", "GOGC", "GOMEMLIMIT", "GODEBUG", "GOENV", "GOTOOLCHAIN", "BAZELISK_HOME", "USE_BAZEL_VERSION")},
        })
        (self.base / "git-head.txt").write_bytes(self.git("rev-parse", "HEAD"))
        (self.base / "git-status.z").write_bytes(self.git("status", "--porcelain=v1", "-z"))
        (self.base / "git-diff.patch").write_bytes(self.git("diff", "--binary", "HEAD"))
        with tarfile.open(self.base / "nominal-source.tar.gz", "w:gz", compresslevel=1) as archive:
            for rel, entry in self.nominal["files"].items():
                if entry["kind"] != "missing":
                    archive.add(ROOT / rel, arcname=rel, recursive=False)
        for rel in (READ, QUEUE, GENERATION, REGRESSION, "pkg/fdbgo/client/BUILD.bazel"):
            if rel not in self.nominal["files"] or not (ROOT / rel).is_file():
                raise RuntimeError(f"missing critical frozen input: {rel}")
        self.assert_manifest(self.nominal)
        self.original = {rel: (ROOT / rel).read_bytes() for rel in (READ, QUEUE, GENERATION)}
        for rel, data in self.original.items():
            if digest(data) != self.nominal["files"][rel]["sha256"]:
                raise RuntimeError(f"source changed while freezing: {rel}")

    def sdk(self) -> dict[str, object]:
        external = Path(self.info["execution_root"]) / "external"
        pinned = re.search(r"^toolchain\s+(\S+)$", (ROOT / "go.mod").read_text(), re.MULTILINE)
        if not pinned:
            raise RuntimeError("missing pinned Go toolchain")
        candidates = [p for p in external.glob("rules_go*go_sdk*/VERSION") if p.read_text().splitlines()[0] == pinned.group(1)]
        if len(candidates) != 1:
            raise RuntimeError(f"pinned SDK population expected1, found{len(candidates)}")
        sdk = candidates[0].parent.resolve()
        files = {}
        for rel in ("VERSION", "bin/go", "pkg/tool/linux_amd64/compile", "pkg/tool/linux_amd64/link", "src/weak/pointer.go", "src/runtime/mcleanup.go"):
            path = sdk / rel
            if not path.is_file():
                raise RuntimeError(f"missing actual Bazel SDK file: {path}")
            files[rel] = digest(path.read_bytes())
        return {"root": str(sdk), "version": candidates[0].read_text(), "sha256": files}

    def restart_server(self) -> None:
        if self.command(["bazelisk", "shutdown"], self.base / "server-shutdown", bazel=True):
            raise RuntimeError("Bazel shutdown failed; no test command started")
        if self.command(["bazelisk", "info"], self.base / "server-info", bazel=True):
            raise RuntimeError("Bazel info failed")
        for line in (self.base / "server-info.log").read_text().splitlines():
            if ": " in line:
                key, value = line.split(": ", 1)
                self.info[key] = value
        for key in ("execution_root", "output_base", "server_pid"):
            if key not in self.info:
                raise RuntimeError(f"Bazel info missing {key}")
        pid = int(self.info["server_pid"])
        policy = {"server_pid": pid, "nice": os.getpriority(os.PRIO_PROCESS, pid), "affinity": sorted(os.sched_getaffinity(pid))}
        save_json(self.base / "server-resources.json", policy)
        if policy["nice"] != 19 or set(policy["affinity"]) != CPUS:
            raise RuntimeError("Bazel server did not inherit required nice19/CPU mask")

    def artifacts(self, bep: Path, stage: Path) -> tuple[list[dict[str, object]], int | None]:
        records, finished_code = [], None
        for line in bep.read_text().splitlines():
            event = json.loads(line)
            if "finished" in event:
                finished_code = finished_exit_code(event["finished"])
            result = event.get("testResult")
            if result is None:
                continue
            ident = event["id"]["testResult"]
            cached = bool(result.get("cachedLocally") or result.get("cachedRemotely") or result.get("executionInfo", {}).get("cachedRemotely"))
            record = {"id": ident, "status": result.get("status"), "cached": cached, "log": "", "xml": ""}
            label = ident["label"]
            dest = stage / "testlogs" / (digest(json.dumps(ident, sort_keys=True).encode())[:16])
            dest.mkdir(parents=True)
            for output in result.get("testActionOutput", []):
                name = output.get("name")
                if name not in ("test.log", "test.xml"):
                    continue
                uri = urlparse(output.get("uri", ""))
                if uri.scheme != "file":
                    continue
                source = Path(unquote(uri.path)).resolve()
                if not source.is_relative_to(Path(self.info["output_base"]).resolve()):
                    raise RuntimeError(f"test artifact outside Bazel output base: {source}")
                data = source.read_bytes()
                (dest / name).write_bytes(data)
                record["log" if name == "test.log" else "xml"] = data.decode("utf-8", errors="replace")
            save_json(dest / "metadata.json", {**ident, "status": record["status"], "cached": cached})
            record["artifact_dir"] = str(dest)
            evidence_tests = set(CRITICAL) | {name for cases in EXPECTED.values() for name, _ in cases}
            runs = set(re.findall(r"^=== RUN\s+(\S+)\s*$", record["log"], re.MULTILINE))
            if label.startswith("//pkg/fdbgo/client:") and runs & evidence_tests:
                package, target = label[2:].split(":")
                binary = ROOT / "bazel-bin" / package / (target + "_") / target
                if binary.is_file():
                    data = binary.read_bytes()
                    (dest / "test-binary").write_bytes(data)
                    record["binary_sha256"] = digest(data)
            records.append(record)
        return records, finished_code

    def lane(self, name: str, race: bool, requirements: list[tuple[str, str | None]], red: bool = False) -> dict[str, object]:
        stage = self.base / name
        stage.mkdir()
        expected = self.expected
        self.assert_manifest(expected)
        save_json(stage / "input-tree.json", expected)
        argv = ["just", "test", "--keep_going", "--test_output=all", "--test_arg=-test.v", "--jobs=4", "--local_resources=cpu=4",
                "--compilation_mode=opt", "--flaky_test_attempts=1", "--cache_test_results=auto",
                f"--build_event_json_file={stage / 'bep.jsonl'}", f"--@rules_go//go/config:race={'true' if race else 'false'}"]
        status = self.command(argv, stage / "whole-fast-lane", bazel=True)
        self.assert_manifest(expected)
        sdk = self.sdk()
        save_json(stage / "actual-bazel-sdk.json", sdk)
        if self.sdk_identity is not None and sdk != self.sdk_identity:
            raise RuntimeError("Bazel SDK changed within frozen epoch")
        self.sdk_identity = sdk
        bep = stage / "bep.jsonl"
        if not bep.is_file():
            raise RuntimeError(f"missing BEP: {name}; no proof")
        records, finished_code = self.artifacts(bep, stage)
        rows = []
        for test, marker in requirements:
            candidates = []
            for record in records:
                if not record["id"]["label"].startswith("//pkg/fdbgo/client:"):
                    continue
                wanted = "FAILED" if red else "PASSED"
                if record["status"] != wanted:
                    continue
                row = named_case(record["log"], record["xml"], test, "FAIL" if red else "PASS", marker, record["cached"], FORBIDDEN.get(name, ()))
                row["label"] = record["id"]["label"]
                row["artifact_dir"] = record["artifact_dir"]
                row["binary_sha256"] = record.get("binary_sha256")
                row["proven"] = row["proven"] and bool(row["binary_sha256"])
                candidates.append(row)
            rows.append({"test": test, "marker": marker, "proven": any(c["proven"] for c in candidates), "candidates": candidates})
        all_green, proven = proof_gate(status, finished_code, [r["status"] for r in records], [row["proven"] for row in rows], red)
        comparison = turnover_population(records) if name in TURNOVER_LANES else None
        proven = proven and (comparison is None or comparison["proven"])
        verdict = {"stage": name, "exit": status, "whole_lane_green": all_green, "named_proof": proven,
                   "turnover_comparison": comparison,
                   "finished_bep_exit": finished_code, "test_results": len(records),
                   "actual_results": sum(not r["cached"] for r in records), "cached_results": sum(r["cached"] for r in records),
                   "statuses": [{"id": r["id"], "status": r["status"], "cached": r["cached"]} for r in records], "required": rows}
        save_json(stage / "verdict.json", verdict)
        self.results.append(verdict)
        save_json(self.base / "results.json", self.results)
        if not proven:
            raise RuntimeError(f"{name}: required named proof missing; see verdict.json (exit alone is not evidence)")
        return verdict

    @contextlib.contextmanager
    def mutation(self, name: str):
        self.assert_manifest(self.nominal)
        changes = mutated(name, self.original)
        backup = self.base / ("mutation-" + name + "-source")
        backup.mkdir()
        modes = {rel: stat.S_IMODE((ROOT / rel).stat().st_mode) for rel in changes}
        installed = []
        try:
            for rel, data in changes.items():
                dest = backup / rel
                dest.parent.mkdir(parents=True, exist_ok=True)
                dest.with_suffix(dest.suffix + ".nominal").write_bytes(self.original[rel])
                dest.with_suffix(dest.suffix + ".mutant").write_bytes(data)
                installed.append(rel)
                atomic_source_write(ROOT / rel, data, modes[rel])
            patch = "".join("".join(difflib.unified_diff(self.original[rel].decode().splitlines(True), data.decode().splitlines(True), fromfile="a/" + rel, tofile="b/" + rel, n=0)) for rel, data in changes.items())
            (backup / "applied.patch").write_text(patch)
            expected = copy.deepcopy(self.nominal)
            for rel, data in changes.items():
                expected["files"][rel]["sha256"] = digest(data)
                expected["files"][rel]["bytes"] = len(data)
            self.expected = expected
            self.assert_manifest(expected)
            yield
        finally:
            errors = []
            for rel in installed:
                current = (ROOT / rel).read_bytes()
                if current not in (changes[rel], self.original[rel]):
                    (backup / (Path(rel).name + ".unexpected-concurrent-bytes")).write_bytes(current)
                    errors.append(f"concurrent edit to {rel}; not overwriting another writer")
                    continue
                atomic_source_write(ROOT / rel, self.original[rel], modes[rel])
            if errors:
                raise RuntimeError("; ".join(errors))
            self.expected = self.nominal
            self.assert_manifest(self.nominal)
            save_json(backup / "restored.json", {"exact_nominal_restored": True})


@contextlib.contextmanager
def optional_lock(enabled: bool, deadline: float, lock_path: Path | None = None):
    if not enabled:
        yield
        return
    if lock_path is None:
        raise RuntimeError("--lock requires --lock-file or RFC263_FAST_LOCK")
    with lock_path.open("a+") as lock:
        while True:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                if time.time() >= deadline:
                    raise RuntimeError("configured owner lock unavailable before explicit deadline; no Bazel command run")
                time.sleep(0.25)
        yield


def interrupt(signum, _frame):
    raise InterruptedError(f"signal {signum}; stopping child and restoring mutation")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--evidence", required=True, type=Path)
    parser.add_argument("--mode", required=True, choices=("race", "opt", "mutations"))
    parser.add_argument("--deadline-utc", required=True, help="owner-approved absolute ISO8601 deadline, future and <=2h away")
    parser.add_argument("--lock", action="store_true", help="also acquire the configured owner lock; availability is not authorization")
    parser.add_argument("--lock-file", type=Path, default=os.environ.get("RFC263_FAST_LOCK") or None)
    parser.add_argument("--optional-mutations", nargs="*", choices=("eager", "frontier", "again", "cleanup", "stub-repair", "logical-order", "generation-barrier", "generation-turnover", "generation-turnover-no-defer"), default=[])
    args = parser.parse_args()
    try:
        deadline = deadline_timestamp(args.deadline_utc)
    except ValueError as exc:
        parser.error(str(exc))
    if args.lock and args.lock_file is None:
        parser.error("--lock requires --lock-file or RFC263_FAST_LOCK")
    if args.lock_file is not None:
        args.lock_file = args.lock_file.resolve()
    if Path.cwd().resolve() != ROOT or Path(__file__).resolve() != ROOT / "tools/benchcampaign/campaigns/rfc-263/rev6-production-proofs.py":
        parser.error(f"run the canonical script from its checkout root {ROOT}")
    evidence = args.evidence.resolve()
    if evidence == ROOT or evidence.is_relative_to(ROOT):
        parser.error("--evidence must be outside the source tree")
    if args.optional_mutations and args.mode != "mutations":
        parser.error("--optional-mutations requires --mode mutations")
    os.sched_setaffinity(0, CPUS)
    os.setpriority(os.PRIO_PROCESS, 0, 19)
    if os.getpriority(os.PRIO_PROCESS, 0) != 19 or os.sched_getaffinity(0) != CPUS:
        parser.error("could not enforce nice19 and required CPU mask")
    for sig in (signal.SIGTERM, signal.SIGINT):
        signal.signal(sig, interrupt)
    evidence.mkdir(parents=True, exist_ok=True)
    epoch = evidence / (dt.datetime.now(dt.timezone.utc).strftime("rev6-production-%Y%m%dT%H%M%S.%fZ-") + args.mode)
    campaign = Campaign(epoch, deadline)
    with (epoch / "guard-unit-tests.log").open("w") as log:
        result = unittest.TextTestRunner(stream=log, verbosity=2).run(unittest.defaultTestLoader.loadTestsFromTestCase(GuardTests))
    if not result.wasSuccessful() or result.testsRun != 5:
        raise RuntimeError("guard helper tests did not pass nonempty population")
    with optional_lock(args.lock, campaign.deadline, args.lock_file):
        try:
            campaign.freeze()
            campaign.restart_server()
            required = [(test, None) for test in CRITICAL]
            if args.mode == "mutations":
                campaign.lane("nominal-race", True, required)
                campaign.assert_manifest(campaign.nominal)
                campaign.lane("nominal-opt", False, required)
                for name in ["no-renewal", *dict.fromkeys(args.optional_mutations)]:
                    with campaign.mutation(name):
                        campaign.lane("mutant-" + name, False, EXPECTED[name], red=True)
                    campaign.lane("restored-" + name + "-opt", False, required)
                campaign.lane("restored-opt", False, required)
            else:
                campaign.lane("nominal-" + args.mode, args.mode == "race", required)
            campaign.assert_manifest(campaign.nominal)
            save_json(epoch / "complete.json", {"mode": args.mode, "exact_nominal_restored": True, "stages": len(campaign.results)})
            return 0
        except BaseException:
            (epoch / "error.txt").write_text(traceback.format_exc())
            raise
        finally:
            files = {}
            for path in sorted(epoch.rglob("*")):
                if path.is_file() and path.name != "artifact-sha256.json":
                    files[str(path.relative_to(epoch))] = digest(path.read_bytes())
            save_json(epoch / "artifact-sha256.json", files)


if __name__ == "__main__":
    sys.exit(main())
