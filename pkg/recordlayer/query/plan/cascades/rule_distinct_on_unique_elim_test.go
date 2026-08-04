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
// distinctScanLayouts is the declared column order of each record type these
// tests scan. A FullUnorderedScanExpression flows this type in production, and
// the DISTINCT-elimination proof is stated in it: the projected references'
// ordinals index it, and the metadata key columns are resolved into it once
// (RFC-197). A scan of UnknownType states no layout and the proof fails closed,
// so a fixture that left the type unknown would exercise nothing.
var distinctScanLayouts = map[string][]string{
	"USERS":       {"ID", "NAME"},
	"ORDER_ITEMS": {"ORDER_ID", "ITEM_ID", "QTY"},
	"ITEMS":       {"ID", "NAME"},
	"T":           {"TAGS", "SCORE", "CITY", "EMAIL"},
	"FLOAT_PK":    {"ID", "PAD"},
	"DOUBLE_PK":   {"ID", "PAD"},
}

func distinctScanType(recType string) values.Type {
	cols, known := distinctScanLayouts[recType]
	if !known {
		return values.UnknownType
	}
	fields := make([]values.Field, len(cols))
	for i, c := range cols {
		fieldType := values.NullableString
		switch c {
		case "ID", "ORDER_ID", "ITEM_ID":
			fieldType = values.NotNullLong
		case "QTY", "SCORE", "PAD":
			fieldType = values.NullableLong
		}
		if recType == "FLOAT_PK" && c == "ID" {
			fieldType = values.NotNullFloat
		}
		if recType == "DOUBLE_PK" && c == "ID" {
			fieldType = values.NotNullDouble
		}
		fields[i] = values.Field{Name: c, FieldType: fieldType, Ordinal: i}
	}
	return &values.RecordType{Fields: fields}
}

func TestDistinctFinal_FloatingPrimaryKeyNeverEliminates(t *testing.T) {
	t.Parallel()
	for _, recType := range []string{"FLOAT_PK", "DOUBLE_PK"} {
		recType := recType
		t.Run(recType, func(t *testing.T) {
			t.Parallel()
			ctx := &pkPlanContext{pk: map[string][]string{recType: {"ID"}}}
			for _, distinctRef := range []*expressions.Reference{
				buildDistinctOverProjection(recType, []values.Value{
					distinctRead(recType, "ID"),
					distinctRead(recType, "PAD"),
				}),
				buildDistinctOverScan(recType),
			} {
				results := FireImplementationRuleWithContext(
					NewImplementDistinctFinalRule(), distinctRef, ctx, nil,
				)
				if len(results) == 0 {
					t.Fatal("ImplementDistinctFinalRule should fire")
				}
				foundDistinct := false
				for _, result := range results {
					if _, ok := result.(*plans.RecordQueryDistinctPlan); ok {
						foundDistinct = true
					}
				}
				if !foundDistinct {
					t.Fatal("raw NaN primary-key variants require logical row DISTINCT")
				}
			}
		})
	}
}

// distinctRead is a projected BARE column reference, baked at the column's
// ordinal in the scan row's layout — the shape the resolver produces
// (expr.go:296-297). Case-insensitive, like every resolution path in the engine.
func distinctRead(recType, col string) values.Value {
	id, ok := values.OrdinalOfNameIn(distinctScanType(recType), col)
	if !ok {
		panic("distinctRead: " + recType + " declares no column " + col)
	}
	return values.NewFieldValueWithResolvedOrdinalInDomain(
		col, id.Ordinal, values.UnknownType, id.Domain)
}

