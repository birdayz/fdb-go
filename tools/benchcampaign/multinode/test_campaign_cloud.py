#!/usr/bin/env python3
"""RFC-283 cloud boundary tests using only private synthetic files and wire bytes.

The fake provider parses requests emitted by the real stdlib HTTPSConnection.
Its socket is entirely in memory: no DNS, socket connection, TLS handshake,
provider credential, CLI configuration, cloud operation or wall-clock alarm.
"""
import contextlib
import copy
import hashlib
import http.client
import io
import json
import os
from pathlib import Path
import ssl
import sys
import tempfile
import traceback
import types
import unittest
from unittest import mock

HERE = Path(os.path.abspath(__file__)).parent
sys.path.insert(0, str(HERE))

import campaign_cloud as cloud  # noqa: E402
import mn_common as mc  # noqa: E402


TOKEN = 'synthetic-alpha-not-a-provider-credential'
OTHER_TOKEN = 'synthetic-beta-not-a-provider-credential'
OVERRIDE_TOKEN = 'synthetic-override-not-a-provider-credential'
DATE = 'Tue, 27 Oct 2026 10:00:00 GMT'
HTTPS_CONNECTION = http.client.HTTPSConnection


def config_text(active='alpha', endpoint=None):
    text = f'active_context = "{active}"\n'
    if endpoint is not None:
        text += f'[preferences]\nendpoint = "{endpoint}"\n'
    return text + (f'[[contexts]]\nname = "alpha"\ntoken = "{TOKEN}"\n'
                   f'[[contexts]]\nname = "beta"\ntoken = "{OTHER_TOKEN}"\n')


def server_body():
    return dict(name='synthetic-node1', server_type='ccx43', location='fsn1', image=161547269,
                ssh_keys=[11], firewalls=[{'firewall': 12}], start_after_create=False,
                public_net={'enable_ipv4': True, 'enable_ipv6': False},
                user_data='synthetic private bootstrap', labels={'campaign': 'synthetic', 'nonce': 'a' * 32})


def page(items, total=None, next_page=None):
    return {'servers': items, 'meta': {'pagination': {
        'total_entries': len(items) if total is None else total, 'next_page': next_page}}}


def action_page(items, total=None, next_page=None):
    payload = page(items, total=total, next_page=next_page)
    payload['actions'] = payload.pop('servers')
    return payload


class MemorySocket:
    """Minimal socket surface used by HTTPConnection, with a provider-side parser."""

    def __init__(self, provider, reply):
        self.provider, self.reply = provider, reply
        self.wire = bytearray()
        self.committed = False
        self.closed = False

    def sendall(self, data):
        self.wire.extend(data)
        if self.committed or b'\r\n\r\n' not in self.wire:
            return
        header, body = bytes(self.wire).split(b'\r\n\r\n', 1)
        lines = header.decode('ascii').split('\r\n')
        method, path, version = lines[0].split(' ')
        if method == 'GET':
            sizes = [field.partition('=')[2] for field in path.partition('?')[2].split('&')
                     if field.partition('=')[0] == 'per_page']
            if any(int(size) > 50 for size in sizes):
                raise AssertionError('fake provider per_page exceeds documented maximum 50')
        headers = dict(line.split(': ', 1) for line in lines[1:])
        length = int(headers.get('Content-Length', '0'))
        if len(body) < length:
            return
        if len(body) != length:
            raise AssertionError('fake provider received trailing request bytes')
        self.committed = True
        self.provider.requests.append(dict(method=method, path=path, version=version,
                                           headers=headers, body=body))
        if method == 'POST':
            self.provider.posts += 1
        if self.reply.get('send_error'):
            raise self.reply['send_error']

    def makefile(self, mode):
        if not self.committed:
            raise AssertionError('response requested before complete provider request')
        if self.reply.get('lost'):
            # Provider has already committed/recorded the request, but no response
            # status or body arrives. Real HTTPResponse raises RemoteDisconnected.
            return io.BytesIO(b'')
        if self.reply.get('response_error'):
            raise self.reply['response_error']
        raw = self.reply.get('raw', b'{}')
        status = self.reply.get('status', 200)
        headers = {'Content-Length': str(len(raw)), 'Date': DATE, **self.reply.get('headers', {})}
        head = f'HTTP/1.1 {status} Fake\r\n' + ''.join(f'{k}: {v}\r\n' for k, v in headers.items())
        return io.BytesIO(head.encode('ascii') + b'\r\n' + raw)

    def close(self):
        self.closed = True


class MemoryProvider:
    def __init__(self, *replies):
        self.replies = list(replies)
        self.requests, self.connections, self.sockets = [], [], []
        self.posts = 0

    def connection(self, host, seconds):
        if not self.replies:
            raise AssertionError('unexpected additional connection/retry')
        connection = HTTPS_CONNECTION(host, timeout=seconds, context=ssl.create_default_context())
        sock = MemorySocket(self, self.replies.pop(0))
        connection.connect = lambda: setattr(connection, 'sock', sock)
        self.connections.append(connection)
        self.sockets.append(sock)
        return connection

    @staticmethod
    def json(value, status=200):
        return {'status': status, 'raw': json.dumps(value).encode()}


class SyntheticFiles(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='campaign-cloud-test-')
        self.addCleanup(self.temp.cleanup)
        self.tmp = Path(self.temp.name)
        self.path = self.write_config('cli.toml', config_text())
        # Even accidentally using ambient resolution cannot read the controller's
        # credentials. The OS-home lookup and process environment are synthetic.
        self.env = mock.patch.dict(os.environ, {}, clear=True)
        self.env.start()
        self.addCleanup(self.env.stop)
        self.home = mock.patch.object(cloud.pwd, 'getpwuid', return_value=types.SimpleNamespace(pw_dir=str(self.tmp)))
        self.home.start()
        self.addCleanup(self.home.stop)

    def write_config(self, name, text):
        path = self.tmp / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text)
        path.chmod(0o600)
        return path

    def profile(self, **kwargs):
        return cloud.Profile(config=self.path, environ={}, **kwargs)

    @contextlib.contextmanager
    def after_credential_read(self, change):
        """Apply a filesystem change after bytes arrive, before final lstat."""
        original = os.fdopen
        @contextlib.contextmanager
        def changing_stream(fd, mode):
            with original(fd, mode) as stream:
                def read(size):
                    data = stream.read(size)
                    change()
                    return data
                yield types.SimpleNamespace(read=read)
        with mock.patch.object(cloud.os, 'fdopen', side_effect=changing_stream):
            yield

    def refusal(self, fn, *args, message=None, **kwargs):
        with self.assertRaises(mc.GuardError) as caught:
            fn(*args, **kwargs)
        if message:
            self.assertIn(message, str(caught.exception))
        rendered = ''.join(traceback.format_exception(caught.exception))
        for token in (TOKEN, OTHER_TOKEN, OVERRIDE_TOKEN):
            self.assertNotIn(token, rendered)
        return caught.exception


