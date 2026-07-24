package cascades

import (
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"google.golang.org/protobuf/proto"
)

func TestExpandValueIndex_TwoColumns(t *testing.T) {
	t.Parallel()

	alias0 := values.UniqueCorrelationIdentifier()
	alias1 := values.UniqueCorrelationIdentifier()
	cand := newKnownDistinctValueIndexCandidate(
		"idx_order_region_amount",
		[]string{"Order"},
		[]string{"region", "amount"},
		[]values.CorrelationIdentifier{alias0, alias1},
		values.UnknownType,
		false,
		nil,
	)

	trav := ExpandValueIndex(cand)
	if trav == nil {
		t.Fatal("ExpandValueIndex returned nil")
	}

	rootRef := trav.GetRootReference()
	if rootRef == nil {
		t.Fatal("root reference is nil")
	}

	// Root expression should be MatchableSortExpression.
	members := rootRef.AllMembers()
	if len(members) != 1 {
		t.Fatalf("root ref members: got %d, want 1", len(members))
	}
	matchSort, ok := members[0].(*expressions.MatchableSortExpression)
	if !ok {
		t.Fatalf("root expression: got %T, want *MatchableSortExpression", members[0])
	}

	// Sort parameter IDs should match sargable aliases.
	sortIDs := matchSort.GetSortParameterIDs()
	if len(sortIDs) != 2 {
		t.Fatalf("sort param IDs: got %d, want 2", len(sortIDs))
	}
	if sortIDs[0] != alias0 || sortIDs[1] != alias1 {
		t.Fatalf("sort param IDs mismatch: got %v, want [%s, %s]", sortIDs, alias0, alias1)
	}
	if matchSort.IsReverse() {
		t.Fatal("isReverse: got true, want false")
	}

	// Inner quantifier leads to SelectExpression.
	innerQ := matchSort.GetInner()
	innerRef := innerQ.GetRangesOver()
	innerMembers := innerRef.AllMembers()
	if len(innerMembers) != 1 {
		t.Fatalf("inner ref members: got %d, want 1", len(innerMembers))
	}
	selectExpr, ok := innerMembers[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("inner expression: got %T, want *SelectExpression", innerMembers[0])
	}

	// SelectExpression should have 2 predicates (Placeholders).
	preds := selectExpr.GetPredicates()
	if len(preds) != 2 {
		t.Fatalf("predicates: got %d, want 2", len(preds))
	}
	for i, pred := range preds {
		ph, ok := pred.(*predicates.Placeholder)
		if !ok {
			t.Fatalf("predicate[%d]: got %T, want *Placeholder", i, pred)
		}
		fv, ok := ph.Value.(*values.FieldValue)
		if !ok {
			t.Fatalf("placeholder[%d] value: got %T, want *FieldValue", i, ph.Value)
		}
		expectedCol := []string{"region", "amount"}[i]
		if fv.Field != expectedCol {
			t.Fatalf("placeholder[%d] field: got %q, want %q", i, fv.Field, expectedCol)
		}
		expectedAlias := []values.CorrelationIdentifier{alias0, alias1}[i]
		if ph.ParameterAlias != expectedAlias {
			t.Fatalf("placeholder[%d] alias: got %s, want %s", i, ph.ParameterAlias, expectedAlias)
		}
	}

	// SelectExpression should have 1 quantifier (the ForEach over the scan).
	selectQuants := selectExpr.GetQuantifiers()
	if len(selectQuants) != 1 {
		t.Fatalf("select quantifiers: got %d, want 1", len(selectQuants))
	}

	// The base scan should be a FullUnorderedScanExpression.
	scanRef := selectQuants[0].GetRangesOver()
	scanMembers := scanRef.AllMembers()
	if len(scanMembers) != 1 {
		t.Fatalf("scan ref members: got %d, want 1", len(scanMembers))
	}
	scan, ok := scanMembers[0].(*expressions.FullUnorderedScanExpression)
	if !ok {
		t.Fatalf("scan expression: got %T, want *FullUnorderedScanExpression", scanMembers[0])
	}
	if rt := scan.GetRecordTypes(); len(rt) != 1 || rt[0] != "Order" {
		t.Fatalf("scan record types: got %v, want [Order]", rt)
	}
}

