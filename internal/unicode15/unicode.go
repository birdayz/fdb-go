// Package unicode15 pins compatibility-sensitive Unicode data to version 15.0.0.
package unicode15

import (
	"strings"
	"unicode/utf8"
)

type (
	Range16 struct{ Lo, Hi, Stride uint16 }
	Range32 struct{ Lo, Hi, Stride uint32 }
)

type RangeTable struct {
	R16 []Range16
	R32 []Range32
}

func Is(table *RangeTable, r rune) bool {
	if r < 0 || r > utf8.MaxRune {
		return false
	}
	if r <= 0xffff {
		x := uint16(r)
		lo, hi := 0, len(table.R16)
		for lo < hi {
			mid := lo + (hi-lo)/2
			rg := table.R16[mid]
			if x < rg.Lo {
				hi = mid
			} else if x > rg.Hi {
				lo = mid + 1
			} else {
				return (x-rg.Lo)%rg.Stride == 0
			}
		}
		return false
	}
	x := uint32(r)
	lo, hi := 0, len(table.R32)
	for lo < hi {
		mid := lo + (hi-lo)/2
		rg := table.R32[mid]
		if x < rg.Lo {
			hi = mid
		} else if x > rg.Hi {
			lo = mid + 1
		} else {
			return (x-rg.Lo)%rg.Stride == 0
		}
	}
	return false
}

func In(r rune, tables ...*RangeTable) bool {
	for _, table := range tables {
		if Is(table, r) {
			return true
		}
	}
	return false
}

func IsLetter(r rune) bool { return Is(L, r) }
func IsDigit(r rune) bool  { return Is(Nd, r) }
func IsUpper(r rune) bool  { return Is(Lu, r) }
func IsLower(r rune) bool  { return Is(Ll, r) }
func IsTitle(r rune) bool  { return Is(Lt, r) }

func ToLower(r rune) rune {
	if r < utf8.RuneSelf {
		if 'A' <= r && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}
	if mapped, ok := lower[r]; ok {
		return mapped
	}
	return r
}

func ToUpper(r rune) rune {
	if r < utf8.RuneSelf {
		if 'a' <= r && r <= 'z' {
			return r - ('a' - 'A')
		}
		return r
	}
	if mapped, ok := upper[r]; ok {
		return mapped
	}
	return r
}

func SimpleFold(r rune) rune {
	if mapped, ok := fold[r]; ok {
		return mapped
	}
	return r
}

// These are simple mappings, not Java's full/contextual String casing.
func ToLowerString(s string) string {
	changed := false
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return strings.Map(ToLower, s)
		}
		changed = changed || 'A' <= s[i] && s[i] <= 'Z'
	}
	if !changed {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		b.WriteByte(c)
	}
	return b.String()
}

func ToUpperString(s string) string {
	changed := false
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return strings.Map(ToUpper, s)
		}
		changed = changed || 'a' <= s[i] && s[i] <= 'z'
	}
	if !changed {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'a' <= c && c <= 'z' {
			c -= 'a' - 'A'
		}
		b.WriteByte(c)
	}
	return b.String()
}

func EqualFold(a, b string) bool {
	for a != "" && b != "" {
		r, n := utf8.DecodeRuneInString(a)
		s, m := utf8.DecodeRuneInString(b)
		a, b = a[n:], b[m:]
		if r == s {
			continue
		}
		if r < utf8.RuneSelf && s < utf8.RuneSelf {
			if ToLower(r) != ToLower(s) {
				return false
			}
			continue
		}
		if r > s {
			r, s = s, r
		}
		f := SimpleFold(r)
		for f != r && f < s {
			f = SimpleFold(f)
		}
		if f != s {
			return false
		}
	}
	return a == b
}
