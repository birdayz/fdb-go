package sqltest

import (
	"context"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

func TestFDB_StringOperations(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := testkit.SetupPlanShapeDB(t, "strop1",
		"CREATE TABLE t1 (id BIGINT, name STRING, PRIMARY KEY (id))")

	for _, q := range []string{
		"INSERT INTO t1 VALUES (1, 'Alice')",
		"INSERT INTO t1 VALUES (2, 'Bob')",
		"INSERT INTO t1 VALUES (3, 'Charlie')",
		"INSERT INTO t1 VALUES (4, NULL)",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	t.Run("like_prefix", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT name FROM t1 WHERE name LIKE 'A%' ORDER BY name")
		if len(rows) != 1 || rows[0][0].(string) != "Alice" {
			t.Fatalf("want [Alice], got %v", rows)
		}
	})

	t.Run("like_suffix", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT name FROM t1 WHERE name LIKE '%ob' ORDER BY name")
		if len(rows) != 1 || rows[0][0].(string) != "Bob" {
			t.Fatalf("want [Bob], got %v", rows)
		}
	})

	t.Run("like_contains", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT name FROM t1 WHERE name LIKE '%li%' ORDER BY name")
		if len(rows) != 2 {
			t.Fatalf("want 2 (Alice, Charlie), got %d: %v", len(rows), rows)
		}
	})

	t.Run("not_like", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT name FROM t1 WHERE name NOT LIKE '%li%' ORDER BY name")
		if len(rows) != 1 || rows[0][0].(string) != "Bob" {
			t.Fatalf("want [Bob], got %v", rows)
		}
	})

	t.Run("like_null_excluded", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT COUNT(*) FROM t1 WHERE name LIKE '%'")
		if len(rows) != 1 || toInt64(rows[0][0]) != 3 {
			t.Fatalf("want 3 (NULL excluded from LIKE), got %v", rows)
		}
	})

	t.Run("order_by_string", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT name FROM t1 WHERE name IS NOT NULL ORDER BY name")
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if rows[0][0].(string) != "Alice" || rows[1][0].(string) != "Bob" || rows[2][0].(string) != "Charlie" {
			t.Errorf("got %v, %v, %v", rows[0][0], rows[1][0], rows[2][0])
		}
	})

	t.Run("order_by_string_desc", func(t *testing.T) {
		rows := testkit.CollectRows(t, db,
			"SELECT name FROM t1 WHERE name IS NOT NULL ORDER BY name DESC")
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if rows[0][0].(string) != "Charlie" || rows[2][0].(string) != "Alice" {
			t.Errorf("got %v, %v", rows[0][0], rows[2][0])
		}
	})
}

// TestFDB_ConcatExpressions — string concatenation and expressions in SELECT
func TestFDB_ConcatExpressions(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "concat", "CREATE TABLE str_t(id BIGINT, first_name STRING, last_name STRING, age BIGINT, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO str_t VALUES
		(1, 'alice', 'smith', 30),
		(2, 'bob', 'jones', 25),
		(3, 'charlie', 'brown', 35)
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("arithmetic_in_select", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id, age * 2 FROM str_t ORDER BY id")
		if len(rows) != 3 {
			t.Fatalf("want 3, got %d", len(rows))
		}
		if toInt64(rows[0][1]) != 60 {
			t.Errorf("30*2=60, got %v", rows[0][1])
		}
	})

	t.Run("arithmetic_in_where", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM str_t WHERE age + 5 > 32 ORDER BY id")
		if len(rows) != 2 {
			t.Fatalf("want 2 (30+5=35>32, 35+5=40>32), got %d: %v", len(rows), rows)
		}
	})

	t.Run("avg_via_sum_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT SUM(age), COUNT(*), SUM(age) / COUNT(*) FROM str_t")
		if len(rows) != 1 {
			t.Fatalf("want 1, got %d", len(rows))
		}
		if toInt64(rows[0][0]) != 90 {
			t.Errorf("SUM(age) = 30+25+35 = 90, got %v", rows[0][0])
		}
		if toInt64(rows[0][1]) != 3 {
			t.Errorf("COUNT = 3, got %v", rows[0][1])
		}
		if toInt64(rows[0][2]) != 30 {
			t.Errorf("AVG = 90/3 = 30, got %v", rows[0][2])
		}
	})
}

