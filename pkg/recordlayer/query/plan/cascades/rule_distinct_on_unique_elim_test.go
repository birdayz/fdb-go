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
	cfg := DefaultPlannerConfiguration()
	// A unit fixture EXECUTES nothing and PAGES nowhere, so "the whole result
	// comes from one read version" is true of it by construction. Stating it
	// keeps the single-read-version gate from silently disabling every proof
	// here, without weakening the gate: the condition really does hold.
	cfg.SingleReadVersion = true
	return cfg
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
	return makeFakePlanWrapperForType(recType, distinctScanType(recType), false)
}

func makeFakePlanWrapperForType(
	recType string, flowedType values.Type, reverse bool,
) *plans.RecordQueryScanPlan {
	return mustDistinctConstruct(plans.NewRecordQueryScanPlan(
		[]string{recType}, flowedType, reverse))
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
	"T":           {"TAGS", "SCORE", "CITY", "EMAIL", "SPARSE_EMAIL", "NULLABLE_EMAIL", "DBL", "A", "B", "SHARED"},
	"FLOAT_PK":    {"ID", "PAD"},
	"DOUBLE_PK":   {"ID", "PAD"},
}

func distinctScanType(recType string) values.Type {
	cols, known := distinctScanLayouts[recType]
	if !known {
		panic("distinctScanType: no exact layout for " + recType)
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
				results := mustFireImplementationRuleWithContext(t,
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
	layout := distinctScanType(recType)
	id, ok := values.OrdinalOfNameIn(layout, col)
	if !ok {
		panic("distinctRead: " + recType + " declares no column " + col)
	}
	root := mustDistinctConstruct(values.NewQuantifiedObjectValue(
		distinctReadAlias(recType), layout))
	return mustDistinctConstruct(values.ResolveFieldOrdinals(root, []int{id.Ordinal}))
}

// distinctReadAlias is the binding used by the compact column fixture above.
// A fresh QOV is still exact and immutable; sharing this semantic alias lets a
// caller construct its projected values before it wires the scan quantifier.
func distinctReadAlias(recType string) values.CorrelationIdentifier {
	return values.NamedCorrelationIdentifier("distinct_read_" + recType)
}

func buildDistinctOverProjection(
	recType string,
	projected []values.Value,
) *expressions.Reference {
	scan := mustDistinctConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{recType}, distinctScanType(recType)))
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.NamedForEachQuantifier(distinctReadAlias(recType), scanRef)

	proj := mustDistinctConstruct(expressions.NewLogicalProjectionExpression(projected, scanQ))
	projRef := expressions.InitialOf(proj)
	projRef.Insert(makeFakePlanWrapperForType(recType, proj.GetResultValue().Type(), false))
	projQ := expressions.ForEachQuantifier(projRef)

	distinct := mustDistinctConstruct(expressions.NewLogicalDistinctExpression(projQ))
	return expressions.InitialOf(distinct)
}

