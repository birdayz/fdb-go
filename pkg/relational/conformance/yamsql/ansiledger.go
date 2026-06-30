package yamsql

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// SQL_ANSI_CONFORMANCE.md generation — Ledger A of RFC-165: the ANSI-standard
// scorecard, derived (not hand-asserted) so it cannot rot like the
// SQL_CONFORMANCE.md it replaces.
//
// Anti-rot model (RFC-165 §4):
//   - HAND-AUTHORED = pinned facts only: the ISO roster (Identifier|Core?|Name,
//     pinned to SQL:2023 Core) and the `Java?` column (a fact about the FROZEN
//     fdb-relational 4.12.11.0 reference). Both live in ansi_roster.go.
//   - DERIVED from the corpus: `Go?` and completeness come from `# ansi:` /
//     `# ansi-gap:` tags on yamsql scenarios + each scenario's typed outcome
//     (reusing classifyTest). A "supported" claim therefore traces to a named,
//     tagged, *passing* corpus case — never a hand-typed status. The live FDB/A3
//     lanes prove those cases actually pass.
//
// Two independent axes (RFC-165 §4.2): `Java?` × `Go?`. Their cross-product is
// what routes work:
//   - (yes, yes)        shared parity
//   - (no,  yes)        Go-only extension (wire-safe by construction)
//   - (no,  no)         shared ANSI gap          → RFC-165 read-side backlog
//   - (yes, no/partial) port-fidelity divergence → RFC-164 bug
// The headline is keyed on `Go?` ONLY (RFC-165 §4.6) — a Java-only feature is
// never counted as Go-supported.

// Support is a feature's support level on one axis (Java or Go).
type Support int

const (
	// SupportNone: tried and not supported (Go: an explicit unsupported pin;
	// Java: known-absent in 4.12.11.0).
	SupportNone Support = iota
	// SupportPartial: some subfeatures supported, others not.
	SupportPartial
	// SupportFull: fully supported.
	SupportFull
	// SupportUntested (Go only): no tagged corpus evidence yet — distinct from
	// SupportNone (which means we have a pin proving rejection). Counts as
	// not-supported for the headline, but flagged so the gap backlog can tell
	// "we reject it" from "we haven't checked."
	SupportUntested
)

func (s Support) String() string {
	switch s {
	case SupportFull:
		return "yes"
	case SupportPartial:
		return "partial"
	case SupportUntested:
		return "untested"
	default:
		return "no"
	}
}

func (s Support) supported() bool { return s == SupportFull || s == SupportPartial }

// AnsiFeature is one roster row. ID/Name/Core/Subfeatures are pinned facts
// (PostgreSQL Appendix D); Java is a fact about frozen 4.12.11.0. Go is left
// zero (derived). Defined alongside the data in ansi_roster.go conceptually, but
// the type lives here next to the engine that consumes it.
type AnsiFeature struct {
	ID          string // e.g. "E051" or subfeature "E091-07"
	Name        string
	Core        bool
	Java        Support  // pinned fact about 4.12.11.0
	Subfeatures []string // child IDs whose rollup defines this feature's completeness (optional)
	// NA marks a Core feature that is structurally outside an embedded
	// record-layer SQL surface (host-language binding, cursors, modules, table
	// privileges, SQL-invoked routines). Such features are kept in the roster
	// (so the 176 denominator is not silently shopped down) but reported in a
	// separate "N/A (out of engine scope)" population — never as a roadmap gap.
	NA   bool
	Note string
}

// ansiTagEvidence is the per-ID corpus evidence derived from tags.
type ansiTagEvidence struct {
	positive []string // scenario names tagged `# ansi: ID` with a passing test
	gaps     []string // scenario names tagged `# ansi-gap: ID` with an unsupported pin
}

