package unicode15_test

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"

	u "fdb.dev/internal/unicode15"
	"fdb.dev/internal/unicode15/text/collate"
	"fdb.dev/internal/unicode15/text/language"
	"fdb.dev/internal/unicode15/text/unicode/norm"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/tools/unicode15baseline/corpus"
)

func TestImmutableGo126Baseline(t *testing.T) {
	t.Parallel()
	wantBytes, err := os.ReadFile("../../tools/unicode15baseline/testdata/go1.26.10.json")
	if err != nil {
		t.Fatal(err)
	}
	const wantSHA256 = "212e81fd6c4323ce01de5f58a3219a69580dd4eabed90eaac608f1917e922dde"
	gotSHA256 := sha256.Sum256(wantBytes)
	if hex.EncodeToString(gotSHA256[:]) != wantSHA256 {
		t.Fatalf("immutable Go 1.26.10 baseline SHA-256 = %x, want %s", gotSHA256, wantSHA256)
	}
	got, err := corpus.Capture(corpus.Ops{
		Properties: func(r rune) uint32 {
			var flags uint32
			for i, table := range []*u.RangeTable{u.L, u.Nd, u.Mn, u.Mc, u.Me, u.Lu, u.Ll, u.Lt, u.Other_Lowercase, u.Other_Uppercase, u.Ideographic, u.White_Space} {
				if u.Is(table, r) {
					flags |= 1 << i
				}
			}
			return flags
		},
		Lower: u.ToLower, Upper: u.ToUpper, Fold: u.SimpleFold,
		Normalize: func(form int, s string) string { return norm.Form(form).String(s) },
		LowerText: u.ToLowerString, UpperText: u.ToUpperString, EqualFold: u.EqualFold,
		Tokens: func(s string) ([]string, error) {
			return recordlayer.DefaultTextTokenizerInstance().TokenizeToList(s, 0, recordlayer.TokenizerModeIndex)
		},
		Collate: func(locale string, strength int, s string) []byte {
			tag := language.Und
			if locale != "" {
				tag = language.Make(strings.ReplaceAll(locale, "_", "-"))
			}
			var opts []collate.Option
			if strength == 0 {
				opts = append(opts, collate.Loose)
			} else if strength == 1 {
				opts = append(opts, collate.IgnoreCase)
			}
			var b collate.Buffer
			return collate.New(tag, opts...).KeyFromString(&b, s)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// JSON canonicalizes malformed UTF-8 sample labels; the digest still hashes
	// their original bytes and distinguishes replacement from preservation.
	gotBytes, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var canonicalGot, want corpus.Report
	if err := json.Unmarshal(gotBytes, &canonicalGot); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(wantBytes, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(canonicalGot, want) {
		t.Errorf("Unicode 15 baseline changed on %s:\ngot %+v\nwant %+v", runtime.Version(), canonicalGot, want)
	}
	if got.CodePoints != 1114112 || got.Scalars != 1112064 || got.Texts != 4186 || got.CollationKeys != 2088 {
		t.Fatalf("baseline population changed: %+v", got)
	}
	t.Logf("compiler=%s unicode=%s codepoints=%d scalars=%d texts=%d collation_keys=%d", runtime.Version(), u.Version, got.CodePoints, got.Scalars, got.Texts, got.CollationKeys)
}

func TestUCDNormalization(t *testing.T) {
	t.Parallel()
	f, err := os.Open("ucd/NormalizationTest.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	count := 0
	for scanner.Scan() {
		body, _, _ := strings.Cut(scanner.Text(), "#")
		fields := strings.Split(body, ";")
		if len(fields) < 5 {
			continue
		}
		var columns [5]string
		for i := range columns {
			var runes []rune
			for _, cp := range strings.Fields(fields[i]) {
				v, err := strconv.ParseUint(cp, 16, 32)
				if err != nil {
					t.Fatal(err)
				}
				runes = append(runes, rune(v))
			}
			columns[i] = string(runes)
		}
		for i, source := range columns {
			nfc, nfd := columns[1], columns[2]
			if i >= 3 {
				nfc, nfd = columns[3], columns[4]
			}
			for _, tc := range []struct {
				form norm.Form
				want string
			}{{norm.NFC, nfc}, {norm.NFD, nfd}, {norm.NFKC, columns[3]}, {norm.NFKD, columns[4]}} {
				if got := tc.form.String(source); got != tc.want {
					t.Fatalf("normalization row %d column %d form %d: %U != %U", count, i, tc.form, []rune(got), []rune(tc.want))
				}
			}
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 19074 {
		t.Fatalf("normalization population=%d, want 19074", count)
	}
	t.Logf("Unicode %s normalization rows=%d", u.Version, count)
}

func TestSimpleMappingBoundaries(t *testing.T) {
	t.Parallel()
	for _, r := range []rune{-1, 0x110000} {
		if u.IsLetter(r) || u.IsDigit(r) || u.ToLower(r) != r || u.SimpleFold(r) != r {
			t.Errorf("out-of-range rune %d changed", r)
		}
	}
	if !u.IsLetter('A') || !u.IsDigit('0') || !u.Is(u.Mn, 0x301) || !u.Is(u.Ll, 0x295) {
		t.Fatal("Unicode 15 positive controls failed")
	}
	for _, r := range []rune{0x1c89, 0xa7cb, 0xa7ce, 0x1acf, 0x18cff} {
		if u.IsLetter(r) || u.Is(u.Mn, r) || u.Is(u.Ideographic, r) {
			t.Errorf("post-15 assignment admitted: %U", r)
		}
	}
	if u.ToUpper(0x264) != 0x264 || u.ToUpper(0xa7d3) != 0xa7d3 || u.ToLower(0xa7cb) != 0xa7cb || u.EqualFold("\u0130", "i") || u.EqualFold("\ufb05", "\ufb06") {
		t.Fatal("simple casing/folding policy changed")
	}
	if u.ToLowerString("\u0130") != "i" || u.ToUpperString("\u0131d") != "ID" || u.ToUpperString("\u017felect") != "SELECT" {
		t.Fatal("pre-existing simple casing exceptions changed")
	}
	if u.ToLowerString(strings.Repeat("ABcd", 256)) != strings.Repeat("abcd", 256) ||
		u.ToUpperString(strings.Repeat("ABcd", 256)) != strings.Repeat("ABCD", 256) {
		t.Fatal("long ASCII casing changed")
	}
}
