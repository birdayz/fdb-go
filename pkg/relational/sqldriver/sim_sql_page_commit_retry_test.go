package sqldriver

// A paged result set's position must advance with the transaction that produced
// the page, or not at all.
//
// paginatingRows.fetchPage runs one page inside a closure that DB.Run's retry
// loop may re-execute. The closure RESUMES the plan from r.continuation and
// truncates r.buf at its top, so any position written to r before the page's
// transaction succeeds is position a re-execution inherits: it resumes past the
// rows the failed attempt drained, having just discarded that attempt's buffer.
// The rows vanish silently — no error, no duplicate, a short result set.
//
// REACHABILITY, MEASURED — and the ordinary shape reaches it.
//
// A SELECT page is NOT read-only. fetchPage opens the record store per page
// through storeIn, which never calls SetSkipPossiblyRebuild, so every page runs
// StoreBuilder.Open() → checkPossiblyRebuild. That WRITES the store header on
// three arms: maybeUpgradeFormatVersion (a persisted format version below
// formatVersionDefault), checkPossiblyRebuildRecordCounts (the record-count key
// changed), and the metadata-version-moved arm (which rebuilds indexes inline).
// A store last written by an older writer — Java pinned at a lower FormatVersion
// is the everyday case — therefore makes the FIRST page of a plain auto-commit
// SELECT a WRITE-CARRYING transaction. Its commit goes to the resolver, a
// concurrent writer can make it not_committed, and before the fix that cost the
// caller page 1's rows with no error raised. TestPageRetry_StaleFormatHeader…
// below builds exactly that and is red on the unfixed code end to end.
//
// So this was SHIPPED, not latent. Two properties do narrow it, and both are
// pinned below because each is owned by code elsewhere and relaxing either
// widens this further:
//
//	1. DML NEVER PAGINATES: executeDelete/executeInsert/executeUpdate each
//	   materialize the whole target set through CollectAllBounded and return a
//	   list cursor, so a DML statement is always exactly one page and has no
//	   continuation to advance. (Pinned: TestPageRetry_DMLIsSinglePage.)
//	2. An explicit transaction is NOT RETRIED — runInCapturedTx calls the closure
//	   directly when a transaction was captured, so RFC-198's write-carrying
//	   SELECT pages have no retry loop to inherit anything. (Pinned:
//	   TestPageRetry_ExplicitTransactionPagesBypassTheRetryLoop.)
//
// Coverage here is deliberately two-layer. The e2e above proves the defect on
// the real production shape with an ordinary injected commit fault, but it can
// only reach page ONE — the header write happens once, and every later page is
// read-only again. Pages 2..n are covered by a backend wrapper that fails a
// chosen page's transaction retryably after its closure completed; that is the
// same failure the resolver produces, applied where SimFDB's faithful read-only
// commit fast path would otherwise swallow an injected fault.
//
// WHAT LET IT THROUGH. Pagination and commit faults were each well covered, on
// disjoint axes: the sqlpage oracle (pkg/simfdb/hunt/sqlpage) paginates whole
// SQL queries hard but runs with faults OFF, and the sim SQL fault tests inject
// at commit but drive single-page statements. The defect lives only at the
// crossing, which nothing reached. The reason it was first written up as latent
// is worth keeping: "an auto-commit SELECT is read-only" was inferred from the
// query path and never checked against the store-open path, which is where the
// writes are.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	relkeyspace "fdb.dev/pkg/relational/core/keyspace"
	"fdb.dev/pkg/relational/core/metadata"
	"fdb.dev/pkg/simfdb"
)

// pageRetryRows is the seeded row count, several times pageRetryScanLimit: with
// one page there is no continuation to advance and the defect cannot be
// expressed at all.
const pageRetryRows = 12

// pageRetryScanLimit is the per-page scanned-rows budget that forces the pages.
const pageRetryScanLimit = 2

// retryOnceBackend is a SimFDB that fails ONE transaction retryably AFTER its
// closure has run to completion, then lets the retry through.
//
// It models a commit-time not_committed: the closure did all its work, the
// transaction then failed and rolled back, and SimFDB's own Transact loop
// re-executes the closure.
//
// It exists to reach pages 2..n, NOT because the defect needs a harness — the
// stale-format e2e reaches page one with an ordinary injected fault and no
// wrapper. Later pages are read-only again (the header upgrade happens once),
// and SimFDB faithfully models FDB's client-side read-only commit fast path, so
// an injected commit fault on those pages is correctly swallowed. Raising the
// error from inside fn instead reproduces the same observable — the closure
// re-run with the result set's position already moved — on pages an injected
// commit fault cannot reach.
//
// Arming is by transaction ORDINAL so a test can choose which page fails.
// SimDB has no TransactCtx, so overriding Transact covers every route through
// recordlayer's runTransactCtx; the embedded *SimDB supplies the rest of the
// fdb.BackendDatabase surface unchanged.
type retryOnceBackend struct {
	*simfdb.SimDB
	// failAt is the 0-based ordinal of the transaction to fail, or -1 for none.
	failAt atomic.Int64
	// seen counts Transact calls since the last arm/reset — the retry route's
	// own traffic, which is what property 3 below measures.
	seen atomic.Int64
	// fired records whether the injection actually happened, so a test fails
	// loudly rather than passing because nothing was exercised.
	fired atomic.Bool
	// permanent keeps the failure armed across the retry loop's attempts, so
	// the transaction exhausts instead of succeeding on the second try.
	permanent atomic.Bool
	// simInjectAt is the ordinal at which to hand the fault to SIMFDB itself
	// (InjectOnce) rather than raise it from the wrapper, or -1 for none. This
	// is a PLACEMENT mechanism only: the fault that results is the sim's own
	// commit-time not_committed, subject to its read-only commit fast path, so
	// a page that carries no writes still correctly sees nothing.
	simInjectAt atomic.Int64
	// everyOnce makes EVERY transaction fail exactly once, so a multi-page
	// statement re-executes every one of its pages. That is what turns a
	// per-page double-count into an effect big enough to cross a cap.
	everyOnce atomic.Bool
	mu        sync.Mutex
	failedOrd map[int64]bool
}

