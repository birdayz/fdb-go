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
| `cascades_translator.go:4076` (unnest leg mint) | `leg + "." + ToUpper(rootName)` | GONE |

The `clustered_outer_scalar` and `scalar_subquery_seed` sites already disagree
among THEMSELVES on the case question — `:509` keeps the leg's own slot name
verbatim while `:143` and `:537` fold the alias.

`cascades_translator.go:4076` is not bookkeeping and the previous draft filed it
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
| `classifyDerivedUnnestArray` (`derived_unnest.go:107`) | LAST dot | **MIGRATED** — see below |
| `projectionOutputNames` (`derived_unnest.go:287`) | LAST dot | **MIGRATED** — see below |
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

**TWO OF THE SEVEN ARE MIGRATED, AND ONE OF THEM WAS HIDING A SILENT
WRONG-COLUMN BIND.** Both sit on the derived-UNNEST path. The reproducers:

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

`classifyDerivedUnnestArray` is migrated too, and the reason is sharper than
naming. Its split BOUND THE WRONG COLUMN, silently and schema-dependently:

```sql
CREATE TABLE dottarr (id BIGINT, "a.b" BIGINT ARRAY, b BIGINT, PRIMARY KEY (id));
SELECT x FROM (SELECT "a.b" AS z FROM dottarr AS a) d, d.z AS x;
```

The source renders `a.b`; the split manufactures qualifier `a`; that MATCHES the
body scan's alias; and `b` — an unrelated sibling — becomes the array source.
Measured both ways:

| `b` declared | before | after |
| --- | --- | --- |
| `BIGINT` (scalar) | `42F10 … only on a column of repeated (array) type` — about the WRONG column | `42703 column "Z" …` |
| `BIGINT ARRAY` | **PLANS**, over the wrong column | `42703 column "Z" …` |

A query whose behaviour flips between "type error" and "plans" based on an
unrelated column's declared type is a misbind, not a diagnostic problem. After,
the two agree — the schema-dependence is gone.

**AN INTERMEDIATE REVISION REVERTED THIS SITE AND WAS WRONG TO.** The argument
was that migrating it turned an honest `0AF00 unsupported` into a false `42703`
one layer down, and that a good message beats a bad one. True as far as it went,
and it weighed two messages against each other while missing that the split's
real cost is silent misbinding. Recorded because the reasoning is the trap: when
neither branch works, it is tempting to grade the ERROR and forget to grade the
BINDING.

**WHAT REMAINS, AND EXACTLY WHERE THE CHASE STOPPED.** Both shapes now fail
`42703: column "Z" does not exist on source "D"`, which is false — `Z` is the
derived table's alias for the column. This is step 6's site, and the search is
already narrowed:

- `SELECT d.z FROM (SELECT "a.b" AS z FROM dottarr) d` RESOLVES, so the ordinary
  projection binder is correct;
- `SELECT x FROM (SELECT b AS z FROM dottarr) d, d.z AS x` reaches the unnest
  path and reports `42F10` on the right column, so `D`/`Z` resolve there for an
  ordinary column;
- and the derived-source registration itself is NOT the culprit: instrumented,
  `buildDerivedTableSourceFromTerm` finds `a.b` in the body table and mints the
  virtual column under the alias `Z`.

So the remaining site is the lateral-unnest source resolver specifically, not
the scope registration that feeds it — a distinction worth having before anyone
starts, because the registration is where a reader would look first and it is
already right.



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
3. Collapse ALL EIGHT renderers, not the first two — including `qualifyAndMergeColumns` (two sites) and the UNNEST leg mint at `cascades_translator.go:4076`, which the table marks GONE and an earlier step list silently left standing: `legColumns`' join arm defers
   to `logicalLegFields`, and `scalar_subquery_seed.go:143`,
   `clustered_outer_scalar.go:509` and `:537` defer to the same boundary, where
   the descriptor-name decision also moves. Collapsing a subset leaves live
   paths spelling keys independently — and those three already disagree with
   each other on case.
4. Migrate `derivedOutputColumns`' own recovery at
   `cascades_translator.go:932` (`strings.LastIndexByte`) onto the structured
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

   Also migrate `splitQualifier` for EXISTS sort keys, a LIVE plan-time
   splitter with a nonzero full-corpus floor that the first draft missed
   entirely. Leaving it means a quoted one-segment name containing a dot is
   still read as qualified on the EXISTS-ordering path after every other
   criterion passes.

   (`classifyDerivedUnnestArray` was on this list and is already migrated —
   see §1. It moved early because its split was binding the WRONG COLUMN, which
   does not wait for a step.)
6. **Finish the derived-UNNEST chain.** Two of its three sites are migrated
   already (see §1); the third is the LATERAL-UNNEST SOURCE RESOLVER, which
   still reports a column the derived table plainly publishes:

   ```sql
   SELECT x FROM (SELECT "a.b" AS z FROM dottarr) d, d.z AS x
     -> 42703: column "Z" does not exist on source "D"
   ```

   §1 records where the chase stopped and, more usefully, three things it ruled
   OUT: the projection binder resolves `d.z` fine, the unnest path resolves
   `D`/`Z` fine for an ordinary column, and the derived-source registration was
   instrumented and does mint the virtual column under `Z`. So it is the
   resolver, not the scope that feeds it — which is where a reader would look
   first.

   This is a STEP and not a note because the shape is a live defect. It lands
   the e2e arm the unit pin currently stands in for, and it is the step that
   turns `classifyDerivedUnnestArray`'s decline into an answer.
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

`cascades_translator.go:932` recovers a qualifier by the LAST dot, with the same
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

**(8) The collision PANIC is gone AND the failure is table-local.** §7b measured
both halves; this is where they become checkable, because a criterion living in
a narrative section is prose and prose cannot fail.

```
grep -c 'panic("NewRecordType: duplicate field name' \
  pkg/recordlayer/query/plan/cascades/values/type.go
```

**1 today; must be 0.** Construction returns an error instead. The regression
must call `values.NewRecordType` DIRECTLY with two fields whose decoded names
collide — not through the driver, whose `recover` is exactly what turns the
panic into `XX000` and hides it.

And the SCOPE, which the panic count alone does not cover — and which is a
SECOND mechanism rather than a consequence of the first, so removing the panic
does not remove it:

