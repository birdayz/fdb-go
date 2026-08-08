package values

// FuseNestedSuffix's arms, driven directly.
//
// The rebase that calls it exercises exactly ONE path — a two-step `n.sk` over a
// leg whose column type is UNKNOWN. Every decline below is therefore unreachable
// from the corpus today, which is precisely why they need driving here: an arm
// whose first real firing is read as a FINDING rather than as an untested branch
// is the most expensive way to discover a bug in an instrument.
//
// The assert arm and the carry arm are BOTH live in production, and which one
// runs is decided by information the caller cannot control: the leg window states
// a struct column's type as UNKNOWN today and could state it tomorrow. A pin
// covering one of them would go quiet the moment the merge learns struct types.

import (
	"strings"
	"testing"
)

func fuseFixture(t *testing.T, nullableRoot bool) (root *FieldValue, into *RecordType) {
	t.Helper()
	into = NewRecordType("NST", false, []Field{
		{Name: "SK", FieldType: NotNullLong, Ordinal: 0},
		{Name: "CO", FieldType: NotNullLong, Ordinal: 1},
	})
	rootTyp := NotNullLong
	if nullableRoot {
		rootTyp = NullableInt
	}
	root = &FieldValue{
		Field:    "N",
		Typ:      rootTyp,
		Child:    NewQuantifiedObjectValue(NamedCorrelationIdentifier("M")),
		Resolved: NewFieldPathOfSingle("N", 11, true),
	}
	return root, into
}

func TestFuseNestedSuffix_AssertArmDerivesAndAgrees(t *testing.T) {
	t.Parallel()
	root, into := fuseFixture(t, false)
	out, err := FuseNestedSuffix(root, into, []ResolvedAccessor{{Field: "CO", Ordinal: 1}}, NotNullLong)
	if err != nil {
		t.Fatalf("the assert arm declined a suffix that AGREES with the record it "+
			"descends into: %v", err)
	}
	if len(out.Resolved.Accessors) != 2 ||
		out.Resolved.Accessors[0].Ordinal != 11 || out.Resolved.Accessors[1].Ordinal != 1 {
		t.Fatalf("fused path %v, want [{N 11} {CO 1}] — the root address followed by "+
			"the derived suffix step", out.Resolved.Accessors)
	}
	if out.Field != "CO" {
		t.Fatalf("display name %q, want the LEAF's (CO). A one-step bake would have "+
			"rendered the leaf, and a fused node must render the same", out.Field)
	}
}

// THE CARRY ARM IS THE ONE PRODUCTION TAKES. Neither the merged row nor the leg
// window carries a struct column's type — both state UNKNOWN — so a design that
// REQUIRED a record type would decline every real nested reference while looking
// careful. Measured: a first implementation did exactly that.
func TestFuseNestedSuffix_CarryArmStandsWithoutALayout(t *testing.T) {
	t.Parallel()
	root, _ := fuseFixture(t, false)
	out, err := FuseNestedSuffix(root, nil, []ResolvedAccessor{{Field: "CO", Ordinal: 1}}, NotNullLong)
	if err != nil {
		t.Fatalf("no record type to descend into and the fuse DECLINED: %v — this is "+
			"the arm every real nested reference takes, because the merged and leg "+
			"layouts both state UNKNOWN for a struct column", err)
	}
	if len(out.Resolved.Accessors) != 2 || out.Resolved.Accessors[1].Ordinal != 1 {
		t.Fatalf("fused path %v, want the carried suffix step at ordinal 1",
			out.Resolved.Accessors)
	}
	if _, isRec := out.Typ.(*RecordType); isRec || out.Typ == nil {
		t.Fatalf("result type %v — want the REFERENCE's own leaf type. A record type "+
			"means the fuse took the root SLOT's type, which reports the whole struct "+
			"as the type of a single-column read", out.Typ)
	}
}

// A typed nil *RecordType in a Type interface is a NON-NIL interface, so a naive
// type assertion succeeds with a nil receiver and the assert arm dereferences it.
// This shipped: all four typed arms of the rebase pin passed while every real
// query crashed, because the fixture only ever stated a type.
func TestFuseNestedSuffix_ATypedNilLayoutTakesTheCarryArmRatherThanPanicking(t *testing.T) {
	t.Parallel()
	root, _ := fuseFixture(t, false)
	var typedNil *RecordType
	out, err := FuseNestedSuffix(root, typedNil, []ResolvedAccessor{{Field: "CO", Ordinal: 1}}, NotNullLong)
	if err != nil {
		t.Fatalf("a typed-nil layout declined: %v — it must read as 'no layout' and "+
			"take the carry arm", err)
	}
	if len(out.Resolved.Accessors) != 2 {
		t.Fatalf("fused path %v, want two steps", out.Resolved.Accessors)
	}
}

