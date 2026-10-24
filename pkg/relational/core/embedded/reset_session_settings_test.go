package embedded

import (
	"context"
	"testing"
	"time"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/session"
)

type nopPlanLogger struct{}

func (nopPlanLogger) LogPlanGeneration(context.Context, PlanGenerationInfo) {}

type nopStatsLogger struct{}

func (nopStatsLogger) LogExecutionStats(context.Context, ExecutionStats) {}

// ResetSession hands the next borrower the connector's configuration: the
// option set SetOptions installed, and New's values for the six Go-local
// settings a borrower may change through Conn.Raw.
func TestResetSession_RestoresEveryPerBorrowSetting(t *testing.T) {
	t.Parallel()
	c := New("/FRL/DB", nil, nil, nil, nil)
	c.sess = &session.Session{Schema: "S", DefaultSchema: "S"}
	type settings struct {
		failOnScan  bool
		timeout     time.Duration
		maxBytes    int64
		slowMicros  int64
		planLogger  PlanGenerationLogger
		statsLogger ExecutionStatsLogger
	}
	snapshot := func() settings {
		return settings{
			c.failOnScanLimitReached, c.statementTimeout, c.maxResultBytes,
			c.slowQueryThresholdMicros, c.planLogger, c.execStatsLogger,
		}
	}
	fresh := snapshot()
	c.SetOptions(api.NoOptions().With(api.OptRestrictDDLToSessionDatabase, true))

	if err := c.SetOption(api.OptDryRun, true); err != nil {
		t.Fatal(err)
	}
	c.SetFailOnScanLimitReached(true)
	c.SetStatementTimeout(time.Second)
	c.SetMaxResultBytes(1 << 20)
	c.SetPlanLogger(nopPlanLogger{})
	c.SetExecutionStatsLogger(nopStatsLogger{})
	c.SetSlowQueryThresholdMicros(fresh.slowMicros + 1)
	if set := snapshot(); set.failOnScan == fresh.failOnScan || set.timeout == fresh.timeout ||
		set.maxBytes == fresh.maxBytes || set.slowMicros == fresh.slowMicros ||
		set.planLogger == fresh.planLogger || set.statsLogger == fresh.statsLogger {
		t.Fatalf("a setter left its setting at New's value: %+v", set)
	}

	if err := c.ResetSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fresh != (settings{slowMicros: defaultSlowQueryThresholdMicros()}) {
		t.Fatalf("New's settings %+v, want everything off and the default slow-query threshold", fresh)
	}
	if got := snapshot(); got != fresh {
		t.Errorf("after ResetSession the Go-local settings are %+v, want New's %+v", got, fresh)
	}
	if optBool(c.Options(), api.OptDryRun, false) {
		t.Error("DRY_RUN outlived the borrow")
	}
	if !optBool(c.Options(), api.OptRestrictDDLToSessionDatabase, false) {
		t.Error("ResetSession dropped the option set SetOptions installed")
	}
}
