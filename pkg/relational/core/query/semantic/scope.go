// Portions derived from FoundationDB Record Layer (SemanticAnalyzer.java,
// FieldValue.java, LogicalPlanFragment.java, Identifier.java, and others),
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2026 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package semantic

import (
	"fmt"
	"strings"

	"fdb.dev/pkg/relational/core/query/logical"
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
	CTE *logical.CTEProducer
	// Table is the resolved schema-level table.
	Table Table
	// Alias is the name used to reference this source in the
	// enclosing query (column qualifier). For `FROM t AS x` → Alias
	// is `x`; for `FROM t` with no alias → Alias is `t`.
	Alias Identifier
	// UnqualifiedOutput distinguishes unnamed SQL operators from their private
	// runtime correlation aliases. Their inherited output names remain eligible
	// for SELECT alias lookup; named operators qualify those same attributes.
	UnqualifiedOutput bool
	// CorrelationName is the identifier the analyzer uses to tie
	// this source back to a Quantifier when building
	// QuantifiedObjectValue / FieldValue trees that reference it.
	// Stored as a string so the semantic package doesn't take a
	// dependency on cascades/values/CorrelationIdentifier — callers wrap
	// this into a cascades.values.CorrelationIdentifier themselves.
	CorrelationName string
	// NamePath, when it has two or more segments, is the name SQL references
	// this source by in place of Alias: an unaliased table qualified by its
	// schema template's name (`FROM T.W`) is named by the whole qualified
	// identifier, as Java names the table operator by it
	// (LogicalOperator.generateTableAccess with no alias), so `T.W.col`
	// qualifies its columns and `W.col` does not. Alias then carries only the
	// last segment, for messages; CorrelationName is unaffected.
	NamePath []Identifier
	// AdditionalQualifiers are query-block-local spellings that may qualify
	// this source without changing its runtime correlation identity. The narrow
	// live use is Java's table-first alias/schema collision: in
	// `FROM PA AS s, s.PB AS B`, where `s` is also the schema template's name
	// (the qualifier a table name may carry), `PA.ID`
	// continues to address the PA source even though its range alias is `s`.
	// Ordinary aliased sources leave this empty, so SQL's usual alias-hides-table
	// rule and the correlation mint remain unchanged.
	AdditionalQualifiers []Identifier
	// Unnamed marks a source this scope level reads through an operator Java
	// builds with preserved expression names and no name
	// (LogicalOperator.newOperatorWithPreservedExpressionNames): an aggregate
	// query block's select-where operator, which its select list, GROUP BY,
	// HAVING and ORDER BY resolve against (QueryVisitor.visitSimpleTable), and
	// an outer join's operator, which replaces the sources it joins for every
	// clause after it (QueryVisitor.wrapOperandsForOuterJoin, and
	// collapseLeftSideOperators before an outer join's ON when two or more
	// sources precede it). Each attribute keeps its qualified name, so `x.f`
	// still names x's column f, but SemanticAnalyzer.lookup's doubled reading
	// needs the operator's name, and lookupNestedField clears an unnamed
	// operator's qualifier: a struct column's field is reached by the column's
	// bare name in the qualified pass too, counted beside the qualified
	// reading, and a qualified path through a column is tried only when the
	// path does not begin with that column's bare name. Measured in
	// conformance/ws_f_table_qualifier_conformance_test.go.
	Unnamed bool
	// Shadowing marks a virtual AS/AT source whose value construction must keep
	// the exact element/ordinal correlation. It grants no lookup precedence:
	// every visible matching attribute participates in ordinary ambiguity.
	Shadowing bool
	// FlowedColumns is the exact row layout carried by this source's quantified
	// object when that layout differs from the columns exposed for SQL name
	// resolution. It is nil for ordinary tables and for a scalar lateral-unnest
	// element (whose quantified object is the whole element). WITH ORDINALITY is
	// the motivating row-valued virtual source: SQL exposes the AS/AT aliases,
	// while an AT-only source still physically carries the unexposed element in
	// slot 0 and the ordinal in slot 1.
	FlowedColumns []Column
	// FlowedObject is the exact whole value carried by a non-row virtual
	// source. A record element publishes its fields and an ephemeral whole
	// alias while the quantifier still flows the original nominal record type;
	// neither the extra alias nor its SQL position is a physical row slot.
	FlowedObject *Column
	// ColumnOrdinals maps SQL attribute positions to physical row slots when
	// they are not parallel to FlowedColumns (for example, AT without AS).
	// A whole-object attribute uses -1 instead of a fabricated row slot.
	ColumnOrdinals []int
	// FlowedNullable is the record-level nullability of FlowedColumns. It is
	// meaningful only when FlowedColumns is non-empty.
	FlowedNullable bool
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

func (s ScopeSource) matchesQualifier(qualifier Identifier) bool {
	if s.NamedBy(qualifier) {
		return true
	}
	for _, alternate := range s.AdditionalQualifiers {
		if alternate.EqualsIgnoreQuoting(qualifier) {
			return true
		}
	}
	return false
}

// NamedBy reports whether SQL names this source by the single identifier q: its
// alias, or its table's name when unaliased. A source named by a qualified path
// (NamePath) is never named by one identifier, its last segment included.
// AdditionalQualifiers are not consulted: they qualify columns, they do not
// name the source.
func (s ScopeSource) NamedBy(q Identifier) bool {
	return len(s.NamePath) < 2 && s.Alias.EqualsIgnoreQuoting(q)
}

// qualifierLength reports how many leading segments of a qualified path
// name this source: the whole NamePath when the source is named by one and the
// path begins with it (with at least one segment after it), else one when the
// first segment matches the source's single-segment name (matchesQualifier),
// else zero.
func (s ScopeSource) qualifierLength(segs []Identifier) int {
	if k := len(s.NamePath); k >= 2 && len(segs) > k {
		named := true
		for i, part := range s.NamePath {
			if !part.EqualsIgnoreQuoting(segs[i]) {
				named = false
				break
			}
		}
		if named {
			return k
		}
	}
	if len(segs) > 1 && s.matchesQualifier(segs[0]) {
		return 1
	}
	return 0
}

// doubledQualifierColumn reports the column a path names through Java's
// doubled qualifier: the source's name with its last segment prepended, then a
// top-level column (`t.t.c` for a source named t, `w.T.w.c` for one named T.w).
// Java's qualified lookup matches an attribute by its table-qualified name with
// the operator's name prepended (SemanticAnalyzer.lookup,
// attributeIdentifier.withQualifier(operatorName.getName()), which PREPENDS),
// so this is a reading of its own, of a top-level column only, never a nested
// path; it counts beside the source's other readings, so it can make a path
// ambiguous. Measured: `SELECT w.w.id FROM w` resolves in both engines, as does
// `x.x.x` over a scalar unnest aliased x (its one attribute is x). An Unnamed
// source takes none: the reading prepends the operator's name.
func (s ScopeSource) doubledQualifierColumn(segs []Identifier) (Identifier, bool) {
	if s.Unnamed {
		return Identifier{}, false
	}
	name := s.NamePath
	if len(name) < 2 {
		if s.Alias.Name() == "" {
			return Identifier{}, false
		}
		name = []Identifier{s.Alias}
	}
	if len(segs) != len(name)+2 || !segs[0].EqualsIgnoreQuoting(name[len(name)-1]) {
		return Identifier{}, false
	}
	for i, part := range name {
		if !part.EqualsIgnoreQuoting(segs[i+1]) {
			return Identifier{}, false
		}
	}
	return segs[len(segs)-1], true
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

// WithUnnamedSources returns this level with every source Unnamed, under the
// same parent: the level an aggregate query block's select list, GROUP BY,
// HAVING and ORDER BY resolve against, Java's generateSelectWhere operator
// over the block's sources. The receiver is unchanged.
func (s *Scope) WithUnnamedSources() *Scope {
	out := *s
	out.sources = make([]ScopeSource, len(s.sources))
	for i, src := range s.sources {
		src.Unnamed = true
		out.sources[i] = src
	}
	return &out
}

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

// AddSource appends a FROM-clause source. SQL aliases may repeat, including
// lateral shadowing sources; references are adjudicated per attribute. Explicit
// runtime correlation identities must remain unique, independent of alias text.
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
		if src.CorrelationName != "" && existing.CorrelationName == src.CorrelationName {
			return &DuplicateAliasError{Alias: src.Alias}
		}
	}
	s.sources = append(s.sources, src)
	return nil
}

