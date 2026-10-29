"""Private, authenticated creation journal; cloud authorization belongs to callers."""
import codecs
import contextlib
import copy
import decimal
import errno
import fcntl
import functools
import hashlib
import hmac
import json
import math
import os
from pathlib import Path
import re
import stat
import threading
import uuid

import mn_common as mc


ANCHOR = 'anchor.json'
KEY = 'integrity.key'
LOCK = 'journal.lock'
RECOVERY = '.recovery-in-progress'
ANCHOR_FORMAT = 'fdb-creation-ledger-anchor/v1'
GENERATION_FORMAT = 'fdb-creation-ledger-generation/v1'
REQUIRED_HEADER = {'campaign', 'prefix', 'boot_id', 'sources', 'baseline', 'time', 'producer',
                   'context', 'plan', 'threat_model', 'graph_reserved'}
MODES = {'OPEN', 'CLOSING', 'COMPLETE', 'INCIDENT'}
GENERATION = re.compile(r'generation-([0-9]{20})\.json')


class ObservationDeferred(mc.GuardError):
    """Busy publication prevented a fresh observation; retain watchdog state."""


class _FileIdentityChanged(mc.GuardError):
    def __init__(self, name):
        super().__init__('journal file identity changed during open')
        self.name = name


def need(ok, reason):
    if not ok:
        raise mc.GuardError(reason)


@functools.lru_cache(maxsize=4096)
def _credential_field(key):
    name = key.lower().replace('-', '_')
    sensitive = re.search(r'(^|_)(token|password|secret|authorization|credentials?)(_|$)', name)
    sensitive = sensitive or 'private_key' in name or name in {'api_key', 'user_data'}
    if not sensitive:
        return None
    if name.endswith(('_sha256', '_hash')):
        return 'digest'
    if name.endswith(('_present', '_redacted')):
        return 'redaction'
    return 'forbidden'


def _json_value(value, credentials=False):
    if type(value) is dict:
        need(all(type(key) is str for key in value), 'journal JSON keys must be strings')
        for key, item in value.items():
            field = _credential_field(key) if credentials else None
            if field is not None:
                digest = field == 'digest' and type(item) is str and re.fullmatch(r'[0-9a-f]{64}', item)
                redaction = field == 'redaction' and type(item) is bool
                need(digest or redaction, 'credential fields are forbidden in journal')
            _json_value(item, credentials)
    elif type(value) is list:
        for item in value:
            _json_value(item, credentials)
    else:
        need(value is None or type(value) in (str, bool, int, float), 'unsupported journal JSON value')
        need(type(value) is not float or math.isfinite(value), 'nonfinite journal JSON value')


def canonical_json(value):
    """Canonical UTF-8 bytes; rejects non-JSON types and nonfinite numbers."""
    _json_value(value)
    try:
        return json.dumps(value, sort_keys=True, separators=(',', ':'), ensure_ascii=False,
                          allow_nan=False).encode('utf-8')
    except UnicodeError as error:
        raise mc.GuardError('invalid journal Unicode') from error


def sha256(value):
    """Hash bytes directly, or canonical JSON for a JSON value."""
    return hashlib.sha256(value if type(value) is bytes else canonical_json(value)).hexdigest()


def _header(header):
    need(type(header) is dict and REQUIRED_HEADER <= set(header), 'incomplete journal header')
    need(all(type(header[name]) is str and header[name] for name in ('campaign', 'prefix', 'boot_id')),
         'invalid journal header identity')
    _json_value(header, credentials=True)


def _state(state):
    need(type(state) is dict and type(state.get('mode')) is str and state['mode'] in MODES,
         'invalid journal state mode')
    need(all(type(state.get(name)) is list for name in ('intents', 'owned', 'associations', 'events')),
         'invalid journal state lists')
    need(all(state.get(name) is None or type(state.get(name)) is dict for name in ('guard', 'active_lease'))
         and {'guard', 'active_lease'} <= set(state), 'invalid journal state bindings')
    _json_value(state, credentials=True)


def _identity(info):
    return {'device': info.st_dev, 'inode': info.st_ino}


def _private(info, directory=False, *, unlinked=False):
    need(info.st_uid == os.getuid(), 'journal ownership differs')
    need(stat.S_IMODE(info.st_mode) == (0o700 if directory else 0o600), 'unsafe journal permissions')
    need(stat.S_ISDIR(info.st_mode) if directory else stat.S_ISREG(info.st_mode), 'unsafe journal file type')
    if not directory:
        need(info.st_nlink == 1 or unlinked and info.st_nlink == 0, 'journal file has multiple links')


def _names(directory):
    # A fresh open description avoids retaining a directory enumeration cursor.
    fd = os.open('.', os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=directory)
    try:
        _private(os.fstat(fd), directory=True)
        need(_identity(os.fstat(fd)) == _identity(os.fstat(directory)), 'journal enumeration directory changed')
        return set(os.listdir(fd))
    finally:
        os.close(fd)


def _path(path):
    path = Path(os.path.abspath(os.fspath(path)))
    need(re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,127}', path.name) is not None
         and path.name not in (ANCHOR, KEY, LOCK) and not GENERATION.fullmatch(path.name)
         and not path.name.startswith('.head-'), 'invalid journal head name')
    for parent in (path.parent, *path.parent.parents):
        if parent.exists() or parent.is_symlink():
            need(not stat.S_ISLNK(os.lstat(parent).st_mode), 'symlink in journal directory path')
    return path


def _seal(body, key):
    return dict(body, mac=hmac.new(key, canonical_json(body), hashlib.sha256).hexdigest())


def _authenticated(record, key):
    need(type(record) is dict and type(record.get('mac')) is str
         and re.fullmatch(r'[0-9a-f]{64}', record['mac']), 'missing journal MAC')
    body = {name: value for name, value in record.items() if name != 'mac'}
    expected = hmac.new(key, canonical_json(body), hashlib.sha256).hexdigest()
    need(hmac.compare_digest(record['mac'], expected), 'journal MAC differs')
    return body


