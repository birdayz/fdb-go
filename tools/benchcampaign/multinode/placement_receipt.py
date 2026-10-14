#!/usr/bin/env python3
"""Prove (or refute) that index entries and their records live on different storage servers.

Inputs (all produced outside timing):
  --status   `fdbcli --exec 'status json'`: address -> process class, machine
             locality, roles; only processes holding a `storage` role count.
  --fixture-receipt  the benchmark's own untimed fixture receipt for the store
             (the FDB_BENCH_AUDIT fixture JSON, or the H4 SQL prefix receipt):
             {"prefix_bytes": N, "rows": 1000, "chunks": 1|2,
              "store_prefix_hex": "..." (required for --layout opaque)}.
             It ties the probed prefix to the store the benchmark actually owns.
  --cluster-file  the pinned cluster file; its SHA-256 and description:id must
             equal the probe's recorded cluster identity.
  --probe    a placement probe document written by placementprobe (H5,
             placementprobe_cgo: one libfdb_c transaction at V1 for keys,
             \xff/keyServers, DD mode, moveKeysLock and transactional
             LocalityGetAddressesForKey; recheck at V2 >= V1):
               {"format": "fdb-bench-placement-probe/v3", "store": "benchfetch/small",
                "store_prefix_hex": "...", "read_version": 123,
                "boundaries_read_version": 123,
                "boundary_range_hex": [prefix, strinc(prefix)],
                "boundaries_complete": true,
                "boundaries_hex": ["..."],            # strictly increasing, inside the range
                "scanned_subspaces_hex": [prefix+(1,), prefix+(2,)],
                "keys_complete": true,                 # EVERY physical key of both subspaces
                "keys": [{"kind": "index"|"record", "id": 0, "key_hex": "...",
                          "addresses": ["10.88.0.12:4502", "10.88.0.13:4503"]}],
                "fixture_receipt_sha256": "...", "cluster": {"connection_string_id", "cluster_file_sha256"},
                "addresses_basis": {"kind": "transactional", "read_version": V1},
                "recheck": {"read_version": V2, "boundaries_identical": true},
                "dd_mode": {"required_off": true, "at_v1": {...mode 0...}, "at_v2": {...}},
                "move_keys_lock": {owner hex at V1/V2 (any value, identical), write token hex
                                   at V1/V2 (identical)},
                "key_servers": {"begin_hex", "end_hex", "entries", "sha256_at_v1", "sha256_at_v2",
                                "unchanged"}}

Probe scope. The probe lists every physical key of exactly two data subspaces of
the store: the record subspace prefix+(1,) and the index subspace prefix+(2,),
each read completely. Store metadata is OUT of scope by design and must not be
listed: the store header prefix+(0,), index state/range/secondary/uniqueness
subspaces prefix+(3..10,) and any other non-data key; a listed key outside the
two data subspaces is an integrity error, never silently accepted as canonical
input. Placement ballast (record primary keys >= 1,000,000) must have been
cleared and verified absent before the probe; any leftover ballast key is a
non-canonical key and fails integrity.

receipt: two separate verdicts.
  integrity_ok (input is trustworthy; no corruption):
  * positive integer read_version, equal boundaries_read_version;
  * complete boundaries over exactly [prefix, strinc(prefix)), strictly increasing;
  * scanned_subspaces exactly the two data subspaces and keys_complete true;
  * every physical key appears once;
  * --layout recordlayer: the probe's key set equals the canonical fixture key set
    exactly: index key prefix+(2, index, id, id), record keys prefix+(1, id, s) for
    the canonical suffix set: unsplit {0}, split {1, 2}; no version (-1) keys,
    no stray suffix, no omitted chunk, no other IDs (ballast included);
  * --layout opaque: index IDs and record IDs are both exactly 0..rows-1, one
    index entry per ID (and --records-per-id record keys when given);
  * every address a storage role in status json, exactly --replicas per key, one
    server set per shard; the owned fixture receipt matches;
  * provenance: addresses_basis transactional at V1 (location-cache/diagnostic
    documents FAIL integrity); recheck V2 >= V1 with identical boundaries; DD
    required off, mode 0 with the key present at V1 and V2; moveKeysLock owner
    present and identical at V1 and V2 (any value: a DD restart under mode 0
    leaves a random owner) and the write token present and identical;
    keyServers digest unchanged; the probe's fixture_receipt_sha256 equals the
    --fixture-receipt file's; its cluster identity equals --cluster-file's.
  The receipt preserves the provenance (`provenance`, `cluster`,
  `key_servers_sha256`, `move_keys_write_token_hex`, `addresses_basis`);
  sealed_errors() re-validates it for sealing and fresh case checks.
  Shards holding both index entries and records are legitimate colocation and are
  reported as `shared_shards` (descriptive), not as an integrity error.
  passed (full-disjointness acceptance): integrity_ok and every cell complete with
  server-disjoint fraction >= --min-disjoint-fraction (default 1.0).
  Per cell (the IDs whose index entries the cell scans): rows whose index-entry
  servers and record-chunk servers are disjoint, and, when integrity_ok, the label
  'cross-server' (fraction 1.0) or 'partially colocated (f = x.xxxx)'. "Server"
  means the storage process (address): with double replication on three machines
  every two-machine team meets every other, so machine-level disjointness is
  structurally 0 and is reported only as a descriptive figure.
compare:  both receipts integrity_ok and the same store, prefix, boundaries, cluster,
          keyServers digest, moveKeysLock owner and write token and per-key server sets
          (partial placements compare fine; any DD toggle since the other receipt
          changes the write token).
controls: mutation controls on a real probe: the original must be integrity_ok
          (its disjointness fraction is irrelevant) and each mutation (duplicate
          key, omitted chunk, stray suffix, version key, truncated boundaries,
          incomplete key scan, zero read version, foreign address) and each
          provenance mutation (diagnostic addresses, DD on/not required, lock
          owner/token changed, keyServers changed, early recheck, fixture or
          cluster mismatch, any provenance field missing) must be rejected BY
          INTEGRITY.
Exit status: receipt 0 passed, 3 integrity_ok but some cell below the fraction
(usable only under a partially-colocated label), 1 integrity failed;
compare/controls 0 ok, 1 not; 2 unusable input.
"""
import argparse
import bisect
import copy
import hashlib
import json
from pathlib import Path
import re
import sys

