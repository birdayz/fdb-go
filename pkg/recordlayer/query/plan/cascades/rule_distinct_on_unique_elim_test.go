package cascades

import (
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"google.golang.org/protobuf/proto"
)

// pkPlanContext provides PK column info for testing distinct
// elimination in the PLANNING phase.
type pkPlanContext struct {
	pk map[string][]string // record type → PK column names
}

func (c *pkPlanContext) GetPlannerConfiguration() PlannerConfiguration {
	return DefaultPlannerConfiguration()
}

func (c *pkPlanContext) GetMatchCandidates() []MatchCandidate { return nil }

func (c *pkPlanContext) GetPrimaryKeyColumns(recordType string) []string {
	if c.pk == nil {
		return nil
	}
	return c.pk[recordType]
}

// makeFakePlanWrapper creates a trivial physical plan that can be
// inserted as a FinalMember of a Reference. Used to simulate what
// the planner's bottom-up implementation phase would produce.
func makeFakePlanWrapper(recType string) *plans.RecordQueryScanPlan {
	return plans.NewRecordQueryScanPlan([]string{recType}, values.UnknownType, false)
}

// buildDistinctOverProjection creates:
//
//	Distinct(Projection([projected...], Scan([recType])))
//
// and returns the Distinct Reference with a physical FinalMember
// in the inner (projection) Reference.
func buildDistinctOverProjection(
	recType string,
	projected []values.Value,
) *expressions.Reference {
	scan := expressions.NewFullUnorderedScanExpression([]string{recType}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	proj := expressions.NewLogicalProjectionExpression(projected, scanQ)
	projRef := expressions.InitialOf(proj)
	projRef.Insert(makeFakePlanWrapper(recType))
	projQ := expressions.ForEachQuantifier(projRef)

	distinct := expressions.NewLogicalDistinctExpression(projQ)
	return expressions.InitialOf(distinct)
}

// buildDistinctOverScan creates:
//
//	Distinct(Scan([recType]))
//
// and returns the Distinct Reference with a physical FinalMember
// in the inner (scan) Reference.
func buildDistinctOverScan(recType string) *expressions.Reference {
	scan := expressions.NewFullUnorderedScanExpression([]string{recType}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanRef.Insert(makeFakePlanWrapper(recType))
	scanQ := expressions.ForEachQuantifier(scanRef)

	distinct := expressions.NewLogicalDistinctExpression(scanQ)
	return expressions.InitialOf(distinct)
}

// TestDistinctFinal_PKProjected_Eliminates verifies DISTINCT elimination
// during PLANNING when the projection includes all PK columns.
func TestDistinctFinal_PKProjected_Eliminates(t *testing.T) {
	t.Parallel()
	distinctRef := buildDistinctOverProjection("USERS", []values.Value{
		&values.FieldValue{Field: "ID", Typ: values.UnknownType},
		&values.FieldValue{Field: "NAME", Typ: values.UnknownType},
	})
	ctx := &pkPlanContext{pk: map[string][]string{"USERS": {"ID"}}}
	results := FireImplementationRuleWithContext(NewImplementDistinctFinalRule(), distinctRef, ctx, nil)
	if len(results) == 0 {
		t.Fatal("ImplementDistinctFinalRule should fire and eliminate DISTINCT when PK is projected")
	}
	for _, r := range results {
		if _, ok := r.(*plans.RecordQueryDistinctPlan); ok {
			t.Fatal("expected elimination (no DistinctWrapper), but got DistinctWrapper")
		}
	}
}

// TestDistinctFinal_NonPKProjected_Wraps verifies DISTINCT is kept
// when the projection does NOT include the PK.
func TestDistinctFinal_NonPKProjected_Wraps(t *testing.T) {
	t.Parallel()
	distinctRef := buildDistinctOverProjection("USERS", []values.Value{
		&values.FieldValue{Field: "NAME", Typ: values.UnknownType},
	})
	ctx := &pkPlanContext{pk: map[string][]string{"USERS": {"ID"}}}
	results := FireImplementationRuleWithContext(NewImplementDistinctFinalRule(), distinctRef, ctx, nil)
	if len(results) == 0 {
		t.Fatal("ImplementDistinctFinalRule should fire")
	}
	foundDistinct := false
	for _, r := range results {
		if _, ok := r.(*plans.RecordQueryDistinctPlan); ok {
			foundDistinct = true
		}
	}
	if !foundDistinct {
		t.Fatal("expected DistinctWrapper when PK is not projected")
	}
}

// TestDistinctFinal_FullScan_Eliminates verifies DISTINCT elimination
// on a full table scan (no projection). Every column is available,
// so the PK is always covered.
func TestDistinctFinal_FullScan_Eliminates(t *testing.T) {
	t.Parallel()
	distinctRef := buildDistinctOverScan("USERS")
	ctx := &pkPlanContext{pk: map[string][]string{"USERS": {"ID"}}}
	results := FireImplementationRuleWithContext(NewImplementDistinctFinalRule(), distinctRef, ctx, nil)
	if len(results) == 0 {
		t.Fatal("ImplementDistinctFinalRule should fire on full scan with PK")
	}
	for _, r := range results {
		if _, ok := r.(*plans.RecordQueryDistinctPlan); ok {
			t.Fatal("expected elimination (no DistinctWrapper), but got DistinctWrapper")
		}
	}
}

// TestDistinctFinal_NoPlanContext_Wraps verifies DISTINCT is kept
// when no PlanContext provides PK info.
func TestDistinctFinal_NoPlanContext_Wraps(t *testing.T) {
	t.Parallel()
	distinctRef := buildDistinctOverScan("USERS")
	results := FireImplementationRule(NewImplementDistinctFinalRule(), distinctRef)
	if len(results) == 0 {
		t.Fatal("ImplementDistinctFinalRule should fire")
	}
	foundDistinct := false
	for _, r := range results {
		if _, ok := r.(*plans.RecordQueryDistinctPlan); ok {
			foundDistinct = true
		}
	}
	if !foundDistinct {
		t.Fatal("expected DistinctWrapper when no PK info available")
	}
}

// TestDistinctFinal_CompositePK_Eliminates verifies DISTINCT elimination
// when all columns of a composite PK are projected.
func TestDistinctFinal_CompositePK_Eliminates(t *testing.T) {
	t.Parallel()
	distinctRef := buildDistinctOverProjection("ORDER_ITEMS", []values.Value{
		&values.FieldValue{Field: "ORDER_ID", Typ: values.UnknownType},
		&values.FieldValue{Field: "ITEM_ID", Typ: values.UnknownType},
		&values.FieldValue{Field: "QTY", Typ: values.UnknownType},
	})
	ctx := &pkPlanContext{pk: map[string][]string{"ORDER_ITEMS": {"ORDER_ID", "ITEM_ID"}}}
	results := FireImplementationRuleWithContext(NewImplementDistinctFinalRule(), distinctRef, ctx, nil)
	if len(results) == 0 {
		t.Fatal("ImplementDistinctFinalRule should eliminate when all composite PK cols projected")
	}
	for _, r := range results {
		if _, ok := r.(*plans.RecordQueryDistinctPlan); ok {
			t.Fatal("expected elimination (no DistinctWrapper), but got DistinctWrapper")
		}
	}
}

// TestDistinctFinal_CompositePKPartial_Wraps verifies DISTINCT is
// kept when only some columns of a composite PK are projected.
func TestDistinctFinal_CompositePKPartial_Wraps(t *testing.T) {
	t.Parallel()
	distinctRef := buildDistinctOverProjection("ORDER_ITEMS", []values.Value{
		&values.FieldValue{Field: "ORDER_ID", Typ: values.UnknownType},
		&values.FieldValue{Field: "QTY", Typ: values.UnknownType},
	})
	ctx := &pkPlanContext{pk: map[string][]string{"ORDER_ITEMS": {"ORDER_ID", "ITEM_ID"}}}
	results := FireImplementationRuleWithContext(NewImplementDistinctFinalRule(), distinctRef, ctx, nil)
	if len(results) == 0 {
		t.Fatal("ImplementDistinctFinalRule should fire")
	}
	foundDistinct := false
	for _, r := range results {
		if _, ok := r.(*plans.RecordQueryDistinctPlan); ok {
			foundDistinct = true
		}
	}
	if !foundDistinct {
		t.Fatal("expected DistinctWrapper when composite PK is partial")
	}
}

// TestDistinctFinal_ComputedPKExpr_Wraps verifies that a projection whose only
// reference to the PK is BURIED inside an expression (id/3) does NOT elide the
// DISTINCT: f(pk) is many-to-one, so eliding dedup would emit duplicates.
// Regression for the buried-FieldValue coverage bug (SELECT DISTINCT id/3 over
// ids 1..6 returned 6 rows instead of 3).
func TestDistinctFinal_ComputedPKExpr_Wraps(t *testing.T) {
	t.Parallel()
	// id / 3 — the PK column ID appears only as an ArithmeticValue operand.
	idOver3 := &values.ArithmeticValue{
		Op:    values.OpDiv,
		Left:  &values.FieldValue{Field: "ID", Typ: values.UnknownType},
		Right: &values.ConstantValue{Value: int64(3), Typ: values.UnknownType},
	}
	distinctRef := buildDistinctOverProjection("USERS", []values.Value{idOver3})
	ctx := &pkPlanContext{pk: map[string][]string{"USERS": {"ID"}}}
	results := FireImplementationRuleWithContext(NewImplementDistinctFinalRule(), distinctRef, ctx, nil)
	if len(results) == 0 {
		t.Fatal("ImplementDistinctFinalRule should fire")
	}
	foundDistinct := false
	for _, r := range results {
		if _, ok := r.(*plans.RecordQueryDistinctPlan); ok {
			foundDistinct = true
		}
	}
	if !foundDistinct {
		t.Fatal("expected DistinctWrapper: id/3 is not injective over the PK, so DISTINCT must NOT be elided")
	}
}

// TestCollectProjectedFieldNames_BuriedFieldNotCredited is a white-box pin on
// the coverage helper: a bare column reference IS credited; the same column
// buried inside an arithmetic expression is NOT.
func TestCollectProjectedFieldNames_BuriedFieldNotCredited(t *testing.T) {
	t.Parallel()

	bareID := &values.FieldValue{Field: "ID", Typ: values.UnknownType}
	buildProj := func(v values.Value) *expressions.LogicalProjectionExpression {
		scan := expressions.NewFullUnorderedScanExpression([]string{"USERS"}, values.UnknownType)
		scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
		return expressions.NewLogicalProjectionExpression([]values.Value{v}, scanQ)
	}

	// Bare FieldValue → credited.
	if cols := collectProjectedFieldNames(buildProj(bareID)); len(cols) != 1 {
		t.Fatalf("bare id: expected 1 credited column, got %v", cols)
	} else if _, ok := cols["ID"]; !ok {
		t.Fatalf("bare id: expected ID credited, got %v", cols)
	}

	// id / 3 → the buried ID must NOT be credited (empty covering set).
	idOver3 := &values.ArithmeticValue{
		Op:    values.OpDiv,
		Left:  bareID,
		Right: &values.ConstantValue{Value: int64(3), Typ: values.UnknownType},
	}
	if cols := collectProjectedFieldNames(buildProj(idOver3)); len(cols) != 0 {
		t.Fatalf("id/3: expected NO credited columns (buried FieldValue), got %v", cols)
	}
}

// TestDistinctFinal_CaseInsensitive verifies case-insensitive
// matching between PK column names and projected field names.
func TestDistinctFinal_CaseInsensitive(t *testing.T) {
	t.Parallel()
	distinctRef := buildDistinctOverProjection("USERS", []values.Value{
		&values.FieldValue{Field: "id", Typ: values.UnknownType},
	})
	ctx := &pkPlanContext{pk: map[string][]string{"USERS": {"ID"}}}
	results := FireImplementationRuleWithContext(NewImplementDistinctFinalRule(), distinctRef, ctx, nil)
	if len(results) == 0 {
		t.Fatal("ImplementDistinctFinalRule should fire with case-insensitive PK match")
	}
	for _, r := range results {
		if _, ok := r.(*plans.RecordQueryDistinctPlan); ok {
			t.Fatal("expected elimination (no DistinctWrapper), but got DistinctWrapper")
		}
	}
}

// TestDistinctFinal_ThroughFilter verifies DISTINCT elimination
// when a filter sits between the projection and the scan.
func TestDistinctFinal_ThroughFilter(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"USERS"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	filter := expressions.NewLogicalFilterExpression(nil, scanQ)
	filterRef := expressions.InitialOf(filter)
	filterQ := expressions.ForEachQuantifier(filterRef)

	proj := expressions.NewLogicalProjectionExpression(
		[]values.Value{
			&values.FieldValue{Field: "ID", Typ: values.UnknownType},
		},
		filterQ,
	)
	projRef := expressions.InitialOf(proj)
	projRef.Insert(makeFakePlanWrapper("USERS"))
	projQ := expressions.ForEachQuantifier(projRef)

	distinct := expressions.NewLogicalDistinctExpression(projQ)
	distinctRef := expressions.InitialOf(distinct)

	ctx := &pkPlanContext{pk: map[string][]string{"USERS": {"ID"}}}
	results := FireImplementationRuleWithContext(NewImplementDistinctFinalRule(), distinctRef, ctx, nil)
	if len(results) == 0 {
		t.Fatal("ImplementDistinctFinalRule should fire through filter")
	}
	for _, r := range results {
		if _, ok := r.(*plans.RecordQueryDistinctPlan); ok {
			t.Fatal("expected elimination (no DistinctWrapper), but got DistinctWrapper")
		}
	}
}

// TestDistinctFinal_UniqueFanOutIndexDoesNotEliminate proves that UNIQUE on a
// fan-out index is uniqueness of index entries, not a base-row uniqueness key.
// In particular, multiple records may all have an empty repeated field and
// produce no index entry, so projecting that field does not make DISTINCT
// redundant. A normal unique scalar index remains a valid elimination proof.
func TestDistinctFinal_UniqueFanOutIndexDoesNotEliminate(t *testing.T) {
	t.Parallel()

	fanOut := true
	scalar := false
	scalarFanType := gen.Field_SCALAR
	newUniqueCandidate := func(name, column string, createsDuplicates *bool) MatchCandidate {
		return NewValueIndexScanMatchCandidateWithFunctions(
			name,
			[]string{"T"},
			[]string{column},
			nil,
			[]values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()},
			values.UnknownType,
			true,
			nil,
			createsDuplicates,
		)
	}
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{
		newUniqueCandidate("T$tags_unique_fanout", "TAGS", &fanOut),
		NewValueIndexScanMatchCandidateWithFunctions(
			"T$cardinality_score_unique",
			[]string{"T"},
			[]string{"SCORE"},
			[]string{FunctionKindCardinality},
			[]values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()},
			values.UnknownType,
			true,
			nil,
			&scalar,
		),
		NewValueIndexScanMatchCandidateWithFunctions(
			"T$addr_city_unique",
			[]string{"T"},
			[]string{"CITY"},
			nil,
			[]values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()},
			values.UnknownType,
			true,
			nil,
			&scalar,
		).WithRootKeyExpression(&gen.KeyExpression{Nesting: &gen.Nesting{
			Parent: &gen.Field{
				FieldName: proto.String("ADDR"),
				FanType:   &scalarFanType,
			},
			Child: candidateTestKeyField("CITY", gen.Field_SCALAR),
		}}),
		newUniqueCandidate("T$email_unique", "EMAIL", &scalar),
	}}

	assertWrapped := func(projected string) {
		t.Helper()
		results := FireImplementationRuleWithContext(
			NewImplementDistinctFinalRule(),
			buildDistinctOverProjection("T", []values.Value{
				&values.FieldValue{Field: projected, Typ: values.UnknownType},
			}),
			ctx,
			nil,
		)
		for _, result := range results {
			if _, ok := result.(*plans.RecordQueryDistinctPlan); ok {
				return
			}
		}
		t.Fatalf("DISTINCT(%s) was elided; the unique fan-out index is not a base-row uniqueness proof", projected)
	}
	assertEliminated := func(projected string) {
		t.Helper()
		results := FireImplementationRuleWithContext(
			NewImplementDistinctFinalRule(),
			buildDistinctOverProjection("T", []values.Value{
				&values.FieldValue{Field: projected, Typ: values.UnknownType},
			}),
			ctx,
			nil,
		)
		if len(results) == 0 {
			t.Fatalf("DISTINCT(%s): rule did not fire", projected)
		}
		for _, result := range results {
			if _, ok := result.(*plans.RecordQueryDistinctPlan); ok {
				t.Fatalf("DISTINCT(%s) was retained; the unique scalar index should prove it redundant", projected)
			}
		}
	}

	assertWrapped("TAGS")
	assertWrapped("SCORE")
	assertWrapped("CITY")
	assertEliminated("EMAIL")
}

