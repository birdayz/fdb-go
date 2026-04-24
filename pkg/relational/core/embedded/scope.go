package embedded

import (
	"database/sql/driver"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Lexical-scope helpers for SQL execution.
//
// Four stacks hang off EmbeddedConnection during query evaluation:
//
//   1. validQualifiers — per-query alias set enforced by the
//      map-path (JOIN) evaluator so `c.name` rejects with 42F01
//      unless `c` is in scope.
//   2. outerScopes — per-subquery stack of (outer msg or row +
//      qualifiers) so correlated references like `EXISTS (SELECT …
//      WHERE x.id = outer_x.id)` resolve past the inner scan.
//   3. currentSourceAliases — the SQL-level aliases of the
//      currently executing outer scan; outerScopeFromMsg copies
//      these into a new scope so inner subqueries expose them.
//   4. (CTE scope lives on c.ctes; pushCTEScope is a sibling
//      helper in connection.go.)
//
// resolveOuterColumn walks the outerScopes stack innermost-first
// to bind a column reference that didn't resolve in the inner
// scope — the SQL semantics of correlated subqueries.
//
// All helpers run on c.* fields that are statement-scoped, so the
// pop-func-returns-restore-prior-state pattern is safe under
// nested defer.

// pushCTEScope replaces c.ctes with a fresh map that inherits the outer
// scope's entries (so inner queries can reference outer CTEs) and returns
// a pop function that restores the previous scope verbatim. Use with
// `defer c.pushCTEScope()()` at every point that introduces new CTE names
// (WITH clauses, derived tables) so inner definitions don't leak out.
func (c *EmbeddedConnection) pushCTEScope() func() {
	prior := c.ctes
	next := make(map[string]*cteData, len(prior))
	for k, v := range prior {
		next[k] = v
	}
	c.ctes = next
	return func() { c.ctes = prior }
}

// pushValidQualifiersScope installs a per-query set of valid qualifier
// aliases (uppercased) and returns a pop function restoring the prior
// scope. Called from execSelectJoin so the map-path evaluator can
// reject WHERE/ON references like `c.name` when no source matches
// `c`. Outside the JOIN scope c.validQualifiers is nil and the
// evaluator preserves the pre-fix silent bare-column fallback — the
// map-path evaluator is JOIN-only so that scope is sufficient.
func (c *EmbeddedConnection) pushValidQualifiersScope(set map[string]bool) func() {
	prior := c.validQualifiers
	c.validQualifiers = set
	return func() { c.validQualifiers = prior }
}

// outerScope is one level of outer-row binding for correlated subqueries.
// At least one of msg / row is non-nil:
//   - msg  : proto-backed outer (single-source SELECT, WHERE call site).
//   - row  : map-backed outer (JOIN / CTE / HAVING / aggregate). Keys are
//     both unqualified (`col`) and qualified (`alias.col`) per
//     scanTableToMaps convention.
//
// qualifiers holds the uppercased set of valid qualifier aliases for this
// outer. A correlated reference `qual.col` matches this scope iff qual is
// in the set. Unqualified `col` falls back through scopes innermost-first
// regardless of qualifiers.
type outerScope struct {
	msg        proto.Message
	row        map[string]driver.Value
	qualifiers map[string]bool
}

// pushOuterScope appends one outer-row scope to the correlation stack and
// returns a pop function that trims it back. Use with `defer` at every
// subquery entry point (EXISTS, IN, scalar subquery) so nested
// correlations stack correctly. Safe to call with a zero-value scope
// (msg == nil && row == nil) — lookups fall through to the next level.
func (c *EmbeddedConnection) pushOuterScope(s outerScope) func() {
	c.outerScopes = append(c.outerScopes, s)
	return func() { c.outerScopes = c.outerScopes[:len(c.outerScopes)-1] }
}

// outerScopeFromMsg builds an outerScope for a proto-backed outer row.
// Qualifier set combines:
//   - the message's descriptor name (always)
//   - any user-level aliases recorded on conn.currentSourceAliases
//     (e.g. `FROM emp AS e` → {"E"} plus the descriptor "EMP")
//
// Returns a zero-value scope when msg is nil so the caller doesn't need
// to nil-check. conn may be nil in unit tests; descriptor name alone
// is sufficient there.
func outerScopeFromMsg(conn *EmbeddedConnection, msg proto.Message) outerScope {
	if msg == nil {
		return outerScope{}
	}
	quals := map[string]bool{
		strings.ToUpper(string(msg.ProtoReflect().Descriptor().Name())): true,
	}
	if conn != nil {
		for a := range conn.currentSourceAliases {
			quals[a] = true
		}
	}
	return outerScope{msg: msg, qualifiers: quals}
}

// pushSourceAliases records the current outer-scan source aliases so
// a subquery's outerScopeFromMsg can expose them to correlated column
// resolution. Pass any SQL-level aliases (e.g. sq.tableAlias and
// sq.tableName) — they're uppercased for case-insensitive match. Returns
// a pop function.
func (c *EmbeddedConnection) pushSourceAliases(aliases ...string) func() {
	prior := c.currentSourceAliases
	m := make(map[string]bool, len(aliases))
	for _, a := range aliases {
		if a == "" {
			continue
		}
		m[strings.ToUpper(a)] = true
	}
	c.currentSourceAliases = m
	return func() { c.currentSourceAliases = prior }
}

// outerScopeFromMapRow builds an outerScope for a map-backed outer row
// (JOIN / CTE / HAVING aggregate). qualifiers is derived from every
// qualified key in the row: for each key of the form `alias.col`, the
// prefix is added (uppercased) to the qualifier set. Returns a zero-
// value scope for a nil/empty row.
func outerScopeFromMapRow(row map[string]driver.Value) outerScope {
	if len(row) == 0 {
		return outerScope{}
	}
	quals := make(map[string]bool)
	for k := range row {
		if dot := strings.LastIndex(k, "."); dot >= 0 {
			quals[strings.ToUpper(k[:dot])] = true
		}
	}
	return outerScope{row: row, qualifiers: quals}
}

// outerScopesContainQualifier reports whether any outer scope on the
// stack declares qualUpper as a valid qualifier alias. Used by the
// map-path evaluator to let correlated `outer.col` references bypass
// the JOIN-scope valid-qualifier reject before falling through to
// resolveOuterColumn.
func outerScopesContainQualifier(c *EmbeddedConnection, qualUpper string) bool {
	for _, s := range c.outerScopes {
		if s.qualifiers[qualUpper] {
			return true
		}
	}
	return false
}

// resolveOuterColumn walks the outer-scope stack innermost-first trying
// to resolve a column reference that was not found in the inner scope.
// Returns (value, found, err).
//
// Qualified `qual.col`: only scopes whose qualifiers set contains qual
// are consulted. A qualified reference binds to exactly one source per
// SQL semantics, so when a scope's qualifier matches but the bare
// column is missing, resolution stops with 42703 — we do NOT continue
// to the next outer scope (another scope with the same qualifier name
// would be a shadowing violation at the SQL level).
//
// Unqualified `col`: every scope is tried in order; first match wins.
// Identifier case is preserved verbatim from the AST; if a GROUP BY
// clause and a correlated reference use different casing, the lookup
// will miss (matches the rest of this evaluator's case-sensitive
// column semantics).
func (c *EmbeddedConnection) resolveOuterColumn(colName string) (driver.Value, bool, error) {
	qual := ""
	bare := colName
	if dot := strings.LastIndex(colName, "."); dot >= 0 {
		qual = strings.ToUpper(colName[:dot])
		bare = colName[dot+1:]
	}
	for i := len(c.outerScopes) - 1; i >= 0; i-- {
		s := c.outerScopes[i]
		if qual != "" && !s.qualifiers[qual] {
			continue
		}
		switch {
		case s.msg != nil:
			fd := s.msg.ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(bare))
			if fd == nil {
				if qual != "" {
					return nil, false, api.NewErrorf(api.ErrCodeUndefinedColumn,
						"column %q not found in correlated source %q", bare, qual)
				}
				continue
			}
			if !s.msg.ProtoReflect().Has(fd) {
				return nil, true, nil
			}
			return functions.ProtoValueToDriver(fd, s.msg.ProtoReflect().Get(fd)), true, nil
		case s.row != nil:
			if qual != "" {
				// Row keys preserve the SQL-level alias case (e.g. `e.id`
				// when the outer wrote `FROM emp AS e`); the qualifier
				// set and lookup qual are uppercased. Do a case-
				// insensitive prefix match so `E.id` → `e.id`.
				for k, v := range s.row {
					dot := strings.LastIndex(k, ".")
					if dot < 0 {
						continue
					}
					if strings.EqualFold(k[:dot], qual) && k[dot+1:] == bare {
						if _, isAmb := v.(ambiguousColumnMarker); isAmb {
							return nil, false, api.NewErrorf(api.ErrCodeAmbiguousColumn,
								"correlated column reference %q is ambiguous", colName)
						}
						return v, true, nil
					}
				}
				return nil, false, api.NewErrorf(api.ErrCodeUndefinedColumn,
					"column %q not found in correlated source %q", bare, qual)
			}
			if v, ok := s.row[bare]; ok {
				if _, isAmb := v.(ambiguousColumnMarker); isAmb {
					return nil, false, api.NewErrorf(api.ErrCodeAmbiguousColumn,
						"correlated column reference %q is ambiguous", bare)
				}
				return v, true, nil
			}
		}
	}
	return nil, false, nil
}
