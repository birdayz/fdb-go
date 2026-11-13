# Confirmed native query path among three eligible CH 1M layouts

**Completed guarded local 1M query evidence, not a 100M, HA, production/SLO,
global-optimality or isolated-engine win.** This archive retains the eligible
three-layout tuning matrix, independently selected native configuration,
post-restart confirmation, separate DATA-query reconciliation, finite resume02/
resume03 sessions and the failed granularity-1 eligibility windows. Earlier
failures remain failed; granularity-1 native timing population is **zero**.

## Populations and fixed selection rule

The matrix contains **18 complete receipts**: granularities **8192/512/64 ×
native/HTTP × three rounds**, each with **1,600 samples, 100 per each of 16
classes**. Total: **28,800 timed answers, zero reported oracle failures**.
Confirmation contains **three native receipts on selected 8192**, each with
**16,000 samples, 1,000 per class**: **48,000 answers, zero reported failures**.
All 16 classes, raw sample timings/digests and histograms are preserved, not just
the four target classes used to choose a configuration.

The fixed score is the geometric mean of **12 native per-round class medians**:
`trace`, `span`, `genai_response`, `time_narrow` × three matrix rounds. HTTP does
not score; exact ties select the smaller numeric granularity. Scores:

| Eligible granularity | Native 12-median geometric mean, ms |
|---|---:|
| **8192 (selected)** | **6.297400852** |
| 512 | 7.342779170 |
| 64 | 11.485563140 |

This is best tested among the three passing layouts for this fixture and rule,
not a universal MergeTree tuning optimum. Original successful 1M source audits
for all three layouts are included: 1M source rows, 8M attribute rows, all three
named projections and retained actual DDL/parts/marks/EXPLAIN evidence. Requested
granularity alone is not an observed rows-per-granule guarantee.

## Preserve failed eligibility gates and session boundaries

- The original 10-minute granularity-1 deadline failure and later `loading` audit
  rejection remain failed, with post-deadline counts distinct from ACK progress.
- Fresh 0288 acknowledged 1M, but its first full audit failed HTTP 408/Code 159 at
  the fixed 2-second query deadline. Partial 1M/8M audit counts are not a full pass.
  Its load shared CH with old layouts and undrained background merges; its ACK
  rate is not isolated granularity-1 ingest performance or an ingest comparison.
- The session01 five-minute drain window failed before another audit.
- Resume02's distinct ten-minute window ended **exit 6**, after 120 observations;
  the final three counts were **1, 0, 0**, not three consecutive zeros. No audit
  followed that gate.
- The separate after-maintenance window observed **three zeros**, then its audit
  still failed **HTTP 408/Code 159, fixed 2-second trace-EXPLAIN limit**. It ended
  **exit 1**. No hidden budget extension or passing granularity-1 audit is inferred.

Failed receipts, their frozen predecessor/source/config hashes and drivers are
retained or explicitly referenced. None is replaced by a later success. Both
resumed profiles retain approval, exact-instance/volume ownership guards,
unapproved-call/negative controls, source/config and readiness receipts, finite
monitors and clean operator-stop metadata. Resume02 finished with exit 0 at
**19:38:34.399470657 UTC**; resume03 at **19:55:42.982146661 UTC**. The retained
readiness framing observations are **output format `None`, progress headers 0**.
State/mount snapshots are filtered operational metadata; no full Docker-inspect
`Config.Env` response is retained in this new archive. The documented inspect
negative-fixture exception belongs only to the earlier pilot archive and is not
duplicated here. Source helpers and their inline negative/vacuity fixtures are
retained; analysis-control receipts are tied to their final frozen helper hashes.

## Guarded query-path comparison, not pure engine latency

The comparison uses prior **FDB 48,000 warm answers** versus the **48,000 CH native
confirmation answers**, 3,000 observations per class per backend. FDB timing JSONs
are referenced by hashes in the existing `query-1m-fdb-warm` bundle rather than
duplicated. Its separate 1M source census is also referenced and the small audit
receipt is included. This is not a new FDB source census.

| Class | FDB per-round p50 range, ms | CH native per-round p50 range, ms |
|---|---:|---:|
| Trace ID | 0.594875–0.603171 | 5.420146–5.852124 |
| Standalone span ID | 0.554239–0.560981 | 5.421187–5.790217 |
| GenAI response attribute | 0.566241–0.570950 | 6.536650–8.130559 |
| Narrow intrinsic time | 0.602269–0.610445 | 6.444506–6.595582 |

All 16-class distributions, tails and descriptive ratio fields remain in the root
comparison artifacts. Ratios are cross-round descriptive bounds, not independent
trial estimates, causal decompositions or production guarantees. Empirical p99.9
at n=1,000 uses nearest-rank **999**, zero-based index **998**; it is not a
production tail SLO. Tuning's class population is 100, not 1,000.

