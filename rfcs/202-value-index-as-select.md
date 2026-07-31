# RFC-202 — One index generator, over the logical plan, for both index forms

Status: **PROPOSED**. Needs a Graefe ACK (it moves index metadata construction
onto the logical plan and it changes what the planner will be asked to match)
and a Torvalds ACK, before implementation starts.

Closes: CQ-71. Advances RFC-201's ledger by deleting the corpus's single largest
skip class.

**Three claims in the CQ-71 booking are REFUTED by measurement here.** They are
corrected in §1 rather than left standing, because each of them changes the
shape of the work:

1. **The blocked class is not "value indexes".** 12 of the 42 booked files fail
   on `exactly one aggregate expression required in SELECT` — Go's *aggregate*
   arm rejecting an *aggregate* index. Two of the 42
   (`composite-aggregates.yamsql`, `index-ddl-aggregates-only.yamsql`) declare
   **only** aggregate AS-SELECT indexes and are booked in this class anyway. A
   non-aggregate arm bolted next to the existing aggregate arm would leave the
   class populated. CQ-71's DONE condition is therefore unreachable without
   replacing the aggregate arm too.
2. **`union.yamsql` is not a hard shape.** TODO.md:12699 names it as carrying
   "the multi-column and multi-table shapes". Its only non-aggregate index is
   `create index vi1 as select col1 from t1` — the trivial single-column form.
   The genuinely hard shapes are enumerated in §3.
3. **INCLUDE must NOT stay fail-closed.** The brief scoped INCLUDE out as
   "explicitly rejected fail-closed today — keep". In Java, `CREATE INDEX i ON
   t(a DESC, b) INCLUDE(c)` and `CREATE INDEX i AS SELECT a, b, c FROM t ORDER
   BY a DESC, b` are **the same call**: `OnSourceIndexGenerator` builds one
   projection from key columns ++ INCLUDE columns, turns the key columns into
   `OrderByExpression`s, and hands the result to
   `MaterializedViewIndexGenerator.generate`
   (`OnSourceIndexGenerator.java:196-227`). Keeping Go's `INCLUDE` rejection
   (`pkg/relational/core/embedded/ddl.go:221-224`) after this port would be a
   deliberate divergence with no Java counterpart, and it would leave a second
   Go-only index-construction path alive — the thing this RFC exists to delete.

Every Java citation is at tag **4.12.11.0** (`fdb-record-layer/`, the pin in
MODULE.bazel). Every Go citation and every measurement is at **`ef2b5911a`**
("RFC-201 Phase 1", the branch point of `rfc/202-value-index-as-select`).

---

## 1. The problem, measured — with each number's instrument named

### 1.1 The ledger

Instrument: one full corpus run,

```
bazelisk test //pkg/relational/conformance/javacorpus:javacorpus_test \
  --test_output=streamed --nocache_test_results
```

at `ef2b5911a`, against a real FDB testcontainer. Exit 0; 206 `SKIP` lines
logged by `corpus_run_test.go:117`.

```
unsupported-DDL:value-index-as-select   42 files
```

42 is also the pinned value in
`pkg/relational/conformance/javacorpus/pinned_ledger_test.go:40` and `:53`. It
is the largest single skip class in the corpus, ahead of
`unsupported-DDL:struct=39`.

**The 42, verbatim from the run** (paths relative to
`third_party/apple/fdb-record-layer/yaml-tests/src/test/resources/`):

```
aggregate-empty-table.yamsql                   aggregate-index-tests-count-empty.yamsql
aggregate-index-tests-count.yamsql             between.yamsql
bitmap-aggregate-index.yamsql                  boolean-ddl.yamsql
case-sensitivity.yamsql                        composite-aggregates.yamsql
cte.yamsql                                     distinct-from.yamsql
exists-in-select.yamsql                        filter-index.yamsql
index-ddl-aggregates-only.yamsql               index-ddl-values-only.yamsql
index-ddl.yamsql                               indexed-functions.yamsql
join-tests-outer.yamsql                        join-tests.yamsql
join-with-order-by-tests.yamsql                like.yamsql
null-extraction-tests.yamsql                   null-operator-tests.yamsql
pseudo-field-clash.yamsql                      recursive-cte.yamsql
serialization-options.yamsql                   sparse-index-tests.yamsql
sql-functions.yamsql                           standard-tests.yamsql
union.yamsql                                   versions-tests.yamsql
views.yamsql                                   include-block/shouldPass/simple-include-different-env.yamsql
documentation-queries/aggregate-functions-documentation-queries.yamsql
documentation-queries/between-operator-queries.yamsql
documentation-queries/case-documentation-queries.yamsql
documentation-queries/group-by-documentation-queries.yamsql
documentation-queries/in-operator-queries.yamsql
documentation-queries/index-documentation-queries.yamsql
documentation-queries/is-distinct-from-operator-queries.yamsql
documentation-queries/like-operator-queries.yamsql
documentation-queries/order-by-documentation-queries.yamsql
documentation-queries/udf-documentation-queries.yamsql
```

This list does not exist anywhere in the repo today: the class is assigned at
runtime off the engine's own error string
(`javacorpus/runner.go:378-381`), `gaps.go` carries no entry for it, and
per-file identity is pinned only as the opaque
`pinnedAssignmentDigest` (`pinned_ledger_test.go:71`). Recovering it required
the run above. **It is recorded here so the transition in §8 is checkable
against a fixed starting set rather than against a number.**

### 1.2 Which rejection each file hits

Instrument: the `SKIP` detail column of the same run.

| Go rejection | files | site |
|---|---|---|
| `select element must contain an aggregate function (SUM, COUNT, MIN, MAX)` | 29 | `ddl.go:566-567` |
| `exactly one aggregate expression required in SELECT` | 12 | `ddl.go:453` |
| `select element must be an expression` | 1 | `ddl.go:561-562` |

The middle row is the refutation. `composite-aggregates.yamsql` dies on
`aggregate index "T1_I2": exactly one aggregate expression required in SELECT`
for `create index t1_i2 as select col2, sum(col1) from t1 group by col2` — a
perfectly ordinary aggregate index whose grouping column is spelled in the
projection. Java handles it at
`MaterializedViewIndexGenerator.java:249-299` (`collectResultValues` reconciles
the projection against `GroupByExpression.getGroupingValue()`). Go's
`parseAggregateIndexDefinition` cannot, because it matches ANTLR shapes and
requires exactly one select element (`ddl.go:445-454`).

