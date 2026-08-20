package embedded

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/session"
	"google.golang.org/protobuf/proto"
)

// decideStatistics is the whole read-side gate (RFC-236). It is exercised here
// rather than through a cluster because two of its arms need a cluster
// MISBEHAVING — a version read that fails, an entry stamped ahead of the
// cluster after a restore from backup — and an arm no test drives fires for the
// first time in front of an operator, where it reads as a finding rather than
// as an untested branch.
//
// TestDecideStatisticsCoversEveryRefusal is the vacuity guard. Its exact reach
// is stated at the test itself, because the obvious claim — "a new refusal
// without a case fails the build" — is FALSE, and this file exists to stop
// exactly that kind of over-claim.

const testVersion int64 = 1_000_000_000_000

func statsAt(version int64, counts map[string]int64) recordlayer.StoreStatistics {
	s := recordlayer.StoreStatistics{
		PerType:            make(map[string]recordlayer.RecordTypeStatistic, len(counts)),
		CollectedAtVersion: version,
	}
	for name, c := range counts {
		s.PerType[name] = recordlayer.RecordTypeStatistic{Count: c, CollectedAtVersion: version}
	}
	return s
}

type decideCase struct {
	name string
	in   statisticsGateInput
	want StatisticsRefusal
	// check runs extra assertions on a verdict whose refusal already matched,
	// for the fields the CLI renders.
	check func(t *testing.T, got StatisticsStatus)
}

