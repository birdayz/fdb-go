package otel

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/hex"
	"math"
	"math/bits"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/layers/otel/otelsql/otelv1pb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// Logs on a tenant store (rfcs/relational-exporter-migration.md §1.6, §3.1,
// §3.2): one LOGS row per event ID, written through the Record Layer with
// standard index maintenance and read by direct scans of the OTEL_V1 log
// indexes, each read one budgeted transaction.

// OTEL_V1 log indexes and their key columns; every entry ends with the row's
// primary key.
const (
	logsRecordType      = "LOGS"
	logsTraceIndex      = "LOGS_TRACE"         // trace_id, span_id, effective_ns, event_id
	logsTimeIndex       = "LOGS_TIME"          // effective_ns, event_id
	logsStrAttrIndex    = "LOGS_STR_ATTR"      // scope, key, value, effective_ns, event_id, trace_id, span_id
	logsLongStrIndex    = "LOGS_LONG_STR_ATTR" // scope, key, digest, effective_ns, event_id, trace_id, span_id
	logsIntAttrIndex    = "LOGS_INT_ATTR"
	logsDoubleAttrIndex = "LOGS_DOUBLE_ATTR"
	logsBoolAttrIndex   = "LOGS_BOOL_ATTR"
	logsEventIndex      = "LOGS_EVENT" // event_name, effective_ns, event_id, trace_id, span_id
)

// The write cost model (§3.1 admission). Costs are native transaction bytes:
// a set is its key three times (mutation and write conflict range) and its
// value plus 69, a clear its key four times plus 70 (logSetWriteCost,
// logClearWriteCost).
const (
	// relLogFixedCost is a log's share of its transaction's store header,
	// index state and existence reads.
	relLogFixedCost = 1024
	// relLogSplitBytes is the Record Layer's split record chunk size.
	relLogSplitBytes = 100_000
	// relLogSuffixBytes bounds a record key's split suffix.
	relLogSuffixBytes = 3
	// fdbMaxKeyBytes is FDB's key size limit.
	fdbMaxKeyBytes = 10_000
)

func relSetCost(key, value int) int { return 3*key + value + 69 }
func relClearCost(key int) int      { return 4*key + 70 }

// relLogIndexes are the LOGS indexes in relLogIndex order.
var relLogIndexes = [...]string{logsTraceIndex, logsTimeIndex, logsStrAttrIndex, logsLongStrIndex, logsIntAttrIndex, logsDoubleAttrIndex, logsBoolAttrIndex, logsEventIndex}

const (
	relLogTrace = iota
	relLogTime
	relLogStr
	relLogLongStr
	relLogInt
	relLogDouble
	relLogBool
	relLogEvent
)

// relLogStore stores and reads logs on a tenant store. ctx carries the
// Exporter's transaction policy and write limiter (withExporterTxns,
// withTxnLimiter), as the relational exporter applies them; lifecycle
// (begin, Shutdown) is the exporter's. Its methods are safe for concurrent use.
type relLogStore struct {
	store *tenantStore
	cfg   ExporterConfig
	rt    *recordlayer.RecordType
	// recordsPrefix and index[i].prefix are the byte lengths of the store's
	// records subspace and of each LOGS index subspace.
	recordsPrefix int
	index         [len(relLogIndexes)]relLogIndexSpace
	// zlibLevel is the store serializer's deflate level when it is known,
	// else noZlib: the serialized row is then bounded by its uncompressed size.
	zlibLevel int
}

type relLogIndexSpace struct {
	idx    *recordlayer.Index
	prefix int
}

const noZlib = math.MinInt

// newRelLogStore resolves the LOGS table and indexes of store. cfg supplies
// the batching, K and query budgets, and the record compression the store's
// serializer was built from.
func newRelLogStore(store *tenantStore, cfg ExporterConfig) (*relLogStore, error) {
	const op = "log store"
	md := store.ts.MetaData
	rt := md.GetRecordType(logsRecordType)
	if rt == nil {
		return nil, failf("ownership", op, "%s is not in the store's metadata", logsRecordType)
	}
	s := &relLogStore{store: store, cfg: cfg, rt: rt, recordsPrefix: len(store.ts.Subspace.Sub(recordlayer.RecordKey).Bytes()), zlibLevel: noZlib}
	for i, name := range relLogIndexes {
		idx := md.GetIndex(name)
		if idx == nil {
			return nil, failf("ownership", op, "index %s is not in the store's metadata", name)
		}
		s.index[i] = relLogIndexSpace{idx, len(store.ts.Subspace.Sub(recordlayer.IndexKey, idx.SubspaceTupleKey()).Bytes())}
	}
	switch {
	case store.ts.Serializer == nil:
		s.zlibLevel = 0 // stored uncompressed
	case cfg.RecordCompression == "zlib-1":
		s.zlibLevel = 1
	case cfg.RecordCompression == "zlib-default":
		s.zlibLevel = recordlayer.DeflaterDefaultCompression
	}
	return s, nil
}