The last row is `include-block/shouldPass/simple-include-different-env.yamsql`,
whose `create index i1 as select * from t1 where col1 > 1000 order by id`
presents a `*` where Go's matcher expects a `SelectExpressionElementContext`.

### 1.3 The shape surface is wider than the ledger class

Instrument: `grep -rhoiE 'create +(unique +)?index .* as +select .*'` over
`third_party/**/*.yamsql`, minus comment lines, split on
`(count|sum|avg|max|min|min_ever|max_ever|bitmap_construct_agg)\s*\(|group\s+by`.

```
statements total : 277 single-line (+1 line-wrapped, arrays-unnesting.yamsql:36) = 278
  non-aggregate  : 195 (+1) = 196
  aggregate      : 82
files with any   : 60
```

**57 files carry at least one non-aggregate AS-SELECT index; only 42 are booked
to this class.** The 15-file gap is not a discrepancy in the ledger — it is
statement order. `classifyDDLGap` keys on the *first failing* statement
(`runner.go:374-384`), so a file whose `create type as struct` precedes its
index is claimed by `unsupported-DDL:struct` instead. Eight files are in that
position by line order (`create-drop-create-template`, `in-predicate`,
`inserts-updates-deletes`, `nested-with-nulls`, `select-a-star`,
`subquery-tests`, `user-defined-macro-function-tests`, `valid-identifiers`);
the rest are claimed by file-level directive classes. **Consequence for §8:
closing CQ-71 must not be scored on "42 files pass". It must be scored on "the
class reaches 0 and every file's new destination is named".**

### 1.4 What Go does today

`parseIndexDefinition` (`ddl.go:204-245`) type-switches the ANTLR alternative
and routes **every** `IndexAsSelectDefinitionContext` to
`parseAggregateIndexDefinition` at `ddl.go:237-238`, with no inspection of the
SELECT. That routing point has no counterpart in Java (§2.1). Everything the
AS-SELECT grammar can carry beyond one aggregate over one table is dropped or
refused:

| clause | Go behaviour | site |
|---|---|---|
| `ORDER BY` | **silently ignored** — the only mention in `ddl.go` is a comment | `ddl.go:457` |
| `WHERE` | **silently dropped** — only `fc.TableSources()` is read | `ddl.go:405-419` |
| `UNIQUE` | **silently dropped** — `def.UNIQUE()` is never called | `ddl.go:391-493` |
| `WITH ATTRIBUTES LEGACY_EXTREMUM_EVER` | silently dropped | `ddl.go:391-493` |
| multi-element projection | rejected | `ddl.go:453` |
| non-aggregate projection | rejected | `ddl.go:566-567` |
| JOIN / unnest in FROM | rejected | `ddl.go:428-431`, `:432-436` |

Three of those are silent, and two of the silent three are on the wire: a
dropped `UNIQUE` produces a non-unique index where Java produces a unique one,
and a dropped `WHERE` produces a full index where Java produces a filtered one.
Both are the corrupt-a-shared-cluster direction — Go writes entries Java would
not write, and Java's uniqueness check does not exist on the Go side.

The sibling defect on the ON-source form is the same class: `indexColumnSpec`'s
`orderClause` is parsed by the grammar (`RelationalParser.g4:182-184`) and
discarded at `ddl.go:227-229`, so `CREATE INDEX i ON t(a DESC)` builds an
**ascending** index. `documentation-queries/index-documentation-queries.yamsql`
exercises exactly that (`idx_on_category_desc`, `idx_on_rating_nulls_last`,
`idx_on_supplier_desc_nulls_first`, `idx_on_complex`).

---

## 2. Java is the spec

### 2.1 Dispatch: there is only one generator, and the split is internal

`DdlVisitor.visitIndexAsSelectDefinition` (`DdlVisitor.java:205-219`) does four
things and no shape analysis:

```java
208  final var ddlCatalog = metadataBuilder.build();
210  getDelegate().replaceSchemaTemplate(ddlCatalog);
211  final var viewPlan = ... indexDefinitionContext.queryTerm().accept(this) ... .getRangesOver().get();
216  final var generator = MaterializedViewIndexGenerator.from(viewPlan, useLegacyBasedExtremumEver);
217  Assert.thatUnchecked(viewPlan instanceof LogicalSortExpression, ErrorCode.INVALID_COLUMN_REFERENCE,
         "Cannot create index and order by an expression that is not present in the projection list");
218  return generator.generate(metadataBuilder, indexId.getName(), isUnique, containsNullableArray, false).build();
```

The index's SELECT is planned by the **ordinary query visitor** against the
metadata built so far. The aggregate/non-aggregate decision happens inside the
generator, at `MaterializedViewIndexGenerator.java:187`:

```java
187  if (aggregateValues.isEmpty()) {          // VALUE / VERSION arm
...
204  } else {                                  // aggregate arm
```

`OnSourceIndexGenerator.generate` (`OnSourceIndexGenerator.java:196-227`)
reaches the same call: key columns ++ INCLUDE columns become one projection
(`:196-203`), key columns become `OrderByExpression`s carrying their per-column
`DESC`/`NULLS` (`:205-209`), `LogicalOperator.generateSort` wraps the result
(`:225`), and `:226-227` calls `MaterializedViewIndexGenerator.generate` with
`generateKeyValueExpressionWithEmptyKey` = the vector flag. **Both index forms
in Java produce their key expression through one function.**

The `viewPlan instanceof LogicalSortExpression` assert at `DdlVisitor.java:217`
is not a redundancy: `LogicalOperator.generateSelect` returns a sort at the top
when the ORDER BY list is a subset of the projection (`LogicalOperator.java:385`,
`:392` — including the empty case, via `LogicalSortExpression.unsorted` at
`:558`), but wraps the sort in **another select** when it is not
(`:393-400`). So a top-level non-sort is precisely the "ORDER BY references
something outside the projection" case, and the assert converts it to
`INVALID_COLUMN_REFERENCE`. `IndexTest.java:710-716` pins it.

