package embedded

import (
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// The planner-level pins for three facts about a FROM item that each engine
// decides the same way, measured against the Java target in
// conformance/ws_f_table_qualifier_conformance_test.go and
// conformance/ws_f_join_unnest_conformance_test.go:
//
//   - a name the statement authored is resolved exactly;
//   - an AT is refused only where Java's generateAccess refuses it;
//   - a block's first FROM item over an enclosing query's array joins the
//     block's other sources.
//
// Each arm states the SQLSTATE the target answers, or that it plans.

const fromItemSchema = `
CREATE TYPE AS STRUCT na(arr BIGINT ARRAY)
CREATE TYPE AS STRUCT b(k BIGINT, tags BIGINT ARRAY)
CREATE TABLE w(id BIGINT, f BIGINT, arr BIGINT ARRAY, "keep" BIGINT, PRIMARY KEY(id))
CREATE TABLE h(id BIGINT, f BIGINT, PRIMARY KEY(id))
CREATE TABLE t2(id BIGINT, n na, PRIMARY KEY(id))
CREATE TABLE q(id BIGINT, bs b ARRAY, PRIMARY KEY(id))
CREATE TYPE AS STRUCT s(k BIGINT, g STRING)
CREATE TABLE kk(id BIGINT, k BIGINT, items s ARRAY, PRIMARY KEY(id))
CREATE TYPE AS STRUCT c(k BIGINT, bs b ARRAY)
CREATE TABLE qq(id BIGINT, cs c ARRAY, PRIMARY KEY(id))`

type fromItemCase struct {
	sql string
	// code is the SQLSTATE the query must fail with; "" means it plans.
	code api.ErrorCode
	// contains, when set, must appear in the plan.
	contains string
	// result, when set, is the plan's root result value (explainWithResult).
	result string
}

func runFromItemCases(t *testing.T, cases []fromItemCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			t.Parallel()
			plan, err := PlanQueryForTest(tc.sql, fromItemSchema, nil)
			if tc.code == "" {
				if err != nil {
					t.Fatalf("want a plan, got %v", err)
				}
				if tc.contains != "" && !strings.Contains(plan, tc.contains) {
					t.Fatalf("plan lacks %q:\n%s", tc.contains, plan)
				}
				if tc.result != "" {
					if got := explainWithResult(t, tc.sql, fromItemSchema); !strings.HasSuffix(got, " => "+tc.result) {
						t.Fatalf("plan result is not %s:\n%s", tc.result, got)
					}
				}
				return
			}
			var apiErr *api.Error
			if !errors.As(err, &apiErr) || apiErr.Code != tc.code {
				t.Fatalf("got %v (plan %q), want SQLSTATE %s", err, plan, tc.code)
			}
		})
	}
}

