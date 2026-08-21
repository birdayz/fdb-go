# RFC-238: a qualifier is structure, not punctuation

**Status:** PROPOSED
**Scope:** how a source qualifier travels from the resolver to the two boundaries that genuinely consume a flat key — presentation, and the merge-shape decision in the executor — and every producer and parser between them.
**Relates to:** RFC-237 (a name is normalized once, at the parse boundary), RFC-229 (a column states its own name), RFC-232 (field values are resolved field accesses).

## 0. The two queries

They are different queries. They render to the same string.

```sql
CREATE TABLE x_probe (id BIGINT, "TOTAL" BIGINT, "X.TOTAL" BIGINT, PRIMARY KEY (id));

SELECT "X.TOTAL" FROM x_probe;                        -- label: X.TOTAL
SELECT s.id, (SELECT x.total FROM x_probe x WHERE x.id = s.id) FROM s_probe s;
                                                      -- label: TOTAL
```

The first reads one column whose declared name contains a dot. The second reads
a different column through a qualifier. Both arrive at the label boundary as the
seven characters `X.TOTAL`, and they want different answers. No rule over that
string can give both, because the information that separates them was discarded
upstream — where the qualifier stopped being *structure* and became
*punctuation*.

Java never faces this. An identifier there is a LIST of parts and
`Identifier.withoutQualifier` takes the last PART. The dot is a rendering, never
a delimiter to be re-found.

## 1. The mechanism

One decision produces every symptom below: a leg's per-column datum key is
carried as a **rendered string**, `ALIAS.COL`.

**EIGHT sites MINT A LEG DATUM KEY.** The first draft listed two, which is the
census failure this repo has a rule about — a count taken over the file that
motivated the work rather than over the population.

The population sweep, and what it actually returns:

```
grep -rn --include='*.go' -E '\+ "\." \+' pkg/relational/core/ pkg/recordlayer/query/ \
  | grep -v '_test.go' | wc -l          # 31
```

**31 raw hits, and THE SWEEP DOES NOT PRODUCE THE TABLE BELOW — it is a
starting point, not the census.** Two ways it misses, both worth stating because
the first two drafts pasted a command and let the reader assume it yielded the
list:

- it does not match `legColumns`, which builds `prefix := alias + "."` on one
  line and appends the column on another, so a renderer central to this RFC is
  absent from its own population sweep;
- most of the 31 render a dotted string for a diagnostic, a display label, or
  scope bookkeeping (`logical_predicate.go`'s `projCol.name`, which already
  travels beside an explicit `qualifier`/`bare` pair — this RFC generalizing
  that shape).

So the census is the table, arrived at by reading the hits and the misses.
Anything later found to mint a key joins it rather than being argued out of it.

| site | what it renders | after |
| --- | --- | --- |
| `legColumns` | `ToUpper(alias) + "." + ToUpper(col)` (two lines; sweep misses it) | GONE |
| `logicalLegFields` | `ToUpper(alias) + "." + col` | THE ONE |
| `scalar_subquery_seed.go:143` | `ToUpper(innerAlias) + "." + scalarCol` | GONE |
| `clustered_outer_scalar.go:509` | `leg.binding + "." + leg.typ.Fields[i].Name` | GONE |
| `clustered_outer_scalar.go:537` | `ToUpper(innerAlias) + "." + scalarCol` | GONE |
| `qualifyAndMergeColumns` (two sites) | `alias + "." + ToUpper(c.Name)` | GONE |
| `cascades_translator.go:4136` (unnest leg mint) | `leg + "." + ToUpper(rootName)` | GONE |

The `clustered_outer_scalar` and `scalar_subquery_seed` sites already disagree
among THEMSELVES on the case question — `:509` keeps the leg's own slot name
verbatim while `:143` and `:537` fold the alias.

