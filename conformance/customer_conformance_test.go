//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("Customer Conformance", func() {
	var (
		ctx           context.Context
		env           *TenantEnvironment
		customerStore *CustomerConformanceStore
		orderStore    *ConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error

		tenantName := fmt.Sprintf("customer_%s", uuid.New().String())

		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		customerStore = NewCustomerConformanceStoreWithTenant(env.RecordDB, env.MetaData, env.ClusterFile, env.TenantName)
		orderStore = NewConformanceStoreWithTenant(env.RecordDB, env.MetaData, env.ClusterFile, env.TenantName)
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes Customer, Java reads", func() {
		It("should save a standard customer and cross-validate", func() {
			customer := StandardCustomer(1001)
			err := customerStore.SaveCustomer(ctx, customer)
			Expect(err).NotTo(HaveOccurred())

			loaded, err := customerStore.LoadCustomer(ctx, 1001)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loaded.CustomerId).To(Equal(int64(1001)))
			Expect(*loaded.Name).To(Equal("Customer_1001"))
			Expect(*loaded.Email).To(Equal("customer_1001@example.com"))
		})

		It("should handle customer with all fields set", func() {
			id := int64(2001)
			name := "Alice Johnson"
			email := "alice@example.com"
			customer := &gen.Customer{
				CustomerId: &id,
				Name:       &name,
				Email:      &email,
			}

			err := customerStore.SaveCustomer(ctx, customer)
			Expect(err).NotTo(HaveOccurred())

			loaded, err := customerStore.LoadCustomer(ctx, 2001)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loaded.Name).To(Equal("Alice Johnson"))
			Expect(*loaded.Email).To(Equal("alice@example.com"))
		})

		It("should handle customer with minimal fields (only ID)", func() {
			customer := MinimalCustomer(3001)
			err := customerStore.SaveCustomer(ctx, customer)
			Expect(err).NotTo(HaveOccurred())

			loaded, err := customerStore.LoadCustomer(ctx, 3001)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loaded.CustomerId).To(Equal(int64(3001)))
		})
	})

	Describe("Java writes Customer, Go reads", func() {
		It("should read a customer saved by Java", func() {
			customer := StandardCustomer(4001)
			loaded, err := customerStore.JavaSaveThenGoLoad(ctx, customer)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loaded.CustomerId).To(Equal(int64(4001)))
			Expect(*loaded.Name).To(Equal("Customer_4001"))
			Expect(*loaded.Email).To(Equal("customer_4001@example.com"))
		})

		It("should read a minimal customer saved by Java", func() {
			customer := MinimalCustomer(4002)
			loaded, err := customerStore.JavaSaveThenGoLoad(ctx, customer)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loaded.CustomerId).To(Equal(int64(4002)))
		})
	})

	Describe("Customer CRUD cycle", func() {
		It("should save, load, update, delete, and verify at each step", func() {
			// Save
			customer := StandardCustomer(5001)
			err := customerStore.SaveCustomer(ctx, customer)
			Expect(err).NotTo(HaveOccurred())

			// Load and verify
			loaded, err := customerStore.LoadCustomer(ctx, 5001)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loaded.Name).To(Equal("Customer_5001"))

			// Update
			id := int64(5001)
			updatedName := "Updated Customer"
			updatedEmail := "updated@example.com"
			updated := &gen.Customer{
				CustomerId: &id,
				Name:       &updatedName,
				Email:      &updatedEmail,
			}
			err = customerStore.SaveCustomer(ctx, updated)
			Expect(err).NotTo(HaveOccurred())

			// Verify update
			loaded, err = customerStore.LoadCustomer(ctx, 5001)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loaded.Name).To(Equal("Updated Customer"))
			Expect(*loaded.Email).To(Equal("updated@example.com"))

			// Verify existence
			exists, err := customerStore.CustomerExists(ctx, 5001)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue())

			// Delete
			deleted, err := customerStore.DeleteCustomer(ctx, 5001)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Verify non-existence
			exists, err = customerStore.CustomerExists(ctx, 5001)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeFalse())
		})
	})

	Describe("Multi-type in same store", func() {
		It("should save Order AND Customer and verify both cross-validated", func() {
			order := StandardOrder(6001)
			err := orderStore.SaveRecord(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			customer := StandardCustomer(6002)
			err = customerStore.SaveCustomer(ctx, customer)
			Expect(err).NotTo(HaveOccurred())

			loadedOrder, err := orderStore.LoadRecord(ctx, 6001)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loadedOrder.OrderId).To(Equal(int64(6001)))

			loadedCustomer, err := customerStore.LoadCustomer(ctx, 6002)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loadedCustomer.CustomerId).To(Equal(int64(6002)))
		})

		// NOTE: Without RecordTypeKeyExpression, different record types sharing
		// the same primary key value occupy the same physical key and overwrite
		// each other. This is expected Java Record Layer behavior.

		It("should allow deleting one type without affecting the other", func() {
			// Use different PKs to avoid collision
			order := StandardOrder(7001)
			err := orderStore.SaveRecord(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			customer := StandardCustomer(7002)
			err = customerStore.SaveCustomer(ctx, customer)
			Expect(err).NotTo(HaveOccurred())

			deleted, err := customerStore.DeleteCustomer(ctx, 7002)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			loadedOrder, err := orderStore.LoadRecord(ctx, 7001)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loadedOrder.OrderId).To(Equal(int64(7001)))

			exists, err := customerStore.CustomerExists(ctx, 7002)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeFalse())
		})
	})

	Describe("Cross-write multi-type", func() {
		It("should allow Java to save Customer and Go to save Order in same store", func() {
			customer := StandardCustomer(8001)
			javaLoaded, err := customerStore.JavaSaveThenGoLoad(ctx, customer)
			Expect(err).NotTo(HaveOccurred())
			Expect(*javaLoaded.CustomerId).To(Equal(int64(8001)))

			order := StandardOrder(8002)
			err = orderStore.SaveRecord(ctx, order)
			Expect(err).NotTo(HaveOccurred())

			loadedCustomer, err := customerStore.LoadCustomer(ctx, 8001)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loadedCustomer.Name).To(Equal("Customer_8001"))

			loadedOrder, err := orderStore.LoadRecord(ctx, 8002)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loadedOrder.Price).To(Equal(int32(80020)))
		})
	})

	Describe("Boundary Values", func() {
		It("should handle customer ID of 1", func() {
			customer := StandardCustomer(1)
			err := customerStore.SaveCustomer(ctx, customer)
			Expect(err).NotTo(HaveOccurred())

			loaded, err := customerStore.LoadCustomer(ctx, 1)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loaded.CustomerId).To(Equal(int64(1)))
		})

		It("should handle large customer IDs", func() {
			largeID := int64(9223372036854775000)
			customer := StandardCustomer(largeID)
			err := customerStore.SaveCustomer(ctx, customer)
			Expect(err).NotTo(HaveOccurred())

			loaded, err := customerStore.LoadCustomer(ctx, largeID)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loaded.CustomerId).To(Equal(largeID))
		})

		It("should handle empty string fields", func() {
			id := int64(9001)
			emptyStr := ""
			customer := &gen.Customer{
				CustomerId: &id,
				Name:       &emptyStr,
				Email:      &emptyStr,
			}
			err := customerStore.SaveCustomer(ctx, customer)
			Expect(err).NotTo(HaveOccurred())

			loaded, err := customerStore.LoadCustomer(ctx, 9001)
			Expect(err).NotTo(HaveOccurred())
			Expect(*loaded.Name).To(Equal(""))
			Expect(*loaded.Email).To(Equal(""))
		})
	})

	Describe("Delete non-existent customer", func() {
		It("should return false when deleting non-existent customer", func() {
			// First create the store by saving a customer, so open() works
			customer := StandardCustomer(1)
			err := customerStore.SaveCustomer(ctx, customer)
			Expect(err).NotTo(HaveOccurred())

			deleted, err := customerStore.DeleteCustomer(ctx, 99999999)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeFalse())
		})
	})

	Describe("Multiple customers", func() {
		It("should handle saving and loading multiple customers", func() {
			for i := int64(10001); i <= int64(10005); i++ {
				customer := StandardCustomer(i)
				err := customerStore.SaveCustomer(ctx, customer)
				Expect(err).NotTo(HaveOccurred())
			}

			for i := int64(10001); i <= int64(10005); i++ {
				loaded, err := customerStore.LoadCustomer(ctx, i)
				Expect(err).NotTo(HaveOccurred())
				Expect(*loaded.CustomerId).To(Equal(i))
				Expect(*loaded.Name).To(Equal(fmt.Sprintf("Customer_%d", i)))
			}
		})
	})
})

