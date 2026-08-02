package ddl

// The aggregate arm of the materialized-view index generator — the Go port of
// MaterializedViewIndexGenerator's aggregate path (tag 4.12.11.0):
// collectResultValues (:249-299), adjustGroupByFieldPaths (:302-327, a no-op
// over Go's already-source-resolved grouping values — see collectAggregate),
// the ordering/permuted logic (:204-244) and
// generateAggregateIndexKeyExpression (:397-465) with removeBitmapBucketOffset
// (:470-495).
//
// Correspondence: Java partitions the flattened result value into
// IndexableAggregateValues and field values. Go's logical plan carries the
// same information structurally: LogicalAggregate.Calls (the aggregates, with
// AggregateOperands the source-resolved operand values), GroupKeys (the
// grouping values, source-resolved), and OutputSlots (the SELECT list in
// order, each addressing a group key or a call). The projection above the
// aggregate reads the aggregate's OUTPUT row by name, so its ProjectedValues
// are not source-resolved and are used only for sort-key identity mapping.

import (
	"fmt"
	"strconv"
	"strings"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/query/logical"
)

// aggOrderRef is one resolved ORDER BY entry of an aggregate index
// definition: either the single aggregate or the native ordinal of a grouping
// value.
type aggOrderRef struct {
	isAgg  bool
	native int // grouping ordinal when !isAgg
}

func generateAggregate(d *decomposed, opts Options, res storageNames) (*GeneratedIndex, error) {
	agg := d.aggregate
	if agg.HasHaving {
		// HAVING becomes an index predicate in Java (getTopLevelPredicate
		// tolerates the select-having sandwich); fail closed until the
		// predicate arm (RFC-202 S5) rather than drop it.
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"Unsupported index definition, HAVING is not yet supported in an index definition")
	}
	if agg.HasDistinctAggregate {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"Unsupported index definition, DISTINCT aggregate is not supported in an index definition")
	}

	groupVals, err := groupingValues(agg)
	if err != nil {
		return nil, err
	}

	// Java's phase order: the ORDER-BY ⊆ projection rejection fires at
	// DdlVisitor.java:217, BEFORE generator.generate() and therefore before
	// the multi-aggregation and alignment checks —
	// createAggregateIndexOnMinMaxWithGroupingColumnsMissingInResultColumn
	// pins INVALID_COLUMN_REFERENCE for a shape that would also fail
	// alignment.
	orderRefs, orderingFns, err := aggregateOrderRefs(d, groupVals)
	if err != nil {
		return nil, err
	}

	// checkValidity (:619-623): at most one aggregation inside the group by.
	// IndexTest.java:773-779 pins the message.
	if len(agg.Calls) > 1 {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"Unsupported index definition, found group by expression with more than one aggregation")
	}

	if err := validateSelectGroupingAlignment(agg); err != nil {
		return nil, err
	}

	if len(agg.Calls) == 0 {
		// GROUP BY with no aggregate: Java's aggregateValues comes out empty
		// and the VALUE arm runs over the grouping values (generate :187).
		orderBy := make([]values.Value, 0, len(orderRefs))
		for _, ref := range orderRefs {
			if ref.isAgg {
				return nil, api.NewError(api.ErrCodeInternalError,
					"index generator: aggregate order reference without an aggregate call")
			}
			orderBy = append(orderBy, groupVals[ref.native])
		}
		return buildValueIndex(d.scan.Table, groupVals, orderBy, orderingFns, res)
	}

	call := agg.Calls[0]
	var operand values.Value
	if len(agg.AggregateOperands) > 0 {
		operand = agg.AggregateOperands[0]
	}
	if !call.Star && operand == nil {
		// The operand resolver is fail-soft for queries (the translator has a
		// lazy fallback); the index generator must not build from an
		// unresolved operand.
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"Unsupported index definition, cannot resolve aggregate operand %q", call.Operand)
	}

	// Java :204-231: the aggregate may appear at most once in the ORDER BY,
	// and the grouping columns must otherwise appear as an exact in-order
	// prefix-complete sequence; anything else is an attempted covering
	// aggregate index.
	aggOrderIndex := -1
	if len(orderRefs) > 0 {
		nextField := 0
		inOrder := true
		for i, ref := range orderRefs {
			if ref.isAgg {
				if aggOrderIndex >= 0 {
					return nil, api.NewError(api.ErrCodeUnsupportedOperation,
						"Unsupported index definition, aggregate can appear only once in ordering clause")
				}
				aggOrderIndex = i
			} else if nextField < len(groupVals) {
				if ref.native != nextField {
					inOrder = false
					break
				}
				nextField++
			} else {
				inOrder = false
				break
			}
		}
		if nextField < len(groupVals) || !inOrder {
			return nil, api.NewError(api.ErrCodeUnsupportedOperation,
				"Unsupported index definition, attempt to create a covering aggregate index")
		}
	}

	// Grouping key expression (Java :233): the grouping values through the
	// SAME expression builder as the value arm — trie compression, arithmetic
	// leaves and ordering wrappers included.
	var groupingExpr recordlayer.KeyExpression
	if len(groupVals) > 0 {
		groupingExpr, err = generateKeyExpression(groupVals, orderingFns, res)
		if err != nil {
			return nil, err
		}
	}

	root, indexType, err := aggregateKeyExpression(call, operand, groupingExpr, opts, res)
	if err != nil {
		return nil, err
	}

	gi := &GeneratedIndex{
		TableName: d.scan.Table,
		Root:      root,
		IndexType: indexType,
	}
	// Java :237-243: permuted min/max carry the permuted size; every other
	// aggregate type refuses an aggregate-ordered key. aggOrderIndex == 0 is
	// admitted for non-permuted types exactly as in Java (`aggregateOrderIndex
	// > 0`), where the ordering is a no-op prefix.
	switch indexType {
	case recordlayer.IndexTypePermutedMin, recordlayer.IndexTypePermutedMax:
		permutedSize := 0
		if aggOrderIndex >= 0 {
			permutedSize = len(groupVals) - aggOrderIndex
		}
		gi.Options = map[string]string{
			recordlayer.IndexOptionPermutedSize: strconv.Itoa(permutedSize),
		}
	default:
		if aggOrderIndex > 0 {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"Unsupported index definition. Cannot order %s index by aggregate value", indexType)
		}
	}
	return gi, nil
}

