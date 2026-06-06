package conformance_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"
	gofdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// JavaInvoker provides method invocation on Java conformance steps via persistent HTTP server
type JavaInvoker struct {
	baseURL    string
	serverCmd  *exec.Cmd
	httpClient *http.Client
	mu         sync.Mutex
	closed     bool
	// borrows counts how many times this server has been handed out by a
	// JavaServerPool. The pool recycles a server once it reaches the pool's
	// maxInvocations bound. Guarded by mu.
	borrows int
}

var (
	globalInvoker     *JavaInvoker
	globalInvokerOnce sync.Once
	globalInvokerErr  error
)

// liveInvokers tracks every Java server ever spawned (global singleton, A3
// per-scenario pool, negative-entry isolates) so the suite can guarantee none
// is left orphaned. fdb-relational's conformance_server is launched via a Bazel
// java_binary WRAPPER SCRIPT that forks the actual JVM as a child, so killing
// only cmd.Process (the wrapper) leaves the JVM running; lifecycle correctness
// requires killing the whole process group AND reaping. This registry is the
// belt-and-suspenders backstop for any Close() that was skipped (a panicking
// spec, an interrupted run): CloseAllJavaServers (called from the suite
// teardown) sweeps it.
var (
	liveInvokers   = map[*JavaInvoker]struct{}{}
	liveInvokersMu sync.Mutex
)

func registerInvoker(j *JavaInvoker) {
	liveInvokersMu.Lock()
	liveInvokers[j] = struct{}{}
	liveInvokersMu.Unlock()
}

func deregisterInvoker(j *JavaInvoker) {
	liveInvokersMu.Lock()
	delete(liveInvokers, j)
	liveInvokersMu.Unlock()
}

// CloseAllJavaServers force-closes every still-registered Java server. Call it
// from the suite teardown so a missed Close (panic, interrupt) can never leak a
// JVM beyond the test process.
func CloseAllJavaServers() {
	liveInvokersMu.Lock()
	invs := make([]*JavaInvoker, 0, len(liveInvokers))
	for j := range liveInvokers {
		invs = append(invs, j)
	}
	liveInvokersMu.Unlock()
	for _, j := range invs {
		_ = j.Close()
	}
}

// CloseJavaInvoker shuts down the global Java server if running.
func CloseJavaInvoker() {
	if globalInvoker != nil {
		_ = globalInvoker.Close()
	}
}

// NewJavaInvoker creates a new Java invoker with persistent server
func NewJavaInvoker() *JavaInvoker {
	globalInvokerOnce.Do(func() {
		globalInvoker, globalInvokerErr = startJavaServer()
	})

	if globalInvokerErr != nil {
		panic(fmt.Sprintf("Failed to start Java server: %v", globalInvokerErr))
	}

	return globalInvoker
}

// NewIsolatedJavaInvoker spawns a FRESH Java conformance server,
// independent of the global singleton. Use it when the test needs
// pristine Java state — e.g. the cross-engine corpus test, where
// state-leaks from prior conformance specs (existence_check_*,
// error_*, the dropped setup-INSERT-error corpus entries before they
// were dropped, etc.) compound into Java-side hangs at >30s
// per-request latency. The investigation in shifts/ uncovered the
// trigger: ~11 setup-time INSERT errors compound state in
// fdb-relational 4.11.1.0's error-path teardown.
//
// Caller is responsible for Close()-ing the returned invoker.
func NewIsolatedJavaInvoker() (*JavaInvoker, error) {
	return startJavaServer()
}

// defaultA3PoolSize is the number of Java servers the pool keeps alive and
// rotates scenarios across. A3 runs serially (one scenario at a time), so ONE
// shared server suffices — exactly like SeedRunCorpus, which drives ~1620
// queries through a single server deterministically. This is the default: it
// spawns a single JVM (not ~119 fresh ones, not a 16-deep buffer), which is both
// the fastest and the lightest on memory — the 16-JVM buffer was what pressured
// a constrained CI runner into GC thrash. Raise it (CONFORMANCE_A3_POOL_SIZE)
// only to parallelize A3 in the future; each extra live JVM is ~250-400MB.
const defaultA3PoolSize = 1

// defaultA3MaxInvocations is how many scenarios a single pooled server handles
// before the pool recycles it (0 = never recycle = pure shared re-use, the
// default). Recycling is NOT needed for determinism: a single JVM serving all
// ~119 A3 scenarios (and SeedRunCorpus's ~1620 queries) is deterministic across
// runs — proven, no cross-query state pollution exists. The knob remains as a
// cheap safety valve: set CONFORMANCE_A3_MAX_INVOCATIONS > 0 to bound per-JVM
// memory or hedge a future fdb-relational version that does leak.
const defaultA3MaxInvocations = 0