func newRetryOnceBackend(sim *simfdb.SimDB) *retryOnceBackend {
	b := &retryOnceBackend{SimDB: sim}
	b.failAt.Store(-1)
	b.simInjectAt.Store(-1)
	return b
}

// armSimInjectAt schedules SimFDB's OWN not_committed at the commit of the nth
// transaction from now. Unlike arm/armPermanent this raises nothing itself — it
// only places a real injected commit fault on a chosen transaction, which is
// what lets a test target one specific page without guessing how many
// transactions the connection makes first.
func (b *retryOnceBackend) armSimInjectAt(n int64) {
	b.seen.Store(0)
	b.fired.Store(false)
	b.simInjectAt.Store(n)
}

// arm schedules the failure for the nth transaction started from now.
func (b *retryOnceBackend) arm(n int64) {
	b.seen.Store(0)
	b.fired.Store(false)
	b.failAt.Store(n)
}

// armPermanent makes the nth transaction fail retryably on EVERY attempt, so
// SimFDB's retry loop exhausts and the error reaches fetchPage as a terminal
// failure. That is the state in which a page's partial buffer and its
// unearned exhaustion flag would otherwise survive.
func (b *retryOnceBackend) armPermanent(n int64) {
	b.seen.Store(0)
	b.fired.Store(false)
	b.permanent.Store(true)
	b.failAt.Store(n)
}

func (b *retryOnceBackend) disarm() {
	b.failAt.Store(-1)
	b.permanent.Store(false)
	b.simInjectAt.Store(-1)
	b.everyOnce.Store(false)
}

// armEveryOnce makes every transaction from now on fail retryably exactly once.
func (b *retryOnceBackend) armEveryOnce() {
	b.seen.Store(0)
	b.fired.Store(false)
	b.mu.Lock()
	b.failedOrd = map[int64]bool{}
	b.mu.Unlock()
	b.everyOnce.Store(true)
}

// takeEveryOnce reports whether this ordinal still owes its one failure.
func (b *retryOnceBackend) takeEveryOnce(ordinal int64) bool {
	if !b.everyOnce.Load() {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failedOrd[ordinal] {
		return false
	}
	b.failedOrd[ordinal] = true
	return true
}

func (b *retryOnceBackend) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	ordinal := b.seen.Add(1) - 1
	if target := b.simInjectAt.Load(); target >= 0 && target == ordinal {
		b.simInjectAt.Store(-1)
		b.fired.Store(true)
		b.SimDB.InjectOnce(1020)
	}
	return b.SimDB.Transact(func(tx fdb.WritableTransaction) (any, error) {
		result, err := fn(tx)
		if err != nil {
			return result, err
		}
		if b.takeEveryOnce(ordinal) {
			b.fired.Store(true)
			return nil, fdb.Error{Code: 1020} // not_committed
		}
		if target := b.failAt.Load(); target >= 0 && target == ordinal {
			b.fired.Store(true)
			if !b.permanent.Load() {
				// One shot: clear before returning so the retry succeeds.
				b.failAt.Store(-1)
			}
			return nil, fdb.Error{Code: 1020} // not_committed
		}
		return result, nil
	})
}

// openRetryOnceSchema is openSimSchema over a retryOnceBackend.
func openRetryOnceSchema(t *testing.T, seed uint64, tableDDL string) (*sql.DB, *retryOnceBackend) {
	t.Helper()
	env := dst.NewSim(seed)
	env.Buggify = dst.DisabledBuggifier()
	backend := newRetryOnceBackend(simfdb.New(env))
	simDB := recordlayer.NewFDBDatabaseWithBackend(backend).SetEnv(env)
	simDB.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())
	key := "sim://" + t.Name()
	fdbDBCache.Store(key, simDB)
	t.Cleanup(func() { fdbDBCache.Delete(key) })

	ctx := context.Background()
	setup, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///simdb?cluster_file=%s", key))
	if err != nil {
		t.Fatalf("open setup: %v", err)
	}
	defer setup.Close()
	mustExecSQL(t, setup, ctx, "CREATE DATABASE /simdb")
	mustExecSQL(t, setup, ctx, "CREATE SCHEMA TEMPLATE tmpl "+tableDDL)
	mustExecSQL(t, setup, ctx, "CREATE SCHEMA /simdb/s WITH TEMPLATE tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///simdb?cluster_file=%s&schema=s", key))
	if err != nil {
		t.Fatalf("open query conn: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, backend
}

// seedPageRetryRows fills the table and returns a connection whose per-page scan
// budget forces pagination.
func seedPageRetryRows(t *testing.T, ctx context.Context, db *sql.DB) *sql.Conn {
	t.Helper()
	for i := 0; i < pageRetryRows; i++ {
		mustExecSQL(t, db, ctx, fmt.Sprintf("INSERT INTO t (id, a) VALUES (%d, %d)", i, i))
	}
	return pinSimConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, pageRetryScanLimit).
			Build())
	})
}

