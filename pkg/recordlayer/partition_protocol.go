package recordlayer

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"sync"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const partitionNamespace = "fdb_go.interval_partitions"

// PartitionStateError reports corrupt, unsupported, or missing routing state.
type PartitionStateError struct {
	RecordType string
	IndexName  string
	Coordinate int64
	Reason     string
	Cause      error
}

func (e *PartitionStateError) Error() string {
	return fmt.Sprintf("partition state: %s (record_type=%q, index=%q, coordinate=%d)", e.Reason, e.RecordType, e.IndexName, e.Coordinate)
}
func (e *PartitionStateError) Unwrap() error { return e.Cause }

// PartitionMutationError prevents committing an incomplete partition mutation.
type PartitionMutationError struct{ Cause error }

func (e *PartitionMutationError) Error() string {
	if e.Cause == nil {
		return "incomplete partition mutation"
	}
	return "incomplete partition mutation: " + e.Cause.Error()
}
func (e *PartitionMutationError) Unwrap() error { return e.Cause }

type partitionSessionID string

type partitionContextState struct {
	mu           sync.Mutex
	failed       error
	epochChanged map[string]bool
	revisions    map[string]int64
	nonce        [16]byte
	nonceReady   bool
	nonceErr     error
}

type partitionMutation struct {
	store    *FDBRecordStore
	state    *partitionContextState
	writes   []partitionProtocolWrite
	effects  bool
	complete bool
	locked   bool
	epochs   map[string]bool
	headers  map[string][]byte
	preview  bool
	topology map[string]bool
}

type partitionProtocolWrite struct{ key, value []byte }

func partitionSubspace(store subspace.Subspace, rt *RecordType) subspace.Subspace {
	return store.Sub(partitionNamespace, int64(1), rt.GetRecordTypeKey())
}

func (store *FDBRecordStore) hasPartitionedTypes() bool {
	for _, rt := range store.metaData.RecordTypes() {
		if len(partitionFunctions(rt.PrimaryKey)) != 0 {
			return true
		}
	}
	return false
}

func (store *FDBRecordStore) indexIsPartitioned(index *Index) bool {
	for _, rt := range store.metaData.RecordTypesForIndex(index) {
		if len(partitionFunctions(rt.PrimaryKey)) != 0 {
			return true
		}
	}
	return false
}

// Session initialization is atomic across store handles. Session and commit
// registration locks are never nested; preparation locks before state/physical
// locks and releases before callbacks. Completion rearms an already-started guard.
func (store *FDBRecordStore) partitionContextState() *partitionContextState {
	rc := store.context
	id := partitionSessionID(string(store.subspace.Bytes()))
	rc.sessionMu.Lock()
	state, _ := rc.session[id].(*partitionContextState)
	if state == nil {
		state = &partitionContextState{}
		if rc.session == nil {
			rc.session = make(map[any]any)
		}
		rc.session[id] = state
	}
	rc.sessionMu.Unlock()
	return state
}

func (store *FDBRecordStore) partitionMutationState() *partitionContextState {
	state := store.partitionContextState()
	rc := store.context
	id := partitionSessionID(string(store.subspace.Bytes()))
	rc.GetOrCreateCommitCheck("partition-mutation:"+string(id), func(string) CommitCheckFunc {
		return func() error {
			state.mu.Lock()
			defer state.mu.Unlock()
			return state.failed
		}
	})
	return state
}

func (store *FDBRecordStore) beginRecordPartitionMutation(rt *RecordType, primaryKey tuple.Tuple) (*partitionMutation, error) {
	if len(partitionFunctions(rt.PrimaryKey)) != 0 {
		return store.beginPartitionMutation()
	}
	// A loaded plain body cannot turn a partitioned physical key into an
	// unfenced plain delete. Ordinary type-prefixed siblings need no guard.
	if len(primaryKey) >= 3 && !recordTypeKeyEquals(primaryKey[0], rt.GetRecordTypeKey()) {
		for _, candidate := range store.metaData.RecordTypes() {
			if len(partitionFunctions(candidate.PrimaryKey)) != 0 && recordTypeKeyEquals(primaryKey[0], candidate.GetRecordTypeKey()) {
				return store.beginPartitionMutation()
			}
		}
	}
	return nil, nil
}

