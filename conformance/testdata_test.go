//go:build bazelrunfiles

package conformance_test

import (
	"fmt"

	"fdb.dev/gen/demopb"
)

// OrderBuilder provides a fluent interface for building test Order records
type OrderBuilder struct {
	order *demopb.Order
}

// NewOrder creates a new OrderBuilder with the given order ID
func NewOrder(orderID int64) *OrderBuilder {
	return &OrderBuilder{
		order: &demopb.Order{OrderId: &orderID},
	}
}

// WithPrice sets the price for the order
func (b *OrderBuilder) WithPrice(price int32) *OrderBuilder {
	b.order.Price = &price
	return b
}

// WithTags sets the tags for the order (repeated string field)
func (b *OrderBuilder) WithTags(tags ...string) *OrderBuilder {
	b.order.Tags = tags
	return b
}

// WithFlower sets the flower type and color for the order
func (b *OrderBuilder) WithFlower(flowerType string, color demopb.Color) *OrderBuilder {
	b.order.Flower = &demopb.Flower{
		Type:  &flowerType,
		Color: &color,
	}
	return b
}

// Build returns the constructed Order
func (b *OrderBuilder) Build() *demopb.Order {
	return b.order
}

// StandardOrder creates a standard test order with predictable values based on the ID
// Price is derived from the last 5 digits of ID (* 10), keeping it within int32 range.
// Flower = "Rose_{ID}" with RED color
func StandardOrder(id int64) *demopb.Order {
	price := int32((id % 100000) * 10)
	flowerType := fmt.Sprintf("Rose_%d", id)
	return NewOrder(id).
		WithPrice(price).
		WithFlower(flowerType, demopb.Color_RED).
		Build()
}

// StandardOrders creates a slice of standard test orders with sequential IDs
func StandardOrders(startID, count int64) []*demopb.Order {
	orders := make([]*demopb.Order, count)
	for i := int64(0); i < count; i++ {
		orders[i] = StandardOrder(startID + i)
	}
	return orders
}

// MinimalOrder creates an order with only the order ID set (minimal valid record)
func MinimalOrder(id int64) *demopb.Order {
	return &demopb.Order{OrderId: &id}
}

// StandardCustomer creates a standard test customer with predictable values based on the ID
func StandardCustomer(id int64) *demopb.Customer {
	name := fmt.Sprintf("Customer_%d", id)
	email := fmt.Sprintf("customer_%d@example.com", id)
	return &demopb.Customer{
		CustomerId: &id,
		Name:       &name,
		Email:      &email,
	}
}

// MinimalCustomer creates a customer with only the customer ID set (minimal valid record)
func MinimalCustomer(id int64) *demopb.Customer {
	return &demopb.Customer{CustomerId: &id}
}
