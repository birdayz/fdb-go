package query

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/query/logical"
)

// A lateral UNNEST's Explode must read its array through an ORDINAL bake over
// the quantifier that owns it. The alternative the lowering used to keep — a
// lazy `FieldValue{Field: "SEG0.COL"}` over the merged outer row — is the
// qualified-name channel (RFC-197 item 6): the owning leg packed into a string
// because a bare read over a multi-namespace row is ambiguous (last-leg-wins,
// and the winner follows an operand order the planner may swap).
//
// A string qualifier is not a fix for that ambiguity, it relocates it. Nothing
// downstream can tell the packed qualifier from a leaf name that legitimately
// contains a dot, so the dependency on the owning leg had to be recovered by
// slicing the prefix back off and treating it as a correlation identifier — a
// quantifier's identity decided by text, which loses the edge when the leg is
// addressed by a minted binding and invents one when a sibling shares the alias.
//
// The bake states the same fact as the leg's own WINDOW OFFSET, so no qualifier
// needs encoding and no recovery needs to run. TestGatheredExplodeOwnerEdge…
// (pkg/recordlayer/query/plan/cascades) is the consumer-side twin: it runs the
// two rules that read this edge and pins that nothing reconstructs it from a
// name.
//
// This test walks the lowering's OWN output, so it fails the moment a
// name-model collection is reintroduced — a plan-level row assertion cannot,
// because both forms yield the same rows until the operand order swaps.

// unnestArm is one routing case: which lowering arm handles the shape, and what
// the collection must come out as.
type unnestArm struct {
	name string
	// underExistential drives the lowering's routing switch. false is the
	// PRODUCTION DEFAULT (translateGatheredUnnestCluster owns a box outer);
	// true is the under-EXISTS path, which declines the gather and takes
	// translateUnnestJoin's own ordinal seed.
	underExistential bool
	outer            logical.LogicalOperator
	segments         []string
	// wantOwner is the sibling quantifier alias the collection must correlate
	// to. It also PINS THE ROUTING: the gathered arm mints a `$BOX`-suffixed
	// quantifier, the non-gathered arm keeps the plain source alias, so a
	// silent re-route between them shows up here.
	wantOwner string
	// wantRootOrdinal is the array column's offset in the row the collection
	// reads — the leg's own window position, which is the fact the `SEG0.`
	// prefix used to carry. A multi-namespace outer with the owner in SECOND
	// position must NOT answer the same ordinal as the single-source arm, or
	// the arm proves nothing the single-source arm did not already prove.
	wantRootOrdinal int
}