// parseAnsiTags scans a scenario file's comment lines for `# ansi: <ID>` and
// `# ansi-gap: <ID>` markers (comma-separated IDs allowed) and returns the two
// ID lists. Comment lines only (a line whose first non-space rune is '#') so a
// tag never matches inside SQL text — CLAUDE.md NO-TEXT-MATCHING: we read our own
// typed tag convention, not the SQL.
func parseAnsiTags(raw []byte) (positive, gaps []string) {
	for _, ln := range strings.Split(string(raw), "\n") {
		t := strings.TrimSpace(strings.TrimRight(ln, "\r"))
		if !strings.HasPrefix(t, "#") {
			continue
		}
		body := strings.TrimSpace(strings.TrimPrefix(t, "#"))
		switch {
		case strings.HasPrefix(body, "ansi-gap:"):
			gaps = append(gaps, splitIDs(strings.TrimPrefix(body, "ansi-gap:"))...)
		case strings.HasPrefix(body, "ansi:"):
			positive = append(positive, splitIDs(strings.TrimPrefix(body, "ansi:"))...)
		}
	}
	return positive, gaps
}

func splitIDs(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		if f != "" {
			out = append(out, strings.ToUpper(f))
		}
	}
	return out
}

// scenarioHasOutcome reports whether any of a scenario's tests classify to want.
func scenarioHasOutcome(s Scenario, want Outcome) bool {
	for _, t := range s.Tests {
		if classifyTest(t) == want {
			return true
		}
	}
	return false
}

// collectAnsiEvidence walks testdata/*.yaml and builds per-ID evidence. A
// `# ansi: ID` tag contributes positive evidence iff the scenario actually has a
// supported test; a `# ansi-gap: ID` tag contributes gap evidence iff the
// scenario actually has an unsupported pin. This is the exercised-not-exists
// guard (RFC-165 §4.3): a tag with no matching outcome is reported as an error,
// not silently accepted.
func collectAnsiEvidence(dir string) (map[string]*ansiTagEvidence, []string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(matches)
	ev := map[string]*ansiTagEvidence{}
	var violations []string
	get := func(id string) *ansiTagEvidence {
		e := ev[id]
		if e == nil {
			e = &ansiTagEvidence{}
			ev[id] = e
		}
		return e
	}
	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", path, err)
		}
		pos, gaps := parseAnsiTags(raw)
		if len(pos) == 0 && len(gaps) == 0 {
			continue
		}
		var s Scenario
		if err := yaml.Unmarshal(raw, &s); err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", path, err)
		}
		name := s.Name
		if name == "" {
			name = strings.TrimSuffix(filepath.Base(path), ".yaml")
		}
		hasPos := scenarioHasOutcome(s, OutcomeSupported)
		hasGap := scenarioHasOutcome(s, OutcomeUnsupported)
		for _, id := range pos {
			if !hasPos {
				violations = append(violations, fmt.Sprintf("%s: `# ansi: %s` but scenario has no supported (positive) test", name, id))
				continue
			}
			get(id).positive = append(get(id).positive, name)
		}
		for _, id := range gaps {
			if !hasGap {
				violations = append(violations, fmt.Sprintf("%s: `# ansi-gap: %s` but scenario has no unsupported-feature pin", name, id))
				continue
			}
			get(id).gaps = append(get(id).gaps, name)
		}
	}
	sort.Strings(violations)
	return ev, violations, nil
}

// deriveGo computes the Go support level for one ID from its direct evidence.
// Conflicting evidence (both positive and gap tags) → Partial.
func deriveGo(e *ansiTagEvidence) Support {
	if e == nil {
		return SupportUntested
	}
	switch {
	case len(e.positive) > 0 && len(e.gaps) > 0:
		return SupportPartial
	case len(e.positive) > 0:
		return SupportFull
	case len(e.gaps) > 0:
		return SupportNone
	default:
		return SupportUntested
	}
}

// rollupGo computes a parent feature's Go support: direct evidence on the parent
// ID, else the rollup of its declared subfeatures (Full iff all Full; Partial if
// mixed/any partial; None iff all None; Untested iff none have evidence).
func rollupGo(f AnsiFeature, ev map[string]*ansiTagEvidence) (Support, []string) {
	direct := deriveGo(ev[f.ID])
	var evidence []string
	if e := ev[f.ID]; e != nil {
		evidence = append(evidence, e.positive...)
	}
	if len(f.Subfeatures) == 0 {
		return direct, evidence
	}
	var full, partial, none, untested int
	for _, sub := range f.Subfeatures {
		s := deriveGo(ev[sub])
		if e := ev[sub]; e != nil {
			evidence = append(evidence, e.positive...)
		}
		switch s {
		case SupportFull:
			full++
		case SupportPartial:
			partial++
		case SupportNone:
			none++
		default:
			untested++
		}
	}
	// Fold in any direct parent-level positive evidence as a "full" vote.
	if direct == SupportFull {
		full++
	} else if direct == SupportNone {
		none++
	} else if direct == SupportPartial {
		partial++
	}
	sort.Strings(evidence)
	switch {
	case full == 0 && partial == 0 && none == 0:
		return SupportUntested, evidence
	case partial == 0 && none == 0 && untested == 0:
		return SupportFull, evidence
	case full == 0 && partial == 0 && untested == 0:
		return SupportNone, evidence
	default:
		return SupportPartial, evidence
	}
}

