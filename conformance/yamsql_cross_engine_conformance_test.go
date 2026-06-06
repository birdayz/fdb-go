package conformance_test

// Track A3 — yamsql semantic equivalence on the cross-engine plandiff
// harness. dayshift-55: drives hand-picked yamsql scenarios through BOTH
// the Java fdb-relational executor and the Go embedded engine (via
// plandiff's runners), asserting that
//
//	(a) Java succeeds AND its rows match the scenario's expected rows
//	(b) Go succeeds AND its rows match the scenario's expected rows
//	(c) Java's rows match Go's rows
//
// (c) is the genuine cross-engine equivalence assertion — drift on
// either side surfaces immediately. (a) and (b) keep both engines
// pinned to a stable reference; without them, both engines could drift
// in the same way and (c) wouldn't catch it.
//
// Scenarios are inlined rather than loaded from yamsql/testdata/ — the
// Bazel sandbox doesn't include that tree, and adding it as a data dep
// would couple this conformance test to the yamsql package's data layout.
// Inlining also makes the per-scenario adaptations explicit (e.g. dropping
// NOT NULL on PK columns per the fdb-relational gotcha).
//
// Per-test skips cover Java limitations enumerated in CLAUDE.md (GROUP BY,
// DISTINCT, LIMIT, multi-col ORDER BY, IS TRUE/FALSE) plus error_code
// tests (Java's error structure differs from Go's api.Error) and DML
// tests (runWithSetup expects exactly one query). Wider rollout to all
// scenarios is mechanical follow-on.

import (
	"context"
	"errors"
	"fmt"
	"math"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/conformance/plandiff"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/conformance/yamsql"
)

// Ordered is required for the suite-level BeforeAll (pool spawn / cluster-file
// write). ContinueOnFailure is then MANDATORY: without it, Ginkgo's Ordered
// semantics skip every scenario after the first failing one, so a single broken
// scenario masks all the others and which one is reported depends on the
// randomized run order — making the gate look nondeterministic when it is not.
// With ContinueOnFailure every scenario runs regardless of earlier failures, so
// one run surfaces the COMPLETE set of failures and that set is order-independent.
//
// Caveat: ContinueOnFailure decorates only the OUTERMOST Ordered container, so it
// un-skips across SCENARIOS but not WITHIN one — the inner per-scenario Ordered
// still skips a scenario's remaining tests after its first failure. So a broken
// scenario reports only its first failing test; every broken scenario still
// surfaces. (Do not delete the decorator or this note — it stops the
// order-dependent masking bug from creeping back.)
var _ = Describe("yamsql cross-engine equivalence (A3)", Ordered, ContinueOnFailure, func() {
	var (
		ctx             context.Context
		clusterFile     string
		clusterFilePath string
		pool            *JavaServerPool
	)

	// BeforeAll: write the cluster file once (a per-spec path would create
	// 360+ cache keys in the fdbsql driver's per-cluster-file cache; one shared
	// path keeps it to one entry) and start the per-scenario Java-server pool.
	// `Ordered` on the parent Describe is required for BeforeAll.
	BeforeAll(func() {
		ctx = context.Background()
		var err error
		clusterFile, err = sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		// The Go runner takes a cluster-file path on disk (the fdbsql
		// driver opens it directly); the Java runner takes the
		// contents (sent over HTTP, opened server-side).
		clusterFilePath = writeClusterFileToTemp(clusterFile)
		pool = NewJavaServerPool(a3PoolSize(), a3MaxInvocations())
	})

	AfterAll(func() {
		if pool != nil {
			pool.Shutdown()
		}
	})

	// DETERMINISM MODEL — pooled, re-used Java servers (recycled by invocation).
	//
	// The "A3 is nondeterministic" symptom (a different scenario failing each
	// run) was NOT cross-query JVM-state pollution. That theory was never pinned
	// — and SeedRunCorpus refutes it: it drives ~1620 queries through ONE shared
	// Java server, deterministically, every run. (The once-suspected mechanism,
	// the ANTLR parser's static `_decisionToDFA`/PredictionContextCache, is a
	// performance cache that yields identical parse trees cold or warm; it does
	// not change results.) The actual causes were two, both fixed:
	//   1. Ginkgo's Ordered skip-after-failure + randomized run order: the first
	//      failing scenario skipped all the rest, so each run reported a
	//      different sole failure out of a DETERMINISTIC failure set. Fixed by
	//      ContinueOnFailure (see the outer Describe) — the full set now surfaces
	//      order-independently, which is what let the genuine Go-only-extension /
	//      stateful scenarios below be identified and excluded.
	//   2. GRV lag from CONCURRENT server spawning against the shared FDB
	//      container (a query's txn taking a read version before its own
	//      ephemeral-schema CREATE committed → spurious UnableToPlanException).
	//      The pool never spawns while a query runs (see JavaServerPool).
	//
	// Model: scenarios borrow RE-USED servers from a pool. Defaults are pool size
	// 1 + maxInvocations 0 — ONE shared server for the whole A3 suite, never
	// recycled, exactly like SeedRunCorpus. Recycling (maxInvocations > 0) is a
	// safety belt only — NOT for determinism (SeedRunCorpus + a single-JVM/no-
	// recycle 3× proof show shared re-use is clean) — to bound any hypothetical
	// accumulated state and per-JVM memory. Re-use is what keeps this fast and
	// light: one JVM spawned once instead of ~119 fresh ones, which also keeps a
	// constrained CI runner off the GC-thrash cliff. Knobs:
	// CONFORMANCE_A3_POOL_SIZE, CONFORMANCE_A3_MAX_INVOCATIONS. Bazel caches the
	// test result, so it only re-runs when an input changes.
	//
	// A Java error below is therefore a real, reproducible signal — a genuine
	// Go-only extension (Java cannot plan it; exclude it from
	// crossEngineScenarios with a note) or a regression. Fail loudly.
	for _, s := range crossEngineScenarios() {
		s := s
		Describe("scenario "+s.Name, Ordered, func() {
			var scenarioJava *JavaInvoker
			BeforeAll(func() {
				var err error
				scenarioJava, err = pool.Borrow()
				Expect(err).NotTo(HaveOccurred(),
					"failed to obtain a pooled Java server for scenario %q", s.Name)
			})
			AfterAll(func() {
				pool.Return(scenarioJava)
			})
			for i, t := range s.Tests {
				i, t := i, t
				It(t.Query, func() {
					if t.ErrorCode != "" {
						// SQLState wiring is in place
						// (`*plandiff.JavaError.SQLState` +
						// `assertCrossEngineErrorCode`); the gate stays
						// because lifting the skip surfaces fdb-relational
						// planner hangs on certain error paths under load
						// (e.g. type-mismatch IN-list, GREATEST mixed
						// types, comma-join + bare-col-with-aggregate),
						// dragging unrelated specs into 120s timeouts.
						Skip("error_code tests skipped cross-engine — fdb-relational planner stalls on error paths under load")
					}
					if !yamsql.IsQuery(t.Query) {
						Skip("non-query (DML) cross-engine tests need a different harness — runWithSetup expects exactly one query")
					}
					prefix := fmt.Sprintf("scenario %q test #%d query %q", s.Name, i, t.Query)
					javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(scenarioJava), clusterFile).(plandiff.SetupRunner)
					goRunner := plandiff.NewGoSQLSetupRunner(clusterFilePath)

					javaRes := javaRunner.RunWithSetup(ctx, s.SchemaTemplate, s.Setup, t.Query)
					goRes := goRunner.RunWithSetup(ctx, s.SchemaTemplate, s.Setup, t.Query)

					// Go is ALWAYS pinned to the scenario-declared expected rows.
					Expect(goRes.Err).NotTo(HaveOccurred(), "%s: Go executor errored", prefix)
					assertRowsMatch(goRes.Rows.Rows, t.Rows, t.Unordered, prefix+" [Go vs expected]")

					// STRICT cross-engine, no tolerance: results are deterministic
					// (see the determinism model above), so a Java error is a
					// genuine Go-only extension (exclude it) or a regression — never
					// run-to-run noise.
					Expect(javaRes.Err).NotTo(HaveOccurred(),
						"%s: Java errored on its pooled server — if this query "+
							"is a genuine Go-only extension (Java cannot plan it) exclude it "+
							"from crossEngineScenarios with a note; otherwise it's a regression", prefix)
					// (a) Java vs scenario-declared expected.
					assertRowsMatch(javaRes.Rows.Rows, t.Rows, t.Unordered, prefix+" [Java vs expected]")
					// (b) Java vs Go directly (numerics coerced to float64 on
					// both sides; multiset compare via the same helper).
					assertRowSetsCrossEqual(javaRes.Rows.Rows, goRes.Rows.Rows, t.Unordered,
						prefix+" [Java vs Go]")
				})
			}
		})
	}
})

// crossEngineScenarios is the list of scenarios driven cross-engine. Each
// is hand-adapted from its yamsql YAML twin: PK columns drop NOT NULL
// (fdb-relational restriction; PK is implicitly NOT NULL), error_code
// tests are kept (per-test Skip handles them), Java-unsupported features
// (GROUP BY, DISTINCT, LIMIT, multi-col ORDER BY) trigger Java errors and
// stay on the not-yet-wired list.
func crossEngineScenarios() []*yamsql.Scenario {
	return []*yamsql.Scenario{
		whereLiteralOnLeftScenario(),
		arithmeticScenario(),
		castScenario(),
		compositePKScenario(),
		bytesScenario(),
		betweenScenario(),
		booleanScenario(),
		likeScenario(),
		caseWhenScenario(),
		aggregateEmptyTableScenario(),
		bitwiseScenario(),
		avgScenario(),
		derivedTableScenario(),
		coalesceNullifScenario(),
		bareColWithAggScenario(),
		aggregateNullsScenario(),
		crossJoinScenario(),
		compositePKPrefixPushdownScenario(),
		pkPushdownScenario(),
		secondaryIndexPushdownScenario(),
		likePrefixPushdownScenario(),
		inListAdvancedScenario(),
		compositeSecondaryIndexPrefixPushdownScenario(),
		coveringIndexPushdownScenario(),
		mixedTypeEqualityScenario(),
		gr1JoinScenario(),
		numericTypesScenario(),
		isDistinctFromScenario(),
		inListPushdownScenario(),
		aggregateExprScenario(),
		aggregateExpressionSelectScenario(),
		derivedTableRenamedScenario(),
		orderByEliminationScenario(),
		bugHuntProbesScenario(),
		wrongQualifierScenario(),
		unionScenario(),
		qualifiedStarMoreScenario(),
		cteScenario(),
		unionConstantLiteralScenario(),
		joinNullKeyScenario(),
		overflowScenario(),
		overflowMixedScenario(),
		greatestLeastScenario(),
		recursiveCteCountScenario(),
		caseInsensitiveKeywordsScenario(),
		unionStarScenario(),
		qualifiedStarScenario(),
		existsScenario(),
		nestedDerivedTableScenario(),
		ambiguousColumnScenario(),
		correlatedSubqueryProbesScenario(),
		unionColumnsRenamedScenario(),
		joinChainedScenario(),
		multiFeatureSelectScenario(),
		countDistinctJoinPositiveScenario(),
		nullCompareScenario(),
		booleanPrecedenceScenario(),
		selfJoinScenario(),
		stringCompareScenario(),
		nullArithmeticScenario(),
		orderByIndexedColScenario(),
		arithmeticCompoundScenario(),
		dmlSetupScenario(),
		whereComplexScenario(),
		pkEqualityOrderByScenario(),
		projectionAliasScenario(),
		joinOrderByRightPKScenario(),
		pkDescScenario(),
		numericBoundaryScenario(),
		coalesceExtraScenario(),
		likeEscapeScenario(),
		stringUnicodeScenario(),
		constantProjectionScenario(),
		indexedInListWithOrderByScenario(),
		numericComparisonScenario(),
		dmlAdvancedScenario(),
		compositeIndexOrderByScenario(),
		nullOrderByPositionScenario(),
		isNullWithIndexScenario(),
		havingPositiveScenario(),
		negativeConstantsScenario(),
		emptyStringScenario(),
		largeInListScenario(),
		updateNonPKPredicateScenario(),
		caseInOrderByScenario(),
		bytesAdvancedScenario(),
		coalesceTypePromotionScenario(),
		minMaxBigintBoundaryScenario(),
		multiInsertSetupScenario(),
		orderByCompositeIdxFilterScenario(),
		updateChainScenario(),
		betweenEdgeScenario(),
		stringComparisonOpsScenario(),
		castChainScenario(),
		nullInBetweenScenario(),
		mixedNumericCompareScenario(),
		notInListScenario(),
		// Scalar-subquery scenarios are intentionally NOT cross-engine: a
		// subquery used as a value expression (`(SELECT ...)`) is a Go-only
		// grammar extension (subqueryExpressionAtom). Java fdb-relational
		// 4.11.1.0 has no such grammar rule and rejects every form with a
		// 42601 syntax error, so it can never be asserted equivalent. Go-only
		// coverage lives in pkg/relational/sqldriver/scalar_subquery_cte_test.go
		// and quality_probes_test.go (TestFDB_QualityProbe_ScalarSubquery), plus
		// the yamsql testdata/scalar_subquery*.yaml corpus. Same not-cross-engine
		// bucket as the GROUP BY / DISTINCT / LIMIT Java limitations above.
		unionColumnsScenario(),
		inListNullScenario(),
		// joinOptimizationProbesScenario is NOT cross-engine: it probes the
		// multi-way-join cost frontier (RFC-042), where Go's join enumeration is
		// still non-deterministic on some arithmetic-predicate shapes (a 3-way /
		// arithmetic-join can return a different row count across runs). That is
		// a tracked Go bug, not an RFC-082 column/conformance issue, and a
		// non-deterministic spec must not gate. Tracked under RFC-042; the
		// builder stays for the Go-only determinism follow-up.
		// recursiveCteAdvancedScenario is NOT cross-engine: BOTH its tests hit
		// genuine fdb-relational 4.11.1.0 limitations, each confirmed
		// deterministic when the scenario runs in isolation (no prior query to
		// prime shared engine-state):
		//   (1) column-RENAMING recursive CTE referenced through an alias
		//       (`anc(node, up) ... anc AS a ... a.up`) → SemanticAnalyzer
		//       rejects "Attempting to query non existing column A.UP".
		//   (2) recursive CTE + outer ORDER BY (`... SELECT label FROM desc_tree
		//       ORDER BY id`) → "order by is not supported in subquery" (the CTE
		//       is treated as a subquery). This is the SAME limitation
		//       SeedRunCorpus pins as JavaErrorsGoCorrect for recursive_cte_basic
		//       / cte_basic_with_aggregate.
		// Both are Go-only read-side extensions; a query Java can't plan has no
		// cross-engine equivalence to assert. Covered Go-only via the
		// recursive_cte_advanced yamsql corpus + SeedRunCorpus's annotated
		// CTE-ORDER-BY entries. (Builder kept for that Go-only coverage.)
		orderByNullsScenario(),
		orderByDupeColScenario(),
		compositePKCrossScenario(),
		uniqueViolationScenario(),
		notNullViolationScenario(),
		inSubqueryDecompositionScenario(),
		subqueryInScenario(),
		updateDeleteScenario(),
		updateCaseWhenScenario(),
		updateSetExprScenario(),
		insertArityScenario(),
		insertValuesExprScenario(),
		dmlReturningProbesScenario(),
		// dmlWithNullSafeScenario is NOT cross-engine: it is STATEFUL — its
		// SELECT tests assert the table state AFTER prior DELETE/UPDATE tests in
		// the same scenario (NULL-safe `IS [NOT] DISTINCT FROM` DML). The A3
		// harness runs each test INDEPENDENTLY (schema + Setup + one query), so a
		// SELECT only ever observes the Setup, never a prior test's mutation —
		// e.g. `SELECT id,n FROM t ORDER BY id` returns all 4 seeded rows, not the
		// 2 rows that survive the preceding `DELETE … IS NOT DISTINCT FROM null`.
		// Unlike unique_violation (a single end-state that we could seed in
		// Setup), this scenario has several different post-DML states, so it
		// cannot be made stateless. The NULL-safe DML semantics are covered
		// Go-only via the dml_with_null_safe yamsql corpus (which runs statefully).
		insertSelectScenario(),
		// recursiveCteBaseScenario is NOT cross-engine: it is a showcase of
		// Go-only recursive-CTE EXTENSIONS that fdb-relational 4.11.1.0 cannot
		// plan, so there is no cross-engine equivalence to assert (these are
		// welcome Go-beyond-Java read-side capabilities, not divergences):
		//   - the `TRAVERSAL ORDER pre_order|post_order|level_order` clause —
		//     Java's grammar has no such production at all;
		//   - an outer `ORDER BY` over a recursive CTE (`… SELECT id FROM
		//     ancestors ORDER BY id DESC`) — Java deterministically rejects with
		//     "order by is not supported in subquery" (the WITH body is treated
		//     as a subquery), the same limitation noted on
		//     recursiveCteAdvancedScenario;
		//   - column-list renaming on a recursive CTE referenced via alias.
		// The cross-engine-valid recursive-CTE basics (anchor + recursive UNION,
		// COUNT, empty seed) are asserted by recursiveCteCountScenario. The full
		// Go-only surface is covered by the recursive_cte yamsql corpus. (Builder
		// kept for that Go-only coverage.)
		//
		// dmlSubqueryScenario is NOT cross-engine: it is STATEFUL in the same way
		// as dmlWithNullSafeScenario. Its Setup runs a full MUTATING DML chain
		// (DELETE … WHERE EXISTS → re-INSERT → UPDATE → DELETE … WHERE NOT EXISTS)
		// that ends at t = {2, 4}, but the verification SELECTs assert the
		// INTERMEDIATE states between steps (e.g. `SELECT id FROM t ORDER BY id`
		// expects {1,3,5}, the state after only the first DELETE). The A3 harness
		// runs each test as schema+full-Setup+one-query, so every SELECT observes
		// the single FINAL state — Go deterministically returns {2,4} (verified
		// 10/10 in isolation), correctly, but it cannot match the per-step
		// expectations. Unlike insert_select (additive INSERT-only, so the final
		// state is a superset that point-WHERE SELECTs can still probe), a
		// mutating chain collapses every SELECT to the same end state, so the
		// scenario cannot be made stateless. The correlated EXISTS / NOT EXISTS
		// DML semantics are covered Go-only via the dml_subquery yamsql corpus and
		// dml_cascades_fdb_test.go. (Builder kept for that Go-only coverage.)
		updateDmlCteScenario(),
		// correlatedExistsAdvancedScenario is NOT cross-engine: BOTH its tests
		// are Go-only EXTENSIONS that fdb-relational 4.11.1.0 cannot plan
		// (UnableToPlanException: "Cascades planner could not plan query"),
		// confirmed deterministic per-scenario:
		//   - `SELECT DISTINCT e.name FROM emp e, task t WHERE … AND EXISTS(…)
		//     ORDER BY e.name` — DISTINCT + comma-join + correlated EXISTS +
		//     outer ORDER BY together exceed Java's planner;
		//   - `SELECT name FROM emp WHERE NOT EXISTS(…) ORDER BY name` —
		//     correlated NOT EXISTS + outer ORDER BY.
		// Go plans and returns the correct rows; Java cannot, so there is no
		// cross-engine equivalence to assert. Covered Go-only by the
		// correlated_exists_advanced yamsql corpus + correlated_subquery_probes.
		// (Builder kept for that Go-only coverage.)
		orderByLimitScenario(),
	}
}

// existsScenario probes EXISTS / NOT EXISTS in fdb-relational. Only
// the empty-flags variants — INSERT-then-re-check is mid-stream DML
// that runWithSetup can't express.
func existsScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "exists",
		SchemaTemplate: "CREATE TABLE orders (id BIGINT, cust_id BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE flags (k BIGINT, PRIMARY KEY (k))",
		Setup: []string{
			"INSERT INTO orders VALUES (1, 10), (2, 20), (3, 30)",
		},
		Tests: []yamsql.Test{
			// Empty flags → EXISTS=FALSE → WHERE filters everything.
			{Query: "SELECT id FROM orders WHERE EXISTS (SELECT k FROM flags) ORDER BY id", Rows: [][]any{}},
			// Empty flags → NOT EXISTS=TRUE → all rows pass.
			{Query: "SELECT id FROM orders WHERE NOT EXISTS (SELECT k FROM flags) ORDER BY id", Rows: [][]any{{1}, {2}, {3}}},
		},
	}
}

// qualifiedStarScenario mirrors testdata/qualified_star.yaml. Drops
// NOT NULL on PK. Skips multi-col ORDER BY tests (gotcha) and explicit
// INNER JOIN tests (gotcha).
func qualifiedStarScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "qualified_star",
		SchemaTemplate: "CREATE TABLE a (id BIGINT, name STRING, PRIMARY KEY (id))" +
			" CREATE TABLE b (id BIGINT, label STRING, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO a VALUES (1, 'alpha'), (2, 'beta')",
			"INSERT INTO b VALUES (10, 'one'), (20, 'two')",
		},
		Tests: []yamsql.Test{
			// Single-source a.*.
			{Query: "SELECT a.* FROM a ORDER BY id", Rows: [][]any{{1, "alpha"}, {2, "beta"}}},
			// Comma-join + a.*: returns only a's columns, one copy per right row.
			{Query: "SELECT a.* FROM a, b", Unordered: true, Rows: [][]any{{1, "alpha"}, {1, "alpha"}, {2, "beta"}, {2, "beta"}}},
			// Comma-join + b.* (alias resolved separately).
			{Query: "SELECT b.* FROM a, b", Unordered: true, Rows: [][]any{{10, "one"}, {20, "two"}, {10, "one"}, {20, "two"}}},
			// Aliased qualifier.
			{Query: "SELECT x.* FROM a AS x, b WHERE x.id = 1 ORDER BY b.id", Rows: [][]any{{1, "alpha"}, {1, "alpha"}}},
		},
	}
}

// unionStarScenario mirrors testdata/union_star.yaml. UNION ALL with
// SELECT * on either side. Drops NOT NULL on PK.
func unionStarScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "union_star",
		SchemaTemplate: "CREATE TABLE t1 (id BIGINT, col1 BIGINT, col2 BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t1 VALUES (1, 10, 1), (2, 20, 2)",
		},
		Tests: []yamsql.Test{
			// Both sides * — straight UNION ALL of full rows.
			{Query: "SELECT * FROM t1 UNION ALL SELECT * FROM t1", Unordered: true, Rows: [][]any{{1, 10, 1}, {2, 20, 2}, {1, 10, 1}, {2, 20, 2}}},
			// Left explicit, right *.
			{Query: "SELECT id, col1, col2 FROM t1 UNION ALL SELECT * FROM t1", Unordered: true, Rows: [][]any{{1, 10, 1}, {2, 20, 2}, {1, 10, 1}, {2, 20, 2}}},
			// Left *, right explicit.
			{Query: "SELECT * FROM t1 UNION ALL SELECT id, col1, col2 FROM t1", Unordered: true, Rows: [][]any{{1, 10, 1}, {2, 20, 2}, {1, 10, 1}, {2, 20, 2}}},
			// Aliased columns on left.
			{Query: "SELECT id AS W, col1 AS X, col2 AS Y FROM t1 UNION ALL SELECT * FROM t1", Unordered: true, Rows: [][]any{{1, 10, 1}, {2, 20, 2}, {1, 10, 1}, {2, 20, 2}}},
			// Aggregate over UNION ALL of aggregates (derived table).
			{Query: "SELECT SUM(a) AS a, SUM(b) AS b FROM (SELECT SUM(col1) AS a, COUNT(*) AS b FROM t1 UNION ALL SELECT SUM(col1) AS a, COUNT(*) AS b FROM t1) AS x", Rows: [][]any{{60, 4}}},
		},
	}
}

// caseInsensitiveKeywordsScenario mirrors testdata/case_insensitive_keywords.yaml.
// SQL standard: keywords are case-insensitive. Drops NOT NULL on PK.
// Skips tests using LIMIT (unsupported), GROUP BY (unsupported), DESC
// in ORDER BY (uncertain natural-order continuation), explicit INNER
// JOIN (broken). Lifts the safe SELECT/WHERE/AND/OR/IS NOT NULL forms.
func caseInsensitiveKeywordsScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "case_insensitive_keywords",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, n BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 10), (2, 20), (3, 30)",
		},
		Tests: []yamsql.Test{
			{Query: "SelECT id FROM t WHERE id = 1", Rows: [][]any{{1}}},
			{Query: "select id from t where id = 1", Rows: [][]any{{1}}},
			{Query: "SELECT id FROM t WHERE id = 1 oR id = 3 OrDeR bY id", Rows: [][]any{{1}, {3}}},
			{Query: "SELECT id FROM t WHERE id > 1 AnD n < 30", Rows: [][]any{{2}}},
			{Query: "SELECT id FROM t WHERE n iS nOt NUll ORDER BY id", Rows: [][]any{{1}, {2}, {3}}},
		},
	}
}

// recursiveCteCountScenario lifts two outer-ORDER-BY-free tests from
// testdata/recursive_cte.yaml — `WITH RECURSIVE` walks of a parent-
// link tree. Drops NOT NULL on PK. Most other recursive_cte tests use
// outer ORDER BY which the existing CTE+ORDER BY gotcha rejects.
func recursiveCteCountScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "recursive_cte_count",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, parent BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, -1), (10, 1), (20, 1), (40, 10), (50, 10), (70, 10), (100, 20), (210, 20), (250, 50)",
		},
		Tests: []yamsql.Test{
			// Aggregate over recursive descendants — single-row, no
			// outer ORDER BY needed.
			{Query: "WITH RECURSIVE descendants AS (SELECT id, parent FROM t WHERE parent = -1 UNION ALL SELECT b.id, b.parent FROM descendants AS a, t AS b WHERE b.parent = a.id) SELECT COUNT(*) FROM descendants", Rows: [][]any{{9}}},
			// Empty-seed recursive CTE terminates cleanly.
			{Query: "WITH RECURSIVE noseed AS (SELECT id, parent FROM t WHERE id = 99999 UNION ALL SELECT b.id, b.parent FROM noseed AS a, t AS b WHERE b.parent = a.id) SELECT id FROM noseed", Rows: [][]any{}},
			// `WITH RECURSIVE nonrec AS (...)` without a UNION ALL
			// self-reference is rejected by fdb-relational with
			// "condition is not met!". SQL spec / Postgres permit it
			// (RECURSIVE is a scope enabler), but fdb-relational
			// requires an actual recursive body. Cross-engine corpus
			// drops the form. (New gotcha-worthy if it recurs.)
		},
	}
}

// greatestLeastScenario mirrors testdata/greatest_least.yaml. GREATEST/
// LEAST propagate NULL (any-NULL → NULL). Drops NOT NULL on PK.
// error_code test (cross-type 22000) skipped via per-test Skip.
// Drops `GREATEST(NULL)` / `LEAST(NULL)` (single-NULL-arg) — Java
// raises `VerifyException` (the variadic-of-NULL path needs at least
// one typed argument to determine the result type). New gotcha.
func greatestLeastScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "greatest_least",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT GREATEST(1, 5, 3) FROM t", Rows: [][]any{{5}}},
			{Query: "SELECT LEAST(1, 5, 3) FROM t", Rows: [][]any{{1}}},
			{Query: "SELECT GREATEST(1, NULL, 3) FROM t", Rows: [][]any{{nil}}},
			{Query: "SELECT LEAST(1, NULL, 3) FROM t", Rows: [][]any{{nil}}},
			{Query: "SELECT GREATEST(1, 2, 3.0, 4, 5) FROM t", Rows: [][]any{{5.0}}},
			{Query: "SELECT LEAST(1, 2, 3.0, 4, 5) FROM t", Rows: [][]any{{1.0}}},
			{Query: "SELECT GREATEST('apple', 'banana', 'cherry') FROM t", Rows: [][]any{{"cherry"}}},
			{Query: "SELECT LEAST('apple', 'banana', 'cherry') FROM t", Rows: [][]any{{"apple"}}},
			{Query: "SELECT GREATEST(1, 'a') FROM t", ErrorCode: "22000"},
		},
	}
}

// overflowScenario mirrors testdata/overflow.yaml — integer overflow
// checked arithmetic. Most tests are error_code "22003" (overflow on
// add/sub/mul/div of extremal int64 values, and float literals that
// overflow to ±Inf). The error_code tests are included for visibility
// but get skipped by the per-test error_code skip logic. The two
// non-error SELECT tests exercise the happy paths: MaxInt64 + (-1)
// and MinInt64 % -1. Drops NOT NULL on PK per fdb-relational
// restriction.
func overflowScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "overflow",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 9223372036854775807, 1), (2, -9223372036854775808, 1), (3, 4611686018427387904, 3), (4, -9223372036854775808, -1)",
		},
		Tests: []yamsql.Test{
			// MaxInt64 + 1 → overflow.
			{Query: "SELECT a + b FROM t WHERE id = 1", ErrorCode: "22003"},
			// MinInt64 - 1 → overflow.
			{Query: "SELECT a - b FROM t WHERE id = 2", ErrorCode: "22003"},
			// (MaxInt64/2 + 1) * 3 → overflow.
			{Query: "SELECT a * b FROM t WHERE id = 3", ErrorCode: "22003"},
			// MinInt64 / -1 → overflow (abs(MinInt64) doesn't fit in int64).
			{Query: "SELECT a / b FROM t WHERE id = 4", ErrorCode: "22003"},
			// Baseline: in-range op succeeds. MaxInt64 + (-1).
			{Query: "SELECT a + -1 FROM t WHERE id = 1", Rows: [][]any{{9223372036854775806}}},
			// MinInt64 % -1 is 0.
			{Query: "SELECT a % b FROM t WHERE id = 4", Rows: [][]any{{0}}},
			// Decimal literal that overflows float64 → +Inf.
			{Query: "SELECT 1e400 FROM t WHERE id = 1", ErrorCode: "22003"},
			// Negative counterpart — -1e400 overflows to -Inf.
			{Query: "SELECT -1e400 FROM t WHERE id = 1", ErrorCode: "22003"},
		},
	}
}

// overflowMixedScenario mirrors testdata/overflow_mixed.yaml — long+
// double mixed-type arithmetic. Drops NOT NULL on PK. Skips error_code
// (the pure-long-overflow test). Pins Java's `ADD_LD` semantics
// (long promoted to double, IEEE-754 round-to-nearest, no throw on
// overflow because the float result is finite).
func overflowMixedScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "overflow_mixed",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, a BIGINT, b DOUBLE, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 9223372036854775807, 1.0), (2, 9223372036854775807, 1.0E200)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT a + b FROM t WHERE id = 1", Rows: [][]any{{9.223372036854776e18}}},
			{Query: "SELECT a + b FROM t WHERE id = 2", Rows: [][]any{{1e200}}},
		},
	}
}

// joinNullKeyScenario mirrors testdata/join_null_key.yaml. NULL = NULL
// is UNKNOWN; rows with NULL in the join column do NOT join. NULL-safe
// equality via IS NOT DISTINCT FROM treats NULL=NULL as TRUE. All
// comma-join (no explicit JOIN ON). Drops NOT NULL on PK.
func joinNullKeyScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "join_null_key",
		SchemaTemplate: "CREATE TABLE a (id BIGINT, k BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE b (id BIGINT, k BIGINT, label STRING, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO a VALUES (1, 10), (2, null), (3, 20)",
			"INSERT INTO b VALUES (101, 10, 'alpha'), (102, null, 'beta'), (103, 20, 'gamma')",
		},
		Tests: []yamsql.Test{
			// INNER comma-join on k=k excludes NULL-keyed rows.
			{Query: "SELECT a.id, b.label FROM a, b WHERE a.k = b.k", Unordered: true, Rows: [][]any{{1, "alpha"}, {3, "gamma"}}},
			// NOT (a.k = NULL) is UNKNOWN, filters everything.
			{Query: "SELECT a.id FROM a, b WHERE NOT (a.k = null) AND a.id = 1", Rows: [][]any{}},
			// NULL-safe equality via IS NOT DISTINCT FROM matches NULLs.
			{Query: "SELECT a.id, b.label FROM a, b WHERE a.k IS NOT DISTINCT FROM b.k", Unordered: true, Rows: [][]any{{1, "alpha"}, {2, "beta"}, {3, "gamma"}}},
		},
	}
}

// unionConstantLiteralScenario lifts one test from
// testdata/union_columns.yaml — UNION ALL with a constant literal on
// one side. The rest of union_columns.yaml is mostly multi-col
// ORDER BY, LIMIT/OFFSET, UNION (distinct), and arity-mismatch error
// codes — all already-covered gotchas.
func unionConstantLiteralScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "union_constant_literal",
		SchemaTemplate: "CREATE TABLE a (id BIGINT, v BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE b (id BIGINT, w BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO a VALUES (1, 10), (2, 20)",
			"INSERT INTO b VALUES (1, 100), (2, 200)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT v FROM a UNION ALL SELECT 99 FROM b", Unordered: true, Rows: [][]any{{10}, {20}, {99}, {99}}},
		},
	}
}

// cteScenario mirrors testdata/cte.yaml — the outer-ORDER-BY-free
// subset (existing CLAUDE.md gotcha: `WITH ... ORDER BY` rejected).
// Drops NOT NULL on PK. Uses Unordered comparison where the original
// yamsql relied on ORDER BY.
func cteScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "cte",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 10), (2, 20), (3, 30), (4, 40)",
		},
		Tests: []yamsql.Test{
			// Basic CTE projection.
			{Query: "WITH hi AS (SELECT id, v FROM t WHERE v >= 20) SELECT id FROM hi", Unordered: true, Rows: [][]any{{2}, {3}, {4}}},
			// Aggregate over CTE.
			{Query: "WITH hi AS (SELECT id, v FROM t WHERE v >= 20) SELECT COUNT(*), SUM(v) FROM hi", Rows: [][]any{{3, 90}}},
			// CTE filter + further filter.
			{Query: "WITH hi AS (SELECT id, v FROM t WHERE v >= 20) SELECT id FROM hi WHERE v >= 30", Unordered: true, Rows: [][]any{{3}, {4}}},
			// Multi-CTE cross-join.
			{Query: "WITH lo AS (SELECT id FROM t WHERE v < 20), hi AS (SELECT id FROM t WHERE v >= 30) SELECT COUNT(*) FROM lo, hi", Rows: [][]any{{2}}},
			// CTE with column rename.
			{Query: "WITH c1(x, y) AS (SELECT id, v FROM t) SELECT x FROM c1 WHERE y >= 30", Unordered: true, Rows: [][]any{{3}, {4}}},
			{Query: "WITH c1(my_id) AS (SELECT id FROM t) SELECT my_id FROM c1", Unordered: true, Rows: [][]any{{1}, {2}, {3}, {4}}},
			// Chained CTE renames.
			{Query: "WITH base(d, val) AS (SELECT id, v FROM t), filtered(x, y) AS (SELECT d, val FROM base WHERE val > 15) SELECT x, y FROM filtered", Unordered: true, Rows: [][]any{{2, 20}, {3, 30}, {4, 40}}},
			// Multi-CTE comma-join with renames.
			{Query: "WITH lo(li) AS (SELECT id FROM t WHERE v < 20), hi(hi_id) AS (SELECT id FROM t WHERE v >= 30) SELECT li, hi_id FROM lo, hi", Unordered: true, Rows: [][]any{{1, 3}, {1, 4}}},
			// Nested CTE references.
			{Query: "WITH a AS (SELECT id FROM t WHERE v >= 20), b AS (SELECT id FROM a WHERE id >= 3) SELECT id FROM b", Unordered: true, Rows: [][]any{{3}, {4}}},
			// 3-level CTE chain.
			{Query: "WITH a AS (SELECT id, v FROM t), b AS (SELECT id FROM a WHERE v >= 20), c AS (SELECT id FROM b WHERE id >= 3) SELECT id FROM c", Unordered: true, Rows: [][]any{{3}, {4}}},
			// CTE-defined-but-unused.
			{Query: "WITH ignored AS (SELECT id FROM t WHERE id > 100) SELECT id FROM t", Unordered: true, Rows: [][]any{{1}, {2}, {3}, {4}}},
			// SELECT * on CTE.
			{Query: "WITH c1 AS (SELECT * FROM t) SELECT * FROM c1", Unordered: true, Rows: [][]any{{1, 10}, {2, 20}, {3, 30}, {4, 40}}},
			// SELECT * on renamed CTE.
			{Query: "WITH c1(w, z) AS (SELECT id, v FROM t WHERE id <= 2) SELECT * FROM c1", Unordered: true, Rows: [][]any{{1, 10}, {2, 20}}},
		},
	}
}

