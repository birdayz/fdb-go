# RFC-205 — The plan gate: user-facing admission control on the completed plan

**Status:** DRAFT, revision 1 — **awaiting owner review before any review
gate or implementation** (owner directive: no implementation until the owner
has personally reviewed this document; the Graefe + Torvalds RFC ACK and the
per-phase joint implementation laps queue behind that read).

Java reference: `fdb-record-layer/` at tag 4.12.11.0. All Java citations are
relative to that tree; all Go citations to the repo root. Every load-bearing
claim below was measured against source in this worktree (origin/master,
11454b9ab) and carries a `file:line`.

Owner intent, verbatim: *"expose the planner to users, e.g. having some kind
of callback, so we can make use of the plan to estimate cost, and therefore
reject if cost too large for multi-tenant ratelimiting — this is often hard."*
And the workload it serves: *"we are interested in highly concurrent agentic
systems. agents will do crap. but rate limits and row budgets will keep them
at bay."*

## 0. Summary

A **PlanGate** is a user callback registered on the SQL driver, invoked at the
single funnel where a fully-costed physical plan exists and no FDB work has
started (`cascadesPlan.Execute`,
`pkg/relational/core/embedded/cascades_generator.go:1205`), with an immutable
**PlanInfo** view of the plan — shape digest, proven cardinality bounds, scan
classification, structural feature counts, scalar cost as a tiebreaker — and
returning one of three decisions: **Allow**, **AllowWithBudget** (statement-
scoped scan-row / scan-byte / time / result-row caps injected into execution),
or **Reject** (SQLSTATE 53400 with machine-readable context). A completion
callback delivers **Actuals** — records scanned, bytes scanned, rows returned,
pages, wall time — read from counters the executor already maintains
(`ScanLimiterState`, `pkg/recordlayer/scan_limiter_state.go:72-84`), so a
tenant ledger, token bucket, or billing pipeline closes the loop without new
instrumentation.

Budgets are the centerpiece, not rejection: agents retry on errors, so a hard
reject breeds retry storms, while a budget yields partial results and a
bounded bill. Reject is reserved for the truly pathological, and its error
carries enough structured context for an agent to *rewrite* the query rather
than blind-retry.

This is a **net-new Go extension**. Java has no equivalent (§2, measured), it
touches nothing on the wire (§2.3), and when no gate is registered the
execution path is unchanged.

## 1. Motivation, and the floor it builds on

A multi-tenant service fronting this engine with LLM agents as query authors
has exactly one enforcement problem: an agent will, sooner or later, emit
`SELECT *` with no predicate over a 100M-row type, a fan-out join it did not
intend, or an IN-list with 4 000 parameters — and it will do so *concurrently
from hundreds of statements*. Today the operator has these tools:

- **The FDB 5-second transaction limit**, reflected in the per-page budget
  `txPageTimeLimit = 4 * time.Second`
  (`pkg/relational/core/embedded/cascades_generator.go:1187`, armed at
  `:1647-1653` where a user value can only *narrow* it). This bounds
  single-transaction damage and is the floor the gate builds on — but a
  paginating statement is a *sequence* of bounded transactions, so it bounds
  nothing statement-wide.
- **Caller-set execution limits** — `EXECUTION_TIME_LIMIT`,
  `EXECUTION_SCANNED_ROWS_LIMIT`, `EXECUTION_SCANNED_BYTES_LIMIT`
  (`pkg/relational/api/options.go:175-190`), flowing into
  `ExecuteProperties` (`pkg/recordlayer/scan_properties.go:132-156`). These
  are static per-connection numbers set *before* planning; they cannot
  distinguish a point lookup from a full scan, so they must be sized for the
  worst acceptable query and therefore over-admit everything below it.
- **Nothing between planning and execution.** The planner produces a plan
  that *provably* returns at most one row, or provably has an unbounded data
  access — and the caller never gets to see that proof before paying for the
  execution.

The gate is the missing piece: policy that sees the plan. Per-tenant rate
limiting needs (a) a pre-execution decision informed by what the plan will
plausibly cost, (b) budgets attached to that decision so a mistake terminates
boundedly instead of erroring instantly, and (c) post-execution actuals so
the tenant ledger charges what was actually consumed and the operator can
calibrate estimate-vs-actual per plan shape.

### 1.1 Prior art: how multi-tenant admission control fails in Postgres

The gate is not a convenience feature; it is a capability the incumbent
single-node SQL engine structurally cannot offer. Every claim below names
the mechanism:

- **`statement_timeout` measures wall-clock, not work.** A 50M-row
  sequential scan that finishes inside the timeout passes; a point lookup
  on an overloaded box can trip it. Time is a proxy for cost only when the
  machine is idle — which a multi-tenant box never is.
- **Cost-based rejection exists only as an out-of-tree hook.** Postgres
  ships no admission control; the known implementation is the
  `pg_plan_filter` module, which hooks executor startup to compare the
  planned statement's `total_cost` against a `statement_cost_limit` GUC.
  It gates on the planner's cost units — famously uncalibrated
  (`random_page_cost` folklore), point estimates with no error bars, no
  proven bounds — so a threshold both over-admits and over-rejects, and
  almost nobody runs it in production.
