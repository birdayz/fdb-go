package fleet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/keyspace"
)

// BuildOptions adds the OnlineIndexer throttle knobs to the shared fan-out
// options. Zero values mean "leave the indexer's own default alone" — the
// fleet driver must not silently re-specify a default the indexer owns.
type BuildOptions struct {
	Options
	// Limit is records per transaction.
	Limit int
	// RecordsPerSecond throttles the build.
	RecordsPerSecond int
	// MaxRetries is the per-range retry budget. It also arms the adaptive
	// batch-halving and the records-per-second throttle.
	MaxRetries int
	// TimeLimit stops a build early; the run is resumable.
	TimeLimit time.Duration
	// Logger, when non-nil, receives the indexer's own progress lines.
	Logger *slog.Logger
	// IndexNames, when non-empty, restricts the build to these indexes.
	// Rolling ONE new index across a fleet is the common case, and a
	// tenant that happens to carry an unrelated half-built index must not
	// be dragged into that roll-out.
	//
	// A name that matches nothing on a given tenant is not an error: tenants
	// legitimately sit at different template versions mid-migration, and a
	// fleet job must tolerate that rather than fail the tenant.
	IndexNames []string
}

// selectIndexes narrows pending indexes to the requested names. An empty
// request selects everything pending.
func selectIndexes(pending []*recordlayer.Index, names []string) []*recordlayer.Index {
	if len(names) == 0 {
		return pending
	}
	want := make(map[string]struct{}, len(names))
	for _, n := range names {
		want[strings.ToUpper(n)] = struct{}{}
	}
	out := make([]*recordlayer.Index, 0, len(pending))
	for _, idx := range pending {
		if _, ok := want[strings.ToUpper(idx.Name)]; ok {
			out = append(out, idx)
		}
	}
	return out
}

// underlyingProvider is the bridge from the relational template to the
// record-layer metadata the OnlineIndexer needs. Asserted, never fallen back
// from: a template that is not record-layer backed cannot be index-built, and
// guessing a substitute metadata would build the wrong index.
type underlyingProvider interface {
	Underlying() *recordlayer.RecordMetaData
}

// PinnedMetadata resolves the record-layer metadata for one schema at the
// template version the schema row is PINNED to.
//
// Never the latest version: a store pinned to template v1 opened with v2
// metadata would have anything honouring checkPossiblyRebuild WRITE to the
// store from what the caller believes is a read. The pinned version is also
// simply the truth — it is the metadata the records were written under.
//
// A fan-out index build therefore only ever builds indexes the schema is
// actually bound to. Making a new index visible to a tenant is the migration
// step's job, not this one's.
func PinnedMetadata(ctx context.Context, db *recordlayer.FDBDatabase, cat api.StoreCatalog, t Target) (*recordlayer.RecordMetaData, error) {
	var md *recordlayer.RecordMetaData
	err := inCatalogTx(ctx, db, func(txn api.Transaction) error {
		s, err := cat.LoadSchema(txn, t.DatabaseID, t.SchemaName)
		if err != nil {
			return err
		}
		tmpl := s.SchemaTemplate()
		up, ok := tmpl.(underlyingProvider)
		if !ok {
			return fmt.Errorf("template %q is not backed by a record-layer MetaData — catalog entry type %T",
				tmpl.MetadataName(), tmpl)
		}
		md = up.Underlying()
		return nil
	})
	if err != nil {
		return nil, err
	}
	if md == nil {
		return nil, fmt.Errorf("schema %s produced no metadata", t)
	}
	return md, nil
}

// PendingIndexes returns the indexes on one store that are not readable —
// DISABLED or WRITE_ONLY — in deterministic name order.
//
// The states come from the OPENED store's GetAllIndexStates, not from
// GetAllIndexStatesMap and not from a raw read of the index-state subspace.
// The three have different domains: GetAllIndexStatesMap returns only the
// persisted non-readable entries, while GetAllIndexStates walks every index in
// the metadata and defaults the missing ones to READABLE. An index added by a
// metadata evolution has NO state key until the store header is reconciled at
// open, so only opening the store and asking it produces the post-
// reconciliation truth a build must act on.
func PendingIndexes(
	ctx context.Context,
	db *recordlayer.FDBDatabase,
	md *recordlayer.RecordMetaData,
	ss subspace.Subspace,
) ([]*recordlayer.Index, error) {
	res, err := db.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		store, openErr := recordlayer.NewStoreBuilder().
			SetContext(rctx).
			SetMetaDataProvider(md).
			SetSubspace(ss).
			Open()
		if openErr != nil {
			return nil, openErr
		}
		return store.GetAllIndexStates(), nil
	})
	if err != nil {
		return nil, err
	}
	states, _ := res.(map[string]recordlayer.IndexState)
	var pending []*recordlayer.Index
	for name, idx := range md.GetAllIndexes() {
		// DISABLED and WRITE_ONLY only. READABLE_UNIQUE_PENDING is
		// deliberately NOT pending work: that index is fully built and is
		// waiting on uniqueness violations being resolved, which a rebuild
		// does not fix and would only redo.
		if st, ok := states[name]; ok && (st.IsDisabled() || st.IsWriteOnly()) {
			pending = append(pending, idx)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].Name < pending[j].Name })
	return pending, nil
}

