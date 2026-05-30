package values

import "strings"

// JoinMergeAllValue is the result value an intermediate re-enumerated join flows
// when MORE THAN ONE of its lower tables is live (referenced by an upper join
// predicate or the final projection). It merges the bindings of all listed
// quantifier aliases into one map with qualified `ALIAS.COL` keys, exactly like
// the binary JoinMergeResultValue but over N aliases.
//
// Why it exists (RFC-043): the binary JoinMergeResultValue flows two tables; a
// chain of binary merges accumulates all tables' qualified columns up a join
// spine because each Evaluate preserves already-qualified keys. An intermediate
// join select with >2 quantifiers always re-partitions before it is implemented
// (PartitionSelectRule fires on ≥3-quantifier selects), and the rule re-stamps
// each sub-select's result over its OWN immediate quantifiers — so a
// JoinMergeAllValue is only ever Evaluate'd once its select has been reduced to
// the quantifiers actually bound at that level. The N-ary form lets the rule
// carry the "flow all my live columns merged" intent across re-partition
// firings without inventing per-level structure up front.
//
// Like JoinMergeResultValue it preserves already-qualified keys (so nested
// merges accumulate) and qualifies bare keys under their alias. Later aliases do
// not overwrite earlier qualified keys (the !exists guard mirrors mergeRows,
// TODO 7.1).
type JoinMergeAllValue struct {
	Aliases []CorrelationIdentifier
}

func NewJoinMergeAllValue(aliases ...CorrelationIdentifier) *JoinMergeAllValue {
	return &JoinMergeAllValue{Aliases: aliases}
}

func (*JoinMergeAllValue) Children() []Value { return nil }
func (*JoinMergeAllValue) Type() Type        { return UnknownType }
func (*JoinMergeAllValue) Name() string      { return "join_merge_all" }

func (v *JoinMergeAllValue) Evaluate(evalCtx any) any {
	binder, ok := evalCtx.(CorrelationBinder)
	if !ok {
		return nil
	}

	merged := make(map[string]any)
	found := false
	for _, alias := range v.Aliases {
		raw, _ := binder.GetCorrelationBinding(alias)
		m, _ := raw.(map[string]any)
		if m == nil {
			continue
		}
		found = true
		qual := strings.ToUpper(alias.Name())
		for k, val := range m {
			// Already-qualified keys (from a nested merge) pass through verbatim
			// and are never overwritten by a later alias's bare key.
			if strings.Contains(k, ".") {
				if _, exists := merged[k]; !exists {
					merged[k] = val
				}
				continue
			}
			if _, exists := merged[k]; !exists {
				merged[k] = val
			}
			if qual != "" {
				qk := qual + "." + strings.ToUpper(k)
				if _, exists := merged[qk]; !exists {
					merged[qk] = val
				}
			}
		}
	}
	if !found {
		return nil
	}
	return merged
}

var _ Value = (*JoinMergeAllValue)(nil)
