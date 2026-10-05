//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// wsgAggregate is one aggregate of the streaming aggregation under test, as Go
// plans it and as the target's aggregatePartialState step builds its
// accumulator.
type wsgAggregate struct {
	op          string // the target's physical operator family
	field       string // TypedRecord field, "" for COUNT(*)
	lane        values.TypeCode
	fn          expressions.AggregateFunction
	ignoreNulls bool
	limit       int
}

var wsgLaneNames = map[values.TypeCode]string{
	values.TypeCodeInt: "INT", values.TypeCodeLong: "LONG", values.TypeCodeFloat: "FLOAT",
	values.TypeCodeDouble: "DOUBLE", values.TypeCodeString: "STRING",
}

var wsgAggregates = []wsgAggregate{
	{op: "COUNT_STAR", fn: expressions.AggCount, lane: values.TypeCodeLong},
	{op: "COUNT", field: "val_int64", fn: expressions.AggCount, lane: values.TypeCodeLong},
	{op: "SUM", field: "val_int32", fn: expressions.AggSum, lane: values.TypeCodeInt},
	{op: "SUM", field: "val_int64", fn: expressions.AggSum, lane: values.TypeCodeLong},
	{op: "SUM", field: "val_float", fn: expressions.AggSum, lane: values.TypeCodeFloat},
	{op: "SUM", field: "val_double", fn: expressions.AggSum, lane: values.TypeCodeDouble},
	{op: "AVG", field: "val_int32", fn: expressions.AggAvg, lane: values.TypeCodeInt},
	{op: "AVG", field: "val_double", fn: expressions.AggAvg, lane: values.TypeCodeDouble},
	{op: "MIN", field: "val_int32", fn: expressions.AggMin, lane: values.TypeCodeInt},
	{op: "MAX", field: "val_int64", fn: expressions.AggMax, lane: values.TypeCodeLong},
	{op: "MIN", field: "val_float", fn: expressions.AggMin, lane: values.TypeCodeFloat},
	{op: "MAX", field: "val_double", fn: expressions.AggMax, lane: values.TypeCodeDouble},
	{op: "BITMAP_CONSTRUCT_AGG", field: "val_int32", fn: expressions.AggBitmapConstructAgg, lane: values.TypeCodeInt},
	{op: "ARRAY_AGG", field: "val_int64", fn: expressions.AggArrayAgg, lane: values.TypeCodeLong, ignoreNulls: true, limit: values.ArrayAggNoLimit},
	{op: "ARRAY_AGG", field: "val_double", fn: expressions.AggArrayAgg, lane: values.TypeCodeDouble, ignoreNulls: true, limit: 2},
	{op: "ARRAY_AGG", field: "val_string", fn: expressions.AggArrayAgg, lane: values.TypeCodeString, limit: values.ArrayAggNoLimit},
}

// wsgRows is one group (the key columns are equal throughout), led by a row
// whose every operand is NULL so the first partial holds absent states.
var wsgRows = []*gen.TypedRecord{
	{Id: proto.Int64(1)},
	{Id: proto.Int64(2), ValInt32: proto.Int32(3), ValInt64: proto.Int64(10), ValFloat: proto.Float32(1.5), ValDouble: proto.Float64(2.25)},
	{Id: proto.Int64(3), ValInt64: proto.Int64(-4), ValDouble: proto.Float64(math.Copysign(0, -1))},
	{Id: proto.Int64(4), ValInt32: proto.Int32(9), ValFloat: proto.Float32(-2.5)},
	{Id: proto.Int64(5), ValInt32: proto.Int32(1), ValInt64: proto.Int64(5), ValFloat: proto.Float32(0.1), ValDouble: proto.Float64(8)},
	{Id: proto.Int64(6), ValInt32: proto.Int32(2), ValInt64: proto.Int64(7), ValFloat: proto.Float32(3), ValDouble: proto.Float64(0.5)},
}

// wsgInput renders a row's operand the way the target step parses it.
func wsgInput(r *gen.TypedRecord, field string) any {
	switch field {
	case "":
		return "1"
	case "val_string":
		return "g"
	case "val_int32":
		if r.ValInt32 != nil {
			return strconv.Itoa(int(*r.ValInt32))
		}
	case "val_int64":
		if r.ValInt64 != nil {
			return strconv.FormatInt(*r.ValInt64, 10)
		}
	case "val_float":
		if r.ValFloat != nil {
			return strconv.FormatFloat(float64(*r.ValFloat), 'g', -1, 32)
		}
	case "val_double":
		if r.ValDouble != nil {
			return strconv.FormatFloat(*r.ValDouble, 'g', -1, 64)
		}
	}
	return nil
}

