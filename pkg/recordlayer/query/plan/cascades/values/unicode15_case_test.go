package values

import "testing"

func TestScalarCasingUsesUnicode15(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, input, want string }{
		{"UPPER", "\u0264", "\u0264"},
		{"UPPER", "\ua7d3", "\ua7d3"},
		{"UPPER", "\u1c8a", "\u1c8a"},
		{"LOWER", "\ua7cb", "\ua7cb"},
		{"LOWER", "\u1c89", "\u1c89"},
		{"LOWER", "\ua7ce", "\ua7ce"},
		{"LOWER", "\ua7f1", "\ua7f1"},
		{"UPPER", "\u00df", "\u00df"},
		{"LOWER", "\u0130", "i"},
		{"UPPER", "\u0131d", "ID"},
		{"UPPER", "\u017felect", "SELECT"},
		{"LOWER", "A\u03a3\u0295", "a\u03c3\u0295"},
	} {
		t.Run(tc.name+"_"+tc.input, func(t *testing.T) {
			t.Parallel()
			got, err := evalScalarFunction(tc.name, []any{tc.input})
			if err != nil || got != tc.want {
				t.Fatalf("%s(%q)=%q,%v; want %q", tc.name, tc.input, got, err, tc.want)
			}
		})
	}
}
