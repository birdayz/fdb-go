# RFC-185: Remove the filter/set-operation commutation rule family

**Status:** Draft, revised after Graefe + Torvalds RFC-review. Both gave
ACK-WITH-CHANGES; this revision folds their required evidence. Seeking the
delta re-confirmation before implementation.

## Summary

Four logical-rewrite rules commute a filter across a set operation and its
inverse:

- `PushFilterThroughIntersectionRule` (default_rules.go:48)
- `PullCommonFilterAboveIntersectionRule` (:65)
- `PushFilterThroughUnionRule` (:47)
- `PullCommonFilterAboveUnionRule` (:63)

They are **broken and unexercised**: the inverse pair interns into a **cyclic
memo** (planner stack overflow, or non-termination if the cycle is guarded),
and on the 2407-query corpus they **never once perform their rewrite** (zero
yields). This RFC proposes removing all four.

The grounds are brokenness + the rewrite being unreachable from the corpus, NOT
"Java lacks them" — Go-only read-side rules are explicitly permitted when they
earn their keep (CLAUDE.md). These do not earn it, and they crash when they do
fire.

CORRECTION FROM THE FIRST DRAFT (caught by Torvalds' delta review, which asked
exactly this): the first draft claimed the rules "fire 1282x and change zero
winning plans". That counted MATCHER ENTRIES, not rewrites — the matcher
matches any LogicalFilter, and OnMatch bails immediately (`if !ok { return }`)
when the filter's inner is not the target set operation. Instrumenting the
actual `call.Yield` instead gives ZERO yields across all 2407 corpus queries.
So the honest claim is stronger for removal AND more precise: the rules never
perform their rewrite on any real query; the only inputs that make them yield
are fuzz-generated Filter-over-set-op shapes, and those crash.

## The bug (structural, not a one-seed fluke)

`GetCorrelatedTo` (reference.go:722) recurses through child references at :752
with no cycle guard. A cyclic reference in the memo overflows the stack.

Mechanism, established by instrumenting `Reference.Insert` to trap a
just-inserted member that transitively reaches its own reference:
`PullCommonFilterAboveIntersectionRule.OnMatch` (:59) builds
`newX = Intersection(A,B)`, calls `MemoizeExpression(newX)` — which INTERNS and
can return a reference reaching the group being explored — and yields
`Filter([P], newXQ)`, closing the cycle. `MemoizeExpression`'s existing guard
(expression_rule_call.go:155) catches only the DIRECT self-loop, not the
transitive intern-to-ancestor.

This is a structural argument about inverse-pair interning, not a property of
one input. The single fuzz seed (`[]byte("\x7fyy1")`) is an existence witness,
not the proof.

Pre-existing: the seed crashes on master before RFC-183 (`15dc17a82`);
RFC-183 left `GetCorrelatedTo` byte-unchanged.

## Measured evidence

**Yield counts on the 2407-query corpus** (instrumented each `call.Yield`, ran
`cmd/explain-differ`): **all four rules yield ZERO times.** The rewrite never
fires on any real query. (Matcher ENTRIES were 1282/1282/34/0 — the matcher
matches every LogicalFilter — but every entry bails before yielding because the
inner is not the target set operation. The entry count is not the rewrite
count; the yield count is.)

Confirmed the instrumentation is not silently broken: exploring 256
fuzz-generated inputs with it in place yields non-zero (`push_union:59
pull_union:18`, etc.). So zero on the corpus is a real "never rewrites," not a
dead counter.

This reframes Torvalds' #2 honestly rather than papering it: zero-drift on the
corpus is EXPECTED because the rules never rewrite there — the corpus does not
contain Filter-over-set-op shapes. Zero-drift alone therefore does NOT prove
the rewrite is useless in general; it proves the corpus does not exercise it.
The removal case does not rest on "useless" — it rests on:

1. The rules CRASH when they do fire (the cyclic memo), and the only observed
   firings are fuzz inputs that crash.
2. On the entire corpus they never rewrite, so removing them is drift-free
   there by construction.
