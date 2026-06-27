package fdb_test

import (
	_ "embed"
	"regexp"
	"strings"
	"testing"
)

// RFC-133 anti-rot guard for the public option matrix (OPTIONS.md). The matrix classifies every
// client option as honored / UnsupportedOptionError / safe no-op, verified against libfdb_c 7.3.77.
// The behavioural contracts themselves are pinned elsewhere — the unsafe family must error
// (fdb_test.go's *_RejectUnsupportedOptions / DB-default tests) and honored options take effect
// (options_internal_test.go). This guard only ensures the *documentation* can't silently fall behind
// the code: every Set* option method must have a row in OPTIONS.md. Name-presence only — it does not
// parse the table columns or validate the C++ citations (Torvalds: that's the fragile part).

//go:embed options.go
var optionsSrc string

//go:embed OPTIONS.md
var optionsMatrixDoc string

// every Set* method on the two option types (the public option surface).
var optionSetterRe = regexp.MustCompile(`func \(o (?:goTransactionOptions|DatabaseOptions)\) (Set\w+)\(`)

func TestOptionMatrix_DocumentsEveryOption(t *testing.T) {
	t.Parallel()
	matches := optionSetterRe.FindAllStringSubmatch(optionsSrc, -1)
	if len(matches) == 0 {
		t.Fatal("no Set* option methods found in options.go — the embed or the regex is broken")
	}
	seen := make(map[string]bool, len(matches))
	for _, m := range matches {
		name := m[1]
		if seen[name] {
			continue // tx/db twins share a method name; one row covers both
		}
		seen[name] = true
		if !strings.Contains(optionsMatrixDoc, name) {
			t.Errorf("option method %q has no row in OPTIONS.md — every Set* option must be classified "+
				"(honored / UnsupportedOptionError / safe no-op) in the public matrix (RFC-133)", name)
		}
	}
}