// TestDistinctFinal_WrapsAllMembers verifies the wrapping path
// yields a DistinctWrapper for EVERY physical member, not just
// the first. Regression test for the early-return bug.
func TestDistinctFinal_WrapsAllMembers(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"ITEMS"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	// Insert TWO physical members to simulate multiple candidates.
	scanRef.Insert(makeFakePlanWrapper("ITEMS"))
	fwd := plans.NewRecordQueryScanPlan([]string{"ITEMS"}, values.UnknownType, false)
	rev := plans.NewRecordQueryScanPlan([]string{"ITEMS"}, values.UnknownType, true)
	scanRef.Insert(fwd)
	scanRef.Insert(rev)
	scanQ := expressions.ForEachQuantifier(scanRef)

	// Project a non-PK column so elimination does NOT fire.
	proj := expressions.NewLogicalProjectionExpression(
		[]values.Value{
			&values.FieldValue{Field: "NAME", Typ: values.UnknownType},
		},
		scanQ,
	)
	projRef := expressions.InitialOf(proj)
	// Copy members to projRef so the rule has plans to wrap.
	for _, m := range scanRef.Members() {
		projRef.Insert(m)
	}
	projQ := expressions.ForEachQuantifier(projRef)

	distinct := expressions.NewLogicalDistinctExpression(projQ)
	distinctRef := expressions.InitialOf(distinct)

	// PK is "ID" but projection only has "NAME" → no elimination.
	ctx := &pkPlanContext{pk: map[string][]string{"ITEMS": {"ID"}}}
	results := FireImplementationRuleWithContext(NewImplementDistinctFinalRule(), distinctRef, ctx, nil)

	wrapCount := 0
	for _, r := range results {
		if _, ok := r.(*plans.RecordQueryDistinctPlan); ok {
			wrapCount++
		}
	}
	if wrapCount < 2 {
		t.Fatalf("expected at least 2 DistinctWrappers (one per FinalMember), got %d", wrapCount)
	}
}

