//go:build race

package client

// sync.Pool.Put randomly discards one quarter of its values under -race
// (Go sync/pool.go), so warm-pool allocation budgets do not apply.
const walkPoolRaceInstrumented = true
