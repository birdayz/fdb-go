package cascades

import "fdb.dev/pkg/recordlayer/query/plan/cascades/values"

// indexTestPlanContext is a minimal PlanContext stub carrying a fixed set of match
// candidates, used by data-access-path planner tests (e.g. the InExplode and benchmark
// tests) that drive the full planner against a hand-built candidate set. Extracted from
// the retired ImplementIndexScanRule's test file (RFC-076) so the surviving tests that
// depend on it keep compiling.
type indexTestPlanContext struct {
	candidates []MatchCandidate
}

func (c *indexTestPlanContext) GetPlannerConfiguration() PlannerConfiguration {
	return DefaultPlannerConfiguration()
}

func (c *indexTestPlanContext) GetMatchCandidates() []MatchCandidate {
	return c.candidates
}

func (c *indexTestPlanContext) GetPrimaryKeyColumns(string) []string {
	return nil
}

// testRecordRowType builds the descriptor-shaped row layout a candidate flows,
// from an ordered column-name list. Covering-column resolution is per record
// type and ORDINAL (RFC-197 item 1), so a test that wants a push to happen must
// give the candidate a real layout — an UnknownType candidate has no layout,
// states no domain, and correctly declines everything.
func testRecordRowType(name string, cols ...string) *values.RecordType {
	fields := make([]values.Field, len(cols))
	for i, c := range cols {
		fields[i] = values.Field{Name: c, FieldType: values.UnknownType, Ordinal: i}
	}
	return values.NewRecordType(name, false, fields)
}

// testColumnRef bakes a source-relative reference to a column of a layout built
// by testRecordRowType: the shape the SQL resolver produces for `t.c`, carrying
// the ordinal AND the domain that ordinal indexes.
func testColumnRef(child values.Value, rowType *values.RecordType, col string, typ values.Type) *values.FieldValue {
	for i, f := range rowType.Fields {
		if f.Name == col {
			return values.NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
				child, col, i, typ, values.OrdinalDomainOfType(rowType),
			)
		}
	}
	panic("testColumnRef: no column " + col + " in " + rowType.RecordName)
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
	return NewValueIndexScanMatchCandidateWithFunctions(
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
}
