package sqldriver_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	relkeyspace "fdb.dev/pkg/relational/core/keyspace"
	"fdb.dev/pkg/relational/core/metadata"
)

// dependentOnUEmail is answered from U_EMAIL, so the plan DEPENDS on that index.
// Tests that transition U_EMAIL mid-statement must use it: under a scoped
// dependency check a primary-key scan does not depend on U_EMAIL at all, and
// asserting 40001 from one would be asserting the bug this scoping removed.
const dependentOnUEmail = "SELECT EMAIL FROM T WHERE EMAIL > 'a'"

const indexStatePlanningDDL = "CREATE TABLE T (" +
	"ID BIGINT, EMAIL STRING, PAD BIGINT, PRIMARY KEY (ID)) " +
	"CREATE UNIQUE INDEX U_EMAIL ON T (EMAIL) " +
	"CREATE INDEX IDX_PAD ON T (PAD)"

type indexStatePlanningFixture struct {
	db  *sql.DB
	rdb *recordlayer.FDBDatabase
	md  *recordlayer.RecordMetaData
	ss  subspace.Subspace
}

// newIndexStatePlanningFixture builds a schema with one secondary UNIQUE index
// and a second, byte-identical metadata handle reached OUTSIDE the SQL layer.
// The out-of-band handle is what lets a test drive an index through a state
// transition mid-statement, which is not expressible in SQL.
func newIndexStatePlanningFixture(t *testing.T) *indexStatePlanningFixture {
	t.Helper()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	dbPath := "/idxstate_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	schemaName := "idxstate_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	db := setupErrorTestDB(t, dbPath, schemaName, indexStatePlanningDDL)

	b := metadata.NewSchemaTemplateBuilder().SetName(schemaName + "_tmpl")
	b.AddTable("T", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(true), 1),
		metadata.NewColumnSpec("EMAIL", api.NewStringType(true), 2),
		metadata.NewColumnSpec("PAD", api.NewLongType(true), 3),
	}, []string{"ID"})
	b.AddIndex("T", "U_EMAIL", []string{"EMAIL"}, true)
	b.AddIndex("T", "IDX_PAD", []string{"PAD"}, false)
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build matching metadata: %v", err)
	}

	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatalf("open raw FDB: %v", err)
	}
	t.Cleanup(rawDB.Close)
	rdb := recordlayer.NewFDBDatabase(rawDB)
	// The driver stores a schema under the CANONICAL (upper-cased) name, so the
	// out-of-band handle must ask for the same subspace the driver wrote.
	ks := relkeyspace.New(subspace.Sub())
	ss, err := ks.SchemaSubspace(dbPath, strings.ToUpper(schemaName))
	if err != nil {
		t.Fatalf("schema subspace: %v", err)
	}

	ctx := context.Background()
	for id, email := range []string{"a@example", "b@example", "c@example"} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO T (ID, EMAIL, PAD) VALUES (%d, '%s', %d)", id+1, email, (id+1)*10)); err != nil {
			t.Fatalf("seed row %d: %v", id+1, err)
		}
	}
	return &indexStatePlanningFixture{db: db, rdb: rdb, md: tmpl.Underlying(), ss: ss}
}

