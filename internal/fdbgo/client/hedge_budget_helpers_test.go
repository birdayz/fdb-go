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
	setSecondState(q, q.multiplier(), budget)
}

// setSecondState sets the second-request pair and its idle flag under secondMu.
func setSecondState(q *QueueModel, multiplier, budget float64) {
	q.secondMu.Lock()
	defer q.secondMu.Unlock()
	q.secondMultiplier, q.secondBudget = multiplier, budget
	q.secondIdle.Store(multiplier == 1.0 && budget == secondRequestMaxBudget)
}

func secondState(q *QueueModel) (budget, multiplier float64) {
	q.secondMu.Lock()
	defer q.secondMu.Unlock()
	return q.secondBudget, q.secondMultiplier
}

// totalOf returns a server's raw smoothOutstanding running total (the leak sentinel).
func totalOf(qm *QueueModel, addr string) (total float64) {
	withServer(qm, addr, func(d *queueData) { total = d.smoothOutstanding.total })
	return total
}

// withServer runs fn on addr's queueData under its lock.
func withServer(q *QueueModel, addr string, fn func(d *queueData)) {
	d := q.getOrCreate(addr)
	d.mu.Lock()
	defer d.mu.Unlock()
	fn(d)
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