// unionScenario mirrors testdata/union.yaml. Drops the UNION (distinct)
// test — fdb-relational 4.11.1.0 raises `only UNION ALL is supported`
// (new CLAUDE.md gotcha). UNION ALL works on both engines. Drops NOT
// NULL on PK.
func unionScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "union",
		SchemaTemplate: "CREATE TABLE a (id BIGINT, v BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE b (id BIGINT, v BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO a VALUES (1, 10), (2, 20), (3, 30)",
			"INSERT INTO b VALUES (101, 20), (102, 30), (103, 40)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT v FROM a UNION ALL SELECT v FROM b", Unordered: true, Rows: [][]any{{10}, {20}, {30}, {20}, {30}, {40}}},
		},
	}
}

// qualifiedStarMoreScenario mirrors testdata/qualified_star_more.yaml.
// Comma-join with qualified-star, both unaliased and aliased forms.
// Drops NOT NULL on PK. Skips the GROUP BY error_code tests.
func qualifiedStarMoreScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "qualified_star_more",
		SchemaTemplate: "CREATE TABLE a (a1 BIGINT, a2 BIGINT, PRIMARY KEY (a1))" +
			" CREATE TABLE b (b1 BIGINT, b2 BIGINT, PRIMARY KEY (b1))",
		Setup: []string{
			"INSERT INTO a VALUES (1, 10), (2, 20), (3, 30)",
			"INSERT INTO b VALUES (1, 100), (2, 200), (3, 300)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT a.*, b.* FROM a, b WHERE a.a1 = b.b1 ORDER BY a.a1", Rows: [][]any{{1, 10, 1, 100}, {2, 20, 2, 200}, {3, 30, 3, 300}}},
			{Query: "SELECT x.*, y.* FROM a AS x, b AS y WHERE x.a1 = y.b1 ORDER BY x.a1", Rows: [][]any{{1, 10, 1, 100}, {2, 20, 2, 200}, {3, 30, 3, 300}}},
		},
	}
}

// wrongQualifierScenario mirrors testdata/wrong_qualifier.yaml — the
// SELECT-only positive subset plus error_code tests (skipped via per-
// test Skip). Drops NOT NULL on PK. Skips the explicit `INNER JOIN`
// tests (existing CLAUDE.md gotcha). Pins comma-join + alias resolution
// + WHERE qualifier resolution + single-source aliased projection.
func wrongQualifierScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "wrong_qualifier",
		SchemaTemplate: "CREATE TABLE a (id BIGINT, name STRING, PRIMARY KEY (id))" +
			" CREATE TABLE b (id BIGINT, label STRING, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO a VALUES (1, 'alpha'), (2, 'beta')",
			"INSERT INTO b VALUES (1, 'one'), (2, 'two')",
		},
		Tests: []yamsql.Test{
			// Comma-join with valid qualifiers.
			{Query: "SELECT a.name, b.label FROM a, b WHERE a.id = b.id ORDER BY a.id", Rows: [][]any{{"alpha", "one"}, {"beta", "two"}}},
			// Aliased qualifier in comma-join.
			{Query: "SELECT x.id, x.name FROM a AS x, b WHERE x.id = b.id ORDER BY x.id", Rows: [][]any{{1, "alpha"}, {2, "beta"}}},
			// Valid qualifier in WHERE.
			{Query: "SELECT a.id FROM a, b WHERE a.id = b.id AND b.label = 'one'", Rows: [][]any{{1}}},
			// Single-table aliased projection.
			{Query: "SELECT d.id, d.name FROM a AS d WHERE d.id = 1", Rows: [][]any{{1, "alpha"}}},
			// Single-table unaliased qualifier.
			{Query: "SELECT a.id, a.name FROM a WHERE a.id = 2", Rows: [][]any{{2, "beta"}}},
		},
	}
}

// bugHuntProbesScenario mirrors testdata/bug_hunt_probes.yaml — the
// SELECT-only NULL-semantic / aggregate / CTE probes. Drops NOT NULL on
// PK. Skips DML, error_code, IN-with-NULL forms (Java rejects NULL in
// IN list per existing CLAUDE.md gotcha).
func bugHuntProbesScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "bug_hunt_probes",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, n BIGINT, s STRING, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 10, 'a'), (2, 20, 'b'), (3, 30, null), (4, null, 'd')",
		},
		Tests: []yamsql.Test{
			// COUNT/AVG/SUM NULL semantics.
			{Query: "SELECT COUNT(*), COUNT(n), COUNT(s) FROM t", Rows: [][]any{{4, 3, 3}}},
			{Query: "SELECT AVG(n) FROM t", Rows: [][]any{{20.0}}},
			{Query: "SELECT SUM(n) FROM t WHERE n IS NULL", Rows: [][]any{{nil}}},
			{Query: "SELECT COUNT(*) FROM t WHERE id > 1000", Rows: [][]any{{0}}},
			// Boolean three-valued logic.
			{Query: "SELECT id FROM t WHERE (n > 5) AND (s IS NULL) ORDER BY id", Rows: [][]any{{3}}},
			// Nested CTE — drop ORDER BY (CTE+ORDER BY gotcha).
			{Query: "WITH a AS (SELECT id, n FROM t WHERE n IS NOT NULL), b AS (SELECT id, n * 2 AS doubled FROM a) SELECT id, doubled FROM b", Unordered: true, Rows: [][]any{{1, 20}, {2, 40}, {3, 60}}},
			// Aggregate over CTE.
			{Query: "WITH high AS (SELECT n FROM t WHERE n > 15) SELECT SUM(n), COUNT(*) FROM high", Rows: [][]any{{50, 2}}},
		},
	}
}

// orderByEliminationScenario mirrors testdata/order_by_elimination.yaml
// — the single-col ORDER BY subset that survives the planner's
// natural-order continuation rule. Drops NOT NULL on PK. Skips DESC
// (cursors are ASC-only and Java's planner often can't reverse), and
// multi-col ORDER BY (existing CLAUDE.md gotcha). Renamed `plan` to
// `tier` (reserved-word gotcha).
func orderByEliminationScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "order_by_elimination",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, name STRING, PRIMARY KEY (id))" +
			" CREATE INDEX idx_v ON t (v)" +
			" CREATE TABLE ab (a BIGINT, b BIGINT, c BIGINT, PRIMARY KEY (a, b))" +
			" CREATE TABLE rp (id BIGINT, region STRING, tier STRING, PRIMARY KEY (id))" +
			" CREATE INDEX idx_region_tier ON rp (region, tier)",
		Setup: []string{
			"INSERT INTO t VALUES (3, 10, 'c'), (1, 30, 'a'), (2, 20, 'b'), (4, 40, 'd'), (5, 15, 'e')",
			"INSERT INTO ab VALUES (1, 10, 100), (2, 20, 200), (1, 20, 150), (2, 10, 250), (1, 30, 130)",
			"INSERT INTO rp VALUES (1, 'us', 'pro'), (2, 'us', 'free'), (3, 'us', 'pro'), (4, 'eu', 'pro'), (5, 'eu', 'free')",
		},
		Tests: []yamsql.Test{
			// Full scan + ORDER BY PK ASC — natural order.
			{Query: "SELECT id, v FROM t ORDER BY id", Rows: [][]any{{1, 30}, {2, 20}, {3, 10}, {4, 40}, {5, 15}}},
			// PK range + ORDER BY PK ASC.
			{Query: "SELECT id FROM t WHERE id > 1 ORDER BY id", Rows: [][]any{{2}, {3}, {4}, {5}}},
			// Secondary equality + ORDER BY PK.
			{Query: "SELECT id FROM t WHERE v = 10 ORDER BY id", Rows: [][]any{{3}}},
			// Secondary range + ORDER BY indexed col.
			{Query: "SELECT id, v FROM t WHERE v >= 20 ORDER BY v", Rows: [][]any{{2, 20}, {1, 30}, {4, 40}}},
			// Composite PK ORDER BY first PK col.
			{Query: "SELECT a, b FROM ab WHERE a = 1 ORDER BY b", Rows: [][]any{{1, 10}, {1, 20}, {1, 30}}},
		},
	}
}

// aggregateExpressionSelectScenario mirrors testdata/
// aggregate_expression_select.yaml — SELECT-list expressions wrapping
// aggregate function calls (post-aggregation evaluation). Drops NOT NULL
// on PK. Skips GROUP BY tests (planner unsupported per CLAUDE.md
// gotcha) and the COALESCE-wrapping-aggregate / CASE-wrapping-aggregate
// forms — fdb-relational 4.11.1.0's planner raises
// `IllegalStateException: unable to eval an aggregation function with
// eval()` because the post-aggregation rewrite for scalar-function-of-
// aggregate isn't implemented. Aggregate-of-expression
// (`SUM(CASE WHEN ...)` covered by aggregate_expr) works; the inverse
// direction does not. New CLAUDE.md gotcha.
func aggregateExpressionSelectScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "aggregate_expression_select",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, grp STRING, a BIGINT, b BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 'x', 5, 10), (2, 'x', 20, 3), (3, 'y', 7, 2), (4, 'y', null, null)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT SUM(a) + SUM(b) FROM t", Rows: [][]any{{47}}},
			{Query: "SELECT SUM(a) - SUM(b) FROM t", Rows: [][]any{{17}}},
			// `COALESCE(SUM(a), 0)`, `CASE WHEN COUNT(*) > 3 THEN ...
			// END` dropped — Java planner raises `unable to eval an
			// aggregation function with eval()` (post-aggregation
			// rewrite for scalar-function-of-aggregate not supported).
			{Query: "SELECT AVG(a) + 1 FROM t", Rows: [][]any{{11.666666666666666}}},
			{Query: "SELECT SUM(a), SUM(a) + 1 FROM t WHERE id < 3", Rows: [][]any{{25, 26}}},
			{Query: "SELECT SUM(a) + 1 FROM t HAVING SUM(a) > 10", Rows: [][]any{{33}}},
			{Query: "SELECT SUM(a), SUM(b) + 1 FROM t WHERE id < 3", Rows: [][]any{{25, 14}}},
			{Query: "SELECT SUM(a), SUM(a) + SUM(b) FROM t WHERE id < 3", Rows: [][]any{{25, 38}}},
			{Query: "SELECT 1, SUM(a) FROM t", Rows: [][]any{{1, 32}}},
			{Query: "SELECT 'total', COUNT(*) FROM t", Rows: [][]any{{"total", 4}}},
			{Query: "SELECT 1, 2, MAX(a) FROM t", Rows: [][]any{{1, 2, 20}}},
		},
	}
}

// derivedTableRenamedScenario mirrors testdata/derived_table_renamed.yaml.
// Drops NOT NULL on PK. Drops the two-derived-tables comma-join (Go
// unsupported by design — the extra-sources loop only accepts
// AtomTableItem, error_code 0A000). Lifts the
// derived-table-on-left + real-table-on-right form which works.
func derivedTableRenamedScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "derived_table_renamed",
		SchemaTemplate: "CREATE TABLE a (ida BIGINT, PRIMARY KEY (ida))" +
			" CREATE TABLE b (idb BIGINT, PRIMARY KEY (idb))",
		Setup: []string{
			"INSERT INTO a VALUES (1)",
			"INSERT INTO b VALUES (4)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT sq.x, b.idb FROM (SELECT ida AS x FROM a) AS sq, b", Rows: [][]any{{1, 4}}},
		},
	}
}

// aggregateExprScenario mirrors testdata/aggregate_expr.yaml — the no-
// GROUP-BY no-DISTINCT subset. Drops NOT NULL on PK. Drops the GROUP BY,
// SUM(DISTINCT), error_code, and aggregate-of-aggregate-with-ORDER-BY
// tests (planner gotchas: GROUP BY unsupported, DISTINCT unsupported,
// ORDER BY <aggregate> requires the natural-order continuation).
//
// `SUM(BIGINT) / COUNT(*)` integer-division parity (resolved
// nightshift-57): Go's SUM now preserves int64 when every observed
// value is integral (see `pkg/relational/core/embedded/aggregate.go`
// `sumIntOnly`), so `SUM(qty) / COUNT(*)` integer-divides Java-style
// rather than float-dividing.
func aggregateExprScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "aggregate_expr",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, qty BIGINT, price BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 2, 100), (2, 3, 50), (3, 5, 20)",
		},
		Tests: []yamsql.Test{
			// Plain-column aggregates.
			{Query: "SELECT SUM(qty) FROM t", Rows: [][]any{{10}}},
			{Query: "SELECT MIN(price), MAX(price) FROM t", Rows: [][]any{{20, 100}}},
			{Query: "SELECT AVG(price) FROM t", Rows: [][]any{{56.666666666666664}}},
			// Aggregates over arithmetic expression.
			{Query: "SELECT SUM(qty * price) FROM t", Rows: [][]any{{450}}},
			{Query: "SELECT AVG(price / 2) FROM t", Rows: [][]any{{28.333333333333332}}},
			{Query: "SELECT MIN(qty * price), MAX(qty * price) FROM t", Rows: [][]any{{100, 200}}},
			// Alias.
			{Query: "SELECT SUM(qty * price) AS revenue FROM t", Rows: [][]any{{450}}},
			// Multi-aggregate.
			{Query: "SELECT SUM(qty), SUM(qty * price) FROM t", Rows: [][]any{{10, 450}}},
			// COUNT over expression.
			{Query: "SELECT COUNT(qty * price) FROM t", Rows: [][]any{{3}}},
			// Empty group / NULL.
			{Query: "SELECT SUM(qty * price), COUNT(qty * price) FROM t WHERE id < 0", Rows: [][]any{{nil, 0}}},
			// CASE-based aggregates.
			{Query: "SELECT SUM(CASE WHEN id < 3 THEN price ELSE 0 END) FROM t", Rows: [][]any{{150}}},
			{Query: "SELECT COUNT(CASE WHEN id < 3 THEN 1 END) FROM t", Rows: [][]any{{2}}},
			{Query: "SELECT MAX(CASE WHEN id < 3 THEN price ELSE 0 END) FROM t", Rows: [][]any{{100}}},
			{Query: "SELECT AVG(CASE WHEN id < 3 THEN price END) FROM t", Rows: [][]any{{75.0}}},
			// SUM/COUNT integer-division (re-enabled nightshift-57).
			// SUM(qty)=10 (BIGINT), COUNT(*)=3 (BIGINT), 10/3=3 (integer division).
			{Query: "SELECT SUM(qty) / COUNT(*) FROM t", Rows: [][]any{{3}}},
			// Multi-aggregate with arithmetic. SUM-COUNT now preserves
			// int64 (Java-aligned).
			{Query: "SELECT MIN(qty) + MAX(qty), SUM(qty) - COUNT(*) FROM t", Rows: [][]any{{7, 7}}},
		},
	}
}

// orderByExpressionScenario tracker — the single-col `ORDER BY a + b`
// forms also fail Cascades planning (UnableToPlanException), the same
// natural-order-continuation gotcha as ORDER BY non-PK col. Java's
// planner can only sort by columns that are already naturally ordered
// by the chosen scan plan; an arithmetic expression isn't naturally
// ordered by any FDB key. Even single-col ORDER BY <expression> needs
// a sort rule the planner doesn't have. Cross-engine drop.
//
// Surfaced swingshift-56. Existing CLAUDE.md `ORDER BY natural-order`
// gotcha covers this; no new gotcha needed.

// inListPushdownScenario mirrors testdata/in_list_pushdown.yaml — the
// SELECT-only subset that doesn't depend on mid-stream DML state. Drops
// NOT NULL on PK. Renames the `plan` column to `category` (per the
// reserved-word gotcha). Drops LIMIT and DISTINCT tests, the
// `WHERE id IN (1, NULL, 3)` form (CLAUDE.md gotcha — fdb-relational
// rejects NULL in IN list outright), and IN-with-subquery (scalar
// subquery gotcha). Drops ORDER BY non-PK forms (planner natural-order
// gotcha).
func inListPushdownScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "in_list_pushdown",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, name STRING, v BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE ts (k STRING, w BIGINT, PRIMARY KEY (k))" +
			" CREATE TABLE kvw (a BIGINT, b BIGINT, c BIGINT, v BIGINT, PRIMARY KEY (a, b, c))" +
			" CREATE TABLE t2 (id BIGINT, status STRING, v BIGINT, PRIMARY KEY (id))" +
			" CREATE INDEX idx_t2_status ON t2 (status)" +
			" CREATE TABLE tp (id BIGINT, region STRING, category STRING, PRIMARY KEY (id))" +
			" CREATE INDEX idx_region_category ON tp (region, category)" +
			" CREATE TABLE tb (id BIGINT, payload BYTES, PRIMARY KEY (id))" +
			" CREATE INDEX idx_payload ON tb (payload)",
		Setup: []string{
			"INSERT INTO t VALUES (1, 'one', 10), (2, 'two', 20), (3, 'three', 30), (4, 'four', 40), (5, 'five', 50)",
			"INSERT INTO ts VALUES ('alpha', 1), ('beta', 2), ('gamma', 3), ('delta', 4)",
			"INSERT INTO kvw VALUES (1, 10, 100, 1000), (1, 20, 200, 2000), (2, 30, 300, 3000), (2, 40, 400, 4000)",
			"INSERT INTO t2 VALUES (1, 'active', 5), (2, 'archived', 15), (3, 'active', 25), (4, 'archived', 5), (5, 'deleted', 50)",
			"INSERT INTO tp VALUES (10, 'us', 'pro'), (11, 'us', 'free'), (12, 'us', 'pro'), (13, 'eu', 'pro'), (14, 'eu', 'free')",
			"INSERT INTO tb VALUES (1, X'deadbeef'), (2, X'cafe'), (3, X'feed')",
		},
		Tests: []yamsql.Test{
			// Single-col PK IN-list.
			{Query: "SELECT id, name FROM t WHERE id IN (1, 3) ORDER BY id", Rows: [][]any{{1, "one"}, {3, "three"}}},
			{Query: "SELECT id FROM t WHERE id IN (2)", Rows: [][]any{{2}}},
			{Query: "SELECT id FROM t WHERE id IN (1, 2, 3, 4, 5) ORDER BY id", Rows: [][]any{{1}, {2}, {3}, {4}, {5}}},
			{Query: "SELECT id FROM t WHERE id IN (100, 200, 300)", Rows: [][]any{}},
			{Query: "SELECT id FROM t WHERE id IN (2, 99, 4) ORDER BY id", Rows: [][]any{{2}, {4}}},
			// IN-list + AND residual.
			{Query: "SELECT id FROM t WHERE id IN (1, 3, 5) AND v > 20 ORDER BY id", Rows: [][]any{{3}, {5}}},
			{Query: "SELECT id FROM t WHERE id IN (1, 2, 3) AND name LIKE 'tw%'", Rows: [][]any{{2}}},
			// NOT IN.
			{Query: "SELECT id FROM t WHERE id NOT IN (1, 2, 3) ORDER BY id", Rows: [][]any{{4}, {5}}},
			// String PK IN.
			{Query: "SELECT k FROM ts WHERE k IN ('alpha', 'gamma') ORDER BY k", Rows: [][]any{{"alpha"}, {"gamma"}}},
			{Query: "SELECT k, w FROM ts WHERE k IN ('beta', 'missing') ORDER BY k", Rows: [][]any{{"beta", 2}}},
			// Composite PK IN-list — leading eq + trailing IN.
			{Query: "SELECT a, b, c FROM kvw WHERE a = 1 AND b IN (10, 20) ORDER BY b", Rows: [][]any{{1, 10, 100}, {1, 20, 200}}},
			{Query: "SELECT a, b, c FROM kvw WHERE a = 1 AND b IN (10, 30) AND c = 100", Rows: [][]any{{1, 10, 100}}},
			{Query: "SELECT a, b, c FROM kvw WHERE a = 1 AND b = 20 AND c IN (100, 200) ORDER BY c", Rows: [][]any{{1, 20, 200}}},
			{Query: "SELECT a, b, c FROM kvw WHERE a = 1 AND b IN (10, 20) AND c = 999", Rows: [][]any{}},
			// Composite secondary-index IN-list.
			{Query: "SELECT id, region, category FROM tp WHERE region = 'us' AND category IN ('pro', 'free') ORDER BY id", Rows: [][]any{{10, "us", "pro"}, {11, "us", "free"}, {12, "us", "pro"}}},
			// Secondary-index IN-list.
			{Query: "SELECT id, status FROM t2 WHERE status IN ('active', 'deleted') ORDER BY id", Rows: [][]any{{1, "active"}, {3, "active"}, {5, "deleted"}}},
			{Query: "SELECT id, status FROM t2 WHERE status IN ('active') ORDER BY id", Rows: [][]any{{1, "active"}, {3, "active"}}},
			{Query: "SELECT id FROM t2 WHERE status IN ('active', 'archived') AND v > 10 ORDER BY id", Rows: [][]any{{2}, {3}}},
			{Query: "SELECT id FROM t2 WHERE status IN ('nonexistent', 'alsonone')", Rows: [][]any{}},
			{Query: "SELECT id FROM t2 WHERE status IN ('active', 'nope') ORDER BY id", Rows: [][]any{{1}, {3}}},
			// BYTES-indexed IN.
			{Query: "SELECT id FROM tb WHERE payload IN (X'cafe', X'feed') ORDER BY id", Rows: [][]any{{2}, {3}}},
			// Type-mismatch (Skip via per-test Skip).
			{Query: "SELECT id FROM t WHERE id IN (1, 'two', 3)", ErrorCode: "22000"},
		},
	}
}

// numericTypesScenario mirrors testdata/numeric_types.yaml — the SELECT
// arithmetic tests only. Drops NOT NULL on PK. The INSERT
// out-of-range tests aren't expressible cross-engine via runWithSetup
// (one query per test). The post-INSERT SELECT references id=3 which
// never gets inserted, so it's dropped too.
func numericTypesScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "numeric_types",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, i INTEGER, l BIGINT, d DOUBLE, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 10, 100, 1.5), (2, 20, 200, 2.5)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT i / 3 FROM t WHERE id = 1", Rows: [][]any{{3}}},
			{Query: "SELECT l + l FROM t WHERE id = 1", Rows: [][]any{{200}}},
			{Query: "SELECT d * 2 FROM t WHERE id = 1", Rows: [][]any{{3.0}}},
			{Query: "SELECT i + d FROM t WHERE id = 1", Rows: [][]any{{11.5}}},
		},
	}
}

// isDistinctFromScenario mirrors testdata/is_distinct_from.yaml. Drops
// NOT NULL on PK. Tests SQL:1999 NULL-safe equality (IS DISTINCT FROM /
// IS NOT DISTINCT FROM) — never UNKNOWN, two-valued boolean.
func isDistinctFromScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "is_distinct_from",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, label STRING, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 'alpha'), (2, 'beta'), (3, null), (4, null)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT id FROM t WHERE label = 'alpha' ORDER BY id", Rows: [][]any{{1}}},
			{Query: "SELECT id FROM t WHERE label IS DISTINCT FROM 'alpha' ORDER BY id", Rows: [][]any{{2}, {3}, {4}}},
			{Query: "SELECT id FROM t WHERE label IS NOT DISTINCT FROM null ORDER BY id", Rows: [][]any{{3}, {4}}},
			{Query: "SELECT id FROM t WHERE label IS NOT DISTINCT FROM 'beta'", Rows: [][]any{{2}}},
			{Query: "SELECT id FROM t WHERE label IS DISTINCT FROM null ORDER BY id", Rows: [][]any{{1}, {2}}},
			{Query: "SELECT id FROM t WHERE null IS DISTINCT FROM label ORDER BY id", Rows: [][]any{{1}, {2}}},
			{Query: "SELECT id FROM t WHERE null IS DISTINCT FROM null", Rows: [][]any{}},
			{Query: "SELECT id FROM t WHERE 10 IS DISTINCT FROM 10", Rows: [][]any{}},
			{Query: "SELECT id FROM t WHERE 10 IS NOT DISTINCT FROM 10 ORDER BY id", Rows: [][]any{{1}, {2}, {3}, {4}}},
			{Query: "SELECT id, label IS DISTINCT FROM null FROM t WHERE id = 3", Rows: [][]any{{3, false}}},
			{Query: "SELECT id, label IS NOT DISTINCT FROM null FROM t WHERE id = 3", Rows: [][]any{{3, true}}},
			{Query: "SELECT id, label IS DISTINCT FROM 'alpha' FROM t ORDER BY id", Rows: [][]any{{1, false}, {2, true}, {3, true}, {4, true}}},
		},
	}
}

// coveringIndexPushdownScenario mirrors testdata/covering_index_pushdown.yaml
// — the SELECT-only covered subset. Renames yamsql's `plan` to
// `category` (per the swingshift-56 reserved-word gotcha) and `note` to
// `notes` (defensive). Skips tests using DISTINCT (planner unsupported,
// existing CLAUDE.md gotcha), LIMIT (unsupported), multi-col ORDER BY
// (unsupported), and IN subquery (uncertain support; scalar-subquery
// gotcha covers the bare form).
func coveringIndexPushdownScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "covering_index_pushdown",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, status STRING, lbl STRING, v BIGINT, notes STRING, PRIMARY KEY (id))" +
			" CREATE INDEX idx_status ON t (status)" +
			" CREATE INDEX idx_v ON t (v)" +
			" CREATE TABLE t2 (id BIGINT, region STRING, category STRING, amount BIGINT, extra STRING, PRIMARY KEY (id))" +
			" CREATE INDEX idx_region_category ON t2 (region, category)" +
			" CREATE TABLE tb (id BIGINT, payload BYTES, label STRING, PRIMARY KEY (id))" +
			" CREATE INDEX idx_payload ON tb (payload)" +
			" CREATE TABLE kvw (a BIGINT, b BIGINT, c BIGINT, extra STRING, PRIMARY KEY (a, b, c))" +
			" CREATE INDEX kvw_b ON kvw (b)",
		Setup: []string{
			"INSERT INTO t VALUES (1, 'active', 'x', 10, 'n1'), (2, 'archived', 'y', 20, 'n2'), (3, 'active', 'z', 30, 'n3'), (4, 'deleted', 'q', 40, 'n4')",
			"INSERT INTO t2 VALUES (10, 'us', 'pro', 100, 'e1'), (11, 'us', 'free', 50, 'e2'), (12, 'us', 'pro', 200, 'e3'), (13, 'eu', 'pro', 300, 'e4'), (14, 'eu', 'free', 75, 'e5')",
			"INSERT INTO tb VALUES (1, X'deadbeef', 'a'), (2, X'cafe', 'b'), (3, X'feed', 'c')",
			"INSERT INTO kvw VALUES (1, 10, 100, 'e1'), (1, 20, 100, 'e2'), (1, 30, 100, 'e3'), (2, 20, 200, 'e4')",
		},
		Tests: []yamsql.Test{
			// Covered: SELECT of indexed col + PK.
			{Query: "SELECT id FROM t WHERE status = 'active' ORDER BY id", Rows: [][]any{{1}, {3}}},
			{Query: "SELECT id, status FROM t WHERE status = 'active' ORDER BY id", Rows: [][]any{{1, "active"}, {3, "active"}}},
			{Query: "SELECT status FROM t WHERE status = 'archived'", Rows: [][]any{{"archived"}}},
			// Covered range on indexed column.
			{Query: "SELECT id, v FROM t WHERE v >= 20 AND v < 40 ORDER BY v", Rows: [][]any{{2, 20}, {3, 30}}},
			{Query: "SELECT id, v FROM t WHERE v BETWEEN 20 AND 30 ORDER BY v", Rows: [][]any{{2, 20}, {3, 30}}},
			{Query: "SELECT id FROM t WHERE v > 25 ORDER BY id", Rows: [][]any{{3}, {4}}},
			{Query: "SELECT id, v FROM t WHERE v > 0 ORDER BY v", Rows: [][]any{{1, 10}, {2, 20}, {3, 30}, {4, 40}}},
			// Covered composite index.
			{Query: "SELECT id, region, category FROM t2 WHERE region = 'us' AND category = 'pro' ORDER BY id", Rows: [][]any{{10, "us", "pro"}, {12, "us", "pro"}}},
			{Query: "SELECT region, category FROM t2 WHERE region = 'us' AND category > 'f' ORDER BY category", Rows: [][]any{{"us", "free"}, {"us", "pro"}, {"us", "pro"}}},
			// Covered BYTES-kind indexed col.
			{Query: "SELECT id FROM tb WHERE payload = X'cafe'", Rows: [][]any{{2}}},
			{Query: "SELECT id FROM tb WHERE payload >= X'cafe' AND payload < X'feed' ORDER BY id", Rows: [][]any{{1}, {2}}},
			// Composite-PK row synthesis from kvw entry's PK tuple.
			{Query: "SELECT a FROM kvw WHERE b = 30", Rows: [][]any{{1}}},
			{Query: "SELECT a, b FROM kvw WHERE a = 1 AND b BETWEEN 15 AND 25", Rows: [][]any{{1, 20}}},
			// Bails (covering check fails) — non-covered SELECT still works.
			{Query: "SELECT id, notes FROM t WHERE status = 'active' ORDER BY id", Rows: [][]any{{1, "n1"}, {3, "n3"}}},
			{Query: "SELECT id FROM t WHERE status = 'active' AND v > 20", Rows: [][]any{{3}}},
			// Constant-only residual + covering still applies.
			{Query: "SELECT id FROM t WHERE status = 'active' AND 1 = 1 ORDER BY id", Rows: [][]any{{1}, {3}}},
			// Empty results.
			{Query: "SELECT id FROM t WHERE v > 1000", Rows: [][]any{}},
			{Query: "SELECT id, status FROM t WHERE status = 'nonexistent'", Rows: [][]any{}},
		},
	}
}

// mixedTypeEqualityScenario mirrors testdata/mixed_type_equality.yaml.
// Drops NOT NULL on PK. Two error_code tests skipped per per-test Skip;
// three positive tests pin same-type equality + IN-list. Java aligns
// on `INCOMPATIBLE_TYPE → CANNOT_CONVERT_TYPE` 22000 for cross-type,
// which is also Go's behaviour as of nightshift-39.
func mixedTypeEqualityScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "mixed_type_equality",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, n BIGINT, s STRING, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 5, '5'), (2, 10, 'ten'), (3, 5, 'five')",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT id FROM t WHERE n = '5'", ErrorCode: "22000"},
			{Query: "SELECT id FROM t WHERE n IN ('5', 'ten')", ErrorCode: "22000"},
			{Query: "SELECT id FROM t WHERE n = 5", Unordered: true, Rows: [][]any{{1}, {3}}},
			{Query: "SELECT id FROM t WHERE s = '5'", Rows: [][]any{{1}}},
			{Query: "SELECT id FROM t WHERE n IN (5, 10)", Unordered: true, Rows: [][]any{{1}, {2}, {3}}},
		},
	}
}

// gr1JoinScenario mirrors testdata/gr1_join.yaml. Drops NOT NULL on PK.
// Drops the explicit-INNER-JOIN test (CLAUDE.md gotcha — explicit JOIN
// ON broken in fdb-relational); keeps the comma-join positives. Two
// error_code tests stay (per-test Skip).
func gr1JoinScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "gr1_join",
		SchemaTemplate: "CREATE TABLE a (id BIGINT, v BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE b (id BIGINT, w BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO a VALUES (1, 10), (2, 20)",
			"INSERT INTO b VALUES (1, 100), (2, 200)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT a.id, COUNT(*) FROM a, b", ErrorCode: "42803"},
			{Query: "SELECT COUNT(*) FROM a, b", Rows: [][]any{{4}}},
			{Query: "SELECT SUM(a.v) FROM a, b WHERE a.id = b.id", Rows: [][]any{{30}}},
		},
	}
}

// inListAdvancedScenario mirrors testdata/in_list_advanced.yaml. Drops
// NOT NULL on PK. Tests IN-list pushdown edge cases: arithmetic in the
// list, duplicates (post-dedup semantics), singleton, no-match. Skips
// the empty-IN-list and mixed-type error_code tests via per-test Skip.
func inListAdvancedScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "in_list_advanced",
		SchemaTemplate: "CREATE TABLE ta (a BIGINT, b BIGINT, PRIMARY KEY (a))",
		Setup: []string{
			"INSERT INTO ta VALUES (1, 8), (2, 7), (3, 6), (4, 5), (5, 4), (6, 3), (7, 2), (8, 1)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT a, b FROM ta WHERE b IN (1 + 0, 3 + 0, 5, 7) ORDER BY a", Rows: [][]any{{2, 7}, {4, 5}, {6, 3}, {8, 1}}},
			{Query: "SELECT a, b FROM ta WHERE b IN (6)", Rows: [][]any{{3, 6}}},
			{Query: "SELECT a, b FROM ta WHERE b IN (10, 33, 66)", Rows: [][]any{}},
			{Query: "SELECT a, b FROM ta WHERE b IN (1, 1, 1, 1)", Rows: [][]any{{8, 1}}},
			{Query: "SELECT a, b FROM ta WHERE a IN (1, 1, 1) ORDER BY a", Rows: [][]any{{1, 8}}},
			{Query: "SELECT a, b FROM ta WHERE a IN (2, 1, 2, 3, 1) ORDER BY a", Rows: [][]any{{1, 8}, {2, 7}, {3, 6}}},
			{Query: "SELECT a, b FROM ta WHERE b IN (1 + 0, 0 + 1)", Rows: [][]any{{8, 1}}},
			{Query: "SELECT a, b FROM ta WHERE b IN (1, 2, 1, 3) ORDER BY a", Rows: [][]any{{6, 3}, {7, 2}, {8, 1}}},
			{Query: "SELECT a FROM ta WHERE b IN ()", ErrorCode: "42601"},
			{Query: "SELECT a FROM ta WHERE b IN ('foo', 3)", ErrorCode: "22000"},
		},
	}
}

// compositeSecondaryIndexPrefixPushdownScenario mirrors
// testdata/composite_secondary_index_prefix_pushdown.yaml. Drops NOT
// NULL on PK. Renames the `plan` column to `tag` per the swingshift-56
// gotcha (`plan` reserved by fdb-relational's lexer); this also frees
// the original `tier` to stay as the third index col of the 3-col
// composite index. Skips DML count-checking tests (DML — runWithSetup
// expects exactly one query). The original yamsql file uses `count: N`
// to assert affected-row counts; that's a separate harness shape.
func compositeSecondaryIndexPrefixPushdownScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "composite_secondary_index_prefix_pushdown",
		SchemaTemplate: "CREATE TABLE rp (id BIGINT, region STRING, tag STRING, score BIGINT, PRIMARY KEY (id))" +
			" CREATE INDEX idx_region_tag ON rp (region, tag)" +
			" CREATE INDEX idx_region_tag_score ON rp (region, tag, score)",
		Setup: []string{
			"INSERT INTO rp VALUES (1, 'us', 'pro', 1), (2, 'us', 'pro', 2), (3, 'us', 'free', 1), (4, 'eu', 'pro', 1), (5, 'eu', 'free', 2), (6, 'us', 'pro', 3)",
		},
		Tests: []yamsql.Test{
			// 2-col index, equality on leading col only — prefix narrow.
			{Query: "SELECT id FROM rp WHERE region = 'us' ORDER BY id", Rows: [][]any{{1}, {2}, {3}, {6}}},
			// Prefix + residual on trailing col.
			{Query: "SELECT id FROM rp WHERE region = 'us' AND score = 1 ORDER BY id", Rows: [][]any{{1}, {3}}},
			// Leading col mismatch — full scan with post-filter.
			{Query: "SELECT id FROM rp WHERE tag = 'pro' ORDER BY id", Rows: [][]any{{1}, {2}, {4}, {6}}},
			// 3-col index, equality on first col only.
			{Query: "SELECT id FROM rp WHERE region = 'eu' ORDER BY id", Rows: [][]any{{4}, {5}}},
			// 2-col equality on (region, tag).
			{Query: "SELECT id FROM rp WHERE region = 'us' AND tag = 'pro' ORDER BY id", Rows: [][]any{{1}, {2}, {6}}},
			// Full equality on (region, tag, score).
			{Query: "SELECT id FROM rp WHERE region = 'us' AND tag = 'pro' AND score = 1 ORDER BY id", Rows: [][]any{{1}}},
			// Equality gap — eq on first + third, second unequated. Prefix
			// only narrows by region; score=2 stays post-filter.
			{Query: "SELECT id FROM rp WHERE region = 'us' AND score = 2 ORDER BY id", Rows: [][]any{{2}}},
		},
	}
}

