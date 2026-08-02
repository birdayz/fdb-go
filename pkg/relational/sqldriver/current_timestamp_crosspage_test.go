package sqldriver_test

// CURRENT_TIMESTAMP must stay statement-stable ACROSS PAGE BOUNDARIES.
// Page 1 of a paginated statement is fetched eagerly inside the driver call
// (while the session clock's statement stamp is live), but pages 2+ are
// fetched lazily from rows.Next() AFTER the driver call returned and its
// deferred clock-restore zeroed the session stamp. The instant is therefore
// captured ONCE on the result set at Execute (paginatingRows.statementTime);
// reading the session clock per page would fall back to the wall clock and
// drift the moment a page boundary straddles a second. Pagination is routine
// (the 4s per-page transaction budget is always armed), so this is the
// production shape, not an option-forced corner.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

func TestFDB_CurrentTimestamp_CrossPage_Stable(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_cts_xpage", "cts_xpage",
		"CREATE TABLE Item (id BIGINT, PRIMARY KEY (id))")
	ctx := context.Background()

	const total, batch = 10000, 1000
	for lo := 0; lo < total; lo += batch {
		var sb strings.Builder
		sb.WriteString("INSERT INTO Item VALUES ")
		for i := 0; i < batch; i++ {
			if i > 0 {
				sb.WriteString(", ")
			}
			fmt.Fprintf(&sb, "(%d)", lo+i)
		}
		if _, err := db.ExecContext(ctx, sb.String()); err != nil {
			t.Fatalf("seed INSERT batch at %d: %v", lo, err)
		}
	}

	// Force ~20 pages per statement so page boundaries are guaranteed, and
	// loop executions across wall-clock second boundaries so at least one
	// statement's pages straddle a second. Without the Execute-time capture
	// the straddling statement observes two instants.
	conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, int64(500)).Build())
	})

	deadline := time.Now().Add(4 * time.Second)
	execs := 0
	for execs == 0 || time.Now().Before(deadline) {
		rows, err := conn.QueryContext(ctx, "SELECT id, CURRENT_TIMESTAMP FROM Item")
		if err != nil {
			t.Fatalf("SELECT: %v", err)
		}
		distinct := map[string]bool{}
		n := 0
		for rows.Next() {
			var id int64
			var ts string
			if err := rows.Scan(&id, &ts); err != nil {
				rows.Close()
				t.Fatalf("scan: %v", err)
			}
			distinct[ts] = true
			n++
		}
		rerr := rows.Err()
		rows.Close()
		if rerr != nil {
			t.Fatalf("rows: %v", rerr)
		}
		if n != total {
			t.Fatalf("row count = %d, want %d", n, total)
		}
		if len(distinct) != 1 {
			t.Fatalf("CURRENT_TIMESTAMP drifted within ONE paginated statement after %d execs: %d distinct values %v — lazy pages must reuse the instant captured at Execute, never re-read the (already restored) session clock", execs, len(distinct), keysOf(distinct))
		}
		execs++
	}
	t.Logf("no drift across %d paginated executions", execs)
}