// makeUniqueIndexPending drives U_EMAIL from READABLE to
// READABLE_UNIQUE_PENDING: the state that is SCANNABLE but whose declared
// uniqueness the data contradicts (IndexState.java:59-66). It is the sharpest
// transition available, because a check written against scannability rather
// than Java's isReadable would not notice it.
func (f *indexStatePlanningFixture) makeUniqueIndexPending(ctx context.Context) error {
	_, err := f.rdb.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		store, openErr := recordlayer.NewStoreBuilder().
			SetContext(rctx).
			SetMetaDataProvider(f.md).
			SetSubspace(f.ss).
			SetStoreStateCache(recordlayer.PassThroughStoreStateCache()).
			Open()
		if openErr != nil {
			return nil, openErr
		}
		if _, stateErr := store.MarkIndexWriteOnly("U_EMAIL"); stateErr != nil {
			return nil, stateErr
		}
		idx := f.md.GetIndex("U_EMAIL")
		if idx == nil {
			return nil, errors.New("U_EMAIL missing from matching metadata")
		}
		if violationErr := store.AddUniquenessViolation(
			idx, tuple.Tuple{"phantom@example"}, tuple.Tuple{int64(999)},
		); violationErr != nil {
			return nil, violationErr
		}
		if _, rangeErr := recordlayer.NewIndexingRangeSet(f.ss, idx).
			InsertRange(rctx.Transaction(), nil, nil, false); rangeErr != nil {
			return nil, rangeErr
		}
		if _, stateErr := store.MarkIndexReadableOrUniquePending("U_EMAIL"); stateErr != nil {
			return nil, stateErr
		}
		if got := store.GetIndexState("U_EMAIL"); got != recordlayer.IndexStateReadableUniquePending {
			return nil, fmt.Errorf("U_EMAIL state = %s, want READABLE_UNIQUE_PENDING", got)
		}
		return nil, nil
	})
	return err
}

// transitionPlanLogger runs fn exactly once, at the moment planning completes,
// which is the only hook that lands strictly between "the plan was built" and
// "the first page opens its transaction".
type transitionPlanLogger struct {
	once sync.Once
	fn   func() error

	mu       sync.Mutex
	transErr error
}

func (l *transitionPlanLogger) LogPlanGeneration(_ context.Context, _ embedded.PlanGenerationInfo) {
	l.once.Do(func() {
		err := l.fn()
		l.mu.Lock()
		l.transErr = err
		l.mu.Unlock()
	})
}

func (l *transitionPlanLogger) err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.transErr
}

func queryIndexStateStrings(t *testing.T, ctx context.Context, conn *sql.Conn, query string) []string {
	t.Helper()
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	sort.Strings(got)
	return got
}

func assertSerializationFailure(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected stale-plan serialization failure, got nil")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("stale-plan error = %T %v, want *api.Error", err, err)
	}
	if apiErr.Code != api.ErrCodeSerializationFailure {
		t.Fatalf("stale-plan SQLSTATE = %s, want %s (full: %v)",
			apiErr.Code, api.ErrCodeSerializationFailure, err)
	}
}

// An auto-commit statement's pages are SEPARATE transactions, so an index-state
// transition landing between two of them is invisible to anything checked once
// per statement — a plan-cache key included. The second page must refuse rather
// than continue under a plan built for a store state that no longer exists.
func TestFDB_IndexStatePlanning_TransitionBetweenPagesFails40001(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIndexStatePlanningFixture(t)
	conn := pinEmbeddedConn(t, f.db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, 1).Build())
	})

	rows, err := conn.QueryContext(ctx, dependentOnUEmail)
	if err != nil {
		t.Fatalf("query first page: %v", err)
	}
	defer func() { _ = rows.Close() }()
	if err := f.makeUniqueIndexPending(ctx); err != nil {
		t.Fatalf("transition after first page: %v", err)
	}
	if !rows.Next() {
		t.Fatalf("first buffered row missing: %v", rows.Err())
	}
	var email string
	if err := rows.Scan(&email); err != nil {
		t.Fatalf("scan first buffered row: %v", err)
	}
	if rows.Next() {
		t.Fatal("second page returned a row from a stale plan")
	}
	assertSerializationFailure(t, rows.Err())
}

// A statement planned BEFORE the transition and first executed after it must
// also refuse: planning and the first page are separate transactions too.
func TestFDB_IndexStatePlanning_TransitionBetweenPlanAndFirstPageFails40001(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIndexStatePlanningFixture(t)
	logger := &transitionPlanLogger{fn: func() error { return f.makeUniqueIndexPending(ctx) }}
	conn := pinEmbeddedConn(t, f.db, func(ec *embedded.EmbeddedConnection) {
		ec.SetPlanLogger(logger)
	})

	rows, err := conn.QueryContext(ctx, dependentOnUEmail)
	if rows != nil {
		_ = rows.Close()
	}
	if transitionErr := logger.err(); transitionErr != nil {
		t.Fatalf("logger state transition: %v", transitionErr)
	}
	assertSerializationFailure(t, err)
}

