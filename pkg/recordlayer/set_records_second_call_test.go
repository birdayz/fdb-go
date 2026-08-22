package recordlayer

import (
	"fmt"
	"strings"
	"testing"

	"fdb.dev/gen"
)

// A SECOND SetRecords IS REFUSED, matching Java, which is ONE of several routes
// to an orphaned index and not the whole family.
//
// Java forbids the second setter: of its four `setRecords` overloads
// (RecordMetaDataBuilder.java:371, :382, :410, :421) the two that do the work
// (:384, :423) open with
// `if (recordsDescriptor != null) throw new MetaDataException("Records already
// set.")`, and the other two delegate to them. Go used to permit the call. The
// second call replaced every RecordType the new descriptor ALSO declared with a
// fresh one whose index slices were nil while `b.indexes` kept its entry, so an
// index came back registered and associated with nothing: Build succeeded,
// `RecordTypesForIndex` came back empty, `GetIndexesForRecordType` lost it, and
// `ToProto` emitted the index with an EMPTY RecordType list -- which a reload
// reads as UNIVERSAL, so the index returned either maintained for every record
// type or, when its key is not valid on all of them, as metadata that will not
// load at all.
//
// AN EARLIER VERSION OF THIS HEADER CALLED THAT "the only route". It was not:
// the builder hands out live maps, so a record type can be deleted after its
// index is registered, and an association slice can be replaced with a
// different *Index of the same name. Those are refused by an invariant in Build
// rather than by another guard here -- see TestBuildRefusesAnIndexNoRecordType
// Claims and TestBuildRefusesAnIndexAssociatedAsADifferentObject -- because
// enumerating routes is what produced the wrong claim in the first place.
//
// EVERY REGISTRATION SPELLING IS DRIVEN, and each carries its own control that
// builds WITHOUT the second call. The control is what distinguishes "the guard
// refused" from "this registration never associated anything in the first
// place" -- with a single shared control over one spelling, a registration that
// silently did nothing passed every other arm.
func TestSetRecordsRefusesASecondCall(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		register func(b *RecordMetaDataBuilder, idx *Index)
		// key is the index key. A universal index is validated against EVERY
		// record type, so that arm cannot use a field the demo proto declares
		// on only some of them.
		key KeyExpression
		// associated is how many record types the index is associated with when
		// the builder is used correctly, i.e. with one SetRecords call.
		associated int
	}{
		{
			name:       "AddIndex",
			register:   func(b *RecordMetaDataBuilder, idx *Index) { b.AddIndex("Order", idx) },
			key:        Field("price"),
			associated: 1,
		},
		{
			// Exactly ONE name RETURNS b.AddIndex(recordTypeNames[0], index), so
			// this is the arm above wearing a different call. It is driven
			// anyway: a spelling left to be INFERRED from a delegation is a
			// characterisation, and two successive characterisations of this
			// route were both wrong.
			name: "AddMultiTypeIndex with exactly one name",
			register: func(b *RecordMetaDataBuilder, idx *Index) {
				b.AddMultiTypeIndex([]string{"Order"}, idx)
			},
			key:        Field("price"),
			associated: 1,
		},
		{
			name: "AddMultiTypeIndex with two names",
			register: func(b *RecordMetaDataBuilder, idx *Index) {
				b.AddMultiTypeIndex([]string{"Order", "Customer"}, idx)
			},
			key:        Field("price"),
			associated: 2,
		},
		{
			// A nil or empty name list delegates to AddUniversalIndex, whose
			// registry hangs off the BUILDER rather than any RecordType. It was
			// the one spelling the old overwrite could not reach, and it is
			// driven here so the guard is shown to cover it too.
			//
			// EmptyKey, not a field: an early version of this arm used
			// Field("id"), which only TypedRecord declares, so Build refused for
			// a reason unrelated to the claim -- a fact about the KEY chosen,
			// not about the proto, which does carry a field on all three
			// (price).
			name: "AddMultiTypeIndex with an empty name list",
			register: func(b *RecordMetaDataBuilder, idx *Index) {
				b.AddMultiTypeIndex(nil, idx)
			},
			key:        EmptyKey(),
			associated: 3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// The control: one SetRecords call, so the association must EXIST.
			// Without it, an arm cannot tell the guard's refusal from a
			// registration that associated nothing to begin with.
			ctlIdx := NewIndex("assoc_idx", tc.key)
			ctlMD, err := newDemoBuilder(tc.register, ctlIdx, false).Build()
			if err != nil {
				t.Fatalf("CONTROL Build: %v", err)
			}
			if got := len(ctlMD.RecordTypesForIndex(ctlIdx)); got != tc.associated {
				t.Fatalf("CONTROL: RecordTypesForIndex = %d types, want %d.\n"+
					"This registration associated nothing, so the refusal asserted below\n"+
					"would hold whatever the guard did.", got, tc.associated)
			}

			// The same registration, then a SECOND SetRecords over the SAME
			// descriptor. Java throws here; Go records a build error.
			idx := NewIndex("assoc_idx", tc.key)
			md, err := newDemoBuilder(tc.register, idx, true).Build()
			if err == nil {
				t.Fatalf("Build SUCCEEDED after a second SetRecords — the guard is gone, and\n"+
					"the metadata it produced is exactly the state the guard exists to prevent:\n"+
					"  RecordTypesForIndex          = %d (want %d)\n"+
					"  GetIndexesForRecordType(Order) contains it = %v\n"+
					"  GetIndex keeps the flat-registry entry     = %v\n"+
					"An index registered and associated with nothing also SERIALIZES that way,\n"+
					"and a reload reads an empty RecordType list as UNIVERSAL.",
					len(md.RecordTypesForIndex(idx)), tc.associated,
					indexNamed(md.GetIndexesForRecordType("Order"), "assoc_idx"),
					md.GetIndex("assoc_idx") != nil)
			}
			if !strings.Contains(err.Error(), "Records already set.") {
				t.Errorf("Build failed with %q, want it to carry Java's wording "+
					"%q — the message is shared with Java deliberately", err, "Records already set.")
			}
		})
	}
}