func (s *relLogStore) primaryKey(eventID string) tuple.Tuple {
	if s.rt.PrimaryKeyHasRecordTypePrefix() {
		return tuple.Tuple{s.rt.GetRecordTypeKey(), eventID}
	}
	return tuple.Tuple{eventID}
}

func (s *relLogStore) newWork() *queryWork {
	return &queryWork{maxCandidates: s.cfg.MaxCandidates, maxBytes: s.cfg.MaxBytes}
}

// ---------------------------------------------------------------------------
// Write cost
// ---------------------------------------------------------------------------

// packedLen is the tuple encoding length of a key column.
func packedLen(v any) int {
	switch x := v.(type) {
	case nil:
		return 1
	case string:
		return 2 + len(x) + strings.Count(x, "\x00")
	case []byte:
		if x == nil {
			return 1 // a NULL column
		}
		return 2 + len(x) + bytes.Count(x, []byte{0})
	case int64:
		u := uint64(x) //nolint:gosec // the magnitude's bytes, as the tuple encoder counts them
		if x < 0 {
			u = uint64(-x) //nolint:gosec // as above; MinInt64 is 1<<63
		}
		return 1 + (bits.Len64(u)+7)/8
	case float64:
		return 9
	case bool:
		return 1
	}
	return len(tuple.Tuple{v}.Pack())
}

func packedTupleLen(t tuple.Tuple) int {
	n := 0
	for _, v := range t {
		n += packedLen(v)
	}
	return n
}

// indexKeys visits the byte length of every index key the Record Layer
// maintains for row r (OTEL_V1's LOGS indexes, the D11 NULL entries
// included) and reports the longest. Its arithmetic is the template's
// column layout; TestRelLogIndexKeyModel pins it to the store's entries.
func (s *relLogStore) indexKeys(r *otelv1pb.LOGS, pk tuple.Tuple, visit func(key int)) (int, error) {
	longest := 0
	emit := func(i, columns int) error {
		trimmed, err := s.index[i].idx.TrimPrimaryKey(pk)
		if err != nil {
			return err
		}
		n := s.index[i].prefix + columns + packedTupleLen(trimmed)
		longest = max(longest, n)
		visit(n)
		return nil
	}
	eff, ev := packedLen(r.GetEFFECTIVE_NS()), packedLen(r.GetEVENT_ID())
	tr, sp := packedLen(r.TRACE_ID), packedLen(r.SPAN_ID)
	tail := eff + ev + tr + sp
	var name any
	if r.EVENT_NAME != nil {
		name = r.GetEVENT_NAME()
	}
	for _, e := range []struct{ index, columns int }{{relLogTrace, tail}, {relLogTime, eff + ev}, {relLogEvent, packedLen(name) + tail}} {
		if err := emit(e.index, e.columns); err != nil {
			return 0, err
		}
	}
	attr := func(i int, scope, key string, value any) error {
		return emit(i, packedLen(scope)+packedLen(key)+packedLen(value)+tail)
	}
	for _, a := range r.GetSTR_ATTRS().GetValues() {
		if err := attr(relLogStr, a.GetSCOPE(), a.GetATTR_KEY(), a.GetSTR_VALUE()); err != nil {
			return 0, err
		}
	}
	for _, a := range r.GetLONG_STR_ATTRS().GetValues() {
		if err := attr(relLogLongStr, a.GetSCOPE(), a.GetATTR_KEY(), a.GetDIGEST()); err != nil {
			return 0, err
		}
	}
	for _, a := range r.GetINT_ATTRS().GetValues() {
		if err := attr(relLogInt, a.GetSCOPE(), a.GetATTR_KEY(), a.GetINT_VALUE()); err != nil {
			return 0, err
		}
	}
	for _, a := range r.GetDOUBLE_ATTRS().GetValues() {
		if err := attr(relLogDouble, a.GetSCOPE(), a.GetATTR_KEY(), a.GetDOUBLE_VALUE()); err != nil {
			return 0, err
		}
	}
	for _, a := range r.GetBOOL_ATTRS().GetValues() {
		if err := attr(relLogBool, a.GetSCOPE(), a.GetATTR_KEY(), a.GetBOOL_VALUE()); err != nil {
			return 0, err
		}
	}
	return longest, nil
}

