package metadata

import (
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
)

// RecordLayerTable adapts a recordlayer.RecordType to api.Table.
//
// The view is computed eagerly at construction so repeated Columns()
// / StructDataType() calls are cheap; if that becomes a problem we can
// switch to lazy caching. Current sizes (dozens of fields) make it
// not worth optimising.
type RecordLayerTable struct {
	underlying *recordlayer.RecordType
	columns    []api.Column
	structType *api.StructType
	indexes    []api.Index
}

// newTable constructs a RecordLayerTable. indexes is the list of
// indexes defined for this record type — the caller has already
// filtered the schema template's global index map via
// RecordMetaData.GetIndexesForRecordType / GetUniversalIndexes.
func newTable(rt *recordlayer.RecordType, indexes []api.Index) (*RecordLayerTable, error) {
	fields := rt.Descriptor.Fields()
	cols := make([]api.Column, 0, fields.Len())
	structFields := make([]api.StructField, 0, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		c, err := newColumn(fd)
		if err != nil {
			return nil, err
		}
		cols = append(cols, c)
		structFields = append(structFields, api.NewStructField(string(fd.Name()), c.DataType(), i))
	}
	// Table's struct type is nullable=true, matching Java's
	// RecordLayerTable.getDatatype() which calls
	// DataType.StructType.from(name, fields, /*nullable=*/ true).
	// Java has a TODO noting the nullability isn't fully correct;
	// track the value Java actually emits, not the intention.
	st := api.NewStructType(rt.Name, structFields, true)
	return &RecordLayerTable{
		underlying: rt,
		columns:    cols,
		structType: st,
		indexes:    indexes,
	}, nil
}

// MetadataName returns the table (record-type) name.
func (t *RecordLayerTable) MetadataName() string { return t.underlying.Name }

// Columns returns the columns in declared order.
func (t *RecordLayerTable) Columns() []api.Column { return t.columns }

// Indexes returns the indexes on this table.
func (t *RecordLayerTable) Indexes() []api.Index { return t.indexes }

// StructDataType returns the table's composite struct type.
func (t *RecordLayerTable) StructDataType() *api.StructType { return t.structType }

// Accept dispatches into the Visitor. The default cascade visits the
// table itself, then its indexes and columns — mirroring Java's
// Table.accept().
func (t *RecordLayerTable) Accept(v api.Visitor) { api.VisitTableTree(t, v) }

// RecordType exposes the underlying record-layer type for callers that
// need its proto descriptor, primary key, etc.
func (t *RecordLayerTable) RecordType() *recordlayer.RecordType { return t.underlying }