// A statement planned and executed with NO transition must not be disturbed.
// Without this the test above passes just as well if every query fails.
func TestFDB_IndexStatePlanning_StableStateExecutesNormally(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIndexStatePlanningFixture(t)
	conn := pinEmbeddedConn(t, f.db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, 1).Build())
	})
	rows, err := conn.QueryContext(ctx, "SELECT ID FROM T")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var got []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("paged scan under a stable index state failed: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("rows = %v, want 3 ids across 3 pages", got)
	}
}

// distinctEmailRun plans and runs one query on the logger-equipped conn and
// returns the plan text the planner actually chose alongside the rows, so an
// assertion can name both the shape and the answer. The plan is read from the
// planning event rather than from EXPLAIN because EXPLAIN is a second planning
// call: reading the event pins the plan that produced THESE rows.
func distinctEmailRun(
	t *testing.T, ctx context.Context, conn *sql.Conn,
	logger *syncCaptureLogger, query string,
) (string, []string) {
	t.Helper()
	before := len(logger.snapshot())
	got := queryIndexStateStrings(t, ctx, conn, query)
	events := logger.snapshot()
	if len(events) != before+1 {
		t.Fatalf("planning events for %q = %d, want exactly one more than %d",
			query, len(events), before)
	}
	return events[len(events)-1].PlanExplain, got
}

