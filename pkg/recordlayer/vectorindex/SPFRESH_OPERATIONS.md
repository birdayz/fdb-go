# SPFresh Operations Runbook

Operating the FDB-native SPFresh vector index (RFC-094) in production: how to
deploy it, what to watch, which knob to turn when, and what to do when a
signal goes bad. Numbers referenced here come from
`VECTOR_BENCHMARK_RESULTS.md` (SIFT-1M, single-node testcontainer — re-derive
on your hardware before treating them as SLOs).

## 1. Deployment shapes

SPFresh maintenance is **caller-driven**: nothing runs unless something calls
it. Pick one of:

- **Reference worker** (turnkey, RFC-156): run `cmd/spfresh-maintainer`, or
  embed `RunSPFreshMaintenance(ctx, db, SPFreshMaintenanceOptions{...})`. It
  loops the rebalance sweep on one cadence (`SweepInterval`, default 10s) and
  RFC-104 refinement on a slower one (`RefineInterval`, default 5m) over your
  tenant list, with the StoreTimer metrics wired and graceful ctx shutdown.
  The one thing you supply is tenant discovery (store layout is your keyspace —
  see `discoverTenants` in the cmd). This is the default starting point; the
  two shapes below are what it drives under the hood.
- **In-process on writers** (RFC-094 §6, the benchmark shape): each writer
  process loops `RebalanceSPFreshIndex` beside its write load. Simple; the
  rebalancer competes with your writers for process CPU.
- **Sweeper fleet** (multi-tenant): dedicated workers loop
  `SweepSPFreshIndexes(ctx, db, tenants, opts)` over the tenant list on a
  cadence (seconds to minutes). Concurrent sweepers are safe by construction
  (unique lease owners, task-level exclusion) — shard the tenant list to
  waste fewer scans, or run the same list everywhere. Per-tenant failures are
  isolated and reported in the joined error; the pass continues. Pair with
  `RefineSPFreshIndexes(ctx, db, tenants, opts)` on a slower cadence (RFC-104
  refinement — see §3a).
- **Bulk build**: load records with the index DISABLED, `BuildSPFreshIndex`,
  `MarkIndexReadable`. Crash-safe: rerunning takes over a dead build's token
  and its cellfin state machine resumes idempotently. A build that died
  pre-flip leaves the token held — rerun `BuildSPFreshIndex`; never write
  around it.

Budgets: `SPFreshSweepOptions.MaxRoundsPerTenant` (default 8) and
`MaxActionsPerTenant` (default 64) bound a pass. Undrained tenants are
reported, not errored — the next pass continues them. Cleanup writes consume
budget; foreign-lease skips do not.

## 2. Metrics

Set a `StoreTimer` on the `FDBRecordContext` (`rtx.SetTimer(timer)`) for
query/write instrumentation, and `SPFreshSweepOptions.Timer` for maintenance.
Scrape with `timer.Snapshot()` and export however you export everything else.
Event reference (`spfresh_metrics.go`):

