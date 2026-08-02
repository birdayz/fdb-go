package query

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/query/logical"
)

// unnestAliasReject returns the duplicate-alias rejection for a lateral
// unnest whose aliases collide, nil when the aliases are sound. The ONE
// collision predicate shared by the loud translation rejects
// (translateUnnestJoin, translateChainedUnnestJoin) and the admission
// declines (unnest_gather, cluster_gate — they decline the shape so the raw
// body surfaces this same error at translation). Two arms:
//
//   - AS == AT (case-insensitive): a name-model builder would append the
//     element and the ordinal under the SAME name, and the map-keyed
//     RecordConstructorValue.Evaluate silently OVERWRITES the element with
//     the ordinal — `... AS Y AT Y` would make `SELECT Y` return the
//     ordinal, not the unnested value. Java's visitAtomTableItem binds AS
//     and AT to two distinct quantifier columns; a duplicate is a binding
//     error upstream. RFC-142.
//
//   - AT-only spelling the reserved default element name `_0`
//     (case-insensitive): unnestSeedInnerFields' `Alias == ""` fallback
//     names the unreferenced element slot OrdinalFieldName(0) = `_0`, where
//     an AT alias `_0` would duplicate the seed leg RecordType's field
//     names and fire values.NewRecordType's unique-names constructor
//     assert. This arm is a PRODUCER INVARIANT, not a live SQL surface:
//     lateralUnnestCandidate (the only LogicalUnnest producer) defaults the
//     element alias to the array field name via unnestAliases, so parser
//     input never arrives AT-only — verified by the benign-`AT "_0"` FDB
//     pin in array_unnest_ordinality_fdb_test.go. The guard keeps a future
//     producer that skips the default from reaching the constructor assert
//     (the panic-audit assert-locality rule puts the user-facing rejection
//     HERE, in this predicate).
//
// The predicate is applied at two points, and both are load-bearing:
// RejectUnnestAliasCollisions runs it over the FROM tree during FROM-scope
// analysis (so the binding error precedes reference resolution), and the
// translation rejects run it again for any path that reaches translation
// without the FROM-scope pass.
func unnestAliasReject(u *logical.LogicalUnnest) *api.Error {
	if u.AtAlias == "" {
		return nil
	}
	if u.Alias != "" && strings.EqualFold(u.Alias, u.AtAlias) {
		return api.NewError(api.ErrCodeDuplicateAlias,
			"lateral unnest AS and AT aliases must be distinct; use different names for the element and the ordinal")
	}
	if u.Alias == "" && strings.EqualFold(u.AtAlias, values.OrdinalFieldName(0)) {
		return api.NewErrorf(api.ErrCodeDuplicateAlias,
			"lateral unnest AT alias %q collides with the reserved element column name %q; add an AS alias or rename the ordinal",
			u.AtAlias, values.OrdinalFieldName(0))
	}
	return nil
}

// RejectUnnestAliasCollisions applies unnestAliasReject to every lateral
// unnest in a logical tree, so the binding error surfaces at FROM-scope
// analysis rather than at translation.
//
// The ORDER is the point, not the reach. `FROM t, t.arr AS X AT X` binds two
// different things — the element and the ordinal — to one range-variable
// name, and Java's visitAtomTableItem rejects that where the binding is made,
// before any SELECT-list reference is resolved against it. Left to
// translation, the duplicate binding stays live long enough for the semantic
// scope to see one source carrying the name twice and answer the reference
// with an ambiguity (42702) — a true statement about a scope that should
// never have been built, and the wrong error for the query.
func RejectUnnestAliasCollisions(op logical.LogicalOperator) error {
	if op == nil {
		return nil
	}
	if u, ok := op.(*logical.LogicalUnnest); ok {
		if err := unnestAliasReject(u); err != nil {
			return err
		}
	}
	for _, ch := range op.Children() {
		if err := RejectUnnestAliasCollisions(ch); err != nil {
			return err
		}
	}
	return nil
}