// TestFDB_WhereWithLikePatterns — LIKE pattern matching edge cases
func TestFDB_WhereWithLikePatterns(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "likep", "CREATE TABLE lp_t(id BIGINT, name STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO lp_t VALUES
		(1, 'alice'), (2, 'bob'), (3, 'charlie'), (4, 'alex'),
		(5, 'alice_jones'), (6, 'ALICE'), (7, 'al')
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("like_prefix", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM lp_t WHERE name LIKE 'al%' ORDER BY id")
		if len(rows) != 4 {
			t.Fatalf("want 4 (alice, alex, alice_jones, al), got %d: %v", len(rows), rows)
		}
	})

	t.Run("like_suffix", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM lp_t WHERE name LIKE '%ice' ORDER BY id")
		if len(rows) != 1 {
			t.Fatalf("want 1 (alice), got %d: %v", len(rows), rows)
		}
	})

	t.Run("like_contains", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM lp_t WHERE name LIKE '%li%' ORDER BY id")
		if len(rows) != 3 {
			t.Fatalf("want 3 (alice, charlie, alice_jones), got %d: %v", len(rows), rows)
		}
	})

	t.Run("like_exact", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM lp_t WHERE name LIKE 'bob'")
		if len(rows) != 1 || toInt64(rows[0][0]) != 2 {
			t.Errorf("want id=2, got %v", rows)
		}
	})

	t.Run("not_like", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM lp_t WHERE name NOT LIKE 'al%' ORDER BY id")
		if len(rows) != 3 {
			t.Fatalf("want 3 (bob, charlie, ALICE), got %d: %v", len(rows), rows)
		}
	})

	t.Run("like_underscore_wildcard", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM lp_t WHERE name LIKE 'a_' ORDER BY id")
		if len(rows) != 1 || toInt64(rows[0][0]) != 7 {
			t.Errorf("want id=7 (al matches a_), got %v", rows)
		}
	})
}

// TestFDB_EmptyStringVsNull — empty string is NOT NULL
func TestFDB_EmptyStringVsNull(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "esvn", "CREATE TABLE esvn_t(id BIGINT, val STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO esvn_t VALUES (1, ''), (2, NULL), (3, 'hello')"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("empty_string_is_not_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM esvn_t WHERE val IS NOT NULL ORDER BY id")
		if len(rows) != 2 {
			t.Fatalf("want 2 (empty string + hello), got %d: %v", len(rows), rows)
		}
		if toInt64(rows[0][0]) != 1 {
			t.Errorf("first should be id=1 (empty string), got %v", rows[0][0])
		}
	})

	t.Run("null_is_null", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM esvn_t WHERE val IS NULL")
		if len(rows) != 1 || toInt64(rows[0][0]) != 2 {
			t.Errorf("want id=2 (NULL), got %v", rows)
		}
	})

	t.Run("empty_string_eq", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM esvn_t WHERE val = ''")
		if len(rows) != 1 || toInt64(rows[0][0]) != 1 {
			t.Errorf("want id=1, got %v", rows)
		}
	})

	t.Run("count_col_excludes_null_not_empty", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(val) FROM esvn_t")
		if toInt64(rows[0][0]) != 2 {
			t.Errorf("COUNT(val) should be 2 (empty string counts, NULL doesn't), got %v", rows[0][0])
		}
	})
}

// TestFDB_SelectWhereOnString — string comparison operators
func TestFDB_SelectWhereOnString(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "swstr", "CREATE TABLE sws_t(id BIGINT, name STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO sws_t VALUES
		(1, 'alice'), (2, 'bob'), (3, 'charlie'), (4, 'david'), (5, 'eve')
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("string_eq", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM sws_t WHERE name = 'bob'")
		if len(rows) != 1 || toInt64(rows[0][0]) != 2 {
			t.Errorf("want id=2, got %v", rows)
		}
	})

	t.Run("string_ne", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM sws_t WHERE name <> 'bob'")
		if toInt64(rows[0][0]) != 4 {
			t.Errorf("want 4 (!= bob), got %v", rows[0][0])
		}
	})

	t.Run("string_order_by", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT name FROM sws_t ORDER BY name")
		if fmt.Sprintf("%v", rows[0][0]) != "alice" {
			t.Errorf("first alphabetically should be alice, got %v", rows[0][0])
		}
		if fmt.Sprintf("%v", rows[4][0]) != "eve" {
			t.Errorf("last alphabetically should be eve, got %v", rows[4][0])
		}
	})

	t.Run("string_in_list", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM sws_t WHERE name IN ('alice', 'eve') ORDER BY id")
		if len(rows) != 2 {
			t.Fatalf("want 2, got %d", len(rows))
		}
	})
}

// TestFDB_SelectWithMultipleStringColumns — queries on multiple string columns
func TestFDB_SelectWithMultipleStringColumns(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "smsc", "CREATE TABLE smsc_t(id BIGINT, first_name STRING, last_name STRING, city STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO smsc_t VALUES
		(1, 'alice', 'smith', 'nyc'),
		(2, 'bob', 'jones', 'la'),
		(3, 'charlie', 'smith', 'nyc'),
		(4, 'david', 'jones', 'sf')
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("group_by_last_name", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT last_name, COUNT(*) FROM smsc_t GROUP BY last_name ORDER BY last_name")
		if len(rows) != 2 {
			t.Fatalf("want 2 groups, got %d", len(rows))
		}
		if fmt.Sprintf("%v", rows[0][0]) != "jones" || toInt64(rows[0][1]) != 2 {
			t.Errorf("jones: want 2, got %v %v", rows[0][0], rows[0][1])
		}
	})

	t.Run("filter_two_string_columns", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM smsc_t WHERE last_name = 'smith' AND city = 'nyc' ORDER BY id")
		if len(rows) != 2 {
			t.Fatalf("want 2 (alice+charlie), got %d: %v", len(rows), rows)
		}
	})

	t.Run("group_by_city_count", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT city, COUNT(*) FROM smsc_t GROUP BY city ORDER BY city")
		if len(rows) != 3 {
			t.Fatalf("want 3 cities, got %d", len(rows))
		}
	})
}

