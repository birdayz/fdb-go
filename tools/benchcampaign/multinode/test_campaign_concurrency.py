"""Offline caller-level dispatch fencing, watchdog, proc and cleanup boundaries."""
import contextlib
import copy
import errno
import fcntl
import io
import json
import os
from pathlib import Path
import re
import signal
import stat
import threading
import unittest
from unittest.mock import patch

import campaign_cloud as cc
import campaign_safety as cs
import creation_ledger as cl
import mn_common as mc
from test_campaign_safety import Fixture, M0, NS, T0


IDENTITY = cs.identity


class ProcessExit(BaseException):
    """Synthetic pidfd-confirmed exit, not an ordinary recoverable call error."""


class ProcFiles:
    """Only /proc file bytes and the current PID are synthetic, not identity()."""
    def __init__(self, fixture):
        self.f = fixture
        self.local = threading.local()
        self.files = {}
        self.before_read = None
        self.stack = contextlib.ExitStack()
        self.read_text, self.read_bytes = Path.read_text, Path.read_bytes
        for pid, process in fixture.processes.items():
            self.add(pid, process['parent'], process['starttime'], str(pid).encode())

    def add(self, pid, parent, starttime, cmdline, comm='worker (nested) name)'):
        # After comm, fields start at field 3 (state). Starttime is field 22;
        # distinct sentinels on either side catch a one-field offset.
        fields = ['S', str(parent)] + [str(700 + i) for i in range(17)]
        fields += [str(starttime), '987654321', '123456789']
        self.files[f'/proc/{pid}/stat'] = f'{pid} ({comm}) ' + ' '.join(fields) + '\n'
        self.files[f'/proc/{pid}/cmdline'] = cmdline

    def remove(self, pid):
        for name in ('stat', 'cmdline'):
            self.files.pop(f'/proc/{pid}/{name}', None)
        self.f.processes.pop(pid, None)

    def actor(self, pid):
        self.local.pid = pid

    def _read(self, path, original, args, kwargs):
        name = str(path)
        if re.fullmatch(r'/proc/[0-9]+/(stat|cmdline)', name):
            if self.before_read is not None:
                self.before_read(name)
            if name not in self.files:
                raise FileNotFoundError(name)
            value = self.files[name]
            if isinstance(value, BaseException):
                raise value
            return value
        return original(path, *args, **kwargs)

    def __enter__(self):
        self.stack.enter_context(patch.object(cs, 'identity', IDENTITY))
        self.stack.enter_context(patch.object(cs.os, 'getpid', side_effect=lambda: getattr(self.local, 'pid', self.f.actor)))
        self.stack.enter_context(patch.object(cs.os, 'pidfd_open', create=True,
            side_effect=AssertionError('synthetic proc must never open a real pidfd')))
        self.stack.enter_context(patch.object(cs.signal, 'pidfd_send_signal', create=True,
            side_effect=AssertionError('synthetic proc must never send a real signal')))
        self.stack.enter_context(patch.object(cs.os, 'kill', side_effect=AssertionError('bare PID signal forbidden')))
        self.stack.enter_context(patch.object(Path, 'read_text', lambda path, *a, **kw: self._read(path, self.read_text, a, kw)))
        self.stack.enter_context(patch.object(Path, 'read_bytes', lambda path, *a, **kw: self._read(path, self.read_bytes, a, kw)))
        return self

    def __exit__(self, *args):
        return self.stack.__exit__(*args)


class Worker:
    def __init__(self, proc, call):
        self.done = threading.Event()
        self.errors, self.result = [], None
        def run():
            proc.actor(111)
            try:
                self.result = call()
            except BaseException as error:
                self.errors.append(error)
            finally:
                self.done.set()
        self.thread = threading.Thread(target=run, name='synthetic-producer', daemon=True)
        self.thread.start()

    def release_and_join(self, *pauses):
        for pause in pauses:
            pause.exit_process = True
            pause.release.set()
        if not self.done.wait(5):
            raise AssertionError('producer did not leave cleanup barrier')
        self.thread.join(5)

    def join(self, test):
        test.assertTrue(self.done.wait(5), 'producer did not leave bounded test barrier')
        self.thread.join(5)
        test.assertFalse(self.thread.is_alive())


class PublicationPause:
    """Pause a real journal directory fsync after the requested headed state."""
    def __init__(self, fixture, observer, sequence, state):
        self.f, self.observer = fixture, observer
        self.sequence, self.state = sequence, state
        self.entered, self.release = threading.Event(), threading.Event()
        self.exit_process = False
        self.original = os.fsync
        self.directory = os.stat(fixture.journal.path.parent)

    def stop(self):
        if self.entered.is_set():
            return
        self.entered.set()
        if not self.release.wait(5):
            raise AssertionError('publication barrier was not released')
        if self.exit_process:
            raise ProcessExit()

    def fsync(self, fd):
        result = self.original(fd)
        self.after_fsync(fd)
        return result

    def after_fsync(self, fd):
        info = os.fstat(fd)
        if (threading.current_thread().name == 'synthetic-producer'
                and stat.S_ISDIR(info.st_mode) and info.st_ino == self.directory.st_ino
                and info.st_dev == self.directory.st_dev and not self.entered.is_set()):
            current = self.observer.peek()
            if len(current['intents']) > self.sequence and current['intents'][self.sequence]['state'] == self.state:
                self.stop()


def observer(f, stack):
    journal = cl.Journal(f.journal.path)
    stack.callback(journal.close)
    return journal


def set_time(f, now):
    f.wall = now
    f.mono = M0 + now - T0


class DispatchFencingTests(unittest.TestCase):
    def test_operation_lock_and_concurrent_cutover_at_every_real_dispatch_boundary(self):
        for operation in ('create', 'subnet'):
            for boundary in ('final-check', 'sending', 'http', 'ack'):
                for lost in (False, True):
                    with self.subTest(operation=operation, boundary=boundary, lost=lost), \
                         Fixture() as f, ProcFiles(f) as proc, contextlib.ExitStack() as stack:
                        rid = f.create() if operation == 'subnet' else None
                        prior = len(f.cloud.requests)
                        watch = observer(f, stack)
                        cutter = observer(f, stack)
                        guard = cs.Campaign(cutter, f.cloud, clock=lambda: f.wall, monotonic=lambda: f.mono)
                        sequence = len(watch.read()['intents'])
                        pause = PublicationPause(f, watch, sequence,
                            'PREPARED' if boundary == 'final-check' else
                            ('UNKNOWN' if lost else 'ACKNOWLEDGED') if boundary == 'ack' else
                            'SENDING' if boundary == 'sending' else 'no-publication-pause')
                        stack.callback(pause.release.set)
                        stack.enter_context(patch.object(cl.os, 'fsync', side_effect=pause.fsync))
                        original_request = f.cloud.request
                        sends = []
                        def request(method, path, body=None, seconds=20, **kwargs):
                            sends.append((method, path, watch.peek()['mode']))
                            if boundary == 'http':
                                pause.stop()
                            reply = original_request(method, path, body, seconds, **kwargs)
                            if lost:
                                raise mc.GuardError('synthetic post-dispatch acknowledgement loss')
                            return reply
                        stack.enter_context(patch.object(f.cloud, 'request', side_effect=request))
                        if operation == 'subnet' and boundary == 'final-check':
                            original_describe = f.cloud.describe
                            def describe(*args, **kwargs):
                                value = original_describe(*args, **kwargs)
                                pause.stop()
                                return value
                            stack.enter_context(patch.object(f.cloud, 'describe', side_effect=describe))
                        call = (lambda: f.create()) if operation == 'create' else (
                            lambda: f.campaign.mutate('network', rid, 'subnet', copy.deepcopy(f.header['subnet'])))
                        worker = Worker(proc, call)
                        stack.callback(worker.release_and_join, pause)
                        self.assertTrue(pause.entered.wait(5), 'real operation did not reach requested boundary')
                        self.assertIsNone(watch.read_if_available(), 'operation released flock before outcome publication')
                        paused = watch.peek()
                        self.assertEqual(paused['mode'], 'OPEN')
                        if boundary == 'sending':
                            self.assertEqual(paused['intents'][sequence]['state'], 'SENDING')
                            self.assertIsNone(paused['intents'][sequence]['ack'])
                            self.assertEqual(paused['active_lease']['intent'], sequence)
                        waiting, cut_done = threading.Event(), threading.Event()
                        cut_errors, signals = [], []
                        read_fd, write_fd = os.pipe()
                        stack.callback(os.close, write_fd)
                        def wait(readable, writable, exceptional, seconds):
                            waiting.set()
                            if not worker.done.wait(5):
                                raise AssertionError('cutover did not wait for producer quiescence')
                            proc.remove(111)
                            return ([read_fd], [], [])
                        stack.enter_context(patch.object(cs.os, 'pidfd_open', return_value=read_fd))
                        stack.enter_context(patch.object(cs.select, 'select', side_effect=wait))
                        stack.enter_context(patch.object(cs.signal, 'pidfd_send_signal', side_effect=lambda *a: signals.append(a)))
                        stack.enter_context(patch.object(cs.os, 'kill', side_effect=AssertionError('bare PID signal forbidden')))
                        def cutover():
                            proc.actor(112)
                            try:
                                guard.close_admission()
                            except BaseException as error:
                                cut_errors.append(error)
                            finally:
                                cut_done.set()
                        thread = threading.Thread(target=cutover, name='synthetic-guard', daemon=True)
                        thread.start()
                        self.assertTrue(waiting.wait(5), 'guard did not reach pidfd quiescence boundary')
                        self.assertFalse(cut_done.is_set())
                        self.assertIsNone(watch.read_if_available())
                        self.assertEqual(watch.peek()['mode'], 'OPEN', 'CLOSING published before producer quiesced')
                        pause.release.set()
                        worker.join(self)
                        self.assertTrue(cut_done.wait(5), 'guard failed to close after producer released lock')
                        thread.join(5)
                        self.assertEqual(cut_errors, [])
                        self.assertEqual(signals, [])
                        final = watch.read()
                        self.assertEqual(final['mode'], 'CLOSING')
                        self.assertIsNone(final['active_lease'])
                        if boundary in {'final-check', 'sending'}:
                            self.assertEqual(sends, [])
                            self.assertEqual(len(worker.errors), 1)
                            self.assertIsInstance(worker.errors[0], mc.GuardError)
                            if boundary == 'sending':
                                self.assertEqual(final['intents'][sequence]['state'], 'UNKNOWN')
                                self.assertIsNone(final['intents'][sequence]['ack'])
                        else:
                            self.assertEqual(len(sends), 1)
                            self.assertEqual(sends[0][2], 'OPEN')
                            self.assertEqual(final['intents'][-1]['state'], 'UNKNOWN' if lost else 'ACKNOWLEDGED')
                            self.assertEqual(len(worker.errors), int(lost))
                        sent = len(f.cloud.requests)
                        self.assertEqual(sent - prior, int(boundary in {'http', 'ack'}))
                        # Even a PID with the original bytes cannot dispatch after CLOSING.
                        proc.add(111, 110, '1110', b'111')
                        proc.actor(111)
                        with self.assertRaises(mc.GuardError):
                            f.campaign.create('firewall', f.body('firewall'))
                        self.assertEqual(len(f.cloud.requests), sent)
                        self.assertIsNotNone(watch.read_if_available())


