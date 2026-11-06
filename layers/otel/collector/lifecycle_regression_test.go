package collector

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"fdb.dev/layers/otel"
)

func TestStartOpenerCanInspectExporter(t *testing.T) {
	t.Parallel()
	var x *TracesExporter
	sink := &fakeSink{}
	var err error
	x, err = NewFactory().CreateTraces(Settings{Open: func(context.Context, otel.ExporterConfig) (Sink, error) {
		if x.Sink() != nil || x.Stats().AcceptedRequests != 0 {
			return nil, errors.New("starting exporter exposed a sink or admissions")
		}
		return sink, nil
	}}, unitConfig(nil))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- x.Start(t.Context()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("opener deadlocked inspecting exporter")
	}
	if err := x.Shutdown(t.Context()); err != nil || sink.shutdownCount() != 1 {
		t.Fatalf("shutdown=%v count=%d", err, sink.shutdownCount())
	}
}

func TestShutdownDeadlineWhileOpeningDisposesLateSink(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	sink := &fakeSink{}
	x, err := NewFactory().CreateTraces(Settings{Open: func(context.Context, otel.ExporterConfig) (Sink, error) { close(entered); <-release; return sink, nil }}, unitConfig(nil))
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan error, 1)
	go func() { start <- x.Start(t.Context()) }()
	<-entered
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	shutdown := make(chan error, 1)
	go func() { shutdown <- x.Shutdown(ctx) }()
	select {
	case err := <-shutdown:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("shutdown=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown blocked behind opener")
	}
	unblock()
	if err := <-start; err == nil {
		t.Fatal("late opener installed a running exporter")
	}
	if err := x.Shutdown(t.Context()); err != nil || sink.shutdownCount() != 1 {
		t.Fatalf("late sink shutdown=%v count=%d", err, sink.shutdownCount())
	}
}

func TestSynchronousFlushWaitsForAdmittedSinkCall(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	sink := &fakeSink{export: func(ctx context.Context, _ int, names []string) (otel.ExportStats, error) {
		close(entered)
		select {
		case <-release:
			return okStats(len(names)), nil
		case <-ctx.Done():
			return otel.ExportStats{}, ctx.Err()
		}
	}}
	x := startUnitExporter(t, unitConfig(func(c *Config) { c.SendingQueue.Enabled = false }), sink)
	done := make(chan error, 1)
	go func() { done <- x.ConsumeTraces(t.Context(), testRequest("sync", 0, 2)) }()
	<-entered
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := x.Flush(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Flush passed an admitted incomplete synchronous export: %v", err)
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := x.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if st := x.Stats(); st.AcceptedRequests != 1 || st.ExportedSpans != 2 || st.QueuedSpans != 0 {
		t.Fatalf("stats=%+v", st)
	}
}
