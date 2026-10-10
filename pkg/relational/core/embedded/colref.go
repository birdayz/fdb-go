// Portions derived from FoundationDB Record Layer (Identifier.java,
// FieldValue.java),
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package embedded

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// colRef carries structured column identity: a (table, column) pair.
// Mirrors Java's Identifier{name, qualifier} / FieldValue(QOV(correlation), field).
// The table part is empty for unqualified references.
type colRef struct {
	table string // table alias or "" for unqualified
	col   string // bare column name
}

// parseColRef splits a flat "TABLE.COL" string into a structured colRef.
// Unqualified names produce colRef{"", "COL"}.
//
// The split ignores dots inside parentheses: a derived aggregate name carries
// its own dots, and `I.SUM(I.AMOUNT)` is one qualifier and one derived column,
// not table `I.SUM(I` and column `AMOUNT)`. A wrong split surfaces as the
// result-set label of
//
//	SELECT s.id, (SELECT SUM(x."Amount") FROM sales x WHERE …) FROM sales s
//
// A qualifier is never inside parentheses, so depth 0 is the only place a
// split can be meant. This does not make the flat string a safe channel — a
// delimited identifier may still contain a literal dot, which is why the
// parse-tree triple (ColumnRef) exists and why callers that have it use it.
//
// Only a matched pair nests. A single-pass depth counter treats an unmatched
// paren as structure in whichever direction it scans: right-to-left, `D.A)B`
// (`"a)b"` is a legal column name) loses its qualifier; left-to-right, `A(B.C`
// does. Matching the pairs first makes both strays inert.
//
// A quote is not structure at all; see the body.
//
// `A(B.C` splitting to `A(B` / `C` is a choice, not a correctness claim:
// quotes are stripped by the time a name arrives here, so a delimited
// `"f(a.b"` is indistinguishable from a qualified reference. The same limit
// applies to a dot inside a delimited identifier — `"a.b"` reads as `a.b`.
func parseColRef(s string) colRef {
	// A quote is not a delimiter here. An apostrophe in one of these flat names
	// is far more often identifier content than a string-literal boundary, and
	// nothing local tells the two apart:
	//
	//	Q'.Z'   -- derived alias `Q'`, column alias `Z'`
	//
	// Pairing those apostrophes would make `'.Z'` a literal span and hide the
	// qualifier dot, so `Rows.Columns()` over joined derived tables would report
	// `["Q'.Z'", "R'.Z'"]` where `["Z'", "Z'"]` is right.
	//
	// Literal-bearing names do occur in production (`CAST('0.0' AS BIGINT)`,
	// `COALESCE(NAME,'unknown')`), but an instrumented run over the planning
	// corpus shows no production name whose split depends on a literal span.
	// The cost is two shapes, both pinned as stated limits in
	// colref_split_test.go and the yamsql corpus:
	//
	//	I.COUNT(CASE WHEN S=')' THEN X.Y END) -- a literal with a MATCHED paren
	//	X.Y || '.'                            -- a literal with a dot at depth 0
	//
	// A literal dot enclosed by parens (`I.COUNT(CASE WHEN S='.' THEN 1 END)`,
	// `CAST('0.0' AS BIGINT)`) is fine, and so is an unmatched paren inside a
	// literal (`X.Y || ')'`), which is inert like any other stray.
	//
	// Pass 1: MATCHED paren pairs. nest[i] is true for a paren that has a
	// partner; a stray one stays false and is inert below.
	nest := make([]bool, len(s))
	var open []int
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			open = append(open, i)
		case ')':
			if n := len(open); n > 0 {
				nest[open[n-1]], nest[i] = true, true
				open = open[:n-1]
			}
		}
	}
	// Pass 2: the last dot at depth 0 is the split.
	depth, split := 0, -1
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '(' && nest[i]:
			depth++
		case s[i] == ')' && nest[i]:
			depth--
		case s[i] == '.' && depth == 0:
			split = i
		}
	}
	if split < 0 {
		return colRef{col: s}
	}
	return colRef{table: s[:split], col: s[split+1:]}
}

// recordProjQualVsScan files one projected-column qualifier decision into the
// qualifier recovery census, cut by whether the projection's parse-tree triple
// for the SAME SLOT agrees with what the split manufactured.
//
// It records the split's OWN verdict (parseColRef's), never the shared
// classifier's, because the two disagree on a trailing dot and an instrument
// must report the decision the site actually made.
//
// The gate is read FIRST, before any classification. RecordQualifierRecovery
// re-checks it, so a gate here is not needed for CORRECTNESS — it is needed so
// the census-off cost is the atomic load and nothing else. Classifying first
// and filing second would make production pay the ToUpper on every projected
// column to build an argument that is then discarded.
func recordProjQualVsScan(proj *logical.LogicalProject, slot int, upper string, ref colRef) {
	if !values.LegIdentityCensusEnabled() {
		return
	}
	ident, present := "", false
	// A ColumnRef that is not Present means "unknown", never "unqualified" —
	// the triple's own contract — so an absent one is NO counterparty rather
	// than an empty one that would read as a disagreement.
	if slot < len(proj.ProjectionRefs) {
		if r := proj.ProjectionRefs[slot]; r.Present {
			ident, present = strings.ToUpper(r.Qualifier), true
		}
	}
	class := values.QualRecBare
	witness := ""
	switch {
	case !ref.isQualified():
	case !present:
		class = values.QualRecManufactured
	case strings.EqualFold(ref.table, ident):
		class, witness = values.QualRecAgreed, ident
	default:
		class = values.QualRecDiverged
		witness = ident
		if ident == "" {
			witness = "<unqualified>"
		}
	}
	values.RecordQualifierRecovery(values.QualRecSiteProjQualVsScan, class, upper, witness)
}

// bare returns the unqualified column name.
func (r colRef) bare() string {
	return r.col
}

// isQualified returns true when the reference has a table qualifier.
func (r colRef) isQualified() bool {
	return r.table != ""
}