func TestExpandValueIndex_ZeroColumns(t *testing.T) {
	t.Parallel()

	cand := newKnownDistinctValueIndexCandidate(
		"idx_empty",
		[]string{"Customer"},
		[]string{},
		[]values.CorrelationIdentifier{},
		values.UnknownType,
		false,
		nil,
	)

	trav := ExpandValueIndex(cand)
	if trav == nil {
		t.Fatal("ExpandValueIndex returned nil")
	}

	rootRef := trav.GetRootReference()
	members := rootRef.AllMembers()
	if len(members) != 1 {
		t.Fatalf("root ref members: got %d, want 1", len(members))
	}
	matchSort, ok := members[0].(*expressions.MatchableSortExpression)
	if !ok {
		t.Fatalf("root expression: got %T, want *MatchableSortExpression", members[0])
	}
	if len(matchSort.GetSortParameterIDs()) != 0 {
		t.Fatalf("sort param IDs: got %d, want 0", len(matchSort.GetSortParameterIDs()))
	}

	// Inner should be a SelectExpression with no predicates.
	innerQ := matchSort.GetInner()
	innerMembers := innerQ.GetRangesOver().AllMembers()
	selectExpr, ok := innerMembers[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("inner expression: got %T, want *SelectExpression", innerMembers[0])
	}
	if len(selectExpr.GetPredicates()) != 0 {
		t.Fatalf("predicates: got %d, want 0", len(selectExpr.GetPredicates()))
	}
}

func TestValueIndexScanMatchCandidate_GetTraversal_NonNil(t *testing.T) {
	t.Parallel()

	cand := newKnownDistinctValueIndexCandidate(
		"idx_name",
		[]string{"Person"},
		[]string{"name"},
		[]values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()},
		values.UnknownType,
		false,
		nil,
	)

	trav := cand.GetTraversal()
	if trav == nil {
		t.Fatal("GetTraversal returned nil")
	}
	if trav.GetRootReference() == nil {
		t.Fatal("traversal root reference is nil")
	}
}

func TestValueIndexScanMatchCandidate_GetTraversal_SyncOnce(t *testing.T) {
	t.Parallel()

	cand := newKnownDistinctValueIndexCandidate(
		"idx_city",
		[]string{"Address"},
		[]string{"city", "zip"},
		[]values.CorrelationIdentifier{
			values.UniqueCorrelationIdentifier(),
			values.UniqueCorrelationIdentifier(),
		},
		values.UnknownType,
		true,
		nil,
	)

	trav1 := cand.GetTraversal()
	trav2 := cand.GetTraversal()
	if trav1 != trav2 {
		t.Fatal("GetTraversal returned different pointers on repeated calls (sync.Once violated)")
	}
}

func TestValueIndexScanMatchCandidate_UnknownMetadataFailsClosed(t *testing.T) {
	t.Parallel()

	alias := values.UniqueCorrelationIdentifier()
	unknown := NewValueIndexScanMatchCandidate(
		"idx_unknown",
		[]string{"Item"},
		[]string{"TAGS"},
		[]values.CorrelationIdentifier{alias},
		values.UnknownType,
		false,
		[]string{"ID"},
	)
	if traversal := unknown.GetTraversal(); traversal != nil {
		t.Fatal("candidate with neither duplicate signal nor root AST produced a traversal")
	}
	if plan := unknown.ToScanPlan(nil, false); plan != nil {
		t.Fatalf("unknown candidate produced a direct scan plan: %T", plan)
	}
	if parts := unknown.ComputeMatchedOrderingParts(
		NewRegularMatchInfo(nil, nil, nil, nil, nil, nil, nil, nil),
		[]values.CorrelationIdentifier{alias},
		false,
	); len(parts) != 0 {
		t.Fatalf("unknown candidate advertised ordering parts: %v", parts)
	}
	if prefix := unknown.ComputeBoundParameterPrefixMap(map[values.CorrelationIdentifier]*predicates.ComparisonRange{
		alias: predicates.EmptyComparisonRange(),
	}); prefix != nil {
		t.Fatalf("unknown candidate computed a scan prefix: %v", prefix)
	}
	if pk := unknown.GetPKColumnNames(); pk != nil {
		t.Fatalf("unknown candidate exposed a PK ordering/coverage suffix: %v", pk)
	}
	source := values.UniqueCorrelationIdentifier()
	target := values.UniqueCorrelationIdentifier()
	if translated, ok := unknown.PushValueThroughFetch(
		values.NewFieldValue(
			values.NewQuantifiedObjectValue(source),
			"TAGS",
			values.UnknownType,
		),
		source,
		target,
	); ok || translated != nil {
		t.Fatalf("unknown candidate translated coverage value: %v, %t", translated, ok)
	}

	knownScalar := newKnownDistinctValueIndexCandidate(
		"idx_scalar",
		[]string{"Item"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()},
		values.UnknownType,
		false,
		[]string{"ID"},
	)
	if knownScalar.GetTraversal() == nil || knownScalar.ToScanPlan(nil, false) == nil {
		t.Fatal("explicit known-scalar twin failed to plan")
	}
}