### 2.2 The value/version arm, line by line

`MaterializedViewIndexGenerator.java:174-203`:

| step | line | rule |
|---|---|---|
| flatten the result value | `:174` → `:249` → `:330-336` | `Values.deconstructRecord`, then `dereference` (`:746-775`) walks each value through the quantifier map built by `collectQuantifiers` (`:720-743`), then `simplify` |
| reject non-indexable aggregates | `:176-178` | `StreamableAggregateValue` that is not `IndexableAggregateValue` |
| accepted value kinds | `:180` | `FieldValue`, `IndexableAggregateValue`, `ArithmeticValue`, `CardinalityValue` |
| partition | `:181-183` | `aggregateValues`, `fieldValues`, `versionValues` (a `FieldValue` whose type is `PseudoField.ROW_VERSION`'s) |
| at most one version column | `:184` | `UNSUPPORTED_OPERATION` |
| ordering | `:186` → `:339-385` | from `LogicalSortExpression.getOrdering().getOrderingParts()` |
| **index type** | `:188` | `versionValues.isEmpty() ? VALUE : VERSION` |
| ORDER BY ⊆ projection kinds | `:189` | "order by must be a subset of projection list" |
| **multi-column needs ORDER BY** | `:190-192` | "value indexes must have an order by clause at the top level" |
| **reorder** | `:193` → `:387-394` | ORDER BY values first, in ORDER BY order; then the remaining projection values in projection order |
| build | `:194` → `:497-524` | see §2.4 |
| **split point** | `:195-198` | `splitPoint = orderByValues.size()`; if ORDER BY is empty **and** `generateKeyValueExpressionWithEmptyKey` is false (which it is for both DDL forms — `DdlVisitor.java:218`, `OnSourceIndexGenerator.java:227` for non-vector), `splitPoint = -1` |
| **covering** | `:199-203` | `keyWithValue(expr, splitPoint)` iff `splitPoint != -1 && splitPoint < fieldValues.size()`; else the bare expression. Both go through `NullableArrayUtils.wrapArray` |

`reorderValues` (`:387-394`) asserts `values.size() >= orderByValues.size()`,
returns `values` unchanged when ORDER BY is empty, and otherwise returns
`orderBy ++ (values \ orderBy)` — **the ORDER BY fixes the key order, the
projection order only fixes the tail.** `IndexTest.java:673-681`
(`SELECT a1, a2 ... ORDER BY a2, a1` → `concat(A2, A1)`) is the pin.

### 2.3 Ordering functions: an exact four-way map

`ParseHelpers.isDescending`/`isNullsLast` (`ParseHelpers.java:167-179`) and
`OrderByExpression.toSortOrder` (`OrderByExpression.java:108-115`) produce a
`RequestedSortOrder`; the generator maps it to a function name at
`MaterializedViewIndexGenerator.java:347-363`:

| SQL | descending | nullsLast | RequestedSortOrder | key-expression wrapper |
|---|---|---|---|---|
| *(absent)* / `ASC` / `ASC NULLS FIRST` | false | false | `ASCENDING` | **none** |
| `ASC NULLS LAST` | false | true | `ASCENDING_NULLS_LAST` | `function("order_asc_nulls_last", …)` |
| `DESC` / `DESC NULLS LAST` | true | true | `DESCENDING` | `function("order_desc_nulls_last", …)` |
| `DESC NULLS FIRST` | true | false | `DESCENDING_NULLS_FIRST` | `function("order_desc_nulls_first", …)` |

`OnSourceIndexGenerator.IndexedColumn.parseColSpec` (`:280-299`) reproduces the
same defaults for the ON-source form, and `parseUid` (`:302-306`) gives INCLUDE
columns `(false, false)` — irrelevant, since they land past the split point.

The wrapper is a real wire artefact, not decoration:
`OrderFunctionKeyExpression` packs the tuple through `TupleOrdering` with the
direction (`OrderFunctionKeyExpressionFactory.java:44-48`), so a DESC column's
index entry bytes differ from an ASC column's.

### 2.4 Building the expression

`generate(List<Value>, orderingFunctions)` (`:497-524`):

- empty → `EmptyKeyExpression.EMPTY`; single → `toKeyExpression(value, …)`.
- otherwise the value list is consumed by a **peeking iterator**: a run of
  consecutive `FieldValue`s is compressed into one `FieldValueTrieNode`
  (`FieldValueTrieNode.computeTrieForValues`,
  `FieldValueTrieNode.java:201-238`), each new trie is checked against the
  previously built ones (`validateNoOverlaps`, `:136-153`), and non-field values
  become their own component. Components are `concat`-enated.
- The trie is what turns `SELECT r.s.a, r.s.b` into
  `field("R").nest(field("S").nest(concat(field("A"), field("B"))))` rather than
  two separate nestings (`IndexTest.java:819-830`), and what produces
  `Index with multiple disconnected references to the same column are not
  supported` when the same parent is reached twice non-adjacently
  (`IndexTest.java:745-752`, `:763-771`).

Leaf construction, `toKeyExpression(Value)` (`:551-582`) and
`toFieldKeyExpression` (`:810-827`):

- `FieldValue` → nested `field(storageName, fanType)`; `fanType` is `FanOut`
  for arrays, `None` otherwise; a `ROW_VERSION`-typed, `__ROW_VERSION`-named
  field becomes `VersionKeyExpression.VERSION` (`:821-823`).
- An array field reached without an unnest and without `Concatenate` is
  rejected: `cannot create index on array field '…' without unnesting`
  (`:814-819`).
- `CardinalityValue` → `function("cardinality", field(path, Concatenate))`
  (`:555-566`).
- `ArithmeticValue` → `function(<logical operator name, lowercased>,
  concat(args))` (`:567-575`) — `add`, `sub`, `mul`, `div`, `mod`, `bitand`,
  `bitor`, `bitxor` (`IndexTest.java:597-613`).
- `LiteralValue` → `Key.Expressions.value(literal)` (`:576-577`).
- **The ordering wrapper is applied at the leaf**, not at the root — including
  inside a trie, at the trie's leaf node (`:598-600`).

### 2.5 Predicate

`getTopLevelPredicate` (`:639-681`) walks the topologically-ordered expression
list, skips a leading sort, tolerates a select-having/group-by/select-where
sandwich, asserts that no *inner* select carries predicates, converts each
predicate with `toResidualPredicate`, ANDs them, normalises to DNF with
`BooleanPredicateNormalizer`, and requires `IndexPredicate.isSupported`
(`IndexPredicate.java:212-232`: constant / not / and / or / value-predicate over
a `FieldValue` with a fully-named path, comparison in `IndexComparison`).
Result → `indexBuilder.setPredicate(IndexPredicate.fromQueryPredicate(p).toProto())`
(`:171`).

### 2.6 Nullable arrays

`NullableArrayUtils.wrapArray` (`NullableArrayUtils.java:79-104`) is applied to
the **serialized `RecordKeyExpressionProto.KeyExpression`**, not to the
expression objects, and it is **descriptor-driven**: `wrapArrayInternal`
(`:106-192`) rewrites a `field(X)` or `nesting(parent=X, …)` only when
`isWrappedArrayDescriptor(X, parentDescriptor)` holds — i.e. only when the
generated protobuf descriptor for `X` really is `message { repeated R values = 1; }`
(`:62-66`). The `containsNullableArray` boolean (`DdlVisitor.java:162`) is a
fast-path gate in front of that check, not the check.

### 2.7 The wire artefact

`RecordMetadataSerializer.visit(Index)`
(`RecordMetadataSerializer.java:75-89`) puts the generated expression into
`new Index(name, keyExpression, indexType, options, predicate)`, which
serializes to `RecordMetaDataProto.Index.root_expression`
(`record_metadata.proto:160`) plus `type` (`:165`), `options` (`:166`) and
`predicate` (`:172`). **`root_expression` is the byte-level acceptance
target of this RFC.**

---

## 3. Branch → corpus coverage

Flags over the 196 non-aggregate statements (instrument: §1.3's sweep, plus the
enumeration in the shape table below):

| flag | count |
|---|---|
| `ORDER BY` present | 135 |
| `ORDER BY` absent | 61 |
| explicit `ASC`/`DESC`/`NULLS` | 16 |
| `WHERE` | 17 |
| `UNIQUE` | 2 |
| `"__ROW_VERSION"` in the projection | 14 |
| multi-column projection | 78 |
| `ORDER BY` a strict subset of the projection (covering) | 14 |
| `ORDER BY` a permutation of the projection | 5 |
| multi-source `FROM` (unnest / derived table) | 9 |
| function or arithmetic in the projection | 9 (3 `CARDINALITY`, 2 `+`, 2 `&`, 2 `bitmap_bucket_offset`) |
| `SELECT *` | 3 |
| `INCLUDE(...)` on the AS-SELECT form | **0** |

### 3.1 Branches the corpus REACHES

| generator branch | Java site | corpus witness |
|---|---|---|
| single field, no ORDER BY, `splitPoint = -1` | `:196-198` | `standard-tests.yamsql` `create index i1 as select col1 from t1` |
| single field + ORDER BY, `splitPoint == size` → bare expression | `:199-203` | `documentation-queries/in-operator-queries.yamsql:4` `price_idx` |
| multi-field, ORDER BY == projection | `:193` | `sql-functions.yamsql` `t1_idx1 ... order by col2, col1, col3` |
| **`reorderValues` permutation** | `:387-394` | `orderby.yamsql:34` `select d, c, b, a … order by a, b, c, d` |
| **`keyWithValue` covering split** | `:199-200` | `index-ddl-values-only.yamsql:47` `select category, price, name, stock … order by category, price` |
| multi-field without ORDER BY → **error** | `:190-192` | `index-ddl-values-only.yamsql` (negative expectations) |
| `order_desc_nulls_last` | `:352` | `documentation-queries/index-documentation-queries.yamsql` `idx_price_desc` |
| `order_asc_nulls_last` | `:355` | `index-ddl.yamsql` `idx_mv_rating_nulls_last` |
| `order_desc_nulls_first` | `:358` | `index-ddl.yamsql` `idx_mv_desc_nulls_first` |
| ASCENDING → **no** wrapper | `:348-350` | `index-ddl.yamsql` `idx_mv_nulls_first` (`asc nulls first`) |
| mixed per-column direction | `:345-381` | `index-ddl.yamsql` `idx_mv_asc_desc` |
| `VERSION` index type | `:188` | `versions-tests.yamsql` `t1_version_index` |
| version mixed into a covering key | `:199-200` + `:188` | `pseudo-field-clash.yamsql` `t2_col1_version`; `versions-tests.yamsql` `t3_version_with_col1` |
| nested `FieldValue` trie | `:504-517` | `orderby.yamsql:41` (6-level path); `user-defined-macro-function-tests.yamsql` `deep_idx` |
| unnest / `ExplodeExpression` quantifier | `:725-743` | `in-predicate.yamsql:39` `from array_table as t, t.fruits as "fruit"` |
| two-level unnest | `:725-743` | `valid-identifiers.yamsql:36` |
| derived table over a repeated field | `:725-743` | `subquery-tests.yamsql:29`; `arrays-unnesting.yamsql:36` (the only line-wrapped index DDL in the corpus) |
| `CardinalityValue`, non-nullable array | `:555-566` | `arrays-cardinality.yamsql:32` |
| `CardinalityValue`, nested | `:555-566` | `arrays-cardinality.yamsql:38` |
| `ArithmeticValue` (`+`, `&`) | `:567-575` | `indexed-functions.yamsql:24-25` |
| `bitmap_bucket_offset` as a plain projection | `:567-575` | `bitmap-aggregate-index.yamsql:28` `agg_index_2` |
| predicate, simple comparison | `:169-172` | `sparse-index-tests.yamsql`; `index-ddl-values-only.yamsql:58` |
| predicate, `OR` → DNF | `:675` | `sparse-index-tests.yamsql:27` (`= true or … is not null`) |
| predicate, boolean constant | `:169-172` | `boolean-ddl.yamsql:30` (`WHERE NULL`) |
| `SELECT *` projection | `:174` | `include-block/shouldPass/simple-include-different-env.yamsql:26` |
| `UNIQUE` | `DdlVisitor.java:215` | `join-with-order-by-tests.yamsql:52` |
| aggregate + projected grouping columns | `:249-299` | `composite-aggregates.yamsql`; `index-ddl-aggregates-only.yamsql:48` |
| `PERMUTED_MIN`/`MAX` permuted size from ORDER BY | `:238-240` | `aggregate-index-tests.yamsql` `mv18`, `mv19`, `mv20` |
| bitmap aggregate | `:413-432` | `bitmap-aggregate-index.yamsql:24` |
| `LEGACY_EXTREMUM_EVER` | `:449-465` | `aggregate-index-tests.yamsql` `mv12` |

### 3.2 Branches the corpus does NOT reach

These still get ported 1:1 — the difference is only where their tests come from:
they are covered by **ported Java unit tests** (`IndexTest.java`) rather than by
a corpus file.

| unreached branch | Java site | ported test |
|---|---|---|
| `LiteralValue` leaf (`SELECT 5+1`) | `:576-577` | `IndexTest.java:522-530` |
| the nullable-array wrapper actually firing | `NullableArrayUtils.java:106-192` | `IndexTest.java:541-549`, `:727-734` |
| `validateNoOverlaps` rejection | `FieldValueTrieNode.java:136-153` | `IndexTest.java:745-752`, `:763-771`, `:927-935` |
| array field without unnesting → error | `:814-819` | (Java has no direct unit test; the RFC adds one) |
| more than one version column → error | `:184` | (added) |
| ORDER BY outside the projection → `INVALID_COLUMN_REFERENCE` | `DdlVisitor.java:217` | `IndexTest.java:710-716` |
| more than one type filter (join) → error | `:611-615` | `IndexTest.java:500-520` |
| predicate inside an inner select → error | `:659-664` | (added) |
| `IndexPredicate.isSupported` rejection | `:676` | (added) |
| `SplitKeyExpression` / `ListKeyExpression` in `wrapArray` | `NullableArrayUtils.java:165-189` | round-trip test only |

---

## 4. Decision

### D1 — One generator, over the logical plan. `parseAggregateIndexDefinition` is deleted.

A new package `pkg/relational/core/query/ddl` (mirroring Java's
`recordlayer/query/ddl`) holds `MaterializedViewIndexGenerator`, a 1:1 port
consuming a `logical.LogicalOperator` and producing an index description
(root key expression + index type + options + predicate). It is the **only**
producer of index metadata from DDL. `parseAggregateIndexDefinition`
(`ddl.go:388-493`), `extractAggregateFromSelectElement` (`:558-623`),
`extractCardinalityColumnFromSelectElement` (`:502-531`) and the aggregate/
cardinality/fan-out arms of `metadata.Builder.Build` (`builder.go:410-456`)
are removed with it.

Import direction: `embedded` → `query/ddl` → {`query/logical`, `core/metadata`,
`recordlayer`}. Verified acyclic — neither `core/metadata` nor `query/logical`
imports `embedded`.

*Why this and not a second arm next to the existing one.* Three reasons, in
order of weight. (a) It is what Java does: the aggregate/non-aggregate split is
at `MaterializedViewIndexGenerator.java:187`, inside one function, and both DDL
forms reach it. (b) The measurement in §1.2 shows a non-aggregate arm alone
leaves 12 of the 42 files in the class. (c) The current arm is an ANTLR-shape
matcher; keeping it is exactly the "no parallel pipelines" failure CLAUDE.md
names — two index constructors that must agree forever and have no mechanism
forcing them to.

### D2 — Both DDL forms funnel into it, and INCLUDE stops failing closed.

`parseIndexDefinition` (`ddl.go:204-245`) becomes a front end:

- `IndexAsSelectDefinitionContext` → plan the `queryTerm` (D3) → generator.
- `IndexOnSourceDefinitionContext` → synthesize the projection
  `keyColumns ++ includeColumns` and the ORDER BY list from the key columns'
  `orderClause` (defaults per §2.3) → the same logical shape → generator.
  This is `OnSourceIndexGenerator.java:196-227` ported. It closes the dropped
  `orderClause` (`ddl.go:227-229`) and the `INCLUDE` rejection
  (`ddl.go:221-224`) as a **consequence**, not as extra scope.
- `VectorIndexDefinitionContext` keeps its own path (§9).

### D3 — The logical plan comes from the existing plan visitor.

`NewPlanVisitorWithSchema(md, schema).VisitQuery(q)`
(`pkg/relational/core/embedded/plan_visitor.go:143-156`) already turns an ANTLR
query context into a `logical.LogicalOperator` given a `*recordlayer.RecordMetaData`.
`buildSchemaTemplateFromDDL` (`cascades_generator.go:5564-5621`) already runs
tables first and indexes second, so at index time a tables-only
`metadata.Builder.Build()` yields the `RecordMetaData` to plan against. This is
`DdlVisitor.java:208-212` verbatim. **No second SQL front end is written.**

Operator correspondence:

| Java | Go |
|---|---|
| `FullUnorderedScanExpression` + `LogicalTypeFilterExpression` | `logical.LogicalScan` (`logical/operators.go:81`) |
| `ExplodeExpression` quantifier | `logical.LogicalUnnest` (`:112`) |
| `SelectExpression` predicates | `logical.LogicalFilter` (`:169`) |
| `SelectExpression` result values | `logical.LogicalProject` (`:217`) |
| `LogicalSortExpression` | `logical.LogicalSort` (`:415`), keys `SortKey{Dir, NullsFirst, Value, Pos}` (`:376-388`) |
| `GroupByExpression` | `logical.LogicalAggregate` (`:522`) |

Go's visitor does not synthesize `LogicalSortExpression.unsorted` for a query
with no ORDER BY, so Java's "top is a sort" test becomes, in Go: **the ORDER BY
list is empty when the top operator is not a `LogicalSort`, and it is an
`INVALID_COLUMN_REFERENCE` error when a `LogicalSort` appears *below* the
top-level `LogicalProject`** — that lower position is Go's spelling of Java's
`generateSelect` wrapping at `LogicalOperator.java:393-400`. The error message
is Java's, verbatim: `Cannot create index and order by an expression that is
not present in the projection list`.

### D4 — The key order, the split point, and the wrapper placement are Java's, verbatim.

`reorderValues` = ORDER BY values in ORDER BY order, then the remaining
projection values in projection order. `splitPoint = len(orderBy)`, forced to
`-1` when the ORDER BY list is empty (`generateKeyValueExpressionWithEmptyKey`
is false for both DDL forms). `KeyWithValue` iff
`splitPoint != -1 && splitPoint < len(fieldValues)`. Ordering wrappers are
applied at the **leaf**, including inside a trie node. Every one of those has a
golden in §8 gate (a).

### D5 — `__ROW_VERSION` becomes a projectable pseudo-column.

The `VALUE`-vs-`VERSION` decision (`:188`) is driven by a projected field whose
type is `PseudoField.ROW_VERSION`'s
(`PseudoField.java:36-40`: name `"__" + ROW_VERSION`, type
`Type.primitiveType(VERSION, true)`), and the leaf becomes
`VersionKeyExpression.VERSION` (`:821-823`). Go cannot resolve the identifier at
all today — `gaps.go:54` books `column "__ROW_VERSION" does not exist` as
`engine-gap:row-version-pseudocolumn`. 14 corpus statements need it, across
`versions-tests.yamsql`, `pseudo-field-clash.yamsql` and
`join-tests-row-version.yamsql`. It is **in scope**: an index generator whose
VERSION arm cannot be reached is the "rule ported but can't fire" non-done.

### D6 — The predicate arm is built, not stubbed.

Go already consumes index predicates (`pkg/recordlayer/index_predicate.go`,
`Index.SetPredicateProto` at `pkg/recordlayer/index.go:492-507`) and
round-trips them through `RecordMetaDataProto.Index.predicate`. The producer is
new: `LogicalFilter` predicate → `gen.Predicate`, via the Java pipeline
(residual form → AND → DNF normalisation → `isSupported` gate → proto).
17 corpus statements, including an `OR` that must normalise
(`sparse-index-tests.yamsql:27`) and a bare boolean constant
(`boolean-ddl.yamsql:30`).

### D7 — `wrapArray` is ported and driven off the real descriptor; its no-op today is PINNED.

Port `NullableArrayUtils.wrapArray`/`wrapArrayInternal` over the proto form,
gated on the same DDL-derived flag Java computes (`DdlVisitor.java:162`: any
nullable `ARRAY` column). Because the rewrite is per-node and conditioned on
`isWrappedArrayDescriptor` against the **actual** built descriptor
(`NullableArrayUtils.java:123`, `:151`), this is byte-identical to Java for any
descriptor Java would produce, and a no-op under Go's current descriptors —
Go writes plain repeated fields, not the wrapper (the RFC-143 §3a divergence,
stated at `pkg/relational/core/metadata/builder.go:539-542` and `:813-819`).

That no-op is a **negative result and it gets pinned**: a test asserts that
Go's generated descriptor for a nullable `ARRAY` column has no `values`
wrapper, with a failure message naming what re-arms — the moment RFC-143 §3a
lands, the wrap becomes live and the golden key expressions for
`arrays-cardinality.yamsql` and the nested-repeated shapes change. Without the
pin, that change would be silent and wire-visible.

### D8 — `metadata.Builder` learns to carry an explicit index.

`indexSpec` (`builder.go:43-68`) has no field for a key expression or an index
type; `Build()` re-derives the shape from column names
(`buildIndexKeyExpression`, `:709-722`). That is the wrong layering once the
generator exists. `indexSpec` gains `rootExpression recordlayer.KeyExpression`,
`indexType string`, `predicate *gen.Predicate`, `options map[string]string`,
and `Build()` short-circuits to `recordlayer.NewIndex(name, rootExpression)`
+ type/options/predicate/unique when they are set. `AddIndex(table, name, cols,
unique)` stays for the primary-key/simple paths that do not go through DDL.

### D9 — The planner learns the two shapes the generator will now emit. Both are live defects today.

Nothing about VALUE-index enumeration is DDL-specific: `buildMatchCandidates`
(`cascades_generator.go:2064-2168`) walks `md.GetAllIndexes()` generically. But
two shapes this RFC starts emitting are mishandled:

**(a) `KeyWithValue` roots — a wrong-column-set bug, currently unreachable.**
`indexKeyColumnNames` (`cascades_generator.go:2265-2295`) handles
`KeyWithValue` by recursing into the inner key and **discarding the split
point** (`:2291-2292`), so every value-part column is reported as a *key*
column. It is unreachable today only because the sole `KeyWithValue` producer
is the vector path, which `tryVectorIndexCandidate` (`:2141-2144`) claims
first, and because DDL rejects `INCLUDE`. The covering shape (14 corpus
statements) makes it reachable. Separately,
`keyExpressionFlatColumnDescriptors` (`index_expansion.go:773-822`) has no
`KeyWithValue` case and returns false at `:820`, so the candidate would decline
rather than mis-plan — which is safe but is not "planned". Both are fixed here:
the split point becomes the key/value boundary, key columns bound the scan
prefix, value columns are available for covering.

**The unreachability is pinned as a negative result** before it is fixed — a
test that builds a `KeyWithValue`-rooted non-vector VALUE index by hand and
asserts today's decline, so that the fix has a red-to-green witness rather than
an assertion that something already worked.

**(b) order-function-wrapped columns.** Only `"cardinality"` is accepted as a
function-keyed column (`index_expansion.go:801-806`;
`match_candidate_index.go:716-720`), and `indexColumnFunctionTags`
(`cascades_generator.go:2237-2263`) tags a generic `FunctionKeyExpression` as
`""`, i.e. reports it as a plain field. 16 corpus statements carry explicit
`ASC`/`DESC`/`NULLS`, and the ON-source form adds more once D2 stops dropping
`orderClause`. The four `order_*` functions are already registered in the
record layer (`pkg/recordlayer/order_function_key_expression.go:18-21`) and
evaluate through `TupleOrdering` (`pkg/recordlayer/tuple_ordering.go:46`), so
this is a planner-side match/ordering change, not new machinery.

D9 is why this RFC needs a Graefe ACK.

### D10 — The cross-engine key-expression check is built. It is buildable today.

Measured feasibility: the Java conformance server already exposes
`createSchemaTemplatePersistentJava(clusterFile, templateName,
schemaTemplateBody)` (`conformance/sql_plan_steps.java:332-346`), which
executes arbitrary `CREATE SCHEMA TEMPLATE … CREATE INDEX … AS SELECT …` DDL
and **persists** the resulting `RecordMetaDataProto.MetaData` into the shared
FDB catalog (companion `dropSchemaTemplatePersistentJava`, `:352-373`). Go reads
those exact bytes back through
`catalog.OpenRecordLayerStoreCatalog()` →
`SchemaTemplateCatalog().ListTemplates(tx)`
(`pkg/relational/core/catalog/fdb_template_catalog.go:167-192`), whose
`META_DATA` column is the stored proto. The normalisation problem is already
solved: `clearProto2Defaults` / `normalizeKeyExprJSON`
(`conformance/metadata_proto_conformance_test.go:71-145`) exist because Java
materialises proto2 defaults Go leaves unset, and `compareMDIndexes` (`:422-435`)
already compares `RootExpression` index by index. The only existing DDL-driven
cross-engine test, `conformance/schema_template_catalog_conformance_test.go:41-121`,
does steps 1 and 2 and then asserts only the table count — it stops one
assertion short.

Decision: extend that file. Java persists the template, Go persists the same
DDL through its own driver, both `MetaData` protos are read back **as bytes**
(not through Go's object model, so a Go deserialisation bug cannot mask a
divergence), and the per-index `root_expression`, `type`, `options` and
`predicate` are compared after normalisation. The suite is already in the
primary merge gate (`.github/workflows/ci.yml:63`, tag `conformance_java`).

*Rejected within D10:* adding a new `@ConformanceStep` that returns metadata
bytes over HTTP. It is ~15 lines and reuses `runWithEphemeralSchema`
(`sql_plan_steps.java:388-453`), but the catalog route needs no Java change at
all and additionally proves the **stored** bytes match, which is the property
that actually matters for a shared cluster.

---

## 5. Rejected alternatives

**Keep `parseAggregateIndexDefinition`, add a parallel value-index arm.**
Smallest diff, and wrong on all three axes: it leaves 12 of the 42 files
blocked (§1.2), it entrenches a second index constructor with no Java
counterpart, and it makes the ON-source convergence (D2) impossible without a
third one. Rejected on "long-term correct is the only selection criterion".

**Build the key expression from the ANTLR tree directly, without the logical
plan.** Tempting because the simple shapes (`select a from t order by a`) are
syntactically obvious. It fails at exactly the shapes the corpus is full of:
unnest and derived-table `FROM` (9 statements) need quantifier dereferencing
(`MaterializedViewIndexGenerator.java:720-775`); `SELECT *` (3) needs
expansion; the `ORDER BY ⊆ projection` rule needs pulled-up expression
identity, not text. Java went through the plan for these reasons; a text- or
tree-shape-driven Go version would diverge on the first nested case and is
forbidden by "no text matching on SQL / parse trees".

**Derive the split point from the projection instead of the ORDER BY.**
Reversed. `IndexTest.java:673-681` and `orderby.yamsql:34` both show the ORDER
BY fixing the key order; `IndexTest.java:683-692` shows the split at
`len(orderBy)` with the projection tail becoming the value.

**Emit `VALUE` for version columns and carry the version separately.** Java
switches the whole index type at `:188` and the leaf becomes
`VersionKeyExpression`. Anything else writes different index entries.

**Skip `wrapArray` because Go does not write wrappers.** It would be correct
today and silently wrong the moment RFC-143 §3a lands, and it would make Go
unable to reproduce a Java-authored key expression. D7 ports it and pins the
current no-op instead.

**Defer the planner work (D9) to a follow-up.** An index that is created
correctly and never chosen is the "infrastructure exists but SQL can't trigger
it" non-done. Worse, D9(a) is a live latent wrong-column-set defect that this
RFC's own output makes reachable; shipping the generator without it would be
arming a known bug.

**Defer `__ROW_VERSION` (D5).** Same reason: it makes the VERSION arm dead code
and leaves 14 corpus statements blocked behind an already-booked gap.

---

## 6. Sequencing

Each step ends green and is committed on its own.

- **S1 — `metadata.Builder` carries an explicit index** (D8). No behaviour
  change: the existing derivation paths keep working, the new fields are unset.
- **S2 — the generator, value/version arm** (D1, D3, D4), driven from the
  AS-SELECT form only. Ported unit tests from `IndexTest.java` for every shape
  in §3.1 and §3.2 that does not need D5/D6.
- **S3 — the aggregate arm** (D1) — `collectResultValues`,
  `generateAggregateIndexKeyExpression`, permuted size, bitmap, legacy
  extremum. `parseAggregateIndexDefinition` is deleted at the end of this step,
  not before.
- **S4 — `__ROW_VERSION`** (D5), then the VERSION arm's goldens.
- **S5 — the predicate arm** (D6).
- **S6 — the ON-source front end** (D2): `orderClause` honoured, `INCLUDE`
  accepted, both routed through the generator.
- **S7 — planner** (D9): the negative-result pins first, then `KeyWithValue`
  key/value boundary, then order-function columns.
- **S8 — the cross-engine key-expression conformance test** (D10).
- **S9 — ledger transition** (§8 gate (c)), in one commit with the updated
  `pinnedLedger`, `pinnedAssignmentDigest`, and the per-file destination table.

Anything S2–S8 surfaces that is a Go-engine divergence is root-caused and fixed
in the same step with a regression pin, per the standing fix-now rule — not
appended to CQ-72.

---

## 7. Instruments

- **Key-expression goldens**: table-driven, DDL in → normalised
  `gen.KeyExpression` proto out, one row per shape in §3.1/§3.2, with the
  expected value written as the Java `IndexTest.java` expression it was ported
  from and its line cited.
- **Index-entry differential**: for the shapes whose entry bytes depend on the
  expression (DESC via `TupleOrdering`, fan-out, `KeyWithValue` split,
  `VERSION`), sample rows written through the Go store and the entry keys/values
  compared against the Java-built store over the existing conformance machinery.
- **Cross-engine metadata**: D10.
- **Ledger census**: the corpus run of §1.1, re-run at S9.
- **Plan-shape assertions**: `EXPLAIN` on the newly-passing corpus files (§8
  gate (d)).

---

## 8. Acceptance gates

**(a) Key-expression goldens — mutation-checked per direction.** Every shape in
§3.1 and §3.2 has a pinned normalised `root_expression`. The fix must be
mutable in each of these directions independently, and each mutation must go
RED with the failure quoted:

1. key order: use the projection order instead of `reorderValues`' ORDER BY
   order → `orderby.yamsql:34`'s golden and `IndexTest.java:673-681`'s port
   must fail;
2. split point: `len(orderBy)` → `len(fieldValues)` (i.e. never split) →
   `index-ddl-values-only.yamsql:47`'s golden must fail;
3. split suppression: drop the `orderBy.isEmpty() → -1` rule → the
   no-ORDER-BY single-column golden must fail;
4. ordering wrapper: emit `order_asc_nulls_first` for plain `ASC` → the
   `idx_mv_nulls_first` golden must fail;
5. nulls default: `DESC` → `order_desc_nulls_first` → the `idx_price_desc`
   golden must fail;
6. wrapper placement: apply the ordering function at the root instead of the
   leaf → the mixed-direction golden (`idx_mv_asc_desc`) must fail;
7. index type: emit `VALUE` for a projected `__ROW_VERSION` → the
   `t1_version_index` golden must fail;
8. trie: build one nesting per field instead of compressing runs → the
   `r.s.a, r.s.b` golden must fail;
9. fan type: `FanOut` instead of `Concatenate` under `CARDINALITY` → the
   `arrays-cardinality.yamsql:32` golden must fail;
10. predicate: drop DNF normalisation → the `sparse-index-tests.yamsql:27`
    golden must fail.

A gate that survives its own mutation is not a gate; each is re-checked until
it goes red.

**(b) Wire equality against Java.**
(b1) D10's cross-engine comparison is green for every §3.1 shape Java accepts,
comparing stored `MetaData` bytes.
(b2) Index **entry** bytes agree for sampled rows on the shapes where the
expression changes the encoding (DESC, fan-out, `KeyWithValue`, `VERSION`).
(b3) The no-op wrapper pin of D7 is present and names its re-arm condition.

**(c) The ledger transition, measured and fully accounted.**
`unsupported-DDL:value-index-as-select` reaches **0** and the class constant is
deleted from `ledger.go:106-110` and from the `allClasses` list at `:195` — a
class that can no longer be reached is dead code, and leaving it would let the
count silently repopulate. `fail=0` holds, `pinnedFileTotal=238` still closes,
and the commit carries a table mapping **each of the 42 files in §1.1** to its
new status and class. A file landing in a class that is not either `pass` or an
already-named RFC-201 phase gap is a hard failure, not a re-booking.

Floor: at least the five files whose only blocker is this one —
`documentation-queries/{between,case,in,is-distinct-from,like}-operator-queries.yamsql`
(measured: zero `explain`/`explainContains`/`planHash`/`resultMetadata`
directives across the whole `documentation-queries/` tree; their schema
templates declare nothing but tables and non-aggregate AS-SELECT indexes) —
must reach `pass`. Fewer than five means the port is incomplete, not that the
corpus is hard.

**(d) The indexes are PLANNABLE.** For each newly-passing corpus file that
declares an index the queries should use, an `EXPLAIN` assertion shows an index
scan over that index — not a full scan returning the right rows. Specifically:
a covering (`KeyWithValue`) index must produce a covering scan with no fetch,
and a `DESC` index must satisfy a `ORDER BY … DESC` query without an in-memory
sort. Mutation: revert D9(a)/D9(b) independently; each must turn its assertion
red.

**(e) No second constructor survives.** `grep` shows no remaining
`parseAggregateIndexDefinition` / `extractAggregateFromSelectElement` /
`buildIndexKeyExpression`-from-column-names path reachable from index DDL, and
`ddl.go` contains no `INCLUDE`-rejection for the non-vector forms.

**(f) `just test` green**, including the `conformance_java`-tagged suite.

---

## 9. Out of scope, stated

- **Vector indexes.** `VectorIndexDefinitionContext` keeps its own front end
  (`ddl.go:239-240`, `:253-296`). In Java it also reaches
  `MaterializedViewIndexGenerator` — but through `OnSourceIndexGenerator` with
  `generateKeyValueExpressionWithEmptyKey = true`
  (`DdlVisitor.java:278`), the one caller that flips that flag, plus HNSW option
  parsing and an `INCLUDE` rejection of its own (`DdlVisitor.java:297-298`).
  Converging it is a follow-on with its own reviewer (RFC-094's SPFresh gate),
  and it is not on the CQ-71 path: no corpus file in §1.1 declares a vector
  index.
- **`unsupported-DDL:struct` (39 files).** Several of the §1.3 gap files are
  claimed by it; it is RFC-201 Phase 3 / CQ-69.6 and is not touched here. This
  RFC's gate (c) explicitly permits a file to move from this class **into**
  `unsupported-DDL:struct`, and requires it to be named when it does.
- **`resultMetadata:` / continuation directives.** RFC-201 Phase 2.
- **The `RowNumberWindowPredicate` / `QUALIFY` sliding-window index form**
  (`IndexTest.java:424-474`). It reaches the generator only through
  `CREATE VIEW … QUALIFY … CREATE INDEX … ON v(...)`, needs view support Go does
  not have, and no corpus file in §1.1 uses it. The predicate proto arm
  (`record_metadata.proto:342`) is ported as data so metadata written by Java
  round-trips, but Go does not construct one.

---

## 10. What this does not do

It does not make Go's stored protobuf descriptors match Java's for nullable
arrays — that is RFC-143 §3a, and D7 pins the boundary rather than moving it.

It does not change how index entries are maintained or scanned at the record
layer: `KeyWithValue`, `VersionKeyExpression`, `FunctionKeyExpression` and
index predicates are all already implemented and tested there
(`pkg/recordlayer/index_maintainer.go`, `order_function_key_expression.go`,
`index_predicate.go`). Everything this RFC adds is upstream of them — the
metadata that says which of those to build.

It does not add a query capability Java lacks. Every shape it enables is a
shape Java already accepts, and every shape Java rejects it rejects with Java's
error code and Java's message.
