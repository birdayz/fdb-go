#!/usr/bin/env python3
"""Real out-of-band caller publication ordering and storage-failure controls."""
import contextlib
import copy
import json
import os
from pathlib import Path
import stat
import sys
import unittest
from unittest.mock import patch

HERE = Path(os.path.abspath(__file__)).parent
sys.path.insert(0, str(HERE))

import campaign_cloud as cc  # noqa: E402
import campaign_safety as cs  # noqa: E402
import creation_ledger as cl  # noqa: E402
from test_campaign_safety import Fixture, NS  # noqa: E402


STEPS = ('write', 'flush', 'file-sync', 'replace', 'directory-sync')


class PublicationStream:
    """Observe explicit buffered operations while keeping real file descriptors."""
    def __init__(self, stream, trace):
        self.stream, self.trace = stream, trace

    def __enter__(self):
        self.stream.__enter__()
        return self

    def __exit__(self, *args):
        return self.stream.__exit__(*args)

    def write(self, data):
        self.trace.data = data
        if self.trace.failure == 'write':
            # The real buffered stream closes on unwind, retaining these bytes.
            self.stream.write(data[:len(data) // 2])
        self.trace.step('write')
        return self.stream.write(data)

    def flush(self):
        self.trace.step('flush')
        return self.stream.flush()


class PublicationTrace:
    """Wrap only the first receipt publication made by the actual caller."""
    def __init__(self, case, destination, failure=None):
        self.case, self.destination, self.failure = case, destination, failure
        self.events, self.data, self.temporary = [], None, None
        self.replaced, self.completed = False, False
        self.stack = contextlib.ExitStack()

    def step(self, name):
        self.events.append(name)
        if self.failure == name:
            raise OSError('synthetic out-of-band ' + name + ' failure')

    def __enter__(self):
        original_fdopen, original_sync, original_replace = cs.os.fdopen, cs.os.fsync, cs.os.replace

        def fdopen(fd, *args, **kwargs):
            target = Path(os.readlink(f'/proc/self/fd/{fd}'))
            stream = original_fdopen(fd, *args, **kwargs)
            if (self.temporary is None and target.parent == self.destination.parent
                    and target.name.startswith('.receipt-')):
                info = os.fstat(fd)
                self.case.assertTrue(stat.S_ISREG(info.st_mode))
                self.case.assertEqual(stat.S_IMODE(info.st_mode), 0o600)
                self.case.assertEqual((info.st_uid, info.st_nlink), (os.getuid(), 1))
                self.case.assertEqual(args, ('wb',))
                self.temporary = target
                return PublicationStream(stream, self)
            return stream

        def sync(fd):
            target = Path(os.readlink(f'/proc/self/fd/{fd}'))
            if not self.completed and target == self.temporary:
                self.case.assertEqual(self.events, ['write', 'flush'])
                self.case.assertEqual(target.read_bytes(), self.data)
                self.step('file-sync')
            elif not self.completed and self.replaced and target == self.destination.parent:
                self.case.assertEqual(self.events, ['write', 'flush', 'file-sync', 'replace'])
                self.case.assertEqual(self.destination.read_bytes(), self.data)
                self.step('directory-sync')
                original_sync(fd)
                self.completed = True
                return
            return original_sync(fd)

        def replace(source, destination, *args, **kwargs):
            if Path(source) == self.temporary and not self.completed:
                self.case.assertEqual(Path(destination), self.destination)
                self.case.assertEqual(self.events, ['write', 'flush', 'file-sync'])
                self.case.assertEqual(self.temporary.read_bytes(), self.data)
                self.step('replace')
                result = original_replace(source, destination, *args, **kwargs)
                self.replaced = True
                return result
            return original_replace(source, destination, *args, **kwargs)

        self.stack.enter_context(patch.object(cs.os, 'fdopen', side_effect=fdopen))
        self.stack.enter_context(patch.object(cs.os, 'fsync', side_effect=sync))
        self.stack.enter_context(patch.object(cs.os, 'replace', side_effect=replace))
        return self

    def __exit__(self, *args):
        return self.stack.__exit__(*args)


class CampaignPublicationTest(unittest.TestCase):
    def setUp(self):
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        self.stack.enter_context(patch.dict(os.environ, {}, clear=True))
        self.stack.enter_context(patch.object(cc.http.client.HTTPSConnection, 'connect',
            side_effect=AssertionError('publication controls must never use a real network')))
        self.stack.enter_context(patch.object(cs.os, 'pidfd_open', create=True,
            side_effect=AssertionError('publication controls must never open a real pidfd')))
        self.stack.enter_context(patch.object(cs.signal, 'pidfd_send_signal', create=True,
            side_effect=AssertionError('publication controls must never signal a real process')))
        self.stack.enter_context(patch.object(cs.os, 'kill',
            side_effect=AssertionError('publication controls must never use a bare PID signal')))

    def prepare(self, f, caller):
        self.assertEqual(f.create(), 201)
        self.assertTrue(f.campaign.poll_actions())
        f.closing()
        f.campaign.heartbeat()  # Establish a strict released lease and cached guard observation.
        self.assertIsNone(f.campaign._watched_lease)
        original = f.journal.read()
        self.assertEqual(original['mode'], 'CLOSING')
        self.assert_unlatched(original)
        self.assertFalse((f.guard_directory / 'hard-lifetime-breached.json').exists())
        self.assertFalse((f.guard_directory / 'cleanup.json').exists())
        f.mono += NS
        f.wall = f.header['time']['h_ns'] + NS if caller == 'marker' else f.wall + NS
        fixed_origin = f.header['time']['t0_ns'] + f.mono - f.header['time']['monotonic_t0_ns']
        self.assertLess(fixed_origin, f.header['time']['h_ns'])
        name = {'heartbeat': 'heartbeat.json', 'stop': 'stop.json', 'marker': 'hard-lifetime-breached.json'}[caller]
        destination = f.guard_directory / name
        return original, destination

    def assert_unlatched(self, state):
        self.assertNotIn('hard_lifetime_breached', state)
        self.assertFalse(any(event.get('event') in ('hard-lifetime-breached', 'hard-deadline-unresolved')
                             for event in state['events']))

    def invoke(self, f, caller):
        return f.campaign.request_stop() if caller == 'stop' else f.campaign.heartbeat()

    def assert_payload(self, f, caller, raw, checkpoint):
        payload = json.loads(raw)
        if caller == 'stop':
            expected = dict(binding=f.journal.binding, requested_unix_ns=f.campaign.clock())
        elif caller == 'marker':
            expected = dict(binding=f.journal.binding, hard_lifetime_breached=True,
                            observed_unix_ns=f.campaign.clock(), hard_lifetime_met=False)
        else:
            guard = f.campaign._last_observation['guard']
            expected = dict(binding=f.journal.binding, identity=f.processes[112], challenge=guard['challenge'],
                            checkpoint=checkpoint, monotonic_ns=f.mono)
        self.assertEqual(payload, expected)
        self.assertEqual(raw, cc.wire_json(expected) + b'\n')

    def assert_storage_and_authority_unchanged(self, f, original, head, checkpoint, requests):
        self.assertEqual(f.journal.path.read_bytes(), head)
        self.assertEqual(f.journal.checkpoint, checkpoint)
        self.assertEqual(f.journal.peek(), original)
        self.assert_unlatched(f.journal.peek())
        self.assertIsNone(f.journal.read_if_available(), 'independent Journal must still own the actual flock')
        self.assertEqual(f.cloud.requests, requests)
        self.assertEqual(f.deletes(), [])
        self.assertIn(201, f.cloud.rows['network'])
        self.assertFalse((f.guard_directory / 'cleanup.json').exists())

    def test_callers_publish_write_flush_file_sync_replace_then_directory_sync(self):
        for caller in ('heartbeat', 'stop', 'marker'):
            with self.subTest(caller=caller), Fixture() as f:
                original, destination = self.prepare(f, caller)
                head, checkpoint = f.journal.path.read_bytes(), copy.deepcopy(f.journal.checkpoint)
                requests = copy.deepcopy(f.cloud.requests)
                heartbeat_before = (f.guard_directory / 'heartbeat.json').read_bytes()
                self.assertEqual(list(f.guard_directory.glob('.receipt-*')), [])
                with contextlib.closing(cl.Journal(f.journal.path)) as external:
                    with external.locked() as held:
                        self.assertEqual(held, original)
                        self.assertIsNone(f.journal.read_if_available())
                        with PublicationTrace(self, destination) as trace:
                            result = self.invoke(f, caller)
                        self.assertEqual(trace.events, list(STEPS))
                        self.assertTrue(trace.replaced)
                        self.assertTrue(trace.completed)
                        self.assertFalse(trace.temporary.exists())
                        self.assertEqual(destination.read_bytes(), trace.data)
                        self.assert_payload(f, caller, trace.data, checkpoint)
                        self.assertEqual(stat.S_IMODE(destination.stat().st_mode), 0o600)
                        self.assertEqual(list(f.guard_directory.glob('.receipt-*')), [])
                        if caller == 'stop':
                            self.assertIsNone(result)
                        else:
                            self.assertEqual(result, original)
                        if caller == 'marker':
                            self.assertNotEqual((f.guard_directory / 'heartbeat.json').read_bytes(), heartbeat_before)
                            self.assert_unlatched(f.campaign._last_observation)
                        self.assert_storage_and_authority_unchanged(f, original, head, checkpoint, requests)
                self.assertEqual(f.journal.read(), original)
                self.assert_unlatched(f.journal.read())

    def test_each_publication_failure_propagates_without_success_and_retains_real_residue(self):
        for caller in ('heartbeat', 'stop', 'marker'):
            for failure in STEPS:
                with self.subTest(caller=caller, failure=failure), Fixture() as f:
                    original, destination = self.prepare(f, caller)
                    head, checkpoint = f.journal.path.read_bytes(), copy.deepcopy(f.journal.checkpoint)
                    requests = copy.deepcopy(f.cloud.requests)
                    previous = destination.read_bytes() if destination.exists() else None
                    heartbeat_before = (f.guard_directory / 'heartbeat.json').read_bytes()
                    self.assertEqual(list(f.guard_directory.glob('.receipt-*')), [])
                    returned = []
                    with contextlib.closing(cl.Journal(f.journal.path)) as external:
                        with external.locked() as held:
                            self.assertEqual(held, original)
                            self.assertIsNone(f.journal.read_if_available())
                            with PublicationTrace(self, destination, failure) as trace:
                                with self.assertRaisesRegex(OSError, 'synthetic out-of-band ' + failure + ' failure'):
                                    returned.append(self.invoke(f, caller))
                            self.assertEqual(returned, [], 'visible bytes must not substitute for a successful durable return')
                            self.assertEqual(trace.events, list(STEPS[:STEPS.index(failure) + 1]))
                            self.assertFalse(trace.completed)
                            self.assert_payload(f, caller, trace.data, checkpoint)
                            if failure == 'directory-sync':
                                self.assertTrue(trace.replaced)
                                self.assertFalse(trace.temporary.exists())
                                self.assertEqual(destination.read_bytes(), trace.data)
                                self.assertEqual(list(f.guard_directory.glob('.receipt-*')), [])
                            else:
                                self.assertFalse(trace.replaced)
                                self.assertEqual(destination.read_bytes() if destination.exists() else None, previous)
                                self.assertEqual(list(f.guard_directory.glob('.receipt-*')), [trace.temporary])
                                expected = trace.data[:len(trace.data) // 2] if failure == 'write' else trace.data
                                self.assertEqual(trace.temporary.read_bytes(), expected)
                                info = trace.temporary.stat()
                                self.assertTrue(stat.S_ISREG(info.st_mode))
                                self.assertEqual(stat.S_IMODE(info.st_mode), 0o600)
                                self.assertEqual((info.st_uid, info.st_nlink), (os.getuid(), 1))
                            if caller == 'marker':
                                self.assertEqual((f.guard_directory / 'heartbeat.json').read_bytes(), heartbeat_before,
                                                 'a failed marker publication must not report a successful heartbeat')
                                self.assert_unlatched(f.campaign._last_observation)
                            self.assert_storage_and_authority_unchanged(f, original, head, checkpoint, requests)
                    self.assertEqual(f.journal.read(), original)
                    self.assert_unlatched(f.journal.read())


if __name__ == '__main__':
    unittest.main()