func TestExpandValueIndex_PreBuiltScalarNestingFailsClosed(t *testing.T) {
	t.Parallel()

	scalar := gen.Field_SCALAR
	nestedScalar := &gen.KeyExpression{Nesting: &gen.Nesting{
		Parent: &gen.Field{
			FieldName: proto.String("ADDR"),
			FanType:   &scalar,
		},
		Child: candidateTestKeyField("CITY", gen.Field_SCALAR),
	}}
	mixed := &gen.KeyExpression{Then: &gen.Then{Child: []*gen.KeyExpression{
		nestedScalar,
		candidateTestKeyField("TAGS", gen.Field_FAN_OUT),
	}}}

	for _, tc := range []struct {
		name    string
		columns []string
		root    *gen.KeyExpression
	}{
		{name: "scalar_nesting", columns: []string{"CITY"}, root: nestedScalar},
		{
			name:    "mixed_scalar_nesting_and_fanout",
			columns: []string{"CITY", "TAGS"},
			root:    mixed,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			aliases := make([]values.CorrelationIdentifier, len(tc.columns))
			for i := range aliases {
				aliases[i] = values.UniqueCorrelationIdentifier()
			}
			createsDuplicates := false
			candidate := NewValueIndexScanMatchCandidateWithFunctions(
				"idx_"+tc.name,
				[]string{"Item"},
				tc.columns,
				nil,
				aliases,
				values.UnknownType,
				false,
				nil,
				&createsDuplicates,
			).WithRootKeyExpression(tc.root)
			if traversal := candidate.GetTraversal(); traversal != nil {
				t.Fatal("prebuilt candidate flattened a scalar nested leaf")
			}
		})
	}
}

func TestExpandValueIndex_KnownDuplicatesWithoutFanOutRootFailsClosed(t *testing.T) {
	t.Parallel()

	duplicates := true
	candidate := NewValueIndexScanMatchCandidateWithFunctions(
		"idx_hidden_fanout",
		[]string{"Item"},
		[]string{"TAGS"},
		nil,
		[]values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()},
		values.UnknownType,
		false,
		nil,
		&duplicates,
	)
	if traversal := candidate.GetTraversal(); traversal != nil {
		t.Fatal("known duplicate-producing candidate without FAN_OUT topology produced a flat traversal")
	}
}

func TestExpandValueIndex_InconsistentFlatRootFailsClosed(t *testing.T) {
	t.Parallel()

	noDuplicates := false
	cardinalityRoot := &gen.KeyExpression{Function: &gen.Function{
		Name: proto.String(FunctionKindCardinality),
		Arguments: candidateTestKeyField(
			"TAGS",
			gen.Field_CONCATENATE,
		),
	}}
	customRoot := &gen.KeyExpression{Function: &gen.Function{
		Name: proto.String("custom"),
		Arguments: candidateTestKeyField(
			"TAGS",
			gen.Field_SCALAR,
		),
	}}

	for _, tc := range []struct {
		name      string
		functions []string
		root      *gen.KeyExpression
	}{
		{
			name: "cardinality_root_without_function_tag",
			root: cardinalityRoot,
		},
		{
			name:      "scalar_root_with_cardinality_tag",
			functions: []string{FunctionKindCardinality},
			root:      candidateTestKeyField("TAGS", gen.Field_SCALAR),
		},
		{
			name: "unsupported_custom_function",
			root: customRoot,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			candidate := NewValueIndexScanMatchCandidateWithFunctions(
				"idx_"+tc.name,
				[]string{"Item"},
				[]string{"TAGS"},
				tc.functions,
				[]values.CorrelationIdentifier{
					values.UniqueCorrelationIdentifier(),
				},
				values.UnknownType,
				false,
				nil,
				&noDuplicates,
			).WithRootKeyExpression(tc.root)
			if traversal := candidate.GetTraversal(); traversal != nil {
				t.Fatal("inconsistent/unsupported flat key metadata produced a traversal")
			}
			if scan := candidate.ToScanPlan(nil, false); scan != nil {
				t.Fatalf("inconsistent/unsupported flat key metadata produced %T", scan)
			}
		})
	}
}

