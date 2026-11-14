package main

import (
	"bytes"
	"fmt"
	"math"
	"math/rand"
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// wireCase is one source wrapper and the ordered single-span outputs Go stores for it.
type wireCase struct {
	Name    string   `json:"name"`
	Input   []byte   `json:"input_otlp"`
	Outputs []vector `json:"outputs"`
}

type wireMalformed struct {
	Name  string `json:"name"`
	Input []byte `json:"input_otlp"`
}

type wireFuzzCase struct {
	Name        string   `json:"name"`
	Input       []byte   `json:"input_otlp"`
	Outputs     []vector `json:"outputs"`
	DecodeError *string  `json:"decode_error"`
}

type wireFixture struct {
	Cases              []wireCase      `json:"cases"`
	Malformed          []wireMalformed `json:"malformed"`
	FuzzCases          []wireFuzzCase  `json:"fuzz_cases"`
	FuzzCandidateCount int             `json:"fuzz_candidate_count"`
	FuzzFilteredCount  int             `json:"fuzz_filtered_count"`
}

type wireInput struct {
	name  string
	input []byte
	// source, when set, is the object the input was marshaled from and the expectation's origin.
	source *tracepb.ResourceSpans
}

type single struct {
	ss   *tracepb.ScopeSpans
	sp   *tracepb.Span
	wire []byte
}

// split mirrors prepareSpan's single-span wrapper in layers/otel/spans_rel.go (lines 153-160).
func split(rs *tracepb.ResourceSpans) ([]single, error) {
	var out []single
	for _, ss := range rs.ScopeSpans {
		for _, sp := range ss.Spans {
			one := &tracepb.ResourceSpans{Resource: rs.Resource, SchemaUrl: rs.SchemaUrl, ScopeSpans: []*tracepb.ScopeSpans{{Scope: ss.Scope, SchemaUrl: ss.SchemaUrl, Spans: []*tracepb.Span{sp}}}}
			one.ProtoReflect().SetUnknown(rs.ProtoReflect().GetUnknown())
			one.ScopeSpans[0].ProtoReflect().SetUnknown(ss.ProtoReflect().GetUnknown())
			wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(one)
			if err != nil {
				return nil, err
			}
			out = append(out, single{ss: ss, sp: sp, wire: wire})
		}
	}
	return out, nil
}

func wireFixtures(compute func(string, *tracepb.ResourceSpans, *tracepb.ScopeSpans, *tracepb.Span, []byte) (vector, error)) (wireFixture, error) {
	var wf wireFixture
	inputs, err := wireInputs()
	if err != nil {
		return wf, err
	}
	for _, in := range inputs {
		decoded := &tracepb.ResourceSpans{}
		if err := proto.Unmarshal(in.input, decoded); err != nil {
			return wf, fmt.Errorf("%s: %w", in.name, err)
		}
		rs := decoded
		if in.source != nil {
			rs = in.source
		}
		singles, err := split(rs)
		if err != nil {
			return wf, err
		}
		if in.source != nil {
			again, err := split(decoded)
			if err != nil {
				return wf, err
			}
			for i := range singles {
				if !bytes.Equal(singles[i].wire, again[i].wire) {
					return wf, fmt.Errorf("%s/%d: object and decoded splits differ", in.name, i)
				}
			}
		}
		c := wireCase{Name: in.name, Input: in.input}
		for i, s := range singles {
			v, err := compute(fmt.Sprintf("%s/%d", in.name, i), rs, s.ss, s.sp, s.wire)
			if err != nil {
				return wf, err
			}
			c.Outputs = append(c.Outputs, v)
		}
		wf.Cases = append(wf.Cases, c)
	}
	for _, m := range malformedInputs() {
		if proto.Unmarshal(m.Input, &tracepb.ResourceSpans{}) == nil {
			return wf, fmt.Errorf("go accepts malformed %s", m.Name)
		}
		wf.Malformed = append(wf.Malformed, m)
	}
	for _, in := range wireFuzzInputs() {
		wf.FuzzCandidateCount++
		c := wireFuzzCase{Name: in.name, Input: in.input, Outputs: []vector{}}
		rs := &tracepb.ResourceSpans{}
		if err := proto.Unmarshal(in.input, rs); err != nil {
			message := goDecodeError(err)
			c.DecodeError = &message
		} else {
			if !fuzzAdmissible(rs) {
				wf.FuzzFilteredCount++
				continue
			}
			singles, err := split(rs)
			if err != nil {
				return wf, fmt.Errorf("%s split: %w", in.name, err)
			}
			for i, s := range singles {
				v, err := compute(fmt.Sprintf("%s/%d", in.name, i), rs, s.ss, s.sp, s.wire)
				if err != nil {
					return wf, fmt.Errorf("%s row: %w", in.name, err)
				}
				c.Outputs = append(c.Outputs, v)
			}
		}
		wf.FuzzCases = append(wf.FuzzCases, c)
	}
	return wf, nil
}

// protobuf-go randomizes only its prefix's space between U+0020 and U+00A0.
func goDecodeError(err error) string {
	message := err.Error()
	if suffix, ok := strings.CutPrefix(message, "proto:\u00a0"); ok {
		return "proto: " + suffix
	}
	return message
}

const (
	wireFuzzMutationCount = 64
	wireFuzzShapeCount    = 12
)

// The fixed controls make success, decode rejection and every filter non-vacuous.
func wireFuzzInputs() []wireInput {
	valid := field(2, field(2, rawSpan(0x70)))
	out := []wireInput{
		{name: "control_valid", input: valid},
		{name: "control_decode_error", input: cat(valid, []byte{0, 1})},
		{name: "filter_identity", input: field(2, field(2, rawSpan(0x71, field(2, make([]byte, 8)))))},
		{name: "filter_start", input: field(2, field(2, rawSpan(0x72, fixed64(7, 1<<63))))},
		{name: "filter_duplicate", input: field(2, field(2, rawSpan(0x73)), field(2, rawSpan(0x73)))},
		{name: "filter_empty", input: []byte{}},
	}
	rng := rand.New(rand.NewSource(283))
	bases := rawCases()[:7] // The seven original, small raw-wire cases.
	for i := range wireFuzzMutationCount {
		base := bases[i%len(bases)]
		wire := append([]byte(nil), base.input...)
		pos := rng.Intn(len(wire))
		switch i % 4 {
		case 0:
			wire[pos] ^= byte(1 << rng.Intn(8))
		case 1:
			wire = append(wire[:pos], wire[pos+1:]...)
		case 2:
			wire = append(wire, 0)
			copy(wire[pos+1:], wire[pos:])
			wire[pos] = byte(rng.Intn(256))
		case 3:
			wire = wire[:pos]
		}
		out = append(out, wireInput{name: fmt.Sprintf("mutation_%02d_%s", i, base.name), input: wire})
	}
	for i := range wireFuzzShapeCount {
		var attrs []byte
		for j := 0; j <= i%4; j++ {
			attrs = append(attrs, field(9, kv(fmt.Sprintf("attr-%d", j), fuzzValue(rng, (i+j)%8)))...)
		}
		resource := field(1, field(1, kv("service.name", sval(fmt.Sprintf("svc-%d", rng.Intn(4))))), field(3, text(2, "service"), text(3, "service.name")))
		scope := field(1, text(1, fmt.Sprintf("scope-%d", rng.Intn(4))), text(2, "1"))
		span := rawSpan(byte(0x90+i), text(5, fmt.Sprintf("span-%d", rng.Intn(100))), uvarint(6, uint64(rng.Intn(6))),
			fixed32(16, rng.Uint32()), field(15, text(2, "status"), uvarint(3, uint64(rng.Intn(3)))), attrs)
		wire := cat(field(2, scope, field(2, span), text(3, "scope-schema")), resource, text(3, "resource-schema"))
		out = append(out, wireInput{name: fmt.Sprintf("shape_%02d", i), input: wire})
	}
	return out
}

func fuzzValue(rng *rand.Rand, kind int) []byte {
	switch kind {
	case 0:
		return sval(fmt.Sprintf("value-%x", rng.Uint32()))
	case 1:
		return uvarint(2, uint64(rng.Intn(3)))
	case 2:
		return uvarint(3, rng.Uint64())
	case 3:
		return fixed64(4, rng.Uint64())
	case 4:
		return field(5, field(1, sval("nested")), field(1, uvarint(3, rng.Uint64())))
	case 5:
		return field(6, field(1, kv("nested", uvarint(2, uint64(rng.Intn(3))))))
	case 6:
		return field(7, []byte{byte(rng.Uint32()), 0, 0xff})
	default:
		return uvarint(8, uint64(rng.Uint32()))
	}
}

// This corpus targets decode and row fidelity, not the full admission rejection set.
// Valid decodes with no span, invalid IDs/start, or duplicate identities are omitted.
func fuzzAdmissible(rs *tracepb.ResourceSpans) bool {
	var zeroTrace [16]byte
	var zeroSpan [8]byte
	seen := map[string]bool{}
	for _, ss := range rs.ScopeSpans {
		for _, sp := range ss.GetSpans() {
			if sp == nil {
				continue
			}
			if len(sp.TraceId) != 16 || bytes.Equal(sp.TraceId, zeroTrace[:]) || len(sp.SpanId) != 8 || bytes.Equal(sp.SpanId, zeroSpan[:]) || sp.StartTimeUnixNano > math.MaxInt64 {
				return false
			}
			key := string(sp.TraceId) + string(sp.SpanId)
			if seen[key] {
				return false
			}
			seen[key] = true
		}
	}
	return len(seen) > 0
}

// overlong appends v as a varint one byte longer than minimal.
func overlong(b []byte, v uint64) []byte {
	b = protowire.AppendVarint(b, v)
	b[len(b)-1] |= 0x80
	return append(b, 0)
}

// suffix is unknown varints 101, 100, 101 with non-minimal values id, id+1, id+2.
func suffix(id uint64) []byte {
	b := protowire.AppendTag(nil, 101, protowire.VarintType)
	b = overlong(b, id)
	b = protowire.AppendTag(b, 100, protowire.VarintType)
	b = overlong(b, id+1)
	b = protowire.AppendTag(b, 101, protowire.VarintType)
	return overlong(b, id+2)
}

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func field(num protowire.Number, parts ...[]byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, num, protowire.BytesType), cat(parts...))
}

