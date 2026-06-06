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

// defaultA3PoolSize is the number of fresh Java servers pre-spawned at suite
// startup as a warm buffer. It is purely a SPEED/RAM knob (more buffer = fewer
// on-demand spawns during the run = faster, at the cost of more live JVMs at
// startup); determinism does NOT depend on it (see JavaServerPool). Override
// with CONFORMANCE_A3_POOL_SIZE.
const defaultA3PoolSize = 16

// a3PoolSize returns the configured A3 server-pool buffer size
// (CONFORMANCE_A3_POOL_SIZE, else defaultA3PoolSize). Lower it on
// memory-constrained machines (each buffered server is a live JVM ~250-400MB);
// raise it on big hosts to pre-spawn more servers up front and cut the
// on-demand-spawn tail.
func a3PoolSize() int {
	if v := os.Getenv("CONFORMANCE_A3_POOL_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultA3PoolSize
}

// serverOrErr carries either a freshly-spawned server or the spawn error.
type serverOrErr struct {
	inv *JavaInvoker
	err error
}

// JavaServerPool hands each A3 scenario a FRESH, never-yet-used Java server so a
// scenario's result is a function of that scenario alone — fdb-relational
// 4.11.1.0 pollutes cross-query state on a long-lived server (see the
// determinism note in yamsql_cross_engine_conformance_test.go), so only a fresh
// process is clean.
//
// CRITICAL determinism rule: a server's QUERIES must never run while ANOTHER
// server is SPAWNING. All servers share the single FDB testcontainer; concurrent
// spawning + querying causes read-version (GRV) lag — a query's transaction can
// get a read version from BEFORE its own ephemeral-schema CREATE committed, so it
// sees no table and the Cascades planner throws a SPURIOUS UnableToPlanException.
// (Proven: a query that is 12/12 OK when servers spawn sequentially intermittently
// fails when servers spawn concurrently.) A naive background-refill pool violates
// this — replacements spawn while the next scenario queries — which is exactly
// what made A3 flaky run-to-run.
//
// So this pool spawns ONLY at two safe moments:
//  1. a startup buffer of `size` servers, spawned concurrently in
//     NewJavaServerPool and fully awaited BEFORE any scenario runs a query
//     (startup has no in-flight queries, so concurrent spawning is harmless); and
//  2. on demand, SYNCHRONOUSLY, inside Borrow — i.e. in a scenario's BeforeAll,
//     before that scenario's query, and (Ginkgo runs specs sequentially) while no
//     other scenario is querying.
//
// There is NO background refill. Buffered-but-idle servers generate no FDB load,
// so they never induce GRV lag for the active query. The result is fully
// order-independent and deterministic at ANY `size`.
type JavaServerPool struct {
	ready chan *JavaInvoker
}

// NewJavaServerPool spawns `size` servers concurrently and BLOCKS until every one
// is ready, so the buffer is full before any scenario queries. Concurrent
// spawning here is safe precisely because no scenario is querying yet.
func NewJavaServerPool(size int) *JavaServerPool {
	p := &JavaServerPool{ready: make(chan *JavaInvoker, size)}
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
		// (and surfaces any persistent error) when the buffer runs short.
	}
	return p
}

// Borrow returns a FRESH server: a pre-spawned buffered one if available, else a
// synchronously-spawned one. The synchronous spawn blocks the caller (a scenario
// BeforeAll) and therefore never overlaps another scenario's query.
func (p *JavaServerPool) Borrow() (*JavaInvoker, error) {
	select {
	case inv := <-p.ready:
		return inv, nil
	default:
		return NewIsolatedJavaInvoker() // synchronous — no query is running
	}
}

// Retire closes a borrowed (single-use) server.
func (p *JavaServerPool) Retire(inv *JavaInvoker) {
	if inv != nil {
		_ = inv.Close()
	}
}

// Shutdown closes every still-buffered server.
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
