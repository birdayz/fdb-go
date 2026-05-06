package embedded

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/metadata"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/session"
)

// startsWithCreateSchemaTemplate reports whether ddl begins (after
// leading whitespace) with the case-insensitive "CREATE SCHEMA
// TEMPLATE" header. Used to decide whether buildSchemaTemplateFromDDL
// must auto-wrap a bare body.
func startsWithCreateSchemaTemplate(ddl string) bool {
	t := strings.TrimSpace(ddl)
	if len(t) < len("CREATE SCHEMA TEMPLATE") {
		return false
	}
	return strings.EqualFold(t[:len("CREATE SCHEMA TEMPLATE")], "CREATE SCHEMA TEMPLATE")
}

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
// produces text-only logical plans. Use NewExplainOnlyGeneratorWithSchema
// to unlock the catalog-aware branch.
func NewExplainOnlyGenerator() query.Generator {
	return &naiveGenerator{c: &EmbeddedConnection{}}
}

// NewExplainOnlyGeneratorWithSchema is the catalog-aware companion to
// NewExplainOnlyGenerator. It parses the supplied CREATE SCHEMA
// TEMPLATE DDL into an in-memory RecordLayerSchemaTemplate (no FDB
// write), wraps it in an api.Schema bound to a synthetic database +
// schema, and seeds the connection's SchemaCache. Subsequent
// statements planned through the returned Generator route through the
// `buildLogicalPlanFor*WithCatalog` paths so WHERE clauses appear as
// real cascades.predicates.QueryPredicate trees in the Explain output.
//
// schemaDDL must contain exactly one CREATE SCHEMA TEMPLATE
// statement. Multiple-statement DDL or any non-CREATE-SCHEMA-TEMPLATE
// shape returns an error — callers should isolate the schema DDL from
// the SELECT/DML they intend to plan.
//
// Per RFC-022 §4.-1 Phase 3 — catalog-aware mode for the plan-
// equivalence harness's Go side.
func NewExplainOnlyGeneratorWithSchema(schemaDDL string) (query.Generator, error) {
	tmpl, err := buildSchemaTemplateFromDDL(schemaDDL)
	if err != nil {
		return nil, err
	}
	const dbPath = "/explain"
	const schemaName = "s"
	sess := &session.Session{
		DBPath: dbPath,
		Schema: schemaName,
		SchemaCache: map[string]api.Schema{
			session.SchemaCacheKey(dbPath, schemaName): tmpl.GenerateSchema(dbPath, schemaName),
		},
	}
	return &naiveGenerator{c: &EmbeddedConnection{sess: sess}}, nil
}