func TestExpandValueIndex_LeafReferences(t *testing.T) {
	t.Parallel()

	cand := newKnownDistinctValueIndexCandidate(
		"idx_leaf",
		[]string{"Item"},
		[]string{"price"},
		[]values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()},
		values.UnknownType,
		false,
		nil,
	)

	trav := ExpandValueIndex(cand)
	leafRefs := trav.GetLeafReferences()
	if len(leafRefs) == 0 {
		t.Fatal("expected at least one leaf reference")
	}

	// The leaf should contain the FullUnorderedScanExpression.
	foundScan := false
	for _, ref := range leafRefs {
		for _, expr := range ref.AllMembers() {
			if _, ok := expr.(*expressions.FullUnorderedScanExpression); ok {
				foundScan = true
			}
		}
	}
	if !foundScan {
		t.Fatal("no FullUnorderedScanExpression found in leaf references")
	}
}

func TestExpandValueIndex_DirectFanOutUsesJavaGraphShape(t *testing.T) {
	t.Parallel()

	alias := values.UniqueCorrelationIdentifier()
	rowType := values.NewRecordType("Item", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{
			Name:      "TAGS",
			FieldType: values.NewArrayType(true, values.NotNullString),
			Ordinal:   1,
		},
	})
	root := keyExpressionField("TAGS", gen.Field_FAN_OUT)
	duplicates := true
	cand := NewValueIndexScanMatchCandidateWithFunctions(
		"item_tags",
		[]string{"Item"},
		[]string{"TAGS"},
		nil,
		[]values.CorrelationIdentifier{alias},
		rowType,
		false,
		nil,
		&duplicates,
	).WithRootKeyExpression(root)
	root.Field.FieldName = proto.String("MUTATED")
	root.Field.FanType = gen.Field_SCALAR.Enum()
	returnedRoot := cand.GetRootKeyExpression()
	returnedRoot.Field.FieldName = proto.String("ALSO_MUTATED")
	if got := cand.GetRootKeyExpression().GetField().GetFieldName(); got != "TAGS" {
		t.Fatalf("candidate root was not defensively cloned: got field %q", got)
	}

	trav := ExpandValueIndex(cand)
	if trav == nil {
		t.Fatal("fanout expansion returned nil")
	}
	top := fanoutExpansionTopSelect(t, trav)
	if len(top.GetPredicates()) != 0 {
		t.Fatalf("top predicates = %d, want 0: the fanout placeholder belongs to the inner Select", len(top.GetPredicates()))
	}
	topQuantifiers := top.GetQuantifiers()
	if len(topQuantifiers) != 2 {
		t.Fatalf("top quantifiers = %d, want base scan + fanout ForEach", len(topQuantifiers))
	}
	innerMembers := topQuantifiers[1].GetRangesOver().AllMembers()
	if len(innerMembers) != 1 {
		t.Fatalf("fanout child members = %d, want 1", len(innerMembers))
	}
	inner, ok := innerMembers[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("fanout child = %T, want *SelectExpression", innerMembers[0])
	}
	if len(inner.GetQuantifiers()) != 1 {
		t.Fatalf("inner quantifiers = %d, want 1 Explode", len(inner.GetQuantifiers()))
	}
	explode, ok := inner.GetQuantifiers()[0].GetRangesOver().Get().(*expressions.ExplodeExpression)
	if !ok {
		t.Fatalf("inner leaf = %T, want *ExplodeExpression", inner.GetQuantifiers()[0].GetRangesOver().Get())
	}
	collection, ok := explode.GetCollectionValue().(*values.FieldValue)
	if !ok {
		t.Fatalf("explode collection = %T, want *FieldValue", explode.GetCollectionValue())
	}
	if collection.Resolved == nil || collection.Resolved.Root().Ordinal != 1 {
		t.Fatalf("explode collection path = %#v, want TAGS at ordinal 1", collection.Resolved)
	}
	innerPredicates := inner.GetPredicates()
	if len(innerPredicates) != 1 {
		t.Fatalf("inner predicates = %d, want 1 placeholder", len(innerPredicates))
	}
	placeholder, ok := innerPredicates[0].(*predicates.Placeholder)
	if !ok {
		t.Fatalf("inner predicate = %T, want *Placeholder", innerPredicates[0])
	}
	if placeholder.ParameterAlias != alias {
		t.Fatalf("inner placeholder alias = %s, want %s", placeholder.ParameterAlias, alias)
	}
	if qov, ok := placeholder.Value.(*values.QuantifiedObjectValue); !ok ||
		qov.Correlation != inner.GetQuantifiers()[0].GetAlias() {
		t.Fatalf("inner placeholder value = %#v, want the exploded element QOV", placeholder.Value)
	}
}

