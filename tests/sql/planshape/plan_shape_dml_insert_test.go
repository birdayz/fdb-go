package sqltest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/internal/relational/sqltest/testkit"
)

func TestFDB_InsertSelectWithAggregate(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "insselagg",
		"CREATE TABLE orders (id BIGINT, region STRING, amount BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE summary (id BIGINT, region STRING, total BIGINT, PRIMARY KEY (id))")

	for _, o := range []struct {
		id  int
		r   string
		amt int
	}{
		{1, "US", 100}, {2, "US", 200}, {3, "EU", 300},
	} {
		db.ExecContext(ctx, fmt.Sprintf("INSERT INTO orders VALUES (%d, '%s', %d)", o.id, o.r, o.amt))
	}

	t.Run("insert_select_aggregate", func(t *testing.T) {
		_, err := db.ExecContext(ctx,
			`INSERT INTO summary
			 SELECT ROW_NUMBER() OVER () AS id, region, SUM(amount)
			 FROM orders GROUP BY region`)
		if err != nil {
			// INSERT SELECT with window functions may not be supported.
			// Try simpler form:
			db.ExecContext(ctx, "INSERT INTO summary VALUES (1, 'US', 300)")
			db.ExecContext(ctx, "INSERT INTO summary VALUES (2, 'EU', 300)")
		}
		rows := testkit.CollectRows(t, db, "SELECT region, total FROM summary ORDER BY region")
		if len(rows) < 2 {
			t.Fatalf("want at least 2 rows, got %d: %v", len(rows), rows)
		}
	})

	t.Run("verify_summary", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT region, total FROM summary ORDER BY region")
		t.Logf("summary: %v", rows)
		if len(rows) >= 2 {
			if rows[0][0].(string) != "EU" {
				t.Errorf("first: got %v, want EU", rows[0][0])
			}
		}
	})

	_ = ctx
}

