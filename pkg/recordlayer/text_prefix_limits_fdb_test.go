package recordlayer

import (
	"context"
	"fmt"
	"slices"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("TEXT prefix child limits", func() {
	ctx := context.Background()
	It("resumes ANY_PREFIX and ALL_PREFIXES after real index scan and byte stops", func() {
		b := baseBuilder()
		idx := NewTextIndex("prefix_stop", Field("name"))
		b.AddIndex("Customer", idx)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			s, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for i := range 100 {
				if _, err := s.SaveRecord(&demopb.Customer{CustomerId: proto.Int64(int64(i)), Name: proto.String("alpha beta")}); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		for _, cmp := range []*TextComparison{NewTextComparison(TextContainsAnyPrefix, "al be", "", ""), NewTextContainsAllPrefixesComparison("al be", false, 500, 1e-9, "", "")} {
			read := func(limited bool, bytes bool) ([]string, int) {
				var continuation []byte
				var result []string
				stops := 0
				for page := 0; page < 1000; page++ {
					type output struct {
						keys   []string
						cont   []byte
						reason NoNextReason
					}
					v, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
						s, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Open()
						if err != nil {
							return nil, err
						}
						exec := DefaultExecuteProperties()
						if limited {
							if bytes {
								exec.ScannedBytesLimit = 1
							} else {
								exec.ScannedRecordsLimit = 1
							}
						}
						cur, err := NewTextScan(idx, nil, cmp, nil).Scan(s, continuation, NewScanProperties(exec))
						if err != nil {
							return nil, err
						}
						defer cur.Close()
						var out output
						for {
							r, err := cur.OnNext(ctx)
							if err != nil {
								return nil, err
							}
							if !r.HasNext() {
								out.reason = r.GetNoNextReason()
								out.cont, err = r.GetContinuation().ToBytes()
								return out, err
							}
							out.keys = append(out.keys, string(tuple.Tuple(r.GetValue().Key[1:]).Pack()))
						}
					})
					Expect(err).NotTo(HaveOccurred())
					out := v.(output)
					result = append(result, out.keys...)
					continuation = out.cont
					if continuation == nil {
						Expect(out.reason).To(Equal(SourceExhausted))
						slices.Sort(result)
						return result, stops
					}
					stops++
					want := ScanLimitReached
					if bytes {
						want = ByteLimitReached
					}
					Expect(out.reason).To(Equal(want))
				}
				Fail(fmt.Sprintf("%s did not resume to exhaustion", cmp))
				return nil, stops
			}
			want, _ := read(false, false)
			Expect(want).NotTo(BeEmpty())
			for _, bytes := range []bool{false, true} {
				got, stops := read(true, bytes)
				Expect(stops).To(BeNumerically(">", 0))
				Expect(got).To(Equal(want))
			}
		}
	})
})
