# RFC-202 — One index generator, over the logical plan, for both index forms

Status: **PROPOSED**, revision 2. Revision 1 was NAK'd by both reviewers with the
architecture endorsed and twelve blockers between them; this revision folds all
of them. It needs a Graefe ACK (it moves index metadata construction onto the
logical plan and changes what the planner is asked to match) and a Torvalds ACK
before implementation starts.

Closes: CQ-71. Advances RFC-201's ledger by deleting the corpus's single largest
skip class.

**What revision 1 got wrong, corrected at the sites that carry it rather than
left standing:**

1. **D3's ORDER-BY discriminator was INVERTED, and could not have been
   structural at all.** Revision 1 read Java's "sort wrapped in another select"
   as "the sort must be the top operator" and ported the POSITION. In Go,
   projection-above-sort is the pinned NORMAL layering
   (`plan_visitor.go:499-503` builds the sort at step 4, `:515-518` the
   projection at step 5; `sort_projection_layering_test.go:11` pins that the
   projection may never sink below it), so revision 1's rule rejected the
   ordinary shape — 134 of the 194 non-aggregate corpus statements. Worse, Go
   validates ORDER BY against the SOURCE scope (`plan_visitor.go:744-758`), so
   it *accepts* ORDER BY on a non-projected column — the shape Java rejects —
   and produces a structurally IDENTICAL plan. Both facts are now measured and
   committed as `pkg/relational/core/embedded/index_ddl_order_by_shape_test.go`.
   D3 is rebuilt on Java's actual mechanism: the set difference over expression
   VALUES at `LogicalOperator.java:389-390`.
2. **The headline "12 aggregate-arm rejections" was a 4× misread.** Only THREE
   are aggregate rejections. Nine are multi-column VALUE indexes hitting
   `len(allElems) != 1` at `ddl.go:451-453`, which runs *before* any aggregate
   inspection. Revision 1 inferred the cause from an error string. §1.2 now
   names each of the twelve.
3. **§1.4's triage was inverted.** `LEGACY_EXTREMUM_EVER` and the ON-source
   `orderClause` are silent accept-and-mis-build defects in live code;
   `UNIQUE`, `WHERE` and `ORDER BY` on the AS-SELECT form are measurably LATENT,
   pre-empted by the `ddl.go:566-567` rejection in the same statement. §1.4 is
   re-triaged, and the two live ones are the subject of a fail-closed hotfix
   landing on a separate branch before this RFC's S1.
4. **§1.3's mechanism was wrong.** Tables always run before indexes
   (`ddl.go:148-149` first pass, `ddl.go:172-174` second), so a struct-column
   rejection pre-empts an index rejection regardless of textual order. Revision
   1 attributed it to line order, which `valid-identifiers.yamsql` falsifies.
5. **Three §3 witnesses were mis-assigned, and eight §3 counts were wrong.**
   Every count in §3 now comes from a committed classifier
   (`pkg/relational/conformance/javayamsql/index_ddl_shape_census_test.go`)
   walking typed parse-tree nodes. Revision 1's numbers came from a grep sweep
   whose `[^\n]*` truncated every statement at the first `n` — inside an ERE
   bracket expression that reads "not backslash, not n" — turning
   `select count(*)` into `select cou` and mis-classifying six aggregate
   statements as value indexes.
6. **D10's comparison read a subspace the other side never writes**
   (`catalog.DefaultCatalogSubspace()` vs the driver's
   `keyspace.RelationalKeyspace.CatalogSubspace()`), and compared neither
   options nor predicate — which is where `UNIQUE`'s coverage lives.
7. **D9(a)'s unreachability claim was too narrow**: Java-authored covering
   indexes reach the defect TODAY through proto deserialization, and the
   fallback path has the same defect as the guarded one.

Every Java citation is at tag **4.12.11.0** (`fdb-record-layer/`, the pin in
MODULE.bazel). Every Go citation and corpus measurement is at **`ee6609738`**.
§1.1's ledger figures are re-pinned against the post-hotfix baseline as the
first act of S0 (§6).

---

## 1. The problem, measured — with each number's instrument named

### 1.1 The ledger

Instrument: one full corpus run,

```
bazelisk test //pkg/relational/conformance/javacorpus:javacorpus_test \
  --test_output=streamed --nocache_test_results
```

against a real FDB testcontainer. Exit 0; 206 `SKIP` lines logged by
`corpus_run_test.go:117`.

```
unsupported-DDL:value-index-as-select   42 files
```

42 is also the pinned value in `pinned_ledger_test.go:40` and `:53`. It is the
largest single skip class in the corpus, ahead of `unsupported-DDL:struct=39`.

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

This list exists nowhere in the repo: the class is assigned at runtime off the
engine's error string (`runner.go:378-381`), `gaps.go` carries no entry for it,
and per-file identity is pinned only as the opaque `pinnedAssignmentDigest`
(`pinned_ledger_test.go:71`). Recovering it required the run above.

**Baseline, checked against the hotfix.** The §1.4 hotfixes turn four silent
accept-and-mis-build paths into fail-closed rejections, and a fail-closed
rejection is a *new* DDL failure — it could have changed which class claims a
file, in particular `index-ddl.yamsql` and
`documentation-queries/index-documentation-queries.yamsql`. It did not: the
hotfix's AS-SELECT guard runs *after* the aggregate check precisely so the
value-index gap keeps this bucket, and
`git diff ef2b5911a fix/ddl-fail-closed-ordering-attrs -- pkg/relational/conformance/javacorpus/`
is empty. **The list above is the post-hotfix baseline, unchanged**, and every
gate in §8 denominates against it.

### 1.2 Which rejection each file hits — and what actually caused it

Instrument: the `SKIP` detail column of the same run, then the named index read
back out of the corpus file.

| Go rejection | files | site |
|---|---|---|
| `select element must contain an aggregate function (SUM, COUNT, MIN, MAX)` | 29 | `ddl.go:566-567` |
| `exactly one aggregate expression required in SELECT` | 12 | `ddl.go:451-453` |
| `select element must be an expression` | 1 | `ddl.go:561-562` |

The middle row does not mean what its message says. `ddl.go:451-453` fires on
`len(allElems) != 1` — **before any aggregate inspection whatsoever**. Reading
the named index out of each of the twelve files separates them:

| file | index | statement | actually |
|---|---|---|---|
| `bitmap-aggregate-index.yamsql` | `BITMAPINDEX1` | `bitmap_construct_agg(bitmap_bit_position(id)), bitmap_bucket_offset(id) … group by …` | **aggregate** |
| `composite-aggregates.yamsql` | `T1_I2` | `select col2, sum(col1) from t1 group by col2` | **aggregate** |
| `index-ddl-aggregates-only.yamsql` | `IDX_MV_CAT_REGION_SUM` | `select category, region, sum(amount) … group by category, region` | **aggregate** |
| `case-sensitivity.yamsql` | `I1` | `select col2, col1 from Table1 order by col2, col1` | value |
| `cte.yamsql` | `I1` | `select col2, col1 from t1 order by col2, col1` | value |
| `filter-index.yamsql` | `I1` | `select "name", "id" … where … order by "name", "id"` | value |
| `null-extraction-tests.yamsql` | `I1` | `select b3, b2 from B order by b3, b2` | value |
| `views.yamsql` | `I1` | `select department, salary from employees order by …` | value |
| `recursive-cte.yamsql` | `PARENTIDX` | `select parent, id from t1 order by parent, id` | value |
| `sql-functions.yamsql` | `T1_IDX1` | `select col2, col1, col3 FROM t1 order by …` | value |
| `documentation-queries/order-by-…` | `PRICE_IDX` | `select price, category from products order by price` | value |
| `documentation-queries/udf-…` | `DEPT_IDX` | `select department, salary from employees order by …` | value |

**Three, not twelve.** The corrected split of the 42 is 38 value-index
rejections, 3 aggregate-arm rejections, 1 `SELECT *`. D1 is argued on those
three plus the no-parallel-pipelines principle, not on twelve.

The last row of the first table is
`include-block/shouldPass/simple-include-different-env.yamsql`, whose
`create index i1 as select * from t1 where col1 > 1000 order by id` presents a
`*` where the matcher expects a `SelectExpressionElementContext`.

### 1.3 The shape surface is wider than the ledger class

The census (§3) measures **56 files** carrying at least one non-aggregate
AS-SELECT index; 42 are booked to this class.

