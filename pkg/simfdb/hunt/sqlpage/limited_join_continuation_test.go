package sqlpage

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"fdb.dev/pkg/relational/api"
)

// TestLimitedJoinContinuation checks semantic inner LIMIT/OFFSET windows under
// correlated execution and page resumes. The expected rows are independent of
// the unpaged execution, so an equally wrong paged/unpaged pair cannot pass.
func TestLimitedJoinContinuation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		sql      string
		want     []string
		wantCode api.ErrorCode
	}{
		{
			name: "scalar_second_match",
			sql: "SELECT a.id, (SELECT b.w FROM t2 b WHERE b.ref = a.id ORDER BY b.id LIMIT 1 OFFSET 1) " +
				"FROM t a ORDER BY a.id",
			want: []string{"1|110", "2|210", "3|NULL"},
		},
		{
			name: "exists_second_match",
			sql: "SELECT a.id FROM t a WHERE EXISTS " +
				"(SELECT 1 FROM t2 b WHERE b.ref = a.id LIMIT 1 OFFSET 1) ORDER BY a.id",
			// The correlated EXISTS fallback deliberately rejects a positive
			// data-dependent OFFSET before building the cursor. Pin that gate:
			// this shape cannot exercise FlatMap's limited-inner stop handling.
			wantCode: api.ErrCodeUnsupportedQuery,
		},
		{
			name: "derived_inner_window",
			sql: "SELECT a.id, b.id FROM t a JOIN " +
				"(SELECT id, ref FROM t2 ORDER BY id LIMIT 3 OFFSET 1) b ON b.ref = a.id " +
				"ORDER BY a.id, b.id",
			want: []string{"1|11", "2|20", "2|21"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			h, err := newHarness(250001)
			if err != nil {
				t.Fatal(err)
			}
			defer h.close()
			for _, stmt := range []string{
				"INSERT INTO t VALUES (1,0,10),(2,0,20),(3,0,30)",
				"INSERT INTO t2 VALUES (10,1,100),(11,1,110),(20,2,200),(21,2,210),(22,2,220)",
			} {
				if _, err := h.db.ExecContext(ctx, stmt); err != nil {
					t.Fatalf("seed %s: %v", stmt, err)
				}
			}
			conn, err := h.db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			for _, limit := range []int{noPageLimit, 1, 2, 3, 4, 7} {
				if err := setPageLimit(conn, limit); err != nil {
					t.Fatal(err)
				}
				got, err := runQuery(ctx, conn, tc.sql)
				if tc.wantCode != "" {
					var sqlErr *api.Error
					if !errors.As(err, &sqlErr) || sqlErr.Code != tc.wantCode {
						t.Fatalf("%s at scanned-rows limit %d: got %v, want SQLSTATE %s", tc.sql, limit, err, tc.wantCode)
					}
					t.Logf("LIMIT-CONTINUATION %s budget=%d rejected=%s", tc.name, limit, sqlErr.Code)
					continue
				}
				if err != nil {
					t.Fatalf("%s at scanned-rows limit %d: %v", tc.sql, limit, err)
				}
				if d := compare(tc.want, got, true); d != "" {
					t.Fatalf("%s at scanned-rows limit %d: got %v, want %v (%s)", tc.sql, limit, got, tc.want, d)
				}
				t.Logf("LIMIT-CONTINUATION %s budget=%d rows=%s", tc.name, limit, fmt.Sprint(got))
			}
		})
	}
}
