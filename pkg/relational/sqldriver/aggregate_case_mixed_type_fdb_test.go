package sqldriver_test

// Pins MIN/MAX over an unpromoted mixed-type CASE operand (the F48 follow-up):
// `MIN(CASE WHEN flag = 1 THEN d ELSE 0 END)` yields float64 on THEN rows and
// int64 on ELSE rows (PickValue branches are not PromoteValue-wrapped, unlike
// Java's encapsulation), so the streaming MIN/MAX accumulator sees a MIXED
// int64/float64 operand stream. Pre-fix, aggMinMax assumed type-homogeneous
// operands: the ELSE-first arrival order PANICKED on the acc.(float64) type
// assertion, and the THEN-first order SILENTLY DROPPED the integer value
// (wrong extremum). Both orders are exercised here via primary-key scan order.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_AggregateMinMax_MixedTypeCaseOperand(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_aggmixed")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_aggmixed")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE aggmixed "+
			"CREATE TABLE mixed (id BIGINT NOT NULL, g BIGINT, flag BIGINT, d DOUBLE, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_aggmixed/s WITH TEMPLATE aggmixed")
	dsn := fmt.Sprintf("fdbsql:///testdb_aggmixed?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Group 1: ELSE (int64 0) arrives FIRST, THEN (float64 1.5) second — the
	// pre-fix PANIC order (acc=int64, val=float64 → acc.(float64) blew up).
	// Group 2: THEN (float64 2.5) arrives FIRST, ELSE (int64 0) second — the
	// pre-fix SILENT-DROP order (asInt64(float64) failed, MIN stayed 2.5).
	// The base-record scan is PK-ordered, so id order IS arrival order.
	mwjoMustExec(t, db, ctx,
		"INSERT INTO mixed (id, g, flag, d) VALUES "+
			"(1, 1, 0, 9.9), (2, 1, 1, 1.5), "+
			"(3, 2, 1, 2.5), (4, 2, 0, 9.9)")

	rows, err := db.QueryContext(ctx,
		"SELECT g, MIN(CASE WHEN flag = 1 THEN d ELSE 0 END), MAX(CASE WHEN flag = 1 THEN d ELSE 0 END) "+
			"FROM mixed GROUP BY g ORDER BY g")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	type res struct {
		g        int64
		min, max float64
	}
	var got []res
	for rows.Next() {
		var r res
		if err := rows.Scan(&r.g, &r.min, &r.max); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	want := []res{
		{g: 1, min: 0, max: 1.5}, // pre-fix: PANIC while accumulating id=2
		{g: 2, min: 0, max: 2.5}, // pre-fix: MIN silently stayed 2.5 (int 0 dropped)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d groups, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("group %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
