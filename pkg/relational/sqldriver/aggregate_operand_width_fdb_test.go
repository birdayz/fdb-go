package sqldriver_test

// The SUM/AVG operand WIDTH decision
// (cascades_translator.go aggregateOperandIntType).
//
// The width is not cosmetic: values.TypeCodeInt selects Java's SUM_I / AVG_I,
// which overflow at the int32 boundary and raise 22003, while anything else
// keeps SUM_L and answers in int64. Picking the wrong width therefore changes
// an ARITHMETIC RESULT — a query either errors or returns a number — and no
// plan golden can see it.
//
// The AUTHORITY is the operand's own static type, which is Java's rule
// (NumericAggregationValue.encapsulate keys the operator map on the operand's
// static TypeCode, NumericAggregationValue.java:196-209) and which survives a
// join because the resolved reference carries its column's own type. An
// UNTYPED operand (a minted bare-column carrier) falls back to an ordinal
// lookup in the aggregate input's proto-faithful column list, and two facts
// shape that fallback, each pinned here:
//
//  1. WHICH column, within one layout, is the ordinal — so a schema is used
//     where INTEGER and BIGINT columns interleave, and every one of them is
//     summed. Any off-by-one, any "first INTEGER wins", and any constant
//     answer flips at least one case.
//  2. WHICH LAYOUT the ordinal indexes. The operand is baked against the
//     translated input expression's output columns; the width list is derived
//     separately from the logical input's leg columns. A join operand carries
//     its own LEG's layout while the width list is the merged, qualified row —
//     so an ordinal taken across the two would read a different column
//     entirely. Here that column is an INTEGER while the summed one is a
//     BIGINT, so the mistake would manufacture a 22003 out of a query that
//     must answer.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// aggWidthDB creates a database + schema from the given table DDL and returns
// an open connection to it.
func aggWidthDB(t *testing.T, ctx context.Context, tag, tables string) *sql.DB {
	t.Helper()
	dbPath := fmt.Sprintf("/aggwidth_%s", tag)
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	tmpl := fmt.Sprintf("aggwidth_tmpl_%s", tag)
	if _, err := setup.ExecContext(ctx, fmt.Sprintf(
		"CREATE SCHEMA TEMPLATE %s %s", tmpl, tables)); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/store WITH TEMPLATE %s", dbPath, tmpl)); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}
	db, err := sql.Open("fdbsql", fmt.Sprintf(
		"fdbsql://%s?cluster_file=%s&schema=store", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// sumOutcome runs a one-column query and reports "22003", "ok:<value>", or
// "err:<message>". Errors surface both from QueryContext and from rows.Err,
// depending on where the cursor faults.
func sumOutcome(t *testing.T, ctx context.Context, db *sql.DB, q string) string {
	t.Helper()
	classify := func(err error) string {
		if strings.Contains(err.Error(), "22003") {
			return "22003"
		}
		return "err:" + err.Error()
	}
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return classify(err)
	}
	defer rows.Close()
	var got sql.NullInt64
	for rows.Next() {
		if serr := rows.Scan(&got); serr != nil {
			return classify(serr)
		}
	}
	if rerr := rows.Err(); rerr != nil {
		return classify(rerr)
	}
	return fmt.Sprintf("ok:%d", got.Int64)
}

// TestFDB_AggregateOperandWidthIsPositional sums every column of a table whose
// INTEGER and BIGINT columns interleave. Each row holds 2_000_000_000 — inside
// int32 — so a two-row SUM of 4_000_000_000 overflows int32 and not int64:
// the INTEGER columns must raise 22003 and the BIGINT columns must answer.
//
// The interleaving is the point. Against a width taken one slot off, or taken
// from the first INTEGER in the list, or hard-wired, at least one of the four
// columns comes out on the wrong side.
func TestFDB_AggregateOperandWidthIsPositional(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := aggWidthDB(t, ctx, "positional", "CREATE TABLE w ("+
		"id BIGINT NOT NULL, a BIGINT, v INTEGER, b BIGINT, u INTEGER, PRIMARY KEY (id))")

	for i := 1; i <= 2; i++ {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO w VALUES (%d, 2000000000, CAST(2000000000 AS INTEGER), 2000000000, CAST(2000000000 AS INTEGER))", i)); err != nil {
			t.Fatalf("INSERT %d: %v", i, err)
		}
	}

	for _, tc := range []struct {
		col     string
		ordinal int
		want    string
	}{
		{col: "a", ordinal: 1, want: "ok:4000000000"},
		{col: "v", ordinal: 2, want: "22003"},
		{col: "b", ordinal: 3, want: "ok:4000000000"},
		{col: "u", ordinal: 4, want: "22003"},
	} {
		t.Run("SUM_"+tc.col, func(t *testing.T) {
			t.Parallel()
			q := fmt.Sprintf("SELECT SUM(%s) FROM w", tc.col)
			if got := sumOutcome(t, ctx, db, q); got != tc.want {
				t.Fatalf("%s = %s, want %s — column %q is at ordinal %d of the aggregate input's row; a width read at any other slot lands on a column of the other integer kind",
					q, got, tc.want, tc.col, tc.ordinal)
			}
		})
	}
}

// TestFDB_AggregateOperandWidthDeclinesForeignLayout sums a BIGINT column of a
// join leg. The operand's ordinal is stated in ITS OWN leg's row — position 1
// of (id2, y) — while the width-fallback list is the merged, qualified join
// row, whose position 1 is the OTHER leg's INTEGER column. Reading the ordinal
// across the two layouts would narrow a BIGINT sum to int32 and turn an
// answerable query into 22003.
//
// The operand's own static type (BIGINT → SUM_L) answers first; if that ever
// degrades to Unknown, the fallback must still DECLINE this reference rather
// than index the foreign layout — either way a wrong narrowing never happens.
func TestFDB_AggregateOperandWidthDeclinesForeignLayout(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := aggWidthDB(t, ctx, "foreign", ""+
		"CREATE TABLE j1 (id BIGINT NOT NULL, x INTEGER, PRIMARY KEY (id)) "+
		"CREATE TABLE j2 (id2 BIGINT NOT NULL, y BIGINT, PRIMARY KEY (id2))")

	for i := 1; i <= 2; i++ {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO j1 VALUES (%d, CAST(1 AS INTEGER))", i)); err != nil {
			t.Fatalf("INSERT j1 %d: %v", i, err)
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO j2 VALUES (%d, 2000000000)", i)); err != nil {
			t.Fatalf("INSERT j2 %d: %v", i, err)
		}
	}

	q := `SELECT SUM(j2.y) FROM j1, j2 WHERE j1.id = j2.id2`
	if got := sumOutcome(t, ctx, db, q); got != "ok:4000000000" {
		t.Fatalf("%s = %s, want ok:4000000000 — the operand's ordinal was read in the merged join row, where that slot is j1.x (INTEGER), instead of in the leg layout the operand was baked against",
			q, got)
	}
}

