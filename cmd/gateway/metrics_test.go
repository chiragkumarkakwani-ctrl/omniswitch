package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMetricsRuntimeSettings(t *testing.T) {
	for _, test := range []struct {
		name       string
		file       string
		publicEnv  string
		enabledEnv string
		wantPublic bool
		wantOn     bool
	}{
		{name: "default-private", wantOn: true},
		{name: "config-public", file: "true", wantPublic: true, wantOn: true},
		{name: "config-private", file: "false", wantOn: true},
		{name: "env-public", publicEnv: "true", wantPublic: true, wantOn: true},
		{name: "env-overrides-public", file: "true", publicEnv: "false", wantOn: true},
		{name: "env-overrides-private", file: "false", publicEnv: "true", wantPublic: true, wantOn: true},
		{name: "disabled-even-if-public", publicEnv: "true", enabledEnv: "false", wantPublic: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			clearGatewayEnv(t)
			if test.file != "" {
				path := filepath.Join(t.TempDir(), "gateway.yaml")
				if err := os.WriteFile(path, []byte("observability:\n  prometheus:\n    public: "+test.file+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("OMNISWITCH_CONFIG", path)
			}
			t.Setenv("OMNISWITCH_METRICS_PUBLIC", test.publicEnv)
			t.Setenv("OMNISWITCH_PROMETHEUS_ENABLED", test.enabledEnv)
			settings, err := loadRuntimeSettings()
			if err != nil {
				t.Fatal(err)
			}
			if settings.metricsPublic != test.wantPublic || settings.prometheusEnabled != test.wantOn {
				t.Errorf("metrics public/enabled = %v/%v, want %v/%v", settings.metricsPublic, settings.prometheusEnabled, test.wantPublic, test.wantOn)
			}
			metrics := runtimeConfigSummary(settings, nil)["metrics"].(map[string]any)
			if got := metrics["public"]; got != (test.wantPublic && test.wantOn) {
				t.Errorf("config summary metrics.public = %v", got)
			}
		})
	}
}

func TestMetricsJSONConfig(t *testing.T) {
	clearGatewayEnv(t)
	path := filepath.Join(t.TempDir(), "gateway.json")
	if err := os.WriteFile(path, []byte(`{"observability":{"prometheus":{"public":true}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMNISWITCH_CONFIG", path)
	settings, err := loadRuntimeSettings()
	if err != nil || !settings.metricsPublic {
		t.Fatalf("JSON public metrics config = %v, error %v", settings.metricsPublic, err)
	}
}

func TestPrometheusRouteAuthentication(t *testing.T) {
	for _, test := range []struct {
		name        string
		enabled     bool
		public      bool
		anonymous   int
		authed      int
		publicField bool
	}{
		{name: "private-by-default", enabled: true, anonymous: 401, authed: 200},
		{name: "public-opt-in", enabled: true, public: true, anonymous: 200, authed: 200, publicField: true},
		{name: "disabled", anonymous: 401, authed: 404},
		{name: "disabled-public", public: true, anonymous: 401, authed: 404},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newRouteTestServer(t, func(settings *runtimeSettings) {
				settings.prometheusEnabled = test.enabled
				settings.metricsPublic = test.public
			})
			for _, auth := range []bool{false, true} {
				want := test.anonymous
				if auth {
					want = test.authed
				}
				rec := serveRoute(h, http.MethodGet, "/metrics", auth)
				if rec.Code != want {
					t.Fatalf("scrape authenticated=%v returned %d, want %d: %s", auth, rec.Code, want, rec.Body.String())
				}
				if want == http.StatusOK && (!strings.Contains(rec.Body.String(), "# TYPE omniswitch_build_info gauge\n") || !strings.Contains(rec.Header().Get("Content-Type"), "version=0.0.4")) {
					t.Fatal("metrics route did not serve the registry")
				}
			}
			for _, path := range []string{"/api/config", "/v1/models", "/metrics/not-a-scrape"} {
				if rec := serveRoute(h, http.MethodGet, path, false); rec.Code != http.StatusUnauthorized {
					t.Errorf("public opt-in changed %s authentication: %d", path, rec.Code)
				}
			}
			if rec := serveRoute(h, http.MethodGet, "/healthz", false); rec.Code != http.StatusOK {
				t.Error("health probe stopped bypassing authentication")
			}
			var summary struct {
				Metrics struct {
					Public bool `json:"public"`
				} `json:"metrics"`
			}
			if err := json.Unmarshal(serveRoute(h, http.MethodGet, "/api/config", true).Body.Bytes(), &summary); err != nil {
				t.Fatal(err)
			}
			if summary.Metrics.Public != test.publicField {
				t.Errorf("GET /api/config metrics.public = %v, want %v", summary.Metrics.Public, test.publicField)
			}
		})
	}
}

func TestRequestMetricsIncludeMiddlewareRejections(t *testing.T) {
	for _, test := range []struct {
		name  string
		auth  bool
		codes []int
	}{
		{name: "auth", auth: true, codes: []int{http.StatusUnauthorized}},
		{name: "rate", codes: []int{http.StatusMethodNotAllowed, http.StatusTooManyRequests}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newRouteTestServer(t, func(settings *runtimeSettings) {
				settings.prometheusEnabled = true
				settings.metricsPublic = true
				settings.authEnabled = test.auth
				settings.rateLimitRequests = 1
			})
			for _, want := range test.codes {
				if rec := serveRoute(h, http.MethodGet, "/v1/chat/completions", false); rec.Code != want {
					t.Fatalf("middleware returned %d, want %d", rec.Code, want)
				}
			}
			for scrape := 0; scrape < 2; scrape++ {
				rec := serveRoute(h, http.MethodGet, "/metrics", false)
				if rec.Code != http.StatusOK {
					t.Fatal("public scrapes must bypass rate limiting")
				}
				want := `omniswitch_requests_total{endpoint="chat_completions",provider="",model="",status="error"} 1`
				if !strings.Contains(rec.Body.String(), want+"\n") {
					t.Errorf("missing middleware metric %s", want)
				}
				if !test.auth {
					want = `omniswitch_requests_total{endpoint="chat_completions",provider="",model="",status="limited"} 1`
					if !strings.Contains(rec.Body.String(), want+"\n") {
						t.Errorf("missing middleware metric %s", want)
					}
				}
			}
		})
	}
}
