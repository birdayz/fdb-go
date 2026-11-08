package otel

import (
	"bytes"
	"context"
	"encoding/hex"
	"math"
	"strconv"
	"sync"
	"time"

	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/layers/otel/otelsql/otelv1pb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Span writes on a tenant store (rfcs/relational-exporter-migration.md §1.2,
// §3.1): every span of a call is admitted before any write, then batches of
// SPANS rows commit independently, one write slot per transaction; a batch
// that replaces stored spans holds only the spans that fit with their clears.

// maxSpanTxnBytes is the actual-size guard of a span write transaction
// (GetApproximateSize), under FDB's 10 MB limit by the conservative estimate.
const maxSpanTxnBytes = 500000

// SpanWriter writes OTLP spans to a tenant store as SPANS rows, as the
// Exporter does: otelbench's sql backend ingests through it, so benchmarks
// measure the production write path. Its methods are safe for concurrent use.
type SpanWriter struct {
	store *tenantStore
	opts  SpanWriterOptions
	keys  spanKeyModel
	// maxTxnBytes is maxSpanTxnBytes; tests lower it.
	maxTxnBytes int64
}

// SpanWriterOptions shape a SpanWriter's transactions.
type SpanWriterOptions struct {
	// BatchSize is the most spans per transaction; a batch also ends at the
	// 300 KB transaction estimate.
	BatchSize int
	// Workers is the number of a call's batches committing at once.
	Workers int
	// BulkIndexConflicts coalesces the batch's index write conflicts.
	BulkIndexConflicts bool
	// TransactionTimeout, when set, runs Write under the exporter's
	// transaction policy (this timeout, no retry limit, classified errors);
	// zero keeps the caller context's.
	TransactionTimeout time.Duration
}

// NewSpanWriter writes to ts on db.
func NewSpanWriter(db *recordlayer.FDBDatabase, ts *otelsql.TenantStore, opts SpanWriterOptions) *SpanWriter {
	opts.BatchSize, opts.Workers = max(opts.BatchSize, 1), max(opts.Workers, 1)
	return newSpanWriter(&tenantStore{db: db, ts: ts, databasePath: ts.DatabasePath, schema: ts.Schema}, opts)
}

func newSpanWriter(store *tenantStore, opts SpanWriterOptions) *SpanWriter {
	return &SpanWriter{store: store, opts: opts, keys: newSpanKeyModel(store.ts), maxTxnBytes: maxSpanTxnBytes}
}

// Write admits every span of spans, then commits them in batches. A
// committed batch stays committed when a later one fails; the stats count
// only committed batches.
func (w *SpanWriter) Write(ctx context.Context, spans []*tracepb.ResourceSpans) (ExportStats, error) {
	if w.opts.TransactionTimeout > 0 {
		ctx = withExporterTxns(ctx, w.opts.TransactionTimeout)
	}
	items, stats, err := w.prepare(ctx, spans, nil)
	if err != nil {
		return stats, err
	}
	return stats, w.commit(ctx, w.partition(items), &stats)
}

// spanMessages is the message flow's part in preparing rows: the GenAI
// content attributes are left out of the equality indexes, and the spans'
// references, when fulltext, become ref_chunks.
type spanMessages struct {
	uses      map[string][]messageUse // by spanKey; nil without references
	refChunks func([]messageUse) ([][]byte, error)
}

// relSpan is one admitted span: its row and the batch accounting.
type relSpan struct {
	key        string
	row        proto.Message
	logical    int64
	cost       int
	references int
	normalized bool
	unindexed  int64
}

// prepare builds the row of every span, each a single-span ResourceSpans
// with the original attribute lists; a later duplicate identity replaces an
// earlier one at its position. Any inadmissible span fails the call.
func (w *SpanWriter) prepare(ctx context.Context, in []*tracepb.ResourceSpans, msg *spanMessages) ([]relSpan, ExportStats, error) {
	var stats ExportStats
	var out []relSpan
	position := map[string]int{}
	for _, rs := range in {
		if rs == nil {
			continue
		}
		for _, ss := range rs.ScopeSpans {
			if ss == nil {
				continue
			}
			for _, sp := range ss.Spans {
				if sp == nil {
					continue
				}
				if err := ctx.Err(); err != nil {
					return nil, stats, wrap("canceled", "export preparation", err)
				}
				stats.ReceivedSpans++
				item, err := prepareSpan(rs, ss, sp, msg)
				if err != nil {
					return nil, stats, err
				}
				if i, ok := position[item.key]; ok {
					stats.DuplicateSpans++
					out[i] = item
				} else {
					position[item.key] = len(out)
					out = append(out, item)
				}
			}
		}
	}
	for _, item := range out {
		if item.normalized {
			stats.NormalizedSpans++
		}
		stats.UnindexedAttributes += item.unindexed
	}
	return out, stats, nil
}

func prepareSpan(rs *tracepb.ResourceSpans, ss *tracepb.ScopeSpans, sp *tracepb.Span, msg *spanMessages) (relSpan, error) {
	if len(sp.TraceId) != 16 || zeroID(sp.TraceId) || len(sp.SpanId) != 8 || zeroID(sp.SpanId) {
		return relSpan{}, fail("admission", "OTLP identity", "expected nonzero 16-byte trace and 8-byte span identifiers")
	}
	if sp.StartTimeUnixNano > math.MaxInt64 {
		return relSpan{}, fail("admission", "OTLP start time", "start_time_unix_nano exceeds int64")
	}
	single := &tracepb.ResourceSpans{Resource: rs.Resource, SchemaUrl: rs.SchemaUrl, ScopeSpans: []*tracepb.ScopeSpans{{Scope: ss.Scope, SchemaUrl: ss.SchemaUrl, Spans: []*tracepb.Span{sp}}}}
	single.ProtoReflect().SetUnknown(rs.ProtoReflect().GetUnknown())
	single.ScopeSpans[0].ProtoReflect().SetUnknown(ss.ProtoReflect().GetUnknown())
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(single)
	if err != nil {
		return relSpan{}, wrap("admission", "OTLP encode", err)
	}
	s := otelsql.Span{Resource: rs.Resource, ResourceSchemaURL: rs.SchemaUrl, Scope: ss.Scope, ScopeSchemaURL: ss.SchemaUrl, Span: sp, OTLP: wire, ExcludeMessageAttributes: msg != nil}
	row, err := otelsql.BuildCompiled(s)
	if err != nil {
		return relSpan{}, wrap("admission", "span row", err)
	}
	var uses []messageUse
	if msg != nil && msg.uses != nil {
		uses = msg.uses[spanKey(sp)]
	}
	if len(uses) > 0 {
		chunks, err := msg.refChunks(uses)
		if err != nil {
			return relSpan{}, err
		}
		for i, c := range chunks {
			if row.REF_CHUNKS == nil {
				row.REF_CHUNKS = &otelv1pb.SPANS_REF_CHUNKS{}
			}
			row.REF_CHUNKS.Values = append(row.REF_CHUNKS.Values, &otelv1pb.MESSAGE_REF_CHUNK{CHUNK: proto.Int64(int64(i)), REFS: c})
		}
	}
	cost := spanCost(row)
	if cost > exportSpanBudget {
		return relSpan{}, fail("admission", "export batch", "single span exceeds conservative 300KB transaction estimate")
	}
	p := otelsql.SpanProjection(s)
	return relSpan{
		key: string(sp.TraceId) + string(sp.SpanId), row: row, logical: int64(len(wire)), cost: cost,
		references: len(uses), normalized: p.Normalized, unindexed: p.Unindexed,
	}, nil
}

// spanCost is a row's conservative transaction estimate: 2048, the row's
// serialized bytes (its OTLP and every column that copies part of it), and
// per projected attribute and ref chunk its index entry, its key and value
// text plus 512. Bounding the whole row keeps an admitted span within the
// actual-size guard however its bytes are spread over the columns.
func spanCost(r *otelv1pb.SPANS) int {
	cost := 2048 + proto.Size(r)
	attr := func(key string, value int) { cost += len(key) + value + 512 }
	for _, a := range r.GetSTR_ATTRS().GetValues() {
		attr(a.GetATTR_KEY(), len(a.GetSTR_VALUE()))
	}
	for _, a := range r.GetLONG_STR_ATTRS().GetValues() {
		attr(a.GetATTR_KEY(), len(a.GetSTR_VALUE()))
	}
	for _, a := range r.GetINT_ATTRS().GetValues() {
		attr(a.GetATTR_KEY(), len(strconv.FormatInt(a.GetINT_VALUE(), 10)))
	}
	for _, a := range r.GetDOUBLE_ATTRS().GetValues() {
		attr(a.GetATTR_KEY(), len(otelsql.FormatDouble(a.GetDOUBLE_VALUE())))
	}
	for _, a := range r.GetBOOL_ATTRS().GetValues() {
		attr(a.GetATTR_KEY(), len(strconv.FormatBool(a.GetBOOL_VALUE())))
	}
	for _, a := range r.GetBYTES_ATTRS().GetValues() {
		attr(a.GetATTR_KEY(), len(a.GetBYTES_VALUE()))
	}
	for _, a := range r.GetANY_ATTRS().GetValues() {
		attr(a.GetATTR_KEY(), len(a.GetJSON_VALUE()))
	}
	for _, c := range r.GetREF_CHUNKS().GetValues() {
		cost += len(tuple.Tuple{c.GetREFS()}.Pack()) + 512
	}
	return cost
}

// spanIndexes are the SPANS indexes in spanKeyModel.index order.
var spanIndexes = [...]string{spansTimeIndex, spansSpanIDIndex, spansStrAttrIndex, spansLongStrIndex, spansIntAttrIndex, spansDoubleAttrIndex, spansBoolAttrIndex, spansRefChunksIndex}

const (
	spanIdxTime = iota
	spanIdxSpanID
	spanIdxStr
	spanIdxLongStr
	spanIdxInt
	spanIdxDouble
	spanIdxBool
	spanIdxRefChunks
)

// spanKeyModel holds the byte lengths of a store's records subspace and SPANS
// index subspaces, from which priorCost sizes a stored row's keys; err is
// set when the store's metadata lacks the table or an index.
type spanKeyModel struct {
	rt            *recordlayer.RecordType
	recordsPrefix int
	index         [len(spanIndexes)]relIndexSpace
	err           error
}

func newSpanKeyModel(ts *otelsql.TenantStore) spanKeyModel {
	const op = "span store"
	m := spanKeyModel{recordsPrefix: len(ts.Subspace.Sub(recordlayer.RecordKey).Bytes())}
	if m.rt = ts.MetaData.GetRecordType(spansRecordType); m.rt == nil {
		m.err = failf("ownership", op, "%s is not in the store's metadata", spansRecordType)
		return m
	}
	for i, name := range spanIndexes {
		idx := ts.MetaData.GetIndex(name)
		if idx == nil {
			m.err = failf("ownership", op, "index %s is not in the store's metadata", name)
			return m
		}
		m.index[i] = relIndexSpace{idx, len(ts.Subspace.Sub(recordlayer.IndexKey, idx.SubspaceTupleKey()).Bytes())}
	}
	return m
}

func (m *spanKeyModel) primaryKey(r *otelv1pb.SPANS) tuple.Tuple {
	if m.rt.PrimaryKeyHasRecordTypePrefix() {
		return tuple.Tuple{m.rt.GetRecordTypeKey(), r.TRACE_ID, r.SPAN_ID}
	}
	return tuple.Tuple{r.TRACE_ID, r.SPAN_ID}
}

// indexEntries visits every index entry the Record Layer maintains for row r,
// OTEL_V1's SPANS indexes: its spanIndexes position, its key columns before
// the primary key's remainder, and its key's byte length. Its arithmetic is
// the template's column layout; TestRelExporterRealFDB/span_index_key_model
// pins it to the store's entries.
func (m *spanKeyModel) indexEntries(r *otelv1pb.SPANS, pk tuple.Tuple, visit func(i int, cols tuple.Tuple, key int)) error {
	var rest [len(spanIndexes)]int
	for i := range spanIndexes {
		trimmed, err := m.index[i].idx.TrimPrimaryKey(pk)
		if err != nil {
			return err
		}
		rest[i] = m.index[i].prefix + packedTupleLen(trimmed)
	}
	emit := func(i int, cols tuple.Tuple) { visit(i, cols, rest[i]+packedTupleLen(cols)) }
	var start tuple.TupleElement
	if r.START_NS != nil {
		start = r.GetSTART_NS()
	}
	emit(spanIdxTime, tuple.Tuple{start, r.TRACE_ID, r.SPAN_ID})
	emit(spanIdxSpanID, tuple.Tuple{r.SPAN_ID, start, r.TRACE_ID})
	attr := func(i int, scope, key *string, value tuple.TupleElement) {
		emit(i, tuple.Tuple{optString(scope), optString(key), value, start, r.TRACE_ID, r.SPAN_ID})
	}
	for _, a := range r.GetSTR_ATTRS().GetValues() {
		attr(spanIdxStr, a.SCOPE, a.ATTR_KEY, optString(a.STR_VALUE))
	}
	for _, a := range r.GetLONG_STR_ATTRS().GetValues() {
		attr(spanIdxLongStr, a.SCOPE, a.ATTR_KEY, a.GetDIGEST())
	}
	for _, a := range r.GetINT_ATTRS().GetValues() {
		attr(spanIdxInt, a.SCOPE, a.ATTR_KEY, a.GetINT_VALUE())
	}
	for _, a := range r.GetDOUBLE_ATTRS().GetValues() {
		attr(spanIdxDouble, a.SCOPE, a.ATTR_KEY, a.GetDOUBLE_VALUE())
	}
	for _, a := range r.GetBOOL_ATTRS().GetValues() {
		attr(spanIdxBool, a.SCOPE, a.ATTR_KEY, a.GetBOOL_VALUE())
	}
	for _, c := range r.GetREF_CHUNKS().GetValues() {
		emit(spanIdxRefChunks, tuple.Tuple{r.TRACE_ID, start, r.SPAN_ID, c.GetCHUNK(), c.GetREFS()})
	}
	return nil
}

// optString is a nullable string column's value, nil for NULL.
func optString(s *string) tuple.TupleElement {
	if s == nil {
		return nil
	}
	return *s
}

// priorCost is the native cost of replacing the stored row old (keyCount
// record keys) with next: clearing its record keys and version, and every
// index entry of old that next does not keep (the Record Layer keeps the
// entries both rows have).
func (m *spanKeyModel) priorCost(old, next *otelv1pb.SPANS, pk tuple.Tuple, keyCount int) (int, error) {
	kept := map[string]struct{}{}
	key := func(i int, cols tuple.Tuple) string { return string(append([]byte{byte(i)}, cols.Pack()...)) }
	if err := m.indexEntries(next, pk, func(i int, cols tuple.Tuple, _ int) { kept[key(i, cols)] = struct{}{} }); err != nil {
		return 0, err
	}
	cost := relRecordClearCost(m.recordsPrefix+packedTupleLen(pk)+relLogSuffixBytes, keyCount)
	err := m.indexEntries(old, pk, func(i int, cols tuple.Tuple, n int) {
		if _, ok := kept[key(i, cols)]; !ok {
			cost += relClearCost(n)
		}
	})
	return cost, err
}

// fitting is how many of spans, from the first, fit the conservative
// transaction estimate with the clears of the stored spans they replace,
// whose rows it loads from store (the save's prefetch has read them). A first
// span that does not fit alone fails as admission.
func (w *SpanWriter) fitting(store *recordlayer.FDBRecordStore, spans []relSpan) (int, error) {
	const op = "export batch"
	if w.keys.err != nil {
		return 0, w.keys.err
	}
	futures := make([]recordlayer.PendingRead[*recordlayer.FDBStoredRecord[proto.Message]], 0, min(loadRecordsBatch, len(spans)))
	pks := make([]tuple.Tuple, 0, cap(futures))
	total := 0
	for lo := 0; lo < len(spans); lo += loadRecordsBatch {
		futures, pks = futures[:0], pks[:0]
		for i := lo; i < min(lo+loadRecordsBatch, len(spans)); i++ {
			pk := w.keys.primaryKey(spans[i].row.(*otelv1pb.SPANS))
			pks = append(pks, pk)
			futures = append(futures, store.LoadRecordAsync(pk, nil))
		}
		for k, f := range futures {
			i := lo + k
			old, err := f.Get()
			if err != nil {
				return 0, err
			}
			cost := spans[i].cost
			if old != nil {
				row, ok := old.Record.(*otelv1pb.SPANS)
				if !ok {
					return 0, failf("ownership", op, "%s record is not the compiled row", spansRecordType)
				}
				prior, err := w.keys.priorCost(row, spans[i].row.(*otelv1pb.SPANS), pks[k], old.KeyCount)
				if err != nil {
					return 0, err
				}
				cost += prior
			}
			if total+cost > exportSpanBudget {
				if i == 0 {
					return 0, fail("admission", op, "single span with the clears of the span it replaces exceeds conservative 300KB transaction estimate")
				}
				return i, nil
			}
			total += cost
		}
	}
	return len(spans), nil
}

// relBatch is one write transaction's spans.
type relBatch struct {
	first, next int // positions in the prepared spans
	spans       []relSpan
	logical     int64
	references  int64
	estimate    int
}

func (b *relBatch) add(it relSpan) {
	b.spans = append(b.spans, it)
	b.logical += it.logical
	b.references += int64(it.references)
	b.estimate += it.cost
	b.next++
}

func (b relBatch) rows() []proto.Message {
	out := make([]proto.Message, len(b.spans))
	for i, it := range b.spans {
		out[i] = it.row
	}
	return out
}

// cut splits b after its first k spans.
func (b relBatch) cut(k int) (head, tail relBatch) {
	if k == len(b.spans) {
		return b, relBatch{first: b.next, next: b.next}
	}
	head, tail = relBatch{first: b.first, next: b.first}, relBatch{first: b.first + k, next: b.first + k}
	for _, it := range b.spans[:k] {
		head.add(it)
	}
	for _, it := range b.spans[k:] {
		tail.add(it)
	}
	return head, tail
}

// partition cuts items into batches of at most BatchSize spans within the
// conservative transaction estimate.
func (w *SpanWriter) partition(items []relSpan) []relBatch {
	var out []relBatch
	cur := relBatch{}
	for i, it := range items {
		if len(cur.spans) > 0 && (len(cur.spans) == w.opts.BatchSize || cur.estimate+it.cost > exportSpanBudget) {
			out = append(out, cur)
			cur = relBatch{first: i, next: i}
		}
		cur.add(it)
	}
	if len(cur.spans) > 0 {
		out = append(out, cur)
	}
	return out
}

// commit saves the batches on up to Workers goroutines; the first failure
// cancels the batches not yet started.
func (w *SpanWriter) commit(ctx context.Context, batches []relBatch, stats *ExportStats) error {
	if len(batches) == 0 {
		return nil
	}
	if len(batches) == 1 {
		return w.save(ctx, batches[0], stats.addBatch)
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan relBatch)
	var mu sync.Mutex
	var firstErr error
	committed := 0
	var workers sync.WaitGroup
	for range min(w.opts.Workers, len(batches)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for batch := range jobs {
				if workCtx.Err() != nil {
					continue
				}
				err := w.save(workCtx, batch, func(b relBatch) {
					mu.Lock()
					stats.addBatch(b)
					mu.Unlock()
				})
				mu.Lock()
				if err != nil {
					if firstErr == nil {
						firstErr = err
						cancel()
					}
				} else {
					committed++
				}
				mu.Unlock()
			}
		}()
	}
feed:
	for _, batch := range batches {
		select {
		case <-workCtx.Done():
			break feed
		case jobs <- batch:
		}
	}
	close(jobs)
	workers.Wait()
	if firstErr != nil {
		return firstErr
	}
	if committed != len(batches) {
		return wrap("canceled", "export", ctx.Err())
	}
	return nil
}

// addBatch counts a committed batch.
func (s *ExportStats) addBatch(b relBatch) {
	s.Spans += int64(len(b.spans))
	s.Batches++
	s.LogicalBytes += b.logical
	s.Messages.References += b.references
}

// save commits batch b, passing each committed transaction's spans to
// committed. The rows' old images are prefetched while the store opens, and
// every transaction must stay within maxTxnBytes. Within the estimate only
// the clears of the stored spans the batch replaces can pass the guard, so a
// batch that does is saved again in as few transactions as fit: each holds
// the longest prefix within the estimate with those clears (fitting), and
// the rest continue in the next.
func (w *SpanWriter) save(ctx context.Context, b relBatch, committed func(relBatch)) error {
	const op = "export batch"
	fit := false
	for len(b.spans) > 0 {
		var head, tail relBatch
		over := false
		_, err := runTxnSlot(ctx, w.store.db, op, func(rc *recordlayer.FDBRecordContext) (any, error) {
			over = false
			rows := b.rows()
			var pending *recordlayer.RecordBatchPrefetch
			store, err := w.store.openWith(rc, op, func(builder *recordlayer.StoreBuilder) {
				pending = builder.PrefetchRecordBatch(rows)
			})
			if err != nil {
				return nil, err
			}
			if err := pending.Wait(); err != nil {
				return nil, err
			}
			k := len(rows)
			if fit {
				if k, err = w.fitting(store, b.spans); err != nil {
					return nil, err
				}
			}
			head, tail = b.cut(k)
			if _, err := store.SaveRecordBatchWithOptions(rows[:k], recordlayer.BatchSaveOptions{CoalesceIndexWriteConflicts: w.opts.BulkIndexConflicts}); err != nil {
				return nil, err
			}
			n, err := rc.Transaction().GetApproximateSize().Get()
			if err != nil {
				return nil, err
			}
			if n > w.maxTxnBytes {
				over = true
				return nil, failf("budget", "export transaction", "transaction guard exceeded: bytes=%d", n)
			}
			return nil, nil
		})
		if err != nil {
			if over && !fit {
				fit = true
				continue
			}
			return err
		}
		committed(head)
		b = tail
	}
	return nil
}

// ExportResourceSpans admits every span before writing, so an inadmissible
// span fails the call without partial writes; batches then commit
// independently. With messages on, the GenAI content attributes are
// decomposed: spans are admitted with placeholder manifests and references,
// the messages persist, and the spans are prepared again with final ones.
func (e *Exporter) ExportResourceSpans(ctx context.Context, spans []*tracepb.ResourceSpans) (ExportStats, error) {
	if err := e.begin("export"); err != nil {
		return ExportStats{}, err
	}
	defer e.active.Done()
	ctx = withTxnLimiter(e.txnContext(ctx), e.txnSlots)
	if e.messages == nil || len(spans) == 0 {
		return e.writer.Write(ctx, spans)
	}
	d, err := decomposeMessages(ctx, spans)
	if err != nil {
		return ExportStats{}, err
	}
	msg := &spanMessages{refChunks: e.messages.refChunks}
	if e.cfg.EnableMessageFullText {
		msg.uses = d.uses
	}
	// With placeholder manifests and references this admits every span
	// before any write.
	_, stats, err := e.writer.prepare(ctx, d.spans, msg)
	if err != nil {
		return stats, err
	}
	// Messages and their trace markers precede the spans that reference them.
	stored, err := e.messages.persist(ctx, d)
	stats.Messages = stored
	if err == nil {
		err = d.encodeManifests(false)
	}
	if err != nil {
		return stats, err
	}
	items, final, err := e.writer.prepare(ctx, d.spans, msg)
	if err != nil {
		return stats, err
	}
	final.MessageDeltas, final.Messages = d.stats, stored
	return final, e.writer.commit(ctx, e.writer.partition(items), &final)
}

// LoadSpanOTLP returns one stored span as the single-span ResourceSpans it
// was exported in, original attribute lists and unknown fields included, its
// GenAI message values restored.
func (e *Exporter) LoadSpanOTLP(ctx context.Context, traceID, spanID string) (*tracepb.ResourceSpans, error) {
	const op = "load span"
	if !validIdentity(traceID, 16) || !validIdentity(spanID, 8) {
		return nil, fail("config", op, "expected lowercase hex 16-byte trace and 8-byte span identifiers")
	}
	if err := e.begin(op); err != nil {
		return nil, err
	}
	defer e.active.Done()
	ctx = e.txnContext(ctx)
	trace, _ := hex.DecodeString(traceID)
	span, _ := hex.DecodeString(spanID)
	var out *tracepb.ResourceSpans
	err := e.store.read(ctx, op, func(store *recordlayer.FDBRecordStore) error {
		out = nil
		pk, err := tableKey(store, op, spansRecordType, trace, span)
		if err != nil {
			return err
		}
		rec, err := store.LoadRecord(pk)
		if err != nil {
			return err
		}
		if rec == nil {
			return &SpanNotFoundError{OtelError: &OtelError{Kind: "operation", Op: op, Message: "span not found"}, Trace: traceID, Span: spanID}
		}
		rs, err := rowOTLP(op, rec.Record.ProtoReflect(), trace, span)
		if err != nil {
			return err
		}
		if e.messages != nil {
			if err := e.messages.reconstruct(ctx, store, rs, newQueryWork(e.reader.cfg)); err != nil {
				return err
			}
		}
		out = rs
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// rowOTLP decodes a SPANS row's otlp column, which must be the single-span
// ResourceSpans of the row's identity.
func rowOTLP(op string, m protoreflect.Message, trace, span []byte) (*tracepb.ResourceSpans, error) {
	f := m.Descriptor().Fields()
	otlp, traceF, spanF := f.ByName("OTLP"), f.ByName("TRACE_ID"), f.ByName("SPAN_ID")
	if otlp == nil || traceF == nil || spanF == nil {
		return nil, failf("ownership", op, "%s lacks TRACE_ID, SPAN_ID or OTLP", spansRecordType)
	}
	rs := &tracepb.ResourceSpans{}
	if err := proto.Unmarshal(m.Get(otlp).Bytes(), rs); err != nil {
		return nil, typedError("corruption", op, "span otlp decode", err)
	}
	if len(rs.ScopeSpans) != 1 || len(rs.ScopeSpans[0].GetSpans()) != 1 || rs.ScopeSpans[0].Spans[0] == nil {
		return nil, fail("corruption", op, "span otlp is not a single-span ResourceSpans")
	}
	sp := rs.ScopeSpans[0].Spans[0]
	if !bytes.Equal(m.Get(traceF).Bytes(), trace) || !bytes.Equal(m.Get(spanF).Bytes(), span) || !bytes.Equal(sp.TraceId, trace) || !bytes.Equal(sp.SpanId, span) {
		return nil, fail("corruption", op, "span otlp disagrees with its record")
	}
	return rs, nil
}
