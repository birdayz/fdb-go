# RFC-104 — SPFresh online assignment refinement (recall recovery under ingest)

**Status:** proposed (design review FIRST). Production-readiness, not perf.
**Scope:** a new SPFresh maintenance op in the rebalancer lifecycle
(`pkg/recordlayer/spfresh_rebalancer.go` + a new `spfresh_refine.go`). Read/write
path of the index maintenance; no wire-format, query-path, or bulk-build change.
**Gates:** Torvalds (code/concurrency), Graefe (systems/idempotence), codex
(external), spfresh-reviewer (recall — this is a recall-recovery feature; the
paper-fidelity question is whether a *global* refinement is faithful to LIRE or a
justified FDB-side extension).

## Problem (measured)

Sustained fast ingest costs **~2–5 pp recall** versus a bulk build of the same
data, and the existing rebalancer does **not** recover it (measured *after*
draining the split/NPA/merge queue to quiescence). SIFT-300k, identical query
sweep:

| 300k | bulk build (ideal) | fast fill, 8 writers (533 vec/s) | gap |
|---|---|---|---|
| cells / fines | 74 / 3,418 | 55 / 1,755 | ~½ the fines |
| replication (entries/N) | **1.20×** | **1.00×** | closure never fired |
| recall fast (16/24/64) | 0.9205 | **0.8720** | **−4.9 pp** |
| recall default (32/64/200) | 0.9880 | **0.9685** | **−1.9 pp** |

(Matches the prior 1M observation: 530 vec/s fill → 0.925 vs 110 vec/s → 0.961.)

**Root cause.** A vector is closure-replicated *once*, at insert, against the
topology that existed at that moment. Under fast ingest the topology is still
coarse (few, large cells), so the SPANN closure RNG rule (§3.2) rejects every
non-home centroid as same-direction — exactly the geometry the α-replication
sweep (recall-at-scale item 3) measured. Result: the vector lands at 1.0×
replication and is **never re-evaluated** as the topology later refines. NPA
(§6 step 3) re-evaluates only the Neighbor Posting Area around a *split*, never
the global population, so the under-replication is permanent. The low replication
also under-feeds the split trigger (fewer entries → fewer splits → the coarser
55-vs-74 cell count), so the two symptoms compound.

## Lever: online assignment refinement

Re-run the closure assignment for vectors against the **current (converged)**
topology and move the ones whose copy-set changed — the online analog of the
bulk build's wave B, generalizing the NPA reassignment beyond split
neighborhoods. Restoring closure replication (1.0 → ~1.2×) also feeds the split
trigger, so the cell count converges too.

This reuses the NPA per-pk primitive almost verbatim (`spfresh_npa.go`):
re-evaluate a pk's closure copy-set, and if it changed, move it in a per-pk
transaction that **REAL-reads the pk's MEMBERSHIP** — the same serialization
point the foreground write path uses, so a concurrent update/delete of the pk
aborts one side at the resolver and the loser's retry sees truth. The only new
piece is the **candidate set**: the global population instead of an NPA.

### Scheduling — a SEPARATE periodic op, NOT the quiescence drain

Refinement does NOT live in `spfreshRebalanceOnce` / the quiescence drain (codex
r1 P1): that drain defines done as `worked == 0`, and the multi-tenant sweeper
only invokes it for indexes that already have task rows — so a drifted-but-task-
drained index would be **skipped**, and a continuous cursor would make a direct
drain **never quiesce**. Instead refinement is its own budgeted maintenance op,
`RefineSPFreshIndex(ctx, db, storeBuilder, indexName, budget)`, run on its own
cadence by the deployment's maintenance loop (beside the rebalancer loop — the §6
shape already runs one). Each call advances the cursor by up to `budget` pks and
returns the move count; it does NOT drain to quiescence. Cadence × budget trades
recovery latency against maintenance CPU.

### Candidate selection — round-robin membership cursor

A persistent cursor over the MEMBERSHIP keyspace (pk-ordered) advances each call,
refining up to `budget` pks, wrapping at the end — uniform exact-once coverage,
no RNG (deterministic), preferred over random sampling. It is a **META row
carrying `(generation, last-pk)`** and is **generation-scoped** (codex r1 P2):
META survives a generation flip but membership lives under the generation prefix,
so a rebuild/retrain resets the cursor (its recorded generation no longer
matches — start from the beginning). Concurrent executors accept **benign
double-coverage** — two advancing the same cursor merely re-process a range,
harmless because every move is idempotent (`spfreshSameIDSet` no-op on re-eval);
no lease/claim row is needed (simpler than split/NPA's claim — Torvalds/Graefe).
The cursor `Set` is idempotent under commit_unknown (re-advancing past an
already-processed pk just re-no-ops it). A partial final batch wraps to the start.

Each candidate: load the pk's full vector from the **fp16 SIDECAR**, route it
two-level against the **current** topology (`cache.route`, the cache reloaded +
re-validated against `s.generation` once per call — abort/reload on a flip,
codex/Torvalds P2), compute the closure copy-set over a pool **the width of the
bulk build's** (`4·spfreshClosurePool(Replication)` — a narrower pool drops
replicas the wide build placed and REGRESSES a converged index, codex r1 P2),
compare to the stored membership, and move on change. The move tx **REAL-reads
each kept fine's centroid state and rejects non-ACTIVE** — the topology lifecycle
fence NPA uses (`spfreshReadCentroidForWrite`; `cache.route` returns
ACTIVE+SEALED, the move filters to ACTIVE) — so a refine-move never deposits a
posting into a fine that seals/splits concurrently (Graefe/codex r1 P1). Budget
bounds per-call cost (the NPA `spfreshNPABatch` shape); the routing cache is
loaded once per call and reused read-only across the move batches.

