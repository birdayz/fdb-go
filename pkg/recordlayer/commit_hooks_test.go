package recordlayer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

var _ = Describe("Commit hooks", func() {
	var (
		ctx context.Context
		md  *RecordMetaData
	)

	BeforeEach(func() {
		ctx = context.Background()
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		var err error
		md, err = builder.Build()
		Expect(err).NotTo(HaveOccurred())
	})

	Describe("explicit commit lifecycle", func() {
		for _, method := range []string{"plain", "hooks", "versionstamp"} {
			commit := func(rtx *FDBRecordContext) error {
				switch method {
				case "plain":
					return rtx.Commit()
				case "hooks":
					return rtx.CommitWithHooks()
				default:
					_, err := rtx.CommitWithVersionstamp()
					return err
				}
			}
			It(method+" runs checks before flushing deferred versions and postcommit after persistence", func() {
				tx, err := sharedDB.CreateTransaction()
				Expect(err).NotTo(HaveOccurred())
				defer tx.Cancel()
				rtx := NewFDBRecordContext(tx, nil)
				timer := NewStoreTimer()
				rtx.SetTimer(timer)
				key := specSubspace().Pack(tuple.Tuple{"checked-version"})
				var order []string
				rtx.AddCommitCheck(func() error {
					order = append(order, "check")
					Expect(timer.GetCount(EventCommit)).To(BeZero())
					rtx.AddVersionMutation(MutationTypeSetVersionstampedValue, key, make([]byte, 14))
					return nil
				})
				rtx.AddPostCommit(func() {
					order = append(order, "post")
					Expect(timer.GetCount(EventCommit)).To(Equal(int64(1)))
					_, err := sharedDB.Run(ctx, func(reader *FDBRecordContext) (any, error) {
						value, err := reader.Transaction().Get(fdb.Key(key)).Get()
						Expect(err).NotTo(HaveOccurred())
						Expect(value).To(HaveLen(10))
						Expect(value).NotTo(Equal(make([]byte, 10)))
						return nil, err
					})
					Expect(err).NotTo(HaveOccurred())
				})
				Expect(commit(rtx)).To(Succeed())
				Expect(order).To(Equal([]string{"check", "post"}))
			})
			It(method+" records a failed transaction commit without running postcommit", func() {
				tx, err := sharedDB.CreateTransaction()
				Expect(err).NotTo(HaveOccurred())
				defer tx.Cancel()
				rtx := NewFDBRecordContext(tx, nil)
				timer := NewStoreTimer()
				rtx.SetTimer(timer)
				var order []string
				rtx.AddCommitCheck(func() error {
					order = append(order, "check")
					return nil
				})
				rtx.AddPostCommit(func() { order = append(order, "post") })
				tx.Cancel()
				var canceled fdb.Error
				Expect(errors.As(commit(rtx), &canceled)).To(BeTrue())
				Expect(canceled.Code).To(Equal(1025))
				Expect(order).To(Equal([]string{"check"}))
				Expect(timer.GetCount(EventCommit)).To(Equal(int64(1)))
			})
			It(method+" propagates a failed check without committing writes or running postcommit", func() {
				tx, err := sharedDB.CreateTransaction()
				Expect(err).NotTo(HaveOccurred())
				defer tx.Cancel()
				rtx := NewFDBRecordContext(tx, nil)
				timer := NewStoreTimer()
				rtx.SetTimer(timer)
				key := specSubspace().Pack(tuple.Tuple{"rejected-write"})
				tx.Set(fdb.Key(key), []byte("must not persist"))
				checkErr := &RecordDoesNotExistError{PrimaryKey: tuple.Tuple{"commit-check"}}
				var order []string
				rtx.AddCommitCheck(func() error {
					order = append(order, "check")
					return checkErr
				})
				rtx.AddPostCommit(func() { order = append(order, "post") })
				Expect(commit(rtx)).To(MatchError(checkErr))
				Expect(timer.GetCount(EventCommit)).To(Equal(int64(1)))
				Expect(order).To(Equal([]string{"check"}))
				_, err = sharedDB.Run(ctx, func(reader *FDBRecordContext) (any, error) {
					value, err := reader.Transaction().Get(fdb.Key(key)).Get()
					Expect(value).To(BeNil())
					return nil, err
				})
				Expect(err).NotTo(HaveOccurred())
			})
		}
	})

	Describe("named commit checks", func() {
		It("deduplicates concurrent registration and preserves anonymous checks", func() {
			rtx := NewFDBRecordContext(nil, nil)
			var supplied, checked atomic.Int32
			var wg sync.WaitGroup
			for range 16 {
				wg.Go(func() {
					rtx.getOrCreateCommitCheck("retirement", func(string) CommitCheckFunc {
						supplied.Add(1)
						return func() error { checked.Add(1); return nil }
					})
				})
			}
			wg.Wait()
			Expect(supplied.Load()).To(Equal(int32(1)))
			Expect(rtx.getCommitCheck("retirement")).NotTo(BeNil())
			Expect(rtx.getCommitCheck("absent")).To(BeNil())
			rtx.AddCommitCheck(func() error { checked.Add(10); return nil })
			Expect(rtx.runCommitChecks()).To(Succeed())
			Expect(checked.Load()).To(Equal(int32(11)))
		})

		It("cancels a snapshotted check from an earlier check without canceling another name", func() {
			rtx := NewFDBRecordContext(nil, nil)
			var order []string
			rtx.AddCommitCheck(func() error {
				order = append(order, "delete")
				rtx.removeCommitCheck("deleted")
				return nil
			})
			for _, name := range []string{"deleted", "retained"} {
				rtx.getOrCreateCommitCheck(name, func(name string) CommitCheckFunc {
					return func() error { order = append(order, name); return nil }
				})
			}
			Expect(rtx.runCommitChecks()).To(Succeed())
			Expect(order).To(Equal([]string{"delete", "retained"}))
			Expect(rtx.getCommitCheck("deleted")).To(BeNil())
		})

		It("re-registers a canceled name with a new callback and keeps registration order", func() {
			rtx := NewFDBRecordContext(nil, nil)
			var order []string
			register := func(name, label string) {
				rtx.getOrCreateCommitCheck(name, func(string) CommitCheckFunc {
					return func() error { order = append(order, label); return nil }
				})
			}
			register("store", "old")
			rtx.removeCommitCheck("store")
			rtx.removeCommitCheck("absent")
			register("other", "other")
			register("store", "new")
			Expect(rtx.runCommitChecks()).To(Succeed())
			Expect(order).To(Equal([]string{"other", "new"}))
		})

		It("propagates named check errors and stops before subsequent checks", func() {
			rtx := NewFDBRecordContext(nil, nil)
			checkErr := &RecordDoesNotExistError{PrimaryKey: tuple.Tuple{"named-check"}}
			var order []string
			rtx.getOrCreateCommitCheck("failure", func(string) CommitCheckFunc {
				return func() error { order = append(order, "failure"); return checkErr }
			})
			rtx.AddCommitCheck(func() error { order = append(order, "later"); return nil })
			Expect(rtx.runCommitChecks()).To(MatchError(checkErr))
			Expect(order).To(Equal([]string{"failure"}))
		})
	})

	Describe("AddCommitCheck", func() {
		It("runs pre-commit check that passes", func() {
			checkRan := false
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				id := int64(1)
				price := int32(100)
				_, err = store.SaveRecord(&gen.Order{OrderId: &id, Price: &price})
				if err != nil {
					return nil, err
				}
				rtx.AddCommitCheck(func() error {
					checkRan = true
					return nil
				})
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(checkRan).To(BeTrue())
		})

		It("aborts on pre-commit check failure", func() {
			errCheck := errors.New("consistency violation")
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				id := int64(1)
				price := int32(100)
				_, err = store.SaveRecord(&gen.Order{OrderId: &id, Price: &price})
				if err != nil {
					return nil, err
				}
				rtx.AddCommitCheck(func() error {
					return errCheck
				})
				return nil, nil
			})
			Expect(err).To(MatchError(errCheck))
		})

		It("runs multiple checks in order", func() {
			var order []int
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.AddCommitCheck(func() error {
					order = append(order, 1)
					return nil
				})
				rtx.AddCommitCheck(func() error {
					order = append(order, 2)
					return nil
				})
				rtx.AddCommitCheck(func() error {
					order = append(order, 3)
					return nil
				})
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(order).To(Equal([]int{1, 2, 3}))
		})

		It("stops at first failing check", func() {
			var order []int
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.AddCommitCheck(func() error {
					order = append(order, 1)
					return nil
				})
				rtx.AddCommitCheck(func() error {
					order = append(order, 2)
					return errors.New("fail")
				})
				rtx.AddCommitCheck(func() error {
					order = append(order, 3)
					return nil
				})
				return nil, nil
			})
			Expect(err).To(HaveOccurred())
			Expect(order).To(Equal([]int{1, 2}))
		})
	})

	Describe("AddPostCommit", func() {
		It("runs post-commit callback after successful commit", func() {
			postCommitRan := false
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.AddPostCommit(func() {
					postCommitRan = true
				})
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(postCommitRan).To(BeTrue())
		})

		It("does not run post-commit on error", func() {
			postCommitRan := false
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.AddPostCommit(func() {
					postCommitRan = true
				})
				return nil, errors.New("user error")
			})
			Expect(err).To(HaveOccurred())
			Expect(postCommitRan).To(BeFalse())
		})

		It("does not run post-commit when pre-commit check fails", func() {
			postCommitRan := false
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.AddCommitCheck(func() error {
					return errors.New("check failed")
				})
				rtx.AddPostCommit(func() {
					postCommitRan = true
				})
				return nil, nil
			})
			Expect(err).To(HaveOccurred())
			Expect(postCommitRan).To(BeFalse())
		})
	})
})

