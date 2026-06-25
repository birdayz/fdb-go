package docscheck

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// TestPlanProtoSchemaMatches412 pins the regenerated record_query_plan.proto schema to the Java
// fdb-record-layer 4.12.11.0 shape (RFC-135). The bump from 4.11.1.0:
//   - removed message PVersionValue and RESERVED the PValue oneof tag 38;
//   - replaced PExistsPredicate with PExistentialValuePredicate on PQueryPredicate tag 4;
//   - added PExistsValue.value (tag 3);
//   - added PRecordQueryExplodePlan.with_ordinality (tag 2).
//
// This is a descriptor-reflection guard, NOT a stored-bytes test (Go marshals no plan through these
// protos — see RFC-135 §3). It lives in pkg/docscheck, not gen/, because `just generate` does
// `rm -rf gen/`. A regen against an older proto, or a schema drift, fails here — the sentinel Graefe
// asked for on RFC-135.
func TestPlanProtoSchemaMatches412(t *testing.T) {
	t.Parallel()

	// PValue oneof tag 38 (version_value) is reserved/removed in 4.12.
	pvalue := (&gen.PValue{}).ProtoReflect().Descriptor()
	if f := pvalue.Fields().ByNumber(38); f != nil {
		t.Errorf("PValue still carries field 38 (%s); 4.12 reserves it (PVersionValue removed)", f.Name())
	}
	if f := pvalue.Fields().ByName("version_value"); f != nil {
		t.Errorf("PValue still has a version_value field; 4.12 removed PVersionValue")
	}

	// PQueryPredicate tag 4 is existential_value_predicate -> PExistentialValuePredicate (was
	// exists_predicate -> PExistsPredicate in 4.11).
	pqp := (&gen.PQueryPredicate{}).ProtoReflect().Descriptor()
	f4 := pqp.Fields().ByNumber(4)
	if f4 == nil {
		t.Fatalf("PQueryPredicate is missing field 4 entirely")
	}
	if got := string(f4.Name()); got != "existential_value_predicate" {
		t.Errorf("PQueryPredicate field 4 = %q; want existential_value_predicate (4.12 renamed from exists_predicate)", got)
	}
	if f4.Kind() != protoreflect.MessageKind || string(f4.Message().Name()) != "PExistentialValuePredicate" {
		t.Errorf("PQueryPredicate field 4 message = %v; want PExistentialValuePredicate", f4.Message().FullName())
	}
	if pqp.Fields().ByName("exists_predicate") != nil {
		t.Errorf("PQueryPredicate still has exists_predicate; 4.12 replaced it with existential_value_predicate")
	}

	// PExistsValue.value (tag 3) added in 4.12.
	pev := (&gen.PExistsValue{}).ProtoReflect().Descriptor()
	if f := pev.Fields().ByNumber(3); f == nil || string(f.Name()) != "value" {
		t.Errorf("PExistsValue is missing field 3 'value' (added in 4.12); got %v", f)
	}

	// PRecordQueryExplodePlan.with_ordinality (tag 2) added in 4.12.
	pep := (&gen.PRecordQueryExplodePlan{}).ProtoReflect().Descriptor()
	if f := pep.Fields().ByNumber(2); f == nil || string(f.Name()) != "with_ordinality" {
		t.Errorf("PRecordQueryExplodePlan is missing field 2 'with_ordinality' (added in 4.12); got %v", f)
	}

	// The replacement message exists with its single 'super' field (PValuePredicate).
	pevp := (&gen.PExistentialValuePredicate{}).ProtoReflect().Descriptor()
	if pevp.Fields().ByName("super") == nil {
		t.Errorf("PExistentialValuePredicate is missing its 'super' field")
	}
}
