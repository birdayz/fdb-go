package javahashmap

import (
	"slices"
	"testing"
)

func TestOrder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		first, then []string
		want        []string
	}{
		{"empty", nil, nil, []string{}},
		// 'p' hashes to bucket 0 at capacity 16, bucket 16 at capacity 32.
		{"default capacity", nil, []string{"p", "a"}, []string{"p", "a"}},
		{"duplicate put", nil, []string{"p", "a", "p"}, []string{"p", "a"}},
		{
			"putAll presizes",
			[]string{"p", "a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"},
			nil,
			[]string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "p"},
		},
		{
			"put resizes", nil,
			[]string{"p", "a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"},
			[]string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l", "p"},
		},
		// These strings have the same Java hash and retain insertion order.
		{"collision chain", nil, []string{"BB", "Aa"}, []string{"BB", "Aa"}},
		{"UTF16", nil, []string{"\U0001F600", "\U0001D11E"}, []string{"\U0001D11E", "\U0001F600"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Order(tc.first, tc.then); !slices.Equal(got, tc.want) {
				t.Fatalf("Order(%v, %v) = %v, want %v", tc.first, tc.then, got, tc.want)
			}
		})
	}
}
