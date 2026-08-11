package docscheck

import (
	"strings"
	"testing"
)

// The live edges table exercises only the arms it happens to contain — today
// that is the well-formed, correctly-classed ones, which is to say the arms that
// find nothing. Every arm that FIRES is therefore untested by the corpus
// reading, including the one that exists to catch the exact claim that shipped
// wrong. An untested arm's first real firing gets read as a finding rather than
// as a branch nobody had run, so each is driven directly here.
//
// The functions under test are pure over explicit state for this reason: they
// take rows, not a document, and return problems, not `t.Error` calls.

func edgeRow(producer, reader, class, retires, evidence string) []string {
	return edgeRowFull(producer, "p_"+evidence, reader, evidence, class, retires)
}

func edgeRowFull(producer, producerEvidence, reader, readerEvidence, class, retires string) []string {
	return []string{producer, producerEvidence, reader, readerEvidence, class, retires}
}

// A well-formed table that satisfies every check, used as the base each arm
// perturbs. Keeping the negative cases one edit away from a passing table is
// what makes each failure attributable to the perturbation.
func goodEdgeRows() [][]string {
	return [][]string{
		edgeRow("prodA", "readerA", edgeClassConsumer, "yes", "a.go"),
		edgeRow("prodB", "readerB", edgeClassDecliner, "no", "b.go"),
		edgeRow("prodC", "readerC", edgeClassCoOccurring, "no", "c.go"),
	}
}

