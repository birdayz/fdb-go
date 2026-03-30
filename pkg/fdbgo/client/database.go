package client

import (
	"context"
	"fmt"
)

// Database represents a connection to an FDB cluster.
// It manages cluster topology and provides transaction creation.
type Database struct {
	cluster *Cluster
}

// OpenDatabase opens a database connection using a cluster file.
func OpenDatabase(clusterFilePath string) (*Database, error) {
	cluster, err := NewCluster(clusterFilePath)
	if err != nil {
		return nil, err
	}

	ctx := context.Background()
	if err := cluster.Connect(ctx); err != nil {
		cluster.Close()
		return nil, fmt.Errorf("connect to cluster: %w", err)
	}

	return &Database{cluster: cluster}, nil
}

// Transact runs a function in a transaction with automatic retry.
// This is the primary API for interacting with FDB.
func (db *Database) Transact(ctx context.Context, fn func(tx *Transaction) (interface{}, error)) (interface{}, error) {
	for {
		tx := db.CreateTransaction()

		result, err := fn(tx)
		if err != nil {
			retryable := tx.OnError(err)
			if retryable != nil {
				return nil, retryable // non-retryable error
			}
			continue // retry
		}

		if err := tx.Commit(ctx); err != nil {
			retryable := tx.OnError(err)
			if retryable != nil {
				return nil, retryable
			}
			continue
		}

		return result, nil
	}
}

// CreateTransaction creates a new transaction.
func (db *Database) CreateTransaction() *Transaction {
	return &Transaction{
		db:    db,
		state: txStateActive,
	}
}

// Close closes the database connection.
func (db *Database) Close() error {
	return db.cluster.Close()
}
