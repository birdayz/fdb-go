package cascades

import (
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// IndexDef describes a secondary index for PlanContext construction.
// This is an adapter interface so the cascades package doesn't
// depend on the recordlayer package directly (avoiding import cycles).
type IndexDef interface {
	IndexName() string
	IndexColumnNames() []string
	IndexRecordTypes() []string
	IndexIsUnique() bool
}

// NewPlanContextFromIndexDefs builds a PlanContext with one
// ValueIndexScanMatchCandidate per index definition. Column names
// are upper-cased for SQL-convention case-insensitive matching
// (FieldValue.Field is upper-cased by the SQL resolver).
func NewPlanContextFromIndexDefs(defs []IndexDef) PlanContext {
	candidates := make([]MatchCandidate, 0, len(defs))
	for _, def := range defs {
		cols := def.IndexColumnNames()
		if len(cols) == 0 {
			continue
		}
		upperCols := make([]string, len(cols))
		for i, c := range cols {
			upperCols[i] = strings.ToUpper(c)
		}
		aliases := make([]values.CorrelationIdentifier, len(cols))
		for i := range cols {
			aliases[i] = values.UniqueCorrelationIdentifier()
		}
		candidates = append(candidates, NewValueIndexScanMatchCandidate(
			def.IndexName(),
			def.IndexRecordTypes(),
			upperCols,
			aliases,
			values.UnknownType,
			def.IndexIsUnique(),
		))
	}
	return &builtPlanContext{candidates: candidates}
}

// NewPlanContextFromMatchCandidates builds a PlanContext from pre-built
// MatchCandidates. Use this when you have a mix of ValueIndexScan and
// AggregateIndex candidates.
func NewPlanContextFromMatchCandidates(candidates []MatchCandidate) PlanContext {
	return &builtPlanContext{candidates: candidates}
}

type builtPlanContext struct {
	candidates []MatchCandidate
}

func (c *builtPlanContext) GetPlannerConfiguration() PlannerConfiguration {
	return DefaultPlannerConfiguration()
}

func (c *builtPlanContext) GetMatchCandidates() []MatchCandidate {
	return c.candidates
}

func (c *builtPlanContext) GetPrimaryKeyColumns(string) []string { return nil }
