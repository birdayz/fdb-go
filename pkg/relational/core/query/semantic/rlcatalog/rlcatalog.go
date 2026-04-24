// Package rlcatalog adapts the Record Layer's `RecordMetaData` into
// the `semantic.Catalog` interface. Isolated in a sub-package so the
// core semantic package stays free of a recordlayer dependency —
// callers that want the adapter import this package explicitly.
package rlcatalog

import (
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/semantic"
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

// recordTypeTable adapts a RecordType to semantic.Table. Columns are
// the proto fields; index names come from RecordType.GetIndexes().
type recordTypeTable struct {
	rt   *recordlayer.RecordType
	name semantic.QualifiedName
}

func (t *recordTypeTable) Name() semantic.QualifiedName { return t.name }

func (t *recordTypeTable) Columns() []semantic.Column {
	if t.rt.Descriptor == nil {
		return []semantic.Column{}
	}
	fields := t.rt.Descriptor.Fields()
	out := make([]semantic.Column, 0, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		out = append(out, semantic.Column{
			Id:       semantic.NewUnquoted(string(f.Name())),
			Type:     protoKindToSQL(f.Kind()),
			Nullable: isNullable(f),
		})
	}
	return out
}

func (t *recordTypeTable) LookupColumn(id semantic.Identifier) (semantic.Column, bool) {
	// Case-insensitive lookup against the descriptor's fields.
	if t.rt.Descriptor == nil {
		return semantic.Column{}, false
	}
	target := id.Name()
	fields := t.rt.Descriptor.Fields()
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		// Fields are identifier-like; case-fold for match.
		candidate := semantic.NewUnquoted(string(f.Name()))
		if candidate.EqualsIgnoreQuoting(semantic.NewUnquoted(target)) {
			return semantic.Column{
				Id:       candidate,
				Type:     protoKindToSQL(f.Kind()),
				Nullable: isNullable(f),
			}, true
		}
	}
	return semantic.Column{}, false
}

func (t *recordTypeTable) Indexes() []string {
	idxs := t.rt.GetIndexes()
	if len(idxs) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(idxs))
	for _, idx := range idxs {
		if idx == nil {
			continue
		}
		out = append(out, idx.Name)
	}
	return out
}

// isRepeated reports whether the descriptor is a list-typed field.
func isRepeated(f protoreflect.FieldDescriptor) bool {
	return f.Cardinality() == protoreflect.Repeated
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
