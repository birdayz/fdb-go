package recordlayer

import (
	"context"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("FDBMetaDataStore", func() {
	ctx := context.Background()

	It("saves and loads metadata proto", func() {
		ss := specSubspace()
		store := NewFDBMetaDataStore(ss)

		mdProto := &gen.MetaData{
			Version: proto.Int32(1),
		}

		// Save.
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			return nil, store.SaveRecordMetaData(rtx.Transaction(), mdProto)
		})
		Expect(err).NotTo(HaveOccurred())

		// Load.
		var loaded *gen.MetaData
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			var loadErr error
			loaded, loadErr = store.LoadRecordMetaDataProto(rtx.Transaction())
			return nil, loadErr
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(loaded).NotTo(BeNil())
		Expect(proto.Equal(loaded, mdProto)).To(BeTrue())
	})

	It("archives previous version on save", func() {
		ss := specSubspace()
		store := NewFDBMetaDataStore(ss)

		// Save version 1.
		mdProto1 := &gen.MetaData{Version: proto.Int32(1)}
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			return nil, store.SaveRecordMetaData(rtx.Transaction(), mdProto1)
		})
		Expect(err).NotTo(HaveOccurred())

		// Save version 2 — should archive version 1.
		mdProto2 := &gen.MetaData{Version: proto.Int32(2)}
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			return nil, store.SaveRecordMetaData(rtx.Transaction(), mdProto2)
		})
		Expect(err).NotTo(HaveOccurred())

		// Current should be version 2.
		var current *gen.MetaData
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			var loadErr error
			current, loadErr = store.LoadRecordMetaDataProto(rtx.Transaction())
			return nil, loadErr
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(current.GetVersion()).To(Equal(int32(2)))

		// History should have version 1.
		var historical *gen.MetaData
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			var loadErr error
			historical, loadErr = store.LoadRecordMetaDataProtoAtVersion(rtx.Transaction(), 1)
			return nil, loadErr
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(historical).NotTo(BeNil())
		Expect(historical.GetVersion()).To(Equal(int32(1)))
	})

	It("returns nil for non-existent metadata", func() {
		ss := specSubspace()
		store := NewFDBMetaDataStore(ss)

		var loaded *gen.MetaData
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			var loadErr error
			loaded, loadErr = store.LoadRecordMetaDataProto(rtx.Transaction())
			return nil, loadErr
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(loaded).To(BeNil())
	})

	It("returns nil for non-existent historical version", func() {
		ss := specSubspace()
		store := NewFDBMetaDataStore(ss)

		var loaded *gen.MetaData
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			var loadErr error
			loaded, loadErr = store.LoadRecordMetaDataProtoAtVersion(rtx.Transaction(), 99)
			return nil, loadErr
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(loaded).To(BeNil())
	})

	It("Subspace returns the configured subspace", func() {
		ss := specSubspace()
		store := NewFDBMetaDataStore(ss)
		Expect(store.Subspace()).To(Equal(ss))
	})

	It("stores metadata at unsplit suffix 0 for Java wire compatibility", func() {
		ss := specSubspace()
		store := NewFDBMetaDataStore(ss)

		mdProto := &gen.MetaData{Version: proto.Int32(1)}
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			return nil, store.SaveRecordMetaData(rtx.Transaction(), mdProto)
		})
		Expect(err).NotTo(HaveOccurred())

		// Verify the raw FDB key matches Java's format: subspace.pack(null, 0)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			expectedKey := ss.Pack(tuple.Tuple{nil, int64(unsplitRecord)})
			value, getErr := rtx.Transaction().Get(fdb.Key(expectedKey)).Get()
			Expect(getErr).NotTo(HaveOccurred())
			Expect(value).NotTo(BeNil(), "metadata should be stored at unsplit suffix 0 key")
			Expect(len(value)).To(BeNumerically(">", 0))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
