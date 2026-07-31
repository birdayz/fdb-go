package javayamsql

// This file records what the corpus *means* — which files are meant to succeed,
// which are meta-tests asserting a failure, and where that failure is meant to
// land. None of that is expressible in the `.yamsql` data itself: upstream keeps
// it in the Java test classes, where a `shouldFail()` stream is wrapped in
// assertThrows. Vendoring the data without the polarity would hand a future
// reader 33 files that look like bugs.
//
// The manifest lives here, in Go, and never in the vendored tree — those files
// are byte-identical to upstream and stay that way.

// Polarity is what upstream expects a corpus file to do when it is run.
type Polarity int

const (
	// Positive: the file is expected to run to completion.
	Positive Polarity = iota
	// NegativeParse: the file is expected to fail, and to fail while the YAML
	// is being loaded. The Go parser must reject these too.
	NegativeParse
	// NegativeExecution: the file is expected to fail, but only once a query
	// runs and an assertion is checked. The Go parser must ACCEPT these — the
	// document structure is entirely valid.
	NegativeExecution
	// Fragment: the file is never run on its own, only pulled in by an
	// `include:`. It is not self-sufficient and asserts nothing standalone.
	Fragment
)

func (p Polarity) String() string {
	switch p {
	case Positive:
		return "positive"
	case NegativeParse:
		return "negative(parse)"
	case NegativeExecution:
		return "negative(execution)"
	case Fragment:
		return "fragment"
	default:
		return "unknown"
	}
}

// ManifestEntry records one file's polarity and why.
type ManifestEntry struct {
	Path     string
	Polarity Polarity
	// Reason is the mechanism, not a restatement of the file name.
	Reason string
	// Source is the Java test class that asserts this polarity.
	Source string
}

// Manifest maps corpus-root-relative paths to their polarity. Files absent from
// it are [Positive] — the overwhelming majority, and listing 200-odd of them
// would bury the 60 that carry information.
//
// Every entry below was read off the Java test class named in Source, not
// inferred from the directory name. That distinction matters: the
// `include-block/includes/` directory looks like a bag of fragments and is not
// one — IncludeBlockTest runs all ten standalone, expecting eight to fail.
var Manifest = buildManifest()

func buildManifest() map[string]ManifestEntry {
	m := map[string]ManifestEntry{}
	add := func(entries []ManifestEntry) {
		for _, e := range entries {
			m[e.Path] = e
		}
	}
	add(checkExplainEntries)
	add(checkResultMetadataEntries)
	add(includeBlockEntries)
	add(transactionSetupEntries)
	add(supportedVersionEntries)
	add(initialVersionEntries)
	add(fragmentEntries)
	return m
}

// PolarityOf returns the recorded polarity, defaulting to [Positive].
func PolarityOf(path string) Polarity {
	if e, ok := Manifest[path]; ok {
		return e.Polarity
	}
	return Positive
}

// ParseLevelNegatives returns the files the parser is required to reject.
func ParseLevelNegatives() []string {
	var out []string
	for p, e := range Manifest {
		if e.Polarity == NegativeParse {
			out = append(out, p)
		}
	}
	return out
}

// CheckExplainTest.java: shouldFail at :86-88, shouldPass at :105-107.
var checkExplainEntries = []ManifestEntry{
	{
		"check-explain/shouldFail/wrong-plan.yamsql", NegativeExecution,
		"explain string is deliberately not the produced plan; fails in CheckExplainConfig.checkExplain", "CheckExplainTest",
	},
	{
		"check-explain/shouldFail/wrong-contains.yamsql", NegativeExecution,
		"explainContains substring is absent from the produced plan", "CheckExplainTest",
	},
}

