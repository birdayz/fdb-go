package recordlayer

import (
	"bytes"
	"context"
	"time"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

type sparseTextRead struct {
	begin       fdb.KeySelector
	limit       int
	maintenance bool
	drained     bool
	rows        []fdb.KeyValue
	err         error
}

type sparseTextObservation struct {
	prefix      []byte
	maintenance bool
	reads       []*sparseTextRead
	secondGate  *snapshotReadCounter
}

type sparseTextTransaction struct {
	fdb.WritableTransaction
	observation *sparseTextObservation
}

func (t *sparseTextTransaction) Snapshot() fdb.ReadTransaction {
	return &sparseTextSnapshot{ReadTransaction: t.WritableTransaction.Snapshot(), observation: t.observation}
}

type sparseTextSnapshot struct {
	fdb.ReadTransaction
	observation *sparseTextObservation
}

func (s *sparseTextSnapshot) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	begin, _ := r.FDBRangeKeySelectors()
	selector := begin.FDBKeySelector()
	o := s.observation
	if !bytes.HasPrefix(selector.Key.FDBKey(), o.prefix) {
		return s.ReadTransaction.GetRange(r, options)
	}
	read := &sparseTextRead{begin: selector, limit: options.Limit, maintenance: o.maintenance}
	o.reads = append(o.reads, read)
	reader := s.ReadTransaction
	if !o.maintenance && options.Limit == 0 {
		reader = &inFlightSnapshot{ReadTransaction: reader, c: o.secondGate}
	}
	return &sparseTextRangeResult{RangeResult: reader.GetRange(r, options), read: read}
}

type sparseTextRangeResult struct {
	fdb.RangeResult
	read *sparseTextRead
}

func (r *sparseTextRangeResult) GetSliceWithError() ([]fdb.KeyValue, error) {
	rows, err := r.RangeResult.GetSliceWithError()
	r.read.drained, r.read.rows, r.read.err = true, rows, err
	return rows, err
}

var _ = Describe("Sparse TEXT bunch prefetch", func() {
	It("drains a truncated map's second wave and warms all subsequent maintenance reads", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		index := NewTextIndex("sparse_prefetch_text", Field("name"))
		builder := baseBuilder()
		builder.AddIndex("Customer", index)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		mapSpace := root.Sub(IndexKey, index.SubspaceTupleKey(), "shared")
		customer := func(id int64) proto.Message {
			return &demopb.Customer{CustomerId: proto.Int64(id), Name: proto.String("shared")}
		}
		// Sixty full bunches separate three requested keys; the first wave is
		// capped at eight rows, far short of the middle and last requested keys.
		for start := int64(0); start < 2400; start += 200 {
			var seed []proto.Message
			for id := start; id < start+200; id += 2 {
				seed = append(seed, customer(id))
			}
			_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecordBatch(seed)
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
		}
		dump := func() []fdb.KeyValue {
			out, err := sharedDB.RunRead(ctx, func(tx fdb.ReadTransaction) (any, error) {
				return tx.GetRange(root, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
			})
			Expect(err).NotTo(HaveOccurred())
			return out.([]fdb.KeyValue)
		}
		initial := dump()
		ids := []int64{1, 1201, 2397}
		var records []proto.Message
		for _, id := range ids {
			records = append(records, customer(id))
		}
		limit := 2*len(ids) + 2
		signposts := 0
		for _, kv := range initial {
			if mapSpace.Contains(kv.Key) {
				signposts++
			}
		}
		Expect(signposts).To(BeNumerically(">", limit), "the fixture must actually truncate the first map read")
		restore := func() {
			_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				rc.Transaction().ClearRange(root)
				for _, kv := range initial {
					rc.Transaction().Set(kv.Key, kv.Value)
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		}
		run := func(prefetch bool) (textPrefetchRun, *sparseTextObservation) {
			tx, err := sharedDB.CreateWritableTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()
			o := &sparseTextObservation{prefix: mapSpace.Bytes(), maintenance: !prefetch, secondGate: newSnapshotReadCounter(2)}
			rc := sharedDB.NewRecordContextIn(ctx, &sparseTextTransaction{WritableTransaction: tx, observation: o})
			b := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root)
			store, err := b.Open()
			Expect(err).NotTo(HaveOccurred())
			if prefetch {
				pending := b.PrefetchRecordBatch(records)
				Expect(o.reads).To(HaveLen(1), "the first wave has exactly one token map")
				Expect(o.reads[0].limit).To(Equal(limit))
				Expect(o.reads[0].drained).To(BeFalse())
				Expect(pending.Wait()).To(Succeed())
				Expect(o.reads).To(HaveLen(3), "one truncated map read and two uncovered-key reads")
				first := o.reads[0]
				Expect(first.rows).To(HaveLen(limit))
				Expect(bytes.Compare(first.rows[len(first.rows)-1].Key, mapSpace.Pack(tuple.Tuple{ids[1]}))).To(BeNumerically("<", 0))
				for i, read := range o.reads {
					Expect(read.drained).To(BeTrue(), "prefetch read %d must finish before maintenance", i)
					Expect(read.err).NotTo(HaveOccurred())
					Expect(read.maintenance).To(BeFalse())
					if i > 0 {
						Expect(read.limit).To(BeZero())
						Expect(read.begin).To(Equal(fdb.LastLessOrEqual(mapSpace.Pack(tuple.Tuple{ids[i]}))))
					}
				}
				Expect(o.secondGate.issued).To(Equal(2))
				Expect(o.secondGate.completing).To(Equal(2))
				Expect(o.secondGate.maxInFlight).To(Equal(2))
				Expect(o.secondGate.inFlight).To(BeZero())
				Expect(o.secondGate.gateTimedOut).To(BeFalse())
			}
			o.maintenance = true
			// Capture only normal SaveRecord maintenance and commit, after the
			// prefetch is fully drained. Cache hits send no storage requests.
			captured := captureTextPrefetchRun(root, func() error {
				for _, record := range records {
					if _, err := store.SaveRecord(record); err != nil {
						return err
					}
				}
				return rc.Commit()
			})
			maintenanceReads := 0
			for _, read := range o.reads {
				if read.maintenance {
					maintenanceReads++
					Expect(read.drained).To(BeTrue())
					Expect(read.err).NotTo(HaveOccurred())
				}
			}
			Expect(maintenanceReads).To(Equal(len(records)), "normal BunchedMap maintenance must still execute")
			Expect(captured.answered).To(Equal(captured.reads))
			return captured, o
		}

		sequential, _ := run(false)
		Expect(sequential.reads).To(BeNumerically(">", 0), "positive control: cold maintenance must send storage reads")
		wantRows := dump()
		restore()
		prefetched, _ := run(true)
		Expect(prefetched.reads).To(BeZero(), "both sparse second-wave ranges must warm subsequent maintenance")
		Expect(prefetched.commit.transaction.Mutations).To(Equal(sequential.commit.transaction.Mutations))
		Expect(prefetched.commit.transaction.ReadConflictRanges).To(Equal(sequential.commit.transaction.ReadConflictRanges))
		Expect(prefetched.commit.transaction.WriteConflictRanges).To(Equal(sequential.commit.transaction.WriteConflictRanges))
		Expect(dump()).To(Equal(wantRows))
	})
})