// groupingValues extracts the source-resolved grouping values in GROUP BY
// order; a value the walker declined is a hard error, never a silent skip.
//
// adjustGroupByFieldPaths (:302-327) strips the select-where quantifier's
// root from Java's field paths so a key expression can be built; Go's
// GroupKey.Value / AggregateOperands are resolved against the base table
// scope directly (single-rooted accessor paths), so there is no root to
// strip and the adjustment is structural identity here.
func groupingValues(agg *logical.LogicalAggregate) ([]values.Value, error) {
	groupVals := make([]values.Value, len(agg.GroupKeys))
	for i, gk := range agg.GroupKeys {
		if gk.Value == nil {
			// A grouping expression the walker declined — silently treating
			// it as "present" is how the wrong index gets built.
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"Unsupported index definition, cannot map group by expression %q to a key expression", gk.Display)
		}
		groupVals[i] = gk.Value
	}
	return groupVals, nil
}

// validateSelectGroupingAlignment is the Go form of collectResultValues
// (:249-299): the SELECT list must be consistent with the grouping.
//
// Java reconciles the flattened result value against the group-by expression's
// grouping value; Go reconciles OutputSlots (the SELECT list, each slot naming
// a native ordinal into [group keys..., calls...]) against GroupKeys. A
// single-aggregation SELECT adopts the grouping values wholesale (Java
// :259-270); a multi-element SELECT must list them exactly in grouping order
// (:272-297).
func validateSelectGroupingAlignment(agg *logical.LogicalAggregate) error {
	// A slot with native ordinal -1 is an expression the front end could not
	// classify (a computed post-aggregate item, or an unrecognized call);
	// Java's value-kind assert (:180) rejects those before any arm runs.
	isSingleAggregation := len(agg.OutputSlots) == 1 && len(agg.Calls) == 1 &&
		agg.OutputSlots[0].NativeOrdinal == len(agg.GroupKeys)
	if isSingleAggregation || len(agg.OutputSlots) == 0 {
		return nil
	}

	nonAggNatives := make([]int, 0, len(agg.OutputSlots))
	for _, slot := range agg.OutputSlots {
		if slot.NativeOrdinal < 0 {
			return api.NewError(api.ErrCodeUnsupportedOperation,
				"Unsupported index definition, cannot map select element to a key expression")
		}
		if slot.NativeOrdinal >= len(agg.GroupKeys) {
			continue // the aggregate call
		}
		nonAggNatives = append(nonAggNatives, slot.NativeOrdinal)
	}
	// Java :272-297, message per direction (IndexTest.java pins each):
	for i, native := range nonAggNatives {
		if i >= len(agg.GroupKeys) {
			return api.NewError(api.ErrCodeUnsupportedOperation,
				"Aggregate result value contains values missing from the grouping expression")
		}
		if native != i {
			return api.NewError(api.ErrCodeUnsupportedOperation,
				"Aggregate result value does not align with grouping value")
		}
	}
	if len(nonAggNatives) < len(agg.GroupKeys) {
		return api.NewError(api.ErrCodeUnsupportedOperation,
			"Grouping value absent from aggregate result value")
	}
	return nil
}

