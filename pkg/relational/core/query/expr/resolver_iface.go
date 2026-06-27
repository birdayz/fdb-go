package expr

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
)

// ExpressionResolver is the interface a parse-tree → values.Value
// walker exposes. *Resolver satisfies it; tests can swap in a fake
// that returns canned Values without standing up a real Analyzer +
// Scope + FunctionCatalog.
//
// Per RFC-025 §"Closing the leaks": the embedded layer's projection
// fold pass needs a way to walk an ANTLR IExpressionContext to a
// values.Value at plan time. Today it instantiates a real Resolver
// inline (see embedded/projection_fold.go) which forces every test to
// set up RecordMetaData + a demo proto schema + an Analyzer + a Scope
// just to verify routing logic. With this interface, callers can
// inject a fake walker — the test boundary stays at "did the fold
// pass route the right Values into the folder?", not "did the entire
// catalog stack agree?".
//
// Contract: WalkExpression returns (values.Value, nil) on a
// supported shape, (nil, error) on an unsupported shape (typically
// `*UnsupportedExpressionShapeError`). The caller filters on error
// type if it cares to distinguish "decline" from "error".
type ExpressionResolver interface {
	WalkExpression(ctx antlrgen.IExpressionContext) (values.Value, error)
}

// Static interface check: the production Resolver implements
// ExpressionResolver. If a future Resolver method-rename breaks this,
// the build catches it at compile time rather than at test time.
var _ ExpressionResolver = (*Resolver)(nil)
