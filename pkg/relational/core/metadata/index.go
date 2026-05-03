package metadata

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
)

// RecordLayerIndex adapts a *recordlayer.Index (plus its owning record
// type name) to api.Index.
//
// Java's RecordLayerIndex.isSparse() returns `predicate != null` — a
// predicate-filtered index is considered sparse. recordlayer.Index
// has an equivalent Predicate field, so we apply the same rule.
type RecordLayerIndex struct {
	underlying *recordlayer.Index
	tableName  string
}

// newIndex constructs a RecordLayerIndex. tableName is the owning
// record-type name.
func newIndex(underlying *recordlayer.Index, tableName string) *RecordLayerIndex {
	return &RecordLayerIndex{underlying: underlying, tableName: tableName}
}

// MetadataName returns the index name.
func (i *RecordLayerIndex) MetadataName() string { return i.underlying.Name }

// TableName returns the name of the record type (table) this index
// belongs to. For universal indexes the name is empty — callers that
// care must check HasUniversalOwner() or inspect the metadata.
func (i *RecordLayerIndex) TableName() string { return i.tableName }

// IndexType returns the record-layer index-type constant ("VALUE",
// "COUNT", "SUM", "RANK", ...).
func (i *RecordLayerIndex) IndexType() string { return i.underlying.Type }

// IsUnique delegates to the underlying recordlayer.Index, which reads
// IndexOptions.UNIQUE_OPTION — matches Java's Index.isUnique().
func (i *RecordLayerIndex) IsUnique() bool { return i.underlying.IsUnique() }

// IsSparse matches Java's RecordLayerIndex.isSparse() which is
// "predicate != null" — a predicate-filtered index is sparse because
// only records matching the predicate have entries.
func (i *RecordLayerIndex) IsSparse() bool { return i.underlying.Predicate != nil }

// Accept dispatches into the Visitor.
func (i *RecordLayerIndex) Accept(v api.Visitor) { v.VisitIndex(i) }

// Underlying exposes the record-layer Index for callers that need
// access to its root expression, options, version metadata, etc.
func (i *RecordLayerIndex) Underlying() *recordlayer.Index { return i.underlying }

// IndexName returns the index name. Satisfies cascades.IndexDef.
func (i *RecordLayerIndex) IndexName() string { return i.underlying.Name }

// IndexColumnNames returns the field names from the index's root key
// expression. Satisfies cascades.IndexDef.
func (i *RecordLayerIndex) IndexColumnNames() []string {
	if i.underlying.RootExpression == nil {
		return nil
	}
	return i.underlying.RootExpression.FieldNames()
}

// IndexRecordTypes returns the owning record type. Satisfies
// cascades.IndexDef.
func (i *RecordLayerIndex) IndexRecordTypes() []string {
	if i.tableName == "" {
		return nil
	}
	return []string{i.tableName}
}

// IndexIsUnique returns whether the index enforces uniqueness.
// Satisfies cascades.IndexDef.
func (i *RecordLayerIndex) IndexIsUnique() bool { return i.underlying.IsUnique() }
