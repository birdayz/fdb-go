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

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

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
	"ID BIGINT NOT NULL, EMAIL STRING NOT NULL, PAD BIGINT NOT NULL, PRIMARY KEY (ID)) " +
	"CREATE UNIQUE INDEX U_EMAIL ON T (EMAIL)"

type indexStatePlanningFixture struct {
	db  *sql.DB
	rdb *recordlayer.FDBDatabase
	md  *recordlayer.RecordMetaData
	ss  subspace.Subspace
}

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
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("EMAIL", api.NewStringType(false), 2),
		metadata.NewColumnSpec("PAD", api.NewLongType(false), 3),
	}, []string{"ID"})
	b.AddIndex("T", "U_EMAIL", []string{"EMAIL"}, true)
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build matching metadata: %v", err)
	}

	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatalf("open raw FDB: %v", err)
	}
	t.Cleanup(rawDB.Close)
	rdb := recordlayer.NewFDBDatabase(rawDB)
	ks := relkeyspace.New(subspace.Sub())
	ss, err := ks.SchemaSubspace(dbPath, schemaName)
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

func (f *indexStatePlanningFixture) makeUniqueIndexPending(ctx context.Context, addDuplicate bool) error {
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
		if addDuplicate {
			desc := f.md.GetRecordType("T").Descriptor
			rec := dynamicpb.NewMessage(desc)
			set := func(name protoreflect.Name, value protoreflect.Value) error {
				field := desc.Fields().ByName(name)
				if field == nil {
					return fmt.Errorf("field %s missing", name)
				}
				rec.Set(field, value)
				return nil
			}
			if setErr := set("ID", protoreflect.ValueOfInt64(4)); setErr != nil {
				return nil, setErr
			}
			if setErr := set("EMAIL", protoreflect.ValueOfString("a@example")); setErr != nil {
				return nil, setErr
			}
			if setErr := set("PAD", protoreflect.ValueOfInt64(40)); setErr != nil {
				return nil, setErr
			}
			if _, saveErr := store.SaveRecord(proto.Message(rec)); saveErr != nil {
				return nil, saveErr
			}
		} else if violationErr := store.AddUniquenessViolation(
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

type transitionPlanLogger struct {
	capture syncCaptureLogger
	once    sync.Once
	fn      func() error

	mu  sync.Mutex
	err error
}

func (l *transitionPlanLogger) LogPlanGeneration(ctx context.Context, info embedded.PlanGenerationInfo) {
	l.capture.LogPlanGeneration(ctx, info)
	l.once.Do(func() {
		err := l.fn()
		l.mu.Lock()
		l.err = err
		l.mu.Unlock()
	})
}

func (l *transitionPlanLogger) snapshot() ([]embedded.PlanGenerationInfo, error) {
	events := l.capture.snapshot()
	l.mu.Lock()
	defer l.mu.Unlock()
	return events, l.err
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

func TestFDB_IndexStatePlanning_PendingUniqueDuplicatesReplanAndKeepDistinct(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIndexStatePlanningFixture(t)
	logger := &syncCaptureLogger{}
	conn := installLogger(t, f.db, logger)
	const query = "SELECT DISTINCT EMAIL FROM T"

	for i := 0; i < 2; i++ {
		if got := queryIndexStateStrings(t, ctx, conn, query); strings.Join(got, ",") !=
			"a@example,b@example,c@example" {
			t.Fatalf("readable query %d = %v", i, got)
		}
	}
	if err := f.makeUniqueIndexPending(ctx, true); err != nil {
		t.Fatalf("make U_EMAIL pending with a real duplicate: %v", err)
	}
	if got := queryIndexStateStrings(t, ctx, conn, query); strings.Join(got, ",") !=
		"a@example,b@example,c@example" {
		t.Fatalf("pending DISTINCT = %v, want the duplicate removed", got)
	}

	events := logger.snapshot()
	if len(events) != 3 {
		t.Fatalf("planning events = %d, want 3", len(events))
	}
	if events[0].Cache != embedded.PlanCacheMiss || events[1].Cache != embedded.PlanCacheHit ||
		events[2].Cache != embedded.PlanCacheMiss {
		t.Fatalf("cache sequence = [%s %s %s], want [miss hit miss]",
			events[0].Cache, events[1].Cache, events[2].Cache)
	}
	if strings.Contains(events[0].PlanExplain, "Distinct") {
		t.Fatalf("READABLE UNIQUE proof failed to eliminate DISTINCT: %s", events[0].PlanExplain)
	}
	if !strings.Contains(events[2].PlanExplain, "Distinct") {
		t.Fatalf("PENDING UNIQUE plan still assumed global uniqueness: %s", events[2].PlanExplain)
	}
	if strings.Contains(events[2].PlanExplain, "U_EMAIL") {
		t.Fatalf("PENDING index was admitted as an access candidate: %s", events[2].PlanExplain)
	}
}

func TestFDB_IndexStatePlanning_TransitionAfterPrimaryProofPlanFails40001(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIndexStatePlanningFixture(t)
	logger := &transitionPlanLogger{fn: func() error {
		return f.makeUniqueIndexPending(ctx, false)
	}}
	conn := pinEmbeddedConn(t, f.db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptDisabledPlannerRules, []string{"MatchLeafRule"}).Build())
		ec.SetPlanLogger(logger)
	})

	rows, err := conn.QueryContext(ctx, "SELECT DISTINCT EMAIL FROM T")
	if rows != nil {
		_ = rows.Close()
	}
	assertSerializationFailure(t, err)
	events, transitionErr := logger.snapshot()
	if transitionErr != nil {
		t.Fatalf("logger state transition: %v", transitionErr)
	}
	if len(events) != 1 {
		t.Fatalf("planning events = %d, want 1", len(events))
	}
	plan := events[0].PlanExplain
	if !strings.Contains(plan, "Scan(T)") || strings.Contains(plan, "IndexScan") {
		t.Fatalf("test did not exercise a primary-scan proof-only plan: %s", plan)
	}
	if strings.Contains(plan, "Distinct") {
		t.Fatalf("test did not exercise the READABLE global-uniqueness proof: %s", plan)
	}
}

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
	if err := f.makeUniqueIndexPending(ctx, false); err != nil {
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
