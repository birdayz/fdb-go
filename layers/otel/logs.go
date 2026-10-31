package otel

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"github.com/andybalholm/brotli"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

const (
	logRecordSubspace = "logs_v1"
	logTraceSubspace  = "log_trace_v1"
	logEqSubspace     = "log_eq_v1"

	logFormatVersion = int64(1)
	logCodecRaw      = int64(0)
	logCodecBrotli   = int64(1)
	logBrotliLevel   = 4
	logCompressMin   = 256
	// Stored payloads up to logInlineBytes live in the header row; larger ones
	// split into logChunkBytes rows at suffixes 1..n, below FDB's 100KB value cap.
	logInlineBytes    = 90000
	logChunkBytes     = 90000
	maxLogRawBytes    = 8 << 20
	maxLogStoredBytes = 4 << 20
	maxLogEventID     = 256
	// logBatchBudget is the soft per-transaction write estimate; one larger
	// log still commits alone, well under FDB's 10MB transaction limit.
	logBatchBudget = 2 << 20

	// LogScopeRecord scopes log-record attributes; LogScopeIntrinsic holds
	// indexed top-level fields (event_name) under a reserved scope.
	LogScopeRecord    = "log"
	LogScopeIntrinsic = "intrinsic"

	LogTimeEvent    = "event"
	LogTimeObserved = "observed"
	LogTimeReceiver = "receiver"
)

var logAttributeScopes = []string{"resource", "scope", LogScopeRecord, LogScopeIntrinsic}

// LogRecord is one OTLP log/event with its resource and scope provenance.
// EffectiveTime and TimeSource are assigned at ingestion: event time, else
// observed time, else receiver time; Timestamp keeps the original value.
type LogRecord struct {
	EventID                string               `json:"event_id"`
	Timestamp              int64                `json:"time_unix_nano"`
	ObservedTimestamp      int64                `json:"observed_time_unix_nano"`
	TraceID                string               `json:"trace_id,omitempty"`
	SpanID                 string               `json:"span_id,omitempty"`
	Flags                  uint32               `json:"flags,omitempty"`
	SeverityText           string               `json:"severity_text,omitempty"`
	SeverityNumber         int32                `json:"severity_number,omitempty"`
	EventName              string               `json:"event_name,omitempty"`
	Body                   *commonpb.AnyValue   `json:"body,omitempty"`
	DroppedAttributesCount uint32               `json:"dropped_attributes_count,omitempty"`
	ResourceSchemaURL      string               `json:"resource_schema_url,omitempty"`
	ResourceAttributes     []*commonpb.KeyValue `json:"resource_attributes,omitempty"`
	ScopeName              string               `json:"scope_name,omitempty"`
	ScopeVersion           string               `json:"scope_version,omitempty"`
	ScopeSchemaURL         string               `json:"scope_schema_url,omitempty"`
	ScopeAttributes        []*commonpb.KeyValue `json:"scope_attributes,omitempty"`
	Attributes             []*commonpb.KeyValue `json:"attributes,omitempty"`
	EffectiveTime          int64                `json:"effective_time_unix_nano"`
	TimeSource             string               `json:"time_source"`
}

// LogMatch is one verified log witness from a log equality posting.
type LogMatch struct {
	EventID string `json:"event_id"`
	Trace   string `json:"trace_id,omitempty"`
	Span    string `json:"span_id,omitempty"`
	Time    int64  `json:"time_unix_nano"`
}

// LogExportStats describes one ExportLogs call. EventIDs is aligned with the
// input, including generated IDs; committed counters cover committed batches.
type LogExportStats struct {
	ReceivedLogs        int64    `json:"received_logs"`
	DuplicateLogs       int64    `json:"duplicate_logs"`
	GeneratedIDs        int64    `json:"generated_ids"`
	Logs                int64    `json:"logs"`
	Batches             int64    `json:"batches"`
	Replaced            int64    `json:"replaced"`
	Correlated          int64    `json:"correlated"`
	LogicalBytes        int64    `json:"logical_bytes"`
	StoredBytes         int64    `json:"stored_bytes"`
	UnindexedAttributes int64    `json:"unindexed_attributes"`
	EventIDs            []string `json:"event_ids"`
}

