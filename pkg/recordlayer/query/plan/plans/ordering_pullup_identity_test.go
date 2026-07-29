package plans

// A single-child plan that reshapes its row states its provided ordering in ITS
// OWN output layout, not its child's.
//
// Java does this at every level: OrderingProperty.visitMapPlan
// (OrderingProperty.java:281-287) is `childOrdering.pullUp(resultValue, …,
// AliasMap.ofAliases(inner.getAlias(), Quantifier.current()), …)`. The child's
// keys never reach a consumer in the child's coordinate system.
//
// Go republished them verbatim, and that is a WRONG-VALUE bug rather than a
// representation quibble — pinned end to end by
// yamsql/testdata/streaming_agg_projection_layout.yaml, where a derived table
// swapping two column names hands a streaming aggregate an adjacency guarantee
// for the wrong column and splits every group. These tests pin the mechanism
// underneath that: the three axes a verbatim republication gets wrong, each
// separately assertable.
//
//   - the NAME: a renaming projection must advertise its OUTPUT name.
//   - the ORDINAL and its DOMAIN: the slot in the projection's own layout,
//     stated in a token derived from that layout.
//   - the ROOT: source-local, Go's stand-in for Java's Quantifier.current(),
//     because a key left rooted at the child quantifier cannot be pulled up a
//     second time by an enclosing join.
//
// The fourth axis is the fail-closed one: a projection that DROPS the ordering
// column has no ordering to state, and claiming the child's is claiming an order
// over a column the row does not contain.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// pullUpBaseLayout is the row the child scan flows: four columns, so an ordinal
// is genuinely discriminating.
func pullUpBaseLayout() *values.RecordType {
	return values.NewRecordType("T", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "A", FieldType: values.NullableLong, Ordinal: 1},
		{Name: "B", FieldType: values.NullableLong, Ordinal: 2},
		{Name: "C", FieldType: values.NullableLong, Ordinal: 3},
	})
}

// pkScanOrderedByA is a primary scan whose PK — and therefore whose provided
// ordering — is column A at base-layout ordinal 1. A REAL provider, not a
// hand-built key: the pull-up must consume what the providers actually mint.
func pkScanOrderedByA(t *testing.T) *RecordQueryScanPlan {
	t.Helper()
	layout := pullUpBaseLayout()
	scan := NewRecordQueryScanPlan([]string{"T"}, layout, false).
		WithPrimaryKey([]values.Value{
			&values.FieldValue{Field: "A", Typ: values.NullableLong},
		})
	provided := PKScanOrdering(scan)
	if !provided.IsKnown || len(provided.Keys) != 1 {
		t.Fatalf("test setup: scan provided ordering = %+v, want 1 known key", provided)
	}
	ident, ok := values.OrderingIdentityOf(provided.Keys[0])
	if !ok || ident.Ordinal != 1 {
		t.Fatalf("test setup: scan key %q has identity %+v ok=%v, want ordinal 1 of "+
			"the base layout", values.ExplainValue(provided.Keys[0]), ident, ok)
	}
	return scan
}

// projectionOver builds the physical projection the memo holds: a live child
// quantifier over the scan, projections and output aliases in parallel.
func projectionOver(inner RecordQueryPlan, projections []values.Value, aliases []string) *RecordQueryProjectionPlan {
	innerQ := QuantifierOverPlan(inner)
	return NewRecordQueryProjectionPlanFromQuantifier(projections, aliases, innerQ)
}

// baseField reads column `name` at `ordinal` of the base layout, the way the
// translator bakes a projection element.
func baseField(name string, ordinal int) values.Value {
	return values.NewFieldValueWithResolvedOrdinalInDomain(
		name, ordinal, values.NullableLong,
		values.OrdinalDomainOfType(pullUpBaseLayout()))
}

