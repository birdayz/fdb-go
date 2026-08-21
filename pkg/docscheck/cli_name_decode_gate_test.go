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
//   - IT SCANS cmd/frl ONLY. Other packages render record-type names too, and
//     one of them was wrong the whole time this gate existed: the fleet
//     package's skipped-types string decoded with a bare ToUserIdentifier into
//     a map keyed by the decoded name, losing a row outright on a collision.
//     Widening the walk means curating an allowlist per package; until that
//     happens, a renderer outside cmd/frl carries the invariant by construction
//     and by its own test, not by this gate.
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
			if nameLineIsLeak(line) {
				leaks = append(leaks, fmt.Sprintf("%s:%d: %s", n, i+1, strings.TrimSpace(line)))
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

// EVERY ARM OF THE GATE IS DRIVEN HERE, not by whatever the corpus contains.
//
// The corpus walk above only exercises the arms its files happen to hit. That
// is not coverage: the one-line-function arm had NO instance in cmd/frl, so
// restoring the `func ` exemption — the exact fail-open that once let
// `func typeName(rt *T) string { return rt.Name }` through — left the gate
// green. Every shape below is a real leak or a real exemption, so each arm
// fails loudly when it regresses.
func TestNameLineClassification(t *testing.T) {
	t.Parallel()

	leaks := map[string]string{
		"one-line function body":             `func typeName(rt *recordlayer.RecordType) string { return rt.Name }`,
		"struct field render":                `	rt = rec.RecordType.Name`,
		"plain field render":                 `	shown := rt.Name`,
		"method render":                      `	names[i] = idx.RootExpression.FieldNames()[0]`,
		"metadata name render":               `	out.name = tbl.MetadataName()`,
		"marker in a string":                 `	return fmt.Sprintf("%s // storage-compare", rt.Name)`,
		"past-tense prose":                   `	shown := rt.Name // storage-compared above, so this is fine`,
		"marker not at line end":             `	shown := rt.Name // storage-compare (see above)`,
		"unspaced marker":                    `	shown := rt.Name //storage-compare`,
		"block-comment marker":               `	shown := rt.Name /* storage-compare */`,
		"allowlist token in prose":           `	fmt.Fprintln(out, rt.Name) // resolved via md.GetRecordType( earlier`,
		"rune literal with an escaped quote": `	sep := '\''; fmt.Fprintln(out, rt.Name) // md.GetRecordType( earlier`,
	}
	for name, line := range leaks {
		if !nameLineIsLeak(line) {
			t.Errorf("%s: should be flagged as a leak, was exempted:\n  %s", name, line)
		}
	}

	// EVERY exemption fixture must contain a tracked SPELLING as well as its
	// exemption token. Without the spelling, nameLineIsLeak returns at the !hit
	// check and the arm passes without ever reaching the allowlist — it proves
	// the line is uninteresting, not that the exemption works. Four of these
	// were written that way and pinned nothing.
	exempt := map[string]string{
		"decoded through userNameFor":   `	rt = userNameFor(md, rec.RecordType.Name)`,
		"decoded through userNamesFor":  `	return userNamesFor(md, []string{rt.Name})`,
		"decoded through userFieldName": `	name: userFieldName(col.MetadataName())`,
		"field helper":                  `	return userFieldNames(idx.RootExpression.FieldNames())`,
		"anchored marker":               `	oldRaw := oldI.RootExpression.FieldNames() // storage-compare`,
		"lookup by stored name":         `	if rt := md.GetRecordType(tbl.MetadataName()); rt != nil {`,
		"lookup for index types":        `	for _, rt := range md.RecordTypesForIndex(idx) { _ = rt.Name }`,
		"EqualFold stored-name arm":     `	if !strings.EqualFold(tbl.MetadataName(), name) {`,
		"collected for later decoding":  `		names[i] = rt.Name`,
		"schema template namespace":     `	return fmt.Errorf("%s", tpl.MetadataName())`,
		"a comment line":                `	// rt.Name is the escaped storage name`,
		"prose naming a lookup":         `	fmt.Fprintln(out, userNameFor(md, rt.Name)) // md.GetRecordType( earlier`,
		"URL in a string literal":       `	fmt.Fprintf(out, "https://%s", userNameFor(md, rt.Name))`,
	}
	// Guard: an exemption fixture that carries no tracked spelling is exempt for
	// the WRONG reason, so assert each one would be a leak with its token removed.
	tokens := map[string]string{
		"decoded through userNameFor":   "userNameFor(md, ",
		"decoded through userNamesFor":  "userNamesFor(md, ",
		"decoded through userFieldName": "userFieldName(",
		"field helper":                  "userFieldNames(",
		"anchored marker":               " // storage-compare",
		"lookup by stored name":         "md.GetRecordType(",
		"lookup for index types":        "md.RecordTypesForIndex(idx)",
		"EqualFold stored-name arm":     "strings.EqualFold(",
		"schema template namespace":     "tpl",
		"collected for later decoding":  "names[i] = ",
		"prose naming a lookup":         "userNameFor(md, ",
		"URL in a string literal":       "userNameFor(md, ",
	}
	// Iterate EXEMPT, not tokens: keying the loop on the token map let a fixture
	// with no entry go unguarded, which is how this guard covered 9 of 11 and
	// then 12 of 13 while claiming "every". A fixture that legitimately has no
	// token names itself here.
	noToken := map[string]string{
		"a comment line": "it is exempt by being a comment, which has no token to strip",
	}
	for name, line := range exempt {
		token, ok := tokens[name]
		if !ok {
			if _, expected := noToken[name]; expected {
				continue
			}
			t.Errorf("%s: no entry in `tokens`, so nothing checks that this fixture "+
				"reaches the allowlist at all. Add its exemption token, or name it in "+
				"`noToken` with the reason:\n  %s", name, line)
			continue
		}
		bare := strings.Replace(line, token, "", 1)
		if !nameLineIsLeak(bare) {
			t.Errorf("%s: the fixture is exempt for the WRONG reason — with its token "+
				"removed it is still not a leak, so it carries no tracked spelling and "+
				"never reaches the allowlist:\n  %s", name, bare)
		}
	}
	for name, line := range exempt {
		if nameLineIsLeak(line) {
			t.Errorf("%s: should be exempt, was flagged as a leak:\n  %s", name, line)
		}
	}

	// Vacuity guard: if the spelling set were emptied, every "leak" above would
	// silently become exempt and only this check would notice.
	if nameLineIsLeak(`	x := 1`) {
		t.Error("a line with no tracked spelling was flagged; the classifier is over-broad")
	}
}

// nameLineIsLeak classifies ONE source line: does it reach a stored
// record-type or column name without going through a decoding helper?
//
// Split out from the corpus walk so every arm can be driven directly. The arms
// were previously exercised only by whatever the scanned files happened to
// contain, which is not coverage: the one-line-function arm had NO instance in
// the corpus, so restoring the `func ` exemption that once let
// `func typeName(rt *T) string { return rt.Name }` through left the gate green.
// A gate whose branches depend on the corpus is a gate that fails open the day
// the corpus changes.
func nameLineIsLeak(line string) bool {
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

	var hit bool
	for _, sp := range spellings {
		if strings.Contains(line, sp) {
			hit = true
			break
		}
	}
	if !hit {
		return false
	}
	trimmed := strings.TrimSpace(line)
	// Comment lines only. A `func ` skip used to live here and was a hole:
	// `func typeName(rt *T) string { return rt.Name }` is a one-line render that
	// walked past with no marker at all.
	if strings.HasPrefix(trimmed, "//") {
		return false
	}
	// An explicit `// storage-compare` marker exempts a line: it reads a stored
	// name for COMPARISON, not display. Decoding is not injective — stored A__B
	// and A__0B both render as A__B — so a diff has to compare stored spellings
	// or it drops real changes.
	//
	// ANCHORED, because an unanchored check was laundered two ways on the first
	// try: past-tense prose (`// storage-compared above, so this is fine`) and
	// the token inside a STRING LITERAL. It also used to key on a `…Raw`
	// variable-name convention, which a maintainer trips by accident.
	if strings.HasSuffix(trimmed, "// storage-compare") {
		return false
	}
	// Allowlist tokens count only in the CODE, never in a trailing comment.
	// Unanchored, `fmt.Fprintln(out, rt.Name) // resolved via md.GetRecordType(
	// earlier` exempts a genuine render by mentioning a lookup in prose -- the
	// same laundering the storage-compare marker was anchored to stop, one check
	// lower down.
	// Find the real comment token: the first `//` outside a string, raw-string or
	// RUNE literal. Runes matter: gating the escape skip on `"` alone makes
	// `sep := '\\''` close on the escaped quote and reopen on the real one, so the
	// line reads as in-string to EOL, the comment is never stripped, and prose
	// tokens count again -- the exact laundering this stripping exists to stop,
	// reintroduced by the parser added to stop it.
	// Taking the first raw `//` truncates a line like
	// `fmt.Fprintf(out, "https://%s", userNameFor(md, rt.Name))` inside the
	// literal, dropping the allowlist token while the spelling check still sees
	// rt.Name -- the gate would then reject correct code and fail the build.
	code := trimmed
	var inQuote rune
	for i := 0; i < len(code); i++ {
		c := rune(code[i])
		switch {
		case inQuote != 0:
			if c == '\\' && inQuote != '`' {
				i++ // skip the escaped char
			} else if c == inQuote {
				inQuote = 0
			}
		case c == '"' || c == '`' || c == '\'':
			inQuote = c
		case c == '/' && i+1 < len(code) && code[i+1] == '/':
			code = code[:i]
			i = len(code) // done
		}
	}
	for _, a := range allowed {
		if strings.Contains(code, a) {
			return false
		}
	}
	return true
}