import mn_common as mc

PROBE_FORMAT = 'fdb-bench-placement-probe/v3'
RECEIPT_FORMAT = 'fdb-bench-placement-receipt/v3'
ADDRESS_TRANSACTIONAL = 'transactional'
# dataDistributionModeLock = UID(6345, 3425) as two little-endian uint64 (7.3.77 SystemData.cpp): the
# owner setDDMode writes. Informational: DataDistributor::init's takeMoveKeysLock replaces it with a
# random owner on every DD (re)start, so integrity requires a STABLE owner, not this value.
DD_MODE_LOCK_HEX = 'c918000000000000610d000000000000'
KEY_SERVERS_PREFIX = b'\xff/keyServers/'
PROVENANCE_FIELDS = ('fixture_receipt_sha256', 'cluster', 'addresses_basis', 'recheck', 'dd_mode', 'move_keys_lock',
                     'key_servers')
RECORD_KEY, INDEX_KEY = 1, 2
# Canonical record-key suffixes of a new store (format >= SAVE_UNSPLIT_WITH_SUFFIX,
# no store record versions): Java SplitHelper UNSPLIT_RECORD 0, START_SPLIT_RECORD 1.
RECORD_SUFFIXES = {'unsplit': (0,), 'split': (1, 2)}

# Cells: name -> [lo, hi) of IDs whose index entries the cell's index scan reads.
# rows100 cells do not exist in the frozen harness; they are the proposed
# harness addition ([100,200), and [100,203) for Skip 3).
PRESETS = {
    'benchfetch': {'limit1-forward': (0, 1), 'limit1-reverse': (999, 1000), 'rows10': (100, 110),
                   'rows100': (100, 200), 'rows1000': (0, 1000)},
    'planskip': {'skip3-rows10': (100, 113), 'skip3-rows100': (100, 203), 'skip3-rows1000': (0, 1000)},
    'sql': {'rows10': (100, 110), 'rows100': (100, 200), 'rows1000': (0, 1000)},
}


class InputError(ValueError):
    pass


# --- FoundationDB tuple encoding (the subset the fixtures use) -------------

def _escape(b):
    return b.replace(b'\x00', b'\x00\xff')


def pack(items):
    out = b''
    for item in items:
        if item is None:
            out += b'\x00'
        elif isinstance(item, bytes):
            out += b'\x01' + _escape(item) + b'\x00'
        elif isinstance(item, str):
            out += b'\x02' + _escape(item.encode()) + b'\x00'
        elif isinstance(item, int) and not isinstance(item, bool):
            if item == 0:
                out += b'\x14'
            elif item > 0:
                n = (item.bit_length() + 7) // 8
                if n > 8:
                    raise InputError('integer too large for this encoder')
                out += bytes([0x14 + n]) + item.to_bytes(n, 'big')
            else:
                n = ((-item).bit_length() + 7) // 8
                if n > 8:
                    raise InputError('integer too large for this encoder')
                out += bytes([0x14 - n]) + (item + (1 << (8 * n)) - 1).to_bytes(n, 'big')
        else:
            raise InputError(f'unsupported tuple element {item!r}')
    return out


def strinc(key):
    """First key after every key with this prefix (FDB strinc)."""
    stripped = key.rstrip(b'\xff')
    if not stripped:
        raise InputError('prefix of only 0xff bytes has no strinc')
    return stripped[:-1] + bytes([stripped[-1] + 1])


# --- inputs ----------------------------------------------------------------

def _addr(a):
    return a[:-4] if a.endswith(':tls') else a


def storage_map(status):
    out = {}
    for p in ((status.get('cluster') or {}).get('processes') or {}).values():
        roles = {r.get('role') for r in p.get('roles') or []}
        if 'storage' in roles:
            loc = p.get('locality') or {}
            out[_addr(p['address'])] = dict(machine=loc.get('machineid'), zone=loc.get('zoneid'),
                                            process_class=p.get('class_type'))
    if not out:
        raise InputError('status json lists no storage servers')
    return out


