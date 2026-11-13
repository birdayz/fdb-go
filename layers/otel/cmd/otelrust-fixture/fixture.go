package main

import (
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"

	"fdb.dev/layers/otel"
	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/layers/otel/otelsql/otelv1pb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/embedded"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

type vector struct {
	Name string              `json:"name"`
	OTLP []byte              `json:"otlp"`
	Row  []byte              `json:"row"`
	PK   string              `json:"pk"`
	Keys map[string][]string `json:"keys"`
}

var spanIndexes = []string{"SPANS_TIME", "SPANS_SPAN_ID", "SPANS_STR_ATTR", "SPANS_LONG_STR_ATTR", "SPANS_INT_ATTR", "SPANS_DOUBLE_ATTR", "SPANS_BOOL_ATTR", "SPANS_REF_CHUNKS"}

func fixture() ([]vector, *descriptorpb.FileDescriptorSet, error) {
	tmpl, err := embedded.BuildSchemaTemplateFromDDL(otelsql.TemplateDDL())
	if err != nil {
		return nil, nil, err
	}
	md := tmpl.Underlying()
	rt := md.GetRecordType("SPANS")
	if rt == nil {
		return nil, nil, fmt.Errorf("SPANS absent")
	}
	var out []vector
	for name, rs := range fixtureInputs() {
		ss := rs.ScopeSpans[0]
		sp := ss.Spans[0]
		wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(rs)
		if err != nil {
			return nil, nil, err
		}
		row, err := otelsql.BuildCompiled(otelsql.Span{Resource: rs.Resource, ResourceSchemaURL: rs.SchemaUrl, Scope: ss.Scope, ScopeSchemaURL: ss.SchemaUrl, Span: sp, OTLP: wire})
		if err != nil {
			return nil, nil, err
		}
		data, err := proto.MarshalOptions{Deterministic: true}.Marshal(row)
		if err != nil {
			return nil, nil, err
		}
		rec := &recordlayer.FDBStoredRecord[proto.Message]{Record: row, RecordType: rt}
		pkEval, err := rt.PrimaryKey.Evaluate(rec, row)
		if err != nil || len(pkEval) != 1 {
			return nil, nil, fmt.Errorf("primary key: %v, %v", pkEval, err)
		}
		pk := asTuple(pkEval[0])
		v := vector{Name: name, OTLP: wire, Row: data, PK: hex.EncodeToString(pk.Pack()), Keys: map[string][]string{}}
		for _, name := range spanIndexes {
			idx := md.GetIndex(name)
			entries, err := idx.RootExpression.Evaluate(rec, row)
			if err != nil {
				return nil, nil, err
			}
			rest, err := idx.TrimPrimaryKey(pk)
			if err != nil {
				return nil, nil, err
			}
			v.Keys[name] = []string{}
			for _, entry := range entries {
				key := append(asTuple(entry), rest...)
				v.Keys[name] = append(v.Keys[name], hex.EncodeToString(key.Pack()))
			}
			sort.Strings(v.Keys[name])
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	files := &descriptorpb.FileDescriptorSet{}
	seen := map[string]bool{}
	var walk func(protoreflect.FileDescriptor)
	walk = func(fd protoreflect.FileDescriptor) {
		if seen[fd.Path()] {
			return
		}
		seen[fd.Path()] = true
		for i := 0; i < fd.Imports().Len(); i++ {
			walk(fd.Imports().Get(i).FileDescriptor)
		}
		files.File = append(files.File, protodesc.ToFileDescriptorProto(fd))
	}
	walk(tracepb.File_opentelemetry_proto_trace_v1_trace_proto)
	walk(otelv1pb.File_OTEL_V1)
	return out, files, nil
}

func fixtureInputs() map[string]*tracepb.ResourceSpans {
	cfg := otel.DefaultConfig()
	cfg.Count, cfg.Tenants, cfg.Attributes = 8, 1, 8
	out := map[string]*tracepb.ResourceSpans{}
	for i := 0; i < cfg.Count; i++ {
		rs, _, err := otel.FixtureOTLP(otel.Generate(cfg, i))
		if err != nil {
			panic(err)
		}
		out[fmt.Sprintf("v2_%d", i)] = rs
	}
	rs := proto.Clone(out["v2_0"]).(*tracepb.ResourceSpans)
	ss, sp := rs.ScopeSpans[0], rs.ScopeSpans[0].Spans[0]
	unknown := protowire.AppendTag(nil, 536870911, protowire.BytesType)
	unknown = protowire.AppendBytes(unknown, []byte("opaque\x00unknown"))
	for _, m := range []proto.Message{rs, ss, sp, rs.Resource, ss.Scope} {
		m.ProtoReflect().SetUnknown(unknown)
	}
	sp.TraceState, sp.Flags, sp.Kind = "vendor=opaque", 0x80000101, tracepb.Span_SpanKind(456)
	sp.EndTimeUnixNano = math.MaxUint64
	sp.Status = &tracepb.Status{Code: tracepb.Status_StatusCode(123), Message: "status"}
	sp.DroppedAttributesCount, sp.DroppedEventsCount, sp.DroppedLinksCount = 2, 3, 4
	sp.Attributes = []*commonpb.KeyValue{
		str("duplicate", "first"), str("duplicate", "last"), str("empty", ""), str("nul\x00.key", "a\x00b"),
		str("long", strings.Repeat("x", 1025)), str("too_long", strings.Repeat("x", 64001)),
		str(strings.Repeat("k", 1025), "unprojected"),
		{Key: "int", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: math.MinInt64}}},
		{Key: "double", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: math.Float64frombits(0x7ff8000000000042)}}},
		{Key: "negative_zero", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: math.Copysign(0, -1)}}},
		{Key: "bool", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: false}}},
		{Key: "bytes", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte{0, 1, 255}}}},
		{Key: "unset"},
		{Key: "array", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: []*commonpb.AnyValue{
			{Value: &commonpb.AnyValue_StringValue{StringValue: "<>&\u2028\u2029"}},
			{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: math.Inf(1)}},
			{Value: &commonpb.AnyValue_IntValue{IntValue: -1}},
		}}}}},
	}
	sp.Events = []*tracepb.Span_Event{{Name: "ev", TimeUnixNano: 9, DroppedAttributesCount: 1, Attributes: []*commonpb.KeyValue{str("event", "value")}}}
	sp.Links = []*tracepb.Span_Link{{TraceId: sp.TraceId, SpanId: sp.SpanId, Flags: 7, Attributes: []*commonpb.KeyValue{str("link", "value")}}}
	out["fidelity"] = rs
	return out
}

func asTuple(values []any) tuple.Tuple {
	out := make(tuple.Tuple, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}

func str(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}}
}
