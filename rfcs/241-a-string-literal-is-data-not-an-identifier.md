# RFC-241 — A string literal is data, not an identifier

**Status:** ACCEPTED — revision 10. Graefe ACK and Torvalds ACK, both on this head. The FIX has
not changed since revision 2; r3-r9 were the machinery proving it safe. r3-r5 were NAK'd for a
static gate each holding a strict SUBSET of the invariant; r7 replaced it with validation at the
point of use, which both reviewers independently NAK'd for a residual admitting a PERMUTATION of
two same-function entries — this RFC's own defect class. r9-r10 close it with an EXACT
operand-text checksum on the structural bind, needing no new state on the node.

**Area:** SQL front-end → Cascades aggregate lowering (query-engine gate: Graefe + Torvalds)
**Fixes:** TODO.md §1, "An aggregate is bound to its output slot by a RENDERING that cannot tell
two aggregates apart"

**What revision 1 got wrong**, recorded because one of its errors was the RFC's own load-bearing
claim and both reviewers found it independently:

- It asserted `logicalAggregateCalls` is the SOLE producer of `agg.Calls`, proving it by "and
  `NewAggregate` is the only assignment of the `Calls` field". That is a non-sequitur — it counts
  assignments, not builders of the slice passed in. `git grep -n 'NewAggregate(' -- '*.go' | grep -v
  _test` returns FOUR call sites (§ Investigation). The index scheme's correctness rested on the
  false claim.
- It proposed putting the index on `logical.AggregateCall`. That struct is memo-facing and is built
  by composite literal at 4 non-test sites (39 including tests), so a new int field defaults to `0`
  and every literal that does not set it silently claims `aggCols[0]` — a sentinel that fails
  toward a WRONG binding rather than no binding.
- It argued the two producers share a backing array because `selectQuery` embeds
  `selectClassification` by value. True but insufficient: a copied slice header is broken by a
  later `append` to either side. The real invariant is narrower and is stated below.
- It rejected "exact-then-fold" as the fix and then retained `EqualFold` in its own fallback.

## Problem

Two aggregates whose operands differ only in the case of a **string literal** silently compute the
same thing. Measured against a real cluster:

```sql
-- sales: (1,'US',100,1) (2,'US',200,2) (3,'EU',300,3)
SELECT COUNT(CASE WHEN "Region" = 'us' THEN 1 END),
       COUNT(CASE WHEN "Region" = 'US' THEN 1 END) FROM sales
  go   -> [2, 2]        correct -> [0, 2]
```

It is a silent wrong answer on ordinary SQL — no error, no plan anomaly, and the two result columns
even carry the two *correct, distinct* labels, which is what makes it invisible.

The defect is **last-wins**, not "both read the second slot", and the reversal proves it:

| query | go | correct |
|---|---|---|
| `COUNT(…'us'…), COUNT(…'US'…)` | `[2, 2]` | `[0, 2]` |
| `COUNT(…'US'…), COUNT(…'us'…)` | `[0, 0]` | `[2, 0]` |
| `SUM(CASE …'us'… THEN "Amount" END), SUM(…'US'…)` | `[300, 300]` | `[NULL, 300]` |
| `COUNT(CASE WHEN 'us' = "Region" …), COUNT(WHEN 'US' = …)` | `[2, 2]` | `[0, 2]` |
| grouped by `plain` | `[1 1 1] [2 1 1] [3 0 0]` | `[1 0 1] [2 0 1] [3 0 0]` |
| control — `'EU'` vs `'US'` | `[1, 2]` | `[1, 2]` ✓ |

Both columns always carry the value of the **last** aggregate in the SELECT list. Reversing the pair
reverses which value both columns show.

## Investigation

**The TODO entry's diagnosis is wrong, and both halves of it are checkably wrong.** It is recorded
here because the entry was the starting point and would have sent the fix to the wrong layer.

1. It cites the failing path as `aggregateValueOutputName` → `aggregateOrdinalFor` in
   `cascades_translator.go`. `git grep -n 'aggregateOrdinalFor' -- .` returns exactly **one** hit:
   the TODO entry's own prose. (Positive control: the same sweep for `aggregateValueOutputName`
   returns 2 files, so the search is well-formed.) The function does not exist.

2. It says the two aggregates "are distinguished by nothing". They are distinguished by two
   authorities and conflated by a third. `EXPLAIN` on the failing query:

   ```
   Project([_current.COUNT(CASEWHENRegion='us'THEN1END)#0,
            _current.COUNT(CASEWHENRegion='US'THEN1END)#1],
           StreamingAgg(keys=[], Scan(SALES)))
   ```

   The projection reads **two different ordinals** under **two different names**. The post-aggregate
   ordinal bind the entry blames is correct. So is the result-metadata label authority —
   `quoted_identifier_aggregate_labels.yaml` already pins the two labels apart.