type preparedLog struct {
	eventID    string
	correlated bool
	header     []byte
	chunks     [][]byte
	traceKey   fdb.Key
	postings   []posting
	logical    int64
	stored     int64
	cost       int
	unindexed  int64
}

type logBatch struct {
	logs []preparedLog
	cost int
}

func logRecordSpace(root subspace.Subspace, tenant int, eventID string) subspace.Subspace {
	return root.Sub(logRecordSubspace, int64(tenant), eventID)
}

func logTraceSpace(root subspace.Subspace, tenant int) subspace.Subspace {
	return root.Sub(logTraceSubspace, int64(tenant))
}

func logTermPrefix(root subspace.Subspace, tenant int, a Attribute, width int) subspace.Subspace {
	return root.Sub(logEqSubspace, int64(tenant), a.Scope, digest(a.Key, width), a.Type, digest(a.Value, width))
}

// newEventID is a random RFC 4122 version 4 UUID in lowercase hex.
func newEventID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", wrap("operation", "log event ID", err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return hex.EncodeToString(b[:]), nil
}

func nonNilKeyValues(kvs []*commonpb.KeyValue) []*commonpb.KeyValue {
	var out []*commonpb.KeyValue
	for _, kv := range kvs {
		if kv != nil {
			out = append(out, kv)
		}
	}
	return out
}

// toResourceLogs is the stored single-record OTLP form of l.
func toResourceLogs(l LogRecord) (*logspb.ResourceLogs, error) {
	rec := &logspb.LogRecord{
		TimeUnixNano:           uint64(l.Timestamp),
		ObservedTimeUnixNano:   uint64(l.ObservedTimestamp),
		SeverityNumber:         logspb.SeverityNumber(l.SeverityNumber),
		SeverityText:           l.SeverityText,
		Body:                   l.Body,
		Attributes:             nonNilKeyValues(l.Attributes),
		DroppedAttributesCount: l.DroppedAttributesCount,
		Flags:                  l.Flags,
		EventName:              l.EventName,
	}
	var err error
	if l.TraceID != "" {
		if rec.TraceId, err = hex.DecodeString(l.TraceID); err != nil {
			return nil, wrap("admission", "log trace identity", err)
		}
	}
	if l.SpanID != "" {
		if rec.SpanId, err = hex.DecodeString(l.SpanID); err != nil {
			return nil, wrap("admission", "log span identity", err)
		}
	}
	return &logspb.ResourceLogs{
		Resource:  &resourcepb.Resource{Attributes: nonNilKeyValues(l.ResourceAttributes)},
		SchemaUrl: l.ResourceSchemaURL,
		ScopeLogs: []*logspb.ScopeLogs{{
			Scope:      &commonpb.InstrumentationScope{Name: l.ScopeName, Version: l.ScopeVersion, Attributes: nonNilKeyValues(l.ScopeAttributes)},
			SchemaUrl:  l.ScopeSchemaURL,
			LogRecords: []*logspb.LogRecord{rec},
		}},
	}, nil
}

