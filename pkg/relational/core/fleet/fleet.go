// Package fleet drives fan-out maintenance across every tenant schema in a
// relational database: rebinding a fleet onto a new schema-template version,
// and building the indexes that rebind leaves unbuilt.
//
// The defining property is that a fan-out is NOT one transaction. RFC-204's
// original migration flow put the whole fleet rebind inside the single catalog
// transaction that saved the new template ("for each schema bound to tmpl,
// call RepairSchema(txn, ...)" then "commit catalog transaction"). That cannot
// work at fleet scale — FDB caps a transaction at 5 s and 10 MB, and a rebind
// touches one catalog row plus a store-header reconciliation per schema — and
// it inverts failure isolation: a single poisoned tenant aborts the commit and
// rolls back every healthy tenant with it.
//
// So atomicity is deliberately forfeited. Each schema gets its OWN
// transaction, and idempotence replaces atomicity as the correctness argument:
// a run that dies halfway leaves a well-defined partial state, and re-running
// converges. The resume key is the schema row's own TEMPLATE_VERSION
// (listSchemasImpl surfaces it), so a schema already bound at or above the
// target version is skipped without a write. A fan-out is therefore restartable
// at any point, and running it twice costs one catalog read per already-done
// tenant.
//
// Per-target failures are collected, never fatal — the same contract as
// SweepSPFreshIndexes: one corrupt tenant must not halt fleet maintenance. The
// pass continues and the joined error reports every failure.
package fleet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/keyspace"
)

// Target identifies one tenant schema, as the catalog currently records it.
// TemplateVersion is the version the schema row is PINNED to — the resume key.
type Target struct {
	DatabaseID      string
	SchemaName      string
	TemplateName    string
	TemplateVersion int
}

// String renders the target as "database/schema".
func (t Target) String() string { return t.DatabaseID + "/" + t.SchemaName }

// Outcome is what a fan-out did to one target.
type Outcome string

const (
	// OutcomeMigrated means the schema was rebound to a newer template version.
	OutcomeMigrated Outcome = "migrated"
	// OutcomeSkipped means the schema was ALREADY at or above the target
	// version, so no transaction was opened for it. This is the observable
	// that proves idempotent resume actually skipped work rather than
	// silently redoing it.
	OutcomeSkipped Outcome = "skipped"
	// OutcomeBuilt means at least one index was driven to READABLE.
	OutcomeBuilt Outcome = "built"
	// OutcomeNoWork means the store had no DISABLED/WRITE_ONLY index.
	OutcomeNoWork Outcome = "no-work"
	// OutcomeFailed means this target errored. Other targets still ran.
	OutcomeFailed Outcome = "failed"
	// OutcomeRefused means the catalog guard rejected the target.
	OutcomeRefused Outcome = "refused"
)

// Event is one progress notification. Exactly one Event is delivered per
// target per run.
type Event struct {
	Target  Target
	Outcome Outcome
	// Err is set when Outcome is OutcomeFailed or OutcomeRefused.
	Err error
	// Indexes names the indexes acted on (index-build mode only).
	Indexes []string
	// Records is the number of records scanned (index-build mode only).
	Records int64
	// FromVersion / ToVersion bracket a rebind (migration mode only).
	FromVersion int
	ToVersion   int
	// Types is how many record types got a statistic (statistics mode only).
	Types int
	// Skipped maps a record type to why it has NO statistic (statistics mode
	// only). Distinct from a zero count: "no rows" and "not counted" are
	// different facts and only one of them describes an empty table.
	Skipped map[string]string
	// Counts is the per-type row count collected (statistics mode only), so a
	// progress printer can show the numbers without a second read.
	Counts map[string]int64
}

// Options are the knobs shared by every fan-out mode.
type Options struct {
	// Concurrency bounds how many targets are in flight at once. <= 0 means
	// DefaultConcurrency. Each in-flight target holds its own FDB
	// transaction, so this is the real load knob against the cluster.
	Concurrency int
	// Progress, when non-nil, receives one Event per target. Calls are
	// serialised, so the callback does not need to be goroutine-safe.
	Progress func(Event)
}

// DefaultConcurrency is deliberately small: fan-out is background maintenance
// competing with live tenant traffic on the same cluster, and each slot can
// hold an OnlineIndexer that is itself already batching aggressively.
const DefaultConcurrency = 4

func (o Options) concurrency() int {
	if o.Concurrency <= 0 {
		return DefaultConcurrency
	}
	return o.Concurrency
}

// Result tallies a fan-out.
//
// The per-outcome counts sum to Total on a pass that ran to completion. On a
// context-cancelled pass they sum to LESS: the shortfall is the targets never
// attempted, and the returned error carries the context error. Do not treat
// Total as "targets handled".
type Result struct {
	Total     int
	Migrated  int
	Skipped   int
	Built     int
	Collected int
	NoWork    int
	Failed    int
	Refused   int
	// Failures carries one entry per failed or refused target.
	Failures []*TargetError
}

