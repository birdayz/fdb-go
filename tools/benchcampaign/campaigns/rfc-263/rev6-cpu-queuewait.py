#!/usr/bin/env python3
"""Sealed post-timing RFC263 queue-lock diagnostic; never build or run workloads.

Validate all 16 runs. For EACH of eight production workload/profile cells require
(shard+root queue delay)/(that run's measured-window process user+system CPU) < 1%.
Baseline is comparison context, not a gate. Mutex attributes other goroutines' wait
to unlocking-holder sites; block attributes blocked-waiter sites. These views MUST NOT be added. Delay is sampled
aggregate goroutine wait, not CPU consumed. No throughput/CPU-NI/merge verdict.
"""
from __future__ import annotations

import argparse
from collections import Counter
from dataclasses import dataclass
import datetime as dt
import fcntl
import gzip
import hashlib
import importlib.util
import json
import os
from pathlib import Path, PurePosixPath
import re
import subprocess
import sys
import tarfile
import time

sys.dont_write_bytecode = True
CPUS = {0, 1, 6, 7, 11, 12, 13, 18, 19, 23}
QUEUE_FILE = 'pkg/fdbgo/client/timeout_queue.go'
PACKAGE = 'fdb.dev/pkg/fdbgo/client.'
WORKLOADS = ('grv', 'get100', 'set100', 'range100')
PROFILE_KINDS = ('mutex', 'block')
# Helpers inherit their origin from an enclosing operation, never from a guess.
CALLER = {('timeoutQueue', m): 'caller_' + m for m in ('schedule', 'retire', 'newNode', 'newNodeForShard')}
CALLER[('timeoutQueue', 'remove')] = 'caller_cleanup_remove'
DRAINER = {('timeoutQueue', m): 'drainer_' + m for m in ('fire', 'collect')}
DRAINER.update({('timeoutQueueRoot', m): 'drainer_root_' + m for m in ('fire', 'deliverReady')})
HELPERS = {('timeoutQueue', m) for m in ('pushExact', 'requestLocked', 'less', 'swap', 'up', 'down', 'removeAt')}
KNOWN = set(CALLER) | set(DRAINER) | HELPERS
METHOD_NAME = re.compile(r'^' + re.escape(PACKAGE) + r'\(\*(timeoutQueue(?:Root)?)\)\.([A-Za-z_]\w*)(\.func\d+(?:\.\d+)*)?$')
METHOD_DECL = re.compile(r'(?m)^func\s+\((q|root)\s+\*(timeoutQueue(?:Root)?)\)\s+([A-Za-z_]\w*)\s*\(')
LOCK_LINE = re.compile(r'^\s*(q|root)\.mu\.(Lock|Unlock)\(\)\s*;?\s*$')


def require(value, message):
    if not value:
        raise ValueError(message)


def digest(value):
    require(isinstance(value, str) and re.fullmatch('[0-9a-f]{64}', value), 'full SHA256 required')
    return value


def sha(path):
    result = hashlib.sha256()
    with Path(path).open('rb') as stream:
        for part in iter(lambda: stream.read(1024 * 1024), b''):
            result.update(part)
    return result.hexdigest()


def read_json(path):
    return json.loads(Path(path).read_text())


def save(path, value):
    Path(path).write_text(json.dumps(value, indent=2, sort_keys=True, allow_nan=False) + '\n')


def load_module(path, name):
    spec = importlib.util.spec_from_file_location(name, path)
    require(spec is not None and spec.loader is not None, 'cannot load frozen collector helper')
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


def mask_go(text):
    """Preserve code and line numbers; blank comments, quoted strings and runes."""
    chars, i = list(text), 0
    while i < len(text):
        start = i
        if text.startswith('//', i):
            end = text.find('\n', i)
            i = len(text) if end < 0 else end
        elif text.startswith('/*', i):
            end = text.find('*/', i + 2)
            require(end >= 0, 'unterminated Go comment')
            i = end + 2
        elif text[i] in ('"', "'", '`'):
            quote = text[i]
            i += 1
            while i < len(text):
                if text[i] == quote:
                    i += 1
                    break
                if text[i] == '\\' and quote != '`':
                    i += 2
                else:
                    i += 1
            else:
                raise ValueError('unterminated Go literal')
        else:
            i += 1
            continue
        for k in range(start, min(i, len(text))):
            if chars[k] != '\n':
                chars[k] = ' '
    return ''.join(chars)


