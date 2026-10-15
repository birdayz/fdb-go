#!/usr/bin/env python3
"""Delete only this campaign's hcloud resources. Dry run by default.

A resource is deletable only if ALL hold:
  * it is returned by `hcloud <kind> list -o json --selector campaign=<label>`;
  * its labels really contain campaign=<label> (re-checked client-side);
  * its name is --prefix or starts with --prefix- ;
  * its name matches no --protect-name glob and its ID is not a --protect-id.
A labelled resource whose name fails the prefix/protection rule is reported as
REFUSED and makes the exit status nonzero; it is never deleted.

Kinds, in deletion order: servers, firewalls, ssh-keys, networks (a firewall or
network cannot go while a server still uses it, so those are retried). Deletion
is by numeric ID. Afterwards every kind is listed again and any surviving owned
resource is an error.

--deadline UTC_ISO|EPOCH waits (polling, interruptible) until the deadline and
then acts; with --execute that is the controller-side hard lifetime guard.
Run it in the foreground of a supervised job; the runbook arms it before
provisioning. Read-only list calls happen in dry-run mode as well.
"""
import argparse
import fnmatch
import json
from pathlib import Path
import sys
import time

import mn_common as mc

KINDS = ('server', 'firewall', 'ssh-key', 'network')


def parse_args(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('--label', required=True, help='campaign=VALUE')
    ap.add_argument('--prefix', required=True, help='owned names are PREFIX or PREFIX-*')
    ap.add_argument('--protect-name', action='append', default=[], help='glob of names never deleted (repeat)')
    ap.add_argument('--protect-id', action='append', type=int, default=[], help='resource ID never deleted (repeat)')
    ap.add_argument('--deadline', help='wait until this UTC ISO time or epoch, then act')
    ap.add_argument('--poll-seconds', type=float, default=30.0)
    ap.add_argument('--retries', type=int, default=6, help='attempts for firewall/network deletes')
    ap.add_argument('--retry-seconds', type=float, default=10.0)
    ap.add_argument('--receipt-dir', help='outside the repo: JSON receipt of what was seen and done')
    ap.add_argument('--hcloud', default='hcloud')
    ap.add_argument('--execute', action='store_true', help='actually delete (default: report only)')
    args = ap.parse_args(argv)
    args.campaign = mc.parse_campaign_label(args.label)
    mc.check_name(args.prefix)
    return args


def list_kind(args, kind, runner):
    argv = [args.hcloud, kind, 'list', '-o', 'json', '--selector', f'campaign={args.campaign}']
    rc, out, err = runner(argv)
    if rc:
        raise mc.GuardError(f'list failed: {mc.quote(argv)}: {err.strip()}')
    return json.loads(out or '[]') or []


def classify(args, kind, items):
    owned, refused = [], []
    for item in items:
        name, rid, labels = item.get('name', ''), item.get('id'), item.get('labels') or {}
        entry = dict(kind=kind, id=rid, name=name, labels=labels)
        reasons = []
        if labels.get('campaign') != args.campaign:
            reasons.append('label mismatch')
        if not mc.owned_name(name, args.prefix):
            reasons.append('name outside prefix')
        if any(fnmatch.fnmatchcase(name, g) for g in args.protect_name):
            reasons.append('protected name')
        if rid in args.protect_id:
            reasons.append('protected id')
        if not isinstance(rid, int):
            reasons.append('missing numeric id')
        (refused if reasons else owned).append(dict(entry, reasons=reasons) if reasons else entry)
    return owned, refused


def discover(args, runner):
    found = {}
    for kind in KINDS:
        owned, refused = classify(args, kind, list_kind(args, kind, runner))
        found[kind] = dict(owned=owned, refused=refused)
    return found


def wait_for_deadline(deadline, clock, sleep, poll, out):
    while True:
        remaining = deadline - clock()
        if remaining <= 0:
            return
        print(f'teardown guard armed: {int(remaining)}s until {mc.iso(deadline)}', file=out)
        sleep(min(poll, remaining))


def delete_all(args, found, runner, sleep, out):
    actions = []
    for kind in KINDS:
        for entry in found[kind]['owned']:
            argv = [args.hcloud, kind, 'delete', str(entry['id'])]
            attempts = 1 if kind == 'server' else max(1, args.retries)
            for attempt in range(attempts):
                rc, stdout, stderr = runner(argv)
                actions.append(dict(argv=argv, returncode=rc, stdout=stdout, stderr=stderr, attempt=attempt))
                if rc == 0:
                    print(f'deleted {kind} {entry["id"]} {entry["name"]}', file=out)
                    break
                if attempt + 1 < attempts:
                    sleep(args.retry_seconds)
            else:
                print(f'FAILED to delete {kind} {entry["id"]} {entry["name"]}: {stderr.strip()}', file=out)
    return actions


def run(args, runner=mc.default_runner, clock=time.time, sleep=time.sleep, out=sys.stdout):
    if args.receipt_dir:
        mc.require_outside_repo(args.receipt_dir, 'receipt dir')
    if args.deadline:
        wait_for_deadline(mc.parse_deadline(args.deadline), clock, sleep, args.poll_seconds, out)
    found = discover(args, runner)
    receipt = dict(campaign=args.campaign, prefix=args.prefix, execute=args.execute,
                   utc=mc.iso(int(clock())), found=found, actions=[], remaining=None)
    for kind in KINDS:
        for e in found[kind]['owned']:
            print(f'{"DELETE" if args.execute else "would delete"} {kind} {e["id"]} {e["name"]}', file=out)
        for e in found[kind]['refused']:
            print(f'REFUSED {kind} {e["id"]} {e["name"]}: {", ".join(e["reasons"])}', file=out)
    ok = not any(found[k]['refused'] for k in KINDS)
    if args.execute:
        receipt['actions'] = delete_all(args, found, runner, sleep, out)
        after = discover(args, runner)
        wanted = {(k, e['id']) for k in KINDS for e in found[k]['owned']}
        survivors = [dict(kind=k, id=e['id'], name=e['name']) for k in KINDS for e in after[k]['owned'] + after[k]['refused']
                     if (k, e['id']) in wanted]
        receipt['remaining'] = survivors
        if survivors:
            ok = False
            print('SURVIVING owned resources: ' + json.dumps(survivors), file=out)
    if args.receipt_dir:
        d = mc.private_dir(args.receipt_dir)
        mc.dump_json(Path(d) / f'teardown-{int(clock())}-{"execute" if args.execute else "dryrun"}.json', receipt)
    return ok, receipt


def main(argv=None):
    try:
        ok, _ = run(parse_args(argv))
    except mc.GuardError as exc:
        print('REFUSED: ' + str(exc), file=sys.stderr)
        return 2
    return 0 if ok else 1


if __name__ == '__main__':
    sys.exit(main())