// merge folds another pass's tally into r. Used when one logical fan-out is
// executed as several Migrate passes — one per template — because the target
// version is a property of the template, not of the fleet.
func (r *Result) merge(o Result) {
	r.Total += o.Total
	r.Migrated += o.Migrated
	r.Skipped += o.Skipped
	r.Built += o.Built
	r.Collected += o.Collected
	r.NoWork += o.NoWork
	r.Failed += o.Failed
	r.Refused += o.Refused
	r.Failures = append(r.Failures, o.Failures...)
}

func (r *Result) record(ev Event) {
	switch ev.Outcome {
	case OutcomeMigrated:
		r.Migrated++
	case OutcomeSkipped:
		r.Skipped++
	case OutcomeBuilt:
		r.Built++
	case OutcomeCollected:
		r.Collected++
	case OutcomeNoWork:
		r.NoWork++
	case OutcomeFailed:
		r.Failed++
	case OutcomeRefused:
		r.Refused++
	}
}

// TargetError attaches a target to the error it produced, so a joined fleet
// error still says WHICH tenant broke.
type TargetError struct {
	Target Target
	Err    error
}

func (e *TargetError) Error() string { return fmt.Sprintf("%s: %v", e.Target, e.Err) }
func (e *TargetError) Unwrap() error { return e.Err }

// CatalogTargetError reports a fan-out target that resolves to the relational
// catalog itself.
type CatalogTargetError struct {
	DatabaseID string
	SchemaName string
	Reason     string
}

func (e *CatalogTargetError) Error() string {
	return fmt.Sprintf("refusing to fan out to %s/%s: %s — the relational catalog is never a "+
		"fan-out target (evolve schemas through SQL DDL)", e.DatabaseID, e.SchemaName, e.Reason)
}

// GuardNotCatalog rejects a target that is, or overlaps, the relational
// catalog store.
//
// This is reachable, not hypothetical: RecordLayerStoreCatalog.Initialize
// persists a /__SYS database row and a /__SYS/CATALOG schema row into the
// catalog it is initialising, so an unfiltered ListSchemas hands the catalog
// back as an ordinary fan-out target.
//
// The NAME check is the one that fires. The subspace check cannot catch this
// case under the default keyspace — CatalogSubspace is the 3-tuple
// ("__SYS","__SYS","CATALOG") while a schema store is the 2-tuple
// (dbPath, schemaName), so ("/__SYS","CATALOG") shares no byte prefix with it.
// The subspace check is kept as defence in depth for keyspace layouts where a
// user schema COULD be placed over the catalog's prefix.
func GuardNotCatalog(ks *keyspace.RelationalKeyspace, t Target) error {
	if strings.EqualFold(t.DatabaseID, catalog.SysDatabaseID) {
		return &CatalogTargetError{
			DatabaseID: t.DatabaseID,
			SchemaName: t.SchemaName,
			Reason:     "database is the system database " + catalog.SysDatabaseID,
		}
	}
	if ks == nil {
		return nil
	}
	ss, err := ks.SchemaSubspace(t.DatabaseID, t.SchemaName)
	if err != nil {
		// An unaddressable target is not a catalog target; the caller's
		// own resolution will report the real problem.
		return nil //nolint:nilerr // resolution errors are reported by the caller
	}
	catalogBytes := ks.CatalogSubspace().Bytes()
	target := ss.Bytes()
	if bytes.HasPrefix(target, catalogBytes) || bytes.HasPrefix(catalogBytes, target) {
		return &CatalogTargetError{
			DatabaseID: t.DatabaseID,
			SchemaName: t.SchemaName,
			Reason:     "target keyspace overlaps the catalog subspace",
		}
	}
	return nil
}

// inCatalogTx runs fn in ONE FDB transaction against the relational catalog.
func inCatalogTx(ctx context.Context, db *recordlayer.FDBDatabase, fn func(api.Transaction) error) error {
	_, err := db.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		return nil, fn(catalog.NewFDBTransaction(rctx))
	})
	return err
}

