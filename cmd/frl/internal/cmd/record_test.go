package cmd

import (
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

func TestParsePrimaryKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in       string
		wantKind string // "int" or "string"
		wantVal  any
	}{
		{"42", "int", int64(42)},
		{"0", "int", int64(0)},
		{"-7", "int", int64(-7)},
		{"9223372036854775807", "int", int64(9223372036854775807)}, // max int64
		{"abc", "string", "abc"},
		{"42abc", "string", "42abc"}, // not a valid int
		{"", "string", ""},
		{"1.5", "string", "1.5"}, // not an int — float is a string (tuple-layer can pack float but we'd need type hint)
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got := parsePrimaryKey(tc.in)
			if len(got) != 1 {
				t.Fatalf("parsePrimaryKey(%q) len = %d, want 1", tc.in, len(got))
			}
			switch tc.wantKind {
			case "int":
				n, ok := got[0].(int64)
				if !ok {
					t.Errorf("parsePrimaryKey(%q)[0] = %v (%T), want int64", tc.in, got[0], got[0])
					return
				}
				if n != tc.wantVal.(int64) {
					t.Errorf("parsePrimaryKey(%q)[0] = %d, want %d", tc.in, n, tc.wantVal)
				}
			case "string":
				s, ok := got[0].(string)
				if !ok {
					t.Errorf("parsePrimaryKey(%q)[0] = %v (%T), want string", tc.in, got[0], got[0])
					return
				}
				if s != tc.wantVal.(string) {
					t.Errorf("parsePrimaryKey(%q)[0] = %q, want %q", tc.in, s, tc.wantVal)
				}
			}
		})
	}
}

func TestFormatPK(t *testing.T) {
	t.Parallel()
	// Empty tuple formats as empty string (edge case — should never happen
	// in practice since PKs are always at least one element).
	if got := formatPK(nil); got != "" {
		t.Errorf("formatPK(nil) = %q, want empty", got)
	}

	// Round-trip via parsePrimaryKey: ensure the formatter produces
	// something that re-parses equivalently for single-element int PKs.
	pk := parsePrimaryKey("42")
	if got := formatPK(pk); got != "42" {
		t.Errorf("formatPK(parsePrimaryKey(42)) = %q, want 42", got)
	}
	pk = parsePrimaryKey("customer-7")
	if got := formatPK(pk); got != "customer-7" {
		t.Errorf("formatPK(parsePrimaryKey(customer-7)) = %q, want customer-7", got)
	}
}

// TestFormatPK_BinaryTypes verifies specialized formatting for the
// tuple element types Go's `%v` prints badly (byte slices as `[1 2 3]`,
// UUIDs as opaque struct values, etc.). For each, the rendered form
// must contain meaningful characters an operator can recognize.
func TestFormatPK_BinaryTypes(t *testing.T) {
	t.Parallel()

	// []byte → hex. Without the fix, this rendered as "[1 2 3]".
	pk := tuple.Tuple{[]byte{0x01, 0x02, 0xff}}
	if got := formatPK(pk); got != "0102ff" {
		t.Errorf("byte PK formatPK = %q; want 0102ff", got)
	}

	// UUID → canonical form (8-4-4-4-12).
	uuid := tuple.UUID{
		0x55, 0x0e, 0x84, 0x00,
		0xe2, 0x9b, 0x41, 0xd4,
		0xa7, 0x16, 0x44, 0x66,
		0x55, 0x44, 0x00, 0x00,
	}
	want := "550e8400-e29b-41d4-a716-446655440000"
	if got := formatPK(tuple.Tuple{uuid}); got != want {
		t.Errorf("UUID PK formatPK = %q; want %s", got, want)
	}

	// nil → readable placeholder rather than the literal "<nil>" Go default
	// renders when it's inside a %v'd struct (which happens to match here,
	// but the contract is "something stable and grep-able").
	if got := formatPK(tuple.Tuple{nil}); got != "<nil>" {
		t.Errorf("nil element formatPK = %q; want <nil>", got)
	}

	// Composite PK with mixed types — comma-separated, no quoting.
	mixed := tuple.Tuple{int64(1), "foo", []byte{0xab}}
	if got := formatPK(mixed); got != "1,foo,ab" {
		t.Errorf("mixed PK formatPK = %q; want 1,foo,ab", got)
	}

	// Nested tuple — wrapped in parens so the composite structure is
	// visually distinct from a flat comma join.
	nested := tuple.Tuple{int64(1), tuple.Tuple{int64(2), "x"}}
	if got := formatPK(nested); got != "1,(2,x)" {
		t.Errorf("nested tuple formatPK = %q; want 1,(2,x)", got)
	}

	// Versionstamp — uses its own compact String(). Apps with VERSION
	// indexes surface these as PK suffixes, so this is the one tuple
	// type whose default `%v` rendering would have been least useful.
	vs := tuple.Versionstamp{
		TransactionVersion: [10]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a},
		UserVersion:        7,
	}
	got := formatPK(tuple.Tuple{vs})
	if !strings.Contains(got, "Versionstamp(") || !strings.Contains(got, "7") {
		t.Errorf("Versionstamp PK formatPK = %q; want contains Versionstamp(…) and user version 7", got)
	}
}

// Composite PKs parse as comma-separated tuple elements — the same form
// formatPK renders, so scan output round-trips into record get.
func TestParsePrimaryKey_Composite(t *testing.T) {
	t.Parallel()
	pk := parsePrimaryKey("1,ITEMS,42x")
	if len(pk) != 3 {
		t.Fatalf("len = %d; want 3 (%v)", len(pk), pk)
	}
	if pk[0] != int64(1) || pk[1] != "ITEMS" || pk[2] != "42x" {
		t.Errorf("parsePrimaryKey = %v; want [1 ITEMS 42x]", pk)
	}
	if got := formatPK(pk); got != "1,ITEMS,42x" {
		t.Errorf("formatPK round-trip = %q; want original", got)
	}
}
