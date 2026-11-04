package client

import (
	"sync"
	"testing"
)

// TestTransactionDefaults_ConcurrentSetAndApply pins the concurrency contract the
// `database` doc comment asserts ("Safe for concurrent use by multiple goroutines
// after creation") for the database-level transaction defaults.
//
// The SetTransaction*/SetDefault* setters are exported on *Database, so an embedder
// may set a default from one goroutine while another creates a transaction. libfdb_c
// is immune by construction — fdb_database_set_option and transaction creation are
// both marshalled onto the single network thread — so the Go client has to serialize
// them explicitly. The fdb facade also forwards its persistent database defaults
// here, so this covers concurrent facade option changes and native construction/reset.
//
// Only fails under -race.
func TestTransactionDefaults_ConcurrentSetAndApply(t *testing.T) {
	t.Parallel()
	db := &Database{db: &database{}}

	const iterations = 200
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			db.SetTransactionTimeout(int64(i%7) + 1)
			db.SetTransactionRetryLimit(int64(i % 5))
			db.SetTransactionMaxRetryDelay(int64(i%3) + 1)
			db.SetTransactionSizeLimit(int64(i%11) + 64)
			db.SetDefaultReadSystemKeys()
			db.SetDefaultAccessSystemKeys()
		}
	}()

	// Both readers of the defaults: transaction creation and the reset/retry path.
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			tx := db.CreateTransaction()
			tx.applyOptionDefaults(i%2 == 0)
		}
	}()

	wg.Wait()
}
