package sqldriver_test

// RFC-130: statement-wide memory byte budget — FDB integration tests.
//
// The budget (api.OptMaxStatementMemoryBytes) bounds the in-memory buffering
// operators (sort buffers, NLJ inner / buffered-union materializations,
// recursive-CTE working sets) by BYTES, where MaterializationLimit bounds them
// only by ROW COUNT. Each test proves either (a) a configured budget trips with
// SQLSTATE 54F01 BEFORE the row-count limit, revert-proven by the unset-budget
// control returning every row, or (b) the statement-wide / cross-level
// accumulation semantics (one shared counter). The default (option unset / 0)
// path is INERT — the controls pin that.
//
// Helpers (pinEmbeddedConn, seedItemsOnConn, drainIDs, wantExecLimit,
// setupErrorTestDB) live in resource_limits_fdb_test.go / embedded_fdb_*_test.go.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/embedded"
)

// drainIDPayload runs sqlText scanning (id BIGINT, payload STRING), draining
// every row; returns the row count and the terminal error. Used by the byte-
// budget tests so the WIDE payload column flows through the buffered operator
// (a SELECT-id-only query would let the planner project the payload away before
// the sort, leaving a narrow buffer the budget can't bite).
func drainIDPayload(ctx context.Context, conn *sql.Conn, sqlText string) (int, error) {
	r, err := conn.QueryContext(ctx, sqlText)
	if err != nil {
		return 0, err
	}
	defer r.Close()
	n := 0
	for r.Next() {
		var id int64
		var p string
		if serr := r.Scan(&id, &p); serr != nil {
			return n, serr
		}
		n++
	}
	return n, r.Err()
}

// withMemBudget installs the RFC-130 byte budget on a pinned connection.
func withMemBudget(bytes int64) func(*embedded.EmbeddedConnection) {
	return func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptMaxStatementMemoryBytes, bytes).Build())
	}
}

// TestFDB_RFC130_ByteBudgetTripsBeforeRowLimit (spec §3.1): a sort over N wide
// rows whose buffered BYTES exceed the budget — but N is far under
// MaterializationLimit (100k) — errors 54F01. The control with NO budget
// returns every row (revert-proof: the same query, budget 0, completes).
//
// The sort is forced by ORDER BY on an unindexed column, which the planner can
// only satisfy with an in-memory sort buffer (memorySortCursor) — every row is
// buffered, so the budget bites.
func TestFDB_RFC130_ByteBudgetTripsBeforeRowLimit(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_rfc130_sortbudget", "sortbudget",
		"CREATE TABLE Item (id BIGINT, payload STRING, PRIMARY KEY (id))")
	ctx := context.Background()

	const rows = 200 // << MaterializationLimit (100k): row count can never trip
	wide := strings.Repeat("a", 500)

	// Confirm the ORDER BY plans as an in-memory sort (the buffer under test),
	// not an ordered index scan that would stream without buffering.
	const orderByPayload = "SELECT id, payload FROM Item ORDER BY payload"
	if plan := planExplainVia(t, ctx, db, orderByPayload); !planHasSort(plan) {
		t.Fatalf("ORDER BY on an unindexed column must plan as an in-memory sort (the buffered op), got: %s", plan)
	}

	// 200 rows * ~500 bytes ~= 100KB buffered. Cap well under that.
	const budget = 20_000

	seedConn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {})
	seedItemsOnConn(t, ctx, seedConn, rows, wide)

	// --- budget bites: the sort buffer exceeds 20KB → 54F01 ---
	capConn := pinEmbeddedConn(t, db, withMemBudget(budget))
	_, err := drainIDPayload(ctx, capConn, orderByPayload)
	wantExecLimit(t, err)

	// --- control: no budget → every row comes back (revert-proof) ---
	okConn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {})
	got, oerr := drainIDPayload(ctx, okConn, orderByPayload)
	if oerr != nil {
		t.Fatalf("no-budget sort must complete, got: %v", oerr)
	}
	if got != rows {
		t.Fatalf("no-budget sort returned %d rows, want %d", got, rows)
	}
}

// TestFDB_RFC130_DefaultUnlimitedRegression (spec §3.4): with the option UNSET,
// a large sort buffer that WOULD trip a small budget succeeds exactly as today.
// This pins default-safety — the budget is genuinely opt-in and 0 == unlimited.
// (The previous test's control covers the same axis from the trip side; this is
// the explicit unset-is-inert guard with a buffer big enough that any nonzero
// reasonable budget would have tripped.)
func TestFDB_RFC130_DefaultUnlimitedRegression(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_rfc130_default", "rfc130default",
		"CREATE TABLE Item (id BIGINT, payload STRING, PRIMARY KEY (id))")
	ctx := context.Background()

	const rows = 300
	wide := strings.Repeat("z", 400) // ~120KB total buffered

	conn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {})
	seedItemsOnConn(t, ctx, conn, rows, wide)

	got, err := drainIDPayload(ctx, conn, "SELECT id, payload FROM Item ORDER BY payload")
	if err != nil {
		t.Fatalf("default (no budget) must be unlimited and complete, got: %v", err)
	}
	if got != rows {
		t.Fatalf("default unlimited returned %d rows, want %d", got, rows)
	}
}