func text(num protowire.Number, s string) []byte { return field(num, []byte(s)) }

func uvarint(num protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, num, protowire.VarintType), v)
}

func fixed64(num protowire.Number, v uint64) []byte {
	return protowire.AppendFixed64(protowire.AppendTag(nil, num, protowire.Fixed64Type), v)
}

func fixed32(num protowire.Number, v uint32) []byte {
	return protowire.AppendFixed32(protowire.AppendTag(nil, num, protowire.Fixed32Type), v)
}

var traceID = bytes.Repeat([]byte{0x31}, 16)

// rawSpan is a Span payload with a valid identity and start, then parts.
func rawSpan(id byte, parts ...[]byte) []byte {
	return cat(field(1, traceID), field(2, bytes.Repeat([]byte{id}, 8)), fixed64(7, uint64(id)), cat(parts...))
}

// kv is a KeyValue payload whose value is the AnyValue payload value.
func kv(key string, value []byte, extra ...[]byte) []byte {
	return cat(text(1, key), field(2, value), cat(extra...))
}

func sval(s string, extra ...[]byte) []byte { return cat(text(1, s), cat(extra...)) }

// rawCases decode with Go's own merge rules; their expectations come from that decode.
func rawCases() []wireInput {
	id := uint64(1000)
	u := func() []byte { id += 3; return suffix(id) }
	resourceA := field(1, field(1, kv("a", sval("1"))), uvarint(2, 7), u())
	resourceB := field(1, field(1, kv("b", sval("2"))), uvarint(2, 0), u())
	repeated := cat(resourceA, field(2, field(2, rawSpan(1))), resourceB)

	status := field(2, field(2, rawSpan(2,
		field(15, uvarint(3, 2), text(2, "x"), u()), text(5, "mid"), field(15, text(2, "y"), u()))))

	value := field(2, field(2, rawSpan(3,
		field(9, text(1, "k"), field(2, sval("a"), u()), u(), field(2, uvarint(3, 5), u())))))

	sameArm := field(2, field(2, rawSpan(4, field(9, kv("arr", cat(
		field(5, field(1, sval("s1", u())), u()), u(),
		field(5, field(1, sval("s2", u())), u()), u()))))))

	switches := field(2, field(2, rawSpan(5,
		field(9, kv("kvlist", cat(
			field(5, field(1, sval("x", u())), u()), u(),
			field(6, field(1, kv("q", sval("r"), u())), u()),
			field(5, field(1, sval("y", u())), u())))),
		field(9, kv("bool", cat(
			field(5, field(1, sval("x")), u()), uvarint(2, 1),
			field(5, field(1, sval("y")), u())))))))

	empty := cat(field(1), field(2, u()),
		field(2, field(1, u()), field(2, rawSpan(6,
			field(15), field(11), field(11, text(2, "e"), u()), field(13, u()),
			field(9, text(1, "nil"), field(2)), field(9, text(1, "none"))))),
		field(2, field(2, rawSpan(7, u()))))

	longLength := protowire.AppendTag(nil, 1, protowire.BytesType)
	resource := field(1, kv("svc", sval("x")))
	longLength = append(overlong(longLength, uint64(len(resource))), resource...)
	shuffled := cat(fixed64(7, 8), text(5, "first"), uvarint(6, 2), field(2, bytes.Repeat([]byte{8}, 8)), field(1, traceID),
		text(5, "last"), uvarint(6, 0), overlong(protowire.AppendTag(nil, 10, protowire.VarintType), 3),
		field(15, uvarint(3, 99)), field(9, kv("zero", uvarint(3, 0))), field(9, kv("false", uvarint(2, 0))), u())
	canonical := cat(longLength, field(2, field(2, shuffled)))

	return []wireInput{
		{name: "raw_array_same_arm_merges", input: sameArm},
		{name: "raw_empty_and_unknown_only_children", input: empty},
		{name: "raw_keyvalue_value_merges", input: value},
		{name: "raw_known_fields_canonicalize", input: canonical},
		{name: "raw_oneof_switch_drops_other_arms", input: switches},
		{name: "raw_repeated_resource_merges", input: repeated},
		{name: "raw_status_merges", input: status},
		{name: "raw_nonminimal_unknown_tags", input: nonminimalTagsCase()},
		{name: "raw_negative_enums_and_oneof_zeroes", input: negativeAndZeroCase()},
		{name: "raw_v111_known_fields", input: v111Case()},
		{name: "raw_message_depth_10000", input: messageDepthFrame(10000)},
		{name: "raw_unknown_group_depth_10001", input: groupDepthFrame(10001)},
		{name: "raw_large_field_number_inside_group", input: cat(field(2, field(2, rawSpan(24))), nestedGroups(1, uvarint(1<<29, 7)))},
	}
}

