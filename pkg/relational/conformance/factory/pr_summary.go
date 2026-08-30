package factory

import (
	"fmt"
	"sort"
	"strings"
)

// PRBodyLimit is GitHub's hard cap on a pull-request body, in characters.
// createPullRequest rejects anything longer with "Body is too long (maximum is
// 65536 characters)" — a GraphQL error, so it arrives AFTER the branch has been
// committed and pushed. The batch is already on the remote when the PR fails,
// which is why this has to be enforced before the call rather than retried
// after it.
const PRBodyLimit = 65536

// prSummaryBudget is the size SummaryMarkdown holds itself to. It is
// deliberately far below PRBodyLimit rather than close to it: the point is a
// body someone actually reads, not a body that merely fits, and the headroom is
// what lets the per-class ledgers below grow a few entries without anyone having to
// think about the cap again.
const prSummaryBudget = 16384

// maxLedgerEntries caps each per-class ledger rendered into the summary.
//
// Every map rendered here is keyed by a CLASS (a skip reason, a blessing
// authority, an exemption family) and those are bounded by the source: there
// are a fixed handful of reason classes, not one per candidate. The cap is
// therefore expected never to bind, and exists because "expected never to bind"
// is exactly the assumption that produced this bug — Census.ByFeature was also
// a map nobody expected to reach 5000 entries.
const maxLedgerEntries = 40

// SummaryMarkdown renders the batch's ledger as a bounded PR body.
//
// WHY A SUMMARY AND NOT THE MANIFEST. The manifest embeds Census, and Census
// carries two maps with ONE ENTRY PER SCENARIO IN THE WHOLE COMMITTED CORPUS —
// ByFeature (a long feature-vector string per key) and ByKeyBlessing. That is
// whole-corpus state, not this batch's output, and it crossed GitHub's 65536
// limit at roughly 500 scenarios. At 5000 scenarios the manifest is ~640KB,
// nearly ten times the cap, and it grows by up to one quota (default 1000)
// every night. Embedding it in a PR body was never going to keep working.
//
// WHAT IS NOT DROPPED, which is the whole point. Every number RFC-201 §5.1 asks
// a stage to report survives: the seed range, all four funnel counts, the dedup
// stats, the name-collision count, the full skip ledger WITH its samples, the
// oracle stats, the blessing mix, the exemption families, the finding count,
// and the census SCALARS. What is left out is exactly the two per-scenario maps
// — and the reader is told so, by name, with the artifact that has them. A
// summary that quietly omitted a count would be worse than the failure it
// replaces, because a failed PR is loud and a missing number is not.
//
// runURL should be the workflow run's URL so the pointer is actionable; an
// empty value renders the artifact name alone rather than a dangling link.
func (m Manifest) SummaryMarkdown(runURL string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "RFC-201 §5 factory batch %s.\n\n", m.Date)

	fmt.Fprintf(&b, "## Batch\n\n")
	fmt.Fprintf(&b, "| field | value |\n|---|---|\n")
	fmt.Fprintf(&b, "| generator | `%s` |\n", m.Generator)
	fmt.Fprintf(&b, "| seeds | %d..%d (%d) |\n", m.SeedStart, m.SeedStart+m.Seeds-1, m.Seeds)
	fmt.Fprintf(&b, "| quota | %d |\n", m.Quota)
	fmt.Fprintf(&b, "| blessing mode | %s |\n", m.BlessingMode)

	fmt.Fprintf(&b, "\n## Funnel\n\n")
	fmt.Fprintf(&b, "| stage | count |\n|---|---|\n")
	fmt.Fprintf(&b, "| candidates generated | %d |\n", m.CandidatesGen)
	fmt.Fprintf(&b, "| candidates executed | %d |\n", m.CandidatesRun)
	fmt.Fprintf(&b, "| blessed | %d |\n", m.Blessed)
	fmt.Fprintf(&b, "| **committed** | **%d** |\n", m.Committed)
	fmt.Fprintf(&b, "| dedup rejected | %d |\n", m.DedupRejected)
	fmt.Fprintf(&b, "| dedup points after | %d |\n", m.DedupPoints)
	// Called out on its own line, not buried in the skip ledger: a non-zero
	// name collision means a committed scenario was nearly clobbered, which is
	// a statement about the engine rather than about the batch.
	fmt.Fprintf(&b, "| name collisions | %d |\n", m.NameCollisions)
	fmt.Fprintf(&b, "| findings | %d |\n", m.Findings)
	// Engine errors are lifted OUT of the skip ledger and onto the funnel,
	// beside `findings`, because they are the one skip class that is a
	// statement about the ENGINE rather than about the batch.
	//
	// The generator emits only shapes it believes are supported, so a query the
	// engine refuses to execute is a defect signal. It is correctly not
	// BLESSED — freezing a rejection would pin a limitation as intended
	// behaviour — but "do not bless" and "do not report" are different
	// decisions, and collapsing them puts the signal in a counter beside
	// `degenerate partition`, where nothing triages it and `findings | 0` reads
	// as a clean batch.
	//
	// Concretely: the QOV-binding defect fixed in InComparisonToExplodeRule was
	// an EXECUTION error on a query that is fully TLP-eligible (a WHERE, no
	// aggregate, union, derived table, LIMIT, OFFSET or DISTINCT), so the
	// factory could have generated it. Had a batch drawn that seed it would
	// have been counted here and reported nothing. Measured over the 11 batches
	// of PR #745 — seeds 1896000..1947999, ~24000 candidates — this class is
	// ZERO, so surfacing it costs no noise and is a rare signal made loud.
	fmt.Fprintf(&b, "| engine errors (not findings — see below) | %d |\n",
		m.SkipsByReason["engine error"]+m.SkipsByReason["plan-error"])

	writeCountLedger(&b, "Skips by reason", m.SkipsByReason)
	writeCountLedger(&b, "Blessings", m.Blessings)
	writeCountLedger(&b, "Blessed by exemption family", m.BlessedByFamily)

	if len(m.SkipSamples) > 0 {
		fmt.Fprintf(&b, "\n## Skip samples\n\n")
		for _, reason := range sortedKeys(m.SkipSamples) {
			fmt.Fprintf(&b, "- **%s**\n", reason)
			for _, s := range m.SkipSamples[reason] {
				fmt.Fprintf(&b, "  - `%s`\n", oneLine(s))
			}
		}
	}

	if len(m.Exemptions) > 0 {
		fmt.Fprintf(&b, "\n## Exemptions\n\n")
		for _, e := range m.Exemptions {
			fmt.Fprintf(&b, "- %s\n", oneLine(e))
		}
	}

	fmt.Fprintf(&b, "\n## Corpus census after this batch\n\n")
	fmt.Fprintf(&b, "| metric | value |\n|---|---|\n")
	fmt.Fprintf(&b, "| scenarios | %d |\n", m.Census.Scenarios)
	fmt.Fprintf(&b, "| tests | %d |\n", m.Census.Tests)
	fmt.Fprintf(&b, "| distinct feature vectors | %d |\n", len(m.Census.ByFeature))
	fmt.Fprintf(&b, "| keyed blessings | %d |\n", len(m.Census.ByKeyBlessing))

	fmt.Fprintf(&b, "\n---\n\n")
	fmt.Fprintf(&b, "The per-scenario census maps (`census_after.by_feature`, "+
		"`census_after.by_key_blessing`) are whole-corpus state and are NOT inlined here: "+
		"at %d scenarios they render to hundreds of kilobytes, well past GitHub's %d-character "+
		"body limit. The complete `factory-manifest.json` is in the **factory-findings** "+
		"artifact", m.Census.Scenarios, PRBodyLimit)
	if runURL != "" {
		fmt.Fprintf(&b, " of [this run](%s)", runURL)
	}
	fmt.Fprintf(&b, ", together with `batch.log` and any persisted findings. "+
		"The committed census baseline is tracked in-tree at "+
		"`pkg/relational/conformance/factorycorpus/census_baseline.json`.\n")

	return Truncate(b.String(), prSummaryBudget)
}

