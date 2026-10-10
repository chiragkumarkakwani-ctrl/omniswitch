package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/omniswitch-dev/omniswitch/internal/provider"
	"github.com/omniswitch-dev/omniswitch/internal/router"
	"github.com/omniswitch-dev/omniswitch/internal/store"
)

func TestMediaUnsupportedProviderIs400(t *testing.T) {
	for _, name := range []string{"anthropic", "team"} {
		t.Run(name, func(t *testing.T) {
			registry := provider.NewRegistry()
			p := provider.Provider(provider.NewAnthropic("unused"))
			if name == "team" {
				p = provider.NewAlias(name, p)
			}
			registry.Register(p)
			st := newGatewayTestStore(t)
			handler := New(registry, router.New(registry), st, nil)
			for _, endpoint := range mediaEndpoints {
				request := mediaEndpointRequest(t, endpoint, "hello")
				request.Header.Set("X-Omniswitch-Provider", p.Name())
				recorder := httptest.NewRecorder()
				serveMediaEndpoint(handler, recorder, request)
				var envelope struct {
					Error struct{ Type, Code string }
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
					t.Fatal(err)
				}
				if recorder.Code != http.StatusBadRequest || envelope.Error.Type != "invalid_request_error" || envelope.Error.Code != "unsupported_endpoint" {
					t.Fatalf("%s unsupported status/body = %d/%s", endpoint, recorder.Code, recorder.Body.String())
				}
			}
			logs, total, err := st.ListLogs(context.Background(), store.LogFilter{Limit: 10, Statuses: []string{"error"}})
			if err != nil || total != 3 || len(logs) != 3 || logs[0].Provider != p.Name() {
				t.Fatalf("unsupported logs/total/error = %+v/%d/%v", logs, total, err)
			}
		})
	}
}

func TestTranscriptionUploadLimitKnownAndChunked(t *testing.T) {
	var calls atomic.Int32
	handler, _ := newMediaTestHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }), nil)
	if handler.maxUploadBytes.Load() != 25<<20 {
		t.Fatal("default upload limit must be 25 MiB")
	}
	for _, chunked := range []bool{false, true} {
		request := mediaMultipartRequest(t, map[string]string{"model": "whisper-1"}, []byte(strings.Repeat("a", 512)))
		handler.SetMaxUploadBytes(request.ContentLength - 10)
		if chunked {
			request.ContentLength = -1
		}
		recorder := httptest.NewRecorder()
		handler.Transcription(recorder, request)
		if recorder.Code != http.StatusRequestEntityTooLarge || !strings.Contains(recorder.Body.String(), "payload_too_large") {
			t.Fatalf("chunked=%t status/body = %d/%s, want bounded multipart body", chunked, recorder.Code, recorder.Body.String())
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("oversized uploads reached upstream %d times", calls.Load())
	}
}

func TestTranscriptionValidationAndTemporaryFileCleanup(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	handler, _ := newMediaTestHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"text":"hello"}`))
	}), nil)
	for _, fields := range []map[string]string{
		{}, {"model": "whisper-1", "temperature": "NaN"},
		{"model": "whisper-1", "temperature": "bad"}, {"model": "whisper-1", "temperature": "2"},
		{"model": "whisper-1", "response_format": "text"},
	} {
		recorder := httptest.NewRecorder()
		handler.Transcription(recorder, mediaMultipartRequest(t, fields, []byte("audio")))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("fields=%v status/body = %d/%s", fields, recorder.Code, recorder.Body.String())
		}
	}
	for _, audio := range [][]byte{nil, {}} {
		recorder := httptest.NewRecorder()
		handler.Transcription(recorder, mediaMultipartRequest(t, map[string]string{"model": "whisper-1"}, audio))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("missing/empty file status = %d", recorder.Code)
		}
	}
	recorder := httptest.NewRecorder()
	handler.Transcription(recorder, mediaMultipartRequest(t, map[string]string{"model": "whisper-1"}, []byte(strings.Repeat("a", (1<<20)+200))))
	if recorder.Code != http.StatusOK {
		t.Fatalf("spilled multipart status/body = %d/%s", recorder.Code, recorder.Body.String())
	}
	entries, err := os.ReadDir(tmp)
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary upload files remain: count/error = %d/%v", len(entries), err)
	}
}

func TestMediaJSONBoundsValidationAndUpstreamFailure(t *testing.T) {
	handler, _ := newMediaTestHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"upstream unavailable"}`))
	}), nil)
	for _, endpoint := range mediaEndpoints {
		recorder := httptest.NewRecorder()
		request := mediaEndpointRequest(t, endpoint, "hello")
		request.Method = http.MethodGet
		serveMediaEndpoint(handler, recorder, request)
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s method status = %d", endpoint, recorder.Code)
		}
		recorder = httptest.NewRecorder()
		serveMediaEndpoint(handler, recorder, mediaEndpointRequest(t, endpoint, "hello"))
		if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "provider_error") {
			t.Fatalf("%s upstream failure status/body = %d/%s", endpoint, recorder.Code, recorder.Body.String())
		}
	}
	for _, path := range []string{"/v1/images/generations", "/v1/audio/speech"} {
		for _, body := range []string{`{`, `{} {}`, `null`, `{}`, `{"model":"tts-1","voice":"alloy","input":"hello","speed":5}`} {
			if path == "/v1/images/generations" && strings.Contains(body, "speed") {
				continue
			}
			recorder := httptest.NewRecorder()
			serveMediaEndpoint(handler, recorder, mediaJSONRequest(path, body))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("%s body=%s validation status = %d", path, body, recorder.Code)
			}
		}
		handler.SetMaxRequestBytes(32)
		recorder := httptest.NewRecorder()
		serveMediaEndpoint(handler, recorder, mediaEndpointRequest(t, path, strings.Repeat("a", 64)))
		if recorder.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("%s oversized JSON status = %d", path, recorder.Code)
		}
		handler.SetMaxRequestBytes(10 << 20)
	}
}