// CheckResultMetadataTest.java: shouldFail at :98-101, shouldPass at :126-128.
// Fourteen of the fifteen negatives compare a descriptor against a real result
// set and so cannot fail before execution; only the config-shape one is caught
// while parsing.
var checkResultMetadataEntries = []ManifestEntry{
	{
		"check-result-metadata/shouldFail/result-metadata-without-result.yamsql", NegativeParse,
		"resultMetadata with no result/unorderedResult/error/count; rejected by QueryConfig.validateConfigs", "CheckResultMetadataTest",
	},

	{"check-result-metadata/shouldFail/wrong-column-name.yamsql", NegativeExecution, "descriptor mismatch", "CheckResultMetadataTest"},
	{"check-result-metadata/shouldFail/wrong-column-type.yamsql", NegativeExecution, "descriptor mismatch", "CheckResultMetadataTest"},
	{"check-result-metadata/shouldFail/missing-column.yamsql", NegativeExecution, "descriptor mismatch", "CheckResultMetadataTest"},
	{"check-result-metadata/shouldFail/extra-column.yamsql", NegativeExecution, "descriptor mismatch", "CheckResultMetadataTest"},
	{"check-result-metadata/shouldFail/wrong-column-order.yamsql", NegativeExecution, "descriptor mismatch", "CheckResultMetadataTest"},
	{"check-result-metadata/shouldFail/wrong-nested-struct-name.yamsql", NegativeExecution, "descriptor mismatch", "CheckResultMetadataTest"},
	{"check-result-metadata/shouldFail/wrong-nested-struct-type.yamsql", NegativeExecution, "descriptor mismatch", "CheckResultMetadataTest"},
	{
		"check-result-metadata/shouldFail/wrong-metadata-on-continuation-page.yamsql", NegativeExecution,
		"per-page descriptor check fails on a later continuation page", "CheckResultMetadataTest",
	},
	{"check-result-metadata/shouldFail/wrong-array-element-type.yamsql", NegativeExecution, "descriptor mismatch", "CheckResultMetadataTest"},
	{"check-result-metadata/shouldFail/wrong-struct-field-name.yamsql", NegativeExecution, "descriptor mismatch", "CheckResultMetadataTest"},
	{"check-result-metadata/shouldFail/wrong-struct-field-type.yamsql", NegativeExecution, "descriptor mismatch", "CheckResultMetadataTest"},
	{"check-result-metadata/shouldFail/wrong-array-of-struct-field-name.yamsql", NegativeExecution, "descriptor mismatch", "CheckResultMetadataTest"},
	{"check-result-metadata/shouldFail/wrong-array-of-struct-field-type.yamsql", NegativeExecution, "descriptor mismatch", "CheckResultMetadataTest"},
	{"check-result-metadata/shouldFail/wrong-struct-type-name.yamsql", NegativeExecution, "descriptor mismatch", "CheckResultMetadataTest"},
}

// IncludeBlockTest.java. Four streams, and the one that matters most:
// `include-block/includes/*` are run STANDALONE at :92-95 and :99-101, not
// treated as fragments. Eight fail — mostly because, on their own, they have no
// database to connect to — and two are self-sufficient enough to pass.
var includeBlockEntries = []ManifestEntry{
	{
		"include-block/includes/include-has-options.yamsql", NegativeParse,
		"its last document includes include-2.yamsql, which does not exist", "IncludeBlockTest",
	},
	{
		"include-block/includes/include-recursion.yamsql", NegativeParse,
		"includes itself; caught by YamlResource.assertNotCyclic", "IncludeBlockTest",
	},

	{
		"include-block/includes/include-connects-to-global-connection.yamsql", NegativeExecution,
		"standalone: `connect: (global) 1` has no database to resolve against", "IncludeBlockTest",
	},
	{"include-block/includes/include-no-explain.yamsql", NegativeExecution, "standalone: no database available", "IncludeBlockTest"},
	{"include-block/includes/include-with-explain.yamsql", NegativeExecution, "standalone: no database available", "IncludeBlockTest"},
	{"include-block/includes/include-setup-insert.yamsql", NegativeExecution, "standalone: setup-only, no database available", "IncludeBlockTest"},
	{"include-block/includes/include-with-a-test-block.yamsql", NegativeExecution, "standalone: no database available", "IncludeBlockTest"},
	{"include-block/includes/include-with-include.yamsql", NegativeExecution, "standalone: nested include resolves, but no database available", "IncludeBlockTest"},

	{"include-block/includes/include-scoped.yamsql", Positive, "carries its own schema_template", "IncludeBlockTest"},
	{"include-block/includes/include-txn-setup.yamsql", Positive, "only a transaction_setups block, so nothing executes", "IncludeBlockTest"},

	{
		"include-block/shouldFail/cycle-in-include.yamsql", NegativeParse,
		"reaches include-recursion.yamsql, which includes itself", "IncludeBlockTest",
	},
	{
		"include-block/shouldFail/include-non-existent.yamsql", NegativeParse,
		"include target is not in the resources bundle", "IncludeBlockTest",
	},
	{
		"include-block/shouldFail/options-in-included.yamsql", NegativeParse,
		"an included file carries file-wide options; Block.parse requires isTopLevel", "IncludeBlockTest",
	},

	{
		"include-block/shouldFail/include-scope-not-visible-outside.yamsql", NegativeExecution,
		"`connect: (global) 2` names a connection scoped to the included file", "IncludeBlockTest",
	},
	{
		"include-block/shouldFail/simple-include-with-wrong-metric.yamsql", NegativeExecution,
		"planner-metrics mismatch; needs the .metrics file, which is not vendored", "IncludeBlockTest",
	},
	{
		"include-block/shouldFail/verify-all-includes-execute.yamsql", NegativeExecution,
		"the second include re-inserts an existing primary key", "IncludeBlockTest",
	},
}