def _generation(number):
    return f'generation-{number:020d}.json'


def _number_prefix(token):
    """Accept only with a concrete number whose canonical spelling extends token."""
    def witness(number):
        return (type(number) is int or math.isfinite(number)) and canonical_json(number).decode().startswith(token)

    if token == '-':
        return witness(-1)
    if token == '-0':
        return witness(-0.0)
    try:
        if witness(json.loads(token)):
            return True
    except (ValueError, mc.GuardError):
        pass
    if 'e' in token:
        mantissa, exponent_prefix = token.split('e', 1)
        if not re.fullmatch(r'-?[1-9](\.[0-9]*[1-9])?', mantissa):
            return False
        for exponent in range(-324, 309):
            suffix = f'{exponent:+03d}'
            if suffix.startswith(exponent_prefix) and witness(float(mantissa + 'e' + suffix)):
                return True
        return False
    if not re.fullmatch(r'-?(0|[1-9][0-9]*)\.[0-9]*', token):
        return False
    unsigned = token.lstrip('-')
    whole, fraction = unsigned.split('.')
    # A decimal prefix defines an exact interval. Neighbouring binary64 values
    # cover its rounding edges; acceptance still requires an actual serializer witness.
    with decimal.localcontext() as context:
        context.prec = len(token) + 400
        lower = decimal.Decimal(unsigned + ('0' if not fraction else ''))
        upper = lower + decimal.Decimal(1).scaleb(-len(fraction))
        scales = range(-324, 309) if len(whole) == 1 and whole != '0' else (0,)
        for exponent in scales:
            left, right = lower.scaleb(exponent), upper.scaleb(exponent)
            for exact in (left, right, (left + right) / 2):
                number = float(exact)
                if not math.isfinite(number):
                    continue
                for candidate in (number, math.nextafter(number, -math.inf), math.nextafter(number, math.inf)):
                    if witness(-candidate if token.startswith('-') else candidate):
                        return True
    return False


class _JSONPrefixEnd(Exception):
    def __init__(self, string=False):
        self.string = string


def _state_prefix(text, pending_utf8):
    """Validate canonical tokens and completed state fields before accepting EOF."""
    incomplete_utf8 = bool(pending_utf8)
    position = 0
    defaults = dict(mode='OPEN', intents=[], owned=[], associations=[], events=[], guard=None, active_lease=None)
    control_escapes = {f'\\u{number:04x}' for number in range(32) if number not in (8, 9, 10, 12, 13)}

    def expect(character):
        nonlocal position
        if position == len(text):
            raise _JSONPrefixEnd()
        if text[position] != character:
            raise ValueError('noncanonical JSON prefix')
        position += 1

    def possible_key(prefix, bounds, endings=None):
        if bounds is None:
            return True
        lower, upper = bounds
        def possible(value):
            return ((lower is None or value >= lower[:len(value)])
                    and (upper is None or value <= upper[:len(value)]))
        if endings is not None:
            return any(possible(prefix + ending) for ending in endings)
        if not pending_utf8:
            return possible(prefix)
        width = 2 if pending_utf8[0] < 0xe0 else 3 if pending_utf8[0] < 0xf0 else 4
        low = bytearray(pending_utf8 + b'\x80' * (width - len(pending_utf8)))
        high = bytearray(pending_utf8 + b'\xbf' * (width - len(pending_utf8)))
        if len(pending_utf8) == 1:
            if low[0] == 0xe0: low[1] = 0xa0
            if high[0] == 0xed: high[1] = 0x9f
            if low[0] == 0xf0: low[1] = 0x90
            if high[0] == 0xf4: high[1] = 0x8f
        first, last = ord(low.decode('utf-8')), ord(high.decode('utf-8'))
        choices = {first, last}
        for bound in (lower, upper):
            if bound is not None and bound.startswith(prefix) and len(bound) > len(prefix):
                choices.add(ord(bound[len(prefix)]))
        return any(first <= number <= last and possible(prefix + chr(number)) for number in choices)

    def string(constraint=None, key_bounds=None):
        nonlocal position
        start = position
        expect('"')
        while True:
            if position == len(text):
                prefix = json.loads(text[start:] + '"')
                need(possible_key(prefix, key_bounds), 'impossible journal object key prefix')
                if constraint in ('mode', 'digest'):
                    need(not incomplete_utf8, 'invalid constrained journal UTF-8 prefix')
                if constraint == 'mode':
                    need(any(mode.startswith(prefix) for mode in MODES), 'invalid journal state mode prefix')
                elif constraint == 'digest':
                    need(len(prefix) <= 64 and re.fullmatch(r'[0-9a-f]*', prefix) is not None,
                         'credential fields are forbidden in journal')
                raise _JSONPrefixEnd(string=True)
            character = text[position]
            if character == '"':
                position += 1
                token = text[start:position]
                result = json.loads(token)
                if canonical_json(result).decode() != token:
                    raise ValueError('noncanonical string')
                return result
            if ord(character) < 32:
                raise ValueError('unescaped control')
            if character != '\\':
                position += 1
                continue
            need(constraint not in ('mode', 'digest'), 'invalid constrained journal string escape')
            escape = position
            position += 1
            if position == len(text):
                prefix = json.loads(text[start:escape] + '"')
                endings = [chr(number) for number in range(32)] + ['"', '\\']
                need(possible_key(prefix, key_bounds, endings), 'impossible journal object key prefix')
                raise _JSONPrefixEnd()
            if text[position] == 'u':
                token = text[escape:escape + 6]
                remaining = [value for value in control_escapes if value.startswith(token)]
                if len(token) < 6 and remaining:
                    prefix = json.loads(text[start:escape] + '"')
                    endings = [json.loads('"' + value + '"') for value in remaining]
                    need(possible_key(prefix, key_bounds, endings), 'impossible journal object key prefix')
                    raise _JSONPrefixEnd()
                if token not in control_escapes:
                    raise ValueError('noncanonical Unicode escape')
                position = escape + 6
            elif text[position] in '"\\bfnrt':
                position += 1
            else:
                raise ValueError('invalid string escape')

    def value(root=False, constraint=None):
        nonlocal position
        if position == len(text):
            raise _JSONPrefixEnd()
        character = text[position]
        if constraint in ('mode', 'digest'):
            need(character == '"', 'invalid constrained journal string type')
        elif constraint == 'redaction':
            need(character in 'tf', 'credential fields are forbidden in journal')
        if character == '"':
            return string(constraint)
        if character == '{':
            position += 1
            result, previous = {}, None
            if position == len(text):
                raise _JSONPrefixEnd()
            if text[position] == '}':
                position += 1
                return result
            while True:
                required = [name for name in defaults if name not in result] if root else []
                key = string(key_bounds=(previous, min(required) if required else None))
                if previous is not None and key <= previous:
                    raise ValueError('duplicate or unordered JSON key')
                if root and any(name < key and name not in result for name in defaults):
                    raise ValueError('missing earlier state field')
                previous = key
                constraint = _credential_field(key)
                need(constraint != 'forbidden', 'credential fields are forbidden in journal')
                if root and key == 'mode':
                    constraint = 'mode'
                expect(':')
                if root and position < len(text):
                    if key in ('intents', 'owned', 'associations', 'events'):
                        need(text[position] == '[', 'invalid journal state lists')
                    elif key == 'mode':
                        need(text[position] == '"', 'invalid journal state mode')
                    elif key in ('guard', 'active_lease'):
                        need(text[position] in '{n', 'invalid journal state bindings')
                result[key] = value(constraint=constraint)
                _json_value({key: result[key]}, credentials=True)
                if root:
                    completed = dict(defaults, **result)
                    _state(completed)
                if position == len(text):
                    raise _JSONPrefixEnd()
                if text[position] == '}':
                    position += 1
                    return result
                expect(',')
        if character == '[':
            position += 1
            result = []
            if position == len(text):
                raise _JSONPrefixEnd()
            if text[position] == ']':
                position += 1
                return result
            while True:
                result.append(value())
                if position == len(text):
                    raise _JSONPrefixEnd()
                if text[position] == ']':
                    position += 1
                    return result
                expect(',')
        for token, result in (('true', True), ('false', False), ('null', None)):
            if character == token[0]:
                rest = text[position:]
                if len(rest) < len(token) and token.startswith(rest):
                    raise _JSONPrefixEnd()
                if not rest.startswith(token):
                    raise ValueError('invalid literal')
                position += len(token)
                return result
        start = position
        while position < len(text) and text[position] in '-+0123456789.eE':
            position += 1
        token = text[start:position]
        if position == len(text):
            if _number_prefix(token):
                raise _JSONPrefixEnd()
            raise ValueError('invalid number prefix')
        result = json.loads(token)
        if canonical_json(result).decode() != token:
            raise ValueError('noncanonical number')
        return result

    try:
        result = value(root=True)
    except _JSONPrefixEnd as end:
        return (not incomplete_utf8 or end.string), None, None
    except (ValueError, RecursionError):
        return False, None, None
    return not incomplete_utf8, result, position


