package testkit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	relkeyspace "fdb.dev/pkg/relational/core/keyspace"
	"fdb.dev/pkg/relational/core/metadata"
)

const indexStatePlanningDDL = "CREATE TABLE T (" +
	"ID BIGINT, EMAIL STRING, PAD BIGINT, PRIMARY KEY (ID)) " +
	"CREATE UNIQUE INDEX U_EMAIL ON T (EMAIL) " +
	"CREATE INDEX IDX_PAD ON T (PAD)"

type IndexStatePlanningFixture struct {
	DB  *sql.DB
	RDB *recordlayer.FDBDatabase
	md  *recordlayer.RecordMetaData
	Ss  subspace.Subspace
}

// newIndexStatePlanningFixture builds a schema with one secondary UNIQUE index
// and a second, byte-identical metadata handle reached OUTSIDE the SQL layer.
// The out-of-band handle is what lets a test drive an index through a state
// transition mid-statement, which is not expressible in SQL.
func NewIndexStatePlanningFixture(t *testing.T) *IndexStatePlanningFixture {
	t.Helper()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	dbPath := "/FRL/idxstate_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	schemaName := "idxstate_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	db := SetupErrorDB(t, dbPath, schemaName, indexStatePlanningDDL)

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
	// DDL stores the database path and the schema under their CANONICAL
	// (upper-cased) names, so the out-of-band handle must ask for the same
	// subspace the driver wrote.
	ks := relkeyspace.New(subspace.Sub())
	ss, err := ks.LookupSchemaSubspace(context.Background(), rdb, strings.ToUpper(dbPath), strings.ToUpper(schemaName))
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
	return &IndexStatePlanningFixture{DB: db, RDB: rdb, md: tmpl.Underlying(), Ss: ss}
}

// makeUniqueIndexPending drives U_EMAIL from READABLE to
// READABLE_UNIQUE_PENDING: the state that is SCANNABLE but whose declared
// uniqueness the data contradicts (IndexState.java:59-66). It is the sharpest
// transition available, because a check written against scannability rather
// than Java's isReadable would not notice it.
func (f *IndexStatePlanningFixture) MakeUniqueIndexPending(ctx context.Context) error {
	_, err := f.RDB.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		store, openErr := recordlayer.NewStoreBuilder().
			SetContext(rctx).
			SetMetaDataProvider(f.md).
			SetSubspace(f.Ss).
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
		if _, rangeErr := recordlayer.NewIndexingRangeSet(f.Ss, idx).
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

// queryIndexStateStrings runs query INSIDE AN EXPLICIT TRANSACTION, which is the
// only regime in which a secondary-UNIQUE proof is drawn at all.
//
// The proof is a statement about the store at one instant, so it is licensed
// only where the whole result comes from a single read version; in auto-commit
// each page takes a fresh one and the proof is withheld. Every arm reached
// through this helper — the affirmative ones that require `narrowed-by:U_EMAIL`
// and the refusals that require its ABSENCE — therefore has to run in a
// transaction. The refusals are the reason it matters most: in auto-commit no
// index proves anything, so "a READABLE_UNIQUE_PENDING index licensed nothing"
// would hold for a reason that has nothing to do with the index's state.
func QueryIndexStateStrings(t *testing.T, ctx context.Context, conn *sql.Conn, query string) []string {
	t.Helper()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin for %q: %v", query, err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, query)
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

func AssertSerializationFailure(t *testing.T, err error) {
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

// distinctEmailRun plans and runs one query on the logger-equipped conn and
// returns the plan text the planner actually chose alongside the rows, so an
// assertion can name both the shape and the answer. The plan is read from the
// planning event rather than from EXPLAIN because EXPLAIN is a second planning
// call: reading the event pins the plan that produced THESE rows.
func DistinctEmailRun(
	t *testing.T, ctx context.Context, conn *sql.Conn,
	logger *SyncCaptureLogger, query string,
) (string, []string) {
	t.Helper()
	before := len(logger.Snapshot())
	got := QueryIndexStateStrings(t, ctx, conn, query)
	events := logger.Snapshot()
	if len(events) != before+1 {
		t.Fatalf("planning events for %q = %d, want exactly one more than %d",
			query, len(events), before)
	}
	return events[len(events)-1].PlanExplain, got
}

// writeGhostIndexState plants a state key for an index name the METADATA does
// not have. This is not a contrived state: an index-state key outlives the
// index whose name it holds whenever metadata is evolved to drop an index
// without vacuuming its state key, and nothing in the read path removes it.
func (f *IndexStatePlanningFixture) WriteGhostIndexState(ctx context.Context, name string) error {
	_, err := f.RDB.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		key := f.Ss.Sub(recordlayer.IndexStateSpaceKey).Pack(tuple.Tuple{name})
		rctx.Transaction().Set(key, tuple.Tuple{int64(recordlayer.IndexStateWriteOnly)}.Pack())
		return nil, nil
	})
	return err
}

// setIndexState drives any index of the fixture's schema to a state, out of
// band, so a test can move an index the SQL layer is not looking at.
func (f *IndexStatePlanningFixture) SetIndexState(
	ctx context.Context, name string, readable bool,
) error {
	_, err := f.RDB.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		store, openErr := recordlayer.NewStoreBuilder().
			SetContext(rctx).
			SetMetaDataProvider(f.md).
			SetSubspace(f.Ss).
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
			if _, rangeErr := recordlayer.NewIndexingRangeSet(f.Ss, idx).
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
func (f *IndexStatePlanningFixture) SetIndexDisabled(ctx context.Context, name string) error {
	_, err := f.RDB.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		store, openErr := recordlayer.NewStoreBuilder().
			SetContext(rctx).
			SetMetaDataProvider(f.md).
			SetSubspace(f.Ss).
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
