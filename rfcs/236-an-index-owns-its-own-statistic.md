# RFC-236 — An index owns its own statistic

**Status:** proposed (PoC scope)
**Depends on:** RFC-235 §17–18 (the measurement that motivated this)

## 1. Decision

Planner statistics become an **optional capability implemented by an index
maintainer**, not an external estimator that reads index subspaces it does not
own. A maintainer answers only about bytes it writes itself; a maintainer that
cannot answer honestly does not implement the interface, and the planner falls
back to today's constant.

The capability is **opt-in per connection** via a connection-string flag, off by
default, and it participates in the plan-cache key.

Three rules follow from "no statistic beats a wrong statistic", and they are the
whole design:

1. **Locality.** The estimate is computed next to the code that writes the
   entries. No caller reconstructs an index's key layout.
2. **Refusal is a first-class answer.** The interface returns an explicit
   "cannot answer", never a guess. Every error path degrades to the constant.
3. **The capability is self-verifying.** Implementing it obligates a maintainer
   to pass a shared conformance test that compares its estimate against a real
   scan. An index and its statistic change together or the build goes red.

## 2. Why now, and what this is NOT fixing

RFC-235 §18 measured that Go and Java disagree on cross-product nesting in 14 of
16 name/cardinality combinations, both engines resolving a genuine cost tie by a
hash over plan structure. That is not what this RFC fixes, and the distinction
matters: **rename-invariance is not a statistics problem.**

What §18 *did* establish is why the tie exists at all. `FlatMapCost` computes
`Cardinality: outerCard * innerCard`, and in production every leaf resolves to
the same `LeafScanCardinality` constant. Both nestings of `FROM a, b` therefore
cost *identically* — there is nothing for the cost model to prefer, so the
comparison falls through to the tie-break. Real per-table counts do not add a
tie-break; they remove the tie.

Direct evidence that the current path is inert: inverting the statistics rung in
`planning_cost_model.go` — swapping `costA` and `costB` — changes **no plan** in
the suite. `HasRealStats` has zero production call sites. The model is not
mis-calibrated; it is unfed.

## 3. What the engine already has

Nothing here needs inventing, which is most of the argument for doing it now.

| Piece | Where | State |
|---|---|---|
| `StatisticsProvider` (per-type cardinality) | `properties/cost.go:244` | interface + 3 impls, none wired |
| Injection point | `Planner.WithStatistics` / `.Statistics()` | exists |
| Cost formulas consuming cardinality | `properties/cost_formulas.go` | live (`FlatMapCost`, `scanLikeCost`) |
| Exact per-type count | `FDBRecordStore.GetSnapshotRecordCountForRecordType` | ported; needs a COUNT index |
| Sampled range size | `fdbgo/client.GetEstimatedRangeSizeBytes` | ported |
| Per-connection options → plan-cache key | `embedded/planner_options.go` | live, and already forces new flags into the key |

## 4. The capability

Declared beside `IndexMaintainer` in `pkg/recordlayer/index_maintainer.go`,
because that adjacency IS the design:

```go
// IndexStatistics is an OPTIONAL capability of an IndexMaintainer.
//
// A maintainer implements it ONLY if it can answer from bytes it writes
// itself. The planner never reconstructs an index's key layout to ask a
// question about it; the maintainer that owns the layout answers, or nobody
// does.
type IndexStatistics interface {
    // EstimateRange returns an estimate for the portion of THIS index selected
    // by prefix (empty prefix = the whole index).
    //
    // ok=false means "I cannot answer" and is a NORMAL return, not a failure:
    // the planner degrades to its constant. An implementation must return
    // ok=false rather than a value it cannot stand behind.
    EstimateRange(ctx context.Context, prefix tuple.Tuple) (IndexRangeEstimate, bool, error)
}

type IndexRangeEstimate struct {
    // SizeBytes is a sampled estimate from the storage servers.
    SizeBytes int64
    // Count is an EXACT entry count; valid only when HasCount is true.
    // Aggregate maintainers (COUNT) can answer exactly; VALUE maintainers
    // cannot, and must not pretend to by dividing bytes by a guessed row size.
    Count    int64
    HasCount bool
}
```