class ProfileTest(SyntheticFiles):
    def test_explicit_context_beats_environment_and_active_context(self):
        profile = cloud.Profile(context='beta', config=self.path, environ={'HCLOUD_CONTEXT': 'alpha'})
        self.assertEqual(profile.binding['context'], 'beta')
        self.assertEqual(profile.binding['credential_sha256'], hashlib.sha256(OTHER_TOKEN.encode()).hexdigest())

    def test_environment_context_beats_active_context(self):
        profile = cloud.Profile(config=self.path, environ={'HCLOUD_CONTEXT': 'beta'})
        self.assertEqual(profile.binding['context'], 'beta')
        self.assertEqual(profile._credential, OTHER_TOKEN)

    def test_credential_cannot_appear_in_context_identity(self):
        text = config_text(active=TOKEN).replace('name = "alpha"', f'name = "{TOKEN}"')
        self.write_config('cli.toml', text)
        self.refusal(self.profile, message='credential found in profile identity')

    def test_credential_cannot_appear_in_configuration_path(self):
        path = self.write_config(TOKEN + '.toml', config_text())
        self.refusal(cloud.Profile, config=path, environ={}, message='credential found in profile identity')

    def test_active_context_and_exact_nonsecret_binding(self):
        profile = self.profile()
        self.assertEqual(profile.binding, {
            'context': 'alpha', 'config': str(self.path.absolute()),
            'config_sha256': hashlib.sha256(self.path.read_bytes()).hexdigest(),
            'credential_sha256': hashlib.sha256(TOKEN.encode()).hexdigest(),
            'endpoint': 'https://api.hetzner.cloud/v1', 'auth_contract': 'hcloud-cli-v1.70.1-strict-context/v1'})
        profile.revalidate(copy.deepcopy(profile.binding))
        self.assertEqual(profile._credential, TOKEN)
        for representation in (repr(profile), str(profile), repr(profile.binding), json.dumps(profile.binding)):
            self.assertNotIn(TOKEN, representation)
            self.assertNotIn(OTHER_TOKEN, representation)

    def test_explicit_config_beats_environment_config(self):
        alternate = self.write_config('alternate.toml', config_text(active='beta'))
        profile = cloud.Profile(config=self.path, environ={'HCLOUD_CONFIG': str(alternate)})
        self.assertEqual(profile.binding['config'], str(self.path))
        self.assertEqual(profile.binding['context'], 'alpha')

    def test_environment_config_beats_default_os_home(self):
        default = self.write_config('.config/hcloud/cli.toml', config_text(active='alpha'))
        alternate = self.write_config('alternate.toml', config_text(active='beta'))
        profile = cloud.Profile(environ={'HCLOUD_CONFIG': str(alternate)})
        self.assertEqual(profile.binding['config'], str(alternate))
        self.assertNotEqual(profile.binding['config'], str(default))
        self.assertEqual(profile.binding['context'], 'beta')

    def test_default_config_uses_os_home_not_home_or_xdg_environment(self):
        default = self.write_config('.config/hcloud/cli.toml', config_text(active='beta'))
        profile = cloud.Profile(environ={'HOME': str(self.tmp / 'wrong-home'),
                                         'XDG_CONFIG_HOME': str(self.tmp / 'wrong-xdg')})
        self.assertEqual(profile.binding['config'], str(default))
        self.assertEqual(profile.binding['context'], 'beta')

    def test_none_environ_uses_process_hcloud_values(self):
        with mock.patch.dict(os.environ, {'HCLOUD_CONFIG': str(self.path), 'HCLOUD_CONTEXT': 'beta',
                                         'HCLOUD_TOKEN': OVERRIDE_TOKEN}):
            profile = cloud.Profile()
        self.assertEqual(profile.binding['context'], 'beta')
        self.assertEqual(profile._credential, OVERRIDE_TOKEN)

    def test_token_override_retains_unique_configured_context(self):
        profile = cloud.Profile(config=self.path, environ={'HCLOUD_TOKEN': OVERRIDE_TOKEN})
        self.assertEqual(profile.binding['context'], 'alpha')
        self.assertEqual(profile._credential, OVERRIDE_TOKEN)
        self.assertEqual(profile.binding['credential_sha256'], hashlib.sha256(OVERRIDE_TOKEN.encode()).hexdigest())
        self.assertNotIn(OVERRIDE_TOKEN, repr(profile))

    def test_override_can_supply_token_for_unique_tokenless_context(self):
        self.write_config('cli.toml', 'active_context = "alpha"\n[[contexts]]\nname = "alpha"\n')
        profile = cloud.Profile(config=self.path, environ={'HCLOUD_TOKEN': OVERRIDE_TOKEN})
        self.assertEqual(profile.binding['context'], 'alpha')
        self.assertEqual(profile._credential, OVERRIDE_TOKEN)

    def test_explicit_context_does_not_require_active_context(self):
        self.write_config('cli.toml', config_text().split('\n', 1)[1])
        self.assertEqual(self.profile(context='beta').binding['context'], 'beta')
        self.assertEqual(cloud.Profile(config=self.path, environ={'HCLOUD_CONTEXT': 'beta'}).binding['context'], 'beta')

    def test_empty_environment_selectors_token_and_endpoint_use_defaults(self):
        default = self.write_config('.config/hcloud/cli.toml', config_text(active='beta'))
        profile = cloud.Profile(environ={key: '' for key in
                                         ('HCLOUD_CONFIG', 'HCLOUD_CONTEXT', 'HCLOUD_TOKEN', 'HCLOUD_ENDPOINT')})
        self.assertEqual(profile.binding['config'], str(default))
        self.assertEqual(profile.binding['context'], 'beta')
        self.assertEqual(profile._credential, OTHER_TOKEN)
        self.assertEqual(profile.binding['endpoint'], 'https://api.hetzner.cloud/v1')

    def test_token_override_cannot_bypass_context_or_file_requirements(self):
        documents = [
            '', 'active_context = "missing"\n' + config_text().split('\n', 1)[1],
            'active_context = "alpha"\n',
            config_text() + '[[contexts]]\nname = "alpha"\ntoken = "duplicate"\n',
            'active_context = 7\ncontexts = []\n', 'active_context = "alpha"\ncontexts = {}\n',
        ]
        for document in documents:
            with self.subTest(document=document):
                self.write_config('cli.toml', document)
                self.refusal(cloud.Profile, config=self.path, environ={'HCLOUD_TOKEN': OVERRIDE_TOKEN})
        self.path.unlink()
        self.refusal(cloud.Profile, config=self.path, environ={'HCLOUD_TOKEN': OVERRIDE_TOKEN})

    def test_explicit_missing_context_cannot_fall_back(self):
        self.refusal(self.profile, context='missing', message='missing or ambiguous')

    def test_missing_invalid_or_whitespace_credential(self):
        for value in (' ', 'bad token', 'bad\ttoken', 'bad\ntoken', 7, True, ['bad']):
            with self.subTest(value=value):
                self.refusal(cloud.Profile, config=self.path, environ={'HCLOUD_TOKEN': value})
        for token_line in ('', 'token = 7\n', 'token = ""\n', 'token = "contains space"\n'):
            self.write_config('cli.toml', 'active_context = "alpha"\n[[contexts]]\nname = "alpha"\n' + token_line)
            self.refusal(self.profile)

    def test_malformed_toml_and_utf8_never_disclose_bytes(self):
        self.write_config('cli.toml', f'active_context = "{TOKEN}"\nnot valid = [')
        self.refusal(self.profile)
        self.path.write_bytes(b'\xff' + TOKEN.encode())
        self.refusal(self.profile)

    def test_global_and_context_preferences_must_be_tables(self):
        for document in (
                'preferences = "wrong"\n' + config_text(),
                config_text() + 'preferences = ["wrong"]\n',
                config_text().replace(f'token = "{TOKEN}"', f'token = "{TOKEN}"\npreferences = false')):
            with self.subTest(document=document):
                self.write_config('cli.toml', document)
                # Select the context whose malformed preferences are exercised.
                context = 'beta' if 'preferences = ["wrong"]' in document else 'alpha'
                self.refusal(self.profile, context=context, message='preferences')

    def test_official_endpoint_at_each_precedence_level_is_allowed(self):
        for scope in ('global', 'context', 'environment'):
            with self.subTest(scope=scope):
                text = config_text(endpoint='https://api.hetzner.cloud/v1') if scope == 'global' else config_text()
                if scope == 'context':
                    text += '[contexts.preferences]\nendpoint = "https://api.hetzner.cloud/v1"\n'
                self.write_config('cli.toml', text)
                env = {'HCLOUD_CONTEXT': 'beta'}
                if scope == 'environment':
                    env['HCLOUD_ENDPOINT'] = 'https://api.hetzner.cloud/v1'
                self.assertEqual(cloud.Profile(config=self.path, environ=env).binding['endpoint'],
                                 'https://api.hetzner.cloud/v1')

    def test_custom_endpoints_are_refused_at_every_precedence_level(self):
        for endpoint in ('http://api.hetzner.cloud/v1', 'https://example.invalid/v1',
                         'https://api.hetzner.cloud/v1/', 'https://api.hetzner.cloud.evil.invalid/v1'):
            for scope in ('global', 'context', 'environment'):
                with self.subTest(endpoint=endpoint, scope=scope):
                    text = config_text(endpoint=endpoint) if scope == 'global' else config_text()
                    if scope == 'context':
                        text += f'[contexts.preferences]\nendpoint = "{endpoint}"\n'
                    self.write_config('cli.toml', text)
                    env = {'HCLOUD_CONTEXT': 'beta'}
                    if scope == 'environment':
                        env['HCLOUD_ENDPOINT'] = endpoint
                    self.refusal(cloud.Profile, config=self.path, environ=env, message='endpoint')

    def test_endpoint_precedence_environment_then_context_then_global(self):
        self.write_config('cli.toml', config_text(endpoint='https://global.invalid/v1') +
                          '[contexts.preferences]\nendpoint = "https://api.hetzner.cloud/v1"\n')
        self.assertEqual(self.profile(context='beta').binding['endpoint'], 'https://api.hetzner.cloud/v1')
        self.write_config('cli.toml', config_text(endpoint='https://global.invalid/v1') +
                          '[contexts.preferences]\nendpoint = "https://context.invalid/v1"\n')
        self.assertEqual(cloud.Profile(config=self.path, environ={
            'HCLOUD_CONTEXT': 'beta', 'HCLOUD_ENDPOINT': 'https://api.hetzner.cloud/v1'}).binding['endpoint'],
            'https://api.hetzner.cloud/v1')

    def test_regular_owned_0600_file_is_accepted(self):
        self.assertEqual(cloud.private_bytes(self.path), self.path.read_bytes())

    def test_file_permissions_other_than_0600_are_refused(self):
        for mode in (0o400, 0o640, 0o644, 0o660, 0o700, 0o6000 | 0o600):
            with self.subTest(mode=oct(mode)):
                self.path.chmod(mode)
                self.refusal(self.profile, message='0600')
        self.path.chmod(0o600)

    def test_nonregular_file_is_refused(self):
        self.refusal(cloud.Profile, config=self.tmp, environ={}, message='0600')

    def test_fifo_is_opened_nonblocking_and_refused_without_reading(self):
        fifo = self.tmp / 'credential.fifo'
        os.mkfifo(fifo, 0o600)
        original = os.open
        opened = []
        def nonblocking_open(path, flags):
            # Assert before opening: removing NONBLOCK must fail this control,
            # not hang the test waiting for a FIFO writer that never exists.
            self.assertTrue(flags & os.O_NONBLOCK)
            self.assertTrue(flags & os.O_NOFOLLOW)
            self.assertTrue(flags & os.O_CLOEXEC)
            fd = original(path, flags)
            opened.append(fd)
            return fd
        with mock.patch.object(cloud.os, 'open', side_effect=nonblocking_open), \
                mock.patch.object(cloud.os, 'fdopen') as read_stream, \
                mock.patch.object(cloud.os, 'close', wraps=os.close) as close:
            self.refusal(cloud.Profile, config=fifo, environ={}, message='0600')
            read_stream.assert_not_called()
            self.assertEqual(len(opened), 1)
            close.assert_called_once_with(opened[0])

    def test_hardlinked_credential_is_refused_through_both_names(self):
        self.assertEqual(self.profile()._credential, TOKEN)
        alias = self.tmp / 'hardlink.toml'
        os.link(self.path, alias)
        self.assertEqual(self.path.stat().st_nlink, 2)
        for path in (self.path, alias):
            with self.subTest(path=path):
                self.refusal(cloud.Profile, config=path, environ={}, message='0600')
        alias.unlink()
        self.assertEqual(self.path.stat().st_nlink, 1)
        self.assertEqual(self.profile()._credential, TOKEN)

    def test_mode_changed_after_read_is_refused(self):
        with self.after_credential_read(lambda: self.path.chmod(0o644)):
            self.refusal(self.profile, message='changed during read')
        self.assertEqual(self.path.stat().st_mode & 0o777, 0o644)
        self.path.chmod(0o600)
        self.assertEqual(self.profile()._credential, TOKEN)

    def test_uid_changed_after_read_is_refused_without_requiring_chown(self):
        real_lstat = Path.lstat
        changed = []
        def changed_lstat(path, *args, **kwargs):
            info = real_lstat(path, *args, **kwargs)
            if path != self.path or not changed:
                return info
            fields = ('st_mode', 'st_uid', 'st_dev', 'st_ino', 'st_size', 'st_mtime_ns', 'st_nlink')
            values = {key: getattr(info, key) for key in fields}
            values['st_uid'] = info.st_uid + 1
            return types.SimpleNamespace(**values)
        with self.after_credential_read(lambda: changed.append(True)), \
                mock.patch.object(Path, 'lstat', new=changed_lstat):
            self.refusal(self.profile, message='changed during read')
        self.assertEqual(changed, [True])
        self.assertEqual(self.profile()._credential, TOKEN)

    def test_hardlink_added_after_read_is_refused(self):
        alias = self.tmp / 'late-hardlink.toml'
        with self.after_credential_read(lambda: os.link(self.path, alias)):
            self.refusal(self.profile, message='changed during read')
        self.assertEqual(self.path.stat().st_nlink, 2)
        alias.unlink()
        self.assertEqual(self.profile()._credential, TOKEN)

    def test_other_owner_is_refused(self):
        real = os.fstat
        def foreign_owner(fd):
            info = real(fd)
            return types.SimpleNamespace(st_mode=info.st_mode, st_uid=os.geteuid() + 1)
        with mock.patch.object(cloud.os, 'fstat', side_effect=foreign_owner):
            self.refusal(self.profile, message='0600')

    def test_leaf_and_parent_symlinks_are_refused(self):
        leaf = self.tmp / 'link.toml'
        leaf.symlink_to(self.path)
        self.refusal(cloud.Profile, config=leaf, environ={})
        linkdir = self.tmp / 'linked'
        linkdir.symlink_to(self.tmp, target_is_directory=True)
        self.refusal(cloud.Profile, config=linkdir / 'cli.toml', environ={}, message='symlink')

    def test_file_changed_during_read_is_refused(self):
        real = os.fstat
        for field in ('st_dev', 'st_ino', 'st_size', 'st_mtime_ns'):
            with self.subTest(field=field):
                def stale_stat(fd):
                    info = real(fd)
                    values = {key: getattr(info, key) for key in
                              ('st_mode', 'st_uid', 'st_dev', 'st_ino', 'st_size', 'st_mtime_ns', 'st_nlink')}
                    values[field] += 1
                    return types.SimpleNamespace(**values)
                with mock.patch.object(cloud.os, 'fstat', side_effect=stale_stat):
                    self.refusal(self.profile, message='changed during read')

    def test_oversized_credential_file_is_refused(self):
        with mock.patch.object(cloud, 'MAX_RESPONSE', 8):
            self.refusal(self.profile, message='too large')

    def test_revalidation_rejects_expected_binding_tampering(self):
        profile = self.profile()
        for key in profile.binding:
            with self.subTest(key=key):
                expected = copy.deepcopy(profile.binding)
                expected[key] = 'changed'
                self.refusal(profile.revalidate, expected, message='changed')

    def test_revalidation_rejects_config_and_token_drift(self):
        environment = {'HCLOUD_TOKEN': OVERRIDE_TOKEN}
        profile = cloud.Profile(config=self.path, environ=environment)
        binding = copy.deepcopy(profile.binding)
        self.path.write_text(config_text() + '# changed bytes\n')
        self.refusal(profile.revalidate, binding, message='changed')
        self.write_config('cli.toml', config_text())
        environment['HCLOUD_TOKEN'] = OTHER_TOKEN
        self.refusal(profile.revalidate, binding, message='changed')


