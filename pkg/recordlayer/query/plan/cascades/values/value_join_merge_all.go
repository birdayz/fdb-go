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
// Like JoinMergeResultValue it qualifies bare keys under their alias and writes
// already-qualified keys (ALIAS.COL, from a nested merge) verbatim; distinct
// table prefixes mean qualified keys never collide across aliases, so they
// accumulate up the whole merge chain. Bare-key precedence matches the binary
// merge exactly (last-table-wins) — consumers resolve by qualified name.
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
			// Mirror the binary JoinMergeResultValue precedence EXACTLY so the two
			// merge values agree (Torvalds review): write the key verbatim, and
			// additionally write the table-qualified ALIAS.COL form for bare keys.
			// Already-qualified keys (ALIAS.COL, from a nested merge) carry a "."
			// and are not re-qualified; because each table contributes a DISTINCT
			// prefix, qualified keys never collide across aliases and survive the
			// whole chain. A bare key shared by two live tables (ambiguous, and
			// rejected at SQL resolution) resolves last-table-wins — consumers read
			// the qualified form, so this is well-defined, not a wrong row.
			merged[k] = val
			if qual != "" && !strings.Contains(k, ".") {
				merged[qual+"."+strings.ToUpper(k)] = val
			}
		}
	}
	if !found {
		return nil
	}
	return merged
}

var _ Value = (*JoinMergeAllValue)(nil)
