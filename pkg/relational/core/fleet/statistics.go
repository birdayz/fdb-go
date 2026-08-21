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
// costs the same as the first. That is the honest trade for COUNTING rather than
// sampling, and it is why this is scheduled maintenance rather than something to
// run in a retry loop. (Counted, not "exact" unqualified: see
// recordlayer.RecordTypeStatistic for what a concurrent primary-key move does.)

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
	load := func(ctx context.Context, t Target) (*recordlayer.RecordMetaData, error) {
		return PinnedMetadata(ctx, db, cat, t)
	}
	return fanOut(ctx, ks, targets, opts.Options, collectStatisticsStep(db, ks, stats, opts, load))
}

// collectStatisticsStep is the per-target step CollectStatistics fans out.
//
// Named rather than inline so a test can drive THE PRODUCTION CLOSURE through
// fanOut. A test that builds its own closure of the same shape verifies its own
// copy: the defect this guards — returning a non-nil error alongside a REFUSED
// event, which makes fanOut stamp FAILED over it — lives in the caller's return
// statement, and a hand-written stand-in simply does not contain it.
// loadMetadata is how the step obtains a target's metadata. Injected so a test
// can drive THE PRODUCTION STEP — including its return statements, which is
// where the defect this guards actually lives — without a cluster or a catalog.
type loadMetadata func(context.Context, Target) (*recordlayer.RecordMetaData, error)

func collectStatisticsStep(
	db *recordlayer.FDBDatabase,
	ks *keyspace.RelationalKeyspace,
	stats recordlayer.StatisticsSubspace,
	opts StatisticsOptions,
	load loadMetadata,
) step {
	return func(ctx context.Context, t Target) (Event, error) {
		// Metadata FIRST, and the refusals below it, before the subspace is
		// resolved or any store is opened. Every decision this step can reach
		// without touching the cluster belongs above the ones that touch it —
		// the whole point of refusing here is not paying for the work.
		md, err := load(ctx, t)
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
		if ev, refused := syntheticRefusal(md); refused {
			return ev, nil
		}
		// AND the ambiguity preflight, for the same reason and at the same place.
		// The single-schema collector refuses a colliding schema before scanning;
		// this path checked only synthetic types, so `--all-schemas` scanned the
		// whole store and reported OutcomeCollected for a set the shared reader
		// always refuses. One rule enforced at one entry point and not its
		// sibling, which is the same gap the collector-level synthetic check was
		// added to close.
		if ev, refused := ambiguousRefusal(md); refused {
			return ev, nil
		}
		ss, err := ks.SchemaSubspace(t.DatabaseID, t.SchemaName)
		if err != nil {
			return Event{}, err
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
			// Neither Records nor Skipped is read on a non-collected outcome: the
			// Failed and Refused printers render only Err, and Result.record
			// accumulates neither. Both facts go in the error text, which is the
			// one field an operator actually sees.
			return Event{},
				fmt.Errorf("collection aborted after %d records: %s",
					report.RecordsScanned, describeSkipped(report.Skipped))
		}
		// No Skipped on this event, deliberately. report.Skipped is non-empty
		// ONLY on the aborted path, which leaves report.Collected empty -- and
		// that case returned an error just above. So a Skipped carried here
		// would provably always be empty, and a printer looping over it could
		// never render a line. The aborted run's detail reaches the operator
		// through that error text instead, which is the field the Failed printer
		// actually renders.
		return Event{
			Outcome: OutcomeCollected,
			Records: report.RecordsScanned,
			Types:   len(report.Collected),
		}, nil
	}
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
	// Record-type keys are STORAGE names, and this text is what the fan-out
	// prints for a refused or failed target -- the caller's own comment calls it
	// "the one field an operator actually sees", which is exactly why it must not
	// name a table the operator does not have.
	//
	// Decoding is decided ONCE for the whole string, from every name it will
	// print, and it is suppressed entirely if any decoded spelling would be
	// another entry's stored name or would collide with another decoded one.
	// Two separate reasons, both learned the hard way in cmd/frl:
	//
	//   - Bare ToUserIdentifier has no round-trip guard, so a type legitimately
	//     named __0Order renders as __Order, which re-encodes to __Order and
	//     resolves to nothing.
	//   - Keying a map by the decoded name LOSES a row on a collision rather
	//     than merely mislabelling it -- one skipped type silently vanishes from
	//     the one field an operator reads.
	//
	// SCOPE, and it is narrower than it looks: the decision is taken over the
	// names this string will PRINT, not over every name the schema DECLARES. Two
	// declared types can collide while the skipped subset does not, and this
	// decoder would then decode happily on a schema that is ambiguous. What
	// closes that is not this function -- it is ambiguousRefusal, which turns the
	// whole target away before any collection runs, so a declared collision never
	// reaches here. Delete that guard and this one narrows silently rather than
	// failing.
	//
	// cmd/frl's docscheck gate cannot see this file, so the invariant is carried
	// by calling the SHARED policy rather than by that gate. It used to be a
	// local copy justified by "a test pins that the two agree" -- there was no
	// such test, and there could not easily be one across an unexported boundary,
	// so the copy is gone instead.
	stored := make([]string, 0, len(skipped))
	for name := range skipped {
		stored = append(stored, name)
	}
	decode := recordlayer.SafeDecoderOver(stored, nil)

	names := make([]string, 0, len(skipped))
	for name := range skipped {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return decode(names[i]) < decode(names[j]) })
	parts := make([]string, 0, len(names))
	for _, name := range names {
		// Keyed by the STORED name throughout; only the printed label is decoded,
		// so no row can be lost to a collision.
		parts = append(parts, decode(name)+": "+skipped[name])
	}
	return strings.Join(parts, "; ")
}

