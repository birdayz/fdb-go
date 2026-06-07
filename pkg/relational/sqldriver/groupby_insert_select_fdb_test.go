package sqldriver_test

import (
	"context"
	"database/sql"
	"strconv"
	"testing"
)

// gbInsertDB sets up src(id,g,v) seeded so GROUP BY g yields two groups, plus a
// few destination tables, for the GROUP BY INSERT…SELECT tests (RFC-084).
func gbInsertDB(t *testing.T, tag string) (*sql.DB, context.Context) {
	t.Helper()
	ctx := context.Background()
	dbPath := "/gbins_" + tag
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	tmpl := "gbins_tmpl_" + tag
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+tmpl+
		" CREATE TABLE src (id BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE dst (id BIGINT, s BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE summary (cat BIGINT, total BIGINT, cnt BIGINT, PRIMARY KEY (cat))"+
		" CREATE TABLE one (s BIGINT, PRIMARY KEY (s))"+
		" CREATE TABLE cnt (c BIGINT, PRIMARY KEY (c))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE "+tmpl); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// g=1 → {10,20} (SUM 30, COUNT 2); g=2 → {30} (SUM 30, COUNT 1).
	if _, err := db.ExecContext(ctx, "INSERT INTO src VALUES (1,1,10),(2,1,20),(3,2,30)"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return db, ctx
}

func readDst(t *testing.T, ctx context.Context, db *sql.DB, q string) [][2]int64 {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer rows.Close()
	var got [][2]int64
	for rows.Next() {
		var a, b int64
		if err := rows.Scan(&a, &b); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, [2]int64{a, b})
	}
	return got
}

// TestFDB_GroupByInsertSelect pins the RFC-084 fix: a bare GROUP BY aggregate
// insert source is aligned to the target columns (was spurious 23505).
func TestFDB_GroupByInsertSelect(t *testing.T) {
	t.Parallel()
	db, ctx := gbInsertDB(t, "basic")

	// The core bug: group key first, single aggregate. Java accepts this
	// (insert_select_java.yaml:60). dst ← (g, SUM(v)) = (1,30),(2,30).
	if _, err := db.ExecContext(ctx, "INSERT INTO dst SELECT g, SUM(v) FROM src GROUP BY g"); err != nil {
		t.Fatalf("INSERT…SELECT GROUP BY (was 23505): %v", err)
	}
	if got := readDst(t, ctx, db, "SELECT id, s FROM dst ORDER BY id"); len(got) != 2 ||
		got[0] != [2]int64{1, 30} || got[1] != [2]int64{2, 30} {
		t.Fatalf("dst = %v, want [[1 30] [2 30]]", got)
	}
}

// TestFDB_GroupByInsertSelect_MultiAggregate mirrors insert_select_java.yaml:60
// (cat, SUM(val), COUNT(*)) — multiple aggregates + group key.
func TestFDB_GroupByInsertSelect_MultiAggregate(t *testing.T) {
	t.Parallel()
	db, ctx := gbInsertDB(t, "multi")
	if _, err := db.ExecContext(ctx, "INSERT INTO summary SELECT g, SUM(v), COUNT(*) FROM src GROUP BY g"); err != nil {
		t.Fatalf("multi-aggregate GROUP BY insert: %v", err)
	}
	rows, err := db.QueryContext(ctx, "SELECT cat, total, cnt FROM summary ORDER BY cat")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	defer rows.Close()
	var got [][3]int64
	for rows.Next() {
		var a, b, c int64
		if err := rows.Scan(&a, &b, &c); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, [3]int64{a, b, c})
	}
	if len(got) != 2 || got[0] != [3]int64{1, 30, 2} || got[1] != [3]int64{2, 30, 1} {
		t.Fatalf("summary = %v, want [[1 30 2] [2 30 1]]", got)
	}
}