The gap is **pass order, not line order**. `parseSchemaTemplate` registers every
table first (`ddl.go:148-149`) and every index second (`ddl.go:172-174`), so a
struct-typed column rejects the template before any index is examined —
regardless of where the index sits in the file. `valid-identifiers.yamsql`
falsifies the line-order reading directly: it is claimed by
`unsupported-DDL:struct` even though its index DDL is not textually last. The
remaining files are claimed by file-level directive classes.

**Consequence for §8: closing CQ-71 must not be scored on "42 files pass". It is
scored on "the class reaches 0 and every file's new destination is named".**

### 1.4 What Go does today — re-triaged

The routing point: `parseIndexDefinition` (`ddl.go:204-245`) type-switches the
ANTLR alternative and routes **every** `IndexAsSelectDefinitionContext` to
`parseAggregateIndexDefinition` at `ddl.go:237-238`, with no inspection of the
SELECT. That routing point has no counterpart in Java (§2.1).

The clauses it drops divide into two groups. Revision 1 had them backwards;
revision 2 fixed two of the four and still mis-filed `UNIQUE` and `WHERE` as
latent, because it triaged the corpus's carriers instead of the code path.

**LIVE — silent accept-and-mis-build. Nothing rejects these; the engine builds a
different index than the DDL asked for.**

| clause | what Go builds | site |
|---|---|---|
| `WITH ATTRIBUTES LEGACY_EXTREMUM_EVER` | `MIN_EVER_TUPLE` / `MAX_EVER_TUPLE` **unconditionally**; `IndexAttributes()` is never read | `ddl.go:597-617` |
| ON-source per-column `orderClause` | an **ascending** index for `ON t(a DESC)`; only `GetColumnName()` is read | `ddl.go:227-229` |
| `UNIQUE` (AS-SELECT) — *live on the aggregate-paired path; the corpus's 2 carriers are non-aggregate and pre-empted at `:566`* | a **non-unique** index; `parseAggregateIndexDefinition` never calls `def.UNIQUE()` (its only call site in the file is the ON-source branch, `ddl.go:210`) | `ddl.go:391-493` |
| `WHERE` (AS-SELECT) — *same qualifier; 17 carriers, all non-aggregate* | a **full** index where Java builds a sparse one, holding entries Java deliberately omits; the aggregate path reads only `fc.TableSources()` and `WhereExpr()` appears nowhere in `ddl.go` | `ddl.go:404-419` |

Java honours all four: `MAX_EVER_LONG`/`MIN_EVER_LONG` when the attribute is
present (`MaterializedViewIndexGenerator.java:449-465`); the per-column
direction threaded into `OrderByExpression`s
(`OnSourceIndexGenerator.java:205-209`, `:280-299`); `isUnique` at
`DdlVisitor.java:215`, which becomes `IndexOptions.UNIQUE_OPTION` in the stored
proto; and the predicate at `MaterializedViewIndexGenerator.java:169-172`. Every
one is on the wire: from identical DDL, Go and Java get different index types,
different entry byte order, different uniqueness enforcement, or different
entry SETS.

`UNIQUE` and `WHERE` are in this group because the *clause* is live even though
its *corpus carriers* are not: reached through an aggregate or `CARDINALITY`
select element, `extractAggregateFromSelectElement` succeeds and the drop
happens. The hotfix's `TestDDLRejectsAsSelectUniqueAndWhere` demonstrates it
directly with `CREATE UNIQUE INDEX mvu AS SELECT min_ever(col2) FROM t1 GROUP BY
col1` and its `WHERE` twin. Revision 2 listed both as latent, which was true of
the 19 corpus carriers and false of the code.

**Corpus reach, measured**: 4 AS-SELECT statements carry `LEGACY_EXTREMUM_EVER`
(`aggregate-index-tests.yamsql:39-40`, `index-ddl.yamsql:82-83`) and 2 ON-source
statements the equivalent option (`index-ddl.yamsql:88-89`); 16 ON-source
statements carry an explicit `orderClause`, across `index-ddl.yamsql`,
`index-ddl-values-only.yamsql` and
`documentation-queries/index-documentation-queries.yamsql`. **None is reached
today** — `aggregate-index-tests.yamsql` dies in the table pass
(`unsupported-DDL:struct`, `column "C"`), and the other three die at their first
AS-SELECT value index (`index-ddl.yamsql` at `IDX_MV_NAME`, line 39, well before
line 82). So the corpus does not currently witness those two defects — **and
closing CQ-71 is exactly what arms them.** `UNIQUE` and `WHERE` have no corpus
carrier on the reachable path at all, which is why they went unnoticed until the
hotfix probed for them directly.

**The hotfix has landed** as `c058d48ee` on `fix/ddl-fail-closed-ordering-attrs`
— one commit off this RFC's own base `ef2b5911a`, rejecting all four shapes (and
the vector arm's copy of the order-clause drop) with
`ErrCodeUnsupportedOperation` on the `INCLUDE`-rejection precedent, with sixteen
rejection pins and three mutation directions each. Its AS-SELECT guard runs
*after* the aggregate check deliberately, so the value-index gap keeps its
42-file bucket. **Measured: S0's baseline is unchanged** —
`git diff ef2b5911a fix/ddl-fail-closed-ordering-attrs -- pkg/relational/conformance/javacorpus/`
is empty, so the ledger of §1.1 stands byte-identical and every gate in §8
denominates against the list already printed there. S0's re-measurement step
(§6) is therefore discharged, not skipped.

**LATENT — one clause, measurably pre-empted in the same statement.**

| clause | statements | why latent |
|---|---|---|
| `ORDER BY` (AS-SELECT) | 134 | every carrier is non-aggregate → `ddl.go:566-567` rejects the statement first |

