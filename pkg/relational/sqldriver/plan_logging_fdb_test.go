package sqldriver_test

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/embedded"
)

// syncCaptureLogger is a concurrency-safe PlanGenerationLogger for tests.
type syncCaptureLogger struct {
	mu     sync.Mutex
	events []embedded.PlanGenerationInfo
}

func (l *syncCaptureLogger) LogPlanGeneration(_ context.Context, info embedded.PlanGenerationInfo) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, info)
}

func (l *syncCaptureLogger) snapshot() []embedded.PlanGenerationInfo {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]embedded.PlanGenerationInfo, len(l.events))
	copy(out, l.events)
	return out
}

// installLogger pins a single *sql.Conn and installs a planning-metrics
// logger on its underlying EmbeddedConnection via Raw. The returned *sql.Conn
// must be used for all subsequent statements so the logger-equipped
// connection is the one that plans them.
func installLogger(t *testing.T, db *sql.DB, logger embedded.PlanGenerationLogger) *sql.Conn {
	t.Helper()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("pin conn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.Raw(func(driverConn any) error {
		ec, ok := driverConn.(*embedded.EmbeddedConnection)
		if !ok {
			t.Fatalf("driver conn is %T, want *embedded.EmbeddedConnection", driverConn)
		}
		ec.SetPlanLogger(logger)
		return nil
	}); err != nil {
		t.Fatalf("Raw: %v", err)
	}
	return conn
}

// TestFDB_PlanLogging_DML proves the planDML funnel emits a planning-metrics
// event with Cache==Skip (DML is never cached) and a valid plan hash.
//
// Reachability note: planDML (the Cascades DML planning step) is only entered
// via QueryContext (ExecContext sets execMode and routes DML through the
// non-Cascades execStatement path). QueryContext plans the DELETE — which
// fires the metrics hook — then rejects the resulting update plan with "only
// SHOW and SELECT statements are supported" (connection.go:359), since
// QueryContext returns rows, not row counts. The planning event is emitted
// before that rejection, so the rejection is the expected outcome here and we
// assert the captured event rather than a successful execution.
func TestFDB_PlanLogging_DML(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	logger := &syncCaptureLogger{}
	conn := installLogger(t, cascadesDB, logger)

	rows, err := conn.QueryContext(ctx, "DELETE FROM Item WHERE item_id = 2")
	if err == nil {
		rows.Close()
	} else if !strings.Contains(err.Error(), "only SHOW and SELECT") {
		t.Fatalf("DELETE: unexpected error: %v", err)
	}

	events := logger.snapshot()
	if len(events) != 1 {
		t.Fatalf("want 1 planning event for DML, got %d", len(events))
	}
	ev := events[0]
	if ev.Err != nil {
		t.Errorf("unexpected error in planning event: %v", ev.Err)
	}
	if ev.Cache != embedded.PlanCacheSkip {
		t.Errorf("DML cache event = %v, want skip", ev.Cache)
	}
	if ev.PlanHash == 0 {
		t.Errorf("DML plan hash should be non-zero")
	}
	if ev.PlanExplain == "" {
		t.Errorf("DML plan explain should be non-empty")
	}
	if ev.PlanningDuration <= 0 {
		t.Errorf("planning duration should be positive, got %v", ev.PlanningDuration)
	}
}

// TestFDB_InsertValues_ThroughCascades proves INSERT … VALUES executes
// through the Cascades path (RFC-035): Exec routes to planDML, the literal
// rows become an array exploded into a RecordQueryInsertPlan, the affected
// rows are counted into RowsAffected, and the rows persist. The captured
// physical plan (Insert over Explode) proves the values-explode path fired
// rather than the naive execInsert fallback.
func TestFDB_InsertValues_ThroughCascades(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	logger := &syncCaptureLogger{}
	conn := installLogger(t, cascadesDB, logger)

	res, err := conn.ExecContext(ctx,
		"INSERT INTO Item VALUES (101, 'Sprocket', 11), (102, 'Cog', 22)")
	if err != nil {
		t.Fatalf("INSERT VALUES: %v", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("RowsAffected: %v", err)
	}
	if n != 2 {
		t.Fatalf("RowsAffected = %d, want 2", n)
	}

	// The DML plan went through planDML (one planning event, cache Skip),
	// and the physical plan is Insert over Explode — the values path.
	events := logger.snapshot()
	if len(events) != 1 {
		t.Fatalf("want 1 planning event, got %d", len(events))
	}
	if events[0].Cache != embedded.PlanCacheSkip {
		t.Errorf("DML cache = %v, want skip", events[0].Cache)
	}
	ex := events[0].PlanExplain
	if !strings.Contains(ex, "Insert(") || !strings.Contains(strings.ToLower(ex), "explode") {
		t.Fatalf("plan explain %q does not show Insert over Explode (values path)", ex)
	}

	// Rows persisted with correct values.
	got := map[int64]string{}
	rows, err := conn.QueryContext(ctx, "SELECT item_id, name FROM Item WHERE item_id >= 101")
	if err != nil {
		t.Fatalf("SELECT back: %v", err)
	}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[id] = name
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	rows.Close()
	if got[101] != "Sprocket" || got[102] != "Cog" {
		t.Fatalf("persisted rows = %v, want {101:Sprocket, 102:Cog}", got)
	}
}

// TestFDB_PlanLogging_SelectMissThenHit proves the planSelectCascades funnel
// emits miss-then-hit across two identical SELECTs on the same connection.
func TestFDB_PlanLogging_SelectMissThenHit(t *testing.T) {
	t.Parallel()
	_, cascadesDB := setupCascadesTestDB(t)
	ctx := context.Background()

	logger := &syncCaptureLogger{}
	conn := installLogger(t, cascadesDB, logger)

	const q = "SELECT name FROM Item WHERE item_id = 1"
	for i := 0; i < 2; i++ {
		rows, err := conn.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("SELECT %d: %v", i, err)
		}
		rows.Close()
	}

	events := logger.snapshot()
	if len(events) != 2 {
		t.Fatalf("want 2 planning events, got %d", len(events))
	}
	if events[0].Cache != embedded.PlanCacheMiss {
		t.Errorf("first event cache = %v, want miss", events[0].Cache)
	}
	if events[1].Cache != embedded.PlanCacheHit {
		t.Errorf("second event cache = %v, want hit", events[1].Cache)
	}
	if events[0].PlanHash == 0 || events[0].PlanHash != events[1].PlanHash {
		t.Errorf("plan hash mismatch: %d vs %d", events[0].PlanHash, events[1].PlanHash)
	}
}
