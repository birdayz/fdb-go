package metadata

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/google/uuid"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
)

// Builder constructs a RecordLayerSchemaTemplate from SQL-level table
// definitions without requiring a pre-compiled protobuf FileDescriptor.
//
// Mirrors Java's RecordLayerSchemaTemplate.Builder at the level needed
// for CREATE SCHEMA TEMPLATE DDL: name, version, tables with typed
// columns and primary keys, and store-level flags.
type Builder struct {
	name             string
	version          int
	tables           []tableSpec
	auxTypes         []api.Named // CREATE TYPE AS STRUCT registrations (aux_types.go)
	errs             []error     // deferred errors from AddIndex
	intermingleTbls  bool
	enableLongRows   bool
	storeRowVersions bool
}

type tableSpec struct {
	name    string
	columns []ColumnSpec
	// primaryKey holds one PATH per key part: a plain column is a
	// one-element path, a nested key (id.a) its segments. Never a joined
	// dotted string — that form cannot distinguish the nested path id.a
	// from the single quoted column "a.b".
	primaryKey [][]string
	indexes    []indexSpec
}

// indexSpec describes a single index within a table.
type indexSpec struct {
	name      string
	columns   []string // field names in index key order
	unique    bool
	aggType   string // "SUM", "COUNT", etc. — empty for VALUE indexes
	aggColumn string // aggregated column (for SUM/MIN/MAX)

	// cardinalityColumn, when non-empty, makes this a CARDINALITY() value
	// index: the key is FunctionExpr("cardinality", field(col, Concatenate))
	// over the named array column (possibly dotted, e.g. "struct.int_arr").
	// Mutually exclusive with aggType/vector. Mirrors Java's value index whose
	// root is a CardinalityFunctionKeyExpression.
	cardinalityColumn string

	// fanOutColumn, when non-empty, makes this a VALUE index with a direct
	// FieldKeyExpression in FAN_OUT mode: one index entry per array element.
	fanOutColumn string

	// VECTOR (HNSW) index fields — set only when vector is true.
	vector           bool
	vectorMethod     string
	vectorColumn     string            // the indexed vector field
	partitionColumns []string          // HNSW partition prefix (independent graph per partition)
	numDimensions    int               // derived from the column's VECTOR type
	options          map[string]string // HNSW tuning options (metric, ef_construction, m, ...)

	// rootExpression, when non-nil, makes this an EXPLICIT index: the key
	// expression, type, options and predicate were produced by an index
	// generator (RFC-202's MaterializedViewIndexGenerator port) rather than
	// re-derived from column names at Build() time. Build() short-circuits to
	// recordlayer.NewIndex(name, rootExpression) plus indexType / options /
	// predicate / unique. Mirrors Java, where RecordLayerIndex always carries
	// the generator's KeyExpression verbatim and the builder never re-derives
	// shape from names.
	rootExpression recordlayer.KeyExpression
	indexType      string
	predicate      *gen.Predicate
}

// ColumnSpec describes a single column within a table.
type ColumnSpec struct {
	name     string
	dt       api.DataType
	fieldNum int32
}

// NewColumnSpec constructs a ColumnSpec for use with Builder.AddTable.
func NewColumnSpec(name string, dt api.DataType, fieldNum int32) ColumnSpec {
	return ColumnSpec{name: name, dt: dt, fieldNum: fieldNum}
}

// Name returns the column's declared (normalized) name.
func (c ColumnSpec) Name() string { return c.name }

// DataType returns the column's declared type.
func (c ColumnSpec) DataType() api.DataType { return c.dt }

// NewSchemaTemplateBuilder returns a Builder with sensible defaults
// (enableLongRows=true, version=1). Matches Java's default.
func NewSchemaTemplateBuilder() *Builder {
	return &Builder{
		version:        1,
		enableLongRows: true,
	}
}

func (b *Builder) SetName(name string) *Builder {
	b.name = name
	return b
}

func (b *Builder) SetVersion(v int) *Builder {
	b.version = v
	return b
}

func (b *Builder) SetIntermingleTables(v bool) *Builder {
	b.intermingleTbls = v
	return b
}

func (b *Builder) SetEnableLongRows(v bool) *Builder {
	b.enableLongRows = v
	return b
}

func (b *Builder) SetStoreRowVersions(v bool) *Builder {
	b.storeRowVersions = v
	return b
}

// AddTable registers a table definition. columns must be listed in
// declared order; primaryKey is the ordered slice of column names — a
// DOTTED element (id.a) is the nested-column convention, split into path
// segments here (see AddTablePrimaryKeyPaths for the unambiguous form a
// quoted identifier carrying a literal '.' needs).
//
// The name check is RECIPROCAL with AddAuxiliaryType's: Java's
// Builder.addTable calls the same verifyNameIsNotUsed
// (RecordLayerSchemaTemplate.java:465), so a table colliding with an
// already-registered struct type is rejected whichever side is seen first.
// Without it a `CREATE TYPE AS STRUCT s ... CREATE TABLE s ...` template
// builds two descriptors named s, one silently shadowing the other.
func (b *Builder) AddTable(name string, columns []ColumnSpec, primaryKey []string) *Builder {
	paths := make([][]string, len(primaryKey))
	for i, col := range primaryKey {
		paths[i] = strings.Split(col, ".")
	}
	return b.AddTablePrimaryKeyPaths(name, columns, paths)
}

// AddTablePrimaryKeyPaths registers a table whose primary key parts are
// given as PATH SEGMENTS rather than dotted strings — Java's
// RecordLayerTable.Builder.addPrimaryKeyPart, which takes a List<String>
// (RecordLayerTable.java:295) fed from Identifier.fullyQualifiedName
// (DdlVisitor.java:183-188). A joined string cannot tell the nested path
// id.a from the single quoted column "a.b"; the segment list can.
func (b *Builder) AddTablePrimaryKeyPaths(name string, columns []ColumnSpec, primaryKey [][]string) *Builder {
	if err := b.verifyNameIsNotUsed(name); err != nil {
		b.errs = append(b.errs, err)
		return b
	}
	b.tables = append(b.tables, tableSpec{name: name, columns: columns, primaryKey: primaryKey})
	return b
}