`ORDER BY` is the one clause that stays latent under scrutiny, and the reasoning
is worth recording because it is what makes it safe to leave to S3 rather than
hotfix. Every corpus aggregate AS-SELECT that carries an `ORDER BY` is
multi-element and dies at `ddl.go:451-453` first. The single-element aggregate
shapes that would slip past that check and reach the drop are exactly the ones
**Java itself rejects** — `MaterializedViewIndexGenerator.java:229-231`
("attempt to create a covering aggregate index") and `:241-243` ("Cannot order
%s index by aggregate value"). So a dropped `ORDER BY` there is
Go-accepts-what-Java-rejects, a conformance divergence, never two engines
writing mutually unreadable indexes for DDL both accept. That is the
qualitative line between the hotfix group and this one, and it is why `ORDER BY`
is closed properly by the generator in S3 instead of being fail-closed first.

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

`OnSourceIndexGenerator.generate` (`OnSourceIndexGenerator.java:172-229`)
reaches the same call: key columns ++ INCLUDE columns become one projection
(`:196-203`), key columns become `OrderByExpression`s carrying their per-column
`DESC`/`NULLS` (`:205-209`), `LogicalOperator.generateSort` wraps the result
(`:225`), and `:226-227` calls `MaterializedViewIndexGenerator.generate`.
**Both index forms in Java produce their key expression through one function.**

The **dedup filter** at `:173-176` is load-bearing for D2: an INCLUDE column
already present as a key column is dropped from the value list before the
projection is built, so `ON t(a) INCLUDE(a)` produces the same key expression as
`ON t(a)` rather than a duplicated column and a shifted split point.

### 2.2 The ORDER-BY-subset rule is a VALUE set difference, not a shape

`LogicalOperator.generateSelect` (`LogicalOperator.java:374-401`):

```java
389  final var orderByExpressions = Expressions.of(orderBys.stream().map(OrderByExpression::getExpression)…);
390  final var remainingOrderByExpressions = orderByExpressions.difference(output, outerCorrelations);
391  if (remainingOrderByExpressions.isEmpty()) {
392      return generateSort(generateSimpleSelect(output, …), orderBys, …);   // sort on top
393  } else {
394-399  … generateSimpleSelect(output ++ remaining) → generateSort → generateSimpleSelect(pulledOutput)
400  }
```

The **difference over expression values** decides; the structural difference
(sort on top vs. select-over-sort) is a *consequence*, which
`DdlVisitor.java:217` then reads. Porting the consequence instead of the cause
is what revision 1 did, and §5/D3 explains why that cannot work in Go.

`IndexTest.java:710-716` pins the rejection; `IndexTest.java:1061-1068` pins the
same error arriving from the aggregate side.

### 2.3 The value/version arm, line by line

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
| build | `:194` → `:497-524` | §2.5 |
| **split point** | `:195-198` | `splitPoint = orderByValues.size()`; if ORDER BY is empty **and** `generateKeyValueExpressionWithEmptyKey` is false (which it is for both DDL forms — `DdlVisitor.java:218`, `OnSourceIndexGenerator.java:227`), `splitPoint = -1` |
| **covering** | `:199-203` | `keyWithValue(expr, splitPoint)` iff `splitPoint != -1 && splitPoint < fieldValues.size()`; else the bare expression. Both go through `NullableArrayUtils.wrapArray` |

`reorderValues` (`:387-394`) asserts `values.size() >= orderByValues.size()`,
returns `values` unchanged when ORDER BY is empty, and otherwise returns
`orderBy ++ (values \ orderBy)` — **the ORDER BY fixes the key order, the
projection order only fixes the tail.** `IndexTest.java:673-681`
(`SELECT a1, a2 … ORDER BY a2, a1` → `concat(A2, A1)`) is the Java pin; the
corpus witnesses are pinned by `TestIndexDDLReorderWitnesses`.

### 2.4 Ordering functions: an exact four-way map

`ParseHelpers.isDescending`/`isNullsLast` (`ParseHelpers.java:167-179`) and
`OrderByExpression.toSortOrder` (`OrderByExpression.java:112-118`) produce a
`RequestedSortOrder`; the generator maps it to a function name at
`MaterializedViewIndexGenerator.java:347-363`:

| SQL | descending | nullsLast | RequestedSortOrder | key-expression wrapper |
|---|---|---|---|---|
| *(absent)* / `ASC` / `ASC NULLS FIRST` | false | false | `ASCENDING` | **none** |
| `ASC NULLS LAST` | false | true | `ASCENDING_NULLS_LAST` | `function("order_asc_nulls_last", …)` |
| `DESC` / `DESC NULLS LAST` | true | true | `DESCENDING` | `function("order_desc_nulls_last", …)` |
| `DESC NULLS FIRST` | true | false | `DESCENDING_NULLS_FIRST` | `function("order_desc_nulls_first", …)` |

`OnSourceIndexGenerator.IndexedColumn.parseColSpec` (`:280-299`) reproduces the
same defaults for the ON-source form; `parseUid` (`:302-306`) gives INCLUDE
columns `(false, false)`, irrelevant since they land past the split.

The wrapper is a wire artefact, not decoration: `OrderFunctionKeyExpression`
packs the tuple through `TupleOrdering` with the direction
(`OrderFunctionKeyExpressionFactory.java:44-48`), so a DESC column's index entry
bytes differ from an ASC column's.

### 2.5 Building the expression

`generate(List<Value>, orderingFunctions)` (`:497-524`):

- empty → `EmptyKeyExpression.EMPTY`; single → `toKeyExpression(value, …)`.
- otherwise the value list is consumed by a **peeking iterator**: a run of
  consecutive `FieldValue`s is compressed into one `FieldValueTrieNode`
  (`FieldValueTrieNode.java:201-238`), each new trie is checked against the
  previously built ones (`validateNoOverlaps`, `:136-153`), and non-field values
  become their own component. Components are `concat`-enated.
- The trie turns `SELECT r.s.a, r.s.b` into
  `field("R").nest(field("S").nest(concat(field("A"), field("B"))))` rather than
  two separate nestings (`IndexTest.java:819-830`), and produces
  `Index with multiple disconnected references to the same column are not
  supported` when the same parent is reached twice non-adjacently
  (`IndexTest.java:745-752`, `:763-771`).

Leaf construction, `toKeyExpression(Value)` (`:551-582`) and
`toFieldKeyExpression` (`:810-827`):

- `FieldValue` → nested `field(storageName, fanType)`; `FanOut` for arrays,
  `None` otherwise; a `ROW_VERSION`-typed, `__ROW_VERSION`-named field becomes
  `VersionKeyExpression.VERSION` (`:821-823`).
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

### 2.6 Validity and record type

`getRecordTypeName` (`:791-801`) runs FIRST, at `:151`, before `checkValidity`
at `:166`. It is what emits `Unsupported query, expected to find exactly one
type filter operator` — the message `IndexTest.java:500-520` and `:718-725`
assert. `checkValidity` (`:611-637`) then adds five further asserts:

| assert | line | message | Java test |
|---|---|---|---|
| exactly one `FullUnorderedScanExpression` | `:615` | `Unsupported index definition, %s iteration generator found` | none |
| at most one `GroupByExpression` | `:619` | `multiple group by expressions found` | `IndexTest.java:781-787` |
| ≤1 aggregation inside the group by | `:623` | `found group by expression with more than one aggregation` | `IndexTest.java:773-779` |
| all operators return record values | `:630` | `some operators return non-record values` | none |
| all fields simple or arithmetic | `:636` | `not all fields can be mapped to key expression in` | none |

The three unmapped ones get a test each (§3.2).

### 2.7 Predicate

`getTopLevelPredicate` is called with **`Lists.reverse(expressionRefs)`**
(`:169`) — the reverse of the topological order, so the walk starts at the
outermost operator. It skips a leading sort, tolerates a
select-having/group-by/select-where sandwich, asserts that no *inner* select
carries predicates (`:659-664`), converts each predicate with
`toResidualPredicate`, ANDs them, normalises to DNF with
`BooleanPredicateNormalizer` (`:675`), and requires `IndexPredicate.isSupported`
(`:676`; `IndexPredicate.java:212-232`).

**And then `:677-679`:**

```java
677  if (IndexPredicateExpansion.dnfPredicateToRanges(result).isEmpty()) {
678      return conjunction;      // the NON-normalised form
679  }
```

A DNF that expands to no ranges is discarded and the original conjunction stored
instead. This is a wire-visible branch — two different predicate protos for the
same DDL depending on whether the DNF yields ranges — and it gets its own gate
row (§8 gate (a) mutation 11).

### 2.8 Nullable arrays

`NullableArrayUtils.wrapArray` (`NullableArrayUtils.java:79-104`) is applied to
the **serialized proto**, not to the expression objects, and it is
**descriptor-driven**: `wrapArrayInternal` (`:106-192`) rewrites a `field(X)` or
`nesting(parent=X, …)` only when `isWrappedArrayDescriptor(X, parentDescriptor)`
holds — i.e. only when the generated descriptor for `X` really is
`message { repeated R values = 1; }` (`:62-66`). The `containsNullableArray`
boolean (`DdlVisitor.java:162`) is a fast-path gate in front of that check, not
the check.

### 2.9 The wire artefact

`RecordMetadataSerializer.visit(Index)` (`:75-89`) puts the generated expression
into `new Index(name, keyExpression, indexType, options, predicate)`, which
serializes to `RecordMetaDataProto.Index.root_expression`
(`record_metadata.proto:160`) plus `type` (`:165`), `options` (`:166`) and
`predicate` (`:172`). **`root_expression`, `type`, `options` and `predicate` are
the byte-level acceptance target of this RFC**; uniqueness rides in `options`
(`IndexOptions.UNIQUE_OPTION`), which is why §8(b1) must compare options and not
only the expression.

---

## 3. Branch → corpus coverage

**Instrument**:
`pkg/relational/conformance/javayamsql/index_ddl_shape_census_test.go`,
committed with this RFC. It parses every `schema_template` body with the real
ANTLR parser and classifies each `IndexAsSelectDefinitionContext` /
`IndexOnSourceDefinitionContext` from typed nodes. Its assertions ARE the table
below; a corpus bump that moves any of them fails the test with the count named.

**Scope**: the census sees index DDL declared in `schema_template` blocks.
`create-drop-create-template.yamsql` additionally issues 2 `create index … as
select` statements as ad-hoc test-block DDL, outside its reach; that exclusion
is itself asserted (`TestIndexDDLCensusScope`), so the surface cannot shrink
silently. Corpus-wide totals are 278 AS-SELECT statements over 61 files; the
census's 276 / 60 are the schema-template subset.

```
files declaring any CREATE INDEX                       60
AS-SELECT statements                                  276
  non-aggregate                                       194
  aggregate                                            82
files with a non-aggregate AS-SELECT                   56
```

Flags over the 194 non-aggregate statements:

| flag | count |
|---|---|
| `ORDER BY` present | 134 |
| `ORDER BY` absent | 60 |
| explicit `ASC`/`DESC`/`NULLS` | 16 |
| `WHERE` | 17 |
| `UNIQUE` | 2 |
| `"__ROW_VERSION"` in the projection | 14 |
| multi-column projection | 76 |
| **covering** — the split point falls strictly inside the projection | 18 |
| **reordered** — `reorderValues` changes the key order | 6 |
| multi-source `FROM` (unnest / derived table) | 15 |
| `SELECT *` | 3 |
| dotted path in the projection | 26 |
| `WITH ATTRIBUTES` | 0 |
| `INCLUDE(...)` on the AS-SELECT form | **0** |

Aggregate side: 4 statements carry `WITH ATTRIBUTES LEGACY_EXTREMUM_EVER`.

ON-source form: **52 statements, 16 with an explicit column `orderClause`, 17
with `INCLUDE`.** Both are branch coverage this RFC newly acquires (D2), and
both are dropped or refused today (§1.4).

The two derived definitions deserve their exact wording, because §8's mutations
are stated in them. *Covering* is "the split point falls strictly inside the
projection" — `0 < |ORDER BY| < |projection|` in `reorderValues`' terms — not "a
coincidence of column counts". *Reordered* is "taking the key order from the
projection instead of the ORDER BY would build a different index", which is
precisely mutation 1's condition. The six reordered statements are pinned by
name in `TestIndexDDLReorderWitnesses`: `orderby.yamsql`'s `I2`, `I3`, `I4`,
`I6`, `I8`, and `index-ddl-values-only.yamsql`'s `IDX_MV_FILTERED_EXPENSIVE`
(`select name, price … where price > 20 order by price` — key `(PRICE)`, `NAME`
past the split).

### 3.1 Branches the corpus REACHES

| generator branch | Java site | corpus witness |
|---|---|---|
| single field, no ORDER BY, `splitPoint = -1` | `:196-198` | `standard-tests.yamsql` `I1` |
| single field + ORDER BY, `splitPoint == size` → bare expression | `:199-203` | `documentation-queries/in-operator-queries.yamsql` `PRICE_IDX` |
| multi-field, ORDER BY == projection | `:193` | `sql-functions.yamsql` `T1_IDX1` |
| **`reorderValues` permutation** | `:387-394` | `orderby.yamsql` `I6` (`select d, c, b, a … order by a, b, c, d`) |
| **`keyWithValue` covering split** | `:199-200` | `index-ddl-values-only.yamsql` `IDX_MV_CAT_PRICE_COVERING` |
| `order_desc_nulls_last` | `:352` | `documentation-queries/index-documentation-queries.yamsql` `IDX_PRICE_DESC` |
| `order_asc_nulls_last` | `:355` | `index-ddl-values-only.yamsql` `IDX_MV_RATING_NULLS_LAST` |
| `order_desc_nulls_first` | `:358` | `index-ddl.yamsql` `IDX_MV_DESC_NULLS_FIRST` |
| ASCENDING → **no** wrapper | `:348-350` | `index-ddl.yamsql` `IDX_MV_NULLS_FIRST` (`asc nulls first`) |
| mixed per-column direction | `:345-381` | `index-ddl.yamsql` `IDX_MV_ASC_DESC` |
| `VERSION` index type | `:188` | `versions-tests.yamsql` `T1_VERSION_INDEX` |
| version mixed into a covering key | `:199-200` + `:188` | `pseudo-field-clash.yamsql` `T2_COL1_VERSION`; `versions-tests.yamsql` `T3_VERSION_WITH_COL1` |
| nested `FieldValue` trie | `:504-517` | `orderby.yamsql` deep-nesting shapes; `user-defined-macro-function-tests.yamsql` `DEEP_IDX` |
| unnest / `ExplodeExpression` quantifier | `:725-743` | `in-predicate.yamsql` `FRUITS` (`from array_table as t, t.fruits as "fruit"`) |
| two-level unnest | `:725-743` | `valid-identifiers.yamsql` |
| derived table over a repeated field | `:725-743` | `subquery-tests.yamsql` `IR`; `arrays-unnesting.yamsql` `MV` |
| `CardinalityValue`, non-nullable array | `:555-566` | `arrays-cardinality.yamsql` `TAB1_INDEXED_NN_INDEX` |
| `CardinalityValue`, **nullable** array | `:555-566` + `NullableArrayUtils` | `arrays-cardinality.yamsql` `TAB1_INDEXED_INDEX` |
| `CardinalityValue`, nested | `:555-566` | `arrays-cardinality.yamsql` `TAB2_INDEX` |
| `ArithmeticValue` (`+`, `&`) | `:567-575` | `indexed-functions.yamsql` `BPLUSC`, `DMASK1` |
| `bitmap_bucket_offset` as a plain projection | `:567-575` | `bitmap-aggregate-index.yamsql` `AGG_INDEX_2` |
| predicate, simple comparison | `:169-172` | `index-ddl-values-only.yamsql` `IDX_MV_FILTERED_EXPENSIVE` (`where price > 20`) |
| predicate, conjunction | `:674` | `index-ddl-values-only.yamsql` `IDX_MV_FILTERED_MULTI` |
| predicate, `OR` → DNF | `:675` | `sparse-index-tests.yamsql`; `filter-index.yamsql` `I1` |
| predicate, boolean constant | `:169-172` | `boolean-ddl.yamsql` |
| `SELECT *` projection | `:174` | `include-block/shouldPass/simple-include-different-env.yamsql` `I1` |
| `UNIQUE` | `DdlVisitor.java:215` | `join-with-order-by-tests.yamsql`; `index-ddl.yamsql` `IDX_MV_UNIQUE_PROFESSION` |
| aggregate + projected grouping columns | `:249-299` | `composite-aggregates.yamsql` `T1_I2` |
| `PERMUTED_MIN`/`MAX` permuted size from ORDER BY | `:238-240` | `aggregate-index-tests.yamsql` `MV18`–`MV20` |
| bitmap aggregate | `:413-432` | `bitmap-aggregate-index.yamsql` `BITMAPINDEX1` |
| `LEGACY_EXTREMUM_EVER` | `:449-465` | `aggregate-index-tests.yamsql` `MV12`; `index-ddl.yamsql` `IDX_MV_AGE_MIN_EXTREMUM` |
| **ON-source key columns** | `OnSourceIndexGenerator.java:196-209` | 52 statements |
| **ON-source `orderClause`** | `:280-299` | 16 statements, incl. `index-ddl.yamsql` `IDX_IOT_DESC_NULLS_FIRST` |
| **ON-source `INCLUDE`** | `:173-176`, `:196-203` | 17 statements, incl. 11 in `index-ddl-aggregates-only.yamsql:70-106` |

### 3.2 Branches the corpus does NOT reach

Ported 1:1 regardless; the difference is only that their tests come from
`IndexTest.java` rather than a corpus file.

| unreached branch | Java site | test |
|---|---|---|
| `LiteralValue` leaf (`SELECT 5+1`) | `:576-577` | `IndexTest.java:522-530` |
| multi-field without ORDER BY → error | `:190-192` | `IndexTest.java:694-700` |
| `validateNoOverlaps` rejection | `FieldValueTrieNode.java:136-153` | `IndexTest.java:745-752`, `:763-771`, `:927-935` |
| ORDER BY outside the projection → `INVALID_COLUMN_REFERENCE` | `DdlVisitor.java:217` | `IndexTest.java:710-716` |
| more than one type filter (join) → error | `:791-801` | `IndexTest.java:500-520`, `:718-725` |
| array field without unnesting → error | `:814-819` | added |
| more than one version column → error | `:184` | added |
| predicate inside an inner select → error | `:659-664` | added |
| `IndexPredicate.isSupported` rejection | `:676` | added |
| DNF-yields-no-ranges → store the conjunction | `:677-679` | added |
| `checkValidity` asserts at `:615`, `:630`, `:636` | `:611-637` | added |
| `SplitKeyExpression` / `ListKeyExpression` in `wrapArray` | `NullableArrayUtils.java:165-189` | round-trip test only |
| ON-source `INCLUDE` column already a key column (dedup) | `OnSourceIndexGenerator.java:173-176` | added |

---

## 4. The two committed probes

Both are in the tree with this RFC, because the decisions in §5 rest on them.

**`pkg/relational/core/embedded/index_ddl_order_by_shape_test.go`** —
`TestIndexDefinitionOrderByIsNotStructurallyDiscriminable` and
`TestIndexDefinitionOrderByUnprojectedIsAccepted`. Eight index-definition
SELECTs from `orderby.yamsql`'s shape family, six legal and two with an ORDER BY
outside the projection, built through the metadata path
(`NewPlanVisitor(tmpl.Underlying()).VisitQuery`). **All eight come out
Project-over-Sort.** A positional discriminator therefore answers "reject" for
all eight: it rejects the six legal ones and cannot distinguish the two illegal
ones. Mutation-checked — flipping the assertion to expect Sort-on-top turns all
eight RED (`top operator is a Sort for "SELECT b, c FROM t1 ORDER BY b"`), and
restoring it returns GREEN.

The second test is a **negative result** with a named re-arm: Go *accepts*
`SELECT b FROM t1 ORDER BY c` today, which is what makes D3's explicit check
load-bearing. Its failure message says so, so a future change that makes the
builder reject the shape on its own re-opens the decision instead of silently
making D3 redundant.

**`pkg/relational/conformance/javayamsql/index_ddl_shape_census_test.go`** —
the §3 table, as assertions, plus the reorder witnesses and the scope pin.

---

## 5. Decision

### D1 — One generator, over the logical plan. `parseAggregateIndexDefinition` is deleted.

A new package `pkg/relational/core/query/ddl` (mirroring Java's
`recordlayer/query/ddl`) holds `MaterializedViewIndexGenerator`, a 1:1 port
consuming a `logical.LogicalOperator` and producing an index description (root
key expression + index type + options + predicate). It is the **only** producer
of index metadata from DDL. `parseAggregateIndexDefinition` (`ddl.go:391-493`),
`extractAggregateFromSelectElement` (`:558-623`),
`extractCardinalityColumnFromSelectElement` (`:502-531`) and the aggregate /
cardinality / fan-out arms of `metadata.Builder.Build` are removed with it.

Import direction: `embedded` → `query/ddl` → {`query/logical`, `core/metadata`,
`recordlayer`}. Verified acyclic — neither `core/metadata` nor `query/logical`
imports `embedded`.

*Why this and not a second arm.* (a) It is what Java does: the split is at
`MaterializedViewIndexGenerator.java:187`, inside one function, and both DDL
forms reach it (§2.1). (b) Three of the 42 files are aggregate indexes Go's arm
rejects (§1.2) — a value-only arm leaves the class populated. (c) The current
arm is an ANTLR-shape matcher; keeping it is the "no parallel pipelines" failure
CLAUDE.md names — two index constructors that must agree forever with no
mechanism forcing them to. Revision 1 argued (b) at twelve files; it is three,
and (a) and (c) carry the decision.

### D2 — Both DDL forms funnel into it, and INCLUDE stops failing closed.

`parseIndexDefinition` becomes a front end:

- `IndexAsSelectDefinitionContext` → plan the `queryTerm` (D4) → generator.
- `IndexOnSourceDefinitionContext` → synthesize the projection
  `keyColumns ++ (includeColumns \ keyColumns)` — the dedup filter at
  `OnSourceIndexGenerator.java:173-176` — and the ORDER BY list from the key
  columns' `orderClause` (defaults per §2.4) → the same logical shape →
  generator. This closes the dropped `orderClause` (`ddl.go:227-229`) and the
  `INCLUDE` rejection (`ddl.go:221-224`) as a **consequence**, not extra scope.
- `VectorIndexDefinitionContext` keeps its own path (§10).

**The INCLUDE reversal is not optional for CQ-71.**
`index-ddl-aggregates-only.yamsql` is one of the 42, and it declares 11
ON-source `INCLUDE` indexes at `:70-106`. Its DDL cannot succeed while
`ddl.go:221-224` stands, so the file cannot leave the class — §8(c) is
unreachable without the reversal. Keeping the rejection would also leave a
second Go-only index-construction path alive, which is what D1 exists to delete.

### D3 — ORDER-BY-subset is a VALUE set difference. Never a shape.

Port `LogicalOperator.java:389-390` semantically: an explicit subset test between
the sort's keys and the projection's values.

- Sort keys: `logical.LogicalSort.Keys`, each a
  `logical.SortKey{Expr, Dir, NullsFirst, Value, Pos}`
  (`logical/operators.go:376-388`) — `Value` is the resolved `values.Value`, the
  identity to compare on; `Dir`/`NullsFirst` carry the four-way ordering
  information §2.4 needs.
- Projection values: `logical.LogicalProject.ProjectedValues`
  (`logical/operators.go:217-221`), parallel to `Projections`.
- The test: every sort key's `Value` must be structurally equal to some
  projected value. A key whose `Value` slot is nil (the walker declined) is a
  hard error, not a pass — silently reading "unknown" as "present" is how the
  wrong index gets built.
- A non-empty remainder → `INVALID_COLUMN_REFERENCE`, message verbatim from
  Java: `Cannot create index and order by an expression that is not present in
  the projection list`.
- `Pos` (positional `ORDER BY <n>`) resolves to the projection slot directly, as
  it already does in the builder.
- `SELECT *` has no `LogicalProject` at all (`visitFinalProjection` returns its
  input unchanged for a star, `plan_visitor.go:1896-1900`), so the subset test
  is vacuously satisfied — matching Java, where the star has already expanded to
  every column before the difference is taken.

**Never the sort's position.** §4's probe is the measurement: all eight shapes —
legal and illegal alike — are Project-over-Sort, so position carries no
information. The layering is not incidental either; it is pinned by
`sort_projection_layering_test.go`, because sinking the projection below the
sort re-arms a name-based sort-key resolution path RFC-197 deleted.

### D4 — The logical plan comes from the existing planning front end, post-passes included.

`NewPlanVisitorWithSchema(md, schema).VisitQuery(q)` (`plan_visitor.go:143-156`)
is **not** the front end — it is its first stage. Five mandatory post-passes
follow it on both existing callers (`cascades_generator.go:361-382`,
`plan_harness.go:530-551`), and the index path must run all five in order:

1. `demoteSchemaQualifiedUnnest` — re-classifies a schema-qualified table the
   metadata-less parser mistook for a lateral unnest (RFC-142).
2. `rejectAtOrdinalityOnTable` — `WRONG_OBJECT_TYPE`, before column validation
   so it is not masked.
3. `rejectDuplicateUnnestAlias` — later-source alias collisions.
4. `resolveQualifiedTableNames`.
5. `validateTablesAndColumns` — **the source of `UNDEFINED_COLUMN`**.

Skipping (5) would let `CREATE INDEX x AS SELECT nonexistent_col FROM t` build
an index over a column that does not exist. That is §8(a) mutation 12.
`IndexTest.java:702-708` is Java's pin for the same input (`UNDEFINED_COLUMN`,
"non existing column").

