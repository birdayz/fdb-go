package docscheck

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// NO frl RENDERER MAY REACH PAST THE NAME-DECODING HELPERS.
//
// Record-type and column names are stored ESCAPED (protoname.ToProtoBufCompliantName):
// a table quoted "MY$TABLE" is stored MY__1TABLE. Every operator-facing render
// decodes, so a name can be copied out of one command and typed into another.
//
// This gate exists because the alternative did not work. The decode sites were
// tracked as a LIST inside a comment, and that list was wrong three times: it
// said FOUR, was repaired to THREE by subtraction instead of re-sweeping, and
// still omitted the sql.go sites and meta_diff's sortSection. Separately, column
// names stayed raw in four files at once — meta.go, meta_types_describe.go,
// meta_diff.go, index_describe.go — with a fully green suite, because a comment
// cannot fail and fixture arms only see the sites they happen to drive.
//
// So the property is asserted on the SOURCE instead: a bare .FieldNames() or
// .MetadataName() reaching a renderer is a leak by construction.
//
// NOT covered, deliberately: this cannot tell a render from a LOOKUP, and a
// lookup must key by the stored name. Lines that name a lookup helper are
// allowed, which means a genuinely new lookup spelling would slip through — the
// allowlist below is the thing to extend when that happens, not the gate.
func TestFRLRenderersDecodeNamesThroughHelpers(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	dir := filepath.Join(root, "cmd", "frl", "internal", "cmd")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v — under Bazel this package's sources must be `data` "+
			"deps of this target, or the gate reports clean while reading nothing", dir, err)
	}

	// A line may hold a raw name when it goes through a decoding helper, or when
	// it is a LOOKUP keyed by the stored name.
	allowed := []string{
		"userFieldNames(", "userName(", "userNameFor(", "userNamesFor(",
		"GetRecordType(",                      // lookup: keys by the stored name
		"RecordTypesForIndex(",                // lookup
		"EqualFold(tbl.MetadataName(), name)", // lookup: the stored-spelling arm
		// A SCHEMA TEMPLATE name is a third namespace: it comes from CREATE SCHEMA
		// TEMPLATE and is never protobuf-escaped, so decoding it would be the same
		// invented-name mistake this gate exists to prevent, pointing the other way.
		"tpl.MetadataName()",
	}
	var scanned int
	var leaks []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatalf("read %s: %v", n, err)
		}
		scanned++
		for i, line := range strings.Split(string(b), "\n") {
			if !strings.Contains(line, ".FieldNames()") && !strings.Contains(line, ".MetadataName()") {
				continue
			}
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "func ") {
				continue
			}
			var ok bool
			for _, a := range allowed {
				if strings.Contains(line, a) {
					ok = true
					break
				}
			}
			if !ok {
				leaks = append(leaks, fmt.Sprintf("%s:%d: %s", n, i+1, trimmed))
			}
		}
	}

	// VACUITY GUARD. Under Bazel a test runs in a runfiles tree holding only its
	// declared inputs, so without the `data` dep this loop reads an empty
	// directory and reports clean — passing while checking nothing. The first
	// run of this gate did exactly that and this guard is what caught it.
	if scanned < 10 {
		t.Fatalf("scanned only %d non-test .go files under %s; the gate is not "+
			"reaching the package and would report clean regardless", scanned, dir)
	}
	for _, l := range leaks {
		t.Errorf("frl renders a stored name without a decoding helper:\n  %s\n"+
			"  Wrap it (userFieldNames/userName/userNameFor/userNamesFor), or if it "+
			"is a LOOKUP rather than a render, add its spelling to this gate's "+
			"allowlist with a note saying why it keys by the stored name.", l)
	}
}
