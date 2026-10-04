package fleet

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer"
)

// TestSelectIndexesNarrowsToTheRequestedRollout pins the two properties an
// index roll-out depends on. An empty request must select EVERYTHING pending —
// otherwise "build whatever this tenant owes" silently becomes "build
// nothing". A non-empty request must select only what was asked, so a tenant
// carrying an unrelated half-built index is not dragged into someone else's
// roll-out.
func TestSelectIndexesNarrowsToTheRequestedRollout(t *testing.T) {
	t.Parallel()
	pending := []*recordlayer.Index{
		{Name: "T_BY_C"},
		{Name: "T_BY_V"},
		{Name: "UNRELATED_HALF_BUILT"},
	}

	if got := selectIndexes(pending, nil); len(got) != 3 {
		t.Fatalf("empty request selected %d indexes, want all 3 — an unfiltered fleet build "+
			"must build everything pending", len(got))
	}

	got := selectIndexes(pending, []string{"T_BY_C"})
	if len(got) != 1 || got[0].Name != "T_BY_C" {
		t.Fatalf("selectIndexes([T_BY_C]) = %v, want exactly [T_BY_C] — a targeted roll-out "+
			"must not touch a tenant's unrelated pending indexes", indexNames(got))
	}

	// Index identifiers are case-insensitive throughout the relational layer;
	// a roll-out typed in lower case must still match the stored name.
	if got := selectIndexes(pending, []string{"t_by_v"}); len(got) != 1 || got[0].Name != "T_BY_V" {
		t.Fatalf("selectIndexes([t_by_v]) = %v, want [T_BY_V]", indexNames(got))
	}

	// A name no tenant carries selects nothing, and that is NOT an error:
	// tenants sit at different template versions mid-migration.
	if got := selectIndexes(pending, []string{"NOT_ON_THIS_TENANT"}); len(got) != 0 {
		t.Fatalf("selectIndexes of an absent index = %v, want empty", indexNames(got))
	}
}

func indexNames(idx []*recordlayer.Index) []string {
	out := make([]string, 0, len(idx))
	for _, i := range idx {
		out = append(out, i.Name)
	}
	return out
}

// A pending index that a peer publishes between PendingIndexes and its session
// is left alone by that session, which must not count it as built: the tenant
// then reports no work instead of "built" with nothing built.
func TestBuiltBySessionCountsOnlyWhatTheSessionDid(t *testing.T) {
	t.Parallel()
	for outcome, want := range map[recordlayer.IndexBuildOutcome]bool{
		recordlayer.IndexBuildOutcomeNone:             false,
		recordlayer.IndexBuildOutcomeBuilt:            true,
		recordlayer.IndexBuildOutcomeLeftAlone:        false,
		recordlayer.IndexBuildOutcomePublished:        true,
		recordlayer.IndexBuildOutcomeCompletedByPeers: false,
	} {
		if got := builtBySession(outcome); got != want {
			t.Errorf("builtBySession(%d) = %v, want %v", outcome, got, want)
		}
	}
}

// TestBuildPendingReportsWhatTheTenantBuilt drives every arm of a tenant's
// event: nothing pending, every index built, some left to peers, all left to
// peers (no work), and a failure part way, which reports the indexes completed
// before it and not the failed one's records. A session that indexed records
// reports Built (LastBuildOutcome), so "left to peers" indexed none.
func TestBuildPendingReportsWhatTheTenantBuilt(t *testing.T) {
	t.Parallel()
	a, b, c := recordlayer.NewIndex("A", recordlayer.Field("x")), recordlayer.NewIndex("B", recordlayer.Field("y")), recordlayer.NewIndex("C", recordlayer.Field("z"))
	type result struct {
		n     int64
		built bool
		err   error
	}
	failure := errors.New("boom")
	for _, tc := range []struct {
		name    string
		pending []*recordlayer.Index
		results map[string]result
		want    Event
		wantErr bool
	}{
		{name: "nothing pending", want: Event{Outcome: OutcomeNoWork}},
		{
			name: "every index built", pending: []*recordlayer.Index{a, b},
			results: map[string]result{"A": {n: 3, built: true}, "B": {n: 4, built: true}},
			want:    Event{Outcome: OutcomeBuilt, Indexes: []string{"A", "B"}, Records: 7},
		},
		{
			name: "one left to a peer", pending: []*recordlayer.Index{a, b},
			results: map[string]result{"A": {n: 3, built: true}, "B": {n: 0, built: false}},
			want:    Event{Outcome: OutcomeBuilt, Indexes: []string{"A"}, Records: 3},
		},
		{
			name: "every index left to peers", pending: []*recordlayer.Index{a, b},
			results: map[string]result{"A": {n: 0}, "B": {n: 0}},
			want:    Event{Outcome: OutcomeNoWork},
		},
		{
			name: "a failure part way", pending: []*recordlayer.Index{a, b, c},
			results: map[string]result{"A": {n: 3, built: true}, "B": {n: 5, err: failure}},
			want:    Event{Indexes: []string{"A"}, Records: 3},
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var ran []string
			ev, err := buildPending(tc.pending, func(idx *recordlayer.Index) (int64, bool, error) {
				ran = append(ran, idx.Name)
				r := tc.results[idx.Name]
				return r.n, r.built, r.err
			})
			if tc.wantErr {
				if !errors.Is(err, failure) || !strings.Contains(err.Error(), `build index "B"`) {
					t.Fatalf("err = %v, want the failure of B", err)
				}
				if len(ran) != 2 {
					t.Fatalf("ran %v, want the build to stop at the failure", ran)
				}
			} else if err != nil {
				t.Fatalf("err = %v", err)
			}
			if ev.Err != nil {
				t.Fatalf("event carries Err %v; buildPending leaves it to the fan-out", ev.Err)
			}
			if ev.Outcome != tc.want.Outcome || ev.Records != tc.want.Records || !slices.Equal(ev.Indexes, tc.want.Indexes) {
				t.Fatalf("event = %+v, want %+v", ev, tc.want)
			}
		})
	}
}

// The advice names every way out the fleet's operator has and offers no
// continuation the fleet cannot run.
func TestPartlyBuiltAdviceOffersOnlyWhatTheFleetCanDo(t *testing.T) {
	t.Parallel()
	msg := partlyBuiltAdvice(&recordlayer.PartlyBuiltError{
		IndexName: "I", SavedStamp: "MUTUAL_BY_RECORDS", ExpectedStamp: "BY_RECORDS",
		Message: "This index was partly built by another method",
	}).Error()
	for _, want := range []string{
		`"MUTUAL_BY_RECORDS"`, `"BY_RECORDS"`, "(This index was partly built by another method)",
		"unblock it first", "finish it with the builder that began it", "frl index rebuild",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("advice missing %q:\n%s", want, msg)
		}
	}
	for _, unwanted := range []string{"same settings", "continue it with"} {
		if strings.Contains(msg, unwanted) {
			t.Errorf("advice offers %q, which the fleet cannot run:\n%s", unwanted, msg)
		}
	}
	if bare := partlyBuiltAdvice(&recordlayer.PartlyBuiltError{IndexName: "I"}).Error(); strings.Contains(bare, "()") {
		t.Errorf("an empty message renders as ():\n%s", bare)
	}
}