// TestFDB_GroupByInsertSelect_Variants pins the reviewer-required axes:
// lowercase arg, AS aliases, reordered SELECT, and the keys==0 HAVING shape.
func TestFDB_GroupByInsertSelect_Variants(t *testing.T) {
	t.Parallel()

	// Uppercase aggregate arg `SUM(V)` referencing the lowercase column `v` —
	// pins case-INSENSITIVE operand resolution agreeing end-to-end (the canonical
	// FieldValue key must match the runtime datum key regardless of the written
	// case). A distinct axis from the core test's `SUM(v)`.
	t.Run("uppercase_arg", func(t *testing.T) {
		db, ctx := gbInsertDB(t, "uc")
		if _, err := db.ExecContext(ctx, "INSERT INTO dst SELECT g, SUM(V) FROM src GROUP BY g"); err != nil {
			t.Fatalf("uppercase arg: %v", err)
		}
		if got := readDst(t, ctx, db, "SELECT id, s FROM dst ORDER BY id"); len(got) != 2 || got[0] != [2]int64{1, 30} {
			t.Fatalf("got %v", got)
		}
	})

	// AS aliases on both columns — the helper sets proj.Aliases (hasAlias path);
	// alignInsertSelectColumns must fully overwrite them positionally to target.
	t.Run("as_aliases", func(t *testing.T) {
		db, ctx := gbInsertDB(t, "as")
		if _, err := db.ExecContext(ctx, "INSERT INTO dst SELECT g AS k, SUM(v) AS total FROM src GROUP BY g"); err != nil {
			t.Fatalf("AS aliases: %v", err)
		}
		if got := readDst(t, ctx, db, "SELECT id, s FROM dst ORDER BY id"); len(got) != 2 || got[1] != [2]int64{2, 30} {
			t.Fatalf("got %v", got)
		}
	})

	// Reordered SELECT `SUM(v), g` — SELECT order preserved by the helper, so it
	// maps positionally to (id, s): id←SUM(v)=30 (same for both groups → would
	// collide), so use distinct sums. Here g=1 SUM=30, g=2 SUM=30 collide; use a
	// target where the aggregate is the non-PK column instead: dst(id=g order)…
	// To keep PKs distinct, map g→id by writing `SELECT g, SUM(v)` order; the
	// reorder case is value-correctness, checked via the DOUBLE-free shape below.
	t.Run("reordered_select", func(t *testing.T) {
		db, ctx := gbInsertDB(t, "ro")
		// id ← SUM(v) (30,30 collide is real) — instead pin that SUM(v),g maps
		// SUM→id and g→s by giving groups distinct sums.
		if _, err := db.ExecContext(ctx, "INSERT INTO src VALUES (4,3,5)"); err != nil {
			t.Fatalf("seed: %v", err)
		}
		// groups: g1 SUM30, g2 SUM30, g3 SUM5 — g1/g2 still collide on SUM→id.
		// Use a fresh src per-group-unique sum: delete and reseed uniquely.
		if _, err := db.ExecContext(ctx, "DELETE FROM src WHERE id >= 0"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO src VALUES (1,1,10),(2,2,20),(3,3,30)"); err != nil {
			t.Fatalf("reseed: %v", err)
		}
		// SUM(v),g → (id=SUM, s=g): (10,1),(20,2),(30,3).
		if _, err := db.ExecContext(ctx, "INSERT INTO dst SELECT SUM(v), g FROM src GROUP BY g"); err != nil {
			t.Fatalf("reordered: %v", err)
		}
		got := readDst(t, ctx, db, "SELECT id, s FROM dst ORDER BY id")
		if len(got) != 3 || got[0] != [2]int64{10, 1} || got[1] != [2]int64{20, 2} || got[2] != [2]int64{30, 3} {
			t.Fatalf("reordered SUM(v),g → (id,s) = %v, want [[10 1] [20 2] [30 3]]", got)
		}
	})

	// Qualified aggregate operand `SUM(s.v)` over an aliased source: on this
	// insert-source path the qualified aggregate's operand is left unresolved
	// (a SEPARATE pre-existing defect) so it computes NULL. The wrap therefore
	// SKIPS qualified-operand sources — leaving the original LOUD failure (unset
	// PK → 23505) rather than silently inserting NULL. Pins that the wrap does NOT
	// corrupt qualified-operand inserts; the loud→correct fix is a tracked follow-up.
	t.Run("qualified_source_stays_loud", func(t *testing.T) {
		db, ctx := gbInsertDB(t, "qs")
		_, err := db.ExecContext(ctx, "INSERT INTO dst SELECT g, SUM(s.v) FROM src s GROUP BY g")
		if err == nil {
			t.Fatal("qualified SUM(s.v) GROUP BY insert must error loudly, not silently insert NULL (wrap skips qualified until operand resolution is fixed)")
		}
	})

	// keys==0 (ungrouped) aggregate with a HAVING over a NON-visible aggregate:
	// only SUM(v) is visible, COUNT(*) must be excluded from the target mapping.
	t.Run("ungrouped_having_nonvisible", func(t *testing.T) {
		db, ctx := gbInsertDB(t, "k0h")
		// COUNT(*) = 3 > 1 → passes; SUM(v) = 60 → INSERT INTO one(s).
		if _, err := db.ExecContext(ctx, "INSERT INTO one SELECT SUM(v) FROM src HAVING COUNT(*) > 1"); err != nil {
			t.Fatalf("ungrouped HAVING: %v", err)
		}
		rows, _ := db.QueryContext(ctx, "SELECT s FROM one")
		defer rows.Close()
		var ss []int64
		for rows.Next() {
			var s int64
			rows.Scan(&s)
			ss = append(ss, s)
		}
		if len(ss) != 1 || ss[0] != 60 {
			t.Fatalf("one = %v, want [60]", ss)
		}
	})
}

