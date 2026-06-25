package semantic

import "fmt"

// Scope is the set of named resolutions visible at a point during
// query analysis. A scope knows about the FROM-clause sources
// (tables + their aliases) at that level and, via parent-chain,
// inherits correlated sources from enclosing scopes (for nested
// subqueries).
//
// Mirrors the subset of Java's `LogicalPlanFragment` + scope chain
// the analyzer uses for identifier resolution.
//
// Construction: start with NewScope(parent) — parent nil means the
// outermost query. Call AddSource to push each FROM source as the
// analyzer walks FROM clauses left-to-right.
//
// Not concurrency-safe; the analyzer is single-threaded per query.
type Scope struct {
	parent  *Scope
	sources []ScopeSource
}

// ScopeSource is one FROM-clause entry: a resolved Table plus the
// alias it's visible under. Alias is always non-zero — when the
// user doesn't write AS, the Table's own name fills in.
type ScopeSource struct {
	// Table is the resolved schema-level table.
	Table Table
	// Alias is the name used to reference this source in the
	// enclosing query (column qualifier). For `FROM t AS x` → Alias
	// is `x`; for `FROM t` with no alias → Alias is `t`.
	Alias Identifier
	// CorrelationName is the identifier the analyzer uses to tie
	// this source back to a Quantifier when building
	// QuantifiedObjectValue / FieldValue trees that reference it.
	// Stored as a string so the semantic package doesn't take a
	// dependency on cascades/values/CorrelationIdentifier — callers wrap
	// this into a cascades.values.CorrelationIdentifier themselves.
	CorrelationName string
	// ColumnAliasMap maps upper-cased aliased column names to the
	// underlying real column names. Used by CTE-derived scopes where
	// the CTE body uses `SELECT col AS alias` — the resolver sees
	// `alias` but the scan produces `col`. nil when no aliasing.
	ColumnAliasMap map[string]string
	// Shadowing marks a source whose columns SHADOW same-named columns of
	// non-shadowing sources at this scope level (instead of colliding into
	// an ambiguity error). A lateral array unnest (`FROM t, t.arr AS x`)
	// uses this: its AS/AT binding shadows a same-named real column of `t`
	// — Java's generateCorrelatedFieldAccess binding wins over the outer
	// (RFC-142). When ≥1 shadowing source matches a bare column, the
	// shadowing match is taken and the non-shadowing matches are ignored;
	// two shadowing matches are still ambiguous.
	Shadowing bool
}

// NewScope constructs a Scope inheriting from parent. parent may be
// nil for the outermost query.
func NewScope(parent *Scope) *Scope {
	return &Scope{parent: parent}
}

// Parent returns the enclosing scope, or nil if this is the
// outermost.
func (s *Scope) Parent() *Scope { return s.parent }

// Sources returns the FROM-clause sources at this scope level
// (defensive copy, does NOT include parent sources).
func (s *Scope) Sources() []ScopeSource {
	if len(s.sources) == 0 {
		return nil
	}
	out := make([]ScopeSource, len(s.sources))
	copy(out, s.sources)
	return out
}

// AllSourcesRecursive returns sources from this scope and every
// ancestor, inner-first. Useful for "did you mean?" error
// suggestions when a qualifier misses — callers can enumerate all
// visible aliases and suggest the closest.
func (s *Scope) AllSourcesRecursive() []ScopeSource {
	var out []ScopeSource
	for cur := s; cur != nil; cur = cur.parent {
		out = append(out, cur.sources...)
	}
	return out
}

// AddSource appends a FROM-clause source. Returns an error on
// duplicate alias within the same scope (SQL forbids two sources
// sharing an alias at the same level).
func (s *Scope) AddSource(src ScopeSource) error {
	for _, existing := range s.sources {
		if existing.Alias.EqualsIgnoreQuoting(src.Alias) {
			return &DuplicateAliasError{Alias: src.Alias}
		}
	}
	s.sources = append(s.sources, src)
	return nil
}

