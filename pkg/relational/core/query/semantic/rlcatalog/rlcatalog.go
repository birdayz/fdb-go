// Package rlcatalog adapts the Record Layer's `RecordMetaData` into
// the `semantic.Catalog` interface. Isolated in a sub-package so the
// core semantic package stays free of a recordlayer dependency —
// callers that want the adapter import this package explicitly.
package rlcatalog

import (
	"strings"
	"sync"

	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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
		c.storeRowVersions = md.IsStoreRecordVersions()
		c.byFoldedName = make(map[string]*recordlayer.RecordType, len(md.RecordTypes()))
		for rtName, rt := range md.RecordTypes() {
			// The SQL surface speaks USER identifiers; the descriptor
			// (and every wire address derived from it) speaks STORAGE
			// names. They differ exactly when the DDL name carried '$',
			// '.' or "__" (ProtoUtils escaping). Java keeps both on the
			// type — Type.Record.fromDescriptorPreservingName stores
			// `ProtoUtils.toUserIdentifier(descriptor.getName())` as the
			// NAME and the raw descriptor name as the storage name
			// (Type.java:2591-2593), and Field does the same per column
			// (Type.java:2874-2877). Indexing the folded USER name here
			// is what lets `SELECT … FROM "foo$table"` find the record
			// type stored as FOO__1TABLE; without it the table exists on
			// the wire and is unreachable from SQL (42F01).
			c.byFoldedName[semantic.NewUnquoted(recordlayer.ToUserIdentifier(rtName)).Name()] = rt
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
	// storeRowVersions mirrors md.IsStoreRecordVersions(): when set, every
	// table exposes the trailing __ROW_VERSION pseudo-column (Java:
	// RecordMetaData.getPlannerType appends Type.Record.addPseudoFields,
	// RecordMetaData.java:732-739).
	storeRowVersions bool
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
	return &recordTypeTable{rt: rt, name: name, storeRowVersions: w.storeRowVersions}, true
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
		// preserves the source casing verbatim. The USER identifier is
		// what a caller can actually type back (INFORMATION_SCHEMA rows,
		// "available tables: …" diagnostics), so the escaping is undone
		// here for the same reason LookupTable indexes on it.
		out = append(out, semantic.FromSegments([]string{recordlayer.ToUserIdentifier(rtName)}, true))
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
	// storeRowVersions: the table appends the ephemeral __ROW_VERSION
	// pseudo-column (see wrappedCatalog.storeRowVersions).
	storeRowVersions bool

	// Column cache: built once per table on first access. colIndex is
	// keyed by the EXACT proto field name (the DDL-normalized spelling:
	// unquoted columns are stored upper-cased, quoted ones verbatim);
	// foldedIndex maps the case-folded name to every column that folds
	// to it, serving the case-insensitive fallback for raw-proto
	// metadata whose field names never went through DDL normalization.
	// Values are fully-materialised semantic.Columns so repeated
	// lookups don't re-allocate Identifier / Column values per call.
	colIndexOnce sync.Once
	colIndex     map[string]semantic.Column
	foldedIndex  map[string][]semantic.Column
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
		t.foldedIndex = make(map[string][]semantic.Column, fields.Len())
		t.colOrdered = make([]semantic.Column, 0, fields.Len())
		for i := 0; i < fields.Len(); i++ {
			f := fields.Get(i)
			col := columnForField(f, nil)
			id := col.Id
			// The EXACT proto field name is a lookup key (a quoted-DDL
			// column's field preserves case — "x" — and a quoted lookup
			// must hit it; folding it away 42703'd SELECT "x", the WS-N
			// quoting-blindness). The COLUMN itself presents the FOLDED
			// identifier everywhere: the runtime positional layout folds
			// names too, so folded presentation keeps plan-time and
			// runtime in one namespace.
			// Both spellings are lookup keys: the STORAGE name (what the
			// descriptor carries, and what an internally-minted reference
			// addressing the proto field uses) and the USER name (what SQL
			// text spells). They coincide unless the DDL name was escaped.
			t.colIndex[string(f.Name())] = col
			if userName := recordlayer.ToUserIdentifier(string(f.Name())); userName != string(f.Name()) {
				t.colIndex[userName] = col
			}
			t.foldedIndex[id.Name()] = append(t.foldedIndex[id.Name()], col)
			t.colOrdered = append(t.colOrdered, col)
		}
		// The __ROW_VERSION pseudo-column: appended as one trailing EPHEMERAL
		// column when the metadata stores row versions and no REAL descriptor
		// field of that exact name exists (real-column-wins — Java's
		// Type.Record.addPseudoFields skip, Type.java:2358-2368, and
		// generateTableAccess's noneMatch gate, LogicalOperator.java:296-301).
		// Ephemeral keeps it out of star expansion while name resolution and
		// the flowed row layout (sourceRowType's trailing slot) still see it.
		if t.storeRowVersions {
			if _, exists := t.colIndex[values.PseudoFieldRowVersion]; !exists {
				id := semantic.NewUnquoted(values.PseudoFieldRowVersion)
				col := semantic.Column{
					Id: id,
					// Java: Type.primitiveType(TypeCode.VERSION, true)
					// (PseudoField.java:37).
					Type:      "VERSION",
					Nullable:  true,
					Ephemeral: true,
				}
				t.colIndex[values.PseudoFieldRowVersion] = col
				t.foldedIndex[id.Name()] = append(t.foldedIndex[id.Name()], col)
				t.colOrdered = append(t.colOrdered, col)
			}
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
	// Exact field spelling first (a quoted lookup hits its
	// case-significant column verbatim; unquoted lookups arrive
	// pre-folded and hit DDL-normalized fields). The case-insensitive
	// fallback serves raw-proto metadata (lowercase field names that
	// never saw DDL normalization) — only when the folded name is
	// UNAMBIGUOUS; a folded collision must not silently pick a column.
	if col, ok := t.colIndex[id.Name()]; ok {
		return col, true
	}
	cands := t.foldedIndex[strings.ToUpper(id.Name())]
	if len(cands) == 1 {
		return cands[0], true
	}
	return semantic.Column{}, false
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

// uuidProtoMessageName is the fully-qualified tuple_fields.UUID message that
// fdb-relational stores UUID column values in (canonical copy in
// pkg/relational/core/functions.UUIDProtoMessageName; duplicated here to avoid
// an import cycle through the functions package). Java treats UUID as a
// first-class primitive (DataType.Primitives.UUID), so the column must resolve
// to a UUID cascades type — not the generic "RECORD"/Unknown a bare MessageKind
// would give — for `WHERE uuid_col = '<uuid>'` to type its comparand and hit
// the index.
const uuidProtoMessageName = "com.apple.foundationdb.record.UUID"

// protoFieldToSQL maps a proto field to the seed's string-valued column Type,
// recognizing the special tuple_fields.UUID message as the scalar "UUID" type
// (Java's DataType.Primitives.UUID) before falling back to the coarse
// kind-based mapping. Every other MessageKind stays "RECORD".
// columnForField builds the analyzer's view of ONE proto field. It is the
// single place the array/nullable/UUID unwrapping happens, because a STRUCT
// column's nested fields must get the identical treatment the table's own
// columns get — a nullable array nested inside a struct is the same
// NullableArrayWrapper shape as one at the top level, and a nested UUID is the
// same tuple_fields.UUID message. Two implementations of that unwrapping would
// disagree exactly one level down, where nothing looks.
//
// enclosing carries the message full-names already open on the current descent
// path. A proto descriptor may be self-referential (nothing at the DESCRIPTOR
// level forbids it — the DDL struct registry rejects cycles, but raw
// record metadata never went through DDL), and this builder is eager, so a
// cycle would recurse forever at catalog-build time. Re-entering a message
// already on the path stops the descent and leaves StructFields empty: the
// column is still a RECORD and still resolvable, only its nested field list is
// unavailable, which resolution reports as an ordinary miss.
func columnForField(f protoreflect.FieldDescriptor, enclosing []protoreflect.FullName) semantic.Column {
	// A repeated field is a SQL ARRAY; so is a NullableArrayWrapper field (a
	// singular message wrapping `repeated values` — the stored shape of a
	// NULLABLE array, whose absence is the NULL array). The Type string
	// carries the ELEMENT kind; IsArray is the signal that the column itself
	// is an array, needed to type the resolved Value as an ArrayType.
	elemF := f
	isArr := isRepeated(f)
	nullable := isNullable(f)
	if inner, wrapped, ok := values.EffectiveListField(f); ok && wrapped {
		elemF = inner
		isArr = true
		nullable = true
	}
	// The COLUMN identifier is the USER name: Java's Field carries
	// `ProtoUtils.toUserIdentifier(fieldDescriptor.getName())` as its name
	// and the raw descriptor name separately as the storage name
	// (Type.java:2874-2877). Identical for every column whose DDL name had
	// no '$', '.' or "__"; for the rest this is what makes `SELECT "a$b"`
	// resolve against the field stored as A__1B.
	col := semantic.Column{
		Id:       semantic.NewUnquoted(recordlayer.ToUserIdentifier(string(f.Name()))),
		Type:     protoFieldToSQL(elemF),
		Nullable: nullable,
		IsArray:  isArr,
	}
	// Only a field the type mapping calls RECORD carries a field list: the
	// UUID message maps to "UUID" and is a scalar to every consumer, so
	// descending into its proto fields would invent `id.msb` as a resolvable
	// column that Java has no field for.
	if col.Type != "RECORD" {
		return col
	}
	msg := elemF.Message()
	if msg == nil {
		return col
	}
	// Preserve the descriptor identity beside the coarse SQL kind. The
	// cascades scan layout derives this same full name directly from the proto
	// descriptor; carrying it through semantic resolution keeps a nested
	// FieldValue's root type exact with that edge.
	col.StructTypeName = string(msg.FullName())
	for _, open := range enclosing {
		if open == msg.FullName() {
			return col
		}
	}
	// COPY rather than append in place: sibling fields at one level would
	// otherwise share the backing array and one sibling's descent would
	// overwrite the path another sibling's descent is still reading.
	path := make([]protoreflect.FullName, len(enclosing)+1)
	copy(path, enclosing)
	path[len(enclosing)] = msg.FullName()
	enclosing = path
	nested := msg.Fields()
	col.StructFields = make([]semantic.Column, 0, nested.Len())
	for i := 0; i < nested.Len(); i++ {
		col.StructFields = append(col.StructFields, columnForField(nested.Get(i), enclosing))
	}
	return col
}

func protoFieldToSQL(f protoreflect.FieldDescriptor) string {
	if f.Kind() == protoreflect.MessageKind {
		if msg := f.Message(); msg != nil && string(msg.FullName()) == uuidProtoMessageName {
			return "UUID"
		}
	}
	return protoKindToSQL(f.Kind())
}

// protoKindToSQL maps a proto field kind to the seed's string-valued
// column Type. The mapping is coarse — the Type hierarchy port will
// replace this with a structured DataType.
func protoKindToSQL(k protoreflect.Kind) string {
	switch k {
	case protoreflect.BoolKind:
		return "BOOL"
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		// Genuine 32-bit INTEGER: Java types these INT and runs the
		// int32-bounded arithmetic lane. The old all-integers→"INT"
		// conflation was harmless only while "INT" aliased to the LONG
		// type; with real width typing it would have put BIGINT columns
		// on the int32 lane.
		return "INTEGER"
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return "BIGINT"
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		// Deliberate divergence for the 32-bit unsigned kinds: Java maps
		// UINT32/FIXED32 → TypeCode.INT (Type.java
		// fromProtobufFieldDescriptor), which is sound there only because
		// Java protobuf wraps uint32 into a Java int. Go decodes unsigned
		// kinds as genuine unsigned values (up to 2^32-1), so an "INTEGER"
		// typing would put values beyond MaxInt32 under a false 32-bit
		// arithmetic bound. BIGINT keeps them on the long lane. Reachable
		// only defensively: record-metadata validation rejects unsigned
		// fields in record types (matching Java).
		return "BIGINT"
	case protoreflect.FloatKind:
		return "FLOAT"
	case protoreflect.DoubleKind:
		return "DOUBLE"
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
