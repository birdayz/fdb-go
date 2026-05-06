package plandiff

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// JavaError carries a Java conformance-server error with structured
// fields so cross-engine harnesses can match on SQLSTATE without
// parsing the message text. Returned by invokeStep when the Java
// side reported `success: false`.
type JavaError struct {
	Message            string // server-side root-cause message
	ExceptionClass     string // simple Java class name (e.g. "RelationalException")
	ExceptionFullClass string // FQN
	SQLState           string // SQLSTATE if Java extracted one (SQLException / RelationalException), else ""
}

func (e *JavaError) Error() string {
	if e.ExceptionClass != "" {
		return "plandiff: java " + e.ExceptionClass + ": " + e.Message
	}
	return "plandiff: java error: " + e.Message
}

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
//	  "exceptionFullClass": string,            // FQN
//	  "sqlState":           string             // SQLSTATE when extractable
//	}
//
// On !success, the returned error is a *JavaError carrying the
// structured fields. Callers can `errors.As(err, &je)` to inspect the
// SQLState (used by the cross-engine error_code harness) or the
// exception class (used by the diff harness's classify functions to
// distinguish planner errors from infrastructure errors).
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
		SQLState           string          `json:"sqlState"`
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
		return &JavaError{
			Message:            r.Error,
			ExceptionClass:     r.ExceptionClass,
			ExceptionFullClass: r.ExceptionFullClass,
			SQLState:           r.SQLState,
		}
	}
	if out != nil {
		if err := json.Unmarshal(r.Result, out); err != nil {
			return fmt.Errorf("plandiff: parse result into %T: %w (result=%q)", out, err, string(r.Result))
		}
	}
	return nil
}