// THE SQL-PATH WITNESS for a secondary UNIQUE index used as a distinctness
// proof. It replaces the pin of the decline that used to stand here: the
// decline's own comment named this migration in advance, and this is it.
//
// What the decline feared was real and is now handled rather than avoided. A
// READABLE_UNIQUE_PENDING index is scannable and carries a `unique` flag its
// data contradicts, and eliding a DISTINCT yields a plan that never READS the
// proving index — so no executor leaf check can catch it. Two mechanisms
// COMPOSE to cover that, and they do not overlap:
//
//   - The candidate set is state-filtered exactly as Java's is
//     (readableIndexesFrom / cascades.ReadableIndexes, the port of
//     PlanContext.Builder.getReadableIndexes) on Java's strict isReadable, which
//     excludes READABLE_UNIQUE_PENDING. That is a PLANNING-time exclusion, and
//     it is what the pending arm below exercises.
//   - The proving index is stamped onto the yielded plan and becomes a plan
//     DEPENDENCY revalidated in every execution transaction. That catches a
//     transition landing AFTER planning, which no plan-time filter and no
//     plan-cache key can see, and it is what this file's other tests exercise.
//
// The readable arms assert the two shapes MEASURED on this fixture, because the
// discharge is a fact about the stream, not about the catalog. EMAIL is
// nullable, so a bare SELECT DISTINCT cannot be fully elided — a UNIQUE index
// admits arbitrarily many NULL entries and DISTINCT must collapse them to one
// row — and the operator is instead KEPT and NARROWED to dedup only that exempt
// subset. Adding a NULL-rejecting conjunct empties the exempt set on this
// stream and the operator disappears entirely. Both carry the proving index's
// name, which is what makes "the rule fired" a positive assertion rather than
// the absence of an operator.
func TestFDB_IndexStatePlanning_SecondaryUniqueIndexProvesDistinctnessOnlyWhileReadable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIndexStatePlanningFixture(t)
	logger := &syncCaptureLogger{}
	conn := installLogger(t, f.db, logger)

	const (
		bare         = "SELECT DISTINCT EMAIL FROM T"
		nullRejected = "SELECT DISTINCT EMAIL FROM T WHERE EMAIL IS NOT NULL"
		wantRows     = "a@example,b@example,c@example"
	)

	// READABLE, unfiltered: the operator SURVIVES, narrowed by U_EMAIL.
	explain, got := distinctEmailRun(t, ctx, conn, logger, bare)
	if strings.Join(got, ",") != wantRows {
		t.Fatalf("readable/unfiltered rows = %v, want %s", got, wantRows)
	}
	if !strings.Contains(explain, "Distinct(") {
		t.Fatalf("a bare SELECT DISTINCT over a NULLABLE unique key lost its "+
			"operator entirely: %s\nA UNIQUE index admits many NULL entries, so "+
			"full elision here would emit one row per NULL row.", explain)
	}
	if !strings.Contains(explain, "narrowed-by:U_EMAIL") {
		t.Fatalf("the DISTINCT over a READABLE secondary UNIQUE index is not "+
			"narrowed by it: %s\nEither the index-state filter stopped admitting a "+
			"READABLE index, or the narrowing stopped firing; the two fail here "+
			"identically and the pending arm below is what separates them.", explain)
	}

	// READABLE, NULL-rejecting: the operator is GONE, licensed by U_EMAIL.
	explain, got = distinctEmailRun(t, ctx, conn, logger, nullRejected)
	if strings.Join(got, ",") != wantRows {
		t.Fatalf("readable/null-rejected rows = %v, want %s", got, wantRows)
	}
	if strings.Contains(explain, "Distinct(") {
		t.Fatalf("a NULL-rejecting conjunct on the unique key did not empty the "+
			"exempt set; the operator is still there: %s", explain)
	}
	if !strings.Contains(explain, "distinct-by:U_EMAIL") {
		t.Fatalf("the elided plan carries no proof stamp: %s\nAn elision without "+
			"the stamp is an elision whose dependency on the index's state was "+
			"never recorded, which is the unguarded elision the decline feared.",
			explain)
	}

	// READABLE_UNIQUE_PENDING: the declared uniqueness is not yet proven, so the
	// index licenses NOTHING. This is the arm the metadata-only harness path
	// structurally cannot make — it plans in the unknown index state, where the
	// secondary-UNIQUE arm never fires at all, so it pins the fail-closed
	// default rather than the candidate filter.
	if err := f.makeUniqueIndexPending(ctx); err != nil {
		t.Fatalf("transition U_EMAIL to READABLE_UNIQUE_PENDING: %v", err)
	}
	for _, query := range []string{bare, nullRejected} {
		explain, got = distinctEmailRun(t, ctx, conn, logger, query)
		if strings.Join(got, ",") != wantRows {
			t.Fatalf("pending %q rows = %v, want %s", query, got, wantRows)
		}
		if !strings.Contains(explain, "Distinct(") {
			t.Fatalf("a READABLE_UNIQUE_PENDING index eliminated the DISTINCT in "+
				"%q: %s\nThat index is scannable and carries a `unique` flag its "+
				"data contradicts; trusting it is how duplicate rows leave a "+
				"DISTINCT.", query, explain)
		}
		if strings.Contains(explain, "U_EMAIL") {
			t.Fatalf("a READABLE_UNIQUE_PENDING index appears in the plan for %q: "+
				"%s\nIt must license neither an elision, nor a narrowing, nor a "+
				"scan.", query, explain)
		}
	}
}

// writeGhostIndexState plants a state key for an index name the METADATA does
// not have. This is not a contrived state: an index-state key outlives the
// index whose name it holds whenever metadata is evolved to drop an index
// without vacuuming its state key, and nothing in the read path removes it.
func (f *indexStatePlanningFixture) writeGhostIndexState(ctx context.Context, name string) error {
	_, err := f.rdb.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		key := f.ss.Sub(recordlayer.IndexStateSpaceKey).Pack(tuple.Tuple{name})
		rctx.Transaction().Set(key, tuple.Tuple{int64(recordlayer.IndexStateWriteOnly)}.Pack())
		return nil, nil
	})
	return err
}

