// Package collector batches protobuf spans into *otel.Exporter with bounded queues
// and a Collector-compatible YAML subset; it is not an upstream Collector component.
//
// The package models Collector lifecycle, sending_queue and retry_on_failure contracts
// on OTLP ResourceSpans without depending on the Collector module graph.
package collector

import (
	"context"
	"errors"
	"sync"

	"fdb.dev/layers/otel"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

const (
	// ComponentType is the exporter type key in Collector YAML.
	ComponentType = "fdb_otel"
	// TracesStability is the stability level of the traces exporter.
	TracesStability = "alpha"

	maxConsumers = 64
	maxQueueSize = 10_000_000
	maxFailures  = 64
)

var (
	// ErrQueueFull rejects a request when the sending queue lacks capacity and
	// block_on_overflow is off. The request was not admitted; it is retryable.
	ErrQueueFull = &QueueFullError{}
	// ErrNotRunning rejects work before Start or after Shutdown.
	ErrNotRunning = &NotRunningError{}
)

// Sink is the export surface the queue flushes into; *otel.Exporter implements it.
type Sink interface {
	ExportResourceSpans(context.Context, []*tracepb.ResourceSpans) (otel.ExportStats, error)
	Shutdown(context.Context) error
}

var _ Sink = (*otel.Exporter)(nil)

type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

func permanent(format string, args ...any) error {
	return &permanentError{componentErrorf("fdb_otel: "+format, args...)}
}

// IsPermanent identifies nonretryable input/config/ownership/corruption and otel.Exporter operation errors.
// Collector NotRunningError, transient, budget and per-attempt cancellation errors are not classified as permanent.
func IsPermanent(err error) bool {
	if err == nil {
		return false
	}
	var p *permanentError
	if errors.As(err, &p) {
		return true
	}
	var oe *otel.OtelError
	if errors.As(err, &oe) {
		switch oe.Kind {
		case "config", "admission", "ownership", "corruption", "operation":
			return true
		}
	}
	return false
}

// Settings carries the component identity; Open overrides how Start opens the
// sink (nil opens *otel.Exporter on the configured FDB cluster).
type Settings struct {
	ID   ComponentID
	Open func(context.Context, otel.ExporterConfig) (Sink, error)
}

// Factory creates fdb_otel traces exporters.
type Factory struct{}

func NewFactory() Factory { return Factory{} }

func (Factory) Type() string            { return ComponentType }
func (Factory) TracesStability() string { return TracesStability }

// CreateDefaultConfig returns defaults; cluster_file and run remain required.
func (Factory) CreateDefaultConfig() *Config {
	c := defaultConfig()
	return &c
}

// CreateTraces validates cfg and returns an exporter that opens FDB on Start.
func (Factory) CreateTraces(set Settings, cfg *Config) (*TracesExporter, error) {
	if cfg == nil {
		return nil, componentErrorf("%s: nil config", ComponentType)
	}
	if set.ID.Type == "" {
		set.ID.Type = ComponentType
	}
	if set.ID.Type != ComponentType {
		return nil, componentErrorf("%s: component id %s has the wrong type", ComponentType, set.ID)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	open := set.Open
	if open == nil {
		open = func(ctx context.Context, c otel.ExporterConfig) (Sink, error) {
			e, err := otel.NewExporter(ctx, c)
			if err != nil {
				return nil, err
			}
			return e, nil
		}
	}
	return &TracesExporter{id: set.ID, cfg: *cfg, open: open, remaining: map[uint64]int{}, wake: make(chan struct{}, 1), changed: make(chan struct{}), done: make(chan struct{})}, nil
}

// Capabilities mirrors consumer.Capabilities.
type Capabilities struct{ MutatesData bool }

// Stats are cumulative exporter counters; QueuedSpans is the current backlog
// of admitted spans not yet exported or failed.
type Stats struct {
	AcceptedRequests int64 `json:"accepted_requests"`
	AcceptedSpans    int64 `json:"accepted_spans"`
	RejectedSpans    int64 `json:"rejected_spans"`
	ExportedSpans    int64 `json:"exported_spans"`
	StoredSpans      int64 `json:"stored_spans"`
	DuplicateSpans   int64 `json:"duplicate_spans"`
	FailedSpans      int64 `json:"failed_spans"`
	Flushes          int64 `json:"flushes"`
	Exports          int64 `json:"exports"`
	Retries          int64 `json:"retries"`
	QueuedSpans      int   `json:"queued_spans"`
}

const (
	stateCreated = iota
	stateStarting
	stateStarted
	stateStopping
	stateStopped
)

// TracesExporter shares a span-bounded queue across sources, batching by min/max size or flush timeout.
// With sending_queue enabled, num_consumers workers export batches with retry.
type TracesExporter struct {
	id   ComponentID
	cfg  Config
	open func(context.Context, otel.ExporterConfig) (Sink, error)

	mu            sync.Mutex
	state         int
	sink          Sink
	pending       []request
	flushWanted   bool
	batchCutProbe func([]request, []request) // optional queue ownership probe; set before Start
	outstanding   int
	nextSeq       uint64
	order         []uint64 // admitted, incomplete request sequences, ascending
	remaining     map[uint64]int
	failures      []error
	dropped       int
	stats         Stats
	shutdownErr   error

	wake       chan struct{} // batcher wakeup, capacity 1
	changed    chan struct{} // closed and replaced on every completion
	batches    chan batch
	workCtx    context.Context
	cancelWork context.CancelFunc
	workers    sync.WaitGroup
	active     sync.WaitGroup // synchronous exports in flight
	done       chan struct{}
	startDone  chan struct{}
}

func (x *TracesExporter) ID() ComponentID            { return x.id }
func (x *TracesExporter) Config() Config             { return x.cfg }
func (x *TracesExporter) Capabilities() Capabilities { return Capabilities{} }

// Sink returns the opened sink (an *otel.Exporter by default), nil before Start.
func (x *TracesExporter) Sink() Sink {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.sink
}

func (x *TracesExporter) Stats() Stats {
	x.mu.Lock()
	defer x.mu.Unlock()
	s := x.stats
	s.QueuedSpans = x.outstanding
	return s
}

// Start opens the sink and starts the batcher and consumers.
func (x *TracesExporter) Start(ctx context.Context) error {
	x.mu.Lock()
	if x.state != stateCreated {
		x.mu.Unlock()
		return componentErrorf("%s: %s already started", ComponentType, x.id)
	}
	x.state = stateStarting
	x.startDone = make(chan struct{})
	x.workCtx, x.cancelWork = context.WithCancel(context.Background())
	x.mu.Unlock()
	openCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(x.workCtx, cancel)
	sink, err := x.open(openCtx, x.cfg.Exporter)
	stop()
	cancel()
	x.mu.Lock()
	defer x.mu.Unlock()
	defer close(x.startDone)
	x.sink = sink
	if x.state != stateStarting {
		return errors.Join(err, &NotRunningError{Operation: "start", State: x.state})
	}
	if err != nil {
		x.state = stateCreated
		x.cancelWork()
		return err
	}
	x.state = stateStarted
	if q := x.cfg.SendingQueue; q.Enabled {
		x.batches = make(chan batch)
		x.workers.Add(1 + q.NumConsumers)
		go x.batchLoop()
		for range q.NumConsumers {
			go x.consumeLoop()
		}
	}
	return nil
}

// Flush sends the partial batch and waits for all prior admissions to settle,
// returning export failures recorded since the previous Flush.
func (x *TracesExporter) Flush(ctx context.Context) error {
	x.mu.Lock()
	if x.state == stateCreated || x.state == stateStarting {
		err := &NotRunningError{Operation: "flush", State: x.state}
		x.mu.Unlock()
		return err
	}
	target := x.nextSeq
	x.flushWanted = true
	x.signal()
	for len(x.order) > 0 && x.order[0] < target {
		wait := x.changed
		x.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
		x.mu.Lock()
	}
	errs := x.failures
	if x.dropped > 0 {
		errs = append(errs, componentErrorf("fdb_otel: %d further export failures omitted", x.dropped))
	}
	x.failures, x.dropped = nil, 0
	x.mu.Unlock()
	return errors.Join(errs...)
}

// Shutdown stops admission, drains and closes; on ctx expiry it cancels exports
// and fails remaining spans, but still closes the sink in the background.
func (x *TracesExporter) Shutdown(ctx context.Context) error {
	x.mu.Lock()
	cancel := x.cancelWork
	switch x.state {
	case stateCreated:
		x.state = stateStopped
		close(x.done)
	case stateStarting, stateStarted:
		x.state = stateStopping
		go func() {
			<-x.startDone
			x.workers.Wait()
			x.active.Wait()
			var err error
			if x.sink != nil {
				err = x.sink.Shutdown(context.Background())
			}
			x.mu.Lock()
			x.shutdownErr = err
			x.state = stateStopped
			x.mu.Unlock()
			x.cancelWork()
			close(x.done)
		}()
	}
	x.mu.Unlock()
	x.signal()
	select {
	case <-x.done:
	case <-ctx.Done():
		select {
		case <-x.done:
		default:
			if cancel != nil {
				cancel()
			}
			return componentErrorf("%s shutdown: %w", ComponentType, ctx.Err())
		}
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.shutdownErr
}
