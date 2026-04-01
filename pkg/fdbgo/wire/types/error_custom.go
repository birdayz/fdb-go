package types

// Error has custom serialize() logic.
// Port the C++ serialize() method to Go.
// Use the generated struct, slot constants, and template from error_generated.go.

func (m *Error) MarshalFDB() []byte {
	panic("Error.MarshalFDB not implemented")
}
