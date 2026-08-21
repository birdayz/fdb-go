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

Two sites render it:

| site | what it renders |
| --- | --- |
| `cascades_translator.go:767` (`legColumns`) | `strings.ToUpper(alias) + "." + strings.ToUpper(col)` |
| `logical_result_type.go:621` (`logicalLegFields`) | `strings.ToUpper(alias) + "." + col` |

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
name during migration. Render ONCE, at the executor's row-map boundary, which is
the only place a flat key is genuinely required — the row map is keyed by string
and two legs' `K` must stay distinguishable there.

Label derivation then reads the structured qualifier instead of re-finding it,
and the two renderers collapse to one.

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
3. Collapse the two renderers: `legColumns`' join arm defers to
   `logicalLegFields`, and the descriptor-name decision moves to that one
   boundary.
4. Delete the parser and its limits.

## 5. Acceptance criteria

This is what makes the collapse checkable rather than asserted. If any of the
three survives, the collapse did not happen and the stand-ins have become the
design:

- `parseColRef` has NO caller on the label path.
- Both of `colref.go`'s KNOWN LIMITs are **deleted**, not re-documented.
- `qualifier_stripped_label_test.go`'s three `LIMIT:` arms are **removed by the
  fix**, not carried past it.

Plus: `leg_column_key_case_divergence_test.go` goes red on step 3 and is deleted
there, because the two producers it watches no longer both exist.

## 6. Why it is one change and not four items

The four steps are the two ends of one mechanism. Splitting them across items
would leave the tree in a state where structured provenance exists and nothing
reads it, which is indistinguishable from dead code and would be deleted by the
next person to sweep for it.

This touches datum keys engine-wide.
