package metadata

import (
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
)

// RecordLayerColumn adapts a single protobuf field descriptor to the
// api.Column interface. Zero value is not useful — build via
// newColumn().
type RecordLayerColumn struct {
	fd   protoreflect.FieldDescriptor
	name string
	dt   api.DataType
}

// newColumn constructs a column from a proto field. Fails if the
// field's type cannot be bridged (unsupported proto kind).
func newColumn(fd protoreflect.FieldDescriptor) (*RecordLayerColumn, error) {
	dt, err := protoFieldToDataType(fd)
	if err != nil {
		return nil, err
	}
	return &RecordLayerColumn{fd: fd, name: string(fd.Name()), dt: dt}, nil
}

// MetadataName returns the column name.
func (c *RecordLayerColumn) MetadataName() string { return c.name }

// DataType returns the column's SQL type.
func (c *RecordLayerColumn) DataType() api.DataType { return c.dt }

// Accept dispatches into the Visitor.
func (c *RecordLayerColumn) Accept(v api.Visitor) { v.VisitColumn(c) }

// FieldDescriptor exposes the underlying proto field descriptor for
// callers that need the raw proto view (e.g. wire-format decoding).
// Not part of api.Column.
func (c *RecordLayerColumn) FieldDescriptor() protoreflect.FieldDescriptor { return c.fd }
