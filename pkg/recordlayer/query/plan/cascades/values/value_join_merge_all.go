package values

import "strings"

// JoinMergeAllValue is the SOLE join-merge result value: it merges the bindings
// of N listed quantifier aliases into one map with qualified `ALIAS.COL` keys.
// It serves both the translator's binary join seeds (N=2, sites in
// cascades_translator.go) and PartitionSelectRule's re-enumerated intermediate
// merges (N≥2). The binary JoinMergeResultValue was retired in RFC-074: two Go
// types for one concept (binary vs N-ary) is dead-weight duplication that blocks
// single-type interning (and is a Go-only divergence — Java has one
// RecordConstructorValue-based path). One canonical type. This is a
// behavior-preserving cleanup; it is NOT a task-count/budget fix (measurement: the
// ≥5-way distinctRefs/tasksRun are unchanged — that blowup is the separate
// broad-merge-under-winners / lattice-pruning problem, PR-C2).
//
// A chain of merges accumulates all tables' qualified columns up a join spine
// because each Evaluate preserves already-qualified keys. An intermediate join
// select with >2 quantifiers always re-partitions before it is implemented
// (PartitionSelectRule fires on ≥3-quantifier selects), and the rule re-stamps
// each sub-select's result over its OWN immediate quantifiers — so a merge is
// only ever Evaluate'd once its select has been reduced to the quantifiers
// actually bound at that level.
//
// It qualifies bare keys under their alias and writes already-qualified keys
// (ALIAS.COL, from a nested merge) verbatim; distinct table prefixes mean
// qualified keys never collide across aliases, so they accumulate up the whole
// merge chain. Bare-key precedence is last-table-wins — consumers resolve by
// qualified name.
//
// Equality/hash are SET-based (order-independent — see SemanticEqualsUnderAliasMap
// and SemanticHashCode): the merge of a leg-set is the same logical sub-product in
// any leg order, so equal leg-sets compare equal regardless of join order (Graefe
// condition 1). The Aliases slice nonetheless preserves INSERTION order, because
// order-sensitive consumers depend on it: the translator seed is built
// [outer, inner], and composeFieldOverJoinMerge (inner = Aliases[1]) and
// joinResultValueIsReversed (SQL-first = Aliases[0]) read that order. Insertion
// order is a representation detail invisible to interning.
//
// Seed marks a translator-built join seed (set only at the cascades_translator.go
// sites, via NewJoinMergeSeedValue). It encodes the PROVENANCE bit the retired
// two-type design carried implicitly: a seed merge names only its two immediate
// source aliases but HIDES the real projection (which lives in the Project above),
// so PartitionSelectRule must keep ALL lower aliases live when partitioning it —
// it cannot trust the seed's named set. A re-enumeration merge (NewJoinMergeAllValue,
// Seed=false) names EXACTLY the live aliases its parent needs, so the rule keeps
// only those.
//
// Seed IS part of equality and hash (NOT Evaluate). The retired binary
// JoinMergeResultValue and N-ary JoinMergeAllValue were distinct Go types and so
// never compared equal — a translator seed never interned with a re-enumeration of
// the same leg-set. The Seed bit preserves that distinction exactly, so the
// collapse is behavior-preserving: one Go type, same equivalence classes. (Were
// Seed excluded, seed and re-enumeration would suddenly intern, triggering the
// RFC-037 cross-group merge the two-type design never did — which blew the ≥4-way
// STAR past the task budget.) Seed is excluded ONLY from Evaluate: the merged-row
// semantics are identical regardless of provenance. The Java-clean end state is
// the seed naming its projection honestly via ordinal source-anchoring (true-7.6),
// which retires this bit; until then it is the faithful 1:1 encoding of the old type.
type JoinMergeAllValue struct {
	Aliases []CorrelationIdentifier
	Seed    bool
}

// NewJoinMergeAllValue builds a re-enumeration merge (Seed=false) — names exactly
// the live aliases.
func NewJoinMergeAllValue(aliases ...CorrelationIdentifier) *JoinMergeAllValue {
	return &JoinMergeAllValue{Aliases: aliases}
}

// NewJoinMergeSeedValue builds a translator join seed (Seed=true) — names its two
// immediate source aliases but hides the real projection, so PartitionSelectRule
// keeps all lower aliases live when it partitions a select carrying this result.
func NewJoinMergeSeedValue(aliases ...CorrelationIdentifier) *JoinMergeAllValue {
	return &JoinMergeAllValue{Aliases: aliases, Seed: true}
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
			// Write the key verbatim, and additionally write the table-qualified
			// ALIAS.COL form for bare keys.
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