`cascades_translator.go:4136` is not bookkeeping and the previous draft filed it
that way: it mints `LEG.COL`, resolves it through `mergedType.FieldIndexUnique`,
and BAKES the resulting ordinal into a predicate, across five merged and chained
UNNEST paths. It is a producer and a consumer in one place, so leaving it out
would have left the flat-key mechanism live after every listed renderer
migrated.

Note what the third column says: the surviving renderer is `logicalLegFields`,
the exact-type authority, NOT the presentation site. Presentation reads the
structured qualifier and renders only if a caller demands a flat key.

**SEVEN sites parse it back, with THREE different rules**, and the first draft
found four. Two are at runtime and only one of those executes; two are already
migrated by this PR:

| site | rule | live? |
| --- | --- | --- |
| `colref.go` (`parseColRef`) on the label path | LAST dot at paren depth zero | yes |
| `derivedOutputColumns` | LAST dot | yes |
| `classifyDerivedUnnestArray` (`derived_unnest.go:146`) | LAST dot | yes — migrated and REVERTED, see below |
| `projectionOutputNames` (`derived_unnest.go:250`) | LAST dot | **MIGRATED** — see below |
| `splitQualifier` (EXISTS sort keys) | LAST dot | yes, nonzero corpus floor |
| `rowSlotForLegColumn` (`ordinal_join.go:1168`) | **FIRST** dot | **no — retired, revival alarm at 0** |
| `isDottedQualifiedName` (`ordinal_join.go:1238`) | any dot, `{`/`[` prefix guard | yes, and it picks a JOIN ARM |

**`rowSlotForLegColumn` IS RETIRED, and that matters more than its being a parser.**
`rowSlotForLegColumn`'s only driver was `adaptLegPositional`'s
layout-permutation gather, which the exact-ordinal seed removed from the live
path. `AssertLegColumnProvenanceCensus` asserts `c.Calls != 0` -> FAIL
(`leg_column_provenance_census.go:609`), unconditionally and with no floors
pointer. That is the sharp fact, and sharper than "the dotted arm never
answers": the whole READER is retired, not just one of its arms. So it is
reachable in source and unreached in fact.

A draft of this RFC treated its two `EqualFold` comparators as MASKING the case
divergence between the producers. They cannot: they never execute. Migrating it
is tidying dead code, which is worth doing when the surrounding change lands and
is not evidence of anything; any criterion resting on it can pass while changing
nothing that runs.

**ONE OF THE SEVEN IS MIGRATED, AND TRYING TO MIGRATE A SECOND MEASURED WHY THE
ORDER MATTERS.** Both sit on the derived-UNNEST path. The reproducers:

```sql
CREATE TABLE dottarr (id BIGINT, "a.b" BIGINT ARRAY, PRIMARY KEY (id));

SELECT x FROM (SELECT "a.b" FROM dottarr) d, d."a.b" AS x;        -- unaliased
SELECT x FROM (SELECT "a.b" AS a FROM dottarr) d, d.a AS x;       -- aliased, VALID
```

`projectionOutputNames` sliced the derived output at its last dot and published
`b`, so the unaliased form died `42703: column "a.b" does not exist on source
"D"` — a false statement, since the derived table does output `a.b`. It reads
`ProjectionRefs` now, and the shape declines honestly instead:
`0AF00: unnest over a computed/non-passthrough … output`.

`classifyDerivedUnnestArray` was migrated the same way AND REVERTED, which is
the useful result. With the triple, classification SUCCEEDS and the query dies
further down, in the semantic derived-source registration, as

```
42703: column "A" does not exist on source "D"
```

on the ALIASED form — a valid query whose column `A` plainly does exist. Neither
form runs either way, so the only thing the migration changed was trading an
honest decline for a false statement about the schema. That is a regression, and
the site keeps its split until step 6 migrates it TOGETHER with the registration
that makes the answer available.

This is the concrete argument for §6 that is not hand-waving: migrating one site
of a chain can make the diagnostic worse while making nothing work, so the steps
are ordered by what turns a decline into an ANSWER, not by which site is easiest
to reach.


