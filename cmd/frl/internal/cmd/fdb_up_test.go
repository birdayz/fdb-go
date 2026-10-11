package cmd

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestFdbUpPullPrecedesStartupDeadline(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		callerDeadline bool
	}{
		{name: "unbounded_caller"},
		{name: "bounded_caller", callerDeadline: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parent := context.Background()
			if tc.callerDeadline {
				var cancel context.CancelFunc
				parent, cancel = context.WithTimeout(parent, time.Hour)
				defer cancel()
			}
			var calls []string
			stop := errors.New("stop at container startup")
			run := func(ctx context.Context, args ...string) (string, error) {
				calls = append(calls, args[0])
				switch args[0] {
				case "ps":
					return "", nil
				case "image":
					return "", errors.New("image missing")
				case "run":
					deadline, ok := ctx.Deadline()
					if !ok || time.Until(deadline) > 2*time.Minute {
						t.Errorf("startup deadline = %v, present=%v", deadline, ok)
					}
					return "", stop
				default:
					t.Fatalf("unexpected Docker command %q", args)
					return "", nil
				}
			}
			pull := func(ctx context.Context, image string, _ io.Writer) error {
				calls = append(calls, "pull")
				got, gotOK := ctx.Deadline()
				want, wantOK := parent.Deadline()
				if gotOK != wantOK || !got.Equal(want) {
					t.Errorf("pull deadline = %v/%v, caller = %v/%v", got, gotOK, want, wantOK)
				}
				if image != defaultFdbImage {
					t.Errorf("pull image = %q", image)
				}
				return nil
			}
			cmd := newFdbUpCmdWithDocker(func(context.Context) error { return nil }, run, pull)
			cmd.SetArgs([]string{})
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			if err := cmd.ExecuteContext(parent); !errors.Is(err, stop) {
				t.Fatalf("startup error = %v, want %v", err, stop)
			}
			if len(calls) < 4 || !slices.Equal(calls[:4], []string{"ps", "image", "pull", "run"}) {
				t.Fatalf("Docker order = %q", calls)
			}
		})
	}
}

func TestFdbUpFailureRemovesOnlyOwnedContainer(t *testing.T) {
	t.Parallel()
	ownedID := strings.Repeat("a", 64)
	otherID := strings.Repeat("b", 64)
	for _, tc := range []struct {
		name            string
		created         bool
		runErr          bool
		clusterMismatch bool
		wantDiagnostics bool
		wantErr         string
	}{
		{name: "name_race", runErr: true},
		{name: "failed_start", created: true, runErr: true},
		{name: "canceled_after_creation", created: true, wantDiagnostics: true},
		{name: "name_replaced_after_creation", created: true, clusterMismatch: true},
		{name: "unresolved_identity", clusterMismatch: true, wantErr: "found 0 containers"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// Another invocation's container holds the name with its own label.
			labels := map[string]string{otherID: fdbInstanceLabelKey + "=" + strings.Repeat("0", 32)}
			var removed []string
			diagnosed := false
			run := func(cleanupCtx context.Context, args ...string) (string, error) {
				switch args[0] {
				case "ps":
					if !slices.Equal(args[:3], []string{"ps", "-aq", "--no-trunc"}) {
						return "", nil // pre-start name check: nothing yet
					}
					var ids []string
					for id, label := range labels {
						if len(args) != 5 || args[3] != "--filter" || args[4] == "label="+label {
							ids = append(ids, id)
						}
					}
					// Merged stderr: a config warning must not hide the IDs.
					return "WARNING: Error loading config file\n" + strings.Join(ids, "\n"), nil
				case "image":
					return "", nil
				case "run":
					at := slices.Index(args, "--label")
					if at < 0 || !strings.HasPrefix(args[at+1], fdbInstanceLabelKey+"=") ||
						len(args[at+1]) <= len(fdbInstanceLabelKey)+1 {
						t.Fatalf("run lacks a per-invocation instance label: %q", args)
					}
					if tc.created {
						labels[ownedID] = args[at+1]
					}
					if !tc.clusterMismatch {
						cancel()
					}
					if tc.runErr {
						return "docker start or name conflict", errors.New("docker run failed")
					}
					return "noise\n" + ownedID + "\n", nil
				case "exec":
					if args[1] != ownedID {
						t.Errorf("exec targeted mutable name instead of owned ID: %q", args)
					}
					if args[2] == "cat" {
						return "unexpected cluster file", nil
					}
					return "The database is available", nil
				case "logs":
					// Diagnostics must read our container while it still exists.
					if args[len(args)-1] != ownedID || cleanupCtx.Err() != nil || len(removed) > 0 {
						t.Errorf("diagnostics read %q after cleanup or under cancellation (%v)", args, cleanupCtx.Err())
					}
					diagnosed = true
					return "FDBD joined cluster.", nil
				case "rm":
					if cleanupCtx.Err() != nil {
						t.Errorf("cleanup inherited cancellation: %v", cleanupCtx.Err())
					}
					if len(args) != 3 || args[1] != "-fv" {
						t.Fatalf("cleanup arguments = %q", args)
					}
					removed = append(removed, args[2])
					return "", nil
				default:
					t.Fatalf("unexpected Docker command %q", args)
					return "", nil
				}
			}
			cmd := newFdbUpCmdWithDocker(func(context.Context) error { return nil }, run,
				func(context.Context, string, io.Writer) error { t.Fatal("present image pulled"); return nil })
			cmd.SetArgs([]string{})
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			if err := cmd.ExecuteContext(ctx); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("startup error = %v, want failure containing %q", err, tc.wantErr)
			}
			var want []string
			if tc.created {
				want = []string{ownedID}
			}
			if !slices.Equal(removed, want) {
				t.Fatalf("removed containers = %q, want %q", removed, want)
			}
			if diagnosed != tc.wantDiagnostics {
				t.Fatalf("startup diagnostics collected = %v, want %v", diagnosed, tc.wantDiagnostics)
			}
		})
	}
}

func TestFdbUpInstanceLabelIsPerInvocation(t *testing.T) {
	t.Parallel()
	var labels []string
	for range 2 {
		run := func(_ context.Context, args ...string) (string, error) {
			if args[0] == "run" {
				if at := slices.Index(args, "--label"); at >= 0 {
					labels = append(labels, args[at+1])
				}
				return "", errors.New("stop")
			}
			return "", nil
		}
		cmd := newFdbUpCmdWithDocker(func(context.Context) error { return nil }, run,
			func(context.Context, string, io.Writer) error { return nil })
		cmd.SetArgs([]string{})
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		_ = cmd.ExecuteContext(context.Background())
	}
	if len(labels) != 2 || labels[0] == labels[1] {
		t.Fatalf("instance labels = %q, want two distinct", labels)
	}
}

func TestFdbContainersByLabelKeepsOnlyIDs(t *testing.T) {
	t.Parallel()
	id := strings.Repeat("c", 64)
	for _, tc := range []struct {
		name string
		out  string
		want []string
	}{
		{"none", "", nil},
		{"full_id", id + "\n", []string{id}},
		{"short_id", "abcd\n", nil},
		{"uppercase", strings.ToUpper(id), nil},
		{"warning", "WARNING: Error loading config file\n" + id + "\n", []string{id}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := func(context.Context, ...string) (string, error) { return tc.out, nil }
			ids, err := fdbContainersByLabel(context.Background(), run, fdbInstanceLabelKey+"=x")
			if err != nil || !slices.Equal(ids, tc.want) {
				t.Fatalf("ids = %q, err = %v; want %q", ids, err, tc.want)
			}
		})
	}
}