// AddIndex registers a VALUE index on the named table. columns is the ordered
// list of field names that form the index key. unique causes uniqueness
// enforcement to be wired into the recordlayer index.
// Must be called after the table is registered via AddTable.
// Returns the builder unchanged (and records a deferred error) if the table
// name is unknown or any column name is not present in the table definition.
func (b *Builder) AddIndex(tableName, indexName string, columns []string, unique bool) *Builder {
	for i := range b.tables {
		if b.tables[i].name != tableName {
			continue
		}
		// Validate every index column exists in the table.
		colSet := make(map[string]bool, len(b.tables[i].columns))
		for _, c := range b.tables[i].columns {
			colSet[c.name] = true
		}
		for _, col := range columns {
			if !colSet[col] {
				b.errs = append(b.errs, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
					"index %q on table %q: column %q not defined in table",
					indexName, tableName, col))
				return b
			}
		}
		b.tables[i].indexes = append(b.tables[i].indexes, indexSpec{
			name:    indexName,
			columns: columns,
			unique:  unique,
		})
		return b
	}
	b.errs = append(b.errs, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
		"index %q references unknown table %q", indexName, tableName))
	return b
}

// AddGeneratedIndex registers an index whose key expression, type, options and
// predicate were fully determined by an index generator (RFC-202). The builder
// stores the description verbatim and Build() materialises it without any
// name-based re-derivation — mirroring Java, where the DdlVisitor hands the
// generator's RecordLayerIndex straight to the schema-template builder.
//
// rootExpression must be non-nil. indexType "" means VALUE. options may be
// nil. predicate may be nil (a full, non-sparse index).
func (b *Builder) AddGeneratedIndex(tableName, indexName string, rootExpression recordlayer.KeyExpression,
	indexType string, unique bool, options map[string]string, predicate *gen.Predicate,
) *Builder {
	if rootExpression == nil {
		b.errs = append(b.errs, api.NewErrorf(api.ErrCodeInternalError,
			"generated index %q on table %q: nil root expression", indexName, tableName))
		return b
	}
	for i := range b.tables {
		if b.tables[i].name != tableName {
			continue
		}
		b.tables[i].indexes = append(b.tables[i].indexes, indexSpec{
			name:           indexName,
			unique:         unique,
			rootExpression: rootExpression,
			indexType:      indexType,
			options:        options,
			predicate:      predicate,
		})
		return b
	}
	b.errs = append(b.errs, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
		"index %q references unknown table %q", indexName, tableName))
	return b
}

// AddAggregateIndex registers an aggregate index (SUM, COUNT, MIN, MAX)
// on the named table. groupColumns are the GROUP BY columns; aggColumn
// is the aggregated column (empty for COUNT). aggType is one of "SUM",
// "COUNT", "MIN", "MAX".
func (b *Builder) AddAggregateIndex(tableName, indexName string, groupColumns []string, aggType, aggColumn string) *Builder {
	for i := range b.tables {
		if b.tables[i].name != tableName {
			continue
		}
		colSet := make(map[string]bool, len(b.tables[i].columns))
		for _, c := range b.tables[i].columns {
			colSet[c.name] = true
		}
		for _, col := range groupColumns {
			if !colSet[col] {
				b.errs = append(b.errs, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
					"aggregate index %q on table %q: grouping column %q not defined",
					indexName, tableName, col))
				return b
			}
		}
		if aggColumn != "" && !colSet[aggColumn] {
			b.errs = append(b.errs, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
				"aggregate index %q on table %q: aggregate column %q not defined",
				indexName, tableName, aggColumn))
			return b
		}
		b.tables[i].indexes = append(b.tables[i].indexes, indexSpec{
			name:      indexName,
			columns:   groupColumns,
			aggType:   aggType,
			aggColumn: aggColumn,
		})
		return b
	}
	b.errs = append(b.errs, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
		"aggregate index %q references unknown table %q", indexName, tableName))
	return b
}

// AddCardinalityIndex registers a CARDINALITY() value index on the named table.
// cardColumn is the array column whose element count forms the single key
// column (possibly dotted for an array nested in a struct, e.g.
// "struct.int_arr"). The index key is built as
// FunctionExpr("cardinality", field(col, Concatenate)) at materialise time.
// Must be called after the table is registered via AddTable.
func (b *Builder) AddCardinalityIndex(tableName, indexName, cardColumn string) *Builder {
	for i := range b.tables {
		if b.tables[i].name != tableName {
			continue
		}
		// Validate the top-level column exists. A dotted reference
		// (struct.field) is validated by its head segment here; deeper
		// resolution happens when the key expression is built/evaluated.
		head := cardColumn
		if dot := strings.IndexByte(head, '.'); dot >= 0 {
			head = head[:dot]
		}
		found := false
		for _, c := range b.tables[i].columns {
			if c.name == head {
				found = true
				break
			}
		}
		if !found {
			b.errs = append(b.errs, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
				"cardinality index %q on table %q: column %q not defined",
				indexName, tableName, head))
			return b
		}
		b.tables[i].indexes = append(b.tables[i].indexes, indexSpec{
			name:              indexName,
			cardinalityColumn: cardColumn,
		})
		return b
	}
	b.errs = append(b.errs, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
		"cardinality index %q references unknown table %q", indexName, tableName))
	return b
}

// AddFanOutIndex registers a VALUE index that emits one key per element of the
// named direct array column. It is the programmatic metadata counterpart of
// Java's `field(column, FAN_OUT)` index root. Nested and composite fanout
// expressions continue to use the lower-level RecordMetaData API.
func (b *Builder) AddFanOutIndex(tableName, indexName, column string) *Builder {
	for i := range b.tables {
		if b.tables[i].name != tableName {
			continue
		}
		found := false
		isArray := false
		for _, c := range b.tables[i].columns {
			if c.name == column {
				found = true
				isArray = c.dt != nil && c.dt.Code() == api.CodeArray
				break
			}
		}
		if !found {
			b.errs = append(b.errs, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
				"fanout index %q on table %q: column %q not defined",
				indexName, tableName, column))
			return b
		}
		if !isArray {
			b.errs = append(b.errs, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
				"fanout index %q on table %q: column %q is not an array",
				indexName, tableName, column))
			return b
		}
		b.tables[i].indexes = append(b.tables[i].indexes, indexSpec{
			name:         indexName,
			fanOutColumn: column,
		})
		return b
	}
	b.errs = append(b.errs, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
		"fanout index %q references unknown table %q", indexName, tableName))
	return b
}

