package sqldriver

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdbmetrics"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

// This file is a COMPILE pin for the operator-facing code in docs/mt-saas.md.
//
// That page hands operators code to paste, and its §3 mechanism is the whole
// reason the page exists: none of the five per-statement governors is settable
// from a DSN or from SQL, so `conn.Raw` down to *embedded.EmbeddedConnection is
// the ONLY route that arms them. A page that documents the only route to a
// quota is worse than no page if that route stops compiling — the operator
// reads it, believes the fleet is bounded, and ships unbounded.
//
// The snippets are reproduced verbatim rather than paraphrased, because the
// failure this guards is a rename or signature change in the setters, the
// option names, or the PlanGenerationLogger interface. A paraphrase that
// happens to avoid the renamed symbol would compile while the doc is wrong.
//
// Nothing here connects to FDB; the test body only asserts the wiring exists.
// Compilation IS the assertion.

// configureTenantConn is docs/mt-saas.md §3, "Setting them: not a DSN, not SQL
// — conn.Raw". The type assertion is load-bearing: Connector.Connect returns
// exactly this concrete type, which is what makes the documented cast safe.
func configureTenantConn(driverConn any) error {
	ec, ok := driverConn.(*embedded.EmbeddedConnection)
	if !ok {
		return fmt.Errorf("not an embedded connection: %T", driverConn)
	}
	ec.SetOptions(api.NewOptionsBuilder().
		Set(api.OptMaxRows, int64(10_000)).                    // egress cap
		Set(api.OptExecutionScannedRowsLimit, int64(500_000)). // per page
		Set(api.OptExecutionScannedBytesLimit, int64(64<<20)). // per page
		Set(api.OptExecutionTimeLimit, int64(2_000)).          // millis, narrows the 4s cap
		Set(api.OptMaxStatementMemoryBytes, int64(256<<20)).   // statement-wide
		Build())
	ec.SetFailOnScanLimitReached(true) // without this the scan limits only paginate
	ec.SetStatementTimeout(30 * time.Second)
	ec.SetMaxResultBytes(32 << 20)
	return nil
}

// useTenantConn is the acquire/configure/use/close shape §3 tells operators to
// route tenant work through, because configureTenantConn arms ONE pooled
// connection and the pool may hand out an unconfigured one later.
func useTenantConn(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Raw(configureTenantConn)
}

// tenantPlanLogger is docs/mt-saas.md §4, "Per-tenant plan logging". The
// package ships no func adapter, so the doc tells operators to declare a type;
// this pins that the one-method interface still has this exact signature and
// that PlanGenerationInfo still carries the fields the snippet reads.
type tenantPlanLogger struct{ tenantID string }

func (l tenantPlanLogger) LogPlanGeneration(ctx context.Context, info embedded.PlanGenerationInfo) {
	slog.InfoContext(ctx, "plan",
		"tenant", l.tenantID, // yours; the engine carries no tenant identity
		"sql", info.SQL,
		"plan_hash", info.PlanHash,
		"cache", info.Cache,
		"planning", info.PlanningDuration,
		"slow", info.SlowQuery)
}

var _ embedded.PlanGenerationLogger = tenantPlanLogger{}

func installPlanLogger(conn *sql.Conn, tenantID string) error {
	return conn.Raw(func(driverConn any) error {
		ec := driverConn.(*embedded.EmbeddedConnection)
		ec.SetSlowQueryThresholdMicros(500_000)
		ec.SetPlanLogger(tenantPlanLogger{tenantID: tenantID})
		return nil
	})
}

// retainClientDatabaseForMetrics is docs/mt-saas.md §4, "Retain the
// *client.Database handle at startup, or you get no metrics" — the handle must
// be captured on the way UP, because there is no accessor back down the stack.
func retainClientDatabaseForMetrics(ctx context.Context, clusterFile string, mux *http.ServeMux) (*recordlayer.FDBDatabase, error) {
	cdb, err := client.OpenDatabase(ctx, clusterFile) // *client.Database — keep this
	if err != nil {
		return nil, err
	}
	mux.Handle("/metrics", fdbmetrics.Handler(cdb)) // Prometheus text exposition
	return recordlayer.NewFDBDatabase(fdb.WrapDatabase(cdb)), nil
}

// openTenantDB is the §2 DSN (with the security flag) plus the §3 pool caps.
func openTenantDB(tenantID string) (*sql.DB, error) {
	db, err := sql.Open("fdbsql",
		"fdbsql:///t/"+tenantID+
			"?cluster_file=/etc/foundationdb/fdb.cluster"+
			"&schema=MAIN"+
			"&restrict_ddl_to_session_database=true")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8) // the per-tenant concurrency cap
	db.SetMaxIdleConns(2)
	return db, nil
}

func TestMTSaaSDocSnippetsCompile(t *testing.T) {
	t.Parallel()

	// Referencing every snippet keeps them reachable, so a signature change
	// surfaces as a build failure on this file rather than as silently dead
	// code the compiler stops type-checking against the doc.
	_ = configureTenantConn
	_ = useTenantConn
	_ = installPlanLogger
	_ = retainClientDatabaseForMetrics
	_ = openTenantDB

	// The §2 option-set form the doc offers as the embedded.New alternative to
	// the DSN parameter.
	if opts := api.NoOptions().With(api.OptRestrictDDLToSessionDatabase, true); opts == nil {
		t.Fatal("RESTRICT_DDL_TO_SESSION_DATABASE option set must build")
	}

	// The doc's §3 table asserts these are the five per-statement governors and
	// that four of them read back effectively unlimited while the memory cap is
	// absent from the defaults map. Pin the names AND the default-map behaviour:
	// the table is what an operator sizes a fleet against.
	defaults := api.DefaultOptionValues()
	for _, tc := range []struct {
		name api.OptionName
		want any
	}{
		{api.OptMaxRows, int(1<<31 - 1)},
		{api.OptExecutionScannedRowsLimit, int(1<<31 - 1)},
		{api.OptExecutionScannedBytesLimit, int64(1<<63 - 1)},
		{api.OptExecutionTimeLimit, int64(0)},
	} {
		got, ok := defaults[tc.name]
		if !ok {
			t.Errorf("%s absent from the defaults map; docs/mt-saas.md §3 documents its default", tc.name)
			continue
		}
		if got != tc.want {
			t.Errorf("%s default = %v (%T), want %v (%T) — docs/mt-saas.md §3's table is sized against this",
				tc.name, got, got, tc.want, tc.want)
		}
	}
	if _, ok := defaults[api.OptMaxStatementMemoryBytes]; ok {
		t.Errorf("%s is now IN the defaults map; docs/mt-saas.md §3 documents it as absent (reads back 0)",
			api.OptMaxStatementMemoryBytes)
	}
	// The doc's "reads back 0 via the optInt64 fallback" claim, which is what
	// makes the memory cap default to unlimited.
	if v := api.NoOptions().Get(api.OptMaxStatementMemoryBytes); v != nil {
		t.Errorf("%s reads back %v, want nil (docs/mt-saas.md §3: absent → 0)",
			api.OptMaxStatementMemoryBytes, v)
	}
}
