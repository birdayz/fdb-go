"""Real Campaign/journal controls with synthetic provider, processes and clocks."""
import contextlib
import copy
import datetime as dt
import json
import os
from pathlib import Path
import signal
import tempfile
import unittest
from unittest.mock import patch

import campaign_cloud as cc
import campaign_safety as cs
import creation_ledger as cl
import mn_common as mc

NS = 1_000_000_000
T0 = 1_700_000_000 * NS
M0 = 500 * NS
INVENTORY_KINDS = ('server', 'network', 'firewall', 'ssh-key', 'volume', 'primary-ip',
                   'floating-ip', 'load-balancer', 'placement-group', 'certificate')


def utc(nanoseconds):
    return dt.datetime.fromtimestamp(nanoseconds // NS, dt.timezone.utc).isoformat().replace('+00:00', 'Z')


class Provider:
    """Only the provider boundary is synthetic; Campaign validation stays real."""
    def __init__(self, fixture, profile):
        self.fixture, self.profile = fixture, profile
        self.rows = {kind: {9000 + index: dict(id=9000 + index, name='baseline-' + kind)}
                     for index, kind in enumerate(INVENTORY_KINDS)}
        self.next_ids = {'server': 101, 'network': 201, 'firewall': 301, 'ssh-key': 401}
        self.requests, self.lists, self.describes, self.action_reads = [], [], [], []
        self.actions = {}
        self.next_action = 1001
        self.before_request = self.before_action = self.before_list = None
        self.lose_ack = self.delay_create = False
        self.pending = []
        self.fail_list = None

    def list(self, kind, seconds=20):
        seconds = float(seconds() if callable(seconds) else seconds)
        self.profile.revalidate(self.fixture.header['context'])
        self.lists.append((kind, seconds))
        if self.before_list:
            self.before_list(kind, seconds)
        if kind == self.fail_list:
            raise mc.GuardError('synthetic provider list unavailable')
        return copy.deepcopy(list(self.rows[kind].values()))

    def describe(self, kind, rid, seconds=20):
        seconds = float(seconds() if callable(seconds) else seconds)
        self.profile.revalidate(self.fixture.header['context'])
        self.describes.append((kind, rid, seconds))
        return copy.deepcopy(self.rows[kind].get(rid))

    def action(self, rid, seconds=20):
        seconds = float(seconds() if callable(seconds) else seconds)
        self.profile.revalidate(self.fixture.header['context'])
        self.action_reads.append(rid)
        if self.before_action:
            self.before_action(rid)
        return copy.deepcopy(self.actions[rid])

    def resource_actions(self, kind, rid, seconds=20, on_page=None):
        seconds = float(seconds() if callable(seconds) else seconds)
        self.profile.revalidate(self.fixture.header['context'])
        rows = [copy.deepcopy(action) for action in self.actions.values()
                if {'type': kind.replace('-', '_'), 'id': rid} in action['resources']]
        if on_page is not None:
            on_page(rows)
        return rows

    def new_action(self, kind, rid, command):
        action = dict(id=self.next_action, command=command, status='success',
                      resources=[dict(type=kind.replace('-', '_'), id=rid)])
        self.actions[action['id']] = copy.deepcopy(action)
        self.next_action += 1
        return action

    def materialize(self, kind, body):
        rid = self.next_ids[kind]
        self.next_ids[kind] += 1
        item = dict(id=rid, name=body['name'], labels=copy.deepcopy(body['labels']), created=utc(self.fixture.wall))
        if kind == 'server':
            ip = rid + 10_000
            item.update(server_type=dict(name=body['server_type']), location=dict(name=body['location']),
                        image=dict(id=body['image']), volumes=[], backup_window=None,
                        public_net=dict(ipv4=dict(id=ip, ip='203.0.113.10'), ipv6=None,
                                        firewalls=[dict(id=f['firewall']) for f in body['firewalls']], floating_ips=[]))
            self.rows['primary-ip'][ip] = dict(id=ip, name=body['name'] + '-ipv4', type='ipv4', assignee_type='server', assignee_id=rid,
                                              auto_delete=True, created=item['created'])
        elif kind == 'network':
            item['ip_range'] = body['ip_range']
        elif kind == 'firewall':
            item['rules'] = copy.deepcopy(body['rules'])
        else:
            item['public_key'] = body['public_key']
        self.rows[kind][rid] = item
        payload = {cs.resource_key(kind): copy.deepcopy(item)}
        if kind == 'server':
            payload['action'] = self.new_action(kind, rid, 'create_server')
            payload['next_actions'] = []
        elif kind == 'firewall':
            payload['actions'] = [self.new_action(kind, rid, 'create_firewall')]
        return payload

    def finish_pending(self):
        for kind, body in self.pending:
            self.materialize(kind, body)
        self.pending.clear()

    def request(self, method, path, body=None, seconds=20):
        seconds = float(seconds() if callable(seconds) else seconds)
        self.profile.revalidate(self.fixture.header['context'])
        cc.need(cc.mutation_allowed(method, path, body), 'synthetic provider received unplanned wire operation')
        self.requests.append((method, path, copy.deepcopy(body), seconds))
        if self.before_request:
            self.before_request(method, path, body, seconds)
        parts = path.strip('/').split('/')
        kind = next(k for k, plural in cc.PLURALS.items() if plural == parts[0])
        if method == 'POST' and len(parts) == 1:
            if self.delay_create:
                self.pending.append((kind, copy.deepcopy(body)))
                raise mc.GuardError('synthetic response lost before visibility')
            payload = self.materialize(kind, body)
            if self.lose_ack:
                raise mc.GuardError('synthetic response lost after commit')
            status = 201
        elif method == 'DELETE':
            rid = int(parts[1])
            self.rows[kind].pop(rid)
            if kind == 'server':
                for ip in list(self.rows['primary-ip'].values()):
                    if ip.get('assignee_id') == rid and ip.get('auto_delete') is True:
                        self.rows['primary-ip'].pop(ip['id'])
            if kind == 'server':
                payload, status = {'action': self.new_action(kind, rid, 'delete_' + kind)}, 200
            else:
                payload, status = None, 204
        else:
            rid = int(parts[1])
            payload, status = {'action': self.new_action(kind, rid, parts[-1])}, 201
        return dict(status=status, payload=payload, json_valid=True, body_empty=payload is None,
                    body_sha256=cc.digest(b'' if payload is None else cc.wire_json(payload)),
                    request_sha256=cc.digest(cc.wire_json(body) if body is not None else b''), provider_date=utc(self.fixture.wall))


class Fixture:
    def __init__(self, producer_parent=110, arm=True):
        self.stack = contextlib.ExitStack()
        self.root = Path(self.stack.enter_context(tempfile.TemporaryDirectory()))
        self.source = self.root / 'sources'
        self.source.mkdir()
        for name in cs.SOURCE_NAMES:
            (self.source / name).write_text('# synthetic pinned source: ' + name + '\n')
        self.stack.enter_context(patch.object(cs, 'SOURCE_DIR', self.source))
        self.wall, self.mono, self.actor = T0 + 10 * NS, M0, 111
        self.processes = {pid: dict(pid=pid, parent=parent, starttime=str(pid * 10),
                                   argv_sha256=cc.digest(str(pid).encode()))
                          for pid, parent in ((110, 1), (111, producer_parent), (112, 110))}
        def identity(pid=None):
            pid = self.actor if pid is None else pid
            if pid not in self.processes:
                raise ProcessLookupError('synthetic process is gone')
            return copy.deepcopy(self.processes[pid])
        self.stack.enter_context(patch.object(cs, 'identity', side_effect=identity))
        self.guard_directory = self.root / 'guard'
        self.guard_directory.mkdir(mode=0o700)
        self.config = self.root / 'cloud.toml'
        self.config.write_text('active_context="synthetic"\n[[contexts]]\nname="synthetic"\ntoken="synthetic-controller-token"\n')
        self.config.chmod(0o600)
        profile = cc.Profile(config=self.config, environ={})
        self.userdata = self.root / 'user-data'
        self.userdata.write_text('# synthetic private deployment input\n')
        self.userdata.chmod(0o600)
        self.bodies = [
            ('network', dict(name='test-network', ip_range='10.42.0.0/24', labels={'campaign': 'synthetic', 'deadline': str((T0 + 15 * 3600 * NS) // NS)})),
            ('firewall', dict(name='test-firewall', rules=[], labels={'campaign': 'synthetic', 'deadline': str((T0 + 15 * 3600 * NS) // NS)})),
            ('ssh-key', dict(name='test-ssh-key', public_key='ssh-ed25519 SYNTHETIC test', labels={'campaign': 'synthetic', 'deadline': str((T0 + 15 * 3600 * NS) // NS)})),
        ]
        for index in range(4):
            self.bodies.append(('server', dict(name=f'test-node{index}', server_type='ccx43', location='fsn1', image=161547269,
                ssh_keys=[401], firewalls=[{'firewall': 301}], start_after_create=False,
                public_net=dict(enable_ipv4=True, enable_ipv6=False), user_data=self.userdata.read_text(),
                labels={'campaign': 'synthetic', 'deadline': str((T0 + 15 * 3600 * NS) // NS), 'role': f'node{index}'})))
        for kind, body in self.bodies:
            body['labels']['deadline'] = '1700054000'
            body['labels'].setdefault('role', kind)
        plan = []
        for index, (kind, body) in enumerate(self.bodies):
            planned = cs.descriptor(body)
            if kind == 'server':
                planned['ssh_keys'] = ['test-ssh-key']
                planned['firewalls'] = [{'firewall': 'test-firewall'}]
            plan.append(dict(kind=kind, name=body['name'], descriptor=planned, private_ip=f'10.42.0.{index + 10}'))
        self.header = dict(campaign='synthetic', prefix='test', boot_id=Path('/proc/sys/kernel/random/boot_id').read_text().strip(),
            sources=cs.source_pins(), baseline={kind: [9000 + i] for i, kind in enumerate(INVENTORY_KINDS)},
            time=dict(t0_ns=T0, c_ns=T0 + 1800 * NS, g_ns=T0 + 15 * 3600 * NS, h_ns=T0 + 16 * 3600 * NS,
                      setup_ns=T0 + 10800 * NS, monotonic_t0_ns=M0),
            producer=copy.deepcopy(self.processes[111]), supervisor=copy.deepcopy(self.processes[110]),
            context=copy.deepcopy(profile.binding), threat_model=cs.THREAT_MODEL, graph_reserved=True,
            private_inputs={str(self.userdata): cc.digest(self.userdata.read_bytes())},
            guard_directory=cs.private_directory(self.guard_directory),
            plan=plan,
            subnet=dict(type='cloud', network_zone='eu-central', ip_range='10.42.0.0/24'))
        self.journal = cl.Journal.initialize(self.root / 'journal' / 'head.json', self.header,
            dict(mode='OPEN', intents=[], owned=[], associations=[], events=[], guard=None, active_lease=None))
        self.stack.callback(self.journal.close)
        self.initial_checkpoint = self.journal.checkpoint
        self.cloud = Provider(self, profile)
        self.campaign = cs.Campaign(self.journal, self.cloud, clock=lambda: self.wall, monotonic=lambda: self.mono)
        self.challenge = None
        if arm:
            self.actor = 112
            self.challenge = self.campaign.arm_guard()['challenge']
            self.actor = 111
            self.heartbeat()

    def __enter__(self):
        return self

    def __exit__(self, *args):
        self.stack.__exit__(*args)

    def close(self):
        self.stack.close()

    def heartbeat(self, **changes):
        value = dict(identity=copy.deepcopy(self.processes[112]), binding=self.journal.binding,
                     challenge=self.challenge, checkpoint=self.initial_checkpoint, monotonic_ns=self.mono)
        value.update(changes)
        cs.atomic_private(self.guard_directory / 'heartbeat.json', value)

    def body(self, kind='network', index=0):
        return copy.deepcopy([body for candidate, body in self.bodies if candidate == kind][index])

    def dependencies(self):
        known = {r['kind'] for r in self.journal.read()['owned']}
        for kind in ('firewall', 'ssh-key'):
            if kind not in known:
                self.campaign.create(kind, self.body(kind))

    def create(self, kind='network', index=0):
        if kind == 'server':
            self.dependencies()
        return self.campaign.create(kind, self.body(kind, index))

    def create_all(self):
        return [(kind, self.campaign.create(kind, copy.deepcopy(body))) for kind, body in self.bodies]

    def closing(self, mode='CLOSING'):
        self.actor = 112
        self.processes.pop(111, None)
        self.campaign.close_admission()
        if mode != 'CLOSING':
            with self.journal.locked() as state:
                state['mode'] = mode
                self.journal.commit(state)

    def deletes(self):
        return [(method, path) for method, path, _, _ in self.cloud.requests if method == 'DELETE']


class CampaignTests(unittest.TestCase):
    def setUp(self):
        self.f = Fixture()
        self.addCleanup(self.f.close)

    def test_all_seven_creates_send_once_after_durable_sending_and_ack_before_action_wait(self):
        f = self.f
        observed = []
        def before_request(method, path, body, seconds):
            current = f.journal.peek()
            intent = current['intents'][-1]
            self.assertEqual((method, intent['state']), ('POST', 'SENDING'))
            self.assertEqual(current['active_lease']['intent'], intent['sequence'])
            self.assertEqual(current['active_lease']['actor'], f.header['producer'])
            self.assertEqual(current['active_lease']['deadline_ns'], T0 + 30 * NS)
            self.assertEqual(intent['request_sha256'], cc.digest(cc.wire_json(body)))
            self.assertEqual(intent['descriptor'], cs.descriptor(body))
            self.assertEqual(set(intent['pre_ids']), set(INVENTORY_KINDS))
            self.assertEqual(intent['nonce'], body['labels']['intent'])
            self.assertEqual(len(intent['nonce']), 32)
            self.assertEqual(seconds, 20)
            observed.append(intent['sequence'])
        f.cloud.before_request = before_request
        self.assertEqual(tuple(mc.PROTECT_KINDS), INVENTORY_KINDS)
        self.assertEqual(set(f.header['sources']), {str(f.source / name) for name in
            ('provision.py', 'teardown_guard.py', 'primary_ip_check.py', 'mn_common.py',
             'inventory.py', 'campaign_safety.py', 'campaign_cloud.py', 'creation_ledger.py')})
        ids = f.create_all()
        self.assertEqual(ids, [('network', 201), ('firewall', 301), ('ssh-key', 401),
                               ('server', 101), ('server', 102), ('server', 103), ('server', 104)])
        self.assertEqual(observed, list(range(7)))
        state = f.journal.read()
        self.assertEqual(len(f.cloud.requests), 7)
        self.assertEqual(len(state['owned']), 7)
        self.assertEqual({(r['kind'], r['id']) for r in state['owned']}, set(ids))
        self.assertTrue(all(i['state'] == 'ACKNOWLEDGED' and i['ack']['status'] == 201 for i in state['intents']))
        self.assertEqual(len({i['nonce'] for i in state['intents']}), 7)
        self.assertIsNone(state['active_lease'])
        self.assertEqual(f.cloud.action_reads, [])
        def before_action(rid):
            durable = f.journal.peek()
            owning = [i for i in durable['intents'] if rid in i['actions']]
            self.assertEqual(len(owning), 1)
            self.assertEqual(owning[0]['state'], 'ACKNOWLEDGED')
            self.assertIsNotNone(owning[0]['ack'])
        f.cloud.before_action = before_action
        self.assertTrue(f.campaign.poll_actions())
        self.assertTrue(all(i['state'] == 'DONE' for i in f.journal.read()['intents']))
        text = ''.join(path.read_text(errors='replace') for path in f.journal.path.parent.iterdir()
                       if path.name != 'integrity.key')
        self.assertNotIn('synthetic-controller-token', text)
        self.assertNotIn(f.userdata.read_text(), text)

    def test_duplicate_sealed_name_is_not_a_second_create(self):
        f = self.f
        self.assertEqual(f.create(), 201)
        with self.assertRaisesRegex(mc.GuardError, 'cannot be retried or replaced'):
            f.create()
        self.assertEqual(len(f.cloud.requests), 1)
        self.assertEqual(len(f.journal.read()['intents']), 1)

    def test_unknown_outcome_preserves_pending_then_recovers_exact_nonce_without_retry(self):
        f = self.f
        f.cloud.lose_ack = True
        with self.assertRaisesRegex(mc.GuardError, 'failed or ambiguous'):
            f.create()
        first = f.journal.read()
        self.assertEqual((first['mode'], first['intents'][0]['state']), ('OPEN', 'UNKNOWN'))
        self.assertTrue((f.guard_directory / 'stop.json').exists())
        self.assertEqual(first['owned'], [])
        self.assertIsNone(first['intents'][0]['ack'])
        with self.assertRaisesRegex(mc.GuardError, 'cleanup requested'):
            f.create()
        f.closing()
        f.campaign.recover()
        recovered = f.journal.read()
        self.assertEqual([(r['kind'], r['id']) for r in recovered['owned']], [('network', 201)])
        intent = recovered['intents'][0]
        self.assertTrue(intent['recovered'])
        self.assertIsNone(intent['original_ack'])
        self.assertEqual(intent['recovery']['origin'], 'conditional-nonce-recovery')
        self.assertEqual(intent['recovery']['candidate']['id'], 201)
        self.assertTrue(f.campaign.poll_actions())
        self.assertEqual(len(f.cloud.requests), 1)

    def test_zero_census_remains_pending_and_late_materialization_is_not_a_new_post(self):
        f = self.f
        f.cloud.delay_create = True
        with self.assertRaisesRegex(mc.GuardError, 'failed or ambiguous'):
            f.create()
        f.closing()
        f.campaign.recover()
        empty = f.campaign.completion()
        self.assertTrue(empty['baseline_equal'])
        self.assertFalse(empty['complete'])
        self.assertEqual(empty['pending_intents'], [0])
        self.assertEqual(f.journal.read()['owned'], [])
        f.cloud.finish_pending()
        f.campaign.recover()
        self.assertEqual(f.journal.read()['owned'][0]['id'], 201)
        self.assertEqual(len(f.cloud.requests), 1)

    def test_multiple_matching_recovery_candidates_are_not_adopted(self):
        f = self.f
        f.cloud.lose_ack = True
        with self.assertRaises(mc.GuardError): f.create()
        other = copy.deepcopy(f.cloud.rows['network'][201])
        other['id'] = 202
        f.cloud.rows['network'][202] = other
        f.campaign.recover()
        state = f.journal.read()
        self.assertEqual(state['owned'], [])
        self.assertEqual(state['intents'][0]['state'], 'UNKNOWN')
        event = [e for e in state['events'] if e['event'] == 'recovery-census'][-1]
        self.assertEqual(event['candidates'], [201, 202])
        self.assertEqual(len(f.cloud.requests), 1)

    def test_recovery_refuses_wrong_nonce_canonical_malformed_metadata_pre_call_and_baseline_ids(self):
        for fault in ('nonce', 'canonical', 'missing-created', 'malformed-created', 'pre-call', 'baseline'):
            with self.subTest(fault=fault), Fixture() as f:
                rid = 201
                if fault == 'pre-call':
                    f.cloud.rows['network'][rid] = dict(id=rid, name='unrelated-concurrent-network')
                if fault == 'baseline':
                    rid = f.header['baseline']['network'][0]
                    f.cloud.rows['network'].pop(rid)
                    f.cloud.next_ids['network'] = rid
                f.cloud.lose_ack = True
                with self.assertRaises(mc.GuardError): f.create()
                item = f.cloud.rows['network'][rid]
                if fault == 'nonce': item['labels']['intent'] = 'not-the-issued-nonce'
                if fault == 'canonical': item['ip_range'] = '10.99.0.0/24'
                if fault == 'missing-created': item.pop('created')
                if fault == 'malformed-created': item['created'] = 'not-a-timestamp'
                f.campaign.recover()
                current = f.journal.read()
                self.assertEqual(current['owned'], [])
                self.assertEqual(current['intents'][0]['state'], 'UNKNOWN')
                census = [e for e in current['events'] if e['event'] == 'recovery-census'][-1]
                self.assertEqual(census['candidates'], [])
                if fault in {'missing-created', 'malformed-created'}:
                    self.assertIn(dict(event='recovery-candidate-unavailable', kind='network', id=rid,
                                       error_type='GuardError'), current['events'])
                    self.assertEqual(census['inconclusive_candidates'], [rid])
                elif fault != 'nonce':
                    self.assertIn(dict(event='recovery-candidate-refused', kind='network', id=rid), current['events'])
                self.assertEqual(len(f.cloud.requests), 1)

    def test_server_recovery_needs_corresponding_create_action_not_just_known_id(self):
        f = self.f
        f.dependencies()
        f.cloud.lose_ack = True
        with self.assertRaises(mc.GuardError): f.create('server')
        action = next(a for a in f.cloud.actions.values() if a['command'] == 'create_server')
        action['resources'] = [dict(type='server', id=999)]
        f.campaign.recover()
        known = f.journal.read()
        self.assertEqual([r['id'] for r in known['owned'] if r['kind'] == 'server'], [101])
        self.assertEqual(known['intents'][-1]['state'], 'UNKNOWN')
        f.closing()
        self.assertFalse(f.campaign.completion()['complete'])
        self.assertEqual(len(f.cloud.requests), 3)

    def test_source_private_input_and_profile_pins_are_rechecked_before_intent(self):
        for fault, reason in [('source', 'pinned guard source changed'), ('source-link', 'pinned guard source changed'),
                              ('input', 'pinned private campaign input changed'), ('profile', 'sealed cloud profile changed')]:
            with self.subTest(fault=fault), Fixture() as f:
                if fault == 'source': (f.source / cs.SOURCE_NAMES[0]).write_text('changed source')
                if fault == 'source-link':
                    target = f.source / cs.SOURCE_NAMES[0]
                    data = target.read_bytes()
                    target.unlink()
                    elsewhere = f.root / 'same-source'
                    elsewhere.write_bytes(data)
                    target.symlink_to(elsewhere)
                if fault == 'input': f.userdata.write_text('changed private input')
                if fault == 'profile':
                    f.config.write_text(f.config.read_text().replace('synthetic-controller-token', 'changed-synthetic-token'))
                with self.assertRaisesRegex(mc.GuardError, reason): f.create()
                self.assertEqual(f.cloud.requests, [])
                self.assertEqual(f.journal.read()['intents'], [])

    def test_guard_process_heartbeat_challenge_and_ancestry_each_block_create(self):
        faults = [('missing-guard', 'missing or stale guard arming'), ('arming-binding', 'missing or stale guard arming'),
                  ('dead-guard', 'not live and supervised'), ('reused-guard-pid', 'not live and supervised'),
                  ('guard-argv', 'not live and supervised'), ('producer-argv', 'unregistered'),
                  ('dead-supervisor', 'unsupervised provisioner'), ('reused-producer-pid', 'unregistered'),
                  ('challenge', 'heartbeat binding differs'), ('identity', 'heartbeat binding differs'),
                  ('binding', 'heartbeat binding differs'), ('stale', 'stale guard heartbeat'),
                  ('future-heartbeat', 'stale guard heartbeat'), ('ancestry', 'stale guard heartbeat'),
                  ('stop', 'cleanup requested')]
        for fault, reason in faults:
            with self.subTest(fault=fault), Fixture() as f:
                if fault in ('missing-guard', 'arming-binding'):
                    with f.journal.locked() as state:
                        if fault == 'missing-guard': state['guard'] = None
                        else: state['guard']['binding']['uuid'] = 'wrong'
                        f.journal.commit(state)
                if fault == 'dead-guard': f.processes.pop(112)
                if fault == 'guard-argv': f.processes[112]['argv_sha256'] = 'b' * 64
                if fault == 'producer-argv': f.processes[111]['argv_sha256'] = 'b' * 64
                if fault == 'reused-guard-pid': f.processes[112]['starttime'] = 'reused'
                if fault == 'dead-supervisor': f.processes.pop(110)
                if fault == 'reused-producer-pid': f.processes[111]['starttime'] = 'reused'
                if fault == 'challenge': f.heartbeat(challenge='wrong')
                if fault == 'identity': f.heartbeat(identity=f.processes[111])
                if fault == 'binding': f.heartbeat(binding=dict(uuid='wrong'))
                if fault == 'stale': f.heartbeat(monotonic_ns=f.mono - 5 * NS - 1)
                if fault == 'future-heartbeat': f.heartbeat(monotonic_ns=f.mono + 1)
                if fault == 'ancestry': f.heartbeat(checkpoint={'generation': 0, 'sha256': 'f' * 64})
                if fault == 'stop': f.campaign.request_stop()
                with self.assertRaisesRegex(mc.GuardError, reason): f.create()
                self.assertEqual(f.cloud.requests, [])
                self.assertEqual(f.journal.read()['intents'], [])
        with Fixture(producer_parent=1) as f:
            with self.assertRaisesRegex(mc.GuardError, 'unregistered or unsupervised'):
                f.create()
            self.assertEqual(f.cloud.requests, [])

    def test_old_authenticated_heartbeat_accepts_legitimate_journal_advancement(self):
        f = self.f
        initial = f.initial_checkpoint
        f.create_all()
        self.assertGreater(f.journal.checkpoint['generation'], initial['generation'])
        with f.journal.locked() as state:
            f.campaign.guard_live(state)
        self.assertEqual(len(f.cloud.requests), 7)

    def test_live_guard_cannot_be_replaced_and_dead_guard_recovery_never_reopens_creation(self):
        f = self.f
        old = f.journal.read()['guard']
        f.processes[113] = dict(pid=113, parent=110, starttime='1130', argv_sha256='a' * 64)
        f.actor = 113
        with self.assertRaisesRegex(mc.GuardError, 'another guard is still live'):
            f.campaign.arm_guard()
        self.assertEqual(f.journal.read()['guard'], old)
        f.processes.pop(112)
        f.processes.pop(111)
        replacement = f.campaign.arm_guard()
        self.assertEqual(replacement['identity'], f.processes[113])
        self.assertNotEqual(replacement['challenge'], old['challenge'])
        self.assertEqual(replacement['binding'], old['binding'])
        self.assertEqual(f.journal.read()['mode'], 'CLOSING')
        heartbeat = json.loads(cc.private_bytes(f.guard_directory / 'heartbeat.json'))
        self.assertEqual(heartbeat['identity'], f.processes[113])
        self.assertEqual(heartbeat['challenge'], replacement['challenge'])
        self.assertEqual(f.cloud.requests, [])

    def test_producer_cannot_self_arm_as_its_guard(self):
        with Fixture(arm=False) as f:
            self.assertEqual(f.actor, f.header['producer']['pid'])
            with self.assertRaisesRegex(mc.GuardError, 'guard must be distinct from the producer'):
                f.campaign.arm_guard()
            self.assertIsNone(f.journal.read()['guard'])
            self.assertFalse((f.guard_directory / 'heartbeat.json').exists())
            self.assertEqual(f.cloud.requests, [])

    def test_heartbeat_never_blocks_on_writer_entering_after_strict_read(self):
        import fcntl
        f = self.f
        f.actor = 112
        observer = cl.Journal(f.journal.path)
        self.addCleanup(observer.close)
        guard = cs.Campaign(observer, f.cloud, clock=lambda: f.wall, monotonic=lambda: f.mono)
        writer = os.open(f.journal.path.parent / cl.LOCK, os.O_RDWR)
        original = fcntl.flock
        unlocks, entered = [], []
        def concurrent_writer(fd, operation):
            if operation == fcntl.LOCK_EX:
                raise AssertionError('heartbeat attempted a blocking journal reacquisition')
            result = original(fd, operation)
            if operation == fcntl.LOCK_UN:
                unlocks.append(fd)
                if len(unlocks) == 2:
                    original(writer, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    entered.append(True)
            return result
        try:
            with patch.object(fcntl, 'flock', side_effect=concurrent_writer):
                observed = guard.heartbeat()
            self.assertEqual(entered, [True])
            self.assertEqual(observed['mode'], 'OPEN')
            heartbeat = json.loads(cc.private_bytes(f.guard_directory / 'heartbeat.json'))
            self.assertEqual(heartbeat['identity'], f.processes[112])
            self.assertEqual(heartbeat['checkpoint'], observer.checkpoint)
        finally:
            original(writer, fcntl.LOCK_UN)
            os.close(writer)
        self.assertEqual(f.cloud.requests, [])

    def test_fixed_header_rejects_changed_threat_plan_sources_boot_and_deadlines(self):
        faults = [('threat', 'unaccepted campaign threat model'), ('reservation', 'unaccepted campaign threat model'),
                  ('sources', 'incomplete fixed guard source population'), ('baseline', 'incomplete ten-kind baseline'),
                  ('deadline', 'deadline or setup budget changed'), ('time-type', 'invalid sealed campaign time policy'),
                  ('plan', 'seven-resource plan'), ('boot', 'controller boot changed'),
                  ('guard-directory', 'guard directory identity changed')]
        for index, (fault, reason) in enumerate(faults):
            with self.subTest(fault=fault):
                header = copy.deepcopy(self.f.header)
                if fault == 'threat': header['threat_model'] = 'hostile-writer-without-provider-proof'
                if fault == 'reservation': header['graph_reserved'] = False
                if fault == 'sources': header['sources'].pop(next(iter(header['sources'])))
                if fault == 'baseline': header['baseline'].pop('volume')
                if fault == 'deadline': header['time']['h_ns'] += NS
                if fault == 'time-type': header['time']['monotonic_t0_ns'] = True
                if fault == 'plan': header['plan'].pop()
                if fault == 'boot': header['boot_id'] = 'different-boot'
                if fault == 'guard-directory': header['guard_directory']['inode'] += 1
                journal = cl.Journal.initialize(self.f.root / f'bad-header-{index}' / 'head.json', header,
                    dict(mode='OPEN', intents=[], owned=[], associations=[], events=[], guard=None, active_lease=None))
                self.addCleanup(journal.close)
                with self.assertRaisesRegex(mc.GuardError, reason):
                    cs.Campaign(journal, self.f.cloud, clock=lambda: self.f.wall, monotonic=lambda: self.f.mono)
        self.assertEqual(self.f.cloud.requests, [])

    def test_creation_plan_changes_and_name_collisions_reach_their_own_guards(self):
        f = self.f
        body = f.body()
        body['ip_range'] = '10.99.0.0/24'
        with self.assertRaisesRegex(mc.GuardError, 'differs from sealed plan'):
            f.campaign.create('network', body)
        f.cloud.rows['network'][888] = dict(id=888, name=f.body()['name'])
        with self.assertRaisesRegex(mc.GuardError, 'name already exists'):
            f.create()
        self.assertEqual(f.cloud.requests, [])
        self.assertEqual(f.journal.read()['intents'], [])

    def test_exact_id_authorization_refuses_matching_strangers_and_identity_drift_before_delete(self):
        cases = [('stranger', 'no owned creation record'), ('wrong-kind', 'no owned creation record'),
                 ('name', 'name/labels changed'), ('record-name-drift', 'owned canonical provenance differs'),
                 ('intent-prefix', 'invalid pinned intent identity'), ('labels', 'name/labels changed'),
                 ('nonce', 'name/labels changed'), ('created', 'identity changed or ID reused'),
                 ('canonical', 'identity changed or ID reused')]
        for fault, reason in cases:
            with self.subTest(fault=fault), Fixture() as f:
                rid = f.create()
                f.closing()
                kind = 'network'
                if fault == 'stranger':
                    item = copy.deepcopy(f.cloud.rows['network'][rid])
                    item['id'] = rid + 1
                    f.cloud.rows['network'][rid + 1] = item
                    rid += 1
                if fault == 'wrong-kind':
                    kind = 'firewall'
                    f.cloud.rows[kind][rid] = dict(id=rid, name='test-network', labels={'campaign': 'synthetic', 'deadline': str((T0 + 15 * 3600 * NS) // NS)}, rules=[])
                item = f.cloud.rows[kind][rid]
                if fault == 'name': item['name'] = 'test-other'
                if fault in {'record-name-drift', 'intent-prefix'}:
                    item['name'] = 'unrelated-project-resource'
                    with f.journal.locked() as state:
                        state['owned'][0]['name'] = item['name']
                        if fault == 'intent-prefix':
                            state['intents'][0]['name'] = item['name']
                        f.journal.commit(state)
                if fault == 'labels': item['labels']['campaign'] = 'other'
                if fault == 'nonce': item['labels']['intent'] = 'replacement'
                if fault == 'created': item['created'] = utc(T0 + 11 * NS)
                if fault == 'canonical': item['ip_range'] = '10.99.0.0/24'
                with self.assertRaisesRegex(mc.GuardError, reason): f.campaign.mutate(kind, rid, 'delete')
                self.assertEqual(f.deletes(), [])
                self.assertEqual(len(f.journal.read()['intents']), 1)

    def test_typed_owned_records_and_intent_provenance_are_checked_before_delete(self):
        faults = [('sequence', 'invalid intent sequence'), ('bool-sequence', 'invalid intent sequence'),
                  ('duplicate', 'duplicate owned typed IDs'), ('kind', 'invalid owned typed ID'),
                  ('missing-intent', 'missing owning intent'), ('missing-ack', 'lacks creation provenance')]
        for fault, reason in faults:
            with self.subTest(fault=fault), Fixture() as f:
                rid = f.create()
                f.closing()
                with f.journal.locked() as state:
                    if fault == 'sequence': state['intents'][0]['sequence'] = 1
                    if fault == 'bool-sequence': state['intents'][0]['sequence'] = False
                    if fault == 'duplicate': state['owned'].append(copy.deepcopy(state['owned'][0]))
                    if fault == 'kind': state['owned'][0]['kind'] = 'volume'
                    if fault == 'missing-intent': state['owned'][0]['intent'] = 99
                    if fault == 'missing-ack': state['intents'][0]['ack'] = None
                    f.journal.commit(state)
                with self.assertRaisesRegex(mc.GuardError, reason):
                    f.campaign.mutate('network', rid, 'delete')
                self.assertEqual(f.deletes(), [])
        with Fixture() as f:
            f.cloud.next_ids['network'] = 1
            f.create()
            f.closing()
            f.cloud.rows['network'][1]['id'] = True
            with self.assertRaisesRegex(mc.GuardError, 'exact ID has no owned creation record'):
                f.campaign.mutate('network', 1, 'delete')
            self.assertEqual(f.deletes(), [])

    def assert_authenticated_schema_refusal(self, label, reason, damage, kind='network'):
        with self.subTest(field=label), Fixture() as f:
            if kind == 'network':
                f.cloud.next_ids['network'] = 1
            rid = f.create(kind)
            self.assertTrue(f.campaign.poll_actions())
            f.closing()
            with f.journal.locked() as state:
                record = next(row for row in state['owned'] if row['kind'] == kind and row['id'] == rid)
                intent = state['intents'][record['intent']]
                self.assertIn('send_lease', intent)
                self.assertGreater(intent['send_generation'], 0)
                damage(intent, record, f)
                damaged = copy.deepcopy(state)
                f.journal.commit(state)
            checkpoint = f.journal.checkpoint
            # A new reader authenticates the intentionally malformed nested
            # record: refusal must come from Campaign, not JSON/MAC validation.
            with contextlib.closing(cl.Journal(f.journal.path)) as reader:
                self.assertEqual(reader.read(), damaged)
                self.assertEqual(reader.checkpoint, checkpoint)
            before = dict(requests=copy.deepcopy(f.cloud.requests), lists=copy.deepcopy(f.cloud.lists),
                          describes=copy.deepcopy(f.cloud.describes), actions=copy.deepcopy(f.cloud.action_reads),
                          rows=copy.deepcopy(f.cloud.rows))
            with self.assertRaisesRegex(mc.GuardError, '^' + reason + '$'):
                f.campaign.mutate(kind, rid, 'delete')
            self.assertEqual(f.cloud.requests, before['requests'])
            self.assertEqual(f.cloud.lists, before['lists'])
            self.assertEqual(f.cloud.describes, before['describes'])
            self.assertEqual(f.cloud.action_reads, before['actions'])
            self.assertEqual(f.cloud.rows, before['rows'])
            self.assertEqual(f.journal.checkpoint, checkpoint)
            self.assertEqual(f.deletes(), [])

    def test_valid_persisted_nested_schema_allows_actual_cleanup(self):
        for kind in ('network', 'server'):
            with self.subTest(kind=kind), Fixture() as f:
                if kind == 'network':
                    f.cloud.next_ids['network'] = 1
                rid = f.create(kind)
                if kind == 'network':
                    self.assertEqual(rid, 1)
                    f.campaign.mutate(kind, rid, 'subnet', copy.deepcopy(f.header['subnet']))
                else:
                    self.assertEqual(rid, 101)
                self.assertTrue(f.campaign.poll_actions())
                state = f.journal.read()
                self.assertTrue(any(intent['actions'] for intent in state['intents']))
                for intent in state['intents']:
                    self.assertIs(intent['acknowledgement_validated'], True)
                    self.assertIs(intent['required_action_identified'], True)
                    self.assertIs(type(intent['send_generation']), int)
                    self.assertGreater(intent['send_generation'], 0)
                    self.assertEqual(intent['send_lease']['intent'], intent['sequence'])
                    self.assertEqual(intent['send_lease']['actor'], intent['producer'])
                record = next(row for row in state['owned'] if row['kind'] == kind)
                self.assertEqual(record['implicit_ips'], [] if kind == 'network' else [10101])
                f.closing()
                result = f.campaign.cleanup_pass()
                self.assertTrue(result['complete'])
                self.assertEqual(result['pending_intents'], [])
                self.assertEqual(f.deletes(), [('DELETE', '/networks/1')] if kind == 'network' else
                    [('DELETE', '/servers/101'), ('DELETE', '/firewalls/301'), ('DELETE', '/ssh_keys/401')])

    def test_persisted_intent_operation_state_and_digest_reach_their_schema_guards(self):
        for field, value in (('kind', 'volume'), ('kind', True), ('operation', 'snapshot'),
                             ('operation', True), ('state', 'COMPLETE'), ('state', True)):
            self.assert_authenticated_schema_refusal(f'{field}={value!r}', 'invalid typed intent operation/state',
                lambda intent, record, fixture, field=field, value=value: intent.__setitem__(field, value))
        for label, value in (('wrong-type', 1), ('short', 'a' * 63), ('nonhex', 'g' * 64)):
            self.assert_authenticated_schema_refusal('request-sha256-' + label, 'invalid pinned intent identity',
                lambda intent, record, fixture, value=value: intent.__setitem__('request_sha256', value))
        self.assert_authenticated_schema_refusal('missing-request-sha256', 'invalid pinned intent identity',
            lambda intent, record, fixture: intent.pop('request_sha256'))

    def test_persisted_actions_flags_and_resource_ids_reach_their_schema_guards(self):
        for label, value in (('not-list', {}), ('boolean-id', [True]), ('zero-id', [0]),
                             ('string-id', ['1']), ('duplicate-id', [1, 1])):
            self.assert_authenticated_schema_refusal('actions-' + label, 'invalid known action IDs',
                lambda intent, record, fixture, value=value: intent.__setitem__('actions', value))
        for value in (True, 0, -1, '1'):
            self.assert_authenticated_schema_refusal(f'resource-id={value!r}', 'invalid intent resource ID',
                lambda intent, record, fixture, value=value: intent.__setitem__('resource_id', value))
        for field in ('acknowledgement_validated', 'required_action_identified'):
            for value in (1, None):
                self.assert_authenticated_schema_refusal(f'{field}={value!r}', 'invalid action evidence flags',
                    lambda intent, record, fixture, field=field, value=value: intent.__setitem__(field, value))

    def test_persisted_send_lease_population_types_and_bindings_reach_their_schema_guard(self):
        reason = 'invalid retained send lease'
        self.assert_authenticated_schema_refusal('send-lease-not-object', reason,
            lambda intent, record, fixture: intent.__setitem__('send_lease', None))
        self.assert_authenticated_schema_refusal('send-lease-missing-key', reason,
            lambda intent, record, fixture: intent['send_lease'].pop('deadline_ns'))
        self.assert_authenticated_schema_refusal('send-lease-extra-key', reason,
            lambda intent, record, fixture: intent['send_lease'].__setitem__('unexpected', True))
        for label, value in (('boolean-sequence', False), ('wrong-sequence', 1)):
            self.assert_authenticated_schema_refusal(label, reason,
                lambda intent, record, fixture, value=value: intent['send_lease'].__setitem__('intent', value))
        self.assert_authenticated_schema_refusal('send-lease-wrong-actor', reason,
            lambda intent, record, fixture: intent['send_lease'].__setitem__('actor',
                dict(intent['producer'], pid=999)))
        for field in ('deadline_ns', 'monotonic_deadline_ns'):
            self.assert_authenticated_schema_refusal(field + '-boolean', reason,
                lambda intent, record, fixture, field=field: intent['send_lease'].__setitem__(field, True))
        for label, value in (('boolean', True), ('nonpositive', 0)):
            self.assert_authenticated_schema_refusal('send-generation-' + label, reason,
                lambda intent, record, fixture, value=value: intent.__setitem__('send_generation', value))
        self.assert_authenticated_schema_refusal('send-generation-missing', reason,
            lambda intent, record, fixture: intent.pop('send_generation'))

    def test_persisted_create_nonce_descriptor_and_producer_reach_their_binding_guard(self):
        reason = 'invalid create provenance binding'
        for label, value in (('wrong-type', True), ('short', 'a' * 31), ('nonhex', 'g' * 32)):
            self.assert_authenticated_schema_refusal('nonce-' + label, reason,
                lambda intent, record, fixture, value=value: intent.__setitem__('nonce', value))
        self.assert_authenticated_schema_refusal('nonce-missing', reason,
            lambda intent, record, fixture: intent.pop('nonce'))
        self.assert_authenticated_schema_refusal('descriptor-not-object', reason,
            lambda intent, record, fixture: intent.__setitem__('descriptor', []))
        self.assert_authenticated_schema_refusal('descriptor-labels-not-object', reason,
            lambda intent, record, fixture: intent['descriptor'].__setitem__('labels', []))
        self.assert_authenticated_schema_refusal('descriptor-intent-label-mismatch', reason,
            lambda intent, record, fixture: intent['descriptor']['labels'].__setitem__('intent',
                ('0' if intent['nonce'][0] != '0' else '1') + intent['nonce'][1:]))
        def producer_mismatch(intent, record, fixture):
            intent['producer'] = dict(intent['producer'], pid=999)
            # Keep the earlier retained-lease actor predicate satisfied so this
            # independently reaches the immutable create-producer binding.
            intent['send_lease']['actor'] = copy.deepcopy(intent['producer'])
        self.assert_authenticated_schema_refusal('create-producer-binding', reason, producer_mismatch)

    def test_persisted_pre_call_census_population_and_typed_ids_reach_their_schema_guard(self):
        reason = 'invalid pre-call typed census'
        self.assert_authenticated_schema_refusal('pre-ids-not-object', reason,
            lambda intent, record, fixture: intent.__setitem__('pre_ids', []))
        self.assert_authenticated_schema_refusal('pre-ids-missing-kind', reason,
            lambda intent, record, fixture: intent['pre_ids'].pop('certificate'))
        self.assert_authenticated_schema_refusal('pre-ids-extra-kind', reason,
            lambda intent, record, fixture: intent['pre_ids'].__setitem__('unplanned-kind', []))
        for label, value in (('not-list', {}), ('boolean-id', [True]), ('zero-id', [0]),
                             ('negative-id', [-1]), ('string-id', ['9000']),
                             ('duplicate-id', [9000, 9000]), ('unsorted-id', [9002, 9000])):
            self.assert_authenticated_schema_refusal('pre-server-ids-' + label, reason,
                lambda intent, record, fixture, value=value: intent['pre_ids'].__setitem__('server', value))

    def test_persisted_observation_bracket_types_and_order_reach_their_schema_guard(self):
        reason = 'invalid pre-call observation bracket'
        for field in ('observed_start_ns', 'observed_end_ns'):
            self.assert_authenticated_schema_refusal(field + '-boolean', reason,
                lambda intent, record, fixture, field=field: intent.__setitem__(field, True))
            self.assert_authenticated_schema_refusal(field + '-missing', reason,
                lambda intent, record, fixture, field=field: intent.pop(field))
        self.assert_authenticated_schema_refusal('observation-bracket-reversed', reason,
            lambda intent, record, fixture: intent.__setitem__('observed_start_ns', intent['observed_end_ns'] + 1))

    def test_persisted_subordinate_id_population_and_types_reach_their_schema_guard(self):
        reason = 'invalid subordinate typed IDs'
        for label, value in (('not-list', None), ('boolean-id', [True]), ('zero-id', [0]),
                             ('negative-id', [-1]), ('string-id', ['10101']), ('duplicate-id', [10101, 10101])):
            self.assert_authenticated_schema_refusal('implicit-ips-' + label, reason,
                lambda intent, record, fixture, value=value: record.__setitem__('implicit_ips', value), kind='server')
        self.assert_authenticated_schema_refusal('implicit-ips-missing', reason,
            lambda intent, record, fixture: record.pop('implicit_ips'), kind='server')

    def test_nonallocating_operations_require_owned_dependencies_and_sealed_fields(self):
        f = self.f
        network = f.create()
        server = f.create('server')
        f.wall = T0 + 2000 * NS  # Create admission expired; sealed setup remains open.
        f.campaign.mutate('network', network, 'subnet', copy.deepcopy(f.header['subnet']))
        address = next(p['private_ip'] for p in f.header['plan'] if p['name'] == 'test-node0')
        f.campaign.mutate('server', server, 'attach', dict(network=network, ip=address))
        f.campaign.mutate('server', server, 'poweron', {})
        self.assertEqual([path for method, path, _, _ in f.cloud.requests[-3:]],
                         ['/networks/201/actions/add_subnet', '/servers/101/actions/attach_to_network', '/servers/101/actions/poweron'])
        for intent in f.journal.read()['intents'][-3:]:
            self.assertEqual(intent['state'], 'ACKNOWLEDGED')
            self.assertEqual(intent['producer'], f.header['producer'])
        before = len(f.cloud.requests)
        with self.assertRaisesRegex(mc.GuardError, 'provisioning budget exhausted'):
            f.create('server', 1)
        self.assertEqual(len(f.cloud.requests), before)
        for fault, reason in [('subnet', 'subnet differs from plan'), ('address', 'attach address differs from plan'),
                              ('network', 'exact ID has no owned creation record')]:
            with self.subTest(fault=fault):
                if fault == 'subnet':
                    body = dict(f.header['subnet'], ip_range='10.99.0.0/24')
                    with self.assertRaisesRegex(mc.GuardError, reason): f.campaign.mutate('network', network, 'subnet', body)
                else:
                    body = dict(network=network, ip=address)
                    if fault == 'address': body['ip'] = '10.42.0.99'
                    if fault == 'network':
                        body['network'] = 777
                        f.cloud.rows['network'][777] = dict(id=777, name='test-foreign-network', ip_range='10.42.0.0/24')
                    with self.assertRaisesRegex(mc.GuardError, reason): f.campaign.mutate('server', server, 'attach', body)
                self.assertEqual(len(f.cloud.requests), before)

    def test_baseline_and_pre_call_authorization_conjuncts_are_independent(self):
        for protected in ('baseline', 'pre-call'):
            with self.subTest(protected=protected), Fixture() as f:
                rid = f.create()
                with f.journal.locked() as state:
                    if protected == 'pre-call':
                        state['intents'][0]['pre_ids']['network'] = sorted(state['intents'][0]['pre_ids']['network'] + [rid])
                    else:
                        # A conflicting local provenance record cannot override sealed baseline protection.
                        baseline_id = f.header['baseline']['network'][0]
                        item = f.cloud.rows['network'].pop(rid)
                        item['id'] = baseline_id
                        f.cloud.rows['network'][baseline_id] = item
                        state['owned'][0]['id'] = baseline_id
                        state['intents'][0]['resource_id'] = baseline_id
                        state['intents'][0]['pre_ids']['network'].remove(baseline_id)
                        rid = baseline_id
                    f.journal.commit(state)
                f.closing()
                with self.assertRaisesRegex(mc.GuardError, 'protected/pre-call inventory'):
                    f.campaign.mutate('network', rid, 'delete')
                self.assertEqual(f.deletes(), [])

    def test_server_delete_requires_current_complete_reserved_ip_graph(self):
        faults = [('ipv6', 'unexpected or missing IP family'), ('volume', 'unplanned server cascade'),
                  ('backup', 'unplanned server cascade'), ('floating', 'unplanned server public-network cascade'),
                  ('missing-ip', 'observed-retired IP cannot inherit association'),
                  ('foreign-ip', 'observed-retired IP cannot inherit association'),
                  ('extra-association', 'incomplete or foreign IP association graph'),
                  ('missing-association', 'observed-retired IP cannot inherit association'),
                  ('not-auto-delete', 'protected, unplanned or unattributable'),
                  ('wrong-family', 'protected, unplanned or unattributable'), ('malformed-ip-created', 'invalid provider creation timestamp'),
                  ('missing-created', 'missing provider creation timestamp')]
        for fault, reason in faults:
            with self.subTest(fault=fault), Fixture() as f:
                rid = f.create('server')
                f.closing()
                item = f.cloud.rows['server'][rid]
                ip = f.cloud.rows['primary-ip'][rid + 10_000]
                if fault == 'ipv6': item['public_net']['ipv6'] = {'id': 999}
                if fault == 'volume': item['volumes'] = [999]
                if fault == 'backup': item['backup_window'] = '22-02'
                if fault == 'floating': item['public_net']['floating_ips'] = [999]
                if fault == 'missing-ip': item['public_net'].pop('ipv4')
                if fault == 'foreign-ip': item['public_net']['ipv4']['id'] = f.header['baseline']['primary-ip'][0]
                if fault == 'extra-association':
                    extra = dict(ip, id=999)
                    f.cloud.rows['primary-ip'][999] = extra
                if fault == 'missing-association': f.cloud.rows['primary-ip'].pop(ip['id'])
                if fault == 'not-auto-delete': ip['auto_delete'] = False
                if fault == 'wrong-family': ip['type'] = 'ipv6'
                if fault == 'malformed-ip-created': ip['created'] = 'not-a-timestamp'
                if fault == 'missing-created': ip.pop('created')
                with self.assertRaisesRegex(mc.GuardError, reason): f.campaign.mutate('server', rid, 'delete')
                if fault in {'missing-ip', 'foreign-ip'}:
                    self.assertTrue(f.campaign._retired(f.journal.read(), 'primary-ip', ip['id']))
                self.assertEqual(f.deletes(), [])
                self.assertIn(rid, f.cloud.rows['server'])
                self.assertEqual(len([r for r in f.journal.read()['owned'] if r['kind'] == 'server']), 1)

    def test_missing_cascade_fields_are_not_proof_of_absence(self):
        for field in ('volumes', 'backup_window', 'floating_ips'):
            with self.subTest(field=field), Fixture() as f:
                rid = f.create('server')
                f.closing()
                item = f.cloud.rows['server'][rid]
                (item['public_net'] if field == 'floating_ips' else item).pop(field)
                with self.assertRaises(mc.GuardError):
                    f.campaign.mutate('server', rid, 'delete')
                self.assertEqual(f.deletes(), [])
                self.assertIn(rid, f.cloud.rows['server'])

    def test_subordinate_baseline_pre_call_and_missing_auto_delete_are_not_delete_authority(self):
        for fault in ('baseline', 'pre-call', 'missing-auto-delete'):
            with self.subTest(fault=fault), Fixture() as f:
                rid = f.create('server')
                ipid = rid + 10_000
                with f.journal.locked() as state:
                    record = next(r for r in state['owned'] if r['kind'] == 'server')
                    intent = state['intents'][record['intent']]
                    if fault == 'baseline':
                        baseline_id = f.header['baseline']['primary-ip'][0]
                        ip = f.cloud.rows['primary-ip'].pop(ipid)
                        ip['id'] = baseline_id
                        f.cloud.rows['primary-ip'][baseline_id] = ip
                        f.cloud.rows['server'][rid]['public_net']['ipv4']['id'] = baseline_id
                        record['implicit_ips'] = [baseline_id]
                        intent['ack']['payload']['server']['public_net']['ipv4']['id'] = baseline_id
                        intent['pre_ids']['primary-ip'].remove(baseline_id)
                    if fault == 'pre-call': intent['pre_ids']['primary-ip'].append(ipid)
                    if fault == 'missing-auto-delete': f.cloud.rows['primary-ip'][ipid].pop('auto_delete')
                    f.journal.commit(state)
                f.closing()
                with self.assertRaisesRegex(mc.GuardError, 'protected, unplanned or unattributable'):
                    f.campaign.mutate('server', rid, 'delete')
                self.assertEqual(f.deletes(), [])

    def test_cleanup_at_cutover_and_after_hard_deadline_uses_phase_specific_lease(self):
        for phase in ('cutover', 'post-hard'):
            with self.subTest(phase=phase), Fixture() as f:
                rid = f.create()
                f.closing()
                f.wall = f.header['time']['g_ns'] if phase == 'cutover' else f.header['time']['h_ns'] + NS
                def inspect(method, path, body, seconds):
                    current = f.journal.peek()
                    self.assertEqual((method, path, seconds), ('DELETE', '/networks/201', 20))
                    self.assertEqual(current['mode'], 'CLOSING' if phase == 'cutover' else 'INCIDENT')
                    self.assertIsNotNone(current['active_lease'])
                f.cloud.before_request = inspect
                f.campaign.mutate('network', rid, 'delete')
                self.assertEqual(f.deletes(), [('DELETE', '/networks/201')])
                self.assertTrue(f.campaign.poll_actions())
                result = f.campaign.completion()
                self.assertTrue(result['complete'])
                self.assertEqual(result['hard_lifetime_met'], phase == 'cutover')
                self.assertEqual(f.journal.header['time']['h_ns'], T0 + 16 * 3600 * NS)
                if phase == 'post-hard':
                    self.assertTrue(any(e['event'] == 'cleanup-reserve-exhausted' and not e['hard_lifetime_met']
                                        for e in f.journal.read()['events']))

    def test_create_and_setup_budgets_use_monotonic_origin_and_termination_reserve(self):
        f = self.f
        f.wall = f.header['time']['c_ns'] - 22 * NS
        captured = []
        f.cloud.before_request = lambda method, path, body, seconds: captured.append((seconds, f.journal.peek()['active_lease']['deadline_ns']))
        f.create()
        self.assertEqual(captured, [(20, f.header['time']['c_ns'] - 2 * NS)])
        for fault in ('wall-expired', 'monotonic-expired', 'reserve-boundary'):
            with self.subTest(fault=fault), Fixture() as other:
                if fault == 'wall-expired': other.wall = other.header['time']['c_ns']
                if fault == 'monotonic-expired':
                    other.wall = T0 - NS
                    other.mono = M0 + 1800 * NS
                    other.heartbeat()
                if fault == 'reserve-boundary': other.wall = other.header['time']['c_ns'] - 2 * NS
                with self.assertRaisesRegex(mc.GuardError, 'provisioning budget exhausted'):
                    other.create()
                self.assertEqual(other.cloud.requests, [])
        with f.journal.locked() as state:
            f.wall = f.header['time']['setup_ns'] - 3 * NS
            self.assertEqual(f.campaign.lease_seconds(state, provisioning=True), 1)
            f.wall += NS
            with self.assertRaisesRegex(mc.GuardError, 'provisioning budget exhausted'):
                f.campaign.lease_seconds(state, provisioning=True)

    def test_action_status_and_exact_resource_binding_prevent_false_completion(self):
        f = self.f
        f.create('server')
        action = next(a for a in f.cloud.actions.values() if a['command'] == 'create_server')
        action['resources'] = [{'type': 'server', 'id': 999}]
        self.assertFalse(f.campaign.poll_actions())
        state = f.journal.read()
        self.assertEqual(state['intents'][-1]['state'], 'ACKNOWLEDGED')
        self.assertEqual(state['intents'][-1]['pending_actions'], [action['id']])
        self.assertTrue(any(event.get('event') == 'action-read-unavailable'
                            and event.get('action_id') == action['id'] and event.get('error_type') == 'GuardError'
                            for event in state['events']))
        action['resources'] = [{'type': 'server', 'id': 101}]
        action['status'] = 'running'
        self.assertFalse(f.campaign.poll_actions())
        f.closing()
        self.assertFalse(f.campaign.completion()['complete'])
        action['status'] = 'error'
        self.assertFalse(f.campaign.poll_actions())
        self.assertEqual(f.journal.read()['intents'][-1]['state'], 'FAILED')
        self.assertFalse(f.campaign.completion()['complete'])

    def test_open_empty_inventory_is_not_a_quiet_completion(self):
        with self.assertRaisesRegex(mc.GuardError, 'completion requires closed admission'):
            self.f.campaign.completion()
        self.assertEqual(self.f.journal.read()['mode'], 'OPEN')
        self.assertEqual(self.f.cloud.lists, [])
        self.assertEqual(self.f.cloud.requests, [])

    def test_pending_delete_action_blocks_completion_even_after_exact_inventory_restoration(self):
        f = self.f
        rid = f.create('server')
        self.assertTrue(f.campaign.poll_actions())
        f.closing()
        f.campaign.mutate('server', rid, 'delete')
        f.campaign.mutate('firewall', 301, 'delete')
        f.campaign.mutate('ssh-key', 401, 'delete')
        action = next(a for a in f.cloud.actions.values() if a['command'] == 'delete_server')
        action['status'] = 'running'
        self.assertFalse(f.campaign.poll_actions())
        result = f.campaign.completion()
        self.assertTrue(result['baseline_equal'])
        self.assertEqual(result['owned_survivors'], [])
        self.assertEqual(result['pending_intents'], [3])
        self.assertFalse(result['complete'])
        action['status'] = 'success'
        self.assertTrue(f.campaign.poll_actions())
        self.assertTrue(f.campaign.completion()['complete'])

    def test_full_cleanup_retains_provenance_and_confirms_all_ten_inventory_kinds(self):
        f = self.f
        ids = f.create_all()
        self.assertTrue(f.campaign.poll_actions())
        f.closing()
        ordered = [(kind, rid) for kind in ('server', 'firewall', 'ssh-key', 'network') for k, rid in ids if k == kind]
        f.cloud.lists.clear()
        result = f.campaign.cleanup_pass()
        self.assertEqual(f.deletes(), [('DELETE', f'/{cc.PLURALS[kind]}/{rid}') for kind, rid in ordered])
        self.assertEqual([kind for kind, _ in f.cloud.lists[-10:]], list(INVENTORY_KINDS))
        self.assertEqual(result, dict(complete=True, baseline_equal=True, owned_survivors=[], subordinate_survivors=0,
                                      pending_intents=[], hard_lifetime_met=True))
        finished = f.journal.read()
        self.assertEqual(finished['mode'], 'COMPLETE')
        self.assertEqual(len(finished['owned']), 7)
        self.assertEqual(len(finished['associations']), 4)
        self.assertEqual(len(finished['intents']), 14)

    def test_unrelated_inventory_and_failed_census_never_authorize_extra_deletes(self):
        for kind in INVENTORY_KINDS:
            with self.subTest(kind=kind), Fixture() as f:
                f.closing()
                f.cloud.rows[kind][888] = dict(id=888, name='unrelated-project-resource')
                result = f.campaign.completion()
                self.assertFalse(result['complete'])
                self.assertFalse(result['baseline_equal'])
                self.assertEqual(result['owned_survivors'], [])
                self.assertEqual(f.deletes(), [])
                f.cloud.fail_list = kind
                with self.assertRaisesRegex(mc.GuardError, 'provider list unavailable'):
                    f.campaign.completion()
                self.assertEqual(f.journal.read()['mode'], 'CLOSING')

    def test_immediate_and_deadline_guard_loop_close_before_shared_cleanup(self):
        for trigger in ('immediate', 'deadline'):
            with self.subTest(trigger=trigger), Fixture(arm=False) as f:
                f.actor = 112
                if trigger == 'immediate': f.campaign.request_stop()
                else: f.wall = f.header['time']['g_ns']
                self.assertTrue(cs.live(f.header['producer']))
                read_fd, write_fd = os.pipe()
                waits, signals, observed_modes = [], [], []
                def pidfd_open(pid):
                    self.assertEqual(pid, f.header['producer']['pid'])
                    return read_fd
                def wait(readable, writable, exceptional, seconds):
                    waits.append(seconds)
                    return ([], [], []) if len(waits) == 1 else ([read_fd], [], [])
                def sent(fd, sig):
                    signals.append((fd, sig))
                    f.processes.pop(111)
                f.cloud.before_list = lambda kind, seconds: observed_modes.append(f.journal.peek()['mode'])
                try:
                    with patch.object(cs.os, 'pidfd_open', side_effect=pidfd_open, create=True), \
                         patch.object(cs.signal, 'pidfd_send_signal', side_effect=sent, create=True), \
                         patch.object(cs.select, 'select', side_effect=wait), \
                         patch.object(cs.os, 'kill', side_effect=AssertionError('bare PID signalling forbidden')):
                        result = f.campaign.guard_loop(sleep=lambda seconds: self.fail('clean abort should finish one pass'))
                finally:
                    os.close(write_fd)
                self.assertEqual(signals, [(read_fd, signal.SIGTERM)])
                self.assertEqual(waits, [0, 1])
                self.assertTrue(observed_modes)
                self.assertEqual(set(observed_modes), {'CLOSING'})
                self.assertTrue(result['complete'])
                self.assertTrue(result['hard_lifetime_met'])
                self.assertEqual(f.journal.read()['mode'], 'COMPLETE')
                self.assertEqual(json.loads(cc.private_bytes(f.guard_directory / 'cleanup.json')), result)
                self.assertFalse((f.guard_directory / 'incident.json').exists())
                self.assertEqual(f.cloud.requests, [])

    def test_cutover_drains_only_remaining_lease_and_caps_total_termination(self):
        for remaining, exits in ((3, True), (-1, True), (-1, False)):
            with self.subTest(remaining=remaining, exits=exits), Fixture() as f:
                f.create()
                with f.journal.locked() as state:
                    state['intents'][0]['state'] = 'SENDING'
                    state['active_lease'] = dict(intent=0, actor=f.header['producer'],
                                                deadline_ns=f.wall + remaining * NS, monotonic_deadline_ns=f.mono + remaining * NS)
                    f.journal.commit(state)
                f.actor = 112
                read_fd, write_fd = os.pipe()
                waits, signals, heartbeats = [], [], []
                initial_mono = f.mono
                def wait(readable, writable, exceptional, seconds):
                    waits.append(seconds)
                    heartbeats.append(json.loads(cc.private_bytes(f.guard_directory / 'heartbeat.json'))['monotonic_ns'])
                    f.mono += int(seconds * NS)
                    terminal = signal.SIGKILL if remaining >= 0 else signal.SIGTERM
                    return ([read_fd], [], []) if exits and any(sig == terminal for _, sig in signals) else ([], [], [])
                def sent(fd, sig):
                    signals.append((fd, sig))
                    if exits and sig == (signal.SIGKILL if remaining >= 0 else signal.SIGTERM):
                        f.processes.pop(111)
                try:
                    with patch.object(cs.os, 'pidfd_open', return_value=read_fd), \
                         patch.object(cs.signal, 'pidfd_send_signal', side_effect=sent), \
                         patch.object(cs.select, 'select', side_effect=wait), \
                         patch.object(cs.os, 'kill', side_effect=AssertionError('bare PID signalling forbidden')):
                        if exits:
                            f.campaign.close_admission()
                        else:
                            with self.assertRaisesRegex(mc.GuardError, 'producer did not quiesce'):
                                f.campaign.close_admission()
                    expected_waits = [1, 1, 1, 1, 1] if remaining >= 0 else [0, 1]
                    self.assertEqual(waits, expected_waits)
                    self.assertEqual(heartbeats, [initial_mono + int(sum(waits[:i]) * NS) for i in range(len(waits))])
                    self.assertEqual(sum(waits), max(remaining + 2, 0))
                    expected_signals = [(read_fd, signal.SIGTERM)]
                    if remaining >= 0 or not exits:
                        expected_signals.append((read_fd, signal.SIGKILL))
                    self.assertEqual(signals, expected_signals)
                    self.assertEqual(f.journal.read()['mode'], 'CLOSING' if exits else 'OPEN')
                    self.assertEqual(f.cloud.requests[1:], [])
                finally:
                    os.close(write_fd)

    def test_close_admission_uses_owned_pidfd_and_commits_only_after_producer_quiesces(self):
        f = self.f
        f.actor = 112
        read_fd, write_fd = os.pipe()
        self.addCleanup(os.close, write_fd)
        signals, waits = [], []
        def pidfd_open(pid):
            self.assertEqual(pid, f.header['producer']['pid'])
            self.assertEqual(cs.identity(pid), f.header['producer'])
            return read_fd
        def wait(readable, writable, exceptional, seconds):
            waits.append(seconds)
            return ([], [], []) if len(waits) == 1 else ([read_fd], [], [])
        def sent(fd, sig):
            signals.append((fd, sig))
            f.processes.pop(111)
        with patch.object(cs.os, 'pidfd_open', side_effect=pidfd_open, create=True), \
             patch.object(cs.signal, 'pidfd_send_signal', side_effect=sent, create=True), \
             patch.object(cs.select, 'select', side_effect=wait), \
             patch.object(cs.os, 'kill', side_effect=AssertionError('bare PID signalling forbidden')):
            f.campaign.close_admission()
        self.assertEqual(signals, [(read_fd, signal.SIGTERM)])
        self.assertEqual(waits, [0, 1])
        self.assertEqual(f.journal.read()['mode'], 'CLOSING')
        stop = json.loads(cc.private_bytes(f.guard_directory / 'stop.json'))
        self.assertEqual(stop['binding'], f.journal.binding)
        self.assertFalse(cs.live(f.header['producer']))
        self.assertEqual(f.cloud.requests, [])


if __name__ == '__main__':
    unittest.main()
