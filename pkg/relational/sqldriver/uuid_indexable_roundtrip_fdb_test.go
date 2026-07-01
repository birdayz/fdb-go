package sqldriver_test

// RFC-162 read-side round-trip: a UUID column is a first-class indexable
// primitive (Java's DataType.Primitives.UUID). The write side (sites 1-2:
// isTupleField + scalarToInterface → tuple.UUID entry) landed first; this pins
// the read side end-to-end:
//   - `WHERE v = '<uuid>'` seeks the index (comparand promoted STRING→UUID,
//     packed as a tuple.UUID that hits the 0x30 entry — NOT a 0x02 string miss).
//   - covering `SELECT v` and a plain projection surface the canonical 36-char
//     string (the [16]byte→string conversion at the materialization boundary).
//   - UUID PRIMARY KEY scan, INL join on a UUID key (the case a positional-mask
//     read side would have silently mis-packed), IN-lists, ranges, MIN/MAX-ever.
// A UUID flows through the engine as a neutral [16]byte; string only at the
// driver boundary, tuple.UUID only at the FDB wire boundary (Graefe decision b).

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// Distinct, clearly byte-ordered UUIDs: u1 < u2 < u3 (unsigned big-endian, the
// tuple.UUID wire order). u3 leads with 0x7f so it sorts strictly after the
// 0x55-leading pair.
const (
	uuidV1 = "550e8400-e29b-41d4-a716-446655440000"
	uuidV2 = "550e8400-e29b-41d4-a716-446655440001"
	uuidV3 = "7f000000-0000-0000-0000-0000000000ff"
)

func TestFDB_UUIDIndexableRoundTrip(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_uuidrt")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_uuidrt")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE uuidrt "+
			"CREATE TABLE t (id BIGINT NOT NULL, v UUID, PRIMARY KEY (id)) "+
			"CREATE INDEX t_v ON t (v)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_uuidrt/s WITH TEMPLATE uuidrt")
	dsn := fmt.Sprintf("fdbsql:///testdb_uuidrt?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, fmt.Sprintf(
		"INSERT INTO t (id, v) VALUES (1, '%s'), (2, '%s'), (3, '%s')", uuidV1, uuidV2, uuidV3))

	ids := func(q string, args ...any) []int64 {
		rows, err := db.QueryContext(ctx, q, args...)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, v)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}
	eq := func(g, w []int64) bool {
		if len(g) != len(w) {
			return false
		}
		for i := range g {
			if g[i] != w[i] {
				return false
			}
		}
		return true
	}
	ck := func(name, q string, want []int64) {
		t.Run(name, func(t *testing.T) {
			if got := ids(q); !eq(got, want) {
				t.Errorf("%s = %v, want %v", name, got, want)
			}
		})
	}

	// The equality probe must FIRE the index (not silently degrade to a full
	// scan whose residual filter happens to be correct — that would pass a
	// rows-only check either way and hide a broken SARG).
	t.Run("equality_uses_index", func(t *testing.T) {
		var plan string
		q := fmt.Sprintf("EXPLAIN SELECT id FROM t WHERE v = '%s'", uuidV1)
		if err := db.QueryRowContext(ctx, q).Scan(&plan); err != nil {
			t.Fatalf("EXPLAIN: %v", err)
		}
		if !strings.Contains(plan, "IndexScan(T_V") {
			t.Fatalf("expected IndexScan(T_V ...) for `v = '<uuid>'`, got: %s", plan)
		}
	})

	// Equality, both operand orders, non-equality, IN, and ranges.
	ck("eq", fmt.Sprintf("SELECT id FROM t WHERE v = '%s'", uuidV1), []int64{1})
	ck("eq_other", fmt.Sprintf("SELECT id FROM t WHERE v = '%s'", uuidV2), []int64{2})
	ck("eq_reversed", fmt.Sprintf("SELECT id FROM t WHERE '%s' = v", uuidV3), []int64{3})
	ck("ne", fmt.Sprintf("SELECT id FROM t WHERE v <> '%s'", uuidV1), []int64{2, 3})
	ck("in", fmt.Sprintf("SELECT id FROM t WHERE v IN ('%s', '%s')", uuidV1, uuidV3), []int64{1, 3})
	ck("lt", fmt.Sprintf("SELECT id FROM t WHERE v < '%s'", uuidV3), []int64{1, 2})
	ck("ge", fmt.Sprintf("SELECT id FROM t WHERE v >= '%s'", uuidV2), []int64{2, 3})
	// Parameter-bound comparand (driver arg, not an inline literal).
	t.Run("eq_param", func(t *testing.T) {
		if got := ids("SELECT id FROM t WHERE v = ?", uuidV2); !eq(got, []int64{2}) {
			t.Errorf("eq_param = %v, want [2]", got)
		}
	})
	ck("no_collision", fmt.Sprintf("SELECT id FROM t WHERE v = '%s'", uuidV2), []int64{2})

	// A projected UUID column surfaces the canonical 36-char string, and its
	// value round-trips: filtering by it returns the same row. This pins the
	// [16]byte→string materialization for BOTH the record-fetch path (SELECT id, v)
	// and, below, the covering path.
	t.Run("projection_roundtrips", func(t *testing.T) {
		var gotV string
		if err := db.QueryRowContext(ctx, "SELECT v FROM t WHERE id = 1").Scan(&gotV); err != nil {
			t.Fatalf("SELECT v: %v", err)
		}
		if gotV != uuidV1 {
			t.Fatalf("SELECT v = %q, want %q (canonical string, not raw bytes)", gotV, uuidV1)
		}
	})

	// Covering scan: SELECT v WHERE v = '<uuid>' is served entirely from the
	// index entry (v + PK). It must (a) fire the index and (b) surface the
	// canonical string, not a raw tuple.UUID. This is the site the RFC flags:
	// IndexEntryObjectValue stays a pure ordinal extractor; the conversion is at
	// materialization.
	t.Run("covering_returns_canonical_string", func(t *testing.T) {
		var plan string
		q := fmt.Sprintf("EXPLAIN SELECT v FROM t WHERE v = '%s'", uuidV2)
		if err := db.QueryRowContext(ctx, q).Scan(&plan); err != nil {
			t.Fatalf("EXPLAIN: %v", err)
		}
		if !strings.Contains(plan, "Index") {
			t.Fatalf("expected an index scan for covering `SELECT v WHERE v=`, got: %s", plan)
		}
		var gotV string
		if err := db.QueryRowContext(ctx,
			fmt.Sprintf("SELECT v FROM t WHERE v = '%s'", uuidV2)).Scan(&gotV); err != nil {
			t.Fatalf("covering SELECT v: %v", err)
		}
		if gotV != uuidV2 {
			t.Fatalf("covering SELECT v = %q, want %q", gotV, uuidV2)
		}
	})

	// MIN/MAX over a UUID column is REJECTED — identically to Java. Java's
	// NumericAggregationValue only registers MIN/MAX physical operators for
	// INT/LONG/FLOAT/DOUBLE (no UUID, no STRING), so encapsulation fails the
	// Verify.verifyNotNull with this exact message. Conformance principle:
	// doesn't work in Java → doesn't work in Go, same wording. (This is not a
	// UUID-specific gap — MIN over any non-numeric column is rejected the same
	// way in both engines.)
	t.Run("min_max_rejected_like_java", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT MIN(v), MAX(v) FROM t")
		if err == nil {
			t.Fatal("expected MIN/MAX over a UUID column to be rejected (Java parity), got no error")
		}
		if !strings.Contains(err.Error(), "unable to encapsulate aggregate operation due to type mismatch") {
			t.Fatalf("expected Java's aggregate type-mismatch rejection, got: %v", err)
		}
	})
}