// AddVectorIndex registers a VECTOR (HNSW) index on the named table.
// vectorColumn is the single indexed vector field; partitionColumns form
// the HNSW partition prefix (each distinct partition value gets its own
// independent graph). options carries the HNSW tuning keys (metric,
// ef_construction, m, ...). The vector column must be declared with a
// VECTOR(dims, ...) type; the dimension count is derived from it.
//
// Mirrors Java's DdlVisitor.visitVectorIndexDefinition, which requires
// exactly one indexed column of VECTOR type and derives
// HNSW_NUM_DIMENSIONS from the column's VectorType.
// AddVectorIndex registers an HNSW vector index (the default method).
func (b *Builder) AddVectorIndex(tableName, indexName, vectorColumn string, partitionColumns []string, options map[string]string) *Builder {
	return b.AddVectorIndexUsing("HNSW", tableName, indexName, vectorColumn, partitionColumns, options)
}

// AddVectorIndexUsing registers a vector index with an explicit method:
// "HNSW" (graph, Java-compatible wire format) or "SPFRESH" (RFC-094
// centroid+posting-list, Go-native). SPFresh does not support PARTITION BY.
func (b *Builder) AddVectorIndexUsing(method, tableName, indexName, vectorColumn string, partitionColumns []string, options map[string]string) *Builder {
	// The method is case-sensitive everywhere downstream (buildVectorIndex
	// treats anything that is not "SPFRESH" as HNSW), so an unknown or
	// mis-cased method must fail loudly here — AddVectorIndexUsing("SPFresh",
	// …) silently building an HNSW index is exactly the kind of quiet
	// misroute a schema author cannot debug.
	if method != "HNSW" && method != "SPFRESH" {
		b.errs = append(b.errs, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
			"vector index %q: unknown method %q (want HNSW or SPFRESH)", indexName, method))
		return b
	}
	if method == "SPFRESH" && len(partitionColumns) > 0 {
		b.errs = append(b.errs, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
			"vector index %q: PARTITION BY is not supported with USING SPFRESH", indexName))
		return b
	}
	for i := range b.tables {
		if b.tables[i].name != tableName {
			continue
		}
		colByName := make(map[string]ColumnSpec, len(b.tables[i].columns))
		for _, c := range b.tables[i].columns {
			colByName[c.name] = c
		}
		vc, ok := colByName[vectorColumn]
		if !ok {
			b.errs = append(b.errs, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
				"vector index %q on table %q: column %q not defined in table",
				indexName, tableName, vectorColumn))
			return b
		}
		vt, ok := vc.dt.(*api.VectorType)
		if !ok {
			b.errs = append(b.errs, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
				"vector index %q: indexed column %q must be of vector type",
				indexName, vectorColumn))
			return b
		}
		for _, pc := range partitionColumns {
			if _, ok := colByName[pc]; !ok {
				b.errs = append(b.errs, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
					"vector index %q on table %q: partition column %q not defined",
					indexName, tableName, pc))
				return b
			}
		}
		b.tables[i].indexes = append(b.tables[i].indexes, indexSpec{
			name:             indexName,
			vector:           true,
			vectorMethod:     method,
			vectorColumn:     vectorColumn,
			partitionColumns: partitionColumns,
			numDimensions:    vt.Dimensions(),
			options:          options,
		})
		return b
	}
	b.errs = append(b.errs, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
		"vector index %q references unknown table %q", indexName, tableName))
	return b
}