def load_probe(probe):
    """Parse without judging: returns prefix, raw boundaries (as given), keys (as given)."""
    if probe.get('format') != PROBE_FORMAT:
        raise InputError('probe format must be ' + PROBE_FORMAT)
    try:
        prefix = bytes.fromhex(probe['store_prefix_hex'])
        boundaries = [bytes.fromhex(b) for b in probe['boundaries_hex']]
        brange = [bytes.fromhex(b) for b in probe['boundary_range_hex']]
        keys = [dict(kind=k['kind'], id=k['id'], key=bytes.fromhex(k['key_hex']),
                     addresses=sorted({_addr(a) for a in k['addresses']})) for k in probe['keys']]
    except (KeyError, TypeError, ValueError, AttributeError) as exc:
        raise InputError(f'malformed probe: {exc}') from exc
    if not prefix or not keys:
        raise InputError('probe needs a store prefix and keys')
    return prefix, boundaries, brange, keys


def _positive_int(v):
    return isinstance(v, int) and not isinstance(v, bool) and v > 0


def probe_integrity(probe, prefix, boundaries, brange, errors):
    rv = probe.get('read_version')
    if not _positive_int(rv):
        errors.append(f'read_version {rv!r} is not a positive integer')
    if probe.get('boundaries_read_version') != rv:
        errors.append('boundaries were not read at the probe read version')
    if probe.get('boundaries_complete') is not True:
        errors.append('boundaries_complete is not true: the boundary list may be truncated')
    if brange != [prefix, strinc(prefix)]:
        errors.append('boundary_range_hex is not exactly [prefix, strinc(prefix))')
    if any(a >= b for a, b in zip(boundaries, boundaries[1:])):
        errors.append('boundaries are not strictly increasing')
    if any(not (prefix <= b < strinc(prefix)) for b in boundaries):
        errors.append('a boundary lies outside the store range')
    want = [(prefix + pack([RECORD_KEY])).hex(), (prefix + pack([INDEX_KEY])).hex()]
    if probe.get('scanned_subspaces_hex') != want:
        errors.append('scanned_subspaces_hex is not exactly [record subspace, index subspace]')
    if probe.get('keys_complete') is not True:
        errors.append('keys_complete is not true: the key scan may be partial')


def cluster_identity(data):
    """{connection_string_id, cluster_file_sha256} of cluster file bytes (as placementprobe computes it)."""
    for line in data.decode().splitlines():
        line = line.strip()
        if not line or line.startswith('#'):
            continue
        at, colon = line.find('@'), line.find(':')
        if at <= 0 or colon <= 0 or colon > at:
            raise InputError(f'malformed connection string {line!r}')
        return dict(connection_string_id=line[:at], cluster_file_sha256=hashlib.sha256(data).hexdigest())
    raise InputError('cluster file has no connection string')


def _hex(v, nbytes):
    return isinstance(v, str) and re.fullmatch(f'[0-9a-f]{{{2 * nbytes}}}', v) is not None


def provenance_errors(probe, prefix, fixture_sha256, expect_cluster):
    """Integrity of how the probe was taken: transactional addresses at V1, DD off
    (mode 0, key present) at V1 and V2, a present and unchanged moveKeysLock owner
    (any UID) and write token, an unchanged keyServers digest, the owned fixture
    receipt and the pinned cluster. Any failure means the document proves nothing.

    Why owner + token + mode + keyServers identity over [V1, V2] rules out an
    intervening relocation (7.3.77 source): setDDMode / DataDistributionImpl write
    mode, owner and a NEW token on every toggle; takeMoveKeysLock (run by every
    DD (re)start before it reads the mode) writes ONLY a new random owner, the
    token unchanged, so the owner-identity check is what catches it; the
    relocation paths (startMoveKeys, finishMoveKeys, startMoveShards,
    finishMoveShards, removeStorageServer, removeKeysFromFailedServer,
    cleanUpDataMove*) call checkMoveKeysLock in their transaction, which writes a
    NEW token. cleanUpSingleShardDataMove writes keyServers without that check
    (only with SHARD_ENCODE_LOCATION_METADATA, off by default); the raw
    keyServers comparison catches it and any other shard-map write. So no toggle,
    DD restart or shard-map commit can fall between V1 and V2 without changing a
    compared value."""
    errors = []
    rv = probe.get('read_version')
    basis = probe.get('addresses_basis') or {}
    if basis.get('kind') != ADDRESS_TRANSACTIONAL:
        errors.append(f'addresses_basis kind {basis.get("kind")!r} is not transactional '
                      '(location-cache/diagnostic addresses are not admissible)')
    if basis.get('read_version') != rv:
        errors.append('addresses were not read at the probe read version')
    recheck = probe.get('recheck') or {}
    v2 = recheck.get('read_version')
    if not (_positive_int(rv) and _positive_int(v2) and v2 >= rv):
        errors.append(f'recheck read version {v2!r} missing or before read version {rv!r}')
    if recheck.get('boundaries_identical') is not True:
        errors.append('boundaries were not rechecked identical at the recheck version')
    dd = probe.get('dd_mode') or {}
    if dd.get('required_off') is not True:
        errors.append('probe did not require data distribution off (diagnostic)')
    for which, version in (('at_v1', rv), ('at_v2', v2)):
        m = dd.get(which) or {}
        if m.get('mode') != 0 or m.get('key_present') is not True or m.get('read_version') != version:
            errors.append(f'data distribution not off (mode 0, key present) {which}')
    lock = probe.get('move_keys_lock') or {}
    o1, o2 = lock.get('owner_hex_at_v1'), lock.get('owner_hex_at_v2')
    if not (_hex(o1, 16) and o1 == o2) or lock.get('owner_unchanged') is not True:
        errors.append('moveKeysLock owner missing or changed between V1 and V2 (a data distributor took the lock)')
    w1, w2 = lock.get('write_token_hex_at_v1'), lock.get('write_token_hex_at_v2')
    if not (_hex(w1, 16) and w1 == w2) or lock.get('write_token_unchanged') is not True:
        errors.append('moveKeysLock write token missing or changed between V1 and V2')
    ks = probe.get('key_servers') or {}
    if (ks.get('begin_hex') != (KEY_SERVERS_PREFIX + prefix).hex() or
            ks.get('end_hex') != (KEY_SERVERS_PREFIX + strinc(prefix)).hex()):
        errors.append('key_servers range is not the store range')
    d1, d2 = ks.get('sha256_at_v1'), ks.get('sha256_at_v2')
    if not (_hex(d1, 32) and d1 == d2) or ks.get('unchanged') is not True or not _positive_int(ks.get('entries')):
        errors.append('keyServers digest missing or changed between V1 and V2')
    if fixture_sha256 is None:
        errors.append('fixture receipt sha256 not supplied for verification')
    elif probe.get('fixture_receipt_sha256') != fixture_sha256:
        errors.append('probe fixture_receipt_sha256 differs from the fixture receipt file')
    cluster = probe.get('cluster') or {}
    if expect_cluster is None:
        errors.append('cluster identity not supplied for verification')
    elif (cluster.get('connection_string_id') != expect_cluster.get('connection_string_id') or
          cluster.get('cluster_file_sha256') != expect_cluster.get('cluster_file_sha256')):
        errors.append('probe cluster identity differs from the pinned cluster file')
    return errors