The op self-throttles: an already-correct vector's re-eval is a no-op (no write),
so a converged index costs only reads. A future optimization skips even those
cheaply by tracking a per-vector "assigned-against topology epoch"; deferred —
budget already bounds the steady-state read-sweep cost.

## Determinism, idempotence, recall

- **Idempotent.** The re-eval is deterministic over authoritative state; a re-run
  (lease takeover, commit_unknown) finds already-moved pks unchanged and no-ops
  them (identical to NPA's contract).
- **No recall regression on a converged index.** Refining a bulk-built (already
  optimal) index moves nothing — the closure set a vector already has IS the one
  re-eval computes against the same topology. (Pinned by a test: refine a bulk
  index, assert zero moves + unchanged recall.)
- **Recall recovery — MEASURED (see Validation).** The `refine-all` prototype
  recovers fast-fill recall to the bulk baseline (0.8675/0.9735 → 0.9225/0.9885
  vs bulk 0.9205/0.9880). The production op delivers the same recovery
  incrementally under a per-round budget.

## Validation (de-risk before the production op)

1. **Prototype "refine-all" — DONE, hypothesis CONFIRMED.** A one-shot pass
   (`spfreshRefineAll`) refining every vector of a drifted 300k fast-fill index
   recovers recall **to the bulk baseline** (SIFT-300k, table in
   VECTOR_BENCHMARK_RESULTS.md):

   | 300k fast fill (8 writers) | PRE-refine | POST-refine | bulk (ideal) |
   |---|---|---|---|
   | recall default (32/64/200) | 0.9735 | **0.9885** | 0.9880 |
   | recall fast (16/24/64) | 0.8675 | **0.9225** | 0.9205 |

   122k/300k pks moved in 3m24s. Decisively, recall recovered **even though the
   topology stayed coarse** (57 vs the bulk's 74 cells; replication 1.0→1.09×,
   not the bulk's 1.20×): the drift was **assignment quality, recoverable by
   re-routing**, NOT granularity. So the production op needs no re-splitting —
   restoring assignment suffices. (The residual cell-count gap doesn't cost
   recall, consistent with item-4's negative.)
2. **Then the budgeted online op:** fast-fill, run `RefineSPFreshIndex` on a
   cadence until the cursor stops moving, assert recall recovers to within
   ~0.5 pp of bulk (the prototype proves the ceiling; this proves the budgeted
   path; Paper).
3. **No regression on a converged index (gates the `kc` width):** refine a
   BULK-built index → **ZERO moves, recall flat**. A too-narrow pool would move
   replicas off the converged bulk assignment — this test is what pins
   `kc = 4·spfreshClosurePool(Replication)`, swept across Replication ∈ {2,3,4}
   (codex r1 P2).
4. **Concurrency:** `-race`; refinement moves interleaved with (a) foreground
   update/delete of the same pk (the per-pk MEMBERSHIP fence) AND (b) a
   SEAL/SPLIT of the TARGET fine — the move's REAL centroid-state read rejecting
   non-ACTIVE is the fence (Graefe/codex r1 P1); pin both as regressions.
5. **Cost:** the steady-state no-op read-sweep cost on a converged 300k/1M index
   (must be a bounded fraction of maintenance CPU, like NPA).

## Risks

- **Partly-structural drift** (the prototype gates this — if recall doesn't
  recover, refinement alone is insufficient).
- **Maintenance CPU** of the continuous sweep — bounded per round; the no-op
  read cost on a converged index is the steady-state floor to keep small.
- **Move churn** racing the foreground write path — the per-pk MEMBERSHIP fence
  (inherited from NPA) is the established serialization point.

## Not in scope / paper note

The SPFresh paper's LIRE bounds reassignment to the NPA (split-triggered). A
*global* periodic refinement is an FDB-side extension justified by our
transactional update model: foreground writes can outrun maintenance and assign
against a lagging topology, a regime the paper's LSM/SSD update path does not hit
the same way. spfresh-reviewer to confirm this is a justified extension (with
deep test coverage), not paper infidelity.
