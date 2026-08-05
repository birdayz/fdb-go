package recordlayer

import (
	"errors"
	"testing"

	"fdb.dev/gen"
	"google.golang.org/protobuf/proto"
)

// TestRecordTypeKey_EmptyBytesSurvivesProtoRoundTrip pins that an EMPTY bytes
// key is a key, and stays one across export and reload.
//
// The copy in the canonicalizer was append([]byte(nil), k...), which returns a
// NIL slice for an empty input — and nil is how this field spells "absent".
// valueToProto then left bytes_value unset, so ToProto/FromProto dropped the
// key entirely and the type silently fell back to its union field number: a
// DIFFERENT key space, leaving every record written under the empty-bytes
// prefix unreachable.
//
// Java keeps it: setRecordTypeKey stores ByteString.copyFrom(new byte[0]),
// which is non-null, LiteralKeyExpression.toProtoValue calls setBytesValue on
// it, and fromProtoValue tests hasBytesValue() — field PRESENCE, not
// emptiness. The Go proto layer preserves presence the same way, which the
// byte-level assertions below state outright.
func TestRecordTypeKey_EmptyBytesSurvivesProtoRoundTrip(t *testing.T) {
	t.Parallel()

	// The proto-layer fact the fix depends on: a non-nil empty bytes value is
	// PRESENT on the wire (tag 6, length 0), a nil one is absent. If this ever
	// changes, an empty-bytes key cannot be represented at all and the
	// canonicalizer must reject it instead of storing it.
	present, err := proto.Marshal(&gen.Value{BytesValue: []byte{}})
	if err != nil {
		t.Fatalf("marshal empty bytes value: %v", err)
	}
	if len(present) == 0 {
		t.Fatal("a non-nil empty bytes_value marshaled to nothing — proto field presence no longer distinguishes " +
			"an empty bytes key from an absent one, so an empty-bytes record type key cannot round-trip")
	}
	absent, err := proto.Marshal(&gen.Value{BytesValue: nil})
	if err != nil {
		t.Fatalf("marshal nil bytes value: %v", err)
	}
	if len(absent) != 0 {
		t.Fatalf("a nil bytes_value marshaled to %x, want nothing", absent)
	}
	var decoded gen.Value
	if err := proto.Unmarshal(present, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.BytesValue == nil {
		t.Fatal("a present empty bytes_value decoded as nil — presence is lost through the round-trip")
	}

	// The canonical form of an empty bytes key must itself be non-nil, or it
	// serializes as absent.
	canonical, err := canonicalRecordTypeKey([]byte{})
	if err != nil {
		t.Fatalf("canonicalRecordTypeKey([]byte{}): %v", err)
	}
	// Errorf, not Fatalf: this is the CAUSE, and the end-to-end assertions
	// below are the CONSEQUENCE. Reporting both in one run says outright that
	// a nil canonical form is what makes the key vanish, rather than leaving
	// the reader to infer it from a test that stopped here.
	canonicalBytes, ok := canonical.([]byte)
	if !ok || canonicalBytes == nil {
		t.Errorf("empty bytes key canonicalized to %v (%T); a nil slice serializes as ABSENT, "+
			"so the key would vanish on export", canonical, canonical)
	}

	// End to end: build, export, reload, and the key is still there.
	md, err := keyMetaData(map[string]any{"Order": []byte{}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	rt := md.GetRecordType("Order")
	if !rt.HasExplicitRecordTypeKey() {
		t.Fatal("an empty bytes key did not register as an explicit record type key")
	}
	mdProto, err := md.ToProto()
	if err != nil {
		t.Fatalf("ToProto: %v", err)
	}
	back, err := RecordMetaDataFromProto(mdProto)
	if err != nil {
		t.Fatalf("RecordMetaDataFromProto: %v", err)
	}
	backRT := back.GetRecordType("Order")
	if !backRT.HasExplicitRecordTypeKey() {
		t.Fatal("the empty bytes record type key VANISHED across ToProto/FromProto — the type falls back to its " +
			"union field number, a different key space, and every record under the empty-bytes prefix is unreachable")
	}
	got, ok := backRT.GetRecordTypeKey().([]byte)
	if !ok || len(got) != 0 {
		t.Fatalf("empty bytes key came back as %v (%T), want an empty []byte", got, got)
	}
	// And it still addresses the same key space it did before the round-trip.
	beforeID, _ := recordTypeKeyIdentity(rt.GetRecordTypeKey())
	afterID, _ := recordTypeKeyIdentity(backRT.GetRecordTypeKey())
	if beforeID != afterID {
		t.Fatalf("empty bytes key changed key space across the round-trip: %x → %x", beforeID, afterID)
	}
}

// TestRecordTypeKey_NilBytesIsNoExplicitKey pins the other half: a NIL byte
// slice is Java's null byte[] reference, which its setter accepts as "no
// explicit key" (the `recordTypeKey == null` arm). Storing a typed-nil []byte
// instead made HasExplicitRecordTypeKey report true for a key that
// serialization then dropped — the same vanishing act, reached from a
// different input.
func TestRecordTypeKey_NilBytesIsNoExplicitKey(t *testing.T) {
	t.Parallel()

	canonical, err := canonicalRecordTypeKey([]byte(nil))
	if err != nil {
		t.Fatalf("canonicalRecordTypeKey([]byte(nil)): %v", err)
	}
	if canonical != nil {
		t.Fatalf("a nil byte slice canonicalized to %v (%T), want an untyped nil meaning "+
			"'no explicit key'; a typed nil reports as an explicit key that then vanishes on export",
			canonical, canonical)
	}

	md, err := keyMetaData(map[string]any{"Order": []byte(nil)})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	rt := md.GetRecordType("Order")
	if rt.HasExplicitRecordTypeKey() {
		t.Fatal("a nil byte slice registered as an EXPLICIT record type key; export drops it, so the type " +
			"would silently change key space on reload")
	}
	if got, want := rt.GetRecordTypeKey(), int64(rt.RecordTypeIndex); got != want {
		t.Fatalf("record type key with no explicit key = %v, want the union field number %v", got, want)
	}
}

// TestRecordTypeKey_LookupDistinguishesStringFromBytes pins that every
// comparison of record type keys agrees with Build about what a key IS.
//
// Build admits "k" and []byte("k") as two types: they carry different tuple
// type codes and occupy different key spaces. The lookup compared through the
// general subspace-key normalizer, which folds bytes into a string, so it
// matched BOTH types and returned whichever the map iteration reached first —
// a coin flip, per call, over which type a key resolves to.
func TestRecordTypeKey_LookupDistinguishesStringFromBytes(t *testing.T) {
	t.Parallel()

	md, err := keyMetaData(map[string]any{
		"Order":    "k",
		"Customer": []byte("k"),
	})
	if err != nil {
		t.Fatalf("Build rejected a string key and a bytes key with the same contents: %v", err)
	}

	// Repeated because the defect is map-iteration order: a single call could
	// return the right type by luck.
	for i := 0; i < 50; i++ {
		byString := md.GetRecordTypeFromRecordTypeKey("k")
		if byString == nil || byString.Name != "Order" {
			t.Fatalf("lookup by string key \"k\" returned %v, want Order — the string and bytes keys are "+
				"distinct types and the lookup is answering with whichever the map yields first", byString)
		}
		byBytes := md.GetRecordTypeFromRecordTypeKey([]byte("k"))
		if byBytes == nil || byBytes.Name != "Customer" {
			t.Fatalf("lookup by bytes key []byte(\"k\") returned %v, want Customer", byBytes)
		}
	}

	// Integer spellings still resolve to the same type, since they encode
	// identically — the lookup must not become type-literal.
	md2, err := keyMetaData(map[string]any{"Order": int64(77)})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, spelling := range []any{int64(77), 77, int32(77), uint(77), uint8(77)} {
		if rt := md2.GetRecordTypeFromRecordTypeKey(spelling); rt == nil || rt.Name != "Order" {
			t.Fatalf("lookup by %v (%T) returned %v, want Order — these spellings all encode to the same key",
				spelling, spelling, rt)
		}
	}
	// A key no type holds resolves to nothing, and a non-key value does not panic.
	if rt := md2.GetRecordTypeFromRecordTypeKey(int64(9999)); rt != nil {
		t.Fatalf("lookup by an unused key returned %v, want nil", rt.Name)
	}
	if rt := md2.GetRecordTypeFromRecordTypeKey(struct{ A int }{1}); rt != nil {
		t.Fatalf("lookup by a value that cannot be a record type key returned %v, want nil", rt.Name)
	}
}

// TestRecordTypeKey_EvolutionDistinguishesStringFromBytes pins the same
// identity in the evolution validator, where folding the two has a worse
// consequence than a bad lookup: a rename is inferred from keys MATCHING, so a
// folded comparison pairs an old type with a new type that lives in a
// different key space, carrying that type's records and indexes over to it.
func TestRecordTypeKey_EvolutionDistinguishesStringFromBytes(t *testing.T) {
	t.Parallel()

	// Same type, key changed from bytes to the string of the same bytes. That
	// moves every record of the type to a different key space, so it must be
	// refused — a folding comparison called the two keys equal and allowed it.
	oldMD, err := keyMetaData(map[string]any{"Order": []byte("k")})
	if err != nil {
		t.Fatalf("Build old: %v", err)
	}
	newMD, err := keyMetaData(map[string]any{"Order": "k"})
	if err != nil {
		t.Fatalf("Build new: %v", err)
	}
	newMD.version = oldMD.version + 1

	err = NewMetaDataEvolutionValidator().Build().Validate(oldMD, newMD)
	var evoErr *MetaDataEvolutionError
	if !errors.As(err, &evoErr) {
		t.Fatalf("changing a record type key from []byte(\"k\") to \"k\" was accepted as no change — "+
			"the two occupy different key spaces, so every record of that type becomes unreachable; got err=%v", err)
	}

	// The control: an unchanged key, spelled the same way, still validates.
	sameMD, err := keyMetaData(map[string]any{"Order": []byte("k")})
	if err != nil {
		t.Fatalf("Build same: %v", err)
	}
	sameMD.version = oldMD.version + 1
	if err := NewMetaDataEvolutionValidator().Build().Validate(oldMD, sameMD); err != nil {
		t.Fatalf("an unchanged bytes record type key was reported as changed: %v", err)
	}
}

// TestRecordTypeKey_RenameInferenceDistinguishesStringFromBytes pins the
// rename-INFERENCE site, which is the one with teeth: a type missing from the
// new metadata is matched to a new type by its key alone, so a folding
// comparison pairs the old []byte("k") type with a new "k" type — a different
// key space — and every index and record of the old type is carried over to
// it under the rename.
func TestRecordTypeKey_RenameInferenceDistinguishesStringFromBytes(t *testing.T) {
	t.Parallel()

	// Order carries a bytes key in old and the STRING of the same bytes in
	// new, where it has also been renamed away. The only same-key candidate
	// for old "Order" is therefore the new type whose key merely FOLDS to the
	// same value.
	build := func(orderKey any, renameOrder bool) *RecordMetaData {
		md, err := keyMetaData(map[string]any{"Order": orderKey, "Customer": int64(500)})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if renameOrder {
			rt := md.recordTypes["Order"]
			delete(md.recordTypes, "Order")
			rt.Name = "Renamed"
			md.recordTypes["Renamed"] = rt
		}
		return md
	}

	oldMD := build([]byte("k"), false)
	newMD := build("k", true)
	newMD.version = oldMD.version + 1

	renames, err := (&MetaDataEvolutionValidator{}).getTypeRenames(oldMD, newMD)
	if err != nil {
		t.Fatalf("getTypeRenames: %v", err)
	}
	if got := renames["Order"]; got == "Renamed" {
		t.Fatalf("old record type Order (key []byte(\"k\")) was inferred to have been renamed to %q "+
			"(key \"k\") — the two keys occupy different key spaces, so the rename would carry every "+
			"record and index of the old type into a key range it does not live in", got)
	}

	// The control: with the key genuinely unchanged, the rename IS inferred.
	renamedSameKey := build([]byte("k"), true)
	renamedSameKey.version = oldMD.version + 1
	renames, err = (&MetaDataEvolutionValidator{}).getTypeRenames(oldMD, renamedSameKey)
	if err != nil {
		t.Fatalf("getTypeRenames: %v", err)
	}
	if got := renames["Order"]; got != "Renamed" {
		t.Fatalf("a real rename with an unchanged key resolved to %q, want Renamed — "+
			"the identity comparison must not stop genuine renames from being detected", got)
	}
}
