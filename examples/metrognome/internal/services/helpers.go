package services

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/storage"
)

// newID generates a prefixed random ID (e.g. "cust_a1b2c3d4e5f6").
func newID(prefix string) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return fmt.Sprintf("%s_%s", prefix, hex.EncodeToString(b))
}

// storageError converts storage-layer errors to connect errors.
func storageError(entity string, err error) *connect.Error {
	if errors.Is(err, storage.ErrNotFound) {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("%s not found", entity))
	}
	if errors.Is(err, storage.ErrAlreadyExists) {
		return connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("%s already exists", entity))
	}
	return connect.NewError(connect.CodeInternal, err)
}
