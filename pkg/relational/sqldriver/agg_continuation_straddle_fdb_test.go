package sqldriver_test

// End-to-end proof for the streaming-aggregate continuation defects F4/F5: a
// GROUP BY whose group STRADDLES a scanned-rows continuation boundary must
// resume from the aggregate's serialized partial state and emit ONE merged row
// per group — never split the straddling group into two.
//
// The setup pins the streaming aggregate directly on an ordered value index (no
// in-memory sort), so a mid-scan break propagates to the aggregate cursor
// (encode/decodeAggregateContinuation), the exact F4/F5 path. With
// EXECUTION_SCANNED_ROWS_LIMIT=3 in paginate mode, the scan stops after 3 rows —
// inside the 4-row first group — and the driver resumes transparently.
//
// F4 trigger: the group-break key is string(tuple.Pack(g)). g=200 packs to
// 0x15 0xC8 and g=1.5 packs to a float tuple; both contain invalid-UTF-8 bytes
// that the prior JSON continuation rewrote to U+FFFD, so the resumed key never
// matched the recomputed key → a FALSE group break split the group.

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

func TestFDB_StreamingAggregate_MidGroupContinuation(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_agg_straddle", "aggstraddle",
		"CREATE TABLE t (id BIGINT, g BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX t_g ON t (g) "+
			"CREATE TABLE td (id BIGINT, g DOUBLE, PRIMARY KEY (id)) "+
			"CREATE INDEX td_g ON td (g)")
	ctx := context.Background()

	const scanLimit = 3
	conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, scanLimit).Build())
		// Paginate mode (no FailOnScanLimitReached): the mid-group scan break
		// rolls forward transparently, resuming the aggregate from its
		// serialized partial state.
	})

	exec := func(q string) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	// requireStreaming asserts the plan is a streaming aggregate over the ordered
	// index with NO in-memory sort — only then does a mid-scan break land in the
	// aggregate cursor. A sort would buffer the whole input and resume via the
	// sort continuation, exercising a different (out-of-scope) code path.
	requireStreaming := func(q string) {
		t.Helper()
		plan := planExplainVia(t, ctx, db, q)
		if !strings.Contains(plan, "StreamingAgg") {
			t.Fatalf("query must plan as a streaming aggregate (exercises the aggregate continuation); got:\n%s", plan)
		}
		if strings.Contains(plan, "Sort") {
			t.Fatalf("query must NOT plan an in-memory sort (that resumes via the sort continuation, not the aggregate's); got:\n%s", plan)
		}
	}
	// drainPairsStr returns the emitted (key,count) rows IN ORDER as
	// "key=count" tokens. scanKey scans the group-key column into a string so the
	// helper works for both a BIGINT and a DOUBLE key.
	drainPairsStr := func(q string, scanKey func(*sql.Rows) (string, int64, error)) string {
		t.Helper()
		rows, err := conn.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			k, c, serr := scanKey(rows)
			if serr != nil {
				t.Fatalf("scan %q: %v", q, serr)
			}
			out = append(out, fmt.Sprintf("%s=%d", k, c))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		return strings.Join(out, " ")
	}

	// ---- F4: BIGINT group key (200 packs to 0x15 0xC8 — 0xC8 is invalid UTF-8) ----
	t.Run("bigint_group_key", func(t *testing.T) {
		const q = "SELECT g, COUNT(*) FROM t GROUP BY g"
		requireStreaming(q)
		// The 200-group has 4 rows; the scan breaks after 3, so it straddles.
		for _, r := range [][2]int64{{1, 200}, {2, 200}, {3, 200}, {4, 200}, {5, 300}} {
			exec(fmt.Sprintf("INSERT INTO t (id, g) VALUES (%d, %d)", r[0], r[1]))
		}
		got := drainPairsStr(q, func(rows *sql.Rows) (string, int64, error) {
			var g, c int64
			err := rows.Scan(&g, &c)
			return strconv.FormatInt(g, 10), c, err
		})
		const want = "200=4 300=1"
		if got != want {
			t.Fatalf("streaming aggregate across a mid-group continuation = %q, want %q "+
				"(buggy F4 splits the straddling group into \"200=3 200=1 300=1\")", got, want)
		}
	})

	// ---- F4 (float key): DOUBLE group key (1.5 packs to a float tuple — invalid UTF-8) ----
	t.Run("double_group_key", func(t *testing.T) {
		const q = "SELECT g, COUNT(*) FROM td GROUP BY g"
		requireStreaming(q)
		for _, r := range []struct {
			id int64
			g  float64
		}{{1, 1.5}, {2, 1.5}, {3, 1.5}, {4, 1.5}, {5, 2.5}} {
			exec(fmt.Sprintf("INSERT INTO td (id, g) VALUES (%d, %g)", r.id, r.g))
		}
		got := drainPairsStr(q, func(rows *sql.Rows) (string, int64, error) {
			var g float64
			var c int64
			err := rows.Scan(&g, &c)
			return strconv.FormatFloat(g, 'g', -1, 64), c, err
		})
		const want = "1.5=4 2.5=1"
		if got != want {
			t.Fatalf("streaming aggregate (double key) across a mid-group continuation = %q, want %q "+
				"(buggy F4 splits the straddling group into \"1.5=3 1.5=1 2.5=1\")", got, want)
		}
	})
}
