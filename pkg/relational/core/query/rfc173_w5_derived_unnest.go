package query

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// RFC-173 unnest-residual class 3 — a lateral array unnest whose OWNER is a
// CTE / derived-table output (`FROM (SELECT ...) AS d, d.arr AS x`, `WITH c AS
// (...) SELECT ... FROM c, c.arr AS x`). Java resolves the array field against
// the derived quantifier's flowed OUTPUT type, which carries a real
// Type.Array. Go's coexistence model erases array-ness twice (fieldTypeForFD
// collapses repeated fields to UnknownType at the scan leaf; a projection's
// flowed result is a typeless QOV), so the translated leg's flowed type is
// UnknownType and cannot classify the array — the literal design ruling 3(ii)
// is unimplementable today.
//
// The element type is instead recovered from the proto DESCRIPTOR through the
// CTE body's projection: map the derived output column back to a BARE
// passthrough of a base-table column, resolve the body's OWN base scan, and
// classify against that descriptor (the same descriptor path the working
// single-table unnest uses). Routing through the body's actual scan — never
// the outer alias — structurally closes the P2a wrong-same-named-table hazard
// (design ruling 3, condition 1). A POSITIVE WHITELIST: only a bare
// passthrough over a SINGLE unambiguous base scan is recognized; every other
// body shape (computed slot, aggregate, set-op, multi-scan, nested
// derived-over-derived) declines loudly (condition 2) — honest "unsupported"
// over silent-wrong. The S4 type-model rider (flow columnCascadesType's real
// ArrayType through the projection result) retires this mechanism and the
// computed-output declines.

// derivedUnnestDisposition is the classification outcome for a CTE/derived
// unnest owner.
type derivedUnnestDisposition int

const (
	// derivedUnnestUnsupported: the owner is a CTE/derived output whose array
	// column is not a base-table passthrough the descriptor path can classify
	// (computed/aggregate/set-op/multi-scan/nested-derived) — a loud decline.
	derivedUnnestUnsupported derivedUnnestDisposition = iota
	// derivedUnnestArray: a bare passthrough of a base-table ARRAY column —
	// classify OK. elementType + outputName are populated.
	derivedUnnestArray
	// derivedUnnestWrongType: a bare passthrough of a base-table SCALAR column
	// (not repeated) — Java's "repeated type" assert.
	derivedUnnestWrongType
	// derivedUnnestUndefined: the field is absent on the derived output.
	derivedUnnestUndefined
)