// Build materialises the schema template. Returns an error when no
// tables are registered or types cannot be mapped to proto field types.
func (b *Builder) Build() (*RecordLayerSchemaTemplate, error) {
	if len(b.errs) > 0 {
		return nil, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate, "%v", b.errs[0])
	}
	if len(b.tables) == 0 {
		return nil, api.NewError(api.ErrCodeInvalidSchemaTemplate, "schema template contains no tables")
	}
	if b.name == "" {
		return nil, api.NewError(api.ErrCodeInvalidSchemaTemplate, "schema template name is required")
	}

	// Resolve forward type references (CREATE TYPE AS STRUCT used before
	// declaration) before any descriptor is emitted — Java's build() runs
	// resolveTypes when any table or auxiliary type is unresolved.
	if b.needsTypeResolution() {
		if rerr := b.resolveTypes(); rerr != nil {
			return nil, rerr
		}
	}

	fd, fdp, containsNullableArray, err := b.buildFileDescriptor()
	if err != nil {
		return nil, api.WrapErrorf(err, api.ErrCodeInvalidSchemaTemplate, "build file descriptor")
	}

	// The union is Java's relational shape: a "RecordTypeUnion" message with
	// the usage=UNION option (FileDescriptorSerializer.java:77-79). The
	// ORIGINAL FileDescriptorProto is retained so RecordMetaData.ToProto()
	// emits Java's exact bytes (relative type names, no json_name) instead of
	// the protodesc.ToFileDescriptorProto normalization.
	mdBuilder := recordlayer.NewRecordMetaDataBuilder().SetRecordsWithUnionName(fd, relationalUnionName)
	mdBuilder.SetRecordsSourceProto(fdp)
	mdBuilder.SetSplitLongRecords(b.enableLongRows)
	mdBuilder.SetStoreRecordVersions(b.storeRowVersions)
	mdBuilder.SetVersion(b.version)
	// NO record count key: the stored template bytes must match Java's, and
	// Java's RecordMetadataSerializer never sets one — Java core marks
	// getRecordCountKey @API(DEPRECATED), superseded by COUNT-type indexes.
	// (Setting one here also bumped the metadata version past Java's.)
	// Byte-golden-pinned. The cost model's per-type row counts went with
	// it; the COUNT-index + CardinalitiesProperty replacement is booked in
	// TODO.md.

	for tableIdx, tbl := range b.tables {
		// Record type names are STORAGE names (Java: the Type.Record storage
		// name, ProtoUtils.toProtoBufCompliantName of the user name — they
		// differ only for quoted identifiers carrying '$', '.' or "__").
		storageName, nameErr := recordlayer.ToProtoBufCompliantName(tbl.name)
		if nameErr != nil {
			return nil, api.WrapErrorf(nameErr, api.ErrCodeInvalidSchemaTemplate,
				"table %q", tbl.name)
		}
		// buildFileDescriptor() derives the proto message types from b.tables, so
		// every table storage name should be present after SetRecords. Check
		// existence via the nil-returning GetRecordTypes() map rather than
		// GetRecordType(), which panics on a missing type: a name mismatch here is
		// a descriptor-build bug, and Build() (which already returns an error)
		// must surface it as a typed error — not a panic that the db/sql boundary
		// recover turns into a generic "internal error" with no context.
		if mdBuilder.GetRecordTypes()[storageName] == nil {
			return nil, api.NewErrorf(api.ErrCodeInternalError,
				"record type %q not found after SetRecords", storageName)
		}
		rt := mdBuilder.GetRecordType(storageName)
		// Explicit record type key = 0-based declaration index, exactly
		// Java's RecordMetadataSerializer visit(Table):
		// setRecordTypeKey(recordTypeCounter++). Stored metadata (and the
		// record-store key prefix for non-intermingled tables), so it must
		// match byte-for-byte.
		rt.SetRecordTypeKey(int64(tableIdx))
		// Index key expressions must match the stored descriptor shape: with
		// the NullableArrayWrapper emitted, any field path through a nullable
		// array gains the `values` hop (Java's NullableArrayUtils.wrapArray,
		// applied by both of its index generators). Applied uniformly to
		// every index root right before registration.
		rtDesc := mdBuilder.GetRecordTypes()[storageName].Descriptor
		addIndex := func(rl *recordlayer.Index) error {
			if containsNullableArray {
				wrapped, werr := recordlayer.KeyExpressionFromProto(
					wrapArrayInternal(rl.RootExpression.ToKeyExpression(), rtDesc))
				if werr != nil {
					return api.WrapErrorf(werr, api.ErrCodeInvalidSchemaTemplate,
						"table %q index %q: nullable-array wrap", tbl.name, rl.Name)
				}
				rl.RootExpression = wrapped
			}
			mdBuilder.AddIndex(storageName, rl)
			return nil
		}
		pkExpr, err := buildPrimaryKeyExpression(tbl, b.intermingleTbls)
		if err != nil {
			return nil, api.WrapErrorf(err, api.ErrCodeInvalidSchemaTemplate,
				"table %q primary key", tbl.name)
		}
		rt.SetPrimaryKey(pkExpr)

		for _, idx := range tbl.indexes {
			if idx.rootExpression != nil {
				rl := recordlayer.NewIndex(idx.name, idx.rootExpression)
				if idx.indexType != "" {
					rl.Type = idx.indexType
				}
				for k, v := range idx.options {
					rl.Options[k] = v
				}
				// Java's generator builder ALWAYS writes the unique option —
				// setUnique(isUnique) stores "true"/"false" alike
				// (RecordLayerIndex.java:216-218, called unconditionally by
				// both generators, MaterializedViewIndexGenerator.java:157 /
				// OnSourceIndexGenerator's builder). An omitted-when-false
				// option is a stored-metadata divergence the D11 cross-engine
				// comparison catches: Java's index carries unique=false where
				// Go's carried nothing.
				rl.Options[recordlayer.IndexOptionUnique] = strconv.FormatBool(idx.unique)
				if idx.predicate != nil {
					if perr := rl.SetPredicateProto(idx.predicate); perr != nil {
						return nil, api.WrapErrorf(perr, api.ErrCodeInvalidSchemaTemplate,
							"table %q index %q predicate", tbl.name, idx.name)
					}
				}
				if aerr := addIndex(rl); aerr != nil {
					return nil, aerr
				}
				continue
			}
			if idx.vector {
				rl, idxErr := buildVectorIndex(idx)
				if idxErr != nil {
					return nil, api.WrapErrorf(idxErr, api.ErrCodeInvalidSchemaTemplate,
						"table %q vector index %q", tbl.name, idx.name)
				}
				if aerr := addIndex(rl); aerr != nil {
					return nil, aerr
				}
				continue
			}
			if idx.aggType != "" {
				rl, idxErr := buildAggregateIndex(idx)
				if idxErr != nil {
					return nil, api.WrapErrorf(idxErr, api.ErrCodeInvalidSchemaTemplate,
						"table %q aggregate index %q", tbl.name, idx.name)
				}
				if aerr := addIndex(rl); aerr != nil {
					return nil, aerr
				}
				continue
			}
			if idx.cardinalityColumn != "" {
				rl, idxErr := buildCardinalityIndex(idx)
				if idxErr != nil {
					return nil, api.WrapErrorf(idxErr, api.ErrCodeInvalidSchemaTemplate,
						"table %q cardinality index %q", tbl.name, idx.name)
				}
				if idx.unique {
					rl.SetUnique()
				}
				if aerr := addIndex(rl); aerr != nil {
					return nil, aerr
				}
				continue
			}
			if idx.fanOutColumn != "" {
				if aerr := addIndex(recordlayer.NewIndex(idx.name, recordlayer.FanOut(idx.fanOutColumn))); aerr != nil {
					return nil, aerr
				}
				continue
			}
			keyExpr, idxErr := buildIndexKeyExpression(idx.columns)
			if idxErr != nil {
				return nil, api.WrapErrorf(idxErr, api.ErrCodeInvalidSchemaTemplate,
					"table %q index %q", tbl.name, idx.name)
			}
			rl := recordlayer.NewIndex(idx.name, keyExpr)
			if idx.unique {
				rl.SetUnique()
			}
			if aerr := addIndex(rl); aerr != nil {
				return nil, aerr
			}
		}
	}

	md, err := mdBuilder.Build()
	if err != nil {
		return nil, api.WrapErrorf(err, api.ErrCodeInternalError, "build RecordMetaData")
	}

	return NewRecordLayerSchemaTemplateWithVersion(b.name, md, b.version)
}

// uuidProtoTypeName is the RELATIVE fully-qualified proto message name for
// the tuple_fields.UUID record (sfixed64 most/least bits) — no leading dot,
// exactly as Java's Type.Uuid.addProtoField stores it. Matches Java's
// Type.uuidType lowering; fdb-relational stores UUID column values as
// TupleFieldsProto.UUID instances.
const uuidProtoTypeName = "com.apple.foundationdb.record.UUID"