func (store *FDBRecordStore) beginPartitionMutation() (*partitionMutation, error) {
	if !store.hasPartitionedTypes() {
		return nil, nil
	}
	state := store.partitionMutationState()
	state.mu.Lock()
	if state.failed != nil {
		state.mu.Unlock()
		return nil, state.failed
	}
	return &partitionMutation{store: store, state: state, locked: true, epochs: make(map[string]bool), headers: make(map[string][]byte)}, nil
}

func (op *partitionMutation) finish(causes ...error) {
	if op == nil {
		return
	}
	var invalidated error
	if op.effects {
		_, invalidated = op.validateIncarnations()
	}
	if !op.locked {
		op.state.mu.Lock()
	}
	if op.effects && (!op.complete || invalidated != nil) && op.state.failed == nil {
		if invalidated == nil && len(causes) != 0 {
			invalidated = causes[0]
		}
		if invalidated == nil {
			invalidated = &RecordCoreError{Message: "partition operation failed after staging mutations"}
		}
		op.state.failed = &PartitionMutationError{Cause: invalidated}
	}
	op.state.mu.Unlock()
	if op.effects {
		op.store.context.rearmCommitCheck("partition-mutation:" + string(op.store.subspace.Bytes()))
	}
}

func (op *partitionMutation) succeed() {
	if op != nil {
		op.complete = true
	}
}

func (op *partitionMutation) apply() {
	if op == nil {
		return
	}
	op.effects = true
	for key := range op.topology {
		op.state.revisions[key]++
	}
	for _, write := range op.writes {
		op.store.context.Transaction().Set(fdb.Key(write.key), write.value)
	}
	op.writes = nil
	if op.state.epochChanged == nil {
		op.state.epochChanged = make(map[string]bool)
	}
	for key := range op.epochs {
		op.state.epochChanged[key] = true
	}
	op.locked = false
	op.state.mu.Unlock()
}

func (op *partitionMutation) releasePreflight() {
	op.writes = nil
	op.headers = nil
	op.epochs = nil
	op.topology = nil
	op.locked = false
	op.state.mu.Unlock()
}

// Capture raw identities without validating routing ahead of per-row existence
// and serializer checks. Each applied child must preserve or create its own identity.
func (op *partitionMutation) captureIncarnations() error {
	if op.headers == nil {
		op.headers = make(map[string][]byte)
	}
	for _, rt := range op.store.metaData.RecordTypes() {
		if len(partitionFunctions(rt.PrimaryKey)) == 0 {
			continue
		}
		key := partitionSubspace(op.store.subspace, rt).Pack(tuple.Tuple{"header"})
		raw, err := op.store.context.Transaction().Get(key).Get()
		if err != nil {
			return err
		}
		op.headers[string(key)] = raw
	}
	return nil
}

// Reuse persisted identities for completion checks and the sticky commit
// guard. The bool distinguishes actual identity loss from a read failure.
func (op *partitionMutation) validateIncarnations() (bool, error) {
	for key, expected := range op.headers {
		actual, err := op.store.context.Transaction().Get(fdb.Key(key)).Get()
		if err != nil {
			return false, &PartitionStateError{Reason: "cannot revalidate partition incarnation", Cause: err}
		}
		if !bytes.Equal(actual, expected) {
			return true, &PartitionStateError{Reason: "table incarnation changed during partition mutation"}
		}
	}
	return false, nil
}