// TestPageRetry_ContinuationAdvancesOnlyOnTransactionSuccess is the regression:
// a paged SELECT whose page transaction fails retryably after the page was
// drained must still return every row, exactly once.
//
// The transaction ordinal is swept so the failure lands on each of the query's
// pages in turn — a fixed ordinal would be a guess about how many transactions
// the connection makes before the first page, and the invariant is supposed to
// hold wherever the failure lands.
// Two query shapes, because a stale continuation costs differently in each. A
// plain scan resumes from a key position; DISTINCT resumes a STATEFUL operator
// whose continuation carries accumulated dedup state, which is where a position
// the transaction never earned does the most damage. `a` is seeded so every
// value is distinct, making the expected DISTINCT result the full key range and
// any loss immediately legible.
func TestPageRetry_ContinuationAdvancesOnlyOnTransactionSuccess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	shapes := []struct {
		name  string
		query string
	}{
		{"scan", "SELECT id FROM t"},
		{"distinct", "SELECT DISTINCT a FROM t"},
	}

	for _, shape := range shapes {
		for n := int64(0); n < 8; n++ {
			t.Run(fmt.Sprintf("%s/fail_tx_%d", shape.name, n), func(t *testing.T) {
				t.Parallel()
				db, backend := openRetryOnceSchema(t, uint64(9100+n),
					"CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id))")
				conn := seedPageRetryRows(t, ctx, db)

				backend.arm(n)
				rows, err := conn.QueryContext(ctx, shape.query)
				if err != nil {
					t.Fatalf("%q with a retryable failure on transaction %d: %v",
						shape.query, n, err)
				}
				seen := map[int64]int{}
				for rows.Next() {
					var v int64
					if err := rows.Scan(&v); err != nil {
						t.Fatalf("scan: %v", err)
					}
					seen[v]++
				}
				if err := rows.Err(); err != nil {
					t.Fatalf("rows: %v", err)
				}
				rows.Close()
				backend.disarm()

				if !backend.fired.Load() {
					t.Fatalf("the retryable failure armed for transaction %d never fired: this "+
						"subtest exercised nothing. %q makes fewer transactions than the "+
						"ordinal swept, so the sweep no longer covers its pages", n, shape.query)
				}

				var missing, duplicated []int64
				for i := int64(0); i < pageRetryRows; i++ {
					switch seen[i] {
					case 1:
					case 0:
						missing = append(missing, i)
					default:
						duplicated = append(duplicated, i)
					}
				}
				if len(missing) > 0 || len(duplicated) > 0 {
					t.Fatalf("%q returned %d distinct of %d rows after a retryable failure on "+
						"transaction %d (missing %v, duplicated %v). The page whose transaction "+
						"failed had already advanced paginatingRows.continuation, so the retry "+
						"resumed past it and its buffered rows were dropped",
						shape.query, len(seen), pageRetryRows, n, missing, duplicated)
				}
			})
		}
	}
}

// TestPageRetry_NoFailureIsUnchanged is the control: the same paged query with
// nothing armed. It keeps the test above honest — a harness that broke the
// query outright would otherwise be indistinguishable from one that exposed the
// defect.
func TestPageRetry_NoFailureIsUnchanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, backend := openRetryOnceSchema(t, 9200,
		"CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id))")
	conn := seedPageRetryRows(t, ctx, db)

	rows, err := conn.QueryContext(ctx, "SELECT id FROM t")
	if err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	n := 0
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	rows.Close()
	if n != pageRetryRows {
		t.Fatalf("unfaulted paged SELECT returned %d rows, want %d", n, pageRetryRows)
	}
	if backend.fired.Load() {
		t.Fatalf("nothing was armed, yet the backend reports a fired injection")
	}
}

// staleFormatVersion is one step below formatVersionDefault (14,
// FULL_STORE_LOCK). A header carrying it is what a store last written by an
// older writer looks like, and it is the cheapest way to arm the header-write
// arm of checkPossiblyRebuild: maybeUpgradeFormatVersion rewrites the header
// whenever the persisted version differs from current, with no other migration
// work on this step.
//
// If formatVersionDefault ever moves, this stays one-below by construction only
// if it is updated with it — TestPageRetry_StoreOpenWritesHeaderOnStaleFormat
// fails loudly rather than silently measuring a no-op, because it asserts the
// header actually changed.
const staleFormatVersion = 13

// pageRetrySchemaSubspace returns the subspace the driver stored the schema
// under. The driver canonicalises the schema name to upper case, so an
// out-of-band handle must ask for the same name the driver wrote.
func pageRetrySchemaSubspace(t *testing.T) subspace.Subspace {
	t.Helper()
	ss, err := relkeyspace.New(subspace.Sub()).SchemaSubspace("/simdb", "S")
	if err != nil {
		t.Fatalf("schema subspace: %v", err)
	}
	return ss
}

// setStoreFormatVersion rewrites the persisted store header's format version
// out of band, making the store look like one an older writer left behind.
func setStoreFormatVersion(t *testing.T, ctx context.Context, rdb *recordlayer.FDBDatabase, ss subspace.Subspace, v int32) {
	t.Helper()
	key := ss.Pack(tuple.Tuple{recordlayer.StoreInfoKey})
	if _, err := rdb.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		raw, err := rctx.Transaction().Get(key).Get()
		if err != nil {
			return nil, err
		}
		if len(raw) == 0 {
			return nil, fmt.Errorf("no store header at %x: the schema was not created where "+
				"this test looks for it", []byte(key))
		}
		hdr := &gen.DataStoreInfo{}
		if err := hdr.UnmarshalVT(raw); err != nil {
			return nil, err
		}
		hdr.FormatVersion = &v
		out, err := hdr.MarshalVT()
		if err != nil {
			return nil, err
		}
		rctx.Transaction().Set(key, out)
		return nil, nil
	}); err != nil {
		t.Fatalf("set store format version: %v", err)
	}
}

// readStoreFormatVersion reads the persisted store header's format version.
func readStoreFormatVersion(t *testing.T, ctx context.Context, rdb *recordlayer.FDBDatabase, ss subspace.Subspace) int32 {
	t.Helper()
	key := ss.Pack(tuple.Tuple{recordlayer.StoreInfoKey})
	v, err := rdb.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		raw, err := rctx.Transaction().Get(key).Get()
		if err != nil {
			return nil, err
		}
		hdr := &gen.DataStoreInfo{}
		if err := hdr.UnmarshalVT(raw); err != nil {
			return nil, err
		}
		return hdr.GetFormatVersion(), nil
	})
	if err != nil {
		t.Fatalf("read store format version: %v", err)
	}
	return v.(int32)
}

