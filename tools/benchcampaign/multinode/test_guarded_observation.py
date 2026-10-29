#!/usr/bin/env python3
"""Observe each exact owned read before later work can fail or publish output.

CLI, Journal, pinning, identity continuity and cleanup are real. Only provider
read results, process identities, fixed clocks and private input files are fake.
"""
import copy
import io
import json
import os
from pathlib import Path
import sys
import unittest

HERE = Path(os.path.abspath(__file__)).parent
sys.path.insert(0, str(HERE))

import campaign_cloud as cc  # noqa: E402
import mn_common as mc  # noqa: E402
import provision  # noqa: E402
from test_guarded_initialize import Initialization  # noqa: E402
from test_guarded_orchestration import OrchestrationProvider, armed, execute, plan_creates  # noqa: E402
import test_guarded_readbacks as rb  # noqa: E402


class ObservationProvider(OrchestrationProvider):
    """Trace the real public caller through its ownership-read wrappers."""

    def __init__(self, fixture, profile):
        super().__init__(fixture, profile)
        self.reads, self.on_read = [], None

    def boundary(self, operation, kind, rid, seconds, caller):
        direct = caller
        source, owner = None, None
        frame = caller
        while frame is not None:
            filename, function = Path(frame.f_code.co_filename).name, frame.f_code.co_name
            if filename == 'campaign_safety.py' and function in {'describe_owned', 'read_owned'}:
                source = source or frame.f_locals.get('source')
            if filename == 'provision.py' and function == 'execute_guarded':
                owner = 'provision-host-readback' if 'hosts' in frame.f_locals else 'provision-cascade-readback'
                break
            if filename == 'primary_ip_check.py' and function == 'check':
                owner = 'primary-check-exact-readback'
                break
            frame = frame.f_back
        direct_cli = Path(direct.f_code.co_filename).name in {'provision.py', 'primary_ip_check.py'}
        # Campaign's internal create/action reads must not impersonate a direct
        # CLI read. Wrapper source is retained verbatim as independent evidence.
        site = owner if source is not None or direct_cli else None
        entry = dict(operation=operation, kind=kind, id=rid, site=site, source=source,
                     direct_function=direct.f_code.co_name, transmitted=False)
        self.reads.append(entry)
        result = rb.DEFAULT if self.on_read is None else self.on_read(entry)
        allowance = self.allowance(seconds)
        entry.update(allowance=allowance, transmitted=True)
        return allowance, result

    def describe(self, kind, rid, seconds=20, **kwargs):
        allowance, result = self.boundary('describe', kind, rid, seconds, sys._getframe(1))
        return super().describe(kind, rid, allowance) if result is rb.DEFAULT else result

    def list(self, kind, seconds=20, **kwargs):
        allowance, result = self.boundary('list', kind, None, seconds, sys._getframe(1))
        return super().list(kind, allowance) if result is rb.DEFAULT else result


def backend(f):
    f.cloud = ObservationProvider(f, f.profile)
    f.cloud.rows = copy.deepcopy(f.baseline)
    return f.cloud


def headed_retirements(f):
    # Evidence only; never acquire a nested journal authorization lock in a
    # provider callback, or use this raw head to authorize a mutation.
    return rb.retirements(json.loads(f.ledger.read_text())['state'])


def has_server_retirement(events, sid, source):
    return any(kind == 'server' and rid == sid and event_source.startswith(source)
               for kind, rid, event_source in events)


