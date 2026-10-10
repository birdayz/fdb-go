# Changelog

All notable changes to `fdb-record-layer-go` are recorded here. Format:
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versioning per `RELEASE.md`
(pre-1.0 `v0.MINOR.PATCH`).

**This project is pre-1.0.** The **Go API may change across minor versions**; the **FDB wire
format must stay compatible with each release's declared Java `fdb-record-layer-core` target** (the
shared-cluster hard line — see `RELEASE.md`). Every entry's **Compatibility** block answers the four
questions a user upgrading between two refs needs: wire format, SQL behaviour, FDB client option
semantics, and required dependency versions.

This changelog starts **2026-06-20**; earlier history is in `git log`. The first tagged release is
**v0.1.0** (2026-08-26). v0.1.0 shipped the `frl` CLI from a parallel nested-module tag,
`cmd/frl/v0.1.0`; from the next release `frl` is a package of the root module and ships under the
project's own `vX.Y.Z` tag, which `go install fdb.dev/cmd/frl@vX.Y.Z` resolves (`RELEASE.md`).

## [Unreleased]

### Compatibility
- **Wire format:** the Java target is upgraded to `fdb-record-layer-core` 4.14.2.0 (RFC-257). Records Java writes through `TransformedRecordSerializer` (compressed, encrypted) are
  read and written. Stores written only by an earlier pre-release Go build are not supported:
  recreate them.
- **SQL behaviour:** follows Java 4.14.2.0 where both engines run a query; see the PR for the
  per-change list.
- **FDB client option semantics:** unchanged since v0.1.0; the honored / `UnsupportedOptionError` /
  safe-no-op classification in `pkg/fdbgo/fdb/OPTIONS.md` still holds against `libfdb_c` 7.3.77.
- **Required versions:** Java `fdb-record-layer-core` **4.14.2.0**, FDB C++ client **7.3.77**, Go
  **1.26.x** (the `MODULE.bazel` / `go.mod` pins; the CI doc-guard enforces docs match them).

### Changed
- The Cascades planner plans about 2x faster with the same plans: memo deduplication rejects
  candidates on group signatures before comparing them, reuses published correlation snapshots
  instead of recomputing them, and allocates less (a 150,000-task planning run: 41 s to 17 s,
  13.3 GB to 5.6 GB allocated).