// wrongWireFields puts each declared field in a wire type its coder does not accept.
func wrongWireFields(md protoreflect.MessageDescriptor) []byte {
	var out []byte
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		switch fd.Kind() {
		case protoreflect.StringKind, protoreflect.BytesKind, protoreflect.MessageKind:
			out = append(out, overlong(protowire.AppendTag(nil, fd.Number(), protowire.VarintType), 7)...)
		default:
			out = append(out, field(fd.Number(), []byte{0xff})...)
		}
	}
	return out
}

func wrongWireCase() *tracepb.ResourceSpans {
	rs := objectCase()
	for _, ss := range rs.ScopeSpans {
		for _, sp := range ss.Spans {
			sp.SpanId = bytes.Repeat([]byte{sp.SpanId[0] + 0x10}, 8)
		}
	}
	pending := []protoreflect.Message{rs.ProtoReflect()}
	for len(pending) > 0 {
		m := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		m.SetUnknown(wrongWireFields(m.Descriptor()))
		m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			if fd.Message() != nil && fd.IsList() {
				for i := 0; i < v.List().Len(); i++ {
					pending = append(pending, v.List().Get(i).Message())
				}
			} else if fd.Message() != nil {
				pending = append(pending, v.Message())
			}
			return true
		})
	}
	return rs
}

