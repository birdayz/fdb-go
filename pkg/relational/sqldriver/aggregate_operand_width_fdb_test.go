package sqldriver_test

// The identity dimension of the SUM/AVG operand WIDTH decision
// (cascades_translator.go aggregateOperandIntType).
//
// The width is not cosmetic: values.TypeCodeInt selects Java's SUM_I / AVG_I,
// which overflow at the int32 boundary and raise 22003, while anything else
// keeps SUM_L and answers in int64. Picking the wrong column's width therefore
// changes an ARITHMETIC RESULT — a query either errors or returns a number —
// and no plan golden can see it.
//
// The operand used to be matched against the input's column list by its leaf
// display name. Two facts make that the wrong key, and each gets a test here:
//
//  1. WHICH column, within one layout, is the ordinal — so a schema is used
//     where INTEGER and BIGINT columns interleave, and every one of them is
//     summed. Any off-by-one, any "first INTEGER wins", and any constant
//     answer flips at least one case.
//  2. WHICH LAYOUT the ordinal indexes. The operand is baked against the
//     translated input expression's output columns; the width list is derived
//     separately from the logical input's leg columns. A join operand carries
//     its own LEG's layout while the width list is the merged, qualified row —
//     so an ordinal taken across the two reads a different column entirely.
//     Here that column is an INTEGER while the summed one is a BIGINT, so the
//     mistake manufactures a 22003 out of a query that must answer.

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
// of (id2, y) — while the width list is the merged, qualified join row, whose
// position 1 is the OTHER leg's INTEGER column. Reading the ordinal across the
// two layouts narrows a BIGINT sum to int32 and turns an answerable query into
// 22003.
//
// The width must therefore be declined here, not guessed: the reference does
// not index the list being consulted, and a declined narrowing only ever costs
// an overflow check that int64 still performs.
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

// TestFDB_AggregateOperandWidthJoinLegGapVsJava records what declining COSTS,
// so the price is a measured number rather than an assumption.
//
// Summing an INTEGER column of a join leg does NOT overflow at int32 here,
// while summing the SAME column of the SAME table without the join does. Java
// raises 22003 for both: NumericAggregationValue.encapsulate picks SUM_I from
// the operand's static TypeCode, and in Java that TypeCode survives a join
// because the reference carries its column's own type. Go's resolver widens
// every integer column reference to LONG, so the width has to be recovered
// from the input's typed column list — and for a join operand that list is the
// merged, qualified row, a layout the operand's ordinal does not index.
//
// This gap is OLDER than the ordinal key and unchanged by it: the leaf-name
// version declined here too (it required a childless operand, which a join
// reference is not). Closing it means giving the operand its LEG's typed
// column list, which changes what queries answer — SUMs that return a number
// today would start raising 22003 — so it is a semantics change, not a
// migration, and it is not made here.
//
// When it is made, this test goes red on the first case. That is the point:
// the pin is what makes the gap visible instead of merely absent.
func TestFDB_AggregateOperandWidthJoinLegGapVsJava(t *testing.T) {
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

	joined := `SELECT SUM(j1.x) FROM j1, j2 WHERE j1.id = j2.id2`
	if got := sumOutcome(t, ctx, db, joined); got != "ok:4000000000" {
		t.Fatalf("%s = %s, want ok:4000000000 — this pins a KNOWN GAP, not desired behaviour: Java raises 22003 here. A 22003 means the leg-scoped width landed; update this pin and say so",
			joined, got)
	}

	alone := `SELECT SUM(x) FROM j1`
	if got := sumOutcome(t, ctx, db, alone); got != "22003" {
		t.Fatalf("%s = %s, want 22003 — the same column of the same table narrows correctly without the join; if this stops, the width lookup broke outright rather than merely not reaching join legs",
			alone, got)
	}
}