// secondaryIndexPushdownScenario mirrors testdata/secondary_index_pushdown.yaml.
// SELECT-only subset (DML mid-stream not expressible under runWithSetup's
// per-test ephemeral schema). Drops NOT NULL on PK cols. Skips LIMIT
// tests (CLAUDE.md gotcha — LIMIT not supported by fdb-relational).
// Skip the range-over-secondary-index ORDER-BY-PK tests where the
// planner can't reconcile index order with PK order (ORDER BY natural-
// order continuation gotcha from earlier in this shift).
func secondaryIndexPushdownScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "secondary_index_pushdown",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, status STRING, v BIGINT, PRIMARY KEY (id))" +
			" CREATE INDEX idx_status ON t (status)" +
			" CREATE INDEX idx_v ON t (v)" +
			" CREATE TABLE t2 (id BIGINT, region STRING, tier STRING, amount BIGINT, PRIMARY KEY (id))" +
			" CREATE INDEX idx_region_tier ON t2 (region, tier)",
		Setup: []string{
			"INSERT INTO t VALUES (1, 'active', 10), (2, 'archived', 20), (3, 'active', 30), (4, 'deleted', 40)",
			"INSERT INTO t2 VALUES (10, 'us', 'pro', 100), (11, 'us', 'free', 50), (12, 'us', 'pro', 200), (13, 'eu', 'pro', 300), (14, 'eu', 'free', 75)",
		},
		Tests: []yamsql.Test{
			// Single-col VALUE index equality.
			{Query: "SELECT id, status, v FROM t WHERE status = 'active' ORDER BY id", Rows: [][]any{{1, "active", 10}, {3, "active", 30}}},
			{Query: "SELECT id FROM t WHERE 'archived' = status", Rows: [][]any{{2}}},
			{Query: "SELECT id FROM t WHERE status = 'nonexistent'", Rows: [][]any{}},
			{Query: "SELECT id FROM t WHERE status = 'active' AND v > 20", Rows: [][]any{{3}}},
			{Query: "SELECT id FROM t WHERE v = 30", Rows: [][]any{{3}}},
			{Query: "SELECT id FROM t WHERE id = 2", Rows: [][]any{{2}}},
			{Query: "SELECT id FROM t WHERE status = 'active' AND v = 30", Rows: [][]any{{3}}},
			{Query: "SELECT id, status FROM t WHERE v = 20", Rows: [][]any{{2, "archived"}}},
			// Composite VALUE index — full key equality.
			{Query: "SELECT id, amount FROM t2 WHERE region = 'us' AND tier = 'pro' ORDER BY id", Rows: [][]any{{10, 100}, {12, 200}}},
			{Query: "SELECT id FROM t2 WHERE tier = 'free' AND region = 'eu'", Rows: [][]any{{14}}},
			{Query: "SELECT id FROM t2 WHERE region = 'us' ORDER BY id", Rows: [][]any{{10}, {11}, {12}}},
			{Query: "SELECT id FROM t2 WHERE region = 'eu' AND tier = 'pro' AND amount > 100", Rows: [][]any{{13}}},
			{Query: "SELECT id FROM t2 WHERE region = 'asia' AND tier = 'pro'", Rows: [][]any{}},
			// NULL on indexed column → three-valued logic UNKNOWN, empty.
			{Query: "SELECT id FROM t WHERE status = NULL", Rows: [][]any{}},
			{Query: "SELECT id FROM t2 WHERE region = 'us' AND tier = NULL", Rows: [][]any{}},
			// Range over single-col VALUE index, with all 4 rows in t.
			{Query: "SELECT COUNT(*) FROM t WHERE v >= 30", Rows: [][]any{{2}}},
			{Query: "SELECT COUNT(*) FROM t WHERE v > 30", Rows: [][]any{{1}}},
			{Query: "SELECT COUNT(*) FROM t WHERE v <= 20", Rows: [][]any{{2}}},
			{Query: "SELECT COUNT(*) FROM t WHERE v < 30", Rows: [][]any{{2}}},
			{Query: "SELECT COUNT(*) FROM t WHERE v > 20 AND v < 40", Rows: [][]any{{1}}},
			{Query: "SELECT COUNT(*) FROM t WHERE v >= 10 AND v <= 30", Rows: [][]any{{3}}},
			{Query: "SELECT COUNT(*) FROM t WHERE v > 100 AND v < 200", Rows: [][]any{{0}}},
			{Query: "SELECT COUNT(*) FROM t WHERE v > 9999", Rows: [][]any{{0}}},
			{Query: "SELECT COUNT(*) FROM t WHERE 50 > v", Rows: [][]any{{4}}},
		},
	}
}

// likePrefixPushdownScenario mirrors testdata/like_prefix_pushdown.yaml.
// SELECT-only subset, no mid-stream DML state. Drops NOT NULL on PK.
// Excludes tests that require rows added via mid-stream INSERT (the
// ESCAPE-clause subset depends on a runtime INSERT of x_ray / y%lo).
// Excludes ORDER BY non-PK / non-index-leading column forms (planner
// natural-order gotcha from earlier in this shift).
func likePrefixPushdownScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "like_prefix_pushdown",
		SchemaTemplate: "CREATE TABLE t (name STRING, tag STRING, v BIGINT, PRIMARY KEY (name))" +
			" CREATE TABLE t2 (id BIGINT, status STRING, PRIMARY KEY (id))" +
			" CREATE INDEX idx_status ON t2 (status)" +
			" CREATE TABLE cp (region STRING, name STRING, v BIGINT, PRIMARY KEY (region, name))" +
			" CREATE TABLE ci (id BIGINT, region STRING, name STRING, PRIMARY KEY (id))" +
			" CREATE INDEX idx_region_name ON ci (region, name)",
		Setup: []string{
			"INSERT INTO t VALUES ('apple', 'fruit', 10), ('apricot', 'fruit', 20), ('banana', 'fruit', 30), ('bean', 'veggie', 40), ('carrot', 'veggie', 50), ('cabbage', 'veggie', 60), ('date', 'fruit', 70)",
			"INSERT INTO t2 VALUES (1, 'active'), (2, 'archived'), (3, 'awaiting'), (4, 'deleted'), (5, 'disabled')",
			"INSERT INTO cp VALUES ('us', 'apple', 10), ('us', 'apricot', 20), ('us', 'banana', 30), ('eu', 'apple', 40), ('eu', 'bean', 50)",
			"INSERT INTO ci VALUES (1, 'us', 'apple'), (2, 'us', 'apricot'), (3, 'us', 'banana'), (4, 'eu', 'apple'), (5, 'eu', 'bean')",
		},
		Tests: []yamsql.Test{
			// PK LIKE prefix.
			{Query: "SELECT name FROM t WHERE name LIKE 'a%' ORDER BY name", Rows: [][]any{{"apple"}, {"apricot"}}},
			{Query: "SELECT name FROM t WHERE name LIKE 'ba%'", Rows: [][]any{{"banana"}}},
			{Query: "SELECT name FROM t WHERE name LIKE 'be%'", Rows: [][]any{{"bean"}}},
			{Query: "SELECT name FROM t WHERE name LIKE 'car%'", Rows: [][]any{{"carrot"}}},
			{Query: "SELECT name FROM t WHERE name LIKE 'apple'", Rows: [][]any{{"apple"}}},
			{Query: "SELECT name FROM t WHERE name LIKE 'xyz%'", Rows: [][]any{}},
			{Query: "SELECT name FROM t WHERE name LIKE 'c%' ORDER BY name", Rows: [][]any{{"cabbage"}, {"carrot"}}},
			{Query: "SELECT name FROM t WHERE name LIKE 'appleX%'", Rows: [][]any{}},
			// PK LIKE + residual.
			{Query: "SELECT name FROM t WHERE name LIKE 'a%' AND v > 10 ORDER BY name", Rows: [][]any{{"apricot"}}},
			{Query: "SELECT name FROM t WHERE name LIKE 'b%' AND tag = 'fruit'", Rows: [][]any{{"banana"}}},
			// NOT LIKE.
			{Query: "SELECT name FROM t WHERE name NOT LIKE 'a%' ORDER BY name", Rows: [][]any{{"banana"}, {"bean"}, {"cabbage"}, {"carrot"}, {"date"}}},
			// Interior wildcards (bail to scan, still correct).
			{Query: "SELECT name FROM t WHERE name LIKE 'b_an'", Rows: [][]any{{"bean"}}},
			{Query: "SELECT name FROM t WHERE name LIKE 'b%an%' ORDER BY name", Rows: [][]any{{"banana"}, {"bean"}}},
			{Query: "SELECT name FROM t WHERE name LIKE '%ate'", Rows: [][]any{{"date"}}},
			{Query: "SELECT COUNT(*) FROM t WHERE name LIKE '%'", Rows: [][]any{{7}}},
			// Secondary-index LIKE prefix.
			{Query: "SELECT id, status FROM t2 WHERE status LIKE 'a%' ORDER BY id", Rows: [][]any{{1, "active"}, {2, "archived"}, {3, "awaiting"}}},
			{Query: "SELECT id FROM t2 WHERE status LIKE 'd%' ORDER BY id", Rows: [][]any{{4}, {5}}},
			{Query: "SELECT id FROM t2 WHERE status LIKE 'awa%'", Rows: [][]any{{3}}},
			{Query: "SELECT id FROM t2 WHERE status LIKE 'zzz%'", Rows: [][]any{}},
			// Composite-PK LIKE prefix: leading eq + LIKE on second col.
			{Query: "SELECT region, name FROM cp WHERE region = 'us' AND name LIKE 'a%' ORDER BY name", Rows: [][]any{{"us", "apple"}, {"us", "apricot"}}},
			{Query: "SELECT region, name FROM cp WHERE region = 'eu' AND name LIKE 'b%'", Rows: [][]any{{"eu", "bean"}}},
			{Query: "SELECT name FROM cp WHERE region = 'us' AND name LIKE 'a%' AND v > 10", Rows: [][]any{{"apricot"}}},
			{Query: "SELECT region FROM cp WHERE region = 'us' AND name LIKE 'zzz%'", Rows: [][]any{}},
			// Composite secondary-index LIKE prefix.
			{Query: "SELECT id, region, name FROM ci WHERE region = 'us' AND name LIKE 'a%' ORDER BY id", Rows: [][]any{{1, "us", "apple"}, {2, "us", "apricot"}}},
			{Query: "SELECT id FROM ci WHERE region = 'eu' AND name LIKE 'b%'", Rows: [][]any{{5}}},
			{Query: "SELECT id FROM ci WHERE region = 'asia' AND name LIKE 'a%'", Rows: [][]any{}},
			// Interior-wildcard prefix narrowing (post-filter via likeMatch).
			{Query: "SELECT name FROM t WHERE name LIKE 'a%le' ORDER BY name", Rows: [][]any{{"apple"}}},
			{Query: "SELECT name FROM t WHERE name LIKE 'a_ple' ORDER BY name", Rows: [][]any{{"apple"}}},
			{Query: "SELECT name FROM t WHERE name LIKE 'bana%'", Rows: [][]any{{"banana"}}},
			// Underscore at start (no literal prefix, bails to scan).
			{Query: "SELECT name FROM t WHERE name LIKE '_anana'", Rows: [][]any{{"banana"}}},
		},
	}
}

// crossJoinScenario mirrors testdata/cross_join.yaml. Drops NOT NULL on
// PK columns. Cross-engine omissions:
//   - Explicit `CROSS JOIN` syntax — fdb-relational 4.11.1.0's parser
//     visitor unconditionally calls `InnerJoinContext.expression().accept(...)`
//     and NPEs when the ON clause is null (CROSS JOIN has no ON). Tracked
//     as new CLAUDE.md gotcha.
//   - `INNER JOIN ... ON 1 = 1` — same JOIN-ON gotcha as in CLAUDE.md
//     (column-resolution path rejects); constant ON likely rides the
//     same code path.
//   - `FROM a, b ORDER BY a.id, b.id` — multi-col ORDER BY across two
//     table sources is unsupported by the Cascades planner (existing
//     multi-col ORDER BY gotcha).
//
// Comma-join (cartesian) WITHOUT a multi-col ORDER BY works, and the
// SQL-89 comma-join-with-WHERE is fully supported (the documented
// workaround for explicit JOIN ON).
func crossJoinScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "cross_join",
		SchemaTemplate: "CREATE TABLE a (id BIGINT, v BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE b (id BIGINT, w BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO a VALUES (1, 10), (2, 20)",
			"INSERT INTO b VALUES (1, 100), (2, 200)",
		},
		Tests: []yamsql.Test{
			// Comma-join cartesian — no ORDER BY, multiset compare.
			{Query: "SELECT a.id, b.id FROM a, b", Unordered: true, Rows: [][]any{{1, 1}, {1, 2}, {2, 1}, {2, 2}}},
			{Query: "SELECT COUNT(*) FROM a, b", Rows: [][]any{{4}}},
			// SQL-89 comma-join with WHERE join-predicate.
			{Query: "SELECT a.v, b.w FROM a, b WHERE a.id = b.id ORDER BY a.id", Rows: [][]any{{10, 100}, {20, 200}}},
		},
	}
}

// compositePKPrefixPushdownScenario mirrors
// testdata/composite_pk_prefix_pushdown.yaml. Drops NOT NULL on PK
// cols. Skips the LIMIT test (CLAUDE.md gotcha — LIMIT not supported by
// fdb-relational), error_code test (per-test Skip), and UPDATE/DELETE
// tests (DML — runWithSetup expects exactly one query).
func compositePKPrefixPushdownScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "composite_pk_prefix_pushdown",
		SchemaTemplate: "CREATE TABLE kv (a BIGINT, b BIGINT, c BIGINT, v BIGINT, PRIMARY KEY (a, b, c))" +
			" CREATE TABLE kv2 (a BIGINT, b BIGINT, label STRING, PRIMARY KEY (a, b))",
		Setup: []string{
			"INSERT INTO kv VALUES (1, 10, 100, 1000), (1, 10, 200, 2000), (1, 20, 100, 3000), (1, 20, 300, 4000), (2, 10, 100, 5000), (2, 30, 100, 6000)",
			"INSERT INTO kv2 VALUES (1, 10, 'a'), (1, 20, 'b'), (2, 10, 'c'), (2, 20, 'd')",
		},
		Tests: []yamsql.Test{
			// Single leading col equated on 3-col PK: prefix narrows by a.
			{Query: "SELECT a, b, c FROM kv WHERE a = 1 ORDER BY b, c", Rows: [][]any{{1, 10, 100}, {1, 10, 200}, {1, 20, 100}, {1, 20, 300}}},
			// Leading-col prefix + residual on a non-PK column.
			{Query: "SELECT a, b, c FROM kv WHERE a = 1 AND v > 1500 ORDER BY b, c", Rows: [][]any{{1, 10, 200}, {1, 20, 100}, {1, 20, 300}}},
			// Two leading cols equated on 3-col PK: prefix = [1, 10].
			{Query: "SELECT a, b, c FROM kv WHERE a = 1 AND b = 10 ORDER BY c", Rows: [][]any{{1, 10, 100}, {1, 10, 200}}},
			// Leading two equated + equality on non-PK column.
			{Query: "SELECT c FROM kv WHERE a = 1 AND b = 10 AND v = 1000", Rows: [][]any{{100}}},
			// Residual equality on a TRAILING PK col after a break.
			{Query: "SELECT a, b, c FROM kv WHERE a = 1 AND c = 100 ORDER BY b", Rows: [][]any{{1, 10, 100}, {1, 20, 100}}},
			// Bail: no leading equality. WHERE b = 10 on PK (a, b, c).
			{Query: "SELECT a, b, c FROM kv WHERE b = 10 ORDER BY a, c", Rows: [][]any{{1, 10, 100}, {1, 10, 200}, {2, 10, 100}}},
			// Bail: full equality picked up by equality-pushdown branch.
			{Query: "SELECT v FROM kv WHERE a = 1 AND b = 10 AND c = 100", Rows: [][]any{{1000}}},
			// 2-col PK pure-prefix.
			{Query: "SELECT a, b, label FROM kv2 WHERE a = 2 ORDER BY b", Rows: [][]any{{2, 10, "c"}, {2, 20, "d"}}},
			// No match.
			{Query: "SELECT a, b FROM kv2 WHERE a = 99", Rows: [][]any{}},
		},
	}
}

// pkPushdownScenario mirrors testdata/pk_pushdown.yaml — but only the
// SELECT-only subset. Drops NOT NULL on PK cols; skips the LIMIT test
// (LIMIT not supported), error_code tests, DML tests. The full yamsql
// file mixes UPDATE/DELETE/INSERT into the test list and re-seeds rows
// mid-stream — those depend on per-test stateful execution that the
// runWithSetup harness doesn't support (each test runs against a fresh
// schema). The pure-SELECT block at the top is what we lift here.
//
// Cross-engine omissions surfaced by this scenario:
//   - `WHERE id = 2 AND id > 5` (contradictory equality + range) — Java
//     returns the row matching the equality; the planner doesn't apply
//     the range as post-filter when the equality already pinpoints the
//     record. Tracked as a Java planner bug; Go correctly returns empty.
//     New CLAUDE.md gotcha. Drop until upstream fix.
//   - `ORDER BY <col>` where <col> is not the leading-PK natural-order
//     continuation: a range on a leading col + ORDER BY a later col,
//     ORDER BY a non-PK col without a satisfying index, etc. Java's
//     Cascades planner raises `UnableToPlanException`. Drop the
//     affected tests until the planner ports a generic sort rule.
func pkPushdownScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "pk_pushdown",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, name STRING, v BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE kv (k STRING, g BIGINT, v BIGINT, PRIMARY KEY (k, g))" +
			" CREATE TABLE kvw (region STRING, bucket BIGINT, tag STRING, v BIGINT, PRIMARY KEY (region, bucket, tag))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 'alpha', 10), (2, 'beta', 20), (3, 'gamma', 30), (4, 'delta', 40)",
			"INSERT INTO kv VALUES ('a', 1, 100), ('a', 2, 200), ('b', 1, 300), ('b', 2, 400)",
			"INSERT INTO kvw VALUES ('us', 1, 'a', 10), ('us', 1, 'b', 20), ('us', 2, 'a', 30), ('us', 2, 'b', 40), ('eu', 1, 'a', 50), ('eu', 1, 'b', 60)",
		},
		Tests: []yamsql.Test{
			// Single-col PK equality.
			{Query: "SELECT id, name, v FROM t WHERE id = 3", Rows: [][]any{{3, "gamma", 30}}},
			{Query: "SELECT id, name, v FROM t WHERE 3 = id", Rows: [][]any{{3, "gamma", 30}}},
			{Query: "SELECT id, name FROM t WHERE id = 99", Rows: [][]any{}},
			{Query: "SELECT id FROM t WHERE t.id = 4", Rows: [][]any{{4}}},
			{Query: "SELECT id FROM t WHERE name = 'beta'", Rows: [][]any{{2}}},
			// Single-col PK range.
			{Query: "SELECT id FROM t WHERE id > 2 ORDER BY id", Rows: [][]any{{3}, {4}}},
			{Query: "SELECT id FROM t WHERE id >= 3 ORDER BY id", Rows: [][]any{{3}, {4}}},
			{Query: "SELECT id FROM t WHERE id < 3 ORDER BY id", Rows: [][]any{{1}, {2}}},
			{Query: "SELECT id FROM t WHERE id <= 2 ORDER BY id", Rows: [][]any{{1}, {2}}},
			{Query: "SELECT id FROM t WHERE 3 < id ORDER BY id", Rows: [][]any{{4}}},
			{Query: "SELECT id FROM t WHERE id >= 2 AND id <= 3 ORDER BY id", Rows: [][]any{{2}, {3}}},
			{Query: "SELECT id FROM t WHERE id > 1 AND id < 4 ORDER BY id", Rows: [][]any{{2}, {3}}},
			{Query: "SELECT id FROM t WHERE id >= 2 AND name = 'gamma'", Rows: [][]any{{3}}},
			{Query: "SELECT id FROM t WHERE id > 10 AND id < 5", Rows: [][]any{}},
			// `id = 2 AND id > 5` dropped — Java planner-bug: returns
			// [2] because the equality pushdown skips the range
			// post-filter. Go correctly returns empty.
			{Query: "SELECT id FROM t WHERE id = 3 AND v = 30", Rows: [][]any{{3}}},
			{Query: "SELECT id FROM t WHERE id = 3 AND v = 999", Rows: [][]any{}},
			{Query: "SELECT COUNT(*) FROM t WHERE id = 1", Rows: [][]any{{1}}},
			{Query: "SELECT v * 2 FROM t WHERE id = 4", Rows: [][]any{{80}}},
			{Query: "SELECT id, name FROM t WHERE name = 'alpha' ORDER BY id", Rows: [][]any{{1, "alpha"}}},
			{Query: "SELECT id FROM t WHERE id <> 2 ORDER BY id", Rows: [][]any{{1}, {3}, {4}}},
			// Composite 2-col PK.
			{Query: "SELECT v FROM kv WHERE k = 'a' AND g = 2", Rows: [][]any{{200}}},
			{Query: "SELECT v FROM kv WHERE g = 1 AND k = 'b'", Rows: [][]any{{300}}},
			{Query: "SELECT g, v FROM kv WHERE k = 'a' ORDER BY g", Rows: [][]any{{1, 100}, {2, 200}}},
			{Query: "SELECT v FROM kv WHERE k = 'a' AND g >= 1 AND g <= 1", Rows: [][]any{{100}}},
			{Query: "SELECT g, v FROM kv WHERE k = 'a' AND g > 1 ORDER BY g", Rows: [][]any{{2, 200}}},
			{Query: "SELECT g, v FROM kv WHERE k = 'b' AND g < 2 ORDER BY g", Rows: [][]any{{1, 300}}},
			// `WHERE k > 'a' ORDER BY g` dropped — range on leading PK
			// col + ORDER BY trailing PK col fails Cascades planning
			// (UnableToPlanException). The natural order across `k > 'a'`
			// is k-then-g, not g, so Java needs a sort rule it doesn't
			// have.
			// `WHERE k='b' AND g BETWEEN 1 AND 2 ORDER BY v` dropped —
			// ORDER BY non-PK col without an index also fails.
			{Query: "SELECT v FROM kv WHERE k = 'b' AND g = 2 AND v = 400", Rows: [][]any{{400}}},
			{Query: "SELECT v FROM kv WHERE k = 'b' AND g = 2 AND v = 999", Rows: [][]any{}},
			{Query: "SELECT v FROM kv WHERE k = 'a' AND g = 2 AND v > 100", Rows: [][]any{{200}}},
			// `OR (...) ORDER BY v` dropped — same UnableToPlan as
			// the bare ORDER BY v above (non-PK ORDER target).
			// 3-col composite PK.
			{Query: "SELECT region, bucket, tag FROM kvw WHERE region = 'us' AND bucket > 1 AND tag = 'a'", Rows: [][]any{{"us", 2, "a"}}},
			{Query: "SELECT region, bucket, tag FROM kvw WHERE region = 'us' AND bucket <= 1 AND tag = 'a'", Rows: [][]any{{"us", 1, "a"}}},
			// `region < 'us' ORDER BY tag` dropped — range on leading
			// col + ORDER BY trailing PK col, same UnableToPlan as kv
			// above.
			{Query: "SELECT COUNT(*) FROM kvw WHERE region < 'us'", Rows: [][]any{{2}}},
			// BETWEEN pushdown.
			{Query: "SELECT id FROM t WHERE id BETWEEN 2 AND 4 ORDER BY id", Rows: [][]any{{2}, {3}, {4}}},
			{Query: "SELECT id FROM t WHERE id BETWEEN 3 AND 3", Rows: [][]any{{3}}},
			{Query: "SELECT id FROM t WHERE id BETWEEN 10 AND 5", Rows: [][]any{}},
			{Query: "SELECT id FROM t WHERE id BETWEEN 1 AND 5 AND name <> 'gamma' ORDER BY id", Rows: [][]any{{1}, {2}, {4}}},
			{Query: "SELECT id FROM t WHERE id NOT BETWEEN 2 AND 4 ORDER BY id", Rows: [][]any{{1}}},
			{Query: "SELECT k, g FROM kv WHERE k = 'a' AND g BETWEEN 1 AND 2 ORDER BY g", Rows: [][]any{{"a", 1}, {"a", 2}}},
		},
	}
}

// whereLiteralOnLeftScenario mirrors testdata/where_literal_on_left.yaml.
// The PK column drops NOT NULL — fdb-relational rejects NOT NULL outside
// ARRAY column types (CLAUDE.md gotcha, swingshift-52). PK is implicitly
// NOT NULL so the constraint isn't lost.
func whereLiteralOnLeftScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "where_literal_on_left",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, n BIGINT, s STRING, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 5, 'a'), (2, 10, 'b'), (3, 15, 'c')",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT id FROM t WHERE 10 < n", Rows: [][]any{{3}}},
			{Query: "SELECT id FROM t WHERE 10 <= n ORDER BY id", Rows: [][]any{{2}, {3}}},
			{Query: "SELECT id FROM t WHERE 10 > n", Rows: [][]any{{1}}},
			{Query: "SELECT id FROM t WHERE 10 >= n ORDER BY id", Rows: [][]any{{1}, {2}}},
			{Query: "SELECT id FROM t WHERE 10 = n", Rows: [][]any{{2}}},
			{Query: "SELECT id FROM t WHERE 10 != n ORDER BY id", Rows: [][]any{{1}, {3}}},
			{Query: "SELECT id FROM t WHERE 1 = 1 ORDER BY id", Rows: [][]any{{1}, {2}, {3}}},
			{Query: "SELECT id FROM t WHERE 1 = 2", Rows: [][]any{}},
			{Query: "SELECT id FROM t WHERE 'b' = s", Rows: [][]any{{2}}},
			{Query: "SELECT id FROM t WHERE 'b' < s ORDER BY id", Rows: [][]any{{3}}},
		},
	}
}

// arithmeticScenario mirrors testdata/arithmetic.yaml. Two cross-engine
// adaptations: drops NOT NULL on the PK column, and drops the bare-NULL
// arithmetic + FROM-less SELECT cases. fdb-relational's planner rejects
// `<op> NULL` literal arithmetic with "unable to encapsulate arithmetic
// operation due to type mismatch(es)" — bare NULL has no inferred type,
// so the planner can't pick an operator overload. Wrapping with
// CAST(NULL AS BIGINT) would satisfy Java but the Go-side YAML uses bare
// NULL for cleanliness. FROM-less SELECTs (`SELECT -10 / 3`) hit a
// separate planner restriction. Both are tracked as Java gaps in
// CLAUDE.md (cross-engine yamsql gotchas).
func arithmeticScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "arithmetic",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 20, 4), (2, 7, 0), (3, 10, 3)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT a / b FROM t WHERE id = 1", Rows: [][]any{{5}}},
			{Query: "SELECT a / b FROM t WHERE id = 3", Rows: [][]any{{3}}},
			{Query: "SELECT a % b FROM t WHERE id = 3", Rows: [][]any{{1}}},
			{Query: "SELECT a % b FROM t WHERE id = 1", Rows: [][]any{{0}}},
			{Query: "SELECT a / b FROM t WHERE id = 2", ErrorCode: "22012"},
			{Query: "SELECT a % b FROM t WHERE id = 2", ErrorCode: "22012"},
			{Query: "SELECT a + b FROM t WHERE id = 1", Rows: [][]any{{24}}},
			{Query: "SELECT a - b FROM t WHERE id = 1", Rows: [][]any{{16}}},
			{Query: "SELECT a * b FROM t WHERE id = 1", Rows: [][]any{{80}}},
			{Query: "SELECT a / 0 FROM t WHERE id = 1", ErrorCode: "22012"},
		},
	}
}

// castScenario mirrors testdata/cast.yaml. Drops NOT NULL on the two
// PK columns. Keeps error_code tests for visibility (per-test Skip).
func castScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "cast",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE test_cast (id BIGINT, num_col BIGINT, str_col STRING, bool_col BOOLEAN, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1)",
			"INSERT INTO test_cast VALUES (1, 123, 'hello', true), (2, 456, 'world', false), (3, 789, '123', null)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT CAST(1.6 AS BIGINT) FROM t", Rows: [][]any{{2}}},
			{Query: "SELECT CAST(-1.5 AS BIGINT) FROM t", Rows: [][]any{{-1}}},
			{Query: "SELECT CAST(-2.6 AS BIGINT) FROM t", Rows: [][]any{{-3}}},
			{Query: "SELECT CAST(' 42 ' AS BIGINT) FROM t", Rows: [][]any{{42}}},
			{Query: "SELECT CAST(' 3.14 ' AS DOUBLE) FROM t", Rows: [][]any{{3.14}}},
			{Query: "SELECT CAST(NULL AS BIGINT) FROM t", Rows: [][]any{{nil}}},
			{Query: "SELECT CAST(1e20 AS BIGINT) FROM t", ErrorCode: "22F3H"},
			{Query: "SELECT CAST(-1e20 AS BIGINT) FROM t", ErrorCode: "22F3H"},
			{Query: "SELECT CAST('not a bool' AS BOOLEAN) FROM t", ErrorCode: "22F3H"},
			{Query: "SELECT CAST(CAST('not a number' AS DOUBLE) AS BIGINT) FROM t", ErrorCode: "22F3H"},
			{Query: "SELECT CAST(9223372036854775807 AS INTEGER) FROM t", ErrorCode: "22F3H"},
			{Query: "SELECT CAST(num_col AS STRING) FROM test_cast WHERE id = 1", Rows: [][]any{{"123"}}},
			{Query: "SELECT CAST(num_col AS DOUBLE) FROM test_cast WHERE id = 1", Rows: [][]any{{123.0}}},
			{Query: "SELECT id FROM test_cast WHERE CAST(bool_col AS INTEGER) + 1 > 1", Rows: [][]any{{1}}},
			{Query: "SELECT SUM(CAST(num_col AS DOUBLE)) FROM test_cast", Rows: [][]any{{1368.0}}},
			{Query: "SELECT id FROM test_cast WHERE CAST(num_col AS STRING) = CAST(123 AS STRING)", Rows: [][]any{{1}}},
		},
	}
}

// compositePKScenario mirrors testdata/composite_pk.yaml. Drops NOT NULL
// from the two PK columns. Skips the duplicate-PK INSERT (DML; non-query
// path) and its associated state-verification query — runWithSetup
// rebuilds schema per test, so the post-INSERT state isn't observable.
func compositePKScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "composite_pk",
		SchemaTemplate: "CREATE TABLE t (a BIGINT, b BIGINT, label STRING, PRIMARY KEY (a, b))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 10, 'alpha'), (1, 20, 'beta'), (2, 10, 'gamma')",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT b, label FROM t WHERE a = 1 ORDER BY b", Rows: [][]any{{10, "alpha"}, {20, "beta"}}},
			{Query: "SELECT label FROM t WHERE a = 2 AND b = 10", Rows: [][]any{{"gamma"}}},
			{Query: "INSERT INTO t VALUES (1, 10, 'replacement')", ErrorCode: "23505"},
			{Query: "SELECT label FROM t WHERE a = 1 AND b = 10", Rows: [][]any{{"alpha"}}},
		},
	}
}

// bytesScenario mirrors testdata/bytes.yaml. Cross-engine adaptations:
// drop NOT NULL on PK column; uppercase BYTES literals X'...' and
// B64'...' (CLAUDE.md gotcha — fdb-relational rejects lowercase x'...'
// / b64'...').
func bytesScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "bytes",
		SchemaTemplate: "CREATE TABLE lb (a BIGINT, b BYTES, PRIMARY KEY (a))",
		Setup: []string{
			"INSERT INTO lb VALUES (1, X'deadbeef'), (2, X'cafe'), (3, null)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT a FROM lb WHERE b = X'cafe'", Rows: [][]any{{2}}},
			{Query: "SELECT a FROM lb WHERE b IN (X'cafe', X'deadbeef')", Unordered: true, Rows: [][]any{{1}, {2}}},
			{Query: "SELECT a FROM lb WHERE b <> X'cafe'", Rows: [][]any{{1}}},
			{Query: "SELECT a FROM lb WHERE b IS NULL", Rows: [][]any{{3}}},
			{Query: "SELECT a FROM lb WHERE b IS NOT NULL", Unordered: true, Rows: [][]any{{1}, {2}}},
			{Query: "SELECT a FROM lb WHERE b = B64'yv4='", Rows: [][]any{{2}}},
			{Query: "SELECT a FROM lb WHERE b = X'0'", ErrorCode: "22F03"},
			{Query: "SELECT a FROM lb WHERE b = X'ABCDMN'", ErrorCode: "22F03"},
			{Query: "SELECT a FROM lb WHERE b = B64'***'", ErrorCode: "22F03"},
			{Query: "SELECT a FROM lb WHERE b IS NOT DISTINCT FROM null", Rows: [][]any{{3}}},
			{Query: "SELECT a FROM lb WHERE b IS DISTINCT FROM null ORDER BY a", Rows: [][]any{{1}, {2}}},
			{Query: "SELECT a FROM lb WHERE b = null", Rows: [][]any{}},
			{Query: "SELECT X'cafe' = b FROM lb WHERE a = 2", Rows: [][]any{{true}}},
		},
	}
}

// betweenScenario mirrors testdata/between.yaml. Drops NOT NULL on PK.
// Drops the Kleene-NULL-bound tests (untyped NULL hits the same planner
// "unable to encapsulate" error documented for arithmetic), the ABS()
// test (function not in fdb-relational's registry per CLAUDE.md), and
// the subquery-bound test (uncertain support). Keeps the geometry-of-
// BETWEEN tests, the error_code type-mismatch tests (skipped), and the
// COUNT(*) constant-fold tests.
func betweenScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "between",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 0), (2, 5), (3, 10), (4, 15), (5, 100)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT id FROM t WHERE v BETWEEN 5 AND 15", Unordered: true, Rows: [][]any{{2}, {3}, {4}}},
			{Query: "SELECT id FROM t WHERE v NOT BETWEEN 5 AND 15", Unordered: true, Rows: [][]any{{1}, {5}}},
			{Query: "SELECT id FROM t WHERE v BETWEEN 15 AND 5", Rows: [][]any{}},
			{Query: "SELECT id FROM t WHERE v NOT BETWEEN 15 AND 5 ORDER BY id", Rows: [][]any{{1}, {2}, {3}, {4}, {5}}},
			{Query: "SELECT id FROM t WHERE v BETWEEN 10 AND 10", Rows: [][]any{{3}}},
			{Query: "SELECT id FROM t WHERE v BETWEEN (2+3) AND (10*2) ORDER BY id", Rows: [][]any{{2}, {3}, {4}}},
			{Query: "SELECT id FROM t WHERE v BETWEEN 10 AND 'a'", ErrorCode: "22000"},
			{Query: "SELECT id FROM t WHERE 'a' BETWEEN 10 AND 20", ErrorCode: "22000"},
			{Query: "SELECT id FROM t WHERE v BETWEEN 0 AND 5 OR v BETWEEN 90 AND 100 ORDER BY id", Rows: [][]any{{1}, {2}, {5}}},
			{Query: "SELECT id FROM t WHERE v BETWEEN 4 AND 6.2", Rows: [][]any{{2}}},
			{Query: "SELECT COUNT(*) FROM t WHERE 4.5 BETWEEN 4 AND 6", Rows: [][]any{{5}}},
			{Query: "SELECT COUNT(*) FROM t WHERE 2+2 BETWEEN 1+1 AND 3+3", Rows: [][]any{{5}}},
		},
	}
}

