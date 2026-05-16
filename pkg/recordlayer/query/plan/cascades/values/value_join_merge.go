package values

import "strings"

// JoinMergeResultValue is the resultValue for a FlatMap join plan. When
// evaluated with both outer and inner correlations bound, it produces a
// merged map containing qualified fields from both sides. Mirrors the
// effect of Java's RecordConstructorValue that explicitly lists fields
// from both quantifiers.
type JoinMergeResultValue struct {
	OuterAlias CorrelationIdentifier
	InnerAlias CorrelationIdentifier
}

func NewJoinMergeResultValue(outerAlias, innerAlias CorrelationIdentifier) *JoinMergeResultValue {
	return &JoinMergeResultValue{OuterAlias: outerAlias, InnerAlias: innerAlias}
}

func (*JoinMergeResultValue) Children() []Value { return nil }
func (*JoinMergeResultValue) Type() Type        { return UnknownType }
func (*JoinMergeResultValue) Name() string      { return "join_merge" }

func (v *JoinMergeResultValue) Evaluate(evalCtx any) any {
	binder, ok := evalCtx.(CorrelationBinder)
	if !ok {
		return nil
	}

	outerRaw, _ := binder.GetCorrelationBinding(v.OuterAlias)
	innerRaw, _ := binder.GetCorrelationBinding(v.InnerAlias)

	outerMap, _ := outerRaw.(map[string]any)
	innerMap, _ := innerRaw.(map[string]any)

	if outerMap == nil && innerMap == nil {
		return nil
	}

	outerQual := strings.ToUpper(v.OuterAlias.Name())
	innerQual := strings.ToUpper(v.InnerAlias.Name())

	merged := make(map[string]any, len(outerMap)+len(innerMap))

	for k, val := range outerMap {
		merged[k] = val
		if !strings.Contains(k, ".") && outerQual != "" {
			merged[outerQual+"."+strings.ToUpper(k)] = val
		}
	}
	for k, val := range innerMap {
		merged[k] = val
		if !strings.Contains(k, ".") && innerQual != "" {
			merged[innerQual+"."+strings.ToUpper(k)] = val
		}
	}

	return merged
}

var _ Value = (*JoinMergeResultValue)(nil)