// fromResourceLogs restores a stored record; it rejects any other shape.
func fromResourceLogs(eventID string, effective int64, source string, rl *logspb.ResourceLogs) (LogRecord, error) {
	if len(rl.GetScopeLogs()) != 1 || len(rl.ScopeLogs[0].GetLogRecords()) != 1 {
		return LogRecord{}, fail("corruption", "log decode", "expected one stored log record")
	}
	sl := rl.ScopeLogs[0]
	rec := sl.LogRecords[0]
	if rec.TimeUnixNano > math.MaxInt64 || rec.ObservedTimeUnixNano > math.MaxInt64 {
		return LogRecord{}, fail("corruption", "log decode", "stored timestamp exceeds int64")
	}
	l := LogRecord{
		EventID:                eventID,
		Timestamp:              int64(rec.TimeUnixNano),
		ObservedTimestamp:      int64(rec.ObservedTimeUnixNano),
		Flags:                  rec.Flags,
		SeverityText:           rec.SeverityText,
		SeverityNumber:         int32(rec.SeverityNumber),
		EventName:              rec.EventName,
		Body:                   rec.Body,
		DroppedAttributesCount: rec.DroppedAttributesCount,
		ResourceSchemaURL:      rl.SchemaUrl,
		ResourceAttributes:     rl.GetResource().GetAttributes(),
		ScopeName:              sl.GetScope().GetName(),
		ScopeVersion:           sl.GetScope().GetVersion(),
		ScopeSchemaURL:         sl.SchemaUrl,
		ScopeAttributes:        sl.GetScope().GetAttributes(),
		Attributes:             rec.Attributes,
		EffectiveTime:          effective,
		TimeSource:             source,
	}
	if len(rec.TraceId) > 0 {
		l.TraceID = hex.EncodeToString(rec.TraceId)
		if !validIdentity(l.TraceID, 16) {
			return LogRecord{}, fail("corruption", "log decode", "invalid stored trace identity")
		}
	}
	if len(rec.SpanId) > 0 {
		l.SpanID = hex.EncodeToString(rec.SpanId)
		if l.TraceID == "" || !validIdentity(l.SpanID, 8) {
			return LogRecord{}, fail("corruption", "log decode", "invalid stored span identity")
		}
	}
	return l, nil
}

// logIndexed is the canonical indexed attribute list of one log, in posting
// ordinal order: resource, scope, log attributes, then intrinsic event_name.
func logIndexed(l LogRecord) ([]Attribute, int64) {
	var out []Attribute
	var unindexed int64
	for _, list := range []struct {
		scope string
		kvs   []*commonpb.KeyValue
	}{{"resource", l.ResourceAttributes}, {"scope", l.ScopeAttributes}, {LogScopeRecord, l.Attributes}} {
		set := canonicalAttributes(list.kvs)
		unindexed += set.unindexed
		for _, kv := range set.indexed {
			out = append(out, CanonicalAttribute(list.scope, kv.Key, kv.Value))
		}
	}
	if l.EventName != "" {
		if len(l.EventName) > maxIndexedValueBytes {
			unindexed++
		} else {
			out = append(out, Attribute{Scope: LogScopeIntrinsic, Key: "event_name", Type: "string", Value: l.EventName})
		}
	}
	return out, unindexed
}

func encodeLogCovering(a Attribute, effective int64, trace, span string) []byte {
	return tuple.Tuple{a.Scope, a.Key, a.Type, a.Value, effective, trace, span}.Pack()
}

func decodeLogCovering(value []byte) (Attribute, int64, string, string, error) {
	bad := func() (Attribute, int64, string, string, error) {
		return Attribute{}, 0, "", "", fail("corruption", "log posting", "malformed log covering value")
	}
	fields, err := tuple.Unpack(value)
	if err != nil || len(fields) != 7 {
		return bad()
	}
	var s [6]string
	for i, at := range []int{0, 1, 2, 3, 5, 6} {
		text, ok := fields[at].(string)
		if !ok || !utf8.ValidString(text) {
			return bad()
		}
		s[i] = text
	}
	effective, ok := fields[4].(int64)
	if !ok || len(s[1]) > maxIndexedKeyBytes || len(s[3]) > maxIndexedValueBytes {
		return bad()
	}
	return Attribute{Scope: s[0], Key: s[1], Type: s[2], Value: s[3]}, effective, s[4], s[5], nil
}

// logIndexKeys derives every index entry of one stored log; replacing a log
// recomputes its old entries from the stored record to retract them exactly.
func logIndexKeys(root subspace.Subspace, tenant, width int, l LogRecord) (fdb.Key, []posting, int64) {
	var traceKey fdb.Key
	if l.TraceID != "" {
		traceKey = logTraceSpace(root, tenant).Pack(tuple.Tuple{l.TraceID, l.SpanID, l.EffectiveTime, l.EventID})
	}
	attrs, unindexed := logIndexed(l)
	out := make([]posting, 0, len(attrs))
	for ordinal, a := range attrs {
		key := logTermPrefix(root, tenant, a, width).Pack(tuple.Tuple{-l.EffectiveTime, l.EventID, int64(ordinal)})
		out = append(out, posting{Key: key, Value: encodeLogCovering(a, l.EffectiveTime, l.TraceID, l.SpanID)})
	}
	return traceKey, out, unindexed
}