def _partial_generation(raw, number, header_hash, previous, journal_id):
    """Recognize only truncated prefixes of this writer's canonical generation."""
    prefix = (b'{"format":' + canonical_json(GENERATION_FORMAT) + b',"generation":' + str(number).encode()
              + b',"header_sha256":' + canonical_json(header_hash) + b',"mac":"')
    if len(raw) <= len(prefix):
        return prefix.startswith(raw)
    if not raw.startswith(prefix):
        return False
    rest = raw[len(prefix):]
    if len(rest) <= 64:
        return all(byte in b'0123456789abcdef' for byte in rest)
    if not re.fullmatch(b'[0-9a-f]{64}', rest[:64]):
        return False
    middle = b'","previous_sha256":' + canonical_json(previous) + b',"state":'
    rest = rest[64:]
    if len(rest) <= len(middle):
        return middle.startswith(rest)
    if not rest.startswith(middle):
        return False
    decoder = codecs.getincrementaldecoder('utf-8')()
    try:
        text = decoder.decode(rest[len(middle):], final=False)
    except UnicodeDecodeError:
        return False
    pending_utf8 = decoder.getstate()[0]
    if len(pending_utf8) >= 2:
        # Some codecs buffer prefixes that cannot complete to a Unicode scalar.
        low, high = {0xe0: (0xa0, 0xbf), 0xed: (0x80, 0x9f),
                     0xf0: (0x90, 0xbf), 0xf4: (0x80, 0x8f)}.get(pending_utf8[0], (0x80, 0xbf))
        if not low <= pending_utf8[1] <= high:
            return False
    if not text.startswith('{'):
        return False
    valid, value, end = _state_prefix(text, pending_utf8)
    if not valid:
        return False
    if end is None:
        return True
    _state(value)
    if canonical_json(value).decode() != text[:end]:
        return False
    tail = ',"uuid":' + canonical_json(journal_id).decode() + '}\n'
    return len(text[end:]) < len(tail) and tail.startswith(text[end:])


def _retains_policy(before, after):
    if type(before) is dict:
        return type(after) is dict and all(key in after and _retains_policy(value, after[key])
                                          for key, value in before.items())
    if type(before) is list:
        return type(after) is list and all(any(canonical_json(value) == canonical_json(item) for item in after)
                                          for value in before)
    return canonical_json(before) == canonical_json(after)