// a3PoolSize returns the configured A3 server-pool size (CONFORMANCE_A3_POOL_SIZE,
// else defaultA3PoolSize). Lower it on memory-constrained machines.
func a3PoolSize() int {
	if v := os.Getenv("CONFORMANCE_A3_POOL_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultA3PoolSize
}

// a3MaxInvocations returns the per-server recycle bound
// (CONFORMANCE_A3_MAX_INVOCATIONS, else defaultA3MaxInvocations; 0 = never).
func a3MaxInvocations() int {
	if v := os.Getenv("CONFORMANCE_A3_MAX_INVOCATIONS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return defaultA3MaxInvocations
}

// serverOrErr carries either a freshly-spawned server or the spawn error.
type serverOrErr struct {
	inv *JavaInvoker
	err error
}

// JavaServerPool hands A3 scenarios pooled Java servers that are RE-USED across
// scenarios and recycled after `maxInvocations` borrows. The earlier
// fresh-JVM-per-scenario model was an over-correction: SeedRunCorpus drives
// ~1620 queries through ONE shared server deterministically, so cross-query
// state pollution is not observable at scale — what actually made A3 look
// nondeterministic was (a) Ginkgo's Ordered skip-after-failure + randomized
// order masking a deterministic failure set (fixed with ContinueOnFailure) and
// (b) GRV lag from CONCURRENT server spawning, below. Re-use makes the suite
// fast and light (a pool of `size` servers, not ~119 fresh JVMs ⇒ far fewer
// spawns and far less memory pressure on a constrained CI runner); the
// maxInvocations recycle is a safety belt that bounds any hypothetical
// accumulated state and caps per-JVM memory growth.
//
// CRITICAL determinism rule (still in force): a server's QUERIES must never run
// while ANOTHER server is SPAWNING. All servers share the single FDB
// testcontainer; concurrent spawning + querying causes read-version (GRV) lag —
// a query's transaction can get a read version from BEFORE its own
// ephemeral-schema CREATE committed, see no table, and the Cascades planner
// throws a SPURIOUS UnableToPlanException. (Proven: a query that is 12/12 OK when
// servers spawn sequentially intermittently fails when servers spawn
// concurrently.) A naive background-refill pool violates this. So this pool
// spawns ONLY at safe moments — never while a query runs:
//  1. a startup buffer of `size` servers, spawned concurrently in
//     NewJavaServerPool and fully awaited BEFORE any scenario queries; and
//  2. on demand, SYNCHRONOUSLY inside Borrow (a scenario's BeforeAll, before that
//     scenario's query; Ginkgo runs specs sequentially so no other scenario is
//     querying) — which also covers the post-recycle re-spawn, since Return
//     closes a maxed-out server and the next Borrow finds the pool short.
//
// There is NO background refill. Re-use keeps the pool populated; recycling only
// drops a server between scenarios, and the next Borrow re-spawns synchronously.
type JavaServerPool struct {
	ready          chan *JavaInvoker
	maxInvocations int // 0 = never recycle (pure shared re-use)
}

// NewJavaServerPool spawns `size` servers concurrently and BLOCKS until every one
// is ready, so the pool is full before any scenario queries. Concurrent spawning
// here is safe precisely because no scenario is querying yet. Servers are re-used
// across scenarios and recycled after `maxInvocations` borrows (0 = never).
func NewJavaServerPool(size, maxInvocations int) *JavaServerPool {
	p := &JavaServerPool{ready: make(chan *JavaInvoker, size), maxInvocations: maxInvocations}
	results := make([]serverOrErr, size)
	var wg sync.WaitGroup
	for i := 0; i < size; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			inv, err := NewIsolatedJavaInvoker()
			results[i] = serverOrErr{inv: inv, err: err}
		}(i)
	}
	wg.Wait()
	for _, r := range results {
		if r.err == nil && r.inv != nil {
			p.ready <- r.inv
		}
		// A startup spawn error is dropped here; Borrow re-spawns synchronously
		// (and surfaces any persistent error) when the pool runs short.
	}
	return p
}

// Borrow returns a pooled server — a re-used idle one if available, else a
// synchronously-spawned fresh one. The synchronous spawn blocks the caller (a
// scenario BeforeAll) and therefore never overlaps another scenario's query.
func (p *JavaServerPool) Borrow() (*JavaInvoker, error) {
	select {
	case inv := <-p.ready:
		return inv, nil
	default:
		return NewIsolatedJavaInvoker() // synchronous — no query is running
	}
}

