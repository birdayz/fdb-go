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
// Naming preserves the opaque merge's qualified-key semantics so name-based resolution
// (composeFieldOverConstructor: field(RC, name) → the leg FieldValue) keeps working:
//   - a column whose bare name is UNIQUE across all legs is named by the bare column, so a
//     bare reference resolves;
//   - a column whose bare name is DUPLICATED across legs is named ALIAS.COL (upper-cased,
//     matching the merge's ToUpper qualification) so both legs' columns stay distinct and
//     addressable. A bare reference to a duplicated column is ambiguous and rejected at SQL
//     resolution, so it legitimately has no bare field here.
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
			name := c.Name
			if bareCount[strings.ToUpper(c.Name)] > 1 {
				name = strings.ToUpper(leg.Alias.Name()) + "." + strings.ToUpper(c.Name)
			}
			fields = append(fields, RecordConstructorField{
				Name:  name,
				Value: NewFieldValue(qov, c.Name, c.FieldType),
			})
		}
	}
	return NewRecordConstructorValue(fields...)
}
