// Portions derived from FoundationDB Record Layer (VectorType.java, Type.java),
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2026 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package values

import "fmt"

// VectorType is Java Type.Vector: precision and dimensions are part of identity.
type VectorType struct {
	Nullable   bool
	Precision  int
	Dimensions int
}

func NewVectorType(nullable bool, precision, dimensions int) *VectorType {
	return &VectorType{Nullable: nullable, Precision: precision, Dimensions: dimensions}
}
func (*VectorType) Code() TypeCode     { return TypeCodeVector }
func (v *VectorType) IsNullable() bool { return v.Nullable }
func (v *VectorType) Equals(other Type) bool {
	o, ok := other.(*VectorType)
	return ok && o != nil && v.Nullable == o.Nullable && v.Precision == o.Precision && v.Dimensions == o.Dimensions
}

func (v *VectorType) String() string { return fmt.Sprintf("VECTOR(%d, %d)", v.Precision, v.Dimensions) }