// TestProjectionOrderingIsStatedInItsOwnLayout is the primary net: a projection
// that RENAMES its ordered column must advertise the output name, output ordinal
// and output layout token — not the child's.
//
// The projection is `SELECT b AS a, a AS b` over a scan ordered by A. Every one
// of the three axes moves: the name A becomes B, the ordinal 1 becomes 1 in a
// DIFFERENT layout, and the domain becomes the projection's (A, B). A verbatim
// republication passes none of them, and the name axis is the one that shipped
// wrong rows.
func TestProjectionOrderingIsStatedInItsOwnLayout(t *testing.T) {
	t.Parallel()

	scan := pkScanOrderedByA(t)
	// Output slot 0 is named A and holds the child's B; slot 1 is named B and
	// holds the child's A — the ordered column. The swap is what makes the name
	// axis load-bearing: a verbatim key spells A, and A is the slot that is NOT
	// ordered.
	proj := projectionOver(scan,
		[]values.Value{baseField("B", 2), baseField("A", 1)},
		[]string{"A", "B"})

	got := proj.HintOrdering()
	if !got.IsKnown || len(got.Keys) != 1 {
		t.Fatalf("projection provided ordering = %+v, want exactly 1 known key "+
			"pulled up from the scan's A", got)
	}
	key := got.Keys[0]

	outputDomain := values.OrdinalDomainOfColumnNames([]string{"A", "B"})
	if !outputDomain.IsKnown() {
		t.Fatalf("test setup: the projection's output layout has no token")
	}

	ident, ok := values.OrderingIdentityOf(key)
	if !ok {
		t.Fatalf("projection provided key %q has NO column identity, so no "+
			"identity-keyed consumer can address it", values.ExplainValue(key))
	}
	if ident.Domain != outputDomain {
		t.Fatalf("projection provided key %q states domain %v, want the "+
			"PROJECTION's output layout %v.\n\n"+
			"A key stated in the CHILD's layout is the ordinal conflation: it reads "+
			"as authoritative while addressing another row. This is the axis Java's "+
			"OrderingProperty.visitMapPlan closes by pulling the child ordering up "+
			"through the result value.",
			values.ExplainValue(key), ident.Domain, outputDomain)
	}
	if ident.Ordinal != 1 {
		t.Fatalf("projection provided key %q addresses output ordinal %d, want 1 "+
			"(the slot holding the child's ordered column A, named B on output)",
			values.ExplainValue(key), ident.Ordinal)
	}
	fv, isField := key.(*values.FieldValue)
	if !isField || fv.Field != "B" {
		t.Fatalf("projection provided key %q carries display name %q, want \"B\".\n\n"+
			"This is the axis that shipped WRONG ROWS. Every value<->value match site "+
			"still compares the accessor NAME path (values.ColumnNamePathsEqual), so a "+
			"key spelled with the CHILD's name for a column the projection emits under "+
			"a different name resolves to the wrong slot. At a streaming-aggregate "+
			"boundary that splits every group -- see "+
			"yamsql/testdata/streaming_agg_projection_layout.yaml.",
			values.ExplainValue(key), fv.Field)
	}
	if fv.Child != nil {
		t.Fatalf("projection provided key %q is rooted at %q rather than being "+
			"SOURCE-LOCAL.\n\n"+
			"Java pulls up against Quantifier.current(); Go's canonical "+
			"source-relative root is the childless value. A key left rooted at the "+
			"projection's inner quantifier cannot be pulled up a SECOND time by an "+
			"enclosing join -- CanBridgeOrderingValueRoots refuses two QOV-rooted "+
			"values by design -- and the join silently loses its ordering. Measured: "+
			"15772 plan-shape golden lines of appearing InMemorySorts.",
			values.ExplainValue(key), values.ExplainValue(fv.Child))
	}
}

// TestProjectionThatDropsTheOrderedColumnClaimsNoOrdering pins the fail-closed
// direction, which is as load-bearing as the positive case.
//
// `SELECT b, c` over a scan ordered by A does not emit A at all. There is no
// output column to state the order over, so the only honest answer is NO
// ordering. Republishing the child's key here advertises an order over a column
// the row does not contain — a claim an evaluating consumer (a merge comparison
// key) would resolve against some other slot entirely.
func TestProjectionThatDropsTheOrderedColumnClaimsNoOrdering(t *testing.T) {
	t.Parallel()

	scan := pkScanOrderedByA(t)
	proj := projectionOver(scan,
		[]values.Value{baseField("B", 2), baseField("C", 3)},
		[]string{"B", "C"})

	got := proj.HintOrdering()
	if got.IsKnown || len(got.Keys) != 0 {
		t.Fatalf("projection that does not emit the ordered column claims "+
			"ordering %+v (keys %d). The output row has no A, so this order is "+
			"stated over a column no consumer above can read.",
			got, len(got.Keys))
	}
}

