"""RFC283's fixed seven-resource campaign state machine. Controller only."""
import copy
import datetime as dt
import errno
import fnmatch
import json
import os
from pathlib import Path
import re
import select
import signal
import stat
import time
import uuid

import campaign_cloud as cc
import creation_ledger as cl
import mn_common as mc

KINDS = ('server', 'firewall', 'ssh-key', 'network')
NS = 1_000_000_000
THREAT_MODEL = 'trusted-controller-benign-cooperative-project-writers/v1'
SOURCE_DIR = Path(__file__).absolute().parent
SOURCE_NAMES = ('provision.py', 'teardown_guard.py', 'primary_ip_check.py', 'mn_common.py',
                'inventory.py', 'campaign_safety.py', 'campaign_cloud.py', 'creation_ledger.py')


def source_pins():
    return {str(SOURCE_DIR / name): cc.digest((SOURCE_DIR / name).read_bytes()) for name in SOURCE_NAMES}


def recover_closed_journal(path, evidence_directory, *, expected_campaign=None, expected_prefix=None,
                           expected_deadline=None):
    """Explicit local repair only; no provider mutation or creator resumption."""
    def authorize(header, binding, checkpoint, headed_state):
        cc.need((expected_campaign is None or expected_campaign == header['campaign'])
                and (expected_prefix is None or expected_prefix == header['prefix']), 'guard campaign binding differs')
        if expected_deadline is not None:
            cc.need(expected_deadline == header['time']['g_ns'], 'guard cutover deadline changed')
        class View:
            pass
        view = View()
        view.header, view.binding = header, binding
        profile = cc.Profile(header['context']['context'], header['context']['config'])
        campaign = Campaign(view, cc.Cloud(profile, header['context']))
        campaign.validate_pins()
        actor = identity()
        cc.need(actor != header['producer'] and supervised(actor, header['supervisor']),
                'journal repair requires the pinned supervised recovery guard')
        guard = headed_state.get('guard')
        cc.need(guard is None or not live(guard['identity']), 'old guard is still live during journal repair')
        campaign.quiesce_producer(drain_seconds=campaign._producer_drain(headed_state.get('active_lease')),
                                   termination_deadline_ns=campaign._termination_deadline(headed_state.get('active_lease')))
        cc.need(not live(header['producer']), 'producer is not quiescent during journal repair')
        return dict(recovery_guard=actor, supervisor=header['supervisor'], producer_quiesced=True,
                    old_guard_gone=True, boot_id=header['boot_id'], sources_sha256=cl.sha256(header['sources']),
                    immutable_time=header['time'], headed_checkpoint=checkpoint)

    def close_state(candidate, receipt):
        headed = {i['sequence']: i for i in receipt['headed_state']['intents']}
        for intent in candidate['intents']:
            previous = headed.get(intent['sequence'])
            previous_state = previous['state'] if previous is not None else None
            if previous_state in {'SENDING', 'UNKNOWN'} or intent['state'] == 'UNKNOWN':
                intent['state'] = 'UNKNOWN'
            elif previous_state in {None, 'PREPARED'} and intent['state'] in {'PREPARED', 'SENDING'}:
                # Pinned synchronous code cannot send before a published/synced
                # SENDING head. A known non-create target is not a new resource.
                cc.need(intent.get('ack') is None and not (intent['operation'] == 'create'
                        and intent.get('resource_id') is not None), 'contradictory pre-dispatch recovery evidence')
                intent['state'] = 'NOT_DISPATCHED'
        candidate['mode'], candidate['active_lease'] = 'CLOSING', None
        candidate['events'].append(dict(event='guard-journal-recovery',
            archive=receipt['archive'], headed_checkpoint=receipt['headed_checkpoint'],
            owner_proof=receipt['authorization']))
        return candidate

    return cl.Journal.recover_closed(path, evidence_directory=evidence_directory,
                                    authorize=authorize, close_state=close_state)


def identity(pid=None):
    pid = os.getpid() if pid is None else pid
    cc.need(type(pid) is int and pid > 0, 'invalid controller PID')
    fields = Path(f'/proc/{pid}/stat').read_text().rsplit(')', 1)[1].split()
    return dict(pid=pid, starttime=fields[19], parent=int(fields[1]),
                argv_sha256=cc.digest(Path(f'/proc/{pid}/cmdline').read_bytes()))


def live(expected):
    if (not isinstance(expected, dict) or set(expected) != {'pid', 'starttime', 'parent', 'argv_sha256'}
            or type(expected['pid']) is not int or expected['pid'] <= 0
            or type(expected['parent']) is not int or expected['parent'] < 0
            or not isinstance(expected['starttime'], str) or not expected['starttime'].isdigit()
            or not isinstance(expected['argv_sha256'], str)
            or re.fullmatch(r'[0-9a-f]{64}', expected['argv_sha256']) is None):
        return False
    try:
        return identity(expected['pid']) == expected
    except (OSError, ValueError, KeyError, IndexError):
        return False


def supervised(process, supervisor):
    if (not live(process) or not live(supervisor) or supervisor['pid'] <= 1
            or process['pid'] == supervisor['pid']):
        return False
    parent = process['parent']
    for _ in range(8):
        if parent == supervisor['pid']:
            return True
        if parent <= 1:
            return False
        try:
            parent = identity(parent)['parent']
        except (OSError, ValueError, IndexError):
            return False
    return False


def private_directory(path):
    path = Path(path).absolute()
    mc.require_outside_repo(path, 'guard directory')
    info = path.lstat()
    cc.need(stat.S_ISDIR(info.st_mode) and info.st_uid == os.getuid()
            and stat.S_IMODE(info.st_mode) == 0o700, 'unsafe private guard directory')
    return dict(path=str(path), device=info.st_dev, inode=info.st_ino)


def atomic_private(path, value):
    path = Path(path)
    private_directory(path.parent)
    temporary = path.parent / ('.receipt-' + uuid.uuid4().hex)
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    try:
        data = cc.wire_json(value) + b'\n'
        with os.fdopen(os.dup(fd), 'wb') as stream:
            stream.write(data)
            stream.flush()
        os.fsync(fd)
        os.replace(temporary, path)
        directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        os.close(fd)


def timestamp(text):
    cc.need(isinstance(text, str), 'missing provider creation timestamp')
    cc.need(re.fullmatch(r'[0-9]{4}-[0-9]{2}-[0-9]{2}[Tt](?:[01][0-9]|2[0-3]):[0-9]{2}:[0-9]{2}'
                         r'(?:\.[0-9]+)?(?:[Zz]|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])', text) is not None,
            'invalid provider creation timestamp')
    try:
        stamp = dt.datetime.fromisoformat(text.upper().replace('Z', '+00:00'))
        cc.need(stamp.tzinfo is not None and stamp.utcoffset() is not None, 'unqualified provider creation timestamp')
        epoch = dt.datetime(1970, 1, 1, tzinfo=dt.timezone.utc)
        delta = stamp - epoch
        return ((delta.days * 86400 + delta.seconds) * NS + delta.microseconds * 1000)
    except (ValueError, OverflowError):
        raise mc.GuardError('invalid provider creation timestamp') from None


def descriptor(body):
    result = copy.deepcopy(body)
    if 'user_data' in result:
        result['user_data_sha256'] = cc.digest(result.pop('user_data').encode())
    return result


def resource_key(kind):
    return {'ssh-key': 'ssh_key'}.get(kind, kind)


def canonical(kind, item):
    if kind == 'server':
        net = item.get('public_net')
        cc.need(isinstance(net, dict) and isinstance(net.get('firewalls'), list)
                and all(isinstance(f, dict) and type(f.get('id')) is int and f['id'] > 0 for f in net['firewalls'])
                and isinstance(item.get('image'), dict) and type(item['image'].get('id')) is int,
                'missing or invalid server canonical identity')
        return dict(server_type=item['server_type']['name'], location=item['location']['name'],
                    image=item['image']['id'], firewalls=sorted(f['id'] for f in net['firewalls']))
    if kind == 'network':
        return dict(ip_range=item['ip_range'])
    if kind == 'ssh-key':
        cc.need(isinstance(item.get('public_key'), str), 'invalid SSH key identity')
        return dict(public_key=item['public_key'].strip())
    if kind == 'firewall':
        return dict(rules=sorted((dict(direction=r['direction'], protocol=r['protocol'], port=r.get('port'),
                    source_ips=sorted(r.get('source_ips', [])), destination_ips=sorted(r.get('destination_ips', [])),
                    description=r.get('description', '')) for r in item['rules']), key=cc.wire_json))
    raise mc.GuardError('unplanned resource kind')


def expected_canonical(kind, body):
    if kind == 'server':
        return dict(server_type=body['server_type'], location=body['location'], image=body['image'],
                    firewalls=sorted(f['firewall'] for f in body['firewalls']))
    return canonical(kind, body)


def action_ids(reply):
    payload = reply.get('payload')
    cc.need(isinstance(payload, dict), 'provider acknowledgement is not an object')
    next_actions, others = payload.get('next_actions', []), payload.get('actions', [])
    cc.need(isinstance(next_actions, list) and isinstance(others, list), 'invalid action arrays')
    actions = ([payload['action']] if payload.get('action') is not None else []) + next_actions + others
    cc.need(all(isinstance(a, dict) and type(a.get('id')) is int
            and a['id'] > 0 for a in actions), 'invalid creation action evidence')
    result = [a['id'] for a in actions]
    cc.need(len(result) == len(set(result)), 'duplicate creation action IDs')
    return result


