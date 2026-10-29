#!/usr/bin/env python3
"""Supervised exact-creation-ID teardown; dry run by default.

Execution requires an authenticated --creation-ledger and the pinned foreground
owner/guard identities. Immediate requests and the sealed deadline enter the same
quiescence, recovery, exact-ID authorization, cascade and ten-kind confirmation
state machine. One-shot controller HTTPS replaces every mutating CLI call.

Legacy dry-run selector discovery is diagnostic only, never deletion authority.
A matching name/label cannot be adopted. Unknown create outcomes remain pending,
including after empty lists; unsafe subordinate associations refuse server DELETE.
The earlier sealed cutover starts cleanup, not proof of a16h provider lifetime.
No timestamp or supervisor timeout proves that resources disappeared.
"""
import argparse
import contextlib
import fnmatch
import json
from pathlib import Path
import sys
import time

import mn_common as mc
import campaign_cloud as cc
import campaign_safety as cs
import creation_ledger as cl

KINDS = ('server', 'firewall', 'ssh-key', 'network')


def parse_args(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('--label', required=True, help='campaign=VALUE')
    ap.add_argument('--prefix', required=True, help='owned names are PREFIX or PREFIX-*')
    ap.add_argument('--protect-name', action='append', default=[], help='glob of names never deleted (repeat)')
    ap.add_argument('--protect-id', action='append', type=int, default=[], help='resource ID never deleted (repeat)')
    ap.add_argument('--protect-file', action='append', default=[],
                    help='private kind/id/name list (inventory.py snapshot); every ID and name is protected (repeat)')
    ap.add_argument('--deadline', help='wait until this UTC ISO time or epoch, then act')
    ap.add_argument('--poll-seconds', type=float, default=30.0)
    ap.add_argument('--receipt-dir', help='outside the repo: JSON receipt of what was seen and done')
    ap.add_argument('--hcloud', default='hcloud')
    ap.add_argument('--creation-ledger', help='mandatory exact creation provenance for execution')
    ap.add_argument('--immediate', action='store_true', help='request the same armed guard to close now')
    ap.add_argument('--recover-journal', action='store_true', help='explicit closed-only orphan recovery after producer/old-guard quiescence')
    ap.add_argument('--recovery-evidence-dir', help='fresh private adverse-recovery archive outside Git')
    ap.add_argument('--request-wait-seconds', type=float, default=60, help='bounded immediate-request confirmation wait')
    ap.add_argument('--execute', action='store_true', help='actually delete (default: report only)')
    args = ap.parse_args(argv)
    args.campaign = mc.parse_campaign_label(args.label)
    mc.check_name(args.prefix)
    args.explicit_protect_id = list(args.protect_id)
    args.typed_protection = {kind: [] for kind in mc.PROTECT_KINDS}
    for path in args.protect_file:
        protected = mc.read_protect_file(path, typed=True)
        args.protect_id += protected['ids']
        for kind, ids in protected['by_kind'].items():
            args.typed_protection[kind].extend(ids)
        # Names in the file are literal, not globs.
        args.protect_name += [''.join(f'[{c}]' if c in '*?[' else c for c in n) for n in protected['names']]
    return args


def list_kind(args, kind, runner):
    argv = [args.hcloud, kind, 'list', '-o', 'json', '--selector', f'campaign={args.campaign}']
    rc, out, err = runner(argv)
    if rc:
        raise mc.GuardError(f'list failed: {mc.quote(argv)}: {err.strip()}')
    return mc.parse_json_list(out, mc.quote(argv))


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


def guarded_run(args, sleep=time.sleep, out=sys.stdout):
    cc.need(0 < args.request_wait_seconds <= 60, 'immediate confirmation wait must be bounded to60s')
    deadline = mc.parse_deadline(args.deadline) * cs.NS if args.deadline else None
    if args.recover_journal:
        cc.need(args.recovery_evidence_dir, '--recover-journal requires --recovery-evidence-dir')
        journal = cs.recover_closed_journal(args.creation_ledger, args.recovery_evidence_dir,
            expected_campaign=args.campaign, expected_prefix=args.prefix, expected_deadline=deadline)
    else:
        journal = cl.Journal(args.creation_ledger, observe=True)
    with contextlib.closing(journal):
        return execute_guarded(args, journal, sleep=sleep, out=out)


def execute_guarded(args, journal, sleep=time.sleep, out=sys.stdout):
    header = journal.header
    cc.need(args.campaign == header['campaign'] and args.prefix == header['prefix'], 'guard campaign binding differs')
    if args.deadline:
        cc.need(mc.parse_deadline(args.deadline) * cs.NS == header['time']['g_ns'], 'guard cutover deadline changed')
    cc.need(0 < args.request_wait_seconds <= 60, 'immediate confirmation wait must be bounded to60s')
    profile = cc.Profile(header['context']['context'], header['context']['config'])
    cloud = cc.Cloud(profile, header['context'])
    campaign = cs.Campaign(journal, cloud)
    campaign.validate_pins()
    policy = cs.protection_policy(args.protect_name, args.explicit_protect_id, args.typed_protection)
    cc.need(not cc.contains_credential(policy, profile._credential),
            'controller credential found in protection policy')
    campaign._guard_protections = policy
    if policy['names'] or policy['ids'] or any(policy['by_kind'].values()):
        observed = journal.peek()
        existing = observed['guard']
        if existing is not None and cs.live(existing['identity']):
            try:
                campaign.bind_protections(policy, blocking=False)
            except BlockingIOError:
                raise mc.GuardError('additional protections could not be bound; no immediate request issued') from None
        observed['protections'] = campaign._protection(observed, policy)
        for record in observed['owned']:
            cc.need(not campaign._protected(observed, record['kind'], record['id'], record['name']),
                    'additional guard protection conflicts with an owned ID')
    if campaign.terminal_result(blocking=False) is not None:
        result = campaign.guard_loop(sleep=sleep)
    elif args.immediate:
        until = time.monotonic() + args.request_wait_seconds
        campaign.request_stop()
        while True:
            if time.monotonic() >= until:
                result = dict(complete=False, hard_lifetime_met=False, status='cleanup-request-pending')
                break
            state = journal.read_if_available()
            if state is None:
                try:
                    observed = journal.peek()
                except cl.ObservationDeferred:
                    observed = None
                if observed is not None:
                    existing = observed['guard']
                    if existing is None or not cs.live(existing['identity']):
                        result = campaign.guard_loop(sleep=sleep)
                        break
                sleep(min(0.1, max(0, until - time.monotonic())))
                continue
            for record in state['owned']:
                cc.need(not campaign._protected(state, record['kind'], record['id'], record['name']),
                        'additional guard protection conflicts with an owned ID')
            if state['mode'] == 'COMPLETE':
                result = campaign.guard_loop(sleep=sleep)
                break
            existing = state['guard']
            if existing is None or not cs.live(existing['identity']):
                result = campaign.guard_loop(sleep=sleep)
                break
            sleep(min(0.1, max(0, until - time.monotonic())))
    else:
        result = campaign.guard_loop(sleep=sleep)
    receipt = dict(campaign=header['campaign'], prefix=header['prefix'], execute=True,
                   journal=journal.binding, result=result)
    if args.receipt_dir:
        directory = mc.private_dir(args.receipt_dir)
        cs.atomic_private(Path(directory) / ('teardown-' + str(time.time_ns()) + '.json'), receipt)
    print(json.dumps(result, sort_keys=True), file=out)
    return result['complete'] and result['hard_lifetime_met'], receipt


def run(args, runner=mc.default_runner, clock=time.time, sleep=time.sleep, out=sys.stdout):
    if args.execute:
        cc.need(args.creation_ledger, '--execute requires --creation-ledger; selector-only execution is retired')
        return guarded_run(args, sleep=sleep, out=out)
    if args.receipt_dir:
        mc.require_outside_repo(args.receipt_dir, 'receipt dir')
    if args.deadline:
        wait_for_deadline(mc.parse_deadline(args.deadline), clock, sleep, args.poll_seconds, out)
    found = discover(args, runner)
    receipt = dict(campaign=args.campaign, prefix=args.prefix, execute=False,
                   utc=mc.iso(int(clock())), found=found, actions=[], remaining=None)
    for kind in KINDS:
        for e in found[kind]['owned']:
            print(f'would delete {kind} {e["id"]} {e["name"]}', file=out)
        for e in found[kind]['refused']:
            print(f'REFUSED {kind} {e["id"]} {e["name"]}: {", ".join(e["reasons"])}', file=out)
    ok = not any(found[k]['refused'] for k in KINDS)
    if args.receipt_dir:
        d = mc.private_dir(args.receipt_dir)
        mc.dump_json(Path(d) / f'teardown-{int(clock())}-dryrun.json', receipt)
    return ok, receipt


def main(argv=None, runner=mc.default_runner):
    try:
        ok, _ = run(parse_args(argv), runner=runner)
    except (mc.GuardError, OSError, ValueError, KeyError, TypeError) as exc:
        print('REFUSED: ' + (str(exc) if isinstance(exc, mc.GuardError) else type(exc).__name__), file=sys.stderr)
        return 2
    return 0 if ok else 1


if __name__ == '__main__':
    sys.exit(main())
