# RFC-238: a qualifier is structure, not punctuation

**Status:** PROPOSED
**Scope:** how a source qualifier travels from the resolver to the two boundaries that consume a flat key — presentation, and leg resolution in the executor — and every producer and parser between them.
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

**SEVEN sites MINT A LEG DATUM KEY.** The first draft listed two, which is the
census failure this repo has a rule about — a count taken over the file that
motivated the work rather than over the population.

The population sweep, and what it actually returns:

```
grep -rn --include='*.go' -E '\+ "\." \+' pkg/relational/core/ pkg/recordlayer/query/ \
  | grep -v '_test.go' | wc -l          # 31
```

**31 raw hits, of which 7 mint a leg datum key onto a `values.Field` or a
`ColumnDef`.** The other 24 render a dotted string for a diagnostic message, a
display label, or scope bookkeeping (`logical_predicate.go`'s `projCol.name`
alongside an explicit `qualifier`/`bare` pair, which is already structured and
is what this RFC generalizes). The number that matters is 7 and the number the
grep prints is 31; classifying the difference is step 1's work, and any of the
24 found to mint a key joins the table rather than being argued out of it.

| site | what it renders | after |
| --- | --- | --- |
| `cascades_translator.go:767` (`legColumns`) | `ToUpper(alias) + "." + ToUpper(col)` | GONE |
| `logical_result_type.go:621` (`logicalLegFields`) | `ToUpper(alias) + "." + col` | THE ONE |
| `scalar_subquery_seed.go:143` | `ToUpper(innerAlias) + "." + scalarCol` | GONE |
| `clustered_outer_scalar.go:509` | `leg.binding + "." + leg.typ.Fields[i].Name` | GONE |
| `clustered_outer_scalar.go:537` | `ToUpper(innerAlias) + "." + scalarCol` | GONE |
| `cascades_generator.go:5954` (`qualifyAndMergeColumns`) | `firstAlias + "." + ToUpper(c.Name)` | GONE |
| `cascades_generator.go:5964` (`qualifyAndMergeColumns`) | `secondAlias + "." + ToUpper(c.Name)` | GONE |

The middle three are the correlated-scalar and clustered-outer seeds and they
already disagree among THEMSELVES on the case question — `:509` keeps the leg's
own slot name verbatim while `:143` and `:537` fold the alias. The last two are
the presentation renderer, which composes `ColumnDef.Name`; the first draft's
"five collapse to one" omitted them and so was neither scoped nor checkable.

Note what the third column says: the surviving renderer is `logicalLegFields`,
the exact-type authority, NOT the presentation site. Presentation reads the
structured qualifier and renders only if a caller demands a flat key.

**FOUR sites parse it back, with THREE different rules**, and one of them is at
runtime:

| site | how it recovers the qualifier |
| --- | --- |
| `colref.go` (`parseColRef`) on the label path | LAST dot at paren depth zero |
| `cascades_translator.go:924` (`derivedOutputColumns`) | LAST dot |
| `ordinal_join.go:1168` (`rowSlotForLegColumn`) | **FIRST** dot |
| `ordinal_join.go:1238` (`isDottedQualifiedName`) | any dot, guarded by a `{`/`[` prefix |

The third is the reader half of this mechanism and it is the consumer of exactly
the three seed renderers above — its own doc says the dotted arm "serves the
correlated-scalar seed legs, whose seed leg types name columns literally
`LEG.COL`". It is called ungated from `:1057`; only the census recording inside
it is gated.

The fourth is worse than a naming defect: `isDottedQualifiedName` decides
CONTROL FLOW — whether a row is merge-shaped — from a bare dot test. A scan row
carrying a column declared `"a.b"` is misclassified there, which is §0 inside
the executor, choosing a join arm.

**And the structure it needs already exists downstream.** `rt.Legs` carries
`(Name, Start, Width)`, and `rowSlotForLegColumn` parses the string to recover a
qualifier it then matches against those very legs. The thesis of this RFC is
half-built already; what is missing is that the field does not say which leg it
came from, so the reader re-derives it from punctuation.

Two of the five renderers already disagree — by case, on the column half — a
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
instead of re-finding it, and the five renderers collapse to one.

**THERE IS NO EXECUTOR ROW-MAP BOUNDARY**, and the first draft was built on one.
`PositionalRow` is the SOLE runtime row (`positional_row.go:7`): slots are
indexed by ORDINAL and every column reference reads a plan-time-baked ordinal.
The one name-keyed projection, `positionalToMap` (`:284`), is deliberately lossy
and DML-only — it drops slot order and collapses duplicate names LAST-WINS — and
its doc says a caller that is not itself name-keyed must not use it. Rendering
there would be dead code or would re-introduce that loss.

**BUT THE EXECUTOR STILL RESOLVES BY NAME**, and the second draft's "the
presentation boundary is the only place a flat key reaches anything that cannot
address by ordinal" was false. `rowSlotForLegColumn` (`ordinal_join.go:1153`,
called ungated from `:1057`) does a flat lookup, then splits at the FIRST dot and
matches both halves `EqualFold` against `rt.Legs`. `PositionalRow`'s doc is
accurate about `PositionalRow`; it was never a claim about the whole executor.

So there are TWO boundaries where a flat key is consumed, and the design has to
name both:

- the **presentation** boundary, where a `ColumnDef` gets its `Name` and `Label`
  — genuinely the last place a flat key must exist, because a caller reading
  `Rows.Columns()` cannot address by ordinal;
