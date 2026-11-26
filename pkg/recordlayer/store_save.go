// Portions derived from FoundationDB Record Layer (FDBRecordStore.java,
// FDBStoredRecord.java, FormatVersion.java, TupleRange.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"time"
	"unsafe"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// SaveRecord saves a protobuf record to the store.
// Equivalent to SaveRecordWithOptions(record, RecordExistenceCheckNone).
// Java equivalent: FDBRecordStore.saveRecord(Message rec)
func (store *FDBRecordStore) SaveRecord(record proto.Message) (*FDBStoredRecord[proto.Message], error) {
	return store.SaveRecordWithOptions(record, RecordExistenceCheckNone)
}

// SaveRecordWithOptions saves a protobuf record with existence checking and the
// default version behavior.
//
// Java equivalent: FDBRecordStoreBase.saveRecord(Message rec, RecordExistenceCheck existenceCheck)
//
// Returns:
//   - *FDBStoredRecord: The saved record with metadata
//   - error: RecordAlreadyExistsError, RecordDoesNotExistError, or RecordTypeChangedError based on existenceCheck
func (store *FDBRecordStore) SaveRecordWithOptions(
	record proto.Message,
	existenceCheck RecordExistenceCheck,
) (*FDBStoredRecord[proto.Message], error) {
	return store.saveRecordInternal(record, existenceCheck, nil, VersionstampSaveBehaviorDefault, false, nil, nil, nil)
}

// SaveRecordWithVersion saves a record with the version behavior chooses from version (nil,
// complete, or incomplete from ClaimLocalVersion): Java's saveRecordAsync(rec, check, version, behavior).
func (store *FDBRecordStore) SaveRecordWithVersion(
	record proto.Message,
	existenceCheck RecordExistenceCheck,
	version *FDBRecordVersion,
	behavior VersionstampSaveBehavior,
) (*FDBStoredRecord[proto.Message], error) {
	return store.saveRecordInternal(record, existenceCheck, version, behavior, false, nil, nil, nil)
}

// SaveRecordsPipelined saves in order with windowed old-record reads; INSERT defaults to depth one.
// The first failure stops saves, but already-issued window reads remain in the transaction, as in Java.
func (store *FDBRecordStore) SaveRecordsPipelined(
	records []proto.Message,
	existenceCheck RecordExistenceCheck,
) ([]*FDBStoredRecord[proto.Message], error) {
	finishWrite, err := store.context.beginRecordWrite()
	if err != nil {
		return nil, err
	}
	defer finishWrite()
	saved := make([]*FDBStoredRecord[proto.Message], 0, len(records))
	tx := store.context.Transaction()
	depth := max(1, store.GetPipelineSize(PipelineOperationInsert))
	for start := 0; start < len(records); start += depth {
		window := records[start:min(start+depth, len(records))]
		loads := store.startExistingRecordLoads(tx, window)
		for i, record := range window {
			stored, err := store.saveRecordInternal(record, existenceCheck, nil, VersionstampSaveBehaviorDefault, false, &loads[i], nil, nil)
			if err != nil {
				return nil, err
			}
			saved = append(saved, stored)
		}
	}
	return saved, nil
}

// saveTarget is what a save derives from its record before reading anything.
type saveTarget struct {
	recordTypeName string
	recordType     *RecordType
	record         proto.Message // as every later load reads it
	write          proto.Message // what the save serializes
	primaryKey     tuple.Tuple
	version        *FDBRecordVersion
}

// existingRecordLoad is a save's read of the record already stored under its
// key, sent ahead of the save; without sent, the save reads for itself.
type existingRecordLoad struct {
	target  saveTarget
	err     error
	pending pendingStoreRecordLoad
	sent    bool
}