// AND THE SECOND DESCRIPTOR IS NOT APPLIED, which is what makes the refusal
// safe for a caller that ignores the Build error. Java throws before touching
// any state; Go must reach the same place by returning before the assignment,
// not by recording an error after it. Nothing else pins that ordering: a guard
// written one line lower would still fail Build while leaving the builder
// holding a half-merged descriptor.
func TestRefusedSetRecordsLeavesTheFirstDescriptorInPlace(t *testing.T) {
	t.Parallel()

	idx := NewIndex("first_desc_idx", Field("price"))
	b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	setDemoPrimaryKeys(b)
	b.AddIndex("Order", idx)

	before := b.GetRecordTypes()["Order"]
	if before == nil {
		t.Fatal("setup: Order missing after the first SetRecords")
	}
	if len(before.indexes) != 1 {
		t.Fatalf("setup: Order carries %d indexes, want 1", len(before.indexes))
	}

	b.SetRecords(gen.File_record_layer_demo_proto)

	after := b.GetRecordTypes()["Order"]
	if after != before {
		t.Error("the refused call REPLACED the RecordType. The guard must return before " +
			"assigning, the way Java throws before touching state; a caller that ignores " +
			"the Build error would otherwise hold a builder whose associations are gone.")
	}
	if len(after.indexes) != 1 {
		t.Errorf("Order carries %d indexes after the refused call, want 1 — the refusal "+
			"discarded the association it was added to protect", len(after.indexes))
	}
}