// rowBytes bounds the stored bytes of r: its record type union, the
// serializer's prefix and, when exact is set and the store deflates at a known
// level, deflated as the serializer deflates it (only when that is shorter).
func (s *relLogStore) rowBytes(r *otelv1pb.LOGS, exact bool) (int, error) {
	n := proto.Size(r)
	union := 1 + protowire.SizeVarint(uint64(n)) + n //nolint:gosec // a size
	if s.zlibLevel == 0 {
		return union, nil
	}
	const prefix = 1 // the transform prefix varint
	if !exact || s.zlibLevel == noZlib || union <= 5 {
		return prefix + union, nil
	}
	data, err := proto.Marshal(&otelv1pb.RecordTypeUnion{LOGS_0: r})
	if err != nil {
		return 0, wrap("admission", "log encode", err)
	}
	var buf bytes.Buffer
	w, err := zlib.NewWriterLevel(&buf, s.zlibLevel)
	if err != nil {
		return 0, wrap("admission", "log encode", err)
	}
	if _, err := w.Write(data); err != nil {
		return 0, wrap("admission", "log encode", err)
	}
	if err := w.Close(); err != nil {
		return 0, wrap("admission", "log encode", err)
	}
	if buf.Len() >= len(data)-5 {
		return prefix + len(data), nil
	}
	return prefix + 5 + buf.Len(), nil
}

// recordKeyBytes bounds the byte length of a row's record keys.
func (s *relLogStore) recordKeyBytes(pk tuple.Tuple) int {
	return s.recordsPrefix + packedTupleLen(pk) + relLogSuffixBytes
}

// writeCost is the native cost of saving r as a new row: its record chunks
// and version, its existence read and every index entry. It reports the
// longest key too.
func (s *relLogStore) writeCost(r *otelv1pb.LOGS, pk tuple.Tuple, rowBytes int) (int, int, error) {
	rk := s.recordKeyBytes(pk)
	chunks := 1
	if rowBytes > relLogSplitBytes {
		chunks = (rowBytes + relLogSplitBytes - 1) / relLogSplitBytes
	}
	cost := relLogFixedCost + rowBytes + chunks*relSetCost(rk, 0) + relSetCost(rk, 16) + 2*rk + 24
	longest, err := s.indexKeys(r, pk, func(key int) { cost += relSetCost(key, 0) })
	return cost, max(longest, rk), err
}

// priorCost is the native cost of replacing the stored row old (keyCount
// record keys): clearing its record keys, version and every index entry.
func (s *relLogStore) priorCost(old *otelv1pb.LOGS, pk tuple.Tuple, keyCount int) (int, error) {
	rk := s.recordKeyBytes(pk)
	cost := (keyCount+1)*relClearCost(rk) + 2*(2*rk) + 68
	_, err := s.indexKeys(old, pk, func(key int) { cost += relClearCost(key) })
	return cost, err
}

// ---------------------------------------------------------------------------
// Export
// ---------------------------------------------------------------------------

// relPreparedLog is one admitted log: its row and its write cost, without and
// with the clears of the prior image the admission preflight read.
type relPreparedLog struct {
	eventID    string
	pk         tuple.Tuple
	row        *otelv1pb.LOGS
	correlated bool
	logical    int64
	unindexed  int64
	writeCost  int
	cost       int
}

type relLogBatch struct {
	logs []relPreparedLog
	cost int
}