// relationalUnionName is the union message name Java's relational layer
// writes ("RecordTypeUnion" with the usage=UNION option —
// FileDescriptorSerializer's unionDescriptorBuilder).
const relationalUnionName = "RecordTypeUnion"

// buildFileDescriptor constructs the schema template's file descriptor in
// Java's EXACT stored shape (FileDescriptorSerializer +
// Type.Record.defineProtoType / Type.addProtoField), because the catalog
// persists these bytes (RecordMetaData.toProto().records) and a Java client
// must open a Go-created template byte-for-byte. The measured contract
// (live-JVM probe, pinned by the RFC-204 conformance byte-goldens):
//
//   - file name = template name verbatim (no ".proto", no syntax field);
//   - dependencies = "tuple_fields.proto", "record_metadata_options.proto";
//   - per TABLE (declaration order), the closure of message types the table
//     needs — the table message, struct types, nullable-array wrappers — is
//     appended in ALPHABETICAL name order (Java iterates a TreeSet,
//     TypeRepository.getMessageTypes), deduplicated template-wide by name
//     (a struct shared by two tables is emitted once, under the FIRST
//     table's batch);
//   - every non-array field is LABEL_OPTIONAL regardless of SQL nullability
//     (Type.Record.defineProtoType passes LABEL_OPTIONAL always; Java has
//     no way to express non-null scalars in RecordMetaData — the
//     RecordLayerTable.calculateDataType TODO);
//   - message-typed fields carry only type_name (no explicit TYPE_MESSAGE),
//     with RELATIVE names ("S1", "com.apple.foundationdb.record.UUID");
//   - a NON-nullable array is a flat LABEL_REPEATED field; a NULLABLE array
//     is an OPTIONAL message field referencing a synthetic single-field
//     wrapper `message W { repeated E values = 1; }` (NullableArrayWrapper);
//   - the union message "RecordTypeUnion" comes last, usage=UNION option,
//     one field per table named "<TYPE>_0" (generation 0), numbered by
//     declaration order, TYPE_MESSAGE + relative type_name + empty
//     FieldOptions and NO label.
//
// The second return value is the ORIGINAL FileDescriptorProto, retained so
// RecordMetaData.ToProto() emits these bytes verbatim instead of the
// protodesc.ToFileDescriptorProto normalization (which absolutizes type
// names and adds json_name — both byte divergences vs Java). The third is
// Java's containsNullableArray flag (DdlVisitor accumulates it over column
// definitions): whether any NullableArrayWrapper was emitted, gating the
// index key-expression wrap pass.
func (b *Builder) buildFileDescriptor() (protoreflect.FileDescriptor, *descriptorpb.FileDescriptorProto, bool, error) {
	fdp := &descriptorpb.FileDescriptorProto{}
	fdp.Name = proto.String(b.name)
	fdp.Dependency = []string{
		gen.File_tuple_fields_proto.Path(),
		gen.File_record_metadata_options_proto.Path(),
	}

	unionOpts := &descriptorpb.MessageOptions{}
	proto.SetExtension(unionOpts, gen.E_Record, &gen.RecordTypeOptions{
		Usage: gen.RecordTypeOptions_UNION.Enum(),
	})
	unionMsg := &descriptorpb.DescriptorProto{
		Name:    proto.String(relationalUnionName),
		Options: unionOpts,
	}

	em := &fileEmitter{
		seen:        map[string]bool{},
		structTypes: map[string]*api.StructType{},
	}
	for i, tbl := range b.tables {
		storageName, err := em.emitTableClosure(tbl)
		if err != nil {
			return nil, nil, false, api.WrapErrorf(err, api.ErrCodeInvalidSchemaTemplate,
				"table %q", tbl.name)
		}
		// One union entry per table generation; DDL-built templates have
		// exactly generation 0. Java sets type AND type_name here (unlike
		// ordinary message fields, which carry only type_name), an empty
		// FieldOptions, and no label — measured against the live JVM.
		unionMsg.Field = append(unionMsg.Field, &descriptorpb.FieldDescriptorProto{
			Name:     proto.String(storageName + "_0"),
			Number:   proto.Int32(int32(i + 1)), //nolint:gosec
			Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
			TypeName: proto.String(storageName),
			Options:  &descriptorpb.FieldOptions{},
		})
	}
	fdp.MessageType = append(em.messages, unionMsg)

	// Build a resolver that includes the two dependency files.
	// RegisterFile returns an error on duplicate registration; ignore it since
	// the global registry already has these files and we just want them
	// available to the local resolver.
	resolver := &protoregistry.Files{}
	_ = resolver.RegisterFile(gen.File_tuple_fields_proto)
	_ = resolver.RegisterFile(gen.File_record_metadata_options_proto)

	// The RETAINED proto keeps Java's RELATIVE type_name references
	// ("com.apple.foundationdb.record.UUID"); protodesc's resolver wants
	// absolute names, so the in-memory descriptor builds from an
	// absolutized CLONE — the same bridge FromProto applies to
	// Java-authored files (recordlayer.AbsolutizeFieldTypeNames).
	buildable := proto.Clone(fdp).(*descriptorpb.FileDescriptorProto)
	recordlayer.AbsolutizeFieldTypeNames(buildable)
	fd, err := protodesc.NewFile(buildable, resolver)
	if err != nil {
		return nil, nil, false, api.WrapErrorf(err, api.ErrCodeInternalError, "protodesc.NewFile")
	}
	return fd, fdp, em.containsNullableArray, nil
}

// fileEmitter accumulates the template's top-level messages with
// template-wide name deduplication — the Go counterpart of Java's
// FileDescriptorSerializer.registerTypeDescriptors' descriptorNames set.
type fileEmitter struct {
	messages []*descriptorpb.DescriptorProto
	seen     map[string]bool
	// structTypes enforces one-storage-name-one-shape template-wide. The
	// comparison normalizes the struct's OWN nullability away: a struct
	// column's nullability lives on the referencing column, not the shared
	// descriptor (Java's nullability collapse, the
	// RecordLayerTable.calculateDataType TODO — nullable and non-nullable
	// uses of one named struct share ONE descriptor; reproduced, not fixed,
	// because the collapsed descriptor is the wire shape).
	structTypes map[string]*api.StructType
	// containsNullableArray: any NullableArrayWrapper emitted (Java's
	// DdlVisitor.containsNullableArray equivalent, gating wrapArray).
	containsNullableArray bool
}

