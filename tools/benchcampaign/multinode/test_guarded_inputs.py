#!/usr/bin/env python3
"""Initializer input/policy provenance and checker census retirement controls.

Real guarded plans, Journal initialization, authorization and cleanup are used.
Only private filesystem inputs and synthetic provider/process boundaries change.
"""
import copy
import os
from pathlib import Path
import sys
import unittest

HERE = Path(os.path.abspath(__file__)).parent
sys.path.insert(0, str(HERE))

import campaign_cloud as cc  # noqa: E402
import mn_common as mc  # noqa: E402
import provision  # noqa: E402
from test_guarded_initialize import Initialization  # noqa: E402
from test_guarded_orchestration import plan_creates  # noqa: E402
import test_guarded_readbacks as rb  # noqa: E402


def initialize_plan(f, plan):
    journal = provision.initialize_guarded(f.args, f.deadline, f.profile, f.cloud, plan)
    f.stack.callback(journal.close)
    f.journal, f.header = journal, journal.header
    return journal


class InitializerInputTest(unittest.TestCase):
    def test_initializer_refuses_key_userdata_and_protection_drift_during_real_census(self):
        for changed in ('key', 'userdata', 'protect'):
            with self.subTest(changed=changed), Initialization() as f:
                rb.backend(f)
                plan = f.plan()
                original_plan = copy.deepcopy(list(plan))
                paths = {'key': f.key, 'userdata': f.userdata['client'], 'protect': f.protect}
                path = paths[changed]
                approved = path.read_bytes()
                key_entry = next(p for p in plan if p['kind'] == 'ssh-key')
                client = next(p for p in plan if p['name'] == f.args.prefix + '-client')
                self.assertEqual(key_entry['body']['public_key'], f.key.read_bytes().decode().strip())
                self.assertEqual(client['body']['user_data'].encode(), f.userdata['client'].read_bytes())
                parsed_names = list(f.args.protected_name)
                replacement = {
                    'key': b'ssh-ed25519 SYNTHETIC-CHANGED-INITIALIZATION-KEY changed@test\n',
                    'userdata': b'#cloud-config\n# changed after approved plan construction\n',
                    'protect': approved + b'server\t99999\tlate-census-protection\n',
                }[changed]
                boundaries = []
                def drift(read):
                    if read['site'].startswith('provision.py:initialize_guarded') and not boundaries:
                        self.assertEqual(read['operation'], 'list')
                        self.assertEqual(list(plan), original_plan)
                        self.assertEqual(path.read_bytes(), approved)
                        boundaries.append((read['operation'], read['kind'], read['site']))
                        path.write_bytes(replacement)
                    return rb.DEFAULT
                f.cloud.on_read = drift
                with self.assertRaises(mc.GuardError):
                    initialize_plan(f, plan)
                self.assertEqual(len(boundaries), 1)
                self.assertEqual(path.read_bytes(), replacement)
                self.assertNotEqual(cc.digest(approved), cc.digest(replacement))
                self.assertEqual(list(plan), original_plan)
                self.assertEqual(f.args.protected_name, parsed_names)
                self.assertEqual(f.cloud.requests, [])
                self.assertFalse(f.ledger.exists())
                self.assertFalse((f.work / 'hosts-all.json').exists())

    def test_unchanged_initializer_seals_the_same_key_userdata_and_protection_bytes_as_plan(self):
        with Initialization() as f:
            rb.backend(f)
            paths = [f.key, *f.userdata.values(), f.protect]
            approved = {str(path): path.read_bytes() for path in paths}
            plan = f.plan()
            journal = initialize_plan(f, plan)
            header = journal.header
            by_name = {p['name']: p for p in header['plan']}
            key_name = next(p['name'] for p in plan if p['kind'] == 'ssh-key')
            self.assertEqual(by_name[key_name]['descriptor']['public_key'],
                             approved[str(f.key)].decode().strip())
            self.assertEqual(header['private_inputs'][str(f.key)], cc.digest(approved[str(f.key)]))
            for role, path in f.userdata.items():
                name = f.args.prefix + '-' + role
                submitted = next(p['body']['user_data'] for p in plan if p['name'] == name).encode()
                self.assertEqual(submitted, approved[str(path)])
                self.assertEqual(by_name[name]['descriptor']['user_data_sha256'], cc.digest(submitted))
                self.assertEqual(header['private_inputs'][str(path)], cc.digest(submitted))
            self.assertEqual(header['private_inputs'][str(f.protect)], cc.digest(approved[str(f.protect)]))
            self.assertEqual([r['kind'] for r in f.cloud.reads
                              if r['site'].startswith('provision.py:initialize_guarded')], list(mc.PROTECT_KINDS))
            f.campaign(journal).validate_pins()
            self.assertEqual(f.cloud.requests, [])
            self.assertEqual(journal.checkpoint['generation'], 0)
            self.assertEqual(rb.reopened(f).header['private_inputs'], header['private_inputs'])

    def test_initializer_only_nonbaseline_protections_seal_exact_policy_and_block_reopened_cleanup(self):
        for protection in ('server', 'primary-ip', 'literal-name', 'unprotected-positive'):
            with self.subTest(protection=protection), Initialization() as f:
                rb.backend(f)
                # This unrelated literal must not become a wildcard. The target
                # protection is supplied ONLY to initialization, never cleanup.
                literal = 'literal*?['
                lines = ['network\t99999\t' + literal + '\n']
                if protection == 'server':
                    lines.append('server\t101\t\n')
                elif protection == 'primary-ip':
                    lines.append('primary-ip\t10101\t\n')
                elif protection == 'literal-name':
                    lines.append('network\t99998\t' + f.args.prefix + '-client\n')
                policy_file = f.private('initialization-only-protections.tsv', ''.join(lines))
                f.args = provision.parse_args([*f.argv, '--protect-file', str(policy_file)])
                plan = f.plan()
                journal = initialize_plan(f, plan)
                expected_names = [row['name'] for rows in f.baseline.values() for row in rows.values()]
                expected_names.append('literal[*][?][[]')
                expected_typed = {kind: sorted(rows) for kind, rows in f.baseline.items()}
                expected_typed['network'] = sorted([*expected_typed['network'], 99999])
                if protection == 'server':
                    expected_typed['server'] = sorted([*expected_typed['server'], 101])
                elif protection == 'primary-ip':
                    expected_typed['primary-ip'] = sorted([*expected_typed['primary-ip'], 10101])
                elif protection == 'literal-name':
                    expected_names.append(f.args.prefix + '-client')
                    expected_typed['network'] = sorted([*expected_typed['network'], 99998])
                expected = dict(names=sorted(set(expected_names)), ids=[], by_kind=expected_typed)
                self.assertEqual(journal.header['protections'], expected)
                self.assertNotIn(101, journal.header['baseline']['server'])
                self.assertNotIn(10101, journal.header['baseline']['primary-ip'])
                self.assertEqual(journal.header['private_inputs'][str(policy_file)], cc.digest(policy_file.read_bytes()))
                campaign = f.campaign(journal)
                f.actor = 112
                campaign.arm_guard()
                f.actor = 111
                if protection == 'literal-name':
                    with self.assertRaisesRegex(mc.GuardError, 'additional guard protection conflicts with a planned name'):
                        plan_creates(f, campaign, count=4)
                    self.assertEqual(len(journal.read()['owned']), 3)
                    self.assertFalse(any(i['kind'] == 'server' for i in journal.read()['intents']))
                    self.assertFalse(any(path == '/servers' for _, path, _, _ in f.cloud.requests))
                else:
                    plan_creates(f, campaign, count=4)
                    self.assertEqual(len(journal.read()['owned']), 4)
                saved = rb.reopened(f)
                self.assertEqual(saved.header['protections'], expected)
                self.assertEqual(saved.header['plan'], journal.header['plan'])
                # Keep pinned files unchanged for validate_pins. No protection
                # file or policy argument is forwarded to this fresh Campaign.
                f.actor = 112
                f.processes.pop(111)
                cleanup = f.campaign(saved)
                cleanup.close_admission()
                result = cleanup.cleanup_pass()
                server_deletes = [path for method, path, _, _ in f.cloud.requests
                                  if method == 'DELETE' and path.startswith('/servers/')]
                self.assertEqual(server_deletes, ['/servers/101'] if protection == 'unprotected-positive' else [])
                self.assertEqual(result['complete'], protection in {'literal-name', 'unprotected-positive'})
                self.assertEqual(101 in f.cloud.rows['server'], protection in {'server', 'primary-ip'})
                self.assertEqual(10101 in f.cloud.rows['primary-ip'], protection in {'server', 'primary-ip'})
                self.assertEqual(rb.reopened(f).header['protections'], expected)
                if protection in {'literal-name', 'unprotected-positive'}:
                    self.assertTrue(result['baseline_equal'])
                    for kind, ids in saved.header['baseline'].items():
                        self.assertEqual(sorted(f.cloud.rows[kind]), ids)


