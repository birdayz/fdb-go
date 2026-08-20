package docscheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// storeTimerEventSources are the files that declare recordlayer.Event values.
// A new file declaring events must be added here — TestStoreTimerEventFilesAreAllScanned
// fails if one appears that this list does not cover, so the list cannot silently
// go stale.
var storeTimerEventSources = []string{
	"pkg/recordlayer/store_timer.go",
	"pkg/recordlayer/spfresh_metrics.go",
	"pkg/recordlayer/sliding_window_index_maintainer.go",
}

// promMetricName is the character set Prometheus accepts for a metric name.
// rlmetrics builds its metric names by concatenating a fixed namespace, the
// event name, and a fixed suffix, so the event name is the only part that can
// make the result illegal — and an illegal name does not degrade gracefully:
// the whole scrape is rejected, taking every OTHER metric down with it.
var promMetricName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// TestStoreTimerEventsAreClassified pins the two properties every declared
// instrumentation event must have for the Prometheus exporter to render it
// correctly: a Kind, and a name that survives concatenation into a metric name.
//
// This is an AST scan rather than a list of events because a list is exactly
// what rots. The failure mode being guarded against is silent: a new event
// declared with no Kind still compiles if written with keyed fields, still
// records fine, and still appears on the scrape — just typed as a counter when
// it may be a duration.
func TestStoreTimerEventsAreClassified(t *testing.T) {
	t.Parallel()
	// sourceTreeRoot, not repoRoot: under Bazel the latter lands in the runfiles
	// tree, which stages this test's data deps and nothing else — the record
	// layer's sources are not there, so every read fails. sourceTreeRoot follows
	// the staged MODULE.bazel symlink back to the real workspace, which is how the
	// other source-scanning gates in this package reach the tree they check.
	root := sourceTreeRoot(t)

	total := 0
	for _, rel := range storeTimerEventSources {
		for _, ev := range parseEventLiterals(t, filepath.Join(root, rel), rel) {
			total++
			if ev.kind == "" || ev.kind == "KindUnspecified" {
				t.Errorf("%s: event %q has no Kind. Every event needs one — the exporter reads it to "+
					"decide between a duration summary and a counter, and an unclassified event is "+
					"exported as a counter even when it is a timing.", ev.pos, ev.name)
			}
			if !promMetricName.MatchString(ev.name) {
				t.Errorf("%s: event name %q is not a legal Prometheus metric-name fragment (%s). "+
					"rlmetrics concatenates it into fdb_recordlayer_<name>_total; an illegal "+
					"character there makes the ENTIRE scrape unparseable, not just this metric.",
					ev.pos, ev.name, promMetricName)
			}
		}
	}

	// A floor, not an exact count: it fails if the scan silently stops finding
	// events (a refactor to a constructor function, say) while leaving every
	// other assertion in this test vacuously true.
	if total < 40 {
		t.Errorf("found only %d event declarations across %v; the scan has stopped seeing them, "+
			"which makes every check in this test vacuous", total, storeTimerEventSources)
	}
}

// TestStoreTimerEventFilesAreAllScanned keeps storeTimerEventSources honest: any
// file in pkg/recordlayer declaring an Event literal must be on the list, or the
// classification test above simply would not look at it.
func TestStoreTimerEventFilesAreAllScanned(t *testing.T) {
	t.Parallel()
	root := sourceTreeRoot(t)

	listed := map[string]bool{}
	for _, rel := range storeTimerEventSources {
		listed[rel] = true
	}

	// The tracked-file enumeration is shared with the other source gates, so this
	// test sees exactly the files they do — including a newly added one, which is
	// the case that matters.
	prefix := filepath.Join("pkg", "recordlayer") + string(filepath.Separator)
	var scanned int
	for _, rel := range trackedGoFiles(t, root) {
		if !strings.HasPrefix(rel, prefix) || strings.HasSuffix(rel, "_test.go") {
			continue
		}
		// Top-level package files only; sub-packages have their own event sets if
		// they ever grow one, and would need their own entry here.
		if strings.Contains(strings.TrimPrefix(rel, prefix), string(filepath.Separator)) {
			continue
		}
		scanned++
		if listed[rel] {
			continue
		}
		if evs := parseEventLiterals(t, filepath.Join(root, rel), rel); len(evs) > 0 {
			t.Errorf("%s declares %d recordlayer.Event value(s) but is not in storeTimerEventSources, "+
				"so its events are never checked for a Kind or a legal metric name", rel, len(evs))
		}
	}

	// Anti-vacuity floor. A scan that has stopped seeing pkg/recordlayer would pass
	// this test in silence while checking nothing, which is the failure mode the
	// test exists to prevent one level down.
	const minFiles = 50
	if scanned < minFiles {
		t.Fatalf("scanned %d non-test files under %s, want at least %d — that is not the real "+
			"package, so this gate cleared code it never looked at", scanned, prefix, minFiles)
	}
}

type eventLiteral struct {
	name string // the Event's Name field, e.g. "save_record"
	kind string // the Kind identifier, e.g. "KindTimed"; empty when absent
	pos  string // file:line
}

// parseEventLiterals finds `Event{...}` composite literals, in both the unkeyed
// form the package uses and the keyed form, and reports the name and kind of each.
func parseEventLiterals(t *testing.T, path, rel string) []eventLiteral {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}

	var out []eventLiteral
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		ident, ok := lit.Type.(*ast.Ident)
		if !ok || ident.Name != "Event" {
			return true
		}

		ev := eventLiteral{pos: rel + ":" + strconv.Itoa(fset.Position(lit.Pos()).Line)}
		for i, elt := range lit.Elts {
			switch e := elt.(type) {
			case *ast.KeyValueExpr:
				key, _ := e.Key.(*ast.Ident)
				if key == nil {
					continue
				}
				switch key.Name {
				case "Name":
					ev.name = stringLit(e.Value)
				case "Kind":
					ev.kind = identName(e.Value)
				}
			default:
				// Unkeyed: Event{Name, Title, Kind}.
				switch i {
				case 0:
					ev.name = stringLit(e)
				case 2:
					ev.kind = identName(e)
				}
			}
		}
		out = append(out, ev)
		return true
	})
	return out
}

func stringLit(e ast.Expr) string {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return ""
	}
	s, err := strconv.Unquote(bl.Value)
	if err != nil {
		return ""
	}
	return s
}

func identName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return v.Sel.Name
	}
	return ""
}
