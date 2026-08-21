package semantic

import "testing"

func TestIdentifier_BareCaseFolds(t *testing.T) {
	t.Parallel()
	id := NewUnquoted("Name")
	if got, want := id.Name(), "NAME"; got != want {
		t.Fatalf("Name: got %q, want %q", got, want)
	}
	if id.WasQuoted() {
		t.Fatal("bare identifier should not be WasQuoted")
	}
}

func TestIdentifier_DoubleQuotedPreservesCase(t *testing.T) {
	t.Parallel()
	id := NewUnquoted(`"Name"`)
	if got, want := id.Name(), "Name"; got != want {
		t.Fatalf("Name: got %q, want %q", got, want)
	}
	if !id.WasQuoted() {
		t.Fatal("quoted identifier should be WasQuoted")
	}
}

func TestIdentifier_SingleQuotedPreservesCase(t *testing.T) {
	t.Parallel()
	id := NewUnquoted(`'Name'`)
	if got, want := id.Name(), "Name"; got != want {
		t.Fatalf("Name: got %q, want %q", got, want)
	}
	if !id.WasQuoted() {
		t.Fatal("single-quoted identifier should be WasQuoted")
	}
}

func TestIdentifier_CaseSensitiveBarePreservesCase(t *testing.T) {
	t.Parallel()
	id := New("Name", true)
	if got, want := id.Name(), "Name"; got != want {
		t.Fatalf("case-sensitive bare Name: got %q, want %q", got, want)
	}
	if id.WasQuoted() {
		t.Fatal("bare identifier should not be WasQuoted")
	}
}

func TestIdentifier_EmptyIsZero(t *testing.T) {
	t.Parallel()
	id := NewUnquoted("")
	if !id.IsZero() {
		t.Fatal("empty input should produce zero Identifier")
	}
}

// Map-key equality: same Name + same WasQuoted → same key.
// Different Name OR different WasQuoted → different key.
func TestIdentifier_MapKey(t *testing.T) {
	t.Parallel()
	m := map[Identifier]string{
		NewUnquoted("age"):   "column1",
		NewUnquoted("name"):  "column2",
		NewUnquoted(`"age"`): "column3", // same name text but wasQuoted → distinct key
	}
	if got, want := m[NewUnquoted("AGE")], "column1"; got != want {
		t.Fatalf("case-folded AGE lookup: got %q, want %q", got, want)
	}
	if got, want := m[NewUnquoted(`"age"`)], "column3"; got != want {
		t.Fatalf("quoted age lookup: got %q, want %q", got, want)
	}
	// `"AGE"` is a different quoted identifier (case preserved, name=AGE).
	if _, found := m[NewUnquoted(`"AGE"`)]; found {
		t.Fatal(`"AGE" should not match any registered key`)
	}
}

// EqualsIgnoreQuoting: quoted-vs-bare with matching name → equal.
func TestIdentifier_EqualsIgnoreQuoting(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b Identifier
		want bool
	}{
		{"same normalized bare", NewUnquoted("name"), NewUnquoted("NAME"), true},
		{"quoted vs bare same name", NewUnquoted(`"NAME"`), NewUnquoted("name"), true},
		{"different identifier", NewUnquoted("foo"), NewUnquoted("bar"), false},
		{"both zero", Identifier{}, Identifier{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.a.EqualsIgnoreQuoting(tc.b); got != tc.want {
				t.Fatalf("%s ≈ %s: got %v, want %v",
					tc.a.Name(), tc.b.Name(), got, tc.want)
			}
		})
	}
}

func TestIdentifier_StringImplStringer(t *testing.T) {
	t.Parallel()
	id := NewUnquoted("Name")
	if got, want := id.String(), "NAME"; got != want {
		t.Fatalf("String: got %q, want %q", got, want)
	}
}

func TestNormalizeString_Semantics(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		in            string
		caseSensitive bool
		want          string
	}{
		{"empty", "", false, ""},
		{"empty case-sensitive", "", true, ""},
		{"bare folded", "name", false, "NAME"},
		{"bare preserved", "Name", true, "Name"},
		{"double-quoted", `"Name"`, false, "Name"},
		{"single-quoted", `'Name'`, false, "Name"},
		{"double-quoted case-sensitive", `"Name"`, true, "Name"},
		{"mismatched quotes not treated as quoted", `"name'`, false, `"NAME'`},
		{"lone quote char not quoted", `"`, false, `"`},
		// Empty quoted strings: the delimiter pair has no content —
		// reject as quoted so we don't manufacture a WasQuoted-true
		// empty Identifier that compares unequal to Identifier{}.
		{`"" not treated as quoted`, `""`, false, `""`},
		{"'' not treated as quoted", `''`, false, `''`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := NormalizeString(tc.in, tc.caseSensitive); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFromNormalizedDiscardsTheQuotingFlag pins a NEGATIVE RESULT that three
// separate documents rest on.
//
// RFC-237 §3.3, the entry in DIVERGENCES.md and the comment on relaxedPass all
// say the same thing: making Go's identifier resolution conformant with Java is
// NOT plumbing CASE_SENSITIVE_IDENTIFIERS but preserving the QUOTING BIT, which
// the reference path destroys. The evidence for that is a probe — gating
// relaxedPass on `!want.WasQuoted()` changes nothing, because there is no flag
// left to gate on — and a probe that only ever ran once proves nothing about
// tomorrow.
//
// So the fact is pinned here rather than described three times. If this goes
// RED, the quoting bit survives normalization and the remediation named in all
// three places has been done (or half-done) — which is exactly when someone
// needs to re-read them.
func TestFromNormalizedDiscardsTheQuotingFlag(t *testing.T) {
	t.Parallel()
	// The captured text of a QUOTED identifier is indistinguishable from an
	// unquoted one that happened to be written in the same case, because
	// StripIdentifierQuotes returns bare text either way.
	fromQuoted := FromNormalized("x")
	if fromQuoted.WasQuoted() {
		t.Fatal("FromNormalized now reports WasQuoted — the quoting bit survives the " +
			"parse capture. Re-read RFC-237 §3.3, the DIVERGENCES.md identifier entry " +
			"and relaxedPass: all three say it does not, and name work that depends on it.")
	}
	// And the relaxed pass therefore cannot separate a quoted reference from an
	// unquoted one: both arrive as bare text with the flag already gone.
	quotedRef := FromNormalized("K")
	unquotedRef := NewUnquoted("k")
	if !relaxedPass.matches(FromNormalized("k"), quotedRef) {
		t.Error(`the relaxed pass declined "K" against a column k — if the quoting bit ` +
			"were preserved this would be correct Java behaviour, and the extension's " +
			"cost would have changed")
	}
	if !relaxedPass.matches(FromNormalized("k"), unquotedRef) {
		t.Error("the relaxed pass declined an UNQUOTED reference, which is the case it exists for")
	}
	// The strict pass separates them by TEXT, which is all the information left.
	if strictPass.matches(FromNormalized("k"), quotedRef) {
		t.Error("the strict pass matched K against k — it must compare exactly")
	}
}
