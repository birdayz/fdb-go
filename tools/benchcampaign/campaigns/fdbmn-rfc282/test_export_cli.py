"""Real staged CLI I/O/pin/privacy boundary; numerical export is tested separately."""
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

import export_results as export
import helpers


class CLITests(unittest.TestCase):
    def stage(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        root = Path(temp.name) / 'campaign'
        root.mkdir()
        relative = 'tooling/tools/benchcampaign/campaigns/fdbmn-rfc282'
        old = 'tooling/tools/benchcampaign/campaigns/fdbmn-20261020/public-export'
        sources = {
            relative + '/export_results.py': Path(export.__file__),
            relative + '/helpers.py': Path(helpers.__file__),
            relative + '/m2_export.py': Path(export.m2_export.__file__),
            relative + '/server_cpu.py': Path(export.server_cpu.__file__),
            old + '/export_public_results.py': Path(export.legacy_io.__file__),
            old + '/export_throughput.py': Path(export.legacy_m1.__file__),
            old + '/m2_throughput.py': Path(export.m2_export.old.__file__),
        }
        multinode = Path(export.__file__).resolve().parents[2] / 'multinode'
        for name in ('node_sampler.py', 'mn_common.py'):
            sources['tooling/tools/benchcampaign/multinode/' + name] = multinode / name
        for name, source in sources.items():
            path = root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(source, path)
        # These independently pinned numerical callbacks are not used by this
        # CLI-boundary fixture; actual collector behavior has separate controls.
        for name, text in {
            'frozen/tools/clientbench/scripts/analyze.py': 'def bootstrap_log_ratio(*args): raise AssertionError("unused")\n',
            'frozen/tools/clientbench/scripts/run.py': '# pinned analyzer dependency\n',
            'tooling/tools/benchcampaign/singlenode/bench_analyze.py': 'def paired_gm(*args): raise AssertionError("unused")\n',
            'tooling/tools/benchcampaign/singlenode/bench_runner.py': '# pinned analyzer dependency\n',
            'private/redactions.json': json.dumps({'identifiers': ['private-test-id'], 'names': ['private-test-host']}),
        }.items():
            path = root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(text)
        manifest = root.parent / 'inputs.sha256'
        manifest.write_text(''.join(hashlib.sha256(path.read_bytes()).hexdigest() + '  ' + path.relative_to(root).as_posix() + '\n'
                                    for path in sorted(root.rglob('*')) if path.is_file()))
        return root, manifest, root.parent / 'public', set(sources)

    def invoke(self, root, manifest, output, payload='safe synthetic result'):
        # Keep main(), real VerifiedInputs, staged imports and final privacy gate
        # intact; replace only the separately tested numerical calculation.
        script = ('import sys; sys.path.insert(0, sys.argv[1]); import export_results as e; '
                  'e.export=lambda *args: {"method": {}, "value": sys.argv[2]}; '
                  'raise SystemExit(e.main(sys.argv[3:]))')
        return subprocess.run([sys.executable, '-B', '-c', script,
            str(root / 'tooling/tools/benchcampaign/campaigns/fdbmn-rfc282'), payload,
            '--campaign', str(root), '--inputs-manifest', str(manifest), '--analysis', 'analysis/current',
            '--output-dir', str(output), '--redactions', 'private/redactions.json'],
            capture_output=True, text=True, timeout=30)

    def test_staged_main_publishes_exact_helper_pins_and_refuses_overwrite(self):
        root, manifest, output, expected = self.stage()
        completed = self.invoke(root, manifest, output)
        self.assertEqual(completed.returncode, 0, completed.stderr)
        result = json.loads((output / 'results.json').read_text())
        self.assertEqual(result['value'], 'safe synthetic result')
        # mn_common is included through node_sampler.mc; all nine helper paths
        # are the actual staged imports, not merely manifest names.
        self.assertEqual(set(result['method']['export_helpers_sha256']), expected)
        for name, value in result['method']['export_helpers_sha256'].items():
            self.assertEqual(value, hashlib.sha256((root / name).read_bytes()).hexdigest())
        retained = (output / 'results.json').read_bytes()
        repeated = self.invoke(root, manifest, output, payload='replacement')
        self.assertNotEqual(repeated.returncode, 0)
        self.assertIn('output already exists', repeated.stderr)
        self.assertEqual((output / 'results.json').read_bytes(), retained)

    def test_staged_main_refuses_missing_changed_or_external_helper_before_output(self):
        for mutation in ('missing', 'changed', 'external', 'analyzer'):
            with self.subTest(mutation=mutation):
                root, manifest, output, _ = self.stage()
                relative = 'tooling/tools/benchcampaign/campaigns/fdbmn-rfc282/server_cpu.py'
                path = root / relative
                if mutation == 'missing':
                    manifest.write_text(''.join(line for line in manifest.read_text().splitlines(keepends=True)
                                               if not line.endswith('  ' + relative + '\n')))
                if mutation == 'changed': path.write_text(path.read_text() + '\n# changed after seal\n')
                if mutation == 'external':
                    outside = root.parent / 'external_server_cpu.py'
                    shutil.copyfile(path, outside)
                    path.unlink()
                    path.symlink_to(outside)
                if mutation == 'analyzer':
                    path = root / 'frozen/tools/clientbench/scripts/analyze.py'
                    path.write_text(path.read_text() + '\n# changed after seal\n')
                completed = self.invoke(root, manifest, output)
                self.assertNotEqual(completed.returncode, 0)
                self.assertIn({'missing': 'unlisted input', 'changed': 'input SHA256 mismatch',
                               'external': 'execute only the staged pinned exporter/helpers',
                               'analyzer': 'input SHA256 mismatch'}[mutation], completed.stderr)
                self.assertFalse(output.exists())

    def test_staged_main_keeps_final_public_redaction_gate(self):
        for payload, reason in (('prefix-private-test-id-suffix', 'private identifier/name'),
                                ('role change at 10.89.20.2:4500', 'IPv4-like address'),
                                ('missing /srv/fdb-bench/private/receipt.json', 'private path/credential')):
            with self.subTest(payload=payload):
                root, manifest, output, _ = self.stage()
                completed = self.invoke(root, manifest, output, payload)
                self.assertNotEqual(completed.returncode, 0)
                self.assertIn(reason, completed.stderr)
                self.assertFalse(output.exists())


if __name__ == '__main__':
    unittest.main()
