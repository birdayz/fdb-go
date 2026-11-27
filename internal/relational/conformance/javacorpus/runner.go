// Portions derived from FoundationDB Record Layer (YamlExecutionContext.java,
// PreambleBlock.java, Command.java, EmbeddedConfig.java),
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2024 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package javacorpus

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"fdb.dev/internal/relational/conformance/javayamsql"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/sqldriver"
)

// Config parameterises one corpus run.
type Config struct {
	// ClusterFile is the FDB cluster file every connection is opened against.
	ClusterFile string
	// Database optionally injects an already-constructed *recordlayer.FDBDatabase
	// (e.g. a SimFDB backend) instead of opening ClusterFile.
	Database *recordlayer.FDBDatabase
	// IDPrefix disambiguates the generated database/template/schema names so
	// concurrent runs of the same file cannot collide. It must be a legal SQL
	// identifier fragment.
	IDPrefix string
	// FactoryResetLoad enables the validated, private generated-fixture setup
	// contract in RunParsed. Vendored Java corpus runs never use this mode.
	FactoryResetLoad bool
	// WorkingDir stands for the directory Java's yaml-tests run in (the
	// module directory): a relative ENCRYPTION_KEY_STORE resolves against it.
	// The caller puts there the files the corpus names by such a path.
	WorkingDir string
}

// catalogPath is the system database every DDL statement is issued against.
// Java hard-codes connection index 0 to `jdbc:embed:/__SYS?schema=CATALOG` and
// runs the whole schema_template lifecycle there.
const catalogPath = "/__SYS"

// catalogTarget is Java's connection 0, `jdbc:embed:/__SYS?schema=CATALOG`:
// SQL over it reads the catalog's tables (SCHEMAS, DATABASES, TEMPLATES).
var catalogTarget = connTarget{Path: catalogPath, Schema: "CATALOG"}

// connTarget is a resolved `connect:` destination.
type connTarget struct {
	// Path is the database path, e.g. "/YAML_X_DB" or catalogPath.
	Path string
	// Schema is the default schema; CATALOG for the catalog connection, as
	// Java's `jdbc:embed:/__SYS?schema=CATALOG`.
	Schema string
}

// runner holds the state Java keeps on its YamlExecutionContext for one file.
type runner struct {
	cfg    Config
	result *FileResult

	// connsByResource is Java's per-resource connectionURIs list: a
	// schema_template block registers its database here, and `connect: n`
	// indexes into it 1-based. It is per-resource because an included file's
	// registrations are scoped to that file unless reached via `(global)`.
	connsByResource map[string][]connTarget
	// resourceOrder is the include call order, oldest ancestor first. The
	// global list is the concatenation of these resources' local lists.
	resourceOrder []string

	engine   *sqldriver.Engine
	dbs      map[connTarget]*sql.DB
	cleanups []func()
	// counter makes the generated identifiers unique within one file.
	counter int
	fixture *privateFixture
	// fileConnOptions is the preamble's connection_options, which every
	// connection gets (connoptions.go).
	fileConnOptions []javayamsql.Entry
}

