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
package metadata