class ExactObservationTest(unittest.TestCase):
    def no_wait(self, seconds):
        self.fail('healthy synthetic actions unexpectedly waited')

    def assert_retired_before_later_read(self, f, sid, source):
        self.assertTrue(has_server_retirement(headed_retirements(f), sid, source))

    def cleanup_retired(self, f, original, source, positive_server=None):
        f.cloud.on_read = None
        self.assertEqual(f.cloud.describe('server', 101), original)
        saved = rb.reopened(f)
        self.assertTrue(has_server_retirement(rb.retirements(saved.read()), 101, source))
        f.actor = 112
        f.processes.pop(111)
        cleanup = f.campaign(saved)
        cleanup.close_admission()
        requests = copy.deepcopy(f.cloud.requests)
        with self.assertRaisesRegex(mc.GuardError, 'retired'):
            cleanup.mutate('server', 101, 'delete')
        self.assertEqual(f.cloud.requests, requests)
        result = cleanup.cleanup_pass()
        self.assertFalse(result['complete'])
        deletes = [path for method, path, _, _ in f.cloud.requests if method == 'DELETE']
        self.assertNotIn('/servers/101', deletes)
        self.assertIn(101, f.cloud.rows['server'])
        self.assertIn(10101, f.cloud.rows['primary-ip'])
        if positive_server is not None:
            self.assertIn(f'/servers/{positive_server}', deletes)
            self.assertNotIn(positive_server, f.cloud.rows['server'])
        else:
            self.assertTrue(any(path.startswith(('/firewalls/', '/ssh_keys/')) for path in deletes), deletes)
        self.assertTrue(has_server_retirement(rb.retirements(rb.reopened(f).read()), 101, source))

    def test_cleanup_preliminary_exact_network_drift_is_retired_before_original_read_can_delete(self):
        with Initialization() as f:
            backend(f)
            journal, campaign = armed(f)
            plan_creates(f, campaign, count=3)
            original = copy.deepcopy(f.cloud.rows['network'][201])
            f.actor = 112
            f.processes.pop(111)
            campaign.close_admission()
            supplied, censuses, later_original = [], [], []
            def discontinuity(read):
                if read['operation'] == 'list' and read['kind'] == 'network':
                    self.assertEqual(f.cloud.rows['network'][201], original)
                    censuses.append(read)
                if read['operation'] != 'describe' or read['kind'] != 'network' or read['id'] != 201:
                    return rb.DEFAULT
                if read['source'] == 'cleanup-exact-readback' and not supplied:
                    self.assertTrue(censuses, 'real recovery census must precede preliminary exact read')
                    self.assertFalse(any(kind == 'network' and rid == 201
                                         for kind, rid, _ in headed_retirements(f)))
                    supplied.append(read)
                    value = copy.deepcopy(original)
                    value['created'] = '2099-01-01T00:00:00Z'
                    return value
                if supplied:
                    self.assertTrue(any(kind == 'network' and rid == 201
                                        for kind, rid, _ in headed_retirements(f)))
                    later_original.append(read)
                return rb.DEFAULT
            f.cloud.on_read = discontinuity
            result = campaign.cleanup_pass()
            self.assertFalse(result['complete'])
            self.assertEqual(len(supplied), 1)
            self.assertEqual(f.cloud.rows['network'][201], original)
            saved = rb.reopened(f)
            self.assertTrue(any(kind == 'network' and rid == 201
                                for kind, rid, _ in rb.retirements(saved.read())))
            deletes = [path for method, path, _, _ in f.cloud.requests if method == 'DELETE']
            self.assertNotIn('/networks/201', deletes)
            self.assertIn('/firewalls/301', deletes)
            self.assertIn('/ssh_keys/401', deletes)
            # Every subsequent exact result is original-looking, including the
            # fresh guard's preliminary read. The tombstone must still govern.
            retry = f.campaign(saved)
            requests = copy.deepcopy(f.cloud.requests)
            self.assertFalse(retry.cleanup_pass()['complete'])
            self.assertTrue(later_original)
            self.assertEqual(f.cloud.requests, requests)
            self.assertEqual(f.cloud.rows['network'][201], original)
            self.assertTrue(any(kind == 'network' and rid == 201
                                for kind, rid, _ in rb.retirements(rb.reopened(f).read())))

    def test_provision_cascade_exact_present_drift_is_durable_before_ip_census_failure(self):
        with Initialization() as f:
            backend(f)
            journal, _ = armed(f)
            supplied, later = [], []
            original = []
            source = 'provision-cascade-readback'
            def drift(read):
                if read['site'] != source:
                    return rb.DEFAULT
                if read['operation'] == 'describe' and read['kind'] == 'server' and read['id'] == 101 and not supplied:
                    value = copy.deepcopy(f.cloud.rows['server'][101])
                    original.append(copy.deepcopy(value))
                    value['created'] = '2099-01-01T00:00:00Z'
                    supplied.append(read)
                    return value
                if supplied and read['operation'] == 'list' and read['kind'] == 'primary-ip':
                    self.assert_retired_before_later_read(f, 101, source)
                    later.append(read)
                    raise mc.GuardError('synthetic later provisioning IP census failure')
                return rb.DEFAULT
            f.cloud.on_read = drift
            with self.assertRaises(mc.GuardError):
                execute(f, journal, self.no_wait)
            self.assertEqual(len(supplied), 1)
            self.assertLessEqual(len(later), 1)  # An immediate continuity refusal may avoid the read.
            self.assertEqual(f.cloud.rows['server'][101], original[0])
            self.assertTrue(has_server_retirement(rb.retirements(rb.reopened(f).read()), 101, source))
            self.assertEqual(len(f.cloud.requests), 5)
            self.assertFalse((f.work / 'hosts-all.json').exists())
            self.assertTrue((f.work / 'guard' / 'stop.json').exists())
            self.cleanup_retired(f, original[0], source)

    def test_provision_hosts_none_or_single_present_drift_cannot_publish_or_regain_cleanup(self):
        for observation in ('none', 'drift-then-error', 'drift-then-original'):
            with self.subTest(observation=observation), Initialization() as f:
                backend(f)
                journal, _ = armed(f)
                source = 'provision-host-readback'
                supplied, later, original = [], [], []
                def changed(read):
                    if read['site'] != source or read['operation'] != 'describe' or read['kind'] != 'server':
                        return rb.DEFAULT
                    if read['id'] == 101 and not supplied:
                        value = copy.deepcopy(f.cloud.rows['server'][101])
                        original.append(copy.deepcopy(value))
                        supplied.append(read)
                        if observation == 'none':
                            return None
                        value['created'] = '2099-01-01T00:00:00Z'
                        return value
                    if supplied:
                        self.assert_retired_before_later_read(f, 101, source)
                        later.append(read)
                        if observation == 'drift-then-error':
                            raise mc.GuardError('synthetic later host exact read failure')
                    return rb.DEFAULT  # All subsequent descriptions are original-looking.
                f.cloud.on_read = changed
                with self.assertRaises(mc.GuardError):
                    execute(f, journal, self.no_wait)
                self.assertEqual(len(supplied), 1)
                if observation == 'none':
                    self.assertEqual(later, [])
                self.assertEqual(f.cloud.rows['server'][101], original[0])
                self.assertTrue(has_server_retirement(rb.retirements(rb.reopened(f).read()), 101, source))
                self.assertEqual(len(f.cloud.requests), 16)
                self.assertFalse((f.work / 'hosts-all.json').exists())
                self.assertTrue((f.work / 'guard' / 'stop.json').exists())
                self.cleanup_retired(f, original[0], source, positive_server=102)

    def test_checker_first_exact_drift_is_durable_before_next_server_or_ip_list_failure(self):
        for fault in ('next-server', 'ip-list'):
            with self.subTest(fault=fault), Initialization() as f:
                backend(f)
                rb.create_servers(f, count=2)
                args = rb.checker_args(f, 'node1')
                previous_state, previous_owned = b'{"sentinel":"state"}\n', b'{"sentinel":"owned"}\n'
                f.private(Path(args.state).name, previous_state.decode())
                f.private(Path(args.owned_output).name, previous_owned.decode())
                source = 'primary-check-exact-readback'
                original = copy.deepcopy(f.cloud.rows['server'][101])
                supplied, later = [], []
                def changed(read):
                    if read['site'] != source:
                        return rb.DEFAULT
                    if read['operation'] == 'describe' and read['kind'] == 'server' and read['id'] == 101 and not supplied:
                        supplied.append(read)
                        value = copy.deepcopy(original)
                        value['created'] = '2099-01-01T00:00:00Z'
                        return value
                    if supplied:
                        self.assert_retired_before_later_read(f, 101, source)
                        target = (fault == 'next-server' and read['operation'] == 'describe' and read['id'] == 102
                                  or fault == 'ip-list' and read['operation'] == 'list' and read['kind'] == 'primary-ip')
                        if target:
                            later.append(read)
                            raise mc.GuardError('synthetic later checker read failure')
                    return rb.DEFAULT
                f.cloud.on_read = changed
                requests = copy.deepcopy(f.cloud.requests)
                with self.assertRaises(mc.GuardError):
                    rb.checker(f, args)
                self.assertEqual(len(supplied), 1)
                self.assertLessEqual(len(later), 1)
                self.assertTrue(has_server_retirement(rb.retirements(rb.reopened(f).read()), 101, source))
                self.assertEqual(f.cloud.rows['server'][101], original)
                self.assertEqual(f.cloud.requests, requests)
                self.assertEqual(Path(args.state).read_bytes(), previous_state)
                self.assertEqual(Path(args.owned_output).read_bytes(), previous_owned)
                self.cleanup_retired(f, original, source, positive_server=102)