// buildDistinctOverScan creates:
//
//	Distinct(Scan([recType]))
//
// and returns the Distinct Reference with a physical FinalMember
// in the inner (scan) Reference.
func buildDistinctOverScan(recType string) *expressions.Reference {
	scan := mustDistinctConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{recType}, distinctScanType(recType)))
	scanRef := expressions.InitialOf(scan)
	scanRef.Insert(makeFakePlanWrapper(recType))
	scanQ := expressions.ForEachQuantifier(scanRef)

	distinct := mustDistinctConstruct(expressions.NewLogicalDistinctExpression(scanQ))
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
	results := mustFireImplementationRuleWithContext(t, NewImplementDistinctFinalRule(), distinctRef, ctx, nil)
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
	results := mustFireImplementationRuleWithContext(t, NewImplementDistinctFinalRule(), distinctRef, ctx, nil)
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
	results := mustFireImplementationRuleWithContext(t, NewImplementDistinctFinalRule(), distinctRef, ctx, nil)
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
	results := mustFireImplementationRule(t, NewImplementDistinctFinalRule(), distinctRef)
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
	results := mustFireImplementationRuleWithContext(t, NewImplementDistinctFinalRule(), distinctRef, ctx, nil)
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
	results := mustFireImplementationRuleWithContext(t, NewImplementDistinctFinalRule(), distinctRef, ctx, nil)
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
		Left:  distinctRead("USERS", "ID"),
		Right: &values.ConstantValue{Value: int64(3), Typ: values.NotNullLong},
	}
	distinctRef := buildDistinctOverProjection("USERS", []values.Value{idOver3})
	ctx := &pkPlanContext{pk: map[string][]string{"USERS": {"ID"}}}
	results := mustFireImplementationRuleWithContext(t, NewImplementDistinctFinalRule(), distinctRef, ctx, nil)
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
		scan := mustDistinctConstruct(expressions.NewFullUnorderedScanExpression(
			[]string{"USERS"}, layoutType))
		scanQ := expressions.NamedForEachQuantifier(
			distinctReadAlias("USERS"), expressions.InitialOf(scan))
		return mustDistinctConstruct(expressions.NewLogicalProjectionExpression(
			[]values.Value{v}, scanQ))
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
		Right: &values.ConstantValue{Value: int64(3), Typ: values.NotNullLong},
	}
	if ords, ok := collectProjectedOrdinals(buildProj(idOver3), layout); !ok || len(ords) != 0 {
		t.Fatalf("id/3: expected NO credited ordinals (buried FieldValue), got %v ok=%v", ords, ok)
	}

	// RFC-232 makes the former LAZY-reference fixture unrepresentable: a QOV
	// with an unresolved root cannot cross the admission boundary, so no public
	// API can publish the childless/name-only FieldValue this arm used to build.
	if unresolved, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("lazy_ID"), values.UnknownType,
	); err == nil || unresolved != nil {
		t.Fatalf("an unresolved QOV was admitted: value=%v err=%v", unresolved, err)
	}

	// So is a reference baked against a DIFFERENT layout, even at the same
	// ordinal under the same name — the DOMAIN is what refuses, and only a case
	// holding the name AND the ordinal equal can show it.
	foreignType := values.NewRecordType("Foreign", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "SOMETHING_ELSE", FieldType: values.NullableString},
	})
	foreignRoot := mustDistinctConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("foreign_ID"), foreignType))
	foreign := mustDistinctConstruct(values.ResolveFieldOrdinals(foreignRoot, []int{0}))
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
	results := mustFireImplementationRuleWithContext(t, NewImplementDistinctFinalRule(), distinctRef, ctx, nil)
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

	scan := mustDistinctConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"USERS"}, distinctScanType("USERS")))
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	filter := mustDistinctConstruct(expressions.NewLogicalFilterExpression(nil, scanQ))
	filterRef := expressions.InitialOf(filter)
	filterQ := expressions.NamedForEachQuantifier(distinctReadAlias("USERS"), filterRef)

	proj := mustDistinctConstruct(expressions.NewLogicalProjectionExpression(
		[]values.Value{
			distinctRead("USERS", "ID"),
		},
		filterQ,
	))
	projRef := expressions.InitialOf(proj)
	projRef.Insert(makeFakePlanWrapperForType("USERS", proj.GetResultValue().Type(), false))
	projQ := expressions.ForEachQuantifier(projRef)

	distinct := mustDistinctConstruct(expressions.NewLogicalDistinctExpression(projQ))
	distinctRef := expressions.InitialOf(distinct)

	ctx := &pkPlanContext{pk: map[string][]string{"USERS": {"ID"}}}
	results := mustFireImplementationRuleWithContext(t, NewImplementDistinctFinalRule(), distinctRef, ctx, nil)
	if len(results) == 0 {
		t.Fatal("ImplementDistinctFinalRule should fire through filter")
	}
	for _, r := range results {
		if _, ok := r.(*plans.RecordQueryDistinctPlan); ok {
			t.Fatal("expected elimination (no DistinctWrapper), but got DistinctWrapper")
		}
	}
}

