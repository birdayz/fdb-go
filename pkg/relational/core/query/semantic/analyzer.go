package semantic

import (
	"fmt"

	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
)

// Analyzer ties Catalog lookups + identifier normalization into
// the resolution helpers that rule authors / logical-plan builders
// invoke. Mirrors the instance surface of Java's
// `SemanticAnalyzer` (the static methods on that class — case
// folding etc. — live as free functions in this package).
//
// Seed scope: resolve table references, resolve bare/qualified
// column references. Star expansion, nested-field lookup,
// correlated-identifier resolution all land in follow-up shifts.
//
// Not safe for concurrent mutation of the underlying Catalog; the
// Analyzer itself is stateless once constructed.
type Analyzer struct {
	catalog Catalog
	// caseSensitive governs how raw ANTLR text is normalized when
	// the analyzer sees it. SQL-standard default is false
	// (identifiers are case-insensitive); test fixtures and
	// case-sensitive modes can flip it.
	caseSensitive bool
}

// NewAnalyzer wires up an Analyzer against the given catalog. A nil
// catalog is rejected — callers who don't have a real schema yet
// should use NewInMemoryCatalog() with no tables.
func NewAnalyzer(catalog Catalog, caseSensitive bool) *Analyzer {
	if catalog == nil {
		panic("NewAnalyzer: catalog is nil; pass NewInMemoryCatalog() for stub use")
	}
	return &Analyzer{catalog: catalog, caseSensitive: caseSensitive}
}

// Catalog returns the underlying Catalog. Exposed so higher-level
// passes (e.g. LogicalPlan builder) can thread the same catalog
// through without re-wiring.
func (a *Analyzer) Catalog() Catalog { return a.catalog }

// CaseSensitive reports the analyzer's case-sensitivity setting.
func (a *Analyzer) CaseSensitive() bool { return a.caseSensitive }

// ResolveTable looks up a table by qualified name. Returns a
// typed error when the name is missing so callers can wrap it
// into the API-level error shape without string-matching.
func (a *Analyzer) ResolveTable(name QualifiedName) (Table, error) {
	if name.IsZero() {
		return nil, &TableNotFoundError{Name: name}
	}
	t, ok := a.catalog.LookupTable(name)
	if !ok {
		return nil, &TableNotFoundError{Name: name}
	}
	return t, nil
}

// ResolveColumn looks up a column by identifier against a resolved
// table. Mirrors the simple case of Java's resolveIdentifier —
// qualifier resolution (`t.col` → column on aliased table) comes
// later with the FROM-clause scope machinery.
func (a *Analyzer) ResolveColumn(table Table, id Identifier) (Column, error) {
	if table == nil {
		return Column{}, &ColumnNotFoundError{Id: id}
	}
	c, ok := table.LookupColumn(id)
	if !ok {
		return Column{}, &ColumnNotFoundError{TableName: table.Name(), Id: id}
	}
	return c, nil
}

// ResolveColumnRefNested is the one-shot column-reference resolver: given a
// qualifier (may be zero) and a column identifier, dispatch to bare or
// qualified lookup against the provided scope. This is the analyzer's top-level
// hook for every identifier reference the expression resolver sees.
//
// - qualifier.IsZero() → Scope.ResolveColumn (bare).
// - qualifier non-zero → Scope.ResolveQualifiedColumnNested.
//
// Returns the same typed errors as the underlying scope methods, plus the
// accessor chain a reference that descends INTO a struct column resolves to —
// Java's lookupNestedField result (SemanticAnalyzer.java:578-601). The chain is
// empty for every reference that addresses a source column directly.
//
// The chain is part of the RESULT, not an optional extra: a caller that mints a
// value from (Column, ScopeSource) alone and ignores the chain has resolved
// `struct.member` to the whole struct — a wrong-column read that raises no
// error. There is deliberately no chain-discarding sibling of this method for a
// caller to reach for; Java has no such split either, because lookupNestedField
// fuses the descent onto the value before any caller sees it
// (SemanticAnalyzer.java:599-600). expr.fuseNestedAccessorsIfAny is the Go side
// of that fuse.
//
// BE PRECISE ABOUT WHAT THAT BUYS. Removing the discarding sibling RAISED THE
// COST of the mistake; it did not make it unrepresentable. Each mint still calls
// the fuse itself, so a NEW mint that resolves through this method and forgets
// the call is the same silent wrong-column read as before — it just has to be
// written deliberately rather than by picking the shorter-named function. The
// change that would remove the shape is a single mint, or the fuse moved inside
// whatever the mints share; neither exists yet.
//
// The BARE arm never descends, and that is Java's rule, not an omission:
// lookupNestedField returns empty immediately when the requested identifier
// has one segment (SemanticAnalyzer.java:557-559), because a descent needs a
// prefix to consume before there is anything left to walk into.
func (a *Analyzer) ResolveColumnRefNested(scope *Scope, qualifier, id Identifier) (Column, ScopeSource, []NestedAccessor, error) {
	if scope == nil {
		return Column{}, ScopeSource{}, nil, &ColumnNotFoundError{Id: id}
	}
	if qualifier.IsZero() {
		c, src, err := scope.ResolveColumn(id)
		return c, src, nil, err
	}
	return scope.ResolveQualifiedColumnNested(qualifier, id)
}