func TestExpandValueIndex_ThenKeepsScalarAndFanOutPlaceholdersAtTheirJavaLevels(t *testing.T) {
	t.Parallel()

	regionAlias := values.UniqueCorrelationIdentifier()
	tagAlias := values.UniqueCorrelationIdentifier()
	rowType := values.NewRecordType("Item", false, []values.Field{
		{Name: "REGION", FieldType: values.NotNullString, Ordinal: 0},
		{
			Name:      "TAGS",
			FieldType: values.NewArrayType(true, values.NotNullString),
			Ordinal:   1,
		},
	})
	root := &gen.KeyExpression{Then: &gen.Then{Child: []*gen.KeyExpression{
		keyExpressionField("REGION", gen.Field_SCALAR),
		keyExpressionField("TAGS", gen.Field_FAN_OUT),
	}}}
	duplicates := true
	cand := NewValueIndexScanMatchCandidateWithFunctions(
		"item_region_tags",
		[]string{"Item"},
		[]string{"REGION", "TAGS"},
		nil,
		[]values.CorrelationIdentifier{regionAlias, tagAlias},
		rowType,
		false,
		nil,
		&duplicates,
	).WithRootKeyExpression(root)

	top := fanoutExpansionTopSelect(t, ExpandValueIndex(cand))
	if got := len(top.GetQuantifiers()); got != 2 {
		t.Fatalf("top quantifiers = %d, want scan + one fanout child", got)
	}
	if got := len(top.GetPredicates()); got != 1 {
		t.Fatalf("top predicates = %d, want the scalar REGION placeholder only", got)
	}
	topPlaceholder, ok := top.GetPredicates()[0].(*predicates.Placeholder)
	if !ok || topPlaceholder.ParameterAlias != regionAlias {
		t.Fatalf("top predicate = %#v, want REGION placeholder %s", top.GetPredicates()[0], regionAlias)
	}
	inner := top.GetQuantifiers()[1].GetRangesOver().Get().(*expressions.SelectExpression)
	if got := len(inner.GetPredicates()); got != 1 {
		t.Fatalf("inner predicates = %d, want the TAGS placeholder only", got)
	}
	innerPlaceholder := inner.GetPredicates()[0].(*predicates.Placeholder)
	if innerPlaceholder.ParameterAlias != tagAlias {
		t.Fatalf("inner placeholder alias = %s, want %s", innerPlaceholder.ParameterAlias, tagAlias)
	}
}