// Run executes one corpus file and returns its ledger entry.
//
// It never returns an error for a corpus-level outcome — a failing file is a
// StatusFail result, a non-executable one is a StatusSkip with a class. Only a
// caller mistake (no cluster file) is reported out of band.
func Run(ctx context.Context, corpus *javayamsql.Corpus, path string, cfg Config) FileResult {
	res := FileResult{Path: path}
	if cfg.FactoryResetLoad {
		res.Status, res.Err = StatusFail, fmt.Errorf("factory reset/load is only valid for RunParsed")
		return res
	}

	switch javayamsql.PolarityOf(path) {
	case javayamsql.NegativeParse:
		// The parse gate already pins these; counting them here closes the
		// file total without re-asserting someone else's check.
		res.Status = StatusSkip
		res.SkipClass = SkipPolarityNegativeParse
		return res
	case javayamsql.Fragment:
		res.Status = StatusSkip
		res.SkipClass = SkipFragment
		return res
	}

	file, err := corpus.ParseFile(path)
	if err != nil {
		res.Status = StatusFail
		res.Err = fmt.Errorf("parse: %w", err)
		return res
	}

	r := &runner{
		cfg:             cfg,
		result:          &res,
		connsByResource: map[string][]connTarget{},
		dbs:             map[connTarget]*sql.DB{},
	}
	defer r.teardown()

	runErr := r.execute(ctx, file)

	// A block that cannot run at all takes the file with it, as a counted
	// skip. Java does the same through Assumptions.assumeTrue, which aborts
	// rather than fails.
	var skipErr *skipFileError
	if errors.As(runErr, &skipErr) {
		res.Status = StatusSkip
		res.SkipClass = skipErr.class
		res.Skips = append(res.Skips, Skip{Class: skipErr.class, Where: path, Detail: skipErr.detail})
		return res
	}
	// A failure this run has already measured, booked, and pinned to its exact
	// rejection (a DDL one included). Any OTHER failure in the same file stays a hard failure.
	if gap, ok := gapFor(path, runErr); ok {
		res.Status = StatusSkip
		res.SkipClass = gap.Class
		res.Skips = append(res.Skips, Skip{Class: gap.Class, Where: path, GapBooking: gap.Booking, Detail: gap.Booking + ": " + runErr.Error()})
		return res
	}
	// A schema_template the engine will not create is an engine gap with a
	// name, not a corpus regression — that is what the DDL skip classes are.
	var ddl *ddlError
	if errors.As(runErr, &ddl) {
		class := classifyDDLGap(file, ddl)
		res.Status = StatusSkip
		res.SkipClass = class
		res.Skips = append(res.Skips, Skip{Class: class, Where: path, Detail: ddl.Error()})
		return res
	}

	// A fixed-version meta-test has no Java stream on the current version, so
	// upstream states no expectation for it. That is a reason to MEASURE it,
	// not to wave it through: Go can run what Java declines to, and the one
	// outcome that would be a real finding is a file asserting something and
	// getting it right — that would mean the gate under test resolves the same
	// way on current as on the pinned version, and the file was never
	// version-specific at all. Anything else (it fails, or it reaches no
	// assertion) is the expected "cannot be evaluated here".
	if javayamsql.PolarityOf(path) == javayamsql.FixedVersionMeta {
		res.Status = StatusSkip
		res.SkipClass = SkipFixedVersionMeta
		if runErr == nil && res.QueriesRun > 0 {
			res.Status = StatusFail
			res.Err = fmt.Errorf("file is classified fixed-version-meta (%s) — Java runs it only against a pinned "+
				"version — but on the current version it asserted %d queries and passed. It is not version-specific; "+
				"reclassify it as Positive",
				javayamsql.Manifest[path].Reason, res.QueriesRun)
		}
		return res
	}

	// Execution-level negatives are the corpus's own meta-tests: upstream
	// asserts they FAIL. Their pin is therefore inverted — a clean run is the
	// bug, not the success.
	if javayamsql.PolarityOf(path) == javayamsql.NegativeExecution {
		// …but ANY error is not the error. A negative that dies in its `setup:`
		// block never reached the directive upstream expects to trip, so
		// crediting it would book an engine gap as a corpus result — the same
		// mistake in the opposite direction from crediting a suppressed
		// assertion. The failure has to come from the test block for the
		// inverted pin to mean anything.
		_, expectedInSetup := SetupNegatives[path]
		var setupErr *setupError
		if errors.As(runErr, &setupErr) && !expectedInSetup {
			runErr = fmt.Errorf("file is classified negative(execution) — upstream asserts its TEST BLOCK "+
				"fails (%s) — but it died in setup before any assertion ran, so the expected failure was "+
				"never reached: %w", javayamsql.Manifest[path].Reason, runErr)
			if gap, ok := gapFor(path, runErr); ok {
				res.Status = StatusSkip
				res.SkipClass = gap.Class
				res.Skips = append(res.Skips, Skip{Class: gap.Class, Where: path, GapBooking: gap.Booking, Detail: gap.Booking + ": " + runErr.Error()})
				return res
			}
			res.Status = StatusFail
			res.Err = runErr
			return res
		}
		if runErr != nil {
			res.Status = StatusSkip
			res.SkipClass = SkipPolarityNegativeExecution
			// The failure is recorded, not just the class: a negative credited
			// for failing the WRONG way still reads as green in the census, and
			// this line is where that is visible.
			res.Skips = append(res.Skips, Skip{
				Class: SkipPolarityNegativeExecution, Where: path, Detail: runErr.Error(),
			})
			return res
		}
		// A negative that ran clean is only a finding if the assertion
		// upstream expects to trip was actually reached. When the directive
		// carrying it was skipped for a capability reason, the file was never
		// evaluated at all, and calling that a passed negative would credit a
		// gap as a result.
		if class, ok := suppressedBy(res.Skips); ok {
			res.Status = StatusSkip
			res.SkipClass = class
			return res
		}
		res.Status = StatusFail
		res.Err = fmt.Errorf("file is classified negative(execution) — upstream asserts it fails (%s) — but it ran clean",
			javayamsql.Manifest[path].Reason)
		return res
	}

	if runErr != nil {
		res.Status = StatusFail
		res.Err = runErr
		return res
	}
	// A positive file that asserted nothing is not a pass. Reporting it as one
	// is how a skipped dimension reads as covered.
	if res.QueriesRun == 0 {
		res.Status = StatusSkip
		if class, ok := suppressedBy(res.Skips); ok {
			res.SkipClass = class
		} else {
			res.SkipClass = SkipVacuous
		}
		return res
	}
	res.Status = StatusPass
	return res
}

// SuppressesAssertion reports whether a skip class removed a result assertion
// rather than merely declining an extra dimension of the same one.
//
// The distinction decides whether a file that ran clean actually proved
// anything: skipping the prepared arm still leaves the simple arm asserting the
// rows, whereas skipping the only `result:` leaves nothing behind it. It is
// exported so the negative-execution accounting can be COUNTED against it
// rather than restated in prose.
func (c SkipClass) SuppressesAssertion() bool {
	switch c {
	// SkipResultMetadataNested belongs here even though the row assertion
	// survives it: the `check-result-metadata/shouldFail/wrong-nested-*` files
	// are negatives whose ONLY failing assertion IS the metadata one, so
	// declining it would let them run clean and be reported as negatives that
	// wrongly passed — crediting a driver gap as a finding.
	case SkipPlanAssertion, SkipResultMetadataNested, SkipContinuation,
		SkipRandomInjection, SkipVersionGate,
		SkipNoChecks:
		return true
	default:
		return false
	}
}

// suppressedBy returns the first skip that removed an assertion.
func suppressedBy(skips []Skip) (SkipClass, bool) {
	for _, s := range skips {
		if s.Class.SuppressesAssertion() {
			return s.Class, true
		}
	}
	return "", false
}

func (r *runner) skip(class SkipClass, where, detail string) {
	r.result.Skips = append(r.result.Skips, Skip{Class: class, Where: where, Detail: detail})
}

func (r *runner) teardown() {
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		r.cleanups[i]()
	}
	for _, db := range r.dbs {
		_ = db.Close()
	}
	if r.engine != nil {
		_ = r.engine.Close()
	}
}