// prepare applies the per-log admission rules (§1.6) and builds the row;
// receiver supplies the fallback time.
func (s *relLogStore) prepare(l LogRecord, receiver int64) (relPreparedLog, error) {
	const op = "log admission"
	if l.original != nil {
		eventID := l.EventID
		l = projectLog(l.original)
		l.EventID = eventID
	}
	if l.Timestamp < 0 || l.ObservedTimestamp < 0 {
		return relPreparedLog{}, fail("admission", op, "timestamps must not be negative")
	}
	if len(l.EventID) > maxLogEventID || !utf8.ValidString(l.EventID) {
		return relPreparedLog{}, failf("admission", op, "event ID must be valid UTF-8 of at most %d bytes", maxLogEventID)
	}
	l.TraceID, l.SpanID = strings.ToLower(l.TraceID), strings.ToLower(l.SpanID)
	if l.TraceID != "" && !validIdentity(l.TraceID, 16) {
		return relPreparedLog{}, fail("admission", op, "trace ID must be 32 hex digits and nonzero")
	}
	if l.SpanID != "" && (l.TraceID == "" || !validIdentity(l.SpanID, 8)) {
		return relPreparedLog{}, fail("admission", op, "span ID must be 16 hex digits, nonzero, with a trace ID")
	}
	switch {
	case l.Timestamp != 0:
		l.EffectiveTime, l.TimeSource = l.Timestamp, LogTimeEvent
	case l.ObservedTimestamp != 0:
		l.EffectiveTime, l.TimeSource = l.ObservedTimestamp, LogTimeObserved
	default:
		l.EffectiveTime, l.TimeSource = receiver, LogTimeReceiver
	}
	rl, err := toResourceLogs(l)
	if err != nil {
		return relPreparedLog{}, err
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(rl)
	if err != nil {
		return relPreparedLog{}, wrap("admission", "log encode", err)
	}
	if len(raw) > maxLogRawBytes {
		return relPreparedLog{}, failf("admission", op, "encoded log of %d bytes exceeds %d", len(raw), maxLogRawBytes)
	}
	sl := rl.ScopeLogs[0]
	in := otelsql.Log{
		Resource: rl.Resource, ResourceSchemaURL: rl.SchemaUrl, Scope: sl.Scope, ScopeSchemaURL: sl.SchemaUrl, Log: sl.LogRecords[0],
		EventID: l.EventID, EffectiveNs: l.EffectiveTime, TimeSource: l.TimeSource, OTLP: raw,
	}
	row, err := otelsql.BuildLogCompiled(in)
	if err != nil {
		return relPreparedLog{}, wrap("admission", op, err)
	}
	p := relPreparedLog{eventID: l.EventID, pk: s.primaryKey(l.EventID), row: row, correlated: row.TRACE_ID != nil, logical: int64(len(raw)), unindexed: otelsql.LogProjection(in).Unindexed}
	if err := s.admitCost(&p); err != nil {
		return relPreparedLog{}, err
	}
	return p, nil
}

// admitCost sets p's new-image write cost and refuses it over the
// transaction budget. The uncompressed row bounds the cost; only a row over
// the budget is deflated to its exact size.
func (s *relLogStore) admitCost(p *relPreparedLog) error {
	const op = "log admission"
	for _, exact := range []bool{false, true} {
		n, err := s.rowBytes(p.row, exact)
		if err != nil {
			return err
		}
		cost, longest, err := s.writeCost(p.row, p.pk, n)
		if err != nil {
			return err
		}
		if longest > fdbMaxKeyBytes {
			return failf("admission", op, "a key of %d bytes exceeds %d", longest, fdbMaxKeyBytes)
		}
		p.writeCost, p.cost = cost, cost
		if cost <= maxLogTxnWriteBytes {
			return nil
		}
	}
	return failf("admission", op, "expanded transaction writes of %d bytes exceed %d", p.writeCost, maxLogTxnWriteBytes)
}

// admitPriors reads every log's prior image in one transaction before the
// first write and adds its clears to the log's cost, so a replacement too
// large for one transaction refuses the whole call.
func (s *relLogStore) admitPriors(ctx context.Context, items []relPreparedLog) error {
	const op = "log replacement admission"
	costs := make([]int, len(items))
	err := s.store.read(ctx, op, func(store *recordlayer.FDBRecordStore) error {
		return s.loadPriors(store, op, items, func(i int, old *recordlayer.FDBStoredRecord[proto.Message], row *otelv1pb.LOGS) error {
			costs[i] = items[i].writeCost
			if row != nil {
				c, err := s.priorCost(row, items[i].pk, old.KeyCount)
				if err != nil {
					return err
				}
				costs[i] += c
			}
			if costs[i] > maxLogTxnWriteBytes {
				return failf("admission", op, "native transaction cost %d exceeds %d", costs[i], maxLogTxnWriteBytes)
			}
			return nil
		})
	})
	if err == nil {
		for i := range items {
			items[i].cost = costs[i]
		}
	}
	return err
}

// loadPriors loads the stored rows of items, loadRecordsBatch in flight,
// and visits each (nil when absent).
func (s *relLogStore) loadPriors(store *recordlayer.FDBRecordStore, op string, items []relPreparedLog, visit func(int, *recordlayer.FDBStoredRecord[proto.Message], *otelv1pb.LOGS) error) error {
	futures := make([]recordlayer.PendingRead[*recordlayer.FDBStoredRecord[proto.Message]], 0, min(loadRecordsBatch, len(items)))
	for lo := 0; lo < len(items); lo += loadRecordsBatch {
		hi := min(lo+loadRecordsBatch, len(items))
		futures = futures[:0]
		for i := lo; i < hi; i++ {
			futures = append(futures, store.LoadRecordAsync(items[i].pk, nil))
		}
		for k, f := range futures {
			old, err := f.Get()
			if err != nil {
				return err
			}
			var row *otelv1pb.LOGS
			if old != nil {
				var ok bool
				if row, ok = old.Record.(*otelv1pb.LOGS); !ok {
					return failf("ownership", op, "%s record is not the compiled row", logsRecordType)
				}
			}
			if err := visit(lo+k, old, row); err != nil {
				return err
			}
		}
	}
	return nil
}

func partitionRelLogs(items []relPreparedLog, size int) []relLogBatch {
	var out []relLogBatch
	var cur relLogBatch
	for _, it := range items {
		if len(cur.logs) > 0 && (len(cur.logs) == size || cur.cost+it.cost > logBatchBudget) {
			out = append(out, cur)
			cur = relLogBatch{}
		}
		cur.logs = append(cur.logs, it)
		cur.cost += it.cost
	}
	if len(cur.logs) > 0 {
		out = append(out, cur)
	}
	return out
}

// saveBatch saves a batch's rows in one transaction; the Record Layer
// replaces a prior row atomically and retracts its index entries. The prior
// images are read again first: one grown past the transaction budget since
// admission fails the batch as an operation, after other batches may have
// committed.
func (s *relLogStore) saveBatch(ctx context.Context, batch relLogBatch) (replaced, stored int64, err error) {
	const op = "log batch"
	err = s.store.run(ctx, op, func(store *recordlayer.FDBRecordStore) error {
		replaced, stored = 0, 0
		cost := 0
		err := s.loadPriors(store, op, batch.logs, func(i int, old *recordlayer.FDBStoredRecord[proto.Message], row *otelv1pb.LOGS) error {
			cost += batch.logs[i].writeCost
			if row == nil {
				return nil
			}
			replaced++
			c, err := s.priorCost(row, batch.logs[i].pk, old.KeyCount)
			cost += c
			return err
		})
		if err != nil {
			return err
		}
		if cost > maxLogTxnWriteBytes {
			return failf("operation", op, "prior image changed after admission: native transaction cost %d exceeds %d", cost, maxLogTxnWriteBytes)
		}
		rows := make([]proto.Message, len(batch.logs))
		for i, l := range batch.logs {
			rows[i] = l.row
		}
		saved, err := store.SaveRecordBatch(rows)
		if err != nil {
			return err
		}
		for _, r := range saved {
			stored += int64(r.ValueSize)
		}
		return ctx.Err()
	})
	return replaced, stored, err
}

// ExportLogs admits every log before any write: the per-log rules, the raw
// cap and the write cost of each new image with its prior image's clears.
// Batches then commit independently; a live budget OperationError does not
// undo committed ones. Missing EventIDs get UUIDs outside retries.
func (s *relLogStore) ExportLogs(ctx context.Context, logs []LogRecord) (LogExportStats, error) {
	const op = "log export"
	stats := LogExportStats{EventIDs: make([]string, len(logs))}
	receiver := time.Now().UnixNano()
	var items []relPreparedLog
	position := map[string]int{}
	for i, l := range logs {
		if err := ctx.Err(); err != nil {
			return stats, wrap("canceled", op, err)
		}
		stats.ReceivedLogs++
		if l.EventID == "" {
			id, err := newEventID()
			if err != nil {
				return stats, err
			}
			l.EventID = id
			stats.GeneratedIDs++
		}
		stats.EventIDs[i] = l.EventID
		p, err := s.prepare(l, receiver)
		if err != nil {
			return stats, err
		}
		if at, ok := position[p.eventID]; ok {
			stats.DuplicateLogs++
			items[at] = p
		} else {
			position[p.eventID] = len(items)
			items = append(items, p)
		}
	}
	for _, it := range items {
		stats.UnindexedAttributes += it.unindexed
	}
	if len(items) == 0 {
		return stats, nil
	}
	if err := s.admitPriors(ctx, items); err != nil {
		return stats, err
	}
	batches := partitionRelLogs(items, s.cfg.BatchSize)
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan relLogBatch)
	var mu sync.Mutex
	var firstErr error
	committed := 0
	var workers sync.WaitGroup
	for range min(s.cfg.Workers, len(batches)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for batch := range jobs {
				if workCtx.Err() != nil {
					continue
				}
				replaced, stored, err := s.saveBatch(workCtx, batch)
				mu.Lock()
				if err != nil {
					if firstErr == nil {
						firstErr = err
						cancel()
					}
				} else {
					committed++
					stats.Batches++
					stats.Replaced += replaced
					stats.StoredBytes += stored
					for _, l := range batch.logs {
						stats.Logs++
						stats.LogicalBytes += l.logical
						if l.correlated {
							stats.Correlated++
						}
					}
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
		return stats, firstErr
	}
	if committed != len(batches) {
		return stats, wrap("canceled", op, ctx.Err())
	}
	return stats, nil
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// decodeLogRow restores the LogRecord of a LOGS row from its otlp column, a
// detached envelope; the row's identity and time columns must agree with it.
func decodeLogRow(op string, r *otelv1pb.LOGS) (LogRecord, error) {
	bad := func(msg string) (LogRecord, error) { return LogRecord{}, fail("corruption", op, msg) }
	source := r.GetTIME_SOURCE()
	if source != LogTimeEvent && source != LogTimeObserved && source != LogTimeReceiver {
		return bad("unknown log time source")
	}
	effective := r.GetEFFECTIVE_NS()
	if effective < 0 {
		return bad("negative log effective time")
	}
	rl := &logspb.ResourceLogs{}
	if err := proto.Unmarshal(r.GetOTLP(), rl); err != nil {
		return LogRecord{}, typedError("corruption", op, "log otlp decode", err)
	}
	l, err := fromResourceLogs(r.GetEVENT_ID(), effective, source, rl)
	if err != nil {
		return LogRecord{}, err
	}
	if l.TraceID != hexID(r.TRACE_ID) || l.SpanID != hexID(r.SPAN_ID) {
		return bad("log row identity disagrees with its otlp")
	}
	var timed bool
	switch source {
	case LogTimeEvent:
		timed = l.Timestamp == effective && effective != 0
	case LogTimeObserved:
		timed = l.Timestamp == 0 && l.ObservedTimestamp == effective && effective != 0
	default:
		timed = l.Timestamp == 0 && l.ObservedTimestamp == 0
	}
	if !timed {
		return bad("log row effective time disagrees with its otlp")
	}
	return l, nil
}

func hexID(b []byte) string {
	if b == nil {
		return ""
	}
	return hex.EncodeToString(b)
}

// relLogEntry is a log index entry's row identity.
type relLogEntry struct {
	eventID     string
	effective   int64
	trace, span []byte // nil when NULL
	pk          tuple.Tuple
}

// match is the entry as an existential witness.
func (e relLogEntry) match() LogMatch {
	return LogMatch{EventID: e.eventID, Trace: hexID(e.trace), Span: hexID(e.span), Time: e.effective}
}

// rowAgrees reports whether row r is the entry's.
func (e relLogEntry) rowAgrees(r *otelv1pb.LOGS) bool {
	return r.GetEVENT_ID() == e.eventID && r.GetEFFECTIVE_NS() == e.effective && bytes.Equal(r.TRACE_ID, e.trace) && bytes.Equal(r.SPAN_ID, e.span) &&
		(r.TRACE_ID == nil) == (e.trace == nil) && (r.SPAN_ID == nil) == (e.span == nil)
}

// logEntryAt reads (effective_ns, event_id, trace_id, span_id) at column col
// and validates the identities.
func logEntryAt(op string, e *recordlayer.IndexEntry, col int, order [4]int) (relLogEntry, error) {
	bad := func() (relLogEntry, error) {
		return relLogEntry{}, failf("corruption", op, "%s entry is not a log identity", e.Index.Name)
	}
	if len(e.Key) < col+4 {
		return bad()
	}
	at := func(i int) any { return e.Key[col+order[i]] }
	effective, eok := at(0).(int64)
	id, iok := at(1).(string)
	trace, tok := at(2).([]byte)
	span, sok := at(3).([]byte)
	tok = tok && len(trace) == 16 && !zeroID(trace) || at(2) == nil
	sok = sok && len(span) == 8 && !zeroID(span) || at(3) == nil
	if !eok || !iok || !tok || !sok || (trace == nil && span != nil) {
		return bad()
	}
	return relLogEntry{eventID: id, effective: effective, trace: trace, span: span, pk: e.PrimaryKey()}, nil
}

// QueryLogsByTrace returns the logs correlated to traceID in (effective
// time, event ID) order; a nonempty spanID restricts them to that span.
// Identities are case-insensitive.
func (s *relLogStore) QueryLogsByTrace(ctx context.Context, traceID, spanID string) ([]LogRecord, error) {
	const op = "log trace query"
	traceID, spanID = strings.ToLower(traceID), strings.ToLower(spanID)
	if !validIdentity(traceID, 16) || (spanID != "" && !validIdentity(spanID, 8)) {
		return []LogRecord{}, fail("config", op, "expected hex nonzero 16-byte trace and optional 8-byte span identifiers")
	}
	trace, _ := hex.DecodeString(traceID)
	prefix := tuple.Tuple{trace}
	if spanID != "" {
		span, _ := hex.DecodeString(spanID)
		prefix = append(prefix, span)
	}
	var out []LogRecord
	err := s.store.read(ctx, op, func(store *recordlayer.FDBRecordStore) error {
		out = nil
		work := s.newWork()
		var entries []relLogEntry
		// LOGS_TRACE: (trace_id, span_id, effective_ns, event_id).
		err := scanIndex(ctx, store, op, logsTraceIndex, recordlayer.TupleRangeAllOf(prefix), false, work, func(e *recordlayer.IndexEntry) (bool, error) {
			en, err := logEntryAt(op, e, 0, [4]int{2, 3, 0, 1})
			if err != nil {
				return false, err
			}
			if !bytes.Equal(en.trace, trace) || (len(prefix) == 2 && !bytes.Equal(en.span, prefix[1].([]byte))) {
				return false, fail("corruption", op, "index returned another trace's log")
			}
			entries = append(entries, en)
			return false, nil
		})
		if err != nil {
			return err
		}
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].effective != entries[j].effective {
				return entries[i].effective < entries[j].effective
			}
			return entries[i].eventID < entries[j].eventID
		})
		logs, err := s.loadEntries(store, op, entries, work, nil)
		out = logs
		return err
	})
	if err != nil {
		return []LogRecord{}, err
	}
	return out, nil
}