// A column whose name the SQL text wrote — an unnest's AS or AT alias, a
// derived table's or CTE's `AS`, a CTE column list, or a column that passes
// such a name through — is compared exactly, as Java compares every name. The
// relaxed pass folds only a descriptor's spelling (`keep` declared quoted
// lower-case), including one passed through a derived table or CTE.
func TestSQLAuthoredNamesResolveExactly(t *testing.T) {
	t.Parallel()
	runFromItemCases(t, []fromItemCase{
		{sql: `SELECT w.id FROM w WHERE EXISTS (SELECT E FROM w.arr AS "e" WHERE "e" = 20)`, code: api.ErrCodeUndefinedColumn},
		{sql: `SELECT w.id FROM w, w.arr AS "e" WHERE E = 20`, code: api.ErrCodeUndefinedColumn},
		{sql: `SELECT w.id FROM w, w.arr AS "e" AT "o" WHERE O = 1`, code: api.ErrCodeUndefinedColumn},
		{sql: `SELECT d.K FROM (SELECT id AS "k" FROM w) AS d`, code: api.ErrCodeUndefinedColumn},
		{sql: `SELECT K FROM (SELECT id AS "k" FROM w) AS d`, code: api.ErrCodeUndefinedColumn},
		{sql: `WITH c("k") AS (SELECT id FROM w) SELECT K FROM c`, code: api.ErrCodeUndefinedColumn},
		{sql: `WITH c AS (SELECT id AS "k" FROM w) SELECT K FROM c`, code: api.ErrCodeUndefinedColumn},
		{sql: `SELECT K FROM (SELECT COUNT(*) AS "k" FROM w) AS d`, code: api.ErrCodeUndefinedColumn},
		{sql: `SELECT d.K FROM (SELECT v AS "k" FROM w, w.arr AS v) AS d`, code: api.ErrCodeUndefinedColumn},
		{sql: `SELECT E FROM (SELECT "e" FROM w, w.arr AS "e") AS d`, code: api.ErrCodeUndefinedColumn},
		// Aggregate blocks record every slot too: an alias over arithmetic on
		// aggregates, and a grouped pass-through of an authored name.
		{sql: `SELECT S FROM (SELECT SUM(id) + 1 AS "s" FROM w) AS d`, code: api.ErrCodeUndefinedColumn},
		{sql: `SELECT K FROM (SELECT "k", COUNT(*) AS n FROM (SELECT id AS "k" FROM w) AS x GROUP BY "k") AS d`, code: api.ErrCodeUndefinedColumn},
		{sql: `SELECT "s" FROM (SELECT SUM(id) + 1 AS "s" FROM w) AS d`},
		// The exact spellings answer.
		{sql: `SELECT w.id FROM w, w.arr AS "e" WHERE "e" = 20`},
		{sql: `SELECT "k" FROM (SELECT id AS "k" FROM w) AS d`},
		{sql: `WITH c("k") AS (SELECT id FROM w) SELECT "k" FROM c`},
		{sql: `SELECT w.id FROM w, w.arr AS e WHERE e = 20`},
		// A descriptor's spelling keeps the Go-only fold, through a derived
		// table and a CTE too.
		{sql: `SELECT keep FROM w`},
		{sql: `SELECT keep FROM (SELECT keep FROM w) AS d`},
		{sql: `SELECT d.keep FROM (SELECT "keep" FROM w) AS d`},
		{sql: `WITH c AS (SELECT keep FROM w) SELECT keep FROM c`},
	})
}

// Java's generateAccess refuses an AT only in its CTE, table, view and
// function branches (WRONG_OBJECT_TYPE); an unqualified name that is none of
// them is an unknown table (UNDEFINED_TABLE); a correlated path takes the AT
// when it is an array and is INVALID_COLUMN_REFERENCE otherwise, AT or not;
// a block's first FROM item that is no correlated path fails as it does
// without the AT.
func TestFromItemATIsDecidedWhereJavaDecidesIt(t *testing.T) {
	t.Parallel()
	runFromItemCases(t, []fromItemCase{
		{sql: `SELECT x, p FROM t2, n.arr AS x AT p`, contains: "WITH ORDINALITY"},
		{sql: `SELECT id FROM w WHERE EXISTS (SELECT 1 FROM h, w.arr AS v AT p WHERE p = 2)`, contains: "WITH ORDINALITY"},
		{sql: `SELECT id FROM w WHERE EXISTS (SELECT 1 FROM w.arr AS v AT p WHERE p = 2)`, contains: "WITH ORDINALITY"},
		{sql: `SELECT id FROM w WHERE EXISTS (SELECT p FROM w.arr AT p)`, contains: "WITH ORDINALITY"},
		{sql: `SELECT d.k, d.p FROM w, (SELECT v AS k, p FROM w.arr AS v AT p) AS d`, contains: "WITH ORDINALITY"},
		{sql: `SELECT x FROM w, w.f AS x AT p`, code: api.ErrCodeInvalidColumnReference},
		{sql: `SELECT x FROM w, w.f AS x`, code: api.ErrCodeInvalidColumnReference},
		{sql: `SELECT 1 FROM w, nosuch AT p`, code: api.ErrCodeUndefinedTable},
		{sql: `SELECT 1 FROM w, h AT p`, code: api.ErrCodeWrongObjectType},
		{sql: `SELECT 1 FROM w, w AT p`, code: api.ErrCodeWrongObjectType},
		{sql: `SELECT 1 FROM w AT p`, code: api.ErrCodeWrongObjectType},
		{sql: `WITH c AS (SELECT id FROM w) SELECT 1 FROM c AT p`, code: api.ErrCodeWrongObjectType},
		{sql: `SELECT h.id FROM w LEFT JOIN h AT p ON 1 = 1`, code: api.ErrCodeWrongObjectType},
		{sql: `SELECT v FROM w.arr AS v AT p`, code: api.ErrCodeUndefinedColumn},
		{sql: `SELECT v FROM w.arr AS v`, code: api.ErrCodeUndefinedColumn},
		// A first item under an enclosing block: only a dotted non-table path
		// is correlated; a single name is a CTE (a WITH CTE or an enclosing
		// block's operator), a table, or an unknown table.
		{sql: `SELECT id FROM w WHERE EXISTS (SELECT p FROM arr AT p)`, code: api.ErrCodeUndefinedTable},
		{sql: `SELECT d.p FROM w, (SELECT p FROM arr AT p) AS d`, code: api.ErrCodeUndefinedTable},
		{sql: `SELECT id FROM w WHERE EXISTS (SELECT 1 FROM h AT p)`, code: api.ErrCodeWrongObjectType},
		{sql: `SELECT id FROM w WHERE EXISTS (SELECT 1 FROM w AT p)`, code: api.ErrCodeWrongObjectType},
		{sql: `SELECT id FROM w WHERE EXISTS (SELECT 1 FROM nosuch AT p)`, code: api.ErrCodeUndefinedTable},
		{sql: `WITH c AS (SELECT id FROM h) SELECT id FROM w WHERE EXISTS (SELECT 1 FROM c AT p)`, code: api.ErrCodeWrongObjectType},
		{sql: `WITH c AS (SELECT id FROM h) SELECT 1 FROM w, c AT p`, code: api.ErrCodeWrongObjectType},
		{sql: `SELECT id FROM w WHERE EXISTS (SELECT 1 FROM h, w AT p)`, code: api.ErrCodeWrongObjectType},
	})
}