// TransactionSetupTest.java: shouldFail at :81-84, shouldPass at :105-107.
// The split here is unusually clean — ordering mistakes inside a query's config
// list are structural and caught while parsing, whereas ordering mistakes
// BETWEEN setups only bite when the SQL runs.
var transactionSetupEntries = []ManifestEntry{
	{
		"transaction-setup/shouldFail/future-reference.yamsql", NegativeParse,
		"setupReference names a transaction_setups block declared later; resolution is eager", "TransactionSetupTest",
	},
	{
		"transaction-setup/shouldFail/setup-after-results.yamsql", NegativeParse,
		"a setup config follows a result config", "TransactionSetupTest",
	},
	{
		"transaction-setup/shouldFail/setup-reference-after-results.yamsql", NegativeParse,
		"a setupReference config follows a result config", "TransactionSetupTest",
	},
	{
		"transaction-setup/shouldFail/setup-before-query.yamsql", NegativeParse,
		"the test's first list item is `setup`, so `query` is then read as a config key", "TransactionSetupTest",
	},
	{
		"transaction-setup/shouldFail/setup-reference-before-query.yamsql", NegativeParse,
		"the test's first list item is `setupReference`, so `query` is then read as a config key", "TransactionSetupTest",
	},

	{
		"transaction-setup/shouldFail/double-setup-reference-wrong-order.yamsql", NegativeExecution,
		"t2 is created referencing t1 before t1 exists", "TransactionSetupTest",
	},
	{
		"transaction-setup/shouldFail/double-setup-wrong-order.yamsql", NegativeExecution,
		"same ordering fault, inline setup form", "TransactionSetupTest",
	},
	{
		"transaction-setup/shouldFail/duplicate-setup-reference-name.yamsql", NegativeExecution,
		"duplicate keys are allowed and last wins, so the query selects a table never created", "TransactionSetupTest",
	},
	{
		"transaction-setup/shouldFail/unsupported-setup.yamsql", NegativeExecution,
		"only CREATE TEMPORARY FUNCTION is permitted; asserted in QueryCommand.executeInternal", "TransactionSetupTest",
	},
	{
		"transaction-setup/shouldFail/unsupported-setup-reference.yamsql", NegativeExecution,
		"same restriction, via a setupReference", "TransactionSetupTest",
	},
}

// SupportedVersionTest.java: shouldFail at :98-103, shouldPass at :120-125,
// both against a pinned SemanticVersion of 3.0.18.0.
//
// The seven execution-level entries are not version-handling bugs: each file
// deliberately contains a broken query (`query: SHOULD ERROR WHEN RUN;`) that
// the version gate is supposed to skip. They fail because the query runs.
var supportedVersionEntries = []ManifestEntry{
	{
		"supported-version/late-file-options.yamsql", NegativeParse,
		"the options block is not the first document", "SupportedVersionTest",
	},
	{
		"supported-version/late-query-supported-version.yamsql", NegativeParse,
		"supported_version is not the query's first config", "SupportedVersionTest",
	},
	{"supported-version/illegal-version-at-file.yamsql", NegativeParse, "unparseable version string", "SupportedVersionTest"},
	{"supported-version/illegal-version-at-block.yamsql", NegativeParse, "unparseable version string", "SupportedVersionTest"},
	{"supported-version/illegal-version-at-query.yamsql", NegativeParse, "unparseable version string", "SupportedVersionTest"},

	{"supported-version/supported-at-file.yamsql", NegativeExecution, "the gate admits the run and the deliberately broken query executes", "SupportedVersionTest"},
	{"supported-version/supported-at-block.yamsql", NegativeExecution, "as above, gated at the block", "SupportedVersionTest"},
	{"supported-version/supported-at-query.yamsql", NegativeExecution, "as above, gated at the query", "SupportedVersionTest"},
	{"supported-version/unsupported-at-block-only.yamsql", NegativeExecution, "only the block is gated, so the ungated query runs", "SupportedVersionTest"},
	{"supported-version/unspecified.yamsql", NegativeExecution, "no gate at all, so the broken query runs", "SupportedVersionTest"},
	{"supported-version/lower-at-block.yamsql", NegativeExecution, "the gate is below the version under test", "SupportedVersionTest"},
	{"supported-version/lower-at-query.yamsql", NegativeExecution, "the gate is below the version under test", "SupportedVersionTest"},
}

