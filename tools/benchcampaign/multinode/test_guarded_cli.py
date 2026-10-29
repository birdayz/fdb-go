#!/usr/bin/env python3
"""CLI glue through real Campaign/Journal controls and a synthetic provider.

Only constructors are redirected to the fixture's existing Campaign (to retain
its injected clocks), real Profile, and fake provider. No authorization, pin,
arming, deadline, ownership, cascade, action or completion method is stubbed.
"""
import contextlib
import copy
import io
import json
import os
from pathlib import Path
import sys
import unittest
from unittest import mock

HERE = Path(os.path.abspath(__file__)).parent
sys.path.insert(0, str(HERE))

import campaign_cloud as cc  # noqa: E402
import campaign_safety as cs  # noqa: E402
import creation_ledger as cl  # noqa: E402
import mn_common as mc  # noqa: E402
import primary_ip_check as pic  # noqa: E402
import provision  # noqa: E402
import teardown_guard as tg  # noqa: E402
import test_campaign_safety as safety  # noqa: E402


class CLIProvider(safety.Provider):
    """The CLI's host receipt also needs private-network provider readback."""

    def request(self, method, path, body=None, seconds=20):
        reply = super().request(method, path, body=body, seconds=seconds)
        if method == 'POST' and path.endswith('/actions/attach_to_network'):
            rid = int(path.split('/')[2])
            self.rows['server'][rid]['private_net'] = [dict(network=body['network'], ip=body['ip'])]
        return reply


def fixture(arm=True):
    """Select CLI names before the immutable header/anchor is ever initialized."""
    initialize = cl.Journal.initialize
    names = {'test-node0': 'test-client', 'test-firewall': 'test-fw', 'test-ssh-key': 'test-controller'}
    def initialize_cli(path, header, state):
        for entry in header['plan']:
            entry['name'] = names.get(entry['name'], entry['name'])
            descriptor = entry['descriptor']
            descriptor['name'] = names.get(descriptor['name'], descriptor['name'])
            if entry['kind'] == 'server':
                descriptor['ssh_keys'] = [names.get(name, name) for name in descriptor['ssh_keys']]
                descriptor['firewalls'] = [dict(firewall=names.get(item['firewall'], item['firewall']))
                                           for item in descriptor['firewalls']]
                if descriptor['labels']['role'] == 'node0':
                    descriptor['labels']['role'] = 'client'
        return initialize(path, header, state)
    with mock.patch.object(cl.Journal, 'initialize', side_effect=initialize_cli), \
            mock.patch.object(safety, 'Provider', CLIProvider):
        result = safety.Fixture(arm=arm)
    # These are submitted request fixtures, not post-anchor journal/header edits.
    for _, body in result.bodies:
        body['name'] = names.get(body['name'], body['name'])
        if body['labels']['role'] == 'node0':
            body['labels']['role'] = 'client'
    return result


@contextlib.contextmanager
def constructors(f):
    with mock.patch.object(cc, 'Profile', return_value=f.cloud.profile) as profile, \
            mock.patch.object(cc, 'Cloud', return_value=f.cloud) as cloud, \
            mock.patch.object(cs, 'Campaign', return_value=f.campaign) as campaign:
        yield profile, cloud, campaign


