package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/omniswitch-dev/omniswitch/internal/telemetry"
)

func TestRegisterPrometheusServesSelectedRegistry(t *testing.T) {
	metrics := telemetry.NewMetrics()
	dash := New(nil)
	dash.SetMetrics(metrics)
	mux := http.NewServeMux()
	dash.RegisterPrometheus(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "version=0.0.4") {
		t.Fatalf("dashboard /metrics = %d %q, want the Prometheus text format", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), "omniswitch_") {
		t.Fatalf("dashboard /metrics body does not contain OmniSwitch families: %q", rec.Body.String())
	}
}