```
grep -rl --include='*.go' EvaluateRecordTypes pkg/ | grep -v _test.go
```

**EMPTY today except the definition file; must name the candidate-building
site.** `buildMatchCandidates` builds a primary candidate for every record type
in the metadata with a usable primary key, and a positional type for each of
those that also has a descriptor. Java's `MetaDataPlanContext.forRootReference`
narrows to the types the QUERY names first (`MetaDataPlanContext.java:176-218`),
which is where its table-locality comes from — it has no drop-list and no
per-type catch.

BOTH LOOPS, NOT JUST THE PRIMARY ONE. `buildMatchCandidates` continues into
`c.md.GetAllIndexes()` (`cascades_generator.go:2858`) — every index in the
SCHEMA — and each resulting `metadataIndexDef` derives its row type through the
same `PositionalTypeForRecordLayout` (`:3433`, `:3462`). So a colliding table
that owns ANY secondary index reproduces the panic for a query that never names
it, and the two-table schema below would not catch it, because neither table has
an index. Java narrows this list too, from the same set:
`for (final var recordType : queriedRecordTypes) { indexList.addAll(readableOf(...recordType.getAllIndexes())); }`.
Add an indexed arm to the probe alongside the fix.

THE NARROWING NEEDS `RecordTypesProperty`, AND GO HAS MOST OF IT.
`properties.EvaluateRecordTypes` (`record_types_property.go:29`) is a port of
`RecordTypesProperty.evaluate` with tests and NO production caller — its only
two files are its own definition and its own test. Two gaps have to close before
it can be wired, and the second is a correctness gap rather than an ergonomic
one:

  - IT TAKES A `RelationalExpression` WHERE JAVA TAKES A `Reference`. The union
    over `ref.Members()` that a `Reference` entry point needs is already written
    inline for child references (`record_types_property.go:88`); it wants
    lifting to a top-level function.
  - IT DOES NOT INTERSECT AT A TYPE FILTER. Java returns
    `Sets.filter(childResults.get(0), typeFilter.getRecordTypes()::contains)`
    (`RecordTypesProperty.java:118-119`) — the filter's set INTERSECTED with the
    child's. Go returns `GetRecordTypes()` and never looks at the child
    (`record_types_property.go:37-51`), under a comment rationalising it as
    "already the intersection result from planning" — which is not true at
    plan-context construction, before the planner has run. The result is a
    SUPERSET, so a
    `TypeFilter([INNOCENT, COLL])` over a scan that can only produce `INNOCENT`
    still reports `COLL`, the candidate is still built, and the panic this
    criterion removes comes back. An earlier draft of this section called the
    helper "a faithful port"; it is not, and wiring it as-is would have shipped
    a narrowing that does not narrow.

Completing and testing the property is therefore part of this criterion, not a
prerequisite someone else already met. §7b says why a drop-list and a "fails for
want of a candidate" argument are both wrong.

AND THE NARROWING GOES WHERE THE CANDIDATES ARE BUILT — it does not have to move
earlier in the pipeline, which was the first objection to it. Java evaluates the
property over the ROOT REFERENCE, and every production `newCascadesPlanner` site
already holds one before it constructs the context:
`cascades_generator.go:488` and `:1161`, and `scalar_subquery_planning.go:70`,
each build `ref`/`subRef` first and pass it to `PlanWithContext` on the next
line. That is Java's `planPartial` shape (`CascadesPlanner.java:378-388`). So
`buildCascadesPlanContext` (`cascades_generator.go:2746`) takes the reference and
narrows there; the `sync.Once` defers only the BUILDING, not the reference.

An empty result then yields an empty candidate set, which is what Java does too
(`MetaDataPlanContext.java:178-180`) and which is harmless here because
`rule_primary_scan.go:46` yields a scan with no candidate at all.

The behaviour that narrowing must produce:

```
CREATE TABLE coll (id BIGINT, "___" BIGINT, "___0" BIGINT, PRIMARY KEY (id))
CREATE TABLE innocent (id BIGINT, v BIGINT, PRIMARY KEY (id))
```

`SELECT id FROM innocent` must ANSWER, matching Java, and `SELECT id FROM coll`
must still FAIL, also matching Java — at `cascades_translator.go:2903`, which
builds the scan leaf's row type from the one table the query names and is
already table-local. `DecodedNameCollisionJavaProbe` asserts both directions on
both engines. The criterion is met when its Go INNOCENT arm flips to
`NotTo(HaveOccurred())` — the arm's own message says to flip and keep it, never
delete it, and says which neighbouring assertions go with the flip — and its Go
COLL arm still fails, with a SQLSTATE it pins rather than a bare `HaveOccurred`.
Pinning the code is what makes the panic half checkable from the probe too: an
`XX000` recovered panic and a real diagnostic are both "an error".

That second half is why "no panic" is not sufficient on its own: an
implementation that simply ignored the duplicate decoded fields would remove the
panic and make BOTH tables queryable, reading one column under the other's name.

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

## 7. One follow-on that is scope, not oversight

`protoFieldLookup` DECLINES when two storage names decode to one SQL identifier,
which is right — a bind decided by descriptor order is worse than no bind. But a
decline surfaces as `42703` (undefined column), and Java counts the matching
attributes and reports `42702` (AMBIGUOUS_COLUMN). The reproducer:

```
descriptor holds repeated fields  foo___0bar  and  foo__0_bar
                                  (both decode to the SQL name foo___bar)
FROM t AS x, x."foo___bar" AS y   -> Go 42703, Java 42702
```

**Why it is not folded into the fix that found it.** The decline returns a nil
descriptor, and `fieldPresent == false` is what the callers turn into 42703.
Carrying "ambiguous" instead of "absent" means a new disposition threaded
through, by FUNCTION rather than by call site — the two `protoFieldLookup`
calls in `chained_unnest.go` sit in one function, and an earlier draft said
"seven places" over a list naming six:

