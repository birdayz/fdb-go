package expressions

// Reference is the planner's handle on an equivalence class of
// RelationalExpressions — Cascades' "memo group". Once the Memo lands
// (Track B3) a Reference will hold a SET of equivalent expressions; for
// the seed it holds exactly one.
//
// References are passed around by pointer (Java passes `Reference`
// objects, which are reference types under the hood). Two Quantifiers
// that range over the same Reference share the same equivalence class —
// this is how the planner avoids re-optimising the same subtree twice.
//
// Ports the relevant subset of Java's
// `com.apple.foundationdb.record.query.plan.cascades.Reference`. The
// Java class is 1068 lines; we expose:
//   - InitialOf: build a Reference holding a single expression
//   - Get: read the (currently single) member
//   - Members: read the full member list (always size 1 in the seed)
//   - Insert: append a member (no-op if EqualsWithoutChildren matches)
//
// Insert's semantic-equality check uses EqualsWithoutChildren only —
// children's equivalence is established by their own References sharing
// identity, so the recursive comparison reduces to local checks at each
// level. This matches Java's ExpressionPartition behaviour.
type Reference struct {
	members []RelationalExpression
}

// InitialOf returns a Reference holding the single expression e as its
// only member. Equivalent to Java's `Reference.initialOf(e)`.
func InitialOf(e RelationalExpression) *Reference {
	return &Reference{members: []RelationalExpression{e}}
}

// Get returns the (first) member. For seed References this is the only
// member; once the Memo lands, callers will iterate via Members instead.
// Returns nil if the Reference is empty (shouldn't happen for
// seed-constructed References — guards against future Memo bugs).
func (r *Reference) Get() RelationalExpression {
	if len(r.members) == 0 {
		return nil
	}
	return r.members[0]
}

// Members returns the equivalence-class members. The slice is read-only;
// callers must not mutate it.
func (r *Reference) Members() []RelationalExpression {
	return r.members
}

// Insert adds e to the equivalence class if no existing member already
// matches under EqualsWithoutChildren with the empty alias map. Returns
// true if the member was inserted, false if a duplicate was found.
//
// This is the seed of memo deduplication — it covers the common case
// (rule fires, produces an expression that's structurally identical to
// an existing one). Full memo-group dedup (where two children are
// distinct objects but share a Reference) needs the Memo machinery and
// lands in B3.
func (r *Reference) Insert(e RelationalExpression) bool {
	for _, m := range r.members {
		if m.EqualsWithoutChildren(e, EmptyAliasMap()) {
			return false
		}
	}
	r.members = append(r.members, e)
	return true
}