// A resolution pass. Every level is resolved twice: STRICT first, and RELAXED
// only if strict found nothing at that same level.
//
// THE SHAPE IS JAVA'S; THE DIMENSION IT RELAXES IS NOT, and the two must not
// be conflated. `SemanticAnalyzer.resolveIdentifierMaybe`
// (SemanticAnalyzer.java:427-438) runs `lookup(id, operators, true)` over ALL
// operators of a fragment and, only when that yields nothing, the same lookup
// with `false` over the SAME operators, before walking to the parent. That
// two-pass, whole-level, strict-then-relaxed structure is exactly what is
// implemented here.
//
// But Java's flag is `matchQualifiedOnly` (SemanticAnalyzer.java:444-446): it
// relaxes whether the reference must be QUALIFIED, and both of its passes
// compare through `Identifier.equals`, which is `String.equals` on the
// normalized name (Identifier.java:155-157). JAVA NEVER RELAXES CASE, under
// any option — `CASE_SENSITIVE_IDENTIFIERS` selects which branch of
// normalizeString runs at the PARSE boundary and leaves the comparison exact
// either way. So the relaxed pass below has no Java analogue and is not a
// port; it is a Go-only read-side extension, argued on its own terms at
// relaxedPass.
//
// The per-level placement is chosen for a reason that stands independently of
// the citation: an inner scope's relaxed match beats an outer scope's exact
// match, which is ordinary SQL shadowing. Letting an outer exact match win
// would silently turn a local reference into a correlated one.
type resolutionPass int

