package plandiff

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// invokeStep POSTs {step, params} to baseURL/invoke (the conformance
// server's RPC endpoint), unmarshals the success-result body into out,
// and returns a typed error on transport / serialization / Java-side
// failure.
//
// The conformance server response shape:
//
//	{
//	  "success":            bool,
//	  "result":             <raw JSON, type-specific>,
//	  "error":              string,            // when !success
//	  "exceptionClass":     string,            // simple Java class name
//	  "exceptionFullClass": string             // FQN
//	}
//
// On !success, the returned error embeds exceptionClass when present so
// callers (and the diff harness's classify functions) can distinguish
// planner errors (RelationalException, UnableToPlanException, etc.)
// from infrastructure errors (HTTP non-200, dial tcp).
//
// Pass nil for `out` if the caller doesn't need the result body parsed.
func invokeStep(ctx context.Context, hc *http.Client, baseURL, step string, params map[string]any, out any) error {
	type request struct {
		Step   string         `json:"step"`
		Params map[string]any `json:"params"`
	}
	type response struct {
		Success            bool            `json:"success"`
		Result             json.RawMessage `json:"result"`
		Error              string          `json:"error"`
		ExceptionClass     string          `json:"exceptionClass"`
		ExceptionFullClass string          `json:"exceptionFullClass"`
	}

	reqBody, err := json.Marshal(request{Step: step, Params: params})
	if err != nil {
		return fmt.Errorf("plandiff: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/invoke", bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("plandiff: build HTTP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("plandiff: HTTP POST: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("plandiff: read body: %w", err)
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("plandiff: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var r response
	if err := json.Unmarshal(body, &r); err != nil {
		return fmt.Errorf("plandiff: unmarshal response: %w (body=%q)", err, string(body))
	}
	if !r.Success {
		if r.ExceptionClass != "" {
			return fmt.Errorf("plandiff: java %s: %s", r.ExceptionClass, r.Error)
		}
		return fmt.Errorf("plandiff: java error: %s", r.Error)
	}
	if out != nil {
		if err := json.Unmarshal(r.Result, out); err != nil {
			return fmt.Errorf("plandiff: parse result into %T: %w (result=%q)", out, err, string(r.Result))
		}
	}
	return nil
}
