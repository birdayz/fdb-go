// Package javacorpus executes the vendored Java `.yamsql` acceptance corpus
// (parsed by the sibling javayamsql package) against the Go SQL engine.
//
// The skip ledger is the product, not a side effect. RFC-201 §8 makes every
// non-executed file, block, query and config a COUNTED entry with a reason
// class, because a suite that silently declines to run something reads exactly
// like a suite that ran it and passed. The ledger is what turns "the corpus is
// green" into a claim with a denominator: N executed, M skipped, and for each
// skip the specific capability whose absence caused it. It is therefore also
// the public statement of what the engine does not yet support — a skip class
// is a specification gap with a name.
package javacorpus

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// SkipClass names why something was not executed.
//
// The vocabulary extends the yamsql coverage classifier's: `unsupported-DDL:*`
// for a schema the engine cannot create, `unsupported:*` for a runner or driver
// capability a later phase adds, `plan-assertion` for the directives RFC-201
// ruling 2 permanently declines, and `polarity:*` for the corpus's own negative
// meta-tests.
type SkipClass string

// The reason classes. Each one must be reachable — the census test asserts the
// exact per-class counts, so a class that goes to zero is a visible event.
const (
	// SkipPlanAssertion covers explain / explainContains / planHash. RFC-201
	// ruling 2: Go never grows a Java-format plan renderer, and Java itself
	// degrades these to no-ops in multi-server mode because plan strings are
	// not version-stable. Go is, in effect, another server version.
	SkipPlanAssertion SkipClass = "plan-assertion"

	// SkipPolarityNegativeExecution is a file the manifest records as an
	// execution-level negative: upstream asserts it FAILS. These are not
	// skipped from execution — they are run and required to fail — but the
	// file's outcome is booked here rather than as a pass.
	SkipPolarityNegativeExecution SkipClass = "polarity:negative-execution"

	// SkipPolarityNegativeParse is a file the parser must refuse. Pinned by
	// javayamsql's own gate; counted here so the file total closes.
	SkipPolarityNegativeParse SkipClass = "polarity:negative-parse"

	// SkipFragment is an include-only file that asserts nothing standalone.
	SkipFragment SkipClass = "fragment"

	// SkipFixedVersionMeta is a file whose polarity Java defines only against a
	// version its test class pins, with no current-version stream running it.
	// A single-current-version runner cannot evaluate it either way.
	SkipFixedVersionMeta SkipClass = "polarity:fixed-version-meta"

	// SkipVacuous marks a file that executed without asserting anything,
	// because every one of its result-consuming configs was skipped for a
	// capability reason. Without this class such a file reports a PASS that
	// proves nothing — the precise shape of green that hides an untested
	// dimension.
	SkipVacuous SkipClass = "vacuous:all-assertions-skipped"

	// SkipVersionGate is a supported_version gate that excludes the version
	// under test.
	SkipVersionGate SkipClass = "version-gate"

	// SkipMultiCluster is `required_clusters` > 1: the corpus file needs two
	// FDB clusters wired as separate connections.
	SkipMultiCluster SkipClass = "unsupported:multi-cluster"

	// SkipPrepared is `statement_type: prepared` and parameter injection into
	// a prepared statement. RFC-201 Phase 5.
	SkipPrepared SkipClass = "unsupported:prepared"

	// SkipContinuation is `maxRows` with multi-page result consumption. The
	// driver has no per-page continuation surface to hand back. RFC-201
	// Phase 2, and under the hard wire-compat line.
	SkipContinuation SkipClass = "unsupported:continuation"

	// SkipResultMetadataNested is a `resultMetadata:` config whose expectation
	// descends into a column — a struct field list, an `{array: …}` map, or a
	// declared struct type name.
	//
	// Java reads these off RelationalResultSetMetaData, which exposes
	// getStructMetaData / getArrayMetaData / getTypeName recursively
	// (CheckResultMetadataConfig.extractDescriptors). Go's driver cannot: the
	// planned result type is flattened into `executor.ColumnDef`, whose
	// `TypeName` is ONE string, so a struct column arrives as "STRUCT" with no
	// fields, an array column arrives as its ELEMENT's type name rather than
	// Java's "ARRAY(elem)", and a declared struct type name is not carried at
	// all. The flat, scalar half of the directive IS asserted; only the
	// descending half is declined, and it is declined with a name so the driver
	// gap is sized rather than hidden inside a passing file.
	SkipResultMetadataNested SkipClass = "unsupported:result-metadata-nested"

	// SkipTemporaryFunction is a `setup:` / `setupReference:` query config,
	// which Java restricts to CREATE TEMPORARY FUNCTION. RFC-201 Phase 4.
	SkipTemporaryFunction SkipClass = "unsupported:temporary-function"

	// SkipCopyBlock is a `copy_block`, which moves data between two clusters.
	SkipCopyBlock SkipClass = "unsupported:copy-block"

	// SkipSchemaCommand is a `load schema template` / `set schema state`
	// command: metadata surgery with no Go equivalent.
	SkipSchemaCommand SkipClass = "unsupported:schema-command"

	// SkipDebugger is a `debugger:` config selecting a Java planner debugger.
	SkipDebugger SkipClass = "unsupported:debugger"

	// SkipDDLStruct is a schema template the engine rejects because it
	// declares a struct type. RFC-201 Phase 3, the largest engine gap.
	SkipDDLStruct SkipClass = "unsupported-DDL:struct"

	// SkipDDLFunction is a schema template declaring a SQL function.
	// RFC-201 Phase 4.
	SkipDDLFunction SkipClass = "unsupported-DDL:function"

	// SkipDDLValueIndexAsSelect is a value index written in materialized-view
	// syntax — `CREATE INDEX x AS SELECT cols FROM t [ORDER BY cols]` with no
	// aggregate. Go's DDL routes every AS-SELECT index to the aggregate
	// builder, which requires an aggregate function. Booked as CQ-71.
	SkipDDLValueIndexAsSelect SkipClass = "unsupported-DDL:value-index-as-select"

	// SkipDDLOther is a schema template the engine rejects for a reason that
	// is not struct or function — the residual DDL gap, kept separate so it
	// cannot hide inside the two named ones.
	SkipDDLOther SkipClass = "unsupported-DDL:other"

	// SkipCheckCache is the extra `check_cache` pass, whose assertion needs a
	// per-connection plan-cache metric collector.
	SkipCheckCache SkipClass = "unsupported:check-cache"

	// SkipRandomInjection is a `!r` / `!a` generator segment, whose value comes
	// from the test block's shared java.util.Random in a draw order interleaved
	// with the prepared/simple mix. RFC-201 Phase 5.
	SkipRandomInjection SkipClass = "unsupported:random-injection"

	// SkipNoChecks is a query carrying no configs at all: Java attaches a
	// synthetic noChecks config, so the query is executed for its side effects
	// and nothing is asserted about its output.
	SkipNoChecks SkipClass = "no-checks"
)

