package testkit

import (
	"flag"
	"os"
)

type narrowingValue string

func (v narrowingValue) String() string   { return string(v) }
func (v narrowingValue) Set(string) error { return nil }

// corpusNarrowing returns the flag that cut the corpus down: -test.run, or a
// synthetic one when this binary is not a whole-corpus target (one package of
// the split suite, or a plain `go test`). Nil means the floors hold.
func corpusNarrowing() *flag.Flag {
	if f := flag.Lookup("test.run"); f != nil && f.Value.String() != "" {
		return f
	}
	if t := os.Getenv("TEST_TARGET"); t != CensusTarget {
		return &flag.Flag{Name: "target", Value: narrowingValue("target=" + t + " (not a whole-corpus target)")}
	}
	return nil
}
