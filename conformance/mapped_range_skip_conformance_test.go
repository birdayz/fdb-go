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

type mappedSkipConformanceScan struct {
	mappedConformanceScan
	InitialSelectors conformanceSelectors `json:"initialSelectors"`
	ErrorCode        int                  `json:"errorCode"`
}

type mappedSkipConformanceCase struct {
	modeName              string
	mode                  fdb.StreamingMode
	limit                 int
	skip                  int32
	reverse               bool
	disableReadYourWrites bool
	emptyBeginAnchor      bool
	emptyEndAnchor        bool
	beginAtFirstKey       bool
	localWriteKey         []byte
}

// This witness pins Java's signed-int selector addition, not Go's positive
// Skip contract: forward MaxInt32 deliberately differs from Java's wraparound.
func mappedSkipConformanceJavaSelectors(r fdb.KeyRange, skip int32, reverse bool) conformanceSelectors {
	beginOffset, endOffset := int32(1), int32(1)
	if skip > 0 {
		if reverse {
			endOffset = int32(1 - int64(skip))
		} else {
			beginOffset = int32(1 + int64(skip))
		}
	}
	return conformanceSelectors{
		Begin: conformanceSelector{Key: mappedConformanceBytes(r.Begin.FDBKey()), Offset: int(beginOffset)},
		End:   conformanceSelector{Key: mappedConformanceBytes(r.End.FDBKey()), Offset: int(endOffset)},
	}
}

func mappedSkipConformanceExpected(rows []mappedConformanceRow, skip int32, reverse bool, limit int) []mappedConformanceRow {
	want := append(make([]mappedConformanceRow, 0, len(rows)), rows...)
	if reverse {
		for i, j := 0, len(want)-1; i < j; i, j = i+1, j-1 {
			want[i], want[j] = want[j], want[i]
		}
	}
	start := min(len(want), int(max(skip, 0)))
	want = want[start:]
	if limit > 0 && limit < len(want) {
		want = want[:limit]
	}
	return want
}

func mappedSkipConformanceCode(err error) int {
	if err == nil {
		return 0
	}
	var native fdb.Error
	Expect(errors.As(err, &native)).To(BeTrue(), "mapped Skip must retain structured FDB error: %v", err)
	return native.Code
}

func mappedSkipConformanceGo(tenant fdb.Tenant, r fdb.KeyRange, mapper []byte, tc mappedSkipConformanceCase) mappedSkipConformanceScan {
	transaction, err := tenant.CreateTransaction()
	Expect(err).NotTo(HaveOccurred())
	defer transaction.Cancel()
	if tc.disableReadYourWrites {
		Expect(transaction.Options().SetReadYourWritesDisable()).To(Succeed())
	}
	if tc.localWriteKey != nil {
		transaction.Set(fdb.Key(tc.localWriteKey), []byte{42})
	}
	reader, ok := any(transaction).(fdb.MappedRangeSkipReader)
	Expect(ok).To(BeTrue(), "public transaction must expose signed-32-bit mapped Skip")
	iterator := reader.GetMappedRangeWithSkip(r, mapper, fdb.RangeOptions{Mode: tc.mode, Limit: tc.limit, Reverse: tc.reverse}, tc.skip).Iterator()
	result := mappedSkipConformanceScan{
		mappedConformanceScan: mappedConformanceScan{Rows: make([]mappedConformanceRow, 0), Batches: make([]mappedConformanceBatch, 0)},
	}
	var fetchErrors []error
	iterator.SetTraceLog(func(iteration, requested, returned int, more bool, err error) {
		result.Batches = append(result.Batches, mappedConformanceBatch{Iteration: iteration, Requested: requested, Returned: returned, More: more})
		if err != nil {
			fetchErrors = append(fetchErrors, err)
		}
	})
	for iterator.Advance() {
		row, err := iterator.Get()
		Expect(err).NotTo(HaveOccurred())
		Expect(row.Kind).To(Equal(fdb.MappedResultGetRange))
		Expect(row.GetRange.More).To(BeFalse(), "fixture secondary ranges must be complete")
		for i := 1; i < len(row.GetRange.Rows); i++ {
			Expect(bytes.Compare(row.GetRange.Rows[i-1].Key, row.GetRange.Rows[i].Key)).To(BeNumerically("<", 0), "secondary rows remain forward during reverse primary scans")
		}
		result.Rows = append(result.Rows, mappedConformanceConvert(row))
	}
	_, err = iterator.Get()
	result.ErrorCode = mappedSkipConformanceCode(err)
	for _, fetchError := range fetchErrors {
		Expect(mappedSkipConformanceCode(fetchError)).To(Equal(result.ErrorCode))
	}
	return result
}

