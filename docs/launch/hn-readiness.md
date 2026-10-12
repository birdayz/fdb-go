# HN launch readiness

This plan comes from nine independent reviews: first look, Java parity, FDB client, public API, code health,
legal/security, performance, operations, and a simulated HN thread.

The baseline tracker was verified at `origin/master` 564f835d3. This release-readiness
update is based on 6b3e2f0f0 (green master CI run 38160820959) on 2026-10-11;
it updates release decisions and evidence, not a fresh audit of every row.

- **Severity:** **B** = launch blocker: launch waits for it. **H** = high: fix before launch, or list
  it as a known gap in `STATUS.md`. **M** = medium: may ship, stays tracked.
- **Status:** `done` (with the PR, "settings" for a repository setting, or "owner"), `PR [#N] open`,
  `in progress` (work landed or under way; row not closed), `not started`.
- **Workstream** is the agent workstream doing the work. D rows are decided by the project owner.

## Blocking launch

- Public API reduction D5 remains for v0.3.0, the HN launch release (§1).
  v0.2.0 preparation does not declare launch readiness.
- Every open H row fixed, or listed as a known gap in `STATUS.md` (release-ws). Five are open: 5 s
  per-RPC read timeouts, 38 KB per read-only
  transaction, real-cluster fault testing, planning cost, unbounded default limits.
- Release (release-ws prepares, the owner tags), per `RELEASE.md`: the cut PR (step 1), then green
  CI including the full lane and the dry run on the cut commit (steps 2–3), then
  explicit maintainer tagging authorization. No local full lane or premature tag.
  Only after v0.2.0 is proxy `@latest` may `cmd/frl/v0.1.1` retract the nested module. The order matters: root
  v0.1.0 does not contain `cmd/frl`.

## 1. Owner decisions

The owner delegated release decisions; API reduction remains a v0.3.0 launch workstream.