Two further gates on this boundary:

- **The `md == nil` text fallback must be unreachable here.** `VisitQuery`
  silently degrades to `buildLogicalPlanForQuery(q)` — a catalog-free builder —
  when `v.md == nil` (`plan_visitor.go:160-162`). An index generator taking that
  path would produce a key expression from unresolved names. The index path
  asserts non-nil metadata and fails loudly; §8(a) mutation 13.
- **Derived tables are `LogicalCTE`, not `LogicalUnnest`.** A FROM-clause
  derived table (`FROM T1, (SELECT col3 FROM T1.A) X`) reaches `logical.NewCTE`
  via `buildOuterPlanOnDerived` (`logical_predicate.go:6421-6434`);
  `lateralUnnestCandidate` requires `derivedQuery == nil`, so it is never an
  unnest. The generator's quantifier-dereferencing walk (Java
  `collectQuantifiers`, `:720-743`) must handle both node kinds. 15 corpus
  statements have a multi-source FROM and the derived-table spelling is the
  common one.

Operator correspondence, corrected:

| Java | Go |
|---|---|
| `FullUnorderedScanExpression` + `LogicalTypeFilterExpression` | `logical.LogicalScan` (`operators.go:81`) |
| `ExplodeExpression` quantifier, **lateral unnest spelling** | `logical.LogicalUnnest` (`:112`) |
| `ExplodeExpression` quantifier, **derived-table spelling** | `logical.LogicalCTE` (`:903`) via `buildOuterPlanOnDerived` |
| `SelectExpression` predicates | `logical.LogicalFilter` (`:169`) |
| `SelectExpression` result values | `logical.LogicalProject` (`:217`) |
| `LogicalSortExpression` | `logical.LogicalSort` (`:415`) |
| `GroupByExpression` | `logical.LogicalAggregate` (`:522`) |

