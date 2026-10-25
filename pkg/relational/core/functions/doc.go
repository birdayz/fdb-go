// Package functions holds SQL helpers that need no connection or session
// state: SQL value comparison, identifier and table-path resolution over parse
// trees, string-literal unquoting, and the protoreflect <-> driver.Value
// marshaling shared by the planner, the executor and the embedded connection.
package functions
