#!/usr/bin/env python3
"""Native F22/F30/F35/F36/F48/F51 regressions through actual orchestration.

No Journal/Campaign authorization, arming, completion, retirement or cleanup
method is stubbed. Only provider/process/clock boundaries and an explicit crash
fsync are synthetic. Thread events bound harness waits without real API traffic.
"""
import contextlib
import copy
import io
import json
import os
from pathlib import Path
import stat
import sys
import threading
import unittest
from unittest import mock

HERE = Path(os.path.abspath(__file__)).parent
sys.path.insert(0, str(HERE))

import campaign_cloud as cc  # noqa: E402
import campaign_safety as cs  # noqa: E402
import creation_ledger as cl  # noqa: E402
import mn_common as mc  # noqa: E402
import provision  # noqa: E402
import teardown_guard as tg  # noqa: E402
from test_guarded_initialize import Initialization, InitProvider, NS, REAL_CAMPAIGN  # noqa: E402

# Physical scheduler headroom for real fsync/chain validation, not a policy lease.
HARNESS_WAIT_SECONDS = 60


class OrchestrationProvider(InitProvider):
    """In-memory provider resolves phase callbacks at each actual read/send."""

    @staticmethod
    def allowance(seconds):
        return float(seconds() if callable(seconds) else seconds)

    def list(self, kind, seconds=20, absolute_deadline_ns=None, **kwargs):
        return super().list(kind, seconds=self.allowance(seconds))

    def describe(self, kind, rid, seconds=20, absolute_deadline_ns=None, **kwargs):
        return super().describe(kind, rid, seconds=self.allowance(seconds))

    def action(self, rid, seconds=20, absolute_deadline_ns=None, **kwargs):
        return super().action(rid, seconds=self.allowance(seconds))

    def resource_actions(self, kind, rid, seconds=20, absolute_deadline_ns=None, **kwargs):
        return super().resource_actions(kind, rid, seconds=self.allowance(seconds), **kwargs)

    def request(self, method, path, body=None, seconds=20, absolute_deadline_ns=None, **kwargs):
        return super().request(method, path, body=body, seconds=self.allowance(seconds))


def backend(fixture):
    provider = OrchestrationProvider(fixture, fixture.profile)
    provider.rows = copy.deepcopy(fixture.baseline)
    fixture.cloud = provider
    return provider


def armed(fixture):
    journal = fixture.initialize()
    campaign = fixture.campaign(journal)
    fixture.actor = 112
    campaign.arm_guard()
    fixture.actor = 111
    return journal, campaign


def plan_creates(fixture, campaign, count=7):
    plan, ids = fixture.plan(), {}
    for entry in plan[:count]:
        body = copy.deepcopy(entry['body'])
        if entry['kind'] == 'server':
            body['ssh_keys'] = [ids[body['ssh_keys'][0]]]
            body['firewalls'] = [dict(firewall=ids[body['firewalls'][0]['firewall']])]
        ids[entry['name']] = campaign.create(entry['kind'], body)
    return ids


def teardown_args(fixture, *extra):
    return tg.parse_args(['--label', 'campaign=synthetic-init', '--prefix', fixture.args.prefix,
                          '--creation-ledger', str(fixture.ledger), '--execute', *extra])


@contextlib.contextmanager
def cli_boundaries(fixture):
    # These factories return real Profile/Campaign objects; only Cloud is the
    # in-memory provider. Campaign construction injects clocks, not behavior.
    def clocked(journal, cloud, **kwargs):
        campaign = REAL_CAMPAIGN(journal, cloud, clock=lambda: fixture.wall,
                                 monotonic=lambda: fixture.mono, **kwargs)
        fixture.constructed.append(campaign)
        return campaign
    with mock.patch.object(cc, 'Cloud', side_effect=fixture.cloud_factory), \
            mock.patch.object(cs, 'Campaign', side_effect=clocked):
        yield


def execute(fixture, journal, sleep):
    with cli_boundaries(fixture), mock.patch.object(provision.time, 'sleep', side_effect=sleep):
        return provision.execute_guarded(fixture.args, journal, fixture.profile, fixture.cloud,
                                         fixture.plan(), io.StringIO())


def advance(fixture, seconds):
    nanoseconds = round(seconds * NS)
    fixture.wall += nanoseconds
    fixture.mono += nanoseconds


