// Package factory is the producer half of the RFC-201 §5 generation factory:
// generate → execute → bless-or-file → dedup, with cmd/factory-run driving it
// and pkg/relational/conformance/factorycorpus owning the committed output.
//
// The split matters. Nothing in the corpus package imports this one, so a
// committed test keeps running when the generator is broken, replaced or
// deleted — which is the entire argument for committing generated tests rather
// than regenerating them (§5, "Why commit rather than regenerate").
//
// Generation is not grammar enumeration. It extends rowdiff's typed-spec
// generator: seeded PCG, a Case/Query/Pred/JoinSpec/AggSpec struct family, and
// renderers from those structs to SQL. That is the proven substrate in this
// tree, and it hands the factory something an ANTLR walk would not — the SPEC
// STRUCT IS THE FEATURE VECTOR. A grammar walker knows which productions it
// fired; the spec struct knows the query's whole shape, including facts no
// production carries (which columns are indexed, whether the join is an ON
// form or a comma form, whether a leaf is sargable), and those are the axes
// coverage has to be measured on.
package factory

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/relational/conformance/rowdiff"
)

// FeatureVector serializes the structural identity of a candidate: everything
// about the query except its literal VALUES.
//
// Erasing literals is the whole design. `a > 3` and `a > 7` are the same test;
// committing both is the "90k variants of one shape" §5.4 names as volume
// without coverage. What survives is what a coverage claim can be made about:
// the query family, the predicate tree's structure and leaf kinds, the sort
// and projection shape, and the index set the planner had available — that
// last one because the same predicate over an indexed and an unindexed column
// are genuinely different tests of the engine.
//
// The rendering is stable and sorted wherever order is not semantic, so the
// same shape always produces the same string and the census can count on it.
func FeatureVector(c *rowdiff.Case, q rowdiff.Query, projection []string) string {
	var parts []string
	parts = append(parts, "shape="+shapeTag(q))
	parts = append(parts, "idx="+indexTag(c))
	parts = append(parts, "proj="+projectionTag(q, projection))

	if q.Where != nil {
		parts = append(parts, "where="+boolTag(q.Where))
	} else {
		parts = append(parts, "where=none")
	}
	if q.Agg != nil {
		parts = append(parts, "agg="+aggTag(q.Agg))
	}
	if q.Exists != nil {
		neg := ""
		if q.Exists.Negated {
			neg = "not-"
		}
		inner := "bare"
		if q.Exists.Inner != nil {
			inner = "filtered"
		}
		parts = append(parts, fmt.Sprintf("exists=%s%s.%s", neg, opTag(q.Exists.CorrOp), inner))
	}
	if q.ScalarSub != nil {
		parts = append(parts, "scalarsub=1")
	}
	parts = append(parts, "order="+orderTag(q.OrderBy))
	if q.Distinct {
		parts = append(parts, "distinct=1")
	}
	if q.Limit > 0 {
		off := ""
		if q.Offset > 0 {
			off = "+offset"
		}
		parts = append(parts, "limit=1"+off)
	}
	return strings.Join(parts, ";")
}

func shapeTag(q rowdiff.Query) string {
	switch {
	case q.Agg != nil:
		return "agg"
	case q.ThreeWay != nil:
		if q.ThreeWay.Comma {
			return "join3.comma"
		}
		if q.ThreeWay.ROuter {
			return "join3.left-outer"
		}
		return "join3.inner"
	case q.Join != nil:
		switch {
		case q.Join.LeftOuter:
			return "join2.left-outer"
		case q.Join.RightOuter:
			return "join2.right-outer"
		case q.Join.Inner:
			return "join2.inner"
		default:
			return "join2.comma"
		}
	case q.Union != nil:
		if q.Union.All {
			return "union-all"
		}
		return "union"
	case q.Derived != nil:
		if q.Derived.Cte {
			return "cte"
		}
		return "derived"
	default:
		return "single"
	}
}

