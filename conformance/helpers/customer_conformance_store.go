package helpers

import (
	"context"
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"google.golang.org/protobuf/proto"
)

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

func (c *CustomerConformanceStore) buildJavaParams() map[string]interface{} {
	params := map[string]interface{}{
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
	_, err := c.recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
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
	_, _ = c.recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
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
