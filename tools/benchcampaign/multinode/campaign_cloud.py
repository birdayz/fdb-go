"""Fixed Hetzner endpoints and one-shot controller HTTPS; never a CLI mutation.

Auth precedence follows hcloud CLI v1.70.1 config/{config,options,context}.go.
Missing/ambiguous contexts and nonofficial endpoints are deliberately stricter.
No credential is read until an explicitly executing caller loads a profile.
"""
import contextlib
import hashlib
import http.client
import json
import os
from pathlib import Path
import pwd
import re
import signal
import ssl
import stat
import time
import tomllib

import mn_common as mc

ENDPOINT = 'https://api.hetzner.cloud/v1'
HOST = 'api.hetzner.cloud'
PLURALS = {'server': 'servers', 'network': 'networks', 'firewall': 'firewalls',
           'ssh-key': 'ssh_keys', 'primary-ip': 'primary_ips', 'floating-ip': 'floating_ips',
           'volume': 'volumes', 'load-balancer': 'load_balancers', 'placement-group': 'placement_groups',
           'certificate': 'certificates'}
MAX_RESPONSE = 8 * 1024 * 1024


def need(ok, message):
    if not ok:
        raise mc.GuardError(message)


def wire_json(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':'), allow_nan=False).encode()


def digest(data):
    return hashlib.sha256(data).hexdigest()


def json_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError('duplicate provider JSON field')
        result[key] = value
    return result


def invalid_constant(value):
    raise ValueError('nonfinite provider JSON value')


def private_bytes(path):
    """No-follow read of a controller credential; error text never contains bytes."""
    path = Path(path).absolute()
    for component in [path.parent, *path.parents]:
        need(not component.is_symlink(), 'symlink in credential path')
    fd = os.open(path, os.O_RDONLY | os.O_NONBLOCK | os.O_NOFOLLOW | os.O_CLOEXEC)
    try:
        info = os.fstat(fd)
        need(stat.S_ISREG(info.st_mode) and info.st_uid == os.geteuid()
             and stat.S_IMODE(info.st_mode) == 0o600 and info.st_nlink == 1,
             'credential file must be owned regular0600')
        with os.fdopen(os.dup(fd), 'rb') as stream:
            data = stream.read(MAX_RESPONSE + 1)
        need(len(data) <= MAX_RESPONSE, 'credential file too large')
        final = path.lstat()
        need((info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns, info.st_mode, info.st_uid, info.st_nlink) ==
             (final.st_dev, final.st_ino, final.st_size, final.st_mtime_ns, final.st_mode, final.st_uid, final.st_nlink),
             'credential file changed during read')
        return data
    finally:
        os.close(fd)


class Profile:
    def __init__(self, context=None, config=None, environ=None):
        self.context_arg, self.config_arg, self.environ = context, config, environ
        self.binding, self._credential = self._resolve()

    def __repr__(self):
        return '<sealed controller cloud profile>'

    def _resolve(self):
        env = os.environ if self.environ is None else self.environ
        path = Path(self.config_arg or env.get('HCLOUD_CONFIG') or
                    (Path(pwd.getpwuid(os.getuid()).pw_dir) / '.config/hcloud/cli.toml')).absolute()
        try:
            raw = private_bytes(path)
            config = tomllib.loads(raw.decode())
            context = self.context_arg or env.get('HCLOUD_CONTEXT') or config.get('active_context')
            need(isinstance(context, str) and bool(context), 'explicit cloud context required')
            contexts = config.get('contexts')
            need(isinstance(contexts, list), 'cloud contexts missing')
            matches = [item for item in contexts if isinstance(item, dict) and item.get('name') == context]
            need(len(matches) == 1, 'missing or ambiguous cloud context')
            selected = matches[0]
            global_preferences, preferences = config.get('preferences', {}), selected.get('preferences', {})
            need(isinstance(global_preferences, dict) and isinstance(preferences, dict), 'invalid cloud preferences')
            endpoint = env.get('HCLOUD_ENDPOINT') or preferences.get('endpoint') or global_preferences.get('endpoint') or ENDPOINT
            need(endpoint == ENDPOINT, 'nonofficial cloud endpoint refused')
            credential = env.get('HCLOUD_TOKEN') or selected.get('token')
            need(isinstance(credential, str) and bool(credential)
                 and all(33 <= ord(c) < 127 for c in credential), 'cloud credential unavailable or invalid')
            need(credential not in context and credential not in str(path), 'credential found in profile identity')
            binding = dict(context=context, config=str(path), config_sha256=digest(raw),
                           credential_sha256=digest(credential.encode()), endpoint=ENDPOINT,
                           auth_contract='hcloud-cli-v1.70.1-strict-context/v1')
            return binding, credential
        except (OSError, ValueError, TypeError, KeyError, UnicodeError):
            raise mc.GuardError('controller cloud profile unreadable or malformed') from None

    def revalidate(self, expected):
        binding, credential = self._resolve()
        need(binding == self.binding == expected, 'sealed cloud profile changed')
        self._credential = credential