**The actual defect is one layer earlier, in operand RESOLUTION.**
`upgradeAggregateOperands` (`pkg/relational/core/embedded/logical_predicate.go`) walks the parsed
aggregate columns and, for each, finds the `agg.Calls` entries it should supply a resolved operand
Value for. It matches the call's rendered operand text against the parsed argument's spelling with

```go
if sp != "" && strings.EqualFold(call.Operand, sp) {
    idxs = append(idxs, i)
```

and then writes `operands[idx] = v` for every index it collected. The comparison is at
`logical_predicate.go:5295`.

`call.Operand` is the canonical *text* of the operand — `CASEWHENRegion='us'THEN1END`. It contains
identifiers (already normalized upper at this layer) **and string literals, which are data**.
`EqualFold` cannot tell them apart. Instrumented on the failing query:

```
AGGPROBE call[0].Operand="CASEWHENRegion='us'THEN1END" spelling="CASEWHENRegion='us'THEN1END" equalfold=true exact=true
AGGPROBE call[1].Operand="CASEWHENRegion='US'THEN1END" spelling="CASEWHENRegion='us'THEN1END" equalfold=true exact=false
AGGPROBE call[0].Operand="CASEWHENRegion='us'THEN1END" spelling="CASEWHENRegion='US'THEN1END" equalfold=true exact=false
AGGPROBE call[1].Operand="CASEWHENRegion='US'THEN1END" spelling="CASEWHENRegion='US'THEN1END" equalfold=true exact=true
```

Every aggregate column matches **both** calls. Each therefore writes both slots, and the second
column's write clobbers the first's — last-wins, which is exactly the table above. The `'EU'`/`'US'`
control shows `equalfold=false` on the cross pairs, which is why it answers correctly.

**This exact bug is already documented one file away, and the sibling site was never fixed.**
`normalizeAggOutputName` (`cascades_translator.go`) carries this in its own doc comment:

> It is no longer CASE-insensitive, and the old doc said it was. The fold went with RFC-237: both
> sides now carry the operand's declared spelling, so a case-insensitive key would conflate two
> aggregates that differ only in a case-sensitive token — the collision
> `COUNT(CASE WHEN s='x' …)` and `…'X'…` produce, which is a wrong ANSWER rather than a wrong name.

RFC-237 removed the fold from the naming key and left the identical fold standing in the operand
matcher. This is the failure CLAUDE.md names: a correction is not done until a grep for the
superseded rule returns zero **across every file**.

**What the fold is actually load-bearing for — measured, not assumed.** The four shapes that could
depend on it:

| shape | result |
|---|---|
| `SELECT plain, SUM("Amount") … HAVING SUM("Amount") > 150` | ONE call, `exact=true` |
| `SELECT plain, SUM(plain) … HAVING SUM(PLAIN) > 1` | ONE call, `exact=true` |
| `SELECT SUM(plain), SUM(PLAIN)` | TWO calls, both `Operand="PLAIN"`, both `exact=true` |
| `SELECT s.plain, SUM(s."Amount") … GROUP BY s.plain` | qualifier handled by the SPELLINGS list, then `exact=true` |

Identifier case appears to be normalized before this comparison — the third row shows `SUM(plain)`
and `SUM(PLAIN)` both arriving as `PLAIN`. That is a CORPUS FACT (20235 matches, 0 fold-only), not
a proof about all inputs, and it must not be read as one: the argument for dropping the fold does
not rest on it. What rests on nothing is the failure DIRECTION — an exact compare converts any
counterexample into a loud typed error rather than a silent rebind. Coverage is what the corpus
measures; the mitigation is what the exactness guarantees, and only the second is a claim about
inputs nobody has run.

The qualifier-strip the fold's neighbouring comment describes is served by the two-entry
`spellings` list, not by the fold: `S.Amount` fails **both** `exact` and `EqualFold`, and the bare
`Amount` then matches exactly.

**The fold is dead weight, and that is measured over a stated population.** Instrumenting the match
site with two counters — every match, and the subset that is fold-only (`EqualFold && !exact`) —
and running `bazelisk test //pkg/relational/... --nocache_test_results` (28 of 28 targets executed,
28 pass), with the reproducer probe REMOVED so it cannot contaminate the count:

```
20235   matches at the site          (positive control)
    0   of them fold-only
   10   of 28 targets reach the site (the other 18 build no aggregate)
```

The positive control is the load-bearing half. A zero with no control is not a measurement — the
first attempt at this census reported `0` from a `tee` pipeline whose captured half held only a
filtered tail, over a run in which three packages had silently hit `go test`'s 10-minute default;
the full log held 12, all of them the probe's own queries. At 20235 matches the population is
plainly non-empty, so the zero is a real zero rather than an absent one.

