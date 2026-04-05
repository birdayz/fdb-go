package fdb

import "fmt"

// Error represents a low-level error returned by FoundationDB.
// Compare the Code field against FDB error codes, or pass to
// Transaction.OnError to determine if the error is retryable.
type Error struct {
	Code int
}

func (e Error) Error() string {
	return fmt.Sprintf("FoundationDB error: %d", e.Code)
}