func decideStatisticsCases() []decideCase {
	return []decideCase{
		{
			name: "usable",
			in: statisticsGateInput{
				Found:          true,
				Stats:          statsAt(testVersion, map[string]int64{"A": 7, "B": 400}),
				CurrentVersion: testVersion + 1000,
				DeclaredTypes:  []string{"A", "B"},
			},
			want: StatisticsOK,
			check: func(t *testing.T, got StatisticsStatus) {
				if !got.Usable {
					t.Errorf("Usable = false for the OK arm")
				}
				want := map[string]float64{"A": 7, "B": 400}
				if !reflect.DeepEqual(got.perType, want) {
					t.Errorf("perType = %v, want %v", got.perType, want)
				}
				if got.AgeVersions != 1000 {
					t.Errorf("AgeVersions = %d, want 1000", got.AgeVersions)
				}
			},
		},
		{
			// Refused BEFORE the read, because no read can repair it. When the
			// metadata declares joined or unnested types, RecordTypes() omits
			// them, so DeclaredTypes is a PARTIAL set and the completeness gate
			// would certify a schema after checking a subset of its types —
			// arriving at the exact inversion the gate exists to prevent, by way
			// of the gate.
			name: "unmodeled synthetic types",
			in: statisticsGateInput{
				HasSyntheticTypes: true,
				// Everything else is healthy on purpose: a complete, fresh set is
				// present, so this arm can only pass by refusing on the synthetic
				// declaration itself rather than tripping a later gate.
				Found:          true,
				Stats:          statsAt(testVersion, map[string]int64{"A": 7}),
				CurrentVersion: testVersion + 1,
				DeclaredTypes:  []string{"A"},
			},
			want: StatisticsSyntheticTypes,
			check: func(t *testing.T, got StatisticsStatus) {
				if got.Found {
					t.Errorf("Found = true — the refusal must precede the read, since a " +
						"read cannot supply types the metadata model does not carry")
				}
			},
		},
		{
			name: "read failed",
			in: statisticsGateInput{
				ReadErr:       errors.New("transaction too old"),
				DeclaredTypes: []string{"A"},
			},
			want: StatisticsReadFailed,
			check: func(t *testing.T, got StatisticsStatus) {
				// A failed read must not claim statistics exist — an operator
				// reading "found, but unusable" would go looking for a stale
				// entry that may not be there at all.
				if got.Found {
					t.Errorf("Found = true after a failed read")
				}
			},
		},
		{
			name: "never collected",
			in: statisticsGateInput{
				Found:          false,
				CurrentVersion: testVersion,
				DeclaredTypes:  []string{"A"},
			},
			want: StatisticsNotCollected,
			check: func(t *testing.T, got StatisticsStatus) {
				if got.Found {
					t.Errorf("Found = true when nothing was collected")
				}
			},
		},
		{
			name: "version read failed",
			in: statisticsGateInput{
				Found:         true,
				Stats:         statsAt(testVersion, map[string]int64{"A": 7}),
				VersionErr:    errors.New("cluster unreachable"),
				DeclaredTypes: []string{"A"},
			},
			want: StatisticsVersionUnavailable,
			check: func(t *testing.T, got StatisticsStatus) {
				// Found stays true: the entry IS there, its age is what is
				// unknown. That distinction is the whole reason Found is a
				// separate field from Usable.
				if !got.Found {
					t.Errorf("Found = false though the entry was read")
				}
			},
		},
		{
			name: "stamped ahead of the cluster",
			in: statisticsGateInput{
				Found: true,
				Stats: statsAt(testVersion+1, map[string]int64{"A": 7}),
				// A restore from backup moves the cluster's version BACKWARDS,
				// leaving an entry stamped in the abandoned future. Read as
				// "age is negative, therefore fresh", it would never expire.
				CurrentVersion: testVersion,
				DeclaredTypes:  []string{"A"},
			},
			want: StatisticsStampedInFuture,
			check: func(t *testing.T, got StatisticsStatus) {
				if got.AgeVersions >= 0 {
					t.Errorf("AgeVersions = %d, want negative", got.AgeVersions)
				}
			},
		},
		{
			name: "exactly at the age bound is still fresh",
			in: statisticsGateInput{
				Found:          true,
				Stats:          statsAt(testVersion, map[string]int64{"A": 7}),
				CurrentVersion: testVersion + statisticsMaxAgeVersions,
				DeclaredTypes:  []string{"A"},
			},
			// The bound is inclusive. Pinned because an off-by-one here is
			// invisible in production: it only ever costs a plan quality, never
			// a wrong row, so nothing would ever report it.
			want: StatisticsOK,
		},
		{
			name: "one version past the bound is expired",
			in: statisticsGateInput{
				Found:          true,
				Stats:          statsAt(testVersion, map[string]int64{"A": 7}),
				CurrentVersion: testVersion + statisticsMaxAgeVersions + 1,
				DeclaredTypes:  []string{"A"},
			},
			want: StatisticsExpired,
		},
		{
			name: "incomplete — a declared type has no entry",
			in: statisticsGateInput{
				Found:          true,
				Stats:          statsAt(testVersion, map[string]int64{"A": 7}),
				CurrentVersion: testVersion + 1,
				DeclaredTypes:  []string{"A", "B", "C"},
			},
			want: StatisticsIncomplete,
			check: func(t *testing.T, got StatisticsStatus) {
				want := []string{"B", "C"}
				if !reflect.DeepEqual(got.MissingTypes, want) {
					t.Errorf("MissingTypes = %v, want %v (sorted)", got.MissingTypes, want)
				}
			},
		},
		{
			// TWO TABLES WHOSE NAMES COLLIDE ACROSS THE NAMESPACES.
			//
			// MY$TABLE is STORED as MY__1TABLE, and a table whose SQL name IS
			// MY__1TABLE is stored as MY__01TABLE. With both declared, the SQL
			// name MY__1TABLE matches a storage key directly — the first table's
			// — so the provider answers with the WRONG table's count and never
			// consults the escaped form. Everything else about the set is
			// healthy, which is why this has to be refused here rather than
			// noticed downstream.
			name: "declared names collide across the SQL and storage namespaces",
			in: statisticsGateInput{
				Found: true,
				Stats: statsAt(testVersion, map[string]int64{
					"MY__1TABLE":  9,
					"MY__01TABLE": 4000,
				}),
				CurrentVersion: testVersion + 1,
				DeclaredTypes:  []string{"MY__1TABLE", "MY__01TABLE"},
			},
			want: StatisticsAmbiguousNames,
			check: func(t *testing.T, got StatisticsStatus) {
				if got.Usable {
					t.Errorf("Usable = true — an ambiguous name prices one of the two " +
						"tables with the other's count, silently")
				}
				// USER identifiers, not the storage names the map is keyed by.
				// MY__1TABLE decodes to MY$TABLE and MY__01TABLE to MY__1TABLE, and
				// those are the names an operator can actually rename. Reporting the
				// raw keys names a table that does not exist.
				want := []string{"MY$TABLE", "MY__1TABLE"}
				if len(got.AmbiguousTypes) != 2 ||
					got.AmbiguousTypes[0] != want[0] || got.AmbiguousTypes[1] != want[1] {
					t.Errorf("AmbiguousTypes = %v, want %v — the refusal has to name the "+
						"pair in USER identifiers, or an operator is told to rename a "+
						"table that does not exist", got.AmbiguousTypes, want)
				}
			},
		},
		{
			// TWO collisions at once. ambiguousStorageName reports the lower
			// pair so an operator comparing two runs sees the same answer, and
			// map iteration order is randomised in Go — so without the tie-break
			// this reports either pair at random. One collision cannot exercise
			// that; the claim was unpinned until there were two.
			name: "two collisions report the lower pair deterministically",
			in: statisticsGateInput{
				Found: true,
				Stats: statsAt(testVersion, map[string]int64{
					"AA__1T": 1, "AA__01T": 2,
					"ZZ__1T": 3, "ZZ__01T": 4,
				}),
				CurrentVersion: testVersion + 1,
				DeclaredTypes:  []string{"AA__1T", "AA__01T", "ZZ__1T", "ZZ__01T"},
			},
			want: StatisticsAmbiguousNames,
			check: func(t *testing.T, got StatisticsStatus) {
				// Decoded: AA__1T is the storage name of AA$T, and AA__01T of AA__1T.
				want := []string{"AA$T", "AA__1T"}
				if len(got.AmbiguousTypes) != 2 ||
					got.AmbiguousTypes[0] != want[0] || got.AmbiguousTypes[1] != want[1] {
					t.Errorf("AmbiguousTypes = %v, want %v — with two collisions the pair "+
						"reported must not depend on map iteration order",
						got.AmbiguousTypes, want)
				}
			},
		},
		{
			name: "escaped name that does NOT collide is fine",
			in: statisticsGateInput{
				Found: true,
				Stats: statsAt(testVersion, map[string]int64{
					"MY__1TABLE": 9,
					"PLAIN":      4000,
				}),
				CurrentVersion: testVersion + 1,
				DeclaredTypes:  []string{"MY__1TABLE", "PLAIN"},
			},
			want: StatisticsOK,
			check: func(t *testing.T, got StatisticsStatus) {
				if !got.Usable {
					t.Errorf("Usable = false — MY__1TABLE escapes to MY__01TABLE, which is " +
						"NOT declared here, so nothing is ambiguous and refusing would " +
						"disable statistics for any schema holding a quotable name")
				}
			},
		},
		{
			// A TORN SET IS NOT AN ABSENT ONE.
			//
			// Both give Found=false, and reporting the first as the second tells
			// an operator the store is empty while it holds something broken.
			// Only StatisticsReadAbsent means absent; the other seven read
			// refusals mean a set IS stored and cannot be vouched for.
			name: "a torn set is refused as torn, not as not-collected",
			in: statisticsGateInput{
				Found:         false,
				ReadRefusal:   recordlayer.StatisticsReadCountMismatch,
				DeclaredTypes: []string{"A"},
			},
			want: StatisticsTorn,
			check: func(t *testing.T, got StatisticsStatus) {
				if got.Usable {
					t.Errorf("Usable = true for a torn set")
				}
				if got.ReadRefusal != recordlayer.StatisticsReadCountMismatch {
					t.Errorf("ReadRefusal = %q, want the read's own reason — an operator "+
						"needs to know WHICH way the set is broken", got.ReadRefusal)
				}
			},
		},
		{
			// The other side: genuinely absent must still be not-collected, or
			// the arm above has simply swallowed the ordinary case.
			name: "an absent set is still not-collected",
			in: statisticsGateInput{
				Found:         false,
				ReadRefusal:   recordlayer.StatisticsReadAbsent,
				DeclaredTypes: []string{"A"},
			},
			want: StatisticsNotCollected,
			check: func(t *testing.T, got StatisticsStatus) {
				if got.Usable {
					t.Errorf("Usable = true for an absent set")
				}
			},
		},
		{
			// AMBIGUITY IS DECIDED BEFORE THE STATISTICS STATE, not after.
			//
			// This case has NO collected set, which is the common state for a
			// schema nobody has collected yet. With the ambiguity check placed
			// after freshness and completeness it returned NotCollected here, and
			// three things followed: `stats show` recommended a collection that
			// CollectStatistics refuses, fetchCollectedStatistics reported no
			// STRUCTURAL refusal so SELECT and DML fell through to the legacy
			// count-key provider, and that provider can price the wrong table on
			// exactly the hand-built metadata this collision needs.
			//
			// A metadata-only verdict behind state-dependent gates is delivered
			// only for schemas that are otherwise healthy — the ones needing it
			// least.
			name: "ambiguous names refuse even with NO statistics collected",
			in: statisticsGateInput{
				Found:         false,
				ReadRefusal:   recordlayer.StatisticsReadAbsent,
				DeclaredTypes: []string{"MY__1TABLE", "MY__01TABLE"},
			},
			want: StatisticsAmbiguousNames,
			check: func(t *testing.T, got StatisticsStatus) {
				if got.Usable {
					t.Errorf("Usable = true for an ambiguous schema")
				}
				if len(got.AmbiguousTypes) != 2 {
					t.Errorf("AmbiguousTypes = %v, want the pair even when nothing is "+
						"stored — the collision is a property of the SCHEMA",
						got.AmbiguousTypes)
				}
			},
		},
		{
			// Same, with a set that exists but is EXPIRED: the ambiguity still
			// wins, because no amount of re-collecting repairs it.
			name: "ambiguous names refuse ahead of expiry",
			in: statisticsGateInput{
				Found:          true,
				Stats:          statsAt(testVersion, map[string]int64{"MY__1TABLE": 9, "MY__01TABLE": 4000}),
				CurrentVersion: testVersion + statisticsMaxAgeVersions + 1,
				DeclaredTypes:  []string{"MY__1TABLE", "MY__01TABLE"},
			},
			want: StatisticsAmbiguousNames,
			check: func(t *testing.T, got StatisticsStatus) {
				if got.Usable {
					t.Errorf("Usable = true for an ambiguous schema")
				}
			},
		},
		{
			name: "empty schema",
			in: statisticsGateInput{
				Found:          true,
				Stats:          statsAt(testVersion, nil),
				CurrentVersion: testVersion + 1,
				DeclaredTypes:  nil,
			},
			want: StatisticsEmptySchema,
		},
		{
			name: "an orphan entry does not refuse",
			in: statisticsGateInput{
				Found: true,
				// DROPPED was collected before the table was dropped. The
				// planner asks by declared type name, so it never asks for it.
				Stats:          statsAt(testVersion, map[string]int64{"A": 7, "DROPPED": 99}),
				CurrentVersion: testVersion + 1,
				DeclaredTypes:  []string{"A"},
			},
			want: StatisticsOK,
			check: func(t *testing.T, got StatisticsStatus) {
				if want := []string{"DROPPED"}; !reflect.DeepEqual(got.ExtraTypes, want) {
					t.Errorf("ExtraTypes = %v, want %v", got.ExtraTypes, want)
				}
				// And the orphan must not reach the provider — its count would
				// inflate the whole-store total that an unknown-type leaf reads.
				if _, leaked := got.perType["DROPPED"]; leaked {
					t.Errorf("perType carries the dropped type: %v", got.perType)
				}
			},
		},
	}
}

