package recordlayer

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	wiretypes "fdb.dev/pkg/fdbgo/wire/types"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// textPrefetchRun is one captured transaction: its commit and its storage reads.
type textPrefetchRun struct {
	commit batchConflictCommit
	// reads is the number of storage reads sent (GetKeyValues and GetValue);
	// roundTrips the number of them sent while no other was outstanding, the
	// read round trips the transaction waited for one after another; answered
	// the replies matched to them by reply token.
	reads, roundTrips, answered int
	elapsed                     time.Duration
}

func captureTextPrefetchRun(root subspace.Subspace, run func() error) textPrefetchRun {
	batchConflictCaptureMu.Lock()
	defer batchConflictCaptureMu.Unlock()
	var log bytes.Buffer
	stop := transport.CaptureWireLog(&log)
	start := time.Now()
	err := run()
	elapsed := time.Since(start)
	stop()
	Expect(err).NotTo(HaveOccurred())
	out := textPrefetchRun{elapsed: elapsed}
	input := bytes.NewReader(log.Bytes())
	var commits []batchConflictCommit
	outstanding := map[[16]byte]bool{}
	for {
		var header [29]byte
		if _, err := io.ReadFull(input, header[:]); err == io.EOF {
			break
		} else {
			Expect(err).NotTo(HaveOccurred())
		}
		body := make([]byte, int(binary.LittleEndian.Uint32(header[25:29])))
		_, err := io.ReadFull(input, body)
		Expect(err).NotTo(HaveOccurred())
		if header[0] == 'R' {
			var token [16]byte
			copy(token[:], header[9:25])
			if outstanding[token] {
				delete(outstanding, token)
				out.answered++
			}
			continue
		}
		r, err := wire.NewReader(body)
		if err != nil {
			continue
		}
		read := func(reply wiretypes.ReplyPromise) {
			out.reads++
			if len(outstanding) == 0 {
				out.roundTrips++
			}
			outstanding[reply.Token] = true
		}
		switch r.FileIdentifier() {
		case wiretypes.GetKeyValuesRequestFileID:
			var request wiretypes.GetKeyValuesRequest
			Expect(request.UnmarshalFromReader(r)).To(Succeed())
			read(request.Reply)
		case wiretypes.GetValueRequestFileID:
			var request wiretypes.GetValueRequest
			Expect(request.UnmarshalFromReader(r)).To(Succeed())
			read(request.Reply)
		case wiretypes.CommitTransactionRequestFileID:
			var request wiretypes.CommitTransactionRequest
			Expect(request.UnmarshalFromReader(r)).To(Succeed())
			for _, mutation := range request.Transaction.Mutations {
				if bytes.HasPrefix(mutation.Param1, root.Bytes()) {
					commits = append(commits, batchConflictCommit{transaction: request.Transaction, bytes: len(body)})
					break
				}
			}
		}
	}
	Expect(commits).To(HaveLen(1))
	out.commit = commits[0]
	return out
}

