package sqldriver_test

// Adversarial probes around EXISTS-in-ON (RFC-154 Phase 2a) — feature-edge
// stress: multiple EXISTS conjuncts, EXISTS-in-ON alongside WHERE-EXISTS,
// uncorrelated EXISTS, and a 3-way join with EXISTS-in-ON. Each asserts a
// hand-computed row set; a crash / planner error / wrong rows here is a bug to
// fix (or a clean rejection to pin).

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_ExistsInOn_Probe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_exists_on_probe")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_exists_on_probe")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE exists_on_probe "+
			"CREATE TABLE a (id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT, a_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE d (id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE e (id BIGINT, c_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX c_a_id ON c (a_id) "+
			"CREATE INDEX e_c_id ON e (c_id)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_exists_on_probe/s WITH TEMPLATE exists_on_probe")
	dsn := fmt.Sprintf("fdbsql:///testdb_exists_on_probe?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// a={1,2,3}; c: 50→a1, 51→a2, 52→a1; d={1}; e: 900→c50.
	mwjoMustExec(t, db, ctx, "INSERT INTO a (id) VALUES (1), (2), (3)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c (id, a_id) VALUES (50, 1), (51, 2), (52, 1)")
	mwjoMustExec(t, db, ctx, "INSERT INTO d (id) VALUES (1)")
	mwjoMustExec(t, db, ctx, "INSERT INTO e (id, c_id) VALUES (900, 50)")

	// Two EXISTS conjuncts in one ON are two WHERE-EXISTS (the builder folds
	// an inner join's ON-EXISTS into the WHERE), applied one after the other
	// by the existential peel. a JOIN c on a_id: (1,50),(2,51),(1,52); EXISTS
	// d (d={1}) keeps (1,50),(1,52); EXISTS e (e.c_id=50) keeps (1,50).
	t.Run("two_exists_in_on", func(t *testing.T) {
		got := scanPairs(t, db, ctx,
			"SELECT a.id, c.id FROM a JOIN c ON c.a_id = a.id "+
				"AND EXISTS (SELECT 1 FROM d WHERE d.id = a.id) "+
				"AND EXISTS (SELECT 1 FROM e WHERE e.c_id = c.id)")
		want := []string{"1|50"}
		if !eqStrSlices(got, want) {
			t.Errorf("two-EXISTS-in-ON rows = %v, want %v", got, want)
		}
	})

	// EXISTS in ON + EXISTS in WHERE together: both existentials are owned by
	// the one flat Select the translator builds (translateJoinWithExists
	// attaches the ON's existential quantifiers as well as the WHERE's) and
	// the existential peel applies them one after the other. a JOIN c on
	// a_id: (1,50),(2,51),(1,52); EXISTS d (d={1}) keeps a=1: (1,50),(1,52);
	// EXISTS e (e.c_id=50) keeps (1,50). This fixture cannot tell a dropped
	// ON-EXISTS from an applied one (d={1} admits the same rows); the arm
	// that can is TestFDB_ExistsInOnPlusWhereExists below.
	t.Run("exists_in_on_plus_where_exists", func(t *testing.T) {
		got := scanPairs(t, db, ctx,
			"SELECT a.id, c.id FROM a JOIN c ON c.a_id = a.id AND EXISTS (SELECT 1 FROM d WHERE d.id = a.id) "+
				"WHERE EXISTS (SELECT 1 FROM e WHERE e.c_id = c.id)")
		want := []string{"1|50"}
		if !eqStrSlices(got, want) {
			t.Errorf("EXISTS-in-ON + WHERE-EXISTS rows = %v, want %v", got, want)
		}
	})

	// Uncorrelated EXISTS in ON: EXISTS(SELECT 1 FROM d) is always true (d
	// non-empty), so the ON reduces to the equi-join → all matches.
	t.Run("uncorrelated_exists_in_on", func(t *testing.T) {
		got := scanPairs(t, db, ctx,
			"SELECT a.id, c.id FROM a JOIN c ON c.a_id = a.id AND EXISTS (SELECT 1 FROM d)")
		want := []string{"1|50", "1|52", "2|51"}
		if !eqStrSlices(got, want) {
			t.Errorf("uncorrelated-EXISTS-in-ON rows = %v, want %v", got, want)
		}
	})

	// 3-way join, EXISTS in the SECOND join's ON correlated to the first join's
	// left leg. a JOIN c ON c.a_id=a.id, then JOIN e ON e.c_id=c.id AND
	// EXISTS(d.id=a.id). e=900→c50; c50→a1; EXISTS(d.id=1) true → (1,50,900).
	t.Run("threeway_exists_in_second_on", func(t *testing.T) {
		got := scanTriples(t, db, ctx,
			"SELECT a.id, c.id, e.id FROM a JOIN c ON c.a_id = a.id "+
				"JOIN e ON e.c_id = c.id AND EXISTS (SELECT 1 FROM d WHERE d.id = a.id)")
		want := []string{"1|50|900"}
		if !eqStrSlices(got, want) {
			t.Errorf("3-way EXISTS-in-2nd-ON rows = %v, want %v", got, want)
		}
	})
}