| Event | Meaning | Healthy looks like |
|---|---|---|
| `spfresh_search` (timed) | per-search latency | p50 tracks your sweep table for the configured (w,kc,c) |
| `spfresh_postings_probed` / `_pruned` | Eq.(3) pruning split per search | probed ≈ kc on SIFT-like data (pruning binds only at fine granularity) |
| `spfresh_entries_scanned` | posting entries estimated per search | ≈ probed × Lavg; a climb means oversized postings (check topology hist) |
| `spfresh_rerank_reads` | sidecar point reads per search | ≈ min(C, candidates) |
| `spfresh_starvation_widenings` | pruned-tail refetches | ~0; sustained >0 means ε too tight for the data |
| `spfresh_forward_follows` | stale-cache split redirects | bursts during heavy churn, then decays; sustained high → raise cache refresh rate |
| `spfresh_insert` (timed) | per-insert latency inside the save tx | single-digit ms |
| `spfresh_insert_fence_reads` | candidate state reads per insert | low single digits (design expectation — meter your own baseline); pinned at the pool cap (16) means dense same-direction routing |
| `spfresh_insert_replicas` | copy-set size per insert | ≈ effective ρ (~1.0-1.2) |
| `spfresh_stale_route_retries` | insert re-route attempts | ~0; spikes during splits of hot cells are normal, sustained means cache thrash |
| `spfresh_splits` / `_merges` / `_csplits` / `_npas` | lifecycle ACTIONS only (cleanup clears are excluded) | proportional to write volume (§5.2.2: actions track entries written) |
| `spfresh_zombie_cleans` | cleanup writes across all kinds: stale/zombie/cooldown/no-target task clears | small; a flood follows crashes or mass merges |
| `spfresh_csplit_defers` | coarse-split pause-window defer bumps | bursts while a hotspot cell's fine splits run; sustained means a stuck SEALED row |
| `spfresh_lease_skips` | tasks skipped: another executor's live lease OR already completed (task gone) | ~0 with sharded sweepers; high means overlapping sweepers duplicating scans |
| `spfresh_refine_moves` | RFC-104 vectors re-routed (assignment refinement) | bursts after a fast-ingest phase, then decays to ~0 as tenants converge |
| `spfresh_refine_converged` | refinement cursors that wrapped a full cycle moving nothing | climbs to == tenant count in steady state; the back-off signal for quiescent tenants |

## 3. Tuning

**Per-query** (no redeploy): the scan contract's `High` tuple is
`(k, kc, w, c[, ε])`. Frozen defaults (SIFT-1M): default 32/64/200/ε7 —
0.96 recall@10; fast 16/24/64/ε7 — 0.79–0.83 @ ~9ms (the fast budget is the most
sensitive to the ingest-rate trade below; both ends measured). Recall ladder: kc 128 →
~0.99 @ ~45ms, kc 192 → ~0.998 @ ~69ms (pre-perf-stack numbers; the shipped
binary is 25-32% faster).

**Per-index** (set at CREATE; immutable): `spfreshLmax` (split threshold,
256 default — reply-budget sized), `spfreshReplication`/`spfreshAlpha`
(closure; r=2/α=1.2 — measured: raising either does ~nothing on SIFT-like
data at default granularity), `spfreshSidecar` (exact re-rank source;
REQUIRED on — validation rejects `false`: every rebalancer lifecycle reads
it, and the estimates-only A/B collapsed recall ~0.999→0.69; see the
provenance note in VECTOR_BENCHMARK_RESULTS.md). Where to set them: the Go
metadata API (`Index.Options`) accepts every `spfresh*` key; the SQL DDL
(`CREATE VECTOR INDEX … USING SPFRESH OPTIONS(…)`) exposes `METRIC` and
`RABITQ_NUM_EX_BITS` — the structural knobs (Lmax, replication, alpha, …)
have no DDL tokens yet and take their RFC defaults from SQL.

**The ingest-rate/recall trade** (measured, the one operational surprise):
recall at fixed probes depends on the ingest rate the topology was built
under — 530 vec/s fills read ~0.93 default where 110 vec/s fills read ~0.96.
Writers outrunning the rebalancer assign vectors against a lagging topology.
If steady-state recall matters more than ingest speed: throttle bulk ingest
phases, or raise kc afterward (the kc=192 point holds ~0.99 even on
fast-filled topologies), or use the bulk build for initial loads.

## 3a. Refinement (RFC-104)

Fast foreground ingest costs recall versus a bulk build of the same data — a
vector is closure-replicated once, at insert, against the topology that existed
then, and is never re-evaluated as the topology refines (the ingest-rate/recall
trade above). Refinement recovers it: a persistent round-robin cursor re-routes
each vector against the current converged topology and moves the stale ones.

