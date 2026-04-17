package api

// ParseTreeInfo is the minimal shape of a parsed SQL query exposed to
// SDK callers. Mirrors Java's
// com.apple.foundationdb.relational.api.ParseTreeInfo.
//
// Concrete ANTLR parse-tree types live in pkg/relational/core/parser;
// callers who want the full AST type-assert on the concrete type.
type ParseTreeInfo interface {
	// QueryType returns the top-level query kind.
	QueryType() QueryType
}

// QueryType is the top-level SQL statement kind. Mirrors Java's
// ParseTreeInfo.QueryType enum.
type QueryType int

const (
	QueryTypeSelect QueryType = iota
	QueryTypeInsert
	QueryTypeUpdate
	QueryTypeDelete
	QueryTypeCreate
)

// String returns the Java enum name for logging / display.
func (q QueryType) String() string {
	switch q {
	case QueryTypeSelect:
		return "SELECT"
	case QueryTypeInsert:
		return "INSERT"
	case QueryTypeUpdate:
		return "UPDATE"
	case QueryTypeDelete:
		return "DELETE"
	case QueryTypeCreate:
		return "CREATE"
	default:
		return "?"
	}
}

// WithMetadata is a trait for values that expose a relational-layer
// DataType — implemented by RelationalArray / RelationalStruct /
// things that carry richer type info than a raw JDBC code can convey.
// Mirrors Java's com.apple.foundationdb.relational.api.WithMetadata.
type WithMetadata interface {
	// RelationalMetaData returns the DataType that describes this
	// value's shape (STRUCT, ARRAY, or primitive).
	RelationalMetaData() (DataType, error)
}
