package fleet

import (
	"context"
	"fmt"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/keyspace"
)

// Statistics collection as a fan-out mode (RFC-236).
//
// Collecting planner statistics has exactly the properties this package was
// built for, and none that are new: it is offline maintenance, it is per-schema,
// it is expensive enough that one transaction per fleet is impossible, and one
// broken tenant must not stop the other thousand. So it is a `step`, like an
// index build — not a new mechanism, and not a loop an operator writes.
//
// The resume story differs from migration's in a way worth stating. A migration
// skips a schema already at the target version, so a re-run is nearly free. A
// collection has no such marker: freshness is a continuum, not a version to
// compare against, and re-collecting is the entire work. A second pass therefore
// costs the same as the first. That is the honest trade for exact counts, and it
// is why this is scheduled maintenance rather than something to run in a retry
// loop.

// OutcomeCollected means the schema's statistics were collected and stored.
const OutcomeCollected Outcome = "collected"

// StatisticsOptions tunes a statistics fan-out.
type StatisticsOptions struct {
	Options
	// Collect is passed to the collector for each schema. BatchSize bounds the
	// records per transaction; MaxRecordsPerType caps the work spent on one
	// type, recording it ABSENT rather than partial when exceeded.
	Collect recordlayer.CollectOptions
}

// CollectStatistics gathers per-record-type row counts for every target,
// one transaction-bounded scan per schema.
//
// A target whose collection SKIPS a type still counts as collected here. The
// distinction between "collected everything" and "collected some types" belongs
// to the reader, which refuses a schema that is not complete — reporting it as a
// fan-out failure would make an operator chase a per-schema outcome the fan-out
// cannot act on. The per-schema detail is in the Event's Skipped map.
func CollectStatistics(
	ctx context.Context,
	db *recordlayer.FDBDatabase,
	cat api.StoreCatalog,
	ks *keyspace.RelationalKeyspace,
	targets []Target,
	opts StatisticsOptions,
) (Result, error) {
	stats := recordlayer.NewStatisticsSubspace(ks.StatisticsSubspace())
	return fanOut(ctx, ks, targets, opts.Options, func(ctx context.Context, t Target) (Event, error) {
		ss, err := ks.SchemaSubspace(t.DatabaseID, t.SchemaName)
		if err != nil {
			return Event{}, err
		}
		md, err := PinnedMetadata(ctx, db, cat, t)
		if err != nil {
			return Event{}, err
		}
		// A schema with no record types has nothing to count, and collecting
		// for it would store an empty set the reader then refuses as an empty
		// schema. Report no-work rather than manufacturing a refusal.
		if len(md.RecordTypes()) == 0 {
			return Event{Outcome: OutcomeNoWork}, nil
		}
		report, err := recordlayer.CollectStatistics(ctx, db,
			func(rtx *recordlayer.FDBRecordContext) (*recordlayer.FDBRecordStore, error) {
				return recordlayer.NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					// Collection is a READ. Opening with the pinned template
					// metadata could otherwise trip checkPossiblyRebuild into
					// writing a header bump or index-rebuild mark against a
					// tenant nobody asked to migrate.
					SetSkipPossiblyRebuild(true).
					Open()
			}, stats, opts.Collect)
		if err != nil {
			return Event{}, fmt.Errorf("collect statistics: %w", err)
		}
		return Event{
			Outcome: OutcomeCollected,
			Records: report.RecordsScanned,
			Types:   len(report.Collected),
			Skipped: report.Skipped,
			Counts:  countsOf(report),
		}, nil
	})
}

// CollectAllStatistics collects for every schema in databaseID.
func CollectAllStatistics(
	ctx context.Context,
	db *recordlayer.FDBDatabase,
	cat api.StoreCatalog,
	ks *keyspace.RelationalKeyspace,
	databaseID string,
	opts StatisticsOptions,
) (Result, error) {
	targets, err := ListTargets(ctx, db, cat, databaseID)
	if err != nil {
		return Result{}, fmt.Errorf("list fan-out targets: %w", err)
	}
	return CollectStatistics(ctx, db, cat, ks, targets, opts)
}

// countsOf flattens a collection report to name -> count for the Event.
func countsOf(report *recordlayer.CollectionReport) map[string]int64 {
	if report == nil || len(report.Collected) == 0 {
		return nil
	}
	out := make(map[string]int64, len(report.Collected))
	for name, st := range report.Collected {
		out[name] = st.Count
	}
	return out
}