**`isDottedQualifiedName` IS LIVE and is worse than a naming defect.** It is
called from `rowIsMergeShaped` (`:1218`) and decides
CONTROL FLOW — whether a row is merge-shaped — from a bare dot test. A scan row
carrying a column declared `"a.b"` is misclassified there, which is §0 inside
the executor, choosing a join arm. This is the runtime site that matters.

**And the structure it needs already exists downstream, in two places.**
`rt.Legs` carries `(Name, Start, Width)`, and `ColumnDef` already separates
`Name` (the datum key) from `Label` (the SQL label). Both are this RFC's thesis
half-built: the containers are structured and the FIELD is not, so every reader
re-derives from punctuation what the producer knew and dropped.

Two of the eight renderers already disagree — by case, on the column half — a
divergence in its own right, pinned by
`pkg/relational/core/query/leg_column_key_case_divergence_test.go`. That is not
a bug to fix by picking a spelling. It is the predictable consequence of having
two producers of one key, and it will recur whichever spelling wins.

## 2. Every symptom is downstream of that

- **The case disagreement.** Two renderers, two spellings of one key.
- **The `a.b` label.** A parser guessing which dot was a qualifier. `SELECT "a.b"
  FROM dott` reported `b` where Java reports `a.b` — measured against a live JVM
  by `conformance/dotted_and_recursive_seed_java_probe_test.go`. Go's own STAR
  expansion already reported `a.b`, because star reads the schema and never
  renders.
- **`colref.go`'s two KNOWN LIMITs.** A parser guessing at all: a matched paren
  inside a string literal, and a literal containing a depth-zero dot.
- **`qualifierStrippedLabel`'s residual.** The interim now in the tree answers
  from two schema facts instead of one parse, which closes two measured
  collisions and leaves the pair of queries in §0. Its NOT-covered list is
  three entries long and every entry is an arm of
  `qualifier_stripped_label_test.go`.
- **`seedResolvesThroughJoin`.** Deleted in this same branch. It was a third
  workaround for render-then-reparse: a structural walk asking whether a seed
  resolved through a join, standing in for "are these names qualified".
- **A JOIN ARM CHOSEN BY PUNCTUATION.** `isDottedQualifiedName` decides whether
  a row is merge-shaped from a bare dot test. This is the only symptom in the
  list that is not a naming defect: a scan row carrying a column declared
  `"a.b"` takes the wrong branch.

`ExactLogicalOutputLabels` is the existence proof for the fix. It never renders,
so it never parses, and it needs no heuristic — its own doc states the rule:
"removed STRUCTURALLY, by not adding it, never by slicing at a dot".

## 3. The design

Carry `(sourceAlias, name)` as separate data on the field, alongside the flat
name during migration. Label derivation then reads the structured qualifier
instead of re-finding it, and the eight renderers collapse to one.

**THERE IS NO EXECUTOR ROW-MAP BOUNDARY**, and the first draft was built on one.
`PositionalRow` is the SOLE runtime row (`positional_row.go:7`): slots are
indexed by ORDINAL and every column reference reads a plan-time-baked ordinal.
The one name-keyed projection, `positionalToMap` (`:284`), is deliberately lossy
and DML-only — it drops slot order and collapses duplicate names LAST-WINS — and
its doc says a caller that is not itself name-keyed must not use it. Rendering
there would be dead code or would re-introduce that loss.

**BUT THE EXECUTOR STILL CONSUMES A FLAT NAME**, and the second draft's "the
presentation boundary is the only place a flat key reaches anything that cannot
address by ordinal" was false. `PositionalRow`'s doc is accurate about
`PositionalRow`; it was never a claim about the whole executor.

So there are TWO boundaries where a flat key is consumed, and the design names
both:

- the **presentation** boundary, where a `ColumnDef` gets its `Name` and
  `Label` — genuinely the last place a flat key must exist, because a caller
  reading `Rows.Columns()` cannot address by ordinal. Note `ColumnDef` already
  separates the two fields, so half the structure is there;
