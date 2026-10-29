#!/usr/bin/env python3
"""Public readback regressions with real journals, callers and authorization.

Only provider metadata/read failures and clock/process boundaries are synthetic.
No Campaign, Journal or CLI authorization method is replaced.
"""
import copy
import json
import os
from pathlib import Path
import sys
import unittest

HERE = Path(os.path.abspath(__file__)).parent
sys.path.insert(0, str(HERE))

import creation_ledger as cl  # noqa: E402
import mn_common as mc  # noqa: E402
import primary_ip_check as pic  # noqa: E402
from test_guarded_initialize import Initialization, NS, T0  # noqa: E402
from test_guarded_orchestration import (  # noqa: E402
    OrchestrationProvider, armed, cli_boundaries, execute, plan_creates,
)

DEFAULT = object()


class ReadbackProvider(OrchestrationProvider):
    """Capture allowances at actual CLI read boundaries, before fake transport."""

    def __init__(self, fixture, profile):
        super().__init__(fixture, profile)
        self.reads, self.on_read = [], None

    def read(self, operation, kind, rid, seconds, caller):
        while (Path(caller.f_code.co_filename).name == 'campaign_safety.py'
               and caller.f_code.co_name in {'describe_owned', 'read_owned'}):
            caller = caller.f_back
        filename = Path(caller.f_code.co_filename).name
        function = caller.f_code.co_name
        site = filename + ':' + function
        if filename == 'provision.py' and function == 'execute_guarded':
            site += ':hosts' if 'hosts' in caller.f_locals else ':cascade'
        # Cascade's exact IP read is shared. Retain the real outer CLI caller.
        outer = caller
        while outer is not None:
            if outer.f_code.co_name in {'execute_guarded', 'check', 'initialize_guarded'}:
                site += ':via-' + Path(outer.f_code.co_filename).name + ':' + outer.f_code.co_name
                break
            outer = outer.f_back
        entry = dict(operation=operation, kind=kind, id=rid, site=site,
                     callback=callable(seconds), allowance=None, transmitted=False)
        self.reads.append(entry)
        result = DEFAULT if self.on_read is None else self.on_read(entry)
        entry['allowance'] = self.allowance(seconds)
        entry['transmitted'] = True
        return entry['allowance'], result

    def list(self, kind, seconds=20, **kwargs):
        allowance, result = self.read('list', kind, None, seconds, sys._getframe(1))
        return super().list(kind, allowance) if result is DEFAULT else result

    def describe(self, kind, rid, seconds=20, **kwargs):
        allowance, result = self.read('describe', kind, rid, seconds, sys._getframe(1))
        return super().describe(kind, rid, allowance) if result is DEFAULT else result


def backend(f):
    f.cloud = ReadbackProvider(f, f.profile)
    f.cloud.rows = copy.deepcopy(f.baseline)
    return f.cloud


def checker_args(f, role='client', *extra):
    return pic.parse_args(['--label', 'campaign=synthetic-init', '--prefix', f.args.prefix,
        '--role', role, '--creation-ledger', str(f.ledger), '--state', str(f.root / 'checked.json'),
        '--owned-output', str(f.root / 'checked-owned.json'), *extra])


def checker(f, args):
    with cli_boundaries(f):
        return pic.check(args)


def create_servers(f, count=1):
    journal, campaign = armed(f)
    plan_creates(f, campaign, count=3 + count)
    for row in f.cloud.rows['server'].values():
        row.setdefault('private_net', [])
    return journal, campaign


def retirements(state):
    return {(e['kind'], e['id'], e['source']) for e in state['events']
            if e.get('event') == 'observed-retirement'}


def reopened(f):
    journal = cl.Journal(f.ledger)
    f.stack.callback(journal.close)
    return journal


