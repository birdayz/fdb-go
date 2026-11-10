package recordlayer

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoregistry"
)

func TestPartitionProtocolByteGoldens(t *testing.T) {
	t.Parallel()
	b := partitionTestBuilder()
	b.GetRecordType("Order").SetRecordTypeKey("DOCS")
	md, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	rt := md.GetRecordType("Order")
	ss := partitionSubspace(subspace.FromBytes(nil), rt)
	base := "026664625f676f2e696e74657276616c5f706172746974696f6e7300150102444f435300"
	for key := int64(0); key <= 10; key++ {
		if bytes.HasPrefix(ss.Bytes(), (tuple.Tuple{key}).Pack()) {
			t.Fatalf("Go namespace collides with Java numeric keyspace %d", key)
		}
	}
	for _, tc := range []struct {
		name string
		key  tuple.Tuple
		want string
	}{
		{"header", tuple.Tuple{"header"}, base + "0268656164657200"},
		{"epoch", tuple.Tuple{"epoch"}, base + "0265706f636800"},
		{"active-negative", tuple.Tuple{"active", int64(-2)}, base + "026163746976650013fd"},
		{"fence-zero", tuple.Tuple{"fence", int64(0)}, base + "0266656e63650014"},
	} {
		if got := hex.EncodeToString(ss.Pack(tc.key)); got != tc.want {
			t.Errorf("%s = %s; want %s", tc.name, got, tc.want)
		}
	}
	spec, err := rt.PartitionSpec()
	if err != nil {
		t.Fatal(err)
	}
	incarnation := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	if got, want := hex.EncodeToString(partitionHeader(rt, spec, incarnation).Pack()), "150115061503150a0100ff0102030405060708090a0b0c0d0e0f00"; got != want {
		t.Fatalf("header = %s; want %s", got, want)
	}
	if got := hex.EncodeToString((tuple.Tuple{int64(1), true}).Pack()); got != "150127" {
		t.Fatalf("live fence = %s", got)
	}
	if got := hex.EncodeToString((tuple.Tuple{int64(1), false}).Pack()); got != "150126" {
		t.Fatalf("tombstone fence = %s", got)
	}
}

func TestPartitionProtocolLayoutIdentityUsesFieldNumber(t *testing.T) {
	t.Parallel()
	md, err := partitionTestBuilder().Build()
	if err != nil {
		t.Fatal(err)
	}
	rt := md.GetRecordType("Order")
	spec, err := rt.PartitionSpec()
	if err != nil {
		t.Fatal(err)
	}
	incarnation := make([]byte, 16)
	original := partitionHeader(rt, spec, incarnation).Pack()
	file := protodesc.ToFileDescriptorProto(md.FileDescriptor())
	for _, message := range file.MessageType {
		if message.GetName() == "Order" {
			for _, field := range message.Field {
				if field.GetName() == "coord_x" {
					field.Name = proto.String("route_x")
				}
			}
		}
	}
	fd, err := protodesc.NewFile(file, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatal(err)
	}
	renamed := *rt
	renamed.Descriptor = fd.Messages().ByName("Order")
	renamedSpec := &PartitionSpec{sourceColumn: "route_x", intervalWidth: 10}
	if !bytes.Equal(original, partitionHeader(&renamed, renamedSpec, incarnation).Pack()) {
		t.Fatal("compatible field-number-preserving rename changed layout identity")
	}
	for _, changed := range []*PartitionSpec{{sourceColumn: "coord_y", intervalWidth: 10}, {sourceColumn: "coord_x", intervalWidth: 11}} {
		if bytes.Equal(original, partitionHeader(rt, changed, incarnation).Pack()) {
			t.Fatal("changed source/width retained layout identity")
		}
	}
}