func compressLog(raw []byte) []byte {
	var buf bytes.Buffer
	w := brotli.NewWriterLevel(&buf, logBrotliLevel)
	if _, err := w.Write(raw); err != nil {
		return nil
	}
	if err := w.Close(); err != nil {
		return nil
	}
	return buf.Bytes()
}

// encodeLogValue returns the header row and any chunk rows. The header is a
// tuple (version, codec, raw_len, stored_len, chunks, effective, source, inline).
func encodeLogValue(raw []byte, effective int64, source string) ([]byte, [][]byte, error) {
	stored, codec := raw, logCodecRaw
	if len(raw) >= logCompressMin {
		if c := compressLog(raw); c != nil && len(c) < len(raw) {
			stored, codec = c, logCodecBrotli
		}
	}
	if len(stored) > maxLogStoredBytes {
		return nil, nil, failf("admission", "log encode", "stored log of %d bytes exceeds %d", len(stored), maxLogStoredBytes)
	}
	inline := stored
	var chunks [][]byte
	if len(stored) > logInlineBytes {
		inline = []byte{}
		for rest := stored; len(rest) > 0; {
			k := min(len(rest), logChunkBytes)
			chunks = append(chunks, rest[:k:k])
			rest = rest[k:]
		}
	}
	header := tuple.Tuple{logFormatVersion, codec, int64(len(raw)), int64(len(stored)), int64(len(chunks)), effective, source, inline}.Pack()
	return header, chunks, nil
}

// decodeLogRows validates one log's rows, read from its record subspace.
func decodeLogRows(space subspace.Subspace, eventID string, rows []fdb.KeyValue) (LogRecord, error) {
	bad := func(msg string) (LogRecord, error) { return LogRecord{}, fail("corruption", "log decode", msg) }
	if len(rows) == 0 {
		return bad("log record is missing")
	}
	for i, kv := range rows {
		t, err := space.Unpack(kv.Key)
		if err != nil || len(t) != 1 {
			return bad("malformed log row key")
		}
		if n, ok := t[0].(int64); !ok || n != int64(i) {
			return bad("log chunk missing or out of order")
		}
	}
	h, err := tuple.Unpack(rows[0].Value)
	if err != nil || len(h) != 8 {
		return bad("malformed log header")
	}
	var n [6]int64
	for i := range n {
		v, ok := h[i].(int64)
		if !ok {
			return bad("malformed log header field")
		}
		n[i] = v
	}
	version, codec, rawLen, storedLen, chunks, effective := n[0], n[1], n[2], n[3], n[4], n[5]
	source, sok := h[6].(string)
	inline, iok := h[7].([]byte)
	if !sok || !iok || version != logFormatVersion || (codec != logCodecRaw && codec != logCodecBrotli) || rawLen < 0 || rawLen > maxLogRawBytes || storedLen < 0 || storedLen > maxLogStoredBytes || effective < 0 {
		return bad("unsupported log header")
	}
	if source != LogTimeEvent && source != LogTimeObserved && source != LogTimeReceiver {
		return bad("unknown log time source")
	}
	if chunks != int64(len(rows)-1) || (chunks > 0 && len(inline) != 0) {
		return bad("log chunk count disagrees with header")
	}
	payload := inline
	if chunks > 0 {
		payload = make([]byte, 0, storedLen)
		for _, kv := range rows[1:] {
			payload = append(payload, kv.Value...)
		}
	}
	if int64(len(payload)) != storedLen || (codec == logCodecRaw && storedLen != rawLen) {
		return bad("log payload length disagrees with header")
	}
	raw := payload
	if codec == logCodecBrotli {
		raw, err = io.ReadAll(io.LimitReader(brotli.NewReader(bytes.NewReader(payload)), rawLen+1))
		if err != nil {
			return LogRecord{}, typedError("corruption", "log decode", "brotli decode", err)
		}
		if int64(len(raw)) != rawLen {
			return bad("decoded log length disagrees with header")
		}
	}
	rl := &logspb.ResourceLogs{}
	if err := proto.Unmarshal(raw, rl); err != nil {
		return LogRecord{}, wrap("corruption", "log decode", err)
	}
	return fromResourceLogs(eventID, effective, source, rl)
}