func FuzzNamedCommitCheckLifecycle(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 0, 3, 1, 0, 2, 3})
	f.Add([]byte{4, 8, 0, 5, 8, 3})
	f.Fuzz(func(t *testing.T, operations []byte) {
		t.Parallel()
		if len(operations) > 512 {
			operations = operations[:512]
		}
		rtx := NewFDBRecordContext(nil, nil)
		type registration struct {
			id        int
			key       byte
			anonymous bool
		}
		var registrations []registration
		live := make(map[byte]int)
		var actual []int
		factories, wantFactories := 0, 0
		verify := func() {
			t.Helper()
			actual = nil
			if err := rtx.runCommitChecks(); err != nil {
				t.Fatal(err)
			}
			var expected []int
			for _, entry := range registrations {
				if entry.anonymous || live[entry.key] == entry.id {
					expected = append(expected, entry.id)
				}
			}
			if !slices.Equal(actual, expected) || factories != wantFactories {
				t.Fatalf("operations=%v: invoked=%v want=%v factories=%d want=%d", operations, actual, expected, factories, wantFactories)
			}
		}
		for pos, operation := range operations {
			id, key := pos+1, operation>>2
			name := fmt.Sprintf("check-%d", key)
			switch operation & 3 {
			case 0:
				if _, exists := live[key]; !exists {
					live[key] = id
					registrations = append(registrations, registration{id: id, key: key})
					wantFactories++
				}
				rtx.getOrCreateCommitCheck(name, func(string) CommitCheckFunc {
					factories++
					return func() error { actual = append(actual, id); return nil }
				})
			case 1:
				delete(live, key)
				rtx.removeCommitCheck(name)
			case 2:
				registrations = append(registrations, registration{id: id, anonymous: true})
				rtx.AddCommitCheck(func() error { actual = append(actual, id); return nil })
			case 3:
				verify()
			}
		}
		verify()
	})
}
