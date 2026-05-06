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
	"strings"
	"sync"
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
}

var (
	globalInvoker     *JavaInvoker
	globalInvokerOnce sync.Once
	globalInvokerErr  error
)

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

	// Start server
	cmd := exec.Command(serverBin)
	cmd.Env = append(os.Environ(), r.Env()...)

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

	// Wait for server to be ready
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
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

	if j.baseURL == "" {
		return nil
	}

	// Try graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "POST", j.baseURL+"/shutdown", nil)
	_, _ = j.httpClient.Do(req)

	// Wait a bit for graceful shutdown
	time.Sleep(500 * time.Millisecond)

	// Force kill if still running
	if j.serverCmd.Process != nil {
		_ = j.serverCmd.Process.Kill()
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