// prepareLog validates and encodes one log; receiver supplies the fallback time.
func (b *FDBBackend) prepareLog(l LogRecord, receiver int64) (preparedLog, error) {
	const op = "log admission"
	if l.Timestamp < 0 || l.ObservedTimestamp < 0 {
		return preparedLog{}, fail("admission", op, "timestamps must not be negative")
	}
	if len(l.EventID) > maxLogEventID || !utf8.ValidString(l.EventID) {
		return preparedLog{}, failf("admission", op, "event ID must be valid UTF-8 of at most %d bytes", maxLogEventID)
	}
	l.TraceID, l.SpanID = strings.ToLower(l.TraceID), strings.ToLower(l.SpanID)
	if l.TraceID != "" && !validIdentity(l.TraceID, 16) {
		return preparedLog{}, fail("admission", op, "trace ID must be 32 hex digits and nonzero")
	}
	if l.SpanID != "" && (l.TraceID == "" || !validIdentity(l.SpanID, 8)) {
		return preparedLog{}, fail("admission", op, "span ID must be 16 hex digits, nonzero, with a trace ID")
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
		return preparedLog{}, err
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(rl)
	if err != nil {
		return preparedLog{}, wrap("admission", "log encode", err)
	}
	if len(raw) > maxLogRawBytes {
		return preparedLog{}, failf("admission", op, "encoded log of %d bytes exceeds %d", len(raw), maxLogRawBytes)
	}
	header, chunks, err := encodeLogValue(raw, l.EffectiveTime, l.TimeSource)
	if err != nil {
		return preparedLog{}, err
	}
	traceKey, postings, unindexed := logIndexKeys(b.root, b.cfg.Tenant, b.cfg.HashBytes, l)
	p := preparedLog{eventID: l.EventID, correlated: traceKey != nil, header: header, chunks: chunks, traceKey: traceKey, postings: postings, logical: int64(len(raw)), unindexed: unindexed}
	p.stored = int64(len(header))
	for _, c := range chunks {
		p.stored += int64(len(c))
	}
	// Writes plus the clear of a replaced log's equally sized old entries.
	p.cost = 1024 + int(p.stored) + len(traceKey)
	for _, ps := range postings {
		p.cost += 2 * (len(ps.Key) + len(ps.Value))
	}
	return p, nil
}

func partitionLogs(items []preparedLog, size int) []logBatch {
	var out []logBatch
	var cur logBatch
	for _, it := range items {
		if len(cur.logs) > 0 && (len(cur.logs) == size || cur.cost+it.cost > logBatchBudget) {
			out = append(out, cur)
			cur = logBatch{}
		}
		cur.logs = append(cur.logs, it)
		cur.cost += it.cost
	}
	if len(cur.logs) > 0 {
		out = append(out, cur)
	}
	return out
}

// saveLogBatch replaces each log atomically: it reads the prior version with
// conflicts, retracts that version's index entries and writes the new ones.
func (b *FDBBackend) saveLogBatch(ctx context.Context, batch logBatch) (int64, error) {
	var replaced int64
	_, err := b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		replaced = 0
		if err := configureStoreContext(rc); err != nil {
			return nil, err
		}
		if err := b.readManifest(rc, true); err != nil {
			return nil, err
		}
		tx := rc.Transaction()
		prior := make([]fdb.RangeResult, len(batch.logs))
		for i, l := range batch.logs {
			r, err := fdb.PrefixRange(logRecordSpace(b.root, b.cfg.Tenant, l.eventID).Bytes())
			if err != nil {
				return nil, err
			}
			prior[i] = tx.GetRange(r, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll})
		}
		for i, l := range batch.logs {
			space := logRecordSpace(b.root, b.cfg.Tenant, l.eventID)
			rows, err := prior[i].GetSliceWithError()
			if err != nil {
				return nil, err
			}
			if len(rows) > 0 {
				old, err := decodeLogRows(space, l.eventID, rows)
				if err != nil {
					return nil, err
				}
				oldTrace, oldPostings, _ := logIndexKeys(b.root, b.cfg.Tenant, b.cfg.HashBytes, old)
				if oldTrace != nil {
					tx.Clear(oldTrace)
				}
				for _, p := range oldPostings {
					tx.Clear(p.Key)
				}
				r, err := fdb.PrefixRange(space.Bytes())
				if err != nil {
					return nil, err
				}
				tx.ClearRange(r)
				replaced++
			}
			tx.Set(space.Pack(tuple.Tuple{int64(0)}), l.header)
			for n, c := range l.chunks {
				tx.Set(space.Pack(tuple.Tuple{int64(n + 1)}), c)
			}
			if l.traceKey != nil {
				tx.Set(l.traceKey, []byte{})
			}
			for _, p := range l.postings {
				tx.Set(p.Key, p.Value)
			}
		}
		return nil, ctx.Err()
	})
	return replaced, err
}