// THE EMPTY RecordType LIST IS JAVA'S UNIVERSAL ENCODING, not damage. It is
// pinned here because the guard above is justified by what that encoding does
// on reload, and a reader who meets only the justification could conclude the
// reload behaviour is the bug. It is not: `RecordMetaDataFromProto` maps an
// empty list to universal, and Java's proto loop reaches the same place by
// calling `addMultiTypeIndex` with an empty list, which routes to
// `universalIndexes`. Round-tripping a genuinely universal index must therefore
// preserve it, and does.
func TestUniversalIndexRoundTripsThroughAnEmptyRecordTypeList(t *testing.T) {
	t.Parallel()

	idx := NewIndex("universal_roundtrip", EmptyKey())
	md, err := newDemoBuilder(
		func(b *RecordMetaDataBuilder, i *Index) { b.AddUniversalIndex(i) }, idx, false).Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if n := len(md.GetUniversalIndexes()); n != 1 {
		t.Fatalf("setup: %d universal indexes, want 1", n)
	}

	proto, err := md.ToProto()
	if err != nil {
		t.Fatalf("ToProto: %v", err)
	}
	var emitted bool
	for _, ip := range proto.Indexes {
		if ip.GetName() != "universal_roundtrip" {
			continue
		}
		emitted = true
		if len(ip.RecordType) != 0 {
			t.Errorf("the universal index emitted %d record types, want 0 — an empty list "+
				"IS the encoding, and naming types here would change what Java reads",
				len(ip.RecordType))
		}
	}
	if !emitted {
		t.Fatal("the universal index was not emitted at all")
	}

	reloaded, err := RecordMetaDataFromProto(proto)
	if err != nil {
		t.Fatalf("RecordMetaDataFromProto: %v", err)
	}
	if !indexNamed(reloaded.GetUniversalIndexes(), "universal_roundtrip") {
		t.Error("the reloaded index is not universal; the empty-list encoding no longer " +
			"round-trips, which is a wire-compatibility break with Java")
	}
	back := reloaded.GetIndex("universal_roundtrip")
	if back == nil {
		t.Fatal("the index did not survive the round trip")
	}
	if got := len(reloaded.RecordTypesForIndex(back)); got != 3 {
		t.Errorf("RecordTypesForIndex = %d after the round trip, want 3 (a universal index "+
			"applies to every type)", got)
	}
}

// newDemoBuilder builds the three-type demo schema, runs one registration
// spelling, and optionally makes the SECOND SetRecords call the guard refuses.
//
// The primary keys are re-applied AFTER that second call, and the re-application
// is deliberate even though the guard makes it a no-op today. Without it, a
// mutation that removes the guard makes an unguarded second SetRecords replace
// every RecordType, so Build fails on a MISSING PRIMARY KEY before it ever
// reaches the state the arms are about -- and their assertions would then be
// unreachable code describing something nothing produced.
func newDemoBuilder(register func(*RecordMetaDataBuilder, *Index), idx *Index, secondSetRecords bool) *RecordMetaDataBuilder {
	b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	setDemoPrimaryKeys(b)
	register(b, idx)
	if secondSetRecords {
		b.SetRecords(gen.File_record_layer_demo_proto)
		// Re-apply the primary keys an UNGUARDED second call would have dropped.
		// With the guard this is a no-op, and that is the point: without it, an
		// unguarded second SetRecords replaces every RecordType, Build fails on a
		// missing primary key, and the arm below never reaches the orphan it is
		// about -- its err==nil branch becomes unreachable and its failure text
		// becomes a claim about a state nothing produced.
		setDemoPrimaryKeys(b)
	}
	return b
}

func setDemoPrimaryKeys(b *RecordMetaDataBuilder) {
	b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
}

func indexNamed(indexes []*Index, name string) bool {
	for _, i := range indexes {
		if i != nil && i.Name == name {
			return true
		}
	}
	return false
}

