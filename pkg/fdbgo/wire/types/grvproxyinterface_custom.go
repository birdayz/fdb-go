package types

// GrvProxyInterface has custom serialize() logic.
// Port the C++ serialize() method to Go.
// Use the generated struct, slot constants, and template from grvproxyinterface_generated.go.

func (m *GrvProxyInterface) UnmarshalFDB(data []byte) error {
	panic("GrvProxyInterface.UnmarshalFDB not implemented")
}

func (m *GrvProxyInterface) MarshalFDB() []byte {
	panic("GrvProxyInterface.MarshalFDB not implemented")
}
