package otel

// GenAI message storage on standard Record Layer records.
//
// Tenant-wide digest dedup replaces repeated GenAI message elements with manifests.
// Raw-byte/text-token ceilings plan parts; tails may share transactions, with the head published last.
//
// Monotonic (writer, seq) IDs make TEXT postings append; one backend isolates its writers by shard.
// Independent backends may share shards and conflict.
//
// fulltext_v2 indexes message text by tenant, role and writer shard. MessageTrace
// markers select traces; envelope_refs_v1 reads each trace's packed ref_chunks.
//
// Message records are immutable, shared by every span of the tenant that
// references them, and never collected.

import (
	"context"
	"sort"
	"sync"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// messagePipelineSizer overlaps token-map reads for TEXT_INDEX_UPDATE only;
// other operations retain Java’s default pipeline sizing.
func messagePipelineSizer(op recordlayer.PipelineOperation) int {
	if op == recordlayer.PipelineOperationTextIndexUpdate {
		return messageTextPipeline
	}
	return recordlayer.DefaultPipelineSizer(op)
}

// ---------------------------------------------------------------------------
// Message records
// ---------------------------------------------------------------------------

func messageDescriptor() (protoreflect.MessageDescriptor, error) {
	fd, err := recordFile(messagesFullText)
	if err != nil {
		return nil, err
	}
	return fd.Messages().ByName("Message"), nil
}

// planMessage pairs raw chunk i with text segment i and repeats the role per part.
// Part 0 also carries digest, encoding, raw length, part count and unindexed-token count.
func planMessage(tenant int, el *messageElement, fulltext bool, distinct int) (*messagePlan, error) {
	d, err := messageDescriptor()
	if err != nil {
		return nil, err
	}
	mp, err := splitMessageParts(el, fulltext, distinct)
	if err != nil {
		return nil, err
	}
	p := &messagePlan{element: el, unindexed: mp.unindexed}
	role, segments, chunks := mp.role, mp.segments, mp.chunks
	parts := max(len(chunks), len(segments), 1)
	f := d.Fields()
	for i := range parts {
		m := dynamicpb.NewMessage(d)
		m.Set(f.ByName("tenant"), protoreflect.ValueOfInt64(int64(tenant)))
		m.Set(f.ByName("part"), protoreflect.ValueOfInt64(int64(i)))
		if fulltext {
			m.Set(f.ByName("role"), protoreflect.ValueOfString(role))
		}
		if i < len(segments) {
			s := segments[i]
			m.Set(f.ByName("text"), protoreflect.ValueOfString(s.text))
			if s.offset > 0 {
				m.Set(f.ByName("token_offset"), protoreflect.ValueOfInt64(int64(s.offset)))
			}
			p.postings += int64(s.postings)
		}
		if i < len(chunks) {
			m.Set(f.ByName("raw"), protoreflect.ValueOfBytes(chunks[i]))
			p.raw += int64(len(chunks[i]))
		}
		if i == 0 {
			m.Set(f.ByName("hash"), protoreflect.ValueOfBytes(el.digest[:]))
			m.Set(f.ByName("encoding"), protoreflect.ValueOfString(el.encoding))
			m.Set(f.ByName("raw_bytes"), protoreflect.ValueOfInt64(int64(len(el.raw))))
			m.Set(f.ByName("parts"), protoreflect.ValueOfInt64(int64(parts)))
			if p.unindexed > 0 {
				m.Set(f.ByName("unindexed_tokens"), protoreflect.ValueOfInt64(p.unindexed))
			}
			if fulltext && len(segments) > 1 {
				m.Mutable(f.ByName("large")).List().Append(protoreflect.ValueOfInt64(1))
			}
		}
		p.records = append(p.records, m)
	}
	return p, nil
}

func setMessageID(m proto.Message, id messageID) {
	r := m.ProtoReflect()
	f := r.Descriptor().Fields()
	r.Set(f.ByName("writer"), protoreflect.ValueOfInt64(id.writer))
	r.Set(f.ByName("seq"), protoreflect.ValueOfInt64(id.seq))
	r.Set(f.ByName("shard"), protoreflect.ValueOfInt64(messageShard(id.writer)))
}

func messageKey(tenant int, id messageID, part int) tuple.Tuple {
	return tuple.Tuple{int64(tenant), id.writer, id.seq, int64(part)}
}

// messageTimerStats reads the BunchedMap counters of one message transaction.
func messageTimerStats(t *recordlayer.StoreTimer, s *MessageStoreStats) {
	s.TextIndexWrites = t.GetCount(recordlayer.CountSaveIndexKey)
	s.TextIndexKeyBytes = t.GetCount(recordlayer.CountSaveIndexKeyBytes)
	s.TextIndexValueBytes = t.GetCount(recordlayer.CountSaveIndexValueBytes)
	s.TextIndexReadBytes = t.GetCount(recordlayer.CountLoadIndexKeyBytes) + t.GetCount(recordlayer.CountLoadIndexValueBytes)
}

// ---------------------------------------------------------------------------
// Writers
// ---------------------------------------------------------------------------

// acquireWriter waits for a slot and reuses or registers a writer;
// the run-wide counter keeps IDs unique across handles.
func (b *FDBBackend) acquireWriter(ctx context.Context) (*messageWriter, error) {
	b.writers.once.Do(func() { b.writers.slots = make(chan struct{}, messageWriterLimit) })
	select {
	case b.writers.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, wrap("canceled", "message writer", ctx.Err())
	}
	w, err := b.takeWriter(ctx)
	if err != nil {
		<-b.writers.slots
	}
	return w, err
}

func (b *FDBBackend) takeWriter(ctx context.Context) (*messageWriter, error) {
	b.writers.mu.Lock()
	if n := len(b.writers.free); n > 0 {
		w := b.writers.free[n-1]
		b.writers.free = b.writers.free[:n-1]
		b.writers.mu.Unlock()
		return w, nil
	}
	// The writer slots bound the writers to messageTextShards, so a shard is free.
	shard := int64(0)
	for b.writers.shards[shard] {
		shard++
	}
	b.writers.shards[shard] = true
	b.writers.mu.Unlock()
	key := b.keys.messageWriters
	var id int64
	_, err := runTxnSlot(ctx, b.db, "message writer", func(rc *recordlayer.FDBRecordContext) (any, error) {
		if err := configureRecordTimeout(rc); err != nil {
			return nil, err
		}
		if err := b.probeMessageTxn("writer-before-write", nil); err != nil {
			return nil, err
		}
		if err := b.readManifest(rc, true); err != nil {
			return nil, err
		}
		v, err := rc.Transaction().Get(key).Get()
		if err != nil {
			return nil, err
		}
		last := int64(0)
		if len(v) > 0 {
			t, err := tuple.Unpack(v)
			ok := false
			if err == nil && len(t) == 1 {
				last, ok = t[0].(int64)
			}
			if !ok || last < 1 {
				return nil, fail("corruption", "message writer", "malformed writer counter")
			}
		}
		id = nextWriterID(last, shard)
		rc.Transaction().Set(key, tuple.Tuple{id}.Pack())
		return nil, nil
	})
	if err != nil {
		b.writers.mu.Lock()
		b.writers.shards[shard] = false
		b.writers.mu.Unlock()
		return nil, err
	}
	return &messageWriter{id: id}, nil
}

func (b *FDBBackend) releaseWriter(w *messageWriter) {
	b.writers.mu.Lock()
	b.writers.free = append(b.writers.free, w)
	b.writers.mu.Unlock()
	<-b.writers.slots
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

// hashIndexRange is the dedup index range of one digest.
func hashIndexRange(space []byte, tenant int, digest messageDigest) (fdb.KeyRange, error) {
	return fdb.PrefixRange(append(append([]byte(nil), space...), tuple.Tuple{int64(tenant), digest[:]}.Pack()...))
}

// decodeHashEntry reads the message ID from a dedup index entry.
func decodeHashEntry(space []byte, kv fdb.KeyValue) (messageID, error) {
	t, err := tuple.Unpack(kv.Key[len(space):])
	var id messageID
	ok := err == nil && len(t) == 5
	var part int64
	if ok {
		id.writer, ok = t[2].(int64)
	}
	if ok {
		id.seq, ok = t[3].(int64)
	}
	if ok {
		part, ok = t[4].(int64)
	}
	if !ok || part != 0 || !id.valid() {
		return messageID{}, fail("corruption", "message dedup index", "malformed message hash index entry")
	}
	return id, nil
}

// readMessagePresence reads, in one snapshot transaction, which elements are
// already stored. It is a hint: writes recheck presence.
func (b *FDBBackend) readMessagePresence(ctx context.Context, tenant int, elements []*messageElement) (map[messageDigest]messageID, error) {
	const op = "message presence"
	var found map[messageDigest]messageID
	_, err := runTxnSlot(ctx, b.db, op, func(rc *recordlayer.FDBRecordContext) (any, error) {
		found = map[messageDigest]messageID{}
		if err := b.readManifest(rc, true); err != nil {
			return nil, err
		}
		store, err := b.open(rc)
		if err != nil {
			return nil, err
		}
		snap := rc.Transaction().Snapshot()
		hashSpace := store.IndexSubspace(b.md.GetIndex(messageHashIndexName)).Bytes()
		hashes := make([]fdb.RangeResult, len(elements))
		for i, el := range elements {
			r, err := hashIndexRange(hashSpace, tenant, el.digest)
			if err != nil {
				return nil, err
			}
			hashes[i] = snap.GetRange(r, fdb.RangeOptions{Limit: 1})
		}
		for i, el := range elements {
			rows, err := hashes[i].GetSliceWithError()
			if err != nil {
				return nil, err
			}
			if len(rows) > 0 {
				id, err := decodeHashEntry(hashSpace, rows[0])
				if err != nil {
					return nil, err
				}
				found[el.digest] = id
			}
		}
		return nil, nil
	})
	return found, err
}

// messageTxn is one message transaction's body; it runs with the store open
// and a writer held, and returns its counters.
type messageTxn func(store *recordlayer.FDBRecordStore, hashSpace []byte) (MessageStoreStats, error)

func (b *FDBBackend) probeMessageTxn(stage string, store *recordlayer.FDBRecordStore) error {
	if b.messageTxnHook != nil {
		return b.messageTxnHook(stage, store)
	}
	return nil
}

// runMessageTxn runs body in one transaction, counting retries and the
// committed attempt's BunchedMap work.
func (b *FDBBackend) runMessageTxn(ctx context.Context, stats *MessageStoreStats, mu *sync.Mutex, body messageTxn) error {
	const op = "message persist"
	var got MessageStoreStats
	var attempts int64
	started := time.Now()
	_, err := runTxnSlot(ctx, b.db, op, func(rc *recordlayer.FDBRecordContext) (any, error) {
		attempts++
		timer := recordlayer.NewStoreTimer()
		rc.SetTimer(timer)
		if err := b.probeMessageTxn("message-before-write", nil); err != nil {
			return nil, err
		}
		if err := b.readManifest(rc, true); err != nil {
			return nil, err
		}
		store, err := b.open(rc)
		if err != nil {
			return nil, err
		}
		if got, err = body(store, store.IndexSubspace(b.md.GetIndex(messageHashIndexName)).Bytes()); err != nil {
			return nil, err
		}
		messageTimerStats(timer, &got)
		return nil, b.probeMessageTxn("message-before-commit", store)
	})
	mu.Lock()
	defer mu.Unlock()
	stats.Retries += max(attempts-1, 0)
	stats.TxnNs += time.Since(started).Nanoseconds()
	if err == nil {
		got.Transactions = 1
		stats.Add(got)
	}
	return err
}

// readHashes issues serializable dedup-index reads for plans' digests, so a
// concurrent writer of the same message conflicts with this transaction.
func readHashes(store *recordlayer.FDBRecordStore, hashSpace []byte, tenant int, plans []*messagePlan) ([]messageID, error) {
	pending := make([]fdb.RangeResult, len(plans))
	for i, p := range plans {
		r, err := hashIndexRange(hashSpace, tenant, p.element.digest)
		if err != nil {
			return nil, err
		}
		pending[i] = store.Context().Transaction().GetRange(r, fdb.RangeOptions{Limit: 1})
	}
	out := make([]messageID, len(plans))
	for i := range plans {
		rows, err := pending[i].GetSliceWithError()
		if err != nil {
			return nil, err
		}
		if len(rows) > 0 {
			if out[i], err = decodeHashEntry(hashSpace, rows[0]); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// writeMessages sets IDs after storing absent elements via group commit or multipart tails-first writes.
// A committed part 0 therefore denotes a complete message.
func (b *FDBBackend) writeMessages(ctx context.Context, tenant int, plans []*messagePlan, fulltext bool) (MessageStoreStats, error) {
	const op = "message persist"
	// Queue a trace’s messages together to share rare-token bunch reads within one transaction.
	var large []*messagePlan
	var byTrace [][]*messagePlan
	index := map[string]int{}
	for _, p := range plans {
		if len(p.records) > 1 {
			large = append(large, p)
			continue
		}
		i, ok := index[p.element.trace]
		if !ok {
			i = len(byTrace)
			index[p.element.trace] = i
			byTrace = append(byTrace, nil)
		}
		byTrace[i] = append(byTrace[i], p)
	}
	var small []*messagePlan
	for _, unit := range byTrace {
		small = append(small, unit...)
	}
	var stats MessageStoreStats
	var smallErr error
	var wg sync.WaitGroup
	if len(small) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stats, smallErr = b.queue.store(ctx, b, tenant, small, fulltext)
		}()
	}
	var mu sync.Mutex
	largeStats := MessageStoreStats{}
	err := runMessageJobs(ctx, op, len(large), func(ctx context.Context, i int) error {
		w, err := b.acquireWriter(ctx)
		if err != nil {
			return err
		}
		defer b.releaseWriter(w)
		p := large[i]
		return b.writeWithFallback(ctx, tenant, w, []*messagePlan{p}, fulltext, &largeStats, &mu, func() error {
			return b.writeLargeMessage(ctx, tenant, w, p, &largeStats, &mu)
		})
	})
	wg.Wait()
	stats.Add(largeStats)
	if err == nil {
		err = smallErr
	}
	return stats, err
}

// store waits for queued plans to settle or ctx to end; shared transactions use ctx values
// without cancellation because they may contain other callers’ messages.
func (q *messageQueue) store(ctx context.Context, b *FDBBackend, tenant int, plans []*messagePlan, fulltext bool) (MessageStoreStats, error) {
	sub := &messageSubmission{left: len(plans), done: make(chan struct{})}
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return MessageStoreStats{}, fail("operation", "message persist", "message queue is closed")
	}
	for _, p := range plans {
		q.pending = append(q.pending, queuedMessage{p, sub})
	}
	start := min(messageWriterLimit-q.loops, (len(q.pending)+messageTxnMessages-1)/messageTxnMessages)
	q.loops += max(start, 0)
	q.workers.Add(max(start, 0))
	q.mu.Unlock()
	loopCtx := context.WithoutCancel(ctx)
	for range start {
		go q.loop(loopCtx, b, tenant, fulltext)
	}
	select {
	case <-sub.done:
		q.mu.Lock()
		defer q.mu.Unlock()
		return sub.stats, sub.err
	case <-ctx.Done():
		err := wrap("canceled", "message persist", ctx.Err())
		q.mu.Lock()
		defer q.mu.Unlock()
		if sub.err == nil && sub.left > 0 {
			// The loops drop its messages still queued.
			sub.err = err
			close(sub.done)
		}
		return MessageStoreStats{}, err
	}
}

// loop releases its writer after each transaction so waiting large-message jobs can acquire it;
// messages queued meanwhile join the next transaction.
func (q *messageQueue) loop(ctx context.Context, b *FDBBackend, tenant int, fulltext bool) {
	defer q.workers.Done()
	for {
		w, err := b.acquireWriter(ctx)
		batch := q.take()
		if len(batch) == 0 {
			if err == nil {
				b.releaseWriter(w)
			}
			return
		}
		if err != nil {
			q.finish(batch, MessageStoreStats{}, err)
			continue
		}
		plans := make([]*messagePlan, len(batch))
		for i, m := range batch {
			plans[i] = m.plan
		}
		var mu sync.Mutex
		var stats MessageStoreStats
		err = b.writeWithFallback(ctx, tenant, w, plans, fulltext, &stats, &mu, func() error {
			return b.writeSmallMessages(ctx, tenant, w, plans, &stats, &mu)
		})
		b.releaseWriter(w)
		q.finish(batch, stats, err)
	}
}

// writeWithFallback runs write and, while it fails with a transaction over
// FDB's size limit, stores plans again cut into smaller segments.
func (b *FDBBackend) writeWithFallback(ctx context.Context, tenant int, w *messageWriter, plans []*messagePlan, fulltext bool, stats *MessageStoreStats, mu *sync.Mutex, write func() error) error {
	err := write()
	for distinct := messageSegmentTokens / 4; fulltext && distinct >= messageSegmentMinTokens && isTransactionTooLarge(err); distinct /= 4 {
		for _, p := range plans {
			if p, err = planMessage(tenant, p.element, true, distinct); err != nil {
				return err
			}
			if err = b.writeLargeMessage(ctx, tenant, w, p, stats, mu); err != nil {
				break
			}
		}
	}
	return err
}

// isTransactionTooLarge reports FDB's transaction_too_large (2101).
func isTransactionTooLarge(err error) bool {
	code, ok := fdbErrorCode(err)
	return ok && code == 2101
}

// writeSmallMessages saves single-part messages in one transaction, numbering
// the absent ones in w.
func (b *FDBBackend) writeSmallMessages(ctx context.Context, tenant int, w *messageWriter, plans []*messagePlan, stats *MessageStoreStats, mu *sync.Mutex) error {
	const op = "message persist"
	// Failed in-flight claims can put the same digest in multiple submissions.
	original := plans
	unique := make([]*messagePlan, 0, len(plans))
	byDigest := make(map[messageDigest]*messagePlan, len(plans))
	for _, p := range plans {
		if byDigest[p.element.digest] == nil {
			byDigest[p.element.digest] = p
			unique = append(unique, p)
		}
	}
	plans = unique
	base := w.next
	// A committed-but-unknown attempt may use every ID even when the retry
	// finds only dedup hits. Reserve the whole range before any attempt.
	w.next = base + int64(len(plans))
	err := b.runMessageTxn(ctx, stats, mu, func(store *recordlayer.FDBRecordStore, hashSpace []byte) (MessageStoreStats, error) {
		var s MessageStoreStats
		found, err := readHashes(store, hashSpace, tenant, plans)
		if err != nil {
			return s, err
		}
		seq := base
		var absent []*messagePlan
		var records []proto.Message
		for k, p := range plans {
			p.element.written = false
			if found[k].valid() {
				p.element.id = found[k]
				s.Present++
				continue
			}
			seq++
			p.element.id = messageID{w.id, seq}
			setMessageID(p.records[0], p.element.id)
			absent = append(absent, p)
			records = append(records, p.records[0])
		}
		if err := canceled(ctx, op); err != nil {
			return s, err
		}
		// One batch: the existence reads and the TEXT bunch reads each take
		// one round trip for all records.
		if _, err := store.SaveRecordBatch(records); err != nil {
			return s, err
		}
		for _, p := range absent {
			p.element.written = true
			s.Written++
			s.Parts++
			s.Postings += p.postings
			s.UnindexedTokens += p.unindexed
		}
		return s, nil
	})
	if err == nil {
		for _, p := range plans {
			if p.element.written {
				p.element.outcome = MessageStoreStats{Written: 1, Parts: 1, Postings: p.postings, UnindexedTokens: p.unindexed}
			} else {
				p.element.outcome.Present = 1
			}
		}
		for _, p := range original {
			canonical := byDigest[p.element.digest]
			if canonical != p {
				p.element.id = canonical.element.id
				p.element.written = false
				p.element.outcome.Present = 1
			}
		}
	}
	return err
}

// writeLargeMessage groups tails below messagePartTxnBytes, then publishes part 0.
// Each transaction checks the digest; size errors trigger finer replanning.
func (b *FDBBackend) writeLargeMessage(ctx context.Context, tenant int, w *messageWriter, p *messagePlan, stats *MessageStoreStats, mu *sync.Mutex) error {
	const op = "message persist"
	if p.element.outcome.Written != 0 || p.element.outcome.Present != 0 {
		return nil // an earlier fallback pass already committed this outcome
	}
	var committed MessageStoreStats
	var committedMu sync.Mutex
	defer func() {
		p.element.outcome.Parts += committed.Parts
		p.element.outcome.Postings += committed.Postings
		p.element.outcome.UnindexedTokens += committed.UnindexedTokens
		if committed.Written > 0 {
			p.element.outcome.Written = 1
		} else if committed.Present > 0 {
			p.element.outcome.Present = 1
		}
		mu.Lock()
		stats.Add(committed)
		mu.Unlock()
	}()
	p.element.written = false
	w.next++
	id := messageID{w.id, w.next}
	for _, r := range p.records {
		setMessageID(r, id)
	}
	present := false
	check := func(store *recordlayer.FDBRecordStore, hashSpace []byte) (bool, error) {
		found, err := readHashes(store, hashSpace, tenant, []*messagePlan{p})
		if err != nil || !found[0].valid() {
			return false, err
		}
		p.element.id = found[0]
		return true, nil
	}
	for next := 1; next < len(p.records) && !present; {
		lo := next
		err := b.runMessageTxn(ctx, &committed, &committedMu, func(store *recordlayer.FDBRecordStore, hashSpace []byte) (MessageStoreStats, error) {
			next = lo
			var err error
			if present, err = check(store, hashSpace); err != nil || present {
				return MessageStoreStats{Present: 1}, err
			}
			var s MessageStoreStats
			for next < len(p.records) {
				if err := canceled(ctx, op); err != nil {
					return s, err
				}
				if _, err := store.SaveRecord(p.records[next]); err != nil {
					return s, err
				}
				next++
				s.Parts++
				n, err := store.Context().Transaction().GetApproximateSize().Get()
				if err != nil {
					return s, err
				}
				if n >= messagePartTxnBytes {
					break
				}
			}
			return s, nil
		})
		if err != nil {
			return err
		}
	}
	if present {
		return nil
	}
	return b.runMessageTxn(ctx, &committed, &committedMu, func(store *recordlayer.FDBRecordStore, hashSpace []byte) (MessageStoreStats, error) {
		present, err := check(store, hashSpace)
		if err != nil || present {
			return MessageStoreStats{Present: 1}, err
		}
		if _, err := store.SaveRecord(p.records[0]); err != nil {
			return MessageStoreStats{}, err
		}
		p.element.id, p.element.written = id, true
		return MessageStoreStats{Written: 1, Parts: 1, Postings: p.postings, UnindexedTokens: p.unindexed}, nil
	})
}

// ---------------------------------------------------------------------------
// Span back references
// ---------------------------------------------------------------------------

// setEnvelopeRefs lists a span's references in its Envelope, packed.
func setEnvelopeRefs(m proto.Message, uses []messageUse) {
	r := m.ProtoReflect()
	field := r.Descriptor().Fields().ByName("ref_chunks")
	r.Clear(field)
	chunks := packSpanRefs(uses)
	if len(chunks) == 0 {
		return
	}
	list := r.Mutable(field).List()
	for i, c := range chunks {
		e := list.NewElement().Message()
		f := e.Descriptor().Fields()
		e.Set(f.ByName("chunk"), protoreflect.ValueOfInt64(int64(i)))
		e.Set(f.ByName("refs"), protoreflect.ValueOfBytes(c))
		list.Append(protoreflect.ValueOfMessage(e))
	}
}

// persistTraceMarkers saves absent (message, trace) markers before any span commits.
// Snapshot presence checks are safe: concurrent saves conflict on their own existence reads.
func (e *Exporter) persistTraceMarkers(ctx context.Context, d *messageDecomposition) (int64, error) {
	const op = "message trace markers"
	b := e.backend
	tenant := int64(e.cfg.Tenant)
	seen := map[traceMarker]bool{}
	var pending []traceMarker
	for key, uses := range d.uses {
		trace := key[:32]
		for _, u := range uses {
			k := traceMarker{u.element.id, trace}
			if !k.id.valid() {
				return 0, fail("operation", op, "message element has no stored ID")
			}
			if !seen[k] && !b.markers.has(k) {
				seen[k] = true
				pending = append(pending, k)
			}
		}
	}
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].id != pending[j].id {
			return pending[i].id.less(pending[j].id)
		}
		return pending[i].trace < pending[j].trace
	})
	d2, err := recordFile(e.cfg.messageLayout())
	if err != nil {
		return 0, err
	}
	desc := d2.Messages().ByName("MessageTrace")
	var written int64
	for lo := 0; lo < len(pending); lo += messageMarkerTxn {
		group := pending[lo:min(lo+messageMarkerTxn, len(pending))]
		var n int64
		_, err := runTxnSlot(ctx, b.db, op, func(rc *recordlayer.FDBRecordContext) (any, error) {
			n = 0
			if err := b.probeMessageTxn("marker-before-write", nil); err != nil {
				return nil, err
			}
			if err := b.readManifest(rc, true); err != nil {
				return nil, err
			}
			store, err := b.open(rc)
			if err != nil {
				return nil, err
			}
			records := store.RecordsSubspace()
			reads := make([]fdb.RangeResult, len(group))
			for i, k := range group {
				r, err := fdb.PrefixRange(records.Pack(tuple.Tuple{messageTraceTypeKey, tenant, k.id.writer, k.id.seq, k.trace}))
				if err != nil {
					return nil, err
				}
				reads[i] = rc.Transaction().Snapshot().GetRange(r, fdb.RangeOptions{Limit: 1})
			}
			var absent []proto.Message
			for i, k := range group {
				rows, err := reads[i].GetSliceWithError()
				if err != nil {
					return nil, err
				}
				if len(rows) > 0 {
					continue
				}
				m := dynamicpb.NewMessage(desc)
				f := desc.Fields()
				m.Set(f.ByName("tenant"), protoreflect.ValueOfInt64(tenant))
				m.Set(f.ByName("writer"), protoreflect.ValueOfInt64(k.id.writer))
				m.Set(f.ByName("seq"), protoreflect.ValueOfInt64(k.id.seq))
				m.Set(f.ByName("trace_id"), protoreflect.ValueOfString(k.trace))
				absent = append(absent, m)
			}
			if len(absent) == 0 {
				return nil, nil
			}
			n = int64(len(absent))
			_, err = store.SaveRecordBatch(absent)
			return nil, err
		})
		if err != nil {
			return written, err
		}
		written += n
		b.markers.add(group)
	}
	return written, nil
}