// A FROM item's correlated path is looked up over the operators of every
// level as one list, qualified readings first: a qualified reading at one
// level is not ambiguous with a struct-relative one at another.
func TestFromItemPathQualifiedReadingWinsAcrossLevels(t *testing.T) {
	t.Parallel()
	runFromItemCases(t, []fromItemCase{
		{sql: `SELECT id FROM t2 WHERE EXISTS (SELECT 1 FROM w AS n, n.arr AS x WHERE x = 20)`, contains: "Explode"},
		{sql: `SELECT id FROM w AS n WHERE EXISTS (SELECT 1 FROM t2, n.arr AS v WHERE v = 20)`, contains: "Explode"},
		{sql: `SELECT id FROM w WHERE EXISTS (SELECT 1 FROM w, w.arr AS v WHERE v = 20)`, code: api.ErrCodeAmbiguousColumn},
	})
}

// A block's first FROM item over an enclosing query's array is one more
// quantifier of the block: it joins the block's other sources, comma or
// explicit INNER JOIN (whose ON is a WHERE conjunct), with or without AT.
func TestCorrelatedPrimaryUnnestJoinsTheBlocksSources(t *testing.T) {
	t.Parallel()
	runFromItemCases(t, []fromItemCase{
		{sql: `SELECT id FROM w WHERE EXISTS (SELECT v FROM w.arr AS v, h WHERE v = h.f)`, contains: "Explode"},
		{sql: `SELECT id FROM w WHERE EXISTS (SELECT v FROM w.arr AS v JOIN h ON v = h.f)`, contains: "Explode"},
		{sql: `SELECT id FROM w WHERE EXISTS (SELECT v FROM w.arr AS v, h, w AS z WHERE v = h.f AND z.id = h.id)`, contains: "Explode"},
		{sql: `SELECT id FROM w WHERE EXISTS (SELECT v, p FROM w.arr AS v AT p, h WHERE p = h.id)`, contains: "WITH ORDINALITY"},
		{sql: `SELECT d.v, d.f FROM w, (SELECT v, h.f FROM w.arr AS v, h) AS d`, contains: "Explode"},
		{sql: `SELECT d.* FROM w, (SELECT * FROM w.arr AS v, h) AS d`, contains: "Explode"},
		// A later derived leg that reads the first item's element.
		{sql: `SELECT id FROM w WHERE EXISTS (SELECT 1 FROM w.arr AS v, (SELECT h.id FROM h WHERE h.f + 3 = v) AS d)`, contains: "Explode"},
	})
}