// buildSchemaTemplateFromDDL parses schemaDDL as a single
// CREATE SCHEMA TEMPLATE statement and builds a
// RecordLayerSchemaTemplate without performing any catalog write.
// Mirrors the in-memory portion of execCreateSchemaTemplate (the
// parse + builder population). The Factory.SaveSchemaTemplate step
// is deliberately skipped — this path is for the explain-only
// harness, which has no FDB to write to.
//
// Bare bodies (a sequence of CREATE TABLE / CREATE INDEX clauses
// without the surrounding `CREATE SCHEMA TEMPLATE <name>` header)
// are accepted and auto-wrapped, matching the corpus shape used by
// the conformance harness's Java side
// (conformance/sql_plan_steps.java#planSql wraps the same body).
func buildSchemaTemplateFromDDL(schemaDDL string) (*metadata.RecordLayerSchemaTemplate, error) {
	wrapped := schemaDDL
	if !startsWithCreateSchemaTemplate(schemaDDL) {
		wrapped = "CREATE SCHEMA TEMPLATE auto_template " + schemaDDL
	}
	root, err := parser.Parse(wrapped)
	if err != nil {
		return nil, fmt.Errorf("parse schema DDL: %w", err)
	}
	stmts := root.Statements()
	if stmts == nil {
		return nil, fmt.Errorf("schema DDL must contain exactly one statement, got 0")
	}
	if len(stmts.AllStatement()) != 1 {
		return nil, fmt.Errorf("schema DDL must contain exactly one statement, got %d",
			len(stmts.AllStatement()))
	}
	create := stmts.AllStatement()[0].DdlStatement()
	if create == nil {
		return nil, fmt.Errorf("schema DDL must be a CREATE SCHEMA TEMPLATE statement")
	}
	cs := create.CreateStatement()
	if cs == nil {
		return nil, fmt.Errorf("schema DDL must be a CREATE SCHEMA TEMPLATE statement")
	}
	stCtx, ok := cs.(*antlrgen.CreateSchemaTemplateStatementContext)
	if !ok {
		return nil, fmt.Errorf("schema DDL must be a CREATE SCHEMA TEMPLATE statement, got %T", cs)
	}

	templateID := stCtx.SchemaTemplateId().GetText()
	b := metadata.NewSchemaTemplateBuilder().SetName(templateID)
	for _, clause := range stCtx.AllTemplateClause() {
		td := clause.TableDefinition()
		if td == nil {
			continue
		}
		tableName := td.Uid().GetText()
		cols, pkCols, err := parseTableDefinition(td)
		if err != nil {
			return nil, fmt.Errorf("table %q: %w", tableName, err)
		}
		b.AddTable(tableName, cols, pkCols)
	}
	for _, clause := range stCtx.AllTemplateClause() {
		idxDef := clause.IndexDefinition()
		if idxDef == nil {
			continue
		}
		if err := parseIndexDefinition(idxDef, b); err != nil {
			return nil, fmt.Errorf("index: %w", err)
		}
	}
	return b.Build()
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

	// EXPLAIN <inner> → driver.Rows plan with a single PLAN column.
	// The inner query/DML is rendered via the existing
	// buildLogicalPlanFor* path; no execution against FDB happens.
	// Mirrors fdb-relational's `EXPLAIN <sql>` shape so the harness
	// + user-facing `EXPLAIN` produce comparable output.
	if util := stmt.UtilityStatement(); util != nil {
		if full := util.FullDescribeStatement(); full != nil {
			return g.planExplain(full)
		}
	}

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
				// cascades.predicates.QueryPredicate trees in the Explain
				// output. Cold cache → text builder (deterministic
				// fallback, never blocks on a catalog fetch).
				if q := sel.Query(); q != nil {
					md := c.cachedMetaData()
					if md != nil {
						if op, err := buildLogicalPlanForQueryWithCatalog(q, md); err == nil && op != nil {
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

// planExplain handles `EXPLAIN <query|delete|insert|update>` — the
// fullDescribeStatement form. Returns a Plan whose Execute produces
// a single-row driver.Rows with column "PLAN" carrying the
// rendered logical-operator tree (or canonical SQL text on
// builder fallback).
//
// Mirrors fdb-relational's PLAN column shape so the plan-equivalence
// harness can diff Go-side EXPLAIN output against Java's.
//
// Format specifiers — the grammar allows `EXPLAIN FORMAT = JSON …`
// / `EXPLAIN EXTENDED = TRADITIONAL …` etc., but this seed silently
// ignores them: the inner plan tree always renders as text. Future
// support for FORMAT=JSON / FORMAT=DOT / EXTENDED would extend this
// path; tooling that requires structured output should not depend
// on the format request being honoured today.
//
// Decline modes (each returns UNSUPPORTED_OPERATION):
//   - `EXPLAIN FOR CONNECTION uid` → `*DescribeConnectionContext`,
//     not a `*DescribeStatementsContext` → exits via the `!ok` arm
//     below. The seed has no connection-identifier surface.
//   - `EXPLAIN EXECUTE CONTINUATION …` → IS a `*DescribeStatementsContext`
//     (the grammar's #describeStatements alternative includes
//     executeContinuationStatement), so it passes the `ok` check
//     but every accessor (Query / DeleteStatement / InsertStatement
//     / UpdateStatement) returns nil → computeExplainText returns
//     "" → the `planText == ""` guard fires.
func (g *naiveGenerator) planExplain(full antlrgen.IFullDescribeStatementContext) (query.Plan, error) {
	objClause := full.DescribeObjectClause()
	if objClause == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"EXPLAIN requires an inner statement")
	}
	descStmts, ok := objClause.(*antlrgen.DescribeStatementsContext)
	if !ok {
		// EXPLAIN FOR CONNECTION lands here (it's a
		// *DescribeConnectionContext, not *DescribeStatementsContext).
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"EXPLAIN form not supported (only EXPLAIN <query|insert|update|delete>)")
	}
	planText := g.computeExplainText(descStmts)
	if planText == "" {
		// `EXPLAIN EXECUTE CONTINUATION …` reaches here — it parses
		// as a *DescribeStatementsContext but none of the
		// query/delete/insert/update accessors return non-nil for it.
		// Future extensions (CTAS-style EXPLAIN, etc.) that don't
		// produce a plan tree also surface here.
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"EXPLAIN inner statement produced no plan text")
	}
	return &query.PlanFunc{
		ExecFn: func(_ context.Context) (query.Result, error) {
			return query.Result{Rows: &staticRows{
				cols: []string{"PLAN"},
				rows: [][]driver.Value{{planText}},
			}}, nil
		},
		UpdateFn:  func() bool { return false },
		ExplainFn: func() string { return "EXPLAIN: " + planText },
	}, nil
}

// computeExplainText builds the plan-tree text for the inner
// statement of an EXPLAIN. Routes through the catalog-aware builder
// when the schema cache is warm (so WHERE clauses become
// cascades.predicates.QueryPredicate trees) and falls back to the text builder
// otherwise.
func (g *naiveGenerator) computeExplainText(d *antlrgen.DescribeStatementsContext) string {
	c := g.c
	md := c.cachedMetaData()
	if q := d.Query(); q != nil {
		if md != nil {
			if op, err := buildLogicalPlanForQueryWithCatalog(q, md); err == nil && op != nil {
				return op.Explain("")
			}
		}
		if op := buildLogicalPlanForQuery(q); op != nil {
			return op.Explain("")
		}
	}
	if del := d.DeleteStatement(); del != nil {
		if md != nil {
			if op := buildLogicalPlanForDeleteWithCatalog(del, md); op != nil {
				return op.Explain("")
			}
		}
		if op := buildLogicalPlanForDelete(del); op != nil {
			return op.Explain("")
		}
	}
	if ins := d.InsertStatement(); ins != nil {
		if md != nil {
			if op := buildLogicalPlanForInsertWithCatalog(ins, md); op != nil {
				return op.Explain("")
			}
		}
		if op := buildLogicalPlanForInsert(ins); op != nil {
			return op.Explain("")
		}
	}
	if upd := d.UpdateStatement(); upd != nil {
		if md != nil {
			if op := buildLogicalPlanForUpdateWithCatalog(upd, md); op != nil {
				return op.Explain("")
			}
		}
		if op := buildLogicalPlanForUpdate(upd); op != nil {
			return op.Explain("")
		}
	}
	return ""
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
