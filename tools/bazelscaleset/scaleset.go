package main

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sort"

	"github.com/actions/scaleset"
)

// scaleSetClient is the subset of *scaleset.Client that ensureRunnerScaleSet
// needs. It deliberately has NO delete method: a scale set is a durable resource
// (GitHub tracks job assignment against its stable ID), and this code path must
// never be able to destroy one, whether the previous incarnation crashed or
// exited cleanly. See ensureRunnerScaleSet's doc comment for why deleting was a
// bug, not a feature.
type scaleSetClient interface {
	GetRunnerScaleSet(ctx context.Context, runnerGroupID int, name string) (*scaleset.RunnerScaleSet, error)
	CreateRunnerScaleSet(ctx context.Context, rs *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error)
	UpdateRunnerScaleSet(ctx context.Context, id int, rs *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error)
}

var _ scaleSetClient = (*scaleset.Client)(nil)

// ensureRunnerScaleSet idempotently ensures a runner scale set matching `want`
// (looked up by Name within groupID) exists, and returns it — WITHOUT ever
// deleting an existing one.
//
// This replaces the previous "delete any existing scale set with this name,
// then always create a new one" logic that ran on every process start (clean
// shutdown, crash, OOM kill, or watchdog restart alike). That logic caused a
// real production incident: GitHub's Actions Runner Scale Set protocol tracks
// job assignment against the scale set's *stable numeric ID* (see
// RunnerScaleSetSession.Statistics.TotalAssignedJobs, which
// MessageSessionClient.createMessageSession fetches and listener.Run feeds
// straight into the first HandleDesiredRunnerCount call — a fresh session
// against an EXISTING scale set correctly resumes any backlog). Deleting the
// scale set instead severs that identity: a job GitHub had already
// assigned/queued against the old ID has nowhere to land once a new ID takes
// its place moments later, and it sits `queued` forever. A restart is not a
// rare event here — the systemd watchdog (infra/cloud-init.yaml) restarts the
// service on sustained high load, which is exactly when CI has the most jobs
// in flight. This is also how GitHub's own actions-runner-controller behaves:
// a scale set is a durable resource, created once and reconciled (PATCHed) in
// place, never torn down just because its listener process restarted.
//
// If a scale set with this name already exists but its config has drifted
// (labels or DisableUpdate changed — e.g. a redeploy changed --labels), it is
// updated in place via UpdateRunnerScaleSet (a PATCH, same ID preserved), not
// replaced.
func ensureRunnerScaleSet(ctx context.Context, client scaleSetClient, logger *slog.Logger, groupID int, want *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	existing, err := client.GetRunnerScaleSet(ctx, groupID, want.Name)
	if err != nil {
		return nil, fmt.Errorf("checking for existing scale set %q: %w", want.Name, err)
	}

	if existing == nil {
		ss, err := client.CreateRunnerScaleSet(ctx, want)
		if err != nil {
			return nil, fmt.Errorf("creating runner scale set %q: %w", want.Name, err)
		}
		logger.Info("runner scale set created", slog.Int("id", ss.ID), slog.String("name", ss.Name))
		return ss, nil
	}

	if scaleSetConfigMatches(existing, want) {
		logger.Info("reusing existing runner scale set",
			slog.Int("id", existing.ID), slog.String("name", existing.Name))
		return existing, nil
	}

	logger.Warn("existing runner scale set config drifted from desired config; updating in place",
		slog.Int("id", existing.ID), slog.String("name", existing.Name))
	updated, err := client.UpdateRunnerScaleSet(ctx, existing.ID, want)
	if err != nil {
		return nil, fmt.Errorf("updating drifted scale set %d: %w", existing.ID, err)
	}
	return updated, nil
}

// scaleSetConfigMatches reports whether an existing scale set's labels (compared
// as a set — GitHub may reorder them) and DisableUpdate setting already match
// what we want, so an UpdateRunnerScaleSet PATCH can be skipped entirely.
func scaleSetConfigMatches(existing, want *scaleset.RunnerScaleSet) bool {
	if existing.RunnerSetting.DisableUpdate != want.RunnerSetting.DisableUpdate {
		return false
	}
	return slices.Equal(labelNames(existing.Labels), labelNames(want.Labels))
}

func labelNames(labels []scaleset.Label) []string {
	names := make([]string, len(labels))
	for i, l := range labels {
		names[i] = l.Name
	}
	sort.Strings(names)
	return names
}
