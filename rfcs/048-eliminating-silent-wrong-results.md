# RFC-048: Systematically eliminating silent-wrong-result errors in the query engine

**Status:** Accepted (Graefe ACK, Torvalds ACK) — implementing
**Scope:** Query planner + executor *result correctness* (the answers SQL returns).
Complements — does not replace — the existing wire-format conformance, chaos, and
fuzz infrastructure.

## Principle (first)

A **silent-wrong result** — the engine returns rows that are *plausible but
incorrect* (a NULL where a value belongs, a dropped group, a substituted
aggregate) with **no error, no crash, no log** — is the single worst failure mode
a query engine can have. It corrupts downstream decisions invisibly and survives
green CI. CLAUDE.md already names the mechanism: *"the gap is dimensional, not
volumetric"* — you can have 100 tests for a feature and miss the one axis that is
broken.

You cannot *prove* the absence of silent-wrongs. You can do three things, and this
RFC commits the project to all three:

1. **Make them LOUD** — independent oracles (a reference engine; metamorphic
   relations) that turn a wrong answer into a failing diff.
2. **Make them IMPOSSIBLE BY CONSTRUCTION** — one source of truth for every
   name/key two code paths must agree on; runtime invariants that refuse to emit
   an unresolved reference.
3. **Make them MEASURABLE** — mutation testing that scores whether the suite can
   even *detect* a wrong result, so "covered" stops being a feeling.

## Motivation — Exhibit A (lived, not hypothetical)

While shipping item 60 (GROUP BY/HAVING in correlated scalar subqueries, RFC-047),
a feature that had already passed Graefe (Cascades) + Torvalds (code) + @claude
review, an independent Codex pass found a **class** of silent-wrongs the entire
existing suite and three reviewers missed — and kept finding more on each patch:

* aggregate slots were materialised under one name (`COUNT(*)`) while the HAVING
  rewrite looked them up under another (`COUNT(1)`) — a **producer/consumer naming
  split** that only *coincidentally* agreed for common cases;
* the divergence reached `COUNT(1)` vs `COUNT(*)`, `COUNT(DISTINCT 1)`, a HAVING
  repeating the visible aggregate, decimal-literal args (`COUNT(1.5)` → a dot in
  the name → mis-parsed), and nested-arithmetic args (`SUM((amount+1)*2)` → a
  lossy parser round-trip);
* every instance failed *silently* — NULL / dropped groups — never an error.

Five independent silent-wrongs, in one Go-only read extension, found only by a
fourth reviewer probing an unprobed dimension. The inference is not "that feature
was buggy" (it is fixed and pinned); it is **that a rigorous human+LLM review
process is not a systematic floor against silent-wrongs — generation, invariants,
and measurement are.**

## Why the current infrastructure misses them

The project already has strong correctness infrastructure; here is precisely where
each stops short of the silent-wrong class:

| Existing | Catches | Misses (the gap) |
|---|---|---|
| Cross-engine conformance (`pkg/relational/conformance/plandiff` corpus) | hand-picked shapes, plan-shape diffs | **curated, not generative** — only the dimensions a human enumerated |
| Java conformance server (`conformance/*.java` + `_test.go`) | targeted Java-vs-Go behaviour | per-feature, fixed inputs; no random shape exploration |
| Fuzz targets (`grep ^func Fuzz`) | panics, crashes, parser robustness | crash-oracle only — a *wrong result* is not a panic |
| Chaos (`pkg/recordlayer/chaos`, model-based) | record-layer state vs `StoreModel` under faults | the model shadows *storage*, not *query results* |
| Reviewer gates (Graefe/Torvalds/@claude/codex) | architecture, code, sampled corners | **sampling, not systematic**; misses unprobed dimensions until someone probes them |

The common thread: **no generative result-oracle**, **no result-level invariants**,
and **no measurement of detection power**.

## The program (six workstreams)

### W1 — Executor invariant: no unresolved reference *(cheap, highest catch-per-effort, do first)*
When a predicate/projection/HAVING references a column name that is **not** a
materialised key in the row it evaluates against, the executor (under a test/debug
build flag, default-off in prod) must **fail loudly** instead of evaluating to
NULL. *Every* silent-wrong in Exhibit A was a lookup of a name that did not exist
resolving to NULL — this one invariant catches the whole class at the point of
failure. ~tens of lines, gated; prod stays lenient.