class LeaseOriginTests(unittest.TestCase):
    def test_proc_delay_cannot_rebase_create_setup_or_cleanup_send_leases(self):
        for operation in ('create', 'subnet', 'delete'):
            for near_cutoff in (False, True):
                with self.subTest(operation=operation, near_cutoff=near_cutoff), \
                     Fixture() as f, ProcFiles(f) as proc, contextlib.ExitStack() as stack:
                    rid = f.create() if operation != 'create' else None
                    if operation == 'delete':
                        proc.remove(111)
                        f.closing()
                        proc.actor(112)
                    else:
                        proc.actor(111)
                    times = copy.deepcopy(f.header['time'])
                    phase = times['c_ns'] if operation == 'create' else (
                        times['setup_ns'] if operation == 'subnet' else times['h_ns'])
                    set_time(f, phase - (3 if near_cutoff else 100) * NS)
                    f.heartbeat()
                    started_wall, started_mono = f.wall, f.mono
                    allowance = 1 if near_cutoff else 20
                    prior = len(f.cloud.requests)
                    watch = observer(f, stack)
                    sequence = len(watch.read()['intents'])
                    enabled, pending, origins, injected, published = [False], [False], [], [], []
                    def clock():
                        if enabled[0] and not injected:
                            pending[0] = True
                            origins.append((f.wall, f.mono))
                        return f.wall
                    campaign = cs.Campaign(f.journal, f.cloud, clock=clock, monotonic=lambda: f.mono)
                    actor = 112 if operation == 'delete' else 111
                    def proc_delay(name):
                        if name == f'/proc/{actor}/stat' and pending[0] and not injected:
                            injected.append((f.wall, f.mono))
                            f.wall += 4 * NS
                            f.mono += 4 * NS
                            f.heartbeat()
                    proc.before_read = proc_delay
                    original_fsync = os.fsync
                    directory = os.stat(f.journal.path.parent)
                    def fsync(fd):
                        result = original_fsync(fd)
                        info = os.fstat(fd)
                        if stat.S_ISDIR(info.st_mode) and (info.st_dev, info.st_ino) == (directory.st_dev, directory.st_ino):
                            current = watch.peek()
                            if len(current['intents']) > sequence:
                                intent = current['intents'][sequence]
                                if operation == 'create' and intent['state'] == 'PREPARED':
                                    enabled[0] = True
                                if intent['state'] == 'SENDING' and not published:
                                    self.assertIsNone(watch.read_if_available())
                                    published.append(copy.deepcopy(current['active_lease']))
                        return result
                    stack.enter_context(patch.object(cl.os, 'fsync', side_effect=fsync))
                    if operation != 'create':
                        original_describe = f.cloud.describe
                        def describe(*args, **kwargs):
                            value = original_describe(*args, **kwargs)
                            enabled[0] = True
                            return value
                        stack.enter_context(patch.object(f.cloud, 'describe', side_effect=describe))
                    def call():
                        if operation == 'create':
                            return campaign.create('network', f.body())
                        return campaign.mutate('network', rid, operation,
                            copy.deepcopy(f.header['subnet']) if operation == 'subnet' else None)
                    if near_cutoff:
                        with self.assertRaisesRegex(mc.GuardError, 'failed or ambiguous'):
                            call()
                    else:
                        call()
                    self.assertTrue(origins, 'no real budget clock was sampled before the proc delay')
                    self.assertEqual(injected, [(started_wall, started_mono)])
                    self.assertEqual(len(published), 1, 'real SENDING publication was not observed')
                    lease = published[0]
                    self.assertEqual(lease['intent'], sequence)
                    self.assertEqual(lease['deadline_ns'], min(started_wall + allowance * NS, phase - 2 * NS))
                    self.assertEqual(lease['monotonic_deadline_ns'], started_mono + allowance * NS)
                    self.assertEqual(f.journal.header['time'], times)
                    final = watch.read()
                    intent = final['intents'][sequence]
                    self.assertIsNone(final['active_lease'])
                    self.assertEqual(intent['state'], 'UNKNOWN' if near_cutoff else 'ACKNOWLEDGED')
                    self.assertEqual(len(f.cloud.requests) - prior, int(not near_cutoff))
                    if near_cutoff:
                        self.assertIsNone(intent['ack'])
                        self.assertEqual(final['mode'], 'INCIDENT' if operation == 'delete' else 'OPEN')
                        self.assertTrue((f.guard_directory / 'stop.json').exists())
                    else:
                        self.assertEqual(f.cloud.requests[-1][3], 16)