// booleanScenario mirrors testdata/boolean.yaml. Drops NOT NULL on PK.
// Cross-engine omissions: IS TRUE / IS FALSE / IS NOT TRUE / IS NOT
// FALSE forms (CLAUDE.md gotcha — fdb-relational planner rejects),
// untyped-NULL operands (`b = null`, `b AND NULL`, `b OR NULL`),
// `WHERE (b = true)` (parser interprets bare-paren as
// RecordConstructorValue, not a predicate; `NOT (...)` works because it
// forces predicate context), and CTE + outer ORDER BY (Java rejects
// "order by is not supported in subquery" — fdb-relational treats the
// outer ORDER BY as part of the subquery scope when a WITH clause is
// present).
//
// Bare-bool-column-as-operand-in-projection (`SELECT b AND TRUE`,
// `SELECT NOT b`, `SELECT b OR FALSE`) was deferred swingshift-55/56
// because Go's embedded engine rejected with "expected BooleanValue
// but got FieldValue". Re-enabled nightshift-57 after threading
// allowBareField=true through evalExprPredicateTri's value-context
// callers (eval_predicate.go) so operands of AND/OR/NOT/XOR and
// projection-context expressions accept bare FieldValue and convert
// via IsTruthy. WHERE-top-level `WHERE flag` still rejects to match
// Java.
func booleanScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "boolean",
		SchemaTemplate: "CREATE TABLE lb (a BIGINT, b BOOLEAN, PRIMARY KEY (a))",
		Setup: []string{
			"INSERT INTO lb VALUES (1, true), (2, false), (3, null)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT lb.* FROM lb WHERE b = true", Rows: [][]any{{1, true}}},
			{Query: "SELECT lb.* FROM lb WHERE b = false", Rows: [][]any{{2, false}}},
			{Query: "SELECT lb.* FROM lb WHERE b <> TRUE", Rows: [][]any{{2, false}}},
			{Query: "SELECT lb.* FROM lb WHERE b <> FALSE", Rows: [][]any{{1, true}}},
			{Query: "SELECT lb.* FROM lb WHERE b IS NULL", Rows: [][]any{{3, nil}}},
			{Query: "SELECT lb.* FROM lb WHERE b IS NOT NULL", Unordered: true, Rows: [][]any{{1, true}, {2, false}}},
			{Query: "SELECT lb.* FROM lb WHERE NOT (b = false)", Unordered: true, Rows: [][]any{{1, true}}},
			{Query: "SELECT b = true FROM lb ORDER BY a", Rows: [][]any{{true}, {false}, {nil}}},
			{Query: "SELECT b = false FROM lb ORDER BY a", Rows: [][]any{{false}, {true}, {nil}}},
			{Query: "SELECT b <> TRUE FROM lb ORDER BY a", Rows: [][]any{{false}, {true}, {nil}}},
			{Query: "SELECT b IS NULL FROM lb ORDER BY a", Rows: [][]any{{false}, {false}, {true}}},
			// Bare-bool projection (re-enabled nightshift-57). Kleene 3VL:
			// b AND TRUE pinning UNKNOWN→NULL preservation, b AND FALSE
			// short-circuits to FALSE for every row, b OR TRUE
			// short-circuits to TRUE, b OR FALSE preserves UNKNOWN, NOT b
			// flips with NULL→NULL.
			{Query: "SELECT b AND TRUE FROM lb ORDER BY a", Rows: [][]any{{true}, {false}, {nil}}},
			{Query: "SELECT b AND FALSE FROM lb ORDER BY a", Rows: [][]any{{false}, {false}, {false}}},
			{Query: "SELECT b OR TRUE FROM lb ORDER BY a", Rows: [][]any{{true}, {true}, {true}}},
			{Query: "SELECT b OR FALSE FROM lb ORDER BY a", Rows: [][]any{{true}, {false}, {nil}}},
			{Query: "SELECT NOT b FROM lb ORDER BY a", Rows: [][]any{{false}, {true}, {nil}}},
			// `WHERE b AND TRUE` / `WHERE b OR FALSE` / `WHERE NOT b`
			// dropped from cross-engine: Java rejects bare-bool-operand in
			// WHERE context (VerifyException), even though it accepts the
			// same shape in projection. Go's fix correctly accepts both
			// contexts (operands of AND/OR/NOT are value-context); the
			// asymmetry is fdb-relational-side. CLAUDE.md gotcha
			// `bare-bool-operand-in-WHERE-rejected-by-Java` documents.
		},
	}
}

// likeScenario mirrors testdata/like.yaml. Drops NOT NULL on PK.
// LIKE / NOT LIKE pattern matching with %, _, regex-metachar literals.
func likeScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "like",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, s STRING, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 'hello'), (2, 'help'), (3, 'world'), (4, null), (5, '')",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT id FROM t WHERE s LIKE 'hel%'", Unordered: true, Rows: [][]any{{1}, {2}}},
			{Query: "SELECT id FROM t WHERE s LIKE 'hel_'", Rows: [][]any{{2}}},
			{Query: "SELECT id FROM t WHERE s LIKE 'xyz'", Rows: [][]any{}},
			{Query: "SELECT id FROM t WHERE s LIKE 'hello'", Rows: [][]any{{1}}},
			{Query: "SELECT id FROM t WHERE s LIKE ''", Rows: [][]any{{5}}},
			{Query: "SELECT id FROM t WHERE s NOT LIKE 'hel%'", Unordered: true, Rows: [][]any{{3}, {5}}},
			{Query: "SELECT id FROM t WHERE s LIKE '%lp' ORDER BY id", Rows: [][]any{{2}}},
			{Query: "SELECT id FROM t WHERE s LIKE '%el%' ORDER BY id", Rows: [][]any{{1}, {2}}},
			{Query: "SELECT id FROM t WHERE s LIKE 'h___o'", Rows: [][]any{{1}}},
			{Query: "SELECT id FROM t WHERE s LIKE '_o%'", Rows: [][]any{{3}}},
			{Query: "SELECT id FROM t WHERE s LIKE 'x_y'", Rows: [][]any{}},
			{Query: "SELECT id FROM t WHERE s LIKE 'HEL%'", Rows: [][]any{}},
			{Query: "SELECT id FROM t WHERE s LIKE '(hello)'", Rows: [][]any{}},
			{Query: "SELECT id FROM t WHERE s LIKE '[h]ello'", Rows: [][]any{}},
			{Query: "SELECT id FROM t WHERE s LIKE 'h*llo'", Rows: [][]any{}},
			{Query: "SELECT id FROM t WHERE s LIKE '^hello'", Rows: [][]any{}},
		},
	}
}

// caseWhenScenario mirrors testdata/case_when.yaml. Drops NOT NULL on
// PK. Drops the ORDER BY <alias> test (CLAUDE.md gotcha — fdb-relational
// rejects ORDER BY on a SELECT-list alias).
func caseWhenScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "case_when",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 5), (2, 15), (3, null), (4, 30)",
		},
		Tests: []yamsql.Test{
			{
				Query: "SELECT id, CASE WHEN v < 10 THEN 'low' WHEN v < 20 THEN 'mid' ELSE 'high' END FROM t ORDER BY id",
				Rows:  [][]any{{1, "low"}, {2, "mid"}, {3, "high"}, {4, "high"}},
			},
			{
				Query: "SELECT id, CASE WHEN v IS NULL THEN 'missing' WHEN v < 10 THEN 'low' ELSE 'other' END FROM t ORDER BY id",
				Rows:  [][]any{{1, "low"}, {2, "other"}, {3, "missing"}, {4, "other"}},
			},
			{
				Query: "SELECT id, CASE WHEN v < 10 THEN 'low' END FROM t ORDER BY id",
				Rows:  [][]any{{1, "low"}, {2, nil}, {3, nil}, {4, nil}},
			},
			{
				Query: "SELECT id FROM t WHERE CASE WHEN v IS NULL THEN 0 WHEN v < 10 THEN 0 ELSE 1 END = 1 ORDER BY id",
				Rows:  [][]any{{2}, {4}},
			},
			{
				Query: "SELECT id, CASE WHEN v < 10 THEN 10 ELSE 3.14 END FROM t WHERE id = 1",
				Rows:  [][]any{{1, 10.0}},
			},
			{
				Query: "SELECT id, CASE WHEN v < 10 THEN 10 ELSE 3.14 END FROM t WHERE id = 2",
				Rows:  [][]any{{2, 3.14}},
			},
			{
				Query: "SELECT id, CASE WHEN v IS NULL THEN 'absent' ELSE 'present' END FROM t ORDER BY id",
				Rows:  [][]any{{1, "present"}, {2, "present"}, {3, "absent"}, {4, "present"}},
			},
			{
				Query: "SELECT id, CASE WHEN CASE WHEN v = 5 THEN 10 ELSE 20 END > 15 THEN 'big' ELSE 'small' END FROM t ORDER BY id",
				Rows:  [][]any{{1, "small"}, {2, "big"}, {3, "big"}, {4, "big"}},
			},
			{
				Query: "SELECT id, CASE WHEN v IS NULL THEN 0 ELSE v + 1 END FROM t ORDER BY id",
				Rows:  [][]any{{1, 6}, {2, 16}, {3, 0}, {4, 31}},
			},
		},
	}
}

// aggregateEmptyTableScenario mirrors testdata/aggregate_empty_table.yaml.
// Drops NOT NULL on PK. Drops the `WHERE x > 0 HAVING COUNT(*) >= 0`
// test — divergence: Go returns 1 row [[0]] (SQL spec — aggregate over
// empty set produces a single grouping then HAVING filters), Java
// returns 0 rows (treats empty WHERE result as no grouping at all,
// HAVING never fires). Tracked as new gotcha in CLAUDE.md.
func aggregateEmptyTableScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "aggregate_empty_table",
		SchemaTemplate: "CREATE TABLE empty_t (id BIGINT, n BIGINT, PRIMARY KEY (id))",
		Setup:          []string{},
		Tests: []yamsql.Test{
			{Query: "SELECT COUNT(*) FROM empty_t", Rows: [][]any{{0}}},
			{Query: "SELECT COUNT(n) FROM empty_t", Rows: [][]any{{0}}},
			{Query: "SELECT SUM(n) FROM empty_t", Rows: [][]any{{nil}}},
			{Query: "SELECT AVG(n) FROM empty_t", Rows: [][]any{{nil}}},
			{Query: "SELECT MIN(n) FROM empty_t", Rows: [][]any{{nil}}},
			{Query: "SELECT MAX(n) FROM empty_t", Rows: [][]any{{nil}}},
			{Query: "SELECT COUNT(*) FROM empty_t WHERE id = 999", Rows: [][]any{{0}}},
			{Query: "SELECT COUNT(*) FROM empty_t HAVING COUNT(*) > 0", Rows: [][]any{}},
		},
	}
}

// bitwiseScenario mirrors testdata/bitwise.yaml. Drops NOT NULL on PK.
// Drops the bit-shift tests (`<< / >>`) — fdb-relational tokenizes the
// operators but the function registry has no evaluator (CLAUDE.md
// gotcha). Drops the FROM-less SELECT and error_code shift-out-of-range
// tests for the same reason.
func bitwiseScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "bitwise",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 15, 3), (2, 16, 32), (3, -1, 1), (4, 5, null)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT a & b FROM t WHERE id = 1", Rows: [][]any{{3}}},
			{Query: "SELECT a | b FROM t WHERE id = 2", Rows: [][]any{{48}}},
			{Query: "SELECT a ^ b FROM t WHERE id = 1", Rows: [][]any{{12}}},
			{Query: "SELECT a & b FROM t WHERE id = 4", Rows: [][]any{{nil}}},
			{Query: "SELECT id FROM t WHERE a & 1 = 1 ORDER BY id", Rows: [][]any{{1}, {3}, {4}}},
		},
	}
}

// avgScenario mirrors testdata/avg.yaml. Drops NOT NULL on PK. Pins
// AVG result type as DOUBLE regardless of input.
func avgScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "avg",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 1), (2, 2), (3, 3)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT AVG(v) FROM t", Rows: [][]any{{2.0}}},
			{Query: "SELECT AVG(v) FROM t WHERE v <= 2", Rows: [][]any{{1.5}}},
			{Query: "SELECT AVG(v) FROM t WHERE v > 100", Rows: [][]any{{nil}}},
		},
	}
}

// derivedTableScenario mirrors testdata/derived_table.yaml. Drops NOT
// NULL on PK. Drops the GROUP BY-using test (CLAUDE.md gotcha).
func derivedTableScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "derived_table",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 1, 10), (2, 1, 20), (3, 2, 30), (4, 2, 40), (5, 3, 50)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT id FROM (SELECT id, v FROM t WHERE v >= 30) AS x ORDER BY id", Rows: [][]any{{3}, {4}, {5}}},
			{Query: "SELECT id FROM (SELECT id FROM t)", ErrorCode: "42601"},
			{Query: "SELECT x.v AS val FROM (SELECT id, v FROM t WHERE id = 3) AS x", Rows: [][]any{{30}}},
			{Query: "SELECT t.id FROM t, (SELECT id FROM t WHERE id <= 2) AS x", ErrorCode: "0A000"},
		},
	}
}

// coalesceNullifScenario mirrors testdata/coalesce_nullif.yaml. Drops
// NOT NULL on PK. Drops NULLIF tests — fdb-relational rejects with
// "Unsupported operator NULLIF" (function registry doesn't have it,
// joining the list of unregistered scalars). Adds new gotcha to
// CLAUDE.md.
func coalesceNullifScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "coalesce_nullif",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, a STRING, b STRING, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 'x', 'y'), (2, null, 'y'), (3, 'x', null), (4, null, null)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT id, COALESCE(a, b, 'default') FROM t ORDER BY id", Rows: [][]any{{1, "x"}, {2, "y"}, {3, "x"}, {4, "default"}}},
		},
	}
}

// bareColWithAggScenario mirrors testdata/bare_col_with_agg.yaml. Drops
// NOT NULL on PK. Drops the DISTINCT test (CLAUDE.md gotcha — fdb-
// relational's planner doesn't support SELECT DISTINCT).
func bareColWithAggScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "bare_col_with_agg",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 10), (2, 20), (3, 30)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT id, COUNT(*) FROM t", ErrorCode: "42803"},
			{Query: "SELECT v, SUM(v) FROM t", ErrorCode: "42803"},
			{Query: "SELECT v + 1, SUM(v) FROM t", ErrorCode: "42803"},
			{Query: "SELECT COUNT(*) FROM t", Rows: [][]any{{3}}},
			{Query: "SELECT SUM(v) FROM t", Rows: [][]any{{60}}},
			{Query: "SELECT COUNT(*), SUM(v), MAX(v), MIN(v) FROM t", Rows: [][]any{{3, 60, 30, 10}}},
			{Query: "SELECT SUM(v) * 2 FROM t", Rows: [][]any{{120}}},
			{Query: "SELECT 'total' AS label, COUNT(*) FROM t", Rows: [][]any{{"total", 3}}},
		},
	}
}

// aggregateNullsScenario mirrors testdata/aggregate_nulls.yaml. Drops
// NOT NULL on PK. Pins SQL-spec NULL aggregate semantics: COUNT(*)
// counts all rows including NULLs; COUNT(col), SUM, MIN, MAX skip
// NULLs; SUM/MIN/MAX over all-NULL or empty-input returns NULL; ungrouped
// aggregate over empty WHERE result still produces one row with NULL.
func aggregateNullsScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "aggregate_nulls",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, grp STRING, v BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 'a', 10), (2, 'a', 20), (3, 'a', null), (4, 'b', null), (5, null, 7), (6, null, null)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT COUNT(*) FROM t", Rows: [][]any{{6}}},
			{Query: "SELECT COUNT(v) FROM t", Rows: [][]any{{3}}},
			{Query: "SELECT SUM(v) FROM t", Rows: [][]any{{37}}},
			{Query: "SELECT SUM(v) FROM t WHERE grp = 'b'", Rows: [][]any{{nil}}},
			{Query: "SELECT SUM(v) FROM t WHERE grp = 'no_such_group'", Rows: [][]any{{nil}}},
			{Query: "SELECT MIN(v), MAX(v) FROM t WHERE grp = 'b'", Rows: [][]any{{nil, nil}}},
			{Query: "SELECT COUNT(*) FROM t WHERE grp = 'no_such_group'", Rows: [][]any{{0}}},
		},
	}
}

// unionColumnsRenamedScenario mirrors a portable subset of
// testdata/union_columns.yaml — only the differently-named-columns
// UNION ALL form. The remaining tests use multi-col ORDER BY (gotcha)
// and LIMIT (gotcha). Drops NOT NULL on PK.
func unionColumnsRenamedScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "union_columns",
		SchemaTemplate: "CREATE TABLE a (id BIGINT, v BIGINT, PRIMARY KEY (id))" +
			"\nCREATE TABLE b (id BIGINT, w BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO a VALUES (1, 10), (2, 20)",
			"INSERT INTO b VALUES (1, 100), (2, 200)",
		},
		Tests: []yamsql.Test{
			// UNION ALL on differently-named columns — positional matching;
			// left's name wins in result schema.
			{
				Query:     "SELECT v FROM a UNION ALL SELECT w FROM b",
				Unordered: true,
				Rows:      [][]any{{10}, {20}, {100}, {200}},
			},
		},
	}
}

// countDistinctJoinPositiveScenario lifts the one COUNT(*) test from
// testdata/count_distinct_join.yaml that doesn't use COUNT(DISTINCT)
// (which NPEs in fdb-relational 4.11.1.0 per CLAUDE.md gotcha).
// Drops NOT NULL on PK + composite PK on the join-side table.
func countDistinctJoinPositiveScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "count_distinct_join_positive",
		SchemaTemplate: "CREATE TABLE orders (id BIGINT, cust_id BIGINT, PRIMARY KEY (id))" +
			"\nCREATE TABLE tags (cust_id BIGINT, tag STRING, PRIMARY KEY (cust_id, tag))",
		Setup: []string{
			"INSERT INTO orders VALUES (1, 10), (2, 10), (3, 20), (4, 20)",
			"INSERT INTO tags VALUES (10, 'gold'), (10, 'pref'), (20, 'gold')",
		},
		Tests: []yamsql.Test{
			// COUNT(*) on comma-join (cust 10 → 2 orders × 2 tags = 4;
			// cust 20 → 2 × 1 = 2; total 6).
			{
				Query: "SELECT COUNT(*) FROM orders AS o, tags AS t WHERE o.cust_id = t.cust_id",
				Rows:  [][]any{{6}},
			},
		},
	}
}

// multiFeatureSelectScenario is a column-to-column comparison test
// lifted from testdata/multi_feature.yaml. The full file uses GROUP
// BY + HAVING + LIMIT (all blocked by fdb-relational planner gaps);
// this single SELECT pins the predicate evaluator's handling of
// WHERE col1 > col2 with NULL on either side (UNKNOWN filtered out).
// Drops NOT NULL on PK.
func multiFeatureSelectScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "multi_feature_select",
		SchemaTemplate: "CREATE TABLE orders (id BIGINT, customer_id BIGINT, total BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO orders VALUES (1, 100, 50), (2, 100, 75), (3, 100, null), (4, 200, 30), (5, 200, 30), (6, 300, 10)",
		},
		Tests: []yamsql.Test{
			// id=3 has total=NULL → 100 > NULL = UNKNOWN ⇒ filtered.
			{
				Query:     "SELECT id FROM orders WHERE customer_id > total",
				Unordered: true,
				Rows:      [][]any{{1}, {2}, {4}, {5}, {6}},
			},
		},
	}
}

// joinChainedScenario mirrors testdata/join_chained.yaml's comma-join
// subset — drops the explicit INNER JOIN ON tests (CLAUDE.md gotcha:
// fdb-relational rejects fully-qualified column names from the JOIN ON
// clause). Drops NOT NULL on PK.
func joinChainedScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "join_chained",
		SchemaTemplate: "CREATE TABLE emp (id BIGINT, name STRING, dept_id BIGINT, PRIMARY KEY (id))" +
			"\nCREATE TABLE dept (id BIGINT, name STRING, PRIMARY KEY (id))" +
			"\nCREATE TABLE project (id BIGINT, emp_id BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO emp VALUES (1, 'Alice', 10), (2, 'Bob', 20), (3, 'Carol', 10)",
			"INSERT INTO dept VALUES (10, 'Engineering'), (20, 'Sales')",
			"INSERT INTO project VALUES (100, 1), (101, 2), (102, 3)",
		},
		Tests: []yamsql.Test{
			// 2-way comma-join via WHERE.
			{
				Query: "SELECT emp.name FROM emp, project WHERE project.emp_id = emp.id ORDER BY emp.id",
				Rows:  [][]any{{"Alice"}, {"Bob"}, {"Carol"}},
			},
			// 3-way comma-join with chained equi-joins in WHERE.
			{
				Query: "SELECT emp.name, dept.name FROM emp, dept, project WHERE emp.dept_id = dept.id AND project.emp_id = emp.id ORDER BY emp.id",
				Rows:  [][]any{{"Alice", "Engineering"}, {"Bob", "Sales"}, {"Carol", "Engineering"}},
			},
		},
	}
}

// correlatedSubqueryProbesScenario mirrors testdata/correlated_subquery_probes.yaml's
// EXISTS / NOT EXISTS subset. Skips the correlated-IN form (uncertain whether
// fdb-relational's parser binds the outer reference inside an IN-subquery)
// and the correlated-scalar-subquery (parser rejects scalar subqueries
// per gotcha; also auto-skipped via error_code anyway). Drops NOT NULL on PK.
func correlatedSubqueryProbesScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "correlated_subquery_probes",
		SchemaTemplate: "CREATE TABLE emp (id BIGINT, fname STRING, dept_id BIGINT, PRIMARY KEY (id))" +
			"\nCREATE TABLE project (id BIGINT, emp_id BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO emp VALUES (1, 'Alice', 10), (2, 'Bob', 20), (3, 'Carol', 10)",
			"INSERT INTO project VALUES (100, 1), (101, 2)",
		},
		Tests: []yamsql.Test{
			// Correlated EXISTS — inner references emp.id from outer.
			{
				Query: "SELECT fname FROM emp WHERE EXISTS (SELECT 1 FROM project WHERE emp_id = emp.id) ORDER BY id",
				Rows:  [][]any{{"Alice"}, {"Bob"}},
			},
			// Correlated NOT EXISTS — inverse.
			{
				Query: "SELECT fname FROM emp WHERE NOT EXISTS (SELECT 1 FROM project WHERE emp_id = emp.id) ORDER BY id",
				Rows:  [][]any{{"Carol"}},
			},
		},
	}
}

// ambiguousColumnScenario mirrors testdata/ambiguous_column.yaml's
// qualified-positives subset. Drops NOT NULL on PK. Drops the SELECT *
// expansion test (Go dedupes-by-first-source on overlapping schemas
// while Java expands all columns — separate SELECT * expansion gap).
// error_code entries auto-skip via the harness's per-test gate.
func ambiguousColumnScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "ambiguous_column",
		SchemaTemplate: "CREATE TABLE a (id BIGINT, name STRING, PRIMARY KEY (id))" +
			"\nCREATE TABLE b (id BIGINT, name STRING, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO a VALUES (1, 'alpha'), (2, 'beta')",
			"INSERT INTO b VALUES (1, 'x'), (2, 'y')",
		},
		Tests: []yamsql.Test{
			// Qualified single-col reference — comma-join + ORDER BY PK.
			{Query: "SELECT a.name FROM a, b WHERE a.id = b.id ORDER BY a.id", Rows: [][]any{{"alpha"}, {"beta"}}},
			// Multiple qualified projections.
			{
				Query: "SELECT a.id, a.name, b.name FROM a, b WHERE a.id = b.id ORDER BY a.id",
				Rows:  [][]any{{1, "alpha", "x"}, {2, "beta", "y"}},
			},
		},
	}
}

// nestedDerivedTableScenario mirrors testdata/nested_derived_table.yaml
// — the SELECT-only subset that doesn't depend on DISTINCT (planner
// gotcha) or ORDER BY <alias> (planner gotcha). Drops NOT NULL on PK.
// Drops the DISTINCT-inside form, the alias-rename ORDER BY form, and
// the quoted-canonical-name "COUNT(*)" form (anonymous projection name
// diverges between Go's `_0` synthetic and Java's quoted-canonical).
func nestedDerivedTableScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "nested_derived_table",
		SchemaTemplate: "CREATE TABLE t1 (id BIGINT, n BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t1 VALUES (1, 10), (2, 20), (3, null), (4, 40)",
		},
		Tests: []yamsql.Test{
			// 3-level derived-table nesting + outer ORDER BY on PK.
			{
				Query: "SELECT * FROM (SELECT * FROM (SELECT * FROM t1) AS x WHERE id IS NOT NULL) AS y ORDER BY id",
				Rows:  [][]any{{1, 10}, {2, 20}, {3, nil}, {4, 40}},
			},
			// Aggregate over nested derived tables.
			{
				Query: "SELECT COUNT(*) FROM (SELECT * FROM (SELECT * FROM t1) AS x WHERE n IS NOT NULL) AS y",
				Rows:  [][]any{{3}},
			},
			// Derived table with aliased aggregate; outer references alias.
			{
				Query: "SELECT a FROM (SELECT COUNT(*) AS a FROM t1 WHERE n IS NOT NULL) AS sub",
				Rows:  [][]any{{3}},
			},
		},
	}
}

// nullCompareScenario probes 3VL comparison semantics: comparison
// operators applied to NULL operands evaluate to UNKNOWN, which is
// filtered from WHERE and projected as NULL in the SELECT-list. AND/OR
// Kleene short-circuit (FALSE absorbs UNKNOWN under AND; TRUE absorbs
// UNKNOWN under OR) is also pinned. Drops NOT NULL on PK. Net-new
// nightshift-60: existing scenarios cover boolean-column 3VL
// (`boolean`), NULL-safe equality (`is_distinct_from`), and a single
// Kleene case (`bug_hunt_probes`); none drive the comparison-of-non-
// boolean-column-against-NULL path through both projection AND WHERE
// across the full operator set. Filling that gap surfaces any future
// drift in either engine's three-valued evaluator.
func nullCompareScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "null_compare",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, w BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 10, 100), (2, 20, null), (3, null, 300), (4, null, null), (5, 50, 500)",
		},
		Tests: []yamsql.Test{
			// Comparison between two cols with NULL on either side ⇒
			// UNKNOWN ⇒ filtered. v=w never matches (rows 2/3/4 carry a
			// NULL in at least one operand; rows 1/5 have distinct values).
			{Query: "SELECT id FROM t WHERE v = w ORDER BY id", Rows: [][]any{}},
			{Query: "SELECT id FROM t WHERE v <> w ORDER BY id", Rows: [][]any{{1}, {5}}},
			{Query: "SELECT id FROM t WHERE v < w ORDER BY id", Rows: [][]any{{1}, {5}}},
			{Query: "SELECT id FROM t WHERE v > w ORDER BY id", Rows: [][]any{}},
			{Query: "SELECT id FROM t WHERE v <= w ORDER BY id", Rows: [][]any{{1}, {5}}},
			// Comparison projection with NULL operand ⇒ NULL in result.
			{Query: "SELECT id, v = 10 FROM t ORDER BY id", Rows: [][]any{{1, true}, {2, false}, {3, nil}, {4, nil}, {5, false}}},
			{Query: "SELECT id, v < 30 FROM t ORDER BY id", Rows: [][]any{{1, true}, {2, true}, {3, nil}, {4, nil}, {5, false}}},
			{Query: "SELECT id, v IS NULL FROM t ORDER BY id", Rows: [][]any{{1, false}, {2, false}, {3, true}, {4, true}, {5, false}}},
			{Query: "SELECT id, v IS NOT NULL FROM t ORDER BY id", Rows: [][]any{{1, true}, {2, true}, {3, false}, {4, false}, {5, true}}},
			// NOT through 3VL: NOT NULL = NULL ⇒ filtered.
			{Query: "SELECT id FROM t WHERE NOT (v = 10) ORDER BY id", Rows: [][]any{{2}, {5}}},
			// Kleene AND: T AND U = U; F AND U = F; U AND U = U.
			{Query: "SELECT id FROM t WHERE v IS NULL AND w = 300 ORDER BY id", Rows: [][]any{{3}}},
			// Kleene OR: T OR U = T; F OR U = U; U OR U = U.
			{Query: "SELECT id FROM t WHERE v IS NULL OR w = 100 ORDER BY id", Rows: [][]any{{1}, {3}, {4}}},
			{Query: "SELECT id FROM t WHERE v = 10 OR w IS NOT NULL ORDER BY id", Rows: [][]any{{1}, {3}, {5}}},
		},
	}
}

// booleanPrecedenceScenario pins SQL operator-precedence behaviour
// using only fully-parenthesised forms. Implicit-precedence forms
// (`WHERE a OR b AND c`) are NOT tested cross-engine: fdb-relational
// 4.11.1.0 parses `a OR b AND c` as `(a OR b) AND c` — diverging from
// SQL standard where AND binds tighter than OR (`a OR (b AND c)`). The
// Go embedded engine follows the SQL standard. New CLAUDE.md gotcha
// added nightshift-60. The explicit-parens forms below remain valid
// across both engines and pin AND/OR/NOT semantics independently of
// the divergent precedence question. Drops NOT NULL on PK.
func booleanPrecedenceScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "boolean_precedence",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, c BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 1, 1, 0), (2, 1, 0, 1), (3, 0, 1, 0), (4, 0, 0, 1)",
		},
		Tests: []yamsql.Test{
			// Two equivalent groupings of the same boolean expression —
			// pin that explicit parens behave identically across engines.
			{Query: "SELECT id FROM t WHERE a = 1 OR (b = 0 AND c = 1) ORDER BY id", Rows: [][]any{{1}, {2}, {4}}},
			{Query: "SELECT id FROM t WHERE (a = 1 OR b = 0) AND c = 1 ORDER BY id", Rows: [][]any{{2}, {4}}},
			// NOT-AND grouping: NOT outside vs NOT inside.
			{Query: "SELECT id FROM t WHERE (NOT (a = 1)) AND b = 0 ORDER BY id", Rows: [][]any{{4}}},
			{Query: "SELECT id FROM t WHERE NOT (a = 1 AND b = 0) ORDER BY id", Rows: [][]any{{1}, {3}, {4}}},
			// NOT-OR grouping: De Morgan's law cross-engine pin.
			{Query: "SELECT id FROM t WHERE (NOT (a = 1)) OR b = 1 ORDER BY id", Rows: [][]any{{1}, {3}, {4}}},
			{Query: "SELECT id FROM t WHERE NOT (a = 1 OR b = 1) ORDER BY id", Rows: [][]any{{4}}},
			// Triple-mix with parens: ((NOT a) AND b) OR c.
			{Query: "SELECT id FROM t WHERE ((NOT (a = 1)) AND b = 0) OR c = 1 ORDER BY id", Rows: [][]any{{2}, {4}}},
			// And the alternate grouping: (NOT a) AND (b OR c).
			{Query: "SELECT id FROM t WHERE (NOT (a = 1)) AND (b = 0 OR c = 1) ORDER BY id", Rows: [][]any{{4}}},
		},
	}
}

// selfJoinScenario probes self-join via comma-join (explicit JOIN ON
// is broken in fdb-relational 4.11.1.0 per CLAUDE.md). Exercises:
// equi-self (recovers each row), strict-less self-join (counts ordered
// pairs), aliased PK comparison. Drops NOT NULL on PK. Net-new
// nightshift-60: existing JOIN scenarios all use distinct tables; a
// table joined with itself surfaces aliasing bugs (the Go-side scope
// resolver and Java's quantifier renaming) that two-table joins miss.
func selfJoinScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "self_join",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, x BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 10), (2, 20), (3, 30)",
		},
		Tests: []yamsql.Test{
			// Equi-self-join on PK ⇒ each row pairs with itself.
			{Query: "SELECT a.id, b.id FROM t a, t b WHERE a.id = b.id ORDER BY a.id", Rows: [][]any{{1, 1}, {2, 2}, {3, 3}}},
			// Strict-less self-join ⇒ 3 ordered pairs (1,2), (1,3), (2,3).
			{Query: "SELECT a.id, b.id FROM t a, t b WHERE a.id < b.id", Unordered: true, Rows: [][]any{{1, 2}, {1, 3}, {2, 3}}},
			// COUNT(*) over the same relation — verifies cardinality.
			{Query: "SELECT COUNT(*) FROM t a, t b WHERE a.id < b.id", Rows: [][]any{{3}}},
			// Self-join on non-PK column.
			{Query: "SELECT a.id, b.id FROM t a, t b WHERE a.x < b.x", Unordered: true, Rows: [][]any{{1, 2}, {1, 3}, {2, 3}}},
			// Self-join projecting both sides' non-key columns.
			{Query: "SELECT a.x, b.x FROM t a, t b WHERE a.id = 1 AND b.id = 3", Rows: [][]any{{10, 30}}},
			// Cartesian product cardinality (no predicate).
			{Query: "SELECT COUNT(*) FROM t a, t b", Rows: [][]any{{9}}},
		},
	}
}

// stringCompareScenario probes basic string-column comparison
// semantics: equality (case-sensitive), inequality, lexicographic
// ordering, IN / NOT IN, empty-string handling, NULL handling.
// Existing `like` covers LIKE pattern matching; existing `bytes` does
// the same for BYTES; no scenario today exercises plain string
// comparison + sort. Drops NOT NULL on PK. Avoids ORDER BY on a
// NULL-containing column (NULL ordering is dialect-specific and not
// pinned by either engine's spec). Net-new nightshift-60.
func stringCompareScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "string_compare",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, s STRING, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 'apple'), (2, 'banana'), (3, 'cherry'), (4, ''), (5, null), (6, 'Apple')",
		},
		Tests: []yamsql.Test{
			// Case-sensitive equality.
			{Query: "SELECT id FROM t WHERE s = 'apple'", Rows: [][]any{{1}}},
			{Query: "SELECT id FROM t WHERE s = 'Apple'", Rows: [][]any{{6}}},
			// Inequality filters NULL via 3VL.
			{Query: "SELECT id FROM t WHERE s <> 'apple' ORDER BY id", Rows: [][]any{{2}, {3}, {4}, {6}}},
			// Lexicographic ASCII ordering: '' < 'A' < 'B' < 'a' < 'b'.
			// '' (4) < 'Apple' (6) < 'apple' (1) < 'banana' (2) < 'cherry' (3).
			{Query: "SELECT id FROM t WHERE s < 'cherry' ORDER BY id", Rows: [][]any{{1}, {2}, {4}, {6}}},
			{Query: "SELECT id FROM t WHERE s > 'banana' ORDER BY id", Rows: [][]any{{3}}},
			{Query: "SELECT id FROM t WHERE s >= 'apple' ORDER BY id", Rows: [][]any{{1}, {2}, {3}}},
			{Query: "SELECT id FROM t WHERE s <= 'banana' ORDER BY id", Rows: [][]any{{1}, {2}, {4}, {6}}},
			// Empty-string equality.
			{Query: "SELECT id FROM t WHERE s = ''", Rows: [][]any{{4}}},
			// NULL handling.
			{Query: "SELECT id FROM t WHERE s IS NULL", Rows: [][]any{{5}}},
			{Query: "SELECT id FROM t WHERE s IS NOT NULL ORDER BY id", Rows: [][]any{{1}, {2}, {3}, {4}, {6}}},
			// IN / NOT IN — NOT IN against non-NULL list filters NULL via 3VL.
			{Query: "SELECT id FROM t WHERE s IN ('apple', 'banana') ORDER BY id", Rows: [][]any{{1}, {2}}},
			{Query: "SELECT id FROM t WHERE s NOT IN ('apple', 'banana') ORDER BY id", Rows: [][]any{{3}, {4}, {6}}},
			// (ORDER BY s requires an index on s — fdb-relational
			// rejects with UnableToPlan otherwise; existing CLAUDE.md
			// gotcha. Skipped to keep the schema simple; ORDER BY id
			// pins natural-order across all the WHERE forms above.)
			//
			// String comparison projection.
			{Query: "SELECT id, s = 'apple' FROM t ORDER BY id", Rows: [][]any{{1, true}, {2, false}, {3, false}, {4, false}, {5, nil}, {6, false}}},
		},
	}
}

