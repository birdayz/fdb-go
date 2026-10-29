"""CPU-controller lifecycle with fake transport; never dispatch real commands."""
import copy
import json
from pathlib import Path
import shlex
import subprocess
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import controller
import test_server_cpu


class Tests(unittest.TestCase):
    def make(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        layout, units, telemetry, _, _ = test_server_cpu.fixture()
        c = controller.Controller.__new__(controller.Controller)
        c.base = Path(temp.name)
        c.folder = c.base / 'phase-controller/M1w1'
        c.folder.mkdir(parents=True)
        c.a = SimpleNamespace(phase='M1w1', protocol='/srv/fdb-bench/protocol.json', diagnostic=False)
        c.label = 'M1w1'
        c.cpu_unit = 'fdb-mn-cpu-M1w1.service'
        c.sampler_unit = 'fdb-mn-sampler-M1w1.service'
        c.cpu_telemetry = '/srv/fdb-bench/telemetry-cpu/M1w1'
        c.cpu_units, c.cpu_starts, c.cpu_intended = units, {}, []
        c.sampler_hash, c.cap = 'a' * 64, 300
        c.errors, c.intents = [], [dict(role=n, unit=c.sampler_unit) for n in controller.legacy.NODES]
        c.events, c.time = [], 0.0
        c.clock = lambda: c.time
        c.sleep = lambda seconds: setattr(c, 'time', c.time + seconds)
        receipts = {}
        for node, (start, stop, _) in telemetry.items():
            for kind, receipt in [('start', start), ('stop', stop)]:
                receipt.update(format=controller.legacy.RECEIPT_FORMAT, sampler_sha256=c.sampler_hash)
                if kind == 'stop': receipt['samples_sha256'] = 'b' * 64
                receipts[node, c.cpu_telemetry + '/receipt-' + kind + '.json'] = receipt
                (c.folder / f'{node}-receipt-{kind}.json').write_text(json.dumps(receipt))
        c.read_json = Mock(side_effect=lambda role, path: copy.deepcopy(receipts[role, path]))
        def call(role, command, **kwargs):
            words = shlex.split(command)
            c.events.append((role, words, kwargs))
            if words[0] == 'systemd-run':
                # Every intent is durable before any potentially lost dispatch.
                intents = json.loads((c.folder / 'intents-6.json').read_text())
                self.assertEqual(sum(i['unit'] == c.cpu_unit for i in intents), 3)
            return subprocess.CompletedProcess(words, 0, stdout='', stderr='')
        c.call = Mock(side_effect=call)
        c.require_phase_budget = Mock()
        c.unit_status = Mock(return_value=dict(ActiveState='active', SubState='running', MainPID='42'))
        c.stop_and_prove = Mock(return_value=True)
        c.pull = Mock(side_effect=lambda role, remote, local: b'synthetic archive')
        return c, layout, receipts

    def test_real_constructor_accepts_external_campaign_and_refuses_checkout_before_writes(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            external = root / 'external'
            checkout = root / 'checkout'
            external.mkdir()
            checkout.mkdir()
            (checkout / '.git').mkdir()
            inside = checkout / 'campaign'
            inside.mkdir()
            for campaign in (external, inside):
                (campaign / 'state.json').write_text(json.dumps(dict(deadline_epoch=1792000300)))
            transport = Mock(side_effect=AssertionError('constructor must not dispatch transport'))
            args = SimpleNamespace(campaign_dir=str(external), phase='M1w1', label=None, diagnostic=False)
            c = controller.Controller(args, transport=transport)
            self.assertEqual(c.base, external.resolve())
            self.assertEqual(c.folder, external / 'phase-controller/M1w1')
            self.assertTrue(c.folder.is_dir())
            self.assertEqual(c.hard_deadline, 1792000300)
            self.assertEqual(c.cpu_unit, 'fdb-mn-cpu-M1w1.service')
            self.assertEqual(c.sampler_unit, 'fdb-mn-sampler-M1w1.service')
            transport.assert_not_called()
            before = {path.relative_to(checkout): path.read_bytes() for path in checkout.rglob('*') if path.is_file()}
            args.campaign_dir = str(inside)
            with self.assertRaisesRegex(controller.mn_common.GuardError, 'campaign directory .*inside a Git work tree'):
                controller.Controller(args, transport=transport)
            self.assertFalse((inside / 'phase-controller').exists())
            self.assertEqual({path.relative_to(checkout): path.read_bytes() for path in checkout.rglob('*') if path.is_file()}, before)
            self.assertEqual(sorted(path.relative_to(checkout).as_posix() for path in checkout.rglob('*')),
                             ['.git', 'campaign', 'campaign/state.json'])
            transport.assert_not_called()

    def start(self, c):
        with patch.object(controller.legacy.Controller, 'start_samplers') as original:
            c.start_samplers()
        original.assert_called_once_with()

    def test_original_r3_unchanged_and_all_three_sidecars_ready_before_runner(self):
        c, _, _ = self.make()
        self.start(c)
        dispatched = [e for e in c.events if e[1][0] == 'systemd-run']
        self.assertEqual([e[0] for e in dispatched], ['node1', 'node2', 'node3'])
        for node, words, _ in dispatched:
            self.assertIn('--cgroup-cpu', words)
            self.assertEqual(words[words.index('--interval') + 1], '0.01')
            self.assertIn('--property=CPUAffinity=0-1', words)
            self.assertIn('--property=RuntimeMaxSec=1200s', words)
            units = [words[i + 1] for i, word in enumerate(words) if word == '--unit']
            self.assertEqual(units, sorted(c.cpu_units[node]))
        self.assertEqual(set(c.cpu_starts), set(controller.legacy.NODES))
        c.require_phase_budget.assert_called_once_with('after CPU sampler readiness')

    def test_collection_filters_legacy_intents_then_restores_and_collects_cpu(self):
        c, _, _ = self.make()
        self.start(c)
        all_intents = copy.deepcopy(c.intents)
        def original_collect():
            self.assertEqual(c.intents, all_intents[:3])
        with patch.object(controller.legacy.Controller, 'collect', side_effect=original_collect):
            c.collect()
        self.assertEqual(c.intents, all_intents)
        self.assertEqual(c.stop_and_prove.call_count, 3)
        self.assertEqual(c.pull.call_count, 3)
        for args in c.pull.call_args_list:
            node, remote, local = args.args
            self.assertEqual(remote, c.cpu_telemetry)
            self.assertEqual(local, c.base / 'telemetry-cpu/M1w1' / node)
        self.assertFalse(c.errors)
        self.assertTrue((c.folder / 'cpu-extension-collection.json').is_file())

    def test_original_collector_exception_does_not_destroy_durable_intents(self):
        c, _, _ = self.make()
        self.start(c)
        all_intents = copy.deepcopy(c.intents)
        with patch.object(controller.legacy.Controller, 'collect', side_effect=RuntimeError('fixture failure')):
            c.collect()
        self.assertEqual(c.intents, all_intents)
        self.assertEqual(c.pull.call_count, 3)
        self.assertIn('original R3 collection', c.errors[0])

    def test_unready_or_foreign_sidecar_never_launches_measured_runner(self):
        for mutation in ('hash', 'membership', 'shared', 'dead'):
            with self.subTest(mutation=mutation):
                c, _, receipts = self.make()
                start = receipts['node1', c.cpu_telemetry + '/receipt-start.json']
                if mutation == 'hash': start['sampler_sha256'] = 'c' * 64
                if mutation == 'membership': next(iter(start['cgroup_units'].values()))['procs'].append(9999)
                if mutation == 'shared':
                    values = list(start['cgroup_units'].values())
                    values[1]['inode'] = values[0]['inode']
                if mutation == 'dead': c.unit_status.return_value['ActiveState'] = 'failed'
                c.preflight = Mock()
                c.client_receipt, c.execute_runner = Mock(), Mock()
                c.finish_runner = Mock(return_value=True)
                c.collect, c.reconcile = Mock(), Mock()
                c.runner_rc, c.reconcile_rc = None, None
                c.complete = Mock(side_effect=lambda code, outcome: code)
                with patch.object(controller.legacy.Controller, 'start_samplers'):
                    self.assertEqual(c.execute(), 2)
                c.execute_runner.assert_not_called()
                c.client_receipt.assert_not_called()
                c.finish_runner.assert_called_once_with()
                c.collect.assert_called_once_with()

    def test_partial_start_failures_stop_all_intended_units_and_collect_real_adverse_raw(self):
        for failure in ('lost_ack', 'readiness_timeout', 'lifetime_after_ready'):
            with self.subTest(failure=failure):
                c, _, _ = self.make()
                transport = c.call.side_effect
                def call(role, command, **kwargs):
                    reply = transport(role, command, **kwargs)
                    words = shlex.split(command)
                    if failure == 'lost_ack' and role == 'node2' and words[0] == 'systemd-run':
                        raise OSError('lost launch acknowledgement after remote dispatch')
                    if failure == 'readiness_timeout' and words[0] == 'test':
                        return subprocess.CompletedProcess(words, 1, stdout='', stderr='not ready')
                    return reply
                c.call.side_effect = call
                if failure == 'lifetime_after_ready':
                    c.require_phase_budget.side_effect = controller.legacy.Refused('insufficient phase lifetime')
                c.preflight, c.client_receipt, c.execute_runner = Mock(), Mock(), Mock()
                c.finish_runner, c.reconcile = Mock(return_value=True), Mock()
                c.runner_rc, c.reconcile_rc = None, None
                c.complete = Mock(side_effect=lambda code, outcome: code)
                with patch.object(controller.legacy.Controller, 'start_samplers'), \
                     patch.object(controller.legacy.Controller, 'collect') as original_collect:
                    self.assertEqual(c.execute(), 2)
                original_collect.assert_called_once_with()
                c.execute_runner.assert_not_called()
                c.client_receipt.assert_not_called()
                self.assertEqual(c.cpu_intended, [(node, c.cpu_unit) for node in ('node1', 'node2', 'node3')])
                self.assertEqual(c.stop_and_prove.call_args_list,
                                 [unittest.mock.call(node, c.cpu_unit, 180) for node in ('node1', 'node2', 'node3')])
                self.assertEqual([call.args[0] for call in c.pull.call_args_list], ['node1', 'node2', 'node3'])
                self.assertEqual(set(c.cpu_starts), set(controller.legacy.NODES) if failure == 'lifetime_after_ready' else set())
                self.assertTrue(any('orchestration:' in error for error in c.errors))
                receipt = json.loads((c.folder / 'cpu-extension-collection.json').read_text())
                self.assertEqual(receipt['intents'], [[node, c.cpu_unit] for node in ('node1', 'node2', 'node3')])
                if failure == 'readiness_timeout':
                    self.assertGreaterEqual(c.time, 90)
                    self.assertTrue(any('never ready' in error for error in c.errors))

    def test_bad_cpu_stop_receipt_still_collects_adverse_raw_artifacts(self):
        c, _, receipts = self.make()
        self.start(c)
        receipts['node1', c.cpu_telemetry + '/receipt-stop.json']['samples_sha256'] = 'not-a-hash'
        with patch.object(controller.legacy.Controller, 'collect'):
            c.collect()
        self.assertEqual(c.pull.call_count, 3)
        self.assertTrue(any('CPU-stop-receipt' in e for e in c.errors))

    def test_cpu_stop_unknown_or_missing_receipt_never_assumed_collected(self):
        c, _, _ = self.make()
        self.start(c)
        def stop(node, unit, seconds):
            if node == 'node1':
                c.errors.append('node1 CPU unit not proven quiescent')
                return False
            return True
        c.stop_and_prove.side_effect = stop
        with patch.object(controller.legacy.Controller, 'collect'):
            c.collect()
        self.assertEqual(c.pull.call_count, 2)
        self.assertIn('not proven quiescent', c.errors[0])

    def test_positive_preflight_checks_every_new_unit_and_pins(self):
        c, layout, _ = self.make()
        protocol = dict(measurement_extensions=dict(server_cpu_source='exclusive-cgroup-v2-usage-usec',
            server_cpu_metric='observed-cgroup-accounting-delta', server_cpu_actual_window_available=False,
            process_cpu_interval_ns=10_000_000, process_cpu_min_inner_intervals=10, process_cpu_max_time_envelope_ratio=1.2,
                process_cpu_clock_reserve_ns=2_000_000, process_cpu_phase_correlation_range_ns=1_000_000))
        c.read_json.side_effect = lambda role, path: protocol if path == c.a.protocol else layout
        sources = {
            'tools/benchcampaign/campaigns/fdbmn-rfc282/controller.py': Path(controller.__file__),
            'tools/benchcampaign/campaigns/fdbmn-rfc282/server_cpu.py': Path(controller.server_cpu.__file__),
            'tools/benchcampaign/campaigns/fdbmn-20261020/execution/run_phase_ops60.py': Path(controller.legacy.__file__),
            'tools/benchcampaign/campaigns/fdbmn-20261020/execution/remote_ops60.py': Path(controller.remote_ops60.__file__),
        }
        c.pins = {controller.legacy.R + '/tooling/' + p: controller.legacy.sha(f.read_bytes()) for p, f in sources.items()}
        c.require_fresh_unit = Mock()
        with patch.object(controller.legacy.Controller, 'preflight'):
            c.preflight()
        self.assertEqual(c.require_fresh_unit.call_count, 3)
        self.assertEqual(c.call.call_count, 4)
        self.assertTrue((c.folder / 'cpu-extension-pins.json').is_file())
        c.require_phase_budget.assert_called_once_with('after CPU-sidecar preflight')

    def test_preflight_refuses_source_swap_or_unsealed_controller_before_dispatch(self):
        mutations = {'server_cpu_source': 'host-proc-stat', 'server_cpu_metric': 'actual-execution-window-cpu',
                     'server_cpu_actual_window_available': True, 'process_cpu_interval_ns': 20_000_000,
                     'process_cpu_min_inner_intervals': 1, 'process_cpu_max_time_envelope_ratio': 2,
                     'process_cpu_clock_reserve_ns': 0, 'process_cpu_phase_correlation_range_ns': 2_000_000,
                     'pin': None}
        for mutation, replacement in mutations.items():
            c, layout, _ = self.make()
            protocol = dict(measurement_extensions=dict(server_cpu_source='exclusive-cgroup-v2-usage-usec',
                server_cpu_metric='observed-cgroup-accounting-delta', server_cpu_actual_window_available=False,
                process_cpu_interval_ns=10_000_000, process_cpu_min_inner_intervals=10, process_cpu_max_time_envelope_ratio=1.2,
                process_cpu_clock_reserve_ns=2_000_000, process_cpu_phase_correlation_range_ns=1_000_000))
            c.read_json.side_effect = lambda role, path: protocol if path == c.a.protocol else layout
            sources = {
                'tools/benchcampaign/campaigns/fdbmn-rfc282/controller.py': Path(controller.__file__),
                'tools/benchcampaign/campaigns/fdbmn-rfc282/server_cpu.py': Path(controller.server_cpu.__file__),
                'tools/benchcampaign/campaigns/fdbmn-20261020/execution/run_phase_ops60.py': Path(controller.legacy.__file__),
                'tools/benchcampaign/campaigns/fdbmn-20261020/execution/remote_ops60.py': Path(controller.remote_ops60.__file__),
            }
            c.pins = {controller.legacy.R + '/tooling/' + p: controller.legacy.sha(f.read_bytes()) for p, f in sources.items()}
            if mutation == 'pin':
                c.pins[controller.legacy.R + '/tooling/tools/benchcampaign/campaigns/fdbmn-rfc282/controller.py'] = '0' * 64
            else:
                protocol['measurement_extensions'][mutation] = replacement
            c.require_fresh_unit = Mock()
            with patch.object(controller.legacy.Controller, 'preflight'):
                with self.assertRaisesRegex(controller.legacy.Refused,
                                            'unsealed controller/helper' if mutation == 'pin' else 'unsealed CPU source/policy'):
                    c.preflight()
            c.call.assert_not_called()
            c.require_fresh_unit.assert_not_called()

    def test_ssh_created_id_protected_id_and_pinned_host_key_boundary(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            (base / 'inventory').mkdir()
            (base / 'inventory/protect.tsv').write_text('server\t99\tprotected\n')
            (base / 'campaign.json').write_text(json.dumps(dict(prefix='campaign')))
            state = dict(campaign_server_ids=[12], ssh_private_key='controller-only-key',
                         hosts=dict(node1=dict(id=12, name='campaign-node1', public_ipv4='192.0.2.1')))
            (base / 'state.json').write_text(json.dumps(state))
            transport = controller.CampaignSSH(base)
            with patch.object(controller.subprocess, 'run') as run:
                transport('node1', 'safe-fixture-command')
                words = run.call_args.args[0]
                self.assertIn('StrictHostKeyChecking=yes', words)
                self.assertIn('HostKeyAlias=campaign-node1', words)
                self.assertEqual(words[-1], 'safe-fixture-command')
            owned = copy.deepcopy(state)
            for mutation in ('unowned', 'protected', 'name', 'role'):
                with self.subTest(mutation=mutation):
                    state = copy.deepcopy(owned)
                    role = 'node1'
                    if mutation == 'unowned': state['hosts']['node1']['id'] = 13
                    if mutation == 'protected':
                        state['hosts']['node1']['id'] = 99
                        state['campaign_server_ids'].append(99)
                    if mutation == 'name': state['hosts']['node1']['name'] = 'another-campaign-node1'
                    if mutation == 'role': role = 'unexpected-role'
                    (base / 'state.json').write_text(json.dumps(state))
                    with patch.object(controller.subprocess, 'run') as run:
                        with self.assertRaisesRegex(controller.legacy.Refused,
                                                    'unknown campaign role' if mutation == 'role' else 'unowned/protected SSH target'):
                            transport(role, 'never-dispatch')
                        run.assert_not_called()


if __name__ == '__main__':
    unittest.main()
