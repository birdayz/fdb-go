package plandiff

// rfc082KnownRed is the regression-LOCK: corpus entries whose Go vs Java
// cross-engine behaviour is a KNOWN, currently-unfixed divergence — NOT
// laundered green (these are real Go gaps: a wrong/UNKNOWN result-set column
// type or name, or a Go-too-lenient acceptance of input Java rejects). They are
// tracked in RFC-082's "tail".
//
// The RunSql harness asserts the diverging set is EXACTLY this list:
//   - a NEW divergence (an entry not listed here that starts diverging) fails
//     the suite — that is a regression caught at the merge gate;
//   - a listed entry that silently STARTS passing fails the suite too — forcing
//     it to be removed here, so the list can only shrink.
//
// This is what lets conformance_java run in CI (the gate) while the tail is
// still red, without faking green and without leaving new breakage unwatched.
// When the list reaches empty, the gate is a plain all-green gate.
var rfc082KnownRed = map[string]bool{
	// Go-too-lenient (withheld from annotation — fix-or-accept):
	// type_mismatch_boolean_eq_int removed: the plan-time promotion gate
	// rejects bool = int with Java's 42804 — cross-engine equivalent.
	"agg_in_where_rejected":        true, // WHERE COUNT(*)>0 accepted; should reject
	"cast_bigint_to_boolean_probe": true, // Go allows a cast Java disallows
	// CAST edge cases (Go vs Java error/behaviour on overflow / malformed strings):
	"cast_bigint_to_integer_overflow":               true,
	"cast_bigint_to_int_overflow_rejected":          true,
	"cast_empty_string_to_bigint_rejected":          true,
	"cast_string_decimal_zero_to_bigint":            true,
	"cast_string_internal_space_to_bigint_rejected": true,
	"cast_string_non_numeric_rejected":              true,
	// nested_derived_col_rename removed: the RFC-141 R4 projected-EXISTS fold's
	// column metadata/alias-provenance unification fixed the derived-column
	// rename so Go now matches Java cross-engine (RFC-082 lock shrinks).
	// The GREATEST/LEAST family and proj_literal_column removed: integer
	// literals now narrow to INTEGER like Java ParseHelpers.parseDecimal,
	// so literal-bearing result types match cross-engine (RFC-082 lock
	// shrinks).
	// select_count_alias REMOVED (the lock shrinks): a SCALAR aggregate's
	// output alias was dropped because the ungrouped plan is a bare
	// StreamingAgg with no projection to carry it, and buildAggColumns named
	// the column from the expression while ignoring AggregateSpec.Alias.
	// `SELECT COUNT(*) AS cnt` now reports CNT, matching Java. Found by the
	// RFC-182 rowdiff harness; pinned by TestFDB_ScalarAggregate_KeepsAlias.
	// "recursive_cte_depth_counter" was REMOVED (the lock shrank): the recursive
	// leg's computed column (SELECT n + 1) silently stalled recursion one level
	// early (count 2 instead of Java's 10) because the normalization wrap read
	// the leg output by its LOGICAL name while the row is keyed by the PHYSICAL
	// name. Fixed by legPhysicalOutputNames (values.ProjectionColumnName naming
	// contract) — found via a cross-engine plan differential.
	// Pre-existing inline annotations that drifted (now re-classified red):
	"distinct_count":                               true,
	"group_by_null_bucket":                         true,
	"groupby_null_key_bigint":                      true,
	"scalar_subq_in_having":                        true,
	"scalar_subq_after_update_with_subq_rhs":       true,
	"scalar_subq_after_delete_with_subq_threshold": true,
	"scalar_subq_with_secondary_index_max":         true,
}

// IsKnownRed reports whether a corpus entry is a tracked, currently-unfixed
// cross-engine divergence (part of the regression lock).
func IsKnownRed(name string) bool { return rfc082KnownRed[name] }