// aggregateOrderRefs maps the sort keys of an aggregate index definition to
// (grouping ordinal | aggregate) references, plus the per-grouping-value
// ordering-function map (getOrderByValues, :339-385, over the aggregate
// plan's projection).
//
// The sort keys carry the PROJECTION's value instances (the projection above
// an aggregate reads the aggregate's output row, and the sort resolved
// against it), so identity against d.project.ProjectedValues recovers the
// select ordinal, and OutputSlots translate it to the native ordinal. A sort
// key that maps to no projection slot is Java's not-in-projection rejection
// (DdlVisitor.java:217).
func aggregateOrderRefs(d *decomposed, groupVals []values.Value) ([]aggOrderRef, map[values.Value]string, error) {
	fns := map[values.Value]string{}
	if d.sort == nil {
		return nil, fns, nil
	}
	agg := d.aggregate
	if d.project == nil {
		return nil, nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"Unsupported index definition, cannot resolve ORDER BY over an aggregate without a projection")
	}
	refs := make([]aggOrderRef, 0, len(d.sort.Keys))
	for _, k := range d.sort.Keys {
		selectOrdinal := -1
		switch {
		case k.Pos > 0:
			if k.Pos > len(d.project.ProjectedValues) {
				return nil, nil, api.NewErrorf(api.ErrCodeInvalidColumnReference,
					"ORDER BY position %d is not in the select list", k.Pos)
			}
			selectOrdinal = k.Pos
		case k.Value != nil:
			for j, pv := range d.project.ProjectedValues {
				if pv != nil && pv == k.Value {
					selectOrdinal = j + 1
					break
				}
			}
		}
		if selectOrdinal < 0 {
			return nil, nil, api.NewError(api.ErrCodeInvalidColumnReference,
				"Cannot create index and order by an expression that is not present in the projection list")
		}
		native := -1
		for _, slot := range agg.OutputSlots {
			if slot.SelectOrdinal == selectOrdinal {
				native = slot.NativeOrdinal
				break
			}
		}
		if native < 0 {
			return nil, nil, api.NewError(api.ErrCodeInvalidColumnReference,
				"Cannot create index and order by an expression that is not present in the projection list")
		}
		if native >= len(agg.GroupKeys) {
			refs = append(refs, aggOrderRef{isAgg: true})
			continue
		}
		refs = append(refs, aggOrderRef{native: native})
		if fn := sortOrderFunction(k); fn != "" {
			// Key the ordering function on the GROUPING value instance —
			// generateKeyExpression consumes groupVals, and the map is
			// identity-keyed (Java's IdentityHashMap, RFC-202 D12).
			fns[groupVals[native]] = fn
		}
	}
	return refs, fns, nil
}