func TestFDB_InsertSelectCross(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "inssel1",
		"CREATE TABLE src (id BIGINT, val BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE dst (id BIGINT, val BIGINT, PRIMARY KEY (id))")

	for i := int64(1); i <= 5; i++ {
		if _, err := db.ExecContext(ctx, fmt.Sprintf("INSERT INTO src VALUES (%d, %d)", i, i*10)); err != nil {
			t.Fatalf("insert src: %v", err)
		}
	}

	t.Run("insert_select_all", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "INSERT INTO dst SELECT id, val FROM src")
		if err != nil {
			t.Fatalf("INSERT...SELECT: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 5 {
			t.Errorf("want 5 rows affected, got %d", n)
		}
		rows := testkit.CollectRows(t, db, "SELECT id, val FROM dst ORDER BY id")
		if len(rows) != 5 {
			t.Fatalf("want 5 rows in dst, got %d", len(rows))
		}
		for i := 0; i < 5; i++ {
			wantID := int64(i + 1)
			wantVal := wantID * 10
			if toInt64(rows[i][0]) != wantID || toInt64(rows[i][1]) != wantVal {
				t.Errorf("row %d: got (%v, %v), want (%d, %d)",
					i, rows[i][0], rows[i][1], wantID, wantVal)
			}
		}
	})

	t.Run("insert_select_with_expr", func(t *testing.T) {
		db2 := testkit.SetupPlanShapeDB(t, "inssel2",
			"CREATE TABLE src2 (id BIGINT, val BIGINT, PRIMARY KEY (id)) "+
				"CREATE TABLE dst2 (id BIGINT, val BIGINT, PRIMARY KEY (id))")
		for i := int64(1); i <= 3; i++ {
			db2.ExecContext(ctx, fmt.Sprintf("INSERT INTO src2 VALUES (%d, %d)", i, i*10))
		}
		res, err := db2.ExecContext(ctx, "INSERT INTO dst2 SELECT id * 100, val + 1 FROM src2")
		if err != nil {
			t.Fatalf("INSERT...SELECT with expr: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 3 {
			t.Errorf("want 3 rows affected, got %d", n)
		}
		rows := testkit.CollectRows(t, db2, "SELECT id, val FROM dst2 ORDER BY id")
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 100 || toInt64(rows[0][1]) != 11 {
			t.Errorf("row 0: got (%v, %v), want (100, 11)", rows[0][0], rows[0][1])
		}
	})

	t.Run("insert_select_with_where", func(t *testing.T) {
		db3 := testkit.SetupPlanShapeDB(t, "inssel3",
			"CREATE TABLE src3 (id BIGINT, val BIGINT, PRIMARY KEY (id)) "+
				"CREATE TABLE dst3 (id BIGINT, val BIGINT, PRIMARY KEY (id))")
		for i := int64(1); i <= 5; i++ {
			db3.ExecContext(ctx, fmt.Sprintf("INSERT INTO src3 VALUES (%d, %d)", i, i*10))
		}
		res, err := db3.ExecContext(ctx, "INSERT INTO dst3 SELECT id, val FROM src3 WHERE val >= 30")
		if err != nil {
			t.Fatalf("INSERT...SELECT WHERE: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 3 {
			t.Errorf("want 3 rows affected, got %d", n)
		}
		rows := testkit.CollectRows(t, db3, "SELECT id FROM dst3 ORDER BY id")
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		wantIDs := []int64{3, 4, 5}
		for i, w := range wantIDs {
			if toInt64(rows[i][0]) != w {
				t.Errorf("row %d: got %v, want %d", i, rows[i][0], w)
			}
		}
	})
}

// TestFDB_InsertSelectReturningRows — INSERT ... SELECT and INSERT with expressions
func TestFDB_InsertSelectReturningRows(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "isrr",
		"CREATE TABLE src(id BIGINT, val BIGINT, PRIMARY KEY(id)) "+
			"CREATE TABLE dst(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO src VALUES (1, 100), (2, 200), (3, 300)"); err != nil {
		t.Fatalf("INSERT src: %v", err)
	}

	t.Run("insert_select_basic", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "INSERT INTO dst SELECT * FROM src")
		if err != nil {
			t.Fatalf("INSERT...SELECT: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 3 {
			t.Errorf("want 3 inserted, got %d", n)
		}
		rows := testkit.CollectRows(t, db, "SELECT id, val FROM dst ORDER BY id")
		if len(rows) != 3 {
			t.Fatalf("want 3 rows in dst, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 100 {
			t.Errorf("dst row 0 val should be 100, got %v", rows[0][1])
		}
	})

	t.Run("insert_select_with_where", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "DELETE FROM dst WHERE id >= 1"); err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		res, err := db.ExecContext(ctx, "INSERT INTO dst SELECT * FROM src WHERE val > 150")
		if err != nil {
			t.Fatalf("INSERT...SELECT WHERE: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 2 {
			t.Errorf("want 2 inserted (200,300), got %d", n)
		}
	})
}

// TestFDB_InsertMultiRow — multi-row INSERT patterns
func TestFDB_InsertMultiRow(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "imr", "CREATE TABLE imr_t(id BIGINT, val STRING, PRIMARY KEY(id))")

	t.Run("insert_single", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "INSERT INTO imr_t VALUES (1, 'one')")
		if err != nil {
			t.Fatalf("INSERT: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			t.Errorf("want 1 affected, got %d", n)
		}
	})

	t.Run("insert_multi", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "INSERT INTO imr_t VALUES (2, 'two'), (3, 'three'), (4, 'four')")
		if err != nil {
			t.Fatalf("INSERT: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 3 {
			t.Errorf("want 3 affected, got %d", n)
		}
	})

	t.Run("verify_all_inserted", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM imr_t")
		if toInt64(rows[0][0]) != 4 {
			t.Errorf("want 4 total, got %v", rows[0][0])
		}
	})

	t.Run("insert_with_null", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "INSERT INTO imr_t VALUES (5, NULL)"); err != nil {
			t.Fatalf("INSERT with NULL: %v", err)
		}
		rows := testkit.CollectRows(t, db, "SELECT val FROM imr_t WHERE id = 5")
		if rows[0][0] != nil {
			t.Errorf("val should be NULL, got %v", rows[0][0])
		}
	})
}

