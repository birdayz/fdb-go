package types

// StorageServerInterface has custom serialize() logic.
// Port the C++ serialize() method to Go.
// Use the generated struct, slot constants, and template from storageserverinterface_generated.go.

func (m *StorageServerInterface) MarshalFDB() []byte {
	panic("StorageServerInterface.MarshalFDB not implemented")
}
