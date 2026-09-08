package sqldriver_test

// Twin-table differential: the same rows live in TI (indexed) and TN (no
// indexes). Every query runs against both, and again on TI through a pinned
// connection with a scanned-rows limit of 3 so it pages through continuations.
// A row-multiset difference between the twins is a planner-soundness finding
// (the index-backed path disagrees with the full-scan path on the same rows); a
// difference between the paged and one-shot readings is a continuation finding.
// The oracle is the engine's own full scan, so it is blind to a defect the
// executor shares across both paths — those are pinned by hand-expected tests.
//
// The axis this covers, which the RFC-182 generator does not: a COMPOSITE
// primary key, a three-column mixed-type index, and indexes that repeat
// primary-key components. That is where the first finding lived — a
// primary-key intersection whose comparison key omitted a component fixed in
// one leg only (pk_intersection_leg_bound_component_fdb_test.go pins it).

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

type twinQuery struct {
	sql     string // uses {T} as the table placeholder
	ordered bool   // ORDER BY is total → compare sequences, not multisets
}

func twinRows(ctx context.Context, q interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}, sqlText string,
) ([]string, []string, error) {
	rows, err := q.QueryContext(ctx, sqlText)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, nil, err
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		parts := make([]string, len(cols))
		for i, v := range vals {
			switch x := v.(type) {
			case nil:
				parts[i] = "NULL"
			case []byte:
				parts[i] = fmt.Sprintf("%q", string(x))
			case float64:
				parts[i] = fmt.Sprintf("%v", x)
			default:
				parts[i] = fmt.Sprintf("%v", x)
			}
		}
		out = append(out, strings.Join(parts, "|"))
	}
	return out, cols, rows.Err()
}

