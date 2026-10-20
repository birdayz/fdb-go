#!/usr/bin/env python3
"""Offline fixtures only: no retained campaign data or estimators are required."""
import csv
import hashlib
import io
import json
from pathlib import Path
import tempfile
import unittest

import export_public_results as exporter


class ExportTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.base = Path(self.temporary.name)
        self.home = self.base / "archive"
        self.home.mkdir()
        self.campaign = self.home / exporter.CAMPAIGN
        self.analysis = self.campaign / exporter.ANALYSIS
        self.manifest = self.base / "inputs.sha256"
        self.redactions = exporter.CAMPAIGN + "/redactions.json"
        self.put(self.home / self.redactions, {"identifiers": ["private-id"], "names": ["private-name"]})
        comparison = {"metric": "mean_ns", "numerator": "final", "denominator": "before", "ratio": 0.8123456789, "ci95_low": 0.7, "ci95_high": 0.9}
        historical = {"metric": "mean_ns", "current_numerator": "final", "current_denominator": "before", "historical": comparison, "current": comparison}
        names = ("get100", "get1000", "grv", "range100", "set100", "batchset50", "batchset50x1000")
        for phase, clean_count in (("M1w1", 4), ("M1w8", 5), ("M1w64", 5)):
            workloads = {}
            for index, name in enumerate(names):
                if index < clean_count:
                    workloads[name] = {"status": "clean", "comparisons": [comparison] * 36, "historical": {"records": [historical]}}
                    path = self.analysis / phase / name / "denom-cgo/absolute.csv"
                    path.parent.mkdir(parents=True, exist_ok=True)
                    path.write_text("arm,mean_ns\nbefore,11\nfinal,12\ncgo,13\n", encoding="utf-8")
                else:
                    workloads[name] = {"status": "N/A", "r4_verdict": "contaminated"}
            self.put(self.analysis / phase / "summary.json", {"workloads": workloads})
        for phase, count in (("M2rl", 9), ("M2skip", 3)):
            cases, comparisons = {}, []
            for index in range(count):
                name = "case-" + str(index)
                record = {"gm": 1.23456789, "nominal_95_interval": [1.1, 1.4]}
                cases[name] = {"status": "ok", "placement_fraction": 1.0,
                               "absolutes": {"final-remote": {"ns/op": {"minimum": 7, "maximum": 17}}},
                               "historical": {"historical_placement": {"placement_fraction": 0.75},
                                              "records": [{"metric": "ns/op", "current": record, "historical": record}]}}
                comparisons.extend({"case": name, "metric": metric, **record} for metric in ("ns/op", "B/op", "allocs/op", "rows/op", "prefix-B"))
            self.put(self.analysis / phase / "report.json", {"cases": cases, "comparisons": comparisons})
        self.put(self.campaign / "sealed/provenance/source-receipt.json", {"production_revisions": {"before": "previous", "final": "current"}})
        self.put(self.campaign / "execution/analyze_campaign.py", {"retained_source": True})
        self.put(self.analysis / "summary.json", {"retained_summary": True})
        self.pin()

    @staticmethod
    def put(path, value):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(value, sort_keys=True) + "\n", encoding="utf-8")

    def pin(self):
        self.manifest.write_text("".join(
            hashlib.sha256(path.read_bytes()).hexdigest() + "  " + path.relative_to(self.home).as_posix() + "\n"
            for path in sorted(self.home.rglob("*")) if path.is_file()), encoding="utf-8")

    def export(self, name):
        output = self.base / name
        hashes = exporter.export_results(self.home, self.manifest, self.redactions, output)
        return output, hashes

    def test_repeatable_eight_files_and_unchanged_stored_values(self):
        first, hashes = self.export("first")
        second, repeated = self.export("second")
        self.assertEqual(hashes, repeated)
        self.assertEqual(set(hashes), set(exporter.AGGREGATE_FILES))
        for name in hashes:
            self.assertEqual((first / name).read_bytes(), (second / name).read_bytes())
        rows = list(csv.DictReader(io.StringIO((first / "analysis/M1-comparisons.csv").read_text())))
        self.assertEqual(len(rows), 504)
        self.assertEqual(rows[0]["ratio"], "0.8123456789")
        m2 = list(csv.DictReader(io.StringIO((first / "analysis/M2-comparisons.csv").read_text())))
        self.assertEqual(len(m2), 60)
        self.assertEqual(m2[0]["nominal_95_interval"], "[1.1,1.4]")
        self.assertEqual({row["metric_role"] for row in m2}, {"audit", "performance"})
        method = json.loads((first / "analysis/method.json").read_text())
        self.assertEqual(method["seed"], 492817)
        self.assertEqual(method["whole_paired_block_resamples"], 20000)

    def test_input_tamper_refused_before_output(self):
        path = self.analysis / "M1w1/summary.json"
        path.write_bytes(path.read_bytes() + b"\n")
        with self.assertRaisesRegex(ValueError, "SHA256 mismatch"):
            self.export("refused")
        self.assertFalse((self.base / "refused").exists())

    def test_unlisted_csv_refused_before_output(self):
        self.manifest.write_text("".join(line + "\n" for line in self.manifest.read_text().splitlines() if "M1w1/get100/" not in line))
        with self.assertRaisesRegex(ValueError, "unlisted input"):
            self.export("refused")
        self.assertFalse((self.base / "refused").exists())

    def test_manifest_escape_duplicate_and_empty_refused(self):
        digest = "a" * 64
        for text in (digest + "  ../outside\n", digest + "  entry\n" + digest + "  entry\n", ""):
            with self.subTest(text=text):
                self.manifest.write_text(text)
                with self.assertRaises(ValueError):
                    exporter.VerifiedInputs(self.home, self.manifest)

    def test_symlink_outside_archive_refused(self):
        target = self.base / "outside"
        target.write_text("outside\n")
        link = self.home / "escape"
        link.symlink_to(target)
        self.pin()
        with self.assertRaisesRegex(ValueError, "outside FDB_PERF_HOME"):
            self.export("refused")
        self.assertFalse((self.base / "refused").exists())

    def test_private_tokens_including_glued_names_refused(self):
        for value in ("private-id", "nodeprivate-idsuffix", "private-name"):
            with self.subTest(value=value):
                path = self.campaign / "sealed/provenance/source-receipt.json"
                self.put(path, {"production_revisions": {"final": value}})
                self.pin()
                with self.assertRaisesRegex(ValueError, "identifier/name"):
                    self.export("refused")
                self.assertFalse((self.base / "refused").exists())

    def test_private_path_address_and_credential_refused(self):
        redactions = {"identifiers": ["private-id"], "names": []}
        for value in ("/" + "home" + "/operator/file", ".".join(("192", "0", "2", "1")), "BEGIN RSA PRIVATE KEY"):
            with self.subTest(value=value):
                with self.assertRaises(ValueError):
                    exporter.validate_public({"sample.json": json.dumps(value) + "\n"}, redactions)

    def test_existing_output_not_overwritten(self):
        path, _ = self.export("existing")
        before = (path / "analysis/method.json").read_bytes()
        with self.assertRaisesRegex(ValueError, "must not already exist"):
            self.export("existing")
        self.assertEqual(before, (path / "analysis/method.json").read_bytes())


if __name__ == "__main__":
    unittest.main()