func TestExpandValueIndex_NestedFanOutSharesOneExplodeAcrossThenChildren(t *testing.T) {
	t.Parallel()

	kindAlias := values.UniqueCorrelationIdentifier()
	scoreAlias := values.UniqueCorrelationIdentifier()
	childType := values.NewRecordType("Child", false, []values.Field{
		{Name: "KIND", FieldType: values.NotNullString, Ordinal: 0},
		{Name: "SCORE", FieldType: values.NotNullLong, Ordinal: 1},
	})
	rowType := values.NewRecordType("Parent", false, []values.Field{
		{
			Name:      "CHILDREN",
			FieldType: values.NewArrayType(true, childType),
			Ordinal:   0,
		},
	})
	parentFanType := gen.Field_FAN_OUT
	root := &gen.KeyExpression{Nesting: &gen.Nesting{
		Parent: &gen.Field{
			FieldName: proto.String("CHILDREN"),
			FanType:   &parentFanType,
		},
		Child: &gen.KeyExpression{Then: &gen.Then{Child: []*gen.KeyExpression{
			keyExpressionField("KIND", gen.Field_SCALAR),
			keyExpressionField("SCORE", gen.Field_SCALAR),
		}}},
	}}
	duplicates := true
	cand := NewValueIndexScanMatchCandidateWithFunctions(
		"parent_children_kind_score",
		[]string{"Parent"},
		[]string{"KIND", "SCORE"},
		nil,
		[]values.CorrelationIdentifier{kindAlias, scoreAlias},
		rowType,
		false,
		nil,
		&duplicates,
	).WithRootKeyExpression(root)

	top := fanoutExpansionTopSelect(t, ExpandValueIndex(cand))
	if got := len(top.GetQuantifiers()); got != 2 {
		t.Fatalf("top quantifiers = %d, want scan + one shared fanout child", got)
	}
	inner := top.GetQuantifiers()[1].GetRangesOver().Get().(*expressions.SelectExpression)
	if got := len(inner.GetQuantifiers()); got != 1 {
		t.Fatalf("inner quantifiers = %d, want exactly one shared Explode", got)
	}
	if _, ok := inner.GetQuantifiers()[0].GetRangesOver().Get().(*expressions.ExplodeExpression); !ok {
		t.Fatalf("inner leaf = %T, want *ExplodeExpression", inner.GetQuantifiers()[0].GetRangesOver().Get())
	}
	if got := len(inner.GetPredicates()); got != 2 {
		t.Fatalf("inner predicates = %d, want KIND and SCORE placeholders", got)
	}
	wantAliases := []values.CorrelationIdentifier{kindAlias, scoreAlias}
	wantOrdinals := []int{0, 1}
	for i, predicate := range inner.GetPredicates() {
		placeholder, ok := predicate.(*predicates.Placeholder)
		if !ok {
			t.Fatalf("inner predicate[%d] = %T, want *Placeholder", i, predicate)
		}
		if placeholder.ParameterAlias != wantAliases[i] {
			t.Fatalf("inner placeholder[%d] alias = %s, want %s", i, placeholder.ParameterAlias, wantAliases[i])
		}
		field, ok := placeholder.Value.(*values.FieldValue)
		if !ok || field.Resolved == nil || field.Resolved.Root().Ordinal != wantOrdinals[i] {
			t.Fatalf("inner placeholder[%d] value = %#v, want child ordinal %d", i, placeholder.Value, wantOrdinals[i])
		}
	}
}

func TestExpandValueIndex_UnsupportedFanOutFailsClosed(t *testing.T) {
	t.Parallel()

	alias := values.UniqueCorrelationIdentifier()
	root := &gen.KeyExpression{Function: &gen.Function{
		Name:      proto.String("unsupported_fanout_function"),
		Arguments: keyExpressionField("TAGS", gen.Field_FAN_OUT),
	}}
	duplicates := true
	cand := NewValueIndexScanMatchCandidateWithFunctions(
		"unsupported",
		[]string{"Item"},
		[]string{"TAGS"},
		nil,
		[]values.CorrelationIdentifier{alias},
		values.UnknownType,
		false,
		nil,
		&duplicates,
	).WithRootKeyExpression(root)

	if traversal := ExpandValueIndex(cand); traversal != nil {
		t.Fatal("unsupported fanout key expression must fail closed instead of becoming a flat candidate")
	}
}

func TestExpandValueIndex_FanOutASTColumnMismatchFailsClosed(t *testing.T) {
	t.Parallel()

	alias := values.UniqueCorrelationIdentifier()
	duplicates := true
	cand := NewValueIndexScanMatchCandidateWithFunctions(
		"mismatched",
		[]string{"Item"},
		[]string{"OTHER"},
		nil,
		[]values.CorrelationIdentifier{alias},
		values.UnknownType,
		false,
		nil,
		&duplicates,
	).WithRootKeyExpression(keyExpressionField("TAGS", gen.Field_FAN_OUT))

	if traversal := ExpandValueIndex(cand); traversal != nil {
		t.Fatal("flat column OTHER must not create a candidate that physically scans AST field TAGS")
	}
}

