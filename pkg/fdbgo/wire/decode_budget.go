package wire

import "fmt"

// DecodeBudget bounds the vector-of-table elements one decode may produce. C++
// writes each element's 4-byte offset once, so a buffer it wrote holds at most
// len/4 elements in total; more means offsets alias the same elements.
type DecodeBudget struct {
	left int
}

// NewDecodeBudget returns the budget for decoding data.
func NewDecodeBudget(data []byte) DecodeBudget {
	return DecodeBudget{left: len(data) / 4}
}

// Take charges n elements and fails once the decode would exceed the budget.
func (b *DecodeBudget) Take(n int) error {
	if n > b.left {
		return &DecodeBudgetError{Elements: n, Left: b.left}
	}
	b.left -= n
	return nil
}

// Left returns the elements the budget still covers.
func (b *DecodeBudget) Left() int { return b.left }

// DecodeBudgetError reports a vector the decode budget cannot cover.
type DecodeBudgetError struct {
	Elements int // elements the vector declares
	Left     int // elements the budget had left
}

func (e *DecodeBudgetError) Error() string {
	return fmt.Sprintf("wire: vector of %d elements exceeds the decode budget (%d left)", e.Elements, e.Left)
}

// DecodeError reports a field a generated decoder could not decode. Field is empty
// for a vector decoded on its own.
type DecodeError struct {
	Type  string
	Field string
	Err   error
}

func (e *DecodeError) Error() string {
	if e.Field == "" {
		return fmt.Sprintf("wire: decode %s: %v", e.Type, e.Err)
	}
	return fmt.Sprintf("wire: decode %s.%s: %v", e.Type, e.Field, e.Err)
}

func (e *DecodeError) Unwrap() error { return e.Err }

// FieldBoundsError reports a present field whose bytes lie outside the buffer, or
// a vtable entry that does.
type FieldBoundsError struct {
	Slot int
}

func (e *FieldBoundsError) Error() string {
	return fmt.Sprintf("wire: field in slot %d lies outside the buffer", e.Slot)
}

// StringVectorError reports a VecSerStrategy::String vector whose elements do not
// exactly fill its blob.
type StringVectorError struct {
	Count uint32 // elements the vector declares
	Len   int    // blob length
}

func (e *StringVectorError) Error() string {
	return fmt.Sprintf("wire: string vector of %d elements does not fill its %d-byte blob", e.Count, e.Len)
}