- **Attribution is post-hoc.** `pg_stat_statements` aggregates actuals by
  query fingerprint (`queryid`) *after* execution: it finds yesterday's
  abusive tenant and cannot stop today's statement.
- **Live remediation is polling and killing.** The operator loop is
  `pg_stat_activity` polling plus `pg_cancel_backend` — and a cancelled
  query yields **nothing**: no partial results, no resume token, all
  consumed work discarded. There is no pause/resume in the execution
  model.
- **Concurrency control devolves to connection counting.** With no
  per-query admission, tenants get isolated by pgbouncer pool sizes — a
  cap on *sessions*, blind to whether a session runs point lookups or
  cross joins.
- **Real workload management is sold separately.** Redshift WLM
  query-monitoring rules, Greenplum resource groups, and Snowflake
  resource monitors exist precisely because the base engine lacks this
  layer.

The structural argument for this codebase is that the FDB 5-second
transaction limit already forced it to build exactly the three primitives
Postgres lacks: every physical cursor counts its reads as it makes them
(the scan-limiter duality, §4.8); execution is natively resumable
(continuations are the wire-level paging primitive, RFC-203); and plans
carry **proven** cardinality bounds, not just a cost guess (RFC-195: the
cost model may not contradict what the property layer proves). So
allow / reject / allow-with-budget-and-resume is a first-class shape
here — the budget-exhausted statement has already streamed its partial
rows and (under RFC-203) holds a token to continue under a fresh grant —
where in Postgres the same behavior is a research project. The one
Postgres mechanism worth copying is `pg_stat_statements`' fingerprint-
keyed actuals, and this RFC copies it as the shape-digest-keyed
completion callback (§4.8) — moved from post-hoc forensics to the same
loop that admits the next statement.

## 2. Java is checked, and has no equivalent

Measured over the whole Java tree (`grep -rni "admission"` over `*.java`,
`*.proto`, `*.md`: one unrelated Lucene changelog hit). What Java has:

1. **Planner-internal complexity aborts, fixed policy.**
   `RecordQueryPlannerConfiguration` carries `complexityThreshold`,
   `maxTaskQueueSize`, `maxTotalTaskCount`, `maxNumMatchesPerRuleCall`
   (`record_planner_config.proto:43-56`; getters
   `RecordQueryPlannerConfiguration.java:154,238,246,263`). They fire as
   `throw new RecordQueryPlanComplexityException(...)` —
   `RecordQueryPlanner.java:329-333` (final plan vs threshold),
   `CascadesPlanner.java:447-450,492-495` (task caps). No callback, no
   tenant identity, no caller-supplied policy. Go already ports this concern
   as SQLSTATE 54F02 (`pkg/relational/api/errcode.go:134`).
2. **Caller-set enforcement primitives.** `ExecuteProperties.java:56-76`
   (row/time/scanned-records/scanned-bytes limits,
   `failOnScanLimitReached`), surfaced in fdb-relational as the
   `EXECUTION_*_LIMIT` options (`Options.java:178-192`) and wired at
   `EmbeddedRelationalConnection.java:522-524`. Set before planning, never
   derived from the plan.
3. **Observe-only instrumentation.** `PlannerEventListeners.EventListener`
   is all `void` methods (`PlannerEventListeners.java:131` ff.), dispatched
   only *during* planning (`CascadesPlanner.java:432-497`); the `Debugger`
   (`Debugger.java:61`) can restart planning but not reject a finished plan;
   `QueryPlanInfo` (`QueryPlanInfo.java`, keys at
   `QueryPlanInfoKeys.java:29-36`) is a read-only diagnostics bag;
   `FDBStoreTimer.PLAN_QUERY` (`FDBStoreTimer.java:218`) is a metric.
4. **The seam is closed.** `FDBRecordStoreBase.java:2089-2090`:
   `executeQuery(query) → executeQuery(planQuery(query))` — nothing between
   plan and execute except the caller's own code, and the SQL layer
   (`PlanGenerator.java:132-138`) interposes only metrics. The only
   post-plan rejection in fdb-relational is `PlanValidator.java:52-72` —
   fixed continuation/plan-hash compatibility criteria, not extensible, not
   cost-based.

So: enforcement primitives exist, admission does not. The gate composes with
(2) rather than replacing it.

### 2.3 Why a Go-only extension is allowed here

- **Wire compat holds trivially.** The gate is read-side admission: it never
  changes what is written to FDB, adds no proto field, and leaves the
  RFC-203 continuation envelope byte-identical (§4.7). A Java app sharing
  the cluster observes nothing.
- **The conformance principle is not violated.** "Doesn't work in Java →
  doesn't work in Go" governs the *shared* query surface — inputs both
  engines attempt. An unregistered gate leaves that surface untouched; a
  registered gate is operator policy layered above it, exactly the
  sanctioned "net-new read-side capability with deep test coverage"
  (CLAUDE.md, "Wire compat is the hard line; query reach is not").

## 3. What the plan carries at planning completion — measured inventory

The design must surface only what is cheap and stable. Measured state of the
finished plan:

