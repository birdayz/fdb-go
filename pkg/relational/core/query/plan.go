// Package query defines the planner/plan seam between the SQL frontend
// (database/sql driver, future gRPC server, REPL) and the SQL execution
// engine. Mirrors the role of Java's
// fdb-relational-core/recordlayer/query/Plan.java +
// AbstractEmbeddedStatement.executeInternal's 40-line dispatch.
//
// A frontend holds a Generator (typically one per connection/session).
// For every SQL string:
//
//	plan, err := gen.Plan(ctx, sql)   // parse + analyze + plan
//	result, err := plan.Execute(ctx)  // run against the bound session
//
// No frontend code touches the execution engine directly. All backend
// shapes live behind Generator + Plan and swap without changing callers.
//
// Introduced in RFC 021 to let a Cascades Generator be swapped in behind
// this boundary. That migration is complete: Cascades is the sole
// Generator for queries and DML, and the original naive per-shape
// executor was removed in RFC-145 (only `execStatement`, for DDL, remains).
package query

import (
	"context"
	"database/sql/driver"
	"strings"
)

// Generator builds executable Plans from SQL strings. One Generator
// per logical session — it carries the session's catalog handle,
// current schema, and options.
type Generator interface {
	// Plan parses, semantically analyzes, and plans a SQL string.
	// Errors are typed via pkg/relational/api — callers should not
	// need to wrap them.
	Plan(ctx context.Context, sql string) (Plan, error)
}

// Plan is a ready-to-execute representation of a SQL statement. One
// Plan per SQL statement; a multi-statement SQL text produces a
// MultiPlan (see below).
//
// Execute returns a Result whose concrete shape depends on the
// statement kind:
//   - SELECT / SHOW / read-only: Result.Rows is non-nil; Result.
//     RowsAffected is zero.
//   - INSERT / UPDATE / DELETE: Result.RowsAffected counts the
//     modified rows; Result.Rows is nil.
//   - DDL (CREATE / DROP): Result.Rows is nil; Result.RowsAffected
//     is zero.
//
// IsUpdate distinguishes mutation plans so the driver knows whether
// to return driver.Rows vs driver.Result at the boundary. Matches
// Java's Plan.isUpdatePlan().
//
// Explain returns a textual description of the plan. Today the naive
// Generator returns the canonical SQL text; future Cascades plans
// will return a plan tree that is stable enough for the RFC-022 §4.-1
// plan-equivalence harness to diff against Java's. Empty string is a
// valid value for plans that cannot produce a useful description.
type Plan interface {
	Execute(ctx context.Context) (Result, error)
	IsUpdate() bool
	Explain() string
}

// Result is the output of a Plan execution. Exactly one of Rows /
// RowsAffected carries the payload; which one is set is determined
// by Plan.IsUpdate().
type Result struct {
	// Rows is non-nil for SELECT-shaped plans. Nil for DML/DDL.
	Rows driver.Rows
	// RowsAffected is the update count for DML. Zero for SELECT/DDL.
	RowsAffected int64
}

// MultiPlan wraps a sequence of Plans produced from a multi-statement
// SQL text (semicolon-separated). Execute runs them in order and
// returns a Result holding the SUM of RowsAffected (for Exec-style
// callers); the last Plan's Rows if any are exposed via Results().
// This matches today's EmbeddedConnection.ExecContext aggregation:
// total modified rows across the batch, no intermediate result sets
// bubbled up.
type MultiPlan struct {
	Plans []Plan
}

// Execute runs every child Plan in order, short-circuiting on the
// first error. Returns the aggregate RowsAffected; Rows is nil
// (multi-statement Exec doesn't expose intermediate row sets).
func (m *MultiPlan) Execute(ctx context.Context) (Result, error) {
	var total int64
	for _, p := range m.Plans {
		r, err := p.Execute(ctx)
		if err != nil {
			return Result{}, err
		}
		total += r.RowsAffected
	}
	return Result{RowsAffected: total}, nil
}

// IsUpdate returns true iff every child Plan is an update plan. A
// mixed batch (e.g. DDL + SELECT) is treated as non-update so the
// last Rows-producing plan would be reachable via a future Results()
// iteration. In practice today's driver doesn't send mixed batches
// through Exec.
func (m *MultiPlan) IsUpdate() bool {
	for _, p := range m.Plans {
		if !p.IsUpdate() {
			return false
		}
	}
	return true
}

// Explain returns every child's explanation joined by ';\n'.
func (m *MultiPlan) Explain() string {
	var b strings.Builder
	for i, p := range m.Plans {
		if i > 0 {
			b.WriteString(";\n")
		}
		b.WriteString(p.Explain())
	}
	return b.String()
}

// PlanFunc is a convenience adapter: a Plan whose Execute delegates to a
// closure. Used to wrap non-Cascades code paths (e.g. the executor-free
// INFORMATION_SCHEMA system-table handler and the explain-only renderer)
// as Plan implementations without duplicating logic.
//
// Post-Phase-1c this adapter becomes obsolete — physical operator
// types implement Plan directly. Kept here during the transition so
// the frontend seam is stable before the executor is split.
type PlanFunc struct {
	ExecFn    func(ctx context.Context) (Result, error)
	UpdateFn  func() bool
	ExplainFn func() string
}

// Execute runs the wrapped closure.
func (p *PlanFunc) Execute(ctx context.Context) (Result, error) {
	return p.ExecFn(ctx)
}

// IsUpdate returns UpdateFn's result, or false when UpdateFn is nil.
func (p *PlanFunc) IsUpdate() bool {
	if p.UpdateFn == nil {
		return false
	}
	return p.UpdateFn()
}

// Explain returns ExplainFn's result, or empty when ExplainFn is nil.
func (p *PlanFunc) Explain() string {
	if p.ExplainFn == nil {
		return ""
	}
	return p.ExplainFn()
}
