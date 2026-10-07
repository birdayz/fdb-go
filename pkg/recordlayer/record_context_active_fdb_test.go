package recordlayer

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/pkg/fdbgo/fdb"
)

// Commit ownership (RFC-257 WS-D D-0): a context is deactivated by its first
// commit whatever the outcome, as Java's commitAsync closes it on both arms,
// and a later commit is refused with Java's RecordContextNotActiveException
// ("Transaction is no longer active."). A context a route (Run, the runner)
// hands its body is the route's to commit: a commit in the body is refused
// before anything commits.
var _ = Describe("Record context commit ownership", func() {
	ctx := context.Background()
	notActive := func(err error) {
		var na *RecordContextNotActiveError
		Expect(errors.As(err, &na)).To(BeTrue(), "got %v", err)
		Expect(na.Message).To(Equal("Transaction is no longer active."))
		var storage *RecordCoreStorageError
		Expect(errors.As(err, &storage)).To(BeTrue(), "a RecordContextNotActiveException is a RecordCoreStorageException")
	}
	read := func(key fdb.Key) []byte {
		v, err := sharedDB.RunRead(ctx, func(tx fdb.ReadTransaction) (any, error) { return tx.Get(key).Get() })
		Expect(err).NotTo(HaveOccurred())
		if v == nil {
			return nil
		}
		return v.([]byte)
	}

	It("refuses a commit in a Run body before anything commits", func() {
		key := fdb.Key(specSubspace().Pack(nil))
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			rc.Transaction().Set(key, []byte("body"))
			return nil, rc.Commit()
		})
		notActive(err)
		Expect(read(key)).To(BeNil(), "the refused body commit committed")
	})

	It("refuses a commit in the runner's body", func() {
		key := fdb.Key(specSubspace().Pack(nil))
		_, err := NewFDBDatabaseRunner(sharedDB).RunWithRetry(ctx, func(rc *FDBRecordContext) (any, error) {
			rc.Transaction().Set(key, []byte("body"))
			return nil, rc.Commit()
		})
		notActive(err)
		Expect(read(key)).To(BeNil())
	})

	It("refuses a second commit of an explicit context, after success and after failure", func() {
		key := fdb.Key(specSubspace().Pack(nil))
		rc, err := NewFDBDatabaseRunner(sharedDB).OpenContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		rc.Transaction().Set(key, []byte("first"))
		Expect(rc.Commit()).To(Succeed())
		notActive(rc.Commit())
		Expect(read(key)).To(Equal([]byte("first")))

		failing, err := NewFDBDatabaseRunner(sharedDB).OpenContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		failing.AddCommitCheck(func() error { return errors.New("check failed") })
		Expect(failing.Commit()).To(MatchError("check failed"))
		notActive(failing.Commit())
	})
})
