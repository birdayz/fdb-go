package otel

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"math"
	"strings"
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
	// Larger singletons must also pass expanded-write admission, reserving
	// space below FDB's 10MB limit for native conflicts and replacement clears.
	logBatchBudget      = 2 << 20
	maxLogTxnWriteBytes = 8_000_000

	// LogScopeRecord scopes log-record attributes; LogScopeIntrinsic holds
	// indexed top-level fields (event_name) under a reserved scope.
	LogScopeRecord    = "log"
	LogScopeIntrinsic = "intrinsic"

	LogTimeEvent    = "event"
	LogTimeObserved = "observed"
	LogTimeReceiver = "receiver"
)

var logAttributeScopes = []string{"resource", "scope", LogScopeRecord, LogScopeIntrinsic}

// LogRecord retains authoritative OTLP bytes with detached projections; construct a new record to edit them.
// EventID is editable receiver metadata; ingestion chooses event, observed, then receiver time.
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
	original               *logspb.ResourceLogs
}

// OTLP returns a detached envelope preserving all stored/converted protobuf fields.
// For constructed records it builds an envelope and validates hex identities.
func (l LogRecord) OTLP() (*logspb.ResourceLogs, error) {
	rl, err := toResourceLogs(l)
	if err != nil {
		return nil, err
	}
	return proto.Clone(rl).(*logspb.ResourceLogs), nil
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
	writeCost  int // new mutations and native conflicts, excluding a prior image
	unindexed  int64
}

type logBatch struct {
	logs []preparedLog
	cost int
}

func logRecordSpace(keys runKeys, tenant int, eventID string) subspace.Subspace {
	return keys.logs.Sub(int64(tenant), eventID)
}

func logTraceSpace(keys runKeys, tenant int) subspace.Subspace {
	return keys.logTrace.Sub(int64(tenant))
}

func logTermPrefix(keys runKeys, tenant int, a Attribute, width int) subspace.Subspace {
	return keys.logEq.Sub(int64(tenant), a.Scope, digest(a.Key, width), a.Type, digest(a.Value, width))
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
	if l.original != nil {
		return l.original, nil
	}
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
	if len(rl.GetScopeLogs()) != 1 || len(rl.ScopeLogs[0].GetLogRecords()) != 1 || rl.ScopeLogs[0].LogRecords[0] == nil {
		return LogRecord{}, fail("corruption", "log decode", "expected one stored log record")
	}
	l := projectLog(rl)
	if l.Timestamp < 0 || l.ObservedTimestamp < 0 {
		return LogRecord{}, fail("corruption", "log decode", "stored timestamp exceeds int64")
	}
	l.EventID, l.EffectiveTime, l.TimeSource = eventID, effective, source
	if l.TraceID != "" && !validIdentity(l.TraceID, 16) {
		return LogRecord{}, fail("corruption", "log decode", "invalid stored trace identity")
	}
	if l.SpanID != "" && (l.TraceID == "" || !validIdentity(l.SpanID, 8)) {
		return LogRecord{}, fail("corruption", "log decode", "invalid stored span identity")
	}
	return l, nil
}

