package recordlayer

import (
	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	proto "google.golang.org/protobuf/proto"
)

// DimensionsKeyExpression splits a key expression into prefix, dimensions, and suffix.
// Used by MULTIDIMENSIONAL indexes to separate grouping, spatial, and key-suffix components.
// Matches Java's DimensionsKeyExpression.
type DimensionsKeyExpression struct {
	WholeKey       KeyExpression
	PrefixSize     int
	DimensionsSize int
}

// Dimensions creates a DimensionsKeyExpression.
func Dimensions(wholeKey KeyExpression, prefixSize, dimensionsSize int) *DimensionsKeyExpression {
	return &DimensionsKeyExpression{
		WholeKey:       wholeKey,
		PrefixSize:     prefixSize,
		DimensionsSize: dimensionsSize,
	}
}

// Evaluate delegates to the whole key expression.
func (d *DimensionsKeyExpression) Evaluate(record *FDBStoredRecord[proto.Message], msg proto.Message) ([][]any, error) {
	return d.WholeKey.Evaluate(record, msg)
}

// ColumnSize returns the column size of the whole key.
func (d *DimensionsKeyExpression) ColumnSize() int {
	return d.WholeKey.ColumnSize()
}

// FieldNames delegates to the whole key.
func (d *DimensionsKeyExpression) FieldNames() []string {
	return d.WholeKey.FieldNames()
}

// ToKeyExpression serializes to proto.
func (d *DimensionsKeyExpression) ToKeyExpression() *gen.KeyExpression {
	inner := d.WholeKey.ToKeyExpression()
	return &gen.KeyExpression{
		Dimensions: &gen.Dimensions{
			WholeKey:       inner,
			PrefixSize:     proto.Int32(int32(d.PrefixSize)),
			DimensionsSize: proto.Int32(int32(d.DimensionsSize)),
		},
	}
}

// SuffixSize returns the number of suffix columns (after prefix + dimensions).
func (d *DimensionsKeyExpression) SuffixSize() int {
	return d.WholeKey.ColumnSize() - d.PrefixSize - d.DimensionsSize
}

// SplitIndexEntry splits an evaluated index entry tuple into prefix, dimensions, and suffix.
func (d *DimensionsKeyExpression) SplitIndexEntry(entry tuple.Tuple) (prefix, dims, suffix tuple.Tuple) {
	if d.PrefixSize > 0 && d.PrefixSize <= len(entry) {
		prefix = entry[:d.PrefixSize]
	}
	dimEnd := d.PrefixSize + d.DimensionsSize
	if dimEnd <= len(entry) {
		dims = entry[d.PrefixSize:dimEnd]
	}
	if dimEnd < len(entry) {
		suffix = entry[dimEnd:]
	}
	return
}
