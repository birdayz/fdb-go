package recordlayer

import (
	"testing"

	. "github.com/onsi/gomega"
)

func TestAsInt64(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	tests := []struct {
		name     string
		input    any
		expected int64
		ok       bool
	}{
		{name: "int64", input: int64(42), expected: 42, ok: true},
		{name: "int32", input: int32(10), expected: 10, ok: true},
		{name: "int", input: int(7), expected: 7, ok: true},
		{name: "float64", input: float64(3.14), expected: 0, ok: false},
		{name: "string", input: "str", expected: 0, ok: false},
		{name: "nil", input: nil, expected: 0, ok: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			val, ok := asInt64(tc.input)
			g.Expect(ok).To(Equal(tc.ok))
			g.Expect(val).To(Equal(tc.expected))
		})
	}
}