func (op *partitionMutation) collectHeaders(child *partitionMutation) error {
	if op == nil || child == nil {
		return nil
	}
	if op.headers == nil {
		op.headers = make(map[string][]byte)
	}
	for key, header := range child.headers {
		if expected, seen := op.headers[key]; seen && !bytes.Equal(expected, header) {
			created := false
			if expected == nil {
				for _, write := range child.writes {
					if string(write.key) == key && bytes.Equal(write.value, header) {
						created = true
						break
					}
				}
			}
			if !created {
				return &PartitionStateError{Reason: "table incarnation changed during partition mutation"}
			}
		}
		op.headers[key] = header
	}
	return nil
}

func (op *partitionMutation) resumePreparation() error {
	op.state.mu.Lock()
	if op.state.failed != nil {
		op.state.mu.Unlock()
		return op.state.failed
	}
	op.locked = true
	op.epochs = make(map[string]bool)
	if op.headers == nil {
		op.headers = make(map[string][]byte)
	}
	return nil
}

func (store *FDBRecordStore) previewPartitionRoute(rt *RecordType, primaryKey tuple.Tuple, record proto.Message, oldRT *RecordType, oldRecord proto.Message) error {
	guardType := rt
	if oldRT != nil && len(partitionFunctions(oldRT.PrimaryKey)) != 0 {
		guardType = oldRT
	}
	op, err := store.beginRecordPartitionMutation(guardType, primaryKey)
	if err != nil || op == nil {
		return err
	}
	defer op.finish()
	op.preview = true
	if err := op.prepareRoute(rt, primaryKey, record, oldRT == nil); err != nil {
		return err
	}
	if oldRT != nil {
		return op.prepareRoute(oldRT, primaryKey, oldRecord, false)
	}
	return nil
}

func (op *partitionMutation) read(key []byte) ([]byte, error) {
	for i := len(op.writes) - 1; i >= 0; i-- {
		if bytes.Equal(op.writes[i].key, key) {
			return op.writes[i].value, nil
		}
	}
	return op.store.context.Transaction().Get(fdb.Key(key)).Get()
}

func (op *partitionMutation) stage(key []byte, value tuple.Tuple) {
	op.writes = append(op.writes, partitionProtocolWrite{key: key, value: value.Pack()})
}

func (store *FDBRecordStore) partitionedSaveGroup(records []proto.Message) bool {
	for _, record := range records {
		if record == nil {
			continue
		}
		rt, err := store.metaData.GetRecordTypeForDescriptor(record.ProtoReflect().Descriptor())
		if err == nil && len(partitionFunctions(rt.PrimaryKey)) != 0 {
			return true
		}
	}
	return false
}

func (store *FDBRecordStore) savePartitionedGroup(records []proto.Message, check RecordExistenceCheck, written indexWriteSet, depth int) (_ []*FDBStoredRecord[proto.Message], resultErr error) {
	loads := make([]existingRecordLoad, len(records))
	for i, record := range records {
		target, err := store.saveTargetFor(record, nil, VersionstampSaveBehaviorDefault)
		if err != nil {
			return nil, err
		}
		loads[i].target = target
	}
	op, err := store.beginPartitionMutation()
	if err != nil {
		return nil, err
	}
	defer func() { op.finish(resultErr) }()
	op.releasePreflight()
	if err := op.captureIncarnations(); err != nil {
		return nil, err
	}
	for i := range loads {
		loads[i].partitionParent = op
	}
	results := make([]*FDBStoredRecord[proto.Message], 0, len(records))
	depth = max(1, depth)
	for start := 0; start < len(records); start += depth {
		end := min(start+depth, len(records))
		seen := make(map[string]bool)
		for i := start; i < end; i++ {
			key := string(loads[i].target.primaryKey.Pack())
			if !seen[key] {
				loads[i].pending = store.startAuthoritativeRecordLoad(store.context.Transaction(), loads[i].target.primaryKey)
				loads[i].sent = true
				seen[key] = true
			}
		}
		for i := start; i < end; i++ {
			saved, err := store.saveRecordInternal(records[i], check, nil, VersionstampSaveBehaviorDefault, false, &loads[i], nil, written)
			if err != nil {
				return nil, err
			}
			op.effects = true
			results = append(results, saved)
		}
	}
	if err := store.addBatchIndexWriteConflicts(written); err != nil {
		return nil, err
	}
	op.succeed()
	return results, nil
}