func TestExplodeCollectionsAreOrdinalBaked(t *testing.T) {
	t.Parallel()

	// Owner FIRST: TAGS is at ordinal 3 of Order's own 8-column row, and at
	// ordinal 3 of the merged row too — so this box shape alone cannot tell an
	// owner-window bake from a lucky coincidence.
	boxOwnerFirst := logical.NewJoin(
		logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinInner, ""),
		scan("TypedRecord", "d"), logical.JoinFull, "")
	// Owner SECOND: Customer's four columns precede Order's, so the same array
	// sits at ordinal 7 of the merged row. This is the shape that distinguishes
	// the two, and it is the one the `O.` qualifier existed to disambiguate.
	boxOwnerSecond := logical.NewJoin(
		logical.NewJoin(scan("Customer", "c"), scan("Order", "o"), logical.JoinInner, ""),
		scan("TypedRecord", "d"), logical.JoinFull, "")

	for _, tc := range []unnestArm{
		// Single namespace: a bare read WOULD have been unambiguous here, so
		// this arm pins that the ordinal bake is unconditional rather than a
		// multi-source special case.
		{
			name:  "single source, production default",
			outer: scan("Order", "o"), segments: []string{"o", "TAGS"},
			wantOwner: "O", wantRootOrdinal: 3,
		},
		{
			name: "single source, under EXISTS", underExistential: true,
			outer: scan("Order", "o"), segments: []string{"o", "TAGS"},
			wantOwner: "O", wantRootOrdinal: 3,
		},
		// MULTI-namespace INNER cluster — `FROM O, C, O.TAGS AS X`. The outer
		// row carries both sources, so the qualifier was the only thing
		// distinguishing the array; the bake reads Order's OWN row instead, so
		// the ordinal is 3 of 8 and the owner is O, not the merged flow leg.
		{
			name:  "multi-source INNER cluster, production default",
			outer: inner(scan("Order", "o"), scan("Customer", "c")), segments: []string{"o", "TAGS"},
			wantOwner: "O", wantRootOrdinal: 3,
		},
		// FULL-outer box. At the production default this routes through
		// translateGatheredUnnestCluster (the `$BOX` owner); under EXISTS the
		// gather declines and translateUnnestJoin's own seed handles it. Both
		// arms are covered because both are reachable, and they mint DIFFERENT
		// owner quantifiers.
		{
			name:  "clustered FULL box, owner first, production default",
			outer: boxOwnerFirst, segments: []string{"o", "TAGS"},
			wantOwner: "D$BOX", wantRootOrdinal: 3,
		},
		{
			name:  "clustered FULL box, owner SECOND, production default",
			outer: boxOwnerSecond, segments: []string{"o", "TAGS"},
			wantOwner: "D$BOX", wantRootOrdinal: 7,
		},
		{
			name: "clustered FULL box, owner SECOND, under EXISTS", underExistential: true,
			outer: boxOwnerSecond, segments: []string{"o", "TAGS"},
			wantOwner: "D", wantRootOrdinal: 7,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := newGateTranslator(t)
			tr.unnestUnderExistential = tc.underExistential
			j := logical.NewJoin(tc.outer,
				&logical.LogicalUnnest{Segments: tc.segments, Alias: "X"},
				logical.JoinInner, "")
			expr := tr.translateUnnestJoin(j, j.Right.(*logical.LogicalUnnest)) //nolint:errcheck // fixture
			if expr == nil {
				t.Fatalf("translation failed: %v", tr.translateErr)
			}
			sel, ok := expr.(*expressions.SelectExpression)
			if !ok {
				t.Fatalf("unnest expr = %T, want a SelectExpression", expr)
			}

			collections := explodeCollectionsOf(sel)
			if len(collections) == 0 {
				t.Fatal("no Explode in the lowered unnest — the fixture stopped exercising the collection producer")
			}
			for _, coll := range collections {
				if why := whyNotOrdinalBaked(coll, tc.wantOwner, tc.wantRootOrdinal); why != "" {
					t.Fatal(why)
				}
			}
			// The owner must be a SIBLING QUANTIFIER of this select, or the
			// correlation names something the bipartition rules do not own and
			// the edge is not an edge.
			if !hasQuantifier(sel, tc.wantOwner) {
				t.Fatalf("owner %q is not a quantifier of the lowered select (%v) — a "+
					"correlation to a non-sibling is invisible to the partition rules",
					tc.wantOwner, quantifierAliases(sel))
			}
		})
	}
}

// TestBoxCollectionOrdinalIndexesTheMergedRow records the fact that makes the
// box arms above worth their lines: the single-source and box collections do
// NOT address the same row.
//
// Ordinal 3 and ordinal 7 are only different if they index different layouts —
// two ordinals against one layout would just be a wrong number. The domain
// token is what states the layout, so comparing the two directly is the
// assertion; without it, an implementation that read Order's OWN row from
// inside the box (and so answered 3) would look identical to the correct one on
// the single-source arm and merely "wrong" on the box arm.
func TestBoxCollectionOrdinalIndexesTheMergedRow(t *testing.T) {
	t.Parallel()

	bake := func(t *testing.T, outer logical.LogicalOperator) *values.FieldValue {
		t.Helper()
		tr := newGateTranslator(t)
		j := logical.NewJoin(outer,
			&logical.LogicalUnnest{Segments: []string{"o", "TAGS"}, Alias: "X"},
			logical.JoinInner, "")
		expr := tr.translateUnnestJoin(j, j.Right.(*logical.LogicalUnnest)) //nolint:errcheck // fixture
		sel, ok := expr.(*expressions.SelectExpression)
		if !ok {
			t.Fatalf("unnest expr = %T (%v)", expr, tr.translateErr)
		}
		colls := explodeCollectionsOf(sel)
		if len(colls) != 1 {
			t.Fatalf("want exactly one Explode collection, got %d", len(colls))
		}
		fv, isFV := colls[0].(*values.FieldValue)
		if !isFV || fv.Resolved == nil {
			t.Fatalf("collection = %#v, want a resolved *FieldValue", colls[0])
		}
		return fv
	}

	single := bake(t, scan("Order", "o"))
	box := bake(t, logical.NewJoin(
		logical.NewJoin(scan("Customer", "c"), scan("Order", "o"), logical.JoinInner, ""),
		scan("TypedRecord", "d"), logical.JoinFull, ""))

	if single.Resolved.Domain == box.Resolved.Domain {
		t.Fatalf("the single-source and box collections claim the SAME ordinal domain (%v) — "+
			"then their ordinals are two readings of one layout and the box arm proves "+
			"nothing about owner windows", single.Resolved.Domain)
	}
	if single.Resolved.Accessors[0].Ordinal == box.Resolved.Accessors[0].Ordinal {
		t.Fatalf("both collections root at ordinal %d — the fixture stopped placing the "+
			"owner off the merged row's front, which is the only arrangement that can "+
			"tell an owner-window bake from a bare first-match",
			single.Resolved.Accessors[0].Ordinal)
	}
}

