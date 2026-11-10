// Package partition implements the interval coordinate shared by stored keys and planner values.
package partition

import "fmt"

// RoutingError reports a value that cannot be routed to an interval.
type RoutingError struct {
	Value  any
	Width  int64
	Reason string
}

func (e *RoutingError) Error() string {
	return fmt.Sprintf("partition routing: %s (value=%v, width=%d)", e.Reason, e.Value, e.Width)
}

// Coordinate returns mathematical floor(value / width), without computing interval endpoints.
func Coordinate(value, width int64) (int64, error) {
	if width <= 0 {
		return 0, &RoutingError{Value: value, Width: width, Reason: "interval width must be positive"}
	}
	p := value / width
	if value%width < 0 {
		p--
	}
	return p, nil
}

// Route accepts only non-NULL BIGINT values, unlike the coercing arithmetic functions.
func Route(value any, width int64) (int64, error) {
	x, ok := value.(int64)
	if !ok {
		return 0, &RoutingError{Value: value, Width: width, Reason: "source must be a non-NULL BIGINT"}
	}
	return Coordinate(x, width)
}