// ExportLogs validates every log before writing, so an inadmissible log fails
// the call without partial writes; batches then commit independently. Logs
// without an EventID get a receiver UUID here, outside any retry loop, so a
// retried transaction never duplicates them; a resent request can.
func (e *Exporter) ExportLogs(ctx context.Context, logs []LogRecord) (LogExportStats, error) {
	const op = "log export"
	stats := LogExportStats{EventIDs: make([]string, len(logs))}
	if err := e.begin(op); err != nil {
		return stats, err
	}
	defer e.active.Done()
	b := e.backend
	receiver := time.Now().UnixNano()
	var items []preparedLog
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
		p, err := b.prepareLog(l, receiver)
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
	batches := partitionLogs(items, e.cfg.BatchSize)
	if len(batches) == 0 {
		return stats, nil
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan logBatch)
	var mu sync.Mutex
	var firstErr error
	committed := 0
	var workers sync.WaitGroup
	for range min(e.cfg.Workers, len(batches)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for batch := range jobs {
				if workCtx.Err() != nil {
					continue
				}
				replaced, err := b.saveLogBatch(workCtx, batch)
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
					for _, l := range batch.logs {
						stats.Logs++
						stats.LogicalBytes += l.logical
						stats.StoredBytes += l.stored
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

// ConsumeLogs accepts OTLP ResourceLogs from a Collector or OTLP receiver.
func (e *Exporter) ConsumeLogs(ctx context.Context, logs []*logspb.ResourceLogs) error {
	_, err := e.ExportLogs(ctx, LogRecordsFromResourceLogs(logs))
	return err
}

// LogRecordsFromResourceLogs flattens OTLP logs; empty or all-zero trace and
// span IDs mean uncorrelated. OTLP carries no stable event ID, so none is set.
func LogRecordsFromResourceLogs(in []*logspb.ResourceLogs) []LogRecord {
	var out []LogRecord
	id := func(b []byte) string {
		if len(b) == 0 || zeroID(b) {
			return ""
		}
		return hex.EncodeToString(b)
	}
	clampTime := func(t uint64) int64 {
		if t > math.MaxInt64 {
			return -1 // rejected at admission rather than silently clamped
		}
		return int64(t)
	}
	for _, rl := range in {
		for _, sl := range rl.GetScopeLogs() {
			for _, r := range sl.GetLogRecords() {
				if r == nil {
					continue
				}
				out = append(out, LogRecord{
					Timestamp:              clampTime(r.TimeUnixNano),
					ObservedTimestamp:      clampTime(r.ObservedTimeUnixNano),
					TraceID:                id(r.TraceId),
					SpanID:                 id(r.SpanId),
					Flags:                  r.Flags,
					SeverityText:           r.SeverityText,
					SeverityNumber:         int32(r.SeverityNumber),
					EventName:              r.EventName,
					Body:                   r.Body,
					DroppedAttributesCount: r.DroppedAttributesCount,
					ResourceSchemaURL:      rl.SchemaUrl,
					ResourceAttributes:     rl.GetResource().GetAttributes(),
					ScopeName:              sl.GetScope().GetName(),
					ScopeVersion:           sl.GetScope().GetVersion(),
					ScopeSchemaURL:         sl.SchemaUrl,
					ScopeAttributes:        sl.GetScope().GetAttributes(),
					Attributes:             r.Attributes,
				})
			}
		}
	}
	return out
}

// loadLogs pipelines one range read per event and charges every row.
func (b *FDBBackend) loadLogs(tx fdb.ReadTransaction, ids []string, budget *queryBudget) ([]LogRecord, error) {
	results := make([]fdb.RangeResult, len(ids))
	for i, id := range ids {
		r, err := fdb.PrefixRange(logRecordSpace(b.root, b.cfg.Tenant, id).Bytes())
		if err != nil {
			return nil, err
		}
		results[i] = tx.GetRange(r, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll})
	}
	out := make([]LogRecord, len(ids))
	for i, id := range ids {
		rows, err := results[i].GetSliceWithError()
		if err != nil {
			return nil, err
		}
		for _, kv := range rows {
			if err := budget.charge(kv); err != nil {
				return nil, err
			}
		}
		if out[i], err = decodeLogRows(logRecordSpace(b.root, b.cfg.Tenant, id), id, rows); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// readLogs runs fn in one bounded, ownership-checked read transaction.
func (b *FDBBackend) readLogs(ctx context.Context, op string, fn func(fdb.ReadTransaction, *queryBudget) error) error {
	_, err := b.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		if err := configureStoreContext(rc); err != nil {
			return nil, err
		}
		if err := b.readManifest(rc, true); err != nil {
			return nil, err
		}
		return nil, fn(rc.ReadTransaction(false), b.newBudget(op))
	})
	return err
}

type logTraceEntry struct {
	span      string
	effective int64
	eventID   string
}

// QueryLogsByTrace returns logs correlated to traceID in (effective time,
// event ID) order; a nonempty spanID restricts them to that span.
func (e *Exporter) QueryLogsByTrace(ctx context.Context, traceID, spanID string) ([]LogRecord, error) {
	const op = "log trace query"
	traceID, spanID = strings.ToLower(traceID), strings.ToLower(spanID)
	if !validIdentity(traceID, 16) || (spanID != "" && !validIdentity(spanID, 8)) {
		return []LogRecord{}, fail("config", op, "expected hex nonzero 16-byte trace and optional 8-byte span identifiers")
	}
	if err := e.begin(op); err != nil {
		return []LogRecord{}, err
	}
	defer e.active.Done()
	b := e.backend
	out := []LogRecord{}
	err := b.readLogs(ctx, op, func(tx fdb.ReadTransaction, budget *queryBudget) error {
		out = []LogRecord{}
		space := logTraceSpace(b.root, b.cfg.Tenant)
		prefix := tuple.Tuple{traceID}
		if spanID != "" {
			prefix = append(prefix, spanID)
		}
		r, err := fdb.PrefixRange(space.Pack(prefix))
		if err != nil {
			return err
		}
		var entries []logTraceEntry
		it := tx.GetRange(r, fdb.RangeOptions{Mode: fdb.StreamingModeIterator}).Iterator()
		for it.Advance() {
			kv, err := it.Get()
			if err != nil {
				return err
			}
			if err := budget.charge(kv); err != nil {
				return err
			}
			t, err := space.Unpack(kv.Key)
			if err != nil || len(t) != 4 || len(kv.Value) != 0 {
				return fail("corruption", op, "malformed log trace entry")
			}
			tr, tok := t[0].(string)
			sp, sok := t[1].(string)
			eff, eok := t[2].(int64)
			id, iok := t[3].(string)
			if !tok || !sok || !eok || !iok || tr != traceID || (spanID != "" && sp != spanID) {
				return fail("corruption", op, "malformed log trace entry")
			}
			entries = append(entries, logTraceEntry{sp, eff, id})
		}
		if _, err := it.Get(); err != nil {
			return err
		}
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].effective != entries[j].effective {
				return entries[i].effective < entries[j].effective
			}
			return entries[i].eventID < entries[j].eventID
		})
		ids := make([]string, len(entries))
		for i, en := range entries {
			ids[i] = en.eventID
		}
		logs, err := b.loadLogs(tx, ids, budget)
		if err != nil {
			return err
		}
		for i, l := range logs {
			en := entries[i]
			if l.TraceID != traceID || l.SpanID != en.span || l.EffectiveTime != en.effective {
				return fail("corruption", op, "log trace entry disagrees with its record")
			}
		}
		out = logs
		return nil
	})
	return out, err
}

