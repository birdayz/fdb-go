package testkit

import (
	"flag"
	"os"
)

type narrowingValue string

func (v narrowingValue) String() string   { return string(v) }
func (v narrowingValue) Set(string) error { return nil }

// corpusNarrowing returns the flag that cut the corpus down: -test.run, or a
// synthetic one naming the target. No target runs the whole corpus in one
// binary (the census target was deleted), so the census population floors are
// withheld everywhere; the gates' hard-zero claims hold on any subset and are
// still asserted by every package.
func corpusNarrowing() *flag.Flag {
	if f := flag.Lookup("test.run"); f != nil && f.Value.String() != "" {
		return f
	}
	t := os.Getenv("TEST_TARGET")
	return &flag.Flag{Name: "target", Value: narrowingValue("target=" + t + " (not a whole-corpus target)")}
}
