package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"os"
	"strings"
	"testing"

	"fdb.dev/layers/otel/otelsql/otelv1pb"
	"github.com/bazelbuild/rules_go/go/runfiles"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestPinnedRustCompatibilityFixtures(t *testing.T) {
	t.Parallel()
	vectors, wf, descriptors, err := fixture()
	if err != nil {
		t.Fatal(err)
	}
	if len(vectors) != 9 {
		t.Fatalf("expected nine compatibility vectors, got %d", len(vectors))
	}
	for _, v := range vectors {
		if len(v.Keys) != 8 || len(v.OTLP) == 0 || len(v.Row) == 0 || v.PK == "" {
			t.Fatalf("incomplete vector %s", v.Name)
		}
		if v.Name != "fidelity" {
			keys := 0
			for _, index := range v.Keys {
				keys += len(index)
			}
			if keys != 10 {
				t.Fatalf("%s has %d indexes, want 8 attribute + 2 intrinsic", v.Name, keys)
			}
		}
	}
	jsonBytes, err := json.MarshalIndent(vectors, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	wireBytes, err := json.MarshalIndent(wf, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	descriptorBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(descriptors)
	if err != nil {
		t.Fatal(err)
	}
	r, err := runfiles.New()
	if err != nil {
		t.Fatal(err)
	}
	for name, expected := range map[string][]byte{"vectors.json": append(jsonBytes, '\n'), "wire_vectors.json": append(wireBytes, '\n'), "descriptors.pb": descriptorBytes} {
		path, err := r.Rlocation("_main/layers/otel/rust/testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, expected) {
			t.Fatalf("%s drift: regenerate with otelrust-fixture into a fresh directory and inspect", name)
		}
	}
}

// Each wire case holds what its name claims under Go's decoder, so the Rust test is not vacuous.
func TestWireFixtureCases(t *testing.T) {
	t.Parallel()
	_, wf, _, err := fixture()
	if err != nil {
		t.Fatal(err)
	}
	outputs := map[string]int{
		"object_unknowns_at_every_node": 4, "raw_array_same_arm_merges": 1, "raw_empty_and_unknown_only_children": 2,
		"raw_keyvalue_value_merges": 1, "raw_known_fields_canonicalize": 1, "raw_oneof_switch_drops_other_arms": 1,
		"raw_repeated_resource_merges": 1, "raw_status_merges": 1,
		"object_wrong_wire_at_every_node": 4, "raw_nonminimal_unknown_tags": 1,
		"raw_negative_enums_and_oneof_zeroes": 1, "raw_v111_known_fields": 1,
		"raw_message_depth_10000": 1, "raw_unknown_group_depth_10001": 1,
		"raw_large_field_number_inside_group": 1,
	}
	inputs := map[string]*tracepb.ResourceSpans{}
	for _, c := range wf.Cases {
		if want, ok := outputs[c.Name]; !ok || len(c.Outputs) != want {
			t.Fatalf("%s has %d outputs", c.Name, len(c.Outputs))
		}
		delete(outputs, c.Name)
		rs := &tracepb.ResourceSpans{}
		if err := proto.Unmarshal(c.Input, rs); err != nil {
			t.Fatal(err)
		}
		inputs[c.Name] = rs
	}
	if len(outputs) != 0 || len(wf.Cases) != 15 || len(wf.Malformed) != 13 {
		t.Fatalf("missing cases %v or case/malformed counts %d/%d", outputs, len(wf.Cases), len(wf.Malformed))
	}
	for _, c := range wf.Cases[:1] {
		if c.Name != "object_unknowns_at_every_node" {
			t.Fatalf("first case is %s", c.Name)
		}
		for _, out := range c.Outputs {
			rs := &tracepb.ResourceSpans{}
			if err := proto.Unmarshal(out.OTLP, rs); err != nil {
				t.Fatal(err)
			}
			seen := map[string]bool{}
			var walk func(protoreflect.Message)
			walk = func(m protoreflect.Message) {
				u := string(m.GetUnknown())
				if u == "" || seen[u] {
					t.Fatalf("%s: %s lacks its own unknown bytes", out.Name, m.Descriptor().FullName())
				}
				seen[u] = true
				m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
					if fd.Message() != nil && fd.IsList() {
						for i := 0; i < v.List().Len(); i++ {
							walk(v.List().Get(i).Message())
						}
					} else if fd.Message() != nil {
						walk(v.Message())
					}
					return true
				})
			}
			walk(rs.ProtoReflect())
			if len(seen) != 40 {
				t.Fatalf("%s: %d message nodes, want 40", out.Name, len(seen))
			}
		}
	}
	unknown := func(m proto.Message) int { return len(m.ProtoReflect().GetUnknown()) / len(suffix(1003)) }
	span := func(name string, scope int) *tracepb.Span { return inputs[name].ScopeSpans[scope].Spans[0] }
	if r := inputs["raw_repeated_resource_merges"].Resource; len(r.Attributes) != 2 || r.DroppedAttributesCount != 0 || unknown(r) != 2 {
		t.Fatalf("repeated resource did not merge: %v", r)
	}
	if s := span("raw_status_merges", 0).Status; s.Code != 2 || s.Message != "y" || unknown(s) != 2 {
		t.Fatalf("status did not merge: %v", s)
	}
	if kv := span("raw_keyvalue_value_merges", 0).Attributes[0]; kv.Value.GetIntValue() != 5 || unknown(kv.Value) != 2 || unknown(kv) != 1 {
		t.Fatalf("value did not merge: %v", kv)
	}
	if v := span("raw_array_same_arm_merges", 0).Attributes[0].Value; len(v.GetArrayValue().GetValues()) != 2 || unknown(v.GetArrayValue()) != 2 || unknown(v) != 2 {
		t.Fatalf("array arm did not merge: %v", v)
	}
	for _, kv := range span("raw_oneof_switch_drops_other_arms", 0).Attributes {
		values := kv.Value.GetArrayValue().GetValues()
		if len(values) != 1 || values[0].GetStringValue() != "y" || unknown(kv.Value.GetArrayValue()) != 1 {
			t.Fatalf("oneof switch kept a replaced arm: %v", kv)
		}
	}
	empty := inputs["raw_empty_and_unknown_only_children"]
	sp := span("raw_empty_and_unknown_only_children", 1)
	if len(empty.ScopeSpans) != 3 || len(empty.ScopeSpans[0].Spans) != 0 || empty.Resource == nil || proto.Size(empty.Resource) != 0 || sp.Status == nil ||
		proto.Size(sp.Status) != 0 || len(sp.Events) != 2 || unknown(sp.Links[0]) != 1 || sp.Attributes[0].Value == nil {
		t.Fatalf("empty or unknown-only children lost presence: %v", empty)
	}
	if sp := span("raw_known_fields_canonicalize", 0); sp.Name != "last" || sp.Kind != 0 || sp.DroppedAttributesCount != 3 || sp.Status.Code != 99 {
		t.Fatalf("known fields did not canonicalize: %v", sp)
	}
	checkCodecCases(t, wf, inputs)
}

