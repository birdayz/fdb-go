package values

import (
	"testing"

	"fdb.dev/gen"
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
	} {
		if fd := c.md.Fields().ByNumber(c.tag); fd == nil || fd.Name() != c.name {
			t.Errorf("%s tag %d = %v, want %s", c.md.FullName(), c.tag, fd, c.name)
		}
	}
}
