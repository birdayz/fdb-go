package embedded

import (
	"reflect"
	"testing"
	"time"

	"fdb.dev/pkg/recordlayer/query/executor"
)

// The driver's scan type per SQL type: a DATE or TIMESTAMP result is its
// canonical text, which is what the driver returns.
func TestPaginatingRows_ColumnTypeScanType(t *testing.T) {
	t.Parallel()
	want := map[string]reflect.Type{
		"BIGINT":    reflect.TypeFor[int64](),
		"INTEGER":   reflect.TypeFor[int32](),
		"DOUBLE":    reflect.TypeFor[float64](),
		"FLOAT":     reflect.TypeFor[float32](),
		"STRING":    reflect.TypeFor[string](),
		"BOOLEAN":   reflect.TypeFor[bool](),
		"BYTES":     reflect.TypeFor[[]byte](),
		"BINARY":    reflect.TypeFor[[]byte](),
		"DATE":      reflect.TypeFor[string](),
		"TIMESTAMP": reflect.TypeFor[string](),
		"UUID":      reflect.TypeFor[any](),
	}
	var r paginatingRows
	var names []string
	for name := range want {
		r.cols = append(r.cols, executor.ColumnDef{TypeName: name})
		names = append(names, name)
	}
	for i, name := range names {
		if got := r.ColumnTypeScanType(i); got != want[name] {
			t.Errorf("%s scans as %v, want %v", name, got, want[name])
		}
		if r.ColumnTypeScanType(i) == reflect.TypeFor[time.Time]() {
			t.Errorf("%s scans as time.Time", name)
		}
	}
}
