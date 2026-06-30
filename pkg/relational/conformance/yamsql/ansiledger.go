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
//   - DERIVED from the corpus: `Go?` and completeness come from `ansi:` /
//     `ansi_gap:` tags on yamsql scenarios + each scenario's typed outcome
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

// normalizeAnsiIDs upper-cases and trims the IDs from a test's `ansi:` /
// `ansi_gap:` yaml field, dropping blanks. IDs match the roster case-insensitively.
func normalizeAnsiIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id = strings.ToUpper(strings.TrimSpace(id)); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// collectAnsiEvidence walks testdata/*.yaml and builds per-ID evidence from the
// PER-TEST `ansi:` / `ansi_gap:` yaml fields (RFC-165 §4.3). A test's `ansi: [ID]`
// contributes positive evidence iff THAT test passes (OutcomeSupported); its
// `ansi_gap: [ID]` contributes gap evidence iff THAT test is an unsupported pin.
// Binding each tag to its own test (not the scenario) is the exercised-not-exists
// guarantee made structural: a positive tag can't be credited off a sibling
// test. A tag whose own test has the wrong outcome is reported as a violation.
func collectAnsiEvidence(dir string, roster []AnsiFeature) (map[string]*ansiTagEvidence, []string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(matches)
	// known is the set of valid roster IDs (parents + subfeatures). A tag for an
	// ID NOT in this set is a typo (`E0511`, `E051-1`) that would otherwise
	// silently credit nothing — BuildAnsiLedger iterates the roster, not the
	// tags, so the real feature stays untested and the typo is invisible. Flag it.
	known := make(map[string]bool)
	for _, f := range roster {
		known[f.ID] = true
		for _, sub := range f.Subfeatures {
			known[sub] = true
		}
	}
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
		var s Scenario
		if err := yaml.Unmarshal(raw, &s); err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", path, err)
		}
		name := s.Name
		if name == "" {
			name = strings.TrimSuffix(filepath.Base(path), ".yaml")
		}
		// Per-test binding (RFC-165 §4.3): each `ansi:` / `ansi_gap:` tag lives ON a
		// specific test and is credited only when THAT test has the matching
		// outcome — a positive `ansi:` tag's test must pass, a gap tag's test must
		// be an unsupported-feature pin. Binding evidence to the exact exercising
		// test means a positive tag can NEVER be credited off a sibling test in the
		// same file (the F261-01 fake-checkbox class the audit caught), so the old
		// scenario-level cross-feature guard is gone — the binding is structural.
		for i, t := range s.Tests {
			for _, id := range normalizeAnsiIDs(t.Ansi) {
				if !known[id] {
					violations = append(violations, fmt.Sprintf("%s test #%d: `ansi: %s` — unknown ANSI ID, not in the roster (typo?)", name, i, id))
					continue
				}
				if classifyTest(t) != OutcomeSupported {
					violations = append(violations, fmt.Sprintf("%s test #%d: `ansi: %s` on a test that does not pass (it asserts an error) — a positive tag must sit on a passing test", name, i, id))
					continue
				}
				get(id).positive = append(get(id).positive, name)
			}
			for _, id := range normalizeAnsiIDs(t.AnsiGap) {
				if !known[id] {
					violations = append(violations, fmt.Sprintf("%s test #%d: `ansi_gap: %s` — unknown ANSI ID, not in the roster (typo?)", name, i, id))
					continue
				}
				if classifyTest(t) != OutcomeUnsupported {
					violations = append(violations, fmt.Sprintf("%s test #%d: `ansi_gap: %s` on a test that is not an unsupported-feature pin (0A000/0AF00/0AF01/42883)", name, i, id))
					continue
				}
				get(id).gaps = append(get(id).gaps, name)
			}
		}
	}
	// Global conflict guard: the same feature ID must not be tagged both
	// `ansi:` (supported) and `ansi_gap:` (rejected) ANYWHERE in the corpus —
	// not just within one file. `ev` accumulates across files, so a same-ID
	// positive-in-A / gap-in-B pair would otherwise slip a per-file scope and
	// deriveGo would silently return Partial → population() could mis-credit a Go
	// rejection pin as shared parity. An atomic feature can't be "partially"
	// supported, so any positive+gap on one ID is an authoring conflict, flagged
	// here (this is also what makes the atomic-subfeature edge in population()
	// genuinely unreachable, same-file AND cross-file).
	for id, e := range ev {
		if len(e.positive) > 0 && len(e.gaps) > 0 {
			violations = append(violations, fmt.Sprintf("%s: tagged both `ansi:` (%s) and `ansi_gap:` (%s) — an ID cannot be both supported and a gap",
				id, strings.Join(e.positive, ", "), strings.Join(e.gaps, ", ")))
		}
	}
	sort.Strings(violations)
	// CAVEAT (the testdata-vs-mirror seam): this guard checks the OUTCOME SHAPE
	// declared in the testdata yaml; it does not RUN the tagged scenario. Tag
	// evidence is therefore only as strong as the lane that actually executes the
	// tagged scenario (the sqldriver probe tests / the cross-engine harness). The
	// yamsql corpus walk is t.Skip'd, and some `*_java.yaml` files carry
	// unvalidated declared outcomes — so a tag must sit on a scenario whose
	// behaviour is genuinely exercised elsewhere. The A3 `Java?` cross-check
	// follow-up (TODO.md) closes the Java side of this seam.
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
		evidence = append(evidence, markGaps(e.gaps)...)
	}
	if len(f.Subfeatures) == 0 {
		return direct, evidence
	}
	var full, partial, none, untested int
	for _, sub := range f.Subfeatures {
		s := deriveGo(ev[sub])
		if e := ev[sub]; e != nil {
			evidence = append(evidence, e.positive...)
			evidence = append(evidence, markGaps(e.gaps)...)
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
//
// Per RFC-165 §4.6, a Java=Full/Go=Partial row routes to popSharedParity
// (Partial counts as supported()), surfacing the subfeature-level divergence in
// the row comment rather than the headline. That is correct for PARENT rows
// (their Partial is a rollup). An *atomic subfeature* carrying both a positive
// and a gap tag (Go=Partial) under Java=Full would be misrouted to parity rather
// than flagged — but that can't reach here: the GLOBAL conflict guard in
// collectAnsiEvidence rejects the same ID tagged `ansi:`+`ansi_gap:` anywhere in
// the corpus (same-file OR cross-file), so a clean ledger never renders an atomic
// Partial. (Per-test binding also means a positive tag is credited only off its
// own passing test, so it can't be mis-credited by a sibling.)
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
// `ansi:` evidence under dir.
func BuildAnsiLedger(dir string, roster []AnsiFeature) (*AnsiLedger, error) {
	ev, violations, err := collectAnsiEvidence(dir, roster)
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

// markGaps suffixes gap-pin scenario names with " (gap)" so the Evidence column
// shows which scenario pinned a rejection (traceability for SupportNone/Partial
// rows), distinct from positive-evidence scenarios.
func markGaps(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = n + " (gap)"
	}
	return out
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
	b.WriteString("     in ansi_roster.go; the Go? column + completeness are DERIVED from `ansi:` tags\n")
	b.WriteString("     on the yamsql corpus. Drift guards: TestAnsiLedgerUpToDate + TestAnsiLedgerEvidenceExists. -->\n\n")
	b.WriteString("Ledger A of RFC-165 — the ANSI-standard scorecard, modeled on PostgreSQL Appendix D.\n")
	b.WriteString("The `Java?`/`Core?`/`Identifier`/`Name` columns are pinned facts (SQL:2023 Core; the\n")
	b.WriteString("frozen fdb-relational 4.12.11.0 reference). **`Go?` and completeness are derived** by\n")
	b.WriteString("walking `ansi:`-tagged corpus scenarios — a claim of support traces to a named,\n")
	b.WriteString("passing case, never a hand-typed status. For the measured corpus number see `SQL_COVERAGE.md`.\n\n")
	b.WriteString("**Axes** (RFC-165 §4.2): `Java?` × `Go?`. The headline is keyed on **`Go?` only** —\n")
	b.WriteString("a feature only Java has is never counted as Go-supported.\n\n")
	b.WriteString("Counting follows PostgreSQL Appendix D: every Core row (parent **and** subfeature) counts. A\n")
	b.WriteString("parent's status is *derived* from its subfeatures (partial = some supported), so a parent-level\n")
	b.WriteString("\"supported\" is slightly more generous than PG's binary per-row assessment — but the number is\n")
	b.WriteString("reproducible and drift-guarded, never hand-typed.\n\n")

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
		b.WriteString("> ⚠️ EVIDENCE VIOLATIONS (drift-guard failures): a `ansi:` tag without a matching\n")
		b.WriteString("> passing test, or a `ansi_gap:` tag without an unsupported pin. Fix the tag or the test.\n")
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
