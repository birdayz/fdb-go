package semantic

import antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"

// FromFullIdContext converts an ANTLR IFullIdContext parse-tree node
// to a QualifiedName. Each Uid segment is read via GetText() (which
// preserves source casing), then normalized per caseSensitive.
//
// The typical call site is table-name resolution:
//
//	tbl := semantic.FromFullIdContext(tblCtx.FullId(), false)
//	if resolved, err := catalog.LookupTable(tbl); err != nil { ... }
//
// Returns the zero QualifiedName when ctx is nil or has no Uid
// children — callers should test IsZero before trusting the result.
func FromFullIdContext(ctx antlrgen.IFullIdContext, caseSensitive bool) QualifiedName {
	if ctx == nil {
		return QualifiedName{}
	}
	uids := ctx.AllUid()
	if len(uids) == 0 {
		return QualifiedName{}
	}
	raws := make([]string, len(uids))
	for i, u := range uids {
		raws[i] = u.GetText()
	}
	return FromSegments(raws, caseSensitive)
}

// FromUidContext converts a single IUidContext to an Identifier.
// Used for unqualified references (column aliases, CTE names, etc.)
// where the full-id shape is overkill.
func FromUidContext(ctx antlrgen.IUidContext, caseSensitive bool) Identifier {
	if ctx == nil {
		return Identifier{}
	}
	return New(ctx.GetText(), caseSensitive)
}
