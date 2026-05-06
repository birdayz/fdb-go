package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
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

func TestDefaultPlannerConfiguration_ZeroFields(t *testing.T) {
	t.Parallel()
	cfg := DefaultPlannerConfiguration()
	if cfg != (PlannerConfiguration{}) {
		t.Fatalf("DefaultPlannerConfiguration not zero-valued: %+v", cfg)
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
