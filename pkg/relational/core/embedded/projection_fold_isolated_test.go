package embedded

// Isolation tests for foldConstantProjectionsWith. Per RFC-025
// §"Closing the leaks", projection-fold's routing logic should be
// unit-testable without a real Analyzer + Scope + RecordMetaData +
// demo proto. These tests inject fake ExpressionResolver + fake
// ExpressionFolder so the test boundary stays at "did the pass route
// the right Values into the folder?", not "did the entire catalog
// stack agree?".
//
// Compare to projection_fold_test.go (the original, integration-
// flavoured tests): those use buildTestMetaData() to instantiate a
// real RecordMetaData and exercise the full stack from SQL string
// through to projConstFolded slot. Both styles complement each other
// — the integration tests catch any wiring drift in
// foldConstantProjections's production deps; the isolation tests
// here pin pure routing semantics.

import (
	"errors"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
)

// fakeResolver returns canned (Value, error) results keyed by the
// IExpressionContext pointer. Implements expr.ExpressionResolver.
//
// Two-tier lookup: when a test wires up `results[ctx]`, that result
// wins. Otherwise the resolver falls through to `defaultV` / `defaultErr`
// — useful when a test wants "all unspecified contexts return X" without
// enumerating every key. Today's tests use only the per-context map
// (defaults are implicitly the zero values, which produce a (nil, nil)
// fall-through that causes the harness to skip the slot — a passable
// no-op contract). Add per-default cases when needed.
type fakeResolver struct {
	calls   int
	results map[antlrgen.IExpressionContext]struct {
		v   values.Value
		err error
	}
	defaultV   values.Value
	defaultErr error
}

func (f *fakeResolver) WalkExpression(ctx antlrgen.IExpressionContext) (values.Value, error) {
	f.calls++
	if r, ok := f.results[ctx]; ok {
		return r.v, r.err
	}
	return f.defaultV, f.defaultErr
}

// fakeFolder returns canned (foldedValue, ok) keyed by the input
// Value pointer. Implements values.ExpressionFolder.
type fakeFolder struct {
	calls   int
	results map[values.Value]struct {
		v  any
		ok bool
	}
}

func (f *fakeFolder) Fold(v values.Value) (any, bool) {
	f.calls++
	if r, ok := f.results[v]; ok {
		return r.v, r.ok
	}
	return nil, false
}

// stubExprCtx is the minimum-viable IExpressionContext stand-in for
// the routing tests — it never gets walked by the fake resolver,
// which only checks pointer identity, so the fake doesn't need a
// real ANTLR tree.
type stubExprCtx struct {
	antlrgen.IExpressionContext
}

func newStubExpr() antlrgen.IExpressionContext { return &stubExprCtx{} }

// TestFoldConstantProjectionsWith_NilDeps_NoOp pins the boundary:
// nil resolver, nil folder, nil sq, empty projExprs all short-circuit
// without panicking.
func TestFoldConstantProjectionsWith_NilDeps_NoOp(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		sq       *selectQuery
		resolver *fakeResolver
		folder   *fakeFolder
	}{
		{"nil sq", nil, &fakeResolver{}, &fakeFolder{}},
		{"empty projExprs", &selectQuery{selectClassification: selectClassification{projExprs: nil}}, &fakeResolver{}, &fakeFolder{}},
		{"nil resolver", &selectQuery{selectClassification: selectClassification{projExprs: []antlrgen.IExpressionContext{newStubExpr()}}}, nil, &fakeFolder{}},
		{"nil folder", &selectQuery{selectClassification: selectClassification{projExprs: []antlrgen.IExpressionContext{newStubExpr()}}}, &fakeResolver{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("unexpected panic: %v", r)
				}
			}()
			var resolver *fakeResolver
			if tc.resolver != nil {
				resolver = tc.resolver
			}
			var folder *fakeFolder
			if tc.folder != nil {
				folder = tc.folder
			}
			// Tricky: passing a nil concrete pointer through an interface
			// gives a non-nil interface with a nil dynamic value. The
			// test wants a true-nil interface — pass nil literally.
			switch {
			case resolver == nil && folder == nil:
				foldConstantProjectionsWith(tc.sq, nil, nil)
			case resolver == nil:
				foldConstantProjectionsWith(tc.sq, nil, folder)
			case folder == nil:
				foldConstantProjectionsWith(tc.sq, resolver, nil)
			default:
				foldConstantProjectionsWith(tc.sq, resolver, folder)
			}
		})
	}
}