class PendingProvisioningTest(unittest.TestCase):
    def pending_subnet(self, fixture):
        original = fixture.cloud.new_action
        selected = []
        def action(kind, rid, command):
            value = original(kind, rid, command)
            if command == 'add_subnet':
                value['status'] = 'running'
                fixture.cloud.actions[value['id']]['status'] = 'running'
                selected.append(value['id'])
            return value
        return mock.patch.object(fixture.cloud, 'new_action', side_effect=action), selected

    def test_execute_waits_for_running_action_success_before_next_operation(self):
        with Initialization() as f:
            backend(f)
            journal, _ = armed(f)
            control, selected = self.pending_subnet(f)
            sleeps = []
            def sleep(seconds):
                self.assertEqual(seconds, 0.25)
                self.assertEqual([(method, path) for method, path, _, _ in f.cloud.requests],
                                 [('POST', '/networks'), ('POST', '/networks/201/actions/add_subnet')])
                self.assertEqual(len(selected), 1)
                state = journal.read()
                self.assertEqual(state['intents'][-1]['state'], 'ACKNOWLEDGED')
                self.assertIsNone(state['active_lease'])
                sleeps.append(seconds)
                self.assertLessEqual(len(sleeps), 2, 'pending action was never completed')
                advance(f, seconds)
                if len(sleeps) == 2:
                    f.cloud.actions[selected[0]]['status'] = 'success'
            observed = []
            def before_request(method, path, body, seconds):
                if path == '/firewalls':
                    self.assertEqual(sleeps, [0.25, 0.25])
                    self.assertEqual(f.cloud.actions[selected[0]]['status'], 'success')
                    observed.append(path)
            f.cloud.before_request = before_request
            with control:
                result = execute(f, journal, sleep)
            self.assertTrue(result['executed'])
            self.assertEqual(observed, ['/firewalls'])
            self.assertEqual(sleeps, [0.25, 0.25])
            self.assertEqual(len(f.cloud.requests), 16)
            self.assertTrue(all(intent['state'] == 'DONE' for intent in journal.read()['intents']))
            self.assertEqual(json.loads((f.work / 'hosts-all.json').read_text()), result['hosts'])

    def test_execute_pending_action_error_stops_before_any_later_post_or_hosts(self):
        with Initialization() as f:
            backend(f)
            journal, _ = armed(f)
            control, selected = self.pending_subnet(f)
            sleeps = []
            def sleep(seconds):
                self.assertEqual(seconds, 0.25)
                sleeps.append(seconds)
                self.assertEqual(len(sleeps), 1, 'failed action was ignored')
                f.cloud.actions[selected[0]]['status'] = 'error'
                advance(f, seconds)
            with control, self.assertRaisesRegex(mc.GuardError, 'failed|failure'):
                execute(f, journal, sleep)
            self.assertEqual(sleeps, [0.25])
            self.assertEqual([(method, path) for method, path, _, _ in f.cloud.requests],
                             [('POST', '/networks'), ('POST', '/networks/201/actions/add_subnet')])
            state = journal.read()
            self.assertEqual(state['intents'][-1]['state'], 'FAILED')
            self.assertEqual(state['intents'][-1]['actions'], selected)
            self.assertIn(selected[0], f.cloud.action_reads)
            self.assertFalse((f.work / 'hosts-all.json').exists())
            self.assertEqual(json.loads((f.work / 'guard' / 'stop.json').read_text())['binding'], journal.binding)

    def test_execute_pending_action_rechecks_guard_loss_before_continuing(self):
        with Initialization() as f:
            backend(f)
            journal, _ = armed(f)
            control, selected = self.pending_subnet(f)
            sleeps = []
            def sleep(seconds):
                self.assertEqual(seconds, 0.25)
                sleeps.append(seconds)
                self.assertEqual(len(sleeps), 1, 'dead guard was ignored while action remained pending')
                f.processes.pop(112)
                advance(f, seconds)
            with control, self.assertRaisesRegex(mc.GuardError, 'guard|supervis'):
                execute(f, journal, sleep)
            self.assertEqual(sleeps, [0.25])
            self.assertEqual(len(f.cloud.requests), 2)
            self.assertEqual(journal.read()['intents'][-1]['actions'], selected)
            self.assertEqual(f.cloud.actions[selected[0]]['status'], 'running')
            self.assertFalse((f.work / 'hosts-all.json').exists())
            self.assertTrue((f.work / 'guard' / 'stop.json').exists())

    def test_execute_pending_action_halts_at_setup_cutoff_with_live_fresh_guard(self):
        with Initialization() as f:
            backend(f)
            journal, _ = armed(f)
            control, selected = self.pending_subnet(f)
            sleeps = []
            time_policy = journal.header['time']
            def sleep(seconds):
                self.assertEqual(seconds, 0.25)
                sleeps.append(seconds)
                self.assertEqual(len(sleeps), 1, 'setup cutoff was ignored while action remained pending')
                # Advance wall time only: heartbeat's monotonic age remains
                # fresh, isolating the setup budget from guard-loss refusal.
                f.wall = time_policy['setup_ns']
            with control, self.assertRaisesRegex(mc.GuardError, 'budget|allowance|deadline|setup'):
                execute(f, journal, sleep)
            self.assertEqual(sleeps, [0.25])
            self.assertEqual(len(f.cloud.requests), 2)
            self.assertEqual(f.cloud.actions[selected[0]]['status'], 'running')
            self.assertTrue(cs.live(f.processes[112]))
            self.assertFalse((f.work / 'hosts-all.json').exists())
            self.assertTrue((f.work / 'guard' / 'stop.json').exists())
            self.assertEqual(journal.header['time'], time_policy)


