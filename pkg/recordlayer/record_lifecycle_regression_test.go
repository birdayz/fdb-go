package recordlayer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

type lifecycleMutation struct {
	op         string
	key, value []byte
	end        []byte
	err        error
}

type lifecycleTranscript struct {
	mu     sync.Mutex
	events []lifecycleMutation
}

func (l *lifecycleTranscript) note(message string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, lifecycleMutation{op: message})
}

// The ledger lock covers only forwarding one native primitive and recording it;
// it is never held while the logical SaveRecord is suspended.
func (l *lifecycleTranscript) forward(op string, key, value, end []byte, fn func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fn()
	l.events = append(l.events, lifecycleMutation{op: op, key: bytes.Clone(key), value: bytes.Clone(value), end: bytes.Clone(end)})
}

func (l *lifecycleTranscript) print(label string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, event := range l.events {
		fmt.Fprintf(GinkgoWriter, "%s event=%d op=%s key=%x end=%x value=%x err=%v\n", label, i, event.op, event.key, event.end, event.value, event.err)
	}
}

func (l *lifecycleTranscript) first(op string, key []byte) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, event := range l.events {
		if event.op == op && bytes.Equal(event.key, key) {
			return i
		}
	}
	return -1
}

type lifecycleVersionState struct {
	local        int
	cached       bool
	queued       bool
	pendingValue []byte
}

func lifecycleVersionSnapshot(rc *FDBRecordContext, key []byte) lifecycleVersionState {
	rc.versionMu.Lock()
	defer rc.versionMu.Unlock()
	local, cached := rc.localVersionCache[string(key)]
	mutation, queued := rc.versionMutations[string(key)]
	return lifecycleVersionState{local: local, cached: cached, queued: queued, pendingValue: bytes.Clone(mutation.value)}
}

type lifecycleBodyGate struct {
	ctx     context.Context
	key     []byte
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	fired   atomic.Bool
	ledger  *lifecycleTranscript
	onEnter func()
}

func (g *lifecycleBodyGate) unblock() {
	g.once.Do(func() {
		g.ledger.note("body gate released")
		close(g.release)
	})
}

func (g *lifecycleBodyGate) afterSet(key []byte) {
	if (g.key != nil && !bytes.Equal(key, g.key)) || !g.fired.CompareAndSwap(false, true) {
		return
	}
	if g.onEnter != nil {
		g.onEnter()
	}
	g.ledger.note("forwarded native mutation held before the rest of its physical group")
	close(g.entered)
	select {
	case <-g.release:
		g.ledger.note("forwarded native mutation returns after release")
	case <-g.ctx.Done():
		g.ledger.note(fmt.Sprintf("forwarded native mutation released by context: %v", g.ctx.Err()))
	}
}

type lifecycleTransaction struct {
	fdb.WritableTransaction
	ledger    *lifecycleTranscript
	gate      *lifecycleBodyGate
	rangeGate *lifecycleBodyGate
}

func (tx *lifecycleTransaction) Set(key fdb.KeyConvertible, value []byte) {
	k := key.FDBKey()
	tx.ledger.forward("Set", k, value, nil, func() { tx.WritableTransaction.Set(key, value) })
	if tx.gate != nil {
		tx.gate.afterSet(k)
	}
}

func (tx *lifecycleTransaction) SetBytes(key, value []byte) {
	tx.ledger.forward("SetBytes", key, value, nil, func() { tx.WritableTransaction.SetBytes(key, value) })
	if tx.gate != nil {
		tx.gate.afterSet(key)
	}
}

func (tx *lifecycleTransaction) Clear(key fdb.KeyConvertible) {
	tx.ledger.forward("Clear", key.FDBKey(), nil, nil, func() { tx.WritableTransaction.Clear(key) })
}

func (tx *lifecycleTransaction) Add(key fdb.KeyConvertible, value []byte) {
	tx.ledger.forward("Add", key.FDBKey(), value, nil, func() { tx.WritableTransaction.Add(key, value) })
}

func (tx *lifecycleTransaction) ClearBytes(key []byte) {
	tx.ledger.forward("ClearBytes", key, nil, nil, func() { tx.WritableTransaction.ClearBytes(key) })
}

func (tx *lifecycleTransaction) ClearRange(r fdb.ExactRange) {
	begin, end := r.FDBRangeKeys()
	tx.ledger.forward("ClearRange", begin.FDBKey(), nil, end.FDBKey(), func() { tx.WritableTransaction.ClearRange(r) })
	if tx.rangeGate != nil {
		tx.rangeGate.afterSet(begin.FDBKey())
	}
}

func (tx *lifecycleTransaction) AddWriteConflictRange(r fdb.ExactRange) error {
	begin, end := r.FDBRangeKeys()
	tx.ledger.mu.Lock()
	defer tx.ledger.mu.Unlock()
	err := tx.WritableTransaction.AddWriteConflictRange(r)
	tx.ledger.events = append(tx.ledger.events, lifecycleMutation{op: "AddWriteConflictRange", key: bytes.Clone(begin.FDBKey()), end: bytes.Clone(end.FDBKey()), err: err})
	return err
}

func (tx *lifecycleTransaction) SetVersionstampedValue(key fdb.KeyConvertible, value []byte) {
	tx.ledger.forward("SetVersionstampedValue", key.FDBKey(), value, nil, func() { tx.WritableTransaction.SetVersionstampedValue(key, value) })
}

func (tx *lifecycleTransaction) SetVersionstampedKey(key fdb.KeyConvertible, value []byte) {
	tx.ledger.forward("SetVersionstampedKey", key.FDBKey(), value, nil, func() { tx.WritableTransaction.SetVersionstampedKey(key, value) })
}

type lifecycleOperation struct {
	called bool
	done   chan struct{}
	record *FDBStoredRecord[proto.Message]
	found  bool
	err    error
}

type lifecycleAttempt struct {
	ledger                              *lifecycleTranscript
	save, deletion, unrelated           lifecycleOperation
	gateState, deletedState, savedState lifecycleVersionState
	twoStores                           bool
	bodyKey, versionKey                 []byte
	openErr, waitErr, callbackErr       error
	gated                               bool
}

type lifecycleFixture struct {
	md         *RecordMetaData
	root       subspace.Subspace
	format     int32
	serializer *TransformedRecordSerializer
}

func (f lifecycleFixture) open(rc *FDBRecordContext, create bool) (*FDBRecordStore, error) {
	if err := rc.Transaction().Options().SetTimeout((15 * time.Second).Milliseconds()); err != nil {
		return nil, err
	}
	format := f.format
	if format == 0 {
		format = 14
	}
	builder := NewStoreBuilder().SetContext(rc).SetSubspace(f.root).SetMetaDataProvider(f.md).SetFormatVersion(format)
	if f.serializer != nil {
		builder.SetSerializer(f.serializer)
	}
	if create {
		return builder.Create()
	}
	return builder.Open()
}

func lifecycleOrder(id int64, quantity int32) *demopb.Order {
	return &demopb.Order{OrderId: proto.Int64(id), Quantity: proto.Int32(quantity), Price: proto.Int32(7)}
}

func lifecycleAwait(ctx context.Context, done <-chan struct{}, what string) error {
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for %s: %w", what, ctx.Err())
	}
}