class CheckerReadbackTest(unittest.TestCase):
    def sentinel_outputs(self, f, args, previous=None):
        state = json.dumps(previous or {'status': 'previous-unpublished'}) + '\n'
        owned = '{"sentinel":"previous-owned"}\n'
        f.private(Path(args.state).name, state)
        f.private(Path(args.owned_output).name, owned)
        return {args.state: state.encode(), args.owned_output: owned.encode()}

    def assert_outputs_unchanged(self, expected):
        for path, content in expected.items():
            self.assertEqual(Path(path).read_bytes(), content)

    def test_checker_exact_none_is_durable_and_identical_reappearance_cannot_be_deleted(self):
        with Initialization() as f:
            backend(f)
            journal, _ = create_servers(f)
            args = checker_args(f)
            before = self.sentinel_outputs(f, args)
            original = copy.deepcopy(f.cloud.rows['server'][101])
            seen = []
            def absent(read):
                if read['site'].startswith('primary_ip_check.py:check') and read['kind'] == 'server':
                    seen.append((read['operation'], read['id']))
                    return None
                return DEFAULT
            f.cloud.on_read = absent
            requests = copy.deepcopy(f.cloud.requests)
            with self.assertRaisesRegex(mc.GuardError, 'absent'):
                checker(f, args)
            self.assertEqual(seen, [('describe', 101)])
            self.assertEqual(f.cloud.requests, requests)
            self.assert_outputs_unchanged(before)
            saved = reopened(f)
            self.assertIn(('server', 101, 'primary-check-exact-readback'), retirements(saved.read()))
            self.assertEqual(f.cloud.rows['server'][101], original)
            f.cloud.on_read = None
            self.assertEqual(f.cloud.describe('server', 101), original)
            f.actor = 112
            f.processes.pop(111)
            cleanup = f.campaign(saved)
            cleanup.close_admission()
            result = cleanup.cleanup_pass()
            self.assertFalse(result['complete'])
            self.assertIn(101, f.cloud.rows['server'])
            self.assertFalse(any(method == 'DELETE' and path == '/servers/101'
                                 for method, path, _, _ in f.cloud.requests))
            self.assertIn(('server', 101, 'primary-check-exact-readback'), retirements(reopened(f).read()))

    def test_checker_commits_absence_before_any_reachable_later_provider_error(self):
        for fault in ('next-server', 'ip-list'):
            with self.subTest(fault=fault), Initialization() as f:
                backend(f)
                create_servers(f, count=2)
                args = checker_args(f, 'node1')
                before = self.sentinel_outputs(f, args)
                observed, failures = [], []
                def interrupted(read):
                    if not read['site'].startswith('primary_ip_check.py:check'):
                        return DEFAULT
                    if read['operation'] == 'describe' and read['kind'] == 'server' and read['id'] == 101:
                        observed.append(101)
                        return None
                    target = (fault == 'next-server' and read['id'] == 102
                              or fault == 'ip-list' and read['operation'] == 'list' and read['kind'] == 'primary-ip')
                    if target:
                        # This is a nonblocking observation of durable publication,
                        # not a nested Journal lock or authorization substitute.
                        state = json.loads(f.ledger.read_text())['state']
                        self.assertIn(('server', 101, 'primary-check-exact-readback'), retirements(state))
                        failures.append(read['site'])
                        raise mc.GuardError('synthetic later checker read failure')
                    return DEFAULT
                f.cloud.on_read = interrupted
                requests = copy.deepcopy(f.cloud.requests)
                with self.assertRaises(mc.GuardError):
                    checker(f, args)
                self.assertEqual(observed, [101])
                self.assertIn(('server', 101, 'primary-check-exact-readback'), retirements(reopened(f).read()))
                self.assertEqual(f.cloud.requests, requests)
                self.assert_outputs_unchanged(before)
                self.assertEqual(failures, [])
                direct = [r for r in f.cloud.reads if r['site'].startswith('primary_ip_check.py:check')]
                self.assertEqual([(r['operation'], r['kind'], r['id']) for r in direct],
                                 [('describe', 'server', 101)])

    def test_checker_retains_every_exact_absence_it_actually_observes(self):
        with Initialization() as f:
            backend(f)
            create_servers(f, count=2)
            args = checker_args(f, 'node1')
            before = self.sentinel_outputs(f, args)
            observed = []
            def absent(read):
                if read['site'].startswith('primary_ip_check.py:check') and read['kind'] == 'server':
                    if observed:
                        self.assertIn(('server', observed[-1], 'primary-check-exact-readback'),
                                      retirements(json.loads(f.ledger.read_text())['state']))
                    observed.append(read['id'])
                    return None
                return DEFAULT
            f.cloud.on_read = absent
            with self.assertRaises(mc.GuardError):
                checker(f, args)
            self.assertEqual(observed, [101])
            saved = retirements(reopened(f).read())
            self.assertIn(('server', 101, 'primary-check-exact-readback'), saved)
            self.assertNotIn(('server', 102, 'primary-check-exact-readback'), saved)
            self.assertFalse(any(r['site'].startswith('primary_ip_check.py:check') and
                                 (r['id'] == 102 or r['operation'] == 'list') for r in f.cloud.reads))
            self.assert_outputs_unchanged(before)

    def test_checker_supplied_name_and_typed_protections_survive_reopen_and_cleanup_without_file(self):
        for kind in ('name-only', 'server', 'primary-ip', 'unprotected-positive'):
            with self.subTest(protection=kind), Initialization() as f:
                backend(f)
                create_servers(f)
                options = []
                if kind != 'unprotected-positive':
                    text = {'name-only': f'network\t99999\t{f.args.prefix}-client\n',
                            'server': 'server\t101\t\n', 'primary-ip': 'primary-ip\t10101\t\n'}[kind]
                    path = f.private('checker-only-protect.tsv', text)
                    options = ['--protect-file', str(path)]
                args = checker_args(f, 'client', *options)
                before = self.sentinel_outputs(f, args)
                requests = copy.deepcopy(f.cloud.requests)
                if kind == 'unprotected-positive':
                    self.assertEqual(checker(f, args), ([101], [10101]))
                else:
                    with self.assertRaisesRegex(mc.GuardError, 'protect'):
                        checker(f, args)
                    self.assert_outputs_unchanged(before)
                self.assertEqual(f.cloud.requests, requests)
                saved = reopened(f)
                policy = saved.read().get('protections', {})
                if kind == 'name-only':
                    self.assertIn(f.args.prefix + '-client', policy['names'])
                    self.assertNotIn(101, policy['by_kind']['server'])
                    self.assertNotIn(10101, policy['by_kind']['primary-ip'])
                elif kind in ('server', 'primary-ip'):
                    self.assertIn(101 if kind == 'server' else 10101, policy['by_kind'][kind])
                if options:
                    Path(options[-1]).unlink()  # Never supplied to the later cleanup caller.
                f.actor = 112
                f.processes.pop(111)
                cleanup = f.campaign(saved)
                cleanup.close_admission()
                result = cleanup.cleanup_pass()
                deleted = [path for method, path, _, _ in f.cloud.requests if method == 'DELETE']
                self.assertEqual('/servers/101' in deleted, kind == 'unprotected-positive')
                self.assertEqual(result['complete'], kind == 'unprotected-positive')
                self.assertEqual(101 in f.cloud.rows['server'], kind != 'unprotected-positive')
                self.assertEqual(10101 in f.cloud.rows['primary-ip'], kind != 'unprotected-positive')

    def test_checker_historical_server_and_ip_populations_fail_independently_and_unchanged_passes(self):
        for population in ('server-only', 'ip-only', 'unchanged'):
            with self.subTest(population=population), Initialization() as f:
                backend(f)
                create_servers(f)
                args = checker_args(f)
                prior = dict(campaign_server_ids=[999] if population == 'server-only' else [101],
                             campaign_primary_ip_ids=[19999] if population == 'ip-only' else [10101],
                             status='previous')
                before = self.sentinel_outputs(f, args, prior)
                requests = copy.deepcopy(f.cloud.requests)
                if population == 'unchanged':
                    self.assertEqual(checker(f, args), ([101], [10101]))
                    current = json.loads(Path(args.state).read_text())
                    self.assertEqual(current['campaign_server_ids'], [101])
                    self.assertEqual(current['campaign_primary_ip_ids'], [10101])
                    self.assertEqual(current['status'], 'provisioned-through-client')
                    self.assertEqual(json.loads(Path(args.owned_output).read_text())['server_ids'], [101])
                else:
                    with self.assertRaisesRegex(mc.GuardError, 'previously recorded IDs disappeared'):
                        checker(f, args)
                    self.assert_outputs_unchanged(before)
                self.assertEqual(f.cloud.requests, requests)