func partitionStateError(rt *RecordType, coordinate int64, reason string, cause error) error {
	return &PartitionStateError{RecordType: rt.Name, Coordinate: coordinate, Reason: reason, Cause: cause}
}

func partitionCounter(rt *RecordType, coordinate int64, raw []byte) (int64, error) {
	if len(raw) > 9 {
		return 0, partitionStateError(rt, coordinate, "malformed counter", nil)
	}
	parts, err := tuple.Unpack(raw)
	if err != nil || len(parts) != 1 {
		return 0, partitionStateError(rt, coordinate, "malformed counter", err)
	}
	n, ok := parts[0].(int64)
	if !ok || n <= 0 || !bytes.Equal(raw, (tuple.Tuple{n}).Pack()) {
		return 0, partitionStateError(rt, coordinate, "counter must be a canonical positive BIGINT", nil)
	}
	return n, nil
}

func nextPartitionCounter(rt *RecordType, coordinate, previous int64) (int64, error) {
	if previous < 0 || previous == math.MaxInt64 {
		return 0, partitionStateError(rt, coordinate, "counter exhausted", nil)
	}
	return previous + 1, nil
}

func (store *FDBRecordStore) partitionPrefixEmpty(prefix []byte) (bool, error) {
	r, err := fdb.PrefixRange(prefix)
	if err != nil {
		return false, err
	}
	rows, err := store.context.Transaction().GetRange(r, fdb.RangeOptions{Limit: 1}).GetSliceWithError()
	return len(rows) == 0, err
}

func (store *FDBRecordStore) partitionRecordPrefixEmpty(prefix tuple.Tuple) (bool, error) {
	for _, ss := range []subspace.Subspace{store.recordsSubspace, store.subspace.Sub(RecordVersionKey)} {
		key := ss.Pack(prefix)
		empty, err := store.partitionPrefixEmpty(key)
		if err != nil || !empty {
			return empty, err
		}
		r, err := fdb.PrefixRange(key)
		if err != nil {
			return false, err
		}
		if store.context.HasVersionMutationsInRange(r.Begin.FDBKey(), r.End.FDBKey()) {
			return false, nil
		}
		store.context.versionMu.Lock()
		cached := false
		for versionKey := range store.context.localVersionCache {
			if bytes.HasPrefix([]byte(versionKey), key) {
				cached = true
				break
			}
		}
		store.context.versionMu.Unlock()
		if cached {
			return false, nil
		}
	}
	return true, nil
}

func partitionHeader(rt *RecordType, spec *PartitionSpec, incarnation []byte) tuple.Tuple {
	field := rt.Descriptor.Fields().ByName(protoreflect.Name(spec.SourceColumn()))
	return tuple.Tuple{int64(1), int64(field.Number()), int64(field.Kind()), spec.IntervalWidth(), incarnation}
}

func decodePartitionHeader(rt *RecordType, spec *PartitionSpec, raw []byte) error {
	if len(raw) > 128 {
		return partitionStateError(rt, 0, "malformed layout header", nil)
	}
	parts, err := tuple.Unpack(raw)
	if err != nil || len(parts) != 5 {
		return partitionStateError(rt, 0, "malformed layout header", err)
	}
	version, ok := parts[0].(int64)
	if !ok || version != 1 {
		return partitionStateError(rt, 0, "unsupported routing header version", nil)
	}
	incarnation, ok := parts[4].([]byte)
	if !ok || len(incarnation) != 16 || !bytes.Equal(raw, partitionHeader(rt, spec, incarnation).Pack()) {
		return partitionStateError(rt, 0, "layout identity or incarnation mismatch", nil)
	}
	return nil
}