- the **merge-shape decision** in `ordinal_join.go`, which does NOT need a flat
  key at all. `rowIsMergeShaped` scans field names for a dot; `rt.Legs` already
  carries `(Name, Start, Width)`, so a row that stated its own leg structure
  would be answered by lookup rather than by punctuation.

The second is not a rendering site to keep — it is a parser to delete, and it is
the one runtime consumer that EXECUTES.

**A THIRD DRAFT AIMED AT THE WRONG RUNTIME SITE, and the correction is worth
keeping because it is the same failure the RFC is about.** That draft named
`rowSlotForLegColumn` as the runtime boundary and its `EqualFold` comparators as
MASKING the producer case divergence — the argument being that removing the
`legColumns` fold moved zero golden rows because the reader could not tell the
producers apart.

Both halves are refuted, each by something already in the tree:

- the reader is RETIRED. `AssertLegColumnProvenanceCensus` fails the build on
  ANY call to it, because the exact-ordinal seed removed its only driver. Dead
  comparators mask nothing;
- the golden is generated through `embedded.PlanPhysicalForTest`, which never
  runs the executor. A planning measurement was being explained by a runtime
  mechanism.

What the zero actually says is narrower: no PLANNED shape in the corpus depends
on a leg key's case. That still argues for collapsing the producers — a latent
disagreement between two writers of one key is worth removing — but it is not
evidence of a live defect, and this RFC no longer claims one.

### Rejected alternatives

**Keep parsing, fix the parser.** This is what the branch already tried twice
and what §0 refutes: the two queries are indistinguishable at that boundary, so
no parser is correct.

**Use the reference's accessor leaf.** Tried and measured dead. That leaf is the
row's field name and the row's field name is sometimes itself a qualified datum
key — `X.SUM(X.Amount)` is its own leaf, so "keep the name when it IS the leaf"
kept the qualifier.

**Use the reference's root correlation.** Tried and measured dead. By the time a
projection's columns are derived, every read is re-anchored to the frontier
quantifier: `X.SUM(X.PLAIN)` reports `alias=_current`, not `X`. This is the
right *answer* and the reason the RFC exists — the provenance has to be carried
to the boundary, not looked up there.

**Pick a spelling for the leg key and leave two producers.** Does not address
§1. The folding line does two jobs — descriptor-to-SQL normalization for a
hand-authored proto field like `order_id`, re-normalization for a DDL-declared
`"KeepCase"` — so removing it breaks a real test and keeping it breaks the
RFC-237 invariant.

## 4. Order

Each step is measurable on its own and the plan golden is regenerated after
each, so a step that moves rows it should not is caught at that step rather
than at the end.

1. Add the structured qualifier alongside the rendered name. No behaviour
   change; golden byte-identical.
2. Move label derivation off the split, onto the structured qualifier.
3. Collapse ALL EIGHT renderers, not the first two — including `qualifyAndMergeColumns` (two sites) and the UNNEST leg mint at `cascades_translator.go:4136`, which the table marks GONE and an earlier step list silently left standing: `legColumns`' join arm defers
   to `logicalLegFields`, and `scalar_subquery_seed.go:143`,
   `clustered_outer_scalar.go:509` and `:537` defer to the same boundary, where
   the descriptor-name decision also moves. Collapsing a subset leaves live
   paths spelling keys independently — and those three already disagree with
   each other on case.
4. Migrate `derivedOutputColumns`' own recovery at
   `cascades_translator.go:924` (`strings.LastIndexByte`) onto the structured
   qualifier. It is a SECOND parser and the first draft's step list left it
   standing while deleting the first one's limits, which would have hidden the
   same ambiguity one site over.