func TestDecideStatistics(t *testing.T) {
	t.Parallel()
	for _, tc := range decideStatisticsCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := decideStatistics(tc.in)
			if got.Refusal != tc.want {
				t.Fatalf("Refusal = %q, want %q (Usable=%v)", got.Refusal, tc.want, got.Usable)
			}
			// Usable and Refusal are two spellings of one fact; a verdict where
			// they disagree would let a caller checking either one be right and
			// the other be wrong.
			if got.Usable != (tc.want == StatisticsOK) {
				t.Errorf("Usable = %v but Refusal = %q — the two disagree", got.Usable, got.Refusal)
			}
			// A refusal must never hand the planner a provider input.
			if !got.Usable && got.perType != nil {
				t.Errorf("refused with %q but perType = %v", got.Refusal, got.perType)
			}
			if got.MaxAgeVersions != statisticsMaxAgeVersions {
				t.Errorf("MaxAgeVersions = %d, want %d", got.MaxAgeVersions, statisticsMaxAgeVersions)
			}
			if tc.check != nil {
				tc.check(t, got)
			}
		})
	}
}

// TestDecideStatisticsCoversEveryRefusal is the vacuity guard, and its reach is
// worth writing down because the natural claim for it is wrong. Go cannot
// enumerate a package's constants at runtime, so `all` below is hand-maintained,
// and the guard catches two of the three ways coverage can rot:
//
//	a constant in `all` with no case producing it     -> caught
//	a case producing a verdict `all` does not name     -> caught
//	a constant with NEITHER a case nor an `all` entry  -> NOT caught
//
// The third is the residue of hand-maintenance. It is narrowed by the two that
// ARE caught — adding a refusal means editing decideStatistics, and the arm
// producing it has to come from somewhere — but it is not closed, and claiming
// otherwise would be the exact failure this file exists to prevent.
func TestDecideStatisticsCoversEveryRefusal(t *testing.T) {
	t.Parallel()
	// The const block carries a pointer back to this list, because the guard
	// cannot catch a constant absent from BOTH here and the cases below.
	allStatisticsRefusals := []StatisticsRefusal{
		StatisticsOK,
		StatisticsNotCollected,
		StatisticsReadFailed,
		StatisticsVersionUnavailable,
		StatisticsStampedInFuture,
		StatisticsExpired,
		StatisticsIncomplete,
		StatisticsEmptySchema,
		StatisticsSyntheticTypes,
		StatisticsAmbiguousNames,
		StatisticsTorn,
	}
	covered := map[StatisticsRefusal]int{}
	for _, tc := range decideStatisticsCases() {
		covered[decideStatistics(tc.in).Refusal]++
	}
	for _, r := range allStatisticsRefusals {
		if covered[r] == 0 {
			t.Errorf("no case in decideStatisticsCases produces refusal %q — add one", r)
		}
	}
	// And the reverse: a verdict the list does not name means a constant was
	// added without being registered here, which would silently shrink the
	// guard above to whatever it still happens to cover.
	named := map[StatisticsRefusal]bool{}
	for _, r := range allStatisticsRefusals {
		named[r] = true
	}
	for r := range covered {
		if !named[r] {
			t.Errorf("cases produce refusal %q, which this guard's list does not name", r)
		}
	}
	if len(covered) != len(allStatisticsRefusals) {
		t.Errorf("covered %d distinct refusals, want %d", len(covered), len(allStatisticsRefusals))
	}
}

