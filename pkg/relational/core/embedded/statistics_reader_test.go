package embedded

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/session"
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
	all := []StatisticsRefusal{
		StatisticsOK,
		StatisticsNotCollected,
		StatisticsReadFailed,
		StatisticsVersionUnavailable,
		StatisticsStampedInFuture,
		StatisticsExpired,
		StatisticsIncomplete,
		StatisticsEmptySchema,
		StatisticsSyntheticTypes,
	}
	covered := map[StatisticsRefusal]int{}
	for _, tc := range decideStatisticsCases() {
		covered[decideStatistics(tc.in).Refusal]++
	}
	for _, r := range all {
		if covered[r] == 0 {
			t.Errorf("no case in decideStatisticsCases produces refusal %q — add one", r)
		}
	}
	// And the reverse: a verdict the list does not name means a constant was
	// added without being registered here, which would silently shrink the
	// guard above to whatever it still happens to cover.
	named := map[StatisticsRefusal]bool{}
	for _, r := range all {
		named[r] = true
	}
	for r := range covered {
		if !named[r] {
			t.Errorf("cases produce refusal %q, which this guard's list does not name", r)
		}
	}
	if len(covered) != len(all) {
		t.Errorf("covered %d distinct refusals, want %d", len(covered), len(all))
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
				ReadErr:            errNoReadVersion,
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
