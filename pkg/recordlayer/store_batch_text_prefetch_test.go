package recordlayer

import (
	"bytes"
	"context"
	"fmt"
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

// textPrefetchRun describes the successful attempt's commit and owned storage reads.
type textPrefetchRun struct {
	commit batchConflictCommit
	// reads is the number of storage reads sent (GetKeyValues and GetValue);
	// roundTrips counts sends while no reply is outstanding: a wire-overlap
	// diagnostic, not the number of dependency waves. answered counts the
	// replies matched to those reads by reply token.
	reads, roundTrips, answered int
	elapsed                     time.Duration
	prefetchGate                *snapshotReadCounter
}

// Gate only the TEXT prefetch's first-wave ranges, not normal maintenance or
// store-open reads. Fast server replies cannot release a wait before all sends.
type gatedTextPrefetchTransaction struct {
	fdb.WritableTransaction
	indexPrefix []byte
	counter     *snapshotReadCounter
}

func (t *gatedTextPrefetchTransaction) Snapshot() fdb.ReadTransaction {
	return &gatedTextPrefetchSnapshot{ReadTransaction: t.WritableTransaction.Snapshot(), indexPrefix: t.indexPrefix, counter: t.counter}
}

type gatedTextPrefetchSnapshot struct {
	fdb.ReadTransaction
	indexPrefix []byte
	counter     *snapshotReadCounter
}

func (s *gatedTextPrefetchSnapshot) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	begin, _ := r.FDBRangeKeySelectors()
	if options.Limit > 0 && bytes.HasPrefix(begin.FDBKeySelector().Key.FDBKey(), s.indexPrefix) {
		return (&inFlightSnapshot{ReadTransaction: s.ReadTransaction, c: s.counter}).GetRange(r, options)
	}
	return s.ReadTransaction.GetRange(r, options)
}

func captureTextPrefetchRun(root subspace.Subspace, run func() error, logs ...*fixtureWireLog) textPrefetchRun {
	batchConflictCaptureMu.Lock()
	defer batchConflictCaptureMu.Unlock()
	log := &fixtureWireLog{}
	if len(logs) != 0 {
		log = logs[0]
	}
	stop := transport.CaptureWireLog(log)
	defer stop()
	start := time.Now()
	err := run()
	stop()
	Expect(err).NotTo(HaveOccurred())
	if len(log.attempts) != 0 {
		start = log.attempts[len(log.attempts)-1].started
	}
	out := textPrefetchRun{elapsed: time.Since(start)}
	frames, final := log.frames(), log.finalAttempt()
	out.commit = successfulFixtureCommit(root, frames, final)
	outstanding := map[[16]byte]bool{}
	for _, frame := range frames {
		if frame.direction == 'R' {
			if outstanding[frame.token] {
				delete(outstanding, frame.token)
				out.answered++
			}
			continue
		}
		if frame.direction != 'S' || frame.attempt != final {
			continue
		}
		r, err := wire.NewReader(frame.body)
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
			if bytes.HasPrefix(request.Begin.Key, root.Bytes()) {
				read(request.Reply)
			}
		case wiretypes.GetValueRequestFileID:
			var request wiretypes.GetValueRequest
			Expect(request.UnmarshalFromReader(r)).To(Succeed())
			if bytes.HasPrefix(request.Key, root.Bytes()) {
				read(request.Reply)
			}
		}
	}
	Expect(outstanding).To(BeEmpty(), "all successful-attempt storage requests must have their own replies")
	return out
}

