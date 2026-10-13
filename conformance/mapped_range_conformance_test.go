//go:build bazelrunfiles

package conformance_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type mappedConformanceKV struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type mappedConformanceRow struct {
	Key       string                `json:"key"`
	Value     string                `json:"value"`
	Begin     string                `json:"begin"`
	End       string                `json:"end"`
	Secondary []mappedConformanceKV `json:"secondary"`
}

type mappedConformanceBatch struct {
	Iteration int  `json:"iteration"`
	Requested int  `json:"requested"`
	Returned  int  `json:"returned"`
	More      bool `json:"more"`
}

type mappedConformanceFixture struct {
	Mapper    []byte                 `json:"mapper"`
	Begin     []byte                 `json:"begin"`
	End       []byte                 `json:"end"`
	UnreadKey []byte                 `json:"unreadKey"`
	Rows      []mappedConformanceRow `json:"rows"`
}

type mappedConformanceScan struct {
	Rows      []mappedConformanceRow   `json:"rows"`
	BatchRows []mappedConformanceRow   `json:"batchRows"`
	Batches   []mappedConformanceBatch `json:"batches"`
}

func mappedConformanceBytes(value []byte) string {
	return base64.StdEncoding.EncodeToString(value)
}

func mappedConformanceConvert(row fdb.MappedKeyValue) mappedConformanceRow {
	secondary := make([]mappedConformanceKV, len(row.GetRange.Rows))
	for i, kv := range row.GetRange.Rows {
		secondary[i] = mappedConformanceKV{Key: mappedConformanceBytes(kv.Key), Value: mappedConformanceBytes(kv.Value)}
	}
	return mappedConformanceRow{
		Key: mappedConformanceBytes(row.Key), Value: mappedConformanceBytes(row.Value),
		Begin: mappedConformanceBytes(row.GetRange.Begin.Key), End: mappedConformanceBytes(row.GetRange.End.Key),
		Secondary: secondary,
	}
}