// pageRetryMetaData rebuilds, outside the SQL layer, the metadata the driver
// created for the one-table schema these tests use. The out-of-band store
// builder needs the same metadata the SQL page opens with, or checkPossiblyRebuild
// would be answering a different question than the one under test.
func pageRetryMetaData(t *testing.T) *recordlayer.RecordMetaData {
	t.Helper()
	b := metadata.NewSchemaTemplateBuilder().SetName("tmpl")
	b.AddTable("T", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(true), 1),
		metadata.NewColumnSpec("A", api.NewLongType(true), 2),
	}, []string{"ID"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build out-of-band metadata: %v", err)
	}
	return tmpl.Underlying()
}

// TestPageRetry_StoreOpenWritesHeaderOnStaleFormat is the fact the whole
// reachability argument rests on, pinned on its own: opening a record store is
// not a read. A transaction that does nothing but Open() a store whose
// persisted format version is below current PERSISTS a header upgrade.
//
// That is what makes a plain auto-commit SELECT's first page write-carrying,
// and therefore what makes its commit able to return not_committed.
//
// SCOPE, measured rather than assumed: this pins the RECORD-LAYER fact only. It
// opens the store through its own builder, so it goes red if
// checkPossiblyRebuild stops persisting the upgrade, or if formatVersionDefault
// moves to or below staleFormatVersion without that constant following it — and
// it does NOT notice if the SQL layer stops reaching this path. Adding
// SetSkipPossiblyRebuild to storeIn leaves this test green (verified). The SQL
// WIRING is guarded instead by the vacuity check inside the e2e below, which
// asserts the header actually moved during the query and fails with "this test
// armed nothing" when it did not. Two facts, two guards; neither covers the
// other.
func TestPageRetry_StoreOpenWritesHeaderOnStaleFormat(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, backend := openRetryOnceSchema(t, 9500,
		"CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id))")
	mustExecSQL(t, db, ctx, "INSERT INTO t (id, a) VALUES (1, 1)")

	rdb := recordlayer.NewFDBDatabaseWithBackend(backend)
	ss := pageRetrySchemaSubspace(t)

	if got := readStoreFormatVersion(t, ctx, rdb, ss); got == staleFormatVersion {
		t.Fatalf("the freshly created store already carries format version %d, which is the "+
			"value this test writes to make it look STALE. formatVersionDefault has moved to "+
			"or below %d and staleFormatVersion must follow it, or this test proves nothing",
			got, staleFormatVersion)
	}
	current := readStoreFormatVersion(t, ctx, rdb, ss)

	setStoreFormatVersion(t, ctx, rdb, ss, staleFormatVersion)
	if got := readStoreFormatVersion(t, ctx, rdb, ss); got != staleFormatVersion {
		t.Fatalf("format version is %d after writing %d: the out-of-band header rewrite did "+
			"not take, so nothing below is armed", got, staleFormatVersion)
	}

	// A transaction that does nothing but open the store, exactly as a SELECT
	// page's storeIn does — same builder settings, no SetSkipPossiblyRebuild.
	if _, err := rdb.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		_, openErr := recordlayer.NewStoreBuilder().
			SetDatabase(rdb).
			SetContext(rctx).
			SetSubspace(ss).
			SetMetaDataProvider(pageRetryMetaData(t)).
			SetStoreStateCache(recordlayer.PassThroughStoreStateCache()).
			Open()
		return nil, openErr
	}); err != nil {
		t.Fatalf("Open()-only transaction: %v", err)
	}

	if got := readStoreFormatVersion(t, ctx, rdb, ss); got != current {
		t.Fatalf("after an Open()-ONLY transaction the persisted format version is %d, want %d "+
			"(it was %d going in). If this is now unchanged at %d, opening a store has become "+
			"read-only and a SELECT page no longer carries writes — which would make the "+
			"stale-header e2e below vacuous rather than passing",
			got, current, staleFormatVersion, staleFormatVersion)
	}
}

// TestPageRetry_StaleFormatHeaderPageSurvivesCommitConflict is the REAL
// end-to-end: the production shape, an ordinary injected commit fault, no
// wrapper.
//
// A store left at an older format version (what a Java writer at a lower
// FormatVersion leaves behind) + a paged SELECT. The first page's storeIn
// upgrades the header, so that page's transaction carries mutations and SimFDB
// does NOT take its read-only commit fast path — an injected not_committed
// reaches it exactly as a concurrent writer's conflict would. The retry must
// re-drain page one, not resume past it.
//
// This is the arm that makes the defect shipped rather than latent: on the
// unfixed code the caller loses page one's rows and the query reports success.
func TestPageRetry_StaleFormatHeaderPageSurvivesCommitConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, backend := openRetryOnceSchema(t, 9600,
		"CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id))")
	conn := seedPageRetryRows(t, ctx, db)

	rdb := recordlayer.NewFDBDatabaseWithBackend(backend)
	ss := pageRetrySchemaSubspace(t)

	// Warm the connection first: the one-shot catalog bootstrap and the metadata
	// load each run their own transaction, and they would otherwise sit between
	// the injection and the page it is aimed at.
	warm, err := conn.QueryContext(ctx, "SELECT id FROM t")
	if err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	for warm.Next() {
	}
	warm.Close()

	// Now make the store look like one an older writer left behind. The next
	// statement's FIRST page must upgrade the header, which is what gives that
	// page's transaction its mutations.
	setStoreFormatVersion(t, ctx, rdb, ss, staleFormatVersion)

	// SimFDB's OWN not_committed, placed at the first transaction of the next
	// statement — the page-one commit. The wrapper only chooses WHERE; the fault
	// is the sim's ordinary commit-time conflict and is still subject to its
	// read-only commit fast path. If page one carried no writes it would be
	// swallowed, and the header assertion below would catch that.
	backend.armSimInjectAt(0)
	defer backend.disarm()

	rows, err := conn.QueryContext(ctx, "SELECT id FROM t")
	if err != nil {
		t.Fatalf("paged SELECT over a stale-format store under an injected not_committed: %v", err)
	}
	if !backend.fired.Load() {
		t.Fatalf("no fault was placed: the statement made no transaction at the armed ordinal")
	}
	seen := map[int64]int{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen[id]++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	rows.Close()

	// The header really was upgraded — i.e. the page transaction really did
	// carry the writes this test depends on. Without this the test could pass
	// by never arming anything.
	if got := readStoreFormatVersion(t, ctx, rdb, ss); got == staleFormatVersion {
		t.Fatalf("the store is still at format version %d after the query: no page upgraded the "+
			"header, so no page carried writes and the injected not_committed was taken by the "+
			"read-only fast path. This test armed nothing", got)
	}

	var missing, duplicated []int64
	for i := int64(0); i < pageRetryRows; i++ {
		switch seen[i] {
		case 1:
		case 0:
			missing = append(missing, i)
		default:
			duplicated = append(duplicated, i)
		}
	}
	if len(missing) > 0 || len(duplicated) > 0 {
		t.Fatalf("paged SELECT over a stale-format store returned %d distinct of %d rows "+
			"(missing %v, duplicated %v) after ONE not_committed at commit, and reported "+
			"SUCCESS. The first page upgraded the store header, so its transaction carried "+
			"writes and its commit conflicted; the page had already advanced "+
			"paginatingRows.continuation, so the retry resumed past it and those rows were "+
			"silently dropped. This is the production shape, not a harness artifact",
			len(seen), pageRetryRows, missing, duplicated)
	}
}

