package types

// SpanContext has custom serialize() logic.
// Port the C++ serialize() method to Go.
// Use the generated struct, slot constants, and template from spancontext_generated.go.

func (m *SpanContext) UnmarshalFDB(data []byte) error {
	panic("SpanContext.UnmarshalFDB not implemented")
}
