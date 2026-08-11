package embedded

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
)

const explicitPageDisabledMessage = "unprotected plan continuations are disabled"

func requireExplicitPageDisabled(t *testing.T, err error) *api.Error {
	t.Helper()
	if err == nil {
		t.Fatal("expected explicit-page capability error, got nil")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("explicit-page error = %T (%v), want *api.Error", err, err)
	}
	if apiErr.Code != api.ErrCodeUnsupportedOperation {
		t.Fatalf("explicit-page SQLSTATE = %s, want %s: %v",
			apiErr.Code, api.ErrCodeUnsupportedOperation, err)
	}
	if apiErr.Message != explicitPageDisabledMessage {
		t.Fatalf("explicit-page message = %q, want %q",
			apiErr.Message, explicitPageDisabledMessage)
	}
	return apiErr
}

func drainEmbeddedRows(t *testing.T, rows driver.Rows) int {
	t.Helper()
	defer rows.Close() //nolint:errcheck
	dest := make([]driver.Value, len(rows.Columns()))
	count := 0
	for {
		err := rows.Next(dest)
		switch {
		case err == nil:
			count++
		case errors.Is(err, io.EOF):
			return count
		default:
			t.Fatalf("iterate embedded rows: %v", err)
		}
	}
}

func seedExplicitPageRows(t *testing.T, c *EmbeddedConnection, count int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < count; i++ {
		if _, err := c.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO t (id, v) VALUES (%d, %d)", i, i), nil); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}
}

type explicitPageStatsCapture struct {
	mu     sync.Mutex
	events []ExecutionStats
}

func (c *explicitPageStatsCapture) LogExecutionStats(_ context.Context, stats ExecutionStats) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, stats)
}

func (c *explicitPageStatsCapture) only(t *testing.T) ExecutionStats {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.events) != 1 {
		t.Fatalf("execution stats events = %d, want 1: %+v", len(c.events), c.events)
	}
	return c.events[0]
}

func TestContinuationReasonForNoNext(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		reason recordlayer.NoNextReason
		want   api.ContinuationReason
	}{
		{"source exhausted", recordlayer.SourceExhausted, api.ContinuationCursorAfterLast},
		{"returned-row limit", recordlayer.ReturnLimitReached, api.ContinuationQueryExecutionLimitReached},
		{"scanned-byte limit", recordlayer.ByteLimitReached, api.ContinuationTransactionLimitReached},
		{"execution-time limit", recordlayer.TimeLimitReached, api.ContinuationTransactionLimitReached},
		{"scanned-row limit", recordlayer.ScanLimitReached, api.ContinuationTransactionLimitReached},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := continuationReasonForNoNext(tc.reason)
			if err != nil {
				t.Fatalf("continuationReasonForNoNext(%v): %v", tc.reason, err)
			}
			if got != tc.want {
				t.Fatalf("continuationReasonForNoNext(%v) = %s, want %s",
					tc.reason, got, tc.want)
			}
		})
	}

	if _, err := continuationReasonForNoNext(recordlayer.NoNextReason(99)); err == nil {
		t.Fatal("unknown NoNextReason was accepted; continuation reason mapping must fail closed")
	} else {
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeInternalError {
			t.Fatalf("unknown NoNextReason error = %T (%v), want *api.Error XX000", err, err)
		}
	}
}

func TestExplicitPageModeDefensivelyNeverAutoFollows(t *testing.T) {
	t.Parallel()

	// conn is deliberately nil. If nextRow falls through to fetchPage, the test
	// panics; the explicit-page boundary must be decided first.
	rows := &paginatingRows{
		explicitPageMode: true,
		pageReason:       recordlayer.ScanLimitReached,
	}
	if _, err := rows.nextRow(); err == nil {
		t.Fatal("non-terminal explicit page entered the legacy fetch loop")
	} else {
		requireExplicitPageDisabled(t, err)
	}

	// The migration gate is read-only. DML keeps countAll's legacy drain until
	// the RFC-232 atomic-DML phase replaces it.
	dmlRows := &paginatingRows{
		explicitPageMode: true,
		isUpdate:         true,
		pageReason:       recordlayer.ScanLimitReached,
	}
	if err := dmlRows.nonTerminalExplicitPageError(); err != nil {
		t.Fatalf("DML was intercepted by the read-page migration gate: %v", err)
	}

	legacyRows := &paginatingRows{pageReason: recordlayer.ScanLimitReached}
	if err := legacyRows.nonTerminalExplicitPageError(); err != nil {
		t.Fatalf("legacy pagination was changed without opt-in: %v", err)
	}

	terminalRows := &paginatingRows{explicitPageMode: true, exhausted: true}
	if err := terminalRows.nonTerminalExplicitPageError(); err != nil {
		t.Fatalf("terminal explicit page failed: %v", err)
	}
}

