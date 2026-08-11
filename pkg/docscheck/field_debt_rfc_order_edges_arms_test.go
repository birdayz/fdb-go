package docscheck

import "testing"

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
	return []string{producer, reader, class, retires, evidence}
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
		if !hasProblemContaining(problems, "want 5") {
			t.Fatalf("a 3-cell row was accepted; problems: %v", problems)
		}
	})

	t.Run("non-identifier cell reported", func(t *testing.T) {
		t.Parallel()
		rows := goodEdgeRows()
		rows[0] = []string{"the star expander", "`readerA`", edgeClassConsumer, "yes", "a.go"}
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

	// readerDeclaredIn's rejection arms. The accept arm is exercised by the live
	// gate against the real table; these are the ones the corpus never reaches.
	t.Run("evidence must be a go file", func(t *testing.T) {
		t.Parallel()
		if p := readerDeclaredIn(".", "rfcs/197-column-identity-is-an-ordinal.md", "legRef"); p == "" {
			t.Fatal("a non-.go evidence path was accepted")
		}
		if p := readerDeclaredIn(".", "", "legRef"); p == "" {
			t.Fatal("an empty evidence path was accepted")
		}
	})
}
