package plans

import (
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

// TupleSource identifies which part of an IndexEntry to extract a field from.
type TupleSource int

const (
	TupleSourceKey   TupleSource = iota // Extract from index key tuple
	TupleSourceValue                    // Extract from index value tuple
)

// IndexKeyValueToPartialRecord reconstructs a partial record from an
// index entry's key and value tuples. Used by covering index plans to
// avoid fetching the full record from FDB.
//
// Ports Java's IndexKeyValueToPartialRecord. The Go version produces
// map[string]any (the SQL layer's row format) instead of proto Messages.
type IndexKeyValueToPartialRecord struct {
	copiers    []Copier
	isRequired bool
}

// Copier extracts a field from an index entry and sets it in the output map.
type Copier interface {
	Copy(output map[string]any, key, value tuple.Tuple) bool
}

// FieldCopier copies a single field from an index key or value tuple.
type FieldCopier struct {
	Field       string
	Source      TupleSource
	OrdinalPath []int
}

func (c *FieldCopier) Copy(output map[string]any, key, value tuple.Tuple) bool {
	var t tuple.Tuple
	switch c.Source {
	case TupleSourceKey:
		t = key
	case TupleSourceValue:
		t = value
	}
	if t == nil {
		return false
	}
	val := getForOrdinalPath(t, c.OrdinalPath)
	if val == nil {
		return false
	}
	output[strings.ToUpper(c.Field)] = val
	return true
}

// ToRecord reconstructs a partial record map from an index entry.
func (r *IndexKeyValueToPartialRecord) ToRecord(key, value tuple.Tuple) map[string]any {
	output := make(map[string]any, len(r.copiers))
	allRefused := true
	for _, copier := range r.copiers {
		if copier.Copy(output, key, value) {
			allRefused = false
		}
	}
	if !r.isRequired && allRefused {
		return nil
	}
	return output
}

// getForOrdinalPath navigates a tuple by ordinal indices.
func getForOrdinalPath(t tuple.Tuple, path []int) any {
	var val any = t
	for _, idx := range path {
		switch v := val.(type) {
		case tuple.Tuple:
			if idx >= len(v) {
				return nil
			}
			val = v[idx]
		default:
			return nil
		}
		if val == nil {
			return nil
		}
	}
	return val
}

// IndexKeyValueToPartialRecordBuilder builds an IndexKeyValueToPartialRecord.
type IndexKeyValueToPartialRecordBuilder struct {
	copiers    []Copier
	isRequired bool
}

// NewIndexKeyValueToPartialRecordBuilder creates a new builder.
func NewIndexKeyValueToPartialRecordBuilder() *IndexKeyValueToPartialRecordBuilder {
	return &IndexKeyValueToPartialRecordBuilder{}
}

// AddField adds a field copier.
func (b *IndexKeyValueToPartialRecordBuilder) AddField(field string, source TupleSource, ordinalPath []int) *IndexKeyValueToPartialRecordBuilder {
	b.copiers = append(b.copiers, &FieldCopier{
		Field:       field,
		Source:      source,
		OrdinalPath: ordinalPath,
	})
	return b
}

// SetRequired marks the record as required (ToRecord panics on nil).
func (b *IndexKeyValueToPartialRecordBuilder) SetRequired(required bool) *IndexKeyValueToPartialRecordBuilder {
	b.isRequired = required
	return b
}

// Build constructs the IndexKeyValueToPartialRecord.
func (b *IndexKeyValueToPartialRecordBuilder) Build() *IndexKeyValueToPartialRecord {
	copiers := make([]Copier, len(b.copiers))
	copy(copiers, b.copiers)
	return &IndexKeyValueToPartialRecord{
		copiers:    copiers,
		isRequired: b.isRequired,
	}
}
