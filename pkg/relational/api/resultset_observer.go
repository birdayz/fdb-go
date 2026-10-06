package api

import "context"

type resultSetMetaDataObserverKey struct{}

// WithResultSetMetaDataObserver returns a context under which each query the
// embedded driver opens hands its result set's metadata to observe, before the
// first row is read. It is database/sql's reach to Java's
// ResultSet.getMetaData: database/sql's ColumnType carries one flat type name,
// while ColumnDataType here carries a struct column's declared type name and
// fields and an array column's element type.
func WithResultSetMetaDataObserver(ctx context.Context, observe func(ResultSetMetaData)) context.Context {
	return context.WithValue(ctx, resultSetMetaDataObserverKey{}, observe)
}

// ResultSetMetaDataObserver is the observer ctx carries, or nil.
func ResultSetMetaDataObserver(ctx context.Context) func(ResultSetMetaData) {
	observe, _ := ctx.Value(resultSetMetaDataObserverKey{}).(func(ResultSetMetaData))
	return observe
}