// TestFoldConstantProjectionsWith_HappyPath pins the success path:
// resolver returns a Value, folder folds it, slot becomes present.
func TestFoldConstantProjectionsWith_HappyPath(t *testing.T) {
	t.Parallel()
	expr1 := newStubExpr()
	v1 := &values.ConstantValue{Value: int64(42), Typ: values.TypeInt}
	resolver := &fakeResolver{
		results: map[antlrgen.IExpressionContext]struct {
			v   values.Value
			err error
		}{expr1: {v: v1}},
	}
	folder := &fakeFolder{
		results: map[values.Value]struct {
			v  any
			ok bool
		}{v1: {v: int64(42), ok: true}},
	}
	sq := &selectQuery{selectClassification: selectClassification{projExprs: []antlrgen.IExpressionContext{expr1}}}
	foldConstantProjectionsWith(sq, resolver, folder)
	if len(sq.projConstFolded) != 1 {
		t.Fatalf("projConstFolded len: got %d, want 1", len(sq.projConstFolded))
	}
	if !sq.projConstFolded[0].present {
		t.Fatalf("expected slot present")
	}
	if sq.projConstFolded[0].value != int64(42) {
		t.Fatalf("value: got %v, want 42", sq.projConstFolded[0].value)
	}
	if resolver.calls != 1 || folder.calls != 1 {
		t.Fatalf("calls: resolver=%d folder=%d, want 1/1", resolver.calls, folder.calls)
	}
}

// TestFoldConstantProjectionsWith_ResolverError_SkipsSlot pins
// best-effort: a resolver error on slot i leaves slot i unset and
// the loop continues to slot i+1.
func TestFoldConstantProjectionsWith_ResolverError_SkipsSlot(t *testing.T) {
	t.Parallel()
	expr1 := newStubExpr()
	expr2 := newStubExpr()
	v2 := &values.ConstantValue{Value: int64(2), Typ: values.TypeInt}
	resolver := &fakeResolver{
		results: map[antlrgen.IExpressionContext]struct {
			v   values.Value
			err error
		}{
			expr1: {err: errors.New("walker decline")},
			expr2: {v: v2},
		},
	}
	folder := &fakeFolder{
		results: map[values.Value]struct {
			v  any
			ok bool
		}{v2: {v: int64(2), ok: true}},
	}
	sq := &selectQuery{selectClassification: selectClassification{projExprs: []antlrgen.IExpressionContext{expr1, expr2}}}
	foldConstantProjectionsWith(sq, resolver, folder)
	if sq.projConstFolded[0].present {
		t.Fatalf("slot 0: should be unset (resolver errored)")
	}
	if !sq.projConstFolded[1].present || sq.projConstFolded[1].value != int64(2) {
		t.Fatalf("slot 1: got %+v", sq.projConstFolded[1])
	}
	if folder.calls != 1 {
		t.Fatalf("folder should be called once (slot 1 only), got %d", folder.calls)
	}
}

// TestFoldConstantProjectionsWith_FolderDecline_SkipsSlot pins that
// when the folder returns ok=false, the slot stays unset — folder
// decline doesn't crash or leave a half-folded slot.
func TestFoldConstantProjectionsWith_FolderDecline_SkipsSlot(t *testing.T) {
	t.Parallel()
	expr1 := newStubExpr()
	v1 := &values.FieldValue{Field: "X", Typ: values.TypeInt}
	resolver := &fakeResolver{
		results: map[antlrgen.IExpressionContext]struct {
			v   values.Value
			err error
		}{expr1: {v: v1}},
	}
	folder := &fakeFolder{
		results: map[values.Value]struct {
			v  any
			ok bool
		}{v1: {ok: false}}, // folder declines (e.g. it's a FieldValue)
	}
	sq := &selectQuery{selectClassification: selectClassification{projExprs: []antlrgen.IExpressionContext{expr1}}}
	foldConstantProjectionsWith(sq, resolver, folder)
	if sq.projConstFolded[0].present {
		t.Fatalf("slot 0: should be unset (folder declined)")
	}
}

