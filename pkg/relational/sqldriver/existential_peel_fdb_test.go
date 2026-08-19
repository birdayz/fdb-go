package sqldriver_test

// RFC-235: shapes that reach Java's Case-1 existential peel in
// PartitionSelectRule — a lower partition holding only the existential,
// `Select(result = 1, quantifiers = [E], predicates = [Exists(E)])` under a
// fresh ForEach. Go could not produce that plan until the residual conversion
// landed; these pin what it now produces, ROWS FIRST, because every defect the
// peel surfaced was a silently or loudly empty result rather than a wrong shape.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_ExistentialPeelShapes(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_exist_peel")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_exist_peel")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE exist_peel "+
			"CREATE TABLE a (id BIGINT, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT, a_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE d (id BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX c_a_id ON c (a_id)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_exist_peel/s WITH TEMPLATE exist_peel")
	dsn := fmt.Sprintf("fdbsql:///testdb_exist_peel?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO a VALUES (1, 1), (2, 2), (3, 3)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b VALUES (101, 2), (102, null)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c (id, a_id) VALUES (50, 1), (51, 2), (52, 1)")
	mwjoMustExec(t, db, ctx, "INSERT INTO d (id) VALUES (1)")

	// scan runs q and returns each row rendered as a |-joined string, so a
	// scenario can state its expectation without knowing the column count.
	scan := func(t *testing.T, q string) []string {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("columns %q: %v", q, err)
		}
		var out []string
		for rows.Next() {
			cells := make([]any, len(cols))
			for i := range cells {
				cells[i] = new(sql.NullInt64)
			}
			if err := rows.Scan(cells...); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			s := ""
			for i, c := range cells {
				if i > 0 {
					s += "|"
				}
				v := c.(*sql.NullInt64)
				if !v.Valid {
					s += "NULL"
					continue
				}
				s += fmt.Sprintf("%d", v.Int64)
			}
			out = append(out, s)
		}
		// rows.Err() is checked BEFORE the comparison and separately from it:
		// an iteration that died mid-stream otherwise reads as a short result
		// set, which is the same green an empty table produces.
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		return out
	}

	eq := func(t *testing.T, name string, got, want []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: got %v (%d rows), want %v (%d rows)", name, got, len(got), want, len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%s: row %d = %q, want %q (full: %v vs %v)", name, i, got[i], want[i], got, want)
			}
		}
	}

	for _, tc := range []struct {
		name string
		sql  string
		want []string
	}{
		{
			// The peel's canonical shape: an UNCORRELATED existential beside two
			// ForEach legs. Java partitions `lower = {E}` and the EXISTS becomes a
			// one-row `Map(Filter(FirstOrDefault(...)))` cross-joined with the pair.
			// Before the residual conversion this returned zero rows with a nil
			// rows.Err(): the peeled filter carried `EXISTS(_current)`, which is
			// UNKNOWN for every row.
			name: "uncorrelated_exists_in_on_over_join",
			sql:  "SELECT a.id, c.id FROM a JOIN c ON c.a_id = a.id AND EXISTS (SELECT 1 FROM d) ORDER BY a.id, c.id",
			want: []string{"1|50", "1|52", "2|51"},
		},
		{
			// Same peel, comma-join spelling, and the shape whose join declared a
			// single-leg QOV output while its cursor emitted the concatenation.
			name: "uncorrelated_exists_over_comma_join",
			sql:  "SELECT a.id, c.id FROM a, c WHERE c.a_id = a.id AND EXISTS (SELECT 1 FROM d) ORDER BY a.id, c.id",
			want: []string{"1|50", "1|52", "2|51"},
		},
		{
			// A join whose result reads ONE leg. This is the bare-QOV result value
			// PartitionSelectRule mints directly (`getFlowedObjectValue()`), and it
			// is what proved the join build could not fall back to plain child
			// concatenation.
			name: "single_leg_projection_over_peeled_join",
			sql:  "SELECT a.id FROM a, c WHERE c.a_id = a.id AND EXISTS (SELECT 1 FROM d) ORDER BY a.id",
			want: []string{"1", "1", "2"},
		},
		{
			// Two CORRELATED NOT EXISTS over one table: the sibling multi-EXISTS
			// peel, where each peeled lower is correlated to the surviving outer.
			// `b` holds (101, 2) and (102, NULL); restricting both legs to id 101
			// makes this the documented `a.v NOT IN (2)` emulation.
			name: "two_correlated_not_exists",
			sql: "SELECT id FROM a WHERE a.v IS NOT NULL " +
				"AND NOT EXISTS (SELECT 1 FROM b AS sub WHERE sub.id = 101 AND sub.v = a.v) " +
				"AND NOT EXISTS (SELECT 1 FROM b AS sub WHERE sub.id = 101 AND sub.v IS NULL) " +
				"ORDER BY id",
			want: []string{"1", "3"},
		},
		{
			// The NULL-probe leg unrestricted: b's NULL row makes the second leg
			// true for every outer row, so the result is empty. Kept beside the
			// case above because an empty expectation is only meaningful next to a
			// non-empty control that shares its machinery.
			name: "two_correlated_not_exists_null_probe",
			sql: "SELECT id FROM a WHERE a.v IS NOT NULL " +
				"AND NOT EXISTS (SELECT 1 FROM b AS sub WHERE sub.v = a.v) " +
				"AND NOT EXISTS (SELECT 1 FROM b AS sub WHERE sub.v IS NULL) " +
				"ORDER BY id",
			want: nil,
		},
		{
			// A CORRELATED existential beside a two-table join — the WHERE-EXISTS
			// over a join that the retired three-quantifier arm used to own.
			name: "correlated_exists_over_join",
			sql: "SELECT a.id, c.id FROM a, c WHERE c.a_id = a.id " +
				"AND EXISTS (SELECT 1 FROM b AS sub WHERE sub.v = a.v) ORDER BY a.id, c.id",
			want: []string{"2|51"},
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eq(t, tc.name, scan(t, tc.sql), tc.want)
		})
	}
}