| Signal | Computed at plan time? | Cost to surface |
|---|---|---|
| `Explain()` string | yes — stored on `cascadesPlan.explain` (`cascades_generator.go:1152`, filled at construction) | free |
| Plan tree: node types, index names, scan comparisons, covering/unique/reverse flags | yes — the returned tree (`plans/scan.go:84-98`, `plans/index_scan.go:137-269`) | free, accessors exported |
| Shape identity: `explaindiff.ShapeOf` | on demand, O(nodes) | free; **stable-by-construction** across runs and releases (`pkg/relational/conformance/explaindiff/explaindiff.go:351-358`) but type-only, so **coarse** — its own doc requires pairing with a query description |
| `plans.PlanHash` | on demand | free; explicitly **unstable across releases** (`pkg/recordlayer/query/plan/plans/plan_hash.go:16-18`) — in-process correlation only |
| Proven `Cardinalities` (min/max/unknown) of the root | derivation exists per operator (`pkg/recordlayer/query/plan/plans/cardinality_bounds.go`, ~51 `ProvenCardinalities` arms), **not stored** on the plan | one O(nodes) walk; `concretePlanCostAndBounds` already does it (`pkg/recordlayer/query/plan/cascades/planning_cost_model.go:1555-1574`) — unexported |
| Scalar `Cost{Cardinality, CPU}` | **stored nowhere, by design** — "the Cascades cost model is a comparator, not a number" (`pkg/relational/core/embedded/plan_logging.go:47-49`; `properties/cost.go:16-19`) | same walk as above — unexported; **1e6-defaulted** absent a record-count key (`properties/cost.go:57-66,215-220`; `fetchTableStatistics`, `cascades_generator.go:1981-2021`) |
| Structural feature vector: `expressionCounts` incl. `unboundedDataAccess`, `inMemorySortCount`, `maxDataAccessCardinality`, join/filter/fetch counts | computed during plan comparison, discarded | `concretePlanCounts` recomputes in one walk (`planning_cost_model.go:447-470,1938-1946`) — unexported |
| Point-probe recognizer | `isProvablePointProbe` (`pkg/recordlayer/query/plan/plans/cost.go:110-138`), shared by cost and proof | unexported; exported equivalent is a proven max of 1 |
| Statistics beyond table cardinality | **do not exist** — `StatisticsProvider` is one method (`properties/cost.go:205-211`); CQ-11 open (TODO.md:1186-1189) | real work, out of scope here |

Two consequences drive the whole design:

1. **Everything PlanInfo needs is one O(nodes) walk over already-written
   code; the only gap is that the entry points are unexported.** The
   implementation is thin wrappers, not new derivation.
2. **The scalar cost is a tiebreaker, not a truth.** In the cost model
   itself it occupies only the tiebreak rung of a lexicographic comparator
   (`planning_cost_model.go:22-49,397-405`); its inputs default to
   `LeafScanCardinality = 1e6` flagged "not measured against real
   workloads" (`properties/cost.go:57-66`); and RFC-195 exists precisely
   because these estimates were wrong by up to 700 000×
   (`rfcs/195-cost-must-not-contradict-proof.md`, "The defect, measured").
   What RFC-195 *did* make trustworthy is the **proven interval**: structural
   `[min,max]` bounds, never statistics
   (`plans/cardinality_bounds.go:24-28`), with cost clamped inside them
   (`properties/cardinality_bounds.go:54,80`). **The gate API therefore
   leads with plan properties — scan class, boundedness, proven bounds,
   structural counts — and presents the scalar cost explicitly as
   low-confidence.** When CQ-11 lands calibrated statistics, the number
   improves in place; nothing here blocks on it.

## 4. The design

### 4.1 The seam: `cascadesPlan.Execute`, and nowhere else

The gate fires at `cascadesPlan.Execute`
(`cascades_generator.go:1205`), immediately after `c := p.conn` and beside
the existing `OptContinuation` reject (`:1214-1218`) — which is already an
admission gate in miniature. Why this line and not the planning site:

- It is the **only funnel** every Cascades-planned statement passes through
  with a costed `plans.RecordQueryPlan` in hand and no FDB work started
  (execution begins at `pr.fetchPage()`, `:1271`). Both plan-cache arms
  converge here; a gate at the plan-construction site
  (`cascades_generator.go:462-501`) is skipped entirely on a cache hit
  (`:318-326`).
- **EXPLAIN never reaches it, structurally.** `planExplain` renders at plan
  time and returns a `query.PlanFunc` serving a static row
  (`cascades_generator.go:508,519,527-537`); `PlanExplain`
  (`connection.go:573`) executes nothing. The requirement "the gate must
  never fire for EXPLAIN" is satisfied by construction, not by a flag — and
  pinned by test (G-P1-5). A gate at `:462` would fire for EXPLAIN via the
  `:619` re-entry, and twice for an uncached one.
- It fires **per execution**, which is what admission means: a cached plan
  re-executed by a different tenant is a fresh decision.
- Scope is deliberate: DDL/SHOW (`query.PlanFunc`) and multi-statement
  batches (`query.MultiPlan`) bypass it. Admission control governs
  data-touching Cascades plans — SELECT and DML both, since `planDML`
  produces a `cascadesPlan` too (`cascades_generator.go:1084`).