// TestFoldConstantProjectionsWith_NilSlotSkipped pins that nil
// projExprs[i] (a plain bare-column projection) is silently skipped.
func TestFoldConstantProjectionsWith_NilSlotSkipped(t *testing.T) {
	t.Parallel()
	expr1 := newStubExpr()
	v1 := &values.ConstantValue{Value: int64(1), Typ: values.TypeInt}
	resolver := &fakeResolver{
		results: map[antlrgen.IExpressionContext]struct {
			v   values.Value
			err error
		}{expr1: {v: v1}},
	}
	folder := &fakeFolder{
		results: map[values.Value]struct {
			v  any
			ok bool
		}{v1: {v: int64(1), ok: true}},
	}
	sq := &selectQuery{selectClassification: selectClassification{projExprs: []antlrgen.IExpressionContext{nil, expr1, nil}}}
	foldConstantProjectionsWith(sq, resolver, folder)
	if sq.projConstFolded[0].present {
		t.Fatalf("slot 0 (nil projExpr): should be unset")
	}
	if !sq.projConstFolded[1].present || sq.projConstFolded[1].value != int64(1) {
		t.Fatalf("slot 1: got %+v", sq.projConstFolded[1])
	}
	if sq.projConstFolded[2].present {
		t.Fatalf("slot 2 (nil projExpr): should be unset")
	}
	if resolver.calls != 1 || folder.calls != 1 {
		t.Fatalf("nil slots should not call deps; got resolver=%d folder=%d, want 1/1",
			resolver.calls, folder.calls)
	}
}

// TestFoldConstantProjectionsWith_AlreadyFolded_Preserved pins the
// idempotency contract: a slot that's already present from a prior
// pass is NOT re-walked or re-folded. Critical because
// execSelectQuery's two dispatchers (full + dispatch) may both run
// the fold pass on the same selectQuery; a non-idempotent
// implementation would either crash or stomp the cached value.
func TestFoldConstantProjectionsWith_AlreadyFolded_Preserved(t *testing.T) {
	t.Parallel()
	expr1 := newStubExpr()
	resolver := &fakeResolver{} // empty — would return (nil, nil) on a call
	folder := &fakeFolder{}
	sq := &selectQuery{
		selectClassification: selectClassification{projExprs: []antlrgen.IExpressionContext{expr1}},
		projConstFolded: []projectionFold{
			{value: "pre-existing", present: true},
		},
	}
	foldConstantProjectionsWith(sq, resolver, folder)
	if sq.projConstFolded[0].value != "pre-existing" {
		t.Fatalf("slot 0: got %v, want pre-existing", sq.projConstFolded[0].value)
	}
	if resolver.calls != 0 || folder.calls != 0 {
		t.Fatalf("already-folded slot must not call deps; got resolver=%d folder=%d, want 0/0",
			resolver.calls, folder.calls)
	}
}

// TestFoldConstantProjectionsWith_GrowsCacheSlice pins the
// slice-growth path: when the caller pre-populated projConstFolded
// with fewer entries than projExprs (rare but possible if a parser
// pass set some slots), the routing pass grows the slice to match,
// preserving the prior entries by copy.
func TestFoldConstantProjectionsWith_GrowsCacheSlice(t *testing.T) {
	t.Parallel()
	expr1 := newStubExpr()
	expr2 := newStubExpr()
	v2 := &values.ConstantValue{Value: int64(99), Typ: values.TypeInt}
	resolver := &fakeResolver{
		results: map[antlrgen.IExpressionContext]struct {
			v   values.Value
			err error
		}{expr2: {v: v2}},
	}
	folder := &fakeFolder{
		results: map[values.Value]struct {
			v  any
			ok bool
		}{v2: {v: int64(99), ok: true}},
	}
	sq := &selectQuery{
		selectClassification: selectClassification{projExprs: []antlrgen.IExpressionContext{expr1, expr2}},
		projConstFolded: []projectionFold{
			{value: "existing", present: true}, // only slot 0 pre-set
		},
	}
	foldConstantProjectionsWith(sq, resolver, folder)
	if len(sq.projConstFolded) != 2 {
		t.Fatalf("projConstFolded should grow to 2, got %d", len(sq.projConstFolded))
	}
	if sq.projConstFolded[0].value != "existing" {
		t.Fatalf("slot 0 (pre-existing) should be preserved")
	}
	if !sq.projConstFolded[1].present || sq.projConstFolded[1].value != int64(99) {
		t.Fatalf("slot 1: got %+v", sq.projConstFolded[1])
	}
}