class CloudWireTest(SyntheticFiles):
    def setUp(self):
        super().setUp()
        self.selected = self.profile()

    def client(self, provider, deadline=contextlib.nullcontext):
        return cloud.Cloud(self.selected, copy.deepcopy(self.selected.binding),
                           connection_factory=provider.connection, deadline=deadline)

    def test_equal_valued_float_image_refuses_before_connection_but_integer_posts_once(self):
        body = server_body()
        self.assertIs(type(body['image']), int)
        self.assertEqual(body['image'], 161547269)
        invalid = copy.deepcopy(body)
        invalid['image'] = 161547269.0
        self.assertIs(type(invalid['image']), float)
        self.assertEqual(invalid['image'], body['image'])
        provider = MemoryProvider(MemoryProvider.json({'server': {'id': 71}}, 201))
        client = self.client(provider)
        with self.assertRaisesRegex(mc.GuardError, '^cloud mutation outside fixed campaign plan$'):
            client.request('POST', '/servers', invalid)
        self.assertEqual((provider.posts, provider.requests, provider.connections, provider.sockets), (0, [], [], []))
        self.assertEqual(client.request('POST', '/servers', body)['status'], 201)
        self.assertEqual((provider.posts, len(provider.requests), len(provider.connections)), (1, 1, 1))
        request = provider.requests[0]
        self.assertEqual((request['method'], request['path'], request['version']), ('POST', '/v1/servers', 'HTTP/1.1'))
        sent = json.loads(request['body'])
        self.assertEqual(sent, body)
        self.assertIs(type(sent['image']), int)
        self.assertEqual(sent['image'], 161547269)
        self.assertEqual(len(provider.sockets), 1)
        self.assertTrue(provider.sockets[0].closed)

    def test_embedded_controller_credential_refuses_before_any_http_dispatch(self):
        changes = [('name', 'prefix-' + TOKEN), ('user_data', 'bootstrap ' + TOKEN),
                   ('labels', {'nested': [{'value': 'prefix ' + TOKEN}]}),
                   ('labels', {TOKEN: 'value'}), ('labels', {'nested': (TOKEN,)})]
        for field, value in changes:
            with self.subTest(field=field, value=value):
                body = server_body()
                body[field] = value
                provider = MemoryProvider(MemoryProvider.json({'server': {'id': 71}}, 201))
                self.refusal(self.client(provider).request, 'POST', '/servers', body,
                             message='credential found in mutation body')
                self.assertEqual((provider.posts, provider.requests, provider.connections), (0, [], []))

    def test_structured_201_raw_body_digest_and_secret_presence_facts(self):
        payload = {'server': {'id': 71, 'name': 'synthetic-node1'},
                   'action': {'id': 81, 'status': 'running'}, 'next_actions': [{'id': 82}],
                   'root_password': 'synthetic-root-secret', 'TOKEN': TOKEN,
                   'nested': [{'Authorization': 'synthetic-header-secret',
                               'password': 'synthetic-password', 'user_data': 'synthetic-private-data'}]}
        provider = MemoryProvider(MemoryProvider.json(payload, 201))
        body = server_body()
        reply = self.client(provider).request('POST', '/servers', body, seconds=7)
        raw = json.dumps(payload).encode()
        self.assertEqual(reply['status'], 201)
        self.assertEqual(reply['payload'], {
            'server': {'id': 71, 'name': 'synthetic-node1'},
            'action': {'id': 81, 'status': 'running'}, 'next_actions': [{'id': 82}],
            'root_password_redacted': True, 'TOKEN_redacted': True,
            'nested': [{'Authorization_redacted': True, 'password_redacted': True, 'user_data_redacted': True}]})
        self.assertEqual(reply['body_sha256'], hashlib.sha256(raw).hexdigest())
        self.assertEqual(reply['request_sha256'], hashlib.sha256(cloud.wire_json(body)).hexdigest())
        self.assertEqual(reply['provider_date'], DATE)
        self.assertIs(reply['json_valid'], True)
        self.assertIs(reply['body_empty'], False)
        self.assertEqual(provider.posts, 1)
        self.assertEqual(len(provider.requests), 1)
        request = provider.requests[0]
        self.assertEqual((request['method'], request['path'], request['version']), ('POST', '/v1/servers', 'HTTP/1.1'))
        self.assertEqual(request['headers']['Host'], cloud.HOST)
        self.assertEqual(request['headers']['Authorization'], 'Bearer ' + TOKEN)
        self.assertEqual(request['headers']['Content-Type'], 'application/json')
        self.assertEqual(json.loads(request['body']), body)
        self.assertTrue(provider.sockets[0].closed)
        for secret in (TOKEN, 'synthetic-root-secret', 'synthetic-header-secret',
                       'synthetic-password', 'synthetic-private-data'):
            self.assertNotIn(secret, json.dumps(reply))

    def test_all_eleven_campaign_operations_emit_one_exact_request_each(self):
        operations = [
            ('POST', '/servers', server_body()),
            ('POST', '/networks', {'name': 'synthetic-net', 'ip_range': '10.0.0.0/16', 'labels': {}}),
            ('POST', '/firewalls', {'name': 'synthetic-fw', 'rules': [], 'labels': {}}),
            ('POST', '/ssh_keys', {'name': 'synthetic-key', 'public_key': 'synthetic-public-key', 'labels': {}}),
            ('POST', '/networks/21/actions/add_subnet', {'type': 'cloud', 'network_zone': 'eu-central',
                                                        'ip_range': '10.0.0.0/24'}),
            ('POST', '/servers/71/actions/attach_to_network', {'network': 21, 'ip': '10.0.0.2'}),
            ('POST', '/servers/71/actions/poweron', {}),
            *[('DELETE', f'/{plural}/71', None) for plural in ('servers', 'networks', 'firewalls', 'ssh_keys')],
        ]
        provider = MemoryProvider(*[MemoryProvider.json({}, 201) for _ in operations])
        client = self.client(provider)
        for method, path, body in operations:
            with self.subTest(method=method, path=path):
                before = len(provider.requests)
                reply = client.request(method, path, body)
                self.assertEqual(reply['status'], 201)
                self.assertEqual(len(provider.requests), before + 1)
                self.assertEqual(provider.requests[-1]['method'], method)
                self.assertEqual(provider.requests[-1]['path'], '/v1' + path)
                self.assertEqual(provider.requests[-1]['body'], b'' if body is None else cloud.wire_json(body))
        self.assertEqual((len(provider.connections), len(provider.requests), provider.posts), (11, 11, 7))
        self.assertTrue(all(sock.closed for sock in provider.sockets))

    def test_provider_commit_then_response_loss_sends_exactly_one_post(self):
        provider = MemoryProvider({'lost': True})
        self.refusal(self.client(provider).request, 'POST', '/servers', server_body(),
                     message='outcome may be unknown')
        self.assertEqual(provider.posts, 1)
        self.assertEqual(len(provider.connections), 1)
        self.assertEqual(len(provider.requests), 1)
        self.assertEqual(json.loads(provider.requests[0]['body']), server_body())
        self.assertTrue(provider.sockets[0].closed)

    def test_send_failure_after_provider_commit_is_not_retried(self):
        provider = MemoryProvider({'send_error': BrokenPipeError(TOKEN)})
        self.refusal(self.client(provider).request, 'POST', '/servers', server_body(), message='unknown')
        self.assertEqual(provider.posts, 1)
        self.assertEqual(len(provider.connections), 1)
        self.assertTrue(provider.sockets[0].closed)

    def test_failed_http_response_is_retained_once_not_retried(self):
        for status in (400, 401, 403, 409, 429, 500, 502, 504):
            with self.subTest(status=status):
                payload = {'error': {'code': 'synthetic_failure', 'message': 'request refused', 'token': TOKEN}}
                raw = json.dumps(payload).encode()
                provider = MemoryProvider({'status': status, 'raw': raw})
                reply = self.client(provider).request('POST', '/servers', server_body())
                self.assertEqual(reply['status'], status)
                self.assertEqual(reply['payload']['error'], {
                    'code': 'synthetic_failure', 'message': 'request refused', 'token_redacted': True})
                self.assertEqual(reply['body_sha256'], hashlib.sha256(raw).hexdigest())
                self.assertNotIn(TOKEN, repr(reply))
                self.assertEqual(provider.posts, 1)
                self.assertEqual(len(provider.connections), 1)

    def test_redirect_response_never_follows_location_or_resends_credentials(self):
        for status in (301, 302, 303, 307, 308):
            with self.subTest(status=status):
                provider = MemoryProvider({'status': status, 'headers': {'Location': 'https://example.invalid/steal'},
                                           'raw': b'{"error":"redirect"}'})
                reply = self.client(provider).request('POST', '/servers', server_body())
                self.assertEqual(reply['status'], status)
                self.assertEqual(provider.posts, 1)
                self.assertEqual(len(provider.requests), 1)
                self.assertEqual(len(provider.connections), 1)
                self.assertEqual(provider.requests[0]['headers']['Host'], cloud.HOST)
                self.assertTrue(provider.sockets[0].closed)

    def test_empty_and_malformed_json_keep_status_and_raw_digest(self):
        for raw in (b'', b'not JSON', b'{"server":', b'\xff', b'null',
                    b'{"server":{"id":1,"id":2}}', b'{"counter":NaN}', b'{"counter":Infinity}'):
            with self.subTest(raw=raw):
                provider = MemoryProvider({'status': 201, 'raw': raw})
                reply = self.client(provider).request('POST', '/servers', server_body())
                self.assertEqual(reply['status'], 201)
                self.assertIsNone(reply['payload'])
                self.assertIs(reply['json_valid'], raw in (b'', b'null'))
                self.assertIs(reply['body_empty'], raw == b'')
                self.assertEqual(reply['body_sha256'], hashlib.sha256(raw).hexdigest())
                self.assertEqual(provider.posts, 1)
                self.assertTrue(provider.sockets[0].closed)

    def test_response_read_errors_are_sanitized_and_connection_is_closed(self):
        for error in (OSError(TOKEN), TimeoutError(TOKEN), http.client.BadStatusLine(TOKEN), ssl.SSLError(TOKEN)):
            with self.subTest(error=type(error).__name__):
                provider = MemoryProvider({'response_error': error})
                self.refusal(self.client(provider).request, 'POST', '/servers', server_body(), message='unknown')
                self.assertEqual(provider.posts, 1)
                self.assertTrue(provider.sockets[0].closed)

    def test_response_size_limit_refuses_without_retry(self):
        provider = MemoryProvider({'raw': b' ' * 33})
        with mock.patch.object(cloud, 'MAX_RESPONSE', 32):
            # The profile is already sealed; do not make the credential read hit
            # this deliberately tiny response-only limit first.
            with mock.patch.object(self.selected, 'revalidate') as validate:
                self.refusal(self.client(provider).request, 'POST', '/servers', server_body(), message='too large')
        validate.assert_called_once()
        self.assertEqual(provider.posts, 1)
        self.assertTrue(provider.sockets[0].closed)

    def test_binding_drift_refuses_before_connection_or_post(self):
        for drift in ('file', 'environment', 'expected'):
            with self.subTest(drift=drift):
                self.write_config('cli.toml', config_text())
                env = {'HCLOUD_TOKEN': TOKEN}
                self.selected = cloud.Profile(config=self.path, environ=env)
                provider = MemoryProvider(MemoryProvider.json({'server': {'id': 71}}, 201))
                client = self.client(provider)
                if drift == 'file':
                    self.path.write_text(config_text() + '# drift\n')
                elif drift == 'environment':
                    env['HCLOUD_TOKEN'] = OTHER_TOKEN
                else:
                    client.binding['context'] = 'beta'
                self.refusal(client.request, 'POST', '/servers', server_body(), message='changed')
                self.assertEqual((provider.posts, provider.requests, provider.connections), (0, [], []))

    def test_default_transport_uses_fixed_host_and_validating_tls_context(self):
        provider = MemoryProvider(MemoryProvider.json({'servers': []}))
        captured = []
        def factory(host, timeout, context):
            captured.append((host, timeout, context))
            # Python's stdlib constructor resolves its own module-level class in
            # super(); restore that binding while constructing the real object.
            with mock.patch.object(cloud.http.client, 'HTTPSConnection', HTTPS_CONNECTION):
                return provider.connection(host, timeout)
        client = cloud.Cloud(self.selected, self.selected.binding, deadline=contextlib.nullcontext)
        with mock.patch.object(cloud.http.client, 'HTTPSConnection', side_effect=factory):
            reply = client.request('GET', '/servers?page=1&per_page=50', seconds=9)
        self.assertEqual(reply['status'], 200)
        self.assertEqual(len(captured), 1)
        host, timeout, context = captured[0]
        self.assertEqual(host, 'api.hetzner.cloud')
        self.assertGreater(timeout, 0)
        self.assertLessEqual(timeout, 9)
        self.assertTrue(context.check_hostname)
        self.assertEqual(context.verify_mode, ssl.CERT_REQUIRED)
        self.assertGreater(context.cert_store_stats()['x509_ca'], 0)
        self.assertEqual(provider.requests[0]['path'], '/v1/servers?page=1&per_page=50')
        self.assertEqual(reply['request_sha256'], hashlib.sha256(b'').hexdigest())

    def test_injected_whole_call_deadline_wraps_connect_send_read(self):
        events = []
        @contextlib.contextmanager
        def deadline(seconds):
            events.append(('enter', seconds))
            yield
            events.append(('exit', seconds))
        provider = MemoryProvider(MemoryProvider.json({}))
        def factory(host, seconds):
            self.assertEqual(len(events), 1)
            self.assertEqual(events[0][0], 'enter')
            self.assertGreater(events[0][1], 0)
            self.assertLessEqual(events[0][1], 6)
            self.assertGreater(seconds, 0)
            self.assertLessEqual(seconds, events[0][1])
            return provider.connection(host, seconds)
        client = cloud.Cloud(self.selected, self.selected.binding, connection_factory=factory, deadline=deadline)
        client.request('GET', '/servers', seconds=6)
        self.assertEqual(events, [('enter', events[0][1]), ('exit', events[0][1])])
        self.assertTrue(provider.sockets[0].closed)

    def test_deadline_failure_before_connect_sends_nothing(self):
        @contextlib.contextmanager
        def deadline(seconds):
            raise TimeoutError(TOKEN)
            yield
        provider = MemoryProvider(MemoryProvider.json({}))
        self.refusal(self.client(provider, deadline).request, 'POST', '/servers', server_body(), message='unknown')
        self.assertEqual((provider.posts, provider.connections), (0, []))

    def test_arbitrary_method_url_ip_delete_and_mutation_paths_refuse_before_call(self):
        attempts = [
            ('PATCH', '/servers/1', {}), ('PUT', '/servers/1', {}), ('post', '/servers', server_body()),
            ('POST', 'https://example.invalid/servers', server_body()),
            ('POST', '//servers', server_body()), ('POST', '/servers/1/actions/rebuild', {}),
            ('DELETE', '/primary_ips/1', None), ('DELETE', '/floating_ips/1', None),
            ('POST', '/primary_ips/1/actions/change_protection', {'delete': False}),
            ('POST', '/primary_ips/1/actions/assign', {'assignee_id': 1, 'assignee_type': 'server'}),
            ('POST', '/servers/1/actions/detach_from_network', {'network': 1}),
            ('POST', '/servers/1/actions/change_dns_ptr', {}),
            ('DELETE', '/servers/0', None), ('DELETE', '/servers/01', None),
            ('DELETE', '/servers/-1', None), ('DELETE', '/servers/1?force=true', None),
            ('DELETE', '/servers/1', {}), ('GET', 'https://example.invalid/servers', None),
            ('GET', '//servers', None), ('GET', '/servers#fragment', None),
        ]
        for method, path, body in attempts:
            with self.subTest(method=method, path=path):
                provider = MemoryProvider(MemoryProvider.json({}))
                self.refusal(self.client(provider).request, method, path, body)
                self.assertEqual((provider.posts, provider.connections), (0, []))

    def test_credential_value_echoed_outside_secret_named_fields_is_redacted(self):
        provider = MemoryProvider(MemoryProvider.json({
            'error': {'message': 'provider echoed ' + TOKEN}, 'items': [TOKEN], 'note': TOKEN, TOKEN: 'key'}, 500))
        provider.replies[0]['headers'] = {'Date': 'provider echoed ' + TOKEN}
        reply = self.client(provider).request('POST', '/servers', server_body())
        self.assertNotIn(TOKEN, json.dumps(reply))
        self.assertEqual(reply['provider_date'], 'provider echoed <redacted-credential>')
        self.assertEqual(reply['status'], 500)
        self.assertEqual(provider.posts, 1)


    def test_private_request_data_and_returned_password_echoes_are_sanitized(self):
        body = server_body()
        password = 'synthetic-provider-generated-password'
        payload = {'server': {'id': 71}, 'root_password': password,
                   'note': 'echo ' + password + ' and ' + body['user_data'],
                   'nested': [{body['user_data']: [password, body['user_data']]}]}
        provider = MemoryProvider(MemoryProvider.json(payload, 201))
        reply = self.client(provider).request('POST', '/servers', body)
        for secret in (TOKEN, password, body['user_data']):
            self.assertNotIn(secret, json.dumps(reply))
        self.assertTrue(reply['payload']['root_password_redacted'])
        self.assertNotIn('root_password', reply['payload'])
        self.assertEqual(reply['body_sha256'], hashlib.sha256(json.dumps(payload).encode()).hexdigest())
        self.assertEqual(reply['request_sha256'], hashlib.sha256(cloud.wire_json(body)).hexdigest())
        self.assertEqual(provider.posts, 1)


