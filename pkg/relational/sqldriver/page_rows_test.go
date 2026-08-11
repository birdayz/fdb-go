package sqldriver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync/atomic"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/relational/api"
)

var (
	_ Queryer = (*sql.DB)(nil)
	_ Queryer = (*sql.Conn)(nil)
	_ Queryer = (*sql.Tx)(nil)
)

type rfc232Continuation struct {
	serialized []byte
	state      []byte
	reason     api.ContinuationReason
}

func (c *rfc232Continuation) Serialize() []byte              { return c.serialized }
func (c *rfc232Continuation) ExecutionState() []byte         { return c.state }
func (c *rfc232Continuation) Reason() api.ContinuationReason { return c.reason }

func rfc232Boundary(t *testing.T, state []byte, reason api.ContinuationReason) *api.ContinuationAvailableError {
	t.Helper()
	var protoReason gen.ContinuationProto_Reason
	switch reason {
	case api.ContinuationQueryExecutionLimitReached:
		protoReason = gen.ContinuationProto_QUERY_EXECUTION_LIMIT_REACHED
	case api.ContinuationTransactionLimitReached:
		protoReason = gen.ContinuationProto_TRANSACTION_LIMIT_REACHED
	default:
		t.Fatalf("unsupported boundary reason %v", reason)
	}
	wire := &gen.ContinuationProto{
		Version:        proto.Int32(1),
		ExecutionState: append([]byte{}, state...),
		Reason:         protoReason.Enum(),
	}
	serialized, err := (proto.MarshalOptions{Deterministic: true}).Marshal(wire)
	if err != nil {
		t.Fatalf("marshal boundary: %v", err)
	}
	boundary, err := api.NewContinuationAvailableError(&rfc232Continuation{
		serialized: serialized,
		state:      state,
		reason:     reason,
	})
	if err != nil {
		t.Fatalf("NewContinuationAvailableError: %v", err)
	}
	return boundary
}

type rfc232Rows struct {
	rows      [][]driver.Value
	position  int
	terminal  error
	closeCall atomic.Int32
}

func (r *rfc232Rows) Columns() []string { return []string{"ID"} }

func (r *rfc232Rows) Close() error {
	r.closeCall.Add(1)
	return nil
}

func (r *rfc232Rows) Next(dest []driver.Value) error {
	if r.position < len(r.rows) {
		copy(dest, r.rows[r.position])
		r.position++
		return nil
	}
	if r.terminal != nil {
		return r.terminal
	}
	return io.EOF
}

func (*rfc232Rows) ColumnTypeDatabaseTypeName(int) string { return "BIGINT" }
func (*rfc232Rows) ColumnTypeScanType(int) reflect.Type   { return reflect.TypeFor[int64]() }
func (*rfc232Rows) ColumnTypeNullable(int) (bool, bool)   { return false, true }

type rfc232Connector struct {
	factory     func(queryNumber int64) driver.Rows
	connections atomic.Int64
	queries     atomic.Int64
	closes      atomic.Int64
}

func (c *rfc232Connector) Connect(context.Context) (driver.Conn, error) {
	c.connections.Add(1)
	return &rfc232Conn{connector: c}, nil
}

func (c *rfc232Connector) Driver() driver.Driver { return &rfc232Driver{connector: c} }

type rfc232Driver struct{ connector *rfc232Connector }

func (d *rfc232Driver) Open(string) (driver.Conn, error) {
	return d.connector.Connect(context.Background())
}

type rfc232Conn struct{ connector *rfc232Connector }

func (*rfc232Conn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported by the RFC-232 fake driver")
}

func (c *rfc232Conn) Close() error {
	c.connector.closes.Add(1)
	return nil
}

func (*rfc232Conn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not supported by the RFC-232 fake driver")
}

func (c *rfc232Conn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	queryNumber := c.connector.queries.Add(1)
	return c.connector.factory(queryNumber), nil
}

func openRFC232DB(t *testing.T, factory func(queryNumber int64) driver.Rows) (*sql.DB, *rfc232Connector) {
	t.Helper()
	connector := &rfc232Connector{factory: factory}
	db := sql.OpenDB(connector)
	t.Cleanup(func() { _ = db.Close() })
	return db, connector
}