// UUID as the PRIMARY KEY: the PK-scan path (record fetched by a tuple.UUID key)
// and equality-by-PK both round-trip.
func TestFDB_UUIDPrimaryKey(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_uuidpk")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_uuidpk")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE uuidpk "+
			"CREATE TABLE t (k UUID NOT NULL, n BIGINT, PRIMARY KEY (k))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_uuidpk/s WITH TEMPLATE uuidpk")
	dsn := fmt.Sprintf("fdbsql:///testdb_uuidpk?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, fmt.Sprintf(
		"INSERT INTO t (k, n) VALUES ('%s', 10), ('%s', 20)", uuidV1, uuidV3))

	t.Run("pk_equality", func(t *testing.T) {
		var n int64
		if err := db.QueryRowContext(ctx,
			fmt.Sprintf("SELECT n FROM t WHERE k = '%s'", uuidV3)).Scan(&n); err != nil {
			t.Fatalf("SELECT by PK: %v", err)
		}
		if n != 20 {
			t.Errorf("n for k=%s = %d, want 20", uuidV3, n)
		}
	})
	t.Run("pk_projection_roundtrips", func(t *testing.T) {
		var k string
		if err := db.QueryRowContext(ctx, "SELECT k FROM t WHERE n = 10").Scan(&k); err != nil {
			t.Fatalf("SELECT k: %v", err)
		}
		if k != uuidV1 {
			t.Errorf("SELECT k = %q, want %q", k, uuidV1)
		}
	})
}

// INL join on a UUID key: the outer side is index-sourced so its join comparand
// is ALREADY a UUID value (tuple.UUID→[16]byte), NOT a STRING literal. This is
// the exact case Graefe flagged that a positional-mask read side would silently
// mis-pack. With the typed [16]byte flow it just works: the inner index probe
// packs the outer's [16]byte as a tuple.UUID.
func TestFDB_UUIDInlJoin(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_uuidjoin")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_uuidjoin")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE uuidjoin "+
			"CREATE TABLE a (id BIGINT NOT NULL, v UUID, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT NOT NULL, v UUID, label STRING, PRIMARY KEY (id)) "+
			"CREATE INDEX b_v ON b (v)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_uuidjoin/s WITH TEMPLATE uuidjoin")
	dsn := fmt.Sprintf("fdbsql:///testdb_uuidjoin?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, fmt.Sprintf(
		"INSERT INTO a (id, v) VALUES (1, '%s'), (2, '%s')", uuidV1, uuidV2))
	mwjoMustExec(t, db, ctx, fmt.Sprintf(
		"INSERT INTO b (id, v, label) VALUES (10, '%s', 'x'), (20, '%s', 'y'), (30, '%s', 'z')",
		uuidV1, uuidV2, uuidV3))

	t.Run("join_on_uuid_key", func(t *testing.T) {
		rows, err := db.QueryContext(ctx,
			"SELECT a.id, b.label FROM a, b WHERE a.v = b.v")
		if err != nil {
			t.Fatalf("join query: %v", err)
		}
		defer rows.Close()
		got := map[int64]string{}
		for rows.Next() {
			var aid int64
			var label string
			if err := rows.Scan(&aid, &label); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got[aid] = label
		}
		want := map[int64]string{1: "x", 2: "y"}
		if len(got) != len(want) {
			t.Fatalf("join rows = %v, want %v", got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("join a.id=%d label = %q, want %q", k, got[k], v)
			}
		}
	})
}
