package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/gatewayconfig"
	"github.com/omniswitch-dev/omniswitch/internal/provider"
)

func TestRegisterInferenceProviderAccount(t *testing.T) {
	for _, runtime := range []string{"vllm", "ollama"} {
		t.Run(runtime, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					w.WriteHeader(http.StatusOK)
					return
				}
				if r.URL.Path != "/v1/chat/completions" {
					t.Errorf("chat path = %q, want /v1/chat/completions", r.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(provider.ChatResponse{
					ID: "local-chat", Model: "local-model",
					Choices: []provider.Choice{{Message: provider.Message{Role: "assistant", Content: "local response"}}},
				})
			}))
			defer server.Close()
			registry := provider.NewRegistry()
			defer registry.Close()
			registerProviderAccount(registry, gatewayconfig.ProviderAccount{
				Name: "local", Type: "inference",
				HealthInterval: &gatewayconfig.Duration{Duration: time.Hour},
				Replicas: []gatewayconfig.InferenceReplica{{
					URL: server.URL + "/v1", Runtime: runtime, Models: []string{"local-model"}, Priority: 3,
				}},
			})
			registered, err := registry.Get("local")
			if err != nil {
				t.Fatalf("registry.Get(inference): %v", err)
			}
			backend, ok := registered.(*provider.InferenceBackend)
			if !ok {
				t.Fatalf("provider type = %T, want *InferenceBackend", registered)
			}
			health := backend.ReplicaHealth()
			if len(health) != 1 || health[0].Runtime != runtime || health[0].Priority != 3 || health[0].Models[0] != "local-model" {
				t.Fatalf("replica config was not preserved: %+v", health)
			}
			if resolved, err := registry.ResolveModel("local-model"); err != nil || resolved != registered {
				t.Fatalf("ResolveModel = %v/%v, want registered inference provider", resolved, err)
			}
			response, meta, err := registered.ChatCompletion(context.Background(), provider.ChatRequest{
				Model: "local-model", Messages: []provider.Message{{Role: "user", Content: "hello"}},
			})
			if err != nil || response.ID != "local-chat" || meta.ProviderType != "inference" {
				t.Fatalf("inference response/meta/error = %+v/%+v/%v", response, meta, err)
			}
		})
	}
}