- the **leg-resolution** boundary in `ordinal_join.go`, which does NOT need a
  flat key at all. `rt.Legs` already carries `(Name, Start, Width)`; a field
  that stated its own leg would be resolved by lookup, not by parsing.

The second is not a rendering site to keep — it is a parser to delete, and
deleting it is what makes the qualifier structural at runtime rather than only
at plan time.

**THE `EqualFold` COMPARATORS ARE PART OF THE DEBT, not incidental.**
`:1179`/`:1185` compare leg and column halves case-insensitively, which is why
removing the `legColumns` fold moved zero golden rows: the divergence pinned by
`leg_column_key_case_divergence_test.go` is MASKED at the reader, not benign.
A collapse that fixes the producers and leaves a folding comparator has not
made the key canonical; it has moved the tolerance.

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
3. Collapse ALL FIVE renderers, not the first two: `legColumns`' join arm defers
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
5. **Migrate the RUNTIME reader.** `rowSlotForLegColumn` resolves against
   `rt.Legs` by leg identity instead of splitting at the first dot, and
   `isDottedQualifiedName` asks the field whether it is leg-qualified instead of
   testing for a dot. Both `EqualFold` comparators become exact, which is what
   makes the collapse in step 3 observable rather than masked. This step is
   where the two-producer case divergence actually resolves.
6. Delete `parseColRef` from the label path and delete its limits.

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
Positive control on the same sweep, filtered the same way: `declaresColumn` = 1
(its one call site), not 2. "The label path" was also undefined, which let any
survivor be declared off-path. These are the callers that must be GONE, by
file:line at `2c0527b84`:

| file:line | what it does |
| --- | --- |
| `qualifier_stripped_label.go:76` | the interim itself; the whole file goes |
| `cascades_generator.go:5072` | `label := parseColRef(f.Name).bare()` |
| `cascades_generator.go:5337` | `label := parseColRef(f.Name).bare()` |
| `cascades_generator.go:5162` | `columnDefDisplayName` |
| `cascades_generator.go:6008` | `bare := parseColRef(name).bare()` |

The remaining 21 are NOT removed by this criterion: they read a bare name to
look up a proto field (`:5015`, `:5114`, `:6289`, `:7185`), to compare against a
display name (`:4423`, `:5181`), to choose a descriptor (`:4057`), or to test
qualification (`:5945`, `:5960`), plus the callers in `colref.go`,
`eval_map.go`, `logical_predicate.go` and `select_helpers.go`.

**TWO OF THEM ARE NOT MERELY "LOOKUP", AND THE FIRST DRAFT'S BLANKET SENTENCE
WAS WRONG ABOUT BOTH.**

`cascades_generator.go:7146` splits a projection name that HAS a
`ProjectionRefs` counterparty, and its own comment says a disagreement there
"does not merely resolve the wrong row, it RAISES ErrCodeUndefinedColumn on a
column the parser saw perfectly well". It is the same ambiguity with a louder
failure, and step 2 migrates it onto the structured qualifier along with the
label sites.

`descriptorForColumn:4057` and `protoFieldTypeName` split the name to pick a
FIELD, so §0's collision has a TYPE half as well as a label half: with `TOTAL
INTEGER` and `"X.TOTAL" BIGINT` in one descriptor, the dotted column's metadata
reports INTEGER — the flowed Long conflates the two widths, so nothing
downstream catches it. Fixing only the label leaves that wrong. Step 2 carries
the structured declared name into these lookups, and criterion (7) pins the
type and nullability.

The rest are lookup and comparison, a separate question; moving them here would
make this criterion unmeetable.

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

**1 today; must be 0 at step 4, before step 6 deletes `colref.go:95`.** Control,
because a zero from a mistyped path reads identically to success: that symbol
has **8** non-test hits repo-wide under `pkg/relational/core`
(`grep -rn --include='*.go' 'strings.LastIndexByte' pkg/relational/core/ | grep -v '_test.go' | wc -l`),
so a sweep returning 0 there is a broken command, not a finished migration.

`cascades_translator.go:924` recovers a qualifier by the LAST dot, with the same
ambiguity and none of `parseColRef`'s paren protection — deleting `colref.go`'s
documented limits while that site lives moves the ambiguity somewhere
undocumented rather than removing it. Step 4 is what makes step 6 honest.

`parseColRef` keeps 22 lookup/comparison callers after this (criterion 1), and
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

**(6) The runtime reader stops parsing, and its comparators stop folding.**

```
grep -c "strings.IndexByte(name, '.')" pkg/recordlayer/query/executor/ordinal_join.go
```

**2 today; must be 0 at step 5.** And the two `EqualFold` comparisons inside
`rowSlotForLegColumn` (`:1179` leg half, `:1185` column half) become exact.

This criterion is the one that makes (5) meaningful rather than decorative. The
case divergence pinned by `leg_column_key_case_divergence_test.go` moves ZERO
golden rows today precisely because those comparators fold — the two producers
disagree and the reader cannot tell. A collapse that fixes the producers and
leaves a folding comparator has moved the tolerance, not removed it, and (5)
alone would pass.

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

**The load-bearing reason is §1:** the six steps are the two ends of one
mechanism. A qualifier carried as a rendered string has to stop being rendered
AND stop being parsed, and neither half is a fix on its own.

*Secondary, and only about one particular bad split:* step 1 is deliberately
reader-less and golden-identical, so landing it alone leaves structured
provenance that nothing reads — indistinguishable from dead code, and the next
sweep deletes it. That argues against splitting after step 1 specifically. It is
not the argument for one change; §1 is.

This touches datum keys engine-wide.
