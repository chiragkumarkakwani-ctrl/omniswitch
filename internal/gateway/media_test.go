package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/omniswitch-dev/omniswitch/internal/guardrail"
	"github.com/omniswitch-dev/omniswitch/internal/provider"
	"github.com/omniswitch-dev/omniswitch/internal/router"
	"github.com/omniswitch-dev/omniswitch/internal/store"
)

func TestImageGenerationJSONAndUnpricedLog(t *testing.T) {
	handler, st := newMediaTestHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/generations" || r.Header.Get("Authorization") != "Bearer provider-test-key" {
			t.Errorf("upstream path/auth = %s/%s", r.URL.Path, r.Header.Get("Authorization"))
		}
		var request provider.ImageRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.Model != "dall-e-3" || request.Prompt != "A mountain" || request.N != 2 || request.Size != "1024x1024" || request.Quality != "hd" || request.ResponseFormat != "b64_json" || request.User != "tester" {
			t.Errorf("upstream request = %+v", request)
		}
		_, _ = w.Write([]byte(`{"created":123,"data":[{"b64_json":"aW1hZ2U="},{"url":"https://images.example.com/a.png"}]}`))
	}), nil)
	request := mediaJSONRequest("/v1/images/generations", `{"model":"dall-e-3","prompt":"A mountain","n":2,"size":"1024x1024","quality":"hd","response_format":"b64_json","user":"tester"}`)
	request.Header.Set("X-Omniswitch-Metadata", `{"client":"media-test"}`)
	recorder := httptest.NewRecorder()
	handler.ImageGeneration(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "aW1hZ2U=") || recorder.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status/body/type = %d/%s/%s", recorder.Code, recorder.Body.String(), recorder.Header().Get("Content-Type"))
	}
	logs, total, err := st.ListLogs(context.Background(), store.LogFilter{Limit: 10})
	if err != nil || total != 1 || logs[0].Status != "success" || logs[0].Provider != "openai" || logs[0].Model != "dall-e-3" || logs[0].Cost != 0 {
		t.Fatalf("media logs = %+v, total/error = %d/%v", logs, total, err)
	}
	var metadata map[string]any
	_ = json.Unmarshal([]byte(logs[0].Metadata), &metadata)
	if metadata["cost_source"] != "unpriced" || metadata["client"] != "media-test" || metadata["media"].(map[string]any)["images"] != float64(2) {
		t.Fatalf("media metadata = %v", metadata)
	}
	if recorder.Header().Get("X-Omniswitch-Trace-Id") == "" || logs[0].TraceID != recorder.Header().Get("X-Omniswitch-Trace-Id") {
		t.Fatal("media trace header and request log are not correlated")
	}
}

func TestTranscriptionMultipartAndJSONTranscript(t *testing.T) {
	audio := []byte{0, 255, 1, 2, 3, 4, 5}
	handler, st := newMediaTestHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" || !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data;") {
			t.Errorf("upstream path/type = %s/%s", r.URL.Path, r.Header.Get("Content-Type"))
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			return
		}
		defer r.MultipartForm.RemoveAll()
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Error(err)
			return
		}
		defer file.Close()
		got, _ := io.ReadAll(file)
		if !bytes.Equal(got, audio) || header.Filename != "voice.wav" {
			t.Errorf("upstream file bytes/name = %v/%s", got, header.Filename)
		}
		for key, want := range map[string]string{"model": "whisper-1", "language": "en", "prompt": "Vocabulary", "response_format": "json", "temperature": "0.25"} {
			if r.FormValue(key) != want {
				t.Errorf("form[%s] = %q, want %q", key, r.FormValue(key), want)
			}
		}
		_, _ = w.Write([]byte(`{"text":"Hello transcript."}`))
	}), nil)
	request := mediaMultipartRequest(t, map[string]string{"model": "whisper-1", "language": "en", "prompt": "Vocabulary", "response_format": "json", "temperature": "0.25"}, audio)
	recorder := httptest.NewRecorder()
	handler.Transcription(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"text":"Hello transcript."`) {
		t.Fatalf("transcription status/body = %d/%s", recorder.Code, recorder.Body.String())
	}
	logs, _, err := st.ListLogs(context.Background(), store.LogFilter{Limit: 10})
	if err != nil || len(logs) != 1 || logs[0].Model != "whisper-1" || logs[0].Status != "success" {
		t.Fatalf("transcription logs/error = %+v/%v", logs, err)
	}
	if strings.Contains(logs[0].RequestBody, "voice.wav") || strings.Contains(logs[0].RequestBody, "255") {
		t.Fatal("uploaded binary/file metadata must not be copied into raw logs")
	}
}

func newMediaTestHandler(t *testing.T, upstream http.Handler, gr *guardrail.Engine) (*Handler, *store.Store) {
	t.Helper()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	registry := provider.NewRegistry()
	registry.Register(provider.NewOpenAI("provider-test-key", provider.WithOpenAIBaseURL(server.URL+"/v1")))
	st := newGatewayTestStore(t)
	return New(registry, router.New(registry), st, gr), st
}

func mediaJSONRequest(path, body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Omniswitch-Provider", "openai")
	return request
}

func mediaMultipartRequest(t *testing.T, fields map[string]string, audio []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if audio != nil {
		file, err := writer.CreateFormFile("file", "voice.wav")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(audio); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-Omniswitch-Provider", "openai")
	return request
}