// A USING column is resolved on every left operator first, then on the right
// one (Java's resolveJoinUsingClause), and an unnest leg is described by its
// own operator on either side: the left owner is found by ownership, not by
// position, and a column two left sources carry is ambiguous. An item that is
// not an array path is refused as itself before any USING column, because
// Java visits the right item first.
func TestUsingBesideAnUnnestLegResolvesLeftThenRight(t *testing.T) {
	t.Parallel()
	runFromItemCases(t, []fromItemCase{
		{sql: `SELECT kk.id, i.g FROM kk JOIN h ON h.id = kk.id JOIN kk.items AS i USING (k)`, contains: "Explode"},
		{sql: `SELECT a.id, i.g FROM kk AS a JOIN kk AS b ON a.id = b.id JOIN a.items AS i USING (k)`, code: api.ErrCodeAmbiguousColumn},
		{sql: `SELECT v, h.f FROM w JOIN w.arr AS v ON 1 = 1 JOIN h USING (id)`, contains: "Explode"},
		{sql: `SELECT id FROM kk WHERE EXISTS (SELECT 1 FROM kk.items AS i JOIN kk AS z USING (k))`, contains: "Explode"},
		{sql: `SELECT k FROM kk JOIN kk.items AS i AT p USING (k)`, code: api.ErrCodeUndefinedColumn},
		{sql: `SELECT h.id FROM w JOIN h AT p USING (id)`, code: api.ErrCodeWrongObjectType},
		{sql: `SELECT a.id FROM kk AS a JOIN kk.items AS i USING (k) JOIN kk AS b USING (k)`, code: api.ErrCodeUndefinedColumn},
	})
}

// A block's first FROM item over an enclosing array is the bottom of a
// chained unnest exactly as a scan is: Java makes it the block's first ForEach
// quantifier and the next item explodes its element, so the plan is a FlatMap
// whose outer is that first Explode — for AT on either link, a fork, a
// predicate on the first element, and a third link.
func TestChainedUnnestOverAFirstFromItem(t *testing.T) {
	t.Parallel()
	const chain = "FlatMap(outer=Explode(field), inner=Explode(field))"
	// A predicate on the second element filters the inner Explode, as Java's
	// partitioning places it: `EXPLODE q.bs | FLATMAP { EXPLODE b.tags | FILTER … }`.
	const filteredChain = "FlatMap(outer=Explode(field), inner=PredicatesFilter(Explode(field), [1 preds]))"
	runFromItemCases(t, []fromItemCase{
		{sql: `SELECT id FROM q WHERE EXISTS (SELECT t FROM q.bs AS b, b.tags AS t WHERE t = 9)`, contains: filteredChain},
		{sql: `SELECT id FROM q WHERE NOT EXISTS (SELECT t FROM q.bs AS b, b.tags AS t WHERE t = q.id + 8)`, contains: filteredChain},
		{sql: `SELECT d.t FROM q, (SELECT t FROM q.bs AS b, b.tags AS t) AS d`, contains: chain},
		{sql: `SELECT d.c FROM q, (SELECT COUNT(*) AS c FROM q.bs AS b, b.tags AS t) AS d`, contains: chain},
		{
			sql:      `SELECT d.t, d.o, d.p FROM q, (SELECT t, o, p FROM q.bs AS b AT o, b.tags AS t AT p) AS d`,
			contains: "FlatMap(outer=Explode(field WITH ORDINALITY), inner=Explode(field WITH ORDINALITY))",
		},
		{
			sql:      `SELECT d.t, d.k FROM q, (SELECT t, b.k AS k FROM q.bs AS b, b.tags AS t WHERE b.k = 1) AS d`,
			contains: "FlatMap(outer=PredicatesFilter(Explode(field), [1 preds]), inner=Explode(field))",
		},
		{
			sql:      `SELECT d.t, d.u FROM q, (SELECT t, u FROM q.bs AS b, b.tags AS t, b.tags AS u) AS d`,
			contains: "FlatMap(outer=" + chain + ", inner=Explode(field))",
		},
		{
			sql:      `SELECT d.t FROM qq, (SELECT t FROM qq.cs AS c, c.bs AS b, b.tags AS t) AS d`,
			contains: "FlatMap(outer=" + chain + ", inner=Explode(field))",
		},
	})
}

// TestLateralDerivedBodyOverAnUndescribablePrefix drives the fallback in
// prepareDerivedSourceBodies: a prior source Go cannot describe (a derived
// column typed NULL — a 0A-class refusal of the prefix scope, which the target
// answers with a crash, XXXXX "should not be called", measured in the join
// spec) must not fail a later derived body on the prior source's account. The
// body is built under the enclosing scope instead: a reference it makes to the
// prior source is its own 42703 — the prefix's 0AF00 would have been returned
// without the fallback — and a body that never reads it builds, leaving the
// refusal to the prior source itself.
func TestLateralDerivedBodyOverAnUndescribablePrefix(t *testing.T) {
	t.Parallel()
	runFromItemCases(t, []fromItemCase{
		{sql: `SELECT d.x FROM (SELECT NULL AS n FROM h) AS a, (SELECT a.n AS x FROM h) AS d`, code: api.ErrCodeUndefinedColumn},
		{sql: `SELECT d.x FROM (SELECT NULL AS n FROM h) AS a, (SELECT zz AS x FROM h) AS d`, code: api.ErrCodeUndefinedColumn},
		{sql: `SELECT d.x FROM (SELECT NULL AS n FROM h) AS a, (SELECT h.f AS x FROM h) AS d`, code: api.ErrCodeUnsupportedQuery},
		{sql: `SELECT a.n FROM (SELECT NULL AS n FROM h) AS a`, code: api.ErrCodeUnsupportedQuery},
	})
}

