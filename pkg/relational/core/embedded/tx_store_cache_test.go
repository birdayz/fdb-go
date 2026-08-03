package embedded

// RFC-198 Decision 10 (the per-transaction store cache) and its named
// collision, open question 4.
//
// Decision 10: fetchPage used to rebuild the whole record store on EVERY page.
// That is free in auto-commit — each page is its own transaction and the store
// must be rebuilt anyway — and stops being free the moment pages share one
// transaction, because each Open() reads the store header out of the same
// 5-second window the query is competing for. Java caches one store per
// transaction (RecordLayerSchema.java:98-108) and so does this.
//
// OQ-4 asks whether the cache introduces a stale-metadata hazard: runDDL
// auto-commits in its own transaction even inside BeginTx (Decision 8f) and
// invalidates the schema cache when it does, while a cached store still holds
// the metadata that DDL replaced. Both halves are pinned below, and the second
// one MEASURED a narrowing of the RFC's own claim — see the test.

import (
	"context"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
)

// TestStoreCache_PagesDoNotCostStoreOpens measures Decision 10's whole reason
// to exist: a ten-page in-transaction SELECT opens the record store exactly as
// many times as a one-page one — which is to say, the pages are free.
//
// It is a COMPARISON rather than an absolute count because a transaction also
// opens the catalog's own store when it reads the catalog inside itself
// (Decision 8b), and those opens land on the same timer. Both transactions pay
// that identically, so the difference between them is the pagination cost and
// nothing else. Under a per-page rebuild the ten-page run pays ten.
//
// That the multi-page run really paginated is proved from production state,
// not assumed: the transaction-scoped scan state accumulated one scanned
// record per row while the armed per-page scanned-records limit was two, and a
// single page cannot scan more than its limit. Without that the whole test
// would pass vacuously the day pagination stopped happening.
func TestStoreCache_PagesDoNotCostStoreOpens(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newSimBackend(t, 101)
	conn := b.connect("")
	bootstrapTwoTemplates(t, conn)
	conn.SetSchema("s")

	const rows = 20
	var sb strings.Builder
	sb.WriteString("INSERT INTO t (id, v) VALUES ")
	for i := 0; i < rows; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "(%d, %d)", i, i)
	}
	if _, err := conn.ExecContext(ctx, sb.String(), nil); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// scanAllInTx runs "SELECT id FROM t" to exhaustion inside a fresh explicit
	// transaction with the given per-page scanned-records limit (0 = unarmed,
	// one page), and reports the store opens the transaction charged and the
	// records its transaction-scoped scan state saw.
	scanAllInTx := func(t *testing.T, perPage int64) (opens int64, scanned int, rowsOut int) {
		t.Helper()
		ob := api.NewOptionsBuilder()
		if perPage > 0 {
			ob.Set(api.OptExecutionScannedRowsLimit, perPage)
		}
		conn.SetOptions(ob.Build())
		if _, err := conn.Begin(); err != nil {
			t.Fatalf("begin: %v", err)
		}
		tx := conn.activeTx
		defer tx.Rollback() //nolint:errcheck
		timer := recordlayer.NewStoreTimer()
		tx.rctx.SetTimer(timer)
		drained := drainInt64s(t, conn, "SELECT id FROM t")
		return timer.GetCount(recordlayer.EventOpenStore), tx.scanState.RecordsScanned(), len(drained)
	}

	const perPage = 2
	multiOpens, multiScanned, multiRows := scanAllInTx(t, perPage)
	singleOpens, _, singleRows := scanAllInTx(t, 0)

	if multiRows != rows || singleRows != rows {
		t.Fatalf("in-tx SELECT returned %d rows paginated and %d rows unpaginated, want %d each",
			multiRows, singleRows, rows)
	}
	if pages := multiScanned / perPage; pages < 2 {
		t.Fatalf("the paginated run scanned %d records at %d per page — that is %d page(s), "+
			"so this test is no longer comparing a multi-page transaction against a "+
			"single-page one and its open-count equality proves nothing",
			multiScanned, perPage, pages)
	}
	if multiOpens != singleOpens {
		t.Fatalf("a %d-page in-transaction SELECT charged %d store opens against a single-page "+
			"one's %d: the store is being rebuilt per page, and every rebuild reads the store "+
			"header out of the transaction's 5-second window (RFC-198 Decision 10)",
			multiScanned/perPage, multiOpens, singleOpens)
	}

	// The cache is TRANSACTION-scoped, not statement-scoped: a second statement
	// in the same transaction reuses the store the first one opened.
	conn.SetOptions(api.NewOptionsBuilder().Build())
	if _, err := conn.Begin(); err != nil {
		t.Fatalf("begin: %v", err)
	}
	tx := conn.activeTx
	defer tx.Rollback() //nolint:errcheck
	timer := recordlayer.NewStoreTimer()
	tx.rctx.SetTimer(timer)
	if got := queryOneInt64(t, conn, "SELECT v FROM t WHERE id = 1"); got != 1 {
		t.Fatalf("statement 1 v = %d, want 1", got)
	}
	afterFirst := timer.GetCount(recordlayer.EventOpenStore)
	if got := queryOneInt64(t, conn, "SELECT v FROM t WHERE id = 3"); got != 3 {
		t.Fatalf("statement 2 v = %d, want 3", got)
	}
	if after := timer.GetCount(recordlayer.EventOpenStore); after != afterFirst {
		t.Fatalf("a second statement in the same transaction charged %d store opens on top of "+
			"the first statement's %d, want none: the cache is statement-scoped, not "+
			"transaction-scoped (RFC-198 Decision 10)", after-afterFirst, afterFirst)
	}
}