@contextlib.contextmanager
def whole_call_deadline(seconds):
    """A main-thread wall deadline, including DNS; not a socket-inactivity timer."""
    need(0 < seconds <= 20, 'invalid cloud whole-call allowance')
    previous_handler = signal.getsignal(signal.SIGALRM)
    previous_timer = signal.getitimer(signal.ITIMER_REAL)
    need(previous_timer == (0.0, 0.0), 'nested cloud deadline refused')
    def expired(signum, frame):
        raise TimeoutError('cloud whole-call deadline')
    signal.signal(signal.SIGALRM, expired)
    signal.setitimer(signal.ITIMER_REAL, seconds)
    try:
        yield
    finally:
        signal.setitimer(signal.ITIMER_REAL, 0)
        signal.signal(signal.SIGALRM, previous_handler)


def sensitive_name(name):
    name = name.lower().replace('-', '_')
    return (bool(re.search(r'(^|_)(token|password|secret|authorization|credentials?)(_|$)', name))
            or 'private_key' in name or name in {'api_key', 'user_data'})


def credential_values(value):
    found = []
    if isinstance(value, dict):
        for key, item in value.items():
            if sensitive_name(key) and isinstance(item, str):
                found.append(item)
            found.extend(credential_values(item))
    elif isinstance(value, list):
        for item in value:
            found.extend(credential_values(item))
    return found


def redacted(value, secrets=()):
    if isinstance(value, str):
        for secret in secrets:
            if isinstance(secret, str) and secret:
                value = value.replace(secret, '<redacted-credential>')
        return value
    if isinstance(value, dict):
        out = {}
        for key, item in value.items():
            if sensitive_name(key):
                out[redacted(key, secrets) + '_redacted'] = True
            else:
                out[redacted(key, secrets)] = redacted(item, secrets)
        return out
    if isinstance(value, list):
        return [redacted(v, secrets) for v in value]
    return value


def contains_credential(value, credential):
    if isinstance(value, str):
        return credential in value
    if isinstance(value, dict):
        return any(contains_credential(k, credential) or contains_credential(v, credential) for k, v in value.items())
    if isinstance(value, (list, tuple)):
        return any(contains_credential(v, credential) for v in value)
    return False


def mutation_allowed(method, path, body):
    """No IP mutation, arbitrary URL, redirect, user endpoint or hidden replay."""
    if method == 'DELETE':
        return body is None and bool(re.fullmatch(r'/(servers|networks|firewalls|ssh_keys)/[1-9][0-9]*', path))
    if method != 'POST' or not isinstance(body, dict):
        return False
    fields = {'/networks': {'name', 'ip_range', 'labels'}, '/firewalls': {'name', 'rules', 'labels'},
              '/ssh_keys': {'name', 'public_key', 'labels'},
              '/servers': {'name', 'server_type', 'location', 'image', 'ssh_keys', 'firewalls',
                           'start_after_create', 'public_net', 'user_data', 'labels'}}
    if path in fields:
        if set(body) != fields[path]:
            return False
        if path == '/servers':
            return (body['server_type'] == 'ccx43' and body['location'] == 'fsn1'
                    and type(body['image']) is int and body['image'] == 161547269
                    and body['start_after_create'] is False
                    and type(body['public_net']) is dict
                    and set(body['public_net']) == {'enable_ipv4', 'enable_ipv6'}
                    and body['public_net']['enable_ipv4'] is True
                    and body['public_net']['enable_ipv6'] is False)
        return True
    if re.fullmatch(r'/networks/[1-9][0-9]*/actions/add_subnet', path):
        return set(body) == {'type', 'network_zone', 'ip_range'} and body['type'] == 'cloud'
    if re.fullmatch(r'/servers/[1-9][0-9]*/actions/attach_to_network', path):
        return set(body) == {'network', 'ip'} and type(body['network']) is int and body['network'] > 0
    return bool(re.fullmatch(r'/servers/[1-9][0-9]*/actions/poweron', path)) and body == {}


