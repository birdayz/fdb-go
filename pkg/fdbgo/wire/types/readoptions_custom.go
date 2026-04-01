package types

// ReadOptions has custom serialize() logic.
// Port the C++ serialize() method to Go.
// Use the generated struct, slot constants, and template from readoptions_generated.go.

func (m *ReadOptions) UnmarshalFDB(data []byte) error {
	panic("ReadOptions.UnmarshalFDB not implemented")
}