// startExistingRecordLoads sends the existing-record read of every record in
// window. A record repeating an earlier key in window is left to read at its
// save, since a read sent before the earlier save would miss it.
func (store *FDBRecordStore) startExistingRecordLoads(tx fdb.ReadTransaction, window []proto.Message) []existingRecordLoad {
	loads := make([]existingRecordLoad, len(window))
	keys := make([][]byte, 0, len(window))
	for i, record := range window {
		load := &loads[i]
		if load.target, load.err = store.saveTargetFor(record, nil, VersionstampSaveBehaviorDefault); load.err != nil {
			continue
		}
		key := load.target.primaryKey.Pack()
		if slices.ContainsFunc(keys, func(k []byte) bool { return bytes.Equal(k, key) }) {
			continue
		}
		keys = append(keys, key)
		load.pending = store.startAuthoritativeRecordLoad(tx, load.target.primaryKey)
		load.sent = true
	}
	return loads
}

// saveTargetFor resolves the record's type, chosen version, Java form and primary key.
// Java chooses the version before evaluating the primary key against the versioned record.
func (store *FDBRecordStore) saveTargetFor(record proto.Message, version *FDBRecordVersion, behavior VersionstampSaveBehavior) (saveTarget, error) {
	if record == nil {
		return saveTarget{}, fmt.Errorf("cannot save nil record")
	}
	// Java's saveTypedRecord resolves the type by descriptor before anything
	// else (FDBRecordStore.java:552): a same-named message of another
	// descriptor is refused, untyped saves included.
	descriptor := record.ProtoReflect().Descriptor()
	recordType, err := store.metaData.GetRecordTypeForDescriptor(descriptor)
	if err != nil {
		return saveTarget{}, err
	}
	// Java chooses the save's version here, before the primary key (FDBRecordStore.java:555).
	savedVersion, err := store.recordVersionForSave(version, behavior)
	if err != nil {
		return saveTarget{}, err
	}
	recordTypeName := string(descriptor.Name())

	if recordType.PrimaryKey == nil {
		return saveTarget{}, &MetaDataError{Message: fmt.Sprintf("no primary key defined for record type: %s", recordTypeName)}
	}
	if err := store.checkPrimaryKeyCollation(recordType); err != nil {
		return saveTarget{}, err
	}
	// A closed enum field holding a number its enum does not declare is read by
	// every later load, in both engines, as unset (proto_closed_enums.go), so
	// the record is keyed, counted, indexed and written as that reading: an
	// update or delete that loads it then removes exactly the entries this save
	// writes. The caller's message is not changed. writeRecord is what the save
	// serializes (asJavaForSave: a DynamicMessage map value keeps its number,
	// which the map rewrite writes in Java's form).
	var writeRecord proto.Message
	record, writeRecord = recordType.asJavaForSave(record)

	// Extract primary key values using the flat evaluator (avoids [][]any alloc).
	// The record type is supplied to the evaluation because a record-type-prefixed
	// primary key reads its leading component off the record's own type. Java has
	// the same ordering constraint and solves it the same way: saveTypedRecord
	// builds FDBStoredRecord.newBuilder(rec).setRecordType(recordType) FIRST, then
	// evaluates the primary key against that builder (FDBRecordStore.java).
	keyValues, err := evaluateKeyFlat(recordType.PrimaryKey, &FDBStoredRecord[proto.Message]{
		RecordType: recordType,
		Record:     record,
		Version:    savedVersion,
	}, record)
	if err != nil {
		return saveTarget{}, fmt.Errorf("failed to extract primary key: %w", err)
	}
	// Reinterpret []any as tuple.Tuple without a copy; both element types are any.
	primaryKey := *(*tuple.Tuple)(unsafe.Pointer(&keyValues))
	if incomplete, _ := primaryKey.HasIncompleteVersionstamp(); incomplete {
		return saveTarget{}, &IncompleteVersionstampError{Context: "record primary key"}
	}
	return saveTarget{
		recordTypeName: recordTypeName,
		recordType:     recordType,
		record:         record,
		write:          writeRecord,
		primaryKey:     primaryKey,
		version:        savedVersion,
	}, nil
}

type existingSaveState struct {
	pending    pendingStoreRecordLoad
	sizeInfo   sizeInfo
	exists     bool
	recordType *RecordType
	record     proto.Message
	wire       *recordWire
	version    *FDBRecordVersion
}