class GuardedCLITest(unittest.TestCase):
    def setUp(self):
        self.f = fixture()
        self.addCleanup(self.f.close)

    def provision_args(self, *extra):
        f = self.f
        public_key = f.root / 'controller.pub'
        public_key.write_text(f.body('ssh-key')['public_key'] + '\n')
        public_key.chmod(0o600)
        userdata = f.root / 'cloud-init'
        userdata.mkdir(exist_ok=True, mode=0o700)
        for kind, body in f.bodies:
            if kind == 'server':
                path = userdata / (body['name'] + '.user-data')
                path.write_text(body['user_data'])
                path.chmod(0o600)
        work = f.root / 'work'
        work.mkdir(exist_ok=True, mode=0o700)
        return provision.parse_args([
            '--label', 'campaign=synthetic', '--prefix', 'test', '--network-name', 'test-network',
            '--controller-cidr', '203.0.113.7/32', '--ssh-public-key', str(public_key),
            '--cloud-init-dir', str(userdata), '--work-dir', str(work),
            '--creation-ledger', str(f.journal.path), '--teardown-deadline', str(f.header['time']['g_ns'] // cs.NS),
            '--max-lifetime-hours', '16', '--image', '161547269', '--guard-wait-seconds', '0.001', '--execute', *extra])

    def teardown_args(self, *extra):
        return tg.parse_args(['--label', 'campaign=synthetic', '--prefix', 'test',
                              '--creation-ledger', str(self.f.journal.path), '--execute', *extra])

    def ip_args(self, *extra):
        return pic.parse_args(['--label', 'campaign=synthetic', '--prefix', 'test', '--role', 'client',
                               '--creation-ledger', str(self.f.journal.path),
                               '--state', str(self.f.root / 'state.json'),
                               '--owned-output', str(self.f.root / 'owned.json'), *extra])

    def plan(self):
        return [dict(kind=kind, name=body['name'], body=copy.deepcopy(body),
                     **({'private_ip': self.f.header['plan'][index]['private_ip']} if kind == 'server' else {}))
                for index, (kind, body) in enumerate(self.f.bodies)]

    def assert_no_new_mutations(self, before):
        self.assertEqual(self.f.cloud.requests, before)

    def stop_unexpected_wait(self, seconds):
        self.fail('unexpected wait instead of a bounded successful/refused path')

    def replacement_cleanup_actor(self):
        f = self.f
        f.closing()
        # The original guard really is gone in the synthetic process table. A
        # distinct supervised guard must rearm through the real recovery checks.
        f.processes.pop(112)
        f.processes[113] = dict(pid=113, parent=110, starttime='1130', argv_sha256=cc.digest(b'113'))
        f.actor = 113

    def assert_main_private_boundary_faults_are_sanitized(self, module, argv, boundary_module, target, prefix):
        f = self.f
        real_open = boundary_module.open
        state = copy.deepcopy(f.journal.read())
        checkpoint = copy.deepcopy(f.journal.checkpoint)
        requests = copy.deepcopy(f.cloud.requests)
        reads = copy.deepcopy((f.cloud.lists, f.cloud.describes, f.cloud.action_reads))
        for exception_type in (OSError, ValueError, KeyError, TypeError):
            with self.subTest(cli=module.__name__, exception=exception_type.__name__):
                sentinel = 'SYNTHETIC_PRIVATE_EXCEPTION_' + module.__name__ + '_' + exception_type.__name__ + '_8e6a7fd9'
                error = exception_type(sentinel)
                self.assertIn(sentinel, str(error))
                attempts = []
                def open_fault(path, *args, **kwargs):
                    if isinstance(path, (str, os.PathLike)) and Path(path) == target:
                        attempts.append(str(path))
                        raise error
                    return real_open(path, *args, **kwargs)
                stdout, stderr = io.StringIO(), io.StringIO()
                # Fault a concrete private-input filesystem read, not main,
                # parsing, run/check, authorization, or exception rendering.
                with constructors(f) as (profile, cloud, campaign), \
                        mock.patch.object(boundary_module, 'open', side_effect=open_fault), \
                        contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
                    status = module.main(argv)
                    profile.assert_not_called()
                    cloud.assert_not_called()
                    campaign.assert_not_called()
                self.assertEqual(attempts, [str(target)])
                self.assertEqual(status, 2)
                self.assertEqual(stdout.getvalue(), '')
                self.assertEqual(stderr.getvalue(), prefix + exception_type.__name__ + '\n')
                for output in (stdout.getvalue(), stderr.getvalue()):
                    self.assertNotIn(sentinel, output)
                    self.assertNotIn('Traceback', output)
                self.assertEqual(f.journal.read(), state)
                self.assertEqual(f.journal.checkpoint, checkpoint)
                self.assert_no_new_mutations(requests)
                self.assertEqual((f.cloud.lists, f.cloud.describes, f.cloud.action_reads), reads)

    def test_provision_main_sanitizes_each_private_public_key_read_failure_class(self):
        f = self.f
        prepared = self.provision_args()
        argv = ['--label', 'campaign=synthetic', '--prefix', 'test', '--network-name', 'test-network',
            '--controller-cidr', '203.0.113.7/32', '--ssh-public-key', prepared.ssh_public_key,
            '--cloud-init-dir', prepared.cloud_init_dir, '--work-dir', prepared.work_dir,
            '--creation-ledger', str(f.journal.path), '--teardown-deadline', str(f.header['time']['g_ns'] // cs.NS),
            '--max-lifetime-hours', '16', '--image', '161547269', '--execute']
        # Only the wall-clock boundary is fixed; actual run validates the real
        # private documents and reaches check_execute_inputs' public-key read.
        with mock.patch.object(provision.time, 'time', return_value=f.wall // cs.NS):
            self.assert_main_private_boundary_faults_are_sanitized(
                provision, argv, io, Path(prepared.ssh_public_key), 'REFUSED: ')

    def test_primary_ip_main_sanitizes_each_private_state_read_failure_class(self):
        f = self.f
        sid = f.create('server', 0)
        f.cloud.rows['server'][sid]['private_net'] = []
        previous = f.root / 'main-private-state.json'
        previous.write_text('{}\n')
        previous.chmod(0o600)
        owned = f.root / 'main-private-owned.json'
        argv = ['--label', 'campaign=synthetic', '--prefix', 'test', '--role', 'client',
            '--creation-ledger', str(f.journal.path), '--state', str(previous), '--owned-output', str(owned)]
        self.assert_main_private_boundary_faults_are_sanitized(pic, argv, os, previous,
            'STOP: ownership check failed; retain IDs for the owner; never change IPs: ')
        self.assertEqual(previous.read_bytes(), b'{}\n')
        self.assertFalse(owned.exists())

    def test_teardown_main_sanitizes_each_private_journal_open_failure_class(self):
        f = self.f
        f.create('network')
        argv = ['--label', 'campaign=synthetic', '--prefix', 'test',
            '--creation-ledger', str(f.journal.path), '--execute']
        self.assert_main_private_boundary_faults_are_sanitized(
            tg, argv, os, f.journal.path.parent, 'REFUSED: ')
        self.assertFalse((f.guard_directory / 'stop.json').exists())

    def test_provision_execute_guarded_creates_all_seven_and_records_exact_hosts(self):
        f = self.f
        args = self.provision_args()
        with constructors(f) as (_, _, campaign_constructor), \
                mock.patch.object(provision.time, 'monotonic', side_effect=lambda: f.mono / cs.NS):
            result = provision.execute_guarded(args, f.journal, f.cloud.profile, f.cloud, self.plan(), io.StringIO())
        campaign_constructor.assert_called_once_with(f.journal, f.cloud)
        self.assertTrue(result['executed'])
        self.assertEqual(set(result['hosts']), {'client', 'node1', 'node2', 'node3'})
        self.assertEqual({host['id'] for host in result['hosts'].values()}, {101, 102, 103, 104})
        self.assertEqual({host['name'] for host in result['hosts'].values()},
                         {'test-client', 'test-node1', 'test-node2', 'test-node3'})
        self.assertEqual(json.loads((Path(args.work_dir) / 'hosts-all.json').read_text()), result['hosts'])
        state = f.journal.read()
        self.assertEqual(len(state['owned']), 7)
        self.assertEqual(len(state['associations']), 4)
        self.assertTrue(all(intent['state'] == 'DONE' for intent in state['intents']))
        self.assertEqual(state['events'][-1]['event'], 'provisioning-complete')
        creates = [(method, path) for method, path, _, _ in f.cloud.requests if path.count('/') == 1]
        self.assertEqual(creates, [('POST', '/networks'), ('POST', '/firewalls'), ('POST', '/ssh_keys'),
                                  *[('POST', '/servers')] * 4])
        self.assertEqual(len(f.cloud.requests), 16)  # seven creates, subnet, four attaches, four power-ons
        self.assertEqual([path for _, path, _, _ in f.cloud.requests[-4:]],
                         [f'/servers/{rid}/actions/poweron' for rid in (101, 102, 103, 104)])
        self.assertEqual(f.deletes(), [])

    def test_provision_profile_mismatch_refuses_without_mutation(self):
        f = self.f
        args = self.provision_args()
        alternate = f.root / 'alternate.toml'
        alternate.write_bytes(f.config.read_bytes())
        alternate.chmod(0o600)
        different = cc.Profile(config=alternate, environ={})
        with constructors(f), self.assertRaisesRegex(mc.GuardError, 'profile differs'):
            provision.execute_guarded(args, f.journal, different, f.cloud, self.plan(), io.StringIO())
        self.assertEqual(f.cloud.requests, [])
        self.assertEqual(f.journal.read()['intents'], [])

    def test_provision_changed_plan_refuses_and_requests_existing_guard(self):
        f = self.f
        args, plan = self.provision_args(), self.plan()
        plan[0]['body']['ip_range'] = '10.99.0.0/24'
        with constructors(f), mock.patch.object(provision.time, 'monotonic', side_effect=lambda: f.mono / cs.NS), \
                self.assertRaisesRegex(mc.GuardError, 'sealed plan'):
            provision.execute_guarded(args, f.journal, f.cloud.profile, f.cloud, plan, io.StringIO())
        self.assertEqual(f.cloud.requests, [])
        self.assertEqual(f.journal.read()['intents'], [])
        stop = json.loads((f.guard_directory / 'stop.json').read_text())
        self.assertEqual(stop['binding'], f.journal.binding)

    def test_provision_lost_create_response_records_unknown_without_retry_and_requests_stop(self):
        f = self.f
        args = self.provision_args()
        f.cloud.lose_ack = True
        with constructors(f), mock.patch.object(provision.time, 'monotonic', side_effect=lambda: f.mono / cs.NS), \
                self.assertRaisesRegex(mc.GuardError, 'failed or ambiguous'):
            provision.execute_guarded(args, f.journal, f.cloud.profile, f.cloud, self.plan(), io.StringIO())
        self.assertEqual([(method, path) for method, path, _, _ in f.cloud.requests], [('POST', '/networks')])
        self.assertIn(201, f.cloud.rows['network'])
        state = f.journal.read()
        self.assertEqual(len(state['intents']), 1)
        self.assertEqual(state['intents'][0]['state'], 'UNKNOWN')
        self.assertEqual(state['owned'], [])
        self.assertTrue((f.guard_directory / 'stop.json').exists())

    def test_provision_closed_admission_never_waits_into_a_create(self):
        f = self.f
        args = self.provision_args()
        f.closing()
        with constructors(f), mock.patch.object(provision.time, 'monotonic', side_effect=[0, 1]), \
                self.assertRaisesRegex(mc.GuardError, 'no create permitted'):
            provision.execute_guarded(args, f.journal, f.cloud.profile, f.cloud, self.plan(), io.StringIO())
        self.assertEqual(f.cloud.requests, [])
        self.assertEqual(f.journal.read()['mode'], 'CLOSING')
        self.assertTrue((f.guard_directory / 'stop.json').exists())

    def test_provision_run_without_ledger_never_uses_cli_or_cloud(self):
        args = self.provision_args()
        args.creation_ledger = None
        runner = mock.Mock(side_effect=AssertionError('legacy CLI must not run'))
        with self.assertRaisesRegex(mc.GuardError, 'requires --creation-ledger'):
            provision.run(args, runner=runner, now=self.f.wall // cs.NS, out=io.StringIO())
        runner.assert_not_called()
        self.assertEqual(self.f.cloud.requests, [])

    def test_teardown_execute_guarded_rearms_real_guard_and_deletes_seven_in_order(self):
        f = self.f
        f.create_all()
        self.replacement_cleanup_actor()
        args = self.teardown_args('--receipt-dir', str(f.root / 'receipts'))
        with constructors(f):
            ok, receipt = tg.execute_guarded(args, f.journal, sleep=self.stop_unexpected_wait, out=io.StringIO())
        self.assertTrue(ok)
        self.assertEqual(f.deletes(), [('DELETE', f'/servers/{rid}') for rid in (101, 102, 103, 104)] +
                         [('DELETE', '/firewalls/301'), ('DELETE', '/ssh_keys/401'), ('DELETE', '/networks/201')])
        self.assertTrue(receipt['result']['complete'])
        self.assertTrue(receipt['result']['baseline_equal'])
        self.assertTrue(receipt['result']['hard_lifetime_met'])
        self.assertEqual(receipt['result']['pending_intents'], [])
        self.assertEqual(f.journal.read()['mode'], 'COMPLETE')
        for kind in safety.INVENTORY_KINDS:
            self.assertEqual(sorted(f.cloud.rows[kind]), f.header['baseline'][kind])
        receipts = list((f.root / 'receipts').glob('teardown-*.json'))
        self.assertEqual(len(receipts), 1)
        self.assertEqual(json.loads(receipts[0].read_text()), receipt)

    def test_teardown_cross_kind_protected_id_does_not_protect_owned_server(self):
        f = self.f
        f.create_all()
        protect = f.root / 'cross-kind-protect.tsv'
        protect.write_text('network\t101\tunrelated-network\n')
        self.replacement_cleanup_actor()
        args = self.teardown_args('--protect-file', str(protect))
        self.assertIn(101, args.protect_id)  # historical dry-run flattened view
        self.assertEqual(args.explicit_protect_id, [])
        self.assertEqual(args.typed_protection['server'], [])
        self.assertEqual(args.typed_protection['network'], [101])
        with constructors(f):
            ok, receipt = tg.execute_guarded(args, f.journal, sleep=self.stop_unexpected_wait, out=io.StringIO())
        self.assertTrue(ok)
        self.assertTrue(receipt['result']['complete'])
        self.assertEqual(len(f.deletes()), 7)
        self.assertIn(('DELETE', '/servers/101'), f.deletes())
        self.assertEqual(f.journal.read()['mode'], 'COMPLETE')

    def test_teardown_same_kind_protect_file_conflict_refuses_before_delete(self):
        f = self.f
        sid = f.create('server', 0)
        protect = f.root / 'same-kind-protect.tsv'
        protect.write_text(f'server\t{sid}\t\n')
        before = copy.deepcopy(f.cloud.requests)
        args = self.teardown_args('--protect-file', str(protect))
        self.assertEqual(args.typed_protection['server'], [sid])
        with constructors(f), self.assertRaisesRegex(mc.GuardError, 'additional guard protection'):
            tg.execute_guarded(args, f.journal, sleep=self.stop_unexpected_wait, out=io.StringIO())
        self.assert_no_new_mutations(before)
        self.assertIn(sid, f.cloud.rows['server'])
        self.assertEqual(f.deletes(), [])

    def test_teardown_additional_protected_owned_id_refuses_before_any_delete(self):
        f = self.f
        rid = f.create('network')
        before = copy.deepcopy(f.cloud.requests)
        with constructors(f), self.assertRaisesRegex(mc.GuardError, 'additional guard protection'):
            tg.execute_guarded(self.teardown_args('--protect-id', str(rid)), f.journal,
                               sleep=self.stop_unexpected_wait, out=io.StringIO())
        self.assert_no_new_mutations(before)
        self.assertIn(rid, f.cloud.rows['network'])
        self.assertEqual(f.journal.read()['mode'], 'OPEN')

    def test_teardown_changed_campaign_prefix_or_deadline_refuses_without_mutation(self):
        f = self.f
        cases = [('--label', 'campaign=other'), ('--prefix', 'other'),
                 ('--deadline', str(f.header['time']['g_ns'] // cs.NS + 1))]
        for extra in cases:
            with self.subTest(extra=extra), constructors(f), self.assertRaisesRegex(mc.GuardError, 'binding|deadline'):
                tg.execute_guarded(self.teardown_args(*extra), f.journal,
                                   sleep=self.stop_unexpected_wait, out=io.StringIO())
            self.assertEqual(f.cloud.requests, [])

    def test_teardown_changed_pinned_source_refuses_before_cleanup(self):
        f = self.f
        f.create('network')
        before = copy.deepcopy(f.cloud.requests)
        source = next(iter(f.header['sources']))
        Path(source).write_text('# changed\n')
        with constructors(f), self.assertRaisesRegex(mc.GuardError, 'pinned guard source changed'):
            tg.execute_guarded(self.teardown_args(), f.journal, sleep=self.stop_unexpected_wait, out=io.StringIO())
        self.assert_no_new_mutations(before)
        self.assertEqual(f.deletes(), [])

    def test_teardown_wait_bounds_refuse_before_guard_can_send(self):
        f = self.f
        for seconds in ('0', '-1', '61', 'nan', 'inf'):
            with self.subTest(seconds=seconds), constructors(f), self.assertRaisesRegex(mc.GuardError, 'bounded'):
                tg.execute_guarded(self.teardown_args('--request-wait-seconds', seconds), f.journal,
                                   sleep=self.stop_unexpected_wait, out=io.StringIO())
            self.assertEqual(f.cloud.requests, [])

    def test_immediate_teardown_requests_live_guard_without_starting_second_sender(self):
        f = self.f
        f.create('network')
        before = copy.deepcopy(f.cloud.requests)
        args = self.teardown_args('--immediate', '--request-wait-seconds', '0.001')
        with constructors(f), mock.patch.object(tg.time, 'monotonic', side_effect=[0, 1]):
            ok, receipt = tg.execute_guarded(args, f.journal, sleep=self.stop_unexpected_wait, out=io.StringIO())
        self.assertFalse(ok)
        self.assertEqual(receipt['result'], dict(complete=False, hard_lifetime_met=False,
                                                 status='cleanup-request-pending'))
        self.assert_no_new_mutations(before)
        self.assertEqual(f.deletes(), [])
        self.assertEqual(f.journal.read()['mode'], 'OPEN')
        self.assertEqual(json.loads((f.guard_directory / 'stop.json').read_text())['binding'], f.journal.binding)

    def test_teardown_run_without_ledger_refuses_before_wait_or_cli(self):
        args = self.teardown_args('--deadline', str(self.f.header['time']['g_ns'] // cs.NS))
        args.creation_ledger = None
        runner = mock.Mock(side_effect=AssertionError('legacy CLI must not run'))
        with self.assertRaisesRegex(mc.GuardError, 'requires --creation-ledger'):
            tg.run(args, runner=runner, clock=lambda: self.f.wall // cs.NS,
                   sleep=self.stop_unexpected_wait, out=io.StringIO())
        runner.assert_not_called()
        self.assertEqual(self.f.cloud.requests, [])

    def test_primary_ip_check_records_only_real_owned_server_and_subordinate(self):
        f = self.f
        sid = f.create('server', 0)
        f.cloud.rows['server'][sid]['private_net'] = [dict(ip=f.header['plan'][3]['private_ip'])]
        before = copy.deepcopy(f.cloud.requests)
        args = self.ip_args()
        with constructors(f):
            self.assertEqual(pic.check(args), ([101], [10101]))
        self.assert_no_new_mutations(before)
        state = json.loads(Path(args.state).read_text())
        owned = json.loads(Path(args.owned_output).read_text())
        self.assertEqual(state['campaign_server_ids'], [101])
        self.assertEqual(state['campaign_primary_ip_ids'], [10101])
        self.assertEqual(state['status'], 'provisioned-through-client')
        self.assertEqual(state['hosts']['client']['id'], 101)
        self.assertFalse(owned['direct_primary_ip_deletion_authorized'])
        self.assertEqual(owned['server_ids'], [101])
        self.assertEqual([(a['server_id'], a['id']) for a in owned['primary_ips']], [(101, 10101)])
        self.assertEqual(f.journal.read()['events'][-1]['event'], 'primary-ip-check')
        self.assertEqual(Path(args.state).stat().st_mode & 0o777, 0o600)
        self.assertEqual(Path(args.owned_output).stat().st_mode & 0o777, 0o600)

    def test_primary_ip_binding_refusals_leave_healthy_ledger_usable_for_matching_check(self):
        f = self.f
        sid = f.create('server', 0)
        f.cloud.rows['server'][sid]['private_net'] = [dict(ip=f.header['plan'][3]['private_ip'])]
        self.assertTrue(f.campaign.poll_actions())
        state = copy.deepcopy(f.journal.read())
        checkpoint = copy.deepcopy(f.journal.checkpoint)
        requests = copy.deepcopy(f.cloud.requests)
        reads = copy.deepcopy((f.cloud.lists, f.cloud.describes, f.cloud.action_reads))
        rows = copy.deepcopy(f.cloud.rows)
        actions = copy.deepcopy(f.cloud.actions)
        def snapshot():
            result = {}
            for path in sorted(f.root.rglob('*')):
                info = path.lstat()
                result[str(path.relative_to(f.root))] = (
                    info.st_dev, info.st_ino, info.st_mode, path.read_bytes() if path.is_file() else None)
            return result
        tree = snapshot()
        for option, value in (('--label', 'campaign=other'), ('--prefix', 'other')):
            with self.subTest(option=option):
                args = self.ip_args(option, value)
                # Change one public binding at a time; the other remains valid.
                self.assertEqual(mc.parse_campaign_label(args.label), 'other' if option == '--label' else 'synthetic')
                self.assertEqual(args.prefix, 'other' if option == '--prefix' else 'test')
                with constructors(f) as (profile, cloud, campaign):
                    with self.assertRaisesRegex(mc.GuardError, '^primary-IP campaign binding differs$'):
                        pic.check(args)
                    profile.assert_not_called()
                    cloud.assert_not_called()
                    campaign.assert_not_called()
                self.assertEqual(f.journal.read(), state)
                self.assertEqual(f.journal.checkpoint, checkpoint)
                self.assertEqual(snapshot(), tree)
                self.assert_no_new_mutations(requests)
                self.assertEqual((f.cloud.lists, f.cloud.describes, f.cloud.action_reads), reads)
                self.assertEqual(f.cloud.rows, rows)
                self.assertEqual(f.cloud.actions, actions)
                self.assertFalse(Path(args.state).exists())
                self.assertFalse(Path(args.owned_output).exists())

        # The same authenticated ledger and provider metadata must work when
        # both caller bindings match, not merely refuse for some other reason.
        args = self.ip_args()
        with constructors(f) as (profile, cloud, campaign):
            self.assertEqual(pic.check(args), ([101], [10101]))
            profile.assert_called_once_with(f.header['context']['context'], f.header['context']['config'])
            cloud.assert_called_once_with(f.cloud.profile, f.header['context'])
            campaign.assert_called_once()
        self.assert_no_new_mutations(requests)
        self.assertEqual(f.cloud.rows, rows)
        self.assertEqual(f.cloud.actions, actions)
        self.assertGreater(len(f.cloud.describes), len(reads[1]))
        self.assertGreater(len(f.cloud.lists), len(reads[0]))
        current = f.journal.read()
        self.assertGreater(f.journal.checkpoint['generation'], checkpoint['generation'])
        self.assertEqual(current['events'][-1]['event'], 'primary-ip-check')
        self.assertEqual(current['events'][-1]['server_ids'], [101])
        self.assertEqual(current['owned'], state['owned'])
        published_state = json.loads(Path(args.state).read_bytes())
        owned = json.loads(Path(args.owned_output).read_bytes())
        self.assertEqual(published_state['campaign_server_ids'], [101])
        self.assertEqual(published_state['campaign_primary_ip_ids'], [10101])
        self.assertEqual(published_state['hosts']['client']['name'], 'test-client')
        self.assertEqual(owned['campaign'], 'synthetic')
        self.assertEqual(owned['server_ids'], [101])
        self.assertEqual([(ip['server_id'], ip['id']) for ip in owned['primary_ips']], [(101, 10101)])
        self.assertIs(owned['direct_primary_ip_deletion_authorized'], False)
        self.assertEqual(Path(args.state).stat().st_mode & 0o777, 0o600)
        self.assertEqual(Path(args.owned_output).stat().st_mode & 0o777, 0o600)

    def test_primary_ip_cross_kind_protected_ids_do_not_protect_server_or_ip(self):
        f = self.f
        sid = f.create('server', 0)
        f.cloud.rows['server'][sid]['private_net'] = []
        # A different kind may have the same numeric ID without becoming this
        # server or its subordinate IP. This is real fake-provider metadata.
        f.cloud.rows['network'][sid] = dict(id=sid, name='unrelated-network', ip_range='10.66.0.0/24')
        protect = f.root / 'cross-kind-protect.tsv'
        protect.write_text(f'network\t{sid}\tunrelated-network\nnetwork\t{sid + 10000}\tother-network\n')
        before = copy.deepcopy(f.cloud.requests)
        args = self.ip_args('--protect-file', str(protect))
        with constructors(f):
            self.assertEqual(pic.check(args), ([sid], [sid + 10000]))
        self.assert_no_new_mutations(before)
        self.assertFalse(json.loads(Path(args.owned_output).read_text())['direct_primary_ip_deletion_authorized'])
        self.assertEqual(f.cloud.rows['network'][sid]['name'], 'unrelated-network')

    def test_primary_ip_same_kind_server_protection_refuses_exact_owned_id(self):
        f = self.f
        sid = f.create('server', 0)
        protect = f.root / 'same-kind-protect.tsv'
        protect.write_text(f'server\t{sid}\t\n')
        before = copy.deepcopy(f.cloud.requests)
        args = self.ip_args('--protect-file', str(protect))
        with constructors(f), self.assertRaisesRegex(mc.GuardError, 'additionally protected'):
            pic.check(args)
        self.assert_no_new_mutations(before)
        self.assertIn(sid, f.cloud.rows['server'])
        self.assertFalse(Path(args.owned_output).exists())

    def test_primary_ip_snapshot_files_cannot_substitute_for_creation_ledger(self):
        f = self.f
        args = self.ip_args('--server-json', str(f.root / 'untrusted-servers.json'),
                            '--primary-ip-json', str(f.root / 'untrusted-ips.json'))
        args.creation_ledger = None
        with constructors(f), self.assertRaisesRegex(mc.GuardError, 'requires --creation-ledger'):
            pic.check(args)
        self.assertEqual(f.cloud.requests, [])
        self.assertEqual(f.cloud.describes, [])
        self.assertFalse(Path(args.state).exists())
        self.assertFalse(Path(args.owned_output).exists())

    def test_primary_ip_unowned_matching_server_cannot_be_adopted(self):
        f = self.f
        f.cloud.rows['server'][777] = dict(id=777, name='test-client', labels={'campaign': 'synthetic'})
        args = self.ip_args()
        with constructors(f), self.assertRaisesRegex(mc.GuardError, 'unexpected ledger-owned server set'):
            pic.check(args)
        self.assertEqual(f.cloud.requests, [])
        self.assertEqual(f.journal.read()['owned'], [])
        self.assertFalse(Path(args.state).exists())

    def test_primary_ip_false_auto_delete_refuses_with_provenance_retained(self):
        f = self.f
        sid = f.create('server', 0)
        f.cloud.rows['primary-ip'][sid + 10000]['auto_delete'] = False
        before = copy.deepcopy(f.cloud.requests)
        args = self.ip_args()
        with constructors(f), self.assertRaisesRegex(mc.GuardError, 'unattributable'):
            pic.check(args)
        self.assert_no_new_mutations(before)
        self.assertEqual([r['id'] for r in f.journal.read()['owned'] if r['kind'] == 'server'], [sid])
        self.assertFalse(Path(args.state).exists())
        self.assertFalse(Path(args.owned_output).exists())

    def test_primary_ip_additional_protection_refuses_without_mutating_ip_flags(self):
        f = self.f
        sid = f.create('server', 0)
        protect = f.root / 'protect.tsv'
        protect.write_text(f'primary-ip\t{sid + 10000}\t\n')
        before = copy.deepcopy(f.cloud.requests)
        args = self.ip_args('--protect-file', str(protect))
        with constructors(f), self.assertRaisesRegex(mc.GuardError, 'primary-IP protection conflict'):
            pic.check(args)
        self.assert_no_new_mutations(before)
        self.assertTrue(f.cloud.rows['primary-ip'][sid + 10000]['auto_delete'])
        self.assertFalse(Path(args.owned_output).exists())

    def test_primary_ip_previous_exact_ids_cannot_disappear_or_transfer(self):
        f = self.f
        f.create('server', 0)
        args = self.ip_args()
        previous = dict(campaign_server_ids=[999], campaign_primary_ip_ids=[19999], status='previous')
        state = Path(args.state)
        state.write_text(json.dumps(previous))
        state.chmod(0o600)
        before = state.read_bytes()
        with constructors(f), self.assertRaisesRegex(mc.GuardError, 'previously recorded IDs disappeared'):
            pic.check(args)
        self.assertEqual(state.read_bytes(), before)
        self.assertFalse(Path(args.owned_output).exists())
        self.assertEqual(f.deletes(), [])


if __name__ == '__main__':
    unittest.main()
