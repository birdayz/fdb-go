package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_NestedSortKeyOrdersByTheMemberNotTheStructRoot answers a question the
// plan-shape golden cannot: `ORDER BY n.sk` over a struct column renders as
// `InMemorySort([N ASC])` — naming the struct ROOT — and the golden shows the
// PLAN, never the ROWS. Two incompatible worlds fit that rendering equally well:
// the comparator really orders by the whole struct (wrong rows, a correctness
// defect), or it orders by the nested member and only the EXPLAIN NAME is flat
// (a rendering defect). Only rows separate them.
//
// THE EXISTING CORPUS CANNOT SEPARATE THEM, and that is the trap this test
// exists to escape. projected_exists_nested_sort_key.yaml orders by `n.sk`,
// which is the struct's FIRST member, so a lexicographic struct-root comparison
// and a compare-by-`sk` comparison agree on every permutation of that data. Its
// header comment says a single-member struct would have made `ORDER BY n` and
// `ORDER BY n.sk` indistinguishable; a TWO-member struct sorted by its FIRST
// member is indistinguishable in exactly the same way. Those stanzas pass under
// both worlds, so their green is silent about which one we are in.
//
// THE DISCRIMINATING FIXTURE therefore orders by the NON-FIRST member and makes
// the two members ANTI-CORRELATED:
//
//	id=1 n=(sk=10, co=300)
//	id=2 n=(sk=20, co=200)
//	id=3 n=(sk=30, co=100)
//
// Ascending by `co` yields ids [3 2 1]. Ascending by `sk` yields [1 2 3], and so
// does ascending by the struct root under any first-member-wins or
// serialized-prefix comparison, and so does ascending by `id` and by the
// insertion order a dropped sort would leave. [3 2 1] is reachable by ordering
// on `n.co` and by nothing else in this row set — which is what makes the
// assertion an assertion rather than a coincidence.
//
// THE MEASURED VERDICT IS "RENDERING ONLY", and the arms below are green because
// of it: the comparator reads the nested MEMBER while EXPLAIN prints the struct
// ROOT. The sharpest evidence is that `ORDER BY n.sk` and `ORDER BY n.co` produce
// BYTE-IDENTICAL plan strings — both `InMemorySort([N ASC], ...)` — and opposite
// row orders. A plan rendering that cannot distinguish two different sorts is
// lossy by demonstration.
//
// AND THE "WRONG ROWS" WORLD IS NOT SILENT ANYWAY, which is worth recording
// because it bounds the severity of any future regression here. Disabling
// sortKeySourceValue's nested arm so a nested key really does sort by the whole
// struct does not mis-order — it fails loudly with `values: no ordering defined
// between *dynamicpb.Message and *dynamicpb.Message`. There is no struct
// comparator, neither first-member-wins nor serialized-bytes, so "sorts by the
// struct root" is a runtime error, not a quiet permutation.
func TestFDB_NestedSortKeyOrdersByTheMemberNotTheStructRoot(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	const dbPath = "/testdb_nested_sort_rows"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE nsk_tmpl "+
			"CREATE TYPE AS STRUCT nst (sk BIGINT, co BIGINT) "+
			"CREATE TABLE t1 (id BIGINT, n nst, m nst, PRIMARY KEY (id)) "+
			"CREATE TABLE t2 (id BIGINT, t1_id BIGINT, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE nsk_tmpl"); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// `m` carries the members swapped relative to `n`, so the two struct columns
	// share both LEAF names (`sk`, `co`) while disagreeing on the order each leaf
	// induces. A key that resolves to the wrong ROOT — right leaf, wrong struct —
	// therefore reds instead of landing on an equal-ordering twin.
	if _, err := db.ExecContext(ctx,
		"INSERT INTO t1 VALUES (1, (10, 300), (300, 10)), "+
			"(2, (20, 200), (200, 20)), (3, (30, 100), (100, 30))"); err != nil {
		t.Fatalf("INSERT t1: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO t2 VALUES (100, 1), (200, 3)"); err != nil {
		t.Fatalf("INSERT t2: %v", err)
	}

	const existsSel = "SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h FROM t1 "

	cases := []struct {
		name string
		sql  string
		want []int64
		why  string
	}{{
		// THE DISCRIMINATOR. Non-first member, key absent from the SELECT list, so
		// the projected-EXISTS fold appends it as a hidden column — the path whose
		// EXPLAIN renders the struct root.
		name: "fold/hidden/co-asc",
		sql:  existsSel + "ORDER BY n.co",
		want: []int64{3, 2, 1},
		why: "ascending n.co is 100,200,300 at ids 3,2,1. [1 2 3] here means the " +
			"comparator read the STRUCT ROOT (whose first member sk ascends with id), " +
			"the primary key, or nothing at all — the three ways this can be wrong " +
			"all collapse onto the same wrong answer, which is why the right answer " +
			"is the reversed one",
	}, {
		name: "fold/hidden/co-desc",
		sql:  existsSel + "ORDER BY n.co DESC",
		want: []int64{1, 2, 3},
		why:  "DESC must invert the discriminating order, not merely differ from ASC",
	}, {
		// The FIRST member, which the existing corpus already covers. Kept because
		// it is the arm that stays green under the struct-root world: if it ever
		// goes red while the `co` arms are green, the defect is not the one this
		// file is about.
		name: "fold/hidden/sk-asc",
		sql:  existsSel + "ORDER BY n.sk",
		want: []int64{1, 2, 3},
		why:  "the non-discriminating twin — green under BOTH worlds by construction",
	}, {
		// The key IS in the SELECT list, so no hidden column is appended and the
		// cleanup projection does not wrap the fold. The visible-column path.
		name: "fold/visible/co-asc",
		sql: "SELECT id, n.co, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h " +
			"FROM t1 ORDER BY n.co",
		want: []int64{3, 2, 1},
		why:  "the visible-column path, where no hidden column is appended",
	}, {
		// The struct ROOT is projected while a MEMBER is the key.
		name: "fold/root-projected/co-asc",
		sql: "SELECT id, n, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h " +
			"FROM t1 ORDER BY n.co",
		want: []int64{3, 2, 1},
		why:  "a projected root must not be mistaken for the key it is the root of",
	}, {
		// THE LEAF-NAME COLLISION. `m.co` is anti-correlated with `n.co`, so a key
		// that keeps the leaf and loses the root lands on the opposite order.
		name: "fold/hidden/collision-m-co-asc",
		sql:  existsSel + "ORDER BY m.co",
		want: []int64{1, 2, 3},
		why: "m.co is 10,20,30 at ids 1,2,3 — the EXACT reverse of n.co, so a key " +
			"resolved by leaf name alone reds here or reds on the n.co arm",
	}, {
		name: "fold/hidden/collision-m-sk-asc",
		sql:  existsSel + "ORDER BY m.sk",
		want: []int64{3, 2, 1},
		why:  "m.sk is 100,200,300 at ids 3,2,1 — the reverse of n.sk",
	}, {
		// NO FOLD. The ordinary sort path, which sortKeySourceValue's comment claims
		// is correct precisely because it passes the resolved Value through
		// untouched. Without this arm a red above could not be localised to the fold.
		name: "nofold/co-asc",
		sql:  "SELECT id FROM t1 ORDER BY n.co",
		want: []int64{3, 2, 1},
		why:  "the non-fold control — localises any red above to the projected-EXISTS fold",
	}}

	for _, tc := range cases {
		// The subtests deliberately do NOT call t.Parallel(): the parent owns the
		// *sql.DB and closes it on return, and a paused parallel subtest resumes
		// after that return — every arm then fails with "database is closed",
		// which is a green-from-an-empty-set in the failing direction. The
		// parallelism that matters for this suite is the OUTER t.Parallel above.
		t.Run(tc.name, func(t *testing.T) {
			// The rendering is RECORDED, never asserted: this test's subject is the
			// rows, and pinning the EXPLAIN text here would make it red on a pure
			// rendering fix that changes nothing a user reads.
			var plan string
			if err := db.QueryRowContext(ctx, "EXPLAIN "+tc.sql).Scan(&plan); err != nil {
				t.Logf("EXPLAIN unavailable: %v", err)
			} else {
				t.Logf("plan: %s", plan)
			}

			rows, err := db.QueryContext(ctx, tc.sql)
			if err != nil {
				t.Fatalf("query %q: %v", tc.sql, err)
			}
			defer rows.Close()
			cols, err := rows.Columns()
			if err != nil {
				t.Fatalf("Columns: %v", err)
			}

			var got []int64
			for rows.Next() {
				vals := make([]any, len(cols))
				ptrs := make([]any, len(vals))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					t.Fatalf("Scan: %v", err)
				}
				id, ok := vals[0].(int64)
				if !ok {
					t.Fatalf("row 0 column is %T (%v), want int64", vals[0], vals[0])
				}
				got = append(got, id)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows.Err: %v", err)
			}

			// A GREEN FROM AN EMPTY SET IS NOT A GREEN. An arm that returns no rows
			// satisfies no ordering claim, so the population is checked before the
			// order is read.
			if len(got) != len(tc.want) {
				t.Fatalf("%s\nsql:  %s\ngot ids %v (%d rows), want %v (%d rows) — "+
					"the ordering claim cannot be read off a population of the wrong size",
					tc.why, tc.sql, got, len(got), tc.want, len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("ORDER BY ordered by the WRONG COLUMN.\n"+
						"sql:  %s\nplan: %s\ngot:  %v\nwant: %v\nwhy:  %s",
						tc.sql, plan, got, tc.want, tc.why)
				}
			}
		})
	}
}

