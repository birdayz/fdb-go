package explaindiff_test

import (
	"strings"
	"testing"

	"fdb.dev/pkg/relational/conformance/explaindiff"
)

// selectListOf returns the portion of a SQL string between SELECT and the FIRST
// FROM — the select list.
//
// THE FIRST `FROM`, NOT THE LAST, IS THE WHOLE POINT. A projected EXISTS embeds
// a subquery that has its own FROM, so taking the select list as everything up
// to the last FROM swallows the subquery and reports a hit for every query with
// `WHERE EXISTS (SELECT … FROM …)`. Re-deriving this census with the greedy
// form returns 119 over this corpus where the true select-list count was 0. If
// a future reader "corrects" the zero, this is why they should not.
func selectListOf(sql string) string {
	up := strings.ToUpper(sql)
	s := strings.Index(up, "SELECT ")
	if s < 0 {
		return ""
	}
	rest := up[s+len("SELECT "):]
	if f := strings.Index(rest, " FROM "); f >= 0 {
		return rest[:f]
	}
	return rest // a FROM-less SELECT (VALUES-style); the whole tail is the list.
}

// projectedExistsCensus is the reading this file gates on.
type projectedExistsCensus struct {
	// SelectListExists counts corpus entries that project an EXISTS into the
	// SELECT list — the population the RFC-218 fold operates on.
	SelectListExists int
	// FoldFixture counts entries from the RFC-218 fixture file specifically.
	FoldFixture int
	// FoldedWithHiddenSortColumn counts fixture entries that PLANNED with both a
	// sort and the fold's cleanup projection — i.e. a hidden sort column was
	// appended and dropped again.
	FoldedWithHiddenSortColumn int
	// DeclinedUnsupported counts fixture entries recorded as a plan error under
	// a corpus stanza that pins one. WATCHED FOR GROWTH, NOT COLLAPSE: while the
	// JOIN arm refused a nested key this was a floor, because a recorded decline
	// was the only thing keeping a later wrong-order regression visible. The arm
	// plans now, so zero is the steady state and a reappearing decline means a
	// shape stopped planning.
	DeclinedUnsupported int
	// ControlNoCleanupProjection counts fixture entries that planned with NO
	// cleanup projection — the single-accessor control. Without a non-zero here
	// the "cleanup projection appears" arm could be true of every folded query.
	ControlNoCleanupProjection int
	// UnpinnedFailures counts fixture entries that failed to plan WITHOUT a
	// corpus error pin. Always a defect: it is a query that stopped planning.
	UnpinnedFailures int
}

const foldFixtureFile = "projected_exists_nested_sort_key.yaml"

// censusProjectedExists is a pure function of the entries so every arm can be
// driven from a unit test rather than only by whatever the corpus happens to
// contain today.
func censusProjectedExists(entries []explaindiff.Entry) projectedExistsCensus {
	var c projectedExistsCensus
	for _, e := range entries {
		if strings.Contains(selectListOf(e.SQL), "EXISTS") {
			c.SelectListExists++
		}
		if e.File != foldFixtureFile {
			continue
		}
		c.FoldFixture++
		switch {
		case e.Failed() && e.ErrorPin != "":
			c.DeclinedUnsupported++
		case e.Failed():
			c.UnpinnedFailures++
		case strings.HasPrefix(e.Plan, "Project(") && strings.Contains(e.Plan, "InMemorySort"):
			c.FoldedWithHiddenSortColumn++
		case !strings.HasPrefix(e.Plan, "Project("):
			c.ControlNoCleanupProjection++
		}
	}
	return c
}