// BUILD REFUSES AN INDEX NO RECORD TYPE CLAIMS, which is what makes the orphan
// state unreachable BY CONSTRUCTION rather than by an enumeration of routes.
//
// Refusing a second SetRecords closes the route Java closes, and an earlier
// revision of RFC-238 §7f concluded from that the state was closed. It was not:
// this builder hands out LIVE maps, so deleting a record type after registering
// an index against it reaches the same state with one SetRecords call and no
// error at all. Java never had to defend this — it has no getRecordTypes on the
// builder. Enumerating routes is how the previous claim went wrong, so the
// property is checked in Build instead: the index registry and the record-type
// associations must agree in BOTH directions. What that does NOT constrain is
// an index's own fields, which stay shared with the builder after Build.
func TestBuildRefusesAnIndexNoRecordTypeClaims(t *testing.T) {
	t.Parallel()

	newBuilderWithOrderIndex := func() (*RecordMetaDataBuilder, *Index) {
		idx := NewIndex("orphan_by_delete", Field("price"))
		b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		setDemoPrimaryKeys(b)
		b.AddIndex("Order", idx)
		return b, idx
	}

	// The control: the same registration, nothing deleted. Without it, an
	// implementation that refused EVERY index would satisfy the arm below.
	ctl, ctlIdx := newBuilderWithOrderIndex()
	ctlMD, err := ctl.Build()
	if err != nil {
		t.Fatalf("CONTROL Build: %v", err)
	}
	if got := len(ctlMD.RecordTypesForIndex(ctlIdx)); got != 1 {
		t.Fatalf("CONTROL: RecordTypesForIndex = %d, want 1", got)
	}

	// The live-map route: GetRecordTypes returns the builder's own map.
	b, _ := newBuilderWithOrderIndex()
	delete(b.GetRecordTypes(), "Order")
	md, err := b.Build()
	if err == nil {
		t.Fatalf("Build SUCCEEDED with an index no record type claims.\n"+
			"RecordTypesForIndex = %d, GetIndex keeps the registry entry = %v.\n"+
			"That metadata serializes with an EMPTY RecordType list, which a reload\n"+
			"reads as UNIVERSAL — the index comes back maintained for every type, or\n"+
			"refuses to load when its key is not valid on all of them.",
			len(md.RecordTypesForIndex(md.GetIndex("orphan_by_delete"))),
			md.GetIndex("orphan_by_delete") != nil)
	}
	if !strings.Contains(err.Error(), "orphan_by_delete") {
		t.Errorf("Build failed with %q, which does not name the offending index", err)
	}

	// A UNIVERSAL index is registered and claimed by no record type BY DESIGN.
	// The check must not reject it — without this arm, a check that refused
	// every unassociated index would pass everything above.
	ub := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	setDemoPrimaryKeys(ub)
	uIdx := NewIndex("universal_ok", EmptyKey())
	ub.AddUniversalIndex(uIdx)
	if _, err := ub.Build(); err != nil {
		t.Errorf("Build refused a UNIVERSAL index: %v — universal indexes belong to no "+
			"record type by design, and the association check must exempt them", err)
	}
}

// THE ASSOCIATION INVARIANT OUTLIVES THE CALL THAT ESTABLISHED IT.
//
// Build checks that every registered index is universal or claimed, but that
// check runs ONCE. If the built metadata shares its RecordType pointers with
// the builder — which it did — then any later builder mutation walks straight
// through the invariant: RemoveIndex strips the association from the object md
// holds, md.indexes keeps its own copied entry, and md.ToProto() emits the
// index with an EMPTY RecordType list. A reload reads that as UNIVERSAL, so an
// index declared for one record type comes back maintained for all of them.
//
// The wire assertion is the point of this test. Checking only
// RecordTypesForIndex would pass on a metadata whose SERIALIZED form is already
// wrong, and the serialized form is what Java reads.
func TestBuiltMetadataIsDetachedFromTheBuilder(t *testing.T) {
	t.Parallel()

	idx := NewIndex("detach_idx", Field("price"))
	b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	setDemoPrimaryKeys(b)
	b.AddIndex("Order", idx)
	md, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := len(md.RecordTypesForIndex(idx)); got != 1 {
		t.Fatalf("setup: RecordTypesForIndex = %d, want 1", got)
	}

	// The mutation. Legal on the builder, and nothing about it touches md.
	b.RemoveIndex("detach_idx")

	if got := len(md.RecordTypesForIndex(idx)); got != 1 {
		t.Errorf("after b.RemoveIndex, md.RecordTypesForIndex = %d, want 1 — the built "+
			"metadata still shares mutable state with the builder, so the association "+
			"check in Build guarantees nothing once Build has returned", got)
	}

	proto, err := md.ToProto()
	if err != nil {
		t.Fatalf("ToProto: %v", err)
	}
	var seen bool
	for _, ip := range proto.Indexes {
		if ip.GetName() != "detach_idx" {
			continue
		}
		seen = true
		if len(ip.RecordType) != 1 || ip.RecordType[0] != "Order" {
			t.Errorf("serialized RecordType list = %v, want [Order]. An EMPTY list here is "+
				"Java's encoding for a UNIVERSAL index, so this metadata reloads with the "+
				"index maintained for every record type — on both engines.", ip.RecordType)
		}
	}
	if !seen {
		t.Error("the index was not emitted at all")
	}
}

