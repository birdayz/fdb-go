package javanum

// IllegalStateError is Java's IllegalStateException. It lives here, below both
// the record layer and the R-tree, so either can raise it and errors.As matches
// one type.
type IllegalStateError struct {
	Message string
}

func (e *IllegalStateError) Error() string { return e.Message }
