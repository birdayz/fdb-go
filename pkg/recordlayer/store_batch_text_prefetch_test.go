package recordlayer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	wiretypes "fdb.dev/pkg/fdbgo/wire/types"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// countingTextTokenizer is the default tokenizer under its own name, counting
// TokenizeToMap calls per text; onTokenize runs inside each call.
type countingTextTokenizer struct {
	*DefaultTextTokenizer
	name       string
	mu         sync.Mutex
	calls      map[string]int
	onTokenize func(text string)
}

func (t *countingTextTokenizer) Name() string                { return t.name }
func (t *countingTextTokenizer) GetTokenizer() TextTokenizer { return t }

func (t *countingTextTokenizer) TokenizeToMap(text string, version int, mode TokenizerMode) (map[string][]int, error) {
	t.mu.Lock()
	t.calls[text]++
	hook := t.onTokenize
	t.mu.Unlock()
	if hook != nil {
		hook(text)
	}
	return t.DefaultTextTokenizer.TokenizeToMap(text, version, mode)
}

// reset clears the counts and sets the hook.
func (t *countingTextTokenizer) reset(onTokenize func(text string)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls, t.onTokenize = map[string]int{}, onTokenize
}

func (t *countingTextTokenizer) counts() map[string]int {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]int, len(t.calls))
	for text, n := range t.calls {
		out[text] = n
	}
	return out
}

// textPrefetchRun describes the successful attempt's commit and owned storage reads.
type textPrefetchRun struct {
	commit batchConflictCommit
	// reads counts GetKeyValues/GetValue sends; roundTrips counts sends with no outstanding reply, not dependency waves.
	// answered matches replies by token; overlap is diagnostic only.
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

	// countingMetadata indexes Customer.name with a counting tokenizer registered as name.
	countingMetadata := func(name string) (*RecordMetaData, *countingTextTokenizer) {
		tok := &countingTextTokenizer{DefaultTextTokenizer: &DefaultTextTokenizer{}, name: name, calls: map[string]int{}}
		Expect(GlobalTextTokenizerRegistry().Register(tok)).To(Succeed())
		b := baseBuilder()
		b.AddIndex("Customer", NewTextIndex("counted_text", Field("name")).SetOption(IndexOptionTextTokenizerName, name))
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		return md, tok
	}

	It("tokenizes each text of a batch once, maintenance reusing the prefetch's tokenization", func() {
		md, tok := countingMetadata("batch_token_reuse_test_tokenizer")
		for _, m := range []struct {
			mode
			want int // TokenizeToMap calls per text
		}{
			{modes[1], 1},   // batch: the prefetch tokenizes, maintenance reuses it
			{modes[0], 1},   // batch without prefetch: maintenance alone
			{modes[2], 2},   // PrefetchRecordBatch then SaveRecord: nothing shared
			{sequential, 1}, // SaveRecord alone
		} {
			root := specSubspace().Sub(m.name)
			var records []proto.Message
			want := map[string]int{}
			for id := range int64(6) {
				text := fmt.Sprintf("%s record%d", words(int(id), 6), id)
				records = append(records, customer(id, text, ""))
				want[text] = m.want
			}
			_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				b := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root)
				s, err := b.CreateOrOpen()
				if err != nil {
					return nil, err
				}
				tok.reset(nil) // the counts are the committing attempt's
				return nil, m.save(s, b, records)
			})
			Expect(err).NotTo(HaveOccurred(), m.name)
			Expect(tok.counts()).To(Equal(want), m.name)
		}
	})

	// Deterministic, without -race: saves run inside a batch's prefetch, after it
	// tokenized t1, neither use nor replace the batch's tokenizations.
	It("keeps a batch's tokenizations from a SaveRecord and a batch run during it", func() {
		md, tok := countingMetadata("batch_token_isolation_test_tokenizer")
		root := specSubspace()
		t1, t2 := "alpha beta gamma", "delta epsilon zeta"
		counts, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			s := open(rc, md, root)
			var nested error
			fired := false
			tok.reset(func(text string) {
				if text != t2 || fired {
					return
				}
				fired = true
				if _, nested = s.SaveRecord(customer(900, t1, "")); nested == nil {
					_, nested = s.SaveRecordBatch([]proto.Message{customer(901, t1, "")})
				}
			})
			defer tok.reset(nil)
			if _, err := s.SaveRecordBatch([]proto.Message{customer(1, t1, ""), customer(2, t2, "")}); err != nil {
				return nil, err
			}
			if !fired {
				return nil, errors.New("the batch never tokenized t2")
			}
			return tok.counts(), nested
		})
		Expect(err).NotTo(HaveOccurred())
		// t1: once each by the batch's prefetch, the SaveRecord and the inner batch's
		// prefetch; t2: once, by the prefetch. Neither batch's maintenance tokenizes.
		Expect(counts).To(Equal(map[string]int{t1: 3, t2: 1}))
	})

	// Run with -race: a batch's tokenizations are its own, so saves beside it on
	// the store neither read nor write them.
	It("keeps a batch's tokenizations to the batch while SaveRecord runs beside it", func() {
		md, root := metadata(), specSubspace()
		const savers, rounds = 4, 8
		indexed, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			s := open(rc, md, root)
			var wg sync.WaitGroup
			errs := make(chan error, savers*rounds)
			for g := range savers {
				wg.Add(1)
				go func() {
					defer GinkgoRecover()
					defer wg.Done()
					for i := range rounds {
						id := int64(100*g + 2*i)
						var err error
						if g%2 == 0 {
							_, err = s.SaveRecordBatch([]proto.Message{customer(id, words(int(id), 12), "b"), customer(id+1, words(int(id), 12), "b")})
						} else {
							_, err = s.SaveRecord(customer(id, words(int(id), 12), "s"))
						}
						errs <- err
					}
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					return nil, err
				}
			}
			// Every record is indexed under the token all texts share.
			entries, err := AsList(ctx, s.ScanIndexByType(md.GetIndex("prefetch_text"), IndexScanByTextToken,
				TupleRangeAllOf(tuple.Tuple{"agent"}), nil, ForwardScan()))
			if err != nil {
				return nil, err
			}
			var ids []int64
			for _, e := range entries {
				ids = append(ids, e.PrimaryKey()[0].(int64))
			}
			return ids, nil
		})
		Expect(err).NotTo(HaveOccurred())
		var want []int64
		for g := range savers {
			for i := range rounds {
				id := int64(100*g + 2*i)
				want = append(want, id)
				if g%2 == 0 {
					want = append(want, id+1)
				}
			}
		}
		Expect(indexed).To(ConsistOf(want))
	})

	// Use exporter-shaped records with gated first-wave replies to prove send-before-wait
	// independently of server speed; uncontrolled wire overlap is only diagnostic.
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
