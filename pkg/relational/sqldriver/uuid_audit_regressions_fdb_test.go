package sqldriver_test

// Regressions found by the RFC-162 UUID representation audit — surfaces the
// [16]byte value flow reached that the initial change missed. Each pins a
// distinct confirmed defect:
//   - multi-aggregate intersection comparison key over a UUID GROUP BY (P1):
//     the intersection compkey builder left a bare [16]byte, panicking tuple.Pack.
//   - paginated ORDER BY / GROUP BY over a UUID column (P2): the in-memory
//     sort / streaming-aggregate continuation JSON-corrupted a buffered UUID.
//   - UPDATE SET uuid / INSERT…SELECT uuid (P2): goToProtoValue had no UUID arm.
//   - string functions over a UUID column (P3): %v rendered a Go array literal.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

// P1: an intersection whose comparison key is a UUID GROUP BY column must pack
// the key as tuple.UUID, not a bare [16]byte (which panics tuple.Pack inside
// compareKeys and fails the whole query). Two aggregate indexes on the same
// UUID group key force a RecordQueryMultiIntersectionOnValuesPlan.
func TestFDB_UUIDMultiAggregateIntersection(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_uuidmiagg")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_uuidmiagg")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE uuidmiagg "+
			"CREATE TABLE t (id BIGINT NOT NULL, g UUID, price BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX cnt_by_g AS SELECT COUNT(*) FROM t GROUP BY g "+
			"CREATE INDEX sum_by_g AS SELECT SUM(price) FROM t GROUP BY g")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_uuidmiagg/s WITH TEMPLATE uuidmiagg")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_uuidmiagg?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, fmt.Sprintf(
		"INSERT INTO t (id, g, price) VALUES (1,'%s',10),(2,'%s',20),(3,'%s',5),(4,'%s',7)",
		uuidV1, uuidV1, uuidV2, uuidV3))

	var g string
	var cnt, sum int64
	if err := db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT g, COUNT(*), SUM(price) FROM t WHERE g = '%s' GROUP BY g", uuidV1)).
		Scan(&g, &cnt, &sum); err != nil {
		t.Fatalf("multi-aggregate intersection over UUID group key: %v", err)
	}
	if g != uuidV1 || cnt != 2 || sum != 30 {
		t.Fatalf("got (g=%q, cnt=%d, sum=%d), want (%s, 2, 30)", g, cnt, sum, uuidV1)
	}
}

// P2: an UPDATE SET on a UUID column and an INSERT…SELECT of a UUID column both
// flow the value through executor.goToProtoValue (not the VALUES-path
// ConvertToProtoValue), which had no UUID arm and rejected the write with 22000.
func TestFDB_UUIDWritePathUpdateAndInsertSelect(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_uuidwrite")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_uuidwrite")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE uuidwrite "+
			"CREATE TABLE t (id BIGINT NOT NULL, v UUID, PRIMARY KEY (id)) "+
			"CREATE TABLE t2 (id BIGINT NOT NULL, v UUID, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_uuidwrite/s WITH TEMPLATE uuidwrite")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_uuidwrite?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO t (id, v) VALUES (1, '%s')", uuidV1))

	// UPDATE SET v = '<string literal>'.
	t.Run("update_set_string", func(t *testing.T) {
		mwjoMustExec(t, db, ctx, fmt.Sprintf("UPDATE t SET v = '%s' WHERE id = 1", uuidV2))
		var got string
		if err := db.QueryRowContext(ctx, "SELECT v FROM t WHERE id = 1").Scan(&got); err != nil {
			t.Fatalf("SELECT after UPDATE: %v", err)
		}
		if got != uuidV2 {
			t.Fatalf("after UPDATE v = %q, want %q", got, uuidV2)
		}
	})

	// INSERT … SELECT of a UUID column (the read side flows v as [16]byte into
	// goToProtoValue). Implicit column form — the explicit `(id, v)` list hits a
	// separate pre-existing 0AF00 "column ordering for insert with select"
	// limitation orthogonal to UUID.
	t.Run("insert_select", func(t *testing.T) {
		mwjoMustExec(t, db, ctx, "INSERT INTO t2 SELECT id, v FROM t WHERE id = 1")
		var got string
		if err := db.QueryRowContext(ctx, "SELECT v FROM t2 WHERE id = 1").Scan(&got); err != nil {
			t.Fatalf("SELECT from t2: %v", err)
		}
		if got != uuidV2 {
			t.Fatalf("INSERT…SELECT v = %q, want %q", got, uuidV2)
		}
	})
}