// saveRecordInternal reuses/prepares a target; overrideLock bypasses the update lock and typed supplies serialization.
// written optionally collects indexes whose own data was updated.
func (store *FDBRecordStore) saveRecordInternal(
	record proto.Message,
	existenceCheck RecordExistenceCheck,
	version *FDBRecordVersion,
	behavior VersionstampSaveBehavior,
	overrideLock bool,
	load *existingRecordLoad,
	typed *typedRecordSerializer,
	written indexWriteSet,
) (*FDBStoredRecord[proto.Message], error) {
	finishWrite, writeErr := store.context.beginRecordWrite()
	if writeErr != nil {
		return nil, writeErr
	}
	defer finishWrite()
	startTime := time.Now()
	var target saveTarget
	var err error
	if load != nil {
		target, err = load.target, load.err
	} else {
		target, err = store.saveTargetFor(record, version, behavior)
	}
	if err != nil {
		return nil, err
	}
	defer store.context.Timer().RecordSince(EventSaveRecord, startTime)

	var existing existingSaveState
	if err := store.loadExistingRecordForSave(target.primaryKey, load, typed, &existing); err != nil {
		return nil, err
	}
	if err := store.validateSavePreconditions(target, &existing, existenceCheck, overrideLock, typed); err != nil {
		return nil, err
	}

	// Serialize directly into union wire format (no UnionDescriptor allocation)
	data, err := serializeUnionOver(target.write, target.recordType, priorRecordInner(existing.recordType, existing.wire, target.recordType))
	if err != nil {
		return nil, &RecordSerializationError{Cause: err}
	}
	// The bytes stored: the serializer's transformation of the union message,
	// which the split then chunks (Java serializes, then splits).
	stored, err := store.writeStoredRecord(data, target.recordType, target.primaryKey)
	if err != nil {
		return nil, err
	}

	var newsizeInfo sizeInfo
	if err := store.writeSavePhysical(target, stored, &existing, &newsizeInfo); err != nil {
		return nil, err
	}

	newStoredRecord := &FDBStoredRecord[proto.Message]{
		PrimaryKey: target.primaryKey,
		RecordType: target.recordType,
		Record:     target.record,
		// Its map entries are indexed in the order it was written in.
		wire:            newRecordWire(target.recordType, unionInner(data, target.recordType.unionFieldNumber)),
		Version:         target.version,
		Store:           store,
		KeyCount:        newsizeInfo.KeyCount,
		ValueSize:       newsizeInfo.ValueSize,
		KeySize:         newsizeInfo.KeySize,
		Split:           newsizeInfo.IsSplit,
		VersionedInline: newsizeInfo.VersionedInline,
	}

	// Only increment record count for new inserts (not updates).
	if !existing.exists {
		if err := store.addRecordCount(newStoredRecord, littleEndianInt64One); err != nil {
			return nil, fmt.Errorf("failed to increment record count: %w", err)
		}
	}

	// Update secondary indexes
	if store.metaData.HasIndexes() {
		var oldStoredRecord *FDBStoredRecord[proto.Message]
		if existing.exists {
			oldStoredRecord = &FDBStoredRecord[proto.Message]{
				PrimaryKey: target.primaryKey,
				RecordType: existing.recordType,
				Record:     existing.record,
				wire:       existing.wire,
				Version:    existing.version,
				Store:      store,
			}
		}
		if err := store.updateSecondaryIndexes(oldStoredRecord, newStoredRecord, written); err != nil {
			return nil, err
		}
	}

	// Record instrumentation
	timer := store.context.Timer()
	timer.Increment(CountSaveRecordKey)
	timer.IncrementBy(CountSaveRecordKeyBytes, int64(newsizeInfo.KeySize))
	timer.IncrementBy(CountSaveRecordValueBytes, int64(newsizeInfo.ValueSize))

	if err := store.context.CheckTransactionSize(); err != nil {
		return newStoredRecord, err
	}

	return newStoredRecord, nil
}