// loadEntries loads and decodes the rows of entries in order; a row that
// disagrees with its entry, or is missing, is corruption. holds, when set,
// also decides whether a row is kept (a long-string digest may collide).
func (s *relLogStore) loadEntries(store *recordlayer.FDBRecordStore, op string, entries []relLogEntry, work *queryWork, holds func(*otelv1pb.LOGS) bool) ([]LogRecord, error) {
	out := make([]LogRecord, 0, len(entries))
	pks := make([]tuple.Tuple, len(entries))
	for i, en := range entries {
		pks[i] = en.pk
	}
	err := loadRecords(store, op, pks, work, func(i int, rec *recordlayer.FDBStoredRecord[proto.Message]) error {
		row, err := logRow(op, rec)
		if err != nil {
			return err
		}
		if !entries[i].rowAgrees(row) {
			return fail("corruption", op, "log index entry disagrees with its record")
		}
		if holds != nil && !holds(row) {
			return nil
		}
		l, err := decodeLogRow(op, row)
		if err != nil {
			return err
		}
		out = append(out, l)
		return nil
	})
	return out, err
}

// logRow is a loaded LOGS row; an index entry without one is corruption.
func logRow(op string, rec *recordlayer.FDBStoredRecord[proto.Message]) (*otelv1pb.LOGS, error) {
	if rec == nil {
		return nil, failf("corruption", op, "a log index entry has no %s record", logsRecordType)
	}
	row, ok := rec.Record.(*otelv1pb.LOGS)
	if !ok {
		return nil, failf("ownership", op, "%s record is not the compiled row", logsRecordType)
	}
	return row, nil
}

