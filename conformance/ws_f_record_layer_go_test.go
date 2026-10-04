package conformance_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// wsfGoRecordLayerIn is the Go counterpart of the target step wsfRecordLayerIn
// (wsf_record_layer_probe.java): the same five orders in their own subspace (order_id
// 1..5, prices 10, 10, 20, absent, 10), `price IN prices` sorted by sort ("price",
// "order_id" or "" for none), planned the way a Go record-layer caller plans, with the
// exported Cascades planner (NewPlanner, match candidates from the index definition, no
// statistics) under DefaultPlannerConfiguration, or under the relational one
// (PREFER_INDEX, in-union size 24) when relational is set, and executed with
// executor.ExecutePlan. The line has the target step's form, so the two compare.
func wsfGoRecordLayerIn(ctx context.Context, prices []int, sort string, relational bool) string {
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	b.AddIndex("Order", recordlayer.NewIndex("wsf_price", recordlayer.Field("price")))
	md, err := b.Build()
	if err != nil {
		return "SETUP-ERROR metadata: " + wsfMsg(err.Error())
	}
	db := recordlayer.NewFDBDatabase(sharedDB)
	ss := subspace.FromBytes([]byte("wsf_rl_go_" + uuid.NewString()))
	open := func(rtx *recordlayer.FDBRecordContext) (*recordlayer.FDBRecordStore, error) {
		return recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
	}
	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := open(rtx)
		if err != nil {
			return nil, err
		}
		for _, r := range [][2]int32{{1, 10}, {2, 10}, {3, 20}, {4, -1}, {5, 10}} {
			order := &gen.Order{OrderId: proto.Int64(int64(r[0]))}
			if r[1] >= 0 {
				order.Price = proto.Int32(r[1])
			}
			if _, err := store.SaveRecord(order); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}); err != nil {
		return "SETUP-ERROR save: " + wsfMsg(err.Error())
	}

	plan, size, err := wsfGoPlanRecordLayerIn(ctx, prices, sort, relational)
	if err != nil {
		return "PLAN-ERROR " + wsfMsg(err.Error())
	}
	line := fmt.Sprintf("RL EXPLAIN %q size=%d", plan.Explain(), size)
	ids, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := open(rtx)
		if err != nil {
			return nil, err
		}
		cursor, err := executor.ExecutePlan(ctx, plan, store, executor.EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			return nil, err
		}
		defer cursor.Close()
		rows, err := executor.CollectAll(ctx, cursor)
		if err != nil {
			return nil, err
		}
		out := make([]int64, 0, len(rows))
		for _, row := range rows {
			if len(row.PrimaryKey) != 1 {
				return nil, fmt.Errorf("row primary key %v", row.PrimaryKey)
			}
			id, ok := row.PrimaryKey[0].(int64)
			if !ok {
				return nil, fmt.Errorf("row primary key element %T", row.PrimaryKey[0])
			}
			out = append(out, id)
		}
		return out, nil
	})
	if err != nil {
		return line + " EXECUTE-ERROR " + wsfMsg(err.Error())
	}
	return line + fmt.Sprintf(" ids=%v", ids)
}

