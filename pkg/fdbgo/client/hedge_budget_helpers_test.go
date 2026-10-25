package client

import (
	"math"
	"testing"
)

// budgetModel is a fresh QueueModel holding budget second requests' worth.
func budgetModel(budget float64) *QueueModel {
	q := newQueueModel()
	q.secondBudget = budget
	return q
}

// setSecondBudget gives a live database's model budget second requests' worth.
func setSecondBudget(q *QueueModel, budget float64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.secondBudget = budget
}

func secondState(q *QueueModel) (budget, multiplier float64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.secondBudget, q.secondMultiplier
}

// checkSecondModel checks C++'s literal budget rules after lone replies or one
// paid hedge (flow/Knobs.cpp:280-283); callers do not both accrue and spend.
func checkSecondModel(t *testing.T, q *QueueModel, budget float64, accrued int, spent bool) {
	t.Helper()
	wantBudget, wantMultiplier := budget, 1.0
	for range accrued {
		wantBudget = math.Min(wantBudget+0.05, 100)
	}
	if spent {
		wantBudget--
		wantMultiplier += 0.01
	}
	gotBudget, gotMultiplier := secondState(q)
	if math.Abs(gotBudget-wantBudget) > 1e-9 || math.Abs(gotMultiplier-wantMultiplier) > 1e-12 {
		t.Fatalf("second-request model budget=%v multiplier=%v; want %v and %v (%d lone replies, spent=%v)", gotBudget, gotMultiplier, wantBudget, wantMultiplier, accrued, spent)
	}
}
