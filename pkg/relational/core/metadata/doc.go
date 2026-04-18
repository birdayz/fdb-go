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
// Deferred behaviours that Java implements but this bridge does NOT
// (track each as an independent Phase 2 follow-up):
//
//  1. Sparse-index IsSparse(): always false here until
//     recordlayer.Index grows Java's NotNullOnly flag. This is a
//     record-layer-side change, not a bridge change.
//
// None of these block the Phase 3 semantic analyzer on the primary
// path (CRUD over typed proto records + basic aggregations).
package metadata
