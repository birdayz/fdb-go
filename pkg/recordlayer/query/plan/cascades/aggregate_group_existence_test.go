package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
)

// TestDropsVacatedGroups_OnlyGroupedRowCounts pins the exact gate RFC-209
// §5.3(a) rests on: the zero-drop is applied to a GROUPED COUNT(*) index and to
// nothing else.
//
// Three of the four axes are also reachable from SQL and pinned there
// (TestFDB_AggregateIndexVacatedGroup_SumAndCountColOracle). The fourth — the
// UNGROUPED row count — is not: tryAggregateIndexCandidate declines any index
// whose grouping count is zero, so an ungrouped aggregate never becomes a
// candidate and no SQL query can reach the guard. Without this test the
// `len(groupCols) > 0` half of the gate would be a claim no run could refute,
// which is precisely the shape of guard this repo has repeatedly found to be
// guarding nothing. If the candidate builder ever admits ungrouped aggregates,
// the SQL path arrives here already correct: an ungrouped aggregate has exactly
// one group, which exists whether or not the table has rows, so its zero is an
// answer and not residue.
func TestDropsVacatedGroups_OnlyGroupedRowCounts(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		countsRows bool
		groupCols  []string
		want       bool
		why        string
	}{
		{
			name:       "grouped COUNT(*)",
			countsRows: true,
			groupCols:  []string{"G"},
			want:       true,
			why:        "the stored value is the group's row count, so a zero can only be a vacated group",
		},
		{
			name:       "ungrouped COUNT(*)",
			countsRows: true,
			groupCols:  nil,
			want:       false,
			why: "an ungrouped aggregate has exactly one group, which exists whether or not the " +
				"table has rows — dropping its zero would turn COUNT(*) over an empty table into no row at all",
		},
		{
			name:       "grouped SUM",
			countsRows: false,
			groupCols:  []string{"G"},
			want:       false,
			why:        "a live group whose values cancel to zero legitimately answers 0",
		},
		{
			name:       "grouped COUNT(col)",
			countsRows: false,
			groupCols:  []string{"G"},
			want:       false,
			why:        "a live group whose every value is NULL legitimately answers 0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fn := expressions.AggSum
			aggColumn := "V"
			if tc.countsRows {
				fn = expressions.AggCount
				aggColumn = ""
			}
			cand := NewAggregateIndexMatchCandidate(
				"IDX", []string{"T"}, tc.groupCols, fn, aggColumn, nil, nil, len(tc.groupCols),
			).WithGroupExistence(tc.countsRows, []byte("g-signature"))
			if got := dropsVacatedGroups(cand); got != tc.want {
				t.Fatalf("dropsVacatedGroups(%s) = %v, want %v — %s", tc.name, got, tc.want, tc.why)
			}
		})
	}
}

// TestAggregateCandidateCarriesGroupExistenceFacts pins that the two facts the
// group-existence machinery reads are the ones the builder set, and that they
// are absent by default. A candidate built without them must never look like a
// COUNT(*) companion: countsRows false and a nil signature both decline, which
// is the fail-closed direction.
func TestAggregateCandidateCarriesGroupExistenceFacts(t *testing.T) {
	t.Parallel()

	bare := NewAggregateIndexMatchCandidate("IDX", []string{"T"}, []string{"G"}, expressions.AggCount, "", nil, nil, 1)
	if bare.CountsRows() {
		t.Fatal("a candidate built without group-existence facts claims to count rows; " +
			"the default must decline, or an index whose type was never inspected would be " +
			"treated as a group-existence oracle")
	}
	if bare.GroupingSignature() != nil {
		t.Fatal("a candidate built without group-existence facts reports a grouping signature")
	}

	set := NewAggregateIndexMatchCandidate("IDX", []string{"T"}, []string{"G"}, expressions.AggCount, "", nil, nil, 1).
		WithGroupExistence(true, []byte("sig"))
	if !set.CountsRows() {
		t.Fatal("WithGroupExistence did not record countsRows")
	}
	if string(set.GroupingSignature()) != "sig" {
		t.Fatalf("WithGroupExistence did not record the grouping signature, got %q", set.GroupingSignature())
	}
}