// THE SYNTHETIC-TYPE VERDICT MUST COST NO I/O.
//
// The refusal is fixed by a property of the metadata, so reading statistics
// cannot change it — and reading anyway costs an FDB transaction on every opt-in
// plan-cache miss for such a schema, one that may retry or wait on a cluster
// whose answer is then discarded.
//
// "It does not read" is the kind of claim that passes by inspection and rots
// silently, so this drives decideStatistics with gate inputs that would produce
// a DIFFERENT verdict if the read had happened. If the short-circuit is ever
// removed, the reader's result reaches the gate and the refusal changes.
func TestSyntheticVerdictIgnoresEverythingElse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   statisticsGateInput
	}{
		{
			// A read that FAILED. Without the short-circuit this is
			// StatisticsReadFailed — a transient verdict that invites a retry,
			// for a condition no retry can fix.
			name: "a failed read does not mask it",
			in: statisticsGateInput{
				HasSyntheticTypes:  true,
				SyntheticTypeNames: []string{"JoinedAB"},
				ReadErr:            &noClusterVersionError{},
				DeclaredTypes:      []string{"A"},
			},
		},
		{
			// A complete, fresh, perfectly usable set. Without the
			// short-circuit this is StatisticsOK — the schema is certified from
			// a type list that omits the synthetic ones, which is the inversion
			// the gate exists to prevent.
			name: "a healthy set does not override it",
			in: statisticsGateInput{
				HasSyntheticTypes:  true,
				SyntheticTypeNames: []string{"JoinedAB"},
				Found:              true,
				Stats:              statsAt(testVersion, map[string]int64{"A": 7}),
				CurrentVersion:     testVersion + 1,
				DeclaredTypes:      []string{"A"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := decideStatistics(tc.in)
			if got.Refusal != StatisticsSyntheticTypes {
				t.Fatalf("Refusal = %q, want %q — the synthetic gate must outrank every "+
					"other input, or a transient verdict sends an operator to re-collect "+
					"into the same refusal, and a healthy set certifies a partial type list",
					got.Refusal, StatisticsSyntheticTypes)
			}
			if got.Usable {
				t.Error("Usable = true for metadata whose type list is incomplete by construction")
			}
			// The names are what make the verdict actionable.
			if len(got.SyntheticTypes) == 0 {
				t.Error("SyntheticTypes is empty — the refusal costs a schema all its " +
					"statistics without saying which declaration did it")
			}
		})
	}
}