class ParsedProtectionInputTest(unittest.TestCase):
    def test_valid_protection_drift_after_real_parse_refuses_before_plan_or_run_census(self):
        for entrypoint in ('guarded_plan', 'run'):
            with self.subTest(entrypoint=entrypoint), Initialization() as f:
                backend(f)
                args = provision.parse_args(f.argv)
                approved = f.protect.read_bytes()
                parsed_names = list(args.protected_name)
                self.assertNotIn('changed-after-parsing', parsed_names)
                f.protect.write_bytes(approved + b'server\t99999\tchanged-after-parsing\n')
                self.assertNotEqual(cc.digest(f.protect.read_bytes()), cc.digest(approved))
                with rb.cli_boundaries(f), self.assertRaisesRegex(
                        mc.GuardError, 'protected campaign input changed after parsing'):
                    if entrypoint == 'guarded_plan':
                        provision.guarded_plan(args, f.deadline)
                    else:
                        provision.run(args, now=f.wall // 1_000_000_000, out=io.StringIO())
                self.assertEqual(args.protected_name, parsed_names)
                self.assertEqual(f.cloud.reads, [])
                self.assertEqual(f.cloud.lists, [])
                self.assertEqual(f.cloud.describes, [])
                self.assertEqual(f.cloud.requests, [])
                self.assertFalse(f.ledger.exists())
                self.assertFalse((f.work / 'hosts-all.json').exists())

    def test_unchanged_parsed_protection_bytes_reach_real_plan_and_initialization(self):
        with Initialization() as f:
            backend(f)
            args = provision.parse_args(f.argv)
            approved = f.protect.read_bytes()
            plan = provision.guarded_plan(args, f.deadline)
            self.assertEqual(plan.private_inputs[str(f.protect)], cc.digest(approved))
            journal = provision.initialize_guarded(args, f.deadline, f.profile, f.cloud, plan)
            f.stack.callback(journal.close)
            self.assertEqual(journal.header['private_inputs'][str(f.protect)], cc.digest(approved))
            self.assertEqual(journal.header['protections'], plan.protections)
            self.assertTrue(journal.header['protections']['names'])
            self.assertEqual([kind for kind, _ in f.cloud.lists], list(mc.PROTECT_KINDS))
            self.assertEqual(f.cloud.requests, [])
            self.assertTrue(f.ledger.exists())
            self.assertEqual(journal.checkpoint['generation'], 0)


if __name__ == '__main__':
    unittest.main()