| function | why it is on the path |
| --- | --- |
| `arrayFieldFromDescriptor` | turns a nil lookup into `fieldPresent == false` |
| `unnestArrayElementType` | forwards that to the plain-unnest caller |
| `inlineValuesArrayElementType` | the parallel signature for inline VALUES |
| `descendToArrayField` | the chained path's own descent, both lookups |
| `classifyDerivedUnnestArray` | maps the result onto a `derivedUnnestDisposition` |
| `classifyChainedUnnestArray` | the chained TWIN of that mapping, and the one a "call sites" count hides |
| the 42703 raise site | where the disposition becomes a SQLSTATE |

Seven, plus a new `derivedUnnestDisposition` arm, each wanting its own pin.

That is a coherent piece of work rather than a line, and it belongs to this
RFC's family: it is the same "a flat name lost information upstream" mechanism,
surfacing as an error class instead of as a wrong bind. It lands with step 6,
where the derived-UNNEST path is already being reworked.

**What is NOT deferred:** the wrong-column bind itself. A descriptor-order-
dependent answer was the defect; it declines now, and the decline is pinned.
What remains is which SQLSTATE the decline reports.

A DRAFT OF THIS PARAGRAPH ADDED "on a shape DDL cannot produce", citing
`protoname` as the authority. That was false and `protoname` now says so in
capitals — §7b carries the DDL counterexample. The reachability claim is
withdrawn rather than narrowed, because the specific `foo___0bar`/`foo__0_bar`
pair being import-only would still be beside the point: the shape a reader
needs to know about is the DDL one, and it is one section down.

### 7b. A second follow-on, found while documenting the first

The claim that DDL cannot produce a decoded-name collision was FALSE, and the
correction is worth more than the claim was:

```
CREATE TABLE coll (id BIGINT, "___" BIGINT, "___0" BIGINT, PRIMARY KEY (id))
SELECT id FROM coll
```

Both names begin `__` and pass through `ToProtoBufCompliantName` unchanged;
`___0` then decodes to `___`, because the decode scan finds `__0` at index 1.
Two legal, non-duplicate SQL columns; one decoded spelling; no buildable row
type. A read of an UNRELATED column fails.

**JAVA FAILS TOO, BUT ITS BLAST RADIUS IS THE TABLE AND GO'S IS THE SCHEMA.**
That difference is the defect, and it took two wrong readings to reach:

```
SELECT id FROM innocent    Java ANSWERS      Go XX000
SELECT id FROM coll        Java fails        Go XX000
```

A draft called this upstream-faithful on the strength of both engines failing
everywhere — which came from a probe whose SETUP inserted into `coll`, so Java
was failing on the INSERT and every later query inherited it. With no setup the
engines separate. Reproducing Java means failing on `coll` and ANSWERING on
everything else; the escaping itself cannot change, because escaped names are
wire. Pinned in the cross-engine probe, whose unrelated-table arm is the one
carrying this.

**What is Go's alone is the PANIC.** `values.NewRecordType` panics on a
duplicate field name; the driver recovers it into `XX000 internal error`. A
library panicking where it can return an error is design principle 4, and the
recovery is what makes the failure survivable rather than correct — a caller
that is not the driver gets a panic.

**AND THE SCHEMA-WIDE SCOPE IS NOT A CONSEQUENCE OF THE PANIC — IT IS A SECOND,
INDEPENDENT MECHANISM.** Removing the panic alone leaves it in place, which is
why (8) has two halves. The stack, captured by mutating the probe's Go COLL arm
and reading what actually reddened:

```
values.NewRecordType                      type.go:768   panics
executor.PositionalTypeForRecordLayout    query_result.go:277
embedded.buildMatchCandidates             cascades_generator.go:2827
embedded.GetMatchCandidates               cascades_generator.go:2776
cascades.MatchLeafRule.OnMatch            rule_match_leaf.go:59
```

`buildMatchCandidates` walks every record type in the metadata. Two guards skip
a type outright — no primary key (`cascades_generator.go:2805`) and no key
components (`:2809`) — and every type that survives both gets a positional type
built from its descriptor. A third guard (`:2823`) does NOT skip: a type with no
descriptor still gets a candidate, flowing `UnknownType`, so it is the only one
that reaches the end without a positional type. One unbuildable table therefore
aborts the candidate set for all of them. The blast radius is schema-wide
because the CONSTRUCTION is schema-wide, not because the failure mode is a
panic.

Note where COLL itself fails, because it is NOT here: the scan leaf's row type
is built at `cascades_translator.go:2888`, from `t.tableColumns(s.Table)` — the
one table the query names. That path is already table-local and already the
right place for the colliding table to fail. Only the candidate loop is
schema-wide.

**JAVA IS TABLE-LOCAL BECAUSE IT IS QUERY-SCOPED, NOT BECAUSE IT TOLERATES A BAD
TABLE.** `MetaDataPlanContext.forRootReference` evaluates `RecordTypesProperty`
over the root reference and builds primary candidates only for the types the
QUERY names (`MetaDataPlanContext.java:176-218`). It has no drop-list and no
per-type catch: an unbuildable candidate would abort its whole set too. It never
touches `coll` when the query names `innocent` because it never looks at `coll`.

So the fix is to port the NARROWING, and the two shapes that suggest themselves
first are both wrong:

  - A DROP-LIST — build every candidate, skip the ones that fail — is a Go-only
    mechanism reaching a Java-shaped outcome by a different route. It also
    leaves Go building every table's positional type on every query, which is
    the cost Java's narrowing avoids and which nothing else in the port pays.
  - "AND THEN THE QUERY FAILS FOR WANT OF A CANDIDATE" is false in Go, and it is
    a Java invariant imported without its premise. `PrimaryScanRule.OnMatch`
    (`rule_primary_scan.go:44-66`) yields a `RecordQueryScanPlan` from a
    `FullUnorderedScanExpression` with no candidate at all; Java has no such
    rule, so there a missing primary candidate really does mean the query cannot
    be planned. In Go, no-candidate plans a scan.

This is separate from §7: that one is a SQLSTATE refinement on a lookup
decline, this one is a panicking row-type construction whose failure is scoped
to the wrong thing. They share only the non-injectivity that produced both,
which is now documented at `protoname` where the next caller will see it.

Both halves are criterion (8) in §5, with the commands that make them
checkable. They are stated there rather than here because a requirement living
only in a narrative section is prose, and prose cannot fail.

### 7c. The plan tree is in the WRONG NAMESPACE, and Java says which one is right