func decodePartitionFence(rt *RecordType, coordinate int64, raw []byte) (int64, bool, error) {
	if len(raw) > 10 {
		return 0, false, partitionStateError(rt, coordinate, "malformed fence", nil)
	}
	parts, err := tuple.Unpack(raw)
	if err != nil || len(parts) != 2 {
		return 0, false, partitionStateError(rt, coordinate, "malformed fence", err)
	}
	generation, ok := parts[0].(int64)
	live, liveOK := parts[1].(bool)
	if !ok || generation <= 0 || !liveOK || !bytes.Equal(raw, (tuple.Tuple{generation, live}).Pack()) {
		return 0, false, partitionStateError(rt, coordinate, "invalid fence generation or state", nil)
	}
	return generation, live, nil
}

// The state lock spans preflight and apply. A revision is reserved before any
// effect, independently of the once-per-transaction persisted epoch write.
func (op *partitionMutation) prepareTopology(rt *RecordType) error {
	key := string(partitionSubspace(op.store.subspace, rt).Bytes())
	if op.state.revisions[key] == math.MaxInt64 {
		return partitionStateError(rt, 0, "local topology revision exhausted", nil)
	}
	if op.state.revisions == nil {
		op.state.revisions = make(map[string]int64)
	}
	if op.topology == nil {
		op.topology = make(map[string]bool)
	}
	op.topology[key] = true
	return nil
}

func (op *partitionMutation) readPartitionHeader(rt *RecordType, spec *PartitionSpec) ([]byte, error) {
	ss := partitionSubspace(op.store.subspace, rt)
	raw, err := op.read(ss.Pack(tuple.Tuple{"header"}))
	if err != nil {
		return nil, err
	}
	if raw == nil {
		if expected := op.headers[string(ss.Pack(tuple.Tuple{"header"}))]; expected != nil {
			return nil, partitionStateError(rt, 0, "table incarnation changed during partition mutation", nil)
		}
		return nil, nil
	}
	if err := decodePartitionHeader(rt, spec, raw); err != nil {
		return nil, err
	}
	key := string(ss.Pack(tuple.Tuple{"header"}))
	if expected, seen := op.headers[key]; seen && !bytes.Equal(expected, raw) {
		return nil, partitionStateError(rt, 0, "table incarnation changed during partition mutation", nil)
	}
	op.headers[key] = raw
	return raw, nil
}

func (op *partitionMutation) stageHeader(rt *RecordType, spec *PartitionSpec, incarnation []byte) {
	key := partitionSubspace(op.store.subspace, rt).Pack(tuple.Tuple{"header"})
	value := partitionHeader(rt, spec, incarnation)
	op.stage(key, value)
	op.headers[string(key)] = value.Pack()
}

func (op *partitionMutation) prepareHeader(rt *RecordType, spec *PartitionSpec, allowCreate bool) error {
	store := op.store
	raw, err := op.readPartitionHeader(rt, spec)
	if err != nil || raw != nil {
		return err
	}
	ss := partitionSubspace(store.subspace, rt)
	empty, err := store.partitionRecordPrefixEmpty(tuple.Tuple{rt.GetRecordTypeKey()})
	if err != nil {
		return err
	}
	if !empty {
		return partitionStateError(rt, 0, "record or version data exist without a routing header", nil)
	}
	empty, err = store.partitionPrefixEmpty(ss.Bytes())
	if err != nil {
		return err
	}
	if !empty || !allowCreate {
		return partitionStateError(rt, 0, "missing routing header", nil)
	}
	if err := op.prepareTopology(rt); err != nil {
		return err
	}
	incarnation := make([]byte, 16)
	if !op.preview {
		if _, err := io.ReadFull(store.context.Env(), incarnation); err != nil {
			return partitionStateError(rt, 0, "cannot generate incarnation", err)
		}
	}
	op.stageHeader(rt, spec, incarnation)
	return nil
}

