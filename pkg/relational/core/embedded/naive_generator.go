package embedded

import (
	"context"
	"database/sql/driver"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query"
)

// naiveGenerator is the Phase 1a implementation of query.Generator. It
// parses SQL via the existing parser package, dispatches each parsed
// statement to today's execSelect / execInsert / execUpdate /
// execDelete / execShowStatement / execStatement on
// EmbeddedConnection, and wraps the result in a query.Plan.
//
// This is a seam, not a rewrite — the execution logic stays in
// connection.go; naiveGenerator only introduces the query.Generator /
// Plan indirection so the driver-level code (ExecContext,
// QueryContext) stops calling exec methods directly. Phase 1c moves
// the actual execution bodies out of connection.go behind this seam.
type naiveGenerator struct {
	c *EmbeddedConnection
}

// NewExplainOnlyGenerator constructs a Generator suitable for capturing
// Plan.Explain() output without executing. The returned Generator is
// backed by a zero-value EmbeddedConnection — Plan.Execute on the
// returned plans is unsupported (no FDB, no catalog, no session
// state). Used by the plan-equivalence harness (RFC-022 §4.-1) to
// produce plan trees for diffing against Java's planner output.
//
// Catalog-aware predicate trees (`buildLogicalPlanFor*WithCatalog`
// paths) require non-nil RecordMetaData; this constructor always
// produces text-only logical plans. A future variant taking a synthetic
// schema cache will unlock the catalog-aware branch for the harness.
func NewExplainOnlyGenerator() query.Generator {
	return &naiveGenerator{c: &EmbeddedConnection{}}
}

// Plan parses the SQL and returns a Plan whose Execute dispatches to
// the appropriate exec* method. Multi-statement SQL is wrapped in a
// query.MultiPlan.
//
// Query routing (one-statement SELECT / SHOW vs multi-statement
// DDL/DML) mirrors QueryContext / ExecContext's existing heuristics:
// a single SELECT or SHOW becomes a non-update Plan returning
// driver.Rows; everything else becomes an update Plan returning a
// driver.RowsAffected count.
func (g *naiveGenerator) Plan(ctx context.Context, sql string) (query.Plan, error) {
	root, err := parser.Parse(sql)
	if err != nil {
		return nil, err
	}
	stmts := root.Statements()
	if stmts == nil || len(stmts.AllStatement()) == 0 {
		return &query.PlanFunc{
			ExecFn: func(_ context.Context) (query.Result, error) {
				return query.Result{RowsAffected: 0}, nil
			},
			UpdateFn:  func() bool { return true },
			ExplainFn: func() string { return "empty" },
		}, nil
	}

	all := stmts.AllStatement()
	if len(all) == 1 {
		return g.planOne(all[0])
	}

	// Multi-statement batch: every child must be an update plan
	// (DDL/DML only). Refuse a mixed batch containing SELECT/SHOW
	// to match today's ExecContext, which ignores any Rows-returning
	// result from execStatement and would silently drop it.
	children := make([]query.Plan, 0, len(all))
	for _, s := range all {
		p, err := g.planOne(s)
		if err != nil {
			return nil, err
		}
		if !p.IsUpdate() {
			return nil, api.NewError(api.ErrCodeUnsupportedOperation,
				"multi-statement batches must be DDL/DML only")
		}
		children = append(children, p)
	}
	return &query.MultiPlan{Plans: children}, nil
}

