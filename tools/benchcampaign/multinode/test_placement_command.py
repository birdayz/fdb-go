#!/usr/bin/env python3
"""Tests for placement_command.py (R2's placement command) and the seal_multinode
run/manifest changes. Temporary fixtures and fake runners only: no placementprobe,
fdbcli or FoundationDB is executed."""
import argparse
import contextlib
import copy
import io
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest

HERE = Path(os.path.abspath(__file__)).parent
sys.path.insert(0, str(HERE))

import mn_common as mc  # noqa: E402
import placement_command as pc  # noqa: E402
import placement_receipt as pr  # noqa: E402
import seal_multinode as seal  # noqa: E402

ROWS = 1000
CLUSTER_TEXT = 'fdbbenchvpc:mn20261014@10.88.0.11:4500,10.88.0.12:4500,10.88.0.13:4500\n'
CLUSTER_ID = pr.cluster_identity(CLUSTER_TEXT.encode())
INDEX = ['10.88.0.11:4505', '10.88.0.13:4503']
RECORDS = ['10.88.0.11:4504', '10.88.0.12:4502']


def probe_doc(store, prefix_items, suffixes, record_addresses=RECORDS, fixture_sha256='a' * 64):
    prefix = pr.pack(list(prefix_items))
    keys = []
    for i in range(ROWS):
        for s in suffixes:
            keys.append(dict(kind='record', id=i, key_hex=(prefix + pr.pack([1, i, s])).hex(),
                             addresses=list(record_addresses)))
    for i in range(ROWS):
        keys.append(dict(kind='index', id=i, key_hex=(prefix + pr.pack([2, 'price_idx', i, i])).hex(),
                         addresses=list(INDEX)))
    return dict(format=pr.PROBE_FORMAT, store=store, store_prefix_hex=prefix.hex(), read_version=7,
                boundaries_read_version=7, boundary_range_hex=[prefix.hex(), pr.strinc(prefix).hex()],
                boundaries_complete=True, boundaries_hex=[(prefix + pr.pack([1, 1000000])).hex()],
                scanned_subspaces_hex=[(prefix + pr.pack([1])).hex(), (prefix + pr.pack([2])).hex()],
                keys_complete=True, keys=keys,
                **pr.synthetic_provenance(prefix, fixture_sha256=fixture_sha256, cluster=CLUSTER_ID))


def status_doc():
    procs = {}
    for i, a in enumerate(INDEX + RECORDS):
        node = {'10.88.0.11': 'node1', '10.88.0.12': 'node2', '10.88.0.13': 'node3'}[a.split(':')[0]]
        procs[f'p{i}'] = dict(address=a, class_type='storage', locality=dict(machineid=node),
                              roles=[dict(role='storage', id=f's{i}')])
    return dict(cluster=dict(processes=procs))


class FakeTools:
    """Answers placementprobe probe (writes the probe to --output) and fdbcli status json."""

    def __init__(self, probe, probe_rc=0, status_rc=0):
        self.probe, self.probe_rc, self.status_rc, self.calls = probe, probe_rc, status_rc, []

    def __call__(self, argv, timeout=None):
        argv = [str(a) for a in argv]
        self.calls.append(argv)
        if argv[1] == 'probe':
            if self.probe_rc:
                return self.probe_rc, '', 'probe exploded'
            Path(argv[argv.index('--output') + 1]).write_text(json.dumps(self.probe))
            return 0, argv[argv.index('--output') + 1] + '\n', ''
        if argv[-2:] == ['--exec', 'status json']:
            return self.status_rc, json.dumps(status_doc()), '' if not self.status_rc else 'no cluster'
        raise AssertionError(f'unexpected command {argv}')