class DefaultDeadlineWireTest(SyntheticFiles):
    """Cloud's default alarm must cover bytes after the paid request is sent."""

    def default_timer_request(self, expire=None):
        provider = MemoryProvider(MemoryProvider.json({'server': {'id': 71}}, 201))
        previous = object()
        handler, armed, stages = [previous], [False], []

        def install(signum, value):
            self.assertEqual(signum, cloud.signal.SIGALRM)
            handler[0] = value

        def timer(which, seconds):
            self.assertEqual(which, cloud.signal.ITIMER_REAL)
            if seconds:
                self.assertGreater(seconds, 0)
                self.assertLessEqual(seconds, 6)
                self.assertTrue(callable(handler[0]))
            armed[0] = bool(seconds)

        def observe(stage, paid=False):
            self.assertTrue(armed[0], stage + ' escaped Cloud default whole-call timer')
            self.assertTrue(callable(handler[0]))
            stages.append(stage)
            if stage == expire and paid:
                self.assertEqual(provider.posts, 1)
                handler[0](cloud.signal.SIGALRM, None)
                self.fail('default deadline handler did not interrupt the paid call')

        test = self
        class ResponseBody(io.BytesIO):
            def readline(self, *args):
                observe('response-header', paid=True)
                return super().readline(*args)

            def read(self, *args):
                observe('response-body', paid=True)
                test.assertEqual(provider.posts, 1)
                return super().read(*args)

        send, makefile = MemorySocket.sendall, MemorySocket.makefile
        def sending(sock, data):
            observe('sendall')
            send(sock, data)
            if sock.committed:
                observe('sent-mutation', paid=True)

        def response(sock, mode):
            observe('makefile', paid=True)
            stream = makefile(sock, mode)
            try:
                return ResponseBody(stream.getvalue())
            finally:
                stream.close()

        def connection(host, seconds):
            observe('connect')
            return provider.connection(host, seconds)

        # Intentionally omit deadline=: this pins Cloud's default binding, not
        # just whole_call_deadline tested as an isolated context manager.
        profile = self.profile()
        client = cloud.Cloud(profile, profile.binding, connection_factory=connection)
        with mock.patch.object(cloud.signal, 'getsignal', return_value=previous), \
                mock.patch.object(cloud.signal, 'getitimer', return_value=(0.0, 0.0)), \
                mock.patch.object(cloud.signal, 'signal', side_effect=install) as installed, \
                mock.patch.object(cloud.signal, 'setitimer', side_effect=timer) as timers, \
                mock.patch.object(MemorySocket, 'sendall', sending), \
                mock.patch.object(MemorySocket, 'makefile', response):
            if expire is None:
                reply = client.request('POST', '/servers', server_body(), seconds=6)
                self.assertEqual((reply['status'], reply['payload']), (201, {'server': {'id': 71}}))
            else:
                self.refusal(client.request, 'POST', '/servers', server_body(), seconds=6, message='unknown')
            self.assertEqual(timers.call_count, 2)
            self.assertEqual(timers.call_args, mock.call(cloud.signal.ITIMER_REAL, 0))
            self.assertEqual(installed.call_count, 2)
            self.assertEqual(installed.call_args, mock.call(cloud.signal.SIGALRM, previous))
        self.assertFalse(armed[0])
        self.assertIs(handler[0], previous)
        self.assertEqual((provider.posts, len(provider.requests), len(provider.connections)), (1, 1, 1))
        self.assertTrue(provider.sockets[0].closed)
        return stages

    def test_default_timer_stays_armed_through_send_headers_and_response_body(self):
        stages = self.default_timer_request()
        for stage in ('connect', 'sendall', 'sent-mutation', 'makefile', 'response-header', 'response-body'):
            self.assertIn(stage, stages)
        self.assertLess(stages.index('sent-mutation'), stages.index('makefile'))
        self.assertLess(stages.index('response-header'), stages.index('response-body'))

    def test_default_timer_expiry_after_dispatch_closes_without_retry(self):
        for stage in ('sent-mutation', 'makefile', 'response-header', 'response-body'):
            with self.subTest(expiry=stage):
                stages = self.default_timer_request(expire=stage)
                self.assertEqual(stages[-1], stage)
                if stage != 'response-body':
                    self.assertNotIn('response-body', stages)