// wsfGoPlanRecordLayerIn builds and plans the query graph of the target's
// RecordQuery(Order, price IN prices, sort): a type filter over the full scan, a
// select with the IN comparison, and a sort by the named field (none for "").
func wsfGoPlanRecordLayerIn(ctx context.Context, prices []int, sort string, relational bool) (plans.RecordQueryPlan, int, error) {
	desc := (&gen.Order{}).ProtoReflect().Descriptor()
	rowType := executor.PositionalTypeForRecordLayout(desc, false)
	ordinal := func(field string) (int, string, error) {
		fd := desc.Fields().ByName(protoreflect.Name(field))
		if fd == nil {
			return 0, "", fmt.Errorf("Order has no field %s", field)
		}
		name := values.FieldNameForProtoField(fd)
		for i, f := range rowType.Fields {
			if f.Name == name {
				return i, name, nil
			}
		}
		return 0, "", fmt.Errorf("Order's row type has no slot %s", name)
	}
	scan, err := expressions.NewFullUnorderedScanExpression([]string{"Order"}, rowType)
	if err != nil {
		return nil, 0, err
	}
	typeFilter, err := expressions.NewLogicalTypeFilterExpression([]string{"Order"}, expressions.ForEachQuantifier(expressions.InitialOf(scan)))
	if err != nil {
		return nil, 0, err
	}
	q := expressions.ForEachQuantifier(expressions.InitialOf(typeFilter))
	qov, err := values.NewQuantifiedObjectValue(q.GetAlias(), rowType)
	if err != nil {
		return nil, 0, err
	}
	priceOrdinal, priceName, err := ordinal("price")
	if err != nil {
		return nil, 0, err
	}
	price, err := values.ResolveFieldOrdinals(qov, []int{priceOrdinal})
	if err != nil {
		return nil, 0, err
	}
	list := make([]any, len(prices))
	for i, p := range prices {
		list[i] = int64(p)
	}
	in := predicates.NewComparisonPredicate(price, predicates.Comparison{
		Type: predicates.ComparisonIn, Operand: &values.ConstantValue{Value: list, Typ: values.TypeUnknown},
	})
	sel, err := expressions.NewSelectExpression(qov, []expressions.Quantifier{q}, []predicates.QueryPredicate{in})
	if err != nil {
		return nil, 0, err
	}
	var root expressions.RelationalExpression = sel
	if sort != "" {
		sq := expressions.ForEachQuantifier(expressions.InitialOf(sel))
		sortQOV, err := values.NewQuantifiedObjectValue(sq.GetAlias(), rowType)
		if err != nil {
			return nil, 0, err
		}
		sortOrdinal, _, err := ordinal(sort)
		if err != nil {
			return nil, 0, err
		}
		key, err := values.ResolveFieldOrdinals(sortQOV, []int{sortOrdinal})
		if err != nil {
			return nil, 0, err
		}
		sorted, err := expressions.NewLogicalSortExpression([]expressions.SortKey{{Value: key}}, sq)
		if err != nil {
			return nil, 0, err
		}
		root = sorted
	}
	_, orderIDName, err := ordinal("order_id")
	if err != nil {
		return nil, 0, err
	}
	base := cascades.NewPlanContextFromIndexDefs([]cascades.IndexDef{wsfIndexDef{
		name: "wsf_price", columns: []string{priceName}, recordTypes: []string{"Order"},
		primaryKey: []string{orderIDName}, row: rowType, root: recordlayer.Field("price"),
	}})
	cfg := cascades.DefaultPlannerConfiguration()
	if relational {
		cfg.IndexScanPreference = cascades.PreferIndex
		cfg.AttemptFailedInJoinAsUnionMaxSize = 24
	}
	planCtx := wsfConfiguredPlanContext{PlanContext: base, cfg: cfg}
	planner := cascades.NewPlanner(append(cascades.DefaultExpressionRules(), cascades.RewritingRules()...), planCtx).
		WithImplementationRules(cascades.DefaultImplementationRules()).
		WithPlanningExpressionRules(cascades.BatchAExpressionRules())
	best, _, err := planner.PlanWithContext(ctx, expressions.InitialOf(root))
	if err != nil {
		return nil, 0, err
	}
	physical, ok := best.(plans.RecordQueryPlan)
	if !ok {
		return nil, 0, fmt.Errorf("planner returned %T, not an executable plan", best)
	}
	return physical, cfg.AttemptFailedInJoinAsUnionMaxSize, nil
}

// wsfIndexDef is the index definition a record-layer caller hands
// NewPlanContextFromIndexDefs, derived from its meta-data as the SQL layer derives its
// own (embedded's metadataIndexDef): physical column names as the descriptor spells
// them, the row type, the key's and the primary key's physical component types (a
// candidate without them declines every binding), the stored root and its duplicates.
type wsfIndexDef struct {
	name                             string
	columns, recordTypes, primaryKey []string
	row                              *values.RecordType
	root                             recordlayer.KeyExpression
}

func (d wsfIndexDef) IndexName() string                { return d.name }
func (d wsfIndexDef) IndexColumnNames() []string       { return d.columns }
func (d wsfIndexDef) IndexRecordTypes() []string       { return d.recordTypes }
func (d wsfIndexDef) IndexIsUnique() bool              { return false }
func (d wsfIndexDef) IndexPrimaryKeyColumns() []string { return d.primaryKey }
func (d wsfIndexDef) IndexRowType() values.Type        { return d.row }
func (d wsfIndexDef) IndexCreatesDuplicates() bool     { return false }
func (d wsfIndexDef) IndexRootKeyExpression() *gen.KeyExpression {
	return d.root.ToKeyExpression()
}
func (d wsfIndexDef) IndexKeyComponentTypes() []values.Type { return d.types(d.columns) }
func (d wsfIndexDef) IndexPrimaryKeyComponentTypes() []values.Type {
	return d.types(d.primaryKey)
}

func (d wsfIndexDef) types(names []string) []values.Type {
	out := make([]values.Type, len(names))
	for i, n := range names {
		if f, ok := d.row.LookupFieldUnique(n); ok {
			out[i] = f.FieldType
		}
	}
	return out
}

// wsfConfiguredPlanContext is a PlanContext with a caller-chosen configuration.
type wsfConfiguredPlanContext struct {
	cascades.PlanContext
	cfg cascades.PlannerConfiguration
}

func (c wsfConfiguredPlanContext) GetPlannerConfiguration() cascades.PlannerConfiguration {
	return c.cfg
}

// wsfRLComparable is a record-layer line reduced to what the WS-F acceptance compares
// for the w8_rl rows: the access path, the in-union size, and the ids or the fact of a
// "too many IN values" failure (each engine words its error its own way).
func wsfRLComparable(engine, line string) string {
	const prefix = "RL EXPLAIN "
	if !strings.HasPrefix(line, prefix) {
		return line
	}
	rest := line[len(prefix):]
	end := strings.Index(rest, `" size=`)
	if end < 0 {
		return line
	}
	explain, err := strconv.Unquote(rest[:end+1])
	if err != nil {
		return line
	}
	tail := rest[end+2:]
	if i := strings.Index(tail, " EXECUTE-ERROR "); i >= 0 {
		outcome := "EXECUTE-ERROR"
		if strings.Contains(tail, "too many IN values") {
			outcome += " too many IN values"
		}
		tail = tail[:i] + " " + outcome
	}
	return wsfAccessPath(engine, explain) + " " + tail
}