@dataclass(frozen=True)
class Frame:
    function: str
    file: str
    line: int
    start: int = 0


@dataclass(frozen=True)
class Site:
    line: int
    family: str
    operation: str
    method: tuple[str, str]


class SourceBinding:
    def __init__(self, text, archive_sha256, role):
        require(role in ('baseline', 'production'), 'unknown source role')
        self.archive_sha256, self.role = digest(archive_sha256), role
        self.text, self.methods, self.sites = text, {}, {}
        self.source_sha256 = hashlib.sha256(text.encode()).hexdigest() if text is not None else None
        if text is None:
            require(role == 'baseline', 'production queue source missing')
            return
        masked = mask_go(text)
        lines = masked.splitlines()
        declarations = list(METHOD_DECL.finditer(masked))
        require(bool(declarations), 'queue source has no methods')
        for declaration in declarations:
            method = (declaration[2], declaration[3])
            require(method in KNOWN and method not in self.methods, 'unknown/duplicate queue source method: ' + str(method))
            opening = masked.find('{', declaration.end())
            require(opening >= 0, 'queue method has no body')
            depth, end = 1, opening + 1
            while end < len(masked) and depth:
                depth += (masked[end] == '{') - (masked[end] == '}')
                end += 1
            require(depth == 0, 'unterminated queue method')
            first, last = masked.count('\n', 0, declaration.start()) + 1, masked.count('\n', 0, end) + 1
            self.methods[method] = (first, last)
            for number in range(first, last + 1):
                match = LOCK_LINE.fullmatch(lines[number - 1])
                if match:
                    require(number not in self.sites, 'overlapping queue methods')
                    self.sites[number] = Site(number, 'shard' if match[1] == 'q' else 'root', match[2], method)
        # A changed coding idiom must fail closed, not silently shrink the census.
        occurrences = len(re.findall(r'\b(?:q|root)\.mu\.(?:Lock|Unlock)\s*\(', masked))
        require(occurrences == len(self.sites) and occurrences > 0, 'unrecognized/empty queue lock-site census')
        if role == 'production':
            require({s.family for s in self.sites.values()} == {'shard', 'root'}, 'production shard/root lock sites required')
            require(all(any(s.family == family and s.operation == operation for s in self.sites.values())
                        for family in ('shard', 'root') for operation in ('Lock', 'Unlock')), 'incomplete queue lock operations')

    @classmethod
    def archive(cls, path, expected_sha256, role):
        require(sha(path) == digest(expected_sha256), 'source archive changed')
        queue, package_files, modules = [], [], []
        names = set()
        with tarfile.open(path, 'r:*') as archive:
            for member in archive:
                name = PurePosixPath(member.name)
                require(not name.is_absolute() and '..' not in name.parts and member.name not in names, 'unsafe/duplicate source archive member')
                names.add(member.name)
                require(not member.issym() and not member.islnk(), 'source archive links forbidden')
                if not member.isfile():
                    continue
                if str(name) == QUEUE_FILE or str(name).endswith('/' + QUEUE_FILE):
                    require(member.size <= 1024 * 1024, 'oversized queue source')
                    queue.append(archive.extractfile(member).read().decode('utf-8'))
                if re.search(r'(?:^|/)pkg/fdbgo/client/[^/]+\.go$', str(name)):
                    package_files.append(str(name))
                if name.name == 'go.mod' and len(name.parts) <= 2:
                    require(member.size <= 1024 * 1024, 'oversized module source')
                    modules.append(archive.extractfile(member).read().decode('utf-8'))
        require(bool(package_files) and len(queue) <= 1, 'empty/ambiguous client source archive')
        require(len(modules) == 1 and re.search(r'(?m)^module\s+fdb\.dev\s*$', modules[0]), 'source module binding mismatch')
        return cls(queue[0] if queue else None, expected_sha256, role)

    def bind(self, frame):
        """None means an unrelated function; malformed queue identities are errors."""
        # The timer's method value may survive stack unwinding as a generated
        # wrapper. It is a known origin, never an attributable source lock site.
        if frame.function == PACKAGE + '(*timeoutQueue).fire-fm':
            method = ('timeoutQueue', 'fire')
            require(method in self.methods and frame.file == '<autogenerated>' and frame.line == 1,
                    'unbound generated queue timer wrapper')
            return method, None
        match = METHOD_NAME.fullmatch(frame.function)
        if match is None:
            require(not ('timeoutQueue' in frame.function and frame.function.startswith(PACKAGE)), 'unknown queue function identity')
            return None
        require(self.text is not None, 'queue frame present in source without queue')
        method = (match[1], match[2])
        require(method in self.methods and method in KNOWN, 'unknown queue frame method')
        require(frame.file == QUEUE_FILE or frame.file.endswith('/' + QUEUE_FILE), 'queue frame bound to wrong source file')
        first, last = self.methods[method]
        require(type(frame.line) is int and first <= frame.line <= last, 'queue frame line outside source method')
        if frame.start:
            require((first <= frame.start <= last) if match[3] else frame.start == first, 'queue frame function-start mismatch')
        site = self.sites.get(frame.line)
        require(site is None or site.method == method, 'queue lock source/function mismatch')
        return method, site


