package recordlayer

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