class Temp(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        self.tmp = Path(self._tmp.name)
        # The tools refuse outputs inside a Git work tree; a TMPDIR there is a broken fixture.
        self.assertFalse(mc.inside_git_worktree(self.tmp), f'TMPDIR {self.tmp} lies inside a Git work tree')
        fixtures = {}
        for store, chunks in [('benchfetch/small', 1), ('benchfetch/split', 2), ('benchfetch/planskip', None)]:
            prefix = pr.pack(list(pc.STORES[store][0]))
            fx = dict(phase='fixture', prefix_bytes=len(prefix), rows=ROWS, case='BenchmarkX')
            if chunks:
                fx['chunks'] = chunks
            path = self.tmp / f'fixture-{store.replace("/", "-")}.json'
            path.write_text(json.dumps(fx))
            fixtures[store] = str(path)
        self.fixtures = self.tmp / 'fixture-receipts.json'
        self.fixtures.write_text(json.dumps(fixtures))
        self.fixture_paths = fixtures
        self.cluster = self.tmp / 'fdb.cluster'
        self.cluster.write_text(CLUSTER_TEXT)

    def probe(self, store, **kw):
        tup = pc.STORES[store][0]
        suffixes = pr.RECORD_SUFFIXES[pc.STORES[store][2] or 'unsplit']
        return probe_doc(store, tup, suffixes, fixture_sha256=pr.sha(self.fixture_paths[store]), **kw)

    def argv(self, store='benchfetch/small', when='before', **over):
        _, layout, fmt = pc.STORES[store]
        values = dict(store=store, case='small-rows10-forward', when=when, preset=pc.PRESET_OF[store], layout=layout,
                      record_format=fmt or '', cluster_file=str(self.cluster),
                      expect_cluster_sha256=CLUSTER_ID['cluster_file_sha256'], output=str(self.tmp / f'r-{when}.json'),
                      placementprobe='/opt/placementprobe', fdbcli='/usr/bin/fdbcli',
                      fixture_receipts=str(self.fixtures), work_dir=str(self.tmp / 'work'))
        values.update(over)
        out = []
        for k, v in values.items():
            out += ['--' + k.replace('_', '-'), v]
        return out


class PlacementCommandTest(Temp):
    def test_writes_an_integral_receipt_for_the_sealed_case(self):
        tools = FakeTools(self.probe('benchfetch/small'))
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(pc.main(self.argv(), runner=tools), 0)
        r = json.loads((self.tmp / 'r-before.json').read_text())
        self.assertTrue(r['integrity_ok'], r['errors'])
        self.assertTrue(r['passed'])
        self.assertEqual(r['sealed_errors'], [])
        self.assertEqual(r['cluster'], CLUSTER_ID)
        self.assertEqual(r['addresses_basis'], 'transactional')
        self.assertEqual((r['store'], r['preset'], r['layout'], r['record_format']),
                         ('benchfetch/small', 'benchfetch', 'recordlayer', 'unsplit'))
        self.assertEqual(r['cells']['rows10']['ids'], [100, 110])
        probe_call = tools.calls[0]
        self.assertEqual(probe_call[:2], ['/opt/placementprobe', 'probe'])
        self.assertEqual(probe_call[probe_call.index('--fixture-receipt') + 1],
                         json.loads(self.fixtures.read_text())['benchfetch/small'])
        self.assertEqual(tools.calls[1], ['/usr/bin/fdbcli', '-C', str(self.cluster), '--exec', 'status json'])
        work = self.tmp / 'work' / 'small-rows10-forward' / 'before'
        self.assertEqual(r['inputs']['probe_sha256'], pr.sha(work / 'probe.json'))
        self.assertEqual(json.loads((work / 'status.json').read_text()), status_doc())

    def test_split_and_partial_placement_are_receipts_not_errors(self):
        colocated = self.probe('benchfetch/split', record_addresses=INDEX)
        colocated['boundaries_hex'] = []  # one shard: legitimately colocated
        with contextlib.redirect_stdout(io.StringIO()):
            rc = pc.main(self.argv('benchfetch/split', case='split-rows10-forward'), runner=FakeTools(colocated))
        self.assertEqual(rc, 0)
        r = json.loads((self.tmp / 'r-before.json').read_text())
        self.assertTrue(r['integrity_ok'], r['errors'])
        self.assertFalse(r['passed'])
        self.assertEqual(r['cells']['rows10']['label'], 'partially colocated (f = 0.0000)')
        self.assertEqual(r['shared_shards'], 1)

    def test_integrity_failure_still_writes_the_receipt_and_exits_1(self):
        bad = self.probe('benchfetch/small')
        bad['keys'].append(copy.deepcopy(bad['keys'][0]))
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(pc.main(self.argv(), runner=FakeTools(bad)), 1)
        self.assertFalse(json.loads((self.tmp / 'r-before.json').read_text())['integrity_ok'])

    def test_tool_failures_exit_1_without_a_receipt(self):
        good = self.probe('benchfetch/small')
        for i, tools in enumerate([FakeTools(good, probe_rc=1), FakeTools(good, status_rc=1)]):
            with self.subTest(i=i), contextlib.redirect_stderr(io.StringIO()):
                out = self.tmp / f'fail-{i}.json'
                self.assertEqual(pc.main(self.argv(output=str(out), work_dir=str(self.tmp / f'w{i}')), runner=tools), 1)
                self.assertFalse(out.exists())

    def test_refusals_touch_nothing(self):
        repo = self.tmp / 'repo'
        (repo / '.git').mkdir(parents=True)
        (self.tmp / 'exists.json').write_text('{}')
        relative = self.tmp / 'relative.json'
        relative.write_text(json.dumps({'benchfetch/small': 'fixture.json'}))
        cases = [
            dict(store='benchfetch/other'),
            dict(layout='opaque'),
            dict(record_format='split'),
            dict(preset='planskip'),
            dict(when='during'),
            dict(case='../escape'),
            dict(output=str(self.tmp / 'exists.json')),
            dict(output=str(repo / 'r.json')),
            dict(work_dir=str(repo / 'work')),
            dict(fixture_receipts=str(relative)),
            dict(expect_cluster_sha256='0' * 64),
            dict(expect_cluster_sha256='not-hex'),
        ]
        no_split = self.tmp / 'no-split.json'
        no_split.write_text(json.dumps({'benchfetch/small': json.loads(self.fixtures.read_text())['benchfetch/small']}))
        for over in cases + [dict(store='benchfetch/split', case='c', fixture_receipts=str(no_split))]:
            store = over.pop('store', 'benchfetch/small')
            argv = self.argv(store) if store in pc.STORES else self.argv()
            if store not in pc.STORES:
                argv[argv.index('--store') + 1] = store
            for k, v in over.items():
                argv[argv.index('--' + k.replace('_', '-')) + 1] = v
            tools = FakeTools(None)
            with self.subTest(over=over, store=store), contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(pc.main(argv, runner=tools), 2)
                self.assertEqual(tools.calls, [])
        (self.tmp / 'work' / 'small-rows10-forward' / 'before').mkdir(parents=True)
        with contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(pc.main(self.argv(), runner=FakeTools(None)), 2)  # reused work dir

    def test_provenance_defects_never_yield_a_sealable_receipt(self):
        cases = {
            'diagnostic addresses': lambda p: p['addresses_basis'].update(kind='location-cache (diagnostic)'),
            'dd on (allow-dd-on probe)': lambda p: p['dd_mode'].update(required_off=False),
            'probe on another cluster': lambda p: p.update(cluster=dict(connection_string_id='other:cluster',
                                                                         cluster_file_sha256='9' * 64)),
            'probe of another fixture receipt': lambda p: p.update(fixture_receipt_sha256='d' * 64),
            'lock token changed': lambda p: p['move_keys_lock'].update(write_token_hex_at_v2='ff' * 16),
        }
        for i, (name, mutate) in enumerate(cases.items()):
            probe = self.probe('benchfetch/small')
            mutate(probe)
            out = self.tmp / f'prov-{i}.json'
            with self.subTest(name), contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(pc.main(self.argv(output=str(out), work_dir=str(self.tmp / f'pw{i}')),
                                         runner=FakeTools(probe)), 1)
                r = json.loads(out.read_text())
                self.assertFalse(r['integrity_ok'])
                self.assertTrue(r['sealed_errors'])

    def test_symlinked_case_directory_into_a_repo_is_refused(self):
        repo = self.tmp / 'repo'
        (repo / '.git').mkdir(parents=True)
        work = self.tmp / 'work'
        work.mkdir()
        (work / 'small-rows10-forward').symlink_to(repo)
        tools = FakeTools(self.probe('benchfetch/small'))
        with contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(pc.main(self.argv(), runner=tools), 2)
        self.assertEqual(tools.calls, [])
        self.assertEqual(sorted(p.name for p in repo.iterdir()), ['.git'])
        self.assertFalse((self.tmp / 'r-before.json').exists())
        (work / 'small-rows10-forward').unlink()
        (work / 'linked-root').symlink_to(work)
        with contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(pc.main(self.argv(work_dir=str(work / 'linked-root')), runner=tools), 2)

    def test_r2_template_renders_into_this_command(self):
        import bench_runner_mn as r2
        template = ['python3', '-B', str(HERE / 'placement_command.py'),
                    '--store', '{store}', '--case', '{case}', '--when', '{when}', '--preset', '{preset}',
                    '--layout', '{layout}', '--record-format', '{record_format}',
                    '--cluster-file', '{cluster_file}', '--expect-cluster-sha256', CLUSTER_ID['cluster_file_sha256'],
                    '--output', '{output}',
                    '--placementprobe', '/opt/placementprobe', '--fdbcli', '/usr/bin/fdbcli',
                    '--fixture-receipts', str(self.fixtures), '--work-dir', str(self.tmp / 'work')]
        case = dict(store='benchfetch/planskip', key='skip3-rows10-forward', placement_preset='planskip',
                    layout='recordlayer', record_format='unsplit')
        argv = r2.placement_argv(template, case, 'after', self.tmp / 'r-after.json', str(self.cluster))
        args = pc.parse_args(argv[3:])
        self.assertEqual((args.store, args.case, args.when, args.preset, args.record_format, args.output),
                         ('benchfetch/planskip', 'skip3-rows10-forward', 'after', 'planskip', 'unsplit',
                          str(self.tmp / 'r-after.json')))
        pc.validate(args)
        sql = dict(case, store='sql/IDX_A', placement_preset='sql', layout='opaque', record_format=None)
        args = pc.parse_args(r2.placement_argv(template, sql, 'before', self.tmp / 'r-sql.json', str(self.cluster))[3:])
        self.assertEqual(args.record_format, '')


class SealRunTest(Temp):
    def test_render_run_substitutes_only_the_runtime_placeholders(self):
        run = ['python3', '-B', '/a/client_campaign_mn.py', 'run', '--manifest-sha256', '{manifest_sha256}',
               '--lifetime-deadline', '{lifetime_deadline}']
        out = seal.render_run(run, 'f' * 64, '2026-10-15T06:00:00Z')
        self.assertEqual(out[-3:], ['f' * 64, '--lifetime-deadline', '2026-10-15T06:00:00Z'])
        for bad in [(run, 'F' * 64, '1792044000'), (run, 'f' * 64, 'tomorrow'), (None, 'f' * 64, '1792044000'),
                    (run + ['{output}'], 'f' * 64, '1792044000'), (run[:-1] + ['x'], 'f' * 64, '1792044000')]:
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                seal.render_run(*bad)

    def stub_scripts(self):
        d = self.tmp / 'stub-scripts'
        d.mkdir()
        (d / 'matrix_plan.py').write_text(
            'def make_plan(args):\n'
            '    arms = []\n'
            '    for a in args.arm:\n'
            '        name, rest = a.split("=", 1)\n'
            '        binary, revision = rest.rsplit("@", 1)\n'
            '        arms.append(dict(name=name, binary=binary, source_revision=revision, command=[]))\n'
            '    return dict(campaign="mx-" + args.campaign_id, seed=args.seed, blocks=args.blocks, arms=arms,\n'
            '                workloads=[dict(name=w, iterations=args.iterations, warmup=args.warmup, workers=1)\n'
            '                           for w in args.workload],\n'
            '                campaign_wall_cap_seconds=args.campaign_wall_cap_seconds, timeout_seconds=args.timeout_seconds)\n')
        (d / 'run.py').write_text(
            'import itertools, math, random\n'
            'def make_schedule(plan, out):\n'
            '    n = len(plan["arms"])\n'
            '    rng = random.Random(plan["seed"])\n'
            '    orders = list(itertools.permutations(range(n))) * (plan["blocks"] // math.factorial(n))\n'
            '    rng.shuffle(orders)\n'
            '    return [dict(block=b, position=p, arm=plan["arms"][i]["name"])\n'
            '            for b in range(plan["blocks"]) for p, i in enumerate(orders[b])]\n')
        return d

    def client_argv(self, out, *extra):
        protocol = dict(
            campaign='mn-test', production_revisions=dict(final='b' * 40),
            runtime=dict(GOMAXPROCS=8, client_vm=dict(housekeeping_cpus='0-1', idle_sentinel_cpus='2-7', client_cpus='8-15')),
            phases=[dict(id='M1w1', kind='client', arms=['final-Go', 'CGo-7.3.77'], workloads=['get100'], blocks=6,
                         workers=1, iterations=3000, warmup=200, order_seed=742001, frozen_order_key='M1w1',
                         per_workload_campaign_wall_cap_seconds=900, per_process_timeout_seconds=180, engine='ssd-2')])
        p, o = self.tmp / 'protocol.json', self.tmp / 'orders.json'
        if not p.exists():
            p.write_text(json.dumps(protocol))
            o.write_text(json.dumps(seal.make_orders(protocol)))
            for name in ['final', 'cgo', 'cluster', 'native', 'server', 'harness.tar.gz', 'adapter.py', 'layout.json',
                         'source-receipt.json']:
                (self.tmp / name).write_text(name)
        return ['client-plans', '--protocol', str(p), '--orders', str(o), '--scripts-dir', str(self.stub_scripts_dir),
                '--cluster-file', str(self.tmp / 'cluster'), '--native-library', str(self.tmp / 'native'),
                '--server-binary', str(self.tmp / 'server'), '--harness-archive', str(self.tmp / 'harness.tar.gz'),
                '--binary', f'final={self.tmp / "final"}', '--binary', f'cgo={self.tmp / "cgo"}',
                '--results-dir', str(self.tmp / 'results'), '--output-dir', str(out), *extra]

    def test_client_manifest_seals_schedule_hash_and_the_r1_run_command(self):
        self.stub_scripts_dir = self.stub_scripts()
        adapter = ['--adapter', str(self.tmp / 'adapter.py'), '--layout', str(self.tmp / 'layout.json'),
                   '--source-receipt', str(self.tmp / 'source-receipt.json'), '--vpc-cidr', '10.88.0.0/24']
        with contextlib.redirect_stdout(io.StringIO()):
            seal.main(self.client_argv(self.tmp / 'sealed', *adapter))
        manifest = json.loads((self.tmp / 'sealed' / 'manifest.json').read_text())
        c = manifest['campaigns'][0]
        self.assertEqual(c['schedule_sha256'], seal.sha256(self.tmp / 'sealed' / 'M1w1' / 'get100' / 'schedule.json'))
        self.assertTrue(Path(c['plan']).is_absolute() and Path(c['schedule']).is_absolute())
        run = c['run']
        self.assertEqual(run[:4], ['python3', '-B', str((self.tmp / 'adapter.py').resolve()), 'run'])
        self.assertNotIn('run.py', ' '.join(run))
        for flag, want in [('--plan', c['plan']), ('--phase', 'M1w1'), ('--workload', 'get100'),
                           ('--vpc-cidr', '10.88.0.0/24'), ('--layout', str((self.tmp / 'layout.json').resolve())),
                           ('--scripts', str(self.stub_scripts_dir.resolve())),
                           ('--output', str((self.tmp / 'results' / 'M1w1' / 'get100').resolve()))]:
            self.assertEqual(run[run.index(flag) + 1], want)
        for path in ['adapter.py', 'layout.json', 'source-receipt.json']:
            self.assertIn(str((self.tmp / path).resolve()), manifest['pinned_files'])
        manifest_sha = seal.sha256(self.tmp / 'sealed' / 'manifest.json')
        final = seal.render_run(run, manifest_sha, '1792044000')
        self.assertEqual(final[final.index('--manifest-sha256') + 1], manifest_sha)
        self.assertIn('render_run', manifest['run_contract'])

    def test_without_adapter_the_run_is_null_and_partial_adapter_flags_refused(self):
        self.stub_scripts_dir = self.stub_scripts()
        with contextlib.redirect_stdout(io.StringIO()):
            seal.main(self.client_argv(self.tmp / 'bare'))
        manifest = json.loads((self.tmp / 'bare' / 'manifest.json').read_text())
        self.assertIsNone(manifest['campaigns'][0]['run'])
        self.assertIn('never invoke run.py directly', manifest['run_contract'])
        with self.assertRaisesRegex(ValueError, 'adapter'):
            seal.main(self.client_argv(self.tmp / 'half', '--adapter', str(self.tmp / 'adapter.py')))

    def test_placement_label_records_the_resolved_receipt_path(self):
        prefix = pr.pack(['benchfetch', 'small'])
        p = probe_doc('benchfetch/small', ('benchfetch', 'small'), [0])
        r = pr.build_receipt(status_doc(), p, pr.PRESETS['benchfetch'], layout='recordlayer', rows=ROWS,
                             record_format='unsplit', fixture=dict(phase='fixture', prefix_bytes=len(prefix), rows=ROWS),
                             expect_prefix=prefix, preset='benchfetch', fixture_sha256='a' * 64,
                             expect_cluster=CLUSTER_ID)
        (self.tmp / 'r.json').write_text(json.dumps(r))
        case = dict(store='benchfetch/small', layout='recordlayer', record_format='unsplit',
                    placement_preset='benchfetch', placement_cell='rows10')
        cwd = os.getcwd()
        os.chdir(self.tmp)
        try:
            label = seal.placement_label(case, {'benchfetch/small': 'r.json'}, CLUSTER_ID)
        finally:
            os.chdir(cwd)
        self.assertEqual(label['placement_receipt'], str((self.tmp / 'r.json').resolve()))
        self.assertEqual(label['placement_label'], 'cross-server')
        other = dict(connection_string_id='other:cluster', cluster_file_sha256='9' * 64)
        with self.assertRaisesRegex(ValueError, 'cluster'):
            seal.placement_label(case, {'benchfetch/small': str(self.tmp / 'r.json')}, other)
        for name, edit in [('diagnostic', lambda d: d['provenance']['addresses_basis'].update(kind='location-cache (diagnostic)')),
                           ('missing provenance', lambda d: d.pop('provenance')),
                           ('dd on', lambda d: d['provenance']['dd_mode']['at_v2'].update(mode=1)),
                           ('no fixture hash', lambda d: d['provenance'].update(fixture_receipt_sha256=None))]:
            doc = json.loads((self.tmp / 'r.json').read_text())
            edit(doc)
            (self.tmp / f'bad-{name}.json').write_text(json.dumps(doc))
            with self.subTest(name), self.assertRaisesRegex(ValueError, 'proves nothing'):
                seal.placement_label(case, {'benchfetch/small': str(self.tmp / f'bad-{name}.json')}, CLUSTER_ID)


if __name__ == '__main__':
    unittest.main(verbosity=1)
