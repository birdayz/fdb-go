package recordlayer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestTextPlannerRefusalRegressions(t *testing.T) {
	t.Parallel()
	builder := baseBuilder()
	plain := NewTextIndex("planner_plain", Field("name"))
	value := NewIndex("planner_value", Field("name"))
	grouped := NewTextIndex("planner_grouped", GroupBy(Field("name"), Field("email")))
	omitted := NewTextIndex("planner_omitted", Field("name")).SetOption(IndexOptionTextOmitPositions, "true")
	for _, index := range []*Index{plain, value, grouped, omitted} {
		builder.AddIndex("Customer", index)
	}
	md, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	all := NewTextComparison(TextContainsAll, "alpha beta", "", "")
	cases := []struct {
		name       string
		index      *Index
		query      TextQuery
		wantReason string
	}{
		{"non TEXT", value, TextQuery{RecordTypes: []string{"Customer"}, Comparison: all}, "index type value is not a text index"},
		{"nil comparison", plain, TextQuery{RecordTypes: []string{"Customer"}}, "no text comparison"},
		{"missing grouping equality", grouped, TextQuery{RecordTypes: []string{"Customer"}, Comparison: all}, "0 grouping values for 1 grouping columns"},
		{"too many grouping equalities", grouped, TextQuery{RecordTypes: []string{"Customer"}, GroupingKey: tuple.Tuple{"x", "y"}, Comparison: all}, "2 grouping values for 1 grouping columns"},
		{"ungrouped index", plain, TextQuery{RecordTypes: []string{"Customer"}, GroupingKey: tuple.Tuple{"x"}, Comparison: all}, "index has no grouping columns"},
		{"tokenizer mismatch", plain, TextQuery{RecordTypes: []string{"Customer"}, Comparison: NewTextComparison(TextContainsAll, "alpha", "planner_other_tokenizer", "")}, "comparison requires tokenizer \"planner_other_tokenizer\""},
		{"omitted phrase positions", omitted, TextQuery{RecordTypes: []string{"Customer"}, Comparison: NewTextComparison(TextContainsPhrase, "alpha beta", "", "")}, "TEXT_CONTAINS_PHRASE needs position lists the index omits"},
		{"omitted distance positions", omitted, TextQuery{RecordTypes: []string{"Customer"}, Comparison: NewTextWithMaxDistanceComparison("alpha beta", 2, "", "")}, "TEXT_CONTAINS_ALL_WITHIN needs position lists the index omits"},
		{"unknown type", plain, TextQuery{RecordTypes: []string{"Ghost"}, Comparison: all}, "index does not cover requested record type \"Ghost\""},
		{"unknown alongside covered type", plain, TextQuery{RecordTypes: []string{"Customer", "Ghost"}, Comparison: all}, "index does not cover requested record type \"Ghost\""},
		{"missing requested type", plain, TextQuery{RecordTypes: []string{"Customer", "Order"}, Comparison: all}, "index does not cover requested record type \"Order\""},
		{"all metadata types", plain, TextQuery{Comparison: all}, "index does not cover requested record type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan, err := PlanTextQuery(md, tc.index, tc.query)
			var refusal *TextQueryPlanError
			if plan != nil || !errors.As(err, &refusal) {
				t.Fatalf("plan=%v, want TextQueryPlanError, got %T: %v", plan, err, err)
			}
			if refusal.IndexName != tc.index.Name || !strings.Contains(refusal.Reason, tc.wantReason) {
				t.Fatalf("refusal=%+v, want index=%s reason=%q", refusal, tc.index.Name, tc.wantReason)
			}
		})
	}
	// Positive controls keep the refusals tied to the distinguishing option.
	for _, tc := range []struct {
		index *Index
		query TextQuery
	}{
		{plain, TextQuery{RecordTypes: []string{"Customer"}, Comparison: all}},
		{grouped, TextQuery{RecordTypes: []string{"Customer"}, GroupingKey: tuple.Tuple{"x"}, Comparison: all}},
		{omitted, TextQuery{RecordTypes: []string{"Customer"}, Comparison: all}},
	} {
		if plan, err := PlanTextQuery(md, tc.index, tc.query); err != nil || plan == nil {
			t.Fatalf("valid %s query: plan=%v, err=%v", tc.index.Name, plan, err)
		}
	}
}

// The demo types share price, but it is numeric. Give that field a string
// descriptor on each type so real multi-type TEXT indexes can use one body.
func textPlannerSharedBodyBuilder() (*RecordMetaDataBuilder, error) {
	file := protodesc.ToFileDescriptorProto(demopb.File_record_layer_demo_proto)
	for _, message := range file.MessageType {
		for _, field := range message.Field {
			if field.GetName() == "price" {
				field.Type = descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()
			}
		}
	}
	fd, err := protodesc.NewFile(file, protoregistry.GlobalFiles)
	if err != nil {
		return nil, err
	}
	builder := NewRecordMetaDataBuilder().SetRecords(fd)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	return builder, nil
}