// InitialVersionTest.java: shouldFail at :88-117 against a pinned 3.0.18.0.
//
// Polarity here is version-dependent for the `wrong-*-less-than` files, which
// appear in shouldFail (fixed version) and shouldPassOnCurrent alike. They are
// recorded as execution-level negatives because that is what they are under the
// fixed-version stream; either way the parser must accept them.
var initialVersionEntries = []ManifestEntry{
	{
		"initial-version/do-not-allow-max-rows-in-at-least.yamsql", NegativeParse,
		"maxRows follows a version-dependent config", "InitialVersionTest",
	},
	{
		"initial-version/do-not-allow-max-rows-in-less-than.yamsql", NegativeParse,
		"maxRows follows a version-dependent config", "InitialVersionTest",
	},
	{
		"initial-version/explain-after-version.yamsql", NegativeParse,
		"explain follows a version-dependent config, which the ordering rule forbids", "InitialVersionTest",
	},
	{
		"initial-version/non-exhaustive-versions.yamsql", NegativeParse,
		"the initialVersion* ranges leave a gap", "InitialVersionTest",
	},
	{
		"initial-version/non-exhaustive-current-version.yamsql", NegativeParse,
		"the ranges leave a gap below !current_version", "InitialVersionTest",
	},
	{
		"initial-version/malformed-version-schema-template-variants.yamsql", NegativeParse,
		"a variant's version string does not parse", "InitialVersionTest",
	},
	{
		"initial-version/missing-version-tag-schema-template-variants.yamsql", NegativeParse,
		"an untagged variant spans all versions and so overlaps its siblings", "InitialVersionTest",
	},
	{
		"initial-version/overlapping-schema-template-variants.yamsql", NegativeParse,
		"two variants claim the same version range", "InitialVersionTest",
	},
	{
		"initial-version/non-exhaustive-schema-template-variants.yamsql", NegativeParse,
		"the variant ranges leave a gap", "InitialVersionTest",
	},

	{
		"initial-version/mid-query.yamsql", NegativeExecution,
		"maxRows leaves a continuation open, so a version check is reached mid-query; the config list itself is well-formed and covers all versions", "InitialVersionTest",
	},
	{"initial-version/wrong-result-at-least.yamsql", NegativeExecution, "expected rows are wrong for the version under test", "InitialVersionTest"},
	{"initial-version/wrong-result-less-than.yamsql", NegativeExecution, "expected rows are wrong for the version under test", "InitialVersionTest"},
	{"initial-version/wrong-count-at-least.yamsql", NegativeExecution, "expected count is wrong for the version under test", "InitialVersionTest"},
	{"initial-version/wrong-count-less-than.yamsql", NegativeExecution, "expected count is wrong for the version under test", "InitialVersionTest"},
	{"initial-version/wrong-unordered-at-least.yamsql", NegativeExecution, "expected rows are wrong for the version under test", "InitialVersionTest"},
	{"initial-version/wrong-unordered-less-than.yamsql", NegativeExecution, "expected rows are wrong for the version under test", "InitialVersionTest"},
	{"initial-version/wrong-error-at-least.yamsql", NegativeExecution, "expected error code is wrong for the version under test", "InitialVersionTest"},
	{"initial-version/wrong-error-less-than.yamsql", NegativeExecution, "expected error code is wrong for the version under test", "InitialVersionTest"},
}

// fragmentEntries are the only files no Java test class runs directly. They are
// pulled in solely by copy-basic.yamsql, which YamlIntegrationTests runs.
var fragmentEntries = []ManifestEntry{
	{
		"copy-basic-includes/copy-clear-dest.yamsql", Fragment,
		"included by copy-basic.yamsql at four call sites; clears the destination", "YamlIntegrationTests (via copy-basic.yamsql)",
	},
	{
		"copy-basic-includes/copy-verify-dest.yamsql", Fragment,
		"included by copy-basic.yamsql at two call sites; verifies the destination", "YamlIntegrationTests (via copy-basic.yamsql)",
	},
}

// KnownInertDirectives are the corpus's dead keys: written by an author who
// plainly meant them to do something, and read by nothing.
//
// These are upstream defects, not parser gaps. `supported_version` belongs under
// a test_block's `options:` map; written directly under `test_block:` it is
// never looked at, so all three files below run unconditionally against every
// version instead of being gated. `noChecks` is a query-config name that has no
// meaning in a file preamble.
//
// They are pinned so the set cannot grow silently. A new entry appearing here
// means someone added another directive that does nothing.
var KnownInertDirectives = []struct {
	Path  string
	Where string
	Key   string
	Line  int
}{
	{"between.yamsql", "test_block", "supported_version", 97},
	{"index-ddl-aggregates-only.yamsql", "options", "noChecks", 27},
	{"uuid-non-prepared.yamsql", "test_block", "supported_version", 62},
	{"uuid-prepared.yamsql", "test_block", "supported_version", 64},
}