// TestDistinctFinal_SecondaryUniqueIndexAdmissionPredicate pins WHICH secondary
// UNIQUE indexes prove a DISTINCT redundant and which do not. Each arm is a
// dimension on which a wrong admission predicate silently emits duplicate rows,
// and each is refused by a different clause — a predicate built from any single
// clause reds this test on its first day.
//
//	TAGS  — clause 3. UNIQUE on a fan-out index constrains index ENTRIES, and a
//	        record whose repeated field is empty produces no entry at all, so the
//	        key says nothing about base-row distinctness. (The fixture's
//	        createsDuplicates is TRUE here.)
//	SCORE — clause 5, not clause 3: its createsDuplicates is false, and it is the
//	        CARDINALITY() function-keying that refuses it. A unique index over
//	        f(col) says nothing about col.
//	CITY  — clause 3, via the cardinality gate's traversal check. The fixture
//	        hand-supplies createsDuplicates = false, so this arm pins the rule's
//	        USE of the signal rather than the signal's derivation from the
//	        nested-repeated root key expression.
//	EMAIL — ADMITTED. A plain, scalar, NOT NULL, non-fan-out, non-sparse unique
//	        index whose single key column is the projected column. This arm
//	        INVERTED when the decline was lifted, and the plan it yields carries
//	        the proving index's name as a stamp so an index-state transition
//	        invalidates the statement instead of returning duplicates.
//
// The elision rests on MUTABLE STORE STATE, which is why the context here must
// assert index-state evidence for the EMAIL arm to fire at all. The companion
// TestDistinctFinal_SecondaryUniqueProofNeedsIndexStateEvidence pins the other
// side: the same fixture with the evidence withheld elides nothing.
//
// The PK case is the positive control on the three refusals. Without it they
// would still pass with the whole rule inert, since "no elision" is what a dead
// rule produces.
// secondaryUniqueTestCandidates is the shared fixture for the admission
// predicate's arms and for its fail-closed companion. One candidate set, two
// index-state views: the only difference between the two tests is the evidence,
// which is the point of the second one.
func secondaryUniqueTestCandidates() []MatchCandidate {
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
			distinctScanType("T"),
			true,
			nil,
			createsDuplicates,
		).WithKeyComponentTypes([]values.Type{values.NotNullString})
	}
	return []MatchCandidate{
		newUniqueCandidate("T$tags_unique_fanout", "TAGS", &fanOut),
		NewValueIndexScanMatchCandidateWithFunctions(
			"T$cardinality_score_unique",
			[]string{"T"},
			[]string{"SCORE"},
			[]string{FunctionKindCardinality},
			[]values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()},
			distinctScanType("T"),
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
			distinctScanType("T"),
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
		// Clauses 3 and 4 — and the ONLY arm that witnesses them. A SPARSE
		// (WHERE-filtered) index omits every record its stored predicate rejects,
		// so its UNIQUE declaration constrains only the rows the predicate
		// ADMITS and says nothing whatever about the excluded rows, which may
		// hold arbitrarily many duplicates of an admitted value. Eliding on that
		// proof emits them.
		//
		// The other clause-3 shapes (TAGS, CITY) are refused EARLIER, by clause
		// 5's canProduceScanPlan gate — a fan-out candidate with no traversal is
		// not a plain-field candidate either. Sparseness is the one failure
		// clause 5 does not subsume, and it is also the one whose cause is
		// invisible in the index's key definition.
		newUniqueCandidate("T$sparse_email_unique", "SPARSE_EMAIL", &scalar).
			WithPredicateProto(&gen.Predicate{ConstantPredicate: &gen.ConstantPredicate{
				Value: gen.ConstantPredicate_FALSE.Enum(),
			}}),
		// Clause 8, NULL direction. Under NULLS DISTINCT the uniqueness check is
		// SKIPPED when the component is NULL, so this index legitimately holds
		// (NULL), (NULL), (NULL) — distinguished only by their appended primary
		// keys — while SELECT DISTINCT must return ONE row.
		newUniqueCandidate("T$nullable_email_unique", "NULLABLE_EMAIL", &scalar).
			WithKeyComponentTypes([]values.Type{values.NullableString}),
		// Clause 8, FLOAT direction. FDB preserves distinct raw NaN sign and
		// payload encodings, so two tuple keys this index happily holds are ONE
		// logical value to values.CompareFloat64. Storage identity FINER than
		// logical equality means eliding emits two rows where DISTINCT emits one.
		newUniqueCandidate("T$dbl_unique", "DBL", &scalar).
			WithKeyComponentTypes([]values.Type{values.NotNullDouble}),
		// Clause 6. A composite UNIQUE (A, B) says nothing about A alone.
		NewValueIndexScanMatchCandidateWithFunctions(
			"T$ab_unique",
			[]string{"T"},
			[]string{"A", "B"},
			nil,
			[]values.CorrelationIdentifier{
				values.UniqueCorrelationIdentifier(),
				values.UniqueCorrelationIdentifier(),
			},
			distinctScanType("T"),
			true,
			nil,
			&scalar,
		).WithKeyComponentTypes([]values.Type{values.NotNullString, values.NotNullString}),
		// Clause 7, candidate side. A key unique across a multi-type candidate is
		// unique only WITHIN a type; two types' keys collide once a shared
		// visible coordinate is projected.
		NewValueIndexScanMatchCandidateWithFunctions(
			"T$multitype_unique",
			[]string{"T", "OTHER"},
			[]string{"SHARED"},
			nil,
			[]values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()},
			distinctScanType("T"),
			true,
			nil,
			&scalar,
		).WithKeyComponentTypes([]values.Type{values.NotNullString}),
	}
}