// Return hands a borrowed server back to the pool for re-use, unless it has
// reached the recycle bound (maxInvocations > 0 && borrows >= max), in which case
// it is closed — the pool then runs short and the next Borrow spawns a fresh one
// (synchronously, between scenarios, so the no-spawn-during-query rule holds).
func (p *JavaServerPool) Return(inv *JavaInvoker) {
	if inv == nil {
		return
	}
	inv.mu.Lock()
	inv.borrows++
	recycle := p.maxInvocations > 0 && inv.borrows >= p.maxInvocations
	inv.mu.Unlock()
	if recycle {
		_ = inv.Close()
		return
	}
	select {
	case p.ready <- inv: // back into the pool for re-use
	default:
		_ = inv.Close() // pool already full (can't happen in serial use) — don't leak
	}
}

// Shutdown closes every server still in the pool.
func (p *JavaServerPool) Shutdown() {
	close(p.ready)
	for inv := range p.ready {
		_ = inv.Close()
	}
}

// startJavaServer launches the Java HTTP server and waits for it to be ready
func startJavaServer() (*JavaInvoker, error) {
	// Find the Bazel-built conformance server binary via runfiles
	r, err := runfiles.New()
	if err != nil {
		return nil, fmt.Errorf("failed to create runfiles: %w", err)
	}

	serverBin, err := r.Rlocation("_main/conformance/conformance_server")
	if err != nil {
		return nil, fmt.Errorf("failed to find conformance_server in runfiles: %w", err)
	}

	if _, err := os.Stat(serverBin); err != nil {
		return nil, fmt.Errorf("conformance_server binary not found at %s: %w", serverBin, err)
	}

	// Start server in its OWN process group (Setpgid) so Close can kill the
	// whole group with one signal. The Bazel java_binary launcher is a wrapper
	// script that forks the real JVM as a child; without a group kill, killing
	// only the wrapper (cmd.Process) orphans the JVM.
	cmd := exec.Command(serverBin)
	cmd.Env = append(os.Environ(), r.Env()...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to get stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to get stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start server: %w", err)
	}

	// Read port from stdout
	scanner := bufio.NewScanner(stdout)
	var port string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "CONFORMANCE_SERVER_PORT=") {
			port = strings.TrimPrefix(line, "CONFORMANCE_SERVER_PORT=")
			break
		}
	}

	if port == "" {
		// Try to read stderr for error details
		stderrBytes, _ := io.ReadAll(stderr)
		return nil, fmt.Errorf("failed to read port from server stdout\nstderr: %s", string(stderrBytes))
	}

	// CRITICAL: drain stderr in the background. Java's HTTP request
	// handler calls e.printStackTrace() on every caught exception
	// (conformance_server.java#handleInvoke), writing ~5-10KB per error
	// to stderr. Linux's pipe buffer is 64KB by default. After ~6
	// errors, the pipe fills up and Java's NEXT printStackTrace() call
	// BLOCKS on the stderr write — the request handler thread blocks
	// before sending the HTTP response, and Go's POST hangs at the 120s
	// response timeout. Surfaced  by the cross-engine
	// negative-path harness; jstack showed Java idle (no active worker
	// thread) because the request handler had been blocked mid-stderr-
	// write since some earlier exception, while subsequent requests
	// queued up in TCP buffers waiting for a worker. Continuously
	// reading from stderr keeps the pipe drained so writes never block.
	// Forward to our stderr so failures still surface visibly during
	// debugging.
	go func() { _, _ = io.Copy(os.Stderr, stderr) }()
	// Continue to drain stdout too — anything beyond CONFORMANCE_SERVER_PORT
	// is unexpected but we don't want it backing up either.
	go func() {
		for scanner.Scan() {
			fmt.Fprintln(os.Stderr, "[java-stdout]", scanner.Text())
		}
	}()

	baseURL := fmt.Sprintf("http://127.0.0.1:%s", port)

	invoker := &JavaInvoker{
		baseURL:   baseURL,
		serverCmd: cmd,
		httpClient: &http.Client{
			Timeout: 2 * time.Minute,
		},
	}
	// Register immediately so even a server that never becomes ready is tracked
	// and gets killed+reaped (by Close below or the suite-end sweep).
	registerInvoker(invoker)

	// Wait for server to be ready
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			_ = invoker.Close() // kills the process group + reaps + deregisters
			return nil, fmt.Errorf("server did not become ready in time")
		default:
			resp, err := invoker.httpClient.Get(baseURL + "/health")
			if err == nil && resp.StatusCode == 200 {
				_ = resp.Body.Close()
				fmt.Fprintf(os.Stderr, "Java conformance server ready at %s\n", baseURL)
				return invoker, nil
			}
			if resp != nil {
				_ = resp.Body.Close()
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// Close shuts down the Java server
func (j *JavaInvoker) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()

	if j.closed {
		return nil // idempotent — safe to call from Retire, Shutdown, and the sweep
	}
	j.closed = true
	defer deregisterInvoker(j)

	// Best-effort graceful shutdown (short — we hard-kill regardless).
	if j.baseURL != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		req, _ := http.NewRequestWithContext(ctx, "POST", j.baseURL+"/shutdown", nil)
		if resp, _ := j.httpClient.Do(req); resp != nil {
			_ = resp.Body.Close()
		}
		cancel()
	}

	if j.serverCmd == nil || j.serverCmd.Process == nil {
		return nil
	}
	pid := j.serverCmd.Process.Pid

	// Kill the whole process GROUP (negative pid). The Bazel java_binary
	// wrapper forks the real JVM as a child in this group (Setpgid at spawn);
	// SIGKILL to the group takes down wrapper AND JVM. Fall back to killing the
	// leader directly in case the group setup didn't take.
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		_ = j.serverCmd.Process.Kill()
	}

	// REAP the wrapper so it doesn't linger as a zombie. Wait can block if a
	// child somehow survives the signal, so bound it; the JVM, reparented to
	// init after the group kill, is reaped by init regardless.
	done := make(chan struct{})
	go func() { _, _ = j.serverCmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	return nil
}

