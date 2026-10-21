// Package corpus defines the immutable RFC-265 baseline population.
package corpus

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"strconv"
	"unicode/utf8"
)

// Ops separates the old compiler's implementation from the pinned implementation.
type Ops struct {
	Properties func(rune) uint32
	Lower      func(rune) rune
	Upper      func(rune) rune
	Fold       func(rune) rune
	Normalize  func(int, string) string
	LowerText  func(string) string
	UpperText  func(string) string
	EqualFold  func(string, string) bool
	Tokens     func(string) ([]string, error)
	Collate    func(string, int, string) []byte
}

type Sample struct {
	Input  string
	Lower  string
	Upper  string
	Tokens []string
}

type Report struct {
	CodePoints          int
	Scalars             int
	Texts               int
	CollationKeys       int
	PropertiesSHA256    string
	NormalizationSHA256 string
	TextSHA256          string
	CollationSHA256     string
	Samples             []Sample
	FoldPairs           []bool
}

// Texts includes old assigned characters, every upgrade witness and existing
// Java divergences. Escapes keep the exact corpus independent of font rendering.
var Texts = []string{
	"", "A", "a", "ASCII 0123", "caf\u00e9 na\u00efve co\u00f6perate", "\u0130stanbul \u0131i I", "Stra\u00dfe STRASSE \u00df",
	"\u039f\u0394\u039f\u03a3 \u03a3\u039f\u03a6\u039f\u03a3 \u03a3 \u03a3\u0391 \u0391\u03a3. \u0391\u03a3'\u03a3", "\u1f48\u03b4\u03c5\u03c3\u03c3\u03b5\u03cd\u03c2", "\u0391\u03a3\u037a", "\u013f\u03a3", "\u0391\u03a3\ufe70\u0392", "\U00010780\u03a3", "A\u03a3\u0295",
	"x:y", "foo_bar __init__ a_1 _", "\u4e2d\u534e\u4eba\u6c11\u5171\u548c\u56fd", "\ud55c\uad6d\uc5b4 \ubb38\uc7a5\uc785\ub2c8\ub2e4", "\u0645\u0631\u062d\u0628\u0627 \u0628\u0627\u0644\u0639\u0627\u0644\u0645", "\u05e9\u05dc\u05d5\u05dd \u05e2\u05d5\u05dc\u05dd", "\u0928\u092e\u0938\u094d\u0924\u0947 \u0926\u0941\u0928\u093f\u092f\u093e", "\u041f\u0440\u0438\u0432\u0435\u0442 \u043c\u0438\u0440 \u0401\u0416",
	"\u1c89", "\ua7cb", "\ua7ce", "\ua7f1", "\U0001ccd6", "\U0001ccf0", "\u1acf", "\u0897", "\u0264", "\u0295", "\ufb05", "\ufb06", "\u0390", "\u1fd3", "\u03b0", "\u1fe3", "\ua7d3", "\ua7d5", "\U00018cff", "\U000323b0", "\U0002ebf0", "\U00010d40", "\U00011de0", "\U00010d50", "\U00016ea0", "\U00010414", "\U0001d400\U0001d401\U0001d402 \U0001d7d9\U0001d7da", "\u216b \u217b \u2103", "\ufb01nance \ufb02 \u01c5 \u01c6", "\uff21\uff22\uff23\uff11\uff12\uff13 \uff76\uff80\uff76\uff85", "\u0131d", "\u017felect", "\"\u0131d\"", "a\xffb", "\xfe\xff", "\xed\xa0\x80",
}

var Locales = []string{"", "en", "de", "tr", "sv", "zh", "ja", "en-u-kn-true", "de_DE", "zh-Hant-TW", "xx", "\u0131"}

var FoldPairs = [][2]string{{"A", "a"}, {"\u0130", "i"}, {"\u0131", "I"}, {"\u0264", "\ua7cb"}, {"\u1c89", "\u1c8a"}, {"\ufb05", "\ufb06"}, {"\u0390", "\u1fd3"}, {"\u03b0", "\u1fe3"}, {"\u212a", "K"}, {"\u03c2", "\u03c3"}}