// Same-cluster Java 4.14.2 public iteration and individual native fetches are
// separate witnesses: matching final rows alone can erase selector reply More.
var _ = Describe("Mapped range initial-selector Skip Java conformance", func() {
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
		root = "mapped_skip_" + uuid.NewString()
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
		Expect(fixture.Rows[17].Secondary).To(HaveLen(2), "positive split fixture")
		chunk, err := base64.StdEncoding.DecodeString(fixture.Rows[17].Secondary[0].Value)
		Expect(err).NotTo(HaveOccurred())
		Expect(chunk).To(HaveLen(100_000))
		Expect(fixture.Rows[0].Secondary).To(HaveLen(2), "positive inline-version fixture")
		Expect(fixture.Rows[count-1].Secondary).To(BeEmpty(), "positive orphan fixture")
		control := mappedSkipConformanceGo(tenant, fdb.KeyRange{Begin: fdb.Key(fixture.Begin), End: fdb.Key(fixture.End)}, fixture.Mapper,
			mappedSkipConformanceCase{modeName: "WANT_ALL", mode: fdb.StreamingModeWantAll})
		Expect(control.ErrorCode).To(BeZero())
		Expect(control.Rows).To(Equal(fixture.Rows), "Go prime must return all 64 Java-written primary/secondary rows")
		Expect(control.Batches).NotTo(BeEmpty())
		returned := 0
		for _, batch := range control.Batches {
			returned += batch.Returned
		}
		Expect(returned).To(Equal(count), "trace population must equal prime rows")
		transaction, err := tenant.CreateTransaction()
		Expect(err).NotTo(HaveOccurred())
		defer transaction.Cancel()
		_, mappedSnapshot := any(transaction.Snapshot()).(fdb.MappedRangeReader)
		_, skipSnapshot := any(transaction.Snapshot()).(fdb.MappedRangeSkipReader)
		Expect(mappedSnapshot).To(BeFalse(), "snapshot must not advertise serializable mapped reads")
		Expect(skipSnapshot).To(BeFalse(), "snapshot must not advertise mapped Skip")
	})

	AfterEach(func() {
		if env != nil {
			Expect(env.Cleanup(ctx)).To(Succeed())
		}
	})

	check := func(tc mappedSkipConformanceCase, want []mappedConformanceRow, code int) (mappedSkipConformanceScan, mappedSkipConformanceScan) {
		label := fmt.Sprintf("mode=%s reverse=%v skip=%d limit=%d", tc.modeName, tc.reverse, tc.skip, tc.limit)
		r := fdb.KeyRange{Begin: fdb.Key(fixture.Begin), End: fdb.Key(fixture.End)}
		if tc.beginAtFirstKey {
			r.Begin = fdb.Key(tuple.Tuple{root, "index", int64(0), "group\x000"}.Pack())
			Expect(mappedConformanceBytes(r.Begin.FDBKey())).To(Equal(fixture.Rows[0].Key))
		}
		if tc.emptyBeginAnchor {
			r.Begin = fdb.Key{}
		}
		if tc.emptyEndAnchor {
			r.End = fdb.Key{}
		}
		params := map[string]any{
			"clusterFile": env.ClusterFile, "tenantName": env.TenantName, "root": root,
			"mode": tc.modeName, "limit": tc.limit, "reverse": tc.reverse, "skip": tc.skip,
		}
		for name, enabled := range map[string]bool{
			"disableReadYourWrites": tc.disableReadYourWrites, "emptyBeginAnchor": tc.emptyBeginAnchor,
			"emptyEndAnchor": tc.emptyEndAnchor, "beginAtFirstKey": tc.beginAtFirstKey,
		} {
			if enabled {
				params[name] = true
			}
		}
		if tc.localWriteKey != nil {
			params["localWriteKey"] = BytesToIntArray(tc.localWriteKey)
		}
		var oracle mappedSkipConformanceScan
		Expect(java.InvokeAs(ctx, "scanMappedRangeWithSkip", params, &oracle)).To(Succeed(), label)
		Expect(oracle.InitialSelectors).To(Equal(mappedSkipConformanceJavaSelectors(r, tc.skip, tc.reverse)), label+" Java initial native selector witness")
		Expect(oracle.ErrorCode).To(Equal(code), label+" Java positive error/success control")
		actual := mappedSkipConformanceGo(tenant, r, fixture.Mapper, tc)
		goCode := code
		forwardMax := !tc.reverse && tc.skip == math.MaxInt32
		if forwardMax && tc.emptyEndAnchor {
			goCode = 0 // Original inverted bounds remain empty without wrapping.
		}
		Expect(actual.ErrorCode).To(Equal(goCode), label+" structured Go error code")
		if code != 0 {
			Expect(oracle.Rows).To(BeEmpty(), label+" Java initial-fetch error")
		} else {
			Expect(oracle.Rows).To(Equal(want), label+" Java iterator vs written bytes")
			Expect(oracle.BatchRows).To(Equal(oracle.Rows), label+" Java single native fetches vs iterator")
			Expect(oracle.Batches).NotTo(BeEmpty(), label+" actual native fetch population")
		}
		if goCode != 0 {
			// Initial-fetch failures must not return partial rows.
			Expect(actual.Rows).To(BeEmpty(), label+" early error must not return partial rows")
			return oracle, actual
		}
		if forwardMax {
			Expect(actual.Rows).To(BeEmpty(), label+" positive MaxInt32 must never wrap toward earlier keys")
			Expect(actual.Batches).To(Equal([]mappedConformanceBatch{{Iteration: 1, Requested: fdb.EffectiveRowLimit(tc.limit)}}), label+" nonwrapping Skip exhausted in one fetch")
			return oracle, actual
		}
		Expect(actual.Rows).To(Equal(oracle.Rows), label+" primary/secondary bytes")
		batches := append([]mappedConformanceBatch(nil), oracle.Batches...)
		if tc.limit == 0 {
			// Java names unlimited as zero; the Go facade decrements MaxInt32.
			remaining := math.MaxInt32
			for i := range batches {
				batches[i].Requested = remaining
				remaining -= batches[i].Returned
			}
		}
		Expect(actual.Batches).To(Equal(batches), label+" every fetch iteration/remaining/returned/More")
		GinkgoWriter.Printf("%s: %d rows in %d Java/Go fetches\n", label, len(actual.Rows), len(actual.Batches))
		return oracle, actual
	}

	for _, mode := range []struct {
		name string
		mode fdb.StreamingMode
	}{
		{"ITERATOR", fdb.StreamingModeIterator},
		{"SMALL", fdb.StreamingModeSmall},
		{"MEDIUM", fdb.StreamingModeMedium},
		{"LARGE", fdb.StreamingModeLarge},
		{"SERIAL", fdb.StreamingModeSerial},
		{"WANT_ALL", fdb.StreamingModeWantAll},
		{"EXACT", fdb.StreamingModeExact},
	} {
		for _, reverse := range []bool{false, true} {
			It(fmt.Sprintf("strictly matches %s reverse=%v across signed Skip and rotating row budgets", mode.name, reverse), func() {
				completed, positive, empty := 0, 0, 0
				// Tenant-scoped native reads bypass the raw KeyRangeRef tenant
				// check, so resolved-inverted overrun witnesses are valid here.
				for _, profile := range []struct {
					skip  int32
					limit int
				}{
					{-100, 0}, {-1, 1}, {0, 17}, {1, count + 1}, {7, 0}, {count, 17}, {count + 3, 1},
				} {
					limit := profile.limit
					if mode.mode == fdb.StreamingModeExact && limit == 0 {
						limit = count + 1 // EXACT zero is a separate preflight-error probe.
					}
					tc := mappedSkipConformanceCase{modeName: mode.name, mode: mode.mode, reverse: reverse, skip: profile.skip, limit: limit}
					want := mappedSkipConformanceExpected(fixture.Rows, profile.skip, reverse, limit)
					_, actual := check(tc, want, 0)
					if len(want) > 0 {
						positive++
					} else {
						empty++
					}
					if profile.skip == -100 && limit == 0 && (mode.mode == fdb.StreamingModeSmall || mode.mode == fdb.StreamingModeIterator) {
						Expect(len(actual.Batches)).To(BeNumerically(">", 1), "unlimited positive control must cross a batch boundary")
					}
					completed++
				}
				Expect(completed).To(Equal(7))
				Expect(positive).To(Equal(5))
				Expect(empty).To(Equal(2))
			})
		}
	}

	It("does not map skipped secondary writes but observes skipped primary writes even after resolving an empty range", func() {
		completed := 0
		for _, reverse := range []bool{false, true} {
			id := int64(0)
			if reverse {
				id = count - 1
			}
			primary := tuple.Tuple{root, "index", id, fmt.Sprintf("group\x00%d", id%3)}.Pack()
			secondary := tuple.Tuple{root, "records", fmt.Sprintf("group\x00%d", id%3), id, int64(0)}.Pack()
			for _, skip := range []int32{0, 1, count + 3} {
				for _, writeSecondary := range []bool{false, true} {
					tc := mappedSkipConformanceCase{modeName: "WANT_ALL", mode: fdb.StreamingModeWantAll, reverse: reverse, skip: skip, limit: 17, localWriteKey: primary}
					code := 2039
					if writeSecondary {
						tc.localWriteKey = secondary
						if skip > 0 {
							code = 0
						}
					}
					if skip == 0 {
						tc.limit = 1 // Force the write into the first fetch, not a later error.
					}
					check(tc, mappedSkipConformanceExpected(fixture.Rows, skip, reverse, tc.limit), code)
					completed++
				}
			}
		}
		Expect(completed).To(Equal(12), "both primary/secondary mutations and resolved-empty controls must execute")
	})

	It("preserves reverse selector reply More and performs the terminal empty fetch", func() {
		key, err := base64.StdEncoding.DecodeString(fixture.Rows[0].Key)
		Expect(err).NotTo(HaveOccurred())
		value, err := base64.StdEncoding.DecodeString(fixture.Rows[0].Value)
		Expect(err).NotTo(HaveOccurred())
		// SMALL is 256 bytes; SS reserves floor(256*0.2)=51 for primary
		// prefetch, while NativeAPI decrements only primary bytes plus eight.
		Expect(len(key) + len(value)).To(BeNumerically(">", 51))
		Expect(len(key) + len(value) + 8).To(BeNumerically("<", 256))
		completed := 0
		for _, limit := range []int{0, 1000} {
			oracle, actual := check(mappedSkipConformanceCase{
				modeName: "SMALL", mode: fdb.StreamingModeSmall, limit: limit,
				skip: count - 1, reverse: true, beginAtFirstKey: true,
			}, fixture.Rows[:1], 0)
			Expect(oracle.InitialSelectors.End.Offset).To(Equal(-62))
			Expect(oracle.Batches).To(HaveLen(2), "actual native batches must exercise the reply-More branch")
			Expect(oracle.Batches[0].Returned).To(Equal(1))
			Expect(oracle.Batches[0].More).To(BeTrue(), "first native fetch must preserve storage More")
			Expect(oracle.Batches[1].Returned).To(BeZero())
			Expect(oracle.Batches[1].More).To(BeFalse())
			Expect(actual.Batches).To(HaveLen(2))
			completed++
		}
		Expect(completed).To(Equal(2))
	})

	It("pins nonwrapping Go maximum-Skip against Java's empty-anchor wraparound witnesses", func() {
		all := mappedSkipConformanceExpected(fixture.Rows, 0, false, 0)
		empty := make([]mappedConformanceRow, 0)
		beforeBegin := tuple.Tuple{root, "before_index"}.Pack()
		Expect(bytes.Compare(beforeBegin, fixture.Begin)).To(BeNumerically("<", 0))
		completed := 0
		for _, tc := range []mappedSkipConformanceCase{
			{modeName: "WANT_ALL", mode: fdb.StreamingModeWantAll, skip: math.MaxInt32, emptyBeginAnchor: true},
			{modeName: "WANT_ALL", mode: fdb.StreamingModeWantAll, skip: math.MaxInt32, emptyEndAnchor: true},
			{modeName: "WANT_ALL", mode: fdb.StreamingModeWantAll, skip: math.MaxInt32, emptyEndAnchor: true, reverse: true},
			{modeName: "WANT_ALL", mode: fdb.StreamingModeWantAll, skip: math.MaxInt32, emptyEndAnchor: true, localWriteKey: beforeBegin},
			{modeName: "WANT_ALL", mode: fdb.StreamingModeWantAll, skip: math.MaxInt32, emptyBeginAnchor: true, localWriteKey: beforeBegin},
		} {
			want, code := empty, 0
			if tc.emptyBeginAnchor {
				want = all
			}
			if tc.localWriteKey != nil {
				code = 2039
			}
			oracle, _ := check(tc, want, code)
			if tc.reverse {
				Expect(oracle.InitialSelectors.Begin.Offset).To(Equal(1))
				Expect(oracle.InitialSelectors.End.Offset).To(Equal(-2147483646))
			} else {
				Expect(oracle.InitialSelectors.Begin.Offset).To(Equal(-2147483648))
				Expect(oracle.InitialSelectors.End.Offset).To(Equal(1))
			}
			if code == 0 && tc.emptyEndAnchor {
				Expect(oracle.Batches).To(HaveLen(1))
				Expect(oracle.Batches[0].Returned).To(BeZero())
				Expect(oracle.Batches[0].More).To(BeFalse())
			}
			completed++
		}
		Expect(completed).To(Equal(5))
	})

	It("reports EXACT and disabled-RYW preflight errors before selector resolution", func() {
		completed := 0
		for _, reverse := range []bool{false, true} {
			check(mappedSkipConformanceCase{modeName: "EXACT", mode: fdb.StreamingModeExact, skip: 1, reverse: reverse}, nil, 2210)
			completed++
			for _, skip := range []int32{1, count + 3, math.MaxInt32} {
				// Disabled RYW rejects before native extreme-offset resolution.
				check(mappedSkipConformanceCase{
					modeName: "SMALL", mode: fdb.StreamingModeSmall, limit: 1,
					skip: skip, reverse: reverse, disableReadYourWrites: true,
				}, nil, 2108)
				completed++
			}
		}
		// Java's forward MaxInt32 wraps and defeats original-bound inversion.
		// Go keeps positive Skip and returns empty before disabled-RYW rejection.
		for _, reverse := range []bool{false, true} {
			for _, skip := range []int32{1, math.MaxInt32} {
				code := 0
				if !reverse && skip == math.MaxInt32 {
					code = 2108
				}
				oracle, _ := check(mappedSkipConformanceCase{
					modeName: "SMALL", mode: fdb.StreamingModeSmall, limit: 1,
					skip: skip, reverse: reverse, disableReadYourWrites: true, emptyEndAnchor: true,
					localWriteKey: tuple.Tuple{root, "before_index"}.Pack(),
				}, make([]mappedConformanceRow, 0), code)
				if code == 0 {
					Expect(oracle.Batches).To(HaveLen(1))
					Expect(oracle.Batches[0].Returned).To(BeZero())
					Expect(oracle.Batches[0].More).To(BeFalse())
				}
				completed++
			}
		}
		Expect(completed).To(Equal(12))
	})
})
