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
//   - a column whose bare name is UNIQUE across all legs ALSO gets a bare field, so a bare
//     reference resolves. A column whose bare name is DUPLICATED across legs gets NO bare field:
//     a bare reference to it is ambiguous and rejected at SQL resolution, so it legitimately has
//     only the two qualified fields.
//
// The field VALUE always carries the original (non-upper-cased) column name so the leg's
// QuantifiedObjectValue field access matches the source row's key.
func NewAnchoredJoinRecord(legs []AnchoredJoinLeg) *RecordConstructorValue {
	bareCount := make(map[string]int)
	for _, leg := range legs {
		for _, c := range leg.Columns {
			bareCount[strings.ToUpper(c.Name)]++
		}
	}
	var fields []RecordConstructorField
	for _, leg := range legs {
		qov := NewQuantifiedObjectValue(leg.Alias)
		for _, c := range leg.Columns {
			// Qualified ALIAS.COL field — always present, always unambiguous.
			fields = append(fields, RecordConstructorField{
				Name:  strings.ToUpper(leg.Alias.Name()) + "." + strings.ToUpper(c.Name),
				Value: NewFieldValue(qov, c.Name, c.FieldType),
			})
			// Bare field — only for columns whose bare name is unique across legs.
			if bareCount[strings.ToUpper(c.Name)] == 1 {
				fields = append(fields, RecordConstructorField{
					Name:  c.Name,
					Value: NewFieldValue(qov, c.Name, c.FieldType),
				})
			}
		}
	}
	return NewRecordConstructorValue(fields...)
}