// Batch saves prefetch their TEXT bunches into the transaction's cache; the
// commit must be the commit of the same records saved one after another.
var _ = Describe("Batch TEXT bunch prefetch", func() {
	ctx := context.Background()

	metadata := func() *RecordMetaData {
		b := baseBuilder()
		b.AddIndex("Customer", NewTextIndex("prefetch_text", Field("name")))
		b.AddIndex("Customer", NewTextIndex("prefetch_text_grouped", GroupBy(Field("name"), Field("email"))))
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}
	// words is a text of n tokens from a vocabulary of 40, the first five
	// common to every text, so maps hold many records and span bunches.
	words := func(seed, n int) string {
		w := []string{"the", "agent", "said", "tool", "call"}
		for j := 0; len(w) < n; j++ {
			w = append(w, fmt.Sprintf("w%02d", (seed*7+j*3)%35))
		}
		return strings.Join(w, " ")
	}
	customer := func(id int64, name, email string) *demopb.Customer {
		return &demopb.Customer{CustomerId: proto.Int64(id), Name: proto.String(name), Email: proto.String(email)}
	}
	open := func(rc *FDBRecordContext, md *RecordMetaData, root subspace.Subspace) *FDBRecordStore {
		s, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).CreateOrOpen()
		Expect(err).NotTo(HaveOccurred())
		return s
	}
	dump := func(root subspace.Subspace) []fdb.KeyValue {
		v, err := sharedDB.RunRead(ctx, func(tx fdb.ReadTransaction) (any, error) {
			r, err := fdb.PrefixRange(root.Bytes())
			if err != nil {
				return nil, err
			}
			return tx.GetRange(r, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
		})
		Expect(err).NotTo(HaveOccurred())
		return v.([]fdb.KeyValue)
	}
	restore := func(root subspace.Subspace, rows []fdb.KeyValue) {
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			r, err := fdb.PrefixRange(root.Bytes())
			if err != nil {
				return nil, err
			}
			rc.Transaction().ClearRange(r)
			for _, row := range rows {
				rc.Transaction().Set(row.Key, row.Value)
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	}
	type mode struct {
		name string
		save func(*FDBRecordStore, *StoreBuilder, []proto.Message) error
	}
	sequential := mode{"sequential SaveRecord", func(s *FDBRecordStore, _ *StoreBuilder, records []proto.Message) error {
		for _, r := range records {
			if _, err := s.SaveRecord(r); err != nil {
				return err
			}
		}
		return nil
	}}
	modes := []mode{
		{"batch without prefetch", func(s *FDBRecordStore, _ *StoreBuilder, records []proto.Message) error {
			_, err := s.SaveRecordBatchWithOptions(records, BatchSaveOptions{skipTextPrefetch: true})
			return err
		}},
		{"batch", func(s *FDBRecordStore, _ *StoreBuilder, records []proto.Message) error {
			_, err := s.SaveRecordBatch(records)
			return err
		}},
		{"PrefetchRecordBatch then SaveRecord", func(s *FDBRecordStore, b *StoreBuilder, records []proto.Message) error {
			if err := b.PrefetchRecordBatch(records).Wait(); err != nil {
				return err
			}
			return sequential.save(s, b, records)
		}},
	}
	runMode := func(md *RecordMetaData, root subspace.Subspace, m mode, records []proto.Message) textPrefetchRun {
		return captureTextPrefetchRun(root, func() error {
			_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				b := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root)
				s, err := b.CreateOrOpen()
				if err != nil {
					return nil, err
				}
				return nil, m.save(s, b, records)
			})
			return err
		})
	}

	It("commits what saving the records one after another commits", func() {
		md, root := metadata(), specSubspace()
		// Even ids, each map's bunches full or nearly so.
		var seed []proto.Message
		for id := int64(0); id < 120; id += 2 {
			seed = append(seed, customer(id, words(int(id), 12), fmt.Sprintf("g%d", id%3)))
		}
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			_, err := open(rc, md, root).SaveRecordBatch(seed)
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		initial := dump(root)

		var batch []proto.Message
		// Odd ids between existing keys: inserts into full bunches, which
		// split, the next insert reading the split bunch.
		for id := int64(41); id < 61; id += 2 {
			batch = append(batch, customer(id, words(int(id), 12), "g1"))
		}
		// New keys past the end: appends, then new signposts.
		for id := int64(200); id < 230; id++ {
			batch = append(batch, customer(id, words(int(id), 10), "g2"))
		}
		batch = append(batch,
			customer(10, words(99, 12), "g1"),        // tokens change: removals and puts
			customer(12, words(12, 12), "g9"),        // same text, new group: the grouped index moves it
			customer(14, words(14, 12), "g2"),        // unchanged entries: no TEXT update
			customer(16, "", "g1"),                   // all tokens removed
			customer(203, words(5, 14), "g2"),        // a record of this batch, again
			customer(43, words(43, 12)+" fresh", ""), // and again, with a new token
		)

		var want textPrefetchRun
		var wantRows []fdb.KeyValue
		for i, m := range append([]mode{sequential}, modes...) {
			restore(root, initial)
			got := runMode(md, root, m, batch)
			rows := dump(root)
			fmt.Fprintf(GinkgoWriter, "%s: storage reads=%d read round trips=%d mutations=%d\n", m.name, got.reads, got.roundTrips, len(got.commit.transaction.Mutations))
			Expect(got.answered).To(Equal(got.reads), "every storage read's reply is matched")
			if i == 0 {
				want, wantRows = got, rows
				continue
			}
			Expect(got.commit.transaction.Mutations).To(Equal(want.commit.transaction.Mutations), m.name)
			Expect(got.commit.transaction.ReadConflictRanges).To(Equal(want.commit.transaction.ReadConflictRanges), m.name)
			Expect(got.commit.transaction.WriteConflictRanges).To(Equal(want.commit.transaction.WriteConflictRanges), m.name)
			Expect(rows).To(Equal(wantRows), m.name)
		}
	})

	// The GenAI exporter's message transaction: 128 new records of 57 tokens,
	// 20 of them in every record, into an index already holding 256 records.
	// Storage read round trips are counted on the wire; with the prefetch they
	// are the store open, the record loads and the prefetch, whatever the
	// batch size.
	It("reads a batch's TEXT bunches in one round trip", func() {
		b := baseBuilder()
		b.AddIndex("Customer", NewTextIndex("prefetch_text", Field("name")))
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		text := func(id int64) string {
			w := make([]string, 0, 57)
			for j := range 20 {
				w = append(w, fmt.Sprintf("common%02d", j))
			}
			for j := 0; len(w) < 57; j++ {
				w = append(w, fmt.Sprintf("v%03d", (int(id)*37+j*11)%400))
			}
			return strings.Join(w, " ")
		}
		var seed, batch []proto.Message
		for id := int64(0); id < 256; id++ {
			seed = append(seed, customer(id, text(id), ""))
		}
		for id := int64(256); id < 256+128; id++ {
			batch = append(batch, customer(id, text(id), ""))
		}
		for i := 0; i < len(seed); i += 64 {
			_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				_, err := open(rc, md, root).SaveRecordBatch(seed[i : i+64])
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
		}
		initial := dump(root)
		var want textPrefetchRun
		for i, m := range append([]mode{sequential}, modes...) {
			var runs []textPrefetchRun
			for range 3 {
				restore(root, initial)
				runs = append(runs, runMode(md, root, m, batch))
			}
			slices.SortFunc(runs, func(a, b textPrefetchRun) int { return int(a.elapsed - b.elapsed) })
			got := runs[1]
			fmt.Printf("text batch prefetch: mode=%q records=%d tokens_per_record=57 storage_reads=%d read_round_trips=%d median_wall=%s\n",
				m.name, len(batch), got.reads, got.roundTrips, got.elapsed)
			if i == 0 {
				want = got
				continue
			}
			Expect(got.commit.transaction.Mutations).To(Equal(want.commit.transaction.Mutations), m.name)
			Expect(got.commit.transaction.ReadConflictRanges).To(Equal(want.commit.transaction.ReadConflictRanges), m.name)
			Expect(got.commit.transaction.WriteConflictRanges).To(Equal(want.commit.transaction.WriteConflictRanges), m.name)
			// The store open, the record loads and the prefetch; against a
			// local server a burst of reads can be answered before it is all
			// sent and count twice.
			if m.name != "batch without prefetch" {
				Expect(got.roundTrips*8).To(BeNumerically("<", want.roundTrips), m.name)
			}
		}
	})
})
