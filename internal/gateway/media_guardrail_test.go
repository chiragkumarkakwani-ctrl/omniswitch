package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/guardrail"
	"github.com/omniswitch-dev/omniswitch/internal/provider"
	"github.com/omniswitch-dev/omniswitch/internal/store"
)

func TestMediaInputGuardrailsDenyBeforeUpstream(t *testing.T) {
	var calls atomic.Int32
	gr := guardrail.NewEngineWithConfig(guardrail.Config{Rules: []guardrail.Rule{{Name: "blocked-input", Stage: "input", Pattern: "BLOCK", Action: "deny", Message: "blocked media input"}}})
	handler, st := newMediaTestHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }), gr)
	for _, endpoint := range mediaEndpoints {
		request := mediaEndpointRequest(t, endpoint, "BLOCK")
		recorder := httptest.NewRecorder()
		serveMediaEndpoint(handler, recorder, request)
		if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "guardrail_triggered") {
			t.Fatalf("%s status/body = %d/%s, want guardrail denial", endpoint, recorder.Code, recorder.Body.String())
		}
	}
	logs, total, err := st.ListLogs(context.Background(), 10, 0, "", "denied")
	if err != nil || total != 3 || len(logs) != 3 || calls.Load() != 0 {
		t.Fatalf("denied logs/total/error/upstream calls = %d/%d/%v/%d", len(logs), total, err, calls.Load())
	}
}

func TestMediaTranscriptOutputGuardrailsDenyAndRedact(t *testing.T) {
	for _, action := range []string{"deny", "redact"} {
		t.Run(action, func(t *testing.T) {
			gr := guardrail.NewEngineWithConfig(guardrail.Config{Rules: []guardrail.Rule{{Name: "private-output", Stage: "output", Pattern: "PRIVATE", Action: action, Message: "private transcript blocked"}}})
			handler, st := newMediaTestHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"text":"PRIVATE transcription"}`))
			}), gr)
			if action == "deny" {
				handler.SetLogPayloads(false)
			}
			recorder := httptest.NewRecorder()
			request := mediaMultipartRequest(t, map[string]string{"model": "whisper-1"}, []byte("audio"))
			handler.Transcription(recorder, request)
			want := http.StatusOK
			if action == "deny" {
				want = http.StatusForbidden
			}
			if recorder.Code != want || strings.Contains(recorder.Body.String(), "PRIVATE") {
				t.Fatalf("output %s status/body = %d/%s", action, recorder.Code, recorder.Body.String())
			}
			logs, _, err := st.ListLogs(context.Background(), 10, 0, "", "")
			if err != nil || len(logs) != 1 || strings.Contains(logs[0].ResponseBody, "PRIVATE") {
				t.Fatalf("output logs/error = %+v/%v", logs, err)
			}
			if action == "redact" && (!strings.Contains(recorder.Body.String(), "REDACTED BY OMNISWITCH") || !strings.Contains(logs[0].ResponseBody, "REDACTED BY OMNISWITCH")) {
				t.Fatal("transcript must be redacted before returning or logging")
			}
		})
	}
}

func TestMediaInputRedactionAffectsProviderRequest(t *testing.T) {
	gr := guardrail.NewEngineWithConfig(guardrail.Config{Rules: []guardrail.Rule{{Name: "private-input", Stage: "input", Pattern: "PRIVATE", Action: "redact"}}})
	handler, _ := newMediaTestHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request provider.ImageRequest
		_ = json.NewDecoder(r.Body).Decode(&request)
		if request.Prompt != "[REDACTED BY OMNISWITCH]" {
			t.Errorf("upstream prompt = %q, want redacted input", request.Prompt)
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}), gr)
	recorder := httptest.NewRecorder()
	handler.ImageGeneration(recorder, mediaJSONRequest("/v1/images/generations", `{"model":"dall-e-3","prompt":"PRIVATE input"}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("redacted input status/body = %d/%s", recorder.Code, recorder.Body.String())
	}
}

func TestMediaMonthlyBudgetsDenyWith402(t *testing.T) {
	for _, budget := range []string{"cost", "tokens"} {
		t.Run(budget, func(t *testing.T) {
			var calls atomic.Int32
			handler, st := newMediaTestHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }), nil)
			key := store.APIKey{ID: "media-budget", Name: "budget", KeyHash: "budget-hash", KeyPrefix: "test", Enabled: true, CreatedAt: time.Now()}
			if budget == "cost" {
				key.MonthlyCostBudget = 0.01
			} else {
				key.MonthlyTokenBudget = 10
			}
			if err := st.InsertAPIKey(context.Background(), key); err != nil {
				t.Fatal(err)
			}
			if err := st.InsertLog(context.Background(), store.RequestLog{ID: "prior-usage", Timestamp: time.Now(), APIKeyID: key.ID, Status: "success", Decision: "ALLOW", Cost: 0.02, TotalTokens: 20}); err != nil {
				t.Fatal(err)
			}
			for _, endpoint := range mediaEndpoints {
				request := mediaEndpointRequest(t, endpoint, "hello")
				request.Header.Set("X-Omniswitch-Key-Id", key.ID)
				recorder := httptest.NewRecorder()
				serveMediaEndpoint(handler, recorder, request)
				if recorder.Code != http.StatusPaymentRequired || !strings.Contains(recorder.Body.String(), "budget_exceeded") {
					t.Fatalf("%s budget status/body = %d/%s", endpoint, recorder.Code, recorder.Body.String())
				}
			}
			if calls.Load() != 0 {
				t.Fatalf("budget-denied upstream calls = %d, want zero", calls.Load())
			}
		})
	}
}

var mediaEndpoints = []string{"/v1/images/generations", "/v1/audio/transcriptions", "/v1/audio/speech"}

func mediaEndpointRequest(t *testing.T, endpoint, text string) *http.Request {
	t.Helper()
	switch endpoint {
	case "/v1/images/generations":
		payload, _ := json.Marshal(map[string]string{"model": "dall-e-3", "prompt": text})
		return mediaJSONRequest(endpoint, string(payload))
	case "/v1/audio/transcriptions":
		return mediaMultipartRequest(t, map[string]string{"model": "whisper-1", "prompt": text}, []byte("audio"))
	default:
		payload, _ := json.Marshal(map[string]string{"model": "tts-1", "input": text, "voice": "alloy"})
		return mediaJSONRequest(endpoint, string(payload))
	}
}

func serveMediaEndpoint(handler *Handler, w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/images/generations":
		handler.ImageGeneration(w, r)
	case "/v1/audio/transcriptions":
		handler.Transcription(w, r)
	default:
		handler.Speech(w, r)
	}
}