// TestFDB_RFC130_StatementWideAcrossTwoBranches (spec §3.2): a query whose plan
// buffers in TWO distinct sites must trip when the SUM exceeds the budget — not
// per-site — proving the ONE shared ExecuteState. A UNION of two ORDER BY
// branches buffers each branch's sort independently; the shared counter
// accumulates across both. The budget is set so NEITHER branch alone exceeds it
// but their SUM does, so a per-site (non-shared) counter would wrongly succeed.
func TestFDB_RFC130_StatementWideAcrossTwoBranches(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_rfc130_twobranch", "twobranch",
		"CREATE TABLE A (id BIGINT, payload STRING, PRIMARY KEY (id)) "+
			"CREATE TABLE B (id BIGINT, payload STRING, PRIMARY KEY (id))")
	ctx := context.Background()

	const perTable = 100
	wide := strings.Repeat("w", 500) // each branch ~50KB

	seedConn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {})
	for i := 0; i < perTable; i++ {
		if _, err := seedConn.ExecContext(ctx, fmt.Sprintf("INSERT INTO A (id, payload) VALUES (%d, '%s')", i, wide)); err != nil {
			t.Fatalf("INSERT A %d: %v", i, err)
		}
		if _, err := seedConn.ExecContext(ctx, fmt.Sprintf("INSERT INTO B (id, payload) VALUES (%d, '%s')", i, wide)); err != nil {
			t.Fatalf("INSERT B %d: %v", i, err)
		}
	}

	// Each branch buffers ~50KB. Budget 70KB: one branch alone (50KB) is under,
	// the two together (100KB) are over → the SHARED counter trips on the 2nd.
	const budget = 70_000
	const twoSorts = "SELECT id, payload FROM A ORDER BY payload " +
		"UNION ALL SELECT id, payload FROM B ORDER BY payload"

	// Sanity: each single branch sort stays under the budget (so the trip below
	// is genuinely the SUM, not one branch on its own).
	singleConn := pinEmbeddedConn(t, db, withMemBudget(budget))
	if _, err := drainIDPayload(ctx, singleConn, "SELECT id, payload FROM A ORDER BY payload"); err != nil {
		t.Fatalf("single branch (%dKB) must stay under the %d budget, got: %v", perTable, budget, err)
	}

	// Subject: the two-branch UNION sums past the budget → 54F01.
	capConn := pinEmbeddedConn(t, db, withMemBudget(budget))
	_, err := drainIDPayload(ctx, capConn, twoSorts)
	wantExecLimit(t, err)

	// Control: no budget → both branches complete (200 rows total).
	okConn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {})
	got, oerr := drainIDPayload(ctx, okConn, twoSorts)
	if oerr != nil {
		t.Fatalf("no-budget two-branch must complete, got: %v", oerr)
	}
	if got != 2*perTable {
		t.Fatalf("no-budget returned %d rows, want %d", got, 2*perTable)
	}
}

// TestFDB_RFC130_RecursiveCTECrossLevel (spec §3.3): a recursive CTE where no
// single level's row count approaches MaterializationLimit, but the accumulated
// cross-level working set (the ping-ponged temp tables charged in TempTable.Add
// plus each level's CollectAllBounded) exceeds the byte budget → 54F01. This is
// exactly the bug the row-count limit misses: a deep recursion of modest levels.
// Control with no budget completes.
func TestFDB_RFC130_RecursiveCTECrossLevel(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_rfc130_rcte", "rfc130rcte",
		"CREATE TABLE Item (id BIGINT, payload STRING, PRIMARY KEY (id))")
	ctx := context.Background()

	// Seed enough rows that a recursive walk accumulates many wide rows across
	// levels. The recursion below counts up id by id, one row per level, but the
	// accumulated allResults/working-set grows by a wide payload each level.
	const rows = 150
	wide := strings.Repeat("r", 600)
	seedConn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {})
	seedItemsOnConn(t, ctx, seedConn, rows, wide)

	// Recursive CTE: start at id 0, step to id+1, carrying the wide payload.
	// Each level holds one row but the accumulated results across ~150 levels
	// total ~90KB — far past a 30KB budget, while no single level is anywhere
	// near MaterializationLimit.
	const rcte = "WITH RECURSIVE walk(id, payload) AS (" +
		"  SELECT id, payload FROM Item WHERE id = 0" +
		"  UNION ALL" +
		"  SELECT Item.id, Item.payload FROM Item, walk WHERE Item.id = walk.id + 1" +
		") SELECT id FROM walk"

	// Confirm it plans as a recursive CTE (so the cross-level buffers are the op
	// under test), not some folded form.
	if plan := planExplainVia(t, ctx, db, rcte); !planHasRecursive(plan) {
		t.Fatalf("query must plan as a recursive CTE, got: %s", plan)
	}

	const budget = 30_000

	capConn := pinEmbeddedConn(t, db, withMemBudget(budget))
	_, err := drainIDs(ctx, capConn, rcte)
	wantExecLimit(t, err)

	// Control: no budget → the full walk returns all 150 rows.
	okConn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {})
	got, oerr := drainIDs(ctx, okConn, rcte)
	if oerr != nil {
		t.Fatalf("no-budget recursive CTE must complete, got: %v", oerr)
	}
	if got != rows {
		t.Fatalf("no-budget recursive CTE returned %d rows, want %d", got, rows)
	}
}

// planHasSort reports whether an EXPLAIN string shows an in-memory sort
// operator (the planner names it "Sort" / "ISORT" depending on shape).
func planHasSort(plan string) bool {
	up := strings.ToUpper(plan)
	return strings.Contains(up, "SORT")
}

// planHasRecursive reports whether an EXPLAIN string shows a recursive-CTE
// operator.
func planHasRecursive(plan string) bool {
	up := strings.ToUpper(plan)
	return strings.Contains(up, "RECURSIVE") || strings.Contains(up, "TEMP")
}
