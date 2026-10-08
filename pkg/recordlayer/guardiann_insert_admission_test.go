package recordlayer

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

var _ = Describe("GuardiANN insert admission", func() {
	// RFC-257 WS-D declared (b): with insertMaxCandidateClusters below 1 Java
	// writes the vector's identity and no reference, so no search returns it,
	// and the knob is immutable. Go refuses the insert with a typed capability
	// error that also poisons the transaction, so the record write cannot
	// commit without its index entry, and refuses it at queue enqueue too.
	It("refuses an insert no search could find, and the transaction cannot commit", func() {
		ctx := context.Background()
		root := specSubspace()
		builder := baseBuilder()
		index := NewVectorIndex("Order$no_candidates", KeyWithValue(Concat(Field("quantity"), Field("price")), 1), 1)
		index.Options[IndexOptionVectorEngine] = "GUARDIANN"
		index.Options[IndexOptionGuardiannInsertMaxCandidateClusters] = "0"
		builder.AddIndex("Order", index)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		var refused error
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			_, refused = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(5), Quantity: proto.Int32(1)})
			return nil, nil // the caller swallows the refusal and tries to commit
		})
		var capability *VectorCapabilityError
		Expect(errors.As(refused, &capability)).To(BeTrue(), "%v", refused)
		Expect(capability.Option).To(Equal(IndexOptionGuardiannInsertMaxCandidateClusters))
		Expect(errors.As(err, &capability)).To(BeTrue(), "the commit is refused with the same error: %v", err)

		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			rec, err := store.LoadRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(rec).To(BeNil(), "the refused record write did not commit")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// A queued index refuses the save at enqueue, not at a replay that
		// could never apply it.
		queued := root.Sub("queued")
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(queued).SetFormatVersion(15).Create()
			if err != nil {
				return nil, err
			}
			_, err = store.MarkIndexWriteOnlyWithQueue(index.Name)
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(queued).Open()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(5), Quantity: proto.Int32(1)})
			return nil, err
		})
		Expect(errors.As(err, &capability)).To(BeTrue(), "%v", err)
	})
})
