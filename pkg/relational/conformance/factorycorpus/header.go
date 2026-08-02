package factorycorpus

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// FormatVersion is the provenance contract's own version. It changes when the
// set of REQUIRED provenance keys or the file layout changes, so an old
// committed file fails to load loudly instead of being read with a key
// silently missing. Version 2 is the yamsql convergence: one `.yamsql` file
// per feature family, each scenario a (schema_template, setup, test_block)
// document triple in the Java yaml-tests dialect, provenance in comments.
const FormatVersion = 2

// Blessing names the oracle authority that froze a scenario's expectations.
// RFC-201 §5.3: a committed expectation is frozen, and WHICH oracle froze it
// determines how much it is worth — so it is recorded in the file rather than
// inferred from the run that produced it.
type Blessing string

const (
	// BlessingCrossEngine means the Java engine returned the same rows: the
	// strongest authority available, because the expectation is then
	// independent of every Go-side belief.
	BlessingCrossEngine Blessing = "cross-engine"
	// BlessingMetamorphic means no second engine saw the query and the
	// expectation rests on the metamorphic oracles alone (§5.5) — the TLP
	// partition plus second-plan agreement. Honest, weaker, and UPGRADEABLE:
	// re-running the batch against a reachable Java server promotes the
	// scenario without regenerating it.
	BlessingMetamorphic Blessing = "metamorphic"
	// BlessingMetamorphicTLPOnly means the TLP partition agreed and the
	// second-plan oracle was STRUCTURALLY INAPPLICABLE to the shape — not
	// skipped by accident, but unable to apply for a reason a committed pin
	// establishes.
	//
	// It exists so that "every applicable oracle agreed" can be said honestly
	// without saying "both oracles agreed". Correlated EXISTS is the first
	// instance: the perturbed plan comes out byte-identical because the outer
	// leg is a filtered scan either way, so there is no second plan to compare
	// and never will be under that perturbation. Blessing such a scenario plain
	// `metamorphic` would claim evidence that does not exist; refusing to bless
	// it at all cost the corpus an entire query family (zero of the first 900
	// scenarios carried an EXISTS, while the generator emits them at ~1/4).
	//
	// It is DELIBERATELY a distinct census dimension. Authority is measured, and
	// a label that quietly absorbed a weaker case would make the measurement lie.
	BlessingMetamorphicTLPOnly Blessing = "metamorphic-tlp-only"
)

// BlessingRank orders authorities from weakest to strongest, and is what makes
// "promotion" a decidable question rather than a matter of opinion.
//
// The per-scenario ratchet compares a scenario against ITSELF across batches
// on this scale, keyed by dedup key: rising is promotion and silent, falling
// is a downgrade and red. An aggregate COUNT per label cannot express that —
// promoting a scenario decrements the count of the label it left, so a count
// floor on `metamorphic` fires on exactly the improvement it should welcome.
func BlessingRank(b Blessing) int {
	switch b {
	case BlessingMetamorphicTLPOnly:
		return 1
	case BlessingMetamorphic:
		return 2
	case BlessingCrossEngine:
		return 3
	}
	return 0
}

// Header is the RFC-201 §5.1 provenance block of ONE committed scenario: the
// `#` comment lines immediately above the scenario's document triple inside
// its family file. It is deliberately a flat key/value comment block rather
// than YAML content — the yamsql parser must see exactly the Java yaml-tests
// schema and nothing else (a provenance key inside a document would be an
// unknown directive), while the census must be able to read provenance from
// thousands of scenarios without executing them. Comments are the one
// extension the format tolerates freely, so a file this package writes is
// parseable by Java's own yaml-tests runner verbatim.
type Header struct {
	// Name is the scenario name; it must equal the scenario's test_block name,
	// so a provenance block cannot drift onto someone else's documents without
	// the mismatch being visible.
	Name string
	// Generator identifies the case generator and its version, e.g.
	// "rowdiff-gen/1". Together with Seed it is the whole reproduction recipe.
	Generator string
	// Seed is the generator seed. Same seed, same generator version, same
	// case — pinned by TestFactoryDeterminism.
	Seed uint64
	// QueryIndex is the index of the query within the seed's case, and
	// Projection the index of the projection variant. Seed alone does not
	// identify a candidate: one case yields several queries and each query
	// several projections.
	QueryIndex int
	Projection int
	// Date is the batch date, YYYY-MM-DD. It is an INPUT to generation, never
	// read from the clock: §5.4 forbids wall-clock in the generation path, so
	// regenerating a committed scenario byte-identically means passing back the
	// date it already carries.
	Date string
	// Blessing is the authority that froze the expectations.
	Blessing Blessing
	// Oracles lists the oracles that actually RAN and agreed, in a stable
	// order. An oracle that was skipped (its precondition unmet) must not
	// appear — the whole point of the list is that it cannot claim coverage
	// the run did not exercise.
	Oracles []string
	// FeatureVector is the serialized spec-struct feature vector: the
	// structural description of the query, literals erased. It is one half of
	// the dedup key, the axis the census reports per-dimension counts on, and
	// what places the scenario in its family file (FamilyOf).
	FeatureVector string
	// PlanShape is a hex digest of explaindiff.ShapeOf over the physical plan.
	// It is the other half of the dedup key: the feature vector says what was
	// ASKED, the plan shape says what the planner DID, and two candidates
	// agreeing on both add nothing to each other.
	PlanShape string
	// DedupKey is the digest of (FeatureVector, PlanShape) — the corpus's
	// primary key, and the key the blessing ratchet compares a scenario
	// against itself on. Stored rather than recomputed so the loader can
	// detect a collision without a planner.
	DedupKey string
}

