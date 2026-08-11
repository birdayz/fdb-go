package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_NestedPathDepthGate pins how DEEP a dotted reference may go, and it
// no longer pins a cap.
//
// WHAT THIS FILE USED TO SAY, and why the change is deliberate rather than a
// regression. Nested field access resolved exactly ONE level (`struct.field`);
// `struct.inner.field` and `alias.struct.field` both failed 42703, because
// splitColumnRef captured a dotted reference as one JOINED qualifier string
// plus one name — `HOME.POS` + `LAT` — and the scope had nothing to decompose.
// This test asserted that refusal from both ends, and it carried the
// instruction that when the Identifier model of RFC-204 §4.4 landed the deep
// arms would start answering and the assertions would flip to value checks.
// They landed, and this is that flip: every arm below asserts the ANSWER.
//
// Java is the spec and has no cap: `fullId : uid (DOT uid)*` in the grammar,
// Identifier carrying `name` plus a `List<String> qualifier` accumulated one
// segment at a time (IdentifierVisitor.java:56-64), and lookupNestedField
// consuming a matched PREFIX and walking whatever remains through an unbounded
// accessor loop (SemanticAnalyzer.java:559-601). There is no arity anywhere in
// that path, so there is none here.
//
// WHAT IS STILL A REFUSAL, and why those arms are the load-bearing half. An
// unbounded descent must stay a descent: a resolver that grew depth by
// becoming permissive — matching a prefix loosely, or falling back to the leaf
// when the path does not resolve — passes every value assertion above and
// answers rows for paths that name nothing. The refusal arms below drive that
// axis: a bad leaf, a bad root, a bad middle, a descent into a scalar, and one
// segment too many past a real leaf. Each of those is one edit away from the
// answering shapes beside it.
func TestFDB_NestedPathDepthGate(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/nestdepth")
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE /nestdepth"); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE nd_tmpl "+
			"CREATE TYPE AS STRUCT GEO (lat BIGINT, lon BIGINT) "+
			"CREATE TYPE AS STRUCT ADDR (city STRING, pos GEO) "+
			"CREATE TABLE T (id BIGINT, home ADDR, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA /nestdepth/s WITH TEMPLATE nd_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///nestdepth?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// lat and lon differ in SIGN as well as magnitude, so a read of the wrong
	// member of GEO is visible in the value and not only in the label. Two rows,
	// with the second row's members swapped in sign, so a read that returned a
	// constant or the primary key cannot match.
	if _, err := db.ExecContext(ctx,
		"INSERT INTO T VALUES (1, ('sf', (37, -122))), (2, ('nyc', (40, -74)))"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// queryOne returns the single column of every row plus the label
	// database/sql surfaces for it, because the depth change moved BOTH: the
	// value comes from the fused descent and the label from the path's leaf.
	queryOne := func(t *testing.T, q string) (string, []string) {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("%s: Columns: %v", q, err)
		}
		if len(cols) != 1 {
			t.Fatalf("%s: %d columns, want 1", q, len(cols))
		}
		var out []string
		for rows.Next() {
			var v any
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("%s: scan: %v", q, err)
			}
			out = append(out, fmt.Sprint(v))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: iterate: %v", q, err)
		}
		return cols[0], out
	}

	t.Run("one_level_resolves", func(t *testing.T) {
		t.Parallel()
		// UNCHANGED, and it is the control that says the depth work did not
		// re-route the shallow case through some new path.
		label, got := queryOne(t, "SELECT home.city FROM T ORDER BY id")
		if label != "CITY" || fmt.Sprint(got) != "[sf nyc]" {
			t.Errorf("SELECT home.city = %v labelled %q, want [sf nyc] labelled CITY", got, label)
		}
	})

	t.Run("two_level_answers", func(t *testing.T) {
		t.Parallel()
		// THE ARM THIS FILE EXISTS FOR. It asserted a 42703 refusal and carried
		// the instruction to flip to a value assertion when the Identifier model
		// landed. This is that value assertion.
		for _, tc := range []struct {
			sql       string
			wantLabel string
			wantRows  string
		}{
			{"SELECT home.pos.lat FROM T ORDER BY id", "LAT", "[37 40]"},
			// The OTHER member of the same leaf struct: a descent that resolved
			// the struct and then returned its first member would pass the arm
			// above and fail this one.
			{"SELECT home.pos.lon FROM T ORDER BY id", "LON", "[-122 -74]"},
		} {
			label, got := queryOne(t, tc.sql)
			if label != tc.wantLabel || fmt.Sprint(got) != tc.wantRows {
				t.Errorf("%s = %v labelled %q, want %s labelled %q",
					tc.sql, got, label, tc.wantRows, tc.wantLabel)
			}
		}
	})

	t.Run("source_qualified_nested_answers", func(t *testing.T) {
		t.Parallel()
		// The second arm that asserted the old refusal. `t.home.city` is the
		// SAME descent as `home.city` with the source named, so the pair is what
		// says the alias segment was peeled off rather than folded into the
		// lookup key.
		for _, tc := range []struct {
			sql       string
			wantLabel string
			wantRows  string
		}{
			{"SELECT t.home.city FROM T AS t ORDER BY t.id", "CITY", "[sf nyc]"},
			{"SELECT T.home.city FROM T ORDER BY id", "CITY", "[sf nyc]"},
			// FOUR segments — alias, struct, struct, member. Java's accessor
			// loop is unbounded, so the depth is not two either; an arity cap
			// re-introduced at any value >= 3 would pass every arm above and
			// fail this one.
			{"SELECT t.home.pos.lon FROM T AS t ORDER BY t.id", "LON", "[-122 -74]"},
		} {
			label, got := queryOne(t, tc.sql)
			if label != tc.wantLabel || fmt.Sprint(got) != tc.wantRows {
				t.Errorf("%s = %v labelled %q, want %s labelled %q",
					tc.sql, got, label, tc.wantRows, tc.wantLabel)
			}
		}
	})

	t.Run("a_deep_path_that_names_nothing_is_still_refused", func(t *testing.T) {
		t.Parallel()
		// THE CONTROL AGAINST OVER-APPLICATION, and the reason this file did not
		// simply become a value test. Depth arrived as a real descent, not as
		// permissiveness: each query here is one segment away from an answering
		// shape above, and every one must stay a typed 42703 — never a crash,
		// never an internal error, and above all never a row.
		for _, tc := range []struct {
			sql string
			why string
		}{
			{
				"SELECT home.pos.zzz FROM T",
				"BAD LEAF — the prefix HOME.POS resolves and only the last segment does not. " +
					"A descent that returned the containing struct once the accessor walk ran " +
					"out would answer this.",
			},
			{
				"SELECT zzz.pos.lat FROM T",
				"BAD ROOT — nothing named ZZZ is a column or a source. A resolver that tried " +
					"the SUFFIX spellings until one matched would find POS.LAT under HOME and " +
					"answer.",
			},
			{
				"SELECT home.zzz.lat FROM T",
				"BAD MIDDLE — both ends exist (HOME is a column, LAT is a member of HOME.POS) " +
					"and only the middle segment is wrong. A walk that skipped an unmatched " +
					"step instead of failing would answer 37.",
			},
			{
				"SELECT home.city.zzz FROM T",
				"DESCENT INTO A SCALAR — HOME.CITY resolves and is a STRING, which has no " +
					"members. The accessor walk must refuse rather than treat a non-struct as " +
					"an empty struct.",
			},
			{
				"SELECT home.pos.lat.zzz FROM T",
				"ONE SEGMENT PAST A REAL LEAF — the entire four-segment prefix of an " +
					"answering query, plus one. This is the shape an unbounded loop with no " +
					"terminal type check answers.",
			},
			{
				"SELECT t.home.pos.zzz FROM T AS t",
				"BAD LEAF UNDER AN ALIAS — the same failure as the first case reached through " +
					"the alias-qualified channel, which is a different candidate rule.",
			},
		} {
			rows, err := db.QueryContext(ctx, tc.sql)
			if err == nil {
				defer rows.Close()
				for rows.Next() {
				}
				err = rows.Err()
			}
			if err == nil {
				t.Errorf("%s ANSWERS, and it must not.\n  %s", tc.sql, tc.why)
				continue
			}
			if !strings.Contains(err.Error(), "42703") {
				t.Errorf("%s failed with %v, want a clean 42703 undefined-column rejection.\n"+
					"  %s\n  A path that names nothing must stay a TYPED rejection — never a "+
					"crash, never an internal error, never a planner decline that hides which "+
					"segment was wrong.", tc.sql, err, tc.why)
			}
		}
	})
}