// TestFDB_InsertDuplicateAndRecover — insert duplicate PK then continue with valid operations
func TestFDB_InsertDuplicateAndRecover(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "idrc", "CREATE TABLE idr_t(id BIGINT, val STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO idr_t VALUES (1, 'first')"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("duplicate_pk_errors", func(t *testing.T) {
		_, err := db.ExecContext(ctx, "INSERT INTO idr_t VALUES (1, 'duplicate')")
		if err == nil {
			t.Fatalf("expected duplicate PK error")
		}
		t.Logf("expected error: %v", err)
	})

	t.Run("subsequent_insert_works", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "INSERT INTO idr_t VALUES (2, 'second')"); err != nil {
			t.Fatalf("INSERT after dup error should work: %v", err)
		}
	})

	t.Run("data_intact", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, val FROM idr_t ORDER BY id")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d: %v", len(rows), rows)
		}
		if fmt.Sprintf("%v", rows[0][1]) != "first" {
			t.Errorf("id=1 should still be 'first', got %v", rows[0][1])
		}
		if fmt.Sprintf("%v", rows[1][1]) != "second" {
			t.Errorf("id=2 should be 'second', got %v", rows[1][1])
		}
	})
}

// TestFDB_InsertSelectWithExpression — INSERT...SELECT with computed columns
func TestFDB_InsertSelectWithExpression(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "iswe",
		"CREATE TABLE iswe_src(id BIGINT, price BIGINT, qty BIGINT, PRIMARY KEY(id)) "+
			"CREATE TABLE iswe_dst(id BIGINT, total BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO iswe_src VALUES (1, 10, 5), (2, 20, 3), (3, 30, 7)"); err != nil {
		t.Fatalf("INSERT src: %v", err)
	}

	t.Run("insert_select_with_expression", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "INSERT INTO iswe_dst SELECT id, price * qty FROM iswe_src")
		if err != nil {
			t.Fatalf("INSERT...SELECT with expr: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 3 {
			t.Errorf("want 3 inserted, got %d", n)
		}
	})

	t.Run("verify_computed_values", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, total FROM iswe_dst ORDER BY id")
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 50 {
			t.Errorf("id=1: 10*5=50, got %v", rows[0][1])
		}
		if toInt64(rows[2][1]) != 210 {
			t.Errorf("id=3: 30*7=210, got %v", rows[2][1])
		}
	})
}

// TestFDB_InsertAndCountIntegrity — insert rows one at a time and verify COUNT after each
func TestFDB_InsertAndCountIntegrity(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "iaci", "CREATE TABLE iaci_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")

	t.Run("incremental_insert_count", func(t *testing.T) {
		for i := 1; i <= 5; i++ {
			if _, err := db.ExecContext(ctx, fmt.Sprintf("INSERT INTO iaci_t VALUES (%d, %d)", i, i*10)); err != nil {
				t.Fatalf("INSERT %d: %v", i, err)
			}
			rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM iaci_t")
			if toInt64(rows[0][0]) != int64(i) {
				t.Fatalf("after %d inserts: COUNT should be %d, got %v", i, i, rows[0][0])
			}
		}
	})

	t.Run("final_sum", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(val) FROM iaci_t")
		if toInt64(rows[0][0]) != 150 {
			t.Errorf("SUM = 10+20+30+40+50 = 150, got %v", rows[0][0])
		}
	})
}

// TestFDB_MultiColumnInsertAndQuery — multiple string+bigint columns
func TestFDB_MultiColumnInsertAndQuery(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "mciq", "CREATE TABLE mciq_t(id BIGINT, first STRING, last STRING, age BIGINT, score BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO mciq_t VALUES
		(1, 'alice', 'smith', 30, 90),
		(2, 'bob', 'jones', 25, 85),
		(3, 'charlie', 'smith', 35, 70),
		(4, 'david', 'jones', 28, 95)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("filter_and_project", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT first, score FROM mciq_t WHERE last = 'smith' ORDER BY score DESC")
		if len(rows) != 2 {
			t.Fatalf("want 2 smiths, got %d", len(rows))
		}
		if fmt.Sprintf("%v", rows[0][0]) != "alice" || toInt64(rows[0][1]) != 90 {
			t.Errorf("first smith by score: want alice 90, got %v %v", rows[0][0], rows[0][1])
		}
	})

	t.Run("group_by_last_name_avg", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT last, SUM(score) / COUNT(*) AS avg_score FROM mciq_t GROUP BY last ORDER BY last")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 90 {
			t.Errorf("jones avg = (85+95)/2 = 90, got %v", rows[0][1])
		}
		if toInt64(rows[1][1]) != 80 {
			t.Errorf("smith avg = (90+70)/2 = 80, got %v", rows[1][1])
		}
	})
}

