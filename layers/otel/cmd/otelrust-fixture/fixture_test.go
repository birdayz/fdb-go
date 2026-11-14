package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
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
	if len(outputs) != 0 || len(wf.Malformed) != 10 {
		t.Fatalf("missing cases %v or malformed count %d", outputs, len(wf.Malformed))
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
}
