package recordlayer

import (
	"context"
	"fmt"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("INDEX_TO_RECORD pipeline sizing", func() {
	ctx := context.Background()
	It("uses the custom depth on each core fetch path before waiting", func() {
		b := baseBuilder()
		idx := NewIndex("fetch_sizer", Field("price"))
		b.AddIndex("Customer", idx)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			s, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for i := range 8 {
				if _, err := s.SaveRecord(&demopb.Customer{CustomerId: proto.Int64(int64(i)), Price: proto.Int32(int32(i))}); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		for _, depth := range []int{1, 4} {
			for _, api := range []string{"fetch", "scan", "scan-by-type"} {
				_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
					calls := 0
					sizer := func(op PipelineOperation) int {
						if op == PipelineOperationIndexToRecord {
							calls++
							return depth
						}
						return DefaultPipelineSizer(op)
					}
					s, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).SetPipelineSizer(sizer).Open()
					if err != nil {
						return nil, err
					}
					var cur RecordCursor[*FDBIndexedRecord]
					switch api {
					case "fetch":
						cur = s.FetchIndexRecords(s.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()), IndexOrphanError, nil)
					case "scan":
						cur = s.ScanIndexRecords(idx.Name, TupleRangeAll, nil, ForwardScan())
					default:
						cur = s.scanIndexRecordsByType(idx, IndexScanByValue, TupleRangeAll, nil, IndexOrphanError, ForwardScan())
					}
					defer cur.Close()
					if calls != 1 {
						return nil, fmt.Errorf("%s did not consult INDEX_TO_RECORD: %d calls", api, calls)
					}
					pipeline, ok := cur.(*mapPipelinedCursor[*IndexEntry, *FDBIndexedRecord])
					if !ok {
						return nil, &RecordCoreError{Message: "unexpected fetch pipeline"}
					}
					if pipeline.size != depth {
						return nil, fmt.Errorf("%s depth%d,want%d", api, pipeline.size, depth)
					}
					issued, gets := 0, 0
					issue := pipeline.issue
					pipeline.issue = func(entry *IndexEntry) PendingRead[*FDBIndexedRecord] {
						issued++
						pending := issue(entry)
						return NewPendingRead(func() (*FDBIndexedRecord, error) {
							if gets == 0 && issued != depth {
								return nil, fmt.Errorf("%s issued%d before first wait,want%d", api, issued, depth)
							}
							gets++
							return pending.Get()
						}, func() bool { return false })
					}
					for want := range 8 {
						r, err := cur.OnNext(ctx)
						if err != nil {
							return nil, err
						}
						if !r.HasNext() || compareTuples(r.GetValue().Record.PrimaryKey, tuple.Tuple{int64(want)}) != 0 {
							return nil, fmt.Errorf("%s lost row%d", api, want)
						}
					}
					end, err := cur.OnNext(ctx)
					if err != nil || end.HasNext() {
						return nil, fmt.Errorf("%s exhaustion: %v", api, err)
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			}
		}
	})
})