// A SAME-NAME REPLACEMENT IS NOT AN ASSOCIATION. Comparing the registry to the
// record types by NAME accepts a record type whose association slice holds a
// DIFFERENT *Index carrying the same name — and then b.indexes["x"] and the
// record type describe one index two ways. The write path takes its definition
// from GetIndexesForRecordType, scans and planning take theirs from GetIndex,
// so a single subspace is written and read under different key expressions.
//
// This is reachable for the same reason the orphan routes are: GetRecordTypes
// hands out the live map, so the slice can be replaced in place.
func TestBuildRefusesAnIndexAssociatedAsADifferentObject(t *testing.T) {
	t.Parallel()

	registered := NewIndex("divergent_idx", Field("price"))
	b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	setDemoPrimaryKeys(b)
	b.AddIndex("Order", registered)

	// The control: as registered, this builds.
	if _, err := b.Build(); err != nil {
		t.Fatalf("CONTROL Build: %v", err)
	}

	// Same NAME, different OBJECT, different key expression.
	replacement := NewIndex("divergent_idx", Field("quantity"))
	b.GetRecordTypes()["Order"].indexes = []*Index{replacement}

	md, err := b.Build()
	if err == nil {
		t.Fatalf("Build SUCCEEDED with two definitions of %q.\n"+
			"GetIndex gives the key expression %v and GetIndexesForRecordType gives %v; "+
			"one subspace would be written under one and scanned under the other.",
			"divergent_idx",
			md.GetIndex("divergent_idx").RootExpression,
			md.GetIndexesForRecordType("Order")[0].RootExpression)
	}
	if !strings.Contains(err.Error(), "divergent_idx") {
		t.Errorf("Build failed with %q, which does not name the offending index", err)
	}
	if !strings.Contains(err.Error(), "different one") {
		t.Errorf("Build failed with %q — it should distinguish a DIVERGENT association "+
			"from a missing one, because the two have different causes and different fixes", err)
	}
}

// THE REGISTRY AND THE ASSOCIATIONS MUST BE A BIJECTION, and each half of that
// is a defect the other half does not catch.
//
// The first version of this check compared NAMES and refused arm one below. It
// was then changed to compare OBJECTS, which fixed a divergent-definition hole
// and simultaneously REOPENED arm one: `Index.Name` is exported, so a caller
// who renames the object it just registered leaves b.indexes keyed "alpha"
// holding an index called "beta". buildIndexRecordTypeMap keys by idx.Name
// while ToProto looks the list up by the MAP key, so the mismatch serializes as
// an EMPTY RecordType list — which a reload reads as UNIVERSAL. Neither
// comparison alone is sufficient; the invariant needs both directions.
func TestBuildRequiresRegistryAndAssociationsToAgree(t *testing.T) {
	t.Parallel()

	t.Run("a renamed index no longer matches its registry key", func(t *testing.T) {
		t.Parallel()
		idx := NewIndex("alpha", Field("price"))
		b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		setDemoPrimaryKeys(b)
		b.AddIndex("Order", idx)

		// Exported field, no accessor gymnastics: the caller still holds the
		// object it passed in.
		idx.Name = "beta"

		md, err := b.Build()
		if err == nil {
			var serialized string
			if p, perr := md.ToProto(); perr == nil {
				for _, ip := range p.Indexes {
					serialized += fmt.Sprintf(" name=%q RecordType=%v", ip.GetName(), ip.RecordType)
				}
			}
			t.Fatalf("Build ACCEPTED an index whose Name no longer matches its registry key.\n"+
				"Serialized as:%s — an EMPTY RecordType list is the UNIVERSAL encoding, so this "+
				"metadata reloads with the index maintained for every record type.", serialized)
		}
		if !strings.Contains(err.Error(), "alpha") && !strings.Contains(err.Error(), "beta") {
			t.Errorf("Build failed with %q, which names neither the registry key nor the index", err)
		}
	})

	t.Run("a multi-type association replaced on one type only", func(t *testing.T) {
		t.Parallel()
		idx := NewIndex("mt_idx", Field("price"))
		b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		setDemoPrimaryKeys(b)
		b.AddMultiTypeIndex([]string{"Order", "Customer"}, idx)

		if _, err := b.Build(); err != nil {
			t.Fatalf("CONTROL Build: %v", err)
		}

		// Exported spelling: element assignment through the accessor. The
		// registered object is STILL associated via Customer, so a check that
		// only walks registry->associations sees it and stops.
		b.GetRecordTypes()["Order"].GetMultiTypeIndexes()[0] = NewIndex("mt_idx", Field("quantity"))

		md, err := b.Build()
		if err == nil {
			t.Fatalf("Build ACCEPTED two definitions of %q reached through the sibling type.\n"+
				"GetIndex gives %v and GetIndexesForRecordType(Order) gives %v; the write path "+
				"takes the second and scans take the first, so one subspace is written and read "+
				"under different key expressions.", "mt_idx",
				md.GetIndex("mt_idx").RootExpression,
				md.GetIndexesForRecordType("Order")[0].RootExpression)
		}
		if !strings.Contains(err.Error(), "mt_idx") {
			t.Errorf("Build failed with %q, which does not name the offending index", err)
		}
	})
}

