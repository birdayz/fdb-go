package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// unfindableHiddenName is a hidden-column name that NO name scan can match: it
// is not a legal SQL identifier, shares no case-folding with any column spelled
// in the fixtures, and cannot be produced by any rendering of a FieldValue's
// leaf. Resolution reaching the column named by it therefore cannot have gone
// through pullUpSortKeyValue's EqualFold arm.
const unfindableHiddenName = "\x00 no name scan can find this \x00"

// TestHiddenSortColumnIsRecoveredByValueNotByName pins the mechanism by which a
// hidden remainingOrderBy column (the one collectExtraSortColumns appends for an
// ORDER BY key absent from the SELECT list) is recovered when the fold re-applies
// the sort above the folded projection.
//
// The mechanism is a VALUE match, not a name scan. pullUpSortKeyValue tries the
// output-field-value match and the source-column-value match BEFORE it falls back
// to comparing the key's leaf name against the output field names, so a hidden
// column whose NAME is unreachable by any scan is still found — by the value it
// carries.
//
// This is load-bearing in the negative direction, which is why it is pinned. The
// hidden column's name is otherwise assumed to be the channel that recovers it,
// and reasoning that starts there concludes that renaming the hidden columns
// changes which keys resolve. It does not: the name is a DISPLAY name here, and
// the only thing it feeds is the Explain rendering of the re-applied sort. A
// change that made recovery depend on the name again would red this test, and it
// would do so for the exact shape that matters — a key whose leaf name matches
// NO output field.
func TestHiddenSortColumnIsRecoveredByValueNotByName(t *testing.T) {
	t.Parallel()

	// `SELECT id, EXISTS(...) AS h FROM t1 ORDER BY col1` — COL1 is not in the
	// SELECT list, so the fold appends it as a hidden trailing column. Give that
	// column a name no scan can find, and keep its VALUE the source column.
	src := sortSource{isJoin: false}
	key := logical.SortKey{
		Expr:  "COL1",
		Value: &values.FieldValue{Field: "COL1", Typ: values.UnknownType},
	}
	fields := []values.RecordConstructorField{
		{Name: "ID", Value: &values.FieldValue{Field: "ID", Typ: values.UnknownType}},
		{Name: "H", Value: &values.FieldValue{Field: "H", Typ: values.UnknownType}},
		{Name: unfindableHiddenName, Value: &values.FieldValue{Field: "COL1", Typ: values.UnknownType}},
	}
	const hiddenOrdinal = 2

	got := pullUpSortKeyValue(key, key.Value, fields, src)

	fv, ok := got.(*values.FieldValue)
	if !ok {
		t.Fatalf("pullUpSortKeyValue returned %T, want *values.FieldValue: the hidden "+
			"column was not recovered at all", got)
	}
	if fv.Resolved == nil {
		t.Fatalf("pullUpSortKeyValue left the key LAZY (Resolved == nil, Field=%q). The "+
			"hidden column is recovered by VALUE; a lazy result means both value matches "+
			"missed and the key fell through to the name arm, which cannot see %q",
			fv.Field, unfindableHiddenName)
	}
	if n := len(fv.Resolved.Accessors); n != 1 {
		t.Fatalf("resolved path has %d accessors, want 1 (a flat output-slot read)", n)
	}
	if ord := fv.Resolved.Accessors[0].Ordinal; ord != hiddenOrdinal {
		t.Errorf("resolved ordinal = %d, want %d (the hidden column's slot). An ordinal "+
			"of 0 or 1 means the key pulled up to an unrelated OUTPUT column and the "+
			"re-applied sort would order by the wrong field", ord, hiddenOrdinal)
	}
	if fv.Field != unfindableHiddenName {
		t.Errorf("pulled-up display name = %q, want the hidden column's own name. A "+
			"different name means the match landed on another field", fv.Field)
	}
}

// TestHiddenSortColumnNameIsNotTheRecoveryChannel is the same claim stated as the
// difference it makes: recovery must be INDIFFERENT to the hidden column's name.
// Two folds identical except for that name must resolve the key to the same slot.
//
// The pin exists because the name is user-VISIBLE in Explain (it is what the
// re-applied InMemorySort renders its key as), so it is tempting to treat the
// name as identity. It is not identity; the value is. Anything that makes the
// two arms below disagree has moved recovery onto the name channel.
func TestHiddenSortColumnNameIsNotTheRecoveryChannel(t *testing.T) {
	t.Parallel()

	src := sortSource{isJoin: false}
	key := logical.SortKey{
		Expr:  "COL1",
		Value: &values.FieldValue{Field: "COL1", Typ: values.UnknownType},
	}

	// A DECOY output column spells the key's own leaf name (`SELECT other AS col1
	// ... ORDER BY col1` where the ORDER BY resolves to the SOURCE column, not to
	// the alias) while holding a DIFFERENT value. The hidden column carries the
	// value the key actually references, under a name no scan can find.
	//
	// The two channels now disagree by construction: a name scan lands on slot 0,
	// the value match on slot 1. That disagreement is what makes this test able to
	// fail — with both channels pointing at the same slot the test would pass with
	// the name arm fully in charge.
	fields := []values.RecordConstructorField{
		{Name: "COL1", Value: &values.FieldValue{Field: "OTHER", Typ: values.UnknownType}},
		{Name: unfindableHiddenName, Value: &values.FieldValue{Field: "COL1", Typ: values.UnknownType}},
	}
	const (
		decoyOrdinal  = 0
		hiddenOrdinal = 1
	)

	got := pullUpSortKeyValue(key, key.Value, fields, src)
	fv, ok := got.(*values.FieldValue)
	if !ok || fv.Resolved == nil || len(fv.Resolved.Accessors) != 1 {
		t.Fatalf("key did not resolve to a flat output slot (%T)", got)
	}

	switch ord := fv.Resolved.Accessors[0].Ordinal; ord {
	case hiddenOrdinal:
		// Correct: the value match found the column holding COL1.
	case decoyOrdinal:
		t.Errorf("the sort key resolved to ordinal %d — the DECOY output column that "+
			"merely SHARES the key's leaf name %q while holding a different value. "+
			"Recovery has moved onto the name channel; the re-applied sort would order "+
			"by the wrong column. It must resolve to ordinal %d, the hidden column whose "+
			"VALUE is the key's source column",
			ord, "COL1", hiddenOrdinal)
	default:
		t.Errorf("the sort key resolved to ordinal %d, want %d (the hidden column)", ord, hiddenOrdinal)
	}
}
