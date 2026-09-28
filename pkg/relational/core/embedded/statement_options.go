package embedded

import (
	"fdb.dev/pkg/relational/api"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"github.com/antlr4-go/antlr/v4"
)

// statementOptions is a statement's `OPTIONS (...)` clause (Java's
// statementOptions). LOG QUERY is parsed and, like the LOG_QUERY connection
// option, not consumed: Go's planning-metrics hook always emits a record.
type statementOptions struct {
	noCache   bool // bypass the plan cache: neither read nor populate it
	dryRun    bool // DML previews its mutation instead of committing it
	rightDeep bool // plan with join enumeration restricted to right-deep trees
	snapshot  bool // execute the statement's reads at snapshot isolation
}

// parseStatementOptions collects the OPTIONS clauses anywhere under tree. The
// grammar admits the clause only at statement level, so a walk finds exactly
// the statement's own options (for INSERT … SELECT, the insert's).
func parseStatementOptions(tree antlr.Tree) statementOptions {
	var so statementOptions
	var walk func(antlr.Tree)
	walk = func(n antlr.Tree) {
		if n == nil {
			return
		}
		if opt, ok := n.(*antlrgen.StatementOptionContext); ok {
			switch {
			case opt.NOCACHE() != nil:
				so.noCache = true
			case opt.DRY() != nil:
				so.dryRun = true
			case opt.PLAN() != nil:
				so.rightDeep = true
			case opt.SNAPSHOT() != nil:
				so.snapshot = true
			}
			return
		}
		for i := 0; i < n.GetChildCount(); i++ {
			walk(n.GetChild(i))
		}
	}
	walk(tree)
	return so
}

// statementOptionsFor merges a statement's OPTIONS clause with the
// connection's options, as Java's PlanGenerator merges them (:170): DRY_RUN
// and ISOLATION_LEVEL_SNAPSHOT set on the connection apply to every statement.
// (PLAN_RIGHT_DEEP on the connection reaches the planner through
// plannerOptionsFrom.)
func statementOptionsFor(tree antlr.Tree, opts *api.Options) statementOptions {
	so := parseStatementOptions(tree)
	so.dryRun = so.dryRun || optBool(opts, api.OptDryRun, false)
	so.snapshot = so.snapshot || optBool(opts, api.OptIsolationLevelSnapshot, false)
	return so
}

// errSnapshotOnlySelect is Java's validateIsolationLevelSnapshotOption refusal
// (PlanGenerator.java:507-517): snapshot isolation is for reads only.
func errSnapshotOnlySelect() error {
	return api.NewError(api.ErrCodeUnsupportedOperation,
		"OPTIONS (ISOLATION LEVEL SNAPSHOT) is only supported on SELECT queries")
}