func nonminimalUnknown(id uint64) []byte {
	return cat(overlong(nil, 100<<3), overlong(nil, id),
		overlong(nil, 101<<3|2), overlong(nil, 3), []byte{0xff, 0xfe, 0})
}

func nonminimalTagsCase() []byte {
	resource := field(1, uvarint(2, 2), nonminimalUnknown(1))
	scope := field(1, text(1, "scope"), nonminimalUnknown(2))
	span := field(2, rawSpan(20, nonminimalUnknown(3)))
	return cat(resource, field(2, scope, span, nonminimalUnknown(4)), nonminimalUnknown(5))
}

func negativeAndZeroCase() []byte {
	return field(2, field(2, rawSpan(21,
		uvarint(6, 1<<32-7), field(15, uvarint(3, 1<<32-2)), fixed32(16, 0x80000101),
		field(9, kv("negative_zero", fixed64(4, 1<<63))),
		field(9, kv("positive_zero", fixed64(4, 0))),
		field(9, kv("int_zero", uvarint(3, 0))),
		field(9, kv("bool_false", uvarint(2, 0))),
		field(9, kv("empty_string", text(1, ""))),
		field(9, kv("empty_bytes", field(7))))))
}

func v111Case() []byte {
	entity := cat(text(1, "entity-schema"), text(2, "service"), text(3, "service.name"), text(3, ""), text(4, "description"), text(4, "owner"))
	resource := field(1, field(3, entity), field(3, text(2, "host")))
	attribute := cat(kv("interned", uvarint(8, 1<<32-3)), uvarint(3, 1<<32-7))
	return cat(resource, field(2, field(2, rawSpan(22, field(9, attribute)))))
}

