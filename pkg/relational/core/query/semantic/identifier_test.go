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