So the case-insensitive comparison decides nothing on this tree except the defect. That is a fact
about a population, not a proof about all inputs — which is why the fix below does not merely
delete it.

## Fix

**Stop re-deriving a correspondence the producer already knows.** The text match exists to answer
"which `agg.Calls` entry did this aggregate column produce?" — and `logicalAggregateCalls` answered
that when it built the list. Return the answer instead of reconstructing it from a rendering.

`logicalAggregateCalls` (`logical_builder.go:55`) walks `aggCols` in order and appends one call per
entry, plus an optional leading synthesized `COUNT(*)` (`:63`) and a `continue` that skips a
harvested duplicate `COUNT(*)` (`:90`). The correspondence is exact but NOT positional, which is
why it must be RECORDED rather than assumed.

1. `logicalAggregateCalls` returns a **side table** — `callToAggCol []int`, parallel to the calls it
   returns, holding the index of the `aggCols` entry that produced each call and `-1` for the
   synthesized `COUNT(*)`.
2. `upgradeAggregateOperands` binds each resolved operand to the calls the table names.

**The index goes on a side table, NOT on `logical.AggregateCall`.** That struct is memo-facing and
shared; a field meaning "position in a parser array" invites exactly the identity misuse `Operand`
already caused. Go's zero value makes it worse: `AggregateCall` is built by composite literal at 4
non-test sites (`logical_builder.go:63`, `:77`, `logical_predicate.go:12275`, `:12316`) and 39
including tests, so an unset int field reads as `aggCols[0]` — a sentinel that fails toward a WRONG
binding. A side table cannot be silently under-populated: it is returned with the calls or it is
absent.

**The sole-producer claim is retracted.** `NewAggregate` has four non-test call sites:
`logical_builder.go:652`, `plan_visitor.go:1546`, `logical_predicate.go:12404`, and `:12641` (nil
calls). The third builds its calls from a local `addAgg` closure (`:12211`) with its own `aggSeen`
dedup keyed by canonical name — a genuine many-to-one from aggregate columns to calls, the shape
revision 1 called impossible by construction. It also **resolves its own operands in place**
(`opVal` from `resolver.WalkExpression`, assigned at `:12405`) and therefore never needs
`upgradeAggregateOperands`; `findAggregate`'s single-child chain walk does not reach it today.
That is stated rather than relied on — the census floor below is what notices if it changes.