def synthetic_provenance(prefix, read_version=7, recheck_read_version=8, fixture_sha256='a' * 64, cluster=None,
                         key_servers_sha256='b' * 64, write_token_hex='00' * 16, owner_hex=DD_MODE_LOCK_HEX):
    """Valid provenance fields for SYNTHETIC test probes (never produced on a cluster)."""
    return dict(
        fixture_receipt_sha256=fixture_sha256,
        cluster=cluster or dict(connection_string_id='fdbbenchvpc:mn20261014', cluster_file_sha256='c' * 64),
        addresses_basis=dict(kind=ADDRESS_TRANSACTIONAL, read_version=read_version, method='synthetic'),
        recheck=dict(read_version=recheck_read_version, boundaries_identical=True),
        dd_mode=dict(required_off=True, at_v1=dict(read_version=read_version, mode=0, key_present=True),
                     at_v2=dict(read_version=recheck_read_version, mode=0, key_present=True)),
        move_keys_lock=dict(owner_hex_at_v1=owner_hex, owner_hex_at_v2=owner_hex, owner_unchanged=True,
                            write_token_hex_at_v1=write_token_hex, write_token_hex_at_v2=write_token_hex,
                            write_token_unchanged=True),
        key_servers=dict(begin_hex=(KEY_SERVERS_PREFIX + prefix).hex(), end_hex=(KEY_SERVERS_PREFIX + strinc(prefix)).hex(),
                         entries=3, sha256_at_v1=key_servers_sha256, sha256_at_v2=key_servers_sha256, unchanged=True,
                         description='synthetic'))


def fixture_integrity(fixture, prefix, layout, rows, record_format, errors):
    if not isinstance(fixture, dict):
        errors.append('missing owned fixture receipt')
        return
    if fixture.get('prefix_bytes') != len(prefix):
        errors.append(f'fixture receipt prefix_bytes {fixture.get("prefix_bytes")!r} != probe prefix length {len(prefix)}')
    if 'store_prefix_hex' in fixture and fixture['store_prefix_hex'] != prefix.hex():
        errors.append('fixture receipt store_prefix_hex differs from the probe prefix')
    if layout == 'opaque' and 'store_prefix_hex' not in fixture:
        errors.append('opaque layout needs the fixture receipt store_prefix_hex')
    if fixture.get('rows') != rows:
        errors.append(f'fixture receipt rows {fixture.get("rows")!r} != {rows}')
    if record_format and fixture.get('chunks') not in (None, len(RECORD_SUFFIXES[record_format])):
        errors.append(f'fixture receipt chunks {fixture.get("chunks")!r} contradict record format {record_format}')


def canonical_keys(prefix, rows, index_name, record_format):
    """{key: (kind, id)} for the exact fixture."""
    out = {}
    for i in range(rows):
        out[prefix + pack([INDEX_KEY, index_name, i, i])] = ('index', i)
        for s in RECORD_SUFFIXES[record_format]:
            out[prefix + pack([RECORD_KEY, i, s])] = ('record', i)
    return out


