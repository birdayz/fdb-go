# Three warm FDB query rounds on the completed 1M corpus

**48,000 timed, source-oracle-checked answers; warm/local closed-loop FDB only.**
This is a separate query campaign on existing run `rw1m0286`, not another load,
a new source census, a 100M gate, a ClickHouse comparison, HA proof or production
latency SLO. The original 1M load/drain/census is archived separately in
[`pilot-1m-v2-workers2-complete`](../pilot-1m-v2-workers2-complete/).

Each of three rounds retains **16,000 timed samples across all 16 classes,
1,000 per class**, with **zero reported oracle failures**: 48,000 answers total.
Six strict before/after health snapshots retain both original FDB statuses and
health summaries; aggregate health was true and no warning exception was used.
Frozen source `c7d00bf938edc6cb9a84df4c63b3bbd3ac22dc94` and executable SHA-256
`e3d5f167aba6c671d31cd278dc16414dcdb784bb36a5f3a516d893f0668f762c`
have unchanged before/after manifests. This does not measure later active-tree
changes. The driver finishes `exit=0 end=2026-10-27T18:12:42Z`.

## Timing and tails are retained, not trimmed

Source-oracle preparation is outside each timed query operation. No hidden
warmup or discarded early observations are claimed: round one's first timed
trace sample, iteration 0, is **2.089242 ms** and remains in the raw receipt.
This is a warm corpus following the earlier load/audit/query work, not a
controlled cold-cache experiment or 48,000 independent randomized trials.

| Round | Reported `wall_ns` | Seconds |
|---|---:|---:|
| 1 | 12,343,030,044 | 12.343030044 |
| 2 | 12,288,651,605 | 12.288651605 |
| 3 | 12,230,106,255 | 12.230106255 |

Those reported round-wall values are separate from individual sample `ns`; do
not turn them into an offered-load or end-to-end service throughput guarantee.
The complete `all-1.json`, `all-2.json` and `all-3.json` preserve every class,
including absent-ID, scope/type controls, broad/narrow/empty time and common
GenAI operation/provider/model cases. They retain raw timings, result digests,
work accounting, histograms and all observed tails, including:

| Observation | Round | Milliseconds |
|---|---:|---:|
| First timed trace / trace maximum | 1 | 2.089242 |
| Standalone span maximum | 1 | **6.737413** |
| GenAI provider maximum | 2 | **6.769635** |
| GenAI provider maximum | 3 | **6.845940** |
| Narrow-time maximum | 3 | **6.166886** |

`selected-summary.json` is the root-computed summary of **six selected classes**,
not a replacement for the full 16-class receipts. Its empirical p99.9 uses
nearest rank: `ceil(0.999 * 1000) = 999`, sorted zero-based index 998. Archive
integrity inspection matched that value and the maximum against raw sample
`ns` for every selected class in all three rounds. A statistic from 1,000 samples
per class is not a production p99.9 SLO or a reliable extrapolation to 100M.

## Contents, provenance and portability

`evidence.tar.gz` losslessly contains `run/` (all three complete raw rounds,
root-selected summary, strict before/after status/health, source/binary manifests,
revision, driver and completion receipt) and `policy/status-check.jq`, the exact
provided strict-health filter. No preceding load receipts or unrelated later
campaigns are duplicated. No new source audit was performed by this campaign;
query success does not replace the separate stored-population audit.

`SOURCE-PATHS.jsonl` and `RAW-SHA256SUMS` inside the bundle map original local
sources and hashes. `archive-members.tsv.gz` enumerates every tar member;
`provenance.json`, `scan-report.json` and `SHA256SUMS` retain population,
nearest-rank checks, full decompressed JSON/address/credential/whitespace scan
and archive integrity. Raw fields, paths and whitespace remain unchanged under
lossless gzip. No Docker-inspect environment/private-address dump is included.

Absolute `/tmp/` paths in original receipts and driver commands are local
provenance, not portable dependencies. Do not execute the driver blindly;
reproduction requires fresh owned paths and all original source/binary/run,
instance, monitor, strict-health and approval safeguards. This archive grants
no launch authorization. Creation only copied, inspected, hashed and compressed
existing files: no source edits, DB/service actions, builds, tests, staging or
commits. Existing pilot data was not touched.