func (op *partitionMutation) prepareRoute(rt *RecordType, primaryKey tuple.Tuple, record proto.Message, activate bool) error {
	if op == nil {
		return nil
	}
	if record != nil && hasRecordTypeKeyPrefix(rt.PrimaryKey) && (len(primaryKey) == 0 || !recordTypeKeyEquals(primaryKey[0], rt.GetRecordTypeKey())) {
		return partitionStateError(rt, 0, "record type does not match physical partition prefix", nil)
	}
	spec, err := rt.PartitionSpec()
	if err != nil || spec == nil {
		return err
	}
	if len(primaryKey) < 3 || !recordTypeKeyEquals(primaryKey[0], rt.GetRecordTypeKey()) {
		return partitionStateError(rt, 0, "invalid physical primary key", nil)
	}
	coordinateParts, err := tuple.Unpack((tuple.Tuple{primaryKey[1]}).Pack())
	if err != nil || len(coordinateParts) != 1 {
		return partitionStateError(rt, 0, "coordinate is not a BIGINT", err)
	}
	coordinate, ok := coordinateParts[0].(int64)
	if !ok {
		return partitionStateError(rt, 0, "coordinate is not a BIGINT", nil)
	}
	minimum, _ := spec.Coordinate(math.MinInt64)
	maximum, _ := spec.Coordinate(math.MaxInt64)
	if coordinate < minimum || coordinate > maximum {
		return partitionStateError(rt, coordinate, "coordinate is outside the source domain", nil)
	}
	if record != nil {
		key, err := evaluateKeyFlat(rt.PrimaryKey, &FDBStoredRecord[proto.Message]{RecordType: rt, Record: record}, record)
		if err != nil {
			return partitionStateError(rt, coordinate, "record route cannot be evaluated", err)
		}
		keyTuple := make(tuple.Tuple, len(key))
		for i, value := range key {
			keyTuple[i] = value
		}
		if !bytes.Equal(keyTuple.Pack(), primaryKey.Pack()) {
			return partitionStateError(rt, coordinate, "record does not match its physical primary key", nil)
		}
	}
	if err := op.prepareHeader(rt, spec, activate); err != nil {
		return err
	}
	store := op.store
	ss := partitionSubspace(store.subspace, rt)
	activeKey := ss.Pack(tuple.Tuple{"active", coordinate})
	fenceKey := ss.Pack(tuple.Tuple{"fence", coordinate})
	active, err := op.read(activeKey)
	if err != nil {
		return err
	}
	fence, err := op.read(fenceKey)
	if err != nil {
		return err
	}
	var generation int64
	if fence != nil {
		var live bool
		generation, live, err = decodePartitionFence(rt, coordinate, fence)
		if err != nil {
			return err
		}
		if live {
			n, err := partitionCounter(rt, coordinate, active)
			if err != nil {
				return err
			}
			if n != generation {
				return partitionStateError(rt, coordinate, "active and fence generations disagree", nil)
			}
			return nil
		}
	}
	if active != nil {
		return partitionStateError(rt, coordinate, "active entry has no live fence", nil)
	}
	empty, err := store.partitionRecordPrefixEmpty(tuple.Tuple{rt.GetRecordTypeKey(), coordinate})
	if err != nil {
		return err
	}
	if !empty || !activate {
		return partitionStateError(rt, coordinate, "record or version route has no active directory entry", nil)
	}
	generation, err = nextPartitionCounter(rt, coordinate, generation)
	if err != nil {
		return err
	}
	epochKey := ss.Pack(tuple.Tuple{"epoch"})
	epochRaw, err := op.read(epochKey)
	if err != nil {
		return err
	}
	var epoch int64
	if epochRaw != nil {
		epoch, err = partitionCounter(rt, coordinate, epochRaw)
		if err != nil {
			return err
		}
	} else if header, err := store.context.Transaction().Get(ss.Pack(tuple.Tuple{"header"})).Get(); err != nil {
		return err
	} else if header != nil {
		return partitionStateError(rt, coordinate, "missing topology epoch", nil)
	}
	if !op.state.epochChanged[string(epochKey)] && !op.epochs[string(epochKey)] {
		epoch, err = nextPartitionCounter(rt, coordinate, epoch)
		if err != nil {
			return err
		}
		op.stage(epochKey, tuple.Tuple{epoch})
		op.epochs[string(epochKey)] = true
	}
	if err := op.prepareTopology(rt); err != nil {
		return err
	}
	op.stage(activeKey, tuple.Tuple{generation})
	op.stage(fenceKey, tuple.Tuple{generation, true})
	return nil
}