def _sample(items, n=3):
    items = sorted(items)
    return ', '.join(x.hex() if isinstance(x, bytes) else str(x) for x in items[:n]) + (' ...' if len(items) > n else '')


# --- receipt ---------------------------------------------------------------

def label(fraction):
    return 'cross-server' if fraction == 1.0 else f'partially colocated (f = {fraction:.4f})'


def build_receipt(status, probe, cells, layout='opaque', index_name='price_idx', rows=1000, record_format=None,
                  expect_prefix=None, fixture=None, replicas=2, min_fraction=1.0, records_per_id=None, preset=None,
                  fixture_sha256=None, expect_cluster=None):
    if not cells:
        raise InputError('no cells: a receipt must certify at least one cell')
    for name, (lo, hi) in cells.items():
        if not (isinstance(lo, int) and isinstance(hi, int) and 0 <= lo < hi):
            raise InputError(f'cell {name} has an empty or invalid ID range [{lo}, {hi})')
    servers = storage_map(status)
    prefix, boundaries, brange, keys = load_probe(probe)
    errors = []
    probe_integrity(probe, prefix, boundaries, brange, errors)
    errors += provenance_errors(probe, prefix, fixture_sha256, expect_cluster)
    if layout == 'recordlayer':
        if record_format not in RECORD_SUFFIXES:
            raise InputError('recordlayer layout needs record_format unsplit or split')
        if expect_prefix is None:
            raise InputError('recordlayer layout needs the expected store prefix')
    fixture_integrity(fixture, prefix, layout, rows, record_format, errors)
    if expect_prefix is not None and prefix != expect_prefix:
        errors.append(f'store prefix {prefix.hex()} != expected {expect_prefix.hex()}')
    seen = {}
    for k in keys:
        seen[k['key']] = seen.get(k['key'], 0) + 1
    dups = [k for k, n in seen.items() if n > 1]
    if dups:
        errors.append(f'{len(dups)} physical key(s) listed more than once: {_sample(dups)}')
    rec_prefix, idx_prefix = prefix + pack([RECORD_KEY]), prefix + pack([INDEX_KEY])
    for k in keys:
        if k['kind'] not in ('index', 'record'):
            errors.append(f'unknown key kind {k["kind"]!r}')
            continue
        if not isinstance(k['id'], int) or isinstance(k['id'], bool):
            errors.append(f'non-integer id {k["id"]!r}')
        if not k['key'].startswith(idx_prefix if k['kind'] == 'index' else rec_prefix):
            errors.append(f'{k["kind"]} key {k["key"].hex()} outside its store subspace')
        if len(k['addresses']) != replicas:
            errors.append(f'{k["key"].hex()} has {len(k["addresses"])} replicas, expected {replicas}')
        for a in k['addresses']:
            if a not in servers:
                errors.append(f'{a} serves {k["key"].hex()} but is not a storage server in status json')
    if layout == 'recordlayer':
        canon = canonical_keys(prefix, rows, index_name, record_format)
        got = {k['key']: (k['kind'], k['id']) for k in keys}
        missing = set(canon) - set(got)
        extra = set(got) - set(canon)
        mislabelled = [key for key in set(canon) & set(got) if canon[key] != got[key]]
        if missing:
            errors.append(f'{len(missing)} canonical fixture key(s) missing (e.g. omitted chunk): {_sample(missing)}')
        if extra:
            errors.append(f'{len(extra)} non-canonical key(s) (stray suffix, version key or foreign ID): {_sample(extra)}')
        if mislabelled:
            errors.append(f'{len(mislabelled)} key(s) with the wrong kind or id: {_sample(mislabelled)}')
    index, records = {}, {}
    for k in keys:
        if k['kind'] == 'index':
            index.setdefault(k['id'], []).append(k)
        elif k['kind'] == 'record':
            records.setdefault(k['id'], []).append(k)
    multi_index = [i for i, v in index.items() if len(v) != 1]
    if multi_index:
        errors.append(f'{len(multi_index)} id(s) with more than one index entry')
    if records_per_id is not None:
        wrong = [i for i, v in records.items() if len(v) != records_per_id]
        if wrong:
            errors.append(f'{len(wrong)} id(s) without exactly {records_per_id} record keys')
    if layout == 'opaque' and not (set(index) == set(records) == set(range(rows))):
        errors.append(f'ID population differs from 0..{rows - 1}: {len(index)} indexed IDs, {len(records)} recorded IDs')

    def shard(key):
        return bisect.bisect_right(boundaries, key)

    by_shard = {}
    for k in keys:
        by_shard.setdefault(shard(k['key']), set()).add(tuple(k['addresses']))
    for s, sets in sorted(by_shard.items()):
        if len(sets) > 1:
            errors.append(f'shard {s} reports {len(sets)} different server sets; probe not at one version')
    index_shards = {shard(k['key']) for v in index.values() for k in v}
    record_shards = {shard(r['key']) for v in records.values() for r in v}
    shared_shards = len(index_shards & record_shards)  # legitimate colocation: descriptive only
    integrity_ok = not errors

    def machines(addrs):
        return {servers[a]['machine'] for a in addrs if a in servers}

    cell_out = {}
    for name, (lo, hi) in sorted(cells.items()):
        ids = list(range(lo, hi))
        present = [i for i in ids if len(index.get(i, [])) == 1 and records.get(i)]
        s_dis = m_dis = 0
        idx_srv, rec_srv = set(), set()
        for i in present:
            ia = set(index[i][0]['addresses'])
            ra = {a for r in records[i] for a in r['addresses']}
            idx_srv |= ia
            rec_srv |= ra
            s_dis += not (ia & ra)
            m_dis += not (machines(ia) & machines(ra))
        n = len(ids)
        frac = s_dis / n
        cell_out[name] = dict(ids=[lo, hi], rows=n, missing_ids=n - len(present), server_disjoint_rows=s_dis,
                              server_disjoint_fraction=frac, machine_disjoint_rows=m_dis,
                              machine_disjoint_fraction=m_dis / n, index_servers=sorted(idx_srv),
                              record_servers=sorted(rec_srv),
                              index_shards=len({shard(index[i][0]['key']) for i in present}),
                              record_shards=len({shard(r['key']) for i in present for r in records[i]}),
                              label=label(frac) if integrity_ok and len(present) == n else None,
                              passed=integrity_ok and len(present) == n and frac >= min_fraction)
    passed = integrity_ok and all(c['passed'] for c in cell_out.values())
    return dict(format=RECEIPT_FORMAT, integrity_ok=integrity_ok, passed=passed, errors=errors[:200],
                error_count=len(errors), preset=preset, shared_shards=shared_shards,
                provenance={f: copy.deepcopy(probe.get(f)) for f in PROVENANCE_FIELDS},
                cluster=probe.get('cluster'), addresses_basis=(probe.get('addresses_basis') or {}).get('kind'),
                key_servers_sha256=(probe.get('key_servers') or {}).get('sha256_at_v1'),
                move_keys_write_token_hex=(probe.get('move_keys_lock') or {}).get('write_token_hex_at_v1'),
                move_keys_owner_hex=(probe.get('move_keys_lock') or {}).get('owner_hex_at_v1'),
                store=probe.get('store'), store_prefix_hex=prefix.hex(), read_version=probe.get('read_version'),
                layout=layout, record_format=record_format, replicas=replicas, min_disjoint_fraction=min_fraction,
                disjointness_unit='storage process (address); machine level is descriptive only',
                storage_servers=servers, boundaries_hex=[b.hex() for b in boundaries],
                index_shards=len(index_shards), record_shards=len(record_shards), cells=cell_out,
                placement={k['key'].hex(): k['addresses'] for k in keys})


