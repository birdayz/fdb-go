package querycensus

import (
	"reflect"
	"testing"
)

// A nil field in an installed table is a record site that panics the moment the
// census turns on, so every field of both tables must be wired.
func TestInstalledHookTablesAreFullyWired(t *testing.T) {
	t.Parallel()
	for name, table := range map[string]any{"CensusHooks": hooks, "OrderingHooks": orderingHooks} {
		v := reflect.ValueOf(table).Elem()
		for i := 0; i < v.NumField(); i++ {
			if v.Field(i).IsNil() {
				t.Errorf("%s.%s is nil: its record sites would panic with the census on",
					name, v.Type().Field(i).Name)
			}
		}
	}
}