// headerKeyLine matches a `# key: value` provenance line. Keys are lower-case
// with dashes; anything else in a comment block is prose and is skipped.
var headerKeyLine = regexp.MustCompile(`^#\s*([a-z][a-z0-9-]*):\s*(.*)$`)

// scenarioKey opens every scenario's provenance block; the loader splits the
// file's comment stream on it.
const scenarioKey = "scenario"

// Render returns the scenario's provenance comment block, newline-terminated,
// ready to sit immediately above the scenario's `---` document separator.
func (h Header) Render() string {
	var b strings.Builder
	for _, kv := range [][2]string{
		{scenarioKey, h.Name},
		{"generator", h.Generator},
		{"seed", strconv.FormatUint(h.Seed, 10)},
		{"query-index", strconv.Itoa(h.QueryIndex)},
		{"projection", strconv.Itoa(h.Projection)},
		{"date", h.Date},
		{"blessing", string(h.Blessing)},
		{"oracles", strings.Join(h.Oracles, ",")},
		{"feature-vector", h.FeatureVector},
		{"plan-shape", h.PlanShape},
		{"dedup-key", h.DedupKey},
	} {
		fmt.Fprintf(&b, "# %s: %s\n", kv[0], kv[1])
	}
	// The reproduce line is derived from seed and date, never parsed back:
	// unknown keys are ignored by ParseHeader, so re-wording it in a later
	// writer does not invalidate committed provenance.
	fmt.Fprintf(&b, "# reproduce: bazelisk run //cmd/factory-run -- -seed-start %d -seeds 1 -date %s -out <dir>\n",
		h.Seed, h.Date)
	return b.String()
}

// ParseHeader reads one scenario's provenance block from its comment lines.
// Every key in the contract is REQUIRED: a missing one is an error, never a
// zero value. A header whose fields default silently is a header that stops
// describing the scenario the moment the contract grows, and the census would
// keep reporting a confident number computed from absent data.
func ParseHeader(lines []string) (Header, error) {
	var h Header
	seen := map[string]string{}
	for _, line := range lines {
		m := headerKeyLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		// A repeated key is REJECTED, not last-wins. Last-wins makes the header
		// depend on line order for a block that is otherwise a set, and it
		// hides the two ways a duplicate actually arises: a hand-edit that
		// appended a corrected line instead of replacing one, and a writer
		// change that emitted a key twice. Both produce a block whose header
		// says one thing to a reader and another to the census.
		if prev, dup := seen[m[1]]; dup {
			return h, fmt.Errorf("provenance key %q appears twice (%q then %q); a duplicate key would silently "+
				"resolve to whichever line came last", m[1], prev, strings.TrimSpace(m[2]))
		}
		seen[m[1]] = strings.TrimSpace(m[2])
	}

	required := []string{
		scenarioKey, "generator", "seed", "query-index", "projection",
		"date", "blessing", "oracles", "feature-vector", "plan-shape", "dedup-key",
	}
	for _, k := range required {
		if _, ok := seen[k]; !ok {
			return h, fmt.Errorf("provenance block is missing required key %q", k)
		}
	}

	var err error
	h.Name = seen[scenarioKey]
	if h.Name == "" {
		return h, fmt.Errorf("scenario name is empty")
	}
	h.Generator = seen["generator"]
	if h.Seed, err = strconv.ParseUint(seen["seed"], 10, 64); err != nil {
		return h, fmt.Errorf("seed: %w", err)
	}
	if h.QueryIndex, err = strconv.Atoi(seen["query-index"]); err != nil {
		return h, fmt.Errorf("query-index: %w", err)
	}
	if h.Projection, err = strconv.Atoi(seen["projection"]); err != nil {
		return h, fmt.Errorf("projection: %w", err)
	}
	h.Date = seen["date"]
	h.Blessing = Blessing(seen["blessing"])
	switch h.Blessing {
	case BlessingCrossEngine, BlessingMetamorphic, BlessingMetamorphicTLPOnly:
	default:
		return h, fmt.Errorf("blessing %q is not one of %q/%q/%q", h.Blessing,
			BlessingCrossEngine, BlessingMetamorphic, BlessingMetamorphicTLPOnly)
	}
	if seen["oracles"] == "" {
		return h, fmt.Errorf("oracles is empty: a blessed scenario must name at least one oracle that ran")
	}
	h.Oracles = strings.Split(seen["oracles"], ",")
	h.FeatureVector = seen["feature-vector"]
	if h.FeatureVector == "" {
		return h, fmt.Errorf("feature-vector is empty: the census counts per feature vector and would silently bucket this scenario under \"\"")
	}
	h.PlanShape = seen["plan-shape"]
	h.DedupKey = seen["dedup-key"]
	if h.PlanShape == "" || h.DedupKey == "" {
		return h, fmt.Errorf("plan-shape/dedup-key must both be set (got %q/%q)", h.PlanShape, h.DedupKey)
	}
	if want := DedupKeyOf(h.FeatureVector, h.PlanShape); want != h.DedupKey {
		return h, fmt.Errorf("dedup-key %s does not match digest of (feature-vector, plan-shape) = %s: the key is the corpus's primary key and a stale one lets a duplicate shape in", h.DedupKey, want)
	}
	if h.Date == "" {
		return h, fmt.Errorf("date is empty")
	}
	return h, nil
}