// deepAnyValue emits a single path alternating arrays and kvlists. The first
// AnyValue is level 5: ResourceSpans, ScopeSpans, Span, KeyValue precede it.
func deepAnyValue(depth int) []byte {
	const (
		anyNode = iota
		arrayNode
		listNode
		pairNode
	)
	kind, array := anyNode, true
	fields := make([]protowire.Number, 0, depth-5)
	for level := 5; level < depth; level++ {
		switch kind {
		case anyNode:
			if array {
				fields = append(fields, 5)
				kind = arrayNode
			} else {
				fields = append(fields, 6)
				kind = listNode
			}
			array = !array
		case arrayNode:
			fields = append(fields, 1)
			kind = anyNode
		case listNode:
			fields = append(fields, 1)
			kind = pairNode
		case pairNode:
			fields = append(fields, 2)
			kind = anyNode
		}
	}
	var leaf []byte
	if kind == anyNode || kind == pairNode {
		leaf = text(1, "leaf")
	}
	lengths := make([]int, len(fields))
	size := len(leaf)
	for i := len(fields) - 1; i >= 0; i-- {
		lengths[i] = size
		size = protowire.SizeTag(fields[i]) + protowire.SizeBytes(size)
	}
	out := make([]byte, 0, size)
	for i, num := range fields {
		out = protowire.AppendTag(out, num, protowire.BytesType)
		out = protowire.AppendVarint(out, uint64(lengths[i]))
	}
	return append(out, leaf...)
}

