// Portions derived from FoundationDB Record Layer (PipelineOperation.java,
// FDBRecordStoreBase.java, FDBRecordStore.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

// PipelineOperation names a kind of asynchronous pipeline a record store runs,
// so a PipelineSizer can choose its depth. Matches Java's
// com.apple.foundationdb.record.PipelineOperation (PipelineOperation.java:31-77).
type PipelineOperation struct {
	name string
}

// NewPipelineOperation is PipelineOperation(String name).
func NewPipelineOperation(name string) PipelineOperation { return PipelineOperation{name: name} }

// Name is getName.
func (o PipelineOperation) Name() string { return o.name }

// String is toString.
func (o PipelineOperation) String() string { return o.name }

// The operations Java declares (PipelineOperation.java:33-56).
var (
	PipelineOperationIndexToRecord       = NewPipelineOperation("INDEX_TO_RECORD")
	PipelineOperationKeyToRecord         = NewPipelineOperation("KEY_TO_RECORD")
	PipelineOperationRecordAsyncFilter   = NewPipelineOperation("RECORD_ASYNC_FILTER")
	PipelineOperationIndexAsyncFilter    = NewPipelineOperation("INDEX_ASYNC_FILTER")
	PipelineOperationRecordFunction      = NewPipelineOperation("RECORD_FUNCTION")
	PipelineOperationResolveUniqueness   = NewPipelineOperation("RESOLVE_UNIQUENESS")
	PipelineOperationInJoin              = NewPipelineOperation("IN_JOIN")
	PipelineOperationTextIndexUpdate     = NewPipelineOperation("TEXT_INDEX_UPDATE")
	PipelineOperationSyntheticRecordJoin = NewPipelineOperation("SYNTHETIC_RECORD_JOIN")
	PipelineOperationUpdate              = NewPipelineOperation("UPDATE")
	PipelineOperationDelete              = NewPipelineOperation("DELETE")
	PipelineOperationInsert              = NewPipelineOperation("INSERT")
)

// PipelineSizer computes how many elements an operation's pipeline keeps in
// flight. Matches Java's FDBRecordStoreBase.PipelineSizer
// (FDBRecordStoreBase.java:1736-1738).
type PipelineSizer func(op PipelineOperation) int

// DefaultPipelineSizer is FDBRecordStore.DEFAULT_PIPELINE_SIZER
// (FDBRecordStore.java:183-191): DML statements run one record at a time,
// everything else DefaultPipelineSize deep.
func DefaultPipelineSizer(op PipelineOperation) int {
	if op == PipelineOperationUpdate || op == PipelineOperationInsert || op == PipelineOperationDelete {
		return 1
	}
	return DefaultPipelineSize
}

// GetPipelineSizer is FDBRecordStore.getPipelineSizer (FDBRecordStore.java:
// 2359-2361): the builder's sizer, DefaultPipelineSizer when none was set.
func (store *FDBRecordStore) GetPipelineSizer() PipelineSizer {
	if store == nil || store.pipelineSizer == nil {
		return DefaultPipelineSizer
	}
	return store.pipelineSizer
}

// GetPipelineSize is FDBRecordStoreBase.getPipelineSize
// (FDBRecordStoreBase.java:1753-1755).
func (store *FDBRecordStore) GetPipelineSize(op PipelineOperation) int {
	return store.GetPipelineSizer()(op)
}

// SetPipelineSizer is FDBRecordStore.Builder.setPipelineSizer
// (FDBRecordStore.java:5918-5921).
func (b *StoreBuilder) SetPipelineSizer(sizer PipelineSizer) *StoreBuilder {
	b.pipelineSizer = sizer
	return b
}

// GetPipelineSizer is FDBRecordStore.Builder.getPipelineSizer
// (FDBRecordStore.java:5912-5914).
func (b *StoreBuilder) GetPipelineSizer() PipelineSizer {
	if b.pipelineSizer == nil {
		return DefaultPipelineSizer
	}
	return b.pipelineSizer
}
