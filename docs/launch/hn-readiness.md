# HN launch readiness (2026-10-09)

This plan comes from nine independent reviews: first look, Java parity, FDB client, public API, code health,
legal/security, performance, operations, and a simulated HN thread. Owner and status per row.
**B** = launch blocker. **H** = high. A blocker does not ship.

## 1. Owner decisions (cannot be delegated)

| # | Decision | Notes |
|---|---|---|
| D1 | ~~Revoke the leaked GitHub OAuth `client_secret`~~ **done 2026-10-09 (owner rotated all metrognome secrets)** | `examples/metrognome/config.yaml:7` (commit `6ad7ae7e5`). Its branches are deleted, but closed PRs #58 and #65 keep the refs forever. Treat it as compromised. |
| D2 | Git history: keep, strip binaries, or squash | 440 MiB pack; 55 MB `factory-migrate` binary. `v0.1.0` is already in the Go module proxy. Must be decided before forks exist. |
| D3 | Agent material in the public tree | `CLAUDE.md` ("ABSOLUTE PRIME DIRECTIVE"), `.claude/` (Graefe/Torvalds personas, 24/7 shifts), `shifts/`, 19 root `.md` files, and real people as reviewer names in RFCs, commits and tests. |
| D4 | AI-assisted disclosure | The HN-thread simulation's top comment is commit forensics. Disclose first, in your own words, and point at the oracles: Java conformance, the libfdb_c differential, the binding tester. |
| D5 | Shrink the public API (about 88% of 11.5k exported identifiers should be internal) | Planner, simfdb, dst, conformance, gomock mocks and test seams are importable. Hard to take back after launch. |
| D6 | Bigger CI boxes / shared-cache write auth | The cache is unauthenticated; a trusted-write split needs a server change (see S5). |

## 2. Blockers and highs, by workstream

### Product correctness and performance
| Sev | Item | Owner |
|---|---|---|
| B | ~1 ms per transaction from the GRV batch timer (Go netpoller rounding). Get 1.22 ms vs CGo 0.21 ms; whole stack inherits it. | fix-grv |
| H | Tenant `commit_unknown_result` fence uses the unprefixed key, so a commit can land after 1021 is returned | fix-tenant-fence |
| H | A GRV wait over 30 s returns a non-retryable `DeadlineExceeded` during recoveries | fix-grv |
| H | Endpoint-not-found dropped; 5 s per-RPC timeouts libfdb_c lacks; commit ignores ctx and timeout; 38 KB per read-only transaction; lazy futures; APIVersion above 730 accepted | fix-clientmisc |
| H | No real-cluster fault testing (recovery, kill, partition, coordinators); binding stress is only 1×100 | fix-faultsuite |
| B (for SQL perf claims) | Plan-cache key includes literals and `?` values, so prepared statements always re-plan (3 ms per lookup) | fix-plancache |
| H | Planning costs milliseconds; a 6-table join takes 5.6 s with no error; no wall-clock budget | fix-planbudget |
| M | Serial index→record fetch and FlatMap (the N+1 question) | fix-fetchpipe |
| H | Collated indexes silently diverge from Java bytes (data corruption when shared) | fix-collation |

### First impression
| Sev | Item | Owner |
|---|---|---|
| B | Every quickstart fails (domain registration, `NOT NULL`, DSN case, unconfigured docker FDB, swallowed errors, typed-store snippet) | fix-quickstart |
| B | "2–4× faster" claims are false (pre-RFC-104 GRV cache; one-way netem labelled as RTT) | fix-perfclaims |
| B | `go get` gives v0.1.0, 682 commits behind and storage-incompatible; `go install …/cmd/frl@latest` resolves to the stale nested module | release (after fixes): cut v0.2.0, retract `cmd/frl` v0.1.0 |
| H | macOS: `frl fdb up` uses `--network host`; the README cluster file uses the container IP | open |
| H | Positioning: what it is, and the "unofficial / not Apple" disclaimer | fix-legal |
| M | Library logs INFO to stderr on every open | fix-quickstart |

### Honesty of docs
| Sev | Item | Owner |
|---|---|---|
| B | No single production-readiness answer. road-to-prod "CONFIRMED" rests on deleted nightlies, which were 0/30 green before deletion. | open: write STATUS.md, archive the rest |
| B | Wire-compat headline without exceptions (collation, vector_spfresh/defaults, Lucene, spatial, synthetic types, SQL continuations, TEXT on non-Latin scripts, format-14 header upgrade, v0.1.0 SQL keyspace) | open: compatibility page |
| H | 20 doc contradictions (4.12.11 vs 4.14.2.0, FDB 8.0 "not released", stale PORT.md "5% done", STRESS_RELATIONAL "COUNT undercount, not a bug", stale counts) | open |
| H | No FDB cluster-upgrade story (single-protocol client) and no upgrade/downgrade guide | open: `docs/upgrade.md` + compatibility matrix |

### Security and legal
| Sev | Item | Owner |
|---|---|---|
| B | Fork PRs could reach persistent root runners | **done**: runner job-started guard (both boxes) + approval for all outside contributors |
| H | `claude.yml` on self-hosted runners with write tokens | **done** (PR #788): GitHub-hosted |
| H | Tag/release protection; secret scanning; push protection; Dependabot; private vulnerability reporting | **done** (settings) |
| H | Apache §4 headers on ~288 ported files; NOTICE; trademark wording; third-party notices; SECURITY.md link | fix-legal |
| M | Unused high-impact repo secrets (`HCLOUD_TOKEN`, `BAZELSCALESET_*`); no Hetzner firewall; cache poisoning by trusted-branch jobs | open (S5) |

### Code health (screenshot material)
| Sev | Item | Owner |
|---|---|---|
| B | Comments that argue with earlier versions of themselves (36 files, some on public API) | open |
| H | Census debt counters in production packages; 18k-line `corpus.go` as non-test code; 66 misattached doc comments; fresh-clone `go build ./...` fails (cgo `fdb_c.h`, a build-tag mismatch) | open |
| M | gomock mocks compiled into every sqldriver binary; demo protos in the global registry; 121-module graph; 59 MB hello-world | open (with D5) |

### Operations
| Sev | Item | Owner |
|---|---|---|
| H | Java conformance is not a required check | open (settings, once #788 merges) |
| H | Unbounded default limits (retries, timeouts, statement memory/rows), not settable from the DSN | open |
| H | No CONTRIBUTING / SUPPORT / issue templates; bus factor 1 | open |
| M | `go 1.26.9` patch-level directive; v0.1.0 has no GitHub release | release |

## 3. Launch framing (from the HN-thread simulation)

**Title:** "Show HN: fdb-go – FoundationDB from Go without cgo, sharing data with Apple's Record Layer".

**Lead with:**
- interop: Java reads what Go writes;
- no cgo / static binaries;
- the external oracles CI checks against.

**Cut:**
- speed claims, until re-measured with the harness in `/tmp/hn-perf` (to be committed);
- feature laundry lists;
- "in production".

**Have ready:**
- an architecture page;
- a compatibility table;
- an FDB 8.0 stance;
- known gaps;
- answers on the N+1/5 s question and on bus factor.