- Go API moves, following Java's packages: index-predicate normalization is
  `pkg/recordlayer/indexpredicate` (was in the cascades planner), `FinalizePlan` and the plan
  walkers are in `pkg/recordlayer/query/plan/plans`, and the R-tree is `pkg/async/rtree`
  (Java's `async.rtree`). `pkg/recordlayer` no longer imports the planner.
- **Breaking (storage):** the relational key space is Java's `RelationalKeyspaceProvider` layout byte for byte. The catalog store is `(NULL, NULL, 0)`; a schema's record store is `(domain, database, schema)`, where the domain is a directory of the FDB directory layer and the database and schema names are interned in the domain's interning layer. Go and Java open each other's databases and schemas in a shared cluster. Data written by earlier Go builds is not migrated: recreate it.
- `pkg/fdbgo/fdb/directory` runs on any `fdb.WritableTransaction` (the libfdb_c backend and the simulator included; `UnsupportedBackendError` is gone) and `NewDirectoryLayerWithRandom` injects the prefix allocator's random source.
- **Breaking (storage/metadata):** a VECTOR index's `hnswMetric`/`vectorMetric` must name one of Java's `Metric` constants (`EUCLIDEAN_METRIC`, `EUCLIDEAN_SQUARE_METRIC`, `COSINE_METRIC`, `DOT_PRODUCT_METRIC`); the lower-case `cosine`, `inner_product` and `euclidean` are refused when the meta-data is built (`MetaDataError` "incorrect index options"), as in Java. Recreate an index that used them.
- **Breaking:** a relational database path is `/DOMAIN/DATABASE`, as in Java, and its domain must be registered once per process before use: `sqldriver.RegisterDomainIfNotExists("FRL")` (Java's `RelationalKeyspaceProvider.instance().registerDomainIfNotExists`). The driver registers none; `frl` registers `FRL`, as Java's server and CLI do. A path outside the registered domains (including a one-segment `/name`) is INVALID_PATH (08F01) on CREATE DATABASE, on a schema's store and when connecting to a schema; a malformed path is 08F01 `invalid database path '…'` (was 22023). `/__SYS` needs no domain.
- An array result column's `DatabaseTypeName` is `ARRAY`, as Java's `getColumnTypeName` (it was the element's type name), and its scan type is not a scalar. `api.WithResultSetMetaDataObserver` gives a query's result-set metadata, whose `ColumnDataType` carries a struct column's declared type name and fields and an array column's element type.
- A SQL query block is one Select, as in Java 4.14.2.0: predicates reach a derived table's or CTE's access paths, a computed column without an alias is named by its position (`_0`), and EXPLAIN shows `Map(…, {…})` where it showed `Project(…)`.
- A SQL function call binds its arguments as Java does (a one-row values source pushed into the body), so its body's predicates reach index scans and joins of calls plan in Java's order.
- A WHERE over a LEFT JOIN with a projected EXISTS reading the null-supplied side, and an EXISTS over a correlated array of scalars under a join, answer instead of failing.
- Fixed-factor union planning uses fewer temporary allocations in boolean normalization and memo matching.
- A recursive CTE's column list names its columns only for the query that reads the CTE; the recursive branch reads the seed's own names, as in Java 4.14.2.0.
- A recursive CTE keeps the seed's column types and nullability for every iteration, as in Java 4.14.2.0; a recursive row that does not fit (a NULL into a NOT NULL column, another type) is refused with XXXXX.
- A SQL function call and a recursive CTE name a repeated or unnamed column by its position (`_0`, `_1`, …), as in Java 4.14.2.0; a SQL function whose name is not ASCII can be called.
- `UPDATE … RETURNING` and `DELETE … RETURNING` answer the modified rows through `Query`, as in Java 4.14.2.0 (an UPDATE's `"old"` and `"new"` records); on `Exec` such a statement is refused with 42F61, and a statement without a result set on `Query` with 02F01, before it runs.
- A boolean ARRAY parameter binds; a join comparing two rows' `__ROW_VERSION` through a version index runs.
- SQL accepts EXISTS inside AND/OR/NOT boolean expressions in WHERE and INNER JOIN ON, matching Java's one-row existential witness semantics.
- An EXISTS subquery whose own source reuses an outer name reads its inner source, as in Java 4.14.2.0 (Java's inner shadow). Shapes Go declined with 0A000 now answer: an inner source named like an outer source or an outer UNNEST frame, an early JOIN ON naming an alias a later inner leg reuses (the ON resolves left to right, so the later leg does not capture it), NOT EXISTS or a projected EXISTS over a middle subquery whose outer-only conjunct sits beside a nested EXISTS, an outer reference in an OUTER join's ON (or in an inner ON before a later RIGHT join) inside the subquery, which stays in its ON, and a correlated UNION ALL body, whose branches keep their correlations.
- A parenthesised statement (`(SELECT …)`, `((SELECT …) UNION ALL SELECT …)`) is the query it encloses, as in Java; it was 0AF00 `could not build logical plan`. Java's corpus files `union.yamsql` and `union-empty-tables.yamsql` pass.
- A SELECT with several projected EXISTS, or a projected EXISTS beside a WHERE (or ON) EXISTS, answers as Java; it was 0AF00 `Cascades planner could not plan query`. Java's corpus file `exists-in-select.yamsql` passes.
- A qualified star over an enclosing query's source (`… EXISTS (SELECT B.* FROM A WHERE …)`) expands to its correlated columns, and a qualified star beside other items under GROUP BY (`SELECT A.*, A1 … GROUP BY A1, A2, A3`) is expanded and checked column by column, as in Java (was 42703 / 42803).
- IN-list type errors follow Java: a list of literals of different types (`IN (1, 2.5)`, `IN (1, 3000000000)`) is 42804 `Elements of array literal are not of identical type!`; a computed list promotes its items (`IN (2 + 1, 2.5)` answers) and one that cannot is 22000; a probe the list's element type cannot take (`n IN ('a', 'b')` over a BIGINT) is 22000 (was 42804). A comparison is an IN item (`d IN (3 < 4)`, was 0AF00).
- A JOIN on a comma-separated FROM source (`FROM t1, t1.refs AS r JOIN t2 ON r = t2.id`, `FROM a, b JOIN c ON …`) is accepted, as in Java; it was 0A000. A RIGHT or FULL join there stays refused. Java's corpus file `right-deep-plan-tests.yamsql` passes.
- A WITH nested inside a recursive CTE's body (`WITH RECURSIVE x AS (WITH RECURSIVE y AS (…) SELECT … FROM y UNION ALL …)`) runs, as in Java; it was 0AF00. Java's corpus file `recursive-cte.yamsql` passes.
- SQL over `/__SYS?schema=CATALOG` reads the catalog's tables `SCHEMAS`, `DATABASES` and `TEMPLATES`, as in Java: the catalog's schema template is built from table definitions, so its stored metadata is Java's (was 42F01 / `no schema metadata available`). An invalid table or column name in CREATE TABLE is 42602 before its types or primary key are checked. Java's corpus files `catalog.yamsql` and `create-drop.yamsql` pass.
- A table of metadata loaded from Java whose record-type name uses a non-canonical escape (`___T6__2__UNESCAPED`) answers to its decoded name (`"___T6.__UNESCAPED"`) in every statement, as Java's template keys tables by decoded name. Java's corpus file `valid-identifiers.yamsql` passes.
- Saving a record larger than a single value without split long records fails with Java's RecordCoreException text `Record is too long to be stored in a single value; consider split_long_records`, reported as XXXXX (was an unmapped error). Java's corpus file `large-record-fails.yamsql` passes.
- A FROM name that is no table, view or CTE but names an enclosing query's FROM item (`… FROM t AS x WHERE EXISTS (SELECT 1 FROM x …)`) reads that item's table or CTE again, as Java's findCteMaybe does (was 0AF00).
- In a block with GROUP BY or aggregates, a QUALIFY naming a column that is not a grouping key (an aggregate's operand included) is Java's 42703 "Attempting to query non existing column X": Java resolves QUALIFY against the aggregate's output. Go answered 42803 or 0AF00.
- An EXISTS in HAVING is refused with Java's 42803 "Invalid reference to non-grouping expression", after the subquery resolves (an ambiguous reference inside it stays 42702); Go refused it with a planner 0AF00.
- QUALIFY in a block with GROUP BY or aggregates filters the aggregate's output, conjoined with HAVING, as in Java; Go applied it to the input rows (`SELECT COUNT(*) FROM a WHERE … QUALIFY 1 = 0` answered `[0]` where Java answers no rows). A correlated QUALIFY inside EXISTS is answered instead of refused with 0AF00. An aggregate call in QUALIFY, where Java fails internally, is answered by Go (DIVERGENCES.md).
- An EXISTS whose grouped body has a HAVING that reads the enclosing row (`EXISTS (SELECT a2 FROM a GROUP BY a2 HAVING COUNT(*) > b.b1)`) is answered as in Java; Go refused it with 0AF00.
- A star over an enclosing source in a grouped select list (`EXISTS (SELECT A.*, B.* FROM A GROUP BY A1, A2, A3)` with `B` from the outer query) is answered as Java answers it; Go refused it with 0A000. Java's corpus file `select-a-star.yamsql` passes.
- A recursive CTE's self-reference can be read inside an EXISTS of its own recursive leg (`FROM r AS c, e … WHERE NOT EXISTS (SELECT … FROM c …)`); it reads the previous level's rows, as in Java. Go refused it. Java's corpus file `documentation-queries/with-documentation-queries.yamsql` passes.
- Names that differ only in case are distinct, as in Java. Quoted columns `"COLUMN"`, `"column"` and `"cOLumN"` in one table are accepted (Go refused them at CREATE) and an INSERT keeps each value in its own column (it used to fold them onto one). Tables `"Table1"` and `"TaBlE1"` each resolve to themselves, in queries and in index and function bodies. Under CASE_SENSITIVE_IDENTIFIERS a user scalar function call keeps its case (`f3` beside `F3`). Java's corpus file `keyword-case-insensitivity.yamsql` passes.
- The `CASE_SENSITIVE_IDENTIFIERS` connection option is honoured: an unquoted identifier keeps its case, as if quoted, as in Java (Go ignored the option). Tables whose names differ only in case resolve exactly in the semantic catalog. Java's corpus file `setup-with-connection-options.yamsql` passes.
- An IN list item that fails to evaluate (`a IN (1 / 0, 3)`) raises when the query runs (22012), even over an empty table, as Java's does; it was 0AF00 `Cascades translation failed`.
- An unknown column is 42703 `Attempting to query non existing column X`, Java's text, with the reference as written (`T.X`, `Q.ID` for an unknown qualifier) in every clause and in UPDATE's SET; a qualified star over no source and a JOIN USING column missing from either side are `Unknown reference X`.
- A query block reports its faults in Java's clause order: a join's ON first, then the WHERE, then the select list (an unknown function in the select list included).
- `frl` is a package of the root module and releases under the project's `vX.Y.Z` tag.
- SQL `LIKE` follows Java 4.14.2.0: wildcards cross newlines, `LIKE NULL` is allowed, and invalid escapes raise 22019/2200B/22025 per row.
- SQL comments follow Java 4.14.2.0: block comments nest, an unterminated one is 42601, and `#` is no longer a comment.
- `OPTIONS (...)` is statement-level only and adds `PLAN RIGHT DEEP` and `ISOLATION LEVEL SNAPSHOT`; `PLAN` and `DEEP` are reserved words.
- A typed, parenthesised or column NULL IN-list item is 0A000 when the list is evaluated, as in Java 4.14.2.0 (a bare NULL stays 42809).
- An ARRAY element is never NULL: a NULL array element is refused with 0A000 as in Java 4.14.2.0 (literal-array `=`/`<>`/`IS DISTINCT FROM` comparisons still accept one).
- Driver parameters are bound as typed constants (int32 INT, int64 LONG, slices ARRAY, uuid.UUID UUID), not spliced into SQL text; named `?x`/`$x` and `IN ?` are supported, extra arguments are ignored and a missing one is 42F02.
- A NULL-strict expression over NULL is NULL when a predicate is simplified, as in Java 4.14.2.0: `WHERE COALESCE(NOT CAST(NULL AS BOOLEAN), TRUE, 1 / 0 = 1)` answers every row (was 22012), and EXPLAIN shows `CAST(NULL AS T)` as `NULL` and a value `NOT x` as `NOT x`.
- `COALESCE`/`GREATEST`/`LEAST` follow Java 4.14.2.0: at least two arguments, all-NULL and BYTES arguments are 22F00, `COALESCE` evaluates every argument, and results are NOT NULL when Java's are.
- The planner removes duplicate expressions from its memo (Cascades duplicate detection): multi-way joins plan with far fewer tasks, and a six-table join chain or a hub joined to five spokes now plans within the default budget.
- A join of two EXISTS subqueries no longer repeats rows: each existential contributes at most one witness row.
- A lateral-unnest chain answers a WHERE reading any link's element, a WHERE [NOT] EXISTS over the chain and a nested EXISTS reading its last element; an EXISTS beside an unnest of a STRUCT array may read the unnested table (all previously 0AF00).
- A FROM item reading a lateral-unnest chain's element, a chain link separated from its owner by another FROM item, unnests of several tables in one FROM, and an unnest of a lateral derived table's array column now plan (previously 0AF00); source reordering preserves indirect lateral dependencies, including inside correlated subqueries.
- An `AT` unnest's element or ordinal read beside another FROM item no longer fails at execution.
- Multi-table `[NOT] EXISTS` subqueries can read an outer unnest's element and ordinal (previously 0AF00).
- Join planning costs a right-deep foreign-key chain as its left-deep re-association, and splits a range predicate spanning several joined tables per table so each keeps its selective probe.
- A record-layer failure no SQL mapping claims is SQLSTATE XXXXX, as Java's `ExceptionUtil` maps an unclaimed `RecordCoreException` (previously no SQLSTATE).
- `ISOLATION LEVEL SNAPSHOT` admits only SELECT and EXPLAIN/DESCRIBE of a SELECT; DDL, SHOW and DML are 0A000, and a DML plan is refused at SNAPSHOT before it reads, as in Java 4.14.2.0.
- An ungrouped query's COUNT is 0 over no rows and NOT NULL (Java's `adjustCountOnEmpty`), in the select list and HAVING; a HAVING-only aggregate query may project constants (was 42703).
- SQL text or a string parameter that is not valid UTF-8 is refused with 22021 (previously its bad bytes were replaced by U+FFFD); a record holding such a string is refused on save with `InvalidUTF8StringError` (22021 through SQL), including an update of a record stored that way, until the string is repaired. Bound byte and VECTOR parameters are copied at bind time.
- A bound `time.Time` is the TIMESTAMP value of its instant, its UTC text to the second (sub-second precision is dropped); it was the date text when the value was midnight in its own zone, so `2024-01-01T00:00+02:00` bound as `'2023-12-31'`. Into a DATE-spelled column it now stores `'2024-01-01 00:00:00'`, and `WHERE d = ?` compares that text; bind a day as `CAST(? AS DATE)` (the instant's UTC day). A time whose UTC year is outside 0000-9999 is refused with 22008 naming the parameter. `[]time.Time` binds as ARRAY<TIMESTAMP>. DATE and TIMESTAMP result columns scan as `string`.
- A DATE meeting a TIMESTAMP in a comparison, an IN list, CASE, COALESCE, IFNULL, GREATEST or LEAST is promoted to its midnight, so `CAST('2024-01-01' AS DATE) = CAST('2024-01-01 00:00:00' AS TIMESTAMP)` is TRUE (was FALSE, comparing text); GREATEST/LEAST accept DATE and TIMESTAMP values (were 22000/22F00). A DATE- or TIMESTAMP-spelled column is STRING and still compares text.
- `CAST(string AS DOUBLE/FLOAT)` is Java's `Double.parseDouble`/`Float.parseFloat`: a signed `NaN`, `+Infinity`, the `d`/`D`/`f`/`F` suffixes and out-of-range magnitudes (`1e400` is Infinity) are accepted, `nan`, `inf`, `infinity` and `1_000` are refused (22F3H, Java's message), FLOAT rounds once to binary32, and a NaN is Java's canonical NaN, so Go and Java write the same record bytes and index key for it (Go wrote `0x7ff8000000000001`, which Java's index probe missed and a UNIQUE index admitted beside Java's NaN).
- `EmbeddedConnection.SetOption` (reach it through `sql.Conn.Raw`) sets one connection option, checked against Java's option contracts (22023 for a wrong type or range), and lasts until the connection returns to the pool; `SetOptions` replaces the whole set. The DSN accepts `dry_run` and `isolation_level_snapshot`, and an unknown DSN parameter is now an error (22023) naming it and listing the accepted ones (it was ignored). A result set keeps the options it started with across its pages.
- Distance functions, `ROW_NUMBER`, `RANK` and the record-type, incarnation and CASE-selector values are typed nullable, as their Java classes are (each could evaluate to NULL while typed NOT NULL); a result column over one reports nullable.
- Record layer: the Cascades planner enforces Java's REWRITING prune. A group crosses into PLANNING only as its one winning final, and REWRITING costs alternatives over single-final children. Either violation is now a planner error (`RewritingCrossingError`, `RewritingPruneError`) where a group used to carry several members across. A union whose legs merge into a group that had not yet been explored plans again; that shape could previously leave the union unfinalized.
- Record layer: an index filtered by a Go closure (`Index.SetPredicate`) no longer answers queries. Its entries cover only the records the closure admits. An ordered full scan, a range or an equality probe of it returned only those records, so rows were silently missing (`SELECT email FROM t ORDER BY email`). Queries now read the base records; the index is still maintained.
- Record layer: the Cascades planner's comparator and the rule calls' `CostModel()` rank with the planner context's configuration and metadata when no statistics are set, as Java's rule calls do; they used to rank as if the context were empty. A SQL self-join's inner leg can now probe an index where it read a primary-key range (`ON a.did = b.did WHERE a.eid < b.eid`). Record-layer callers who hold a continuation across this change, or across the IN-union changes below, should restart paged queries: a continuation minted by a plan that changed is refused when the new plan's outermost cursor is an index or ranged scan, and is not detected (as in Java) when it is an unfiltered primary scan or an in-join reading an in-union's continuation.
- Record layer: an IN-union ranges over every plan of its ordering partition, as Java's does, instead of one pinned plan. A merge over a non-covering index therefore reads covering entries and fetches above the merge (`Fetch(InUnion(IndexScan(…, COVERING)))`, Java's `INUNION … | FETCH`). A keyed set operation now pushes below its fetch when every leg covers its comparison keys; before, those keys never translated. An extracted in-union whose child does not provide its merge order fails planning with `InUnionChildOrderingError`, wrapped as a `PlannerInvariantViolationError`. SQL plans in the corpus are unchanged, because the sorted IN-join still wins where both apply.
- An `IN (…)` with no ORDER BY is never an IN-union merge, as in Java 4.14.2.0, whose rule skips an unordered request: it is an IN-join (`WHERE o.id IN (2, 3) AND NOT EXISTS (…)` was `InUnion(FlatMap(…))`). Record layer: `plans.NewRecordQueryInUnionPlan` takes the in-union's size as its last argument (`plans.UnboundedInUnionSize` for no limit), `NewRecordQueryInUnionPlanWithBindingAliases` is removed in favour of `NewRecordQueryInUnionPlanWithBindingAliasesAndMaxSize`, and `ImplementInUnionRule` fails a rule call that has no planner context instead of assuming size 0.
- An IN-union merge refuses more than 24 child executions, the product of its IN lists' distinct sizes, with XXXXX "too many IN values" before it reads, as Java 4.14.2.0's relational layer does (`IN` of 25 values, or 5 × 5 over two lists, ordered so that only a merge serves it). Record layer: an in-union takes its maximum from `PlannerConfiguration.AttemptFailedInJoinAsUnionMaxSize`, whose default is Java's 0, so a planned in-union over any IN list now fails with `RecordCoreError` "too many IN values" unless the caller raises it; a hand-built `NewRecordQueryInUnionPlanWithBindingAliases` plan is unbounded.
- `col IN (…) ORDER BY id` over an index that does not name `id` in its key plans no IN-union merge, as Java 4.14.2.0 builds none: the per-value index reads are sorted (`InMemorySort(InJoin(…))`), rows unchanged. A relational index entry holds `id` after its record type, and Java's index match stops at that record type. An index that names `id` (`CREATE INDEX … ON t (col, id)`) keeps the merge, and `col = ? ORDER BY id` still reads the index without a sort (DIVERGENCES.md).
- Record layer: `plans.NewRecordQueryExplodePlanWithOrdinalityBase` and `expressions.NewExplodeExpressionWithOrdinalityBase` build an ordinality explode numbered from 0, as Java's three-argument constructors do; SQL `AT` stays 1-based.
- `col IS NOT DISTINCT FROM x`, with either operand first, is an index or primary-key scan bound, as in Java 4.14.2.0 (#4598). `IS NOT DISTINCT FROM NULL` reads the null key (it was a full scan with a filter). A sparse `WHERE col IS NOT NULL` index serves it only for a non-NULL literal or bound value. EXPLAIN shows the bound as `≡` (`IndexScan(I2, [≡])`). Record-layer callers: a continuation from the old full-scan plan is refused by the new index plan, so restart such paged queries across this change.
- `COALESCE`, `GREATEST` and `LEAST` promote every argument to the common type, as Java's `VariadicFunctionValue` does: `COALESCE(int_col, double_col)` returns a DOUBLE value when the INTEGER argument is chosen (it returned the integer), and EXPLAIN shows the promotions (`COALESCE(_current.A#1, PROMOTE(0 TO BIGINT))`).
- A projected `AND`, `OR`, `NOT` or `EXISTS` is a nullable BOOLEAN column whatever its operands, as in Java 4.14.2.0 (`SELECT NOT FALSE` and `SELECT TRUE AND TRUE` reported NOT NULL).
- `OPTIONS (LOG QUERY)` and the `LOG_QUERY` connection option set `PlanGenerationInfo.LogQuery` on the statement's planning record (they were parsed and ignored). Java logs such a record at INFO, as it logs a slow one.
- An explicit transaction no longer aborts (40001) because an index it did not scan changed state: planning and plan revalidation read index states without a read conflict, as in Java; a scanned index's state change still conflicts. `FDBRecordStore.GetAllIndexStates` conflicts on the whole index-state subspace, as Java's `getAllIndexStates`; `PeekIndexStates` reads without one.
- A NaN equality on a float index or primary-key column (`d = CAST('NaN' AS DOUBLE)`, a bound NaN, `d IN (NaN, …)`) uses the index and returns every stored NaN (it used a full scan, and an IN-join over the index failed with "exact indexed NaN equivalence is unsupported"), including when later index columns are also bound (`d IN (NaN) AND g IN (1, 2)`), which are then checked per index entry. An ORDER BY over a later index column sorts.
- `MIN`/`MAX` over DOUBLE and FLOAT return the NaN operand with its own bits, as Java's `Math.min`/`Math.max` (Go returned `0x7ff8000000000001`).
- A statement re-run with a different bound value no longer reuses the plan built for the first: the plan-cache key renders BOOLEAN values (`true` and `false` shared one plan, so `… AND ?` bound `false` returned the rows of `true`), NaN payloads and signed zeros exactly.
- `CAST(… AS DATE)` reads every text `CAST(… AS TIMESTAMP)` reads (RFC 3339 with an offset included, surrounding whitespace trimmed), and both refuse a UTC year outside 0000-9999 (22F3H); the date-part functions trim too (`YEAR(' 2024-01-01')` is 2024, was 22023).
- Driver parameters: a `*uuid.UUID` binds as UUID (a nil one as NULL; it panicked), `[N]byte` and named byte slices as BYTES, database/sql null wrappers by their payload (`sql.NullInt64` is LONG), and an array from its static element type (an empty `[]int` is the untyped empty array); a nested array or unsupported type is 22023 naming the parameter.
- A SUM or COUNT aggregate index answers alone, as Java 4.14.2.0's does: a group whose rows were all deleted or moved away reads 0, a group whose values are all NULL has no SUM or `COUNT(col)` row, and a group whose last non-NULL value was removed reads SUM 0. The DDL no longer adds a `<index>__GROUP_COUNT` index beside a grouped SUM or `COUNT(col)` index, so Go-created metadata holds exactly the declared indexes; recreate schemas an earlier build created. An ungrouped SUM or COUNT index serves the ungrouped aggregate, and an index name ending in `__GROUP_COUNT` is accepted.
- A table whose name escapes (`"MY$TABLE"`, `"foo.table$nested"`) plans primary-key ranges, secondary-index and aggregate-index scans as any other table does (previously a full scan with a residual): the plan carries the record type's stored name, and EXPLAIN shows the SQL name, as Java 4.14.2.0 does.
- A column whose name escapes (`"c$1"`, `"i.d"`) plans as any other column: its indexes and primary key are used for ranges and ORDER BY, aggregate indexes over it answer, and `CREATE INDEX … ON t ("c$1")` is accepted (previously 42703).
- An aggregate index serves a coarser grouping than its own, as Java 4.14.2.0's does: the query's grouping columns in any order, a grouping column bound by an equality (`SELECT SUM(v) FROM t WHERE g = 1` over an index grouped by `g` is a point read), and otherwise a roll-up of the SUM, COUNT or MIN/MAX_EVER index to its matched grouping prefix (`WHERE g > 0`, or `GROUP BY a` over an index grouped by `a, b`).
- A table whose quoted lowercase primary-key or vector column names differ from their upper-case spelling gets primary-key scans and vector index plans (previously a full scan, or 0AF00).
- A vector query without `OPTIONS EF_SEARCH` searches with Java's default, min(max(4k, 64), max(k, 400)), instead of 200.
- **Breaking (behaviour):** `FDBDatabase.Run`, `RunWithWeakReads`, `RunWithVersionstamp`, `RunRead` and `FDBDatabaseRunner.RunWithRetry` retry as Java's transaction runner does: at most 10 attempts (`FDBDatabase.SetMaxAttempts`, `SetRetryDelays`), each a fresh transaction whose backend retry limit is 0, retried while any cause in the error is retriable, with Java's randomized exponential delay between attempts (recorded as `EventRetryDelay`). They used to retry without bound. Weak reads apply to the first attempt only. A context that ends between attempts returns an error wrapping both the context's error and the last attempt's. A transactor implementing `AttemptTransactor` receives each attempt's identity; `SetAttemptObserver` reports each attempt. The online indexer bounds its transactions with `OnlineIndexerBuilder.SetMaxAttempts` (default the database's). A SQL DDL statement runs in one attempt, as in Java: a conflicting DDL fails with 40001 instead of re-executing. An SPFresh write that meets a split in progress (`SPFreshSplitWindowError`, a retryable 1020) does not use up attempts; SPFresh's background lifecycles keep the client's unbounded loop (DIVERGENCES.md).
- **Breaking (behaviour):** a body that commits the context `Run` (or a variant, or `RunWithRetry`) handed it is refused before anything commits, with `RecordContextNotActiveError` ("Transaction is no longer active.", a `RecordCoreStorageError`); `Run` commits. A context refuses a second commit the same way.
- Pure-Go client: a commit made while no commit proxy is known waits for the proxy set to change (bounded by its context) and then fails with `commit_unknown_result` (1021), as libfdb_c does. The Go-only error 1200 (`ErrAllProxiesUnreachable`) is gone, and code 1200 now has its canonical FDB name, `recruitment_failed`.
- `fdb.Database.TransactCtx` / `ReadTransactCtx` and the tenant forms return the error the transaction body returned (or panicked with), chain intact, when the retry loop gives up on that error's code, as the Apple binding does; they returned a bare `fdb.Error{Code}`.
- HNSW graphs are written byte-for-byte as Java writes them, for the same inserts and deletes:
  - a pruned neighbour list keeps its survivors in list order;
  - a list at exactly mMax is pruned;
  - a prune never extends candidates;
  - a delete repair prunes every candidate and samples at Java's rate;
  - a re-inserted neighbour moves to the end of its list;
  - a layer search starts from its entry's stored vector;
  - a pairwise distance between an encoded and a plain vector is a RaBitQ estimate.

  Graphs written by earlier Go builds stay readable but differ in neighbour lists.
- GuardiANN on a trained index with an extra-bit count RaBitQ cannot use refuses only where Java constructs the quantizer (an insert of a new key, a search, a task write or task body); a delete that queues nothing still runs.
- HNSW and GuardiANN cosine distance is Java's: a zero vector is at +Inf and a non-finite input is NaN, with no clamping, and distances order as Java's `Double.compare`. HNSW node caches live for one operation, a vector search read-locks its partition while it searches, and HNSW sample keys carry a tuple UUID as Java's do (older entries are still read). SPFresh keeps its clamped cosine.
- The plan cache is shared by every database and schema of one schema template, as Java's `RelationalPlanCache` is, and stored queries are planned into it when a connector starts.
- Planner: disabling the rule name `ConditionalCascadesRule` (`DISABLED_PLANNER_RULES`) turns off every conditional rule chain, as in Java; it was ignored.
- An IN-union over an IN list whose items planning cannot evaluate evaluates them when the query runs, as Java does; it failed with "no planning-time values".
- **Breaking (API):** `keyspace.FDBResolver` is removed; it had no users. The resolver interface and `ResolverDirectory` remain.
- GuardiANN vector indexes refuse malformed stored values instead of panicking, check the deferred insert cap before writing anything, poison the transaction when a task fails after its removal, and merge or drain every requested index even when an earlier one fails.
- Decimal literals parse as Java's `ParseHelpers.parseDecimal` wherever they stand: a dotless exponent (`1e5`) or an out-of-width integer is XXXXX `For input string: "…"`, and an overflowing `1.0e400` is an infinity (previously 22003 or 0AF00).
- A record constructor's field takes its element's own name (`SELECT (x, y)` is `{X, Y}`, `(x + 1)` is `{_0}`, a repeated name falls back to the position), so a derived record's fields can be read by name; INSERT … SELECT, UPDATE and COALESCE bind a record to its struct type by position, ignoring element names, and an INSERT VALUES element naming another field is XX000, as in Java 4.14.2.0.
- Scalar macro calls take named arguments (`f(b => 1, a => 2)`), bound by name with the declared defaults; a repeated name is 42601, and an unknown name, a missing argument without a default or too many arguments is 42883 `could not find function '…'` (also for table functions), as in Java 4.14.2.0. A macro may take or return a struct type no table stores.
- A window's `OPTIONS EF_SEARCH` accepts `L`/`I` suffixes, refuses a repeat (22F00) and a value beyond int (22000), and an HNSW search uses it as given: below k it returns fewer rows, as in Java 4.14.2.0.
- The top-level query is a sort over its block, as in Java 4.14.2.0: EXPLAIN shows fewer `Map` operators, index scans that hold every projected column are covering, and an IN list over an unindexed column runs as Java's per-row explode.
- A disjunction over a key (`id = 1 OR id = 3`, `NOT BETWEEN`) merges its legs' ordered scans as Java does, without a sort.
- An `IN (…) ORDER BY` merge keeps rows that tie on a projected sort key (Java drops them), and an IN-union over several IN lists runs every combination.
- `col IN (…) ORDER BY col` runs as Java's sorted IN-join with no sort, ascending, descending or followed by the rows' own order in either direction; over a non-covering index the IN-join runs under the fetch where Java merges an IN-union (DIVERGENCES.md).
- A WHERE conjunct on a LEFT JOIN's preserved side narrows that side's scan (a key equality is a point lookup), a conjunct rejecting the null-extended row turns the join inner, and an EXISTS over the null-supplied side runs inside its join, as in Java 4.14.2.0.
- A version index is never a covering scan, as in Java 4.14.2.0: `ORDER BY` a column with ties returns them in primary-key order through the plain index.
- A `COALESCE` folds while planning only when its first argument is NULL or a BOOLEAN literal, as in Java 4.14.2.0, so its other arguments are evaluated as written: `WHERE COALESCE(1, 1/0) = 1` raises 22012 (it answered every row).

- Runnable SQL/typed-store quickstarts check errors; default cluster lookup follows C++ precedence and routine SQL warm-up logs only at Debug.

## [v0.1.0] - 2026-08-26

### Added
- **Record-layer metrics exporter** (`pkg/recordlayer/rlmetrics`): `StoreTimer` (the port of Java's
  `FDBStoreTimer`) rendered in the Prometheus text exposition format under `fdb_recordlayer_`, with
  zero new dependencies — the record-layer counterpart to `pkg/fdbgo/fdbmetrics`. Timed events are
  summaries in seconds, counts and byte totals are counters. `recordlayer.Event` gains a `Kind`
  (timed / count / size) porting Java's `Event`-vs-`Count(isSize)` split, and `StoreTimer` gains
  `Add` and `KeysAndValues` from Java. Reachable from the SQL layer via
  `FDBDatabase.SetTimer` or, for `database/sql`-only deployments,
  `sqldriver.EnableStoreTimer(clusterFile)` — one timer per cluster-file key (per process), with no
  per-tenant label by design; see `docs/mt-saas.md` §4 for the cardinality reasoning.

- **Multi-tenant SaaS operator guide** (`docs/mt-saas.md`): the tenancy model (one database path per
  tenant, subspace-per-database, and why native FDB tenants are not used), the trust boundary and
  `RESTRICT_DDL_TO_SESSION_DATABASE`, the five per-statement quotas and how to arm them (none is
  DSN-settable), per-tenant observability, the pure-Go TLS certificate requirement, and the
  tenant-facing SQL contract. Every claim carries a `file:line` citation; the page is on
  `pkg/docscheck`'s `livingDocs` list so its version anchors are drift-guarded.
- Statement-wide **memory byte budget** for the SQL executor: opt-in `OptMaxStatementMemoryBytes`
  bounds every cardinality-growing buffer by bytes (not just the 100k-row `MaterializationLimit`);
  breach → SQLSTATE `54F01`. Default `0` = unlimited (RFC-130).
- A documentation-consistency CI guard (`pkg/docscheck`) that fails the build if a living doc drifts
  from the `MODULE.bazel` / `go.mod` version pins or reintroduces a known contradiction (RFC-131/132).
- A public **FDB client option matrix** (`pkg/fdbgo/fdb/OPTIONS.md`) classifying every `Set*` option
  as honored / `UnsupportedOptionError` / safe no-op, each with its `libfdb_c` 7.3.77 reference, plus
  a completeness guard that fails CI if an option is added without a matrix row (RFC-133).
- A **panic-boundary release gate** (RFC-134): a `norecover` nogo analyzer fails the build if a
  `recover()` is added outside the documented panic→error boundary allowlist (`docs/panic-audit.md`
  §2), plus docscheck guards that keep the four input-boundary fuzz nets wired and the doc in lockstep
  with the allowlist. Makes the "untrusted input → error, never crash" discipline self-enforcing.

### Changed
- **FDB C++ wire-protocol baseline bumped 7.3.75 → 7.3.77** (RFC-152). Patch bump within the 7.3
  line: no wire-format, error-code, `ClientKnobs`, RYW, or serialization change (regenerated
  `pkg/fdbgo/wire/types/*_generated.go` differ only in the version-string header comment). The only
  client-relevant upstream delta is PR #12935 (a `peer->disconnect` arm in `waitValueOrSignal` so
  `loadBalance` fails an in-flight request immediately on peer disconnect instead of waiting out the
  failure-monitor lag); the pure-Go client already has this structurally (single-owner connection:
  `readLoop` EOF → `failConnection` → `failAllPending`), pinned by
  `TestPeerDisconnect_FailsInFlightReplyImmediately`. No production-code behaviour change.
- SQL `LIMIT`/`OFFSET` now flows through a single uniform `RecordQueryLimitPlan` + continuation
  envelope, including for nested derived tables (RFC-128).

### Fixed
- **Record-layer instrumentation was measuring the wrong things, or nothing at all.**
  `Counter.Increment` wrote its amount into the cumulative-value field as well as the count, so every
  count event reported a bogus duration (Java's `Counter.increment` touches `count` alone). The scan
  events timed cursor *construction* rather than each record produced, which for a lazy cursor
  measures an allocation and reports one occurrence per scan regardless of size (Java instruments the
  cursor's `onNext`). Scan events also sat on entry points the query engine does not use, leaving the
  executor's record scans, aggregate/group index scans, vector scans and primary index scans
  uncounted. And `EventCommit` was recorded only by `CommitWithVersionstamp`, so every commit on the
  SQL path — autocommit and explicit `BeginTx` alike — went unrecorded. Instrumentation only; no
  wire-format, plan or result change.
- **Legacy Java store layouts are now fully readable, writable, and auto-upgraded** — closes the
  `FormatVersion` < 6 / `omit_unsplit_record_suffix` wire-compatibility gap that was previously a
  *silent* data-correctness bug (Go accepted an old-format store header but only understood the modern
  inline layout, so it would silently fail to see a legacy store's record versions and unsplit
  records). Go now mirrors Java's `FDBRecordStore.useOldVersionFormat()` end-to-end:
  record versions are read/written in the separate `RecordVersionKey(8)` subspace for stores below
  `SAVE_VERSION_WITH_RECORD` (format 6), and unsplit records are read/written at the bare primary key
  (no `0` suffix) when `omit_unsplit_record_suffix` is set — across load, scan, `scanRecordKeys`,
  `recordExists`, save, update, delete, and `deleteRecordsWhere`. On open, Go also performs Java's
  transactional format upgrade (`checkRebuild`/`addConvertRecordVersions`): it bumps the stored
  `FormatVersion`, sets `omit_unsplit_record_suffix` for a non-splitting store created before format 5,
  and moves versions from subspace 8 to their inline `pk + -1` location when upgrading a splitting
  store past format 6. Pinned by FDB integration tests that lay down each legacy layout and assert
  byte-level read/write/scan/delete and migration parity with Java. (Closes the `TODO.md` "no read
  path for format-version-<6 record versions / unsplit records" gap surfaced by the RFC-131 audit.)
- SQL pagination no longer treats a non-terminal `StartContinuation` as end-of-results (a latent
  early-truncation bug); exhaustion is decided off `IsEnd()`, not byte-emptiness (RFC-127).
- Pure-Go FDB client: `Get`/`GetRange` read-conflict ranges are clamped to the data actually returned
  and filtered through the RYW overlay, matching `libfdb_c` (no under-conflict; RFC-121).
- `go test ./...` is clean from a fresh checkout: the Bazel-runfiles-only suites are build-tagged so a
  plain `go test` no longer panics (RFC-129), and the heavy million-row stress benchmarks under
  `pkg/relational/sqldriver/stress` now carry a `stress` build tag, so a plain `go test ./...` skips
  them instead of spinning up million/ten-million-row FDB workloads (they still run via Bazel's
  `manual` stress target and the nightly stress workflow).
- Pure-Go FDB client: three **database-level transaction defaults** that change read semantics are now
  honored instead of silently dropped — `SetSnapshotRywDisable`/`Enable` (a cumulative counter,
  matching `libfdb_c`), `SetTransactionBypassUnreadable`, and `SetTransactionCausalReadRisky`. They
  propagate to each new transaction via `applyTxDefaults` and are replayed idempotently across retries
  (RFC-133).

### Compatibility
- **Wire format:** unchanged for modern stores — records, indexes, versions, continuations, and split
  records remain byte-identical to Java `fdb-record-layer-core` 4.12.11.0. **Newly closed gap:** Go now
  reads *and* writes legacy Java store layouts (`FormatVersion` < 6 record versions in the
  `RecordVersionKey(8)` subspace, and `omit_unsplit_record_suffix` bare-key unsplit records) and
  performs Java's on-open format upgrade — previously a silent read gap (see Fixed). A Go client can
  now safely share a cluster with legacy Java stores in either direction.
- **SQL behaviour:** net additions only (memory-budget option; the LIMIT-envelope and pagination
  fixes correct latent bugs, they don't change correct-query results).
- **FDB client option semantics:** now documented option-by-option in `pkg/fdbgo/fdb/OPTIONS.md`
  (honored / `UnsupportedOptionError` / safe no-op, vs `libfdb_c` 7.3.77). **One behavioural change:**
  three database-level defaults (`snapshot_ryw_disable`/`enable`, `transaction_bypass_unreadable`,
  `transaction_causal_read_risky`) that were previously silent no-ops now take effect on each new
  transaction — a caller that set them and relied on them being ignored will now see them applied
  (this is the faithful `libfdb_c` behaviour). The unsafe access/auth/quota family still fails loud
  with `UnsupportedOptionError`; no option's *wire* behaviour changed (RFC-133).
- **Required versions:** Java `fdb-record-layer-core` **4.12.11.0**, FDB C++ client **7.3.77**, Go
  **1.26.x** (the `MODULE.bazel` / `go.mod` pins; the CI doc-guard enforces docs match them).
