package metadata

import (
	"fmt"

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
// list of field names that form the index key. unique is not yet enforced by
// the recordlayer but is stored for future use.
// Must be called after the table is registered via AddTable.
func (b *Builder) AddIndex(tableName, indexName string, columns []string, unique bool) *Builder {
	for i := range b.tables {
		if b.tables[i].name == tableName {
			b.tables[i].indexes = append(b.tables[i].indexes, indexSpec{
				name:    indexName,
				columns: columns,
				unique:  unique,
			})
			return b
		}
	}
	// Unknown table — store anyway; Build() will report the error.
	b.tables = append(b.tables, tableSpec{
		name:    tableName,
		indexes: []indexSpec{{name: indexName, columns: columns, unique: unique}},
	})
	return b
}

// Build materialises the schema template. Returns an error when no
// tables are registered or types cannot be mapped to proto field types.
func (b *Builder) Build() (*RecordLayerSchemaTemplate, error) {
	if len(b.tables) == 0 {
		return nil, api.NewError(api.ErrCodeInvalidSchemaTemplate, "schema template contains no tables")
	}
	if b.name == "" {
		return nil, api.NewError(api.ErrCodeInvalidSchemaTemplate, "schema template name is required")
	}

	fd, err := b.buildFileDescriptor()
	if err != nil {
		return nil, fmt.Errorf("build file descriptor: %w", err)
	}

	mdBuilder := recordlayer.NewRecordMetaDataBuilder().SetRecords(fd)
	mdBuilder.SetSplitLongRecords(b.enableLongRows)
	mdBuilder.SetStoreRecordVersions(b.storeRowVersions)
	mdBuilder.SetVersion(b.version)

	for _, tbl := range b.tables {
		rt := mdBuilder.GetRecordType(tbl.name)
		if rt == nil {
			return nil, fmt.Errorf("record type %q not found after SetRecords", tbl.name)
		}
		pkExpr, err := buildPrimaryKeyExpression(tbl, b.intermingleTbls)
		if err != nil {
			return nil, fmt.Errorf("table %q primary key: %w", tbl.name, err)
		}
		rt.SetPrimaryKey(pkExpr)

		for _, idx := range tbl.indexes {
			keyExpr, idxErr := buildIndexKeyExpression(idx.columns)
			if idxErr != nil {
				return nil, fmt.Errorf("table %q index %q: %w", tbl.name, idx.name, idxErr)
			}
			rl := recordlayer.NewIndex(idx.name, keyExpr)
			mdBuilder.AddIndex(tbl.name, rl)
		}
	}

	md, err := mdBuilder.Build()
	if err != nil {
		return nil, fmt.Errorf("build RecordMetaData: %w", err)
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
			return nil, fmt.Errorf("table %q: %w", tbl.name, err)
		}
		fdp.MessageType = append(fdp.MessageType, msgDesc)
	}

	// Build a resolver that includes the two dependency files.
	// RegisterFile returns an error on duplicate registration; ignore it since
	// the global registry already has these files and we just want them
	// available to the local resolver.
	resolver := &protoregistry.Files{}
	_ = resolver.RegisterFile(gen.File_tuple_fields_proto)
	_ = resolver.RegisterFile(gen.File_record_metadata_options_proto)

	fd, err := protodesc.NewFile(fdp, resolver)
	if err != nil {
		return nil, fmt.Errorf("protodesc.NewFile: %w", err)
	}
	return fd, nil
}

// buildMessageDescriptor converts a tableSpec into a proto DescriptorProto.
func buildMessageDescriptor(tbl tableSpec) (*descriptorpb.DescriptorProto, error) {
	msg := &descriptorpb.DescriptorProto{Name: proto.String(tbl.name)}
	for _, col := range tbl.columns {
		ft, err := datatypeToProtoFieldType(col.dt)
		if err != nil {
			return nil, fmt.Errorf("column %q: %w", col.name, err)
		}
		msg.Field = append(msg.Field, &descriptorpb.FieldDescriptorProto{
			Name:   proto.String(col.name),
			Number: proto.Int32(col.fieldNum),
			Label:  datatypeToLabel(col.dt).Enum(),
			Type:   ft.Enum(),
		})
	}
	return msg, nil
}

// datatypeToProtoFieldType maps an api.DataType to the corresponding
// proto field type. Scalar primitives only.
func datatypeToProtoFieldType(dt api.DataType) (descriptorpb.FieldDescriptorProto_Type, error) {
	switch dt.Code() {
	case api.CodeBoolean:
		return descriptorpb.FieldDescriptorProto_TYPE_BOOL, nil
	case api.CodeInteger:
		return descriptorpb.FieldDescriptorProto_TYPE_INT32, nil
	case api.CodeLong:
		return descriptorpb.FieldDescriptorProto_TYPE_INT64, nil
	case api.CodeFloat:
		return descriptorpb.FieldDescriptorProto_TYPE_FLOAT, nil
	case api.CodeDouble:
		return descriptorpb.FieldDescriptorProto_TYPE_DOUBLE, nil
	case api.CodeString:
		return descriptorpb.FieldDescriptorProto_TYPE_STRING, nil
	case api.CodeBytes:
		return descriptorpb.FieldDescriptorProto_TYPE_BYTES, nil
	default:
		return 0, fmt.Errorf("unsupported DataType code %v", dt.Code())
	}
}

// datatypeToLabel returns OPTIONAL for nullable types, REQUIRED for
// not-nullable. (Proto2 semantics.)
func datatypeToLabel(dt api.DataType) descriptorpb.FieldDescriptorProto_Label {
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
		return nil, fmt.Errorf("index must have at least one column")
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
		return nil, fmt.Errorf("no primary key columns specified")
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