func lifecycleRun(ctx context.Context, fixture lifecycleFixture, schedule string) ([]*lifecycleAttempt, error) {
	var attempts []*lifecycleAttempt
	_, runErr := sharedDB.Run(ctx, func(rc *FDBRecordContext) (_ any, callbackErr error) {
		a := &lifecycleAttempt{ledger: &lifecycleTranscript{}, gated: schedule == "gated two-store save/delete"}
		attempts = append(attempts, a)
		defer func() {
			a.callbackErr = callbackErr
			a.ledger.note(fmt.Sprintf("Run callback returns: %v", callbackErr))
		}()
		storeA, err := fixture.open(rc, false)
		if err != nil {
			a.openErr = err
			return nil, err
		}
		storeB, err := fixture.open(rc, false)
		if err != nil {
			a.openErr = err
			return nil, err
		}
		a.twoStores = storeA != storeB && storeA.context == rc && storeB.context == rc
		a.bodyKey = storeA.recordsSubspace.Pack(tuple.Tuple{int64(0), unsplitRecord})
		a.versionKey = storeA.versionKey(tuple.Tuple{int64(0)})
		tx := &lifecycleTransaction{WritableTransaction: rc.tx, ledger: a.ledger}
		// Both stores have opened before replacement. Keep the wrapper installed
		// through Run's real precommit, so its version flush is also recorded.
		rc.tx = tx
		save := func(op *lifecycleOperation, store *FDBRecordStore, id int64) {
			op.record, op.err = store.SaveRecord(lifecycleOrder(id, 2))
			a.ledger.note(fmt.Sprintf("SaveRecord(%d) returned version=%v err=%v", id, lifecycleSavedVersion(op.record), op.err))
			close(op.done)
		}
		deleteTarget := func() {
			a.deletion.found, a.deletion.err = storeB.DeleteRecord(tuple.Tuple{int64(0)})
			a.ledger.note(fmt.Sprintf("DeleteRecord(0) returned found=%t err=%v", a.deletion.found, a.deletion.err))
			close(a.deletion.done)
		}
		a.save = lifecycleOperation{called: true, done: make(chan struct{})}
		a.deletion = lifecycleOperation{called: true, done: make(chan struct{})}
		if !a.gated {
			if schedule == "sequential save then delete" {
				save(&a.save, storeA, 0)
				if a.save.err != nil {
					a.deletion.called = false
					return nil, a.save.err
				}
				deleteTarget()
			} else {
				deleteTarget()
				if a.deletion.err != nil {
					a.save.called = false
					return nil, a.deletion.err
				}
				save(&a.save, storeA, 0)
			}
			a.savedState = lifecycleVersionSnapshot(rc, a.versionKey)
			return nil, errors.Join(a.save.err, a.deletion.err)
		}

		gate := &lifecycleBodyGate{ctx: ctx, key: a.bodyKey, entered: make(chan struct{}), release: make(chan struct{}), ledger: a.ledger}
		gate.onEnter = func() { a.gateState = lifecycleVersionSnapshot(rc, a.versionKey) }
		tx.gate = gate
		var workers sync.WaitGroup
		// Every error/timeout path releases the save and joins every operation
		// before the callback can leave its transaction/context behind.
		defer func() {
			gate.unblock()
			workers.Wait()
			a.ledger.note(fmt.Sprintf("joined operations: save=%v delete=%v unrelated=%v", a.save.err, a.deletion.err, a.unrelated.err))
		}()
		workers.Go(func() { save(&a.save, storeA, 0) })
		select {
		case <-gate.entered:
		case <-a.save.done:
			a.deletion.called = false
			return nil, fmt.Errorf("save ended without reaching the body gate: record=%v err=%v", a.save.record, a.save.err)
		case <-ctx.Done():
			a.deletion.called = false
			a.waitErr = ctx.Err()
			return nil, a.waitErr
		}
		a.unrelated = lifecycleOperation{called: true, done: make(chan struct{})}
		workers.Go(func() { save(&a.unrelated, storeB, 1) })
		if err := lifecycleAwait(ctx, a.unrelated.done, "unrelated PK progress with target save suspended"); err != nil {
			a.deletion.called = false
			a.waitErr = err
			return nil, err
		}
		if a.unrelated.err != nil {
			a.deletion.called = false
			return nil, a.unrelated.err
		}
		contending := make(chan struct{})
		var contentionOnce sync.Once
		prefix := storeA.recordsSubspace.Pack(tuple.Tuple{int64(0)})
		rc.recordPhysicalHook = func(stage string, key []byte) {
			if stage == "before-lock" && bytes.Equal(key, prefix) {
				contentionOnce.Do(func() {
					a.ledger.note("delete reached before-lock while target physical group is held")
					close(contending)
				})
			}
		}
		workers.Go(deleteTarget)
		if err := lifecycleAwait(ctx, contending, "delete physical-group contention"); err != nil {
			a.waitErr = err
			return nil, err
		}
		gate.unblock()
		if err := lifecycleAwait(ctx, a.save.done, "released target save"); err != nil {
			a.waitErr = err
			return nil, err
		}
		a.savedState = lifecycleVersionSnapshot(rc, a.versionKey)
		if err := lifecycleAwait(ctx, a.deletion.done, "delete after target physical group release"); err != nil {
			a.waitErr = err
			return nil, err
		}
		a.deletedState = lifecycleVersionSnapshot(rc, a.versionKey)
		return nil, errors.Join(a.save.err, a.deletion.err, a.unrelated.err)
	})
	return attempts, runErr
}

func lifecycleSavedVersion(record *FDBStoredRecord[proto.Message]) *FDBRecordVersion {
	if record == nil {
		return nil
	}
	return record.Version
}

type lifecycleCensus struct {
	rows                                                                    []fdb.KeyValue
	body, versionBytes                                                      []byte
	version                                                                 *FDBRecordVersion
	freshState                                                              lifecycleVersionState
	loaded, nextSaved                                                       *FDBStoredRecord[proto.Message]
	openErr, rangeErr, bodyErr, versionErr, decodeErr, loadErr, nextSaveErr error
	probeSave                                                               bool
}

// No competing operations run here. The raw census precedes both public calls;
// retain both public errors even if the first one diagnoses an orphan version.
func lifecycleInspect(ctx context.Context, fixture lifecycleFixture, probeSave bool) ([]*lifecycleCensus, error) {
	return lifecycleInspectAt(ctx, fixture, tuple.Tuple{int64(0)}, probeSave)
}

func lifecycleInspectAt(ctx context.Context, fixture lifecycleFixture, pk tuple.Tuple, probeSave bool) ([]*lifecycleCensus, error) {
	var attempts []*lifecycleCensus
	_, runErr := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
		c := &lifecycleCensus{probeSave: probeSave}
		attempts = append(attempts, c)
		store, err := fixture.open(rc, false)
		if err != nil {
			c.openErr = err
			return nil, err
		}
		bodyTuple := pk
		if store.metaData.IsSplitLongRecords() || !store.omitUnsplitRecordSuffix() {
			bodyTuple = appendToTuple(pk, unsplitRecord)
		}
		bodyKey := store.recordsSubspace.Pack(bodyTuple)
		versionKey := store.versionKey(pk)
		c.freshState = lifecycleVersionSnapshot(rc, versionKey)
		prefix := store.recordsSubspace.Pack(pk)
		c.rows, c.rangeErr = rc.Transaction().GetRange(fdb.KeyRange{Begin: prefix, End: fdb.Key(append(bytes.Clone(prefix), 0xff))}, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
		c.body, c.bodyErr = rc.Transaction().Get(bodyKey).Get()
		c.versionBytes, c.versionErr = rc.Transaction().Get(versionKey).Get()
		if c.versionBytes != nil {
			if store.useOldVersionFormat() {
				c.version, c.decodeErr = completeVersionFromBytesUnchecked(c.versionBytes)
			} else {
				c.version, c.decodeErr = unpackVersion(c.versionBytes)
			}
		}
		c.loaded, c.loadErr = store.LoadRecord(pk)
		if probeSave {
			c.nextSaved, c.nextSaveErr = store.SaveRecord(lifecycleOrder(0, 99))
		}
		return nil, errors.Join(c.rangeErr, c.bodyErr, c.versionErr, c.decodeErr, c.loadErr, c.nextSaveErr)
	})
	return attempts, runErr
}

func lifecyclePrintCensus(label string, attempts []*lifecycleCensus, runErr error) {
	fmt.Fprintf(GinkgoWriter, "%s Run error=%v attempts=%d\n", label, runErr, len(attempts))
	for i, c := range attempts {
		fmt.Fprintf(GinkgoWriter, "%s attempt=%d rows=%d body=%x versionBytes=%x version=%v freshLocal=%+v openErr=%v rangeErr=%v bodyErr=%v versionErr=%v decodeErr=%v LoadRecord=%v loadErr=%v nextSaveCalled=%t nextSaveVersion=%v nextSaveErr=%v\n",
			label, i, len(c.rows), c.body, c.versionBytes, c.version, c.freshState, c.openErr, c.rangeErr, c.bodyErr, c.versionErr, c.decodeErr, c.loaded, c.loadErr, c.probeSave, lifecycleSavedVersion(c.nextSaved), c.nextSaveErr)
		var loadOrphan, saveOrphan *FoundSplitWithoutStartError
		fmt.Fprintf(GinkgoWriter, "%s attempt=%d LoadRecordErrorType=%T nextSaveErrorType=%T loadOrphan=%t nextSaveOrphan=%t\n", label, i, c.loadErr, c.nextSaveErr, errors.As(c.loadErr, &loadOrphan), errors.As(c.nextSaveErr, &saveOrphan))
		for _, row := range c.rows {
			fmt.Fprintf(GinkgoWriter, "%s attempt=%d raw key=%x value=%x\n", label, i, row.Key, row.Value)
		}
	}
}