// projectLog keeps mutable query projections separate from the retained source.
// rl is an owned, single-log envelope and is never mutated after this call.
func projectLog(rl *logspb.ResourceLogs) LogRecord {
	projection := proto.Clone(rl).(*logspb.ResourceLogs)
	sl := projection.ScopeLogs[0]
	rec := sl.LogRecords[0]
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
	return LogRecord{
		original:               rl,
		Timestamp:              clampTime(rec.TimeUnixNano),
		ObservedTimestamp:      clampTime(rec.ObservedTimeUnixNano),
		TraceID:                id(rec.TraceId),
		SpanID:                 id(rec.SpanId),
		Flags:                  rec.Flags,
		SeverityText:           rec.SeverityText,
		SeverityNumber:         int32(rec.SeverityNumber),
		EventName:              rec.EventName,
		Body:                   rec.Body,
		DroppedAttributesCount: rec.DroppedAttributesCount,
		ResourceSchemaURL:      projection.SchemaUrl,
		ResourceAttributes:     projection.GetResource().GetAttributes(),
		ScopeName:              sl.GetScope().GetName(),
		ScopeVersion:           sl.GetScope().GetVersion(),
		ScopeSchemaURL:         sl.SchemaUrl,
		ScopeAttributes:        sl.GetScope().GetAttributes(),
		Attributes:             rec.Attributes,
	}
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
			out = append(out, legacyCanonicalAttribute(list.scope, kv.Key, kv.Value))
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
func logIndexKeys(root runKeys, tenant, width int, l LogRecord) (fdb.Key, []posting, int64) {
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
	if l.original != nil {
		eventID := l.EventID
		l = projectLog(l.original)
		l.EventID = eventID
	}
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
	traceKey, postings, unindexed := logIndexKeys(b.keys, b.cfg.Tenant, b.cfg.HashBytes, l)
	for _, ps := range postings {
		if err := checkCoveringValue(ps.Value); err != nil {
			return preparedLog{}, err
		}
	}
	p := preparedLog{eventID: l.EventID, correlated: traceKey != nil, header: header, chunks: chunks, traceKey: traceKey, postings: postings, logical: int64(len(raw)), unindexed: unindexed}
	p.stored = int64(len(header))
	for _, c := range chunks {
		p.stored += int64(len(c))
	}
	space := logRecordSpace(b.keys, b.cfg.Tenant, l.EventID)
	begin, end := space.FDBRangeKeys()
	// Native 7.3 counts mutation storage AND read/write conflict ranges.
	// Reserve fixed transaction/manifest overhead in addition to every row.
	p.cost = 1024 + len(begin.FDBKey()) + len(end.FDBKey()) + 24
	p.cost += logSetWriteCost(space.Pack(tuple.Tuple{int64(0)}), header)
	for i, chunk := range chunks {
		p.cost += logSetWriteCost(space.Pack(tuple.Tuple{int64(i + 1)}), chunk)
	}
	if traceKey != nil {
		p.cost += logSetWriteCost(traceKey, nil)
	}
	for _, ps := range postings {
		p.cost += logSetWriteCost(ps.Key, ps.Value)
	}
	p.writeCost = p.cost
	if p.cost > maxLogTxnWriteBytes {
		return preparedLog{}, failf("admission", op, "expanded transaction writes of %d bytes exceed %d", p.cost, maxLogTxnWriteBytes)
	}
	return p, nil
}

// Counting a Blob per mutation is conservative: NativeAPI's getSize includes
// MutationList headers/blobs and both conflict vectors (not just key/value bytes).
func logSetWriteCost(key, value []byte) int { return 3*len(key) + len(value) + 69 }
func logClearWriteCost(key []byte) int      { return 4*len(key) + 70 }

func (b *FDBBackend) logReplacementWriteCost(space subspace.Subspace, old LogRecord) int {
	trace, postings, _ := logIndexKeys(b.keys, b.cfg.Tenant, b.cfg.HashBytes, old)
	begin, end := space.FDBRangeKeys()
	// ClearRange mutation and its write conflict; old chunk values are not
	// retransmitted. The serializable read range was charged with the new image.
	cost := 2*(len(begin.FDBKey())+len(end.FDBKey())) + 68
	if trace != nil {
		cost += logClearWriteCost(trace)
	}
	for _, p := range postings {
		cost += logClearWriteCost(p.Key)
	}
	return cost
}

// Preflight every replacement before the first batch writes, so a too-large
// old image cannot turn a mixed request into partially admitted logs.
func (b *FDBBackend) admitLogReplacements(ctx context.Context, items []preparedLog) error {
	costs := make([]int, len(items))
	_, err := runTxnSlot(ctx, b.db, "log replacement admission", func(rc *recordlayer.FDBRecordContext) (any, error) {
		if err := b.readManifest(rc, true); err != nil {
			return nil, err
		}
		for i, item := range items {
			costs[i] = item.writeCost
			space := logRecordSpace(b.keys, b.cfg.Tenant, item.eventID)
			rows, err := rc.Transaction().GetRange(space, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
			if err != nil {
				return nil, err
			}
			if len(rows) != 0 {
				old, err := decodeLogRows(space, item.eventID, rows)
				if err != nil {
					return nil, err
				}
				costs[i] += b.logReplacementWriteCost(space, old)
			}
			if costs[i] > maxLogTxnWriteBytes {
				return nil, failf("admission", "log replacement admission", "native transaction cost %d exceeds %d", costs[i], maxLogTxnWriteBytes)
			}
		}
		return nil, nil
	})
	if err == nil {
		for i := range items {
			items[i].cost = costs[i]
		}
	}
	return err
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
	_, err := runTxnSlot(ctx, b.db, "log batch", func(rc *recordlayer.FDBRecordContext) (any, error) {
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
			r, err := fdb.PrefixRange(logRecordSpace(b.keys, b.cfg.Tenant, l.eventID).Bytes())
			if err != nil {
				return nil, err
			}
			prior[i] = tx.GetRange(r, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll})
		}
		oldLogs := make([]*LogRecord, len(batch.logs))
		cost := 0
		for i, l := range batch.logs {
			space := logRecordSpace(b.keys, b.cfg.Tenant, l.eventID)
			cost += l.writeCost
			rows, err := prior[i].GetSliceWithError()
			if err != nil {
				return nil, err
			}
			if len(rows) > 0 {
				old, err := decodeLogRows(space, l.eventID, rows)
				if err != nil {
					return nil, err
				}
				oldLogs[i] = &old
				cost += b.logReplacementWriteCost(space, old)
			}
		}
		// Recheck prior images changed since preflight; this operational failure
		// can occur after independent batches have committed.
		if cost > maxLogTxnWriteBytes {
			return nil, failf("operation", "log batch", "prior image changed after admission: native transaction cost %d exceeds %d", cost, maxLogTxnWriteBytes)
		}
		for i, l := range batch.logs {
			space := logRecordSpace(b.keys, b.cfg.Tenant, l.eventID)
			if old := oldLogs[i]; old != nil {
				oldTrace, oldPostings, _ := logIndexKeys(b.keys, b.cfg.Tenant, b.cfg.HashBytes, *old)
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

// LogRecordsFromResourceLogs owns each log envelope and exposes detached projections without an EventID.
// Empty/all-zero trace or span IDs remain in source bytes but are uncorrelated in indexes.
func LogRecordsFromResourceLogs(in []*logspb.ResourceLogs) []LogRecord {
	var out []LogRecord
	for _, rl := range in {
		for _, sl := range rl.GetScopeLogs() {
			for _, r := range sl.GetLogRecords() {
				if r == nil {
					continue
				}
				// Copy only this record's ancestors, not sibling scopes or logs.
				scope := &logspb.ScopeLogs{Scope: sl.Scope, SchemaUrl: sl.SchemaUrl, LogRecords: []*logspb.LogRecord{r}}
				scope.ProtoReflect().SetUnknown(sl.ProtoReflect().GetUnknown())
				envelope := &logspb.ResourceLogs{Resource: rl.Resource, SchemaUrl: rl.SchemaUrl, ScopeLogs: []*logspb.ScopeLogs{scope}}
				envelope.ProtoReflect().SetUnknown(rl.ProtoReflect().GetUnknown())
				out = append(out, projectLog(proto.Clone(envelope).(*logspb.ResourceLogs)))
			}
		}
	}
	return out
}

// loadLogs pipelines one range read per event and charges every row.
func (b *FDBBackend) loadLogs(tx fdb.ReadTransaction, ids []string, budget *queryBudget) ([]LogRecord, error) {
	results := make([]fdb.RangeResult, len(ids))
	for i, id := range ids {
		r, err := fdb.PrefixRange(logRecordSpace(b.keys, b.cfg.Tenant, id).Bytes())
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
		if out[i], err = decodeLogRows(logRecordSpace(b.keys, b.cfg.Tenant, id), id, rows); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// readLogs runs fn in one bounded, ownership-checked read transaction.
func (b *FDBBackend) readLogs(ctx context.Context, op string, fn func(fdb.ReadTransaction, *queryBudget) error) error {
	read := func(rc *recordlayer.FDBRecordContext) (any, error) {
		if err := configureStoreContext(rc); err != nil {
			return nil, err
		}
		if err := b.readManifest(rc, true); err != nil {
			return nil, err
		}
		return nil, fn(rc.ReadTransaction(false), b.newBudget(op))
	}
	_, err := b.db.Run(ctx, read)
	return err
}

type logTraceEntry struct {
	span      string
	effective int64
	eventID   string
}

// scanLogTerm visits verified log postings newest first until visit stops it.
func scanLogTerm(tx fdb.ReadTransaction, root runKeys, tenant, width int, a Attribute, w timeWindow, budget *queryBudget, visit func(LogMatch) (bool, error)) error {
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
