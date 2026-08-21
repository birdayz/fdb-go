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
// NOT COVERED — read this list first, because it is larger than the positive
// claim and the positive claim has already been too broad once:
//
//   - THE SPELLING SET IS FINITE. The gate matches `.MetadataName()`,
//     `.FieldNames()`, `rt.Name` and `.RecordType.Name`. A renderer that reaches
//     a stored name any other way is invisible to it. This is not hypothetical:
//     the gate's FIRST version matched only the two method calls, and
//     `rec.RecordType.Name` walked straight past it into `record scan -o json`'s
//     `record_type` — the field the guide tells operators to pipe into `--type`,
//     i.e. into `record delete`. Adding a spelling is the fix; assuming the set
//     is complete is the bug.
//   - IT CANNOT TELL A RENDER FROM A LOOKUP, and a lookup must key by the stored
//     name. Lines naming a lookup helper are allowed, so a genuinely new lookup
//     spelling slips through. Extend the allowlist, with the reason written next
//     to it.
//   - THE `// storage-compare` MARKER IS TRUSTED, NOT VERIFIED. The gate checks
//     that the line ENDS with it — anchored, because an unanchored check was
//     laundered two ways on the first try: past-tense prose
//     (`// storage-compared above, so this is fine`) and the token inside a
//     STRING LITERAL. Anchoring stops both, but nothing stops someone marking a
//     genuine render. The marker records an intent that reading the line can
//     confirm; it does not prove one.
//   - IT IS LINE-BASED. A raw read on one line whose value is decoded on
//     another reads as a leak (see the `names[i] = rt.Name` allowance) or, worse,
//     the reverse.
//   - `.MetadataName()` IS ALSO DEFINED ON SCHEMA TEMPLATES AND SCHEMAS, whose
//     names are not protobuf-escaped and must NOT be decoded. Those are
//     allowlisted individually.
//
// What it does cover: a renderer in cmd/frl that reaches a stored record-type or
// column name through one of the four known spellings, without going through a
// decoding helper and without an explicit `// storage-compare` marker.
func TestFRLRenderersDecodeNamesThroughHelpers(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	dir := filepath.Join(root, "cmd", "frl", "internal", "cmd")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v — under Bazel this package's sources must be `data` "+
			"deps of this target, or the gate reports clean while reading nothing", dir, err)
	}

	// Spellings that reach a stored name. The struct-FIELD forms are here
	// because omitting them is how the highest-consequence leak survived this
	// gate's first version: `rec.RecordType.Name` is neither a .MetadataName()
	// nor a .FieldNames() call, so `record scan -o json` printed an ungated
	// record_type — the value the guide tells operators to pipe into
	// `record delete`.
	spellings := []string{".FieldNames()", ".MetadataName()", "rt.Name", ".RecordType.Name"}

	// A line may hold a raw name when it goes through an md-AWARE helper, or
	// when it is a LOOKUP keyed by the stored name.
	//
	// Bare userName( is NOT on this list and needs no exemption: it decodes
	// without consulting the declared set, so at a site holding an md it is the
	// very bug found at record.go. The helpers in stats.go that wrap it do not
	// trip the gate because none of their lines carry a flagged spelling -- if
	// that ever changes, add the exemption rather than allowlisting userName(.
	allowed := []string{
		"userFieldNames(", "userFieldName(", "userNameFor(", "userNamesFor(",
		"GetRecordType(",                      // lookup: keys by the stored name
		"RecordTypesForIndex(",                // lookup
		"EqualFold(tbl.MetadataName(), name)", // lookup: the stored-spelling arm
		// A SCHEMA TEMPLATE name is a third namespace: it comes from CREATE
		// SCHEMA TEMPLATE and is never protobuf-escaped, so decoding it would be
		// the same invented-name mistake this gate exists to prevent, pointing
		// the other way.
		"tpl.MetadataName()",
		// Collects stored names into a slice that this same function returns
		// through userNamesFor two lines later. A line-based gate cannot see
		// that; re-check by hand if index.go's recordTypeNames changes shape.
		"names[i] = rt.Name",
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
			var hit bool
			for _, sp := range spellings {
				if strings.Contains(line, sp) {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
			trimmed := strings.TrimSpace(line)
			// Comment lines only. A `func ` skip used to live here and was a hole:
			// `func typeName(rt *T) string { return rt.Name }` is a one-line render
			// that walked past with no marker at all.
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			// An explicit `// storage-compare` marker exempts a line: it reads a
			// stored name for COMPARISON, not for display. Decoding is not
			// injective — stored A__B and A__0B both render as A__B — so a diff
			// has to compare stored spellings or it drops real changes.
			//
			// This used to key on a `…Raw` variable-name convention, and that was
			// wrong: `nameRaw := rt.Name; return nameRaw` is a genuine render that
			// a natural variable name silently exempted. A marker nobody writes by
			// accident is the point; a naming habit is not.
			if strings.HasSuffix(trimmed, "// storage-compare") {
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
