package factorycorpus

import (
	"fmt"
	"unsafe"
)

// StreamingIndexStringsDetachedForTest verifies the exact directory path's
// uniqueness indexes do not retain a parsed family's source-buffer substrings.
func StreamingIndexStringsDetachedForTest(dir string) (bool, error) {
	var loaded []*Scenario
	_, names, keys, err := computeCensusDir(dir, func(path string) (*FamilyFile, error) {
		family, err := Load(path)
		if err == nil {
			loaded = append(loaded, family.Scenarios...)
		}
		return family, err
	})
	if err != nil {
		return false, err
	}
	if len(loaded) == 0 || len(names) != len(loaded) || len(keys) != len(loaded) {
		return false, fmt.Errorf("streaming uniqueness-index test population is incomplete: scenarios=%d names=%d keys=%d", len(loaded), len(names), len(keys))
	}
	for _, scenario := range loaded {
		for name := range names {
			if name == scenario.Header.Name && unsafe.StringData(name) == unsafe.StringData(scenario.Header.Name) {
				return false, nil
			}
		}
		for key := range keys {
			if key == scenario.Header.DedupKey && unsafe.StringData(key) == unsafe.StringData(scenario.Header.DedupKey) {
				return false, nil
			}
		}
	}
	return true, nil
}
