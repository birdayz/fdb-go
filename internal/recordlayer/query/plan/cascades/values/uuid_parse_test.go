package values

import (
	"testing"

	"github.com/google/uuid"
)

func TestParseJavaUUID(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"123e4567-e89b-12d3-a456-426614174000":   "123e4567-e89b-12d3-a456-426614174000",
		"1-2-3-4-5":                              "00000001-0002-0003-0004-000000000005",
		"123456789-1-1-1-1":                      "23456789-0001-0001-0001-000000000001",
		"١-2-3-4-5":                              "00000001-0002-0003-0004-000000000005",
		"Ａ-2-3-4-5":                              "0000000a-0002-0003-0004-000000000005",
		"+1-2-3-4-5":                             "00000001-0002-0003-0004-000000000005",
		"1-2-3-4-7fffffffffffffff":               "00000001-0002-0003-0004-ffffffffffff",
		"{123e4567-e89b-12d3-a456-426614174000}": "",
		"urn:uuid:123e4567-e89b-12d3-a456-426614174000": "",
		"123e4567e89b12d3a456426614174000":              "",
		" 123e4567-e89b-12d3-a456-426614174000":         "",
		"123e4567-e89b-12d3-a456-4266141740000":         "",
		"1--3-4-5":                                      "",
		"1-2-3-4--5":                                    "",
		"1-2-3-4-8000000000000000":                      "",
		"1-2-3-4-10000000000000000":                     "",
		"not-a-uuid":                                    "",
	} {
		got, ok := ParseJavaUUID(in)
		switch {
		case want == "" && ok:
			t.Errorf("%q: accepted as %s, Java rejects", in, uuid.UUID(got))
		case want != "" && (!ok || uuid.UUID(got).String() != want):
			t.Errorf("%q: %v %s, want %s", in, ok, uuid.UUID(got), want)
		}
	}
}
