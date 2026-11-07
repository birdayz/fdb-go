package recordlayer

import (
	"context"
	"errors"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// PrefetchStoreState issues an Open's two first reads early (the SQL layer
// overlaps them with its catalog witness reads). The specs pin its contract:
// the Open that follows reaches storage for neither read, opens the same
// state as an Open without it, and the reads conflict exactly as the Open's
// own do (the header read takes a conflict range, the index-state read does
// not).
var _ = Describe("PrefetchStoreState", func() {
	ctx := context.Background()

	metaBuilder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
	metaBuilder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	metaBuilder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	metaBuilder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	metaBuilder.AddIndex("Order", NewIndex("prefetch_price", Field("price")))
	metaData, metaErr := metaBuilder.Build()

	open := func(rctx *FDBRecordContext, ss subspace.Subspace) (*FDBRecordStore, error) {
		return NewStoreBuilder().SetContext(rctx).SetMetaDataProvider(metaData).SetSubspace(ss).
			SetStoreStateCache(PassThroughStoreStateCache()).Open()
	}

	// createStore creates the store at ss with its index WRITE_ONLY, so the
	// index-state range the Open reads is not empty.
	createStore := func(ss subspace.Subspace) {
		Expect(metaErr).NotTo(HaveOccurred())
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ss).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			_, err = store.MarkIndexWriteOnly("prefetch_price")
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
	}

	// storageReads is the number of point and range reads that reached
	// storage: the client counts one read-latency sample per GetValue reply
	// and one request per range read.
	storageReads := func(db fdb.Database) (gets, ranges int64) {
		m := db.Metrics()
		return m.ReadLatency.Count, m.TransactionGetRangeRequests
	}

	It("answers the Open that follows from the transaction's reads, with the same state", func() {
		ss := specSubspace()
		createStore(ss)
		// A client of its own, so its counters see only this spec's reads.
		raw, err := fdb.OpenDatabase(clusterTmpFilePath)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(raw.Close)
		db := NewFDBDatabase(raw)

		begin := func() *FDBRecordContext {
			tx, err := db.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(tx.Cancel)
			// The read version first: the counts below are storage reads only.
			_, err = tx.GetReadVersion().Get()
			Expect(err).NotTo(HaveOccurred())
			return NewFDBRecordContext(tx, nil)
		}

		// Positive control: an Open without the prefetch reaches storage for
		// both reads, so a zero below is not an instrument that sees nothing.
		control := begin()
		g0, r0 := storageReads(raw)
		want, err := open(control, ss)
		Expect(err).NotTo(HaveOccurred())
		g1, r1 := storageReads(raw)
		Expect(g1-g0).To(BeNumerically(">=", 1), "the control Open's header read did not reach storage")
		Expect(r1-r0).To(BeNumerically(">=", 1), "the control Open's index-state read did not reach storage")

		rctx := begin()
		g0, r0 = storageReads(raw)
		PrefetchStoreState(rctx, ss)()
		g1, r1 = storageReads(raw)
		Expect(g1-g0).To(BeNumerically(">=", 1), "the prefetch issued no header read")
		Expect(r1-r0).To(BeNumerically(">=", 1), "the prefetch issued no index-state read")

		got, err := open(rctx, ss)
		Expect(err).NotTo(HaveOccurred())
		g2, r2 := storageReads(raw)
		Expect(g2-g1).To(BeZero(), "the Open after the prefetch read the header from storage again")
		Expect(r2-r1).To(BeZero(), "the Open after the prefetch read the index states from storage again")

		Expect(proto.Equal(got.GetStoreHeader(), want.GetStoreHeader())).To(BeTrue(),
			"header after prefetch %v, without %v", got.GetStoreHeader(), want.GetStoreHeader())
		Expect(got.PeekIndexStates()).To(Equal(want.PeekIndexStates()))
		Expect(got.PeekIndexStates()).To(HaveKeyWithValue("prefetch_price", IndexStateWriteOnly))
	})

	It("leaves a missing store missing and creatable", func() {
		Expect(metaErr).NotTo(HaveOccurred())
		ss := specSubspace()
		var openErr error
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			PrefetchStoreState(rtx, ss)()
			_, openErr = open(rtx, ss)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		var dne *RecordStoreDoesNotExistError
		Expect(errors.As(openErr, &dne)).To(BeTrue(), "Open after a prefetch of a missing store = %v", openErr)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			PrefetchStoreState(rtx, ss)()
			return NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ss).CreateOrOpen()
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// The conflict behavior is the Open's: each case runs once with the
	// prefetch and once with a plain Open as the reader's only store read, and
	// both must agree with the expectation.
	DescribeTable("conflicts as the Open does",
		func(change func(ss subspace.Subspace) error, wantConflict bool) {
			for _, reader := range []struct {
				name string
				read func(*FDBRecordContext, subspace.Subspace)
			}{
				{"prefetch", func(rctx *FDBRecordContext, ss subspace.Subspace) { PrefetchStoreState(rctx, ss)() }},
				{"open", func(rctx *FDBRecordContext, ss subspace.Subspace) {
					_, err := open(rctx, ss)
					Expect(err).NotTo(HaveOccurred())
				}},
			} {
				ss := specSubspace().Sub(reader.name)
				createStore(ss)
				tx, err := sharedDB.CreateTransaction()
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(tx.Cancel)
				reader.read(NewFDBRecordContext(tx, nil), ss)
				// A write makes the reader's commit go through conflict checking.
				tx.Set(fdb.Key(ss.Pack(tuple.Tuple{"conflict-marker"})), []byte(reader.name))

				Expect(change(ss)).To(Succeed())

				err = tx.Commit().Get()
				if wantConflict {
					var fe fdb.Error
					Expect(errors.As(err, &fe)).To(BeTrue(), "%s: commit = %v, want not_committed", reader.name, err)
					Expect(fe.Code).To(Equal(1020), "%s: commit = %v, want not_committed", reader.name, err)
				} else {
					Expect(err).NotTo(HaveOccurred(), "%s: the snapshot index-state read took a conflict range", reader.name)
				}
			}
		},
		Entry("a concurrent header change conflicts", func(ss subspace.Subspace) error {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := open(rtx, ss)
				if err != nil {
					return nil, err
				}
				return nil, store.SetHeaderUserField("prefetch-conflict", []byte("changed"))
			})
			return err
		}, true),
		Entry("a concurrent index-state change does not", func(ss subspace.Subspace) error {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.Transaction().Set(fdb.Key(ss.Sub(IndexStateSpaceKey).Pack(tuple.Tuple{"prefetch_price"})),
					tuple.Tuple{int64(IndexStateDisabled)}.Pack())
				return nil, nil
			})
			return err
		}, false),
	)
})