| # | Decision | Status | Notes |
|---|---|---|---|
| D1 | Credential rotation | done (owner) | Completed by the owner on 2026-10-09. Rotation and repository-history cleanup are separate operations. |
| D2 | Git history: keep, strip binaries, or squash | done (owner) | Keep history. README notes the approximately 440 MiB full clone observed during preparation and offers a blobless clone. Forks, caches and module proxies retain old objects. |
| D3 | Agent material in the public tree | done (owner) | Keep `CLAUDE.md`, `AGENTS.md` and `.claude/`; CLAUDE uses plain general rules. Remove tracked `shifts/`, `TODO_OLD.md`, `TODO_client.md`, `road-to-prod.md`, `PRODUCTION_READINESS.md`, `PORT.md` and `STRESS_RELATIONAL.md`; history retains them. [#791] removed reviewer names from test names. |
| D4 | AI-assisted disclosure | done (owner) | README states AI agents work under human direction and links Java conformance, libfdb_c differentials, the binding tester and deterministic simulation. Test infrastructure is not a universal-parity claim. |
| D5 | Shrink the public API (about 88% of 11.5k exported identifiers should be internal) | in progress | Planner, simfdb, dst, conformance and test seams are importable. The gomock mocks moved to `pkg/relational/api/apimock` ([#810]) and the demo protos to `gen/demopb` ([#842]); both are still importable. Neither move made those packages internal. Owner assigned API reduction to v0.3.0, the HN launch release, not v0.2.0. |
| D6 | CI fleet capacity and deployment security review | done (owner, scoped verification) | Root verified two online runners on 2026-10-11: master's root-owned 0755 job guard and hook setting on both; actual master run 38160820959 executes it before steps in all seven jobs. Default token is read-only and cannot approve PRs; Claude mention workflow uses hosted runners and a trusted-association allowlist. This checks the deployment/trust boundary, not blanket security. |

## 2. Findings, by workstream

### Product correctness and performance

| Sev | Item | Status | Workstream |
|---|---|---|---|
| B | ~1 ms per transaction from the GRV batch timer (Go netpoller rounding). Get 1.22 ms vs CGo 0.21 ms; whole stack inherits it. | done [#789]: idle batches dispatch without timer delay | perf-lead2 |
| H | A GRV wait over 30 s returns a non-retryable `DeadlineExceeded` during recoveries | done [#789]: the batch waits on the database context; regression holds a GRV past 31 s. [#844] fixes the coordinator wedge behind it | perf-lead2 |
| H | Tenant `commit_unknown_result` fence uses the unprefixed key, so a commit can land after 1021 is returned | done [#800] | hn-client-ws2 |
| H | Endpoint-not-found replies are dropped | done [#835], [#857] (hedge fail-over fix) | hn-client-ws2 |
| H | 5 s per-RPC timeouts that libfdb_c lacks | in progress: GRV replies done [#789], commits done [#882]; reads still time out | hn-client-ws2 |
| H | Commit ignores ctx and timeout | done [#882]: transaction timeout and Cancel end the public wait; Reset, collection and database close drop native work | client-gpt |
| H | 38 KB allocated per read-only transaction | in progress: [#832] and [#843] cut allocations; re-measure pending | hn-client-ws2 |
| H | Lazy futures | done [#840] | hn-client-ws2 |
| H | `APIVersion` above 730 accepted | done [#823] | hn-client-ws2 |
| H | No real-cluster fault testing (recovery, kill, partition, coordinators); binding stress is only 1×100 | in progress: [#844] adds a recovery regression that kills every fdbserver; the rest has no PR | hn-client-ws2 |
| H | Plan-cache key includes literals and `?` values, so prepared statements always re-plan (3 ms per lookup). Becomes B if launch makes any SQL performance claim (§3 cuts them). | done [#792] | hn-sql-ws3 |
| H | A 6-table join plans for 5.6 s with no error; no planner bound | done [#801] as Java parity: no caps by default, the caller's context deadline bounds planning; [#805] pins Java's own cost for this join | hn-sql-ws3 |
| H | Planning costs milliseconds | not started | hn-sql-ws3 |
| M | Serial index→record fetch (the N+1 question) | done [#798] | hn-sql-ws3 |
| M | Serial FlatMap | in progress | hn-sql-ws3 |
| H | Collated indexes silently diverge from Java bytes (data corruption when shared) | done [#790]: refuses Java-incompatible collation by default; Go-only opt-in | hn-compat-ws2 |

### First impression

| Sev | Item | Status | Workstream |
|---|---|---|---|
| B | Every quickstart fails (domain registration, `NOT NULL`, DSN case, unconfigured docker FDB, swallowed errors, typed-store snippet) | done [#808]: [#788] fixed the listed failures; [#808] chains the steps so a failed `frl fdb up` stops the demo | hn-health-ws2 |
| B | "2–4× faster" claims are false (pre-RFC-104 GRV cache; one-way netem labelled as RTT) | done [#788]: claims removed, website check guards them | perf-lead2 |
| B | `go get` gives v0.1.0, 682 commits behind and storage-incompatible; `go install …/cmd/frl@latest` resolves to the stale nested module | in progress: [#825] landed the notes, checklist and `cmd/frl` vanity stub (live); a rehearsal dry run on master e047fc58c passed (run 38152910574); retraction commit 948f2d3b6 is ready. Next: reviewed readiness PR, correctness fixes, exact-cut CI/full-lane and dry-run gates, explicit tagging approval, then v0.2.0 proxy latest before `cmd/frl/v0.1.1`; no tag is done | release-ws |
| M | `frl fdb up` hangs silently on a nearly full host disk (FDB throttles to 0 TPS) | done [#814] | hn-health-ws2 |
| H | macOS: `frl fdb up` used `--network host` | done [#788]: loopback-published bridge network | hn-health-ws2 |
| H | Positioning: what it is, and the "unofficial / not Apple" disclaimer | done [#788]: README, website, NOTICE | release-ws |
| M | Library logs INFO to stderr on every open | done [#788]: routine SQL warm-up logs at Debug | hn-sql-ws3 |

### Honesty of docs

| Sev | Item | Status | Workstream |
|---|---|---|---|
| B | No single production-readiness answer. road-to-prod "CONFIRMED" rests on deleted nightlies, which were 0/30 green before deletion. | done [#788]: `STATUS.md`; road-to-prod, PRODUCTION_READINESS and PORT marked historical | release-ws |
| B | Wire-compat headline without exceptions (collation, vector_spfresh/defaults, Lucene, spatial, synthetic types, SQL continuations, TEXT on non-Latin scripts, format-14 header upgrade, v0.1.0 SQL keyspace) | done [#788]: `docs/compatibility.md` lists all nine. Narrowing, not closing: done [#803] pins TEXT segmentation and [#804] refuses non-Java vector options; done [#809] refuses VECTOR structure faults and missing dimensions, [#815] reads column-0 vectors as bytes like Java, [#790] refuses collation | hn-compat-ws2 |
| H | 20 doc contradictions (4.12.11 vs 4.14.2.0, FDB 8.0 "not released", stale PORT.md "5% done", STRESS_RELATIONAL "COUNT undercount, not a bug", stale counts) | done [#826]: [#788] fixed most; [#826] cites 4.14.2.0 in `DIVERGENCES.md`, `SQL_ANSI_CONFORMANCE.md` and `FEATURE_MATRIX.md` and marks what is still 4.12-pinned | hn-compat-ws2 |
| H | No FDB cluster-upgrade story (single-protocol client) and no upgrade/downgrade guide | done [#788]: `docs/upgrade.md`, matrix in `docs/compatibility.md` | release-ws |

### Security and legal

| Sev | Item | Status | Workstream |
|---|---|---|---|
| B | Runner trust boundaries: job-started guard, contributor approval, provisioning preserves the guard | done [#788] (settings, manual sync): [#788] installs the guard in both provisioning modes; settings require approval for all external contributors. Instead of a re-roll, on 2026-10-11 ci-health synced master's `runner-job-guard.sh` to both live runners (gh-runner-drain-0, gh-runner-fdb), which already ran the job-started hook, and tested it on each box: push, own-repo PR and non-PR `issue_comment` admitted; fork PR, foreign repo, PR comment and `issue_comment` without an issue object denied | ci-health |
| H | `claude.yml` on self-hosted runners with write tokens | done [#788]: GitHub-hosted | ci-health |
| H | Tag/release protection; secret scanning; push protection; Dependabot; private vulnerability reporting | done (settings) | ci-health |
| H | Apache §4 attribution on ported files; NOTICE; trademark wording; third-party notices; SECURITY.md link | done [#788]: headers in 732 files, `NOTICE`; PR CI checks that `THIRD_PARTY_NOTICES.txt` is current | release-ws |

### Code health (screenshot material)

| Sev | Item | Status | Workstream |
|---|---|---|---|
| B | Comments that argue with earlier versions of themselves (36 files, some on public API) | done [#791] | hn-health-ws2 |
| H | Fresh-clone `go build ./...` fails (cgo `fdb_c.h`, a build-tag mismatch) | done [#807]: CI's `go install lane` builds `./...`. `go test ./...` compiles: done [#833] | hn-health-ws2 |
| H | 66 misattached doc comments | done [#791] fixes the API ones; done [#817] moves the rest | hn-health-ws2 |
| H | Census debt counters in production packages (e.g. `executor/leg_column_provenance_census.go`) | done [#836] | hn-health-ws2 |
| H | 18.6k-line `plandiff/corpus.go` compiled as non-test code | done [#831] | hn-health-ws2 |
| M | gomock mocks compiled into every sqldriver binary | done [#810] | hn-health-ws2 |
| M | Demo protos in the global registry | done [#842] | hn-health-ws2 |
| M | 121-module graph; 59 MB hello-world | not started (with D5) | hn-health-ws2 |

### Operations

| Sev | Item | Status | Workstream |
|---|---|---|---|
| H | Java conformance must gate merges | done (settings): required `Java conformance (Go vs Java 4.14.2.0)`; all six previous required checks and strictness preserved | ci-health |
| H | Limits (retries, timeouts, statement memory/rows) not settable from the DSN | done [#799]: DSN-settable timeouts and statement limits; no retry-limit parameter | hn-sql-ws3 |
| H | Unbounded default limits | not started: [#799] keeps Java's unbounded defaults; decide or document | hn-sql-ws3 |
| H | No CONTRIBUTING / SUPPORT / issue templates | done [#788]: `CONTRIBUTING.md`, `SUPPORT.md`, issue forms | release-ws |
| M | `go 1.26.9` patch-level directive; root v0.1.0 has no GitHub release (only `cmd/frl/v0.1.0` does) | in progress: v0.2.0 gets a GitHub release ([#825]). `go 1.26.0` is the dependencies' floor; `toolchain go1.26.9` preserves the tested build pin | release-ws |

## 3. Launch framing (from the HN-thread simulation)

**Title:** "Show HN: fdb-go – FoundationDB from Go without cgo, sharing data with Apple's Record Layer".

**Lead with:**
- interop: Java reads what Go writes;
- no cgo / static binaries;
- the external oracles CI checks against.

**Cut:**
- speed claims, until re-measured with a committed harness;
- feature laundry lists;
- "in production".

**Have ready:**

| Item | Status | Workstream |
|---|---|---|
| Re-measured performance with a committed harness (`/tmp/hn-perf` is gone) | in progress: the Go/CGo harness landed ([#822]); re-measurement pending | perf-lead2 |
| Architecture page | not started | release-ws |
| Compatibility table and FDB 8.0 stance | done [#788]: `docs/compatibility.md` | hn-compat-ws2 |
| Known gaps in one list | in progress: STATUS.md lists the five launch concerns and scoped mitigations in the v0.2.0 candidate | release-gpt |
| Answer on the N+1 question | not started: [#798] landed; FlatMap still open | hn-sql-ws3 |
| Answer on the 5 s per-RPC timeout question | not started: after the client timeout fixes | hn-client-ws2 |
| Answer on bus factor | not started | owner |

[#788]: https://github.com/birdayz/fdb-go/pull/788
[#789]: https://github.com/birdayz/fdb-go/pull/789
[#790]: https://github.com/birdayz/fdb-go/pull/790
[#791]: https://github.com/birdayz/fdb-go/pull/791
[#792]: https://github.com/birdayz/fdb-go/pull/792
[#798]: https://github.com/birdayz/fdb-go/pull/798
[#799]: https://github.com/birdayz/fdb-go/pull/799
[#800]: https://github.com/birdayz/fdb-go/pull/800
[#801]: https://github.com/birdayz/fdb-go/pull/801
[#803]: https://github.com/birdayz/fdb-go/pull/803
[#804]: https://github.com/birdayz/fdb-go/pull/804
[#805]: https://github.com/birdayz/fdb-go/pull/805
[#807]: https://github.com/birdayz/fdb-go/pull/807
[#808]: https://github.com/birdayz/fdb-go/pull/808
[#809]: https://github.com/birdayz/fdb-go/pull/809
[#810]: https://github.com/birdayz/fdb-go/pull/810
[#814]: https://github.com/birdayz/fdb-go/pull/814
[#815]: https://github.com/birdayz/fdb-go/pull/815
[#817]: https://github.com/birdayz/fdb-go/pull/817
[#822]: https://github.com/birdayz/fdb-go/pull/822
[#823]: https://github.com/birdayz/fdb-go/pull/823
[#825]: https://github.com/birdayz/fdb-go/pull/825
[#826]: https://github.com/birdayz/fdb-go/pull/826
[#831]: https://github.com/birdayz/fdb-go/pull/831
[#832]: https://github.com/birdayz/fdb-go/pull/832
[#833]: https://github.com/birdayz/fdb-go/pull/833
[#835]: https://github.com/birdayz/fdb-go/pull/835
[#836]: https://github.com/birdayz/fdb-go/pull/836
[#840]: https://github.com/birdayz/fdb-go/pull/840
[#842]: https://github.com/birdayz/fdb-go/pull/842
[#843]: https://github.com/birdayz/fdb-go/pull/843
[#844]: https://github.com/birdayz/fdb-go/pull/844
[#857]: https://github.com/birdayz/fdb-go/pull/857
[#882]: https://github.com/birdayz/fdb-go/pull/882
