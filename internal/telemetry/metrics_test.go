package telemetry

import (
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func scrapeMetrics(t *testing.T, m *Metrics) string {
	t.Helper()
	var text strings.Builder
	if err := m.WritePrometheus(&text); err != nil {
		t.Fatal(err)
	}
	return text.String()
}

func TestMetricsExposition(t *testing.T) {
	m := NewMetrics()
	m.SetBuildInfo("v0.2.0", "abc123")
	m.RecordRequest("chat_completions", "openai", "gpt-test", "success", 10*time.Millisecond)
	m.RecordRequest("chat_completions", "openai", "gpt-test", "success", 3*time.Second)
	m.RecordUsage("openai", "gpt-test", 5, 7, 0.125)
	m.RecordGuardrailEvent("pii", "redact")
	m.RecordRateLimitRejection("requests")
	m.RegisterCircuitStateSource(func() map[string]int { return map[string]int{"openai": 2} })

	text := scrapeMetrics(t, m)
	for _, want := range []string{
		"# HELP omniswitch_requests_total Completed gateway API requests since process start.\n",
		"# TYPE omniswitch_requests_total counter\n",
		`omniswitch_requests_total{endpoint="chat_completions",provider="openai",model="gpt-test",status="success"} 2`,
		"# TYPE omniswitch_request_duration_seconds histogram\n",
		`omniswitch_request_duration_seconds_bucket{endpoint="chat_completions",provider="openai",le="0.005"} 0`,
		`omniswitch_request_duration_seconds_bucket{endpoint="chat_completions",provider="openai",le="0.01"} 1`,
		`omniswitch_request_duration_seconds_bucket{endpoint="chat_completions",provider="openai",le="2.5"} 1`,
		`omniswitch_request_duration_seconds_bucket{endpoint="chat_completions",provider="openai",le="5"} 2`,
		`omniswitch_request_duration_seconds_bucket{endpoint="chat_completions",provider="openai",le="+Inf"} 2`,
		`omniswitch_request_duration_seconds_sum{endpoint="chat_completions",provider="openai"} 3.01`,
		`omniswitch_request_duration_seconds_count{endpoint="chat_completions",provider="openai"} 2`,
		`omniswitch_tokens_total{provider="openai",model="gpt-test",direction="input"} 5`,
		`omniswitch_tokens_total{provider="openai",model="gpt-test",direction="output"} 7`,
		`omniswitch_cost_usd_total{provider="openai",model="gpt-test"} 0.125`,
		`omniswitch_guardrail_events_total{type="pii",action="redact"} 1`,
		`omniswitch_rate_limit_rejections_total{policy="requests"} 1`,
		"# TYPE omniswitch_provider_circuit_state gauge\n",
		`omniswitch_provider_circuit_state{provider="openai"} 2`,
		`omniswitch_build_info{version="v0.2.0",commit="abc123"} 1`,
	} {
		if !strings.Contains(text, strings.TrimSuffix(want, "\n")+"\n") {
			t.Errorf("missing %q in scrape:\n%s", want, text)
		}
	}
	if strings.Contains(text, `version="dev"`) {
		t.Fatal("SetBuildInfo retained stale labels")
	}
	if second := scrapeMetrics(t, m); second != text {
		t.Fatal("scrapes without updates must be identical")
	}
}

func TestMetricsEscapingAndOrdering(t *testing.T) {
	first, second := NewMetrics(), NewMetrics()
	labels := []string{"z-model", "model\"with\nnewline\\slash", "a-model", "日本語"}
	for _, model := range labels {
		first.RecordRequest("responses", "p", model, "success", time.Second)
	}
	for index := len(labels) - 1; index >= 0; index-- {
		second.RecordRequest("responses", "p", labels[index], "success", time.Second)
	}
	text := scrapeMetrics(t, first)
	if got := scrapeMetrics(t, second); got != text {
		t.Fatal("insertion order changed exposition")
	}
	if !strings.Contains(text, `model="model\"with\nnewline\\slash"`) {
		t.Fatalf("label quotes, newlines, and backslashes are not escaped: %s", text)
	}
	previous := ""
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "# HELP ") {
			continue
		}
		name := strings.Fields(line)[2]
		if name <= previous {
			t.Fatal("metric families are not sorted")
		}
		previous = name
	}
	if escapeHelp("back\\slash\nnext") != `back\\slash\nnext` {
		t.Fatal("HELP text must escape backslashes and newlines")
	}
}

