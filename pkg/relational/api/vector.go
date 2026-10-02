package api

// Vector is a prepared-statement VECTOR parameter in Java's serialized
// RealVector format. Unlike []byte (BYTES), it retains vector typing when the
// parameter is compared or projected without a target column.
type Vector []byte
