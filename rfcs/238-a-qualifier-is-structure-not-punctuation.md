# RFC-238: a qualifier is structure, not punctuation

**Status:** PROPOSED
**Scope:** how a source qualifier travels from the resolver to the executor's row map, and every consumer that recovers one from a flat string today.
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

**FIVE sites render it**, not two. The first draft of this RFC listed two, which
is the census failure this repo has a rule about — a count taken over the file
that motivated the work rather than over the population:

| site | what it renders |
| --- | --- |
| `cascades_translator.go:767` (`legColumns`) | `strings.ToUpper(alias) + "." + strings.ToUpper(col)` |
| `logical_result_type.go:621` (`logicalLegFields`) | `strings.ToUpper(alias) + "." + col` |
| `scalar_subquery_seed.go:143` | `strings.ToUpper(innerAlias) + "." + scalarCol` |
| `clustered_outer_scalar.go:509` | `leg.binding + "." + leg.typ.Fields[i].Name` |
| `clustered_outer_scalar.go:537` | `strings.ToUpper(innerAlias) + "." + scalarCol` |

The last three are the correlated-scalar and clustered-outer seeds, and they
matter to the ORDER below: collapsing only the first two leaves three live paths
still discarding provenance and still spelling datum keys independently — and
note they already disagree among themselves on the case question, `:509` keeping
the leg's own slot name verbatim while `:143` and `:537` fold the alias.

At least two parse it back:

| site | how it recovers the qualifier |
| --- | --- |
| `colref.go` (`parseColRef`) on the label path | last dot at paren depth zero |
| `cascades_translator.go:924` (`derivedOutputColumns`) | `strings.LastIndexByte(name, '.')` |

The two renderers already disagree — by case, on the column half — which is a
divergence in its own right and is pinned by
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

`ExactLogicalOutputLabels` is the existence proof for the fix. It never renders,
so it never parses, and it needs no heuristic — its own doc states the rule:
"removed STRUCTURALLY, by not adding it, never by slicing at a dot".

## 3. The design

Carry `(sourceAlias, name)` as separate data on the field, alongside the flat
name during migration. Label derivation then reads the structured qualifier
instead of re-finding it, and the five renderers collapse to one.

**THERE IS NO EXECUTOR ROW-MAP BOUNDARY, and the first draft of this design was
built on one.** `PositionalRow` is the SOLE runtime row (`positional_row.go:7`):
slots are indexed by ORDINAL and every column reference reads a plan-time-baked
ordinal — "there is no runtime name resolution", the Type's names serve plan-time
binding and diagnostics. The one name-keyed projection, `positionalToMap`
(`:284`), is deliberately lossy and DML-only: it drops slot order and collapses
duplicate names LAST-WINS, and its doc says a caller that is not itself
name-keyed must not use it.

So "render once at the executor's row map" would either be dead code or would
re-introduce the duplicate-name loss that projection exists to warn about. The
structured identity is preserved through the POSITIONAL type and layout — where
it already lives — and the single rendering site is the PRESENTATION boundary:
where a `ColumnDef` gets its `Name` and `Label`, which is the only place a flat
key reaches anything that cannot address by ordinal.

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
   qualifier. It is a SECOND parser and the four steps as first written left it
   standing while deleting the first one's limits, which would have hidden the
   same ambiguity one site over.
5. Delete `parseColRef` from the label path and delete its limits.

## 5. Acceptance criteria

These make the collapse checkable rather than asserted. Each one is a command
with an expected output, because a criterion naming an artifact by a description
gets satisfied by assertion — two of the first draft's did, and they are
rewritten here.

**(1) The label path stops parsing.** `parseColRef` has **27** non-test callers
under `pkg/relational/core` today (`grep -rn --include='*.go' 'parseColRef('
pkg/relational/core/ | grep -v '_test.go' | wc -l`; positive control on the same
sweep: `declaresColumn` = 2, so the search is well-formed). "The label path" was
undefined in the first draft, which let any survivor be declared off-path. These
are the callers that must be GONE, by file:line at `2c0527b84`:

| file:line | what it does |
| --- | --- |
| `qualifier_stripped_label.go:76` | the interim itself; the whole file goes |
| `cascades_generator.go:5072` | `label := parseColRef(f.Name).bare()` |
| `cascades_generator.go:5337` | `label := parseColRef(f.Name).bare()` |
| `cascades_generator.go:5162` | `columnDefDisplayName` |
| `cascades_generator.go:6008` | `bare := parseColRef(name).bare()` |

The remaining 22 are NOT in scope and must be unchanged: they read a bare name
to look up a proto field (`:5015`, `:5114`, `:6289`, `:7185`), to compare
against a display name (`:4423`, `:5181`), to choose a descriptor (`:4057`), or
to test qualification (`:5945`, `:5960`), plus the callers in `colref.go`,
`eval_map.go`, `logical_predicate.go` and `select_helpers.go`. Those are lookup
and comparison, not label derivation; they are a separate question and moving
them here would make this criterion unmeetable.

**(2) The two limits are deleted, not re-documented — and only after BOTH
parsers are gone.** They are prose, not markers: `grep -rn "KNOWN LIMIT"
colref.go` returns nothing today, which the first draft's wording did not
survive. They live in the block at `colref.go:95` headed *"WHAT IT COSTS IS TWO
SHAPES, NOT ONE"*: a literal containing a matched paren, and a literal
containing a depth-zero dot. That block, and the two rows in
`colref_split_test.go` that pin them, are deleted.

The ordering is load-bearing. `cascades_translator.go:924` recovers a qualifier
by `strings.LastIndexByte`, which has the SAME ambiguity and does not even have
the paren protection — deleting `colref.go`'s documented limits while that site
lives moves the ambiguity rather than removing it, and moves it somewhere
undocumented. Step 4 is what makes step 5 honest.

`parseColRef` keeps 22 lookup/comparison callers after this (criterion 1), and
those are NOT covered by the deletion: what is deleted is the file's claim to be
a safe channel for LABELS, and the two limits are limits of that claim.

**(3) The interim's limits are removed BY the fix.** `grep -c 'name:  "LIMIT:'
pkg/relational/core/embedded/qualifier_stripped_label_test.go` returns 2 today
and must return 0 — by the file being deleted, not by the arms being relaxed.

And the SQL-visible half moves with them: `quoted_identifier_labels.yaml`'s
`SELECT "X.TOTAL" FROM xprobe` is pinned at today's WRONG label (`TOTAL`, where
Java says `X.TOTAL`), so this RFC landing turns that arm red. Flipping it to
`X.TOTAL` in the same commit is the demonstration; its sibling — the correlated
read, correct today at `TOTAL` — must stay green, because a fix that pays for
one by breaking the other has not separated them, it has moved the wrong answer.

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

## 6. Why it is one change and not four items

**The load-bearing reason is §1:** the four steps are the two ends of one
mechanism. A qualifier carried as a rendered string has to stop being rendered
AND stop being parsed, and neither half is a fix on its own.

*Secondary, and only about one particular bad split:* step 1 is deliberately
reader-less and golden-identical, so landing it alone leaves structured
provenance that nothing reads — indistinguishable from dead code, and the next
sweep deletes it. That argues against splitting after step 1 specifically. It is
not the argument for one change; §1 is.

This touches datum keys engine-wide.