def compare(a, b):
    diffs = []
    for field in ('format', 'store', 'store_prefix_hex', 'boundaries_hex', 'cluster', 'key_servers_sha256',
                  'move_keys_write_token_hex', 'move_keys_owner_hex'):
        if a.get(field) != b.get(field):
            diffs.append(field + ' differs')
    if not (a.get('integrity_ok') is True and b.get('integrity_ok') is True):
        diffs.append('a compared receipt is without integrity')
    pa, pb = a.get('placement') or {}, b.get('placement') or {}
    if set(pa) != set(pb):
        diffs.append(f'key sets differ ({len(set(pa) ^ set(pb))} keys)')
    moved = [k for k in set(pa) & set(pb) if pa[k] != pb[k]]
    if moved:
        diffs.append(f'{len(moved)} keys changed servers, e.g. {sorted(moved)[0]}')
    return dict(equal=not diffs, differences=diffs)


# --- mutation controls -------------------------------------------------------

def mutations(probe, layout, record_format):
    """Named copies of a real probe, each with one defect the receipt must reject."""
    keys = probe['keys']
    records = [k for k in keys if k['kind'] == 'record']
    first = records[0]
    prefix = bytes.fromhex(probe['store_prefix_hex'])
    out = {}

    def variant(name, fn):
        p = copy.deepcopy(probe)
        fn(p)
        out[name] = p

    variant('duplicate-key', lambda p: p['keys'].append(copy.deepcopy(first)))
    variant('zero-read-version', lambda p: p.update(read_version=0, boundaries_read_version=0))
    variant('truncated-boundaries', lambda p: p.update(boundaries_complete=False))
    variant('incomplete-key-scan', lambda p: p.update(keys_complete=False))
    variant('foreign-address', lambda p: p['keys'][0].update(addresses=['192.0.2.1:4500'] + p['keys'][0]['addresses'][1:]))
    if layout == 'recordlayer':
        last_suffix = RECORD_SUFFIXES[record_format][-1]
        omitted = prefix + pack([RECORD_KEY, first['id'], last_suffix])
        variant('omitted-chunk', lambda p: p.update(keys=[k for k in p['keys'] if k['key_hex'] != omitted.hex()]))
        for name, suffix in [('stray-suffix', last_suffix + 1), ('version-key', -1)]:
            stray = dict(copy.deepcopy(first), key_hex=(prefix + pack([RECORD_KEY, first['id'], suffix])).hex())
            variant(name, lambda p, s=stray: p['keys'].append(s))
    else:
        variant('omitted-record', lambda p: p.update(keys=[k for k in p['keys'] if k != first]))
    return out