def parse_frame(text):
    if text == '??':
        return None
    # SDK pprof formats both file:line and file:line:column, then optional s=start.
    match = re.fullmatch(r'(\S+) (.+?):(\d+)(?::(\d+))?(?: s=(\d+)(?:\(.*\))?)?', text.strip())
    require(match is not None, 'unsupported pprof frame syntax: ' + text)
    return Frame(match[1], match[2], int(match[3]), int(match[5] or 0))


def parse_raw(text):
    """Decode the SDK pprof -raw shape; reject missing/empty/unknown populations."""
    lines = text.splitlines()
    require(lines.count('Samples:') == lines.count('Locations') == lines.count('Mappings') == 1, 'missing/duplicate raw sections')
    sample_start, location_start, mapping_start = (lines.index(k) for k in ('Samples:', 'Locations', 'Mappings'))
    require(sample_start + 1 < location_start < mapping_start, 'raw sections out of order')
    types = tuple(re.sub(r'\[dflt\]$', '', x) for x in lines[sample_start + 1].split())
    require(types == ('contentions/count', 'delay/nanoseconds'), 'expected count + nanosecond delay sample types')
    samples = []
    for line in lines[sample_start + 2:location_start]:
        if not line.strip():
            continue
        match = re.fullmatch(r'\s*(-?\d+)\s+(-?\d+):\s*((?:\d+\s*)+)', line)
        if match is None:
            # Label lines are indented and use name:[values], never numeric samples.
            require(bool(samples) and bool(re.fullmatch(r'\s{16,}\S+:\[.*\](?: \S+:\[.*\])*', line)), 'unsupported raw sample/label syntax')
            continue
        count, delay = int(match[1]), int(match[2])
        require(count >= 0 and delay >= 0 and (count > 0 or delay == 0), 'invalid profile sample counters')
        ids = tuple(int(x) for x in match[3].split())
        require(bool(ids) and all(x > 0 for x in ids), 'empty/invalid sample stack')
        samples.append(dict(count=count, delay_ns=delay, locations=ids))
    locations, current = {}, None
    for line in lines[location_start + 1:mapping_start]:
        if not line.strip():
            continue
        match = re.fullmatch(r'\s*(\d+): 0x[0-9a-fA-F]+ (?:M=\d+ )?(?:\[F\] )?(.*)', line)
        if match:
            current = int(match[1])
            require(current > 0 and current not in locations, 'duplicate/invalid location ID')
            locations[current] = []
            frame_text = match[2]
        else:
            require(current is not None and line.startswith('             '), 'unsupported raw location syntax')
            frame_text = line.strip()
        if frame_text:
            frame = parse_frame(frame_text)
            if frame is not None:
                locations[current].append(frame)
    require(bool(samples) and sum(s['count'] for s in samples) > 0 and sum(s['delay_ns'] for s in samples) > 0, 'empty/zero profile population')
    for sample in samples:
        require(all(i in locations and locations[i] for i in sample['locations']), 'missing/unsymbolized referenced profile location')
    return dict(samples=samples, locations=locations, sample_types=types)


def sample_classification(frames, source, profile):
    require(profile in PROFILE_KINDS, 'unknown profile kind')
    bound = [(index, source.bind(frame)) for index, frame in enumerate(frames)]
    operation = 'Unlock' if profile == 'mutex' else 'Lock'
    hits = [(index, binding[1]) for index, binding in bound if binding is not None and binding[1] is not None and binding[1].operation == operation]
    require(len(hits) <= 1, 'ambiguous multiple queue-lock sites in one sample')
    if not hits:
        return None
    index, site = hits[0]
    origin = None
    for position, binding in bound:
        if position < index or binding is None:
            continue
        method = binding[0]
        if method in CALLER or method in DRAINER:
            origin = CALLER.get(method, DRAINER.get(method))
            break
    require(origin is not None, 'unknown queue-lock caller/drainer origin')
    return site.family, 'caller' if origin.startswith('caller_') else 'drainer', origin


