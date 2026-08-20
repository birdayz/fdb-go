package fleet

import (
	"context"
	"strings"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"google.golang.org/protobuf/proto"
)

// A TENANT WHOSE STATISTICS CAN NEVER BE USED IS REFUSED, NOT FAILED.
//
// The reader rejects metadata declaring joined or unnested synthetic types
// outright: RecordTypes() omits them, so completeness is undecidable against the
// partial list it returns. The fan-out refuses before scanning, which would
// otherwise bill a full pass per tenant for a set the planner has already
// decided to reject.
//
// REFUSED rather than FAILED, and that distinction is the finding this pins.
// `failed` tells an operator to retry, and no retry changes a property of the
// metadata. It is easy to get wrong in one specific way: returning a non-nil
// error from the step makes fanOut stamp OutcomeFailed unconditionally, so the
// step must set the outcome itself and return nil. A previous revision did the
// former while its comment, its commit subject and RFC-236 all said REFUSED —
// three sites asserting an outcome the code could not produce, and no test.
//
// It reaches production code without a cluster: the relational SQL layer cannot
// construct synthetic metadata, so a driver-level test cannot get here at all.
func TestSyntheticRefusalIsRefusedNotFailed(t *testing.T) {
	t.Parallel()

	md := syntheticMetaData(t)
	if !md.DeclaresSyntheticRecordTypes() {
		t.Fatal("the fixture declares no synthetic types, so this test cannot reach the " +
			"refusal it exists to pin")
	}

	ev, refused := syntheticRefusal(md)
	if !refused {
		t.Fatal("metadata declaring synthetic types was not refused — the completeness " +
			"gate would then certify a schema from a type list that omits them")
	}
	if ev.Outcome != OutcomeRefused {
		t.Errorf("Outcome = %q, want %q. FAILED reads as a fault and tells an operator to "+
			"retry; REFUSED reads as a decision and counts separately in the summary.",
			ev.Outcome, OutcomeRefused)
	}
	if ev.Err == nil {
		t.Fatal("a refusal with no Err prints nothing: the refused arm renders only Err")
	}
	if !strings.Contains(ev.Err.Error(), "JoinedAB") {
		t.Errorf("the refusal must name the declarations that caused it, or it is a verdict "+
			"an operator cannot act on: %v", ev.Err)
	}

	// Control: ordinary metadata is not refused, or the arm above would fire for
	// every tenant and this test would pass while the feature was disabled.
	plainBuilder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	plainBuilder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	plainBuilder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	plainBuilder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	plain, err := plainBuilder.Build()
	if err != nil {
		t.Fatalf("build plain metadata: %v", err)
	}
	if _, refusedPlain := syntheticRefusal(plain); refusedPlain {
		t.Error("ordinary metadata was refused — the guard fires on every schema")
	}
}

