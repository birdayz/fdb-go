package fdb

import "testing"

// TestStartStopNetwork_NoOp pins the source-compat no-ops: the pure-Go client has
// no global network thread, so both return nil.
func TestStartStopNetwork_NoOp(t *testing.T) {
	t.Parallel()
	if err := StartNetwork(); err != nil {
		t.Errorf("StartNetwork() = %v, want nil", err)
	}
	if err := StopNetwork(); err != nil {
		t.Errorf("StopNetwork() = %v, want nil", err)
	}
}

// TestAPIVersionHelpers checks IsAPIVersionSelected and MustGetAPIVersion stay in
// lockstep with GetAPIVersion. The API version is a package-global set-once value,
// so this asserts the invariant rather than a fixed value.
func TestAPIVersionHelpers(t *testing.T) {
	v, err := GetAPIVersion()
	if IsAPIVersionSelected() != (err == nil) {
		t.Fatalf("IsAPIVersionSelected()=%v but GetAPIVersion err=%v", IsAPIVersionSelected(), err)
	}
	if err == nil && MustGetAPIVersion() != v {
		t.Errorf("MustGetAPIVersion()=%d, want %d", MustGetAPIVersion(), v)
	}
}