def summarize(raw, source, profile, cpu_ns):
    require(type(cpu_ns) is int and cpu_ns > 0, 'positive measured-window client CPU required')
    parsed = parse_raw(raw) if isinstance(raw, str) else raw
    cells = {(family, origin): dict(delay_ns=0, contentions=0, sample_rows=0) for family in ('shard', 'root') for origin in ('caller', 'drainer')}
    details = {}
    excluded_queue = 0
    for sample in parsed['samples']:
        frames = [frame for location in sample['locations'] for frame in parsed['locations'][location]]
        label = sample_classification(frames, source, profile)
        if label is None:
            excluded_queue += any(METHOD_NAME.fullmatch(f.function) for f in frames)
            continue
        family, origin, detail = label
        for bucket in (cells[(family, origin)], details.setdefault(detail, dict(delay_ns=0, contentions=0, sample_rows=0))):
            bucket['delay_ns'] += sample['delay_ns']
            bucket['contentions'] += sample['count']
            bucket['sample_rows'] += 1
    delay = sum(x['delay_ns'] for x in cells.values())
    by_origin = {origin: {key: sum(cells[(family, origin)][key] for family in ('shard', 'root'))
                         for key in ('delay_ns', 'contentions', 'sample_rows')} for origin in ('caller', 'drainer')}
    return dict(profile=profile, profile_sample_rows=len(parsed['samples']), profile_contentions=sum(s['count'] for s in parsed['samples']),
                profile_delay_ns=sum(s['delay_ns'] for s in parsed['samples']), measured_window_client_cpu_ns=cpu_ns,
                queue_delay_ns=delay, queue_contentions=sum(x['contentions'] for x in cells.values()),
                queue_sample_rows=sum(x['sample_rows'] for x in cells.values()), queue_delay_to_cpu_ratio=delay / cpu_ns,
                strict_below_one_percent=delay * 100 < cpu_ns,
                by_lock_and_origin={family: {origin: cells[(family, origin)] for origin in ('caller', 'drainer')} for family in ('shard', 'root')},
                by_origin=by_origin, origin_details=details, excluded_non_lock_queue_stack_sample_rows=excluded_queue)


def diagnostic_decision(results):
    require(len(results) == 16, 'full 16-profile result population required')
    population = Counter((r['workload'], r['binary'], r['profile']) for r in results)
    require(population == Counter({(w, role, profile): 1 for w in WORKLOADS for role in ('baseline', 'production') for profile in PROFILE_KINDS}),
            'complete production and baseline diagnostic factorial required')
    for row in results:
        require(type(row['queue_delay_ns']) is int and row['queue_delay_ns'] >= 0 and
                type(row['measured_window_client_cpu_ns']) is int and row['measured_window_client_cpu_ns'] > 0,
                'invalid queue-delay/CPU decision counters')
        require(type(row['strict_below_one_percent']) is bool and row['strict_below_one_percent'] ==
                (row['queue_delay_ns'] * 100 < row['measured_window_client_cpu_ns']), 'inconsistent strict queuewait result')
    production = [r for r in results if r['binary'] == 'production']
    return dict(verdict='PASS' if all(r['strict_below_one_percent'] for r in production) else 'FAIL',
                gated_production_profiles=8, baseline_comparison_profiles=8, baseline_is_gate=False,
                rule='EACH production workload/profile: (shard+root queue delay_ns)*100 < measured-window process CPU_ns')


def measured_cpu_ns(summary):
    differences = []
    for key in ('process_user_cpu_us', 'process_system_cpu_us'):
        before, after = summary['stats_before'][key], summary['stats_after'][key]
        require(type(before) is int and type(after) is int and 0 <= before <= after, 'invalid/decreasing measured CPU counter: ' + key)
        differences.append(after - before)
    result = sum(differences) * 1000
    require(result > 0, 'nonpositive measured-window client CPU')
    return result


