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
	// HiddenColumns names columns of this source that UNQUALIFIED
	// references skip — Java's Expression visibility
	// (SemanticAnalyzer.java:468: an unqualified reference ignores a
	// non-visible attribute; a qualified reference still binds it). A
	// JOIN … USING marks the RIGHT side's copy of each USING column
	// hidden (QueryVisitor.resolveJoinUsingClause → asHidden), which is
	// what makes a bare reference to the USING column resolve the LEFT
	// copy instead of being ambiguous. Keys are UPPER-folded bare names.
	HiddenColumns map[string]struct{}
}

// hidesColumn reports whether this source hides id from UNQUALIFIED
// resolution. Fold matches the scope's case-insensitive lookup.
func (s ScopeSource) hidesColumn(id Identifier) bool {
	if len(s.HiddenColumns) == 0 {
		return false
	}
	_, hidden := s.HiddenColumns[strings.ToUpper(id.Name())]
	return hidden
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

// matchingColumns returns EVERY column of tbl whose name matches id — Java's
// per-attribute lookup, which appends one candidate per matching output
// expression rather than stopping at the first (SemanticAnalyzer.java:417,
// :422 then reject a list longer than one with AMBIGUOUS_COLUMN).
//
// Base tables cannot repeat a column name, so this differs from
// Table.LookupColumn only for the synthesised sources that CAN: a derived
// table whose body repeats a star, or a CTE over one.
//
// The fast path is deliberate — Columns() copies defensively, and the
// overwhelming majority of lookups are against base tables.
func matchingColumns(tbl Table, id Identifier) []Column {
	first, ok := tbl.LookupColumn(id)
	if !ok {
		return nil
	}
	var out []Column
	seenFirst := false
	for _, c := range tbl.Columns() {
		if !c.Id.EqualsIgnoreQuoting(id) {
			continue
		}
		if !seenFirst {
			seenFirst = true
			continue
		}
		if out == nil {
			out = []Column{first}
		}
		out = append(out, c)
	}
	if out == nil {
		return []Column{first}
	}
	return out
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
		// An UNQUALIFIED reference skips a source's hidden columns
		// (SemanticAnalyzer.java:468) — the right-side USING copy stays
		// addressable via its qualifier only.
		if src.hidesColumn(id) {
			continue
		}
		// EVERY matching attribute of the source counts, not the first.
		// Java's lookup walks the operator's output expressions and appends
		// each match into one list, so a source that emits the name TWICE
		// (a derived body with a repeated star: `SELECT a.*, a.* FROM a`)
		// contributes two candidates and the reference is ambiguous
		// (SemanticAnalyzer.java:417,:422). A first-match lookup answered
		// such a reference off column 0 instead — silently, since duplicate
		// output names are legal to PRODUCE and only illegal to REFERENCE.
		for _, c := range matchingColumns(src.Table, id) {
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
//
// This is the chain-free form, for callers that need only the resolved
// (column, source) IDENTITY. A resolution that DESCENDED into a struct column
// has no identity to report without its chain — the Column it would hand back
// is the struct ROOT, not the field the reference named — so this form DECLINES
// it with NestedResolutionError rather than returning the root. Returning the
// root is a wrong-column answer that no caller can detect, and a caller that
// wants the descent already has ResolveQualifiedColumnNested.
func (s *Scope) ResolveQualifiedColumn(qualifier, col Identifier) (Column, ScopeSource, error) {
	c, src, accessors, err := s.ResolveQualifiedColumnNested(qualifier, col)
	if err != nil {
		return c, src, err
	}
	if len(accessors) > 0 {
		return Column{}, ScopeSource{}, &NestedResolutionError{Qualifier: qualifier, Id: col}
	}
	return c, src, nil
}

// NestedResolutionError reports a reference that resolved by DESCENDING into a
// struct column, asked of a lookup form that cannot express the descent. It is
// never a user-facing error: every SQL path resolves through
// ResolveQualifiedColumnNested. It exists so the chain-free form fails LOUDLY
// instead of answering with the struct root.
type NestedResolutionError struct {
	Qualifier Identifier
	Id        Identifier
}

func (e *NestedResolutionError) Error() string {
	return fmt.Sprintf("reference %s.%s descends into a struct column; resolve it "+
		"through ResolveQualifiedColumnNested, which carries the accessor chain",
		e.Qualifier.Name(), e.Id.Name())
}

// NestedAccessor is one resolved step of a descent INTO a struct column —
// Java's FieldValue.Accessor(name, ordinal) as lookupNestedField mints it
// (SemanticAnalyzer.java:586-588). Ordinal is the field's position in the
// enclosing struct's declared field list, which is the ordinal Java's
// resolveFieldPath ends up storing (FieldValue.java:288-295).
type NestedAccessor struct {
	// Name is the struct field's declared name.
	Name string
	// Ordinal is its position in the enclosing struct's field list.
	Ordinal int
	// Col is the field's own column view, so a consumer can type the result
	// and a deeper descent can continue from it.
	Col Column
}

// ResolveQualifiedColumnNested is ResolveQualifiedColumn plus Java's
// lookupNestedField rule (SemanticAnalyzer.java:481-488 — the fifth and last
// matching rule `lookup` applies per output attribute).
//
// Two candidate kinds compete at each scope level:
//
//   - a DIRECT match: `qualifier` names a FROM source and `col` is one of its
//     columns. This is the shape every reference had before struct columns
//     existed, and it is unchanged.
//   - a NESTED match: `qualifier` names a STRUCT COLUMN of some source in
//     scope and `col` is one of that struct's fields, so the reference
//     `home_address.city` descends rather than addressing a source.
//
// They are counted TOGETHER, which is what makes a reference that both kinds
// could answer an ambiguity rather than a silent preference. Java reaches the
// same place by a different route: rules 1-4 and rule 5 all append into the
// one `directMatchesBuilder` list and `resolveIdentifierMaybe` errors when
// that list holds more than one entry (SemanticAnalyzer.java:433-437,
// "Ambiguous reference %s", ErrorCode.AMBIGUOUS_COLUMN). A nested candidate
// evaluated only after direct resolution FAILED would resolve that collision
// by order of attempt, and order of attempt is not a semantics.
//
// Everything else about the level walk is unchanged, including the property
// that a zero-match level falls through to the parent.
func (s *Scope) ResolveQualifiedColumnNested(qualifier, col Identifier) (Column, ScopeSource, []NestedAccessor, error) {
	return s.ResolvePathNested([]Identifier{qualifier, col})
}

// descendStruct walks `rest` as a chain of struct-field steps starting from
// `col`, returning one NestedAccessor per step — Java's remainingPath accessor
// loop (SemanticAnalyzer.java:576-593). The loop is UNBOUNDED there and here:
// each step re-types on the field it just consumed, so `n.inner.leaf` descends
// as far as the declared struct nesting goes. A step whose current type is not
// a struct, or whose name is not one of its fields, fails the whole descent —
// Java returns Optional.empty() in both cases rather than a partial path.
//
// An empty `rest` succeeds with no accessors: the reference addresses the
// column itself, which is what makes the alias-qualified direct form
// (`a.id`) and the descending form (`a.n.sk`) one rule instead of two.
func descendStruct(col Column, rest []Identifier) ([]NestedAccessor, bool) {
	if len(rest) == 0 {
		return nil, true
	}
	acc := make([]NestedAccessor, 0, len(rest))
	cur := col
	for _, seg := range rest {
		field, ord, found := cur.LookupStructField(seg)
		if !found {
			return nil, false
		}
		acc = append(acc, NestedAccessor{Name: field.Id.Name(), Ordinal: ord, Col: field})
		cur = field
	}
	return acc, true
}

// ResolvePathNested resolves a dotted reference of ARBITRARY depth — the shape
// Java resolves natively because its Identifier carries `name` plus a
// `List<String> qualifier` and every rule reasons over the joined
// `fullyQualifiedName()` list (IdentifierVisitor.java:56-64 builds it segment
// by segment; SemanticAnalyzer.lookupNestedField consumes a matched PREFIX and
// walks whatever remains). Go's two-argument (qualifier, column) shape could
// express only the first two segments, so `a.n.sk` was flattened into a
// qualifier string "A.N" that names neither a source nor a struct column and
// the reference died as UNDEFINED_COLUMN.
//
// Two candidate kinds compete per source, counted TOGETHER so a reference both
// could answer is an ambiguity rather than a silent preference:
//
//   - STRUCT-RELATIVE: segs[0] names a column of the source and segs[1:]
//     descend into it (`n.sk`, `n.inner.leaf`). This carries no source
//     qualifier at all, so it is tried against EVERY source in scope.
//   - ALIAS-QUALIFIED: segs[0] names the source, segs[1] one of its columns,
//     and segs[2:] descend into that column (`a.id`, `a.n.sk`).
//
// The alias-qualified arm is what keeps two sources declaring the same struct
// apart: `a.n.sk` and `b.n.sk` each match exactly one source because the
// leading segment is compared against the source ALIAS, not discarded.
//
// The scope-chain walk is unchanged from the two-segment form it subsumes:
// ambiguity at a level is terminal, a zero-match level falls through to the
// parent, and exhaustion reports ColumnNotFound when some alias matched
// somewhere and SourceNotFound when nothing did.
func (s *Scope) ResolvePathNested(segs []Identifier) (Column, ScopeSource, []NestedAccessor, error) {
	if len(segs) == 0 {
		return Column{}, ScopeSource{}, nil, &ColumnNotFoundError{}
	}
	if len(segs) == 1 {
		c, src, err := s.ResolveColumn(segs[0])
		return c, src, nil, err
	}
	qualifier := segs[0]
	leaf := segs[len(segs)-1]
	var firstAliasTable QualifiedName
	aliasSeen := false
	for cur := s; cur != nil; cur = cur.parent {
		var matches []struct {
			col       Column
			src       ScopeSource
			accessors []NestedAccessor
		}
		for _, src := range cur.sources {
			// Rule 5 (nested): segs[0] names a STRUCT column of this source.
			// Checked for EVERY source, not only alias-matching ones, because
			// the struct column is reached through the source's columns — the
			// reference `home_address.city` carries no source qualifier at all.
			if structCol, ok := src.Table.LookupColumn(qualifier); ok {
				if acc, found := descendStruct(structCol, segs[1:]); found {
					matches = append(matches, struct {
						col       Column
						src       ScopeSource
						accessors []NestedAccessor
					}{structCol, src, acc})
				}
			}
			if !src.Alias.EqualsIgnoreQuoting(qualifier) {
				continue
			}
			if !aliasSeen {
				aliasSeen = true
				firstAliasTable = src.Table.Name()
			}
			// Per-attribute, exactly as the bare form: a source emitting the
			// name twice makes `nested.id` ambiguous, not first-match.
			for _, c := range matchingColumns(src.Table, segs[1]) {
				acc, found := descendStruct(c, segs[2:])
				if !found {
					continue
				}
				matches = append(matches, struct {
					col       Column
					src       ScopeSource
					accessors []NestedAccessor
				}{c, src, acc})
			}
		}
		switch len(matches) {
		case 1:
			return matches[0].col, matches[0].src, matches[0].accessors, nil
		case 0:
			continue // zero-match level: fall through to the parent (Java)
		default:
			sources := make([]Identifier, 0, len(matches))
			for _, m := range matches {
				sources = append(sources, m.src.Alias)
			}
			return Column{}, ScopeSource{}, nil, &AmbiguousColumnError{
				Id: leaf, Qualifier: qualifier, Path: segs,
				Matches: len(matches), Sources: sources,
			}
		}
	}
	if aliasSeen {
		return Column{}, ScopeSource{}, nil, &ColumnNotFoundError{TableName: firstAliasTable, Id: leaf}
	}
	// Collect all visible aliases across the chain for a better
	// error message.
	all := s.AllSourcesRecursive()
	avail := make([]Identifier, 0, len(all))
	for _, src := range all {
		avail = append(avail, src.Alias)
	}
	return Column{}, ScopeSource{}, nil, &SourceNotFoundError{
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
	// Path is the reference's FULL segment list when it was resolved as a
	// dotted path (`a.n.sk` → [A N SK]). Qualifier/Id keep the first and last
	// segments so two-segment callers are unaffected, but they cannot render a
	// deeper reference AS WRITTEN — and the rendering is the message operand
	// Java prints, so a three-segment ambiguity would otherwise report a
	// reference the user never typed. Empty for callers that pass no path.
	Path []Identifier
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
	if len(e.Path) > 0 {
		parts := make([]string, len(e.Path))
		for i, p := range e.Path {
			parts[i] = p.Name()
		}
		return joinStrings(parts, ".")
	}
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
