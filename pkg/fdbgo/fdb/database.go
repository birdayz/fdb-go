package fdb

import (
	"context"
	"sync/atomic"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/client"
)

var apiVersion atomic.Int32

// MustAPIVersion is a convenience function equivalent to APIVersion that
// panics on error. Matches Apple's fdb.MustAPIVersion.
func MustAPIVersion(version int) {
	if err := APIVersion(version); err != nil {
		panic(err)
	}
}

// APIVersion must be called before any other fdb function. It selects the
// FDB API version to use. The pure Go client supports a broad range of
// API versions but does not enforce version-specific behavior differences.
func APIVersion(version int) error {
	if version < 510 {
		return Error{Code: 2201} // api_version_not_supported
	}
	v := int32(version)
	// Atomic set-if-unset. Reject re-set with a different version.
	if !apiVersion.CompareAndSwap(0, v) {
		if apiVersion.Load() != v {
			return Error{Code: 2201}
		}
	}
	return nil
}

// GetAPIVersion returns the API version that has been selected, or 0 if
// none has been selected.
func GetAPIVersion() (int, error) {
	v := apiVersion.Load()
	if v == 0 {
		return 0, Error{Code: 2200} // api_version_unset
	}
	return int(v), nil
}

// internalDB wraps client.Database with a context for async operations.
type internalDB struct {
	inner *client.Database
	ctx   context.Context
}

// Database is a handle to a FoundationDB database.
// Database is safe for concurrent use by multiple goroutines.
type Database struct {
	d *internalDB
}

// OpenDatabase opens a connection using the specified cluster file path.
// APIVersion must have been called first.
func OpenDatabase(clusterFile string) (Database, error) {
	if apiVersion.Load() == 0 {
		return Database{}, Error{Code: 2200} // api_version_unset
	}
	ctx := context.Background()
	db, err := client.OpenDatabase(ctx, clusterFile)
	if err != nil {
		return Database{}, err
	}
	return Database{d: &internalDB{inner: db, ctx: ctx}}, nil
}

// OpenWithConnectionString opens a connection using a cluster connection string.
func OpenWithConnectionString(_ string) (Database, error) {
	// TODO: connection string support
	return Database{}, Error{Code: 2051} // not yet implemented
}

// OpenDatabaseFromConfig creates a Database from a client.ClusterFile.
// The provided ctx is used only for the initial bootstrap (coordinator
// connection). The Database uses context.Background() for ongoing operations.
func OpenDatabaseFromConfig(ctx context.Context, cf *client.ClusterFile) (Database, error) {
	db, err := client.OpenDatabaseFromConfig(ctx, cf, nil)
	if err != nil {
		return Database{}, err
	}
	return Database{d: &internalDB{inner: db, ctx: context.Background()}}, nil
}

// MustOpenDefault opens the default database or panics.
func MustOpenDefault() Database {
	db, err := OpenDatabase("/etc/foundationdb/fdb.cluster")
	if err != nil {
		panic(err)
	}
	return db
}

// MustOpenDatabase opens a database or panics.
func MustOpenDatabase(clusterFile string) Database {
	db, err := OpenDatabase(clusterFile)
	if err != nil {
		panic(err)
	}
	return db
}

// Close closes the database connection.
func (db Database) Close() {
	if db.d != nil && db.d.inner != nil {
		db.d.inner.Close()
	}
}

// CreateTransaction creates a new Transaction.
func (db Database) CreateTransaction() (Transaction, error) {
	tx := db.d.inner.CreateTransaction()
	return Transaction{t: &transaction{
		inner: tx,
		db:    db,
		ctx:   db.d.ctx,
	}}, nil
}

// Transact runs a transactional function with automatic retry.
func (db Database) Transact(f func(Transaction) (any, error)) (any, error) {
	result, err := db.d.inner.Transact(db.d.ctx, func(tx *client.Transaction) (any, error) {
		tr := Transaction{t: &transaction{
			inner: tx,
			db:    db,
			ctx:   db.d.ctx,
		}}
		return f(tr)
	})
	if err != nil {
		return nil, convertError(err)
	}
	return result, nil
}

// ReadTransact runs a read-only transactional function with automatic retry.
func (db Database) ReadTransact(f func(ReadTransaction) (any, error)) (any, error) {
	result, err := db.d.inner.ReadTransact(db.d.ctx, func(tx *client.Transaction) (any, error) {
		tr := Transaction{t: &transaction{
			inner: tx,
			db:    db,
			ctx:   db.d.ctx,
		}}
		return f(tr)
	})
	if err != nil {
		return nil, convertError(err)
	}
	return result, nil
}

// Options returns a DatabaseOptions handle (currently a no-op).
func (db Database) Options() DatabaseOptions {
	return DatabaseOptions{}
}

// Tenant operations (stubs).

// OpenTenant opens a named tenant on this database.
func (db Database) OpenTenant(_ KeyConvertible) (Tenant, error) {
	return Tenant{}, Error{Code: 2051}
}

func (db Database) CreateTenant(_ KeyConvertible) error {
	return Error{Code: 2051}
}

func (db Database) DeleteTenant(_ KeyConvertible) error {
	return Error{Code: 2051}
}

func (db Database) ListTenants() ([]Key, error) {
	return nil, Error{Code: 2051}
}

// GetClientStatus is not yet implemented.
func (db Database) GetClientStatus() ([]byte, error) {
	return nil, Error{Code: 2051}
}

// LocalityGetBoundaryKeys is not yet implemented.
func (db Database) LocalityGetBoundaryKeys(_ ExactRange, _ int, _ int64) ([]Key, error) {
	return nil, Error{Code: 2051}
}

// RebootWorker is not yet implemented.
func (db Database) RebootWorker(_ string, _ bool, _ int) error {
	return Error{Code: 2051}
}
