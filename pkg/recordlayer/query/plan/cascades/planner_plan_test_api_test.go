package cascades

import (
	"context"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
)

// Plan runs the planning pipeline under context.Background().
//
// It is declared in a test file on purpose. Production code must plan under a
// caller-scoped context so cancellation and deadlines reach task execution;
// making the context-less entry point test-only means no production package in
// any repo can reach it, and the guarantee is enforced by the compiler rather
// than by a source scan that a rename or a method value could evade. In-package
// tests and package cascades_test still see it, which keeps the existing
// planning tests terse.
func (p *Planner) Plan(rootRef *expressions.Reference) (expressions.RelationalExpression, int, error) {
	return p.plan(context.Background(), rootRef)
}