def _intent_evidence(before, after):
    """Retain dispatch bindings and all receipts while allowing lifecycle progress."""
    mutable = {'state', 'resource_id', 'ack', 'original_ack', 'late_acknowledgements', 'actions',
               'acknowledgement_validated', 'required_action_identified', 'pending_actions', 'recovery'}
    need(all(name in after and canonical_json(after[name]) == canonical_json(value)
             for name, value in before.items() if name not in mutable),
         'orphan changed headed intent binding')
    if before.get('resource_id') is not None:
        need('resource_id' in after and canonical_json(after['resource_id']) == canonical_json(before['resource_id']),
             'orphan changed headed target ID')
    elif after.get('resource_id') is not None:
        need(before.get('operation') == 'create', 'orphan changed headed target ID')
    if before.get('recovery') is not None:
        prior, current = before['recovery'], after.get('recovery')
        need(type(prior) is dict and type(current) is dict
             and all(name in current and canonical_json(current[name]) == canonical_json(value)
                     for name, value in prior.items()), 'orphan changed headed recovery proof')

    def acknowledgements(intent):
        alternatives = intent.get('late_acknowledgements', [])
        need(type(alternatives) is list, 'invalid recovery acknowledgement alternatives')
        return [value for value in (intent.get('ack'), intent.get('original_ack'), *alternatives) if value is not None]

    retained = acknowledgements(after)
    need(all(any(canonical_json(old) == canonical_json(current) for current in retained)
             for old in acknowledgements(before)), 'orphan discarded headed acknowledgement')


def _closed_successor(candidate, headed, successor):
    _state(successor)
    need(successor['mode'] == 'CLOSING' and successor['active_lease'] is None,
         'journal recovery must close admission and clear the send lease')
    for name, value in candidate.items():
        if name not in {'mode', 'active_lease', 'intents', 'events'}:
            need(name in successor and successor[name] == value, 'journal recovery discarded state evidence')
    need(successor['events'][:len(candidate['events'])] == candidate['events'],
         'journal recovery discarded adverse events')
    need(len(successor['intents']) == len(candidate['intents']), 'journal recovery changed intent population')
    for index, (before, after) in enumerate(zip(candidate['intents'], successor['intents'])):
        need(type(before) is dict and type(after) is dict, 'invalid recovery intent record')
        need(all(name in after and after[name] == value for name, value in before.items() if name != 'state'),
             'journal recovery discarded intent proof')
        old = headed['intents'][index] if index < len(headed['intents']) else {}
        need(type(old) is dict, 'invalid headed recovery intent')
        if old.get('state') in {'SENDING', 'UNKNOWN'} or before.get('state') == 'UNKNOWN':
            need(after.get('state') == 'UNKNOWN', 'journal recovery cleared an ambiguous dispatch')
        elif before.get('state') in {'PREPARED', 'SENDING'}:
            need(before.get('ack') is None and (before.get('operation') != 'create' or before.get('resource_id') is None),
                 'contradictory pre-dispatch recovery evidence')
            need(after.get('state') == 'NOT_DISPATCHED', 'journal recovery lost pre-dispatch distinction')
        else:
            need(after.get('state') == before.get('state'), 'journal recovery changed terminal evidence')