// UNIVERSAL AND ASSOCIATED ARE MUTUALLY EXCLUSIVE, because the encoding cannot
// represent both and Java cannot construct it.
//
// One proto field carries the coverage: an index with an empty `recordType`
// list is universal, and a non-empty one names its types. So an index that is
// BOTH universal and associated with "Order" serializes as `recordType=[Order]`
// and the reload DROPS it from universalIndexes — a silent narrowing on the
// wire. In memory it is worse: the store maintains it once for being universal
// and once for the association, and for COUNT/SUM that is a double atomic ADD,
// which is not idempotent.
//
// Java reaches neither state: addUniversalIndex and addMultiTypeIndex write to
// disjoint places and nothing merges them. The exemption in Build's forward
// walk is what let it through here — a universal index skipped the association
// test before anything looked at whether it was ALSO associated.
func TestBuildRefusesAnIndexBothUniversalAndAssociated(t *testing.T) {
	t.Parallel()

	idx := NewIndex("both_idx", EmptyKey())
	b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	setDemoPrimaryKeys(b)
	b.AddUniversalIndex(idx)

	// The control: universal alone builds.
	if _, err := b.Build(); err != nil {
		t.Fatalf("CONTROL Build: %v", err)
	}

	// Now ALSO associate it. This writes an unexported field, which only this
	// package can do: addIndexCommon records "Index %s already defined" on a
	// second registration and Build drains buildErrors first, so the exported
	// path cannot reach the state. The check is defence in depth against a
	// builder-internal mistake, not against a caller.
	rt := b.GetRecordTypes()["Order"]
	rt.multiTypeIndexes = append(rt.multiTypeIndexes, idx)

	md, err := b.Build()
	if err == nil {
		p, perr := md.ToProto()
		var serialized string
		if perr == nil {
			for _, ip := range p.Indexes {
				if ip.GetName() == "both_idx" {
					serialized = fmt.Sprintf("recordType=%v", ip.RecordType)
				}
			}
		}
		t.Fatalf("Build ACCEPTED an index that is BOTH universal and associated.\n"+
			"It serializes as %s, so the reload drops it from GetUniversalIndexes — the\n"+
			"coverage NARROWS across a round trip. In memory it is maintained twice, and\n"+
			"for COUNT/SUM a double atomic ADD is not idempotent.", serialized)
	}
	if !strings.Contains(err.Error(), "both_idx") {
		t.Errorf("Build failed with %q, which does not name the offending index", err)
	}
}