// aggregateKeyExpression is generateAggregateIndexKeyExpression (:397-465):
// the grouped/grouping split per aggregate kind, plus the extremum-ever
// index-type rewrite.
func aggregateKeyExpression(call logical.AggregateCall, operand values.Value,
	groupingExpr recordlayer.KeyExpression, opts Options, res storageNames,
) (recordlayer.KeyExpression, string, error) {
	fn := strings.ToUpper(call.Func)

	// COUNT(*) is a special case (:407-413): the whole key is the grouping,
	// with zero grouped columns — never an Empty child inside the concat.
	if fn == "COUNT" && call.Star {
		if groupingExpr == nil {
			return recordlayer.GroupAll(recordlayer.EmptyKey()), recordlayer.IndexTypeCount, nil
		}
		return recordlayer.GroupAll(groupingExpr), recordlayer.IndexTypeCount, nil
	}

	// AVG is streamable but not indexable (:176-178).
	if fn == "AVG" {
		return nil, "", api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"Unsupported aggregate index definition containing non-indexable aggregation (%s), consider using a value index on the aggregated column instead.",
			strings.ToLower(call.Func)+"("+call.Operand+")")
	}

	if fn == "BITMAP_CONSTRUCT_AGG" {
		return bitmapKeyExpression(operand, groupingExpr, res)
	}

	// Generic arm (:434-446): the operand must be a plain column.
	child, ok := operand.(*values.FieldValue)
	if !ok {
		return nil, "", api.NewError(api.ErrCodeUnsupportedOperation,
			"Unsupported index definition, expecting a column argument in aggregation function")
	}
	groupedValue, err := generateKeyExpression([]values.Value{child}, nil, res)
	if err != nil {
		return nil, "", err
	}
	switch groupedValue.(type) {
	case *recordlayer.FieldKeyExpression, *recordlayer.CompositeKeyExpression:
		// Java :437 asserts FieldKeyExpression or ThenKeyExpression.
	default:
		return nil, "", api.NewErrorf(api.ErrCodeInternalError,
			"index generator: aggregate operand built a %T key expression", groupedValue)
	}

	indexType, err := aggregateIndexType(fn, operand, opts)
	if err != nil {
		return nil, "", err
	}
	if groupingExpr == nil {
		return recordlayer.Ungrouped(groupedValue), indexType, nil
	}
	return recordlayer.GroupBy(groupedValue, groupingExpr), indexType, nil
}

// bitmapKeyExpression is the BITMAP_CONSTRUCT_AGG arm (:414-432): only
// bitmap_construct_agg(bitmap_bit_position(column)) is supported, the raw
// column becomes the grouped value, and the trailing bitmap_bucket_offset
// grouping element is removed (removeBitmapBucketOffset, :470-495).
func bitmapKeyExpression(operand values.Value, groupingExpr recordlayer.KeyExpression, res storageNames) (recordlayer.KeyExpression, string, error) {
	groupedValue, err := leafKeyExpression(operand, nil, res)
	if err != nil {
		return nil, "", err
	}
	fnExpr, ok := groupedValue.(*recordlayer.FunctionKeyExpression)
	if !ok || fnExpr.Name() != "bitmap_bit_position" {
		return nil, "", api.NewError(api.ErrCodeUnsupportedOperation,
			"Unsupported index definition, expecting a bitmap_bit_position function in bitmap_construct_agg function")
	}
	// Java :421: the grouped column is the first child of the function's
	// argument concat (the second is the injected entry-size literal), cast
	// to FieldKeyExpression.
	args, ok := fnExpr.Arguments().(*recordlayer.CompositeKeyExpression)
	if !ok || len(args.SubKeyExpressions()) == 0 {
		return nil, "", api.NewError(api.ErrCodeInternalError,
			"index generator: bitmap_bit_position without argument concat")
	}
	groupedColumn, ok := args.SubKeyExpressions()[0].(*recordlayer.FieldKeyExpression)
	if !ok {
		return nil, "", api.NewError(api.ErrCodeUnsupportedOperation,
			"Unsupported index definition, expecting a column argument in aggregation function")
	}
	if groupingExpr == nil {
		// Java :429-431: a bitmap index without a grouping expression is
		// rejected — the bucket offset must be grouped on.
		return nil, "", api.NewError(api.ErrCodeInternalError,
			fmt.Sprintf("Unsupported index definition, unexpected grouping expression %v", groupedValue))
	}
	afterRemove, err := removeBitmapBucketOffset(groupingExpr)
	if err != nil {
		return nil, "", err
	}
	if afterRemove == nil {
		return recordlayer.Ungrouped(groupedColumn), recordlayer.IndexTypeBitmapValue, nil
	}
	return recordlayer.GroupBy(groupedColumn, afterRemove), recordlayer.IndexTypeBitmapValue, nil
}