class Journal:
    @classmethod
    def initialize(cls, path, header, initial_state):
        """Create a fresh journal; incomplete initialization is never resumed."""
        _header(header)
        _state(initial_state)
        header, initial_state = copy.deepcopy(header), copy.deepcopy(initial_state)
        path = _path(path)
        mc.require_outside_repo(path, 'creation ledger')
        if not path.parent.exists():
            os.mkdir(path.parent, 0o700)
            parent = os.open(path.parent.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
            try:
                os.fsync(parent)
            finally:
                os.close(parent)
        directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            _private(os.fstat(directory), directory=True)
            need(not _names(directory), 'journal directory is not empty')
            def create(name, data):
                fd = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=directory)
                try:
                    _private(os.fstat(fd))
                    with os.fdopen(os.dup(fd), 'wb') as stream:
                        stream.write(data)
                        stream.flush()
                    os.fsync(fd)
                    return _identity(os.fstat(fd))
                finally:
                    os.close(fd)
            key = os.urandom(32)
            key_identity = create(KEY, key)
            lock_identity = create(LOCK, b'')
            anchor_fd = os.open(ANCHOR, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=directory)
            try:
                _private(os.fstat(anchor_fd))
                journal_id, header_hash = str(uuid.uuid4()), sha256(header)
                first = _seal(dict(format=GENERATION_FORMAT, uuid=journal_id, header_sha256=header_hash,
                                   generation=0, previous_sha256='0' * 64, state=initial_state), key)
                create(_generation(0), canonical_json(first) + b'\n')
                anchor = _seal(dict(format=ANCHOR_FORMAT, uuid=journal_id, header=header, header_sha256=header_hash,
                    initial_generation=0, initial_sha256=sha256(first), directory=_identity(os.fstat(directory)),
                    anchor=_identity(os.fstat(anchor_fd)), key=key_identity, key_sha256=sha256(key), lock=lock_identity,
                    head_name=path.name), key)
                with os.fdopen(os.dup(anchor_fd), 'wb') as stream:
                    stream.write(canonical_json(anchor) + b'\n')
                    stream.flush()
                os.fsync(anchor_fd)
            finally:
                os.close(anchor_fd)
            os.fsync(directory)
            temporary = '.head-' + uuid.uuid4().hex
            create(temporary, canonical_json(first) + b'\n')
            os.rename(temporary, path.name, src_dir_fd=directory, dst_dir_fd=directory)
            os.fsync(directory)
        finally:
            os.close(directory)
        return cls(path)

    def _open_reader(self, path):
        self.path = _path(path)
        self._directory = os.open(self.path.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        self._directory_identity = _identity(os.fstat(self._directory))
        self._anchor_raw = self._key = self._header = None
        self._accepted = None
        self._chain = ()
        self._held_by = None
        self._mutex = threading.Lock()
        self._poisoned = False

    @classmethod
    def recover_closed(cls, path, *, evidence_directory, authorize, close_state):
        """One-shot guard-led repair; callbacks run under the journal lock.

        authorize(header, binding, checkpoint, headed_state) proves quiescence and
        pinned actor/source identity. close_state(candidate, receipt) normalizes
        interrupted records. Neither callback may send mutations or reenter locks.
        Pre-publication validation or authorization refusals do not create a marker.
        After marker publication begins, a failed repair preserves the refusal
        barrier on a best-effort basis and requires an owner incident.
        """
        journal = cls.__new__(cls)
        journal._open_reader(path)
        lock_fd = archive_root_fd = archive_fd = None
        success = False
        marker_started = False
        try:
            journal._check_directory()
            lock_fd, lock_identity = journal._file(LOCK, os.O_RDWR)
            try:
                fcntl.flock(lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                raise mc.GuardError('journal recovery requires quiescent unlocked actors') from None
            need(RECOVERY not in _names(journal._directory), 'prior journal recovery requires owner incident')
            headed = journal._load(lock_identity, observational=True)
            need(headed['mode'] != 'COMPLETE', 'completed journal cannot recover creation admission')
            headed_checkpoint = journal.checkpoint
            names = _names(journal._directory)
            head_raw, head_identity = journal._read_file(journal.path.name)
            saved = {journal.path.name: (head_raw, head_identity), ANCHOR: (journal._anchor_raw, journal.binding['anchor'])}
            candidate = copy.deepcopy(headed)
            orphan_checkpoint = None
            next_name = _generation(headed_checkpoint['generation'] + 1)
            orphan_raw = None
            if next_name in names:
                orphan_raw, orphan_identity = journal._read_file(next_name)
                saved[next_name] = (orphan_raw, orphan_identity)
                record = journal._recovery_generation(orphan_raw, headed_checkpoint)
                if record is not None:
                    candidate = copy.deepcopy(record['state'])
                    orphan_checkpoint = dict(generation=record['generation'], sha256=sha256(record))
            temporary = [name for name in names if name.startswith('.head-')]
            for name in temporary:
                raw, file_identity = journal._read_file(name)
                saved[name] = (raw, file_identity)
                need(orphan_checkpoint is not None and orphan_raw.startswith(raw), 'forked or unbound recovery temporary head')
                journal._recovery_generation(raw, headed_checkpoint)
            need(all(record in candidate['owned'] for record in headed['owned'])
                 and all(record in candidate['associations'] for record in headed['associations']),
                 'orphan discarded headed ownership evidence')
            need(canonical_json(candidate['events'][:len(headed['events'])]) == canonical_json(headed['events']),
                 'orphan discarded headed events')
            if 'protections' in headed:
                need('protections' in candidate and _retains_policy(headed['protections'], candidate['protections']),
                     'orphan discarded headed protections')
            need(len(candidate['intents']) >= len(headed['intents']), 'orphan discarded headed intents')
            for old, current in zip(headed['intents'], candidate['intents']):
                need(type(old) is dict and type(current) is dict, 'invalid recovery intent record')
                need(type(current.get('actions', [])) is list
                     and all(any(canonical_json(action) == canonical_json(kept) for kept in current.get('actions', []))
                             for action in old.get('actions', [])), 'orphan discarded headed action IDs')
                _intent_evidence(old, current)
            authorization = authorize(journal.header, journal.binding, copy.deepcopy(headed_checkpoint), copy.deepcopy(headed))
            authorization = {} if authorization is None else authorization
            need(type(authorization) is dict, 'invalid recovery authorization evidence')
            _json_value(authorization, credentials=True)
            evidence = Path(os.path.abspath(os.fspath(evidence_directory)))
            mc.require_outside_repo(evidence, 'recovery evidence directory')
            _path(evidence / 'manifest.json')
            need(evidence != journal.path.parent and journal.path.parent not in evidence.parents,
                 'recovery evidence must be outside journal')
            archive = evidence / uuid.uuid4().hex
            files = []
            raw_files = []
            for index, (name, (raw, file_identity)) in enumerate(saved.items()):
                archive_name = f'evidence-{index:03d}.bin'
                files.append(dict(source=name, archive_name=archive_name, sha256=sha256(raw), size=len(raw), **file_identity))
                raw_files.append((archive_name, raw))
            receipt = dict(archive=str(archive), headed_checkpoint=headed_checkpoint, orphan_checkpoint=orphan_checkpoint,
                           candidate_source='orphan' if orphan_checkpoint is not None else 'headed',
                           authorization=authorization, files=files)
            callback_receipt = dict(copy.deepcopy(receipt), headed_state=copy.deepcopy(headed))
            successor = close_state(copy.deepcopy(candidate), callback_receipt)
            _closed_successor(candidate, headed, successor)
            archive_root_fd = journal._evidence_directory(evidence)
            os.mkdir(archive.name, 0o700, dir_fd=archive_root_fd)
            archive_fd = os.open(archive.name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=archive_root_fd)
            _private(os.fstat(archive_fd), directory=True)
            receipt['archive_identity'] = _identity(os.fstat(archive_fd))
            receipt['archive_root_identity'] = _identity(os.fstat(archive_root_fd))
            manifest = _seal(dict(format='fdb-creation-ledger-recovery-evidence/v1', binding=journal.binding, **receipt), journal._key)
            manifest_raw = canonical_json(manifest) + b'\n'
            receipt['manifest_sha256'] = sha256(manifest_raw)
            state_evidence = copy.deepcopy(receipt)
            state_evidence['owner_proof'] = state_evidence.pop('authorization')
            successor['events'].append(dict(event='journal-recovery-closed', evidence=state_evidence))
            _state(successor)
            previous = orphan_checkpoint or headed_checkpoint
            record = _seal(dict(format=GENERATION_FORMAT, uuid=journal.binding['uuid'],
                header_sha256=journal.binding['header_sha256'], generation=previous['generation'] + 1,
                previous_sha256=previous['sha256'], state=successor), journal._key)
            closed_raw = canonical_json(record) + b'\n'
            marker = _seal(dict(format='fdb-creation-ledger-recovery-marker/v1', binding=journal.binding,
                evidence=receipt, successor=dict(generation=record['generation'], sha256=sha256(record))), journal._key)
            marker_raw = canonical_json(marker) + b'\n'
            journal._recovery_pins(lock_identity, head_raw)
            need(_names(journal._directory) == names, 'journal changed during recovery authorization')
            marker_started = True
            journal._create(RECOVERY, marker_raw)
            os.fsync(journal._directory)
            # No adverse source file is retired until every archive byte is durable.
            for name, raw in raw_files:
                journal._create(name, raw, directory=archive_fd)
            journal._create('manifest.json', manifest_raw, directory=archive_fd)
            os.fsync(archive_fd)
            os.fsync(archive_root_fd)
            journal._recovery_pins(lock_identity, head_raw)
            for name, raw in [*raw_files, ('manifest.json', manifest_raw)]:
                need(journal._read_file(name, directory=archive_fd)[0] == raw, 'recovery archive evidence changed')
            for name, (raw, file_identity) in saved.items():
                need(journal._read_file(name) == (raw, file_identity), 'recovery evidence changed before retirement')
            need(_names(journal._directory) == names | {RECOVERY}, 'journal changed during recovery archive')
            retired = set(temporary)
            if orphan_raw is not None and orphan_checkpoint is None:
                retired.add(next_name)
            for name in retired:
                os.unlink(name, dir_fd=journal._directory)
            os.fsync(journal._directory)
            closed_name = _generation(record['generation'])
            journal._create(closed_name, closed_raw)
            os.fsync(journal._directory)
            head_temporary = '.head-' + uuid.uuid4().hex
            journal._create(head_temporary, closed_raw)
            journal._recovery_pins(lock_identity, head_raw)
            _path(archive / 'manifest.json')
            _private(os.lstat(archive), directory=True)
            _private(os.lstat(evidence), directory=True)
            for name, original in saved.items():
                if name not in retired:
                    need(journal._read_file(name) == original, 'retained recovery evidence changed')
            need(_identity(os.lstat(archive)) == receipt['archive_identity']
                 and _identity(os.lstat(evidence)) == receipt['archive_root_identity'], 'recovery archive directory changed')
            for name, raw in [*raw_files, ('manifest.json', manifest_raw)]:
                need(journal._read_file(name, directory=archive_fd)[0] == raw, 'recovery archive evidence changed')
            need(_names(journal._directory) == (names - retired) | {RECOVERY, closed_name, head_temporary},
                 'journal changed before recovery publication')
            need(journal._read_file(RECOVERY)[0] == marker_raw and journal._read_file(closed_name)[0] == closed_raw,
                 'recovery successor or marker changed')
            os.rename(head_temporary, journal.path.name, src_dir_fd=journal._directory, dst_dir_fd=journal._directory)
            os.fsync(journal._directory)
            os.unlink(RECOVERY, dir_fd=journal._directory)
            os.fsync(journal._directory)
            journal._load(lock_identity)
            success = True
            return journal
        finally:
            if not success and marker_started:
                # Preserve refusal even if the final marker-unlink directory sync failed.
                try:
                    if RECOVERY not in _names(journal._directory):
                        journal._create(RECOVERY, marker_raw)
                        os.fsync(journal._directory)
                except (OSError, mc.GuardError):
                    pass  # Original failure propagates; failed storage cannot promise persistence.
            for fd in (archive_fd, archive_root_fd):
                if fd is not None:
                    os.close(fd)
            if lock_fd is not None:
                fcntl.flock(lock_fd, fcntl.LOCK_UN)
                os.close(lock_fd)
            if not success:
                journal._poisoned = True
                journal.close()

    def _recovery_generation(self, raw, headed_checkpoint):
        try:
            record = json.loads(raw)
        except (ValueError, UnicodeError):
            need(_partial_generation(raw, headed_checkpoint['generation'] + 1, self.binding['header_sha256'],
                                     headed_checkpoint['sha256'], self.binding['uuid']), 'unrecognized partial recovery generation')
            return None
        canonical = canonical_json(record)
        missing_newline = raw == canonical
        need(missing_newline or canonical + b'\n' == raw, 'noncanonical recovery generation')
        body = _authenticated(record, self._key)
        need(set(body) == {'format', 'uuid', 'header_sha256', 'generation', 'previous_sha256', 'state'}
             and body['format'] == GENERATION_FORMAT, 'unknown recovery generation format')
        need(type(body['generation']) is int and body['generation'] == headed_checkpoint['generation'] + 1
             and body['uuid'] == self.binding['uuid'] and body['header_sha256'] == self.binding['header_sha256'],
             'recovery generation binding differs')
        need(body['previous_sha256'] == headed_checkpoint['sha256'], 'recovery predecessor differs')
        _state(body['state'])
        return None if missing_newline else record

    def _recovery_pins(self, lock_identity, head_raw):
        self._check_directory()
        need(self._read_file(ANCHOR) == (self._anchor_raw, self.binding['anchor']), 'recovery anchor changed')
        key, key_identity = self._read_file(KEY)
        need(hmac.compare_digest(key, self._key) and key_identity == self.binding['key'], 'recovery key changed')
        need(self._read_file(LOCK) == (b'', lock_identity), 'recovery lock changed')
        need(self._read_file(self.path.name)[0] == head_raw, 'headed journal changed during recovery')
        for number, expected in enumerate(self._chain):
            value, _, _ = self._read_json(_generation(number))
            need(sha256(value) == expected, 'headed ancestry changed during recovery')

    @staticmethod
    def _evidence_directory(path):
        _path(path / 'manifest.json')
        if not path.exists():
            os.mkdir(path, 0o700)
            fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
            try:
                os.fsync(fd)
            finally:
                os.close(fd)
        fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            _private(os.fstat(fd), directory=True)
            return fd
        except BaseException:
            os.close(fd)
            raise

    def __init__(self, path, observe=False):
        self._open_reader(path)
        try:
            self.peek() if observe else self.read()
        except BaseException:
            self.close()
            raise

    @property
    def header(self):
        return copy.deepcopy(self._header)

    @property
    def binding(self):
        anchor = json.loads(self._anchor_raw)
        return copy.deepcopy({name: anchor[name] for name in
                              ('uuid', 'header_sha256', 'directory', 'anchor', 'key', 'key_sha256', 'lock')})

    @property
    def checkpoint(self):
        return {'generation': self._accepted[0], 'sha256': self._accepted[1]}

    def is_ancestor(self, checkpoint):
        """Check the last authenticated view; caller must first read/lock/peek.

        This does not refresh the journal or confer mutation authority.
        """
        if self._poisoned or self._directory is None or type(checkpoint) is not dict:
            return False
        if set(checkpoint) != {'generation', 'sha256'}:
            return False
        number, digest = checkpoint['generation'], checkpoint['sha256']
        return (type(number) is int and 0 <= number < len(self._chain) and type(digest) is str
                and re.fullmatch(r'[0-9a-f]{64}', digest) is not None
                and hmac.compare_digest(self._chain[number], digest))

    def close(self):
        need(self._held_by is None, 'cannot close a locked journal')
        if self._directory is not None:
            os.close(self._directory)
            self._directory = None

    def __del__(self):
        directory = getattr(self, '_directory', None)
        if directory is not None:
            os.close(directory)

    def _check_directory(self):
        need(not self._poisoned, 'journal requires recovery after failed operation')
        need(self._directory is not None, 'journal is closed')
        _path(self.path)
        info = os.lstat(self.path.parent)
        _private(info, directory=True)
        need(_identity(info) == self._directory_identity == _identity(os.fstat(self._directory)),
             'journal directory identity differs')

    def _file(self, name, flags=os.O_RDONLY, directory=None):
        directory = self._directory if directory is None else directory
        _private(os.stat(name, dir_fd=directory, follow_symlinks=False))
        fd = os.open(name, flags | os.O_NOFOLLOW | os.O_CLOEXEC | os.O_NONBLOCK, dir_fd=directory)
        try:
            info = os.fstat(fd)
            # A temporary can be promoted and replaced again before this fstat.
            # Only mutable names in this journal may explain an unlinked inode.
            unlinked_mutable = False
            if directory == self._directory and info.st_nlink == 0:
                if name == self.path.name:
                    unlinked_mutable = _identity(info) != _identity(os.stat(name, dir_fd=directory, follow_symlinks=False))
                elif re.fullmatch(r'\.head-[0-9a-f]{32}', name):
                    try:
                        os.stat(name, dir_fd=directory, follow_symlinks=False)
                    except FileNotFoundError:
                        unlinked_mutable = True
            _private(info, unlinked=unlinked_mutable)
            if unlinked_mutable and name != self.path.name:
                raise FileNotFoundError(errno.ENOENT, 'journal temporary disappeared during open', name)
            if unlinked_mutable or _identity(info) != _identity(os.stat(name, dir_fd=directory, follow_symlinks=False)):
                raise _FileIdentityChanged(name)
            return fd, _identity(info)
        except BaseException:
            os.close(fd)
            raise

    def _read_file(self, name, directory=None):
        fd, identity = self._file(name, directory=directory)
        with os.fdopen(fd, 'rb') as stream:
            raw = stream.read()
        return raw, identity

    def _read_json(self, name):
        raw, identity = self._read_file(name)
        try:
            value = json.loads(raw)
        except (ValueError, UnicodeError) as error:
            raise mc.GuardError('malformed journal JSON') from error
        need(canonical_json(value) + b'\n' == raw, 'noncanonical journal JSON')
        return value, raw, identity

    def _load(self, lock_identity, observational=False):
        self._check_directory()
        lock_raw, current_lock_identity = self._read_file(LOCK)
        need(lock_raw == b'' and current_lock_identity == lock_identity, 'journal lock identity/content differs')
        key, key_identity = self._read_file(KEY)
        need(len(key) == 32, 'invalid journal integrity key')
        anchor, anchor_raw, anchor_identity = self._read_json(ANCHOR)
        body = _authenticated(anchor, key)
        need(set(body) == {'format', 'uuid', 'header', 'header_sha256', 'initial_generation', 'initial_sha256',
                          'directory', 'anchor', 'key', 'key_sha256', 'lock', 'head_name'}
             and body['format'] == ANCHOR_FORMAT, 'unknown journal anchor format')
        need(body['directory'] == self._directory_identity and body['anchor'] == anchor_identity
             and body['key'] == key_identity and body['lock'] == lock_identity, 'journal anchor identity differs')
        need(body['key_sha256'] == sha256(key) and body['head_name'] == self.path.name,
             'journal key/head binding differs')
        try:
            valid_uuid = type(body['uuid']) is str and str(uuid.UUID(body['uuid'])) == body['uuid']
        except ValueError:
            valid_uuid = False
        need(valid_uuid, 'invalid journal UUID')
        _header(body['header'])
        need(body['header_sha256'] == sha256(body['header']) and type(body['initial_generation']) is int
             and body['initial_generation'] == 0, 'journal header/initial generation differs')
        if self._anchor_raw is not None:
            need(anchor_raw == self._anchor_raw and hmac.compare_digest(key, self._key), 'journal anchor/key changed')
        head, head_raw, _ = self._read_json(self.path.name)
        head_body = _authenticated(head, key)
        need(type(head_body.get('generation')) is int and head_body['generation'] >= 0,
             'invalid journal head generation')
        names = _names(self._directory)
        generations = sorted(int(match[1]) for name in names if (match := GENERATION.fullmatch(name)))
        need(generations and generations == list(range(len(generations))), 'journal generation gap')
        expected_names = {ANCHOR, KEY, LOCK, self.path.name, *(_generation(n) for n in generations)}
        if observational:
            need(generations[-1] in (head_body['generation'], head_body['generation'] + 1),
                 'observational journal has multiple unheaded generations')
            temporary = {name for name in names if re.fullmatch(r'\.head-[0-9a-f]{32}', name)}
            need(len(temporary) <= 1 and (not temporary or generations[-1] == head_body['generation'] + 1),
                 'unexpected journal temporary heads')
            for name in temporary:
                fd, _ = self._file(name)
                os.close(fd)
            expected_names |= temporary
            if generations[-1] == head_body['generation'] + 1:
                fd, _ = self._file(_generation(generations[-1]))
                os.close(fd)
            generations = [number for number in generations if number <= head_body['generation']]
            need(generations and generations[-1] == head_body['generation'], 'journal head has no generation')
        need(names == expected_names, 'unexpected journal files or crash residue')
        previous = '0' * 64
        chain = []
        accepted_hash = None
        latest_raw = latest = None
        for number in generations:
            record, raw, _ = self._read_json(_generation(number))
            generation = _authenticated(record, key)
            need(set(generation) == {'format', 'uuid', 'header_sha256', 'generation', 'previous_sha256', 'state'}
                 and generation['format'] == GENERATION_FORMAT, 'unknown journal generation format')
            need(type(generation['generation']) is int and generation['generation'] == number
                 and generation['uuid'] == body['uuid'] and generation['header_sha256'] == body['header_sha256'],
                 'journal generation binding differs')
            need(generation['previous_sha256'] == previous, 'journal predecessor differs')
            _state(generation['state'])
            previous = sha256(raw[:-1])
            chain.append(previous)
            if number == 0:
                need(previous == body['initial_sha256'], 'journal initial ancestry differs')
            if self._accepted is not None and number == self._accepted[0]:
                accepted_hash = previous
            latest, latest_raw = record, raw
        need(head_raw == latest_raw and head == latest, 'journal head differs from retained chain/orphan generation')
        if self._accepted is not None:
            need(generations[-1] >= self._accepted[0] and accepted_hash == self._accepted[1],
                 'journal rollback or fork')
        self._anchor_raw, self._key, self._header = anchor_raw, key, copy.deepcopy(body['header'])
        self._accepted = (generations[-1], previous)
        self._chain = tuple(chain)
        return copy.deepcopy(latest['state'])

    @contextlib.contextmanager
    def locked(self, blocking=True):
        need(self._held_by != threading.get_ident(), 'nested journal lock')
        if not self._mutex.acquire(blocking=blocking):
            raise BlockingIOError('journal is busy')
        try:
            self._check_directory()
            lock_fd, lock_identity = self._file(LOCK, os.O_RDWR)
            try:
                fcntl.flock(lock_fd, fcntl.LOCK_EX | (0 if blocking else fcntl.LOCK_NB))
                try:
                    state = self._load(lock_identity)
                except BaseException:
                    self._poisoned = True
                    raise
                self._held_by = threading.get_ident()
                self._lock_identity = lock_identity
                try:
                    yield state
                finally:
                    self._held_by = None
            finally:
                fcntl.flock(lock_fd, fcntl.LOCK_UN)
                os.close(lock_fd)
        finally:
            self._mutex.release()

    def read(self):
        with self.locked() as state:
            return copy.deepcopy(state)

    def read_if_available(self):
        """Strict authenticated read under a nonblocking lock, or None if busy."""
        need(self._held_by != threading.get_ident(), 'nested journal lock')
        if not self._mutex.acquire(blocking=False):
            return None
        try:
            self._check_directory()
            lock_fd, lock_identity = self._file(LOCK, os.O_RDWR)
            try:
                try:
                    fcntl.flock(lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                except BlockingIOError:
                    return None
                try:
                    return self._load(lock_identity)
                except BaseException:
                    self._poisoned = True
                    raise
                finally:
                    fcntl.flock(lock_fd, fcntl.LOCK_UN)
            finally:
                os.close(lock_fd)
        finally:
            self._mutex.release()

    def peek(self):
        """Nonblocking authenticated observation, NEVER mutation/admission authority.

        Retry at most three busy-writer publication races, then raise
        ObservationDeferred without replacing the last authenticated checkpoint.
        Callers must retain watchdog state and enforce its deadline on deferral.
        Strict read/locked never ignore residue or substitute an older view.
        """
        last_error = None
        for _ in range(3):
            self._check_directory()
            lock_fd, lock_identity = self._file(LOCK, os.O_RDWR)
            acquired = False
            try:
                try:
                    fcntl.flock(lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    acquired = True
                except BlockingIOError:
                    pass
                try:
                    return self._load(lock_identity, observational=not acquired)
                except FileNotFoundError as error:
                    temporary = re.fullmatch(r'\.head-[0-9a-f]{32}', str(error.filename))
                    if acquired or temporary is None:
                        self._poisoned = True
                        raise
                    last_error = error
                except mc.GuardError as error:
                    transient = (isinstance(error, _FileIdentityChanged) and error.name == self.path.name
                                 or str(error) == 'observational journal has multiple unheaded generations')
                    if acquired or not transient:
                        self._poisoned = True
                        raise
                    last_error = error
                except OSError:
                    self._poisoned = True
                    raise
            finally:
                if acquired:
                    fcntl.flock(lock_fd, fcntl.LOCK_UN)
                os.close(lock_fd)
        raise ObservationDeferred('journal observation deferred during publication: ' + str(last_error)) from None

    def _create(self, name, data, directory=None):
        directory = self._directory if directory is None else directory
        fd = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC,
                     0o600, dir_fd=directory)
        try:
            _private(os.fstat(fd))
            with os.fdopen(os.dup(fd), 'wb') as stream:
                stream.write(data)
                stream.flush()
            os.fsync(fd)
        finally:
            os.close(fd)

    def commit(self, state):
        need(self._held_by == threading.get_ident(), 'journal commit requires held lock')
        try:
            self._load(self._lock_identity)
            _state(state)
            number = self._accepted[0] + 1
            record = _seal(dict(format=GENERATION_FORMAT, uuid=json.loads(self._anchor_raw)['uuid'],
                header_sha256=sha256(self._header), generation=number, previous_sha256=self._accepted[1], state=state), self._key)
            raw = canonical_json(record) + b'\n'
            self._create(_generation(number), raw)
            os.fsync(self._directory)
            temporary = '.head-' + uuid.uuid4().hex
            self._create(temporary, raw)
            os.rename(temporary, self.path.name, src_dir_fd=self._directory, dst_dir_fd=self._directory)
            os.fsync(self._directory)
            self._load(self._lock_identity)
        except BaseException:
            self._poisoned = True
            raise