// fireDistinctOverT fires the rule for `SELECT DISTINCT <projected> FROM T`
// against the given plan context.
func fireDistinctOverT(
	t *testing.T, planCtx PlanContext, projected string,
) []expressions.RelationalExpression {
	t.Helper()
	return mustFireImplementationRuleWithContext(t,
		NewImplementDistinctFinalRule(),
		buildDistinctOverProjection("T", []values.Value{
			distinctRead("T", projected),
		}),
		planCtx,
		nil,
	)
}

func TestDistinctFinal_SecondaryUniqueIndexAdmissionPredicate(t *testing.T) {
	t.Parallel()

	ctx := &indexTestPlanContext{
		candidates: secondaryUniqueTestCandidates(),
		// This planning run consulted a store and every index came back strictly
		// READABLE. Without that evidence the secondary-UNIQUE arm declines
		// everything and the EMAIL assertion below would be vacuous.
		readableIndexes: AllIndexesReadable(),
	}

	fire := func(planCtx PlanContext, projected string) []expressions.RelationalExpression {
		t.Helper()
		return fireDistinctOverT(t, planCtx, projected)
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
	assertWrapped("SPARSE_EMAIL", "a WHERE-filtered UNIQUE index constrains only "+
		"the rows its stored predicate ADMITS — the excluded rows may hold "+
		"arbitrarily many duplicates of an admitted value")
	assertWrapped("NULLABLE_EMAIL", "under NULLS DISTINCT the uniqueness check is "+
		"skipped when the component is NULL, so the index holds (NULL),(NULL),(NULL) "+
		"and DISTINCT must collapse them to one row")
	assertWrapped("DBL", "FDB preserves distinct raw NaN encodings the index happily "+
		"holds as two keys, while the comparator canonicalizes them to one logical "+
		"value — storage identity finer than logical equality")
	assertWrapped("A", "a composite UNIQUE (A, B) constrains the PAIR; A alone may "+
		"repeat across rows with different B")
	assertWrapped("SHARED", "a key unique across two record types is unique only "+
		"WITHIN a type — the two types' keys collide once the shared coordinate "+
		"is the only thing projected")

	// EMAIL — the arm that inverted. A plain scalar NOT NULL unique index over
	// exactly the projected column proves the DISTINCT removes nothing, and the
	// yielded plan must NAME the proving index: the elision's evidence is
	// otherwise the ABSENCE of an operator, which a rule that silently died also
	// produces.
	emailResults := fire(ctx, "EMAIL")
	if len(emailResults) == 0 {
		t.Fatal("DISTINCT(EMAIL) over a scalar unique index: rule did not fire at all")
	}
	stampedBy := ""
	for _, result := range emailResults {
		if _, ok := result.(*plans.RecordQueryDistinctPlan); ok {
			t.Fatal("DISTINCT(EMAIL) was retained over T$email_unique — a scalar, " +
				"NOT NULL, non-fan-out, non-sparse UNIQUE index whose only key column " +
				"IS the projected column proves the operator removes nothing")
		}
		if stamped, ok := result.(plans.DistinctProofStamped); ok {
			if name := stamped.GetDistinctProofIndexName(); name != "" {
				stampedBy = name
			}
		}
	}
	if stampedBy != "T$email_unique" {
		t.Fatalf("the elided plan records its proving index as %q, want "+
			"\"T$email_unique\" — an unstamped elision is an elision whose index "+
			"can leave READABLE mid-statement with nothing to invalidate the plan",
			stampedBy)
	}

	// Positive control: the SAME projection over the SAME layout IS elided when
	// EMAIL is the primary key rather than a unique index. Primary-key
	// uniqueness is a storage invariant with no pending state to diverge into.
	// This is also what keeps the three refusals above honest — an inert rule
	// would satisfy every one of them. It must also NOT stamp: a primary key has
	// no index state to move, so recording a dependency on it would fail
	// statements that were correct regardless.
	pkCtx := &pkPlanContext{pk: map[string][]string{"T": {"EMAIL"}}}
	pkResults := fire(pkCtx, "EMAIL")
	if len(pkResults) == 0 {
		t.Fatal("DISTINCT(EMAIL) over a PK-covering projection: rule did not fire at all")
	}
	for _, result := range pkResults {
		if _, ok := result.(*plans.RecordQueryDistinctPlan); ok {
			t.Fatal("DISTINCT(EMAIL) was retained even though EMAIL is the PRIMARY KEY — " +
				"primary-key coverage is still a valid elimination proof, and if it " +
				"stopped being one the three refusals above would prove nothing")
		}
		if stamped, ok := result.(plans.DistinctProofStamped); ok {
			if name := stamped.GetDistinctProofIndexName(); name != "" {
				t.Fatalf("a PRIMARY-KEY elision recorded a dependency on index %q. "+
					"A primary key is a storage invariant with no state to move, so "+
					"the dependency can only turn an unrelated index build into a "+
					"40001 on a statement that was correct regardless", name)
			}
		}
	}
}

// TestDistinctFinal_SecondaryUniqueProofNeedsIndexStateEvidence is the
// fail-closed pin, and it is the reason the admission predicate demands
// AFFIRMATIVE evidence rather than the absence of contrary evidence.
//
// "It is a match candidate, therefore the store called it READABLE" is FALSE on
// every planning path that never consulted a store — the metadata-only harness,
// offline tools, and this rule unit test. Those paths leave the configuration at
// its zero value, which permits every index to back a scan and asserts nothing
// about any index's state. Implemented without this clause, the arm returned
// duplicate rows against a real store over a READABLE_UNIQUE_PENDING index.
//
// The fixture is byte-identical to the admission-predicate test's; the ONLY
// difference is the withheld evidence. So a regression that re-collapses the
// unknown and all-readable states cannot hide behind a differently-shaped
// candidate.
func TestDistinctFinal_SecondaryUniqueProofNeedsIndexStateEvidence(t *testing.T) {
	t.Parallel()

	// Zero-value readableIndexes: nobody consulted a store.
	ctx := &indexTestPlanContext{candidates: secondaryUniqueTestCandidates()}
	if ctx.GetPlannerConfiguration().ReadableIndexes.IndexStatesEstablished() {
		t.Fatal("the fixture claims to have established index state, so it cannot " +
			"pin what happens when nobody asked")
	}

	results := fireDistinctOverT(t, ctx, "EMAIL")
	if len(results) == 0 {
		t.Fatal("rule did not fire at all")
	}
	for _, result := range results {
		if _, ok := result.(*plans.RecordQueryDistinctPlan); ok {
			return
		}
	}
	t.Fatal("DISTINCT(EMAIL) was elided on a planning run that never established " +
		"index state. A run that did not ask may still SCAN an index; it may not " +
		"PROVE anything from the index's declared uniqueness, because the index " +
		"may be READABLE_UNIQUE_PENDING — fully populated and carrying a `unique` " +
		"flag the data contradicts")
}

func TestDistinctFinal_MultiTypeVisiblePrimaryKeyDoesNotEliminate(t *testing.T) {
	t.Parallel()
	// Physical keys include a record-type discriminator, so A/1 and B/1 are
	// distinct records but collide after projecting the visible ID column.
	layout := values.NewRecordType("AB", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	scan := mustDistinctConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"A", "B"}, layout))
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	scanRow := mustDistinctConstruct(scanQ.RequireFlowedObjectValue())
	id := mustDistinctConstruct(values.ResolveFieldOrdinals(scanRow, []int{0}))
	projection := mustDistinctConstruct(expressions.NewLogicalProjectionExpression(
		[]values.Value{id}, scanQ))
	projectionRef := expressions.InitialOf(projection)
	projectionRef.Insert(mustDistinctConstruct(plans.NewRecordQueryScanPlan(
		[]string{"A", "B"}, projection.GetResultValue().Type(), false)))
	distinct := mustDistinctConstruct(expressions.NewLogicalDistinctExpression(
		expressions.ForEachQuantifier(projectionRef),
	))
	ctx := &pkPlanContext{pk: map[string][]string{
		"A": {"ID"},
		"B": {"ID"},
	}}
	results := mustFireImplementationRuleWithContext(t,
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

	scan := mustDistinctConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"ITEMS"}, distinctScanType("ITEMS")))
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.NamedForEachQuantifier(distinctReadAlias("ITEMS"), scanRef)

	// Project a non-PK column so elimination does NOT fire.
	proj := mustDistinctConstruct(expressions.NewLogicalProjectionExpression(
		[]values.Value{
			distinctRead("ITEMS", "NAME"),
		},
		scanQ,
	))
	projRef := expressions.InitialOf(proj)
	// Insert TWO physical projection-shaped members to simulate multiple
	// candidates. A full-row scan is not an implementation of a one-column
	// projection and RFC-232 now rejects that memo disagreement immediately.
	projType := proj.GetResultValue().Type()
	projRef.Insert(makeFakePlanWrapperForType("ITEMS", projType, false))
	projRef.Insert(makeFakePlanWrapperForType("ITEMS", projType, true))
	projQ := expressions.ForEachQuantifier(projRef)

	distinct := mustDistinctConstruct(expressions.NewLogicalDistinctExpression(projQ))
	distinctRef := expressions.InitialOf(distinct)

	// PK is "ID" but projection only has "NAME" → no elimination.
	ctx := &pkPlanContext{pk: map[string][]string{"ITEMS": {"ID"}}}
	results := mustFireImplementationRuleWithContext(t, NewImplementDistinctFinalRule(), distinctRef, ctx, nil)

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
	indexPlan := mustDistinctConstruct(plans.NewRecordQueryIndexPlan(
		"idx_g", nil, []string{"T"}, gRec, false))
	gKey := mustDistinctConstruct(values.ResolveFieldOrdinals(
		indexPlan.GetResultValue(), []int{0}))
	sortKeys := []plans.SortKey{{
		Field:      "G",
		NullsFirst: true,
		ValueExpr:  gKey,
	}}
	// Since RFC-184 W2 the bare in-memory sort IS its own physical member (no
	// physicalInMemorySortWrapper), so it doubles as the ordered member here.
	sortPlan := mustDistinctConstruct(plans.NewRecordQueryInMemorySortPlan(indexPlan, sortKeys))
	orderedMember := sortPlan

	if !distinctStreamingEligible(orderedMember, sortPlan) {
		t.Fatal("precondition: the ordered member must be streaming-eligible")
	}
	got := mustDistinctConstruct(newPhysicalDistinctFor(call, orderedMember))
	dp, ok := got.(*plans.RecordQueryDistinctPlan)
	if !ok {
		t.Fatalf("newPhysicalDistinctFor = %T, want *plans.RecordQueryDistinctPlan", got)
	}
	if !dp.IsStreaming() {
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
	plainMember := mustDistinctConstruct(plans.NewRecordQueryScanPlan(
		[]string{"T"}, distinctScanType("T"), false))
	if distinctStreamingEligible(plainMember, plainMember) {
		t.Fatal("precondition: the plain member must NOT be streaming-eligible")
	}
	gotPlain := mustDistinctConstruct(newPhysicalDistinctFor(call, plainMember))
	dpPlain, ok := gotPlain.(*plans.RecordQueryDistinctPlan)
	if !ok {
		t.Fatalf("newPhysicalDistinctFor = %T, want *plans.RecordQueryDistinctPlan", gotPlain)
	}
	if dpPlain.IsStreaming() {
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
			rowType := values.NewRecordType("PartitionDistinctness", false, []values.Field{
				{Name: "PK", FieldType: test.pkType},
			})
			scan := mustDistinctConstruct(plans.NewRecordQueryScanPlan(
				[]string{"T"}, rowType, false))
			primaryKey := mustDistinctConstruct(values.ResolveFieldOrdinals(
				scan.GetResultValue(), []int{0}))
			scan = scan.WithPrimaryKey([]values.Value{primaryKey}).
				WithKeyComponentTypes([]values.Type{test.pkType})
			innerRef := expressions.InitialOf(scan)
			computeRefPlanProperties(innerRef)
			distinctExpr := mustDistinctConstruct(expressions.NewLogicalDistinctExpression(
				expressions.ForEachQuantifier(innerRef),
			))
			results := mustFireImplementationRule(t,
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

// TestDistinctFinal_SecondaryUniqueMultiTypeStreamDoesNotEliminate is clause 7's
// STREAM-side witness, and it is a different failure from the candidate-side one
// the SHARED arm pins.
//
// A candidate can name exactly one record type and still be offered to a stream
// carrying two. Physical keys include a record-type discriminator, so A/"x" and
// B/"x" are distinct index entries — but the moment the shared visible
// coordinate is the only thing projected they are the same SQL row, and a
// UNIQUE declaration made about A's entries says nothing about B's.
//
// Without this the predicate could take recordTypes[0] and ignore the rest,
// which reds nothing else in the suite.
func TestDistinctFinal_SecondaryUniqueMultiTypeStreamDoesNotEliminate(t *testing.T) {
	t.Parallel()

	layout := values.NewRecordType("AB", false, []values.Field{
		{Name: "CODE", FieldType: values.NotNullString, Ordinal: 0},
	})
	scan := mustDistinctConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"A", "B"}, layout))
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	scanRow := mustDistinctConstruct(scanQ.RequireFlowedObjectValue())
	code := mustDistinctConstruct(values.ResolveFieldOrdinals(scanRow, []int{0}))
	projection := mustDistinctConstruct(expressions.NewLogicalProjectionExpression(
		[]values.Value{code}, scanQ))
	projectionRef := expressions.InitialOf(projection)
	projectionRef.Insert(mustDistinctConstruct(plans.NewRecordQueryScanPlan(
		[]string{"A", "B"}, projection.GetResultValue().Type(), false)))
	distinct := mustDistinctConstruct(expressions.NewLogicalDistinctExpression(
		expressions.ForEachQuantifier(projectionRef),
	))

	scalar := false
	ctx := &indexTestPlanContext{
		readableIndexes: AllIndexesReadable(),
		candidates: []MatchCandidate{
			NewValueIndexScanMatchCandidateWithFunctions(
				"A$code_unique",
				[]string{"A"},
				[]string{"CODE"},
				nil,
				[]values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()},
				layout,
				true,
				nil,
				&scalar,
			).WithKeyComponentTypes([]values.Type{values.NotNullString}),
		},
	}

	results := mustFireImplementationRuleWithContext(t,
		NewImplementDistinctFinalRule(), expressions.InitialOf(distinct), ctx, nil,
	)
	if len(results) == 0 {
		t.Fatal("rule did not fire at all")
	}
	for _, result := range results {
		if _, ok := result.(*plans.RecordQueryDistinctPlan); ok {
			return
		}
	}
	t.Fatal("DISTINCT(CODE) was elided over a TWO-record-type stream on a UNIQUE " +
		"index declared over ONE of them. A$code_unique constrains A's entries; " +
		"B may hold the same CODE, and after projecting only CODE the two rows " +
		"are indistinguishable")
}

