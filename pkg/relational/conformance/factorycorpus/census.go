package factorycorpus

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// Census is the measured state of the committed corpus: RFC-201 §8's
// governance instrument. Raw test count is deliberately NOT the headline — a
// corpus of 100k tests on one axis and zero on another is the failure this
// design exists to kill — so the per-feature-vector map is a first-class field
// and the ratchet gates it dimension by dimension.
type Census struct {
	// Scenarios is the number of committed files.
	Scenarios int `json:"scenarios"`
	// Tests is the number of committed test stanzas across all files.
	Tests int `json:"tests"`
	// ByFeature counts SCENARIOS per feature vector. This is the dimension
	// audit: a shrink here names exactly which query shape stopped being
	// covered, which a total count cannot.
	ByFeature map[string]int `json:"by_feature"`
	// ByBlessing counts scenarios per blessing authority. It is reported so a
	// batch cannot quietly downgrade: a corpus drifting from cross-engine to
	// metamorphic is losing oracle strength while its totals still rise.
	ByBlessing map[string]int `json:"by_blessing"`
}

// ComputeCensus measures the corpus from the FILES, never from a manifest the
// factory also wrote. A census derived from the producer's own bookkeeping
// agrees with the producer by construction and would stay green through a
// batch that wrote nothing to disk.
func ComputeCensus(files []*File) Census {
	c := Census{
		ByFeature:  map[string]int{},
		ByBlessing: map[string]int{},
	}
	for _, f := range files {
		c.Scenarios++
		c.Tests += len(f.Scenario.Tests)
		c.ByFeature[f.Header.FeatureVector]++
		c.ByBlessing[string(f.Header.Blessing)]++
	}
	return c
}

// LoadCensus reads a committed census baseline.
func LoadCensus(path string) (Census, error) {
	var c Census
	data, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("parse %s: %w", path, err)
	}
	return c, nil
}

// RenderCensus serializes a baseline with sorted keys and a trailing newline,
// so a batch that adds nothing produces no diff.
func RenderCensus(c Census) ([]byte, error) {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// CheckRatchet compares a measured census against the committed baseline and
// returns one message per SHRINK, empty when the corpus only grew.
//
// The rule is componentwise, not aggregate: totals rising while a dimension
// empties out is exactly the failure mode a single count cannot see, and it is
// the realistic one — a regenerated batch that drops a query family still
// grows the total. A feature vector present in the baseline and absent now is
// a shrink to zero and is named as such; a NEW feature vector is growth and is
// silent.
//
// Blessing is ratcheted the same way and for the same reason: re-running a
// batch without the Java server would otherwise re-bless cross-engine files as
// metamorphic, keeping every count intact while quietly deleting the second
// engine from the corpus's authority.
func CheckRatchet(baseline, current Census) []string {
	var shrinks []string
	if current.Scenarios < baseline.Scenarios {
		shrinks = append(shrinks, fmt.Sprintf("scenarios: %d < baseline %d (%d committed scenarios disappeared)",
			current.Scenarios, baseline.Scenarios, baseline.Scenarios-current.Scenarios))
	}
	if current.Tests < baseline.Tests {
		shrinks = append(shrinks, fmt.Sprintf("tests: %d < baseline %d (%d committed test stanzas disappeared)",
			current.Tests, baseline.Tests, baseline.Tests-current.Tests))
	}
	shrinks = append(shrinks, dimensionShrinks("feature-vector", baseline.ByFeature, current.ByFeature)...)
	shrinks = append(shrinks, dimensionShrinks("blessing", baseline.ByBlessing, current.ByBlessing)...)
	return shrinks
}

func dimensionShrinks(dim string, baseline, current map[string]int) []string {
	keys := make([]string, 0, len(baseline))
	for k := range baseline {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		if current[k] < baseline[k] {
			out = append(out, fmt.Sprintf("%s %q: %d < baseline %d", dim, k, current[k], baseline[k]))
		}
	}
	return out
}

// CensusBaselinePath is the committed baseline, relative to this package.
const CensusBaselinePath = "census_baseline.json"
