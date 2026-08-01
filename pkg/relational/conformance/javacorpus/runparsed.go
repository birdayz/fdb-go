package javacorpus

import (
	"context"
	"database/sql"
	"errors"

	"fdb.dev/pkg/relational/conformance/javayamsql"
)

// RunParsed executes one ALREADY-PARSED yamsql file through the same runner
// that executes the vendored Java corpus, without the vendored corpus's
// manifest bookkeeping.
//
// It exists for generated corpora (the RFC-201 factory corpus emits genuine
// yamsql), whose files differ from the vendored ones in exactly the ways the
// omitted bookkeeping covers:
//
//   - No polarity manifest: a generated file is always positive; upstream
//     never asserts one fails.
//   - No gap ledger: a gap entry books a MEASURED upstream divergence by
//     path, and generated paths are not in the measurement.
//   - A ddlError is a plain FAILURE, not a counted DDL-gap skip: the vendored
//     corpus's schema templates probe engine surface the port may not have,
//     but a generated schema was created successfully when the scenario was
//     blessed, so its rejection later is a regression, never a gap.
//
// Everything the runner itself does — the schema_template lifecycle, connect
// resolution, option layering, sticky metadata, result matching — is shared,
// not forked.
func RunParsed(ctx context.Context, file *javayamsql.File, cfg Config) FileResult {
	res := FileResult{Path: file.Path}
	r := &runner{
		cfg:             cfg,
		result:          &res,
		connsByResource: map[string][]connTarget{},
		dbs:             map[connTarget]*sql.DB{},
	}
	defer r.teardown()

	runErr := r.execute(ctx, file)

	var skipErr *skipFileError
	if errors.As(runErr, &skipErr) {
		res.Status = StatusSkip
		res.SkipClass = skipErr.class
		res.Skips = append(res.Skips, Skip{Class: skipErr.class, Where: file.Path, Detail: skipErr.detail})
		return res
	}
	if runErr != nil {
		res.Status = StatusFail
		res.Err = runErr
		return res
	}
	// A file that asserted nothing is not a pass. Reporting it as one is how a
	// skipped dimension reads as covered.
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