// planOne maps a parsed statement onto a Plan. The exec* dispatch
// mirrors execStatement / QueryContext — any change to statement
// routing must land here and stay in sync with them during Phase 1a.
func (g *naiveGenerator) planOne(stmt antlrgen.IStatementContext) (query.Plan, error) {
	c := g.c

	// SELECT → driver.Rows plan.
	if sel := stmt.SelectStatement(); sel != nil {
		return &query.PlanFunc{
			ExecFn: func(ctx context.Context) (query.Result, error) {
				rows, err := c.execSelect(ctx, sel)
				if err != nil {
					return query.Result{}, err
				}
				return query.Result{Rows: rows}, nil
			},
			UpdateFn: func() bool { return false },
			ExplainFn: func() string {
				// Route through the top-level builder which handles
				// WITH (CTE) + UNION + simple SELECT + JOIN + aggregate
				// + derived. Only SELECT-without-FROM still falls back
				// to canonical SQL text.
				//
				// When the session schema cache already holds the
				// active schema, route through the catalog-aware
				// builder so WHERE clauses become real
				// cascades.QueryPredicate trees in the Explain
				// output. Cold cache → text builder (deterministic
				// fallback, never blocks on a catalog fetch).
				if q := sel.Query(); q != nil {
					md := c.cachedMetaData()
					if md != nil {
						if op := buildLogicalPlanForQueryWithCatalog(q, md); op != nil {
							return op.Explain("")
						}
					}
					if op := buildLogicalPlanForQuery(q); op != nil {
						return op.Explain("")
					}
				}
				return explainStatement("SELECT", sel)
			},
		}, nil
	}

	// SHOW → driver.Rows plan (via admin dispatch).
	if admin := stmt.AdministrationStatement(); admin != nil {
		if show := admin.ShowStatement(); show != nil {
			return &query.PlanFunc{
				ExecFn: func(ctx context.Context) (query.Result, error) {
					rows, err := c.execShowStatement(ctx, show)
					if err != nil {
						return query.Result{}, err
					}
					return query.Result{Rows: rows}, nil
				},
				UpdateFn:  func() bool { return false },
				ExplainFn: func() string { return explainStatement("SHOW", show) },
			}, nil
		}
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"only SHOW administration statements are supported")
	}

	// DDL / DML / transaction → update plan through execStatement.
	// execStatement already contains the per-shape dispatch; we don't
	// duplicate it here.
	return &query.PlanFunc{
		ExecFn: func(ctx context.Context) (query.Result, error) {
			n, err := c.execStatement(ctx, stmt)
			if err != nil {
				return query.Result{}, err
			}
			return query.Result{RowsAffected: n}, nil
		},
		UpdateFn: func() bool { return true },
		ExplainFn: func() string {
			// INSERT / UPDATE / DELETE: emit a real LogicalOperator
			// tree. Other DDL / TX shapes fall back to canonical-SQL
			// text (same Phase 1a placeholder as before).
			//
			// DELETE / UPDATE route through the catalog-aware
			// builder when the schema cache is warm so WHERE
			// clauses become predicate trees instead of source text.
			// Cold cache → text builder (deterministic fallback).
			md := c.cachedMetaData()
			if dml := stmt.DmlStatement(); dml != nil {
				if del := dml.DeleteStatement(); del != nil {
					if md != nil {
						if op := buildLogicalPlanForDeleteWithCatalog(del, md); op != nil {
							return op.Explain("")
						}
					}
					if op := buildLogicalPlanForDelete(del); op != nil {
						return op.Explain("")
					}
				}
				if upd := dml.UpdateStatement(); upd != nil {
					if md != nil {
						if op := buildLogicalPlanForUpdateWithCatalog(upd, md); op != nil {
							return op.Explain("")
						}
					}
					if op := buildLogicalPlanForUpdate(upd); op != nil {
						return op.Explain("")
					}
				}
				if ins := dml.InsertStatement(); ins != nil {
					if md != nil {
						if op := buildLogicalPlanForInsertWithCatalog(ins, md); op != nil {
							return op.Explain("")
						}
					}
					if op := buildLogicalPlanForInsert(ins); op != nil {
						return op.Explain("")
					}
				}
			}
			return explainStatement(statementKind(stmt), stmt)
		},
	}, nil
}

// explainStatement returns a trivial textual description of a parsed
// statement: the kind (SELECT / INSERT / UPDATE / DELETE / DDL / SHOW)
// followed by its source text.
//
// This is the Phase 1a seed of the plan-explain surface; the Cascades
// port will replace it with a structured plan tree for the RFC-022
// §4.-1 plan-equivalence harness to diff against Java's. Today's
// naive Generator has no plan tree — it goes straight from parse
// tree to execution — so the best we can produce without false
// precision is the canonical source text.
func explainStatement(kind string, node interface {
	GetText() string
},
) string {
	txt := ""
	if node != nil {
		txt = node.GetText()
	}
	if txt == "" {
		return kind
	}
	return fmt.Sprintf("%s: %s", kind, txt)
}

// statementKind returns a short human-readable tag for a parsed top-
// level statement. Used only for the Phase 1a Explain surface; once
// Cascades lands, this becomes structural plan-tree rendering.
func statementKind(stmt antlrgen.IStatementContext) string {
	if stmt == nil {
		return "STATEMENT"
	}
	if ddl := stmt.DdlStatement(); ddl != nil {
		return "DDL"
	}
	if dml := stmt.DmlStatement(); dml != nil {
		switch {
		case dml.InsertStatement() != nil:
			return "INSERT"
		case dml.DeleteStatement() != nil:
			return "DELETE"
		case dml.UpdateStatement() != nil:
			return "UPDATE"
		}
		return "DML"
	}
	if stmt.TransactionStatement() != nil {
		return "TX"
	}
	return "STATEMENT"
}

// rowsOrEmpty returns `rows` or a non-nil empty driver.Rows when rows
// is nil. The driver layer expects a non-nil driver.Rows for Query-
// shaped calls.
func rowsOrEmpty(rows driver.Rows) driver.Rows {
	if rows == nil {
		return emptyRows{}
	}
	return rows
}
