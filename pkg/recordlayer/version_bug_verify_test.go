package recordlayer

import (
	"context"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("VersionBugVerify", func() {
	ctx := context.Background()
	_ = ctx

	// Bug 1: Next()/Prev() must carry/borrow across full 12 bytes for complete versions.
	Describe("Next/Prev carry/borrow across full 12 bytes", func() {
		It("Next on LastInDBVersion carries into global version", func() {
			last := LastInDBVersion(5)
			Expect(last.GetLocalVersion()).To(Equal(0xFFFF))

			next, err := last.Next()
			Expect(err).NotTo(HaveOccurred())

			expected := FirstInDBVersion(6)
			Expect(next.Equal(expected)).To(BeTrue(),
				"next should equal FirstInDBVersion(6)")
		})

		It("Prev on FirstInDBVersion borrows from global version", func() {
			first := FirstInDBVersion(5)
			Expect(first.GetLocalVersion()).To(Equal(0))

			prev, err := first.Prev()
			Expect(err).NotTo(HaveOccurred())

			expected := LastInDBVersion(4)
			Expect(prev.Equal(expected)).To(BeTrue(),
				"prev should equal LastInDBVersion(4)")
		})

		It("Next on incomplete version only touches local (matches Java)", func() {
			v, err := IncompleteVersion(5)
			Expect(err).NotTo(HaveOccurred())

			next, err := v.Next()
			Expect(err).NotTo(HaveOccurred())
			Expect(next.GetLocalVersion()).To(Equal(6))
			Expect(next.IsComplete()).To(BeFalse())
		})

		It("Next on incomplete version at max local errors (matches Java)", func() {
			v, err := IncompleteVersion(0xFFFF)
			Expect(err).NotTo(HaveOccurred())

			_, err = v.Next()
			Expect(err).To(HaveOccurred())
		})

		It("Next on MaxVersion errors", func() {
			_, err := MaxVersion().Next()
			Expect(err).To(HaveOccurred())
		})

		It("Prev on MinVersion errors", func() {
			_, err := MinVersion().Prev()
			Expect(err).To(HaveOccurred())
		})
	})

	// Bug 2: NewCompleteVersion must reject all-0xFF global version.
	Describe("NewCompleteVersion rejects all-0xFF global version", func() {
		It("rejects all-0xFF global version", func() {
			allFF := make([]byte, GlobalVersionBytes)
			for i := range allFF {
				allFF[i] = 0xFF
			}

			_, err := NewCompleteVersion(allFF, 0)
			Expect(err).To(HaveOccurred())
		})

		It("rejects all-0xFF via CompleteVersionFromBytes", func() {
			allFF := make([]byte, VersionBytes)
			for i := 0; i < GlobalVersionBytes; i++ {
				allFF[i] = 0xFF
			}

			_, err := CompleteVersionFromBytes(allFF)
			Expect(err).To(HaveOccurred())
		})

		It("MaxVersion byte 9 is 0xFE (not all-0xFF)", func() {
			maxV := MaxVersion()
			gv, err := maxV.GetGlobalVersion()
			Expect(err).NotTo(HaveOccurred())
			Expect(gv[9]).To(Equal(byte(0xFE)))
		})
	})

	// Bug 3: WithCommittedVersion must reject already-complete versions.
	Describe("WithCommittedVersion rejects complete version", func() {
		It("rejects already-complete version", func() {
			globalV := make([]byte, GlobalVersionBytes)
			globalV[0] = 0x01
			v, err := NewCompleteVersion(globalV, 42)
			Expect(err).NotTo(HaveOccurred())
			Expect(v.IsComplete()).To(BeTrue())

			newGlobal := make([]byte, GlobalVersionBytes)
			newGlobal[0] = 0x02

			_, err = v.WithCommittedVersion(newGlobal)
			Expect(err).To(HaveOccurred())
		})

		It("works on incomplete version (baseline)", func() {
			v, err := IncompleteVersion(7)
			Expect(err).NotTo(HaveOccurred())
			Expect(v.IsComplete()).To(BeFalse())

			committed := make([]byte, GlobalVersionBytes)
			committed[0] = 0x42
			result, err := v.WithCommittedVersion(committed)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsComplete()).To(BeTrue())
			Expect(result.GetLocalVersion()).To(Equal(7))
			gv, err := result.GetGlobalVersion()
			Expect(err).NotTo(HaveOccurred())
			Expect(gv[0]).To(Equal(byte(0x42)))
		})
	})

	// Bug 4: CommitWithVersionstamp must run pre-commit checks and post-commit hooks.
	Describe("CommitWithVersionstamp runs hooks", func() {
		It("runs both pre-commit checks and post-commit hooks", func() {
			checkRan := false
			postCommitRan := false

			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()

			rtx := &FDBRecordContext{
				tx: tx,
			}

			rtx.AddCommitCheck(func() error {
				checkRan = true
				return nil
			})
			rtx.AddPostCommit(func() {
				postCommitRan = true
			})

			tx.Set(fdb.Key(specSubspace().Pack(tuple.Tuple{"bug4-test"})), []byte("val"))

			_, _ = rtx.CommitWithVersionstamp()
			// May or may not error (read-only versionstamp), but hooks should have run
			Expect(checkRan).To(BeTrue(), "CommitWithVersionstamp must run pre-commit checks")
			Expect(postCommitRan).To(BeTrue(), "CommitWithVersionstamp must run post-commit hooks")
		})

		It("Run() calls both hooks (baseline)", func() {
			checkRan := false
			postCommitRan := false

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.AddCommitCheck(func() error {
					checkRan = true
					return nil
				})
				rtx.AddPostCommit(func() {
					postCommitRan = true
				})
				rtx.Transaction().Set(
					fdb.Key(specSubspace().Pack(tuple.Tuple{"bug4-baseline"})),
					[]byte("val"),
				)
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(checkRan).To(BeTrue())
			Expect(postCommitRan).To(BeTrue())
		})
	})
})