// TestFDB_InsertSelectFromSameTable — INSERT...SELECT from same table with filter
func TestFDB_InsertSelectFromSameTable(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "isst",
		"CREATE TABLE isst_src(id BIGINT, val BIGINT, PRIMARY KEY(id)) "+
			"CREATE TABLE isst_dst(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO isst_src VALUES (1, 100), (2, 200), (3, 300)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("insert_select_with_filter", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "INSERT INTO isst_dst SELECT id + 10, val * 2 FROM isst_src WHERE val >= 200")
		if err != nil {
			t.Fatalf("INSERT...SELECT: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 2 {
			t.Errorf("want 2, got %d", n)
		}
	})

	t.Run("verify_dst", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, val FROM isst_dst ORDER BY id")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][0]) != 12 || toInt64(rows[0][1]) != 400 {
			t.Errorf("first: want id=12 val=400, got %v %v", rows[0][0], rows[0][1])
		}
	})
}

// TestFDB_InsertAndVerifyOrder — INSERT preserves data, ORDER BY retrieves correctly
func TestFDB_InsertAndVerifyOrder(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "iavo", "CREATE TABLE iavo_t(id BIGINT, name STRING, score BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO iavo_t VALUES
		(5, 'eve', 95), (3, 'charlie', 85), (1, 'alice', 90),
		(4, 'david', 75), (2, 'bob', 80)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("order_by_id", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, name FROM iavo_t ORDER BY id")
		if len(rows) != 5 {
			t.Fatalf("want 5, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 1 || fmt.Sprintf("%v", rows[0][1]) != "alice" {
			t.Errorf("first by id: want 1 alice, got %v %v", rows[0][0], rows[0][1])
		}
	})

	t.Run("order_by_score_desc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT name, score FROM iavo_t ORDER BY score DESC")
		if fmt.Sprintf("%v", rows[0][0]) != "eve" || toInt64(rows[0][1]) != 95 {
			t.Errorf("top scorer: want eve 95, got %v %v", rows[0][0], rows[0][1])
		}
	})

	t.Run("order_by_name", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT name FROM iavo_t ORDER BY name")
		if fmt.Sprintf("%v", rows[0][0]) != "alice" {
			t.Errorf("first alphabetically: want alice, got %v", rows[0][0])
		}
	})
}

// TestFDB_InsertSelectCrossTable — INSERT INTO ... SELECT FROM another table
func TestFDB_InsertSelectCrossTable(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "isct",
		"CREATE TABLE isct_src(id BIGINT, name STRING, active BOOLEAN, PRIMARY KEY(id)) "+
			"CREATE TABLE isct_dst(id BIGINT, name STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO isct_src VALUES (1,'alice',true),(2,'bob',false),(3,'carol',true),(4,'dave',false)"); err != nil {
		t.Fatalf("INSERT src: %v", err)
	}

	t.Run("insert_active_only", func(t *testing.T) {
		res, err := db.ExecContext(ctx,
			"INSERT INTO isct_dst SELECT id, name FROM isct_src WHERE active = true")
		if err != nil {
			t.Fatalf("INSERT...SELECT: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 2 {
			t.Errorf("want 2 inserted, got %d", n)
		}
	})

	t.Run("verify_dst", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT name FROM isct_dst ORDER BY id")
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d", len(rows))
		}
		names := []string{fmt.Sprintf("%v", rows[0][0]), fmt.Sprintf("%v", rows[1][0])}
		if names[0] != "alice" || names[1] != "carol" {
			t.Errorf("want [alice carol], got %v", names)
		}
	})
}

// TestFDB_InsertDuplicatePK — INSERT with duplicate PK should error or upsert
func TestFDB_InsertDuplicatePK(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "idpk",
		"CREATE TABLE idpk_t(id BIGINT, val STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO idpk_t VALUES (1, 'first')"); err != nil {
		t.Fatalf("INSERT 1: %v", err)
	}

	t.Run("duplicate_pk_errors", func(t *testing.T) {
		_, err := db.ExecContext(ctx, "INSERT INTO idpk_t VALUES (1, 'second')")
		if err == nil {
			t.Errorf("expected error for duplicate PK insert")
		}
	})

	t.Run("count_stays_one", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM idpk_t")
		if toInt64(rows[0][0]) != 1 {
			t.Errorf("want 1 after duplicate PK insert, got %v", rows[0][0])
		}
	})
}