def client_counters(summary):
    snapshots = [summary[k] for k in ('client_counters_before', 'client_counters_after', 'client_counters_delta')]
    require(all(s['observation'] == 'observed' and isinstance(s['values'], dict) for s in snapshots), 'unobserved client counters')
    before, after, delta = [s['values'] for s in snapshots]
    require(set(before) == set(after) == set(delta) and bool(before), 'empty/changed client counter population')
    for key in before:
        require(all(type(s[key]) is int and s[key] >= 0 for s in (before, after, delta)) and
                before[key] <= after[key] and delta[key] == after[key] - before[key], 'invalid client counter delta: ' + key)
    require({'grv_cache_hits', 'read_versions_completed', 'grv_successful_batch_replies'} <= set(delta), 'missing GRV counters')
    require(delta['grv_cache_hits'] == 0 and delta['read_versions_completed'] >= summary['completed'] and
            delta['grv_successful_batch_replies'] > 0, 'uncached workload GRV counter failure')
    return delta


def checked_population(evidence, directory, seal, rows, collector):
    require(sha(directory / 'artifact-sha256.json') == digest(seal), 'population seal changed')
    index = read_json(directory / 'artifact-sha256.json')
    require(isinstance(index, dict) and bool(index) and 'status.json' in index, 'empty/missing artifact census')
    all_paths = list(directory.rglob('*'))
    require(not any(p.is_symlink() for p in all_paths), 'sealed population contains symlink')
    actual = {p.relative_to(directory).as_posix() for p in all_paths if p.is_file() and p != directory / 'artifact-sha256.json'}
    require(actual == set(index), 'extra/missing sealed artifacts')
    for rel, expected in index.items():
        path = PurePosixPath(rel)
        require(not path.is_absolute() and '..' not in path.parts and str(path) == rel, 'unsafe artifact path')
        require(sha(directory / rel) == digest(expected), 'sealed artifact changed: ' + rel)
    status = read_json(directory / 'status.json')
    require(status['epoch'] == rows[0]['epoch'] and status['complete'] is True and status['input_hashes_verified_after'] is True and
            status['completed'] == [row['id'] for row in rows], 'incomplete population status')
    receipts = []
    for row in rows:
        run = directory / 'runs' / row['id']
        required = ['manifest.json', 'summary.json', 'samples.csv', 'warmup.csv']
        if row.get('profile', 'none') in PROFILE_KINDS:
            required += [row['profile'] + '.pprof']
            require(not (run / (('block' if row['profile'] == 'mutex' else 'mutex') + '.pprof')).exists(), 'unexpected opposite profile artifact')
        for name in required:
            require((run / name).relative_to(directory).as_posix() in index, 'missing required run artifact: ' + name)
        receipt_rel = 'runs/' + row['id'] + '.command.json'
        require(receipt_rel in index, 'missing command receipt')
        receipt = read_json(directory / receipt_rel)
        require(receipt['schedule'] == row and receipt['returncode'] == 0 and receipt['env'] == collector.ENV and
                type(receipt['duration_ns']) is int and receipt['duration_ns'] > 0 and
                0 < receipt['timeout_seconds'] <= collector.CHILD_LIMIT, 'command receipt mismatch')
        receipts.append(receipt)
    return receipts


def timestamp(value):
    stamp = dt.datetime.fromisoformat(value)
    require(stamp.tzinfo is not None, 'timezone required in receipt')
    return stamp


def require_posttiming(timing_receipts, diagnostic_receipts, expected_timing=256):
    require(type(expected_timing) is int and expected_timing in (256, 176), 'unsupported timing receipt population')
    require(len(timing_receipts) == expected_timing and len(diagnostic_receipts) == 16, 'complete post-timing receipt populations required')
    for receipt in timing_receipts + diagnostic_receipts:
        require(timestamp(receipt['started_utc']) < timestamp(receipt['finished_utc']), 'nonpositive receipt chronology')
    require(max(timestamp(r['finished_utc']) for r in timing_receipts) <= min(timestamp(r['started_utc']) for r in diagnostic_receipts),
            'diagnostic process preceded completion of timing population')


def varint(value):
    require(type(value) is int and value >= 0, 'nonnegative protobuf integer required')
    result = bytearray()
    while value > 127:
        result.append((value & 127) | 128)
        value >>= 7
    result.append(value)
    return bytes(result)


def proto_int(tag, value):
    return varint(tag << 3) + varint(value)


def proto_bytes(tag, value):
    return varint((tag << 3) | 2) + varint(len(value)) + value


