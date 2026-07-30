package metamorphic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Supported SQL surface (verified against SimFDB by the metamorphic hunts — write scenarios and
// generator prompts against this; an unsupported construct just shows up as an "errored" query, not
// a bug, so filter it out early):
//
//	SUPPORTED: COALESCE, CASE, GREATEST, LEAST, ABS, MOD, UPPER, LOWER, TRIM, LENGTH,
//	  CHARACTER_LENGTH, LIKE / NOT LIKE, CAST(… AS DOUBLE|INTEGER), + - * /, mixed-numeric compare,
//	  IS NULL, NOT IN (value list), BETWEEN, IN (value list), EXISTS, correlated & non-correlated
//	  scalar subqueries, INNER/CROSS/3-way JOIN, derived tables, GROUP BY (column), HAVING,
//	  UNION ALL, ORDER BY <expr>, LIMIT/OFFSET.
//	UNSUPPORTED (avoid — they error, not diverge): NULLIF, `||` string concat, SUBSTRING,
//	  IN-subquery / NOT-IN-subquery, UNION (distinct — only UNION ALL), GROUP BY <ordinal>,
//	  COUNT(DISTINCT …), scalar-subquery arithmetic without FROM.
//	KNOWN BUGS (don't re-file; see TODO.md "## DST findings"): DISTINCT-under-pagination dedup drop;
//	  multi-value IN / UNION ALL 54F01 under a scan limit (fixed on #473); aggregate-index MIN/MAX
//	  null-group (fixed on #474 via permuted); NULL ordering DISTINCT+ORDER BY vs GROUP BY+ORDER BY.

// SeedCorpus is the hand-written metamorphic relations — equivalences that hold under SQL
// three-valued logic regardless of NULLs, so a correct engine returns zero violations. It gives
// the oracle immediate teeth and a template for what the LLM generator emits. Keep only relations
// you are certain are equivalent; a false relation here would be a spurious "bug".
func SeedCorpus() []Scenario {
	base := Scenario{
		Name:   "rewrites",
		Seed:   1,
		Tables: []string{"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id))"},
		Data: []string{
			"INSERT INTO t (id, a, b) VALUES (1, 1, 10)",
			"INSERT INTO t (id, a, b) VALUES (2, 2, 20)",
			"INSERT INTO t (id, a, b) VALUES (3, 3, 5)",
			"INSERT INTO t (id, a, b) VALUES (4, 2, 200)",
			"INSERT INTO t (id, a, b) VALUES (5, NULL, 30)",
			"INSERT INTO t (id, a, b) VALUES (6, 3, NULL)",
		},
		Groups: []Group{
			{
				Name: "in-vs-or", Reason: "a IN (1,2,3) ≡ a=1 OR a=2 OR a=3 (NULL-safe both sides)",
				Queries: []string{
					"SELECT id FROM t WHERE a IN (1, 2, 3)",
					"SELECT id FROM t WHERE a = 1 OR a = 2 OR a = 3",
				},
			},
			{
				Name: "and-commute", Reason: "AND is commutative",
				Queries: []string{
					"SELECT id FROM t WHERE a > 1 AND b < 100",
					"SELECT id FROM t WHERE b < 100 AND a > 1",
				},
			},
			{
				Name: "or-commute", Reason: "OR is commutative",
				Queries: []string{
					"SELECT id FROM t WHERE a = 1 OR b = 20",
					"SELECT id FROM t WHERE b = 20 OR a = 1",
				},
			},
			{
				Name: "redundant-true", Reason: "p AND (1=1) ≡ p",
				Queries: []string{
					"SELECT id FROM t WHERE a > 1",
					"SELECT id FROM t WHERE a > 1 AND (1 = 1)",
				},
			},
			{
				Name: "between-vs-range", Reason: "x BETWEEN lo AND hi ≡ x >= lo AND x <= hi",
				Queries: []string{
					"SELECT id FROM t WHERE b BETWEEN 10 AND 30",
					"SELECT id FROM t WHERE b >= 10 AND b <= 30",
				},
			},
			{
				Name: "orderby-default-asc", Reason: "ORDER BY id ≡ ORDER BY id ASC", Ordered: true,
				Queries: []string{
					"SELECT id FROM t ORDER BY id",
					"SELECT id FROM t ORDER BY id ASC",
				},
			},
			{
				Name: "coalesce-vs-case", Reason: "COALESCE(a,0) is exactly CASE WHEN a IS NULL THEN 0 ELSE a END (2-arg COALESCE definition; NULL-safe both sides)",
				Queries: []string{
					"SELECT id, COALESCE(a, 0) FROM t",
					"SELECT id, CASE WHEN a IS NULL THEN 0 ELSE a END FROM t",
				},
			},
			{
				Name: "arith-commute", Reason: "a + b ≡ b + a (integer addition is commutative; NULL propagates identically on both sides)",
				Queries: []string{
					"SELECT id FROM t WHERE a + b > 10",
					"SELECT id FROM t WHERE b + a > 10",
				},
			},
			{
				Name: "abs-idempotent", Reason: "ABS(ABS(a)) ≡ ABS(a) (ABS is idempotent; ABS(NULL)=NULL on both sides)",
				Queries: []string{
					"SELECT id, ABS(ABS(a)) FROM t",
					"SELECT id, ABS(a) FROM t",
				},
			},
			{
				Name: "mult-by-one", Reason: "a * 1 ≡ a (identity; NULL*1=NULL on both sides)",
				Queries: []string{
					"SELECT id, a * 1 FROM t",
					"SELECT id, a FROM t",
				},
			},
			{
				Name: "add-zero", Reason: "a + 0 ≡ a (identity; NULL+0=NULL on both sides)",
				Queries: []string{
					"SELECT id, a + 0 FROM t",
					"SELECT id, a FROM t",
				},
			},
			{
				Name: "single-in-vs-eq", Reason: "a IN (2) ≡ a = 2 (single-value IN degenerates to equality; both exclude NULL)",
				Queries: []string{
					"SELECT id FROM t WHERE a IN (2)",
					"SELECT id FROM t WHERE a = 2",
				},
			},
		},
	}
	return []Scenario{base, joins(), nulls(), indexed()}
}

// nulls targets three-valued-logic (SQL NULL) semantics head-on — the classic engine-divergence
// dimension (the corpus doc-comment already records one known NULL-ordering bug). Every relation is
// a Kleene-3VL identity that a NULL-correct engine must satisfy and a NULL-sloppy one breaks:
//   - `a = a` is NOT a tautology — it is UNKNOWN (row excluded) when a IS NULL, so it equals
//     `a IS NOT NULL`. An engine that folds x=x to TRUE returns the NULL rows and diverges.
//   - `a <> b` ≡ `NOT (a = b)` (both UNKNOWN, i.e. excluded, when either operand is NULL).
//   - De Morgan holds in Kleene logic: `NOT (P AND Q)` ≡ `NOT P OR NOT Q` even with UNKNOWN.
//   - `COUNT(a)` counts only non-NULL a; `SUM(a)` ignores NULL — each equals its explicit
//     `WHERE a IS NOT NULL` form.
func nulls() Scenario {
	return Scenario{
		Name:   "nulls",
		Seed:   3,
		Tables: []string{"CREATE TABLE tn (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id))"},
		Data: []string{
			"INSERT INTO tn (id, a, b) VALUES (1, 1, 1)",
			"INSERT INTO tn (id, a, b) VALUES (2, 2, 3)",
			"INSERT INTO tn (id, a, b) VALUES (3, NULL, 5)",
			"INSERT INTO tn (id, a, b) VALUES (4, 7, NULL)",
			"INSERT INTO tn (id, a, b) VALUES (5, NULL, NULL)",
			"INSERT INTO tn (id, a, b) VALUES (6, 5, 5)",
			"INSERT INTO tn (id, a, b) VALUES (7, 9, 2)",
		},
		Groups: []Group{
			{
				Name: "eq-reflexive-is-not-tautology", Reason: "a = a is UNKNOWN when a IS NULL, so a=a ≡ a IS NOT NULL (NOT always-true)",
				Queries: []string{
					"SELECT id FROM tn WHERE a = a",
					"SELECT id FROM tn WHERE a IS NOT NULL",
				},
			},
			{
				Name: "ne-vs-not-eq", Reason: "a <> b ≡ NOT (a = b) in 3VL (both UNKNOWN — excluded — when either is NULL)",
				Queries: []string{
					"SELECT id FROM tn WHERE a <> b",
					"SELECT id FROM tn WHERE NOT (a = b)",
				},
			},
			{
				Name: "demorgan-and", Reason: "De Morgan holds in Kleene 3VL: NOT (P AND Q) ≡ NOT P OR NOT Q, UNKNOWN included",
				Queries: []string{
					"SELECT id FROM tn WHERE NOT (a > 1 AND b > 1)",
					"SELECT id FROM tn WHERE NOT (a > 1) OR NOT (b > 1)",
				},
			},
			{
				Name: "count-ignores-null", Reason: "COUNT(a) counts only non-NULL a ≡ COUNT(*) filtered to a IS NOT NULL",
				Queries: []string{
					"SELECT COUNT(a) FROM tn",
					"SELECT COUNT(*) FROM tn WHERE a IS NOT NULL",
				},
			},
			{
				Name: "sum-ignores-null", Reason: "SUM(a) ignores NULL summands ≡ SUM over a IS NOT NULL",
				Queries: []string{
					"SELECT SUM(a) FROM tn",
					"SELECT SUM(a) FROM tn WHERE a IS NOT NULL",
				},
			},
			{
				// Regression for the GROUP-BY-ORDER-BY NULL-placement fix (rule_implement_streaming_agg:
				// grouping-key contiguity sort must be nulls-first). Same relation (distinct values of a)
				// under the same ORDER BY a ASC must produce the IDENTICAL ordered sequence regardless of
				// whether it's served by the DISTINCT/scan path or the GROUP BY/StreamingAgg path — NULL
				// placement is a property of the ORDER BY clause (ASC ⇒ NULLS FIRST, Java default), not of
				// the plan. Before the fix GROUP BY put NULL last while DISTINCT put it first.
				Name: "groupby-orderby-null-first-matches-distinct", Reason: "GROUP BY a ORDER BY a ≡ DISTINCT a ORDER BY a — ASC NULLS FIRST on both paths", Ordered: true,
				Queries: []string{
					"SELECT a FROM tn GROUP BY a ORDER BY a",
					"SELECT DISTINCT a FROM tn ORDER BY a",
				},
			},
			{
				// The DESC twin: both paths must place NULL LAST (ASC NULLS FIRST ⇒ DESC NULLS LAST), so
				// the fix to the ascending contiguity sort must not have disturbed the descending output.
				Name: "groupby-orderby-desc-null-last-matches-distinct", Reason: "GROUP BY a ORDER BY a DESC ≡ DISTINCT a ORDER BY a DESC — DESC NULLS LAST on both", Ordered: true,
				Queries: []string{
					"SELECT a FROM tn GROUP BY a ORDER BY a DESC",
					"SELECT DISTINCT a FROM tn ORDER BY a DESC",
				},
			},
			{
				// Multi-key twin: the fix sets NullsFirst on EVERY grouping-key sort key, so a two-column
				// GROUP BY ordered by both must place NULL first on each key, identically to multi-column
				// DISTINCT. A sibling-operator sweep confirmed no other ordered operator (UNION ALL,
				// computed-column sort, second-column sort) diverges — this pins the multi-key path.
				Name: "groupby-multikey-orderby-null-first-matches-distinct", Reason: "GROUP BY a,b ORDER BY a,b ≡ DISTINCT a,b ORDER BY a,b — NULLS FIRST on every key", Ordered: true,
				Queries: []string{
					"SELECT a, b FROM tn GROUP BY a, b ORDER BY a, b",
					"SELECT DISTINCT a, b FROM tn ORDER BY a, b",
				},
			},
			{
				Name: "having-vs-derived-filter", Reason: "HAVING p on an aggregate ≡ the same predicate on a derived aggregate table (SUM ignores NULL identically both sides)",
				Queries: []string{
					"SELECT a FROM tn GROUP BY a HAVING SUM(b) > 3",
					"SELECT a FROM (SELECT a, SUM(b) s FROM tn GROUP BY a) x WHERE s > 3",
				},
			},
			{
				Name: "distinct-count-eq-groupby-count", Reason: "|DISTINCT a| = |GROUP BY a| — both collapse NULLs to one group, so the group cardinality matches",
				Queries: []string{
					"SELECT COUNT(*) FROM (SELECT DISTINCT a FROM tn) x",
					"SELECT COUNT(*) FROM (SELECT a FROM tn GROUP BY a) y",
				},
			},
		},
	}
}

// joins is a two-table scenario whose relations are all rock-solid INNER-join equivalences —
// commutativity, comma-vs-explicit JOIN, ON-vs-WHERE predicate placement, and derived-table filter
// pushdown. None carries a 3VL trap: the join key equality (e.dept = d.did) excludes NULL/unmatched
// rows identically on both sides of every relation, so the multiset is invariant. It gives the
// oracle teeth on the join/filter planner — where the sqlpage hunt already found a bug family — that
// the single-table `rewrites` scenario cannot reach.
func joins() Scenario {
	return Scenario{
		Name: "joins",
		Seed: 2,
		Tables: []string{
			"CREATE TABLE emp (id BIGINT NOT NULL, dept BIGINT, sal BIGINT, PRIMARY KEY (id))",
			"CREATE TABLE dept (did BIGINT NOT NULL, budget BIGINT, PRIMARY KEY (did))",
		},
		Data: []string{
			"INSERT INTO emp (id, dept, sal) VALUES (1, 10, 100)",
			"INSERT INTO emp (id, dept, sal) VALUES (2, 10, 200)",
			"INSERT INTO emp (id, dept, sal) VALUES (3, 20, 150)",
			"INSERT INTO emp (id, dept, sal) VALUES (4, NULL, 300)", // NULL dept: excluded by inner join
			"INSERT INTO emp (id, dept, sal) VALUES (5, 99, 50)",    // dept 99 absent: excluded by inner join
			"INSERT INTO dept (did, budget) VALUES (10, 1000)",
			"INSERT INTO dept (did, budget) VALUES (20, 500)",
			"INSERT INTO dept (did, budget) VALUES (30, 2000)", // dept 30 unreferenced by any emp
		},
		Groups: []Group{
			{
				Name: "inner-join-commute", Reason: "INNER JOIN is commutative as a multiset (same projection)",
				Queries: []string{
					"SELECT e.id, d.did FROM emp e JOIN dept d ON e.dept = d.did",
					"SELECT e.id, d.did FROM dept d JOIN emp e ON e.dept = d.did",
				},
			},
			{
				Name: "eq-commute-on", Reason: "the join equality is commutative: ON e.dept=d.did ≡ ON d.did=e.dept",
				Queries: []string{
					"SELECT e.id, d.did FROM emp e JOIN dept d ON e.dept = d.did",
					"SELECT e.id, d.did FROM emp e JOIN dept d ON d.did = e.dept",
				},
			},
			{
				Name: "comma-vs-join", Reason: "SQL-89 comma join + WHERE equality ≡ explicit INNER JOIN … ON equality",
				Queries: []string{
					"SELECT e.id, d.did FROM emp e, dept d WHERE e.dept = d.did",
					"SELECT e.id, d.did FROM emp e JOIN dept d ON e.dept = d.did",
				},
			},
			{
				Name: "on-vs-where", Reason: "for an INNER join a WHERE predicate ≡ folding it into the ON clause",
				Queries: []string{
					"SELECT e.id FROM emp e JOIN dept d ON e.dept = d.did WHERE d.budget > 800",
					"SELECT e.id FROM emp e JOIN dept d ON e.dept = d.did AND d.budget > 800",
				},
			},
			{
				Name: "derived-filter-pushdown", Reason: "a filter over a derived table ≡ the conjunction pushed into the base scan",
				Queries: []string{
					"SELECT id FROM (SELECT id, sal FROM emp WHERE dept = 10) AS s WHERE s.sal > 100",
					"SELECT id FROM emp WHERE dept = 10 AND sal > 100",
				},
			},
		},
	}
}

// LoadDir reads scenarios from every *.json file in dir (each file is one Scenario object or an
// array of them) — the entry point for LLM-generated corpora.
func LoadDir(dir string) ([]Scenario, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Scenario
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		got, err := parseScenarios(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		out = append(out, got...)
	}
	return out, nil
}

// parseScenarios accepts a single Scenario object or a JSON array of them.
func parseScenarios(b []byte) ([]Scenario, error) {
	t := bytes.TrimSpace(b)
	if len(t) > 0 && t[0] == '[' {
		var many []Scenario
		if err := json.Unmarshal(t, &many); err != nil {
			return nil, err
		}
		return many, nil
	}
	var one Scenario
	if err := json.Unmarshal(t, &one); err != nil {
		return nil, err
	}
	return []Scenario{one}, nil
}

// indexed is the SECONDARY-INDEX scenario. Every other seed scenario runs over bare tables, so
// the relations only ever exercised the full-scan path — and the planner's index-backed paths are
// exactly where all three checked-in findings came from (an aggregate index dropping an all-NULL
// group, aggregate-index edge sums, a DISTINCT/GROUP BY divergence). A corpus that could not
// build an index could not have found any of them a second time.
//
// The relations are index-INVARIANCE relations: each pair asks the same question two ways, one of
// which the planner can answer from an index and one of which it cannot, so a wrong index entry —
// a dropped group, a missed NULL, a wrong extremum, a stale entry after an update — shows up as a
// row difference. They are equivalences of MEANING, so they hold whatever the planner chooses;
// that is what makes them safe to assert without pinning a plan shape.
func indexed() Scenario {
	return Scenario{
		Name: "indexed",
		Seed: 4,
		Tables: []string{
			"CREATE TABLE t (id BIGINT NOT NULL, g BIGINT, v BIGINT, PRIMARY KEY (id))",
			"CREATE INDEX by_v ON t (v)",
			"CREATE INDEX by_g_v ON t (g, v)",
			"CREATE INDEX sum_by_g AS SELECT SUM(v) FROM t GROUP BY g",
			"CREATE INDEX cnt_by_g AS SELECT COUNT(*) FROM t GROUP BY g",
		},
		Data: []string{
			"INSERT INTO t (id, g, v) VALUES (1, 1, 10)",
			"INSERT INTO t (id, g, v) VALUES (2, 1, 20)",
			"INSERT INTO t (id, g, v) VALUES (3, 2, NULL)", // all-NULL group: an index may drop it
			"INSERT INTO t (id, g, v) VALUES (4, 2, NULL)",
			"INSERT INTO t (id, g, v) VALUES (5, 3, 100)",
			"INSERT INTO t (id, g, v) VALUES (6, 3, -100)", // sums to zero: distinguishes 0 from absent
			"INSERT INTO t (id, g, v) VALUES (7, NULL, 5)", // NULL group key
			"INSERT INTO t (id, g, v) VALUES (8, 4, 42)",
			"UPDATE t SET v = 25 WHERE id = 2", // an index entry that must have been maintained
			"DELETE FROM t WHERE id = 8",       // an index entry that must have been removed
			"INSERT INTO t (id, g, v) VALUES (9, 4, 7)",
		},
		Groups: []Group{
			{
				Name:   "index-scan-equals-filtered-scan",
				Reason: "a range predicate on an indexed column selects the same rows however it is planned; the index-eligible form and the arithmetic form the planner cannot use an index for must agree",
				Queries: []string{
					"SELECT id FROM t WHERE v > 10",
					"SELECT id FROM t WHERE v - 10 > 0",
				},
			},
			{
				Name:   "indexed-order-equals-unindexed-order",
				Reason: "ORDER BY on an indexed column ≡ the same order derived through an expression the index cannot serve; NULLS FIRST on both",
				Queries: []string{
					"SELECT id FROM t ORDER BY v, id",
					"SELECT id FROM t ORDER BY v + 0, id",
				},
				Ordered: true,
			},
			{
				Name:   "aggregate-index-count-equals-derived-count",
				Reason: "COUNT(*) GROUP BY g counts every row in each group including all-NULL value groups; an index that skips NULL values would undercount",
				Queries: []string{
					"SELECT g, COUNT(*) FROM t GROUP BY g",
					"SELECT g, COUNT(*) FROM t WHERE id IS NOT NULL GROUP BY g",
				},
			},
			{
				Name:   "covering-projection-equals-full-projection",
				Reason: "a projection the (g,v) index covers ≡ the same projection the planner must fetch records for; index entries must have been maintained across the UPDATE and DELETE above",
				Queries: []string{
					"SELECT g, v FROM t ORDER BY g, v, id",
					"SELECT g, v FROM t WHERE id + 0 = id ORDER BY g, v, id",
				},
				Ordered: true,
			},
			{
				Name:   "indexed-equality-equals-in-singleton",
				Reason: "v = 25 ≡ v IN (25) on an indexed column; the row is one the UPDATE moved, so a stale index entry at the old value diverges",
				Queries: []string{
					"SELECT id FROM t WHERE v = 25",
					"SELECT id FROM t WHERE v IN (25)",
				},
			},
		},
	}
}
