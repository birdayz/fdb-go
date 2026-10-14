#!/usr/bin/env python3
"""Prove (or refute) that index entries and their records live on different storage servers.

Inputs (both produced outside timing, at one read version where possible):
  --status  `fdbcli --exec 'status json'`: address -> process class, machine
            locality and roles; only processes holding a `storage` role count.
  --probe   a placement probe document (fdb-bench-placement-probe/v1), written by
            the placement probe tool (a NEW, separately reviewed harness tool that
            calls LocalityGetBoundaryKeys over the store prefix and
            LocalityGetAddressesForKey for every fixture key):
              {"format": "fdb-bench-placement-probe/v1", "store": "benchfetch/small",
               "store_prefix_hex": "...", "read_version": 123,
               "boundaries_hex": ["..."],
               "keys": [{"kind": "index"|"record", "id": 0, "key_hex": "...",
                         "addresses": ["10.88.0.12:4502", "10.88.0.13:4503"]}]}

receipt: checks integrity (every address is a storage server, replica count,
every key under the store prefix, keys in one shard report one address set,
Record Layer key derivation when --layout recordlayer) and, per cell (the set
of IDs whose index entries the cell scans), the fraction of rows whose index
entry's servers are disjoint from all of that row's record-chunk servers, at
storage-server level and at machine level. A cell passes when the
server-level fraction is >= --min-disjoint-fraction (default 1.0).
compare:  two receipts describe the same placement (boundaries and per-key
          server sets); use before and after every timed case.
Exit status: 0 pass/equal, 1 fail/different, 2 unusable input.
"""
import argparse
import bisect
import hashlib
import json
from pathlib import Path
import sys

import mn_common as mc

PROBE_FORMAT = 'fdb-bench-placement-probe/v1'
RECEIPT_FORMAT = 'fdb-bench-placement-receipt/v1'
RECORD_KEY, INDEX_KEY = 1, 2

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
    if probe.get('format') != PROBE_FORMAT:
        raise InputError('probe format must be ' + PROBE_FORMAT)
    try:
        prefix = bytes.fromhex(probe['store_prefix_hex'])
        boundaries = sorted({bytes.fromhex(b) for b in probe.get('boundaries_hex') or []})
        keys = [dict(kind=k['kind'], id=int(k['id']), key=bytes.fromhex(k['key_hex']),
                     addresses=sorted({_addr(a) for a in k['addresses']})) for k in probe['keys']]
    except (KeyError, TypeError, ValueError) as exc:
        raise InputError(f'malformed probe: {exc}') from exc
    if not prefix or not keys:
        raise InputError('probe needs a store prefix and keys')
    return prefix, boundaries, keys


def parse_cells(preset, cell_args):
    cells = dict(PRESETS[preset]) if preset else {}
    for c in cell_args:
        name, _, span = c.partition('=')
        lo, _, hi = span.partition(':')
        cells[name] = (int(lo), int(hi))
    for name, (lo, hi) in cells.items():
        if not name or lo >= hi or lo < 0:
            raise InputError(f'bad cell {name}=[{lo},{hi})')
    return cells


# --- receipt ---------------------------------------------------------------