// emitTableClosure builds the table's message-type closure in a per-table
// namespace (Java: one TypeRepository per table visit), sorts it
// alphabetically, and appends the not-yet-seen names to the file. Returns
// the table's storage name.
//
// The per-table scope is measured Java behavior, not an approximation: a
// nullable array inside a struct SHARED by two tables makes Java's second
// table define a fresh wrapper (its repo cannot see the first's), whose
// message is then copied into the file even though the deduplicated struct
// still references the first wrapper — an orphan message. Byte compat means
// reproducing that, so wrapper identity is deliberately per-table.
func (em *fileEmitter) emitTableClosure(tbl tableSpec) (string, error) {
	storageName, err := recordlayer.ToProtoBufCompliantName(tbl.name)
	if err != nil {
		return "", api.WrapErrorf(err, api.ErrCodeInvalidSchemaTemplate, "table name %q", tbl.name)
	}
	r := &tableTypeRepo{
		em:        em,
		tableName: storageName,
		msgs:      map[string]*descriptorpb.DescriptorProto{},
		wrappers:  map[string]string{},
	}
	msg := &descriptorpb.DescriptorProto{Name: proto.String(storageName)}
	// Register before walking fields: a column typed as its own table (legal
	// to EXPRESS — cycles die later in type resolution) must not recurse
	// forever here.
	r.msgs[storageName] = msg
	for _, col := range tbl.columns {
		if err := r.addField(msg, col.name, col.fieldNum, col.dt); err != nil {
			return "", api.WrapErrorf(err, api.ErrCodeInvalidSchemaTemplate,
				"column %q", col.name)
		}
	}

	names := make([]string, 0, len(r.msgs))
	for n := range r.msgs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if em.seen[n] {
			continue
		}
		em.seen[n] = true
		em.messages = append(em.messages, r.msgs[n])
	}
	return storageName, nil
}

// tableTypeRepo is the per-table type namespace: the closure of message
// descriptors this table's type needs, plus wrapper dedup by element type
// (Java: TypeRepository's Type->name BiMap makes equal Array types share
// one wrapper within a repo).
type tableTypeRepo struct {
	em        *fileEmitter
	tableName string // wrapper-name derivation input (per-table wrapper identity)
	msgs      map[string]*descriptorpb.DescriptorProto
	wrappers  map[string]string // element-type signature -> wrapper message name
}

// addField appends one field to msg following Java's addProtoField
// dispatch: LABEL_OPTIONAL for everything except flat non-nullable arrays
// (LABEL_REPEATED) — nullability of scalars/structs does NOT reach the
// label.
func (r *tableTypeRepo) addField(msg *descriptorpb.DescriptorProto, userName string, number int32, dt api.DataType) error {
	fieldName, err := recordlayer.ToProtoBufCompliantName(userName)
	if err != nil {
		return api.WrapErrorf(err, api.ErrCodeInvalidSchemaTemplate, "field name %q", userName)
	}
	f := &descriptorpb.FieldDescriptorProto{
		Name:   proto.String(fieldName),
		Number: proto.Int32(number),
	}
	if at, ok := dt.(*api.ArrayType); ok {
		if at.IsNullable() {
			wrapperName, werr := r.wrapperFor(at.ElementType())
			if werr != nil {
				return werr
			}
			f.Label = descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
			f.TypeName = proto.String(wrapperName)
		} else {
			if serr := r.setFieldType(f, at.ElementType()); serr != nil {
				return serr
			}
			f.Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
		}
	} else {
		if serr := r.setFieldType(f, dt); serr != nil {
			return serr
		}
		f.Label = descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	}
	msg.Field = append(msg.Field, f)
	return nil
}

// setFieldType fills the field's type or type_name (never both for message
// kinds — Java's Record/Uuid addProtoField set only type_name; the union
// entry is the one deliberate exception, handled by the caller).
func (r *tableTypeRepo) setFieldType(f *descriptorpb.FieldDescriptorProto, dt api.DataType) error {
	setScalar := func(t descriptorpb.FieldDescriptorProto_Type) {
		f.Type = t.Enum()
	}
	switch dt.Code() {
	case api.CodeBoolean:
		setScalar(descriptorpb.FieldDescriptorProto_TYPE_BOOL)
	case api.CodeInteger:
		setScalar(descriptorpb.FieldDescriptorProto_TYPE_INT32)
	case api.CodeLong:
		setScalar(descriptorpb.FieldDescriptorProto_TYPE_INT64)
	case api.CodeFloat:
		setScalar(descriptorpb.FieldDescriptorProto_TYPE_FLOAT)
	case api.CodeDouble:
		setScalar(descriptorpb.FieldDescriptorProto_TYPE_DOUBLE)
	case api.CodeString:
		setScalar(descriptorpb.FieldDescriptorProto_TYPE_STRING)
	case api.CodeBytes:
		setScalar(descriptorpb.FieldDescriptorProto_TYPE_BYTES)
	case api.CodeDate, api.CodeTimestamp:
		// Go extension (Java's relational grammar has no DATE/TIMESTAMP
		// primitive); stored as ISO strings.
		setScalar(descriptorpb.FieldDescriptorProto_TYPE_STRING)
	case api.CodeVector:
		// TYPE_BYTES plus the vectorOptions field extension carrying
		// precision and dimensions — measured Java shape
		// ([com.apple.foundationdb.record.field] { vectorOptions {...} }).
		vt := dt.(*api.VectorType)
		setScalar(descriptorpb.FieldDescriptorProto_TYPE_BYTES)
		opts := &descriptorpb.FieldOptions{}
		proto.SetExtension(opts, gen.E_Field, &gen.FieldOptions{
			VectorOptions: &gen.FieldOptions_VectorOptions{
				Precision:  proto.Int32(int32(vt.Precision())),  //nolint:gosec
				Dimensions: proto.Int32(int32(vt.Dimensions())), //nolint:gosec
			},
		})
		f.Options = opts
	case api.CodeUUID:
		// Java emits the RELATIVE fully-qualified name (no leading dot) and
		// no explicit TYPE_MESSAGE — measured.
		f.TypeName = proto.String(uuidProtoTypeName)
	case api.CodeStruct:
		st := dt.(*api.StructType)
		name, err := r.defineStruct(st)
		if err != nil {
			return err
		}
		f.TypeName = proto.String(name)
	case api.CodeArray:
		// An array element that is itself an array — inexpressible in the
		// SQL grammar (columnDefinition has a single ARRAY suffix).
		return api.NewError(api.ErrCodeUnsupportedOperation,
			"array-of-array column types are not supported")
	default:
		return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
			"unsupported DataType code %v", dt.Code())
	}
	return nil
}