const (
	lifecycleCallbackIndexType = "record_lifecycle_callback_delete_v1"
	lifecycleCallbackSession   = "record-lifecycle-callback-observation"
)

func init() { RegisterIndexMaintainerFactory(lifecycleCallbackFactory{}) }

type lifecycleCallbackFactory struct{}

func (lifecycleCallbackFactory) IndexTypes() []string              { return []string{lifecycleCallbackIndexType} }
func (lifecycleCallbackFactory) IsIdempotent(*Index) bool          { return true }
func (lifecycleCallbackFactory) ValidateIndexOptions(*Index) error { return nil }
func (lifecycleCallbackFactory) ValidateChangedOptions(_, _ *Index, _ map[string]bool) error {
	return nil
}

func (lifecycleCallbackFactory) NewIndexMaintainer(state IndexMaintainerState) (IndexMaintainer, error) {
	return &lifecycleCallbackMaintainer{StandardIndexMaintainer: *NewStandardIndexMaintainer(state), state: state}, nil
}

type lifecycleCallbackMaintainer struct {
	StandardIndexMaintainer
	state IndexMaintainerState
}

type lifecycleCallbackObservation struct {
	ledger                      *lifecycleTranscript
	newCalls, nilCalls          int
	found                       bool
	deleteErr, saveErr, openErr error
	saved                       *FDBStoredRecord[proto.Message]
	versionState                lifecycleVersionState
}

// Preserve the existing Go callback path: the nested delete terminates because
// its Update(old,nil) does not call DeleteRecord again. No recursion flag is used.
func (m *lifecycleCallbackMaintainer) Update(_, newRecord *FDBStoredRecord[proto.Message]) error {
	observation, observed := m.state.Context.Session(lifecycleCallbackSession).(*lifecycleCallbackObservation)
	if newRecord == nil {
		if observed {
			observation.nilCalls++
			observation.ledger.note("callback Update(old,nil) returns nil")
		}
		return nil
	}
	if observed {
		observation.newCalls++
		observation.ledger.note(fmt.Sprintf("callback Update(old,new) calls public DeleteRecord(%v)", newRecord.PrimaryKey))
	}
	found, err := m.state.Store.DeleteRecord(newRecord.PrimaryKey)
	if observed {
		observation.found, observation.deleteErr = found, err
		observation.ledger.note(fmt.Sprintf("callback public DeleteRecord returned found=%t err=%v", found, err))
	}
	return err
}

func (m *lifecycleCallbackMaintainer) UpdateWhileWriteOnly(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	return m.Update(oldRecord, newRecord)
}

func lifecycleCallbackBaseline(root subspace.Subspace) {
	GinkgoHelper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	builder := baseBuilder().SetStoreRecordVersions(true).SetSplitLongRecords(true).SetRecordCountKey(EmptyKey())
	index := NewIndex("lifecycle_callback_delete", Field("order_id"))
	index.Type = lifecycleCallbackIndexType
	builder.AddIndex("Order", index)
	md, err := builder.Build()
	Expect(err).NotTo(HaveOccurred())
	Expect(md.GetRecordCountKey()).NotTo(BeNil())
	fixture := lifecycleFixture{md: md, root: root}
	var observations []*lifecycleCallbackObservation
	_, runErr := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
		o := &lifecycleCallbackObservation{ledger: &lifecycleTranscript{}}
		observations = append(observations, o)
		rc.PutSession(lifecycleCallbackSession, o)
		store, err := fixture.open(rc, true)
		if err != nil {
			o.openErr = err
			return nil, err
		}
		rc.tx = &lifecycleTransaction{WritableTransaction: rc.tx, ledger: o.ledger}
		o.saved, o.saveErr = store.SaveRecord(lifecycleOrder(0, 1))
		o.versionState = lifecycleVersionSnapshot(rc, store.versionKey(tuple.Tuple{int64(0)}))
		o.ledger.note(fmt.Sprintf("outer SaveRecord returned version=%v err=%v", lifecycleSavedVersion(o.saved), o.saveErr))
		return nil, o.saveErr
	})
	fmt.Fprintf(GinkgoWriter, "RECORD_LIFECYCLE_CALLBACK_BASELINE RunErr=%v attempts=%d\n", runErr, len(observations))
	for i, o := range observations {
		o.ledger.print(fmt.Sprintf("callback baseline attempt=%d", i))
		fmt.Fprintf(GinkgoWriter, "callback baseline attempt=%d newCalls=%d nilCalls=%d deleteFound=%t deleteErr=%v saveErr=%v openErr=%v versionState=%+v\n", i, o.newCalls, o.nilCalls, o.found, o.deleteErr, o.saveErr, o.openErr, o.versionState)
	}
	censuses, inspectErr := lifecycleInspect(ctx, fixture, false)
	lifecyclePrintCensus("callback baseline committed census", censuses, inspectErr)
	type countCensus struct {
		count                       int64
		rows                        []fdb.KeyValue
		openErr, countErr, rangeErr error
	}
	var counts []*countCensus
	_, countRunErr := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
		c := &countCensus{}
		counts = append(counts, c)
		store, err := fixture.open(rc, false)
		if err != nil {
			c.openErr = err
			return nil, err
		}
		c.count, c.countErr = store.GetRecordCount()
		c.rows, c.rangeErr = rc.Transaction().GetRange(store.recordsSubspace, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
		return nil, errors.Join(c.countErr, c.rangeErr)
	})
	for i, c := range counts {
		fmt.Fprintf(GinkgoWriter, "callback baseline fresh count attempt=%d count=%d fullRecordSuffixes=%d openErr=%v countErr=%v rangeErr=%v RunErr=%v\n", i, c.count, len(c.rows), c.openErr, c.countErr, c.rangeErr, countRunErr)
	}
	Expect(runErr).NotTo(HaveOccurred())
	Expect(observations).NotTo(BeEmpty())
	o := observations[len(observations)-1]
	Expect(o.openErr).NotTo(HaveOccurred())
	Expect(o.saveErr).NotTo(HaveOccurred())
	Expect(o.deleteErr).NotTo(HaveOccurred())
	Expect(o.saved).NotTo(BeNil())
	Expect(o.newCalls).To(Equal(1))
	Expect(o.nilCalls).To(Equal(1))
	Expect(o.found).To(BeTrue())
	Expect(o.versionState.cached || o.versionState.queued).To(BeFalse())
	var counterValues [][]byte
	countKey := root.Sub(RecordCountKey).Pack(tuple.Tuple{})
	for _, event := range o.ledger.events {
		if event.op == "Add" && bytes.Equal(event.key, countKey) {
			counterValues = append(counterValues, event.value)
		}
	}
	Expect(counterValues).To(Equal([][]byte{{1, 0, 0, 0, 0, 0, 0, 0}, {255, 255, 255, 255, 255, 255, 255, 255}}), "count maintenance must actually increment then decrement")
	Expect(inspectErr).NotTo(HaveOccurred())
	Expect(censuses).NotTo(BeEmpty())
	Expect(censuses[len(censuses)-1].rows).To(BeEmpty())
	Expect(censuses[len(censuses)-1].loaded).To(BeNil())
	Expect(countRunErr).NotTo(HaveOccurred())
	Expect(counts).NotTo(BeEmpty())
	Expect(counts[len(counts)-1].count).To(BeZero())
	Expect(counts[len(counts)-1].rows).To(BeEmpty())
	fmt.Fprintf(GinkgoWriter, "RECORD_LIFECYCLE_CALLBACK_BASELINE completed=true outerSaveReturned=true nestedDeleteReturned=true newCalls=1 nilCalls=1 rawSuffixes=0 count=0\n")
}

func lifecycleMeta(versions bool) *RecordMetaData {
	GinkgoHelper()
	md, err := baseBuilder().SetStoreRecordVersions(versions).SetSplitLongRecords(true).Build()
	Expect(err).NotTo(HaveOccurred())
	return md
}

func lifecycleCreate(ctx context.Context, f lifecycleFixture, record *demopb.Order) {
	GinkgoHelper()
	_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
		store, err := f.open(rc, true)
		if err == nil && record != nil {
			_, err = store.SaveRecord(record)
		}
		return nil, err
	})
	Expect(err).NotTo(HaveOccurred())
}