// scanLogTerm visits verified log postings newest first until visit stops it.
func scanLogTerm(tx fdb.ReadTransaction, root subspace.Subspace, tenant, width int, a Attribute, w timeWindow, budget *queryBudget, visit func(LogMatch) (bool, error)) error {
	term := logTermPrefix(root, tenant, a, width)
	b, e := term.FDBRangeKeys()
	begin, end := b.FDBKey(), e.FDBKey()
	if w.set {
		if w.empty {
			return nil
		}
		begin, end = w.descendingRange(term)
	}
	it := tx.GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{Mode: fdb.StreamingModeIterator}).Iterator()
	for it.Advance() {
		kv, err := it.Get()
		if err != nil {
			return err
		}
		if err := budget.charge(kv); err != nil {
			return err
		}
		got, effective, trace, span, err := decodeLogCovering(kv.Value)
		if err != nil {
			return err
		}
		t, err := term.Unpack(kv.Key)
		if err != nil || len(t) != 3 {
			return fail("corruption", "log posting", "malformed log posting key")
		}
		ts, tok := t[0].(int64)
		id, iok := t[1].(string)
		_, ook := t[2].(int64)
		if !tok || !iok || !ook || ts != -effective || (trace != "" && !validIdentity(trace, 16)) || (span != "" && !validIdentity(span, 8)) {
			return fail("corruption", "log posting", "malformed log posting identity")
		}
		if got != a || !w.contains(effective) {
			continue
		}
		stop, err := visit(LogMatch{EventID: id, Trace: trace, Span: span, Time: effective})
		if err != nil || stop {
			return err
		}
	}
	_, err := it.Get()
	return err
}

