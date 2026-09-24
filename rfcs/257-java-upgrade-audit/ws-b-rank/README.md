# WS-B rank-valued scans — intermediate acceptance

This is not WS-B completion or an implementation ACK. All changes remain local.

Retained regressions in `pkg/recordlayer/rank_scan_test.go` exercise grouped
composite scores, ties, duplicate counting, overlapping/appended primary keys,
cold reopen, forward/reverse row-limit pagination, empty scans, malformed entry
keys, invalid bounds literals, nil/non-rank indexes, disabled state, cancellation
(FDB 1025), and missing secondary scores represented as `tuple(nil)`. Enrichment
copies entries; ordinary values remain empty.

`conformance/rank_index_conformance{.java,_test.go}` adds both-writer checks of
packed key, primary-key and value bytes. Each writer exercises both scan types,
both directions and both enrichment flags, with explicit ranks `[0,0,1,2]`.
The initial red Java run was a Go fixture expecting non-nil empty tuple instead
of the existing nil empty value. The expected raw value now matches the existing
API; exact packed-value assertions remain mandatory.

Evidence on the four-file population in `rank-values.sha256`:
- `rank-values-java-green.log`: 2/1498 focused Ginkgo specs pass.
- `rank-values-full.log`: `just test`, 93 targets pass; 4 executed, 89 cached.
- `rank-values-race.log`: uncached full record-layer race 3338/3339 pass;
  conformance race 1379/1498 pass. The one opt-in million-record exclusion and
  119 conformance target exclusions are existing exclusions, not new skips.
- All four SHA256 values were checked unchanged after both full and race runs.
  This is a four-file freeze check, not a claim of a whole-source-tree freeze.

At this initial checkpoint, additional acceptance was still outstanding. The
follow-on checkpoint below supersedes that remaining-work list. No CI or upgrade
completion claim is made.


## Follow-on: terminal replay, limits and snapshot conflict proof

Paired plain/enriched tests now cover one-byte and one-row budgets across both
scan types, both directions, duplicate-counting modes and PK-overlap modes.
Every returned continuation is compared, stop reasons are pinned, terminal
results are replayed, and each pair of OnNext calls increments EventScanIndex
exactly twice. Absolute ranks are asserted separately from pairwise equality.

The probe exposed a production defect in `indexCursor`: repeated row-limit
lookahead consumed more entries until the terminal reason and continuation
changed. Java `KeyValueCursorBase.onNext` caches its first no-next result.
`indexCursor` now does the same, storing the terminal result by value rather
than allocating a result on each returned row. The original failure is retained
in `rank-limits.log`. `rank-terminal-mutation.py` verifies the replay-disabling
mutation actually lands and compiles; the retained regression kills it.
The first post-fix run also corrected a fixture expectation: the last row-limited
page is source-exhausted in the existing Go scan API, unlike a byte-limited page.

A separate real-FDB pair changes only the secondary ranked set after snapshot
BY_VALUE reads. Enriched scans conflict with 1020 on commit; plain scans commit.
This pins serializable enrichment and absence of secondary conflict reads for
plain scans without mocked instrumentation.

Both-writer Java tests now compare complete persisted store bytes before and
after each scan pair. Store open is established before the baseline: opening
Java's format-7 store upgrades its header to Go's configured format, independently
of rank scanning (`rank-persistence.log` captures that initial fixture boundary).
The scan measurements include index/ranked-set/header/record bytes and show no
changes (`rank-persistence-green.log`).

Frozen acceptance (`rank-freeze.py`, `rank-tree-freeze.json`): **6666 existing
tracked/untracked nonignored files**, including source, build files and evidence,
were hashed before verification and all hashes/population checked after each run.
Gitignored Java checkout is outside this population; canonical source remains
4.14.2.0 at fdacd162a9c8acfadc49082b89185c823ab8ae4a.
- `rank-tree-full.log`: all **93/93 targets executed uncached and passed**.
- `rank-tree-race.log`: full uncached recordlayer **3340/3341** and conformance
  **1379/1498** pass; existing exclusions unchanged.

The grouped/null interoperability and enrichment mutation work outstanding at
this checkpoint is covered below. Completed-milestone review ACKs remain open.
The terminal-replay fix belongs in those implementation reviews; this is not
published CI.


## Grouped Java matrix and preload conflict correction

`rank-matrix-java-green.log`: ten focused Java specs pass (of 1506 total):
both writers × duplicate-counting off/on × overlapping PK off/on for grouped
composite scores, plus the two ungrouped writer cases. Grouped cases exercise
both groups, both scan types/directions, and BY_VALUE null ranks after clearing
only group zero's secondary data. Group one remains intact. Packed keys, primary
keys and value bytes are compared with explicit absolute ranks.
The initial Java-writer fixture needed DynamicMessage decoding against its
loaded metadata descriptor rather than passing a generated Order descriptor to
RecordMetaData built from protobuf; the red log is retained.

Mutation testing exposed an additional Go alignment defect: every record-layer
PreloadForLookup caller passed a serializable transaction, while Java's
RankedSetIndexHelper preloads using readTransaction(true). The existing-score
conflict probe initially killed neither a snapshot-only rank mutation nor proved
which read caused its conflict: preload added it independently. A new
missing-score test returns tuple(null), then adds a DIFFERENT score concurrently.
Only snapshot preload permits this valid commit. Its red/green evidence is
`rank-preload-{red,green}.log`. All seven Go preload calls in rank, aggregate and
time-window maintainers now pass Snapshot(); actual rank/GetNth reads retain
the original transaction. Existing-score enrichment still conflicts with 1020;
plain scans and missing-score lookups against unrelated scores commit.

`rank-mutations.json` records eight compiled mutation kills over the ten focused
rank specs, including serializable preload and snapshot rank lookup separately.
The first surviving snapshot mutant is explicitly retained as
`rank-mutant-snapshot-rank-survived.log`, not counted as a kill. Terminal replay's
separate mutation kill above remains valid on its stated earlier population.

Final code checkpoint for implementation review:
- `rank-matrix-full.log`: **93/93 uncached targets executed and passed**.
- `rank-matrix-race.log`: recordlayer **3342/3343**, conformance **1387/1506**
  uncached race specs passed; existing opt-in/target exclusions unchanged.
- `rank-matrix-tree-freeze.json`: **6678 tracked/untracked nonignored files**
  unchanged across both runs; checker `rank-matrix-freeze.py` retained.
- Java, FDB C++ and publication boundaries unchanged. No implementation ACK yet.