class CallerReadBudgetTest(unittest.TestCase):
    def no_wait(self, seconds):
        self.fail('healthy provider unexpectedly required a polling sleep')

    def test_initializer_recomputes_census_allowance_at_c_and_stops_before_next_transmission(self):
        with Initialization() as f:
            backend(f)
            selected = []
            def cutoff(read):
                if read['site'].startswith('provision.py:initialize_guarded'):
                    selected.append(read)
                    f.wall = T0 + (1795 if len(selected) == 1 else 1798) * NS
                return DEFAULT
            f.cloud.on_read = cutoff
            with self.assertRaisesRegex(mc.GuardError, 'initial inventory phase budget exhausted'):
                f.initialize()
            self.assertEqual(len(selected), 2)
            self.assertTrue(selected[0]['callback'])
            self.assertEqual(selected[0]['allowance'], 3)
            self.assertTrue(selected[0]['transmitted'])
            self.assertTrue(selected[1]['callback'])
            self.assertFalse(selected[1]['transmitted'])
            self.assertEqual(f.cloud.lists, [(selected[0]['kind'], 3.0)])
            self.assertEqual(f.cloud.requests, [])
            self.assertFalse(f.ledger.exists())
            self.assertFalse((f.work / 'hosts-all.json').exists())

    def test_provision_cascade_exact_list_and_subordinate_reads_share_fresh_setup_budgets(self):
        for stop_at in ('list', 'subordinate'):
            with self.subTest(stop_at=stop_at), Initialization() as f:
                backend(f)
                journal, _ = armed(f)
                selected = []
                def cutoff(read):
                    direct = read['site'].startswith('provision.py:execute_guarded:cascade')
                    subordinate = (read['site'].startswith('campaign_safety.py:cascade')
                                   and ':via-provision.py:execute_guarded' in read['site'])
                    if direct or subordinate:
                        selected.append(read)
                        step = len(selected)
                        f.wall = journal.header['time']['setup_ns'] - (5 if step == 1 else
                            2 if stop_at == 'list' or step >= 3 else 3) * NS
                    return DEFAULT
                f.cloud.on_read = cutoff
                with self.assertRaisesRegex(mc.GuardError, 'budget exhausted'):
                    execute(f, journal, self.no_wait)
                self.assertEqual(len(selected), 2 if stop_at == 'list' else 3)
                self.assertEqual((selected[0]['operation'], selected[0]['kind']), ('describe', 'server'))
                self.assertEqual(selected[0]['allowance'], 3)
                self.assertEqual((selected[1]['operation'], selected[1]['kind']), ('list', 'primary-ip'))
                if stop_at == 'subordinate':
                    self.assertEqual(selected[1]['allowance'], 1)
                    self.assertEqual((selected[2]['operation'], selected[2]['kind']), ('describe', 'primary-ip'))
                self.assertTrue(all(read['callback'] for read in selected))
                self.assertFalse(selected[-1]['transmitted'])
                self.assertEqual(len(f.cloud.requests), 5)
                self.assertFalse((f.work / 'hosts-all.json').exists())
                self.assertTrue((f.work / 'guard' / 'stop.json').exists())

    def test_provision_host_publication_recomputes_each_budget_and_does_not_publish_partial_hosts(self):
        with Initialization() as f:
            backend(f)
            journal, _ = armed(f)
            selected = []
            def cutoff(read):
                if read['site'].startswith('provision.py:execute_guarded:hosts'):
                    selected.append(read)
                    f.wall = journal.header['time']['setup_ns'] - (5 if len(selected) == 1 else 2) * NS
                return DEFAULT
            f.cloud.on_read = cutoff
            with self.assertRaisesRegex(mc.GuardError, 'budget exhausted'):
                execute(f, journal, self.no_wait)
            self.assertEqual([read['id'] for read in selected], [101, 102])
            self.assertEqual(selected[0]['allowance'], 3)
            self.assertTrue(all(read['callback'] for read in selected))
            self.assertTrue(selected[0]['transmitted'])
            self.assertFalse(selected[1]['transmitted'])
            self.assertEqual(len(f.cloud.requests), 16)
            self.assertFalse((f.work / 'hosts-all.json').exists())
            self.assertTrue((f.work / 'guard' / 'stop.json').exists())

    def test_checker_exact_census_and_cascade_reads_recompute_setup_allowances_without_output(self):
        for stop_at in ('list', 'subordinate'):
            with self.subTest(stop_at=stop_at), Initialization() as f:
                backend(f)
                journal, _ = create_servers(f)
                args = checker_args(f)
                previous = CheckerReadbackTest().sentinel_outputs(f, args)
                selected = []
                def cutoff(read):
                    if (read['site'].startswith('primary_ip_check.py:check') or
                            read['site'].startswith('campaign_safety.py:cascade') and
                            ':via-primary_ip_check.py:check' in read['site']):
                        selected.append(read)
                        step = len(selected)
                        f.wall = journal.header['time']['setup_ns'] - (5 if step == 1 else
                            2 if stop_at == 'list' or step >= 3 else 3) * NS
                    return DEFAULT
                f.cloud.on_read = cutoff
                requests = copy.deepcopy(f.cloud.requests)
                with self.assertRaisesRegex(mc.GuardError, 'budget exhausted'):
                    checker(f, args)
                self.assertEqual(len(selected), 2 if stop_at == 'list' else 3)
                self.assertEqual((selected[0]['operation'], selected[0]['kind']), ('describe', 'server'))
                self.assertEqual(selected[0]['allowance'], 3)
                self.assertEqual((selected[1]['operation'], selected[1]['kind']), ('list', 'primary-ip'))
                if stop_at == 'subordinate':
                    self.assertEqual(selected[1]['allowance'], 1)
                    self.assertEqual((selected[2]['operation'], selected[2]['kind']), ('describe', 'primary-ip'))
                self.assertTrue(all(read['callback'] for read in selected))
                self.assertFalse(selected[-1]['transmitted'])
                self.assertEqual(f.cloud.requests, requests)
                CheckerReadbackTest().assert_outputs_unchanged(previous)

    def test_closed_checker_crosses_h_into_incident_without_reopening_or_resetting_earlier_budget(self):
        with Initialization() as f:
            backend(f)
            journal, campaign = create_servers(f)
            f.actor = 112
            f.processes.pop(111)
            campaign.close_admission()
            args = checker_args(f)
            selected = []
            def cross_h(read):
                if (read['site'].startswith('primary_ip_check.py:check') or
                        read['site'].startswith('campaign_safety.py:cascade') and
                        ':via-primary_ip_check.py:check' in read['site']):
                    selected.append(read)
                    f.wall = journal.header['time']['h_ns'] - 5 * NS if len(selected) == 1 else journal.header['time']['h_ns'] + NS
                return DEFAULT
            f.cloud.on_read = cross_h
            before = copy.deepcopy(f.cloud.requests)
            self.assertEqual(checker(f, args), ([101], [10101]))
            self.assertEqual([(r['operation'], r['kind']) for r in selected],
                             [('describe', 'server'), ('list', 'primary-ip'), ('describe', 'primary-ip')])
            self.assertEqual([r['allowance'] for r in selected], [3, 20, 20])
            self.assertTrue(all(r['callback'] and r['transmitted'] for r in selected))
            state = reopened(f).read()
            self.assertEqual(state['mode'], 'INCIDENT')
            self.assertEqual(sum(e.get('event') == 'cleanup-reserve-exhausted' for e in state['events']), 1)
            self.assertFalse(any(e.get('event') == 'provisioning-complete' for e in state['events']))
            self.assertEqual(f.cloud.requests, before)
            self.assertEqual(json.loads(Path(args.state).read_text())['campaign_server_ids'], [101])

    def test_healthy_real_provision_and_checker_publish_complete_exact_populations(self):
        with Initialization() as f:
            backend(f)
            journal, _ = armed(f)
            result = execute(f, journal, self.no_wait)
            self.assertEqual(set(result['hosts']), {'client', 'node1', 'node2', 'node3'})
            self.assertEqual(len(f.cloud.requests), 16)
            self.assertEqual(len(journal.read()['owned']), 7)
            self.assertEqual(json.loads((f.work / 'hosts-all.json').read_text()), result['hosts'])
            before = copy.deepcopy(f.cloud.requests)
            args = checker_args(f, 'node3')
            self.assertEqual(checker(f, args), ([101, 102, 103, 104], [10101, 10102, 10103, 10104]))
            self.assertEqual(f.cloud.requests, before)
            self.assertEqual(len(json.loads(Path(args.owned_output).read_text())['primary_ips']), 4)
            direct = [r for r in f.cloud.reads if r['site'].startswith(('provision.py:execute_guarded', 'primary_ip_check.py:check'))]
            self.assertTrue(direct)
            self.assertTrue(all(r['callback'] and r['transmitted'] and r['allowance'] == 20 for r in direct))
            self.assertEqual(journal.read()['mode'], 'OPEN')


if __name__ == '__main__':
    unittest.main()
