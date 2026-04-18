// Package metadata provides concrete bridge implementations of the
// api.* metadata interfaces backed by *recordlayer.RecordMetaData.
//
// Mirrors Java's RecordLayerSchemaTemplate / RecordLayerTable /
// RecordLayerColumn / RecordLayerIndex types — the bridge between the
// SQL layer's declarative view of schema and the record-layer storage
// engine's proto-descriptor view.
//
// Direction of dependency: the relational layer depends on the record
// layer, never the other way. Don't reach back from pkg/recordlayer
// into pkg/relational.
//
// # Java-compliance notes
//
// The proto-to-DataType mapping in proto_types.go mirrors Java's
// com.apple.foundationdb.record.query.plan.cascades.typing.Type
// (fromProtobufFieldDescriptor + Record.fromDescriptorPreservingName)
// exactly for the cases that matter today:
//
//   - scalar kinds → primitive types (IntegerType / LongType / etc)
//   - enum → EnumType with declared values
//   - message / group → recursive StructType, with two short-circuits:
//     (a) com.apple.foundationdb.record.UUID → UUIDType, and
//     (b) "message M { repeated R values = 1; }" nullable-array wrapper
//     → ArrayType (see unwrapWrappedArray / Java's
//     NullableArrayTypeUtils.describesWrappedArray)
//   - repeated → ArrayType(element)
//   - map → UnresolvedType
//
// No known behavioural divergences from Java remain in this bridge
// for the Phase 3 primary path. Edge cases the Java side also has
// acknowledged TODOs about (table-level nullable=true, maps as
// UnresolvedType, auxiliary types from Union enums) mirror Java's
// behaviour rather than Java's intent.
//
// Doesn't block the Phase 3 semantic analyzer on the primary path
// (CRUD over typed proto records + basic aggregations).
//
// # Version semantics
//
// SchemaTemplate.Version() is catalog-level and decoupled from
// RecordMetaData.Version(). The default constructor
// NewRecordLayerSchemaTemplate passes md.Version() for convenience,
// but NewRecordLayerSchemaTemplateWithVersion lets the catalog pin a
// different value — matches Java's fromRecordMetadata(md, name,
// version) signature exactly. Catalogs that want to increment the
// schema version without also rebuilding storage metadata need the
// explicit-version constructor.
package metadata