// syntheticRefusal returns the refusal Event for metadata this port does not
// fully model, and whether it applies.
//
// The OUTCOME is set here and the caller returns a NIL error. Returning a
// non-nil error instead makes fanOut stamp OutcomeFailed unconditionally, and
// that is a different instruction to an operator: `failed` says retry, and no
// retry changes a property of the metadata. emit still joins on ev.Err, so the
// non-zero exit survives — the same shape the catalog guard's refusal uses.
//
// Split out so a test can reach it without a cluster or a catalog: the
// relational SQL layer cannot construct synthetic metadata, so a driver-level
// test cannot get here at all, and an inline guard would be unreachable by
// anything except production traffic that this port refuses by design.
func syntheticRefusal(md *recordlayer.RecordMetaData) (Event, bool) {
	if !md.DeclaresSyntheticRecordTypes() {
		return Event{}, false
	}
	// WRAPS the collector's typed error, for the same reason the connection
	// does: one rule fired at three depths must be ONE error type, or a test
	// pinning it can only fire on whichever path it happens to take.
	return Event{
		Outcome: OutcomeRefused,
		Err: fmt.Errorf("%w", &recordlayer.SyntheticRecordTypesNotModeledError{
			TypeNames: md.SyntheticRecordTypeNames(),
		}),
	}, true
}

// ambiguousRefusal returns the refusal Event for metadata whose declared record
// types collide across the SQL and storage namespaces, the same condition the
// reader refuses in decideStatistics.
//
// Metadata-only, so it costs no I/O and runs before a store is opened — and it
// must run here as well as in the single-schema collector, or `--all-schemas`
// scans a store to produce a set that can never be used and calls it collected.
// The names are already USER identifiers: AmbiguousDeclaredNames decodes them,
// because the operator has to act on the SQL names and the map is keyed by
// storage ones.
func ambiguousRefusal(md *recordlayer.RecordMetaData) (Event, bool) {
	pair, ambiguous := md.AmbiguousDeclaredNames()
	if !ambiguous {
		return Event{}, false
	}
	return Event{
		Outcome: OutcomeRefused,
		Err: fmt.Errorf(
			"declares record types whose names collide across the SQL and storage "+
				"namespaces (%s), so a lookup cannot say which table is meant and the "+
				"planner would always refuse the result", strings.Join(pair, " and ")),
	}, true
}