func TestDatabaseSQLRetainsContinuationBoundaryAfterImplicitClose(t *testing.T) {
	boundary := rfc232Boundary(t, []byte{1, 2, 3}, api.ContinuationQueryExecutionLimitReached)
	var produced []*rfc232Rows
	db, connector := openRFC232DB(t, func(int64) driver.Rows {
		rows := &rfc232Rows{rows: [][]driver.Value{{int64(42)}}, terminal: boundary}
		produced = append(produced, rows)
		return rows
	})
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	readPage := func() {
		rows, err := db.QueryContext(context.Background(), "SELECT ID")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		if !rows.Next() {
			t.Fatalf("first Next = false: %v", rows.Err())
		}
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if id != 42 {
			t.Fatalf("id = %d, want 42", id)
		}
		if rows.Next() {
			t.Fatal("terminating Next = true")
		}
		err = rows.Err()
		var gotBoundary *api.ContinuationAvailableError
		if !errors.As(err, &gotBoundary) {
			t.Fatalf("Rows.Err = %T %v, want ContinuationAvailableError", err, err)
		}
		if errors.Is(err, driver.ErrBadConn) {
			t.Fatalf("boundary unexpectedly matches driver.ErrBadConn: %v", err)
		}
	}

	readPage()
	readPage()
	if got := connector.connections.Load(); got != 1 {
		t.Fatalf("physical connections = %d, want 1; boundary discarded a usable connection", got)
	}
	if len(produced) != 2 {
		t.Fatalf("produced rows = %d, want 2", len(produced))
	}
	for i, rows := range produced {
		if got := rows.closeCall.Load(); got != 1 {
			t.Fatalf("rows %d Close calls = %d, want implicit close exactly once", i, got)
		}
	}
}