// persistMessages assigns IDs before spans commit and awaits same-backend in-flight elements.
// Cross-backend writers still resolve duplicates through the unique digest index.
func (e *Exporter) persistMessages(ctx context.Context, d *messageDecomposition) (stats MessageStoreStats, err error) {
	b := e.backend
	if len(d.elements) == 0 {
		return stats, nil
	}
	started := time.Now()
	defer func() { stats.WallNs = time.Since(started).Nanoseconds() }()
	found, err := b.readMessagePresence(ctx, e.cfg.Tenant, d.elements)
	if err != nil {
		return stats, err
	}
	stats.Transactions++
	var owned, awaited []*messageElement
	var waits []*inflightMessage
	var claims []*inflightMessage
	for _, el := range d.elements {
		if id, ok := found[el.digest]; ok {
			el.id = id
			stats.Present++
			continue
		}
		claim := &inflightMessage{done: make(chan struct{})}
		if other, loaded := b.inflight.LoadOrStore(el.digest, claim); loaded {
			if err := b.probeMessageTxn("message-await", nil); err != nil {
				return stats, err
			}
			awaited = append(awaited, el)
			waits = append(waits, other.(*inflightMessage))
			continue
		}
		owned = append(owned, el)
		claims = append(claims, claim)
	}
	written, err := e.storeMessages(ctx, owned)
	stats.Add(written)
	// Release claims before waiting to avoid cycles; after failure, waiters must store again
	// because the assigned ID may belong to an uncommitted attempt.
	for i, el := range owned {
		if err == nil {
			claims[i].id = el.id
		}
		b.inflight.Delete(el.digest)
		close(claims[i].done)
	}
	if err != nil {
		return stats, err
	}
	var retry []*messageElement
	for i, el := range awaited {
		select {
		case <-waits[i].done:
		case <-ctx.Done():
			return stats, wrap("canceled", "message persist", ctx.Err())
		}
		if el.id = waits[i].id; el.id.valid() {
			stats.Present++
		} else {
			retry = append(retry, el) // the other call failed
		}
	}
	written, err = e.storeMessages(ctx, retry)
	stats.Add(written)
	return stats, err
}

