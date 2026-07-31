package factory

import (
	"fmt"

	"fdb.dev/pkg/relational/conformance/rowdiff"
)

// GeneratorVersion identifies the candidate-derivation rules. It is recorded
// in every emitted file and MUST be bumped whenever a change here would make a
// seed produce a different candidate — otherwise a committed file claims a
// reproduction recipe that no longer reproduces it, and TestFactoryDeterminism
// is the thing that notices.
const GeneratorVersion = "rowdiff-gen/1"

// FactoryRowCap bounds a case's row count.
//
// rowdiff generates 20-120 rows because its comparison is in-memory and free.
// The factory's rows are WRITTEN INTO EVERY FILE, once as an INSERT and again
// as expectations, so the same number would make a thousand-file batch
// unreviewable — and an expectation nobody can read is not a reviewable proof,
// which is half the argument for committing them. The cap keeps the NULL rate
// (~1 in 6 per nullable column) and the duplicate-heavy 0..9 integer domain
// intact, which are the properties the predicates actually exercise; it costs
// the rarer boundary values (-1 and 2^62, ~1 in 20), which the un-capped
// rowdiff nightly still sweeps.
const FactoryRowCap = 24

// MaxExpectationRows bounds how many rows a committed test may assert.
//
// Cross-join shapes can return thousands of rows for a 24-row table, and a
// file holding a 4,000-row expectation is a diff nobody reviews and a
// regression nobody can localise. Over-large candidates are DROPPED with a
// counted reason, never truncated: a truncated expectation would be a
// silently weaker assertion wearing a frozen expectation's clothes.
const MaxExpectationRows = 80

// Candidate is one generation unit — a case, one query within it, one
// projection variant — that the TLP oracle can be built over.
type Candidate struct {
	Seed          uint64
	QueryIndex    int
	ProjIndex     int
	Case          *rowdiff.Case
	Query         rowdiff.Query
	Projection    []string
	FeatureVector string
}

// Name is the candidate's stable file name: seed, query and projection, which
// together identify it uniquely and reproducibly.
func (c Candidate) Name() string {
	return fmt.Sprintf("fc_%010d_q%d_p%d", c.Seed, c.QueryIndex, c.ProjIndex)
}

// TLPQueries returns the four renderings of the ternary-logic partition, in
// the order (base, p, NOT p, p IS NULL).
//
// The triple is emitted FROM THE TYPED PREDICATE SPEC, not by rewriting parsed
// SQL. That is a deliberate choice and the cheaper one by a wide margin: a
// rewriter has to re-parse the query, find the WHERE, and reproduce operator
// precedence when it wraps — three places to be subtly wrong, each of which
// turns a correct engine into a reported bug. The generator already HAS the
// predicate as a tree; rendering it three more ways is the same renderer
// called three more times.
//
// Composition with the rest of the WHERE is what makes this valid on more than
// toy queries. The override replaces only the predicate-tree conjunct, so a
// comma join's equality and an appended EXISTS stay identical across all four
// renderings and factor out of the partition exactly as the FROM clause does.
func (c Candidate) TLPQueries() []rowdiff.Query {
	p := rowdiff.PredicateSQL(c.Query.Where)
	variants := []string{"", "(" + p + ")", "NOT (" + p + ")", "(" + p + ") IS NULL"}
	out := make([]rowdiff.Query, 0, len(variants))
	for i := range variants {
		q := c.Query
		q.WhereOverride = &variants[i]
		out = append(out, q)
	}
	return out
}

// TLPLabels names the four renderings, in TLPQueries order, for diagnostics.
var TLPLabels = []string{"unfiltered", "p", "not-p", "p-is-null"}

// Candidates enumerates a seed's TLP-eligible candidates.
//
// Eligibility is narrow, and every exclusion is a property of the ORACLE, not
// a limitation of the engine — the excluded shapes are covered by rowdiff's
// Oracle M and by the hand-authored corpus:
//
//   - No WHERE: nothing to partition on.
//   - Aggregate: the output is one row per group, not a row set the input
//     partition maps onto. `COUNT(*)` over p, NOT p and p IS NULL sums to the
//     unfiltered count, but SUM/MIN/MAX do not, and an oracle that holds for
//     some functions is not an oracle.
//   - UNION and derived-table: they render through their own SQL paths, which
//     the WHERE override does not reach. Silently rendering them unmodified
//     would produce four identical queries and a partition that "holds"
//     trivially — a tautological oracle, the exact instrument failure class
//     RFC-201 §8.5 gates against.
//   - LIMIT: the partition is over the FULL result; truncating each branch
//     independently does not reassemble.
//   - DISTINCT: dedup does not distribute over a partition. A value appearing
//     in both the p and the NOT-p branch survives once in each branch and once
//     in the union — three rows on the left, one on the right.
func Candidates(seed uint64) []Candidate {
	c := rowdiff.Generate(seed)
	if len(c.Rows) > FactoryRowCap {
		c.Rows = c.Rows[:FactoryRowCap]
	}
	var out []Candidate
	for qi, q := range c.Queries {
		if !tlpEligible(q) {
			continue
		}
		for pi, proj := range c.ProjectionsFor(q) {
			out = append(out, Candidate{
				Seed:          seed,
				QueryIndex:    qi,
				ProjIndex:     pi,
				Case:          c,
				Query:         q,
				Projection:    proj,
				FeatureVector: FeatureVector(c, q, proj),
			})
		}
	}
	return out
}

func tlpEligible(q rowdiff.Query) bool {
	switch {
	case q.Where == nil,
		q.Agg != nil,
		q.Union != nil,
		q.Derived != nil,
		q.Limit > 0,
		q.Distinct:
		return false
	}
	return true
}
