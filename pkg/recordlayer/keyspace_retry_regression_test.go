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
	context        *FDBRecordContext
	injected       bool
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
	reads                []resolverRetryRead
	configureCalls       *int
	parentVersion        int64
	extraRetry, injected bool
}

func (r *observingRetryResolver) readReverse(rc *FDBRecordContext, value int64) (string, bool, error) {
	read := resolverRetryRead{context: rc, configured: rc.configure != nil, configureCalls: *r.configureCalls}
	var name string
	var found bool
	rv, err := rc.Transaction().GetReadVersion().Get()
	if err == nil {
		name, found, err = r.LocatableResolver.readReverse(rc, value)
	}
	if err == nil && found && rv > r.parentVersion && r.extraRetry && !r.injected {
		r.injected, read.injected = true, true
		err = fdb.Error{Code: 1007}
	}
	read.readVersion, read.name, read.found, read.err = rv, name, found, err
	r.reads = append(r.reads, read)
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

	for _, extraRetry := range []bool{false, true} {
		It(fmt.Sprintf("retries a borrowed stale reverse miss and inherits each attempt's configuration extraRetry=%t", extraRetry), func() {
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

			observed := &observingRetryResolver{LocatableResolver: scope, configureCalls: &configureCalls, parentVersion: parentVersion, extraRetry: extraRetry}
			back, err := reverseLookupFromDatabase(parent, observed, value)
			Expect(err).NotTo(HaveOccurred())
			Expect(back).To(Equal(name))
			Expect(observed.reads).NotTo(BeEmpty())
			borrowed, injected := 0, 0
			for i, read := range observed.reads {
				Expect(read.configured).To(BeTrue())
				Expect(read.configureCalls).To(Equal(i+2), "the parent configured once, then each entered child attempt configures exactly once")
				if i != 0 {
					Expect(read.context).NotTo(BeIdenticalTo(observed.reads[i-1].context))
				}
				if read.injected {
					injected++
					Expect(borrowed).To(Equal(1), "the extra retry follows the intended borrowed-version miss")
					var native fdb.Error
					Expect(errors.As(read.err, &native)).To(BeTrue())
					Expect(native.Code).To(Equal(1007))
					Expect(read.readVersion).To(BeNumerically(">", parentVersion))
					Expect(read.found).To(BeTrue())
					Expect(read.name).To(Equal(name))
					continue
				}
				if read.err != nil {
					Expect(isRetriableAnyCause(read.err)).To(BeTrue(), "independent failed attempt: %v", read.err)
					continue
				}
				if read.readVersion == parentVersion {
					borrowed++
					Expect(read.found).To(BeFalse())
					Expect(read.name).To(BeEmpty())
				} else {
					Expect(borrowed).To(Equal(1))
					Expect(read.readVersion).To(BeNumerically(">", parentVersion))
					Expect(read.found).To(BeTrue())
					Expect(read.name).To(Equal(name))
				}
			}
			Expect(borrowed).To(Equal(1), "exactly the intended borrowed miss, not an always-fresh implementation")
			wantInjected := 0
			if extraRetry {
				wantInjected = 1
			}
			Expect(injected).To(Equal(wantInjected))
			last := observed.reads[len(observed.reads)-1]
			Expect(last.injected).To(BeFalse())
			Expect(last.err).NotTo(HaveOccurred())
			Expect(last.readVersion).To(BeNumerically(">", parentVersion))
			Expect(last.found).To(BeTrue())
			Expect(last.name).To(Equal(name))
			Expect(configureCalls).To(Equal(1 + len(observed.reads)))
			fmt.Fprintf(GinkgoWriter, "RESOLVER_ATTEMPTS extraRetry=%t borrowedMisses=%d injectedFreshErrors=%d enteredReads=%d configurations=%d\n", extraRetry, borrowed, injected, len(observed.reads), configureCalls)

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
	}
})
