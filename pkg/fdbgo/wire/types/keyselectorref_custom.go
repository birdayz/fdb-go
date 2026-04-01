package types

// KeySelectorRef has custom serialize() logic.
// Port the C++ serialize() method to Go.
// Use the generated struct, slot constants, and template from keyselectorref_generated.go.

func (m *KeySelectorRef) UnmarshalFDB(data []byte) error {
	panic("KeySelectorRef.UnmarshalFDB not implemented")
}
