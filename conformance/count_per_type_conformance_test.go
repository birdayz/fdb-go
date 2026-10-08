//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"errors"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// Java's getSnapshotRecordCountForRecordType (FDBRecordStore.java:2431-2453)
// counts a type from a COUNT index, never from the record count key.
var _ = Describe("RFC-257 a record type's count comes from a COUNT index, as Java's", func() {
	for _, c := range []struct {
		mode      string
		javaCount int64
		javaError string
	}{
		{"countKey", -1, "Require a COUNT index on Order"},
		{"typeIndex", 3, ""},
		{"universalByType", 3, ""},
	} {
		It(c.mode, func() {
			ctx := context.Background()
			clusterFile, err := sharedContainer.ClusterFile(ctx)
			Expect(err).NotTo(HaveOccurred())
			var java struct {
				Count int64  `json:"count"`
				Class string `json:"class"`
				Error string `json:"error"`
			}
			javaSS := subspace.Sub(tuple.Tuple{"per_type_java", uuid.NewString()}...)
			Expect(NewJavaInvoker().InvokeAs(ctx, "perTypeRecordCountJava", map[string]any{
				"clusterFile": clusterFile, "subspace": BytesToIntArray(javaSS.Bytes()), "mode": c.mode,
			}, &java)).To(Succeed())
			GinkgoWriter.Printf("PERTYPECOUNT %s java=%d %s %q\n", c.mode, java.Count, java.Class, java.Error)
			Expect([]any{java.Count, java.Error}).To(Equal([]any{c.javaCount, c.javaError}))

			b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
			b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
			b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
			switch c.mode {
			case "countKey":
				b.SetRecordCountKey(recordlayer.RecordTypeKey())
			case "typeIndex":
				b.AddIndex("Order", recordlayer.NewCountIndex("order_count", recordlayer.GroupAll(recordlayer.EmptyKey())))
			case "universalByType":
				b.AddUniversalIndex(recordlayer.NewCountIndex("count_by_type", recordlayer.GroupAll(recordlayer.RecordTypeKey())))
			}
			md, err := b.Build()
			Expect(err).NotTo(HaveOccurred())
			goSS := subspace.Sub(tuple.Tuple{"per_type_go", uuid.NewString()}...)
			var n int64
			_, goErr := recordlayer.NewFDBDatabase(sharedDB).Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(goSS).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				for i := int64(1); i <= 3; i++ {
					if _, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(i)}); err != nil {
						return nil, err
					}
				}
				if _, err := store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(9)}); err != nil {
					return nil, err
				}
				n, err = store.GetSnapshotRecordCountForRecordType("Order")
				return nil, err
			})
			if c.javaError != "" {
				// Java's RecordCoreException, whose Go class is RecordCoreError.
				Expect(java.Class).To(Equal("com.apple.foundationdb.record.RecordCoreException"))
				var rc *recordlayer.RecordCoreError
				Expect(errors.As(goErr, &rc)).To(BeTrue(), "%v", goErr)
				Expect(rc.Message).To(Equal(java.Error))
				return
			}
			Expect(goErr).NotTo(HaveOccurred())
			Expect(n).To(Equal(java.Count))
		})
	}
})
