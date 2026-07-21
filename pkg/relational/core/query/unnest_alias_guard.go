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
//     HERE, at translation).
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