// TestPageRetry_DMLIsSinglePage pins reachability property 2: DML materializes
// its whole target set, so it never paginates and its write-carrying page never
// has a continuation to advance.
//
// The observable is that a DML whose scan exceeds the per-page budget FAILS
// (54F01) rather than resuming on a second page — the signature of a
// materializing executor, not a streaming one. If DML ever gains a streaming,
// resumable executor this goes red, and that is the point: at that moment a
// write-carrying page acquires a continuation and fetchPage's staged-then-
// published position stops being latent protection and starts being the thing
// standing between a commit conflict and silently unmodified records.
func TestPageRetry_DMLIsSinglePage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, _ := openRetryOnceSchema(t, 9300,
		"CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id))")
	conn := seedPageRetryRows(t, ctx, db)

	// The same budget under which the SELECT above paginates cleanly.
	_, err := conn.ExecContext(ctx, "UPDATE t SET a = 7")
	if err == nil {
		t.Fatalf("a multi-row UPDATE under a %d-row per-page scan budget SUCCEEDED. DML used to "+
			"materialize its whole target set (CollectAllBounded) and fail rather than "+
			"paginate; if it now resumes across pages, a write-carrying page transaction has "+
			"a continuation and the paging loop's retry safety is load-bearing on a real "+
			"production shape", pageRetryScanLimit)
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeExecutionLimitReached {
		t.Fatalf("UPDATE over budget failed with %v (%T), want 54F01 (execution limit reached): "+
			"the materialize-or-fail behaviour this property rests on has changed shape",
			err, err)
	}
}

// TestPageRetry_ExplicitTransactionPagesBypassTheRetryLoop pins reachability
// property 3: statements inside an explicit transaction do not go through the
// retrying Transact route at all, so RFC-198's write-carrying SELECT pages have
// no re-execution to inherit a stale position from.
//
// Measured directly — the backend counts Transact calls, and an explicit
// transaction's statements must add none. If a retry loop is ever put in front
// of captured transactions, this goes red, and at that moment the defect's
// window opens on the one path where pages really do carry writes.
func TestPageRetry_ExplicitTransactionPagesBypassTheRetryLoop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, backend := openRetryOnceSchema(t, 9400,
		"CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id))")
	conn := seedPageRetryRows(t, ctx, db)

	drain := func(rows *sql.Rows, err error) int {
		t.Helper()
		if err != nil {
			t.Fatalf("paged SELECT: %v", err)
		}
		n := 0
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan: %v", err)
			}
			n++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		rows.Close()
		return n
	}

	// CALIBRATION. The same paged query in AUTO-COMMIT, which is the mode whose
	// pages do go through the retrying route. Its Transact count is the number
	// the in-transaction run is compared against — without it, a zero below
	// could mean "the counter counts nothing" just as easily as "the pages
	// bypassed the retry loop". (Run first so the one-shot catalog/metadata
	// bootstrap is charged here rather than to the measurement.)
	drain(conn.QueryContext(ctx, "SELECT id FROM t"))
	backend.seen.Store(0)
	if got := drain(conn.QueryContext(ctx, "SELECT id FROM t")); got != pageRetryRows {
		t.Fatalf("auto-commit paged SELECT returned %d rows, want %d", got, pageRetryRows)
	}
	autoCommitTxns := backend.seen.Load()
	// pageRetryRows/pageRetryScanLimit pages, each its own transaction.
	if wantPages := int64(pageRetryRows / pageRetryScanLimit); autoCommitTxns < wantPages {
		t.Fatalf("auto-commit paged SELECT made %d transactions through the retrying route, "+
			"want at least %d (one per page). The counter is not observing the page loop, so "+
			"the in-transaction comparison below would be vacuous", autoCommitTxns, wantPages)
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer tx.Rollback()

	// Make the transaction carry writes, so its SELECT pages run on a
	// transaction whose commit really can fail — the shape that would be
	// dangerous if anything retried it.
	if _, err := tx.ExecContext(ctx, "INSERT INTO t (id, a) VALUES (100, 100)"); err != nil {
		t.Fatalf("in-transaction INSERT: %v", err)
	}

	// Zero the counter AFTER BeginTx and the write, so what follows measures
	// only the paged in-transaction SELECT.
	backend.seen.Store(0)

	if got := drain(tx.QueryContext(ctx, "SELECT id FROM t")); got != pageRetryRows+1 {
		t.Fatalf("in-transaction paged SELECT saw %d rows, want %d (read-your-writes across "+
			"pages)", got, pageRetryRows+1)
	}
	if got := backend.seen.Load(); got != 0 {
		t.Fatalf("a paged SELECT inside an explicit transaction made %d call(s) through the "+
			"retrying Transact route (the same query in auto-commit made %d), want 0. "+
			"Explicit-transaction statements are supposed to run directly on the captured "+
			"transaction (runInCapturedTx); routing them through a retry loop makes their "+
			"write-carrying pages re-executable, which is exactly the shape "+
			"paginatingRows.fetchPage must not be replayed under", got, autoCommitTxns)
	}
}