const (
	// strictPass compares identifier text exactly. This is Java's whole rule:
	// SemanticAnalyzer.normalizeString (SemanticAnalyzer.java:146-153) folds an
	// unquoted identifier UPPER and strips a quoted one verbatim at the PARSE
	// boundary, and every catalog comparison downstream is `.equals`.
	strictPass resolutionPass = iota
	// relaxedPass additionally accepts a name that differs only by case.
	//
	// NOT COVERED, and each of these is deliberate:
	//   - qualifiers and source aliases. They originate in SQL text or in the
	//     catalog's own already-folded table index, never in a descriptor, so
	//     there is nothing for a fold to repair.
	//   - the quoting FLAG, beyond what EqualsIgnoreQuoting already ignores.
	//   - `__2` / `__1` / `__0` escaping (dot, dollar, double-underscore —
	//     ProtoUtils.java:39-41, mirrored in protoname.go). That is
	//     un-escaped once, at the catalog boundary, by ToUserIdentifier.
	//   - Unicode case folding beyond strings.EqualFold's simple folding.
	//   - a column whose name the statement's SQL text authored
	//     (Column.SQLAuthored: an unnest's AS/AT alias, a derived table's or
	//     CTE's SELECT-list `AS`, a CTE column list). Like a qualifier it never
	//     came from a descriptor, so it is compared exactly (matchesColumn).
	//
	// COVERED: descriptor-named columns, and struct-field names below them —
	// including a derived table's or CTE's pass-through of one.
	//
	// THIS HAS NO JAVA ANALOGUE. It is a Go-only read-side extension, not a
	// port, and saying so precisely matters because the surrounding structure
	// IS a port and the two are easy to conflate.
	//
	// Java compares identifiers exactly, always: `Identifier.equals` is
	// `String.equals` on the normalized name (Identifier.java:155-157), and
	// `CASE_SENSITIVE_IDENTIFIERS` (Options.java:211) only chooses which
	// branch of normalizeString runs at the PARSE boundary — setting it makes
	// Java MORE case-sensitive, never less. There is no configuration in which
	// Java resolves `foo` against a column called `Foo`; its own
	// case-sensitivity.yamsql shows the mismatch answering UNDEFINED.
	//
	// It exists because the two engines have different POPULATIONS, not
	// because Java's rule is wrong. Java's SQL surface is always DDL-fed, so a
	// descriptor whose field names are not already the normalized SQL spelling
	// is a corner case there. Here, wrapping a user's own hand-written .proto
	// as a SQL catalog is a first-class entry point, so an unquoted
	// `SELECT order_id` over a field literally named `order_id` has to keep
	// working. The extension is read-side only and never reaches the wire,
	// which is what the project's rule permits.
	//
	// Its cost is measured and pinned, not assumed: every `goOnly` arm of
	// QuotedIdentifierCaseJavaProbe records Go answering where Java raises
	// 42703. Deliberately not a count — the arms are a population that grows,
	// and a number here is one nobody re-runs
	// (`grep -c 'mode: goOnly'` on that file is the check).
	relaxedPass
)

