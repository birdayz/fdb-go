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
func parseColRef(s string) colRef {
	if dot := strings.LastIndex(s, "."); dot >= 0 {
		return colRef{table: s[:dot], col: s[dot+1:]}
	}
	return colRef{col: s}
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
