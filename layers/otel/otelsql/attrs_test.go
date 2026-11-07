package otelsql

import (
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"fdb.dev/layers/otel/otelsql/otelv1pb"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// buildProjected is BuildCompiled with SpanProjection.
func buildProjected(s Span) (*otelv1pb.SPANS, Projection, error) {
	r, err := BuildCompiled(s)
	return r, SpanProjection(s), err
}

// projected renders a SPANS row's span-scope arrays as "array:key=value".
func projected(r *otelv1pb.SPANS, scope string) []string {
	var out []string
	short := func(s string) string {
		if len(s) > 12 {
			return fmt.Sprintf("%s..%d", s[:4], len(s))
		}
		return s
	}
	for _, a := range r.GetSTR_ATTRS().GetValues() {
		if a.GetSCOPE() == scope {
			out = append(out, "str:"+short(a.GetATTR_KEY())+"="+short(a.GetSTR_VALUE()))
		}
	}
	for _, a := range r.GetLONG_STR_ATTRS().GetValues() {
		if a.GetSCOPE() == scope {
			out = append(out, fmt.Sprintf("long:%s=%s/%x", short(a.GetATTR_KEY()), short(a.GetSTR_VALUE()), a.GetDIGEST()[:2]))
		}
	}
	for _, a := range r.GetINT_ATTRS().GetValues() {
		if a.GetSCOPE() == scope {
			out = append(out, fmt.Sprintf("int:%s=%d", a.GetATTR_KEY(), a.GetINT_VALUE()))
		}
	}
	for _, a := range r.GetDOUBLE_ATTRS().GetValues() {
		if a.GetSCOPE() == scope {
			out = append(out, fmt.Sprintf("double:%s=%v", a.GetATTR_KEY(), a.GetDOUBLE_VALUE()))
		}
	}
	for _, a := range r.GetBOOL_ATTRS().GetValues() {
		if a.GetSCOPE() == scope {
			out = append(out, fmt.Sprintf("bool:%s=%v", a.GetATTR_KEY(), a.GetBOOL_VALUE()))
		}
	}
	for _, a := range r.GetBYTES_ATTRS().GetValues() {
		if a.GetSCOPE() == scope {
			out = append(out, fmt.Sprintf("bytes:%s=%x/%v", a.GetATTR_KEY(), a.GetBYTES_VALUE(), a.BYTES_VALUE != nil))
		}
	}
	for _, a := range r.GetANY_ATTRS().GetValues() {
		if a.GetSCOPE() == scope {
			out = append(out, "any:"+a.GetATTR_KEY()+"="+a.GetJSON_VALUE())
		}
	}
	return out
}

// The canonical set: unique keys at first position with their last value,
// typed values, caps counted and not projected, message keys excluded on
// request; bytes, arrays, kvlists and unset values reported, not indexed.
func TestCanonicalAttributeProjection(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("l", MaxIndexedStringBytes+1)
	digest := fmt.Sprintf("%x", LongStringDigest(long)[:2])
	longest := fmt.Sprintf("%x", LongStringDigest(strings.Repeat("m", MaxAttributeValueBytes))[:2])
	base := []string{
		"str:s=v", "str:empty=", "str:kkkk..1024=longest key",
		"long:prompt=llll..1025/" + digest, "long:longest=mmmm..64000/" + longest,
		"int:i=-7", "int:dup=2", "double:d=2.5", "double:nan=NaN", "bool:b=false",
		"bytes:raw=0102/true", "bytes:raw_empty=/true",
		`any:arr=[1]`, `any:kv={"a":"b"}`, "any:none=null", "any:nil=null",
	}
	for _, exclude := range []bool{false, true} {
		r, proj, err := buildProjected(Span{Span: &tracepb.Span{Attributes: fixtureAttributes()}, ExcludeMessageAttributes: exclude})
		if err != nil {
			t.Fatal(err)
		}
		want := append([]string(nil), base...)
		unindexed := int64(2 + 2 + 4) // two caps; two bytes and four JSON values
		if exclude {
			unindexed++
		} else {
			want = append(want[:3:3], append([]string{"str:gen_..21=[]"}, want[3:]...)...)
		}
		if got := projected(r, ScopeSpan); !reflect.DeepEqual(got, want) {
			t.Errorf("exclude=%v: projected\n %q\nwant\n %q", exclude, got, want)
		}
		if proj != (Projection{Normalized: true, Unindexed: unindexed}) {
			t.Errorf("exclude=%v: projection %+v, want %d unindexed", exclude, proj, unindexed)
		}
	}

	// A clean list is projected as given, and is not normalized.
	clean := []*commonpb.KeyValue{kvStr("a", "x"), kvInt("b", 1), {Key: "c", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}}}
	r, proj, err := buildProjected(Span{Resource: &resourcepb.Resource{Attributes: clean}, Span: &tracepb.Span{Attributes: clean}})
	if err != nil {
		t.Fatal(err)
	}
	if proj != (Projection{}) || len(projected(r, ScopeSpan)) != 3 || len(projected(r, ScopeResource)) != 3 {
		t.Fatalf("clean list: %+v %v", proj, projected(r, ScopeSpan))
	}
	if set := canonicalSet(clean, true); &set.kvs[0] != &clean[0] {
		t.Error("a clean list was copied")
	}

	// A capped last value leaves its key out, whatever its earlier values;
	// the key keeps its first position otherwise.
	for _, tc := range []struct {
		kvs  []*commonpb.KeyValue
		want []string
	}{
		{[]*commonpb.KeyValue{kvStr("a", "ok"), kvStr("b", "x"), kvStr("a", strings.Repeat("z", MaxAttributeValueBytes+1))}, []string{"str:b=x"}},
		{[]*commonpb.KeyValue{kvStr("a", strings.Repeat("z", MaxAttributeValueBytes+1)), kvStr("b", "x"), kvStr("a", "ok")}, []string{"str:a=ok", "str:b=x"}},
		{[]*commonpb.KeyValue{kvStr("a", "1"), kvStr("b", "x"), kvInt("a", 1)}, []string{"str:b=x", "int:a=1"}},
	} {
		r, _, err := buildProjected(Span{Span: &tracepb.Span{Attributes: tc.kvs}})
		if err != nil {
			t.Fatal(err)
		}
		if got := projected(r, ScopeSpan); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("projected %q, want %q", got, tc.want)
		}
	}
	// Bytes and JSON beyond the value cap are left out too.
	big := []*commonpb.KeyValue{
		{Key: "raw", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: make([]byte, MaxAttributeValueBytes+1)}}},
		{Key: "arr", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: []*commonpb.AnyValue{{Value: &commonpb.AnyValue_StringValue{StringValue: strings.Repeat("q", MaxAttributeValueBytes)}}}}}}},
	}
	if r, proj, err := buildProjected(Span{Span: &tracepb.Span{Attributes: big}}); err != nil || len(projected(r, ScopeSpan)) != 0 || proj.Unindexed != 2 {
		t.Fatalf("over-cap bytes and JSON: %v %+v %v", projected(r, ScopeSpan), proj, err)
	}

	// Events drop message keys with the span; links never carry them.
	ev := &tracepb.Span_Event{Attributes: []*commonpb.KeyValue{kvStr("gen_ai.output.messages", "m"), kvStr("x", "y")}}
	r, _, err = buildProjected(Span{Span: &tracepb.Span{Events: []*tracepb.Span_Event{ev}}, ExcludeMessageAttributes: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.GetEVENTS().GetValues()[0].GetSTR_ATTRS().GetValues(); len(got) != 1 || got[0].GetATTR_KEY() != "x" {
		t.Fatalf("event attributes %v", got)
	}
	// service_name is the canonical resource set's service.name.
	r, _, _ = buildProjected(Span{Span: &tracepb.Span{}, Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kvStr("service.name", "a"), kvStr("service.name", "b")}}})
	if r.GetSERVICE_NAME() != "b" {
		t.Fatalf("service_name %q", r.GetSERVICE_NAME())
	}
}

