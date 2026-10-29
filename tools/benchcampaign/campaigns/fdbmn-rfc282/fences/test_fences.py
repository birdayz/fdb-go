#!/usr/bin/env python3
"""Offline patch-integrity controls; Go behavioral tests travel in the overlay."""
import hashlib
import json
from pathlib import Path
import unittest

from apply_patch import apply_patch

HERE = Path(__file__).resolve().parent
RUN = 'tools/clientbench/internal/bench/run.go'
TEST = 'tools/clientbench/internal/bench/bench_test.go'
BUILD = 'tools/clientbench/scripts/BUILD.bazel'
SMOKE = 'tools/clientbench/integration/smoke_test.go'
BACKEND_BUILD = 'tools/clientbench/cmd/clientbench/BUILD.bazel'
BACKEND_TEST = 'tools/clientbench/cmd/clientbench/backend_selection_test.go'
VERSION_FILES = {'tools/clientbench/scripts/' + name for name in ('matrix_plan.py', 'r3_plan.py', 'test_campaign.py')}


def sparse_sources(manifest):
    sources = {}
    for name, spec in manifest['original_context'].items():
        lines = [f'// omitted original line {i + 1}\n' for i in range(spec['line_count'])]
        for hunk in spec['hunks']:
            start = hunk['start']
            lines[start:start + len(hunk['lines'])] = hunk['lines']
        sources[name] = ''.join(lines)
    return sources