// AnsiRow is a fully-resolved ledger row (facts + derived Go).
type AnsiRow struct {
	AnsiFeature
	Go       Support
	Evidence []string
}

// classify routes the row by the (Java?, Go?) cross-product (RFC-165 §4.2/§4.6).
type ansiPopulation int

const (
	popSharedParity ansiPopulation = iota // Java yes, Go yes
	popGoExt                              // Java no,  Go yes
	popSharedGap                          // Java no,  Go no (explicit reject pin)
	popDivergence                         // Java yes, Go no (explicit reject pin) → RFC-164 bug
	popUntested                           // Go has no tagged evidence yet — unknown, NOT a divergence
)

// population routes a row by (Java?, Go?). A row with no Go evidence is
// popUntested — explicitly NOT a divergence: a divergence requires an actual Go
// rejection pin (SupportNone) proving Go diverges from a Java-supported feature,
// not the mere absence of a tag.
func (r AnsiRow) population() ansiPopulation {
	if r.Go == SupportUntested {
		return popUntested
	}
	switch {
	case r.Java.supported() && r.Go.supported():
		return popSharedParity
	case !r.Java.supported() && r.Go.supported():
		return popGoExt
	case r.Java.supported() && !r.Go.supported():
		return popDivergence
	default:
		return popSharedGap
	}
}

// AnsiLedger is the resolved roster + derived populations.
type AnsiLedger struct {
	Rows       []AnsiRow
	Violations []string // exercised-not-exists tag violations (a non-empty list = drift-guard failure)
}

// BuildAnsiLedger resolves the hand-authored roster against the corpus-derived
// `# ansi:` evidence under dir.
func BuildAnsiLedger(dir string, roster []AnsiFeature) (*AnsiLedger, error) {
	ev, violations, err := collectAnsiEvidence(dir)
	if err != nil {
		return nil, err
	}
	l := &AnsiLedger{Violations: violations}
	for _, f := range roster {
		goSupp, evidence := rollupGo(f, ev)
		l.Rows = append(l.Rows, AnsiRow{AnsiFeature: f, Go: goSupp, Evidence: dedup(evidence)})
	}
	return l, nil
}

