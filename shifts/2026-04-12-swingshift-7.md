# Swingshift-7 Handover

**Date:** 2026-04-12 14:00 — 22:00 CEST
**PRs:** #38 (merged), #39 (continuation, pending)

## Objective

RYW getRange correctness fix, tag throttle implementation, and full test report infrastructure (BEP-native, Ginkgo tree rendering, Hetzner Object Storage CI upload).

## What was done

### 1. RYW getRange silent truncation fix (correctness)

**Bug:** When `serverMore=true` and all fetched results were locally cleared, `getRange` returned `([], false)` — silently losing keys beyond the over-fetch boundary.

**Fix:** Replaced single-call approach with iterative fetch+merge loop. When server has more data but clears consumed all results, advances the scan range and re-fetches. Matches the spirit of C++'s `RYWIterator` demand-driven fetching.

Also optimized `isClearedLocked` and `hasClearsInRangeLocked` from O(n) linear scan to O(log n) binary search — hot paths in the new loop.

10 unit tests with mock server functions. Binding stress: 30/30 seeds, 0 failures.

### 2. Tag throttle duration tracking

Parses `TagThrottleInfo` from GRV reply, stores per-priority tag throttle data on the database, uses server-supplied throttle duration in `nextBackoff` for `tag_throttled` (1213) errors instead of standard exponential backoff. Simplified vs C++ (no Smoother — uses remaining time to expiry).

- `parseTagThrottleInfo`: FDB standard serialization of `unordered_map<StringRef, ClientTagThrottleLimits>`
- `tagThrottleState.replace()`: full map replacement per priority (fixes reviewer-caught cleanup bug)
- `SetTag()` API on Transaction
- `proxyTagThrottledDuration` accumulated but not yet sent back to proxy (tracked in TODO.md)

12 unit tests.

### 3. Test report tool — BEP rewrite

Rewrote `cmd/test-report` from directory-walking + regex parsing to Bazel Build Event Protocol (BEP) + JUnit XML. This is the proper Bazel-native approach:

- `.bazelrc` now includes `--build_event_json_file=.bazel-bep.jsonl` — every `just test` produces BEP
- Tool parses `testResult` events from BEP JSONL → extracts `test.xml` URIs → parses JUnit XML
- Generic — works with any Bazel project (Go, Java, C++, Python)
- `just report` builds the tool then runs it outside sandbox

### 4. Ginkgo per-spec tree rendering

Added `ReportAfterSuite` hooks to both Ginkgo suites that write `ginkgo-report.json` to `$TEST_UNDECLARED_OUTPUTS_DIR`. This preserves Ginkgo's `ContainerHierarchyTexts` for tree rendering:

- Individual spec names: `"SaveRecord creates a new record"` (was `"TestRecordLayer"`)
- Collapsible tree in HTML: Describe/Context nodes with aggregate count badges
- Sub-millisecond duration precision via `RunTime.Seconds()*1000`
- Filters Ginkgo infrastructure nodes ([BeforeSuite], [AfterSuite], etc.)
- Strips `[It]` prefix from spec names

Report went from 1907 flat entries to 3635 with tree hierarchy.

### 5. Hetzner Object Storage CI pipeline

- **OpenTofu:** `minio_s3_bucket` + `minio_s3_bucket_policy` (public-read) via aminueza/minio provider
- **GitHub Actions:** Report generates on every CI run (`if: always()`), uploads to Hetzner per-branch:
  - `/reports/<branch>/latest.html` — always current
  - `/reports/<branch>/<sha>/report.html` — permanent archive
- **README badges:** CI status + test report link to master's latest
- **4 GitHub secrets:** `HETZNER_S3_ACCESS_KEY`, `HETZNER_S3_SECRET_KEY`, `HETZNER_S3_ENDPOINT`, `HETZNER_S3_BUCKET`

Report URL: https://fdb-record-layer-go-reports.fsn1.your-objectstorage.com/reports/master/latest.html

## Current state

- **Branch:** `swingshift-7` merged as #38, `swingshift-7b` (#39) for continuation
- **All 13 Bazel test targets pass** (cached)
- **3635 tests** in report (2315 Ginkgo recordlayer + 429 conformance + 891 standard)
- **Binding stress:** 30/30 API seeds, 0 failures
- **CI:** Green, Hetzner upload working

## Known issues

- `proxyTagThrottledDuration` accumulated but not sent back to proxy (tracked in TODO.md, LOW priority)
- Test report HTML parsing for count display is fragile (grep for stat-total span)
- `mc` CLI downloaded on each CI run — could pre-install on runner via cloud-init
- Node.js 20 deprecation warning for `actions/upload-artifact@v4` — update before June 2026

## What to work on next

### High priority
- **RYW getRange proper iterator** — the iterative fix handles the truncation bug correctly, but a full segment-tree `RYWIterator` port (SnapshotCache + WriteMap) would be more efficient for workloads with many clears. Current approach does multiple server round-trips; C++ does one.
- **DatabaseContext refactor** — consolidate Database/GRVBatcher/LocationCache/Cluster into a cleaner structure

### Medium priority
- **`onProxiesChanged` mid-commit race** — monitor topology changes during commit for faster `commit_unknown_result` detection
- **Test report CI improvements:** pre-install `mc` on runner, content-type header for HTML, link report URL in PR comment
- **Tag throttle: Smoother-based capacity** — current simplified `throttleDuration()` may over-throttle; C++ uses continuous decay Smoother

### Low priority
- **secondDelay speculative requests** — C++ sends hedge request to second server after delay
- **Multi-node testcontainer** — multiple FDB processes for multi-shard testing
- **`proxyTagThrottledDuration` send path** — send accumulated duration back to proxy in GRV request
