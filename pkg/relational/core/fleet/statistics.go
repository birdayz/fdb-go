package fleet

import (
	"context"
	"fmt"
	"sort"
	"strings"

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
	// type: crossing it ABORTS that schema's collection and stores nothing,
	// which surfaces as a per-target failure rather than an OutcomeCollected.
	Collect recordlayer.CollectOptions
}

// CollectStatistics gathers per-record-type row counts for every target,
// one transaction-bounded scan per schema.
//
// A target that ABORTS — because a type crossed MaxRecordsPerType — is a
// per-target FAILURE, not a collection. It stored nothing, so reporting it as
// collected would put it in the summary's collected tally and tell an operator a
// fan-out that wrote nothing had succeeded. An earlier revision of this comment
// argued the opposite, from a time when crossing the cap skipped one type and
// kept the rest; that behaviour is gone, and the reasoning went with it.
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
		// REFUSED, not no-work. The reader rejects metadata declaring synthetic
		// types outright, so scanning this tenant would bill a full pass for a set
		// that can never be planned with — but "no work" is what the line above
		// reports for a schema with zero record types, and collapsing the two
		// makes a million-row tenant print "nothing to build" and exit 0, while
		// `--schema` on that same tenant exits non-zero naming the types. One
		// outcome meaning two things depending on which flag was used is the exact
		// defect the capped-run fix closed; OutcomeRefused already exists, counts
		// separately in the summary, and prints its Err.
		if md.DeclaresSyntheticRecordTypes() {
			return Event{}, fmt.Errorf(
				"declares synthetic record types (%s) that this port does not model, so "+
					"collected statistics could never be complete enough to plan with",
				strings.Join(md.SyntheticRecordTypeNames(), ", "))
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
		// An ABORTED run stored nothing, so it is not "collected". Reporting it
		// as such makes a 500-tenant fan-out that wrote nothing at all print
		// collected=500 — a summary an operator reads as success, for the exact
		// state they most need to see. A capped run leaves report.Collected
		// empty and names the offending type in Skipped, which is what
		// distinguishes it from a schema that genuinely had nothing to count.
		if len(report.Collected) == 0 && len(report.Skipped) > 0 {
			// The scan volume goes in the ERROR, not in Event.Records: fanOut
			// overwrites Outcome on the error path, Result.record accumulates no
			// Records, and the failure printer renders only Err — so a Records set
			// here would be written and never read, which is the fourth instance of
			// that shape in this branch. It is also the number the operator most
			// wants: what the abandoned pass cost before it gave up.
			return Event{Skipped: report.Skipped},
				fmt.Errorf("collection aborted after %d records: %s",
					report.RecordsScanned, describeSkipped(report.Skipped))
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

// describeSkipped renders why a run was abandoned, for the per-target error.
// Sorted so a fan-out over many schemas produces diffable output.
func describeSkipped(skipped map[string]string) string {
	names := make([]string, 0, len(skipped))
	for name := range skipped {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+": "+skipped[name])
	}
	return strings.Join(parts, "; ")
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