func lifecycleFinal(ctx context.Context, f lifecycleFixture, want *demopb.Order, suffixes int) {
	GinkgoHelper()
	attempts, err := lifecycleInspect(ctx, f, false)
	lifecyclePrintCensus("physical-group final", attempts, err)
	Expect(err).NotTo(HaveOccurred())
	Expect(attempts).NotTo(BeEmpty())
	c := attempts[len(attempts)-1]
	Expect(c.rows).To(HaveLen(suffixes))
	if want == nil {
		Expect(c.loaded).To(BeNil())
		Expect(c.body).To(BeNil())
		Expect(c.versionBytes).To(BeNil())
	} else {
		Expect(c.loaded).NotTo(BeNil())
		Expect(proto.Equal(c.loaded.Record, want)).To(BeTrue())
		if f.md.IsStoreRecordVersions() {
			Expect(c.loaded.Version).NotTo(BeNil())
			Expect(c.loaded.Version.IsComplete()).To(BeTrue())
		}
	}
	if c.version != nil {
		Expect(c.version.IsComplete()).To(BeTrue())
	}
}

type lifecycleKeyManager struct {
	SerializationKeyManager
	onWrite, onKey func() error
	writes, keys   int
	err            error
}

func (m *lifecycleKeyManager) GetSerializationKey() int32 {
	m.writes++
	if m.onWrite != nil {
		m.err = m.onWrite()
	}
	return m.SerializationKeyManager.GetSerializationKey()
}

func (m *lifecycleKeyManager) GetKey(number int32) ([]byte, error) {
	if m.err != nil {
		return nil, m.err
	}
	if m.onKey != nil {
		m.keys++
		if err := m.onKey(); err != nil {
			return nil, err
		}
	}
	return m.SerializationKeyManager.GetKey(number)
}

func lifecycleCodec(manager SerializationKeyManager) *TransformedRecordSerializer {
	GinkgoHelper()
	codec, err := NewTransformedRecordSerializerJCEBuilder().SetEncryptWhenSerializing(true).SetKeyManager(manager).
		SetWriteValidationRatio(0).SetWriteEncryptionValidationRatio(0).Build()
	Expect(err).NotTo(HaveOccurred())
	return codec
}

func lifecycleBulkOverlap(ctx context.Context, f lifecycleFixture, kind string, pointFirst bool) {
	GinkgoHelper()
	lifecycleCreate(ctx, f, lifecycleOrder(0, 1))
	ledger := &lifecycleTranscript{}
	var pointErr, bulkErr error
	_, runErr := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
		a, err := f.open(rc, false)
		if err != nil {
			return nil, err
		}
		b, err := f.open(rc, false)
		if err != nil {
			return nil, err
		}
		gate := &lifecycleBodyGate{ctx: ctx, entered: make(chan struct{}), release: make(chan struct{}), ledger: ledger}
		tx := &lifecycleTransaction{WritableTransaction: rc.tx, ledger: ledger}
		if pointFirst {
			gate.key = a.recordsSubspace.Pack(tuple.Tuple{int64(0), unsplitRecord})
			tx.gate = gate
		} else {
			tx.rangeGate = gate
		}
		rc.tx = tx
		contending := make(chan struct{})
		var once sync.Once
		var observe atomic.Bool
		prefix := a.recordsSubspace.Pack(tuple.Tuple{int64(0)})
		rc.recordPhysicalHook = func(stage string, key []byte) {
			if !observe.Load() {
				return
			}
			if (pointFirst && stage == "before-bulk" && bytes.Equal(key, a.recordsSubspace.Bytes())) || (!pointFirst && stage == "before-lock" && bytes.Equal(key, prefix)) {
				once.Do(func() { ledger.note("second operation reached physical contention hook"); close(contending) })
			}
		}
		pointDone, bulkDone := make(chan struct{}), make(chan struct{})
		var workers sync.WaitGroup
		defer func() { gate.unblock(); workers.Wait() }()
		point := func() {
			_, pointErr = a.SaveRecord(lifecycleOrder(0, 2))
			ledger.note(fmt.Sprintf("point returned %v", pointErr))
			close(pointDone)
		}
		bulk := func() {
			if kind == "all" {
				bulkErr = b.DeleteAllRecords()
			} else {
				bulkErr = b.DeleteRecordsWhere(tuple.Tuple{int64(0)})
			}
			ledger.note(fmt.Sprintf("bulk %s returned %v", kind, bulkErr))
			close(bulkDone)
		}
		if pointFirst {
			workers.Go(point)
		} else {
			workers.Go(bulk)
		}
		if err := lifecycleAwait(ctx, gate.entered, "first physical group"); err != nil {
			return nil, err
		}
		observe.Store(true)
		if pointFirst {
			workers.Go(bulk)
		} else {
			workers.Go(point)
		}
		if err := lifecycleAwait(ctx, contending, "second-store bulk/point contention"); err != nil {
			return nil, err
		}
		gate.unblock()
		if err := lifecycleAwait(ctx, pointDone, "point after contention"); err != nil {
			return nil, err
		}
		if err := lifecycleAwait(ctx, bulkDone, "bulk after contention"); err != nil {
			return nil, err
		}
		return nil, errors.Join(pointErr, bulkErr)
	})
	ledger.print(fmt.Sprintf("BULK_OVERLAP kind=%s pointFirst=%t", kind, pointFirst))
	Expect(runErr).NotTo(HaveOccurred())
	Expect(pointErr).NotTo(HaveOccurred())
	Expect(bulkErr).NotTo(HaveOccurred())
	if pointFirst {
		lifecycleFinal(ctx, f, nil, 0)
	} else {
		lifecycleFinal(ctx, f, lifecycleOrder(0, 2), 2)
	}
}

type lifecycleBackendFault struct {
	fdb.WritableTransaction
	begin, end []byte
	armed      bool
	err        error
	onIssue    func() error
}

func (tx *lifecycleBackendFault) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	result := tx.WritableTransaction.GetRange(r, options)
	exact, ok := r.(fdb.ExactRange)
	if !ok || !tx.armed {
		return result
	}
	begin, end := exact.FDBRangeKeys()
	if !bytes.Equal(begin.FDBKey(), tx.begin) || !bytes.Equal(end.FDBKey(), tx.end) {
		return result
	}
	tx.armed = false
	if err := tx.onIssue(); err != nil {
		return &lifecycleFaultRange{RangeResult: result, err: err}
	}
	return &lifecycleFaultRange{RangeResult: result, err: tx.err}
}

type lifecycleFaultRange struct {
	fdb.RangeResult
	err error
}

func (r *lifecycleFaultRange) GetSliceWithError() ([]fdb.KeyValue, error) { return nil, r.err }
func (r *lifecycleFaultRange) Iterator() fdb.RangeIterator {
	return &lifecycleFaultIterator{RangeIterator: r.RangeResult.Iterator(), err: r.err}
}

type lifecycleFaultIterator struct {
	fdb.RangeIterator
	err error
}

func (i *lifecycleFaultIterator) Get() (fdb.KeyValue, error) { return fdb.KeyValue{}, i.err }

type lifecycleLegacyFault struct {
	fdb.WritableTransaction
	versionKey []byte
	armed      bool
	sentinel   error
	onIssue    func() error
	future     *lifecycleLegacyFuture
}

func (tx *lifecycleLegacyFault) Get(key fdb.KeyConvertible) fdb.FutureByteSlice {
	future := tx.WritableTransaction.Get(key)
	if !tx.armed || !bytes.Equal(key.FDBKey(), tx.versionKey) {
		return future
	}
	tx.armed = false
	tx.future = &lifecycleLegacyFuture{FutureByteSlice: future, err: tx.sentinel}
	if err := tx.onIssue(); err != nil {
		tx.future.err = err
	}
	return tx.future
}

type lifecycleLegacyFuture struct {
	fdb.FutureByteSlice
	gets atomic.Int64
	err  error
}

func (f *lifecycleLegacyFuture) Get() ([]byte, error) {
	f.gets.Add(1)
	if _, err := f.FutureByteSlice.Get(); err != nil {
		return nil, err
	}
	return nil, f.err
}