Metadata for the index's SELECT comes from the tables-only
`metadata.Builder.Build()` of the first DDL pass (`ddl.go:148-149`), matching
`DdlVisitor.java:208-212`. **No second SQL front end is written.**

### D5 — Key order, split point and wrapper placement are Java's, verbatim.

`reorderValues` = ORDER BY values in ORDER BY order, then the remaining
projection values in projection order. `splitPoint = len(orderBy)`, forced to
`-1` when the ORDER BY list is empty. `KeyWithValue` iff
`splitPoint != -1 && splitPoint < len(fieldValues)`. Ordering wrappers at the
**leaf**, including inside a trie. Each has a golden and a mutation in §8(a).

### D6 — `__ROW_VERSION` becomes a projectable pseudo-column.

The `VALUE`-vs-`VERSION` decision (`:188`) is driven by a projected field whose
type is `PseudoField.ROW_VERSION`'s (`PseudoField.java:36-40`: name
`"__" + ROW_VERSION`, type `Type.primitiveType(VERSION, true)`), and the leaf
becomes `VersionKeyExpression.VERSION` (`:821-823`). Go cannot resolve the
identifier at all today — `gaps.go:54` books `column "__ROW_VERSION" does not
exist` as `engine-gap:row-version-pseudocolumn`. 14 corpus statements need it.
In scope: an index generator whose VERSION arm cannot be reached is the "rule
ported but can't fire" non-done.