func wsgSpec(rows []*gen.TypedRecord, states []string) map[string]any {
	aggs := make([]map[string]any, len(wsgAggregates))
	for i, a := range wsgAggregates {
		inputs := make([]any, len(rows))
		for j, r := range rows {
			inputs[j] = wsgInput(r, a.field)
		}
		spec := map[string]any{"op": a.op, "type": wsgLaneNames[a.lane], "inputs": inputs, "ignoreNulls": a.ignoreNulls, "limit": a.limit}
		if states != nil && states[i] != "" {
			spec["state"] = states[i]
		}
		aggs[i] = spec
	}
	return map[string]any{
		"groupKey": []map[string]any{
			{"type": "STRING", "value": "g"},
			{"type": "BOOLEAN", "value": "true"},
			{"type": "INT", "value": "7"},
			{"type": "BYTES", "value": nil},
		},
		"aggregates": aggs,
	}
}

func wsgJSON(v any) string {
	b, err := json.Marshal(v)
	Expect(err).NotTo(HaveOccurred())
	return string(b)
}

type wsgJavaPartial struct {
	GroupKey string   `json:"groupKey"`
	States   []string `json:"states"`
	Finished []any    `json:"finished"`
}

// The streaming aggregation's continuation carries Java's own partial state
// (StreamGrouping.getPartialAggregationResult): Go's grouping-key message and
// accumulator states are byte-identical to the target's for the same rows, the
// target finishes from Go's states with the uninterrupted answer, and Go
// resumes a continuation carrying the target's states. Every prefix of the
// group is taken, from the all-NULL first row on.
var _ = Describe("WSGAggregateStateConformance", func() {
	It("writes and resumes the target's aggregate partial state", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		java := NewJavaInvoker()

		b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		db := recordlayer.NewFDBDatabase(sharedDB)
		ss := subspace.FromBytes([]byte("wsg_agg_" + uuid.NewString()))
		open := func(rtx *recordlayer.FDBRecordContext) (*recordlayer.FDBRecordStore, error) {
			return recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
		}
		_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := open(rtx)
			if err != nil {
				return nil, err
			}
			for _, r := range wsgRows {
				r.ValString, r.ValBool, r.Price = proto.String("g"), proto.Bool(true), proto.Int32(7)
				if _, err := store.SaveRecord(r); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		plan := wsgPlan()
		type page struct {
			row  string
			cont []byte
		}
		run := func(cont []byte, limit int) page {
			out, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := open(rtx)
				if err != nil {
					return nil, err
				}
				props := recordlayer.DefaultExecuteProperties()
				if limit > 0 {
					props = props.WithScannedRecordsLimit(limit)
				}
				cursor, err := executor.ExecutePlan(ctx, plan, store, executor.EmptyEvaluationContext(), cont, props)
				if err != nil {
					return nil, err
				}
				defer cursor.Close()
				res, err := cursor.OnNext(ctx)
				if err != nil {
					return nil, err
				}
				if res.HasNext() {
					return page{row: fmt.Sprintf("%#v", res.GetValue().Positional.Slots)}, nil
				}
				if res.GetNoNextReason().IsSourceExhausted() {
					return page{}, nil
				}
				c, err := res.GetContinuation().ToBytes()
				return page{cont: c}, err
			})
			Expect(err).NotTo(HaveOccurred())
			return out.(page)
		}
		full := run(nil, 0)
		Expect(full.row).NotTo(BeEmpty())

		var whole wsgJavaPartial
		Expect(java.InvokeAs(ctx, "aggregatePartialState", map[string]any{"spec": wsgJSON(wsgSpec(wsgRows, nil))}, &whole)).To(Succeed())
		Expect(whole.Finished).To(HaveLen(len(wsgAggregates)))

		var seen []int64
		for limit := 1; limit < len(wsgRows); limit++ {
			paused := run(nil, limit)
			Expect(paused.cont).NotTo(BeEmpty(), "limit %d did not stop inside the group", limit)
			var cont gen.AggregateCursorContinuation
			Expect(proto.Unmarshal(paused.cont, &cont)).To(Succeed())
			par := cont.GetPartialAggregationResults()
			Expect(par).NotTo(BeNil(), "limit %d: no partial group", limit)
			Expect(par.GetAccumulatorStates()).To(HaveLen(len(wsgAggregates)))
			n := par.GetAccumulatorStates()[0].GetState()[0].GetInt64State()
			seen = append(seen, n)
			goStates := make([]string, len(wsgAggregates))
			for i, s := range par.GetAccumulatorStates() {
				b, err := proto.MarshalOptions{Deterministic: true}.Marshal(s)
				Expect(err).NotTo(HaveOccurred())
				goStates[i] = base64.StdEncoding.EncodeToString(b)
			}

			var target wsgJavaPartial
			Expect(java.InvokeAs(ctx, "aggregatePartialState", map[string]any{"spec": wsgJSON(wsgSpec(wsgRows[:n], nil))}, &target)).To(Succeed())
			fmt.Fprintf(GinkgoWriter, "WSG-AGG-STATE rows=%d go=%v java=%v\n", n, goStates, target.States)
			Expect(base64.StdEncoding.EncodeToString(par.GetGroupKey())).To(Equal(target.GroupKey), "rows %d: grouping key", n)
			Expect(goStates).To(Equal(target.States), "rows %d: accumulator states", n)

			var resumed wsgJavaPartial
			Expect(java.InvokeAs(ctx, "aggregatePartialState", map[string]any{"spec": wsgJSON(wsgSpec(wsgRows[n:], goStates))}, &resumed)).To(Succeed())
			Expect(resumed.Finished).To(Equal(whole.Finished), "rows %d: the target resumed Go's states", n)

			javaPartial := &gen.PartialAggregationResult{}
			javaPartial.GroupKey, err = base64.StdEncoding.DecodeString(target.GroupKey)
			Expect(err).NotTo(HaveOccurred())
			for _, s := range target.States {
				b, err := base64.StdEncoding.DecodeString(s)
				Expect(err).NotTo(HaveOccurred())
				state := &gen.AccumulatorState{}
				Expect(proto.Unmarshal(b, state)).To(Succeed())
				javaPartial.AccumulatorStates = append(javaPartial.AccumulatorStates, state)
			}
			cont.PartialAggregationResults = javaPartial
			fromJava, err := proto.Marshal(&cont)
			Expect(err).NotTo(HaveOccurred())
			Expect(run(fromJava, 0).row).To(Equal(full.row), "rows %d: Go resumed the target's states", n)
			Expect(run(paused.cont, 0).row).To(Equal(full.row), "rows %d: Go resumed its own continuation", n)
		}
		Expect(seen).To(Equal([]int64{1, 2, 3, 4, 5}))
	})
})

