package recordlayer

import (
	"bytes"
	"context"
	"sync"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// PartitionRecordUpdate identifies an existing physical row and its replacement.
// Only validated partitioned record types are accepted; this is not a raw move API.
type PartitionRecordUpdate struct {
	OldPrimaryKey tuple.Tuple
	Record        proto.Message
}

type partitionPreparedUpdate struct {
	oldKey tuple.Tuple
	target saveTarget
}

// PreparedPartitionRecordUpdates owns a snapshot of its inputs and is bound to
// one store/context. Apply is single-use, including unsuccessful applications.
// SQL uses this boundary after evaluating every RHS against its materialized OLD
// rows. The ordinary (nonpartitioned) UPDATE path deliberately does not use it.
type PreparedPartitionRecordUpdates struct {
	mu       sync.Mutex
	used     bool
	store    *FDBRecordStore
	rows     []partitionPreparedUpdate
	parent   *partitionMutation
	retained int64
}

// PreparePartitionRecordUpdates derives and retains the entire final physical
// keys, rejecting immediate destination collisions independently of row order.
// It neither serializes nor claims a local version. Version-dependent primary
// keys are refused, so choosing the actual save version later cannot change a key.
func (store *FDBRecordStore) PreparePartitionRecordUpdates(updates []PartitionRecordUpdate) (*PreparedPartitionRecordUpdates, error) {
	group := &PreparedPartitionRecordUpdates{store: store, rows: make([]partitionPreparedUpdate, 0, len(updates))}
	oldKeys := make(map[string]int, len(updates))
	finalKeys := make(map[string]int, len(updates))
	for i, update := range updates {
		if update.Record == nil {
			return nil, &RecordCoreArgumentError{Message: "partition update requires a record"}
		}
		recordType, err := store.metaData.GetRecordTypeForDescriptor(update.Record.ProtoReflect().Descriptor())
		if err != nil {
			return nil, err
		}
		versionKey := false
		walkPartitionExpressions(recordType.PrimaryKey, func(expr KeyExpression) {
			if _, ok := expr.(*VersionKeyExpression); ok {
				versionKey = true
			}
		})
		if versionKey {
			return nil, &PartitionMetadataError{RecordType: recordType.Name, Message: "partition update primary key cannot depend on a record version"}
		}
		target, err := store.saveTargetFor(proto.Clone(update.Record), nil, VersionstampSaveBehaviorIfPresent)
		if err != nil {
			return nil, err
		}
		layout, err := DescribePartitionKey(target.recordType)
		if err != nil {
			return nil, err
		}
		if layout == nil {
			return nil, &RecordCoreArgumentError{Message: "partition update requires a partitioned record type"}
		}
		oldPacked := update.OldPrimaryKey.Pack()
		old, err := tuple.Unpack(oldPacked)
		if err != nil || len(old) != len(layout.components) {
			return nil, partitionStateError(target.recordType, 0, "invalid old physical primary key", err)
		}
		canonical, err := layout.PhysicalKey(old[2:])
		if err != nil || !bytes.Equal(canonical.Pack(), oldPacked) {
			return nil, partitionStateError(target.recordType, 0, "old physical primary key does not match its route", err)
		}
		oldIdentity, finalIdentity := string(oldPacked), string(target.primaryKey.Pack())
		if _, seen := oldKeys[oldIdentity]; seen {
			return nil, &RecordCoreArgumentError{Message: "partition update contains a duplicate old primary key"}
		}
		if _, seen := finalKeys[finalIdentity]; seen {
			return nil, &RecordAlreadyExistsError{Message: "partition update contains duplicate final primary keys", PrimaryKey: target.primaryKey}
		}
		oldKeys[oldIdentity], finalKeys[finalIdentity] = i, i
		group.rows = append(group.rows, partitionPreparedUpdate{oldKey: old, target: target})
		// Include the caller's transformed message AND our private canonical
		// snapshot, tuple backing storage, packed-key identities and map slots.
		// This conservative reservation also covers the temporary collision maps.
		group.retained += 2*int64(proto.Size(update.Record)) + 2*int64(proto.Size(target.record)) +
			4*int64(len(oldPacked)+len(finalIdentity)) + 32*int64(len(old)+len(target.primaryKey)) + 256
	}
	for identity, i := range finalKeys {
		if oldIndex, occupied := oldKeys[identity]; occupied && oldIndex != i {
			return nil, &RecordAlreadyExistsError{Message: "partition update destination is another selected row's original key", PrimaryKey: group.rows[i].target.primaryKey}
		}
	}
	parent, err := store.beginPartitionMutation()
	if err != nil {
		return nil, err
	}
	if parent == nil {
		return nil, &RecordCoreArgumentError{Message: "partition update requires a partitioned store"}
	}
	parent.releasePreflight()
	if err := parent.captureIncarnations(); err != nil {
		return nil, err
	}
	group.parent = parent
	return group, nil
}

// RetainedMemoryBytes reserves the transformed records, actual NEW packed keys,
// private OLD identities and collision bookkeeping before the first effect.
func (group *PreparedPartitionRecordUpdates) RetainedMemoryBytes() int64 {
	if group == nil {
		return 0
	}
	return group.retained
}

// Apply performs the already-prepared replacements in order. A refusal before
// the first effect remains committable; any later failure poisons the group.
// Dry runs omit update-lock and secondary uniqueness checks, and allocate no
// local versions or pending mutations.
func (group *PreparedPartitionRecordUpdates) Apply(ctx context.Context, dryRun bool) (_ []*FDBStoredRecord[proto.Message], resultErr error) {
	if group == nil || group.store == nil || group.parent == nil {
		return nil, &RecordCoreArgumentError{Message: "partition update group is not initialized"}
	}
	group.mu.Lock()
	if group.used {
		group.mu.Unlock()
		return nil, &RecordCoreArgumentError{Message: "partition update group was already applied"}
	}
	group.used = true
	group.mu.Unlock()
	store := group.store
	finishWrite, err := store.context.beginRecordWrite()
	if err != nil {
		return nil, err
	}
	defer finishWrite()
	defer func() { group.parent.finish(resultErr) }()
	results := make([]*FDBStoredRecord[proto.Message], 0, len(group.rows))
	for _, row := range group.rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result, err := store.applyPartitionRecordUpdate(ctx, row, group.parent, dryRun)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	group.parent.succeed()
	return results, nil
}

// Ownership is deliberately strict: never refresh sizing while retaining a
// stale logical OLD image. Retry callbacks only in a fresh transaction.
func partitionUpdateOwnership(old, destination *pendingStoreRecordLoad) error {
	if old.stamp != old.physical.stamp() || destination.stamp != destination.physical.stamp() {
		return &RecordCoreError{Message: "partition update physical ownership changed; retry in a fresh context"}
	}
	return nil
}

func (store *FDBRecordStore) applyPartitionRecordUpdate(ctx context.Context, row partitionPreparedUpdate, parent *partitionMutation, dryRun bool) (_ *FDBStoredRecord[proto.Message], resultErr error) {
	target := row.target
	var err error
	if dryRun {
		if store.metaData.IsStoreRecordVersions() {
			// Preview the next incomplete version without reserving it or
			// touching pending/local state. Its inline sizes match a real save.
			target.version, err = IncompleteVersion(int(store.context.localVersion.Load()))
		}
	} else {
		// Preserve saveTypedRecord's version-before-load/callback ordering,
		// including exhaustion precedence and serializer-reentrant saves.
		target.version, err = store.recordVersionForSave(nil, VersionstampSaveBehaviorDefault)
	}
	if err != nil {
		return nil, err
	}
	var oldSizes sizeInfo
	old := store.startAuthoritativeRecordLoad(store.context.Transaction(), row.oldKey)
	oldBytes, oldVersion, err := old.finishAuthoritative(&oldSizes)
	if err != nil {
		return nil, err
	}
	if oldBytes == nil {
		if err := old.finishRetiredVersionErrors(); err != nil {
			return nil, err
		}
		return nil, &RecordDoesNotExistError{Message: "record does not exist", PrimaryKey: row.oldKey}
	}
	changed := !bytes.Equal(row.oldKey.Pack(), target.primaryKey.Pack())
	destination := old
	if changed {
		// Observe ownership, not destination data, before OLD deserialization
		// can call user code. Destination read/error precedence stays below.
		physical := store.physicalRecord(target.primaryKey)
		finish, err := physical.lock()
		if err != nil {
			return nil, err
		}
		destination = pendingStoreRecordLoad{physical: physical, stamp: physical.stamp()}
		finish()
	}
	// Checkpoint this row's current incarnation, not the Prepare-time group
	// identity: a completed pre-Apply reset is a pure refusal, while a reset
	// inside a callback is itself an effect. DeleteStore bypasses key stamps.
	identity := partitionMutation{store: store}
	identityErr := identity.captureIncarnations()
	// A callback's writes are effects too. Even an early serializer/filter
	// error must not leave an interfering old/destination mutation committable.
	defer func() {
		if parent.effects {
			return
		}
		finish, err := store.lockPartitionUpdatePhysical()
		if err != nil {
			parent.effects = true
			return
		}
		if ownershipErr := partitionUpdateOwnership(&old, &destination); ownershipErr != nil {
			parent.effects = true
			if resultErr == nil {
				resultErr = ownershipErr
			}
		}
		finish()
		if identityErr == nil {
			changed, err := identity.validateIncarnations()
			parent.effects = parent.effects || changed
			if resultErr == nil {
				resultErr = err
			}
		}
	}()
	oldType, oldMessage, oldWire, err := store.deserializeAndDiscover(oldBytes)
	if err != nil {
		return nil, &RecordDeserializationError{PrimaryKey: row.oldKey, Cause: err}
	}
	oldVersion, err = old.finishVersion(oldVersion)
	if err != nil {
		return nil, err
	}
	if oldType != target.recordType {
		return nil, &RecordTypeChangedError{Message: "record type changed", PrimaryKey: row.oldKey, ActualType: oldType.Name, ExpectedType: target.recordTypeName}
	}
	if changed {
		loaded := store.startAuthoritativeRecordLoad(store.context.Transaction(), target.primaryKey)
		value, _, err := loaded.finishAuthoritative(nil)
		if err != nil {
			return nil, err
		}
		if err := loaded.finishRetiredVersionErrors(); err != nil {
			return nil, err
		}
		if value != nil {
			return nil, &RecordAlreadyExistsError{Message: "record already exists", PrimaryKey: target.primaryKey}
		}
		if destination.stamp != loaded.stamp {
			return nil, &RecordCoreError{Message: "partition update destination ownership changed during OLD deserialization; retry in a fresh context"}
		}
		destination = loaded
	}
	if !dryRun {
		if err := store.validateRecordUpdateAllowed(); err != nil {
			return nil, err
		}
		if err := store.checkRecordWriteCollation(false, target.recordType, oldType); err != nil {
			return nil, err
		}
	}
	data, err := serializeUnionOver(target.write, target.recordType, priorRecordInner(oldType, oldWire, target.recordType))
	if err != nil {
		return nil, &RecordSerializationError{Cause: err}
	}
	serialized, err := store.writeStoredRecord(data, target.recordType, target.primaryKey)
	if err != nil {
		return nil, err
	}
	newRecord := &FDBStoredRecord[proto.Message]{
		PrimaryKey: target.primaryKey, RecordType: target.recordType, Record: target.record, Version: target.version, Store: store,
		wire: newRecordWire(target.recordType, unionInner(data, target.recordType.unionFieldNumber)),
	}
	oldRecord := &FDBStoredRecord[proto.Message]{PrimaryKey: row.oldKey, RecordType: oldType, Record: oldMessage, Version: oldVersion, Store: store, wire: oldWire}
	var indexes []partitionPreparedIndexUpdate
	if !dryRun {
		indexes, err = store.preparePartitionUpdateIndexes(oldRecord, newRecord)
		if err != nil {
			return nil, err
		}
		if len(serialized) > splitRecordSize && !store.metaData.IsSplitLongRecords() {
			return nil, &RecordCoreError{Message: "Record is too long to be stored in a single value; consider split_long_records"}
		}
	}
	sizes := dryRunSaveWithSplitSizeInfo(store.recordsSubspace, target.primaryKey, serialized, store.metaData.IsSplitLongRecords(), store.omitUnsplitRecordSuffix())
	if !dryRun {
		lastKey := target.primaryKey
		if sizes.IsSplit {
			lastKey = appendToTuple(lastKey, int64(sizes.KeyCount))
		} else if store.metaData.IsSplitLongRecords() || !store.omitUnsplitRecordSuffix() {
			lastKey = appendToTuple(lastKey, unsplitRecord)
		}
		keySize := len(store.recordsSubspace.Pack(lastKey))
		if target.version != nil {
			keySize = max(keySize, len(store.versionKey(target.primaryKey)))
		}
		if keySize > keySizeLimit {
			return nil, &RecordCoreArgumentError{Message: "partition update record key exceeds FoundationDB key limit"}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if identityErr != nil {
		return nil, identityErr
	}
	op, err := store.beginPartitionMutation()
	if err != nil {
		return nil, err
	}
	defer func() { op.finish(resultErr) }()
	op.preview = dryRun
	if err := op.prepareRoute(oldType, row.oldKey, oldMessage, false); err != nil {
		return nil, err
	}
	if err := op.prepareRoute(target.recordType, target.primaryKey, target.record, changed); err != nil {
		return nil, err
	}
	if err := parent.collectHeaders(op); err != nil {
		return nil, err
	}
	if dryRun {
		if store.metaData.IsStoreRecordVersions() && !store.useOldVersionFormat() {
			sizes.KeyCount++
			sizes.KeySize += len(store.versionKey(target.primaryKey))
			sizes.ValueSize += VersionBytes + 1
			sizes.VersionedInline = true
		}
		setPartitionUpdateSizes(newRecord, sizes)
		return newRecord, nil
	}
	finishPhysical, err := store.lockPartitionUpdatePhysical()
	if err != nil {
		return nil, err
	}
	var expectedOld, expectedNew recordPhysicalStamp
	err = func() error {
		defer finishPhysical()
		if err := partitionUpdateOwnership(&old, &destination); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// No callbacks or nested public writers under physical ownership.
		op.apply()
		parent.effects = true
		// Invalidate both images even if a subsequent raw effect fails. A
		// read-only validation/refusal must not advance ownership itself.
		old.physical.key.generation++
		if destination.physical.key != old.physical.key {
			destination.physical.key.generation++
		}
		var previous *sizeInfo
		if changed {
			if _, err := deleteSplit(store.context.Transaction(), old.physical.records, row.oldKey, old.physical.split, old.physical.omit, &oldSizes); err != nil {
				return err
			}
		} else {
			previous = &oldSizes
		}
		old.physical.clearVersion()
		if err := saveWithSplit(store.context, store.context.Transaction(), destination.physical.records, target.primaryKey, serialized,
			destination.physical.split, destination.physical.omit, previous, &sizes); err != nil {
			return err
		}
		if newRecord.Version != nil {
			if err := store.saveRecordVersionAtKey(destination.physical.versionKey, newRecord.Version, &sizes); err != nil {
				return err
			}
		}
		expectedOld, expectedNew = old.physical.stamp(), destination.physical.stamp()
		return nil
	}()
	if err != nil {
		return nil, err
	}
	setPartitionUpdateSizes(newRecord, sizes)
	if err := store.applyPartitionUpdateIndexes(indexes, oldRecord, newRecord, changed); err != nil {
		return nil, err
	}
	finishPhysical, err = store.lockPartitionUpdatePhysical()
	if err != nil {
		return nil, err
	}
	err = func() error {
		defer finishPhysical()
		if old.physical.stamp() != expectedOld || destination.physical.stamp() != expectedNew {
			return &RecordCoreError{Message: "partition update physical ownership changed during index maintenance"}
		}
		return nil
	}()
	if err != nil {
		return nil, err
	}
	if _, err := op.validateIncarnations(); err != nil {
		return nil, err
	}
	if err := store.context.CheckTransactionSize(); err != nil {
		return nil, err
	}
	op.succeed()
	return newRecord, nil
}

// Like beginPhysicalBulk, but validation alone does not invalidate images.
// The raw writer advances its two key generations at the first effect instead.
func (store *FDBRecordStore) lockPartitionUpdatePhysical() (func(), error) {
	scope := store.physicalScope()
	store.context.physicalHook("before-bulk", store.recordsSubspace.Bytes())
	if err := store.context.recordWriteContext().Err(); err != nil {
		return nil, err
	}
	scope.gate.Lock()
	if err := store.context.recordWriteContext().Err(); err != nil {
		scope.gate.Unlock()
		return nil, err
	}
	return scope.gate.Unlock, nil
}

func setPartitionUpdateSizes(record *FDBStoredRecord[proto.Message], sizes sizeInfo) {
	record.KeyCount, record.KeySize, record.ValueSize = sizes.KeyCount, sizes.KeySize, sizes.ValueSize
	record.Split, record.VersionedInline = sizes.IsSplit, sizes.VersionedInline
}