// sumErrText runs a one-column query expected to ERROR and returns the full
// error text, so a caller can pin both the SQLSTATE and Java's verbatim
// overflow message. An answering query returns "ok:<value>".
func sumErrText(t *testing.T, ctx context.Context, db *sql.DB, q string) string {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return err.Error()
	}
	defer rows.Close()
	var got sql.NullInt64
	for rows.Next() {
		if serr := rows.Scan(&got); serr != nil {
			return serr.Error()
		}
	}
	if rerr := rows.Err(); rerr != nil {
		return rerr.Error()
	}
	return fmt.Sprintf("ok:%d", got.Int64)
}

// TestFDB_AggregateOperandWidthJoinLegRaises pins that summing an INTEGER
// column overflows at the int32 boundary and raises 22003 IDENTICALLY with and
// without a join around the operand's table. Java raises for both:
// NumericAggregationValue.encapsulate picks SUM_I from the operand's static
// TypeCode (NumericAggregationValue.java:196-209), which survives a join
// because the reference carries its column's own type — and since RFC-181
// P0.5's width-faithful typing Go's resolved references do too, so the width
// no longer depends on the merged-row ordinal lookup that a join-leg reference
// cannot index.
//
// This test previously pinned the OPPOSITE on the joined case — the silent
// int64 answer — as a measured gap vs Java (its red was the signal the gap
// closed). The gap is now closed; a silent ok:4000000000 here is the old bug
// back.
//
// The message is pinned too: Java's Math.addExact(int, int) throws
// ArithmeticException("integer overflow"), not the long overload's
// "long overflow".
func TestFDB_AggregateOperandWidthJoinLegRaises(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := aggWidthDB(t, ctx, "javagap", ""+
		"CREATE TABLE j1 (id BIGINT NOT NULL, x INTEGER, PRIMARY KEY (id)) "+
		"CREATE TABLE j2 (id2 BIGINT NOT NULL, y BIGINT, PRIMARY KEY (id2))")

	for i := 1; i <= 2; i++ {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO j1 VALUES (%d, CAST(2000000000 AS INTEGER))", i)); err != nil {
			t.Fatalf("INSERT j1 %d: %v", i, err)
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO j2 VALUES (%d, 1)", i)); err != nil {
			t.Fatalf("INSERT j2 %d: %v", i, err)
		}
	}

	for _, q := range []string{
		`SELECT SUM(j1.x) FROM j1, j2 WHERE j1.id = j2.id2`,
		`SELECT SUM(x) FROM j1`,
	} {
		got := sumErrText(t, ctx, db, q)
		if !strings.Contains(got, "22003") || !strings.Contains(got, "integer overflow") {
			t.Fatalf("%s = %q, want 22003 %q on BOTH the joined and the standalone path — a silent number on the joined one is the width degrading through the join leg again",
				q, got, "integer overflow")
		}
	}
}