class CheckerCensusRetirementTest(unittest.TestCase):
    def test_missing_second_server_ip_is_retired_before_earlier_server_auth_failure(self):
        with Initialization() as f:
            rb.backend(f)
            journal, _ = rb.create_servers(f, count=3)
            args = rb.checker_args(f, 'node2')
            previous_state = b'{"status":"previous-checker-state"}\n'
            previous_owned = b'{"status":"previous-owned-ips"}\n'
            f.private(Path(args.state).name, previous_state.decode())
            f.private(Path(args.owned_output).name, previous_owned.decode())
            original_servers = {sid: copy.deepcopy(f.cloud.rows['server'][sid]) for sid in (101, 102, 103)}
            missing_ip = copy.deepcopy(f.cloud.rows['primary-ip'].pop(10102))
            f.cloud.rows['server'][101]['labels']['campaign'] = 'independently-wrong-earlier-server'
            events_before = copy.deepcopy(journal.read()['events'])
            requests_before = copy.deepcopy(f.cloud.requests)
            start_read = len(f.cloud.reads)
            with self.assertRaisesRegex(mc.GuardError, 'observed-retired ID cannot inherit ownership'):
                rb.checker(f, args)
            reads = f.cloud.reads[start_read:]
            self.assertEqual([(r['operation'], r['kind'], r['id']) for r in reads],
                             [('describe', 'server', 101), ('describe', 'server', 102),
                              ('describe', 'server', 103), ('list', 'primary-ip', None)])
            self.assertTrue(all(r['site'].startswith('primary_ip_check.py:check') and r['transmitted'] for r in reads))
            saved = rb.reopened(f)
            after = saved.read()
            self.assertIn(('primary-ip', 10102, 'primary-check-census'), rb.retirements(after))
            self.assertTrue(any(kind == 'server' and rid == 101 for kind, rid, _ in rb.retirements(after)))
            self.assertEqual(sum(e.get('event') == 'cascade-readback' for e in after['events']),
                             sum(e.get('event') == 'cascade-readback' for e in events_before))
            self.assertFalse(any(e.get('event') == 'primary-ip-check' for e in after['events']))
            self.assertEqual(f.cloud.requests, requests_before)
            self.assertEqual(Path(args.state).read_bytes(), previous_state)
            self.assertEqual(Path(args.owned_output).read_bytes(), previous_owned)
            # Restore all original-looking metadata. Server101's observed
            # identity drift remains retired independently of IP10102's census
            # absence. Server103 is the unaffected real-cleanup positive.
            f.cloud.rows['server'].update(copy.deepcopy(original_servers))
            f.cloud.rows['primary-ip'][10102] = copy.deepcopy(missing_ip)
            self.assertEqual(f.cloud.describe('server', 101), original_servers[101])
            self.assertEqual(f.cloud.describe('server', 102), original_servers[102])
            self.assertEqual(f.cloud.describe('primary-ip', 10102), missing_ip)
            f.actor = 112
            f.processes.pop(111)
            cleanup = f.campaign(saved)
            cleanup.close_admission()
            for sid in (101, 102):
                with self.assertRaisesRegex(mc.GuardError, 'retired'):
                    cleanup.mutate('server', sid, 'delete')
            self.assertEqual(f.cloud.requests, requests_before)
            result = cleanup.cleanup_pass()
            self.assertFalse(result['complete'])
            server_deletes = [path for method, path, _, _ in f.cloud.requests
                              if method == 'DELETE' and path.startswith('/servers/')]
            self.assertEqual(server_deletes, ['/servers/103'])
            self.assertIn(101, f.cloud.rows['server'])
            self.assertIn(102, f.cloud.rows['server'])
            self.assertNotIn(103, f.cloud.rows['server'])
            self.assertNotIn(10103, f.cloud.rows['primary-ip'])
            self.assertEqual(f.cloud.rows['primary-ip'][10102], missing_ip)
            self.assertIn(('primary-ip', 10102, 'primary-check-census'), rb.retirements(rb.reopened(f).read()))


if __name__ == '__main__':
    unittest.main()
