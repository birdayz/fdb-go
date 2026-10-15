"""Failure-path checks use isolated static-site fixtures, never production output."""

import contextlib
import io
from pathlib import Path
import re
import tempfile
import unittest

from check import PUBLIC_PAGES, SECTION_PAGES, check


class SiteCheckTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        for path in PUBLIC_PAGES:
            kind = "website" if path in SECTION_PAGES else "article"
            page = self.root / path.lstrip("/") / "index.html"
            page.parent.mkdir(parents=True, exist_ok=True)
            page.write_text(f'''<!doctype html><html><head>
<link rel="canonical" href="https://fdb.dev{path}">
<meta name="description" content="FoundationDB for Go">
<meta property="og:description" content="FoundationDB for Go">
<meta name="twitter:description" content="FoundationDB for Go">
<meta property="og:type" content="{kind}">
<meta name="twitter:card" content="summary_large_image">
<meta property="og:image" content="https://fdb.dev/og.png">
<meta name="twitter:image" content="https://fdb.dev/og.png">
<meta name="go-import" content="fdb.dev git https://github.com/birdayz/fdb-go">
</head><body><h1 id="overview">FoundationDB for Go</h1>
<a href="/docs/getting-started/#overview">Get started</a>
<a href="/sample.txt">Download</a><a href="https://example.com/">External</a>
</body></html>''')
        (self.root / "sample.txt").write_text("fixture")
        (self.root / "og.png").write_bytes(b"fixture; raster text is reviewed separately")

    def run_check(self):
        with contextlib.redirect_stdout(io.StringIO()):
            check(self.root)

    def test_valid_site(self):
        self.run_check()

    def test_changelog_measurement_link(self):
        repo = Path(__file__).resolve().parent.parent
        report = "docs/benchmarks/rfc-258-pr4-allocations.md"
        links = re.findall(r"\[RFC-258 PR4 measurements\]\(([^)]+)\)",
                           (repo / "CHANGELOG.md").read_text())
        self.assertEqual(links, [f"https://github.com/birdayz/fdb-go/blob/master/{report}"])
        self.assertTrue((repo / report).is_file())
        changelog = self.root / "changelog/index.html"
        changelog.parent.mkdir()
        changelog.write_text(f'<a href="{links[0]}">Measurements</a>')
        self.run_check()
        changelog.write_text(f'<a href="{report}">Measurements</a>')
        with self.assertRaisesRegex(SystemExit, r"/changelog/: missing local page/file"):
            self.run_check()
        changelog.write_text(f'<a href="{links[0]}">Measurements</a>')
        self.run_check()

    def test_rejects_broken_output(self):
        changes = [
            ('href="/sample.txt"', 'href="/missing/"', "missing local page/file"),
            ('#overview"', '#missing"', "missing anchor"),
            ('href="https://fdb.dev/"', 'href="http://localhost/"', "wrong canonical"),
            ('name="description" content="FoundationDB for Go"', 'name="description"', "missing description"),
            ('property="og:description" content="FoundationDB for Go"',
             'property="og:description" content="2-4x faster reads"', "unsupported performance"),
            ('name="twitter:description" content="FoundationDB for Go"',
             'name="twitter:description" content="Byte-identical with Java"', "blanket compatibility"),
            ('property="og:type" content="website"', 'property="og:type" content="article"', "wrong og:type"),
            ('name="twitter:card" content="summary_large_image"',
             'name="twitter:card" content="video"', "wrong twitter:card"),
            ('property="og:image" content="https://fdb.dev/og.png"',
             'property="og:image" content="/missing.png"', "wrong og:image"),
            ('name="twitter:image" content="https://fdb.dev/og.png"',
             'name="twitter:image" content="/missing.png"', "wrong twitter:image"),
            ('content="fdb.dev git https://github.com/birdayz/fdb-go"',
             'content="fdb.dev git https://example.com/wrong"', "vanity-import"),
            ('<body><h1 id="overview">',
             '<body><nav class="hextra-toc"></nav><h1 id="overview">', "right-side navigation"),
        ]
        page = self.root / "index.html"
        original = page.read_text()
        for old, new, message in changes:
            with self.subTest(message=message):
                self.assertEqual(original.count(old), 1, "mutation must hit exactly one attribute")
                mutated = original.replace(old, new)
                page.write_text(mutated)
                self.assertIn(new, page.read_text(), "mutation must reach the fixture")
                with self.assertRaisesRegex(SystemExit, message):
                    self.run_check()
                page.write_text(original)
                self.run_check()

    def test_rejects_missing_card(self):
        (self.root / "og.png").unlink()
        with self.assertRaisesRegex(SystemExit, "Missing social card"):
            self.run_check()

    def test_rejects_missing_public_page(self):
        (self.root / "docs/maturity/index.html").unlink()
        with self.assertRaisesRegex(SystemExit, "Missing public pages"):
            self.run_check()

    def test_benchmark_sync_in_sync_and_clean(self):
        import importlib.util
        import subprocess
        import sys
        repo = Path(__file__).resolve().parent.parent
        script = repo / "scripts/sync-website-benchmarks.py"
        if script.is_file():
            subprocess.run([sys.executable, "-B", str(script), "--check"], check=True)
            spec = importlib.util.spec_from_file_location("sync_wb", script)
            mod = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(mod)
            for bad in ["10.88.0.10", "172.31.99.1", "192.168.1.5", "/home/birdy/x", "/srv/fdb-bench/x"]:
                with self.assertRaises(SystemExit):
                    mod.check_text(Path("fixture.md"), f"host {bad} here")
            self.assertEqual(mod.format_tick(1.05), "1.05×")
            self.assertEqual(mod.format_tick(1.10), "1.1×")
            self.assertEqual(mod.format_tick(1.0), "1.0×")
            with self.assertRaisesRegex(ValueError, "falls outside axis tick bounds"):
                mod.render_forest_svg(
                    "Out of bounds test", "sub",
                    [("G", [("case1", "1.000 [0.800, 1.500]")])],
                    [0.9, 1.0, 1.1], "L", "R"
                )
            with self.assertRaisesRegex(ValueError, "falls outside"):
                mod.render_grouped_bars_svg(
                    "Bar out of bounds", "sub",
                    [("case1", 100.0, 500.0, 100.0, 2500.0)],
                    "µs", 2000.0, [0, 1000, 2000]
                )
            with self.assertRaisesRegex(ValueError, "not found in canonical markdown"):
                mod.verify_summary_numbers("test", "Wins: 0.999×", "table has 0.500 only")

    def test_empty_site(self):
        for page in self.root.rglob("index.html"):
            page.unlink()
        with self.assertRaisesRegex(SystemExit, "Missing public pages"):
            self.run_check()


if __name__ == "__main__":
    unittest.main()
