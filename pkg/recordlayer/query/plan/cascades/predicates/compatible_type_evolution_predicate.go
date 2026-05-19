package predicates

import (
	"fmt"
	"strings"
)

// CompatibleTypeEvolutionPredicate is a leaf predicate used by the plan
// cache to verify that a cached plan is still valid under the current
// schema. It carries a list of (recordTypeName, fieldAccessTrieNode)
// pairs describing which record type fields the plan accesses.
//
// Ports Java's
// com.apple.foundationdb.record.query.plan.cascades.predicates.CompatibleTypeEvolutionPredicate
//
// At eval time in Java, this checks that the current schema's record
// types are compatible with the cached plan's field access patterns.
// In Go, the plan cache is not yet ported, so Eval returns TriTrue
// unconditionally. The type exists for structural conformance: it can
// be round-tripped through serialization and participates in
// predicate equality.
type CompatibleTypeEvolutionPredicate struct {
	// RecordTypeNameFieldAccessMap maps record type names to their
	// field access trie roots. Each trie encodes which fields of
	// that record type are accessed by the plan.
	RecordTypeNameFieldAccessMap map[string]*FieldAccessTrieNode
}

// FieldAccessTrieNode is one node in the field-access trie that
// CompatibleTypeEvolutionPredicate carries. Each node either has
// children (keyed by field accessor) or a terminal type value.
//
// Ports Java's CompatibleTypeEvolutionPredicate.FieldAccessTrieNode.
type FieldAccessTrieNode struct {
	// FieldName is the name of the field this node represents.
	FieldName string
	// Ordinal is the protobuf field ordinal.
	Ordinal int
	// Children, if non-nil, are the child trie nodes keyed by
	// field name. A nil map means this is a terminal node.
	Children map[string]*FieldAccessTrieNode
	// TypeName is the terminal type string. Set only when
	// Children is nil (leaf of the trie).
	TypeName string
}

// NewCompatibleTypeEvolutionPredicate constructs the predicate from
// the given record type -> field access trie mapping.
func NewCompatibleTypeEvolutionPredicate(m map[string]*FieldAccessTrieNode) *CompatibleTypeEvolutionPredicate {
	// Defensive copy.
	cp := make(map[string]*FieldAccessTrieNode, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return &CompatibleTypeEvolutionPredicate{RecordTypeNameFieldAccessMap: cp}
}

// Children returns nil -- this is a leaf predicate.
func (*CompatibleTypeEvolutionPredicate) Children() []QueryPredicate {
	return []QueryPredicate{}
}

// GetCorrelatedTo returns the empty set — type evolution predicates
// reference no quantifier aliases.
func (*CompatibleTypeEvolutionPredicate) GetCorrelatedTo() map[CorrelationIdentifier]struct{} {
	return map[CorrelationIdentifier]struct{}{}
}

// Eval returns TriTrue. Plan-cache schema validation is not yet
// ported; this predicate exists for structural conformance.
func (*CompatibleTypeEvolutionPredicate) Eval(_ any) TriBool {
	return TriTrue
}

// Explain renders the predicate in a human-readable form matching
// Java's explain output: compatibleTypeEvolution(<record types>).
func (p *CompatibleTypeEvolutionPredicate) Explain() string {
	if len(p.RecordTypeNameFieldAccessMap) == 0 {
		return "compatibleTypeEvolution()"
	}
	names := make([]string, 0, len(p.RecordTypeNameFieldAccessMap))
	for name := range p.RecordTypeNameFieldAccessMap {
		names = append(names, name)
	}
	return fmt.Sprintf("compatibleTypeEvolution(%s)", strings.Join(names, ", "))
}
