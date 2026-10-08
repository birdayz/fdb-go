package recordlayer

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// Index-state read conflicts are Java's (RFC-257 WS-E 6.4, measured by the
// target's indexStateReadScopeProbe): planning and plan revalidation read the
// loaded states with no conflict (PeekIndexStates), a scan conflicts on the
// state key of the index it scans (ReadIndexState), and getAllIndexStates
// conflicts on the whole index-state subspace. Each case changes one index's
// state the way the probe does, a raw write of its state key in its own
// transaction, while the reader's transaction is open; the reader then writes
// a record of a type with no indexes and commits.
var _ = Describe("index-state read conflicts", func() {
	ctx := context.Background()
	md := func() *RecordMetaData {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", NewIndex("order_price", Field("price")))
		builder.AddIndex("Order", NewIndex("order_quantity", Field("quantity")))
		metaData, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		return metaData
	}()

	// commitAfterStateChange opens the reader, lets read take its states,
	// commits a raw state-key write of changed, and returns the reader's
	// commit error.
	commitAfterStateChange := func(read func(*FDBRecordStore), changed string) error {
		root := specSubspace()
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			_, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).CreateOrOpen()
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		tx, err := sharedDB.CreateWritableTransaction()
		Expect(err).NotTo(HaveOccurred())
		reader := NewFDBRecordContext(tx, nil)
		DeferCleanup(reader.Cancel)
		store, err := NewStoreBuilder().SetContext(reader).SetMetaDataProvider(md).SetSubspace(root).Open()
		Expect(err).NotTo(HaveOccurred())
		read(store)
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			other, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Open()
			if err != nil {
				return nil, err
			}
			rc.Transaction().Set(other.IndexStateSubspace().Pack(tuple.Tuple{changed}),
				tuple.Tuple{int64(IndexStateDisabled)}.Pack())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("w")})
		Expect(err).NotTo(HaveOccurred())
		return reader.Commit()
	}
	expectConflict := func(err error) {
		var conflict fdb.Error
		Expect(errors.As(err, &conflict)).To(BeTrue(), "commit = %v, want not_committed (1020)", err)
		Expect(conflict.Code).To(Equal(1020))
	}

	It("PeekIndexStates takes no conflict", func() {
		var states map[string]IndexState
		err := commitAfterStateChange(func(s *FDBRecordStore) { states = s.PeekIndexStates() }, "order_price")
		Expect(err).NotTo(HaveOccurred())
		Expect(states).To(Equal(map[string]IndexState{
			"order_price": IndexStateReadable, "order_quantity": IndexStateReadable,
		}))
	})

	It("a scan's state read conflicts on its own index only", func() {
		read := func(s *FDBRecordStore) {
			state, err := s.ReadIndexState("order_price")
			Expect(err).NotTo(HaveOccurred())
			Expect(state).To(Equal(IndexStateReadable))
		}
		Expect(commitAfterStateChange(read, "order_quantity")).To(Succeed())
		expectConflict(commitAfterStateChange(read, "order_price"))
	})

	It("GetAllIndexStates conflicts on every index's state", func() {
		expectConflict(commitAfterStateChange(func(s *FDBRecordStore) { _ = s.GetAllIndexStates() }, "order_quantity"))
	})
})