// THE SYNTHETIC VERDICT MUST COST NO I/O, AND THIS DRIVES THE GATHERER TO PROVE IT.
//
// The sibling test above drives decideStatistics, which performs no I/O of its
// own — so it stays green whether or not evaluateCollectedStatistics returns
// early, and cannot observe the regression it names. Pinning the predicate is
// not pinning the path.
//
// This drives evaluateCollectedStatistics with a connection whose session
// database is nil. If the early return is removed, the function reaches
// statisticsLocation and ReadStatisticsAt and PANICS on that nil, which the
// recover below reports.
//
// The dependency is worth stating: it survives on the panic ALONE. An earlier
// version of this comment said "panics or errors — either way it stops
// returning StatisticsSyntheticTypes", and the second half is false: on an
// error, evaluateCollectedStatistics sets in.ReadErr and decideStatistics'
// GATE 0 still returns StatisticsSyntheticTypes, so the test would pass with the
// I/O done. A defensive nil-guard added to statisticsLocation would therefore
// disarm this test silently. If that happens, the seam has to become an explicit
// failing database rather than a nil.
func TestSyntheticVerdictTouchesNoIO(t *testing.T) {
	t.Parallel()

	md := syntheticTestMetaData(t)
	if !md.DeclaresSyntheticRecordTypes() {
		t.Fatal("fixture declares no synthetic types; this cannot reach the gate")
	}

	// DB and Keyspace deliberately nil: the read path cannot run without them,
	// so reaching the read at all is observable rather than merely slower.
	c := &EmbeddedConnection{sess: &session.Session{Schema: "S", DBPath: "/db"}}

	var st StatisticsStatus
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("evaluateCollectedStatistics reached the read path for synthetic "+
					"metadata and panicked on the nil database (%v). The verdict is fixed "+
					"before any I/O, so the early return must precede statisticsLocation.", r)
			}
		}()
		st = evaluateCollectedStatistics(context.Background(), c, md)
	}()

	if st.Refusal != StatisticsSyntheticTypes {
		t.Fatalf("Refusal = %q, want %q", st.Refusal, StatisticsSyntheticTypes)
	}
	if len(st.SyntheticTypes) == 0 {
		t.Error("the refusal must name the declarations that caused it")
	}
}