func lifecycleLegacyError(ctx context.Context, f lifecycleFixture, corrupt bool) {
	GinkgoHelper()
	base := NewFixedZeroKeyManager(bytes.Repeat([]byte{4}, 16), "", bytes.NewReader(bytes.Repeat([]byte{6}, 4096)))
	f.serializer = lifecycleCodec(base)
	lifecycleCreate(ctx, f, lifecycleOrder(0, 1))
	sentinel := errors.New("displaced original legacy version backend sentinel")
	var outerErr, nestedErr error
	var gets, earlyGets, readCalls int64
	_, runErr := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
		aFixture := f
		manager := &lifecycleKeyManager{SerializationKeyManager: base}
		aFixture.serializer = lifecycleCodec(manager)
		a, err := aFixture.open(rc, false)
		if err != nil {
			return nil, err
		}
		b, err := f.open(rc, false)
		if err != nil {
			return nil, err
		}
		fault := &lifecycleLegacyFault{WritableTransaction: rc.tx, versionKey: a.versionKey(tuple.Tuple{int64(0)}), armed: true, sentinel: sentinel}
		manager.onKey = func() error {
			readCalls++
			earlyGets = fault.future.gets.Load()
			if earlyGets != 0 {
				return errors.New("legacy backend future observed before fresh deserialization")
			}
			return nil
		}
		fault.onIssue = func() error {
			_, nestedErr = b.SaveRecord(lifecycleOrder(0, 2))
			if nestedErr != nil {
				return nestedErr
			}
			if corrupt {
				bad, err := f.serializer.transformRead([]byte{0xff}, rc.Env(), nil)
				if err != nil {
					return err
				}
				body := tuple.Tuple{int64(0)}
				if b.metaData.IsSplitLongRecords() || !b.omitUnsplitRecordSuffix() {
					body = appendToTuple(body, unsplitRecord)
				}
				rc.Transaction().Set(b.recordsSubspace.Pack(body), bad)
			}
			return nil
		}
		rc.tx = fault
		_, outerErr = a.SaveRecord(lifecycleOrder(0, 3))
		if fault.future != nil {
			gets = fault.future.gets.Load()
		}
		fmt.Fprintf(GinkgoWriter, "LEGACY_DISPLACED format=%d corrupt=%t nestedErr=%v outerErr=%v backendGets=%d getsAtDeserialize=%d readCalls=%d sentinelPreserved=%t\n", f.format, corrupt, nestedErr, outerErr, gets, earlyGets, readCalls, errors.Is(outerErr, sentinel))
		if nestedErr != nil {
			return nil, nestedErr
		}
		if corrupt {
			var deserialize *RecordDeserializationError
			if !errors.As(outerErr, &deserialize) || errors.Is(outerErr, sentinel) || gets != 0 {
				return nil, fmt.Errorf("deserialization precedence changed: outer=%v gets=%d", outerErr, gets)
			}
		} else if !errors.Is(outerErr, sentinel) || gets != 1 {
			return nil, fmt.Errorf("original backend failure was displaced: outer=%v gets=%d", outerErr, gets)
		}
		return nil, nil
	})
	Expect(runErr).NotTo(HaveOccurred())
	Expect(nestedErr).NotTo(HaveOccurred())
	Expect(earlyGets).To(BeZero())
	Expect(readCalls).To(Equal(int64(1)))
	if corrupt {
		Expect(gets).To(BeZero())
	} else {
		Expect(gets).To(Equal(int64(1)))
		lifecycleFinal(ctx, f, lifecycleOrder(0, 2), 1)
	}
}

type lifecycleCommitProbe struct {
	rc                                                             *FDBRecordContext
	store                                                          *FDBRecordStore
	ledger                                                         *lifecycleTranscript
	held, release, writerDone, checksDone, sealed                  chan struct{}
	releaseOnce                                                    sync.Once
	holdOnce, sealedOnce                                           atomic.Bool
	admissions, pipelineAdmissions, lateChecks                     atomic.Int64
	writerErr, checksErr, progressErr, latePublicErr, lateBeginErr error
	written                                                        int
}

func (p *lifecycleCommitProbe) unblock() { p.releaseOnce.Do(func() { close(p.release) }) }

func lifecycleCommitActivity(ctx context.Context, f lifecycleFixture, route, api string) {
	GinkgoHelper()
	lifecycleCreate(ctx, f, nil)
	const n = 2*DefaultPipelineSize + 3
	probes := make(chan *lifecycleCommitProbe, 16)
	runDone := make(chan struct{})
	var runErr error
	var handled []*lifecycleCommitProbe
	prepare := func(rc *FDBRecordContext) (*lifecycleCommitProbe, error) {
		a, err := f.open(rc, false)
		if err != nil {
			return nil, err
		}
		b, err := f.open(rc, false)
		if err != nil {
			return nil, err
		}
		p := &lifecycleCommitProbe{rc: rc, store: b, ledger: &lifecycleTranscript{}, held: make(chan struct{}), release: make(chan struct{}), writerDone: make(chan struct{}), checksDone: make(chan struct{}), sealed: make(chan struct{})}
		rc.tx = &lifecycleTransaction{WritableTransaction: rc.tx, ledger: p.ledger}
		prior := a.recordsSubspace.Pack(tuple.Tuple{int64(2*DefaultPipelineSize - 1), unsplitRecord})
		target := a.recordsSubspace.Pack(tuple.Tuple{int64(2 * DefaultPipelineSize)})
		rc.recordPhysicalHook = func(stage string, key []byte) {
			if stage == "admitted" {
				p.admissions.Add(1)
			}
			if stage == "before-lock" && bytes.Equal(key, target) && (p.ledger.first("SetBytes", prior) >= 0 || p.ledger.first("Set", prior) >= 0) && p.holdOnce.CompareAndSwap(false, true) {
				p.pipelineAdmissions.Store(p.admissions.Load())
				p.ledger.note("later-window record reached hook after prior-window body writes")
				close(p.held)
				select {
				case <-p.release:
				case <-ctx.Done():
				}
				// This registration occurs after the initial checks completed,
				// while the joined public writer is still admitted.
				rc.AddCommitCheck(func() error {
					p.lateChecks.Add(1)
					_, err := b.SaveRecord(lifecycleOrder(103, 5))
					return err
				})
			}
			if stage == "sealed" && p.sealedOnce.CompareAndSwap(false, true) {
				_, p.latePublicErr = b.SaveRecord(lifecycleOrder(999, 9))
				finish, err := rc.beginRecordWrite()
				p.lateBeginErr = err
				if finish != nil {
					finish()
				}
				p.ledger.note(fmt.Sprintf("sealed hook publicErr=%v beginErr=%v", p.latePublicErr, p.lateBeginErr))
				close(p.sealed)
			}
		}
		rc.AddCommitCheck(func() error {
			errs := make(chan error, 2)
			var joined sync.WaitGroup
			for _, id := range []int64{100, 101} {
				joined.Go(func() { _, err := b.SaveRecord(lifecycleOrder(id, 5)); errs <- err })
			}
			joined.Wait()
			close(errs)
			for err := range errs {
				p.checksErr = errors.Join(p.checksErr, err)
			}
			p.ledger.note(fmt.Sprintf("joined mutating commit checks returned %v", p.checksErr))
			close(p.checksDone)
			return p.checksErr
		})
		probes <- p
		go func() {
			defer close(p.writerDone)
			records := make([]proto.Message, n)
			for i := range records {
				records[i] = lifecycleOrder(int64(i), int32(i+1))
			}
			var saved []*FDBStoredRecord[proto.Message]
			if api == "pipeline" {
				saved, p.writerErr = a.SaveRecordsPipelined(records, RecordExistenceCheckNone)
			} else {
				saved, p.writerErr = a.SaveRecordBatch(records)
			}
			p.written = len(saved)
			p.ledger.note(fmt.Sprintf("full %s operation returned rows=%d err=%v", api, p.written, p.writerErr))
		}()
		return p, nil
	}
	go func() {
		defer close(runDone)
		var all []*lifecycleCommitProbe
		defer func() {
			for _, p := range all {
				p.unblock()
				<-p.writerDone
			}
		}()
		body := func(rc *FDBRecordContext) (any, error) {
			p, err := prepare(rc)
			if err != nil {
				return nil, err
			}
			all = append(all, p)
			return nil, lifecycleAwait(ctx, p.held, "later public-operation window before commit")
		}
		switch route {
		case "Run":
			_, runErr = sharedDB.Run(ctx, body)
		case "runner":
			_, runErr = NewFDBDatabaseRunner(sharedDB).RunWithRetry(ctx, body)
		case "direct":
			rc, err := NewFDBDatabaseRunner(sharedDB).OpenContext(ctx)
			if err != nil {
				runErr = err
				return
			}
			defer rc.Cancel()
			_, runErr = body(rc)
			if runErr == nil {
				runErr = rc.Commit()
			}
		}
	}()
	defer func() {
		for _, p := range handled {
			p.unblock()
		}
		<-runDone
	}()
	finished := false
	for !finished {
		select {
		case p := <-probes:
			handled = append(handled, p)
			waitErr := lifecycleAwait(ctx, p.held, "held later window")
			if waitErr == nil {
				waitErr = lifecycleAwait(ctx, p.checksDone, "joined mutating checks while writer remains admitted")
			}
			if waitErr == nil {
				_, p.progressErr = p.store.SaveRecord(lifecycleOrder(102, 6))
			}
			p.unblock()
			if waitErr == nil {
				waitErr = lifecycleAwait(ctx, p.writerDone, "entire public write operation")
			}
			if waitErr == nil {
				waitErr = lifecycleAwait(ctx, p.sealed, "atomic quiescence seal")
			}
			p.ledger.note(fmt.Sprintf("control waits=%v progressErr=%v", waitErr, p.progressErr))
			Expect(waitErr).NotTo(HaveOccurred())
		case <-runDone:
			finished = true
		case <-ctx.Done():
			for _, p := range handled {
				p.unblock()
			}
			<-runDone
			Fail(fmt.Sprintf("commit activity control timed out route=%s api=%s: %v", route, api, ctx.Err()))
		}
	}
	fmt.Fprintf(GinkgoWriter, "WRITE_QUIESCENCE route=%s api=%s RunErr=%v attempts=%d\n", route, api, runErr, len(handled))
	for i, p := range handled {
		p.ledger.print(fmt.Sprintf("quiescence %s/%s attempt=%d", route, api, i))
	}
	Expect(runErr).NotTo(HaveOccurred())
	Expect(handled).NotTo(BeEmpty())
	p := handled[len(handled)-1]
	fmt.Fprintf(GinkgoWriter, "WRITE_QUIESCENCE_ADMISSION route=%s api=%s atHeldWindow=%d totalAdmissions=%d written=%d checksErr=%v progressErr=%v\n", route, api, p.pipelineAdmissions.Load(), p.admissions.Load(), p.written, p.checksErr, p.progressErr)
	Expect(p.pipelineAdmissions.Load()).To(BeNumerically(">=", 1), "the held later window must belong to admitted public work")
	Expect(p.admissions.Load()).To(BeNumerically(">=", 4), "public operation, joined check writes and pre-seal progress must all have been admitted")
	Expect(p.writerErr).NotTo(HaveOccurred())
	Expect(p.checksErr).NotTo(HaveOccurred())
	Expect(p.progressErr).NotTo(HaveOccurred())
	Expect(p.written).To(Equal(n))
	Expect(p.lateChecks.Load()).To(Equal(int64(1)), "late registration must drain exactly once before seal")
	var inactivePublic, inactiveBegin *RecordContextNotActiveError
	Expect(errors.As(p.latePublicErr, &inactivePublic)).To(BeTrue())
	Expect(errors.As(p.lateBeginErr, &inactiveBegin)).To(BeTrue())
	_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
		store, err := f.open(rc, false)
		if err != nil {
			return nil, err
		}
		rows, err := rc.Transaction().GetRange(store.recordsSubspace, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
		if err != nil {
			return nil, err
		}
		if len(rows) != 2*(n+4) {
			return nil, fmt.Errorf("durable suffixes=%d want %d", len(rows), 2*(n+4))
		}
		for i := 0; i < n+4; i++ {
			id, quantity := int64(i), int32(i+1)
			if i >= n {
				id, quantity = int64(100+i-n), 5
				if id == 102 {
					quantity = 6
				}
			}
			got, err := store.LoadRecord(tuple.Tuple{id})
			if err != nil {
				return nil, err
			}
			if got == nil || !proto.Equal(got.Record, lifecycleOrder(id, quantity)) || got.Version == nil || !got.Version.IsComplete() {
				return nil, fmt.Errorf("incomplete durable record id=%d record=%v", id, got)
			}
		}
		fmt.Fprintf(GinkgoWriter, "WRITE_QUIESCENCE_DURABLE route=%s api=%s records=%d suffixes=%d allVersionsComplete=true latePublicType=%T lateBeginType=%T\n", route, api, n+4, len(rows), p.latePublicErr, p.lateBeginErr)
		return nil, nil
	})
	Expect(err).NotTo(HaveOccurred())
}