// A signature compared across two DOMAINS is not a weaker version of a
// signature compared across one — it is a different property, and it fails
// catastrophically rather than gradually.
//
// The planning side once read the raw index-state subspace while execution read
// the metadata's index set. On a healthy store the two agree, which is exactly
// what makes the divergence invisible until a store has a state key for a name
// the metadata no longer carries. Then the signatures can never match: every
// query fails 40001 forever, and because 40001 means "retry", a client obeys and
// the replan re-derives the same mismatch.
//
// A stray key must therefore be INERT. The store still answers, and the
// staleness check still works — the second half is what stops this from being
// satisfied by simply deleting the check.
func TestFDB_IndexStatePlanning_StrayIndexStateKeyIsInert(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIndexStatePlanningFixture(t)
	if err := f.writeGhostIndexState(ctx, "IDX_DROPPED_LONG_AGO"); err != nil {
		t.Fatalf("plant stray index-state key: %v", err)
	}
	// One row per page, so the armed-check below spans real page boundaries
	// rather than finding every row already buffered from a single fetch.
	conn := pinEmbeddedConn(t, f.db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, 1).Build())
	})

	// Repeated, because the failure this pins is PERMANENT: the first query
	// misses the plan cache and the second hits it, and both re-derive the
	// signature, so one attempt could pass by luck where three cannot.
	for i := 0; i < 3; i++ {
		got := queryIndexStateStrings(t, ctx, conn, "SELECT EMAIL FROM T")
		if strings.Join(got, ",") != "a@example,b@example,c@example" {
			t.Fatalf("attempt %d returned %v", i, got)
		}
	}

	// The check is still armed: a REAL transition on an index the metadata DOES
	// name must still be caught. Without this, deleting the check outright
	// would satisfy the assertions above.
	rows, err := conn.QueryContext(ctx, dependentOnUEmail)
	if err != nil {
		t.Fatalf("query before transition: %v", err)
	}
	defer func() { _ = rows.Close() }()
	if err := f.makeUniqueIndexPending(ctx); err != nil {
		t.Fatalf("transition: %v", err)
	}
	for rows.Next() {
	}
	assertSerializationFailure(t, rows.Err())
}

// The planning-path store open FAILS CLOSED: a store that cannot be opened is
// an error, not a silent "assume every index is readable".
//
// EXPLAIN is the arm that decides it. planExplain's own contract says that from
// the point the Cascades plan is built, "the Cascades plan IS the plan. Every
// failure below is the failure `SELECT ...` would raise, so it is surfaced
// verbatim." Failing open broke exactly that: EXPLAIN rendered a plan for a
// store that does not exist, while the SELECT it claims to describe could not
// run at all. Planning against a GUESSED state and then validating the guess
// downstream is incoherent in the same way.
func TestFDB_IndexStatePlanning_MissingStoreFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIndexStatePlanningFixture(t)
	conn := pinEmbeddedConn(t, f.db, func(ec *embedded.EmbeddedConnection) {})

	if _, err := f.rdb.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		return nil, recordlayer.DeleteStore(rctx, f.ss)
	}); err != nil {
		t.Fatalf("clear the schema's store subspace: %v", err)
	}

	rows, err := conn.QueryContext(ctx, "SELECT ID FROM T")
	if rows != nil {
		_ = rows.Close()
	}
	if err == nil {
		t.Fatal("SELECT against a nonexistent store succeeded")
	}
	explainRows, explainErr := conn.QueryContext(ctx, "EXPLAIN SELECT ID FROM T")
	if explainRows != nil {
		_ = explainRows.Close()
	}
	if explainErr == nil {
		t.Fatal("EXPLAIN rendered a plan for a store that does not exist, " +
			"while the SELECT it claims to describe cannot run")
	}
}