func scanPairs(t *testing.T, db *sql.DB, ctx context.Context, q string) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	got := siScanRows(t, rows)
	return got
}

func scanTriples(t *testing.T, db *sql.DB, ctx context.Context, q string) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var x, y, z sql.NullInt64
		if err := rows.Scan(&x, &y, &z); err != nil {
			t.Fatalf("scan: %v", err)
		}
		r := func(v sql.NullInt64) string {
			if !v.Valid {
				return "NULL"
			}
			return fmt.Sprintf("%d", v.Int64)
		}
		out = append(out, r(x)+"|"+r(y)+"|"+r(z))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// TestFDB_ExistsInOnPlusWhereExists pins, on data that can tell, that BOTH
// existentials of `JOIN … ON … AND EXISTS(d) WHERE EXISTS(e)` are applied —
// an inner join's ON-EXISTS is a WHERE-EXISTS, folded into the WHERE by the
// builder (embedded/on_exists_fold.go) — on the binary join and on a
// three-leg cluster with the EXISTS in either join's ON. The translator used
// to attach only the WHERE's existential quantifier on the binary shape,
// leaving the ON's `EXISTS(q$N)` marker referencing a quantifier no Select
// owned; the buried-existential backstop happened to reject the query while
// the ON predicate arrived as one AND, and once the Select constructor lifted
// the conjunction the dangling marker reached the planner and was dropped —
// every row the ON-EXISTS should have excluded came back. The three-leg
// shapes were refused outright (the gate poisoned an N-way join carrying its
// own existential) until the builder folded the ON-EXISTS into the WHERE so
// no route ever saw a join carrying one.
//
// Binary: a={1,2,3}; c: 50→a1, 51→a2, 52→a1, 53→a2; d={2}; e: 900→c50,
// 901→c51. Join pairs on a_id: (1,50),(2,51),(1,52),(2,53). EXISTS d (a.id ∈
// {2}) keeps (2,51),(2,53); EXISTS e (c.id ∈ {50,51}) keeps (2,51). Dropping
// the ON-EXISTS would return (1,50) as well; dropping the WHERE-EXISTS would
// return (2,53) as well.
//
// Three-leg: g: 900→c50, 901→c51, 902→c53; h: 7000→g901, 7001→g900. Triples
// on a_id/c_id: (1,50,900),(2,51,901),(2,53,902). EXISTS d keeps
// (2,51,901),(2,53,902); EXISTS h (g.id ∈ {900,901}) keeps (2,51,901).
// Dropping the ON-EXISTS would return (1,50,900) as well; dropping the
// WHERE-EXISTS would return (2,53,902) as well.
func TestFDB_ExistsInOnPlusWhereExists(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_exists_on_where")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_exists_on_where")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE exists_on_where "+
			"CREATE TABLE a (id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT, a_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE d (id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE e (id BIGINT, c_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE g (id BIGINT, c_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE h (id BIGINT, g_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX c_a_id ON c (a_id) "+
			"CREATE INDEX e_c_id ON e (c_id) "+
			"CREATE INDEX g_c_id ON g (c_id) "+
			"CREATE INDEX h_g_id ON h (g_id)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_exists_on_where/s WITH TEMPLATE exists_on_where")
	dsn := fmt.Sprintf("fdbsql:///testdb_exists_on_where?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO a (id) VALUES (1), (2), (3)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c (id, a_id) VALUES (50, 1), (51, 2), (52, 1), (53, 2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO d (id) VALUES (2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO e (id, c_id) VALUES (900, 50), (901, 51)")
	mwjoMustExec(t, db, ctx, "INSERT INTO g (id, c_id) VALUES (900, 50), (901, 51), (902, 53)")
	mwjoMustExec(t, db, ctx, "INSERT INTO h (id, g_id) VALUES (7000, 901), (7001, 900)")

	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"exists_in_on_plus_where_exists", "SELECT a.id, c.id FROM a JOIN c ON c.a_id = a.id AND EXISTS (SELECT 1 FROM d WHERE d.id = a.id) WHERE EXISTS (SELECT 1 FROM e WHERE e.c_id = c.id)"},
		// Control: the same two existentials both in WHERE, which Java folds
		// an inner join's ON into anyway.
		{"both_exists_in_where", "SELECT a.id, c.id FROM a JOIN c ON c.a_id = a.id WHERE EXISTS (SELECT 1 FROM d WHERE d.id = a.id) AND EXISTS (SELECT 1 FROM e WHERE e.c_id = c.id)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := scanPairs(t, db, ctx, tc.sql)
			want := []string{"2|51"}
			if !eqStrSlices(got, want) {
				t.Errorf("rows = %v, want %v\n  sql: %s", got, want, tc.sql)
			}
		})
	}

	const whereH = " WHERE EXISTS (SELECT 1 FROM h WHERE h.g_id = g.id)"
	const existsD = " AND EXISTS (SELECT 1 FROM d WHERE d.id = a.id)"
	for _, tc := range []struct {
		name string
		sql  string
		want []string
	}{
		// The EXISTS in the ROOT join's ON (the second JOIN) beside a WHERE-EXISTS.
		{
			"threeway_root_on_exists_plus_where_exists",
			"SELECT a.id, c.id, g.id FROM a JOIN c ON c.a_id = a.id JOIN g ON g.c_id = c.id" + existsD + whereH,
			[]string{"2|51|901"},
		},
		// The EXISTS in the NESTED join's ON (the first JOIN) beside a WHERE-EXISTS.
		{
			"threeway_nested_on_exists_plus_where_exists",
			"SELECT a.id, c.id, g.id FROM a JOIN c ON c.a_id = a.id" + existsD + " JOIN g ON g.c_id = c.id" + whereH,
			[]string{"2|51|901"},
		},
		// Control: both existentials in WHERE.
		{
			"threeway_both_exists_in_where",
			"SELECT a.id, c.id, g.id FROM a JOIN c ON c.a_id = a.id JOIN g ON g.c_id = c.id WHERE EXISTS (SELECT 1 FROM d WHERE d.id = a.id) AND EXISTS (SELECT 1 FROM h WHERE h.g_id = g.id)",
			[]string{"2|51|901"},
		},
		// A ROOT ON-EXISTS beside a plain (non-EXISTS) WHERE.
		{
			"threeway_root_on_exists_plus_where",
			"SELECT a.id, c.id, g.id FROM a JOIN c ON c.a_id = a.id JOIN g ON g.c_id = c.id" + existsD + " WHERE a.id > 0",
			[]string{"2|51|901", "2|53|902"},
		},
		// A NESTED ON-EXISTS with no WHERE at all (the projection-level lift).
		{
			"threeway_nested_on_exists_no_where",
			"SELECT a.id, c.id, g.id FROM a JOIN c ON c.a_id = a.id" + existsD + " JOIN g ON g.c_id = c.id",
			[]string{"2|51|901", "2|53|902"},
		},
		// A ROOT ON-EXISTS with no WHERE (already served before the fold).
		{
			"threeway_root_on_exists_no_where",
			"SELECT a.id, c.id, g.id FROM a JOIN c ON c.a_id = a.id JOIN g ON g.c_id = c.id" + existsD,
			[]string{"2|51|901", "2|53|902"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := scanTriples(t, db, ctx, tc.sql)
			if !eqStrSlices(got, tc.want) {
				t.Errorf("rows = %v, want %v\n  sql: %s", got, tc.want, tc.sql)
			}
		})
	}

	// A PROJECTED EXISTS beside a WHERE-EXISTS is a separate, pre-existing gap
	// (TODO.md "A projected EXISTS beside a WHERE-EXISTS fails opaquely"): it
	// fails on a single table too, so it is not about the ON clause or the
	// join. Pinned as a refusal — never wrong rows — on the single-table form
	// and on the ON-EXISTS form the builder's fold turns into it; when the
	// capability lands these arms flip to row assertions.
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{
			"projected_exists_beside_where_exists_single_table_refused",
			"SELECT a.id, EXISTS (SELECT 1 FROM c WHERE c.a_id = a.id) FROM a WHERE EXISTS (SELECT 1 FROM d WHERE d.id = a.id)",
		},
		{
			"projected_exists_beside_on_exists_threeway_refused",
			"SELECT a.id, c.id, g.id, EXISTS (SELECT 1 FROM h WHERE h.g_id = g.id) FROM a JOIN c ON c.a_id = a.id JOIN g ON g.c_id = c.id" + existsD,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertUnsupported(t, db, ctx, tc.sql)
		})
	}
}

