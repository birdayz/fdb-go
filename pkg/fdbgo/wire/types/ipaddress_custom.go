package types

// IPAddress has custom serialize() logic.
// Port the C++ serialize() method to Go.
// Use the generated struct, slot constants, and template from ipaddress_generated.go.

func (m *IPAddress) UnmarshalFDB(data []byte) error {
	panic("IPAddress.UnmarshalFDB not implemented")
}
