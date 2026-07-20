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
			"CREATE INDEX td_g ON td (g) "+
			"CREATE TABLE tb (id BIGINT, g BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX tb_g ON tb (g) "+
			// Composite index (g, v) COVERS the group key and the aggregate
			// operand, so the streaming aggregate sits directly on the ordered
			// index scan (no in-memory sort) and a mid-scan break lands in the
			// AGGREGATE continuation — the SUM/MIN/MAX partial-state path.
			"CREATE TABLE ta (id BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX ta_gv ON ta (g, v)")
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

	// ---- A6: the scan break lands exactly ON a group boundary (BETWEEN groups) ----
	// The first group has EXACTLY scanLimit rows, so the page ends with the
	// group complete in the serialized partial state and the resumed page's
	// FIRST row group-breaks against it: that emit's continuation is the
	// (decoded inner position, NO partial state) form — Java AggregateCursor's
	// "previousValidResult == null" first-row-break arm. The groups must come
	// back exactly once each: a drop here means the restored group was lost, a
	// duplicate means the resume re-aggregated it.
	t.Run("boundary_between_groups", func(t *testing.T) {
		const q = "SELECT g, COUNT(*) FROM tb GROUP BY g"
		requireStreaming(q)
		for _, r := range [][2]int64{{1, 100}, {2, 100}, {3, 100}, {4, 200}, {5, 200}} {
			exec(fmt.Sprintf("INSERT INTO tb (id, g) VALUES (%d, %d)", r[0], r[1]))
		}
		got := drainPairsStr(q, func(rows *sql.Rows) (string, int64, error) {
			var g, c int64
			err := rows.Scan(&g, &c)
			return strconv.FormatInt(g, 10), c, err
		})
		const want = "100=3 200=2"
		if got != want {
			t.Fatalf("streaming aggregate with the scan break BETWEEN groups = %q, want %q", got, want)
		}
	})

	// ---- SUM/MIN/MAX/COUNT partial state straddling a page boundary ----
	// The COUNT-only cases above drive only the `count` field of the partial
	// aggregate state through the continuation. A group straddling the break
	// must ALSO carry sums/sumsI/allInt (SUM) and mins/maxs (MIN/MAX): the g=200
	// group has 4 rows, the scan stops after 3, so its partial SUM/MIN/MAX must
	// resume from the serialized state and merge into one row — not split.
	t.Run("sum_min_max_partial_state", func(t *testing.T) {
		const q = "SELECT g, SUM(v), MIN(v), MAX(v), COUNT(*) FROM ta GROUP BY g"
		requireStreamingAgg := func(query string) {
			t.Helper()
			plan := planExplainVia(t, ctx, db, query)
			if !strings.Contains(plan, "StreamingAgg") || strings.Contains(plan, "Sort") {
				t.Fatalf("query must plan as a streaming aggregate with no in-memory sort "+
					"(so the break lands in the aggregate cursor); got:\n%s", plan)
			}
		}
		for _, r := range [][3]int64{{1, 200, 10}, {2, 200, 20}, {3, 200, 30}, {4, 200, 40}, {5, 300, 50}} {
			exec(fmt.Sprintf("INSERT INTO ta (id, g, v) VALUES (%d, %d, %d)", r[0], r[1], r[2]))
		}
		requireStreamingAgg(q)
		rows, qerr := conn.QueryContext(ctx, q)
		if qerr != nil {
			t.Fatalf("query: %v", qerr)
		}
		defer rows.Close()
		type agg struct{ sum, mn, mx, cnt int64 }
		got := map[int64]agg{}
		for rows.Next() {
			var g, s, mn, mx, c int64
			if serr := rows.Scan(&g, &s, &mn, &mx, &c); serr != nil {
				t.Fatalf("scan: %v", serr)
			}
			got[g] = agg{s, mn, mx, c}
		}
		if rerr := rows.Err(); rerr != nil {
			t.Fatalf("rows.Err: %v", rerr)
		}
		want := map[int64]agg{
			200: {sum: 100, mn: 10, mx: 40, cnt: 4},
			300: {sum: 50, mn: 50, mx: 50, cnt: 1},
		}
		if len(got) != 2 || got[200] != want[200] || got[300] != want[300] {
			t.Fatalf("SUM/MIN/MAX/COUNT across a mid-group continuation = %+v, want %+v "+
				"(a lost partial SUM/MIN/MAX splits or mis-aggregates the straddling g=200 group)", got, want)
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