// BuildIndexes drives every DISABLED / WRITE_ONLY index on every target to
// READABLE, one target at a time up to the configured concurrency.
//
// The OnlineIndexer manages its own transactions per range, so this fan-out
// spans many transactions per target on top of the one-transaction-per-target
// floor. A poisoned tenant fails alone: its error is recorded against its own
// target and the remaining tenants keep building.
func BuildIndexes(
	ctx context.Context,
	db *recordlayer.FDBDatabase,
	cat api.StoreCatalog,
	ks *keyspace.RelationalKeyspace,
	targets []Target,
	opts BuildOptions,
) (Result, error) {
	return fanOut(ctx, ks, targets, opts.Options, func(ctx context.Context, t Target) (Event, error) {
		ss, err := ks.SchemaSubspace(t.DatabaseID, t.SchemaName)
		if err != nil {
			return Event{}, err
		}
		md, err := PinnedMetadata(ctx, db, cat, t)
		if err != nil {
			return Event{}, err
		}
		pending, err := PendingIndexes(ctx, db, md, ss)
		if err != nil {
			return Event{}, err
		}
		pending = selectIndexes(pending, opts.IndexNames)
		if len(pending) == 0 {
			return Event{Outcome: OutcomeNoWork}, nil
		}
		var (
			names []string
			total int64
		)
		for _, idx := range pending {
			n, buildErr := buildOne(ctx, db, md, ss, idx, opts)
			if buildErr != nil {
				// Report what DID get built before the failure — a
				// half-finished tenant is the state an operator has to
				// reason about on the next run.
				return Event{Indexes: names, Records: total},
					fmt.Errorf("build index %q: %w", idx.Name, buildErr)
			}
			names = append(names, idx.Name)
			total += n
		}
		return Event{Outcome: OutcomeBuilt, Indexes: names, Records: total}, nil
	})
}

// buildOne drives a single index to READABLE.
func buildOne(
	ctx context.Context,
	db *recordlayer.FDBDatabase,
	md *recordlayer.RecordMetaData,
	ss subspace.Subspace,
	idx *recordlayer.Index,
	opts BuildOptions,
) (int64, error) {
	b := recordlayer.NewOnlineIndexerBuilder().
		SetDatabase(db).
		SetMetaData(md).
		SetSubspace(ss).
		SetIndex(idx).
		SetMarkReadable(true)
	if opts.Logger != nil {
		b = b.SetLogger(opts.Logger)
	}
	if opts.Limit > 0 {
		b = b.SetLimit(opts.Limit)
	}
	if opts.RecordsPerSecond > 0 {
		b = b.SetRecordsPerSecond(opts.RecordsPerSecond)
	}
	if opts.MaxRetries > 0 {
		b = b.SetMaxRetries(opts.MaxRetries)
	}
	if opts.TimeLimit > 0 {
		b = b.SetTimeLimit(opts.TimeLimit)
	}
	oi, err := b.Build()
	if err != nil {
		return 0, err
	}
	n, err := oi.BuildIndex(ctx)
	if err != nil {
		var partly *recordlayer.PartlyBuiltError
		if errors.As(err, &partly) {
			return n, fmt.Errorf("index %q has a partial build with DIFFERENT settings — "+
				"saved stamp %q, this invocation would stamp %q; rerun with the same settings "+
				"to take the build over, or start from scratch with `frl index rebuild`",
				partly.IndexName, partly.SavedStamp, partly.ExpectedStamp)
		}
		return n, err
	}
	return n, nil
}

// BuildAll is the whole index fan-out in one call: enumerate the schemas of
// databaseID (empty means every database) and build their pending indexes.
func BuildAll(
	ctx context.Context,
	db *recordlayer.FDBDatabase,
	cat api.StoreCatalog,
	ks *keyspace.RelationalKeyspace,
	databaseID string,
	opts BuildOptions,
) (Result, error) {
	targets, err := ListTargets(ctx, db, cat, databaseID)
	if err != nil {
		return Result{}, fmt.Errorf("list fan-out targets: %w", err)
	}
	return BuildIndexes(ctx, db, cat, ks, targets, opts)
}