// relLogAttr is a validated log attribute equality (D1) and the index entries
// that may equal it: those under prefix.
type relLogAttr struct {
	a      Attribute
	index  string
	prefix tuple.Tuple
	// long is the verbatim value of a long string, matched by digest and
	// verified on the record.
	long *string
	// none: no stored log can hold the attribute.
	none bool
}

// newRelLogAttr plans a log attribute equality over the scopes resource,
// scope, log and intrinsic (event_name, LOGS_EVENT).
func newRelLogAttr(op string, a Attribute) (relLogAttr, error) {
	if err := validateRelQueryAttribute(op, a, logAttributeScopes...); err != nil {
		return relLogAttr{}, err
	}
	if a.Scope == LogScopeIntrinsic {
		if a.Key != "event_name" || a.Type != "string" {
			return relLogAttr{a: a, none: true}, nil
		}
		if len(a.Value) > otelsql.MaxIndexedStringBytes {
			return relLogAttr{}, failf("config", op, "event names over %d bytes are not indexed", otelsql.MaxIndexedStringBytes)
		}
		return relLogAttr{a: a, index: logsEventIndex, prefix: tuple.Tuple{a.Value}}, nil
	}
	q, ok := spanAttrQuery(a)
	if !ok {
		return relLogAttr{a: a, none: true}, nil
	}
	var index string
	switch q.index {
	case spansStrAttrIndex:
		index = logsStrAttrIndex
	case spansLongStrIndex:
		index = logsLongStrIndex
	case spansIntAttrIndex:
		index = logsIntAttrIndex
	case spansDoubleAttrIndex:
		index = logsDoubleAttrIndex
	case spansBoolAttrIndex:
		index = logsBoolAttrIndex
	}
	return relLogAttr{a: a, index: index, prefix: tuple.Tuple{a.Scope, a.Key, q.value}, long: q.long}, nil
}