func TestExpandValueIndex_NullableArrayWrapperFailsClosed(t *testing.T) {
	t.Parallel()

	for _, childFanType := range []gen.Field_FanType{
		gen.Field_FAN_OUT,
		gen.Field_CONCATENATE,
	} {
		childFanType := childFanType
		t.Run(childFanType.String(), func(t *testing.T) {
			t.Parallel()

			parentFanType := gen.Field_SCALAR
			root := &gen.KeyExpression{Nesting: &gen.Nesting{
				Parent: &gen.Field{
					FieldName: proto.String("TAGS"),
					FanType:   &parentFanType,
				},
				Child: keyExpressionField("values", childFanType),
			}}
			columns := []string{"values"}
			aliases := []values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()}
			if childFanType == gen.Field_CONCATENATE {
				// CONCATENATE alone keeps the ordinary flat compatibility path.
				// The unsafe case is a larger fanout root, whose structural
				// visitor would otherwise interpret this wrapper sibling as an
				// ordinary nested field.
				root = &gen.KeyExpression{Then: &gen.Then{Child: []*gen.KeyExpression{
					root,
					keyExpressionField("OTHER", gen.Field_FAN_OUT),
				}}}
				columns = append(columns, "OTHER")
				aliases = append(aliases, values.UniqueCorrelationIdentifier())
			}
			duplicates := true
			cand := NewValueIndexScanMatchCandidateWithFunctions(
				"java_nullable_tags",
				[]string{"Item"},
				columns,
				nil,
				aliases,
				values.UnknownType,
				false,
				nil,
				&duplicates,
			).WithRootKeyExpression(root)

			if traversal := ExpandValueIndex(cand); traversal != nil {
				t.Fatal("nullable-array wrapper must decline until its descriptor rewrite is available")
			}
		})
	}
}

func keyExpressionField(name string, fanType gen.Field_FanType) *gen.KeyExpression {
	return &gen.KeyExpression{Field: &gen.Field{
		FieldName: proto.String(name),
		FanType:   &fanType,
	}}
}

func fanoutExpansionTopSelect(t *testing.T, traversal *Traversal) *expressions.SelectExpression {
	t.Helper()
	if traversal == nil || traversal.GetRootReference() == nil {
		t.Fatal("fanout traversal/root is nil")
	}
	sortExpr, ok := traversal.GetRootReference().Get().(*expressions.MatchableSortExpression)
	if !ok {
		t.Fatalf("root = %T, want *MatchableSortExpression", traversal.GetRootReference().Get())
	}
	selectExpr, ok := sortExpr.GetInner().GetRangesOver().Get().(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("sort inner = %T, want *SelectExpression", sortExpr.GetInner().GetRangesOver().Get())
	}
	return selectExpr
}

func TestAggregateIndexMatchCandidate_GetTraversal_NonNil(t *testing.T) {
	t.Parallel()

	cand := NewAggregateIndexMatchCandidate(
		"idx_sum_region",
		[]string{"Order"},
		[]string{"region"},
		expressions.AggSum,
		"amount",
	)

	trav := cand.GetTraversal()
	if trav == nil {
		t.Fatal("AggregateIndexMatchCandidate.GetTraversal returned nil")
	}
	if trav.GetRootReference() == nil {
		t.Fatal("traversal root reference is nil")
	}

	// Should have a MatchableSortExpression at root.
	members := trav.GetRootReference().AllMembers()
	if len(members) != 1 {
		t.Fatalf("root ref members: got %d, want 1", len(members))
	}
	if _, ok := members[0].(*expressions.MatchableSortExpression); !ok {
		t.Fatalf("root expression: got %T, want *MatchableSortExpression", members[0])
	}
}

func TestAggregateIndexMatchCandidate_GetTraversal_SyncOnce(t *testing.T) {
	t.Parallel()

	cand := NewAggregateIndexMatchCandidate(
		"idx_count",
		[]string{"Event"},
		[]string{"category"},
		expressions.AggCount,
		"id",
	)

	trav1 := cand.GetTraversal()
	trav2 := cand.GetTraversal()
	if trav1 != trav2 {
		t.Fatal("AggregateIndexMatchCandidate.GetTraversal returned different pointers on repeated calls")
	}
}