Worth recording because it is the design this RFC cannot reach: `:12211` already does what Java
does — construct the operand Value at production — and already handles the collision correctly,
rejecting rather than overwriting when two aggregates cannot be told apart by name ("We cannot
disambiguate them by name, so reject rather than return wrong rows", `:12216`). The correct-or-loud
treatment exists in this tree, one function from the site that silently returns wrong rows.

**Why the operand is not resolved at the producer, which is the Java-shaped end state.**
`logicalAggregateCalls` has two callers and they do NOT agree on what is in scope:

- `plan_visitor.go:1538`, inside `visitSelectGroupBy`, **can** build a resolver and does so 34
  lines above at `:1504` — `buildSelectScope(selectQueryFromClassification(cls, fs), v.md,
  v.schemaName, v.cteScopes)`, conditionally, for computed GROUP BY keys.
- `logical_builder.go:649`, inside `buildSelectShell(op, sq, stripPrefix)` (`:628`), **cannot**:
  that signature carries no `md`, no `schemaName` and no `cteScopes`, so the ingredients a resolver
  needs are not there.

Both callers of one shared function must agree on what it may do, so the shared producer cannot
resolve operands until `buildSelectShell` is given the catalog context — a signature change through
the second SQL builder. That is why resolution is a separate later pass that builds its own
resolver (`upgradeAggregateOperands`, `logical_predicate.go:5225-5231`).

(Revision 2 originally wrote "neither of which has a semantic resolver in scope". That is false for
the plan_visitor caller and was corrected on review. The scope boundary survives; the reason for it
is the asymmetry above, not a uniform absence.)

Converging on `:12211`'s shape therefore means plumbing catalog context into `buildSelectShell`, a
change to lowering order that deserves its own RFC and does not belong in a wrong-answer fix. It is
booked in `TODO.md` §4 with a pointer back to this RFC's side table, which is the mechanism that
makes that later change a deletion rather than a rewrite.

**The shared-array invariant, on the third attempt.** Two earlier statements of it were wrong, and
the way they were wrong is worth keeping because both looked rigorous.

Revision 1 argued the producers index the same array because `selectQuery` embeds
`selectClassification` by value (`select_parser.go:833`). True and insufficient: a copied slice
header is silently broken by a later `append` to either side.

Revision 2 then claimed every `append` lives inside `classifySelectElements`
(`select_parser.go:862-1812`) and that "after that only ELEMENT mutation occurs". Also wrong, and
sloppily so — it listed **eight** header mutations where **twelve** exist (missing the appends at
`:1693`, `:1801`, `:1807` and the whole-slice rebind at `:1547`), missed the element writes at
`:1671` and `:1672`, and cited element writes at `:1392`/`:1642`/`:1643`/`:1670` as happening
"after" appends that in fact occur *later in the same function* (`:1801`, `:1807`). The sentence
described nothing coherent.

**The invariant that actually holds is about VISIBILITY, not ordering.** `aggCols` and
`aggSelectCol` are unexported, so the population that can touch the slice header is one package.
All TWELVE header mutations are inside `classifySelectElements` —
`grep -nE '(^|[^.a-zA-Z])(cls\.)?aggCols[[:space:]]*=[^=]' select_parser.go` returns `:920 :981
:984 :986 :999 :1005 :1092 :1141 :1547 :1693 :1801 :1807`, all within `862-1812`, none after.
Outside `select_parser.go` there is no `append` and no whole-slice assignment: every use is a READ
— `len`, a `range`, an index read, or one of three pointer-takes.

The pointer-takes need their own accounting, because a pointer handed to a function is exactly
where a write hides:

- `logical_predicate.go:12327` (`ac := &sq.aggCols[i]`) — read at `:12328`, `:12340`, `:12345`,
  `:12351`, `:12357`, `:12358`, `:12360`, `:12361`. Eight reads, no write.
- `:12393` and `:12616` (`visibleGroupCol = &sq.aggCols[i]`) — read at `:12422-12425` and
  `:12672-12676`, AND the pointer **escapes** into
  `resolveCorrelatedVisibleGroupKeyOrdinal(aggOp, visibleGroupCol, resolver)` at `:12414` and
  `:12655`. That callee (`:11749-11791`) reads `groupColBare`, `groupColQualifier` and
  `groupColQualified` (`:11754`, `:11760-11762`) and writes nothing.

So the header the producers and `upgradeAggregateOperands` hold cannot diverge after
`classifySelectElements` returns.

### The invariant is VALIDATED AT USE, not gated statically — and four failed gates are why

Revisions 3 through 5 each proposed a static gate over this invariant, and each was NAK'd for
holding a strict subset of it:

| draft | gate | shape that slipped |
|---|---|---|
| r3 | no `aggCols` mutation outside `select_parser.go` | a helper added INSIDE that file |
| r4 | every `aggCols` mutation inside `classifySelectElements`, walking only `select_parser.go` | `sq.aggCols = append(…)` added to `logical_predicate.go` |
| r5 | both of the above, package-wide | `*cls = normalized(cls)` — a write that never names `aggCols` |

The fifth finding is the one that settles the approach rather than extending it — **and it is a
HYPOTHETICAL shape, not an in-tree site.** `visitSelectGroupBy` (`plan_visitor.go:1433`) holds
`cls *selectClassification` and is the function that calls `logicalAggregateCalls` at `:1538`, so a
`*cls = …` added there WOULD replace the whole classification, `aggCols` header included, while
containing no `aggCols` token. No such write exists: `grep -c '\*cls = ' plan_visitor.go` returns
0 against a control of 31 `cls` references in that file. An earlier revision of this paragraph read
as though the site were real, which would have rested a design change on a citation that does not
resolve.

**The aliasing shape it illustrates IS in the tree**, at `select_parser.go:833` —
`selectClassification: *cls`, a whole-struct copy that carries the `aggCols` header into a second
owner. That is the real demonstration that a classification can be duplicated wholesale without any
`aggCols` token appearing, and it is why the hypothetical is worth guarding against rather than
dismissing.

**No rule keyed on an identifier can see a write that does not use it**, and each new node kind
added to the walk is a guess at the next spelling — an unbounded list chased one entry at a time.

All three candidate spellings are measured absent today, each zero with a live control so it is a
measurement rather than a missing path:

```
^\s*\*(cls|sq)\s*=[^=]          -> 0   control: any deref-assign in the package -> 2
\.selectClassification\s*=[^=]  -> 0   control: selectClassification references -> 29
&…aggCols  (header take)        -> 0   control: &…aggCols[i] element takes      -> 4
```

**So the table validates itself where it is consumed.** `logicalAggregateCalls` returns
`callToAggCol []int` and the length of the slice it walked; both are carried on the
`LogicalAggregate` beside `AggregateOperands`, which is already lowering state of exactly this kind
(and is NOT `AggregateCall`, whose 39 composite-literal sites made a field there a zero-value
sentinel — Graefe's revision-1 objection stands). `upgradeAggregateOperands` then checks, before
using an index:

- `len(callToAggCol)` still equals `len(agg.Calls)` — the table is parallel to the calls, and
  nothing today reassigns `Calls` after construction (`\.Calls\s*=[^=]` non-test writes: **0**,
  against a control of 26 `.Calls` references **in `pkg/relational/core/embedded`**; the same sweep
  tree-wide gives 43, and the two numbers are different populations, not a disagreement);
- the recorded `aggCols` length still equals `len(sq.aggCols)`;
- every recorded index is in range;
- the aggregate column at each index is still **the same one the call was built from**:
  `agg.Calls[i].Func == strings.ToUpper(sq.aggCols[j].aggFunc)` (the producer's own fold at
  `logical_builder.go:78`, or the validator mismatches itself), `Distinct` equal, and
  `agg.Calls[i].Operand` **EXACTLY** equal to one of the spellings recomputed from
  `sq.aggCols[j]` — never `EqualFold`, which would reintroduce this RFC's defect inside its own
  guard.

**Why exact TEXT and not parse-node pointers.** The stronger-sounding key — `aggExpr` pointer
identity plus `selectOrdinal` — cannot be built from what exists: `logical.AggregateCall`
(`operators.go:553-570`) carries `Func`, `Operand`, `Star`, `Distinct`, `BareColumn`, `Qualified`,
`Bare`, `Qualifier` and **no parse-node pointer and no `selectOrdinal`**. Validating against those
would mean carrying an ANTLR `IExpressionContext` on `LogicalAggregate` — new parser state on a
logical node, to check a fact the operand text already encodes deterministically, since that text
is *derived from* the parse node by `aggOperandCanonicalText`. The text is a checksum on a
structural bind, not the bind itself; the defect this RFC fixes was the FOLD, not the text.

This closes the permutation for the same reason: swapping two same-function entries moves each
one's operand text, so the recomputed spelling at index `j` no longer matches the call's.

**The two sides do not share a derivation, which is what makes the checksum able to fire.**
`call.Operand` is FROZEN at production (`logical_builder.go:76`); the spellings are RECOMPUTED from
the live `sq.aggCols[j]` at consumption (`logical_predicate.go:5245-5257`). The comparison
straddles a time boundary, so it detects a change between the two moments — unlike a paired
assertion whose two sides move together, which this repo has been bitten by before and which would
have been vacuous here.

**The checksum is blind in exactly one case**, and it is a subsumption argument rather than a
hope: two entries sharing `Func`, `Distinct` AND canonical operand text. Under those three the
bind is interchangeable — the resolved Value is identical either way — so admitting it is correct
rather than tolerated. (`Distinct` is compared separately precisely because canonical text omits
it.)

**The obvious objection is that both sides run the same renderer, so this is a paired assertion
whose halves move together — and for THIS invariant that is harmless rather than vacuous.** A
change to `aggOperandCanonicalText` moves both sides and the check keeps passing, which is the
CORRECT outcome: the renderer changing does not invalidate the index correspondence. The mutation
being watched is a change to `aggCols` between production and consumption, and that moves one side
only. The general rule this repo learned the hard way — a pair sharing a derivation cannot check
itself — applies to a pair asserting a VALUE; here the pair asserts that a value did not change
across a time boundary, which is the one thing a shared derivation still detects.

**THE FALSE-POSITIVE RISK IS THE DERIVED-TABLE PATH, and it is where correct-or-loud can turn into
loud-on-correct.** `logical_predicate.go:8620` calls
`buildSelectShell(op, sq, strings.ToUpper(sq.tableName)+".")` — a NON-EMPTY `stripPrefix`, so the
producer's `strip(arg)` (`logical_builder.go:76`) removes a qualifier the consumer's two-entry
`spellings` list reconstructs by a different rule. Producer and consumer spellings can therefore
legitimately differ on that path, and an exact compare would reject a CORRECT query. This is the
one shape where the fix could ship a regression rather than a fix, so it gets its own test arm
before anything else is believed.

A mismatch is a typed error, not a silent rebind. This is **correct-or-loud**, the treatment
`logical_predicate.go:12216` already applies to the same class one function away — "We cannot
disambiguate them by name, so reject rather than return wrong rows."

**Function alone is NOT enough, both reviewers found it independently, and the codebase says so in
its own comment.** Revision 7 checked only length plus per-index `aggFunc`, and reasoned that any
mutation preserving both left the indices correct. That is false for a PERMUTATION of two
same-function entries — precisely this RFC's own defect class. Every row of the table in § Problem
is same-function (COUNT/COUNT, SUM/SUM), so a function check has **zero** discriminating power
exactly where it matters: swap `aggCols[0]` and `aggCols[1]` on the `'us'`/`'US'` pair and length,
function and range all still hold while `[0,2]` silently becomes `[2,0]`.

It is not hypothetical. `selectOrdinal`'s own doc (`select_parser.go:337-341`) states
**"Reclassification may reorder aggSelectCol storage, but it must never rewrite this identity"** —
reordering is an anticipated operation. And there is a concrete plausible edit that performs it:
sorting `aggCols` by `selectOrdinal` instead of returning an index list, which is what
`aggregateColumnsInSelectOrder` (`logical_builder.go:22`) exists to avoid doing.

**Neither field may enter memo identity**, or two structurally identical `GroupByExpression`s
differing only in parser provenance would land in different groups — splitting the memo on a parse
accident. This holds **by construction, not by discipline**: `logical.LogicalAggregate` never
reaches the memo package at all. `grep -rn 'LogicalAggregate' pkg/recordlayer/query/plan/cascades/`
returns **0**, against controls of 148 `GroupByExpression` references in that same package and 105
`LogicalAggregate` references repo-wide. It is a pre-memo lowering draft, translated into
`expressions.GroupByExpression`; `AggregateOperands` and `OutputSlots` already live there for the
same reason.

**Why this is strictly stronger than any of the four gates.** It validates the DATA rather than
forbidding the spellings that could corrupt it, so it catches every shape in the table above and
every shape nobody enumerated: reallocation moves the length, wholesale replacement moves the
length or the content, a header swap or a reorder moves the per-index identity. It holds in
production rather than only in CI, and it cannot be circumvented by a code shape that has not been
predicted.

**What it does not do**, stated because a validation is as prone to an over-broad scope sentence as
a gate: it does not detect a substitution that leaves the operand's canonical TEXT identical while
changing what it means — which requires mutating the ANTLR tree under the classification. Measured,
not asserted: `SetChildren|ReplaceChild|AddChild|SetParent|RemoveLastChild` returns **0** across
non-test sources in `pkg/relational/core/embedded`. That residual is a different CATEGORY from the
one it replaces, not merely a smaller probability — reordering is an operation this codebase
performs and documents, whereas in-place parse-tree mutation is one nothing does and which would
break every parse-derived identity in the package, not just this table.

**The table is nilled on EVERY exit from `upgradeAggregateOperands`, not only the validated one.**
It has two early returns before any validation runs — `findAggregate` yielding nil (`:5222-5223`)
and both resolvers yielding nil (`:5229-5230`) — and the second is the one that matters: it leaves
a populated table on a live aggregate that nothing validated. Nilling only "after validation" would
let the field outlive an undeclined-but-unvalidated path, which is the shape a later reader would
most reasonably trust. The field exists for exactly one consumer and stops existing when that
consumer returns, however it returns.

One nuance that narrows the exposure further: `plan_visitor.go:624` copies the classification
*after* the production at `:568`, so a whole-struct write placed after that copy cannot affect the
side table at all and is harmless by construction rather than by check.

The static gate is dropped. Four review rounds spent widening it, against a runtime check of about
a dozen lines that subsumes all four, is itself the argument: an invariant that is hard to gate
from the outside should be asserted from the inside.

**The retained text fallback is EXACT, never folded.** Revision 1 rejected exact-then-fold as the
fix and then shipped `EqualFold` in its own fallback — incoherent, and it would have left the
literal collision live on any path the side table does not cover. The fallback drops the fold.

Why not the alternatives:

- **Not "exact match first, fold as fallback" as the fix.** A narrowing, not a fix: it makes the
  collision unlikely rather than unrepresentable, and leaves a text channel between two layers as
  the authority for a structural fact.
- **Not "compare the resolved Values structurally".** What the TODO entry asks for, and it inverts
  a dependency: the Value is what this function PRODUCES, so it cannot also be the key selecting
  which call to produce it for.
- **Not "tokenize the canonical text and fold only the identifiers".** Text matching on SQL, which
  CLAUDE.md forbids, and it re-implements the lexer at a call site.

**Behaviour this narrows, deliberately.** Today one aggregate column can fan out to several calls.
The neighbouring comment cites `SELECT SUM(x) … HAVING SUM(x) > k` as the reason; measured, that
shape produces ONE call, not two (§ Investigation). The third census floor below — non-star calls
left with a nil operand — is what proves the narrowing strands nothing, and it is the floor
revision 1 was missing: two counters can see a call bound by index and a call bound by text, but
neither can see a call bound by NEITHER, which is exactly the regression the narrowing risks.

## Adjacent latent defect, pinned not fixed

Both name fallbacks render an aggregate WITHOUT `DISTINCT` — `canonicalAggName`
(`logical_predicate.go:6004`) and `aggregateValueOutputName` (`cascades_translator.go:1437`) both
emit `FN(operand)` — while `AggregateCall.CanonicalName()` (`operators.go:655-663`) emits
`FN(DISTINCT operand)`. A DISTINCT call can therefore never bind through a name fallback. Worse,
`aggregateCallOutputSlot`'s structural arm (`logical_predicate.go:5793-5806`) never compares
`call.Distinct` at all, so `SUM(x)` and `SUM(DISTINCT x)` — same Func, same operand Value — both
match and `matches[0]` wins.

**MEASURED UNREACHABLE**: every DISTINCT-aggregate shape is refused upstream with
`0AF00: DISTINCT aggregates are not supported` (`cascades_generator.go:6441`) — on
`SUM(DISTINCT …)` alone, on both orderings of a `SUM` / `SUM(DISTINCT)` pair, on
`COUNT(DISTINCT …)`, and grouped. So this is latent, not live, and it is NOT fixed here: a fix
would be unreachable code with no way to redden it.

It is pinned instead, with the direction of the alarm stated: the rejection is what makes it
unreachable, so **closing TODO §5 "E091-07 `COUNT(DISTINCT)`" arms this defect on its first day**.
The pin's failure message says so, and names both sites.

**The rejection is already asserted FOUR times, and not in a way that can carry this.**
`embedded_fdb_test.go:4884`, `:4889`, `:7755` and `:7660` all go through
`expectRejectionOrCascadesError` (`:736`) — a tolerant either/or matcher that passes on a rejection
OR on a generic Cascades error. That is right for what those tests are about (the feature is
absent, and they do not care how), and exactly wrong as a tripwire for this defect: each would keep
passing if the rejection moved, weakened, or became a different error — which is the event that
arms the collision.

`:7660` is the weakest of the four and the clearest illustration: it loops all five DISTINCT
shapes and matches on the bare substring `"DISTINCT"`, so any error whose text happens to contain
that word satisfies it. Breadth of coverage is not strength of assertion, and four tolerant
assertions do not add up to one tripwire.

The new pin asserts the specific SQLSTATE and message at the specific site
(`cascades_generator.go:6441`), so it fails when the rejection changes rather than only when the
query stops erroring.

## Duplicate mechanisms — what this does and does not collapse

Stated because the honest answer is "adds one, deletes none", and a reviewer should not have to
discover that:

| pair | status |
|---|---|
| `normalizeAggregateBindingName` (`logical_predicate.go:5991`) / `normalizeAggOutputName` (`cascades_translator.go:1289`) | byte-identical bodies |
| `canonicalAggName` (`:6004`) / `aggregateValueOutputName` (`:1437`) | near-identical renderers |
| `aggregateCallOutputSlot` (`:5788`) / `aggregateValueNativeOrdinal` (`:1401`) | one algorithm twice, DIVERGING: collect-all-then-`matches[0]` vs first-match |

This RFC adds the side table and collapses none of these. That is a deliberate scope call, and the
reason is that they are **not** further copies of this bug: RFC-237 already de-folded both
normalizers, and the structural arm of both binders consumes `agg.AggregateOperands` — so fixing
the operands fixes what those binders see, and their name fallbacks stop being reached for this
shape. Collapsing the three pairs is a separate cleanup with no wrong answer behind it; booking it
here rather than doing it keeps this change to the defect.

## Performance

No runtime cost. This runs once per query at logical-build time over `len(aggCols) × len(calls)`,
both single digits in every corpus query, and the side table replaces a string comparison per pair
with an integer one.

**Plan shape: predicted unchanged, and predicted rather than asserted.** Two aggregates that
currently share one operand Value will carry two distinct ones, which changes the
`GroupByExpression`'s specs and therefore its memo identity — so a plan change is possible *for the
shapes that collide today*. The census says the corpus contains no such shape (0 fold-only matches
over 20235), so the plan-shape golden should not move. That is a prediction the test plan
verifies by regenerating the golden, not a claim.

On the failing shape the fix costs one more accumulator — two aggregates that currently compute the
same operand twice will compute their two distinct operands, which is the correct answer.

## Test plan

1. **The wrong answer, pinned at the shape that breaks** — an FDB scenario asserting `[0, 2]` for
   the case-only pair, its reversal asserting `[2, 0]`, the `SUM`/`THEN "Amount"` variant, the
   literal-on-the-left spelling, and the grouped form. The reversal is not decoration: with the
   defect present the pair and its reversal disagree about which value both columns take, and a
   single-order test cannot express that.
2. **The controls stay green** — the `'EU'`/`'US'` pair, and the same case-only pair outside an
   aggregate, both already in `quoted_identifier_aggregate_labels.yaml`.
3. **A unit pin that drives BOTH passes**, not just the corpus reading: a table over
   (exact-only, fold-only, both, neither) asserting which call each aggregate column claims, so the
   pass-2 arm is exercised even though the corpus may not reach it. Per CLAUDE.md a census needs a
   unit pin that drives every arm.
4. **A census with a floor of zero on the text fallback**, carrying BOTH counters so it cannot go
   vacuous: the number of calls bound by recorded index (must be non-zero — otherwise the
   instrument is dead and every verdict below it is noise) and the number bound by the text
   fallback (must be zero). Measured today: 20235 and 0 over 28 targets. The failure message names
   which direction is the alarm, because the two mean opposite things — a zero on the first says
   the census died, a non-zero on the second says a call arrived with no producer index.
5. **Mutation-verified**, with the mutation shown present in the same invocation and the BUILD
   result read, not only the test result: reverting the index bind must redden the new scenario,
   and only it.
6. **The side-table validation driven on every arm by a unit test** (§ Fix) — four failure modes
   and the success path, as explicit state rather than as whatever the corpus happens to reach: a
   length that no longer matches; an index out of range; an index whose aggregate column now
   carries a different function; and — the arm the weaker check would have missed — a PERMUTATION
   of two same-function entries, which must be caught on parse-node identity. Each must produce the
   typed error and NOT a rebind. The success arm asserts a non-zero number of validated indices, so
   the check cannot pass by validating nothing.

   Plus a pin that `logical.LogicalAggregate` stays out of the memo package, since the "neither
   field enters memo identity" condition rests on it: assert the reference count in
   `pkg/recordlayer/query/plan/cascades/` is zero, with the failure message saying that a reference
   appearing there means `callToAggCol` can now split a memo group on parser provenance.

   **And the derived-table arm, which is the one that can turn this fix into a regression**
   (§ Fix): an aggregate reached through `logical_predicate.go:8620`, where `stripPrefix` is
   non-empty and producer and consumer spellings can legitimately differ. It must ANSWER, not
   error. Written and run BEFORE the exact compare is believed on any other shape, because every
   other arm in this plan tests that the check fires and only this one tests that it does not fire
   when it should not.

   Then **mutation-verify it end to end**, mutation shown present in the same invocation: append an
   aggregate column between production and consumption and confirm the wrong-answer query errors
   loudly rather than returning `[2,2]`. This is the arm that says the validation is load-bearing
   rather than decorative — four earlier drafts specified a static gate whose arms were never
   driven at all, which is precisely how a gate comes to be believed without being tested.
7. **The DISTINCT latent defect pinned at its rejection**: the probe that measured
   `0AF00: DISTINCT aggregates are not supported` across five DISTINCT shapes becomes a committed
   test, with a failure message saying that closing TODO §5 E091-07 arms the two sites named above.
   A negative result is what lets this RFC classify the defect as latent, so the measurement has to
   survive, not the conclusion alone.
8. `just test` green; plan-shape golden regenerated and shown unmoved (see Performance — this is
   the prediction, not an assumption).

## Java

Read against the pinned 4.12.11.0 tree, because revision 1 asserted this section with no citations
and an uncited claim about the reference implementation is exactly the folklore the watch-list
exists to catch.

`ExpressionVisitor.visitAggregateWindowedFunction`
(`fdb-relational-core/src/main/java/com/apple/foundationdb/relational/recordlayer/query/visitors/ExpressionVisitor.java:352-362`)
builds the aggregate's argument **in place** and hands it straight to construction:

```java
} else if (functionContext.functionArg() != null) {
    argumentMaybe = Optional.of(visitFunctionArg(functionContext.functionArg()));   // :360
}
return argumentMaybe.map(expression -> getDelegate().resolveFunction(functionName, expression))  // :362
```

`visitFunctionArg` returns an `Expression` carrying a real `Value` derived from the parse tree, and
`resolveFunction` receives that Expression. The operand is bound AT CONSTRUCTION. Java never
records a rendered operand name to be matched back to a call later, so there is no channel in which
two operands differing only by a string literal's case can be conflated — the defect is not
expressible.

**Java rejects DISTINCT aggregates too**, at `:353-354`: the visitor asserts the aggregator token
is absent or `ALL` and otherwise raises `UNSUPPORTED_QUERY`. So the latent DISTINCT defect above
sits behind a rejection in BOTH engines, which is consistent with TODO §5's E091-07 entry recording
`COUNT(DISTINCT)` as a gap rejected by both — and confirms that closing it is a shared-surface
extension, not a parity fix, which is when the two Go sites named above become reachable.

This is a Go-only defect introduced by the text channel between `logicalAggregateCalls` and
`upgradeAggregateOperands`. Retiring that channel — resolving the operand at the producer, as
`logical_predicate.go:12211` already does — is the structural end state, blocked on a resolver at
the builder, and is booked as a follow-up rather than done here.