// classifyDerivedUnnestArray resolves a lateral unnest's array field through
// its CTE/derived owner's body (class 3). Returns the element type and the
// OUTPUT column name the collection reads (the derived leg flows its row keyed
// by output names, so the collection reads by the output name while the
// element TYPE comes from the base descriptor). See the file header for the
// mechanism and the whitelist discipline.
func (t *cascadesTranslator) classifyDerivedUnnestArray(outerLeft logical.LogicalOperator, u *logical.LogicalUnnest) (elementType values.Type, outputName string, disp derivedUnnestDisposition) {
	if len(u.Segments) != 2 {
		// Multi-segment (`d.rec.arr`) over a derived output: the body would
		// need to type the struct too — out of the base-passthrough whitelist.
		return values.UnknownType, "", derivedUnnestUnsupported
	}
	body, outNames, ok := t.derivedOwnerBody(outerLeft, u.Segments[0])
	if !ok {
		return values.UnknownType, "", derivedUnnestUnsupported
	}
	// The body's OUTPUT projection (through row-shape-preserving wrappers).
	proj := bodyOutputProject(body)
	if proj == nil {
		// Aggregate / set-op / recursive / bare-scan body — not a projection
		// we can map columns through. Decline (whitelist).
		return values.UnknownType, "", derivedUnnestUnsupported
	}
	// Map the referenced OUTPUT name (segment 1) to a projection slot. outNames
	// is the body's output column list in projection order (ColumnAliases /
	// aliases already applied), so its index is the projection slot.
	want := strings.ToUpper(u.Segments[1])
	slot := -1
	for i, n := range outNames {
		if strings.EqualFold(n, want) && i < len(proj.Projections) {
			slot = i
			break
		}
	}
	if slot < 0 {
		return values.UnknownType, "", derivedUnnestUndefined
	}
	// WHITELIST: only a BARE (non-computed) passthrough of a plain column ref
	// is classifiable. A computed slot (expression/aggregate/array literal)
	// has no base-table column to resolve — decline.
	if slot < len(proj.IsComputed) && proj.IsComputed[slot] {
		return values.UnknownType, "", derivedUnnestUnsupported
	}
	source := proj.Projections[slot] // the base column name (possibly qualified)
	// Condition 1: the body's source must resolve to a SINGLE unambiguous BASE
	// scan — never a nested CTE/derived (which would re-open P2a one level
	// down) and never one of several scans (ambiguous). singleBaseScan
	// declines a body containing any join/CTE/aggregate/union.
	scan, ok := singleBaseScan(proj.Input)
	if !ok {
		return values.UnknownType, "", derivedUnnestUnsupported
	}
	// A qualified source (`t.arr`) must name that scan; a bare source binds it.
	baseCol := source
	if dot := strings.LastIndexByte(source, '.'); dot >= 0 {
		qual := source[:dot]
		scanAlias := scan.Alias
		if scanAlias == "" {
			scanAlias = scan.Table
		}
		if !strings.EqualFold(qual, scanAlias) && !strings.EqualFold(qual, scan.Table) {
			return values.UnknownType, "", derivedUnnestUnsupported
		}
		baseCol = source[dot+1:]
	}
	// The base scan must be a REAL table (not itself a CTE/derived — the
	// body-level structural guard, condition 1). A CTE-scoped scan has its
	// table name in the CTE scope maps.
	if t.outerSourceIsCTE(scan.Table) {
		return values.UnknownType, "", derivedUnnestUnsupported
	}
	// Classify the base column against the BODY's base-table descriptor — the
	// correct table, reached through the body's own scan, never the outer
	// alias.
	elemType, _, isArray, fieldPresent := t.unnestArrayElementType(scan.Table, []string{baseCol})
	switch {
	case isArray:
		// The collection reads the DERIVED OUTPUT column by name (the derived
		// row is keyed by output names); the element TYPE is the base
		// column's.
		return elemType, want, derivedUnnestArray
	case fieldPresent:
		return values.UnknownType, "", derivedUnnestWrongType
	default:
		return values.UnknownType, "", derivedUnnestUndefined
	}
}

// derivedOwnerBody returns the CTE/derived owner's BODY subtree and its output
// column names (in projection order, ColumnAliases/aliases applied) for the
// owner named alias. It resolves BOTH representations: a derived table
// `(SELECT...) AS d` lowers to an inline LogicalCTE node in outerLeft; a WITH
// CTE registers its body in cteScope and its output names in cteColumnsScope
// (j.Left only references it by name). ok=false when neither resolves.
func (t *cascadesTranslator) derivedOwnerBody(outerLeft logical.LogicalOperator, alias string) (logical.LogicalOperator, []string, bool) {
	if cte := findDerivedOwnerCTE(outerLeft, alias); cte != nil {
		outNames := deriveOutputNames(cte)
		return cte.Body, outNames, outNames != nil
	}
	key := strings.ToUpper(alias)
	body, ok := t.cteScope[key]
	if !ok {
		return nil, nil, false
	}
	// Output names: the registered cteColumnsScope (which reflects a
	// `WITH c(a,b)` rename) when present; else the body's own projection
	// names. Non-recursive CTEs don't always populate cteColumnsScope, so the
	// body-projection fallback is the common path.
	if cols, ok := t.cteColumnsScope[key]; ok && len(cols) > 0 {
		names := make([]string, len(cols))
		for i, c := range cols {
			names[i] = c.Name
		}
		return body, names, true
	}
	if proj := bodyOutputProject(body); proj != nil {
		names := make([]string, len(proj.Projections))
		for i := range proj.Projections {
			out := proj.Projections[i]
			if i < len(proj.Aliases) && proj.Aliases[i] != "" {
				out = proj.Aliases[i]
			}
			names[i] = out
		}
		return body, names, true
	}
	return body, nil, false
}

