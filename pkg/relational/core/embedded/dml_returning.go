// Portions derived from FoundationDB Record Layer (QueryVisitor.java),
// Copyright 2021-2026 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package embedded

import (
	"strings"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/parser"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/query"
	"fdb.dev/pkg/relational/core/query/logical"
	"fdb.dev/pkg/relational/core/query/semantic"
	"github.com/antlr4-go/antlr/v4"
)

// dmlReturningSource names the modified rows a RETURNING list reads.
const dmlReturningSource = "$DML_RETURNING"

// returningSelectElements is a DML statement's RETURNING list, or nil. A
// lone `*` is no list: Java's select over it passes the modification's row
// through, so the statement stays an update without a result set.
func returningSelectElements(dml antlrgen.IDmlStatementContext) antlrgen.ISelectElementsContext {
	var elements antlrgen.ISelectElementsContext
	if upd := dml.UpdateStatement(); upd != nil && upd.RETURNING() != nil {
		elements = upd.SelectElements()
	} else if del := dml.DeleteStatement(); del != nil && del.RETURNING() != nil {
		elements = del.SelectElements()
	}
	if elements == nil {
		return nil
	}
	if all := elements.AllSelectElement(); len(all) == 1 {
		if _, star := all[0].(*antlrgen.SelectStarElementContext); star {
			return nil
		}
	}
	return elements
}

// buildReturning is Java's generateSelect over a modification's quantifier
// (QueryVisitor.visitUpdateStatement / visitDeleteStatement): the RETURNING
// list resolves against the modified rows alone, an UPDATE's "old" and "new"
// records or a DELETE's deleted record, which no qualifier names.
func buildReturning(
	dmlOp logical.LogicalOperator,
	elements antlrgen.ISelectElementsContext,
	md *recordlayer.RecordMetaData,
	templateName string,
) (logical.LogicalOperator, error) {
	row, err := query.ExactLogicalResultType(dmlOp, md)
	if err != nil {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedQuery, "modified rows have no exact row type: %v", err)
	}
	producer, err := logical.PrepareCTE(dmlReturningSource, false, logical.CTERegistry{},
		func(logical.CTERegistry) (logical.LogicalOperator, error) { return dmlOp, nil },
		logical.CTENamePath(dmlReturningSource))
	if err != nil {
		return nil, err
	}
	source, ok := virtualScopeSourceFromResultType(dmlReturningSource, dmlOp, md, row, nil, nil)
	if !ok {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, "modified rows have no exact semantic schema")
	}
	source.CTE = producer
	q, err := parser.ParseView(`SELECT 1 FROM "` + dmlReturningSource + `"`)
	if err != nil {
		return nil, err
	}
	if err := graftSelectElements(q, elements); err != nil {
		return nil, err
	}
	visitor := NewPlanVisitorWithTemplate(md, templateName)
	visitor.cteScopes = map[string]semantic.ScopeSource{strings.ToUpper(dmlReturningSource): source}
	visitor.cteProducers = visitor.cteProducers.With(producer)
	main, err := visitor.VisitQuery(q)
	if err != nil {
		return nil, err
	}
	if main == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery, "RETURNING has no logical plan")
	}
	return logical.NewCTEReference(producer, main), nil
}

// graftSelectElements puts the statement's own RETURNING list in place of the
// select list of q, so its parameters keep the bindings the statement's parse
// gave them.
func graftSelectElements(q antlrgen.IQueryContext, elements antlrgen.ISelectElementsContext) error {
	term, ok := q.QueryExpressionBody().(*antlrgen.QueryTermDefaultContext)
	if !ok {
		return api.NewError(api.ErrCodeInternalError, "RETURNING select has no query term")
	}
	table, ok := term.QueryTerm().(*antlrgen.SimpleTableContext)
	if !ok {
		return api.NewError(api.ErrCodeInternalError, "RETURNING select has no simple table")
	}
	children := table.GetChildren()
	for i, child := range children {
		if _, isList := child.(antlrgen.ISelectElementsContext); isList {
			children[i] = elements.(antlr.Tree)
			return nil
		}
	}
	return api.NewError(api.ErrCodeInternalError, "RETURNING select has no select list")
}

// DMLReturnsRows reports whether sql is an UPDATE or DELETE whose RETURNING
// list makes it answer a result set: database/sql has no Statement.execute,
// so a caller must pick Query for it.
func DMLReturnsRows(sql string) bool {
	root, err := parser.Parse(sql)
	if err != nil || root.Statements() == nil {
		return false
	}
	statements := root.Statements().AllStatement()
	if len(statements) != 1 || statements[0].DmlStatement() == nil {
		return false
	}
	return returningSelectElements(statements[0].DmlStatement()) != nil
}
