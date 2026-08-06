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
func recordProjQualVsScan(proj *logical.LogicalProject, slot int, upper string, ref colRef) {
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
func projScopeAlias(fv *values.FieldValue) string {
	if qov, isQOV := fv.Child.(*values.QuantifiedObjectValue); isQOV {
		alias := strings.ToUpper(qov.Correlation.Name())
		values.RecordQualifierRecovery(values.QualRecSiteProjScopeClassify,
			values.QualRecCarried, fv.Field, alias)
		return alias
	}
	if ref := parseColRef(fv.Field); ref.isQualified() {
		values.RecordQualifierRecovery(values.QualRecSiteProjScopeClassify,
			values.QualRecManufactured, fv.Field, "")
		return strings.ToUpper(ref.table)
	}
	values.RecordQualifierRecovery(values.QualRecSiteProjScopeClassify,
		values.QualRecBare, fv.Field, "")
	return ""
}

// recordDisplayLabelStrip files one machinery-alias display-label decision into
// the qualifier recovery census.
//
// The counterparty is the projected VALUE. A machinery-minted alias exists
// because the dedup pinned a projected reference's QUALIFIED spelling, and that
// reference is a FieldValue over a QuantifiedObjectValue — so the correlation
// the alias was minted FROM is right there, and whether the qualifier sliced out
// of the label equals it is this site's whole conversion question.
//
// The parenthesis rejection gets its own class rather than being folded into
// bare: the site WAS handed a dotted name and declined by inspecting
// punctuation in a rendering, which is the opposite finding from never having
// seen a dot.
func recordDisplayLabelStrip(label string, v values.Value) {
	ref := parseColRef(label)
	if !ref.isQualified() {
		values.RecordQualifierRecovery(values.QualRecSiteDisplayLabelStrip,
			values.QualRecBare, label, "")
		return
	}
	if !isPlainQualifiedColumnReference(label) {
		values.RecordQualifierRecovery(values.QualRecSiteDisplayLabelStrip,
			values.QualRecHeuristicDecline, label, "")
		return
	}
	ident, present := "", false
	if fv, isField := v.(*values.FieldValue); isField && fv.Child != nil {
		if qov, isQOV := fv.Child.(*values.QuantifiedObjectValue); isQOV {
			ident, present = strings.ToUpper(qov.Correlation.Name()), true
		}
	}
	switch {
	case !present:
		values.RecordQualifierRecovery(values.QualRecSiteDisplayLabelStrip,
			values.QualRecManufactured, label, "")
	case strings.EqualFold(ref.table, ident):
		values.RecordQualifierRecovery(values.QualRecSiteDisplayLabelStrip,
			values.QualRecAgreed, label, ident)
	default:
		values.RecordQualifierRecovery(values.QualRecSiteDisplayLabelStrip,
			values.QualRecDiverged, label, ident)
	}
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