class ProvisioningRetirementTest(unittest.TestCase):
    def test_each_provisioning_absence_is_durable_and_same_id_cannot_inherit_cleanup(self):
        for site, source in (('cascade', 'provision-cascade-readback'), ('hosts', 'provision-host-readback')):
            with self.subTest(site=site), Initialization() as f:
                backend(f)
                journal, campaign = armed(f)
                original = f.cloud.describe
                missing = []
                identical = []
                def describe(kind, rid, seconds=20, **kwargs):
                    value = original(kind, rid, seconds=seconds, **kwargs)
                    powers = sum(path.endswith('/actions/poweron') for _, path, _, _ in f.cloud.requests)
                    attached = any(path == '/servers/101/actions/attach_to_network'
                                   for _, path, _, _ in f.cloud.requests)
                    selected = site == 'cascade' and not attached or site == 'hosts' and powers == 4
                    if kind == 'server' and rid == 101 and selected and not missing:
                        self.assertIsNotNone(value)
                        identical.append(copy.deepcopy(value))
                        missing.append((kind, rid))
                        return None
                    return value
                def no_wait(seconds):
                    self.fail('healthy action fixture unexpectedly waited')
                with mock.patch.object(f.cloud, 'describe', side_effect=describe):
                    with self.assertRaisesRegex(mc.GuardError, 'absent'):
                        execute(f, journal, no_wait)
                self.assertEqual(missing, [('server', 101)])
                self.assertEqual(f.cloud.rows['server'][101], identical[0])
                self.assertFalse((f.work / 'hosts-all.json').exists())
                self.assertTrue((f.work / 'guard' / 'stop.json').exists())
                retired = [event for event in journal.read()['events']
                           if event.get('event') == 'observed-retirement' and event.get('kind') == 'server'
                           and event.get('id') == 101]
                self.assertEqual(len(retired), 1)
                self.assertEqual(retired[0]['source'], source)
                self.assertEqual(len(f.cloud.requests), 5 if site == 'cascade' else 16)
                f.actor = 112
                f.processes.pop(111)
                campaign.close_admission()
                before = copy.deepcopy(f.cloud.requests)
                with self.assertRaisesRegex(mc.GuardError, 'retired'):
                    campaign.mutate('server', 101, 'delete')
                self.assertEqual(f.cloud.requests, before)
                result = campaign.cleanup_pass()
                self.assertFalse(result['complete'])
                self.assertIn(101, f.cloud.rows['server'])
                self.assertFalse(any(method == 'DELETE' and path == '/servers/101'
                                     for method, path, _, _ in f.cloud.requests))
                self.assertTrue(any(record['kind'] == 'server' and record['id'] == 101
                                    for record in journal.read()['owned']))


class HarnessStop(Exception):
    pass


