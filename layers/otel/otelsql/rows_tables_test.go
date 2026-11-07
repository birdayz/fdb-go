package otelsql

import (
	"bytes"
	"crypto/sha256"
	"strings"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

func fixtureLogs() []Log {
	all := fixtureAttributes()
	return []Log{
		{EventID: "e0", Log: &logspb.LogRecord{TraceId: make([]byte, 16), SpanId: make([]byte, 8)}, EffectiveNs: 5, TimeSource: "receiver"},
		{
			EventID: "e1", EffectiveNs: 10, TimeSource: "event", OTLP: []byte("envelope"),
			Resource:          &resourcepb.Resource{Attributes: append([]*commonpb.KeyValue{kvStr("service.name", "svc")}, all...)},
			ResourceSchemaURL: "https://res", Scope: &commonpb.InstrumentationScope{Name: "lib", Version: "1", Attributes: all}, ScopeSchemaURL: "https://scope",
			Log: &logspb.LogRecord{
				TimeUnixNano: 10, ObservedTimeUnixNano: 11, TraceId: []byte{0, 1}, SpanId: []byte{2}, Flags: 1,
				SeverityNumber: logspb.SeverityNumber_SEVERITY_NUMBER_ERROR, SeverityText: "ERROR", EventName: "evt",
				Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "body"}}, Attributes: all, DroppedAttributesCount: 2,
			},
		},
		{EventID: "e2", Log: &logspb.LogRecord{EventName: strings.Repeat("n", MaxIndexedStringBytes+1), Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 3}}}},
	}
}

func TestBuildLogCompiled(t *testing.T) {
	t.Parallel()
	logs := fixtureLogs()
	r, err := BuildLogCompiled(logs[0])
	if err != nil {
		t.Fatal(err)
	}
	// All-zero IDs are OTLP's "no trace": NULL, so LOGS_TRACE holds NULLs.
	if r.TRACE_ID != nil || r.SPAN_ID != nil || r.GetEFFECTIVE_NS() != 5 || r.GetTIME_SOURCE() != "receiver" || r.EVENT_NAME != nil || r.BODY_TEXT != nil {
		t.Fatalf("uncorrelated log %v", r)
	}
	r, err = BuildLogCompiled(logs[1])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(r.TRACE_ID, []byte{0, 1}) || !bytes.Equal(r.SPAN_ID, []byte{2}) || r.GetEVENT_NAME() != "evt" || r.GetBODY_TEXT() != "body" ||
		r.GetSERVICE_NAME() != "svc" || r.GetSEVERITY_NUMBER() != 17 || r.GetTIME_NS() != 10 || r.GetOBSERVED_NS() != 11 || r.GetSCOPE_NAME() != "lib" {
		t.Fatalf("correlated log %v", r)
	}
	// The log scope's arrays are the canonical set, as a span's.
	var logKeys []string
	for _, a := range r.GetSTR_ATTRS().GetValues() {
		if a.GetSCOPE() == ScopeLog {
			logKeys = append(logKeys, a.GetATTR_KEY())
		}
	}
	if len(logKeys) != 4 || logKeys[0] != "s" || logKeys[3] != "gen_ai.input.messages" || len(r.GetLONG_STR_ATTRS().GetValues()) != 3*2 {
		t.Fatalf("log str attrs %q, %d long", logKeys, len(r.GetLONG_STR_ATTRS().GetValues()))
	}
	if p := LogProjection(logs[1]); p != (Projection{Normalized: true, Unindexed: 3 * 8}) {
		t.Fatalf("projection %+v", p)
	}
	// An event name LOGS_EVENT cannot key is counted and not projected; a
	// non-string body has no body_text.
	r, err = BuildLogCompiled(logs[2])
	if err != nil || r.EVENT_NAME != nil || r.BODY_TEXT != nil {
		t.Fatalf("long event name: %v %v", r, err)
	}
	if p := LogProjection(logs[2]); p != (Projection{Unindexed: 1}) {
		t.Fatalf("long event name projection %+v", p)
	}
	for _, bad := range []Log{{EventID: "x"}, {Log: &logspb.LogRecord{}}} {
		if _, err := BuildLogCompiled(bad); err == nil {
			t.Errorf("BuildLogCompiled(%+v) succeeded", bad)
		}
	}
}