def known_action_ids(reply):
    """Retain parseable IDs even if another acknowledgement field is invalid."""
    payload = reply.get('payload')
    if not isinstance(payload, dict):
        return []
    actions = [payload.get('action')]
    for name in ('actions', 'next_actions'):
        if isinstance(payload.get(name), list):
            actions.extend(payload[name])
    return sorted({a['id'] for a in actions if isinstance(a, dict) and type(a.get('id')) is int and a['id'] > 0})


def acknowledgement_actions(reply, kind, operation):
    payload = reply.get('payload')
    cc.need(reply.get('json_valid') is True, 'mutation response is not valid JSON')
    if operation == 'delete' and kind != 'server':
        cc.need(reply.get('status') == 204 and reply.get('body_empty') is True and payload is None,
                'synchronous deletion acknowledgement differs from pinned endpoint')
        return []
    expected_status = 200 if operation == 'delete' else 201
    cc.need(reply.get('status') == expected_status and isinstance(payload, dict), 'mutation acknowledgement unavailable')
    if operation != 'create' or kind == 'server':
        cc.need(isinstance(payload.get('action'), dict), 'required mutation action identity unavailable')
    if operation == 'create' and kind == 'server':
        cc.need(isinstance(payload.get('next_actions'), list), 'server next-action identities unavailable')
    return action_ids(reply)


def acknowledgement_records(reply):
    payload = reply.get('payload')
    if not isinstance(payload, dict):
        return []
    return ([payload['action']] if payload.get('action') is not None else []) + payload.get('next_actions', []) + payload.get('actions', [])


def observe_action(intent, action, pending, unread):
    cc.need(isinstance(action, dict) and type(action.get('id')) is int and action['id'] > 0
            and action_for(action, intent['kind'], intent['resource_id']), 'provider action resource binding differs')
    status = action.get('status')
    cc.need(isinstance(status, str) and status in {'running', 'success', 'error'}, 'invalid known action status')
    if status == 'running':
        pending.add(action['id'])
        pending.update(unread)
    else:
        pending.discard(action['id'])
    if status == 'error':
        intent['operation_failed'] = True
        pending.update(unread)
    return status


def action_for(action, kind, rid):
    resources = action.get('resources')
    if not isinstance(resources, list) or not all(isinstance(r, dict) and type(r.get('id')) is int
            and r['id'] > 0 and isinstance(r.get('type'), str) for r in resources):
        return False
    keys = [(r['type'], r['id']) for r in resources]
    return len(keys) == len(set(keys)) and (kind.replace('-', '_'), rid) in keys


def protection_policy(names=(), ids=(), by_kind=None):
    by_kind = {} if by_kind is None else by_kind
    cc.need(isinstance(by_kind, dict) and set(by_kind) <= set(mc.PROTECT_KINDS), 'invalid typed protection policy')
    cc.need(isinstance(names, (list, tuple)) and all(isinstance(n, str) and n for n in names), 'invalid protection names')
    cc.need(isinstance(ids, (list, tuple)) and all(type(i) is int and i > 0 for i in ids), 'invalid protection IDs')
    typed = {}
    for kind in mc.PROTECT_KINDS:
        values = by_kind.get(kind, [])
        cc.need(isinstance(values, (list, tuple)) and all(type(i) is int and i > 0 for i in values),
                'invalid typed protection IDs')
        typed[kind] = sorted(set(values))
    return dict(names=sorted(set(names)), ids=sorted(set(ids)), by_kind=typed)