// entry reads an entry's row identity, after the prefix columns.
func (q relLogAttr) entry(op string, e *recordlayer.IndexEntry) (relLogEntry, error) {
	return logEntryAt(op, e, len(q.prefix), [4]int{0, 1, 2, 3})
}

// scanRange is the entries of q effective in w.
func (q relLogAttr) scanRange(w timeWindow) recordlayer.TupleRange {
	if !w.set {
		return recordlayer.TupleRangeAllOf(q.prefix)
	}
	at := func(t int64) tuple.Tuple { return append(slices.Clip(q.prefix), t) }
	return recordlayer.TupleRangeBetween(at(w.lo), at(w.hi))
}

// holds reports whether row r holds the attribute: what its entry claims,
// verified on the record.
func (q relLogAttr) holds(r *otelv1pb.LOGS) bool {
	a := q.a
	if q.index == logsEventIndex {
		return r.EVENT_NAME != nil && r.GetEVENT_NAME() == a.Value
	}
	is := func(scope, key string) bool { return scope == a.Scope && key == a.Key }
	switch v := q.prefix[2].(type) {
	case string:
		for _, x := range r.GetSTR_ATTRS().GetValues() {
			if is(x.GetSCOPE(), x.GetATTR_KEY()) && x.GetSTR_VALUE() == v {
				return true
			}
		}
	case []byte:
		for _, x := range r.GetLONG_STR_ATTRS().GetValues() {
			if is(x.GetSCOPE(), x.GetATTR_KEY()) && x.GetSTR_VALUE() == *q.long {
				return true
			}
		}
	case int64:
		for _, x := range r.GetINT_ATTRS().GetValues() {
			if is(x.GetSCOPE(), x.GetATTR_KEY()) && x.GetINT_VALUE() == v {
				return true
			}
		}
	case float64:
		for _, x := range r.GetDOUBLE_ATTRS().GetValues() {
			if is(x.GetSCOPE(), x.GetATTR_KEY()) && math.Float64bits(x.GetDOUBLE_VALUE()) == math.Float64bits(v) {
				return true
			}
		}
	case bool:
		for _, x := range r.GetBOOL_ATTRS().GetValues() {
			if is(x.GetSCOPE(), x.GetATTR_KEY()) && x.GetBOOL_VALUE() == v {
				return true
			}
		}
	}
	return false
}