// ResolveColumnRefPath is ResolveColumnRefNested for a reference of ARBITRARY
// segment depth — `a.n.sk` and deeper. Java has no arity cap on this path
// (`fullId : uid (DOT uid)*` in the grammar, an unbounded remainingPath loop in
// SemanticAnalyzer.lookupNestedField), so neither does this.
//
// A single segment takes the BARE arm, which never descends: Java's
// lookupNestedField returns empty immediately for a one-segment identifier
// (SemanticAnalyzer.java:557-559), because a descent needs a prefix to consume
// before there is anything left to walk into.
func (a *Analyzer) ResolveColumnRefPath(scope *Scope, segs []Identifier) (Column, ScopeSource, []NestedAccessor, error) {
	if scope == nil {
		var leaf Identifier
		if len(segs) > 0 {
			leaf = segs[len(segs)-1]
		}
		return Column{}, ScopeSource{}, nil, &ColumnNotFoundError{Id: leaf}
	}
	return scope.ResolvePathNested(segs)
}

// ResolveTableRef is the parse-tree convenience wrapper over
// ResolveTable. Reads the IFullIdContext (ANTLR's table reference
// node), builds a QualifiedName with the analyzer's case-sensitivity,
// then looks it up in the catalog.
//
// Returns TableNotFoundError with the QualifiedName the caller
// requested; callers preserve user-facing names through
// `err.Name.String()`.
func (a *Analyzer) ResolveTableRef(ctx antlrgen.IFullIdContext) (Table, error) {
	name := FromFullIdContext(ctx, a.caseSensitive)
	return a.ResolveTable(name)
}

// ExpandStar implements the `SELECT *` rewrite — returns the full
// column list of the given table in declared order. Each Column is
// returned unchanged (same Id, Type, Nullable) so downstream plan
// builders can wrap each into a ColumnReference / ProjectionItem.
//
// Mirrors the single-qualifier case of Java's
// `SemanticAnalyzer.expandStar`. The multi-table / alias-qualified
// cases (`SELECT t.* FROM t JOIN u`) come with the FROM-scope port.
func (a *Analyzer) ExpandStar(table Table) []Column {
	if table == nil {
		return nil
	}
	return NonEphemeral(table.Columns())
}

// NonEphemeral filters ephemeral columns out of a column list — the star
// visibility rule (Java's Expressions.nonEphemeralVisible, consumed by
// SemanticAnalyzer.expandStar at SemanticAnalyzer.java:346-348). Resolution
// by NAME keeps seeing ephemeral columns (LookupColumn is unfiltered).
func NonEphemeral(cols []Column) []Column {
	out := cols[:0:0]
	for _, c := range cols {
		if !c.Ephemeral {
			out = append(out, c)
		}
	}
	return out
}

// ExpandedColumn pairs a Column with the ScopeSource it came from.
// The scope-aware star expander / qualified-star expander returns
// these so downstream plan builders know which FROM source to
// attribute each projected column to.
type ExpandedColumn struct {
	Column Column
	Source ScopeSource
}

// ExpandScopeStar implements unqualified `SELECT *` against a Scope:
// concatenates each source's columns in FROM-order. Ambiguity is
// NOT flagged here — Java's SQL lets two sources expose same-named
// columns through `SELECT *` (the output just gets two columns);
// only bare *references* error. Downstream callers tag each
// ExpandedColumn with its source so later projection rewrites can
// qualify.
func (a *Analyzer) ExpandScopeStar(scope *Scope) []ExpandedColumn {
	if scope == nil {
		return nil
	}
	var out []ExpandedColumn
	for _, src := range scope.Sources() {
		for _, c := range NonEphemeral(src.Table.Columns()) {
			out = append(out, ExpandedColumn{Column: c, Source: src})
		}
	}
	return out
}

// ExpandQualifiedStar implements `SELECT alias.*` against a Scope:
// looks up the named source, then its columns. Walks the parent
// chain for correlated-star references. Returns SourceNotFoundError
// (with the Available alias list populated from every visible
// scope for "did you mean?" rendering) when no source matches.
func (a *Analyzer) ExpandQualifiedStar(scope *Scope, qualifier Identifier) ([]ExpandedColumn, error) {
	if scope == nil {
		return nil, &SourceNotFoundError{Alias: qualifier}
	}
	// Iterate the scope chain non-recursively so we can collect all
	// visible aliases if the qualifier misses entirely.
	for cur := scope; cur != nil; cur = cur.Parent() {
		for _, src := range cur.sources {
			if src.matchesQualifier(qualifier) {
				cols := NonEphemeral(src.Table.Columns())
				out := make([]ExpandedColumn, len(cols))
				for i, c := range cols {
					out[i] = ExpandedColumn{Column: c, Source: src}
				}
				return out, nil
			}
		}
	}
	// Chain exhausted — collect every visible alias for the error.
	all := scope.AllSourcesRecursive()
	avail := make([]Identifier, 0, len(all))
	for _, src := range all {
		avail = append(avail, src.Alias)
	}
	return nil, &SourceNotFoundError{Alias: qualifier, Available: avail}
}

// --- Errors ---------------------------------------------------------

// TableNotFoundError is returned when ResolveTable can't find a
// table. Carries the qualified name the caller requested; follows
// the error-type pattern from CLAUDE.md (Java exception = Go
// error struct).
type TableNotFoundError struct {
	Name QualifiedName
}

func (e *TableNotFoundError) Error() string {
	if e.Name.IsZero() {
		return "table not found: <empty name>"
	}
	return fmt.Sprintf("table not found: %s", e.Name)
}

// ColumnNotFoundError is returned when ResolveColumn can't find a
// column on the given table.
type ColumnNotFoundError struct {
	TableName QualifiedName
	Id        Identifier
}

func (e *ColumnNotFoundError) Error() string {
	if e.TableName.IsZero() {
		return fmt.Sprintf("column not found: %s", e.Id)
	}
	return fmt.Sprintf("column %s not found on table %s", e.Id, e.TableName)
}
