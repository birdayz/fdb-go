//go:generate go run go.uber.org/mock/mockgen -source=$GOFILE -destination=mocks_$GOFILE -package=api

package api

// Metadata is the base interface for every relational metadata node
// (Table, Column, Index, Schema, SchemaTemplate, View,
// InvokedRoutine). Mirrors Java's
// com.apple.foundationdb.relational.api.metadata.Metadata.
type Metadata interface {
	// MetadataName returns the node's own name. Named "MetadataName"
	// rather than "Name" so embeddings keep Name() free for other
	// uses (e.g. ColumnName() on Column).
	MetadataName() string
	// Accept dispatches through the given Visitor. Implementations
	// should call the appropriate v.Visit* method for their type
	// and (for composite nodes) recurse into children.
	Accept(v Visitor)
}

// Visitor is the metadata-tree visitor. Implementations need to
// handle every concrete node type; the tree is traversed by calling
// Metadata.Accept. Mirrors Java's
// com.apple.foundationdb.relational.api.metadata.Visitor.
type Visitor interface {
	VisitTable(t Table)
	VisitColumn(c Column)
	StartVisitSchemaTemplate(s SchemaTemplate)
	VisitSchemaTemplate(s SchemaTemplate)
	FinishVisitSchemaTemplate(s SchemaTemplate)
	VisitSchema(s Schema)
	VisitIndex(i Index)
	VisitInvokedRoutine(r InvokedRoutine)
	VisitView(v View)
}

// Column is one column of a Table. Mirrors Java's Column.
type Column interface {
	Metadata
	// DataType returns the column's declared type.
	DataType() DataType
}

// Table is a relational table. Mirrors Java's Table.
type Table interface {
	Metadata
	// Indexes returns the indexes defined on this table.
	Indexes() []Index
	// Columns returns the columns in declared order.
	Columns() []Column
	// StructDataType returns the struct type whose fields match this
	// table's columns. Matches Java's Table.getDatatype() returning
	// DataType.StructType.
	StructDataType() *StructType
}

// Index is a secondary index metadata. Mirrors Java's Index.
//
// IndexType is represented as a string here because Java does the
// same (it references string constants from
// com.apple.foundationdb.record.metadata.IndexTypes). When we port
// IndexTypes to a Go enum we will update this interface accordingly.
type Index interface {
	Metadata
	// TableName returns the name of the owning table.
	TableName() string
	// IndexType returns the index-type string (VALUE, COUNT, RANK,
	// VERSION, TEXT, VECTOR, ...). Matches Java IndexTypes constants.
	IndexType() string
	// IsUnique reports whether the index rejects duplicate entries.
	IsUnique() bool
	// IsSparse reports whether the index skips rows where the
	// expression evaluates to null.
	IsSparse() bool
}

// View is a SQL view. Mirrors Java's View interface.
// Minimal today — the Java side is also mostly placeholder.
type View interface {
	Metadata
}

// InvokedRoutine is a stored routine (function / procedure).
// Mirrors Java's InvokedRoutine. Minimal today.
type InvokedRoutine interface {
	Metadata
}

// SchemaTemplate is the versioned schema shape (tables + views +
// indexes). Mirrors Java's SchemaTemplate.
type SchemaTemplate interface {
	Metadata
	// Version is the schema version; incremented on every metadata
	// change. Matches RecordMetaData.version semantics.
	Version() int
	// EnableLongRows allows records larger than 100 KB via split
	// records (matches RecordMetaData.splitLongRecords).
	EnableLongRows() bool
	// StoreRowVersions indicates each row carries a monotonically
	// increasing version (matches Java's storeRowVersions).
	StoreRowVersions() bool
	// IntermingleTables reports whether rows from different record
	// types share the same keyspace prefix (no RecordTypeKey prefix
	// on primary keys). Matches Java's
	// RecordLayerSchemaTemplate.isIntermingleTables() which is
	// derived from `!primaryKeyHasRecordTypePrefix()`.
	IntermingleTables() bool
	// Tables returns the tables defined in this template. Error is
	// reserved for I/O / catalog-access failures; an empty template
	// returns a nil slice and nil error.
	Tables() ([]Table, error)
	// Views returns the views defined in this template. Same error
	// semantics as Tables.
	Views() ([]View, error)
	// FindTable looks up a table by name. Returns (nil, nil) when the
	// name is not found; returns an error only on a catalog access
	// failure.
	FindTable(name string) (Table, error)
	// FindView looks up a view by name; same not-found semantics as
	// FindTable.
	FindView(name string) (View, error)
	// TableIndexMapping returns (tableName → []indexName) mapping
	// (Java returns Guava Multimap; Go idiom is a map-of-slices).
	TableIndexMapping() (map[string][]string, error)
	// Indexes returns the names of every index declared in this
	// template.
	Indexes() ([]string, error)
	// InvokedRoutines returns every routine declared in this template.
	InvokedRoutines() ([]InvokedRoutine, error)
	// FindInvokedRoutine looks up a routine by name; nil if not found.
	FindInvokedRoutine(name string) (InvokedRoutine, error)
	// TemporaryInvokedRoutines returns transient routines added during
	// the current transaction.
	TemporaryInvokedRoutines() ([]InvokedRoutine, error)
	// TransactionBoundMetadataAsString is a diagnostic string
	// representation of the transaction-bound metadata.
	TransactionBoundMetadataAsString() (string, error)
	// GenerateSchema materializes a Schema from this template.
	GenerateSchema(databaseID, schemaName string) Schema
}

// Schema is an instantiated schema (one per database/namespace).
// Mirrors Java's Schema. The Tables / Views / Indexes / InvokedRoutines
// methods are intentionally delegated to the underlying SchemaTemplate
// in every current implementation — matches Java's Schema default
// method bodies. They're part of the interface (not package-level
// helpers) because Java's static type system puts them on Schema, and
// consumers call schema.getTables() rather than schema.getSchemaTemplate().getTables().
type Schema interface {
	Metadata
	// SchemaTemplate returns the template this schema was generated
	// from. Matches Java's Schema.getSchemaTemplate().
	SchemaTemplate() SchemaTemplate
	// DatabaseName returns the owning database's name.
	DatabaseName() string
	// Tables delegates to SchemaTemplate.Tables — convenience matching
	// Java's default Schema.getTables().
	Tables() ([]Table, error)
	// Views delegates to SchemaTemplate.Views.
	Views() ([]View, error)
	// Indexes returns the (tableName -> indexNames) mapping — matches
	// Java's Schema.getIndexes() default which delegates to the
	// template's TableIndexMapping. NOT the same shape as
	// SchemaTemplate.Indexes (which returns a flat []string).
	Indexes() (map[string][]string, error)
	// InvokedRoutines delegates to SchemaTemplate.InvokedRoutines.
	InvokedRoutines() ([]InvokedRoutine, error)
}

// ---- Visitor convenience dispatchers ----

// VisitTableTree calls v.VisitTable(t) then recurses into t's indexes
// and columns. Mirrors Java's default Table.accept().
func VisitTableTree(t Table, v Visitor) {
	v.VisitTable(t)
	for _, idx := range t.Indexes() {
		idx.Accept(v)
	}
	for _, col := range t.Columns() {
		col.Accept(v)
	}
}

// VisitSchemaTemplateTree mirrors Java's default
// SchemaTemplate.accept(): startVisit → visit → finishVisit.
func VisitSchemaTemplateTree(s SchemaTemplate, v Visitor) {
	v.StartVisitSchemaTemplate(s)
	v.VisitSchemaTemplate(s)
	v.FinishVisitSchemaTemplate(s)
}