var _ = Describe("Callback-free physical record groups", func() {
	for _, format := range []int32{4, 5} {
		for _, corrupt := range []bool{false, true} {
			It(fmt.Sprintf("retains displaced legacy backend future after fresh deserialize format=%d corrupt=%t", format, corrupt), func() {
				ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
				defer cancel()
				builder := baseBuilder().SetStoreRecordVersions(true).SetSplitLongRecords(format != 4)
				md, err := builder.Build()
				Expect(err).NotTo(HaveOccurred())
				lifecycleLegacyError(ctx, lifecycleFixture{md: md, root: specSubspace(), format: format}, corrupt)
			})
		}
	}
	for _, route := range []string{"Run", "runner", "direct"} {
		for _, api := range []string{"pipeline", "batch"} {
			It(fmt.Sprintf("joins complete %s activity and mutating checks before %s seals", api, route), func() {
				ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
				defer cancel()
				lifecycleCommitActivity(ctx, lifecycleFixture{md: lifecycleMeta(true), root: specSubspace()}, route, api)
			})
		}
	}
	for _, kind := range []string{"all", "where"} {
		for _, pointFirst := range []bool{true, false} {
			It(fmt.Sprintf("coordinates second-store bulk %s pointFirst=%t", kind, pointFirst), func() {
				ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
				defer cancel()
				lifecycleBulkOverlap(ctx, lifecycleFixture{md: lifecycleMeta(true), root: specSubspace()}, kind, pointFirst)
			})
		}
	}

	It("preserves a backend iterator error even after the issued image generation changed", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		f := lifecycleFixture{md: lifecycleMeta(true), root: specSubspace()}
		lifecycleCreate(ctx, f, lifecycleOrder(0, 1))
		sentinel := errors.New("record lifecycle backend iterator sentinel")
		var saveErr, nestedErr error
		var nestedCalls int
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			a, err := f.open(rc, false)
			if err != nil {
				return nil, err
			}
			b, err := f.open(rc, false)
			if err != nil {
				return nil, err
			}
			begin, end := a.recordsSubspace.Sub(int64(0)).FDBRangeKeys()
			fault := &lifecycleBackendFault{WritableTransaction: rc.tx, begin: begin.FDBKey(), end: end.FDBKey(), armed: true, err: sentinel}
			fault.onIssue = func() error { nestedCalls++; _, nestedErr = b.SaveRecord(lifecycleOrder(0, 2)); return nestedErr }
			rc.tx = fault
			_, saveErr = a.SaveRecord(lifecycleOrder(0, 3))
			fmt.Fprintf(GinkgoWriter, "BACKEND_ORIGIN nestedCalls=%d nestedErr=%v saveErr=%v preserved=%t\n", nestedCalls, nestedErr, saveErr, errors.Is(saveErr, sentinel))
			if nestedErr != nil {
				return nil, nestedErr
			}
			if !errors.Is(saveErr, sentinel) {
				return nil, fmt.Errorf("backend error was replaced after generation change: %v", saveErr)
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(nestedErr).NotTo(HaveOccurred())
		Expect(nestedCalls).To(Equal(1))
		Expect(errors.Is(saveErr, sentinel)).To(BeTrue())
		lifecycleFinal(ctx, f, lifecycleOrder(0, 2), 2)
	})
	for _, disableSplit := range []bool{false, true} {
		It(fmt.Sprintf("refreshes unsplit to multichunk to unsplit sizing without replaying serialization disableSplit=%t", disableSplit), func() {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			f := lifecycleFixture{md: lifecycleMeta(true), root: specSubspace()}
			lifecycleCreate(ctx, f, lifecycleOrder(0, 1))
			base := NewFixedZeroKeyManager(bytes.Repeat([]byte{7}, 16), "", bytes.NewReader(bytes.Repeat([]byte{3}, 4096)))
			var calls, innerKeys int
			var innerErr error
			want := lifecycleOrder(0, 3)
			_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				plain, err := f.open(rc, false)
				if err != nil {
					return nil, err
				}
				manager := &lifecycleKeyManager{SerializationKeyManager: base}
				manager.onWrite = func() error {
					calls++
					large := lifecycleOrder(0, 2)
					large.VectorData = bytes.Repeat([]byte{9}, 2*splitRecordSize+50)
					stored, saveErr := plain.SaveRecord(large)
					innerErr = saveErr
					if stored != nil {
						innerKeys = stored.KeyCount
					}
					return saveErr
				}
				outerFixture := f
				outerFixture.serializer = lifecycleCodec(manager)
				if disableSplit {
					copyMD := *f.md
					copyMD.splitLongRecords = false
					outerFixture.md = &copyMD
				}
				outer, err := outerFixture.open(rc, false)
				if err != nil {
					return nil, err
				}
				_, err = outer.SaveRecord(want)
				fmt.Fprintf(GinkgoWriter, "PHYSICAL_REFRESH serializationCalls=%d innerKeys=%d innerErr=%v outerErr=%v\n", calls, innerKeys, innerErr, err)
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(innerErr).NotTo(HaveOccurred())
			Expect(calls).To(Equal(1))
			Expect(innerKeys).To(BeNumerically(">=", 4))
			f.serializer = lifecycleCodec(base)
			lifecycleFinal(ctx, f, want, 2)
		})
	}

	for _, format := range []int32{5, 14} {
		for _, automatic := range []bool{false, true} {
			It(fmt.Sprintf("removes current deferred versions after a read hook under mixed metadata format=%d automaticWriter=%t", format, automatic), func() {
				ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
				defer cancel()
				base := NewFixedZeroKeyManager(bytes.Repeat([]byte{5}, 16), "", bytes.NewReader(bytes.Repeat([]byte{2}, 4096)))
				f := lifecycleFixture{md: lifecycleMeta(false), root: specSubspace(), format: format, serializer: lifecycleCodec(base)}
				lifecycleCreate(ctx, f, lifecycleOrder(0, 1))
				var hookCalls int
				var nestedErr error
				var nestedVersion *FDBRecordVersion
				_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
					writerFixture := f
					// Keep descriptors, numbering and version identical; only this handle's automatic-version policy differs.
					if automatic {
						copyMD := *f.md
						copyMD.storeRecordVersions = true
						writerFixture.md = &copyMD
					}
					writer, err := writerFixture.open(rc, false)
					if err != nil {
						return nil, err
					}
					manager := &lifecycleKeyManager{SerializationKeyManager: base}
					manager.onKey = func() error {
						hookCalls++
						var saved *FDBStoredRecord[proto.Message]
						if automatic {
							saved, nestedErr = writer.SaveRecord(lifecycleOrder(0, 2))
						} else {
							version, versionErr := IncompleteVersion(rc.ClaimLocalVersion())
							if versionErr != nil {
								return versionErr
							}
							saved, nestedErr = writer.SaveRecordWithVersion(lifecycleOrder(0, 2), RecordExistenceCheckNone, version, VersionstampSaveBehaviorIfPresent)
						}
						nestedVersion = lifecycleSavedVersion(saved)
						return nestedErr
					}
					deletingFixture := f
					deletingFixture.serializer = lifecycleCodec(manager)
					deleter, err := deletingFixture.open(rc, false)
					if err != nil {
						return nil, err
					}
					found, err := deleter.DeleteRecord(tuple.Tuple{int64(0)})
					state := lifecycleVersionSnapshot(rc, deleter.versionKey(tuple.Tuple{int64(0)}))
					fmt.Fprintf(GinkgoWriter, "MIXED_VERSION format=%d automaticWriter=%t hookCalls=%d nestedVersion=%v nestedErr=%v deleted=%t deleteErr=%v state=%+v\n", format, automatic, hookCalls, nestedVersion, nestedErr, found, err, state)
					if err == nil && (!found || state.cached || state.queued) {
						return nil, errors.New("mixed metadata delete retained current deferred version")
					}
					return nil, err
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(nestedErr).NotTo(HaveOccurred())
				Expect(hookCalls).To(Equal(1))
				Expect(nestedVersion).NotTo(BeNil())
				Expect(nestedVersion.IsComplete()).To(BeFalse())
				lifecycleFinal(ctx, f, nil, 0)
			})
		}
	}

	for _, deletingVersions := range []bool{false, true} {
		for _, deleteKind := range []string{"point", "where"} {
			It(fmt.Sprintf("does not resurrect pending legacy versions while preserving ignored bytes versions=%t delete=%s", deletingVersions, deleteKind), func() {
				ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
				defer cancel()
				builder := baseBuilder().SetStoreRecordVersions(true).SetSplitLongRecords(true)
				builder.GetRecordType("Order").SetPrimaryKey(Concat(Field("order_id"), Field("price")))
				md, err := builder.Build()
				Expect(err).NotTo(HaveOccurred())
				pk, prefix := tuple.Tuple{int64(0), int64(7)}, tuple.Tuple{int64(0)}
				f := lifecycleFixture{md: md, root: specSubspace(), format: 5}
				lifecycleCreate(ctx, f, lifecycleOrder(0, 1))
				seed, seedErr := lifecycleInspectAt(ctx, f, pk, false)
				lifecyclePrintCensus("legacy committed version seed", seed, seedErr)
				Expect(seedErr).NotTo(HaveOccurred())
				Expect(seed).NotTo(BeEmpty())
				oldVersion := bytes.Clone(seed[len(seed)-1].versionBytes)
				Expect(oldVersion).To(HaveLen(VersionBytes))
				deleteFixture := f
				if !deletingVersions {
					copyMD := *f.md
					copyMD.storeRecordVersions = false
					deleteFixture.md = &copyMD
				}
				_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
					writer, err := f.open(rc, false)
					if err != nil {
						return nil, err
					}
					deleter, err := deleteFixture.open(rc, false)
					if err != nil {
						return nil, err
					}
					_, err = writer.SaveRecord(lifecycleOrder(0, 2))
					if err != nil {
						return nil, err
					}
					versionKey := writer.versionKey(pk)
					before := lifecycleVersionSnapshot(rc, versionKey)
					var found bool
					if deleteKind == "point" {
						found, err = deleter.DeleteRecord(pk)
					} else {
						// A legacy version has no suffix: a full-PK range excludes
						// its exact key. Exercise a genuine partial-PK range instead.
						recordBegin, recordEnd := deleter.recordsSubspace.Sub(prefix...).FDBRangeKeys()
						versionSpace := deleter.subspace.Sub(RecordVersionKey)
						versionBegin, versionEnd := versionSpace.Sub(prefix...).FDBRangeKeys()
						bodyKey := deleter.recordsSubspace.Pack(appendToTuple(pk, unsplitRecord))
						fullBegin, _ := versionSpace.Sub(pk...).FDBRangeKeys()
						if bytes.Compare(bodyKey, recordBegin.FDBKey()) < 0 || bytes.Compare(bodyKey, recordEnd.FDBKey()) >= 0 ||
							bytes.Compare(versionKey, versionBegin.FDBKey()) < 0 || bytes.Compare(versionKey, versionEnd.FDBKey()) >= 0 ||
							bytes.Compare(versionKey, fullBegin.FDBKey()) >= 0 {
							return nil, errors.New("legacy partial-prefix fixture does not cover the target body and version exclusively of the full-PK lower bound")
						}
						fmt.Fprintf(GinkgoWriter, "LEGACY_PARTIAL_PREFIX pk=%v prefix=%v body=%x recordRange=[%x,%x) version=%x versionRange=[%x,%x) fullPKBegin=%x\n", pk, prefix, bodyKey, recordBegin.FDBKey(), recordEnd.FDBKey(), versionKey, versionBegin.FDBKey(), versionEnd.FDBKey(), fullBegin.FDBKey())
						err = deleter.DeleteRecordsWhere(prefix)
					}
					after := lifecycleVersionSnapshot(rc, versionKey)
					fmt.Fprintf(GinkgoWriter, "LEGACY_PENDING_POLICY deletingVersions=%t deleteKind=%s before=%+v after=%+v pointFound=%t deleteErr=%v\n", deletingVersions, deleteKind, before, after, found, err)
					if err == nil && ((deleteKind == "point" && !found) || !before.cached || !before.queued || after.cached || after.queued) {
						return nil, errors.New("legacy target pending bookkeeping was not removed")
					}
					return nil, err
				})
				Expect(err).NotTo(HaveOccurred())
				final, finalErr := lifecycleInspectAt(ctx, deleteFixture, pk, false)
				lifecyclePrintCensus("legacy committed-byte policy", final, finalErr)
				Expect(finalErr).NotTo(HaveOccurred())
				Expect(final).NotTo(BeEmpty())
				last := final[len(final)-1]
				Expect(last.rows).To(BeEmpty())
				Expect(last.body).To(BeNil())
				Expect(last.loaded).To(BeNil())
				Expect(last.freshState.cached || last.freshState.queued).To(BeFalse())
				if deletingVersions {
					Expect(last.versionBytes).To(BeNil())
				} else {
					Expect(last.versionBytes).To(Equal(oldVersion))
				}
			})
		}
	}

	It("preserves unchanged-generation persisted corruption instead of repairing or overwriting it", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		f := lifecycleFixture{md: lifecycleMeta(true), root: specSubspace()}
		lifecycleCreate(ctx, f, nil)
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := f.open(rc, false)
			if err != nil {
				return nil, err
			}
			version, err := packVersion(MinVersion())
			if err != nil {
				return nil, err
			}
			rc.Transaction().Set(store.versionKey(tuple.Tuple{int64(0)}), version)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		attempts, inspectErr := lifecycleInspect(ctx, f, true)
		lifecyclePrintCensus("stable persisted corruption", attempts, inspectErr)
		Expect(inspectErr).To(HaveOccurred())
		Expect(attempts).NotTo(BeEmpty())
		last := attempts[len(attempts)-1]
		var loadErr, saveErr *FoundSplitWithoutStartError
		Expect(errors.As(last.loadErr, &loadErr)).To(BeTrue())
		Expect(errors.As(last.nextSaveErr, &saveErr)).To(BeTrue())
		Expect(last.rows).To(HaveLen(1))
		Expect(last.body).To(BeNil())
		Expect(last.version).NotTo(BeNil())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := f.open(rc, false)
			if err != nil {
				return nil, err
			}
			_, deleteErr := store.DeleteRecord(tuple.Tuple{int64(0)})
			var structural *FoundSplitWithoutStartError
			fmt.Fprintf(GinkgoWriter, "STABLE_CORRUPTION deleteErr=%v\n", deleteErr)
			if !errors.As(deleteErr, &structural) {
				return nil, fmt.Errorf("delete lost stable corruption: %v", deleteErr)
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		after, afterErr := lifecycleInspect(ctx, f, false)
		lifecyclePrintCensus("stable corruption after refused delete", after, afterErr)
		Expect(afterErr).To(HaveOccurred())
		Expect(after).NotTo(BeEmpty())
		Expect(after[len(after)-1].rows).To(HaveLen(1))
		Expect(after[len(after)-1].body).To(BeNil())
		Expect(after[len(after)-1].versionBytes).To(Equal(last.versionBytes))
	})
})