def build_receipt(status, probe, cells, layout='opaque', index_name='price_idx', rows=1000, chunks=None,
                  expect_prefix=None, replicas=2, min_fraction=1.0):
    servers = storage_map(status)
    prefix, boundaries, keys = load_probe(probe)
    errors = []
    if expect_prefix is not None and prefix != expect_prefix:
        errors.append(f'store prefix {prefix.hex()} != expected {expect_prefix.hex()}')
    rec_prefix, idx_prefix = prefix + pack([RECORD_KEY]), prefix + pack([INDEX_KEY])
    for k in keys:
        if k['kind'] not in ('index', 'record'):
            errors.append(f'unknown key kind {k["kind"]}')
            continue
        if not k['key'].startswith(idx_prefix if k['kind'] == 'index' else rec_prefix):
            errors.append(f'{k["kind"]} key {k["key"].hex()} outside its store subspace')
        if len(k['addresses']) != replicas:
            errors.append(f'{k["key"].hex()} has {len(k["addresses"])} replicas, expected {replicas}')
        for a in k['addresses']:
            if a not in servers:
                errors.append(f'{a} serves {k["key"].hex()} but is not a storage server in status json')
    # One shard (boundary interval) must report one server set.
    by_shard = {}
    for k in keys:
        by_shard.setdefault(bisect.bisect_right(boundaries, k['key']), set()).add(tuple(k['addresses']))
    for shard, sets in sorted(by_shard.items()):
        if len(sets) > 1:
            errors.append(f'shard {shard} reports {len(sets)} different server sets; probe not at one version')
    index = {}
    records = {}
    for k in keys:
        if k['kind'] == 'index':
            if k['id'] in index:
                errors.append(f'duplicate index key for id {k["id"]}')
            index[k['id']] = k
        elif k['kind'] == 'record':
            records.setdefault(k['id'], []).append(k)
    if layout == 'recordlayer':
        for i in range(rows):
            want = idx_prefix + pack([index_name, i, i])
            if i not in index or index[i]['key'] != want:
                errors.append(f'id {i}: index key missing or not {want.hex()}')
            rk = records.get(i, [])
            if not rk or any(not r['key'].startswith(rec_prefix + pack([i])) for r in rk):
                errors.append(f'id {i}: record keys missing or outside the record range of its primary key')
            if chunks is not None and len(rk) != chunks:
                errors.append(f'id {i}: {len(rk)} record keys, expected {chunks}')
    index_shards = {bisect.bisect_right(boundaries, k['key']) for k in index.values()}
    record_shards = {bisect.bisect_right(boundaries, r['key']) for rs in records.values() for r in rs}
    if index_shards & record_shards:
        errors.append(f'{len(index_shards & record_shards)} shard(s) hold both index entries and records')

    def machines(addrs):
        return {servers[a]['machine'] for a in addrs if a in servers}

    cell_out = {}
    for name, (lo, hi) in sorted(cells.items()):
        ids = list(range(lo, hi))
        missing = [i for i in ids if i not in index or i not in records]
        s_dis = m_dis = 0
        idx_srv, rec_srv = set(), set()
        for i in ids:
            if i in missing:
                continue
            ia = set(index[i]['addresses'])
            ra = {a for r in records[i] for a in r['addresses']}
            idx_srv |= ia
            rec_srv |= ra
            s_dis += not (ia & ra)
            m_dis += not (machines(ia) & machines(ra))
        n = len(ids)
        frac = s_dis / n
        cell_out[name] = dict(ids=[lo, hi], rows=n, missing_ids=len(missing), server_disjoint_rows=s_dis,
                              server_disjoint_fraction=frac, machine_disjoint_rows=m_dis,
                              machine_disjoint_fraction=m_dis / n, index_servers=sorted(idx_srv),
                              record_servers=sorted(rec_srv),
                              index_shards=len({bisect.bisect_right(boundaries, index[i]['key']) for i in ids if i in index}),
                              record_shards=len({bisect.bisect_right(boundaries, r['key']) for i in ids if i in records for r in records[i]}),
                              passed=not missing and frac >= min_fraction)
    passed = not errors and all(c['passed'] for c in cell_out.values())
    return dict(format=RECEIPT_FORMAT, passed=passed, errors=errors[:200], error_count=len(errors),
                store=probe.get('store'), store_prefix_hex=prefix.hex(), read_version=probe.get('read_version'),
                layout=layout, replicas=replicas, min_disjoint_fraction=min_fraction,
                storage_servers=servers, boundaries_hex=[b.hex() for b in boundaries],
                index_shards=len(index_shards), record_shards=len(record_shards), cells=cell_out,
                placement={k['key'].hex(): k['addresses'] for k in keys})


def compare(a, b):
    diffs = []
    for field in ('format', 'store_prefix_hex', 'boundaries_hex'):
        if a.get(field) != b.get(field):
            diffs.append(field + ' differs')
    pa, pb = a.get('placement') or {}, b.get('placement') or {}
    if set(pa) != set(pb):
        diffs.append(f'key sets differ ({len(set(pa) ^ set(pb))} keys)')
    moved = [k for k in set(pa) & set(pb) if pa[k] != pb[k]]
    if moved:
        diffs.append(f'{len(moved)} keys changed servers, e.g. {sorted(moved)[0]}')
    return dict(equal=not diffs, differences=diffs)


def sha(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest='command', required=True)
    r = sub.add_parser('receipt')
    r.add_argument('--status', required=True)
    r.add_argument('--probe', required=True)
    r.add_argument('--preset', choices=sorted(PRESETS))
    r.add_argument('--cell', action='append', default=[], help='NAME=LO:HI (IDs [LO,HI)); repeat')
    r.add_argument('--layout', choices=['recordlayer', 'opaque'], default='opaque')
    r.add_argument('--index-name', default='price_idx')
    r.add_argument('--rows', type=int, default=1000)
    r.add_argument('--chunks', type=int, help='record key-values per record (1 small, 2 split)')
    r.add_argument('--expect-prefix-tuple', help='comma-separated strings, e.g. benchfetch,small')
    r.add_argument('--replicas', type=int, default=2)
    r.add_argument('--min-disjoint-fraction', type=float, default=1.0)
    r.add_argument('--output', required=True, help='receipt path (exclusive create)')
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
        expect = pack(args.expect_prefix_tuple.split(',')) if args.expect_prefix_tuple else None
        receipt = build_receipt(json.loads(Path(args.status).read_text()), json.loads(Path(args.probe).read_text()),
                                cells, args.layout, args.index_name, args.rows, args.chunks, expect,
                                args.replicas, args.min_disjoint_fraction)
        receipt['inputs'] = dict(status_sha256=sha(args.status), probe_sha256=sha(args.probe))
        mc.dump_json(args.output, receipt)
        print(json.dumps({k: receipt[k] for k in ('passed', 'error_count', 'index_shards', 'record_shards')}, sort_keys=True))
        for name, cell in receipt['cells'].items():
            print(f'{name}: server-disjoint {cell["server_disjoint_rows"]}/{cell["rows"]}, '
                  f'machine-disjoint {cell["machine_disjoint_rows"]}/{cell["rows"]}, passed={cell["passed"]}')
        return 0 if receipt['passed'] else 1
    except (InputError, ValueError, KeyError, OSError) as exc:
        print('UNUSABLE: ' + str(exc), file=sys.stderr)
        return 2


if __name__ == '__main__':
    sys.exit(main())
