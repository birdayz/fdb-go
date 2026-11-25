package javanum

// IllegalArgumentError is Java's IllegalArgumentException. It lives here, below
// both the record layer and the R-tree, so either can raise it and errors.As
// matches one type.
type IllegalArgumentError struct {
	Message string
	Cause   error
}

func (e *IllegalArgumentError) Error() string { return e.Message }

func (e *IllegalArgumentError) Unwrap() error { return e.Cause }
