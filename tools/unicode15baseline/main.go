// unicode15baseline captures the pre-pin implementation with its original SDK.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"unicode"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
	"golang.org/x/text/unicode/norm"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/tools/unicode15baseline/corpus"
)

func main() {
	if runtime.Version() != "go1.26.10" || unicode.Version != "15.0.0" {
		fmt.Fprintln(os.Stderr, "baseline requires Go 1.26.10 / Unicode 15.0.0; got", runtime.Version(), unicode.Version)
		os.Exit(1)
	}
	report, err := corpus.Capture(corpus.Ops{
		Properties: func(r rune) uint32 {
			var flags uint32
			for i, table := range []*unicode.RangeTable{unicode.L, unicode.Nd, unicode.Mn, unicode.Mc, unicode.Me, unicode.Lu, unicode.Ll, unicode.Lt, unicode.Other_Lowercase, unicode.Other_Uppercase, unicode.Ideographic, unicode.White_Space} {
				if unicode.Is(table, r) {
					flags |= 1 << i
				}
			}
			return flags
		},
		Lower: unicode.ToLower, Upper: unicode.ToUpper, Fold: unicode.SimpleFold,
		Normalize: func(form int, s string) string { return norm.Form(form).String(s) },
		LowerText: strings.ToLower, UpperText: strings.ToUpper, EqualFold: strings.EqualFold,
		Tokens: func(s string) ([]string, error) {
			return recordlayer.DefaultTextTokenizerInstance().TokenizeToList(s, 0, recordlayer.TokenizerModeIndex)
		},
		Collate: func(locale string, strength int, s string) []byte {
			tag := language.Und
			if locale != "" {
				tag = language.Make(strings.ReplaceAll(locale, "_", "-"))
			}
			var options []collate.Option
			if strength == 0 {
				options = append(options, collate.Loose)
			} else if strength == 1 {
				options = append(options, collate.IgnoreCase)
			}
			var b collate.Buffer
			return collate.New(tag, options...).KeyFromString(&b, s)
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "compiler="+runtime.Version(), "unicode="+unicode.Version, "normalization="+norm.Version, "collation="+collate.UnicodeVersion)
	e := json.NewEncoder(os.Stdout)
	e.SetIndent("", "  ")
	if err := e.Encode(report); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