class WholeCallDeadlineTest(unittest.TestCase):
    """Exercise alarm bookkeeping with every signal API mocked, never armed."""

    def test_invalid_allowance_refuses_before_touching_signals(self):
        with mock.patch.object(cloud.signal, 'getsignal') as getsignal:
            for allowance in (0, -1, 20.001, 21, float('inf'), float('nan')):
                with self.subTest(allowance=allowance):
                    with self.assertRaisesRegex(mc.GuardError, 'allowance'):
                        with cloud.whole_call_deadline(allowance):
                            self.fail('invalid deadline admitted')
            getsignal.assert_not_called()

    def test_existing_timer_refuses_without_rearming_or_changing_handler(self):
        with mock.patch.object(cloud.signal, 'getsignal', return_value='previous-handler'), \
                mock.patch.object(cloud.signal, 'getitimer', return_value=(1.0, 0.0)), \
                mock.patch.object(cloud.signal, 'signal') as install, \
                mock.patch.object(cloud.signal, 'setitimer') as timer:
            with self.assertRaisesRegex(mc.GuardError, 'nested'):
                with cloud.whole_call_deadline(7):
                    self.fail('nested deadline admitted')
            install.assert_not_called()
            timer.assert_not_called()

    def test_success_arms_whole_call_and_restores_original_handler(self):
        previous = object()
        with mock.patch.object(cloud.signal, 'getsignal', return_value=previous), \
                mock.patch.object(cloud.signal, 'getitimer', return_value=(0.0, 0.0)), \
                mock.patch.object(cloud.signal, 'signal') as install, \
                mock.patch.object(cloud.signal, 'setitimer') as timer:
            with cloud.whole_call_deadline(7):
                self.assertEqual(timer.call_args_list, [mock.call(cloud.signal.ITIMER_REAL, 7)])
                self.assertEqual(install.call_count, 1)
                self.assertEqual(install.call_args.args[0], cloud.signal.SIGALRM)
                self.assertTrue(callable(install.call_args.args[1]))
            self.assertEqual(timer.call_args_list,
                             [mock.call(cloud.signal.ITIMER_REAL, 7), mock.call(cloud.signal.ITIMER_REAL, 0)])
            self.assertEqual(install.call_args, mock.call(cloud.signal.SIGALRM, previous))

    def test_expiry_raises_timeout_and_still_disarms_and_restores(self):
        previous = object()
        with mock.patch.object(cloud.signal, 'getsignal', return_value=previous), \
                mock.patch.object(cloud.signal, 'getitimer', return_value=(0.0, 0.0)), \
                mock.patch.object(cloud.signal, 'signal') as install, \
                mock.patch.object(cloud.signal, 'setitimer') as timer:
            with self.assertRaisesRegex(TimeoutError, 'whole-call deadline'):
                with cloud.whole_call_deadline(3):
                    installed_handler = install.call_args.args[1]
                    installed_handler(cloud.signal.SIGALRM, None)
            self.assertEqual(timer.call_args_list,
                             [mock.call(cloud.signal.ITIMER_REAL, 3), mock.call(cloud.signal.ITIMER_REAL, 0)])
            self.assertEqual(install.call_args, mock.call(cloud.signal.SIGALRM, previous))


