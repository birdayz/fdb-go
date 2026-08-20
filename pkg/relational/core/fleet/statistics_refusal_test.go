package fleet

import (
	"strings"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// A TENANT WHOSE STATISTICS CAN NEVER BE USED IS REFUSED, NOT FAILED.
//
// The reader rejects metadata declaring joined or unnested synthetic types
// outright: RecordTypes() omits them, so completeness is undecidable against the
// partial list it returns. The fan-out refuses before scanning, which would
// otherwise bill a full pass per tenant for a set the planner has already
// decided to reject.
//
// REFUSED rather than FAILED, and that distinction is the finding this pins.
// `failed` tells an operator to retry, and no retry changes a property of the
// metadata. It is easy to get wrong in one specific way: returning a non-nil
// error from the step makes fanOut stamp OutcomeFailed unconditionally, so the
// step must set the outcome itself and return nil. A previous revision did the
// former while its comment, its commit subject and RFC-236 all said REFUSED —
// three sites asserting an outcome the code could not produce, and no test.
//
// It reaches production code without a cluster: the relational SQL layer cannot
// construct synthetic metadata, so a driver-level test cannot get here at all.
func TestSyntheticRefusalIsRefusedNotFailed(t *testing.T) {
	t.Parallel()

	md := syntheticMetaData(t)
	if !md.DeclaresSyntheticRecordTypes() {
		t.Fatal("the fixture declares no synthetic types, so this test cannot reach the " +
			"refusal it exists to pin")
	}

	ev, refused := syntheticRefusal(md)
	if !refused {
		t.Fatal("metadata declaring synthetic types was not refused — the completeness " +
			"gate would then certify a schema from a type list that omits them")
	}
	if ev.Outcome != OutcomeRefused {
		t.Errorf("Outcome = %q, want %q. FAILED reads as a fault and tells an operator to "+
			"retry; REFUSED reads as a decision and counts separately in the summary.",
			ev.Outcome, OutcomeRefused)
	}
	if ev.Err == nil {
		t.Fatal("a refusal with no Err prints nothing: the refused arm renders only Err")
	}
	if !strings.Contains(ev.Err.Error(), "JoinedAB") {
		t.Errorf("the refusal must name the declarations that caused it, or it is a verdict "+
			"an operator cannot act on: %v", ev.Err)
	}

	// Control: ordinary metadata is not refused, or the arm above would fire for
	// every tenant and this test would pass while the feature was disabled.
	plainBuilder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	plainBuilder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	plainBuilder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	plainBuilder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	plain, err := plainBuilder.Build()
	if err != nil {
		t.Fatalf("build plain metadata: %v", err)
	}
	if _, refusedPlain := syntheticRefusal(plain); refusedPlain {
		t.Error("ordinary metadata was refused — the guard fires on every schema")
	}
}

// syntheticMetaData builds metadata carrying a joined-type declaration, which is
// what this port preserves opaquely and does not model.
func syntheticMetaData(t *testing.T) *recordlayer.RecordMetaData {
	t.Helper()
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	md, err := builder.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	p, err := md.ToProto()
	if err != nil {
		t.Fatalf("to proto: %v", err)
	}
	name := "JoinedAB"
	p.JoinedRecordTypes = append(p.JoinedRecordTypes, &gen.JoinedRecordType{Name: &name})
	got, err := recordlayer.RecordMetaDataFromProto(p)
	if err != nil {
		t.Fatalf("from proto: %v", err)
	}
	return got
}