PlanInfo assembly at the seam is one O(nodes) walk (§3); P3 memoizes it
alongside the plan-cache entry so cache hits pay a pointer read.

### 4.2 Registration: pool-safe, connection-scoped, no context override

`database/sql` pools connections, so per-connection setters alone are
operationally wrong for a service. Two mechanisms, both thin:

1. **Per-DB (primary):** `sqldriver.RegisterPlanGate(name string, g
   PlanGate)` — a process-global registry keyed by a DSN option
   `plan_gate=name`, resolved at `Connect` time and installed on every
   `EmbeddedConnection` the pool creates. This follows the existing
   `RegisterBackend` precedent exactly (`pkg/relational/sqldriver/driver.go:84`,
   consumed at `:145-148`). Today DSN options beyond `schema` and
   `cluster_file` are parsed and dropped (`sqldriver/dsn.go:324-330`);
   `plan_gate` becomes the third consumed key.
2. **Per-connection (embedded users, tests):**
   `(*EmbeddedConnection).SetPlanGate(g PlanGate)`, reached through
   `sql.Conn.Raw` behind a narrow interface exported from `sqldriver` —
   the exact pattern RFC-203 ruled for `ContinuationConn`
   (`rfcs/203-sql-continuation-envelope.md:1264-1272`) and that every
   existing knob uses (`connection.go:122-168`).

The gate field on the connection is an `atomic.Pointer[PlanGate]`, the same
idiom as `FDBRecordContext.timer` (`pkg/recordlayer/database.go:576`).

**Tenant identity comes from `ctx`, not from registration.** The callback
receives the statement's context (threaded end-to-end today:
`connection.go:393` → `cascades_generator.go:1205`); the gate extracts its
own tenant key from it. One gate multiplexes all tenants. A per-context
*gate override* is rejected (§5.7).

### 4.3 The API

```go
// pkg/relational/sqldriver (interface), implemented against
// pkg/relational/core/embedded internals.

type PlanGate interface {
        // Admit is called after planning and before any FDB work, once per
        // execution (including, under RFC-203, each EXECUTE CONTINUATION
        // resume). It MUST be safe for concurrent calls from many
        // connections; the engine holds no lock across the call and never
        // serializes callers. PlanInfo is an immutable value snapshot.
        Admit(ctx context.Context, info PlanInfo) Decision

        // Done is called exactly once per admitted execution at its
        // terminal point (exhaustion, result-limit, budget exhaustion,
        // error, or Close), with counters read from the execution ledger.
        // Same concurrency contract as Admit. (Phase 2.)
        Done(ctx context.Context, info PlanInfo, a Actuals)
}
```

**PlanInfo** — every field grounded in §3's inventory:

| Field | Type | Source |
|---|---|---|
| `SQL` | string | normalized text, the plan-cache key component (`plan_cache.go:112-115`) |
| `QueryDigest` | string | sha256 of `SQL`, hex-truncated like `ShapeDigest` (`factorycorpus/writer.go:32-42`) |
| `ExplainText` | string | `cascadesPlan.explain` (`cascades_generator.go:1152`) — free |
| `ShapeLines` | []string | `ShapeOf` rendering (`explaindiff.go:359-379`), promoted to the `plans` package (§4.8) |
| `ShapeDigest` | string | sha256/16-hex of ShapeLines (`factorycorpus/writer.go:32-42` algorithm) — the **stable, coarse** class key; API contract in §4.6 |
| `EstimatedCost` | `{Rows, CPU, Total float64; LowConfidence bool}` | exported wrapper over `concretePlanCostAndBounds` (`planning_cost_model.go:1555-1574`); `LowConfidence` is true when statistics fell back to defaults (`fetchTableStatistics` returned nil, `cascades_generator.go:1981-2021`) |
| `ProvenRows` | `{Min, Max int64; MinKnown, MaxKnown bool}` | same walk; `properties.Cardinalities` (`properties/cardinality.go:129`) |
| `ScanClass` | enum `PointLookup \| Bounded \| Unbounded` | `Unbounded` iff `expressionCounts.unboundedDataAccess` (`planning_cost_model.go:464-469`); `PointLookup` iff every data access has proven max 1 (`plans/cardinality_bounds.go:165-171,196-215`); else `Bounded` |
| `Indexes` | `[]IndexUse{Name, Covering, Unique, Reverse}` | `GetChildren()` walk over `RecordQueryIndexPlan` accessors (`plans/index_scan.go:137,184,249,266`) |
| `Joins` | `{FlatMap, NestedLoop, InJoin, InUnion, NLJPredicates int}` | `concretePlanCounts` (`planning_cost_model.go:447-470,1938`) |
| `InMemorySorts`, `Distincts` | int | same counts — the memory-risk operators (`plans/in_memory_sort.go:41`, `plans/distinct.go:23`) |
| `ParameterCount` | int | the substituted-parameter count (`connection.go:399`) — huge IN-lists show here |
| `StatementKind` | enum `Select \| DML` | `cascadesPlan.IsUpdate` |
| `Resumed` | bool | false today; true on the RFC-203 resume path (§4.7) |

