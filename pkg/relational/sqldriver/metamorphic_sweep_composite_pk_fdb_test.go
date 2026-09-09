package sqldriver_test

// Composite-primary-key axis of the indexed/unindexed twin.
//
// Every other twin in this family keys its table on a single `id`. That is the
// one shape under which a primary-key intersection can never be built over a
// leg that fixes a primary-key component: a single-component key fixed in a leg
// makes that leg max-cardinality 1, and the partition is pruned as redundant.
// With PRIMARY KEY (pk1, pk2) the planner CAN build such a merge, and did —
// intersecting the (b, pk1) and (pk2) covering scans on (pk1) alone, so that
// `WHERE b = 1 AND pk2 = 3` returned every b = 1 record whose pk1 also had some
// pk2 = 3 record (pk_intersection_leg_bound_component_fdb_test.go pins the
// repair; RFC-245 has the write-up). This file is the net that found it and the
// axis nothing else here reaches: a composite primary key, a three-column
// mixed-type index, and indexes that repeat primary-key components.
//
// Two sweeps. The READ sweep runs every query against both schemas and then
// again on the indexed side through a pinned connection with a scanned-rows
// limit of 3, so it pages through continuations: a twin difference is a
// planner-soundness finding, a paged/one-shot difference is a continuation
// finding. The DML sweep applies the same UPDATE/DELETE/INSERT sequence to both
// schemas — predicates that reach the intersection shapes, index-key columns
// set to NULL and back, aggregate indexes maintained across every statement —
// and compares the whole table and the index-backed reads after each one.
//
// The oracle is the engine's own full scan, so it is blind to a defect shared
// by both paths; those are pinned by hand-expected tests. Each sweep carries
// non-vacuity floors stated with the population they were measured over.

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

type mhcpkQuery struct {
	sql     string
	ordered bool // ORDER BY is total → compare sequences, not multisets
}

const mhcpkTable = "CREATE TABLE t (pk1 BIGINT, pk2 BIGINT, a BIGINT, b BIGINT, s STRING, f BOOLEAN, d DOUBLE, PRIMARY KEY (pk1, pk2)) "

// mhcpkLit renders a fixture value as a SQL literal.
func mhcpkLit(v any) string {
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
	default:
		return fmt.Sprintf("%v", x)
	}
}

// mhcpkCompare runs q on both schemas and reports a twin disagreement. It
// returns (compared, nonEmpty): compared is false when both sides errored (a
// legitimate outcome for a query the engine declines, counted by the caller),
// nonEmpty is whether the agreed answer had rows.
func mhcpkCompare(w *mmTwin, stage string, q mhcpkQuery) (compared, nonEmpty bool) {
	w.t.Helper()
	gi, ei := mmRows(w.t, w.ctx, w.idx, q.sql)
	gn, en := mmRows(w.t, w.ctx, w.plain, q.sql)
	if (ei == nil) != (en == nil) {
		w.t.Errorf("%s: ERROR ASYMMETRY\n  q: %s\n  indexed:   %v\n  unindexed: %v", stage, q.sql, ei, en)
		return false, false
	}
	if ei != nil {
		w.t.Logf("%s: both errored: %s: %v", stage, q.sql, ei)
		return false, false
	}
	si, sn := append([]string(nil), gi...), append([]string(nil), gn...)
	if !q.ordered {
		sort.Strings(si)
		sort.Strings(sn)
	}
	if !mmEqRows(si, sn) {
		w.t.Errorf("%s: indexed and unindexed DISAGREE (ordered=%v)\n  q: %s\n  plan: %s\n  indexed   (%d): %v\n  unindexed (%d): %v",
			stage, q.ordered, q.sql, w.Explain(q.sql), len(gi), gi, len(gn), gn)
		return true, len(gi) > 0
	}
	return true, len(gi) > 0
}