class MutationAllowlistTest(unittest.TestCase):
    def test_complete_campaign_operation_population(self):
        operations = [
            ('POST', '/servers', server_body()),
            ('POST', '/networks', {'name': 'synthetic-net', 'ip_range': '10.0.0.0/16', 'labels': {}}),
            ('POST', '/firewalls', {'name': 'synthetic-fw', 'rules': [], 'labels': {}}),
            ('POST', '/ssh_keys', {'name': 'synthetic-key', 'public_key': 'synthetic-public-key', 'labels': {}}),
            ('POST', '/networks/21/actions/add_subnet', {'type': 'cloud', 'network_zone': 'eu-central',
                                                        'ip_range': '10.0.0.0/24'}),
            ('POST', '/servers/71/actions/attach_to_network', {'network': 21, 'ip': '10.0.0.2'}),
            ('POST', '/servers/71/actions/poweron', {}),
            *[('DELETE', f'/{plural}/71', None) for plural in ('servers', 'networks', 'firewalls', 'ssh_keys')],
        ]
        self.assertEqual(len(operations), 11)
        for method, path, body in operations:
            with self.subTest(method=method, path=path):
                self.assertTrue(cloud.mutation_allowed(method, path, body))

    def test_server_pinned_material_fields_and_ip_flags_cannot_change(self):
        changes = {'server_type': ['ccx33', None], 'location': ['nbg1', None], 'image': [True, '161547269', 1, 161547269.0],
                   'start_after_create': [True, 0, None],
                   'public_net': [{'enable_ipv4': False, 'enable_ipv6': False},
                                  {'enable_ipv4': True, 'enable_ipv6': True},
                                  {'enable_ipv4': 1, 'enable_ipv6': 0},
                                  {'enable_ipv4': True, 'enable_ipv6': False, 'ipv4': 99}, {}]}
        for key, values in changes.items():
            for value in values:
                with self.subTest(key=key, value=value):
                    body = server_body()
                    body[key] = value
                    self.assertFalse(cloud.mutation_allowed('POST', '/servers', body))

    def test_create_extra_missing_and_nonobject_bodies_are_refused(self):
        bodies = {'/servers': server_body(), '/networks': {'name': 'net', 'ip_range': '10.0.0.0/16', 'labels': {}},
                  '/firewalls': {'name': 'fw', 'rules': [], 'labels': {}},
                  '/ssh_keys': {'name': 'key', 'public_key': 'synthetic-key', 'labels': {}}}
        for path, good in bodies.items():
            for key in good:
                with self.subTest(path=path, missing=key):
                    body = copy.deepcopy(good)
                    del body[key]
                    self.assertFalse(cloud.mutation_allowed('POST', path, body))
            for body in ({**good, 'unapproved': True}, None, [], 'wire body', True):
                with self.subTest(path=path, body=body):
                    self.assertFalse(cloud.mutation_allowed('POST', path, body))

    def test_nonallocating_action_body_mutations_are_refused(self):
        attempts = [
            ('/networks/21/actions/add_subnet', {'type': 'server', 'network_zone': 'eu-central', 'ip_range': '10/8'}),
            ('/networks/21/actions/add_subnet', {'type': 'cloud', 'network_zone': 'eu-central'}),
            ('/networks/21/actions/add_subnet', {'type': 'cloud', 'network_zone': 'eu-central',
                                               'ip_range': '10/8', 'extra': True}),
            *[('/servers/71/actions/attach_to_network', {'network': value, 'ip': '10.0.0.2'})
              for value in (True, False, 0, -1, '21', None)],
            ('/servers/71/actions/attach_to_network', {'network': 21}),
            ('/servers/71/actions/attach_to_network', {'network': 21, 'ip': '10.0.0.2', 'alias_ips': []}),
            ('/servers/71/actions/poweron', {'force': True}),
            ('/servers/0/actions/poweron', {}), ('/servers/01/actions/poweron', {}),
        ]
        for path, body in attempts:
            with self.subTest(path=path, body=body):
                self.assertFalse(cloud.mutation_allowed('POST', path, body))


