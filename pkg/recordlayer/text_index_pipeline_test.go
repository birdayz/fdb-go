package recordlayer

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// snapshotReadCounter tracks the snapshot range reads made through a
// transaction: a read is in flight from GetRange until its GetSliceWithError
// returns. The first `gate` reads wait until `gate` are in flight at once, so
// a maintainer that read serially would time out instead of passing.
type snapshotReadCounter struct {
	mu                            sync.Mutex
	issued, inFlight, maxInFlight int
	gate                          int
	opened                        chan struct{}
	gateTimedOut                  bool
}

func newSnapshotReadCounter(gate int) *snapshotReadCounter {
	return &snapshotReadCounter{gate: gate, opened: make(chan struct{})}
}

func (c *snapshotReadCounter) begin() {
	c.mu.Lock()
	c.issued++
	c.inFlight++
	c.maxInFlight = max(c.maxInFlight, c.inFlight)
	wait := c.issued <= c.gate
	if c.issued == c.gate {
		close(c.opened)
	}
	c.mu.Unlock()
	if wait {
		select {
		case <-c.opened:
		case <-time.After(10 * time.Second):
			c.mu.Lock()
			c.gateTimedOut = true
			c.mu.Unlock()
		}
	}
}

func (c *snapshotReadCounter) end() {
	c.mu.Lock()
	c.inFlight--
	c.mu.Unlock()
}

type inFlightTransaction struct {
	fdb.WritableTransaction
	c *snapshotReadCounter
}

func (t *inFlightTransaction) Snapshot() fdb.ReadTransaction {
	return &inFlightSnapshot{ReadTransaction: t.WritableTransaction.Snapshot(), c: t.c}
}

type inFlightSnapshot struct {
	fdb.ReadTransaction
	c *snapshotReadCounter
}

func (s *inFlightSnapshot) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	s.c.begin()
	return &inFlightRangeResult{RangeResult: s.ReadTransaction.GetRange(r, options), c: s.c}
}

type inFlightRangeResult struct {
	fdb.RangeResult
	c *snapshotReadCounter
}

func (r *inFlightRangeResult) GetSliceWithError() ([]fdb.KeyValue, error) {
	defer r.c.end()
	return r.RangeResult.GetSliceWithError()
}