var _ = Describe("Committed same-context record lifecycle", func() {
	It("keeps body and inline version consistent after sequential controls and gated two-store save/delete", func() {
		lifecycleCallbackBaseline(specSubspace().Sub("callback baseline"))
		for _, schedule := range []string{"sequential save then delete", "sequential delete then save", "gated two-store save/delete"} {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			md, err := baseBuilder().SetStoreRecordVersions(true).SetSplitLongRecords(true).Build()
			Expect(err).NotTo(HaveOccurred())
			fixture := lifecycleFixture{md: md, root: specSubspace().Sub(schedule)}
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := fixture.open(rc, true)
				if err != nil {
					return nil, err
				}
				saved, err := store.SaveRecord(lifecycleOrder(0, 1))
				fmt.Fprintf(GinkgoWriter, "seed SaveRecord version=%v err=%v\n", lifecycleSavedVersion(saved), err)
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			seedAttempts, seedErr := lifecycleInspect(ctx, fixture, false)
			lifecyclePrintCensus("healthy committed seed", seedAttempts, seedErr)
			Expect(seedErr).NotTo(HaveOccurred())
			Expect(seedAttempts).NotTo(BeEmpty())
			seed := seedAttempts[len(seedAttempts)-1]
			Expect(seed.rows).To(HaveLen(2))
			Expect(seed.body).NotTo(BeEmpty())
			Expect(seed.version).NotTo(BeNil())
			Expect(seed.version.IsComplete()).To(BeTrue())
			Expect(seed.freshState.cached).To(BeFalse())
			Expect(seed.loaded).NotTo(BeNil())
			Expect(seed.loaded.Record.(*demopb.Order).GetQuantity()).To(Equal(int32(1)))

			attempts, runErr := lifecycleRun(ctx, fixture, schedule)
			fmt.Fprintf(GinkgoWriter, "schedule=%s Run error=%v attempts=%d (only final successful attempt is committed)\n", schedule, runErr, len(attempts))
			for i, a := range attempts {
				a.ledger.print(fmt.Sprintf("schedule=%s attempt=%d", schedule, i))
				fmt.Fprintf(GinkgoWriter, "attempt=%d twoStores=%t gateState=%+v deletedState=%+v savedState=%+v openErr=%v waitErr=%v callbackErr=%v saveErr=%v deleteErr=%v unrelatedErr=%v\n", i, a.twoStores, a.gateState, a.deletedState, a.savedState, a.openErr, a.waitErr, a.callbackErr, a.save.err, a.deletion.err, a.unrelated.err)
			}
			// Collect committed raw state and both public failures before any
			// assertion can stop the diagnostic at its first corruption error.
			censuses, inspectErr := lifecycleInspect(ctx, fixture, true)
			lifecyclePrintCensus("post-lifecycle committed census", censuses, inspectErr)
			Expect(runErr).NotTo(HaveOccurred(), "producer schedule must finish and commit")
			Expect(attempts).NotTo(BeEmpty())
			a := attempts[len(attempts)-1]
			Expect(a.twoStores).To(BeTrue())
			Expect(a.save.called && a.deletion.called).To(BeTrue())
			Expect(a.save.err).NotTo(HaveOccurred())
			Expect(a.deletion.err).NotTo(HaveOccurred())
			Expect(a.deletion.found).To(BeTrue())
			if a.gated {
				Expect(a.unrelated.called).To(BeTrue())
				Expect(a.unrelated.err).NotTo(HaveOccurred(), "unrelated PK must progress while target SaveRecord is held")
				Expect(a.unrelated.record).NotTo(BeNil())
				Expect(a.gateState.cached || a.gateState.queued).To(BeFalse(), "gate must precede target version enqueue")
				Expect(a.deletedState.cached || a.deletedState.queued).To(BeFalse(), "delete must remove the target version after physical-group contention")
				Expect(lifecycleSavedVersion(a.save.record)).NotTo(BeNil())
				Expect(a.save.record.Version.IsComplete()).To(BeFalse())
				bodySet := a.ledger.first("SetBytes", a.bodyKey)
				if bodySet < 0 {
					bodySet = a.ledger.first("Set", a.bodyKey)
				}
				// The first body clear belongs to SaveRecord's old-image cleanup;
				// find the delete's later clear after the forwarded body Set.
				a.ledger.mu.Lock()
				deleteClear := -1
				for i, event := range a.ledger.events {
					if i > bodySet && (event.op == "Clear" || event.op == "ClearBytes") && bytes.Equal(event.key, a.bodyKey) {
						deleteClear = i
						break
					}
				}
				a.ledger.mu.Unlock()
				flush := a.ledger.first("SetVersionstampedValue", a.versionKey)
				Expect(bodySet).To(BeNumerically(">=", 0))
				Expect(deleteClear).To(BeNumerically(">", bodySet))
				Expect(flush).To(Equal(-1), "delete must remove the target deferred version before real Run precommit")
			}
			Expect(censuses).NotTo(BeEmpty())
			c := censuses[len(censuses)-1]
			Expect(errors.Join(c.openErr, c.rangeErr, c.bodyErr, c.versionErr, c.decodeErr)).NotTo(HaveOccurred())
			Expect(c.freshState.cached || c.freshState.queued).To(BeFalse(), "fresh-context observations cannot come from a previous local-version overlay")
			if c.version != nil {
				Expect(c.version.IsComplete()).To(BeTrue())
				if a.gated {
					Expect(c.version.GetLocalVersion()).To(Equal(a.save.record.Version.GetLocalVersion()))
				}
			}
			Expect(c.body == nil && c.versionBytes != nil).To(BeFalse(), "COMMITTED_VERSION_WITHOUT_BODY: see raw census and mutation transcript above")
			if schedule == "sequential save then delete" || a.gated {
				Expect(c.rows).To(BeEmpty())
				Expect(c.loaded).To(BeNil())
			} else if schedule == "sequential delete then save" {
				Expect(c.rows).To(HaveLen(2))
				Expect(c.loaded).NotTo(BeNil())
				Expect(c.loaded.Record.(*demopb.Order).GetQuantity()).To(Equal(int32(2)))
			}
			Expect(c.loadErr).NotTo(HaveOccurred())
			Expect(c.nextSaveErr).NotTo(HaveOccurred())
			Expect(inspectErr).NotTo(HaveOccurred())
			fmt.Fprintf(GinkgoWriter, "RECORD_LIFECYCLE_CONTROL schedule=%q completed=true healthySeed=true twoStores=%t rawSuffixes=%d bodyPresent=%t versionPresent=%t loadErr=%v nextSaveErr=%v\n", schedule, a.twoStores, len(c.rows), c.body != nil, c.versionBytes != nil, c.loadErr, c.nextSaveErr)
		}
	})
})
