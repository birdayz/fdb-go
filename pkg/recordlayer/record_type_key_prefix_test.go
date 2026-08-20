package recordlayer

import "testing"

// hasRecordTypeKeyPrefix decides, for DeleteRecordsWhere, whether an index's
// entries are physically keyed by the record-type column. Getting it wrong in
// either direction is a wrong ANSWER rather than a slow one: reading a type
// prefix that is not there derives an index prefix naming no entry, so a delete
// Java accepts is refused (or, before the prefix bounds landed, silently
// cleared a range that did not exist); missing one that IS there scopes the
// clear too widely.
//
// Java decides it in Key.Expressions.hasRecordTypePrefix, and the two WRAPPER
// arms carry width guards that are easy to drop because they look redundant —
// the wrapper contains a RecordTypeKey, so surely the key starts with one. It
// does not, when the wrapper has aggregated or valued that column away. Those
// are the arms this drives, at zero width and at positive width, because only
// the pair distinguishes a real guard from a missing one.
func TestHasRecordTypeKeyPrefix(t *testing.T) {
	t.Parallel()

	typed := func(rest ...KeyExpression) KeyExpression {
		return Concat(append([]KeyExpression{RecordTypeKey()}, rest...)...)
	}

	cases := []struct {
		name string
		expr KeyExpression
		want bool
		why  string
	}{
		{
			name: "the bare record-type key",
			expr: RecordTypeKey(),
			want: true,
		},
		{
			name: "a concat starting with it",
			expr: typed(Field("order_id")),
			want: true,
		},
		{
			name: "a concat NOT starting with it",
			expr: Concat(Field("order_id"), RecordTypeKey()),
			want: false,
			why:  "only a LEADING type column prefixes the entries",
		},
		{
			name: "an empty concat",
			expr: Concat(),
			want: false,
		},
		{
			name: "GroupBy keeping the type column as a grouping column",
			expr: GroupBy(Field("price"), RecordTypeKey()),
			want: true,
			why:  "grouping count 1, so entries really are keyed by the type",
		},
		{
			name: "Ungrouped, which aggregates the type column away",
			expr: Ungrouped(typed(Field("price"))),
			want: false,
			why: "grouping count is ZERO — no entry is keyed by the type column, so a " +
				"type-only delete must derive an EMPTY index prefix and clear the whole index",
		},
		{
			name: "KeyWithValue with a positive split point",
			expr: KeyWithValue(typed(Field("price")), 1),
			want: true,
			why:  "the type column is inside the key half",
		},
		{
			name: "KeyWithValue split at zero, putting the type column in the VALUE",
			expr: KeyWithValue(typed(Field("price")), 0),
			want: false,
			why:  "nothing is in the key half, so the entries are not keyed by the type",
		},
		{
			name: "a nest whose child starts with the type key",
			expr: Nest("flower", typed(Field("price"))),
			want: true,
			why:  "Java recurses into a nesting child with no width guard",
		},
		{
			name: "a nest whose child does not",
			expr: Nest("flower", Field("price")),
			want: false,
		},
		{
			name: "a plain field",
			expr: Field("order_id"),
			want: false,
		},
		{
			name: "wrappers nested: Ungrouped inside a KeyWithValue",
			expr: KeyWithValue(Ungrouped(typed(Field("price"))), 1),
			want: false,
			why: "the split point is positive but the grouping underneath still aggregates " +
				"the type column away, so the recursion must keep asking rather than stop " +
				"at the outermost wrapper",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := hasRecordTypeKeyPrefix(tc.expr)
			if got != tc.want {
				msg := tc.why
				if msg == "" {
					msg = "see Key.java:365-382"
				}
				t.Fatalf("hasRecordTypeKeyPrefix = %v, want %v: %s", got, tc.want, msg)
			}
		})
	}

	// Both verdicts must be represented, or a mutation collapsing the function
	// to a constant would satisfy every remaining row.
	var trues, falses int
	for _, tc := range cases {
		if tc.want {
			trues++
		} else {
			falses++
		}
	}
	if trues == 0 || falses == 0 {
		t.Fatalf("the table must exercise BOTH verdicts (true=%d false=%d) — a one-sided "+
			"table passes against a function that always returns that answer", trues, falses)
	}
}