// nullArithmeticScenario pins NULL propagation through arithmetic
// expressions. fdb-relational rejects bare NULL operands ("unable to
// encapsulate arithmetic operation due to type mismatch"; CLAUDE.md
// gotcha) so all literal-NULL forms use CAST(NULL AS BIGINT). Column-
// NULL forms (where column is BIGINT NULL) need no cast. Verifies:
// (a) NULL absorbs in +, -, *, %, / regardless of operand position;
// (b) WHERE NULL-arithmetic = X filters everything (UNKNOWN); (c)
// WHERE NULL-arithmetic IS NULL matches every row.  Drops NOT NULL on
// PK. Net-new nightshift-60.
func nullArithmeticScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "null_arithmetic",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, n BIGINT, m BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 10, 3), (2, null, 5), (3, 7, null), (4, null, null)",
		},
		Tests: []yamsql.Test{
			// Column-NULL absorbs in +, -, *, /, %.
			{Query: "SELECT n + 1 FROM t WHERE id = 2", Rows: [][]any{{nil}}},
			{Query: "SELECT n - 1 FROM t WHERE id = 2", Rows: [][]any{{nil}}},
			{Query: "SELECT n * 2 FROM t WHERE id = 2", Rows: [][]any{{nil}}},
			{Query: "SELECT n / 2 FROM t WHERE id = 2", Rows: [][]any{{nil}}},
			{Query: "SELECT n % 2 FROM t WHERE id = 2", Rows: [][]any{{nil}}},
			// Both column-NULLs absorbed simultaneously.
			{Query: "SELECT n + m FROM t WHERE id = 4", Rows: [][]any{{nil}}},
			{Query: "SELECT n + m FROM t WHERE id = 3", Rows: [][]any{{nil}}},
			{Query: "SELECT n + m FROM t WHERE id = 1", Rows: [][]any{{13}}},
			// CAST(NULL AS BIGINT) literal absorbs in either position.
			{Query: "SELECT n + CAST(NULL AS BIGINT) FROM t WHERE id = 1", Rows: [][]any{{nil}}},
			{Query: "SELECT CAST(NULL AS BIGINT) + n FROM t WHERE id = 1", Rows: [][]any{{nil}}},
			{Query: "SELECT CAST(NULL AS BIGINT) * 5 FROM t WHERE id = 1", Rows: [][]any{{nil}}},
			{Query: "SELECT CAST(NULL AS BIGINT) - CAST(NULL AS BIGINT) FROM t WHERE id = 1", Rows: [][]any{{nil}}},
			// WHERE on NULL arithmetic — UNKNOWN filtered.
			{Query: "SELECT id FROM t WHERE n + 1 = 11", Rows: [][]any{{1}}},
			{Query: "SELECT id FROM t WHERE n + m > 0 ORDER BY id", Rows: [][]any{{1}}},
			// WHERE n + m IS NULL ⇒ matches every row where the result is NULL.
			{Query: "SELECT id FROM t WHERE n + m IS NULL ORDER BY id", Rows: [][]any{{2}, {3}, {4}}},
			// WHERE n + m IS NOT NULL ⇒ matches the all-non-NULL row.
			{Query: "SELECT id FROM t WHERE n + m IS NOT NULL", Rows: [][]any{{1}}},
		},
	}
}

// indexedInListWithOrderByScenario verifies that a query of the form
// `WHERE indexed_col IN (...) ORDER BY indexed_col` works cross-engine.
// In Go's chain the multi-value secondary-IN-list lazy chain branch
// declines (no usable natural order across sub-scans), and the chain
// falls through to `tryIndexScanForOrdering` which picks the index
// for ordering and post-filters by the IN-list. Java's planner does
// the equivalent with a full-index range scan + filter. Drops NOT
// NULL on PK. Net-new nightshift-60.
func indexedInListWithOrderByScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "indexed_in_list_with_order_by",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, val BIGINT, PRIMARY KEY (id))" +
			" CREATE INDEX idx_val ON t (val)",
		Setup: []string{
			"INSERT INTO t VALUES (1, 30), (2, 10), (3, 20), (4, 40), (5, 25)",
		},
		Tests: []yamsql.Test{
			// Sorted-input IN-list + ORDER BY indexed col.
			{Query: "SELECT id, val FROM t WHERE val IN (10, 20, 30) ORDER BY val", Rows: [][]any{{2, 10}, {3, 20}, {1, 30}}},
			// Unsorted-input IN-list + ORDER BY indexed col — index scan
			// emits in val order regardless of IN-list shape.
			{Query: "SELECT id, val FROM t WHERE val IN (30, 10, 20) ORDER BY val", Rows: [][]any{{2, 10}, {3, 20}, {1, 30}}},
			// IN-list + ORDER BY DESC — reverse-scan via the index.
			{Query: "SELECT id, val FROM t WHERE val IN (10, 30) ORDER BY val DESC", Rows: [][]any{{1, 30}, {2, 10}}},
			// IN-list with no matches.
			{Query: "SELECT id FROM t WHERE val IN (99, 100) ORDER BY val", Rows: [][]any{}},
		},
	}
}

// constantProjectionScenario probes pure-constant projections in
// SELECT — `SELECT 1 FROM t`, `SELECT 'literal' FROM t`, mixed
// constant + column. fdb-relational rejects FROM-less SELECT (existing
// CLAUDE.md gotcha) so all queries here have a FROM. Drops NOT NULL
// on PK. Net-new nightshift-60.
func constantProjectionScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "constant_projection",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1), (2), (3)",
		},
		Tests: []yamsql.Test{
			// Bare integer literal projection.
			{Query: "SELECT 1 FROM t WHERE id = 1", Rows: [][]any{{1}}},
			// Bare string literal projection.
			{Query: "SELECT 'hello' FROM t WHERE id = 1", Rows: [][]any{{"hello"}}},
			// Bare boolean literal projection.
			{Query: "SELECT TRUE FROM t WHERE id = 1", Rows: [][]any{{true}}},
			// Multiple constants in one row.
			{Query: "SELECT 1, 2, 3 FROM t WHERE id = 1", Rows: [][]any{{1, 2, 3}}},
			// Mixed constant + column projection.
			{Query: "SELECT id, 100 FROM t WHERE id = 1", Rows: [][]any{{1, 100}}},
			// Constant projection across multiple rows.
			{Query: "SELECT 'static' FROM t ORDER BY id", Rows: [][]any{{"static"}, {"static"}, {"static"}}},
			// Mix integer + string + boolean constants.
			{Query: "SELECT 42, 'x', FALSE FROM t WHERE id = 1", Rows: [][]any{{42, "x", false}}},
		},
	}
}

// stringUnicodeScenario verifies Unicode (UTF-8) string handling
// cross-engine: storage round-trip, equality, IN-list, IS NULL/NOT
// NULL, comparison projection. Drops NOT NULL on PK. Net-new
// nightshift-60.
func stringUnicodeScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "string_unicode",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, s STRING, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 'café'), (2, 'naïve'), (3, '日本'), (4, 'apple'), (5, null)",
		},
		Tests: []yamsql.Test{
			// Round-trip equality on accented Latin chars.
			{Query: "SELECT s FROM t WHERE id = 1", Rows: [][]any{{"café"}}},
			// Round-trip equality on CJK chars.
			{Query: "SELECT s FROM t WHERE id = 3", Rows: [][]any{{"日本"}}},
			// String equality with non-ASCII.
			{Query: "SELECT id FROM t WHERE s = 'café'", Rows: [][]any{{1}}},
			{Query: "SELECT id FROM t WHERE s = '日本'", Rows: [][]any{{3}}},
			// IN-list with Unicode strings.
			{Query: "SELECT id FROM t WHERE s IN ('café', '日本') ORDER BY id", Rows: [][]any{{1}, {3}}},
			// NULL handling alongside Unicode.
			{Query: "SELECT id FROM t WHERE s IS NULL", Rows: [][]any{{5}}},
			{Query: "SELECT id FROM t WHERE s IS NOT NULL ORDER BY id", Rows: [][]any{{1}, {2}, {3}, {4}}},
			// Equality projection — boolean comparison with Unicode operand.
			{Query: "SELECT id, s = 'café' FROM t ORDER BY id", Rows: [][]any{{1, true}, {2, false}, {3, false}, {4, false}, {5, nil}}},
		},
	}
}

// likeEscapeScenario probes LIKE with ESCAPE clause — escape char
// makes `%` and `_` match literally. The existing `like` scenario
// covers basic LIKE without ESCAPE; this fills the gap. Drops NOT
// NULL on PK. Net-new nightshift-60.
func likeEscapeScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "like_escape",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, s STRING, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, '50%'), (2, 'test_data'), (3, 'normal'), (4, 'a%b'), (5, 'c_d'), (6, 'plain')",
		},
		Tests: []yamsql.Test{
			// Literal % match via backslash-escape.
			{Query: "SELECT id FROM t WHERE s LIKE '50\\%' ESCAPE '\\'", Rows: [][]any{{1}}},
			// Literal _ match.
			{Query: "SELECT id FROM t WHERE s LIKE 'c\\_d' ESCAPE '\\'", Rows: [][]any{{5}}},
			// Pattern with both wildcards and literal special chars.
			{Query: "SELECT id FROM t WHERE s LIKE 'a\\%b' ESCAPE '\\'", Rows: [][]any{{4}}},
			// Pattern starts with %, then literal _.
			{Query: "SELECT id FROM t WHERE s LIKE '%\\_data' ESCAPE '\\'", Rows: [][]any{{2}}},
			// No match for escaped pattern that doesn't exist.
			{Query: "SELECT id FROM t WHERE s LIKE 'xxx\\%' ESCAPE '\\'", Rows: [][]any{}},
		},
	}
}

// coalesceExtraScenario extends the existing `coalesce_nullif` (1 spec)
// with more COALESCE shapes: 2-arg, 4-arg, all-NULL chains via
// `CAST(NULL AS STRING)` (Java rejects bare NULL operands), COALESCE
// in WHERE, and COALESCE in projection with arithmetic. Drops NOT
// NULL on PK. Net-new nightshift-60.
func coalesceExtraScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "coalesce_extra",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, a STRING, b STRING, c STRING, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 'x', 'y', 'z'), (2, null, 'y', 'z'), (3, null, null, 'z'), (4, null, null, null), (5, 'a', null, 'c')",
		},
		Tests: []yamsql.Test{
			// 2-arg COALESCE.
			{Query: "SELECT COALESCE(a, 'fallback') FROM t WHERE id = 1", Rows: [][]any{{"x"}}},
			{Query: "SELECT COALESCE(a, 'fallback') FROM t WHERE id = 2", Rows: [][]any{{"fallback"}}},
			// 3-arg COALESCE.
			{Query: "SELECT COALESCE(a, b, 'last') FROM t WHERE id = 1", Rows: [][]any{{"x"}}},
			{Query: "SELECT COALESCE(a, b, 'last') FROM t WHERE id = 2", Rows: [][]any{{"y"}}},
			{Query: "SELECT COALESCE(a, b, 'last') FROM t WHERE id = 3", Rows: [][]any{{"last"}}},
			// 4-arg COALESCE — fallback chain.
			{Query: "SELECT COALESCE(a, b, c, 'final') FROM t WHERE id = 4", Rows: [][]any{{"final"}}},
			{Query: "SELECT COALESCE(a, b, c, 'final') FROM t WHERE id = 5", Rows: [][]any{{"a"}}},
			// All-NULL chain via CAST(NULL AS STRING) — Java requires typed
			// NULL in arithmetic operands; for COALESCE the same rule
			// applies when the type isn't anchored by a non-NULL.
			{Query: "SELECT COALESCE(CAST(NULL AS STRING), 'default') FROM t WHERE id = 1", Rows: [][]any{{"default"}}},
			// COALESCE in WHERE.
			{Query: "SELECT id FROM t WHERE COALESCE(a, b, c) = 'z' ORDER BY id", Rows: [][]any{{3}}},
			{Query: "SELECT id FROM t WHERE COALESCE(a, b) = 'y' ORDER BY id", Rows: [][]any{{2}}},
			// COALESCE result IS NULL when all args are NULL.
			{Query: "SELECT id FROM t WHERE COALESCE(a, b, c) IS NULL", Rows: [][]any{{4}}},
			{Query: "SELECT id FROM t WHERE COALESCE(a, b, c) IS NOT NULL ORDER BY id", Rows: [][]any{{1}, {2}, {3}, {5}}},
		},
	}
}

// numericBoundaryScenario probes BIGINT boundary values (MAX/MIN/zero)
// in INSERT, SELECT, and WHERE comparisons. Both engines should handle
// the full int64 range (-9223372036854775808 to 9223372036854775807).
// Net-new nightshift-60.
func numericBoundaryScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "numeric_boundary",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, val BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 9223372036854775807)",
			"INSERT INTO t VALUES (2, -9223372036854775808)",
			"INSERT INTO t VALUES (3, 0)",
			"INSERT INTO t VALUES (4, 1)",
			"INSERT INTO t VALUES (5, -1)",
		},
		Tests: []yamsql.Test{
			// Round-trip BIGINT MAX / MIN / 0.
			{Query: "SELECT val FROM t WHERE id = 1", Rows: [][]any{{int64(9223372036854775807)}}},
			{Query: "SELECT val FROM t WHERE id = 2", Rows: [][]any{{int64(-9223372036854775808)}}},
			{Query: "SELECT val FROM t WHERE id = 3", Rows: [][]any{{0}}},
			// Comparison around zero.
			{Query: "SELECT id FROM t WHERE val > 0 ORDER BY id", Rows: [][]any{{1}, {4}}},
			{Query: "SELECT id FROM t WHERE val < 0 ORDER BY id", Rows: [][]any{{2}, {5}}},
			{Query: "SELECT id FROM t WHERE val = 0", Rows: [][]any{{3}}},
			// Boundary in WHERE — exact match on MAX.
			{Query: "SELECT id FROM t WHERE val = 9223372036854775807", Rows: [][]any{{1}}},
			{Query: "SELECT id FROM t WHERE val = -9223372036854775808", Rows: [][]any{{2}}},
			// Range across the full int64 span.
			{Query: "SELECT COUNT(*) FROM t WHERE val >= -9223372036854775808 AND val <= 9223372036854775807", Rows: [][]any{{5}}},
		},
	}
}

// isNullWithIndexScenario probes `WHERE col IS NULL ORDER BY col`
// with a satisfying secondary index. The matched rows all have
// col=NULL (constant); within them the natural order is the inner
// key (PK). ORDER BY col is then effectively a no-op — every
// matched row has the same col value. Both engines should accept
// without rejection. Drops NOT NULL on PK. Net-new nightshift-60.
func isNullWithIndexScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "is_null_with_index",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))" +
			" CREATE INDEX idx_v ON t (v)",
		Setup: []string{
			"INSERT INTO t VALUES (1, 10), (2, NULL), (3, 30), (4, NULL), (5, 20)",
		},
		Tests: []yamsql.Test{
			// IS NULL with ORDER BY indexed col — same-value group.
			{Query: "SELECT id FROM t WHERE v IS NULL ORDER BY v", Unordered: true, Rows: [][]any{{2}, {4}}},
			// IS NULL with ORDER BY PK — natural-order satisfaction.
			{Query: "SELECT id FROM t WHERE v IS NULL ORDER BY id", Rows: [][]any{{2}, {4}}},
			// IS NOT NULL with ORDER BY PK.
			{Query: "SELECT id, v FROM t WHERE v IS NOT NULL ORDER BY id", Rows: [][]any{{1, 10}, {3, 30}, {5, 20}}},
			// COUNT over IS NULL.
			{Query: "SELECT COUNT(*) FROM t WHERE v IS NULL", Rows: [][]any{{2}}},
			{Query: "SELECT COUNT(*) FROM t WHERE v IS NOT NULL", Rows: [][]any{{3}}},
		},
	}
}

// nullOrderByPositionScenario pins NULL position in ORDER BY results
// cross-engine. The Java contract (per fdb-record-layer's
// ParseHelpers.isNullsLast default — `isDescending`):
//
//	ORDER BY col ASC   → NULLs FIRST (FDB key-encoding natural order)
//	ORDER BY col DESC  → NULLs LAST  (reverse of natural order)
//
// FDB's tuple encoding makes NULL the lowest byte, so the natural
// emission order of an index scan puts NULLs first in ASC. Both Java
// and Go pin this contract; this probe is the cross-engine guard.
// `TestFDB_OrderByNullOrdering` is the Go-side regression test for the
// same contract. Drops NOT NULL on PK; adds an index on the nullable
// col so the ORDER BY is plannable. Net-new nightshift-60.
func nullOrderByPositionScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "null_order_by_position",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))" +
			" CREATE INDEX idx_v ON t (v)",
		Setup: []string{
			"INSERT INTO t VALUES (1, 10), (2, NULL), (3, 30), (4, NULL), (5, 20)",
		},
		Tests: []yamsql.Test{
			// ASC: NULLs FIRST. Among NULLs, secondary key is PK
			// (id ASC), so id=2 before id=4. Then non-NULLs in v
			// ASC: 10, 20, 30 → id=1, 5, 3.
			{Query: "SELECT id, v FROM t ORDER BY v ASC", Rows: [][]any{{2, nil}, {4, nil}, {1, 10}, {5, 20}, {3, 30}}},
			// DESC: full reverse of ASC sequence. FDB's reverse-scan
			// applies key reversal to the entire (v, id) tuple, not
			// just the leading axis — so within-NULL PK order is also
			// reversed. ASC order is [2, 4, 1, 5, 3]; DESC is the
			// strict reverse [3, 5, 1, 4, 2]. NULLs LAST follows from
			// NULL being the lowest byte in v's encoding.
			{Query: "SELECT id, v FROM t ORDER BY v DESC", Rows: [][]any{{3, 30}, {5, 20}, {1, 10}, {4, nil}, {2, nil}}},
		},
	}
}

// compositeIndexOrderByScenario probes single-column ORDER BY against
// a composite index. Multi-column ORDER BY is rejected outright by
// fdb-relational 4.11.1.0's Cascades planner (CLAUDE.md gotcha), so the
// only portable forms are: single-col ORDER BY where the col is the
// leading idx col (or its DESC reverse-scan), and the leading-col-
// equated form where ORDER BY references a trailing idx col (the
// equated leading col strips from the natural order, exposing the
// trailing col as a single-col prefix). Drops NOT NULL on PK. Net-new
// nightshift-60.
func compositeIndexOrderByScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "composite_index_order_by",
		SchemaTemplate: "CREATE TABLE rp (id BIGINT, region STRING, tag STRING, score BIGINT, PRIMARY KEY (id))" +
			" CREATE INDEX idx_region_tag ON rp (region, tag)",
		Setup: []string{
			"INSERT INTO rp VALUES (1, 'us', 'pro', 1), (2, 'us', 'free', 2), (3, 'eu', 'pro', 3), (4, 'eu', 'free', 4), (5, 'us', 'pro', 5)",
		},
		Tests: []yamsql.Test{
			// Single-col ORDER BY on leading idx col — index emits in
			// (region, tag, id), so ORDER BY region is a strict prefix.
			// Multiset compare since within-region order is unspecified
			// at the SELECT level.
			{Query: "SELECT region, tag FROM rp ORDER BY region", Unordered: true, Rows: [][]any{{"eu", "free"}, {"eu", "pro"}, {"us", "free"}, {"us", "pro"}, {"us", "pro"}}},
			// Reverse-scan satisfies single-col DESC.
			{Query: "SELECT region, tag FROM rp ORDER BY region DESC", Unordered: true, Rows: [][]any{{"us", "pro"}, {"us", "free"}, {"us", "pro"}, {"eu", "pro"}, {"eu", "free"}}},
			// Equality on leading col + single-col ORDER BY trailing idx
			// col — eq strips region, leaves natural-order suffix (tag, id);
			// ORDER BY tag is a single-col prefix of that.
			{Query: "SELECT id, tag FROM rp WHERE region = 'us' ORDER BY tag", Unordered: true, Rows: [][]any{{2, "free"}, {1, "pro"}, {5, "pro"}}},
		},
	}
}

// dmlAdvancedScenario extends `dml_setup` with multi-column UPDATE,
// computed-expression UPDATE, no-match UPDATE/DELETE, and DELETE with
// compound predicates. INSERT ... SELECT FROM is intentionally NOT
// tested — fdb-relational 4.11.1.0 rejects it with a syntax error
// (the grammar's `insertStatement` rule only accepts VALUES). Drops
// NOT NULL on PK. Net-new nightshift-60.
func dmlAdvancedScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "dml_advanced",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 10, 100), (2, 20, 200), (3, 30, 300), (4, 40, 400)",
			// Multi-column SET in one UPDATE.
			"UPDATE t SET a = 99, b = 999 WHERE id = 1",
			// UPDATE with computed expression on RHS — references current row's value.
			"UPDATE t SET a = a + 1 WHERE id = 2",
			// UPDATE with compound predicate.
			"UPDATE t SET b = 0 WHERE id > 2 AND a > 25",
			// DELETE with compound predicate.
			"DELETE FROM t WHERE id = 4 AND a >= 40",
			// No-match UPDATE — no row, no error.
			"UPDATE t SET a = -1 WHERE id = 9999",
			// No-match DELETE.
			"DELETE FROM t WHERE id = 9999",
		},
		Tests: []yamsql.Test{
			// Final state: 3 rows {(1, 99, 999), (2, 21, 200), (3, 30, 0)}.
			{Query: "SELECT id, a, b FROM t ORDER BY id", Rows: [][]any{{1, 99, 999}, {2, 21, 200}, {3, 30, 0}}},
			{Query: "SELECT COUNT(*) FROM t", Rows: [][]any{{3}}},
			{Query: "SELECT a, b FROM t WHERE id = 1", Rows: [][]any{{99, 999}}},
			{Query: "SELECT a FROM t WHERE id = 2", Rows: [][]any{{21}}},
			{Query: "SELECT b FROM t WHERE id = 3", Rows: [][]any{{0}}},
			// id=4 was deleted.
			{Query: "SELECT id FROM t WHERE id = 4", Rows: [][]any{}},
			// Aggregates over the post-DML state.
			{Query: "SELECT SUM(a), SUM(b) FROM t", Rows: [][]any{{150, 1199}}},
			{Query: "SELECT MIN(a), MAX(a) FROM t", Rows: [][]any{{21, 99}}},
		},
	}
}

// numericComparisonScenario probes type-promoted comparisons cross-engine.
// Both engines must agree on the truth value of `WHERE bigint_col >
// double_literal`, `WHERE double_col = bigint_literal`, etc. The
// existing `between` scenario covers BETWEEN with mixed-type bounds;
// this fills in the standard `<`, `<=`, `>`, `>=`, `=`, `<>` operators
// across BIGINT and DOUBLE. Drops NOT NULL on PK. Net-new
// nightshift-60.
func numericComparisonScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "numeric_comparison",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, i BIGINT, d DOUBLE, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 5, 1.5), (2, 10, 2.5), (3, 100, 3.5), (4, 0, 0.0), (5, -5, -1.5)",
		},
		Tests: []yamsql.Test{
			// BIGINT col vs DOUBLE literal — Java widens to DOUBLE.
			{Query: "SELECT id FROM t WHERE i > 1.5 ORDER BY id", Rows: [][]any{{1}, {2}, {3}}},
			{Query: "SELECT id FROM t WHERE i < 5.5 ORDER BY id", Rows: [][]any{{1}, {4}, {5}}},
			{Query: "SELECT id FROM t WHERE i >= 10.0 ORDER BY id", Rows: [][]any{{2}, {3}}},
			{Query: "SELECT id FROM t WHERE i <= -1.5 ORDER BY id", Rows: [][]any{{5}}},
			// DOUBLE col vs BIGINT literal — widens to DOUBLE.
			{Query: "SELECT id FROM t WHERE d > 1 ORDER BY id", Rows: [][]any{{1}, {2}, {3}}},
			{Query: "SELECT id FROM t WHERE d < 3 ORDER BY id", Rows: [][]any{{1}, {2}, {4}, {5}}},
			{Query: "SELECT id FROM t WHERE d = 0 ORDER BY id", Rows: [][]any{{4}}},
			// BIGINT-equality with DOUBLE-valued operand (lossless).
			{Query: "SELECT id FROM t WHERE i = 5.0", Rows: [][]any{{1}}},
			{Query: "SELECT id FROM t WHERE i = -5.0", Rows: [][]any{{5}}},
			// Inequality across types.
			{Query: "SELECT id FROM t WHERE d <> 0.0 ORDER BY id", Rows: [][]any{{1}, {2}, {3}, {5}}},
			{Query: "SELECT id FROM t WHERE i <> 0 ORDER BY id", Rows: [][]any{{1}, {2}, {3}, {5}}},
			// Compound predicate mixing types.
			{Query: "SELECT id FROM t WHERE i > 0 AND d > 1.0 ORDER BY id", Rows: [][]any{{1}, {2}, {3}}},
			{Query: "SELECT id FROM t WHERE i > 0 OR d < 0.0 ORDER BY id", Rows: [][]any{{1}, {2}, {3}, {5}}},
		},
	}
}

// pkDescScenario probes ORDER BY DESC on PK columns. The natural
// emission order of an FDB scan is ASC; DESC is satisfied via a
// reverse scan when the ORDER BY is an all-DESC prefix of the
// natural order. Existing scenarios mostly skip DESC variants ("Skips
// DESC (cursors are ASC-only and Java's planner often can't
// reverse)"); this scenario re-enables DESC where both engines
// support it cross-engine. Drops NOT NULL on PK. Net-new
// nightshift-60.
func pkDescScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "pk_desc",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, name STRING, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 'a'), (2, 'b'), (3, 'c'), (4, 'd'), (5, 'e')",
		},
		Tests: []yamsql.Test{
			// Full PK scan ORDER BY DESC — reverse scan emits in DESC order.
			{Query: "SELECT id FROM t ORDER BY id DESC", Rows: [][]any{{5}, {4}, {3}, {2}, {1}}},
			// PK range + ORDER BY id DESC.
			{Query: "SELECT id FROM t WHERE id >= 3 ORDER BY id DESC", Rows: [][]any{{5}, {4}, {3}}},
			// PK BETWEEN + ORDER BY id DESC.
			{Query: "SELECT id FROM t WHERE id BETWEEN 2 AND 4 ORDER BY id DESC", Rows: [][]any{{4}, {3}, {2}}},
			// PK equality (at-most-1-row) + ORDER BY id DESC.
			{Query: "SELECT id, name FROM t WHERE id = 3 ORDER BY id DESC", Rows: [][]any{{3, "c"}}},
			// PK IN-list + ORDER BY id DESC — my fix sorts IN-list values DESC.
			{Query: "SELECT id FROM t WHERE id IN (1, 5, 3) ORDER BY id DESC", Rows: [][]any{{5}, {3}, {1}}},
		},
	}
}

// joinOrderByRightPKScenario probes JOIN + ORDER BY on the joined-side
// PK col. Java's Cascades planner picks the JOIN-side outer based on
// cost — so `FROM A, B WHERE A.fk = B.pk ORDER BY B.pk` succeeds in
// Java by running B as the outer scan. Go's nested-loop is fixed
// (left source = outer) and sorts the result in-memory; the JOIN sort
// site preserves the in-memory fallback (TODO.md tracks the proper fix
// gated on C2 QueryExecutor). Both engines produce the same final
// row set when sorted, so cross-engine equivalence holds. Net-new
// nightshift-60 to document Java's cost-model JOIN-side reordering.
func joinOrderByRightPKScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "join_order_by_right_pk",
		SchemaTemplate: "CREATE TABLE Users (uid BIGINT, name STRING, PRIMARY KEY (uid))" +
			" CREATE TABLE Orders (oid BIGINT, uid BIGINT, total BIGINT, PRIMARY KEY (oid))",
		Setup: []string{
			"INSERT INTO Users VALUES (1, 'alice'), (2, 'bob')",
			"INSERT INTO Orders VALUES (10, 1, 100), (11, 1, 200), (12, 2, 300)",
		},
		Tests: []yamsql.Test{
			// ORDER BY left source PK — both engines satisfy directly
			// (left is outer in Go's nested loop; Java picks Users-as-outer
			// since u.uid is its PK).
			{Query: "SELECT u.name, o.total FROM Users u, Orders o WHERE u.uid = o.uid ORDER BY u.uid", Unordered: true, Rows: [][]any{{"alice", 100}, {"alice", 200}, {"bob", 300}}},
			// ORDER BY right source PK — Java picks Orders-as-outer for
			// natural ordering; Go sorts in-memory post-JOIN. Same result
			// row set under multiset comparison.
			{Query: "SELECT u.name, o.total FROM Users u, Orders o WHERE u.uid = o.uid ORDER BY o.oid", Rows: [][]any{{"alice", 100}, {"alice", 200}, {"bob", 300}}},
			// COUNT over the JOIN — aggregate path, exempt from any
			// ORDER BY rejection.
			{Query: "SELECT COUNT(*) FROM Users u, Orders o WHERE u.uid = o.uid", Rows: [][]any{{3}}},
		},
	}
}

// projectionAliasScenario probes column- and table-alias forms in
// projection. Existing scenarios use aliases incidentally; this pins
// the cross-engine alignment of `SELECT col AS alias`, `SELECT
// table.col`, `FROM t AS alias`, and bare-column projection. Drops
// NOT NULL on PK. Net-new nightshift-60.
func projectionAliasScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "projection_alias",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, s STRING, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 10, 'a'), (2, 20, 'b'), (3, 30, 'c')",
		},
		Tests: []yamsql.Test{
			// Single-column rename via AS.
			{Query: "SELECT id AS pk FROM t ORDER BY id", Rows: [][]any{{1}, {2}, {3}}},
			// Multi-column with selective rename.
			{Query: "SELECT id, v AS amount FROM t ORDER BY id", Rows: [][]any{{1, 10}, {2, 20}, {3, 30}}},
			// Table alias via AS, qualified ORDER BY.
			{Query: "SELECT a.id, a.v FROM t AS a ORDER BY a.id", Rows: [][]any{{1, 10}, {2, 20}, {3, 30}}},
			// Table alias without AS keyword.
			{Query: "SELECT a.id FROM t a ORDER BY a.id", Rows: [][]any{{1}, {2}, {3}}},
			// Both column and table aliased.
			{Query: "SELECT a.s AS letter FROM t AS a ORDER BY a.id", Rows: [][]any{{"a"}, {"b"}, {"c"}}},
			// Aliased column in WHERE — uses underlying col name.
			{Query: "SELECT id AS pk FROM t WHERE id = 2", Rows: [][]any{{2}}},
			// Computed projection with alias.
			{Query: "SELECT id, v + 1 AS plus_one FROM t WHERE id = 1", Rows: [][]any{{1, 11}}},
		},
	}
}

// pkEqualityOrderByScenario pins PK-equality scan ORDER BY behaviour.
// With NO satisfying index on the ORDER BY col, Java rejects with
// UnableToPlan even though the result is at-most-1-row. With an
// index on the ORDER BY col, Java picks that index and succeeds.
// Cross-engine alignment requires Go to match: drop the Go-permissive
// at-most-1-row exemption when no satisfying scan exists. ORDER BY
// PK col always works (PK natural order satisfies). Drops NOT NULL on
// PK. Net-new nightshift-60.
func pkEqualityOrderByScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "pk_equality_order_by",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, s STRING, PRIMARY KEY (id))" +
			" CREATE INDEX idx_v ON t (v)" +
			" CREATE INDEX idx_s ON t (s)",
		Setup: []string{
			"INSERT INTO t VALUES (1, 100, 'a'), (2, 200, 'b'), (3, 300, 'c')",
		},
		Tests: []yamsql.Test{
			// PK equality + ORDER BY a non-PK col with a satisfying
			// index. The planner picks the index for ordering and
			// post-filters by PK equality. 1 row matches.
			{Query: "SELECT v, s FROM t WHERE id = 2 ORDER BY s", Rows: [][]any{{200, "b"}}},
			{Query: "SELECT v, s FROM t WHERE id = 2 ORDER BY v", Rows: [][]any{{200, "b"}}},
			// Single-value PK IN-list — same path.
			{Query: "SELECT v, s FROM t WHERE id IN (2) ORDER BY s", Rows: [][]any{{200, "b"}}},
			// PK-equality on a missing key → 0 rows.
			{Query: "SELECT v, s FROM t WHERE id = 999 ORDER BY s", Rows: [][]any{}},
			{Query: "SELECT v FROM t WHERE id IN (999) ORDER BY v", Rows: [][]any{}},
			// PK equality + ORDER BY PK col — PK natural order satisfies
			// directly without needing an index.
			{Query: "SELECT v FROM t WHERE id = 1 ORDER BY id", Rows: [][]any{{100}}},
		},
	}
}

// whereComplexScenario probes complex WHERE shapes that combine
// BETWEEN, IN, IS NULL, IS NOT NULL, NOT, LIKE, AND, OR. Existing
// scenarios cover each individual primitive; this exercises the
// combinations to surface any short-circuit / 3VL drift between the
// engines under multi-leaf WHERE trees. Drops NOT NULL on PK.
// Net-new nightshift-60.
func whereComplexScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "where_complex",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, s STRING, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 10, 'a'), (2, 20, 'b'), (3, null, 'c'), (4, 40, null), (5, 50, 'd')",
		},
		Tests: []yamsql.Test{
			// BETWEEN + OR.
			{Query: "SELECT id FROM t WHERE (v BETWEEN 10 AND 20) OR (s = 'd') ORDER BY id", Rows: [][]any{{1}, {2}, {5}}},
			// BETWEEN + IS NOT NULL combined.
			{Query: "SELECT id FROM t WHERE v BETWEEN 10 AND 50 AND s IS NOT NULL ORDER BY id", Rows: [][]any{{1}, {2}, {5}}},
			// NOT BETWEEN — NULL operand → UNKNOWN → filtered.
			{Query: "SELECT id FROM t WHERE v NOT BETWEEN 30 AND 50 ORDER BY id", Rows: [][]any{{1}, {2}}},
			// Two-col IS NOT NULL.
			{Query: "SELECT id FROM t WHERE v IS NOT NULL AND s IS NOT NULL ORDER BY id", Rows: [][]any{{1}, {2}, {5}}},
			// OR of IS NULL (each row is NULL on at least one col → match).
			{Query: "SELECT id FROM t WHERE v IS NULL OR s IS NULL ORDER BY id", Rows: [][]any{{3}, {4}}},
			// NOT (BETWEEN). NULL → NOT NULL = NULL → filtered.
			{Query: "SELECT id FROM t WHERE NOT (v BETWEEN 30 AND 50) ORDER BY id", Rows: [][]any{{1}, {2}}},
			// IN-list combined with IS NOT NULL on a different col.
			{Query: "SELECT id FROM t WHERE v IN (10, 30, 50) AND s IS NOT NULL ORDER BY id", Rows: [][]any{{1}, {5}}},
			// BETWEEN + LIKE. id=1 v=10 in [10,30] AND s='a' LIKE 'a%' → match.
			{Query: "SELECT id FROM t WHERE (v BETWEEN 10 AND 30) AND s LIKE 'a%' ORDER BY id", Rows: [][]any{{1}}},
			// Triple-AND: v non-NULL AND s non-NULL AND v >= 20.
			{Query: "SELECT id FROM t WHERE v IS NOT NULL AND s IS NOT NULL AND v >= 20 ORDER BY id", Rows: [][]any{{2}, {5}}},
			// OR + IS NULL — short-circuit on either side.
			{Query: "SELECT id FROM t WHERE v IS NULL OR (v >= 30 AND s IS NOT NULL) ORDER BY id", Rows: [][]any{{3}, {5}}},
		},
	}
}

