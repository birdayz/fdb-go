package testkit

import (
	"context"
	"database/sql"
	"sync"
	"testing"

	"fdb.dev/pkg/relational/core/embedded"
)

// syncCaptureLogger is a concurrency-safe PlanGenerationLogger for tests.
type SyncCaptureLogger struct {
	mu     sync.Mutex
	events []embedded.PlanGenerationInfo
}

func (l *SyncCaptureLogger) LogPlanGeneration(_ context.Context, info embedded.PlanGenerationInfo) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, info)
}

func (l *SyncCaptureLogger) Snapshot() []embedded.PlanGenerationInfo {
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
func InstallLogger(t *testing.T, db *sql.DB, logger embedded.PlanGenerationLogger) *sql.Conn {
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