// ListTargets enumerates the schemas a fan-out would touch. An empty
// databaseID enumerates every database on the cluster; otherwise the listing
// is narrowed to that database URI.
//
// Catalog-owned rows are NOT filtered here — enumeration reports what the
// catalog holds, and GuardNotCatalog decides what may be written. Filtering
// early would hide the fact that the catalog self-registers.
func ListTargets(ctx context.Context, db *recordlayer.FDBDatabase, cat api.StoreCatalog, databaseID string) ([]Target, error) {
	var out []Target
	err := inCatalogTx(ctx, db, func(txn api.Transaction) error {
		var (
			rs      api.ResultSet
			listErr error
		)
		if databaseID != "" {
			rs, listErr = cat.ListSchemasInDatabase(txn, databaseID, nil)
		} else {
			rs, listErr = cat.ListSchemas(txn, nil)
		}
		if listErr != nil {
			return listErr
		}
		targets, collectErr := collectTargets(rs)
		if collectErr != nil {
			return collectErr
		}
		out = targets
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// collectTargets reads a ListSchemas ResultSet by COLUMN NAME, so a
// column-order change in the catalog cannot silently shift TemplateVersion
// (the resume key) onto the wrong column.
func collectTargets(rs api.ResultSet) ([]Target, error) {
	defer rs.Close()
	md := rs.MetaData()
	idx := make(map[string]int, md.ColumnCount())
	for i := 1; i <= md.ColumnCount(); i++ {
		name, err := md.ColumnName(i)
		if err != nil {
			return nil, fmt.Errorf("column %d name: %w", i, err)
		}
		idx[strings.ToUpper(name)] = i
	}
	for _, required := range []string{
		catalog.ColDatabaseID, catalog.ColSchemaName,
		catalog.ColTemplateName, catalog.ColTemplateVersion,
	} {
		if _, ok := idx[strings.ToUpper(required)]; !ok {
			return nil, fmt.Errorf("schema listing has no %s column — "+
				"fan-out cannot resume without it", required)
		}
	}
	var out []Target
	for rs.Next() {
		var t Target
		var err error
		if t.DatabaseID, err = rs.String(idx[strings.ToUpper(catalog.ColDatabaseID)]); err != nil {
			return nil, fmt.Errorf("read %s: %w", catalog.ColDatabaseID, err)
		}
		if t.SchemaName, err = rs.String(idx[strings.ToUpper(catalog.ColSchemaName)]); err != nil {
			return nil, fmt.Errorf("read %s: %w", catalog.ColSchemaName, err)
		}
		if t.TemplateName, err = rs.String(idx[strings.ToUpper(catalog.ColTemplateName)]); err != nil {
			return nil, fmt.Errorf("read %s: %w", catalog.ColTemplateName, err)
		}
		v, err := rs.Long(idx[strings.ToUpper(catalog.ColTemplateVersion)])
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", catalog.ColTemplateVersion, err)
		}
		t.TemplateVersion = int(v)
		out = append(out, t)
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// FilterByTemplate narrows targets to those bound to templateName. An empty
// templateName returns targets unchanged.
func FilterByTemplate(targets []Target, templateName string) []Target {
	if templateName == "" {
		return targets
	}
	out := make([]Target, 0, len(targets))
	for _, t := range targets {
		if strings.EqualFold(t.TemplateName, templateName) {
			out = append(out, t)
		}
	}
	return out
}

// step is the per-target unit of work. It returns the Event describing what it
// did; a non-nil error is turned into OutcomeFailed by the runner.
type step func(ctx context.Context, t Target) (Event, error)

// fanOut runs fn over targets with bounded concurrency and per-target failure
// isolation, and returns the tally plus the joined per-target errors.
//
// No target failure ever aborts the pass. Context cancellation DOES stop
// launching new targets — a cancelled context means the operator wants out,
// which is different from one tenant being broken.
func fanOut(ctx context.Context, ks *keyspace.RelationalKeyspace, targets []Target, opts Options, fn step) (Result, error) {
	var (
		mu     sync.Mutex
		result Result
		errs   []error
	)
	result.Total = len(targets)

	// emit serialises both the tally and the progress callback, so callers
	// get a consistent view and need no locking of their own.
	emit := func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		result.record(ev)
		if ev.Err != nil {
			te := &TargetError{Target: ev.Target, Err: ev.Err}
			result.Failures = append(result.Failures, te)
			errs = append(errs, te)
		}
		if opts.Progress != nil {
			opts.Progress(ev)
		}
	}

	sem := make(chan struct{}, opts.concurrency())
	var wg sync.WaitGroup
	var cancelled bool

	for _, t := range targets {
		if ctx.Err() != nil {
			cancelled = true
			break
		}
		// The guard runs BEFORE a slot is taken: refusing a target must
		// never depend on cluster capacity.
		if err := GuardNotCatalog(ks, t); err != nil {
			emit(Event{Target: t, Outcome: OutcomeRefused, Err: err})
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(t Target) {
			defer wg.Done()
			defer func() { <-sem }()
			ev, err := fn(ctx, t)
			ev.Target = t
			if err != nil {
				ev.Outcome = OutcomeFailed
				ev.Err = err
			}
			emit(ev)
		}(t)
	}
	wg.Wait()

	if cancelled {
		errs = append(errs, ctx.Err())
	}
	return result, errors.Join(errs...)
}
