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
// SPANS rows commit independently, one write slot per transaction.

// maxSpanTxnBytes is the actual-size guard of a span write transaction
// (GetApproximateSize), under FDB's 10 MB limit by the conservative estimate.
const maxSpanTxnBytes = 500000

// SpanWriter writes OTLP spans to a tenant store as SPANS rows, as the
// Exporter does: otelbench's sql backend ingests through it, so benchmarks
// measure the production write path. Its methods are safe for concurrent use.
type SpanWriter struct {
	store *tenantStore
	opts  SpanWriterOptions
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
	return &SpanWriter{store: &tenantStore{db: db, ts: ts, databasePath: ts.DatabasePath, schema: ts.Schema}, opts: opts, maxTxnBytes: maxSpanTxnBytes}
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
	cost := spanCost(row, len(wire))
	if cost > exportSpanBudget {
		return relSpan{}, fail("admission", "export batch", "single span exceeds conservative 300KB transaction estimate")
	}
	p := otelsql.SpanProjection(s)
	return relSpan{
		key: string(sp.TraceId) + string(sp.SpanId), row: row, logical: int64(len(wire)), cost: cost,
		references: len(uses), normalized: p.Normalized, unindexed: p.Unindexed,
	}, nil
}

// spanCost is a row's conservative transaction estimate: 2048 and the otlp
// bytes, plus 2·(key + value text + 256) per projected attribute (its record
// copy and index entry) and per ref chunk its encoded index key bytes.
func spanCost(r *otelv1pb.SPANS, otlp int) int {
	cost := 2048 + otlp
	attr := func(key string, value int) { cost += 2 * (len(key) + value + 256) }
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
		attr(a.GetATTR_KEY(), len(formatDouble(a.GetDOUBLE_VALUE())))
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
		cost += 2 * (len(tuple.Tuple{c.GetREFS()}.Pack()) + 256)
	}
	return cost
}

// relBatch is one write transaction's rows.
type relBatch struct {
	first, next int // positions in the prepared spans
	rows        []proto.Message
	logical     int64
	references  int64
	estimate    int
}

// partition cuts items into batches of at most BatchSize spans within the
// conservative transaction estimate.
func (w *SpanWriter) partition(items []relSpan) []relBatch {
	var out []relBatch
	cur := relBatch{}
	for i, it := range items {
		if len(cur.rows) > 0 && (len(cur.rows) == w.opts.BatchSize || cur.estimate+it.cost > exportSpanBudget) {
			out = append(out, cur)
			cur = relBatch{first: i, next: i}
		}
		cur.rows = append(cur.rows, it.row)
		cur.logical += it.logical
		cur.references += int64(it.references)
		cur.estimate += it.cost
		cur.next = i + 1
	}
	if len(cur.rows) > 0 {
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
		if err := w.save(ctx, batches[0]); err != nil {
			return err
		}
		stats.addBatch(batches[0])
		return nil
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
				err := w.save(workCtx, batch)
				mu.Lock()
				if err != nil {
					if firstErr == nil {
						firstErr = err
						cancel()
					}
				} else {
					committed++
					stats.addBatch(batch)
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
	s.Spans += int64(len(b.rows))
	s.Batches++
	s.LogicalBytes += b.logical
	s.Messages.References += b.references
}

// save commits one batch: the rows' old images are prefetched while the store
// opens, the batch saves in one call, and the transaction must stay within
// maxTxnBytes.
func (w *SpanWriter) save(ctx context.Context, b relBatch) error {
	const op = "export batch"
	_, err := runTxnSlot(ctx, w.store.db, op, func(rc *recordlayer.FDBRecordContext) (any, error) {
		var pending *recordlayer.RecordBatchPrefetch
		store, err := w.store.openWith(rc, op, func(builder *recordlayer.StoreBuilder) {
			pending = builder.PrefetchRecordBatch(b.rows)
		})
		if err != nil {
			return nil, err
		}
		if err := pending.Wait(); err != nil {
			return nil, err
		}
		if _, err := store.SaveRecordBatchWithOptions(b.rows, recordlayer.BatchSaveOptions{CoalesceIndexWriteConflicts: w.opts.BulkIndexConflicts}); err != nil {
			return nil, err
		}
		n, err := rc.Transaction().GetApproximateSize().Get()
		if err != nil {
			return nil, err
		}
		if n > w.maxTxnBytes {
			return nil, failf("budget", "export transaction", "transaction guard exceeded: bytes=%d", n)
		}
		return nil, nil
	})
	return err
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
	d, err := decomposeMessages(ctx, spans, e.cfg.Tenant)
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
