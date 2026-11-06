package recordlayer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("TEXT prefix child limits", func() {
	ctx := context.Background()
	It("returns malformed comparison keys as errors and leaves real TEXT scans caller-owned", func() {
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		b := baseBuilder()
		idx := NewTextIndex("prefix_key_error", Field("name"))
		b.AddIndex("Customer", idx)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			return store.SaveRecord(&demopb.Customer{CustomerId: proto.Int64(42), Name: proto.String("alpha beta")})
		})
		Expect(err).NotTo(HaveOccurred())
		for _, invalid := range []bool{true, false} {
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Open()
				if err != nil {
					return nil, err
				}
				var children []RecordCursor[*IndexEntry]
				var factories []CursorFunction[*IndexEntry]
				for _, token := range []string{"alpha", "beta"} {
					factories = append(factories, func(cont []byte) RecordCursor[*IndexEntry] {
						child := store.ScanIndexByType(idx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{token}), cont, ForwardScan())
						children = append(children, child)
						return child
					})
				}
				keyCalls := 0
				cur, err := ProbableIntersection(func(entry *IndexEntry) (tuple.Tuple, error) {
					keyCalls++
					Expect(entry.Key[1:]).To(Equal(tuple.Tuple{int64(42)}))
					if invalid {
						return tuple.Tuple{entry.Key[1:], struct{}{}}, nil
					}
					return entry.Key[1:], nil
				}, factories, 500, 0.01, nil)
				if err != nil {
					return nil, err
				}
				defer cur.Close()
				result, nextErr := cur.OnNext(ctx)
				if invalid {
					var argument *RecordCoreArgumentError
					Expect(errors.As(nextErr, &argument)).To(BeTrue(), "%v", nextErr)
					Expect(argument.Message).To(ContainSubstring("child 0 comparison key"))
					Expect(argument.Cause).NotTo(BeNil())
					Expect(result.HasNext()).To(BeFalse())
					Expect(keyCalls).To(Equal(1))
				} else {
					Expect(nextErr).NotTo(HaveOccurred())
					Expect(result.HasNext()).To(BeTrue())
					Expect(result.GetValue().Key).To(Equal(tuple.Tuple{"beta", int64(42)}))
					Expect(keyCalls).To(Equal(2))
				}
				Expect(children).To(HaveLen(2))
				for _, child := range children {
					Expect(child.IsClosed()).To(BeFalse())
				}
				Expect(cur.Close()).To(Succeed())
				Expect(cur.Close()).To(Succeed())
				for _, child := range children {
					Expect(child.IsClosed()).To(BeTrue())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		}
	})

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