// matches reports whether a candidate name answers a reference under this pass.
func (p resolutionPass) matches(have, want Identifier) bool {
	if have.EqualsIgnoreQuoting(want) {
		return true
	}
	return p == relaxedPass && strings.EqualFold(have.Name(), want.Name())
}

// matchesColumn is matches for a candidate COLUMN: a name the statement's SQL
// text authored (Column.SQLAuthored) is compared exactly in both passes,
// because the fold exists only for descriptor spellings.
func (p resolutionPass) matchesColumn(c Column, want Identifier) bool {
	if c.SQLAuthored {
		return strictPass.matches(c.Id, want)
	}
	return p.matches(c.Id, want)
}

// lookupColumn resolves a single column of tbl under this pass.
func (p resolutionPass) lookupColumn(tbl Table, id Identifier) (Column, bool) {
	if col, ok := tbl.LookupColumn(id); ok {
		return col, true
	}
	if p == strictPass {
		return Column{}, false
	}
	for _, c := range tbl.Columns() {
		if p.matchesColumn(c, id) {
			return c, true
		}
	}
	return Column{}, false
}

// lookupStructField resolves a nested field of col under this pass.
func (p resolutionPass) lookupStructField(col Column, id Identifier) (Column, int, bool) {
	if col.IsArray || col.Type != "RECORD" {
		return Column{}, 0, false
	}
	if f, ord, ok := col.LookupStructField(id); ok {
		return f, ord, true
	}
	if p == strictPass {
		return Column{}, 0, false
	}
	for i, f := range col.StructFields {
		if p.matchesColumn(f, id) {
			return f, i, true
		}
	}
	return Column{}, 0, false
}

// LookupColumnRelaxed asks ONE table whether it declares a column the given
// reference could name: the exact spelling first, then a case-insensitive
// match — the same two steps Scope's relaxed pass applies.
//
// It exists for the SINGLE-SOURCE questions that are not resolutions: "does
// this leg still carry the column the outer reference named?", "does the CTE
// body project this name?". Those have nothing to adjudicate across sources,
// which is the whole reason Table.LookupColumn itself stays exact — a table
// that relaxed on its own would let one source's loose match compete with
// another source's exact one. Do NOT use this to resolve a reference; use the
// Scope methods, which run the two passes level by level and count candidates.
func LookupColumnRelaxed(tbl Table, id Identifier) (Column, bool) {
	return relaxedPass.lookupColumn(tbl, id)
}