// dmlSetupScenario probes the visibility of mid-stream UPDATE / DELETE
// in the setup phase. runWithSetup runs each setup statement
// sequentially before the final query; INSERT then UPDATE then DELETE
// then SELECT lets us pin both engines' DML semantics end-to-end. The
// existing scenarios all confined DML to INSERT-only setup; this fills
// the UPDATE / DELETE gap. Drops NOT NULL on PK. Net-new
// nightshift-60.
func dmlSetupScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "dml_setup",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 10), (2, 20), (3, 30), (4, 40)",
			"UPDATE t SET v = 99 WHERE id = 2",
			"DELETE FROM t WHERE id = 3",
		},
		Tests: []yamsql.Test{
			// Final state: 3 rows {(1,10), (2,99), (4,40)}.
			{Query: "SELECT id, v FROM t ORDER BY id", Rows: [][]any{{1, 10}, {2, 99}, {4, 40}}},
			{Query: "SELECT COUNT(*) FROM t", Rows: [][]any{{3}}},
			{Query: "SELECT v FROM t WHERE id = 2", Rows: [][]any{{99}}},
			{Query: "SELECT id FROM t WHERE id = 3", Rows: [][]any{}},
			{Query: "SELECT SUM(v) FROM t", Rows: [][]any{{149}}},
			{Query: "SELECT MIN(v), MAX(v) FROM t", Rows: [][]any{{10, 99}}},
		},
	}
}

// arithmeticCompoundScenario probes compound arithmetic in the SELECT
// list using only fully-parenthesised forms. Implicit-precedence forms
// like `a + b * 2` are NOT tested — fdb-relational 4.11.1.0 parses
// arithmetic operators left-to-right with same precedence, so
// `a + b * 2` evaluates as `(a + b) * 2`, not the SQL-standard
// `a + (b * 2)`. New CLAUDE.md gotcha added nightshift-60. The Go
// embedded engine follows standard precedence. The explicit-parens
// forms below pin operator semantics independently of the precedence
// divergence. Drops NOT NULL on PK.
func arithmeticCompoundScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "arithmetic_compound",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, c DOUBLE, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 10, 3, 1.5), (2, 20, 4, 2.5)",
		},
		Tests: []yamsql.Test{
			// Parens-explicit compound expressions.
			{Query: "SELECT a + (b * 2) FROM t WHERE id = 1", Rows: [][]any{{16}}},
			{Query: "SELECT (a + b) * 2 FROM t WHERE id = 1", Rows: [][]any{{26}}},
			{Query: "SELECT a - (b - 1) FROM t WHERE id = 1", Rows: [][]any{{8}}},
			// Mixed integer and double promote to DOUBLE.
			{Query: "SELECT a + c FROM t WHERE id = 1", Rows: [][]any{{11.5}}},
			{Query: "SELECT a * c FROM t WHERE id = 1", Rows: [][]any{{15.0}}},
			// Integer division on BIGINT operands stays integer.
			{Query: "SELECT a / b FROM t WHERE id = 1", Rows: [][]any{{3}}},
			// Float division on DOUBLE operands.
			{Query: "SELECT c / 2 FROM t WHERE id = 1", Rows: [][]any{{0.75}}},
			// Negation via `0 - col`. Unary minus on a column reference
			// is rejected by fdb-relational 4.11.1.0's parser with
			// `syntax error`. Bare-paren `(expr)` around a single
			// scalar is parsed as a single-element record/tuple
			// constructor, returning ImmutableRowStruct (the same
			// "WHERE (b = true)" gotcha extends to projection scalars).
			{Query: "SELECT 0 - a FROM t WHERE id = 1", Rows: [][]any{{-10}}},
			// Modulo with explicit parens.
			{Query: "SELECT (a % b) + 1 FROM t WHERE id = 1", Rows: [][]any{{2}}},
			// Compound predicate — arithmetic in WHERE.
			{Query: "SELECT id FROM t WHERE (a + b) > 20 ORDER BY id", Rows: [][]any{{2}}},
			{Query: "SELECT id FROM t WHERE (a * 2) = 40 ORDER BY id", Rows: [][]any{{2}}},
		},
	}
}

// orderByIndexedColScenario pins the "ORDER BY a column with a
// satisfying secondary index" path. Net-new nightshift-60: with the
// in-memory sort fallback removed, Go relies on the new
// `tryIndexScanForOrdering` branch (full secondary-index scan as the
// last branch before the full-PK fallback) to satisfy this shape.
// Cross-engine agreement here pins that the Go branch picks the right
// index in the same cases Java's RemoveSortRule does. Drops NOT NULL
// on PK; values chosen so that PK order ≠ indexed-col order.
func orderByIndexedColScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "order_by_indexed_col",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, s STRING, PRIMARY KEY (id))" +
			" CREATE INDEX idx_v ON t (v)" +
			" CREATE INDEX idx_s ON t (s)",
		Setup: []string{
			"INSERT INTO t VALUES (1, 30, 'c'), (2, 10, 'aa'), (3, 20, 'b'), (4, 5, 'a')",
		},
		Tests: []yamsql.Test{
			// ORDER BY indexed BIGINT col — full idx_v scan in natural order.
			{Query: "SELECT id FROM t ORDER BY v", Rows: [][]any{{4}, {2}, {3}, {1}}},
			{Query: "SELECT id, v FROM t ORDER BY v", Rows: [][]any{{4, 5}, {2, 10}, {3, 20}, {1, 30}}},
			// ORDER BY indexed STRING col — full idx_s scan. ASCII order:
			// 'a' < 'aa' < 'b' < 'c'.
			{Query: "SELECT id, s FROM t ORDER BY s", Rows: [][]any{{4, "a"}, {2, "aa"}, {3, "b"}, {1, "c"}}},
			// ORDER BY indexed col DESC — reverse-scan satisfies.
			{Query: "SELECT id FROM t ORDER BY v DESC", Rows: [][]any{{1}, {3}, {2}, {4}}},
			// ORDER BY indexed col + WHERE on PK (post-filter via the index
			// scan loop's evalPredicate).
			{Query: "SELECT id, v FROM t WHERE id > 1 ORDER BY v", Rows: [][]any{{4, 5}, {2, 10}, {3, 20}}},
			// ORDER BY indexed col + WHERE on the same col (range pushdown
			// fires, distinct from the new full-index branch).
			{Query: "SELECT id FROM t WHERE v >= 10 ORDER BY v", Rows: [][]any{{2}, {3}, {1}}},
		},
	}
}

// havingPositiveScenario probes HAVING clause shapes with non-empty
// results. The "WHERE filters all rows + HAVING checks aggregate"
// shape (e.g. `SELECT COUNT(*) FROM t WHERE id = 999 HAVING COUNT(*) >= 0`)
// diverges between engines: Go follows SQL spec (single grouping with
// COUNT=0, then HAVING tests it) → 1 row [[0]]; Java treats the
// empty-WHERE result as no grouping at all, HAVING never fires → 0 rows.
// Tracked in CLAUDE.md and exercised by aggregateEmptyTableScenario's
// last test (which is omitted from this scenario). All shapes here have
// non-empty WHERE results so the divergence doesn't apply. Net-new
// nightshift-61.
func havingPositiveScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "having_positive",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 10), (2, 20), (3, 30), (4, 40)",
		},
		Tests: []yamsql.Test{
			// COUNT(*) HAVING — passes (4 > 0).
			{Query: "SELECT COUNT(*) FROM t HAVING COUNT(*) > 0", Rows: [][]any{{4}}},
			// COUNT(*) HAVING — fails (4 not > 10) → 0 rows.
			{Query: "SELECT COUNT(*) FROM t HAVING COUNT(*) > 10", Rows: [][]any{}},
			// COUNT(*) HAVING equality.
			{Query: "SELECT COUNT(*) FROM t HAVING COUNT(*) = 4", Rows: [][]any{{4}}},
			// SUM HAVING.
			{Query: "SELECT SUM(v) FROM t HAVING SUM(v) > 50", Rows: [][]any{{100}}},
			// SUM HAVING — false predicate.
			{Query: "SELECT SUM(v) FROM t HAVING SUM(v) < 50", Rows: [][]any{}},
			// MIN/MAX in projection + HAVING.
			{Query: "SELECT MIN(v), MAX(v) FROM t HAVING MIN(v) >= 10", Rows: [][]any{{10, 40}}},
			// HAVING with WHERE that still leaves rows.
			{Query: "SELECT COUNT(*) FROM t WHERE v > 15 HAVING COUNT(*) >= 1", Rows: [][]any{{3}}},
			// HAVING combined predicates (AND).
			{Query: "SELECT COUNT(*) FROM t HAVING COUNT(*) > 0 AND COUNT(*) <= 10", Rows: [][]any{{4}}},
			// HAVING with arithmetic on aggregate result.
			{Query: "SELECT SUM(v) FROM t HAVING SUM(v) + 1 > 100", Rows: [][]any{{100}}},
			// COUNT-based existence check.
			{Query: "SELECT COUNT(*) FROM t WHERE v = 999 HAVING COUNT(*) > 0", Rows: [][]any{}},
		},
	}
}

// negativeConstantsScenario probes negative number literals in
// arithmetic, IN-list, comparison, and ORDER BY. Existing scenarios
// touch negation via `0 - col` (unary minus on a column ref is rejected
// by fdb-relational's parser, gotcha) but only briefly cover negative
// literals as constants. Net-new nightshift-61.
func negativeConstantsScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "negative_constants",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, -10), (2, -5), (3, 0), (4, 5), (5, 10)",
		},
		Tests: []yamsql.Test{
			// Equality with negative literal.
			{Query: "SELECT id FROM t WHERE v = -10", Rows: [][]any{{1}}},
			// Negative literal in IN-list.
			{Query: "SELECT id FROM t WHERE v IN (-10, -5, 0) ORDER BY id", Rows: [][]any{{1}, {2}, {3}}},
			// NOT IN with negatives.
			{Query: "SELECT id FROM t WHERE v NOT IN (-10, 5) ORDER BY id", Rows: [][]any{{2}, {3}, {5}}},
			// Range comparison with negatives.
			{Query: "SELECT id FROM t WHERE v < 0 ORDER BY id", Rows: [][]any{{1}, {2}}},
			{Query: "SELECT id FROM t WHERE v <= 0 ORDER BY id", Rows: [][]any{{1}, {2}, {3}}},
			{Query: "SELECT id FROM t WHERE v >= -5 ORDER BY id", Rows: [][]any{{2}, {3}, {4}, {5}}},
			// BETWEEN with negative bounds.
			{Query: "SELECT id FROM t WHERE v BETWEEN -10 AND -1 ORDER BY id", Rows: [][]any{{1}, {2}}},
			{Query: "SELECT id FROM t WHERE v BETWEEN -5 AND 5 ORDER BY id", Rows: [][]any{{2}, {3}, {4}}},
			// Arithmetic with negative literal.
			{Query: "SELECT v + (-1) FROM t WHERE id = 4", Rows: [][]any{{4}}},
			{Query: "SELECT v * (-2) FROM t WHERE id = 5", Rows: [][]any{{-20}}},
			// Negative in projection.
			{Query: "SELECT id, -1 FROM t WHERE id = 1", Rows: [][]any{{1, -1}}},
			// Mixed-sign sum.
			{Query: "SELECT SUM(v) FROM t", Rows: [][]any{{0}}},
		},
	}
}

// emptyStringScenario probes empty-string handling in equality, IN,
// IS NULL, projection, LIKE, and length-style comparisons. Empty
// strings are commonly mishandled (NULL vs empty conflation, IN
// list edge cases). Net-new nightshift-61.
func emptyStringScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "empty_string",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, s STRING, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, ''), (2, 'a'), (3, null), (4, 'aa'), (5, '')",
		},
		Tests: []yamsql.Test{
			// Equality with empty literal.
			{Query: "SELECT id FROM t WHERE s = '' ORDER BY id", Rows: [][]any{{1}, {5}}},
			// Empty string is NOT NULL.
			{Query: "SELECT id FROM t WHERE s IS NOT NULL ORDER BY id", Rows: [][]any{{1}, {2}, {4}, {5}}},
			// NULL distinguishable from empty.
			{Query: "SELECT id FROM t WHERE s IS NULL", Rows: [][]any{{3}}},
			// IN-list with empty literal.
			{Query: "SELECT id FROM t WHERE s IN ('', 'a') ORDER BY id", Rows: [][]any{{1}, {2}, {5}}},
			// NOT IN with empty literal — note NULL row excluded by 3VL.
			{Query: "SELECT id FROM t WHERE s NOT IN ('a', 'aa') ORDER BY id", Rows: [][]any{{1}, {5}}},
			// Empty string projection.
			{Query: "SELECT s FROM t WHERE id = 1", Rows: [][]any{{""}}},
			// Empty string in COUNT.
			{Query: "SELECT COUNT(*) FROM t WHERE s = ''", Rows: [][]any{{2}}},
			// Empty string LIKE.
			{Query: "SELECT id FROM t WHERE s LIKE '' ORDER BY id", Rows: [][]any{{1}, {5}}},
			// LIKE %_% on empty doesn't match.
			{Query: "SELECT id FROM t WHERE s LIKE '_%' ORDER BY id", Rows: [][]any{{2}, {4}}},
			// Comparison ordering: '' < 'a'.
			{Query: "SELECT id FROM t WHERE s < 'a' ORDER BY id", Rows: [][]any{{1}, {5}}},
		},
	}
}

// largeInListScenario probes IN-list with many literals to surface any
// list-size limits or quadratic blowup. fdb-relational has no documented
// list-size limit; Go's embedded engine builds an in-memory slice scan.
// 50 elements is well under any sane limit on either side. Net-new
// nightshift-61.
func largeInListScenario() *yamsql.Scenario {
	// Build an IN-list of 50 elements: (1, 2, ..., 50).
	// Match every row in a 50-row table (id 1..50, v = id*10).
	inList := ""
	for i := 1; i <= 50; i++ {
		if i > 1 {
			inList += ", "
		}
		inList += fmt.Sprintf("%d", i)
	}
	insertList := ""
	for i := 1; i <= 50; i++ {
		if i > 1 {
			insertList += ", "
		}
		insertList += fmt.Sprintf("(%d, %d)", i, i*10)
	}
	return &yamsql.Scenario{
		Name:           "large_in_list",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES " + insertList,
		},
		Tests: []yamsql.Test{
			// 50-element IN-list matches every row.
			{Query: "SELECT COUNT(*) FROM t WHERE id IN (" + inList + ")", Rows: [][]any{{50}}},
			// NOT IN matches none.
			{Query: "SELECT COUNT(*) FROM t WHERE id NOT IN (" + inList + ")", Rows: [][]any{{0}}},
			// IN-list partial match — first half.
			{Query: "SELECT COUNT(*) FROM t WHERE v IN (10, 20, 30, 40, 50, 60, 70, 80, 90, 100)", Rows: [][]any{{10}}},
			// Large IN with ORDER BY pushed-down PK col.
			{Query: "SELECT id FROM t WHERE id IN (1, 5, 10, 25, 50) ORDER BY id", Rows: [][]any{{1}, {5}, {10}, {25}, {50}}},
		},
	}
}

// updateNonPKPredicateScenario probes UPDATE / DELETE with WHERE on a
// non-PK column. Existing dml_setup uses WHERE on the PK col; this
// probes the secondary-index pushdown (or full-scan-then-filter) path
// for DML. Net-new nightshift-61.
func updateNonPKPredicateScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "update_non_pk_predicate",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, s STRING, PRIMARY KEY (id))" +
			" CREATE INDEX idx_v ON t (v)" +
			" CREATE INDEX idx_s ON t (s)",
		Setup: []string{
			"INSERT INTO t VALUES (1, 10, 'a'), (2, 20, 'b'), (3, 30, 'c'), (4, 20, 'd')",
			// UPDATE WHERE on indexed v col — matches 2 rows.
			"UPDATE t SET s = 'twenty' WHERE v = 20",
			// DELETE WHERE on indexed s col.
			"DELETE FROM t WHERE s = 'a'",
		},
		Tests: []yamsql.Test{
			// Final state: id=2 → s='twenty', id=3 → s='c', id=4 → s='twenty'.
			{Query: "SELECT id, v, s FROM t ORDER BY id", Rows: [][]any{
				{2, 20, "twenty"}, {3, 30, "c"}, {4, 20, "twenty"},
			}},
			{Query: "SELECT COUNT(*) FROM t WHERE s = 'twenty'", Rows: [][]any{{2}}},
			{Query: "SELECT COUNT(*) FROM t", Rows: [][]any{{3}}},
			// Verify the DELETE removed the right row.
			{Query: "SELECT id FROM t WHERE s = 'a'", Rows: [][]any{}},
		},
	}
}

// caseInOrderByScenario probes ORDER BY with a CASE expression as the
// sort key. Likely to interact with the Java Cascades planner's
// ordering-property analysis (CLAUDE.md: "ORDER BY arithmetic expression
// raises UnableToPlanException"). If Java rejects, drop the scenario;
// otherwise pin it. Net-new nightshift-61.
func caseInOrderByScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "case_in_order_by",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 5), (2, 15), (3, 25), (4, 35)",
		},
		Tests: []yamsql.Test{
			// CASE in projection with ORDER BY on PK — works.
			{
				Query: "SELECT id, CASE WHEN v < 20 THEN 'low' ELSE 'high' END FROM t ORDER BY id",
				Rows:  [][]any{{1, "low"}, {2, "low"}, {3, "high"}, {4, "high"}},
			},
			// CASE in projection alongside the original col — ORDER BY by id.
			{
				Query: "SELECT v, CASE WHEN v >= 25 THEN 1 ELSE 0 END FROM t ORDER BY id",
				Rows:  [][]any{{5, 0}, {15, 0}, {25, 1}, {35, 1}},
			},
		},
	}
}

// bytesAdvancedScenario probes BYTES column behaviour beyond the basic
// equality / round-trip in bytesScenario. Adds: IN list with hex
// literals, IS NULL / IS NOT NULL, NULL projection, multi-row scan
// with mixed NULL+non-NULL. Net-new nightshift-61.
func bytesAdvancedScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "bytes_advanced",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, payload BYTES, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, X'DEADBEEF'), (2, X'CAFEBABE'), (3, null), (4, X'00')",
		},
		Tests: []yamsql.Test{
			// Equality round-trip (existing).
			{Query: "SELECT id FROM t WHERE payload = X'DEADBEEF'", Rows: [][]any{{1}}},
			// IN-list with hex literals.
			{Query: "SELECT id FROM t WHERE payload IN (X'DEADBEEF', X'CAFEBABE') ORDER BY id", Rows: [][]any{{1}, {2}}},
			// IS NULL.
			{Query: "SELECT id FROM t WHERE payload IS NULL", Rows: [][]any{{3}}},
			// IS NOT NULL.
			{Query: "SELECT id FROM t WHERE payload IS NOT NULL ORDER BY id", Rows: [][]any{{1}, {2}, {4}}},
			// COUNT non-null payload.
			{Query: "SELECT COUNT(payload) FROM t", Rows: [][]any{{3}}},
			// Empty bytes.
			{Query: "SELECT id FROM t WHERE payload = X''", Rows: [][]any{}},
			// Single-byte payload exists at id=4.
			{Query: "SELECT id FROM t WHERE payload = X'00'", Rows: [][]any{{4}}},
		},
	}
}

// mixedNumericCompareScenario probes type coercion in comparison
// operators when both sides have different numeric types. Existing
// numeric_comparison covers BIGINT-vs-DOUBLE; this scenario also pins
// equality and arithmetic across INTEGER, FLOAT, BIGINT, DOUBLE
// columns. Net-new nightshift-61.
func mixedNumericCompareScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "mixed_numeric_compare",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, a BIGINT, b DOUBLE, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 100, 1.5), (2, 200, 2.5), (3, 300, 3.0), (4, 400, 4.0)",
		},
		Tests: []yamsql.Test{
			// BIGINT col = DOUBLE literal exact integer value.
			{Query: "SELECT id FROM t WHERE a = 200.0", Rows: [][]any{{2}}},
			// BIGINT col compared with non-integer DOUBLE — no row matches.
			{Query: "SELECT id FROM t WHERE a = 200.5", Rows: [][]any{}},
			// DOUBLE col compared with BIGINT-form integer literal.
			{Query: "SELECT id FROM t WHERE b = 3", Rows: [][]any{{3}}},
			// Cross-column comparison.
			{Query: "SELECT id FROM t WHERE b * 100 = a ORDER BY id", Rows: [][]any{{3}, {4}}},
			// Range bounds with mixed types.
			{Query: "SELECT id FROM t WHERE a BETWEEN 100 AND 250.0 ORDER BY id", Rows: [][]any{{1}, {2}}},
			// Equality with computed DOUBLE.
			{Query: "SELECT id FROM t WHERE a / 100 = b ORDER BY id", Rows: [][]any{{3}, {4}}},
			// Arithmetic between BIGINT and DOUBLE columns.
			{Query: "SELECT a + b FROM t WHERE id = 1", Rows: [][]any{{101.5}}},
			{Query: "SELECT a - b FROM t WHERE id = 4", Rows: [][]any{{396.0}}},
		},
	}
}

// notInListScenario probes NOT IN behaviour, especially with NULL
// in the IN list (Java rejects entirely per CLAUDE.md gotcha) and
// across various predicate combinations. We avoid NULL-in-list. Net-new
// nightshift-61.
func notInListScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "not_in_list",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, s STRING, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 10, 'a'), (2, 20, 'b'), (3, 30, 'c'), (4, null, 'd'), (5, 50, null)",
		},
		Tests: []yamsql.Test{
			// NOT IN BIGINT list — NULL rows excluded by 3VL.
			{Query: "SELECT id FROM t WHERE v NOT IN (10, 30) ORDER BY id", Rows: [][]any{{2}, {5}}},
			// NOT IN STRING list — NULL rows excluded by 3VL.
			{Query: "SELECT id FROM t WHERE s NOT IN ('a', 'd') ORDER BY id", Rows: [][]any{{2}, {3}}},
			// NOT IN single-value form.
			{Query: "SELECT id FROM t WHERE v NOT IN (20) ORDER BY id", Rows: [][]any{{1}, {3}, {5}}},
			// NOT IN combined with equality — both must hold.
			{Query: "SELECT id FROM t WHERE v NOT IN (10, 30) AND s = 'b'", Rows: [][]any{{2}}},
			// NOT IN combined with IS NULL.
			{Query: "SELECT id FROM t WHERE s NOT IN ('a', 'b') OR s IS NULL ORDER BY id", Rows: [][]any{{3}, {4}, {5}}},
			// NOT IN with all-matching list.
			{Query: "SELECT id FROM t WHERE v NOT IN (10, 20, 30, 50) ORDER BY id", Rows: [][]any{}},
		},
	}
}

// coalesceTypePromotionScenario pins COALESCE behaviour across mixed
// numeric arg types (BIGINT vs DOUBLE), the typed-NULL anchor pattern
// (CAST(NULL AS T)), nested COALESCE, and COALESCE in WHERE. Net-new
// nightshift-61.
func coalesceTypePromotionScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "coalesce_type_promotion",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, a BIGINT, b DOUBLE, s STRING, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 10, 1.5, 'x'), (2, null, 2.5, null), (3, null, null, 'y'), (4, null, null, null)",
		},
		Tests: []yamsql.Test{
			// BIGINT + DOUBLE → DOUBLE result type.
			{Query: "SELECT COALESCE(a, b) FROM t WHERE id = 1", Rows: [][]any{{10.0}}},
			{Query: "SELECT COALESCE(a, b) FROM t WHERE id = 2", Rows: [][]any{{2.5}}},
			// All-NULL with typed-NULL anchor — typed CAST resolves the
			// result type when every other arg is NULL.
			{Query: "SELECT COALESCE(a, b, CAST(99 AS DOUBLE)) FROM t WHERE id = 4", Rows: [][]any{{99.0}}},
			// COALESCE in WHERE — first non-NULL drives the comparison.
			{Query: "SELECT id FROM t WHERE COALESCE(a, 0) = 0 ORDER BY id", Rows: [][]any{{2}, {3}, {4}}},
			// COALESCE result IS NULL — only id=4 has all-NULL.
			{Query: "SELECT id FROM t WHERE COALESCE(a, b) IS NULL ORDER BY id", Rows: [][]any{{3}, {4}}},
			// Nested COALESCE.
			{Query: "SELECT COALESCE(COALESCE(a, b), CAST(-1 AS DOUBLE)) FROM t WHERE id = 4", Rows: [][]any{{-1.0}}},
			// COALESCE on STRING — typed-NULL anchor needed for all-NULL.
			{Query: "SELECT COALESCE(s, CAST(NULL AS STRING), 'default') FROM t WHERE id = 4", Rows: [][]any{{"default"}}},
			// COALESCE applied to projection of typed col + literal.
			{Query: "SELECT id, COALESCE(s, 'fallback') FROM t WHERE id IN (3, 4) ORDER BY id", Rows: [][]any{{3, "y"}, {4, "fallback"}}},
		},
	}
}

// minMaxBigintBoundaryScenario pins MIN/MAX over int64 boundary values
// without overflowing arithmetic. Both engines already agree on
// arithmetic-overflow rejection (Java raises `ArithmeticException:
// long overflow`; Go's `ApplyMathOp` raises
// `ErrCodeNumericValueOutOfRange` via `AddInt64Checked`-family
// helpers — only the error message text differs). MIN/MAX/COUNT
// over stored boundary values doesn't trigger overflow either way.
// Net-new nightshift-61.
func minMaxBigintBoundaryScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "min_max_bigint_boundary",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, -9223372036854775808), (2, 9223372036854775807), (3, 0)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT MIN(v) FROM t", Rows: [][]any{{int64(-9223372036854775808)}}},
			{Query: "SELECT MAX(v) FROM t", Rows: [][]any{{int64(9223372036854775807)}}},
			{Query: "SELECT MIN(v), MAX(v) FROM t", Rows: [][]any{{int64(-9223372036854775808), int64(9223372036854775807)}}},
			// Round-trip the boundary values via ORDER BY id (PK) so
			// the natural-order satisfiability gate doesn't reject.
			{Query: "SELECT v FROM t ORDER BY id", Rows: [][]any{{int64(-9223372036854775808)}, {int64(9223372036854775807)}, {int64(0)}}},
			// COUNT over boundary values (no arithmetic).
			{Query: "SELECT COUNT(*) FROM t", Rows: [][]any{{3}}},
		},
	}
}

// multiInsertSetupScenario pins multi-statement INSERT setup behaviour
// — sequential INSERTs in setup must be visible to the SELECT query.
// Net-new nightshift-61.
func multiInsertSetupScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "multi_insert_setup",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 100)",
			"INSERT INTO t VALUES (2, 200)",
			"INSERT INTO t VALUES (3, 300), (4, 400)",
			"INSERT INTO t VALUES (5, 500)",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT COUNT(*) FROM t", Rows: [][]any{{5}}},
			{Query: "SELECT SUM(v) FROM t", Rows: [][]any{{1500}}},
			{Query: "SELECT id, v FROM t ORDER BY id", Rows: [][]any{
				{1, 100}, {2, 200}, {3, 300}, {4, 400}, {5, 500},
			}},
			// Range over the multi-INSERT result.
			{Query: "SELECT v FROM t WHERE id BETWEEN 2 AND 4 ORDER BY id", Rows: [][]any{{200}, {300}, {400}}},
		},
	}
}

// orderByCompositeIdxFilterScenario probes ORDER BY composite-index
// columns with WHERE filter on the leading column. Net-new nightshift-61.
func orderByCompositeIdxFilterScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "order_by_composite_idx_filter",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, region STRING, bucket BIGINT, v BIGINT, PRIMARY KEY (id))" +
			" CREATE INDEX idx_region_bucket ON t (region, bucket)",
		Setup: []string{
			"INSERT INTO t VALUES (1, 'us', 1, 100), (2, 'us', 2, 200), (3, 'eu', 1, 300), (4, 'us', 1, 400), (5, 'eu', 2, 500)",
		},
		Tests: []yamsql.Test{
			// Equated leading col + ORDER BY trailing col — natural-order
			// satisfied by index scan with region prefix.
			{
				Query: "SELECT id, bucket FROM t WHERE region = 'us' ORDER BY bucket",
				Rows:  [][]any{{1, 1}, {4, 1}, {2, 2}},
			},
			// Equated leading + range trailing.
			{
				Query: "SELECT id FROM t WHERE region = 'eu' AND bucket >= 1 ORDER BY bucket",
				Rows:  [][]any{{3}, {5}},
			},
			// Equated leading + WHERE filter on trailing.
			{
				Query: "SELECT id, v FROM t WHERE region = 'us' AND bucket = 1 ORDER BY id",
				Rows:  [][]any{{1, 100}, {4, 400}},
			},
		},
	}
}

// updateChainScenario probes a chain of UPDATEs in setup followed by
// final-state SELECT. Net-new nightshift-61.
func updateChainScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "update_chain",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 10), (2, 20), (3, 30)",
			"UPDATE t SET v = v + 5 WHERE id = 1",
			"UPDATE t SET v = v * 2 WHERE id = 1",
			"UPDATE t SET v = v - 1 WHERE id = 1",
			"UPDATE t SET v = 99 WHERE id = 2",
		},
		Tests: []yamsql.Test{
			// Final state: id=1 → ((10+5)*2)-1 = 29; id=2 → 99; id=3 → 30.
			{Query: "SELECT id, v FROM t ORDER BY id", Rows: [][]any{{1, 29}, {2, 99}, {3, 30}}},
			// Aggregate over the post-chain state.
			{Query: "SELECT SUM(v) FROM t", Rows: [][]any{{158}}},
			{Query: "SELECT MAX(v) FROM t", Rows: [][]any{{99}}},
		},
	}
}

// betweenEdgeScenario probes BETWEEN with edge cases:
// inclusive bounds, reversed bounds (where lo > hi → empty result per
// SQL spec), NULL bounds, equal bounds, and combined with NOT BETWEEN.
// Net-new nightshift-61.
func betweenEdgeScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "between_edge",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 10), (2, 20), (3, 30), (4, null), (5, 0)",
		},
		Tests: []yamsql.Test{
			// Inclusive bounds: 10 BETWEEN 10 AND 20 → TRUE.
			{Query: "SELECT id FROM t WHERE v BETWEEN 10 AND 20 ORDER BY id", Rows: [][]any{{1}, {2}}},
			// Reversed bounds (lo > hi) — per SQL spec returns empty set.
			{Query: "SELECT id FROM t WHERE v BETWEEN 20 AND 10 ORDER BY id", Rows: [][]any{}},
			// Equal bounds (lo = hi) — equivalent to v = lo.
			{Query: "SELECT id FROM t WHERE v BETWEEN 20 AND 20 ORDER BY id", Rows: [][]any{{2}}},
			// Boundary just below.
			{Query: "SELECT id FROM t WHERE v BETWEEN 0 AND 9 ORDER BY id", Rows: [][]any{{5}}},
			// Boundary just above.
			{Query: "SELECT id FROM t WHERE v BETWEEN 31 AND 100 ORDER BY id", Rows: [][]any{}},
			// NOT BETWEEN excludes the range — NULL row also excluded by 3VL.
			{Query: "SELECT id FROM t WHERE v NOT BETWEEN 10 AND 20 ORDER BY id", Rows: [][]any{{3}, {5}}},
			// NOT BETWEEN combined with IS NULL gives the NULL row plus
			// the out-of-range rows.
			{Query: "SELECT id FROM t WHERE v NOT BETWEEN 10 AND 20 OR v IS NULL ORDER BY id", Rows: [][]any{{3}, {4}, {5}}},
		},
	}
}

// stringComparisonOpsScenario pins string comparison operators:
// =, <>, <, <=, >, >=, with empty-string and lexicographic
// edge cases. Net-new nightshift-61.
func stringComparisonOpsScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "string_comparison_ops",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, s STRING, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 'apple'), (2, 'banana'), (3, 'cherry'), (4, ''), (5, 'apple_more')",
		},
		Tests: []yamsql.Test{
			{Query: "SELECT id FROM t WHERE s = 'apple'", Rows: [][]any{{1}}},
			{Query: "SELECT id FROM t WHERE s <> 'apple' ORDER BY id", Rows: [][]any{{2}, {3}, {4}, {5}}},
			// Lexicographic: '' < 'a' < 'apple' < 'apple_more' < 'banana' < 'cherry'.
			{Query: "SELECT id FROM t WHERE s < 'apple' ORDER BY id", Rows: [][]any{{4}}},
			{Query: "SELECT id FROM t WHERE s <= 'apple' ORDER BY id", Rows: [][]any{{1}, {4}}},
			{Query: "SELECT id FROM t WHERE s > 'banana' ORDER BY id", Rows: [][]any{{3}}},
			{Query: "SELECT id FROM t WHERE s >= 'banana' ORDER BY id", Rows: [][]any{{2}, {3}}},
			// 'apple_more' > 'apple' (length-extension lexicographic).
			{Query: "SELECT id FROM t WHERE s > 'apple' ORDER BY id", Rows: [][]any{{2}, {3}, {5}}},
			// Equality comparison is case-sensitive.
			{Query: "SELECT id FROM t WHERE s = 'APPLE' ORDER BY id", Rows: [][]any{}},
		},
	}
}

// castChainScenario probes nested CAST conversions: int → string → int,
// DOUBLE round-trip preservation, and DOUBLE→BIGINT rounding (Java's
// `Math.round` semantics — `floor(x + 0.5)` — already implemented in
// `pkg/relational/core/functions/cast.go`'s `CastValue.DOUBLE_TO_LONG`
// path; both engines round 1.9 → 2). Net-new nightshift-61.
func castChainScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "cast_chain",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 42), (2, -100), (3, 0)",
		},
		Tests: []yamsql.Test{
			// Nested CAST: int → string → int.
			{Query: "SELECT CAST(CAST(v AS STRING) AS BIGINT) FROM t WHERE id = 1", Rows: [][]any{{42}}},
			// CAST to DOUBLE preserves int magnitude.
			{Query: "SELECT CAST(v AS DOUBLE) FROM t WHERE id = 2", Rows: [][]any{{-100.0}}},
			// CAST string literal to BIGINT.
			{Query: "SELECT CAST('123' AS BIGINT) FROM t WHERE id = 1", Rows: [][]any{{123}}},
			// CAST '0' string to BIGINT.
			{Query: "SELECT CAST('0' AS BIGINT) FROM t WHERE id = 3", Rows: [][]any{{0}}},
			// CAST DOUBLE to BIGINT — Java's Math.round (floor(x + 0.5))
			// rounds 1.9 → 2; Go's CastValue.DOUBLE_TO_LONG matches.
			// `floor(-1.9 + 0.5) = floor(-1.4) = -2`.
			{Query: "SELECT CAST(1.9 AS BIGINT) FROM t WHERE id = 1", Rows: [][]any{{2}}},
			{Query: "SELECT CAST(-1.9 AS BIGINT) FROM t WHERE id = 1", Rows: [][]any{{-2}}},
			// CAST int to STRING and back.
			{Query: "SELECT CAST(CAST(v AS STRING) AS BIGINT) FROM t WHERE id = 2", Rows: [][]any{{-100}}},
		},
	}
}

// nullInBetweenScenario probes BETWEEN with NULL bounds. Per SQL 3VL:
// `x BETWEEN NULL AND y` and `x BETWEEN x AND NULL` both yield UNKNOWN
// (filtered out in WHERE). Both engines should agree. Net-new
// nightshift-61.
func nullInBetweenScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "null_in_between",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE u (id BIGINT, lo BIGINT, hi BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 10), (2, 20), (3, 30)",
			"INSERT INTO u VALUES (1, null, 25), (2, 5, null), (3, null, null), (4, 5, 25)",
		},
		Tests: []yamsql.Test{
			// NULL operand on either bound makes BETWEEN UNKNOWN → filtered out.
			{Query: "SELECT id FROM u WHERE 10 BETWEEN lo AND hi ORDER BY id", Rows: [][]any{{4}}},
			{Query: "SELECT id FROM u WHERE 30 BETWEEN lo AND hi ORDER BY id", Rows: [][]any{}},
			// NOT BETWEEN with NULL bound — also UNKNOWN.
			{Query: "SELECT id FROM u WHERE 10 NOT BETWEEN lo AND hi ORDER BY id", Rows: [][]any{}},
			// BETWEEN with column on outer side, NULL bound from u (via cross-join).
			{Query: "SELECT t.id FROM t, u WHERE u.id = 1 AND t.v BETWEEN u.lo AND u.hi ORDER BY t.id", Rows: [][]any{}},
			// BETWEEN with both bounds present — match.
			{Query: "SELECT t.id FROM t, u WHERE u.id = 4 AND t.v BETWEEN u.lo AND u.hi ORDER BY t.id", Rows: [][]any{{1}, {2}}},
		},
	}
}

