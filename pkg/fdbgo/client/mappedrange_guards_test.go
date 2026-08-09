package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
	"github.com/onsi/gomega"
)

// The guards pinned here all run BEFORE any getMappedKeyValues RPC leaves the
// client, so they are exercised against a real transaction on a real cluster
// without needing the storage server to resolve a mapper. Their ORDER is as
// load-bearing as their existence: C++ checks the special-key space, the legal
// range and the limit all before it checks snapshot / RYW-disabled
// (ReadYourWrites.actor.cpp:1786-1833 then :1219-1233), so a call that is
// invalid for two reasons must report the earlier one.

func mappedFDBErrCode(t *testing.T, err error) int {
	t.Helper()
	var fe *wire.FDBError
	if !errors.As(err, &fe) {
		t.Fatalf("expected a *wire.FDBError, got %T: %v", err, err)
	}
	return fe.Code
}

// TestGetMappedRange_RYWDisabled_IsUnsupportedOperation pins that
// read-your-writes-disabled is an ERROR for getMappedRange, not a mode. C++
// ReadYourWrites.actor.cpp:1230-1233 throws unsupported_operation; there is no
// read-through fallback. A client that quietly served the read instead would be
// returning data with none of the conflict ranges that make it serializable.
func TestGetMappedRange_RYWDisabled_IsUnsupportedOperation(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	tx := db.CreateTransaction()
	defer tx.Cancel()

	tx.SetReadYourWritesDisable()

	prefix := []byte(t.Name() + "_")
	_, _, err := tx.GetMappedRange(ctx, prefix, append(append([]byte{}, prefix...), 0xff), []byte("\x01x\x00"), 10, false)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(mappedFDBErrCode(t, err)).To(gomega.Equal(2108),
		"read-your-writes-disabled must be unsupported_operation (2108), not a silently-served read")
}

// TestGetMappedRange_LimitBelowMinusOne_IsRangeLimitsInvalid pins
// GetRangeLimits::isValid (FDBTypes.h:753): rows must be >= 0 or exactly
// ROW_LIMIT_UNLIMITED (-1), so -2 is range_limits_invalid.
//
// It also pins the ORDER against the RYW guard above: RYW is left ENABLED here,
// and a limit of -2 with RYW disabled is covered by the ordering test below.
func TestGetMappedRange_LimitBelowMinusOne_IsRangeLimitsInvalid(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	tx := db.CreateTransaction()
	defer tx.Cancel()

	prefix := []byte(t.Name() + "_")
	_, _, err := tx.GetMappedRange(ctx, prefix, append(append([]byte{}, prefix...), 0xff), []byte("\x01x\x00"), -2, false)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(mappedFDBErrCode(t, err)).To(gomega.Equal(2012), "limit < -1 must be range_limits_invalid")
}

// TestGetMappedRange_LimitCheckPrecedesRYWGuard is the ordering pin. Both the
// limit guard (C++ :1817) and the RYW-disabled guard (C++ :1230) would fire on
// this call, and C++ reaches the limit check first because the snapshot/RYW
// guards live inside readWithConflictRangeForGetMappedRange, which is only
// CALLED at :1828 — after every argument check. Swapping the two in Go would
// still return "an error" and would still be caught by neither test above.
func TestGetMappedRange_LimitCheckPrecedesRYWGuard(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	tx := db.CreateTransaction()
	defer tx.Cancel()

	tx.SetReadYourWritesDisable()

	prefix := []byte(t.Name() + "_")
	_, _, err := tx.GetMappedRange(ctx, prefix, append(append([]byte{}, prefix...), 0xff), []byte("\x01x\x00"), -2, false)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(mappedFDBErrCode(t, err)).To(gomega.Equal(2012),
		"the limit check must precede the RYW-disabled guard: C++ runs every argument check before it calls readWithConflictRangeForGetMappedRange")
}

// TestGetMappedRange_InvertedRange_IsEmptyNotError pins that an inverted range
// is an EMPTY RESULT, not inverted_range (2005). C++ :1823-1826 returns a
// default-constructed MappedRangeResult. This is the arm most likely to be
// "hardened" into an error by someone reading the plain-range path, where an
// inverted range is also tolerated but for different reasons.
func TestGetMappedRange_InvertedRange_IsEmptyNotError(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	tx := db.CreateTransaction()
	defer tx.Cancel()

	prefix := t.Name() + "_"
	rows, more, err := tx.GetMappedRange(ctx, []byte(prefix+"z"), []byte(prefix+"a"), []byte("\x01x\x00"), 10, false)
	g.Expect(err).ToNot(gomega.HaveOccurred(), "an inverted range is an empty result, not an error")
	g.Expect(rows).To(gomega.BeEmpty())
	g.Expect(more).To(gomega.BeFalse())
}

// TestGetMappedRange_SpecialKeySpace_IsClientInvalidOperation pins C++ :1786.
// The special key space (\xff\xff/...) is not mapped-readable, and the check
// runs before the read version is even taken.
func TestGetMappedRange_SpecialKeySpace_IsClientInvalidOperation(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	tx := db.CreateTransaction()
	defer tx.Cancel()

	_, _, err := tx.GetMappedRange(ctx, []byte("\xff\xff/status/json"), []byte("\xff\xff/status/json\x00"), []byte("\x01x\x00"), 10, false)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(mappedFDBErrCode(t, err)).To(gomega.Equal(2000),
		"a mapped read over the special key space must be client_invalid_operation")
}

