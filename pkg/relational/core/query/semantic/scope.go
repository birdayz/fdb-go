package semantic

import (
	"fmt"
	"strings"
)

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

// AddSource appends a FROM-clause source. Duplicate PLAIN aliases at the
// same level are ACCEPTED — Java registers quantifiers freely (unique ids;
// the SQL alias is only a display qualifier) and errors per-ATTRIBUTE at
// reference resolution; the caller distinguishes duplicate legs via
// CorrelationName (the parser-minted binding id). A duplicate involving a
// SHADOWING source (a lateral-unnest AS/AT binding, either direction) still
// errors: Java genuinely forbids a duplicate unnest alias at FROM (RFC-142),
// and the scope-level signal is what the join-ON builder's drop-risk
// taxonomy keys on.
func (s *Scope) AddSource(src ScopeSource) error {
	// A nil-Table source is the declared-but-underivable CTE TOMBSTONE (or a
	// construction bug) — never a resolvable relation. Rejecting it HERE, at
	// the single chokepoint, converts every consumer that forgets the
	// tombstone check into a clean decline instead of a later nil
	// dereference inside ResolveColumn (recovered panic → XX000 on a valid
	// query). Legitimate virtual sources (lateral unnest) always carry a
	// StaticTable.
	if src.Table == nil {
		return &UnresolvableSourceError{Alias: src.Alias}
	}
	// CorrelationName is the RUNTIME correlation key and lives in the
	// CANONICAL UPPER namespace (merged-row leg names, sourceBinding,
	// the bake/window sites are all upper-folded). Canonicalize at the
	// single registration chokepoint so a quoted-verbatim alias
	// (`AS "q$1"` — Alias keeps the verbatim text for quote-aware
	// RESOLUTION) still keys the runtime namespace consistently; the
	// verbatim form leaking through here made a correlated EXISTS over
	// such an alias silently match no leg (identity compare against the
	// upper leg name) and serve zero rows. Minted bindings (Q$DUPn) are
	// fold-stable upper already, and the lowercase q$N machine-counter
	// namespace stays case-DISJOINT from every upper-folded user
	// correlation — quoted "q$5" cannot forge a planner-minted q$5.
	if src.CorrelationName != "" {
		src.CorrelationName = strings.ToUpper(src.CorrelationName)
	}
	for _, existing := range s.sources {
		if !existing.Alias.EqualsIgnoreQuoting(src.Alias) {
			// FOLD-COLLISION guard: two DIFFERENT aliases whose
			// correlation keys canonicalize to the same upper form
			// (`AS "q$1"` beside `AS "Q$1"`) would be two legs behind ONE
			// runtime key — first-span-wins silent misbinding. Java keeps
			// quoted identifiers case-distinct end-to-end and can never
			// conflate them; we reject loudly. Equal-alias duplicates
			// (the dup-FROM-alias class) keep distinct minted binding
			// keys and are adjudicated per-attribute downstream.
			if src.CorrelationName != "" && existing.CorrelationName == src.CorrelationName {
				return &DuplicateAliasError{Alias: src.Alias}
			}
			continue
		}
		if existing.Shadowing || src.Shadowing {
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

// ResolveQualifiedColumn handles `alias.col` with Java's PER-ATTRIBUTE
// semantics (SemanticAnalyzer.resolveIdentifierMaybe + resolveAcrossFragments):
//   - ALL alias-matching sources at a level are candidates; >1 carrying the
//     column is ambiguous (42702 at the caller) — ambiguity is TERMINAL, it
//     never falls through to a parent scope;
//   - ZERO matches at a level fall through to the PARENT — even when the
//     alias exists locally without the column (the correlated shadow shape:
//     `SELECT p.v FROM t1 AS p WHERE EXISTS(SELECT 1 FROM t2 AS p WHERE
//     p.v = 10)` ANSWERS in Java, live-verified);
//   - at chain exhaustion: ColumnNotFoundError when some alias-matching
//     source existed anywhere on the chain (named after the innermost one),
//     SourceNotFoundError when the qualifier matched nothing.
func (s *Scope) ResolveQualifiedColumn(qualifier, col Identifier) (Column, ScopeSource, error) {
	var firstAliasTable QualifiedName
	aliasSeen := false
	for cur := s; cur != nil; cur = cur.parent {
		var matches []struct {
			col Column
			src ScopeSource
		}
		for _, src := range cur.sources {
			if !src.Alias.EqualsIgnoreQuoting(qualifier) {
				continue
			}
			if !aliasSeen {
				aliasSeen = true
				firstAliasTable = src.Table.Name()
			}
			if c, ok := src.Table.LookupColumn(col); ok {
				matches = append(matches, struct {
					col Column
					src ScopeSource
				}{c, src})
			}
		}
		switch len(matches) {
		case 1:
			return matches[0].col, matches[0].src, nil
		case 0:
			continue // zero-match level: fall through to the parent (Java)
		default:
			sources := make([]Identifier, 0, len(matches))
			for _, m := range matches {
				sources = append(sources, m.src.Alias)
			}
			return Column{}, ScopeSource{}, &AmbiguousColumnError{
				Id: col, Qualifier: qualifier, Matches: len(matches), Sources: sources,
			}
		}
	}
	if aliasSeen {
		return Column{}, ScopeSource{}, &ColumnNotFoundError{TableName: firstAliasTable, Id: col}
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

// AmbiguousColumnError is returned when a column reference matches
// multiple sources at the same scope level — bare (two tables expose
// the name) or qualified (two same-aliased sources both carry the
// column, Java's per-attribute 42702). Carries the conflicting
// identifier and the conflicting source aliases so the user knows
// which tables to qualify against.
type AmbiguousColumnError struct {
	Id Identifier
	// Qualifier is the reference's qualifier for the QUALIFIED form
	// (`a.id` over duplicate aliases both carrying id); zero for a bare
	// reference. Callers render Java's exact message from the reference
	// as written: `Ambiguous reference A.ID` / `Ambiguous reference ID`.
	Qualifier Identifier
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

// Reference renders the ambiguous reference AS WRITTEN (normalized) — Java's
// message operand: `A.ID` for a qualified reference, `ID` for a bare one.
// The callers' user-facing mapping is `Ambiguous reference %s` byte-equal to
// Java's SemanticAnalyzer text (verified for duplicate AND distinct aliases,
// bare AND qualified).
func (e *AmbiguousColumnError) Reference() string {
	if e.Qualifier.Name() != "" {
		return e.Qualifier.Name() + "." + e.Id.Name()
	}
	return e.Id.Name()
}

func (e *AmbiguousColumnError) Error() string {
	if len(e.Sources) == 0 {
		return fmt.Sprintf("ambiguous column %s (matches %d sources)", e.Reference(), e.Matches)
	}
	names := make([]string, 0, len(e.Sources))
	for _, s := range e.Sources {
		names = append(names, s.Name())
	}
	return fmt.Sprintf("ambiguous column %s (matched by: %s)", e.Reference(), joinStrings(names, ", "))
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
// UnresolvableSourceError reports an attempt to add a scope source with no
// Table — the declared-but-underivable CTE tombstone reaching a resolver.
type UnresolvableSourceError struct {
	Alias Identifier
}

func (e *UnresolvableSourceError) Error() string {
	return "source " + e.Alias.Name() + " has no resolvable schema in this context"
}

type DuplicateAliasError struct {
	Alias Identifier
}

func (e *DuplicateAliasError) Error() string {
	return fmt.Sprintf("duplicate alias %s in FROM clause", e.Alias)
}

// CorrelatedShadowError is returned when a qualified reference resolves to a
// PARENT-scope source (Java's zero-match fallthrough) whose correlation name
// is SHADOWED by a local FROM source that lacks the column — an
// emitted-uncorrelatable case. Emitting QOV(correlation) would bind the local
// (inner) leg's quantifier, so resolution declines LOUDLY (never wrong rows);
// this matches Java (unique quantifier ids) and will flip once cross-scope
// binding ids are supported. Carries the reference as written for the
// surfaced message.
type CorrelatedShadowError struct {
	Qualifier string
	Field     string
}

func (e *CorrelatedShadowError) Error() string {
	return fmt.Sprintf("correlated reference %s.%s is shadowed by a same-named FROM source that lacks the column; cross-scope binding is not yet supported", e.Qualifier, e.Field)
}
