package main

import (
	"context"
	"errors"
	"testing"

	"github.com/actions/scaleset"
)

// fakeScaleSetClient implements scaleSetClient for ensureRunnerScaleSet tests.
// It deliberately has no Delete method, matching the production interface: the
// capability to delete an existing scale set must not even be reachable from
// this code path (see scaleSetClient's doc comment).
type fakeScaleSetClient struct {
	existing  *scaleset.RunnerScaleSet
	getErr    error
	createErr error
	updateErr error

	createCalls []scaleset.RunnerScaleSet
	updateCalls []scaleset.RunnerScaleSet
	updateIDs   []int
}

func (f *fakeScaleSetClient) GetRunnerScaleSet(context.Context, int, string) (*scaleset.RunnerScaleSet, error) {
	return f.existing, f.getErr
}

func (f *fakeScaleSetClient) CreateRunnerScaleSet(_ context.Context, rs *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	f.createCalls = append(f.createCalls, *rs)
	if f.createErr != nil {
		return nil, f.createErr
	}
	created := *rs
	created.ID = 999 // a freshly minted ID, distinct from any "existing" ID used below
	return &created, nil
}

func (f *fakeScaleSetClient) UpdateRunnerScaleSet(_ context.Context, id int, rs *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	f.updateCalls = append(f.updateCalls, *rs)
	f.updateIDs = append(f.updateIDs, id)
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	updated := *rs
	updated.ID = id
	return &updated, nil
}

func wantScaleSet() *scaleset.RunnerScaleSet {
	return &scaleset.RunnerScaleSet{
		Name:          "hetzner-fdb",
		RunnerGroupID: 1,
		Labels:        []scaleset.Label{{Name: "hetzner-fdb"}},
		RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
	}
}

func TestEnsureRunnerScaleSetCreatesWhenMissing(t *testing.T) {
	t.Parallel()
	f := &fakeScaleSetClient{}
	ss, err := ensureRunnerScaleSet(context.Background(), f, discardLogger(), 1, wantScaleSet())
	if err != nil {
		t.Fatalf("ensureRunnerScaleSet: %v", err)
	}
	if len(f.createCalls) != 1 {
		t.Fatalf("CreateRunnerScaleSet calls = %d, want 1", len(f.createCalls))
	}
	if len(f.updateCalls) != 0 {
		t.Fatalf("UpdateRunnerScaleSet calls = %d, want 0", len(f.updateCalls))
	}
	if ss.ID != 999 {
		t.Fatalf("ID = %d, want 999 (from CreateRunnerScaleSet)", ss.ID)
	}
}

// TestEnsureRunnerScaleSetReusesExistingScaleSet pins the production incident
// fix: when a scale set with this name already exists and its config matches,
// it MUST be reused by its existing ID — never deleted and replaced with a
// freshly minted one. GitHub tracks in-flight job assignment against the scale
// set's numeric ID; minting a new ID orphans any job already assigned/queued
// against the old one. This happened live: under a systemd watchdog restart
// during heavy CI load, the old "delete any existing scale set, then always
// create a new one" logic orphaned a 7+ PR backlog that sat `queued` forever
// because GitHub had already routed job assignments to the deleted ID.
func TestEnsureRunnerScaleSetReusesExistingScaleSet(t *testing.T) {
	t.Parallel()
	want := wantScaleSet()
	existing := &scaleset.RunnerScaleSet{
		ID:            42,
		Name:          want.Name,
		RunnerGroupID: want.RunnerGroupID,
		Labels:        want.Labels,
		RunnerSetting: want.RunnerSetting,
	}
	f := &fakeScaleSetClient{existing: existing}

	ss, err := ensureRunnerScaleSet(context.Background(), f, discardLogger(), 1, want)
	if err != nil {
		t.Fatalf("ensureRunnerScaleSet: %v", err)
	}
	if len(f.createCalls) != 0 {
		t.Fatalf("CreateRunnerScaleSet was called %d times; an existing scale set must never be replaced", len(f.createCalls))
	}
	if len(f.updateCalls) != 0 {
		t.Fatalf("UpdateRunnerScaleSet was called %d times; config matched, no update needed", len(f.updateCalls))
	}
	if ss.ID != 42 {
		t.Fatalf("ID = %d, want 42 (the existing scale set's ID must be preserved)", ss.ID)
	}
}

