package cascades

import (
	"strings"

	"fdb.dev/gen"
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

// IndexDefWithRecordTypeRowTypes is an optional extension of IndexDef for defs
// that can state the row layout of EACH record type the index serves.
//
// IndexRowType above answers only for a single-record-type index and degrades
// to UnknownType otherwise, because a multi-type index's rows have no one
// layout. That degradation is correct for anything that needs THE layout, and
// useless for covering-column resolution, which needs a per-type answer: an
// ordinal is meaningful against one type's descriptor and meaningless against
// another's (RFC-197 item 1). Defs that don't implement this leave a multi-type
// candidate with no layout at all, which fails closed — the push is declined
// rather than resolved against a layout that does not exist.
type IndexDefWithRecordTypeRowTypes interface {
	IndexDef
	IndexRecordTypeRowTypes() []values.Type
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
// distinct records; defs that don't implement this interface remain UNKNOWN,
// so cardinality-sensitive properties and shortcuts abstain.
type IndexDefWithCreatesDuplicates interface {
	IndexDef
	IndexCreatesDuplicates() bool
}

// IndexDefWithRootKeyExpression is an optional extension of IndexDef for defs
// that can expose the record-layer key-expression AST. The value-index
// expansion uses this structure to reproduce Java's fan-out candidate graph
// (a correlated Explode under an inner Select) instead of flattening a
// repeated field into an ordinary scalar placeholder. Implementations must
// return a caller-owned message; NewPlanContextFromIndexDefs defensively
// clones it again when attaching it to the candidate.
type IndexDefWithRootKeyExpression interface {
	IndexDef
	IndexRootKeyExpression() *gen.KeyExpression
}

// IndexDefWithCommonPrimaryKey is an optional extension of IndexDef exposing the
// index's common primary key translated to structure-encoding Values (record-type
// -key prefixes, nesting) — the input to PrimaryKeyProperty (RFC-189 B3). Defs
// that don't implement it (or return nil) leave the property abstaining, so the
// DistinctUnion dedup optimization is disabled rather than risking a wrong one.
type IndexDefWithCommonPrimaryKey interface {
	IndexDef
	IndexCommonPrimaryKeyValues() []values.Value
}

// NewPlanContextFromIndexDefs builds a PlanContext with one
// ValueIndexScanMatchCandidate per index definition. Column names
// are upper-cased for SQL-convention case-insensitive matching
// (FieldValue.Field is upper-cased by the SQL resolver).
func NewPlanContextFromIndexDefs(defs []IndexDef) PlanContext {
	candidates := make([]MatchCandidate, 0, len(defs))
	for _, def := range defs {
		var rootKeyExpression *gen.KeyExpression
		if withRoot, ok := def.(IndexDefWithRootKeyExpression); ok {
			rootKeyExpression = withRoot.IndexRootKeyExpression()
		}
		if keyExpressionContainsNonFanOutNestedLeaf(rootKeyExpression) {
			// The candidate's column/coverage bridge cannot yet preserve the
			// path identity of a scalar nested leaf: ADDR.CITY could bind a
			// top-level CITY. Reject the whole root even when another branch
			// fans out. Leaves below a fan-out parent, and nested leaves that
			// fan out themselves, are structurally represented by the
			// Explode-based expansion and remain supported.
			continue
		}
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
		// Structural common PK (RFC-189 B3) — for PrimaryKeyProperty; nil unless
		// the def supplies it, in which case the property abstains.
		var commonPK []values.Value
		if pkDef, ok := def.(IndexDefWithCommonPrimaryKey); ok {
			commonPK = pkDef.IndexCommonPrimaryKeyValues()
		}
		candidate := NewValueIndexScanMatchCandidateWithFunctions(
			def.IndexName(),
			def.IndexRecordTypes(),
			upperCols,
			columnFns,
			aliases,
			flowed,
			def.IndexIsUnique(),
			upperPK,
			createsDuplicatesSignal,
		).WithCommonPrimaryKey(commonPK)
		if perType, ok := def.(IndexDefWithRecordTypeRowTypes); ok {
			candidate.WithRecordTypeRowTypes(perType.IndexRecordTypeRowTypes())
		}
		if rootKeyExpression != nil {
			candidate.WithRootKeyExpression(rootKeyExpression)
		}
		candidates = append(candidates, candidate)
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
