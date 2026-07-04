---
name: dst
description: How to hunt bugs and reproduce failures in the FDB record-layer Go port with the DST toolbox — chaos, fuzz, the libfdb_c differential, SQL/binding stress, and dlv/rr replay — plus what each RFC-179 DST tier unlocks. Living doc: a symptom→tool decision table, verified reproduce-from-seed recipes, and how to pin every bug with a regression. Invoke for bug hunting, flake root-causing, "reproduce this seed", or "what can I test at this stage".
---

# DST — deterministic simulation testing & bug hunting

The practical guide to finding a bug, reproducing it, and pinning it, using what this repo has
**today** — and what each RFC-179 tier will add. Read [`rfcs/179-deterministic-simulation-testing.md`](../../../rfcs/179-deterministic-simulation-testing.md)
for the design; this skill is the *usage* manual.

## Honest status (read this first)

**SimFDB (RFC-179 Tiers 0–2) is built.** The record/relational stack now runs over a deterministic
in-memory FDB backend (`pkg/simfdb`) with a seeded Env (`pkg/dst`: sim clock + seeded RNG + Buggify),
so a single `uint64` seed replays the **record-layer** run bit-exactly — no Docker, no cluster — and
the fault schedule (commit_unknown/conflict/too_old) is part of that seed with **true rollback** (unlike
chaos's double-commit fake). The brute-force loop-until-bug hunter that rides on this is
[`pkg/simfdb/hunt`](#hunt). So:

- **Seed-reproducible today, no Docker:** the **hunt** harness (`hunt.Run(seed)` over the real record
  layer + all 7 index types), plus fuzz-corpus entries (`go test -run=FuzzX/<hash>`) and binding-stress
  (`-seed-start N`).
- **Partially reproducible:** chaos `RunRandom` and stress replay the same *op sequence / dataset* but
  **not** the fault interleaving or server versions — real FDB + wall-clock underneath. (For the record
  layer, prefer **hunt** when you want the full-fidelity seed replay.)
- **Not built:** the **client-transport** bit-exact replay (a whole simulated network/disk under the
  pure-Go client) — that's **Track B**, a separate RFC. SimFDB simulates the *backend contract*
  (`fdb.BackendDatabase`), not the wire transport.

When you say a bug is pinned, be precise about *which* replay you have: a hunt seed reproduces the
record-layer run exactly; a chaos seed reproduces only the op stream.

## The two axes (how to think about any tool here)

Every tool sits on two axes. Pick the one that matches your bug.

| | **In-memory / no Docker** | **Real FDB (testcontainers, Docker)** |
|---|---|---|
| **Seed-reproducible** | **hunt** (record layer over SimFDB); fuzz targets (planner, tuple, RYW, wire, parse) | binding-stress `-seed-start`; fuzz-corpus replay of a `FuzzDifferential*` crash |
| **Not (or partially) reproducible** | — | chaos, SQL stress, `-race` loops, the differential battery |

The rule (from RFC-179 §3a): **intercept as high as you can while still covering the code you care
about.** In-memory + seeded is the fast inner loop; real-FDB is the fidelity gate. SimFDB moved the
record layer into the top-left cell — hunt there first, then confirm anything wire-adjacent on real FDB.

## Symptom → tool (start here)

| Symptom | Reach for | Section |
|---|---|---|
| "Find me *any* record-layer bug" — brute-force loop-until-bug, no Docker, seed-reproducible | **hunt** (`hunt.Run` / `TestBruteHunt` / `FuzzHunt`) | [hunt](#hunt) |
| Index/aggregate count or sum drifts; retry corrupts derived state | **chaos** (`RunRandom` + fault injection), or **hunt** for the seed-exact replay | [chaos](#chaos) / [hunt](#hunt) |
| Panic / OOB slice on malformed bytes; decode never round-trips | **fuzz** (tuple/wire/parse/RYW) | [fuzz](#fuzz) |
| Same query/plan differs run-to-run; memo grows on re-explore | **fuzz** (`FuzzPlanner_*`, subprocess) | [fuzz](#fuzz) |
| Go client persists different bytes / different commit outcome than the C binding | **differential** (`FuzzDifferential*`) | [differential](#differential) |
| A binding-tester op diverges from the FDB spec | **binding-stress** | [binding-stress](#binding-stress) |
| Wrong rows / silent truncation / plan-shape regression at scale; perf regressed | **SQL stress** (+ worktree baseline) | [stress](#stress) |
| Intermittent race / conflict flake (`not_committed 1020`, `commit_unknown`) | **`-race` loop-until-fail** + knobs | [replay & knobs](#replay--knobs) |
| Need to root-cause a captured failing run step-by-step | **dlv** (today) / **rr+dlv reverse** (setup) | [replay & knobs](#replay--knobs) |

**Then always:** reproduce it ([reproduce-from-seed](#reproduce-from-seed)) → fix the *code*, never the
test expectation → **pin it with a regression** ([pin the bug](#pin-the-bug)). A green suite with the bug
still latent is the real danger.

---

## chaos

Model-based oracle (RFC-002, `pkg/recordlayer/chaos/`): an in-memory `StoreModel` shadows a real FDB
store; every op hits both; `Verify()` cross-checks **14 invariant families** (COUNT/SUM/MIN-MAX_EVER/
PERMUTED_MIN-MAX/RANK/MULTIDIMENSIONAL/VERSION/VECTOR/SPFresh/BITMAP/TEXT + record & scan-count). Faults
(`commit_unknown 1021` / `conflict 1020` / `too_old 1007`) are injected at the tx boundary via
`ChaosTransactor` to hunt **non-idempotent index maintenance under retry** — the headline bug class.

```sh
# Run one scenario (PLAIN GO TESTS — use --test.run, NOT --ginkgo.focus; anchor with $)
bazelisk test //pkg/recordlayer/chaos:chaos_test --test_arg="--test.run=TestRandomMultiIndex$" --test_output=streamed
# Fast local loop (needs Docker). -parallel 1 is MANDATORY (BUILD bakes it; default parallelism trips a client bug, not yours)
go test ./pkg/recordlayer/chaos/ -run TestMaxEverCommitUnknown -count=1 -parallel 1 -v
```

- **Reproduce:** a violation fatals `chaos: <n> violation(s) at op <M> (seed=<S>):` + a per-invariant
  line. The seed is a **compile-time literal** (`WithSeed(S)` / `RandomConfig{Seed: S}`), not a CLI
  flag. `RunRandom` is single-goroutine off `rand.NewPCG(seed,0)` with de-randomized map order, so
  re-running the same test replays the op stream and fails at the same op **M** — add
  `--nocache_test_results` or Bazel serves the cached result without executing. Set `VerifyEvery: 1`
  (default 50) to pin the exact first divergence; shrink `NumOps` toward M to bisect.
- **`RunConcurrent` is NOT replayable** (per-worker seeds, wall-clock pacing, real-FDB conflict retries).
  Brute-force with `--runs_per_test=50`.
- **Gotcha:** needs Docker (`TestMain` spins a container; no mock). `fault.go:24-29` fakes conflicts as
  double-commit-then-re-execute; for **true rollback** (a faulted tx leaves zero trace) use **hunt**
  over SimFDB. chaos remains the real-FDB fidelity gate; hunt is the fast seeded inner loop.

## hunt

Brute-force, seed-driven bug hunter for the record layer (`pkg/simfdb/hunt/`, RFC-179 Tier 2). It drives
the **same op vocabulary as chaos `RunRandom`** (save/overwrite/delete/deleteAll over the 7-index
kitchen-sink schema: VALUE+COUNT+SUM+RANK+MAX_EVER+VERSION+COVERING) but over **SimFDB** — in-process,
no Docker — under a **seed-derived commit-fault schedule** (Buggify fires 1021/1020/1007 with true
rollback), checking `chaos.Verify` after every batch. A bug is a single `uint64`: `hunt.Run(seed)`
replays it, `hunt.Shrink(seed, cfg)` minimizes it. This is the "run in a loop until a bug drops out"
tool — the brute-force complement to chaos (one fixed seed, real FDB).

```sh
# Loop-until-bug: time-budgeted parallel sweep, full 300-op profile (the overnight hunt).
bazelisk test //pkg/simfdb/hunt:hunt_test --test_arg=-test.run=TestBruteHunt \
  --test_env=HUNT_SECONDS=3600 --test_env=HUNT_WORKERS=8 \
  --test_timeout=4000 --test_output=streamed --nocache_test_results
#   HUNT_SEED_START=N shards disjoint seed space across machines. First failing seed → full reproducer.

# Coverage-guided fuzzer (libFuzzer mutates the seed; crashers saved to testdata/fuzz):
bazelisk test //pkg/simfdb/hunt:hunt_test --test_arg=-test.fuzz=^FuzzHunt$ \
  --test_arg=-test.fuzztime=120s --test_arg=-test.fuzzcachedir=/tmp/fuzz-cache \
  --sandbox_writable_path=/tmp/fuzz-cache

# Replay ONE suspect seed with per-op verify (pins the exact failing op):
bazelisk test //pkg/simfdb/hunt:hunt_test --test_arg=-test.run=TestHuntSeed \
  --test_env=HUNT_SEED=1099511627776 --test_arg=-test.v --nocache_test_results
```

- **Reproduce & shrink:** a failing `Report` prints `seed=<S> FAILED after <N> ops` + each violation +
  the exact replay command. `hunt.Shrink(S, cfg)` scans up from 1 op to the **minimal failing prefix**
  (verify-every-op) so the reproducer is the fewest ops that still trip the oracle;
  `hunt.FaultDependent(S, cfg)` tells you if the bug needs faults (a retry/idempotency defect) or
  reproduces on the happy path (a plain, more serious, logic bug).
- **Oracle stack (what gives it teeth):** (1) `chaos.Verify` after every `VerifyEvery` ops — index↔record
  consistency across all 7 maintainers; (2) **determinism** — same seed ⇒ byte-identical `Fingerprint`
  (sha256 of the whole keyspace) + identical fault count, catching any wall-clock / `crypto/rand` /
  map-order leak; (3) an **unexpected op error** on this UNIQUE-free schema is itself a bug (faults are
  retried transparently, so nothing should surface). `TestOracleHasTeethOverSimFDB` proves Verify catches
  an injected store/model divergence, so a green sweep is meaningful, not a no-op.
- **Throughput & knobs:** ~470 ms per full 300-op × 7-index run (dominated by per-op store re-open +
  RankedSet), ≈17 seeds/s on 8 cores → ~1.5M seeds/day. `Config{NumOps, MaxPKs, VerifyEvery, FaultProb,
  Metadata}` — smaller `MaxPKs` = more overwrite/conflict density; swap `Metadata` to a single-index
  builder for far higher throughput when hunting one maintainer. The in-suite `smokeCfg` (120 ops, few
  seeds) keeps `just test` ~8 s; real hunts use `HUNT_SECONDS`/`FuzzHunt`.
- **What it has already found:** (1) the SimFDB API-version precondition gap (versionstamp writes failed
  `api_version_unset 2200` — `simfdb.New` now selects 730); (2) a **real record-layer wire bug** — the
  SQL workload's determinism oracle caught non-deterministic record-body serialization: the slow-path
  `proto.Marshal(record)` in `store.go serializeUnion` iterated a *dynamic* message's fields in map order,
  so the same SQL row persisted to different bytes each write (and diverged from Java's ascending
  field-order). Fixed with `Deterministic:true`. Both found on the first relevant run.
- **Workloads (add a surface over time).** A `Workload` (interface in `hunt.go`: `Name()` + `Run(seed,
  cfg) *Report`) is the extension point — implement one, drop it in a `Config`, and the whole loop /
  fault schedule / shrink / recording works unchanged. Two exist: the default **record-layer** workload
  (`recordWorkload`, chaos vocab + `chaos.Verify`) and **`sqlhunt.SQLWorkload`** (`pkg/simfdb/hunt/sqlhunt`)
  which drives the full parser→Cascades→executor stack over SimFDB via `sqldriver.RegisterBackend` +
  `sql.Open("fdbsql", …cluster_file=<key>)`, runs idempotent DML (absolute `UPDATE`+`DELETE`) under the
  fault schedule, and compares the table to a Go row-model. To add one: new package, implement `Workload`,
  build the seeded env with `hunt.NewSimEnv`, fingerprint with `hunt.Fingerprint`, export `Profiles()`,
  and add it to the `profiles` slice in `cmd/dst-hunt/main.go`. Design an oracle that avoids KNOWN-hazard
  noise (the SQL workload excludes bare `INSERT` / relative `UPDATE` — their commit_unknown
  non-idempotency is a known Java-matching hazard, not a hunt target).
- **Gotcha:** hunt exercises the **record + relational** layers (the `fdb.BackendDatabase` contract),
  not the wire transport — after a hunt finds a bug, confirm anything key-encoding/wire-adjacent on real
  FDB (chaos/differential) before calling it wire-safe. (The serialization fix above *is* wire-adjacent —
  it was cross-checked against Java's field ordering.)

## interleave (concurrent-transaction SSI driver)

Concurrent-open-transaction driver (`pkg/simfdb/hunt/interleave/`, RFC-179 Tier 2, a `hunt.Workload`).
Every other hunt workload is single-writer, so SimFDB's SSI resolver never resolved a real conflict —
`1020` never fired. This one holds N transactions open, interleaves **RMW-increment** (`Get`+`Set`, a
read conflict) and **atomic-add** (`Add`, no read conflict) ops through one deterministic goroutine, and
commits through the serialized resolver. That makes `not_committed(1020)` fire for real: **5,317 across
2,000 pure-RMW seeds**, and **zero** on the pure-add profile (the "why atomic ops exist" contrast).

```sh
bazelisk test //pkg/simfdb/hunt/interleave:interleave_test --test_output=errors
# TestConflictsFire logs the live 1020 count; TestClean sweeps every profile × 3000 seeds.
# Runs inside the overnight sweep too: dst-hunt -profiles interleave
```

- **Two intrinsic oracles, no external reference.** *Verdict:* independently recompute the SSI verdict
  from point-key read/write sets + a model version counter kept in lockstep with SimFDB's `lastVersion`
  (pin the read version at the first `Get`, exactly as SimFDB's lazy GRV does) — catches a **missed
  conflict** (committed a lost update) AND a **spurious abort**; the point-set-vs-byte-range gap is its
  teeth against the resolver's range arithmetic. *State:* retry-drain every abort so each program applies
  exactly once, then assert the final keyspace equals the **seed-derived** sum of every program's effect
  (increment +1, add +delta) — catches lost updates, rolled-back-write leaks, atomic-add miscounts.
- **Faithfulness rule:** the verdict oracle only works because the model tracks read-version pinning and
  commit-version bumps in lockstep (single goroutine, serialized commits). If you extend the op set, keep
  the model's read/write-set bookkeeping exact — a model drift shows up as a false `verdict oracle` diff.
- **Fault mode (`Faults > 0`):** activates SimFDB's commit BUGGIFY so `commit_unknown(1021)` (writes
  durable, outcome ambiguous) races the other open transactions — a dimension the single-writer record
  workload can't reach. The **state oracle is unchanged** (every program still applies exactly once: a
  1021 applied and is NOT retried — retrying would double-apply its atomic adds; a 1020/1007 applied
  nothing and is retry-drained). The **verdict oracle goes one-directional**: the resolver runs BEFORE
  fault injection (`conflict.go`), so a real conflict still surfaces as `1020`, but an injected fault on
  a clean txn isn't predicted — assert only the missed-conflict direction. `TestFaultsApply1021` proves
  the 1021 path fires (1,017 applied-and-committed across 4,000 seeds) — it's what shows a 1021 txn
  correctly PARTICIPATES in later conflict detection and applies exactly once. Profiles:
  `interleave-faults`, `interleave-faults-add`.

## rangeconflict (range-aware SSI driver)

The interleave driver only makes *point-width* conflict ranges, so the resolver's arithmetic over
genuine **spans** (`rangesOverlap` on wide ranges, the GetRange read-conflict extent, ClearRange write
conflicts, the `keyAfter` boundary between adjacent keys) went untested. This driver
(`pkg/simfdb/hunt/rangeconflict/`, RFC-179 Tier 2, a `hunt.Workload`) interleaves point AND range ops
— `GetRange` (read conflict over a span), `ClearRange` (write conflict over a span), point Get/Set/
Clear — across open transactions and commits through the serialized resolver. **9,238 real `1020`s +
30,417 range ops across 3,000 hot seeds**, verdict model in lockstep.

```sh
bazelisk test //pkg/simfdb/hunt/rangeconflict:rangeconflict_test --test_output=errors
# dst-hunt -profiles rangeconflict
```

- **The oracle is airtight because byte-range overlap ≡ integer-interval overlap** for ascending
  tuple-encoded keys: point key `i` = `[i, i+1)`, range `[lo,hi)` = `[lo,hi)`, `keyAfter(k_i)` sorts
  strictly between `k_i` and `k_{i+1}` so it never bridges a gap. The verdict model resolves over plain
  integer intervals (simpler than the resolver's byte ranges — that gap is its teeth); the state oracle
  replays blind Set/Clear writes in commit order.
- **Unlimited `GetRange` reads** keep the read-conflict extent the full span (SimFDB narrows it only for
  a limit-truncated read) — so the interval model matches the resolver exactly. If you add limited range
  reads, model the narrowed extent too.
- **GOTCHA the clean sweep caught (read-only fast path):** a transaction with **no writes** never
  reaches conflict resolution — FDB completes a read-only commit client-side (SimFDB `conflict.go`
  short-circuits when `buffer` and `writeConflicts` are both empty), taking version `-1` (no
  `lastVersion` bump). A verdict model MUST treat a read-only txn as never-conflicting AND must not
  advance its model version on that commit, or every later verdict drifts. The interleave driver never
  hit this (every op there writes); rangeconflict did on its first sweep. When a new driver's model
  tracks a version counter, replicate the read-only fast path.

## continuation (cursor-resume replay driver)

Continuation-under-fault replay driver (`pkg/simfdb/hunt/continuation/`, RFC-179 Tier 2, a
`hunt.Workload`). A scan that hits a row/byte/time limit mints a **continuation token** and the
caller resumes in a fresh transaction from those bytes — every byte is a place a bug hides (page-
boundary off-by-one, plan-hash-in-token, resume skip/dup, version-path-dependence). The record layer
has *point* tests for a few cases (`continuation_stability_test.go`); this is the seeded sweep that
fuzzes the whole space over SimFDB.

The oracle is an **independent Go model**, not the engine judging itself: for a seeded record set the
scan order is known a priori (records in primary-key order, a VALUE index in `(indexed-value, pk)`
order), so it's computed straight from the `(pk, price)` pairs. Three airtight checks: **pagination
equivalence** (concatenated pages == model at *every* page size — size 1 round-trips every row
through a token, the hard case), **prefix-delete invariance** (delete an already-scanned key mid-scan
→ tail untouched), **tail-delete reflected** (delete a not-yet-scanned key → tail == model minus that
row).

```sh
bazelisk test //pkg/simfdb/hunt/continuation:continuation_test --test_output=errors
# in the overnight sweep: dst-hunt -profiles continuation
```

- **Why an independent model, not metamorphic self-comparison:** a metamorphic "paginated == one-shot"
  check misses a bug both paths share (e.g. a consistently wrong scan order). The model is proven
  faithful by `TestModelMatchesEngine` (a one-shot engine scan of a known dataset == the model), so a
  `TestClean` divergence is a real engine bug, not a wrong expectation.
- **Non-advancing token guard:** `drainWithLimit` caps pages at `4*(expected+1)+16` — a token that
  never advances is reported as a violation, not an infinite loop.
- **Fresh-tx-per-page is the point:** each page runs in its own `db.Run`, so the token must survive a
  transaction/read-version boundary. The between-page delete (a committed version bump) is the "fault".

## sqlpage (SQL-query pagination oracle)

Goes a layer above the raw-scan `continuation` driver: it paginates whole SQL QUERIES
(`pkg/simfdb/hunt/sqlpage/`, RFC-179 Tier 2, a `hunt.Workload`), so the **executor's cursor tree** —
filter/sort/GROUP BY/join — and its per-operator continuation logic is what's under test. It runs each
query on one pinned connection at the default execution scanned-rows limit (no pagination = reference)
and again at a tiny limit (forcing the executor to exhaust its budget and resume through internal
continuations over and over), and asserts the two results match (multiset, or ordered for a total
ORDER BY). Metamorphic — the engine judges itself.

```sh
bazelisk test //pkg/simfdb/hunt/sqlpage:sqlpage_test --test_output=errors
# dst-hunt -profiles sqlpage   (SQL runs are ~1-2/sec/core — far slower than the raw drivers)
```

- **Mechanism:** `conn.Raw(func(dc any){ dc.(*embedded.EmbeddedConnection).SetOptions(
  api.NewOptionsBuilder().Set(api.OptExecutionScannedRowsLimit, N).Build()) })` on a pinned `*sql.Conn`,
  then query — the driver auto-resumes across internal continuations to a full result, so paged==unpaged
  must hold. This is the same knob the `flatmap_continuation_drop_fdb_test` uses.
- **FOUND TWO BUGS** (both executor-continuation gaps, both TODO.md "## DST findings", both pinned by
  fix-detector tests + quarantined out of the sweep): (1) streaming `DISTINCT` drops its dedup state
  across a continuation resume — returns every row under a tiny scanned-rows limit, even with ORDER BY,
  while `GROUP BY` stays correct (`TestKnownBug_DistinctContinuation`); (2) multi-value `IN (a,b)` (an
  InJoin over a concat of per-value scans) has **no per-branch continuation** — the concat errors
  `54F01` under a tiny limit instead of resuming, so pagination changes whether the query executes
  (`TestKnownBug_InJoinContinuation`; same gap in `executeInUnion`; Java's `InJoinCursor` resumes). Both
  reachable in prod when the scan exceeds the txn/scanned-rows budget. The lesson: **an operator whose
  continuation doesn't serialize its per-operator state (DISTINCT's seen-value, concat's branch cursor)
  is a bug the moment it paginates mid-stream — this oracle finds exactly that class.**
- **Building a query-pagination oracle:** make total-order queries `ordered:true` (append the PK so
  ties are deterministic — else a legit tie-order difference reads as a false drop); a query that errors
  unpaged is unsupported SQL → skip it, but a query that errors ONLY when paged is a finding.

## golden (characterization gate)

Behavior lock for the SQL engine (`pkg/simfdb/hunt/golden/`, RFC-179 Tier 2 oracle). For a curated
corpus of (schema, seeded data, queries) it captures the engine's **`EXPLAIN` plan + result rows** over
SimFDB into a committed baseline (`testdata/*.golden`) and diffs a fresh capture every run. Catches
**CHANGED, not WRONG** — a silent plan/result regression (right rows, wrong/slower plan — the PR #201
class) that *no result-based or metamorphic test sees*. It's the merge/release gate: the diff is the
review artifact. Only trustworthy because SimFDB makes engine output deterministic — verified byte-stable
across 160+ fresh processes, `-race`, and every `GOMAXPROCS`.

```sh
just golden           # check current behavior vs the committed baseline (also runs in `just test`)
just golden-update    # after an INTENTIONAL engine change: regenerate, then review the diff
git diff pkg/simfdb/hunt/golden/testdata/   # THE review artifact
```

- **Reviewing a golden diff (author + Graefe):** a **result-row** change is loud → almost always a bug;
  a **plan** change with identical rows is a query-engine change → intended optimization or a regression,
  reviewed with its cost delta (a `IndexScan(X)` → `Scan(T)` swap is a red flag). A PR that changes engine
  behavior *must* regenerate + commit the baseline, or `just test` fails — that's the gate.
- **Add coverage:** append a `Scenario` to `corpus()` in `golden_test.go` (new schema / index / query
  shapes — joins, composite PK, aggregate indexes are in already), then `just golden-update`. Keep it
  curated (not a random dump) so the baseline stays reviewable. A query that can't plan shows as
  `PLAN-ERR`/`ROWS-ERR` in the golden — fix or drop it, never bake an error into the baseline.
- **Limit:** locks *current* behavior, right or wrong — pair it with the metamorphic/model oracles (which
  catch WRONG). Golden alone would keep a day-one bug green.

## fuzz

125 targets across 61 files. Property/no-panic fuzzers: wire byte-identity to libfdb_c, tuple/RYW/wire
decode robustness, and **Cascades planner determinism/idempotence/memo-consistency**. A fuzz target
proves an *invariant* over adversarial in-process inputs — it does **not** prove a query returns the
right rows (that's yamsql + FDB integration tests). Most are pure in-memory (no Docker); the
`FuzzDifferential*` family needs Docker.

```sh
grep -rE "^func Fuzz" pkg/                                    # enumerate every target
# Find crashes (in-memory target; DOTTED flags under bazel; anchor -test.fuzz with ^…$)
bazelisk test //pkg/recordlayer/query/plan/cascades:cascades_test \
  --test_arg='-test.run=^$' --test_arg='-test.fuzz=^FuzzPlanner_Determinism$' \
  --test_arg='-test.fuzztime=30s' --test_arg='-test.fuzzcachedir=/tmp/fuzz-cache' \
  --sandbox_writable_path=/tmp/fuzz-cache
# Or plain go (UNDOTTED flags, no Docker/Bazel):
go test -run='^$' -fuzz='^FuzzUnpack$' -fuzztime=30s ./pkg/fdbgo/fdb/tuple/
```

- **Highest-value targets for bug hunting:** `FuzzDifferential` / `FuzzDifferential_TuplePack` (wire
  byte-identity vs C — the wire hard line), `FuzzRYWCache` ("most important target for the pure-Go
  client — any bug = silent data corruption"), `FuzzPlanner_Determinism` / `_Idempotence` /
  `_MemoConsistency`, `FuzzParse` (never panic; failures must be `*api.Error{SyntaxError}`), `FuzzUnpack`
  (never panic + `Pack∘Unpack` fixpoint).
- **Reproduce:** a crash is minimized to `<pkg>/testdata/fuzz/<Fuzz>/<hash>` (two lines: `go test fuzz
  v1` + `type(value)`). Replay with **`-run`, not `-fuzz`**: `go test -run='FuzzX/<hash>' ./pkg/<path>/`.
  Verified live: `go test -run='FuzzPlanner_Determinism/seed1' ./pkg/recordlayer/query/plan/cascades/`.
- **Gotchas:** two flag namespaces — `bazelisk` uses **dotted** `-test.fuzz`, plain `go test` uses
  **undotted** `-fuzz`; mixing silently no-ops. `-fuzz` refuses >1 match (anchor `^…$`). On packages
  with a Ginkgo suite add `-test.run='^$'`. Under Bazel, new crashers land in the sandbox — copy into
  source `testdata/fuzz/` and commit. Bazel replay only works if the `go_test` globs `testdata/**`
  (cascades/recordlayer/parser do; tuple/client/wire do **not** — add the glob before Bazel replays).

## differential

The go-vs-C oracle (`pkg/fdbgo/libfdbc` + `pkg/fdbgo/bench`): same ops through the **pure-Go client** and
Apple's **libfdb_c** against one real container, asserting byte-identical persisted state / identical
commit-abort / identical reads. **libfdb_c is the spec.** This is exactly the 2-backend harness SimFDB
(Tier 1) plugs into as a 3rd oracle.

```sh
# Bench differentials (part of `just test`; links libfdb_c, spins a container in TestMain)
bazelisk test //pkg/fdbgo/bench:bench_test --test_output=errors
# Conflict-outcome oracle as an active fuzz (RFC-121: go-vs-cgo commit/abort agreement)
mkdir -p /tmp/fuzz-cache && bazelisk test //pkg/fdbgo/bench:bench_test \
  --test_arg=-test.run='^$' --test_arg=-test.fuzz='^FuzzDifferential_ConflictOutcome$' \
  --test_arg=-test.fuzztime=90s --test_arg=-test.fuzzcachedir=/tmp/fuzz-cache \
  --sandbox_writable_path=/tmp/fuzz-cache --nocache_test_results --test_output=streamed
# Record-layer wire gold gate (RFC-109) — INVISIBLE to bazel/`just test`; go+tag only
CGO_ENABLED=1 go test -tags libfdbc -count=1 -timeout 30m -v ./pkg/fdbgo/libfdbc/... ./pkg/internal/fdbclient/...
```

- **Catches:** wire-format byte divergence (records/indexes/inline versions/split records), cross-backend
  read incompat, atomic-op width/opcode bugs, **read-conflict-set divergence** (Go under-conflicts →
  lost serializability, the catastrophic direction — `FuzzDifferential_ConflictOutcome`), GetKey/RYW
  selector resolution, GetRange chunking-invariance, tuple byte-identity.
- **Reproduce:** a `FuzzDifferential*` crash writes `pkg/fdbgo/bench/testdata/fuzz/<Fuzz>/<hash>` (auto-
  replays via `data = glob(testdata/**)`). Replay one: `CGO_ENABLED=1 go test
  -run='FuzzDifferential_ConflictOutcome/<hash>' ./pkg/fdbgo/bench/`. Both clients are pinned to one
  shared read version, so a genuine divergence replays every time (transient `1007` is retried, never a
  false mismatch).
- **Gotchas:** needs **libfdb_c installed + cgo + Docker** (even *building* `bench_test` fails to link
  without libfdb_c). Container needs `WithDirectIP()` (libfdb_c asserts `canonicalRemotePort`). The
  record-layer gold gate runs **only** via `go test -tags libfdbc` — `bazelisk test
  //pkg/fdbgo/libfdbc/...` does NOT exercise it, so green bazel ≠ wire gate passed.

## stress

Million-row SQL suite (`//pkg/relational/sqldriver/stress:stress_test`): bulk-inserts a seed-42 dataset
into real FDB, runs ~25 query subtests logging durations and asserting **row-count correctness** + a few
`EXPLAIN` plan-shape pins. The eyeball perf gate — run before/after any planner/cost/executor change
against a master worktree baseline.

```sh
bazelisk test //pkg/relational/sqldriver/stress:stress_test --test_output=streamed \
  --test_arg="--test.run=TestFDB_Stress_1M$" --test_arg="--test.v"
# Fresh perf numbers (defeat the result cache) + generous timeout:
#   … --test_arg="--test.timeout=600s" --test_timeout=600 --cache_test_results=no
# Baseline-vs-branch compare:
git worktree add /tmp/fdb-master master && cd /tmp/fdb-master \
  && bazelisk test //pkg/relational/sqldriver/stress:stress_test --test_output=streamed \
     --test_arg="--test.run=TestFDB_Stress_1M$" --test_arg="--test.v"; \
  git worktree remove /tmp/fdb-master --force
```

- **Catches:** silent row truncation (COUNT==n asserts — continuation/cursor/5s-timeout bugs), aggregate-
  index correctness, plan-shape regressions under load (`full_scan_sparse_filter` must NOT use an index),
  point-lookup complexity blowups, throughput regressions (manual log diff — thresholds in CLAUDE.md are
  eyeball, not enforced).
- **Reproduce:** no seed flag — dataset is `rand.NewSource(42)` (fixed). Re-run the same test; the
  **correctness** asserts (row counts, EXPLAIN shape, affected rows) reproduce deterministically;
  **throughput** does not (multi-goroutine, wall-clock ingest — the exact non-replayability SimFDB Tier 2
  replaces). Iterate on a correctness bug with `TestFDB_Stress_10K` (shares the suite body; 10K actually
  exercises *more* query shapes — joins/exists gate on n≤10000).
- **Gotchas:** tagged `manual`+`stress`, so `just test` and `bazel test //...` **never** run it — name
  the target. No-Docker = `os.Exit(0)` fake green (confirm the container started in logs). `--test_output=streamed`
  is required to see durations. Cached passes measure nothing — add `--cache_test_results=no` for perf.

## binding-stress

The official FDB binding-tester across many seeds (`just binding-stress`, `//cmd/fdb-binding-stress`):
each seed = a fresh real FDB container + a deterministic seed-generated op program driving the Go
stack-machine binding (backed by `pkg/fdbgo`), flagging any op that diverges from the FDB spec or any
client bug that kills the server.

```sh
just binding-stress                 # default 100 seeds × 1000 ops
just binding-stress 200 2000        # N seeds × M ops
just binding-stress-directory 50 500  # directory-layer
# Reproduce ONE failing seed (a REAL CLI seed flag) — -ops AND -test-name must match the original run:
bazelisk run //cmd/fdb-binding-stress -- -seeds 1 -seed-start NNN -ops 1000
```

- **Catches:** GET/SET/CLEAR/GET_KEY/GET_RANGE divergence, RYW, every atomic op, versionstamp
  placement, tuple/subspace round-trip, retry/conflict/GRV semantics, directory-layer, and
  **server-killing client bugs** (detected as PASS-but-DEAD via a post-seed `fdbcli status`).
- **Reproduce:** the op sequence is a function of `(seed, ops, test-name)` — all three must match. Per-seed
  artifacts under `binding-stress-out/<ts>/seed-NNN/` (`tester.log`, `fdb-logs/`) + a crash-safe
  `report.json`. A pass needs `had 0 incorrect result(s) and 0 error(s)`.
- **Gotchas:** spins Docker per seed — **never run concurrently with `just test`** (pre-commit runs
  `just test`). Only flags: `-seeds -ops -duration -seed-start -out -test-name` (no `-race/-v`). Needs
  `python3` + a prior `bazelisk build`. A flaky seed **is a bug signal** (no unrelated flakes), not noise.

## replay & knobs

What works **today** for reproducing/root-causing, and what needs setup.

```sh
# [TODAY] step-debug a pure-Go test (no Docker for leaf pkgs)
dlv test ./pkg/fdbgo/fdb/tuple -- -test.run '^TestPackUnpack_Int64$'
# [TODAY] loop-until-fail a race target (warm race cache; force real re-runs)
bazelisk --output_base=$HOME/.cache/bazel/_race_output_base test //pkg/fdbgo/client:client_test \
  --@rules_go//go/config:race --runs_per_test=200 --cache_test_results=no --test_output=errors
just race        # or: just race-all
# [TODAY] scheduler knobs as a DIAGNOSTIC (can hide a race as easily as surface it)
GOMAXPROCS=1 GODEBUG=asyncpreemptoff=1 go test -race -count=200 -failfast -run '^TestX$' ./pkg/recordlayer/chaos/
# [TODAY] inter-test order dependence (prints a seed to replay)
go test -shuffle=on -count=1 ./pkg/recordlayer/chaos/     # then: -shuffle=<printed-seed>
# [SETUP] rr reverse-debug (rr NOT installed; `dlv replay` is NOT a dlv 1.26.3 command)
sudo pacman -S rr && echo 1 | sudo tee /proc/sys/kernel/perf_event_paranoid
dlv test --backend=rr ./pkg/fdbgo/client -- -test.run '^TestFlaky$'   # rev next / rev step / rewind
```

- `dlv test`/`dlv debug` work now (Delve 1.26.3). **rr is not installed**, and the RFC's `dlv replay
  <trace>` is stale — this build uses `--backend=rr` (still needs `rr` on PATH, a hardware PMU, and
  `perf_event_paranoid ≤ 1`; fails in most containers). rr reproduces a **captured** run — not a chosen
  seed, no fault injection.
- `testing/synctest` (Go 1.26.4, stable — `synctest.Test`/`synctest.Wait`, **not** `synctest.Run`) gives
  a fake clock + quiescence for **Track-B/client** time-races only; its "no real net/OS in the bubble"
  rule breaks determinism *silently*, not with a panic. **Not** the record-layer DST path.
- Bound hangs with `--test_timeout=<secs>` so a stuck test doesn't cascade 200 tests through 30s waits.

---

## Reproduce-from-seed

Three recipes — **not interchangeable**; only two are true CLI seeds.

1. **Fuzz corpus (fully deterministic, no engine):** `go test -run='FuzzX/<hash>' ./pkg/<path>/`. The
   input is the committed `testdata/fuzz/FuzzX/<hash>` file.
2. **Binding-stress (real CLI seed):** `bazelisk run //cmd/fdb-binding-stress -- -seeds 1 -seed-start N
   -ops <same-as-original> -test-name <same>`.
3. **Chaos (partial, source seed):** edit the test's `WithSeed(S)` / `RandomConfig{Seed:S}` and re-run
   with `--nocache_test_results`. Replays the op sequence but **not** fault interleaving / versions
   (real FDB, wall clock).

Stress has no seed flag (`rand.NewSource(42)` fixed) — re-run the same `TestFDB_Stress_*`; correctness
reproduces, throughput doesn't.

## Pin the bug

Every bug gets a regression on the *dimension that was unprobed* (volumetric coverage misses the broken
axis). Match the pin to the tool:

- **fuzz / differential:** commit the minimized `testdata/fuzz/<Fuzz>/<hash>` crasher — it replays
  forever. If the package's `go_test` doesn't `glob(testdata/**)`, add the glob first.
- **hunt:** first `hunt.Shrink(S, cfg)` to the minimal reproducer, then pin the ROOT cause — a hunt
  failure is a record-layer bug, so the regression is a focused chaos/FDB test (or a `hunt.Run(S)`
  assertion) on the exact maintainer/op, not the raw seed. If the fix is a SimFDB fidelity gap, pin it
  in `pkg/simfdb` (see `TestNewSelectsAPIVersion`).
- **chaos:** add a `NewScenario(t, …, WithSeed(S))` or `RandomConfig{Seed:S}` test at the failing seed
  (and, for a fault bug, `InjectOnce(FaultX)`), so the exact violation is a permanent test.
- **stress / SQL-visible:** add a **yamsql scenario** with a `plan_contains:`/`EXPLAIN` assertion (prove
  the *optimization fires*, not just that rows are correct) or a targeted FDB integration test.
- **binding-stress:** a reproducing seed is the signal; fix the client and add a focused
  differential/unit test for the diverging op (the seed itself isn't a committed regression).

A reviewer (human or Graefe/Torvalds/@claude) catch the suite missed is doubly important: fix the bug
**and** add the test that should have caught it.

---

## The DST tier roadmap (what each tier unlocks)

Status against RFC-179. Move rows from *Planned* to *Available* as tiers land, and add their commands
above.

| Tier | Unlocks | Status |
|---|---|---|
| **−1** rr + Delve replay | reverse-debug a *captured* real-FDB flake | ⚠️ **setup** — `rr` not installed; `--backend=rr` |
| **0** Clock + seeded RNG + Buggify | byte-reproducible persisted state; deterministic `CURRENT_*`; fault points | ✅ **Available** (`pkg/dst`) |
| **1** SimFDB (in-mem MVCC backend) | **single-seed bit-exact replay, no Docker**; true rollback; SSI-conflict + idempotency bug hunting; the conflict-outcome differential as its oracle | ✅ **Available** (`pkg/simfdb`) |
| **2** workload drivers + replay | seed-reproducible record workloads (**[hunt](#hunt)**: brute-force loop-until-bug + shrink); SQL workloads (`sqlhunt`); metamorphic + golden oracles; **[interleave](#interleave-concurrent-transaction-ssi-driver)** + **[rangeconflict](#rangeconflict-range-aware-ssi-driver)** SSI drivers; **[continuation](#continuation-cursor-resume-replay-driver)** cursor-resume replay driver | ✅ **Available** (`pkg/simfdb/hunt` + `sqlhunt`/`metamorphic`/`golden`/`interleave` [+ fault mode]/`rangeconflict`/`continuation`/`sqlpage`) — continuation-under-injected-fault next |
| **3** client sim (Track B) | `synctest`-driven client concurrency + simulated server processes | ⏳ **separate RFC** (not full DST) |

The fast/reproducible inner loop for the record layer is now **hunt** (in-memory, seeded, true-rollback
faults); real-FDB fidelity still comes from **chaos / differential / stress / binding-stress**. Confirm
anything wire-adjacent on real FDB before calling it wire-safe.

## Building a new DST driver (the repeatable recipe)

Every Tier-2 driver so far (`interleave`, `continuation`, the `sqlhunt` family) followed the same
rhythm. When you add a surface, follow it — one driver at a time, fixed to completion, committed, and
handed to a background hunter.

1. **Find the unexercised checkbox.** Read the existing hand-written tests for the surface. If they're
   *point samples* of a space (one fixed schedule, one fixed dataset), the driver is the *seeded sweep*
   over that whole space. Name the bug class it makes fire (interleave → real `1020`; continuation →
   token/resume defects). If a capability is only proven by one fixed test, it is a fake checkbox until a
   seeded driver sweeps it — that's the NO-FAKE-CHECKBOX bar.
2. **Design an INDEPENDENT oracle before writing the driver.** Rank by strength: an independent Go model
   (know the right answer a priori — `continuation`'s PK-order model, `interleave`'s point-key verdict +
   summed-effect state) beats metamorphic self-comparison (engine judges itself — catches WRONG-
   inconsistent but misses a bug both paths share) beats golden (catches CHANGED only). Prefer a model
   when the correct answer is computable from the seed; it catches bugs a metamorphic check can't. Keep
   the model *simpler* than the engine (point-keys vs byte-ranges) — the representation gap is its teeth.
3. **Implement as a `hunt.Workload`** in `pkg/simfdb/hunt/<name>/`: `Name()` + `Run(seed, hunt.Config)
   *hunt.Report`. Own your knobs on the struct (ignore the record-oriented `hunt.Config` fields), build
   the env with `hunt.NewSimEnv(seed, faultProb)`, run **fault-free** unless commit-fault injection is
   itself what you're testing (injected faults confound a serializability/exactness oracle — say so in
   the package doc). Never panic on a found bug — capture it in the `Report`. Fingerprint the final
   keyspace (`hunt.Fingerprint` or a local range-hash) for the determinism probe.
4. **Tests that earn trust, not padding:** a wide **clean sweep** (seeds × profiles, 0 `Failed()`); a
   **teeth** test proving the oracle discriminates (white-box unit-test the predicate, AND — for a model
   oracle — a *fidelity* test that the engine's one-shot output equals the model, so a sweep divergence is
   a real bug not a wrong expectation); **determinism** (same seed → same fingerprint twice). Keep the
   in-suite seed band small (≈10–15 s total — siblings target that); volume is the hunter's job, not
   `just test`'s.
5. **Wire `Profiles()`** into `cmd/dst-hunt/main.go`'s `profiles` slice so it joins the overnight sweep.
6. **Ship:** `go vet` → `gofumpt -w` (nogo enforces it) → `just gazelle` → `bazelisk test //pkg/simfdb/hunt/<name>:...` → commit (the pre-commit runs the full `just generate && lint && build && test`; no `--no-verify`, ever).
7. **Dispatch a background hunter** (general-purpose subagent, `run_in_background`, **`isolation:
   worktree`**): give it (a) a *gated* white-box brute `_test.go` (env-var guarded like `TestBruteHunt`)
   sweeping WIDER configs than the shipped profiles across millions of seeds for a wall-clock budget,
   failing with a full reproducer on the first `.Failed()`; and (b) the operational `dst-hunt -seconds N`
   across all profiles. Tell it to **reproduce → shrink → root-cause but NOT fix** (a Tier-1 resolver or
   query-engine bug needs the lead, and query-engine fixes need a Graefe ACK), and to **delete the gated
   harness if clean**. **Use `isolation: worktree`** — a hunter that writes its gated `_test.go` into the
   *shared* worktree makes the next commit's pre-commit `gazelle` regenerate that package's BUILD.bazel
   to reference a file the hunter is about to delete, and the commit fails until the hunter finishes. An
   isolated worktree keeps its scratch files off your commit path so you can keep building in parallel. A
   clean result is only meaningful if the machinery has teeth — have it report the live count of the bug
   class it fires (interleave/rangeconflict: `1020`s; continuation: pages), so "0 findings" is backed by
   "N million conflicts actually resolved", not silence.
8. **Codify here.** Add a `## <name>` section (what it drives, the oracle, the run command, the
   gotchas), flip the roadmap row, and record any bug + its regression.

## Evolving this skill

This doc is meant to grow. When you touch the DST system or discover a new technique:

1. **A tier lands →** flip its roadmap row to ✅ **Available**, add a `## <tier>` section with the real
   commands + reproduce recipe, and update [Honest status](#honest-status-read-this-first) and the
   [two-axis table](#the-two-axes-how-to-think-about-any-tool-here) (e.g. SimFDB moves the record layer
   into "in-memory + seeded").
2. **New bug class or repro trick →** add a row to [symptom→tool](#symptom--tool-start-here) and a bullet
   to the relevant tool.
3. **Verify before you write.** Every command here was run against the tree — a wrong command makes this
   skill worse than nothing. Re-run it; if a flag/path changed, fix it. When code + RFC-179 disagree with
   this doc, the code wins — fix the doc.
4. **Stay honest about determinism.** Never upgrade a "partial replay" to "seed reproduces the run"
   without the mechanism that earns it. That distinction is the whole point of DST.