### D7 — The predicate arm is built, including the DNF fallback.

Go already consumes index predicates (`pkg/recordlayer/index_predicate.go`,
`Index.SetPredicateProto` at `pkg/recordlayer/index.go:492-507`) and round-trips
them through `RecordMetaDataProto.Index.predicate`. The producer is new:
`LogicalFilter` predicate → `gen.Predicate`, via Java's pipeline — residual form
→ AND → DNF normalisation → `isSupported` gate → **and `:677-679`'s
dnf-yields-no-ranges fallback to the non-normalised conjunction**, a distinct
stored proto with its own gate row.

### D8 — `metadata.Builder` learns to carry an explicit index.

`indexSpec` (`builder.go:43-68`) has no field for a key expression or index
type; `Build()` re-derives the shape from column names
(`buildIndexKeyExpression`, `:709-722`). Wrong layering once the generator
exists. `indexSpec` gains `rootExpression recordlayer.KeyExpression`,
`indexType string`, `predicate *gen.Predicate`, `options map[string]string`, and
`Build()` short-circuits to `recordlayer.NewIndex(name, rootExpression)` plus
type/options/predicate/unique when set. `AddIndex(table, name, cols, unique)`
stays for the primary-key and simple paths that do not go through DDL.

### D9 — `wrapArray` is ported descriptor-driven; its no-op today is PINNED.

Port `wrapArray`/`wrapArrayInternal` over the proto form, gated on the same
DDL-derived flag Java computes (`DdlVisitor.java:162`). Because the rewrite is
per-node and conditioned on `isWrappedArrayDescriptor` against the **actual**
built descriptor (`NullableArrayUtils.java:123`, `:151`), this is byte-identical
to Java for any descriptor Java would produce, and a no-op under Go's current
descriptors — Go writes plain repeated fields, not the wrapper (the RFC-143 §3a
divergence, stated at `metadata/builder.go:536-542`).

That no-op is a **negative result and it gets pinned**: a test asserts that Go's
generated descriptor for a nullable `ARRAY` column has no `values` wrapper, with
a failure message naming what re-arms — the moment RFC-143 §3a lands, the wrap
becomes live and the goldens for `arrays-cardinality.yamsql`'s
`TAB1_INDEXED_INDEX` and `TAB2_INDEX` change.

### D10 — The planner learns the two shapes the generator will emit. Both are live defects.

Index enumeration is not DDL-specific: `buildMatchCandidates`
(`cascades_generator.go:2064-2168`) walks `md.GetAllIndexes()` generically. But
two shapes this RFC starts emitting are mishandled.

**(a) `KeyWithValue` roots — a wrong-column-set defect, reachable TODAY.**
`indexKeyColumnNames` (`cascades_generator.go:2265-2295`) handles `KeyWithValue`
by recursing into the inner key and **discarding the split point**
(`:2291-2292`), so every value-part column is reported as a key column. The
guard at `:2184-2192` does not save it: `KeyWithValueExpression.ColumnSize()`
returns the split point (`key_expression.go:1286-1288`), so
`len(names) == ColumnSize()` fails, and the **fallback**
`d.idx.RootExpression.FieldNames()` returns the same full inner list. Both arms
are wrong; the fix must cover the fallback, not only the guarded path.

Revision 1 called this unreachable. It is not: a Go process reading
**Java-authored** metadata deserializes a `KeyWithValue` root through
`key_expression_proto.go:305-310` and reaches `IndexColumnNames` immediately.
Go-authored DDL cannot produce one today (vector claims those candidates first
at `:2141-2144`, and `INCLUDE` is refused), which is a statement about Go's
writers, not about the defect. **The negative pin is therefore built from
Java-authored metadata bytes**, not from a hand-built Go index — the latter
would pin the wrong thing.

Separately, `keyExpressionFlatColumnDescriptors` (`pkg/recordlayer/query/plan/cascades/index_expansion.go:773-822`)
has no `KeyWithValue` case and returns false at `:820`, so the candidate
declines rather than mis-plans — safe, but not "planned". Both are fixed here:
the split point becomes the key/value boundary, key columns bound the scan
prefix, value columns are available for covering.

**(b) order-function-wrapped columns.** Only `"cardinality"` is accepted as a
function-keyed column (`pkg/recordlayer/query/plan/cascades/index_expansion.go:801-806`;
`pkg/recordlayer/query/plan/cascades/match_candidate_index.go:716-720`), and `indexColumnFunctionTags`
(`cascades_generator.go:2237-2263`) tags a generic `FunctionKeyExpression` as
`""`, reporting it as a plain field. 16 AS-SELECT and 16 ON-source statements
carry explicit `ASC`/`DESC`/`NULLS`. The four `order_*` functions are already
registered in the record layer
(`pkg/recordlayer/order_function_key_expression.go:18-21`) and evaluate through
`TupleOrdering`, so this is a matching/ordering change, not new machinery.

D10 is why this RFC needs a Graefe ACK.

### D11 — The cross-engine metadata check, at the subspace each side actually writes.

Feasibility is measured, not assumed. `createSchemaTemplatePersistentJava`
(`conformance/sql_plan_steps.java:332-346`) executes arbitrary
`CREATE SCHEMA TEMPLATE … CREATE INDEX …` DDL and **persists** the resulting
`RecordMetaDataProto.MetaData` into the shared FDB catalog (companion
`dropSchemaTemplatePersistentJava`, `:352-373`). Go reads it back via
`SchemaTemplateCatalog().ListTemplates` (`fdb_template_catalog.go:167-192`),
whose `META_DATA` column is the stored proto. The normalisation problem is
already solved: `clearProto2Defaults` / `normalizeKeyExprJSON`
(`conformance/metadata_proto_conformance_test.go:71-145`) exist because Java
materialises proto2 defaults Go leaves unset, and `compareMDIndexes` (`:422-435`)
already compares `RootExpression` index by index.

**Two corrections revision 1 got wrong:**

1. **Subspace.** `catalog.OpenRecordLayerStoreCatalog()` opens the
   Java-wire-compatible `(NULL, NULL, int64(0))` subspace
   (`fdb_store_catalog.go:47-79`), while the Go sqldriver writes through
   `keyspace.RelationalKeyspace.CatalogSubspace()` — three string tuple
   elements. A comparison built on `OpenRecordLayerStoreCatalog` reads the Java
   side and *nothing* on the Go side. The test opens **each side at the subspace
   its writer used**, and says so at the call site; migrating the driver onto
   the Java-compatible subspace is tracked separately and is not a prerequisite.
2. **What is compared.** `compareMDIndexes` compares only names and
   `RootExpression`; the Java emitter
   (`metadata_proto_conformance.java:179-190`) emits neither `options` nor
   `predicate`. **Both sides gain both fields.** That is not tidiness:
   `IndexOptions.UNIQUE_OPTION` is where uniqueness lives in the stored proto,
   so **`UNIQUE`'s entire cross-engine coverage is the options comparison** —
   without it, a Go index that silently drops `UNIQUE` (today's behaviour,
   §1.4) compares equal to Java's unique index.

Comparison is at the **raw stored bytes**, not through Go's object model, so a
Go deserialisation bug cannot mask a divergence. The suite is already in the
merge gate (`conformance/BUILD.bazel:141`, `tags = ["conformance_java"]`).

