package values

import (
	"bytes"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/indexfetch"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// TupleSource carries the proto's numbers (PTupleSource KEY=1, VALUE=2,
// OTHER=3), so a codec converts it by value and the zero value is no source.
func TestTupleSourceIsTheProtoEnum(t *testing.T) {
	t.Parallel()
	for _, s := range []TupleSource{TupleSourceKey, TupleSourceValue, TupleSourceOther} {
		if got := gen.PIndexKeyValueToPartialRecord_PTupleSource(s).String(); got != s.String() {
			t.Errorf("TupleSource %s is proto %s", s, got)
		}
	}
	var zero TupleSource
	if zero.String() != "INVALID" {
		t.Errorf("the zero TupleSource is %s, want no source", zero)
	}
}

// The 4.14.2.0 plan schema: the tags the target retired stay reserved and
// the ones it added carry the messages Go reads them as.
func TestPlanSchemaTags(t *testing.T) {
	t.Parallel()
	reserved := func(md protoreflect.MessageDescriptor, tag protoreflect.FieldNumber) bool {
		return md.ReservedRanges().Has(tag)
	}
	pvalue := (&gen.PValue{}).ProtoReflect().Descriptor()
	plan := (&gen.PRecordQueryPlan{}).ProtoReflect().Descriptor()
	for _, c := range []struct {
		md  protoreflect.MessageDescriptor
		tag protoreflect.FieldNumber
	}{{pvalue, 38}, {pvalue, 58}, {plan, 25}} {
		if !reserved(c.md, c.tag) || c.md.Fields().ByNumber(c.tag) != nil {
			t.Errorf("%s tag %d is not reserved", c.md.FullName(), c.tag)
		}
	}
	for _, c := range []struct {
		md   protoreflect.MessageDescriptor
		tag  protoreflect.FieldNumber
		name protoreflect.Name
	}{
		{pvalue, 64, "array_agg_value"},
		{pvalue, 41, "index_entry_object_value"},
		{plan, 38, "streaming_aggregation_plan"},
		{plan, 41, "covering_index_value_plan"},
		{(&gen.PRecordQueryExplodePlan{}).ProtoReflect().Descriptor(), 3, "zero_based_ordinality"},
		{(&gen.PRecordQueryAggregateIndexPlan{}).ProtoReflect().Descriptor(), 7, "result_type"},
		{(&gen.PlannerConfiguration{}).ProtoReflect().Descriptor(), 15, "vectorIndexEnginePreference"},
		{(&gen.PRecordQueryIndexPlan{}).ProtoReflect().Descriptor(), 4, "index_fetch_method"},
		{(&gen.PlannerConfiguration{}).ProtoReflect().Descriptor(), 10, "indexFetchMethod"},
	} {
		if fd := c.md.Fields().ByNumber(c.tag); fd == nil || fd.Name() != c.name {
			t.Errorf("%s tag %d = %v, want %s", c.md.FullName(), c.tag, fd, c.name)
		}
	}
}

// The index fetch method has two wire enums numbered differently: the plan's
// PIndexFetchMethod is 1..3 (record_query_plan.proto) and the planner
// configuration's IndexFetchMethod is 0..2 (record_planner_config.proto). Java
// always writes both fields, SCAN_AND_FETCH included. Go serializes neither
// yet (DIVERGENCES.md), so a codec must map indexfetch.Method by name, never
// cast it.
func TestIndexFetchMethodWireNumbers(t *testing.T) {
	t.Parallel()
	methods := []indexfetch.Method{indexfetch.ScanAndFetch, indexfetch.UseRemoteFetch, indexfetch.UseRemoteFetchWithFallback}
	if got := len(gen.PIndexFetchMethod_name); got != len(methods) {
		t.Fatalf("PIndexFetchMethod has %d values, indexfetch.Method %d", got, len(methods))
	}
	if got := len(gen.PlannerConfiguration_IndexFetchMethod_name); got != len(methods) {
		t.Fatalf("PlannerConfiguration.IndexFetchMethod has %d values, indexfetch.Method %d", got, len(methods))
	}
	for i, m := range methods {
		plan := gen.PIndexFetchMethod(gen.PIndexFetchMethod_value[m.String()])
		if int(plan) != i+1 {
			t.Errorf("PIndexFetchMethod %s = %d, want %d", m, plan, i+1)
		}
		config := gen.PlannerConfiguration_IndexFetchMethod(gen.PlannerConfiguration_IndexFetchMethod_value[m.String()])
		if int(config) != i || config.String() != m.String() {
			t.Errorf("PlannerConfiguration.IndexFetchMethod %s = %d (%s), want %d", m, config, config, i)
		}
		encoded, err := proto.Marshal(&gen.PRecordQueryIndexPlan{IndexFetchMethod: plan.Enum()})
		if err != nil || !bytes.Equal(encoded, []byte{0x20, byte(i + 1)}) {
			t.Errorf("PRecordQueryIndexPlan{%s} = % x (%v), want 20 %02x", m, encoded, err, i+1)
		}
		encoded, err = proto.Marshal(&gen.PlannerConfiguration{IndexFetchMethod: config.Enum()})
		if err != nil || !bytes.Equal(encoded, []byte{0x50, byte(i)}) {
			t.Errorf("PlannerConfiguration{%s} = % x (%v), want 50 %02x", m, encoded, err, i)
		}
	}
}
