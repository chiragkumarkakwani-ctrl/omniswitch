package router

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/provider"
)

func TestCircuitSnapshotDetachedAndNonMutating(t *testing.T) {
	breaker := NewCircuitBreaker(2, time.Minute)
	breaker.RecordFailure("primary")
	breaker.RecordFailure("primary")
	snapshot := breaker.Snapshot()["primary"]
	if snapshot.State != CircuitOpen || snapshot.Failures != 2 || snapshot.Threshold != 2 || snapshot.OpenUntil == nil || snapshot.LastFailure == nil {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	*snapshot.OpenUntil = time.Time{}
	*snapshot.LastFailure = time.Time{}
	fresh := breaker.Snapshot()["primary"]
	if fresh.OpenUntil.IsZero() || fresh.LastFailure.IsZero() || fresh.OpenUntil.Location() != time.UTC || fresh.LastFailure.Location() != time.UTC {
		t.Fatal("snapshot aliases internal times")
	}
	breaker.mu.Lock()
	breaker.entries["primary"].openUntil = time.Now().Add(-time.Second)
	breaker.mu.Unlock()
	if breaker.Snapshot()["primary"].State != CircuitOpen {
		t.Fatal("inspection advanced circuit to half-open")
	}
	breaker.Reset("primary")
	reset := breaker.Snapshot()["primary"]
	if reset.State != CircuitClosed || reset.Failures != 0 || reset.OpenUntil != nil || reset.LastFailure != nil {
		t.Fatalf("reset = %+v", reset)
	}
	registry := provider.NewRegistry()
	registry.Register(&fakeProvider{name: "untouched", model: "model"})
	rtr := New(registry)
	rtr.SetCircuitBreaker(breaker)
	if row := rtr.CircuitSnapshot()["untouched"]; row.State != CircuitClosed || row.Threshold != 2 || row.Failures != 0 {
		t.Fatalf("untouched defaults = %+v", row)
	}
	if _, exists := breaker.Snapshot()["untouched"]; exists {
		t.Fatal("router inspection created breaker history")
	}
	if err := rtr.ResetCircuit("missing"); err == nil {
		t.Fatal("unknown provider reset accepted")
	}
	if err := rtr.ResetCircuit("untouched"); err != nil {
		t.Fatal(err)
	}
}

func TestRouterRecordsEachAttemptBackoffAndFallback(t *testing.T) {
	registry := provider.NewRegistry()
	registry.Register(&fakeProvider{name: "primary", model: "model", err: errors.New("upstream status 503")})
	registry.Register(&fakeProvider{name: "fallback", model: "model"})
	rtr := New(registry)
	rtr.SetRoute("model", Route{Provider: "primary", Fallbacks: []string{"fallback"}, MaxRetries: 1, RetryBackoff: "2ms"})
	names := []string{}
	attempts := []int{}
	var fallback bool
	start := time.Now()
	ctx := WithSpanRecorder(context.Background(), func(name, providerName, status string, stamp time.Time, metadata map[string]any) {
		if stamp.Before(start) || stamp.After(time.Now()) {
			t.Errorf("unmeasured start %s", stamp)
		}
		names = append(names, name)
		if name == "provider_attempt" {
			attempts = append(attempts, metadata["attempt"].(int))
			if providerName == "primary" && (metadata["http_status"] != 503 || status != "error") {
				t.Errorf("attempt = %s/%s/%v", providerName, status, metadata)
			}
			if providerName == "fallback" {
				fallback = metadata["fallback"] == true
			}
		}
		if name == "retry_backoff" && time.Since(stamp) < time.Millisecond {
			t.Errorf("backoff recorded before measured wait")
		}
		if name == "guardrail_fallback" && (metadata["from"] != "primary" || metadata["to"] != "fallback") {
			t.Errorf("fallback = %v", metadata)
		}
	})
	if _, _, err := rtr.Execute(ctx, provider.ChatRequest{Model: "model"}, ""); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"route", "provider_attempt", "retry_backoff", "provider_attempt", "guardrail_fallback", "provider_attempt"}) || !reflect.DeepEqual(attempts, []int{1, 2, 1}) || !fallback {
		t.Fatalf("stages=%v attempts=%v fallback=%v", names, attempts, fallback)
	}
}