// syntheticMetaData builds metadata carrying a joined-type declaration, which is
// what this port preserves opaquely and does not model.
func syntheticMetaData(t *testing.T) *recordlayer.RecordMetaData {
	t.Helper()
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	md, err := builder.Build()
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

// AND THROUGH fanOut, WHICH IS WHERE THE OUTCOME CAN STILL BE LOST.
//
// The test above checks syntheticRefusal's return value. That is the predicate,
// not the path: fanOut stamps `ev.Outcome = OutcomeFailed` whenever the step
// returns a non-nil error, so a caller that returns `ev, ev.Err` instead of
// `ev, nil` turns REFUSED into FAILED — the exact defect this pins — while the
// predicate test stays green.
//
// So this drives a step through fanOut and asserts the TALLY: Refused == 1 and
// Failed == 0. That is the number an operator reads in the summary, and it is
// the only place the distinction becomes observable.
func TestSyntheticRefusalSurvivesFanOut(t *testing.T) {
	t.Parallel()

	md := syntheticMetaData(t)
	targets := []Target{{DatabaseID: "/db", SchemaName: "S"}}

	// THE PRODUCTION STEP, not a stand-in of the same shape. The defect being
	// pinned lives in the caller's return statement — returning a non-nil error
	// alongside a REFUSED event makes fanOut stamp FAILED over it — and a
	// hand-written closure simply does not contain that statement, so it cannot
	// exhibit the bug. An earlier version of this test did exactly that and
	// stayed green under the real mutation.
	//
	// db/cat/ks are nil: the refusal is decided from metadata before any of them
	// is touched, so a nil is safe here and would announce itself loudly if the
	// guard ever moved below the store open.
	step := collectStatisticsStep(nil, nil, recordlayer.StatisticsSubspace{}, StatisticsOptions{},
		func(context.Context, Target) (*recordlayer.RecordMetaData, error) { return md, nil })
	res, err := fanOut(context.Background(), nil, targets, Options{}, step)

	// fanOut joins per-target errors, and a refusal carries one — so a non-nil
	// error here is expected and is what makes the CLI exit non-zero.
	if err == nil {
		t.Error("a refused target must still surface an error, or the fan-out exits 0")
	}
	if res.Refused != 1 {
		t.Errorf("Refused = %d, want 1. The summary is what an operator reads; a refusal "+
			"that does not land there is invisible.", res.Refused)
	}
	if res.Failed != 0 {
		t.Errorf("Failed = %d, want 0. FAILED tells an operator to retry, and no retry "+
			"changes a property of the metadata — that is the whole distinction.", res.Failed)
	}
	if res.Collected != 0 {
		t.Errorf("Collected = %d, want 0 — nothing was stored", res.Collected)
	}
}

// ambiguousMetaData builds metadata declaring two record types whose names
// collide across the SQL and storage namespaces.
//
// MY$TABLE is stored as MY__1TABLE; a table whose SQL name IS MY__1TABLE is
// stored as MY__01TABLE. Declaring both makes the unescaped lookup for
// MY__1TABLE hit the FIRST table's entry.
func ambiguousMetaData(t *testing.T) *recordlayer.RecordMetaData {
	t.Helper()
	base := syntheticMetaData(t)
	p, err := base.ToProto()
	if err != nil {
		t.Fatalf("to proto: %v", err)
	}
	// Drop the joined declaration, or the SYNTHETIC refusal fires first and this
	// fixture would pin that instead — the arms are ordered, so the fixture has
	// to isolate the one it names.
	p.JoinedRecordTypes = nil

	// RENAME two record types so their names actually collide. The demo proto has
	// no colliding pair, and SKIPPING when the fixture cannot express the
	// condition would leave the fleet arm undriven while reporting green -- the
	// shape this PR has already found four times. Build the condition instead.
	//
	// MY$TABLE is stored as MY__1TABLE and a table whose SQL name IS MY__1TABLE
	// is stored as MY__01TABLE, so declaring both storage names is exactly the
	// collision: an unescaped lookup for MY__1TABLE hits the first entry.
	rename := map[string]string{"Order": "MY__1TABLE", "Customer": "MY__01TABLE"}
	for _, msg := range p.GetRecords().GetMessageType() {
		if to, ok := rename[msg.GetName()]; ok {
			msg.Name = proto.String(to)
		}
	}
	for _, rt := range p.GetRecordTypes() {
		if to, ok := rename[rt.GetName()]; ok {
			rt.Name = proto.String(to)
		}
	}
	// The UNION message references each record type by TYPE NAME, so renaming the
	// messages alone unlinks them and they stop being record types at all --
	// which the fixture guard below caught, reporting one surviving type instead
	// of the collision. Rewrite the references too.
	for _, msg := range p.GetRecords().GetMessageType() {
		for _, f := range msg.GetField() {
			// TypeName is FULLY QUALIFIED (.pkg.Order), so matching a short-name
			// map against it silently matched nothing and left the references
			// dangling -- the messages renamed, the union still pointed at the old
			// names, and the types stopped resolving. Match the last segment and
			// preserve the package.
			full := strings.TrimPrefix(f.GetTypeName(), ".")
			short := full
			pkgPrefix := ""
			if i := strings.LastIndex(full, "."); i >= 0 {
				short, pkgPrefix = full[i+1:], full[:i+1]
			}
			if to, ok := rename[short]; ok {
				f.TypeName = proto.String("." + pkgPrefix + to)
				// The union FIELD NAME carries the record type by convention
				// (_Order for Order), and resolution goes through it -- renaming
				// only the message and the type reference left the types
				// unresolvable, which the guard reported as one surviving type.
				if strings.HasPrefix(f.GetName(), "_") {
					f.Name = proto.String("_" + to)
				}
			}
		}
	}
	md, err := recordlayer.RecordMetaDataFromProto(p)
	if err != nil {
		t.Fatalf("from proto: %v", err)
	}
	if md.DeclaresSyntheticRecordTypes() {
		t.Fatal("the fixture still declares synthetic types, so the synthetic gate would " +
			"fire before the ambiguity one and this test would pin the wrong arm")
	}
	return md
}

// AN AMBIGUOUS SCHEMA MUST BE REFUSED BY THE FLEET PATH TOO.
//
// The single-schema collector refuses a colliding schema before scanning. This
// path checked only synthetic types, so `stats collect --all-schemas` scanned
// the whole store and reported OutcomeCollected for a set the shared reader
// always refuses — one rule enforced at one entry point and not its sibling.
//
// Driven through fanOut and asserted on the TALLY, for the same reason the
// synthetic version is: the distinction between REFUSED and FAILED only becomes
// observable in the summary an operator reads, and a hand-written closure cannot
// exhibit a defect that lives in the production caller's return statement.
func TestAmbiguousRefusalSurvivesFanOut(t *testing.T) {
	t.Parallel()

	md := ambiguousMetaData(t)
	pair, ambiguous := md.AmbiguousDeclaredNames()
	if !ambiguous {
		t.Fatalf("the fixture does not declare a colliding pair, so this test cannot "+
			"reach the refusal it exists to pin (declared: %v)", md.RecordTypes())
	}
	if len(pair) != 2 {
		t.Fatalf("AmbiguousDeclaredNames returned %v, want a pair", pair)
	}

	step := collectStatisticsStep(nil, nil, recordlayer.StatisticsSubspace{}, StatisticsOptions{},
		func(context.Context, Target) (*recordlayer.RecordMetaData, error) { return md, nil })
	res, err := fanOut(context.Background(), nil,
		[]Target{{DatabaseID: "/db", SchemaName: "S"}}, Options{}, step)

	if err == nil {
		t.Error("a refused target must still surface an error, or the fan-out exits 0")
	}
	if res.Refused != 1 {
		t.Errorf("Refused = %d, want 1 — an ambiguous schema must be refused by the "+
			"fleet path as it is by the single-schema one", res.Refused)
	}
	if res.Collected != 0 {
		t.Errorf("Collected = %d, want 0 — nothing may be stored for a schema whose "+
			"set the reader always refuses", res.Collected)
	}
}

// THE FAN-OUT'S SKIPPED TEXT NAMES TABLES THE OPERATOR HAS.
//
// describeSkipped renders record-type keys, which are STORAGE names, into the
// error a refused or failed target prints — and the caller's own comment calls
// that "the one field an operator actually sees". It is unexported, so no other
// package can cover it: decoding it in the same change as the CLI's copy and
// pinning only the CLI's is the two-consumers-one-pinned shape one level down.
func TestDescribeSkippedNamesUserIdentifiers(t *testing.T) {
	t.Parallel()

	const storage, sql = "MY__1TABLE", "MY$TABLE"
	if recordlayer.ToUserIdentifier(storage) != sql {
		t.Fatalf("fixture is wrong: %q does not decode to %q", storage, sql)
	}

	got := describeSkipped(map[string]string{storage: "exceeds MaxRecordsPerType"})
	if !strings.Contains(got, sql) {
		t.Errorf("describeSkipped does not name the table by its SQL identifier: %s", got)
	}
	if strings.Contains(got, storage) {
		t.Errorf("describeSkipped leaks the storage name: %s", got)
	}

	// Ordering is by the USER name, since that is what a reader scans. Two
	// entries make the sort observable; one cannot.
	two := describeSkipped(map[string]string{
		"ZZ__1TABLE": "second",
		storage:      "first",
	})
	if !strings.HasPrefix(two, sql) {
		t.Errorf("entries are not sorted by the name actually printed: %s", two)
	}
}