### W2 — Metamorphic / TLP property testing *(systematic engine for the no-oracle surface)*
For the Go-only query surface (correlated scalar subqueries, Go-only joins) there
is **no Java reference**. Use **oracle-free metamorphic relations** over generated
queries on the real-FDB test schema:
* **Equivalences as oracles** — `COUNT(*) ≡ COUNT(1) ≡ SUM(1)`; `x ≡ x+0`; FROM
  reorder / re-alias / redundant `AND TRUE` must not change results. *The
  `COUNT(1) ≡ COUNT(*)` relation alone catches Exhibit A.*
* **TLP (Ternary Logic Partitioning)** — `WHERE p` ∪ `WHERE NOT p` ∪ `WHERE p IS
  NULL` must reconstruct the unfiltered set (catches predicate/NULL/3VL bugs with
  no reference).
* **Decomposition** — a correlated scalar subquery's value equals the inner query
  run standalone with the outer correlation substituted.
A small generator emits random valid queries over the conformance schema; each
relation is an assertion. This is the durable net that finds the *next* dimension
before a reviewer does.

*Caveat — the relations must be exactly true under SQL semantics, not
approximately.* TLP requires strict three-valued logic: the partition is `WHERE p`
∪ `WHERE NOT p` ∪ `WHERE p IS NULL`, and it only reconstructs the unfiltered set
because the third arm captures the rows where `p` is UNKNOWN — drop it and the
oracle reports false violations on every NULL. Arithmetic identities have the same
trap: `x ≡ x+0` fails on float rounding and on integer overflow, and `COUNT(*) ≡
SUM(1)` differs on the empty group (0 vs NULL). Each relation must be stated with
its domain restriction (integer-only, non-overflowing, NULL-aware) or it generates
noise that trains the team to ignore the oracle. Start with the relations that are
*unconditionally* true (`COUNT(*) ≡ COUNT(1)`, FROM reorder, redundant `AND TRUE`)
and add domain-restricted ones deliberately.

### W3 — Generative differential testing vs Java *(strongest oracle, for the shared surface — gated on Track C2)*
Extend the existing conformance harness from a *curated corpus* into a
**generative differential tester**: random valid SQL over random schema+data, run
on both Go and the Java conformance server, **diff results row-for-row**.
Silent-wrongs become loud diffs. This is the biggest correctness multiplier for
the SQL surface Java also implements.

**Hard gate — name it honestly.** The Java server, the curated corpus, and the
row-for-row diff logic already exist (`pkg/relational/conformance/plandiff`), but
the **Go side of that harness is a stub**: `runsql.go:84`
`ErrGoUnimplemented = "plandiff: Go runner not yet implemented (waits on Track C2
QueryExecutor)"`. So W3 is *not* "extend a working differ" — it is **blocked on
Track C2 (in-process QueryExecutor)** for the plandiff route. Two ways forward,
pick deliberately: (a) complete the C2 Go runner so the existing diff harness
lights up end-to-end, or (b) route generated SQL through the already-working
`sqldriver` (database/sql → real FDB) execution path and diff that against the
Java server, bypassing the stubbed plandiff runner. The **generator** and the
**Java-baseline** half can be built now regardless; the Go-execution half of the
plandiff harness specifically waits on C2. This RFC does **not** count W3 as
landable until that gate is named in the plan and one of (a)/(b) is chosen.

### W3.5 — Plan-diversity oracle *(the Cascades-specific silent-wrong, self-oracling)*
A Cascades optimizer's defining hazard: **one logical query has many legal
physical plans**, and an unsound transformation rule returns wrong rows *only on
the plan it fires on*. W3 (differential-vs-Java) compares **Go's chosen plan**
against **Java's chosen plan** — it does **not** compare Go's chosen plan against
Go's *other* legal plans, which is exactly where a bad implementation rule hides
(both engines might pick the same safe plan and agree, while a rarely-costed
alternative is silently wrong). The oracle: for one generated logical query, force
the optimizer to enumerate **multiple** physical plans (disable selected rules /
perturb the cost model / `forcePlan` hooks) and assert **all plans return
byte-identical row sets** (modulo ORDER BY). This is **self-oracling** — no
reference engine needed — and it is the *only* workstream that directly targets
transformation-rule unsoundness, the Cascades silent-wrong. It complements W2
(which varies the *query*) by varying the *plan* for a fixed query. Land it
alongside W2: both are oracle-free and need no external gate. Where the executor
exposes plan-forcing for tests this is small; where it does not, the hook is the
first deliverable.