func TestPageRowsBoundaryStateMachine(t *testing.T) {
	boundary := rfc232Boundary(t, []byte{4, 5, 6}, api.ContinuationTransactionLimitReached)
	var driverRows *rfc232Rows
	db, _ := openRFC232DB(t, func(int64) driver.Rows {
		driverRows = &rfc232Rows{rows: [][]driver.Value{{int64(7)}}, terminal: boundary}
		return driverRows
	})

	rows, err := QueryPage(context.Background(), db, "SELECT ID")
	if err != nil {
		t.Fatalf("QueryPage: %v", err)
	}
	assertContinuationUnavailable(t, rows)

	columns, err := rows.Columns()
	if err != nil || !reflect.DeepEqual(columns, []string{"ID"}) {
		t.Fatalf("Columns = %v, %v", columns, err)
	}
	columnTypes, err := rows.ColumnTypes()
	if err != nil {
		t.Fatalf("ColumnTypes: %v", err)
	}
	if len(columnTypes) != 1 || columnTypes[0].DatabaseTypeName() != "BIGINT" ||
		columnTypes[0].ScanType() != reflect.TypeFor[int64]() {
		t.Fatalf("ColumnTypes = %#v", columnTypes)
	}

	if !rows.Next() {
		t.Fatalf("first Next = false: %v", rows.Err())
	}
	var id int64
	if err := rows.Scan(&id); err != nil || id != 7 {
		t.Fatalf("Scan = id %d, err %v", id, err)
	}
	if rows.Next() {
		t.Fatal("terminating Next = true")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("PageRows.Err = %v, want nil boundary", err)
	}
	continuation, err := rows.Continuation()
	if err != nil {
		t.Fatalf("Continuation: %v", err)
	}
	if api.AtEnd(continuation) {
		t.Fatal("boundary continuation is END")
	}
	if got := continuation.ExecutionState(); !reflect.DeepEqual(got, []byte{4, 5, 6}) {
		t.Fatalf("continuation state = %v", got)
	}
	reason, err := rows.Reason()
	if err != nil || reason != api.ContinuationTransactionLimitReached {
		t.Fatalf("Reason = %v, %v", reason, err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := rows.Continuation(); err != nil {
		t.Fatalf("Continuation after terminal Close: %v", err)
	}
	if got := driverRows.closeCall.Load(); got != 1 {
		t.Fatalf("driver Rows.Close calls = %d, want 1", got)
	}
}

func TestPageRowsNaturalEOFCreatesEndContinuation(t *testing.T) {
	db, _ := openRFC232DB(t, func(int64) driver.Rows { return &rfc232Rows{} })
	rows, err := QueryPage(context.Background(), db, "SELECT ID")
	if err != nil {
		t.Fatalf("QueryPage: %v", err)
	}
	if rows.Next() {
		t.Fatal("empty result Next = true")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("Err = %v", err)
	}
	continuation, err := rows.Continuation()
	if err != nil {
		t.Fatalf("Continuation: %v", err)
	}
	if !api.AtEnd(continuation) || continuation.ExecutionState() == nil {
		t.Fatalf("natural EOF continuation state = %#v, want present-empty END", continuation.ExecutionState())
	}
	reason, err := rows.Reason()
	if err != nil || reason != api.ContinuationCursorAfterLast {
		t.Fatalf("Reason = %v, %v", reason, err)
	}
}

func TestPageRowsEarlyCloseHasNoContinuation(t *testing.T) {
	var driverRows *rfc232Rows
	db, _ := openRFC232DB(t, func(int64) driver.Rows {
		driverRows = &rfc232Rows{rows: [][]driver.Value{{int64(1)}, {int64(2)}}}
		return driverRows
	})
	rows, err := QueryPage(context.Background(), db, "SELECT ID")
	if err != nil {
		t.Fatalf("QueryPage: %v", err)
	}
	if !rows.Next() {
		t.Fatalf("first Next = false: %v", rows.Err())
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	assertContinuationUnavailable(t, rows)
	if got := driverRows.closeCall.Load(); got != 1 {
		t.Fatalf("driver Rows.Close calls = %d, want 1", got)
	}
}

func TestPageRowsPreservesOrdinaryAndInvalidBoundaryErrors(t *testing.T) {
	ordinary := errors.New("execution failed")
	boundary := rfc232Boundary(t, []byte{9}, api.ContinuationQueryExecutionLimitReached)
	tests := []struct {
		name   string
		err    error
		wantIs error
	}{
		{name: "ordinary", err: ordinary, wantIs: ordinary},
		{name: "zero boundary", err: &api.ContinuationAvailableError{}},
		{name: "wrapped boundary", err: fmt.Errorf("execution failed: %w", boundary), wantIs: boundary},
		{name: "boundary joined with cancellation", err: errors.Join(boundary, context.Canceled), wantIs: context.Canceled},
		{name: "boundary joined with bad connection", err: errors.Join(boundary, driver.ErrBadConn), wantIs: driver.ErrBadConn},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, _ := openRFC232DB(t, func(int64) driver.Rows {
				return &rfc232Rows{terminal: test.err}
			})
			rows, err := QueryPage(context.Background(), db, "SELECT ID")
			if err != nil {
				t.Fatalf("QueryPage: %v", err)
			}
			if rows.Next() {
				t.Fatal("Next = true")
			}
			if got := rows.Err(); !errors.Is(got, test.err) {
				t.Fatalf("Err = %T %v, want %T %v", got, got, test.err, test.err)
			}
			if test.wantIs != nil && !errors.Is(rows.Err(), test.wantIs) {
				t.Fatalf("Err = %v, want errors.Is(..., %v)", rows.Err(), test.wantIs)
			}
			if _, err := rows.Continuation(); !errors.Is(err, test.err) {
				t.Fatalf("Continuation error = %v, want %v", err, test.err)
			}
		})
	}
}

func TestPageRowsContinuationsAreResultScoped(t *testing.T) {
	db, _ := openRFC232DB(t, func(queryNumber int64) driver.Rows {
		return &rfc232Rows{terminal: rfc232Boundary(
			t,
			[]byte{byte(queryNumber)},
			api.ContinuationQueryExecutionLimitReached,
		)}
	})
	first, err := QueryPage(context.Background(), db, "SELECT ID")
	if err != nil {
		t.Fatalf("first QueryPage: %v", err)
	}
	second, err := QueryPage(context.Background(), db, "SELECT ID")
	if err != nil {
		t.Fatalf("second QueryPage: %v", err)
	}
	if first.Next() || second.Next() {
		t.Fatal("empty boundary page returned a row")
	}
	firstContinuation, err := first.Continuation()
	if err != nil {
		t.Fatalf("first Continuation: %v", err)
	}
	secondContinuation, err := second.Continuation()
	if err != nil {
		t.Fatalf("second Continuation: %v", err)
	}
	if reflect.DeepEqual(firstContinuation.ExecutionState(), secondContinuation.ExecutionState()) {
		t.Fatalf("concurrent results exchanged/collapsed continuations: first=%v second=%v",
			firstContinuation.ExecutionState(), secondContinuation.ExecutionState())
	}
}

func TestQueryPageRejectsNilQueryer(t *testing.T) {
	rows, err := QueryPage(context.Background(), nil, "SELECT ID")
	if rows != nil || err == nil {
		t.Fatalf("QueryPage(nil) = (%v, %v), want nil, error", rows, err)
	}
	if apiErr := api.AsError(err); apiErr == nil || apiErr.Code != api.ErrCodeInvalidParameter {
		t.Fatalf("QueryPage(nil) error = %T %v, want 22023", err, err)
	}
}

func assertContinuationUnavailable(t *testing.T, rows *PageRows) {
	t.Helper()
	if continuation, err := rows.Continuation(); continuation != nil || !isContinuationPrecondition(err) {
		t.Fatalf("Continuation = (%v, %v), want nil, exhausted-result precondition", continuation, err)
	}
	if _, err := rows.Reason(); !isContinuationPrecondition(err) {
		t.Fatalf("Reason error = %v, want exhausted-result precondition", err)
	}
}

func isContinuationPrecondition(err error) bool {
	apiErr := api.AsError(err)
	return apiErr != nil && apiErr.Code == api.ErrCodeUnsupportedOperation &&
		apiErr.Message == continuationExhaustionMessage
}