// logHasAttribute re-derives the canonical indexed set from the record itself.
func logHasAttribute(l LogRecord, a Attribute) bool {
	attrs, _ := logIndexed(l)
	for _, x := range attrs {
		if x == a {
			return true
		}
	}
	return false
}

// QueryLogsByAttribute returns up to limit logs, newest first by (effective
// time, event ID), whose canonical attribute equals a, correlated or not.
func (e *Exporter) QueryLogsByAttribute(ctx context.Context, a Attribute, limit int) ([]LogRecord, error) {
	const op = "log attribute query"
	if err := validateQueryAttribute(op, a, logAttributeScopes...); err != nil {
		return []LogRecord{}, err
	}
	if limit < 0 {
		return []LogRecord{}, fail("config", op, "limit must not be negative")
	}
	if limit == 0 {
		limit = e.cfg.K
	}
	if err := e.begin(op); err != nil {
		return []LogRecord{}, err
	}
	defer e.active.Done()
	b := e.backend
	out := []LogRecord{}
	err := b.readLogs(ctx, op, func(tx fdb.ReadTransaction, budget *queryBudget) error {
		out = []LogRecord{}
		var matches []LogMatch
		seen := map[string]bool{}
		err := scanLogTerm(tx, b.root, b.cfg.Tenant, b.cfg.HashBytes, a, timeWindow{}, budget, func(m LogMatch) (bool, error) {
			if !seen[m.EventID] {
				seen[m.EventID] = true
				matches = append(matches, m)
			}
			return len(matches) == limit, nil
		})
		if err != nil {
			return err
		}
		ids := make([]string, len(matches))
		for i, m := range matches {
			ids[i] = m.EventID
		}
		logs, err := b.loadLogs(tx, ids, budget)
		if err != nil {
			return err
		}
		for i, l := range logs {
			m := matches[i]
			if l.EffectiveTime != m.Time || l.TraceID != m.Trace || l.SpanID != m.Span || !logHasAttribute(l, a) {
				return fail("corruption", op, "log posting disagrees with its record")
			}
		}
		out = logs
		return nil
	})
	return out, err
}
