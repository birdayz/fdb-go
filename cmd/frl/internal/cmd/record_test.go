package cmd

import (
	"testing"
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