Chasing §7b's blast radius turned up a family, not an incident. Every member has
the same shape — one side holds the SQL identifier, the other holds the stored
protobuf identifier, and something compares them as strings.

Measured, on this branch:

```
FDBRecordStore.ScanRecordsByType    slow path filtered on the caller's string    0 rows, nil error
executeTypeFilter                   allowed set keyed on the plan's string       latent
OnlineIndexer                       shouldIndexRecordForIndex vs                 built, readable,
                                    indexedRecordTypes disagreed                 EMPTY index
CREATE INDEX ... ON "MY$TABLE"      md.RecordTypes()[sqlName]                    42F59 unknown table
primary/index match candidates      registered under rt.Name, query scan          NO access path
                                    carries s.Table                              for an escaped table
```

The first four are fixed here, each with a regression that reddens without the
fix. The fifth is the one this section is about, because it cannot be fixed the
same way.

**IT IS NOT FIXABLE AT THE COMPARISON.** `FullUnorderedScanExpression.Equals-
WithoutChildren` compares its record-type list element by element and has no
metadata to resolve with — nor should it: it is structural equality on a memo
expression. So the two sides have to AGREE BY CONSTRUCTION. Today they cannot:
`buildMatchCandidates` passes `[]string{rt.Name}` (stored,
`cascades_generator.go:2842`) and `cascades_translator.go:2904` passes
`[]string{s.Table}` (SQL).

The visible cost, same query shape over one schema, one per table:

```
u            ->  TypeFilter([U], Scan(U, [<>]))            PK range
"MY$TABLE"   ->  PredicatesFilter(Scan(MY$TABLE), ...)     full scan, residual
u            ->  IndexScan(IDX_TAG, [=] COVERING)          index used
"MY$TABLE"   ->  PredicatesFilter(Scan(MY$TABLE), ...)     index built, never chosen
```

Both pairs are pinned in `yamsql/testdata/intermingle_escaped_table_name.yaml`
and `escaped_table_secondary_index.yaml`, asserted at the WRONG value on purpose
so the fix reddens them. A THIRD sentinel carries the same wrong-on-purpose
value and is easy to miss because it is generated rather than written:
`explaindiff/testdata/plan_shape.golden` records every corpus query's plan, so
THREE of those two files' five queries appear there with the wrong-on-purpose
value -- the two `#1` controls are unescaped tables and must NOT move. The three
are under the stanza headers
`=== escaped_table_secondary_index.yaml#0` and
`=== intermingle_escaped_table_name.yaml#0` and `#2`. Named by STANZA and not
by line, because the golden is generated and every new corpus file shifts its
line numbers — the failure this very commit is correcting, one file over.
Re-bless it with the fix; a yamsql-only update leaves the golden red and reads
as unrelated drift.

**THE DECISION: the plan tree carries STORAGE names, translated AT EVERY POINT
A TABLE NAME ENTERS IT.** In Go that is four sites, not the one an earlier draft
named: the scan leaf (`cascades_translator.go:2904`, `s.Table`) and the three
DML targets — INSERT `:10750` (`ins.Table`), UPDATE `:10792` (`upd.Target`),
DELETE `:10814` (`del.Target`). Candidates keep `rt.Name`;
`EqualsWithoutChildren` then compares like with like and never learns about
escaping.

Translating only the scan leaf would ship a TWO-NAMESPACE PLAN TREE —
`Scan(MY__1TABLE)` beneath `Delete(MY$TABLE)` — and the memo-identity argument
below applies to the DML targets verbatim, because all three DML expressions compare
that string in `EqualsWithoutChildren` -- it IS the structural key:
`cascades/expressions/delete.go:84`, `cascades/expressions/insert.go:107`,
`cascades/expressions/update.go:146`. All three carry their directory because
`plans/` holds a `delete.go`, an `insert.go` and an `update.go` of its own -- a
bare basename names two files here, not one. THE UPDATE TARGET IS NOT JUST A NAME -- IT IS A CORRELATION IDENTIFIER, and
that constrains how it may be translated. `executeUpdate` builds the target
quantified-object value as
`NewQuantifiedObjectValue(NamedCorrelationIdentifier(p.GetTargetRecordType()),
...)` (`executor.go:4110-4112`), and the SET right-hand sides are correlated to
it. Re-spelling `upd.Target` alone would leave `SET name = name` bound to a
correlation nobody publishes.

The answer is to CARRY THE CORRELATION SEPARATELY from the structural identity,
and that is not one option of two. Java never couples them:
`QueryVisitor.java:836` sets `targetRecordType` from `getStorageName()` at
construction, and `UpdateExpression.java:100-105` correlates the transforms to
the SOURCE quantifier only. Go's coupling is its own, originating at
`logical_predicate.go:7009` where `buildSelectScope` takes the bare table name
as the alias. "Rebase the transforms onto the new identifier" would preserve
that divergence while working around it. INSERT has no such coupling: `executor.go:3962`
resolves ITS target through the tolerant `GetRecordType` -- an INSERT-only
path, not the general tolerance an earlier draft read it as.

Go's type filter needs nothing: the translator never builds one. It arrives from
`primary_scan_match_candidate.go:432`, already carrying the stored name.

That is a 1:1 port. Java's `LogicalOperator.generateTableAccess` builds the scan
from `semanticAnalyzer.getAllTableStorageNames()`
(`LogicalOperator.java:262-270`), which is
`table.getType().getStorageName()` (`SemanticAnalyzer.java:288-298`), and its
type filter from `type.getStorageName()` (`LogicalOperator.java:280-282`). Java
does the same for all three DML targets: INSERT `LogicalOperator.java:585`,
UPDATE `QueryVisitor.java:836`, DELETE `QueryVisitor.java:876`, each reading
`getStorageName()` off the target type. Five sites in Java, four in Go. The
candidate side maps `RecordType::getName`
(`PrimaryScanMatchCandidate.java:131-136`), which is `descriptor.getName()`
(`RecordType.java:69`). Both sides independently arrive at the mangled string,
which is exactly what makes the raw `recordTypes.equals` at
`FullUnorderedScanExpression.java:130` sound rather than lucky.