func TestFDB_MetamorphicCompositePrimaryKey(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_mhcpk", "mhcpk", mhcpkTable,
		"CREATE INDEX t_a ON t (a) "+
			"CREATE INDEX t_s ON t (s) "+
			"CREATE INDEX t_asb ON t (a, s, b) "+
			"CREATE INDEX t_b_pk1 ON t (b, pk1) "+
			"CREATE INDEX t_d ON t (d) "+
			"CREATE INDEX t_f ON t (f) "+
			"CREATE INDEX t_pk2 ON t (pk2) ")

	// Deterministic data: duplicates, NULLs, boundaries.
	rng := rand.New(rand.NewPCG(7, 11))
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
			values = append(values, fmt.Sprintf("(%d, %d, %s, %s, %s, %s, %s)", pk1, pk2, mhcpkLit(a), mhcpkLit(b), mhcpkLit(s), mhcpkLit(f), mhcpkLit(d)))
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
	w.Exec("INSERT INTO t (pk1, pk2, a, b, s, f, d) VALUES " + strings.Join(values, ", "))

	queries := []mhcpkQuery{
		// Composite PK bounds.
		{"SELECT * FROM t WHERE pk1 = 1 AND pk2 > 3 ORDER BY pk1, pk2", true},
		{"SELECT * FROM t WHERE pk1 > 1 AND pk2 = 3 ORDER BY pk1, pk2", true},
		{"SELECT * FROM t WHERE pk1 IN (1, 3) AND pk2 = 2 ORDER BY pk1, pk2", true},
		{"SELECT * FROM t WHERE pk2 = 2 ORDER BY pk1", true},
		{"SELECT * FROM t WHERE pk1 = 2 ORDER BY pk2 DESC", true},
		{"SELECT * FROM t WHERE pk1 >= 5 ORDER BY pk1 DESC, pk2 DESC", true},
		{"SELECT * FROM t WHERE pk1 = 1 AND pk2 BETWEEN 2 AND 4 ORDER BY pk2", true},
		{"SELECT * FROM t WHERE pk1 = 1 OR pk2 = 1 ORDER BY pk1, pk2", true},
		{"SELECT * FROM t WHERE pk1 < 0 ORDER BY pk1, pk2", true},
		{"SELECT pk1, pk2 FROM t ORDER BY pk1, pk2 LIMIT 5 OFFSET 3", true},
		{"SELECT pk1, pk2 FROM t ORDER BY pk1 DESC, pk2 DESC LIMIT 4", true},
		{"SELECT pk1, pk2 FROM t WHERE pk1 = 1 ORDER BY pk2 DESC LIMIT 2 OFFSET 1", true},
		// Single-column index + residuals.
		{"SELECT * FROM t WHERE a = 1 AND b = 2", false},
		{"SELECT * FROM t WHERE a = 1 AND b IS NULL", false},
		{"SELECT * FROM t WHERE a IS NULL", false},
		{"SELECT * FROM t WHERE a IS NOT NULL AND s IS NULL", false},
		{"SELECT * FROM t WHERE a = -1", false},
		{"SELECT * FROM t WHERE a > 2", false},
		{"SELECT * FROM t WHERE a >= 9223372036854775807", false},
		{"SELECT * FROM t WHERE a <= -9223372036854775808", false},
		{"SELECT * FROM t WHERE a > -9223372036854775808 AND a < 9223372036854775807 AND a IS NOT NULL", false},
		{"SELECT * FROM t WHERE a <> 1", false},
		{"SELECT * FROM t WHERE NOT (a = 1)", false},
		{"SELECT * FROM t WHERE NOT (a = 1 OR a = 2)", false},
		{"SELECT * FROM t WHERE NOT (a = 1 AND b = 2)", false},
		{"SELECT * FROM t WHERE a IN (1, 1)", false},
		{"SELECT * FROM t WHERE a IN (1, NULL)", false},
		{"SELECT * FROM t WHERE a NOT IN (1, NULL)", false},
		{"SELECT * FROM t WHERE a NOT IN (1, 2)", false},
		{"SELECT * FROM t WHERE NOT (a IN (1, 2))", false},
		{"SELECT * FROM t WHERE a IN (3, 2, 1, 0, -1) ORDER BY a, pk1, pk2", true},
		{"SELECT * FROM t WHERE a IN (1, 2) ORDER BY a DESC, pk1, pk2", true},
		{"SELECT * FROM t WHERE a IN (2, 1) AND b IN (0, 1) ORDER BY a, b, pk1, pk2", true},
		// Cross-type comparisons on an indexed BIGINT column.
		{"SELECT * FROM t WHERE a = 1.0", false},
		{"SELECT * FROM t WHERE a = 1.5", false},
		{"SELECT * FROM t WHERE a > 1.5", false},
		{"SELECT * FROM t WHERE a >= 1.5", false},
		{"SELECT * FROM t WHERE a < 0.5", false},
		{"SELECT * FROM t WHERE a BETWEEN 0.5 AND 2.5", false},
		{"SELECT * FROM t WHERE a IN (1.0, 2.5)", false},
		{"SELECT * FROM t WHERE a > 1e19", false},
		{"SELECT * FROM t WHERE a < -1e19", false},
		{"SELECT * FROM t WHERE a < 1e19", false},
		// Indexed DOUBLE vs int literals.
		{"SELECT * FROM t WHERE d = 1", false},
		{"SELECT * FROM t WHERE d > 0", false},
		{"SELECT * FROM t WHERE d >= 0.5 AND d < 2", false},
		{"SELECT * FROM t WHERE d IN (0, 1, 0.5)", false},
		{"SELECT * FROM t WHERE d = 0.0", false},
		{"SELECT * FROM t WHERE d > -1e300", false},
		{"SELECT * FROM t WHERE d >= 1e300", false},
		{"SELECT * FROM t WHERE d IS NULL OR d = 2", false},
		{"SELECT * FROM t ORDER BY d, pk1, pk2", true},
		{"SELECT * FROM t ORDER BY d DESC NULLS LAST, pk1, pk2", true},
		{"SELECT * FROM t ORDER BY d NULLS LAST, pk1, pk2", true},
		{"SELECT * FROM t ORDER BY d DESC, pk1, pk2", true},
		// Strings.
		{"SELECT * FROM t WHERE s = ''", false},
		{"SELECT * FROM t WHERE s > 'b'", false},
		{"SELECT * FROM t WHERE s >= 'b' AND s < 'c'", false},
		{"SELECT * FROM t WHERE s LIKE 'b%'", false},
		{"SELECT * FROM t WHERE s LIKE 'b_'", false},
		{"SELECT * FROM t WHERE s LIKE 'b\\%' ESCAPE '\\'", false},
		{"SELECT * FROM t WHERE s LIKE 'b\\_' ESCAPE '\\'", false},
		{"SELECT * FROM t WHERE s LIKE '%a'", false},
		{"SELECT * FROM t WHERE s LIKE ' %'", false},
		{"SELECT * FROM t WHERE s LIKE '%'", false},
		{"SELECT * FROM t WHERE s NOT LIKE 'b%'", false},
		{"SELECT * FROM t WHERE s LIKE 'B%'", false},
		{"SELECT * FROM t WHERE s IN ('b', 'B', '')", false},
		{"SELECT * FROM t WHERE s = 'b' OR s = 'ba' ORDER BY s, pk1, pk2", true},
		{"SELECT * FROM t ORDER BY s, pk1, pk2", true},
		{"SELECT * FROM t ORDER BY s DESC, pk1 DESC, pk2 DESC", true},
		{"SELECT * FROM t WHERE s IS NOT NULL ORDER BY s DESC NULLS FIRST, pk1, pk2", true},
		// Booleans.
		{"SELECT * FROM t WHERE f", false},
		{"SELECT * FROM t WHERE NOT f", false},
		{"SELECT * FROM t WHERE f = TRUE", false},
		{"SELECT * FROM t WHERE f IS TRUE", false},
		{"SELECT * FROM t WHERE f IS NOT TRUE", false},
		{"SELECT * FROM t WHERE f IS FALSE", false},
		{"SELECT * FROM t WHERE f IS NOT FALSE", false},
		{"SELECT * FROM t WHERE f <> TRUE", false},
		{"SELECT * FROM t WHERE f IS NULL", false},
		{"SELECT * FROM t WHERE f = TRUE AND a = 1", false},
		{"SELECT * FROM t WHERE f = TRUE OR a = 1", false},
		{"SELECT * FROM t ORDER BY f, pk1, pk2", true},
		// 3-column composite index.
		{"SELECT * FROM t WHERE a = 1 AND s = 'b' AND b = 1", false},
		{"SELECT * FROM t WHERE a = 1 AND s = 'b' AND b > 0", false},
		{"SELECT * FROM t WHERE a = 1 AND s > 'b' AND b = 1", false},
		{"SELECT * FROM t WHERE a = 1 AND b = 1", false},
		{"SELECT * FROM t WHERE a = 1 AND s IS NULL AND b = 1", false},
		{"SELECT * FROM t WHERE a = 1 AND s IS NULL ORDER BY b, pk1, pk2", true},
		{"SELECT * FROM t WHERE a = 1 ORDER BY s, b, pk1, pk2", true},
		{"SELECT * FROM t WHERE a = 1 ORDER BY s DESC, b DESC, pk1 DESC, pk2 DESC", true},
		{"SELECT * FROM t WHERE a = 1 ORDER BY s, b DESC, pk1, pk2", true},
		{"SELECT * FROM t WHERE a = 1 ORDER BY s NULLS LAST, b, pk1, pk2", true},
		{"SELECT * FROM t WHERE a = 1 AND s = 'b' ORDER BY b DESC, pk1, pk2", true},
		{"SELECT * FROM t WHERE a IN (1, 2) AND s = 'b' ORDER BY a, b, pk1, pk2", true},
		{"SELECT * FROM t WHERE a IN (1, 2) AND s = 'b' ORDER BY b, pk1, pk2", true},
		{"SELECT * FROM t WHERE a = 1 AND s IN ('b', 'ba') ORDER BY s, b, pk1, pk2", true},
		{"SELECT * FROM t WHERE a = 1 AND s IN ('b', 'ba') AND b IN (0, 1) ORDER BY s, b, pk1, pk2", true},
		{"SELECT * FROM t WHERE a = 1 AND s = 'b' AND b = 1 ORDER BY pk1, pk2", true},
		{"SELECT * FROM t WHERE a = 1 AND s = 'b' AND b = 1 ORDER BY pk2, pk1", true},
		{"SELECT a, s, b FROM t WHERE a = 1 ORDER BY s, b, pk1, pk2", true},
		{"SELECT a, s, b FROM t WHERE a >= 1 ORDER BY a, s, b, pk1, pk2", true},
		{"SELECT a, s, b FROM t WHERE a >= 1 ORDER BY a DESC, s DESC, b DESC, pk1 DESC, pk2 DESC", true},
		{"SELECT a, s, b FROM t ORDER BY a, s, b, pk1, pk2", true},
		{"SELECT a, s, b FROM t ORDER BY a NULLS LAST, s NULLS LAST, b NULLS LAST, pk1, pk2", true},
		{"SELECT a, s, b FROM t WHERE a = 1 AND s > 'a' AND s < 'c' ORDER BY s, b, pk1, pk2", true},
		{"SELECT a, s, b, pk1, pk2 FROM t WHERE a = 1 AND s = 'b' AND b > 0 AND pk1 > 1 ORDER BY pk1, pk2", true},
		// Index containing a pk column.
		{"SELECT * FROM t WHERE b = 1 AND pk1 = 2", false},
		{"SELECT * FROM t WHERE a = 1 AND pk2 = 3", false},
		{"SELECT * FROM t WHERE a = 1 AND pk1 = 3", false},
		{"SELECT * FROM t WHERE a = 1 AND pk1 = 3 ORDER BY pk2", true},
		{"SELECT * FROM t WHERE s = 'b' AND pk2 = 3 ORDER BY pk1", true},
		{"SELECT * FROM t WHERE a = 1 AND s = 'b' AND pk2 = 3", false},
		{"SELECT * FROM t WHERE a = 1 AND b = 1 AND pk2 = 3", false},
		{"SELECT * FROM t WHERE b = 1 AND pk2 IN (3, 4)", false},
		{"SELECT * FROM t WHERE b IN (0, 1) AND pk2 = 3", false},
		{"SELECT * FROM t WHERE b = 1 OR pk2 = 3 ORDER BY pk1, pk2", true},
		{"SELECT * FROM t WHERE b = 1 OR pk2 = 3 ORDER BY pk1", false},
		{"SELECT * FROM t WHERE b = 1 OR pk2 = 3", false},
		{"SELECT * FROM t WHERE pk2 = 3 OR pk2 = 4 ORDER BY pk1", false},
		{"SELECT * FROM t WHERE a = 1 OR pk2 = 3 ORDER BY pk1", false},
		{"SELECT * FROM t WHERE (a = 1 OR pk2 = 3) AND b = 1 ORDER BY pk1", false},
		{"SELECT * FROM t WHERE d = 1 AND pk2 = 3", false},
		{"SELECT * FROM t WHERE f = TRUE AND pk2 = 3", false},
		{"SELECT COUNT(*) FROM t WHERE b = 1 AND pk2 = 3", false},
		{"SELECT pk1 FROM t WHERE b = 1 AND pk2 = 3", false},
		{"SELECT * FROM t WHERE b = 1 AND pk1 > 2 ORDER BY pk1, pk2", true},
		{"SELECT * FROM t WHERE b = 1 ORDER BY pk1, pk2", true},
		{"SELECT * FROM t WHERE b = 1 ORDER BY pk1 DESC, pk2 DESC", true},
		{"SELECT * FROM t WHERE b = 1 AND pk2 = 3", false},
		{"SELECT * FROM t WHERE b IN (0, 1) AND pk1 = 2 ORDER BY b, pk2", true},
		{"SELECT pk1, pk2 FROM t WHERE b = 1 AND pk1 = 2 AND pk2 > 1 ORDER BY pk2", true},
		{"SELECT b, pk1 FROM t WHERE b IS NULL ORDER BY pk1, pk2", true},
		{"SELECT * FROM t WHERE pk2 = 3 AND b = 1 ORDER BY pk1", true},
		{"SELECT * FROM t WHERE pk2 > 4 ORDER BY pk2, pk1", true},
		{"SELECT * FROM t WHERE pk2 IN (0, 6) ORDER BY pk2 DESC, pk1", true},
		// OR / union shapes.
		{"SELECT * FROM t WHERE a = 1 OR s = 'b' ORDER BY pk1, pk2", true},
		{"SELECT * FROM t WHERE a = 1 OR a = 2 OR s = 'b'", false},
		{"SELECT * FROM t WHERE (a = 1 OR s = 'b') AND b > 0", false},
		{"SELECT * FROM t WHERE (a = 1 OR s = 'b') AND (b = 1 OR d = 1)", false},
		{"SELECT * FROM t WHERE a = 1 OR b IS NULL", false},
		{"SELECT * FROM t WHERE a = 1 OR a IS NULL", false},
		{"SELECT * FROM t WHERE a > 1 OR a < 1", false},
		{"SELECT * FROM t WHERE a > 0 OR d > 0 ORDER BY pk1, pk2", true},
		{"SELECT * FROM t WHERE a = 1 OR s = 'b' ORDER BY pk1, pk2 LIMIT 3", true},
		{"SELECT pk1, pk2 FROM t WHERE a = 1 OR s = 'b' ORDER BY pk1 DESC, pk2 DESC LIMIT 3 OFFSET 2", true},
		{"SELECT * FROM t WHERE a = 1 OR s = 'b' ORDER BY a, pk1, pk2", true},
		{"SELECT * FROM t WHERE (a = 1 AND s = 'b') OR (a = 2 AND s = 'ba') ORDER BY a, s, b, pk1, pk2", true},
		{"SELECT * FROM t WHERE (a = 1 AND b = 1) OR (a = 1 AND b = 2)", false},
		{"SELECT * FROM t WHERE a = 1 AND (b = 1 OR b = 2)", false},
		{"SELECT * FROM t WHERE a = 1 AND (b = 1 OR s = 'b')", false},
		{"SELECT * FROM t WHERE (a = 1 OR a = 2) AND (s = 'b' OR s = 'ba') ORDER BY a, s, pk1, pk2", true},
		{"SELECT DISTINCT a FROM t WHERE a = 1 OR s = 'b'", false},
		{"SELECT DISTINCT a, s FROM t ORDER BY a, s", true},
		{"SELECT DISTINCT a FROM t ORDER BY a DESC", true},
		{"SELECT DISTINCT a FROM t ORDER BY a NULLS LAST LIMIT 3", true},
		{"SELECT DISTINCT b FROM t WHERE a = 1 ORDER BY b", true},
		{"SELECT DISTINCT s FROM t WHERE a = 1 ORDER BY s", true},
		{"SELECT DISTINCT pk1 FROM t WHERE b = 1 ORDER BY pk1", true},
		// Constant / tautological predicates with NULL semantics.
		{"SELECT * FROM t WHERE a = a", false},
		{"SELECT * FROM t WHERE a <> a", false},
		{"SELECT * FROM t WHERE a = b", false},
		{"SELECT * FROM t WHERE a < b", false},
		{"SELECT * FROM t WHERE a = pk1", false},
		{"SELECT * FROM t WHERE a IN (b, pk1)", false},
		{"SELECT * FROM t WHERE 1 = 1 AND a = 1", false},
		{"SELECT * FROM t WHERE 1 = 0 OR a = 1", false},
		{"SELECT * FROM t WHERE NULL IS NULL AND a = 1", false},
		{"SELECT * FROM t WHERE a = NULL", false},
		{"SELECT * FROM t WHERE NOT (a = NULL)", false},
		{"SELECT * FROM t WHERE a IS DISTINCT FROM 1", false},
		{"SELECT * FROM t WHERE a IS NOT DISTINCT FROM NULL", false},
		{"SELECT * FROM t WHERE a IS NOT DISTINCT FROM b", false},
		{"SELECT * FROM t WHERE COALESCE(a, 0) = 0", false},
		{"SELECT * FROM t WHERE COALESCE(a, b) = 1", false},
		{"SELECT * FROM t WHERE a + 1 = 2", false},
		{"SELECT * FROM t WHERE a + b > 2", false},
		{"SELECT * FROM t WHERE -a = -1", false},
		{"SELECT * FROM t WHERE a * 0 = 0", false},
		{"SELECT * FROM t WHERE a BETWEEN b AND 2", false},
		{"SELECT * FROM t WHERE a BETWEEN 2 AND 1", false},
		{"SELECT * FROM t WHERE a BETWEEN NULL AND 2", false},
		{"SELECT * FROM t WHERE a NOT BETWEEN 1 AND 2", false},
		{"SELECT * FROM t WHERE CASE WHEN a = 1 THEN TRUE ELSE FALSE END", false},
		{"SELECT * FROM t WHERE CASE WHEN a = 1 THEN b ELSE 0 END = 1", false},
		{"SELECT * FROM t WHERE (a = 1) = (b = 1)", false},
		{"SELECT * FROM t WHERE (a = 1) IS NULL", false},
		{"SELECT * FROM t WHERE (a > 1) IS NOT TRUE", false},
		{"SELECT * FROM t WHERE a = 1 XOR b = 1", false},
		// Aggregates.
		{"SELECT a, COUNT(*) FROM t GROUP BY a ORDER BY a", true},
		{"SELECT a, COUNT(b) FROM t GROUP BY a ORDER BY a", true},
		{"SELECT a, SUM(b) FROM t GROUP BY a ORDER BY a", true},
		{"SELECT a, s, COUNT(*) FROM t GROUP BY a, s ORDER BY a, s", true},
		{"SELECT a, s, MAX(b), MIN(b) FROM t WHERE a = 1 GROUP BY a, s ORDER BY s", true},
		{"SELECT s, COUNT(*) FROM t WHERE a = 1 GROUP BY s ORDER BY s", true},
		{"SELECT a, COUNT(*) FROM t WHERE s = 'b' GROUP BY a ORDER BY a", true},
		{"SELECT a, COUNT(*) FROM t GROUP BY a HAVING COUNT(*) > 5 ORDER BY a", true},
		{"SELECT a, SUM(d) FROM t GROUP BY a ORDER BY a", true},
		{"SELECT COUNT(*) FROM t WHERE a = 1 AND s = 'b'", false},
		{"SELECT COUNT(*) FROM t WHERE a = 100", false},
		{"SELECT MAX(a), MIN(a) FROM t WHERE a = 100", false},
		{"SELECT SUM(a) FROM t WHERE a = 100", false},
		{"SELECT MAX(s), MIN(s) FROM t", false},
		{"SELECT MAX(pk2), MIN(pk2) FROM t WHERE pk1 = 1", false},
		{"SELECT MAX(a) FROM t", false},
		{"SELECT MIN(a) FROM t", false},
		{"SELECT MAX(d), MIN(d) FROM t", false},
		{"SELECT b, MAX(pk1) FROM t GROUP BY b ORDER BY b", true},
		{"SELECT a, COUNT(*) FROM t WHERE a IN (1, 2) GROUP BY a ORDER BY a", true},
		{"SELECT a, COUNT(*) FROM t WHERE a = 1 OR s = 'b' GROUP BY a ORDER BY a", true},
		{"SELECT pk1, COUNT(*) FROM t GROUP BY pk1 ORDER BY pk1", true},
		{"SELECT pk1, COUNT(*) FROM t WHERE pk1 > 3 GROUP BY pk1 ORDER BY pk1", true},
		{"SELECT a, COUNT(*) FROM t GROUP BY a ORDER BY COUNT(*) DESC, a", true},
		{"SELECT COUNT(*), SUM(a), AVG(a) FROM t", false},
		{"SELECT AVG(b) FROM t WHERE a = 1", false},
		// Joins (self).
		{"SELECT l.pk1, l.pk2, r.pk1, r.pk2 FROM t AS l JOIN t AS r ON l.a = r.b WHERE l.pk1 = 1 ORDER BY l.pk1, l.pk2, r.pk1, r.pk2", true},
		{"SELECT l.pk1, l.pk2, r.pk1, r.pk2 FROM t AS l JOIN t AS r ON l.a = r.a WHERE l.pk1 = 1 AND r.s = 'b' ORDER BY l.pk1, l.pk2, r.pk1, r.pk2", true},
		{"SELECT l.pk1, l.pk2, r.pk1, r.pk2 FROM t AS l JOIN t AS r ON l.pk1 = r.pk1 AND l.pk2 = r.pk2 WHERE l.a = 1 ORDER BY l.pk1, l.pk2", true},
		{"SELECT l.pk1, l.pk2, r.pk1, r.pk2 FROM t AS l JOIN t AS r ON l.pk1 = r.pk2 WHERE l.a = 1 AND r.b = 1 ORDER BY l.pk1, l.pk2, r.pk1, r.pk2", true},
		{"SELECT l.pk1, l.pk2, r.pk1, r.pk2 FROM t AS l LEFT JOIN t AS r ON l.a = r.a AND r.b = 1 WHERE l.pk1 = 1 ORDER BY l.pk1, l.pk2, r.pk1, r.pk2", true},
		{"SELECT l.pk1, l.pk2, r.pk1, r.pk2 FROM t AS l LEFT JOIN t AS r ON l.a = r.a WHERE l.pk1 = 1 AND r.b = 1 ORDER BY l.pk1, l.pk2, r.pk1, r.pk2", true},
		{"SELECT l.pk1, l.pk2, r.pk1, r.pk2 FROM t AS l LEFT JOIN t AS r ON l.a = r.a WHERE l.pk1 = 1 AND r.pk1 IS NULL ORDER BY l.pk1, l.pk2", true},
		{"SELECT l.pk1, l.pk2, r.pk1, r.pk2 FROM t AS l LEFT JOIN t AS r ON l.s = r.s AND l.a = r.a WHERE l.pk1 = 1 ORDER BY l.pk1, l.pk2, r.pk1, r.pk2", true},
		{"SELECT l.pk1, l.pk2, r.pk1, r.pk2 FROM t AS l JOIN t AS r ON l.a < r.a WHERE l.pk1 = 1 AND r.pk1 = 2 ORDER BY l.pk1, l.pk2, r.pk1, r.pk2", true},
		{"SELECT l.pk1, l.pk2, r.pk1, r.pk2 FROM t AS l JOIN t AS r ON l.a = r.a WHERE l.pk1 = 1 AND r.a = 2 ORDER BY l.pk1, l.pk2, r.pk1, r.pk2", true},
		{"SELECT l.pk1, l.pk2, r.pk1, r.pk2 FROM t AS l JOIN t AS r ON l.a = r.a AND l.s = r.s AND l.b = r.b WHERE l.pk1 = 1 ORDER BY l.pk1, l.pk2, r.pk1, r.pk2", true},
		{"SELECT COUNT(*) FROM t AS l JOIN t AS r ON l.a = r.a WHERE l.pk1 = 1", false},
		{"SELECT l.a, COUNT(*) FROM t AS l JOIN t AS r ON l.a = r.a WHERE l.pk1 = 1 GROUP BY l.a ORDER BY l.a", true},
		{"SELECT l.pk1, l.pk2 FROM t AS l JOIN t AS r ON l.a = r.a WHERE l.pk1 = 1 ORDER BY l.pk1, l.pk2, r.pk1, r.pk2 LIMIT 3", true},
		// Subqueries.
		{"SELECT * FROM t AS o WHERE EXISTS (SELECT 1 FROM t AS i WHERE i.a = o.b AND i.s = 'b') ORDER BY pk1, pk2", true},
		{"SELECT * FROM t AS o WHERE NOT EXISTS (SELECT 1 FROM t AS i WHERE i.a = o.b AND i.s = 'b') ORDER BY pk1, pk2", true},
		{"SELECT * FROM t AS o WHERE EXISTS (SELECT 1 FROM t AS i WHERE i.pk1 = o.pk2 AND i.a = 1) ORDER BY pk1, pk2", true},
		{"SELECT * FROM t WHERE a IN (SELECT b FROM t WHERE s = 'b') ORDER BY pk1, pk2", true},
		{"SELECT * FROM t WHERE a NOT IN (SELECT b FROM t WHERE s = 'b') ORDER BY pk1, pk2", true},
		{"SELECT * FROM t WHERE a NOT IN (SELECT b FROM t WHERE s = 'alpha') ORDER BY pk1, pk2", true},
		{"SELECT * FROM t WHERE a IN (SELECT b FROM t WHERE s = 'nonexistent') ORDER BY pk1, pk2", true},
		{"SELECT * FROM t WHERE a NOT IN (SELECT b FROM t WHERE s = 'nonexistent') ORDER BY pk1, pk2", true},
		{"SELECT * FROM t WHERE a = (SELECT MAX(b) FROM t) ORDER BY pk1, pk2", true},
		{"SELECT * FROM t WHERE a = (SELECT MAX(a) FROM t WHERE a = 100) ORDER BY pk1, pk2", true},
		{"SELECT * FROM t WHERE a > (SELECT COUNT(*) FROM t WHERE a = 100) ORDER BY pk1, pk2", true},
		{"SELECT * FROM t WHERE a = (SELECT MIN(a) FROM t WHERE s = 'b') ORDER BY pk1, pk2", true},
		{"SELECT * FROM t AS o WHERE a = (SELECT MAX(i.b) FROM t AS i WHERE i.pk1 = o.pk1) ORDER BY pk1, pk2", true},
		// Unions.
		{"SELECT pk1, pk2 FROM t WHERE a = 1 UNION SELECT pk1, pk2 FROM t WHERE s = 'b'", false},
		{"SELECT pk1, pk2 FROM t WHERE a = 1 UNION ALL SELECT pk1, pk2 FROM t WHERE s = 'b'", false},
		{"SELECT a FROM t WHERE a = 1 UNION SELECT b FROM t WHERE s = 'b'", false},
		{"SELECT a, s FROM t WHERE a = 1 UNION SELECT a, s FROM t WHERE a IS NULL", false},
		{"SELECT a FROM t WHERE pk1 = 1 UNION SELECT a FROM t WHERE pk1 = 2 ORDER BY a", true},
		// Derived tables.
		{"SELECT * FROM (SELECT a, b FROM t WHERE a = 1) AS x WHERE b = 1", false},
		{"SELECT * FROM (SELECT a, b, s FROM t WHERE a = 1 ORDER BY s, b, pk1, pk2 LIMIT 3) AS x WHERE b IS NOT NULL", false},
		{"SELECT * FROM (SELECT pk1, pk2 FROM t ORDER BY pk1, pk2 LIMIT 4) AS x ORDER BY pk1 DESC, pk2 DESC", true},
		{"SELECT * FROM (SELECT pk1, pk2 FROM t ORDER BY pk1, pk2 LIMIT 4 OFFSET 2) AS x ORDER BY pk1, pk2 LIMIT 2", true},
		{"SELECT * FROM (SELECT pk1, pk2 FROM t WHERE a = 1 ORDER BY pk1, pk2 LIMIT 4) AS x WHERE pk1 > 0 ORDER BY pk1, pk2", true},
		{"SELECT x.a, x.n FROM (SELECT a, COUNT(*) AS n FROM t GROUP BY a) AS x WHERE x.n > 3 ORDER BY x.a", true},
		{"SELECT x.a FROM (SELECT DISTINCT a FROM t WHERE s = 'b') AS x ORDER BY x.a", true},
		{"SELECT COUNT(*) FROM (SELECT pk1 FROM t WHERE a = 1 ORDER BY pk1, pk2 LIMIT 3) AS x", false},
		{"SELECT COUNT(*) FROM (SELECT DISTINCT a FROM t) AS x", false},
		{"SELECT * FROM (SELECT a, b FROM t WHERE a = 1 UNION SELECT a, b FROM t WHERE s = 'b') AS x WHERE b = 1", false},
	}

	// Population counters. Every degenerate outcome (both sides error, both
	// return nothing, the paged read declines) is legitimate for SOME query in
	// the list — but a run in which most queries land there has compared
	// nothing, and would otherwise be green. The floors at the end state the
	// population this net actually measures.
	var compared, nonEmpty, bothErrored, pagedCompared, pagingDeclined int
	for _, q := range queries {
		c, ne := mhcpkCompare(w, "read", q)
		if !c {
			bothErrored++
			continue
		}
		compared++
		if ne {
			nonEmpty++
		}
	}

	// Paging variant on the indexed side: a pinned connection with a tiny
	// scanned-rows limit so every query pages through continuations.
	conn, err := w.idx.Conn(ctx)
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
		full, err := mmRows(t, ctx, w.idx, q.sql)
		if err != nil {
			continue
		}
		paged, err := mhcpkRowsOnConn(ctx, conn, q.sql)
		if err != nil {
			if strings.Contains(err.Error(), "54F01") {
				pagingDeclined++
				t.Logf("paging decline (54F01): %s", q.sql)
				continue
			}
			t.Errorf("PAGING ERROR\n  q: %s\n  err: %v", q.sql, err)
			continue
		}
		pagedCompared++
		sf, sp := append([]string(nil), full...), append([]string(nil), paged...)
		if !q.ordered {
			sort.Strings(sf)
			sort.Strings(sp)
		}
		if !mmEqRows(sf, sp) {
			t.Errorf("PAGING DIVERGENCE (ordered=%v)\n  q: %s\n  plan: %s\n  full  (%d): %v\n  paged (%d): %v",
				q.ordered, q.sql, w.Explain(q.sql), len(full), full, len(paged), paged)
		}
	}
	t.Logf("composite-pk read sweep: %d queries; compared=%d nonEmpty=%d bothErrored=%d pagedCompared=%d pagingDeclined=%d",
		len(queries), compared, nonEmpty, bothErrored, pagedCompared, pagingDeclined)

	// Non-vacuity floors, measured at 258 queries: compared=241 nonEmpty=224
	// bothErrored=17 pagedCompared=239 pagingDeclined=2. Each floor sits far
	// enough below its reading to absorb a query or two changing class, and
	// far enough above zero that a net comparing nothing cannot pass.
	if compared < 230 {
		t.Errorf("only %d of %d queries were compared across the twins (measured 241 at 258 queries); the net is not measuring what it claims", compared, len(queries))
	}
	if nonEmpty < 200 {
		t.Errorf("only %d compared queries returned rows (measured 224 at 258 queries); an empty fixture agrees with itself on everything", nonEmpty)
	}
	if bothErrored > 25 {
		t.Errorf("%d queries errored on both schemas (measured 17 at 258 queries); a parse/plan regression is being logged as agreement", bothErrored)
	}
	if pagedCompared < 220 {
		t.Errorf("only %d paged re-reads were compared (measured 239 at 258 queries); the continuation axis is not being exercised", pagedCompared)
	}
	if pagingDeclined > 10 {
		t.Errorf("%d paged re-reads declined with 54F01 (measured 2 at 258 queries); the scan limit is silencing the continuation axis", pagingDeclined)
	}
}