This vector makes each agent failure mode *recognizable*: the no-predicate
`SELECT *` is `ScanClass=Unbounded` with empty `Indexes`; the accidental
cross join is `Joins.NestedLoop > 0` with `NLJPredicates == 0` and unknown
`ProvenRows.Max`; the 4 000-way IN-list is `ParameterCount`; the repeated
expensive shape is the same `ShapeDigest` arriving at rate (stampede
detection is gate-side, §4.6); deep pagination is the gate's own resume
count per digest (§4.7).

**Decision:**

```go
type Decision struct {
        Kind    DecisionKind // Allow | AllowWithBudget | Reject
        Budget  *Budget      // required iff AllowWithBudget
        Reject  *Rejection   // required iff Reject
}
type Budget struct { // statement-scoped; zero value = dimension uncapped
        ScanRows   int64
        ScanBytes  int64
        Time       time.Duration
        ResultRows int64
}
type Rejection struct {
        Message string
        Context map[string]any // merged into the api.Error context
}
```

### 4.4 Budget enforcement: statement-scoped, riding existing machinery

Everything a budget needs already exists; the design only *intersects*.

- **Injection point:** `(*paginatingRows).executeProps()`
  (`cascades_generator.go:1627-1676`) — the single site where per-page
  limits become an `ExecuteProperties` value, already performing exactly
  this kind of clamp (`min(txPageTimeLimit, OptExecutionTimeLimit)`,
  `:1647-1653`). Gate caps **intersect** with connection options and the
  4-second page ceiling — `min`, never overwrite, so a gate can only
  narrow. Injected caps reach scalar subqueries too, which are deliberately
  executed under the same props before the main plan
  (`:1779-1782`), and reach every leg of an IN-join via the shared
  `ScanState` pointer (`scan_limiter_state.go:32-45`).
- **Statement-scoping:** `ScanLimiterState` is minted per page
  (`scan_properties.go:196-211`), so a per-page cap alone leaks: an
  unbounded scan just pages forever, each page under budget. The gate
  budget therefore lives as a **statement ledger** on `paginatingRows` —
  the layer that already owns the statement-scoped counters (`maxRows` /
  `maxResultBytes`, enforced in `Next`, `cascades_generator.go:1454-1495`)
  and the statement-scoped memory budget (`props.State = r.execState`,
  `:1674`). Each page's caps are set to the *remaining* ledger balance; at
  page end the page's `ScanState` counters
  (`RecordsScanned`/`BytesScanned`, `scan_limiter_state.go:136-166`) are
  drained into the ledger before the next page mints fresh state
  (`fetchPage`, `:1770-1800`).
- **Time:** the statement-level arm rides the existing
  `context.WithTimeoutCause` mechanism (`:1296-1303`) as
  `min(statementTimeout, Budget.Time)`; the per-page arm is the existing
  clamp. The docscheck DST seam gate
  (`pkg/docscheck/dst_seam_gate_test.go:207-311`) already forces any
  `WithTimeLimit` arming site to use `DefaultExecutePropertiesIn(env)`; the
  injection point is that exact site, so the seam discipline is inherited,
  not re-argued.
- **ResultRows:** `min` into the existing `maxRows` machinery
  (`pageRowBudget`, `:1606-1625`). Semantics follow `MAX_ROWS`: clean
  truncation.
- **Exhaustion of scan/byte/time budgets** raises the *existing*
  `ErrCodeExecutionLimitReached` (54F01, `errcode.go:128`) — the code the
  liveness tripwire already uses (`cascades_generator.go:1837-1845`) — with
  gate context added: `{plan_gate: true, budget_dimension, consumed,
  budget, shape_digest}`. Rows already streamed to the client stand.
  One consequence is stated rather than hidden, inherited from the
  tripwire: a budget so tight that a page cannot progress turns a valid
  query into a hard 54F01, not a slow one.

### 4.5 Reject, and fail-closed

- **Reject** returns `api.NewError` with a **new** code
  `ErrCodeQueryAdmissionDenied = "53400"` — class 53 "insufficient
  resources" is the correct home for *policy refused you now* (Postgres
  53400, `configuration_limit_exceeded`); the 54-class codes already in use
  (`54F01` execution limit, `54F02` planner complexity,
  `errcode.go:128,134`) mean *the statement itself exceeded an engine
  limit* and are not reused. `53400` is currently unused anywhere in the
  repo (measured: zero grep hits). The `api.Error.Context` map
  (`pkg/relational/api/error.go:23-30`) carries the machine-readable
  payload — gate-supplied `Rejection.Context` merged with engine-supplied
  `shape_digest`, `estimated_cost`, `scan_class`, `proven_max_rows` — so an
  agent can self-correct (add a predicate, shrink the IN-list) instead of
  blind-retrying. Per the error-contract note at `error.go:13-21`, the
  contract is the code and context keys, never the message text.