// removeBitmapBucketOffset removes the trailing bitmap_bucket_offset(col)
// element from the grouping expression; nil when the grouping was ONLY the
// bucket offset (:470-495).
func removeBitmapBucketOffset(groupingExpr recordlayer.KeyExpression) (recordlayer.KeyExpression, error) {
	switch g := groupingExpr.(type) {
	case *recordlayer.CompositeKeyExpression:
		children := g.SubKeyExpressions()
		last, ok := children[len(children)-1].(*recordlayer.FunctionKeyExpression)
		if !ok || last.Name() != "bitmap_bucket_offset" {
			return nil, api.NewError(api.ErrCodeUnsupportedOperation,
				"Unsupported index definition, expecting the last element in group by to be a bitmap_bucket_offset function")
		}
		if len(children) >= 3 {
			return recordlayer.Concat(children[:len(children)-1]...), nil
		}
		return children[0], nil
	case *recordlayer.FunctionKeyExpression:
		if g.Name() == "bitmap_bucket_offset" {
			return nil, nil
		}
		return g, nil
	default:
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"Unsupported index definition, expecting column or function arguments in group by")
	}
}

// aggregateIndexType maps the aggregate function to Java's index type name:
// CountValue → COUNT / COUNT_NOT_NULL, Sum → SUM, Min/Max → PERMUTED_MIN /
// PERMUTED_MAX (NumericAggregationValue.java:238, :307, :439, :508), and the
// extremum family through the LEGACY_EXTREMUM_EVER rewrite (:449-465): the
// LONG-based maintainer needs a numeric operand (Verify → internal error,
// IndexTest.java:1141-1176), the tuple-based one takes any type.
func aggregateIndexType(fn string, operand values.Value, opts Options) (string, error) {
	switch fn {
	case "COUNT":
		return recordlayer.IndexTypeCountNotNull, nil
	case "SUM":
		return recordlayer.IndexTypeSum, nil
	case "MIN":
		return recordlayer.IndexTypePermutedMin, nil
	case "MAX":
		return recordlayer.IndexTypePermutedMax, nil
	case "MAX_EVER", "MIN_EVER":
		if !opts.UseLegacyExtremumEver {
			if fn == "MAX_EVER" {
				return recordlayer.IndexTypeMaxEverTuple, nil
			}
			return recordlayer.IndexTypeMinEverTuple, nil
		}
		longType := recordlayer.IndexTypeMaxEverLong
		if fn == "MIN_EVER" {
			longType = recordlayer.IndexTypeMinEverLong
		}
		if !valueIsNumeric(operand) {
			return "", api.NewErrorf(api.ErrCodeInternalError,
				"only numeric types allowed in %s aggregation operation", longType)
		}
		return longType, nil
	default:
		return "", api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"Unsupported aggregate index definition containing non-indexable aggregation (%s), consider using a value index on the aggregated column instead.",
			strings.ToLower(fn))
	}
}

// valueIsNumeric mirrors Java Type.isNumeric for the extremum-ever operand
// check.
func valueIsNumeric(v values.Value) bool {
	if v == nil || v.Type() == nil {
		return false
	}
	switch v.Type().Code() {
	case values.TypeCodeInt, values.TypeCodeLong, values.TypeCodeFloat, values.TypeCodeDouble:
		return true
	default:
		return false
	}
}
