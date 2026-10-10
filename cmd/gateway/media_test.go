package main

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omniswitch-dev/omniswitch/internal/gateway"
	"github.com/omniswitch-dev/omniswitch/internal/gatewayconfig"
	"github.com/omniswitch-dev/omniswitch/internal/provider"
	"github.com/omniswitch-dev/omniswitch/internal/router"
	"github.com/omniswitch-dev/omniswitch/internal/store"
)

func TestGatewayMediaRoutesMountedAndAuthenticated(t *testing.T) {
	registry := provider.NewRegistry()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	handler := gateway.New(registry, router.New(registry), st, nil)
	mux := http.NewServeMux()
	registerGatewayAPIRoutes(mux, handler)
	for _, endpoint := range []string{"/v1/images/generations", "/v1/audio/transcriptions", "/v1/audio/speech"} {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, endpoint, nil))
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("GET %s = %d, want mounted handler", endpoint, recorder.Code)
		}
		recorder = httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(`{}`)))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("POST %s = %d, want handler validation", endpoint, recorder.Code)
		}
		recorder = httptest.NewRecorder()
		gateway.NewAuthMiddleware(st, true).Wrap(mux).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(`{}`)))
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous POST %s = %d, want auth protection", endpoint, recorder.Code)
		}
	}
}

func TestUploadLimitRuntimeDefaultConfigEnvAndSummary(t *testing.T) {
	clearGatewayEnv(t)
	settings, err := loadRuntimeSettings()
	if err != nil || settings.maxUploadBytes != 25<<20 {
		t.Fatalf("default upload limit/error = %d/%v", settings.maxUploadBytes, err)
	}
	path := filepath.Join(t.TempDir(), "media.yaml")
	if err := os.WriteFile(path, []byte("apiVersion: omniswitch.dev/v1\nkind: GatewayConfig\ngateway:\n  max_upload_bytes: 1024\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMNISWITCH_CONFIG", path)
	settings, err = loadRuntimeSettings()
	if err != nil || settings.maxUploadBytes != 1024 {
		t.Fatalf("configured upload limit/error = %d/%v", settings.maxUploadBytes, err)
	}
	if summary := runtimeConfigSummary(settings, nil)["gateway"].(map[string]any); summary["max_upload_bytes"] != int64(1024) {
		t.Fatalf("upload summary = %v", summary)
	}
	t.Setenv("OMNISWITCH_MAX_UPLOAD_BYTES", "2048")
	settings, err = loadRuntimeSettings()
	if err != nil || settings.maxUploadBytes != 2048 {
		t.Fatalf("env upload limit/error = %d/%v", settings.maxUploadBytes, err)
	}
	for _, value := range []string{"0", "-1", "bad"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("OMNISWITCH_MAX_UPLOAD_BYTES", value)
			if _, err := loadRuntimeSettings(); err == nil || !strings.Contains(err.Error(), "OMNISWITCH_MAX_UPLOAD_BYTES") {
				t.Fatalf("invalid upload env = %v, want validation error", err)
			}
		})
	}
	cfg := gatewayconfig.Config{APIVersion: gatewayconfig.APIVersion, Kind: gatewayconfig.Kind, Gateway: gatewayconfig.Gateway{MaxUploadBytes: -1}}
	if err := gatewayconfig.Validate(cfg); err == nil || !strings.Contains(err.Error(), "max_upload_bytes") {
		t.Fatalf("negative upload config = %v, want validation error", err)
	}
}

func TestUploadLimitHotReloadAndEnvironmentPrecedence(t *testing.T) {
	clearGatewayEnv(t)
	registry := provider.NewRegistry()
	rtr := router.New(registry)
	handler := gateway.New(registry, rtr, nil, nil)
	base := runtimeSettings{routes: map[string]router.Route{}, maxUploadBytes: 1024}
	handler.SetMaxUploadBytes(base.maxUploadBytes)
	cfg := gatewayconfig.Config{Gateway: gatewayconfig.Gateway{MaxUploadBytes: 2048}}
	applyHotConfig(cfg, rtr, handler, base, nil)
	if status := checkReloadUpload(t, handler, 1024); status != http.StatusBadGateway {
		t.Fatalf("reload accepted upload = %d, want provider lookup after parsing", status)
	}
	applyHotConfig(gatewayconfig.Config{}, rtr, handler, base, nil)
	if status := checkReloadUpload(t, handler, 3072); status != http.StatusBadGateway {
		t.Fatalf("removed setting upload = %d, want default 25 MiB", status)
	}
	t.Setenv("OMNISWITCH_MAX_UPLOAD_BYTES", "500")
	applyHotConfig(cfg, rtr, handler, base, nil)
	if status := checkReloadUpload(t, handler, 1024); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("env-precedence upload = %d, want env byte limit", status)
	}
}

func checkReloadUpload(t *testing.T, handler *gateway.Handler, audioBytes int) int {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("model", "whisper-1")
	file, err := writer.CreateFormFile("file", "voice.wav")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write(bytes.Repeat([]byte("a"), audioBytes))
	_ = writer.Close()
	request := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	recorder := httptest.NewRecorder()
	handler.Transcription(recorder, request)
	return recorder.Code
}