// Lateral legs correlated to other legs of one FROM are more quantifiers of
// the block, each planned inside the leg it reads — Java's PartitionSelectRule
// reaches them through a lower that depends on an upper leg (the only
// bipartition such a block has). A LEFT JOIN's null-supplying leg read by a
// later leg plans because a leg's provided aliases include its physical
// join's own, not through that partition; a NOT EXISTS reading it with an IS
// NULL plans the per-row DefaultOnEmpty form.
func TestLateralLegsCorrelatedToOtherLegs(t *testing.T) {
	t.Parallel()
	runFromItemCases(t, []fromItemCase{
		{
			sql:      `SELECT e.k FROM w, (SELECT v AS k FROM w.arr AS v) AS d, (SELECT d.k AS k FROM h) AS e`,
			contains: "FlatMap(outer=Scan(H), inner=FlatMap(outer=Scan(W), inner=Explode(field)))",
			result:   "{K: Q$BOUND1}",
		},
		{
			sql:      `SELECT e.k FROM w, (SELECT w.f AS k FROM h) AS d, (SELECT w.f AS k FROM h AS h2) AS e`,
			contains: "NestedLoopJoin(INNER, NestedLoopJoin(INNER, Scan(H), Scan(H)), Scan(W))",
			result:   "{K: W.F#1}",
		},
		{
			sql:      `SELECT e.k FROM w, h, (SELECT w.f + h.f AS k FROM h AS h2) AS e`,
			contains: "NestedLoopJoin(INNER, Scan(H), NestedLoopJoin(INNER, Scan(H), Scan(W)))",
			result:   "{K: (W.F#1 + H.F#1)}",
		},
		// An unnest behind a later table inside a derived leg: the leg is an
		// opaque ordinal leg exactly when its body's rotation classifies it,
		// and the body then lowers the rotated cluster.
		{
			sql:      `SELECT d.b, d.f FROM w, (SELECT b.k AS b, h.f FROM q, q.bs AS b, h) AS d`,
			contains: "NestedLoopJoin(INNER, FlatMap(outer=Scan(Q), inner=Explode(field)), NestedLoopJoin(INNER, Scan(W), Scan(H)))",
			result:   "{B: Q$BOUND2.K#0, F: Q$BOUND3.F#1}",
		},
		{
			sql:      `SELECT d.t, d.f FROM q, (SELECT t, h.f FROM q.bs AS b, b.tags AS t, h) AS d`,
			contains: "FlatMap(outer=NestedLoopJoin(INNER, Explode(field), Scan(H)), inner=Explode(field))",
		},
		{
			sql:      `SELECT d.t, d.f FROM w, (SELECT t, h.f FROM q, q.bs AS b, b.tags AS t, h) AS d`,
			contains: "FlatMap(outer=NestedLoopJoin(INNER, FlatMap(outer=Scan(Q), inner=Explode(field)), Scan(H)), inner=Explode(field))",
		},
		{
			sql:      `SELECT w.id FROM w LEFT JOIN h ON h.id = w.id WHERE h.id IS NULL AND NOT EXISTS (SELECT 1 FROM q WHERE q.id = h.id)`,
			contains: "DefaultOnEmpty",
		},
		{
			// q.id = h.id rejects h's null-extended row, so the outer join is an
			// inner one (EliminateNullOnEmptyRule), as Java plans it.
			sql:      `SELECT w.id, d.x FROM w LEFT JOIN h ON h.id = w.id, (SELECT q.id AS x FROM q WHERE q.id = h.id) AS d`,
			contains: "FlatMap(outer=Scan(H), inner=FlatMap(outer=Scan(Q, [=]), inner=Scan(W, [=])))",
			result:   `{ID: $m"1._1#1.ID#0, X: $m"1._0#0.ID#0}`,
		},
	})
}