def provenance_mutations(probe):
    """Named copies of a real probe, each with one provenance defect the receipt must reject."""
    out = {}

    def variant(name, fn):
        p = copy.deepcopy(probe)
        fn(p)
        out[name] = p

    def set_path(*path, value):
        def fn(p):
            d = p
            for k in path[:-1]:
                d = d.setdefault(k, {})
            d[path[-1]] = value
        return fn

    rv = probe.get('read_version')
    variant('diagnostic-addresses', set_path('addresses_basis', 'kind', value='location-cache (diagnostic)'))
    variant('addresses-at-other-version', set_path('addresses_basis', 'read_version', value=(rv or 0) + 1))
    variant('dd-not-required-off', set_path('dd_mode', 'required_off', value=False))
    variant('dd-on-at-v1', set_path('dd_mode', 'at_v1', 'mode', value=1))
    variant('dd-on-at-v2', set_path('dd_mode', 'at_v2', 'mode', value=1))
    variant('dd-key-absent-at-v1', set_path('dd_mode', 'at_v1', 'key_present', value=False))
    variant('recheck-before-v1', set_path('recheck', 'read_version', value=(rv or 0) - 1))
    variant('boundaries-not-rechecked', set_path('recheck', 'boundaries_identical', value=False))
    # Owner-only change V1 -> V2 (takeMoveKeysLock: token unchanged) must be rejected.
    variant('lock-owner-taken', set_path('move_keys_lock', 'owner_hex_at_v2', value='07' * 16))
    variant('lock-owner-missing', set_path('move_keys_lock', 'owner_hex_at_v1', value=None))
    variant('lock-owner-not-rechecked', set_path('move_keys_lock', 'owner_unchanged', value=False))
    variant('lock-token-changed', set_path('move_keys_lock', 'write_token_hex_at_v2', value='ff' * 16))
    variant('key-servers-changed', set_path('key_servers', 'sha256_at_v2', value='e' * 64))
    variant('fixture-sha-mismatch', set_path('fixture_receipt_sha256', value='d' * 64))
    variant('cluster-mismatch', set_path('cluster', 'connection_string_id', value='other:cluster'))
    for field in PROVENANCE_FIELDS:
        variant(f'missing-{field}', lambda p, f=field: p.pop(f, None))
    return out


def positive_controls(probe):
    """Variants that must KEEP integrity: a stable random moveKeysLock owner (what
    DataDistributor::init's takeMoveKeysLock leaves under mode 0) is not a defect."""
    p = copy.deepcopy(probe)
    lock = p.setdefault('move_keys_lock', {})
    lock.update(owner_hex_at_v1='9e3779b97f4a7c15632be59bd9b4e019', owner_hex_at_v2='9e3779b97f4a7c15632be59bd9b4e019')
    return {'stable-random-owner': p}


def run_controls(status, probe, cells, **kw):
    """Original judged by integrity only (a partial placement qualifies); every
    key mutation and every provenance mutation must fail integrity."""
    kw = dict(kw, min_fraction=0.0)
    original = build_receipt(status, probe, cells, **kw)
    report = dict(original_integrity_ok=original['integrity_ok'], original_errors=original['errors'], mutations={},
                  provenance_mutations={}, positive_controls={})
    for group, variants in (('mutations', mutations(probe, kw.get('layout', 'opaque'), kw.get('record_format'))),
                            ('provenance_mutations', provenance_mutations(probe))):
        for name, mutated in variants.items():
            r = build_receipt(status, mutated, cells, **kw)
            report[group][name] = dict(rejected=not r['integrity_ok'], errors=r['errors'][:3])
    for name, accepted in positive_controls(probe).items():
        r = build_receipt(status, accepted, cells, **kw)
        report['positive_controls'][name] = dict(accepted=r['integrity_ok'], errors=r['errors'][:3])
    report['ok'] = original['integrity_ok'] and all(
        m['rejected'] for group in ('mutations', 'provenance_mutations') for m in report[group].values()) and all(
        c['accepted'] for c in report['positive_controls'].values())
    return report


def sealed_errors(receipt, expect_cluster):
    """Why a receipt may not be used for sealing or a fresh case check ([] = usable).

    Re-validates the preserved provenance, so a receipt lacking any field, or
    built from a diagnostic probe, is refused even if its flags were edited."""
    if receipt.get('format') != RECEIPT_FORMAT:
        return [f'receipt format {receipt.get("format")!r} is not {RECEIPT_FORMAT}']
    errors = []
    if receipt.get('integrity_ok') is not True or receipt.get('error_count') != 0 or receipt.get('errors'):
        errors.append('receipt lacks integrity')
    prov = receipt.get('provenance')
    if not isinstance(prov, dict) or any(f not in prov or prov[f] is None for f in PROVENANCE_FIELDS):
        return errors + ['receipt lacks preserved provenance fields']
    try:
        prefix = bytes.fromhex(receipt.get('store_prefix_hex') or '')
    except ValueError:
        return errors + ['receipt store prefix is not hex']
    probe_like = dict(prov, read_version=receipt.get('read_version'))
    errors += provenance_errors(probe_like, prefix, prov.get('fixture_receipt_sha256'), expect_cluster)
    if not _hex(prov.get('fixture_receipt_sha256'), 32):
        errors.append('receipt lacks a fixture receipt sha256')
    for field, value in (('cluster', prov.get('cluster')), ('addresses_basis', ADDRESS_TRANSACTIONAL),
                         ('key_servers_sha256', prov['key_servers'].get('sha256_at_v1')),
                         ('move_keys_write_token_hex', prov['move_keys_lock'].get('write_token_hex_at_v1')),
                         ('move_keys_owner_hex', prov['move_keys_lock'].get('owner_hex_at_v1'))):
        if receipt.get(field) != value:
            errors.append(f'receipt {field} disagrees with its provenance')
    return errors


