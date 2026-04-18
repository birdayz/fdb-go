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
//   - message / group → recursive StructType (UUID short-circuits to
//     UUIDType; see isUUIDDescriptor)
//   - repeated → ArrayType(element)
//   - map → UnresolvedType
//
// Deferred behaviours that Java implements but this bridge does NOT
// (track each as an independent Phase 2 follow-up):
//
//  1. NullableArrayTypeUtils.describesWrappedArray: Java unwraps
//     the "wrapper message around a repeated field" pattern that the
//     serializer uses to make arrays nullable. We currently surface
//     the wrapper as a regular StructType. Round-trip via
//     Java-written metadata will diverge until this lands.
//
//  2. primaryKeyHasRecordTypePrefix → intermingleTables: Java
//     surfaces this as a template flag. Not exposed on our
//     api.SchemaTemplate interface yet.
//
//  3. Sparse-index IsSparse(): always false here until
//     recordlayer.Index grows Java's NotNullOnly flag.
//
// None of these block the Phase 3 semantic analyzer on the primary
// path (CRUD over typed proto records + basic aggregations).
package metadata