type textPrefetchCaptureOptions struct {
	gateSize   int
	extraRetry bool
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
	runMode := func(md *RecordMetaData, root subspace.Subspace, m mode, records []proto.Message, options ...textPrefetchCaptureOptions) textPrefetchRun {
		opt := textPrefetchCaptureOptions{}
		if len(options) != 0 {
			opt = options[0]
		}
		var counter *snapshotReadCounter
		counters := map[*FDBRecordContext]*snapshotReadCounter{}
		log := &fixtureWireLog{}
		retry := newFixtureCommitRetry(root)
		captured := captureTextPrefetchRun(root, func() error {
			_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				log.begin(rc)
				retry.begin(rc)
				counter = nil
				if opt.gateSize > 0 {
					counter = newSnapshotReadCounter(opt.gateSize)
					counters[rc] = counter
					rc.tx = &gatedTextPrefetchTransaction{WritableTransaction: rc.tx, indexPrefix: root.Sub(IndexKey).Bytes(), counter: counter}
				}
				b := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root)
				s, err := b.CreateOrOpen()
				if err != nil {
					return nil, err
				}
				if err := m.save(s, b, records); err != nil {
					return nil, err
				}
				if opt.extraRetry {
					return nil, retry.conflict(rc)
				}
				return nil, nil
			})
			return err
		}, log)
		if opt.extraRetry {
			retry.assertReplayed()
			Expect(captured.commit.rejected[retry.injectedContext]).To(Equal([]int{1020}))
			if opt.gateSize > 0 {
				for _, rc := range []*FDBRecordContext{retry.injectedContext, retry.lastContext} {
					gate := counters[rc]
					Expect(gate).NotTo(BeNil())
					Expect(gate.issued).To(Equal(opt.gateSize))
					Expect(gate.completing).To(Equal(opt.gateSize))
					Expect(gate.maxInFlight).To(Equal(opt.gateSize))
					Expect(gate.inFlight).To(BeZero())
					Expect(gate.gateTimedOut).To(BeFalse())
				}
			}
		}
		captured.prefetchGate = counter
		return captured
	}

	It("keeps the successful-attempt TEXT wire and first-wave observations after a rejected commit", func() {
		b := baseBuilder()
		b.AddIndex("Customer", NewTextIndex("prefetch_text", Field("name")))
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) { open(rc, md, root); return nil, nil })
		Expect(err).NotTo(HaveOccurred())
		initial := dump(root)
		records := []proto.Message{customer(1, "quokka common", ""), customer(2, "quokka tool", "")}
		control := runMode(md, root, sequential, records)
		want := dump(root)
		restore(root, initial)
		got := runMode(md, root, modes[1], records, textPrefetchCaptureOptions{gateSize: 3, extraRetry: true})
		Expect(got.answered).To(Equal(got.reads))
		Expect(got.commit.transaction.Mutations).To(Equal(control.commit.transaction.Mutations))
		Expect(got.commit.transaction.ReadConflictRanges).To(Equal(control.commit.transaction.ReadConflictRanges))
		Expect(got.commit.transaction.WriteConflictRanges).To(Equal(control.commit.transaction.WriteConflictRanges))
		Expect(dump(root)).To(Equal(want))
	})

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
			fmt.Fprintf(GinkgoWriter, "%s: storage reads=%d wire idle sends=%d mutations=%d\n", m.name, got.reads, got.roundTrips, len(got.commit.transaction.Mutations))
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
	// Gate the first-wave responses to prove send-before-wait independently of
	// server speed; uncontrolled wire overlap remains diagnostic only.
	It("issues every token-map prefetch before completing the first wait", func() {
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
		tokens := map[string]struct{}{}
		for _, record := range batch {
			for _, token := range strings.Fields(record.(*demopb.Customer).GetName()) {
				tokens[token] = struct{}{}
			}
		}
		Expect(len(tokens)).To(BeNumerically(">", DefaultPipelineSize))
		var want textPrefetchRun
		for i, m := range append([]mode{sequential}, modes...) {
			var runs []textPrefetchRun
			for range 3 {
				restore(root, initial)
				gateSize := 0
				if m.name != sequential.name && m.name != "batch without prefetch" {
					gateSize = len(tokens)
				}
				run := runMode(md, root, m, batch, textPrefetchCaptureOptions{gateSize: gateSize})
				Expect(run.answered).To(Equal(run.reads), m.name)
				if gateSize > 0 {
					Expect(run.prefetchGate).NotTo(BeNil(), m.name)
					Expect(run.prefetchGate.issued).To(Equal(gateSize), m.name)
					Expect(run.prefetchGate.completing).To(Equal(gateSize), m.name)
					Expect(run.prefetchGate.maxInFlight).To(Equal(gateSize), m.name)
					Expect(run.prefetchGate.inFlight).To(BeZero(), m.name)
					Expect(run.prefetchGate.gateTimedOut).To(BeFalse(), m.name)
				}
				runs = append(runs, run)
			}
			slices.SortFunc(runs, func(a, b textPrefetchRun) int { return int(a.elapsed - b.elapsed) })
			got := runs[1]
			fmt.Printf("text batch prefetch: mode=%q records=%d tokens_per_record=57 storage_reads=%d wire_idle_sends=%d median_wall=%s\n",
				m.name, len(batch), got.reads, got.roundTrips, got.elapsed)
			if i == 0 {
				want = got
				continue
			}
			Expect(got.commit.transaction.Mutations).To(Equal(want.commit.transaction.Mutations), m.name)
			Expect(got.commit.transaction.ReadConflictRanges).To(Equal(want.commit.transaction.ReadConflictRanges), m.name)
			Expect(got.commit.transaction.WriteConflictRanges).To(Equal(want.commit.transaction.WriteConflictRanges), m.name)
			fmt.Fprintf(GinkgoWriter, "wire-overlap counts (diagnostic): mode=%q sequential=%d observed=%d\n", m.name, want.roundTrips, got.roundTrips)
		}
	})
})