- **Cadence:** slower than the rebalance sweep. Refinement is recall-recovery,
  not correctness — a fully converged tenant re-scans its cursor for zero moves
  each pass. The reference worker defaults to 5m (vs 10s sweep).
- **Drivers:** `RefineSPFreshIndexes(ctx, db, tenants, opts)` (fleet, one
  budgeted pass per tenant), `RefineSPFreshIndex(...)` (one index),
  `RefineSPFreshIndexAll(...)` (one-shot validation: refine every vector once).
  `BudgetPerTenant` (default 1000) bounds the vectors re-evaluated per pass.
- **Metrics:** `spfresh_refine_moves` and `spfresh_refine_converged` (§2). Use
  `Converged` to back off quiescent tenants — when a tenant's cursor wraps a
  full cycle moving nothing, it is at the bulk-build recall ideal and needs no
  further passes until more ingest drifts it.
- **Concurrency:** the refiner and rebalancer are safe to run concurrently —
  the lifecycle fence serializes their conflicting writes, and every move
  transaction re-verifies its centroid state. This is exercised under injected
  faults by the chaos suite (`chaos_vector_spfresh_test.go`, the concurrent
  refiner-vs-rebalancer test), not just in isolation.

## 4. Playbooks

**Recall dropped.**
1. `SPFreshDebugTopology` — oversized postings in the hist (`>Lmax`,
   `4Lmax+`)? Maintenance is behind: check the sweeper is running, raise its
   cadence/budgets; watch `spfresh_splits` start moving.
2. Hist clean? Check whether a bulk-ingest phase just ran (ingest-rate
   trade above) — raise kc per-query as the stopgap.
3. `spfresh_starvation_widenings` sustained — ε too aggressive for this
   data; raise ε or disable per-query (5th tuple element 0).
4. `SPFreshDebugIntegrity(…, n)` — sampled membership⊆postings violations
   mean a real bug: stop, capture topology+integrity output, file it.

**Task queue growing / `spfresh_lease_skips` high.**
Sweeper underprovisioned or overlapping. One sweeper per tenant shard;
raise `MaxActionsPerTenant` before adding workers (budget, not parallelism,
is usually the limit). Skips with NO other sweeper running mean leases from
a crashed worker — they expire on their own (lease deadline); a flood of
`spfresh_zombie_cleans` right after is the cleanup happening.

**`spfresh_task_errors` nonzero with stable queue depth.**
A poisoned task: its handler fails every pass (the pass skips it, finishes
the rest, and surfaces the joined error — the rest of the queue still
drains). Expect `spfresh_lease_skips` to climb alongside it: each failed
attempt burns one lease TTL in skips before the next executor retries. The
error text names the task kind + id; capture it with
`SPFreshDebugTopology` + `SPFreshAuditTrail(fineID)` and file it — a task
that can never complete is a bug, not an operational condition.

**Inserts erroring `did not converge after cache reloads`.**
Every routed candidate failed the state fence three times — extreme churn on
a hot region (mass splits mid-insert). The error is retryable: the caller's
transaction retry re-runs with a fresh cache. Sustained occurrences mean the
rebalancer can't keep up with a hot-spot write pattern: raise sweeper
budgets; check one cell isn't absorbing all writes (topology dump).

**`transaction_too_large` on saves.**
A ballooned posting pushed the insert tx over limits — should not happen
within the 4×Lmax envelope (the chunked drain holds it); if seen, the
cascade stalled: topology hist will show `4Lmax+` rows; run/fix maintenance
and capture the topology output.

**Build appears stuck.**
`BuildSPFreshIndex` reruns take over a dead build (token + cellfin resume).
A build that finished but didn't flip (crash in the gap) re-flips on rerun.
Writers during a build error with "a bulk build is in flight" — that is the
designed fence, not a bug.

**Generation bumped unexpectedly / queries miss fresh writes.**
Builds flip generations; foreground writes target the readable generation
with a REAL-read fence, so a mid-write flip aborts and retries the write
into the new generation. Queries refresh routing on an amortized changelog
timer — sustained `spfresh_forward_follows` means the refresh interval is
too long for your churn.