func checkCodecCases(t *testing.T, wf wireFixture, inputs map[string]*tracepb.ResourceSpans) {
	t.Helper()
	if !proto.Equal(inputs["object_wrong_wire_at_every_node"], wrongWireCase()) {
		t.Fatal("wrong-wire fields changed a known value, presence or oneof arm")
	}
	types := map[protoreflect.Name]bool{}
	for _, c := range wf.Cases {
		if c.Name != "object_wrong_wire_at_every_node" {
			continue
		}
		for _, out := range c.Outputs {
			rs := &tracepb.ResourceSpans{}
			if err := proto.Unmarshal(out.OTLP, rs); err != nil {
				t.Fatal(err)
			}
			nodes := 0
			pending := []protoreflect.Message{rs.ProtoReflect()}
			for len(pending) > 0 {
				m := pending[len(pending)-1]
				pending = pending[:len(pending)-1]
				nodes++
				types[m.Descriptor().Name()] = true
				if !bytes.Equal(m.GetUnknown(), wrongWireFields(m.Descriptor())) {
					t.Fatalf("%s: %s lost wrong-wire fields", out.Name, m.Descriptor().FullName())
				}
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
			if nodes != 40 {
				t.Fatalf("%s: wrong-wire nodes %d, want 40", out.Name, nodes)
			}
		}
	}
	for _, name := range []protoreflect.Name{"ResourceSpans", "ScopeSpans", "Span", "Event", "Link", "Status", "Resource", "EntityRef", "InstrumentationScope", "KeyValue", "AnyValue", "ArrayValue", "KeyValueList"} {
		if !types[name] {
			t.Errorf("wrong-wire cases do not visit %s", name)
		}
	}
	if len(types) != 13 {
		t.Fatalf("wrong-wire message types %d, want 13", len(types))
	}

	rs := inputs["raw_nonminimal_unknown_tags"]
	ss := rs.ScopeSpans[0]
	for i, m := range []proto.Message{rs.Resource, ss.Scope, ss.Spans[0], ss, rs} {
		want := cat(overlong(protowire.AppendTag(nil, 100, protowire.VarintType), uint64(i+1)),
			protowire.AppendTag(nil, 101, protowire.BytesType), overlong(nil, 3), []byte{0xff, 0xfe, 0})
		if !bytes.Equal(m.ProtoReflect().GetUnknown(), want) {
			t.Fatalf("node %d: nonminimal tag was not canonicalized with raw payload retained", i)
		}
	}

	sp := inputs["raw_negative_enums_and_oneof_zeroes"].ScopeSpans[0].Spans[0]
	if sp.Kind != -7 || sp.Status.Code != -2 || sp.Flags != 0x80000101 {
		t.Fatal("negative enum conversion or fixed32 flags changed")
	}
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(sp)
	if err != nil || !bytes.Contains(wire, uvarint(6, ^uint64(6))) {
		t.Fatalf("negative kind does not marshal as a ten-byte sign-extended value: %v", err)
	}
	wire, err = proto.MarshalOptions{Deterministic: true}.Marshal(sp.Status)
	if err != nil || !bytes.Equal(wire, uvarint(3, ^uint64(1))) {
		t.Fatalf("negative status does not marshal as a ten-byte sign-extended value: %v", err)
	}
	zeroWires := [][]byte{fixed64(4, 1<<63), fixed64(4, 0), uvarint(3, 0), uvarint(2, 0), text(1, ""), field(7)}
	if len(sp.Attributes) != len(zeroWires) || math.Float64bits(sp.Attributes[0].Value.GetDoubleValue()) != 1<<63 {
		t.Fatal("negative zero bits or zero-valued oneof population changed")
	}
	for i, kv := range sp.Attributes {
		wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(kv.Value)
		if err != nil || !bytes.Equal(wire, zeroWires[i]) {
			t.Fatalf("oneof %s lost its zero-valued presence or bits: %x, %v", kv.Key, wire, err)
		}
	}

	rs = inputs["raw_v111_known_fields"]
	if len(rs.Resource.EntityRefs) != 2 {
		t.Fatal("entity_refs did not decode as repeated known messages")
	}
	entity := rs.Resource.EntityRefs[0]
	if entity.SchemaUrl != "entity-schema" || entity.Type != "service" || len(entity.IdKeys) != 2 || entity.IdKeys[0] != "service.name" || entity.IdKeys[1] != "" ||
		len(entity.DescriptionKeys) != 2 || entity.DescriptionKeys[0] != "description" || entity.DescriptionKeys[1] != "owner" || rs.Resource.EntityRefs[1].Type != "host" {
		t.Fatal("entity_refs known scalar/repeated values changed")
	}
	kv := rs.ScopeSpans[0].Spans[0].Attributes[0]
	if kv.Key != "interned" || kv.KeyStrindex != -7 || kv.Value.GetStringValueStrindex() != -3 {
		t.Fatal("v1.11 int32 or oneof fields were not decoded")
	}
	for _, m := range []proto.Message{rs.Resource, entity, rs.Resource.EntityRefs[1], kv, kv.Value} {
		if len(m.ProtoReflect().GetUnknown()) != 0 {
			t.Fatalf("%s: v1.11 fields were treated as unknown", m.ProtoReflect().Descriptor().FullName())
		}
	}
	wire, err = proto.MarshalOptions{Deterministic: true}.Marshal(kv.Value)
	if err != nil || !bytes.Equal(wire, uvarint(8, ^uint64(2))) {
		t.Fatalf("negative string_value_strindex wire: %x, %v", wire, err)
	}
	for _, c := range wf.Cases {
		if c.Name == "raw_v111_known_fields" {
			row := &otelv1pb.SPANS{}
			if err := proto.Unmarshal(c.Outputs[0].Row, row); err != nil {
				t.Fatal(err)
			}
			attrs := row.GetANY_ATTRS().GetValues()
			if len(attrs) != 1 || attrs[0].GetATTR_KEY() != "interned" || attrs[0].GetJSON_VALUE() != "null" {
				t.Fatal("string_value_strindex projection is not ANY_ATTRS JSON null")
			}
		}
	}

	groups := inputs["raw_unknown_group_depth_10001"].ProtoReflect().GetUnknown()
	if len(groups) != 40004 || !bytes.Equal(groups, nestedGroups(10001, nil)) {
		t.Fatal("accepted 10001-level unknown group fixture has another depth")
	}
	group := inputs["raw_large_field_number_inside_group"].ProtoReflect().GetUnknown()
	if !bytes.Equal(group, nestedGroups(1, uvarint(1<<29, 7))) {
		t.Fatal("field number 2^29 inside an unknown group was not preserved")
	}
}

func TestGoDecodeErrorNormalizesOnlyPrefixWhitespace(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ input, want string }{
		{"proto:\u00a0cannot parse", "proto: cannot parse"},
		{"proto: cannot parse", "proto: cannot parse"},
		{"proto:\u00a0bad proto:\u00a0payload", "proto: bad proto:\u00a0payload"},
		{"wrapped proto:\u00a0message", "wrapped proto:\u00a0message"},
		{"proto:\u00a0", "proto: "},
		{"proto:\tmessage", "proto:\tmessage"},
	} {
		if got := goDecodeError(errors.New(c.input)); got != c.want {
			t.Errorf("goDecodeError(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestWireFuzzFixtureCases(t *testing.T) {
	t.Parallel()
	_, wf, _, err := fixture()
	if err != nil {
		t.Fatal(err)
	}
	fixedOutputs := 0
	for _, c := range wf.Cases {
		fixedOutputs += len(c.Outputs)
	}
	if len(wf.Cases) != 15 || fixedOutputs != 22 || len(wf.Malformed) != 13 {
		t.Fatal("fuzz additions changed the fixed fixture populations")
	}
	candidates := wireFuzzInputs()
	if len(candidates) != 82 || wf.FuzzCandidateCount != 82 || wf.FuzzFilteredCount < 4 || len(wf.FuzzCases)+wf.FuzzFilteredCount != 82 {
		t.Fatalf("fuzz census: %d candidates, %d recorded candidates, %d filtered, %d emitted", len(candidates), wf.FuzzCandidateCount, wf.FuzzFilteredCount, len(wf.FuzzCases))
	}
	cases := map[string]wireFuzzCase{}
	for _, c := range wf.FuzzCases {
		if _, exists := cases[c.Name]; exists || len(c.Input) > 4096 || c.Outputs == nil {
			t.Fatalf("duplicate, oversized or incomplete fuzz case %s", c.Name)
		}
		cases[c.Name] = c
	}
	controls := map[string]struct{ decodeError, admissible bool }{
		"control_valid": {false, true}, "control_decode_error": {true, false},
		"filter_identity": {false, false}, "filter_start": {false, false},
		"filter_duplicate": {false, false}, "filter_empty": {false, false},
	}
	accepted, rejected, filtered, shapes := 0, 0, 0, 0
	for _, in := range candidates {
		if len(in.input) > 4096 {
			t.Fatalf("oversized fuzz candidate %s", in.name)
		}
		rs := &tracepb.ResourceSpans{}
		decodeErr := proto.Unmarshal(in.input, rs)
		admissible := decodeErr == nil && fuzzAdmissible(rs)
		if want, control := controls[in.name]; control {
			if (decodeErr != nil) != want.decodeError || admissible != want.admissible {
				t.Fatalf("fuzz control %s: decode_error=%v admissible=%v", in.name, decodeErr, admissible)
			}
			delete(controls, in.name)
		}
		c, emitted := cases[in.name]
		if decodeErr == nil && !admissible {
			filtered++
			if emitted {
				t.Fatalf("filtered candidate %s was emitted", in.name)
			}
			continue
		}
		if !emitted || !bytes.Equal(c.Input, in.input) {
			t.Fatalf("candidate %s has no exact emitted input", in.name)
		}
		delete(cases, in.name)
		if decodeErr != nil {
			rejected++
			if c.DecodeError == nil || *c.DecodeError != goDecodeError(decodeErr) || len(c.Outputs) != 0 {
				t.Fatalf("%s did not retain the normalized Go decode error without outputs", in.name)
			}
			continue
		}
		accepted++
		singles, err := split(rs)
		if err != nil || c.DecodeError != nil || len(singles) == 0 || len(c.Outputs) != len(singles) {
			t.Fatalf("%s: incomplete success outputs: %v", in.name, err)
		}
		if strings.HasPrefix(in.name, "shape_") {
			shapes++
			if len(singles) != 1 {
				t.Fatalf("shape %s has %d spans", in.name, len(singles))
			}
		}
		for i, out := range c.Outputs {
			row := &otelv1pb.SPANS{}
			if err := proto.Unmarshal(out.Row, row); err != nil {
				t.Fatal(err)
			}
			if len(out.Row) == 0 || out.PK == "" || len(out.Keys) != 8 || !bytes.Equal(out.OTLP, singles[i].wire) || !bytes.Equal(row.OTLP, out.OTLP) ||
				!bytes.Equal(row.TRACE_ID, singles[i].sp.TraceId) || !bytes.Equal(row.SPAN_ID, singles[i].sp.SpanId) || len(row.ProtoReflect().GetUnknown()) != 0 {
				t.Fatalf("%s/%d lacks its Go-prepared OTLP, compiled row, identity or keys", in.name, i)
			}
		}
	}
	if len(cases) != 0 || len(controls) != 0 || filtered != wf.FuzzFilteredCount || accepted < 13 || rejected == 0 || shapes != 12 || accepted+rejected+filtered != 82 {
		t.Fatalf("fuzz populations: accepted=%d rejected=%d filtered=%d shapes=%d unmatched=%d controls=%d", accepted, rejected, filtered, shapes, len(cases), len(controls))
	}
}

func decodedMessageDepth(m protoreflect.Message) int {
	type node struct {
		message protoreflect.Message
		depth   int
	}
	pending := []node{{m, 1}}
	depth := 0
	for len(pending) > 0 {
		n := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		depth = max(depth, n.depth)
		n.message.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			if fd.Message() != nil && fd.IsList() {
				for i := 0; i < v.List().Len(); i++ {
					pending = append(pending, node{v.List().Get(i).Message(), n.depth + 1})
				}
			} else if fd.Message() != nil {
				pending = append(pending, node{v.Message(), n.depth + 1})
			}
			return true
		})
	}
	return depth
}

func TestWireCodecDepthBoundaries(t *testing.T) {
	t.Parallel()
	accepted, rejected := messageDepthFrame(10000), messageDepthFrame(10001)
	rs := &tracepb.ResourceSpans{}
	if err := proto.Unmarshal(accepted, rs); err != nil {
		t.Fatalf("10000 message levels: %v", err)
	}
	if depth := decodedMessageDepth(rs.ProtoReflect()); depth != 10000 {
		t.Fatalf("accepted message depth %d, want 10000", depth)
	}
	if err := (proto.UnmarshalOptions{RecursionLimit: 9999}).Unmarshal(accepted, &tracepb.ResourceSpans{}); err == nil {
		t.Fatal("10000 message levels fit a 9999 limit")
	}
	if err := proto.Unmarshal(rejected, &tracepb.ResourceSpans{}); err == nil {
		t.Fatal("10001 message levels fit the default limit")
	}
	if err := (proto.UnmarshalOptions{RecursionLimit: 10001}).Unmarshal(rejected, rs); err != nil {
		t.Fatalf("10001-level input is invalid even at its own limit: %v", err)
	}
	if depth := decodedMessageDepth(rs.ProtoReflect()); depth != 10001 {
		t.Fatalf("rejected message depth %d, want 10001", depth)
	}
	if err := proto.Unmarshal(groupDepthFrame(10001), &tracepb.ResourceSpans{}); err != nil {
		t.Fatalf("10001 unknown groups: %v", err)
	}
	if err := proto.Unmarshal(groupDepthFrame(10002), &tracepb.ResourceSpans{}); err == nil {
		t.Fatal("10002 unknown groups were accepted")
	}
	if err := proto.Unmarshal(cat(field(2, field(2, rawSpan(24))), uvarint(1<<29, 7)), &tracepb.ResourceSpans{}); err == nil {
		t.Fatal("field number 2^29 at message level was accepted")
	}
}
