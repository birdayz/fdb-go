package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/api"
)

func TestFDB_CorrelatedScalarCardinality_AllConsumers(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_cq4_scalar")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_cq4_scalar")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE cq4_scalar "+
		"CREATE TABLE parent (id BIGINT NOT NULL, wanted BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE child (id BIGINT NOT NULL, parent_id BIGINT, grp STRING, val BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE marker (id BIGINT NOT NULL, parent_id BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_cq4_scalar/s WITH TEMPLATE cq4_scalar")
	dsn := fmt.Sprintf("fdbsql:///testdb_cq4_scalar?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Parent 1 has two scalar rows and two aggregate groups. Its wanted=20
	// deliberately matches exactly one of them: filtering before strict
	// cardinality would therefore hide the violation, so this is the barrier
	// discriminator. Parent 2 has one row, parent 3 none, parent 4 two raw rows
	// in one group (one grouped scalar row, SUM=5).
	mwjoMustExec(t, db, ctx, "INSERT INTO parent VALUES (1,20),(2,30),(3,40),(4,5)")
	mwjoMustExec(t, db, ctx, "INSERT INTO child VALUES "+
		"(10,1,'a',10),(11,1,'b',20),(20,2,'a',30),(40,4,'a',2),(41,4,'a',3)")
	mwjoMustExec(t, db, ctx, "INSERT INTO marker VALUES (100,1),(200,2),(300,3),(400,4)")

	t.Run("projection_grouped_multiple_groups_21000", func(t *testing.T) {
		err := expectError(t, db,
			"SELECT (SELECT SUM(c.val) FROM child c WHERE c.parent_id = p.id GROUP BY c.grp) "+
				"FROM parent p WHERE p.id = 1")
		requireSQLSTATE(t, err, api.ErrCodeCardinalityViolation)
	})

	t.Run("simplified_away_correlation_still_21000", func(t *testing.T) {
		for _, query := range []string{
			"SELECT (SELECT c.val FROM child c WHERE c.parent_id = p.id OR 1 = 1) " +
				"FROM parent p WHERE p.id = 1",
			"SELECT p.id FROM parent p WHERE p.id = 1 AND p.wanted = " +
				"(SELECT c.val FROM child c WHERE c.parent_id = p.id OR 1 = 1)",
		} {
			err := expectError(t, db, query)
			requireSQLSTATE(t, err, api.ErrCodeCardinalityViolation)
		}
	})

	t.Run("projection_group_key_only_multiple_groups_21000", func(t *testing.T) {
		err := expectError(t, db,
			"SELECT (SELECT c.grp FROM child c WHERE c.parent_id = p.id GROUP BY c.grp) "+
				"FROM parent p WHERE p.id = 1")
		requireSQLSTATE(t, err, api.ErrCodeCardinalityViolation)
	})

	t.Run("projection_grouped_order_without_limit_still_21000", func(t *testing.T) {
		err := expectError(t, db,
			"SELECT (SELECT SUM(c.val) FROM child c WHERE c.parent_id = p.id "+
				"GROUP BY c.grp ORDER BY SUM(c.val) DESC) FROM parent p WHERE p.id = 1")
		requireSQLSTATE(t, err, api.ErrCodeCardinalityViolation)
	})

	t.Run("projection_grouped_having_cardinality_is_post_having", func(t *testing.T) {
		var one int64
		if err := db.QueryRowContext(ctx,
			"SELECT (SELECT SUM(c.val) FROM child c WHERE c.parent_id = p.id "+
				"GROUP BY c.grp HAVING SUM(c.val) = 20) FROM parent p WHERE p.id = 1",
		).Scan(&one); err != nil {
			t.Fatalf("one surviving HAVING group: %v", err)
		}
		if one != 20 {
			t.Fatalf("one surviving HAVING group = %d, want 20", one)
		}

		var zero sql.NullInt64
		if err := db.QueryRowContext(ctx,
			"SELECT (SELECT SUM(c.val) FROM child c WHERE c.parent_id = p.id "+
				"GROUP BY c.grp HAVING SUM(c.val) > 100) FROM parent p WHERE p.id = 1",
		).Scan(&zero); err != nil {
			t.Fatalf("zero surviving HAVING groups: %v", err)
		}
		if zero.Valid {
			t.Fatalf("zero surviving HAVING groups = %d, want NULL", zero.Int64)
		}

		err := expectError(t, db,
			"SELECT (SELECT SUM(c.val) FROM child c WHERE c.parent_id = p.id "+
				"GROUP BY c.grp HAVING SUM(c.val) > 0) FROM parent p WHERE p.id = 1")
		requireSQLSTATE(t, err, api.ErrCodeCardinalityViolation)
	})

	t.Run("projection_grouped_joined_outer_21000", func(t *testing.T) {
		err := expectError(t, db,
			"SELECT (SELECT SUM(c.val) FROM child c WHERE c.parent_id = p.id GROUP BY c.grp) "+
				"FROM parent p JOIN marker m ON m.parent_id = p.id WHERE p.id = 1")
		requireSQLSTATE(t, err, api.ErrCodeCardinalityViolation)
	})

	t.Run("where_comparison_two_rows_one_matches_still_21000", func(t *testing.T) {
		err := expectError(t, db,
			"SELECT p.id FROM parent p "+
				"WHERE p.id = 1 AND p.wanted = (SELECT c.val FROM child c WHERE c.parent_id = p.id)")
		requireSQLSTATE(t, err, api.ErrCodeCardinalityViolation)
	})

	t.Run("where_comparison_grouped_multiple_groups_21000", func(t *testing.T) {
		err := expectError(t, db,
			"SELECT p.id FROM parent p WHERE p.id = 1 AND p.wanted = "+
				"(SELECT SUM(c.val) FROM child c WHERE c.parent_id = p.id GROUP BY c.grp)")
		requireSQLSTATE(t, err, api.ErrCodeCardinalityViolation)
	})

	t.Run("single_empty_and_one_group", func(t *testing.T) {
		var got int64
		if err := db.QueryRowContext(ctx,
			"SELECT (SELECT c.val FROM child c WHERE c.parent_id = p.id) FROM parent p WHERE p.id = 2",
		).Scan(&got); err != nil {
			t.Fatalf("single scalar: %v", err)
		}
		if got != 30 {
			t.Fatalf("single scalar = %d, want 30", got)
		}

		var empty sql.NullInt64
		if err := db.QueryRowContext(ctx,
			"SELECT (SELECT c.val FROM child c WHERE c.parent_id = p.id) FROM parent p WHERE p.id = 3",
		).Scan(&empty); err != nil {
			t.Fatalf("empty scalar: %v", err)
		}
		if empty.Valid {
			t.Fatalf("empty scalar = %d, want NULL", empty.Int64)
		}

		var grouped int64
		if err := db.QueryRowContext(ctx,
			"SELECT (SELECT SUM(c.val) FROM child c WHERE c.parent_id = p.id GROUP BY c.grp) "+
				"FROM parent p WHERE p.id = 4",
		).Scan(&grouped); err != nil {
			t.Fatalf("one grouped scalar: %v", err)
		}
		if grouped != 5 {
			t.Fatalf("one grouped scalar = %d, want 5", grouped)
		}
	})

	t.Run("where_single_matches_and_empty_drops", func(t *testing.T) {
		var id int64
		if err := db.QueryRowContext(ctx,
			"SELECT p.id FROM parent p WHERE p.id = 2 AND p.wanted = "+
				"(SELECT c.val FROM child c WHERE c.parent_id = p.id)",
		).Scan(&id); err != nil {
			t.Fatalf("single WHERE scalar: %v", err)
		}
		if id != 2 {
			t.Fatalf("single WHERE scalar id = %d, want 2", id)
		}
		var count int64
		if err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM parent p WHERE p.id = 3 AND p.wanted = "+
				"(SELECT c.val FROM child c WHERE c.parent_id = p.id)",
		).Scan(&count); err != nil {
			t.Fatalf("empty WHERE scalar: %v", err)
		}
		if count != 0 {
			t.Fatalf("empty WHERE scalar matched %d rows, want 0", count)
		}

		if err := db.QueryRowContext(ctx,
			"SELECT p.id FROM parent p WHERE p.id = 3 AND "+
				"(SELECT c.val FROM child c WHERE c.parent_id = p.id) IS NULL",
		).Scan(&id); err != nil {
			t.Fatalf("empty WHERE scalar IS NULL: %v", err)
		}
		if id != 3 {
			t.Fatalf("empty WHERE scalar IS NULL id = %d, want 3", id)
		}
	})

	t.Run("where_scalar_lhs_preserves_outer_schema_order", func(t *testing.T) {
		var wanted, id int64
		if err := db.QueryRowContext(ctx,
			"SELECT p.wanted, p.id FROM parent p WHERE p.id = 2 AND "+
				"(SELECT c.val FROM child c WHERE c.parent_id = p.id) = p.wanted",
		).Scan(&wanted, &id); err != nil {
			t.Fatalf("scalar-LHS WHERE with reordered outer projection: %v", err)
		}
		if wanted != 30 || id != 2 {
			t.Fatalf("scalar-LHS WHERE output = [%d,%d], want [30,2]", wanted, id)
		}
	})

	t.Run("explicit_limit_and_offset_exact", func(t *testing.T) {
		cases := []struct {
			name  string
			query string
			want  sql.NullInt64
		}{
			{
				name: "limit_one_top",
				query: "SELECT (SELECT c.val FROM child c WHERE c.parent_id = p.id " +
					"ORDER BY c.val DESC LIMIT 1) FROM parent p WHERE p.id = 1",
				want: sql.NullInt64{Int64: 20, Valid: true},
			},
			{
				name: "limit_one_offset_one",
				query: "SELECT (SELECT c.val FROM child c WHERE c.parent_id = p.id " +
					"ORDER BY c.val DESC LIMIT 1 OFFSET 1) FROM parent p WHERE p.id = 1",
				want: sql.NullInt64{Int64: 10, Valid: true},
			},
			{
				name: "limit_zero",
				query: "SELECT (SELECT c.val FROM child c WHERE c.parent_id = p.id " +
					"LIMIT 0) FROM parent p WHERE p.id = 1",
				want: sql.NullInt64{},
			},
			{
				name: "grouped_limit_one_top",
				query: "SELECT (SELECT SUM(c.val) FROM child c WHERE c.parent_id = p.id GROUP BY c.grp " +
					"ORDER BY SUM(c.val) DESC LIMIT 1) FROM parent p WHERE p.id = 1",
				want: sql.NullInt64{Int64: 20, Valid: true},
			},
			{
				name: "grouped_limit_one_offset_one",
				query: "SELECT (SELECT SUM(c.val) FROM child c WHERE c.parent_id = p.id GROUP BY c.grp " +
					"ORDER BY SUM(c.val) DESC LIMIT 1 OFFSET 1) FROM parent p WHERE p.id = 1",
				want: sql.NullInt64{Int64: 10, Valid: true},
			},
			{
				name: "grouped_limit_zero",
				query: "SELECT (SELECT SUM(c.val) FROM child c WHERE c.parent_id = p.id GROUP BY c.grp " +
					"LIMIT 0) FROM parent p WHERE p.id = 1",
				want: sql.NullInt64{},
			},
			{
				name: "global_aggregate_limit_five",
				query: "SELECT (SELECT MAX(c.val) FROM child c WHERE c.parent_id = p.id " +
					"LIMIT 5) FROM parent p WHERE p.id = 1",
				want: sql.NullInt64{Int64: 20, Valid: true},
			},
			{
				name: "global_aggregate_limit_five_offset_one",
				query: "SELECT (SELECT MAX(c.val) FROM child c WHERE c.parent_id = p.id " +
					"LIMIT 5 OFFSET 1) FROM parent p WHERE p.id = 1",
				want: sql.NullInt64{},
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				var got sql.NullInt64
				if err := db.QueryRowContext(ctx, tc.query).Scan(&got); err != nil {
					t.Fatalf("query: %v", err)
				}
				if got != tc.want {
					t.Fatalf("scalar = %+v, want %+v", got, tc.want)
				}
			})
		}

		var id int64
		if err := db.QueryRowContext(ctx,
			"SELECT p.id FROM parent p WHERE p.wanted = "+
				"(SELECT c.val FROM child c WHERE c.parent_id = p.id ORDER BY c.val DESC LIMIT 1) "+
				"AND p.id = 1",
		).Scan(&id); err != nil {
			t.Fatalf("WHERE LIMIT 1: %v", err)
		}
		if id != 1 {
			t.Fatalf("WHERE LIMIT 1 id = %d, want 1", id)
		}
	})

	t.Run("limit_greater_than_one_typed_loud", func(t *testing.T) {
		for _, query := range []string{
			"SELECT (SELECT c.val FROM child c WHERE c.parent_id = p.id LIMIT 5) FROM parent p",
			"SELECT p.id FROM parent p WHERE p.wanted = (SELECT c.val FROM child c WHERE c.parent_id = p.id LIMIT 2)",
			"SELECT (SELECT SUM(c.val) FROM child c WHERE c.parent_id = p.id GROUP BY c.grp LIMIT 2) FROM parent p",
		} {
			err := expectError(t, db, query)
			requireSQLSTATE(t, err, api.ErrCodeUnsupportedQuery)
		}
	})

	t.Run("group_key_only_having_typed_loud", func(t *testing.T) {
		err := expectError(t, db,
			"SELECT (SELECT c.grp FROM child c WHERE c.parent_id = p.id "+
				"GROUP BY c.grp HAVING c.grp = 'a') FROM parent p")
		requireSQLSTATE(t, err, api.ErrCodeUnsupportedQuery)
	})

	t.Run("bound_limit_preserves_guard", func(t *testing.T) {
		var got int64
		if err := db.QueryRowContext(ctx,
			"SELECT (SELECT c.val FROM child c WHERE c.parent_id = p.id ORDER BY c.val DESC LIMIT ?) "+
				"FROM parent p WHERE p.id = 1", 1,
		).Scan(&got); err != nil {
			t.Fatalf("bound LIMIT 1: %v", err)
		}
		if got != 20 {
			t.Fatalf("bound LIMIT 1 scalar = %d, want 20", got)
		}
		rows, boundErr := db.QueryContext(ctx,
			"SELECT (SELECT c.val FROM child c WHERE c.parent_id = p.id LIMIT ?) FROM parent p", 2)
		if boundErr == nil {
			for rows.Next() {
				var v sql.NullInt64
				if scanErr := rows.Scan(&v); scanErr != nil {
					boundErr = scanErr
					break
				}
			}
			if boundErr == nil {
				boundErr = rows.Err()
			}
			_ = rows.Close()
		}
		requireSQLSTATE(t, boundErr, api.ErrCodeUnsupportedQuery)
	})

	t.Run("dml_correlated_scalar_stays_typed_loud", func(t *testing.T) {
		for _, query := range []string{
			"UPDATE parent SET wanted = 999 WHERE wanted = " +
				"(SELECT c.val FROM child c WHERE c.parent_id = parent.id)",
			"DELETE FROM parent WHERE wanted = " +
				"(SELECT c.val FROM child c WHERE c.parent_id = parent.id)",
			"UPDATE parent SET wanted = 999 WHERE wanted = " +
				"(SELECT c.val FROM child c WHERE c.parent_id = parent.id LIMIT 2)",
			"DELETE FROM parent WHERE wanted = " +
				"(SELECT c.val FROM child c WHERE c.parent_id = parent.id LIMIT 2)",
		} {
			err := expectError(t, db, query)
			requireSQLSTATE(t, err, api.ErrCodeUnsupportedQuery)
		}
		var count int64
		var wantedSum int64
		if err := db.QueryRowContext(ctx,
			"SELECT COUNT(*), SUM(wanted) FROM parent",
		).Scan(&count, &wantedSum); err != nil {
			t.Fatalf("verify DML non-mutation: %v", err)
		}
		if count != 4 || wantedSum != 95 {
			t.Fatalf("DML guard mutated/deleted rows; count/sum = %d/%d, want 4/95", count, wantedSum)
		}
	})
}