// CanonicalJSON keeps array and kvlist order and repeated keys, and is the
// same for equal values however they were built.
func TestCanonicalJSON(t *testing.T) {
	t.Parallel()
	str := func(s string) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
	}
	dbl := func(f float64) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: f}}
	}
	kvl := func(kvs ...*commonpb.KeyValue) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: kvs}}}
	}
	arr := func(vs ...*commonpb.AnyValue) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: vs}}}
	}
	for _, tc := range []struct {
		v    *commonpb.AnyValue
		want string
	}{
		{kvl(&commonpb.KeyValue{Key: "z", Value: str("1")}, &commonpb.KeyValue{Key: "a", Value: str("2")}, &commonpb.KeyValue{Key: "z", Value: str("3")}), `{"z":"1","a":"2","z":"3"}`},
		{arr(dbl(math.NaN()), dbl(math.Inf(1)), dbl(math.Inf(-1)), dbl(0.1), dbl(1e21), dbl(math.Copysign(0, -1))), `["NaN","+Inf","-Inf",0.1,1e+21,-0]`},
		{arr(&commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte{0xff, 0}}}, &commonpb.AnyValue{}, nil, str("<&\"\x00\xff>")), `["/wA=",null,null,"<&\"\u0000` + "\ufffd" + `>"]`},
		{kvl(&commonpb.KeyValue{Key: "n", Value: kvl(&commonpb.KeyValue{Key: "b", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}}, &commonpb.KeyValue{Key: "i", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: -3}}})}, &commonpb.KeyValue{Key: "e"}), `{"n":{"b":true,"i":-3},"e":null}`},
	} {
		if got := string(CanonicalJSON(tc.v)); got != tc.want {
			t.Errorf("CanonicalJSON = %s, want %s", got, tc.want)
		}
	}
}