func (store *FDBRecordStore) partitionIndexMutation(index *Index, indexKey, primaryKey tuple.Tuple, mutate func() error) (resultErr error) {
	if !store.indexIsPartitioned(index) {
		return mutate()
	}
	finishWrite, err := store.context.beginRecordWrite()
	if err != nil {
		return err
	}
	defer finishWrite()
	op, err := store.beginPartitionMutation()
	if err != nil {
		return err
	}
	defer func() { op.finish(resultErr) }()
	types := store.metaData.RecordTypesForIndex(index)
	if len(types) != 1 || len(indexKey) == 0 || len(primaryKey) < 3 || !recordTypeKeyEquals(indexKey[0], primaryKey[1]) {
		return &PartitionStateError{IndexName: index.Name, Reason: "raw index mutation does not identify one matching partition"}
	}
	if err := op.prepareRoute(types[0], primaryKey, nil, false); err != nil {
		return err
	}
	op.apply()
	if err := mutate(); err != nil {
		return err
	}
	op.succeed()
	return nil
}

// A wrong-type body must not be filtered out before its physical partition
// prefix is checked, or rebuilding would silently publish an incomplete index.
func (store *FDBRecordStore) checkPartitionBackfillRecordType(rec *FDBStoredRecord[proto.Message]) error {
	if len(rec.PrimaryKey) < 3 || recordTypeKeyEquals(rec.PrimaryKey[0], rec.RecordType.GetRecordTypeKey()) {
		return nil
	}
	op, err := store.beginRecordPartitionMutation(rec.RecordType, rec.PrimaryKey)
	if err != nil || op == nil {
		return err
	}
	defer op.finish()
	return op.prepareRoute(rec.RecordType, rec.PrimaryKey, rec.Record, false)
}

// Backfill validates the stored route and never creates directory state.
func (store *FDBRecordStore) updatePartitionedBackfill(maintainer IndexMaintainer, rec *FDBStoredRecord[proto.Message]) (resultErr error) {
	if len(partitionFunctions(rec.RecordType.PrimaryKey)) == 0 {
		return maintainer.Update(nil, rec)
	}
	finishWrite, err := store.context.beginRecordWrite()
	if err != nil {
		return err
	}
	defer finishWrite()
	op, err := store.beginPartitionMutation()
	if err != nil {
		return err
	}
	defer func() { op.finish(resultErr) }()
	if err := op.prepareRoute(rec.RecordType, rec.PrimaryKey, rec.Record, false); err != nil {
		return err
	}
	op.apply()
	if err := maintainer.Update(nil, rec); err != nil {
		return err
	}
	op.succeed()
	return nil
}

func (op *partitionMutation) prepareBackfillTable(rt *RecordType) error {
	if op == nil {
		return nil
	}
	spec, err := rt.PartitionSpec()
	if err != nil || spec == nil {
		return err
	}
	header, err := op.readPartitionHeader(rt, spec)
	if err != nil {
		return err
	}
	if header == nil {
		empty, err := op.store.partitionRecordPrefixEmpty(tuple.Tuple{rt.GetRecordTypeKey()})
		if err != nil {
			return err
		}
		if !empty {
			return partitionStateError(rt, 0, "backfill found records without a routing header", nil)
		}
		empty, err = op.store.partitionPrefixEmpty(partitionSubspace(op.store.subspace, rt).Bytes())
		if err != nil {
			return err
		}
		if !empty {
			return partitionStateError(rt, 0, "backfill found extension state without a routing header", nil)
		}
		return nil
	}
	return op.prepareTableFence(rt)
}

