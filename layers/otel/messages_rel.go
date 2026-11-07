package otel

// GenAI message storage on a tenant store (rfcs/relational-exporter-migration.md
// §1.5, §3.1): messages.go's design on OTEL_V1 rows. Unique elements are
// MESSAGES rows deduplicated by MESSAGES_HASH (content_hash in the v2 domain,
// the store being the tenant); writers number them (writer, seq) from the
// STORE_META("message_writer") counter, with shard = writer % 16, so
// MESSAGES_TEXT postings append. Small messages share group commits, large
// ones commit their tails first and part 0 last, and MESSAGE_TRACES markers
// precede the spans whose SPANS_REF_CHUNKS reference them.
//
// Message rows are immutable, shared by every span of the store that
// references them, and never collected.

import (
	"bytes"
	"context"
	"encoding/hex"
	"sort"
	"sync"
	"time"

	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/layers/otel/otelsql/otelv1pb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// OTEL_V1 message tables and indexes.
const (
	messagesRecordType      = "MESSAGES"
	messageTracesRecordType = "MESSAGE_TRACES"
	messagesHashIndex       = "MESSAGES_HASH"    // content_hash, unique
	messagesTextIndex       = "MESSAGES_TEXT"    // FULLTEXT content, groups (role, shard)
	messagesLargeIndex      = "MESSAGES_LARGE"   // ("L", writer, seq) of multipart text
	spansRefChunksIndex     = "SPANS_REF_CHUNKS" // trace_id, start_ns, span_id, chunk, refs
)

// relMessageStore stores the GenAI messages of one tenant store's exporter:
// its writers, group-commit queue, in-flight claims and marker cache are
// this exporter's; other exporters of the store dedup through MESSAGES_HASH.
type relMessageStore struct {
	store    *tenantStore
	fullText bool
	writers  messageWriters
	queue    messageQueue
	inflight sync.Map // messageDigest -> *inflightMessage
	markers  markerCache
	// hook, when set, runs at named stages of the message transactions;
	// tests use it to interleave failures and concurrent work.
	hook func(stage string, store *recordlayer.FDBRecordStore) error
}

// newRelMessageStore stores messages on s; fullText also indexes their text,
// writes trace markers, and sets the message pipeline sizer on every open of
// s, as FDBBackend does for the fulltext layout.
func newRelMessageStore(s *tenantStore, fullText bool) *relMessageStore {
	s.fullText = fullText
	return &relMessageStore{store: s, fullText: fullText}
}

// close rejects new group commits and joins the queue's workers; it does
// not cancel the shared transactions in flight.
func (m *relMessageStore) close() { m.queue.close() }

func (m *relMessageStore) probe(stage string, store *recordlayer.FDBRecordStore) error {
	if m.hook != nil {
		return m.hook(stage, store)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Rows
// ---------------------------------------------------------------------------

// compiledRecord is a loaded record as its compiled otelv1pb type, which a
// store opened with the compiled metadata decodes it as.
func compiledRecord[T proto.Message](op string, rec *recordlayer.FDBStoredRecord[proto.Message]) (T, error) {
	out, ok := rec.Record.(T)
	if !ok {
		return out, failf("ownership", op, "record %v is %T, not the compiled OTEL_V1 type", rec.PrimaryKey, rec.Record)
	}
	return out, nil
}

// messagePK reads (writer, seq, part) from a MESSAGES primary key.
func messagePK(md *recordlayer.RecordMetaData, op string, pk tuple.Tuple) (messageID, int64, error) {
	bad := failf("corruption", op, "malformed message primary key %v", pk)
	rt := md.GetRecordType(messagesRecordType)
	if rt == nil {
		return messageID{}, 0, failf("ownership", op, "%s is not in the store's metadata", messagesRecordType)
	}
	if rt.PrimaryKeyHasRecordTypePrefix() {
		if len(pk) == 0 || pk[0] != rt.GetRecordTypeKey() {
			return messageID{}, 0, bad
		}
		pk = pk[1:]
	}
	if len(pk) != 3 {
		return messageID{}, 0, bad
	}
	w, wok := pk[0].(int64)
	s, sok := pk[1].(int64)
	part, pok := pk[2].(int64)
	id := messageID{w, s}
	if !wok || !sok || !pok || !id.valid() || part < 0 {
		return messageID{}, 0, bad
	}
	return id, part, nil
}

// planRelMessage is planMessage on MESSAGES rows: raw chunk i and text
// segment i form part i, every text part repeats the role, and part 0
// carries the content hash and the whole message's columns. The rows carry
// a placeholder ID until setRelMessageID assigns the stored one.
func planRelMessage(el *messageElement, hash []byte, fullText bool, distinct int) (*messagePlan, error) {
	const op = "message records"
	mp, err := splitMessageParts(el, fullText, distinct)
	if err != nil {
		return nil, err
	}
	p := &messagePlan{element: el, unindexed: mp.unindexed}
	parts := max(len(mp.chunks), len(mp.segments), 1)
	for i := range parts {
		part := otelsql.MessagePart{Writer: 1, Seq: 1, Part: int64(i)}
		var seg *messageSegment
		if i < len(mp.segments) {
			seg = &mp.segments[i]
			part.Text = &otelsql.MessageText{Role: mp.role, Content: seg.text, TokenOffset: int64(seg.offset)}
		}
		if i < len(mp.chunks) {
			part.Raw = mp.chunks[i]
			p.raw += int64(len(mp.chunks[i]))
		}
		if i == 0 {
			part.ContentHash = hash
			part.Encoding = el.encoding
			part.RawBytes = int64(len(el.raw))
			part.Parts = int64(parts)
			part.UnindexedTokens = p.unindexed
			part.Large = fullText && len(mp.segments) > 1
		}
		rec, err := otelsql.BuildMessageCompiled(part)
		if err != nil {
			return nil, typedError("admission", op, "message row", err)
		}
		if seg != nil {
			n := int64(seg.postings)
			if rec.GetCONTENT() != seg.text {
				// The content holds OverlongTokenPlaceholder for overlong tokens,
				// which MESSAGES_TEXT indexes: count its postings as stored.
				if n, err = distinctTextTokens(rec.GetCONTENT()); err != nil {
					return nil, err
				}
			}
			p.postings += n
		}
		p.records = append(p.records, rec)
	}
	return p, nil
}

// distinctTextTokens counts the distinct tokens MESSAGES_TEXT (the default
// tokenizer) indexes for content.
func distinctTextTokens(content string) (int64, error) {
	m, err := recordlayer.DefaultTextTokenizerInstance().TokenizeToMap(content, recordlayer.GlobalMinVersion, recordlayer.TokenizerModeIndex)
	if err != nil {
		return 0, typedError("config", "message text segments", "tokenizer", err)
	}
	return int64(len(m)), nil
}

// setRelMessageID gives a planned row its stored ID and the writer's shard.
func setRelMessageID(m proto.Message, id messageID) {
	r := m.(*otelv1pb.MESSAGES)
	r.WRITER, r.SEQ, r.SHARD = proto.Int64(id.writer), proto.Int64(id.seq), proto.Int64(otelsql.MessageShard(id.writer))
}

// planHash is the content hash of a plan's element, held by its part 0.
func planHash(p *messagePlan) []byte {
	return p.records[0].(*otelv1pb.MESSAGES).GetCONTENT_HASH()
}

// ---------------------------------------------------------------------------
// Writers
// ---------------------------------------------------------------------------

// acquireWriter waits for a writer slot and reuses a free writer or
// allocates one in a free shard of this exporter; the store-wide counter
// keeps writer IDs unique across exporters.
func (m *relMessageStore) acquireWriter(ctx context.Context) (*messageWriter, error) {
	m.writers.once.Do(func() { m.writers.slots = make(chan struct{}, messageWriterLimit) })
	select {
	case m.writers.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, wrap("canceled", "message writer", ctx.Err())
	}
	w, err := m.takeWriter(ctx)
	if err != nil {
		<-m.writers.slots
	}
	return w, err
}

func (m *relMessageStore) takeWriter(ctx context.Context) (*messageWriter, error) {
	const op = "message writer"
	m.writers.mu.Lock()
	if n := len(m.writers.free); n > 0 {
		w := m.writers.free[n-1]
		m.writers.free = m.writers.free[:n-1]
		m.writers.mu.Unlock()
		return w, nil
	}
	// The writer slots bound the writers to messageTextShards, so a shard is free.
	shard := int64(0)
	for m.writers.shards[shard] {
		shard++
	}
	m.writers.shards[shard] = true
	m.writers.mu.Unlock()
	var id int64
	err := m.store.run(ctx, op, func(store *recordlayer.FDBRecordStore) error {
		if err := m.probe("writer-before-write", store); err != nil {
			return err
		}
		// A read-modify-write in this transaction: the serializable read
		// conflicts with a concurrent allocator's, so IDs never repeat.
		row, ok, err := loadStoreMeta(store, otelsql.StoreMetaMessageWriter)
		if err != nil {
			return err
		}
		last := int64(0)
		if ok {
			if row.Int == nil || *row.Int < 1 {
				return fail("corruption", op, "malformed message writer counter")
			}
			last = *row.Int
		}
		id = nextWriterID(last, shard)
		return saveStoreMeta(store, otelsql.StoreMeta{Name: otelsql.StoreMetaMessageWriter, Int: &id})
	})
	if err != nil {
		m.writers.mu.Lock()
		m.writers.shards[shard] = false
		m.writers.mu.Unlock()
		return nil, err
	}
	return &messageWriter{id: id}, nil
}

func (m *relMessageStore) releaseWriter(w *messageWriter) {
	m.writers.mu.Lock()
	m.writers.free = append(m.writers.free, w)
	m.writers.mu.Unlock()
	<-m.writers.slots
}

// ---------------------------------------------------------------------------
// Dedup reads
// ---------------------------------------------------------------------------

// hashRows issues one MESSAGES_HASH range read per content hash, snapshot or
// serializable, so a concurrent writer of the same message conflicts with a
// serializable reader.
func hashRows(store *recordlayer.FDBRecordStore, op string, hashes [][]byte, snapshot bool) (func(i int) (messageID, error), error) {
	md := store.GetRecordMetaData()
	idx := md.GetIndex(messagesHashIndex)
	if idx == nil {
		return nil, failf("ownership", op, "index %s is not in the store's metadata", messagesHashIndex)
	}
	space := store.IndexSubspace(idx)
	var tx fdb.ReadTransaction = store.Context().Transaction()
	if snapshot {
		tx = store.Context().Transaction().Snapshot()
	}
	pending := make([]fdb.RangeResult, len(hashes))
	for i, h := range hashes {
		r, err := fdb.PrefixRange(space.Pack(tuple.Tuple{h}))
		if err != nil {
			return nil, err
		}
		pending[i] = tx.GetRange(r, fdb.RangeOptions{Limit: 1})
	}
	return func(i int) (messageID, error) {
		rows, err := pending[i].GetSliceWithError()
		if err != nil || len(rows) == 0 {
			return messageID{}, err
		}
		key, err := space.Unpack(rows[0].Key)
		if err != nil || len(key) == 0 || !bytes.Equal(asBytes(key[0]), hashes[i]) {
			return messageID{}, fail("corruption", op, "malformed message hash index entry")
		}
		id, part, err := messagePK(md, op, idx.EntryPrimaryKey(key))
		if err != nil {
			return messageID{}, err
		}
		if part != 0 {
			return messageID{}, fail("corruption", op, "message hash index entry names a part other than 0")
		}
		return id, nil
	}, nil
}

func asBytes(v tuple.TupleElement) []byte {
	b, _ := v.([]byte)
	return b
}

// readMessagePresence reads, in one snapshot transaction, which elements are
// already stored. It is a hint: writes recheck presence.
func (m *relMessageStore) readMessagePresence(ctx context.Context, elements []*messageElement, hashes map[messageDigest][]byte) (map[messageDigest]messageID, error) {
	const op = "message presence"
	var found map[messageDigest]messageID
	err := m.store.read(ctx, op, func(store *recordlayer.FDBRecordStore) error {
		found = map[messageDigest]messageID{}
		hs := make([][]byte, len(elements))
		for i, el := range elements {
			hs[i] = hashes[el.digest]
		}
		get, err := hashRows(store, op, hs, true)
		if err != nil {
			return err
		}
		for i, el := range elements {
			id, err := get(i)
			if err != nil {
				return err
			}
			if id.valid() {
				found[el.digest] = id
			}
		}
		return nil
	})
	return found, err
}

// readPlanHashes is the serializable dedup read of plans' messages inside a
// write transaction.
func readPlanHashes(store *recordlayer.FDBRecordStore, plans []*messagePlan) ([]messageID, error) {
	const op = "message persist"
	hs := make([][]byte, len(plans))
	for i, p := range plans {
		hs[i] = planHash(p)
	}
	get, err := hashRows(store, op, hs, false)
	if err != nil {
		return nil, err
	}
	out := make([]messageID, len(plans))
	for i := range plans {
		if out[i], err = get(i); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

// persist stores d's unique elements, then (fulltext) their trace markers,
// before any span referencing them commits, and sets every element's ID.
// Elements already stored, or being stored by another call of this
// exporter, are not written again; other exporters' duplicates are resolved
// by the serializable MESSAGES_HASH read.
func (m *relMessageStore) persist(ctx context.Context, d *messageDecomposition) (stats MessageStoreStats, err error) {
	if d == nil || len(d.elements) == 0 {
		return stats, nil
	}
	started := time.Now()
	defer func() { stats.WallNs = time.Since(started).Nanoseconds() }()
	hashes := make([][]byte, len(d.elements))
	_ = parallelMessageWork(len(d.elements), func(i int) error {
		el := d.elements[i]
		hashes[i] = otelsql.MessageContentHash(el.encoding, el.raw)
		return nil
	})
	byDigest := make(map[messageDigest][]byte, len(d.elements))
	for i, el := range d.elements {
		byDigest[el.digest] = hashes[i]
	}
	found, err := m.readMessagePresence(ctx, d.elements, byDigest)
	if err != nil {
		return stats, err
	}
	stats.Transactions++
	var owned, awaited []*messageElement
	var waits, claims []*inflightMessage
	for _, el := range d.elements {
		if id, ok := found[el.digest]; ok {
			el.id = id
			stats.Present++
			continue
		}
		claim := &inflightMessage{done: make(chan struct{})}
		if other, loaded := m.inflight.LoadOrStore(el.digest, claim); loaded {
			if err := m.probe("message-await", nil); err != nil {
				return stats, err
			}
			awaited = append(awaited, el)
			waits = append(waits, other.(*inflightMessage))
			continue
		}
		owned = append(owned, el)
		claims = append(claims, claim)
	}
	written, err := m.storeMessages(ctx, owned, byDigest)
	stats.Add(written)
	// Release claims before waiting, so no two calls wait on each other; a
	// failed claim leaves no ID, as its ID may name an uncommitted attempt.
	for i, el := range owned {
		if err == nil {
			claims[i].id = el.id
		}
		m.inflight.Delete(el.digest)
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
	written, err = m.storeMessages(ctx, retry, byDigest)
	stats.Add(written)
	if err != nil || !m.fullText {
		return stats, err
	}
	markers, err := m.persistTraceMarkers(ctx, d)
	stats.TraceMarkers += markers
	return stats, err
}

// storeMessages plans every element before the first write, so an admission
// failure leaves nothing behind, then writes them.
func (m *relMessageStore) storeMessages(ctx context.Context, elements []*messageElement, hashes map[messageDigest][]byte) (MessageStoreStats, error) {
	if len(elements) == 0 {
		return MessageStoreStats{}, nil
	}
	plans := make([]*messagePlan, len(elements))
	if err := parallelMessageWork(len(elements), func(i int) error {
		var err error
		plans[i], err = planRelMessage(elements[i], hashes[elements[i].digest], m.fullText, messageSegmentTokens)
		return err
	}); err != nil {
		return MessageStoreStats{}, err
	}
	if err := canceled(ctx, "message persist"); err != nil {
		return MessageStoreStats{}, err
	}
	return m.writeMessages(ctx, plans)
}

// writeMessages stores single-part plans through the group-commit queue and
// multipart plans tails first, setting each element's ID. A committed part 0
// therefore denotes a complete message.
func (m *relMessageStore) writeMessages(ctx context.Context, plans []*messagePlan) (MessageStoreStats, error) {
	const op = "message persist"
	// A trace's messages queue together to share rare-token bunch reads.
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
			stats, smallErr = m.enqueue(ctx, small)
		}()
	}
	var mu sync.Mutex
	largeStats := MessageStoreStats{}
	err := runMessageJobs(ctx, op, len(large), func(ctx context.Context, i int) error {
		w, err := m.acquireWriter(ctx)
		if err != nil {
			return err
		}
		defer m.releaseWriter(w)
		p := large[i]
		return m.writeWithFallback(ctx, w, []*messagePlan{p}, &largeStats, &mu, func() error {
			return m.writeLargeMessage(ctx, w, p, &largeStats, &mu)
		})
	})
	wg.Wait()
	stats.Add(largeStats)
	if err == nil {
		err = smallErr
	}
	return stats, err
}

// enqueue queues plans for group commit and waits until they settle or ctx
// ends. The shared transactions run under ctx's values without its
// cancellation, as they hold other callers' messages; a canceled caller's
// still-queued messages are dropped.
func (m *relMessageStore) enqueue(ctx context.Context, plans []*messagePlan) (MessageStoreStats, error) {
	q := &m.queue
	sub := &messageSubmission{left: len(plans), done: make(chan struct{})}
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return MessageStoreStats{}, fail("operation", "message persist", "message queue is closed")
	}
	for _, p := range plans {
		q.pending = append(q.pending, queuedMessage{p, sub})
	}
	start := max(min(messageWriterLimit-q.loops, (len(q.pending)+messageTxnMessages-1)/messageTxnMessages), 0)
	q.loops += start
	q.workers.Add(start)
	q.mu.Unlock()
	loopCtx := context.WithoutCancel(ctx)
	for range start {
		go m.queueLoop(loopCtx)
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
			sub.err = err
			close(sub.done)
		}
		return MessageStoreStats{}, err
	}
}

// queueLoop commits queued messages one transaction at a time, releasing its
// writer after each so waiting large-message jobs can take it; messages
// queued meanwhile join the next transaction.
func (m *relMessageStore) queueLoop(ctx context.Context) {
	q := &m.queue
	defer q.workers.Done()
	for {
		w, err := m.acquireWriter(ctx)
		batch := q.take()
		if len(batch) == 0 {
			if err == nil {
				m.releaseWriter(w)
			}
			return
		}
		if err != nil {
			q.finish(batch, MessageStoreStats{}, err)
			continue
		}
		plans := make([]*messagePlan, len(batch))
		for i, qm := range batch {
			plans[i] = qm.plan
		}
		var mu sync.Mutex
		var stats MessageStoreStats
		err = m.writeWithFallback(ctx, w, plans, &stats, &mu, func() error {
			return m.writeSmallMessages(ctx, w, plans, &stats, &mu)
		})
		m.releaseWriter(w)
		q.finish(batch, stats, err)
	}
}

// writeWithFallback runs write and, while it fails with FDB's
// transaction_too_large, stores plans again as multipart messages cut at a
// quarter of the distinct tokens, down to messageSegmentMinTokens.
func (m *relMessageStore) writeWithFallback(ctx context.Context, w *messageWriter, plans []*messagePlan, stats *MessageStoreStats, mu *sync.Mutex, write func() error) error {
	err := write()
	for distinct := messageSegmentTokens / 4; m.fullText && distinct >= messageSegmentMinTokens && isTransactionTooLarge(err); distinct /= 4 {
		for _, p := range plans {
			if p, err = planRelMessage(p.element, planHash(p), true, distinct); err != nil {
				return err
			}
			if err = m.writeLargeMessage(ctx, w, p, stats, mu); err != nil {
				break
			}
		}
	}
	return err
}

// runMessageTxn runs body in one store transaction, counting retries and
// the committed attempt's TEXT index work.
func (m *relMessageStore) runMessageTxn(ctx context.Context, stats *MessageStoreStats, mu *sync.Mutex, body func(*recordlayer.FDBRecordStore) (MessageStoreStats, error)) error {
	const op = "message persist"
	var got MessageStoreStats
	var attempts int64
	started := time.Now()
	err := m.store.run(ctx, op, func(store *recordlayer.FDBRecordStore) error {
		attempts++
		timer := recordlayer.NewStoreTimer()
		store.Context().SetTimer(timer)
		if err := m.probe("message-before-write", store); err != nil {
			return err
		}
		var err error
		if got, err = body(store); err != nil {
			return err
		}
		messageTimerStats(timer, &got)
		return m.probe("message-before-commit", store)
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

// writeSmallMessages saves single-part messages in one transaction,
// numbering the absent ones in w.
func (m *relMessageStore) writeSmallMessages(ctx context.Context, w *messageWriter, plans []*messagePlan, stats *MessageStoreStats, mu *sync.Mutex) error {
	const op = "message persist"
	// Failed in-flight claims can put one digest in several submissions.
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
	// A commit whose outcome is unknown may have used every seq even when
	// the retry finds only dedup hits, so the whole range is reserved first.
	w.next = base + int64(len(plans))
	err := m.runMessageTxn(ctx, stats, mu, func(store *recordlayer.FDBRecordStore) (MessageStoreStats, error) {
		var s MessageStoreStats
		found, err := readPlanHashes(store, plans)
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
			setRelMessageID(p.records[0], p.element.id)
			absent = append(absent, p)
			records = append(records, p.records[0])
		}
		if err := canceled(ctx, op); err != nil {
			return s, err
		}
		// One batch: the existence reads and the TEXT bunch reads each take
		// one round trip for every record.
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
			if canonical := byDigest[p.element.digest]; canonical != p {
				p.element.id = canonical.element.id
				p.element.written = false
				p.element.outcome.Present = 1
			}
		}
	}
	return err
}

// writeLargeMessage commits the tails in transactions of up to
// messagePartTxnBytes, then part 0. Each transaction rechecks the hash; a
// message found stored ends the write. The message takes a fresh seq per
// call, so a failed attempt's tails are never published under it.
func (m *relMessageStore) writeLargeMessage(ctx context.Context, w *messageWriter, p *messagePlan, stats *MessageStoreStats, mu *sync.Mutex) error {
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
		setRelMessageID(r, id)
	}
	present := false
	check := func(store *recordlayer.FDBRecordStore) (bool, error) {
		found, err := readPlanHashes(store, []*messagePlan{p})
		if err != nil || !found[0].valid() {
			return false, err
		}
		p.element.id = found[0]
		return true, nil
	}
	for next := 1; next < len(p.records) && !present; {
		lo := next
		err := m.runMessageTxn(ctx, &committed, &committedMu, func(store *recordlayer.FDBRecordStore) (MessageStoreStats, error) {
			next = lo
			var err error
			if present, err = check(store); err != nil || present {
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
	return m.runMessageTxn(ctx, &committed, &committedMu, func(store *recordlayer.FDBRecordStore) (MessageStoreStats, error) {
		present, err := check(store)
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
// Trace markers and span references
// ---------------------------------------------------------------------------

// persistTraceMarkers saves the absent MESSAGE_TRACES markers of d's
// references before any span commits. The snapshot presence reads are safe:
// concurrent saves of one marker conflict on their own existence reads.
func (m *relMessageStore) persistTraceMarkers(ctx context.Context, d *messageDecomposition) (int64, error) {
	const op = "message trace markers"
	seen := map[traceMarker]bool{}
	var pending []traceMarker
	for key, uses := range d.uses {
		trace := key[:32]
		for _, u := range uses {
			k := traceMarker{u.element.id, trace}
			if !k.id.valid() {
				return 0, fail("operation", op, "message element has no stored ID")
			}
			if !seen[k] && !m.markers.has(k) {
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
	var written int64
	for lo := 0; lo < len(pending); lo += messageMarkerTxn {
		group := pending[lo:min(lo+messageMarkerTxn, len(pending))]
		var n int64
		err := m.store.run(ctx, op, func(store *recordlayer.FDBRecordStore) error {
			n = 0
			if err := m.probe("marker-before-write", store); err != nil {
				return err
			}
			records := store.RecordsSubspace()
			snap := store.Context().Transaction().Snapshot()
			reads := make([]fdb.RangeResult, len(group))
			traces := make([][]byte, len(group))
			for i, k := range group {
				trace, err := hex.DecodeString(k.trace)
				if err != nil {
					return wrap("operation", op, err)
				}
				traces[i] = trace
				pk, err := tableKey(store, op, messageTracesRecordType, k.id.writer, k.id.seq, trace)
				if err != nil {
					return err
				}
				r, err := fdb.PrefixRange(records.Pack(pk))
				if err != nil {
					return err
				}
				reads[i] = snap.GetRange(r, fdb.RangeOptions{Limit: 1})
			}
			var absent []proto.Message
			for i, k := range group {
				rows, err := reads[i].GetSliceWithError()
				if err != nil {
					return err
				}
				if len(rows) == 0 {
					absent = append(absent, otelsql.BuildMessageTraceCompiled(k.id.writer, k.id.seq, traces[i]))
				}
			}
			if len(absent) == 0 {
				return nil
			}
			n = int64(len(absent))
			_, err := store.SaveRecordBatch(absent)
			return err
		})
		if err != nil {
			return written, err
		}
		written += n
		m.markers.add(group)
	}
	return written, nil
}

// refChunks packs one span's references into SPANS.ref_chunks values. An
// element without a stored ID packs as messagePlaceholderID, which chunks as
// any real ID does (otelsql.PackRefChunks), so preflight sizing holds.
func (m *relMessageStore) refChunks(uses []messageUse) ([][]byte, error) {
	chunks, err := otelsql.PackRefChunks(messageRefs(uses))
	if err != nil {
		return nil, typedError("admission", "message references", "pack", err)
	}
	return chunks, nil
}

// messageRefs is uses as otelsql.MessageRef, for PackRefChunks or
// otelsql.Span.Refs.
func messageRefs(uses []messageUse) []otelsql.MessageRef {
	refs := make([]otelsql.MessageRef, len(uses))
	for i, u := range uses {
		id := u.element.id
		if !id.valid() {
			id = messagePlaceholderID
		}
		refs[i] = otelsql.MessageRef{Writer: id.writer, Seq: id.seq, Attribute: u.attribute, Newest: u.newest, Ordinals: u.ordinals}
	}
	return refs
}

// ---------------------------------------------------------------------------
// Reconstruction
// ---------------------------------------------------------------------------

// reconstruct restores, in place, the original attribute values of a loaded
// single-span ResourceSpans from the MESSAGES rows its manifests name, in
// two read waves (heads, then the remaining raw parts of multipart
// messages), counting every row read in work. LoadSpanOTLP has no budget
// (§1.3), so the count never fails the load: a span may hold 16 MiB elements.
func (m *relMessageStore) reconstruct(ctx context.Context, store *recordlayer.FDBRecordStore, rs *tracepb.ResourceSpans, work *queryWork) error {
	p, err := planReconstruct(ctx, []*tracepb.ResourceSpans{rs})
	if err != nil || len(p.sites) == 0 {
		return err
	}
	raws, err := loadRelMessages(store, p.refs, p.encodings, work)
	if err != nil {
		return err
	}
	out, err := p.apply(raws)
	if err != nil {
		return err
	}
	proto.Reset(rs)
	proto.Merge(rs, out[0])
	return nil
}

// loadRelMessages returns the raw element bytes of each message.
func loadRelMessages(store *recordlayer.FDBRecordStore, refs []messageID, encodings map[messageID]string, work *queryWork) (map[messageID][]byte, error) {
	const op = "message reconstruct"
	load := func(id messageID, part int64) (recordlayer.PendingRead[*recordlayer.FDBStoredRecord[proto.Message]], error) {
		pk, err := tableKey(store, op, messagesRecordType, id.writer, id.seq, part)
		if err != nil {
			return nil, err
		}
		return store.LoadRecordAsync(pk, nil), nil
	}
	get := func(f recordlayer.PendingRead[*recordlayer.FDBStoredRecord[proto.Message]], id messageID, what string) (*otelv1pb.MESSAGES, error) {
		r, err := f.Get()
		if err != nil {
			return nil, err
		}
		if r == nil {
			return nil, failf("corruption", op, "missing %s of message %d/%d", what, id.writer, id.seq)
		}
		work.candidates++
		work.bytes += int64(r.KeySize + r.ValueSize)
		return compiledRecord[*otelv1pb.MESSAGES](op, r)
	}
	heads := make([]recordlayer.PendingRead[*recordlayer.FDBStoredRecord[proto.Message]], len(refs))
	for i, id := range refs {
		var err error
		if heads[i], err = load(id, 0); err != nil {
			return nil, err
		}
	}
	type rest struct {
		id    messageID
		size  int64
		parts []recordlayer.PendingRead[*recordlayer.FDBStoredRecord[proto.Message]]
	}
	var more []rest
	out := make(map[messageID][]byte, len(refs))
	for i, id := range refs {
		h, err := get(heads[i], id, "head")
		if err != nil {
			return nil, err
		}
		parts, size := h.GetPARTS(), h.GetRAW_BYTES()
		if h.GetENCODING() != encodings[id] || parts < 1 || size < 0 || size > maxMessageBytes {
			return nil, failf("corruption", op, "message %d/%d header disagrees with its manifest", id.writer, id.seq)
		}
		if raw := h.GetRAW(); int64(len(raw)) == size {
			out[id] = raw
			continue
		}
		x := rest{id: id, size: size}
		out[id] = append([]byte(nil), h.GetRAW()...)
		for p := int64(1); p < parts && p*messagePartRawBytes < size; p++ {
			f, err := load(id, p)
			if err != nil {
				return nil, err
			}
			x.parts = append(x.parts, f)
		}
		more = append(more, x)
	}
	for _, x := range more {
		for _, f := range x.parts {
			r, err := get(f, x.id, "part")
			if err != nil {
				return nil, err
			}
			out[x.id] = append(out[x.id], r.GetRAW()...)
		}
		if int64(len(out[x.id])) != x.size {
			return nil, failf("corruption", op, "message %d/%d has %d raw bytes, header says %d", x.id.writer, x.id.seq, len(out[x.id]), x.size)
		}
	}
	return out, nil
}