// TestFDB_InPredicateWithStrings — IN predicate with string values
func TestFDB_InPredicateWithStrings(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "ipws", "CREATE TABLE ipws_t(id BIGINT, color STRING, size STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO ipws_t VALUES
		(1, 'red', 'S'), (2, 'blue', 'M'), (3, 'red', 'L'),
		(4, 'green', 'S'), (5, 'blue', 'XL'), (6, 'red', 'M')
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("in_string_filter", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM ipws_t WHERE color IN ('red', 'green') ORDER BY id")
		if len(rows) != 4 {
			t.Fatalf("want 4 (red:1,3,6 + green:4), got %d: %v", len(rows), rows)
		}
	})

	t.Run("in_with_group_by", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, `
			SELECT color, COUNT(*) FROM ipws_t
			WHERE size IN ('S', 'M')
			GROUP BY color ORDER BY color
		`)
		if len(rows) != 3 {
			t.Fatalf("want 3 (blue:1, green:1, red:2), got %d: %v", len(rows), rows)
		}
	})

	t.Run("not_in_string", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM ipws_t WHERE color NOT IN ('red')")
		if toInt64(rows[0][0]) != 3 {
			t.Errorf("NOT IN red: want 3 (blue:2 + green:1), got %v", rows[0][0])
		}
	})
}

// TestFDB_WhereWithMultipleLikePatterns — multiple LIKE conditions
func TestFDB_WhereWithMultipleLikePatterns(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "wmlp", "CREATE TABLE wmlp_t(id BIGINT, name STRING, email STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO wmlp_t VALUES
		(1, 'alice_smith', 'alice@example.com'),
		(2, 'bob_jones', 'bob@test.org'),
		(3, 'alice_jones', 'aj@example.com'),
		(4, 'charlie_brown', 'cb@test.org')
	`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("like_or_like", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM wmlp_t WHERE name LIKE 'alice%' OR name LIKE 'charlie%' ORDER BY id")
		if len(rows) != 3 {
			t.Fatalf("want 3 (alice_smith, alice_jones, charlie_brown), got %d: %v", len(rows), rows)
		}
	})

	t.Run("like_and_like", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT id FROM wmlp_t WHERE name LIKE '%jones' AND email LIKE '%example%' ORDER BY id")
		if len(rows) != 1 || toInt64(rows[0][0]) != 3 {
			t.Errorf("want id=3 (alice_jones + example.com), got %v", rows)
		}
	})
}

// TestFDB_LikePatternVariants — LIKE with various wildcard patterns
func TestFDB_LikePatternVariants(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "lpv",
		"CREATE TABLE lpv_t(id BIGINT, email STRING, PRIMARY KEY(id))")
	if _, err := db.ExecContext(ctx, `INSERT INTO lpv_t VALUES
		(1,'alice@example.com'),(2,'bob@test.org'),(3,'carol@example.com'),
		(4,'dave@test.net'),(5,'eve@example.org')`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	t.Run("suffix_match", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM lpv_t WHERE email LIKE '%example.com'")
		if toInt64(rows[0][0]) != 2 {
			t.Errorf("want 2 @example.com, got %v", rows[0][0])
		}
	})

	t.Run("prefix_match", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT email FROM lpv_t WHERE email LIKE 'alice%'")
		if len(rows) != 1 || fmt.Sprintf("%v", rows[0][0]) != "alice@example.com" {
			t.Errorf("want alice@example.com, got %v", rows)
		}
	})

	t.Run("contains_match", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM lpv_t WHERE email LIKE '%test%'")
		if toInt64(rows[0][0]) != 2 {
			t.Errorf("want 2 containing 'test', got %v", rows[0][0])
		}
	})

	t.Run("not_like", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT COUNT(*) FROM lpv_t WHERE email NOT LIKE '%example%'")
		if toInt64(rows[0][0]) != 2 {
			t.Errorf("want 2 not containing 'example', got %v", rows[0][0])
		}
	})

	t.Run("like_with_order", func(t *testing.T) {
		rows := testkit.CollectRows(t, db, "SELECT email FROM lpv_t WHERE email LIKE '%@example%' ORDER BY email")
		if len(rows) != 3 {
			t.Fatalf("want 3 @example rows, got %d", len(rows))
		}
		if fmt.Sprintf("%v", rows[0][0]) != "alice@example.com" {
			t.Errorf("first: want alice@example.com, got %v", rows[0][0])
		}
	})
}