// TestNewPhysicalDistinctFor_FreezesStreamingInner pins the constraint-preserving
// disentangle at the heart of the RFC-184 W2 distinct-wrapper collapse: the
// bare distinct plan carries ONE child edge, and WHICH edge is conditional on
// the streaming mode.
//
// A STREAMING distinct is sound only over the exact ordering its flag was
// computed for; if its inner floated to a cost-tied but differently-ordered
// sibling it would run the adjacent-dedup executor over unordered input and
// LEAK a duplicate. So a Streaming=true distinct must FREEZE its inner in a
// detached single-member FINAL reference — never a live exploratory edge that
// a later winner selection could swap. A PLAIN (hash) distinct dedups over any
// order, so it carries the LIVE exploratory edge (a frozen snapshot would
// instead strand a pre-push member once a push rule re-explores the leg).
func TestNewPhysicalDistinctFor_FreezesStreamingInner(t *testing.T) {
	t.Parallel()

	call := &ImplementationRuleCall{}

	// ORDERED member: an in-memory sort on G over an index whose result type
	// carries G, so the whole-row dedup key {G} is adjacent → Streaming=true.
	gRec := values.NewRecordType("", false, []values.Field{
		{Name: "G", FieldType: values.NullableLong, Ordinal: 0},
	})
	indexPlan := plans.NewRecordQueryIndexPlan("idx_g", nil, []string{"T"}, gRec, false)
	sortKeys := []plans.SortKey{{
		Field:      "G",
		NullsFirst: true,
		ValueExpr:  values.NewFieldValueWithResolvedOrdinal("G", 0, values.NullableLong),
	}}
	// Since RFC-184 W2 the bare in-memory sort IS its own physical member (no
	// physicalInMemorySortWrapper), so it doubles as the ordered member here.
	sortPlan := plans.NewRecordQueryInMemorySortPlan(indexPlan, sortKeys)
	orderedMember := sortPlan

	if !distinctStreamingEligible(orderedMember, sortPlan) {
		t.Fatal("precondition: the ordered member must be streaming-eligible")
	}
	got := newPhysicalDistinctFor(call, orderedMember)
	dp, ok := got.(*plans.RecordQueryDistinctPlan)
	if !ok {
		t.Fatalf("newPhysicalDistinctFor = %T, want *plans.RecordQueryDistinctPlan", got)
	}
	if !dp.Streaming {
		t.Fatal("streaming-eligible member must yield Streaming=true")
	}
	innerRef := dp.GetInnerQuantifier().GetRangesOver()
	// FROZEN: the inner is a detached single-member FINAL reference over the
	// exact ordered plan — nothing can grow or swap it to an unordered sibling.
	if len(innerRef.FinalMembers()) != 1 {
		t.Fatalf("a streaming distinct must FREEZE its inner in a single-member final reference, got %d final members", len(innerRef.FinalMembers()))
	}
	if dp.GetInner() != sortPlan {
		t.Fatalf("the frozen inner must resolve to the exact ordered plan; got %T", dp.GetInner())
	}

	// PLAIN member: a bare primary scan (no adjacent dedup key) → Streaming=false.
	plainMember := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	if distinctStreamingEligible(plainMember, plainMember) {
		t.Fatal("precondition: the plain member must NOT be streaming-eligible")
	}
	gotPlain := newPhysicalDistinctFor(call, plainMember)
	dpPlain, ok := gotPlain.(*plans.RecordQueryDistinctPlan)
	if !ok {
		t.Fatalf("newPhysicalDistinctFor = %T, want *plans.RecordQueryDistinctPlan", gotPlain)
	}
	if dpPlain.Streaming {
		t.Fatal("non-streaming-eligible member must yield Streaming=false")
	}
	plainInnerRef := dpPlain.GetInnerQuantifier().GetRangesOver()
	// LIVE: the plain inner is the exploratory edge (no final members), so a
	// later push-rule canonicalization of the leg stays reachable.
	if len(plainInnerRef.FinalMembers()) != 0 {
		t.Fatalf("a plain distinct must carry the LIVE exploratory edge (no frozen final members), got %d", len(plainInnerRef.FinalMembers()))
	}
	if len(plainInnerRef.Members()) == 0 {
		t.Fatal("the plain distinct's live edge must hold the member as an exploratory member")
	}
	if dpPlain.GetInner() != plainMember {
		t.Fatalf("the plain inner must resolve to the member's plan; got %T", dpPlain.GetInner())
	}
}
