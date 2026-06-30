package client

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
	"github.com/onsi/gomega"
)

// TestAtomic_RejectsNonAtomicOpCode pins the C++ atomicOp op-code gate
// (ReadYourWrites.actor.cpp:2234: !isValidMutationType || !isAtomicOp → invalid_mutation_type).
// libfdb_c's fdb_transaction_atomic_op wraps the throw in CATCH_AND_DIE (fdb_c.cpp:1149), aborting
// the client before any write reaches the cluster — so a non-atomic op-code passed to Atomic() must
// NOT mutate the shared cluster. The most dangerous case is Atomic(MutClearRange,...), which at
// commit time is indistinguishable from a real Clear and would silently DELETE a range. Go now
// rejects every non-atomic op eagerly in Atomic() (without buffering the mutation) and fails the
// commit with 2018. Revert-proof: backing out the isAtomicOp gate makes the ClearRange subtest
// delete a,b and the commit succeed.
func TestAtomic_RejectsNonAtomicOpCode(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	prefix := t.Name() + "_"
	ka := []byte(prefix + "a")
	kb := []byte(prefix + "b")
	kc := []byte(prefix + "c")
	le8 := func(n uint64) []byte { b := make([]byte, 8); binary.LittleEndian.PutUint64(b, n); return b }

	codeOf := func(err error) int {
		var fe *wire.FDBError
		if errors.As(err, &fe) {
			return fe.Code
		}
		return -1
	}

	// Seed a=1, b=2.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(ka, []byte("1"))
		tx.Set(kb, []byte("2"))
		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// DESTRUCTIVE op-code (MutClearRange via Atomic): must be rejected with 2018, and a,b must survive.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Atomic(MutClearRange, ka, kc) // op-code 1 — NOT an atomic op
		return nil, nil
	})
	g.Expect(codeOf(err)).To(gomega.Equal(ErrInvalidMutationType), "Atomic(MutClearRange) must fail with invalid_mutation_type (2018)")

	// The bad ClearRange must NOT have reached the cluster.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		va, e := tx.Get(ctx, ka)
		g.Expect(e).ToNot(gomega.HaveOccurred())
		g.Expect(string(va)).To(gomega.Equal("1"), "a must survive a rejected Atomic(MutClearRange)")
		vb, e := tx.Get(ctx, kb)
		g.Expect(e).ToNot(gomega.HaveOccurred())
		g.Expect(string(vb)).To(gomega.Equal("2"), "b must survive a rejected Atomic(MutClearRange)")
		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// SetValue via Atomic (op-code 0) — also non-atomic → rejected, k stays absent.
	kset := []byte(prefix + "set")
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Atomic(MutSetValue, kset, []byte("x"))
		return nil, nil
	})
	g.Expect(codeOf(err)).To(gomega.Equal(ErrInvalidMutationType))
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		v, e := tx.Get(ctx, kset)
		g.Expect(e).ToNot(gomega.HaveOccurred())
		g.Expect(v).To(gomega.BeNil(), "Atomic(MutSetValue) must not write")
		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// POSITIVE control: a real atomic op (Add) still commits and applies.
	kadd := []byte(prefix + "add")
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Atomic(MutAddValue, kadd, le8(5))
		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		v, e := tx.Get(ctx, kadd)
		g.Expect(e).ToNot(gomega.HaveOccurred())
		g.Expect(v).To(gomega.Equal(le8(5)), "valid Atomic(MutAddValue) must apply")
		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// RESET clears the poison: a reused handle can commit valid work after a rejected atomic op.
	tx := db.CreateTransaction()
	tx.Atomic(MutClearRange, ka, kc) // poisons
	tx.Reset()
	kok := []byte(prefix + "ok")
	tx.Set(kok, []byte("v"))
	g.Expect(tx.Commit(ctx)).ToNot(gomega.HaveOccurred(), "Reset must clear the invalid-atomic-op poison")
}