// PlanGenerationLogger's contract is ONE callback per Plan() call
// (plan_logging.go:73-77). "One per call that SUCCEEDS" would be a different and
// much weaker promise, and the difference is not academic: an operator installs
// this logger to see planning failures, so a failure path that emits nothing is
// silent about exactly what the logger exists to report.
//
// The index-state store open is the first fallible step on the planning path and
// it fails CLOSED, so it is the failure most likely to be observed — and it was
// the one that emitted nothing, because the logging scope was opened after it.
//
// EXACTLY one, in both directions: zero means the failure was invisible, and
// more than one means a caller counting plans would double-count.
func TestFDB_IndexStatePlanning_FailedStoreOpenStillLogsOnePlanEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIndexStatePlanningFixture(t)
	logger := &syncCaptureLogger{}
	conn := installLogger(t, f.db, logger)

	if _, err := f.rdb.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		return nil, recordlayer.DeleteStore(rctx, f.ss)
	}); err != nil {
		t.Fatalf("clear the schema's store subspace: %v", err)
	}

	rows, queryErr := conn.QueryContext(ctx, "SELECT ID FROM T")
	if rows != nil {
		_ = rows.Close()
	}
	if queryErr == nil {
		t.Fatal("SELECT against a nonexistent store succeeded; this test can no " +
			"longer reach the planning failure it exists to observe")
	}

	events := logger.snapshot()
	if len(events) != 1 {
		t.Fatalf("planning events = %d, want exactly 1 for one Plan() call", len(events))
	}
	if events[0].Err == nil {
		t.Fatalf("the logged event reported success for a planning call that "+
			"failed with: %v", queryErr)
	}
}

// setIndexState drives any index of the fixture's schema to a state, out of
// band, so a test can move an index the SQL layer is not looking at.
func (f *indexStatePlanningFixture) setIndexState(
	ctx context.Context, name string, readable bool,
) error {
	_, err := f.rdb.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		store, openErr := recordlayer.NewStoreBuilder().
			SetContext(rctx).
			SetMetaDataProvider(f.md).
			SetSubspace(f.ss).
			SetStoreStateCache(recordlayer.PassThroughStoreStateCache()).
			Open()
		if openErr != nil {
			return nil, openErr
		}
		if readable {
			// An index cannot be marked readable with unbuilt ranges; a real
			// build writes the range set, so the test does too.
			idx := f.md.GetIndex(name)
			if idx == nil {
				return nil, fmt.Errorf("%s missing from matching metadata", name)
			}
			if _, rangeErr := recordlayer.NewIndexingRangeSet(f.ss, idx).
				InsertRange(rctx.Transaction(), nil, nil, false); rangeErr != nil {
				return nil, rangeErr
			}
			_, stateErr := store.MarkIndexReadable(name)
			return nil, stateErr
		}
		_, stateErr := store.MarkIndexWriteOnly(name)
		return nil, stateErr
	})
	return err
}

// setIndexDisabled drives an index to DISABLED, the state setIndexState cannot
// reach. DISABLED and WRITE_ONLY are both non-READABLE but they are not the same
// state — DISABLED additionally clears the index data — and a check written
// against "not readable" must hold for both.
func (f *indexStatePlanningFixture) setIndexDisabled(ctx context.Context, name string) error {
	_, err := f.rdb.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		store, openErr := recordlayer.NewStoreBuilder().
			SetContext(rctx).
			SetMetaDataProvider(f.md).
			SetSubspace(f.ss).
			SetStoreStateCache(recordlayer.PassThroughStoreStateCache()).
			Open()
		if openErr != nil {
			return nil, openErr
		}
		if _, stateErr := store.MarkIndexDisabled(name); stateErr != nil {
			return nil, stateErr
		}
		if got := store.GetIndexState(name); got != recordlayer.IndexStateDisabled {
			return nil, fmt.Errorf("%s state = %s, want DISABLED", name, got)
		}
		return nil, nil
	})
	return err
}

// planIndexScans reports the index names a logged plan scans, so a test can
// assert it is exercising the access path it claims to.
func planIndexScans(explain string) []string {
	var names []string
	for _, candidate := range []string{"U_EMAIL", "IDX_PAD"} {
		if strings.Contains(explain, candidate) {
			names = append(names, candidate)
		}
	}
	sort.Strings(names)
	return names
}