class WatchdogTests(unittest.TestCase):
    def _expiry(self, cleared_ack):
        with Fixture() as f, ProcFiles(f) as proc, contextlib.ExitStack() as stack:
            watch, guard_journal = observer(f, stack), observer(f, stack)
            guard = cs.Campaign(guard_journal, f.cloud, clock=lambda: f.wall, monotonic=lambda: f.mono)
            sending = PublicationPause(f, watch, 0, 'SENDING')
            ack = PublicationPause(f, watch, 0, 'ACKNOWLEDGED')
            stack.callback(sending.release.set)
            stack.callback(ack.release.set)
            def fsync(fd):
                sending.fsync(fd)
                if cleared_ack:
                    ack.after_fsync(fd)
            stack.enter_context(patch.object(cl.os, 'fsync', side_effect=fsync))
            worker = Worker(proc, lambda: f.create())
            stack.callback(worker.release_and_join, sending, ack)
            self.assertTrue(sending.entered.wait(5))
            proc.actor(112)
            lease = watch.peek()['active_lease']
            deadline = lease['monotonic_deadline_ns']
            self.assertEqual(deadline, M0 + 20 * NS)
            self.assertEqual(lease['deadline_ns'], T0 + 30 * NS)
            f.mono = deadline - 1
            self.assertLess(guard.clock(), lease['deadline_ns'])
            self.assertEqual(guard.heartbeat()['mode'], 'OPEN')
            self.assertEqual(guard._watched_lease, lease)
            if cleared_ack:
                sending.release.set()
                self.assertTrue(ack.entered.wait(5))
                self.assertIsNone(watch.peek()['active_lease'])
                self.assertIsNone(guard_journal.read_if_available())
                guard.heartbeat()
                self.assertEqual(guard._watched_lease, lease, 'observational clear is not strict release proof')
            f.mono = deadline
            self.assertLess(guard.clock(), lease['deadline_ns'], 'physical expiry must fire even before phase expiry')
            read_fd, write_fd = os.pipe()
            stack.callback(os.close, write_fd)
            waits, signals = [], []
            pause = ack if cleared_ack else sending
            def pidfd_open(pid):
                self.assertEqual(pid, 111)
                # Watchdog bookkeeping spends part of the same two-second reserve.
                f.mono += NS // 4
                return read_fd
            def sent(fd, sig):
                self.assertEqual(fd, read_fd)
                signals.append(sig)
                f.mono += NS // 2 if sig == signal.SIGTERM else NS // 10
                if sig == signal.SIGKILL:
                    proc.remove(111)
                    pause.exit_process = True
                    pause.release.set()
            def wait(readable, writable, exceptional, seconds):
                waits.append(seconds)
                f.mono += round(seconds * NS)
                if len(waits) == 3:
                    self.assertTrue(worker.done.wait(5))
                    return ([read_fd], [], [])
                return ([], [], [])
            with patch.object(cs.os, 'pidfd_open', side_effect=pidfd_open), \
                 patch.object(cs.signal, 'pidfd_send_signal', side_effect=sent), \
                 patch.object(cs.select, 'select', side_effect=wait), \
                 patch.object(cs.os, 'kill', side_effect=AssertionError('bare PID signal forbidden')):
                result = guard.heartbeat()
            worker.join(self)
            self.assertEqual(result['mode'], 'CLOSING', 'watchdog must fire at expiry, not expiry plus reserve')
            self.assertEqual(signals, [signal.SIGTERM, signal.SIGKILL])
            self.assertEqual(waits[0], 0)
            self.assertGreater(waits[1], 0)
            self.assertLessEqual(waits[1], 1)
            self.assertAlmostEqual(waits[2], 0.15)
            self.assertEqual(f.mono, deadline + 2 * NS)
            self.assertLess(guard.clock(), lease['deadline_ns'])
            final = watch.read()
            self.assertEqual(final['mode'], 'CLOSING')
            self.assertIsNone(final['active_lease'])
            self.assertEqual(final['intents'][0]['state'], 'ACKNOWLEDGED' if cleared_ack else 'UNKNOWN')
            self.assertEqual(len(f.cloud.requests), int(cleared_ack))
            self.assertEqual(len(worker.errors), 1)
            self.assertIsInstance(worker.errors[0], ProcessExit)

    def test_heartbeat_expires_real_sending_lease_with_one_absolute_termination_reserve(self):
        self._expiry(cleared_ack=False)

    def test_observational_ack_clear_cannot_release_a_still_locked_watchdog_lease(self):
        self._expiry(cleared_ack=True)

    def test_strict_ack_release_clears_watch_and_a_subsequent_real_lease_is_watched(self):
        with Fixture() as f, ProcFiles(f) as proc, contextlib.ExitStack() as stack:
            watch, guard_journal = observer(f, stack), observer(f, stack)
            guard = cs.Campaign(guard_journal, f.cloud, clock=lambda: f.wall, monotonic=lambda: f.mono)
            pause = PublicationPause(f, watch, 0, 'SENDING')
            stack.callback(pause.release.set)
            with patch.object(cl.os, 'fsync', side_effect=pause.fsync):
                worker = Worker(proc, lambda: f.create())
                stack.callback(worker.release_and_join, pause)
                self.assertTrue(pause.entered.wait(5))
                proc.actor(112)
                guard.heartbeat()
                first = copy.deepcopy(guard._watched_lease)
                pause.release.set()
                worker.join(self)
                self.assertEqual(worker.errors, [])
            guard.heartbeat()
            self.assertIsNone(guard._watched_lease)
            f.mono = first['monotonic_deadline_ns']
            with patch.object(cs.os, 'pidfd_open', side_effect=AssertionError('released ACK must not terminate producer')):
                self.assertEqual(guard.heartbeat()['mode'], 'OPEN')
            # Re-arm the normal receipt after advancing the synthetic clock.
            f.heartbeat()
            second = PublicationPause(f, watch, 1, 'SENDING')
            stack.callback(second.release.set)
            with patch.object(cl.os, 'fsync', side_effect=second.fsync):
                worker = Worker(proc, lambda: f.campaign.create('firewall', f.body('firewall')))
                stack.callback(worker.release_and_join, second)
                self.assertTrue(second.entered.wait(5))
                lease = watch.peek()['active_lease']
                self.assertEqual(lease['intent'], first['intent'] + 1)
                self.assertGreater(lease['deadline_ns'], first['deadline_ns'])
                self.assertGreater(lease['monotonic_deadline_ns'], first['monotonic_deadline_ns'])
                guard.heartbeat()
                self.assertEqual(guard._watched_lease, lease)
                f.mono = lease['monotonic_deadline_ns'] - 1
                with patch.object(cs.os, 'pidfd_open', side_effect=AssertionError('lease not yet expired')):
                    self.assertEqual(guard.heartbeat()['mode'], 'OPEN')
                second.release.set()
                worker.join(self)
                self.assertEqual(worker.errors, [])
            guard.heartbeat()
            self.assertIsNone(guard._watched_lease)
            self.assertEqual(len(f.cloud.requests), 2)

    def test_strictly_released_expired_lease_stays_unwatched_while_an_independent_reader_holds_the_lock(self):
        with Fixture() as f, ProcFiles(f) as proc, contextlib.ExitStack() as stack:
            self.assertEqual(f.create(), 201)
            watch, guard_journal, reader_journal = (observer(f, stack) for _ in range(3))
            guard = cs.Campaign(guard_journal, f.cloud, clock=lambda: f.wall, monotonic=lambda: f.mono)
            proc.actor(112)
            released = guard.heartbeat()
            self.assertEqual(released['mode'], 'OPEN')
            self.assertIsNone(released['active_lease'])
            self.assertEqual(released['intents'][0]['state'], 'ACKNOWLEDGED')
            lease = copy.deepcopy(released['intents'][0]['send_lease'])
            confirmed_generation = guard._strict_release_generation
            self.assertGreaterEqual(confirmed_generation, released['intents'][0]['send_generation'])
            self.assertIsNone(guard._watched_lease)
            self.assertTrue(cs.live(f.header['producer']))
            self.assertFalse((f.guard_directory / 'stop.json').exists())
            proc.add(113, 110, '1130', b'113')
            f.processes[113] = cs.identity(113)
            held, release = threading.Event(), threading.Event()
            errors = []
            def reader():
                proc.actor(113)
                try:
                    with reader_journal.locked() as state:
                        self.assertEqual(state['mode'], 'OPEN')
                        self.assertIsNone(state['active_lease'])
                        held.set()
                        if not release.wait(60):
                            raise AssertionError('test did not release the independent authenticated journal reader')
                except BaseException as error:
                    errors.append(error)
            thread = threading.Thread(target=reader, name='strict-release-reader', daemon=True)
            thread.start()
            try:
                self.assertTrue(held.wait(60), 'independent reader did not acquire its real journal flock')
                self.assertIsNone(guard_journal.read_if_available())
                busy = watch.peek()
                self.assertEqual(busy['intents'][0]['send_lease'], lease)
                self.assertLessEqual(busy['intents'][0]['send_generation'], confirmed_generation)
                f.wall = lease['deadline_ns'] + NS
                f.mono = lease['monotonic_deadline_ns'] + NS
                self.assertGreater(guard.clock(), lease['deadline_ns'])
                self.assertTrue(cs.live(f.header['producer']))
                with patch.object(cs.os, 'pidfd_open', side_effect=AssertionError(
                        'a strictly released old lease must not terminate the healthy producer')) as pidfd:
                    state = guard.heartbeat()
                    pidfd.assert_not_called()
                self.assertEqual(state['mode'], 'OPEN')
                self.assertIsNone(state['active_lease'])
                self.assertIsNone(guard._watched_lease, 'busy observation must not resurrect a strictly released saved lease')
                self.assertEqual(guard._strict_release_generation, confirmed_generation)
                self.assertTrue(thread.is_alive(), 'reader must still hold the lock throughout the heartbeat')
                self.assertIsNone(guard_journal.read_if_available())
                self.assertTrue(cs.live(f.header['producer']))
                self.assertFalse((f.guard_directory / 'stop.json').exists())
                self.assertFalse((f.guard_directory / 'cutover-incident.json').exists())
                heartbeat = json.loads(cc.private_bytes(f.guard_directory / 'heartbeat.json'))
                self.assertEqual(heartbeat['identity'], f.processes[112])
                self.assertEqual(heartbeat['monotonic_ns'], f.mono)
                self.assertEqual(len(f.cloud.requests), 1)
            finally:
                release.set()
                thread.join(60)
                self.assertFalse(thread.is_alive(), 'independent reader did not release its journal flock')
            self.assertEqual(errors, [])
            self.assertEqual(watch.read()['mode'], 'OPEN')
            self.assertIsNone(guard._watched_lease)
            self.assertTrue(cs.live(f.header['producer']))
            self.assertEqual(f.deletes(), [])

    def test_effective_only_expiry_and_late_detection_keep_the_original_signed_reserve(self):
        for boundary in ('http', 'ack'):
            for late_ns in (0, NS // 2):
                with self.subTest(boundary=boundary, late_ns=late_ns), \
                     Fixture() as f, ProcFiles(f) as proc, contextlib.ExitStack() as stack:
                    watch, guard_journal = observer(f, stack), observer(f, stack)
                    guard = cs.Campaign(guard_journal, f.cloud, clock=lambda: f.wall, monotonic=lambda: f.mono)
                    sending = PublicationPause(f, watch, 0, 'SENDING')
                    target = PublicationPause(f, watch, 0,
                        'ACKNOWLEDGED' if boundary == 'ack' else 'no-publication-pause')
                    original_fsync = os.fsync
                    def fsync(fd):
                        result = original_fsync(fd)
                        sending.after_fsync(fd)
                        if boundary == 'ack':
                            target.after_fsync(fd)
                        return result
                    stack.enter_context(patch.object(cl.os, 'fsync', side_effect=fsync))
                    if boundary == 'http':
                        f.cloud.before_request = lambda *args: target.stop()
                    worker = Worker(proc, lambda: f.create())
                    stack.callback(worker.release_and_join, sending, target)
                    self.assertTrue(sending.entered.wait(5))
                    proc.actor(112)
                    lease = copy.deepcopy(watch.peek()['active_lease'])
                    guard.heartbeat()
                    self.assertEqual(guard._watched_lease, lease)
                    sending.release.set()
                    self.assertTrue(target.entered.wait(5), 'real operation did not reach the held HTTP/ACK boundary')
                    self.assertIsNone(guard_journal.read_if_available())
                    f.wall = lease['deadline_ns'] - 1
                    self.assertLess(f.mono, lease['monotonic_deadline_ns'])
                    with patch.object(cs.os, 'pidfd_open', side_effect=AssertionError('effective lease has not expired')):
                        self.assertEqual(guard.heartbeat()['mode'], 'OPEN')
                    self.assertEqual(guard._watched_lease, lease)
                    f.wall = lease['deadline_ns'] + late_ns
                    started = f.mono
                    self.assertEqual(guard.clock(), f.wall)
                    self.assertLess(started, lease['monotonic_deadline_ns'])
                    read_fd, write_fd = os.pipe()
                    stack.callback(os.close, write_fd)
                    waits, signals = [], []
                    def pidfd_open(pid):
                        self.assertEqual(pid, 111)
                        f.mono += NS // 10
                        return read_fd
                    def sent(fd, sig):
                        self.assertEqual(fd, read_fd)
                        signals.append(sig)
                        f.mono += NS // 10
                        if sig == signal.SIGKILL:
                            proc.remove(111)
                            target.exit_process = True
                            target.release.set()
                    def wait(readable, writable, exceptional, seconds):
                        waits.append(seconds)
                        f.mono += round(seconds * NS)
                        if len(waits) == 3:
                            self.assertTrue(worker.done.wait(5))
                            return ([read_fd], [], [])
                        return ([], [], [])
                    with patch.object(cs.os, 'pidfd_open', side_effect=pidfd_open), \
                         patch.object(cs.signal, 'pidfd_send_signal', side_effect=sent), \
                         patch.object(cs.select, 'select', side_effect=wait):
                        result = guard.heartbeat()
                    worker.join(self)
                    self.assertEqual(result['mode'], 'CLOSING')
                    self.assertEqual(signals, [signal.SIGTERM, signal.SIGKILL])
                    self.assertEqual(waits[:2], [0, 1])
                    self.assertAlmostEqual(waits[2], 0.7 - late_ns / NS)
                    self.assertEqual(f.mono, started + 2 * NS - late_ns,
                        'late effective expiry must spend elapsed reserve, not start a fresh two seconds')
                    self.assertLess(f.mono, lease['monotonic_deadline_ns'], 'only effective expiry may trigger this cutover')
                    final = watch.read()
                    self.assertEqual(final['mode'], 'CLOSING')
                    self.assertIsNone(final['active_lease'])
                    intent = final['intents'][0]
                    self.assertEqual(intent['state'], 'ACKNOWLEDGED' if boundary == 'ack' else 'UNKNOWN')
                    if boundary == 'http':
                        self.assertIsNone(intent['ack'])
                    else:
                        self.assertIsNotNone(intent['ack'])
                        self.assertEqual(intent['resource_id'], 201)
                    self.assertEqual(len(f.cloud.requests), 1)
                    self.assertEqual(len(worker.errors), 1)
                    self.assertIsInstance(worker.errors[0], ProcessExit)

    def test_first_heartbeat_after_ack_clear_recovers_the_unreleased_unsampled_send_lease(self):
        for operation in ('create', 'subnet'):
            with self.subTest(operation=operation), Fixture() as f, ProcFiles(f) as proc, \
                 contextlib.ExitStack() as stack:
                rid = f.create() if operation == 'subnet' else None
                watch, guard_journal = observer(f, stack), observer(f, stack)
                guard = cs.Campaign(guard_journal, f.cloud, clock=lambda: f.wall, monotonic=lambda: f.mono)
                sequence = len(watch.read()['intents'])
                pause = PublicationPause(f, watch, sequence, 'ACKNOWLEDGED')
                stack.enter_context(patch.object(cl.os, 'fsync', side_effect=pause.fsync))
                call = (lambda: f.create()) if operation == 'create' else (
                    lambda: f.campaign.mutate('network', rid, 'subnet', copy.deepcopy(f.header['subnet'])))
                worker = Worker(proc, call)
                stack.callback(worker.release_and_join, pause)
                self.assertTrue(pause.entered.wait(5))
                proc.actor(112)
                current = watch.peek()
                self.assertIsNone(current['active_lease'])
                self.assertEqual(current['intents'][sequence]['state'], 'ACKNOWLEDGED')
                self.assertIsNone(guard_journal.read_if_available())
                self.assertIsNone(getattr(guard, '_watched_lease', None))
                lease = copy.deepcopy(current['intents'][sequence]['send_lease'])
                self.assertEqual(lease['actor'], f.header['producer'])
                self.assertEqual(lease['intent'], sequence)
                f.mono = lease['monotonic_deadline_ns'] - 1
                with patch.object(cs.os, 'pidfd_open', side_effect=AssertionError('unsampled lease not yet expired')):
                    self.assertEqual(guard.heartbeat()['mode'], 'OPEN')
                self.assertEqual(guard._watched_lease, lease,
                    'first ACK-only observation must recover the still-unreleased original lease')
                self.assertIsNone(guard_journal.read_if_available())
                f.mono = lease['monotonic_deadline_ns']
                read_fd, write_fd = os.pipe()
                stack.callback(os.close, write_fd)
                waits, signals = [], []
                def pidfd_open(pid):
                    self.assertEqual(pid, 111)
                    f.mono += NS // 10
                    return read_fd
                def sent(fd, sig):
                    self.assertEqual(fd, read_fd)
                    signals.append(sig)
                    f.mono += NS // 10
                    if sig == signal.SIGKILL:
                        proc.remove(111)
                        pause.exit_process = True
                        pause.release.set()
                def wait(readable, writable, exceptional, seconds):
                    waits.append(seconds)
                    f.mono += round(seconds * NS)
                    if len(waits) == 3:
                        self.assertTrue(worker.done.wait(5))
                        return ([read_fd], [], [])
                    return ([], [], [])
                with patch.object(cs.os, 'pidfd_open', side_effect=pidfd_open), \
                     patch.object(cs.signal, 'pidfd_send_signal', side_effect=sent), \
                     patch.object(cs.select, 'select', side_effect=wait):
                    result = guard.heartbeat()
                worker.join(self)
                self.assertEqual(result['mode'], 'CLOSING')
                self.assertEqual(signals, [signal.SIGTERM, signal.SIGKILL])
                self.assertEqual(waits[:2], [0, 1])
                self.assertAlmostEqual(waits[2], 0.7)
                self.assertEqual(f.mono, lease['monotonic_deadline_ns'] + 2 * NS)
                final = watch.read()
                self.assertIsNone(final['active_lease'])
                self.assertEqual(final['intents'][sequence]['state'], 'ACKNOWLEDGED')
                self.assertEqual(final['intents'][sequence]['ack'], current['intents'][sequence]['ack'])
                self.assertEqual(final['intents'][sequence]['send_lease'], lease)
                self.assertEqual(len(f.cloud.requests), sequence + 1)
                self.assertEqual(len(worker.errors), 1)
                self.assertIsInstance(worker.errors[0], ProcessExit)


class ProducerExitTests(unittest.TestCase):
    def test_guard_loop_handles_normal_exit_at_pidfd_open_term_and_kill_without_resetting_the_budget(self):
        for boundary in ('open', 'term', 'kill'):
            for outcome in ('gone', 'pid-reused', 'still-live'):
                with self.subTest(boundary=boundary, outcome=outcome), Fixture() as f, \
                     ProcFiles(f) as proc, contextlib.ExitStack() as stack:
                    f.create()
                    original_producer = copy.deepcopy(f.header['producer'])
                    proc.remove(112)
                    proc.add(113, 110, '1130', b'113')
                    f.processes[113] = cs.identity(113)
                    proc.actor(113)
                    guard_journal = observer(f, stack)
                    guard = cs.Campaign(guard_journal, f.cloud, clock=lambda: f.wall, monotonic=lambda: f.mono)
                    read_fd, write_fd = os.pipe()
                    stack.callback(os.close, read_fd)
                    stack.callback(os.close, write_fd)
                    opens, signals, waits, races, delete_actors = [], [], [], [], []
                    started = f.mono
                    def exit_race():
                        races.append(boundary)
                        if outcome == 'gone':
                            proc.remove(111)
                        elif outcome == 'pid-reused':
                            proc.add(111, 110, '1111', b'replacement\x00not-our-producer\x00')
                            f.processes[111] = cs.identity(111)
                        raise ProcessLookupError(errno.ESRCH, 'synthetic exit between identity/readiness and pidfd operation')
                    def pidfd_open(pid):
                        self.assertEqual(pid, 111)
                        opens.append(pid)
                        f.mono += NS // 10
                        if boundary == 'open':
                            exit_race()
                        return os.dup(read_fd)
                    def sent(fd, sig):
                        self.assertTrue(cs.live(original_producer), 'never signal an identity-reused PID')
                        signals.append(sig)
                        f.mono += NS // 10
                        if (boundary, sig) in (('term', signal.SIGTERM), ('kill', signal.SIGKILL)):
                            exit_race()
                    def wait(readable, writable, exceptional, seconds):
                        waits.append(seconds)
                        self.assertGreaterEqual(seconds, 0)
                        self.assertLessEqual(seconds, max(0, (started + 2 * NS - f.mono) / NS))
                        if not cs.live(original_producer):
                            return (readable, [], [])
                        f.mono += round(seconds * NS)
                        return ([], [], [])
                    f.cloud.before_request = lambda method, path, body, seconds: (
                        delete_actors.append(cs.identity()['pid']) if method == 'DELETE' else None)
                    with patch.object(cs.os, 'pidfd_open', side_effect=pidfd_open), \
                         patch.object(cs.signal, 'pidfd_send_signal', side_effect=sent), \
                         patch.object(cs.select, 'select', side_effect=wait):
                        if outcome == 'still-live':
                            with self.assertRaises((mc.GuardError, ProcessLookupError)):
                                guard.guard_loop(sleep=lambda seconds: self.fail('failed quiescence must not authorize cleanup'))
                        else:
                            result = guard.guard_loop(sleep=lambda seconds: self.fail('normal producer exit should finish cleanup'))
                            self.assertTrue(result['complete'])
                            self.assertTrue(result['hard_lifetime_met'])
                    self.assertEqual(opens, [111], 'ESRCH must not start a new pidfd/quiescence attempt')
                    self.assertEqual(races, [boundary])
                    self.assertEqual(signals, [] if boundary == 'open' else [signal.SIGTERM] if boundary == 'term'
                        else [signal.SIGTERM, signal.SIGKILL])
                    self.assertLessEqual(f.mono, started + 2 * NS)
                    self.assertEqual(f.journal.header['producer'], original_producer)
                    final = guard_journal.read()
                    if outcome == 'still-live':
                        self.assertEqual(final['mode'], 'OPEN')
                        self.assertEqual(final['guard']['identity']['pid'], 112)
                        self.assertEqual(delete_actors, [])
                        self.assertTrue(cs.live(original_producer))
                        self.assertEqual(len(f.cloud.requests), 1)
                    else:
                        self.assertEqual(final['mode'], 'COMPLETE')
                        self.assertEqual(final['guard']['identity']['pid'], 113)
                        self.assertEqual(delete_actors, [113])
                        self.assertEqual([(method, path) for method, path, _, _ in f.cloud.requests],
                            [('POST', '/networks'), ('DELETE', '/networks/201')])
                        self.assertFalse(cs.live(original_producer))
                        if outcome == 'pid-reused':
                            self.assertTrue(cs.live(f.processes[111]))
                            self.assertEqual(f.processes[111]['starttime'], '1111')

    def test_guard_loop_confirms_term_and_kill_esrch_through_terminal_pidfd_before_proc_reaping(self):
        for boundary in ('term', 'kill'):
            with self.subTest(boundary=boundary), Fixture() as f, ProcFiles(f) as proc, \
                 contextlib.ExitStack() as stack:
                f.create()
                original_producer = copy.deepcopy(f.header['producer'])
                proc.remove(112)
                proc.add(113, 110, '1130', b'113')
                f.processes[113] = cs.identity(113)
                proc.actor(113)
                guard_journal = observer(f, stack)
                guard = cs.Campaign(guard_journal, f.cloud, clock=lambda: f.wall, monotonic=lambda: f.mono)
                read_fd, write_fd = os.pipe()
                stack.callback(os.close, read_fd)
                stack.callback(os.close, write_fd)
                owned, signals, waits, esrch, terminal, reaped, delete_actors = [], [], [], [], [], [], []
                started = f.mono
                original_close = os.close
                def pidfd_open(pid):
                    self.assertEqual(pid, 111)
                    self.assertTrue(cs.live(original_producer))
                    self.assertEqual(owned, [], 'terminal confirmation must not acquire a second pidfd')
                    f.mono += NS // 10
                    owned.append(os.dup(read_fd))
                    return owned[0]
                def sent(fd, sig):
                    self.assertEqual(fd, owned[0])
                    self.assertTrue(cs.live(original_producer))
                    self.assertEqual(terminal, [], 'no signal is allowed after terminal-pidfd confirmation')
                    signals.append(sig)
                    f.mono += NS // 10
                    if (boundary, sig) in (('term', signal.SIGTERM), ('kill', signal.SIGKILL)):
                        esrch.append(sig)
                        # The task is terminal, but its original /proc identity
                        # remains readable until the supervisor reaps it.
                        raise ProcessLookupError(errno.ESRCH, 'synthetic terminal task with unreaped proc metadata')
                def wait(readable, writable, exceptional, seconds):
                    self.assertEqual(readable, [owned[0]])
                    waits.append(seconds)
                    self.assertTrue(cs.live(original_producer), 'identity disappearance must not satisfy this confirmation')
                    self.assertLessEqual(seconds, max(0, (started + 2 * NS - f.mono) / NS))
                    if esrch:
                        self.assertEqual(seconds, 0, 'ESRCH confirmation must inspect the already-owned pidfd without waiting')
                        self.assertEqual(terminal, [])
                        terminal.append(owned[0])
                        return (readable, [], [])
                    f.mono += round(seconds * NS)
                    return ([], [], [])
                def close(fd):
                    if owned and fd == owned[0] and not reaped:
                        self.assertEqual(terminal, [fd], 'reaping must follow independent terminal-pidfd confirmation')
                        self.assertTrue(cs.live(original_producer), 'original identity must survive through confirmation')
                        original_close(fd)
                        proc.remove(111)
                        reaped.append(fd)
                    else:
                        original_close(fd)
                f.cloud.before_request = lambda method, path, body, seconds: (
                    delete_actors.append(cs.identity()['pid']) if method == 'DELETE' else None)
                with patch.object(cs.os, 'pidfd_open', side_effect=pidfd_open), \
                     patch.object(cs.signal, 'pidfd_send_signal', side_effect=sent), \
                     patch.object(cs.select, 'select', side_effect=wait), \
                     patch.object(cs.os, 'close', side_effect=close):
                    result = guard.guard_loop(sleep=lambda seconds: self.fail('confirmed terminal producer must proceed to cleanup'))
                expected_signals = [signal.SIGTERM] if boundary == 'term' else [signal.SIGTERM, signal.SIGKILL]
                self.assertEqual(signals, expected_signals)
                self.assertEqual(esrch, [expected_signals[-1]])
                self.assertEqual(len(owned), 1)
                self.assertEqual(terminal, owned)
                self.assertEqual(reaped, owned)
                self.assertEqual(waits, [0, 0] if boundary == 'term' else [0, 1, 0])
                self.assertEqual(f.mono - started, NS // 5 if boundary == 'term' else 13 * NS // 10)
                self.assertLessEqual(f.mono, started + 2 * NS)
                self.assertTrue(result['complete'])
                self.assertTrue(result['hard_lifetime_met'])
                final = guard_journal.read()
                self.assertEqual(final['mode'], 'COMPLETE')
                self.assertIsNone(final['active_lease'])
                self.assertEqual(final['guard']['identity']['pid'], 113)
                self.assertFalse(cs.live(original_producer))
                self.assertEqual(guard_journal.header['producer'], original_producer)
                self.assertEqual(delete_actors, [113])
                self.assertEqual([(method, path) for method, path, _, _ in f.cloud.requests],
                    [('POST', '/networks'), ('DELETE', '/networks/201')])


class CutoverReaderTests(unittest.TestCase):
    def test_public_primary_checker_cannot_block_guard_cutover_heartbeat_or_retry(self):
        import primary_ip_check as pic
        from test_guarded_initialize import Initialization
        from test_guarded_orchestration import RunningGuard, backend, cli_boundaries, execute

        with Initialization() as f:
            backend(f)
            journal = f.initialize()
            checker_held, release_checker, guard_progress, release_guard = (threading.Event() for _ in range(4))
            checker_result, checker_errors, delete_actors = [], [], []
            def guard_sleep(guard, seconds):
                self.assertEqual(seconds, 1)
                self.assertEqual(len(guard.sleeps), 2, 'guard did not retry through the one bounded lock refusal')
                guard_progress.set()
                if not release_guard.wait(60):
                    raise AssertionError('test did not release the guard cutover retry')
            with RunningGuard(f, sleep=guard_sleep) as guard:
                guard.release_on_exit.extend((release_checker, release_guard))
                def no_wait(seconds):
                    self.fail('healthy provisioning unexpectedly waited')
                provisioned = execute(f, journal, no_wait)
                self.assertTrue(provisioned['executed'])
                self.assertEqual(len(journal.read()['owned']), 7)
                self.assertEqual(len(f.cloud.requests), 16)
                f.processes[113] = dict(pid=113, parent=110, starttime='1130', argv_sha256=cc.digest(b'113'))
                args = pic.parse_args(['--label', 'campaign=synthetic-init', '--prefix', f.args.prefix,
                    '--role', 'node3', '--creation-ledger', str(f.ledger),
                    '--state', str(f.root / 'checked.json'), '--owned-output', str(f.root / 'checked-owned.json')])
                def before_list(kind, seconds):
                    if kind == 'primary-ip' and threading.current_thread().name == 'real-primary-checker':
                        checker_held.set()
                        if not release_checker.wait(60):
                            raise AssertionError('test did not release the actual primary-IP census')
                f.cloud.before_list = before_list
                f.cloud.before_request = lambda method, path, body, seconds: (
                    delete_actors.append(cs.identity()['pid']) if method == 'DELETE' else None)
                def check():
                    guard.local.actor = 113
                    try:
                        with cli_boundaries(f):
                            checker_result.append(pic.check(args))
                    except BaseException as error:
                        checker_errors.append(error)
                thread = threading.Thread(target=check, name='real-primary-checker', daemon=True)
                thread.start()
                try:
                    self.assertTrue(checker_held.wait(60), 'public checker did not reach its locked provider census')
                    observer_journal = cl.Journal(f.ledger, observe=True)
                    f.stack.callback(observer_journal.close)
                    self.assertIsNone(observer_journal.read_if_available())
                    self.assertEqual(observer_journal.peek()['mode'], 'OPEN')
                    before = json.loads(cc.private_bytes(f.work / 'guard' / 'heartbeat.json'))
                    f.processes.pop(111)
                    f.wall += NS
                    f.mono += NS
                    resumed_at = f.mono
                    guard.campaign.request_stop()
                    guard.resume.set()
                    self.assertTrue(guard_progress.wait(60), 'guard blocked behind a nonproducer checker lock')
                    self.assertFalse(guard.done.is_set())
                    self.assertTrue(thread.is_alive(), 'checker must still own the independent lock')
                    self.assertTrue(cs.live(f.processes[113]), 'guard must not terminate the checker')
                    self.assertEqual(f.mono, resumed_at, 'busy-lock refusal must not consume a new termination allowance')
                    self.assertIsNone(observer_journal.read_if_available())
                    self.assertEqual(observer_journal.peek()['mode'], 'OPEN')
                    self.assertEqual(delete_actors, [])
                    self.assertEqual(len(f.cloud.requests), 16)
                    progress = json.loads(cc.private_bytes(f.work / 'guard' / 'heartbeat.json'))
                    self.assertEqual(progress['identity']['pid'], 112)
                    self.assertGreater(progress['monotonic_ns'], before['monotonic_ns'])
                    self.assertEqual(progress['monotonic_ns'], resumed_at)
                    incident = json.loads(cc.private_bytes(f.work / 'guard' / 'cutover-incident.json'))
                    self.assertEqual(incident, dict(binding=journal.binding, event='cutover-lock-unavailable',
                        unix_ns=guard.campaign.clock(), hard_lifetime_met=False, provisioning_reopened=False))
                    release_checker.set()
                    thread.join(60)
                    self.assertFalse(thread.is_alive())
                    self.assertEqual(checker_errors, [])
                    self.assertEqual(checker_result, [([101, 102, 103, 104], [10101, 10102, 10103, 10104])])
                    self.assertEqual(delete_actors, [])
                    release_guard.set()
                    result = guard.wait_complete()
                    self.assertTrue(result['complete'])
                    self.assertTrue(result['hard_lifetime_met'])
                    self.assertEqual(delete_actors, [112] * 7)
                    self.assertEqual(len(f.cloud.requests), 23)
                    self.assertEqual(journal.read()['mode'], 'COMPLETE')
                    self.assertTrue(cs.live(f.processes[113]))
                    self.assertEqual(json.loads(cc.private_bytes(f.work / 'guard' / 'cleanup.json')), result)
                finally:
                    release_checker.set()
                    release_guard.set()
                    thread.join(60)
                    self.assertFalse(thread.is_alive(), 'public checker did not leave its provider boundary')


class PublicTakeoverTests(unittest.TestCase):
    def test_normal_and_immediate_parsed_takeover_quiesce_the_locked_producer_before_strict_reads(self):
        import teardown_guard as tg

        for immediate in (False, True):
            with self.subTest(immediate=immediate), Fixture() as f, ProcFiles(f) as proc, \
                 contextlib.ExitStack() as stack:
                watch = observer(f, stack)
                pause = PublicationPause(f, watch, 0, 'ACKNOWLEDGED')
                stack.enter_context(patch.object(cl.os, 'fsync', side_effect=pause.fsync))
                worker = Worker(proc, lambda: f.create())
                stack.callback(worker.release_and_join, pause)
                self.assertTrue(pause.entered.wait(5))
                current = watch.peek()
                self.assertIsNone(current['active_lease'])
                self.assertEqual(current['intents'][0]['state'], 'ACKNOWLEDGED')
                lease = copy.deepcopy(current['intents'][0]['send_lease'])
                original_ack = copy.deepcopy(current['intents'][0]['ack'])
                self.assertIsNone(watch.read_if_available())
                proc.remove(112)
                proc.add(113, 110, '1130', b'113')
                f.processes[113] = cs.identity(113)
                f.mono = lease['monotonic_deadline_ns']
                argv = ['--label', 'campaign=synthetic', '--prefix', 'test', '--creation-ledger', str(f.journal.path),
                    '--execute', '--request-wait-seconds', '2']
                if immediate:
                    argv.append('--immediate')
                args = tg.parse_args(argv)
                if immediate:
                    # Schedule the request deadline on the same fixture clock;
                    # real stop-file fsync latency is not part of this interleaving.
                    stack.enter_context(patch.object(tg.time, 'monotonic', side_effect=lambda: f.mono / NS))
                real_campaign = cs.Campaign
                def clocked(journal, cloud):
                    return real_campaign(journal, cloud, clock=lambda: f.wall, monotonic=lambda: f.mono)
                def cloud_factory(profile, binding):
                    self.assertEqual(profile.binding, binding)
                    self.assertEqual(binding, f.header['context'])
                    f.cloud.profile = profile
                    return f.cloud
                stack.enter_context(patch.object(cc, 'Cloud', side_effect=cloud_factory))
                stack.enter_context(patch.object(cs, 'Campaign', side_effect=clocked))
                original_flock = fcntl.flock
                def flock(fd, operation):
                    if (threading.current_thread().name == 'public-replacement' and
                            operation & fcntl.LOCK_EX and not operation & fcntl.LOCK_NB and not worker.done.is_set()):
                        raise AssertionError('replacement attempted a blocking journal lock before producer quiescence')
                    return original_flock(fd, operation)
                stack.enter_context(patch.object(fcntl, 'flock', side_effect=flock))
                read_fd, write_fd = os.pipe()
                stack.callback(os.close, read_fd)
                stack.callback(os.close, write_fd)
                opens, waits, signals, delete_actors = [], [], [], []
                def pidfd_open(pid):
                    self.assertEqual(pid, 111)
                    self.assertFalse(cs.live(current['guard']['identity']))
                    self.assertIsNone(watch.read_if_available(), 'producer lock must still be held when takeover starts quiescence')
                    opens.append(pid)
                    f.mono += NS // 10
                    return os.dup(read_fd)
                def sent(fd, sig):
                    signals.append(sig)
                    f.mono += NS // 10
                    if sig == signal.SIGKILL:
                        proc.remove(111)
                        pause.exit_process = True
                        pause.release.set()
                def wait(readable, writable, exceptional, seconds):
                    waits.append(seconds)
                    f.mono += round(seconds * NS)
                    if len(waits) == 3:
                        self.assertTrue(worker.done.wait(5))
                        return (readable, [], [])
                    return ([], [], [])
                stack.enter_context(patch.object(cs.os, 'pidfd_open', side_effect=pidfd_open))
                stack.enter_context(patch.object(cs.signal, 'pidfd_send_signal', side_effect=sent))
                stack.enter_context(patch.object(cs.select, 'select', side_effect=wait))
                f.cloud.before_request = lambda method, path, body, seconds: (
                    delete_actors.append(cs.identity()['pid']) if method == 'DELETE' else None)
                results, errors, done = [], [], threading.Event()
                def replace():
                    proc.actor(113)
                    try:
                        results.append(tg.run(args, out=io.StringIO(),
                            sleep=lambda seconds: self.fail('dead-guard takeover must not wait for the producer lock'),
                            runner=lambda *a, **kw: self.fail('guarded takeover must never invoke legacy commands')))
                    except BaseException as error:
                        errors.append(error)
                    finally:
                        done.set()
                thread = threading.Thread(target=replace, name='public-replacement', daemon=True)
                thread.start()
                self.assertTrue(done.wait(60), 'parsed teardown could not quiesce the lock-owning producer')
                thread.join(60)
                self.assertFalse(thread.is_alive())
                self.assertEqual(errors, [])
                worker.join(self)
                self.assertEqual(len(results), 1)
                ok, receipt = results[0]
                self.assertTrue(ok)
                self.assertTrue(receipt['result']['complete'])
                self.assertTrue(receipt['result']['hard_lifetime_met'])
                self.assertEqual(opens, [111])
                self.assertEqual(signals, [signal.SIGTERM, signal.SIGKILL])
                self.assertEqual(waits[:2], [0, 1])
                self.assertAlmostEqual(waits[2], 0.7)
                self.assertEqual(f.mono, lease['monotonic_deadline_ns'] + 2 * NS)
                final = watch.read()
                self.assertEqual(final['mode'], 'COMPLETE')
                self.assertEqual(final['guard']['identity']['pid'], 113)
                self.assertEqual(final['intents'][0]['ack'], original_ack)
                self.assertEqual(final['intents'][0]['send_lease'], lease)
                self.assertEqual(f.journal.header['producer'], f.header['producer'])
                self.assertEqual(delete_actors, [113])
                self.assertEqual([(method, path) for method, path, _, _ in f.cloud.requests],
                    [('POST', '/networks'), ('DELETE', '/networks/201')])
                self.assertEqual(len(worker.errors), 1)
                self.assertIsInstance(worker.errors[0], ProcessExit)
                heartbeat = json.loads(cc.private_bytes(f.guard_directory / 'heartbeat.json'))
                self.assertEqual(heartbeat['identity'], final['guard']['identity'])
                self.assertEqual(heartbeat['challenge'], final['guard']['challenge'])


class RegistrationTests(unittest.TestCase):
    def test_two_stale_none_observations_cannot_register_two_live_guards(self):
        with Fixture(arm=False) as f, ProcFiles(f) as proc, contextlib.ExitStack() as stack:
            proc.add(113, 110, '1130', b'113')
            f.processes[113] = cs.identity(113)
            journals = {pid: observer(f, stack) for pid in (112, 113)}
            views = {pid: observer(f, stack) for pid in (112, 113)}
            guards = {pid: cs.Campaign(journals[pid], f.cloud, clock=lambda: f.wall,
                monotonic=lambda: f.mono) for pid in (112, 113)}
            barrier = threading.Barrier(2)
            winner_done = threading.Event()
            winner_snapshot, loser_locks, returned = {}, [], set()
            seen, acquired, stale, results, errors = set(), set(), {}, {}, {}
            observation_gate = threading.Lock()
            original = fcntl.flock
            def flock(fd, operation):
                name = threading.current_thread().name
                pid = int(name.split('-')[1]) if name.startswith('register-') else None
                if pid is not None and operation & fcntl.LOCK_EX and pid not in acquired:
                    # Give each initial peek its real flock before rendezvous;
                    # otherwise a busy observational peek has no unlock to pause.
                    observation_gate.acquire()
                    acquired.add(pid)
                result = original(fd, operation)
                if pid == 113 and operation & fcntl.LOCK_EX and pid in returned:
                    self.assertTrue(winner_done.is_set())
                    loser_locks.append(operation)
                if operation == fcntl.LOCK_UN and pid is not None and pid not in seen:
                    seen.add(pid)
                    stale[pid] = views[pid].peek()['guard']
                    observation_gate.release()
                    barrier.wait(60)
                    if pid == 113:
                        # This unlock has already happened. Hold no journal
                        # lock while the winner completes registration AND HB.
                        if not winner_done.wait(60):
                            raise AssertionError('winner did not finish registration and heartbeat publication')
                    returned.add(pid)
                return result
            stack.enter_context(patch.object(fcntl, 'flock', side_effect=flock))
            def arm(pid):
                proc.actor(pid)
                try:
                    results[pid] = guards[pid].arm_guard()
                    if pid == 112:
                        winner_snapshot['state'] = views[pid].read()
                        winner_snapshot['checkpoint'] = copy.deepcopy(views[pid].checkpoint)
                        winner_snapshot['heartbeat'] = cc.private_bytes(f.guard_directory / 'heartbeat.json')
                except BaseException as error:
                    errors[pid] = error
                finally:
                    if pid == 112:
                        winner_done.set()
            threads = [threading.Thread(target=arm, args=(pid,), name=f'register-{pid}', daemon=True)
                       for pid in (112, 113)]
            for thread in threads:
                thread.start()
            for thread in threads:
                thread.join(60)
                self.assertFalse(thread.is_alive(), 'registration contender did not finish')
            self.assertEqual(stale, {112: None, 113: None})
            self.assertEqual(set(results), {112}, 'the stale loser must not overwrite the completed live winner')
            self.assertEqual(set(errors), {113})
            self.assertEqual(loser_locks, [fcntl.LOCK_EX | fcntl.LOCK_NB],
                'the loser must actually acquire its registration lock, not merely encounter contention')
            self.assertIsInstance(errors[113], mc.GuardError)
            self.assertEqual(str(errors[113]), 'another guard is still live or registration changed')
            current = f.journal.read()
            self.assertEqual(current, winner_snapshot['state'])
            self.assertEqual(f.journal.checkpoint, winner_snapshot['checkpoint'])
            self.assertEqual(current['guard'], results[112])
            self.assertEqual(current['guard']['identity']['pid'], 112)
            raw = cc.private_bytes(f.guard_directory / 'heartbeat.json')
            self.assertEqual(raw, winner_snapshot['heartbeat'])
            receipt = json.loads(raw)
            self.assertEqual(receipt['identity'], current['guard']['identity'])
            self.assertEqual(receipt['challenge'], current['guard']['challenge'])
            self.assertEqual(f.cloud.requests, [])

    def test_registration_lock_contention_is_a_separate_exact_refusal_without_publication(self):
        with Fixture(arm=False) as f, ProcFiles(f) as proc, contextlib.ExitStack() as stack:
            proc.add(113, 110, '1130', b'113')
            f.processes[113] = cs.identity(113)
            guard_journal, holder_journal, view = (observer(f, stack) for _ in range(3))
            guard = cs.Campaign(guard_journal, f.cloud, clock=lambda: f.wall, monotonic=lambda: f.mono)
            initial = view.read()
            checkpoint = copy.deepcopy(view.checkpoint)
            head = f.journal.path.read_bytes()
            names = sorted(path.name for path in f.journal.path.parent.iterdir())
            observed, held, release, done = (threading.Event() for _ in range(4))
            paused, stale, results, errors = [], [], [], {}
            original = fcntl.flock
            def flock(fd, operation):
                result = original(fd, operation)
                if (threading.current_thread().name == 'contention-registrant'
                        and operation == fcntl.LOCK_UN and not paused):
                    paused.append(True)
                    stale.append(view.peek()['guard'])
                    observed.set()
                    if not held.wait(60):
                        raise AssertionError('independent actor did not acquire the actual registration lock')
                return result
            stack.enter_context(patch.object(fcntl, 'flock', side_effect=flock))
            def holder():
                proc.actor(113)
                try:
                    if not observed.wait(60):
                        raise AssertionError('guard did not finish its initial None observation')
                    with holder_journal.locked() as state:
                        self.assertIsNone(state['guard'])
                        held.set()
                        if not release.wait(60):
                            raise AssertionError('test did not release registration lock contention')
                except BaseException as error:
                    errors['holder'] = error
            def register():
                proc.actor(112)
                try:
                    results.append(guard.arm_guard())
                except BaseException as error:
                    errors['guard'] = error
                finally:
                    done.set()
            holding = threading.Thread(target=holder, name='registration-lock-holder', daemon=True)
            registering = threading.Thread(target=register, name='contention-registrant', daemon=True)
            holding.start()
            registering.start()
            try:
                self.assertTrue(done.wait(60), 'contended registration attempted a blocking lock')
                self.assertTrue(held.is_set())
                self.assertTrue(holding.is_alive(), 'registration must be refused while the independent lock is still held')
                self.assertEqual(stale, [None])
                self.assertEqual(results, [])
                self.assertEqual(set(errors), {'guard'})
                self.assertIsInstance(errors['guard'], mc.GuardError)
                self.assertEqual(str(errors['guard']), 'guard registration lock unavailable; another guard may be live')
                self.assertIsNone(view.read_if_available())
                self.assertEqual(view.peek(), initial)
                self.assertEqual(view.checkpoint, checkpoint)
                self.assertEqual(f.journal.path.read_bytes(), head)
                self.assertEqual(sorted(path.name for path in f.journal.path.parent.iterdir()), names)
                self.assertFalse((f.guard_directory / 'heartbeat.json').exists())
                self.assertFalse((f.guard_directory / 'stop.json').exists())
                self.assertEqual(f.cloud.requests, [])
            finally:
                observed.set()
                release.set()
                holding.join(60)
                registering.join(60)
                self.assertFalse(holding.is_alive())
                self.assertFalse(registering.is_alive())
            self.assertEqual(set(errors), {'guard'})
            self.assertEqual(view.read(), initial)


class ProcIdentityTests(unittest.TestCase):
    def test_parenthesized_comm_exact_starttime_and_raw_cmdline_digest_feed_real_create(self):
        with Fixture(producer_parent=113) as f, ProcFiles(f) as proc:
            proc.add(113, 114, '1130', b'intermediate\x00--child\x00')
            proc.add(114, 110, '1140', b'other-ancestor\x00')
            proc.actor(111)
            self.assertEqual(cs.identity(), dict(pid=111, parent=113, starttime='1110', argv_sha256=cc.digest(b'111')))
            self.assertEqual(cs.identity(113), dict(pid=113, parent=114, starttime='1130',
                argv_sha256=cc.digest(b'intermediate\x00--child\x00')))
            self.assertTrue(cs.live(f.header['producer']))
            self.assertTrue(cs.supervised(f.header['producer'], f.header['supervisor']))
            self.assertEqual(f.create(), 201)
            self.assertEqual(len(f.cloud.requests), 1)
            self.assertEqual(f.journal.read()['intents'][0]['producer'], f.header['producer'])

    def test_proc_changes_and_missing_or_malformed_ancestors_refuse_real_create(self):
        for fault in ('starttime', 'cmdline', 'stat-missing', 'cmdline-missing', 'stat-malformed',
                      'parent-malformed', 'ancestor-missing', 'ancestor-malformed', 'ancestor-reparented', 'ancestor-cycle'):
            with self.subTest(fault=fault), Fixture(producer_parent=113) as f, ProcFiles(f) as proc:
                proc.add(113, 110, '1130', b'intermediate\x00')
                proc.actor(111)
                self.assertTrue(cs.supervised(f.header['producer'], f.header['supervisor']))
                if fault == 'starttime': proc.add(111, 113, '1111', b'111')
                if fault == 'cmdline': proc.files['/proc/111/cmdline'] = b'111\x00--changed\x00'
                if fault == 'stat-missing': proc.files.pop('/proc/111/stat')
                if fault == 'cmdline-missing': proc.files.pop('/proc/111/cmdline')
                if fault == 'stat-malformed': proc.files['/proc/111/stat'] = '111 no comm delimiter'
                if fault == 'parent-malformed': proc.files['/proc/111/stat'] = '111 (comm) S not-a-pid'
                if fault == 'ancestor-missing': proc.files.pop('/proc/113/stat')
                if fault == 'ancestor-malformed': proc.files['/proc/113/stat'] = '113 (comm) S'
                if fault == 'ancestor-reparented': proc.add(113, 1, '1130', b'intermediate\x00')
                if fault == 'ancestor-cycle': proc.add(113, 113, '1130', b'intermediate\x00')
                self.assertFalse(cs.supervised(f.header['producer'], f.header['supervisor']))
                with self.assertRaises((mc.GuardError, OSError, ValueError, IndexError)):
                    f.create()
                self.assertEqual(f.cloud.requests, [])
                self.assertEqual(f.journal.read()['intents'], [])


class CleanupBoundaryTests(unittest.TestCase):
    def test_real_cleanup_readback_and_delete_at_shortened_reserve_and_incident_boundaries(self):
        for before_h, allowance, incident in ((22 * NS, 20, False), (7 * NS, 5, False),
                (2 * NS + 1, 1 / NS, False), (2 * NS, 20, True),
                (2 * NS - 1, 20, True), (-1, 20, True)):
            with self.subTest(before_h=before_h), Fixture() as f, ProcFiles(f) as proc:
                rid = f.create()
                proc.remove(111)
                f.closing()
                proc.actor(112)
                policy = copy.deepcopy(f.header['time'])
                now = policy['h_ns'] - before_h
                set_time(f, now)
                f.cloud.describes.clear()
                sent = []
                def inspect(method, path, body, seconds):
                    current = f.journal.peek()
                    self.assertEqual((method, path), ('DELETE', '/networks/201'))
                    self.assertAlmostEqual(seconds, allowance)
                    self.assertEqual(current['mode'], 'INCIDENT' if incident else 'CLOSING')
                    self.assertEqual(current['active_lease']['deadline_ns'], now + round(allowance * NS))
                    self.assertEqual(current['active_lease']['actor'], f.processes[112])
                    self.assertEqual(current['intents'][-1]['state'], 'SENDING')
                    sent.append(True)
                f.cloud.before_request = inspect
                f.campaign.mutate('network', rid, 'delete')
                self.assertEqual(sent, [True])
                self.assertEqual(f.cloud.describes, [('network', rid, allowance)])
                final = f.journal.read()
                self.assertEqual(final['mode'], 'INCIDENT' if incident else 'CLOSING')
                self.assertIsNone(final['active_lease'])
                self.assertEqual(final['intents'][-1]['state'], 'ACKNOWLEDGED')
                events = [e for e in final['events'] if e['event'] == 'cleanup-reserve-exhausted']
                self.assertEqual(bool(events), incident)
                for event in events:
                    self.assertEqual(event['unix_ns'], now)
                    self.assertIs(event['hard_lifetime_met'], False)
                    self.assertIs(event['provisioning_reopened'], False)
                self.assertEqual(f.journal.header['time'], policy)
                self.assertEqual(policy['h_ns'], T0 + 16 * 3600 * NS)
                self.assertTrue(f.campaign.poll_actions())
                result = f.campaign.completion()
                self.assertTrue(result['complete'])
                self.assertEqual(result['hard_lifetime_met'], now <= policy['h_ns'])
                self.assertEqual(f.journal.header['time'], policy)
                self.assertEqual(f.deletes(), [('DELETE', '/networks/201')])


if __name__ == '__main__':
    unittest.main()
