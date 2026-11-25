// Portions derived from FoundationDB Record Layer (RealVector.java),
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package api

// Vector is a prepared-statement VECTOR parameter in Java's serialized
// RealVector format. Unlike []byte (BYTES), it retains vector typing when the
// parameter is compared or projected without a target column.
type Vector []byte
