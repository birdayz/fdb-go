package recordlayer

import (
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
)

// THE MAX-SUPPORTED CEILING AND THE DEFAULT TARGET ARE TWO DIFFERENT NUMBERS.
//
// Java keeps them apart deliberately: getMaximumSupportedVersion()
// (FormatVersion.java:203) answers "what can this binary OPEN", and
// getDefaultFormatVersion() (`:215-217`) answers "what is a new store BORN at".
// Go had them fused into one constant, `formatVersionCurrent`, which served BOTH
// the validation bound (store_builder.go:185, :1338 rejecting `stored > current`)
// and the creation/upgrade target (`:199`, `:1005`, `:1200`).
//
// The fusion was not a naming problem, it was a correctness trap: lowering the
// default to match Java's 7 would have simultaneously lowered the CEILING, and
// every store this binary had already created at 14 would then have been
// REJECTED on open with UnsupportedFormatVersionError. That is why "just change
// the default to match Java" was a change that could not be made safely, and why
// DIVERGENCES.md's claim that the remaining work was "a decision about the
// DEFAULT, not a missing capability" was wrong — the missing capability was one
// level down, in the constant.
//
// These pin the separation. They are the reason the default is now a free
// decision rather than one blocked by its own ceiling.

func headerAt(v int32) *gen.DataStoreInfo {
	return &gen.DataStoreInfo{FormatVersion: proto.Int32(v)}
}

// The CEILING is what validation uses. A stored version at the ceiling opens; one
// above it does not.
func TestFormatVersion_ValidationUsesTheCeiling(t *testing.T) {
	t.Parallel()
	store := &FDBRecordStore{}

	if err := store.validateFormatVersion(headerAt(int32(formatVersionMaxSupported))); err != nil {
		t.Fatalf("a header at the max supported version was REJECTED: %v.\n"+
			"  Every store this binary creates by default is written at the default, and\n"+
			"  the default is at or below the ceiling — so rejecting the ceiling rejects\n"+
			"  stores this very binary wrote.", err)
	}
	err := store.validateFormatVersion(headerAt(int32(formatVersionMaxSupported) + 1))
	var unsupported *UnsupportedFormatVersionError
	if !errors.As(err, &unsupported) {
		t.Fatalf("a header ABOVE the max supported version was accepted (err=%v).\n"+
			"  Java throws UnsupportedFormatVersionException for candidate >\n"+
			"  getMaximumSupportedVersion() (FormatVersion.java:224-231); a Go store that\n"+
			"  proceeds is reading a layout it does not understand.", err)
	}
	if unsupported.MaxVersion != int32(formatVersionMaxSupported) {
		t.Fatalf("the error reports MaxVersion %d, want the CEILING %d — a caller told the "+
			"wrong bound cannot tell how far out of range it is",
			unsupported.MaxVersion, formatVersionMaxSupported)
	}
	if err := store.validateFormatVersion(headerAt(int32(formatVersionMinimum) - 1)); err == nil {
		t.Fatal("a header BELOW the minimum was accepted; Java rejects that end too")
	}
}

// THE LOAD-BEARING ONE: validation must NOT consult the default. A store opened
// with a target BELOW its header still opens, because the ceiling admits it and
// the upgrade path never downgrades.
//
// This is the case the fused constant made impossible to express, and it is the
// exact shape of "a Go binary whose default was lowered meets a store it created
// earlier at 14".
func TestFormatVersion_AStoreOpensAboveItsOwnTarget(t *testing.T) {
	t.Parallel()
	// Target pinned well below the header — the shape a rolling downgrade, or a
	// lowered default, produces.
	store := &FDBRecordStore{targetFormatVersion: int32(formatVersionCacheableState)}
	stored := headerAt(int32(formatVersionMaxSupported))

	if got := store.effectiveFormatVersion(); got != int32(formatVersionCacheableState) {
		t.Fatalf("effectiveFormatVersion = %d, want the pinned target %d", got, formatVersionCacheableState)
	}
	if err := store.validateFormatVersion(stored); err != nil {
		t.Fatalf("a store pinned at %d REJECTED a header at %d: %v.\n"+
			"  Validation must use the CEILING, never the target. If it uses the target,\n"+
			"  then lowering the default — or pinning any instance to an older format for a\n"+
			"  rolling upgrade, which is the entire purpose of SetFormatVersion — makes this\n"+
			"  binary unable to open stores it wrote itself.\n"+
			"  WHAT THIS RE-ARMS: the fused-constant trap. formatVersionMaxSupported and\n"+
			"  formatVersionDefault were one constant, and re-fusing them turns every\n"+
			"  existing store at the old ceiling into UnsupportedFormatVersionError.",
			formatVersionCacheableState, formatVersionMaxSupported, err)
	}
}

// The ordering constraint that makes the pair coherent, and the one that would
// break the interop argument if it were ever violated.
func TestFormatVersion_DefaultIsWithinTheCeiling(t *testing.T) {
	t.Parallel()
	if formatVersionDefault > formatVersionMaxSupported {
		t.Fatalf("the DEFAULT (%d) is above the MAX SUPPORTED (%d).\n"+
			"  A new store would be born at a version this same binary refuses to open.",
			formatVersionDefault, formatVersionMaxSupported)
	}
	if formatVersionDefault < formatVersionMinimum {
		t.Fatalf("the DEFAULT (%d) is below the MINIMUM (%d)",
			formatVersionDefault, formatVersionMinimum)
	}
	// The interop argument recorded in DIVERGENCES.md rests on this exact number:
	// Java 4.12.11.0's MAX_SUPPORTED_VERSION is FULL_STORE_LOCK(14)
	// (FormatVersion.java:173, :182), so a Go store written at 14 passes Java's
	// validateFormatVersion (14 <= 14) and checkPossiblyRebuild adopts it via
	// Math.max without writing anything back.
	if formatVersionDefault > formatVersionFullStoreLock {
		t.Fatalf("the DEFAULT (%d) is above Java 4.12.11.0's MAX_SUPPORTED_VERSION (%d).\n"+
			"  WHAT THIS RE-ARMS: the whole interop decision. Java's\n"+
			"  validateFormatVersion rejects candidate > MAX_SUPPORTED, so a Go store born\n"+
			"  above that number is a store the pinned Java reference CANNOT OPEN — which\n"+
			"  is the interop break DIVERGENCES.md concluded does not exist. Re-read that\n"+
			"  entry before raising this default.",
			formatVersionDefault, formatVersionFullStoreLock)
	}
}