class RunningGuard:
    """An actual guard_loop, paused solely at its injected sleep boundary."""

    def __init__(self, fixture, sleep=None):
        self.f, self.sleep_callback = fixture, sleep
        self.local = threading.local()
        self.ready, self.resume, self.done, self.cancel = (threading.Event() for _ in range(4))
        self.release_on_exit = [self.resume]
        self.result, self.error, self.sleeps = None, None, []
        self.thread = None
        self.journal = cl.Journal(fixture.ledger)
        fixture.stack.callback(self.journal.close)
        self.campaign = fixture.campaign(self.journal)

    def identity(self, pid=None):
        pid = getattr(self.local, 'actor', self.f.actor) if pid is None else pid
        if pid not in self.f.processes:
            raise ProcessLookupError('synthetic process is gone')
        return copy.deepcopy(self.f.processes[pid])

    def sleep(self, seconds):
        if self.cancel.is_set():
            raise HarnessStop()
        self.sleeps.append(seconds)
        if len(self.sleeps) == 1:
            self.ready.set()
            if not self.resume.wait(HARNESS_WAIT_SECONDS):
                raise AssertionError('test did not release the armed guard')
        elif self.sleep_callback:
            self.sleep_callback(self, seconds)
        else:
            raise AssertionError('guard unexpectedly waited after its first resumed cleanup')
        if self.cancel.is_set():
            raise HarnessStop()

    def run(self):
        self.local.actor = 112
        try:
            self.result = self.campaign.guard_loop(sleep=self.sleep)
        except HarnessStop:
            pass
        except BaseException as error:
            self.error = error
        finally:
            self.done.set()

    def __enter__(self):
        self.identity_patch = mock.patch.object(cs, 'identity', side_effect=self.identity)
        self.identity_patch.start()
        self.thread = threading.Thread(target=self.run, name='synthetic-real-guard', daemon=True)
        self.thread.start()
        if not self.ready.wait(HARNESS_WAIT_SECONDS):
            self.close()
            raise AssertionError('actual guard did not arm and reach its open-state sleep') from self.error
        return self

    def wait_complete(self):
        if not self.done.wait(HARNESS_WAIT_SECONDS):
            raise AssertionError(f'actual guard loop did not finish: error={self.error!r}, '
                                 f'requests={len(self.f.cloud.requests)}, sleeps={self.sleeps!r}')
        if self.error is not None:
            raise self.error
        if self.result is None:
            raise AssertionError('guard exited without its actual completion receipt')
        return self.result

    def close(self):
        self.cancel.set()
        for event in self.release_on_exit:
            event.set()
        try:
            if self.thread is not None:
                self.thread.join(HARNESS_WAIT_SECONDS)
                if self.thread.is_alive():
                    raise AssertionError('guard harness thread did not terminate')
        finally:
            self.identity_patch.stop()

    def __exit__(self, *args):
        self.close()