// matchingColumns returns EVERY column of tbl whose name matches id under the
// pass — Java's per-attribute lookup, which appends one candidate per matching
// output expression rather than stopping at the first
// (SemanticAnalyzer.java:417, :422 then reject a list longer than one with
// AMBIGUOUS_COLUMN).
//
// Counting rather than declining is what makes the relaxed pass safe: two
// columns that differ only by case are two candidates for a folded reference,
// so the reference is AMBIGUOUS (42702) and says so. Declining instead would
// report 42703 — that the column does not exist — about a name that exists
// twice.
//
// Base tables cannot repeat a column name, so this differs from
// Table.LookupColumn only for the synthesised sources that CAN: a derived
// table whose body repeats a star, or a CTE over one.
//
// The fast path is deliberate — Columns() copies defensively, and the
// overwhelming majority of lookups are against base tables.
func matchingColumns(tbl Table, id Identifier, pass resolutionPass) []Column {
	first, ok := pass.lookupColumn(tbl, id)
	if !ok {
		return nil
	}
	var out []Column
	seenFirst := false
	for _, c := range tbl.Columns() {
		if !pass.matchesColumn(c, id) {
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
	// STRICT at this level, then RELAXED at this same level, then the parent.
	// See resolutionPass for why the second pass belongs here and not after
	// the whole chain.
	var matches []struct {
		col Column
		src ScopeSource
	}
	for _, pass := range [...]resolutionPass{strictPass, relaxedPass} {
		for _, src := range s.sources {
			// An UNQUALIFIED reference skips a source's hidden columns
			// (SemanticAnalyzer.java:468) — the right-side USING copy stays
			// addressable via its qualifier only. Hiding is decided the same
			// way in both passes: a column hidden from the strict pass is not
			// un-hidden by a fold.
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
			for _, c := range matchingColumns(src.Table, id, pass) {
				matches = append(matches, struct {
					col Column
					src ScopeSource
				}{c, src})
			}
		}
		if len(matches) > 0 {
			break
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
// Two candidate kinds, in Java's order (ResolvePathNested):
//
//   - a DIRECT match: `qualifier` names a FROM source and `col` is one of its
//     columns. This is the shape every reference had before struct columns
//     existed.
//   - a NESTED match: `qualifier` names a STRUCT COLUMN of some source in
//     scope and `col` is one of that struct's fields, so the reference
//     `home_address.city` descends rather than addressing a source.
//
// Java 4.14.2.0's resolveIdentifierMaybe looks the direct (qualified) match
// up first and the nested one only when that finds nothing, so a source's
// column outranks a same-named struct column's field (measured: `SELECT h.f
// FROM y, h` is table h's f). Two candidates of one kind at a level are an
// ambiguity ("Ambiguous reference %s", ErrorCode.AMBIGUOUS_COLUMN).
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
func descendStruct(col Column, rest []Identifier, pass resolutionPass) ([]NestedAccessor, bool) {
	if len(rest) == 0 {
		return nil, true
	}
	acc := make([]NestedAccessor, 0, len(rest))
	cur := col
	for _, seg := range rest {
		field, ord, found := pass.lookupStructField(cur, seg)
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
// Two candidate kinds, in Java's order (SemanticAnalyzer.resolveIdentifierMaybe
// runs lookup(…, matchQualifiedOnly=true) and only when that finds nothing
// lookup(…, false)):
//
//   - QUALIFIED, first: the leading segments name the source (its alias, or the
//     whole NamePath), the next segment one of its columns, and the rest
//     descend into that column (`a.id`, `a.n.sk`, `T.W.id`); or Java's doubled
//     qualifier names a top-level column (doubledQualifierColumn, `t.t.c`).
//     Every such candidate at the level counts, so two are an ambiguity.
//   - STRUCT-RELATIVE, only when no qualified candidate exists: segs[0] names a
//     column of the source and segs[1:] descend into it (`n.sk`,
//     `n.inner.leaf`). It carries no source qualifier, so it is tried against
//     EVERY source in scope. Java's qualified lookup cannot produce it (a named
//     operator's attributes carry the operator's name, and a reference without
//     it is no prefix of theirs), so `x.f` over a table x with a column f and
//     a struct column x holding f is x's f, as Java answers (measured).
//
// The qualified arm is what keeps two sources declaring the same struct apart:
// `a.n.sk` and `b.n.sk` each match exactly one source because the leading
// segment is compared against the source ALIAS, not discarded. The RELAXED
// pass (the declared case over-resolution) takes both kinds together, so a
// fold that reaches a field by both readings is ambiguous rather than the
// first reading's, as resolveUpdateColumn's fold is.
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
		var matches []pathCandidate
		// STRICT then RELAXED at this level; only then the parent. The
		// qualifier is compared exactly in BOTH passes — a source alias never
		// comes from a descriptor, so a fold has nothing to repair there.
		for _, pass := range [...]resolutionPass{strictPass, relaxedPass} {
			direct, structRelative, seen, table := cur.levelPathCandidates(segs, pass)
			if seen && !aliasSeen {
				aliasSeen, firstAliasTable = true, table
			}
			matches = narrowPathMatches(direct, structRelative, pass)
			if len(matches) > 0 {
				break
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
		return Column{}, ScopeSource{}, nil, &ColumnNotFoundError{TableName: firstAliasTable, Id: leaf, Path: append([]Identifier(nil), segs...)}
	}
	// Collect all visible aliases across the chain for a better
	// error message.
	all := s.AllSourcesRecursive()
	avail := make([]Identifier, 0, len(all))
	for _, src := range all {
		avail = append(avail, src.Alias)
	}
	return Column{}, ScopeSource{}, nil, &SourceNotFoundError{
		Alias: qualifier, Available: avail, Path: append([]Identifier(nil), segs...),
	}
}

// pathCandidate is one attribute a multi-segment path reads at a scope level.
type pathCandidate struct {
	col              Column
	src              ScopeSource
	accessors        []NestedAccessor
	ephemeralDerived bool
	// doubled marks the doubled reading's match. Java names it by its
	// attribute's own name, not by the requested path, so it does not
	// cover a nested match through an ephemeral whole object (lookup
	// drops an ephemeral-derived match only when a direct match carries
	// its name): `item.item.b` over a record element aliased item is
	// ambiguous (measured).
	doubled bool
}

// levelPathCandidates is ONE scope level's lookup of a multi-segment path under
// one pass. direct holds the readings Java's first (qualified-only) lookup
// returns — a source-qualified reading, a doubled reading, a struct column of
// an unnamed operator read by its bare name — and structRelative a named
// source's struct-relative readings, which only Java's second lookup returns.
// aliasSeen and firstAliasTable report the first source the leading segment
// named, for the refusal's wording.
func (s *Scope) levelPathCandidates(segs []Identifier, pass resolutionPass) (matches, structRelative []pathCandidate, aliasSeen bool, firstAliasTable QualifiedName) {
	qualifier := segs[0]
	for _, src := range s.sources {
		// Rule 5 (nested): segs[0] names a STRUCT column of this source.
		// Checked for EVERY source, not only alias-matching ones, because
		// the struct column is reached through the source's columns — the
		// reference `home_address.city` carries no source qualifier at all.
		k := src.qualifierLength(segs)
		for _, structCol := range matchingColumns(src.Table, qualifier, pass) {
			if acc, found := descendStruct(structCol, segs[1:], pass); found {
				c := pathCandidate{structCol, src, acc, structCol.Ephemeral, false}
				if src.Unnamed {
					// An unnamed operator's attribute keeps its qualified
					// name, so a reference spelling exactly that name is
					// the attribute's direct match, and Java stops there
					// (lookup's `continue`) before the nested path through
					// the same attribute: `ss.ss` over table ss with a
					// struct column ss holding ss is the column (measured).
					if k > 0 && len(segs) == k+1 && pass.matchesColumn(structCol, segs[k]) {
						continue
					}
					// Otherwise it is looked up by its bare name in the
					// qualified pass (lookupNestedField clears the
					// qualifier), beside the qualified reading.
					matches = append(matches, c)
				} else {
					structRelative = append(structRelative, c)
				}
			}
		}
		doubled, isDoubled := src.doubledQualifierColumn(segs)
		if k == 0 && !isDoubled {
			continue
		}
		if !aliasSeen {
			aliasSeen = true
			firstAliasTable = src.Table.Name()
		}
		// Per-attribute, exactly as the bare form: a source emitting the
		// name twice makes `nested.id` ambiguous, not first-match.
		if k > 0 {
			for _, c := range matchingColumns(src.Table, segs[k], pass) {
				// Through an unnamed operator, a path beginning with the
				// column's bare name is read from the column
				// (lookupNestedField's cleared-qualifier prefix) and never
				// as the qualified path through it: `x.x.f` over table x
				// with a struct column x names no column there (measured).
				if src.Unnamed && len(segs) > k+1 && pass.matchesColumn(c, segs[0]) {
					continue
				}
				// A lateral unnest's whole element is named by its bare
				// alias (LogicalOperator.generateCorrelatedFieldAccess
				// names the EphemeralExpression by the alias alone), so
				// through an unnamed operator nothing qualifies it:
				// `COUNT(item.item) … GROUP BY` is 42703 where
				// `COUNT(item)` is the element (measured).
				if src.Unnamed && src.Shadowing && src.FlowedObject != nil && c.Ephemeral {
					continue
				}
				acc, found := descendStruct(c, segs[k+1:], pass)
				if !found {
					continue
				}
				matches = append(matches, pathCandidate{c, src, acc, c.Ephemeral && len(acc) > 0, false})
			}
		}
		if isDoubled {
			for _, c := range matchingColumns(src.Table, doubled, pass) {
				// Java tries each attribute's direct forms before its
				// nested path and stops at the first that matches
				// (lookup's `continue`), so the doubled reading of a
				// column replaces a qualified path through that same
				// column rather than competing with it: `ss.ss.ss`, where
				// table ss has a struct column ss holding a field ss, is
				// the column (measured).
				kept := matches[:0]
				for _, m := range matches {
					if m.src.CorrelationName != src.CorrelationName || !m.col.Id.EqualsIgnoreQuoting(c.Id) {
						kept = append(kept, m)
					}
				}
				matches = append(kept, pathCandidate{c, src, nil, false, true})
			}
		}
	}
	return matches, structRelative, aliasSeen, firstAliasTable
}

// narrowPathMatches applies Java's lookup rules to one lookup's candidates: the
// struct-relative readings count only when no direct one does (strict), both
// together when folding; and a nested route through an ephemeral whole object
// drops out when a direct attribute carries the name.
func narrowPathMatches(matches, structRelative []pathCandidate, pass resolutionPass) []pathCandidate {
	// Java's second lookup: a named source's struct-relative reading
	// only when the qualified one found nothing (strict), both together
	// when folding.
	if pass == relaxedPass || len(matches) == 0 {
		matches = append(matches, structRelative...)
	}
	// Every match but the doubled one is named by this requested
	// identifier. Java drops a nested route through an ephemeral whole
	// object when a direct attribute already carries that name.
	// Independent non-ephemeral attributes still compete, regardless of
	// source.
	hasDirect := false
	for _, match := range matches {
		if !match.ephemeralDerived && !match.doubled {
			hasDirect = true
			break
		}
	}
	if hasDirect {
		kept := matches[:0]
		for _, match := range matches {
			if !match.ephemeralDerived {
				kept = append(kept, match)
			}
		}
		matches = kept
	}
	return matches
}

// ResolvePathAcrossLevels resolves a FROM item's correlated path as Java does:
// resolveCorrelatedIdentifier's resolveIdentifier over the operators of this
// query block and every enclosing one as ONE list
// (LogicalPlanFragment.getLogicalOperatorsIncludingOuter). Java looks up that
// list twice (SemanticAnalyzer.resolveIdentifier): the qualified-only lookup
// first, where more than one answer is ambiguous, and the struct-relative
// lookup only when the first finds nothing. So a qualified reading at one level
// wins over a struct-relative one at another — `EXISTS (SELECT 1 FROM w AS n,
// n.arr AS x)` under an outer t2 whose struct column n holds arr is the inner
// N.ARR (measured) — and two qualified readings at different levels are 42702
// (`EXISTS (SELECT 1 FROM w, w.arr AS v)` under an outer w, measured).
//
// Each level contributes the candidates ResolvePathNested reads there
// (levelPathCandidates), under the strict pass and then Go's relaxed pass,
// which folds case; the relaxed pass runs only when the strict one answers
// nothing at any level. A single-segment path is not a correlated identifier
// (Java refuses it as an unknown table before any lookup); it keeps
// ResolvePathNested's innermost-level reading.
func (s *Scope) ResolvePathAcrossLevels(segs []Identifier) (Column, ScopeSource, []NestedAccessor, error) {
	if len(segs) < 2 {
		return s.ResolvePathNested(segs)
	}
	for _, pass := range [...]resolutionPass{strictPass, relaxedPass} {
		var direct, structRelative []pathCandidate
		for cur := s; cur != nil; cur = cur.parent {
			d, r, _, _ := cur.levelPathCandidates(segs, pass)
			direct = append(direct, d...)
			structRelative = append(structRelative, r...)
		}
		matches := narrowPathMatches(direct, structRelative, pass)
		switch len(matches) {
		case 0:
			continue
		case 1:
			return matches[0].col, matches[0].src, matches[0].accessors, nil
		}
		sources := make([]Identifier, len(matches))
		for i, m := range matches {
			sources[i] = m.src.Alias
		}
		return Column{}, ScopeSource{}, nil, &AmbiguousColumnError{
			Id: segs[len(segs)-1], Qualifier: segs[0], Path: append([]Identifier(nil), segs...),
			Matches: len(matches), Sources: sources,
		}
	}
	// Nothing answers: ResolvePathNested's own refusal, which names what the
	// leading segment reached.
	return s.ResolvePathNested(segs)
}

// ResolveSourceQualifiedPath resolves a path whose leading segment is already
// proven by the grammar/caller to name a FROM source. Unlike ResolvePathNested,
// it does not also consider the leading segment as a struct column, which a
// FROM item's path never is once the FROM-item classifier has established that
// its first segment names a preceding source (`FROM t, t.records AS item,
// item.item AS leaf`). Nor does it take the doubled reading: its callers
// resolve Go's own expansion of a FROM item's path through a prior unnest's
// whole element (`item.m` read as `item.item.m`, unnestSemanticPath), never a
// path a user spelled, whose doubled reading ResolvePathNested and
// ResolvePathAcrossLevels take (`FROM w, w.w.arr AS x` unnests w.arr, measured).
//
// Duplicate source aliases retain Java's per-attribute ambiguity rule: all
// alias-matching sources at one scope level are considered, and more than one
// complete match is loud. Zero matches fall through to the parent exactly as
// ResolvePathNested does. An ordinary expression resolves through
// ResolvePathNested, which weighs the source-qualified reading against the
// struct-relative and doubled ones as Java's lookup does.
func (s *Scope) ResolveSourceQualifiedPath(segs []Identifier) (Column, ScopeSource, []NestedAccessor, error) {
	if len(segs) < 2 {
		return Column{}, ScopeSource{}, nil, &ColumnNotFoundError{}
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
		// STRICT then RELAXED at this level, as in ResolvePathNested.
		for _, pass := range [...]resolutionPass{strictPass, relaxedPass} {
			for _, src := range cur.sources {
				k := src.qualifierLength(segs)
				if k == 0 {
					continue
				}
				if !aliasSeen {
					aliasSeen = true
					firstAliasTable = src.Table.Name()
				}
				for _, c := range matchingColumns(src.Table, segs[k], pass) {
					accessors, found := descendStruct(c, segs[k+1:], pass)
					if !found {
						continue
					}
					matches = append(matches, struct {
						col       Column
						src       ScopeSource
						accessors []NestedAccessor
					}{c, src, accessors})
				}
			}
			if len(matches) > 0 {
				break
			}
		}
		switch len(matches) {
		case 1:
			return matches[0].col, matches[0].src, matches[0].accessors, nil
		case 0:
			continue
		default:
			sources := make([]Identifier, 0, len(matches))
			for _, match := range matches {
				sources = append(sources, match.src.Alias)
			}
			return Column{}, ScopeSource{}, nil, &AmbiguousColumnError{
				Id: leaf, Qualifier: qualifier, Path: segs,
				Matches: len(matches), Sources: sources,
			}
		}
	}
	if aliasSeen {
		return Column{}, ScopeSource{}, nil, &ColumnNotFoundError{TableName: firstAliasTable, Id: leaf, Path: append([]Identifier(nil), segs...)}
	}
	all := s.AllSourcesRecursive()
	available := make([]Identifier, 0, len(all))
	for _, src := range all {
		available = append(available, src.Alias)
	}
	return Column{}, ScopeSource{}, nil, &SourceNotFoundError{Alias: qualifier, Available: available, Path: append([]Identifier(nil), segs...)}
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
	// Path is the column reference as written (`Q.ID`) when a column
	// lookup, not a star expansion, missed the qualifier.
	Path []Identifier
}

// Reference renders the reference as written: the column path when a column
// lookup missed its qualifier, else the qualifier alone.
func (e *SourceNotFoundError) Reference() string {
	if len(e.Path) == 0 {
		return e.Alias.Name()
	}
	parts := make([]string, len(e.Path))
	for i, part := range e.Path {
		parts[i] = part.Name()
	}
	return joinStrings(parts, ".")
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
