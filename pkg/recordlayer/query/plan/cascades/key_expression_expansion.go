package cascades

import (
	"errors"
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// keyExpansionUnsupportedError is Java's UnsupportedOperationException out of a
// KeyExpressionExpansionVisitor: a key shape no candidate graph models. The
// index is not a candidate (MatchCandidateExpansion catches exactly this).
type keyExpansionUnsupportedError struct {
	Message string
}

func (e *keyExpansionUnsupportedError) Error() string {
	return "key expression expansion: " + e.Message
}

func unsupportedKeyExpansion(format string, args ...any) error {
	return &keyExpansionUnsupportedError{Message: fmt.Sprintf(format, args...)}
}

// keyExpansionColumn describes one registered entry column the way the index
// metadata names it: the field it reads (the parent of a collapsed array
// wrapper, the argument field of a function) and the function wrapping it.
// The name-keyed candidate surfaces are checked against these.
type keyExpansionColumn struct {
	name     string
	function string
	// path is the full field path a field-valued column reads from the base,
	// nil for an exploded element or a column with no field path.
	path []string
	// concatenate marks a plain CONCATENATE field: the whole repeated field as
	// one key column.
	concatenate bool
}

// keyExpansionRegistry is the mutable half of Java's VisitorState: the key and
// value Values every non-internal visit registers, and the sargable aliases
// the key placeholders take in key order.
type keyExpansionRegistry struct {
	aliases       []values.CorrelationIdentifier
	nextAlias     int
	keyValues     []values.Value
	keyColumns    []keyExpansionColumn
	keyDuplicates []bool
	valueValues   []values.Value
	valueColumns  []keyExpansionColumn
}

func (r *keyExpansionRegistry) takeAlias() (values.CorrelationIdentifier, error) {
	if r.nextAlias >= len(r.aliases) {
		return values.CorrelationIdentifier{}, unsupportedKeyExpansion(
			"key has more columns than the candidate's %d sargable aliases", len(r.aliases))
	}
	alias := r.aliases[r.nextAlias]
	r.nextAlias++
	return alias, nil
}

// keyExpansionState is Java's KeyExpressionExpansionVisitor.VisitorState:
// immutable per visit, sharing the registry.
type keyExpansionState struct {
	reg        *keyExpansionRegistry
	base       expressions.Quantifier
	prefix     []string
	splitPoint int
	ordinal    int
	internal   bool
	selectStar bool
	// fanOut marks a visit below a FAN_OUT field: the column it registers is
	// a normalized key that creates duplicates.
	fanOut bool
	// describe walks the columns without a base: nothing is resolved, no graph
	// is built, and every registered Value is nil. The name-keyed candidate
	// surfaces admit a candidate on this walk alone, so they need no base type.
	describe bool
}

func (s keyExpansionState) isKey() bool {
	return s.splitPoint < 0 || s.ordinal < s.splitPoint
}

func (s keyExpansionState) registerValue(value values.Value, column keyExpansionColumn) {
	if s.internal {
		return
	}
	if s.isKey() {
		s.reg.keyValues = append(s.reg.keyValues, value)
		s.reg.keyColumns = append(s.reg.keyColumns, column)
		s.reg.keyDuplicates = append(s.reg.keyDuplicates, s.fanOut)
		return
	}
	s.reg.valueValues = append(s.reg.valueValues, value)
	s.reg.valueColumns = append(s.reg.valueColumns, column)
}

func (s keyExpansionState) placeholderFor(value values.Value) (*predicates.Placeholder, error) {
	alias, err := s.reg.takeAlias()
	if err != nil {
		return nil, err
	}
	return predicates.NewPlaceholder(alias, value), nil
}

// expandKeyExpression dispatches one key expression, as Java's
// KeyExpression.expand does to the visitor.
func expandKeyExpression(expression *gen.KeyExpression, s keyExpansionState) (*GraphExpansion, error) {
	if keyExpressionShapeCount(expression) != 1 {
		return nil, unsupportedKeyExpansion("key expression has no single shape")
	}
	switch {
	case expression.Field != nil:
		return expandKeyField(expression.Field, s)
	case expression.Nesting != nil:
		return expandKeyNesting(expression.Nesting, s)
	case expression.Then != nil:
		return expandKeyThen(expression.Then, s)
	case expression.Function != nil:
		return expandKeyFunction(expression.Function, s)
	case expression.Version != nil:
		return expandKeyVersion(s)
	case expression.Empty != nil:
		return NewGraphExpansion([]GraphExpansionColumn{{Value: values.NewEmptyValue()}}, nil, nil, nil), nil
	case expression.KeyWithValue != nil:
		return nil, unsupportedKeyExpansion("a key with value is expanded at the root only")
	default:
		return nil, unsupportedKeyExpansion("no visitor for this key expression")
	}
}

func resolveKeyFieldPath(base values.Value, path []string) (values.Value, error) {
	requests := make([]values.FieldRequest, len(path))
	for i, segment := range path {
		request, err := values.FieldByName(segment)
		if err != nil {
			return nil, err
		}
		requests[i] = request
	}
	return values.ResolveFieldAccess(base, requests)
}

func expandKeyField(field *gen.Field, s keyExpansionState) (*GraphExpansion, error) {
	if field == nil || field.FieldName == nil || field.FanType == nil {
		return nil, unsupportedKeyExpansion("field without a name or fan type")
	}
	name := field.GetFieldName()
	path := appendPath(s.prefix, name)
	if s.describe {
		return describeKeyField(field, path, s)
	}
	baseObject, err := s.base.RequireFlowedObjectValue()
	if err != nil {
		return nil, err
	}
	switch field.GetFanType() {
	case gen.Field_FAN_OUT:
		collection, err := resolveKeyFieldPath(baseObject, path)
		if err != nil {
			return nil, err
		}
		if _, ok := arrayElementType(collection.Type()); !ok {
			return nil, unsupportedKeyExpansion("fan-out field %s is not an array", name)
		}
		explode, err := expressions.NewExplodeExpression(collection)
		if err != nil {
			return nil, err
		}
		explodeQuantifier := expressions.ForEachQuantifier(expressions.InitialOf(explode))
		element, err := explodeQuantifier.RequireFlowedObjectValue()
		if err != nil {
			return nil, err
		}
		elementState := s
		elementState.fanOut = true
		elementState.registerValue(element, keyExpansionColumn{name: name})
		column := GraphExpansionColumn{Value: element}
		var child *GraphExpansion
		if s.isKey() && !s.internal {
			placeholder, err := s.placeholderFor(element)
			if err != nil {
				return nil, err
			}
			child = NewGraphExpansion([]GraphExpansionColumn{column},
				[]predicates.QueryPredicate{placeholder},
				[]expressions.Quantifier{explodeQuantifier},
				[]*predicates.Placeholder{placeholder})
		} else {
			child = NewGraphExpansion([]GraphExpansionColumn{column}, nil,
				[]expressions.Quantifier{explodeQuantifier}, nil)
		}
		childSelect, err := child.Seal().BuildSelect()
		if err != nil {
			return nil, err
		}
		childQuantifier := expressions.ForEachQuantifier(expressions.InitialOf(childSelect))
		return NewGraphExpansion(
			fanOutFlowedColumns(childQuantifier, childSelect.GetResultValue().Type()),
			nil,
			[]expressions.Quantifier{childQuantifier},
			child.GetPlaceholders(),
		), nil

	case gen.Field_SCALAR, gen.Field_CONCATENATE:
		value, err := resolveKeyFieldPath(baseObject, path)
		if err != nil {
			return nil, err
		}
		s.registerValue(value, keyExpansionColumn{
			name: name, path: path, concatenate: field.GetFanType() == gen.Field_CONCATENATE,
		})
		withPlaceholder := s.isKey() && !s.internal
		var placeholder *predicates.Placeholder
		if withPlaceholder {
			if placeholder, err = s.placeholderFor(value); err != nil {
				return nil, err
			}
		}
		if s.selectStar {
			if withPlaceholder {
				return NewGraphExpansion(nil, []predicates.QueryPredicate{placeholder}, nil,
					[]*predicates.Placeholder{placeholder}), nil
			}
			return EmptyGraphExpansion(), nil
		}
		column := []GraphExpansionColumn{{Value: value}}
		if withPlaceholder {
			return NewGraphExpansion(column, []predicates.QueryPredicate{placeholder}, nil,
				[]*predicates.Placeholder{placeholder}), nil
		}
		return NewGraphExpansion(column, nil, nil, nil), nil

	default:
		return nil, unsupportedKeyExpansion("fan type %v", field.GetFanType())
	}
}

func describeKeyField(field *gen.Field, path []string, s keyExpansionState) (*GraphExpansion, error) {
	column := keyExpansionColumn{name: field.GetFieldName()}
	switch field.GetFanType() {
	case gen.Field_FAN_OUT:
		s.fanOut = true
	case gen.Field_SCALAR, gen.Field_CONCATENATE:
		column.path = path
		column.concatenate = field.GetFanType() == gen.Field_CONCATENATE
	default:
		return nil, unsupportedKeyExpansion("fan type %v", field.GetFanType())
	}
	s.registerValue(nil, column)
	if s.isKey() && !s.internal {
		if _, err := s.reg.takeAlias(); err != nil {
			return nil, err
		}
	}
	return EmptyGraphExpansion(), nil
}

func expandKeyThen(then *gen.Then, s keyExpansionState) (*GraphExpansion, error) {
	ordinal := s.ordinal
	expansions := make([]*GraphExpansion, 0, len(then.GetChild()))
	for _, child := range then.GetChild() {
		childState := s
		childState.ordinal = ordinal
		expansion, err := expandKeyExpression(child, childState)
		if err != nil {
			return nil, err
		}
		size, err := keyExpressionColumnSize(child)
		if err != nil {
			return nil, err
		}
		ordinal += size
		expansions = append(expansions, expansion)
	}
	return MergeGraphExpansions(expansions...), nil
}

// matchArrayWrapper is NullableArrayTypeUtils.matchArrayWrapper: whether a
// SCALAR nesting is the stored spelling of a nullable array column, and the fan
// type of its "values" hop. The match is exact-case, as Java's equals is.
func matchArrayWrapper(nesting *gen.Nesting) (gen.Field_FanType, bool) {
	child := nesting.GetChild()
	if field := child.GetField(); field != nil && field.GetFieldName() == values.WrappedArrayValuesFieldName {
		return field.GetFanType(), true
	}
	if inner := child.GetNesting(); inner != nil && inner.GetParent().GetFieldName() == values.WrappedArrayValuesFieldName {
		return inner.GetParent().GetFanType(), true
	}
	return 0, false
}

func expandKeyNesting(nesting *gen.Nesting, s keyExpansionState) (*GraphExpansion, error) {
	if nesting == nil || nesting.Parent == nil || nesting.Child == nil ||
		nesting.Parent.FieldName == nil || nesting.Parent.FanType == nil {
		return nil, unsupportedKeyExpansion("nesting without a parent or child")
	}
	parent := nesting.Parent
	switch parent.GetFanType() {
	case gen.Field_SCALAR:
		if wrapperFanType, wrapped := matchArrayWrapper(nesting); wrapped {
			collapsed := &gen.Field{
				FieldName:          parent.FieldName,
				FanType:            wrapperFanType.Enum(),
				NullInterpretation: parent.NullInterpretation,
			}
			childNesting := nesting.Child.GetNesting()
			switch wrapperFanType {
			case gen.Field_FAN_OUT:
				if childNesting != nil {
					return expandKeyNesting(&gen.Nesting{Parent: collapsed, Child: childNesting.GetChild()}, s)
				}
				return expandKeyField(collapsed, s)
			case gen.Field_CONCATENATE:
				if childNesting != nil {
					return nil, errors.New(`"values" field in a Concatenate array wrapper must be a leaf`)
				}
				return expandKeyField(collapsed, s)
			default:
				return nil, errors.New("unexpected fan type for array wrapper values field")
			}
		}
		childState := s
		childState.prefix = appendPath(s.prefix, parent.GetFieldName())
		return expandKeyExpression(nesting.Child, childState)

	case gen.Field_FAN_OUT:
		if s.describe {
			childState := s
			childState.prefix = nil
			childState.fanOut = true
			return expandKeyExpression(nesting.Child, childState)
		}
		baseObject, err := s.base.RequireFlowedObjectValue()
		if err != nil {
			return nil, err
		}
		collection, err := resolveKeyFieldPath(baseObject, appendPath(s.prefix, parent.GetFieldName()))
		if err != nil {
			return nil, err
		}
		if _, ok := arrayElementType(collection.Type()); !ok {
			return nil, unsupportedKeyExpansion("fan-out parent %s is not an array", parent.GetFieldName())
		}
		explode, err := expressions.NewExplodeExpression(collection)
		if err != nil {
			return nil, err
		}
		explodeQuantifier := expressions.ForEachQuantifier(expressions.InitialOf(explode))
		childState := s
		childState.base = explodeQuantifier
		childState.prefix = nil
		childState.fanOut = true
		childExpansion, err := expandKeyExpression(nesting.Child, childState)
		if err != nil {
			return nil, err
		}
		if s.selectStar {
			// The exploded parent's flowed columns and the child expansion go
			// into ONE inner Select, so a Then child shares this one Explode.
			elementType, _ := arrayElementType(collection.Type())
			columns := append(fanOutFlowedColumns(explodeQuantifier, elementType),
				childExpansion.GetResultColumns()...)
			quantifiers := append([]expressions.Quantifier{explodeQuantifier},
				childExpansion.GetQuantifiers()...)
			inner := NewGraphExpansion(columns, childExpansion.GetPredicates(), quantifiers,
				childExpansion.GetPlaceholders())
			innerSelect, err := inner.Seal().BuildSelect()
			if err != nil {
				return nil, err
			}
			childQuantifier := expressions.ForEachQuantifier(expressions.InitialOf(innerSelect))
			return NewGraphExpansion(
				fanOutFlowedColumns(childQuantifier, innerSelect.GetResultValue().Type()),
				nil,
				[]expressions.Quantifier{childQuantifier},
				childExpansion.GetPlaceholders(),
			), nil
		}
		// The child's columns and placeholders are pulled up through the new
		// Select so they are defined over its quantifier at this level.
		quantifiers := append([]expressions.Quantifier{explodeQuantifier},
			childExpansion.GetQuantifiers()...)
		inner := NewGraphExpansion(nil, childExpansion.GetPredicates(), quantifiers,
			childExpansion.GetPlaceholders())
		innerSelect, err := inner.Seal().BuildSelect()
		if err != nil {
			return nil, err
		}
		childQuantifier := expressions.ForEachQuantifier(expressions.InitialOf(innerSelect))
		resultValue := innerSelect.GetResultValue()
		pulledColumns := make([]GraphExpansionColumn, 0, len(childExpansion.GetResultColumns()))
		for _, column := range childExpansion.GetResultColumns() {
			pulled, err := values.PullUpValue(column.Value, resultValue, childQuantifier.GetAlias())
			if err != nil {
				return nil, err
			}
			if pulled == nil {
				return nil, fmt.Errorf("could not pull expansion value %v", column.Value)
			}
			pulledColumns = append(pulledColumns, GraphExpansionColumn{Value: pulled})
		}
		pulledPlaceholders := make([]*predicates.Placeholder, 0, len(childExpansion.GetPlaceholders()))
		pulledPredicates := make([]predicates.QueryPredicate, 0, len(childExpansion.GetPlaceholders()))
		for _, placeholder := range childExpansion.GetPlaceholders() {
			pulled, err := values.PullUpValue(placeholder.Value, resultValue, childQuantifier.GetAlias())
			if err != nil {
				return nil, err
			}
			if pulled == nil {
				return nil, fmt.Errorf("could not pull expansion value %v", placeholder.Value)
			}
			pulledPlaceholder := predicates.NewPlaceholder(placeholder.ParameterAlias, pulled)
			pulledPlaceholders = append(pulledPlaceholders, pulledPlaceholder)
			pulledPredicates = append(pulledPredicates, pulledPlaceholder)
		}
		return NewGraphExpansion(pulledColumns, pulledPredicates,
			[]expressions.Quantifier{childQuantifier}, pulledPlaceholders), nil

	default:
		return nil, errors.New("unsupported fan type")
	}
}

// expandKeyFunction expands the arguments without registering them, then
// registers the function's Value (FunctionKeyExpression.toValue).
func expandKeyFunction(function *gen.Function, s keyExpansionState) (*GraphExpansion, error) {
	if function == nil || function.Name == nil || function.Arguments == nil {
		return nil, unsupportedKeyExpansion("function without a name or arguments")
	}
	argumentState := s
	argumentState.internal = true
	argumentState.selectStar = false
	arguments, err := expandKeyExpression(function.Arguments, argumentState)
	if err != nil {
		return nil, err
	}
	argumentSize, err := keyExpressionColumnSize(function.Arguments)
	if err != nil {
		return nil, err
	}
	if s.describe {
		if err := functionKeyHasValue(function.GetName(), argumentSize); err != nil {
			return nil, err
		}
		column := keyExpansionColumn{function: function.GetName()}
		if argument := keyFunctionArgumentField(function.Arguments); argument != "" {
			column.name = argument
			column.path = appendPath(s.prefix, argument)
		}
		s.registerValue(nil, column)
		if s.isKey() && !s.internal {
			if _, err := s.reg.takeAlias(); err != nil {
				return nil, err
			}
		}
		return EmptyGraphExpansion(), nil
	}
	if len(arguments.GetResultColumns()) != argumentSize {
		return nil, fmt.Errorf("function %s expanded %d argument columns, want %d",
			function.GetName(), len(arguments.GetResultColumns()), argumentSize)
	}
	argumentValues := make([]values.Value, len(arguments.GetResultColumns()))
	for i, column := range arguments.GetResultColumns() {
		argumentValues[i] = column.Value
	}
	value, err := functionKeyToValue(function.GetName(), argumentValues)
	if err != nil {
		return nil, err
	}
	column := keyExpansionColumn{function: function.GetName()}
	if argument := keyFunctionArgumentField(function.Arguments); argument != "" {
		column.name = argument
		column.path = appendPath(s.prefix, argument)
	}
	s.registerValue(value, column)
	columns := []GraphExpansionColumn{{Value: value}}
	var preds []predicates.QueryPredicate
	var placeholders []*predicates.Placeholder
	if s.isKey() && !s.internal {
		placeholder, err := s.placeholderFor(value)
		if err != nil {
			return nil, err
		}
		preds = append(preds, placeholder)
		placeholders = append(placeholders, placeholder)
	}
	quantifiers := arguments.GetQuantifiers()
	preds = append(append([]predicates.QueryPredicate(nil), arguments.GetPredicates()...), preds...)
	return NewGraphExpansion(columns, preds, quantifiers, placeholders), nil
}

// keyFunctionArgumentField names the field a single-field function argument
// reads: a direct field, or the parent of a collapsed array wrapper.
func keyFunctionArgumentField(arguments *gen.KeyExpression) string {
	if field := arguments.GetField(); field != nil {
		return field.GetFieldName()
	}
	if nesting := arguments.GetNesting(); nesting != nil {
		if _, wrapped := matchArrayWrapper(nesting); wrapped {
			return nesting.GetParent().GetFieldName()
		}
	}
	return ""
}

// functionKeyHasValue reports whether functionKeyToValue models the function
// over that many argument columns.
func functionKeyHasValue(name string, argumentCount int) error {
	_, isOrder := OrderFunctionDirection(name)
	if name != FunctionKindCardinality && !isOrder {
		return unsupportedKeyExpansion("function key %s has no value", name)
	}
	if argumentCount != 1 {
		return unsupportedKeyExpansion("%s takes one argument, got %d", name, argumentCount)
	}
	return nil
}

// functionKeyToValue is FunctionKeyExpression.toValue for the function keys
// Go models; any other function has no Value and declines the candidate.
func functionKeyToValue(name string, arguments []values.Value) (values.Value, error) {
	if err := functionKeyHasValue(name, len(arguments)); err != nil {
		return nil, err
	}
	if direction, isOrder := OrderFunctionDirection(name); isOrder {
		return values.NewToOrderedBytesValue(arguments[0], direction), nil
	}
	return values.NewCardinalityValue(arguments[0]), nil
}

// expandKeyVersion is VersionKeyExpression.toValue: the __ROW_VERSION
// pseudo-field of the base.
func expandKeyVersion(s keyExpansionState) (*GraphExpansion, error) {
	if s.describe {
		s.registerValue(nil, keyExpansionColumn{name: values.PseudoFieldRowVersion, path: []string{values.PseudoFieldRowVersion}})
		if s.isKey() && !s.internal {
			if _, err := s.reg.takeAlias(); err != nil {
				return nil, err
			}
		}
		return EmptyGraphExpansion(), nil
	}
	baseObject, err := s.base.RequireFlowedObjectValue()
	if err != nil {
		return nil, err
	}
	value, err := resolveKeyFieldPath(baseObject, []string{values.PseudoFieldRowVersion})
	if err != nil {
		return nil, err
	}
	s.registerValue(value, keyExpansionColumn{name: values.PseudoFieldRowVersion, path: []string{values.PseudoFieldRowVersion}})
	columns := []GraphExpansionColumn{{Value: value}}
	if s.isKey() && !s.internal {
		placeholder, err := s.placeholderFor(value)
		if err != nil {
			return nil, err
		}
		return NewGraphExpansion(columns, []predicates.QueryPredicate{placeholder}, nil,
			[]*predicates.Placeholder{placeholder}), nil
	}
	return NewGraphExpansion(columns, nil, nil, nil), nil
}

// keyExpressionColumnSize is KeyExpression.getColumnSize for the shapes the
// expansion visits.
func keyExpressionColumnSize(expression *gen.KeyExpression) (int, error) {
	if keyExpressionShapeCount(expression) != 1 {
		return 0, unsupportedKeyExpansion("key expression has no single shape")
	}
	switch {
	case expression.Field != nil, expression.Function != nil, expression.Version != nil,
		expression.Value != nil, expression.RecordTypeKey != nil:
		return 1, nil
	case expression.Nesting != nil:
		return keyExpressionColumnSize(expression.Nesting.GetChild())
	case expression.Then != nil:
		size := 0
		for _, child := range expression.Then.GetChild() {
			childSize, err := keyExpressionColumnSize(child)
			if err != nil {
				return 0, err
			}
			size += childSize
		}
		return size, nil
	case expression.List != nil:
		return len(expression.List.GetChild()), nil
	case expression.Empty != nil:
		return 0, nil
	case expression.KeyWithValue != nil:
		return int(expression.KeyWithValue.GetSplitPoint()), nil
	default:
		return 0, unsupportedKeyExpansion("no column size for this key expression")
	}
}

// valueIndexExpansion is what expanding a value index produces
// (ValueIndexExpansionVisitor.expand): the candidate graph and the Values of
// the entry's key and value columns over the base quantifier.
type valueIndexExpansion struct {
	traversal     *Traversal
	base          values.QuantifiedObjectValue
	keyValues     []values.Value
	keyColumns    []keyExpansionColumn
	keyDuplicates []bool
	valueValues   []values.Value
	valueColumns  []keyExpansionColumn
}

// splitKeyWithValue unwraps a KeyWithValue root into its inner key and split
// point; any other root has none (-1).
func splitKeyWithValue(root *gen.KeyExpression) (*gen.KeyExpression, int) {
	if root != nil && keyExpressionShapeCount(root) == 1 && root.KeyWithValue != nil {
		return root.KeyWithValue.GetInnerKey(), int(root.KeyWithValue.GetSplitPoint())
	}
	return root, -1
}

func checkAllAliasesTaken(registry *keyExpansionRegistry) error {
	if registry.nextAlias != len(registry.aliases) {
		return unsupportedKeyExpansion("key has %d placeholders for %d sargable aliases",
			registry.nextAlias, len(registry.aliases))
	}
	return nil
}

// describeValueIndexRoot walks a value index's columns without a base (see
// keyExpansionState.describe): their names, functions, paths and fan-out,
// in key and value order. The traversal and Values are nil.
func describeValueIndexRoot(candidate MatchCandidate, root *gen.KeyExpression) (*valueIndexExpansion, error) {
	inner, splitPoint := splitKeyWithValue(root)
	registry := &keyExpansionRegistry{aliases: candidate.GetSargableAliases()}
	if _, err := expandKeyExpression(inner, keyExpansionState{
		reg:        registry,
		splitPoint: splitPoint,
		selectStar: true,
		describe:   true,
	}); err != nil {
		return nil, err
	}
	if err := checkAllAliasesTaken(registry); err != nil {
		return nil, err
	}
	return &valueIndexExpansion{
		keyColumns:    registry.keyColumns,
		keyDuplicates: registry.keyDuplicates,
		valueColumns:  registry.valueColumns,
	}, nil
}

// expandValueIndexRoot is ValueIndexExpansionVisitor.expand over a value
// index's root: the base scan, the key and value columns of a KeyWithValue
// split, and a sparse index's predicate. The primary key stays out of the
// sargable surface (GetPKColumnNames).
func expandValueIndexRoot(
	candidate MatchCandidate,
	root *gen.KeyExpression,
	predicateProto *gen.Predicate,
) (*valueIndexExpansion, error) {
	baseType, ok := candidateBaseType(candidate)
	if !ok {
		return nil, unsupportedKeyExpansion("candidate has no exact base type")
	}
	scan, err := expressions.NewFullUnorderedScanExpression(candidate.GetRecordTypes(), baseType)
	if err != nil {
		return nil, err
	}
	baseQuantifier := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	baseObject, err := baseQuantifier.RequireFlowedObjectValue()
	if err != nil {
		return nil, err
	}

	inner, splitPoint := splitKeyWithValue(root)
	registry := &keyExpansionRegistry{aliases: candidate.GetSargableAliases()}
	keyExpansion, err := expandKeyExpression(inner, keyExpansionState{
		reg:        registry,
		base:       baseQuantifier,
		splitPoint: splitPoint,
		selectStar: true,
	})
	if err != nil {
		return nil, err
	}
	if err := checkAllAliasesTaken(registry); err != nil {
		return nil, err
	}

	quantifiers := append([]expressions.Quantifier{baseQuantifier}, keyExpansion.GetQuantifiers()...)
	preds := append([]predicates.QueryPredicate(nil), keyExpansion.GetPredicates()...)
	if predicateProto != nil {
		converted, err := indexPredicateToQueryPredicate(predicateProto, baseObject)
		if err != nil {
			return nil, err
		}
		if !predicates.IsTautology(converted) {
			preds = append(preds, converted)
		}
	}
	complete := NewGraphExpansion(nil, preds, quantifiers, keyExpansion.GetPlaceholders())
	selectExpression, err := complete.Seal().BuildSelectWithResultValue(baseObject)
	if err != nil {
		return nil, err
	}
	matchableSort, err := expressions.NewMatchableSortExpressionFromExpr(
		candidate.GetSargableAliases(), false, selectExpression)
	if err != nil {
		return nil, err
	}
	return &valueIndexExpansion{
		traversal:     NewTraversal(expressions.InitialOf(matchableSort)),
		base:          baseObject,
		keyValues:     registry.keyValues,
		keyColumns:    registry.keyColumns,
		keyDuplicates: registry.keyDuplicates,
		valueValues:   registry.valueValues,
		valueColumns:  registry.valueColumns,
	}, nil
}

// flatColumnsRootKeyExpression spells a list of top-level columns as the key
// expression they are: one SCALAR field per column, with a CARDINALITY or
// order function where the column carries one.
func flatColumnsRootKeyExpression(columns, functions []string) *gen.KeyExpression {
	children := make([]*gen.KeyExpression, len(columns))
	for i, column := range columns {
		function := ""
		if i < len(functions) {
			function = functions[i]
		}
		switch {
		case function == FunctionKindCardinality:
			children[i] = &gen.KeyExpression{Function: &gen.Function{
				Name:      &function,
				Arguments: flatColumnField(column, gen.Field_CONCATENATE),
			}}
		case function != "":
			children[i] = &gen.KeyExpression{Function: &gen.Function{
				Name:      &function,
				Arguments: flatColumnField(column, gen.Field_SCALAR),
			}}
		default:
			children[i] = flatColumnField(column, gen.Field_SCALAR)
		}
	}
	return &gen.KeyExpression{Then: &gen.Then{Child: children}}
}

func flatColumnField(name string, fanType gen.Field_FanType) *gen.KeyExpression {
	return &gen.KeyExpression{Field: &gen.Field{FieldName: &name, FanType: fanType.Enum()}}
}

// resolveUpperFieldPath resolves a metadata field path against the record the
// base flows, each step by its unique case-insensitive name; nil when a step
// does not resolve.
func resolveUpperFieldPath(base values.QuantifiedObjectValue, path []string) values.Value {
	record, _ := base.FlowedType().(*values.RecordType)
	ordinals := make([]int, len(path))
	for i, name := range path {
		if record == nil {
			return nil
		}
		ordinal, unique := uniqueUpperFieldIndex(record, name)
		if !unique {
			return nil
		}
		ordinals[i] = ordinal
		record, _ = record.Fields[ordinal].FieldType.(*values.RecordType)
	}
	resolved, err := values.ResolveFieldOrdinals(base, ordinals)
	if err != nil {
		return nil
	}
	return resolved
}