def positive_fixture(source, profile):
    """Minimal pprof protobuf; a decoder/classifier control, not benchmark data."""
    operation = 'Unlock' if profile == 'mutex' else 'Lock'
    chosen = []
    for family, origin in (('shard', 'caller'), ('shard', 'drainer'), ('root', 'drainer')):
        candidates = [site for site in source.sites.values() if site.family == family and site.operation == operation and
                      (site.method in CALLER if origin == 'caller' else site.method in DRAINER)]
        require(bool(candidates), 'positive control source site absent')
        chosen.append(sorted(candidates, key=lambda site: site.line)[0])
    strings = ['', 'contentions', 'count', 'delay', 'nanoseconds', QUEUE_FILE]
    functions, locations, samples = [], [], []
    expected = {}
    for index, site in enumerate(chosen, 1):
        function = PACKAGE + '(*' + site.method[0] + ').' + site.method[1]
        strings.append(function)
        functions.append(proto_int(1, index) + proto_int(2, len(strings) - 1) + proto_int(3, len(strings) - 1) +
                         proto_int(4, 5) + proto_int(5, source.methods[site.method][0]))
        locations.append(proto_int(1, index) + proto_bytes(4, proto_int(1, index) + proto_int(2, site.line)))
        delay = 10 + index
        samples.append(proto_int(1, index) + proto_int(2, index) + proto_int(2, delay))
        label = sample_classification([Frame(function, QUEUE_FILE, site.line, source.methods[site.method][0])], source, profile)
        require(label is not None, 'positive control failed source classifier')
        expected[(label[0], label[1])] = dict(delay_ns=delay, contentions=index, sample_rows=1)
    value_types = [proto_int(1, 1) + proto_int(2, 2), proto_int(1, 3) + proto_int(2, 4)]
    message = b''.join(proto_bytes(tag, item) for tag, items in ((1, value_types), (2, samples), (4, locations), (5, functions)) for item in items)
    message += b''.join(proto_bytes(6, value.encode()) for value in strings)
    message += proto_int(14, 3)
    return gzip.compress(message, mtime=0), expected


def run_raw(pprof, expected_pprof_sha256, binary, expected_binary_sha256, profile, source, output):
    require(sha(pprof) == digest(expected_pprof_sha256), 'prebuilt pprof changed')
    require(sha(binary) == digest(expected_binary_sha256), 'bound executable changed')
    require(profile.is_file() and not profile.is_symlink() and profile.stat().st_size > 0, 'missing/empty profile')
    require(not output.exists(), 'refuse raw-output replacement')
    output.mkdir(parents=True)
    # Go profiles embed source frames. Do not consult the mutable checkout or a
    # symbol server. Exact frame/line binding is against the frozen archive below.
    command = [str(pprof), '-raw', '-symbolize=none', str(binary), str(profile)]
    before = sha(profile)
    started = time.monotonic_ns()
    with (output / 'stdout.raw').open('xb') as stdout, (output / 'stderr.log').open('xb') as stderr:
        result = subprocess.run(command, stdin=subprocess.DEVNULL, stdout=stdout, stderr=stderr,
                                timeout=60, env=dict(os.environ, PPROF_TMPDIR=str(output)))
    save(output / 'command.json', dict(argv=command, returncode=result.returncode, duration_ns=time.monotonic_ns() - started,
         pprof_sha256=expected_pprof_sha256, binary_sha256=expected_binary_sha256, profile_sha256=before,
         source_archive_sha256=source.archive_sha256, queue_source_sha256=source.source_sha256))
    require(result.returncode == 0, 'pprof -raw failed; see retained stderr.log')
    require(sha(profile) == before and sha(pprof) == expected_pprof_sha256 and sha(binary) == expected_binary_sha256, 'raw-input drift')
    return (output / 'stdout.raw').read_text()