func TestFuseNestedSuffix_Declines(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		into    *RecordType
		suffix  []ResolvedAccessor
		wantErr string
		why     string
	}{
		{
			name:   "the suffix names a field the record does not have",
			into:   NewRecordType("NST", false, []Field{{Name: "SK", FieldType: NotNullLong, Ordinal: 0}}),
			suffix: []ResolvedAccessor{{Field: "CO", Ordinal: 1}}, wantErr: "absent from the record",
			why: "descending a name the record lacks would land on nothing; carrying the " +
				"ordinal anyway would land on whatever slot 1 happens to be",
		},
		{
			name: "the suffix name is DUPLICATED in the record",
			// HAND-BUILT, and that is a finding rather than a shortcut:
			// NewRecordType PANICS on a duplicate field name, so this arm cannot
			// be reached through the constructor. It is defensive against a
			// hand-assembled type — of which this package has several — and the
			// constructor guarantee is why it stays cheap rather than why it goes.
			into: &RecordType{Fields: []Field{
				{Name: "CO", FieldType: NotNullLong, Ordinal: 0},
				{Name: "CO", FieldType: NotNullLong, Ordinal: 1},
			}},
			suffix: []ResolvedAccessor{{Field: "CO", Ordinal: 1}}, wantErr: "ambiguous",
			why: "a first name match is indistinguishable from a correct one, and " +
				"disambiguating needs an identity this site does not carry",
		},
		{
			name: "the carried ordinal CONTRADICTS the record it descends into",
			into: NewRecordType("NST", false, []Field{
				{Name: "SK", FieldType: NotNullLong, Ordinal: 0},
				{Name: "CO", FieldType: NotNullLong, Ordinal: 1},
			}),
			suffix: []ResolvedAccessor{{Field: "CO", Ordinal: 0}}, wantErr: "disagrees",
			why: "the carried ordinal is a TRIPWIRE, not a source. Taking it would read " +
				"SK while claiming to be CO — silent, because slot 0 is a real column",
		},
		{
			name:   "an UNNAMED suffix step out of range",
			into:   NewRecordType("NST", false, []Field{{Name: "SK", FieldType: NotNullLong, Ordinal: 0}}),
			suffix: []ResolvedAccessor{{Ordinal: 7}}, wantErr: "out of range",
			why: "a nameless accessor has only its ordinal, so the range check is the whole check",
		},
		{
			name: "an unnamed step with NO ordinal and no layout to derive one",
			into: nil, suffix: []ResolvedAccessor{{Ordinal: -1}}, wantErr: "states no ordinal",
			why: "Go mints Ordinal:-1 name-only accessors at four producer sites; " +
				"answering -1 hands the caller an address that matches every other one",
		},
		{
			name:    "the suffix descends past a STATED leaf type",
			into:    NewRecordType("NST", false, []Field{{Name: "SK", FieldType: NotNullLong, Ordinal: 0}}),
			suffix:  []ResolvedAccessor{{Field: "SK", Ordinal: 0}, {Field: "DEEPER", Ordinal: 0}},
			wantErr: "stated non-record",
			why: "SK is a scalar and the schema SAYS so, so there is nothing below it " +
				"to address. This is the case that must not collapse into the carry " +
				"arm: an UNSTATED type carries the next accessor, a STATED leaf refuses " +
				"it, and treating both as 'no layout' would mint a plausible deeper " +
				"address into a scalar",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root, _ := fuseFixture(t, false)
			out, err := FuseNestedSuffix(root, tc.into, tc.suffix, NotNullLong)
			if err == nil {
				t.Fatalf("FuseNestedSuffix ACCEPTED %s and returned %v — %s",
					tc.name, out.Resolved.Accessors, tc.why)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("declined with %q, want a message containing %q — the reason a "+
					"fuse refused is the only diagnostic a caller gets, and every arm "+
					"here declines for a DIFFERENT reason", err, tc.wantErr)
			}
		})
	}
}

// An EMPTY suffix hands the root back unchanged. It is the flat case, and every
// caller relies on it being free rather than special-cased at the call site.
func TestFuseNestedSuffix_AnEmptySuffixIsTheIdentity(t *testing.T) {
	t.Parallel()
	root, into := fuseFixture(t, false)
	out, err := FuseNestedSuffix(root, into, nil, NotNullLong)
	if err != nil || out != root {
		t.Fatalf("empty suffix returned (%v, %v), want the root unchanged", out, err)
	}
}

// Nullability accumulates DOWN the descent. A column read through a nullable
// record is nullable, because a null-supplied row serves NULL in every slot —
// and that rule applies once per step, not once per fuse.
func TestFuseNestedSuffix_NullabilityAccumulates(t *testing.T) {
	t.Parallel()

	// A NULLABLE merged slot promotes a NOT NULL leaf.
	root, into := fuseFixture(t, true)
	out, err := FuseNestedSuffix(root, into, []ResolvedAccessor{{Field: "SK", Ordinal: 0}}, NotNullLong)
	if err != nil {
		t.Fatalf("fuse: %v", err)
	}
	if out.Typ == nil || !out.Typ.IsNullable() {
		t.Fatalf("leaf type %v is NOT nullable through a NULLABLE merged slot — a "+
			"LEFT-outer null-supplied column would be reported NOT NULL, which is the "+
			"direction that silently drops NULLs", out.Typ)
	}

	// CONTROL: with a NOT NULL slot and a NOT NULL record, nothing is promoted.
	// Without this the assertion above could hold because the fuse promotes
	// everything unconditionally.
	rootNN, intoNN := fuseFixture(t, false)
	outNN, err := FuseNestedSuffix(rootNN, intoNN, []ResolvedAccessor{{Field: "SK", Ordinal: 0}}, NotNullLong)
	if err != nil {
		t.Fatalf("fuse: %v", err)
	}
	if outNN.Typ != nil && outNN.Typ.IsNullable() {
		t.Fatalf("leaf type %v is nullable with NOTHING nullable on the path — the "+
			"promotion above then proves nothing, because it fires unconditionally",
			outNN.Typ)
	}
}
