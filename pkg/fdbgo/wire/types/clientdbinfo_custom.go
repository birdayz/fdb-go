package types

// ClientDBInfo has custom serialize() logic.
// Port the C++ serialize() method to Go.
// Use the generated struct, slot constants, and template from clientdbinfo_generated.go.

func (m *ClientDBInfo) UnmarshalFDB(data []byte) error {
	panic("ClientDBInfo.UnmarshalFDB not implemented")
}

func (m *ClientDBInfo) MarshalFDB() []byte {
	panic("ClientDBInfo.MarshalFDB not implemented")
}
