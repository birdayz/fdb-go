package yamsql

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// SQL_COVERAGE.md generation — the *measured* corpus coverage report (Ledger B
// of RFC-165). Where FEATURE_MATRIX.md inventories WHICH scenarios exist, this
// classifies every individual test case by its declared OUTCOME and reports how
// much of the corpus is a positive (rows-verified) assertion, how much pins an
// explicitly-unsupported feature, and how much pins correct error/constraint
// semantics. Like FEATURE_MATRIX it is generated from the corpus (so it cannot
// rot) and drift-guarded (TestSQLCoverageUpToDate). Static — no Docker.
//
// Classification is purely on TYPED outcome fields (Test.Rows / Test.ErrorCode),
// never on the SQL text (CLAUDE.md NO-TEXT-MATCHING). The three buckets are
// distinct and load-bearing:
//
//   - supported       — Test.ErrorCode == "": a positive result assertion
//     (rows verified, empty result, or a DML step that must succeed).
//   - unsupported     — Test.ErrorCode is a feature-gap SQLSTATE
//     (0A000 / 0AF00 / 0AF01 / 42883): the corpus pins that we cleanly REJECT a
//     feature we don't implement. An honest "we don't do this yet" pin, NOT a
//     bug and NOT an error-handling success.
//   - error-path      — any other Test.ErrorCode (42703 unknown column, 22003
//     overflow, 23505 unique violation, 42804 type mismatch, …): the corpus
//     pins CORRECT rejection / constraint semantics. This is supported
//     behaviour (we correctly reject bad input), not a feature gap — kept in its
//     own bucket so it is never confused with the "unsupported" gap count.

// unsupportedFeatureCodes are the SQLSTATEs the corpus uses to pin an
// explicitly-unsupported feature (vs. a correctly-rejected bad input). Class 0A
// is "feature not supported" (0A000 standard; 0AF00/0AF01 fdb-relational
// extensions); 42883 is undefined_function. These are the honest feature-gap
// pins — see classifyTest.
var unsupportedFeatureCodes = map[string]bool{
	"0A000": true, // feature_not_supported (e.g. COUNT(DISTINCT), UNION-distinct)
	"0AF00": true, // unsupported query/clause (e.g. LIMIT in a subquery)
	"0AF01": true, // unsupported feature variant
	"42883": true, // undefined_function (no function-catalog entry)
}

// Outcome is the coverage classification of a single test case.
type Outcome int

const (
	// OutcomeSupported is a positive assertion: rows verified, empty result, or
	// a DML step that must succeed (Test.ErrorCode == "").
	OutcomeSupported Outcome = iota
	// OutcomeUnsupported pins an explicitly-unsupported feature
	// (Test.ErrorCode in unsupportedFeatureCodes).
	OutcomeUnsupported
	// OutcomeErrorPath pins correct rejection/constraint semantics (any other
	// Test.ErrorCode).
	OutcomeErrorPath
)

// classifyTest buckets a single test by its declared outcome. Typed fields
// only — never the SQL text.
func classifyTest(t Test) Outcome {
	code := strings.TrimSpace(t.EffectiveErrorCode())
	if code == "" {
		return OutcomeSupported
	}
	if unsupportedFeatureCodes[strings.ToUpper(code)] {
		return OutcomeUnsupported
	}
	return OutcomeErrorPath
}

// CoverageBucket is the per-category (or total) case tally.
type CoverageBucket struct {
	Supported   int
	Unsupported int
	ErrorPath   int
}

// Total is supported + unsupported + error-path.
func (b CoverageBucket) Total() int { return b.Supported + b.Unsupported + b.ErrorPath }

// SupportedPct is the share of cases that are positive (rows-verified)
// assertions, rounded to one decimal. Returns 0 for an empty bucket.
func (b CoverageBucket) SupportedPct() float64 {
	if b.Total() == 0 {
		return 0
	}
	return float64(b.Supported) * 100 / float64(b.Total())
}

// CoverageReport is the parsed corpus coverage, by feature area + total.
type CoverageReport struct {
	ByCategory map[string]*CoverageBucket
	Total      CoverageBucket
	Scenarios  int
}