func dedup(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// RenderAnsiLedger renders SQL_ANSI_CONFORMANCE.md.
func RenderAnsiLedger(l *AnsiLedger) string {
	// Headline keyed on Go? only (RFC-165 §4.6). Count every Core row (parent +
	// subfeature), matching PostgreSQL Appendix D's published methodology
	// ("Go conforms to N of M"), excluding only NA rows from the applicable
	// denominator (but reporting them so the 176 total is not silently shopped).
	var coreTotal, naCount int
	var goSupported, sharedGap, divergence, goExt, sharedParity, untested int
	for _, r := range l.Rows {
		if !r.Core {
			continue
		}
		coreTotal++
		if r.NA {
			naCount++
			continue
		}
		switch r.population() {
		case popSharedParity:
			sharedParity++
			goSupported++
		case popGoExt:
			goExt++
			goSupported++
		case popSharedGap:
			sharedGap++
		case popDivergence:
			divergence++
		case popUntested:
			untested++
		}
	}
	applicable := coreTotal - naCount

	var b strings.Builder
	b.WriteString("# SQL ANSI Conformance (SQL:2023 Core)\n\n")
	b.WriteString("<!-- GENERATED FILE — DO NOT EDIT BY HAND.\n")
	b.WriteString("     Regenerate with `just sql-coverage` (or `go run ./cmd/gen-sql-coverage`).\n")
	b.WriteString("     The roster (Identifier/Core?/Name/Java?) is the hand-authored PINNED-FACT source\n")
	b.WriteString("     in ansi_roster.go; the Go? column + completeness are DERIVED from `# ansi:` tags\n")
	b.WriteString("     on the yamsql corpus. Drift guards: TestAnsiLedgerUpToDate + TestAnsiLedgerEvidenceExists. -->\n\n")
	b.WriteString("Ledger A of RFC-165 — the ANSI-standard scorecard, modeled on PostgreSQL Appendix D.\n")
	b.WriteString("The `Java?`/`Core?`/`Identifier`/`Name` columns are pinned facts (SQL:2023 Core; the\n")
	b.WriteString("frozen fdb-relational 4.12.11.0 reference). **`Go?` and completeness are derived** by\n")
	b.WriteString("walking `# ansi:`-tagged corpus scenarios — a claim of support traces to a named,\n")
	b.WriteString("passing case, never a hand-typed status. For the measured corpus number see `SQL_COVERAGE.md`.\n\n")
	b.WriteString("**Axes** (RFC-165 §4.2): `Java?` × `Go?`. The headline is keyed on **`Go?` only** —\n")
	b.WriteString("a feature only Java has is never counted as Go-supported.\n\n")

	b.WriteString("> **Denominator (pinned fact):** SQL:2023 Core as enumerated by PostgreSQL 18 = **176** mandatory\n")
	b.WriteString("> feature/subfeature rows (it was 177 in PG13–15; `F812` \"Basic flagging\" lost Core status in PG16).\n\n")
	fmt.Fprintf(&b, "This ledger tracks **%d** Core rows", coreTotal)
	if coreTotal < 176 {
		fmt.Fprintf(&b, " of 176 (the remainder pending roster entry, Phase 1)")
	}
	fmt.Fprintf(&b, ". **%d** are N/A for an embedded record-layer SQL surface "+
		"(cursors, table privileges, host-language binding, modules, SQL-invoked routines). "+
		"Of the **%d applicable** Core rows: **Go supports %d** "+
		"(%d shared-parity + %d Go-only extension); **%d shared gaps** (roadmap); "+
		"**%d port-fidelity divergences** (Java has it, Go rejects it → RFC-164); "+
		"**%d not yet tagged** (Phase 1 — these are unknown, not gaps).\n\n",
		naCount, applicable, goSupported, sharedParity, goExt, sharedGap, divergence, untested)

	if len(l.Violations) > 0 {
		b.WriteString("> ⚠️ EVIDENCE VIOLATIONS (drift-guard failures): a `# ansi:` tag without a matching\n")
		b.WriteString("> passing test, or a `# ansi-gap:` tag without an unsupported pin. Fix the tag or the test.\n")
		for _, v := range l.Violations {
			fmt.Fprintf(&b, "> - %s\n", v)
		}
		b.WriteString("\n")
	}

	b.WriteString("| Identifier | Core? | Feature | Java? | Go? | Routing | Evidence |\n")
	b.WriteString("|---|:---:|---|:---:|:---:|---|---|\n")
	for _, r := range l.Rows {
		core := ""
		if r.Core {
			core = "✓"
		}
		ev := strings.Join(r.Evidence, ", ")
		if ev == "" {
			ev = "—"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s |\n",
			r.ID, core, escapePipe(r.Name), r.Java, r.Go, routingLabel(r), ev)
	}
	b.WriteString("\n")
	return b.String()
}

func routingLabel(r AnsiRow) string {
	if r.NA {
		return "N/A (out of engine scope)"
	}
	switch r.population() {
	case popSharedParity:
		return "shared parity"
	case popGoExt:
		return "Go-only ext"
	case popDivergence:
		return "**divergence → RFC-164**"
	case popUntested:
		return "untested (Phase 1)"
	default:
		return "shared gap → backlog"
	}
}

func escapePipe(s string) string { return strings.ReplaceAll(s, "|", "\\|") }

// GenerateAnsiLedger builds and renders SQL_ANSI_CONFORMANCE.md from the corpus
// under dir and the hand-authored roster (ansiCoreRoster).
func GenerateAnsiLedger(dir string) (string, error) {
	l, err := BuildAnsiLedger(dir, ansiCoreRoster)
	if err != nil {
		return "", err
	}
	return RenderAnsiLedger(l), nil
}
