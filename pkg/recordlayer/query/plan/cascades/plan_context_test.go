package cascades

import (
	"reflect"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestEmptyPlanContext_NotNil(t *testing.T) {
	t.Parallel()
	ctx := EmptyPlanContext()
	if ctx == nil {
		t.Fatal("EmptyPlanContext returned nil")
	}
	cfg := ctx.GetPlannerConfiguration()
	if cfg.AllowDuplicateProjections {
		t.Fatal("default config has AllowDuplicateProjections=true; should be false")
	}
	if got := ctx.GetMatchCandidates(); got != nil && len(got) != 0 {
		t.Fatalf("empty context returned non-empty candidates: %v", got)
	}
}

func TestDefaultPlannerConfiguration_JavaDefaults(t *testing.T) {
	t.Parallel()
	cfg := DefaultPlannerConfiguration()
	// Java defaults deferCrossProducts ON (RecordQueryPlannerConfiguration:
	// DONT_DEFER_CROSS_PRODUCTS unset); every other flag is zero-valued.
	// reflect.DeepEqual, not ==: the config carries the readable-index view,
	// which holds a set. The default must be the UNRESTRICTED view — Java's
	// Optional.empty() — because planning also happens from unit tests, offline
	// harnesses and the DDL front end, none of which can know index states, and
	// defaulting them to "nothing is readable" would delete every index plan
	// they have.
	if !reflect.DeepEqual(cfg, PlannerConfiguration{ShouldDeferCrossProducts: true}) {
		t.Fatalf("DefaultPlannerConfiguration diverges from the Java defaults: %+v", cfg)
	}
	if cfg.ReadableIndexes.IsRestricted() {
		t.Fatal("the default readable-index view is RESTRICTED; it must be Java's " +
			"Optional.empty() (all indexes readable), or every planning site that " +
			"never learned about index states silently loses all of them")
	}
	if !cfg.ReadableIndexes.Allows("ANY_INDEX_NAME") {
		t.Fatal("the default readable-index view rejects an index name")
	}
	// The default is PERMISSIVE but not AFFIRMATIVE. A planning run that never
	// consulted a record store has established nothing about index state, so it
	// may scan every index and prove nothing from any of them (RFC-210 §5.1.1).
	// Without this assertion the tri-state re-collapses to two the moment
	// AllIndexesReadable is made the zero value again — which is exactly how
	// the collapse survived review the first time.
	if cfg.ReadableIndexes.IndexStatesEstablished() {
		t.Fatal("the default readable-index view claims index state was ESTABLISHED; " +
			"the zero value means nobody consulted the store, and treating it as an " +
			"affirmative all-readable assertion lets an unchecked index license a " +
			"uniqueness proof")
	}
}

// TestReadableIndexes_TriStateIsThreeStates pins that the three views are
// mutually distinguishable on both axes that matter — whether an index may back
// a SCAN, and whether it may back a PROOF. The two axes are independent, and
// the whole design rests on their independence: UNKNOWN is permissive on the
// first and refusing on the second. A two-valued rendering necessarily agrees
// with this test on exactly one of the three rows.
func TestReadableIndexes_TriStateIsThreeStates(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		view        ReadableIndexes
		allows      bool
		established bool
		restricted  bool
	}{
		{"unknown", IndexStatesUnknown(), true, false, false},
		{"unknown-is-the-zero-value", ReadableIndexes{}, true, false, false},
		{"all-readable", AllIndexesReadable(), true, true, false},
		{"restricted-including", OnlyReadableIndexes(map[string]struct{}{"BY_EMAIL": {}}), true, true, true},
		{"restricted-excluding", OnlyReadableIndexes(map[string]struct{}{"OTHER": {}}), false, true, true},
		{"restricted-empty", OnlyReadableIndexes(nil), false, true, true},
	} {
		if got := tc.view.Allows("BY_EMAIL"); got != tc.allows {
			t.Errorf("%s: Allows(BY_EMAIL) = %v, want %v", tc.name, got, tc.allows)
		}
		if got := tc.view.IndexStatesEstablished(); got != tc.established {
			t.Errorf("%s: IndexStatesEstablished() = %v, want %v", tc.name, got, tc.established)
		}
		if got := tc.view.IsRestricted(); got != tc.restricted {
			t.Errorf("%s: IsRestricted() = %v, want %v", tc.name, got, tc.restricted)
		}
	}
	// The property that makes IsRestricted the WRONG gate for a proof, stated
	// as an assertion rather than a comment: a healthy store yields the
	// UNRESTRICTED affirmative form, so "restricted" and "established" are not
	// the same question.
	if AllIndexesReadable().IsRestricted() {
		t.Fatal("AllIndexesReadable is restricted")
	}
	if !AllIndexesReadable().IndexStatesEstablished() {
		t.Fatal("AllIndexesReadable does not establish index state; then no healthy " +
			"store could ever license a proof, and the gate is backwards")
	}
}

// stubMatchCandidate is a minimal MatchCandidate impl for verifying
// the interface is callable + the EmptyPlanContext can be replaced
// with a richer one when a rule needs it.
type stubMatchCandidate struct{ name string }

func (s stubMatchCandidate) CandidateName() string                              { return s.name }
func (s stubMatchCandidate) GetColumnNames() []string                           { return nil }
func (s stubMatchCandidate) GetSargableAliases() []values.CorrelationIdentifier { return nil }
func (s stubMatchCandidate) GetRecordTypes() []string                           { return nil }
func (s stubMatchCandidate) IsUnique() bool                                     { return false }
func (s stubMatchCandidate) GetTraversal() *Traversal                           { return nil }
func (s stubMatchCandidate) ComputeBoundParameterPrefixMap(
	_ map[values.CorrelationIdentifier]*predicates.ComparisonRange,
) map[values.CorrelationIdentifier]*predicates.ComparisonRange {
	return nil
}

func (s stubMatchCandidate) ToScanPlan(
	_ map[values.CorrelationIdentifier]*predicates.ComparisonRange, _ bool,
) plans.RecordQueryPlan {
	return nil
}

func TestMatchCandidate_Interface(t *testing.T) {
	t.Parallel()
	c := stubMatchCandidate{name: "Order$price"}
	var mc MatchCandidate = c
	if mc.CandidateName() != "Order$price" {
		t.Fatalf("CandidateName=%q, want Order$price", mc.CandidateName())
	}
}

// stubPlanContext lets a test fixture wire a non-empty context into
// rule invocations once those land.
type stubPlanContext struct {
	cfg        PlannerConfiguration
	candidates []MatchCandidate
}

func (s stubPlanContext) GetPlannerConfiguration() PlannerConfiguration { return s.cfg }
func (s stubPlanContext) GetMatchCandidates() []MatchCandidate          { return s.candidates }

func TestPlanContext_StubFixture(t *testing.T) {
	t.Parallel()
	ctx := stubPlanContext{
		cfg:        PlannerConfiguration{AllowDuplicateProjections: true},
		candidates: []MatchCandidate{stubMatchCandidate{name: "X"}},
	}
	if !ctx.GetPlannerConfiguration().AllowDuplicateProjections {
		t.Fatal("config flag not preserved")
	}
	if len(ctx.GetMatchCandidates()) != 1 {
		t.Fatalf("candidate count=%d, want 1", len(ctx.GetMatchCandidates()))
	}
}