// ResolveColumn looks up a bare column reference (no qualifier)
// against the scope's sources, following the parent chain if no
// local match. Ambiguous matches within a single scope level
// (multiple tables with a column of this name) return an error —
// the caller should instruct the user to qualify.
//
// Mirrors Java's resolution: inner scopes shadow outer; within a
// scope, ambiguity is a hard error.
func (s *Scope) ResolveColumn(id Identifier) (Column, ScopeSource, error) {
	// First pass at this level: collect matches. Ambiguity within
	// one level is an error; we check before descending the parent
	// chain.
	var matches []struct {
		col Column
		src ScopeSource
	}
	for _, src := range s.sources {
		if c, ok := src.Table.LookupColumn(id); ok {
			matches = append(matches, struct {
				col Column
				src ScopeSource
			}{c, src})
		}
	}
	// A SHADOWING source (a lateral array unnest binding, RFC-142) wins over
	// non-shadowing sources at this level: when ≥1 shadowing source matches,
	// keep only the shadowing matches (Java's unnest binding shadows the outer
	// table's same-named column). Two shadowing matches are still ambiguous.
	if len(matches) > 1 {
		var shadow []struct {
			col Column
			src ScopeSource
		}
		for _, m := range matches {
			if m.src.Shadowing {
				shadow = append(shadow, m)
			}
		}
		if len(shadow) > 0 {
			matches = shadow
		}
	}
	switch len(matches) {
	case 1:
		return matches[0].col, matches[0].src, nil
	case 0:
		// No match here — walk parent chain.
		if s.parent != nil {
			return s.parent.ResolveColumn(id)
		}
		return Column{}, ScopeSource{}, &ColumnNotFoundError{Id: id}
	default:
		sources := make([]Identifier, 0, len(matches))
		for _, m := range matches {
			sources = append(sources, m.src.Alias)
		}
		return Column{}, ScopeSource{}, &AmbiguousColumnError{
			Id: id, Matches: len(matches), Sources: sources,
		}
	}
}

// ResolveQualifiedColumn handles `alias.col` — looks up a source
// whose Alias matches the qualifier, then the column on that
// source's Table. Unlike ResolveColumn, a qualifier-matched source
// is unambiguous; if the qualifier doesn't match any source in any
// enclosing scope, returns SourceNotFoundError.
func (s *Scope) ResolveQualifiedColumn(qualifier, col Identifier) (Column, ScopeSource, error) {
	for _, src := range s.sources {
		if src.Alias.EqualsIgnoreQuoting(qualifier) {
			c, ok := src.Table.LookupColumn(col)
			if !ok {
				return Column{}, ScopeSource{}, &ColumnNotFoundError{TableName: src.Table.Name(), Id: col}
			}
			return c, src, nil
		}
	}
	if s.parent != nil {
		return s.parent.ResolveQualifiedColumn(qualifier, col)
	}
	// Collect all visible aliases across the chain for a better
	// error message.
	all := s.AllSourcesRecursive()
	avail := make([]Identifier, 0, len(all))
	for _, src := range all {
		avail = append(avail, src.Alias)
	}
	return Column{}, ScopeSource{}, &SourceNotFoundError{
		Alias: qualifier, Available: avail,
	}
}

// AmbiguousColumnError is returned when a bare column reference
// matches multiple sources at the same scope level. Carries the
// conflicting identifier and the conflicting source aliases so the
// user knows which tables to qualify against.
type AmbiguousColumnError struct {
	Id Identifier
	// Matches is always equal to len(Sources); exists as a
	// convenience accessor for callers who don't need the full
	// alias list. Future API tightening may remove it — prefer
	// len(Sources) for new code.
	Matches int
	// Sources is the list of ScopeSource aliases that matched,
	// allowing the user-facing message to suggest
	// `alias.column` for each candidate.
	Sources []Identifier
}

func (e *AmbiguousColumnError) Error() string {
	if len(e.Sources) == 0 {
		return fmt.Sprintf("ambiguous column %s (matches %d sources)", e.Id, e.Matches)
	}
	names := make([]string, 0, len(e.Sources))
	for _, s := range e.Sources {
		names = append(names, s.Name())
	}
	return fmt.Sprintf("ambiguous column %s (matched by: %s)", e.Id, joinStrings(names, ", "))
}

// joinStrings is a tiny strings.Join to avoid pulling strings into
// the package import surface (we already ToUpper via strings
// elsewhere — reuse would be fine but this keeps the error-building
// path cheap).
func joinStrings(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += sep + p
	}
	return out
}

// SourceNotFoundError is returned when a qualifier doesn't match
// any FROM-clause alias in the scope chain. Carries the list of
// available aliases (inner-first) so callers can render a "did you
// mean?" suggestion.
type SourceNotFoundError struct {
	Alias     Identifier
	Available []Identifier
}

func (e *SourceNotFoundError) Error() string {
	if len(e.Available) == 0 {
		return fmt.Sprintf("no FROM source aliased as %s", e.Alias)
	}
	names := make([]string, 0, len(e.Available))
	for _, a := range e.Available {
		names = append(names, a.Name())
	}
	return fmt.Sprintf("no FROM source aliased as %s (available: %s)",
		e.Alias, joinStrings(names, ", "))
}

// DuplicateAliasError is returned by AddSource when the same alias
// is already registered at this scope level.
type DuplicateAliasError struct {
	Alias Identifier
}

func (e *DuplicateAliasError) Error() string {
	return fmt.Sprintf("duplicate alias %s in FROM clause", e.Alias)
}