*Rejected within D11:* a new `@ConformanceStep` returning metadata over HTTP.
~15 lines, but the catalog route needs no Java change and additionally proves
the **stored** bytes match, which is the property a shared cluster depends on.

### D12 — Five 1:1 hazards, named so the port does not rediscover them

| hazard | Java | what a naive Go port gets wrong |
|---|---|---|
| `orderingFunctions` is an **`IdentityHashMap`** (`:185`) | keyed on object identity | a value-equality map merges two equal `Value`s: `ORDER BY a ASC, a DESC` takes one wrapper for both. Go must key on the value's identity (pointer / ordinal), not its structure |
| `AnnotatedAccessor` carries an **explode counter** in `equals`/`hashCode` (`:683-718`, set at `:733`) | two unnests of the same array field stay distinct trie keys | a plain accessor makes them equal, and `validateNoOverlaps` (`FieldValueTrieNode.java:136-153`) then rejects the legal cartesian shape `IndexTest.java:736-743` accepts |
| `withDisabledLiteralProcessing` (`DdlVisitor.java:211`) | literals stay literal instead of becoming parameters | Go has no analogue; with literal extraction on, `SELECT 5+1` arrives as a parameter reference and trips `:180`'s value-kind assert. The index path must plan with literal processing off |
| `adjustGroupByFieldPaths` (`:302-327`) | strips the field path's root for values referencing the select-where quantifier | omitting it in S3 yields a grouping key expression one nesting level too deep |
| `OnSourceIndexGenerator`'s dedup filter (`:173-176`) | INCLUDE ∩ key removed before projection | without it `ON t(a) INCLUDE(a)` builds a duplicated column and a shifted split point |

---

## 6. Sequencing

Each step ends green and is committed on its own.