**AN EARLIER DRAFT OF THIS SECTION DECIDED THE OPPOSITE, and both of its
objections described Java's shipped behaviour rather than a cost.**

  - It said storage names in the plan would make EXPLAIN print
    `Scan(MY__1TABLE)`. Java's EXPLAIN prints exactly that:
    `SCAN([IS foo__2table__1nested, ...])` and
    `SCAN([IS my__1adjacency__1list, ...])`, at
    `valid-identifiers.yamsql:237` and `:422`. The RECORD-TYPE name is mangled;
    :237 goes on to project `level2$field.1` in the same line, so the columns
    beside it are NOT. Index names are mangled nowhere -- the covering scan at
    `:222` prints `foo.table$nested.repeated.idx.field.1.3` verbatim, and
    `:227`/`:232` print sibling index names the same way. Table names mangled,
    column and index names not. So Go printing
    `Scan(MY$TABLE)` is itself a divergence on the shared surface, not a
    feature to protect.
  - It said the render boundary would then have to decode through a
    non-injective map. Java DOES decode -- `Record.fromDescriptorPreservingName`
    is `new Record(ProtoUtils.toUserIdentifier(descriptor.getName()),
    descriptor.getName(), ...)` (`Type.java:2591-2593`), and
    `RecordMetadataDeserializer.java:92` derives the user name the same way --
    so "it would not decode" was simply false. The objection still fails, for a
    different and narrower reason: nothing in the MEMO resolves through it. Java
    decodes once at construction and carries both spellings on the Type, and the
    scan, type filter and DML targets are all built from `getStorageName()`, so
    the decoded name never enters a structural key. It DOES serve lookups
    elsewhere -- `SemanticAnalyzer` resolves a SQL table through
    `findTableByName` (`RecordLayerSchemaTemplate.java:242-248`), comparing
    `RecordLayerTable.getName()`, and `RecordMetadataDeserializer.java:92-96`
    keys its builder map by the decoded name, so a decode collision routes two
    declarations through one builder there. Go documents the same misresolution
    in the doc comment on `GetRecordType` (`pkg/recordlayer/metadata.go`). The OBJECTION was about the plan tree, and confined there it fails. Outside
    the memo it gains force rather than losing it -- so it is this REBUTTAL,
    not the objection, that must not be stated any wider.

**AND THE SQL NAMESPACE IS NOT A LEGAL MEMO IDENTITY, which is the argument that
settles it independently of Java.** Candidate scans flow `UnknownType`, so per
`full_unordered_scan.go:110-118` the record-type NAMES are the sole
discriminator between two scan expressions. SQL names are not injective, and
the example has to be chosen against the rule that DERIVES them, which an
earlier draft got wrong: it paired a proto message named `A__1B` with a DDL
table declared `"A__1B"`, but under Java's decode the proto presents `A$B` and
the pair does not collide at all. A pair that DOES, under the decode rule
whichever side supplies it: `X__0__1Y` and `X____1Y` both decode to `X__$Y`
(the scan replaces `__1` before `__0`, so the first loses its `__0` and the
second keeps two underscores). Two distinct record types, one memo key. Storage
names are injective by construction — they ARE the descriptor's name.

**WHERE THE SECOND NAME LIVES: not in `pkg/recordlayer`.** Java's core
`RecordType` has ONE name, and `pkg/recordlayer` is the wire layer whose
`RecordType` maps 1:1 to a stored protobuf message. The two Java-shaped homes
are `values.RecordType`, which should grow `storageName` beside `name` as a port
of `Type.java:2185,2225-2233,2536-2539`, and `metadata.RecordLayerTable`, whose
`MetadataName()` returns `underlying.Name` (`metadata/table.go:53`) and thereby
conflates the two names at the one place Java keeps them apart
(`RecordLayerTable.java:62` and `:77`).

**THE POPULATION IS ESCAPED NAMES, AND THE CASE ARM WAS A DIFFERENT BUG THAT IS
FIXED HERE.** This section reached that answer three times by three routes, and
only the third is sound. Recording all three, because the first two are the
readings a later engineer will re-derive.

FIRST: "escaped names only", from a SELECT-only measurement. Wrong as method —
SELECT and DML did not resolve a table name the same way and nothing said so.

SECOND: "two populations, and case is the larger one", after measuring DML.
`recordTypeCI` (`logical_predicate.go:6801`) resolved a DML target
CASE-INSENSITIVELY, so an unquoted `DELETE FROM customer` against a table
declared `"Customer"` VALIDATED and then planned
`Delete(CUSTOMER, PredicatesFilter(Scan(CUSTOMER), [1 preds]))` — a target
naming no record type in that metadata, over a scan that matched nothing. The
statement reported success having deleted no rows; UPDATE modified none. Pinned
end to end in `yamsql/testdata/unquoted_dml_against_a_quoted_table.yaml`, which
without the fix reports `expected error 42F01, got nil` on both.

THIRD, and correct: that fold was never a legal alias to canonicalise. It was a
VALIDATION DIVERGENCE, and every other path already said so. Java rejects —
`select * from restaurant` against a table declared `"Restaurant"` throws
UNDEFINED_TABLE / "Unknown table RESTAURANT"
(`CaseSensitivityQueryTests.caseSensitiveConnectionTestCase3`). Go's SELECT path
rejects. Go's `INSERT … VALUES` rejects, through `md.GetRecordType(insOp.Table)`
(`cascades_generator.go:1074`). Only UPDATE and DELETE folded.

So the DML target now resolves strictly, and the case arm leaves this section
entirely. **Canonicalising it — which is what this section proposed one revision
ago — would have been worse than the bug.** A rewrite from `CUSTOMER` to the
stored `Customer` lets an unquoted write MUTATE a table that can only be named
with quotes: silently doing the wrong thing in place of silently doing nothing.

What remains is escaping, and it is genuinely different in kind: quoting is
MANDATORY to write `"MY$TABLE"` at all, and quoting it still does not make the
SQL spelling and the stored spelling coincide. There is no strict-resolution
answer available there, because the two names are different by construction
rather than by a resolver being lax.

Storage-name rewriting is therefore reserved for the protobuf escaping — one
rule, one population, and not a general licence to canonicalise whatever a
resolver happened to match.