func messageDepthFrame(depth int) []byte {
	return field(2, field(2, rawSpan(23, field(9, kv("depth", deepAnyValue(depth))))))
}

func nestedGroups(depth int, body []byte) []byte {
	var out []byte
	for range depth {
		out = protowire.AppendTag(out, 100, protowire.StartGroupType)
	}
	out = append(out, body...)
	for range depth {
		out = protowire.AppendTag(out, 100, protowire.EndGroupType)
	}
	return out
}

func groupDepthFrame(depth int) []byte {
	return cat(field(2, field(2, rawSpan(25))), nestedGroups(depth, nil))
}

// objectCase sets a distinct suffix on every message node, plus whole fixed, bytes and group records.
func objectCase() *tracepb.ResourceSpans {
	val := func(v any) *commonpb.AnyValue {
		switch x := v.(type) {
		case string:
			return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: x}}
		case int64:
			return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: x}}
		case bool:
			return &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: x}}
		case float64:
			return &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: x}}
		case []*commonpb.AnyValue:
			return &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: x}}}
		case []*commonpb.KeyValue:
			return &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: x}}}
		}
		panic(v)
	}
	pair := func(k string, v any) *commonpb.KeyValue { return &commonpb.KeyValue{Key: k, Value: val(v)} }
	rs := &tracepb.ResourceSpans{
		Resource: &resourcepb.Resource{
			Attributes: []*commonpb.KeyValue{
				pair("service.name", "svc"),
				pair("array", []*commonpb.AnyValue{val("a"), val(int64(-2)), val([]*commonpb.KeyValue{pair("inner", true)})}),
				pair("kvlist", []*commonpb.KeyValue{pair("b", false), pair("d", 1.5)}),
			},
			DroppedAttributesCount: 1,
			EntityRefs:             []*commonpb.EntityRef{{Type: "service", IdKeys: []string{"service.name"}}},
		},
		SchemaUrl: "rs-url",
	}
	for s := 0; s < 2; s++ {
		ss := &tracepb.ScopeSpans{Scope: &commonpb.InstrumentationScope{Name: fmt.Sprintf("scope-%d", s), Version: "1", Attributes: []*commonpb.KeyValue{pair("scope.attr", int64(s))}}, SchemaUrl: "ss-url"}
		for p := 0; p < 2; p++ {
			id := byte(0x40 + 2*s + p)
			sid := bytes.Repeat([]byte{id}, 8)
			ss.Spans = append(ss.Spans, &tracepb.Span{
				TraceId: traceID, SpanId: sid, StartTimeUnixNano: uint64(id), Name: fmt.Sprintf("span-%d-%d", s, p),
				Attributes: []*commonpb.KeyValue{pair("http.method", "GET"), pair("list", []*commonpb.AnyValue{val(int64(p)), val("v")})},
				Events:     []*tracepb.Span_Event{{Name: "ev", TimeUnixNano: 3, Attributes: []*commonpb.KeyValue{pair("ev.attr", "x")}}},
				Links:      []*tracepb.Span_Link{{TraceId: traceID, SpanId: sid, Attributes: []*commonpb.KeyValue{pair("link.attr", "y")}}},
				Status:     &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "bad"},
			})
		}
		rs.ScopeSpans = append(rs.ScopeSpans, ss)
	}
	id := uint64(0)
	var mark func(protoreflect.Message)
	mark = func(m protoreflect.Message) {
		id += 3
		m.SetUnknown(suffix(id))
		fields := m.Descriptor().Fields()
		for i := 0; i < fields.Len(); i++ {
			fd := fields.Get(i)
			if fd.Message() == nil || !m.Has(fd) {
				continue
			}
			if fd.IsList() {
				list := m.Get(fd).List()
				for j := 0; j < list.Len(); j++ {
					mark(list.Get(j).Message())
				}
			} else {
				mark(m.Get(fd).Message())
			}
		}
	}
	mark(rs.ProtoReflect())
	appendUnknown := func(m proto.Message, raw []byte) {
		r := m.ProtoReflect()
		r.SetUnknown(append(append([]byte(nil), r.GetUnknown()...), raw...))
	}
	opaque := protowire.AppendTag(nil, 105, protowire.BytesType)
	opaque = append(overlong(opaque, 3), 0xff, 0xfe, 0x00)
	group := protowire.AppendTag(nil, 102, protowire.StartGroupType)
	group = overlong(protowire.AppendTag(group, 101, protowire.VarintType), 5)
	group = overlong(protowire.AppendTag(group, 100, protowire.VarintType), 6)
	group = protowire.AppendTag(group, 102, protowire.EndGroupType)
	appendUnknown(rs, opaque)
	appendUnknown(rs.ScopeSpans[0].Spans[0], cat(fixed64(103, 0x0102030405060708), fixed32(104, 0x0a0b0c0d)))
	appendUnknown(rs.ScopeSpans[0].Spans[1].Status, opaque)
	appendUnknown(rs.ScopeSpans[1].Spans[0].Links[0], group)
	return rs
}