func frame(h hash.Hash, s string) {
	var n [8]byte
	binary.LittleEndian.PutUint64(n[:], uint64(len(s)))
	h.Write(n[:])
	h.Write([]byte(s))
}

func sum(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }

func Capture(o Ops) (Report, error) {
	r := Report{}
	properties, normalization, text, collation := sha256.New(), sha256.New(), sha256.New(), sha256.New()
	var row [20]byte
	for cp := rune(0); cp <= utf8.MaxRune; cp++ {
		binary.LittleEndian.PutUint32(row[0:4], uint32(cp))
		binary.LittleEndian.PutUint32(row[4:8], o.Properties(cp))
		binary.LittleEndian.PutUint32(row[8:12], uint32(o.Lower(cp)))
		binary.LittleEndian.PutUint32(row[12:16], uint32(o.Upper(cp)))
		binary.LittleEndian.PutUint32(row[16:20], uint32(o.Fold(cp)))
		properties.Write(row[:])
		r.CodePoints++
		if cp >= 0xd800 && cp <= 0xdfff {
			continue
		}
		r.Scalars++
		for form := 0; form < 4; form++ {
			frame(normalization, o.Normalize(form, string(cp)))
		}
	}
	texts := append([]string(nil), Texts...)
	for _, cp := range []rune{0x1c89, 0xa7cb, 0xa7ce, 0xa7f1, 0x1ccd6, 0x1acf, 0x0897, 0x18cff} {
		texts = append(texts, "left "+string(cp)+" right", "A"+string(cp)+"\u03a3", "A\u03a3"+string(cp))
	}
	for _, count := range []int{29, 30, 31, 61} {
		s := "A"
		for i := 0; i < count; i++ {
			s += "\u0301"
		}
		texts = append(texts, s, s+"\u03a3")
	}
	// A fixed LCG population, not a host-version-dependent random generator.
	seed := uint32(265)
	alphabet := []rune{'A', 'a', '\u03a3', '\u0295', '\u0301', '\u1acf', '\u1c89', '\ua7f1', 0x1ccd6, '\u0130', '\u013f', '\u037a', ' ', '_', ':', 0x18cff, '\u1100', '\u1161', '\u11a8'}
	for i := 0; i < 4096; i++ {
		var s []rune
		for j := 0; j < i%31+1; j++ {
			seed = seed*1664525 + 1013904223
			s = append(s, alphabet[int(seed)%len(alphabet)])
		}
		texts = append(texts, string(s))
	}
	for _, s := range texts {
		tokens, err := o.Tokens(s)
		if err != nil {
			return r, err
		}
		frame(text, s)
		frame(text, o.LowerText(s))
		frame(text, o.UpperText(s))
		frame(text, strconv.Itoa(len(tokens)))
		for _, token := range tokens {
			frame(text, token)
		}
		for form := 0; form < 4; form++ {
			frame(normalization, o.Normalize(form, s))
		}
		r.Texts++
	}
	for _, s := range Texts {
		tokens, err := o.Tokens(s)
		if err != nil {
			return r, err
		}
		r.Samples = append(r.Samples, Sample{Input: s, Lower: o.LowerText(s), Upper: o.UpperText(s), Tokens: tokens})
		for _, locale := range Locales {
			for strength := 0; strength < 3; strength++ {
				frame(collation, string(o.Collate(locale, strength, s)))
				r.CollationKeys++
			}
		}
	}
	for _, pair := range FoldPairs {
		r.FoldPairs = append(r.FoldPairs, o.EqualFold(pair[0], pair[1]))
	}
	r.PropertiesSHA256, r.NormalizationSHA256, r.TextSHA256, r.CollationSHA256 = sum(properties), sum(normalization), sum(text), sum(collation)
	return r, nil
}