// deriveOutputNames returns an inline derived-table CTE's output column names
// in projection order: ColumnAliases when `WITH c(a,b)`-style, else each
// projection's alias (or source name). nil when the body has no mappable
// projection.
func deriveOutputNames(cte *logical.LogicalCTE) []string {
	if len(cte.ColumnAliases) > 0 {
		return cte.ColumnAliases
	}
	proj := bodyOutputProject(cte.Body)
	if proj == nil {
		return nil
	}
	names := make([]string, len(proj.Projections))
	for i := range proj.Projections {
		out := proj.Projections[i]
		if i < len(proj.Aliases) && proj.Aliases[i] != "" {
			out = proj.Aliases[i]
		}
		names[i] = out
	}
	return names
}

// findDerivedOwnerCTE returns the LogicalCTE leg in the outer whose Name
// matches alias (the derived/CTE owner of the unnest). Mirrors
// logical.OuterSourceIsDerivedTable's walk (Main only, never Body) but returns
// the node so the caller can resolve through its Body. nil when no such leg.
func findDerivedOwnerCTE(op logical.LogicalOperator, alias string) *logical.LogicalCTE {
	var walk func(logical.LogicalOperator) *logical.LogicalCTE
	walk = func(o logical.LogicalOperator) *logical.LogicalCTE {
		switch n := o.(type) {
		case *logical.LogicalCTE:
			if strings.EqualFold(n.Name, alias) {
				return n
			}
			return walk(n.Main)
		case *logical.LogicalUnnest:
			return nil
		default:
			for _, c := range o.Children() {
				if r := walk(c); r != nil {
					return r
				}
			}
			return nil
		}
	}
	return walk(op)
}

// bodyOutputProject returns the LogicalProject that defines a CTE body's
// OUTPUT columns, descending through row-shape-preserving wrappers
// (Filter/Sort/Limit/Distinct). Returns nil for a body whose output is NOT a
// plain projection (aggregate, union/set-op, recursive, bare scan) — the
// whitelist declines those.
func bodyOutputProject(op logical.LogicalOperator) *logical.LogicalProject {
	switch o := op.(type) {
	case *logical.LogicalProject:
		return o
	case *logical.LogicalFilter:
		return bodyOutputProject(o.Input)
	case *logical.LogicalSort:
		return bodyOutputProject(o.Input)
	case *logical.LogicalLimit:
		return bodyOutputProject(o.Input)
	case *logical.LogicalDistinct:
		return bodyOutputProject(o.Input)
	}
	return nil
}

// singleBaseScan returns the sole LogicalScan of a projection's input subtree,
// descending only through row-shape-preserving unary wrappers. ok=false when
// the subtree contains a join, CTE/derived source, aggregate, union, unnest,
// or more than one scan — the ambiguity/nesting the whitelist forbids
// (condition 1 + 2). A single scan whose table is itself a CTE is caught by
// the caller's outerSourceIsCTE check.
func singleBaseScan(op logical.LogicalOperator) (*logical.LogicalScan, bool) {
	switch o := op.(type) {
	case *logical.LogicalScan:
		return o, true
	case *logical.LogicalFilter:
		return singleBaseScan(o.Input)
	case *logical.LogicalSort:
		return singleBaseScan(o.Input)
	case *logical.LogicalLimit:
		return singleBaseScan(o.Input)
	case *logical.LogicalDistinct:
		return singleBaseScan(o.Input)
	case *logical.LogicalProject:
		return singleBaseScan(o.Input)
	}
	return nil, false
}
