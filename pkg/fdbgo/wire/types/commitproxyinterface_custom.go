package types

// CommitProxyInterface has custom serialize() logic.
// Port the C++ serialize() method to Go.
// Use the generated struct, slot constants, and template from commitproxyinterface_generated.go.

func (m *CommitProxyInterface) UnmarshalFDB(data []byte) error {
	panic("CommitProxyInterface.UnmarshalFDB not implemented")
}

func (m *CommitProxyInterface) MarshalFDB() []byte {
	panic("CommitProxyInterface.MarshalFDB not implemented")
}
