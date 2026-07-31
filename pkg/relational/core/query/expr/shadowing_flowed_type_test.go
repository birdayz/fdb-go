package expr_test

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/expr"
	"fdb.dev/pkg/relational/core/query/semantic"
)

// TestShadowingSourceNeverFlowsARow pins the flowed-type decision at the
// RESOLVER BOUNDARY — the return value of each mint — rather than at whatever
// survives into a physical plan.
//
// The distinction is the whole point of this test. A lateral unnest's AS/AT
// binding is a VIRTUAL one-column table in the scope (RFC-142): the column list
// exists so `SELECT "X"` resolves, and it is NOT the row the quantifier flows —
// an element is one array element, Java's isPrimitive() whole-object case. If a
// mint states that virtual row on the quantifier object,
// values.IsMixedSeedElementType (which discriminates an element from a join leg
// by record-ness) reads the element as a leg, and the measured consequences are
// a loud "multi-leg row cannot serve a source-relative ordinal" on some shapes
// and SILENTLY MISSING ROWS on others.
//
// THREE mints build a QuantifiedObjectValue from a ScopeSource, and they are
// reached by SQL that differs by ONE FROM item:
//
//	ResolveIdentifier (correlated arm)     bare `"X"`, multi-source scope
//	ResolveColumnShadowingQualified        bare `"X"` with a LATER FROM item
//	ResolveQualifiedProjection             qualified read on the join path
//
// A guard written at one of them is a guard the other two do not have. That was
// the state this test was written against: only ResolveIdentifier narrowed the
// type, while the other two passed the declared row and cited ResolveIdentifier
// as their authority in a comment. MEASURED at that point — the later-FROM-item
// shape `SELECT A."K","X" FROM A, C, C."ARR" AS "X", U WHERE A."K" = U."UID"`
// routed the element's mint through ResolveColumnShadowingQualified, which
// returned `QOV(X) : RECORD<X UNKNOWN NULL>` and handed it to the Cascades
// translator.
//
// WHEN THIS GOES RED, the repair is in expr.flowedTypeFor (the single
// authority), never at the individual call site — a per-site fix is how the
// three answers diverged in the first place.
func TestShadowingSourceNeverFlowsARow(t *testing.T) {
	t.Parallel()

	// `FROM u AS u, <element> AS x` — a real multi-column source plus a lateral
	// unnest binding, which is what makes the correlated arms reachable at all
	// (a single-source scope resolves childless).
	users := &semantic.StaticTable{
		TableName: semantic.ParseQualifiedName("USERS", false),
		TableColumns: []semantic.Column{
			{Id: semantic.NewUnquoted("id"), Type: "INT"},
			{Id: semantic.NewUnquoted("name"), Type: "STRING", Nullable: true},
		},
	}
	// The virtual one-column table an unnest binding registers — built exactly
	// as unnestVirtualScopeSource builds it.
	elem := &semantic.StaticTable{
		TableName:    semantic.FromSegments([]string{"X"}, false),
		TableColumns: []semantic.Column{{Id: semantic.NewUnquoted("x"), Type: "INT", Nullable: true}},
	}
	cat := semantic.NewInMemoryCatalog(users, elem)
	a := semantic.NewAnalyzer(cat, false)
	s := semantic.NewScope(nil)
	if err := s.AddSource(semantic.ScopeSource{
		Table: users, Alias: semantic.NewUnquoted("u"), CorrelationName: "U",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSource(semantic.ScopeSource{
		Table: elem, Alias: semantic.NewUnquoted("x"), CorrelationName: "X", Shadowing: true,
	}); err != nil {
		t.Fatal(err)
	}
	r := expr.New(a, s)

	qovTypeOf := func(t *testing.T, mint string, v values.Value) values.Type {
		t.Helper()
		fv, ok := v.(*values.FieldValue)
		if !ok {
			t.Fatalf("%s: got %T, want *values.FieldValue", mint, v)
		}
		qov, ok := fv.Child.(*values.QuantifiedObjectValue)
		if !ok {
			t.Fatalf("%s: reference is not QOV-correlated (child %T) — this test no "+
				"longer probes the mint it was written for", mint, fv.Child)
		}
		return qov.Typ
	}
	assertNotARow := func(t *testing.T, mint string, ty values.Type) {
		t.Helper()
		if _, isRow := ty.(*values.RecordType); isRow {
			t.Fatalf("%s minted a quantifier object stating a ROW (%v) for a SHADOWING "+
				"source.\n\n"+
				"  A lateral unnest's AS/AT binding flows ONE array element, not the "+
				"virtual one-column table that makes its name resolve. "+
				"values.IsMixedSeedElementType discriminates an element from a join leg "+
				"by exactly this record-ness, so a row here makes the element read as a "+
				"leg: LOUD \"multi-leg row cannot serve a source-relative ordinal\" on "+
				"some shapes, SILENTLY MISSING ROWS on others.\n\n"+
				"  Fix expr.flowedTypeFor — the single authority all three mints route "+
				"through. Do NOT narrow the type at this call site; three independent "+
				"guards is the shape that produced this bug.", mint, ty)
		}
	}

	// POSITIVE CONTROL. The NON-shadowing source in the same scope must still
	// state its declared row — otherwise every assertion below holds for the
	// uninteresting reason that nothing is typed at all, and the leg-local
	// identity census silently falls back to identityOtherDomain.
	nonShadow, err := r.ResolveIdentifier(semantic.NewUnquoted("u"), semantic.NewUnquoted("name"))
	if err != nil {
		t.Fatalf("resolve U.NAME: %v", err)
	}
	ctlType := qovTypeOf(t, "control: ResolveIdentifier(U.NAME)", nonShadow)
	if _, isRow := ctlType.(*values.RecordType); !isRow {
		t.Fatalf("control: a NON-shadowing source's quantifier object states no ROW (%v). "+
			"The resolver's correlated mint is supposed to carry the source's declared "+
			"row so a leg-correlated read can state its own identity; if that regressed, "+
			"the shadowing assertions below are vacuous.", ctlType)
	}

	// MINT 1 — ResolveIdentifier's correlated arm, reached by a bare `"X"` in a
	// multi-source scope.
	bare, err := r.ResolveIdentifier(semantic.Identifier{}, semantic.NewUnquoted("x"))
	if err != nil {
		t.Fatalf("resolve bare X: %v", err)
	}
	assertNotARow(t, "ResolveIdentifier", qovTypeOf(t, "ResolveIdentifier", bare))

	// MINT 2 — ResolveColumnShadowingQualified, reached by the same bare `"X"`
	// when a LATER FROM item can clobber the bare merged-row key.
	shadowQ, ok, err := r.ResolveColumnShadowingQualified(semantic.Identifier{}, semantic.NewUnquoted("x"))
	if err != nil {
		t.Fatalf("ResolveColumnShadowingQualified: %v", err)
	}
	if !ok {
		t.Fatal("ResolveColumnShadowingQualified declined a SHADOWING source — the " +
			"helper is definitionally shadowing-only, so this test no longer reaches it")
	}
	assertNotARow(t, "ResolveColumnShadowingQualified",
		qovTypeOf(t, "ResolveColumnShadowingQualified", shadowQ))

	// MINT 3 — ResolveQualifiedProjection. It DECLINES (nil, nil) for an alias
	// bound by its own name, which an UNQUOTED unnest binding always is. The
	// decline is not the interesting case; it is the precondition that makes
	// the next one the only probe of this mint, so pin it rather than assume it.
	if v, derr := r.ResolveQualifiedProjection(semantic.NewUnquoted("x"), semantic.NewUnquoted("x")); derr != nil || v != nil {
		t.Fatalf("ResolveQualifiedProjection answered for an UNQUOTED shadowing alias "+
			"(v=%v, err=%v). The decline is what makes the quoted-alias scope below "+
			"the only probe of this mint; if the helper now answers for unquoted "+
			"aliases too, this test must probe BOTH.", v, derr)
	}
	assertQualifiedProjectionMintOnQuotedShadowingAlias(t, assertNotARow, qovTypeOf)
}

// assertQualifiedProjectionMintOnQuotedShadowingAlias builds the scope shape
// that routes a SHADOWING source through ResolveQualifiedProjection.
//
// The route is narrow and worth stating, because it is the reason a guard here
// is not dead code. ResolveQualifiedProjection declines when
// `src.CorrelationName == src.Alias.Name()` and the alias is unique. For a
// shadowing source the alias is ALWAYS unique — Scope.AddSource rejects a
// duplicate involving a shadowing source outright (Java forbids a duplicate
// unnest alias at FROM, RFC-142) — so the decline turns entirely on the two
// names matching. They do not match for a QUOTED LOWERCASE binding
// (`FROM t, t.arr AS "x"`): the parse capture keeps the alias verbatim as `x`,
// while AddSource canonicalizes CorrelationName to the UPPER runtime namespace
// as `X`. The mint is reached, with src.Shadowing true.
//
// So the third mint's guard is load-bearing on exactly one axis — identifier
// QUOTING — which is precisely the axis nobody writes a test for.
func assertQualifiedProjectionMintOnQuotedShadowingAlias(
	t *testing.T,
	assertNotARow func(*testing.T, string, values.Type),
	qovTypeOf func(*testing.T, string, values.Value) values.Type,
) {
	t.Helper()

	// `AS "x"` — FromNormalized wraps the parse capture WITHOUT re-folding, so
	// the alias stays lowercase while AddSource upper-folds the correlation.
	quoted := semantic.FromNormalized("x")
	elem := &semantic.StaticTable{
		TableName:    semantic.FromSegments([]string{"x"}, false),
		TableColumns: []semantic.Column{{Id: quoted, Type: "INT", Nullable: true}},
	}
	other := &semantic.StaticTable{
		TableName:    semantic.ParseQualifiedName("OTHER", false),
		TableColumns: []semantic.Column{{Id: semantic.NewUnquoted("id"), Type: "INT"}},
	}
	a := semantic.NewAnalyzer(semantic.NewInMemoryCatalog(elem, other), false)
	s := semantic.NewScope(nil)
	if err := s.AddSource(semantic.ScopeSource{
		Table: other, Alias: semantic.NewUnquoted("o"), CorrelationName: "O",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSource(semantic.ScopeSource{
		Table: elem, Alias: quoted, CorrelationName: quoted.Name(), Shadowing: true,
	}); err != nil {
		t.Fatal(err)
	}
	r := expr.New(a, s)

	v, err := r.ResolveQualifiedProjection(quoted, quoted)
	if err != nil {
		t.Fatalf("ResolveQualifiedProjection on a quoted shadowing alias: %v", err)
	}
	if v == nil {
		t.Fatal("ResolveQualifiedProjection declined a QUOTED shadowing alias — that " +
			"was the one scope shape reaching this mint with src.Shadowing true, so " +
			"the mint is now unprobed. Either the alias/correlation canonicalization " +
			"changed (find the new reaching shape) or the mint became genuinely " +
			"unreachable for shadowing sources (say so here, and keep the decline " +
			"pinned). Do not delete this assertion.")
	}
	assertNotARow(t, "ResolveQualifiedProjection", qovTypeOf(t, "ResolveQualifiedProjection", v))
}
