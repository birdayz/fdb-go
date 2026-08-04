package sqldriver_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

const indexStatePlanningDDL = "CREATE TABLE T (" +
	"ID BIGINT, EMAIL STRING, PAD BIGINT, PRIMARY KEY (ID)) " +
	"CREATE UNIQUE INDEX U_EMAIL ON T (EMAIL)"

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

	rows, err := conn.QueryContext(ctx, "SELECT ID FROM T")
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
	var id int64
	if err := rows.Scan(&id); err != nil {
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

	rows, err := conn.QueryContext(ctx, "SELECT ID FROM T")
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

// PIN OF A NEGATIVE RESULT whose stated unlock condition has SINCE BEEN MET.
//
// This planner does NOT trust a secondary UNIQUE index as a distinctness proof
// — rule_implement_distinct_final.go declines it explicitly. Its stated reason
// was that Go built match candidates from metadata alone, so a
// READABLE_UNIQUE_PENDING index (scannable, carrying a `unique` flag its data
// contradicts) could back a uniqueness proof; and because eliding a DISTINCT
// produces a plan that never READS that index, no executor leaf check could
// catch it. The decline is the ONLY reason duplicate rows cannot flow out of a
// DISTINCT in that state, and nothing else pins it.
//
// That reason no longer holds. The candidate set IS now state-filtered the way
// Java's is (readableIndexesFrom / cascades.ReadableIndexes, the port of
// PlanContext.Builder.getReadableIndexes), and it excludes
// READABLE_UNIQUE_PENDING on Java's strict isReadable. The decline's own
// comment still says "until then" — the precondition it names is satisfied and
// the decline was simply never revisited.
//
// So this test now pins a CONSERVATIVE LEFTOVER, not a necessity. It is kept
// because the behaviour it describes is real and current, and because lifting
// the decline is a query-engine change that must be made deliberately, with its
// own review — not drifted into. Whoever lifts it replaces this test with
// coverage of the pending state; they do not delete it to go green.
//
// Note the two mechanisms compose rather than overlap: the filter excludes a
// pending index at PLANNING time, and the signature this file's other tests
// exercise catches a transition that happens AFTER planning, which no
// plan-time filter and no plan-cache key can see.
func TestFDB_IndexStatePlanning_SecondaryUniqueIndexIsNotADistinctnessProof(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIndexStatePlanningFixture(t)
	logger := &syncCaptureLogger{}
	conn := installLogger(t, f.db, logger)

	rows, err := conn.QueryContext(ctx, "SELECT DISTINCT EMAIL FROM T")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	var got []string
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, email)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	_ = rows.Close()
	if len(got) != 3 {
		t.Fatalf("rows = %v, want 3 distinct emails", got)
	}

	events := logger.snapshot()
	if len(events) != 1 {
		t.Fatalf("planning events = %d, want 1", len(events))
	}
	if !strings.Contains(events[0].PlanExplain, "Distinct") {
		t.Fatalf("a READABLE secondary UNIQUE index eliminated DISTINCT — if that "+
			"was deliberate, the pending-unique state now needs its own coverage "+
			"here rather than this assertion: %s", events[0].PlanExplain)
	}
}
