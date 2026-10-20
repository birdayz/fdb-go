#!/usr/bin/env python3
"""Check that every imported campaign file still parses after sanitization.

Reads docs/perf/IMPORT-PROVENANCE.tsv. Each destination ending in .json, .jsonl,
.py or .sh must parse (json.loads per document or line, compile(), bash -n),
unless its provenance notes say `source-unparseable` (the original did not parse
either). With FDB_PERF_HOME (and TMPDIR for /tmp sources) set, also parses each
available source and fails if a source parses but its destination does not.

Usage: check_imports.py [repo root]. Exit 0 all parse, 1 otherwise.
"""
import csv
import json
import os
from pathlib import Path
import subprocess
import sys

KINDS = ('.json', '.jsonl', '.py', '.sh')


def parses(path):
    """None if path parses for its type, else the error text."""
    data = Path(path).read_bytes()
    try:
        if path.endswith('.jsonl'):
            for line in data.decode().splitlines():
                if line.strip():
                    json.loads(line)
        elif path.endswith('.json'):
            json.loads(data)
        elif path.endswith('.py'):
            compile(data, path, 'exec')
        elif path.endswith('.sh'):
            done = subprocess.run(['bash', '-n', path], capture_output=True, text=True)
            if done.returncode:
                return done.stderr.strip() or 'bash -n failed'
    except (ValueError, SyntaxError) as exc:
        return f'{type(exc).__name__}: {exc}'
    return None


def source_path(src):
    for var, placeholder in (('FDB_PERF_HOME', '${FDB_PERF_HOME}'), ('TMPDIR', '${TMPDIR}')):
        if src.startswith(placeholder) and os.environ.get(var):
            return os.environ[var] + src[len(placeholder):]
    return None


def main(argv):
    root = Path(argv[1] if len(argv) > 1 else Path(__file__).resolve().parents[2])
    rows = list(csv.DictReader((root / 'docs/perf/IMPORT-PROVENANCE.tsv').open(), delimiter='\t'))
    checked = skipped = compared = 0
    failures = []
    for row in rows:
        dst = row['destination']
        if not dst.endswith(KINDS):
            continue
        path = root / dst
        if not path.exists():
            failures.append(f'{dst}: missing')
            continue
        error = parses(str(path))
        src = source_path(row['source'])
        if src and Path(src).exists():
            compared += 1
            if parses(src) is not None:
                skipped += 1
                continue
        if 'source-unparseable' in row['substitutions']:
            skipped += 1
            continue
        checked += 1
        if error:
            failures.append(f'{dst}: {error}')
    for f in failures:
        print('FAIL', f)
    print(f'{checked} imported files parse-checked, {skipped} with an unparseable source skipped, '
          f'{compared} compared with an available source, {len(failures)} failures')
    if checked == 0:
        print('FAIL: empty population')
        return 1
    return 1 if failures else 0


if __name__ == '__main__':
    sys.exit(main(sys.argv))