5. **Migrate the runtime, LIVE SITE FIRST.** `rowIsMergeShaped` asks the row
   whether it is merge-shaped — from `rt.Legs` and each field's own leg
   identity — instead of scanning names for a dot through
   `isDottedQualifiedName`. That is the one runtime consumer that executes and
   the only one deciding control flow.

   `rowSlotForLegColumn` migrates in the same step for tidiness, not for
   evidence: it is retired behind an unconditional revival alarm, so nothing
   observable changes when its first-dot split and its `EqualFold` comparators
   go. A draft of this RFC had it the other way round.

   Also migrate the two remaining LIVE plan-time splitters the first draft
   missed entirely: `classifyDerivedUnnestArray` (`derived_unnest.go:146`) and
   `splitQualifier` for EXISTS sort keys, both with nonzero full-corpus floors.
   Leaving them means a quoted one-segment name containing a dot is still read
   as qualified on the derived-UNNEST and EXISTS-ordering paths after every
   other criterion passes.
6. **Finish the derived-UNNEST chain.** Two of its three sites are already
   migrated (see §1); the third is the semantic derived-source registration for
   the unnest path, which still publishes a stripped name and fails

   ```sql
   SELECT x FROM (SELECT "a.b" FROM dottarr) d, d."a.b" AS x
     -> 42703: column "a.b" does not exist on source "D"
   ```

   This is a STEP and not a note because the shape is a live defect and the
   remaining site is already identified: `SELECT d."a.b" FROM (…) d` resolves,
   so the ordinary projection binder is correct and only the unnest-source
   registration is not. The step lands the e2e arm the unit pin currently
   stands in for.
7. Delete `parseColRef` from the label path and delete its limits.

## 5. Acceptance criteria

These make the collapse checkable rather than asserted. Each one is a command
with an expected output, because a criterion naming an artifact by a description
gets satisfied by assertion — two of the first draft's did, and they are
rewritten here.

**(1) The label path stops parsing.** `parseColRef` has **26** non-test CALLERS
under `pkg/relational/core` today:

```
grep -rn --include='*.go' 'parseColRef(' pkg/relational/core/ \
  | grep -v '_test.go' | grep -v 'func parseColRef' | wc -l
```

The `grep -v 'func …'` filter is load-bearing and the first draft omitted it: the
raw count is 27 and includes the DECLARATION, which is the "grep -c counts lines,
not what you meant" failure inside the criterion written to be checkable.
Positive control on the same sweep, filtered the same way: `declaresColumn`
prints **1**. That control is itself line-counting — `declaresColumn` is invoked
TWICE on one line (`declaresColumn(descs, name) && !declaresColumn(descs,
ref.col)`), so the control reports 1 line for 2 calls, which is the exact defect
it was added to guard against, one level up. It serves only to show the search
is well-formed, and any count used as a GATE must count call expressions —
`grep -o 'parseColRef(' | wc -l`, or an AST pass — not lines. "The label path"
was also undefined, which let any survivor be declared off-path.

Callers are named by ENCLOSING FUNCTION, not by line: a file:line table is stale
by the next commit that touches the file, and this criterion is read at
implementation time. These five must be GONE:

| function | what it does |
| --- | --- |
| `qualifierStrippedLabel` | the interim itself; the whole file goes |
| `foldedColumnDef` | `label := parseColRef(f.Name).bare()` |
| `ordinalUnnestColumnDef` | `label := parseColRef(f.Name).bare()` |
| `columnDefDisplayName` | returns `parseColRef(c.Name).bare()` |
| `buildAggColumns` | `bare := parseColRef(name).bare()` |

Three more go in STEP 2, and the first draft mis-filed all three as "just
lookup":

| function | why it is not just lookup |
| --- | --- |
| `validateTablesAndColumnsInner` (**TWO** calls) | splits a name that HAS a `ProjectionRefs` counterparty; its own comment says a disagreement RAISES `ErrCodeUndefinedColumn` on a column the parser saw perfectly well |
| `descriptorForColumn` | splits to pick a DESCRIPTOR |
| `protoFieldTypeName` | splits to pick a FIELD, which is §0's TYPE half — see criterion (7) |