- **S0 — DONE, `c058d48ee` on `fix/ddl-fail-closed-ordering-attrs`** (one commit
  off this RFC's base `ef2b5911a`). The §1.4 hotfixes: `WITH ATTRIBUTES` /
  `OPTIONS(LEGACY_EXTREMUM_EVER)`, the per-column `orderClause` (plus the vector
  arm's copy of it), and AS-SELECT `UNIQUE` / `WHERE` all reject fail-closed
  instead of being dropped. Ledger re-measured and **byte-identical** (§1.1), so
  the baseline every gate below denominates against is the one already printed
  there. S2–S6 retire each of these rejections as the generator grows the
  capability it stands in for.
- **S1 — `metadata.Builder` carries an explicit index** (D8). No behaviour
  change; existing derivation paths keep working with the new fields unset.
- **S2 — the generator, value/version arm** (D1, D3, D4, D5), driven from the
  AS-SELECT form only. Ported `IndexTest.java` cases for every §3.1/§3.2 shape
  not needing D6/D7.
- **S3 — the aggregate arm** (D1) — `collectResultValues`,
  `adjustGroupByFieldPaths`, `generateAggregateIndexKeyExpression`, permuted
  size, bitmap, legacy extremum. `parseAggregateIndexDefinition` is deleted at
  the end of this step, and S0's fail-closed `LEGACY_EXTREMUM_EVER` rejection
  becomes a real implementation here.
- **S4 — `__ROW_VERSION`** (D6), then the VERSION arm's goldens.
- **S5 — the predicate arm** (D7), including the `:677-679` fallback.
- **S6 — the ON-source front end** (D2): `orderClause` honoured, `INCLUDE`
  accepted with the dedup filter, both routed through the generator. S0's
  fail-closed `orderClause` rejection becomes a real implementation here.
- **S7 — planner** (D10): the Java-authored-metadata negative pin first, then
  the `KeyWithValue` key/value boundary in both the guarded and fallback arms,
  then order-function columns.
- **S8 — the cross-engine metadata conformance test** (D11), including the
  options/predicate emitter change on the Java side.
- **S9 — ledger transition** (§8(c)), in one commit with the updated
  `pinnedLedger`, `pinnedAssignmentDigest`, `maskedClasses` and the per-file
  destination table.

Anything S1–S8 surfaces that is a Go-engine divergence is root-caused and fixed
in the same step with a regression pin, per the standing fix-now rule.

---

## 7. Instruments

- **Key-expression goldens**: table-driven, DDL in → normalised
  `gen.KeyExpression` proto out, one row per §3.1/§3.2 shape, each expected
  value written as the `IndexTest.java` expression it was ported from, with the
  line cited.
- **Index-entry differential**: for shapes whose entry bytes depend on the
  expression (DESC via `TupleOrdering`, fan-out, `KeyWithValue` split,
  `VERSION`), sample rows written through the Go store and entry keys/values
  compared against the Java-built store.
- **Cross-engine metadata**: D11.
- **Corpus census**: §3's committed classifier.
- **Ledger census**: the corpus run of §1.1, re-run at S0 and S9.
- **Plan-shape assertions**: §8(d)'s named venue.

---

## 8. Acceptance gates

**(a) Goldens — mutation-checked per direction.** Every §3.1/§3.2 shape has a
pinned normalised `root_expression`, `type`, `options` and `predicate`. The fix
must be mutable in each direction independently, and each mutation must go RED
with the failure quoted:

1. key order: projection order instead of `reorderValues`' ORDER BY order → the
   six `TestIndexDDLReorderWitnesses` shapes' goldens must fail;
2. split point: `len(orderBy)` → `len(fieldValues)` → the 18 covering shapes'
   goldens must fail;
3. split suppression: drop the `orderBy.isEmpty() → -1` rule → the
   no-ORDER-BY single-column golden must fail;
4. ordering wrapper: emit `order_asc_nulls_first` for plain `ASC` → the
   `IDX_MV_NULLS_FIRST` golden must fail;
5. nulls default: `DESC` → `order_desc_nulls_first` → the `IDX_PRICE_DESC`
   golden must fail;
6. wrapper placement: apply the ordering function at the root instead of the
   leaf → the `IDX_MV_ASC_DESC` golden must fail;
7. index type: emit `VALUE` for a projected `__ROW_VERSION` → the
   `T1_VERSION_INDEX` golden must fail;
8. trie: one nesting per field instead of compressing runs → the `r.s.a, r.s.b`
   golden must fail;
9. fan type: `FanOut` instead of `Concatenate` under `CARDINALITY` → the
   **`TAB1_INDEXED_NN_INDEX`** golden must fail. *(The NON-nullable witness,
   deliberately. `TAB1_INDEXED_INDEX` is the nullable one, and its golden is
   carved out of (b1) — see (b1) and D9. Revision 1 cited the nullable
   statement, which collides with the carve-out and would have left this
   mutation ungated.)*
10. predicate: drop DNF normalisation → the `sparse-index-tests.yamsql` /
    `filter-index.yamsql` `OR` golden must fail;
11. predicate fallback: always store the normalised DNF, never `:677-679`'s
    conjunction → the DNF-yields-no-ranges golden must fail;
12. post-pass: skip `validateTablesAndColumns` →
    `CREATE INDEX x AS SELECT nonexistent_col FROM t` must stop returning
    `UNDEFINED_COLUMN`;
13. metadata guard: allow the `md == nil` text fallback → the loud-failure test
    must go red;
14. **aggregate arm** — the four §3.1 branches the deleted arm used to serve,
    now with the red-green they never had: drop `collectResultValues`' grouping
    reconciliation → `composite-aggregates.yamsql` `T1_I2`'s golden fails; force
    `permutedSize = 0` → `aggregate-index-tests.yamsql` `MV20`'s option fails;
    accept `bitmap_construct_agg(col)` without `bitmap_bit_position` →
    `BITMAPINDEX1`'s golden fails; ignore `LEGACY_EXTREMUM_EVER` →
    `IDX_MV_AGE_MIN_EXTREMUM`'s index **type** fails (`MIN_EVER_TUPLE` vs
    `MIN_EVER_LONG`);
15. **ON-source `orderClause`** — a key-expression golden, not merely a planner
    assertion: drop the per-column direction → `IDX_IOT_DESC_NULLS_FIRST`'s
    golden must fail. Its AS-SELECT twin `IDX_MV_DESC_NULLS_FIRST` must have the
    byte-identical expression, which is the assertion that the two front ends
    really converged;
16. **ON-source `INCLUDE` dedup**: drop the `:173-176` filter → `ON t(a)
    INCLUDE(a)`'s golden must fail.

A gate that survives its own mutation is not a gate; each is re-checked until it
goes red.

**(b) Wire equality against Java.**

- **(b1)** D11's cross-engine comparison green for every §3.1 shape Java
  accepts, over stored `MetaData` bytes, comparing `root_expression`, `type`,
  **`options`** (where `UNIQUE` lives — D11) and **`predicate`**.
  **Carve-out**: shapes whose table declares a *nullable* `ARRAY` are excluded,
  and the exclusion names RFC-143 §3a. Go's descriptor has no `values` wrapper,
  so Java's key expression legitimately differs; including them would assert a
  divergence this RFC does not close, and (a) mutation 9 covers the fan-type
  direction on the non-nullable witness instead. The carve-out is an explicit
  list, not a predicate, so it cannot silently grow.
- **(b2)** Index **entry** bytes agree for sampled rows on DESC, fan-out,
  `KeyWithValue` and `VERSION` shapes.
- **(b3)** D9's no-op wrapper pin is present and names its re-arm condition.

**(c) The ledger transition, measured and fully accounted.** The deletion
surface is the whole surface — a class left half-deleted repopulates silently:

- `SkipDDLValueIndexAsSelect` removed from `ledger.go:106-110` **and** from the
  `AllSkipClasses()` list at `:195` (it is a function, not a var);
- the class removed from both `pinnedLedger` strings
  (`pinned_ledger_test.go:40`, `:53`) and every affected count among the **60**
  pinned numbers re-measured — 4 header pairs (`pass`, `fail`, `skip`,
  `queries`) + 29 `file_skips` + 27 `inner_skips` — with
  `pinnedFileTotal = 238` still closing and `fail=0` holding;
- `pinnedAssignmentDigest` (`:71`) recomputed — the only thing that catches two
  files trading classes at constant totals;
- **`maskedClasses` (`corpus_run_test.go:246-256`) updated**: its
  `SkipDDLFunction` entry states that value-index-as-select claims those files
  first. Once the class is gone, either `SkipDDLFunction` starts appearing — in
  which case the entry is deleted — or something else masks it and the entry
  must say what. `TestSkipClassesAreAllReachable` (`:259`) is blind in one
  direction: it checks that declared classes are produced or explained, not that
  a produced class is declared, so a stale entry there fails nothing;
- the commit carries a table mapping **each file in the re-measured §1.1 list**
  to its new status and class. A file landing in a class that is neither `pass`
  nor an already-named RFC-201 phase gap is a hard failure, not a re-booking.

Floor: at least the five files whose only blocker is this one —
`documentation-queries/{between,case,in,is-distinct-from,like}-operator-queries.yamsql`
(measured: zero `explain`/`explainContains`/`planHash`/`resultMetadata`
directives anywhere under `documentation-queries/`; their templates declare
nothing but tables and non-aggregate AS-SELECT indexes) — must reach `pass`.
Fewer than five means the port is incomplete.

**(d) The indexes are PLANNABLE — with a venue.** The floor files have zero plan
directives, so the corpus cannot host this assertion; asserting it there would
be a checkbox with nothing behind it. The venue is **Go-side yamsql scenario
pins** under `pkg/relational/conformance/yamsql/testdata`, one per shape family,
each an `EXPLAIN`-asserted scenario using the same DDL as its corpus twin:

- a covering (`KeyWithValue`) index produces a covering scan with **no fetch**;
- a `DESC` index satisfies `ORDER BY … DESC` with **no in-memory sort**;
- an ON-source `INCLUDE` index and its AS-SELECT twin produce the **same plan**;
- a `VERSION` index is chosen for an ordered `__ROW_VERSION` query.

Mutation: revert D10(a) and D10(b) independently; each must turn its assertion
red.

**(e) No second constructor survives.** No reachable
`parseAggregateIndexDefinition` / `extractAggregateFromSelectElement` /
column-name-derived `buildIndexKeyExpression` path from index DDL, and no
`INCLUDE` rejection for the non-vector forms.

**(f) `just test` green**, including the `conformance_java`-tagged suite, with
§4's two probes and §3's census still green and re-pinned wherever the corpus
moved.

---

## 9. Rejected alternatives

**Keep `parseAggregateIndexDefinition`, add a parallel value-index arm.**
Smallest diff; wrong on all three axes. It leaves the three aggregate files
blocked (§1.2), entrenches a second index constructor with no Java counterpart,
and makes the ON-source convergence (D2) impossible without a third.

**Discriminate ORDER-BY-outside-the-projection on the plan's shape.** Revision
1's rule. Refuted by measurement (§4): all eight shapes are Project-over-Sort,
so it rejects 134 legal statements and cannot detect the illegal ones.

**Build the key expression from the ANTLR tree directly.** Tempting for
`select a from t order by a`. Fails on the shapes the corpus is full of: 15
multi-source FROMs need quantifier dereferencing; 3 `SELECT *` need expansion;
the ORDER-BY-subset rule needs value identity, not text. Forbidden by "no text
matching on SQL / parse trees".

**Derive the split point from the projection.** Reversed —
`IndexTest.java:673-681` and the six reorder witnesses both show the ORDER BY
fixing the key order, and `IndexTest.java:683-692` the split at `len(orderBy)`.

**Emit `VALUE` for version columns and carry the version separately.** Java
switches the whole index type at `:188`. Anything else writes different entries.

**Skip `wrapArray`.** Correct today, silently wrong the moment RFC-143 §3a
lands, and it would leave Go unable to reproduce a Java-authored key expression.

**Defer the planner work (D10).** An index created correctly and never chosen is
"infrastructure exists but SQL can't trigger it". Worse, D10(a) is a live
wrong-column-set defect reachable today from Java-authored metadata.

**Defer `__ROW_VERSION` (D6).** Makes the VERSION arm dead code and leaves 14
statements blocked behind an already-booked gap.

**Keep `INCLUDE` fail-closed.** Refuted by Java (D2) and by the ledger:
`index-ddl-aggregates-only.yamsql` is one of the 42 and declares 11 ON-source
`INCLUDE` indexes at `:70-106`, so §8(c) is unreachable while the rejection
stands.

**Land the generator before the §1.4 hotfixes.** Rejected, and the hotfix
landed first (`c058d48ee`). The generator is what makes `index-ddl.yamsql` reach
line 82; landing it first would have armed a silent wrong-index-type build in
the same commit that claims to fix index DDL. The ordering also bought the
`UNIQUE`/`WHERE` finding: probing for a fail-closed rejection is what showed
both were live on the aggregate-paired path, which no corpus file reaches.

---

## 10. Out of scope, stated

- **Vector indexes.** `VectorIndexDefinitionContext` keeps its own front end
  (`ddl.go:239-240`, `:253-296`). In Java it also reaches the generator, but
  through `OnSourceIndexGenerator` with
  `generateKeyValueExpressionWithEmptyKey = true` (`DdlVisitor.java:278`) — the
  one caller that flips that flag — plus HNSW option parsing and its own
  `INCLUDE` rejection (`DdlVisitor.java:297-298`). Converging it is a follow-on
  with its own reviewer (RFC-094), and no corpus file in §1.1 declares one.
- **`unsupported-DDL:struct` (39 files).** RFC-201 Phase 3 / CQ-69.6. §8(c)
  explicitly permits a file to move from this class **into** it, and requires it
  to be named when it does.
- **`resultMetadata:` / continuation directives.** RFC-201 Phase 2.
- **The `RowNumberWindowPredicate` / `QUALIFY` sliding-window index form**
  (`IndexTest.java:424-474`). Reaches the generator only through
  `CREATE VIEW … QUALIFY … CREATE INDEX … ON v(...)`, needs view support Go
  lacks, and no corpus file in §1.1 uses it. The predicate proto arm is ported
  as data so Java-written metadata round-trips; Go does not construct one.
- **Migrating the Go sqldriver onto the Java-compatible catalog subspace.** D11
  works around it explicitly rather than depending on it.

---

## 11. What this does not do

It does not make Go's stored descriptors match Java's for nullable arrays — that
is RFC-143 §3a; D9 pins the boundary and (b1) carves it out rather than
asserting a divergence this RFC does not close.

It does not change index maintenance or scanning at the record layer:
`KeyWithValue`, `VersionKeyExpression`, `FunctionKeyExpression` and index
predicates are all implemented and tested there. Everything this RFC adds is
upstream of them — the metadata that says which to build.

It does not add a query capability Java lacks. Every shape it enables is one
Java already accepts, and every shape Java rejects it rejects with Java's error
code and Java's message.
