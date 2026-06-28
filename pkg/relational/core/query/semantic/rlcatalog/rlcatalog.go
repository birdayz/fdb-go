// Package rlcatalog adapts the Record Layer's `RecordMetaData` into
// the `semantic.Catalog` interface. Isolated in a sub-package so the
// core semantic package stays free of a recordlayer dependency —
// callers that want the adapter import this package explicitly.
package rlcatalog

import (
	"sync"

	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/query/semantic"
)

// Wrap adapts a *RecordMetaData to the semantic.Catalog interface.
// Returns a Catalog that looks up tables by name against
// md.RecordTypes(). Nil metadata → empty catalog (every lookup
// returns false; matches the "no schema yet" stub case).
//
// Pre-builds a folded-name index at construction so LookupTable is
// O(1) instead of scanning all record types every call. RecordMetaData
// is effectively immutable post-Build (callers re-wrap on rebuild),
// so caching is safe.
func Wrap(md *recordlayer.RecordMetaData) semantic.Catalog {
	c := &wrappedCatalog{md: md}
	if md != nil {
		c.byFoldedName = make(map[string]*recordlayer.RecordType, len(md.RecordTypes()))
		for rtName, rt := range md.RecordTypes() {
			c.byFoldedName[semantic.NewUnquoted(rtName).Name()] = rt
		}
	}
	return c
}

// NewAnalyzer is the end-user convenience: given a RecordMetaData
// and a case-sensitivity flag, wire up a ready-to-use
// semantic.Analyzer. Saves callers a Wrap() + NewAnalyzer() boilerplate
// pair.
func NewAnalyzer(md *recordlayer.RecordMetaData, caseSensitive bool) *semantic.Analyzer {
	return semantic.NewAnalyzer(Wrap(md), caseSensitive)
}

type wrappedCatalog struct {
	md *recordlayer.RecordMetaData
	// byFoldedName indexes RecordTypes by case-folded key for O(1)
	// LookupTable — computed once at Wrap time.
	byFoldedName map[string]*recordlayer.RecordType
}

// LookupTable implements semantic.Catalog. RecordMetaData has no
// schema qualifier — qualified names don't match anything. Hits the
// pre-built case-folded index so lookup is O(1).
func (w *wrappedCatalog) LookupTable(name semantic.QualifiedName) (semantic.Table, bool) {
	if w.md == nil {
		return nil, false
	}
	if name.IsQualified() {
		return nil, false
	}
	rt, ok := w.byFoldedName[name.Name()]
	if !ok {
		return nil, false
	}
	return &recordTypeTable{rt: rt, name: name}, true
}

// TableExists implements semantic.Catalog.
func (w *wrappedCatalog) TableExists(name semantic.QualifiedName) bool {
	_, ok := w.LookupTable(name)
	return ok
}

// AllTableNames implements semantic.Catalog. Returns original
// proto-casing names (not case-folded) so INFORMATION_SCHEMA
// enumeration and 'available tables: …' error messages show the
// user's preferred spelling, not UPPER-ed keys. Iterates
// md.RecordTypes() rather than the folded index for that reason.
func (w *wrappedCatalog) AllTableNames() []semantic.QualifiedName {
	if w.md == nil {
		return nil
	}
	types := w.md.RecordTypes()
	out := make([]semantic.QualifiedName, 0, len(types))
	for rtName := range types {
		// Wrap as unqualified QualifiedName since Record Layer has
		// no schemas. FromSegments with caseSensitive=true
		// preserves the source casing verbatim.
		out = append(out, semantic.FromSegments([]string{rtName}, true))
	}
	return out
}

// recordTypeTable adapts a RecordType to semantic.Table. Columns are
// the proto fields; index names come from RecordType.GetIndexes().
//
// LookupColumn builds a folded-name → field index on first access
// (via sync.Once) so repeated column lookups on the same table are
// O(1). The per-table cost is one map allocation; amortised across
// every column reference in a query, worth it.
type recordTypeTable struct {
	rt   *recordlayer.RecordType
	name semantic.QualifiedName

	// Column cache: built once per table on first access. Keyed by
	// case-folded column name. Values are fully-materialised
	// semantic.Columns so repeated lookups don't re-allocate
	// Identifier / Column values per call.
	colIndexOnce sync.Once
	colIndex     map[string]semantic.Column
	colOrdered   []semantic.Column
}

