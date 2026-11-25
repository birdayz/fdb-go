package metadata

import "fdb.dev/internal/javahashmap"

// javaHashMapOrder is javahashmap.Order.
func javaHashMapOrder(first, then []string) []string { return javahashmap.Order(first, then) }