def analyze(args):
    evidence = args.evidence.resolve()
    require(sha(evidence / 'frozen.json') == digest(args.frozen_sha256), 'frozen manifest changed')
    frozen = read_json(evidence / 'frozen.json')
    helper = args.collector_helper.resolve()
    require(str(helper) in frozen['hashes'] and sha(helper) == digest(frozen['hashes'][str(helper)]), 'collector helper not frozen')
    require(str(Path(__file__).resolve()) in frozen['hashes'] and sha(__file__) == frozen['hashes'][str(Path(__file__).resolve())], 'queuewait analyzer not frozen')
    collector = load_module(helper, 'queuewait_frozen_collector')
    design = collector.timing_design(frozen)
    require(frozen['seed'] == collector.timing_seed(design), 'wrong diagnostic design seed')
    collector.verify_inputs(evidence, frozen)
    collector.helper_chain(evidence / 'inputs', frozen['helper_hashes'])
    pprof = args.pprof_binary.resolve()
    require(digest(args.pprof_sha256) == frozen['pprof_sha256'] and str(pprof) in frozen['hashes'] and
            sha(pprof) == frozen['hashes'][str(pprof)] == args.pprof_sha256, 'explicit pprof pin not frozen')
    rows = read_json(evidence / 'schedule-diagnostics.json')
    require(rows == collector.diagnostic_schedule(frozen['identities'], frozen['prefix_namespace']) and len(rows) == 16, 'noncanonical 16-process diagnostic schedule')
    counts = Counter((r['workload'], r['binary'], r['profile']) for r in rows)
    require(counts == Counter({(w, role, profile): 1 for w in WORKLOADS for role in ('baseline', 'production') for profile in PROFILE_KINDS}), 'incomplete diagnostic factorial')
    require(all(r['workers'] == 32 and r['iterations'] == 500000 and r['warmup'] == 2000 for r in rows), 'diagnostic workload budget changed')
    timing = read_json(evidence / 'schedule.json')
    collector.validate_timing_schedule(timing, frozen['identities'], design)
    require(timing == collector.timing_schedule(frozen['identities'], frozen['prefix_namespace'], design), 'noncanonical timing schedule')
    timing_receipts = []
    for epoch in (1, 2):
        timing_receipts += checked_population(evidence, evidence / f'epoch{epoch}', getattr(args, f'seal{epoch}'),
                                               [r for r in timing if r['epoch'] == epoch], collector)
    diagnostic_receipts = checked_population(evidence, evidence / 'diagnostics', args.seal, rows, collector)
    require_posttiming(timing_receipts, diagnostic_receipts, collector.timing_count(design))
    confirmation_receipt = None
    if design == 'a2-grv-w8':
        confirmation_receipt = collector.verify_a2_confirmation_admission(evidence, frozen, args.frozen_sha256,
            {'1': args.seal1, '2': args.seal2}, args.seal)
    sources = {}
    for role in ('baseline', 'production'):
        source = frozen['source_archives'][role]
        require(source['source'] == frozen['identities'][role]['source'], 'source archive role mismatch')
        require(str(Path(source['path']).resolve()) in frozen['hashes'] and frozen['hashes'][str(Path(source['path']).resolve())] == source['sha256'], 'source archive not frozen')
        sources[role] = SourceBinding.archive(Path(source['path']), source['sha256'], role)
    require(sources['production'].source_sha256 == frozen['source_tree']['files'][QUEUE_FILE]['sha256'],
            'production queue source does not match sealed input tree')
    # Validate every run before decoding or producing any diagnostic decision.
    validated = []
    for row in rows:
        run = evidence / 'diagnostics' / 'runs' / row['id']
        manifest, summary = read_json(run / 'manifest.json'), read_json(run / 'summary.json')
        collector.check_manifest(manifest, row, run, frozen)
        collector.check_summary(summary, row, manifest)
        collector.validate_lifecycle(summary, frozen, evidence)
        collector.csv_integrity(run / 'samples.csv', row['iterations'], row['workers'], 'measure')
        collector.csv_integrity(run / 'warmup.csv', row['warmup'], row['workers'], 'warmup')
        client_counters(summary)
        cpu = measured_cpu_ns(summary)
        require(collector.endpoint(summary)['window_cpu_us_per_tx'] == cpu / 1000 / summary['completed'], 'CPU denominator/helper mismatch')
        validated.append((row, run, cpu))
    output = args.output.resolve()
    require(collector.durable(output, Path(frozen['source_root'])) and not output.is_relative_to(evidence) and
            not evidence.is_relative_to(output), 'output must be durable/off-tree and separate from evidence')
    output.mkdir(parents=True, exist_ok=False)
    control_reports = {}
    for profile in PROFILE_KINDS:
        fixture, expected = positive_fixture(sources['production'], profile)
        path = output / ('positive-' + profile + '.pprof')
        path.write_bytes(fixture)
        raw = run_raw(pprof, args.pprof_sha256, evidence / 'bin' / 'production', frozen['identities']['production']['binary_sha256'],
                      path, sources['production'], output / ('positive-' + profile))
        report = summarize(raw, sources['production'], profile, 1000000)
        for (family, origin), values in expected.items():
            require(report['by_lock_and_origin'][family][origin] == values, 'pprof decoder positive-control mismatch')
        require(report['queue_sample_rows'] == 3 and report['queue_delay_ns'] == 36 and report['queue_contentions'] == 6,
                'pprof decoder positive-control population mismatch')
        control_reports[profile] = report
    results = []
    for row, run, cpu in validated:
        raw = run_raw(pprof, args.pprof_sha256, evidence / 'bin' / row['binary'], row['binary_sha256'],
                      run / (row['profile'] + '.pprof'), sources[row['binary']], output / row['id'])
        report = summarize(raw, sources[row['binary']], row['profile'], cpu)
        results.append(dict(row, **report))
    decision = diagnostic_decision(results)
    verdict = decision['verdict']
    result = dict(verdict=verdict, decision=decision, diagnostic_only=True, timing_design=design, timing_processes=collector.timing_count(design),
                  processes=16, measured_transactions=8000000, warmup_transactions=32000,
                  confirmation_results_sha256=confirmation_receipt['results_sha256'] if confirmation_receipt else None,
                  rule=decision['rule'],
                  denominator='same profiled process (stats_after-stats_before).(process_user_cpu_us+process_system_cpu_us)*1000',
                  mutex_and_block_must_not_be_added=True, positive_controls=control_reports, runs=results,
                  source_binding={role: dict(archive_sha256=s.archive_sha256, queue_source_sha256=s.source_sha256,
                      queue_absent=s.text is None, exact_lock_sites=[dict(line=x.line, family=x.family, operation=x.operation, method=list(x.method)) for x in s.sites.values()]) for role, s in sources.items()},
                  caveats=['Queue delay is aggregate sampled goroutine wait, not CPU consumed.',
                           'Mutex delay is attributed to the unlocking holder context; block delay to the blocked waiter context. Never add them or treat them as matched events.',
                           'Profile enable/disable brackets the measured phase slightly more broadly than process CPU snapshots.',
                           'Baseline without queue source may have zero queue delay only after nonempty profile and decoder controls.',
                           'Caller/drainer origin fields mean holder context for mutex and waiter context for block. Mutex caller_cleanup_remove does not mean cleanup itself waited. Unknown queue identity/source/origin is an error.',
                           'This diagnostic does not change the separate unprofiled CPU NI decision or authorize a merge.'])
    collector.verify_inputs(evidence, frozen)
    require(sha(evidence / 'frozen.json') == args.frozen_sha256, 'frozen manifest changed during analysis')
    for epoch in (1, 2):
        checked_population(evidence, evidence / f'epoch{epoch}', getattr(args, f'seal{epoch}'), [r for r in timing if r['epoch'] == epoch], collector)
    checked_population(evidence, evidence / 'diagnostics', args.seal, rows, collector)
    save(output / 'results.json', result)
    save(output / 'analysis-inputs.json', dict(frozen_sha256=args.frozen_sha256, timing_seals=[args.seal1, args.seal2], diagnostic_seal=args.seal,
         collector_sha256=sha(helper), analyzer_sha256=sha(__file__), pprof_sha256=args.pprof_sha256, python=sys.version))
    save(output / 'artifact-sha256.json', {p.relative_to(output).as_posix(): sha(p) for p in sorted(output.rglob('*')) if p.is_file()})
    print('QUEUEWAIT_DIAGNOSTIC', verdict, '16/16 profiles; 8 production gates strict <1%; baseline context; mutex/block separate')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for key in ('evidence', 'output', 'collector-helper', 'pprof-binary'):
        parser.add_argument('--' + key, type=Path, required=True)
    for key in ('frozen-sha256', 'pprof-sha256', 'seal1', 'seal2', 'seal'):
        parser.add_argument('--' + key, required=True)
    args = parser.parse_args()
    require(os.getpriority(os.PRIO_PROCESS, 0) == 19 and set(os.sched_getaffinity(0)) == CPUS, 'analysis needs nice19/reserved affinity')
    require(sha(args.evidence / 'frozen.json') == digest(args.frozen_sha256), 'frozen manifest changed')
    frozen = read_json(args.evidence / 'frozen.json')
    # The collection lock is also the reserved fast lane lock; never bypass it.
    with Path(frozen['lock']).open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        analyze(args)


if __name__ == '__main__':
    main()