// Always load the existing record (matching Java's saveRecordAsync behavior).
// This is needed for: existence checks, record counting, and future
// index updates / version management.
func (store *FDBRecordStore) loadExistingRecordForSave(primaryKey tuple.Tuple, load *existingRecordLoad, typed *typedRecordSerializer, existing *existingSaveState) error {
	loadTimer := recordLoadTimer{timer: store.context.Timer(), start: time.Now()}
	defer loadTimer.finish()
	if load != nil && load.sent {
		existing.pending = load.pending
	} else {
		existing.pending = store.startAuthoritativeRecordLoad(store.context.Transaction(), primaryKey)
	}
	oldValue, oldRecordVersion, err := existing.pending.finishAuthoritative(&existing.sizeInfo)
	if err != nil {
		return fmt.Errorf("failed to load existing record: %w", err)
	}
	existing.exists = oldValue != nil

	// Java's saveTypedRecord loads the existing record through the serializer
	// (loadExistingRecord → loadTypedRecord, FDBRecordStore.java:561, :636-642)
	// before any existence check, so a record the store cannot read (one
	// encrypted under a key it lacks, or corrupt) fails the save with
	// RecordDeserializationException whatever the check, and is never
	// overwritten. The decoded record serves the type check and the index
	// update below.
	if existing.exists {
		var deserErr error
		existing.recordType, existing.record, existing.wire, deserErr = store.deserializeAndDiscover(oldValue)
		if deserErr != nil {
			return &RecordDeserializationError{PrimaryKey: primaryKey, Cause: deserErr}
		}
		if typed != nil {
			if err := typed.checkDeserialized(primaryKey, existing.recordType); err != nil {
				return err
			}
		}
		existing.version, err = existing.pending.finishVersion(oldRecordVersion)
		if err != nil {
			return fmt.Errorf("load old record version for save: %w", err)
		}
	} else if err := existing.pending.finishRetiredVersionErrors(); err != nil {
		return fmt.Errorf("load old record version for save: %w", err)
	}

	loadTimer.finish()
	return nil
}

func (store *FDBRecordStore) validateSavePreconditions(target saveTarget, existing *existingSaveState, existenceCheck RecordExistenceCheck, overrideLock bool, typed *typedRecordSerializer) error {
	if err := checkSaveExistence(target.primaryKey, target.recordTypeName, existing, existenceCheck); err != nil {
		return err
	}

	// Check lock state AFTER load and existence checks but BEFORE write,
	// matching Java's saveRecordAsync() error precedence: existence/type
	// errors take priority over lock errors.
	if !overrideLock {
		if err := store.validateRecordUpdateAllowed(); err != nil {
			return err
		}
	}

	if err := store.checkRecordWriteCollation(!existing.exists, target.recordType, existing.recordType); err != nil {
		return err
	}

	if typed != nil {
		if err := typed.checkSerialize(target.recordType, store.metaData.Version()); err != nil {
			return err
		}
	}
	return nil
}

func checkSaveExistence(primaryKey tuple.Tuple, recordTypeName string, existing *existingSaveState, existenceCheck RecordExistenceCheck) error {
	if existenceCheck == RecordExistenceCheckNone {
		return nil
	}
	if existenceCheck.ErrorIfExists() && existing.exists {
		return &RecordAlreadyExistsError{
			Message:    "record already exists",
			PrimaryKey: primaryKey,
		}
	}
	if existenceCheck.ErrorIfNotExists() && !existing.exists {
		return &RecordDoesNotExistError{
			Message:    "record does not exist",
			PrimaryKey: primaryKey,
		}
	}
	if existenceCheck.ErrorIfTypeChanged() && existing.exists {
		existingTypeName := string(existing.record.ProtoReflect().Descriptor().Name())
		if existingTypeName != recordTypeName {
			return &RecordTypeChangedError{
				Message:      "record type changed",
				PrimaryKey:   primaryKey,
				ActualType:   existingTypeName,
				ExpectedType: recordTypeName,
			}
		}
	}
	return nil
}