// TestFDB_ExistsInOnBelowOuterJoinAndBesideUnnest pins the ON-EXISTS fold
// at the two places it is NOT the block's WHERE. An inner cluster carrying
// an ON-EXISTS below an OUTER join is filtered in place — directly above
// itself, before the outer join preserves or null-extends its rows (Java's
// collapseLeftSideOperators) — for every outer kind: LEFT (the cluster
// preserved; the WHERE spelling is the control and agrees), RIGHT and FULL
// (the cluster null-supplying; the WHERE spelling would drop the
// null-extended rows, so the rows are pinned explicitly). An inner lateral
// unnest is transparent: its outer's ON-EXISTS is the block's WHERE-EXISTS,
// and the WHERE spelling is the control.
//
// a={1,2,3} with tags [10,11],[20],[30]; c: 50→a1, 51→a2, 52→a1, 53→a2;
// d={2}; e: 900→c50, 901→c51. The cluster a⋈c∧∃d is (2,51),(2,53).
func TestFDB_ExistsInOnBelowOuterJoinAndBesideUnnest(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_exists_on_outer")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_exists_on_outer")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE exists_on_outer "+
			"CREATE TABLE a (id BIGINT, tags BIGINT ARRAY, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT, a_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE d (id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE e (id BIGINT, c_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX c_a_id ON c (a_id) "+
			"CREATE INDEX e_c_id ON e (c_id)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_exists_on_outer/s WITH TEMPLATE exists_on_outer")
	dsn := fmt.Sprintf("fdbsql:///testdb_exists_on_outer?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO a (id, tags) VALUES (1, [10, 11]), (2, [20]), (3, [30])")
	mwjoMustExec(t, db, ctx, "INSERT INTO c (id, a_id) VALUES (50, 1), (51, 2), (52, 1), (53, 2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO d (id) VALUES (2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO e (id, c_id) VALUES (900, 50), (901, 51)")

	const existsD = " AND EXISTS (SELECT 1 FROM d WHERE d.id = a.id)"
	const whereD = " WHERE EXISTS (SELECT 1 FROM d WHERE d.id = a.id)"
	for _, tc := range []struct {
		name string
		sql  string
		want []string
	}{
		// LEFT: the cluster is preserved; (2,53) has no e row and null-extends.
		{"left_join", "SELECT a.id, c.id, e.id FROM a JOIN c ON c.a_id = a.id" + existsD + " LEFT JOIN e ON e.c_id = c.id", []string{"2|51|901", "2|53|NULL"}},
		{"left_join_where_control", "SELECT a.id, c.id, e.id FROM a JOIN c ON c.a_id = a.id LEFT JOIN e ON e.c_id = c.id" + whereD, []string{"2|51|901", "2|53|NULL"}},
		{"left_join_plus_where", "SELECT a.id, c.id, e.id FROM a JOIN c ON c.a_id = a.id" + existsD + " LEFT JOIN e ON e.c_id = c.id WHERE a.id > 0", []string{"2|51|901", "2|53|NULL"}},
		{"left_join_then_inner_join", "SELECT a.id, c.id, e.id FROM a JOIN c ON c.a_id = a.id" + existsD + " LEFT JOIN e ON e.c_id = c.id JOIN d ON d.id = a.id", []string{"2|51|901", "2|53|NULL"}},
		// RIGHT: the cluster is null-supplying; e=900 (→c50, a=1, excluded by
		// the EXISTS) is preserved null-extended. Dropping the in-place filter
		// would pair 900 with (1,50); lifting it to the WHERE would drop 900.
		{"right_join", "SELECT a.id, c.id, e.id FROM a JOIN c ON c.a_id = a.id" + existsD + " RIGHT JOIN e ON e.c_id = c.id", []string{"2|51|901", "NULL|NULL|900"}},
		// FULL: both sides preserved.
		{"full_join", "SELECT a.id, c.id, e.id FROM a JOIN c ON c.a_id = a.id" + existsD + " FULL JOIN e ON e.c_id = c.id", []string{"2|51|901", "2|53|NULL", "NULL|NULL|900"}},
		// Lateral unnest: transparent — the block's WHERE-EXISTS.
		{"beside_unnest", "SELECT a.id, c.id, v FROM a JOIN c ON c.a_id = a.id" + existsD + ", a.tags AS v", []string{"2|51|20", "2|53|20"}},
		{"beside_unnest_where_control", "SELECT a.id, c.id, v FROM a JOIN c ON c.a_id = a.id, a.tags AS v" + whereD, []string{"2|51|20", "2|53|20"}},
		{"beside_unnest_plus_where", "SELECT a.id, c.id, v FROM a JOIN c ON c.a_id = a.id" + existsD + ", a.tags AS v WHERE a.id > 0", []string{"2|51|20", "2|53|20"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := scanTriples(t, db, ctx, tc.sql)
			if !eqStrSlices(got, tc.want) {
				t.Errorf("rows = %v, want %v\n  sql: %s", got, tc.want, tc.sql)
			}
		})
	}
}