// TestJavaEquivalent: IndexPrefetchRangeKeyValueCursor.Builder.build and the
// RemoteFetch* forward/reverse, split-record, compound-key, limit and RYW cases.
// This pins the raw facade only: no fetch-method dispatch or record reconstruction.
var _ = Describe("Mapped range facade Java conformance (RFC-258 PR1)", func() {
	const count = 64
	var (
		ctx     context.Context
		env     *JavaTenant
		tenant  fdb.Tenant
		java    *JavaInvoker
		root    string
		fixture mappedConformanceFixture
	)

	BeforeEach(func() {
		ctx = context.Background()
		root = "mapped_" + uuid.NewString()
		var err error
		env, err = SetupJavaTenant(ctx, sharedContainer, root)
		Expect(err).NotTo(HaveOccurred())
		tenant, err = sharedDB.OpenTenant(fdb.Key(env.TenantName))
		Expect(err).NotTo(HaveOccurred())
		java = NewJavaInvoker()
		fixture = mappedConformanceFixture{}
		Expect(java.InvokeAs(ctx, "seedMappedRangeFixture", map[string]any{
			"clusterFile": env.ClusterFile, "tenantName": env.TenantName, "root": root, "count": count,
		}, &fixture)).To(Succeed())
		Expect(fixture.Rows).To(HaveLen(count))
		Expect(fixture.Mapper).To(Equal(tuple.Tuple{root, "records", "{K[3]}", "{K[2]}", "{...}"}.Pack()))
		begin, end := tuple.Tuple{root, "index"}.FDBRangeKeys()
		Expect(fixture.Begin).To(Equal([]byte(begin.FDBKey())))
		Expect(fixture.End).To(Equal([]byte(end.FDBKey())))
		// Non-vacuity: the Java writer really seeded a split, a version and an orphan.
		Expect(fixture.Rows[17].Secondary).To(HaveLen(2))
		chunk, err := base64.StdEncoding.DecodeString(fixture.Rows[17].Secondary[0].Value)
		Expect(err).NotTo(HaveOccurred())
		Expect(chunk).To(HaveLen(100_000))
		Expect(fixture.Rows[0].Secondary).To(HaveLen(2))
		Expect(fixture.Rows[count-1].Secondary).To(BeEmpty())
	})

	AfterEach(func() {
		if env != nil {
			Expect(env.Cleanup(ctx)).To(Succeed())
		}
	})

	for _, mode := range []struct {
		name string
		mode fdb.StreamingMode
	}{
		{"SMALL", fdb.StreamingModeSmall},
		{"ITERATOR", fdb.StreamingModeIterator},
		{"MEDIUM", fdb.StreamingModeMedium},
		{"LARGE", fdb.StreamingModeLarge},
		{"SERIAL", fdb.StreamingModeSerial},
		{"WANT_ALL", fdb.StreamingModeWantAll},
		{"EXACT", fdb.StreamingModeExact},
	} {
		for _, reverse := range []bool{false, true} {
			It(fmt.Sprintf("matches %s reverse=%v rows, boundaries and remaining row budgets", mode.name, reverse), func() {
				limits := []int{1, 19, 0, count + 1}
				if mode.mode == fdb.StreamingModeExact {
					limits[2] = count // EXACT requires a nonzero row limit in Java.
				}
				for _, limit := range limits {
					label := fmt.Sprintf("%s reverse=%v limit=%d", mode.name, reverse, limit)
					var oracle mappedConformanceScan
					Expect(java.InvokeAs(ctx, "scanMappedRangeFixture", map[string]any{
						"clusterFile": env.ClusterFile, "tenantName": env.TenantName, "root": root,
						"mode": mode.name, "limit": limit, "reverse": reverse,
					}, &oracle)).To(Succeed(), label)
					want := append([]mappedConformanceRow(nil), fixture.Rows...)
					if reverse {
						for i, j := 0, len(want)-1; i < j; i, j = i+1, j-1 {
							want[i], want[j] = want[j], want[i]
						}
					}
					if limit > 0 && limit < len(want) {
						want = want[:limit]
					}
					Expect(oracle.Rows).To(Equal(want), label+" Java public iterator vs writer")
					Expect(oracle.BatchRows).To(Equal(oracle.Rows), label+" Java single batches vs public iterator")
					Expect(oracle.Batches).NotTo(BeEmpty(), label)

					transaction, err := tenant.CreateTransaction()
					Expect(err).NotTo(HaveOccurred())
					func() {
						defer transaction.Cancel()
						reader, ok := any(transaction).(fdb.MappedRangeReader)
						Expect(ok).To(BeTrue(), "pure-Go transaction must expose mapped capability")
						iterator := reader.GetMappedRange(fdb.KeyRange{Begin: fdb.Key(fixture.Begin), End: fdb.Key(fixture.End)}, fixture.Mapper,
							fdb.RangeOptions{Mode: mode.mode, Limit: limit, Reverse: reverse}).Iterator()
						var batches []mappedConformanceBatch
						iterator.SetTraceLog(func(iteration, requested, returned int, more bool, err error) {
							Expect(err).NotTo(HaveOccurred(), label)
							batches = append(batches, mappedConformanceBatch{iteration, requested, returned, more})
						})
						var rows []mappedConformanceRow
						for iterator.Advance() {
							row, err := iterator.Get()
							Expect(err).NotTo(HaveOccurred(), label)
							Expect(row.Kind).To(Equal(fdb.MappedResultGetRange), label)
							// The secondary range stays forward even for reverse primary scans.
							for i := 1; i < len(row.GetRange.Rows); i++ {
								Expect(bytes.Compare(row.GetRange.Rows[i-1].Key, row.GetRange.Rows[i].Key)).To(BeNumerically("<", 0), label)
							}
							Expect(row.GetRange.More).To(BeFalse(), "fixture secondary range must not be truncated")
							rows = append(rows, mappedConformanceConvert(row))
						}
						_, err = iterator.Get()
						Expect(err).NotTo(HaveOccurred(), label+" terminal iterator error")
						Expect(rows).To(Equal(oracle.Rows), label+" primary and secondary bytes")
						if limit == 0 {
							// Go represents the unlimited row budget as MaxInt32; Java passes 0.
							remaining := math.MaxInt32
							for i := range oracle.Batches {
								oracle.Batches[i].Requested = remaining
								remaining -= oracle.Batches[i].Returned
							}
						}
						Expect(batches).To(Equal(oracle.Batches), label+" per-batch division/more/budget")
						if limit == 0 && (mode.mode == fdb.StreamingModeSmall || mode.mode == fdb.StreamingModeIterator) {
							Expect(len(batches)).To(BeNumerically(">", 1), label+" must cross a batch boundary")
						}
						GinkgoWriter.Printf("%s: %d rows in %d Java/Go batches\n", label, len(rows), len(batches))
					}()
				}
			})
		}
	}

	It("matches truncated secondary rows and permits local writes to their unread tail in both directions", func() {
		const secondaryCount = 2500
		truncatedRoot := root + "_truncated"
		var truncated mappedConformanceFixture
		Expect(java.InvokeAs(ctx, "seedTruncatedMappedRangeFixture", map[string]any{
			"clusterFile": env.ClusterFile, "tenantName": env.TenantName, "root": truncatedRoot,
			"secondaryCount": secondaryCount,
		}, &truncated)).To(Succeed())
		Expect(truncated.Rows).To(HaveLen(1))
		Expect(truncated.Rows[0].Secondary).To(HaveLen(secondaryCount))
		Expect(truncated.Mapper).To(Equal(tuple.Tuple{truncatedRoot, "records", "{K[3]}", "{K[2]}", "{...}"}.Pack()))
		Expect(mappedConformanceBytes(truncated.UnreadKey)).To(Equal(truncated.Rows[0].Secondary[secondaryCount-1].Key))
		Expect(truncated.UnreadKey).To(Equal(tuple.Tuple{truncatedRoot, "records", "group\x000", int64(0), int64(secondaryCount - 1)}.Pack()))

		for _, reverse := range []bool{false, true} {
			var control []mappedConformanceRow
			for _, writeTail := range []bool{false, true} {
				label := fmt.Sprintf("truncated secondary reverse=%v local-write-tail=%v", reverse, writeTail)
				params := map[string]any{
					"clusterFile": env.ClusterFile, "tenantName": env.TenantName, "root": truncatedRoot,
					"mode": "ITERATOR", "limit": 1, "reverse": reverse,
				}
				if writeTail {
					params["localWriteKey"] = BytesToIntArray(truncated.UnreadKey)
				}
				var oracle mappedConformanceScan
				Expect(java.InvokeAs(ctx, "scanMappedRangeFixture", params, &oracle)).To(Succeed(), label+" Java must not raise 2039")
				Expect(oracle.Rows).To(HaveLen(1), label)
				Expect(oracle.BatchRows).To(Equal(oracle.Rows), label)
				returned := len(oracle.Rows[0].Secondary)
				// The public Java binding drops secondary More; its short result proves truncation.
				Expect(returned).To(BeNumerically(">", 0), label)
				Expect(returned).To(BeNumerically("<", secondaryCount), label+" must truncate the secondary range")
				Expect(returned).To(Equal(2000), label+" pinned 7.3.77 QUICK_GET_KEY_VALUES_LIMIT")
				want := truncated.Rows[0]
				want.Secondary = want.Secondary[:returned]
				Expect(oracle.Rows[0]).To(Equal(want), label+" returned secondary rows must be the written forward prefix")
				if !writeTail {
					control = oracle.Rows
				} else {
					Expect(oracle.Rows).To(Equal(control), label+" unread-tail write must not change the returned prefix")
				}

				transaction, err := tenant.CreateTransaction()
				Expect(err).NotTo(HaveOccurred())
				func() {
					defer transaction.Cancel()
					if writeTail {
						transaction.Set(fdb.Key(truncated.UnreadKey), []byte{42})
					}
					reader, ok := any(transaction).(fdb.MappedRangeReader)
					Expect(ok).To(BeTrue())
					iterator := reader.GetMappedRange(fdb.KeyRange{Begin: fdb.Key(truncated.Begin), End: fdb.Key(truncated.End)}, truncated.Mapper,
						fdb.RangeOptions{Mode: fdb.StreamingModeIterator, Limit: 1, Reverse: reverse}).Iterator()
					var rows []mappedConformanceRow
					for iterator.Advance() {
						row, err := iterator.Get()
						Expect(err).NotTo(HaveOccurred(), label)
						Expect(row.Kind).To(Equal(fdb.MappedResultGetRange), label)
						Expect(row.GetRange.More).To(BeTrue(), label+" must preserve secondary truncation metadata")
						Expect(row.GetRange.Rows).To(HaveLen(returned), label)
						Expect(bytes.Compare(row.GetRange.Rows[len(row.GetRange.Rows)-1].Key, truncated.UnreadKey)).To(BeNumerically("<", 0), label)
						rows = append(rows, mappedConformanceConvert(row))
					}
					_, err = iterator.Get()
					Expect(err).NotTo(HaveOccurred(), label+" Go must not raise 2039")
					Expect(rows).To(Equal(oracle.Rows), label+" primary/secondary bytes")
				}()
			}
		}
	})

	It("propagates Java mapper and read-your-writes error codes in both directions", func() {
		for _, reverse := range []bool{false, true} {
			for _, probe := range []struct {
				name           string
				mapper         []byte
				writePrimary   bool
				writeSecondary bool
				code           int
			}{
				{"not_tuple", []byte{0xff}, false, false, 2043},
				{"bad_index", tuple.Tuple{root, "records", "{K[9]}", "{...}"}.Pack(), false, false, 2030},
				{"bad_descriptor", tuple.Tuple{root, "{...}", "records"}.Pack(), false, false, 2032},
				{"primary_ryw", fixture.Mapper, true, false, 2039},
				{"secondary_ryw", fixture.Mapper, false, true, 2039},
			} {
				label := fmt.Sprintf("%s reverse=%v", probe.name, reverse)
				var javaCode int
				Expect(java.InvokeAs(ctx, "mappedRangeErrorCode", map[string]any{
					"clusterFile": env.ClusterFile, "tenantName": env.TenantName, "root": root,
					"mapper": BytesToIntArray(probe.mapper), "writePrimary": probe.writePrimary,
					"writeSecondary": probe.writeSecondary, "reverse": reverse,
				}, &javaCode)).To(Succeed(), label)
				Expect(javaCode).To(Equal(probe.code), label+" Java positive error control")
				transaction, err := tenant.CreateTransaction()
				Expect(err).NotTo(HaveOccurred())
				func() {
					defer transaction.Cancel()
					if probe.writePrimary {
						transaction.Set(fdb.Key(tuple.Tuple{root, "index", int64(0), "group\x000"}.Pack()), []byte{42})
					}
					if probe.writeSecondary {
						transaction.Set(fdb.Key(tuple.Tuple{root, "records", "group\x000", int64(0), int64(0)}.Pack()), []byte{42})
					}
					reader, ok := any(transaction).(fdb.MappedRangeReader)
					Expect(ok).To(BeTrue())
					iterator := reader.GetMappedRange(fdb.KeyRange{Begin: fdb.Key(fixture.Begin), End: fdb.Key(fixture.End)}, probe.mapper,
						fdb.RangeOptions{Mode: fdb.StreamingModeIterator, Reverse: reverse}).Iterator()
					for iterator.Advance() {
						_, err = iterator.Get()
						Expect(err).NotTo(HaveOccurred(), label)
					}
					_, err = iterator.Get()
					Expect(err).To(HaveOccurred(), label)
					var fdbError fdb.Error
					Expect(errors.As(err, &fdbError)).To(BeTrue(), label+" must retain structured FDB error")
					Expect(fdbError.Code).To(Equal(javaCode), label)
				}()
			}
		}
	})
})