3. OPEN (does not block removal, but stated plainly): whether a real front-end
   query can even produce a Filter directly over an Intersection/Union of
   common-filtered legs. Intersections arise from index data-access already
   below any filter, so the trigger shape may be unreachable from real SQL — if
   so the rules are pure fuzz-only crash surface. This is asserted as plausible,
   NOT proven; the removal stands on (1)+(2) regardless.

**Zero explain-diff drift** across all 2407 queries when all four rules are
removed (`explain-differ diff`: `identical=2407 differing=0 shape_flips=0`) —
consistent with, and explained by, the zero-yield finding above.

**Crash class cleared** — with the rules removed, the four previously-crashing
fuzz targets run clean at 700k–790k execs each (MemoConsistency 793k,
Determinism 704k, Idempotence 770k, InitialMemberPreserved 754k). This is the
broad-fuzz confirmation, not one seed.

## What the differ cannot see (Torvalds #3, Graefe b)

`explain-differ` records Explain text + node Shape only: **no cost, no
ordering, explain-only (nil stats), single run**. A winner-flip to a
same-shape / different-ordering plan is invisible to it. Two things bound this
risk:

1. The removed rules never yield on the corpus (measured), so there is no
   winner to flip there at all — the differ blindness is moot for this change
   on this corpus. It matters only for the OPEN question of unseen real queries.
2. The go/no-go is not "zero drift" alone. It is: zero drift AND the fallback
   for any future query that would want these plans is the match-then-implement
   DATA-ACCESS path (how Java reaches them), never a resurrected cyclic rule.

Implementation will additionally run the diff across N determinism iterations
(via `FuzzPlanner_Determinism` / repeated dumps), not a single dump.

## Why a guard is not the resolution

The obvious alternative — extend `MemoizeExpression`'s direct-self-loop guard
to transitive reachability — was implemented and disproven ON THE REPRODUCING
SEED: it stops the overflow but yields "exploration did not converge." The
ARGUMENT is not the seed; it is structural: interning is what both bounds the
inverse-pair fixpoint (dedup) and maps the intermediate onto an ancestor group
(the cycle). Remove the dedup to break the cycle and the fixpoint livelocks.

This does not prove NO cycle-safe redesign exists — only that the naive guard
fails and that these specific rules are the wrong thing to preserve, given they
never rewrite on the corpus. A cycle-safe redesign would be resurrecting rules
whose only observed firings are fuzz inputs that crash; not worth it.

Correction to an earlier draft (Graefe c): dropping EITHER rule of a pair
breaks the cycle, so "only removal converges" was overstated. Removal is
justified by redundancy + brokenness, not by being the unique cycle-breaker.

## Proposal

Remove the four rules, their four unit tests, and their four registry lines.

## Union (Torvalds #5, Graefe d)

Union removal is drift-free (the 2407-query measurement includes Union-bearing
queries) and fuzz-clean, and PullCommonFilterAboveUnion/PushFilterThroughUnion
yield ZERO times on the corpus — same never-rewrites-here as the Intersection
pair. The Union CYCLE itself is by STRUCTURAL ANALOGY (same
inverse-intern shape); it has not been reproduced by a fuzz seed. Implementation
must attempt a Union reproducer in the fuzz run; the removal stands on
zero-drift + never-rewrites-on-corpus regardless, but the analogy is labeled, not
asserted as reproduced.

## Verification plan (implementation, separately gated)

1. Zero explain-diff drift over N determinism runs — the go/no-go.
2. The four crashing fuzz targets clean at ≥200k execs; attempt a Union
   reproducer.
3. Full `just test` green; removed rules' unit tests deleted with them.
4. Confirm planning time does not regress (removing four never-yielding rules
   removes their per-Filter matcher work, so if anything it reduces it).

## What I am asking for

Delta ACK that the folded evidence (fire counts, measured zero drift,
broad-fuzz clean, honest differ-blindness framing) satisfies the RFC-review
conditions, to proceed to implementation.
