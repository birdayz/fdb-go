package docscheck

// `logical.LogicalAggregate` carries two fields of PARSE-TIME provenance —
// `CallProvenance` and `CallProvenanceCols` — recorded by the aggregate-call producer
// and consumed by operand resolution (RFC-241, which removed a silent wrong
// answer caused by reconstructing that correspondence from folded text).
//
// Carrying them is only safe because the type never reaches the memo. If it did,
// two structurally identical `GroupByExpression`s differing ONLY in parser
// provenance could land in different memo groups — splitting the memo on a parse
// accident, which is a plan-quality defect with no visible symptom.
//
// That holds by construction today rather than by discipline: `LogicalAggregate`
// is a pre-memo lowering draft, translated into `expressions.GroupByExpression`
// before anything memo-facing sees it. This asserts the construction, so the
// safety argument fails loudly if the layering changes instead of silently
// becoming false.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogicalAggregateStaysOutOfTheMemoPackage(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)

	const (
		memoPkg = "pkg/recordlayer/query/plan/cascades"
		subject = "LogicalAggregate"
		// The control is a type that IS memo-facing, swept over the same tree.
		// Without it a zero cannot be told from a walk that reached no files —
		// the dominant false positive in this repo, and the reason this gate
		// asserts a non-zero as well as a zero.
		control = "GroupByExpression"
	)
	// TWO ROUTES, and an earlier version of this gate walked only the first.
	// The memo package importing the type is one way the provenance reaches
	// memo identity; the TRANSLATOR copying a field off it onto a memo node —
	// `gb.X = agg.CallProvenance` in cascades_translator.go — is the other, and
	// it happens entirely outside the memo package. Watching one directory is
	// the same subset failure that sank three drafts of the RFC-241 static gate.
	roots := []string{memoPkg, "pkg/relational/core/query"}

	subjectHits, controlHits, scanned := 0, 0, 0
	provenanceLeaks := map[string]int{}
	// Per-root, because a total summed across roots cannot tell "walked both"
	// from "walked one and missed the other" — and the second root was added
	// precisely because watching one directory is how the earlier drafts of this
	// gate held a strict subset of the property.
	scannedPerRoot := map[string]int{}
	for _, rel := range roots {
		walkErr := filepath.Walk(filepath.Join(root, rel), func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			b, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			scanned++
			scannedPerRoot[rel]++
			body := string(b)
			if rel == memoPkg {
				subjectHits += strings.Count(body, subject)
				controlHits += strings.Count(body, control)
			}
			// The field names themselves, anywhere outside the file that DECLARES
			// them. The declaration site is exempt by construction; every other
			// reference under these roots is a route by which parse-time state
			// could reach a memo node.
			if strings.HasSuffix(path, filepath.Join("query", "logical", "operators.go")) {
				return nil
			}
			for _, field := range []string{"CallProvenance", "CallProvenanceCols", "HasCallProvenance"} {
				if n := strings.Count(body, field); n > 0 {
					provenanceLeaks[path] += n
				}
			}
			return nil
		})
		if walkErr != nil {
			t.Fatalf("walking %s: %v", rel, walkErr)
		}
	}
	if len(provenanceLeaks) > 0 {
		for path, n := range provenanceLeaks {
			t.Errorf("%s references the aggregate provenance fields %d time(s). Those fields are "+
				"parse-time state owned by the lowering pass that consumes them; a reference from "+
				"the translator or the memo means they can reach GroupByExpression identity, and two "+
				"structurally identical aggregates differing only in provenance would then land in "+
				"different memo groups.", path, n)
		}
	}

	for _, rel := range roots {
		if scannedPerRoot[rel] == 0 {
			t.Fatalf("walked %s and found no .go files. Each root needs its own floor: a "+
				"total summed across roots reports the same number whether both were read or "+
				"only one was, and this gate exists because watching one directory missed the "+
				"translator route entirely.", rel)
		}
	}
	if scanned == 0 {
		t.Fatalf("walked %s and found no .go files. A gate that reads nothing reports the "+
			"same green as a clean tree; this floor is what separates them.", memoPkg)
	}
	if controlHits == 0 {
		t.Fatalf("the control %q appears 0 times across %d file(s) in %s. The memo package "+
			"unquestionably names its own expression type, so a zero here means the sweep is "+
			"broken and the verdict below is noise.", control, scanned, memoPkg)
	}
	if subjectHits != 0 {
		t.Errorf("`logical.%s` is referenced %d time(s) in %s (control %q: %d, files: %d).\n\n"+
			"RFC-241 put parse-time provenance (CallProvenance, CallProvenanceCols) on that type, and "+
			"the argument that it is safe there is precisely that the type never reaches the memo. "+
			"A reference here means those fields can now participate in memo identity, so two "+
			"GroupByExpressions differing only in parser provenance would land in different groups.\n\n"+
			"Either keep the type out of this package, or move the provenance off it.",
			subject, subjectHits, memoPkg, control, controlHits, scanned)
	}
}