// The measured engine gaps. Each is a divergence a corpus file surfaced by
// being RUN, each carries its exact rejection in the gaps table, and each is
// booked. They are separate classes rather than one bucket because a gap
// without a name cannot be sized, prioritised or noticed when it closes.
const (
	// SkipGapNullableArrayWrapper is a query over a STORED nullable array
	// whose answer depends on distinguishing NULL from empty: Go writes a
	// plain repeated field for nullable arrays instead of Java's
	// `message{ repeated values }` wrapper (RFC-143 §3a), so a stored `[]`
	// is byte-identical to a stored NULL and reads back as NULL. The
	// array COMPARISON semantics themselves are closed (the former
	// engine-gap:array-comparison class): `[1] = [1]` is TRUE, the
	// NULL/NONE operand matrix matches the corpus, mismatched ARRAY types
	// reject 42804 — pinned by TestFDB_ArrayComparison (sqldriver) and
	// ArrayComparisonJavaProbe (conformance, live-Java measured).
	SkipGapNullableArrayWrapper SkipClass = "engine-gap:nullable-array-wrapper"
	// SkipGapCommaJoinFrom is a JOIN clause combined with comma-separated
	// FROM sources (`FROM a, a.refs AS r JOIN b ON …`).
	SkipGapCommaJoinFrom SkipClass = "engine-gap:comma-join-mixed-from"
	// SkipGapDMLReturning is a DML statement's RETURNING clause: the engine
	// executes the mutation but produces no result set (the same surface
	// SkipGapReturningDryRun covers for the DRY RUN variant).
	SkipGapDMLReturning SkipClass = "engine-gap:dml-returning-result-set"
	// SkipGapCatalogTables is a query against the catalog's own system tables.
	SkipGapCatalogTables SkipClass = "engine-gap:catalog-system-tables"
	// SkipGapRowVersion is the `__ROW_VERSION` pseudo-column.
	SkipGapRowVersion SkipClass = "engine-gap:row-version-pseudocolumn"
	// SkipGapTableValuedFunction is a table-valued function in FROM.
	SkipGapTableValuedFunction SkipClass = "engine-gap:table-valued-function"
	// SkipGapInlineValues is `FROM VALUES (…)` as a table source.
	SkipGapInlineValues SkipClass = "engine-gap:inline-values-table"
	// SkipGapCorrelatedExistsSetOp is a correlated EXISTS over a set operation.
	SkipGapCorrelatedExistsSetOp SkipClass = "engine-gap:correlated-exists-setop"
	// SkipGapNestedRecursiveWith is a WITH nested inside a recursive CTE body.
	SkipGapNestedRecursiveWith SkipClass = "engine-gap:nested-recursive-with"
	// SkipGapDerivedJoinOn is a JOIN-bodied derived table whose ON clause the
	// FROM resolver cannot bind back to its sources.
	SkipGapDerivedJoinOn SkipClass = "engine-gap:derived-table-join-on"
	// SkipGapReturningDryRun is UPDATE … RETURNING … OPTIONS(DRY RUN).
	SkipGapReturningDryRun SkipClass = "engine-gap:returning-dry-run"
	// SkipGapPlannerDeclines is a query Cascades declines to plan.
	SkipGapPlannerDeclines SkipClass = "engine-gap:planner-declines"
	// SkipGapErrorClass is an error that reaches the client without a SQLSTATE,
	// so the corpus's error-class assertion has nothing to compare against.
	SkipGapErrorClass SkipClass = "engine-gap:error-class"
	// SkipConformanceGoAccepts is a query Go executes that Java rejects — a
	// widening of the shared surface, tracked because the conformance
	// principle governs that surface in both directions.
	SkipConformanceGoAccepts SkipClass = "conformance:go-accepts-what-java-rejects"
)