class ImmediateRequesterTest(unittest.TestCase):
    def provision(self, f, journal):
        def no_wait(seconds):
            self.fail('healthy provisioning unexpectedly waited')
        result = execute(f, journal, no_wait)
        self.assertTrue(result['executed'])
        self.assertEqual(len(result['hosts']), 4)
        self.assertEqual(len(journal.read()['owned']), 7)
        self.assertEqual(len(f.cloud.requests), 16)
        return result

    def test_live_guard_immediate_confirms_real_cleanup_and_propagates_post_h_lifetime(self):
        for after_h in (False, True):
            with self.subTest(after_h=after_h), Initialization() as f:
                backend(f)
                journal = f.initialize()
                with RunningGuard(f) as guard:
                    self.provision(f, journal)
                    time_policy = journal.header['time']
                    f.processes[113] = dict(pid=113, parent=110, starttime='1130', argv_sha256=cc.digest(b'113'))
                    f.actor = 113
                    sends = []
                    f.cloud.before_request = lambda method, path, body, seconds: sends.append((method, path, cs.identity()['pid']))
                    waits = []
                    def requester_sleep(seconds):
                        waits.append(seconds)
                        self.assertEqual(len(waits), 1)
                        self.assertTrue((f.work / 'guard' / 'stop.json').exists())
                        self.assertEqual(cs.identity()['pid'], 113)
                        f.processes.pop(111)
                        if after_h:
                            f.wall = journal.header['time']['h_ns'] + NS
                        guard.resume.set()
                        guard.wait_complete()
                    args = teardown_args(f, '--immediate', '--request-wait-seconds', '1')
                    observer = cl.Journal(f.ledger)
                    f.stack.callback(observer.close)
                    with cli_boundaries(f):
                        ok, receipt = tg.execute_guarded(args, observer, sleep=requester_sleep, out=io.StringIO())
                    self.assertEqual(waits, [0.1])
                    self.assertEqual(ok, not after_h)
                    self.assertTrue(receipt['result']['complete'])
                    self.assertEqual(receipt['result']['hard_lifetime_met'], not after_h)
                    self.assertEqual(receipt['result'], guard.result)
                    self.assertEqual(json.loads((f.work / 'guard' / 'cleanup.json').read_text()), guard.result)
                    self.assertEqual(len(sends), 7)
                    self.assertTrue(all(method == 'DELETE' and actor == 112 for method, _, actor in sends))
                    self.assertFalse(any(actor == 113 for _, _, actor in sends))
                    self.assertEqual(observer.read()['mode'], 'COMPLETE')
                    self.assertEqual(observer.header['time'], time_policy)
                    for kind, ids in observer.header['baseline'].items():
                        self.assertEqual(sorted(f.cloud.rows[kind]), ids)

    def test_immediate_wait_is_bounded_while_real_guard_holds_census_flock(self):
        with Initialization() as f:
            backend(f)
            journal = f.initialize()
            with RunningGuard(f) as guard:
                self.provision(f, journal)
                observer = cl.Journal(f.ledger)
                f.stack.callback(observer.close)
                held, release, request_done = threading.Event(), threading.Event(), threading.Event()
                guard.release_on_exit.append(release)
                def hold_census(kind, seconds):
                    if kind == 'server' and not held.is_set():
                        held.set()
                        # The guard must outlive the requester's independent scheduler allowance.
                        if not release.wait(2 * HARNESS_WAIT_SECONDS):
                            raise AssertionError('test never released guard census')
                f.cloud.before_list = hold_census
                f.processes.pop(111)
                guard.campaign.request_stop()
                guard.resume.set()
                self.assertTrue(held.wait(HARNESS_WAIT_SECONDS), 'guard never entered its actual locked census')
                lock = os.open(f.ledger.parent / cl.LOCK, os.O_RDWR)
                try:
                    import fcntl
                    with self.assertRaises(BlockingIOError):
                        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                finally:
                    os.close(lock)
                f.processes[113] = dict(pid=113, parent=110, starttime='1130', argv_sha256=cc.digest(b'113'))
                result, errors, sleeps = [], [], []
                before = copy.deepcopy(f.cloud.requests)
                start = f.mono
                def requester_sleep(seconds):
                    sleeps.append(seconds)
                    self.assertLessEqual(len(sleeps), 4, 'bounded wait did not terminate')
                    advance(f, seconds)
                def requester():
                    guard.local.actor = 113
                    try:
                        with cli_boundaries(f):
                            result.append(tg.run(teardown_args(f, '--immediate', '--request-wait-seconds', '0.3'),
                                sleep=requester_sleep, out=io.StringIO(),
                                runner=lambda *args: self.fail('immediate execution called legacy CLI')))
                    except BaseException as error:
                        errors.append(error)
                    finally:
                        request_done.set()
                thread = threading.Thread(target=requester, name='synthetic-immediate-requester', daemon=True)
                thread.start()
                try:
                    bounded = request_done.wait(HARNESS_WAIT_SECONDS)
                    self.assertTrue(bounded, 'requester blocked on the real guard-held journal flock')
                    self.assertFalse(release.is_set())
                    self.assertFalse(guard.done.is_set(), 'guard exited before requester finished under its flock')
                    if errors:
                        raise errors[0]
                    self.assertEqual(len(result), 1)
                    ok, receipt = result[0]
                    self.assertFalse(ok)
                    self.assertEqual(receipt['result']['status'], 'cleanup-request-pending')
                    self.assertFalse(receipt['result']['complete'])
                    self.assertLessEqual(f.mono - start, 400_000_000)
                    self.assertEqual(f.cloud.requests, before)
                finally:
                    release.set()
                    thread.join(HARNESS_WAIT_SECONDS)
                self.assertFalse(thread.is_alive())
                guard.wait_complete()
                self.assertTrue(guard.result['complete'])