// TestFDB_AggregateOperandWidthNegativeOverflow pins the NEGATIVE direction of
// the int32 check on both paths: Java's Math.addExact raises on underflow
// exactly as on overflow, and a check written only as `s > MaxInt32` passes
// this sum silently.
func TestFDB_AggregateOperandWidthNegativeOverflow(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := aggWidthDB(t, ctx, "negdir", ""+
		"CREATE TABLE j1 (id BIGINT NOT NULL, x INTEGER, PRIMARY KEY (id)) "+
		"CREATE TABLE j2 (id2 BIGINT NOT NULL, y BIGINT, PRIMARY KEY (id2))")

	for i := 1; i <= 2; i++ {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO j1 VALUES (%d, CAST(-2000000000 AS INTEGER))", i)); err != nil {
			t.Fatalf("INSERT j1 %d: %v", i, err)
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO j2 VALUES (%d, 1)", i)); err != nil {
			t.Fatalf("INSERT j2 %d: %v", i, err)
		}
	}

	for _, q := range []string{
		`SELECT SUM(j1.x) FROM j1, j2 WHERE j1.id = j2.id2`,
		`SELECT SUM(x) FROM j1`,
	} {
		if got := sumOutcome(t, ctx, db, q); got != "22003" {
			t.Fatalf("%s = %s, want 22003 — -4000000000 is below int32 min; a pass here means the underflow arm of the int32 check is gone",
				q, got)
		}
	}
}

// TestFDB_AggregateOperandWidthInt32BoundaryExact pins that a sum landing
// EXACTLY on the int32 boundaries answers — the checked lane must not
// manufacture a false positive at MaxInt32 or MinInt32 — on both the joined
// and the standalone path.
func TestFDB_AggregateOperandWidthInt32BoundaryExact(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := aggWidthDB(t, ctx, "i32edge", ""+
		"CREATE TABLE jmax (id BIGINT NOT NULL, x INTEGER, PRIMARY KEY (id)) "+
		"CREATE TABLE jmin (id BIGINT NOT NULL, x INTEGER, PRIMARY KEY (id)) "+
		"CREATE TABLE j2 (id2 BIGINT NOT NULL, y BIGINT, PRIMARY KEY (id2))")

	for i, v := range []int64{2000000000, 147483647} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO jmax VALUES (%d, CAST(%d AS INTEGER))", i+1, v)); err != nil {
			t.Fatalf("INSERT jmax %d: %v", i+1, err)
		}
	}
	for i, v := range []int64{-2000000000, -147483648} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO jmin VALUES (%d, CAST(%d AS INTEGER))", i+1, v)); err != nil {
			t.Fatalf("INSERT jmin %d: %v", i+1, err)
		}
	}
	for i := 1; i <= 2; i++ {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO j2 VALUES (%d, 1)", i)); err != nil {
			t.Fatalf("INSERT j2 %d: %v", i, err)
		}
	}

	for _, tc := range []struct{ q, want string }{
		{`SELECT SUM(jmax.x) FROM jmax, j2 WHERE jmax.id = j2.id2`, "ok:2147483647"},
		{`SELECT SUM(x) FROM jmax`, "ok:2147483647"},
		{`SELECT SUM(jmin.x) FROM jmin, j2 WHERE jmin.id = j2.id2`, "ok:-2147483648"},
		{`SELECT SUM(x) FROM jmin`, "ok:-2147483648"},
	} {
		if got := sumOutcome(t, ctx, db, tc.q); got != tc.want {
			t.Fatalf("%s = %s, want %s — the exact boundary is inside the int32 domain; a 22003 here is an off-by-one in the checked lane",
				tc.q, got, tc.want)
		}
	}
}

