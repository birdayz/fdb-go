package values

import "strings"

// AnchoredJoinLeg is one source leg of a source-anchored join result (RFC-077): a
// quantifier alias and the columns its result row carries (name + type).
type AnchoredJoinLeg struct {
	Alias   CorrelationIdentifier
	Columns []Field
}

// NewAnchoredJoinRecord builds the source-anchored join result value (RFC-077): a
// RecordConstructorValue whose fields are FieldValue(QuantifiedObjectValue(legAlias), col)
// — one per column of each leg. This replaces the opaque, name-keyed JoinMergeAllValue and
// its after-the-fact composeFieldOverJoinMerge re-anchoring: every projected field names its
// source quantifier directly, exactly as Java's RecordConstructorValue of
// FieldValue(QOV(leg), col) does.
//
// Naming preserves the opaque merge's bare+qualified key set so name-based resolution
// (composeFieldOverConstructor: field(RC, name) → the leg FieldValue) keeps working for
// EVERY reference the SARG/derivation can pull up — exactly the keys the merge's Evaluate
// emits (value_join_merge_all.go writes both the bare and the ALIAS.COL form):
//   - EVERY column gets a qualified ALIAS.COL field (upper-cased, matching the merge's ToUpper
//     qualification), so a qualified reference (e.g. A.NAME) ALWAYS resolves — including to a
//     column whose bare name happens to be unique;
//   - EVERY column also gets a bare field, LAST-LEG-WINS on a cross-leg collision — exactly the
//     opaque merge's Evaluate, which writes every leg's keys bare (later legs overwrite earlier
//     ones for a shared name). A RecordConstructorValue cannot hold two fields of the same name,
//     so for a duplicated bare name only the LAST leg's column gets the bare field (the earlier
//     legs are still reachable by their qualified ALIAS.COL). Emitting bare-only-when-unique
//     instead (an earlier cut) dropped the bare key for duplicated columns, which broke 3+-way
//     joins with 0 rows: a quantifier OVER an inner join reuses the inner right leg's alias
//     (sourceAlias(join) = right-leg alias), so a qualified predicate FieldValue(QOV(rightLeg),
//     COL) reads the join's merged row by the bare COL key — present here, last-leg-wins = the
//     right leg, which is exactly the source the predicate means.
//
// The field VALUE always carries the original (non-upper-cased) column name so the leg's
// QuantifiedObjectValue field access matches the source row's key.
//
// ALREADY-QUALIFIED (DOTTED) leg columns — NESTED joins. A join leg's exposed
// columns are themselves anchored-RC field names: an inner join (A⋈B) exposes
// A.ID, B.ID, etc. When such a DOTTED name is a column of a parent leg, it
// propagates VERBATIM — the field name stays "A.ID" (NOT re-qualified to
// "PARENTLEG.A.ID") and the value is FieldValue(QOV(parentLeg), "A.ID"). This
// mirrors JoinMergeAllValue.Evaluate's "preserve already-qualified keys
// verbatim, never re-prefix": each table contributes a DISTINCT prefix, so a
// dotted key never collides across legs and reaches the right source via the
// parent leg's merged row. A dotted column gets NO extra bare/qualified form
// (it is already the resolvable key), matching the merge.
func NewAnchoredJoinRecord(legs []AnchoredJoinLeg) *RecordConstructorValue {
	// For each BARE (non-dotted) column name, record the LAST leg that carries it —
	// the bare field is emitted only there (last-leg-wins, matching the opaque
	// merge's Evaluate, and avoiding a duplicate RecordConstructorValue field name).
	// Dotted names are already-qualified keys from a nested-join leg and get no bare
	// form.
	lastLeg := make(map[string]int)
	for li, leg := range legs {
		for _, c := range leg.Columns {
			if !strings.Contains(c.Name, ".") {
				lastLeg[strings.ToUpper(c.Name)] = li
			}
		}
	}
	var fields []RecordConstructorField
	for li, leg := range legs {
		qov := NewQuantifiedObjectValue(leg.Alias)
		for _, c := range leg.Columns {
			if strings.Contains(c.Name, ".") {
				// Already-qualified (dotted) column from a nested-join leg —
				// propagate verbatim, no re-qualification, no bare form.
				fields = append(fields, RecordConstructorField{
					Name:  strings.ToUpper(c.Name),
					Value: NewFieldValue(qov, c.Name, c.FieldType),
				})
				continue
			}
			// Qualified ALIAS.COL field — always present, always unambiguous.
			fields = append(fields, RecordConstructorField{
				Name:  strings.ToUpper(leg.Alias.Name()) + "." + strings.ToUpper(c.Name),
				Value: NewFieldValue(qov, c.Name, c.FieldType),
			})
			// Bare field — emitted at the LAST leg carrying this bare name
			// (last-leg-wins), exactly mirroring the opaque merge's Evaluate.
			if lastLeg[strings.ToUpper(c.Name)] == li {
				fields = append(fields, RecordConstructorField{
					Name:  strings.ToUpper(c.Name),
					Value: NewFieldValue(qov, c.Name, c.FieldType),
				})
			}
		}
	}
	rc := NewRecordConstructorValue(fields...)
	rc.AnchoredJoin = true
	return rc
}