// AllSkipClasses is every declared reason class.
//
// It exists so a class can be asserted REACHABLE. A skip vocabulary is only
// worth having if each name is actually produced by something; a label nothing
// emits reads as a covered case and is worse than no label at all.
func AllSkipClasses() []SkipClass {
	return []SkipClass{
		SkipPlanAssertion,
		SkipPolarityNegativeExecution,
		SkipPolarityNegativeParse,
		SkipFragment,
		SkipFixedVersionMeta,
		SkipVacuous,
		SkipVersionGate,
		SkipMultiCluster,
		SkipPrepared,
		SkipContinuation,
		SkipResultMetadataNested,
		SkipTemporaryFunction,
		SkipCopyBlock,
		SkipSchemaCommand,
		SkipDebugger,
		SkipDDLStruct,
		SkipDDLFunction,
		SkipDDLValueIndexAsSelect,
		SkipDDLOther,
		SkipGapNullableArrayWrapper,
		SkipGapCommaJoinFrom,
		SkipGapDMLReturning,
		SkipGapCatalogTables,
		SkipGapRowVersion,
		SkipGapTableValuedFunction,
		SkipGapInlineValues,
		SkipGapCorrelatedExistsSetOp,
		SkipGapNestedRecursiveWith,
		SkipGapDerivedJoinOn,
		SkipGapReturningDryRun,
		SkipGapPlannerDeclines,
		SkipGapErrorClass,
		SkipConformanceGoAccepts,
		SkipCheckCache,
		SkipRandomInjection,
		SkipNoChecks,
	}
}

