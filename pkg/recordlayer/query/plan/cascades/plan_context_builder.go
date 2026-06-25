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
	IndexPrimaryKeyColumns() []string
}

// IndexDefWithColumnFunctions is an optional extension of IndexDef for indexes
// whose key columns are not all bare fields. IndexColumnFunctions returns a
// slice parallel to IndexColumnNames: entry i is the function wrapping the i-th
// column ("" for a plain field, FunctionKindCardinality for a CARDINALITY()
// column). A nil/empty return means every column is a plain field. Defs that
// don't implement this interface are treated as all-plain-field.
type IndexDefWithColumnFunctions interface {
	IndexDef
	IndexColumnFunctions() []string
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
		var upperPK []string
		if pkCols := def.IndexPrimaryKeyColumns(); len(pkCols) > 0 {
			upperPK = make([]string, len(pkCols))
			for i, c := range pkCols {
				upperPK[i] = strings.ToUpper(c)
			}
		}
		// Carry per-column function tags (e.g. CARDINALITY) when the def
		// provides them, so a function-keyed column matches by its Value, not
		// a bare field name.
		var columnFns []string
		if withFns, ok := def.(IndexDefWithColumnFunctions); ok {
			columnFns = withFns.IndexColumnFunctions()
		}
		candidates = append(candidates, NewValueIndexScanMatchCandidateWithFunctions(
			def.IndexName(),
			def.IndexRecordTypes(),
			upperCols,
			columnFns,
			aliases,
			values.UnknownType,
			def.IndexIsUnique(),
			upperPK,
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