// A plan depends on the indexes it USES, not on the store's every index.
//
// Signing the whole index-state snapshot made any index transition anywhere in
// the schema invalidate every in-flight statement — an index build, the most
// ordinary administrative act there is, would 40001 unrelated queries across the
// database. That is a self-inflicted outage, and 40001 asks the client to retry
// into it.
//
// Both arms are required. The unrelated arm alone is satisfied by deleting the
// check; the dependent arm is what proves the scoping narrowed the check rather
// than removed it.
func TestFDB_IndexStatePlanning_OnlyDependenciesInvalidate(t *testing.T) {
	t.Parallel()
	const query = "SELECT EMAIL FROM T WHERE EMAIL > 'a'"

	t.Run("unrelated index transition keeps pages flowing", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		f := newIndexStatePlanningFixture(t)
		logger := &syncCaptureLogger{}
		conn := pinEmbeddedConn(t, f.db, func(ec *embedded.EmbeddedConnection) {
			ec.SetOptions(api.NewOptionsBuilder().
				Set(api.OptExecutionScannedRowsLimit, 1).Build())
			ec.SetPlanLogger(logger)
		})

		rows, err := conn.QueryContext(ctx, query)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer func() { _ = rows.Close() }()
		if err := f.setIndexState(ctx, "IDX_PAD", false); err != nil {
			t.Fatalf("take IDX_PAD WRITE_ONLY mid-statement: %v", err)
		}
		n := 0
		for rows.Next() {
			n++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("an unrelated index transition invalidated the plan: %v", err)
		}
		if n != 3 {
			t.Fatalf("rows = %d, want 3 across separate pages", n)
		}
		events := logger.snapshot()
		if len(events) != 1 {
			t.Fatalf("planning events = %d, want 1", len(events))
		}
		// If the plan never touched U_EMAIL, the arm below is testing a
		// dependency this one never had, and the pair proves nothing.
		if got := planIndexScans(events[0].PlanExplain); len(got) == 0 {
			t.Fatalf("the query scanned no index, so it has no dependency to "+
				"distinguish from IDX_PAD: %s", events[0].PlanExplain)
		} else if got[0] == "IDX_PAD" {
			t.Fatalf("the query scanned IDX_PAD, which this arm transitions: %s",
				events[0].PlanExplain)
		}
	})

	t.Run("dependent index transition still fires", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		f := newIndexStatePlanningFixture(t)
		conn := pinEmbeddedConn(t, f.db, func(ec *embedded.EmbeddedConnection) {
			ec.SetOptions(api.NewOptionsBuilder().
				Set(api.OptExecutionScannedRowsLimit, 1).Build())
		})
		rows, err := conn.QueryContext(ctx, query)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer func() { _ = rows.Close() }()
		if err := f.setIndexState(ctx, "U_EMAIL", false); err != nil {
			t.Fatalf("take U_EMAIL WRITE_ONLY mid-statement: %v", err)
		}
		for rows.Next() {
		}
		assertSerializationFailure(t, rows.Err())
	})
}

// The check is DIRECTIONAL. An index that was EXCLUDED at planning and has since
// become readable leaves the plan correct — merely less optimal than the one
// that would be planned now. Invalidating there would mean every completed index
// build kills the statements running alongside it, which is the same outage as
// the unscoped check, arriving from the opposite direction.
//
// Java reaches this structurally: RecordStoreState.compatibleWith iterates the
// CURRENT state's index map, which holds only the non-readable exceptions, so an
// index readable now and excluded then is never examined at all.
func TestFDB_IndexStatePlanning_IndexBecomingReadableDoesNotInvalidate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIndexStatePlanningFixture(t)
	// Excluded BEFORE planning, so no plan can depend on it.
	if err := f.setIndexState(ctx, "IDX_PAD", false); err != nil {
		t.Fatalf("take IDX_PAD WRITE_ONLY before planning: %v", err)
	}
	conn := pinEmbeddedConn(t, f.db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptExecutionScannedRowsLimit, 1).Build())
	})

	rows, err := conn.QueryContext(ctx, "SELECT EMAIL FROM T WHERE EMAIL > 'a'")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	if err := f.setIndexState(ctx, "IDX_PAD", true); err != nil {
		t.Fatalf("make IDX_PAD readable mid-statement: %v", err)
	}
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("an index BECOMING readable invalidated a plan that never used it: %v", err)
	}
	if n != 3 {
		t.Fatalf("rows = %d, want 3 across separate pages", n)
	}
}