class CallBudget(float):
    """A relative allowance bound to one absolute controller deadline."""
    def __new__(cls, seconds, deadline_ns, clock, phase_deadline_ns=None, phase_clock=None):
        value = super().__new__(cls, seconds)
        value.deadline_ns, value.clock = deadline_ns, clock
        value.phase_deadline_ns, value.phase_clock = phase_deadline_ns, phase_clock
        return value


class Cloud:
    def __init__(self, profile, expected_binding, connection_factory=None, deadline=whole_call_deadline):
        self.profile, self.binding = profile, expected_binding
        self.connection_factory = connection_factory
        self.deadline = deadline

    def request(self, method, path, body=None, seconds=20, deadline_ns=None, clock=None):
        need(method == 'GET' or mutation_allowed(method, path, body), 'cloud mutation outside fixed campaign plan')
        need(bool(re.fullmatch(r'/[a-z_0-9/?=&-]+', path)) and '//' not in path, 'invalid fixed API path')
        allowance = seconds() if callable(seconds) else seconds
        need(0 < allowance <= 20, 'invalid cloud whole-call allowance')
        clock = clock or getattr(allowance, 'clock', time.monotonic_ns)
        deadline_ns = deadline_ns if deadline_ns is not None else getattr(allowance, 'deadline_ns', None)
        end = clock() + int(allowance * 1_000_000_000) if deadline_ns is None else deadline_ns
        def remaining():
            value = min(allowance, (end - clock()) / 1_000_000_000)
            phase_end = getattr(allowance, 'phase_deadline_ns', None)
            if phase_end is not None:
                value = min(value, (phase_end - allowance.phase_clock()) / 1_000_000_000)
            need(value > 0, 'absolute cloud lease expired before dispatch')
            return float(value)
        connection = None
        try:
            with self.deadline(remaining()):
                self.profile.revalidate(self.binding)
                data = wire_json(body) if body is not None else None
                need(not contains_credential(body, self.profile._credential), 'controller credential found in mutation body')
                seconds = remaining()
                if self.connection_factory is None:
                    connection = http.client.HTTPSConnection(HOST, timeout=seconds, context=ssl.create_default_context())
                else:
                    connection = self.connection_factory(HOST, seconds)
                remaining()
                connection.connect()
                remaining()
                connection.request(method, '/v1' + path, body=data,
                                   headers={'Authorization': 'Bearer ' + self.profile._credential,
                                            'Content-Type': 'application/json', 'User-Agent': 'fdb-bench-rfc283/1'})
                response = connection.getresponse()
                raw = response.read(MAX_RESPONSE + 1)
                need(len(raw) <= MAX_RESPONSE, 'cloud response too large')
                json_valid = True
                try:
                    payload = json.loads(raw, object_pairs_hook=json_object, parse_constant=invalid_constant) if raw else None
                except (ValueError, UnicodeError):
                    payload, json_valid = None, False
                secrets = [self.profile._credential]
                if isinstance(body, dict) and isinstance(body.get('user_data'), str):
                    secrets.append(body['user_data'])
                secrets.extend(credential_values(payload))
                # HTTPResponse ignores 204 content. Contradictory framing is
                # not proof of a bodyless acknowledgement, even when read() is empty.
                length = response.getheader('Content-Length')
                body_empty = (not raw and (length is None or re.fullmatch(r'0+', length.strip()) is not None)
                              and response.getheader('Transfer-Encoding') is None)
                return dict(status=response.status, payload=redacted(payload, secrets), json_valid=json_valid,
                            body_empty=body_empty, body_sha256=digest(raw),
                            provider_date=redacted(response.getheader('Date'), secrets), request_sha256=digest(data or b''))
        except (OSError, TimeoutError, http.client.HTTPException, ValueError, ssl.SSLError):
            raise mc.GuardError('cloud request failed; mutation outcome may be unknown') from None
        finally:
            if connection is not None:
                connection.close()

    def list(self, kind, seconds=20):
        plural = PLURALS[kind]
        rows, seen_pages, expected_total = [], set(), None
        page = 1
        while page is not None:
            need(type(page) is int and 0 < page <= 10000 and page not in seen_pages, 'invalid cloud pagination')
            seen_pages.add(page)
            reply = self.request('GET', f'/{plural}?page={page}&per_page=50', seconds=seconds)
            need(reply['status'] == 200 and isinstance(reply['payload'], dict), 'cloud census unavailable')
            payload = reply['payload']
            items = payload.get(plural)
            meta = payload.get('meta')
            need(isinstance(meta, dict), 'incomplete cloud census metadata')
            pagination = meta.get('pagination')
            need(isinstance(items, list) and isinstance(pagination, dict)
                 and 'next_page' in pagination, 'incomplete cloud census')
            total = pagination.get('total_entries')
            need(type(total) is int and total >= 0 and (expected_total is None or total == expected_total),
                 'changing cloud census population')
            expected_total = total
            rows.extend(items)
            page = pagination.get('next_page')
        ids = [item.get('id') if isinstance(item, dict) else None for item in rows]
        need(all(type(i) is int and i > 0 for i in ids) and len(ids) == len(set(ids)) == expected_total,
             'invalid or incomplete cloud census IDs')
        return rows

    def describe(self, kind, rid, seconds=20):
        need(type(rid) is int and rid > 0, 'invalid exact resource ID')
        reply = self.request('GET', f'/{PLURALS[kind]}/{rid}', seconds=seconds)
        if reply['status'] == 404:
            return None
        key = {'ssh-key': 'ssh_key', 'primary-ip': 'primary_ip', 'floating-ip': 'floating_ip',
               'load-balancer': 'load_balancer', 'placement-group': 'placement_group'}.get(kind, kind)
        need(reply['status'] == 200 and isinstance(reply['payload'], dict)
             and isinstance(reply['payload'].get(key), dict), 'exact resource metadata unavailable')
        item = reply['payload'][key]
        need(type(item.get('id')) is int and item['id'] == rid, 'exact resource ID differs')
        return item

    def resource_actions(self, kind, rid, seconds=20, on_page=None):
        need(isinstance(kind, str) and kind in {'server', 'firewall'} and type(rid) is int and rid > 0,
             'resource action lookup requires an exact owned server/firewall ID')
        rows, seen, page, total = [], set(), 1, None
        while page is not None:
            need(type(page) is int and 0 < page <= 10000 and page not in seen, 'invalid action pagination')
            seen.add(page)
            reply = self.request('GET', f'/{PLURALS[kind]}/{rid}/actions?page={page}&per_page=50', seconds=seconds)
            payload = reply['payload']
            need(reply['status'] == 200 and isinstance(payload, dict)
                 and isinstance(payload.get('actions'), list) and isinstance(payload.get('meta'), dict),
                 'resource action census unavailable')
            pagination = payload['meta'].get('pagination')
            need(isinstance(pagination, dict) and 'next_page' in pagination
                 and type(pagination.get('total_entries')) is int
                 and pagination['total_entries'] >= 0, 'incomplete resource action census')
            need(total is None or total == pagination['total_entries'], 'changing resource action census')
            total = pagination['total_entries']
            rows.extend(payload['actions'])
            ids = [a.get('id') if isinstance(a, dict) else None for a in rows]
            need(all(type(i) is int and i > 0 for i in ids) and len(ids) == len(set(ids)) <= total,
                 'invalid resource action census IDs')
            if on_page is not None:
                on_page(payload['actions'])
            page = pagination['next_page']
        ids = [a.get('id') if isinstance(a, dict) else None for a in rows]
        need(all(type(i) is int and i > 0 for i in ids) and len(ids) == len(set(ids)) == total,
             'invalid resource action census IDs')
        return rows

    def action(self, rid, seconds=20):
        need(type(rid) is int and rid > 0, 'invalid known action ID')
        reply = self.request('GET', f'/actions/{rid}', seconds=seconds)
        need(reply['status'] == 200 and isinstance(reply['payload'], dict)
             and isinstance(reply['payload'].get('action'), dict), 'known provider action unavailable')
        action = reply['payload']['action']
        need(type(action.get('id')) is int and action['id'] == rid
             and isinstance(action.get('status'), str)
             and action['status'] in {'running', 'success', 'error'}, 'invalid provider action')
        return action