// defineStruct registers the struct's message descriptor (and recursively
// its field types) in the per-table namespace, returning its storage name.
// Mirrors Type.Record.defineProtoType: field numbers are declaration index
// + 1, every field LABEL_OPTIONAL unless an array.
func (r *tableTypeRepo) defineStruct(st *api.StructType) (string, error) {
	if st.Name() == "" {
		return "", api.NewError(api.ErrCodeInvalidSchemaTemplate,
			"struct column type must be named")
	}
	storage, err := recordlayer.ToProtoBufCompliantName(st.Name())
	if err != nil {
		return "", api.WrapErrorf(err, api.ErrCodeInvalidSchemaTemplate, "struct name %q", st.Name())
	}
	// One name, one shape — template-wide. Nullability normalized away (the
	// collapse described on fileEmitter.structTypes). Identity short-circuits
	// first so a self-referential struct re-entering mid-build is accepted
	// rather than mis-flagged against a still-empty descriptor.
	if prev, seenShape := r.em.structTypes[storage]; seenShape {
		if !(prev == st || prev.WithNullable(false).Equal(st.WithNullable(false))) {
			return "", api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
				"struct type %q declared twice with different shapes", st.Name())
		}
	} else {
		r.em.structTypes[storage] = st
	}
	if _, ok := r.msgs[storage]; ok {
		return storage, nil
	}
	msg := &descriptorpb.DescriptorProto{Name: proto.String(storage)}
	r.msgs[storage] = msg // register BEFORE recursing (self-reference guard)
	for i := 0; i < st.NumFields(); i++ {
		fld := st.Field(i)
		if err := r.addField(msg, fld.Name(), int32(fld.Index()+1), fld.Type()); err != nil { //nolint:gosec
			return "", api.WrapErrorf(err, api.ErrCodeInvalidSchemaTemplate,
				"struct %q field %q", st.Name(), fld.Name())
		}
	}
	return storage, nil
}

// wrapperFor returns (defining on first use) the per-table NullableArrayWrapper
// message for the given element type: message W { repeated E values = 1; }.
//
// NAME divergence, deliberate and documented: Java names the wrapper
// ProtoUtils.uniqueTypeName() = "__type__" + UUID.randomUUID() — a fresh
// RANDOM name on every serialization (measured: two identical templates get
// different wrapper names). Every reader is structural
// (NullableArrayUtils.isWrappedArrayDescriptor checks shape, never the
// name), so any name is wire-valid; Go keeps Java's exact FORMAT but
// derives the UUID deterministically from (table, element type) so that
// identical DDL produces identical bytes. The byte-goldens compare with
// wrapper names normalized on both sides — Java's own nondeterminism makes
// literal equality on this one token unpinnable.
func (r *tableTypeRepo) wrapperFor(elem api.DataType) (string, error) {
	sig := elem.String()
	r.em.containsNullableArray = true
	if name, ok := r.wrappers[sig]; ok {
		return name, nil
	}
	name := "__type__" + strings.ReplaceAll(
		uuid.NewSHA1(uuid.NameSpaceOID, []byte(r.tableName+"\x00"+sig)).String(), "-", "_")
	r.wrappers[sig] = name
	msg := &descriptorpb.DescriptorProto{Name: proto.String(name)}
	vf := &descriptorpb.FieldDescriptorProto{
		Name:   proto.String(wrappedArrayFieldName),
		Number: proto.Int32(1),
		Label:  descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
	}
	if err := r.setFieldType(vf, elem); err != nil {
		return "", err
	}
	msg.Field = append(msg.Field, vf)
	r.msgs[name] = msg
	return name, nil
}

// buildPrimaryKeyExpression builds the record layer primary key expression.
// In intermingled mode it's just the column fields; in non-intermingled mode
// a RecordType prefix is prepended (matching Java).
func buildIndexKeyExpression(columns []string) (recordlayer.KeyExpression, error) {
	if len(columns) == 0 {
		return nil, api.NewError(api.ErrCodeInvalidSchemaTemplate,
			"index must have at least one column")
	}
	if len(columns) == 1 {
		return recordlayer.Field(columns[0]), nil
	}
	exprs := make([]recordlayer.KeyExpression, len(columns))
	for i, col := range columns {
		exprs[i] = recordlayer.Field(col)
	}
	return recordlayer.Concat(exprs...), nil
}

// buildVectorIndex materialises a VECTOR (HNSW) index from its spec.
// The root is a KeyWithValueExpression whose key part is the partition
// prefix and whose value part is the single vector column — identical to
// Java's vector index layout (cols before the split point = partition
// prefix, first col after = the indexed vector). The split point is the
// number of partition columns (0 for an unpartitioned index).
func buildVectorIndex(idx indexSpec) (*recordlayer.Index, error) {
	cols := make([]string, 0, len(idx.partitionColumns)+1)
	cols = append(cols, idx.partitionColumns...)
	cols = append(cols, idx.vectorColumn)

	var inner recordlayer.KeyExpression
	if len(cols) == 1 {
		inner = recordlayer.Field(cols[0])
	} else {
		exprs := make([]recordlayer.KeyExpression, len(cols))
		for i, col := range cols {
			exprs[i] = recordlayer.Field(col)
		}
		inner = recordlayer.Concat(exprs...)
	}
	root := recordlayer.KeyWithValue(inner, len(idx.partitionColumns))

	if idx.vectorMethod == "SPFRESH" {
		rl := recordlayer.NewIndex(idx.name, root)
		rl.Type = recordlayer.IndexTypeVectorSPFresh
		rl.Options = map[string]string{
			recordlayer.IndexOptionSPFreshNumDimensions: fmt.Sprintf("%d", idx.numDimensions),
		}
		for k, v := range idx.options {
			rl.Options[k] = v
		}
		return rl, nil
	}
	rl := recordlayer.NewVectorIndex(idx.name, root, idx.numDimensions)
	for k, v := range idx.options {
		rl.Options[k] = v
	}
	return rl, nil
}