// assertFoldCoverage drives the verdict off explicit state so the decision is
// testable without a corpus.
//
// `floors` is the acceptable reading, and it is NOT uniformly a minimum: most
// counts are populations whose COLLAPSE is the alarm, while DeclinedUnsupported
// and UnpinnedFailures are populations whose GROWTH is. Each check states its
// own direction rather than sharing one, because the direction of this gate has
// already inverted once.
func assertFoldCoverage(t *testing.T, got, floors projectedExistsCensus) {
	t.Helper()
	if got.SelectListExists < floors.SelectListExists {
		t.Errorf("projected (SELECT-list) EXISTS entries = %d, want >= %d — the "+
			"RFC-218 fold would again be exercised by NO corpus query, and "+
			"TestPlanShapeGolden would pass VACUOUSLY for any change to it",
			got.SelectListExists, floors.SelectListExists)
	}
	if got.FoldFixture < floors.FoldFixture {
		t.Errorf("%s contributed %d entries, want >= %d — the fixture is not being "+
			"REACHED by the baseline run. A corpus entry no run touches is the same "+
			"vacuity one layer up", foldFixtureFile, got.FoldFixture, floors.FoldFixture)
	}
	if got.FoldedWithHiddenSortColumn < floors.FoldedWithHiddenSortColumn {
		t.Errorf("folded-with-hidden-sort-column entries = %d, want >= %d — the "+
			"nested-key arm is no longer planning, so the corpus records no evidence "+
			"the fold threads a multi-accessor key through",
			got.FoldedWithHiddenSortColumn, floors.FoldedWithHiddenSortColumn)
	}
	if got.DeclinedUnsupported > floors.DeclinedUnsupported {
		t.Errorf("recorded declines = %d, want <= %d — THE ALARM ON THIS COUNT HAS "+
			"INVERTED. It was a FLOOR while the JOIN arm refused a nested key: a "+
			"recorded decline was the only thing making a later silent wrong-order "+
			"regression visible. The leg-window re-anchor landed (RFC-220), the arm "+
			"plans, and the fixture asserts an ORDER instead — so zero is now the "+
			"steady state and the danger is GROWTH: a decline reappearing means a "+
			"shape that used to plan has gone back to refusing, and the query it "+
			"refuses is a capability regression, not a safety net",
			got.DeclinedUnsupported, floors.DeclinedUnsupported)
	}
	if got.ControlNoCleanupProjection < floors.ControlNoCleanupProjection {
		t.Errorf("single-accessor controls = %d, want >= %d — with the control gone, "+
			"'a cleanup projection appears' no longer discriminates: it could be true "+
			"of every folded query",
			got.ControlNoCleanupProjection, floors.ControlNoCleanupProjection)
	}
	if got.UnpinnedFailures > floors.UnpinnedFailures {
		t.Errorf("unpinned plan failures in %s = %d, want <= %d — a query that "+
			"stopped planning without a corpus error pin",
			foldFixtureFile, got.UnpinnedFailures, floors.UnpinnedFailures)
	}
}

// TestCorpusCoversTheProjectedExistsFold is RFC-218 §6's merge gate, kept as a
// standing assertion rather than a one-time reading.
//
// The gate is NOT "the corpus contains the shape" — it is "the baseline run
// demonstrably REACHED it". Those are different facts, and only the second one
// makes TestPlanShapeGolden's verdict mean anything about this fold. Before the
// fixture existed the golden covered 2506 queries and zero of them projected an
// EXISTS, so "no plan movement" was absence of coverage wearing the costume of
// evidence.
func TestCorpusCoversTheProjectedExistsFold(t *testing.T) {
	t.Parallel()

	entries, st, err := explaindiff.Collect(corpusDir)
	if err != nil {
		t.Fatalf("collect corpus: %v", err)
	}
	// Guard the population before reading the verdict: a census over an empty or
	// truncated corpus reports clean for the wrong reason.
	if st.Queries == 0 || len(entries) == 0 {
		t.Fatalf("empty corpus reading: %d entries, %d queries — the census below "+
			"would pass by measuring nothing", len(entries), st.Queries)
	}

	got := censusProjectedExists(entries)
	t.Logf("projected-EXISTS coverage: selectListExists=%d fixture=%d folded=%d "+
		"declined=%d control=%d unpinnedFailures=%d (corpus: %d files, %d queries)",
		got.SelectListExists, got.FoldFixture, got.FoldedWithHiddenSortColumn,
		got.DeclinedUnsupported, got.ControlNoCleanupProjection, got.UnpinnedFailures,
		st.Files, st.Queries)

	assertFoldCoverage(t, got, projectedExistsCensus{
		SelectListExists:           6,
		FoldFixture:                6,
		FoldedWithHiddenSortColumn: 5,
		DeclinedUnsupported:        0,
		ControlNoCleanupProjection: 1,
		UnpinnedFailures:           0,
	})
}

