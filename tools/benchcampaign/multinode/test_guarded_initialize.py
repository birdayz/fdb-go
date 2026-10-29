#!/usr/bin/env python3
"""Real guarded-plan and initialization coverage with synthetic provider inputs.

Journal/Profile/Campaign validation, private files, source pins and guard arming
are real. Only cloud/provider, process identities and clock/sleep boundaries are
injected. No credentials, baseline configuration or network from the host is used.
"""
import contextlib
import copy
import errno
import io
import json
import os
from pathlib import Path
import stat
import sys
import tempfile
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
from test_campaign_safety import Provider, T0, M0, NS  # noqa: E402

REAL_CAMPAIGN = cs.Campaign
TOKEN = 'synthetic-initialization-controller-credential'
PUBLIC_KEY = 'ssh-ed25519 SYNTHETIC-INITIALIZATION-KEY controller@test'
PREFIX = 'init-test'


class InitProvider(Provider):
    def request(self, method, path, body=None, seconds=20):
        reply = super().request(method, path, body=body, seconds=seconds)
        if method == 'POST' and path.endswith('/actions/attach_to_network'):
            rid = int(path.split('/')[2])
            self.rows['server'][rid]['private_net'] = [dict(network=body['network'], ip=body['ip'])]
        return reply


class Initialization:
    def __init__(self):
        self.stack = contextlib.ExitStack()
        self.root = Path(self.stack.enter_context(tempfile.TemporaryDirectory(prefix='guard-init-test-')))
        self.stack.enter_context(mock.patch.dict(os.environ, {}, clear=True))
        # Bazel source inputs may be runfile symlinks. Pin exact declared source
        # bytes in ordinary private files, not fabricated hashes or source text.
        self.source = self.root / 'pinned-sources'
        self.source.mkdir(mode=0o700)
        for name in cs.SOURCE_NAMES:
            path = self.source / name
            path.write_bytes((HERE / name).read_bytes())
            path.chmod(0o600)
        self.stack.enter_context(mock.patch.object(cs, 'SOURCE_DIR', self.source))
        self.wall, self.mono, self.actor = T0 + 10 * NS, M0, 111
        self.processes = {pid: dict(pid=pid, parent=parent, starttime=str(pid * 10),
                                   argv_sha256=cc.digest(str(pid).encode()))
                          for pid, parent in ((110, 1), (111, 110), (112, 110))}
        def identity(pid=None):
            pid = self.actor if pid is None else pid
            if pid not in self.processes:
                raise ProcessLookupError('synthetic process is gone')
            return copy.deepcopy(self.processes[pid])
        self.stack.enter_context(mock.patch.object(cs, 'identity', side_effect=identity))
        self.stack.enter_context(mock.patch.object(provision.time, 'time_ns', side_effect=lambda: self.wall))
        self.stack.enter_context(mock.patch.object(provision.time, 'monotonic_ns', side_effect=lambda: self.mono))
        self.stack.enter_context(mock.patch.object(provision.time, 'monotonic', side_effect=lambda: self.mono / NS))
        for target, name in ((cc.http.client, 'HTTPSConnection'), (os, 'pidfd_open'),
                             (cs.signal, 'pidfd_send_signal')):
            self.stack.enter_context(mock.patch.object(target, name, side_effect=AssertionError(
                'unexpected real network/process boundary: ' + name)))
        self.config = self.private('cloud.toml',
            'active_context="synthetic-init"\n[[contexts]]\nname="synthetic-init"\n' + f'token="{TOKEN}"\n')
        self.profile = cc.Profile(context='synthetic-init', config=self.config, environ={})
        self.header = {'context': self.profile.binding}
        self.cloud = InitProvider(self, self.profile)
        self.cloud.rows['network'][9001]['ip_range'] = '10.66.0.0/24'
        self.baseline = copy.deepcopy(self.cloud.rows)
        self.inventory = self.root / 'inventory'
        self.inventory.mkdir(mode=0o700)
        for kind in mc.PROTECT_KINDS:
            self.private(f'inventory/before-{kind}.json', json.dumps(list(self.baseline[kind].values())) + '\n')
        self.protect = self.private('inventory/protect.tsv', '\n'.join(
            f'{kind}\t{rid}\t{row["name"]}' for kind, rows in self.baseline.items() for rid, row in rows.items()) + '\n')
        self.key = self.private('controller.pub', PUBLIC_KEY + '\n')
        self.userdata = {}
        for role in provision.ROLE_NAMES:
            self.userdata[role] = self.private(f'cloud-init/{PREFIX}-{role}.user-data',
                                              '#cloud-config\n# synthetic private input for ' + role + '\n')
        self.work = self.root / 'work'
        self.ledger = self.work / 'journal' / 'head.json'
        self.deadline = (T0 + 15 * 3600 * NS) // NS
        self.argv = [
            '--label', 'campaign=synthetic-init', '--prefix', PREFIX,
            '--controller-cidr', '203.0.113.7/32', '--ssh-public-key', str(self.key),
            '--cloud-init-dir', str(self.root / 'cloud-init'), '--work-dir', str(self.work),
            '--creation-ledger', str(self.ledger), '--initialize-ledger', '--inventory', str(self.inventory),
            '--activation-time', str(T0 // NS), '--teardown-deadline', str(self.deadline),
            '--supervisor-pid', '110', '--context', 'synthetic-init', '--hcloud-config', str(self.config),
            '--accept-benign-writers', '--reserve-owned-graph',
            '--protect-file', str(self.protect), '--max-lifetime-hours', '16', '--image', '161547269',
            '--guard-wait-seconds', '1', '--execute']
        self.args = provision.parse_args(self.argv)
        self.journal = self.guard_journal = None
        self.sleep_calls, self.constructed, self.cloud_profiles = [], [], []

    def __enter__(self):
        return self

    def __exit__(self, *args):
        return self.stack.__exit__(*args)

    def private(self, relative, text):
        path = self.root / relative
        path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        path.write_text(text)
        path.chmod(0o600)
        return path

    def plan(self):
        return provision.guarded_plan(self.args, self.deadline)

    def initialize(self):
        journal = provision.initialize_guarded(self.args, self.deadline, self.profile, self.cloud, self.plan())
        self.stack.callback(journal.close)
        self.journal, self.header = journal, journal.header
        return journal

    def campaign(self, journal, cloud=None):
        instance = REAL_CAMPAIGN(journal, self.cloud if cloud is None else cloud,
                                 clock=lambda: self.wall, monotonic=lambda: self.mono)
        self.constructed.append(instance)
        return instance

    def cloud_factory(self, profile, binding):
        if profile.binding != binding or binding != self.profile.binding:
            raise AssertionError('cloud factory received a different real profile binding')
        self.cloud_profiles.append(profile)
        self.cloud.profile = profile
        return self.cloud

    def arm_on_guard_wait(self, seconds):
        self.sleep_calls.append(seconds)
        if seconds != 0.1 or self.guard_journal is not None:
            raise AssertionError('unexpected wait outside the first real guard-arming handshake')
        self.guard_journal = cl.Journal(self.ledger)
        self.stack.callback(self.guard_journal.close)
        self.header = self.guard_journal.header
        self.actor = 112
        try:
            self.campaign(self.guard_journal).arm_guard()
        finally:
            self.actor = 111

    def run(self):
        # Constructor substitution injects clocks only and returns a new genuine
        # Campaign. Every method including _producer/arm_guard remains production.
        with mock.patch.object(cc, 'Cloud', side_effect=self.cloud_factory), \
                mock.patch.object(cs, 'Campaign', side_effect=self.campaign), \
                mock.patch.object(provision.time, 'sleep', side_effect=self.arm_on_guard_wait):
            return provision.run(self.args, now=self.wall // NS, out=io.StringIO(),
                                 runner=lambda *args, **kwargs: (_ for _ in ()).throw(
                                     AssertionError('legacy CLI must never run')))


class GuardedInitializationTest(unittest.TestCase):
    def assert_no_creates(self, f):
        self.assertEqual(f.cloud.requests, [])
        self.assertEqual(f.cloud.next_ids, {'server': 101, 'network': 201, 'firewall': 301, 'ssh-key': 401})
        self.assertEqual(f.cloud.rows, f.baseline)

    def test_guarded_plan_contains_exact_seven_bodies_names_dependencies_and_private_bytes(self):
        with Initialization() as f:
            plan = f.plan()
            self.assertEqual([entry['kind'] for entry in plan],
                             ['network', 'firewall', 'ssh-key', 'server', 'server', 'server', 'server'])
            self.assertEqual([entry['name'] for entry in plan],
                             [PREFIX, PREFIX + '-fw', PREFIX + '-controller',
                              *[PREFIX + '-' + role for role in provision.ROLE_NAMES]])
            self.assertEqual(plan[0]['body']['ip_range'], '10.88.0.0/24')
            self.assertEqual(plan[1]['body']['rules'], [
                dict(direction='in', protocol='tcp', port='22', source_ips=['203.0.113.7/32'],
                     description='controller ssh only'),
                dict(direction='in', protocol='icmp', source_ips=['203.0.113.7/32'],
                     description='controller icmp only')])
            self.assertEqual(plan[2]['body']['public_key'], PUBLIC_KEY)
            for role, ip, entry in zip(provision.ROLE_NAMES,
                                      ['10.88.0.10', '10.88.0.11', '10.88.0.12', '10.88.0.13'], plan[3:]):
                body = entry['body']
                self.assertEqual(entry['private_ip'], ip)
                self.assertEqual(body['user_data'], f.userdata[role].read_text())
                self.assertEqual(body['ssh_keys'], [PREFIX + '-controller'])
                self.assertEqual(body['firewalls'], [dict(firewall=PREFIX + '-fw')])
                self.assertEqual((body['server_type'], body['location'], body['image']), ('ccx43', 'fsn1', 161547269))
                self.assertIs(body['start_after_create'], False)
                self.assertEqual(body['public_net'], dict(enable_ipv4=True, enable_ipv6=False))
                self.assertEqual(body['labels'], dict(campaign='synthetic-init', deadline=str(f.deadline),
                    role='client' if role == 'client' else 'fdb-server', node=role))
            self.assert_no_creates(f)
            self.assertFalse(f.work.exists())

    def test_initialize_pins_full_header_real_profile_sources_and_sixteen_private_files(self):
        with Initialization() as f:
            plan = f.plan()
            journal = f.initialize()
            campaign = f.campaign(journal)
            campaign.validate_pins()
            header = journal.header
            self.assertEqual(header['campaign'], 'synthetic-init')
            self.assertEqual(header['prefix'], PREFIX)
            self.assertEqual(header['context'], f.profile.binding)
            self.assertEqual(header['context']['config_sha256'], cc.digest(f.config.read_bytes()))
            self.assertEqual(header['context']['credential_sha256'], cc.digest(TOKEN.encode()))
            self.assertEqual(header['producer'], f.processes[111])
            self.assertEqual(header['supervisor'], f.processes[110])
            self.assertEqual(header['baseline'], {kind: sorted(rows) for kind, rows in f.baseline.items()})
            self.assertEqual(header['sources'], {str(f.source / name): cc.digest((HERE / name).read_bytes())
                                                  for name in cs.SOURCE_NAMES})
            for name in cs.SOURCE_NAMES:
                self.assertEqual((f.source / name).read_bytes(), (HERE / name).read_bytes())
            self.assertEqual(len(header['sources']), 8)
            inputs = [f.inventory / f'before-{kind}.json' for kind in mc.PROTECT_KINDS]
            inputs += [f.key, *f.userdata.values(), f.protect]
            self.assertEqual(len(inputs), 16)
            self.assertEqual(header['private_inputs'], {str(path): cc.digest(path.read_bytes()) for path in inputs})
            self.assertEqual(header['plan'], [dict(kind=p['kind'], name=p['name'], descriptor=cs.descriptor(p['body']),
                **({'private_ip': p['private_ip']} if 'private_ip' in p else {})) for p in plan])
            self.assertEqual(header['time'], dict(t0_ns=T0, c_ns=T0 + 1800 * NS, g_ns=T0 + 15 * 3600 * NS,
                h_ns=T0 + 16 * 3600 * NS, setup_ns=T0 + 10800 * NS, monotonic_t0_ns=M0 - 10 * NS))
            self.assertNotIn('provider_clock_bound_ns', header['time'])
            self.assertFalse(hasattr(f.args, 'provider_clock_bound_ms'))
            self.assertEqual(header['subnet'], dict(type='cloud', network_zone='eu-central', ip_range='10.88.0.0/24'))
            self.assertEqual(header['threat_model'], cs.THREAT_MODEL)
            self.assertIs(header['graph_reserved'], True)
            self.assertEqual(header['guard_directory'], cs.private_directory(f.work / 'guard'))
            self.assertEqual(journal.read(), dict(mode='OPEN', intents=[], owned=[], associations=[], events=[],
                                                  guard=None, active_lease=None))
            self.assertEqual(journal.checkpoint['generation'], 0)
            self.assertEqual([kind for kind, _ in f.cloud.lists], list(mc.PROTECT_KINDS))
            self.assert_no_creates(f)
            serialized = json.dumps(header)
            self.assertNotIn(TOKEN, serialized)
            self.assertNotIn('user_data"', serialized)
            for path in [journal.path, journal.path.parent / cl.ANCHOR, journal.path.parent / cl.KEY,
                         journal.path.parent / cl.LOCK]:
                self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
            self.assertEqual(stat.S_IMODE(journal.path.parent.stat().st_mode), 0o700)

    def test_full_run_initializes_arms_real_guard_and_creates_fixed_seven(self):
        with Initialization() as f:
            result = f.run()
            self.assertTrue(result['executed'])
            self.assertEqual(f.sleep_calls, [0.1])
            self.assertEqual(len(f.cloud_profiles), 1)
            self.assertIsInstance(f.cloud_profiles[0], cc.Profile)
            self.assertTrue(all(isinstance(instance, REAL_CAMPAIGN) for instance in f.constructed))
            journal = cl.Journal(f.ledger)
            f.stack.callback(journal.close)
            f.campaign(journal).validate_pins()
            state = journal.read()
            self.assertEqual(len(state['owned']), 7)
            self.assertEqual(len(state['associations']), 4)
            self.assertEqual(len(state['intents']), 16)
            self.assertTrue(all(intent['state'] == 'DONE' for intent in state['intents']))
            self.assertEqual(state['guard']['identity'], f.processes[112])
            self.assertEqual(state['events'][-1]['event'], 'provisioning-complete')
            self.assertEqual(len(f.cloud.requests), 16)
            self.assertEqual([(method, path) for method, path, _, _ in f.cloud.requests if path.count('/') == 1],
                [('POST', '/networks'), ('POST', '/firewalls'), ('POST', '/ssh_keys'), *[('POST', '/servers')] * 4])
            self.assertEqual([path for _, path, _, _ in f.cloud.requests[-4:]],
                             [f'/servers/{rid}/actions/poweron' for rid in (101, 102, 103, 104)])
            self.assertEqual(set(result['hosts']), set(provision.ROLE_NAMES))
            self.assertEqual({host['id'] for host in result['hosts'].values()}, {101, 102, 103, 104})
            self.assertEqual(json.loads((f.work / 'hosts-all.json').read_text()), result['hosts'])
            for role, host in result['hosts'].items():
                self.assertEqual(host['name'], PREFIX + '-' + role)
                self.assertEqual(host['labels']['deadline'], str(f.deadline))
            self.assertEqual(journal.header['time']['h_ns'], T0 + 16 * 3600 * NS)
            for path in journal.path.parent.iterdir():
                if path.is_file() and path.name != cl.KEY:
                    self.assertNotIn(TOKEN.encode(), path.read_bytes())

    def test_missing_registration_inputs_refuse_before_census_or_journal(self):
        for field in ('inventory', 'activation_time', 'supervisor_pid'):
            with self.subTest(field=field), Initialization() as f:
                setattr(f.args, field, None)
                with self.assertRaisesRegex(mc.GuardError, 'initialization requires'):
                    f.initialize()
                self.assert_no_creates(f)
                self.assertEqual(f.cloud.lists, [])
                self.assertFalse(f.ledger.exists())

    def test_unaccepted_threat_or_graph_policy_refuses_before_census(self):
        for field in ('accept_benign_writers', 'reserve_owned_graph'):
            with self.subTest(field=field), Initialization() as f:
                setattr(f.args, field, False)
                with self.assertRaisesRegex(mc.GuardError, 'policy must be accepted'):
                    f.initialize()
                self.assert_no_creates(f)
                self.assertEqual(f.cloud.lists, [])
                self.assertFalse(f.ledger.exists())

    def test_retired_provider_clock_flag_is_not_an_initialization_requirement(self):
        with Initialization() as f:
            self.assertFalse(hasattr(f.args, 'provider_clock_bound_ms'))
            with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as caught:
                provision.parse_args([*f.argv, '--provider-clock-bound-ms', '2'])
            self.assertEqual(caught.exception.code, 2)
            journal = f.initialize()
            f.campaign(journal).validate_pins()
            self.assertNotIn('provider_clock_bound_ns', journal.header['time'])
            self.assert_no_creates(f)

    def test_ledger_must_be_fresh_and_in_its_own_work_subdirectory(self):
        with Initialization() as f:
            f.args.creation_ledger = str(f.root / 'outside-work' / 'head.json')
            with self.assertRaisesRegex(mc.GuardError, 'fresh ledger'):
                f.initialize()
            self.assert_no_creates(f)
            self.assertEqual(f.cloud.lists, [])
        with Initialization() as f:
            journal = f.initialize()
            before = journal.path.read_bytes()
            with self.assertRaisesRegex(mc.GuardError, 'fresh ledger'):
                f.initialize()
            self.assertEqual(journal.path.read_bytes(), before)
            self.assert_no_creates(f)

    def test_malformed_baseline_ids_refuse_before_any_census_or_create(self):
        for rows in ([{'id': True}], [{'id': '9000'}], [{'id': 0}], [{'id': -1}],
                     [{'id': 9000}, {'id': 9000}], [{}]):
            with self.subTest(rows=rows), Initialization() as f:
                f.private('inventory/before-server.json', json.dumps(rows))
                with self.assertRaisesRegex(mc.GuardError, 'invalid baseline IDs'):
                    f.initialize()
                self.assertEqual(f.cloud.lists, [], 'typed baseline validation must precede the fresh census')
                self.assert_no_creates(f)
                self.assertFalse(f.ledger.exists())

    def test_boolean_baseline_id_cannot_alias_fresh_integer_one_but_integer_one_initializes(self):
        for label, saved_id, refuses in (('boolean', True, True), ('integer', 1, False)):
            with self.subTest(saved_id=label), Initialization() as f:
                fresh = f.cloud.rows['server'].pop(9000)
                fresh['id'] = 1
                f.cloud.rows['server'][1] = fresh
                f.baseline = copy.deepcopy(f.cloud.rows)
                saved = dict(fresh, id=saved_id)
                f.private('inventory/before-server.json', json.dumps([saved]))
                self.assertIs(type(fresh['id']), int)
                self.assertEqual(saved['id'], fresh['id'], 'ordinary equality must not mask the boolean ID')
                if refuses:
                    with self.assertRaisesRegex(mc.GuardError, 'invalid baseline IDs'):
                        f.initialize()
                    self.assertEqual(f.cloud.lists, [])
                    self.assertFalse(f.ledger.exists())
                else:
                    journal = f.initialize()
                    f.campaign(journal).validate_pins()
                    self.assertEqual(journal.header['baseline']['server'], [1])
                    self.assertIs(type(journal.header['baseline']['server'][0]), int)
                    self.assertEqual([kind for kind, _ in f.cloud.lists], list(mc.PROTECT_KINDS))
                    self.assertTrue(f.ledger.exists())
                self.assert_no_creates(f)

    def test_nonobject_baseline_rows_refuse_in_list_parser_before_any_census(self):
        with Initialization() as f:
            f.private('inventory/before-server.json', json.dumps([None]))
            with self.assertRaisesRegex(mc.GuardError, 'sealed baseline: expected a JSON list of objects'):
                f.initialize()
            self.assertEqual(f.cloud.lists, [])
            self.assert_no_creates(f)
            self.assertFalse(f.ledger.exists())

    def test_failed_or_changed_fresh_census_never_initializes_or_creates(self):
        for fault in ('unavailable', 'changed'):
            with self.subTest(fault=fault), Initialization() as f:
                if fault == 'unavailable':
                    f.cloud.fail_list = 'certificate'
                else:
                    f.cloud.rows['certificate'][777] = dict(id=777, name='concurrent-unrelated')
                    f.baseline = copy.deepcopy(f.cloud.rows)
                with self.assertRaisesRegex(mc.GuardError, 'unavailable|differs from sealed baseline'):
                    f.initialize()
                self.assert_no_creates(f)
                self.assertFalse(f.ledger.exists())

    def test_fresh_network_overlap_and_planned_name_collision_refuse(self):
        for collision in ('network-overlap', 'server-name'):
            with self.subTest(collision=collision), Initialization() as f:
                if collision == 'network-overlap':
                    f.cloud.rows['network'][9001]['ip_range'] = '10.88.0.0/16'
                else:
                    f.cloud.rows['server'][9000]['name'] = PREFIX + '-client'
                f.baseline = copy.deepcopy(f.cloud.rows)
                with self.assertRaisesRegex(mc.GuardError, 'overlaps campaign range|planned name already exists'):
                    f.initialize()
                self.assert_no_creates(f)
                self.assertFalse(f.ledger.exists())

    def test_activation_cutover_and_admission_times_are_fixed(self):
        for offset, cutover_delta in ((-1, 0), (1800 * NS, 0), (10 * NS, 1)):
            with self.subTest(offset=offset, cutover_delta=cutover_delta), Initialization() as f:
                f.wall = T0 + offset
                f.deadline += cutover_delta
                with self.assertRaisesRegex(mc.GuardError, 'activation/cutover must be fixed'):
                    f.initialize()
                self.assert_no_creates(f)
                self.assertFalse(f.ledger.exists())

    def test_unsupervised_or_missing_supervisor_refuses_initialization(self):
        for fault in ('unrelated', 'missing'):
            with self.subTest(fault=fault), Initialization() as f:
                if fault == 'unrelated':
                    f.processes[111]['parent'] = 1
                else:
                    f.processes.pop(110)
                with self.assertRaises((mc.GuardError, ProcessLookupError)):
                    f.initialize()
                self.assert_no_creates(f)
                self.assertFalse(f.ledger.exists())

    def test_private_baseline_key_userdata_and_protection_permissions_refuse(self):
        for relative in ('inventory/before-server.json', 'controller.pub',
                         f'cloud-init/{PREFIX}-client.user-data', 'inventory/protect.tsv'):
            with self.subTest(relative=relative), Initialization() as f:
                (f.root / relative).chmod(0o644)
                with self.assertRaisesRegex(mc.GuardError, '0600'):
                    f.initialize()
                self.assert_no_creates(f)
                self.assertFalse(f.ledger.exists())

    def test_private_input_drift_after_initialization_refuses_real_campaign_pins(self):
        for source in ('baseline', 'key', 'userdata', 'protect', 'profile'):
            with self.subTest(source=source), Initialization() as f:
                journal = f.initialize()
                path = {'baseline': f.inventory / 'before-server.json', 'key': f.key,
                        'userdata': f.userdata['client'], 'protect': f.protect, 'profile': f.config}[source]
                path.write_bytes(path.read_bytes() + b'\n')
                with self.assertRaisesRegex(mc.GuardError, 'pinned private campaign input changed|sealed cloud profile changed'):
                    f.campaign(journal).validate_pins()
                self.assert_no_creates(f)
                self.assertEqual(journal.read()['owned'], [])

    def test_plan_requires_a_new_public_ssh_key_and_private_userdata(self):
        for fault in ('reuse-key', 'private-key', 'missing-userdata', 'symlink-userdata'):
            with self.subTest(fault=fault), Initialization() as f:
                if fault == 'reuse-key':
                    f.args.ssh_public_key, f.args.ssh_key_name = None, 'existing-key'
                elif fault == 'private-key':
                    f.key.write_text('-----BEGIN OPENSSH PRIVATE KEY-----\nsynthetic\n')  # secretscan:allow synthetic invalid-key rejection fixture; no key material
                elif fault == 'missing-userdata':
                    f.userdata['client'].unlink()
                else:
                    f.userdata['client'].unlink()
                    f.userdata['client'].symlink_to(f.userdata['node1'])
                with self.assertRaises((mc.GuardError, OSError)):
                    f.plan()
                self.assert_no_creates(f)
                self.assertFalse(f.work.exists())

    def test_run_rejects_partial_unpinned_or_unbounded_invocations_before_cloud(self):
        changes = [('role', ['client']), ('skip_shared', True), ('client_type', 'ccx33'),
                   ('server_type', 'ccx33'), ('location', 'nbg1'), ('image', 'ubuntu-24.04'),
                   ('guard_wait_seconds', 0), ('guard_wait_seconds', 61), ('guard_wait_seconds', float('nan'))]
        for field, value in changes:
            with self.subTest(field=field, value=value), Initialization() as f:
                setattr(f.args, field, value)
                with self.assertRaises(mc.GuardError):
                    f.run()
                self.assert_no_creates(f)
                self.assertEqual(f.cloud_profiles, [])
                self.assertEqual(f.cloud.lists, [])
                self.assertFalse(f.ledger.exists())

    def test_run_rejects_controller_credential_in_deployment_before_journal(self):
        for field in ('userdata', 'key'):
            with self.subTest(field=field), Initialization() as f:
                if field == 'userdata':
                    f.userdata['node3'].write_text('#cloud-config\n# ' + TOKEN + '\n')
                else:
                    f.key.write_text(PUBLIC_KEY + ' ' + TOKEN + '\n')
                with self.assertRaisesRegex(mc.GuardError, 'controller credential found in deployment input'):
                    f.run()
                self.assert_no_creates(f)
                self.assertEqual(f.cloud.lists, [])
                self.assertFalse(f.ledger.exists())
                self.assertFalse((f.work / 'guard').exists())

    def test_initialization_fsync_failure_forbids_creation_and_head_publication(self):
        with Initialization() as f:
            with mock.patch.object(cl.os, 'fsync', side_effect=OSError('synthetic initialization sync failure')):
                with self.assertRaisesRegex(OSError, 'initialization sync failure'):
                    f.initialize()
            self.assert_no_creates(f)
            self.assertFalse(f.ledger.exists())


class InitialHeartbeatPublicationTest(unittest.TestCase):
    def public_race(self, f, outcome):
        f.args.guard_wait_seconds = 60
        started = f.mono
        heartbeat = f.work / 'guard' / 'heartbeat.json'
        paused, release, finished = threading.Event(), threading.Event(), threading.Event()
        guard_errors, missing_reads, healthy_reads, io_reads, sleeps = [], [], [], [], []
        guard_thread, observer = None, None
        original_open = os.open
        guard_name = 'initial-heartbeat-guard'
        def identity(pid=None):
            if pid is None:
                pid = 112 if threading.current_thread().name == guard_name else 111
            if pid not in f.processes:
                raise ProcessLookupError('synthetic process is gone')
            return copy.deepcopy(f.processes[pid])
        def opened(path, flags, *args, **kwargs):
            candidate = Path(os.fsdecode(path))
            is_guard = threading.current_thread().name == guard_name
            if (is_guard and candidate.parent == heartbeat.parent and candidate.name.startswith('.receipt-')
                    and not paused.is_set()):
                self.assertFalse(heartbeat.exists())
                paused.set()
                if not release.wait(60):
                    raise AssertionError('test did not release the first real heartbeat publication')
            if not is_guard and candidate == heartbeat and paused.is_set():
                if outcome == 'io-error':
                    io_reads.append(str(candidate))
                    raise OSError(errno.EIO, 'synthetic heartbeat I/O failure', str(candidate))
                if outcome == 'permission':
                    io_reads.append(str(candidate))
                    raise PermissionError(errno.EACCES, 'synthetic heartbeat permission failure', str(candidate))
            try:
                fd = original_open(path, flags, *args, **kwargs)
                if not is_guard and candidate == heartbeat:
                    healthy_reads.append(str(candidate))
                return fd
            except FileNotFoundError as error:
                if not is_guard and candidate == heartbeat:
                    missing_reads.append(error.filename)
                raise
        def guard_arm():
            try:
                f.campaign(f.guard_journal).arm_guard()
            except BaseException as error:
                guard_errors.append(error)
            finally:
                finished.set()
        def sleep(seconds):
            nonlocal guard_thread, observer
            sleeps.append(seconds)
            self.assertEqual(seconds, 0.1, 'healthy provider actions must not add provisioning waits')
            if len(sleeps) == 1:
                self.assertEqual(f.cloud.requests, [])
                f.guard_journal = cl.Journal(f.ledger)
                f.stack.callback(f.guard_journal.close)
                f.header = f.guard_journal.header
                observer = cl.Journal(f.ledger)
                f.stack.callback(observer.close)
                self.assertIsNone(observer.read()['guard'])
                self.assertFalse(heartbeat.exists())
                # Simulate delayed initial guard scheduling, not a new wait
                # allowance: registration occurs 59.8s into the same 60s wait.
                f.wall += 598 * NS // 10
                f.mono += 598 * NS // 10
                guard_thread = threading.Thread(target=guard_arm, name=guard_name, daemon=True)
                guard_thread.start()
                self.assertTrue(paused.wait(60), 'guard did not reach the first heartbeat filesystem publication')
                registered = observer.read_if_available()
                self.assertIsNotNone(registered, 'registration must be committed and its operation lock released')
                self.assertEqual(registered['guard']['identity'], f.processes[112])
                self.assertEqual(registered['guard']['binding'], observer.binding)
                self.assertEqual(registered['mode'], 'OPEN')
                self.assertFalse(heartbeat.exists())
                self.assertFalse(finished.is_set())
                if outcome == 'other-missing':
                    f.userdata['client'].unlink()
                return
            self.assertIn(outcome, ('published', 'published-late', 'deadline'), 'unrelated filesystem failures must not be retried')
            self.assertEqual(len(sleeps), 2, 'initial arming must not reset its absolute deadline')
            self.assertEqual(missing_reads, [str(heartbeat)])
            self.assertFalse(heartbeat.exists())
            self.assertTrue(guard_thread.is_alive())
            self.assertEqual(f.cloud.requests, [])
            if outcome in ('published', 'published-late'):
                elapsed = NS // 10 if outcome == 'published' else 3 * NS // 10
                f.wall += elapsed
                f.mono += elapsed
                release.set()
                self.assertTrue(finished.wait(60), 'guard did not finish its first heartbeat')
                self.assertEqual(guard_errors, [])
                self.assertTrue(heartbeat.exists())
                self.assertFalse((heartbeat.parent / 'stop.json').exists())
            else:
                # Keep that heartbeat unpublished beyond the original bound.
                # A reset-on-FileNotFound implementation would sleep again.
                f.wall += 3 * NS // 10
                f.mono += 3 * NS // 10
        def before_request(method, path, body, seconds):
            self.assertEqual(outcome, 'published')
            self.assertTrue(finished.is_set())
            self.assertTrue(heartbeat.exists(), 'no request may precede the first heartbeat publication')
        f.cloud.before_request = before_request
        result = None
        try:
            with mock.patch.object(cs, 'identity', side_effect=identity), \
                    mock.patch.object(os, 'open', side_effect=opened), \
                    mock.patch.object(cc, 'Cloud', side_effect=f.cloud_factory), \
                    mock.patch.object(cs, 'Campaign', side_effect=f.campaign), \
                    mock.patch.object(provision.time, 'sleep', side_effect=sleep):
                try:
                    def run():
                        return provision.run(f.args, now=f.wall // NS, out=io.StringIO(),
                            runner=lambda *a, **kw: self.fail('guarded initialization must never invoke legacy commands'))
                    if outcome == 'published':
                        result = run()
                    elif outcome in ('deadline', 'published-late'):
                        with self.assertRaisesRegex(mc.GuardError, 'live supervised guard did not arm; no create permitted'):
                            run()
                    elif outcome in ('io-error', 'permission'):
                        expected = OSError if outcome == 'io-error' else PermissionError
                        with self.assertRaises(expected) as caught:
                            run()
                        self.assertEqual(caught.exception.errno, errno.EIO if outcome == 'io-error' else errno.EACCES)
                        self.assertEqual(caught.exception.filename, str(heartbeat))
                    else:
                        with self.assertRaises(FileNotFoundError) as caught:
                            run()
                        self.assertEqual(caught.exception.filename, str(f.userdata['client']))
                        self.assertNotEqual(caught.exception.filename, str(heartbeat))
                finally:
                    release.set()
                    if guard_thread is not None:
                        guard_thread.join(60)
                        self.assertFalse(guard_thread.is_alive(), 'real guard arming thread did not leave its publication boundary')
        finally:
            release.set()
        self.assertEqual(guard_errors, [])
        self.assertTrue(paused.is_set())
        self.assertTrue(finished.is_set())
        self.assertEqual(f.args.guard_wait_seconds, 60)
        if outcome == 'published':
            self.assertEqual(sleeps, [0.1, 0.1])
            self.assertEqual(missing_reads, [str(heartbeat)])
            self.assertEqual(f.mono - started, 599 * NS // 10)
            self.assertLess(f.mono, started + 60 * NS)
            self.assertTrue(result['executed'])
            self.assertEqual(len(f.cloud.requests), 16)
            creates = [(method, path) for method, path, _, _ in f.cloud.requests if path.count('/') == 1]
            self.assertEqual(creates, [('POST', '/networks'), ('POST', '/firewalls'), ('POST', '/ssh_keys'),
                                      *[('POST', '/servers')] * 4])
            state = observer.read()
            self.assertEqual(len(state['owned']), 7)
            self.assertEqual(len(state['intents']), 16)
            self.assertTrue(all(i['state'] == 'DONE' for i in state['intents']))
            self.assertEqual(state['events'][-1]['event'], 'provisioning-complete')
            self.assertFalse((heartbeat.parent / 'stop.json').exists())
            self.assertEqual(json.loads((f.work / 'hosts-all.json').read_text()), result['hosts'])
        else:
            self.assertEqual(f.cloud.requests, [])
            self.assertEqual(f.cloud.rows, f.baseline)
            self.assertFalse((f.work / 'hosts-all.json').exists())
            self.assertTrue((heartbeat.parent / 'stop.json').exists())
            state = observer.read()
            self.assertEqual(state['intents'], [])
            self.assertEqual(state['owned'], [])
            if outcome == 'deadline':
                self.assertEqual(sleeps, [0.1, 0.1])
                self.assertEqual(missing_reads, [str(heartbeat), str(heartbeat)])
                self.assertEqual(f.mono - started, 601 * NS // 10)
                self.assertEqual(healthy_reads, [])
            elif outcome == 'published-late':
                self.assertEqual(sleeps, [0.1, 0.1])
                self.assertEqual(missing_reads, [str(heartbeat)])
                self.assertTrue(healthy_reads, 'the late successful observation must actually read a published heartbeat')
                self.assertEqual(f.mono - started, 601 * NS // 10)
                self.assertGreater(f.mono, started + 60 * NS)
                receipt = json.loads(cc.private_bytes(heartbeat))
                self.assertEqual(receipt['identity'], f.processes[112])
                self.assertEqual(receipt['binding'], observer.binding)
                self.assertEqual(receipt['challenge'], state['guard']['challenge'])
                self.assertTrue(observer.is_ancestor(receipt['checkpoint']))
                self.assertLessEqual(f.mono - receipt['monotonic_ns'], 5 * NS)
            else:
                self.assertEqual(sleeps, [0.1])
                self.assertEqual(missing_reads, [])
                self.assertEqual(io_reads, [] if outcome == 'other-missing' else [str(heartbeat)])

    def test_public_initial_producer_waits_for_first_heartbeat_only_within_the_original_sixty_seconds(self):
        for outcome in ('published', 'deadline', 'published-late'):
            with self.subTest(outcome=outcome), Initialization() as f:
                self.public_race(f, outcome)

    def test_initial_arming_does_not_retry_other_heartbeat_io_errors_or_missing_pinned_inputs(self):
        for outcome in ('io-error', 'permission', 'other-missing'):
            with self.subTest(outcome=outcome), Initialization() as f:
                self.public_race(f, outcome)


if __name__ == '__main__':
    unittest.main()