- **Fail-closed.** A gate that panics or misbehaves must never silently
  admit. `Admit` is called under a `recover` (precedent: the connection's
  panic conversion at `connection.go:346`); a panic — or a `Decision`
  violating its own invariants (e.g. `AllowWithBudget` with nil `Budget`)
  — fails the statement with a **distinct** code
  `ErrCodePlanGateFailure = "39F01"`: class 39 is "external routine
  invocation exception", which is precisely what a user callback is, and
  the `F`-suffixed private code follows the repo's convention
  (`54F01`, `54F02`, `53F00`, `24F00`, `0AF00` — `errcode.go`).
  A panic in `Done` is recovered and reported the same way but cannot fail
  a statement that already terminated; it surfaces as the statement's
  close error.

### 4.6 Shape stability: what the API promises, and where caching lives

The API promises exactly what `ShapeOf` proves
(`explaindiff.go:351-358`):

- `ShapeDigest` is **stable across processes and releases** for the same
  plan tree — no aliases, no counters, no cost numbers. It is already the
  RFC-201 factory dedup key (`factory/execute.go:154`,
  `factorycorpus/writer.go:17-42`).
- It is **coarse by construction** — type identity only; two plans
  differing only in index name collapse. The API therefore documents the
  caching key as `(ShapeDigest, QueryDigest, tenant)`, mirroring the
  factory's own `DedupKeyOf(featureVector, planShape)` pairing rule.
- It is **not** promised that the same SQL yields the same digest across
  metadata or planner-option changes — plan choice may legitimately change,
  which is the point of re-admission.

**Decision caching is gate-side, deliberately.** An engine-side decision
cache is rejected (§5.8): a rate-limit decision is time-varying (a token
bucket that said Allow at t₀ must be free to say Reject at t₁), so caching
it in the engine would be caching a policy the engine cannot know the TTL
of. What the engine *does* cache (P3) is the expensive-to-build input:
PlanInfo is memoized on the plan-cache entry
(`plan_cache.go:117-120`), making `Admit`'s marginal cost on the hot path a
map read plus the user's own policy check. Because the gate cannot alter
the *plan* (only reject or budget it), gate identity does **not** enter the
plan-cache key — the injectivity concern documented at
`planner_options.go:102-128` does not apply.

### 4.7 Continuations: re-admission on every resume, nothing in the envelope

Today Go SQL continuations do not exist as a user surface —
`cascadesPlan.Execute` rejects `OptContinuation` (`:1214-1218`) and
RFC-203 (`EXECUTE CONTINUATION`, DRAFT rev 2) is unimplemented (measured:
no `GO_V0` in `pkg/`, `planOne`'s administration arm handles only SHOW,
`cascades_generator.go:168-181`). Internal pagination within one `Execute`
is invisible and fully covered by the statement ledger (§4.4). The design
still decides the resume question now, because it constrains RFC-203's
implementation:

**Decision: the gate sees every resume; the decision never rides the
envelope.**

1. **Re-admission.** An `EXECUTE CONTINUATION` resume is a new execution
   under possibly-new tenant state; it gets a fresh `Admit` with
   `PlanInfo.Resumed = true`, PlanInfo rebuilt from the *deserialized* plan
   (same walk — the deserialized tree is a `plans.RecordQueryPlan`). A
   budget granted at admission covers that execution; the aggregate across
   resumes is the gate's ledger, fed one `Done` per execution. This is how
   per-statement cost attribution spans a paged statement: the gate keys
   its ledger by `(tenant, ShapeDigest, QueryDigest)` and accumulates —
   which also makes deep pagination (many resumes of one digest) directly
   observable without any engine counter.
2. **Nothing in the envelope**, for three independent reasons. (a) RFC-203
   adopts Java's `ContinuationProto` with the explicit rule "Scoping is by
   VALUE, never by SCHEMA" (`rfcs/203:571-573`) — no Go-only fields; the
   Go-owned `Any` extension point (`rfcs/203:117-131`) exists for plan-node
   transport, and a decision record there would still fail reason (b).
   (b) The token is **client-held and replayable**: any consumed-so-far or
   allow-decision carried in it is attacker-writable state. The only
   tamper-proof ledger is server-side — the gate's own. (c) Rate limiting
   aggregates per tenant; exact per-statement lineage in the token adds no
   enforcement power.
3. **Binding constraint on RFC-203 implementation:** the resume path must
   materialize a `cascadesPlan` and enter `cascadesPlan.Execute` — not
   construct a bare `paginatingRows` — so it crosses the gate seam and the
   `GO_V0` / plan-hash / constraint fences in the order RFC-203 §"Resume
   flow" specifies (`rfcs/203:1184-1215`), with the gate after the fences
   (a token that fails the fence is 24F00 before policy is consulted).
   This is recorded here and must be carried into RFC-203's sequencing.

### 4.8 Actuals: read the counters that already exist

Java's precedent is the enforce-and-count duality of
`RecordScanLimiter`/`ByteScanLimiter` inside `ExecuteState` plus
`FDBStoreTimer` metrics. Go has both halves already:

- **`ScanLimiterState`** (`pkg/recordlayer/scan_limiter_state.go:72-84`) is
  the Go port of exactly that duality (doc comment `:9-28`): every physical
  cursor already charges it per record/byte on the read path
  (`key_value_cursor.go:194-196`, `index_scan.go:531-532`,
  `record_key_cursor.go:77-82`, `text_cursor.go:102-103`,
  `count_index_maintainer.go:199-200`,
  `bitmap_value_index_maintainer.go:574-575`), and it is shared across all
  legs of a page by pointer (`scan_limiter_state.go:32-45`). Reading
  `RecordsScanned()`/`BytesScanned()`/`Elapsed()` (`:136-192`) at page end
  costs nothing new.
- **`StoreTimer`** (`pkg/recordlayer/store_timer.go:99`) is the
  `FDBStoreTimer` analog, attached per `FDBRecordContext`
  (`database.go:576,981,988`) with scan-timing events already recorded on
  the read path (`EventScanIndex` at `index_scan.go:301`,
  `EventGetReadVersion` at `database.go:919`). Finer FDB read-time
  attribution attaches a per-statement timer through the existing
  `SetTimer` seam rather than duplicating instrumentation.

```go
type Actuals struct {
        RecordsScanned int64         // Σ pages' ScanState.RecordsScanned
        BytesScanned   int64         // Σ pages' ScanState.BytesScanned
        RowsReturned   int64         // paginatingRows emitted counter
        Pages          int           // internal page count (per execution)
        WallTime       time.Duration // env clock, DST-seamed
        Termination    TerminationKind // Exhausted | ResultLimit |
                                       // BudgetExhausted | Error | Canceled
}
```

Aggregation across `EXECUTE CONTINUATION` resumes is gate-side (§4.7):
one `Done` per execution, `Resumed` distinguishing pages of a longer
logical statement, the gate's ledger summing to the statement's full cost.
Delivery point is the statement's terminal event — the same place the
timeout cancel is released (`Close`, `cascades_generator.go:1386-1397`) —
exactly once, on every path including error and abandonment.

**Free byproduct, recorded not promised:** `Done` pairs
`PlanInfo.EstimatedCost` with actual rows/bytes per `ShapeDigest` — the
estimate-vs-actual calibration signal CQ-11's statistics work
(TODO.md:1186-1189) needs. The gate makes the data collectible in
production; CQ-11 remains its own workstream and nothing here blocks on it.

### 4.9 `ShapeOf` moves to production

Production code must not import `pkg/relational/conformance/…`. `ShapeOf`
and `planTypeName` (`explaindiff.go:359-391`) move to the `plans` package
(where their input type lives), and `explaindiff` delegates — behavior
pinned by the existing factory dedup tests
(`factory/determinism_test.go:102`). The digest helper follows
(`factorycorpus/writer.go:32-42`).

## 5. Rejected alternatives

1. **Exposing the raw plan object.** `plans.RecordQueryPlan` and the memo
   are internal, release-unstable API (`plan_hash.go:16-18` documents even
   the hash as free to change); handing them out freezes the planner's
   internals forever and invites callers to walk mutable structure
   concurrently. PlanInfo is a stable value view; every field names its
   source so the view can be regenerated under any internal refactor.
2. **Cost-threshold-only gating.** The scalar is a comparator tiebreak
   over 1e6 defaults (§3, `plan_logging.go:47-49`,
   `properties/cost.go:57-66`), CQ-11 is open, and RFC-195's own defect
   table shows what these numbers did before clamping. A threshold on a
   number this soft over-admits and over-rejects at the same time. The
   properties lead; the number is offered with `LowConfidence` attached.
3. **SQL-level hints or session options as the admission mechanism.** The
   query author is the agent — the party being defended against. Policy
   must bind at the operator's seam, not travel in the untrusted text.
4. **Gate at the plan-construction site** (`cascades_generator.go:462/485`,
   the `PlanGenerationLogger` precedent). Skipped on every plan-cache hit
   (`:318-326`), fires for EXPLAIN (twice, when uncached, via `:619`), and
   can never see an RFC-203 resume (which performs no Cascades search).
   Wrong on all three axes; kept for what it is — observability.
5. **Gate at the `connection.go:408` plan/execute boundary.** Covers all
   statement kinds but sees only the three-method `query.Plan` interface
   (`pkg/relational/core/query/plan.go:60-64`) — no physical plan, no
   cost, nothing to decide with.
6. **Decision or consumed-so-far in the continuation envelope.** Violates
   RFC-203's schema-purity rule for the adopted proto
   (`rfcs/203:571-573`), and — decisive independently — the token is
   client-held and replayable, so any budget state in it is
   attacker-writable. Server-side gate ledger only. (§4.7.)
7. **Per-context gate override.** Tenant multiplexing belongs *inside* one
   gate (identity from ctx); two gate sources make the effective policy
   ambiguous per statement, and no ctx-value option mechanism exists in the
   driver to extend (measured: zero `context.WithValue` in
   `pkg/relational` production code — the `options.go:22-24` comment
   claiming otherwise is aspirational).
8. **Engine-side decision caching.** Admission decisions are time-varying
   policy (token buckets drain); the engine cannot know their TTL. The
   engine memoizes PlanInfo (P3); the user caches decisions if their policy
   is cacheable, on the promised `(ShapeDigest, QueryDigest, tenant)` key.
