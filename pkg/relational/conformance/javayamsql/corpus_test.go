package javayamsql_test

import (
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/conformance/javayamsql"
)

// pinnedCensus is the measured directive surface of the vendored corpus.
//
// It is a baseline, not a target. Its job is to make a corpus re-sync
// *describable*: "still parses" is a weak claim that a whole directive could
// vanish without disturbing, whereas a diff of these counts says exactly what
// upstream changed. Update it in the same commit as the re-sync.
// The census covers the files that PARSE, so its file count is the corpus total
// minus the parse-level negatives: 238 - 25 = 213. The three constants are
// asserted against each other below so they cannot drift apart.
const pinnedCensus = "files=213 " +
	"blocks{copy_block=4,include=25,options=62,schema_template=190,setup=208,test_block=330,transaction_setups=17} " +
	"queries=3018 " +
	"commands{load schema template=9,query=3018,set schema state=7} " +
	"configs{count=157,debugger=3,error=249,explain=1155,explainContains=50," +
	"initialVersionAtLeast=99,initialVersionLessThan=99,maxRows=38,result=1774," +
	"resultMetadata=312,setup=21,setupReference=65,supported_version=286,unorderedResult=599} " +
	"tags{!a=3,!b=8,!current_version=23,!f=14,!ignore=48,!in=5,!l=458,!n=10,!not_null=470," +
	"!null=327,!pos=137,!r=3,!randomStr=15,!sc=1,!uuid=180,!v16=216,!v32=46,!v64=26} " +
	"rows=6267 cells=13950 positional_cells=5836 segments=387 includes=25"

// pinnedFileCount is asserted separately from the census so that a corpus that
// gained or lost files fails with an obvious message rather than a 900-column
// string diff.
const pinnedFileCount = 238

// pinnedParseLevelNegatives is how many of those 238 the parser must refuse.
const pinnedParseLevelNegatives = 25