// TestFDB_TwoNestedSortKeysOfTheSameStructRootDoNotCollapse is the arm the
// severity question opened up rather than closed.
//
// The rows test above establishes that a SINGLE nested key sorts correctly and
// only RENDERS flat. But the flat rendering is not confined to EXPLAIN: it is the
// NAME the projected-EXISTS fold gives the hidden column it appends
// (collectExtraSortColumns, cascades_translator.go), and that name is also the
// key of the fold's `seen` dedup. `n.sk` and `n.co` both render `N`, so the
// second of them is dropped as a duplicate of the first.
//
// That dedup was previously argued safe on the grounds that two alike-rendering
// keys carry an identical source value BY CONSTRUCTION, since sortKeySourceValue
// depends on the key only through sortKeyFieldRef. That is true for FLAT keys and
// FALSE for nested ones: sortKeySourceValue's nested arm sits ABOVE the
// sortKeyFieldRef call and returns a distinct per-member value, so two keys that
// render alike now carry DIFFERENT reads. The construction the argument rested on
// no longer holds.
//
// THE FIXTURE TIES BOTH MEMBERS, because a collapse is only visible where the
// dropped key was doing work:
//
//	id=1 n=(sk=2, co=5)
//	id=2 n=(sk=1, co=5)
//	id=3 n=(sk=1, co=4)
//
// ORDER BY n.co, n.sk → [3 2 1]; collapsed to n.co alone → [3 1 2].
// ORDER BY n.sk, n.co → [3 2 1]; collapsed to n.sk alone → [2 3 1].
// Each direction is separately falsifiable, so a fix that satisfies only one is
// not mistaken for a fix.
func TestFDB_TwoNestedSortKeysOfTheSameStructRootDoNotCollapse(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	const dbPath = "/testdb_nested_sort_twokeys"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE nsk2_tmpl "+
			"CREATE TYPE AS STRUCT nst (sk BIGINT, co BIGINT) "+
			"CREATE TABLE t1 (id BIGINT, n nst, PRIMARY KEY (id)) "+
			"CREATE TABLE t2 (id BIGINT, t1_id BIGINT, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE nsk2_tmpl"); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx,
		"INSERT INTO t1 VALUES (1, (2, 5)), (2, (1, 5)), (3, (1, 4))"); err != nil {
		t.Fatalf("INSERT t1: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO t2 VALUES (100, 1), (200, 3)"); err != nil {
		t.Fatalf("INSERT t2: %v", err)
	}

	const existsSel = "SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h FROM t1 "

	// `correct` is what SQL requires. `broken` is what this build ACTUALLY
	// returns, and where the two differ the arm is a CHARACTERIZATION pin: it
	// asserts the defect so the defect cannot drift unobserved, and it fails
	// LOUDLY the moment the rows become correct so whoever fixes it is told to
	// flip the assertion rather than left wondering why a test went red.
	//
	// The alternative — asserting `correct` and shipping a permanently red test —
	// was rejected: a test that is always red is a test nobody reads, and it
	// cannot be told apart from a test that broke today.
	cases := []struct {
		name    string
		sql     string
		correct []int64
		broken  []int64
		why     string
	}{{
		name:    "fold/co-then-sk",
		sql:     existsSel + "ORDER BY n.co, n.sk",
		correct: []int64{3, 2, 1},
		broken:  []int64{3, 1, 2},
		why:     "n.sk (the SECOND key) is dropped as a duplicate of n.co's hidden column",
	}, {
		name:    "fold/sk-then-co",
		sql:     existsSel + "ORDER BY n.sk, n.co",
		correct: []int64{3, 2, 1},
		broken:  []int64{2, 3, 1},
		why:     "n.co (the SECOND key) is dropped as a duplicate of n.sk's hidden column",
	}, {
		// NO FOLD — the ordinary sort path appends no hidden column and runs no
		// dedup, so it localises the defect to the projected-EXISTS fold rather
		// than to nested multi-key sorting in general. These two arms assert the
		// CORRECT order (broken == correct), which is what makes the pair above a
		// statement about the fold and not about nested sorting.
		name:    "nofold/co-then-sk",
		sql:     "SELECT id FROM t1 ORDER BY n.co, n.sk",
		correct: []int64{3, 2, 1},
		broken:  []int64{3, 2, 1},
		why:     "the non-fold control — this path has no hidden column and no dedup",
	}, {
		name:    "nofold/sk-then-co",
		sql:     "SELECT id FROM t1 ORDER BY n.sk, n.co",
		correct: []int64{3, 2, 1},
		broken:  []int64{3, 2, 1},
		why:     "the non-fold control",
	}}

	for _, tc := range cases {
		// Sequential for the same reason as the test above: the parent owns the DB.
		t.Run(tc.name, func(t *testing.T) {
			var plan string
			if err := db.QueryRowContext(ctx, "EXPLAIN "+tc.sql).Scan(&plan); err != nil {
				t.Logf("EXPLAIN unavailable: %v", err)
			} else {
				t.Logf("plan: %s", plan)
			}

			rows, err := db.QueryContext(ctx, tc.sql)
			if err != nil {
				t.Fatalf("query %q: %v", tc.sql, err)
			}
			defer rows.Close()
			cols, err := rows.Columns()
			if err != nil {
				t.Fatalf("Columns: %v", err)
			}
			var got []int64
			for rows.Next() {
				vals := make([]any, len(cols))
				ptrs := make([]any, len(vals))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					t.Fatalf("Scan: %v", err)
				}
				id, ok := vals[0].(int64)
				if !ok {
					t.Fatalf("row 0 column is %T (%v), want int64", vals[0], vals[0])
				}
				got = append(got, id)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows.Err: %v", err)
			}
			if len(got) != len(tc.correct) {
				t.Fatalf("sql: %s\ngot ids %v (%d rows), want %v (%d rows) — the "+
					"ordering claim cannot be read off a population of the wrong size",
					tc.sql, got, len(got), tc.correct, len(tc.correct))
			}

			isBroken := !equalIDs(tc.broken, tc.correct)
			switch {
			case equalIDs(got, tc.correct) && isBroken:
				t.Fatalf("THE KNOWN-BROKEN SHAPE NOW RETURNS THE CORRECT ROWS — "+
					"flip this arm's `broken` to the correct order and delete this "+
					"branch.\nsql:  %s\nplan: %s\ngot:  %v (correct)\n"+
					"previously: %v, because %s\n\n"+
					"This is a PASS in substance: a multi-key nested ORDER BY through "+
					"the projected-EXISTS fold used to lose its second key.",
					tc.sql, plan, got, tc.broken, tc.why)
			case !equalIDs(got, tc.broken):
				verdict := "a multi-key nested ORDER BY changed its answer"
				if !isBroken {
					verdict = "the non-fold control lost a sort key — the defect has " +
						"SPREAD out of the projected-EXISTS fold"
				}
				t.Fatalf("%s.\nsql:  %s\nplan: %s\ngot:  %v\nthis build returned: %v\n"+
					"SQL requires: %v\nwhy: %s",
					verdict, tc.sql, plan, got, tc.broken, tc.correct, tc.why)
			case isBroken:
				t.Logf("KNOWN-BROKEN, pinned: got %v, SQL requires %v — %s",
					got, tc.correct, tc.why)
			}
		})
	}
}

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestFDB_NestedSortKeyExplainRendersTheStructRoot records the RENDERING half of
// the same question, separately from the rows half, because they are separate
// facts with opposite severities and a single test asserting both would report
// one severity for either failure.
//
// `ORDER BY n.co` renders its sort key as the struct ROOT (`N`) rather than the
// member (`N.CO`). This test pins that this is COSMETIC — it stands beside the
// rows test above, which pins that the same query returns member-ordered rows.
// If a change makes the rendering faithful, this test reds and the correct
// response is to update it; if a change makes the ROWS follow the rendering, the
// test above reds and that is a correctness regression. Keeping them apart is
// what makes the two outcomes distinguishable at a glance.
func TestFDB_NestedSortKeyExplainRendersTheStructRoot(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	const dbPath = "/testdb_nested_sort_explain"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE nske_tmpl "+
			"CREATE TYPE AS STRUCT nst (sk BIGINT, co BIGINT) "+
			"CREATE TABLE t1 (id BIGINT, n nst, PRIMARY KEY (id)) "+
			"CREATE TABLE t2 (id BIGINT, t1_id BIGINT, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE nske_tmpl"); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	const q = "SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h " +
		"FROM t1 ORDER BY n.co"
	var plan string
	if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	t.Logf("plan: %s", plan)

	if !strings.Contains(plan, "InMemorySort([N ASC]") {
		t.Errorf("the flat sort-key RENDERING changed.\nplan: %s\n"+
			"This test pins the COSMETIC half of the nested-sort-key question: the "+
			"key renders as the struct ROOT while the rows follow the MEMBER. If the "+
			"rendering was made faithful (`N.CO`), update this expectation. If it "+
			"changed for any other reason, check the rows test in this file first — "+
			"it is the one that guards correctness.", plan)
	}
	if strings.Contains(plan, "N.CO") {
		t.Logf("the rendering now names the member — the flat-name defect appears fixed")
	}
}