func TestBuildMessageCompiled(t *testing.T) {
	t.Parallel()
	hash := MessageContentHash("any", []byte("hi"))
	sum := sha256.Sum256(append([]byte("otel_msg_v2\x00any\x00"), "hi"...))
	if !bytes.Equal(hash, sum[:MessageHashBytes]) || bytes.Equal(hash, MessageContentHash("json", []byte("hi"))) {
		t.Fatalf("content hash %x", hash)
	}
	r, err := BuildMessageCompiled(MessagePart{Writer: 17, Seq: 2, Text: &MessageText{Role: "", Content: ""}, ContentHash: hash, Encoding: "any", RawBytes: 0, Parts: 1})
	if err != nil {
		t.Fatal(err)
	}
	// An empty role and text are values (the role is a MESSAGES_TEXT group);
	// part 0's whole-message columns are present even when zero.
	if r.GetSHARD() != 1 || r.ROLE == nil || r.CONTENT == nil || r.TOKEN_OFFSET != nil || r.RAW != nil || r.RAW_BYTES == nil || r.ENCODING == nil ||
		r.GetPARTS() != 1 || r.UNINDEXED_TOKENS != nil || r.LARGE != nil {
		t.Fatalf("part 0 %v", r)
	}
	r, err = BuildMessageCompiled(MessagePart{Writer: 17, Seq: 2, Part: 3, Raw: []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	if r.ROLE != nil || r.CONTENT != nil || r.CONTENT_HASH != nil || r.ENCODING != nil || r.RAW_BYTES != nil || r.PARTS != nil || r.GetSHARD() != 1 {
		t.Fatalf("part 3 %v", r)
	}
	r, err = BuildMessageCompiled(MessagePart{Writer: 16, Seq: 1, Large: true, UnindexedTokens: 4, Text: &MessageText{Role: "user", Content: "x", TokenOffset: 9}})
	if err != nil || r.GetSHARD() != 0 || len(r.GetLARGE().GetValues()) != 1 || r.GetLARGE().GetValues()[0] != 1 || r.GetUNINDEXED_TOKENS() != 4 || r.GetTOKEN_OFFSET() != 9 {
		t.Fatalf("large part 0 %v %v", r, err)
	}
	for _, bad := range []MessagePart{
		{Writer: 0, Seq: 1},
		{Writer: 1, Seq: 0},
		{Writer: 1, Seq: 1, Part: -1},
		{Writer: 1, Seq: 1, Part: 1, ContentHash: hash},
		{Writer: 1, Seq: 1, Part: 1, Large: true},
		{Writer: 1, Seq: 1, Part: 1, Parts: 2},
	} {
		if _, err := BuildMessageCompiled(bad); err == nil {
			t.Errorf("BuildMessageCompiled(%+v) succeeded", bad)
		}
	}
	mt := BuildMessageTraceCompiled(1, 2, []byte{3})
	if mt.GetWRITER() != 1 || mt.GetSEQ() != 2 || !bytes.Equal(mt.GetTRACE_ID(), []byte{3}) {
		t.Fatalf("marker %v", mt)
	}
	if _, err := BuildStoreMetaCompiled(StoreMeta{}); err == nil {
		t.Error("unnamed store_meta row built")
	}
	m, err := BuildStoreMetaCompiled(StoreMeta{Name: StoreMetaContinuationHMAC, Bytes: []byte{9}})
	if err != nil || m.GetNAME() != "continuation_hmac" || m.INT_VALUE != nil || m.STR_VALUE != nil || !bytes.Equal(m.BYTES_VALUE, []byte{9}) {
		t.Fatalf("store_meta %v %v", m, err)
	}
}