func TestMetricsCountersIgnoreInvalidUsage(t *testing.T) {
	m := NewMetrics()
	m.RecordUsage("p", "m", 4, 6, 1)
	m.RecordUsage("p", "m", -1, -2, math.NaN())
	m.RecordUsage("p", "m", 0, 0, math.Inf(1))
	m.RecordUsage("p", "m", 0, 0, -3)
	text := scrapeMetrics(t, m)
	for _, want := range []string{
		`omniswitch_tokens_total{provider="p",model="m",direction="input"} 4`,
		`omniswitch_tokens_total{provider="p",model="m",direction="output"} 6`,
		`omniswitch_cost_usd_total{provider="p",model="m"} 1`,
	} {
		if !strings.Contains(text, want+"\n") {
			t.Errorf("counter changed after invalid update; want %s", want)
		}
	}
}

func TestMetricsCircuitSnapshotIsFresh(t *testing.T) {
	m := NewMetrics()
	states := map[string]int{"closed": 0, "half-open": 1, "open": 2, "invalid": 3}
	m.RegisterCircuitStateSource(func() map[string]int {
		// The hook must not run under the metrics lock.
		m.RecordGuardrailEvent("hook", "log")
		return states
	})
	text := scrapeMetrics(t, m)
	for name, state := range states {
		if got := strings.Contains(text, `omniswitch_provider_circuit_state{provider="`+name+`"}`); got != (state <= 2) {
			t.Errorf("provider %s inclusion = %v", name, got)
		}
	}
	delete(states, "open")
	states["half-open"] = 0
	text = scrapeMetrics(t, m)
	if strings.Contains(text, `provider="open"`) || !strings.Contains(text, `omniswitch_provider_circuit_state{provider="half-open"} 0`) {
		t.Fatal("scrape retained stale circuit state")
	}
	m.RegisterCircuitStateSource(nil)
	if strings.Contains(scrapeMetrics(t, m), "omniswitch_provider_circuit_state{") {
		t.Fatal("unregistered circuit hook still emits samples")
	}
}

func TestMetricsConcurrentUpdatesAndScrapes(t *testing.T) {
	m := NewMetrics()
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for iteration := 0; iteration < 100; iteration++ {
				m.RecordRequest("messages", "p", "m", "success", time.Second)
				m.RecordUsage("p", "m", 2, 3, 0.25)
				m.RecordGuardrailEvent("pii", "deny")
				m.RecordRateLimitRejection("rpm")
				m.RegisterCircuitStateSource(func() map[string]int { return map[string]int{"p": 0} })
				var text strings.Builder
				if err := m.WritePrometheus(&text); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	workers.Wait()
	text := scrapeMetrics(t, m)
	for _, want := range []string{
		`omniswitch_requests_total{endpoint="messages",provider="p",model="m",status="success"} 800`,
		`omniswitch_request_duration_seconds_count{endpoint="messages",provider="p"} 800`,
		`omniswitch_request_duration_seconds_sum{endpoint="messages",provider="p"} 800`,
		`omniswitch_tokens_total{provider="p",model="m",direction="input"} 1600`,
		`omniswitch_tokens_total{provider="p",model="m",direction="output"} 2400`,
		`omniswitch_cost_usd_total{provider="p",model="m"} 200`,
		`omniswitch_guardrail_events_total{type="pii",action="deny"} 800`,
		`omniswitch_rate_limit_rejections_total{policy="rpm"} 800`,
	} {
		if !strings.Contains(text, want+"\n") {
			t.Errorf("concurrent updates lost samples; want %s", want)
		}
	}
}

func TestMetricsHTTPAndWriteError(t *testing.T) {
	m := NewMetrics()
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
		rec := httptest.NewRecorder()
		m.ServeHTTP(rec, httptest.NewRequest(method, "/metrics", nil))
		if method == http.MethodPost {
			if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
				t.Fatal("unsupported method should return 405 with Allow")
			}
			continue
		}
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != prometheusContentType {
			t.Fatalf("scrape status/content type = %d/%q", rec.Code, rec.Header().Get("Content-Type"))
		}
		if method == http.MethodHead && rec.Body.Len() != 0 {
			t.Fatal("HEAD must not write a body")
		}
	}
	if err := m.WritePrometheus(failingMetricsWriter{}); err == nil {
		t.Fatal("WritePrometheus must report writer errors")
	}
}

type failingMetricsWriter struct{}

func (failingMetricsWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}