// writeCountLedger renders one class->count map, sorted for determinism (Go map
// order is randomized, and a PR body that reshuffles between runs is one nobody
// can diff).
func writeCountLedger(b *strings.Builder, title string, counts map[string]int) {
	if len(counts) == 0 {
		return
	}
	fmt.Fprintf(b, "\n## %s\n\n", title)
	fmt.Fprintf(b, "| class | count |\n|---|---|\n")
	keys := sortedKeys(counts)
	shown := keys
	if len(shown) > maxLedgerEntries {
		shown = shown[:maxLedgerEntries]
	}
	for _, k := range shown {
		fmt.Fprintf(b, "| %s | %d |\n", oneLine(k), counts[k])
	}
	if len(keys) > len(shown) {
		fmt.Fprintf(b, "\n_%d further classes omitted; see `factory-manifest.json` in the factory-findings artifact._\n",
			len(keys)-len(shown))
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// oneLine flattens a value into a single markdown table cell. A raw newline
// ends the row and a raw pipe starts a new column, so an unescaped skip sample
// does not merely look wrong — it silently restructures the table around it.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.ReplaceAll(s, "|", "\\|")
}

// Truncate bounds a body at limit CHARACTERS, leaving an explicit marker rather
// than a silent cut.
//
// GitHub counts characters, not bytes, so the budget is spent in runes; cutting
// by byte offset would also risk splitting a multi-byte rune and emitting
// invalid UTF-8. The marker is mandatory: a body that stops mid-sentence with
// no marker reads as a generator bug and sends the reader looking for a defect
// that is not there, while a marked cut sends them to the artifact.
func Truncate(body string, limit int) string {
	const marker = "\n\n---\n\n**[truncated: this body exceeded the size budget. " +
		"The complete `factory-manifest.json` is in the factory-findings artifact of this run.]**\n"
	if len([]rune(body)) <= limit {
		return body
	}
	runes := []rune(body)
	keep := limit - len([]rune(marker))
	if keep < 0 {
		keep = 0
	}
	return string(runes[:keep]) + marker
}
