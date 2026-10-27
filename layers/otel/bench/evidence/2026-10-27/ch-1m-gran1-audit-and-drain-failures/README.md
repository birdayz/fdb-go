# Granularity-1 successor: successful ingest, failed audit and separate failed drain window

**Two failed validation windows; no timed 1M query matrix started.** This separate
successor archive does not overwrite or duplicate the initial 10-minute deadline
archive. It preserves fresh run `ch1m_g1_30m_0288`, its first failed audit, a later
distinct bounded drain window and the clean operator stop of CH session01. It
establishes no winner, 100M capacity, production/HA result, granularity infeasibility
or global optimum. Files were prepared under `/tmp` and published only after the
owner's explicit unfreeze. The unchanged `provenance.json` publication value
records that earlier preparation-only state, not the archive's current location.

## Window 1: ingest ACK succeeded; full audit did not

The fresh granularity-1 ingest used a **30-minute whole-command budget** and
acknowledged **1,000,000 source rows / 8,000,000 attributes** in
**767.957222264 seconds**. Its ingest receipt has no error. This load shared the
CH container with the old failed granularity-1 namespace and three other layouts;
background merge work from those earlier layouts was **not drained before the
fresh 0288 load**. Its ACK rate is an observed closed-loop campaign cost, not
isolated granularity-1 ingest performance or a valid ingest comparison with FDB
or the other layouts. Frozen source is
`9244882a5914e0f897b0216866ca77d2bd4b6910`, executable SHA-256
`ffdb0e4ff43e12fa2605b7c61bf3a2695ab464c8c518cd21c5ff136a46f1a515`.
This budget does not override the server-side query deadline.

The first audit started **18:57:40.697562326 UTC** and failed after
**31.168207820 seconds overall**. Its decisive nonempty error is **HTTP 408,
ClickHouse Code 159, `TIMEOUT_EXCEEDED`**, reporting 2003.823 ms elapsed against
2000.000 ms maximum during the EXPLAIN phase. The partial audit receipt already
contains counts of 1M source rows and 8M attributes, active-parts storage values
and a rare-query EXPLAIN. Those partial fields—and the raw `failures=0` counter—
**do not make the full audit pass** or certify every expected projection/semantic
check. The campaign remained **`exit=1 end=2026-10-27T18:58:12Z`**. No error field
or partial-count value was rewritten.

## Window 2: bounded postdrain gate failed; second audit never executed

The later driver is a separately labeled window on the existing namespace,
without new ingestion, source/config changes, ownership adoption or alteration
of the original failed receipt. The ownership/readiness check passed before it
sampled active merges **60 times**, from **18:59:25 through 19:04:21 UTC**. Every
sample was nonzero (range **1–7**, final **1**), so the requirement for three
consecutive zero-active samples was never met. It ended
**`exit=6 end=2026-10-27T19:04:27Z`**. There is no second `audit.command` or
`audit.json`: execution stopped before them, not after another audit attempt.

The merge query covers five owned namespaces: 8192, 512, 64, the original failed
granularity-1 namespace and fresh 0288. Thus each aggregate count is not solely
an attribution to 0288. The later operator-stop snapshot separately shows an
active 0288 `attr_lookup` merge. This is sampled background-work evidence, not
proof of a hardware cause or a precise per-query bottleneck.

Source/binary/seven-config before/after manifests match byte-for-byte in both
windows. The second window's predecessor hashes also match, proving that its
later validation did not rewrite the first campaign's status/ingest/audit.
No timed 1M needle campaign, tuning matrix or query-log comparison is present.

## Explicit operator stop, not an inferred crash

The owner stopped the exact CH session01 instance, beginning **19:05:29** and
ending **19:05:32 UTC**. Filtered state records `Running=false`, `ExitCode=0`,
`OOMKilled=false` and finish time **19:05:32.333863904 UTC**. The owner retained the
named data volume; filtered metadata preserves its mount identity, not a new
post-stop data audit. A clean process exit does not mean outstanding merges had
finished or that an audit passed. Archive creation
did not stop, restart, query, drop or modify any database or service.

The finite session monitor is retained as an unmodified contiguous window from
the last baseline before ingest (**18:44:52 UTC**) through the operator stop:
**244 lines**, last timed memory observation **19:05:27**, followed by the original
`Container is not running` message. This terminal message follows the documented
operator stop; it is not reclassified as a monitor-initiated failure. Initial
campaign-era monitor lines are excluded. `provenance.json` distinguishes the
campaign, after-campaign drain window, intervening observations and stop boundary.

## Contents, integrity and portability

`evidence.tar.gz` losslessly contains `campaign-0288/`, `postdrain-window/`,
`operator-stop/` and `profile/`. It retains both drivers, all available raw
receipts/manifests, the exact seven pinned profile sources, the finite monitor
window and filtered stop/mount metadata. No full Docker-inspect environment or
private-address dump, initial campaign archive or subsequent resumed session is
included. Missing second-audit/physical/source-verification results are absent,
not fabricated zeros or successes.

`SOURCE-PATHS.jsonl` and `RAW-SHA256SUMS` inside the bundle identify original local
sources and hashes. `archive-members.tsv.gz`, `provenance.json`, `scan-report.json`
and `SHA256SUMS` retain inventory, population/error checks and full decompressed
JSON/address/credential/whitespace scan. Raw fields, paths and whitespace remain
unchanged under lossless gzip.

Absolute `/tmp/` paths in receipts/drivers are local provenance, not portable
dependencies. Do not execute archived drivers blindly; reproduction needs fresh
owned paths and every original approval, source/binary/config, instance/profile,
monitor and readiness guard. This archive grants no launch/drop authorization.
Preparation only copied, inspected, hashed and compressed existing files: no
active-worktree edits, builds, tests, jobs, services, DB actions, staging or commits.