// storeMessages plans every element before the first write, so an admission
// failure leaves nothing behind, then writes them.
func (e *Exporter) storeMessages(ctx context.Context, elements []*messageElement) (MessageStoreStats, error) {
	if len(elements) == 0 {
		return MessageStoreStats{}, nil
	}
	fulltext := e.cfg.messageLayout() == messagesFullText
	plans := make([]*messagePlan, len(elements))
	if err := parallelMessageWork(len(elements), func(i int) error {
		var err error
		plans[i], err = planMessage(e.cfg.Tenant, elements[i], fulltext, messageSegmentTokens)
		return err
	}); err != nil {
		return MessageStoreStats{}, err
	}
	if err := canceled(ctx, "message persist"); err != nil {
		return MessageStoreStats{}, err
	}
	return e.backend.writeMessages(ctx, e.cfg.Tenant, plans, fulltext)
}

// ---------------------------------------------------------------------------
// Reconstruction
// ---------------------------------------------------------------------------

// reconstructMessages restores spans in two read waves: referenced heads first,
// then remaining raw parts of multipart messages, within the caller’s transaction.
func reconstructMessages(ctx context.Context, store *recordlayer.FDBRecordStore, tenant int, spans []*tracepb.ResourceSpans) ([]*tracepb.ResourceSpans, error) {
	p, err := planReconstruct(ctx, spans)
	if err != nil {
		return nil, err
	}
	raws, err := loadMessages(store, tenant, p.refs, p.encodings)
	if err != nil {
		return nil, err
	}
	return p.apply(raws)
}

