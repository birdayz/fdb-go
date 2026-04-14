package webhook_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/webhook"
)

func TestWebhookDelivery(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	var mu sync.Mutex
	var received *webhook.Payload

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var p webhook.Payload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		received = &p
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	payload := webhook.Payload{
		AlertID:     "alert-1",
		CustomerID:  "cust-1",
		MeterSlug:   "api_calls",
		Threshold:   1000,
		Usage:       1500,
		AlertType:   "usage",
		TriggeredAt: time.Now().UnixMilli(),
	}

	webhook.Deliver(context.Background(), server.URL, payload, slog.Default())

	// Wait for async delivery
	g.Eventually(func() *webhook.Payload {
		mu.Lock()
		defer mu.Unlock()
		return received
	}, 5*time.Second, 50*time.Millisecond).ShouldNot(BeNil())

	mu.Lock()
	defer mu.Unlock()
	g.Expect(received.AlertID).To(Equal("alert-1"))
	g.Expect(received.CustomerID).To(Equal("cust-1"))
	g.Expect(received.Usage).To(Equal(int64(1500)))
	g.Expect(received.Threshold).To(Equal(int64(1000)))
}

func TestWebhookRetry(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	var mu sync.Mutex
	attempts := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		a := attempts
		mu.Unlock()
		if a < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	webhook.Deliver(context.Background(), server.URL, webhook.Payload{
		AlertID: "retry-test",
	}, slog.Default())

	// Wait for retries (3 attempts: fail, fail, success)
	g.Eventually(func() int {
		mu.Lock()
		defer mu.Unlock()
		return attempts
	}, 15*time.Second, 100*time.Millisecond).Should(Equal(3))
}