// ParseCoverage walks every *.yaml under dir and classifies every test case.
// Tolerant parse (no Load validation) so the inventory matches
// ParseFeatureScenarios exactly — one corpus file uses `schema:` and would fail
// Load but still belongs in the count.
func ParseCoverage(dir string) (*CoverageReport, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no scenarios found under %s", dir)
	}
	sort.Strings(matches)

	rep := &CoverageReport{ByCategory: map[string]*CoverageBucket{}}
	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		var s Scenario
		if err := yaml.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		name := s.Name
		if name == "" {
			name = strings.TrimSuffix(filepath.Base(path), ".yaml")
		}
		rep.Scenarios++
		b := rep.ByCategory[categoryFor(name)]
		if b == nil {
			b = &CoverageBucket{}
			rep.ByCategory[categoryFor(name)] = b
		}
		for _, t := range s.Tests {
			switch classifyTest(t) {
			case OutcomeSupported:
				b.Supported++
				rep.Total.Supported++
			case OutcomeUnsupported:
				b.Unsupported++
				rep.Total.Unsupported++
			case OutcomeErrorPath:
				b.ErrorPath++
				rep.Total.ErrorPath++
			}
		}
	}
	return rep, nil
}

// RenderCoverageReport renders SQL_COVERAGE.md from a parsed report.
func RenderCoverageReport(rep *CoverageReport) string {
	// Category display order: known order first (from categoryOrder), then any
	// extras alphabetically, then "Other" last — identical idiom to
	// RenderFeatureMatrix so the two docs group consistently.
	known := map[string]bool{}
	for _, c := range categoryOrder {
		known[c] = true
	}
	var cats []string
	for _, c := range categoryOrder {
		if c == "Other" {
			continue
		}
		if _, ok := rep.ByCategory[c]; ok {
			cats = append(cats, c)
		}
	}
	var extra []string
	for c := range rep.ByCategory {
		if !known[c] {
			extra = append(extra, c)
		}
	}
	sort.Strings(extra)
	cats = append(cats, extra...)
	if _, ok := rep.ByCategory["Other"]; ok {
		cats = append(cats, "Other")
	}

	var b strings.Builder
	b.WriteString("# SQL Coverage (measured)\n\n")
	b.WriteString("<!-- GENERATED FILE — DO NOT EDIT BY HAND.\n")
	b.WriteString("     Regenerate with `just sql-coverage` (or `go run ./cmd/gen-sql-coverage`).\n")
	b.WriteString("     Source: pkg/relational/conformance/yamsql/testdata/*.yaml. A drift guard\n")
	b.WriteString("     (TestSQLCoverageUpToDate) fails CI if this file is stale. -->\n\n")
	b.WriteString("Ledger B of RFC-165 — the **measured** corpus number. Every count is computed by\n")
	b.WriteString("walking the yamsql conformance corpus and classifying each test case by its declared\n")
	b.WriteString("outcome, so it cannot go stale. For the ANSI-standard scorecard see\n")
	b.WriteString("`SQL_ANSI_CONFORMANCE.md`; for the scenario inventory see `FEATURE_MATRIX.md`.\n\n")
	b.WriteString("**Buckets** (classified on typed outcome fields, never SQL text):\n")
	b.WriteString("- **supported** — a positive assertion (rows verified, empty result, or a DML step that must succeed).\n")
	b.WriteString("- **unsupported** — an explicitly-unsupported feature we cleanly reject (SQLSTATE `0A000`/`0AF00`/`0AF01`/`42883`).\n")
	b.WriteString("- **error-path** — correct rejection/constraint semantics (unknown column, overflow, unique violation, type mismatch, …): supported behaviour, not a gap.\n\n")

	fmt.Fprintf(&b, "**%d scenarios · %d test cases** — %d supported (%.1f%%), %d unsupported-feature pins, %d error-path pins.\n\n",
		rep.Scenarios, rep.Total.Total(), rep.Total.Supported, rep.Total.SupportedPct(),
		rep.Total.Unsupported, rep.Total.ErrorPath)

	b.WriteString("| Feature area | Cases | Supported | Unsupported | Error-path | Supported % |\n")
	b.WriteString("|---|--:|--:|--:|--:|--:|\n")
	for _, c := range cats {
		bb := rep.ByCategory[c]
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %.1f%% |\n",
			c, bb.Total(), bb.Supported, bb.Unsupported, bb.ErrorPath, bb.SupportedPct())
	}
	fmt.Fprintf(&b, "| **Total** | **%d** | **%d** | **%d** | **%d** | **%.1f%%** |\n",
		rep.Total.Total(), rep.Total.Supported, rep.Total.Unsupported, rep.Total.ErrorPath, rep.Total.SupportedPct())
	b.WriteString("\n")
	return b.String()
}

// GenerateCoverageReport parses the corpus under dir and renders SQL_COVERAGE.md.
func GenerateCoverageReport(dir string) (string, error) {
	rep, err := ParseCoverage(dir)
	if err != nil {
		return "", err
	}
	return RenderCoverageReport(rep), nil
}