// TestProjectionOrderingTruncatesRatherThanSkipping pins that a lost key takes
// its DEPENDENTS with it.
//
// The scan is ordered by (A, C); the projection emits C but not A. Key 2 (C)
// only orders rows that TIE on key 1 (A), so keeping C alone would advertise a
// global order by C that the stream does not have. Java's Ordering.pullUp drops
// a key whose prerequisite was not translatable for the same reason
// (RichOrdering.translateKeys documents it on the rich side).
func TestProjectionOrderingTruncatesRatherThanSkipping(t *testing.T) {
	t.Parallel()

	layout := pullUpBaseLayout()
	scan := NewRecordQueryScanPlan([]string{"T"}, layout, false).
		WithPrimaryKey([]values.Value{
			&values.FieldValue{Field: "A", Typ: values.NullableLong},
			&values.FieldValue{Field: "C", Typ: values.NullableLong},
		})
	if provided := PKScanOrdering(scan); !provided.IsKnown || len(provided.Keys) != 2 {
		t.Fatalf("test setup: scan provided ordering = %+v, want 2 known keys (A, C)",
			provided)
	}

	proj := projectionOver(scan,
		[]values.Value{baseField("B", 2), baseField("C", 3)},
		[]string{"B", "C"})

	got := proj.HintOrdering()
	if got.IsKnown && len(got.Keys) > 0 {
		t.Fatalf("projection ordering = %+v: the leading key A is not emitted, so "+
			"the trailing key C was kept on its own. C only orders rows that tie on "+
			"A; advertising it alone is a lexicographic guarantee the stream does not "+
			"have.", got)
	}
}

// TestIdentityProjectionKeepsItsChildOrdering is the control that keeps the
// pull-up from being satisfiable by simply abstaining.
//
// A projection that passes its ordered column through under the SAME name has an
// order to state, and must state it. Without this the two tests above are
// satisfied by a body that returns Ordering{} unconditionally.
func TestIdentityProjectionKeepsItsChildOrdering(t *testing.T) {
	t.Parallel()

	scan := pkScanOrderedByA(t)
	proj := projectionOver(scan,
		[]values.Value{baseField("A", 1), baseField("B", 2)},
		[]string{"A", "B"})

	got := proj.HintOrdering()
	if !got.IsKnown || len(got.Keys) != 1 {
		t.Fatalf("pass-through projection provided ordering = %+v, want 1 known "+
			"key: the ordered column IS emitted, under its own name, so abstaining "+
			"here costs a sort for nothing", got)
	}
	ident, ok := values.OrderingIdentityOf(got.Keys[0])
	if !ok || ident.Ordinal != 0 ||
		ident.Domain != values.OrdinalDomainOfColumnNames([]string{"A", "B"}) {
		t.Fatalf("pass-through projection key %q has identity %+v ok=%v, want "+
			"ordinal 0 of the projection's own (A, B) layout",
			values.ExplainValue(got.Keys[0]), ident, ok)
	}
}