def sha(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest='command', required=True)
    for name in ('receipt', 'controls'):
        r = sub.add_parser(name)
        r.add_argument('--status', required=True)
        r.add_argument('--probe', required=True)
        r.add_argument('--fixture-receipt', required=True, help="the benchmark's own fixture/prefix receipt (JSON)")
        r.add_argument('--cluster-file', required=True, help='the pinned cluster file the probe must have used')
        r.add_argument('--preset', choices=sorted(PRESETS))
        r.add_argument('--cell', action='append', default=[], help='NAME=LO:HI (IDs [LO,HI)); repeat')
        r.add_argument('--layout', choices=['recordlayer', 'opaque'], default='opaque')
        r.add_argument('--record-format', choices=sorted(RECORD_SUFFIXES), help='recordlayer: unsplit {0} or split {1,2}')
        r.add_argument('--records-per-id', type=int, help='opaque: exact record keys per ID')
        r.add_argument('--index-name', default='price_idx')
        r.add_argument('--rows', type=int, default=1000)
        r.add_argument('--expect-prefix-tuple', help='comma-separated strings, e.g. benchfetch,small')
        r.add_argument('--replicas', type=int, default=2)
        r.add_argument('--min-disjoint-fraction', type=float, default=1.0)
        r.add_argument('--output', required=True, help='receipt/report path (exclusive create)')
    c = sub.add_parser('compare')
    c.add_argument('before')
    c.add_argument('after')
    args = ap.parse_args(argv)
    try:
        if args.command == 'compare':
            result = compare(json.loads(Path(args.before).read_text()), json.loads(Path(args.after).read_text()))
            print(json.dumps(result, sort_keys=True))
            return 0 if result['equal'] else 1
        cells = parse_cells(args.preset, args.cell)
        kw = dict(layout=args.layout, index_name=args.index_name, rows=args.rows, record_format=args.record_format,
                  expect_prefix=pack(args.expect_prefix_tuple.split(',')) if args.expect_prefix_tuple else None,
                  fixture=json.loads(Path(args.fixture_receipt).read_text()), replicas=args.replicas,
                  min_fraction=args.min_disjoint_fraction, records_per_id=args.records_per_id, preset=args.preset,
                  fixture_sha256=sha(args.fixture_receipt), expect_cluster=cluster_identity(Path(args.cluster_file).read_bytes()))
        status, probe = json.loads(Path(args.status).read_text()), json.loads(Path(args.probe).read_text())
        inputs = dict(status_sha256=sha(args.status), probe_sha256=sha(args.probe),
                      fixture_receipt_sha256=sha(args.fixture_receipt), cluster_file_sha256=sha(args.cluster_file))
        if args.command == 'controls':
            report = dict(run_controls(status, probe, cells, **kw), inputs=inputs)
            mc.dump_json(args.output, report)
            print(json.dumps({'ok': report['ok'], 'original_integrity_ok': report['original_integrity_ok'],
                              'rejected': {k: v['rejected'] for g in ('mutations', 'provenance_mutations')
                                           for k, v in report[g].items()}}, sort_keys=True))
            return 0 if report['ok'] else 1
        receipt = build_receipt(status, probe, cells, **kw)
        receipt['inputs'] = inputs
        mc.dump_json(args.output, receipt)
        print(json.dumps({k: receipt[k] for k in ('integrity_ok', 'passed', 'error_count', 'index_shards', 'record_shards',
                                                  'shared_shards')}, sort_keys=True))
        for name, cell in receipt['cells'].items():
            print(f'{name}: server-disjoint {cell["server_disjoint_rows"]}/{cell["rows"]}, '
                  f'machine-disjoint {cell["machine_disjoint_rows"]}/{cell["rows"]} (descriptive), '
                  f'label={cell["label"]}, passed={cell["passed"]}')
        return 0 if receipt['passed'] else (3 if receipt['integrity_ok'] else 1)
    except (InputError, ValueError, KeyError, OSError) as exc:
        print('UNUSABLE: ' + str(exc), file=sys.stderr)
        return 2


def parse_cells(preset, cell_args):
    cells = dict(PRESETS[preset]) if preset else {}
    for c in cell_args:
        name, _, span = c.partition('=')
        lo, _, hi = span.partition(':')
        cells[name] = (int(lo), int(hi))
    if not cells:
        raise InputError('no cells: pass --preset or --cell')
    for name, (lo, hi) in cells.items():
        if not name or lo >= hi or lo < 0:
            raise InputError(f'bad cell {name}=[{lo},{hi})')
    return cells


if __name__ == '__main__':
    sys.exit(main())
