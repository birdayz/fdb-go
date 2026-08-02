package cascades

import (
	"strings"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// columnValueProvider is implemented by match candidates whose key columns are
// not all bare fields — they supply the per-column match Value (e.g. a
// CARDINALITY()-keyed column yields CardinalityValue(FieldValue(col))). The
// base argument is the QuantifiedObjectValue of the index's record source.
// Candidates that don't implement it default to FieldValue(base, col).
type columnValueProvider interface {
	ColumnValue(i int, base values.Value) values.Value
}

type rootKeyExpressionProvider interface {
	GetRootKeyExpression() *gen.KeyExpression
}

// ExpandValueIndex builds a Traversal from an index definition,
// producing a candidate expression tree with Placeholder predicates
// for each index column. The resulting Traversal is used by matching
// rules to match query predicates against index columns.
//
// The output structure matches Java's ValueIndexExpansionVisitor:
//
//	MatchableSortExpression(sortParamIDs, isReverse=false,
//	  SelectExpression(resultValue,
//	    [ForEach(FullUnorderedScanExpression(recordTypes))],
//	    [Placeholder(param0, FieldValue("col0")),
//	     Placeholder(param1, FieldValue("col1")),
//	     ...]))
//
// When the candidate exposes its record-layer key-expression AST, fan-out
// fields are expanded with the same nested Select/Explode topology as Java's
// KeyExpressionExpansionVisitor. Candidates without that optional structure,
// and supported non-fan-out indexes, retain the flat-column expansion.
func ExpandValueIndex(candidate MatchCandidate) *Traversal {
	if valueCandidate, ok := candidate.(*ValueIndexScanMatchCandidate); ok &&
		!valueCandidate.metadataSufficientForPlanning() {
		// A flat fallback is safe only when metadata affirmatively says the
		// index preserves one entry per record. UNKNOWN metadata may hide a
		// fan-out index; a known duplicate-producing index without a FAN_OUT
		// AST has no structural Explode with which to repair cardinality.
		return nil
	}
	if provider, ok := candidate.(rootKeyExpressionProvider); ok {
		root := provider.GetRootKeyExpression()
		if keyExpressionContainsNonFanOutNestedLeaf(root) {
			// Keep this at the traversal authority as well as the metadata
			// builder: callers may inject a prebuilt candidate directly.
			// Flattening ADDR.CITY to CITY is unsafe even when a separate
			// branch of the same root fans out.
			return nil
		}
		if keyExpressionContainsFanOut(root) {
			if vc, isValue := candidate.(*ValueIndexScanMatchCandidate); isValue && vc.predicateProto != nil {
				// A SPARSE fan-out index: the fan-out expansion does not yet
				// carry the candidate-side predicate, and matching without it
				// would treat the filtered index as full — silently missing
				// rows. Go DDL cannot author this shape today (the generator
				// rejects non-unnested arrays), so it is reachable only from
				// Java-authored metadata; excluding the candidate is the safe
				// failure mode (the query falls back to a scan).
				return nil
			}
			traversal, expanded := expandFanOutValueIndex(candidate, root)
			if !expanded {
				// A flat fallback would turn an unsupported repeated-field key
				// into a scalar access path and admit semantically invalid
				// matches. Excluding that candidate is the safe failure mode.
				return nil
			}
			return traversal
		}
	}
	return expandFlatValueIndex(candidate)
}

// expandFlatValueIndex is the historical flat-column expansion. It remains
// the compatibility path for ordinary scalar indexes and for non-fan-out key
// expression kinds whose Value bridge is supplied separately (for example
// CARDINALITY()).
func expandFlatValueIndex(candidate MatchCandidate) *Traversal {
	columnNames := candidate.GetColumnNames()
	sargableAliases := candidate.GetSargableAliases()
	recordTypes := candidate.GetRecordTypes()

	// Base scan: FullUnorderedScanExpression over the candidate's record types.
	scan := expressions.NewFullUnorderedScanExpression(recordTypes, values.UnknownType)
	baseQuantifier := expressions.ForEachQuantifier(expressions.InitialOf(scan))

	// Build the graph expansion: one Placeholder per index column.
	builder := NewGraphExpansionBuilder()
	builder.AddQuantifier(baseQuantifier)

	// columnNames and sargableAliases are parallel slices; iterate over
	// sargableAliases as the authoritative length (callers that pass nil
	// sargableAliases get zero placeholders).
	//
	// The per-column placeholder Value is normally FieldValue(base, col). A
	// candidate that carries a function-keyed column (e.g. a CARDINALITY()
	// index) overrides this via columnValueProvider so the placeholder Value
	// is CardinalityValue(FieldValue(base, col)) — the SAME Value the query
	// side builds, so the predicate (and, via the same provider, the sort)
	// binds by Value-tree equality. Mirrors Java's match candidate carrying
	// the column's Value (CardinalityFunctionKeyExpression.toValue()).
	baseAlias := baseQuantifier.GetAlias()
	provider, _ := candidate.(columnValueProvider)
	for i, alias := range sargableAliases {
		var colValue values.Value
		if provider != nil {
			colValue = provider.ColumnValue(i, values.NewQuantifiedObjectValue(baseAlias))
		} else {
			colValue = values.NewFieldValue(
				values.NewQuantifiedObjectValue(baseAlias),
				columnNames[i], values.UnknownType,
			)
		}
		ph := predicates.NewPlaceholder(alias, colValue)
		builder.AddPredicate(ph)
		builder.AddPlaceholder(ph)
	}

	// A SPARSE index carries its stored predicate into the candidate graph
	// (ValueIndexExpansionVisitor.java:138-162): the filtered predicate,
	// rooted at the base quantifier's flowed value, becomes a candidate-side
	// predicate unless it is a tautology (:141). The matcher refuses to match
	// a candidate predicate it cannot account for
	// (MatchIntermediateRule's candidate-predicate walk), so a query can
	// never treat the filtered index as full — the wrong-results direction.
	// Java's ranges arm (:146-158) additionally re-expresses a DNF-of-ranges
	// predicate as extra placeholder ranges so IMPLIED queries still match;
	// Go's Placeholder carries no candidate-side ranges yet, so every
	// non-tautological sparse candidate is conservatively unmatchable —
	// correct results, narrower plan reach than Java (sparse-index-tests
	// .yamsql's COVERING expectations are the re-arm witness).
	if vc, isValue := candidate.(*ValueIndexScanMatchCandidate); isValue && vc.predicateProto != nil {
		converted, convErr := indexPredicateToQueryPredicate(
			vc.predicateProto, values.NewQuantifiedObjectValue(baseAlias))
		if convErr != nil {
			// An unconvertible stored predicate (unknown proto arm) leaves
			// the candidate's entry set unknowable — exclude the candidate.
			return nil
		}
		if !predicates.IsTautology(converted) {
			builder.AddPredicate(converted)
		}
	}

	expansion := builder.Build()
	sealed := expansion.Seal()

	// Build SelectExpression with the base quantifier's flowed object value
	// as the result value. The sealed expansion must have no result columns
	// (we only added predicates/placeholders, no columns), so
	// BuildSelectWithResultValue is the right call.
	selectExpr := sealed.BuildSelectWithResultValue(baseQuantifier.GetFlowedObjectValue())

	// Wrap in MatchableSortExpression — the sort is defined by the
	// sargable aliases (one per index key column), not reversed.
	matchableSort := expressions.NewMatchableSortExpressionFromExpr(
		sargableAliases,
		false,
		selectExpr,
	)

	return NewTraversal(expressions.InitialOf(matchableSort))
}

// fanOutKeyExpansionState is the immutable traversal context plus a shared
// key-column ordinal. Then children and a nesting child consume aliases from
// the same left-to-right stream, matching ThenKeyExpression.getColumnSize().
type fanOutKeyExpansionState struct {
	candidate MatchCandidate
	baseAlias values.CorrelationIdentifier
	baseType  values.Type
	prefix    []string
	ordinal   *int
}

func (s fanOutKeyExpansionState) takeColumn() (
	values.CorrelationIdentifier,
	string,
	bool,
) {
	aliases := s.candidate.GetSargableAliases()
	columns := s.candidate.GetColumnNames()
	if s.ordinal == nil || *s.ordinal < 0 ||
		*s.ordinal >= len(aliases) || *s.ordinal >= len(columns) {
		return values.CorrelationIdentifier{}, "", false
	}
	ordinal := *s.ordinal
	*s.ordinal = ordinal + 1
	return aliases[ordinal], columns[ordinal], true
}

// expandFanOutValueIndex ports the subset of Java's
// KeyExpressionExpansionVisitor needed by value fan-out indexes: Field,
// Then, and Nesting. Unsupported shapes reject the candidate instead of
// silently manufacturing a flat scalar graph.
func expandFanOutValueIndex(
	candidate MatchCandidate,
	root *gen.KeyExpression,
) (*Traversal, bool) {
	if candidate == nil || root == nil {
		return nil, false
	}

	baseType := values.Type(values.UnknownType)
	if withBaseType, ok := candidate.(interface{ GetBaseType() values.Type }); ok {
		if typ := withBaseType.GetBaseType(); typ != nil {
			baseType = typ
		}
	}
	scan := expressions.NewFullUnorderedScanExpression(
		candidate.GetRecordTypes(),
		values.UnknownType,
	)
	baseQuantifier := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	ordinal := 0
	state := fanOutKeyExpansionState{
		candidate: candidate,
		baseAlias: baseQuantifier.GetAlias(),
		baseType:  baseType,
		ordinal:   &ordinal,
	}
	keyExpansion, ok := expandFanOutKeyExpression(root, state)
	if !ok || ordinal != len(candidate.GetSargableAliases()) ||
		ordinal != len(candidate.GetColumnNames()) {
		return nil, false
	}

	quantifiers := make([]expressions.Quantifier, 0, 1+len(keyExpansion.GetQuantifiers()))
	quantifiers = append(quantifiers, baseQuantifier)
	quantifiers = append(quantifiers, keyExpansion.GetQuantifiers()...)
	completeExpansion := NewGraphExpansion(
		nil, // ValueIndexExpansionVisitor.removeAllResultColumns()
		keyExpansion.GetPredicates(),
		quantifiers,
		keyExpansion.GetPlaceholders(),
	)
	sealed := completeExpansion.Seal()
	selectExpression := sealed.BuildSelectWithResultValue(
		baseQuantifier.GetFlowedObjectValue(),
	)
	matchableSort := expressions.NewMatchableSortExpressionFromExpr(
		candidate.GetSargableAliases(),
		false,
		selectExpression,
	)
	return NewTraversal(expressions.InitialOf(matchableSort)), true
}

func expandFanOutKeyExpression(
	expression *gen.KeyExpression,
	state fanOutKeyExpansionState,
) (*GraphExpansion, bool) {
	if keyExpressionShapeCount(expression) != 1 {
		return nil, false
	}

	switch {
	case expression.Field != nil:
		return expandFanOutField(expression.Field, state)
	case expression.Then != nil:
		expansions := make([]*GraphExpansion, 0, len(expression.Then.GetChild()))
		for _, child := range expression.Then.GetChild() {
			expansion, ok := expandFanOutKeyExpression(child, state)
			if !ok {
				return nil, false
			}
			expansions = append(expansions, expansion)
		}
		return MergeGraphExpansions(expansions...), true
	case expression.Nesting != nil:
		return expandFanOutNesting(expression.Nesting, state)
	default:
		return nil, false
	}
}

func expandFanOutField(
	field *gen.Field,
	state fanOutKeyExpansionState,
) (*GraphExpansion, bool) {
	if field == nil || field.FieldName == nil || field.FanType == nil {
		return nil, false
	}
	parameterAlias, columnName, ok := state.takeColumn()
	if !ok {
		return nil, false
	}
	physicalFieldName := field.GetFieldName()
	if !strings.EqualFold(columnName, physicalFieldName) {
		// The AST is the physical index authority. A flat metadata column that
		// disagrees with it must not create a placeholder for one field and
		// later execute a scan over another.
		return nil, false
	}
	path := appendPath(state.prefix, strings.ToUpper(physicalFieldName))

	switch field.GetFanType() {
	case gen.Field_SCALAR, gen.Field_CONCATENATE:
		value, _ := fanOutFieldPathValue(
			state.baseAlias,
			state.baseType,
			path,
			false,
		)
		placeholder := predicates.NewPlaceholder(parameterAlias, value)
		return NewGraphExpansion(
			nil,
			[]predicates.QueryPredicate{placeholder},
			nil,
			[]*predicates.Placeholder{placeholder},
		), true

	case gen.Field_FAN_OUT:
		collection, collectionType := fanOutFieldPathValue(
			state.baseAlias,
			state.baseType,
			path,
			true,
		)
		elementType := arrayElementType(collectionType)
		explode := expressions.NewExplodeExpression(collection)
		explodeQuantifier := expressions.ForEachQuantifier(
			expressions.InitialOf(explode),
		)
		elementValue := values.NewQuantifiedObjectValueOfType(
			explodeQuantifier.GetAlias(),
			elementType,
		)
		placeholder := predicates.NewPlaceholder(parameterAlias, elementValue)

		// Java Field/FanOut: the placeholder is a predicate of the inner
		// Select over Explode. The outer expansion inherits only its
		// placeholder metadata and pulls up a ForEach over that Select.
		innerExpansion := NewGraphExpansion(
			[]GraphExpansionColumn{{Value: elementValue}},
			[]predicates.QueryPredicate{placeholder},
			[]expressions.Quantifier{explodeQuantifier},
			[]*predicates.Placeholder{placeholder},
		)
		innerSelect := innerExpansion.Seal().BuildSelect()
		childQuantifier := expressions.ForEachQuantifier(
			expressions.InitialOf(innerSelect),
		)
		return NewGraphExpansion(
			fanOutFlowedColumns(childQuantifier, innerSelect.GetResultValue().Type()),
			nil,
			[]expressions.Quantifier{childQuantifier},
			[]*predicates.Placeholder{placeholder},
		), true

	default:
		return nil, false
	}
}

func expandFanOutNesting(
	nesting *gen.Nesting,
	state fanOutKeyExpansionState,
) (*GraphExpansion, bool) {
	if nesting == nil || nesting.Parent == nil || nesting.Child == nil ||
		nesting.Parent.FieldName == nil || nesting.Parent.FanType == nil {
		return nil, false
	}

	parent := nesting.Parent
	switch parent.GetFanType() {
	case gen.Field_SCALAR:
		// NullableArrayTypeUtils.matchArrayWrapper: field(X).nest("values",
		// FT) — or nest(nesting with "values" as its parent, for a struct
		// element — is the STORED spelling of a wrapped nullable array. The
		// wrapper hop is storage-only (the query side's logical field is the
		// array column X itself), so expansion collapses it back onto the
		// parent and recurses — KeyExpressionExpansionVisitor.java:266-297.
		// The "values" match is exact-case (matchWrappedField's equals).
		var wrapperFanType gen.Field_FanType
		wrapperMatched := false
		childField := nesting.Child.GetField()
		childNesting := nesting.Child.GetNesting()
		switch {
		case childField != nil && childField.GetFieldName() == values.WrappedArrayValuesFieldName:
			wrapperFanType, wrapperMatched = childField.GetFanType(), true
		case childNesting != nil && childNesting.GetParent().GetFieldName() == values.WrappedArrayValuesFieldName:
			wrapperFanType, wrapperMatched = childNesting.GetParent().GetFanType(), true
		}
		if wrapperMatched {
			collapsedParent := &gen.Field{
				FieldName:          parent.FieldName,
				FanType:            wrapperFanType.Enum(),
				NullInterpretation: parent.NullInterpretation,
			}
			switch wrapperFanType {
			case gen.Field_FAN_OUT:
				if childNesting != nil {
					// field(X).nest(values.nest(child)) -> field(X, FAN_OUT).nest(child)
					return expandFanOutNesting(&gen.Nesting{
						Parent: collapsedParent,
						Child:  childNesting.GetChild(),
					}, state)
				}
				return expandFanOutField(collapsedParent, state)
			case gen.Field_CONCATENATE:
				// "values" is always a leaf for a Concatenate wrapper
				// (concatenation produces a list, which has no sub-fields);
				// a nested child here is a malformed candidate — decline
				// (Java: Verify.verify).
				if childNesting != nil {
					return nil, false
				}
				return expandFanOutField(collapsedParent, state)
			default:
				// SCALAR "values" under a wrapper spelling: Java throws
				// "unexpected fan type for array wrapper values field";
				// declining rejects the candidate fail-closed.
				return nil, false
			}
		}
		childState := state
		childState.prefix = appendPath(
			state.prefix,
			strings.ToUpper(parent.GetFieldName()),
		)
		return expandFanOutKeyExpression(nesting.Child, childState)

	case gen.Field_FAN_OUT:
		collectionPath := appendPath(
			state.prefix,
			strings.ToUpper(parent.GetFieldName()),
		)
		collection, collectionType := fanOutFieldPathValue(
			state.baseAlias,
			state.baseType,
			collectionPath,
			true,
		)
		elementType := arrayElementType(collectionType)
		explodeQuantifier := expressions.ForEachQuantifier(
			expressions.InitialOf(expressions.NewExplodeExpression(collection)),
		)

		childState := state
		childState.baseAlias = explodeQuantifier.GetAlias()
		childState.baseType = elementType
		childState.prefix = nil
		childExpansion, ok := expandFanOutKeyExpression(nesting.Child, childState)
		if !ok {
			return nil, false
		}

		// Java's select-star Nesting/FanOut branch pulls the exploded
		// parent's flowed columns and the child expansion into ONE inner
		// Select. A Then child therefore shares this single Explode.
		innerColumns := append(
			fanOutFlowedColumns(explodeQuantifier, elementType),
			childExpansion.GetResultColumns()...,
		)
		innerQuantifiers := make(
			[]expressions.Quantifier,
			0,
			1+len(childExpansion.GetQuantifiers()),
		)
		innerQuantifiers = append(innerQuantifiers, explodeQuantifier)
		innerQuantifiers = append(innerQuantifiers, childExpansion.GetQuantifiers()...)
		innerExpansion := NewGraphExpansion(
			innerColumns,
			childExpansion.GetPredicates(),
			innerQuantifiers,
			childExpansion.GetPlaceholders(),
		)
		innerSelect := innerExpansion.Seal().BuildSelect()
		childQuantifier := expressions.ForEachQuantifier(
			expressions.InitialOf(innerSelect),
		)
		return NewGraphExpansion(
			fanOutFlowedColumns(childQuantifier, innerSelect.GetResultValue().Type()),
			nil,
			[]expressions.Quantifier{childQuantifier},
			childExpansion.GetPlaceholders(),
		), true

	case gen.Field_CONCATENATE:
		return nil, false
	default:
		return nil, false
	}
}

func fanOutFlowedColumns(
	quantifier expressions.Quantifier,
	flowedType values.Type,
) []GraphExpansionColumn {
	typedObject := values.NewQuantifiedObjectValueOfType(
		quantifier.GetAlias(),
		flowedType,
	)
	recordType, ok := flowedType.(*values.RecordType)
	if !ok || len(recordType.Fields) == 0 {
		return []GraphExpansionColumn{{Value: typedObject}}
	}
	columns := make([]GraphExpansionColumn, 0, len(recordType.Fields))
	for ordinal, field := range recordType.Fields {
		fieldValue, err := values.NewFieldValueOfOrdinal(typedObject, ordinal)
		if err != nil {
			return []GraphExpansionColumn{{Value: typedObject}}
		}
		columns = append(columns, GraphExpansionColumn{
			Name:  field.Name,
			Value: fieldValue,
		})
	}
	return columns
}

// fanOutFieldPathValue builds the same positional-root value the SQL lateral
// unnest lowering uses. The top-level record slot is baked by ordinal. For a
// collection path, nested proto-message suffixes remain name-addressed
// (ordinal -1); ordinary scalar paths resolve every available nested ordinal.
func fanOutFieldPathValue(
	baseAlias values.CorrelationIdentifier,
	baseType values.Type,
	path []string,
	collectionPath bool,
) (values.Value, values.Type) {
	if len(path) == 0 {
		return values.NewQuantifiedObjectValueOfType(baseAlias, baseType),
			values.UnknownType
	}

	recordType, ok := baseType.(*values.RecordType)
	if !ok {
		return lazyFanOutFieldPathValue(baseAlias, path), values.UnknownType
	}
	rootOrdinal, rootField, ok := recordTypeField(recordType, path[0])
	if !ok {
		return lazyFanOutFieldPathValue(baseAlias, path), values.UnknownType
	}
	baseObject := values.NewQuantifiedObjectValueOfType(baseAlias, baseType)
	rootValue, err := values.NewFieldValueOfOrdinal(baseObject, rootOrdinal)
	if err != nil {
		return lazyFanOutFieldPathValue(baseAlias, path), values.UnknownType
	}

	leafType := rootField.FieldType
	if len(path) == 1 {
		rootValue.Typ = leafType
		return rootValue, leafType
	}

	suffix := make([]values.ResolvedAccessor, 0, len(path)-1)
	currentType := rootField.FieldType
	for _, segment := range path[1:] {
		accessor := values.ResolvedAccessor{
			Field:   strings.ToUpper(segment),
			Ordinal: -1,
		}
		if nestedType, nested := currentType.(*values.RecordType); nested {
			if ordinal, field, found := recordTypeField(nestedType, segment); found {
				if !collectionPath {
					accessor.Ordinal = ordinal
				}
				accessor.Field = field.Name
				currentType = field.FieldType
				leafType = field.FieldType
			} else {
				currentType = values.UnknownType
				leafType = values.UnknownType
			}
		} else {
			currentType = values.UnknownType
			leafType = values.UnknownType
		}
		suffix = append(suffix, accessor)
	}
	return &values.FieldValue{
		Field: strings.ToUpper(path[len(path)-1]),
		Typ:   leafType,
		Child: rootValue.Child,
		Resolved: rootValue.Resolved.WithSuffix(
			&values.FieldPath{Accessors: suffix},
		),
	}, leafType
}

func lazyFanOutFieldPathValue(
	baseAlias values.CorrelationIdentifier,
	path []string,
) values.Value {
	var value values.Value = values.NewQuantifiedObjectValue(baseAlias)
	for _, segment := range path {
		value = values.NewFieldValue(
			value,
			strings.ToUpper(segment),
			values.UnknownType,
		)
	}
	return value
}

func recordTypeField(
	recordType *values.RecordType,
	name string,
) (int, values.Field, bool) {
	if recordType == nil {
		return 0, values.Field{}, false
	}
	for ordinal, field := range recordType.Fields {
		if strings.EqualFold(field.Name, name) {
			return ordinal, field, true
		}
	}
	return 0, values.Field{}, false
}

func arrayElementType(typ values.Type) values.Type {
	if arrayType, ok := typ.(*values.ArrayType); ok &&
		arrayType.ElementType != nil {
		return arrayType.ElementType
	}
	return values.UnknownType
}

func appendPath(prefix []string, segment string) []string {
	path := make([]string, 0, len(prefix)+1)
	path = append(path, prefix...)
	path = append(path, segment)
	return path
}

func keyExpressionContainsFanOut(expression *gen.KeyExpression) bool {
	if expression == nil {
		return false
	}
	if expression.Field != nil &&
		expression.Field.GetFanType() == gen.Field_FAN_OUT {
		return true
	}
	if expression.Nesting != nil {
		if parent := expression.Nesting.GetParent(); parent != nil &&
			parent.GetFanType() == gen.Field_FAN_OUT {
			return true
		}
		if keyExpressionContainsFanOut(expression.Nesting.GetChild()) {
			return true
		}
	}
	if expression.Then != nil {
		for _, child := range expression.Then.GetChild() {
			if keyExpressionContainsFanOut(child) {
				return true
			}
		}
	}
	if expression.Function != nil &&
		keyExpressionContainsFanOut(expression.Function.GetArguments()) {
		return true
	}
	if expression.Grouping != nil &&
		keyExpressionContainsFanOut(expression.Grouping.GetWholeKey()) {
		return true
	}
	if expression.KeyWithValue != nil &&
		keyExpressionContainsFanOut(expression.KeyWithValue.GetInnerKey()) {
		return true
	}
	if expression.List != nil {
		for _, child := range expression.List.GetChild() {
			if keyExpressionContainsFanOut(child) {
				return true
			}
		}
	}
	if expression.Dimensions != nil &&
		keyExpressionContainsFanOut(expression.Dimensions.GetWholeKey()) {
		return true
	}
	if expression.Split != nil &&
		keyExpressionContainsFanOut(expression.Split.GetJoined()) {
		return true
	}
	return false
}

// keyExpressionContainsNonFanOutNestedLeaf reports whether flattening the
// candidate's column names would erase a scalar nesting path. A nested leaf is
// safe only when it either fans out itself or is below a fan-out parent; in
// both cases the Explode-based expansion gives it structural identity.
//
// The inheritedFanOut bit is intentionally branch-local. A fan-out sibling in
// a Then expression must not make an unrelated scalar nested leaf safe.
func keyExpressionContainsNonFanOutNestedLeaf(
	expression *gen.KeyExpression,
) bool {
	return keyExpressionContainsNonFanOutNestedLeafUnder(
		expression,
		false,
		false,
	)
}

func keyExpressionContainsNonFanOutNestedLeafUnder(
	expression *gen.KeyExpression,
	inheritedFanOut bool,
	underNesting bool,
) bool {
	if expression == nil {
		return false
	}
	if expression.Field != nil {
		return underNesting &&
			!inheritedFanOut &&
			expression.Field.GetFanType() != gen.Field_FAN_OUT
	}
	if expression.Nesting != nil {
		parent := expression.Nesting.GetParent()
		// The NullableArrayWrapper hop collapses before classification
		// (NullableArrayTypeUtils.matchArrayWrapper): field(X).nest("values",
		// FT) IS the array column X at the CURRENT depth, not a nested leaf.
		if parent != nil && parent.GetFanType() == gen.Field_SCALAR {
			child := expression.Nesting.GetChild()
			if cf := child.GetField(); cf != nil &&
				cf.GetFieldName() == values.WrappedArrayValuesFieldName &&
				(cf.GetFanType() == gen.Field_FAN_OUT || cf.GetFanType() == gen.Field_CONCATENATE) {
				collapsed := &gen.KeyExpression{Field: &gen.Field{
					FieldName: parent.FieldName,
					FanType:   cf.GetFanType().Enum(),
				}}
				return keyExpressionContainsNonFanOutNestedLeafUnder(collapsed, inheritedFanOut, underNesting)
			}
			if cn := child.GetNesting(); cn != nil && cn.GetParent() != nil &&
				cn.GetParent().GetFieldName() == values.WrappedArrayValuesFieldName &&
				cn.GetParent().GetFanType() == gen.Field_FAN_OUT {
				collapsed := &gen.KeyExpression{Nesting: &gen.Nesting{
					Parent: &gen.Field{
						FieldName: parent.FieldName,
						FanType:   gen.Field_FAN_OUT.Enum(),
					},
					Child: cn.GetChild(),
				}}
				return keyExpressionContainsNonFanOutNestedLeafUnder(collapsed, inheritedFanOut, underNesting)
			}
		}
		parentFansOut := parent != nil &&
			parent.GetFanType() == gen.Field_FAN_OUT
		return keyExpressionContainsNonFanOutNestedLeafUnder(
			expression.Nesting.GetChild(),
			inheritedFanOut || parentFansOut,
			true,
		)
	}
	if expression.Then != nil {
		for _, child := range expression.Then.GetChild() {
			if keyExpressionContainsNonFanOutNestedLeafUnder(
				child,
				inheritedFanOut,
				underNesting,
			) {
				return true
			}
		}
	}
	if expression.Function != nil &&
		keyExpressionContainsNonFanOutNestedLeafUnder(
			expression.Function.GetArguments(),
			inheritedFanOut,
			underNesting,
		) {
		return true
	}
	if expression.Grouping != nil &&
		keyExpressionContainsNonFanOutNestedLeafUnder(
			expression.Grouping.GetWholeKey(),
			inheritedFanOut,
			underNesting,
		) {
		return true
	}
	if expression.KeyWithValue != nil &&
		keyExpressionContainsNonFanOutNestedLeafUnder(
			expression.KeyWithValue.GetInnerKey(),
			inheritedFanOut,
			underNesting,
		) {
		return true
	}
	if expression.List != nil {
		for _, child := range expression.List.GetChild() {
			if keyExpressionContainsNonFanOutNestedLeafUnder(
				child,
				inheritedFanOut,
				underNesting,
			) {
				return true
			}
		}
	}
	if expression.Dimensions != nil &&
		keyExpressionContainsNonFanOutNestedLeafUnder(
			expression.Dimensions.GetWholeKey(),
			inheritedFanOut,
			underNesting,
		) {
		return true
	}
	if expression.Split != nil &&
		keyExpressionContainsNonFanOutNestedLeafUnder(
			expression.Split.GetJoined(),
			inheritedFanOut,
			underNesting,
		) {
		return true
	}
	return false
}

// keyExpressionTopLevelScalarFieldNames recognizes the only key-expression
// shape that can safely participate in a shortcut which compares bare column
// names instead of candidate Values: one or more top-level SCALAR fields.
// Nesting, functions, concatenation, and fan-out all require semantic or
// structural matching and therefore decline these shortcuts.
func keyExpressionTopLevelScalarFieldNames(
	expression *gen.KeyExpression,
) ([]string, bool) {
	if keyExpressionShapeCount(expression) != 1 {
		return nil, false
	}
	if expression.Field != nil {
		field := expression.Field
		if field.FieldName == nil ||
			field.FanType == nil ||
			field.GetFanType() != gen.Field_SCALAR {
			return nil, false
		}
		return []string{field.GetFieldName()}, true
	}
	if expression.Then != nil {
		var names []string
		for _, child := range expression.Then.GetChild() {
			childNames, ok := keyExpressionTopLevelScalarFieldNames(child)
			if !ok {
				return nil, false
			}
			names = append(names, childNames...)
		}
		if len(names) == 0 {
			return nil, false
		}
		return names, true
	}
	if expression.Version != nil {
		// A VERSION index's version key column behaves exactly like a
		// top-level scalar field of the pseudo-field-extended base type —
		// Java's VersionKeyExpression.toValue is
		// FieldValue.ofFieldName(base, "__ROW_VERSION")
		// (VersionKeyExpression.java:119-121).
		return []string{values.PseudoFieldRowVersion}, true
	}
	if expression.KeyWithValue != nil {
		// A covering root's PHYSICAL entry key is its inner key up to the
		// split point (then the primary key) — plain scalar key columns
		// order the scan exactly as a non-covering index's do; the VALUE
		// part rides in the FDB value and contributes no key ordering. The
		// truncation matches ColumnSize semantics
		// (KeyWithValueExpression.getColumnSize() == split point) at any
		// depth, keeping this list parallel to the column count everywhere
		// the two are compared.
		names, ok := keyExpressionTopLevelScalarFieldNames(
			expression.KeyWithValue.GetInnerKey(),
		)
		if !ok {
			return nil, false
		}
		split := int(expression.KeyWithValue.GetSplitPoint())
		if split < 0 || split > len(names) {
			return nil, false
		}
		return names[:split], true
	}
	return nil, false
}

type flatKeyColumnDescriptor struct {
	name     string
	function string
}

// keyExpressionFlatColumnDescriptors recognizes the non-fan-out key Values the
// flat candidate bridge can model exactly. CARDINALITY over a direct
// CONCATENATE field is the sole function shape currently represented by
// columnFunctions; nullable/nested wrappers remain deliberately unsupported.
func keyExpressionFlatColumnDescriptors(
	expression *gen.KeyExpression,
) ([]flatKeyColumnDescriptor, bool) {
	if keyExpressionShapeCount(expression) != 1 {
		return nil, false
	}
	if expression.Field != nil {
		field := expression.Field
		if field.FieldName == nil ||
			field.FanType == nil ||
			field.GetFanType() != gen.Field_SCALAR {
			return nil, false
		}
		return []flatKeyColumnDescriptor{{name: field.GetFieldName()}}, true
	}
	if expression.Then != nil {
		var descriptors []flatKeyColumnDescriptor
		for _, child := range expression.Then.GetChild() {
			childDescriptors, ok := keyExpressionFlatColumnDescriptors(child)
			if !ok {
				return nil, false
			}
			descriptors = append(descriptors, childDescriptors...)
		}
		if len(descriptors) == 0 {
			return nil, false
		}
		return descriptors, true
	}
	if expression.Function != nil {
		function := expression.Function
		if function.Name == nil {
			return nil, false
		}
		arguments := function.GetArguments()
		if strings.EqualFold(function.GetName(), FunctionKindCardinality) {
			if keyExpressionShapeCount(arguments) != 1 {
				return nil, false
			}
			// Flat spelling — field(col, CONCATENATE) — for a NOT NULL
			// (unwrapped) array column.
			if arguments.Field != nil {
				if arguments.Field.FieldName == nil ||
					arguments.Field.FanType == nil ||
					arguments.Field.GetFanType() != gen.Field_CONCATENATE {
					return nil, false
				}
				return []flatKeyColumnDescriptor{{
					name:     arguments.Field.GetFieldName(),
					function: FunctionKindCardinality,
				}}, true
			}
			// Wrapped spelling — field(col, SCALAR).nest(field("values",
			// CONCATENATE)) — the NullableArrayWrapper hop over a nullable
			// array column; the logical key column is the PARENT
			// (NullableArrayTypeUtils.matchArrayWrapper collapse).
			if n := arguments.GetNesting(); n != nil &&
				n.GetParent().GetFieldName() != "" &&
				n.GetParent().GetFanType() == gen.Field_SCALAR {
				if cf := n.GetChild().GetField(); cf != nil &&
					cf.GetFieldName() == values.WrappedArrayValuesFieldName &&
					cf.GetFanType() == gen.Field_CONCATENATE {
					return []flatKeyColumnDescriptor{{
						name:     n.GetParent().GetFieldName(),
						function: FunctionKindCardinality,
					}}, true
				}
			}
			return nil, false
		}
		if _, isOrder := OrderFunctionDirection(function.GetName()); isOrder {
			// An order-function wrapper over one direct SCALAR field — the
			// shape the index generator emits
			// (OrderFunctionKeyExpressionFactory.java:44-48; single argument,
			// getColumnSize() == 1). Anything else under the wrapper declines.
			if keyExpressionShapeCount(arguments) != 1 ||
				arguments.Field == nil ||
				arguments.Field.FieldName == nil ||
				arguments.Field.FanType == nil ||
				arguments.Field.GetFanType() != gen.Field_SCALAR {
				return nil, false
			}
			return []flatKeyColumnDescriptor{{
				name:     arguments.Field.GetFieldName(),
				function: strings.ToLower(function.GetName()),
			}}, true
		}
		return nil, false
	}
	if expression.Version != nil {
		// The version key column IS the __ROW_VERSION pseudo-field of the
		// extended base type (VersionKeyExpression.toValue,
		// VersionKeyExpression.java:119-121) — a plain scalar column to the
		// flat candidate bridge; the executor materializes it from the
		// Versionstamp key element / the record's version.
		return []flatKeyColumnDescriptor{{name: values.PseudoFieldRowVersion}}, true
	}
	return nil, false
}

// keyExpressionKeyValueColumnDescriptors is keyExpressionFlatColumnDescriptors
// with the covering split applied: a TOP-LEVEL KeyWithValue root yields its
// inner key's descriptors partitioned at the split point into (key, value);
// every other root yields (descriptors, nil). This is the flat-bridge form of
// Java's expansion split (ValueIndexExpansionVisitor.java:109-121: the split
// point partitions the visited columns into keyValues and valueValues).
// A nested KeyWithValue (under Then etc.) is not a shape Java can produce and
// declines through the inner walk having no KeyWithValue arm.
func keyExpressionKeyValueColumnDescriptors(
	expression *gen.KeyExpression,
) (keyPart, valuePart []flatKeyColumnDescriptor, ok bool) {
	if expression != nil && keyExpressionShapeCount(expression) == 1 &&
		expression.KeyWithValue != nil {
		inner := expression.KeyWithValue.GetInnerKey()
		descriptors, innerOK := keyExpressionFlatColumnDescriptors(inner)
		if !innerOK {
			return nil, nil, false
		}
		split := int(expression.KeyWithValue.GetSplitPoint())
		if split < 0 || split > len(descriptors) {
			return nil, nil, false
		}
		return descriptors[:split], descriptors[split:], true
	}
	descriptors, flatOK := keyExpressionFlatColumnDescriptors(expression)
	if !flatOK {
		return nil, nil, false
	}
	return descriptors, nil, true
}

func keyExpressionShapeCount(expression *gen.KeyExpression) int {
	if expression == nil {
		return 0
	}
	count := 0
	if expression.Then != nil {
		count++
	}
	if expression.Nesting != nil {
		count++
	}
	if expression.Field != nil {
		count++
	}
	if expression.Grouping != nil {
		count++
	}
	if expression.Empty != nil {
		count++
	}
	if expression.Split != nil {
		count++
	}
	if expression.Version != nil {
		count++
	}
	if expression.Value != nil {
		count++
	}
	if expression.Function != nil {
		count++
	}
	if expression.KeyWithValue != nil {
		count++
	}
	if expression.RecordTypeKey != nil {
		count++
	}
	if expression.List != nil {
		count++
	}
	if expression.Dimensions != nil {
		count++
	}
	return count
}
