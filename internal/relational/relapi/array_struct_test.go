package api

// Compile-time stubs for Array / Struct / their metadata interfaces.
// See connection_test.go for the same pattern on Connection /
// Statement / ResultSet. Catches drift when interfaces gain methods.

type arrayStub struct{}

func (*arrayStub) MetaData() ArrayMetaData  { return nil }
func (*arrayStub) BaseType() int            { return 0 }
func (*arrayStub) BaseTypeName() string     { return "" }
func (*arrayStub) Length() int              { return 0 }
func (*arrayStub) Element(int) (any, error) { return nil, nil }
func (*arrayStub) Elements() []any          { return nil }

type arrayMDStub struct{}

func (*arrayMDStub) ElementType() int          { return 0 }
func (*arrayMDStub) ElementTypeName() string   { return "" }
func (*arrayMDStub) ElementDataType() DataType { return nil }
func (*arrayMDStub) Nullable() int             { return 0 }

type structStub struct{}

func (*structStub) MetaData() StructMetaData            { return nil }
func (*structStub) AttributeCount() int                 { return 0 }
func (*structStub) Attribute(int) (any, error)          { return nil, nil }
func (*structStub) AttributeByName(string) (any, error) { return nil, nil }
func (*structStub) Attributes() []any                   { return nil }

type structMDStub struct{}

func (*structMDStub) TypeName() string                        { return "" }
func (*structMDStub) AttributeCount() int                     { return 0 }
func (*structMDStub) AttributeName(int) (string, error)       { return "", nil }
func (*structMDStub) AttributeType(int) (int, error)          { return 0, nil }
func (*structMDStub) AttributeTypeName(int) (string, error)   { return "", nil }
func (*structMDStub) AttributeDataType(int) (DataType, error) { return nil, nil }
func (*structMDStub) AttributeNullable(int) (int, error)      { return 0, nil }

var (
	_ Array          = (*arrayStub)(nil)
	_ ArrayMetaData  = (*arrayMDStub)(nil)
	_ Struct         = (*structStub)(nil)
	_ StructMetaData = (*structMDStub)(nil)
)
