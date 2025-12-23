package helpers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// JavaInvoker provides generic method invocation on Java conformance steps
type JavaInvoker struct {
	gradlew string
	workdir string
	timeout time.Duration
}

// NewJavaInvoker creates a new Java invoker with default settings
func NewJavaInvoker() *JavaInvoker {
	// workdir is relative to where tests run (the conformance directory)
	return &JavaInvoker{
		gradlew: "gradlew",
		workdir: "java",
		timeout: 2 * time.Minute, // FDB operations can take time
	}
}

// Request is the JSON structure sent to Java
type Request struct {
	Step string      `json:"step"`
	Args interface{} `json:"args"`
}

// Response is the JSON structure returned from Java
type Response struct {
	Success bool            `json:"success"`
	Result  json.RawMessage `json:"result"`
	Error   *ErrorDetail    `json:"error"`
}

// ErrorDetail contains error information from Java
type ErrorDetail struct {
	Message    string `json:"message"`
	StackTrace string `json:"stackTrace"`
}

// Invoke calls a Java conformance step with generic JSON args
// Returns the raw JSON result that can be unmarshaled by the caller
func (j *JavaInvoker) Invoke(ctx context.Context, stepName string, args interface{}) (json.RawMessage, error) {
	// Build request
	req := Request{
		Step: stepName,
		Args: args,
	}

	reqBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Execute Java
	ctx, cancel := context.WithTimeout(ctx, j.timeout)
	defer cancel()

	// Tests run from the conformance package directory, so paths are relative to that
	// Get absolute path to gradlew since exec.Command needs it
	wd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("failed to get working directory: %w", err)
	}
	absGradlewPath := filepath.Join(wd, j.workdir, j.gradlew)
	absWorkdir := filepath.Join(wd, j.workdir)

	cmd := exec.CommandContext(ctx, absGradlewPath, "runConformance", "--console=plain")
	cmd.Dir = absWorkdir

	var stdout, stderr bytes.Buffer
	cmd.Stdin = bytes.NewReader(reqBytes)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("java execution failed: %w\nstderr: %s", err, stderr.String())
	}

	// Parse response - find the JSON line in Gradle output
	// Gradle writes task output to stdout, we need to find the line with our JSON
	output := stdout.String()
	lines := strings.Split(output, "\n")
	var jsonLine string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Look for a line starting with { and ending with } (our JSON response)
		if strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") {
			jsonLine = trimmed
			break
		}
	}

	if jsonLine == "" {
		return nil, fmt.Errorf("no JSON response found in output: %s", output)
	}

	var resp Response
	if err := json.Unmarshal([]byte(jsonLine), &resp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w\nfull output: %s\njson line: %s", err, output, jsonLine)
	}

	if !resp.Success {
		if resp.Error != nil {
			return nil, fmt.Errorf("java error: %s\n%s", resp.Error.Message, resp.Error.StackTrace)
		}
		return nil, fmt.Errorf("java step failed with no error details")
	}

	return resp.Result, nil
}

// InvokeAs calls a Java step and unmarshals result into target
// If result is nil, the return value is ignored
func (j *JavaInvoker) InvokeAs(ctx context.Context, stepName string, args interface{}, result interface{}) error {
	raw, err := j.Invoke(ctx, stepName, args)
	if err != nil {
		return err
	}

	if result != nil && len(raw) > 0 && string(raw) != "null" {
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
