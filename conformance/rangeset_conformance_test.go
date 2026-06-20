//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("RangeSet Wire Format Conformance", func() {
	var (
		ctx        context.Context
		env        *TenantEnvironment
		java       *JavaInvoker
		rsSubspace subspace.Subspace
		db         *recordlayer.FDBDatabase
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("rs_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		db = env.RecordDB
		java = NewJavaInvoker()

		// Use a unique subspace for the RangeSet within the tenant
		ks := subspace.Sub(tuple.Tuple{})
		rsSubspace = ks.Sub("rangeset_test")
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	buildJavaParams := func() map[string]any {
		params := map[string]any{
			"clusterFile": env.ClusterFile,
			"rsSubspace":  BytesToIntArray(rsSubspace.Bytes()),
		}
		if env.TenantName != "" {
			params["tenantName"] = env.TenantName
		}
		return params
	}

	Describe("Go writes full range, Java reads", func() {
		It("should see the range as complete via Java", func() {
			// Go inserts full range [0x00, 0xFF)
			_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				rs := recordlayer.NewRangeSet(rsSubspace)
				_, err := rs.InsertRange(rtx.Transaction(), []byte{0x00}, []byte{0xff}, false)
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Java: contains(0x50) should be true
			params := buildJavaParams()
			params["key"] = BytesToIntArray([]byte{0x50})
			var contains bool
			err = java.InvokeAs(ctx, "rangeSetContains", params, &contains)
			Expect(err).NotTo(HaveOccurred())
			Expect(contains).To(BeTrue())

			// Java: missingRanges should be empty
			params = buildJavaParams()
			var missing []map[string]any
			err = java.InvokeAs(ctx, "rangeSetMissingRanges", params, &missing)
			Expect(err).NotTo(HaveOccurred())
			Expect(missing).To(BeEmpty())
		})
	})

	Describe("Java writes full range, Go reads", func() {
		It("should see the range as complete via Go", func() {
			// Java inserts full range [0x00, 0xFF)
			params := buildJavaParams()
			params["begin"] = BytesToIntArray([]byte{0x00})
			params["end"] = BytesToIntArray([]byte{0xff})
			var inserted bool
			err := java.InvokeAs(ctx, "rangeSetInsert", params, &inserted)
			Expect(err).NotTo(HaveOccurred())
			Expect(inserted).To(BeTrue())

			// Go: Contains(0x50) should be true
			_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				rs := recordlayer.NewRangeSet(rsSubspace)
				contains, err := rs.Contains(rtx.Transaction(), []byte{0x50})
				Expect(err).NotTo(HaveOccurred())
				Expect(contains).To(BeTrue())

				// MissingRanges should be empty
				missing, err := rs.MissingRanges(rtx.Transaction(), []byte{0x00}, []byte{0xff}, 100)
				Expect(err).NotTo(HaveOccurred())
				Expect(missing).To(BeEmpty())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Go writes partial range, Java reads gaps", func() {
		It("should see the correct missing range via Java", func() {
			// Go inserts [0x00, 0x50)
			_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				rs := recordlayer.NewRangeSet(rsSubspace)
				_, err := rs.InsertRange(rtx.Transaction(), []byte{0x00}, []byte{0x50}, false)
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Java: contains(0x20) should be true (inside range)
			params := buildJavaParams()
			params["key"] = BytesToIntArray([]byte{0x20})
			var contains bool
			err = java.InvokeAs(ctx, "rangeSetContains", params, &contains)
			Expect(err).NotTo(HaveOccurred())
			Expect(contains).To(BeTrue())

			// Java: contains(0x80) should be false (outside range)
			params = buildJavaParams()
			params["key"] = BytesToIntArray([]byte{0x80})
			err = java.InvokeAs(ctx, "rangeSetContains", params, &contains)
			Expect(err).NotTo(HaveOccurred())
			Expect(contains).To(BeFalse())

			// Java: missingRanges should be [0x50, 0xFF)
			params = buildJavaParams()
			var missing []map[string]any
			err = java.InvokeAs(ctx, "rangeSetMissingRanges", params, &missing)
			Expect(err).NotTo(HaveOccurred())
			Expect(missing).To(HaveLen(1))

			beginInts := missing[0]["begin"].([]any)
			endInts := missing[0]["end"].([]any)
			Expect(intSliceToBytes(beginInts)).To(Equal([]byte{0x50}))
			Expect(intSliceToBytes(endInts)).To(Equal([]byte{0xff}))
		})
	})

	Describe("Java writes partial range, Go reads gaps", func() {
		It("should see the correct missing range via Go", func() {
			// Java inserts [0x00, 0x50)
			params := buildJavaParams()
			params["begin"] = BytesToIntArray([]byte{0x00})
			params["end"] = BytesToIntArray([]byte{0x50})
			var inserted bool
			err := java.InvokeAs(ctx, "rangeSetInsert", params, &inserted)
			Expect(err).NotTo(HaveOccurred())
			Expect(inserted).To(BeTrue())

			// Go reads
			_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				rs := recordlayer.NewRangeSet(rsSubspace)

				// Contains(0x20) = true
				contains, err := rs.Contains(rtx.Transaction(), []byte{0x20})
				Expect(err).NotTo(HaveOccurred())
				Expect(contains).To(BeTrue())

				// Contains(0x80) = false
				contains, err = rs.Contains(rtx.Transaction(), []byte{0x80})
				Expect(err).NotTo(HaveOccurred())
				Expect(contains).To(BeFalse())

				// MissingRanges = [[0x50, 0xFF)]
				missing, err := rs.MissingRanges(rtx.Transaction(), []byte{0x00}, []byte{0xff}, 100)
				Expect(err).NotTo(HaveOccurred())
				Expect(missing).To(HaveLen(1))
				Expect(missing[0].Begin).To(Equal([]byte{0x50}))
				Expect(missing[0].End).To(Equal([]byte{0xff}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

// intSliceToBytes converts a JSON number array to a byte slice.
func intSliceToBytes(ints []any) []byte {
	result := make([]byte, len(ints))
	for i, v := range ints {
		switch n := v.(type) {
		case float64:
			result[i] = byte(n)
		case int64:
			result[i] = byte(n)
		case int:
			result[i] = byte(n)
		}
	}
	return result
}
