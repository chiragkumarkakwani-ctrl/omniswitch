package provider

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestInferenceReplicaHealth(t *testing.T) {
	for _, runtime := range []string{"vllm", "ollama"} {
		t.Run(runtime, func(t *testing.T) {
			wantPath := "/health"
			if runtime == "ollama" {
				wantPath = "/api/tags"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != wantPath {
					t.Errorf("probe path = %q, want %q", r.URL.Path, wantPath)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_, _ = w.Write([]byte(`{"queue_len":12}`))
			}))
			defer server.Close()
			backend := NewInferenceBackend("local", []InferenceReplica{{
				URL: server.URL + "/v1", Runtime: runtime, Models: []string{"local-model"}, Priority: 2,
			}}, time.Hour)
			defer backend.Close()
			snapshot := waitForInferenceProbe(t, backend)[0]
			if snapshot.Healthy == nil || !*snapshot.Healthy || snapshot.Runtime != runtime || snapshot.QueueLen != 12 || snapshot.Priority != 2 {
				t.Fatalf("replica health = %+v, want healthy %s with queue and priority", snapshot, runtime)
			}
			if snapshot.LatencyMS <= 0 || snapshot.LastCheck == nil {
				t.Fatalf("missing health observation: %+v", snapshot)
			}
			// The caller cannot mutate the backend's model list or health state.
			snapshot.Models[0] = "changed"
			*snapshot.Healthy = false
			*snapshot.LastCheck = time.Time{}
			fresh := backend.ReplicaHealth()[0]
			if fresh.Models[0] != "local-model" || !*fresh.Healthy || fresh.LastCheck.IsZero() {
				t.Fatalf("snapshot exposed mutable backend state: %+v", fresh)
			}
		})
	}
}

func TestInferenceReplicaHealthRejectsHTTPFailures(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
			}))
			defer server.Close()
			backend := NewInferenceBackend("local", []InferenceReplica{{URL: server.URL + "/v1", Runtime: "vllm"}}, time.Hour)
			defer backend.Close()
			health := waitForInferenceProbe(t, backend)[0]
			if health.Healthy == nil || *health.Healthy {
				t.Fatalf("status %d counted as healthy: %+v", code, health)
			}
			if health.LatencyMS >= 3000 || health.QueueLen != 0 {
				t.Fatalf("failed probe exposed synthetic score penalties as measurements: %+v", health)
			}
		})
	}
}

func TestInferenceReplicaHealthUnknownAndRequestLatency(t *testing.T) {
	backend := &InferenceBackend{
		replicas: []InferenceReplica{{URL: "http://local/v1"}},
		health: &healthTracker{
			latency: map[string]time.Duration{}, queueLen: map[string]int{},
			lastCheck: map[string]time.Time{}, healthy: map[string]bool{},
		},
	}
	backend.health.recordLatency("http://local/v1", time.Millisecond)
	health := backend.ReplicaHealth()[0]
	if health.Healthy != nil || health.LastCheck != nil {
		t.Fatalf("request latency fabricated a health probe: %+v", health)
	}
}

func TestInferenceCloseCancelsProbe(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	backend := NewInferenceBackend("local", []InferenceReplica{{URL: server.URL}}, time.Hour)
	defer backend.Close()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("health probe did not start")
	}
	done := make(chan struct{})
	go func() {
		_ = backend.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel the in-flight health probe")
	}
}

func TestInferenceSelectionPrefersHealthyReplica(t *testing.T) {
	backend := &InferenceBackend{
		replicas: []InferenceReplica{
			{URL: "http://unhealthy", Models: []string{"local-model"}},
			{URL: "http://healthy", Models: []string{"local-model"}},
		},
		health: &healthTracker{
			latency: map[string]time.Duration{}, queueLen: map[string]int{},
			lastCheck: map[string]time.Time{}, healthy: map[string]bool{},
		},
	}
	backend.health.recordProbe("http://unhealthy", time.Millisecond, 0, false)
	backend.health.recordProbe("http://healthy", time.Minute, 0, true)
	replica, ok := backend.selectReplica("local-model")
	if !ok || replica.URL != "http://healthy" {
		t.Fatalf("selected replica = %+v/%v, want healthy replica", replica, ok)
	}
	if _, ok := backend.selectReplica("unregistered-model"); ok {
		t.Fatal("selected a replica for an unsupported model")
	}
}

func waitForInferenceProbe(t *testing.T, backend *InferenceBackend) []InferenceReplicaHealth {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		health := backend.ReplicaHealth()
		if len(health) > 0 && health[0].LastCheck != nil {
			return health
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("health probe did not complete")
	return nil
}
