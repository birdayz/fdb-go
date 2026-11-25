package api

// The Connection / Statement / PreparedStatement / ResultSet /
// ResultSetMetaData / Driver types in this package are interfaces.
// Concrete implementations live in pkg/relational/core (later phases).
// These tests exist to verify the interfaces compile, document the
// nominal method set, and pin down user-visible constant values.

import (
	"context"
	"net/url"
	"testing"
)

// ---- Column-nullable flag values are wire-compatible with JDBC ----

func TestColumnNullableConstantsMatchJDBC(t *testing.T) {
	t.Parallel()
	// java.sql.ResultSetMetaData defines these exact integer values;
	// our API must not diverge or result-set metadata exchanged with
	// Java becomes unreadable.
	if ColumnNoNulls != 0 {
		t.Errorf("ColumnNoNulls = %d, want 0", ColumnNoNulls)
	}
	if ColumnNullable != 1 {
		t.Errorf("ColumnNullable = %d, want 1", ColumnNullable)
	}
	if ColumnNullableUnknown != 2 {
		t.Errorf("ColumnNullableUnknown = %d, want 2", ColumnNullableUnknown)
	}
}

// ---- Compile-time interface assertions ----
//
// Tiny stub types implementing every method — if the interface grows
// a method that a future impl must provide, this file fails to build
// and prompts a review of the stub.

type driverStub struct{}

func (*driverStub) Connect(context.Context, *url.URL, *Options) (Connection, error) {
	return nil, nil
}
func (*driverStub) MajorVersion() int { return 0 }
func (*driverStub) MinorVersion() int { return 0 }

type connStub struct{}

func (*connStub) CreateStatement(context.Context) (Statement, error) { return nil, nil }
func (*connStub) PrepareStatement(context.Context, string) (PreparedStatement, error) {
	return nil, nil
}
func (*connStub) Options() *Options               { return nil }
func (*connStub) SetOption(OptionName, any) error { return nil }
func (*connStub) Path() *url.URL                  { return nil }
func (*connStub) SetAutoCommit(bool) error        { return nil }
func (*connStub) AutoCommit() bool                { return true }
func (*connStub) Commit() error                   { return nil }
func (*connStub) Rollback() error                 { return nil }
func (*connStub) SetSchema(string) error          { return nil }
func (*connStub) Schema() string                  { return "" }
func (*connStub) Close() error                    { return nil }
func (*connStub) IsClosed() bool                  { return false }

type stmtStub struct{}

func (*stmtStub) ExecuteQuery(context.Context, string) (ResultSet, error) { return nil, nil }
func (*stmtStub) ExecuteUpdate(context.Context, string) (int64, error)    { return 0, nil }
func (*stmtStub) Execute(context.Context, string) (bool, error)           { return false, nil }
func (*stmtStub) ResultSet() ResultSet                                    { return nil }
func (*stmtStub) UpdateCount() int64                                      { return 0 }
func (*stmtStub) Close() error                                            { return nil }
func (*stmtStub) IsClosed() bool                                          { return false }

type preparedStub struct{ stmtStub }

func (*preparedStub) SetObject(int, any) error                                { return nil }
func (*preparedStub) ClearParameters() error                                  { return nil }
func (*preparedStub) ExecuteQueryPrepared(context.Context) (ResultSet, error) { return nil, nil }
func (*preparedStub) ExecuteUpdatePrepared(context.Context) (int64, error)    { return 0, nil }

type rsStub struct{}

func (*rsStub) Next() bool                          { return false }
func (*rsStub) Err() error                          { return nil }
func (*rsStub) Close() error                        { return nil }
func (*rsStub) MetaData() ResultSetMetaData         { return nil }
func (*rsStub) Long(int) (int64, error)             { return 0, nil }
func (*rsStub) Float(int) (float32, error)          { return 0, nil }
func (*rsStub) Double(int) (float64, error)         { return 0, nil }
func (*rsStub) String(int) (string, error)          { return "", nil }
func (*rsStub) Bytes(int) ([]byte, error)           { return nil, nil }
func (*rsStub) Boolean(int) (bool, error)           { return false, nil }
func (*rsStub) Object(int) (any, error)             { return nil, nil }
func (*rsStub) WasNull() bool                       { return false }
func (*rsStub) Continuation() (Continuation, error) { return nil, nil }
func (*rsStub) LongByName(string) (int64, error)    { return 0, nil }
func (*rsStub) StringByName(string) (string, error) { return "", nil }
func (*rsStub) BytesByName(string) ([]byte, error)  { return nil, nil }
func (*rsStub) BooleanByName(string) (bool, error)  { return false, nil }
func (*rsStub) ObjectByName(string) (any, error)    { return nil, nil }

type rsmdStub struct{}

func (*rsmdStub) ColumnCount() int                     { return 0 }
func (*rsmdStub) ColumnName(int) (string, error)       { return "", nil }
func (*rsmdStub) ColumnLabel(int) (string, error)      { return "", nil }
func (*rsmdStub) ColumnType(int) (int, error)          { return 0, nil }
func (*rsmdStub) ColumnTypeName(int) (string, error)   { return "", nil }
func (*rsmdStub) ColumnNullable(int) (int, error)      { return 0, nil }
func (*rsmdStub) ColumnDataType(int) (DataType, error) { return nil, nil }

var (
	_ Driver            = (*driverStub)(nil)
	_ Connection        = (*connStub)(nil)
	_ Statement         = (*stmtStub)(nil)
	_ PreparedStatement = (*preparedStub)(nil)
	_ ResultSet         = (*rsStub)(nil)
	_ ResultSetMetaData = (*rsmdStub)(nil)
)