func TestPartitionProtocolCheckedCounters(t *testing.T) {
	t.Parallel()
	rt := &RecordType{Name: "T"}
	for _, raw := range [][]byte{nil, {0xff}, {0x16, 0x00, 0x01}, (tuple.Tuple{int64(0)}).Pack(), (tuple.Tuple{int64(-1)}).Pack(), (tuple.Tuple{"1"}).Pack(), (tuple.Tuple{true}).Pack(), (tuple.Tuple{int64(1), int64(2)}).Pack()} {
		_, err := partitionCounter(rt, -2, raw)
		var state *PartitionStateError
		if !errors.As(err, &state) || state.RecordType != "T" || state.Coordinate != -2 {
			t.Fatalf("counter %x: %v", raw, err)
		}
	}
	for _, n := range []int64{1, math.MaxInt64} {
		got, err := partitionCounter(rt, 0, (tuple.Tuple{n}).Pack())
		if err != nil || got != n {
			t.Fatalf("counter %d: %d, %v", n, got, err)
		}
	}
	for _, n := range []int64{-1, math.MaxInt64} {
		if _, err := nextPartitionCounter(rt, 0, n); err == nil {
			t.Fatalf("increment %d succeeded", n)
		}
	}
	for _, n := range []int64{0, math.MaxInt64 - 1} {
		got, err := nextPartitionCounter(rt, 0, n)
		if err != nil || got != n+1 {
			t.Fatalf("increment %d: %d, %v", n, got, err)
		}
	}
}

type partitionProtocolTransaction struct {
	fdb.WritableTransaction
	mu    sync.Mutex
	reads [][]byte
	sets  [][]byte
}

func (tx *partitionProtocolTransaction) Get(key fdb.KeyConvertible) fdb.FutureByteSlice {
	tx.mu.Lock()
	tx.reads = append(tx.reads, bytes.Clone(key.FDBKey()))
	tx.mu.Unlock()
	return tx.WritableTransaction.Get(key)
}

func (tx *partitionProtocolTransaction) Set(key fdb.KeyConvertible, value []byte) {
	tx.mu.Lock()
	tx.sets = append(tx.sets, bytes.Clone(key.FDBKey()))
	tx.mu.Unlock()
	tx.WritableTransaction.Set(key, value)
}

func partitionOrder(id, source int64, price int32) *demopb.Order {
	return &demopb.Order{OrderId: proto.Int64(id), CoordX: proto.Int64(source), Price: proto.Int32(price)}
}

func partitionProtocolRows(rc *FDBRecordContext, root subspace.Subspace) []fdb.KeyValue {
	GinkgoHelper()
	rows, err := rc.Transaction().GetRange(root.Sub(partitionNamespace, int64(1)), fdb.RangeOptions{}).GetSliceWithError()
	Expect(err).NotTo(HaveOccurred())
	return rows
}

func partitionProtocolStore(rc *FDBRecordContext, md *RecordMetaData, root subspace.Subspace) *FDBRecordStore {
	GinkgoHelper()
	store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).CreateOrOpen()
	Expect(err).NotTo(HaveOccurred())
	return store
}

type partitionMaintenanceGate struct {
	id          int64
	entered     chan struct{}
	release     chan struct{}
	once        sync.Once
	releaseOnce sync.Once
}

func (g *partitionMaintenanceGate) MaintainIndex(_ *Index, record proto.Message) IndexValues {
	if record.(*demopb.Order).GetOrderId() == g.id {
		g.once.Do(func() {
			close(g.entered)
			<-g.release
		})
	}
	return IndexValuesAll
}

func (*partitionMaintenanceGate) MaintainIndexValue(*Index, proto.Message, *IndexEntry) bool {
	return true
}

func (g *partitionMaintenanceGate) unblock() { g.releaseOnce.Do(func() { close(g.release) }) }

func partitionGuardCount(rc *FDBRecordContext) int {
	rc.commitMu.Lock()
	defer rc.commitMu.Unlock()
	count := 0
	for name := range rc.namedCommitChecks {
		if bytes.HasPrefix([]byte(name), []byte("partition-mutation:")) {
			count++
		}
	}
	return count
}