// rawPagedRows opens a paged SELECT through the DRIVER interface, bypassing
// database/sql, and returns the driver.Rows so a test can call Next again after
// an error. database/sql stops iterating at the first error, which is the only
// reason a page's leftover state is unobservable through it — that is a
// property of the caller, not of the paging loop, so the pin must not depend on
// it.
func rawPagedRows(t *testing.T, ctx context.Context, conn *sql.Conn, query string) driver.Rows {
	t.Helper()
	var rows driver.Rows
	if err := conn.Raw(func(dc any) error {
		q, ok := dc.(driver.QueryerContext)
		if !ok {
			t.Fatalf("driver conn is %T, which does not implement driver.QueryerContext", dc)
		}
		var err error
		rows, err = q.QueryContext(ctx, query, nil)
		return err
	}); err != nil {
		t.Fatalf("raw QueryContext(%q): %v", query, err)
	}
	return rows
}

// TestPageRetry_TerminalPageFailureLeavesNothingBehind pins the other half of
// "a page's outcome belongs to its transaction": when a page's transaction
// fails TERMINALLY, neither the rows it drained nor the exhaustion it computed
// may survive it.
//
// Both directions are one bug with two faces, and nextRow's check order is why
// each is silent rather than loud:
//
//		buffer → r.exhausted → r.fetchErr
//
//	  - Rows left in r.buf (bufPos still 0) are served BEFORE the error is ever
//	    consulted, so the caller is handed rows from a transaction that never
//	    committed, as results.
//	  - An r.exhausted set true by the failed attempt is consulted BEFORE the
//	    error too, so the caller gets a clean io.EOF and the failure disappears.
//
// Reordering those checks would NOT fix either one — buffered rows still would
// not belong to the caller, and an unearned exhaustion flag would still be
// wrong. Dropping both on the error path is the fix, and this is its pin.
func TestPageRetry_TerminalPageFailureLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Calibrate: how many transactions does the paged query take, unfaulted?
	// The last one is the page that reports exhaustion; an earlier one is a page
	// that reports rows. Both arms are derived rather than guessed.
	db, backend := openRetryOnceSchema(t, 9700,
		"CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id))")
	conn := seedPageRetryRows(t, ctx, db)
	drainAll := func() int {
		rows, err := conn.QueryContext(ctx, "SELECT id FROM t")
		if err != nil {
			t.Fatalf("calibration SELECT: %v", err)
		}
		n := 0
		for rows.Next() {
			n++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("calibration rows: %v", err)
		}
		rows.Close()
		return n
	}
	drainAll() // warm the one-shot catalog/metadata bootstrap out of the count
	backend.seen.Store(0)
	if got := drainAll(); got != pageRetryRows {
		t.Fatalf("calibration returned %d rows, want %d", got, pageRetryRows)
	}
	total := backend.seen.Load()
	if total < 4 {
		t.Fatalf("the paged query took only %d transactions; this test needs a first page, at "+
			"least one middle page and a final page to place its two arms", total)
	}

	arms := []struct {
		name string
		// failing transaction ordinal within the query
		at int64
		// what a leftover would look like
		leak string
	}{
		// A middle page: it drains rows, so a surviving buffer is served as data.
		{"mid_stream_buffer", 1, "rows from the failed attempt's buffer"},
		// The final page: it computes exhaustion, so a surviving flag is io.EOF.
		{"final_page_exhaustion", total - 1, "a clean io.EOF that swallowed the error"},
	}

	for _, arm := range arms {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			db, backend := openRetryOnceSchema(t, uint64(9710+arm.at),
				"CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id))")
			conn := seedPageRetryRows(t, ctx, db)
			// Same warm-up as the calibration, so ordinals line up.
			warm, err := conn.QueryContext(ctx, "SELECT id FROM t")
			if err != nil {
				t.Fatalf("warm-up: %v", err)
			}
			for warm.Next() {
			}
			warm.Close()

			backend.armPermanent(arm.at)
			defer backend.disarm()

			rows := rawPagedRows(t, ctx, conn, "SELECT id FROM t")
			defer rows.Close()

			dest := make([]driver.Value, len(rows.Columns()))
			var firstErr error
			for i := 0; i < pageRetryRows+4; i++ {
				if err := rows.Next(dest); err != nil {
					firstErr = err
					break
				}
			}
			if firstErr == nil {
				t.Fatalf("the paged SELECT never failed even though transaction %d was armed to "+
					"fail permanently: this arm exercised nothing", arm.at)
			}
			if !backend.fired.Load() {
				t.Fatalf("transaction %d was never reached; the arm is misplaced", arm.at)
			}
			if errors.Is(firstErr, io.EOF) {
				t.Fatalf("the paged SELECT ended with io.EOF instead of the terminal page " +
					"failure. The failed attempt's exhaustion flag was published before its " +
					"transaction succeeded, and nextRow consults it before r.fetchErr — so the " +
					"error vanished and the caller saw a clean, short result set")
			}

			// THE PIN: after a terminal failure, iterating again must keep
			// reporting the failure. It must never yield %s.
			for i := 0; i < 3; i++ {
				err := rows.Next(dest)
				if err == nil {
					t.Fatalf("driver.Rows.Next returned a ROW after the page's transaction "+
						"failed terminally (%s). Those rows were drained by an attempt that "+
						"never committed; they are not results. Only database/sql's "+
						"stop-at-first-error hides this from an ordinary caller",
						arm.leak)
				}
				if errors.Is(err, io.EOF) {
					t.Fatalf("driver.Rows.Next returned io.EOF after the page's transaction "+
						"failed terminally (%s), turning a failure into a silent short read",
						arm.leak)
				}
			}
		})
	}
}