// and the reload rekeys it. The message must say that rather than borrowing the
// type-specific one.
func TestBuildRefusesARenamedUniversalIndex(t *testing.T) {
	t.Parallel()

	idx := NewIndex("uni_alpha", EmptyKey())
	b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	setDemoPrimaryKeys(b)
	b.AddUniversalIndex(idx)
	idx.Name = "uni_beta"

	_, err := b.Build()
	if err == nil {
		t.Fatal("Build ACCEPTED a universal index whose Name no longer matches its registry key")
	}
	if strings.Contains(err.Error(), "universal index") && strings.Contains(err.Error(), "reloads as") {
		t.Errorf("Build failed with %q — that is the TYPE-SPECIFIC harm. For a universal "+
			"index an empty record-type list is correct; the harm is the registry key and "+
			"the index name disagreeing across a reload", err)
	}
}

// THE BUILT METADATA OWNS EVERYTHING IT HOLDS, driven through every public
// mutation route rather than argued about.

// A DUPLICATE ASSOCIATION IS ACCEPTED, MATCHING JAVA, and this arm exists
// because an earlier revision refused it and that would have broken loading.
//
// Java's addMultiTypeIndex appends per name with no dedup
// (RecordMetaDataBuilder.java:1174-1176), MetaDataValidator has no duplicate
// check, and loadFromProto preserves a repeated record-type name. So metadata
// Java WROTE contains this shape, and a Go build that refused it could not open
// that metadata -- the one line this port may not cross.
//
// The cost is real and shared: each copy is maintained separately on write, so
// for COUNT/SUM it is a double atomic ADD and those are not idempotent. That is
// an upstream behaviour to raise upstream, not one to diverge on unilaterally.
func TestBuildAcceptsADuplicateAssociationBecauseJavaDoes(t *testing.T) {
	t.Parallel()

	idx := NewIndex("dup_idx", Field("price"))
	b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	setDemoPrimaryKeys(b)
	b.AddMultiTypeIndex([]string{"Order", "Order"}, idx)

	md, err := b.Build()
	if err != nil {
		t.Fatalf("Build REFUSED a duplicate association: %v.\n"+
			"Java accepts it, so this refusal would reject metadata Java wrote.", err)
	}
	if got := len(md.GetIndexesForRecordType("Order")); got != 2 {
		t.Errorf("GetIndexesForRecordType(Order) = %d copies, want 2 — matching Java's "+
			"non-deduplicating append is the point of this arm", got)
	}
}

// GO SHARES ITS INDEX OBJECTS WITH THE BUILDER; JAVA CANNOT. These arms pin a
// DIVERGENCE, not a design.
//
// Java's Index has `private final` name, rootExpression and options with
// getters only, and RecordMetaDataBuilder passes its index maps straight into
// `new RecordMetaData(...)` — sharing is safe there because the fields cannot
// be rewritten. Go exports those fields, so the same sharing leaves post-Build
// mutation reachable here and impossible there.
//
// An earlier revision cloned the indexes to close that. It was the wrong layer:
// Java shares them, a shallow copy did not achieve isolation anyway (the
// KeyExpression graph exposes exported mutators and a []byte subspace key
// shares its backing array), and it split "the caller's index" from "the
// metadata's index" across 544 call sites that hand a pre-Build *Index to
// ScanIndex, RebuildIndex and SetIndex — degrading OnlineIndexer's containment
// check from "is the metadata's definition" to "shares a name with it".
//
// WHEN ENCAPSULATION LANDS THESE ARMS FAIL. That is the signal to delete them,
// not to relax them. DIVERGENCES.md carries the entry.
func TestBuiltMetadataSharesIndexObjectsWithTheBuilder(t *testing.T) {
	t.Parallel()

	idx := NewIndex("shared_idx", Field("price"))
	b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	setDemoPrimaryKeys(b)
	b.AddIndex("Order", idx)
	md, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if md.GetIndex("shared_idx") != idx {
		t.Fatal("the built metadata no longer holds the caller's *Index. If that is " +
			"deliberate, this divergence is closed and this test should be DELETED — but " +
			"check first that positions still reach the caller's object, because 544 call " +
			"sites scan with it and only the JVM conformance suite catches the difference.")
	}

	// The consequence, stated so nobody has to rediscover it: mutating the
	// caller's object reaches the built metadata.
	idx.Name = "renamed_after_build"
	if md.GetIndex("shared_idx").Name != "renamed_after_build" {
		t.Error("the rename did not reach the built metadata — the divergence may be " +
			"closed; see above before deleting")
	}
}