// Status is a file-level outcome.
type Status uint8

// File outcomes.
const (
	// StatusPass: the file executed and every assertion held. For an
	// execution-level negative, it means the file failed as upstream requires.
	StatusPass Status = iota
	// StatusFail: an assertion did not hold, or an execution-level negative
	// passed when upstream requires it to fail.
	StatusFail
	// StatusSkip: the file was not executed at all.
	StatusSkip
)

func (s Status) String() string {
	switch s {
	case StatusPass:
		return "pass"
	case StatusFail:
		return "fail"
	case StatusSkip:
		return "skip"
	default:
		return fmt.Sprintf("Status(%d)", uint8(s))
	}
}

// Skip is one counted non-execution.
type Skip struct {
	Class SkipClass
	// Where locates it: the corpus path plus a block/query hint.
	Where string
	// Detail is the specific cause, e.g. the engine's DDL rejection message.
	Detail string
}

// FileResult is one corpus file's outcome.
type FileResult struct {
	Path   string
	Status Status
	// SkipClass is set when Status is StatusSkip.
	SkipClass SkipClass
	// Err is the failure that produced StatusFail, or — for an execution-level
	// negative that passed — nil with a Status of StatusFail.
	Err error
	// QueriesRun counts queries whose configs were actually asserted.
	QueriesRun int
	// Skips are the sub-file counted skips (block, query and config level).
	Skips []Skip
}

// Ledger accumulates the run. It is safe for concurrent use because corpus
// files run in parallel subtests.
type Ledger struct {
	mu    sync.Mutex
	files []FileResult
}

// Add records one file's outcome.
func (l *Ledger) Add(r FileResult) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.files = append(l.files, r)
}

// Files returns the recorded results, sorted by path.
func (l *Ledger) Files() []FileResult {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := append([]FileResult(nil), l.files...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Census is the pinned summary: file outcomes, file-level skip classes, and
// sub-file skip classes.
type Census struct {
	Pass int
	Fail int
	Skip int
	// FileSkips counts files not executed, by class.
	FileSkips map[SkipClass]int
	// InnerSkips counts block/query/config-level skips, by class.
	InnerSkips map[SkipClass]int
	// QueriesRun is the total number of asserted queries.
	QueriesRun int
}

// Census folds the ledger.
func (l *Ledger) Census() Census {
	c := Census{FileSkips: map[SkipClass]int{}, InnerSkips: map[SkipClass]int{}}
	for _, f := range l.Files() {
		switch f.Status {
		case StatusPass:
			c.Pass++
		case StatusFail:
			c.Fail++
		case StatusSkip:
			c.Skip++
			c.FileSkips[f.SkipClass]++
		}
		c.QueriesRun += f.QueriesRun
		for _, s := range f.Skips {
			c.InnerSkips[s.Class]++
		}
	}
	return c
}

// Line renders the census as one stable, greppable line — the same shape the
// parse gate's census uses, so the two read alike in CI output.
func (c Census) Line() string {
	var b strings.Builder
	fmt.Fprintf(&b, "pass=%d fail=%d skip=%d queries=%d file_skips{%s} inner_skips{%s}",
		c.Pass, c.Fail, c.Skip, c.QueriesRun, classCounts(c.FileSkips), classCounts(c.InnerSkips))
	return b.String()
}

func classCounts(m map[SkipClass]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[SkipClass(k)]))
	}
	return strings.Join(parts, ",")
}