// recursiveChainLevels is the length of the stored chain the recursive CTE
// walks, and therefore the number of recursion levels one clean execution
// counts. It is chosen so that ONE count sits comfortably below the
// maxStreamingRecursionDepth cap of 1000 and a per-page DOUBLE count sits
// comfortably above it — the whole point being that neither verdict is a near
// miss that could flip on an off-by-one in how levels straddle pages.
const recursiveChainLevels = 600

// recursiveScanLimit is this test's per-page scan budget. It is far larger than
// pageRetryScanLimit because the effect under test does not depend on page SIZE:
// retrying every page double-counts every level regardless of how the levels are
// distributed across pages. A tiny budget only multiplies the number of
// transactions (and the runtime) for no extra signal, so this takes the largest
// budget that still breaks the walk into many pages.
const recursiveScanLimit = 40

// seedRecursiveChain builds a 1→2→…→N parent chain in one statement and
// returns a connection with a small per-page scan budget, so the recursive CTE
// really does break across fetchPage boundaries.
//
// The chain has to be STORED and the recursive leg has to join against it: the
// scanned-rows limiter counts store reads only (key_value_cursor.go), so a pure
// counter CTE — whose recursive leg reads only the temp table — never consumes
// the budget, never paginates, and would resume from a nil continuation every
// time. A nil continuation is exactly the case newRecursiveUnionCursor DOES
// reset, which would make this test measure nothing.
func seedRecursiveChain(t *testing.T, ctx context.Context, db *sql.DB) *sql.Conn {
	t.Helper()
	var vals strings.Builder
	for i := 1; i <= recursiveChainLevels; i++ {
		if i > 1 {
			vals.WriteString(",")
		}
		fmt.Fprintf(&vals, "(%d,%d)", i, i-1)
	}
	mustExecSQL(t, db, ctx, "INSERT INTO edges VALUES "+vals.String())
	return pinSimConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, recursiveScanLimit).
			Build())
	})
}

// recursiveChainQuery walks the stored chain one level at a time. TRAVERSAL
// ORDER level_order pins the STREAMING level-union plan, whose cursor
// (recursiveUnionCursor) is the only caller of checkDepth — the eager and DFS
// arms have their own separate caps and would not exercise this at all.
const recursiveChainQuery = "WITH RECURSIVE r(n) AS (" +
	"SELECT id FROM edges WHERE parent = 0 " +
	"UNION ALL " +
	"SELECT e.id FROM edges AS e, r WHERE e.parent = r.n" +
	") TRAVERSAL ORDER level_order SELECT n FROM r"

// TestPageRetry_RecursionLevelIsPagePositional pins the last member of the
// class this file is about: state a retried page advances that is neither on
// paginatingRows nor staged as a page outcome.
//
// The recursive-CTE level count lives on the statement-wide ExecuteState and is
// bumped deep inside the executor. A resumed cursor deliberately does not reset
// it, so an attempt that walks k levels and then fails leaves those k counted
// and the retry walks them again. Retry every page of a 600-level CTE and the
// count reaches ~1200 against a cap of 1000: the statement fails 54F01 claiming
// a recursion depth it never reached, on a query that is not even close to the
// limit.
//
// This is the same rule as the continuation, applied to position that could not
// be staged: it is rolled back at the top of every attempt instead.
func TestPageRetry_RecursionLevelIsPagePositional(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, backend := openRetryOnceSchema(t, 9800,
		"CREATE TABLE edges (id BIGINT, parent BIGINT, PRIMARY KEY (id))")
	conn := seedRecursiveChain(t, ctx, db)

	// Baseline: the query is well under the cap when nothing is retried, so a
	// depth error below can only have come from double counting.
	clean := countRecursiveRows(t, ctx, conn)
	if clean != recursiveChainLevels {
		t.Fatalf("unfaulted recursive CTE returned %d rows, want %d: the query does not walk the "+
			"chain one level per row, so the level count this test reasons about is not the "+
			"chain length", clean, recursiveChainLevels)
	}

	backend.armEveryOnce()
	defer backend.disarm()

	rows, err := conn.QueryContext(ctx, recursiveChainQuery)
	if err == nil {
		n := 0
		for rows.Next() {
			n++
		}
		err = rows.Err()
		rows.Close()
		if err == nil && n != recursiveChainLevels {
			t.Fatalf("recursive CTE returned %d rows under per-page retries, want %d",
				n, recursiveChainLevels)
		}
	}
	if !backend.fired.Load() {
		t.Fatalf("no page was retried: this test exercised nothing")
	}
	if err != nil {
		if strings.Contains(err.Error(), "recursive CTE exceeded maximum depth") {
			t.Fatalf("a %d-level recursive CTE failed the 1000-level depth cap because its pages "+
				"were retried: %v.\n\nEvery retried page re-counted the levels it had already "+
				"counted before its transaction failed, because the recursion level is page "+
				"POSITION on the statement-wide ExecuteState and a resumed cursor deliberately "+
				"does not reset it. The query never reached that depth; the counter did.",
				recursiveChainLevels, err)
		}
		t.Fatalf("recursive CTE under per-page retries: %v", err)
	}
}

// countRecursiveRows runs the chain query and returns the row count.
func countRecursiveRows(t *testing.T, ctx context.Context, conn *sql.Conn) int {
	t.Helper()
	rows, err := conn.QueryContext(ctx, recursiveChainQuery)
	if err != nil {
		t.Fatalf("recursive CTE: %v", err)
	}
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("recursive CTE rows: %v", err)
	}
	rows.Close()
	return n
}

