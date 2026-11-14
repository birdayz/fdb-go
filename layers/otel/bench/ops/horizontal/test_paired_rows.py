#!/usr/bin/env python3
"""Fake/local-only row-ledger guards. No producers, cloud, or paid resources."""
from concurrent.futures import ProcessPoolExecutor
from contextlib import redirect_stderr, redirect_stdout
import io
import json
import multiprocessing
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import paired_rows as rows


def concurrent_issue(path, number):
    try:
        receipt = rows.issue(path, campaign="parallel", stage="pairs3", arm="go_pure", repeat=number,
                             phase="measured", rows=3000000)
        return "issued", receipt["issue_id"]
    except rows.RowBudgetError:
        return "refused", None


class PairedRowsTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name) / "paired-rows.jsonl"

    def init(self):
        return rows.init(self.path)

    def issue(self, **changes):
        arguments = {"campaign": "test-campaign", "stage": "local", "arm": "go_pure",
                     "repeat": 0, "phase": "local", "rows": 100}
        arguments.update(changes)
        return rows.issue(self.path, **arguments)

    def rewrite_with_witness(self, records):
        """Simulate corruption independently of the suffix/byte-count witness."""
        payload = b"".join(rows._canonical(record) + b"\n" for record in records)
        self.path.write_bytes(payload)
        head = records[-1]["record_sha256"] if records else rows.ZERO_HASH
        os.setxattr(self.path, rows.HEAD_XATTR, rows._canonical({"bytes": len(payload), "head_sha256": head}))

    def records(self):
        return [json.loads(line) for line in self.path.read_text().splitlines()]

    def test_init_has_fixed_named_budget_zero_prior_and_private_file(self):
        result = self.init()
        self.assertEqual(result["budget_name"], "otel_paired_go_rust_v1")
        self.assertEqual(result["ceiling_rows"], 15000000)
        self.assertEqual(result["prior_rows"], 0)
        self.assertEqual(result["issued_rows"], 0)
        self.assertEqual(result["remaining_rows"], 15000000)
        self.assertEqual(result["record_count"], 1)
        self.assertEqual(result["issues"], [])
        self.assertEqual(self.path.stat().st_mode & 0o777, 0o600)
        self.assertEqual(rows.inspect(self.path), result)
        with self.assertRaises(rows.RowBudgetError):
            self.init()
        self.assertEqual(rows.inspect(self.path), result)

    def test_issue_is_durable_and_append_only_before_return(self):
        self.init()
        before = self.path.read_bytes()
        receipt = self.issue(rows=12345)
        self.assertTrue(self.path.read_bytes().startswith(before))
        self.assertEqual(receipt["rows"], 12345)
        self.assertEqual(receipt["issued_rows_total"], 12345)
        self.assertEqual(receipt["record_type"], "issue")
        result = rows.inspect(self.path)
        self.assertEqual(result["issued_rows"], 12345)
        self.assertEqual(result["pending_issue_ids"], [receipt["issue_id"]])
        self.assertEqual(result["head_sha256"], receipt["record_sha256"])
        self.assertEqual(result["issues"], [receipt])

    def test_failed_aborted_unknown_and_passed_never_reclaim(self):
        self.init()
        for repeat, status in enumerate(rows.STATUSES):
            issued = self.issue(repeat=repeat)
            before = self.path.read_bytes()
            outcome = rows.outcome(self.path, issue_id=issued["issue_id"], status=status)
            self.assertTrue(self.path.read_bytes().startswith(before))
            self.assertEqual(outcome["issued_rows_total"], (repeat + 1) * 100)
            self.assertEqual(outcome["status"], status)
        result = rows.inspect(self.path)
        self.assertEqual(result["issued_rows"], 400)
        self.assertEqual(result["remaining_rows"], 14999600)
        self.assertEqual(result["pending_issue_ids"], [])
        self.assertEqual(len(result["outcomes"]), 4)

    def test_population_token_prevents_duplicate_rerelease_even_after_failure(self):
        self.init()
        issued = self.issue()
        rows.outcome(self.path, issue_id=issued["issue_id"], status="failed")
        before = self.path.read_bytes()
        for count in (100, 1, 1000000):
            with self.subTest(count=count), self.assertRaises(rows.RowBudgetError):
                self.issue(rows=count)
            self.assertEqual(self.path.read_bytes(), before)

    def test_token_fields_distinguish_campaign_stage_arm_repeat_phase(self):
        self.init()
        base = {"stage": "pilot", "phase": "prerequisite"}
        receipts = [self.issue(**base)]
        for changes in ({"campaign": "another"}, {"stage": "pairs1"}, {"arm": "go_native"},
                        {"arm": "rust_native"}, {"repeat": 1}, {"phase": "measured"}):
            arguments = dict(base)
            arguments.update(changes)
            receipts.append(self.issue(**arguments))
        self.assertEqual(len({item["issue_id"] for item in receipts}), 7)
        self.assertEqual(rows.inspect(self.path)["issued_rows"], 700)

    def test_ceiling_exact_accepts_next_row_refuses_without_append(self):
        self.init()
        self.issue(rows=14999999)
        self.issue(repeat=1, rows=1)
        before = self.path.read_bytes()
        self.assertEqual(rows.inspect(self.path)["remaining_rows"], 0)
        with self.assertRaises(rows.RowBudgetError):
            self.issue(repeat=2, rows=1)
        self.assertEqual(self.path.read_bytes(), before)

    def test_bad_rows_repeat_stage_arm_phase_and_campaign_refused(self):
        self.init()
        cases = [{"rows": value} for value in (0, -1, True, False, None, 1.0, "1", 15000001, float("inf"))]
        cases += [{"repeat": value} for value in (-1, True, 1.0, "1", 1000001)]
        cases += [{"stage": value} for value in (0, "Stage0", "unknown", None, True)]
        cases += [{"arm": value} for value in ("go", "rust", "unknown", True)]
        cases += [{"phase": value} for value in ("measured", "unknown", True)]
        cases += [{"stage": "pilot", "phase": "local"}]
        cases += [{"campaign": value} for value in ("", "../escape", "spaces here", "x" * 129, True, None)]
        before = self.path.read_bytes()
        for changes in cases:
            with self.subTest(changes=changes), self.assertRaises(rows.RowBudgetError):
                self.issue(**changes)
            self.assertEqual(self.path.read_bytes(), before)

    def test_unknown_duplicate_or_invalid_outcome_refused(self):
        self.init()
        receipt = self.issue()
        for identity, status in (("a" * 64, "passed"), (True, "passed"), ("invalid", "passed"),
                                 (receipt["issue_id"], "PASSED"), (receipt["issue_id"], True)):
            with self.subTest(identity=identity, status=status), self.assertRaises(rows.RowBudgetError):
                rows.outcome(self.path, issue_id=identity, status=status)
        rows.outcome(self.path, issue_id=receipt["issue_id"], status="aborted")
        before = self.path.read_bytes()
        with self.assertRaises(rows.RowBudgetError):
            rows.outcome(self.path, issue_id=receipt["issue_id"], status="passed")
        self.assertEqual(self.path.read_bytes(), before)

    def test_no_implicit_initialization_or_caller_prior(self):
        with self.assertRaises(rows.RowBudgetError):
            rows.inspect(self.path)
        with self.assertRaises(rows.RowBudgetError):
            self.issue()
        self.init()
        with self.assertRaises(TypeError):
            self.issue(prior_rows=0)
        self.assertEqual(rows.inspect(self.path)["issued_rows"], 0)

    def test_partial_record_and_valid_whole_record_suffix_truncation_refused(self):
        self.init()
        self.issue()
        pristine = self.path.read_bytes()
        for payload in (b"", pristine[:-1], pristine[:-20], pristine.splitlines(keepends=True)[0]):
            with self.subTest(length=len(payload)):
                self.path.write_bytes(payload)
                with self.assertRaises(rows.RowBudgetError):
                    rows.inspect(self.path)
                with self.assertRaises(rows.RowBudgetError):
                    self.issue(repeat=1)
        self.path.write_bytes(pristine)
        self.assertEqual(rows.inspect(self.path)["issued_rows"], 100)

    def test_missing_witness_is_not_accepted_as_new_ledger(self):
        self.init()
        os.removexattr(self.path, rows.HEAD_XATTR)
        with self.assertRaisesRegex(rows.RowBudgetError, "witness"):
            rows.inspect(self.path)

    def test_raw_duplicate_keys_blank_lines_nonfinite_or_noncanonical_refused(self):
        self.init()
        original = self.path.read_bytes()
        for payload in (b'{"record_type":"init","record_type":"issue"}\n', b"\n", b'{"x":NaN}\n',
                        b'{"x":Infinity}\n', b"\xff\n", b"[]\n", b"{}", b" " + original):
            with self.subTest(payload=payload[:40]):
                self.path.write_bytes(payload)
                os.setxattr(self.path, rows.HEAD_XATTR,
                            rows._canonical({"bytes": len(payload), "head_sha256": "a" * 64}))
                with self.assertRaises(rows.RowBudgetError):
                    rows.inspect(self.path)

    def test_corrupt_chain_sequence_hash_scope_totals_or_fields_refused(self):
        self.init()
        self.issue()
        good = self.records()
        cases = ((0, "prior_rows", 100), (0, "prior_rows", False), (0, "ceiling_rows", 16000000),
                 (0, "budget_name", "old_native_family"), (0, "schema", "other"),
                 (1, "sequence", 0), (1, "sequence", True), (1, "previous_sha256", "a" * 64),
                 (1, "record_sha256", "a" * 64), (1, "issued_rows_total", 0),
                 (1, "issue_id", "a" * 64), (1, "rows", True), (1, "rows", -1),
                 (1, "unknown", 1), (1, "record_type", "refund"))
        for index, key, value in cases:
            records = json.loads(json.dumps(good))
            records[index][key] = value
            # Re-seal the entire chain, so semantic checks do not rely on hashes.
            for number, record in enumerate(records):
                if number and not (key == "previous_sha256" and number == index):
                    record["previous_sha256"] = records[number - 1]["record_sha256"]
                if key != "record_sha256":
                    record["record_sha256"] = rows._digest({k: v for k, v in record.items() if k != "record_sha256"})
            self.rewrite_with_witness(records)
            with self.subTest(index=index, key=key), self.assertRaises(rows.RowBudgetError):
                rows.inspect(self.path)

    def test_duplicate_issue_or_outcome_record_rejected_even_with_valid_chain(self):
        self.init()
        receipt = self.issue()
        records = self.records()
        duplicate = dict(records[-1])
        duplicate["sequence"] = 2
        duplicate["previous_sha256"] = records[-1]["record_sha256"]
        duplicate["issued_rows_total"] = 200
        duplicate["record_sha256"] = rows._digest({k: v for k, v in duplicate.items() if k != "record_sha256"})
        self.rewrite_with_witness(records + [duplicate])
        with self.assertRaises(rows.RowBudgetError):
            rows.inspect(self.path)
        self.rewrite_with_witness(records)
        rows.outcome(self.path, issue_id=receipt["issue_id"], status="failed")
        records = self.records()
        duplicate = dict(records[-1])
        duplicate["sequence"] += 1
        duplicate["previous_sha256"] = records[-1]["record_sha256"]
        duplicate["record_sha256"] = rows._digest({k: v for k, v in duplicate.items() if k != "record_sha256"})
        self.rewrite_with_witness(records + [duplicate])
        with self.assertRaises(rows.RowBudgetError):
            rows.inspect(self.path)

    def test_owner_permissions_parent_permissions_hardlinks_and_symlinks(self):
        self.init()
        for mode in (0o644, 0o640, 0o660, 0o400, 0o700):
            self.path.chmod(mode)
            with self.subTest(mode=mode), self.assertRaises(rows.RowBudgetError):
                rows.inspect(self.path)
        self.path.chmod(0o600)
        link = self.path.with_name("symlink.jsonl")
        link.symlink_to(self.path)
        with self.assertRaises(rows.RowBudgetError):
            rows.inspect(link)
        hard = self.path.with_name("hard.jsonl")
        os.link(self.path, hard)
        with self.assertRaises(rows.RowBudgetError):
            rows.inspect(self.path)
        hard.unlink()
        parent = self.path.parent
        parent.chmod(0o755)
        try:
            with self.assertRaises(rows.RowBudgetError):
                rows.inspect(self.path)
        finally:
            parent.chmod(0o700)
        with tempfile.TemporaryDirectory() as other:
            ancestor = Path(other) / "alias"
            ancestor.symlink_to(parent, target_is_directory=True)
            with self.assertRaises(rows.RowBudgetError):
                rows.inspect(ancestor / self.path.name)

    def test_nonregular_relative_and_noncanonical_paths_refused(self):
        for path in ("relative.jsonl", "/tmp/../tmp/rows.jsonl", "/tmp//rows.jsonl", None, b"/tmp/rows"):
            with self.subTest(path=path), self.assertRaises(rows.RowBudgetError):
                rows.init(path)
        os.mkfifo(self.path, 0o600)
        with self.assertRaises(rows.RowBudgetError):
            rows.inspect(self.path)
        self.path.unlink()
        self.path.mkdir(mode=0o700)
        with self.assertRaises(rows.RowBudgetError):
            rows.inspect(self.path)

    def test_fsync_issue_bytes_then_witness_before_receipt_return(self):
        self.init()
        events = []
        real_write, real_fsync, real_setxattr = os.write, os.fsync, os.setxattr
        def write(fd, payload):
            events.append("write")
            return real_write(fd, payload)
        def sync(fd):
            events.append("fsync")
            return real_fsync(fd)
        def witness(fd, key, value):
            events.append("witness")
            return real_setxattr(fd, key, value)
        with mock.patch.object(rows.os, "write", side_effect=write), \
                mock.patch.object(rows.os, "fsync", side_effect=sync), \
                mock.patch.object(rows.os, "setxattr", side_effect=witness):
            self.issue()
        self.assertEqual(events, ["write", "fsync", "witness", "fsync"])
        self.assertEqual(rows.inspect(self.path)["issued_rows"], 100)

    def test_append_or_witness_failure_never_returns_authorization(self):
        self.init()
        with mock.patch.object(rows.os, "fsync", side_effect=OSError("injected fsync failure")):
            with self.assertRaises(rows.RowBudgetError):
                self.issue()
        # Bytes may have been written; no caller is authorized, no automatic reset.
        with self.assertRaises(rows.RowBudgetError):
            rows.inspect(self.path)
        with self.assertRaises(rows.RowBudgetError):
            self.issue()

    def test_short_write_quarantines_partial_tail(self):
        self.init()
        real_write = os.write
        def partial(fd, data):
            return real_write(fd, data[:10])
        with mock.patch.object(rows.os, "write", side_effect=partial):
            with self.assertRaises(rows.RowBudgetError):
                self.issue()
        with self.assertRaises(rows.RowBudgetError):
            rows.inspect(self.path)

    def test_flock_serializes_concurrent_issues_without_budget_overrun(self):
        self.init()
        with ProcessPoolExecutor(max_workers=8, mp_context=multiprocessing.get_context("fork")) as executor:
            futures = [executor.submit(concurrent_issue, str(self.path), number) for number in range(8)]
            results = [future.result(timeout=30) for future in futures]
        self.assertEqual(sum(status == "issued" for status, _ in results), 5)
        self.assertEqual(sum(status == "refused" for status, _ in results), 3)
        summary = rows.inspect(self.path)
        self.assertEqual(summary["issued_rows"], 15000000)
        self.assertEqual(summary["record_count"], 6)
        self.assertEqual(len(summary["pending_issue_ids"]), 5)

    def cli(self, arguments):
        stdout, stderr = io.StringIO(), io.StringIO()
        with redirect_stdout(stdout), redirect_stderr(stderr):
            result = rows.main(arguments)
        return result, stdout.getvalue(), stderr.getvalue()

    def test_stage0_cli_init_issue_outcome_show_and_duplicate_exit(self):
        code, output, error = self.cli(["init", str(self.path)])
        self.assertEqual((code, error), (0, ""))
        self.assertEqual(json.loads(output)["issued_rows"], 0)
        command = ["issue", str(self.path), "--campaign", "stage0", "--stage", "local", "--arm", "go_native",
                   "--repeat", "0", "--phase", "local", "--rows", "275000"]
        code, output, error = self.cli(command)
        self.assertEqual((code, error), (0, ""))
        issued = json.loads(output)
        self.assertEqual(issued["rows"], 275000)
        code, output, error = self.cli(["outcome", str(self.path), "--issue-id", issued["issue_id"], "--status", "passed"])
        self.assertEqual((code, error), (0, ""))
        code, output, error = self.cli(["show", str(self.path)])
        self.assertEqual((code, error), (0, ""))
        self.assertEqual(json.loads(output)["issued_rows"], 275000)
        code, output, error = self.cli(command)
        self.assertEqual(code, 2)
        self.assertEqual(output, "")
        self.assertIn("rerelease forbidden", error)


if __name__ == "__main__":
    unittest.main()
