"""Shared helpers for the multi-node campaign scripts. Standard library only.

Nothing here talks to a cloud API or a host; callers inject a runner so the
unit tests never execute hcloud, ssh or fdbcli.
"""
import datetime as dt
import ipaddress
import json
import os
from pathlib import Path
import re
import shlex
import subprocess

# hcloud label values: at most 63 characters, alphanumerics at both ends,
# '-', '_' and '.' inside.
LABEL_VALUE_RE = re.compile(r'[A-Za-z0-9](?:[A-Za-z0-9._-]{0,61}[A-Za-z0-9])?')
# Resource names we create: lowercase DNS-ish.
NAME_RE = re.compile(r'[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?')


class GuardError(RuntimeError):
    """A safety check refused to continue."""


def parse_campaign_label(value):
    """'campaign=VALUE' -> VALUE. The key must be exactly 'campaign'."""
    key, sep, label = value.partition('=')
    if key != 'campaign' or not sep or not LABEL_VALUE_RE.fullmatch(label):
        raise GuardError('label must be campaign=VALUE with an hcloud-valid value: ' + value)
    return label


def check_name(name):
    if not NAME_RE.fullmatch(name):
        raise GuardError('unsafe resource name: ' + name)
    return name


def owned_name(name, prefix):
    """A campaign resource is the prefix itself or prefix-<suffix>."""
    return name == prefix or name.startswith(prefix + '-')


def parse_deadline(value):
    """ISO-8601 UTC ('2026-10-15T06:00:00Z') or integer epoch seconds -> epoch int."""
    if re.fullmatch(r'[0-9]{9,11}', value):
        return int(value)
    text = value[:-1] + '+00:00' if value.endswith('Z') else value
    parsed = dt.datetime.fromisoformat(text)
    if parsed.tzinfo is None or parsed.utcoffset() != dt.timedelta(0):
        raise GuardError('deadline must be UTC (suffix Z or +00:00) or epoch seconds: ' + value)
    return int(parsed.timestamp())


def iso(epoch):
    return dt.datetime.fromtimestamp(epoch, dt.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')


def inside_git_worktree(path):
    """True if path (existing or not) lies inside a directory holding .git."""
    p = Path(path).resolve()
    for parent in [p, *p.parents]:
        if (parent / '.git').exists():
            return True
    return False


def require_outside_repo(path, what):
    if inside_git_worktree(path):
        raise GuardError(f'{what} {path} is inside a Git work tree; campaign state must live outside the repository')


def private_dir(path):
    """Create (or accept an existing empty-or-owned) 0700 directory."""
    p = Path(path)
    p.mkdir(parents=True, exist_ok=True)
    os.chmod(p, 0o700)
    return p


def write_private(path, text, exclusive=True):
    """Write text with mode 0600; refuse to overwrite unless exclusive is False."""
    flags = os.O_WRONLY | os.O_CREAT | (os.O_EXCL if exclusive else os.O_TRUNC)
    fd = os.open(path, flags, 0o600)
    with os.fdopen(fd, 'w') as stream:
        stream.write(text)
    os.chmod(path, 0o600)


def dump_json(path, value, exclusive=True):
    require_outside_repo(path, 'output')
    with open(path, 'x' if exclusive else 'w') as stream:
        json.dump(value, stream, indent=2, sort_keys=True, allow_nan=False)
        stream.write('\n')


def ip_in(network, address):
    return ipaddress.ip_address(address) in ipaddress.ip_network(network)


def quote(argv):
    return shlex.join(str(a) for a in argv)


def default_runner(argv, timeout=120):
    """Run argv (no shell); return (returncode, stdout, stderr)."""
    done = subprocess.run([str(a) for a in argv], capture_output=True, text=True, timeout=timeout)
    return done.returncode, done.stdout, done.stderr