func (op *partitionMutation) prepareTableExclusion(rt *RecordType) error {
	if op == nil {
		return nil
	}
	spec, err := rt.PartitionSpec()
	if err != nil || spec == nil {
		return err
	}
	if err := op.prepareHeader(rt, spec, true); err != nil {
		return err
	}
	ss := partitionSubspace(op.store.subspace, rt)
	if err := op.store.context.Transaction().AddReadConflictRange(ss.Sub("fence")); err != nil {
		return err
	}
	if err := op.store.context.Transaction().AddWriteConflictRange(ss.Sub("fence")); err != nil {
		return err
	}
	if raw, err := op.read(ss.Pack(tuple.Tuple{"epoch"})); err != nil {
		return err
	} else if raw == nil {
		if header, err := op.store.context.Transaction().Get(ss.Pack(tuple.Tuple{"header"})).Get(); err != nil {
			return err
		} else if header != nil {
			return partitionStateError(rt, 0, "missing topology epoch", nil)
		}
		op.stage(ss.Pack(tuple.Tuple{"epoch"}), tuple.Tuple{int64(1)})
		op.epochs[string(ss.Pack(tuple.Tuple{"epoch"}))] = true
	} else if _, err := partitionCounter(rt, 0, raw); err != nil {
		return err
	}
	return nil
}

func (op *partitionMutation) prepareTableFence(rt *RecordType) error {
	if op == nil {
		return nil
	}
	if err := op.prepareTableExclusion(rt); err != nil {
		return err
	}
	spec, err := rt.PartitionSpec()
	if err != nil || spec == nil {
		return err
	}
	// One physical landmark per occupied interval, never one read per record.
	r, err := fdb.PrefixRange(op.store.recordsSubspace.Pack(tuple.Tuple{rt.GetRecordTypeKey()}))
	if err != nil {
		return err
	}
	for {
		rows, err := op.store.context.Transaction().GetRange(r, fdb.RangeOptions{Limit: 1}).GetSliceWithError()
		if err != nil || len(rows) == 0 {
			return err
		}
		parts, err := op.store.recordsSubspace.Unpack(rows[0].Key)
		if err != nil || len(parts) < 3 {
			return partitionStateError(rt, 0, "malformed physical record key", err)
		}
		if err := op.prepareRoute(rt, parts[:3], nil, false); err != nil {
			return err
		}
		partitionPrefix := op.store.recordsSubspace.Pack(parts[:2])
		if !bytes.HasPrefix(rows[0].Key, partitionPrefix) {
			return partitionStateError(rt, 0, "noncanonical physical partition prefix", nil)
		}
		partitionRange, err := fdb.PrefixRange(partitionPrefix)
		if err != nil {
			return err
		}
		r.Begin = partitionRange.End
	}
}

func (op *partitionMutation) prepareReset() error {
	if op == nil {
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(op.store.metaData.RecordTypes())) {
		rt := op.store.metaData.GetRecordType(name)
		spec, err := rt.PartitionSpec()
		if err != nil {
			return err
		}
		if spec == nil {
			continue
		}
		if err := op.prepareTableExclusion(rt); err != nil {
			return err
		}
		if err := op.prepareTopology(rt); err != nil {
			return err
		}
		incarnation := make([]byte, 16)
		if _, err := io.ReadFull(op.store.context.Env(), incarnation); err != nil {
			return err
		}
		ss := partitionSubspace(op.store.subspace, rt)
		op.stageHeader(rt, spec, incarnation)
		op.stage(ss.Pack(tuple.Tuple{"epoch"}), tuple.Tuple{int64(1)})
		op.epochs[string(ss.Pack(tuple.Tuple{"epoch"}))] = true
	}
	return nil
}