THE TWO MECHANISMS ARE UNLIKE, and an earlier revision of this section stated a
single rule over both — "a name a resolver matched loosely must be replaced by
what it matched" — which is where the wrong prescription came from.
`GetRecordType`'s retry is an ENCODING (`pkg/recordlayer/metadata.go`):
`ToProtoBufCompliantName` is total and deterministic, the SQL name and the
stored name are two spellings of ONE declaration, and its ambiguity is a
declared-name collision rather than an iteration-order accident. Rewriting there
is recovering what the schema already said. The case fold is a SEARCH — one Java
never performs — over names that are two DIFFERENT declarations, and rewriting
there invents an identity the schema denies. Same-looking operation, opposite
correct response.


**THE CANDIDATE SIDE MUST NOT MOVE.** `rt.Name` reaches candidates at four
places (`cascades_generator.go:2842`, `:3471`, `:3695`, `:3770`) and those are
cross-compared in `rule_aggregate_data_access.go:84,299`; converting one
silently disables aggregate matching. `queriedRecordTypes` flows into physical
plans (`primary_scan_match_candidate.go:393,432`). Translating on the QUERY side
leaves every one of them untouched, which is the other reason it is the right
place.

**AND THE FIX SWITCHES MATCHING ON, which is the point but should be said out
loud rather than discovered.** FIVE gates compare the SCAN's record types against
a CANDIDATE's and therefore decline today for the same reason the primary
candidate does: `rule_aggregate_data_access.go:84` and `:831`,
`rule_ordered_index_scan.go:74`, `rule_implement_nested_loop_join.go:4480`, and
`rule_streaming_agg_from_index.go:100`, which is live in
`BatchAExpressionRules`. Landing this turns aggregate matching, ordered-index
matching, FK-probe matching and streaming aggregation ON for those tables in one
step, so the plan diff at fix time is wider than the two scenarios and the
golden will move in ways that are CORRECT rather than drift.

THAT PREDICTION IS NOW MEASURED FOR THE STREAMING-AGGREGATE GATE rather than
reasoned about, because it is the sentence that would wave a wide golden diff
through unexamined at fix time. One escaped table and one unescaped one, same
covering index, same grouped query
(`yamsql/testdata/escaped_table_grouped_aggregate.yaml`):

```
u         StreamingAgg(keys=[G], IndexScan(U_G, [*] COVERING))
"AGG$T"   StreamingAgg(keys=[G], InMemorySort([G ASC], Scan(AGG$T)))
```

The escaped table does not merely lose an access path: it materialises an
IN-MEMORY SORT where the index supplied the order. Same rows, and a plan whose
cost is a different shape. Both are asserted at the values they HAVE, so the
fix has to come to that file.

Two NEAR MEMBERS are not members, and both were on an earlier version of this
list. `rule_aggregate_data_access.go:299` is candidate-vs-candidate. And
`rule_type_filter_redundant.go:51` is query-vs-QUERY —
`typesAreSubset(scan.GetRecordTypes(), tf.GetRecordTypes())`, both operands from
the same subtree, so re-spelling moves them together and the outcome cannot
change. The rule is also starved: no query-side producer of
`LogicalTypeFilterExpression` exists, which this section already says where it
states the decision -- "Go's type filter needs nothing: the translator never
builds one". A gate whose two operands share an origin is not a gate on this
change.

**THE CONTINUATION SALT DOES MOVE, and saying it does not was wrong.**
`PrimaryScanRule.OnMatch` builds the physical plan from the LOGICAL leaf's
names (`rule_primary_scan.go:46`), and `executor.go:320` feeds that plan to
`primaryScanRangeFingerprintSalt` — so for an escaped table the salt input goes
from `MY$TABLE` to `MY__1TABLE`. That is harmless, but only for a reason with an
expiry condition, which is why it has to be written down rather than waved
through: the salt is computed ONLY when `len(comps) > 0` (`executor.go:319`),
and an escaped table has no pushed-down comparisons today, so no continuation
can exist through that path to be invalidated. The fix creates the pushdown and
the salt in the same stroke. Anything that changes that ordering — a partial
landing that gives an escaped table comparisons before the salt input settles —
breaks live continuations.

**This is a DESIGN CHANGE TO THE CASCADES MATCHING INFRASTRUCTURE and is not
implemented here.** It needs its ACK before the code, which is the gate this repo
puts on query-engine work. What IS here is the measurement, the pinned
divergence in two scenarios, and the four boundary fixes — which stay correct
independently of the fifth, because they make a caller holding either spelling
correct, the contract `GetRecordType` already offers.

### 7d. Half a line-cite gate is worth building, and the other half is discipline

The obvious response to a drifting `file.go:NNN` cite is a gate over every cite
in `rfcs/`. Half of that is worth having and is now built; the other half
measures citation STYLE and is not.

**RESOLUTION — built.** A cite naming a basename the tree holds three of is not
a cite: it resolves to whichever file the walk meets first. That is a real
defect class under any style, so `pkg/docscheck`'s
`TestRFC238CitesResolveUniquely` holds ambiguous, dangling and past-EOF at zero
— for THIS document only; the rest is a cleanup, not a widening.

**CLASSIFICATION — not built.** Most resolved cites that are not code land on a
`//` line, and citing a doc comment is a legitimate thing to do, so at that rate
the check scores style rather than staleness.

THE FIGURES BEHIND BOTH PARAGRAPHS ARE COMPUTED, NOT QUOTED FROM MEMORY.
`TestRFCCiteCensusRepoWide` prints the whole census on every run:

    bazelisk test //pkg/docscheck:docscheck_test --test_output=streamed \
      --test_arg=-test.run=TestRFCCiteCensusRepoWide --test_arg=-test.v

NO SNAPSHOT OF THOSE FIGURES APPEARS HERE. One did, and it rotted twice inside
this branch -- the second time while `918 weak` stayed ACCIDENTALLY correct
across the drift, which is exactly the half that cannot signal staleness. The
numbers move with every RFC edit INCLUDING edits to this section, so a prose
copy is stale the moment the section is touched. Pinning them instead would be
the style gate this section rejects. Run the command.