// TestDistinctFinal_BothLicensesHoldYieldsUnstampedPlan pins the rule's LICENSE
// ORDER, and it is the criterion that fails the conservative-looking
// implementation — stamp whenever a qualifying unique index exists — which every
// other test here passes.
//
// A primary key is a storage invariant with no state to move; a unique index's
// READABLE-ness can move under a live statement. When BOTH license the same
// elision, the plan's correctness rests on the primary key alone, so recording a
// dependency on the index can only turn an unrelated index build into a 40001 on
// a statement that would have been correct regardless. That is the same
// over-scoping this design rejects for a planner-global accumulator, arrived at
// from the other direction.
//
// The fixture is the one place both licenses are stated at once: EMAIL is the
// primary key AND T$email_unique covers it.
func TestDistinctFinal_BothLicensesHoldYieldsUnstampedPlan(t *testing.T) {
	t.Parallel()

	ctx := &indexTestPlanContext{
		candidates:      secondaryUniqueTestCandidates(),
		readableIndexes: AllIndexesReadable(),
		pk:              map[string][]string{"T": {"EMAIL"}},
	}

	// The fixture is only meaningful if the secondary proof WOULD have fired on
	// its own — otherwise "unstamped" is trivially satisfied.
	soleLicenseCtx := &indexTestPlanContext{
		candidates:      secondaryUniqueTestCandidates(),
		readableIndexes: AllIndexesReadable(),
	}
	if p := secondaryUniqueEliminationProof(
		soleUniqueProjectionFor(t, "EMAIL"), soleLicenseCtx,
	); !p.FullElision || p.IndexName != "T$email_unique" {
		t.Fatalf("the secondary-UNIQUE proof does not fully elide on this projection "+
			"on its own (%q, elision=%v), so asserting it does not STAMP proves nothing",
			p.IndexName, p.FullElision)
	}

	results := fireDistinctOverT(t, ctx, "EMAIL")
	if len(results) == 0 {
		t.Fatal("rule did not fire at all")
	}
	elided := false
	for _, result := range results {
		if _, ok := result.(*plans.RecordQueryDistinctPlan); ok {
			t.Fatal("DISTINCT(EMAIL) was retained even though EMAIL is the PRIMARY KEY")
		}
		elided = true
		if stamped, ok := result.(plans.DistinctProofStamped); ok {
			if name := stamped.GetDistinctProofIndexName(); name != "" {
				t.Fatalf("the elision was licensed by PRIMARY-KEY coverage and still "+
					"recorded a dependency on index %q. Transitioning that index would "+
					"then 40001 a statement whose correctness never rested on it", name)
			}
		}
	}
	if !elided {
		t.Fatal("no plan was yielded, so nothing was observed")
	}
}

// soleUniqueProjectionFor rebuilds the logical projection the rule inspects, so
// the admission predicate can be asked directly.
func soleUniqueProjectionFor(t *testing.T, column string) expressions.RelationalExpression {
	t.Helper()
	scan := mustDistinctConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"T"}, distinctScanType("T")))
	scanQ := expressions.NamedForEachQuantifier(
		distinctReadAlias("T"), expressions.InitialOf(scan))
	return mustDistinctConstruct(expressions.NewLogicalProjectionExpression(
		[]values.Value{distinctRead("T", column)}, scanQ,
	))
}
