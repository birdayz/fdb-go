// Portions derived from FoundationDB Record Layer (SemanticAnalyzer.java),
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package embedded

import (
	"sort"

	"fdb.dev/internal/unicode15"
	"fdb.dev/pkg/relational/core/functions"
	"fdb.dev/pkg/relational/core/parser"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/query/expr"
	"github.com/antlr4-go/antlr/v4"
)

// caseSensitiveIdentifiers rewrites every unquoted identifier of sql into its
// quoted form, `Table1` to `"Table1"`. Under the CASE_SENSITIVE_IDENTIFIERS
// connection option Java's normalizeString keeps an unquoted identifier as
// written instead of upper-casing it (SemanticAnalyzer.unquoteOrUpperCase),
// which is exactly what quoting it does, so the rest of the engine needs no
// option of its own. Identifiers are the grammar's `uid` nodes, the only place
// Java normalizes; keywords, literals and built-in function names are not
// uids and are untouched. A statement that does not parse is returned as it
// is, for the ordinary path to report.
func caseSensitiveIdentifiers(sql string, schemaFunction ...func(string) bool) string {
	root, err := parser.Parse(sql)
	if err != nil || root == nil {
		return sql
	}
	type span struct{ start, stop int }
	var spans []span
	var hasFunction func(string) bool
	if len(schemaFunction) > 0 {
		hasFunction = schemaFunction[0]
	}
	var walk func(antlr.Tree)
	walk = func(n antlr.Tree) {
		if template, ok := n.(*antlrgen.CreateSchemaTemplateStatementContext); ok {
			previous := hasFunction
			var declared []string
			hasFunction = func(name string) bool {
				for _, function := range declared {
					if unicode15.EqualFold(function, name) {
						return true
					}
				}
				return false
			}
			// Routines see preceding declarations; views and indexes see all routines.
			// Match DdlVisitor's compilation passes, not textual clause placement.
			for _, clause := range template.AllTemplateClause() {
				if function := clause.SqlInvokedFunction(); function != nil {
					walk(function)
					name := function.FunctionSpecification().GetSchemaQualifiedRoutineName()
					declared = append(declared, functions.NormalizeIdentifier(name.GetText()))
				}
			}
			for i := 0; i < template.GetChildCount(); i++ {
				child := template.GetChild(i)
				if clause, ok := child.(antlrgen.ITemplateClauseContext); ok && clause.SqlInvokedFunction() != nil {
					continue
				}
				walk(child)
			}
			hasFunction = previous
			return
		}
		if u, ok := n.(*antlrgen.UidContext); ok {
			if u.DOUBLE_QUOTE_ID() == nil && u.GetStart() != nil && u.GetStop() != nil {
				spans = append(spans, span{u.GetStart().GetStart(), u.GetStop().GetStop()})
			}
			return
		}
		// Shared built-ins keep case-insensitive spelling. A schema routine
		// sharing a Go extension's name retains normal identifier semantics.
		if f, ok := n.(*antlrgen.UserDefinedScalarFunctionNameContext); ok {
			if id := f.ID(); id != nil {
				name := id.GetText()
				sharedBuiltin := unicode15.EqualFold(name, "CARDINALITY") || unicode15.EqualFold(name, "JAVA_CALL")
				macro := !sharedBuiltin && expr.IsBareNameBuiltin(name) && hasFunction != nil && hasFunction(name)
				if !sharedBuiltin && (!expr.IsBareNameBuiltin(name) || macro) {
					spans = append(spans, span{id.GetSymbol().GetStart(), id.GetSymbol().GetStop()})
				}
			}
			return
		}
		for i := 0; i < n.GetChildCount(); i++ {
			walk(n.GetChild(i))
		}
	}
	walk(root)
	if len(spans) == 0 {
		return sql
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	runes := []rune(sql)
	out := make([]rune, 0, len(runes)+2*len(spans))
	at := 0
	for _, s := range spans {
		if s.start < at || s.stop >= len(runes) || s.stop < s.start {
			return sql
		}
		out = append(out, runes[at:s.start]...)
		out = append(out, '"')
		out = append(out, runes[s.start:s.stop+1]...)
		out = append(out, '"')
		at = s.stop + 1
	}
	out = append(out, runes[at:]...)
	return string(out)
}