// indexTag records WHICH columns are indexed, not how many. The distinction
// between an index on the filtered column and an index on some other column is
// the difference between an index-scan test and a full-scan test, and a count
// cannot tell them apart.
func indexTag(c *rowdiff.Case) string {
	if len(c.Table.Indexes) == 0 {
		return "none"
	}
	specs := make([]string, 0, len(c.Table.Indexes))
	for _, idx := range c.Table.Indexes {
		specs = append(specs, strings.Join(idx.Cols, "+"))
	}
	sort.Strings(specs)
	return strings.Join(specs, ",")
}

func projectionTag(q rowdiff.Query, projection []string) string {
	if q.Agg != nil {
		return "agg"
	}
	if projection == nil {
		return "star"
	}
	return "n" + strconv.Itoa(len(projection))
}

// boolTag renders the predicate tree's structure: connectives, nesting and
// per-leaf KIND, with every literal erased.
func boolTag(n *rowdiff.BoolNode) string {
	var b strings.Builder
	writeBoolTag(&b, n)
	return b.String()
}

func writeBoolTag(b *strings.Builder, n *rowdiff.BoolNode) {
	if n == nil {
		return
	}
	if n.Not {
		b.WriteString("!")
	}
	if n.Leaf != nil {
		b.WriteString(leafTag(n.Leaf))
		return
	}
	op := "or"
	if n.And {
		op = "and"
	}
	b.WriteString(op)
	b.WriteString("(")
	for i, k := range n.Kids {
		if i > 0 {
			b.WriteString(",")
		}
		writeBoolTag(b, k)
	}
	b.WriteString(")")
}

// leafTag names a comparison leaf by its KIND and operator — what the leaf
// exercises in the planner — plus whether its column is indexed, which decides
// whether the leaf can be sargable at all.
func leafTag(p *rowdiff.Pred) string {
	var kind string
	switch {
	case p.Case != nil:
		kind = "case"
	case p.StrFn != nil:
		kind = "strfn"
	case p.NumFn != nil:
		kind = "numfn"
	case p.Cast != nil:
		kind = "cast"
	case p.Bitwise:
		kind = "bit"
	case p.HasArith:
		kind = "arith"
	case p.RhsCol != "":
		kind = "colcol"
	case p.IsBetween:
		kind = "between"
	case p.Op == predicates.ComparisonIn:
		kind = "in"
	default:
		kind = "cmp"
	}
	out := kind + "." + opTag(p.Op)
	if p.Negated {
		out = "!" + out
	}
	return out
}

func opTag(op predicates.ComparisonType) string {
	switch op {
	case predicates.ComparisonEquals:
		return "eq"
	case predicates.ComparisonNotEquals:
		return "ne"
	case predicates.ComparisonLessThan:
		return "lt"
	case predicates.ComparisonLessThanOrEq:
		return "le"
	case predicates.ComparisonGreaterThan:
		return "gt"
	case predicates.ComparisonGreaterThanEq:
		return "ge"
	case predicates.ComparisonIsNull:
		return "isnull"
	case predicates.ComparisonIsNotNull:
		return "notnull"
	case predicates.ComparisonIn:
		return "in"
	case predicates.ComparisonLike:
		return "like"
	default:
		return "op" + strconv.Itoa(int(op))
	}
}

func aggTag(a *rowdiff.AggSpec) string {
	var fn string
	switch a.Func {
	case rowdiff.AggCountStar:
		fn = "count-star"
	case rowdiff.AggCountCol:
		fn = "count-col"
	case rowdiff.AggSum:
		fn = "sum"
	case rowdiff.AggMin:
		fn = "min"
	case rowdiff.AggMax:
		fn = "max"
	default:
		fn = "agg" + strconv.Itoa(int(a.Func))
	}
	out := fmt.Sprintf("%s.g%d", fn, len(a.GroupBy))
	if a.HavingOn && a.Having != nil {
		out += ".having"
	}
	return out
}

func orderTag(keys []rowdiff.OrderKey) string {
	if len(keys) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		dir := "asc"
		if k.Desc {
			dir = "desc"
		}
		switch k.Nulls {
		case rowdiff.NullsFirst:
			dir += ".nf"
		case rowdiff.NullsLast:
			dir += ".nl"
		}
		parts = append(parts, dir)
	}
	return strings.Join(parts, "+")
}