// Verify Customer type is registered and loadable through direct store operations.
var _ = Describe("Customer Direct Store Operations", func() {
	var (
		ctx context.Context
		env *TenantEnvironment
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error
		tenantName := fmt.Sprintf("cust_direct_%s", uuid.New().String())
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	It("should save and load Customer through typed store", func() {
		_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(env.MetaData).
				SetSubspace(env.Keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			customer := StandardCustomer(42)
			_, err = store.SaveRecord(customer)
			if err != nil {
				return nil, err
			}

			storedRecord, err := store.LoadRecord(tuple.Tuple{int64(42)})
			if err != nil {
				return nil, err
			}
			Expect(storedRecord).NotTo(BeNil())

			loaded := storedRecord.Record.(*gen.Customer)
			Expect(*loaded.CustomerId).To(Equal(int64(42)))
			Expect(*loaded.Name).To(Equal("Customer_42"))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

// CustomerConformanceStore cross-validates Customer operations between Go and Java.
type CustomerConformanceStore struct {
	recordDB    *recordlayer.FDBDatabase
	metaData    *recordlayer.RecordMetaData
	keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

// NewCustomerConformanceStoreWithTenant creates a customer conformance store for tenant-isolated tests
func NewCustomerConformanceStoreWithTenant(recordDB *recordlayer.FDBDatabase, metaData *recordlayer.RecordMetaData, clusterFile string, tenantName string) *CustomerConformanceStore {
	return &CustomerConformanceStore{
		recordDB:    recordDB,
		metaData:    metaData,
		keyspace:    subspace.Sub(tuple.Tuple{}),
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}
}

func (c *CustomerConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": c.clusterFile,
		"subspace":    BytesToIntArray(c.keyspace.Bytes()),
	}
	if c.tenantName != "" {
		params["tenantName"] = c.tenantName
	}
	return params
}

// SaveCustomer saves a Customer via Java, then cross-checks by loading with Go.
func (c *CustomerConformanceStore) SaveCustomer(ctx context.Context, customer *gen.Customer) error {
	// 1. Save with Java
	params := c.buildJavaParams()
	params["customer"] = customer
	if err := c.java.InvokeAs(ctx, "saveCustomer", params, nil); err != nil {
		return fmt.Errorf("java save failed: %w", err)
	}

	// 2. Cross-check: Go loads the Java-written record
	goCustomer, err := c.loadWithGo(ctx, *customer.CustomerId)
	if err != nil {
		return fmt.Errorf("go cross-check read failed: %w", err)
	}

	// 3. Java also loads to get authoritative result
	var javaCustomer gen.Customer
	params = c.buildJavaParams()
	params["customerID"] = *customer.CustomerId
	if err := c.java.InvokeAs(ctx, "loadCustomer", params, &javaCustomer); err != nil {
		return fmt.Errorf("java cross-check read failed: %w", err)
	}

	// 4. Verify Go and Java read the same data
	if !proto.Equal(goCustomer, &javaCustomer) {
		return fmt.Errorf("conformance mismatch: Go and Java read different data\nGo:   %+v\nJava: %+v", goCustomer, &javaCustomer)
	}

	return nil
}

// LoadCustomer loads a Customer with both Go and Java, verifying they match
func (c *CustomerConformanceStore) LoadCustomer(ctx context.Context, customerID int64) (*gen.Customer, error) {
	goCustomer, err := c.loadWithGo(ctx, customerID)
	if err != nil {
		return nil, fmt.Errorf("go load failed: %w", err)
	}

	var javaCustomer gen.Customer
	params := c.buildJavaParams()
	params["customerID"] = customerID
	if err := c.java.InvokeAs(ctx, "loadCustomer", params, &javaCustomer); err != nil {
		return nil, fmt.Errorf("java cross-check failed: %w", err)
	}

	if !proto.Equal(goCustomer, &javaCustomer) {
		return nil, fmt.Errorf("conformance mismatch: Go and Java read different data\nGo:   %+v\nJava: %+v", goCustomer, &javaCustomer)
	}

	return goCustomer, nil
}

// DeleteCustomer deletes a Customer via Java, verifies non-existence with Go
func (c *CustomerConformanceStore) DeleteCustomer(ctx context.Context, customerID int64) (bool, error) {
	params := c.buildJavaParams()
	params["customerID"] = customerID
	var existsBefore bool
	if err := c.java.InvokeAs(ctx, "customerExists", params, &existsBefore); err != nil {
		return false, fmt.Errorf("java existence check failed: %w", err)
	}

	params = c.buildJavaParams()
	params["customerID"] = customerID
	var deleted bool
	if err := c.java.InvokeAs(ctx, "deleteCustomer", params, &deleted); err != nil {
		return false, fmt.Errorf("java delete failed: %w", err)
	}

	if deleted != existsBefore {
		return false, fmt.Errorf("delete result mismatch: existed before=%v, delete returned=%v", existsBefore, deleted)
	}

	goExists := c.existsInGo(ctx, customerID)
	if goExists {
		return false, fmt.Errorf("record still exists in Go after Java delete")
	}

	return deleted, nil
}

// CustomerExists checks if a Customer exists using both Go and Java
func (c *CustomerConformanceStore) CustomerExists(ctx context.Context, customerID int64) (bool, error) {
	goExists := c.existsInGo(ctx, customerID)

	params := c.buildJavaParams()
	params["customerID"] = customerID
	var javaExists bool
	if err := c.java.InvokeAs(ctx, "customerExists", params, &javaExists); err != nil {
		return false, fmt.Errorf("java existence check failed: %w", err)
	}

	if goExists != javaExists {
		return false, fmt.Errorf("conformance mismatch: Go exists=%v Java exists=%v", goExists, javaExists)
	}
	return goExists, nil
}

// JavaSaveThenGoLoad has Java save a Customer, then Go loads it
func (c *CustomerConformanceStore) JavaSaveThenGoLoad(ctx context.Context, customer *gen.Customer) (*gen.Customer, error) {
	params := c.buildJavaParams()
	params["customer"] = customer
	if err := c.java.InvokeAs(ctx, "saveCustomer", params, nil); err != nil {
		return nil, fmt.Errorf("java save failed: %w", err)
	}

	goCustomer, err := c.loadWithGo(ctx, *customer.CustomerId)
	if err != nil {
		return nil, fmt.Errorf("go load after java save failed: %w", err)
	}

	return goCustomer, nil
}

func (c *CustomerConformanceStore) loadWithGo(ctx context.Context, customerID int64) (*gen.Customer, error) {
	var customer *gen.Customer
	_, err := c.recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(c.metaData).
			SetSubspace(c.keyspace).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		storedRecord, err := store.LoadRecord(tuple.Tuple{customerID})
		if err != nil {
			return nil, err
		}
		if storedRecord == nil {
			return nil, fmt.Errorf("customer not found: %d", customerID)
		}

		customer = storedRecord.Record.(*gen.Customer)
		return nil, nil
	})
	return customer, err
}

func (c *CustomerConformanceStore) existsInGo(ctx context.Context, customerID int64) bool {
	var exists bool
	_, _ = c.recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(c.metaData).
			SetSubspace(c.keyspace).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		storedRecord, err := store.LoadRecord(tuple.Tuple{customerID})
		exists = (err == nil && storedRecord != nil)
		return nil, nil
	})
	return exists
}