class Campaign:
    def __init__(self, journal, cloud, clock=time.time_ns, monotonic=time.monotonic_ns):
        self.journal, self.cloud, self.monotonic = journal, cloud, monotonic
        self.header = journal.header
        self.clock = lambda: max(clock(), self.header['time']['t0_ns'] + monotonic() - self.header['time']['monotonic_t0_ns'])
        self._validate_header()

    def _validate_header(self):
        h = self.header
        self._protection({})
        cc.need(h['threat_model'] == THREAT_MODEL and h['graph_reserved'] is True, 'unaccepted campaign threat model')
        cc.need(set(h['sources']) == {str(SOURCE_DIR / n) for n in SOURCE_NAMES}, 'incomplete fixed guard source population')
        t = h['time']
        cc.need(set(t) == {'t0_ns', 'c_ns', 'g_ns', 'h_ns', 'setup_ns', 'monotonic_t0_ns'}
                and all(type(v) is int and v >= 0 for v in t.values()), 'invalid sealed campaign time policy')
        cc.need(t['h_ns'] - t['t0_ns'] == 16 * 3600 * NS and t['c_ns'] == t['t0_ns'] + 1800 * NS
                and t['g_ns'] == t['h_ns'] - 3600 * NS and t['setup_ns'] == t['t0_ns'] + 10800 * NS,
                'campaign deadline or setup budget changed')
        cc.need(set(h['baseline']) == set(mc.PROTECT_KINDS), 'incomplete ten-kind baseline')
        for ids in h['baseline'].values():
            cc.need(isinstance(ids, list) and all(type(i) is int and i > 0 for i in ids)
                    and ids == sorted(set(ids)), 'invalid typed baseline IDs')
        plan = h['plan']
        cc.need(isinstance(plan, list) and len(plan) == 7
                and [p['kind'] for p in plan].count('server') == 4
                and all([p['kind'] for p in plan].count(k) == 1 for k in ('network', 'firewall', 'ssh-key')),
                'campaign is not the seven-resource plan')
        names = [p['name'] for p in plan]
        cc.need(len(names) == len(set(names)) and all(mc.owned_name(n, h['prefix']) for n in names), 'invalid plan names')
        for entry in plan:
            body = entry.get('descriptor')
            labels = body.get('labels') if isinstance(body, dict) else None
            cc.need(isinstance(labels, dict) and labels.get('campaign') == h['campaign']
                    and labels.get('deadline') == str(t['g_ns'] // NS) and 'intent' not in labels,
                    'plan campaign/deadline labels differ from immutable policy')
            if entry['kind'] == 'server':
                cc.need(body.get('server_type') == 'ccx43' and body.get('location') == 'fsn1'
                        and type(body.get('image')) is int and body['image'] == 161547269
                        and body.get('start_after_create') is False
                        and isinstance(body.get('public_net'), dict)
                        and set(body['public_net']) == {'enable_ipv4', 'enable_ipv6'}
                        and body['public_net']['enable_ipv4'] is True and body['public_net']['enable_ipv6'] is False,
                        'server plan differs from four ccx43/fsn1 pinned-image policy')
        cc.need(h['boot_id'] == Path('/proc/sys/kernel/random/boot_id').read_text().strip(), 'controller boot changed')
        cc.need(private_directory(h['guard_directory']['path']) == h['guard_directory'], 'guard directory identity changed')

    def validate_pins(self):
        for path, expected in self.header['sources'].items():
            cc.need(not Path(path).is_symlink() and cc.digest(Path(path).read_bytes()) == expected, 'pinned guard source changed')
        for path, expected in self.header['private_inputs'].items():
            cc.need(cc.digest(cc.private_bytes(path)) == expected, 'pinned private campaign input changed')
        self.cloud.profile.revalidate(self.header['context'])
        self._validate_header()

    def _records(self, state):
        self._protection(state)
        cc.need(isinstance(state.get('mode'), str) and state['mode'] in cl.MODES, 'invalid campaign mode')
        intents = state['intents']
        cc.need(all(isinstance(r, dict) and type(r.get('sequence')) is int for r in intents)
                and [r['sequence'] for r in intents] == list(range(len(intents))), 'invalid intent sequence')
        for intent in intents:
            cc.need(isinstance(intent.get('kind'), str) and intent['kind'] in KINDS
                    and isinstance(intent.get('operation'), str) and intent['operation'] in {'create', 'subnet', 'attach', 'poweron', 'delete'}
                    and isinstance(intent.get('state'), str)
                    and intent['state'] in {'PREPARED', 'SENDING', 'UNKNOWN', 'ACKNOWLEDGED', 'DONE', 'FAILED', 'NOT_DISPATCHED'},
                    'invalid typed intent operation/state')
            cc.need(isinstance(intent.get('name'), str) and mc.owned_name(intent['name'], self.header['prefix'])
                    and isinstance(intent.get('request_sha256'), str)
                    and re.fullmatch(r'[0-9a-f]{64}', intent['request_sha256']) is not None,
                    'invalid pinned intent identity')
            cc.need(isinstance(intent.get('actions'), list)
                    and all(type(a) is int and a > 0 for a in intent['actions'])
                    and len(intent['actions']) == len(set(intent['actions'])), 'invalid known action IDs')
            cc.need(intent.get('resource_id') is None or type(intent['resource_id']) is int and intent['resource_id'] > 0,
                    'invalid intent resource ID')
            cc.need(all(type(intent.get(name, False)) is bool for name in
                        ('acknowledgement_validated', 'required_action_identified')), 'invalid action evidence flags')
            if 'send_lease' in intent:
                lease = intent['send_lease']
                cc.need(isinstance(lease, dict) and set(lease) == {'intent', 'actor', 'deadline_ns', 'monotonic_deadline_ns'}
                        and type(lease['intent']) is int and lease['intent'] == intent['sequence']
                        and lease['actor'] == intent['producer']
                        and type(lease['deadline_ns']) is int and type(lease['monotonic_deadline_ns']) is int
                        and type(intent.get('send_generation')) is int and intent['send_generation'] > 0,
                        'invalid retained send lease')
            if intent['operation'] == 'create':
                cc.need(isinstance(intent.get('nonce'), str) and re.fullmatch(r'[0-9a-f]{32}', intent['nonce']) is not None
                        and isinstance(intent.get('descriptor'), dict)
                        and isinstance(intent['descriptor'].get('labels'), dict)
                        and intent['descriptor']['labels'].get('intent') == intent['nonce']
                        and intent.get('producer') == self.header['producer'], 'invalid create provenance binding')
                cc.need(isinstance(intent.get('pre_ids'), dict) and set(intent['pre_ids']) == set(mc.PROTECT_KINDS)
                        and all(isinstance(ids, list) and all(type(i) is int and i > 0 for i in ids)
                                and ids == sorted(set(ids)) for ids in intent['pre_ids'].values()), 'invalid pre-call typed census')
                cc.need(type(intent.get('observed_start_ns')) is int and type(intent.get('observed_end_ns')) is int
                        and intent['observed_start_ns'] <= intent['observed_end_ns'], 'invalid pre-call observation bracket')
        keys = []
        for record in state['owned']:
            cc.need(isinstance(record, dict) and record.get('kind') in KINDS and type(record.get('id')) is int and record['id'] > 0,
                    'invalid owned typed ID')
            seq = record.get('intent')
            cc.need(type(seq) is int and 0 <= seq < len(intents), 'missing owning intent')
            intent = intents[seq]
            recovery = intent.get('recovery')
            recovered = (intent.get('recovered') is True and isinstance(recovery, dict)
                         and recovery.get('origin') == 'conditional-nonce-recovery'
                         and isinstance(recovery.get('candidate'), dict))
            if recovered:
                candidate = recovery['candidate']
                recovered = (type(candidate.get('id')) is int and candidate['id'] == record['id']
                             and candidate.get('name') == record.get('name')
                             and candidate.get('labels') == record.get('labels')
                             and candidate.get('created') == record.get('created')
                             and canonical(record['kind'], candidate) == record.get('canonical'))
            cc.need(intent['operation'] == 'create' and intent['kind'] == record['kind']
                    and intent.get('resource_id') == record['id']
                    and (intent.get('ack') is not None or recovered), 'owned ID lacks creation provenance')
            cc.need(record.get('name') == intent['name'] and record.get('labels') == intent['descriptor']['labels']
                    and record.get('canonical') == expected_canonical(record['kind'], intent['descriptor'])
                    and isinstance(record.get('created'), str), 'owned canonical provenance differs')
            timestamp(record['created'])
            cc.need(isinstance(record.get('implicit_ips'), list)
                    and all(type(i) is int and i > 0 for i in record['implicit_ips'])
                    and len(record['implicit_ips']) == len(set(record['implicit_ips'])), 'invalid subordinate typed IDs')
            keys.append((record['kind'], record['id']))
        cc.need(len(keys) == len(set(keys)), 'duplicate owned typed IDs')
        return intents

    def census(self, seconds=20, state=None, source='census'):
        rows = {}
        for kind in mc.PROTECT_KINDS:
            rows[kind] = self.cloud.list(kind, seconds)
            if state is not None:
                self._observe_inventory(state, kind, rows[kind], source)
        return rows

    def _phase_budget(self, state, create=False):
        before = (state['mode'], len(state['events']))
        provisioning = state['mode'] == 'OPEN'
        started, effective_started = self.monotonic(), self.clock()
        seconds = self.lease_seconds(state, create=create, provisioning=provisioning)
        times = self.header['time']
        phase_end = None
        if provisioning:
            phase_end = (times['c_ns'] if create else min(times['g_ns'], times['setup_ns'])) - 2 * NS
        elif state['mode'] != 'INCIDENT':
            phase_end = times['h_ns'] - 2 * NS
        end = started + int(seconds * NS)
        effective_end = effective_started + int(seconds * NS)
        phase_end = effective_end if phase_end is None else min(effective_end, phase_end)
        if before != (state['mode'], len(state['events'])):
            self.journal.commit(state)
        remaining = min(20, (end - self.monotonic()) / NS)
        if phase_end is not None:
            remaining = min(remaining, (phase_end - self.clock()) / NS)
        cc.need(remaining > 0, 'read-only phase budget exhausted')
        return cc.CallBudget(remaining, end, self.monotonic, phase_end, self.clock)

    def _send_lease(self, state, intent, create=False):
        budget = self._phase_budget(state, create=create)
        return dict(intent=intent, actor=identity(), deadline_ns=budget.phase_deadline_ns,
                    monotonic_deadline_ns=budget.deadline_ns)

    def _send_budget(self, state):
        lease = state['active_lease']
        end, phase_end = lease['monotonic_deadline_ns'], lease['deadline_ns']
        remaining = min(20, (end - self.monotonic()) / NS, (phase_end - self.clock()) / NS)
        cc.need(remaining > 0, 'absolute send lease expired before dispatch')
        return cc.CallBudget(remaining, end, self.monotonic, phase_end, self.clock)

    def lease_seconds(self, state, create=False, provisioning=False):
        now, t = self.clock(), self.header['time']
        if provisioning:
            cc.need(state['mode'] == 'OPEN', 'provisioning admission closed')
            end = t['c_ns'] if create else min(t['g_ns'], t['setup_ns'])
            available = end - now - 2 * NS
            cc.need(available > 0, 'provisioning budget exhausted')
        else:
            cc.need(state['mode'] in {'CLOSING', 'INCIDENT'}, 'cleanup requires closed admission')
            self._lifetime_breached(state)
            available = t['h_ns'] - now - 2 * NS
            if available <= 0:
                if state['mode'] != 'INCIDENT':
                    state['mode'] = 'INCIDENT'
                    state['events'].append(dict(event='cleanup-reserve-exhausted', unix_ns=now,
                        hard_lifetime_met=False, provisioning_reopened=False))
                available = 20 * NS
        return min(20.0, available / NS)

    def guard_live(self, state):
        guard = state.get('guard')
        cc.need(isinstance(guard, dict) and {'binding', 'identity', 'challenge', 'time', 'supervisor'} <= set(guard)
                and guard['binding'] == self.journal.binding and guard['time'] == self.header['time']
                and guard['supervisor'] == self.header['supervisor'], 'missing or stale guard arming')
        cc.need(supervised(guard['identity'], self.header['supervisor']), 'guard is not live and supervised')
        heartbeat = json.loads(cc.private_bytes(Path(self.header['guard_directory']['path']) / 'heartbeat.json'))
        cc.need(heartbeat['challenge'] == guard['challenge'] and heartbeat['binding'] == self.journal.binding
                and heartbeat['identity'] == guard['identity'], 'guard heartbeat binding differs')
        cc.need(type(heartbeat.get('monotonic_ns')) is int, 'invalid guard heartbeat clock')
        age = self.monotonic() - heartbeat['monotonic_ns']
        cc.need(0 <= age <= 5 * NS and self.journal.is_ancestor(heartbeat['checkpoint']), 'stale guard heartbeat')

    def _producer(self, state):
        cc.need(identity() == self.header['producer'] and supervised(identity(), self.header['supervisor']),
                'unregistered or unsupervised provisioner')
        cc.need(not (Path(self.header['guard_directory']['path']) / 'stop.json').exists()
                and not any(i['state'] in {'UNKNOWN', 'FAILED'} or i.get('operation_failed') for i in state['intents']), 'cleanup requested')
        self.guard_live(state)

    def _protection(self, state, additional=None):
        policies = [self.header.get('protections'), state.get('protections'), additional]
        names, ids, typed = [], [], {kind: [] for kind in mc.PROTECT_KINDS}
        for policy in policies:
            if policy is None:
                continue
            cc.need(isinstance(policy, dict) and set(policy) == {'names', 'ids', 'by_kind'}, 'invalid sealed protection policy')
            checked = protection_policy(**policy)
            names.extend(checked['names'])
            ids.extend(checked['ids'])
            for kind in typed:
                typed[kind].extend(checked['by_kind'][kind])
        return protection_policy(names, ids, typed)

    def bind_protections(self, policy, blocking=True):
        cc.need(not cc.contains_credential(policy, self.cloud.profile._credential),
                'controller credential found in protection policy')
        with self.journal.locked(blocking=blocking) as state:
            merged = self._protection(state, policy)
            if merged != self._protection(state):
                state['protections'] = merged
                self.journal.commit(state)
            return merged

    def _protected(self, state, kind, rid, name):
        policy = self._protection(state)
        return (rid in policy['ids'] or rid in policy['by_kind'][kind]
                or any(fnmatch.fnmatchcase(name, pattern) for pattern in policy['names']))

    def _retired(self, state, kind, rid):
        return any(e.get('event') == 'observed-retirement' and e.get('kind') == kind and e.get('id') == rid
                   for e in state['events'] if isinstance(e, dict))

    def _retire(self, state, kind, rid, source):
        if self._retired(state, kind, rid):
            return False
        state['events'].append(dict(event='observed-retirement', kind=kind, id=rid, source=source, unix_ns=self.clock()))
        return True

    def _observe_absent(self, state, kind, rid, source):
        if self._retire(state, kind, rid, source):
            self.journal.commit(state)

    def _observe_inventory(self, state, kind, rows, source):
        known = {r['id'] for r in state['owned'] if r['kind'] == kind}
        if kind == 'primary-ip':
            known.update(ip for r in state['owned'] for ip in r['implicit_ips'])
        present = {r['id']: r for r in rows}
        changed = False
        for rid in sorted(known - set(present)):
            changed = self._retire(state, kind, rid, source) or changed
        for record in state['owned']:
            if record['kind'] != kind or record['id'] not in present:
                continue
            item = present[record['id']]
            discontinuity = any(key in item and item[key] != record[key] for key in ('name', 'labels', 'created'))
            try:
                discontinuity = discontinuity or canonical(kind, item) != record['canonical']
            except (mc.GuardError, KeyError, TypeError, ValueError):
                pass  # A list lacking canonical fields is not replacement proof.
            if discontinuity:
                changed = self._retire(state, kind, record['id'], source + '-identity') or changed
            if kind == 'server' and 'public_net' in item:
                changed = self._observe_server_reference(state, record, item, source) or changed
        if kind == 'primary-ip':
            for association in state['associations']:
                item = present.get(association['id'])
                if item is not None and self._ip_changed(association, item):
                    changed = self._retire(state, kind, association['id'], source + '-identity') or changed
        if changed:
            self.journal.commit(state)

    def _observe_server_reference(self, state, record, item, source):
        net = item.get('public_net')
        ipv4 = net.get('ipv4') if isinstance(net, dict) else None
        rid = ipv4.get('id') if isinstance(ipv4, dict) else None
        if type(rid) is int and rid > 0 and [rid] == record['implicit_ips']:
            return False
        changed = False
        for recorded in record['implicit_ips']:
            changed = self._retire(state, 'primary-ip', recorded, source + '-server-reference') or changed
        if not record['implicit_ips']:
            changed = self._retire(state, 'server', record['id'], source + '-server-reference') or changed
        return changed

    def describe_owned(self, state, kind, rid, source, seconds=None):
        item = self.cloud.describe(kind, rid, (lambda: self._phase_budget(state)) if seconds is None else seconds)
        if item is None:
            self._observe_absent(state, kind, rid, source)
        else:
            records = [r for r in state['owned'] if r['kind'] == kind and r['id'] == rid]
            cc.need(len(records) == 1 and type(item.get('id')) is int and item['id'] == rid,
                    'exact readback lacks owned typed ID')
            record = records[0]
            try:
                continuous = (all(item.get(key) == record[key] for key in ('name', 'labels', 'created'))
                              and canonical(kind, item) == record['canonical'])
            except (mc.GuardError, KeyError, TypeError, ValueError):
                continuous = False
            changed = False if continuous else self._retire(state, kind, rid, source + '-identity')
            if kind == 'server':
                changed = self._observe_server_reference(state, record, item, source) or changed
            if changed:
                self.journal.commit(state)
        return item

    def read_owned(self, kind, rid, source, blocking=True):
        with self.journal.locked(blocking=blocking) as state:
            self._records(state)
            cc.need(any(r['kind'] == kind and r['id'] == rid for r in state['owned']), 'readback lacks owned typed ID')
            item = self.describe_owned(state, kind, rid, source)
            if item is not None:
                self._authorize(state, kind, item)
            return item

    @staticmethod
    def _ip_changed(association, item, exact=False):
        expected = dict(type=association['type'], auto_delete=association['auto_delete'],
                        created=association['created'], assignee_type='server', assignee_id=association['server_id'])
        return any((exact or key in item) and (type(item.get(key)) is not type(value) or item.get(key) != value)
                   for key, value in expected.items())

    def _authorize(self, state, kind, item, source='owned-identity-discontinuity'):
        self._records(state)
        matches = [r for r in state['owned'] if r['kind'] == kind and r['id'] == item.get('id')]
        cc.need(len(matches) == 1 and type(item.get('id')) is int, 'exact ID has no owned creation record')
        record = matches[0]
        cc.need(not self._retired(state, kind, item['id']), 'observed-retired ID cannot inherit ownership')
        if not (item.get('name') == record['name'] and mc.owned_name(item['name'], self.header['prefix'])
                and item.get('labels') == record['labels']):
            self._observe_absent(state, kind, item['id'], source)
            raise mc.GuardError('owned resource name/labels changed')
        try:
            continuous = item.get('created') == record['created'] and canonical(kind, item) == record['canonical']
        except (mc.GuardError, KeyError, TypeError, ValueError):
            self._observe_absent(state, kind, item['id'], source)
            raise
        if not continuous:
            self._observe_absent(state, kind, item['id'], source)
            raise mc.GuardError('owned resource identity changed or ID reused')
        if kind == 'server':
            if self._observe_server_reference(state, record, item, source):
                self.journal.commit(state)
            cc.need(not self._retired(state, 'server', item['id'])
                    and not any(self._retired(state, 'primary-ip', rid) for rid in record['implicit_ips']),
                    'observed-retired IP cannot inherit association')
        cc.need(not self._protected(state, kind, item['id'], item.get('name', '')),
                'additional guard protection conflicts with an owned ID')
        intent = state['intents'][record['intent']]
        cc.need(item['id'] not in self.header['baseline'][kind] and item['id'] not in intent['pre_ids'][kind],
                'owned ID conflicts with protected/pre-call inventory')
        return record

    def _accept(self, state, intent, item, reply, recovered=False):
        kind, rid = intent['kind'], item.get('id')
        cc.need(type(rid) is int and rid > 0 and rid not in self.header['baseline'][kind]
                and rid not in intent['pre_ids'][kind], 'creation response conflicts with existing typed ID')
        expected = intent['descriptor']
        cc.need(item.get('name') == intent['name'] and item.get('labels') == expected['labels']
                and canonical(kind, item) == expected_canonical(kind, expected), 'creation canonical binding differs')
        timestamp(item.get('created'))
        cc.need(not any(r['kind'] == kind and r['id'] == rid for r in state['owned']), 'duplicate creation acknowledgement')
        ips = []
        if kind == 'server':
            net = item['public_net']
            ipv4 = net.get('ipv4')
            if isinstance(ipv4, dict) and type(ipv4.get('id')) is int and ipv4['id'] > 0:
                ips = [ipv4['id']]
            # Retain exact server provenance even when its cascade is unsafe.
            # Only cascade() may authorize subordinate deletion.
        intent.update(resource_id=rid, recovered=recovered, state='ACKNOWLEDGED')
        if not recovered:
            intent['ack'] = reply
        state['owned'].append(dict(kind=kind, id=rid, name=intent['name'], labels=expected['labels'],
            created=item['created'], canonical=canonical(kind, item), intent=intent['sequence'], implicit_ips=ips))

    def _plan_descriptor(self, state, kind, body):
        result = descriptor(body)
        if kind == 'server':
            def name(resource_kind, rid):
                matches = [r['name'] for r in state['owned'] if r['kind'] == resource_kind and r['id'] == rid]
                cc.need(type(rid) is int and len(matches) == 1, 'server dependency is not recorded-owned')
                return matches[0]
            result['ssh_keys'] = [name('ssh-key', rid) for rid in body['ssh_keys']]
            result['firewalls'] = [dict(firewall=name('firewall', f['firewall'])) for f in body['firewalls']]
        return result

    def create(self, kind, body):
        cc.need(kind in KINDS, 'unplanned create kind')
        with self.journal.locked() as state:
            self._records(state)
            self.validate_pins()
            self._producer(state)
            seconds = self.lease_seconds(state, create=True, provisioning=True)
            plans = [p for p in self.header['plan'] if p['kind'] == kind and p['name'] == body.get('name')]
            cc.need(len(plans) == 1 and self._plan_descriptor(state, kind, body) == plans[0]['descriptor'],
                    'create differs from sealed plan')
            cc.need(not any(i['operation'] == 'create' and i['name'] == body['name'] for i in state['intents']),
                    'create intent cannot be retried or replaced')
            cc.need(not any(fnmatch.fnmatchcase(body['name'], pattern) for pattern in self._protection(state)['names']),
                    'additional guard protection conflicts with a planned name')
            census_start = self.clock()
            before = self.census(lambda: self._phase_budget(state, create=True), state, 'pre-create-census')
            cc.need(not any(r.get('name') == body['name'] for r in before[kind]), 'create name already exists')
            if kind == 'server':
                dependencies = [('ssh-key', rid) for rid in body['ssh_keys']]
                dependencies.extend(('firewall', row['firewall']) for row in body['firewalls'])
                for dependency_kind, rid in dependencies:
                    item = self.describe_owned(state, dependency_kind, rid, 'server-dependency-readback',
                        lambda: self._phase_budget(state, create=True))
                    cc.need(item is not None, 'server dependency is absent')
                    self._authorize(state, dependency_kind, item)
            nonce = os.urandom(16).hex()
            body = copy.deepcopy(body)
            body['labels']['intent'] = nonce
            intent = dict(sequence=len(state['intents']), operation='create', kind=kind, name=body['name'], nonce=nonce,
                descriptor=descriptor(body), request_sha256=cc.digest(cc.wire_json(body)),
                pre_ids={k: sorted(r['id'] for r in rows) for k, rows in before.items()},
                pre_census_sha256=cl.sha256(before), observed_start_ns=census_start, observed_end_ns=self.clock(),
                producer=self.header['producer'], state='PREPARED', resource_id=None, ack=None, actions=[],
                acknowledgement_validated=False, required_action_identified=False)
            state['intents'].append(intent)
            self.journal.commit(state)
            self._producer(state)
            state['active_lease'] = self._send_lease(state, intent['sequence'], create=True)
            intent['send_lease'] = copy.deepcopy(state['active_lease'])
            intent['send_generation'] = self.journal.checkpoint['generation'] + 1
            intent['state'] = 'SENDING'
            self.journal.commit(state)
            try:
                self.validate_pins()
                self._producer(state)
                reply = self.cloud.request('POST', '/' + cc.PLURALS[kind], body, self._send_budget(state))
                intent['ack'] = reply
                intent['actions'] = known_action_ids(reply)
                cc.need(reply['status'] == 201 and isinstance(reply['payload'], dict), 'create acknowledgement unavailable')
                actions = acknowledgement_actions(reply, kind, 'create')
                intent.update(acknowledgement_validated=True, required_action_identified=True)
                intent['actions'] = sorted(set(intent['actions']) | set(actions))
                item = reply['payload'].get(resource_key(kind))
                cc.need(isinstance(item, dict), 'create resource missing from acknowledgement')
                self._accept(state, intent, item, reply)
                self._observe_actions(state, intent, acknowledgement_records(reply))
                cc.need(kind != 'server' or bool(actions), 'server create action identity unavailable')
            except (mc.GuardError, KeyError, TypeError, ValueError) as error:
                intent['state'] = 'UNKNOWN'
                self.request_stop()
                state['events'].append(dict(event='unknown-create', intent=intent['sequence'], error_type=type(error).__name__))
                raise mc.GuardError('create failed or ambiguous; exact-ledger cleanup required') from None
            finally:
                state['active_lease'] = None
                self.journal.commit(state)
            return intent['resource_id']

    def mutate(self, kind, rid, operation, body=None):
        cc.need(kind in KINDS and type(rid) is int and rid > 0, 'invalid mutation kind/ID')
        cc.need(operation == 'delete' or {'subnet': 'network', 'attach': 'server', 'poweron': 'server'}.get(operation) == kind,
                'mutation operation differs from owned resource kind')
        paths = {'subnet': f'/networks/{rid}/actions/add_subnet', 'attach': f'/servers/{rid}/actions/attach_to_network',
                 'poweron': f'/servers/{rid}/actions/poweron', 'delete': f'/{cc.PLURALS[kind]}/{rid}'}
        cc.need(operation in paths, 'unplanned operation')
        provisioning = operation != 'delete'
        with self.journal.locked(blocking=provisioning) as state:
            self.validate_pins()
            self._records(state)
            if provisioning:
                self._producer(state)
            else:
                cc.need(state['guard'] is not None and identity() == state['guard']['identity']
                        and state['guard']['binding'] == self.journal.binding
                        and supervised(identity(), self.header['supervisor']), 'unregistered cleanup sender')
            seconds = self.lease_seconds(state, provisioning=provisioning)
            reads = lambda: self._phase_budget(state)
            item = self.cloud.describe(kind, rid, reads)
            if item is None:
                if any(r['kind'] == kind and r['id'] == rid for r in state['owned']):
                    self._observe_absent(state, kind, rid, 'exact-mutation-readback')
                raise mc.GuardError('mutation resource is absent')
            record = self._authorize(state, kind, item)
            if operation == 'delete' and kind == 'server':
                self.cascade(state, item, self.cloud.list('primary-ip', reads), reads)
            if operation == 'attach':
                target = self.cloud.describe('network', body['network'], reads)
                if target is None:
                    self._observe_absent(state, 'network', body['network'], 'attach-network-readback')
                    raise mc.GuardError('attach network missing')
                self._authorize(state, 'network', target)
                plan = next(p for p in self.header['plan'] if p['name'] == record['name'])
                cc.need(body['ip'] == plan['private_ip'], 'attach address differs from plan')
            if operation == 'subnet':
                cc.need(body == self.header['subnet'], 'subnet differs from plan')
            self.validate_pins()
            if provisioning:
                self._producer(state)
            intent = dict(sequence=len(state['intents']), operation=operation, kind=kind, name=record['name'],
                resource_id=rid, descriptor=descriptor(body) if body is not None else None,
                request_sha256=cc.digest(cc.wire_json(body)) if body is not None else cc.digest(b''),
                state='SENDING', ack=None, actions=[], producer=identity(),
                acknowledgement_validated=False, required_action_identified=False)
            lease = self._send_lease(state, intent['sequence'])
            state['intents'].append(intent)
            state['active_lease'] = lease
            intent['send_lease'] = copy.deepcopy(lease)
            intent['send_generation'] = self.journal.checkpoint['generation'] + 1
            self.journal.commit(state)
            try:
                self.validate_pins()
                if provisioning:
                    self._producer(state)
                reply = self.cloud.request('DELETE' if operation == 'delete' else 'POST', paths[operation], body,
                                           self._send_budget(state))
                intent['ack'] = reply
                intent['actions'] = known_action_ids(reply)
                intent['actions'] = sorted(set(intent['actions']) | set(acknowledgement_actions(reply, kind, operation)))
                intent.update(acknowledgement_validated=True, required_action_identified=True)
                intent['state'] = 'ACKNOWLEDGED'
                self._observe_actions(state, intent, acknowledgement_records(reply))
            except (mc.GuardError, KeyError, TypeError, ValueError) as error:
                intent['state'] = 'UNKNOWN'
                if provisioning:
                    self.request_stop()
                else:
                    state['mode'] = 'INCIDENT'
                state['events'].append(dict(event='unknown-mutation', intent=intent['sequence'], error_type=type(error).__name__))
                raise mc.GuardError('mutation failed or ambiguous; cleanup remains incomplete') from None
            finally:
                state['active_lease'] = None
                self.journal.commit(state)

    def cascade(self, state, server, ips, seconds=20):
        self._observe_inventory(state, 'primary-ip', ips, 'cascade-census')
        record = self._authorize(state, 'server', server)
        cc.need(self.header['graph_reserved'] is True and {'volumes', 'backup_window', 'public_net'} <= set(server)
                and server['volumes'] == [] and server['backup_window'] is None,
                'missing or unplanned server cascade or reservation')
        net = server['public_net']
        cc.need({'floating_ips', 'ipv4', 'ipv6', 'firewalls'} <= set(net) and net['floating_ips'] == [],
                'incomplete or unplanned server public-network cascade')
        cc.need(net.get('ipv6') is None and isinstance(net.get('ipv4'), dict), 'unexpected or missing IP family')
        refs = [net['ipv4'].get('id')]
        cc.need(all(type(i) is int and i > 0 for i in refs) and refs == record['implicit_ips'], 'server IP graph drifted')
        cc.need(not any(self._retired(state, 'primary-ip', rid) for rid in refs), 'observed-retired IP cannot inherit association')
        assigned = [ip for ip in ips if ip.get('assignee_type') == 'server' and ip.get('assignee_id') == server['id']]
        cc.need(sorted(ip['id'] for ip in assigned) == refs, 'incomplete or foreign IP association graph')
        intent = state['intents'][record['intent']]
        cc.need(intent['descriptor'].get('public_net') == dict(enable_ipv4=True, enable_ipv6=False),
                'implicit IP allocation differs from sealed request')
        origin = 'recovered-server-association' if intent.get('recovered') is True else 'saved-create-response'
        if origin == 'saved-create-response':
            saved = intent['ack'].get('payload', {}).get('server', {})
            cc.need(saved.get('public_net', {}).get('ipv4', {}).get('id') == refs[0],
                    'saved server acknowledgement IP reference differs')
        identity_record = {key: record[key] for key in ('kind', 'id', 'name', 'labels', 'created', 'canonical')}
        associations, observations = [], []
        fields = ('id', 'type', 'assignee_type', 'assignee_id', 'auto_delete', 'created')
        for ip in assigned:
            rid = ip['id']
            cc.need(type(ip.get('assignee_id')) is int and ip['assignee_id'] > 0
                    and ip.get('assignee_type') == 'server' and ip['assignee_id'] == server['id'],
                    'invalid primary-IP assignee')
            cc.need(not self._protected(state, 'primary-ip', rid, ip.get('name', '')),
                    'additional guard protection conflicts with a primary IP')
            cc.need(type(rid) is int and ip.get('type') == 'ipv4' and ip.get('auto_delete') is True
                    and rid not in self.header['baseline']['primary-ip'] and rid not in intent['pre_ids']['primary-ip'],
                    'auto-deleted IP is protected, unplanned or unattributable')
            timestamp(ip.get('created'))
            exact = self.cloud.describe('primary-ip', rid, seconds)
            previous = [a for a in state['associations'] if a['server_id'] == server['id'] and a['id'] == rid]
            if exact is not None and any(self._ip_changed(a, exact, exact=True) for a in previous):
                self._observe_absent(state, 'primary-ip', rid, 'primary-ip-identity-discontinuity')
                raise mc.GuardError('recorded primary-IP identity changed')
            if exact is None:
                if self._retire(state, 'primary-ip', rid, 'cascade-exact-readback'):
                    self.journal.commit(state)
                raise mc.GuardError('attributed primary IP disappeared')
            cc.need(isinstance(exact.get('name'), str), 'missing or invalid exact primary-IP name')
            cc.need(not self._protected(state, 'primary-ip', rid, exact['name']),
                    'additional guard protection conflicts with a primary IP')
            cc.need(type(exact.get('assignee_id')) is int and exact['assignee_id'] > 0
                    and exact.get('assignee_type') == 'server' and exact['assignee_id'] == server['id'],
                    'invalid primary-IP assignee')
            cc.need(all(key in ip and key in exact and type(exact[key]) is type(ip[key]) and exact[key] == ip[key]
                        for key in fields), 'primary-IP exact readback differs from complete census')
            association = dict(id=rid, server_id=server['id'], type='ipv4', auto_delete=True, created=ip['created'], origin=origin,
                proof=dict(intent=intent['sequence'], server_kind='server', server_id=server['id'], server_nonce=intent['nonce'],
                    server_identity_sha256=cl.sha256(identity_record), server_description_sha256=cl.sha256(server),
                    ip_census_sha256=cl.sha256(ips), ip_description_sha256=cl.sha256(exact),
                    baseline_ids_sha256=cl.sha256(self.header['baseline']['primary-ip']),
                    pre_call_ids_sha256=cl.sha256(intent['pre_ids']['primary-ip'])))
            cc.need(len(previous) <= 1 and (not previous or all(previous[0].get(key) == association[key]
                    for key in ('id', 'server_id', 'type', 'auto_delete', 'created', 'origin'))),
                    'recorded primary-IP identity changed')
            observations.append(association)
            if not previous:
                state['associations'].append(association)
            associations.append(previous[0] if previous else association)
        state['events'].append(dict(event='cascade-readback', server_id=server['id'], proofs=observations, unix_ns=self.clock()))
        return associations

    def poll_actions(self, blocking=True):
        with self.journal.locked(blocking=blocking) as state:
            self._records(state)
            for intent in state['intents']:
                if intent['state'] not in {'ACKNOWLEDGED', 'DONE', 'FAILED'}:
                    continue
                if intent.get('required_action_identified') is not True:
                    intent['state'] = 'UNKNOWN'
                    continue
                unread = set(intent['actions'])
                pending = set(intent.get('pending_actions', []))
                if intent['state'] == 'ACKNOWLEDGED':
                    pending.update(unread)
                for rid in intent['actions']:
                    previous, event_count = copy.deepcopy(intent), len(state['events'])
                    budget = self._phase_budget(state)
                    unread.discard(rid)
                    status, unavailable = None, False
                    try:
                        action = self.cloud.action(rid, budget)
                        status = observe_action(intent, action, pending, unread)
                    except (mc.GuardError, OSError, KeyError, TypeError, ValueError) as error:
                        unavailable = True
                        pending.add(rid)
                        pending.update(unread)
                        state['events'].append(dict(event='action-read-unavailable', intent=intent['sequence'],
                            action_id=rid, error_type=type(error).__name__, unix_ns=self.clock()))
                    intent['pending_actions'] = sorted(pending)
                    intent['state'] = ('ACKNOWLEDGED' if pending else
                                       'FAILED' if intent.get('operation_failed') else 'DONE')
                    if intent != previous or len(state['events']) != event_count:
                        self.journal.commit(state)
                    if state['mode'] == 'OPEN' and (status == 'error' or unavailable):
                        self.request_stop()
                if not intent['actions']:
                    intent['pending_actions'] = []
                    intent['state'] = 'FAILED' if intent.get('operation_failed') else 'DONE'
            self.journal.commit(state)
            return all(i['state'] == 'DONE' for i in state['intents'])

    def _observe_actions(self, state, intent, actions, previous=None):
        previous = copy.deepcopy(intent) if previous is None else previous
        pending, unread = set(intent.get('pending_actions', [])), set(intent['actions'])
        failed = False
        for action in actions:
            if (not isinstance(action, dict) or type(action.get('id')) is not int
                    or action['id'] not in unread or not action_for(action, intent['kind'], intent['resource_id'])
                    or not isinstance(action.get('status'), str) or action['status'] not in {'running', 'success', 'error'}):
                continue
            unread.discard(action['id'])
            failed = observe_action(intent, action, pending, unread) == 'error' or failed
        if pending or 'pending_actions' in intent:
            intent['pending_actions'] = sorted(pending)
        if intent != previous:
            self.journal.commit(state)
        if failed and state['mode'] == 'OPEN':
            self.request_stop()

    def _recovery_actions(self, state, intent):
        policy_error = []
        def reads():
            try:
                return self._phase_budget(state)
            except (mc.GuardError, OSError, KeyError, TypeError, ValueError):
                policy_error.append(True)
                raise
        def observed(actions):
            try:
                bound = [a for a in actions if isinstance(a, dict) and type(a.get('id')) is int and a['id'] > 0
                         and action_for(a, 'server', intent['resource_id'])]
                previous = copy.deepcopy(intent)
                intent['actions'] = sorted(set(intent['actions']) | {a['id'] for a in bound})
                self._observe_actions(state, intent, bound, previous)
            except (mc.GuardError, OSError, KeyError, TypeError, ValueError):
                policy_error.append(True)
                raise
        try:
            actions = self.cloud.resource_actions('server', intent['resource_id'], reads, on_page=observed)
        except (mc.GuardError, OSError, KeyError, TypeError, ValueError) as error:
            if policy_error:
                raise
            state['events'].append(dict(event='recovery-action-read-unavailable', intent=intent['sequence'],
                error_type=type(error).__name__, unix_ns=self.clock()))
            return []
        bound = [a for a in actions if type(a.get('id')) is int and a['id'] > 0
                 and action_for(a, 'server', intent['resource_id'])]
        return [a for a in bound if a.get('command') == 'create_server']

    def recover(self, blocking=True):
        with self.journal.locked(blocking=blocking) as state:
            self._records(state)
            self.validate_pins()
            before = self.census(lambda: self._phase_budget(state), state, 'recovery-census')
            for intent in state['intents']:
                if intent['state'] != 'UNKNOWN':
                    continue
                if intent.get('resource_id') is not None:
                    if intent['operation'] == 'create':
                        if self._retired(state, intent['kind'], intent['resource_id']):
                            if intent.get('required_action_identified') is True:
                                intent['state'] = 'ACKNOWLEDGED'
                            continue  # Never correlate a replacement with the original create.
                        item = self.cloud.describe(intent['kind'], intent['resource_id'], lambda: self._phase_budget(state))
                        if item is None:
                            self._observe_absent(state, intent['kind'], intent['resource_id'], 'known-recovery-readback')
                            if intent.get('required_action_identified') is True:
                                intent['state'] = 'ACKNOWLEDGED'
                        else:
                            self._authorize(state, intent['kind'], item)
                            if intent.get('required_action_identified') is True:
                                intent['state'] = 'ACKNOWLEDGED'
                            elif intent['kind'] in {'network', 'ssh-key'}:
                                intent['required_action_identified'] = True
                                intent['state'] = 'ACKNOWLEDGED'
                            elif intent['kind'] == 'server':
                                candidates = self._recovery_actions(state, intent)
                                if len(candidates) == 1:
                                    intent['actions'] = sorted(set(intent['actions']) | {candidates[0]['id']})
                                    intent['required_action_identified'] = True
                                    intent['state'] = 'ACKNOWLEDGED'
                    elif intent.get('required_action_identified') is True:
                        # Partial sibling IDs alone cannot resolve a missing operation action.
                        intent['state'] = 'ACKNOWLEDGED'
                    continue
                if intent['operation'] != 'create':
                    continue
                matches, inconclusive = [], []
                for item in before[intent['kind']]:
                    labels = item.get('labels')
                    cc.need(labels is None or isinstance(labels, dict), 'malformed recovery census labels')
                    if (labels or {}).get('intent') != intent['nonce']:
                        continue
                    budget = self._phase_budget(state)
                    try:
                        full = self.cloud.describe(intent['kind'], item['id'], budget)
                        cc.need(full is not None, 'recovery candidate disappeared')
                        observed_canonical = canonical(intent['kind'], full)
                        timestamp(full.get('created'))
                    except (mc.GuardError, OSError, KeyError, TypeError, ValueError) as error:
                        inconclusive.append(item.get('id'))
                        state['events'].append(dict(event='recovery-candidate-unavailable', kind=intent['kind'],
                            id=item.get('id'), error_type=type(error).__name__))
                        continue
                    try:
                        cc.need(full.get('name') == intent['name'] and full.get('labels') == intent['descriptor']['labels']
                            and observed_canonical == expected_canonical(intent['kind'], intent['descriptor']),
                            'recovery canonical mismatch')
                        cc.need(full['id'] not in intent['pre_ids'][intent['kind']]
                                and full['id'] not in self.header['baseline'][intent['kind']], 'recovery candidate protected')
                        cc.need(not self._protected(state, intent['kind'], full['id'], full.get('name', '')),
                                'additional guard protection conflicts with a recovery candidate')
                        matches.append(full)
                    except (mc.GuardError, KeyError, TypeError, ValueError):
                        state['events'].append(dict(event='recovery-candidate-refused', kind=intent['kind'], id=item.get('id')))
                state['events'].append(dict(event='recovery-census', intent=intent['sequence'],
                    candidates=[m['id'] for m in matches], inconclusive_candidates=inconclusive,
                    census_sha256=cl.sha256(before), unix_ns=self.clock()))
                if len(matches) == 1 and not inconclusive:
                    proof = dict(origin='conditional-nonce-recovery', census_sha256=cl.sha256(before),
                                 original_ack_missing=intent['ack'] is None, candidate=matches[0])
                    # Preserve the adverse/missing response; recovery is an additional receipt.
                    original = intent['ack']
                    intent['recovery'] = proof
                    self._accept(state, intent, matches[0], proof, recovered=True)
                    intent['original_ack'] = original
                    # Publish identity independently of fallible action correlation.
                    intent['state'] = 'UNKNOWN'
                    self.journal.commit(state)
                    if intent.get('required_action_identified') is True:
                        intent['state'] = 'ACKNOWLEDGED'
                    elif intent['kind'] in {'network', 'ssh-key'}:
                        # Their pinned API create responses have no asynchronous actions.
                        intent['required_action_identified'] = True
                        intent['state'] = 'ACKNOWLEDGED'
                    elif intent['kind'] == 'server':
                        matches_action = self._recovery_actions(state, intent)
                        if len(matches_action) == 1:
                            intent['actions'] = sorted(set(intent['actions']) | {matches_action[0]['id']})
                            intent['required_action_identified'] = True
                            intent['state'] = 'ACKNOWLEDGED'
                            intent['recovery']['corroborating_create_action'] = matches_action[0]
            self.journal.commit(state)

    def request_stop(self):
        atomic_private(Path(self.header['guard_directory']['path']) / 'stop.json',
                       dict(binding=self.journal.binding, requested_unix_ns=self.clock()))

    def _lifetime_breached(self, state=None, *, observed_ns=None):
        path = Path(self.header['guard_directory']['path']) / 'hard-lifetime-breached.json'
        recorded = False
        if path.exists():
            evidence = json.loads(cc.private_bytes(path))
            cc.need(evidence.get('binding') == self.journal.binding and evidence.get('hard_lifetime_breached') is True,
                    'hard-lifetime breach evidence binding differs')
            recorded = True
        observed = self.clock() if observed_ns is None else observed_ns
        breached = (recorded or observed >= self.header['time']['h_ns']
                    or state is not None and (state.get('hard_lifetime_breached') is True
                        or any(e.get('event') == 'hard-deadline-unresolved' for e in state['events'])))
        if breached and not recorded:
            atomic_private(path, dict(binding=self.journal.binding, hard_lifetime_breached=True,
                                      observed_unix_ns=observed, hard_lifetime_met=False))
        if breached and state is not None and state.get('hard_lifetime_breached') is not True:
            state['hard_lifetime_breached'] = True
            state['events'].append(dict(event='hard-lifetime-breached', unix_ns=observed, hard_lifetime_met=False))
        return bool(breached)

    def _producer_drain(self, lease):
        if lease is None or lease.get('actor') != self.header['producer']:
            return 0
        cc.need(type(lease.get('deadline_ns')) is int and type(lease.get('monotonic_deadline_ns')) is int,
                'invalid producer lease during cutover')
        return max(0, min(20, (lease['deadline_ns'] - self.clock()) / NS,
                          (lease['monotonic_deadline_ns'] - self.monotonic()) / NS))

    def _termination_deadline(self, lease):
        if lease is not None and lease.get('actor') == self.header['producer']:
            self._producer_drain(lease)  # Validate both absolute clock bindings.
            now = self.monotonic()
            effective_end = now + lease['deadline_ns'] - self.clock()
            return min(lease['monotonic_deadline_ns'], effective_end) + 2 * NS
        return self.monotonic() + 2 * NS

    def quiesce_producer(self, drain_seconds=0, terminate_seconds=2, termination_deadline_ns=None, progress=None):
        cc.need(0 <= drain_seconds <= 20 and 0 < terminate_seconds <= 2, 'invalid producer quiescence budget')
        termination_end = (self.monotonic() + int((drain_seconds + terminate_seconds) * NS)
                           if termination_deadline_ns is None else termination_deadline_ns)
        self.request_stop()
        producer = self.header['producer']
        if not live(producer):
            return
        cc.need(hasattr(os, 'pidfd_open') and hasattr(signal, 'pidfd_send_signal'), 'safe controller pidfd unavailable')
        try:
            fd = os.pidfd_open(producer['pid'])
        except OSError as error:
            if error.errno != errno.ESRCH:
                raise
            cc.need(not live(producer), 'producer exit could not be confirmed')
            return
        try:
            if not live(producer):
                return  # Never signal an unrelated replacement PID.
            def wait(seconds):
                if progress is None:
                    return bool(select.select([fd], [], [], seconds)[0])
                end = min(termination_end, self.monotonic() + int(seconds * NS))
                while True:
                    if progress is not None:
                        progress()
                    remaining = max(0, (end - self.monotonic()) / NS)
                    if select.select([fd], [], [], min(1, remaining))[0]:
                        return True
                    if self.monotonic() >= end:
                        return False
            drain = min(drain_seconds, max(0, (termination_end - int(terminate_seconds * NS) - self.monotonic()) / NS))
            if wait(drain):
                return
            def send(sig):
                if not live(producer):
                    return False
                try:
                    signal.pidfd_send_signal(fd, sig)
                except OSError as error:
                    if error.errno != errno.ESRCH:
                        raise
                    cc.need(not live(producer) or bool(select.select([fd], [], [], 0)[0]),
                            'producer exit could not be confirmed')
                    return False
                return True
            if not send(signal.SIGTERM):
                return
            term_wait = max(0, min(terminate_seconds / 2, (termination_end - self.monotonic()) / NS))
            if not wait(term_wait):
                if not send(signal.SIGKILL):
                    return
                remaining = (termination_end - self.monotonic()) / NS
                cc.need(remaining > 0 and wait(remaining), 'producer did not quiesce')
        finally:
            os.close(fd)

    def close_admission(self, termination_deadline_ns=None):
        self.validate_pins()
        cc.need(identity() != self.header['producer'] and supervised(identity(), self.header['supervisor']),
                'cutover requires a supervised guard distinct from the producer')
        self.request_stop()
        try:
            observed = self.journal.peek()
        except cl.ObservationDeferred:
            observed = self._last_observation
        confirmed = self.journal.read_if_available()
        lease = observed['active_lease']
        if confirmed is not None:
            observed, lease = confirmed, confirmed['active_lease']
        elif lease is None:
            saved = [i['send_lease'] for i in observed['intents']
                     if isinstance(i.get('send_lease'), dict) and i['send_lease'].get('actor') == self.header['producer']]
            if saved:
                lease = max(saved, key=lambda entry: entry['intent'])
        progress = (lambda: self._publish_heartbeat(observed)) if (
            observed.get('guard') is not None and observed['guard']['identity'] == identity()) else None
        self.quiesce_producer(drain_seconds=self._producer_drain(lease),
            termination_deadline_ns=(self._termination_deadline(lease)
                                     if termination_deadline_ns is None else termination_deadline_ns), progress=progress)
        acquired = False
        try:
            with self.journal.locked(blocking=False) as state:
                acquired = True
                self.validate_pins()
                registered = state['guard']
                cc.need(registered is None or registered['identity'] == identity() or not live(registered['identity']),
                        'another guard is still live at cutover')
                cc.need(not live(self.header['producer']), 'producer can still dispatch at cutover')
                if state['mode'] == 'OPEN':
                    state['mode'] = 'CLOSING'
                self._records(state)
                self._lifetime_breached(state)
                for intent in state['intents']:
                    if intent['state'] == 'SENDING':
                        intent['state'] = 'UNKNOWN'
                    elif intent['state'] == 'PREPARED':
                        cc.need(intent.get('ack') is None and not intent['actions']
                                and (intent['operation'] != 'create' or intent.get('resource_id') is None),
                                'contradictory prepared dispatch evidence')
                        intent['state'] = 'NOT_DISPATCHED'
                state['active_lease'] = None
                self.journal.commit(state)
                self._last_observation = copy.deepcopy(state)
                return True
        except BlockingIOError:
            if acquired:
                raise
            self._lifetime_breached()
            atomic_private(Path(self.header['guard_directory']['path']) / 'cutover-incident.json',
                dict(binding=self.journal.binding, event='cutover-lock-unavailable', unix_ns=self.clock(),
                     hard_lifetime_met=False, provisioning_reopened=False))
            return False

    def completion(self, blocking=True):
        with self.journal.locked(blocking=blocking) as state:
            self._records(state)
            cc.need(state['mode'] in {'CLOSING', 'INCIDENT'}, 'completion requires closed admission')
            current = self.census(lambda: self._phase_budget(state), state, 'completion-census')
            exact = all(sorted(item['id'] for item in current[k]) == self.header['baseline'][k] for k in mc.PROTECT_KINDS)
            survivor_ids = {(r['kind'], r['id']) for r in state['owned']
                            if any(item['id'] == r['id'] for item in current[r['kind']])}
            implicit_ids = {ip for r in state['owned'] for ip in r['implicit_ips']}
            ips = {ip for ip in implicit_ids if any(item['id'] == ip for item in current['primary-ip'])}
            unresolved = []
            targets = sorted({(r['kind'], r['id']) for r in state['owned']}
                             | {('primary-ip', ip) for ip in implicit_ids})
            for kind, rid in targets:
                budget = self._phase_budget(state)
                try:
                    if kind == 'primary-ip':
                        item = self.cloud.describe(kind, rid, budget)
                        if item is None:
                            self._observe_absent(state, kind, rid, 'completion-exact')
                    else:
                        item = self.describe_owned(state, kind, rid, 'completion-exact', seconds=budget)
                except (mc.GuardError, OSError, KeyError, TypeError, ValueError) as error:
                    unresolved.append(dict(kind=kind, id=rid, error_type=type(error).__name__))
                    continue
                if item is not None:
                    if kind == 'primary-ip':
                        if any(self._ip_changed(association, item, exact=True) for association in state['associations']
                               if association['id'] == rid):
                            self._observe_absent(state, kind, rid, 'primary-ip-identity-discontinuity')
                        ips.add(rid)
                    else:
                        survivor_ids.add((kind, rid))
            survivors = [dict(kind=kind, id=rid) for kind, rid in sorted(survivor_ids)]
            pending = [i['sequence'] for i in state['intents'] if i.get('pending_actions')
                       or (i['state'] != 'NOT_DISPATCHED' and (i['state'] not in {'DONE', 'FAILED'}
                           or i.get('required_action_identified') is not True))]
            ok = exact and not survivors and not ips and not pending and not unresolved
            observed = self.clock()
            breached = self._lifetime_breached(state, observed_ns=observed)
            result = dict(complete=ok, baseline_equal=exact, owned_survivors=survivors, subordinate_survivors=len(ips),
                          pending_intents=pending, hard_lifetime_met=ok and not breached
                              and observed <= self.header['time']['h_ns'])
            if unresolved:
                result['unresolved_readbacks'] = unresolved
            state['events'].append(dict(event='completion-check', unix_ns=observed, **result))
            if ok:
                state['mode'] = 'COMPLETE'
            self.journal.commit(state)
            return result

    def arm_guard(self):
        self.validate_pins()
        actor = identity()
        cc.need(actor != self.header['producer'], 'guard must be distinct from the producer')
        cc.need(supervised(actor, self.header['supervisor']), 'guard has no live owning supervisor')
        initial = self.journal.peek()
        self._last_observation = copy.deepcopy(initial)
        previous = initial['guard']
        if previous is not None:
            cc.need(not live(previous['identity']), 'another guard is still live')
            cc.need(self.close_admission(), 'journal lock unavailable during guard takeover')
        try:
            with self.journal.locked(blocking=False) as state:
                current = state['guard']
                cc.need(current == previous and (current is None or not live(current['identity'])),
                        'another guard is still live or registration changed')
                cc.need(state['mode'] != 'COMPLETE', 'completed campaign cannot rearm')
                policy = getattr(self, '_guard_protections', None)
                if policy is not None:
                    state['protections'] = self._protection(state, policy)
                for record in state['owned']:
                    cc.need(not self._protected(state, record['kind'], record['id'], record['name']),
                            'additional guard protection conflicts with an owned ID')
                state['guard'] = dict(identity=actor, binding=self.journal.binding, challenge=uuid.uuid4().hex,
                                      time=self.header['time'], supervisor=self.header['supervisor'])
                self.journal.commit(state)
                self._strict_release_generation = self.journal.checkpoint['generation']
        except BlockingIOError:
            raise mc.GuardError('guard registration lock unavailable; another guard may be live') from None
        self._last_observation = copy.deepcopy(state)
        self.heartbeat()
        return state['guard']

    def heartbeat(self):
        # peek is observational; never use it to authorize a provider mutation.
        try:
            state = self.journal.peek()
        except cl.ObservationDeferred:
            state = copy.deepcopy(self._last_observation)
        guard = state['guard']
        cc.need(guard is not None and guard['identity'] == identity(), 'heartbeat actor differs from armed guard')
        lease = state['active_lease']
        if lease is not None and lease['actor'] == self.header['producer']:
            old = getattr(self, '_watched_lease', None)
            if old is None or lease['intent'] > old['intent']:
                self._watched_lease = copy.deepcopy(lease)
        # A cleared observational head can precede its final directory fsync.
        # Only successful nonblocking lock acquisition permits strict release proof.
        confirmed = self.journal.read_if_available()
        if confirmed is not None:
            self._strict_release_generation = self.journal.checkpoint['generation']
            state = confirmed
            if confirmed['active_lease'] is None:
                self._watched_lease = None
            elif confirmed['active_lease']['actor'] == self.header['producer']:
                self._watched_lease = copy.deepcopy(confirmed['active_lease'])
        else:
            # The ACK head can replace SENDING between samples while publication
            # still owns the lock. Its authenticated intent retains the same lease.
            for intent in state['intents']:
                saved = intent.get('send_lease')
                if (isinstance(saved, dict) and saved.get('actor') == self.header['producer']
                        and type(intent.get('send_generation')) is int
                        and intent['send_generation'] > getattr(self, '_strict_release_generation', -1)):
                    old = getattr(self, '_watched_lease', None)
                    if old is None or saved['intent'] > old['intent']:
                        self._watched_lease = copy.deepcopy(saved)
        if state['mode'] != 'COMPLETE':
            self._lifetime_breached()
        self._last_observation = copy.deepcopy(state)
        watched = getattr(self, '_watched_lease', None)
        if watched is not None and (self.clock() >= watched['deadline_ns']
                                    or self.monotonic() >= watched['monotonic_deadline_ns']):
            if self.close_admission(termination_deadline_ns=self._termination_deadline(watched)):
                state = copy.deepcopy(self._last_observation)
        self._publish_heartbeat(state)
        return state

    def _publish_heartbeat(self, state):
        guard = state['guard']
        cc.need(guard is not None and guard['identity'] == identity(), 'heartbeat actor differs from armed guard')
        atomic_private(Path(self.header['guard_directory']['path']) / 'heartbeat.json',
            dict(binding=self.journal.binding, identity=guard['identity'], challenge=guard['challenge'],
                 checkpoint=self.journal.checkpoint, monotonic_ns=self.monotonic()))

    def cleanup_pass(self):
        state = self.journal.read_if_available()
        cc.need(state is not None, 'cleanup journal lock unavailable')
        cc.need(state['mode'] in {'CLOSING', 'INCIDENT'}, 'cleanup before admission closed')
        self.recover(blocking=False)
        self.poll_actions(blocking=False)
        state = self.journal.read_if_available()
        cc.need(state is not None, 'cleanup journal lock unavailable')
        for kind in KINDS:
            for record in [r for r in state['owned'] if r['kind'] == kind]:
                try:
                    item = self.read_owned(kind, record['id'], 'cleanup-exact-readback', blocking=False)
                    if item is None:
                        continue
                    self.mutate(kind, record['id'], 'delete')
                except (mc.GuardError, OSError, ValueError, KeyError, TypeError) as error:
                    with self.journal.locked(blocking=False) as fresh:
                        fresh['events'].append(dict(event='delete-refused-or-unknown', kind=kind,
                            id=record['id'], error_type=type(error).__name__, unix_ns=self.clock(),
                            refusal=cc.redacted(str(error), (self.cloud.profile._credential,))
                                if isinstance(error, mc.GuardError) else None))
                        self.journal.commit(fresh)
        self.poll_actions(blocking=False)
        return self.completion(blocking=False)

    def terminal_result(self, blocking=True):
        state = self.journal.read() if blocking else self.journal.read_if_available()
        if state is None:
            return None
        self._records(state)
        if state['mode'] != 'COMPLETE':
            return None
        self.validate_pins()
        checks = [event for event in state['events'] if isinstance(event, dict) and event.get('event') == 'completion-check']
        cc.need(bool(checks), 'completed campaign lacks authenticated completion evidence')
        event = checks[-1]
        fields = ('complete', 'baseline_equal', 'owned_survivors', 'subordinate_survivors',
                  'pending_intents', 'hard_lifetime_met')
        cc.need(all(key in event for key in fields) and event['complete'] is True
                and event['baseline_equal'] is True and event['owned_survivors'] == []
                and type(event['subordinate_survivors']) is int and event['subordinate_survivors'] == 0
                and event['pending_intents'] == [] and type(event['hard_lifetime_met']) is bool
                and type(event.get('unix_ns')) is int and event['unix_ns'] >= self.header['time']['t0_ns']
                and all(not intent.get('pending_actions') and (intent['state'] == 'NOT_DISPATCHED'
                        or (intent['state'] in {'DONE', 'FAILED'} and intent.get('required_action_identified') is True))
                        for intent in state['intents']),
                'invalid authenticated completion evidence')
        cc.need(not event['hard_lifetime_met'] or (event['unix_ns'] <= self.header['time']['h_ns']
                and state.get('hard_lifetime_breached') is not True), 'terminal lifetime evidence conflicts')
        return {key: copy.deepcopy(event[key]) for key in fields}

    def publish_cleanup(self, blocking=True):
        result = self.terminal_result(blocking=blocking)
        if result is not None:
            atomic_private(Path(self.header['guard_directory']['path']) / 'cleanup.json', result)
        return result

    def guard_loop(self, sleep=time.sleep, poll_seconds=1, cleanup_interval_seconds=60):
        cc.need(0 < poll_seconds <= 1, 'guard poll must be in (0,1] seconds')
        cc.need(1 <= cleanup_interval_seconds <= 60, 'cleanup retry interval must be bounded to60s')
        terminal_only = self.terminal_result(blocking=False) is not None
        if not terminal_only:
            self.arm_guard()
        next_cleanup = 0
        while True:
            if terminal_only:
                try:
                    result = self.publish_cleanup(blocking=False)
                    if result is not None:
                        return result
                except (mc.GuardError, OSError, ValueError, KeyError, TypeError) as error:
                    atomic_private(Path(self.header['guard_directory']['path']) / ('incident-' + uuid.uuid4().hex + '.json'),
                        dict(complete=False, error_type=type(error).__name__, unix_ns=self.clock(), hard_lifetime_met=False))
                sleep(poll_seconds)
                continue
            state = self.heartbeat()
            if state['mode'] == 'COMPLETE':
                terminal_only = True
                continue
            stop = (Path(self.header['guard_directory']['path']) / 'stop.json').exists()
            setup_complete = any(e.get('event') == 'provisioning-complete' for e in state['events'])
            producer_lost = not live(self.header['producer']) and not setup_complete
            if state['mode'] == 'OPEN' and (stop or producer_lost or self.clock() >= self.header['time']['g_ns']):
                if not self.close_admission():
                    sleep(poll_seconds)
                    continue
                state = copy.deepcopy(self._last_observation)
            if state['mode'] == 'CLOSING' and self.clock() >= self.header['time']['h_ns']:
                try:
                    admission = self.journal.locked(blocking=False)
                    with admission as expired:
                        expired['mode'] = 'INCIDENT'
                        self._lifetime_breached(expired)
                        expired['events'].append(dict(event='hard-deadline-unresolved', unix_ns=self.clock(), hard_lifetime_met=False))
                        self.journal.commit(expired)
                    state = expired
                except BlockingIOError:
                    self._lifetime_breached()
                    sleep(poll_seconds)
                    continue
            if state['mode'] in {'CLOSING', 'INCIDENT'} and self.monotonic() >= next_cleanup:
                next_cleanup = self.monotonic() + int(cleanup_interval_seconds * NS)
                try:
                    result = self.cleanup_pass()
                    if result['complete']:
                        terminal_only = True
                        continue
                    atomic_private(Path(self.header['guard_directory']['path']) / 'cleanup.json', result)
                except (mc.GuardError, OSError, ValueError, KeyError, TypeError) as error:
                    atomic_private(Path(self.header['guard_directory']['path']) / ('incident-' + uuid.uuid4().hex + '.json'),
                        dict(complete=False, error_type=type(error).__name__, unix_ns=self.clock(), hard_lifetime_met=False))
            sleep(poll_seconds)
