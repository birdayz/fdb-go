package recordlayer

import (
	"context"
	"errors"

	"fdb.dev/gen"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// This package's test binary does not link package vectorindex (it cannot: the
// vector package imports this one), so it is exactly the binary that forgot the
// import. Every path that needs the vector implementation must refuse loudly,
// naming the package, rather than treat the index as one some other program
// maintains.
var _ = Describe("a vector index in a binary that does not link vectorindex", func() {
	ctx := context.Background()

	expectNotLinked := func(err error, indexName, indexType string) {
		GinkgoHelper()
		var notLinked *IndexMaintainerNotLinkedError
		Expect(errors.As(err, &notLinked)).To(BeTrue(), "expected IndexMaintainerNotLinkedError, got %T: %v", err, err)
		Expect(notLinked.IndexName).To(Equal(indexName))
		Expect(notLinked.IndexType).To(Equal(indexType))
		Expect(notLinked.Package).To(Equal("fdb.dev/pkg/recordlayer/vectorindex"))
		Expect(err.Error()).To(ContainSubstring(`import _ "fdb.dev/pkg/recordlayer/vectorindex"`))
		var md *MetaDataError
		Expect(errors.As(err, &md)).To(BeTrue(), "the refusal must still read as Java's registry MetaDataException")
	}

	for _, tc := range []struct {
		name  string
		index func() *Index
	}{
		{"VECTOR", func() *Index {
			return NewVectorIndex("Order$vec", KeyWithValue(Concat(Field("quantity"), Field("price")), 1), 1)
		}},
		{"SPFresh", func() *Index {
			idx := NewIndex("Order$vec", Concat(Field("price"), Field("quantity")))
			idx.Type = IndexTypeVectorSPFresh
			idx.Options = map[string]string{IndexOptionSPFreshNumDimensions: "2"}
			return idx
		}},
	} {
		It("refuses to maintain a "+tc.name+" index", func() {
			idx := tc.index()
			builder := baseBuilder()
			builder.AddIndex("Order", idx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred(), "meta-data holding the index still builds; only using it needs the package")
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				return store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(1), Quantity: proto.Int32(1)})
			})
			expectNotLinked(err, idx.Name, idx.Type)
		})

		It("refuses to validate a changed "+tc.name+" option", func() {
			oldIdx, newIdx := tc.index(), tc.index()
			newIdx.Options = map[string]string{IndexOptionVectorMetric: "COSINE_METRIC"}
			err := ValidateChangedIndexOptions(oldIdx, newIdx, computeChangedOptions(oldIdx.Options, newIdx.Options))
			expectNotLinked(err, newIdx.Name, newIdx.Type)
		})
	}

	It("refuses to build a windowed VECTOR index it cannot validate", func() {
		idx := NewVectorIndex("Order$window", KeyWithValue(Concat(Field("quantity"), Field("price")), 1), 1)
		Expect(idx.SetPredicateProto(&gen.Predicate{RowNumberWindowPredicate: &gen.RowNumberWindowPredicate{
			OrderingField: []string{"price"},
			Size:          proto.Int32(2),
			Direction:     gen.RowNumberWindowPredicate_ASC.Enum(),
		}})).To(Succeed())
		builder := baseBuilder()
		builder.AddIndex("Order", idx)
		_, err := builder.Build()
		expectNotLinked(err, idx.Name, idx.Type)
	})
})
