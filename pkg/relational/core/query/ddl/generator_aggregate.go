// Portions derived from FoundationDB Record Layer (
// MaterializedViewIndexGenerator.java, ValueToKeyExpressionVisitor.java,
// IndexSpec.java, FieldKeyExpression.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2026 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package ddl

// The aggregate arm of the materialized-view index generator — the aggregate
// half of Java 4.14.2.0's MaterializedViewIndexGenerator and
// ValueToKeyExpressionVisitor over the resolved projection IndexSpec collects.

import (
	"fmt"
	"strconv"
	"strings"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
)

// generateAggregate is the aggregate arm of generate() (Java 4.14.2.0,
// MaterializedViewIndexGenerator.java:95-117 with translateToKeyExpression and
// addAggregatePermutationOptions): the projection in its own order — the
// grouping columns with NO ordering functions (an aggregate index takes
// Map.of(), whatever directions its ORDER BY names), then the aggregate — and,
// for a permuted index, the permuted size its ordering implies.
func generateAggregate(c *specCollector, spec *indexSpec, opts Options, res storageNames) (*GeneratedIndex, error) {
	aggregate := aggregateOf(spec.projection)
	grouping := fieldValuesOf(spec.projection)
	var groupingExpr recordlayer.KeyExpression
	if len(grouping) > 0 {
		var err error
		if groupingExpr, err = generateKeyExpression(grouping, nil, res); err != nil {
			return nil, err
		}
	}
	root, indexType, err := aggregateKeyExpression(aggregate, groupingExpr, opts, res)
	if err != nil {
		return nil, err
	}
	gi := &GeneratedIndex{TableName: spec.recordType, Root: root, IndexType: indexType}
	aggOrderIndex, err := c.aggregateOrderIndex(spec)
	if err != nil {
		return nil, err
	}
	switch indexType {
	case recordlayer.IndexTypePermutedMin, recordlayer.IndexTypePermutedMax:
		permutedSize := 0
		if aggOrderIndex >= 0 {
			permutedSize = len(grouping) - aggOrderIndex
		}
		gi.Options = map[string]string{recordlayer.IndexOptionPermutedSize: strconv.Itoa(permutedSize)}
	default:
		if aggOrderIndex > 0 {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"Unsupported index definition. Cannot order %s index by aggregate value", indexType)
		}
	}
	return gi, nil
}

// aggregateKeyExpression is the aggregate half of ValueToKeyExpressionVisitor:
// COUNT(*) groups everything and aggregates nothing; BITMAP_CONSTRUCT_AGG takes
// the column under bitmap_bit_position; every other aggregate groups a plain
// column by the grouping columns (groupedAggregate).
func aggregateKeyExpression(aggregate values.Value, groupingExpr recordlayer.KeyExpression, opts Options, res storageNames) (recordlayer.KeyExpression, string, error) {
	var fn string
	var operand values.Value
	switch a := aggregate.(type) {
	case *values.AggregateValue:
		switch a.Op {
		case values.AggCountStar:
			if groupingExpr == nil {
				return recordlayer.GroupAll(recordlayer.EmptyKey()), recordlayer.IndexTypeCount, nil
			}
			return recordlayer.GroupAll(groupingExpr), recordlayer.IndexTypeCount, nil
		case values.AggBitmapConstructAgg:
			return bitmapKeyExpression(a.Operand, groupingExpr, res)
		}
		fn, operand = a.Op.Symbol(), a.Operand
	case *values.IndexOnlyAggregateValue:
		fn, operand = "MIN_EVER", a.Child
		if a.Op == values.IndexOnlyMaxEverLong {
			fn = "MAX_EVER"
		}
	default:
		return nil, "", unableToConstruct()
	}
	indexType, err := aggregateIndexType(fn, operand, opts)
	if err != nil {
		return nil, "", err
	}
	child, ok := operand.(*pathColumn)
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
		// Java asserts FieldKeyExpression or ThenKeyExpression
		// (groupedAggregate); a nested column's NestingKeyExpression is its
		// "condition is not met!" internal error.
	default:
		return nil, "", api.NewErrorf(api.ErrCodeInternalError,
			"index generator: aggregate operand built a %T key expression", groupedValue)
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