// TestStreamingAggregationGroupKeysStateTheirDomain pins the sixth ordering
// provider, the one that still minted a bare ordinal.
//
// A group key's advertised ordinal is its position in the aggregate's OUTPUT row,
// and GroupByOutputColumnNames is the single authority for that row — grouping
// keys in GROUP BY order, then aggregates. Without a token derived from that
// list the ordinal has no layout to answer for: values.OrderingIdentityOf
// declines an unknown domain, so the key is unaddressable by identity and only
// its rendering is left, which is the name channel every other provider in this
// file has stopped using.
//
// The aggregate column is included deliberately. It is part of the output layout
// and therefore part of its token, so a domain derived from the grouping keys
// alone would pass a weaker test and fail this one.
func TestStreamingAggregationGroupKeysStateTheirDomain(t *testing.T) {
	t.Parallel()

	groupKeys := []values.Value{
		&values.FieldValue{Field: "REGION", Typ: values.NullableString},
		&values.FieldValue{Field: "STATUS", Typ: values.NullableString},
	}
	aggregates := []expressions.AggregateSpec{{
		Function: expressions.AggCount,
	}}
	inner := NewRecordQueryScanPlan([]string{"T"}, pullUpBaseLayout(), false)
	agg := NewRecordQueryStreamingAggregationPlanFromQuantifier(
		QuantifierOverPlan(inner), groupKeys, aggregates)

	want := values.OrdinalDomainOfColumnNames(
		expressions.GroupByOutputColumnNames(groupKeys, aggregates))
	if !want.IsKnown() {
		t.Fatalf("test setup: the aggregate's output layout has no token")
	}

	got := agg.HintOrdering()
	if !got.IsKnown || len(got.Keys) != 2 {
		t.Fatalf("streaming aggregate provided ordering = %+v, want 2 known keys", got)
	}
	for i, key := range got.Keys {
		ident, ok := values.OrderingIdentityOf(key)
		if !ok {
			t.Fatalf("group-key ordering key %q has NO column identity. An ordinal "+
				"with no layout token is an ordinal no consumer may trust: "+
				"OrderingIdentityOf declines it, leaving only the rendering.",
				values.ExplainValue(key))
		}
		if ident.Ordinal != i {
			t.Fatalf("group-key ordering key %q addresses output ordinal %d, want %d",
				values.ExplainValue(key), ident.Ordinal, i)
		}
		if ident.Domain != want {
			t.Fatalf("group-key ordering key %q states domain %v, want the "+
				"aggregate's OUTPUT layout %v (grouping keys then aggregates, the "+
				"order the executor's aggregateCursor emits and the translator bakes "+
				"downstream references against)",
				values.ExplainValue(key), ident.Domain, want)
		}
	}
}

// TestMapPlanOrderingIsStatedInItsOwnLayout pins the SAME pull-up on the map
// plan, which is Java's RecordQueryMapPlan itself — the exact node
// OrderingProperty.visitMapPlan is written for.
//
// It is a SEPARATE body over a stored result value rather than a reassembled
// one, so a conversion that taught only the projection would leave the map
// republishing verbatim while every projection test stayed green. Go split
// Java's one node into two; both owe the pull-up.
func TestMapPlanOrderingIsStatedInItsOwnLayout(t *testing.T) {
	t.Parallel()

	scan := pkScanOrderedByA(t)
	// The same swap, expressed as a map's result value: output slot A holds the
	// child's B, output slot B holds the child's ordered A.
	result := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "A", Value: baseField("B", 2)},
		values.RecordConstructorField{Name: "B", Value: baseField("A", 1)},
	)
	mapPlan := NewRecordQueryMapPlanFromQuantifier(QuantifierOverPlan(scan), result)

	got := mapPlan.HintOrdering()
	if !got.IsKnown || len(got.Keys) != 1 {
		t.Fatalf("map provided ordering = %+v, want exactly 1 known key pulled up "+
			"from the scan's A", got)
	}
	fv, isField := got.Keys[0].(*values.FieldValue)
	if !isField || fv.Field != "B" || fv.Child != nil {
		t.Fatalf("map provided key %q, want a SOURCE-LOCAL read of the output "+
			"column \"B\".\n\n"+
			"A map is Java's RecordQueryMapPlan, the node visitMapPlan pulls the "+
			"child ordering up through. Republishing the child's key verbatim "+
			"advertises the name A over a row whose A is a different column.",
			values.ExplainValue(got.Keys[0]))
	}
	ident, ok := values.OrderingIdentityOf(got.Keys[0])
	if !ok || ident.Ordinal != 1 ||
		ident.Domain != values.OrdinalDomainOfColumnNames([]string{"A", "B"}) {
		t.Fatalf("map provided key %q has identity %+v ok=%v, want ordinal 1 of "+
			"the map's own (A, B) output layout",
			values.ExplainValue(got.Keys[0]), ident, ok)
	}
}