// TestFDB_AggregateOperandWidthBigintBothPaths pins the SUM_L lane on both
// paths: SUM(BIGINT) answers at exactly MaxInt64 and raises 22003
// "long overflow" (Java's Math.addExact(long, long) message) one past it —
// with and without a join around the operand's table. This is the control that
// keeps the int32 fix from over-reaching: a BIGINT operand must never take the
// int32-checked lane.
func TestFDB_AggregateOperandWidthBigintBothPaths(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := aggWidthDB(t, ctx, "bigint", ""+
		"CREATE TABLE bmax (id BIGINT NOT NULL, y BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE bovf (id BIGINT NOT NULL, y BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE j2 (id2 BIGINT NOT NULL, y BIGINT, PRIMARY KEY (id2))")

	for i, v := range []int64{9223372036854775806, 1} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO bmax VALUES (%d, %d)", i+1, v)); err != nil {
			t.Fatalf("INSERT bmax %d: %v", i+1, err)
		}
	}
	for i, v := range []int64{9223372036854775807, 1} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO bovf VALUES (%d, %d)", i+1, v)); err != nil {
			t.Fatalf("INSERT bovf %d: %v", i+1, err)
		}
	}
	for i := 1; i <= 2; i++ {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO j2 VALUES (%d, 1)", i)); err != nil {
			t.Fatalf("INSERT j2 %d: %v", i, err)
		}
	}

	for _, q := range []string{
		`SELECT SUM(bmax.y) FROM bmax, j2 WHERE bmax.id = j2.id2`,
		`SELECT SUM(y) FROM bmax`,
	} {
		if got := sumOutcome(t, ctx, db, q); got != "ok:9223372036854775807" {
			t.Fatalf("%s = %s, want ok:9223372036854775807 — exactly MaxInt64 is inside the int64 domain",
				q, got)
		}
	}
	for _, q := range []string{
		`SELECT SUM(bovf.y) FROM bovf, j2 WHERE bovf.id = j2.id2`,
		`SELECT SUM(y) FROM bovf`,
	} {
		got := sumErrText(t, ctx, db, q)
		if !strings.Contains(got, "22003") || !strings.Contains(got, "long overflow") {
			t.Fatalf("%s = %q, want 22003 %q — one past MaxInt64 must overflow the SUM_L lane on both paths",
				q, got, "long overflow")
		}
	}
}

// TestFDB_AggregateOperandWidthAvgCountControls pins the neighbours of the
// SUM_I fix: AVG over an INTEGER join-leg operand shares the int-checked
// accumulation (Java AVG_I sums with Math.addExact(int, int),
// NumericAggregationValue.java:636-640) and must raise 22003, while COUNT of
// the same operand never consults the width (CountValue, not
// NumericAggregationValue) and must answer.
func TestFDB_AggregateOperandWidthAvgCountControls(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := aggWidthDB(t, ctx, "avgcount", ""+
		"CREATE TABLE j1 (id BIGINT NOT NULL, x INTEGER, PRIMARY KEY (id)) "+
		"CREATE TABLE j2 (id2 BIGINT NOT NULL, y BIGINT, PRIMARY KEY (id2))")

	for i := 1; i <= 2; i++ {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO j1 VALUES (%d, CAST(2000000000 AS INTEGER))", i)); err != nil {
			t.Fatalf("INSERT j1 %d: %v", i, err)
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO j2 VALUES (%d, 1)", i)); err != nil {
			t.Fatalf("INSERT j2 %d: %v", i, err)
		}
	}

	for _, q := range []string{
		`SELECT AVG(j1.x) FROM j1, j2 WHERE j1.id = j2.id2`,
		`SELECT AVG(x) FROM j1`,
	} {
		got := sumErrText(t, ctx, db, q)
		if !strings.Contains(got, "22003") {
			t.Fatalf("%s = %q, want 22003 — AVG_I accumulates through the same int32-checked lane as SUM_I",
				q, got)
		}
	}
	countQ := `SELECT COUNT(j1.x) FROM j1, j2 WHERE j1.id = j2.id2`
	if got := sumOutcome(t, ctx, db, countQ); got != "ok:2" {
		t.Fatalf("%s = %s, want ok:2 — COUNT never takes the width-checked lane", countQ, got)
	}
}