BOTH of `validateTablesAndColumnsInner`'s calls go, and that is not pedantry:
for a one-segment `"T.COL"`, migrating only the first leaves the second looking
up `COL` and raising 42703 anyway. A draft counted it as one site.

**26 callers − 5 − 4 = 17 after the change**, and that is the number to grep.
Earlier drafts said 21, 22, 27 and 18; the count that matters is the one AFTER
the retirements the RFC itself specifies, and stating it five ways meant a
reader at implementation could see the criterion as failed while it succeeded.

The rest are genuine lookup and comparison — a bare-name proto lookup
(`deriveProjectionColumnDef`, `columnDefFromRef`), a display-name comparison
(`legRead.column`, `mergedRVSequenceDiverges`), a qualification test
(`qualifyAndMergeColumns`), plus the callers in `colref.go`, `eval_map.go`,
`logical_predicate.go` and `select_helpers.go`. They are a separate question;
moving them here would make this criterion unmeetable.

**(2) The two limits are deleted, not re-documented — and only after BOTH
parsers are gone.** They are prose, not markers, and the first two drafts of
this criterion both named something ungreppable: `grep -rn "KNOWN LIMIT"
colref.go` exits 2 from the repo root (wrong path — the file is under
`pkg/relational/core/embedded`), and with the path fixed it returns zero BEFORE
the change as well, so zero could never have demonstrated anything.

Gate a marker that is non-zero today:

```
grep -c 'WHAT IT COSTS IS TWO SHAPES, NOT ONE' \
  pkg/relational/core/embedded/colref.go
```

**1 today; must be 0.** `grep -c` exits 1 on no-match, so check the STATUS as
well as the count — and confirm the path resolves first (`test -e` on it), since
a mistyped path exits 2 and prints the same empty result.

That block, and the two rows in `colref_split_test.go` that pin the limits, are
deleted.

The ordering is load-bearing and it has a command, which the first two drafts of
this criterion did not:

```
grep -c 'strings.LastIndexByte' pkg/relational/core/query/cascades_translator.go
```

**1 today; must be 0 at step 4, before step 7 deletes `colref.go:95`.** Control,
because a zero from a mistyped path reads identically to success: that symbol
has **8** non-test hits repo-wide under `pkg/relational/core`
(`grep -rn --include='*.go' 'strings.LastIndexByte' pkg/relational/core/ | grep -v '_test.go' | wc -l`),
so a sweep returning 0 there is a broken command, not a finished migration.

`cascades_translator.go:924` recovers a qualifier by the LAST dot, with the same
ambiguity and none of `parseColRef`'s paren protection — deleting `colref.go`'s
documented limits while that site lives moves the ambiguity somewhere
undocumented rather than removing it. Step 4 is what makes step 7 honest.

`parseColRef` keeps 17 lookup/comparison callers after this (criterion 1), and
those are NOT covered by the deletion: what is deleted is the file's claim to be
a safe channel for LABELS, and the two limits are limits of that claim.

**(3) The interim is removed BY the fix, not relaxed.**

```
test ! -e pkg/relational/core/embedded/qualifier_stripped_label.go \
  && test ! -e pkg/relational/core/embedded/qualifier_stripped_label_test.go
```

Both must be gone. The first draft used `grep -c 'name:  "LIMIT:' <file>` and
expected 0, which a DELETED file cannot produce — grep exits 2 on a missing path
and prints nothing, so the criterion was unsatisfiable by the very outcome it
was written to require. (For the record, that count is 2 today; it is the
relaxation guard while the file still exists, not the completion gate.)

And the SQL-visible half moves with it. The divergent query lives in
`conformance/dotted_and_recursive_seed_java_probe_test.go`'s
`dotted_column_whose_tail_is_its_sibling` arm, which measures BOTH engines:
Java `[X.TOTAL]`, Go `[TOTAL]`. This RFC landing makes that arm red, and
flipping `wantGo` to `[X.TOTAL]` in the same commit is the demonstration.