func buildAggregateIndex(idx indexSpec) (*recordlayer.Index, error) {
	groupByExprs := make([]recordlayer.KeyExpression, len(idx.columns))
	for i, col := range idx.columns {
		groupByExprs[i] = recordlayer.Field(col)
	}

	var gke *recordlayer.GroupingKeyExpression
	if idx.aggColumn != "" {
		gke = recordlayer.GroupBy(recordlayer.Field(idx.aggColumn), groupByExprs...)
	} else if len(groupByExprs) == 0 {
		gke = recordlayer.GroupAll(recordlayer.EmptyKey())
	} else if len(groupByExprs) == 1 {
		// COUNT(*): GroupingKeyExpression(grouping, 0) with the grouping AS
		// the whole key — Java never stores an Empty child inside the concat
		// (MaterializedViewIndexGenerator.java:408-412; the generated-index
		// golden count_star_grouped pins the byte shape).
		gke = recordlayer.GroupAll(groupByExprs[0])
	} else {
		gke = recordlayer.GroupAll(recordlayer.Concat(groupByExprs...))
	}

	switch strings.ToUpper(idx.aggType) {
	case "SUM":
		return recordlayer.NewSumIndex(idx.name, gke), nil
	case "COUNT":
		return recordlayer.NewCountIndex(idx.name, gke), nil
	case "COUNT_NOT_NULL":
		return recordlayer.NewCountNotNullIndex(idx.name, gke), nil
	case "MAX":
		// Plain SQL MAX(col) is a PERMUTED_MAX index, matching Java's
		// NumericAggregationValue.Max.getIndexTypeName() == IndexTypes.PERMUTED_MAX.
		// It tracks the TRUE current maximum under deletes/updates (a MIN_EVER/
		// MAX_EVER index is monotone and goes stale). With no ORDER BY on the
		// aggregate, Java sets the permuted size to 0 (MaterializedViewIndexGenerator:
		// permutedSize = aggregateOrderIndex < 0 ? 0 : ...). The SEPARATE max_ever()
		// SQL function maps to the _EVER indexes below.
		return recordlayer.NewPermutedMaxIndex(idx.name, gke, 0), nil
	case "MIN":
		return recordlayer.NewPermutedMinIndex(idx.name, gke, 0), nil
	case "MAX_EVER_TUPLE":
		return recordlayer.NewMaxEverTupleIndex(idx.name, gke), nil
	case "MIN_EVER_TUPLE":
		return recordlayer.NewMinEverTupleIndex(idx.name, gke), nil
	default:
		return nil, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
			"unsupported aggregate index type %q", idx.aggType)
	}
}

// buildCardinalityIndex materialises a CARDINALITY() value index. The root is
// CardinalityExpr over the array column accessed with FanType.Concatenate (so
// the whole array materialises into one Key.Evaluated whose element count is
// the key). A dotted column (struct.int_arr) nests through the struct field.
// Mirrors Java's function("cardinality", field("arr", Concatenate)) /
// field("struct").nest(field("int_arr", Concatenate)).
//
// Go writes plain repeated array fields (no nullable-array wrapper — RFC-143
// §3a), so the argument is always field(col, Concatenate), never the Java
// wrapper shape field(col).nest(field("values", Concatenate)). The evaluator
// (CardinalityFunctionKeyExpression) still descends the wrapper for
// Java-written records on the read side.
func buildCardinalityIndex(idx indexSpec) (*recordlayer.Index, error) {
	segments := strings.Split(idx.cardinalityColumn, ".")
	if len(segments) == 0 || segments[len(segments)-1] == "" {
		return nil, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
			"cardinality index %q: empty column reference", idx.name)
	}
	// Innermost: the array field with Concatenate fan-out.
	arg := recordlayer.FieldConcatenate(segments[len(segments)-1])
	// Wrap each preceding segment as a nesting (left-to-right struct descent).
	for i := len(segments) - 2; i >= 0; i-- {
		arg = recordlayer.Nest(segments[i], arg)
	}
	root := recordlayer.CardinalityExpr(arg)
	return recordlayer.NewIndex(idx.name, root), nil
}

func buildPrimaryKeyExpression(tbl tableSpec, intermingle bool) (recordlayer.KeyExpression, error) {
	if len(tbl.primaryKey) == 0 {
		return nil, api.NewError(api.ErrCodeInvalidSchemaTemplate,
			"no primary key columns specified")
	}

	exprs := make([]recordlayer.KeyExpression, 0, len(tbl.primaryKey)+1)
	if !intermingle {
		exprs = append(exprs, recordlayer.RecordTypeKey())
	}
	for _, segments := range tbl.primaryKey {
		// Key expressions address proto STORAGE names (Java:
		// RecordLayerTable.Builder.toKeyExpression via getFieldStorageName) —
		// identical to the user name except for quoted identifiers carrying
		// '$', '.' or "__". A MULTI-SEGMENT part (id.a — a primary key over
		// a struct column's field) walks the segments into
		// field(storage).nest(child), Java's toKeyExpression recursion
		// (RecordLayerTable.java:313-329). The segmentation is decided by
		// the parse tree, not by re-splitting a joined name, so a quoted
		// identifier carrying a literal '.' stays ONE segment and escapes
		// to a single field(...) on its storage name.
		colName := strings.Join(segments, ".")
		var expr recordlayer.KeyExpression
		for i := len(segments) - 1; i >= 0; i-- {
			storage, err := recordlayer.ToProtoBufCompliantName(segments[i])
			if err != nil {
				return nil, api.WrapErrorf(err, api.ErrCodeInvalidSchemaTemplate,
					"primary key column %q", colName)
			}
			if expr == nil {
				expr = recordlayer.Field(storage)
			} else {
				expr = recordlayer.Nest(storage, expr)
			}
		}
		exprs = append(exprs, expr)
	}
	if len(exprs) == 1 {
		return exprs[0], nil
	}
	return recordlayer.Concat(exprs...), nil
}
