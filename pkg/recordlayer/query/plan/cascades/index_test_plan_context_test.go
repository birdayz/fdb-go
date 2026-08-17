package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// indexTestPlanContext is a minimal PlanContext stub carrying a fixed set of match
// candidates, used by data-access-path planner tests (e.g. the InExplode and benchmark
// tests) that drive the full planner against a hand-built candidate set. Extracted from
// the retired ImplementIndexScanRule's test file (RFC-076) so the surviving tests that
// depend on it keep compiling.
type indexTestPlanContext struct {
	candidates []MatchCandidate
	// readableIndexes is the run's index-state evidence. The zero value is
	// UNKNOWN — nobody consulted a store — which is what a rule unit test
	// truthfully is, and which is why a metadata-property proof over an index's
	// mutable state must not fire here. A test that wants such a proof to fire
	// has to say so, exactly as the production SELECT path does.
	readableIndexes ReadableIndexes
	pk              map[string][]string
}

func (c *indexTestPlanContext) GetPlannerConfiguration() PlannerConfiguration {
	cfg := DefaultPlannerConfiguration()
	cfg.ReadableIndexes = c.readableIndexes
	// A unit fixture EXECUTES nothing and PAGES nowhere, so "the whole result
	// comes from one read version" is true of it by construction. Stating it
	// keeps the single-read-version gate from silently disabling every proof
	// here, without weakening the gate: the condition really does hold.
	cfg.SingleReadVersion = true
	return cfg
}

func (c *indexTestPlanContext) GetMatchCandidates() []MatchCandidate {
	return c.candidates
}

// pk lets one context state BOTH licenses at once — a primary key covering the
// projection AND a qualifying unique index over it. That combination is the only
// way to observe that the stamp is written when the secondary-UNIQUE proof is the
// SOLE license and not merely whenever a unique index happens to qualify.
func (c *indexTestPlanContext) GetPrimaryKeyColumns(recordType string) []string {
	if c.pk == nil {
		return nil
	}
	return c.pk[recordType]
}

// testRecordRowType builds the descriptor-shaped row layout a candidate flows,
// from an ordered column-name list. Covering-column resolution is per record
// type and ORDINAL (RFC-197 item 1), so a test that wants a push to happen must
// give the candidate a real layout — an UnknownType candidate has no layout,
// states no domain, and correctly declines everything.
func testRecordRowType(name string, cols ...string) *values.RecordType {
	fields := make([]values.Field, len(cols))
	for i, c := range cols {
		fields[i] = values.Field{Name: c, FieldType: values.NullableLong, Ordinal: i}
	}
	return values.NewRecordType(name, false, fields)
}

// testColumnRef bakes a source-relative reference to a column of a layout built
// by testRecordRowType: the shape the SQL resolver produces for `t.c`, carrying
// the ordinal AND the domain that ordinal indexes.
func testColumnRef(child values.Value, rowType *values.RecordType, col string, _ values.Type) values.Value {
	for i, f := range rowType.Fields {
		if f.Name == col {
			if child == nil {
				var err error
				child, err = values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier(), rowType)
				if err != nil {
					panic(err)
				}
			}
			resolved, err := values.ResolveFieldOrdinals(child, []int{i})
			if err != nil {
				panic(err)
			}
			return resolved
		}
	}
	panic("testColumnRef: no column " + col + " in " + rowType.RecordName)
}

func syntheticIndexKeyTypes(size int) []values.Type {
	result := make([]values.Type, size)
	for i := range result {
		result[i] = values.NullableLong
	}
	return result
}

func withSyntheticIndexPlanKeyTypes(plan *plans.RecordQueryIndexPlan) *plans.RecordQueryIndexPlan {
	size := max(len(plan.GetScanComparisons()), len(plan.GetColumnNames()))
	return plan.WithKeyComponentTypes(syntheticIndexKeyTypes(size))
}

func withSyntheticScanPlanKeyTypes(plan *plans.RecordQueryScanPlan) *plans.RecordQueryScanPlan {
	size := max(len(plan.GetScanComparisons()), len(plan.GetPrimaryKeyValues()))
	return plan.WithKeyComponentTypes(syntheticIndexKeyTypes(size))
}

// newKnownDistinctValueIndexCandidate constructs an ordinary scalar value
// index for shortcut-rule tests. The production metadata adapter supplies this
// affirmative non-fanout signal; the legacy constructor intentionally leaves
// it unknown so missing metadata fails closed.
func newKnownDistinctValueIndexCandidate(
	indexName string,
	recordTypes []string,
	columnNames []string,
	sargableAliases []values.CorrelationIdentifier,
	flowedType values.Type,
	unique bool,
	pkColumnNames []string,
) *ValueIndexScanMatchCandidate {
	createsDuplicates := false
	candidate := NewValueIndexScanMatchCandidateWithFunctions(
		indexName,
		recordTypes,
		columnNames,
		nil,
		sargableAliases,
		flowedType,
		unique,
		pkColumnNames,
		&createsDuplicates,
	)
	// Synthetic planner fixtures do not have descriptor metadata. Give every
	// advertised key coordinate an explicit scalar tuple domain so they test the
	// planner behavior they name without exercising the production Unknown-type
	// rejection path. Tests for physical FLOAT/DOUBLE behavior override this
	// vector with WithKeyComponentTypes.
	return candidate.
		WithKeyComponentTypes(syntheticIndexKeyTypes(len(columnNames))).
		WithPrimaryKeyComponentTypes(syntheticIndexKeyTypes(len(pkColumnNames)))
}