func (store *FDBRecordStore) writeSavePhysical(target saveTarget, stored []byte, existing *existingSaveState, newsizeInfo *sizeInfo) error {
	physicalPresent, finishPhysical, err := existing.pending.beginPhysicalMutation(existing.exists, &existing.sizeInfo)
	if err != nil {
		return err
	}
	defer finishPhysical()
	var oldsizeInfoPtr *sizeInfo
	if physicalPresent {
		oldsizeInfoPtr = &existing.sizeInfo
	}
	if err := saveWithSplit(store.context, store.context.Transaction(), existing.pending.physical.records, target.primaryKey, stored,
		store.metaData.IsSplitLongRecords(), store.omitUnsplitRecordSuffix(), oldsizeInfoPtr, newsizeInfo); err != nil {
		return fmt.Errorf("failed to save record: %w", err)
	}
	if target.version != nil {
		return store.saveRecordVersionAtKey(existing.pending.physical.versionKey, target.version, newsizeInfo)
	}
	return nil
}

// InsertRecord saves a record and throws an error if it already exists.
// This is equivalent to SaveRecordWithOptions(record, RecordExistenceCheckErrorIfExists).
//
// Java equivalent: FDBRecordStore.insertRecordAsync(Message rec)
// Java location: fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/provider/foundationdb/FDBRecordStoreBase.java:629
//
// Returns:
//   - *FDBStoredRecord: The saved record with metadata
//   - error: RecordAlreadyExistsError if a record with the same primary key already exists
func (store *FDBRecordStore) InsertRecord(record proto.Message) (*FDBStoredRecord[proto.Message], error) {
	return store.SaveRecordWithOptions(record, RecordExistenceCheckErrorIfExists)
}

// UpdateRecord saves a record and throws an error if it does not already exist or if its type has changed.
// This is equivalent to SaveRecordWithOptions(record, RecordExistenceCheckErrorIfNotExistsOrTypeChanged).
//
// Java equivalent: FDBRecordStore.updateRecordAsync(Message rec)
// Java location: fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/provider/foundationdb/FDBRecordStoreBase.java:649
//
// Returns:
//   - *FDBStoredRecord: The saved record with metadata
//   - error: RecordDoesNotExistError if no record exists with this primary key
//   - error: RecordTypeChangedError if an existing record has a different type
func (store *FDBRecordStore) UpdateRecord(record proto.Message) (*FDBStoredRecord[proto.Message], error) {
	return store.SaveRecordWithOptions(record, RecordExistenceCheckErrorIfNotExistsOrTypeChanged)
}

// serializeUnionOver marshals a record into the UnionDescriptor wire format
// without allocating a UnionDescriptor struct: tag(fieldNum, LEN) + varint(len)
// + innerBytes, wire-compatible with Java's UnionDescriptor serialization. The
// record replaces a stored one whose record bytes (inside the union) are
// priorInner, nil for a new record or one of another type (priorRecordInner):
// a type that reaches a map field keeps
// each map in the order priorInner stored it (marshalMapRecord,
// internal/recordlayer/rlwire/map_order_merge.go), and is never written with vtproto's MarshalVT,
// whose map order is Go's random iteration order.
func serializeUnionOver(record proto.Message, recordType *RecordType, priorInner []byte) ([]byte, error) {
	if err := recordType.checkUTF8Strings(record); err != nil {
		return nil, err
	}
	return serializeUnionOverChecked(record, recordType, priorInner)
}

