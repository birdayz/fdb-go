#!/usr/bin/env python3
"""R2 adapter: literal PP hash guard around the frozen placement_command.py.

This does not alter that command or its placement judgment. Its original
per-probe/status command receipts are retained; an additional exclusive receipt
records the literal PP and cluster hashes before/after both commands.
"""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time

from p0 import Failure, dump, need, safe, sha


def main(argv=None):
    os.umask(0o077)
    ap = argparse.ArgumentParser(description=__doc__, add_help=False)
    ap.add_argument('--expect-placementprobe-sha256', required=True)
    ap.add_argument('--tooling', required=True)
    extra, rest = ap.parse_known_args(argv)
    command_receipt = None
    started = time.time()
    commands = []
    try:
        need(Path(extra.tooling).is_absolute(), '--tooling must be absolute')
        safe(extra.tooling)
        sys.path.insert(0, extra.tooling)
        import placement_command as pc
        args = pc.parse_args(rest)
        pc.validate(args)
        command_receipt = safe(args.output + '.command.json')
        need(not os.path.lexists(command_receipt), 'hash command receipt exists')
        need(re.fullmatch('[0-9a-f]{64}', extra.expect_placementprobe_sha256) is not None, 'invalid PP hash literal')
        literals = dict(cluster_file_sha256=args.expect_cluster_sha256,
                        placementprobe_sha256=extra.expect_placementprobe_sha256)

        def check():
            need(sha(args.cluster_file) == literals['cluster_file_sha256'], 'pinned cluster file changed')
            need(sha(args.placementprobe) == literals['placementprobe_sha256'], 'pinned placementprobe changed')

        check()
        pins = {str(p): sha(p) for p in Path(extra.tooling).glob('*.py')}
        pins[str(Path(__file__).absolute())] = sha(__file__)
        pins[str(Path(__file__).with_name('p0.py').absolute())] = sha(Path(__file__).with_name('p0.py'))

        def runner(argv, timeout=None):
            check()
            start = time.time()
            attempt = dict(argv=argv, returncode=None, started_epoch=start, timeout=False, **literals)
            commands.append(attempt)
            try:
                rc, out, err = pc.mc.default_runner(argv, timeout=timeout)
            except (subprocess.SubprocessError, OSError) as exc:
                attempt.update(error=str(exc), timeout=isinstance(exc, subprocess.TimeoutExpired),
                               duration_seconds=time.time() - start, timeout_seconds=timeout)
                for field in ('stdout', 'stderr'):
                    value = getattr(exc, field, None)
                    attempt[field] = value.decode('utf-8', errors='replace') if isinstance(value, bytes) else value
                raise
            attempt.update(returncode=rc, stdout=out, stderr=err, duration_seconds=time.time() - start)
            check()
            for p, expected in pins.items():
                need(sha(p) == expected, f'placement command input changed: {p}')
            return rc, out, err

        receipt = pc.run(args, runner=runner)
        check()
        dump(command_receipt, dict(argv=sys.argv if argv is None else argv, started_epoch=started, **literals,
             pinned_hashes=pins, commands=commands, integrity_ok=receipt['integrity_ok'],
             sealed_errors=receipt['sealed_errors']))
        print(json.dumps({k: receipt[k] for k in ('integrity_ok', 'passed', 'error_count', 'shared_shards')}, sort_keys=True))
        return 0 if receipt['integrity_ok'] and not receipt['sealed_errors'] else 1
    except (Failure, OSError, ValueError, RuntimeError, subprocess.SubprocessError) as exc:
        if command_receipt is not None and not command_receipt.exists() and command_receipt.parent.exists():
            dump(command_receipt, dict(error=str(exc), started_epoch=started, commands=commands,
                 cluster_file_sha256=locals().get('args') and args.expect_cluster_sha256,
                 placementprobe_sha256=extra.expect_placementprobe_sha256))
        print('STOP: ' + str(exc), file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main())
