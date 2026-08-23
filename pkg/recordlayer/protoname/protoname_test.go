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

// THE ESCAPE IS NOT A BIJECTION, WHICH IS WHY A DECODED NAME MUST BE
// ROUND-TRIP-CHECKED BEFORE IT IS SHOWN TO ANYONE.
//
// Record-layer metadata does not only come from the SQL layer:
// RecordMetaDataBuilder.SetRecords copies protobuf identifiers verbatim, so a
// record type may legally be named __0Order having never been escaped from
// anything. Decoding it is then not a translation, it is a rename — and the
// renamed spelling addresses nothing, because re-encoding does not recover the
// stored name.
//
// cmd/frl's userName relies on exactly this test: it decodes only when
// encode(decode(s)) == s. If these shapes ever start round-tripping, that guard
// becomes dead weight rather than load-bearing, and this test says so.
func TestEscapeIsNotABijection(t *testing.T) {
	t.Parallel()

	// Shapes where decode-then-encode does NOT recover the input. Each would be
	// displayed as a name that resolves to nothing, or to something else.
	for _, s := range []string{
		"__0Order", // decodes to __Order, which re-encodes to __Order
		"A__3B",    // decode is identity, but encoding it yields A__03B
	} {
		user := ToUserIdentifier(s)
		back, err := ToProtoBufCompliantName(user)
		if err == nil && back == s {
			t.Errorf("%q now round-trips (%q -> %q); the round-trip guard in "+
				"cmd/frl's userName is no longer load-bearing and should be re-examined",
				s, user, back)
		}
	}

	// And the shapes that DO round-trip. A regression here would make the guard
	// reject good names and silently stop decoding.
	//
	// ROUND-TRIPPING IS NOT THE SAME AS SAFE TO OFFER, and MY__01TABLE in this
	// very list is the counterexample: it round-trips, yet its decoded spelling
	// MY__1TABLE is itself a legal STORED name, so if both are declared then
	// GetRecordType's direct-key step answers with the other type. That is why
	// the round-trip guard is paired with an ambiguity gate rather than trusted
	// alone -- see cmd/frl's userNamesFor and
	// TestGetRecordTypeMisResolvesAnAmbiguousPair. What this loop pins is only
	// that decoding these names LOSES NOTHING; whether offering one is safe
	// depends on the declared set, which this package cannot see.
	for _, s := range []string{"MY__1TABLE", "MY__01TABLE", "A__2B", "Order"} {
		user := ToUserIdentifier(s)
		back, err := ToProtoBufCompliantName(user)
		if err != nil || back != s {
			t.Errorf("%q must round-trip: decoded %q, re-encoded %q (err %v)", s, user, back, err)
		}
	}
}

// A REPEATED NAME IS NOT A COLLISION.
//
// SafeDecoderOver's collision check keys on the DECODED name. Keyed as a set it
// cannot tell "two stored names decode alike" from "one stored name listed
// twice", so any caller passing overlapping lists silently lost decoding for
// the whole output — measured: the two-element call returned the stored
// spelling while the one-element call decoded.
//
// That mattered concretely: cmd/frl's status renderer feeds PerType's keys and
// MissingTypes, and ExtraTypes is a SUBSET of PerType's keys, so re-adding it
// would have forced stored names for every status carrying an orphan type.
func TestSafeDecoderOverToleratesRepeats(t *testing.T) {
	t.Parallel()

	const stored, decoded = "MY__1TABLE", "MY$TABLE"
	if ToUserIdentifier(stored) != decoded {
		t.Fatalf("fixture is vacuous: %q decodes to %q", stored, ToUserIdentifier(stored))
	}

	once := SafeDecoderOver([]string{stored}, nil)(stored)
	twice := SafeDecoderOver([]string{stored, stored}, nil)(stored)
	if once != decoded {
		t.Fatalf("single-element call did not decode: got %q", once)
	}
	if twice != once {
		t.Errorf("listing the same name twice suppressed decoding (%q vs %q) — a "+
			"repeat is not a collision, and a caller passing overlapping lists "+
			"would silently lose decoding for its whole output", twice, once)
	}

	// And a genuine collision still suppresses.
	const other = "MY__01TABLE" // decodes to MY__1TABLE, the other stored name
	if got := SafeDecoderOver([]string{stored, other}, nil)(other); got != other {
		t.Errorf("a real collision must fall back to stored names, got %q", got)
	}
}

// DECODING COLLIDES, and the witness has to be chosen against the rule that
// derives the name -- which is the mistake this test exists to prevent.
//
// RFC-238 §7c argues that a decoded (SQL) spelling cannot serve as a memo
// identity because two distinct stored names can present one. An earlier draft
// of that argument paired a proto message named `A__1B` with a DDL table
// declared `"A__1B"`, and it is wrong: the proto decodes to `A$B` while the DDL
// table stores `A__01B` and decodes to `A__1B`, so the two never meet. The
// argument survives, but only on a pair that actually collides.
//
// Both directions are pinned here because the docs cite both: protoname's own
// package comment cites `__0_`/`___0`, and §7c cites `X__0__1Y`/`X____1Y`.
func TestDecodeCollisionWitnesses(t *testing.T) {
	t.Parallel()

	for _, c := range []struct{ a, b, want string }{
		// Cited by ToUserIdentifier's doc comment.
		{"__0_", "___0", "___"},
		// Cited by RFC-238 §7c. The scan replaces `__1` before `__0`, so the
		// first loses its `__0` to `__` while the second keeps the two
		// underscores it started with.
		{"X__0__1Y", "X____1Y", "X__$Y"},
	} {
		gotA, gotB := ToUserIdentifier(c.a), ToUserIdentifier(c.b)
		if gotA != c.want || gotB != c.want {
			t.Errorf("%q and %q must BOTH decode to %q; got %q and %q.\n"+
				"If the escaping changed, the non-injectivity argument in RFC-238 §7c\n"+
				"and the collision note on ToUserIdentifier both need a new witness --\n"+
				"they are load-bearing, not illustrative.", c.a, c.b, c.want, gotA, gotB)
		}
		if c.a == c.b {
			t.Errorf("witness pair %q is not a pair", c.a)
		}
	}

	// THE NON-WITNESS, kept so the corrected example cannot quietly revert to
	// the wrong one. These decode APART, which is why they prove nothing about
	// memo identity.
	if a, b := ToUserIdentifier("A__1B"), ToUserIdentifier("A__01B"); a == b {
		t.Errorf("A__1B and A__01B now decode alike (%q); RFC-238 §7c rejects this pair\n"+
			"as a non-witness on exactly that ground and would need rewriting.", a)
	}
}