// The TEXT maintainer reads each token's BunchedMap with up to the store's
// TEXT_INDEX_UPDATE pipeline size of reads in flight, and writes the same
// index whatever that size is.
var _ = Describe("TEXT index pipelined update", func() {
	ctx := context.Background()

	textMetaData := func() (*RecordMetaData, []*Index) {
		plain := NewTextIndex("pipeline_text", Field("name"))
		aggressive := NewTextIndex("pipeline_text_aggressive", Field("name")).
			SetOption(IndexOptionTextAddAggressiveConflictRanges, "true")
		grouped := NewTextIndex("pipeline_text_grouped", GroupBy(Field("name"), Field("email")))
		builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Customer", plain)
		builder.AddIndex("Customer", aggressive)
		builder.AddIndex("Customer", grouped)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		return md, []*Index{plain, aggressive, grouped}
	}

	// manyTokens is a name of n distinct tokens drawn from a shared vocabulary,
	// so token maps hold several records.
	manyTokens := func(seed, n int) string {
		words := make([]string, n)
		for j := range words {
			words[j] = fmt.Sprintf("w%03d", (seed*7+j*3)%41)
		}
		// A repeated word gives a position list longer than one.
		return strings.Join(words, " ") + " " + words[0]
	}

	sizer := func(textSize int) PipelineSizer {
		return func(op PipelineOperation) int {
			if op == PipelineOperationTextIndexUpdate {
				return textSize
			}
			return DefaultPipelineSizer(op)
		}
	}

	openStore := func(rtx *FDBRecordContext, md *RecordMetaData, ks subspace.Subspace, size int) *FDBRecordStore {
		store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).
			SetPipelineSizer(sizer(size)).CreateOrOpen()
		Expect(err).NotTo(HaveOccurred())
		return store
	}

	It("issues token reads concurrently up to the pipeline size", func() {
		md, indexes := textMetaData()
		for _, size := range []int{1, 4, DefaultPipelineSize, 1000} {
			ks := specSubspace().Sub(size)
			var counter *snapshotReadCounter
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				counter = newSnapshotReadCounter(min(size, 30))
				store := openStore(rtx, md, ks, size)
				Expect(store.GetPipelineSize(PipelineOperationTextIndexUpdate)).To(Equal(size))
				m, err := store.getIndexMaintainer(indexes[0])
				Expect(err).NotTo(HaveOccurred())
				tm := m.(*textIndexMaintainer)
				Expect(tm.pipelineSize).To(Equal(size))
				tm.tx = &inFlightTransaction{WritableTransaction: tm.tx, c: counter}
				_, err = store.SaveRecord(&demopb.Customer{CustomerId: proto.Int64(1), Name: proto.String(manyTokens(1, 30))})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			// One BunchedMap read per distinct token, all of them completed.
			Expect(counter.issued).To(Equal(30), "size=%d", size)
			Expect(counter.inFlight).To(BeZero(), "size=%d", size)
			Expect(counter.gateTimedOut).To(BeFalse(), "size=%d: reads were not concurrent", size)
			Expect(counter.maxInFlight).To(Equal(min(size, 30)), "size=%d", size)
		}
	})

	It("reads concurrently on delete, and when an update changes the token set", func() {
		md, indexes := textMetaData()
		ks := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store := openStore(rtx, md, ks, 5)
			_, err := store.SaveRecord(&demopb.Customer{CustomerId: proto.Int64(1), Name: proto.String(manyTokens(1, 20))})
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		for _, step := range []func(*FDBRecordStore) error{
			func(store *FDBRecordStore) error {
				_, err := store.SaveRecord(&demopb.Customer{CustomerId: proto.Int64(1), Name: proto.String(manyTokens(2, 20))})
				return err
			},
			func(store *FDBRecordStore) error {
				_, err := store.DeleteRecord(tuple.Tuple{int64(1)})
				return err
			},
		} {
			var counter *snapshotReadCounter
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				counter = newSnapshotReadCounter(5)
				store := openStore(rtx, md, ks, 5)
				m, err := store.getIndexMaintainer(indexes[0])
				Expect(err).NotTo(HaveOccurred())
				tm := m.(*textIndexMaintainer)
				tm.tx = &inFlightTransaction{WritableTransaction: tm.tx, c: counter}
				return nil, step(store)
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(counter.issued).To(BeNumerically(">=", 20))
			Expect(counter.inFlight).To(BeZero())
			Expect(counter.gateTimedOut).To(BeFalse())
			Expect(counter.maxInFlight).To(Equal(5))
		}
	})

	It("writes the same index entries at every pipeline size", func() {
		md, indexes := textMetaData()
		// rawIndex is every key-value of the TEXT indexes' primary and secondary
		// subspaces, relative to the store.
		rawIndex := func(ks subspace.Subspace) []string {
			var out []string
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				for _, idx := range indexes {
					for _, ss := range []subspace.Subspace{ks.Sub(IndexKey, idx.SubspaceTupleKey()), ks.Sub(IndexSecondarySpaceKey, idx.SubspaceTupleKey())} {
						kvs, err := rtx.Transaction().GetRange(ss, fdb.RangeOptions{}).GetSliceWithError()
						if err != nil {
							return nil, err
						}
						for _, kv := range kvs {
							out = append(out, hex.EncodeToString(kv.Key[len(ks.Bytes()):])+"="+hex.EncodeToString(kv.Value))
						}
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			return out
		}
		run := func(size int) []string {
			ks := specSubspace().Sub(size)
			// Inserts, then an update that changes the token set, then a delete,
			// each in its own transaction.
			steps := []func(*FDBRecordStore) error{}
			for i := 0; i < 60; i++ {
				i := i
				steps = append(steps, func(store *FDBRecordStore) error {
					_, err := store.SaveRecord(&demopb.Customer{
						CustomerId: proto.Int64(int64(i)), Name: proto.String(manyTokens(i, 25)), Email: proto.String(fmt.Sprintf("g%d", i%2)),
					})
					return err
				})
			}
			steps = append(steps,
				func(store *FDBRecordStore) error {
					_, err := store.SaveRecord(&demopb.Customer{CustomerId: proto.Int64(7), Name: proto.String(manyTokens(50, 40)), Email: proto.String("g1")})
					return err
				},
				func(store *FDBRecordStore) error {
					_, err := store.DeleteRecord(tuple.Tuple{int64(11)})
					return err
				})
			for _, step := range steps {
				_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					return nil, step(openStore(rtx, md, ks, size))
				})
				Expect(err).NotTo(HaveOccurred())
			}
			return rawIndex(ks)
		}
		serial := run(1)
		Expect(len(serial)).To(BeNumerically(">", 3*41))
		// More bunches than the 41 tokens: token maps span several bunches.
		plainPrefix := hex.EncodeToString(tuple.Tuple{IndexKey, indexes[0].SubspaceTupleKey()}.Pack())
		plainKVs := 0
		for _, kv := range serial {
			if strings.HasPrefix(kv, plainPrefix) {
				plainKVs++
			}
		}
		Expect(plainKVs).To(BeNumerically(">", 41))
		Expect(run(DefaultPipelineSize)).To(Equal(serial))
		Expect(run(64)).To(Equal(serial))
	})
})