// QueryLogsByAttribute returns up to limit (0: K) distinct logs whose
// attribute equals a (typed, D1), correlated or not, newest first by
// effective time; equal times emit event IDs descending, index order (D3).
func (s *relLogStore) QueryLogsByAttribute(ctx context.Context, a Attribute, limit int) ([]LogRecord, error) {
	const op = "log attribute query"
	q, err := newRelLogAttr(op, a)
	if err != nil {
		return []LogRecord{}, err
	}
	if limit < 0 {
		return []LogRecord{}, fail("config", op, "limit must not be negative")
	}
	if limit == 0 {
		limit = s.cfg.K
	}
	if q.none {
		return []LogRecord{}, nil
	}
	var out []LogRecord
	err = s.store.read(ctx, op, func(store *recordlayer.FDBRecordStore) error {
		out = []LogRecord{}
		work := s.newWork()
		seen := map[string]bool{}
		var batch []relLogEntry
		// flush loads the candidates and keeps those whose rows hold a.
		flush := func() (bool, error) {
			logs, err := s.loadEntries(store, op, batch, work, q.holds)
			if err != nil {
				return false, err
			}
			// Only a long string's digest may name a value the row lacks.
			if q.long == nil && len(logs) != len(batch) {
				return false, fail("corruption", op, "log index entry disagrees with its record")
			}
			out = append(out, logs...)
			batch = batch[:0]
			return len(out) == limit, nil
		}
		err := scanIndex(ctx, store, op, q.index, q.scanRange(timeWindow{}), true, work, func(e *recordlayer.IndexEntry) (bool, error) {
			en, err := q.entry(op, e)
			if err != nil {
				return false, err
			}
			if seen[en.eventID] {
				return false, nil
			}
			seen[en.eventID] = true
			batch = append(batch, en)
			if len(batch) < min(loadRecordsBatch, limit-len(out)) {
				return false, nil
			}
			return flush()
		})
		if err != nil || len(batch) == 0 {
			return err
		}
		_, err = flush()
		return err
	})
	if err != nil {
		return []LogRecord{}, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Existential log clauses
// ---------------------------------------------------------------------------

// logClause scans the correlated logs carrying one typed attribute, newest
// first, as an existential clause: uncorrelated logs witness no trace.
type logClause struct{ attr relLogAttr }

func newLogClause(op string, a Attribute) (logClause, error) {
	attr, err := newRelLogAttr(op, a)
	return logClause{attr}, err
}

func (c logClause) scanWitnesses(ctx context.Context, store *recordlayer.FDBRecordStore, w timeWindow, work *queryWork, visit func(string, LogMatch)) error {
	const op = "trace existential"
	if c.attr.none {
		return nil
	}
	var pending []relLogEntry
	var pks []tuple.Tuple
	// flush verifies a batch of long-string candidates on their rows.
	flush := func() error {
		err := loadRecords(store, op, pks, work, func(i int, rec *recordlayer.FDBStoredRecord[proto.Message]) error {
			row, err := logRow(op, rec)
			if err != nil {
				return err
			}
			if !pending[i].rowAgrees(row) {
				return fail("corruption", op, "log index entry disagrees with its record")
			}
			if c.attr.holds(row) {
				m := pending[i].match()
				visit(m.Trace, m)
			}
			return nil
		})
		pending, pks = pending[:0], pks[:0]
		return err
	}
	seen := map[string]bool{}
	err := scanIndex(ctx, store, op, c.attr.index, c.attr.scanRange(w), true, work, func(e *recordlayer.IndexEntry) (bool, error) {
		en, err := c.attr.entry(op, e)
		if err != nil {
			return false, err
		}
		if en.trace == nil || !w.contains(en.effective) || seen[en.eventID] {
			return false, nil
		}
		seen[en.eventID] = true
		if c.attr.long == nil {
			m := en.match()
			visit(m.Trace, m)
			return false, nil
		}
		pending, pks = append(pending, en), append(pks, en.pk)
		if len(pending) < loadRecordsBatch {
			return false, nil
		}
		return false, flush()
	})
	if err != nil || len(pending) == 0 {
		return err
	}
	return flush()
}
