package docscheck

import (
	"fmt"
	"go/scanner"
	"go/token"
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
//   - THE WALK'S MASKING IS NOT CORPUS-DRIVEN. Replacing maskLiterals with a
//     plain line split in the corpus loop leaves this test green, because
//     cmd/frl currently has no line where masking changes the verdict (measured
//     over its 31 non-test files: exactly ONE line carries a spelling only
//     inside a literal -- index.go:241, a full-line comment already exempt).
//     TestNameLineClassification drives the classifier and
//     TestMaskingCarriesLexicalStateAcrossLines drives the masker; the WIRING
//     between them rests on those two, not on the corpus.
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
		// Masked at FILE level, so a raw string or comment spanning lines is one
		// construct rather than something re-lexed per line.
		maskedLines := strings.Split(maskLiterals(string(b)), "\n")
		for i, line := range strings.Split(string(b), "\n") {
			if i >= len(maskedLines) {
				t.Fatalf("%s: masked source has %d lines but raw has more; the mask "+
					"changed the line count, which misaligns every classification", n, len(maskedLines))
			}
			if nameLineIsLeak(line, maskedLines[i]) {
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
		"block comment with an apostrophe":   `	shown := rt.Name /* don't */ // md.GetRecordType( earlier`,
		"block comment with a backtick":      `	shown := rt.Name /* a ` + "`" + ` b */ // md.GetRecordType( earlier`,
		"block comment with a quote":         `	shown := rt.Name /* a " b */ // md.GetRecordType( earlier`,
		"allowlist token in a string":        `	fmt.Fprintln(out, "userNameFor(", rt.Name)`,
		"allowlist token in a block comment": `	fmt.Fprintln(out, rt.Name) /* md.GetRecordType( */`,
		"CR-padded line comment":             "\tfmt.Fprintln(out, rt.Name) //" + strings.Repeat("\r", 12) + "userNameFor(",
		"CR-padded block comment":            "\tfmt.Fprintln(out, rt.Name) /*" + strings.Repeat("\r", 16) + "md.GetRecordType(*/",
		"CR-padded raw string":               "\tfmt.Fprintln(out, `" + strings.Repeat("\r", 13) + "userNameFor(`, rt.Name)",
		"CR inside a block comment":          "\tfmt.Fprintln(out, rt.Name) /* // *" + "\r" + "/ userNameFor( */",
	}
	for name, line := range leaks {
		if !nameLineIsLeak(line, maskedLine(line)) {
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
		"a comment line":                "// ",
		"URL in a string literal":       "userNameFor(md, ",
	}
	// Iterate EXEMPT, not tokens: keying the loop on the token map let a fixture
	// with no entry go unguarded, which is how this guard covered 9 of 11 and
	// then 12 of 13 while claiming "every". A fixture that legitimately has no
	// token names itself here.
	for name, line := range exempt {
		token, ok := tokens[name]
		if !ok {
			t.Errorf("%s: no entry in `tokens`, so nothing checks that this fixture "+
				"reaches the allowlist at all. Add the exemption token it relies on, so "+
				"removing it can be shown to leave a leak:\n  %s", name, line)
			continue
		}
		bare := strings.Replace(line, token, "", 1)
		if !nameLineIsLeak(bare, maskedLine(bare)) {
			t.Errorf("%s: the fixture is exempt for the WRONG reason — with its token "+
				"removed it is still not a leak, so it carries no tracked spelling and "+
				"never reaches the allowlist:\n  %s", name, bare)
		}
	}
	for name, line := range exempt {
		if nameLineIsLeak(line, maskedLine(line)) {
			t.Errorf("%s: should be exempt, was flagged as a leak:\n  %s", name, line)
		}
	}

	// Vacuity guard: if the spelling set were emptied, every "leak" above would
	// silently become exempt and only this check would notice.
	if nameLineIsLeak(`	x := 1`, maskedLine(`	x := 1`)) {
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
func nameLineIsLeak(line, masked string) bool {
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

	// The SPELLING check runs on the RAW line, not the masked code: masking here
	// would fail OPEN (a spelling hidden in a literal is not a render, but so is
	// a spelling the mask mis-locates). Raw fails CLOSED -- worst case something
	// harmless is flagged and a human looks. Only the ALLOWLIST, which grants
	// exemptions, is masked; that is the direction where a mistake is silent.
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
	// Allowlist tokens count only in the CODE, never in a comment: unanchored,
	// `fmt.Fprintln(out, rt.Name) // resolved via md.GetRecordType( earlier`
	// exempts a genuine render by naming a lookup in prose.
	//
	// Finding where the code ends is GO LEXING, and hand-rolling it failed three
	// times in a row -- each fix reopening the hole it closed. Taking the first
	// `//` truncated inside `"https://%s"` and rejected valid code (loud). Adding
	// literal tracking made a rune's escaped quote reopen the literal (silent).
	// Fixing that left `/* don't */` opening a literal that never closes, which
	// is silent too and was a REGRESSION against the naive version.
	//
	// So it is not hand-rolled any more. go/scanner is the same lexer the
	// compiler uses; every quote, escape, raw string and block comment is its
	// problem, not this gate's.
	code := masked
	for _, a := range allowed {
		if strings.Contains(code, a) {
			return false
		}
	}
	return true
}

// maskLiterals blanks every COMMENT, STRING and CHAR token in src, preserving
// byte offsets and newlines so the result can be split into lines that line up
// with the original.
//
// It takes WHOLE SOURCE, not a line, and that is load-bearing. Go is not
// line-oriented: a raw string opened on one line makes the next line's backtick
// a CLOSER, and a per-line scanner reads it as an opener — blanking a real
// helper call and rejecting valid code.
//
// Spans run to the offset of the next NON-INSERTED token, never to len(lit).
// Two separate traps in one line. len(lit) is short because the scanner applies
// stripCR to `//` comments, general comments and raw strings, so blanking that
// many bytes leaves an attacker-chosen tail exposed. And the plain "next token"
// is wrong too: automatic semicolon insertion puts a synthetic SEMICOLON at a
// general comment's first newline, an offset INSIDE the comment, so stopping
// there blanks only its first line.
//
// And CRs are NOT deleted to work around that. Deleting them changes
// tokenization rather than reconciling spans: `*<CR>/` does not close a block
// comment in Go, so removing the CR creates `*/`, ends the comment early, and
// leaves the rest of it unmasked —
// `fmt.Fprintln(out, rt.Name) /* // *<CR>/ userNameFor( */` was exempted that
// way. Offsets from the original bytes need no such trick.
//
// Errors from the scanner are ignored: only token KIND and POSITION matter and
// it keeps producing both, which is why the handler is nil rather than missing.
func maskLiterals(src string) string {
	var sc scanner.Scanner
	fset := token.NewFileSet()
	f := fset.AddFile("", fset.Base(), len(src))
	sc.Init(f, []byte(src), nil, scanner.ScanComments)

	type tokenSpan struct {
		off      int
		blank    bool
		inserted bool // auto-inserted semicolon: a position, not a boundary
	}
	var toks []tokenSpan
	for {
		pos, tok, lit := sc.Scan()
		off := fset.Position(pos).Offset
		if tok == token.EOF {
			toks = append(toks, tokenSpan{off: len(src)})
			break
		}
		toks = append(toks, tokenSpan{
			off:   off,
			blank: tok == token.COMMENT || tok == token.STRING || tok == token.CHAR,
			// go/scanner performs automatic semicolon insertion, and for a general
			// comment containing newlines it emits that semicolon at the comment's
			// FIRST newline -- an offset INSIDE the token that precedes it. Treat
			// that as a span boundary and only the comment's first line is blanked:
			//
			//	_ = 1 /* opened here
			//	md.GetRecordType( */ fmt.Fprintln(out, rt.Name)
			//
			// left line 2 byte-identical to the source, so a helper name in comment
			// PROSE exempted a genuine render. An inserted semicolon carries the
			// literal "\n"; a real one carries ";".
			inserted: tok == token.SEMICOLON && lit == "\n",
		})
	}

	out := []byte(src)
	for i, t := range toks {
		if !t.blank {
			continue
		}
		// The next REAL boundary, stepping over inserted semicolons.
		end := len(src)
		for j := i + 1; j < len(toks); j++ {
			if toks[j].inserted {
				continue
			}
			end = toks[j].off
			break
		}
		for j := t.off; j < end && j < len(out); j++ {
			// Newlines survive so line numbering does.
			if out[j] != '\n' {
				out[j] = ' '
			}
		}
	}
	return string(out)
}

// maskedLine is maskLiterals over one line, for the unit fixtures only.
//
// It does NOT generally agree with masking the same line inside its file, and
// the difference is not limited to unterminated constructs: a perfectly ordinary
// `x := rt.Name` sitting INSIDE a multi-line raw string masks to spaces in the
// file and to itself alone. The fixtures are self-contained single lines, which
// is why this is sound for them and why the corpus walk masks whole files.
func maskedLine(line string) string { return maskLiterals(line) }

// MASKING IS A WHOLE-FILE OPERATION, BECAUSE GO IS NOT LINE-ORIENTED.
//
// A raw string opened on one line makes the NEXT line's backtick a closer. Lex
// that line alone and the backtick reads as an OPENER, so everything after it
// gets blanked — including a real decoding helper — while the raw spelling
// before it still counts. The gate then rejects valid, correctly decoded code.
//
// This cannot be expressed against a single line, which is exactly why the
// per-line version survived so long.
func TestMaskingCarriesLexicalStateAcrossLines(t *testing.T) {
	t.Parallel()

	src := "package p\n" +
		"var banner = `opened here\n" +
		"still inside` + userNameFor(md, rt.Name)\n"

	masked := strings.Split(maskLiterals(src), "\n")
	raw := strings.Split(src, "\n")
	if len(masked) != len(raw) {
		t.Fatalf("masking changed the line count (%d vs %d); every classification "+
			"would misalign", len(masked), len(raw))
	}

	// Line 3 closes the raw string and then makes a real, correctly decoded
	// call. It must NOT be reported as a leak.
	const closing = 2
	if !strings.Contains(raw[closing], "userNameFor(") {
		t.Fatalf("fixture drifted: line %d is %q", closing, raw[closing])
	}
	if nameLineIsLeak(raw[closing], masked[closing]) {
		t.Errorf("the closing line of a multi-line raw string was flagged:\n  raw    %q\n"+
			"  masked %q\nlexing it alone makes the closing backtick an opener, which "+
			"blanks the helper call that exempts it", raw[closing], masked[closing])
	}
	// And the same line lexed ALONE is misread — the reason file-level matters.
	if !nameLineIsLeak(raw[closing], maskedLine(raw[closing])) {
		t.Log("note: the single-line mask now agrees; if that becomes permanent the " +
			"file-level requirement may be re-examined, but do not assume it")
	}
}

// A MULTILINE BLOCK COMMENT IS BLANKED TO ITS END, NOT TO ITS FIRST NEWLINE.
//
// go/scanner performs automatic semicolon insertion, and for a general comment
// containing newlines it emits that semicolon at the comment's FIRST newline —
// an offset INSIDE the comment. Taking the next token's offset as the span end
// then blanks only the first line, and a helper name sitting in comment PROSE
// exempts a real render on the line where the comment closes.
//
// Found independently by two reviews on the same day; the fix skips inserted
// semicolons, which carry the literal "\n" where a real one carries ";".
func TestMultilineBlockCommentIsFullyMasked(t *testing.T) {
	t.Parallel()

	src := "package p\n" +
		"func f() {\n" +
		"\t_ = 1 /* opened here\n" +
		"md.GetRecordType( */ fmt.Fprintln(out, rt.Name)\n" +
		"}\n"

	raw := strings.Split(src, "\n")
	masked := strings.Split(maskLiterals(src), "\n")
	if len(raw) != len(masked) {
		t.Fatalf("masking changed the line count: %d vs %d", len(raw), len(masked))
	}

	const closing = 3 // the line that closes the comment and then renders
	if !strings.Contains(raw[closing], "md.GetRecordType(") ||
		!strings.Contains(raw[closing], "rt.Name") {
		t.Fatalf("fixture drifted: line %d is %q", closing, raw[closing])
	}
	if masked[closing] == raw[closing] {
		t.Fatalf("the comment tail was not blanked at all — masked line is byte-"+
			"identical to source:\n  %q", masked[closing])
	}
	if strings.Contains(masked[closing], "md.GetRecordType(") {
		t.Errorf("the allowlist token survives in comment prose, so it exempts the "+
			"real rt.Name render on the same line:\n  raw    %q\n  masked %q",
			raw[closing], masked[closing])
	}
	if !nameLineIsLeak(raw[closing], masked[closing]) {
		t.Errorf("a genuine rt.Name render was exempted by a token in comment prose:\n"+
			"  raw    %q\n  masked %q", raw[closing], masked[closing])
	}
}