// TestCorpusParses is the gate: every vendored file is parsed, and each one
// either parses clean or is refused for the exact reason upstream refuses it.
//
// The polarity split is the substance here. A parser that rejected everything
// would trivially satisfy "the negatives fail", and one that accepted
// everything would trivially satisfy "the positives pass"; requiring both
// directions on a per-file basis is what makes this a real check.
func TestCorpusParses(t *testing.T) {
	t.Parallel()

	corpus, err := javayamsql.OpenCorpus()
	if err != nil {
		t.Fatalf("open corpus: %v", err)
	}
	files, err := corpus.List()
	if err != nil {
		t.Fatalf("list corpus: %v", err)
	}
	if len(files) != pinnedFileCount {
		t.Errorf("corpus has %d .yamsql files, pinned baseline is %d "+
			"(a re-sync changed the corpus; update pinnedFileCount and pinnedCensus together)",
			len(files), pinnedFileCount)
	}

	if n := len(javayamsql.ParseLevelNegatives()); n != pinnedParseLevelNegatives {
		t.Errorf("manifest lists %d parse-level negatives, pinned baseline is %d", n, pinnedParseLevelNegatives)
	}

	census := javayamsql.NewCensus()
	var gotInert []string
	refused := 0

	for _, path := range files {
		want := javayamsql.PolarityOf(path)
		f, err := corpus.ParseFile(path)

		if want == javayamsql.NegativeParse {
			if err != nil {
				refused++
			}
			if err == nil {
				t.Errorf("%s: parsed clean, but upstream rejects it while loading (%s). "+
					"The parser is missing a structural rule Java enforces.",
					path, javayamsql.Manifest[path].Reason)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s (%s): %v", path, want, err)
			continue
		}
		census.Add(f)
		for _, b := range f.Blocks {
			for _, d := range b.Inert {
				gotInert = append(gotInert, path+": "+d.String())
			}
		}
	}

	// Every file is either refused or censused, exactly once. This is what makes
	// the three pinned constants a closed system rather than three independent
	// numbers that could each be quietly wrong.
	if refused+census.Files != pinnedFileCount {
		t.Errorf("accounted for %d refused + %d parsed = %d files, corpus has %d",
			refused, census.Files, refused+census.Files, pinnedFileCount)
	}

	// The census counts entry points only. Included files are parsed again as
	// their own entry point, so folding them in here would double-count.
	if got := census.Line(); got != pinnedCensus {
		t.Errorf("corpus census drifted.\n got: %s\nwant: %s", got, pinnedCensus)
	}
	t.Logf("CENSUS %s", census.Line())

	var wantInert []string
	for _, d := range javayamsql.KnownInertDirectives {
		wantInert = append(wantInert, d.Path+": "+javayamsql.InertDirective{
			Where: d.Where, Key: d.Key, Line: d.Line,
		}.String())
	}
	sort.Strings(gotInert)
	sort.Strings(wantInert)
	if strings.Join(gotInert, "\n") != strings.Join(wantInert, "\n") {
		t.Errorf("set of directives Java silently ignores changed.\n got:\n%s\nwant:\n%s",
			strings.Join(gotInert, "\n"), strings.Join(wantInert, "\n"))
	}
}

// TestParseLevelNegativesAreRefusedForTheRightReason pins not just THAT each
// parse-level negative is rejected, but the rule that rejects it.
//
// Without this, the previous test is satisfiable by a parser that rejects those
// 25 files for any reason at all — a stray syntax quirk, a mis-typed key — while
// the rule upstream actually relies on goes unimplemented. Each substring below
// names the specific mechanism.
func TestParseLevelNegativesAreRefusedForTheRightReason(t *testing.T) {
	t.Parallel()

	wantReason := map[string]string{
		"check-result-metadata/shouldFail/result-metadata-without-result.yamsql": "resultMetadata requires at least one of",

		"include-block/includes/include-has-options.yamsql":       "could not find",
		"include-block/includes/include-recursion.yamsql":         "cyclic path detected",
		"include-block/shouldFail/cycle-in-include.yamsql":        "cyclic path detected",
		"include-block/shouldFail/include-non-existent.yamsql":    "could not find",
		"include-block/shouldFail/options-in-included.yamsql":     "file-wide options must be the first block",
		"supported-version/late-file-options.yamsql":              "file-wide options must be the first block",
		"supported-version/late-query-supported-version.yamsql":   "supported_version must be the first config",
		"supported-version/illegal-version-at-file.yamsql":        "invalid version",
		"supported-version/illegal-version-at-block.yamsql":       "invalid version",
		"supported-version/illegal-version-at-query.yamsql":       "invalid version",
		"transaction-setup/shouldFail/future-reference.yamsql":    "is not defined",
		"transaction-setup/shouldFail/setup-after-results.yamsql": "only result configurations can follow",

		"transaction-setup/shouldFail/setup-reference-after-results.yamsql":   "only result configurations can follow",
		"transaction-setup/shouldFail/setup-before-query.yamsql":              "could not find command",
		"transaction-setup/shouldFail/setup-reference-before-query.yamsql":    "could not find command",
		"initial-version/do-not-allow-max-rows-in-at-least.yamsql":            "only result configurations can follow",
		"initial-version/do-not-allow-max-rows-in-less-than.yamsql":           "only result configurations can follow",
		"initial-version/explain-after-version.yamsql":                        "only result configurations can follow",
		"initial-version/non-exhaustive-versions.yamsql":                      "does not cover complete set of versions",
		"initial-version/non-exhaustive-current-version.yamsql":               "does not cover complete set of versions",
		"initial-version/malformed-version-schema-template-variants.yamsql":   "invalid version",
		"initial-version/missing-version-tag-schema-template-variants.yamsql": "overlapping version ranges",
		"initial-version/overlapping-schema-template-variants.yamsql":         "overlapping version ranges",
		"initial-version/non-exhaustive-schema-template-variants.yamsql":      "do not cover the complete set of versions",
	}

	corpus, err := javayamsql.OpenCorpus()
	if err != nil {
		t.Fatalf("open corpus: %v", err)
	}

	negatives := javayamsql.ParseLevelNegatives()
	if len(negatives) != len(wantReason) {
		t.Fatalf("manifest lists %d parse-level negatives, this test pins %d reasons",
			len(negatives), len(wantReason))
	}

	for _, path := range negatives {
		want, ok := wantReason[path]
		if !ok {
			t.Errorf("%s: manifest calls this a parse-level negative but no reason is pinned", path)
			continue
		}
		_, err := corpus.ParseFile(path)
		if err == nil {
			t.Errorf("%s: expected a parse error mentioning %q", path, want)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: rejected for the wrong reason\n got: %v\nwant substring: %q", path, err, want)
		}
	}
}

// TestManifestPathsExist keeps the manifest honest: an entry naming a file that
// is no longer in the corpus is a silently dead assertion, and after a re-sync
// that is exactly how a negative test disappears without anyone noticing.
func TestManifestPathsExist(t *testing.T) {
	t.Parallel()

	corpus, err := javayamsql.OpenCorpus()
	if err != nil {
		t.Fatalf("open corpus: %v", err)
	}
	files, err := corpus.List()
	if err != nil {
		t.Fatalf("list corpus: %v", err)
	}
	present := make(map[string]bool, len(files))
	for _, f := range files {
		present[f] = true
	}
	for path := range javayamsql.Manifest {
		if !present[path] {
			t.Errorf("manifest names %q, which is not in the corpus", path)
		}
	}
	for _, d := range javayamsql.KnownInertDirectives {
		if !present[d.Path] {
			t.Errorf("KnownInertDirectives names %q, which is not in the corpus", d.Path)
		}
	}
}
