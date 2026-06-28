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

// ResolveColumnRef is the one-shot column-reference resolver: given
// a qualifier (may be zero) and a column identifier, dispatch to
// bare or qualified lookup against the provided scope. This is the
// analyzer's top-level hook for every identifier reference the
// expression resolver sees.
//
// - qualifier.IsZero() → Scope.ResolveColumn (bare).
// - qualifier non-zero → Scope.ResolveQualifiedColumn.
//
// Returns the same typed errors as the underlying scope methods.
func (a *Analyzer) ResolveColumnRef(scope *Scope, qualifier, id Identifier) (Column, ScopeSource, error) {
	if scope == nil {
		return Column{}, ScopeSource{}, &ColumnNotFoundError{Id: id}
	}
	if qualifier.IsZero() {
		return scope.ResolveColumn(id)
	}
	return scope.ResolveQualifiedColumn(qualifier, id)
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
	return table.Columns()
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
		for _, c := range src.Table.Columns() {
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
			if src.Alias.EqualsIgnoreQuoting(qualifier) {
				cols := src.Table.Columns()
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