// TestNameModelCollectionIsRejectedByTheGate proves the checker above can
// EXPRESS the defect it claims to exclude. Every arm asserts an absence, and an
// absence assertion that no input can violate is not coverage — it passes just
// as happily over a checker that returns "" unconditionally.
//
// The three inputs are the three ways the name model showed up.
func TestNameModelCollectionIsRejectedByTheGate(t *testing.T) {
	t.Parallel()

	arrType := values.NewArrayType(true, values.NullableString)
	for _, tc := range []struct {
		name string
		coll values.Value
		want string
	}{
		{
			// The multi-namespace mint: leg packed into the name, read off the
			// merged flow leg's row.
			name: "dotted read off the merged row",
			coll: &values.FieldValue{
				Field: "O.TAGS", Typ: arrType,
				Child: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("D$BOX")),
			},
			want: "packs its owning leg into the name",
		},
		{
			// The single-namespace mint: bare, but still LAZY, so it resolves
			// by name against whatever row it lands on.
			name: "lazy bare read",
			coll: values.NewFieldValue(
				values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("O")),
				"TAGS", arrType),
			want: "is LAZY",
		},
		{
			// Baked, but CHILDLESS: an ordinal with no quantifier under it, so
			// there is no owner for a bipartition to keep the Explode with. The
			// bake alone is not the property — the correlation it hangs off is.
			name: "baked but childless",
			coll: &values.FieldValue{
				Field: "TAGS", Typ: arrType,
				Resolved: &values.FieldPath{
					Accessors: []values.ResolvedAccessor{{Field: "TAGS", Ordinal: 3}},
				},
			},
			want: "correlates to nothing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			why := whyNotOrdinalBaked(tc.coll, "O", 3)
			if why == "" {
				t.Fatalf("the gate ACCEPTED %#v — it cannot express the shape it exists to "+
					"exclude, so every green arm above proves nothing", tc.coll)
			}
			if !strings.Contains(why, tc.want) {
				t.Fatalf("gate rejected for %q, want a rejection mentioning %q", why, tc.want)
			}
		})
	}
}

// TestUnnestBakedRootCollectionFusesAMultiSegmentPath covers the MULTI-SEGMENT
// arm (`FROM t, t.rec.arr AS x`), which has no coverage through
// translateUnnestJoin because no metadata reachable from SQL can express it —
// see TestStructColumnIsRejectedAtDDL (pkg/relational/core/embedded), which
// pins the gate that makes it unreachable and names what re-arms this.
//
// The arm is exercised at its own entry point instead of being left untested:
// the root segment must bake POSITIONALLY (the owner's window offset) and only
// the suffix may stay name-addressed, because a struct materializes as a proto
// message rather than a positional row. Getting that split wrong is how a
// multi-segment path silently reads the wrong column.
func TestUnnestBakedRootCollectionFusesAMultiSegmentPath(t *testing.T) {
	t.Parallel()

	tr := newGateTranslator(t)
	outer := scan("Order", "o")
	u := &logical.LogicalUnnest{Segments: []string{"o", "FLOWER", "TYPE"}, Alias: "X"}
	got := tr.unnestBakedRootCollection(outer, values.NamedCorrelationIdentifier("O"),
		u, "TYPE", values.NullableString, 1, -1)

	fv, isFV := got.(*values.FieldValue)
	if !isFV || fv.Resolved == nil {
		t.Fatalf("multi-segment bake = %#v, want a resolved *FieldValue", got)
	}
	if n := len(fv.Resolved.Accessors); n != 2 {
		t.Fatalf("accessors = %v, want exactly root+suffix", fv.Resolved.Accessors)
	}
	// FLOWER is Order's second column. The ROOT is positional: the whole point
	// is that the outer row is ordinal-addressed at this build, so a name-keyed
	// root has nothing to resolve against.
	if root := fv.Resolved.Accessors[0]; root.Ordinal != 1 {
		t.Fatalf("root accessor = %#v, want ordinal 1 (FLOWER's offset in Order) — a "+
			"root that is not positional is the name model with extra steps", root)
	}
	// The suffix descends a struct VALUE, so its ordinal is the loud -1
	// sentinel: out of range everywhere, never a silent slot read.
	suffix := fv.Resolved.Accessors[1]
	if suffix.Field != "TYPE" || suffix.Ordinal != -1 {
		t.Fatalf("suffix accessor = %#v, want {TYPE -1} — a real ordinal here would be "+
			"read as a positional slot of a proto message", suffix)
	}
	if corr := values.GetCorrelatedToOfValue(fv); len(corr) != 1 {
		t.Fatalf("multi-segment collection correlates to %v, want exactly the owner O", corr)
	}
	if _, hasOwner := values.GetCorrelatedToOfValue(fv)[values.NamedCorrelationIdentifier("O")]; !hasOwner {
		t.Fatal("multi-segment collection does not correlate to its owner O")
	}
}

