# Initial CH16 granularity campaign: stopped at the first granularity-1 deadline

**FAILED/incomplete campaign; no timed 1M query matrix ran.** The initial
`ch1m-tune` campaign successfully loaded and audited three fresh 1M layouts, then
its exploratory granularity-1 ingest exhausted a **10-minute whole-operation
budget**. The driver stopped before the merge-drain/timed-query loops. This is
not a query win, a completed four-layout matrix, a 100M result, evidence that
granularity 1 is infeasible, a capacity failure or a global granularity optimum.
The separate fresh 30-minute-budget `0288` arm is excluded, as is its ongoing
monitor tail. Earlier failed archives are not duplicated.

Measured source was clean, frozen commit
`9244882a5914e0f897b0216866ca77d2bd4b6910`, executable SHA-256
`ffdb0e4ff43e12fa2605b7c61bf3a2695ab464c8c518cd21c5ff136a46f1a515`.
Source, executable and seven pinned CH16 configuration files have byte-identical
campaign-start/end manifests. Actual table DDL/settings, parent/projection rows,
marks/parts, bytes and EXPLAIN outputs remain in the successful audit receipts.
Requested granularity alone is not observed rows per granule: adaptive byte
settings and part boundaries matter.

## Completed arms versus the failed extreme

| Requested granularity / namespace | ACK source rows | Ingest seconds | Audit seconds | Audited attributes |
|---|---:|---:|---:|---:|
| 8192 / `ch1m_g8192_0287` | 1,000,000 | 42.307497516 | 28.173450127 | 8,000,000 |
| 512 / `ch1m_g512_0287` | 1,000,000 | 42.790925153 | 27.965601659 | 8,000,000 |
| 64 / `ch1m_g64_0287` | 1,000,000 | 44.226404684 | 28.337884403 | 8,000,000 |

Each successful audit reports zero failures, the full source/attribute population,
and one million rows in each `by_trace`, `by_span` and `by_time` projection. All
three raw audits are retained, not just their timing summaries. This establishes
these immutable fixture layouts, not equivalent replication/durability or a
production comparison with FDB.

Granularity 1 (`ch1m_g1_0287`) reports `context deadline exceeded` at **batch 921075**
after **600.000871682 seconds**. Its `stats.rows=0` is preserved exactly. The
failure API currently discards acknowledged progress; zero is **not** evidence
that no batches committed or that zero source rows were stored. Exact acknowledged
progress for this failed operation is unavailable.

Subsequent SQL observations report **925,010 source rows** and **7,400,080 attribute
rows**, with separate active-parts bytes/marks. They are post-failure count/physical
snapshots, **not ACK counts or a full source audit**. The original files' mtimes
are 18:46:36.469 and 18:46:36.695 UTC, respectively—after the campaign's
`exit=1 end=2026-10-27T18:41:02Z elapsed_seconds=820` receipt. Those filesystem
times label retained observations, not embedded SQL sampling timestamps or values
inside the frozen ingest window. A separate ready-audit attempt at
18:46:36.785 UTC was explicitly rejected in **`loading`** state before any full
source census. Original empty/zero API fields and rejection exit remain unchanged.

## Profile readiness and explicitly separate preflight

CH16 readiness identifies server **26.10.2.103**, loopback HTTP 18125/native 19005,
four CPUs, 16-GiB container cap, 12-GiB server memory cap, 8-GiB query cap,
`max_threads=4` and query cache disabled. Original configuration/source and
resource/ownership guards are included; no Docker-inspect environment or private
address dump is included. Loopback on a shared host is not authentication or a
production security guarantee.

The first readiness failure is retained: `use_query_cache` was rendered as Bool
`false`, while the guard expected TSV `0`. The corrected `toUInt8(...)` readiness
source and whole positive `readiness-02.log` pass are retained. Positive TSV
`4 / 8589934592 / 0` and cache-enabled negative TSV `4 / 8589934592 / 1` are
separate controls. The negative SQL exit **0 means that SQL query succeeded**,
not that whole readiness passed. Root's later
`readiness-settings-guard-controls.txt` records **posthoc pure exact-settings
comparison** exits 0/1; it is neither another whole-readiness invocation nor a
server-profile mutation. The original empty `readiness-cast-negative.tsv` is
also retained: root reports exception 704 for that earlier failed control attempt;
no missing stderr or rejection exit is fabricated.

`preflight-10k/` is an **earlier, separate 10K corpus**, with ingest, audit,
**160 native source-oracle-checked queries** and an owned-drop receipt. Its
subdirectory retains a separate **untimed reconciliation of 160 query IDs**
against filtered `system.query_log`, including opt-in log flush provenance,
schema, raw selected fields and reconciliation outputs. Helper/policy sources
are in `querylog-helper/`. All preflight counts and timings are excluded from the
main 1M campaign's totals. Integer query-log duration zero means sub-millisecond
at that resolution, not zero client time. The helper's pure test source is
preserved but was not executed during archiving.

## Frozen selection, integrity and ownership

`evidence.tar.gz` losslessly retains `run/`, `profile/`, `preflight-10k/` and
`querylog-helper/`. Monitor selection is an unmodified completed prefix through
**18:41:02 UTC**: 260 memory-monitor lines, ending at the initial campaign end.
Later observations from the separate arm are excluded. Post-deadline SQL and
posthoc guard artifacts are explicitly labeled in `provenance.json`, not
backdated into the campaign. No new CH query matrix is published here.

`SOURCE-PATHS.jsonl` and `RAW-SHA256SUMS` inside the bundle identify original local
files, hashes and selections. `archive-members.tsv.gz`, `provenance.json`,
`scan-report.json` and `SHA256SUMS` preserve member inventory, populations,
full decompressed JSON/address/credential/whitespace scanning and integrity.
Raw fields, paths and intentional whitespace are unchanged under lossless gzip.

Absolute `/tmp/` paths are local provenance, not portable dependencies. Do not
blindly execute archived drivers/helpers; reproduction requires fresh owned
paths and the original source/binary/config, instance, monitor, profile and
approval safeguards. Archiving grants no permission to drop or modify a run.
The failed root remains subject to a separate owner-authorized identity/config/
count-matched cleanup decision. Archive creation only copied, inspected, hashed
and compressed existing files: no source edits, builds, tests, jobs, DB/service
actions, staging or commits. Pilot data and the separate running arm were untouched.