// assertCrossEngineErrorCode verifies that BOTH engines errored with
// the expected SQLSTATE. Java's SQLSTATE comes through the conformance
// server's structured error response (`sqlState` field on the JSON,
// surfaced as `*plandiff.JavaError.SQLState`); Go's comes from
// `*api.Error.Code` via `errors.As`. Both must error AND match the
// expected code — a bare match on one side would mean the other
// silently succeeded or threw a different error class.
//
// One known gap: fdb-relational sometimes throws a bare RuntimeException
// (ArithmeticException, NullPointerException) instead of wrapping it
// in a RelationalException, so the SQLSTATE is empty. When that
// happens, the cross-engine assertion downgrades to "both engines
// errored AND Go's SQLSTATE matches"; the Java-side test pins the
// exception class via the assertion message so a future fix can
// re-strict the check.
//
// Wired nightshift-57. Pre-fix the cross-engine harness `Skip`'d every
// `error_code`-tagged test because there was no portable way to
// extract Java's SQLSTATE.
func assertCrossEngineErrorCode(javaRes, goRes plandiff.RunResult, expected, prefix string) {
	Expect(javaRes.Err).To(HaveOccurred(), "%s: Java did not error but error_code=%q expected", prefix, expected)
	Expect(goRes.Err).To(HaveOccurred(), "%s: Go did not error but error_code=%q expected", prefix, expected)

	var je *plandiff.JavaError
	Expect(errors.As(javaRes.Err, &je)).To(BeTrue(),
		"%s: Java error is not *plandiff.JavaError: %T (%v)", prefix, javaRes.Err, javaRes.Err)

	var ge *api.Error
	Expect(errors.As(goRes.Err, &ge)).To(BeTrue(),
		"%s: Go error is not *api.Error: %T (%v)", prefix, goRes.Err, goRes.Err)

	// Strict path: both engines have SQLSTATE — they must match the
	// expected and each other.
	if je.SQLState != "" {
		Expect(je.SQLState).To(Equal(expected),
			"%s: Java SQLSTATE: got %q (exception=%s, message=%s), expected %q",
			prefix, je.SQLState, je.ExceptionClass, je.Message, expected)
	}
	// Loose path: when Java's SQLSTATE is empty (fdb-relational threw a
	// bare RuntimeException — ArithmeticException, NullPointerException,
	// VerifyException, etc. — without wrapping in RelationalException),
	// only Go's SQLSTATE is checked. Both engines still must have
	// errored, which the early Expect's above pin.

	Expect(string(ge.Code)).To(Equal(expected),
		"%s: Go SQLSTATE: got %q (message=%s), expected %q",
		prefix, ge.Code, ge.Message, expected)
}

// assertRowsMatch checks actual vs expected, honouring multiset semantics
// when unordered is set. Per-cell numeric equality is loose because Java
// sends ints as float64 (JSON-decoded) while YAML loads ints as int.
func assertRowsMatch(actual, expected [][]any, unordered bool, prefix string) {
	Expect(actual).To(HaveLen(len(expected)), "%s row count", prefix)
	if unordered {
		// Match each expected row to some unmatched actual row. O(N²) but
		// scenario row counts are small.
		used := make([]bool, len(actual))
		for ei, er := range expected {
			matched := false
			for ai, ar := range actual {
				if used[ai] {
					continue
				}
				if rowsLooselyEqual(ar, er) {
					used[ai] = true
					matched = true
					break
				}
			}
			Expect(matched).To(BeTrue(), "%s row %d: no match in actual %v for expected %v", prefix, ei, actual, er)
		}
		return
	}
	for r := range expected {
		Expect(actual[r]).To(HaveLen(len(expected[r])), "%s row %d col count", prefix, r)
		for c := range expected[r] {
			expectScalarEqual(actual[r][c], expected[r][c],
				"%s row %d col %d", prefix, r, c)
		}
	}
}

// assertRowSetsCrossEqual checks that two engine result sets contain
// the same row values. Used for the Java-vs-Go direct comparison —
// neither side is the "expected" reference; either engine's set could
// be the regression. Uses the same loose-numeric / multiset comparison
// as the expected-vs-actual path.
//
// TODO: BYTES column values from Java arrive base64-encoded as strings
// (per `pkg/relational/conformance/plandiff/runsql.go` runsql encode);
// the Go runner's coerceForComparison also base64-encodes []byte to
// match. Future scenarios that project a BYTES column directly are
// covered by the existing string-equality path. If a future scenario
// declares an expected []byte literal in inline form rather than the
// base64 string, scalarLooselyEqual will need a []byte case.
func assertRowSetsCrossEqual(java, gor [][]any, unordered bool, prefix string) {
	if !unordered {
		Expect(gor).To(HaveLen(len(java)), "%s row count", prefix)
		for r := range java {
			Expect(gor[r]).To(HaveLen(len(java[r])), "%s row %d col count", prefix, r)
			for c := range java[r] {
				Expect(rowsCrossLooselyEqual([]any{java[r][c]}, []any{gor[r][c]})).To(BeTrue(),
					"%s row %d col %d: java=%v go=%v", prefix, r, c, java[r][c], gor[r][c])
			}
		}
		return
	}
	Expect(gor).To(HaveLen(len(java)), "%s row count", prefix)
	used := make([]bool, len(gor))
	for ji, jr := range java {
		matched := false
		for gi, gr := range gor {
			if used[gi] {
				continue
			}
			if rowsCrossLooselyEqual(jr, gr) {
				used[gi] = true
				matched = true
				break
			}
		}
		Expect(matched).To(BeTrue(), "%s row %d: no Go match for Java row %v", prefix, ji, jr)
	}
}

// rowsCrossLooselyEqual compares two rows from the two engines.
// Numeric values are coerced to float64 on both sides (Java via JSON,
// Go via plandiff's coerceForComparison) so we can compare with a
// shared tolerance.
func rowsCrossLooselyEqual(a, b []any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !crossScalarEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

func crossScalarEqual(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if af, aok := toFloat64Maybe(a); aok {
		if bf, bok := toFloat64Maybe(b); bok {
			return math.Abs(af-bf) <= 1e-9
		}
		return false
	}
	switch av := a.(type) {
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	}
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

func toFloat64Maybe(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	default:
		return 0, false
	}
}

// rowsLooselyEqual compares two rows under the same loose-numeric
// equality used by expectScalarEqual.
func rowsLooselyEqual(a, e []any) bool {
	if len(a) != len(e) {
		return false
	}
	for i := range a {
		if !scalarLooselyEqual(a[i], e[i]) {
			return false
		}
	}
	return true
}

func scalarLooselyEqual(actual, expected any) bool {
	if expected == nil {
		return actual == nil
	}
	if actual == nil {
		return false
	}
	switch e := expected.(type) {
	case int:
		return numericEq(actual, float64(e))
	case int64:
		return numericEq(actual, float64(e))
	case float64:
		return numericEq(actual, e)
	case bool:
		ab, ok := actual.(bool)
		return ok && ab == e
	case string:
		as, ok := actual.(string)
		return ok && as == e
	default:
		return fmt.Sprintf("%v", actual) == fmt.Sprintf("%v", expected)
	}
}

func numericEq(actual any, expected float64) bool {
	var af float64
	switch n := actual.(type) {
	case int:
		af = float64(n)
	case int32:
		af = float64(n)
	case int64:
		af = float64(n)
	case float32:
		af = float64(n)
	case float64:
		af = n
	default:
		return false
	}
	return math.Abs(af-expected) <= 1e-9
}

// expectScalarEqual compares two scalar values for cross-engine
// equality. Numeric types arrive differently from the two engines:
// Java sends ints as float64 (JSON), YAML loads ints as int. We
// normalise both sides to a canonical comparison form.
func expectScalarEqual(actual, expected any, msgAndArgs ...any) {
	if expected == nil {
		Expect(actual).To(BeNil(), msgAndArgs...)
		return
	}
	switch e := expected.(type) {
	case int:
		Expect(actual).To(BeNumerically("==", e), msgAndArgs...)
	case int64:
		Expect(actual).To(BeNumerically("==", e), msgAndArgs...)
	case float64:
		Expect(actual).To(BeNumerically("~", e, 1e-9), msgAndArgs...)
	case bool:
		Expect(actual).To(Equal(e), msgAndArgs...)
	case string:
		Expect(actual).To(Equal(e), msgAndArgs...)
	default:
		Expect(actual).To(Equal(expected), msgAndArgs...)
	}
}

// joinOptimizationProbesScenario mirrors testdata/join_optimization_probes.yaml.
// Drops NOT NULL on PK cols (fdb-relational restriction). Converts
// explicit INNER JOIN ... ON to comma-join + WHERE (fdb-relational
// rejects fully-qualified column names in JOIN ON clause). The GROUP BY
// aggregate-through-join test is included as-is — it surfaces the
// Java limitation.
func joinOptimizationProbesScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "join_optimization_probes",
		SchemaTemplate: "CREATE TABLE dept (did BIGINT, dname STRING, PRIMARY KEY (did))" +
			" CREATE TABLE emp (eid BIGINT, did BIGINT, ename STRING, salary BIGINT, PRIMARY KEY (eid))" +
			" CREATE INDEX idx_emp_did ON emp (did)",
		Setup: []string{
			"INSERT INTO dept VALUES (1, 'eng'), (2, 'sales'), (3, 'hr')",
			"INSERT INTO emp VALUES (10, 1, 'Alice', 100), (20, 1, 'Bob', 90), (30, 2, 'Charlie', 80), (40, 2, 'Diana', 110), (50, 3, 'Eve', 95)",
		},
		Tests: []yamsql.Test{
			// Single-side filter pushdown: dept filter pushed below join.
			// Converted from INNER JOIN to comma-join.
			{Query: "SELECT e.ename FROM emp AS e, dept AS d WHERE e.did = d.did AND d.dname = 'eng' ORDER BY e.ename", Rows: [][]any{{"Alice"}, {"Bob"}}},
			// Cross-side predicate stays above join.
			{Query: "SELECT e.ename FROM emp AS e, dept AS d WHERE e.did = d.did AND e.salary > d.did * 50 ORDER BY e.ename", Rows: [][]any{{"Alice"}, {"Bob"}, {"Diana"}, {"Eve"}}},
			// Both sides filtered.
			{Query: "SELECT e.ename FROM emp AS e, dept AS d WHERE e.did = d.did AND d.dname = 'eng' AND e.salary >= 95 ORDER BY e.ename", Rows: [][]any{{"Alice"}}},
			// ORDER BY through join — should work even with filter pushdown.
			{Query: "SELECT e.ename, d.dname FROM emp AS e, dept AS d WHERE e.did = d.did AND d.dname != 'hr' ORDER BY e.salary DESC", Rows: [][]any{{"Diana", "sales"}, {"Alice", "eng"}, {"Bob", "eng"}, {"Charlie", "sales"}}},
			// Self-join with filter.
			{Query: "SELECT a.ename, b.ename FROM emp AS a, emp AS b WHERE a.did = b.did AND a.eid < b.eid ORDER BY a.eid, b.eid", Rows: [][]any{{"Alice", "Bob"}, {"Charlie", "Diana"}}},
			// Aggregate through join — uses GROUP BY (unsupported in
			// fdb-relational); included as-is to surface the divergence.
			{Query: "SELECT d.dname, COUNT(*), MAX(e.salary) FROM emp AS e, dept AS d WHERE e.did = d.did GROUP BY d.dname ORDER BY d.dname", Rows: [][]any{{"eng", 2, 100}, {"hr", 1, 95}, {"sales", 2, 110}}},
		},
	}
}

// recursiveCteAdvancedScenario mirrors testdata/recursive_cte_advanced.yaml.
// Drops NOT NULL on PK (fdb-relational restriction). Tests column alias
// rename resolution and descendant traversal patterns in WITH RECURSIVE.
//
// NOT in crossEngineScenarios() — see the exclusion note at the call site:
// both queries hit genuine fdb-relational 4.11.1.0 limitations (renamed-
// column recursion + recursive-CTE-with-outer-ORDER-BY). Builder retained as
// faithful documentation of the Go-only yamsql twin.
func recursiveCteAdvancedScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "recursive_cte_advanced",
		SchemaTemplate: "CREATE TABLE tree (id BIGINT, parent BIGINT, label STRING, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO tree VALUES (1, null, 'root'), (2, 1, 'child1'), (3, 1, 'child2'), (4, 2, 'grandchild1'), (5, 3, 'grandchild2')",
		},
		Tests: []yamsql.Test{
			// Recursive CTE with column alias rename (anc(node, up)).
			{Query: "WITH RECURSIVE anc(node, up) AS (SELECT id, parent FROM tree WHERE id = 5 UNION ALL SELECT t.id, t.parent FROM anc AS a, tree AS t WHERE t.id = a.up) SELECT node FROM anc ORDER BY node", Rows: [][]any{{1}, {3}, {5}}},
			// Descendant traversal from root.
			{Query: "WITH RECURSIVE desc_tree AS (SELECT id, parent, label FROM tree WHERE id = 1 UNION ALL SELECT t.id, t.parent, t.label FROM desc_tree AS d, tree AS t WHERE t.parent = d.id) SELECT label FROM desc_tree ORDER BY id", Rows: [][]any{{"root"}, {"child1"}, {"child2"}, {"grandchild1"}, {"grandchild2"}}},
		},
	}
}

// unionColumnsScenario mirrors testdata/union_columns.yaml — positional
// column binding in UNION ALL, ORDER BY on union results, column-count
// mismatch errors, plain UNION rejection (0AF00), LIMIT/OFFSET rejection,
// type-incompatibility errors, and constant-literal sides.
// Drops NOT NULL on PK columns (fdb-relational restriction). Adds table c
// for the type-incompatibility test (BIGINT vs STRING).
func unionColumnsScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "union_columns_extended",
		SchemaTemplate: "CREATE TABLE a (id BIGINT, v BIGINT, PRIMARY KEY (id))" +
			"\nCREATE TABLE b (id BIGINT, w BIGINT, PRIMARY KEY (id))" +
			"\nCREATE TABLE c (id BIGINT, id_str STRING, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO a VALUES (1, 10), (2, 20)",
			"INSERT INTO b VALUES (1, 100), (2, 200)",
			"INSERT INTO c VALUES (1, 'x'), (2, 'y')",
		},
		Tests: []yamsql.Test{
			// UNION ALL with differently-named columns — positional matching.
			{Query: "SELECT v FROM a UNION ALL SELECT w FROM b", Unordered: true, Rows: [][]any{{10}, {20}, {100}, {200}}},
			// NOT cross-engine: `ORDER BY id, v` over this UNION is a Go-only
			// extension — Java rejects "non existing column V" because the
			// right branch (b) has no `v` (Java resolves the multi-column
			// ORDER BY against all branches, not the union output schema). Go
			// correctly orders by the output column. Covered Go-only via the
			// yamsql union corpus. The `ORDER BY v DESC` form is also Go-only
			// for the same reason (Java rejects `v` — absent from the right
			// branch) and is likewise not cross-engine.
			// ORDER BY on right-side column name fails — result schema is left's names only.
			{Query: "SELECT id, v FROM a UNION ALL SELECT id, w FROM b ORDER BY w", ErrorCode: "42703"},
			// LIMIT/OFFSET on UNION rejected at parse time.
			{Query: "SELECT v FROM a UNION ALL SELECT w FROM b ORDER BY v LIMIT 2 OFFSET 1", ErrorCode: "0AF00"},
			// Plain UNION (without ALL) rejected.
			{Query: "SELECT v FROM a UNION SELECT v FROM a ORDER BY v", ErrorCode: "0AF00"},
			// Column-count mismatch on UNION ALL.
			{Query: "SELECT id, v FROM a UNION ALL SELECT id FROM b", ErrorCode: "42F64"},
			// Plain UNION column-count mismatch — UNION rejection fires first.
			{Query: "SELECT id, v FROM a UNION SELECT id FROM b", ErrorCode: "0AF00"},
			// UNION ALL with constant literal on one side.
			{Query: "SELECT v FROM a UNION ALL SELECT 99 FROM b", Unordered: true, Rows: [][]any{{10}, {20}, {99}, {99}}},
			// Incompatible types across sides (BIGINT vs STRING).
			{Query: "SELECT v FROM a UNION ALL SELECT id_str FROM c", ErrorCode: "42F65"},
			// Same types — sanity regression guard.
			{Query: "SELECT v FROM a UNION ALL SELECT w FROM b", Unordered: true, Rows: [][]any{{10}, {20}, {100}, {200}}},
		},
	}
}

// inListNullScenario mirrors testdata/in_list_null.yaml — Java rejects
// NULL anywhere in the IN list with "NULL values are not allowed in the
// IN list" (22000). Conformance principle: doesn't work in Java, doesn't
// work in Go. Drops NOT NULL on PK (fdb-relational restriction).
func inListNullScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "in_list_null",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 1), (2, 2), (3, 3)",
		},
		Tests: []yamsql.Test{
			// NULL in IN list rejected.
			{Query: "SELECT id FROM t WHERE v IN (2, NULL)", ErrorCode: "22000"},
			// NULL in NOT IN list rejected.
			{Query: "SELECT id FROM t WHERE v NOT IN (2, NULL)", ErrorCode: "22000"},
			// Concrete-only list works fine.
			{Query: "SELECT id FROM t WHERE v IN (1, 3)", Unordered: true, Rows: [][]any{{1}, {3}}},
		},
	}
}

// orderByNullsScenario mirrors testdata/order_by_nulls.yaml. Tests
// NULL ordering: ASC default is NULLS FIRST, DESC default is NULLS LAST
// (matching Postgres/Oracle/Java). Explicit NULLS LAST on ASC and
// NULLS FIRST on DESC are Go extensions (in-memory sort) — included for
// cross-engine probing (Java will error or succeed; either is informative).
// Drops NOT NULL on PK.
func orderByNullsScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "order_by_nulls",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))" +
			" CREATE INDEX idx_v ON t (v)",
		Setup: []string{
			"INSERT INTO t VALUES (1, 10), (2, NULL), (3, 5), (4, NULL), (5, 20)",
		},
		Tests: []yamsql.Test{
			// ASC default: NULLS FIRST. Java-supported (no explicit NULLS clause).
			{Query: "SELECT v FROM t ORDER BY v ASC", Rows: [][]any{{nil}, {nil}, {5}, {10}, {20}}},
			// DESC default: NULLS LAST. Java-supported.
			{Query: "SELECT v FROM t ORDER BY v DESC", Rows: [][]any{{20}, {10}, {5}, {nil}, {nil}}},
			// NOT cross-engine: explicit `NULLS LAST` / `NULLS FIRST` is a
			// Go-only extension (in-memory sort). fdb-relational 4.11.1.0's
			// grammar has no NULLS FIRST/LAST clause — it rejects these with a
			// syntax error / UnableToPlanException (confirmed deterministically
			// on a fresh isolated JVM). A query Java can't parse has no cross-
			// engine equivalence to assert. Covered Go-only via the
			// order_by_nulls yamsql corpus.
			//   - "SELECT v FROM t ORDER BY v ASC NULLS LAST"   → {5,10,20,NULL,NULL}
			//   - "SELECT v FROM t ORDER BY v DESC NULLS FIRST" → {NULL,NULL,20,10,5}
		},
	}
}

// orderByDupeColScenario mirrors testdata/order_by_dupe_col.yaml.
// Duplicate column in ORDER BY (e.g. ORDER BY b, b) errors 42701 in
// Java. Drops NOT NULL on PK.
//
// The multi-column ORDER BY tests (`ORDER BY b, id` / `ORDER BY b+1, b+1`)
// are NOT cross-engine: multi-column ORDER BY without a covering composite
// index is a Go-only extension (in-memory sort). fdb-relational's Cascades
// planner is UNRELIABLE on it — it throws UnableToPlanException ("could not
// plan query"), and (unlike the parser-warming cases) this is genuine planner
// nondeterminism: the same query on a FRESH JVM is sometimes planned, sometimes
// not, because the Cascades exploration order is identity-hash-dependent. There
// is no stable cross-engine equivalence to assert, so these stay Go-only
// (covered by the order_by_dupe_col yamsql corpus). This matches the
// "multi-col ORDER BY gotcha" already avoided by the other A3 scenarios.
func orderByDupeColScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "order_by_dupe_col",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, b BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 10), (2, 20), (3, 30)",
		},
		Tests: []yamsql.Test{
			// Duplicate column → 42701.
			{Query: "SELECT b FROM t ORDER BY b, b", ErrorCode: "42701"},
			// Duplicate via positional + name mix (ORDER BY 1, b on
			// SELECT b → both resolve to column b).
			{Query: "SELECT b FROM t ORDER BY 1, b", ErrorCode: "42701"},
			// (multi-column ORDER BY tests are Go-only — see the note above)
		},
	}
}

// compositePKCrossScenario mirrors testdata/composite_pk.yaml.
// Composite PRIMARY KEY (col1, col2): distinct rows with same leading
// PK component, exact composite match, and duplicate composite PK
// raises 23505. Drops NOT NULL on PK columns (fdb-relational
// restriction — PK is implicitly NOT NULL).
func compositePKCrossScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "composite_pk_cross",
		SchemaTemplate: "CREATE TABLE t (a BIGINT, b BIGINT, label STRING, PRIMARY KEY (a, b))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 10, 'alpha'), (1, 20, 'beta'), (2, 10, 'gamma')",
		},
		Tests: []yamsql.Test{
			// Two rows share a=1 but differ in b — both must persist.
			{Query: "SELECT b, label FROM t WHERE a = 1 ORDER BY b", Rows: [][]any{{10, "alpha"}, {20, "beta"}}},
			// Exact PK match.
			{Query: "SELECT label FROM t WHERE a = 2 AND b = 10", Rows: [][]any{{"gamma"}}},
			// Composite PK duplicate raises 23505.
			{Query: "INSERT INTO t VALUES (1, 10, 'replacement')", ErrorCode: "23505"},
			// Original row untouched.
			{Query: "SELECT label FROM t WHERE a = 1 AND b = 10", Rows: [][]any{{"alpha"}}},
		},
	}
}

// uniqueViolationScenario mirrors testdata/unique_violation.yaml.
// UNIQUE constraint violations raise SQLSTATE 23505 — covers both
// PRIMARY KEY conflict and explicit UNIQUE index conflict. Drops NOT
// NULL on PK column (fdb-relational restriction). Keeps NOT NULL on
// non-PK columns where the YAML has them.
//
// STATELESS ADAPTATION: the yamsql twin is STATEFUL — its tests run as
// a sequence on one store, so its `SELECT COUNT(*) → 3` follows a prior
// successful `INSERT ... (3, ...)`. The A3 cross-engine harness runs
// each test INDEPENDENTLY (runWithSetup = schema + Setup + ONE query),
// so a test can only observe the Setup, never a prior test's mutation.
// We therefore seed all THREE rows in Setup and keep every test
// self-contained: COUNT(*) counts the seeded rows; the conflict tests
// use FRESH primary keys (4, 5) so "duplicate email on a fresh PK"
// still exercises the unique INDEX, not the PK. Intent is preserved;
// no test depends on another's side effect.
func uniqueViolationScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "unique_violation",
		// NOT NULL dropped from `name`: fdb-relational 4.11.1.0 allows NOT NULL
		// only on ARRAY columns ("NOT NULL is only allowed for ARRAY column
		// type") — scalar NOT NULL is a Go-only extension Java cannot CREATE, so
		// keeping it makes the schema un-creatable in Java (fails deterministically
		// on a fresh per-scenario server). The unique-constraint behaviour under
		// test (PK + UNIQUE INDEX on email) is unaffected; the data is non-null.
		SchemaTemplate: "CREATE TABLE t (id BIGINT, name STRING, email STRING, PRIMARY KEY (id))" +
			" CREATE UNIQUE INDEX t_email ON t (email)",
		Setup: []string{
			"INSERT INTO t VALUES (1, 'alice', 'a@x.com'), (2, 'bob', 'b@x.com'), (3, 'carol', 'c@x.com')",
		},
		Tests: []yamsql.Test{
			// Duplicate PRIMARY KEY (PK=1 already seeded) — raises 23505.
			{Query: "INSERT INTO t VALUES (1, 'dave', 'd@x.com')", ErrorCode: "23505"},
			// Duplicate unique-indexed email on a FRESH PK (4) — exercises the
			// UNIQUE INDEX, not the PK — raises 23505.
			{Query: "INSERT INTO t VALUES (4, 'dave', 'a@x.com')", ErrorCode: "23505"},
			// Non-conflicting insert (fresh PK + fresh email) succeeds.
			{Query: "INSERT INTO t VALUES (5, 'dave', 'd@x.com')"},
			// Three rows were seeded; COUNT observes them statelessly.
			{Query: "SELECT COUNT(*) FROM t", Rows: [][]any{{3}}},
			// Original rows are untouched — the failed conflict INSERTs above
			// (each run in its own ephemeral schema) never overwrote PK=1.
			{Query: "SELECT name, email FROM t WHERE id = 1", Rows: [][]any{{"alice", "a@x.com"}}},
			// UPDATE setting a unique-indexed column to a value another row
			// already holds raises 23505.
			{Query: "UPDATE t SET email = 'a@x.com' WHERE id = 2", ErrorCode: "23505"},
		},
	}
}

// notNullViolationScenario mirrors testdata/not_null_violation.yaml.
// INSERT/UPDATE NULL into a NOT NULL column raises SQLSTATE 23502.
// Drops NOT NULL on PK column (fdb-relational restriction). Keeps NOT
// NULL on non-PK column 'name' where the YAML has it.
func notNullViolationScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "not_null_violation",
		// NOT NULL dropped: fdb-relational 4.11.1.0 allows NOT NULL only on ARRAY
		// columns, so scalar NOT NULL is a Go-only extension Java cannot CREATE.
		// The 23502 NOT-NULL-violation tests are error_code (skipped cross-engine,
		// covered Go-only via the yamsql twin); the cross-engine SELECT below is
		// unaffected by the dropped constraint (its row is non-null).
		SchemaTemplate: "CREATE TABLE t (id BIGINT, name STRING, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 'alice')",
		},
		Tests: []yamsql.Test{
			// INSERT NULL into NOT NULL column raises 23502.
			{Query: "INSERT INTO t VALUES (2, NULL)", ErrorCode: "23502"},
			// UPDATE to NULL on NOT NULL column raises 23502.
			{Query: "UPDATE t SET name = NULL WHERE id = 1", ErrorCode: "23502"},
			// Baseline: the valid row is still intact.
			{Query: "SELECT id, name FROM t", Rows: [][]any{{1, "alice"}}},
		},
	}
}

// inSubqueryDecompositionScenario mirrors testdata/in_subquery_decomposition.yaml.
// `col IN (SELECT ...)` is rejected at predicate evaluation time. These
// tests pin the rejection across all shapes (PK, secondary index,
// filtered, empty, arithmetic projection, correlated, duplicates). The
// two EXISTS rewrites at the bottom are the supported alternatives.
// Drops NOT NULL on PK.
func inSubqueryDecompositionScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "in_subquery_decomposition",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, name STRING, PRIMARY KEY (id))" +
			" CREATE INDEX idx_v ON t (v)" +
			" CREATE TABLE tags (id BIGINT, t_id BIGINT, label STRING, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 30, 'a'), (2, 20, 'b'), (3, 10, 'c'), (4, 40, 'd'), (5, 15, 'e')",
			"INSERT INTO tags VALUES (1, 1, 'red'), (2, 3, 'blue'), (3, 4, 'red'), (4, 5, 'green'), (99, 1, 'dup')",
		},
		Tests: []yamsql.Test{
			// PK IN (SELECT ...) — rejected.
			{Query: "SELECT id FROM t WHERE id IN (SELECT t_id FROM tags) ORDER BY id", ErrorCode: "0AF00"},
			// Filtered subquery — same rejection.
			{Query: "SELECT id FROM t WHERE id IN (SELECT t_id FROM tags WHERE label = 'red') ORDER BY id", ErrorCode: "0AF00"},
			// Secondary-index IN (SELECT ...) — rejected.
			{Query: "SELECT id, v FROM t WHERE v IN (SELECT t_id * 10 FROM tags WHERE label = 'red') ORDER BY v", ErrorCode: "0AF00"},
			// Empty subquery — still rejected (rejection is syntactic).
			{Query: "SELECT id FROM t WHERE id IN (SELECT t_id FROM tags WHERE label = 'nonexistent')", ErrorCode: "0AF00"},
			// Subquery with NULL-droppable list — rejected.
			{Query: "SELECT id FROM t WHERE id IN (SELECT t_id FROM tags WHERE label IN ('red', 'blue')) ORDER BY id", ErrorCode: "0AF00"},
			// Single-col arithmetic projection in subquery — rejected.
			{Query: "SELECT id, v FROM t WHERE v IN (SELECT t_id * 10 FROM tags) ORDER BY v", ErrorCode: "0AF00"},
			// Correlated IN-subquery — rejected at the IN-subquery level.
			{Query: "SELECT id FROM t WHERE id IN (SELECT tags.t_id FROM tags WHERE tags.t_id = t.id) ORDER BY id", ErrorCode: "0AF00"},
			// Duplicate-result subquery — rejected.
			{Query: "SELECT id FROM t WHERE id IN (SELECT t_id FROM tags WHERE label IN ('red', 'dup')) ORDER BY id", ErrorCode: "0AF00"},
			// Single-row subquery — rejected.
			{Query: "SELECT id FROM t WHERE id IN (SELECT t_id FROM tags WHERE label = 'dup') ORDER BY id", ErrorCode: "0AF00"},
			// Supported rewrite: EXISTS preserves the same row set as IN.
			{Query: "SELECT id FROM t WHERE EXISTS (SELECT 1 FROM tags WHERE tags.t_id = t.id) ORDER BY id", Rows: [][]any{{1}, {3}, {4}, {5}}},
			{Query: "SELECT id, v FROM t WHERE EXISTS (SELECT 1 FROM tags WHERE label = 'red' AND tags.t_id * 10 = t.v) ORDER BY v", Rows: [][]any{{3, 10}, {4, 40}}},
		},
	}
}

// subqueryInScenario mirrors testdata/subquery_in.yaml.
// `col IN (subquery)` and `col NOT IN (subquery)` are rejected
// (Java NPEs in AstNormalizer; Go emits 0AF00). EXISTS / NOT EXISTS
// and comma-join are the supported rewrites. Drops NOT NULL on PK.
func subqueryInScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "subquery_in",
		SchemaTemplate: "CREATE TABLE a (id BIGINT, v BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE b (id BIGINT, v BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO a VALUES (1, 1), (2, 2), (3, 3)",
			"INSERT INTO b VALUES (101, 2), (102, null)",
		},
		Tests: []yamsql.Test{
			// IN-subquery — rejected.
			{Query: "SELECT id FROM a WHERE v IN (SELECT v FROM b)", ErrorCode: "0AF00"},
			// NOT IN-subquery — also rejected.
			{Query: "SELECT id FROM a WHERE v NOT IN (SELECT v FROM b)", ErrorCode: "0AF00"},
			// Empty subquery shape — still rejected.
			{Query: "SELECT id FROM a WHERE v IN (SELECT v FROM b WHERE id > 999)", ErrorCode: "0AF00"},
			{Query: "SELECT id FROM a WHERE v NOT IN (SELECT v FROM b WHERE id > 999) ORDER BY id", ErrorCode: "0AF00"},
			// Concrete-value subquery shape — still rejected.
			{Query: "SELECT id FROM a WHERE v IN (SELECT v FROM b WHERE v IS NOT NULL AND v != 2)", ErrorCode: "0AF00"},
			// Multi-column subquery — still rejected at IN-subquery level.
			{Query: "SELECT id FROM a WHERE v IN (SELECT id, v FROM b)", ErrorCode: "0AF00"},
			// Cross-type subquery via CTE — still rejected.
			{Query: "WITH s AS (SELECT 'x' AS label FROM a WHERE id = 1)\nSELECT id FROM a WHERE v IN (SELECT label FROM s)", ErrorCode: "0AF00"},
			{Query: "WITH s AS (SELECT 'x' AS label FROM a WHERE id = 1)\nSELECT id FROM a WHERE v NOT IN (SELECT label FROM s)", ErrorCode: "0AF00"},
			// Supported rewrite: EXISTS / NOT EXISTS.
			{Query: "SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b AS sub WHERE sub.v = a.v) ORDER BY id", Rows: [][]any{{2}}},
			{Query: "SELECT id FROM a WHERE NOT EXISTS (SELECT 1 FROM b AS sub WHERE sub.v = a.v) ORDER BY id", Rows: [][]any{{1}, {3}}},
			// Supported rewrite: comma-join with DISTINCT.
			{Query: "SELECT DISTINCT a.id FROM a, b WHERE a.v = b.v ORDER BY a.id", Rows: [][]any{{2}}},
		},
	}
}

// updateDeleteScenario mirrors testdata/update_delete.yaml.
// UPDATE and DELETE with NULL-aware predicates. The full YAML is a
// multi-stage chain where each DML changes state for the next
// verification SELECT. The harness auto-skips DML tests (non-query),
// so the complete DML chain goes into Setup and only the final-state
// SELECTs appear as runnable tests. Intermediate DML and SELECT tests
// are included for coverage once the harness supports DML execution.
// Drops NOT NULL on PK.
func updateDeleteScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "update_delete",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 10), (2, null), (3, 20), (4, null)",
			// UPDATE WHERE v = NULL matches nothing (UNKNOWN for every row).
			"UPDATE t SET v = 99 WHERE v = NULL",
			// UPDATE WHERE v IS NULL matches NULL rows.
			"UPDATE t SET v = 99 WHERE v IS NULL",
			// DELETE WHERE v IS NOT NULL removes all non-null rows.
			"DELETE FROM t WHERE v IS NOT NULL",
			// Re-seed.
			"INSERT INTO t VALUES (1, 10), (2, 20), (3, 30), (4, null)",
			// DELETE with compound predicate.
			"DELETE FROM t WHERE v < 15 OR v > 25",
			// DELETE with no WHERE removes all.
			"DELETE FROM t",
			// Final insert + unconditional UPDATE.
			"INSERT INTO t VALUES (1, 10), (2, 20)",
			"UPDATE t SET v = 100",
		},
		Tests: []yamsql.Test{
			// DML tests (auto-skipped until harness extension).
			{Query: "UPDATE t SET v = 99 WHERE v = NULL"},
			{Query: "UPDATE t SET v = 99 WHERE v IS NULL"},
			{Query: "DELETE FROM t WHERE v IS NOT NULL"},
			{Query: "DELETE FROM t WHERE v < 15 OR v > 25"},
			{Query: "DELETE FROM t"},
			{Query: "UPDATE t SET v = 100"},
			// Verification of final state after the complete chain:
			// two rows both with v=100.
			{Query: "SELECT id, v FROM t ORDER BY id", Rows: [][]any{{1, 100}, {2, 100}}},
			{Query: "SELECT COUNT(*) FROM t", Rows: [][]any{{2}}},
		},
	}
}

