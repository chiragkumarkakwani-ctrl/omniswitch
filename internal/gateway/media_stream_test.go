package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/provider"
	"github.com/omniswitch-dev/omniswitch/internal/router"
	"github.com/omniswitch-dev/omniswitch/internal/store"
)

func TestSpeechStreamsBinaryBeforeUpstreamCompletes(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	handler, st := newMediaTestHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/speech" {
			t.Errorf("upstream speech path = %s", r.URL.Path)
		}
		var request provider.SpeechRequest
		_ = json.NewDecoder(r.Body).Decode(&request)
		if request.Model != "tts-1" || request.Input != "你好a" || request.Voice != "alloy" || request.ResponseFormat != "wav" || request.Speed != 1.25 {
			t.Errorf("upstream speech request = %+v", request)
		}
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write([]byte{0, 1, 255, 2})
		w.(http.Flusher).Flush()
		select {
		case <-release:
			_, _ = w.Write([]byte{3, 4, 5})
		case <-r.Context().Done():
		}
	}), nil)
	handler.SetLogPayloads(false)
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.Speech(w, r)
		close(done)
	}))
	defer func() { unblock(); server.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/audio/speech", strings.NewReader(`{"model":"tts-1","input":"你好a","voice":"alloy","response_format":"wav","speed":1.25}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Omniswitch-Provider", "openai")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("speech did not start before upstream completion: %v", err)
	}
	defer response.Body.Close()
	prefix := make([]byte, 4)
	if _, err := io.ReadFull(response.Body, prefix); err != nil || !bytes.Equal(prefix, []byte{0, 1, 255, 2}) {
		t.Fatalf("stream prefix/error = %v/%v", prefix, err)
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "audio/wav" {
		t.Fatalf("speech status/type = %d/%s", response.StatusCode, response.Header.Get("Content-Type"))
	}
	select {
	case <-done:
		t.Fatal("gateway completed while upstream tail was still blocked")
	default:
	}
	unblock()
	tail, err := io.ReadAll(response.Body)
	if err != nil || !bytes.Equal(tail, []byte{3, 4, 5}) {
		t.Fatalf("stream tail/error = %v/%v", tail, err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("speech did not finish and log")
	}
	logs, total, err := st.ListLogs(context.Background(), store.LogFilter{Limit: 10})
	if err != nil || total != 1 || logs[0].Status != "success" || logs[0].RequestBody != "" || logs[0].ResponseBody != "" {
		t.Fatalf("speech logs/total/error = %+v/%d/%v", logs, total, err)
	}
	var metadata map[string]any
	_ = json.Unmarshal([]byte(logs[0].Metadata), &metadata)
	if metadata["media"].(map[string]any)["characters"] != float64(3) {
		t.Fatalf("speech usage = %v, want Unicode character count", metadata)
	}
}

func TestSpeechReadFailureClosesUpstreamWithoutAppendingJSON(t *testing.T) {
	body := &failingMediaBody{}
	registry := provider.NewRegistry()
	registry.Register(failingSpeechProvider{gatewayProvider: gatewayProvider{name: "audio", model: "tts-1"}, body: body})
	st := newGatewayTestStore(t)
	handler := New(registry, router.New(registry), st, nil)
	request := mediaEndpointRequest(t, "/v1/audio/speech", "hello")
	request.Header.Set("X-Omniswitch-Provider", "audio")
	recorder := httptest.NewRecorder()
	handler.Speech(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "audio" || !body.closed.Load() {
		t.Fatalf("partial stream status/body/closed = %d/%q/%t", recorder.Code, recorder.Body.String(), body.closed.Load())
	}
	logs, total, err := st.ListLogs(context.Background(), store.LogFilter{Limit: 10, Statuses: []string{"error"}})
	if err != nil || total != 1 || !strings.Contains(logs[0].ErrorMessage, "speech stream failed") {
		t.Fatalf("stream failure logs/total/error = %+v/%d/%v", logs, total, err)
	}
}

type failingMediaBody struct{ closed atomic.Bool }

func (b *failingMediaBody) Read(p []byte) (int, error) {
	return copy(p, "audio"), errors.New("upstream read failed")
}

func (b *failingMediaBody) Close() error { b.closed.Store(true); return nil }

type failingSpeechProvider struct {
	gatewayProvider
	body io.ReadCloser
}

func (p failingSpeechProvider) Transcription(context.Context, provider.TranscriptionRequest) (provider.TranscriptionResponse, provider.ProviderMeta, error) {
	return provider.TranscriptionResponse{}, provider.ProviderMeta{}, errors.New("not implemented")
}

func (p failingSpeechProvider) Speech(ctx context.Context, request provider.SpeechRequest) (io.ReadCloser, string, provider.ProviderMeta, error) {
	return p.body, "audio/ogg", provider.ProviderMeta{Provider: p.Name(), Model: request.Model}, nil
}