func TestTextPlannerIncompatiblePrimaryKeyDedup(t *testing.T) {
	t.Parallel()
	builder, err := textPlannerSharedBodyBuilder()
	if err != nil {
		t.Fatal(err)
	}
	index := NewTextIndex("planner_multi_prefix", Field("price"))
	builder.AddMultiTypeIndex([]string{"Customer", "Order"}, index)
	md, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	query := TextQuery{RecordTypes: []string{"Customer", "Order"}, Comparison: NewTextComparison(TextContainsPrefix, "al", "", "")}
	plan, err := PlanTextQuery(md, index, query)
	var refusal *RecordCoreError
	if plan != nil || !errors.As(err, &refusal) {
		t.Fatalf("plan=%v, want common-primary-key error, got %T: %v", plan, err, err)
	}
	if refusal.IndexName != index.Name || refusal.Message != "A common primary key is required to remove duplicates" {
		t.Fatalf("wrong dedup refusal: %+v", refusal)
	}
	query.KeepDuplicates = true
	plan, err = PlanTextQuery(md, index, query)
	if err != nil || plan == nil || plan.RemovesDuplicates {
		t.Fatalf("keeping duplicates must not require a common PK: plan=%+v, err=%v", plan, err)
	}
}

var _ = Describe("TEXT planner type coverage regressions", func() {
	It("filters populated multi-type scans and accepts only complete type coverage", func() {
		ctx := context.Background()
		builder, err := textPlannerSharedBodyBuilder()
		Expect(err).NotTo(HaveOccurred())
		single := NewTextIndex("planner_customer", Field("price"))
		multi := NewTextIndex("planner_customer_order", Field("price"))
		universal := NewTextIndex("planner_universal", Field("price"))
		builder.AddIndex("Customer", single)
		builder.AddMultiTypeIndex([]string{"Customer", "Order"}, multi)
		builder.AddUniversalIndex(universal)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		ss := specSubspace()
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for i, name := range []string{"Order", "Customer", "TypedRecord"} {
				rt := md.GetRecordType(name)
				idField := rt.Descriptor.Fields().ByName([]protoreflect.Name{"order_id", "customer_id", "id"}[i])
				for j, body := range []string{"alpha beta", "omega"} {
					record := dynamicpb.NewMessage(rt.Descriptor)
					record.Set(idField, protoreflect.ValueOfInt64(int64(10*(i+1)+j)))
					record.Set(rt.Descriptor.Fields().ByName("price"), protoreflect.ValueOfString(body))
					if _, err := store.SaveRecord(record); err != nil {
						return nil, err
					}
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		comparison := NewTextComparison(TextContainsAll, "alpha", "", "")
		for _, tc := range []struct {
			index      *Index
			types      []string
			wantFilter []string
			wantRows   []string
		}{
			{single, []string{"Customer"}, nil, []string{"Customer:20:alpha beta"}},
			{multi, []string{"Customer"}, []string{"Customer"}, []string{"Customer:20:alpha beta"}},
			{multi, []string{"Order"}, []string{"Order"}, []string{"Order:10:alpha beta"}},
			{multi, []string{"Customer", "Order"}, nil, []string{"Order:10:alpha beta", "Customer:20:alpha beta"}},
			{universal, nil, nil, []string{"Order:10:alpha beta", "Customer:20:alpha beta", "TypedRecord:30:alpha beta"}},
			{universal, []string{"Customer", "Order"}, []string{"Customer", "Order"}, []string{"Order:10:alpha beta", "Customer:20:alpha beta"}},
		} {
			query := TextQuery{RecordTypes: tc.types, Comparison: comparison}
			plan, err := PlanTextQuery(md, tc.index, query)
			Expect(err).NotTo(HaveOccurred())
			Expect(plan).NotTo(BeNil())
			Expect(plan.TextPlan.IndexName).To(Equal(tc.index.Name))
			Expect(plan.TextPlan.TextScan.Index).To(BeIdenticalTo(tc.index))
			Expect(plan.TextPlan.TextScan.Comparison).To(BeIdenticalTo(comparison))
			Expect(plan.TextPlan.TextScan.GroupingKey).To(BeNil())
			Expect(plan.TextPlan.Reverse).To(BeFalse())
			Expect(plan.TypeFilter).To(Equal(tc.wantFilter))
			Expect(plan.StrictFilter).To(BeFalse())
			Expect(plan.RemovesDuplicates).To(BeFalse())
			rows, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}
				cursor, err := plan.Execute(store, nil, DefaultExecuteProperties())
				if err != nil {
					return nil, err
				}
				defer cursor.Close()
				var attempt []string
				for {
					result, err := cursor.OnNext(ctx)
					if err != nil {
						return nil, err
					}
					if !result.HasNext() {
						if result.GetNoNextReason() != SourceExhausted {
							return nil, fmt.Errorf("unexpected text query stop: %v", result.GetNoNextReason())
						}
						return attempt, nil
					}
					record := result.GetValue().Record
					message := record.Record.ProtoReflect()
					body := message.Get(message.Descriptor().Fields().ByName("price")).String()
					attempt = append(attempt, fmt.Sprintf("%s:%d:%s", record.RecordType.Name, record.PrimaryKey[0], body))
				}
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(rows.([]string)).To(ConsistOf(tc.wantRows), "index=%s types=%v", tc.index.Name, tc.types)
		}

		for _, tc := range []struct {
			index *Index
			types []string
		}{
			{single, []string{"Customer", "Order"}},
			{single, nil},
			{multi, nil},
			{multi, []string{"Customer", "Order", "TypedRecord"}},
			{universal, []string{"Ghost"}},
		} {
			plan, err := PlanTextQuery(md, tc.index, TextQuery{RecordTypes: tc.types, Comparison: comparison})
			Expect(plan).To(BeNil())
			var refusal *TextQueryPlanError
			Expect(errors.As(err, &refusal)).To(BeTrue(), "index=%s types=%v error=%v", tc.index.Name, tc.types, err)
			Expect(refusal.IndexName).To(Equal(tc.index.Name))
			Expect(refusal.Reason).To(ContainSubstring("index does not cover requested record type"))
		}
	})
})
