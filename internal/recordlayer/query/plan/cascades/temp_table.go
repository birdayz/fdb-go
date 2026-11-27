package cascades

import "sync"

// TempTable is a mutable in-memory buffer used as intermediate storage
// for recursive CTE evaluation. Thread-safe.
type TempTable struct {
	mu   sync.Mutex
	data []any
}

func NewTempTable() *TempTable {
	return &TempTable{}
}

func (t *TempTable) Add(element any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data = append(t.data, element)
}

func (t *TempTable) Clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data = t.data[:0]
}

func (t *TempTable) IsEmpty() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.data) == 0
}

func (t *TempTable) List() []any {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]any, len(t.data))
	copy(out, t.data)
	return out
}

func (t *TempTable) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.data)
}