// TestGetMappedRange_EndpointIndexIsFourteen pins the endpoint index against the
// C++ interface. getMappedKeyValues is DECLARED immediately after getKeyValues
// (StorageServerInterface.h:103), which reads as index 3, but it is ASSIGNED
// getValue.getEndpoint().getAdjustedEndpoint(14) (:184-185) — it was appended
// after getKeyValuesStream so the older indices stayed stable. Taking the index
// from declaration order would address getShardState and the failure would look
// like a wire bug, not an off-by-eleven.
func TestGetMappedRange_EndpointIndexIsFourteen(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	g.Expect(EndpointGetMappedKeyValues).To(gomega.Equal(14),
		"StorageServerInterface.h:184-185 assigns getAdjustedEndpoint(14); declaration order (3) is a decoy")
	g.Expect(EndpointGetMappedKeyValues).ToNot(gomega.Equal(EndpointGetShardState))
}

// TestGetMappedRange_EmptyPrimaryRange_IsEmptyNotError is the first test here
// that actually puts a getMappedKeyValues request on the wire and reads the
// reply, so it is what proves the request is well-formed enough for a real
// 7.3 storage server to accept: adjusted endpoint 14, the mapper spliced at
// serialize position 3, and a reply decoded through the generated union.
//
// The primary range is empty, which is the shape that decodes successfully
// while yielding garbage if any of the above is wrong — a misaddressed endpoint
// or a malformed request would surface as an error here, not as an empty result.
func TestGetMappedRange_EmptyPrimaryRange_IsEmptyNotError(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	tx := db.CreateTransaction()
	defer tx.Cancel()

	prefix := []byte(t.Name() + "_")
	// A valid one-element tuple ("x",) with no placeholder: parseable by the
	// server, and never actually applied because there are no primary rows.
	rows, more, err := tx.GetMappedRange(ctx, prefix, append(append([]byte{}, prefix...), 0xff), []byte("\x01x\x00"), 100, false)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(rows).To(gomega.BeEmpty())
	g.Expect(more).To(gomega.BeFalse())
}

// TestGetMappedRange_MapperNotTuple_PropagatesServerError pins the
// propagate-unchanged contract. The client does ZERO mapper validation
// (fdb_c.cpp forwards the bytes verbatim), so mapper_not_tuple (2043) can only
// come from the storage server — Tuple::unpack at
// fdbserver/storageserver.actor.cpp:5969, which throws at :5972.
//
// That parse happens at the TOP of mapKeyValues, BEFORE the per-row loop, so it
// fires even though the primary range here is empty. That is what makes this
// deterministic with no data setup — and it is also why the error cannot be
// mistaken for a client-side check: an empty range gives a Go-side validator
// nothing to reject.
//
// Reaching 2043 at all proves the reply's INLINE LoadBalancedReply.error arm is
// being read. The storage server delivers mapper errors through that field, not
// through the ErrorOr root, so a parser that skipped it would report this as a
// successful empty read — green, and silently wrong.
func TestGetMappedRange_MapperNotTuple_PropagatesServerError(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	tx := db.CreateTransaction()
	defer tx.Cancel()

	prefix := []byte(t.Name() + "_")
	// 0xFF is not a valid tuple type code, so Tuple::unpack throws.
	_, _, err := tx.GetMappedRange(ctx, prefix, append(append([]byte{}, prefix...), 0xfe), []byte{0xff, 0xff, 0xff}, 100, false)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(mappedFDBErrCode(t, err)).To(gomega.Equal(2043),
		"mapper_not_tuple must arrive from the storage server, through the reply's inline error arm, unchanged")
}

// TestGetMappedRange_SnapshotCannotRequestIt pins a NEGATIVE result that the
// rest of this file silently depends on. C++ raises unsupported_operation (2108)
// for a snapshot mapped range (ReadYourWrites.actor.cpp:1219-1225). Go never
// returns that code for snapshot — not because the guard was forgotten, but
// because *Snapshot exposes no GetMappedRange, so the request cannot be
// expressed. Unreachable-by-construction is the stronger position, but it is
// invisible: adding a single method to *Snapshot would arm the exact divergence
// C++ guards against, with every existing test still green.
//
// So the absence itself is the assertion. If someone gives *Snapshot a
// GetMappedRange, this fails and names what must be added with it.
func TestGetMappedRange_SnapshotCannotRequestIt(t *testing.T) {
	t.Parallel()

	// A *Snapshot must NOT satisfy an interface carrying GetMappedRange. This is
	// a compile-time-shaped fact checked at runtime so the failure message can
	// explain the consequence rather than just failing to build.
	type mappedRangeCapable interface {
		GetMappedRange(ctx context.Context, begin, end, mapper []byte, limit int, reverse bool) ([]MappedKeyValue, bool, error)
	}
	var s any = (*Snapshot)(nil)
	if _, ok := s.(mappedRangeCapable); ok {
		t.Fatal("*Snapshot now exposes GetMappedRange. C++ raises unsupported_operation " +
			"(2108) for a snapshot mapped range (ReadYourWrites.actor.cpp:1219-1225); Go " +
			"relied on the method not existing instead of returning that code. Either drop " +
			"the method again, or add an explicit snapshot guard returning 2108 and pin it.")
	}

	// Guard the guard: the interface must actually match the real signature, or
	// the assertion above is vacuous — it would pass for a *Snapshot that had the
	// method under a different shape. A *Transaction MUST satisfy it.
	var tx any = (*Transaction)(nil)
	if _, ok := tx.(mappedRangeCapable); !ok {
		t.Fatal("*Transaction does not satisfy mappedRangeCapable — the signature in this " +
			"test has drifted from Transaction.GetMappedRange, so the *Snapshot assertion " +
			"above proves nothing")
	}
}
