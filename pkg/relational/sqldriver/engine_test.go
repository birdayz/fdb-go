package sqldriver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/internal/dst"
	"fdb.dev/internal/dstwire"
	"fdb.dev/internal/relational/relapi"
	"fdb.dev/internal/simfdb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/embedded"
)

func newEngineSimDB(seed uint64) *recordlayer.FDBDatabase {
	env := dst.NewSim(seed)
	env.Buggify = dst.DisabledBuggifier()
	db := recordlayer.NewFDBDatabaseWithBackend(simfdb.New(env)).SetEnv(dstwire.Env(env))
	db.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())
	return db
}

type closeCountingBackend struct {
	fdb.BackendDatabase
	closes atomic.Int32
}

func (b *closeCountingBackend) Close() {
	b.closes.Add(1)
	b.BackendDatabase.Close()
}

func setupEngineSchema(t *testing.T, eng *Engine, dbPath, schema, tableDDL string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	setupDB, err := eng.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB(%q): %v", dbPath, err)
	}
	defer setupDB.Close()

	tmplName := "TMPL_" + schema
	for _, stmt := range []string{
		"CREATE DATABASE " + dbPath,
		fmt.Sprintf("CREATE SCHEMA TEMPLATE %s %s", tmplName, tableDDL),
		fmt.Sprintf("CREATE SCHEMA %s/%s WITH TEMPLATE %s", dbPath, schema, tmplName),
	} {
		if _, err := setupDB.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	db, err := eng.OpenDB(dbPath, WithSchema(schema))
	if err != nil {
		t.Fatalf("OpenDB(%q, schema=%s): %v", dbPath, schema, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestEngine_LifecycleAndNoGlobalCacheLeak(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	simDB := newEngineSimDB(1001)

	eng, err := NewEngine(EngineConfig{
		Domains: []string{"ENG_LIFE"},
	}, WithDatabase(simDB))
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	if eng.StoreTimer() == nil {
		t.Fatal("StoreTimer() = nil, want non-nil")
	}

	db := setupEngineSchema(t, eng, "/ENG_LIFE/APP", "S",
		"CREATE TABLE item (id BIGINT, name STRING, PRIMARY KEY (id))")

	if _, err := db.ExecContext(ctx, "INSERT INTO item (id, name) VALUES (1, 'one'), (2, 'two')"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	var count int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM item").Scan(&count); err != nil {
		t.Fatalf("SELECT COUNT: %v", err)
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}

	// Verify Engine does not leak into process-global fdbDBCache.
	fdbDBCache.Range(func(key, value any) bool {
		if value == simDB {
			t.Errorf("Engine leaked database into process-global fdbDBCache under key %q", key)
		}
		return true
	})

	metrics := eng.Metrics()
	if len(metrics.StoreTimer) == 0 {
		t.Error("Metrics().StoreTimer is empty after SQL writes and reads")
	}
	if metrics.PlanCache.PrimaryMiss == 0 {
		t.Errorf("Metrics().PlanCache.PrimaryMiss = 0, want > 0")
	}
	_ = eng.WarmUp()

	conn, err := eng.Connector("/ENG_LIFE/APP", WithSchema("S"))
	if err != nil {
		t.Fatalf("Connector before Close: %v", err)
	}

	// Close is idempotent and prevents subsequent Connector/OpenDB/Connect calls.
	if err := eng.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := eng.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	var ae *api.Error
	if _, err := eng.OpenDB("/ENG_LIFE/APP"); !errors.As(err, &ae) || ae.Code != api.ErrCodeConnectionDoesNotExist {
		t.Errorf("OpenDB after Close = %v, want ErrCodeConnectionDoesNotExist", err)
	}
	if _, err := eng.Connector("/ENG_LIFE/APP"); !errors.As(err, &ae) || ae.Code != api.ErrCodeConnectionDoesNotExist {
		t.Errorf("Connector after Close = %v, want ErrCodeConnectionDoesNotExist", err)
	}
	if _, err := conn.Connect(ctx); !errors.As(err, &ae) || ae.Code != api.ErrCodeConnectionDoesNotExist {
		t.Errorf("Connect after Close = %v, want ErrCodeConnectionDoesNotExist", err)
	}
}

func TestEngine_ConstructorValidationAndOwnedClose(t *testing.T) {
	t.Parallel()
	var ae *api.Error

	if _, err := NewEngine(EngineConfig{}, WithDatabase(nil)); !errors.As(err, &ae) || ae.Code != api.ErrCodeInvalidParameter {
		t.Errorf("WithDatabase(nil) = %v, want ErrCodeInvalidParameter", err)
	}
	if _, err := NewEngine(EngineConfig{ClusterFile: "/some/cluster"}, WithDatabase(newEngineSimDB(1002))); !errors.As(err, &ae) || ae.Code != api.ErrCodeInvalidParameter {
		t.Errorf("ClusterFile + WithDatabase = %v, want ErrCodeInvalidParameter", err)
	}
	if _, err := NewEngine(EngineConfig{
		ClusterFile: filepath.Join(t.TempDir(), "missing.cluster"),
	}); err == nil {
		t.Error("NewEngine with missing cluster file succeeded, want error")
	}

	// Verify ownedBackend is closed exactly once across repeated Engine.Close() calls.
	env := dst.NewSim(1003)
	env.Buggify = dst.DisabledBuggifier()
	backend := &closeCountingBackend{BackendDatabase: simfdb.New(env)}
	eng := &Engine{
		fdbDB:        recordlayer.NewFDBDatabaseWithBackend(backend).SetEnv(dstwire.Env(env)),
		ownedBackend: backend,
		planCache:    embedded.NewRelationalPlanCache(nil),
	}
	if err := eng.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := eng.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := backend.closes.Load(); got != 1 {
		t.Fatalf("ownedBackend.Close() called %d times, want 1", got)
	}
}

func TestEngine_DomainIsolationBetweenTwoEngines(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Register a global domain in the same process to prove instance-scoped
	// Engines do not inherit global domains.
	RegisterDomainIfNotExists("ENG_GLOBAL_DOM")

	// Share one underlying SimFDB so catalog entries created by eng1 are visible
	// in storage, testing that domain enforcement isolates the two engines.
	sharedDB := newEngineSimDB(1004)

	eng1, err := NewEngine(EngineConfig{Domains: []string{"ALPHA"}}, WithDatabase(sharedDB))
	if err != nil {
		t.Fatalf("eng1: %v", err)
	}
	defer eng1.Close()

	eng2, err := NewEngine(EngineConfig{Domains: []string{"BETA"}}, WithDatabase(sharedDB))
	if err != nil {
		t.Fatalf("eng2: %v", err)
	}
	defer eng2.Close()

	if !slices.Equal(eng1.RegisteredDomains(), []string{"ALPHA"}) {
		t.Fatalf("eng1 domains = %v, want [ALPHA]", eng1.RegisteredDomains())
	}
	if !slices.Equal(eng2.RegisteredDomains(), []string{"BETA"}) {
		t.Fatalf("eng2 domains = %v, want [BETA]", eng2.RegisteredDomains())
	}

	db1 := setupEngineSchema(t, eng1, "/ALPHA/DB1", "S",
		"CREATE TABLE item (id BIGINT, PRIMARY KEY (id))")
	if _, err := db1.ExecContext(ctx, "INSERT INTO item (id) VALUES (42)"); err != nil {
		t.Fatalf("eng1 INSERT: %v", err)
	}

	// eng1 rejects BETA and global ENG_GLOBAL_DOM.
	sys1, err := eng1.OpenDB("/__SYS")
	if err != nil {
		t.Fatalf("eng1 OpenDB(/__SYS): %v", err)
	}
	defer sys1.Close()
	for _, badStmt := range []string{
		"CREATE DATABASE /BETA/DB2",
		"CREATE DATABASE /ENG_GLOBAL_DOM/DB3",
	} {
		var ae *api.Error
		if _, err := sys1.ExecContext(ctx, badStmt); !errors.As(err, &ae) || ae.Code != api.ErrCodeInvalidPath {
			t.Errorf("eng1 %q = %v, want ErrCodeInvalidPath", badStmt, err)
		}
	}

	// eng2 accepts BETA, rejects ALPHA, and cannot query /ALPHA/DB1 even though
	// /ALPHA/DB1/S exists in the shared catalog.
	db2 := setupEngineSchema(t, eng2, "/BETA/DB2", "S2",
		"CREATE TABLE item (id BIGINT, PRIMARY KEY (id))")
	if _, err := db2.ExecContext(ctx, "INSERT INTO item (id) VALUES (7)"); err != nil {
		t.Fatalf("eng2 INSERT: %v", err)
	}

	sys2, err := eng2.OpenDB("/__SYS")
	if err != nil {
		t.Fatalf("eng2 OpenDB(/__SYS): %v", err)
	}
	defer sys2.Close()
	var ae *api.Error
	if _, err := sys2.ExecContext(ctx, "CREATE DATABASE /ALPHA/DB4"); !errors.As(err, &ae) || ae.Code != api.ErrCodeInvalidPath {
		t.Errorf("eng2 CREATE DATABASE /ALPHA/DB4 = %v, want ErrCodeInvalidPath", err)
	}

	crossDB, err := eng2.OpenDB("/ALPHA/DB1", WithSchema("S"))
	if err != nil {
		t.Fatalf("eng2 OpenDB(/ALPHA/DB1): %v", err)
	}
	defer crossDB.Close()
	var id int64
	if err := crossDB.QueryRowContext(ctx, "SELECT id FROM item").Scan(&id); !errors.As(err, &ae) || ae.Code != api.ErrCodeInvalidPath {
		t.Errorf("eng2 querying /ALPHA/DB1 = %v, want ErrCodeInvalidPath", err)
	}
}

func TestEngine_ResourceLimitsPresets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	def := DefaultLimits()
	if def.MaxRows <= 0 || def.ScannedRowsLimit <= 0 || def.ScannedBytesLimit <= 0 ||
		def.PageTimeLimit <= 0 || !def.FailOnScanLimitReached || def.StatementTimeout <= 0 ||
		def.MaxStatementMemoryBytes <= 0 || def.MaxResultBytes <= 0 {
		t.Fatalf("DefaultLimits() has non-positive/disabled field: %+v", def)
	}
	unbounded := UnboundedJavaCompatLimits()
	if *unbounded != (ResourceLimits{}) {
		t.Fatalf("UnboundedJavaCompatLimits() = %+v, want zero struct", unbounded)
	}

	// Invalid ResourceLimits fail at NewEngine and Connector time.
	for _, bad := range []*ResourceLimits{
		{MaxRows: -1},
		{MaxRows: math.MaxInt32 + 1},
		{ScannedRowsLimit: -1},
		{ScannedRowsLimit: math.MaxInt32 + 1},
		{ScannedBytesLimit: -1},
		{PageTimeLimit: -time.Millisecond},
		{StatementTimeout: -time.Millisecond},
		{MaxStatementMemoryBytes: -1},
		{MaxResultBytes: -1},
		{TransactionTimeout: -time.Millisecond},
	} {
		var ae *api.Error
		if _, err := NewEngine(EngineConfig{Limits: bad}, WithDatabase(newEngineSimDB(1005))); !errors.As(err, &ae) || ae.Code != api.ErrCodeInvalidParameter {
			t.Errorf("NewEngine(%+v) = %v, want ErrCodeInvalidParameter", bad, err)
		}
	}

	// Default Limits: MaxRows=3 truncates, ScannedRowsLimit=3 + FailOnScanLimitReached fails 54F01,
	// MaxResultBytes=10 fails 54F01, and UnboundedJavaCompatLimits returns all 10 rows.
	simDB := newEngineSimDB(1006)
	eng, err := NewEngine(EngineConfig{
		Domains: []string{"LIM"},
		Limits: &ResourceLimits{
			MaxRows:                10,
			ScannedRowsLimit:       3,
			FailOnScanLimitReached: true,
		},
	}, WithDatabase(simDB))
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()

	// Seed 10 rows via unbounded connection override.
	seedDB := setupEngineSchema(t, eng, "/LIM/DB", "S",
		"CREATE TABLE item (id BIGINT, payload STRING, PRIMARY KEY (id))")
	unboundedDB, err := eng.OpenDB("/LIM/DB", WithSchema("S"), WithResourceLimits(UnboundedJavaCompatLimits()))
	if err != nil {
		t.Fatalf("OpenDB unbounded: %v", err)
	}
	defer unboundedDB.Close()
	for i := range 10 {
		if _, err := unboundedDB.ExecContext(ctx, fmt.Sprintf("INSERT INTO item (id, payload) VALUES (%d, 'payload-%02d')", i, i)); err != nil {
			t.Fatalf("INSERT %d: %v", i, err)
		}
	}

	// Engine's default ScannedRowsLimit=3 + FailOnScanLimitReached=true fails 54F01 on 10-row scan.
	rows, err := seedDB.QueryContext(ctx, "SELECT id FROM item")
	if err == nil {
		for rows.Next() {
		}
		err = rows.Err()
		_ = rows.Close()
	}
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != api.ErrCodeExecutionLimitReached {
		t.Errorf("scanned-row limit query err = %v, want 54F01", err)
	}

	// UnboundedJavaCompatLimits override returns all 10 rows.
	allRows, err := unboundedDB.QueryContext(ctx, "SELECT id FROM item ORDER BY id")
	if err != nil {
		t.Fatalf("unbounded query: %v", err)
	}
	var gotCount int
	for allRows.Next() {
		var id int64
		if err := allRows.Scan(&id); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		gotCount++
	}
	if err := allRows.Err(); err != nil {
		t.Fatalf("allRows.Err: %v", err)
	}
	_ = allRows.Close()
	if gotCount != 10 {
		t.Fatalf("unbounded row count = %d, want 10", gotCount)
	}

	// MaxRows=3 override without scan-limit failure truncates to 3 rows.
	maxRowsDB, err := eng.OpenDB("/LIM/DB", WithSchema("S"), WithResourceLimits(&ResourceLimits{MaxRows: 3}))
	if err != nil {
		t.Fatalf("OpenDB MaxRows: %v", err)
	}
	defer maxRowsDB.Close()
	r3, err := maxRowsDB.QueryContext(ctx, "SELECT id FROM item ORDER BY id")
	if err != nil {
		t.Fatalf("MaxRows query: %v", err)
	}
	var n3 int
	for r3.Next() {
		var id int64
		if err := r3.Scan(&id); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		n3++
	}
	if err := r3.Err(); err != nil {
		t.Fatalf("r3.Err: %v", err)
	}
	_ = r3.Close()
	if n3 != 3 {
		t.Fatalf("MaxRows=3 returned %d rows, want 3", n3)
	}

	// MaxResultBytes=10 override fails with 54F01.
	bytesDB, err := eng.OpenDB("/LIM/DB", WithSchema("S"), WithResourceLimits(&ResourceLimits{MaxResultBytes: 10}))
	if err != nil {
		t.Fatalf("OpenDB MaxResultBytes: %v", err)
	}
	defer bytesDB.Close()
	rb, err := bytesDB.QueryContext(ctx, "SELECT id, payload FROM item ORDER BY id")
	if err == nil {
		for rb.Next() {
		}
		err = rb.Err()
		_ = rb.Close()
	}
	if !errors.As(err, &ae) || ae.Code != api.ErrCodeExecutionLimitReached {
		t.Errorf("MaxResultBytes=10 err = %v, want 54F01", err)
	}
}

func TestEngine_HooksSurviveResetSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var (
		mu        sync.Mutex
		planInfos []PlanInfo
		execStats []ExecStats
	)

	eng, err := NewEngine(EngineConfig{
		Domains: []string{"HOOKS"},
		PlanHook: func(_ context.Context, info PlanInfo) {
			mu.Lock()
			planInfos = append(planInfos, info)
			mu.Unlock()
		},
		ExecStatsHook: func(_ context.Context, stats ExecStats) {
			mu.Lock()
			execStats = append(execStats, stats)
			mu.Unlock()
		},
		SlowQueryThreshold: 1 * time.Microsecond,
	}, WithDatabase(newEngineSimDB(1007)))
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()

	db := setupEngineSchema(t, eng, "/HOOKS/DB", "S",
		"CREATE TABLE item (id BIGINT, PRIMARY KEY (id))")
	// Force single pooled connection so database/sql invokes ResetSession between every call.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if _, err := db.ExecContext(ctx, "INSERT INTO item (id) VALUES (1)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	var id int64
	if err := db.QueryRowContext(ctx, "SELECT id FROM item WHERE id = 1").Scan(&id); err != nil || id != 1 {
		t.Fatalf("SELECT 1 = %d, %v", id, err)
	}
	// Second SELECT reuses the same pooled connection after ResetSession and hits the plan cache.
	if err := db.QueryRowContext(ctx, "SELECT id FROM item WHERE id = 1").Scan(&id); err != nil || id != 1 {
		t.Fatalf("SELECT 2 = %d, %v", id, err)
	}

	// Temporarily override via Conn.Raw for one borrow, then return connection (ResetSession).
	rawConn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	if err := rawConn.Raw(func(dc any) error {
		ec := dc.(*embedded.EmbeddedConnection)
		ec.SetPlanLogger(nil)
		ec.SetExecutionStatsLogger(nil)
		return nil
	}); err != nil {
		t.Fatalf("Conn.Raw: %v", err)
	}
	if err := rawConn.Close(); err != nil {
		t.Fatalf("rawConn.Close: %v", err)
	}

	// Query after ResetSession must still invoke Engine hooks!
	if err := db.QueryRowContext(ctx, "SELECT id FROM item WHERE id = 1").Scan(&id); err != nil || id != 1 {
		t.Fatalf("SELECT 3 after ResetSession = %d, %v", id, err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(planInfos) != 4 {
		t.Fatalf("PlanHook called %d times, want 4 (INSERT + miss + hit + post-ResetSession hit)", len(planInfos))
	}
	if planInfos[1].Cache != PlanCacheMiss {
		t.Errorf("planInfos[1].Cache = %v, want PlanCacheMiss", planInfos[1].Cache)
	}
	if planInfos[2].Cache != PlanCacheHit {
		t.Errorf("planInfos[2].Cache = %v, want PlanCacheHit", planInfos[2].Cache)
	}
	if planInfos[3].Cache != PlanCacheHit {
		t.Errorf("planInfos[3].Cache = %v, want PlanCacheHit", planInfos[3].Cache)
	}
	if len(execStats) != 4 {
		t.Fatalf("ExecStatsHook called %d times, want 4 (INSERT + 3 SELECTs)", len(execStats))
	}
	if execStats[0].RowsAffected != 1 {
		t.Errorf("INSERT RowsAffected = %d, want 1", execStats[0].RowsAffected)
	}
	if execStats[3].RowsReturned != 1 {
		t.Errorf("post-ResetSession SELECT RowsReturned = %d, want 1", execStats[3].RowsReturned)
	}

	// Verify per-connection hook override works independently.
	var overrideCalled atomic.Int32
	dbOverride, err := eng.OpenDB(
		"/HOOKS/DB",
		WithSchema("S"),
		WithPlanHook(func(context.Context, PlanInfo) { overrideCalled.Add(1) }),
		WithExecStatsHook(nil),
	)
	if err != nil {
		t.Fatalf("OpenDB override: %v", err)
	}
	defer dbOverride.Close()
	if err := dbOverride.QueryRowContext(ctx, "SELECT id FROM item WHERE id = 1").Scan(&id); err != nil {
		t.Fatalf("override SELECT: %v", err)
	}
	if overrideCalled.Load() != 1 {
		t.Errorf("override PlanHook count = %d, want 1", overrideCalled.Load())
	}
}

func TestEngine_ConnOptionsAndValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	eng, err := NewEngine(EngineConfig{
		Domains: []string{"OPTS"},
	}, WithDatabase(newEngineSimDB(1008)))
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()

	var ae *api.Error
	for _, badPath := range []string{"", "/", "OPTS/DB"} {
		if _, err := eng.Connector(badPath); !errors.As(err, &ae) || ae.Code != api.ErrCodeInvalidPath {
			t.Errorf("Connector(%q) = %v, want ErrCodeInvalidPath", badPath, err)
		}
	}
	if _, err := eng.OpenDB("/OPTS/DB", WithTransactionTags("tag_longer_than_16_characters")); !errors.As(err, &ae) || ae.Code != api.ErrCodeInvalidParameter {
		t.Errorf("long transaction tag = %v, want ErrCodeInvalidParameter", err)
	}

	rwDB := setupEngineSchema(t, eng, "/OPTS/DB", "S",
		"CREATE TABLE item (id BIGINT, PRIMARY KEY (id))")
	if _, err := rwDB.ExecContext(ctx, "INSERT INTO item (id) VALUES (10)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// ReadOnly blocks writes (0A000) and allows reads.
	roDB, err := eng.OpenDB("/OPTS/DB", WithSchema("S"), WithReadOnly(true))
	if err != nil {
		t.Fatalf("OpenDB ReadOnly: %v", err)
	}
	defer roDB.Close()
	if _, err := roDB.ExecContext(ctx, "INSERT INTO item (id) VALUES (11)"); !errors.As(err, &ae) || ae.Code != api.ErrCodeUnsupportedOperation {
		t.Errorf("ReadOnly INSERT = %v, want 0A000", err)
	}
	var id int64
	if err := roDB.QueryRowContext(ctx, "SELECT id FROM item").Scan(&id); err != nil || id != 10 {
		t.Fatalf("ReadOnly SELECT = %d, %v", id, err)
	}

	// RestrictDDLToSessionDatabase blocks cross-database DDL (42501).
	scopedDB, err := eng.OpenDB("/OPTS/DB", WithSchema("S"), WithRestrictDDLToSessionDatabase(true))
	if err != nil {
		t.Fatalf("OpenDB RestrictDDL: %v", err)
	}
	defer scopedDB.Close()
	if _, err := scopedDB.ExecContext(ctx, "CREATE DATABASE /OPTS/OTHER"); !errors.As(err, &ae) || ae.Code != api.ErrCodeInsufficientPrivilege {
		t.Errorf("scoped CREATE DATABASE /OPTS/OTHER = %v, want 42501", err)
	}

	// DryRun previews DML without persisting.
	dryDB, err := eng.OpenDB("/OPTS/DB", WithConnConfig(ConnConfig{
		Schema: "S",
		DryRun: true,
	}))
	if err != nil {
		t.Fatalf("OpenDB DryRun: %v", err)
	}
	defer dryDB.Close()
	if _, err := dryDB.ExecContext(ctx, "INSERT INTO item (id) VALUES (99)"); err != nil {
		t.Fatalf("DryRun INSERT: %v", err)
	}
	var count int64
	if err := rwDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM item").Scan(&count); err != nil || count != 1 {
		t.Fatalf("count after DryRun = %d (err %v), want 1", count, err)
	}
}

func TestEngine_DisableStoreTimer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	simDB := newEngineSimDB(1009)
	eng, err := NewEngine(EngineConfig{
		Domains:           []string{"NOTIMER"},
		DisableStoreTimer: true,
	}, WithDatabase(simDB))
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()

	if eng.StoreTimer() != nil {
		t.Fatalf("StoreTimer() = %v, want nil", eng.StoreTimer())
	}
	if simDB.Timer() != nil {
		t.Fatalf("simDB.Timer() = %v, want nil", simDB.Timer())
	}
	if got := eng.Metrics().StoreTimer; got != nil {
		t.Fatalf("Metrics().StoreTimer = %v, want nil", got)
	}

	db := setupEngineSchema(t, eng, "/NOTIMER/DB", "S",
		"CREATE TABLE item (id BIGINT, PRIMARY KEY (id))")
	if _, err := db.ExecContext(ctx, "INSERT INTO item (id) VALUES (1)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if simDB.Timer() != nil {
		t.Fatalf("simDB.Timer() after SQL exec = %v, want nil", simDB.Timer())
	}

	var ae *api.Error
	if _, err := NewEngine(EngineConfig{
		StoreTimer:        recordlayer.NewStoreTimer(),
		DisableStoreTimer: true,
	}, WithDatabase(newEngineSimDB(1010))); !errors.As(err, &ae) || ae.Code != api.ErrCodeInvalidParameter {
		t.Errorf("StoreTimer + DisableStoreTimer = %v, want ErrCodeInvalidParameter", err)
	}
}