func (t *recordTypeTable) ensureColumnIndex() {
	t.colIndexOnce.Do(func() {
		if t.rt.Descriptor == nil {
			t.colIndex = map[string]semantic.Column{}
			return
		}
		fields := t.rt.Descriptor.Fields()
		t.colIndex = make(map[string]semantic.Column, fields.Len())
		t.colOrdered = make([]semantic.Column, 0, fields.Len())
		for i := 0; i < fields.Len(); i++ {
			f := fields.Get(i)
			id := semantic.NewUnquoted(string(f.Name()))
			col := semantic.Column{
				Id:       id,
				Type:     protoKindToSQL(f.Kind()),
				Nullable: isNullable(f),
				// A repeated field is a SQL ARRAY. The Type string carries
				// the element kind (protoKindToSQL maps the scalar Kind);
				// IsArray is the signal that the column itself is an array,
				// needed to type the resolved Value as an ArrayType.
				IsArray: isRepeated(f),
			}
			t.colIndex[id.Name()] = col
			t.colOrdered = append(t.colOrdered, col)
		}
	})
}

func (t *recordTypeTable) Name() semantic.QualifiedName { return t.name }

func (t *recordTypeTable) Columns() []semantic.Column {
	t.ensureColumnIndex()
	if len(t.colOrdered) == 0 {
		return []semantic.Column{}
	}
	// Defensive copy: callers may mutate the returned slice (tests
	// do). The underlying Column values are value-types, so a flat
	// copy is sufficient.
	out := make([]semantic.Column, len(t.colOrdered))
	copy(out, t.colOrdered)
	return out
}

func (t *recordTypeTable) LookupColumn(id semantic.Identifier) (semantic.Column, bool) {
	t.ensureColumnIndex()
	col, ok := t.colIndex[id.Name()]
	return col, ok
}

func (t *recordTypeTable) Indexes() []string {
	// Include single-type AND multi-type indexes defined on this
	// record type. Universal indexes (defined on every type) are
	// not included here — they're a RecordMetaData-level concept
	// and belong on a future Catalog.UniversalIndexes() accessor.
	idxs := t.rt.GetIndexes()
	multi := t.rt.GetMultiTypeIndexes()
	if len(idxs)+len(multi) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(idxs)+len(multi))
	for _, idx := range idxs {
		if idx != nil {
			out = append(out, idx.Name)
		}
	}
	for _, idx := range multi {
		if idx != nil {
			out = append(out, idx.Name)
		}
	}
	return out
}

// isRepeated reports whether the descriptor is a list-typed (array) field.
// A proto map<k,v> field is also Cardinality()==Repeated, so it must be
// excluded — otherwise a map column would be mistyped as an array. Matches
// the IsMap() guard already used at metadata/proto_types.go.
func isRepeated(f protoreflect.FieldDescriptor) bool {
	return f.Cardinality() == protoreflect.Repeated && !f.IsMap()
}

// isNullable reports whether a proto field can be absent (and thus
// read as SQL NULL). A field is NOT nullable when it's a repeated
// (list) field or declared as proto2 `required`. Everything else —
// proto3 singular scalars, proto2 optional (with or without explicit
// defaults), proto3 message fields — is nullable per SQL semantics.
//
// Review feedback caught an earlier bug here: `!f.HasDefault()` was
// used as the nullability proxy, which flagged proto2 explicit-default
// fields as NOT nullable. Cardinality is the right signal — required
// is proto2-only; proto3 singular is Optional (and thus nullable).
func isNullable(f protoreflect.FieldDescriptor) bool {
	if isRepeated(f) {
		return false
	}
	return f.Cardinality() != protoreflect.Required
}

// protoKindToSQL maps a proto field kind to the seed's string-valued
// column Type. The mapping is coarse — the Type hierarchy port will
// replace this with a structured DataType.
func protoKindToSQL(k protoreflect.Kind) string {
	switch k {
	case protoreflect.BoolKind:
		return "BOOL"
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return "INT"
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return "FLOAT"
	case protoreflect.StringKind:
		return "STRING"
	case protoreflect.BytesKind:
		return "BYTES"
	case protoreflect.EnumKind:
		return "ENUM"
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return "RECORD"
	}
	return "UNKNOWN"
}