9. **Porting Java's `complexityThreshold` as the answer.** Fixed scalar
   policy, no tenant identity, throws from inside planning
   (`RecordQueryPlanner.java:329-333`) — and Go already has its analog
   (54F02). It bounds planner effort, not tenant cost; different problem.
10. **Reject-only gate.** Agents retry on error; a fleet of them turns a
    hard reject into a retry storm. Budgets convert the failure mode into
    bounded partial progress with an attributable bill — which is why
    `AllowWithBudget` ships in P1 with rejection, not after it.

## 6. Phasing and acceptance

Budgets are the centerpiece (owner directive), so they land in P1 with the
seam; the API never declares an arm before it works (no fake surface).

**P1 — the gate, all three decision arms.** Seam at
`cascadesPlan.Execute`; PlanInfo assembly (exported wrappers over
`concretePlanCostAndBounds` / `concretePlanCounts`; `ShapeOf` promotion
§4.9); registration (registry + DSN key + `Raw` interface); Reject → 53400
with context; fail-closed → 39F01; statement-scoped budget ledger and
`executeProps()` intersection. Acceptance: the FDB e2e suite in §7 items
1-6 green; unregistered-gate path proven unchanged (item 8).

**P2 — actuals.** Statement ledger surfaced as `Actuals`; `Done` delivered
exactly once per execution on every terminal path; per-statement
`StoreTimer` attachment. Acceptance: §7 item 7 — actuals equal the limiter
counters the enforcement read, on the same run.

**P3 — PlanInfo memoization + stability contract.** PlanInfo cached on the
plan-cache entry; digest-stability tests (same digest across cache
hit/miss, across processes); documented caching key. Acceptance: memoized
and fresh PlanInfo byte-equal; hot-path `Admit` overhead measured in the
plan-cache benchmark harness (`sqldriver/plan_cache_bench_test.go`).

**Gated residue — RFC-203 resume re-admission.** The constraint on the
resume path (§4.7.3) binds when RFC-203 lands; its conformance tests
(resume crosses the seam, `Resumed=true`, gate rejection of a resume)
land with that implementation, not before. This is a genuine external gate
(RFC-203 is itself DRAFT), not a deferral: the decision is made here, the
test obligation is booked there.

## 7. Test plan (deep-coverage rule for Go extensions)

Unit + real-FDB e2e (testcontainers, `t.Parallel()`, per-test prefixes),
all mutation-checked red→green:

1. **PlanInfo fidelity per shape** (FDB e2e): for each representative
   shape — PK point probe, unique-index probe, bounded range, unbounded
   full scan, covering index, IN-join, nested-loop join with and without
   predicates, in-memory sort, streaming aggregate, DML — assert the full
   PlanInfo vector (ScanClass, ProvenRows, Indexes, Joins, counts) against
   the plan the EXPLAIN assertion proves was chosen.
2. **Budget injection actually caps** (FDB e2e, tied to the existing
   scan-limiter coverage): a gate budget of N scan rows over an M≫N-row
   table terminates with 54F01 + gate context after ~N records — asserted
   via the same `ScanState` counters — across page boundaries (budget
   spans pages, per §4.4), for rows, bytes, and time (env-clock, DST
   seam); ResultRows truncates cleanly like `MAX_ROWS`.
3. **Rejection surface:** 53400 through `database/sql` via `errors.As`,
   context keys present (engine-merged + gate-supplied); wording not
   asserted (per `error.go:13-21`).
4. **Fail-closed:** panicking gate, erroring decision shape → 39F01, never
   execution; panic in `Done` cannot corrupt the statement result.
5. **EXPLAIN never gated:** a rejecting gate + `EXPLAIN SELECT …` returns
   the plan text; a counter-gate proves zero `Admit` calls. Pins §4.1's
   structural claim so a future EXPLAIN refactor re-arms loudly.
6. **Concurrency:** many connections, one gate, `-race`; per-tenant
   decisions from ctx; `Admit` counts equal execution counts under the
   plan cache (hit and miss both gated).
7. **Actuals = enforcement's own numbers:** on one run, `Done`'s
   `RecordsScanned/BytesScanned` equal the limiter state the budget was
   enforced against; `Done` fires exactly once on each terminal path
   (exhaustion, MAX_ROWS, budget, error, early Close).
8. **Null gate = no change:** with no gate registered, plan text, results,
   and the stress baseline are unchanged (stress comparison per CLAUDE.md
   before/after the P1 merge).

## 8. Residues, named

- **RFC-203 resume re-admission** — decided (§4.7), implementable only
  with RFC-203; booked as that RFC's binding constraint.
- **Scalar-cost quality** — unchanged by this RFC; CQ-11 owns it. The
  gate's `Done` stream is its calibration input (§4.8).
- **DDL/SHOW are ungated** — deliberate scope (§4.1); revisit only if a
  workload shows metadata-op abuse, which budgets cannot express anyway.
- **EXPLAIN cannot preview the gate's decision** — a `WOULD ADMIT?` probe
  is expressible later as a gate-side dry call on `PlanExplain`'s plan;
  not designed here.