// syntheticTestMetaData builds metadata carrying a joined-type declaration —
// the shape this port preserves opaquely and does not model.
func syntheticTestMetaData(t *testing.T) *recordlayer.RecordMetaData {
	t.Helper()
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	md, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	p, err := md.ToProto()
	if err != nil {
		t.Fatalf("to proto: %v", err)
	}
	name := "JoinedAB"
	p.JoinedRecordTypes = append(p.JoinedRecordTypes, &gen.JoinedRecordType{Name: &name})
	got, err := recordlayer.RecordMetaDataFromProto(p)
	if err != nil {
		t.Fatalf("from proto: %v", err)
	}
	return got
}

// THE CONNECTION'S EARLY REFUSAL CARRIES THE COLLECTOR'S TYPED ERROR.
//
// This is the path that was unreachable. The connection refused synthetic
// metadata with a freshly minted api.Error, so *SyntheticRecordTypesNotModeledError
// could not be matched through it — one rule with two representations, and an
// errors.As pin would have passed on the direct record-layer path while being
// structurally unable to fire here.
//
// Formatting the typed error into the message with %v would look identical to a
// reader and remain invisible to a matcher, so this asserts the CAUSE chain
// rather than the text.
func TestConnectionSyntheticRefusalCarriesTheTypedError(t *testing.T) {
	t.Parallel()

	// api.WrapErrorf is what the connection uses; this asserts the shape it
	// produces is matchable, independently of reaching FDB.
	wrapped := api.WrapErrorf(
		&recordlayer.SyntheticRecordTypesNotModeledError{TypeNames: []string{"OrderWithCustomer"}},
		api.ErrCodeUnsupportedOperation, "schema %q", "MAIN")

	var typed *recordlayer.SyntheticRecordTypesNotModeledError
	if !errors.As(wrapped, &typed) {
		t.Fatalf("the connection's refusal does not carry the collector's typed error, "+
			"so a matcher cannot reach it through the relational path: %v", wrapped)
	}
	if len(typed.TypeNames) == 0 || typed.TypeNames[0] != "OrderWithCustomer" {
		t.Errorf("TypeNames = %v, want the refused declaration — an error that cannot "+
			"say what it refused for is half a diagnosis", typed.TypeNames)
	}
	// The SQLSTATE must survive too: this is an API boundary, and a caller
	// matching on the code is as legitimate as one matching on the type.
	var apiErr *api.Error
	if !errors.As(wrapped, &apiErr) || apiErr.Code != api.ErrCodeUnsupportedOperation {
		t.Errorf("the relational error code was lost while wrapping: %v", wrapped)
	}
}