### W4 — One source of truth for every name/key *(impossible-by-construction)*
Exhibit A's root was two code paths independently coining a name that must match.
The RFC-047 fix — factoring **one** `canonicalAggName` that both producer
(`buildCorrelatedScalar`) and consumer (`rewriteAggregateValue`) call — is the
**template**. Do a deliberate sweep of every seam where two places independently
compute a key/alias/name that must agree (aggregate names, `JoinMergeResultValue`
merge-row keys, alias qualification, continuation keys, scalarCol) and **unify or
assert-equal** them. Each such seam is a silent-wrong factory.

### W5 — Mutation testing *(the correctness scorecard — feasibility first, not yet a gate)*
The idea: mutate the planner/executor (flip a comparison, drop a dedup, change an
aggregate accumulation); a **surviving mutant = a dimension the suite cannot
detect a wrong result on**. This is the only honest measure of the suite's
silent-wrong-detection power — line coverage is volumetric and lies here.

**Be honest about feasibility.** There is **no mutation tooling wired in this
repo today**, and the off-the-shelf Go options (`gremlins`, `go-mutesting`) were
built for fast in-process unit tests — they re-run the suite once *per mutant*,
and this suite's result-bearing tests spin **real-FDB testcontainers** (seconds
each, `--local_test_jobs=4` capped). Naïvely mutating the whole planner/executor
and running the FDB suite per mutant is hours-to-days per pass — not a CI gate.
So W5 is **scoped, not promised**: (1) pick `gremlins` (actively maintained,
Bazel-runnable); (2) target only **pure, FDB-free** units first — `canonicalAggName`,
`parseAggregateText`, cost comparison, dedup-key projection, aggregate
accumulation — run against the **unit** tests, not the container suite; (3)
publish the mutation score on those units as a *tracked number*, ratcheted, before
proposing any gate. Mutation-as-CI-gate over the full executor stays a **non-goal
until the unit-scoped pass proves the tool and the runtime are tolerable.** No
placeholder thresholds: the first deliverable is the score on the pure units, and
that number sets the bar.

### W6 — Dimensional coverage discipline *(process, makes W1–W5 a habit)*
For each operator, enumerate the **axes** (correlated/not, single-source/join,
expr/bare arg, HAVING-same/different-aggregate, empty/non-empty groups, NULL keys,
multi-group, decimal/integer literal …) and require a test per axis-*combination*,
not per feature. A reviewer checklist gate; the generators in W2/W3 then explore
the combinatorial space humans don't enumerate.

## Sequencing (cheap-and-catching first)

1. **W1** (executor invariant) + **W2** equivalence/TLP relations + **W3.5**
   plan-diversity oracle — all small, all oracle-free (no external gate), and W1,
   W2, and W3.5 each independently catch a distinct silent-wrong class (name→NULL,
   query-equivalence, rule-unsoundness). Land first.
2. **W4** sweep (one-time architectural debt pass) — unify-or-assert-equal every
   producer/consumer name seam.
3. **W3** generative differential vs Java — the discovery engine for the shared
   surface. **Gated on Track C2** (or the `sqldriver` route, W3 (b)); cannot land
   until that gate is chosen.
4. **W5** mutation testing — feasibility-scoped on pure units first, score
   published before any gate; **W6** as the review gate.

## Success criteria

* W1 invariant on under the test/debug flag, suite green — i.e. **no production
  code path emits an unresolved reference today** (if the invariant trips, that is
  a found bug, fix it).
* W2 harness runs a fixed budget of generated queries per CI run with **zero**
  metamorphic violations; a **deliberately reintroduced Exhibit-A bug is caught**
  by the suite (the `COUNT(1)≡COUNT(*)` relation) — this is the concrete
  acceptance test, not a query-count threshold.
* W3.5: for each generated query, every enumerated plan returns identical rows; a
  deliberately unsound rule is caught by a plan that disagrees.
* W3 (once C2/sqldriver gate is met): generated queries, **zero** Go-vs-Java
  result diffs modulo documented divergences.
* W5: a **published mutation score on the pure units** (`canonicalAggName`,
  `parseAggregateText`, cost comparison, dedup/accumulation), tracked and
  ratcheted — no thresholds asserted until that first number exists.

## Non-goals

Not a rewrite of the planner/executor; not a replacement for wire-format
conformance or chaos (those guard *storage/format*, this guards *answers*). Prod
behaviour is unchanged — W1's invariant is test/debug-gated. W3 is **not** counted
as landable until the Track C2 (or `sqldriver`) execution gate is chosen. W5 as a
full-executor CI gate is **out of scope** until the unit-scoped feasibility pass
proves the tooling and runtime are tolerable.