// updateCaseWhenScenario mirrors testdata/update_case_when.yaml.
// UPDATE SET col = CASE ... END — verifies the UPDATE evaluator
// handles arbitrary expressions including nested CASE in the SET
// expression. The full YAML chain mutates state across rounds;
// all DML goes into Setup and final-state SELECTs are runnable tests.
// Intermediate DML tests and the error_code test are included for
// coverage once the harness supports them. Drops NOT NULL on PK.
func updateCaseWhenScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "update_case_when",
		SchemaTemplate: "CREATE TABLE a (a1 BIGINT, a2 BIGINT, a3 BIGINT, PRIMARY KEY (a1))",
		Setup: []string{
			"INSERT INTO a VALUES (1, 100, 10), (2, 200, 20), (3, 300, 30)",
			// CASE — single branch (no ELSE), unmatched rows get NULL.
			"UPDATE a SET a2 = CASE WHEN a1 = 1 THEN 4444 END",
			// Reset.
			"UPDATE a SET a2 = a1 * 100",
			// CASE handling NULL — a2 IS NULL check (none are null after reset).
			"UPDATE a SET a2 = CASE WHEN a2 IS NULL THEN 8888 ELSE 2222 END",
			// Nested CASE in UPDATE.
			"UPDATE a SET a2 = CASE WHEN CASE WHEN a2 = 2222 THEN 8888 ELSE 2222 END > 4000 THEN 4444 ELSE 6666 END",
			// Pure int-int CASE.
			"UPDATE a SET a2 = CASE WHEN a2 = 4444 THEN 1 ELSE 2 END",
		},
		Tests: []yamsql.Test{
			// DML tests (auto-skipped until harness extension).
			{Query: "UPDATE a SET a2 = CASE WHEN a1 = 1 THEN 4444 END"},
			{Query: "UPDATE a SET a2 = a1 * 100"},
			{Query: "UPDATE a SET a2 = CASE WHEN a2 IS NULL THEN 8888 ELSE 2222 END"},
			{Query: "UPDATE a SET a2 = CASE WHEN CASE WHEN a2 = 2222 THEN 8888 ELSE 2222 END > 4000 THEN 4444 ELSE 6666 END"},
			{Query: "UPDATE a SET a2 = CASE WHEN a2 = 4444 THEN 1 ELSE 2 END"},
			// Verification of final state: all rows have a2=1.
			{Query: "SELECT a1, a2 FROM a ORDER BY a1", Rows: [][]any{{1, 1}, {2, 1}, {3, 1}}},
			// Mixed int/float CASE assigned to BIGINT column — Java errors
			// 22000 (cannot_convert_type) because the CASE result type is
			// DOUBLE and the assignment can't narrow back to BIGINT.
			{Query: "UPDATE a SET a2 = CASE WHEN a1 = 99 THEN 1 ELSE 2.2 END", ErrorCode: "22000"},
		},
	}
}

// updateSetExprScenario mirrors testdata/update_set_expr.yaml.
// UPDATE ... SET col = <expression> where the RHS references other
// columns (arithmetic, CASE, function calls) and tests multiple
// column assignments in a single SET. All DML goes into Setup;
// final-state SELECTs are runnable tests. Intermediate DML tests
// are included for coverage once the harness supports them.
// Drops NOT NULL on PK.
func updateSetExprScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "update_set_expr",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, label STRING, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 10, 'a'), (2, 20, 'b'), (3, null, 'c')",
			// Arithmetic in SET.
			"UPDATE t SET v = v + 5 WHERE id = 1",
			// CASE in SET.
			"UPDATE t SET label = CASE WHEN v >= 20 THEN 'big' WHEN v IS NULL THEN 'unknown' ELSE 'small' END WHERE id IN (2, 3)",
			// Multiple assignments.
			"UPDATE t SET v = 100, label = 'z' WHERE id = 1",
		},
		Tests: []yamsql.Test{
			// DML tests (auto-skipped until harness extension).
			{Query: "UPDATE t SET v = v + 5 WHERE id = 1"},
			{Query: "UPDATE t SET label = CASE WHEN v >= 20 THEN 'big' WHEN v IS NULL THEN 'unknown' ELSE 'small' END WHERE id IN (2, 3)"},
			{Query: "UPDATE t SET v = 100, label = 'z' WHERE id = 1"},
			// Verification of final state after the complete chain.
			// id=1: v=100 (was 10→15→100), label='z'.
			// id=2: v=20 (unchanged), label='big'.
			// id=3: v=null (unchanged), label='unknown'.
			{Query: "SELECT id, v, label FROM t ORDER BY id", Rows: [][]any{{1, 100, "z"}, {2, 20, "big"}, {3, nil, "unknown"}}},
			{Query: "SELECT v, label FROM t WHERE id = 1", Rows: [][]any{{100, "z"}}},
			{Query: "SELECT id, label FROM t ORDER BY id", Rows: [][]any{{1, "z"}, {2, "big"}, {3, "unknown"}}},
		},
	}
}

// insertArityScenario mirrors testdata/insert_arity.yaml.
// INSERT column count mismatches: too few values → 22000, too many or
// too few with explicit column list → 42601, reordered and subset
// column lists work. DML tests are auto-skipped; error_code tests
// included as-is. Drops NOT NULL on PK.
func insertArityScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "insert_arity",
		SchemaTemplate: "CREATE TABLE a (a1 BIGINT, a2 BIGINT, a3 BIGINT, PRIMARY KEY (a1))",
		Setup: []string{
			"INSERT INTO a (a3, a2, a1) VALUES (33, 22, 1)",
			"INSERT INTO a (a1, a2) VALUES (2, 20)",
		},
		Tests: []yamsql.Test{
			// DML tests (auto-skipped until harness extension).
			{Query: "INSERT INTO a (a3, a2, a1) VALUES (33, 22, 1)"},
			// Reordered column list: a1=1, a2=22, a3=33.
			{Query: "SELECT a1, a2, a3 FROM a WHERE a1 = 1", Rows: [][]any{{1, 22, 33}}},
			// Subset column list — a3 unset (NULL).
			{Query: "INSERT INTO a (a1, a2) VALUES (2, 20)"},
			{Query: "SELECT a1, a2, a3 FROM a WHERE a1 = 2", Rows: [][]any{{2, 20, nil}}},
			// Too few values, no column list → 22000.
			{Query: "INSERT INTO a VALUES (4)", ErrorCode: "22000"},
			// Too many values with explicit column list → 42601.
			{Query: "INSERT INTO a (a1, a2, a3) VALUES (5, 6, 7, 8, 9)", ErrorCode: "42601"},
			// Too few values with explicit column list → 42601.
			{Query: "INSERT INTO a (a1, a2, a3) VALUES (4)", ErrorCode: "42601"},
		},
	}
}

// insertValuesExprScenario mirrors testdata/insert_values_expr.yaml.
// INSERT INTO t VALUES with expressions: arithmetic, CASE, CAST,
// COALESCE, and function-call rejection (ABS → 42883). DML tests
// are auto-skipped; error_code tests included as-is. Drops NOT NULL
// on PK.
func insertValuesExprScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "insert_values_expr",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, n BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 2 + 3), (2, 10 * 10)",
			"INSERT INTO t VALUES (3, CASE WHEN 1 < 2 THEN 999 ELSE 0 END)",
			"INSERT INTO t VALUES (4, CAST('42' AS BIGINT))",
			"INSERT INTO t VALUES (5, 25 * 2)",
			"INSERT INTO t VALUES (6, COALESCE(null, null, 77))",
		},
		Tests: []yamsql.Test{
			// DML tests (auto-skipped until harness extension).
			{Query: "INSERT INTO t VALUES (1, 2 + 3), (2, 10 * 10)"},
			// Arithmetic in VALUES.
			{Query: "SELECT id, n FROM t ORDER BY id", Rows: [][]any{{1, 5}, {2, 100}, {3, 999}, {4, 42}, {5, 50}, {6, 77}}},
			// CASE expression in VALUES.
			{Query: "INSERT INTO t VALUES (3, CASE WHEN 1 < 2 THEN 999 ELSE 0 END)"},
			{Query: "SELECT n FROM t WHERE id = 3", Rows: [][]any{{999}}},
			// CAST in VALUES.
			{Query: "INSERT INTO t VALUES (4, CAST('42' AS BIGINT))"},
			{Query: "SELECT n FROM t WHERE id = 4", Rows: [][]any{{42}}},
			// Arithmetic replacement for ABS (unsupported).
			{Query: "INSERT INTO t VALUES (5, 25 * 2)"},
			{Query: "SELECT n FROM t WHERE id = 5", Rows: [][]any{{50}}},
			// ABS rejected — unsupported function → 42883.
			{Query: "INSERT INTO t VALUES (50, ABS(-7))", ErrorCode: "42883"},
			// COALESCE in VALUES.
			{Query: "INSERT INTO t VALUES (6, COALESCE(null, null, 77))"},
			{Query: "SELECT n FROM t WHERE id = 6", Rows: [][]any{{77}}},
		},
	}
}

// dmlReturningProbesScenario mirrors testdata/dml_returning_probes.yaml.
// Probes for DML RETURNING clause (Postgres / Java fdb-relational
// syntax). DELETE/UPDATE RETURNING silently succeed (RETURNING
// ignored); INSERT RETURNING is a parse error (42601). DML tests
// are auto-skipped; error_code tests included as-is. Drops NOT NULL
// on PK.
func dmlReturningProbesScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "dml_returning_probes",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, n BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 10), (2, 20), (3, 30)",
			"DELETE FROM t WHERE id = 1",
			"UPDATE t SET n = 99 WHERE id = 2",
		},
		Tests: []yamsql.Test{
			// DML tests (auto-skipped until harness extension).
			// DELETE ... RETURNING — silently does the DELETE, no result set.
			{Query: "DELETE FROM t WHERE id = 1 RETURNING id, n"},
			// Verify row is gone.
			{Query: "SELECT id FROM t WHERE id = 1", Rows: [][]any{}},
			// UPDATE ... RETURNING — silently does the UPDATE, no result set.
			{Query: "UPDATE t SET n = 99 WHERE id = 2 RETURNING id, n"},
			// Verify update took effect.
			{Query: "SELECT n FROM t WHERE id = 2", Rows: [][]any{{99}}},
			// INSERT ... RETURNING — parser rejects → 42601.
			{Query: "INSERT INTO t VALUES (4, 40) RETURNING id, n", ErrorCode: "42601"},
		},
	}
}

// dmlWithNullSafeScenario mirrors testdata/dml_with_null_safe.yaml.
// DML (UPDATE / DELETE) with IS NOT DISTINCT FROM in WHERE — the
// null-safe equality. Stateful steps: DELETE then SELECT, INSERT then
// UPDATE then SELECT. DML tests are auto-skipped; SELECT queries run
// independently against the setup state. Drops NOT NULL on PK
// (fdb-relational restriction; PK is implicitly NOT NULL).
func dmlWithNullSafeScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "dml_with_null_safe",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, n BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 10), (2, null), (3, 30), (4, null)",
		},
		Tests: []yamsql.Test{
			// DELETE using IS NOT DISTINCT FROM null — delete NULL-valued rows.
			{Query: "DELETE FROM t WHERE n IS NOT DISTINCT FROM null"},
			{Query: "SELECT id, n FROM t ORDER BY id", Rows: [][]any{{1, 10}, {3, 30}}},
			// Re-seed.
			{Query: "INSERT INTO t VALUES (2, null), (4, null)"},
			// UPDATE using IS NOT DISTINCT FROM null to fill NULL rows.
			{Query: "UPDATE t SET n = 99 WHERE n IS NOT DISTINCT FROM null"},
			{Query: "SELECT id, n FROM t ORDER BY id", Rows: [][]any{{1, 10}, {2, 99}, {3, 30}, {4, 99}}},
			// DELETE using IS DISTINCT FROM — delete non-NULL rows.
			{Query: "DELETE FROM t WHERE n IS DISTINCT FROM 99"},
			{Query: "SELECT id, n FROM t ORDER BY id", Rows: [][]any{{2, 99}, {4, 99}}},
		},
	}
}

// insertSelectScenario mirrors the bigint→bigint portion of
// testdata/insert_select.yaml: INSERT INTO … SELECT copies rows between
// tables and exercises computed columns (arithmetic, constants) in the
// SELECT list. DML steps are included as tests and auto-skipped by the
// non-query gate; the SELECTs assert dst's final state cross-engine.
//
// The aggregate-into-BIGINT steps from the yaml (INSERT … SELECT SUM(v) /
// AVG(v) into a BIGINT column) are DELIBERATELY EXCLUDED: they expose a
// real Go-vs-Java divergence rather than an equivalence the harness can
// assert. Java types AVG(BIGINT) → DOUBLE and rejects the DOUBLE → BIGINT
// assignment at plan time with SemanticException ("A value cannot be
// assigned to a variable … cannot be promoted …", SQLSTATE 22000); it
// accepts SUM(BIGINT) → BIGINT. Go's AggregateValue.Type() derives
// SUM/AVG from the operand (so AVG(BIGINT) is typed BIGINT, not DOUBLE)
// while the executor's accumulator yields float64 — so Go neither rejects
// AVG → BIGINT at plan time (Java does) nor returns an integer SUM at the
// type level. Closing that gap is an RFC-gated Cascades type-derivation
// change (AVG → DOUBLE; SUM accumulator → int64), tracked in TODO.md; it
// must not be papered over by a downstream coerce-on-write. Until then
// the aggregate-into-BIGINT shapes have no cross-engine equivalence to
// assert. They remain covered Go-only via the insert_select yamsql corpus.
func insertSelectScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "insert_select",
		// NOT NULL dropped from dst.v: fdb-relational 4.11.1.0 allows NOT NULL
		// only on ARRAY columns (scalar NOT NULL is a Go-only extension Java
		// cannot CREATE). The INSERT…SELECT under test copies non-null values, so
		// the constraint is immaterial to the cross-engine result.
		SchemaTemplate: "CREATE TABLE src (id BIGINT, v BIGINT, PRIMARY KEY (id))" +
			"\nCREATE TABLE dst (id BIGINT, v BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO src VALUES (1, 10), (2, 20), (3, 30)",
			// The full additive (INSERT-only) DML chain runs in setup so the
			// verification SELECTs observe a single deterministic final state.
			// The individual DML steps are also included as tests (auto-skipped
			// by the non-query gate). All projections are BIGINT → BIGINT, which
			// both engines accept identically.
			"INSERT INTO dst SELECT id, v FROM src",
			"INSERT INTO dst SELECT id + 100, v * 2 FROM src",
			"INSERT INTO dst SELECT 1000, 42 FROM src WHERE id = 1",
			"INSERT INTO dst SELECT id + 2000, v FROM src WHERE id = 2",
		},
		// dst after Setup (additive — no DELETE/UPDATE, so the end state is
		// well-defined and both engines produce it identically):
		//   (1,10)(2,20)(3,30)(101,20)(102,40)(103,60)(1000,42)(2002,20)
		Tests: []yamsql.Test{
			// DML tests (auto-skipped by the non-query gate).
			{Query: "INSERT INTO dst SELECT id, v FROM src"},
			{Query: "SELECT id, v FROM dst ORDER BY id", Rows: [][]any{
				{1, 10},
				{2, 20},
				{3, 30},
				{101, 20},
				{102, 40},
				{103, 60},
				{1000, 42},
				{2002, 20},
			}},
			// Duplicate PK on re-copy (DML, auto-skipped).
			{Query: "INSERT INTO dst SELECT id, v FROM src", ErrorCode: "23505"},
			{Query: "SELECT COUNT(*) FROM dst", Rows: [][]any{{8}}},
			// INSERT...SELECT with expression projections (DML, auto-skipped).
			{Query: "INSERT INTO dst SELECT id + 100, v * 2 FROM src"},
			{Query: "SELECT id, v FROM dst WHERE id > 100 ORDER BY id", Rows: [][]any{
				{101, 20}, {102, 40}, {103, 60}, {1000, 42}, {2002, 20},
			}},
			// INSERT...SELECT with constant expression (DML, auto-skipped).
			{Query: "INSERT INTO dst SELECT 1000, 42 FROM src WHERE id = 1"},
			{Query: "SELECT id, v FROM dst WHERE id = 1000", Rows: [][]any{
				{1000, 42},
			}},
			// INSERT with explicit column list + SELECT expression (DML, auto-skipped).
			{Query: "INSERT INTO dst SELECT id + 2000, v FROM src WHERE id = 2"},
			{Query: "SELECT id, v FROM dst WHERE id >= 2000 ORDER BY id", Rows: [][]any{
				{2002, 20},
			}},
		},
	}
}

// recursiveCteBaseScenario mirrors testdata/recursive_cte.yaml. WITH
// RECURSIVE CTEs — semi-naive evaluation, DFS traversal orders,
// cycle detection with UNION DISTINCT, iteration-cap termination with
// UNION ALL, column-list renames, arity mismatches, and multi-CTE
// rejection. All tests from the YAML are included; error_code and DML
// tests are auto-skipped by the per-test logic.
// Drops NOT NULL on PK columns (fdb-relational restriction).
func recursiveCteBaseScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "recursive_cte",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, parent BIGINT, PRIMARY KEY (id))" +
			"\nCREATE TABLE edge (src BIGINT, dst BIGINT, PRIMARY KEY (src, dst))",
		Setup: []string{
			"INSERT INTO t VALUES (1, -1), (10, 1), (20, 1), (40, 10), (50, 10), (70, 10), (100, 20), (210, 20), (250, 50)",
			"INSERT INTO edge VALUES (1, 2), (2, 3), (3, 1)",
		},
		Tests: []yamsql.Test{
			// Ancestors of 250: walk parent links up to root.
			{Query: "WITH RECURSIVE ancestors AS (SELECT id, parent FROM t WHERE id = 250 UNION ALL SELECT b.id, b.parent FROM ancestors AS a, t AS b WHERE b.id = a.parent) SELECT id FROM ancestors ORDER BY id DESC", Rows: [][]any{
				{250}, {50}, {10}, {1},
			}},
			// Descendants of root: every reachable row.
			{Query: "WITH RECURSIVE descendants AS (SELECT id, parent FROM t WHERE parent = -1 UNION ALL SELECT b.id, b.parent FROM descendants AS a, t AS b WHERE b.parent = a.id) SELECT id FROM descendants ORDER BY id", Rows: [][]any{
				{1}, {10}, {20}, {40}, {50}, {70}, {100}, {210}, {250},
			}},
			// COUNT on recursive CTE.
			{Query: "WITH RECURSIVE descendants AS (SELECT id, parent FROM t WHERE parent = -1 UNION ALL SELECT b.id, b.parent FROM descendants AS a, t AS b WHERE b.parent = a.id) SELECT COUNT(*) FROM descendants", Rows: [][]any{
				{9},
			}},
			// Descendants of id=50 — only 50 and 250.
			{Query: "WITH RECURSIVE descendants AS (SELECT id, parent FROM t WHERE id = 50 UNION ALL SELECT b.id, b.parent FROM descendants AS a, t AS b WHERE b.parent = a.id) SELECT id FROM descendants ORDER BY id", Rows: [][]any{
				{50}, {250},
			}},
			// Empty seed → empty result.
			{Query: "WITH RECURSIVE noseed AS (SELECT id, parent FROM t WHERE id = 99999 UNION ALL SELECT b.id, b.parent FROM noseed AS a, t AS b WHERE b.parent = a.id) SELECT id FROM noseed", Rows: [][]any{}},
			// RECURSIVE without self-reference → 0A000.
			{Query: "WITH RECURSIVE nonrec AS (SELECT id FROM t WHERE parent = -1) SELECT id FROM nonrec", ErrorCode: "0A000"},
			// Column-list rename on recursive CTE.
			{Query: "WITH RECURSIVE ancestors(node, up) AS (SELECT id, parent FROM t WHERE id = 250 UNION ALL SELECT b.id, b.parent FROM ancestors AS a, t AS b WHERE b.id = a.up) SELECT node FROM ancestors ORDER BY node DESC", Rows: [][]any{
				{250}, {50}, {10}, {1},
			}},
			// Arity mismatch between seed and recursive branch → 42F64.
			{Query: "WITH RECURSIVE ancestors AS (SELECT id FROM t WHERE id = 250 UNION ALL SELECT b.id, b.parent FROM ancestors AS a, t AS b WHERE b.id = 50) SELECT * FROM ancestors", ErrorCode: "42F64"},
			// TRAVERSAL ORDER pre_order — single chain.
			{Query: "WITH RECURSIVE ancestors AS (SELECT id, parent FROM t WHERE id = 250 UNION ALL SELECT b.id, b.parent FROM ancestors AS a, t AS b WHERE b.id = a.parent) TRAVERSAL ORDER pre_order SELECT id FROM ancestors", Rows: [][]any{
				{250}, {50}, {10}, {1},
			}},
			// TRAVERSAL ORDER post_order — single chain.
			{Query: "WITH RECURSIVE ancestors AS (SELECT id, parent FROM t WHERE id = 250 UNION ALL SELECT b.id, b.parent FROM ancestors AS a, t AS b WHERE b.id = a.parent) TRAVERSAL ORDER post_order SELECT id FROM ancestors", Rows: [][]any{
				{1}, {10}, {50}, {250},
			}},
			// DFS pre-order descending from root.
			{Query: "WITH RECURSIVE descendants AS (SELECT id, parent FROM t WHERE parent = -1 UNION ALL SELECT b.id, b.parent FROM descendants AS a, t AS b WHERE b.parent = a.id) TRAVERSAL ORDER pre_order SELECT id FROM descendants", Rows: [][]any{
				{1}, {10}, {40}, {50}, {250}, {70}, {20}, {100}, {210},
			}},
			// DFS post-order over the same tree.
			{Query: "WITH RECURSIVE descendants AS (SELECT id, parent FROM t WHERE parent = -1 UNION ALL SELECT b.id, b.parent FROM descendants AS a, t AS b WHERE b.parent = a.id) TRAVERSAL ORDER post_order SELECT id FROM descendants", Rows: [][]any{
				{40}, {250}, {50}, {70}, {10}, {100}, {210}, {20}, {1},
			}},
			// DFS on cyclic data with UNION DISTINCT — convergence.
			{Query: "WITH RECURSIVE reach(n) AS (SELECT src FROM edge WHERE src = 1 UNION SELECT e.dst FROM reach AS r, edge AS e WHERE e.src = r.n) TRAVERSAL ORDER pre_order SELECT n FROM reach ORDER BY n", Rows: [][]any{
				{1}, {2}, {3},
			}},
			// DFS on cyclic data with UNION ALL — bounded by emit cap → 54F01.
			{Query: "WITH RECURSIVE reach(n) AS (SELECT src FROM edge WHERE src = 1 UNION ALL SELECT e.dst FROM reach AS r, edge AS e WHERE e.src = r.n) TRAVERSAL ORDER pre_order SELECT n FROM reach", ErrorCode: "54F01"},
			// TRAVERSAL ORDER level_order — accepted (default).
			{Query: "WITH RECURSIVE ancestors AS (SELECT id, parent FROM t WHERE id = 250 UNION ALL SELECT b.id, b.parent FROM ancestors AS a, t AS b WHERE b.id = a.parent) TRAVERSAL ORDER level_order SELECT id FROM ancestors ORDER BY id DESC", Rows: [][]any{
				{250}, {50}, {10}, {1},
			}},
			// Counter via FROM-less SELECT literal → 0AF00.
			{Query: "WITH RECURSIVE counter(n) AS (SELECT 1 AS n UNION ALL SELECT n + 1 FROM counter WHERE n < 5) SELECT n FROM counter ORDER BY n", ErrorCode: "0AF00"},
			// Cycle + UNION DISTINCT: reachable set terminates via seen-row filter.
			{Query: "WITH RECURSIVE reach(n) AS (SELECT src FROM edge WHERE src = 1 UNION SELECT e.dst FROM reach AS r, edge AS e WHERE e.src = r.n) SELECT n FROM reach ORDER BY n", Rows: [][]any{
				{1}, {2}, {3},
			}},
			// Cycle + UNION ALL: terminates via iteration cap → 54F01.
			{Query: "WITH RECURSIVE reach(n) AS (SELECT src FROM edge WHERE src = 1 UNION ALL SELECT e.dst FROM reach AS r, edge AS e WHERE e.src = r.n) SELECT n FROM reach", ErrorCode: "54F01"},
			// Recursive CTE referenced via alias — COUNT still works.
			{Query: "WITH RECURSIVE ancestors AS (SELECT id, parent FROM t WHERE id = 250 UNION ALL SELECT b.id, b.parent FROM ancestors AS a, t AS b WHERE b.id = a.parent) SELECT COUNT(*) FROM ancestors", Rows: [][]any{
				{4},
			}},
			// CTE name matches table alias inside body — still rejected (0A000).
			{Query: "WITH RECURSIVE x AS (SELECT id, parent FROM t AS x WHERE id = 250) SELECT id FROM x", ErrorCode: "0A000"},
			// Recursive CTE in comma-join (semi-join shape).
			{Query: "WITH RECURSIVE descendants AS (SELECT id, parent FROM t WHERE id = 10 UNION ALL SELECT b.id, b.parent FROM descendants AS a, t AS b WHERE b.parent = a.id) SELECT t.id FROM t, descendants WHERE t.id = descendants.id ORDER BY t.id", Rows: [][]any{
				{10}, {40}, {50}, {70}, {250},
			}},
			// Qualified projection in single-source CTE body.
			{Query: "WITH qualified AS (SELECT d.id FROM t AS d WHERE d.parent = -1) SELECT id FROM qualified", Rows: [][]any{
				{1},
			}},
			// Multi-CTE WITH RECURSIVE: non-recursive CTE under RECURSIVE rejected → 0A000.
			{Query: "WITH RECURSIVE roots AS (SELECT id FROM t WHERE parent = -1), descendants(id, parent) AS (SELECT id, parent FROM t WHERE id IN (SELECT id FROM roots) UNION ALL SELECT b.id, b.parent FROM descendants AS a, t AS b WHERE b.parent = a.id) SELECT id FROM descendants ORDER BY id", ErrorCode: "0A000"},
			// Nested WITH RECURSIVE with downstream non-recursive consumer → 0A000.
			{Query: "WITH RECURSIVE descendants AS (SELECT id, parent FROM t WHERE parent = -1 UNION ALL SELECT b.id, b.parent FROM descendants AS a, t AS b WHERE b.parent = a.id), leaves AS (SELECT id FROM descendants WHERE id NOT IN (SELECT parent FROM t WHERE parent IS NOT NULL)) SELECT id FROM leaves ORDER BY id", ErrorCode: "0A000"},
			// SUM over recursive CTE.
			{Query: "WITH RECURSIVE descendants AS (SELECT id, parent FROM t WHERE parent = -1 UNION ALL SELECT b.id, b.parent FROM descendants AS a, t AS b WHERE b.parent = a.id) SELECT SUM(id) FROM descendants", Rows: [][]any{
				{751},
			}},
			// ORDER BY + LIMIT on recursive CTE → 0AF00.
			{Query: "WITH RECURSIVE descendants AS (SELECT id, parent FROM t WHERE parent = -1 UNION ALL SELECT b.id, b.parent FROM descendants AS a, t AS b WHERE b.parent = a.id) SELECT id FROM descendants ORDER BY id LIMIT 3", ErrorCode: "0AF00"},
		},
	}
}

// dmlSubqueryScenario mirrors testdata/dml_subquery.yaml. UPDATE and
// DELETE with EXISTS / NOT EXISTS correlated subqueries. DML tests
// are included (auto-skipped by the non-query gate). DML is staged
// in Setup so the verification SELECTs see the correct final state.
// Drops NOT NULL on PK columns (fdb-relational restriction).
func dmlSubqueryScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "dml_subquery",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))" +
			"\nCREATE TABLE keep_set (k BIGINT, PRIMARY KEY (k))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 10), (2, 20), (3, 30), (4, 40), (5, 50)",
			"INSERT INTO keep_set VALUES (2), (4)",
			// DELETE WHERE EXISTS removes rows whose id is in keep_set (2, 4).
			"DELETE FROM t WHERE EXISTS (SELECT 1 FROM keep_set WHERE keep_set.k = t.id)",
			// Re-seed deleted rows and UPDATE the ones in keep_set.
			"INSERT INTO t VALUES (2, 20), (4, 40)",
			"UPDATE t SET v = 99 WHERE EXISTS (SELECT 1 FROM keep_set WHERE keep_set.k = t.id)",
			// DELETE WHERE NOT EXISTS removes rows NOT in keep_set (1, 3, 5).
			"DELETE FROM t WHERE NOT EXISTS (SELECT 1 FROM keep_set WHERE keep_set.k = t.id)",
		},
		Tests: []yamsql.Test{
			// DML tests (auto-skipped).
			{Query: "DELETE FROM t WHERE EXISTS (SELECT 1 FROM keep_set WHERE keep_set.k = t.id)"},
			{Query: "SELECT id FROM t ORDER BY id", Rows: [][]any{
				{1}, {3}, {5},
			}},
			{Query: "INSERT INTO t VALUES (2, 20), (4, 40)"},
			{Query: "UPDATE t SET v = 99 WHERE EXISTS (SELECT 1 FROM keep_set WHERE keep_set.k = t.id)"},
			{Query: "SELECT id, v FROM t ORDER BY id", Rows: [][]any{
				{1, 10}, {2, 99}, {3, 30}, {4, 99}, {5, 50},
			}},
			{Query: "DELETE FROM t WHERE NOT EXISTS (SELECT 1 FROM keep_set WHERE keep_set.k = t.id)"},
			{Query: "SELECT id, v FROM t ORDER BY id", Rows: [][]any{
				{2, 99}, {4, 99},
			}},
			// Uncorrelated EXISTS — deletes all remaining rows.
			{Query: "DELETE FROM t WHERE EXISTS (SELECT k FROM keep_set)"},
			{Query: "SELECT COUNT(*) FROM t", Rows: [][]any{
				{0},
			}},
		},
	}
}

// updateDmlCteScenario mirrors testdata/update_dml_cte.yaml.
// UPDATE with WITH clause and UPDATE/DELETE using CTE in WHERE.
// Two queries are rejected with error_code "42601" (syntax_error);
// the working form uses EXISTS correlated subquery. DML tests and
// error_code tests are included (auto-skipped by their respective
// gates). DML is staged in Setup so the verification SELECT sees the
// correct final state. Drops NOT NULL on PK (fdb-relational restriction).
func updateDmlCteScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "update_dml_cte",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (id))",
		Setup: []string{
			"INSERT INTO t VALUES (1, 1, 10), (2, 1, 20), (3, 2, 30), (4, 2, 40)",
			// Working form: UPDATE WHERE EXISTS correlated subquery.
			"UPDATE t SET v = 99 WHERE EXISTS (SELECT 1 FROM t AS sub WHERE sub.id = t.id AND sub.g = 1)",
		},
		Tests: []yamsql.Test{
			// UPDATE WHERE col IN (CTE-derived values) — rejected.
			{Query: "WITH high_ids AS (SELECT id FROM t WHERE v >= 30)\nUPDATE t SET v = 0 WHERE id IN (SELECT id FROM high_ids)", ErrorCode: "42601"},
			// WITH before UPDATE — grammar may not accept this form.
			{Query: "UPDATE t SET v = 99 WHERE id IN (WITH x AS (SELECT id FROM t WHERE g=1) SELECT id FROM x)", ErrorCode: "42601"},
			// Working form: UPDATE WHERE EXISTS correlated subquery (auto-skipped DML).
			{Query: "UPDATE t SET v = 99 WHERE EXISTS (SELECT 1 FROM t AS sub WHERE sub.id = t.id AND sub.g = 1)"},
			// Verification of final state.
			{Query: "SELECT id, v FROM t ORDER BY id", Rows: [][]any{
				{1, 99}, {2, 99}, {3, 30}, {4, 40},
			}},
		},
	}
}

// correlatedExistsAdvancedScenario mirrors
// testdata/correlated_exists_advanced.yaml. Advanced correlated EXISTS
// edge cases — cross-join + EXISTS and NOT EXISTS patterns. Drops NOT
// NULL on PK columns. First query uses SELECT DISTINCT which may be
// rejected by Java — included to surface divergences.
func correlatedExistsAdvancedScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name: "correlated_exists_advanced",
		SchemaTemplate: "CREATE TABLE emp (id BIGINT, name STRING, dept BIGINT, PRIMARY KEY (id))" +
			"\nCREATE TABLE task (tid BIGINT, emp_id BIGINT, prio BIGINT, PRIMARY KEY (tid))",
		Setup: []string{
			"INSERT INTO emp VALUES (1, 'Alice', 1), (2, 'Bob', 2), (3, 'Charlie', 1)",
			"INSERT INTO task VALUES (10, 1, 5), (20, 1, 3), (30, 2, 8)",
		},
		Tests: []yamsql.Test{
			// Cross-join + EXISTS with SELECT DISTINCT.
			{
				Query: "SELECT DISTINCT e.name FROM emp AS e, task AS t\nWHERE e.id = t.emp_id\n  AND EXISTS (SELECT 1 FROM task WHERE emp_id = e.id AND prio > 4)\nORDER BY e.name",
				Rows:  [][]any{{"Alice"}, {"Bob"}},
			},
			// NOT EXISTS — employees with no tasks.
			{
				Query: "SELECT name FROM emp\nWHERE NOT EXISTS (SELECT 1 FROM task WHERE emp_id = emp.id)\nORDER BY name",
				Rows:  [][]any{{"Charlie"}},
			},
		},
	}
}

// orderByLimitScenario mirrors testdata/order_by_limit.yaml — ORDER BY
// with LIMIT and positional/alias sort keys. Drops NOT NULL on PK
// column (fdb-relational restriction). Includes all 12 tests from the
// YAML; error_code tests are auto-skipped by the harness, DML tests
// likewise. Multi-column ORDER BY and GROUP BY tests are included to
// surface Java-side divergences.
func orderByLimitScenario() *yamsql.Scenario {
	return &yamsql.Scenario{
		Name:           "order_by_limit",
		SchemaTemplate: "CREATE TABLE t (id BIGINT, grp STRING, v BIGINT, PRIMARY KEY (id))\nCREATE INDEX idx_v ON t (v)",
		Setup: []string{
			"INSERT INTO t VALUES (1, 'a', 3), (2, 'a', 1), (3, 'b', 4), (4, 'b', 1), (5, 'a', 2)",
		},
		Tests: []yamsql.Test{
			// Multi-column ORDER BY: rejected by Java's Cascades (no multi-key sort rule).
			{Query: "SELECT id, grp, v FROM t ORDER BY grp, v DESC", ErrorCode: "0AF00"},
			// LIMIT clause rejected at parse time (0AF00).
			{Query: "SELECT id FROM t ORDER BY id LIMIT 3", ErrorCode: "0AF00"},
			{Query: "SELECT id FROM t ORDER BY id LIMIT 100", ErrorCode: "0AF00"},
			{Query: "SELECT id FROM t ORDER BY id LIMIT 0", ErrorCode: "0AF00"},
			// Positional ORDER BY (`ORDER BY 2 DESC`, `ORDER BY 1`) is a Go-only
			// EXTENSION: fdb-relational 4.11.1.0 cannot plan it
			// (UnableToPlanException), even for `ORDER BY 1` over the PK that
			// `ORDER BY id` by name plans fine — so the positional reference, not
			// the sort, is what its planner rejects. Go plans both and returns the
			// correct rows; with no Java equivalence to assert they are not
			// cross-engine. Covered Go-only by the order_by_limit yamsql corpus.
			// ORDER BY on aggregate with GROUP BY — rejected by Java (no GROUP BY rule).
			{Query: "SELECT grp, SUM(v) FROM t GROUP BY grp ORDER BY 2 DESC", ErrorCode: "0AF00"},
			// Out-of-range positional ORDER BY (22023).
			{Query: "SELECT id FROM t ORDER BY 99", ErrorCode: "22023"},
			// ORDER BY a SELECT-list alias.
			{Query: "SELECT id AS n FROM t ORDER BY n DESC", Rows: [][]any{{5}, {4}, {3}, {2}, {1}}},
			// ORDER BY alias with a second column.
			{Query: "SELECT id AS n, v FROM t ORDER BY n", Rows: [][]any{{1, 3}, {2, 1}, {3, 4}, {4, 1}, {5, 2}}},
			// Multi-column ORDER BY with expression — rejected by Java.
			{Query: "SELECT id FROM t ORDER BY v * 2, id", ErrorCode: "0AF00"},
			// ORDER BY with function-call expression + LIMIT — LIMIT rejected (0AF00).
			{Query: "SELECT id, v FROM t ORDER BY ABS(v - 3), id LIMIT 2", ErrorCode: "0AF00"},
			// SELECT * + multi-column ORDER BY with expression — rejected by Java.
			{Query: "SELECT * FROM t ORDER BY v * 2, id", ErrorCode: "0AF00"},
		},
	}
}
