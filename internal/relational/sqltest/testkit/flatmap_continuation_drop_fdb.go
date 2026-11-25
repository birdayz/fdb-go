package testkit

import (
	"context"
	"database/sql"
	"testing"
)

func ReadIDPairs(t *testing.T, ctx context.Context, db *sql.DB, q string) [][2]int64 {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer func() { _ = rows.Close() }()
	return scanIDPairs(t, rows)
}

func ReadIDPairsConn(t *testing.T, ctx context.Context, conn *sql.Conn, q string) [][2]int64 {
	t.Helper()
	rows, err := conn.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer func() { _ = rows.Close() }()
	return scanIDPairs(t, rows)
}

func scanIDPairs(t *testing.T, rows *sql.Rows) [][2]int64 {
	t.Helper()
	var out [][2]int64
	for rows.Next() {
		var a, b int64
		if err := rows.Scan(&a, &b); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, [2]int64{a, b})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}