func buildDistinctOverProjection(
	recType string,
	projected []values.Value,
) *expressions.Reference {
	scan := expressions.NewFullUnorderedScanExpression([]string{recType}, distinctScanType(recType))
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
	scan := expressions.NewFullUnorderedScanExpression([]string{recType}, distinctScanType(recType))
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
		distinctRead("USERS", "ID"),
		distinctRead("USERS", "NAME"),
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
		distinctRead("USERS", "NAME"),
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
		distinctRead("ORDER_ITEMS", "ORDER_ID"),
		distinctRead("ORDER_ITEMS", "ITEM_ID"),
		distinctRead("ORDER_ITEMS", "QTY"),
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
		distinctRead("ORDER_ITEMS", "ORDER_ID"),
		distinctRead("ORDER_ITEMS", "QTY"),
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

// TestCollectProjectedOrdinals_BuriedFieldNotCredited is a white-box pin on the
// coverage helper: a bare column reference IS credited, at its ORDINAL in the
// scan row's layout; the same column buried inside an arithmetic expression is
// NOT; and a reference that cannot state an ordinal in THAT layout makes the
// whole set unstatable rather than being credited by the name it renders.
func TestCollectProjectedOrdinals_BuriedFieldNotCredited(t *testing.T) {
	t.Parallel()

	layoutType := distinctScanType("USERS")
	layout := values.OrdinalDomainOfType(layoutType)
	bareID := distinctRead("USERS", "ID")
	buildProj := func(v values.Value) *expressions.LogicalProjectionExpression {
		scan := expressions.NewFullUnorderedScanExpression([]string{"USERS"}, layoutType)
		scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
		return expressions.NewLogicalProjectionExpression([]values.Value{v}, scanQ)
	}

	// Bare baked FieldValue → credited at its ordinal.
	ords, ok := collectProjectedOrdinals(buildProj(bareID), layout)
	if !ok || len(ords) != 1 {
		t.Fatalf("bare id: expected 1 credited ordinal, got %v ok=%v", ords, ok)
	}
	if _, credited := ords[0]; !credited {
		t.Fatalf("bare id: expected ordinal 0 credited, got %v", ords)
	}

	// id / 3 → the buried ID must NOT be credited (empty covering set).
	idOver3 := &values.ArithmeticValue{
		Op:    values.OpDiv,
		Left:  bareID,
		Right: &values.ConstantValue{Value: int64(3), Typ: values.UnknownType},
	}
	if ords, ok := collectProjectedOrdinals(buildProj(idOver3), layout); !ok || len(ords) != 0 {
		t.Fatalf("id/3: expected NO credited ordinals (buried FieldValue), got %v ok=%v", ords, ok)
	}

	// A LAZY reference states no ordinal in this layout. The set becomes
	// UNSTATABLE rather than crediting the column its display name spells —
	// crediting it is how a projection of some other source's same-named column
	// would be counted as covering this one's key (RFC-197).
	lazyID := &values.FieldValue{Field: "ID", Typ: values.UnknownType}
	if ords, ok := collectProjectedOrdinals(buildProj(lazyID), layout); ok {
		t.Fatalf("a lazy reference named ID was credited as covering the layout's ID column: "+
			"got %v", ords)
	}

	// So is a reference baked against a DIFFERENT layout, even at the same
	// ordinal under the same name — the DOMAIN is what refuses, and only a case
	// holding the name AND the ordinal equal can show it.
	foreign := values.NewFieldValueWithResolvedOrdinalInDomain(
		"ID", 0, values.UnknownType,
		values.OrdinalDomainOfColumnNames([]string{"ID", "SOMETHING_ELSE"}))
	if ords, ok := collectProjectedOrdinals(buildProj(foreign), layout); ok {
		t.Fatalf("a reference baked in ANOTHER layout was credited: got %v", ords)
	}
}

// TestDistinctFinal_CaseInsensitive verifies case-insensitive
// matching between PK column names and projected field names.
func TestDistinctFinal_CaseInsensitive(t *testing.T) {
	t.Parallel()
	distinctRef := buildDistinctOverProjection("USERS", []values.Value{
		distinctRead("USERS", "id"),
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

	scan := expressions.NewFullUnorderedScanExpression([]string{"USERS"}, distinctScanType("USERS"))
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	filter := expressions.NewLogicalFilterExpression(nil, scanQ)
	filterRef := expressions.InitialOf(filter)
	filterQ := expressions.ForEachQuantifier(filterRef)

	proj := expressions.NewLogicalProjectionExpression(
		[]values.Value{
			distinctRead("USERS", "ID"),
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

// TestDistinctFinal_SecondaryUniqueIndexIsNeverAnEliminationProof pins that NO
// secondary index's `unique` flag makes a DISTINCT redundant — not a fan-out
// one, and not a perfectly ordinary scalar one either.
//
// The fan-out cases (TAGS, SCORE, CITY) fail for a second, independent reason:
// UNIQUE on a fan-out index constrains index ENTRIES, and records with an empty
// repeated field produce no entry at all, so the key says nothing about base-row
// distinctness. They are kept because that reason must survive on its own.
//
// EMAIL — a plain, scalar, non-fan-out unique index — is the case this test
// exists for. It USED to be elided, and that elision returned duplicate rows
// against a real store: a unique index whose build completes over violating data
// lands in READABLE_UNIQUE_PENDING, keeping its `unique` flag while the data
// contradicts it. Go's match candidates are built from metadata alone and carry
// no index state, so the planner cannot tell the two apart; Java can only trust
// the flag because MetaDataPlanContext filters candidates through
// RecordStoreState::isReadable first (MetaDataPlanContext.java:96-103,
// RecordStoreState.java:172-174), which excludes that state from planning.
//
// The elision cannot be resurrected by state-filtering the candidate set alone:
// see TestFDB_UniquePendingIndexDoesNotEliminateDistinct for the end-to-end
// witness, and note it fires on a plan that never reads the pending index, so
// no executor leaf check can substitute.
//
// The PK case is the positive control. Without it this test would still pass
// with the whole rule inert, since "no elision" is what a dead rule produces.
func TestDistinctFinal_SecondaryUniqueIndexIsNeverAnEliminationProof(t *testing.T) {
	t.Parallel()

	fanOut := true
	scalar := false
	scalarFanType := gen.Field_SCALAR
	newUniqueCandidate := func(name, column string, createsDuplicates *bool) *ValueIndexScanMatchCandidate {
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
		).WithKeyComponentTypes([]values.Type{values.NotNullString})
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
		).WithKeyComponentTypes([]values.Type{values.NotNullLong}),
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
		).WithKeyComponentTypes([]values.Type{values.NotNullString}).
			WithRootKeyExpression(&gen.KeyExpression{Nesting: &gen.Nesting{
				Parent: &gen.Field{
					FieldName: proto.String("ADDR"),
					FanType:   &scalarFanType,
				},
				Child: candidateTestKeyField("CITY", gen.Field_SCALAR),
			}}),
		newUniqueCandidate("T$email_unique", "EMAIL", &scalar),
	}}

	fire := func(planCtx PlanContext, projected string) []expressions.RelationalExpression {
		t.Helper()
		return FireImplementationRuleWithContext(
			NewImplementDistinctFinalRule(),
			buildDistinctOverProjection("T", []values.Value{
				distinctRead("T", projected),
			}),
			planCtx,
			nil,
		)
	}
	assertWrapped := func(projected, why string) {
		t.Helper()
		for _, result := range fire(ctx, projected) {
			if _, ok := result.(*plans.RecordQueryDistinctPlan); ok {
				return
			}
		}
		t.Fatalf("DISTINCT(%s) was elided; %s", projected, why)
	}

	assertWrapped("TAGS", "UNIQUE on a fan-out index constrains index entries, "+
		"not base rows — records with an empty repeated field produce no entry at all")
	assertWrapped("SCORE", "a CARDINALITY()-keyed unique index keys a derived "+
		"value, not the projected column")
	assertWrapped("CITY", "a unique index nested under a repeated parent is "+
		"fan-out, whatever the leaf's fan type says")
	assertWrapped("EMAIL", "a secondary index's `unique` flag is a metadata "+
		"INTENT, not a statement about the stored rows — a READABLE_UNIQUE_PENDING "+
		"index carries that flag over data that violates it")

	// Positive control: the SAME projection over the SAME layout IS elided when
	// EMAIL is the primary key rather than a unique index. Primary-key
	// uniqueness is a storage invariant with no pending state to diverge into.
	// This is also what keeps the four assertions above honest — an inert rule
	// would satisfy every one of them.
	pkCtx := &pkPlanContext{pk: map[string][]string{"T": {"EMAIL"}}}
	pkResults := fire(pkCtx, "EMAIL")
	if len(pkResults) == 0 {
		t.Fatal("DISTINCT(EMAIL) over a PK-covering projection: rule did not fire at all")
	}
	for _, result := range pkResults {
		if _, ok := result.(*plans.RecordQueryDistinctPlan); ok {
			t.Fatal("DISTINCT(EMAIL) was retained even though EMAIL is the PRIMARY KEY — " +
				"primary-key coverage is still a valid elimination proof, and if it " +
				"stopped being one the four assertions above would prove nothing")
		}
	}
}

func TestDistinctFinal_MultiTypeVisiblePrimaryKeyDoesNotEliminate(t *testing.T) {
	t.Parallel()
	// Physical keys include a record-type discriminator, so A/1 and B/1 are
	// distinct records but collide after projecting the visible ID column.
	layout := values.NewRecordType("AB", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	domain := values.OrdinalDomainOfType(layout)
	id := values.NewFieldValueWithResolvedOrdinalInDomain(
		"ID", 0, values.NotNullLong, domain,
	)
	scan := expressions.NewFullUnorderedScanExpression([]string{"A", "B"}, layout)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	projection := expressions.NewLogicalProjectionExpression([]values.Value{id}, scanQ)
	projectionRef := expressions.InitialOf(projection)
	projectionRef.Insert(plans.NewRecordQueryScanPlan([]string{"A", "B"}, layout, false))
	distinct := expressions.NewLogicalDistinctExpression(
		expressions.ForEachQuantifier(projectionRef),
	)
	ctx := &pkPlanContext{pk: map[string][]string{
		"A": {"ID"},
		"B": {"ID"},
	}}
	results := FireImplementationRuleWithContext(
		NewImplementDistinctFinalRule(), expressions.InitialOf(distinct), ctx, nil,
	)
	for _, result := range results {
		if _, ok := result.(*plans.RecordQueryDistinctPlan); ok {
			return
		}
	}
	t.Fatal("DISTINCT(ID) was elided over a multi-type stream whose visible primary keys can collide")
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
			distinctRead("ITEMS", "NAME"),
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

// The DISTINCT elision has TWO arms, and until now only one had unit coverage.
// The other — the PLAN-PARTITION arm, which absorbs the operator when the
// partition already produces distinct RECORDS — was caught solely by an
// end-to-end FDB test, so the cascades package alone stayed green when its
// guard was removed. A package that can be made wrong while its own suite
// passes is a package with a hole in it.
//
// Record distinctness is distinctness of stored RECORDS. That is a stand-in for
// SQL row distinctness only while record identity agrees with logical equality,
// and a raw NaN float coordinate in the primary key breaks the agreement: two
// physically distinct keys, one logical row.
//
// Both arms are needed here for the same reason as elsewhere — the LONG control
// is what distinguishes a filter from an off switch.
func TestDistinctFinal_PartitionDistinctnessNeedsLogicalEquality(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		pkType     values.Type
		wantElided bool
	}{
		{name: "DOUBLE primary key", pkType: values.NotNullDouble},
		{name: "LONG primary key", pkType: values.NotNullLong, wantElided: true},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
				WithPrimaryKey([]values.Value{
					values.NewFieldValueWithResolvedOrdinal("PK", 0, test.pkType),
				}).
				WithKeyComponentTypes([]values.Type{test.pkType})
			innerRef := expressions.InitialOf(scan)
			computeRefPlanProperties(innerRef)
			distinctExpr := expressions.NewLogicalDistinctExpression(
				expressions.ForEachQuantifier(innerRef),
			)
			results := FireImplementationRule(
				NewImplementDistinctFinalRule(), expressions.InitialOf(distinctExpr),
			)
			if len(results) == 0 {
				t.Fatal("ImplementDistinctFinalRule should fire over a stored-record partition")
			}
			elided := true
			for _, result := range results {
				if _, ok := result.(*plans.RecordQueryDistinctPlan); ok {
					elided = false
				}
			}
			if elided != test.wantElided {
				if test.wantElided {
					t.Fatal("a comparator-congruent primary key was denied the elision — " +
						"the guard is an off switch, not a filter, and the float arm " +
						"above now proves nothing")
				}
				t.Fatal("record distinctness absorbed DISTINCT over a raw NaN primary key, " +
					"where two distinct records are ONE logical row")
			}
		})
	}
}