// wsgPlan is StreamingAgg(Scan(TypedRecord)) grouped by (val_string, val_bool,
// price, val_bytes) over wsgAggregates, typed as the planner types them.
func wsgPlan() plans.RecordQueryPlan {
	desc := (&gen.TypedRecord{}).ProtoReflect().Descriptor()
	rowType := executor.PositionalTypeForRecordLayout(desc, false)
	scan, err := plans.NewRecordQueryScanPlan([]string{"TypedRecord"}, rowType, false)
	Expect(err).NotTo(HaveOccurred())
	field := func(name string) values.Value {
		want := values.FieldNameForProtoField(desc.Fields().ByName(protoreflect.Name(name)))
		for i, f := range rowType.Fields {
			if f.Name == want {
				v, err := values.ResolveFieldOrdinals(scan.GetResultValue(), []int{i})
				Expect(err).NotTo(HaveOccurred())
				return v
			}
		}
		Fail("TypedRecord has no slot " + name)
		return nil
	}
	keys := []values.Value{field("val_string"), field("val_bool"), field("price"), field("val_bytes")}
	aggs := make([]expressions.AggregateSpec, len(wsgAggregates))
	for i, a := range wsgAggregates {
		spec := expressions.AggregateSpec{Function: a.fn, IgnoreNulls: a.ignoreNulls, Limit: a.limit}
		if a.field != "" {
			spec.Operand = field(a.field)
			if a.fn != expressions.AggArrayAgg && a.fn != expressions.AggCount {
				spec.OperandIntType = a.lane
			}
		}
		aggs[i] = spec
	}
	plan, err := plans.NewRecordQueryStreamingAggregationPlan(scan, keys, aggs)
	Expect(err).NotTo(HaveOccurred())
	return plan
}
