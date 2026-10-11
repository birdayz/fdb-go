// Package rangecheck lets the fdb facade run the client's selector-range pre-checks
// without making them public client API.
package rangecheck

import "context"

// Check runs a selector range read's checks on its raw selector keys; the client
// package installs it. tx is the facade's *client.Transaction.
var Check func(ctx context.Context, tx any, beginKey, endKey []byte, limit int) error
