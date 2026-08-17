package main

import (
	"testing"

	"fdb.dev/pkg/relational/conformance/factory"
	"fdb.dev/pkg/relational/conformance/factorycorpus"
)

// reblessSeed is an arbitrary fixed seed. Nothing about the assertions below
// depends on WHICH seed it is — only that both generators derive from the same
// one, which is the condition under which a seed-only lookup is wrong.
const reblessSeed = uint64(1)

// TestResolveCandidatesUsesTheGeneratorTheHeaderNames pins the axis the
// re-bless tool got wrong: it resolved every committed scenario through
// `factory.Candidates(seed)` — the FLAT generator, unconditionally — while the
// corpus records a generator NAME per file. Every nested-generator file was
// therefore compared against an unrelated candidate, and the tool refused on
// the corpus's first file with a "feature vector moved" error describing a
// family change that never happened.
//
// The first assertion is the VACUITY GUARD and it has to come first: if the two
// generators ever derived the same candidates from a seed, the second
// assertion would hold with the bug fully present.
func TestResolveCandidatesUsesTheGeneratorTheHeaderNames(t *testing.T) {
	t.Parallel()

	flat := factory.Candidates(reblessSeed)
	nested := factory.NestedCandidates(reblessSeed)
	if len(flat) == 0 || len(nested) == 0 {
		t.Fatalf("seed %d yields %d flat and %d nested candidates; a lookup test over an "+
			"empty candidate set asserts nothing", reblessSeed, len(flat), len(nested))
	}
	if flat[0].FeatureVector == nested[0].FeatureVector {
		t.Fatalf("both generators derive feature vector %q from seed %d, so this test "+
			"cannot tell the two lookups apart — pick a seed where they differ",
			flat[0].FeatureVector, reblessSeed)
	}

	got, err := resolveCandidates(genSeed{factory.NestedGeneratorVersion, reblessSeed})
	if err != nil {
		t.Fatalf("resolveCandidates(nested): %v", err)
	}
	if len(got) != len(nested) {
		t.Fatalf("nested recipe resolved %d candidates, want the nested generator's %d",
			len(got), len(nested))
	}
	for i := range got {
		if got[i].FeatureVector != nested[i].FeatureVector {
			t.Fatalf("nested recipe resolved candidate %d as %q, want %q — the header's "+
				"generator name is being ignored", i, got[i].FeatureVector, nested[i].FeatureVector)
		}
	}

	flatGot, err := resolveCandidates(genSeed{factory.GeneratorVersion, reblessSeed})
	if err != nil {
		t.Fatalf("resolveCandidates(flat): %v", err)
	}
	if len(flatGot) != len(flat) || flatGot[0].FeatureVector != flat[0].FeatureVector {
		t.Fatalf("flat recipe resolved %d candidates starting %q, want %d starting %q",
			len(flatGot), flatGot[0].FeatureVector, len(flat), flat[0].FeatureVector)
	}
}

// TestResolveCandidatesRejectsAnUnknownGenerator pins the fail-closed half. A
// header naming a generator this build does not have cannot be checked at all,
// and regenerating it under whichever generator the build DOES have is exactly
// the silent mis-match the test above exists to forbid.
func TestResolveCandidatesRejectsAnUnknownGenerator(t *testing.T) {
	t.Parallel()
	if _, err := resolveCandidates(genSeed{"rowdiff-gen/999", reblessSeed}); err == nil {
		t.Fatal("an unknown generator resolved successfully; a recipe this build cannot " +
			"reproduce must fail, not fall back to another generator")
	}
}

// TestGroupByRecipeSeparatesGeneratorsSharingASeed pins the other half of the
// same fix. Resolving through the header's generator is not enough on its own:
// if the buckets are keyed on the seed alone, two generators' scenarios collide
// into one bucket and the whole bucket is compared against whichever
// generator's candidates the single lookup returned.
func TestGroupByRecipeSeparatesGeneratorsSharingASeed(t *testing.T) {
	t.Parallel()
	scenarios := []*factorycorpus.Scenario{
		{Header: factorycorpus.Header{Name: "flat_a", Generator: factory.GeneratorVersion, Seed: reblessSeed}},
		{Header: factorycorpus.Header{Name: "nested_a", Generator: factory.NestedGeneratorVersion, Seed: reblessSeed}},
		{Header: factorycorpus.Header{Name: "flat_b", Generator: factory.GeneratorVersion, Seed: reblessSeed}},
	}
	recipes, byRecipe := groupByRecipe(scenarios)
	if len(recipes) != 2 {
		t.Fatalf("grouped into %d recipes, want 2 — one seed under two generators is two "+
			"reproduction recipes, not one", len(recipes))
	}
	// Stable order: generator name, then seed.
	if recipes[0].gen != factory.GeneratorVersion || recipes[1].gen != factory.NestedGeneratorVersion {
		t.Fatalf("recipe order = %q, %q; want the flat generator before the nested one",
			recipes[0].gen, recipes[1].gen)
	}
	if got := len(byRecipe[recipes[0]]); got != 2 {
		t.Fatalf("flat recipe holds %d scenarios, want 2", got)
	}
	if got := len(byRecipe[recipes[1]]); got != 1 {
		t.Fatalf("nested recipe holds %d scenarios, want 1", got)
	}
}