// serializeUnionOverChecked is serializeUnionOver for a record whose strings
// the same save call has already passed through checkUTF8Strings (the record
// itself, or its asJavaForSave write view, which differs only in enums).
func serializeUnionOverChecked(record proto.Message, recordType *RecordType, priorInner []byte) ([]byte, error) {
	if recordType.unionFieldNumber == 0 {
		return nil, fmt.Errorf("no union field number for record type: %s", recordType.Name)
	}
	if recordType.reachesMap {
		if priorInner == nil && recordType.reachesClosedEnum && holdsUndeclared(record.ProtoReflect(), recordType.closedEnumReach) {
			// A map value holding an undeclared closed-enum number (the only
			// one asJavaForSave's write view keeps): a new record's maps go
			// through the rewrite too, which writes that entry in Java's form.
			priorInner = []byte{}
		}
		innerBytes, err := marshalMapRecord(record, priorInner, recordType.mapReach, recordType.closedEnumReach)
		if err != nil {
			return nil, err
		}
		out := make([]byte, 0, 10+len(innerBytes))
		out = protowire.AppendTag(out, recordType.unionFieldNumber, protowire.BytesType)
		return protowire.AppendBytes(out, innerBytes), nil
	}

	// Fast path: if SizeVT is available, compute size first and allocate once.
	// This avoids the intermediate MarshalVT allocation.
	type sizer interface {
		SizeVT() int
		MarshalToSizedBufferVT([]byte) (int, error)
	}
	if sv, ok := record.(sizer); ok {
		innerSize := sv.SizeVT()
		// Allocate single buffer: tag + length varint + inner bytes
		tagSize := protowire.SizeTag(recordType.unionFieldNumber)
		lenSize := protowire.SizeBytes(innerSize) - innerSize // just the varint length
		totalSize := tagSize + lenSize + innerSize
		out := make([]byte, totalSize)

		// Write tag + length prefix
		header := protowire.AppendTag(out[:0], recordType.unionFieldNumber, protowire.BytesType)
		header = protowire.AppendVarint(header, uint64(innerSize))
		headerLen := len(header)

		// Marshal directly into the remaining buffer
		_, err := sv.MarshalToSizedBufferVT(out[headerLen : headerLen+innerSize])
		if err != nil {
			return nil, err
		}
		return out[:headerLen+innerSize], nil
	}

	// Slow path: marshal first, then wrap
	var innerBytes []byte
	var err error
	if vm, ok := record.(interface{ MarshalVT() ([]byte, error) }); ok {
		innerBytes, err = vm.MarshalVT()
	} else {
		// Deterministic marshal so the persisted record bytes are BYTE-STABLE: the same record
		// value marshals to the same bytes every time, so rewriting a row whose content did not
		// change produces bytes that did not change. Generated protos take the VT paths above,
		// which emit a fixed, code-generated field order; this reflection path is hit by dynamic
		// messages (dynamicpb — SQL rows), whose default marshal iterates fields in Go map order
		// and so serializes the same row to DIFFERENT bytes across writes.
		//
		// Stability is the whole of what Deterministic:true buys — NOT a canonical encoding. It
		// selects order.LegacyFieldOrder (extensions first, then non-oneof fields before oneof
		// fields, then by field number) and sorts map entries by key; upstream explicitly
		// disclaims that this output is canonical across languages or stable across releases.
		// So this is not a claim that the bytes match what Java emits for the same record. Wire
		// compatibility does not need one: any permutation of top-level fields is a valid
		// encoding of the same message, and decoders — Java's included — are order-agnostic.
		//
		// Surfaced by the RFC-199 SQL workload's determinism oracle, which replays a seed and
		// diffs the persisted bytes.
		innerBytes, err = proto.MarshalOptions{Deterministic: true}.Marshal(record)
	}
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 10+len(innerBytes))
	out = protowire.AppendTag(out, recordType.unionFieldNumber, protowire.BytesType)
	out = protowire.AppendBytes(out, innerBytes)
	return out, nil
}

// writeStoredRecord is the bytes a record's union message is stored as: the
// store's serializer's transformation, or the union message itself. A failed
// write-time validation names the record, as Java's
// RecordSerializationValidationException does.
func (store *FDBRecordStore) writeStoredRecord(union []byte, recordType *RecordType, primaryKey tuple.Tuple) ([]byte, error) {
	if store.serializer == nil {
		return union, nil
	}
	// The serialization validation deserializes what it reads back as this
	// store reads a record (Java's inner serializer).
	read := func(b []byte) (proto.Message, error) {
		_, msg, _, err := store.discoverUnion(b)
		return msg, err
	}
	stored, err := store.serializer.transformRead(union, store.Env(), read)
	var ve *RecordSerializationValidationError
	if errors.As(err, &ve) {
		ve.RecordType = recordType.Name
		ve.PrimaryKey = primaryKey
	}
	return stored, err
}
