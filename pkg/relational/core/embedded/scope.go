package embedded

import (
	"database/sql/driver"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Lexical-scope read helpers for the shared map/proto expression
// evaluators (eval_map.go, eval_proto.go).
//
// Two EmbeddedConnection fields back correlated-reference resolution:
//
//   1. validQualifiers — per-query alias set the map-path evaluator
//      consults so `c.name` rejects unless `c` is in scope.
//   2. outerScopes — per-subquery stack of (outer msg or row +
//      qualifiers) so correlated references like `EXISTS (SELECT …
//      WHERE x.id = outer_x.id)` resolve past the inner scan.
//
// resolveOuterColumn walks the outerScopes stack innermost-first to
// bind a column reference that didn't resolve in the inner scope —
// the SQL semantics of correlated subqueries.
//
// RFC-145 note: the executor that pushed these scopes (the legacy
// embedded interpreter) is gone. These read helpers + the outerScope
// type are retained because the shared evaluators still reference the
// fields; the kept consumers (INFORMATION_SCHEMA WHERE, INSERT-VALUES
// folding) never populate the stacks, so the reads fall through.

// outerScope is one level of outer-row binding for correlated subqueries.
// At least one of msg / row is non-nil:
//   - msg  : proto-backed outer (single-source SELECT, WHERE call site).
//   - row  : map-backed outer (JOIN / CTE / HAVING / aggregate). Keys are
//     both unqualified (`col`) and qualified (`alias.col`) by convention.
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
	ref := parseColRef(colName)
	qual := strings.ToUpper(ref.table)
	bare := ref.bare()
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
					kr := parseColRef(k)
					if !kr.isQualified() {
						continue
					}
					if strings.EqualFold(kr.table, qual) && kr.bare() == bare {
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