// loadMessages returns the raw element bytes of each message.
func loadMessages(store *recordlayer.FDBRecordStore, tenant int, refs []messageID, encodings map[messageID]string) (map[messageID][]byte, error) {
	const op = "message reconstruct"
	heads := make([]recordlayer.PendingRead[*recordlayer.FDBStoredRecord[proto.Message]], len(refs))
	for i, id := range refs {
		heads[i] = store.LoadRecordAsync(messageKey(tenant, id, 0), nil)
	}
	type rest struct {
		id    messageID
		size  int64
		parts []recordlayer.PendingRead[*recordlayer.FDBStoredRecord[proto.Message]]
	}
	var more []rest
	out := make(map[messageID][]byte, len(refs))
	for i, id := range refs {
		r, err := heads[i].Get()
		if err != nil {
			return nil, err
		}
		if r == nil {
			return nil, failf("corruption", op, "missing message %d/%d", id.writer, id.seq)
		}
		m := r.Record.ProtoReflect()
		f := m.Descriptor().Fields()
		parts, size := m.Get(f.ByName("parts")).Int(), m.Get(f.ByName("raw_bytes")).Int()
		if m.Get(f.ByName("encoding")).String() != encodings[id] || parts < 1 || size < 0 || size > maxMessageBytes {
			return nil, failf("corruption", op, "message %d/%d header disagrees with its manifest", id.writer, id.seq)
		}
		raw := m.Get(f.ByName("raw")).Bytes()
		if int64(len(raw)) == size {
			out[id] = raw
			continue
		}
		x := rest{id: id, size: size}
		out[id] = append([]byte(nil), raw...)
		for p := int64(1); p < parts && p*messagePartRawBytes < size; p++ {
			x.parts = append(x.parts, store.LoadRecordAsync(messageKey(tenant, id, int(p)), nil))
		}
		more = append(more, x)
	}
	for _, x := range more {
		for _, pr := range x.parts {
			r, err := pr.Get()
			if err != nil {
				return nil, err
			}
			if r == nil {
				return nil, failf("corruption", op, "missing part of message %d/%d", x.id.writer, x.id.seq)
			}
			m := r.Record.ProtoReflect()
			out[x.id] = append(out[x.id], m.Get(m.Descriptor().Fields().ByName("raw")).Bytes()...)
		}
		if int64(len(out[x.id])) != x.size {
			return nil, failf("corruption", op, "message %d/%d has %d raw bytes, header says %d", x.id.writer, x.id.seq, len(out[x.id]), x.size)
		}
	}
	return out, nil
}