// TestEnsureRunnerScaleSetUpdatesDriftedConfigInPlace pins that a config drift
// (e.g. --labels changed across a redeploy) is reconciled via PATCH
// (UpdateRunnerScaleSet), preserving the existing ID — not by deleting and
// recreating, which would orphan in-flight job assignments exactly like the
// unconditional-recreate bug this file fixes.
func TestEnsureRunnerScaleSetUpdatesDriftedConfigInPlace(t *testing.T) {
	t.Parallel()
	want := wantScaleSet()
	existing := &scaleset.RunnerScaleSet{
		ID:            42,
		Name:          want.Name,
		RunnerGroupID: want.RunnerGroupID,
		Labels:        []scaleset.Label{{Name: "some-old-label"}},
		RunnerSetting: want.RunnerSetting,
	}
	f := &fakeScaleSetClient{existing: existing}

	ss, err := ensureRunnerScaleSet(context.Background(), f, discardLogger(), 1, want)
	if err != nil {
		t.Fatalf("ensureRunnerScaleSet: %v", err)
	}
	if len(f.createCalls) != 0 {
		t.Fatalf("CreateRunnerScaleSet was called %d times; drift must be patched, not replaced", len(f.createCalls))
	}
	if len(f.updateCalls) != 1 {
		t.Fatalf("UpdateRunnerScaleSet calls = %d, want 1", len(f.updateCalls))
	}
	if f.updateIDs[0] != 42 {
		t.Fatalf("UpdateRunnerScaleSet called with id %d, want 42 (existing ID)", f.updateIDs[0])
	}
	if ss.ID != 42 {
		t.Fatalf("ID = %d, want 42 (existing scale set's ID preserved across the update)", ss.ID)
	}
}

func TestEnsureRunnerScaleSetDisableUpdateDriftTriggersUpdate(t *testing.T) {
	t.Parallel()
	want := wantScaleSet() // DisableUpdate: true
	existing := &scaleset.RunnerScaleSet{
		ID:            7,
		Name:          want.Name,
		Labels:        want.Labels,
		RunnerSetting: scaleset.RunnerSetting{DisableUpdate: false},
	}
	f := &fakeScaleSetClient{existing: existing}

	if _, err := ensureRunnerScaleSet(context.Background(), f, discardLogger(), 1, want); err != nil {
		t.Fatalf("ensureRunnerScaleSet: %v", err)
	}
	if len(f.updateCalls) != 1 {
		t.Fatalf("UpdateRunnerScaleSet calls = %d, want 1 (DisableUpdate drifted)", len(f.updateCalls))
	}
}

func TestEnsureRunnerScaleSetLabelOrderDoesNotTriggerUpdate(t *testing.T) {
	t.Parallel()
	want := &scaleset.RunnerScaleSet{
		Name:   "hetzner-fdb",
		Labels: []scaleset.Label{{Name: "a"}, {Name: "b"}},
	}
	existing := &scaleset.RunnerScaleSet{
		ID:     5,
		Name:   want.Name,
		Labels: []scaleset.Label{{Name: "b"}, {Name: "a"}}, // same set, different order
	}
	f := &fakeScaleSetClient{existing: existing}

	if _, err := ensureRunnerScaleSet(context.Background(), f, discardLogger(), 1, want); err != nil {
		t.Fatalf("ensureRunnerScaleSet: %v", err)
	}
	if len(f.updateCalls) != 0 {
		t.Fatalf("UpdateRunnerScaleSet calls = %d, want 0 (label order alone is not drift)", len(f.updateCalls))
	}
}

func TestEnsureRunnerScaleSetPropagatesErrors(t *testing.T) {
	t.Parallel()

	t.Run("get error", func(t *testing.T) {
		t.Parallel()
		f := &fakeScaleSetClient{getErr: errors.New("boom")}
		if _, err := ensureRunnerScaleSet(context.Background(), f, discardLogger(), 1, wantScaleSet()); err == nil {
			t.Fatal("expected error to propagate from GetRunnerScaleSet")
		}
	})
	t.Run("create error", func(t *testing.T) {
		t.Parallel()
		f := &fakeScaleSetClient{createErr: errors.New("boom")}
		if _, err := ensureRunnerScaleSet(context.Background(), f, discardLogger(), 1, wantScaleSet()); err == nil {
			t.Fatal("expected error to propagate from CreateRunnerScaleSet")
		}
	})
	t.Run("update error", func(t *testing.T) {
		t.Parallel()
		want := wantScaleSet()
		existing := &scaleset.RunnerScaleSet{ID: 1, Name: want.Name, Labels: []scaleset.Label{{Name: "drifted"}}}
		f := &fakeScaleSetClient{existing: existing, updateErr: errors.New("boom")}
		if _, err := ensureRunnerScaleSet(context.Background(), f, discardLogger(), 1, want); err == nil {
			t.Fatal("expected error to propagate from UpdateRunnerScaleSet")
		}
	})
}