## 5. Diagnostics (opt-in, off the hot path)

- `SPFreshDebugTopology(rtx, store, index)` — generation, cells, ACTIVE
  fines, entry count (→ effective ρ = entries/records), task backlog by
  kind, posting-size histogram. O(index) — never call it on a serving path.
- `SPFreshDebugIntegrity(rtx, store, index, n)` — n sampled pks:
  membership ⊆ postings and all targets ACTIVE (human-readable string).
- `SPFreshCheckIntegrity(rtx, store, index, n)` — the **structured** form of
  the above (RFC-156): returns `SPFreshIntegrityReport` with `Members`,
  `Sampled`, `MembershipWithoutEntry`, `BadTargets` (forward/dead/absent
  references), `TargetStates`, and per-violation detail. Use it to wire an
  alert: post-drain, `MembershipWithoutEntry` and `BadTargets` must be 0. This
  is the same check the chaos suite asserts.
- `MeasureSPFreshRecall(ctx, store, index, k, querySamples, seed)` — the
  **ground-truth recall monitor** (RFC-156). Samples query vectors from the
  index's own records, computes the true k-NN by a full metric scan, compares
  to the index's results, returns `SPFreshRecallReport{MeanRecall, MinRecall,
  PerfectFraction, ...}`. A vector index corrupts *silently*, so this is the
  load-bearing production signal: alert on `MeanRecall` dropping below your
  measured baseline (maintenance behind, an ingest-rate trade, or — with the
  integrity check also red — real corruption). O(querySamples × corpus); run
  off the serving path on a cadence.
- `SPFreshEnableAudit()` / `SPFreshDisableAudit()` — records per-fineID
  lifecycle steps in an in-memory map (unbounded while enabled — incident
  debugging only); read a centroid's history with
  `SPFreshAuditTrail(fineID)`. Nothing is printed; disable releases the
  memory.

## 6. Known limits

- Bulk build is the **fast** high-recall path (≈10.7k vec/s with the perf
  stack, ~20× the online fill) since RFC-099/101 made wave-B two-level — prefer
  it for offline/batch loads. (The earlier note that bulk build was *slower*
  than the foreground fill predated RFC-099 and is no longer true.)
- A single-store bulk build hard-errors above ~267M vectors
  (`spfreshMaxDeltasPerTx = 65536` coarse cells per changelog tx,
  `spfresh_storage.go`). Multi-tenant fleets sit far under it; lifting it for a
  single giant store needs changelog chunking (RFC-156 §4.3, not yet done).
- Scale validated to 1M vectors; 10M+ soak is the next step (RFC-156 §4.3).
- Single FDB cluster; multi-tenant via the sweeper; cross-cluster is out of
  scope.

## 7. Scale validation (RFC-156 §4.3)

The churn-soak harness is N-parameterized — drive it at 10M/100M to validate
beyond the current 1M ceiling. It tracks recall@10 vs brute force per wave plus
the topology histogram and sampled integrity:

```sh
SPFRESH_BENCH=1 SIFT_N=10000000 SOAK_WAVES=6 bazelisk test \
  //pkg/recordlayer/bench:bench_test --test_arg="--test.run=TestSPFreshChurnSoak" \
  --test_output=streamed --test_env=SPFRESH_BENCH --test_env=SIFT_N --test_env=SOAK_WAVES \
  --test_timeout=36000
```

Recall floors at scale are spfresh-reviewer-owned: fixed probes cover a
shrinking list fraction as N grows (kc=64 covers 64/11,336 fines at 1M, far
less at 100M), so the kc/w defaults likely need a per-scale freeze — the same
re-tune 094.5 did for the 1M+ regime. Pair each run with `MeasureSPFreshRecall`
(§5) and `SPFreshCheckIntegrity` to pin both recall and structural correctness.