class FenceOverlayTest(unittest.TestCase):
    def setUp(self):
        self.manifest = json.loads((HERE / 'source-context.json').read_text())
        self.patch = (HERE / 'measurement-fences.patch').read_text()
        self.sources = sparse_sources(self.manifest)

    def test_patch_pins_and_exact_context_application(self):
        self.assertEqual(self.manifest['base_revision'], 'd94b4c61377d173b7c6884fde426a227dab0f0bb')
        self.assertEqual(self.manifest['original_sha256'][RUN],
                         'b82341a5652afa1a94931ff687519c262065f592b85b16e5b832182efea00214')
        self.assertEqual(hashlib.sha256(self.patch.encode()).hexdigest(), self.manifest['patch_sha256'])
        self.assertEqual(set(apply_patch(self.sources, self.patch)), {RUN, TEST, BUILD, SMOKE, BACKEND_BUILD, BACKEND_TEST} | VERSION_FILES)

    def test_patch_refuses_changed_context_and_incomplete_population(self):
        changed = dict(self.sources)
        changed[RUN] = changed[RUN].replace('p.WallNS = time.Since(epoch).Nanoseconds()', 'p.WallNS = 0')
        with self.assertRaisesRegex(ValueError, 'source context changed'):
            apply_patch(changed, self.patch)
        with self.assertRaises(ValueError):
            apply_patch(self.sources, '')
        with self.assertRaises(ValueError):
            apply_patch(self.sources, self.patch.replace('--- a/' + RUN, '--- a/unknown.go'))
        with self.assertRaises(ValueError):
            apply_patch(self.sources, self.patch.replace('@@ -', '@@ -999', 1))

    def test_only_original_fence_clocks_feed_metadata(self):
        patched = apply_patch(self.sources, self.patch)[RUN]
        self.assertIn('end := time.Now()\n\tp.WallNS = end.Sub(epoch).Nanoseconds()\n'
                      '\tp.Started, p.Ended = epoch, end\n', patched)
        self.assertIn('fences, fenceErr := measured.measurementFences()', patched)
        self.assertNotIn('warm.measurementFences()', patched)
        self.assertIn('p.Ended.Sub(p.Started).Nanoseconds() != p.WallNS', patched)
        added = [line[1:] for line in self.patch.splitlines() if line.startswith('+') and not line.startswith('+++')]
        run_added = '\n'.join(added[:added.index('func TestMeasurementFencesUseOriginalTimes(t *testing.T) {')])
        self.assertEqual(run_added.count('time.Now()'), 1)
        for forbidden in ('operation(i, worker)', 'p.Samples[i]', 'close(start)', 'epoch =', 'cfg.Iterations ='):
            self.assertNotIn(forbidden, run_added)

    def test_build_compatibility_only_exports_four_existing_scripts(self):
        self.assertEqual(self.manifest['original_sha256'][BUILD],
                         '8b56aadaced964d10d425acfb42b6bb509aa850351fe1421b1c989d8e7e778c1')
        self.assertEqual(hashlib.sha256(self.sources[BUILD].encode()).hexdigest(),
                         self.manifest['original_sha256'][BUILD])
        patched = apply_patch(self.sources, self.patch)[BUILD]
        block = ('exports_files(\n    [\n        "analyze.py",\n        "matrix_plan.py",\n'
                 '        "r3_plan.py",\n        "run.py",\n    ],\n'
                 '    visibility = ["//tools/benchcampaign:__subpackages__"],\n)\n\n')
        self.assertEqual(patched.count(block), 1)
        self.assertEqual(patched.replace(block, ''), self.sources[BUILD])
        self.assertEqual(hashlib.sha256(patched.encode()).hexdigest(), self.manifest['patched_sha256'][BUILD])
        self.assertNotIn('//visibility:public', patched)

    def test_compiler_metadata_and_fixture_literals_only(self):
        patched = apply_patch(self.sources, self.patch)
        for name in VERSION_FILES:
            with self.subTest(name=name):
                self.assertEqual(self.sources[name].count('go1.26.9'), 1)
                self.assertEqual(patched[name], self.sources[name].replace('go1.26.9', 'go1.27.3'))
                self.assertEqual(patched[name].count('go1.27.3'), 1)
        self.assertNotIn('tools/clientbench/scripts/run.py', patched)
        self.assertNotIn('tools/clientbench/README.md', patched)

    def test_smoke_preserves_cleanup_contract_and_checks_run_summary_publication(self):
        patched = apply_patch(self.sources, self.patch)[SMOKE]
        obsolete = '\t\t\t\t\t\tdrain.After.CleanupsQueued != drain.Before.CleanupsQueued ||\n'
        self.assertEqual(self.sources[SMOKE].count(obsolete), 1)
        smoke_patch = self.patch.split('--- a/' + SMOKE + '\n', 1)[1].split('\n--- a/', 1)[0]
        removed = [line[1:] + '\n' for line in smoke_patch.splitlines()
                   if line.startswith('-') and not line.startswith('---')]
        self.assertEqual(removed, [obsolete])
        self.assertNotIn(obsolete, patched)
        for field in ('measured_fences_format', 'measured_started_unix_ns', 'measured_ended_unix_ns',
                      'measured_started_utc', 'measured_ended_utc', 'measured_wall_ns'):
            self.assertIn('`json:"' + field + '"`', patched)
        for check in ('summary.FencesFormat != "fdb-bench-measured-fences/v1"',
                      'summary.StartedNS <= 1<<53', 'summary.EndedNS <= summary.StartedNS',
                      'summary.WallNS <= 0', 'summary.StartedNS < invocationStarted.UnixNano()',
                      'summary.EndedNS > invocationEnded.UnixNano()',
                      '{summary.StartedUTC, summary.StartedNS}, {summary.EndedUTC, summary.EndedNS}',
                      'parsed.UnixNano() != endpoint.ns',
                      'endpoint.text != time.Unix(0, endpoint.ns).UTC().Format(time.RFC3339Nano)'):
            self.assertIn(check, patched)
        for check in ('!drain.Complete', 'drain.Rounds < 2', 'drain.WallNS <= 0',
                      'drain.After.FinalizersExecuted != drain.After.FinalizersQueued',
                      'drain.After.CleanupsExecuted != drain.After.CleanupsQueued'):
            self.assertIn(check, patched)
        self.assertNotIn('tools/clientbench/internal/bench/lifecycle.go', self.sources)
        self.assertNotIn('tools/clientbench/internal/bench/lifecycle_test.go', self.sources)
        self.assertNotIn('tools/clientbench/scripts/run.py', self.sources)

    def test_real_cgo_run_controls_reuse_actual_backend_and_keep_old_selection_controls(self):
        patched = apply_patch(self.sources, self.patch)
        control = patched[BACKEND_TEST]
        self.assertEqual(hashlib.sha256(control.encode()).hexdigest(), self.manifest['patched_sha256'][BACKEND_TEST])
        original_tests = self.sources[BACKEND_TEST].split('func TestBackendBuildTags', 1)[1]
        self.assertTrue(control.endswith('func TestBackendBuildTags' + original_tests))
        for name in ('default_without_cgo', 'default_with_cgo', 'libfdbc_without_cgo', 'libfdbc_with_cgo'):
            self.assertIn('name: "' + name + '"', control)
        for check in ('foundationdb.Run(ctx, "", foundationdb.WithDirectIP())',
                      'context.WithTimeout(context.Background(), 2*time.Minute)',
                      'exec.CommandContext(ctx, os.Args[0]',
                      '[]string{"positive", "zero"}',
                      'db, metadata, err := openBackend(cfg)',
                      'synctest.Test(t, func(t *testing.T)',
                      'runErr = bench.Run(cfg, database, metadata, afterIdentity)',
                      'err := d.LifecycleDatabase.Transact(readOnly, fn)',
                      'if err == nil && readOnly', 'time.Sleep(time.Nanosecond)',
                      'Workload: "read1000"', 'Iterations: 1, Warmup: 0, Workers: 1',
                      'summary.Completed != 1 || summary.Errors != 0',
                      'summary.PersistedValidation != "ok"',
                      '!life.Complete || !life.DatabaseClosed || !life.NetworkStopped',
                      'runErr.Error() != "invalid measured phase fences"',
                      'summary.FailureStage != "measurement_fences"',
                      'summary.WallNS != 0', 'summary.WallNS != 1',
                      'present != (mode == "positive")'):
            self.assertIn(check, control)
        self.assertLess(control.index('db, metadata, err := openBackend(cfg)'), control.index('synctest.Test(t,'))
        for key in ('measured_fences_format', 'measured_started_unix_ns', 'measured_ended_unix_ns',
                    'measured_started_utc', 'measured_ended_utc'):
            self.assertIn('"' + key + '"', control)
        self.assertNotIn('type transaction struct', control)
        self.assertNotIn('func (d delayedRealDatabase) Get', control)
        build = patched[BACKEND_BUILD]
        self.assertEqual(build.split('# Each executable', 1)[1], self.sources[BACKEND_BUILD].split('# Each executable', 1)[1])
        for declaration in ('"backend_cgo.go",', 'cgo = True', 'clinkopts = ["-lfdb_c"]',
                            'gotags = ["libfdbc"]', 'pure = "off"',
                            '"//pkg/testcontainers/foundationdb"', '"//tools/clientbench/internal/bench"',
                            '"@com_github_apple_foundationdb_bindings_go//src/fdb"'):
            self.assertIn(declaration, build.split('# Each executable', 1)[0])
        for source in ('backend_cgo.go', 'backend_go.go', 'main.go'):
            self.assertNotIn('tools/clientbench/cmd/clientbench/' + source, patched)

    def test_real_go_controls_are_in_existing_declared_test_file(self):
        controls = (HERE / 'fence-tests.go.txt').read_text()
        self.assertEqual(hashlib.sha256(controls.encode()).hexdigest(), self.manifest['go_controls_sha256'])
        patched = apply_patch(self.sources, self.patch)[TEST]
        self.assertTrue(patched.endswith(controls))
        for name in ('TestMeasurementFencesUseOriginalTimes', 'TestMeasurementFencesRejectInvalidPhase',
                     'TestMeasurementFencesKeepNanosecondPrecision'):
            self.assertIn('func ' + name + '(t *testing.T)', patched)
        self.assertIn('json.Unmarshal(data, &got)', controls)
        self.assertIn('{got.StartUTC, start}, {got.EndUTC, p.Ended}', controls)
        self.assertNotIn('range map[string]time.Time', controls)
        self.assertIn('parsed.UnixNano() != endpoint.want.UnixNano()', controls)
        self.assertIn('p.WallNS++', controls)
        self.assertIn('p.Started, p.Ended = p.Ended, p.Started', controls)
        self.assertIn('measured.Started.Before(warm.Ended)', controls)


if __name__ == '__main__':
    unittest.main()