// TestMultiSegmentUnnestIsRejectedBeforeTheBake records the OTHER half of why
// the arm above has no end-to-end route: even with a nested path spelled out,
// the lowering's classifier rejects it before any collection is built, because
// the leaf is not an array. If a nested ARRAY ever becomes expressible, this
// stops being the reason and the arm needs the full translateUnnestJoin case.
func TestMultiSegmentUnnestIsRejectedBeforeTheBake(t *testing.T) {
	t.Parallel()

	tr := newGateTranslator(t)
	j := logical.NewJoin(scan("Order", "o"),
		&logical.LogicalUnnest{Segments: []string{"o", "FLOWER", "TYPE"}, Alias: "X"},
		logical.JoinInner, "")
	if expr := tr.translateUnnestJoin(j, j.Right.(*logical.LogicalUnnest)); expr != nil {
		t.Fatalf("a multi-segment unnest over a NON-array leaf translated (%T) — the "+
			"classifier must reject it, and if it no longer does, the multi-segment "+
			"routing arm is live and needs coverage through translateUnnestJoin", expr)
	}
	var apiErr *api.Error
	if !errors.As(tr.translateErr, &apiErr) || apiErr.Code != api.ErrCodeInvalidColumnReference {
		t.Fatalf("translate error = %v, want %s", tr.translateErr, api.ErrCodeInvalidColumnReference)
	}
}

// whyNotOrdinalBaked returns "" when coll is the ordinal bake this lowering
// must emit, or the reason it is not. Shared by the routing arms and by the
// rejection test that proves it discriminates.
func whyNotOrdinalBaked(coll values.Value, wantOwner string, wantRootOrdinal int) string {
	fv, isFV := coll.(*values.FieldValue)
	if !isFV {
		return "Explode collection is not a *FieldValue read of the owner's column"
	}
	if strings.Contains(fv.Field, ".") {
		return "Explode collection field " + fv.Field + " packs its owning leg into the " +
			"name — qualification must be the bake's ordinal, not a string prefix"
	}
	if fv.Resolved == nil {
		return "Explode collection " + fv.Field + " is LAZY (Resolved == nil) — a name-keyed " +
			"read of a merged row, the qualified-name channel this lowering must not reopen"
	}
	corr := values.GetCorrelatedToOfValue(coll)
	if len(corr) == 0 {
		return "Explode collection " + fv.Field + " correlates to nothing — no owner edge " +
			"for the bipartition rules to respect"
	}
	if _, ok := corr[values.NamedCorrelationIdentifier(wantOwner)]; !ok || len(corr) != 1 {
		return "Explode collection " + fv.Field + " correlates to something other than its " +
			"owner " + wantOwner
	}
	if len(fv.Resolved.Accessors) == 0 {
		return "Explode collection " + fv.Field + " has no resolved accessor"
	}
	if got := fv.Resolved.Accessors[0].Ordinal; got != wantRootOrdinal {
		return "Explode collection " + fv.Field + " roots at ordinal " +
			strconv.Itoa(got) + ", want " + strconv.Itoa(wantRootOrdinal) +
			" — the array's offset in the row the collection reads is the fact the " +
			"dotted qualifier used to carry, so a wrong offset is a wrong column"
	}
	if !fv.Resolved.Domain.IsKnown() {
		return "Explode collection " + fv.Field + " carries no ordinal DOMAIN — without it " +
			"the ordinal is an unchecked integer rather than an addressed slot, and " +
			"OrdinalIn declines for it downstream"
	}
	return ""
}

// explodeCollectionsOf returns the collection Value of every Explode reachable
// through sel's quantifiers.
func explodeCollectionsOf(sel *expressions.SelectExpression) []values.Value {
	var out []values.Value
	for _, q := range sel.GetQuantifiers() {
		ref := q.GetRangesOver()
		if ref == nil {
			continue
		}
		for _, m := range ref.Members() {
			if ex, isExplode := m.(*expressions.ExplodeExpression); isExplode {
				out = append(out, ex.GetCollectionValue())
			}
		}
	}
	return out
}

func quantifierAliases(sel *expressions.SelectExpression) []string {
	var out []string
	for _, q := range sel.GetQuantifiers() {
		out = append(out, q.GetAlias().Name())
	}
	return out
}

func hasQuantifier(sel *expressions.SelectExpression, alias string) bool {
	for _, a := range quantifierAliases(sel) {
		if a == alias {
			return true
		}
	}
	return false
}