Two fidelities, one interface, each maintainer answering with what it actually
knows. The alternative — a single `Count` that VALUE indexes fake from bytes —
is precisely the "buggy statistic" this RFC exists to refuse.

## 5. PoC scope

**In:**

- the interface above, plus `IndexRangeEstimate`;
- `standardIndexMaintainer` (VALUE) implementing it for the WHOLE index only,
  via `GetEstimatedRangeSizeBytes` — bytes only, `HasCount=false`, and
  `ok=false` on a 0 estimate (§9);
- `atomicMutationIndexMaintainer` implementing it for `COUNT` — exact,
  `HasCount=true`;
- a `StatisticsProvider` that consults maintainers through the store and falls
  back per record type;
- `OptPlannerStatistics` (`"PLANNER_STATISTICS"`, bool, default **false**),
  threaded through `planner_options.go` and into `cacheKeyPart`;
- the shared conformance test (§6);
- an end-to-end test showing a plan that CHANGES with the flag on, returning
  identical rows both ways.

**Out (named so they are not silently assumed):**

- every other maintainer — version, bitmap, permuted-min-max, max-ever, text,
  spfresh — implements nothing and is unaffected;
- column histograms, NDV, correlated-predicate selectivity;
- PREFIX selectivity from sampled range size — measured unsound (§9);
- persisted statistics (a wire-format question, deliberately untouched);
- plan-cache invalidation on data drift (§8);
- statistics for record types with no index at all.

## 6. The conformance test is part of the interface

A capability that each maintainer implements separately is exactly where two
pieces of code drift apart. So implementing `IndexStatistics` obligates a
maintainer to a shared table-driven test that, against real FDB:

1. builds a known population through the maintainer's own `Update` path;
2. calls `EstimateRange` over the full range and over a prefix;
3. asserts the estimate against an actual `Scan` count of the same range.

For `HasCount` maintainers the assertion is **exact equality** — a COUNT index
that disagrees with its own entries is broken. For byte-only maintainers it is a
band, and the band's width is measured and recorded rather than assumed, because
sampled estimators are coarse on small ranges (§9 is the experiment).

The test is driven from a registry of implementing maintainers, so adding a
maintainer to the capability without adding it to the test is not possible
without deleting a line someone has to justify.

## 7. Safety, concretely

- **Default off.** No connection gets this without asking.
- **Refusal is normal.** `ok=false` → that record type uses
  `LeafScanCardinality`, exactly as today.
- **Errors degrade, never propagate.** A failed estimate is not a failed query.
  Planning must not acquire a new failure mode.
- **Snapshot reads only.** Statistics reads must not add conflict ranges — a
  planner read must never make a transaction retry.
- **Mixed answers are fine.** Some types estimated, others constant, is a
  supported state; the cost model already handles heterogeneous cardinality.

## 8. What this knowingly leaves broken

Stated because the PoC will otherwise be read as complete:

**Plan-cache staleness.** `EmbeddedConnection` caches physical plans keyed by
normalized SQL and invalidates on DDL, not on data change. A plan costed when a
table held 10 rows keeps serving at 10M. The flag is in the cache key, so
turning it on cannot serve a plan built without it — but nothing invalidates on
drift. That is a follow-on, deliberately deferred by the owner.

**Test determinism.** Data-dependent plans make `plan_shape.golden`, the
cross-engine corpus and every `EXPLAIN` assertion sensitive to fixture row
counts. This is the largest practical risk in the RFC and the reason the flag is
default-off: with it off, every existing test plans exactly as it does today,
and that invariance is itself asserted (§10).

## 9. The experiment — RUN, and it changed the design

