// Package dstwire adapts internal/dst simulation environments into stdlib-only
// recordlayer seams so pkg/recordlayer never exposes internal/dst in its public API.
package dstwire

import (
	"fdb.dev/internal/dst"
	"fdb.dev/pkg/recordlayer"
)

// Env adapts a *dst.Env to a *recordlayer.Env (nil-preserving).
func Env(env *dst.Env) *recordlayer.Env {
	if env == nil {
		return nil
	}
	return &recordlayer.Env{
		Clock:  env,
		Random: env,
	}
}