func TestFieldDebtOrderEdgeGateArms(t *testing.T) {
	t.Parallel()

	t.Run("well-formed table is silent", func(t *testing.T) {
		t.Parallel()
		edges, problems := parseOrderEdges(goodEdgeRows())
		problems = append(problems, checkEdgeClasses(edges)...)
		if len(problems) != 0 {
			t.Fatalf("a table satisfying every rule reported %d problems: %v\n"+
				"  Every negative arm below perturbs THIS table, so a false positive here "+
				"would make each of them pass for the wrong reason.", len(problems), problems)
		}
		if len(edges) != 3 {
			t.Fatalf("parsed %d edges, want 3", len(edges))
		}
	})

	// THE ARM THE WHOLE GATE EXISTS FOR. A decliner or co-occurring reader
	// claiming it retires behind its producer is the shape that shipped.
	for _, class := range []string{edgeClassDecliner, edgeClassCoOccurring} {
		t.Run("retires=yes rejected for "+class, func(t *testing.T) {
			t.Parallel()
			rows := goodEdgeRows()
			rows[1] = edgeRow("prodB", "readerB", class, "yes", "b.go")
			edges, problems := parseOrderEdges(rows)
			problems = append(problems, checkEdgeClasses(edges)...)
			if !hasProblemContaining(problems, "claims retires = yes") {
				t.Fatalf("a %s edge claiming retires = yes was accepted; problems: %v\n"+
					"  This is the exact refuted claim the gate was added for.", class, problems)
			}
		})
	}

	t.Run("consumer may claim either retirement", func(t *testing.T) {
		t.Parallel()
		// Asserted as a VALUE, not as a relationship: the one-directional
		// implication is only meaningful if `consumer` genuinely admits BOTH,
		// and a gate that quietly tightened to consumer=>yes would still pass
		// every other arm here.
		for _, retires := range []string{"yes", "no"} {
			rows := goodEdgeRows()
			rows[0] = edgeRow("prodA", "readerA", edgeClassConsumer, retires, "a.go")
			edges, problems := parseOrderEdges(rows)
			problems = append(problems, checkEdgeClasses(edges)...)
			if len(problems) != 0 {
				t.Fatalf("consumer with retires=%q reported %v; a consumer reading from "+
					"more than one producer does not retire behind any single one, so both "+
					"values must be admissible", retires, problems)
			}
		}
	})

	t.Run("unknown class rejected", func(t *testing.T) {
		t.Parallel()
		rows := goodEdgeRows()
		rows[1] = edgeRow("prodB", "readerB", "downstream", "no", "b.go")
		edges, problems := parseOrderEdges(rows)
		problems = append(problems, checkEdgeClasses(edges)...)
		if !hasProblemContaining(problems, "is not one of") {
			t.Fatalf("class %q was accepted; problems: %v", "downstream", problems)
		}
	})

	t.Run("empty table is a failure, not a pass", func(t *testing.T) {
		t.Parallel()
		_, problems := parseOrderEdges(nil)
		if !hasProblemContaining(problems, "no rows at all") {
			t.Fatalf("an empty edges table passed; problems: %v\n"+
				"  A graph describing nothing satisfies every per-row rule — the "+
				"green-from-an-empty-set shape.", problems)
		}
	})

	t.Run("wrong cell count reported", func(t *testing.T) {
		t.Parallel()
		_, problems := parseOrderEdges([][]string{{"`a`", "`b`", edgeClassConsumer}})
		if !hasProblemContaining(problems, "want 6") {
			t.Fatalf("a 3-cell row was accepted; problems: %v", problems)
		}
	})

	t.Run("non-identifier cell reported", func(t *testing.T) {
		t.Parallel()
		rows := goodEdgeRows()
		rows[0] = edgeRowFull("the star expander", "p_a.go", "readerA", "a.go", edgeClassConsumer, "yes")
		_, problems := parseOrderEdges(rows)
		if !hasProblemContaining(problems, "not a bare Go identifier") {
			t.Fatalf("a prose producer cell was accepted; problems: %v", problems)
		}
	})

	t.Run("bad retires value reported", func(t *testing.T) {
		t.Parallel()
		rows := goodEdgeRows()
		rows[0] = edgeRow("prodA", "readerA", edgeClassConsumer, "probably", "a.go")
		_, problems := parseOrderEdges(rows)
		if !hasProblemContaining(problems, "want \"yes\" or \"no\"") {
			t.Fatalf("retires=%q was accepted; problems: %v", "probably", problems)
		}
	})

	// Both collapse directions. These guard the instrument rather than the
	// document: a table that has drifted to a single class passes every per-row
	// check while having lost the distinction it exists to draw.
	t.Run("all-consumer table reported", func(t *testing.T) {
		t.Parallel()
		rows := [][]string{
			edgeRow("prodA", "readerA", edgeClassConsumer, "yes", "a.go"),
			edgeRow("prodB", "readerB", edgeClassConsumer, "no", "b.go"),
		}
		edges, _ := parseOrderEdges(rows)
		problems := checkEdgeClasses(edges)
		if !hasProblemContaining(problems, "no decliner and no co-occurring") {
			t.Fatalf("an all-consumer table passed; problems: %v\n"+
				"  That is a co-occurrence graph re-admitted unnoticed.", problems)
		}
	})

	t.Run("no-consumer table reported", func(t *testing.T) {
		t.Parallel()
		rows := [][]string{
			edgeRow("prodB", "readerB", edgeClassDecliner, "no", "b.go"),
			edgeRow("prodC", "readerC", edgeClassCoOccurring, "no", "c.go"),
		}
		edges, _ := parseOrderEdges(rows)
		problems := checkEdgeClasses(edges)
		if !hasProblemContaining(problems, "no consumer edge at all") {
			t.Fatalf("a table with nothing retiring behind anything passed; problems: %v", problems)
		}
	})

	// endpointDeclaredIn's rejection arms. The accept arm is exercised by the
	// live gate against the real table; these are the ones the corpus never
	// reaches. Driven for BOTH endpoint kinds, because the whole point of the
	// producer column is that a check running on one endpoint only is not a
	// check on the other.
	for _, what := range []string{"producer", "reader"} {
		t.Run(what+" evidence must be a go file", func(t *testing.T) {
			t.Parallel()
			if p := endpointDeclaredIn(".", "rfcs/197-column-identity-is-an-ordinal.md", what, "legRef"); p == "" {
				t.Fatalf("a non-.go %s evidence path was accepted", what)
			}
			if p := endpointDeclaredIn(".", "", what, "legRef"); p == "" {
				t.Fatalf("an empty %s evidence path was accepted", what)
			}
		})
	}

	// The gap this column closes, driven directly rather than only through the
	// live document: a producer cited to a file that does not declare it must be
	// rejected even when the name is a perfectly live symbol elsewhere. Before
	// the producer evidence column existed, membership in the repo-wide
	// identifier set was the ONLY producer check, and that swap passed.
	t.Run("producer cited to a file that does not declare it", func(t *testing.T) {
		t.Parallel()
		tree := sourceTreeRoot(t)
		const cascades = "pkg/relational/core/query/cascades_translator.go"

		if p := endpointDeclaredIn(tree, cascades, "producer", "rebaseUnnestOuterLegPredicate"); p != "" {
			t.Fatalf("the true producer citation was rejected: %s\n"+
				"  The negative below would then pass for the wrong reason.", p)
		}
		// `legRef` is live — it is a reader in the live table — but it is not
		// declared here. That swap is the one the repo-wide set waves through.
		p := endpointDeclaredIn(tree, cascades, "producer", "legRef")
		if p == "" {
			t.Fatal("a producer cited to a file that does not declare it was accepted; " +
				"a live-but-unrelated identifier is exactly the mis-citation the " +
				"repo-wide existence check cannot see")
		}
		if !strings.Contains(p, "producer") {
			t.Fatalf("the rejection does not name the endpoint kind: %q — a message that "+
				"cannot say WHICH citation is wrong sends the reader to the other column", p)
		}
	})

	// THE WIRING, driven without a source tree. The gap that shipped was not in
	// either check but in which endpoints reached them, and a loop living inline
	// in the gate body could not be perturbed by any unit arm. Each endpoint kind
	// is asserted to reach EACH check, one at a time, so dropping any single
	// pairing reddens exactly one subtest here.
	for _, endpoint := range []string{"producer", "reader"} {
		t.Run(endpoint+" reaches the existence check", func(t *testing.T) {
			t.Parallel()
			edges, _ := parseOrderEdges([][]string{
				edgeRowFull("prodA", "p.go", "readerA", "r.go", edgeClassConsumer, "yes"),
			})
			// Only the endpoint under test is dead; evidence always accepts, so
			// nothing but the existence check can produce a problem.
			dead := map[string]string{"producer": "prodA", "reader": "readerA"}[endpoint]
			problems := checkEdgeEndpoints(edges,
				func(name string) bool { return name != dead },
				func(_, _, _ string) string { return "" })
			if !hasProblemContaining(problems, "declared nowhere in the tracked Go tree") {
				t.Fatalf("a dead %s never reached the existence check; problems: %v", endpoint, problems)
			}
			if !hasProblemContaining(problems, endpoint+" \""+dead+"\"") {
				t.Fatalf("the report does not name the %s endpoint; problems: %v", endpoint, problems)
			}
		})

		t.Run(endpoint+" reaches the evidence check", func(t *testing.T) {
			t.Parallel()
			edges, _ := parseOrderEdges([][]string{
				edgeRowFull("prodA", "p.go", "readerA", "r.go", edgeClassConsumer, "yes"),
			})
			// Everything is live, so a problem can only come from the evidence
			// resolver — and it rejects only the endpoint under test. This is the
			// arm that would stay green if that endpoint's evidence column were
			// simply never passed down.
			var sawWhat []string
			problems := checkEdgeEndpoints(edges,
				func(string) bool { return true },
				func(evidence, what, name string) string {
					sawWhat = append(sawWhat, what)
					if what != endpoint {
						return ""
					}
					return "cited " + name + " to " + evidence + " which does not declare it"
				})
			if !hasProblemContaining(problems, "which does not declare it") {
				t.Fatalf("the %s evidence column never reached the evidence check; "+
					"the resolver saw %v, problems: %v\n"+
					"  An endpoint anchored only by the repo-wide identifier set is "+
					"anchored by nothing a rename or a mis-citation would disturb.",
					endpoint, sawWhat, problems)
			}
			if len(problems) != 1 {
				t.Fatalf("want exactly the %s evidence problem, got %v", endpoint, problems)
			}
		})
	}

	// A type, not a func. The existence check accepts carrier types, so the
	// per-file anchor must too, or a legitimate row would be unfixable except by
	// deleting its citation.
	t.Run("a carrier type anchors as well as a func", func(t *testing.T) {
		t.Parallel()
		tree := sourceTreeRoot(t)
		if p := endpointDeclaredIn(tree, "pkg/relational/core/embedded/select_parser.go",
			"producer", "projCol"); p != "" {
			t.Fatalf("a type-valued producer was rejected by its own declaring file: %s", p)
		}
	})
}