`GetEstimatedRangeSizeBytes` is sampled, so §5 was gated on measuring where it
stops discriminating. Measured over a VALUE index at descending selectivities
(`pkg/recordlayer/estimated_range_size_probe_test.go`, deterministic across
repeated runs):

    range              entries   est_bytes   bytes/entry
    whole index           4000      404494       101.1     accurate
    price=0 (~50%)        2000      201498       100.7     accurate
    price=1 (~25%)        1000      101498       101.5     accurate
    price=2 (~10%)         400           0         0.0     ZERO, range is NOT empty
    price=3 (small)        599      101498       169.4     same value as the 1000-row range
    price=4 (1 row)          1           0         0.0     ZERO
    price=99 (empty)         0           0         n/a     correctly zero

Above roughly 100KB the estimator is excellent — three ranges agreeing at ~101
bytes/entry. Below it, it fails in the two worst ways available: it returns **0
for a non-empty range**, and it **quantizes** (599 entries reported the identical
101498 as 1000 entries, a 69% overestimate).

Zero is the dangerous one. An empty range is the most selective answer possible,
so a cost model that believed it would rank that index above every alternative.

**This is FDB behaviour, not a Go client defect**, which matters because C++ is
this port's spec for the client. The repo's own C-port test
(`client/c_binding_port_test.go`, ported from `unit_tests.cpp:2500`) states it:
*"On a freshly written range the estimate may be 0 (storage server not yet
compacted), which is acceptable — the C++ test only asserts no error."*

That sentence also names a SECOND failure dimension the table does not separate:
a 0 can mean "below sampling granularity" OR "written too recently to be
accounted". A freshly bulk-loaded table can therefore estimate 0 bytes. The two
are indistinguishable from the return value, and both are unusable.

### What it changes

**Prefix selectivity from range size is OFF the table**, and this is a reversal:
an earlier framing proposed prefix-size / whole-index-size as a direct
selectivity estimate. It is wrong at exactly the selectivities that matter —
index choice is decided on selective predicates, and that is where the estimator
returns 0 or a quantized jump.

**Whole-range cardinality stays viable.** A whole index or record-type subspace
is large, sits well above the granularity, and measured within ~1%. That is also
precisely what §2 needs: `FlatMapCost` ties because every leaf resolves to the
same constant, and per-table counts remove the tie.

So the PoC narrows to what was measured to work:

  - `standardIndexMaintainer` answers for its WHOLE index only, and must map a
    0 estimate to `ok=false` — never to "empty";
  - prefix selectivity comes only from an EXACT source (a COUNT index) or is
    refused;
  - no threshold constant is picked, because the measurement shows a threshold
    would not be sound anyway: 599 entries returned a large, wrong number, so
    "the estimate is big enough to trust" is not a test an implementation can
    apply to its own output.

## 10. Acceptance

1. Flag off: the full suite plans **identically** to today — asserted, not
   assumed, by a golden run with the flag absent.
2. Flag on: at least one query demonstrably changes plan, with identical rows
   under both settings.
3. Every implementing maintainer passes the §6 conformance test.
4. A maintainer that returns `ok=false` for everything produces today's plans.
5. No new query failure mode: an induced statistics error still plans and runs.

## 11. Alternatives rejected

**An external estimator that walks index subspaces.** Rejected on the owner's
constraint, and it is the right constraint: the estimator would encode each
index type's key layout a second time, and the two copies would agree until one
changed. This is the same failure RFC-235 spent a phase deleting.

**Dividing bytes by an assumed row size to fake a count.** Rejected: it produces
a number nobody can stand behind, for indexes whose entry width varies by orders
of magnitude across types.

**Requiring a COUNT index for statistics at all.** Rejected as the *only* route:
it needs metadata opt-in and per-write maintenance, so it would make statistics
unavailable on exactly the stores that most need them. It is kept as the exact
refinement where present.
