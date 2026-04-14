package conformance_test

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	gofdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

var _ = Describe("FDBMetaDataStore Conformance", func() {
	var (
		ctx         context.Context
		java        *JavaInvoker
		goRecordDB  *recordlayer.FDBDatabase
		ss          subspace.Subspace
		clusterFile string
	)

	BeforeEach(func() {
		ctx = context.Background()
		java = NewJavaInvoker()
		// Use non-tenant database directly — avoids tenant prefixing issues
		// with direct SplitHelper calls in Java
		goRecordDB = recordlayer.NewFDBDatabase(sharedDB)
		// Unique subspace per spec for isolation
		prefix := fmt.Sprintf("mdstore_%s", uuid.New().String())
		ss = subspace.Sub(tuple.Tuple{prefix}...)

		var err error
		clusterFile, err = sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		// Clean up subspace
		_, _ = sharedDB.Transact(func(tr gofdb.Transaction) (any, error) {
			begin, end := ss.FDBRangeKeys()
			tr.ClearRange(gofdb.KeyRange{Begin: begin, End: end})
			return nil, nil
		})
	})

	buildMetaDataProto := func(version int32) *gen.MetaData {
		builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		mdProto, err := md.ToProto()
		Expect(err).NotTo(HaveOccurred())
		mdProto.Version = proto.Int32(version)
		return mdProto
	}

	Describe("Go writes, Java reads", func() {
		It("Java can read metadata stored by Go", func() {
			// Go saves metadata with version 42
			_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store := recordlayer.NewFDBMetaDataStore(ss)
				return nil, store.SaveRecordMetaData(rtx.Transaction(), buildMetaDataProto(42))
			})
			Expect(err).NotTo(HaveOccurred())

			// Java loads and verifies
			params := map[string]any{
				"clusterFile": clusterFile,
				"subspace":    BytesToIntArray(ss.Bytes()),
			}
			var result struct {
				Found   bool `json:"found"`
				Version int  `json:"version"`
			}
			err = java.InvokeAs(ctx, "loadMetaDataJava", params, &result)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Found).To(BeTrue())
			Expect(result.Version).To(Equal(42))
		})
	})

	Describe("Java writes, Go reads", func() {
		It("Go can read metadata stored by Java", func() {
			// Java saves metadata with version 99
			params := map[string]any{
				"clusterFile": clusterFile,
				"subspace":    BytesToIntArray(ss.Bytes()),
				"version":     99,
			}
			var saveResult struct {
				SavedBytes int `json:"savedBytes"`
			}
			err := java.InvokeAs(ctx, "saveMetaDataJava", params, &saveResult)
			Expect(err).NotTo(HaveOccurred())
			Expect(saveResult.SavedBytes).To(BeNumerically(">", 0))

			// Go loads and verifies
			var loaded *gen.MetaData
			_, err = goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store := recordlayer.NewFDBMetaDataStore(ss)
				var loadErr error
				loaded, loadErr = store.LoadRecordMetaDataProto(rtx.Transaction())
				return nil, loadErr
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded).NotTo(BeNil())
			Expect(loaded.GetVersion()).To(Equal(int32(99)))
		})
	})

	Describe("History cross-language", func() {
		It("Java can read historical version stored by Go", func() {
			// Go saves v1, then v2 (archives v1)
			_, err := goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store := recordlayer.NewFDBMetaDataStore(ss)
				return nil, store.SaveRecordMetaData(rtx.Transaction(), buildMetaDataProto(1))
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = goRecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store := recordlayer.NewFDBMetaDataStore(ss)
				return nil, store.SaveRecordMetaData(rtx.Transaction(), buildMetaDataProto(2))
			})
			Expect(err).NotTo(HaveOccurred())

			// Java reads historical v1
			params := map[string]any{
				"clusterFile": clusterFile,
				"subspace":    BytesToIntArray(ss.Bytes()),
				"version":     1,
			}
			var result struct {
				Found   bool `json:"found"`
				Version int  `json:"version"`
			}
			err = java.InvokeAs(ctx, "loadMetaDataHistoryJava", params, &result)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Found).To(BeTrue())
			Expect(result.Version).To(Equal(1))
		})
	})
})