// Request is the JSON structure sent to Java
type Request struct {
	Step   string         `json:"step"`
	Params map[string]any `json:"params"`
}

// Response is the JSON structure returned from Java
type Response struct {
	Success            bool            `json:"success"`
	Result             json.RawMessage `json:"result"`
	Error              string          `json:"error,omitempty"`
	ExceptionClass     string          `json:"exceptionClass,omitempty"`
	ExceptionFullClass string          `json:"exceptionFullClass,omitempty"`
}

// JavaError represents a structured error from the Java conformance server.
// It includes the Java exception class name for cross-language error type verification.
type JavaError struct {
	Message            string
	ExceptionClass     string // Simple class name, e.g. "RecordAlreadyExistsException"
	ExceptionFullClass string // Fully qualified, e.g. "com.apple.foundationdb.record.provider.foundationdb.RecordAlreadyExistsException"
}

func (e *JavaError) Error() string {
	return fmt.Sprintf("java %s: %s", e.ExceptionClass, e.Message)
}

// Invoke calls a Java conformance step via HTTP POST
func (j *JavaInvoker) Invoke(ctx context.Context, stepName string, params map[string]any) (json.RawMessage, error) {
	// Build request
	req := Request{
		Step:   stepName,
		Params: params,
	}

	reqBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Make HTTP request
	httpReq, err := http.NewRequestWithContext(ctx, "POST", j.baseURL+"/invoke", bytes.NewReader(reqBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := j.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if httpResp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", httpResp.StatusCode, string(body))
	}

	var resp Response
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w\nBody: %s", err, string(body))
	}

	if !resp.Success {
		if resp.ExceptionClass != "" {
			return nil, &JavaError{
				Message:            resp.Error,
				ExceptionClass:     resp.ExceptionClass,
				ExceptionFullClass: resp.ExceptionFullClass,
			}
		}
		return nil, fmt.Errorf("java error: %s", resp.Error)
	}

	return resp.Result, nil
}

// InvokeAs calls a Java step and unmarshals result into target
// If result is nil, the return value is ignored
func (j *JavaInvoker) InvokeAs(ctx context.Context, stepName string, params map[string]any, result any) error {
	raw, err := j.Invoke(ctx, stepName, params)
	if err != nil {
		return err
	}

	// After a Java step completes (which may have committed data), invalidate
	// Go's GRV cache. Without this, the 100ms cache can serve stale versions
	// that predate Java's commit → Go reads miss Java's writes.
	if sharedDB != (gofdb.Database{}) {
		sharedDB.InvalidateGRVCache()
	}

	if result != nil && len(raw) > 0 && string(raw) != "null" && string(raw) != `""` {
		// Check if result is a proto.Message - use protojson for unmarshaling
		if msg, ok := result.(proto.Message); ok {
			if err := protojson.Unmarshal(raw, msg); err != nil {
				return fmt.Errorf("failed to unmarshal protobuf result: %w", err)
			}
		} else {
			// Use standard JSON unmarshaling for non-proto types
			if err := json.Unmarshal(raw, result); err != nil {
				return fmt.Errorf("failed to unmarshal result: %w", err)
			}
		}
	}

	return nil
}