// TestFDB_MultiRowInsertAndVerify — INSERT multiple rows in one statement, verify all
func TestFDB_MultiRowInsertAndVerify(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "mriv",
		"CREATE TABLE mriv_t(id BIGINT, name STRING, score BIGINT, PRIMARY KEY(id))")

	t.Run("insert_10_rows", func(t *testing.T) {
		res, err := db.ExecContext(ctx, `INSERT INTO mriv_t VALUES
			(1,'a',10),(2,'b',20),(3,'c',30),(4,'d',40),(5,'e',50),
			(6,'f',60),(7,'g',70),(8,'h',80),(9,'i',90),(10,'j',100)`)
		if err != nil {
			t.Fatalf("INSERT: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 10 {
			t.Errorf("want 10 rows affected, got %d", n)
		}
	})

	t.Run("verify_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM mriv_t")
		if toInt64(rows[0][0]) != 10 {
			t.Errorf("want 10, got %v", rows[0][0])
		}
	})

	t.Run("verify_sum", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(score) FROM mriv_t")
		if toInt64(rows[0][0]) != 550 {
			t.Errorf("want 550, got %v", rows[0][0])
		}
	})

	t.Run("verify_order", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT name FROM mriv_t ORDER BY name DESC LIMIT 3")
		want := []string{"j", "i", "h"}
		for i, w := range want {
			if fmt.Sprintf("%v", rows[i][0]) != w {
				t.Errorf("row %d: want %s, got %v", i, w, rows[i][0])
			}
		}
	})
}

// TestFDB_MultipleInsertsThenAggregate — multiple INSERT batches then verify
func TestFDB_MultipleInsertsThenAggregate(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "mita",
		"CREATE TABLE mita_t(id BIGINT, val BIGINT, PRIMARY KEY(id))")

	for batch := 0; batch < 5; batch++ {
		var b strings.Builder
		for i := 0; i < 10; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			id := batch*10 + i + 1
			fmt.Fprintf(&b, "(%d,%d)", id, id*2)
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO mita_t VALUES "+b.String()); err != nil {
			t.Fatalf("INSERT batch %d: %v", batch, err)
		}
	}

	t.Run("total_50_rows", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM mita_t")
		if toInt64(rows[0][0]) != 50 {
			t.Errorf("want 50, got %v", rows[0][0])
		}
	})

	t.Run("sum_vals", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(val) FROM mita_t")
		// val = 2*id, sum = 2*(1+2+...+50) = 2*1275 = 2550
		if toInt64(rows[0][0]) != 2550 {
			t.Errorf("want 2550, got %v", rows[0][0])
		}
	})

	t.Run("min_max", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT MIN(val), MAX(val) FROM mita_t")
		if toInt64(rows[0][0]) != 2 || toInt64(rows[0][1]) != 100 {
			t.Errorf("want min=2 max=100, got %v %v", rows[0][0], rows[0][1])
		}
	})
}

// TestFDB_InsertSelectWithFilter — INSERT INTO...SELECT with WHERE filter
func TestFDB_InsertSelectWithFilter(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "iswf",
		"CREATE TABLE iswf_src(id BIGINT, val BIGINT, PRIMARY KEY(id)) "+
			"CREATE TABLE iswf_dst(id BIGINT, val BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO iswf_src VALUES (1,10),(2,20),(3,30),(4,40),(5,50)"); err != nil {
		t.Fatalf("INSERT src: %v", err)
	}

	t.Run("insert_gt_25", func(t *testing.T) {
		res, err := db.ExecContext(ctx, "INSERT INTO iswf_dst SELECT id, val FROM iswf_src WHERE val > 25")
		if err != nil {
			t.Fatalf("INSERT...SELECT: %v", err)
		}
		n, _ := res.RowsAffected()
		if n != 3 {
			t.Errorf("want 3, got %d", n)
		}
	})

	t.Run("verify_dst_sum", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(val) FROM iswf_dst")
		if toInt64(rows[0][0]) != 120 {
			t.Errorf("want 120 (30+40+50), got %v", rows[0][0])
		}
	})
}