class DiscoveryTest(SyntheticFiles):
    def client(self, *payloads):
        self.provider = MemoryProvider(*[MemoryProvider.json(value) for value in payloads])
        profile = self.profile()
        return cloud.Cloud(profile, profile.binding, connection_factory=self.provider.connection,
                           deadline=contextlib.nullcontext)

    def test_complete_paginated_census_returns_exact_ids_and_requests(self):
        rows = [{'id': 1, 'name': 'first'}, {'id': 7, 'name': 'second'}, {'id': 42, 'name': 'third'}]
        client = self.client(page(rows[:2], total=3, next_page=2), page(rows[2:], total=3))
        self.assertEqual(client.list('server', seconds=4), rows)
        self.assertEqual([request['path'] for request in self.provider.requests],
                         ['/v1/servers?page=1&per_page=50', '/v1/servers?page=2&per_page=50'])
        self.assertTrue(all(request['method'] == 'GET' for request in self.provider.requests))
        self.assertEqual(self.provider.posts, 0)

    def test_wire_provider_enforces_literal_page_size_limit_for_inventory_and_actions(self):
        for route in ('/servers', '/servers/71/actions'):
            for size in (50, 51, 100):
                with self.subTest(route=route, per_page=size):
                    payload = page([]) if route == '/servers' else action_page([])
                    client = self.client(payload)
                    path = route + '?page=1&per_page=' + str(size)
                    if size == 50:
                        self.assertEqual(client.request('GET', path)['status'], 200)
                        self.assertEqual([r['path'] for r in self.provider.requests], ['/v1' + path])
                    else:
                        with self.assertRaisesRegex(AssertionError, 'fake provider per_page exceeds documented maximum 50'):
                            client.request('GET', path)
                        self.assertEqual(self.provider.requests, [])
                    self.assertEqual(len(self.provider.connections), 1)
                    self.assertEqual(len(self.provider.sockets), 1)
                    self.assertIn(('GET /v1' + path + ' HTTP/1.1\r\n').encode(),
                                  bytes(self.provider.sockets[0].wire))
                    self.assertTrue(self.provider.sockets[0].closed)
                    self.assertEqual(self.provider.posts, 0)

    def test_empty_complete_census_is_valid(self):
        self.assertEqual(self.client(page([])).list('server'), [])
        self.assertEqual(len(self.provider.requests), 1)

    def test_all_ten_kinds_have_exact_list_and_describe_paths(self):
        expected = {'server': ('servers', 'server'), 'network': ('networks', 'network'),
                    'firewall': ('firewalls', 'firewall'), 'ssh-key': ('ssh_keys', 'ssh_key'),
                    'primary-ip': ('primary_ips', 'primary_ip'), 'floating-ip': ('floating_ips', 'floating_ip'),
                    'volume': ('volumes', 'volume'), 'load-balancer': ('load_balancers', 'load_balancer'),
                    'placement-group': ('placement_groups', 'placement_group'),
                    'certificate': ('certificates', 'certificate')}
        self.assertEqual(set(cloud.PLURALS), set(expected))
        for kind, (plural, singular) in expected.items():
            with self.subTest(kind=kind):
                item = {'id': 71, 'name': 'synthetic'}
                payload = {plural: [item], 'meta': {'pagination': {'total_entries': 1, 'next_page': None}}}
                client = self.client(payload, {singular: item})
                self.assertEqual(client.list(kind), [item])
                self.assertEqual(client.describe(kind, 71), item)
                self.assertEqual([request['path'] for request in self.provider.requests],
                                 [f'/v1/{plural}?page=1&per_page=50', f'/v1/{plural}/71'])

    def test_census_rejects_missing_duplicate_noninteger_or_nonpositive_ids(self):
        for items in ([{'id': 1}, {'id': 1}], [{'id': True}], [{'id': '1'}], [{'id': 0}],
                      [{'id': -1}], [{}], [None], [1], [{'id': 1.0}]):
            with self.subTest(items=items):
                self.refusal(self.client(page(items)).list, 'server', message='IDs')
                self.assertEqual(len(self.provider.requests), 1)

    def test_census_rejects_invalid_or_inconsistent_totals(self):
        for total in (True, '1', -1, 1.0, None, 0, 2):
            with self.subTest(total=total):
                payload = page([{'id': 1}])
                payload['meta']['pagination']['total_entries'] = total
                self.refusal(self.client(payload).list, 'server')
        client = self.client(page([{'id': 1}], total=2, next_page=2), page([{'id': 2}], total=3))
        self.refusal(client.list, 'server', message='changing')
        self.assertEqual(len(self.provider.requests), 2)

    def test_census_rejects_bad_next_page_before_additional_request(self):
        for next_page in (True, False, 0, -1, '2', 2.0, 10001, 1):
            with self.subTest(next_page=next_page):
                client = self.client(page([{'id': 1}], total=2, next_page=next_page))
                self.refusal(client.list, 'server', message='pagination')
                self.assertEqual(len(self.provider.requests), 1)

    def test_census_rejects_cycle_after_valid_second_page(self):
        client = self.client(page([{'id': 1}], total=3, next_page=2),
                             page([{'id': 2}], total=3, next_page=1))
        self.refusal(client.list, 'server', message='pagination')
        self.assertEqual(len(self.provider.requests), 2)

    def test_census_rejects_incomplete_or_malformed_metadata(self):
        for payload in (None, [], {}, {'servers': []}, {'servers': {}, 'meta': {'pagination': {}}},
                        {'servers': [], 'meta': []}, {'servers': [], 'meta': ['bad']},
                        {'servers': [], 'meta': 'bad'}, {'servers': [], 'meta': {'pagination': []}},
                        {'servers': [], 'meta': {'pagination': {'next_page': None}}},
                        {'servers': [], 'meta': {'pagination': {'total_entries': 0}}}):
            with self.subTest(payload=payload):
                self.refusal(self.client(payload).list, 'server')
                self.assertEqual(len(self.provider.requests), 1)

    def test_census_http_failure_is_not_an_empty_success(self):
        client = self.client(page([]))
        self.provider.replies[0]['status'] = 503
        self.refusal(client.list, 'server', message='unavailable')
        self.assertEqual(len(self.provider.requests), 1)

    def test_describe_exact_id_and_absence(self):
        client = self.client({'server': {'id': 71, 'name': 'synthetic'}})
        self.assertEqual(client.describe('server', 71), {'id': 71, 'name': 'synthetic'})
        self.assertEqual(self.provider.requests[0]['path'], '/v1/servers/71')
        client = self.client({'error': {'code': 'not_found'}})
        self.provider.replies[0]['status'] = 404
        self.assertIsNone(client.describe('server', 71))
        self.assertEqual(len(self.provider.requests), 1)

    def test_describe_rejects_bad_requested_id_before_any_call(self):
        for rid in (True, False, 0, -1, '71', 71.0, None):
            with self.subTest(rid=rid):
                client = self.client({})
                self.refusal(client.describe, 'server', rid, message='exact resource ID')
                self.assertEqual(self.provider.requests, [])

    def test_describe_rejects_wrong_or_malformed_returned_identity(self):
        for payload in ({'server': {'id': 72}}, {'server': {'id': True}}, {'server': {'id': '71'}},
                        {'server': {'id': 71.0}}, {'server': {}}, {'server': []}, {'network': {'id': 71}}, None):
            with self.subTest(payload=payload):
                self.refusal(self.client(payload).describe, 'server', 71)
                self.assertEqual(len(self.provider.requests), 1)

    def test_describe_requested_id_one_accepts_integer_but_rejects_boolean_returned_identity(self):
        for returned, accepted in ((1, True), (True, False)):
            with self.subTest(returned_type=type(returned).__name__):
                self.assertEqual(returned, 1, 'equality alone must not distinguish these response IDs')
                self.assertIs(type(returned), int if accepted else bool)
                payload = {'server': {'id': returned, 'name': 'synthetic'}}
                client = self.client(payload)
                if accepted:
                    item = client.describe('server', 1)
                    self.assertEqual(item, {'id': 1, 'name': 'synthetic'})
                    self.assertIs(type(item['id']), int)
                else:
                    self.refusal(client.describe, 'server', 1, message='exact resource ID differs')
                self.assertEqual(len(self.provider.requests), 1)
                self.assertEqual(self.provider.requests[0]['method'], 'GET')
                self.assertEqual(self.provider.requests[0]['path'], '/v1/servers/1')
                self.assertEqual(self.provider.requests[0]['body'], b'')
                self.assertEqual(self.provider.posts, 0)
                self.assertEqual(len(self.provider.connections), 1)
                self.assertEqual(json.loads(self.provider.sockets[0].reply['raw']), payload)
                self.assertTrue(self.provider.sockets[0].closed)

    def test_describe_non404_failure_is_not_absence(self):
        for status in (201, 403, 429, 500):
            with self.subTest(status=status):
                client = self.client({'server': {'id': 71}})
                self.provider.replies[0]['status'] = status
                self.refusal(client.describe, 'server', 71, message='unavailable')

    def test_resource_actions_complete_pages_for_exact_server_and_firewall(self):
        for kind, plural, command in (('server', 'servers', 'create_server'),
                                      ('firewall', 'firewalls', 'create_firewall')):
            with self.subTest(kind=kind):
                actions = [{'id': 81, 'command': command, 'status': 'running',
                            'resources': [{'id': 71, 'type': kind}]},
                           {'id': 82, 'command': 'synthetic_later_action', 'status': 'success',
                            'resources': [{'id': 71, 'type': kind}]}]
                client = self.client(action_page(actions[:1], total=2, next_page=2),
                                     action_page(actions[1:], total=2))
                self.assertEqual(client.resource_actions(kind, 71, seconds=7), actions)
                self.assertEqual([request['path'] for request in self.provider.requests],
                                 [f'/v1/{plural}/71/actions?page=1&per_page=50',
                                  f'/v1/{plural}/71/actions?page=2&per_page=50'])
                self.assertTrue(all(request['method'] == 'GET' for request in self.provider.requests))
                self.assertTrue(all(0 < connection.timeout <= 7 for connection in self.provider.connections))
                self.assertTrue(all(sock.closed for sock in self.provider.sockets))
                self.assertEqual(self.provider.posts, 0)

    def test_resource_actions_empty_complete_census_is_only_an_empty_result(self):
        for kind in ('server', 'firewall'):
            with self.subTest(kind=kind):
                client = self.client(action_page([]))
                self.assertEqual(client.resource_actions(kind, 71), [])
                self.assertEqual(len(self.provider.requests), 1)
                self.assertEqual(self.provider.posts, 0)

    def test_resource_actions_rejects_unsupported_or_malformed_kind_before_call(self):
        for kind in ('network', 'ssh-key', 'primary-ip', 'floating-ip', 'volume', 'load-balancer',
                     'placement-group', 'certificate', 'actions', '', 'servers/71', None, True, 1, [], {}):
            with self.subTest(kind=kind):
                client = self.client(action_page([]))
                self.refusal(client.resource_actions, kind, 71, message='exact owned server/firewall ID')
                self.assertEqual(self.provider.requests, [])
                self.assertEqual(self.provider.connections, [])

    def test_resource_actions_rejects_bad_exact_resource_id_before_call(self):
        for kind in ('server', 'firewall'):
            for rid in (True, False, 0, -1, '71', 71.0, None, [], {}):
                with self.subTest(kind=kind, rid=rid):
                    client = self.client(action_page([]))
                    self.refusal(client.resource_actions, kind, rid, message='exact owned server/firewall ID')
                    self.assertEqual(self.provider.requests, [])
                    self.assertEqual(self.provider.connections, [])

    def test_resource_actions_rejects_bad_next_page_before_followup(self):
        for next_page in (True, False, 0, -1, '2', 2.0, 10001, 1, [], {}):
            with self.subTest(next_page=next_page):
                client = self.client(action_page([{'id': 81}], total=2, next_page=next_page))
                self.refusal(client.resource_actions, 'server', 71, message='pagination')
                self.assertEqual(len(self.provider.requests), 1)
        client = self.client(action_page([{'id': 81}], total=3, next_page=2),
                             action_page([{'id': 82}], total=3, next_page=1))
        self.refusal(client.resource_actions, 'server', 71, message='pagination')
        self.assertEqual(len(self.provider.requests), 2)

    def test_resource_actions_rejects_invalid_or_inconsistent_totals(self):
        for total in (True, '1', -1, 1.0, None, 0, 2):
            with self.subTest(total=total):
                payload = action_page([{'id': 81}])
                payload['meta']['pagination']['total_entries'] = total
                self.refusal(self.client(payload).resource_actions, 'server', 71)
                self.assertEqual(len(self.provider.requests), 1)
        client = self.client(action_page([{'id': 81}], total=2, next_page=2),
                             action_page([{'id': 82}], total=3))
        self.refusal(client.resource_actions, 'server', 71, message='changing')
        self.assertEqual(len(self.provider.requests), 2)

    def test_resource_actions_rejects_missing_duplicate_or_wrongly_typed_ids(self):
        for items in ([{'id': 81}, {'id': 81}], [{'id': True}], [{'id': '81'}], [{'id': 0}],
                      [{'id': -1}], [{'id': 81.0}], [{}], [None], [81]):
            with self.subTest(items=items):
                self.refusal(self.client(action_page(items)).resource_actions, 'server', 71, message='IDs')
                self.assertEqual(len(self.provider.requests), 1)
        client = self.client(action_page([{'id': 81}], total=2, next_page=2),
                             action_page([{'id': 81}], total=2))
        self.refusal(client.resource_actions, 'server', 71, message='IDs')
        self.assertEqual(len(self.provider.requests), 2)

    def test_resource_actions_rejects_missing_or_malformed_metadata(self):
        for payload in (None, [], {}, {'actions': []}, {'actions': {}, 'meta': {'pagination': {}}},
                        {'actions': [], 'meta': []}, {'actions': [], 'meta': ['bad']},
                        {'actions': [], 'meta': 'bad'}, {'actions': [], 'meta': {'pagination': []}},
                        {'actions': [], 'meta': {'pagination': {'next_page': None}}},
                        {'actions': [], 'meta': {'pagination': {'total_entries': 0}}}):
            with self.subTest(payload=payload):
                self.refusal(self.client(payload).resource_actions, 'server', 71)
                self.assertEqual(len(self.provider.requests), 1)

    def test_resource_actions_http_or_malformed_json_failure_is_not_empty_success(self):
        for status in (201, 403, 404, 429, 500, 503):
            with self.subTest(status=status):
                client = self.client(action_page([]))
                self.provider.replies[0]['status'] = status
                self.refusal(client.resource_actions, 'server', 71, message='unavailable')
                self.assertEqual(len(self.provider.requests), 1)
        client = self.client(action_page([]))
        self.provider.replies[0]['raw'] = b'{"actions":'
        self.refusal(client.resource_actions, 'server', 71, message='unavailable')
        self.assertEqual(len(self.provider.requests), 1)

    def test_resource_actions_failed_second_page_does_not_return_partial_population(self):
        client = self.client(action_page([{'id': 81}], total=2, next_page=2), action_page([]))
        self.provider.replies[1]['status'] = 503
        self.refusal(client.resource_actions, 'server', 71, message='unavailable')
        self.assertEqual(len(self.provider.requests), 2)
        self.assertEqual(self.provider.posts, 0)

    def test_known_action_status_population(self):
        for status in ('running', 'success', 'error'):
            with self.subTest(status=status):
                action = {'id': 81, 'status': status, 'resources': [{'id': 71, 'type': 'server'}]}
                self.assertEqual(self.client({'action': action}).action(81), action)
                self.assertEqual(self.provider.requests[0]['path'], '/v1/actions/81')

    def test_known_action_rejects_bad_requested_id_before_call(self):
        for rid in (True, False, 0, -1, '81', 81.0, None):
            with self.subTest(rid=rid):
                client = self.client({})
                self.refusal(client.action, rid, message='known action ID')
                self.assertEqual(self.provider.requests, [])

    def test_known_action_rejects_wrong_id_type_and_unknown_or_malformed_status(self):
        for action in ({'id': 82, 'status': 'success'}, {'id': '81', 'status': 'success'},
                       {'id': 81.0, 'status': 'success'}, {'id': 81, 'status': 'pending'},
                       {'id': 81, 'status': []}, {'id': 81, 'status': {}}, {'id': 81}, {}):
            with self.subTest(action=action):
                self.refusal(self.client({'action': action}).action, 81, message='invalid provider action')
        self.refusal(self.client({'action': {'id': True, 'status': 'success'}}).action, 1,
                     message='invalid provider action')

    def test_known_action_requires_200_and_structured_action(self):
        for payload in (None, [], {}, {'action': []}, {'action': None}):
            with self.subTest(payload=payload):
                self.refusal(self.client(payload).action, 81, message='unavailable')
        for status in (201, 404, 403, 500):
            with self.subTest(status=status):
                client = self.client({'action': {'id': 81, 'status': 'success'}})
                self.provider.replies[0]['status'] = status
                self.refusal(client.action, 81, message='unavailable')


if __name__ == '__main__':
    unittest.main()
