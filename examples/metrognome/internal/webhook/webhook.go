// Package webhook delivers alert notifications to configured webhook URLs.
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// Payload is the JSON body sent to webhook URLs when an alert triggers.
type Payload struct {
	AlertID     string `json:"alert_id"`
	CustomerID  string `json:"customer_id"`
	MeterSlug   string `json:"meter_slug"`
	Threshold   int64  `json:"threshold"`
	Usage       int64  `json:"usage"`
	AlertType   string `json:"alert_type"`
	TriggeredAt int64  `json:"triggered_at"`
}

// Deliver sends a webhook payload to the given URL.
// Retries up to 3 times with exponential backoff.
// Non-blocking: runs in a goroutine.
func Deliver(ctx context.Context, url string, payload Payload, log *slog.Logger) {
	go func() {
		body, err := json.Marshal(payload)
		if err != nil {
			log.Error("webhook marshal failed", "url", url, "error", err)
			return
		}

		var lastErr error
		for attempt := 0; attempt < 3; attempt++ {
			if attempt > 0 {
				delay := time.Duration(1<<uint(attempt)) * time.Second
				select {
				case <-time.After(delay):
				case <-ctx.Done():
					return
				}
			}

			req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
			if err != nil {
				log.Error("webhook request creation failed", "url", url, "error", err)
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("User-Agent", "metrognome-webhook/1.0")

			client := &http.Client{Timeout: 10 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				lastErr = err
				log.Warn("webhook delivery failed", "url", url, "attempt", attempt+1, "error", err)
				continue
			}
			resp.Body.Close()

			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				log.Info("webhook delivered", "url", url, "status", resp.StatusCode)
				return
			}

			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			log.Warn("webhook non-2xx response", "url", url, "status", resp.StatusCode, "attempt", attempt+1)
		}

		log.Error("webhook delivery exhausted retries", "url", url, "error", lastErr)
	}()
}