type partitionPanicFilter struct{ normalMaintenanceFilter }

func (partitionPanicFilter) MaintainIndex(*Index, proto.Message) IndexValues {
	panic("partition callback panic")
}

func newPartitionMaintenanceGate(id int64) *partitionMaintenanceGate {
	return &partitionMaintenanceGate{id: id, entered: make(chan struct{}), release: make(chan struct{})}
}

func partitionAwait(done <-chan struct{}) {
	GinkgoHelper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		Fail("timed out waiting for partition protocol barrier")
	}
}

var _ = Describe("Partition routing protocol", func() {
	ctx := context.Background()

	It("creates exact sparse state for negative/extreme coordinates and isolates two types", func() {
		md, err := partitionTestBuilder().SetStoreRecordVersions(true).Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		xs := []int64{math.MinInt64, -11, -1, 0, 10, math.MaxInt64}
		ps := []int64{-922337203685477581, -2, -1, 0, 1, 922337203685477580}
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			Expect(partitionProtocolRows(rc, root)).To(BeEmpty())
			for i, x := range xs {
				_, err := store.SaveRecord(partitionOrder(1, x, 7))
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&demopb.TypedRecord{Id: proto.Int64(1), ValInt64: proto.Int64(x)})
				Expect(err).NotTo(HaveOccurred())
				for _, name := range []string{"Order", "TypedRecord"} {
					ss := partitionSubspace(root, md.GetRecordType(name))
					Expect(rc.Transaction().Get(ss.Pack(tuple.Tuple{"active", ps[i]})).MustGet()).To(Equal((tuple.Tuple{int64(1)}).Pack()))
					Expect(rc.Transaction().Get(ss.Pack(tuple.Tuple{"fence", ps[i]})).MustGet()).To(Equal((tuple.Tuple{int64(1), true}).Pack()))
				}
			}
			Expect(partitionProtocolRows(rc, root)).To(HaveLen(2 * (2 + 2*len(xs))))
			for _, name := range []string{"Order", "TypedRecord"} {
				ss := partitionSubspace(root, md.GetRecordType(name))
				Expect(rc.Transaction().Get(ss.Pack(tuple.Tuple{"epoch"})).MustGet()).To(Equal((tuple.Tuple{int64(1)}).Pack()), "one epoch change per table/transaction")
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("does not read epoch or rewrite shared state for an active route", func() {
		md, err := partitionTestBuilder().Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			_, err := partitionProtocolStore(rc, md, root).SaveRecord(partitionOrder(1, -11, 7))
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		rc, err := sharedDB.OpenRecordContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer rc.Cancel()
		store := partitionProtocolStore(rc, md, root)
		before := partitionProtocolRows(rc, root)
		probe := &partitionProtocolTransaction{WritableTransaction: rc.tx}
		rc.tx = probe
		_, err = store.SaveRecord(partitionOrder(1, -11, 8))
		Expect(err).NotTo(HaveOccurred())
		pk := tuple.Tuple{md.GetRecordType("Order").GetRecordTypeKey(), int64(-2), int64(-11), int64(1)}
		found, err := store.DeleteRecord(pk)
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		_, err = store.SaveRecord(partitionOrder(1, -11, 9))
		Expect(err).NotTo(HaveOccurred())
		ss := partitionSubspace(root, md.GetRecordType("Order"))
		for _, key := range probe.reads {
			Expect(key).NotTo(Equal(ss.Pack(tuple.Tuple{"epoch"})))
		}
		for _, key := range probe.sets {
			Expect(bytes.HasPrefix(key, root.Sub(partitionNamespace).Bytes())).To(BeFalse(), "active-route write changed extension key %x", key)
		}
		for _, part := range []tuple.Tuple{{"header"}, {"active", int64(-2)}, {"fence", int64(-2)}} {
			found := false
			for _, key := range probe.reads {
				found = found || bytes.Equal(key, ss.Pack(part))
			}
			Expect(found).To(BeTrue(), "positive read population for %v", part)
		}
		Expect(partitionProtocolRows(rc, root)).To(Equal(before))
		Expect(rc.Commit()).To(Succeed())
	})

	It("does not conflict active writers with an unrelated activation", func() {
		md, err := partitionTestBuilder().Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			_, err := partitionProtocolStore(rc, md, root).SaveRecord(partitionOrder(1, 0, 7))
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		a, err := sharedDB.OpenRecordContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer a.Cancel()
		b, err := sharedDB.OpenRecordContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer b.Cancel()
		_, err = partitionProtocolStore(a, md, root).SaveRecord(partitionOrder(1, 0, 8))
		Expect(err).NotTo(HaveOccurred())
		_, err = partitionProtocolStore(b, md, root).SaveRecord(partitionOrder(2, 100, 9))
		Expect(err).NotTo(HaveOccurred())
		Expect(b.Commit()).To(Succeed())
		Expect(a.Commit()).To(Succeed())
	})

	It("conflicts competing first activations and retries with fresh context state", func() {
		md, err := partitionTestBuilder().Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) { partitionProtocolStore(rc, md, root); return nil, nil })
		Expect(err).NotTo(HaveOccurred())
		a, err := sharedDB.OpenRecordContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer a.Cancel()
		b, err := sharedDB.OpenRecordContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer b.Cancel()
		_, err = partitionProtocolStore(a, md, root).SaveRecord(partitionOrder(1, -11, 7))
		Expect(err).NotTo(HaveOccurred())
		_, err = partitionProtocolStore(b, md, root).SaveRecord(partitionOrder(2, -12, 8))
		Expect(err).NotTo(HaveOccurred())
		Expect(a.Commit()).To(Succeed())
		var conflict fdb.Error
		Expect(errors.As(b.Commit(), &conflict)).To(BeTrue())
		Expect(conflict.Code).To(Equal(1020))
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			_, err := partitionProtocolStore(rc, md, root).SaveRecord(partitionOrder(2, -12, 8))
			Expect(err).NotTo(HaveOccurred())
			Expect(partitionProtocolRows(rc, root)).To(HaveLen(4))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("initializes one canonical state and guard under parallel first use through multiple handles", func() {
		md, err := partitionTestBuilder().Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) { partitionProtocolStore(rc, md, root); return nil, nil })
		Expect(err).NotTo(HaveOccurred())
		rc, err := sharedDB.OpenRecordContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer rc.Cancel()
		stores := make([]*FDBRecordStore, 8)
		for i := range stores {
			stores[i] = partitionProtocolStore(rc, md, root)
		}
		probe := &partitionProtocolTransaction{WritableTransaction: rc.tx}
		rc.tx = probe
		start := make(chan struct{})
		results := make(chan error, len(stores))
		var wg sync.WaitGroup
		for i, store := range stores {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := store.SaveRecord(partitionOrder(int64(i+1), -11, int32(i)))
				results <- err
			}()
		}
		close(start)
		wg.Wait()
		for range stores {
			Expect(<-results).To(Succeed())
		}
		state := stores[0].partitionContextState()
		for _, store := range stores {
			Expect(store.partitionContextState()).To(BeIdenticalTo(state))
		}
		Expect(partitionGuardCount(rc)).To(Equal(1))
		Expect(partitionProtocolRows(rc, root)).To(HaveLen(4))
		ss := partitionSubspace(root, md.GetRecordType("Order"))
		for _, part := range []tuple.Tuple{{"header"}, {"epoch"}, {"active", int64(-2)}, {"fence", int64(-2)}} {
			n := 0
			for _, key := range probe.sets {
				if bytes.Equal(key, ss.Pack(part)) {
					n++
				}
			}
			Expect(n).To(Equal(1), "one persisted first-use intent for %v", part)
		}
		Expect(rc.Commit()).To(Succeed())
	})

	for _, failLate := range []bool{false, true} {
		It(fmt.Sprintf("rechecks the named guard after concurrent Commit's initially clean check (late_failure=%v)", failLate), func() {
			b := partitionTestBuilder()
			idx := NewIndex("commit_guard_unique", Concat(PartitionExpr(Field("coord_x"), 10), Field("coord_x"), Field("price"))).SetUnique()
			b.AddIndex("Order", idx)
			md, err := b.Build()
			Expect(err).NotTo(HaveOccurred())
			root := specSubspace()
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				_, err := partitionProtocolStore(rc, md, root).SaveRecord(partitionOrder(1, 0, 7))
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			rc, err := sharedDB.OpenRecordContext(ctx)
			Expect(err).NotTo(HaveOccurred())
			defer rc.Cancel()
			gate := newPartitionMaintenanceGate(2)
			defer gate.unblock()
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).SetIndexMaintenanceFilter(gate).Open()
			Expect(err).NotTo(HaveOccurred())
			writerDone := make(chan struct{})
			var writerErr error
			price := int32(8)
			if failLate {
				price = 7
			}
			go func() { defer close(writerDone); _, writerErr = store.SaveRecord(partitionOrder(2, 0, price)) }()
			partitionAwait(gate.entered)
			checked := make(chan struct{})
			rc.AddCommitCheck(func() error { close(checked); return nil })
			commitDone := make(chan struct{})
			var commitErr error
			go func() { defer close(commitDone); commitErr = rc.Commit() }()
			partitionAwait(checked)
			select {
			case <-commitDone:
				Fail("commit returned while maintenance remained admitted")
			default:
			}
			gate.unblock()
			partitionAwait(writerDone)
			partitionAwait(commitDone)
			if failLate {
				Expect(writerErr).To(HaveOccurred())
				var incomplete *PartitionMutationError
				Expect(errors.As(commitErr, &incomplete)).To(BeTrue(), "%v", commitErr)
				Expect(errors.Is(commitErr, writerErr)).To(BeTrue(), "the sticky guard retains the actual late failure")
			} else {
				Expect(writerErr).NotTo(HaveOccurred())
				Expect(commitErr).NotTo(HaveOccurred())
			}
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionProtocolStore(rc, md, root)
				rows, err := AsList(ctx, store.ScanRecords(nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				want := 2
				if failLate {
					want = 1
				}
				Expect(rows).To(HaveLen(want))
				indexed, err := AsList(ctx, store.ScanIndexRecords(idx.Name, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(indexed).To(HaveLen(want))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	It("keeps a multi-handle late failure sticky when an already-admitted operation succeeds afterward", func() {
		b := partitionTestBuilder()
		idx := NewIndex("sticky_unique", Concat(PartitionExpr(Field("coord_x"), 10), Field("coord_x"), Field("price"))).SetUnique()
		b.AddIndex("Order", idx)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			_, err := partitionProtocolStore(rc, md, root).SaveRecord(partitionOrder(1, 0, 7))
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		rc, err := sharedDB.OpenRecordContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer rc.Cancel()
		before, err := rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
		Expect(err).NotTo(HaveOccurred())
		failedGate, successGate := newPartitionMaintenanceGate(2), newPartitionMaintenanceGate(3)
		defer failedGate.unblock()
		defer successGate.unblock()
		failedStore, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).SetIndexMaintenanceFilter(failedGate).Open()
		Expect(err).NotTo(HaveOccurred())
		successStore, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).SetIndexMaintenanceFilter(successGate).Open()
		Expect(err).NotTo(HaveOccurred())
		failedDone, successDone := make(chan struct{}), make(chan struct{})
		var failedErr, successErr error
		go func() { defer close(failedDone); _, failedErr = failedStore.SaveRecord(partitionOrder(2, 0, 7)) }()
		partitionAwait(failedGate.entered)
		go func() { defer close(successDone); _, successErr = successStore.SaveRecord(partitionOrder(3, 10, 9)) }()
		partitionAwait(successGate.entered)
		Expect(failedStore.partitionContextState()).To(BeIdenticalTo(successStore.partitionContextState()))
		Expect(partitionGuardCount(rc)).To(Equal(1))
		failedGate.unblock()
		partitionAwait(failedDone)
		Expect(failedErr).To(HaveOccurred())
		successGate.unblock()
		partitionAwait(successDone)
		Expect(successErr).NotTo(HaveOccurred())
		var incomplete *PartitionMutationError
		commitErr := rc.Commit()
		Expect(errors.As(commitErr, &incomplete)).To(BeTrue())
		Expect(errors.Is(commitErr, failedErr)).To(BeTrue())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			after, err := rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(after).To(Equal(before))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("marks a panicking callback rollback-only before releasing its write admission", func() {
		b := partitionTestBuilder()
		b.AddIndex("Order", NewIndex("panic_value", Concat(PartitionExpr(Field("coord_x"), 10), Field("price"))))
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) { partitionProtocolStore(rc, md, root); return nil, nil })
		Expect(err).NotTo(HaveOccurred())
		rc, err := sharedDB.OpenRecordContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer rc.Cancel()
		store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).SetIndexMaintenanceFilter(partitionPanicFilter{}).Open()
		Expect(err).NotTo(HaveOccurred())
		var recovered any
		func() {
			defer func() { recovered = recover() }()
			_, _ = store.SaveRecord(partitionOrder(1, -11, 7))
		}()
		Expect(recovered).To(Equal("partition callback panic"))
		Expect(partitionProtocolRows(rc, root)).To(HaveLen(4), "activation effects really happened")
		var incomplete *PartitionMutationError
		Expect(errors.As(rc.Commit(), &incomplete)).To(BeTrue())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			Expect(partitionProtocolRows(rc, root)).To(BeEmpty())
			rows, err := AsList(ctx, partitionProtocolStore(rc, md, root).ScanRecords(nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(BeEmpty())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("rejects a same-context reset racing delayed index maintenance instead of committing orphan entries", func() {
		b := partitionTestBuilder()
		idx := NewIndex("reset_race_value", Concat(PartitionExpr(Field("coord_x"), 10), Field("price")))
		b.AddIndex("Order", idx)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			_, err := partitionProtocolStore(rc, md, root).SaveRecord(partitionOrder(1, 0, 7))
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		rc, err := sharedDB.OpenRecordContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer rc.Cancel()
		gate := newPartitionMaintenanceGate(2)
		defer gate.unblock()
		store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).SetIndexMaintenanceFilter(gate).Open()
		Expect(err).NotTo(HaveOccurred())
		other := partitionProtocolStore(rc, md, root)
		writerDone := make(chan struct{})
		var writerErr error
		go func() { defer close(writerDone); _, writerErr = store.SaveRecord(partitionOrder(2, 0, 8)) }()
		partitionAwait(gate.entered)
		Expect(other.DeleteAllRecords()).To(Succeed())
		gate.unblock()
		partitionAwait(writerDone)
		Expect(writerErr).NotTo(HaveOccurred())
		var incomplete *PartitionMutationError
		Expect(errors.As(rc.Commit(), &incomplete)).To(BeTrue())
		var state *PartitionStateError
		Expect(errors.As(incomplete, &state)).To(BeTrue())
		Expect(state.Reason).To(Equal("table incarnation changed during partition mutation"))
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			rows, err := AsList(ctx, store.ScanIndexRecords(idx.Name, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(HaveLen(1))
			Expect(rows[0].Record.Record.(*demopb.Order).GetOrderId()).To(Equal(int64(1)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
