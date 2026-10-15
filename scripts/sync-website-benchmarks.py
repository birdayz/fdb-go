#!/usr/bin/env python3
"""Sync docs/benchmarks/{hcloud-rebench-20261014,hcloud-multinode-vpc-20261014}
into website/content/docs/benchmarks/ with Hugo front matter and clean links.
"""
from pathlib import Path
import re

REPO = Path(__file__).resolve().parent.parent
SRC_ROOT = REPO / 'docs/benchmarks'
DST_ROOT = REPO / 'website/content/docs/benchmarks'

CAMPAIGNS = [
    (
        'hcloud-multinode-vpc-20261014',
        '4-Node VPC Campaign (2026-10-14)',
        'Preregistered 4-host VPC benchmark campaign (1 client VM + 3 FDB 7.3.77 server VMs, double ssd-2, 9 storage servers): pure-Go vs CGo at 1, 8, 32, and 64 workers and cross-server USE_REMOTE_FETCH vs SCAN_AND_FETCH.',
        1,
        [
            ('phase-m1-client.md', 'Phase M1: Client (1, 8, 32, 64 Workers)', 'Pure-Go vs CGo client latency and process CPU across 1, 8, 32, and 64 closed-loop workers over a real VPC to a 3-node double ssd-2 cluster.', 1),
            ('phase-m2-remotefetch.md', 'Phase M2: Remote Fetch (3-Node VPC)', 'Record Layer USE_REMOTE_FETCH vs SCAN_AND_FETCH across 9 storage servers on a 3-node VPC cluster with verified shard placement.', 2),
        ],
    ),
    (
        'hcloud-rebench-20261014',
        'Single-Node 16-vCPU Campaign (2026-10-14)',
        'Preregistered 8-phase benchmark campaign on a dedicated 16-vCPU host comparing the pure-Go client, Record Layer, mapped remote fetch, and SQL across the RFC-258 trees.',
        2,
        [
            ('phase-a-client.md', 'Phase A: Client (1 Worker)', 'Pure-Go client vs CGo (libfdb_c 7.3.77) and vs pre-allocation pure-Go across 11 workloads at 1 closed-loop worker.', 1),
            ('phase-a2-client-8w.md', 'Phase A2: Client (8 Workers)', 'Pure-Go client vs CGo and vs pre-allocation pure-Go at 8 concurrent closed-loop workers.', 2),
            ('phase-b-recordlayer.md', 'Phase B: Record Layer', 'Record Layer benchmarks across legacy, before-allocation, and final trees.', 3),
            ('phase-c1-remotefetch.md', 'Phase C1: Remote Fetch (Forward)', 'Forward USE_REMOTE_FETCH vs SCAN_AND_FETCH across pr2 and final trees on small and two-chunk split records.', 4),
            ('phase-c2-reverse-skip.md', 'Phase C2: Remote Fetch (Reverse & Skip)', 'Reverse scans and executor Skip 3 paths with USE_REMOTE_FETCH vs SCAN_AND_FETCH.', 5),
            ('phase-c3-synthetic-rtt.md', 'Phase C3: Remote Fetch (0.5 ms Synthetic RTT)', 'Sensitivity of USE_REMOTE_FETCH vs SCAN_AND_FETCH to 0.5 ms nominal synthetic veth RTT.', 6),
            ('phase-d1-sql.md', 'Phase D1: SQL Common', 'SQL plan-cache point select across legacy, before, and final trees.', 7),
            ('phase-d2-sql-remotefetch.md', 'Phase D2: SQL Remote Fetch', 'SQL non-covering index scan with USE_REMOTE_FETCH vs SCAN_AND_FETCH.', 8),
        ],
    ),
]


def transform_body(text: str, is_index: bool) -> str:
    # Strip the top-level H1 heading because Hugo renders `title` from front matter.
    lines = text.splitlines()
    if lines and lines[0].startswith('# '):
        lines = lines[1:]
        while lines and not lines[0].strip():
            lines.pop(0)
    body = '\n'.join(lines) + '\n'

    # Rewrite relative repo links that escape docs/benchmarks/<campaign>/ to GitHub URLs.
    body = body.replace('(../campaign-harness.md)', '(https://github.com/birdayz/fdb-go/blob/master/docs/benchmarks/campaign-harness.md)')
    body = body.replace('(../../../tools/benchcampaign/singlenode/README.md)', '(https://github.com/birdayz/fdb-go/blob/master/tools/benchcampaign/singlenode/README.md)')
    body = body.replace('(../../../tools/benchcampaign/multinode/README.md)', '(https://github.com/birdayz/fdb-go/blob/master/tools/benchcampaign/multinode/README.md)')

    # Rewrite cross-campaign and sibling `.md` links to Hugo page paths.
    body = re.sub(r'\(\.\./([a-z0-9-]+)/README\.md\)', r'(/docs/benchmarks/\1/)', body)
    body = re.sub(r'\(\.\./([a-z0-9-]+)/([a-z0-9-]+)\.md\)', r'(/docs/benchmarks/\1/\2/)', body)
    body = body.replace('(README.md)', '(../)')
    if is_index:
        body = re.sub(r'\((phase-[a-z0-9-]+)\.md\)', r'(\1/)', body)
    else:
        body = re.sub(r'\((phase-[a-z0-9-]+)\.md\)', r'(../\1/)', body)
    return body


def main():
    for cid, title, desc, weight, children in CAMPAIGNS:
        src_dir = SRC_ROOT / cid
        dst_dir = DST_ROOT / cid
        dst_dir.mkdir(parents=True, exist_ok=True)

        readme_body = transform_body((src_dir / 'README.md').read_text(), is_index=True)
        index_md = f'---\ntitle: "{title}"\ndescription: "{desc}"\nweight: {weight}\n---\n\n{readme_body}'
        (dst_dir / '_index.md').write_text(index_md)

        for fname, ctitle, cdesc, cweight in children:
            cbody = transform_body((src_dir / fname).read_text(), is_index=False)
            page_md = f'---\ntitle: "{ctitle}"\ndescription: "{cdesc}"\nweight: {cweight}\n---\n\n{cbody}'
            (dst_dir / fname).write_text(page_md)


if __name__ == '__main__':
    main()