Confirmation followed a clean server restart without hidden/discarded warmup.
Its first trace sample, iteration 0, is **9.875163 ms**; the first native matrix
trace was **18.354470 ms**. These samples and all subsequent tails remain raw.
A restart does not establish cold OS/file caches. Source-oracle preparation is
outside individual sample clocks; client `ns` measures the guarded operation,
not a naked storage-engine request.

FDB measured source is **c7d00bf938edc6cb9a84df4c63b3bbd3ac22dc94**; CH is
**9244882a5914e0f897b0216866ca77d2bd4b6910**, both reporting **Go 1.27.3**.
Binary/source hashes are retained in selection/comparison/provenance. Both used
nominal **4-CPU/16-GiB** profiles, but deadlines/instrumentation differ:
**FDB 20-minute command timeout, diagnostics on; CH 10-minute command timeout,
diagnostics off, server query limit 2 seconds**. Caps do not establish identical
cache history, implementation overhead or failure/durability guarantees.
Ingestion batching/acknowledgment/durability was not matched; **no ingest win** is
claimed. These are sequential local immutable-corpus guarded paths, not isolated
engines or a full dynamic OTel/content/log/session backend.

## Query-log DATA work is separate, untimed evidence

All **21 successful reconciliations** are retained, linking **76,800 globally
unique DATA query IDs**: 28,800 tuning plus 48,000 confirmation. The independent
root positive-population census checks source/provenance/artifact hashes, one
`QueryFinish` per ID, balanced classes, scope, missing/duplicate/error absence,
and observed **4 threads / 8-GiB query limit / cache off**. Per-class read-row,
read-byte and server-duration histograms for all 16 classes and 21 reports remain
in `ch1m-eligible3-data-work` artifacts. Raw timings are not rewritten.

The four target DATA server-duration medians span **3–6 ms** across confirmation;
the guarded client medians are roughly **5–8 ms**. A sample ID identifies only
the DATA request, **not the separate ownership request or the complete public
operation's work/elapsed time**. Rounded integer server milliseconds must not be
subtracted to invent a precise ownership/network cost. Duration **0 ms means
less than 1 ms at that resolution**, not zero time. No equivalent FDB DATA-server
work measurement is available. Untimed log flushing/reconciliation is distinct
from query timing and preserves raw unavailable native metric fields.

## Posthoc read-only analysis controls

The final `compare-eligible3.mjs`, `verify-eligible3-reconciliation.mjs` and
`analysis-census-test.mjs` are retained with the owner's complete validation
receipt. Helper hashes before/after are identical. The recorded harness stdout
reports **66 helper executions, including four positive controls**, plus
negative/vacuity, quantile/zero-ms boundaries and existing-output refusal. Its
per-case command construction, mutations and exit/assertion checks are in the
frozen harness source; the harness emits a final summary rather than separate
per-case terminal logs. Full recorded stdout/status/start/end and refreshed
comparison/census stdout are retained unchanged.

Both fresh `*.validated.json` outputs are **byte-identical** to their historical
numeric artifacts. This is posthoc pure-file validation, not another DB benchmark;
no helper or test was executed to create this archive. Four small positive-control
outputs and an unchanged existing-output marker are retained under
`analysis-controls/owned-fixture-results/`. They come from the owner's isolated,
mutated temporary fixtures, **not real measurement inputs**. The deliberately
non-JSON `already-exists.json` contains `untouched`; it tests refusal to overwrite
an existing output and is documented as a scan exception. The large temporary
fixture tree is not copied; the frozen harness reconstructs it by copying the
referenced frozen inputs. Offline reproduction uses each final analysis helper's
`--root`/`--output` interface, after reconstructing that input tree in a fresh
owned location; do not run a receipt blindly against the original machine paths.

## Archive, provenance and portability

`evidence.tar.gz` losslessly retains runtime receipts, both finite resumed
profiles/approvals/stops, helpers and negative controls, original root analyses,
validated analysis/census outputs, failed windows and three original CH audits.
`REFERENCES.json` identifies prior FDB bundles and raw member hashes. Absolute
`/tmp/` paths in scripts/receipts are local provenance, not portable dependencies;
reconstruction requires the referenced earlier bundle where explicitly stated.
Do not blindly execute archived helpers/drivers: fresh owned paths and all
original approval, instance/profile/volume/TTL, source/binary/config, readiness
and budget safeguards remain mandatory. This archive grants no launch/drop
permission.

`SOURCE-PATHS.jsonl` and `RAW-SHA256SUMS` retain source paths and byte hashes;
`archive-members.tsv.gz`, `provenance.json`, `scan-report.json` and `SHA256SUMS`
record exact populations/inventory, full decompressed JSON/address/credential/
whitespace scanning and integrity. Raw fields and intentional whitespace are
unchanged under gzip. Tar bytes are immutable after publication/freeze. Archive
creation only copied, inspected, hashed and compressed existing files: no DB or
service actions, source/library edits, builds, tests, staging or commits.
