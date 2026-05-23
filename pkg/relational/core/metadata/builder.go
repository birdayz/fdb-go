package metadata

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
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
	errs             []error // deferred errors from AddIndex
	intermingleTbls  bool
	enableLongRows   bool
	storeRowVersions bool
}

type tableSpec struct {
	name       string
	columns    []ColumnSpec
	primaryKey []string
	indexes    []indexSpec
}

// indexSpec describes a single index within a table.
type indexSpec struct {
	name    string
	columns []string // field names in index key order
	unique  bool
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
// declared order; primaryKey is the ordered slice of column names.
func (b *Builder) AddTable(name string, columns []ColumnSpec, primaryKey []string) *Builder {
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

	fd, err := b.buildFileDescriptor()
	if err != nil {
		return nil, api.WrapErrorf(err, api.ErrCodeInvalidSchemaTemplate, "build file descriptor")
	}

	mdBuilder := recordlayer.NewRecordMetaDataBuilder().SetRecords(fd)
	mdBuilder.SetSplitLongRecords(b.enableLongRows)
	mdBuilder.SetStoreRecordVersions(b.storeRowVersions)
	mdBuilder.SetVersion(b.version)
	if !b.intermingleTbls {
		mdBuilder.SetRecordCountKey(recordlayer.RecordTypeKey())
	} else {
		mdBuilder.SetRecordCountKey(recordlayer.EmptyKey())
	}

	for _, tbl := range b.tables {
		rt := mdBuilder.GetRecordType(tbl.name)
		if rt == nil {
			return nil, api.NewErrorf(api.ErrCodeInternalError,
				"record type %q not found after SetRecords", tbl.name)
		}
		pkExpr, err := buildPrimaryKeyExpression(tbl, b.intermingleTbls)
		if err != nil {
			return nil, api.WrapErrorf(err, api.ErrCodeInvalidSchemaTemplate,
				"table %q primary key", tbl.name)
		}
		rt.SetPrimaryKey(pkExpr)

		for _, idx := range tbl.indexes {
			keyExpr, idxErr := buildIndexKeyExpression(idx.columns)
			if idxErr != nil {
				return nil, api.WrapErrorf(idxErr, api.ErrCodeInvalidSchemaTemplate,
					"table %q index %q", tbl.name, idx.name)
			}
			rl := recordlayer.NewIndex(idx.name, keyExpr)
			if idx.unique {
				rl.SetUnique()
			}
			mdBuilder.AddIndex(tbl.name, rl)
		}
	}

	md, err := mdBuilder.Build()
	if err != nil {
		return nil, api.WrapErrorf(err, api.ErrCodeInternalError, "build RecordMetaData")
	}

	return NewRecordLayerSchemaTemplateWithVersion(b.name, md, b.version)
}

// buildFileDescriptor constructs a protoreflect.FileDescriptor from
// the registered table specs. No union descriptor is generated because
// dynamically-created message types are not in the global proto type
// registry, so RecordMetaDataBuilder.Build() cannot obtain a message
// factory for them. Without a union, setRecordsWithoutUnion() is used,
// which leaves UnionFieldDescriptor nil and skips the factory lookup.
func (b *Builder) buildFileDescriptor() (protoreflect.FileDescriptor, error) {
	fdp := &descriptorpb.FileDescriptorProto{}
	fdp.Name = proto.String(b.name + ".proto")
	fdp.Syntax = proto.String("proto2")
	fdp.Dependency = []string{
		gen.File_tuple_fields_proto.Path(),
		gen.File_record_metadata_options_proto.Path(),
	}

	for _, tbl := range b.tables {
		msgDesc, err := buildMessageDescriptor(tbl)
		if err != nil {
			return nil, api.WrapErrorf(err, api.ErrCodeInvalidSchemaTemplate,
				"table %q", tbl.name)
		}
		fdp.MessageType = append(fdp.MessageType, msgDesc)
	}

	// Generate the UnionDescriptor message required for record serialization.
	// Each table gets one optional field numbered starting at 1.
	unionMsg := &descriptorpb.DescriptorProto{Name: proto.String("UnionDescriptor")}
	for i, tbl := range b.tables {
		unionMsg.Field = append(unionMsg.Field, &descriptorpb.FieldDescriptorProto{
			Name:     proto.String("_" + tbl.name),
			Number:   proto.Int32(int32(i + 1)),
			Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
			TypeName: proto.String(tbl.name),
		})
	}
	fdp.MessageType = append(fdp.MessageType, unionMsg)

	// Build a resolver that includes the two dependency files.
	// RegisterFile returns an error on duplicate registration; ignore it since
	// the global registry already has these files and we just want them
	// available to the local resolver.
	resolver := &protoregistry.Files{}
	_ = resolver.RegisterFile(gen.File_tuple_fields_proto)
	_ = resolver.RegisterFile(gen.File_record_metadata_options_proto)

	fd, err := protodesc.NewFile(fdp, resolver)
	if err != nil {
		return nil, api.WrapErrorf(err, api.ErrCodeInternalError, "protodesc.NewFile")
	}
	return fd, nil
}

// uuidProtoTypeName is the fully-qualified proto message name for the
// tuple_fields.UUID record (sfixed64 most/least bits). Matches Java's
// Type.uuidType lowering — fdb-relational stores UUID column values
// as TupleFieldsProto.UUID instances.
const uuidProtoTypeName = ".com.apple.foundationdb.record.UUID"

// buildMessageDescriptor converts a tableSpec into a proto DescriptorProto.
func buildMessageDescriptor(tbl tableSpec) (*descriptorpb.DescriptorProto, error) {
	msg := &descriptorpb.DescriptorProto{Name: proto.String(tbl.name)}
	for _, col := range tbl.columns {
		ft, typeName, err := datatypeToProtoFieldType(col.dt)
		if err != nil {
			return nil, api.WrapErrorf(err, api.ErrCodeInvalidSchemaTemplate,
				"column %q", col.name)
		}
		field := &descriptorpb.FieldDescriptorProto{
			Name:   proto.String(col.name),
			Number: proto.Int32(col.fieldNum),
			Label:  datatypeToLabel(col.dt).Enum(),
			Type:   ft.Enum(),
		}
		if typeName != "" {
			field.TypeName = proto.String(typeName)
		}
		msg.Field = append(msg.Field, field)
	}
	return msg, nil
}

// datatypeToProtoFieldType maps an api.DataType to the corresponding
// proto field type and (for message-typed fields) the fully-qualified
// type name. Scalar primitives return (TYPE_*, "", nil); message types
// return (TYPE_MESSAGE, ".pkg.Name", nil).
func datatypeToProtoFieldType(dt api.DataType) (descriptorpb.FieldDescriptorProto_Type, string, error) {
	switch dt.Code() {
	case api.CodeBoolean:
		return descriptorpb.FieldDescriptorProto_TYPE_BOOL, "", nil
	case api.CodeInteger:
		return descriptorpb.FieldDescriptorProto_TYPE_INT32, "", nil
	case api.CodeLong:
		return descriptorpb.FieldDescriptorProto_TYPE_INT64, "", nil
	case api.CodeFloat:
		return descriptorpb.FieldDescriptorProto_TYPE_FLOAT, "", nil
	case api.CodeDouble:
		return descriptorpb.FieldDescriptorProto_TYPE_DOUBLE, "", nil
	case api.CodeString:
		return descriptorpb.FieldDescriptorProto_TYPE_STRING, "", nil
	case api.CodeBytes:
		return descriptorpb.FieldDescriptorProto_TYPE_BYTES, "", nil
	case api.CodeUUID:
		return descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, uuidProtoTypeName, nil
	case api.CodeDate, api.CodeTimestamp:
		return descriptorpb.FieldDescriptorProto_TYPE_STRING, "", nil
	case api.CodeArray:
		// Array types use the element type's proto field type with LABEL_REPEATED.
		// The label is handled by datatypeToLabel; here we return the element's type.
		at := dt.(*api.ArrayType)
		return datatypeToProtoFieldType(at.ElementType())
	default:
		return 0, "", api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
			"unsupported DataType code %v", dt.Code())
	}
}

// datatypeToLabel returns LABEL_REPEATED for array types, OPTIONAL for
// nullable types, REQUIRED for not-nullable. (Proto2 semantics.)
func datatypeToLabel(dt api.DataType) descriptorpb.FieldDescriptorProto_Label {
	if dt.Code() == api.CodeArray {
		return descriptorpb.FieldDescriptorProto_LABEL_REPEATED
	}
	if dt.IsNullable() {
		return descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	}
	return descriptorpb.FieldDescriptorProto_LABEL_REQUIRED
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

func buildPrimaryKeyExpression(tbl tableSpec, intermingle bool) (recordlayer.KeyExpression, error) {
	if len(tbl.primaryKey) == 0 {
		return nil, api.NewError(api.ErrCodeInvalidSchemaTemplate,
			"no primary key columns specified")
	}

	exprs := make([]recordlayer.KeyExpression, 0, len(tbl.primaryKey)+1)
	if !intermingle {
		exprs = append(exprs, recordlayer.RecordTypeKey())
	}
	for _, colName := range tbl.primaryKey {
		exprs = append(exprs, recordlayer.Field(colName))
	}
	if len(exprs) == 1 {
		return exprs[0], nil
	}
	return recordlayer.Concat(exprs...), nil
}