// P3: string functions over a UUID column render the canonical 36-char string,
// not a Go array literal ("[85 14 …]").
func TestFDB_UUIDScalarFunctionRender(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_uuidscalar")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_uuidscalar")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE uuidscalar CREATE TABLE t (id BIGINT NOT NULL, v UUID, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_uuidscalar/s WITH TEMPLATE uuidscalar")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_uuidscalar?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO t (id, v) VALUES (1, '%s')", uuidV1))

	var got string
	if err := db.QueryRowContext(ctx, "SELECT CONCAT(v, '-x') FROM t WHERE id = 1").Scan(&got); err != nil {
		t.Fatalf("SELECT CONCAT(v,'-x'): %v", err)
	}
	if want := uuidV1 + "-x"; got != want {
		t.Fatalf("CONCAT(v,'-x') = %q, want %q", got, want)
	}
}

// P2: a paginated ORDER BY / GROUP BY over a UUID column round-trips the buffered
// UUID key across the in-memory-sort / streaming-aggregate continuation boundary
// (JSON-corrupted a bare [16]byte before the fix). Forced via a tiny scanned-rows
// budget so the sort/aggregate buffer straddles a page.
func TestFDB_UUIDPaginatedSortAndGroupBy(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_uuidpage")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_uuidpage")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE uuidpage CREATE TABLE t (id BIGINT NOT NULL, v UUID, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_uuidpage/s WITH TEMPLATE uuidpage")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_uuidpage?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// Insert 7 rows, UUIDs out of byte order, with an UNEVEN largest group
	// (uuidV1 × 3) so a paginated GROUP BY over a UUID column exercises an
	// uneven group under a tiny per-page budget (the group value round-trips as
	// [16]byte via the continuation's UUID tag, materialized to the canonical
	// string). The aggregate group KEY deliberately stays a JSON-safe %T:%v
	// string, not raw tuple.UUID bytes (see computeGroupKey).
	mwjoMustExec(t, db, ctx, fmt.Sprintf(
		"INSERT INTO t (id, v) VALUES (1,'%s'),(2,'%s'),(3,'%s'),(4,'%s'),(5,'%s'),(6,'%s'),(7,'%s')",
		uuidV3, uuidV1, uuidV2, uuidV1, uuidV3, uuidV2, uuidV1))

	// Pin the connection with a tiny per-page scanned-rows budget so the
	// in-memory sort / streaming aggregate pages mid-buffer and must serialize
	// a partial buffer holding UUID keys.
	conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, 2).
			Build())
	})

	readStrings := func(q string) []string {
		rows, err := conn.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, s)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		return out
	}

	t.Run("paginated_order_by", func(t *testing.T) {
		got := readStrings("SELECT v FROM t ORDER BY v")
		want := []string{uuidV1, uuidV1, uuidV1, uuidV2, uuidV2, uuidV3, uuidV3}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("paginated ORDER BY v = %v, want %v (canonical strings, byte order)", got, want)
		}
	})

	t.Run("paginated_group_by", func(t *testing.T) {
		type gc struct {
			v string
			c int64
		}
		rows, err := conn.QueryContext(ctx, "SELECT v, COUNT(*) FROM t GROUP BY v")
		if err != nil {
			t.Fatalf("GROUP BY: %v", err)
		}
		defer rows.Close()
		var got []gc
		for rows.Next() {
			var g gc
			if err := rows.Scan(&g.v, &g.c); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, g)
		}
		sort.Slice(got, func(i, j int) bool { return got[i].v < got[j].v })
		want := []gc{{uuidV1, 3}, {uuidV2, 2}, {uuidV3, 2}}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("paginated GROUP BY = %v, want %v (uuidV1 group of 3 must NOT split across the page)", got, want)
		}
	})
}
