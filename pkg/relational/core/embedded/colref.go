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
// The split ignores dots inside PARENTHESES, and that is a correctness fix
// rather than a nicety. A derived aggregate name carries its own dots:
// `I.SUM(I.AMOUNT)` is one qualifier and one derived column, but a plain
// last-dot split cut it into table `I.SUM(I` and column `AMOUNT)` — and that
// fragment was the RESULT-SET LABEL a user saw for
//
//	SELECT s.id, (SELECT SUM(x."Amount") FROM sales x WHERE …) FROM sales s
//
// A qualifier is never inside parentheses, so depth-0 is the only place a
// split can be meant. This does not make the flat string a safe channel — a
// delimited identifier may still contain a literal dot, which is why the
// parse-tree triple (ColumnRef) exists and why callers that have it use it.
// It stops this one from mangling names it has no business splitting.
//
// ONLY A MATCHED PAIR NESTS, and that qualification was learned by getting it
// wrong in both directions. A single-pass depth counter treats an UNMATCHED
// paren as structure, and which way it is wrong depends on which way it scans:
// right-to-left, `D.A)B` — `"a)b"` is a legal column name — reads the `)` as an
// opening nest and returns one unqualified name; left-to-right, `A(B.C` reads
// the `(` the same way. Neither direction is sound. Matching the pairs FIRST
// makes both strays inert and removes the choice.
//
// A QUOTE IS NOT STRUCTURE AT ALL — see the body for the measurement that
// settled it. An intermediate version treated apostrophes as string-literal
// delimiters, which fixed a literal containing a `)` and broke `Q'.Z'`, two
// apostrophe-bearing identifiers either side of a real qualifier dot.
//
// `A(B.C` splitting to `A(B` / `C` is a CHOICE and not a correctness claim:
// quotes are stripped by the time a name arrives here, so a delimited
// `"f(a.b"` is indistinguishable from a qualified reference and this now splits
// one the old code kept whole. The same limit applies to a dot inside a
// delimited identifier — `"a.b"` reads as `a.b`. That is why the parse-tree
// triple (ColumnRef) exists and why callers holding one use it instead.
func parseColRef(s string) colRef {
	// A QUOTE IS NOT A DELIMITER HERE. An apostrophe in one of these flat names
	// is far more often IDENTIFIER CONTENT than a string-literal boundary, and
	// nothing local tells the two apart — so treating it as a boundary is a
	// guess that costs more than it buys:
	//
	//	Q'.Z'   -- derived alias `Q'`, column alias `Z'`
	//
	// Pairing those two apostrophes makes `'.Z'` a literal span, hides the
	// qualifier dot inside it, and `Rows.Columns()` over joined derived tables
	// reports `["Q'.Z'", "R'.Z'"]` where `["Z'", "Z'"]` is right. That is a
	// REACHABLE regression through ordinary delimited identifiers.
	//
	// The literal handling existed for the opposite shape — a `)` inside a
	// string literal, `I.COUNT(CASE WHEN S=')' THEN X.Y END)`, where the
	// literal's paren closes the real one early and the inner dot lands at
	// depth 0. That shape needs a string literal to appear in a derived NAME,
	// and a live differential over 2,682,910 production calls found no
	// literal-bearing name at all. So the trade is a measured regression
	// against an unmeasured one, and it goes the only way it can. The `)`-in-
	// literal case is pinned as a stated limit instead.
	//
	// Note what this does NOT cost: a literal containing a DOT (`S='.'`) still
	// resolves correctly, because the surrounding parens put that dot at depth
	// 1 whether or not the quotes mean anything. Only a literal containing a
	// PAREN is affected.
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

// projScopeAlias returns the source alias a projection field reference binds,
// for the inner- vs outer-scoping decision, and files the decision into the
// qualifier recovery census.
//
// It is a named function rather than the inline branch it replaces so the
// recorder can be pinned PER CLASS by unit test. That is not cosmetic: this
// site's MANUFACTURED bucket is 0 over every corpus that runs, so nothing else
// can tell a debt population that is genuinely empty from a recorder that never
// reaches the branch.
//
// TWO CHANNELS, and which one answers is the whole measurement:
//
//   - a QuantifiedObjectValue child — the correlation IS the alias, carried, no
//     string sliced. CARRIED.
//   - otherwise a LAST-dot split of the rendered field name, with NO
//     counterparty: the split arm runs precisely where no correlation was
//     carried, so this site can report carried or manufactured and never AGREED.
//     That structural fact is its conversion answer — there is nothing local to
//     convert to.
//
// The DECISION is parseColRef's, unchanged. The census is recorded BESIDE it
// rather than derived from the shared classifier, because the two disagree on
// the degenerate `T.` (a trailing dot is a qualifier to parseColRef and a bare
// name to the classifier) and an instrument may not move the behaviour it
// measures.
func projScopeAlias(fv values.FieldValue) string {
	if qov, isQOV := values.AsQuantifiedObjectValue(fv.ChildValue()); isQOV {
		alias := strings.ToUpper(qov.Correlation().Name())
		values.RecordQualifierRecovery(values.QualRecSiteProjScopeClassify,
			values.QualRecCarried, fv.DisplayName(), alias)
		return alias
	}
	values.RecordQualifierRecovery(values.QualRecSiteProjScopeClassify,
		values.QualRecBare, fv.DisplayName(), "")
	return ""
}

// stripDisplayLabelQualifier removes the SOURCE qualifier from a
// machinery-pinned alias and reports whether it removed one. It classifies the
// same decision it makes, which is the point: a recorder that classified a
// split the production line then performed differently would measure the wrong
// predicate.
//
// The counterparty is the slot's frozen ProjectionAliasSource, captured from
// structured parse/resolver identity when the machinery minted the alias. It is
// deliberately not the projected Value: physical planning can correctly move
// that program onto `_current`, which says where to evaluate it now rather than
// which authored source supplied the display key.
//
// WHAT IS REMOVED is the leaf-preserving strip Java performs: its clearQualifier
// drops the whole qualifier LIST (LogicalOperator.java:484-487), and a
// three-segment reference puts BOTH leading segments in that list, so the label
// is the last segment either way.
//
// WHAT IS CLASSIFIED is whether the qualifier agrees with that frozen source, and
// the qualifier is the LEADING segment. The comparison used to be against
// everything before the LAST dot — the same string at two segments and a
// different one at three. For `a.n.sk` that read a source "A.N" which exists
// nowhere and recorded DIVERGED against an identity plainly saying "A", firing
// this census's one asserted zero on a correct read. The second dot is inside
// the struct path and is not a qualifier boundary at all.
//
// The classification lives in recordDisplayLabelStrip below, which reads its
// gate FIRST and re-derives nothing: this function's parse is passed to it.
func stripDisplayLabelQualifier(label string, source values.ProjectionAliasSource) (string, bool) {
	ref := parseColRef(label)
	strips := ref.isQualified() && isPlainQualifiedColumnReference(label)
	recordDisplayLabelStrip(label, ref.isQualified(), strips, source)
	if !strips {
		return label, false
	}
	return strings.ToUpper(ref.bare()), true
}

// recordDisplayLabelStrip files the decision stripDisplayLabelQualifier just
// made. The GATE IS ITS FIRST STATEMENT and it classifies nothing of its own:
// the caller has already parsed the label for its own purposes and passes the
// two bits of that parse this recorder needs, so with the census off the cost
// here is one comparison. It used to re-derive them — a parseColRef plus an
// isPlainQualifiedColumnReference that parses AGAIN — doubling the caller's
// work on every projected column of every query to build an argument the
// disabled sink drops.
//
// The parenthesis rejection gets its own class rather than being folded into
// bare: the site WAS handed a dotted name and declined by inspecting
// punctuation in a rendering, which is the opposite finding from never having
// seen a dot.
func recordDisplayLabelStrip(
	label string,
	qualified, strips bool,
	source values.ProjectionAliasSource,
) {
	if !values.LegIdentityCensusEnabled() {
		return
	}
	if !strips {
		class := values.QualRecBare
		if qualified {
			class = values.QualRecHeuristicDecline
		}
		values.RecordQualifierRecovery(values.QualRecSiteDisplayLabelStrip, class, label, "")
		return
	}
	// The counterparty was frozen when the machinery minted the alias. The
	// projected Value is intentionally NOT consulted: physical planning may
	// correctly reanchor it onto `_current`, which is an execution carrier and
	// not the authored source that supplied `A.NAME`.
	ident, present := "", source.Present
	if present {
		ident = strings.ToUpper(source.Source.Name())
	}
	switch {
	case !present:
		// A dotted label over a value carrying no correlation at all: the
		// qualifier comes off with nothing to check it against.
		values.RecordQualifierRecovery(values.QualRecSiteDisplayLabelStrip,
			values.QualRecManufactured, label, "")
	case strings.EqualFold(leadingSegment(label), ident):
		values.RecordQualifierRecovery(values.QualRecSiteDisplayLabelStrip,
			values.QualRecAgreed, label, ident)
	default:
		values.RecordQualifierRecovery(values.QualRecSiteDisplayLabelStrip,
			values.QualRecDiverged, label, ident)
	}
}

// leadingSegment is a reference's FIRST segment — the only one that can name a
// SOURCE. parseColRef answers the LAST dot, which is the other end of the same
// string and coincides with this only at two segments.
func leadingSegment(label string) string {
	if dot := strings.IndexByte(label, '.'); dot > 0 {
		return label[:dot]
	}
	return label
}

// bare returns the unqualified column name.
func (r colRef) bare() string {
	return r.col
}

// isQualified returns true when the reference has a table qualifier.
func (r colRef) isQualified() bool {
	return r.table != ""
}

// isPlainQualifiedColumnReference distinguishes the qualifier dot in a column
// label from a dot rendered inside a function/expression label. Identifier
// quotes have already been stripped at this layer, so punctuation cannot be
// used as a general rejection criterion: a delimited machinery identifier may
// legitimately contain spaces, dashes, or operators. Parentheses, however,
// identify the rendered aggregate/function label at issue and are not part of
// a plain qualified column reference after parsing.
func isPlainQualifiedColumnReference(s string) bool {
	ref := parseColRef(s)
	if !ref.isQualified() || ref.table == "" || ref.col == "" {
		return false
	}
	return !strings.ContainsAny(s, "()")
}