class GuardLoopContinuationTest(unittest.TestCase):
    def test_premature_producer_exit_triggers_real_cleanup_before_g(self):
        with Initialization() as f:
            backend(f)
            journal = f.initialize()
            with RunningGuard(f) as guard:
                campaign = f.campaign(journal)
                self.assertEqual(campaign.create('network', f.plan()[0]['body']), 201)
                self.assertTrue(campaign.poll_actions())
                self.assertFalse((f.work / 'guard' / 'stop.json').exists())
                self.assertFalse(any(event.get('event') == 'provisioning-complete'
                                     for event in journal.read()['events']))
                self.assertLess(f.wall, journal.header['time']['g_ns'])
                f.processes.pop(111)
                guard.resume.set()
                result = guard.wait_complete()
                self.assertTrue(result['complete'])
                self.assertTrue(result['hard_lifetime_met'])
                self.assertEqual([(method, path) for method, path, _, _ in f.cloud.requests],
                                 [('POST', '/networks'), ('DELETE', '/networks/201')])
                self.assertEqual(journal.read()['mode'], 'COMPLETE')
                self.assertEqual(guard.sleeps, [1])

    def test_completed_producer_exit_stays_open_until_real_g_cutover(self):
        with Initialization() as f:
            backend(f)
            journal = f.initialize()
            observed_open, deadline = threading.Event(), threading.Event()
            def guard_sleep(guard, seconds):
                self.assertEqual(seconds, 1)
                self.assertEqual(len(guard.sleeps), 2)
                state = guard.journal.read()
                self.assertEqual(state['mode'], 'OPEN')
                self.assertTrue(any(event.get('event') == 'provisioning-complete' for event in state['events']))
                self.assertFalse((f.work / 'guard' / 'stop.json').exists())
                self.assertFalse(any(method == 'DELETE' for method, _, _, _ in f.cloud.requests))
                observed_open.set()
                if not deadline.wait(HARNESS_WAIT_SECONDS):
                    raise AssertionError('test did not release the completed producer deadline wait')
            with RunningGuard(f, sleep=guard_sleep) as guard:
                guard.release_on_exit.append(deadline)
                execute(f, journal, lambda seconds: self.fail('healthy provisioning unexpectedly waited'))
                f.processes.pop(111)
                guard.resume.set()
                self.assertTrue(observed_open.wait(HARNESS_WAIT_SECONDS), 'normal producer exit caused premature teardown')
                self.assertFalse(any(method == 'DELETE' for method, _, _, _ in f.cloud.requests))
                sends = []
                f.cloud.before_request = lambda method, path, body, seconds: sends.append((method, path, f.wall))
                advance(f, (journal.header['time']['g_ns'] - f.wall) / NS)
                deadline.set()
                result = guard.wait_complete()
                self.assertTrue(result['complete'])
                self.assertTrue(result['hard_lifetime_met'])
                self.assertEqual(len(sends), 7)
                self.assertTrue(all(method == 'DELETE' and at >= journal.header['time']['g_ns']
                                    for method, _, at in sends))
                self.assertEqual(journal.read()['mode'], 'COMPLETE')
                self.assertEqual(guard.sleeps, [1, 1])

    def test_loop_survives_cleanup_exception_retries_crosses_h_and_recovers_late_create(self):
        with Initialization() as f:
            backend(f)
            journal = f.initialize()
            h = journal.header['time']['h_ns']
            intermediate, census_times, sleeps = [], [], []
            materialized = []
            failed = []
            def guard_sleep(guard, seconds):
                self.assertEqual(seconds, 1)
                sleeps.append(seconds)
                self.assertLess(len(sleeps), 150, 'incident cleanup did not continue to eventual completion')
                path = f.work / 'guard' / 'cleanup.json'
                if f.wall >= h + 30 * NS and path.exists() and not materialized:
                    value = json.loads(path.read_text())
                    self.assertFalse(value['complete'])
                    self.assertTrue(value['baseline_equal'])
                    self.assertEqual(value['pending_intents'], [0])
                    intermediate.append(value)
                    f.cloud.finish_pending()
                    materialized.append(201)
                advance(f, seconds)
            with RunningGuard(f, sleep=guard_sleep) as guard:
                campaign = f.campaign(journal)
                f.cloud.delay_create = True
                with self.assertRaisesRegex(mc.GuardError, 'failed or ambiguous'):
                    campaign.create('network', f.plan()[0]['body'])
                f.cloud.delay_create = False
                self.assertEqual(journal.read()['intents'][0]['state'], 'UNKNOWN')
                self.assertNotIn(201, f.cloud.rows['network'])
                self.assertEqual(len(f.cloud.pending), 1)
                f.processes.pop(111)
                advance(f, (h - 30 * NS - f.wall) / NS)
                def census(kind, seconds):
                    if kind == 'server':
                        census_times.append(f.wall)
                    if kind == 'network' and not failed:
                        failed.append(f.wall)
                        raise mc.GuardError('synthetic first cleanup census failed')
                f.cloud.before_list = census
                guard.resume.set()
                result = guard.wait_complete()
                self.assertTrue(result['complete'])
                self.assertTrue(result['baseline_equal'])
                self.assertFalse(result['hard_lifetime_met'])
                self.assertEqual(result['pending_intents'], [])
                self.assertEqual(failed, [h - 30 * NS])
                self.assertEqual(sorted(set(census_times)), [h - 30 * NS, h + 30 * NS, h + 90 * NS])
                self.assertEqual(materialized, [201])
                self.assertEqual(len(intermediate), 1)
                self.assertEqual([(method, path) for method, path, _, _ in f.cloud.requests],
                                 [('POST', '/networks'), ('DELETE', '/networks/201')])
                incidents = list((f.work / 'guard').glob('incident-*.json'))
                self.assertEqual(len(incidents), 1)
                incident = json.loads(incidents[0].read_text())
                self.assertFalse(incident['complete'])
                self.assertEqual(incident['error_type'], 'GuardError')
                self.assertFalse(incident['hard_lifetime_met'])
                state = journal.read()
                self.assertEqual(state['mode'], 'COMPLETE')
                self.assertTrue(any(event.get('event') == 'hard-deadline-unresolved'
                                    and event['unix_ns'] >= h for event in state['events']))
                self.assertEqual(state['intents'][0]['recovery']['origin'], 'conditional-nonce-recovery')
                self.assertIsNone(state['intents'][0]['ack'])
                self.assertEqual(journal.header['time']['h_ns'], h)
                for kind, ids in journal.header['baseline'].items():
                    self.assertEqual(sorted(f.cloud.rows[kind]), ids)