`TestRFC238WeakCitesAreTheOnesSection7dNames` pins the one thing worth pinning:
the weak SET for this document, by name. WHAT THAT PINS IS THE SET AND NOTHING
ELSE — of the figures the census prints, only three carry assertions, and they
are FLOORS against a collapsed population (files, distinct cites, resolved), not
values. The classification split is asserted nowhere. In
THIS revision the set has FIVE members. Three cite a doc comment on purpose:
`positional_row.go:7`, `colref.go:95` and `full_unordered_scan.go:110-118`. Two
are this section's own narrative naming cites it CORRECTED —
`derived_unnest.go:250` and `cascades_generator.go:2826` — which read weak
precisely because they name lines that have since moved, the thing the sentences
around them say.

A cite earns a line number when the line is the thing being pointed at. When the
target is "this function", the function's name is the cite and cannot drift —
which is why the `metadata.go` cites, corrected six times in this one branch,
name the function now and are no longer in the set above.

THE DISCIPLINE IS THE OTHER HALF, and it took three failures to state:

  1. When a commit edits a file, re-run the cites INTO that file. The first
     failure was fixing only the copies a reviewer had named.
  2. SWEEP rather than patch — over the whole document, not over the sentences
     you remember writing. The second failure was patching five cites in a
     report and missing seven more in the same file.
  3. Compute the cites LAST, against the tree as it will be COMMITTED. The third
     was the subtlest: the sweep was correct when run, and the same commit then
     deleted one line from one file and inserted ten into another, so every
     number it had just fixed was off by one.

The practical form of (3) is that the checker run is the LAST thing before
`git commit`, with no edit between them.

A measurement taken with a broken instrument does not fail. It produces a number
to reason about, and each wrong explanation of that number survives until
someone re-runs the thing rather than re-reading the sentence.

**As a "does the cite still name its function" check: it was silent for five of
the six corrections examined by hand.** Only `derived_unnest.go:250` → `:287`
crossed a declaration boundary. Four others drifted INSIDE one function —
`:3022`/`:3031` in the same builder, `:2817`/`:2826` both inside
`buildMatchCandidates`, `:4136`/`:4204` both inside the same rebase,
`:924`/`:925` both in `derivedOutputColumns` — and the sixth moved repeatedly
inside the doc comment above `GetRecordType`, pushed down every time something
was added above it, including twice by this section. Its successive line numbers
are NOT listed here: each listing was itself a cite that drifted, and a sentence
whose number was wrong at every revision that quoted it should not carry the
number. That cite now names the FUNCTION and cannot drift again. A function-range
gate is silent for that one too, so it would have fired for one of six.

THAT IS A SAMPLE, NOT THE BRANCH, and the branch figure is DELIBERATELY ABSENT.
An earlier revision said "the seven cites this branch corrected", which was one
commit's count written as if it were the branch's. Three later revisions each
replaced it with a different precise number, and a reviewer implementing the
rule as stated got anywhere from 47 to 70 depending on how a "correction" is
counted — one file cited from several places yields several corrections for one
edit, and whether to count occurrences or cite-sites is not settled by the
sentence. A number nobody can reproduce from the rule beside it is worse than no
number, because it reads as measured.

What IS reproducible, and is all this section needs: it was never seven; the
drift is dominated by whole-file shifts rather than moves between functions
(`cascades_generator.go:2826` alone moved four further times, `:2840` → `:2839` →
`:2848` → `:2858`, every one staying inside the same function); and the corpus
figures above are printed by a committed test rather than quoted from memory. If
a branch-wide count is ever wanted, ship the counter next to the claim — four
revisions of prose did not get there.

### 7e. The target is validated after its columns are, and UPDATE is on both sides of it

Making the DML target strict exposed an ORDERING that the fold had been hiding.
The 42F01 guard runs on the BUILT logical operator, so anything that resolves
columns during construction speaks first. Measured against
`CREATE TABLE "Customer" (id BIGINT, name STRING, PRIMARY KEY (id))`, FOUR rows,
and the fourth is why the heading is not "only UPDATE escapes":

```
UPDATE customer SET nosuchcol = 'z'          42F01   closed below
UPDATE customer SET name='z' WHERE nosuchcol=1   42703   column "NOSUCHCOL" ...
DELETE FROM customer WHERE nosuchcol = 1     42703   column "NOSUCHCOL" ...
SELECT nosuchcol FROM customer               42703   column "NOSUCHCOL" ...
```

Every one names a column of a table that does not exist. Java reports the TABLE
in all four — its DML visitors and its analyzer both resolve the target through
an exact `SemanticAnalyzer.getTable` before anything looks at a column.

UPDATE IS ON BOTH SIDES, BY CLAUSE, and that is the shape to carry away rather
than "UPDATE is fixed". Its SET-column check had its own `recordTypeCI` call
(`logical_predicate.go:6900`) that folded case purely to find a descriptor, so
making it strict leaves `rt` nil for an unresolvable target, the SET check
declines, and the 42F01 answers. Its WHERE clause does not go through that
check at all.

THE OTHER THREE ROWS SHARE ONE RESOLVER AND CANNOT BE CLOSED SEPARATELY.
`buildWherePredicateForTableE` (`logical_predicate.go:130`) resolves its table
through `semantic.Analyzer.ResolveTable` over `rlcatalog.Wrap(md)` — the SAME
analyzer the SELECT path uses, and the one UPDATE's WHERE reaches. Making it
strict fixes the UPDATE-WHERE, DELETE-WHERE and SELECT rows together, moves all
three toward Java, and changes the SQLSTATE of every existing query that names a
bad column on a case-mismatched table. That is a decision about the SELECT
diagnostic surface, not about DML, and it is not made here.

All four rows are asserted at their measured values in
`unquoted_dml_against_a_quoted_table.yaml`, the SELECT twin sitting beside the
DELETE one specifically so the pair cannot drift apart unnoticed. An earlier
version of this section had three rows and said the two WHERE rows "agree with
each other and disagree with UPDATE"; the fourth row was added to the yaml and
the prose was never re-run, so it went on describing a population it no longer
had. The yaml arm points back here, which is exactly how a reader would have
landed on prose contradicting the arm that sent them.