// AN AMBIGUOUS SCHEMA MUST ALSO COST NO READ.
//
// The ambiguity gate's own comment claimed it was decided "before any read",
// and that was true of decideStatistics and FALSE of evaluateCollectedStatistics,
// which short-circuited only on synthetic types. So an ambiguous schema paid an
// FDB transaction on every opt-in plan-cache miss and threw the answer away.
//
// A comment describing the GATE reads as a claim about the PATH through it, and
// this is the second time in this feature that gap has been real. The nil
// database is what makes the difference observable rather than merely slower.
func TestAmbiguousVerdictTouchesNoIO(t *testing.T) {
	t.Parallel()

	md := ambiguousTestMetaData(t)
	pair, ambiguous := md.AmbiguousDeclaredNames()
	if !ambiguous {
		t.Fatalf("fixture declares no colliding pair; this cannot reach the gate (%v)",
			md.RecordTypes())
	}

	c := &EmbeddedConnection{sess: &session.Session{Schema: "S", DBPath: "/db"}}

	var st StatisticsStatus
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("evaluateCollectedStatistics reached the read path for ambiguous "+
					"metadata and panicked on the nil session Keyspace (%v). The verdict is fixed "+
					"before any I/O, so the early return must precede statisticsLocation.", r)
			}
		}()
		st = evaluateCollectedStatistics(context.Background(), c, md)
	}()

	if st.Refusal != StatisticsAmbiguousNames {
		t.Fatalf("Refusal = %q, want %q", st.Refusal, StatisticsAmbiguousNames)
	}
	if len(st.AmbiguousTypes) != 2 {
		t.Errorf("AmbiguousTypes = %v, want the pair %v", st.AmbiguousTypes, pair)
	}
}

// ambiguousTestMetaData builds metadata declaring two record types whose names
// collide across the SQL and storage namespaces.
//
// MY$TABLE is stored as MY__1TABLE, and a table whose SQL name IS MY__1TABLE is
// stored as MY__01TABLE — so declaring both storage names is the collision. The
// demo proto has no such pair, and SKIPPING when a fixture cannot express the
// condition would leave the arm undriven while reporting green, so it is built.
//
// Renaming a record type means renaming three things, not one: the message, the
// RecordTypes entry, and the UNION's field — both its name and its FULLY
// QUALIFIED type reference. Missing the qualification silently matches nothing
// and leaves the references dangling, and the types then stop resolving at all.
func ambiguousTestMetaData(t *testing.T) *recordlayer.RecordMetaData {
	t.Helper()
	p, err := syntheticTestMetaData(t).ToProto()
	if err != nil {
		t.Fatalf("to proto: %v", err)
	}
	// No joined types: the synthetic gate runs first and would mask this one.
	p.JoinedRecordTypes = nil
	rename := map[string]string{"Order": "MY__1TABLE", "Customer": "MY__01TABLE"}
	for _, msg := range p.GetRecords().GetMessageType() {
		if to, ok := rename[msg.GetName()]; ok {
			msg.Name = proto.String(to)
		}
		for _, f := range msg.GetField() {
			full := strings.TrimPrefix(f.GetTypeName(), ".")
			short, pkgPrefix := full, ""
			if i := strings.LastIndex(full, "."); i >= 0 {
				short, pkgPrefix = full[i+1:], full[:i+1]
			}
			if to, ok := rename[short]; ok {
				f.TypeName = proto.String("." + pkgPrefix + to)
				if strings.HasPrefix(f.GetName(), "_") {
					f.Name = proto.String("_" + to)
				}
			}
		}
	}
	for _, rt := range p.GetRecordTypes() {
		if to, ok := rename[rt.GetName()]; ok {
			rt.Name = proto.String(to)
		}
	}
	md, err := recordlayer.RecordMetaDataFromProto(p)
	if err != nil {
		t.Fatalf("from proto: %v", err)
	}
	return md
}