// TestStoreCache_OwnDDLDropsTheStoreAndTheSnapshotHolds pins both halves of
// RFC-198's open question 4.
//
// The half that is REAL and asserted structurally: a DDL issued inside an
// explicit transaction auto-commits in its own transaction (Decision 8f) and
// invalidates the schema cache — and must drop the transaction's cached STORE
// with it. The store was built with SetMetaDataProvider(cachedMetaData()) and
// holds that metadata for its lifetime, so leaving it behind couples the
// transaction's execution to a cache entry that no longer exists.
//
// The half the RFC states and this test MEASURES DIFFERENTLY: OQ-4 predicts
// the leftover store would leave the transaction "planning against the new
// schema and executing against the old one". It would not — the skew is not
// reachable through this path, because Decision 8b's re-read is pinned to the
// TRANSACTION'S SNAPSHOT, which predates the DDL the transaction itself just
// committed. Measured below: after DROP SCHEMA + CREATE SCHEMA WITH an indexed
// template, the transaction's next statement still plans against the pre-DDL
// schema (no idx_v) and still reads the pre-DDL row. Plan and store therefore
// agree on the old metadata; the invalidation is defense in depth, not a
// repair.
//
// That negative result is what this test exists to pin. If a future change
// lets an explicit transaction observe its own mid-transaction DDL — a fresh
// read version per statement, or a catalog read that escapes the transaction —
// the plan/store skew OQ-4 describes becomes reachable for real, and the
// assertions below are what fire.
func TestStoreCache_OwnDDLDropsTheStoreAndTheSnapshotHolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newSimBackend(t, 103)
	conn := b.connect("")
	bootstrapTwoTemplates(t, conn)
	conn.SetSchema("s")
	if _, err := conn.ExecContext(ctx, "INSERT INTO t (id, v) VALUES (1, 1)", nil); err != nil {
		t.Fatalf("seed insert: %v", err)
	}

	if _, err := conn.Begin(); err != nil {
		t.Fatalf("begin: %v", err)
	}
	tx := conn.activeTx
	defer tx.Rollback() //nolint:errcheck

	// Statement 1 caches the PRE-DDL store (built from tmpl_plain, no index)
	// and pins the transaction's read version.
	if got := queryOneInt64(t, conn, "SELECT v FROM t WHERE id = 1"); got != 1 {
		t.Fatalf("stmt1 v = %d, want 1", got)
	}
	if len(tx.stores) != 1 {
		t.Fatalf("after one in-tx statement the transaction holds %d cached stores, want 1: "+
			"the rest of this test asserts what happens to a cached store and cannot do "+
			"that if nothing was cached", len(tx.stores))
	}

	// The DDL, on THIS connection, INSIDE the transaction. runDDL auto-commits
	// it in its own transaction; invalidateSchemaCache is what must reach the
	// store the transaction is still holding.
	for _, stmt := range []string{
		"DROP SCHEMA /simdb/s",
		"CREATE SCHEMA /simdb/s WITH TEMPLATE tmpl_indexed",
	} {
		if _, err := conn.ExecContext(ctx, stmt, nil); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if len(tx.stores) != 0 {
		t.Fatalf("the transaction still holds %d cached store(s) after its own DDL invalidated "+
			"the schema cache: the store outlives the metadata it was built from, and the "+
			"one-lifecycle rule every transaction-scoped object here follows is broken "+
			"(RFC-198 Decision 10 / open question 4)", len(tx.stores))
	}

	// The snapshot holds: the transaction cannot see the DDL it just committed,
	// so its next statement plans from the pre-DDL schema and reads the pre-DDL
	// row. Plan and store agree — which is why the dropped store is rebuilt
	// identically and no skew is observable here.
	explain, err := conn.PlanExplain(ctx, "SELECT id FROM t WHERE v = 1")
	if err != nil {
		t.Fatalf("post-DDL explain: %v", err)
	}
	if strings.Contains(strings.ToUpper(explain), "IDX_V") {
		t.Fatalf("after its own mid-transaction DDL the transaction planned against the "+
			"POST-DDL schema (plan contains idx_v) while its data still sits at the "+
			"pre-DDL snapshot: the plan/store skew OQ-4 describes is now REACHABLE and "+
			"the store invalidation above stopped being merely defensive. Plan:\n%s", explain)
	}
	if got := queryOneInt64(t, conn, "SELECT id FROM t WHERE v = 1"); got != 1 {
		t.Fatalf("post-DDL in-tx read returned %d, want 1: the transaction stopped reading at "+
			"its own snapshot after committing a DDL that dropped and recreated the schema "+
			"(RFC-198 Decision 8b pins the catalog to the transaction's read version)", got)
	}
}

// drainInt64s runs a single-column query on the driver connection and returns
// every row, driving the page loop to exhaustion.
func drainInt64s(t *testing.T, c *EmbeddedConnection, sqlText string) []int64 {
	t.Helper()
	rows, err := c.QueryContext(context.Background(), sqlText, nil)
	if err != nil {
		t.Fatalf("%s: %v", sqlText, err)
	}
	defer rows.Close() //nolint:errcheck
	var out []int64
	dest := make([]driver.Value, 1)
	for {
		err := rows.Next(dest)
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("%s next: %v", sqlText, err)
		}
		v, ok := dest[0].(int64)
		if !ok {
			t.Fatalf("%s returned %T, want int64", sqlText, dest[0])
		}
		out = append(out, v)
	}
}