// Measured sizing for the memory-budget test below. These are not round
// numbers picked for looks — they are the outcome of a threshold sweep, and the
// test is only meaningful inside the window they describe. All figures are the
// budget at which the statement first reports "statement memory budget
// exceeded", for 12 rows of memPayloadBytes each under a 2-row page budget:
//
//	                            paired release working   release dropped
//	one honest pass                  ~49 KiB                 ~132 KiB
//	every page retried once          ~49 KiB                 ~263 KiB
//
// Two things to read off that table. First, with release working the retried
// column equals the honest column — that IS the property under test, and it is
// why nothing needs rolling back. Second, the gap only opens when release is
// dropped, and it opens WIDE: the budget below sits at ~2.7x the honest peak
// (so the baseline has real headroom and is not passing by a whisker) and at
// ~0.5x the leaked retried figure (so the mutation fails decisively, not
// marginally).
//
// The earlier version of this test used 12 single-BIGINT rows against a 64 KiB
// budget. Those rows charge a few hundred bytes in total, so no amount of
// leakage could ever cross the limit: it passed with the release call deleted
// and therefore supported nothing. If these numbers drift, re-run the sweep
// rather than nudging the budget — the margin is the whole point.
const (
	// memPayloadBytes makes each row big enough that a page's worth of buffer
	// is a meaningful fraction of the budget. PAYLOAD SIZE is the lever, not row
	// count: more rows would mean more pages and a longer runtime for the same
	// signal.
	memPayloadBytes = 4096
	// memBudgetBytes sits between the honest peak and the leaked total.
	memBudgetBytes = int64(128 << 10) // 131072
	// memHeadroomBudgetBytes is half of it — the honest peak must fit even here,
	// which is what makes the margin above a measurement rather than a hope.
	memHeadroomBudgetBytes = int64(64 << 10) // 65536
)

// TestPageRetry_MemoryBudgetIsStatementCumulative is the other half of the
// split, and it is a MEASUREMENT rather than a restatement: the memory budget
// is deliberately NOT rolled back per attempt, and this checks that choice is
// actually right instead of merely asserted.
//
// The budget gauges LIVE bytes, with every charge released on teardown and the
// page closure closing its result set on the way out of a failed attempt too.
// So a retried page must return the gauge to where it started on its own. If it
// did not — if charges leaked on the error path — a retried multi-page query
// under a budget would drift upward and eventually trip a limit it never
// legitimately reached, and the correct fix would be to roll this back with the
// recursion level rather than to leave it alone.
//
// Rolling it back is what would be WRONG if this passes: it would double-release
// what teardown already released, and discard charges for buffers that outlive
// the page.
func TestPageRetry_MemoryBudgetIsStatementCumulative(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// An ORDER BY forces a buffering operator, so there are real charges that
	// could leak; a pure streaming scan charges nothing and would prove nothing.
	const q = "SELECT id FROM t ORDER BY a"

	run := func(t *testing.T, seed uint64, budget int64, retryEveryPage bool) error {
		t.Helper()
		db, backend := openRetryOnceSchema(t, seed,
			"CREATE TABLE t (id BIGINT, a BIGINT, payload STRING, PRIMARY KEY (id))")
		pad := strings.Repeat("x", memPayloadBytes)
		for i := 0; i < pageRetryRows; i++ {
			mustExecSQL(t, db, ctx, fmt.Sprintf(
				"INSERT INTO t (id, a, payload) VALUES (%d, %d, '%s')", i, pageRetryRows-i, pad))
		}
		conn := pinSimConn(t, db, func(ec *embedded.EmbeddedConnection) {
			ec.SetOptions(api.NewOptionsBuilder().
				Set(api.OptExecutionScannedRowsLimit, pageRetryScanLimit).
				Set(api.OptMaxStatementMemoryBytes, budget).
				Build())
		})
		if retryEveryPage {
			backend.armEveryOnce()
			defer backend.disarm()
		}
		rows, err := conn.QueryContext(ctx, q)
		if err != nil {
			return err
		}
		n := 0
		for rows.Next() {
			n++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if retryEveryPage && !backend.fired.Load() {
			t.Fatalf("no page was retried: this arm exercised nothing")
		}
		if n != pageRetryRows {
			t.Fatalf("query returned %d rows, want %d", n, pageRetryRows)
		}
		return nil
	}

	// THE PROPERTY: retrying every page must not move the gauge. Charges made by
	// an attempt that failed are released when that attempt tears down, so the
	// retried run costs the same as the honest one and the budget is never
	// approached — which is exactly why memUsed is left out of the per-attempt
	// rollback that the recursion level gets.
	if err := run(t, 9902, memBudgetBytes, true); err != nil {
		t.Fatalf("a buffered query failed under a %d-byte budget when every page was retried: "+
			"%v.\n\nOne honest pass fits in %d bytes, so this is leaked charge, not real "+
			"growth: a failed attempt's charges are surviving its teardown and the gauge "+
			"climbs once per retried page. If that is now true, the memory budget belongs on "+
			"the PAGE-POSITIONAL side of the split with the recursion level — it must be "+
			"snapshotted and rolled back in fetchPage — and ExecuteState's documentation of "+
			"the split is wrong",
			memBudgetBytes, err, memHeadroomBudgetBytes)
	}

	// MARGIN SENTINEL, checked second on purpose. One honest pass must fit in
	// HALF the budget the arm above uses, so that arm is measuring a leak and not
	// a margin. It runs AFTER the property so that a change breaking both reports
	// the property failure — the one that says what actually went wrong — rather
	// than this one, which would only say the numbers need re-deriving.
	if err := run(t, 9901, memHeadroomBudgetBytes, false); err != nil {
		t.Fatalf("an honest pass no longer fits in %d bytes (half the budget the real "+
			"assertion uses): %v.\n\nThe headroom this test depends on is gone, so the arm "+
			"below can no longer distinguish a leaked retry charge from ordinary growth. "+
			"Re-run the threshold sweep and re-derive both constants",
			memHeadroomBudgetBytes, err)
	}
}
