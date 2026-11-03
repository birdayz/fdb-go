package expr

import (
	"fmt"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
)

// These functions are a Go SQL extension. Their resolved representation is the
// Record Layer's typed comparison, not a separate SQL execution path.
func textFunctionType(name string) (predicates.ComparisonType, bool) {
	switch name {
	case "TEXT_CONTAINS_ALL":
		return predicates.ComparisonTextContainsAll, true
	case "TEXT_CONTAINS_ANY":
		return predicates.ComparisonTextContainsAny, true
	case "TEXT_CONTAINS_PHRASE":
		return predicates.ComparisonTextContainsPhrase, true
	case "TEXT_CONTAINS_ALL_WITHIN":
		return predicates.ComparisonTextContainsAllWithin, true
	case "TEXT_CONTAINS_PREFIX":
		return predicates.ComparisonTextContainsPrefix, true
	case "TEXT_CONTAINS_ALL_PREFIXES":
		return predicates.ComparisonTextContainsAllPrefixes, true
	case "TEXT_CONTAINS_ANY_PREFIX":
		return predicates.ComparisonTextContainsAnyPrefix, true
	default:
		return 0, false
	}
}

func (r *Resolver) walkTextFunction(name string, typ predicates.ComparisonType, fa antlrgen.INamedOrUnnamedFunctionArgsContext) (values.Value, error) {
	args, err := r.positionalFunctionArgs(fa)
	if err != nil {
		return nil, err
	}
	arity := 2
	if typ == predicates.ComparisonTextContainsAllWithin {
		arity = 3
	}
	if len(args) != arity && len(args) != arity+1 {
		return nil, api.NewErrorf(api.ErrCodeCannotConvertType, "%s requires %d arguments and an optional literal tokenizer name", name, arity)
	}
	for i := 0; i < 2; i++ {
		t := args[i].Type()
		if t != nil && t.Code() != values.TypeCodeString && t.Code() != values.TypeCodeUnknown && t.Code() != values.TypeCodeNull {
			return nil, api.NewErrorf(api.ErrCodeCannotConvertType, "%s argument %d must be STRING", name, i+1)
		}
	}
	comparison := predicates.Comparison{Type: typ, Operand: args[1], TextStrictPrefix: true, TextTokenizerName: recordlayer.DefaultTextTokenizerName}
	if typ == predicates.ComparisonTextContainsAllWithin {
		distanceValue, _ := specialize(args[2])
		distance, ok := values.EvaluateConstant(distanceValue)
		if !ok {
			return nil, api.NewErrorf(api.ErrCodeCannotConvertType, "%s distance must be an integer literal", name)
		}
		switch d := distance.(type) {
		case int64:
			if d < 0 || int64(int(d)) != d {
				return nil, api.NewErrorf(api.ErrCodeCannotConvertType, "%s distance is out of range", name)
			}
			comparison.TextMaxDistance = int(d)
		case int32:
			if d < 0 {
				return nil, api.NewErrorf(api.ErrCodeCannotConvertType, "%s distance is negative", name)
			}
			comparison.TextMaxDistance = int(d)
		default:
			return nil, api.NewErrorf(api.ErrCodeCannotConvertType, "%s distance must be an integer literal, got %s", name, fmt.Sprintf("%T", distance))
		}
	}
	if len(args) == arity+1 {
		tokenizerValue, _ := specialize(args[arity])
		tokenizer, ok := values.EvaluateConstant(tokenizerValue)
		if !ok {
			return nil, api.NewErrorf(api.ErrCodeCannotConvertType, "%s tokenizer must be a literal STRING", name)
		}
		s, ok := tokenizer.(string)
		if !ok {
			return nil, api.NewErrorf(api.ErrCodeCannotConvertType, "%s tokenizer must be a literal STRING", name)
		}
		if s == "" {
			s = recordlayer.DefaultTextTokenizerName
		}
		comparison.TextTokenizerName = s
	}
	if _, err := recordlayer.GetTextTokenizer(comparison.TextTokenizerName); err != nil {
		return nil, err
	}
	return &predicateValue{pred: predicates.NewComparisonPredicate(args[0], comparison)}, nil
}