func TestFDB_TwinTableDifferential(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_twin")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_twin")
	const colsDDL = "(pk1 BIGINT, pk2 BIGINT, a BIGINT, b BIGINT, s STRING, f BOOLEAN, d DOUBLE, PRIMARY KEY (pk1, pk2))"
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE twin "+
			"CREATE TABLE TI "+colsDDL+" "+
			"CREATE TABLE TN "+colsDDL+" "+
			"CREATE INDEX ti_a ON TI (a) "+
			"CREATE INDEX ti_s ON TI (s) "+
			"CREATE INDEX ti_asb ON TI (a, s, b) "+
			"CREATE INDEX ti_b_pk1 ON TI (b, pk1) "+
			"CREATE INDEX ti_d ON TI (d) "+
			"CREATE INDEX ti_f ON TI (f) "+
			"CREATE INDEX ti_pk2 ON TI (pk2)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_twin/s WITH TEMPLATE twin")
	dsn := fmt.Sprintf("fdbsql:///testdb_twin?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Deterministic data: duplicates, NULLs, boundaries.
	rng := rand.New(rand.NewPCG(7, 11))
	lit := func(v any) string {
		switch x := v.(type) {
		case nil:
			return "NULL"
		case string:
			return "'" + strings.ReplaceAll(x, "'", "''") + "'"
		case bool:
			if x {
				return "TRUE"
			}
			return "FALSE"
		case float64:
			return fmt.Sprintf("%v", x)
		default:
			return fmt.Sprintf("%v", x)
		}
	}
	var values []string
	strDomain := []any{"", "alpha", "beta", "b", "ba", "b%", "b_", "B", " a", "a "}
	for pk1 := int64(0); pk1 < 6; pk1++ {
		for pk2 := int64(0); pk2 < 7; pk2++ {
			var a, b, s, f, d any
			if rng.IntN(5) == 0 {
				a = nil
			} else if rng.IntN(15) == 0 {
				a = int64(-1)
			} else {
				a = int64(rng.IntN(4))
			}
			if rng.IntN(5) == 0 {
				b = nil
			} else {
				b = int64(rng.IntN(3))
			}
			if rng.IntN(5) == 0 {
				s = nil
			} else {
				s = strDomain[rng.IntN(len(strDomain))]
			}
			if rng.IntN(5) == 0 {
				f = nil
			} else {
				f = rng.IntN(2) == 0
			}
			switch rng.IntN(8) {
			case 0:
				d = nil
			case 1:
				d = 0.5
			case 2:
				d = -1.0
			default:
				d = float64(rng.IntN(3))
			}
			values = append(values, fmt.Sprintf("(%d, %d, %s, %s, %s, %s, %s)", pk1, pk2, lit(a), lit(b), lit(s), lit(f), lit(d)))
		}
	}
	// Boundary rows.
	values = append(values,
		"(6, 0, 9223372036854775807, 0, 'zz', TRUE, 1e300)",
		"(6, 1, -9223372036854775808, 1, '', FALSE, -1e300)",
		"(6, 2, 0, 2, 'alpha', NULL, 0.0)",
		"(7, 0, 1, NULL, NULL, NULL, NULL)",
		"(7, 1, 2, 1, 'beta', TRUE, 2.0)",
		"(-1, -1, 1, 1, 'b', TRUE, 1.0)",
	)
	for _, tbl := range []string{"TI", "TN"} {
		mwjoMustExec(t, db, ctx, "INSERT INTO "+tbl+" (pk1, pk2, a, b, s, f, d) VALUES "+strings.Join(values, ", "))
	}

	queries := []twinQuery{
		// Composite PK bounds.
		{"SELECT * FROM {T} WHERE pk1 = 1 AND pk2 > 3 ORDER BY pk1, pk2", true},
		{"SELECT * FROM {T} WHERE pk1 > 1 AND pk2 = 3 ORDER BY pk1, pk2", true},
		{"SELECT * FROM {T} WHERE pk1 IN (1, 3) AND pk2 = 2 ORDER BY pk1, pk2", true},
		{"SELECT * FROM {T} WHERE pk2 = 2 ORDER BY pk1", true},
		{"SELECT * FROM {T} WHERE pk1 = 2 ORDER BY pk2 DESC", true},
		{"SELECT * FROM {T} WHERE pk1 >= 5 ORDER BY pk1 DESC, pk2 DESC", true},
		{"SELECT * FROM {T} WHERE pk1 = 1 AND pk2 BETWEEN 2 AND 4 ORDER BY pk2", true},
		{"SELECT * FROM {T} WHERE pk1 = 1 OR pk2 = 1 ORDER BY pk1, pk2", true},
		{"SELECT * FROM {T} WHERE pk1 < 0 ORDER BY pk1, pk2", true},
		{"SELECT pk1, pk2 FROM {T} ORDER BY pk1, pk2 LIMIT 5 OFFSET 3", true},
		{"SELECT pk1, pk2 FROM {T} ORDER BY pk1 DESC, pk2 DESC LIMIT 4", true},
		{"SELECT pk1, pk2 FROM {T} WHERE pk1 = 1 ORDER BY pk2 DESC LIMIT 2 OFFSET 1", true},
		// Single-column index + residuals.
		{"SELECT * FROM {T} WHERE a = 1 AND b = 2", false},
		{"SELECT * FROM {T} WHERE a = 1 AND b IS NULL", false},
		{"SELECT * FROM {T} WHERE a IS NULL", false},
		{"SELECT * FROM {T} WHERE a IS NOT NULL AND s IS NULL", false},
		{"SELECT * FROM {T} WHERE a = -1", false},
		{"SELECT * FROM {T} WHERE a > 2", false},
		{"SELECT * FROM {T} WHERE a >= 9223372036854775807", false},
		{"SELECT * FROM {T} WHERE a <= -9223372036854775808", false},
		{"SELECT * FROM {T} WHERE a > -9223372036854775808 AND a < 9223372036854775807 AND a IS NOT NULL", false},
		{"SELECT * FROM {T} WHERE a <> 1", false},
		{"SELECT * FROM {T} WHERE NOT (a = 1)", false},
		{"SELECT * FROM {T} WHERE NOT (a = 1 OR a = 2)", false},
		{"SELECT * FROM {T} WHERE NOT (a = 1 AND b = 2)", false},
		{"SELECT * FROM {T} WHERE a IN (1, 1)", false},
		{"SELECT * FROM {T} WHERE a IN (1, NULL)", false},
		{"SELECT * FROM {T} WHERE a NOT IN (1, NULL)", false},
		{"SELECT * FROM {T} WHERE a NOT IN (1, 2)", false},
		{"SELECT * FROM {T} WHERE NOT (a IN (1, 2))", false},
		{"SELECT * FROM {T} WHERE a IN (3, 2, 1, 0, -1) ORDER BY a, pk1, pk2", true},
		{"SELECT * FROM {T} WHERE a IN (1, 2) ORDER BY a DESC, pk1, pk2", true},
		{"SELECT * FROM {T} WHERE a IN (2, 1) AND b IN (0, 1) ORDER BY a, b, pk1, pk2", true},
		// Cross-type comparisons on an indexed BIGINT column.
		{"SELECT * FROM {T} WHERE a = 1.0", false},
		{"SELECT * FROM {T} WHERE a = 1.5", false},
		{"SELECT * FROM {T} WHERE a > 1.5", false},
		{"SELECT * FROM {T} WHERE a >= 1.5", false},
		{"SELECT * FROM {T} WHERE a < 0.5", false},
		{"SELECT * FROM {T} WHERE a BETWEEN 0.5 AND 2.5", false},
		{"SELECT * FROM {T} WHERE a IN (1.0, 2.5)", false},
		{"SELECT * FROM {T} WHERE a > 1e19", false},
		{"SELECT * FROM {T} WHERE a < -1e19", false},
		{"SELECT * FROM {T} WHERE a < 1e19", false},
		// Indexed DOUBLE vs int literals.
		{"SELECT * FROM {T} WHERE d = 1", false},
		{"SELECT * FROM {T} WHERE d > 0", false},
		{"SELECT * FROM {T} WHERE d >= 0.5 AND d < 2", false},
		{"SELECT * FROM {T} WHERE d IN (0, 1, 0.5)", false},
		{"SELECT * FROM {T} WHERE d = 0.0", false},
		{"SELECT * FROM {T} WHERE d > -1e300", false},
		{"SELECT * FROM {T} WHERE d >= 1e300", false},
		{"SELECT * FROM {T} WHERE d IS NULL OR d = 2", false},
		{"SELECT * FROM {T} ORDER BY d, pk1, pk2", true},
		{"SELECT * FROM {T} ORDER BY d DESC NULLS LAST, pk1, pk2", true},
		{"SELECT * FROM {T} ORDER BY d NULLS LAST, pk1, pk2", true},
		{"SELECT * FROM {T} ORDER BY d DESC, pk1, pk2", true},
		// Strings.
		{"SELECT * FROM {T} WHERE s = ''", false},
		{"SELECT * FROM {T} WHERE s > 'b'", false},
		{"SELECT * FROM {T} WHERE s >= 'b' AND s < 'c'", false},
		{"SELECT * FROM {T} WHERE s LIKE 'b%'", false},
		{"SELECT * FROM {T} WHERE s LIKE 'b_'", false},
		{"SELECT * FROM {T} WHERE s LIKE 'b\\%' ESCAPE '\\'", false},
		{"SELECT * FROM {T} WHERE s LIKE 'b\\_' ESCAPE '\\'", false},
		{"SELECT * FROM {T} WHERE s LIKE '%a'", false},
		{"SELECT * FROM {T} WHERE s LIKE ' %'", false},
		{"SELECT * FROM {T} WHERE s LIKE '%'", false},
		{"SELECT * FROM {T} WHERE s NOT LIKE 'b%'", false},
		{"SELECT * FROM {T} WHERE s LIKE 'B%'", false},
		{"SELECT * FROM {T} WHERE s IN ('b', 'B', '')", false},
		{"SELECT * FROM {T} WHERE s = 'b' OR s = 'ba' ORDER BY s, pk1, pk2", true},
		{"SELECT * FROM {T} ORDER BY s, pk1, pk2", true},
		{"SELECT * FROM {T} ORDER BY s DESC, pk1 DESC, pk2 DESC", true},
		{"SELECT * FROM {T} WHERE s IS NOT NULL ORDER BY s DESC NULLS FIRST, pk1, pk2", true},
		// Booleans.
		{"SELECT * FROM {T} WHERE f", false},
		{"SELECT * FROM {T} WHERE NOT f", false},
		{"SELECT * FROM {T} WHERE f = TRUE", false},
		{"SELECT * FROM {T} WHERE f IS TRUE", false},
		{"SELECT * FROM {T} WHERE f IS NOT TRUE", false},
		{"SELECT * FROM {T} WHERE f IS FALSE", false},
		{"SELECT * FROM {T} WHERE f IS NOT FALSE", false},
		{"SELECT * FROM {T} WHERE f <> TRUE", false},
		{"SELECT * FROM {T} WHERE f IS NULL", false},
		{"SELECT * FROM {T} WHERE f = TRUE AND a = 1", false},
		{"SELECT * FROM {T} WHERE f = TRUE OR a = 1", false},
		{"SELECT * FROM {T} ORDER BY f, pk1, pk2", true},
		// 3-column composite index.
		{"SELECT * FROM {T} WHERE a = 1 AND s = 'b' AND b = 1", false},
		{"SELECT * FROM {T} WHERE a = 1 AND s = 'b' AND b > 0", false},
		{"SELECT * FROM {T} WHERE a = 1 AND s > 'b' AND b = 1", false},
		{"SELECT * FROM {T} WHERE a = 1 AND b = 1", false},
		{"SELECT * FROM {T} WHERE a = 1 AND s IS NULL AND b = 1", false},
		{"SELECT * FROM {T} WHERE a = 1 AND s IS NULL ORDER BY b, pk1, pk2", true},
		{"SELECT * FROM {T} WHERE a = 1 ORDER BY s, b, pk1, pk2", true},
		{"SELECT * FROM {T} WHERE a = 1 ORDER BY s DESC, b DESC, pk1 DESC, pk2 DESC", true},
		{"SELECT * FROM {T} WHERE a = 1 ORDER BY s, b DESC, pk1, pk2", true},
		{"SELECT * FROM {T} WHERE a = 1 ORDER BY s NULLS LAST, b, pk1, pk2", true},
		{"SELECT * FROM {T} WHERE a = 1 AND s = 'b' ORDER BY b DESC, pk1, pk2", true},
		{"SELECT * FROM {T} WHERE a IN (1, 2) AND s = 'b' ORDER BY a, b, pk1, pk2", true},
		{"SELECT * FROM {T} WHERE a IN (1, 2) AND s = 'b' ORDER BY b, pk1, pk2", true},
		{"SELECT * FROM {T} WHERE a = 1 AND s IN ('b', 'ba') ORDER BY s, b, pk1, pk2", true},
		{"SELECT * FROM {T} WHERE a = 1 AND s IN ('b', 'ba') AND b IN (0, 1) ORDER BY s, b, pk1, pk2", true},
		{"SELECT * FROM {T} WHERE a = 1 AND s = 'b' AND b = 1 ORDER BY pk1, pk2", true},
		{"SELECT * FROM {T} WHERE a = 1 AND s = 'b' AND b = 1 ORDER BY pk2, pk1", true},
		{"SELECT a, s, b FROM {T} WHERE a = 1 ORDER BY s, b, pk1, pk2", true},
		{"SELECT a, s, b FROM {T} WHERE a >= 1 ORDER BY a, s, b, pk1, pk2", true},
		{"SELECT a, s, b FROM {T} WHERE a >= 1 ORDER BY a DESC, s DESC, b DESC, pk1 DESC, pk2 DESC", true},
		{"SELECT a, s, b FROM {T} ORDER BY a, s, b, pk1, pk2", true},
		{"SELECT a, s, b FROM {T} ORDER BY a NULLS LAST, s NULLS LAST, b NULLS LAST, pk1, pk2", true},
		{"SELECT a, s, b FROM {T} WHERE a = 1 AND s > 'a' AND s < 'c' ORDER BY s, b, pk1, pk2", true},
		{"SELECT a, s, b, pk1, pk2 FROM {T} WHERE a = 1 AND s = 'b' AND b > 0 AND pk1 > 1 ORDER BY pk1, pk2", true},
		// Index containing a pk column.
		{"SELECT * FROM {T} WHERE b = 1 AND pk1 = 2", false},
		{"SELECT * FROM {T} WHERE a = 1 AND pk2 = 3", false},
		{"SELECT * FROM {T} WHERE a = 1 AND pk1 = 3", false},
		{"SELECT * FROM {T} WHERE a = 1 AND pk1 = 3 ORDER BY pk2", true},
		{"SELECT * FROM {T} WHERE s = 'b' AND pk2 = 3 ORDER BY pk1", true},
		{"SELECT * FROM {T} WHERE a = 1 AND s = 'b' AND pk2 = 3", false},
		{"SELECT * FROM {T} WHERE a = 1 AND b = 1 AND pk2 = 3", false},
		{"SELECT * FROM {T} WHERE b = 1 AND pk2 IN (3, 4)", false},
		{"SELECT * FROM {T} WHERE b IN (0, 1) AND pk2 = 3", false},
		{"SELECT * FROM {T} WHERE b = 1 OR pk2 = 3 ORDER BY pk1, pk2", true},
		{"SELECT * FROM {T} WHERE b = 1 OR pk2 = 3 ORDER BY pk1", false},
		{"SELECT * FROM {T} WHERE b = 1 OR pk2 = 3", false},
		{"SELECT * FROM {T} WHERE pk2 = 3 OR pk2 = 4 ORDER BY pk1", false},
		{"SELECT * FROM {T} WHERE a = 1 OR pk2 = 3 ORDER BY pk1", false},
		{"SELECT * FROM {T} WHERE (a = 1 OR pk2 = 3) AND b = 1 ORDER BY pk1", false},
		{"SELECT * FROM {T} WHERE d = 1 AND pk2 = 3", false},
		{"SELECT * FROM {T} WHERE f = TRUE AND pk2 = 3", false},
		{"SELECT COUNT(*) FROM {T} WHERE b = 1 AND pk2 = 3", false},
		{"SELECT pk1 FROM {T} WHERE b = 1 AND pk2 = 3", false},
		{"SELECT * FROM {T} WHERE b = 1 AND pk1 > 2 ORDER BY pk1, pk2", true},
		{"SELECT * FROM {T} WHERE b = 1 ORDER BY pk1, pk2", true},
		{"SELECT * FROM {T} WHERE b = 1 ORDER BY pk1 DESC, pk2 DESC", true},
		{"SELECT * FROM {T} WHERE b = 1 AND pk2 = 3", false},
		{"SELECT * FROM {T} WHERE b IN (0, 1) AND pk1 = 2 ORDER BY b, pk2", true},
		{"SELECT pk1, pk2 FROM {T} WHERE b = 1 AND pk1 = 2 AND pk2 > 1 ORDER BY pk2", true},
		{"SELECT b, pk1 FROM {T} WHERE b IS NULL ORDER BY pk1, pk2", true},
		{"SELECT * FROM {T} WHERE pk2 = 3 AND b = 1 ORDER BY pk1", true},
		{"SELECT * FROM {T} WHERE pk2 > 4 ORDER BY pk2, pk1", true},
		{"SELECT * FROM {T} WHERE pk2 IN (0, 6) ORDER BY pk2 DESC, pk1", true},
		// OR / union shapes.
		{"SELECT * FROM {T} WHERE a = 1 OR s = 'b' ORDER BY pk1, pk2", true},
		{"SELECT * FROM {T} WHERE a = 1 OR a = 2 OR s = 'b'", false},
		{"SELECT * FROM {T} WHERE (a = 1 OR s = 'b') AND b > 0", false},
		{"SELECT * FROM {T} WHERE (a = 1 OR s = 'b') AND (b = 1 OR d = 1)", false},
		{"SELECT * FROM {T} WHERE a = 1 OR b IS NULL", false},
		{"SELECT * FROM {T} WHERE a = 1 OR a IS NULL", false},
		{"SELECT * FROM {T} WHERE a > 1 OR a < 1", false},
		{"SELECT * FROM {T} WHERE a > 0 OR d > 0 ORDER BY pk1, pk2", true},
		{"SELECT * FROM {T} WHERE a = 1 OR s = 'b' ORDER BY pk1, pk2 LIMIT 3", true},
		{"SELECT pk1, pk2 FROM {T} WHERE a = 1 OR s = 'b' ORDER BY pk1 DESC, pk2 DESC LIMIT 3 OFFSET 2", true},
		{"SELECT * FROM {T} WHERE a = 1 OR s = 'b' ORDER BY a, pk1, pk2", true},
		{"SELECT * FROM {T} WHERE (a = 1 AND s = 'b') OR (a = 2 AND s = 'ba') ORDER BY a, s, b, pk1, pk2", true},
		{"SELECT * FROM {T} WHERE (a = 1 AND b = 1) OR (a = 1 AND b = 2)", false},
		{"SELECT * FROM {T} WHERE a = 1 AND (b = 1 OR b = 2)", false},
		{"SELECT * FROM {T} WHERE a = 1 AND (b = 1 OR s = 'b')", false},
		{"SELECT * FROM {T} WHERE (a = 1 OR a = 2) AND (s = 'b' OR s = 'ba') ORDER BY a, s, pk1, pk2", true},
		{"SELECT DISTINCT a FROM {T} WHERE a = 1 OR s = 'b'", false},
		{"SELECT DISTINCT a, s FROM {T} ORDER BY a, s", true},
		{"SELECT DISTINCT a FROM {T} ORDER BY a DESC", true},
		{"SELECT DISTINCT a FROM {T} ORDER BY a NULLS LAST LIMIT 3", true},
		{"SELECT DISTINCT b FROM {T} WHERE a = 1 ORDER BY b", true},
		{"SELECT DISTINCT s FROM {T} WHERE a = 1 ORDER BY s", true},
		{"SELECT DISTINCT pk1 FROM {T} WHERE b = 1 ORDER BY pk1", true},
		// Constant / tautological predicates with NULL semantics.
		{"SELECT * FROM {T} WHERE a = a", false},
		{"SELECT * FROM {T} WHERE a <> a", false},
		{"SELECT * FROM {T} WHERE a = b", false},
		{"SELECT * FROM {T} WHERE a < b", false},
		{"SELECT * FROM {T} WHERE a = pk1", false},
		{"SELECT * FROM {T} WHERE a IN (b, pk1)", false},
		{"SELECT * FROM {T} WHERE 1 = 1 AND a = 1", false},
		{"SELECT * FROM {T} WHERE 1 = 0 OR a = 1", false},
		{"SELECT * FROM {T} WHERE NULL IS NULL AND a = 1", false},
		{"SELECT * FROM {T} WHERE a = NULL", false},
		{"SELECT * FROM {T} WHERE NOT (a = NULL)", false},
		{"SELECT * FROM {T} WHERE a IS DISTINCT FROM 1", false},
		{"SELECT * FROM {T} WHERE a IS NOT DISTINCT FROM NULL", false},
		{"SELECT * FROM {T} WHERE a IS NOT DISTINCT FROM b", false},
		{"SELECT * FROM {T} WHERE COALESCE(a, 0) = 0", false},
		{"SELECT * FROM {T} WHERE COALESCE(a, b) = 1", false},
		{"SELECT * FROM {T} WHERE a + 1 = 2", false},
		{"SELECT * FROM {T} WHERE a + b > 2", false},
		{"SELECT * FROM {T} WHERE -a = -1", false},
		{"SELECT * FROM {T} WHERE a * 0 = 0", false},
		{"SELECT * FROM {T} WHERE a BETWEEN b AND 2", false},
		{"SELECT * FROM {T} WHERE a BETWEEN 2 AND 1", false},
		{"SELECT * FROM {T} WHERE a BETWEEN NULL AND 2", false},
		{"SELECT * FROM {T} WHERE a NOT BETWEEN 1 AND 2", false},
		{"SELECT * FROM {T} WHERE CASE WHEN a = 1 THEN TRUE ELSE FALSE END", false},
		{"SELECT * FROM {T} WHERE CASE WHEN a = 1 THEN b ELSE 0 END = 1", false},
		{"SELECT * FROM {T} WHERE (a = 1) = (b = 1)", false},
		{"SELECT * FROM {T} WHERE (a = 1) IS NULL", false},
		{"SELECT * FROM {T} WHERE (a > 1) IS NOT TRUE", false},
		{"SELECT * FROM {T} WHERE a = 1 XOR b = 1", false},
		// Aggregates.
		{"SELECT a, COUNT(*) FROM {T} GROUP BY a ORDER BY a", true},
		{"SELECT a, COUNT(b) FROM {T} GROUP BY a ORDER BY a", true},
		{"SELECT a, SUM(b) FROM {T} GROUP BY a ORDER BY a", true},
		{"SELECT a, s, COUNT(*) FROM {T} GROUP BY a, s ORDER BY a, s", true},
		{"SELECT a, s, MAX(b), MIN(b) FROM {T} WHERE a = 1 GROUP BY a, s ORDER BY s", true},
		{"SELECT s, COUNT(*) FROM {T} WHERE a = 1 GROUP BY s ORDER BY s", true},
		{"SELECT a, COUNT(*) FROM {T} WHERE s = 'b' GROUP BY a ORDER BY a", true},
		{"SELECT a, COUNT(*) FROM {T} GROUP BY a HAVING COUNT(*) > 5 ORDER BY a", true},
		{"SELECT a, SUM(d) FROM {T} GROUP BY a ORDER BY a", true},
		{"SELECT COUNT(*) FROM {T} WHERE a = 1 AND s = 'b'", false},
		{"SELECT COUNT(*) FROM {T} WHERE a = 100", false},
		{"SELECT MAX(a), MIN(a) FROM {T} WHERE a = 100", false},
		{"SELECT SUM(a) FROM {T} WHERE a = 100", false},
		{"SELECT MAX(s), MIN(s) FROM {T}", false},
		{"SELECT MAX(pk2), MIN(pk2) FROM {T} WHERE pk1 = 1", false},
		{"SELECT MAX(a) FROM {T}", false},
		{"SELECT MIN(a) FROM {T}", false},
		{"SELECT MAX(d), MIN(d) FROM {T}", false},
		{"SELECT b, MAX(pk1) FROM {T} GROUP BY b ORDER BY b", true},
		{"SELECT a, COUNT(*) FROM {T} WHERE a IN (1, 2) GROUP BY a ORDER BY a", true},
		{"SELECT a, COUNT(*) FROM {T} WHERE a = 1 OR s = 'b' GROUP BY a ORDER BY a", true},
		{"SELECT pk1, COUNT(*) FROM {T} GROUP BY pk1 ORDER BY pk1", true},
		{"SELECT pk1, COUNT(*) FROM {T} WHERE pk1 > 3 GROUP BY pk1 ORDER BY pk1", true},
		{"SELECT a, COUNT(*) FROM {T} GROUP BY a ORDER BY COUNT(*) DESC, a", true},
		{"SELECT COUNT(*), SUM(a), AVG(a) FROM {T}", false},
		{"SELECT AVG(b) FROM {T} WHERE a = 1", false},
		// Joins (self).
		{"SELECT l.pk1, l.pk2, r.pk1, r.pk2 FROM {T} AS l JOIN {T} AS r ON l.a = r.b WHERE l.pk1 = 1 ORDER BY l.pk1, l.pk2, r.pk1, r.pk2", true},
		{"SELECT l.pk1, l.pk2, r.pk1, r.pk2 FROM {T} AS l JOIN {T} AS r ON l.a = r.a WHERE l.pk1 = 1 AND r.s = 'b' ORDER BY l.pk1, l.pk2, r.pk1, r.pk2", true},
		{"SELECT l.pk1, l.pk2, r.pk1, r.pk2 FROM {T} AS l JOIN {T} AS r ON l.pk1 = r.pk1 AND l.pk2 = r.pk2 WHERE l.a = 1 ORDER BY l.pk1, l.pk2", true},
		{"SELECT l.pk1, l.pk2, r.pk1, r.pk2 FROM {T} AS l JOIN {T} AS r ON l.pk1 = r.pk2 WHERE l.a = 1 AND r.b = 1 ORDER BY l.pk1, l.pk2, r.pk1, r.pk2", true},
		{"SELECT l.pk1, l.pk2, r.pk1, r.pk2 FROM {T} AS l LEFT JOIN {T} AS r ON l.a = r.a AND r.b = 1 WHERE l.pk1 = 1 ORDER BY l.pk1, l.pk2, r.pk1, r.pk2", true},
		{"SELECT l.pk1, l.pk2, r.pk1, r.pk2 FROM {T} AS l LEFT JOIN {T} AS r ON l.a = r.a WHERE l.pk1 = 1 AND r.b = 1 ORDER BY l.pk1, l.pk2, r.pk1, r.pk2", true},
		{"SELECT l.pk1, l.pk2, r.pk1, r.pk2 FROM {T} AS l LEFT JOIN {T} AS r ON l.a = r.a WHERE l.pk1 = 1 AND r.pk1 IS NULL ORDER BY l.pk1, l.pk2", true},
		{"SELECT l.pk1, l.pk2, r.pk1, r.pk2 FROM {T} AS l LEFT JOIN {T} AS r ON l.s = r.s AND l.a = r.a WHERE l.pk1 = 1 ORDER BY l.pk1, l.pk2, r.pk1, r.pk2", true},
		{"SELECT l.pk1, l.pk2, r.pk1, r.pk2 FROM {T} AS l JOIN {T} AS r ON l.a < r.a WHERE l.pk1 = 1 AND r.pk1 = 2 ORDER BY l.pk1, l.pk2, r.pk1, r.pk2", true},
		{"SELECT l.pk1, l.pk2, r.pk1, r.pk2 FROM {T} AS l JOIN {T} AS r ON l.a = r.a WHERE l.pk1 = 1 AND r.a = 2 ORDER BY l.pk1, l.pk2, r.pk1, r.pk2", true},
		{"SELECT l.pk1, l.pk2, r.pk1, r.pk2 FROM {T} AS l JOIN {T} AS r ON l.a = r.a AND l.s = r.s AND l.b = r.b WHERE l.pk1 = 1 ORDER BY l.pk1, l.pk2, r.pk1, r.pk2", true},
		{"SELECT COUNT(*) FROM {T} AS l JOIN {T} AS r ON l.a = r.a WHERE l.pk1 = 1", false},
		{"SELECT l.a, COUNT(*) FROM {T} AS l JOIN {T} AS r ON l.a = r.a WHERE l.pk1 = 1 GROUP BY l.a ORDER BY l.a", true},
		{"SELECT l.pk1, l.pk2 FROM {T} AS l JOIN {T} AS r ON l.a = r.a WHERE l.pk1 = 1 ORDER BY l.pk1, l.pk2, r.pk1, r.pk2 LIMIT 3", true},
		// Subqueries.
		{"SELECT * FROM {T} AS o WHERE EXISTS (SELECT 1 FROM {T} AS i WHERE i.a = o.b AND i.s = 'b') ORDER BY pk1, pk2", true},
		{"SELECT * FROM {T} AS o WHERE NOT EXISTS (SELECT 1 FROM {T} AS i WHERE i.a = o.b AND i.s = 'b') ORDER BY pk1, pk2", true},
		{"SELECT * FROM {T} AS o WHERE EXISTS (SELECT 1 FROM {T} AS i WHERE i.pk1 = o.pk2 AND i.a = 1) ORDER BY pk1, pk2", true},
		{"SELECT * FROM {T} WHERE a IN (SELECT b FROM {T} WHERE s = 'b') ORDER BY pk1, pk2", true},
		{"SELECT * FROM {T} WHERE a NOT IN (SELECT b FROM {T} WHERE s = 'b') ORDER BY pk1, pk2", true},
		{"SELECT * FROM {T} WHERE a NOT IN (SELECT b FROM {T} WHERE s = 'alpha') ORDER BY pk1, pk2", true},
		{"SELECT * FROM {T} WHERE a IN (SELECT b FROM {T} WHERE s = 'nonexistent') ORDER BY pk1, pk2", true},
		{"SELECT * FROM {T} WHERE a NOT IN (SELECT b FROM {T} WHERE s = 'nonexistent') ORDER BY pk1, pk2", true},
		{"SELECT * FROM {T} WHERE a = (SELECT MAX(b) FROM {T}) ORDER BY pk1, pk2", true},
		{"SELECT * FROM {T} WHERE a = (SELECT MAX(a) FROM {T} WHERE a = 100) ORDER BY pk1, pk2", true},
		{"SELECT * FROM {T} WHERE a > (SELECT COUNT(*) FROM {T} WHERE a = 100) ORDER BY pk1, pk2", true},
		{"SELECT * FROM {T} WHERE a = (SELECT MIN(a) FROM {T} WHERE s = 'b') ORDER BY pk1, pk2", true},
		{"SELECT * FROM {T} AS o WHERE a = (SELECT MAX(i.b) FROM {T} AS i WHERE i.pk1 = o.pk1) ORDER BY pk1, pk2", true},
		// Unions.
		{"SELECT pk1, pk2 FROM {T} WHERE a = 1 UNION SELECT pk1, pk2 FROM {T} WHERE s = 'b'", false},
		{"SELECT pk1, pk2 FROM {T} WHERE a = 1 UNION ALL SELECT pk1, pk2 FROM {T} WHERE s = 'b'", false},
		{"SELECT a FROM {T} WHERE a = 1 UNION SELECT b FROM {T} WHERE s = 'b'", false},
		{"SELECT a, s FROM {T} WHERE a = 1 UNION SELECT a, s FROM {T} WHERE a IS NULL", false},
		{"SELECT a FROM {T} WHERE pk1 = 1 UNION SELECT a FROM {T} WHERE pk1 = 2 ORDER BY a", true},
		// Derived tables.
		{"SELECT * FROM (SELECT a, b FROM {T} WHERE a = 1) AS x WHERE b = 1", false},
		{"SELECT * FROM (SELECT a, b, s FROM {T} WHERE a = 1 ORDER BY s, b, pk1, pk2 LIMIT 3) AS x WHERE b IS NOT NULL", false},
		{"SELECT * FROM (SELECT pk1, pk2 FROM {T} ORDER BY pk1, pk2 LIMIT 4) AS x ORDER BY pk1 DESC, pk2 DESC", true},
		{"SELECT * FROM (SELECT pk1, pk2 FROM {T} ORDER BY pk1, pk2 LIMIT 4 OFFSET 2) AS x ORDER BY pk1, pk2 LIMIT 2", true},
		{"SELECT * FROM (SELECT pk1, pk2 FROM {T} WHERE a = 1 ORDER BY pk1, pk2 LIMIT 4) AS x WHERE pk1 > 0 ORDER BY pk1, pk2", true},
		{"SELECT x.a, x.n FROM (SELECT a, COUNT(*) AS n FROM {T} GROUP BY a) AS x WHERE x.n > 3 ORDER BY x.a", true},
		{"SELECT x.a FROM (SELECT DISTINCT a FROM {T} WHERE s = 'b') AS x ORDER BY x.a", true},
		{"SELECT COUNT(*) FROM (SELECT pk1 FROM {T} WHERE a = 1 ORDER BY pk1, pk2 LIMIT 3) AS x", false},
		{"SELECT COUNT(*) FROM (SELECT DISTINCT a FROM {T}) AS x", false},
		{"SELECT * FROM (SELECT a, b FROM {T} WHERE a = 1 UNION SELECT a, b FROM {T} WHERE s = 'b') AS x WHERE b = 1", false},
	}

	var mismatches int
	explain := mwjoExplainer(t, db, ctx)
	for _, q := range queries {
		qi := strings.ReplaceAll(q.sql, "{T}", "TI")
		qn := strings.ReplaceAll(q.sql, "{T}", "TN")
		ri, ci, erri := twinRows(ctx, db, qi)
		rn, cn, errn := twinRows(ctx, db, qn)
		if (erri == nil) != (errn == nil) {
			mismatches++
			t.Errorf("ERROR DIVERGENCE\n  sql: %s\n  TI err: %v\n  TN err: %v", q.sql, erri, errn)
			continue
		}
		if erri != nil {
			t.Logf("both errored: %s\n  err: %v", q.sql, erri)
			continue
		}
		if strings.Join(ci, ",") != strings.Join(cn, ",") {
			mismatches++
			t.Errorf("COLUMN DIVERGENCE\n  sql: %s\n  TI: %v\n  TN: %v", q.sql, ci, cn)
		}
		si, sn := append([]string(nil), ri...), append([]string(nil), rn...)
		if !q.ordered {
			sort.Strings(si)
			sort.Strings(sn)
		}
		if strings.Join(si, "\n") != strings.Join(sn, "\n") {
			mismatches++
			t.Errorf("ROW DIVERGENCE (ordered=%v)\n  sql: %s\n  plan(TI): %s\n  TI (%d rows):\n    %s\n  TN (%d rows):\n    %s",
				q.ordered, q.sql, explain(qi), len(ri), strings.Join(ri, "\n    "), len(rn), strings.Join(rn, "\n    "))
			continue
		}
		if len(ri) == 0 {
			t.Logf("EMPTY (both): %s", q.sql)
		}
	}

	// Paging variant on the indexed twin: a pinned connection with a tiny
	// scanned-rows limit so every query pages through continuations.
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	defer conn.Close()
	if err := conn.Raw(func(dc any) error {
		ec, ok := dc.(*embedded.EmbeddedConnection)
		if !ok {
			return fmt.Errorf("driver conn is %T", dc)
		}
		ec.SetOptions(api.NewOptionsBuilder().Set(api.OptExecutionScannedRowsLimit, 3).Build())
		return nil
	}); err != nil {
		t.Fatalf("set scan limit: %v", err)
	}
	for _, q := range queries {
		qi := strings.ReplaceAll(q.sql, "{T}", "TI")
		full, _, err := twinRows(ctx, db, qi)
		if err != nil {
			continue
		}
		paged, _, err := twinRows(ctx, conn, qi)
		if err != nil {
			if strings.Contains(err.Error(), "54F01") {
				t.Logf("paging decline (54F01): %s", q.sql)
				continue
			}
			mismatches++
			t.Errorf("PAGING ERROR\n  sql: %s\n  err: %v", q.sql, err)
			continue
		}
		sf, sp := append([]string(nil), full...), append([]string(nil), paged...)
		if !q.ordered {
			sort.Strings(sf)
			sort.Strings(sp)
		}
		if strings.Join(sf, "\n") != strings.Join(sp, "\n") {
			mismatches++
			t.Errorf("PAGING DIVERGENCE (ordered=%v)\n  sql: %s\n  plan: %s\n  full (%d rows):\n    %s\n  paged (%d rows):\n    %s",
				q.ordered, q.sql, explain(qi), len(full), strings.Join(full, "\n    "), len(paged), strings.Join(paged, "\n    "))
		}
	}
	t.Logf("twin differential: %d queries, %d mismatches", len(queries), mismatches)
}