Its sibling — the correlated read, correct today, pinned in
`quoted_identifier_labels.yaml` at `TOTAL` — must stay GREEN. A fix that pays
for one by breaking the other has not separated the two queries; it has moved
the wrong answer. That is why the divergent half is in the probe and the correct
half is in the corpus: the corpus is Java-authoritative, and pinning Go's wrong
label there would have credited it as supported in the generated ledgers.

**(4) One rendering site, shown in the same commit.** At step 3,
`ALIAS + "." + COL` must be rendered in exactly ONE place, shown by grep in that
commit's message. `leg_column_key_case_divergence_test.go` is deleted **in that
same commit** and not before: it watches two producers, and deleting it earlier
would satisfy the criterion while the second producer still exists.

**(5) Step 3 lands a replacement pin, or it has silently re-decided the case
question.** Deleting (4)'s test drops the only thing watching the
descriptor-name axis. The new single boundary needs a pin asserting BOTH jobs
the old fold did: a hand-authored proto field `order_id` and a DDL-declared
`"KeepCase"` each come out right. Without it the collapse re-decides case with
nothing watching.

**(6) The LIVE runtime splitter stops picking a join arm from punctuation.**

`rowIsMergeShaped` must no longer call `isDottedQualifiedName`; a row states
whether it is merge-shaped, from `rt.Legs` and the field's own leg identity.
This is the runtime half that MATTERS, because it decides control flow:

```
grep -c 'isDottedQualifiedName' pkg/recordlayer/query/executor/ordinal_join.go
```

**3 today** — the call at `:1218`, the doc-comment mention at `:1225`, and the
declaration at `:1238` — **and must be 0.** The count is 3 and not 2 because
`grep -c` counts every line the identifier appears on, its own comment included;
stating 2 here would have been the line-counting mistake one more time, in the
paragraph correcting it.

**A DRAFT OF THIS CRITERION AIMED AT DEAD CODE.** It required
`rowSlotForLegColumn`'s first-dot split and its two `EqualFold` comparators to
go, and argued that those comparators were MASKING the producer case divergence
— that the zero golden movement was tolerance rather than absence. Both halves
are false. That reader is retired: `AssertLegColumnProvenanceCensus` fails the
build if it receives any call. And the golden is generated by
`embedded.PlanPhysicalForTest`, which never runs the executor, so no runtime
comparator could have been masking a planning measurement in the first place.

Migrating `rowSlotForLegColumn` alongside the rest is still right — a retired
parser is still a parser, and a revival would revive the ambiguity with it — but
it is tidying, and it is NOT evidence. A criterion resting on it can pass while
changing nothing that executes, which is why it is not the gate here.

**(7) The collision's TYPE half is fixed too, and pinned.** §0's pair diverges on
more than the label: `descriptorForColumn`/`protoFieldTypeName` split the name to
pick a field, so a descriptor declaring `TOTAL INTEGER` and `"X.TOTAL" BIGINT`
reports the dotted column's metadata as INTEGER. Nothing downstream catches it —
the flowed Long type deliberately conflates the two widths — so a fix that
corrects the label and leaves this is half a fix that LOOKS complete.

The pin asserts declared TYPE and NULLABILITY, not only the name, on both halves
of the pair. Without it, criteria (1) through (6) can all pass with the type
still wrong.

## 6. Why it is one change and not four items

**The load-bearing reason is §1:** the seven steps are the two ends of one
mechanism. A qualifier carried as a rendered string has to stop being rendered
AND stop being parsed, and neither half is a fix on its own.

*Secondary, and only about one particular bad split:* step 1 is deliberately
reader-less and golden-identical, so landing it alone leaves structured
provenance that nothing reads — indistinguishable from dead code, and the next
sweep deletes it. That argues against splitting after step 1 specifically. It is
not the argument for one change; §1 is.

This touches datum keys engine-wide.