// mhcpkRowsOnConn is mmRows over a pinned *sql.Conn (the paged reader).
func mhcpkRowsOnConn(ctx context.Context, conn *sql.Conn, q string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		cells := make([]any, len(cols))
		for i := range cells {
			cells[i] = new(sql.NullString)
		}
		if err := rows.Scan(cells...); err != nil {
			return nil, err
		}
		parts := make([]string, len(cells))
		for i, c := range cells {
			v := c.(*sql.NullString)
			if v.Valid {
				parts[i] = v.String
			} else {
				parts[i] = "NULL"
			}
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func TestFDB_MetamorphicCompositePrimaryKeyDML(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_mhcpkdml", "mhcpkdml", mhcpkTable,
		"CREATE INDEX t_a ON t (a) "+
			"CREATE INDEX t_s ON t (s) "+
			"CREATE INDEX t_asb ON t (a, s, b) "+
			"CREATE INDEX t_b_pk1 ON t (b, pk1) "+
			"CREATE INDEX t_d ON t (d) "+
			"CREATE INDEX t_pk2 ON t (pk2) "+
			"CREATE INDEX t_cnt_a AS SELECT COUNT(*) FROM t GROUP BY a "+
			// b is NOT NULL by construction below: SUM over a nullable column
			// is the pinned sumResidualZero divergence, which a sweep that kept
			// walking into it would report on every statement.
			"CREATE INDEX t_sum_b_by_s AS SELECT SUM(b) FROM t GROUP BY s "+
			"CREATE INDEX t_max_d_by_a AS SELECT MAX(d) FROM t GROUP BY a ")

	rng := rand.New(rand.NewPCG(3, 5))
	var values []string
	strDomain := []any{"", "alpha", "beta", "b", "ba"}
	for pk1 := int64(0); pk1 < 5; pk1++ {
		for pk2 := int64(0); pk2 < 6; pk2++ {
			var a, s, f, d any
			if rng.IntN(5) != 0 {
				a = int64(rng.IntN(4))
			}
			b := int64(rng.IntN(3))
			if rng.IntN(5) != 0 {
				s = strDomain[rng.IntN(len(strDomain))]
			}
			if rng.IntN(5) != 0 {
				f = rng.IntN(2) == 0
			}
			if rng.IntN(5) != 0 {
				d = float64(rng.IntN(3))
			}
			values = append(values, fmt.Sprintf("(%d, %d, %s, %d, %s, %s, %s)", pk1, pk2, mhcpkLit(a), b, mhcpkLit(s), mhcpkLit(f), mhcpkLit(d)))
		}
	}
	w.Exec("INSERT INTO t (pk1, pk2, a, b, s, f, d) VALUES " + strings.Join(values, ", "))

	reads := []mhcpkQuery{
		{"SELECT * FROM t ORDER BY pk1, pk2", true},
		{"SELECT * FROM t WHERE a = 1 ORDER BY pk1, pk2", true},
		{"SELECT * FROM t WHERE a = 5 ORDER BY pk1, pk2", true},
		{"SELECT * FROM t WHERE a IS NULL ORDER BY pk1, pk2", true},
		{"SELECT * FROM t WHERE s = 'b' ORDER BY pk1, pk2", true},
		{"SELECT * FROM t WHERE s = 'zz' ORDER BY pk1, pk2", true},
		{"SELECT * FROM t WHERE a = 1 AND s = 'b' ORDER BY b, pk1, pk2", true},
		{"SELECT * FROM t WHERE b = 1 ORDER BY pk1, pk2", true},
		{"SELECT * FROM t WHERE b = 1 AND pk1 = 2 ORDER BY pk2", true},
		{"SELECT * FROM t WHERE d = 1 ORDER BY pk1, pk2", true},
		{"SELECT * FROM t WHERE d IS NULL ORDER BY pk1, pk2", true},
		{"SELECT * FROM t WHERE pk2 = 3 ORDER BY pk1", true},
		{"SELECT a, COUNT(*) FROM t GROUP BY a ORDER BY a", true},
		{"SELECT s, SUM(b) FROM t GROUP BY s ORDER BY s", true},
		{"SELECT a, MAX(d) FROM t GROUP BY a ORDER BY a", true},
		{"SELECT COUNT(*) FROM t WHERE a = 1", false},
		{"SELECT SUM(b) FROM t WHERE s = 'b'", false},
		{"SELECT MAX(d) FROM t WHERE a = 2", false},
		{"SELECT a, s, b FROM t ORDER BY a, s, b, pk1, pk2", true},
	}

	dml := []string{
		"UPDATE t SET a = 5 WHERE b = 1 AND pk2 = 3",
		"UPDATE t SET a = a + 1 WHERE a = 1",
		"UPDATE t SET s = 'zz' WHERE a = 2 AND s = 'b'",
		"UPDATE t SET b = 0 WHERE s = 'alpha'",
		"UPDATE t SET d = NULL WHERE d = 2",
		"UPDATE t SET d = 7.5 WHERE d IS NULL AND a = 0",
		"UPDATE t SET a = NULL WHERE pk1 = 1",
		"UPDATE t SET s = NULL, b = 2 WHERE pk2 = 0",
		"UPDATE t SET b = 0 - b WHERE a = 3",
		"UPDATE t SET a = 3, s = 'b', b = 1 WHERE pk1 = 4 AND pk2 > 2",
		"DELETE FROM t WHERE a = 5",
		"DELETE FROM t WHERE s = 'zz' AND pk1 = 2",
		"DELETE FROM t WHERE pk1 = 0 AND pk2 IN (1, 3)",
		"DELETE FROM t WHERE b = 0 AND d IS NULL",
		"INSERT INTO t (pk1, pk2, a, b, s, f, d) VALUES (9, 9, 1, 1, 'b', TRUE, 1.0), (9, 8, NULL, 0, NULL, NULL, NULL)",
		"UPDATE t SET a = 1 WHERE pk1 = 9",
		"DELETE FROM t WHERE a = 1 AND s = 'b' AND b = 1",
		"UPDATE t SET b = b + 10 WHERE b >= 0",
		"DELETE FROM t WHERE a > 1 OR s = 'beta'",
	}

	var compared, bothErrored int
	sweep := func(stage string) {
		t.Helper()
		for _, q := range reads {
			if c, _ := mhcpkCompare(w, stage, q); c {
				compared++
			} else {
				bothErrored++
			}
		}
	}
	sweep("initial")
	for i, stmt := range dml {
		ri, ei := w.idx.ExecContext(ctx, stmt)
		rn, en := w.plain.ExecContext(ctx, stmt)
		if (ei == nil) != (en == nil) {
			t.Fatalf("dml %d: DML asymmetry\n  stmt: %s\n  indexed:   %v\n  unindexed: %v", i, stmt, ei, en)
		}
		if ei != nil {
			t.Fatalf("dml %d failed on both schemas: %s: %v", i, stmt, ei)
		}
		ai, _ := ri.RowsAffected()
		an, _ := rn.RowsAffected()
		if ai != an {
			t.Errorf("dml %d: rows affected diverge: indexed=%d unindexed=%d\n  stmt: %s", i, ai, an, stmt)
		}
		sweep(fmt.Sprintf("after dml %d (%s)", i, stmt))
	}
	t.Logf("composite-pk DML sweep: %d statements x %d reads; compared=%d bothErrored=%d", len(dml), len(reads), compared, bothErrored)
	// Measured: 19 statements, 19 reads, 20 sweeps → compared=380 bothErrored=0.
	if want := (len(dml) + 1) * len(reads); compared != want {
		t.Errorf("compared %d reads, want every one of %d (bothErrored=%d); a read that errors on both schemas is not comparing anything", compared, want, bothErrored)
	}
}