// TestFDB_GroupByInsertSelect_HavingStripProject pins that a GROUP BY with a
// HAVING over a non-projected aggregate (which builds a strip Project, so
// findProjection succeeds and the wrap is skipped) still inserts correctly.
func TestFDB_GroupByInsertSelect_HavingStripProject(t *testing.T) {
	t.Parallel()
	db, ctx := gbInsertDB(t, "hsp")
	// HAVING COUNT(*) > 1 → only g=1 (count 2). dst ← (1, 30).
	if _, err := db.ExecContext(ctx, "INSERT INTO dst SELECT g, SUM(v) FROM src GROUP BY g HAVING COUNT(*) > 1"); err != nil {
		t.Fatalf("GROUP BY HAVING insert: %v", err)
	}
	if got := readDst(t, ctx, db, "SELECT id, s FROM dst ORDER BY id"); len(got) != 1 || got[0] != [2]int64{1, 30} {
		t.Fatalf("dst = %v, want [[1 30]]", got)
	}
}

// TestFDB_GroupByInsertSelect_CountStar pins the COUNT(*) shape: a sole
// `SELECT COUNT(*)` is parsed as sq.countStar with EMPTY aggCols, so the wrap
// must synthesize its column — else the bare aggregate keys on "COUNT(*)" and
// buildInsertRecord leaves the target unset (silently wrong, or 23505 under
// GROUP BY).
func TestFDB_GroupByInsertSelect_CountStar(t *testing.T) {
	t.Parallel()

	// Scalar COUNT(*) → 3 rows in src. Was silently inserting 0.
	t.Run("scalar", func(t *testing.T) {
		db, ctx := gbInsertDB(t, "csscalar")
		if _, err := db.ExecContext(ctx, "INSERT INTO cnt SELECT COUNT(*) FROM src"); err != nil {
			t.Fatalf("scalar COUNT(*): %v", err)
		}
		rows, _ := db.QueryContext(ctx, "SELECT c FROM cnt")
		defer rows.Close()
		var got []int64
		for rows.Next() {
			var c int64
			rows.Scan(&c)
			got = append(got, c)
		}
		if len(got) != 1 || got[0] != 3 {
			t.Fatalf("cnt = %v, want [3] (was silently [0])", got)
		}
	})

	// COUNT(*) per group (group key NOT projected) → counts {2,1} into distinct
	// PKs — was a 23505 (all rows keyed the unset PK).
	t.Run("groupby", func(t *testing.T) {
		db, ctx := gbInsertDB(t, "csgb")
		if _, err := db.ExecContext(ctx, "INSERT INTO cnt SELECT COUNT(*) FROM src GROUP BY g"); err != nil {
			t.Fatalf("COUNT(*) GROUP BY (was 23505): %v", err)
		}
		rows, _ := db.QueryContext(ctx, "SELECT c FROM cnt ORDER BY c")
		defer rows.Close()
		var got []int64
		for rows.Next() {
			var c int64
			rows.Scan(&c)
			got = append(got, c)
		}
		if len(got) != 2 || got[0] != 1 || got[1] != 2 {
			t.Fatalf("cnt = %v, want [1 2]", got)
		}
	})
}

// TestFDB_GroupByInsertSelect_Determinism runs the core case repeatedly — a
// plan-time wrap must be stable.
func TestFDB_GroupByInsertSelect_Determinism(t *testing.T) {
	t.Parallel()
	for i := 0; i < 10; i++ {
		db, ctx := gbInsertDB(t, "det"+strconv.Itoa(i))
		if _, err := db.ExecContext(ctx, "INSERT INTO dst SELECT g, SUM(v) FROM src GROUP BY g"); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if got := readDst(t, ctx, db, "SELECT id, s FROM dst ORDER BY id"); len(got) != 2 {
			t.Fatalf("run %d: dst = %v, want 2 rows", i, got)
		}
	}
}