func wireInputs() ([]wireInput, error) {
	var out []wireInput
	for _, c := range []struct {
		name   string
		source *tracepb.ResourceSpans
	}{
		{"object_unknowns_at_every_node", objectCase()},
		{"object_wrong_wire_at_every_node", wrongWireCase()},
	} {
		wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(c.source)
		if err != nil {
			return nil, err
		}
		out = append(out, wireInput{name: c.name, input: wire, source: c.source})
	}
	return append(out, rawCases()...), nil
}

// malformedInputs must be rejected by Go's default proto.Unmarshal.
func malformedInputs() []wireMalformed {
	valid := field(2, field(2, rawSpan(9)))
	tag := func(num protowire.Number, typ protowire.Type) []byte { return protowire.AppendTag(nil, num, typ) }
	return []wireMalformed{
		{Name: "bad_utf8_known_string", Input: cat(valid, field(2, field(2, rawSpan(10, field(5, []byte{0xff})))))},
		{Name: "bad_utf8_nested_key", Input: cat(valid, field(1, field(1, kv("\xc3\x28", sval("v")))))},
		{Name: "field_number_zero", Input: cat(valid, []byte{0x00, 0x01})},
		{Name: "invalid_wire_type", Input: cat(valid, []byte{0xa7, 0x06, 0x00})},
		{Name: "mismatched_end_group", Input: cat(valid, tag(102, protowire.StartGroupType), tag(103, protowire.EndGroupType))},
		{Name: "nested_truncation", Input: cat(valid, []byte{0x12, 0x04, 0x12, 0x10, 0x0a, 0x01})},
		{Name: "stray_end_group", Input: cat(valid, tag(102, protowire.EndGroupType))},
		{Name: "truncated_length", Input: cat(valid, []byte{0x0a, 0x05, 0x0a, 0x03})},
		{Name: "truncated_unknown_varint", Input: cat(valid, []byte{0xa8, 0x06, 0x80})},
		{Name: "unterminated_group", Input: cat(valid, tag(102, protowire.StartGroupType))},
		{Name: "message_depth_10001", Input: messageDepthFrame(10001)},
		{Name: "unknown_group_depth_10002", Input: groupDepthFrame(10002)},
		{Name: "large_field_number_at_message_level", Input: cat(valid, uvarint(1<<29, 7))},
	}
}
