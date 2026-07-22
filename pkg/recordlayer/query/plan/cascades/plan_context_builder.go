package cascades

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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

// IndexDefWithRowType is an optional extension of IndexDef for defs that
// can state the flowed row layout of the records the index serves (the
// descriptor-shaped positional type). Candidates built without one flow
// UnknownType, which disqualifies them from plans that must bind
// comparison keys to row slots at plan time (the pk-merge intersection).
type IndexDefWithRowType interface {
	IndexDef
	IndexRowType() values.Type
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

// IndexDefWithCreatesDuplicates is an optional extension of IndexDef for indexes
// that can state whether their root key expression FANS OUT (a repeated/collection
// field produces multiple entries per record). Ports Java's
// index.getRootExpression().createsDuplicates(). A fan-out index does NOT produce
// distinct records; defs that don't implement this interface are treated as
// non-fan-out (the safe under-report — see DistinctRecordsProperty / RFC-188 M4).
type IndexDefWithCreatesDuplicates interface {
	IndexDef
	IndexCreatesDuplicates() bool
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
		flowed := values.Type(values.UnknownType)
		if withRT, ok := def.(IndexDefWithRowType); ok {
			if t := withRT.IndexRowType(); t != nil {
				flowed = t
			}
		}
		// Fan-out (createsDuplicates) drives DistinctRecordsProperty (RFC-188 M4):
		// a repeated-field index does NOT produce distinct records. Threaded from
		// the def's root key expression when available. When the def does NOT
		// supply the signal, pass nil (unknown) — the property abstains to
		// distinct=false rather than assuming non-fan-out, so a fan-out index is
		// never mis-stamped as distinct (the safe under-report).
		var createsDuplicatesSignal *bool
		if dup, ok := def.(IndexDefWithCreatesDuplicates); ok {
			v := dup.IndexCreatesDuplicates()
			createsDuplicatesSignal = &v
		}
		candidates = append(candidates, NewValueIndexScanMatchCandidateWithFunctions(
			def.IndexName(),
			def.IndexRecordTypes(),
			upperCols,
			columnFns,
			aliases,
			flowed,
			def.IndexIsUnique(),
			upperPK,
			createsDuplicatesSignal,
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
