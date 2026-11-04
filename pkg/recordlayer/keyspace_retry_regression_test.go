package recordlayer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type resolverRetryRead struct {
	readVersion    int64
	name           string
	found          bool
	err            error
	configured     bool
	configureCalls int
}

// observingRetryResolver delegates to the real persisted reverse mapping and
// records the read versions and outcomes of the borrowed and fresh attempts.
type observingRetryResolver struct {
	LocatableResolver
	reads          []resolverRetryRead
	configureCalls *int
}

func (r *observingRetryResolver) readReverse(rc *FDBRecordContext, value int64) (string, bool, error) {
	rv, err := rc.Transaction().GetReadVersion().Get()
	if err != nil {
		return "", false, err
	}
	name, found, err := r.LocatableResolver.readReverse(rc, value)
	r.reads = append(r.reads, resolverRetryRead{
		readVersion:    rv,
		name:           name,
		found:          found,
		err:            err,
		configured:     rc.configure != nil,
		configureCalls: *r.configureCalls,
	})
	return name, found, err
}

var _ = Describe("KeySpace resolver retry regressions", func() {
	It("rejects a value whose warmed in-memory reverse name disagrees with the allocation", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		scope := ExtendedDirectoryLayerGlobal{}
		name := fmt.Sprintf("%x/cached-name", specSubspace().Bytes())
		resolver := NewKeySpaceResolver(sharedDB, scope)
		value, err := resolver.Resolve(ctx, name)
		Expect(err).NotTo(HaveOccurred())
		back, found, err := resolver.ReverseLookup(ctx, value)
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(back).To(Equal(name))

		cache := sharedDB.resolvers()
		cache.mu.RLock()
		cached, present := cache.reverse[reverseCacheKey(scope, value)]
		cache.mu.RUnlock()
		Expect(present).To(BeTrue(), "the regression must hit the in-memory cache branch")
		Expect(cached).To(Equal(name))

		tx, err := sharedDB.CreateWritableTransaction()
		Expect(err).NotTo(HaveOccurred())
		defer tx.Cancel()
		parent := sharedDB.NewRecordContextIn(ctx, tx)
		Expect(putReverseIfNotExists(parent, scope, name, value)).To(Succeed())
		err = putReverseIfNotExists(parent, scope, name+"-conflicting-allocation", value)
		Expect(err).To(HaveOccurred())
		var retriable *RecordCoreRetriableTransactionError
		Expect(errors.As(err, &retriable)).To(BeTrue(), "a disk-only name mismatch has a different error type")
		Expect(retriable.Message).To(Equal("Provided value for path key does not match existing value in reverse directory layer in-memory cache"))
		Expect(isRetriableAnyCause(err)).To(BeTrue())

		back, found, err = resolver.ReverseLookup(ctx, value)
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(back).To(Equal(name), "the failed allocation must not replace the cached name")
	})

	It("retries a borrowed stale reverse miss at a fresh version and inherits configuration on both attempts", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		scope := NewScopedInterningLayer(specSubspace().Sub("stale-reverse"))
		tx, err := sharedDB.CreateWritableTransaction()
		Expect(err).NotTo(HaveOccurred())
		defer tx.Cancel()
		parent := sharedDB.NewRecordContextIn(ctx, tx)
		configureCalls := 0
		Expect(parent.Configure(func(options fdb.TransactionOptions) error {
			configureCalls++
			return options.SetTimeout(30000)
		})).To(Succeed())
		Expect(configureCalls).To(Equal(1))
		parentVersion, err := tx.GetReadVersion().Get()
		Expect(err).NotTo(HaveOccurred())

		// Resolve commits independently after the parent has pinned its snapshot.
		const name = "committed-after-parent-snapshot"
		value, err := NewKeySpaceResolver(sharedDB, scope).Resolve(ctx, name)
		Expect(err).NotTo(HaveOccurred())
		staleName, staleFound, err := scope.readReverse(parent, value)
		Expect(err).NotTo(HaveOccurred())
		Expect(staleFound).To(BeFalse(), "the pinned parent must actually predate the reverse mapping")
		Expect(staleName).To(BeEmpty())
		cache := sharedDB.resolvers()
		cache.mu.RLock()
		_, cached := cache.reverse[reverseCacheKey(scope, value)]
		cache.mu.RUnlock()
		Expect(cached).To(BeFalse(), "a warmed reverse cache would bypass the stale-read shape")

		observed := &observingRetryResolver{LocatableResolver: scope, configureCalls: &configureCalls}
		back, err := reverseLookupFromDatabase(parent, observed, value)
		Expect(err).NotTo(HaveOccurred())
		Expect(back).To(Equal(name))
		Expect(observed.reads).To(HaveLen(2), "one borrowed miss followed by one fresh successful read")
		Expect(observed.reads[0].readVersion).To(Equal(parentVersion))
		Expect(observed.reads[0].found).To(BeFalse())
		Expect(observed.reads[0].name).To(BeEmpty())
		Expect(observed.reads[0].err).NotTo(HaveOccurred())
		Expect(observed.reads[1].readVersion).To(BeNumerically(">", parentVersion))
		Expect(observed.reads[1].found).To(BeTrue())
		Expect(observed.reads[1].name).To(Equal(name))
		Expect(observed.reads[1].err).NotTo(HaveOccurred())
		Expect(observed.reads[0].configured).To(BeTrue())
		Expect(observed.reads[1].configured).To(BeTrue())
		Expect(observed.reads[0].configureCalls).To(Equal(2))
		Expect(observed.reads[1].configureCalls).To(Equal(3))
		Expect(configureCalls).To(Equal(3))

		cache.mu.RLock()
		cachedName, cached := cache.reverse[reverseCacheKey(scope, value)]
		cache.mu.RUnlock()
		Expect(cached).To(BeTrue())
		Expect(cachedName).To(Equal(name))
		// Returning a fresh child's mapping must not advance the parent's snapshot.
		afterVersion, err := tx.GetReadVersion().Get()
		Expect(err).NotTo(HaveOccurred())
		Expect(afterVersion).To(Equal(parentVersion))
	})
})
