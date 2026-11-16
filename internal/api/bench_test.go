package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"
)

// latencySamples is how many calls TestAPILatencyBudget measures; p99 over
// 2000 samples is the number docs/reference/api.md budgets.
const latencySamples = 2000

// latencyBudget is the p99 the API may add on localhost.
const latencyBudget = time.Millisecond

// BenchmarkStatus measures the cheapest operation over a real unix socket,
// through the whole stack: listener, middlewares, handler, client.
func BenchmarkStatus(b *testing.B) {
	h := newHarness(b)
	opts, _ := h.serve(b, ServeOptions{})
	c, err := Dial(opts.Socket, "")
	if err != nil {
		b.Fatalf("dial: %v", err)
	}
	ctx := context.Background()
	if _, err := c.Status(ctx); err != nil {
		b.Fatalf("warm-up: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := c.Status(ctx); err != nil {
			b.Fatalf("status: %v", err)
		}
	}
}

// TestAPILatencyBudget guards the performance budget of docs/reference/api.md:
// p99 of the API-added latency under 1 ms. It measures the handler in process,
// with no network in the way, so what it reports is the API's own cost:
// authentication, scope, routing, the handler and the JSON.
func TestAPILatencyBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("latency budget: skipped in short mode")
	}
	if raceEnabled {
		t.Skip("latency budget: the race detector makes the number meaningless")
	}
	h := newHarness(t)

	samples := make([]time.Duration, 0, latencySamples)
	for i := 0; i < latencySamples; i++ {
		req := httptest.NewRequest(http.MethodGet, BaseURL+"/status", nil)
		req.Header.Set("Authorization", "Bearer "+h.ownerToken)
		rec := httptest.NewRecorder()
		start := time.Now()
		h.h.ServeHTTP(rec, req)
		samples = append(samples, time.Since(start))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	p50 := samples[len(samples)/2]
	p99 := samples[(len(samples)*99)/100]
	t.Logf("GET /v1/status over %d calls: p50 %v, p99 %v, max %v", len(samples), p50, p99, samples[len(samples)-1])
	if p99 >= latencyBudget {
		t.Fatalf("p99 = %v, budget is %v", p99, latencyBudget)
	}
}