func TestExplicitPageModeRejectsNonTerminalReadBeforeRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newSimConnection(t, 232001)
	seedExplicitPageRows(t, c, 6)
	c.SetOptions(api.NewOptionsBuilder().
		Set(api.OptExecutionScannedRowsLimit, 2).
		Build())
	c.SetExplicitPageMode(true)

	stats := &explicitPageStatsCapture{}
	c.SetExecutionStatsLogger(stats)
	rows, err := c.QueryContext(ctx, "SELECT id FROM t", nil)
	if rows != nil {
		_ = rows.Close()
		t.Fatalf("non-terminal explicit page published %T Rows before GO_V1 was available", rows)
	}
	requireExplicitPageDisabled(t, err)

	event := stats.only(t)
	if event.Pages != 1 {
		t.Fatalf("explicit-page execution fetched %d pages, want exactly 1", event.Pages)
	}
	if event.RowsReturned != 0 {
		t.Fatalf("RowsReturned = %d, want 0: the buffered page was not published", event.RowsReturned)
	}
	requireExplicitPageDisabled(t, event.Err)

	// Opt-in is the only behavior change in this migration slice. The same
	// connection and scan budget still transparently drain under legacy mode.
	c.SetExecutionStatsLogger(nil)
	c.SetExplicitPageMode(false)
	legacy, legacyErr := c.QueryContext(ctx, "SELECT id FROM t", nil)
	if legacyErr != nil {
		t.Fatalf("legacy query: %v", legacyErr)
	}
	if got := drainEmbeddedRows(t, legacy); got != 6 {
		t.Fatalf("legacy query returned %d rows, want 6", got)
	}
}

func TestExplicitPageModeReturnedRowBoundaryAndTerminalRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newSimConnection(t, 232002)
	seedExplicitPageRows(t, c, 5)
	c.SetOptions(api.NewOptionsBuilder().Set(api.OptMaxRows, 2).Build())
	c.SetExplicitPageMode(true)

	rows, err := c.QueryContext(ctx, "SELECT id FROM t", nil)
	if rows != nil {
		_ = rows.Close()
		t.Fatalf("MAX_ROWS boundary published %T Rows before GO_V1 was available", rows)
	}
	requireExplicitPageDisabled(t, err)

	// A semantic SQL LIMIT exhausts its envelope in the same cursor. It is a
	// terminal query, not a pagination boundary, and must remain usable while
	// explicit-page mode is enabled.
	terminal, terminalErr := c.QueryContext(ctx, "SELECT id FROM t LIMIT 1", nil)
	if terminalErr != nil {
		t.Fatalf("terminal explicit-page query: %v", terminalErr)
	}
	if got := drainEmbeddedRows(t, terminal); got != 1 {
		t.Fatalf("terminal LIMIT query returned %d rows, want 1", got)
	}
}

func TestExplicitPageModeDoesNotInterceptDMLOrPoisonExplicitTransaction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newSimConnection(t, 232003)
	seedExplicitPageRows(t, c, 6)
	c.SetOptions(api.NewOptionsBuilder().Set(api.OptMaxRows, 2).Build())
	c.SetExplicitPageMode(true)

	result, err := c.ExecContext(ctx, "UPDATE t SET v = v + 10", nil)
	if err != nil {
		t.Fatalf("explicit-page mode changed DML behavior: %v", err)
	}
	if affected, affectedErr := result.RowsAffected(); affectedErr != nil || affected != 6 {
		t.Fatalf("UPDATE RowsAffected = %d, %v; want 6, nil", affected, affectedErr)
	}

	txDriver, err := c.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	tx := txDriver.(*embeddedTx)
	defer tx.Rollback() //nolint:errcheck

	rows, queryErr := c.QueryContext(ctx, "SELECT id FROM t", nil)
	if rows != nil {
		_ = rows.Close()
		t.Fatalf("in-transaction non-terminal page published %T Rows", rows)
	}
	requireExplicitPageDisabled(t, queryErr)
	if c.activeTx != tx || tx.terminated.Load() {
		t.Fatal("explicit-page capability error ended the owning explicit transaction")
	}

	terminal, terminalErr := c.QueryContext(ctx, "SELECT id FROM t LIMIT 1", nil)
	if terminalErr != nil {
		t.Fatalf("transaction was unusable after explicit-page capability error: %v", terminalErr)
	}
	if got := drainEmbeddedRows(t, terminal); got != 1 {
		t.Fatalf("post-boundary transaction query returned %d rows, want 1", got)
	}
}