class PublicTeardownTest(unittest.TestCase):
    def no_wait(self, seconds):
        self.fail('public teardown unexpectedly waited instead of its first complete cleanup')

    def replacement_actor(self, f):
        f.processes.pop(111, None)
        f.processes.pop(112, None)
        f.processes[114] = dict(pid=114, parent=110, starttime='1140', argv_sha256=cc.digest(b'114'))
        f.actor = 114

    def crash_last_ack(self, f):
        journal, campaign = armed(f)
        ids = plan_creates(f, campaign, 6)
        self.assertTrue(campaign.poll_actions())
        body = copy.deepcopy(f.plan()[6]['body'])
        body['ssh_keys'] = [ids[body['ssh_keys'][0]]]
        body['firewalls'] = [dict(firewall=ids[body['firewalls'][0]['firewall']])]
        real_fsync = os.fsync
        dispatched, syncs = [], []
        def before_request(method, path, body, seconds):
            self.assertEqual((method, path), ('POST', '/servers'))
            dispatched.append(path)
        f.cloud.before_request = before_request
        def sync(fd):
            if dispatched and len(syncs) < 2:
                syncs.append(stat.S_IFMT(os.fstat(fd).st_mode))
                if len(syncs) == 2:
                    raise OSError('synthetic public-repair final ACK directory sync failed')
            return real_fsync(fd)
        with mock.patch.object(cl.os, 'fsync', side_effect=sync):
            with self.assertRaisesRegex(OSError, 'public-repair final ACK directory sync'):
                campaign.create('server', body)
        f.cloud.before_request = None
        self.assertEqual(syncs, [stat.S_IFREG, stat.S_IFDIR])
        self.assertEqual(len(f.cloud.requests), 7)
        self.assertIn(104, f.cloud.rows['server'])
        headed = json.loads(f.ledger.read_bytes())
        self.assertEqual(headed['state']['intents'][6]['state'], 'SENDING')
        orphan_path = f.ledger.parent / f'generation-{headed["generation"] + 1:020d}.json'
        orphan = json.loads(orphan_path.read_bytes())
        self.assertEqual(orphan['state']['intents'][6]['state'], 'ACKNOWLEDGED')
        self.assertEqual(len(orphan['state']['owned']), 7)
        adverse = {f.ledger.name: f.ledger.read_bytes(), cl.ANCHOR: (f.ledger.parent / cl.ANCHOR).read_bytes(),
                   orphan_path.name: orphan_path.read_bytes()}
        journal.close()
        self.replacement_actor(f)
        return adverse, orphan

    def test_parsed_run_opens_normal_ledger_and_performs_actual_cleanup(self):
        with Initialization() as f:
            backend(f)
            journal, campaign = armed(f)
            plan_creates(f, campaign)
            self.replacement_actor(f)
            args = teardown_args(f, '--receipt-dir', str(f.root / 'public-receipt'))
            with cli_boundaries(f):
                ok, receipt = tg.run(args, sleep=self.no_wait, out=io.StringIO(),
                                     runner=lambda *args: self.fail('public execution called legacy CLI'))
            self.assertTrue(ok)
            self.assertTrue(receipt['result']['complete'])
            self.assertEqual(len([method for method, _, _, _ in f.cloud.requests if method == 'DELETE']), 7)
            self.assertEqual(journal.read()['mode'], 'COMPLETE')
            paths = list((f.root / 'public-receipt').glob('teardown-*.json'))
            self.assertEqual(len(paths), 1)
            self.assertEqual(json.loads(paths[0].read_text()), receipt)
            for kind, ids in journal.header['baseline'].items():
                self.assertEqual(sorted(f.cloud.rows[kind]), ids)

    def test_parsed_recovery_requires_evidence_directory_before_any_mutation(self):
        with Initialization() as f:
            backend(f)
            adverse, _ = self.crash_last_ack(f)
            before = copy.deepcopy(f.cloud.requests)
            with cli_boundaries(f), self.assertRaisesRegex(mc.GuardError, 'requires --recovery-evidence-dir'):
                tg.run(teardown_args(f, '--recover-journal'), sleep=self.no_wait, out=io.StringIO())
            self.assertEqual(f.cloud.requests, before)
            self.assertEqual(f.cloud_profiles, [])
            for name, raw in adverse.items():
                self.assertEqual((f.ledger.parent / name).read_bytes(), raw)
            self.assertFalse((f.ledger.parent / cl.RECOVERY).exists())

    def test_parsed_recovery_refuses_nonprivate_or_git_evidence_without_deleting(self):
        for fault in ('mode', 'git'):
            with self.subTest(fault=fault), Initialization() as f:
                backend(f)
                adverse, _ = self.crash_last_ack(f)
                evidence = f.root / 'unsafe-evidence'
                evidence.mkdir(mode=0o700)
                if fault == 'mode':
                    evidence.chmod(0o755)
                else:
                    (evidence / '.git').mkdir()
                before = copy.deepcopy(f.cloud.requests)
                with cli_boundaries(f), self.assertRaises(mc.GuardError):
                    tg.run(teardown_args(f, '--recover-journal', '--recovery-evidence-dir', str(evidence)),
                           sleep=self.no_wait, out=io.StringIO())
                self.assertEqual(f.cloud.requests, before)
                for name, raw in adverse.items():
                    self.assertEqual((f.ledger.parent / name).read_bytes(), raw)
                self.assertFalse((f.ledger.parent / cl.RECOVERY).exists())

    def test_parsed_run_repairs_actual_orphan_archives_adverse_bytes_and_cleans_seven(self):
        with Initialization() as f:
            backend(f)
            adverse, orphan = self.crash_last_ack(f)
            evidence = f.root / 'public-recovery-evidence'
            args = teardown_args(f, '--recover-journal', '--recovery-evidence-dir', str(evidence))
            with cli_boundaries(f):
                ok, receipt = tg.run(args, sleep=self.no_wait, out=io.StringIO(),
                                     runner=lambda *args: self.fail('repair execution called legacy CLI'))
            self.assertTrue(ok)
            self.assertTrue(receipt['result']['complete'])
            self.assertTrue(receipt['result']['hard_lifetime_met'])
            journal = cl.Journal(f.ledger)
            f.stack.callback(journal.close)
            state = journal.read()
            self.assertEqual(state['mode'], 'COMPLETE')
            self.assertEqual(state['intents'][6]['ack'], orphan['state']['intents'][6]['ack'])
            self.assertEqual(state['intents'][6]['actions'], orphan['state']['intents'][6]['actions'])
            self.assertEqual(len(f.cloud.requests), 14)
            self.assertEqual([(method, path) for method, path, _, _ in f.cloud.requests if method == 'DELETE'],
                             [('DELETE', f'/servers/{rid}') for rid in (101, 102, 103, 104)] +
                             [('DELETE', '/firewalls/301'), ('DELETE', '/ssh_keys/401'), ('DELETE', '/networks/201')])
            repairs = [event for event in state['events'] if event.get('event') == 'journal-recovery-closed']
            self.assertEqual(len(repairs), 1)
            proof = repairs[0]['evidence']
            self.assertEqual(Path(proof['archive']).parent, evidence)
            self.assertEqual(proof['candidate_source'], 'orphan')
            self.assertEqual({item['source'] for item in proof['files']}, set(adverse))
            for item in proof['files']:
                self.assertEqual((Path(proof['archive']) / item['archive_name']).read_bytes(), adverse[item['source']])
                self.assertEqual(item['sha256'], cc.digest(adverse[item['source']]))
            self.assertFalse((f.ledger.parent / cl.RECOVERY).exists())
            self.assertEqual(journal.header, f.header)
            for kind, ids in journal.header['baseline'].items():
                self.assertEqual(sorted(f.cloud.rows[kind]), ids)


if __name__ == '__main__':
    unittest.main()
