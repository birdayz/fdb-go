package api

import "context"

// Compile-time stub verifying the DatabaseMetaData method set.

type dbMDStub struct{}

func (*dbMDStub) Schemas(context.Context) (ResultSet, error) { return nil, nil }
func (*dbMDStub) SchemasFiltered(context.Context, string, string) (ResultSet, error) {
	return nil, nil
}

func (*dbMDStub) Tables(context.Context, string, string, string, []string) (ResultSet, error) {
	return nil, nil
}

func (*dbMDStub) Columns(context.Context, string, string, string, string) (ResultSet, error) {
	return nil, nil
}

func (*dbMDStub) IndexInfo(context.Context, string, string, string, bool, bool) (ResultSet, error) {
	return nil, nil
}

func (*dbMDStub) PrimaryKeys(context.Context, string, string, string) (ResultSet, error) {
	return nil, nil
}
func (*dbMDStub) URL() string                    { return "" }
func (*dbMDStub) UserName() string               { return "" }
func (*dbMDStub) IsReadOnly() bool               { return false }
func (*dbMDStub) DatabaseProductName() string    { return "" }
func (*dbMDStub) DatabaseProductVersion() string { return "" }
func (*dbMDStub) DriverName() string             { return "" }
func (*dbMDStub) DriverVersion() string          { return "" }

var _ DatabaseMetaData = (*dbMDStub)(nil)