// TestFoldCoverageGateDrivesEveryArm drives the gate's decision from synthetic
// state, because a corpus run only exercises the arms the corpus reaches. Every
// floor and the vacuity guard get their own case, so an arm that is rare today
// cannot ship untested and be mistaken for a finding the first time it fires.
func TestFoldCoverageGateDrivesEveryArm(t *testing.T) {
	t.Parallel()

	full := projectedExistsCensus{
		SelectListExists: 6, FoldFixture: 6, FoldedWithHiddenSortColumn: 5,
		DeclinedUnsupported: 0, ControlNoCleanupProjection: 1, UnpinnedFailures: 0,
	}
	for _, tc := range []struct {
		name string
		got  projectedExistsCensus
	}{
		{"select_list_exists_collapsed", func() projectedExistsCensus {
			c := full
			c.SelectListExists = 0
			return c
		}()},
		{"fixture_not_reached", func() projectedExistsCensus {
			c := full
			c.FoldFixture = 0
			return c
		}()},
		{"folded_arm_stopped_planning", func() projectedExistsCensus {
			c := full
			c.FoldedWithHiddenSortColumn = 2
			return c
		}()},
		// The alarm on this count INVERTED with RFC-220: zero is the steady
		// state now that the JOIN arm plans, so the case that must red is a
		// decline REAPPEARING — a shape that used to plan going back to refusing.
		{"a_decline_reappeared", func() projectedExistsCensus {
			c := full
			c.DeclinedUnsupported = 1
			return c
		}()},
		{"control_gone", func() projectedExistsCensus {
			c := full
			c.ControlNoCleanupProjection = 0
			return c
		}()},
		{"an_unpinned_failure_appeared", func() projectedExistsCensus {
			c := full
			c.UnpinnedFailures = 1
			return c
		}()},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var probe testing.T
			assertFoldCoverage(&probe, tc.got, full)
			if !probe.Failed() {
				t.Fatalf("the gate stayed GREEN for %s — that floor is decorative", tc.name)
			}
		})
	}

	t.Run("the_healthy_reading_passes", func(t *testing.T) {
		t.Parallel()
		var probe testing.T
		assertFoldCoverage(&probe, full, full)
		if probe.Failed() {
			t.Fatal("the gate reds on the reading it is supposed to accept — it would " +
				"fail closed on every run and stop meaning anything")
		}
	})
}

// TestSelectListOfEndsAtTheFirstFrom pins the greedy-vs-non-greedy distinction
// the whole gate reading rests on. Without it, a "simplification" to the last
// FROM would silently inflate the census by counting every WHERE-EXISTS query,
// and the gate would then read as satisfied while covering nothing.
func TestSelectListOfEndsAtTheFirstFrom(t *testing.T) {
	t.Parallel()

	const whereExists = "SELECT id FROM orders WHERE EXISTS (SELECT k FROM flags) ORDER BY id"
	if got := selectListOf(whereExists); strings.Contains(got, "EXISTS") {
		t.Fatalf("a WHERE-EXISTS query counted as a PROJECTED EXISTS; select list "+
			"was %q — the extractor ran past the first FROM into the subquery", got)
	}

	const projected = "SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h FROM t1 ORDER BY n.sk"
	if got := selectListOf(projected); !strings.Contains(got, "EXISTS") {
		t.Fatalf("a genuinely PROJECTED EXISTS was missed; select list was %q", got)
	}
}
