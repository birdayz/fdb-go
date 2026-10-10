// Portions derived from FoundationDB Record Layer (RangeValue.java,
// PromoteValue.java),
// Copyright 2015-2024 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package embedded

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/functions"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/query/expr"
	"fdb.dev/pkg/relational/core/query/logical"
)

// buildTableFunctionLogical lowers a built-in table function in FROM. range is
// the only built-in (Java's RangeValue.RangeFn); SQL table functions were
// already expanded text-wise before parsing. resolver is nil for a source with
// no lateral prefix.
func buildTableFunctionLogical(
	item *antlrgen.TableValuedFunctionContext,
	alias, binding string,
	resolver *expr.Resolver,
) (*logical.LogicalInlineValues, error) {
	fn := item.TableFunction()
	name := functions.NormalizeIdentifier(fn.TableFunctionName().GetText())
	if !strings.EqualFold(name, "RANGE") {
		return nil, api.NewErrorf(api.ErrCodeUndefinedFunction, "Unknown table function %s", name)
	}
	if fn.InlineTableDefinition() != nil {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation, "column list on a table function")
	}
	args := fn.NamedOrUnnamedFunctionArgs()
	if args == nil || len(args.AllNamedFunctionArg()) > 0 || len(args.AllFunctionArg()) > 3 {
		return nil, api.NewError(api.ErrCodeUndefinedFunction, "range expects 1 to 3 positional arguments")
	}
	if resolver == nil {
		// Unbound until bindTableFunctions sees the lateral scope; evaluating
		// it before then fails instead of yielding an empty range.
		source, err := logical.NewTableFunctionSource(alias, values.NewRangeValue(nil, nil, nil))
		if err != nil {
			return nil, api.NewErrorf(api.ErrCodeInternalError, "range source: %v", err)
		}
		source.Binding = binding
		return source, nil
	}
	bounds := make([]values.Value, 0, 3)
	for _, arg := range args.AllFunctionArg() {
		v, err := resolver.WalkExpressionForProjection(arg.(*antlrgen.FunctionArgContext).Expression())
		if err != nil {
			if mapped := mapPredicateWalkError(err); mapped != nil {
				return nil, mapped
			}
			return nil, err
		}
		promoted, err := rangeBoundary(v)
		if err != nil {
			return nil, err
		}
		bounds = append(bounds, promoted)
	}
	begin := values.Value(values.LiteralValue(int64(0)))
	step := values.Value(values.LiteralValue(int64(1)))
	var end values.Value
	switch len(bounds) {
	case 1:
		end = bounds[0]
	case 2:
		begin, end = bounds[0], bounds[1]
	case 3:
		begin, end, step = bounds[0], bounds[1], bounds[2]
	}
	source, err := logical.NewTableFunctionSource(alias, values.NewRangeValue(begin, end, step))
	if err != nil {
		return nil, api.NewErrorf(api.ErrCodeInternalError, "range source: %v", err)
	}
	source.Binding = binding
	return source, nil
}

// rangeBoundary is Java's checkValidBoundaryType + PromoteValue.inject(LONG).
func rangeBoundary(v values.Value) (values.Value, error) {
	t := v.Type()
	switch t.(type) {
	case *values.RecordType, *values.ArrayType:
		return nil, api.NewError(api.ErrCodeCannotConvertType, "range boundary must be a primitive type")
	}
	if values.MaximumType(t, values.NullableLong) == nil {
		return nil, api.NewErrorf(api.ErrCodeCannotConvertType, "range boundary of type %s is not coercible to LONG", t)
	}
	if t.Code() == values.TypeCodeLong {
		return v, nil
	}
	p, err := values.NewPromoteValueChecked(v, values.NullableLong)
	if err != nil {
		return nil, api.NewErrorf(api.ErrCodeCannotConvertType, "range boundary: %v", err)
	}
	return p, nil
}
