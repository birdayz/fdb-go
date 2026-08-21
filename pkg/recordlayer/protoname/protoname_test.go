package protoname

import (
	"errors"
	"testing"
)

// The escaping is WIRE (message/field names in the persisted descriptor).
// The "x$$" and "foo.tableA" expectations are the live-JVM measured shapes
// from the RFC-204 descriptor probe (Java 4.12.11.0 stores struct "x$$" as
// message "x__1__1" and table "foo.tableA" as "foo__2tableA").
func TestToProtoBufCompliantName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{"T", "T"},
		{"foo", "foo"},
		{"x$$", "x__1__1"},
		{"foo.tableA", "foo__2tableA"},
		{"a__b", "a__0b"},
		{"a$b.c", "a__1b__2c"},
		{"__ROW_VERSION", "__ROW_VERSION"},
		{"__a.b", "__a__2b"},
		{"__a__b", "__a__0b"},
		{"____", "____0"}, // leading "__" kept, remaining "__" escaped
		{"_x", "_x"},
	}
	for _, c := range cases {
		got, err := ToProtoBufCompliantName(c.in)
		if err != nil {
			t.Fatalf("ToProtoBufCompliantName(%q): unexpected error %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("ToProtoBufCompliantName(%q) = %q, want %q", c.in, got, c.want)
		}
		// Reversibility (Java: toUserIdentifier is the exact inverse).
		if back := ToUserIdentifier(got); back != c.in {
			t.Errorf("ToUserIdentifier(%q) = %q, want %q", got, back, c.in)
		}
	}
}

func TestToProtoBufCompliantNameRejections(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", ".x", "$x", "__0x", "__1x", "__2x", "a b", "1abc", "a-b", "ü"} {
		_, err := ToProtoBufCompliantName(in)
		if err == nil {
			t.Errorf("ToProtoBufCompliantName(%q): expected error, got none", in)
			continue
		}
		var ine *InvalidNameError
		if !errors.As(err, &ine) {
			t.Errorf("ToProtoBufCompliantName(%q): error %T is not *InvalidNameError", in, err)
		}
	}
}

func TestCheckValidProtoBufCompliantName(t *testing.T) {
	t.Parallel()
	if err := CheckValidProtoBufCompliantName("Valid_Name9"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	err := CheckValidProtoBufCompliantName("9bad")
	if err == nil {
		t.Fatal("expected error for 9bad")
	}
	// Java's exact wording, typo included ("it not").
	if want := "9bad it not a valid protobuf identifier"; err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

// TestToProtoBufCompliantName_CollisionWitnesses pins the two known
// NON-injective pairs of the escaping — Java's own defect, reproduced
// faithfully because the escaped names are wire: a leading "_" followed by
// a special character produces the same escaped name as the literal
// triple-underscore identifier. If either pair stops colliding, Go's
// escaping has diverged from ProtoUtils and stores different bytes than
// Java for the same DDL — that is a WIRE regression, not a fix; align with
// upstream first.
func TestToProtoBufCompliantName_CollisionWitnesses(t *testing.T) {
	t.Parallel()
	for _, pair := range [][2]string{
		{"_$", "___1"},
		{"_.", "___2"},
	} {
		a, errA := ToProtoBufCompliantName(pair[0])
		if errA != nil {
			t.Fatalf("escape(%q): %v", pair[0], errA)
		}
		b, errB := ToProtoBufCompliantName(pair[1])
		if errB != nil {
			t.Fatalf("escape(%q): %v", pair[1], errB)
		}
		if a != b {
			t.Fatalf("escape(%q)=%q and escape(%q)=%q no longer collide — the escaping diverged from ProtoUtils",
				pair[0], a, pair[1], b)
		}
	}
}

// ToUserIdentifier IS NOT IDEMPOTENT, and that is a hazard rather than a quirk.
//
// Decoding twice CORRUPTS any name whose decoded form is itself a valid escape:
//
//	MY__01TABLE -> MY__1TABLE -> MY$TABLE
//
// So a value's namespace has to be tracked, not inferred. A struct carrying some
// fields in storage names and others already decoded is a trap for the next
// caller, who cannot tell them apart by looking and gets no error when wrong —
// just a different table name.
//
// Pinned because a decision rests on it: statistics diagnostics document the
// namespace per field instead of decoding defensively at every consumer.
func TestToUserIdentifierIsNotIdempotent(t *testing.T) {
	t.Parallel()

	// The load-bearing case: two decodes give a DIFFERENT answer than one.
	const doubleEscaped = "MY__01TABLE"
	once := ToUserIdentifier(doubleEscaped)
	twice := ToUserIdentifier(once)
	if once == twice {
		t.Fatalf("ToUserIdentifier is idempotent for %q (%q), so the per-field namespace "+
			"documentation it justifies is unnecessary — check whether the escaping "+
			"changed", doubleEscaped, once)
	}
	if once != "MY__1TABLE" || twice != "MY$TABLE" {
		t.Errorf("decode chain = %q -> %q, want MY__1TABLE -> MY$TABLE", once, twice)
	}

	// THE CHAIN IS NOT BOUNDED AT TWO. Each decode peels exactly ONE escape
	// level, so a name with N nested escapes needs N decodes to settle:
	//
	//	MY__001TABLE -> MY__01TABLE -> MY__1TABLE -> MY$TABLE
	//
	// An earlier version of this test asserted decoding was "stable from the
	// second application onward", which is true only for names escaped once and
	// made the hazard sound like a fixed off-by-one. It is not bounded: the
	// depth is whatever the name carries. That is why the rule is to track a
	// value's namespace rather than to decode defensively "one more time".
	chain := []string{"MY__001TABLE", "MY__01TABLE", "MY__1TABLE", "MY$TABLE"}
	for i := 0; i < len(chain)-1; i++ {
		if got := ToUserIdentifier(chain[i]); got != chain[i+1] {
			t.Errorf("ToUserIdentifier(%q) = %q, want %q — the escape depth changed",
				chain[i], got, chain[i+1])
		}
	}
	// Only the fully decoded form is a fixed point.
	if got := ToUserIdentifier("MY$TABLE"); got != "MY$TABLE" {
		t.Errorf("the decoded form is not stable: MY$TABLE -> %q", got)
	}
	// And the intermediate forms are NOT fixed points, which is the whole
	// hazard. Guard it so the loop above cannot go vacuous by every element
	// happening to be stable.
	for _, notStable := range []string{"MY__001TABLE", "MY__01TABLE", "MY__1TABLE"} {
		if ToUserIdentifier(notStable) == notStable {
			t.Errorf("%q is a fixed point; the nesting hazard this test pins is gone",
				notStable)
		}
	}
}