**Follow-on booked here, not implemented:** unify the two 42F01 wordings.
`Unknown table %s` is what Java uses on the whole query path
(`SemanticAnalyzer.java:249`, reached for sources and for all three DML targets
at `QueryVisitor.java:753`, `:813`, `:867`); `table <X> does not exist` exists in
Java only in the JDBC catalog API (`CatalogMetaData.java:207,319`). So Go's
SELECT path is the divergent side and should change, not DML. It is deferred
because everything this branch did was an ADDITIVE rejection, while unification
re-words queries that currently PASS, across every corpus entry pinning the
text — a different risk class.


### 7f. The aggregate-index row type degrades silently, and one axis is now closed

`RecordQueryAggregateIndexPlan.recordTypeName` feeds a metadata lookup that
derives the plan's result-column types, and that lookup is NIL-TOLERANT. On a
miss the descriptor stays nil and the types are invented: one derivation reports
every GROUP BY column as `STRING` and an aggregate over a COLUMN as `BIGINT`
(`cascades_generator.go:4189`), the other reports GROUP BY columns as `BIGINT`
(`:4256`) and reaches its miss only when EVERY child plan misses. `COUNT(*)` is
`BIGINT` with or without the miss, so it is the one output not degraded.
Plausible values, wrong types, no error — and which wrong type you get depends
on which derivation ran.

A draft comment at the field said no such lookup existed and used that to
dismiss the §7c namespace question. Both halves were false, and the second is
the point: this is the consumer MOST exposed to that question, not one immune
to it.

TWO WAYS TO REACH THE MISS ON PAPER. Neither is reachable, and they are closed
for different reasons, which is why they stay separate here:

  - NAMESPACE — FORBIDDEN BY §7c'S COMMITTED DESIGN, so it does not arm and is
    recorded only to keep the next reader from re-deriving it. `recordTypeName`
    comes from the candidate's stored names at all three construction sites, so
    both spellings resolve: `GetRecordType` maps SQL to storage through the
    escaping, and case mismatches are rejected before planning. It WOULD arm if
    candidates moved into the SQL namespace — and §7c rules that out in bold,
    translating on the QUERY side precisely so the candidate side does not move.
    A draft of this bullet cited §7c as the thing that arms it, which reads as
    licence to make the change §7c forbids.

  - EMPTY ASSOCIATION — CLOSED IN `Build`, AFTER AN ENUMERATION OF ROUTES GOT
    IT WRONG. `RecordTypesForIndex` returns nothing for an index that is neither
    universal nor associated; the aggregate rule then leaves `recordTypeName`
    empty and `GetRecordType("")` misses.

    A revision of this bullet said every route ran through a SECOND `SetRecords`
    call, and closed on that basis. Java does forbid the second call: of its FOUR
    `setRecords` overloads (`RecordMetaDataBuilder.java:371`, `:382`, `:410`,
    `:421`) the two that do the work (`:384`, `:423`) open with
    `if (recordsDescriptor != null) { throw new MetaDataException("Records
    already set."); }` and the other two delegate to them.
    `setLocalFileDescriptor` carries the same guard; `addDependency` carries a
    similar one with a DIFFERENT message. Go permitted the call, and porting
    that guard is right on its own terms. But it is NOT the only route, and the
    claim that it was survived a full review round because nobody could
    enumerate what nobody had thought of.

    THE BUILDER HANDS OUT LIVE MAPS. `GetRecordTypes` returns `b.recordTypes`
    itself, not a copy, so `AddIndex("Order", idx)` followed by
    `delete(b.GetRecordTypes(), "Order")` reaches the state with ONE `SetRecords`
    call and no error; after `Build`, `RecordTypes` and `GetAllIndexes` are live
    too. Java never had to defend this — its builder has no `getRecordTypes` at
    all. So the property is now asserted in `Build`: every registered index must
    be universal or claimed by some record type, or the build fails naming the
    index. That closes the routes above and the ones not yet found, which is the
    difference between a checked property and a list.

WHAT THE STATE COST, recorded because a check is only obviously worth keeping
once someone knows what it stops. A second `SetRecords` replaced every
`RecordType` THE SECOND DESCRIPTOR ALSO DECLARED with a fresh one whose index
slices are nil -- a type present only in the first survived with its indexes
intact, which an earlier revision of this paragraph got wrong -- while
the flat registry `b.indexes` kept its entry, so an index registered against a
type the second descriptor STILL DECLARED came back associated with nothing.
`Build` succeeded. `RecordTypesForIndex` came back empty and
`GetIndexesForRecordType` lost it — an index-maintenance hole before it is a
row-type one. And `ToProto` still emitted the index, with an EMPTY `RecordType`
list, which a reload reads as UNIVERSAL: it returned either maintained for every
record type, or, when its key is not valid on all of them, as metadata that will
not load at all, since `Build` validates a universal index against every type.
Java reads those bytes identically — an empty list is its INTENDED universal
encoding — so the reader was never the defect. The writer was.

THE GUARD RETURNS BEFORE IT ASSIGNS, which is the part a reimplementation gets
wrong. Java throws before touching any state; Go records a build error and
returns, so a caller that ignores the `Build` error still holds the first
descriptor rather than a half-merged one. That ordering has exactly one witness:
with the error recorded but the assignment left in place, all four refusal arms
still pass, because `Build` still fails.

Pinned by `TestSetRecordsRefusesASecondCall` — four registration spellings, each
carrying its own control that asserts the association EXISTED first, without
which a spelling that silently associated nothing satisfies its own
assertion — by `TestRefusedSetRecordsLeavesTheFirstDescriptorInPlace` for the
ordering, and by `TestUniversalIndexRoundTripsThroughAnEmptyRecordTypeList`,
which pins the encoding so a later reader does not mistake the reload behaviour
for the bug. The `Build` check has its own arm,
`TestBuildRefusesAnIndexNoRecordTypeClaims`, which drives the live-map route and
separately pins that a UNIVERSAL index is EXEMPT -- without that second half, a
check rejecting every unassociated index would satisfy the first.

Two drafts got the mechanism wrong in opposite directions and both are worth
recording, because each is the reading a later engineer arrives at first. One
said `SetRecords` "repopulates" the record types — it does not; the map is
created once and only inserted into, so a second call MERGED. The other said the
orphan came from the second descriptor DROPPING the type — also wrong, and for
the same reason: a dropped type kept its old entry, indexes intact. The danger
was the type SURVIVING the second call.
