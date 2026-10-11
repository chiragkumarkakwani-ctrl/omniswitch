package telemetry

import (
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
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
		"# HELP omniswitch_requests_total Completed gateway API requests since process start. At most 2000 label sets; overflow uses other for every family label value.\n",
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

func assertMetricsSeriesCapTotals(t *testing.T, m *Metrics, observations int) {
	t.Helper()
	for name, want := range map[string]float64{
		requestsMetric:   float64(observations),
		durationMetric:   float64(observations),
		tokensMetric:     float64(observations * 5),
		costMetric:       float64(observations) * 0.25,
		guardrailsMetric: float64(observations),
		rateLimitsMetric: float64(observations),
	} {
		family := m.families[name]
		if got := len(family.series); got != maxMetricSeriesPerFamily {
			t.Errorf("%s retained %d label sets, want %d including overflow", name, got, maxMetricSeriesPerFamily)
		}
		var total float64
		for _, sample := range family.series {
			if family.kind != "histogram" {
				total += sample.value
				continue
			}
			total += float64(sample.count)
			if sample.sum != float64(sample.count) {
				t.Errorf("histogram lost duration sum: %v for %d one-second observations", sample.sum, sample.count)
			}
			for index, bound := range durationBuckets {
				var wantBucket uint64
				if bound >= 1 {
					wantBucket = sample.count
				}
				if sample.buckets[index] != wantBucket {
					t.Errorf("histogram lost bucket observations at %v", bound)
				}
			}
		}
		if total != want {
			t.Errorf("%s total = %v, want %v; capped labels must not lose updates", name, total, want)
		}
	}
	for name, family := range m.families {
		if len(family.series) > maxMetricSeriesPerFamily {
			t.Errorf("%s exceeded the hard series cap", name)
		}
	}
}

func recordDistinctMetricLabels(m *Metrics, index int) {
	suffix := strconv.Itoa(index)
	m.RecordRequest("endpoint-"+suffix, "provider-"+suffix, "model-"+suffix, "error", time.Second)
	m.RecordUsage("provider-"+suffix, "model-"+suffix, 2, 3, 0.25)
	m.RecordGuardrailEvent("guardrail-"+suffix, "action-"+suffix)
	m.RecordRateLimitRejection("policy-" + suffix)
}

func TestMetricsSeriesCap(t *testing.T) {
	m := NewMetrics()
	const observations = maxMetricSeriesPerFamily + 7
	for index := 0; index < observations; index++ {
		recordDistinctMetricLabels(m, index)
	}
	assertMetricsSeriesCapTotals(t, m, observations)
	const retained = maxMetricSeriesPerFamily - 1
	const overflow = observations - retained
	text := scrapeMetrics(t, m)
	for _, want := range []string{
		`omniswitch_requests_total{endpoint="other",provider="other",model="other",status="other"} ` + strconv.Itoa(overflow),
		`omniswitch_request_duration_seconds_count{endpoint="other",provider="other"} ` + strconv.Itoa(overflow),
		`omniswitch_request_duration_seconds_bucket{endpoint="other",provider="other",le="+Inf"} ` + strconv.Itoa(overflow),
		`omniswitch_tokens_total{provider="other",model="other",direction="other"} ` + strconv.Itoa(observations*5-(retained/2*5+retained%2*2)),
		`omniswitch_cost_usd_total{provider="other",model="other"} ` + formatFloat(float64(overflow)*0.25),
		`omniswitch_guardrail_events_total{type="other",action="other"} ` + strconv.Itoa(overflow),
		`omniswitch_rate_limit_rejections_total{policy="other"} ` + strconv.Itoa(overflow),
		`omniswitch_metrics_series_overflow_total{family="omniswitch_requests_total"} ` + strconv.Itoa(overflow),
		`omniswitch_metrics_series_overflow_total{family="omniswitch_request_duration_seconds"} ` + strconv.Itoa(overflow),
		`omniswitch_metrics_series_overflow_total{family="omniswitch_tokens_total"} ` + strconv.Itoa(observations*2-retained),
		`omniswitch_metrics_series_overflow_total{family="omniswitch_cost_usd_total"} ` + strconv.Itoa(overflow),
		`omniswitch_metrics_series_overflow_total{family="omniswitch_guardrail_events_total"} ` + strconv.Itoa(overflow),
		`omniswitch_metrics_series_overflow_total{family="omniswitch_rate_limit_rejections_total"} ` + strconv.Itoa(overflow),
	} {
		if !strings.Contains(text, want+"\n") {
			t.Errorf("missing overflow sample %s", want)
		}
	}
	// Existing labels stay individually accounted for after the cap, while
	// repeated updates to an unretained label set continue to overflow.
	recordDistinctMetricLabels(m, 0)
	recordDistinctMetricLabels(m, observations-1)
	assertMetricsSeriesCapTotals(t, m, observations+2)
	family := m.families[requestsMetric]
	if got := family.series[formatLabels(family.labels, []string{"endpoint-0", "provider-0", "model-0", "error"})].value; got != 2 {
		t.Errorf("existing series value = %v, want 2", got)
	}
	if got := family.series[family.overflowKey].value; got != overflow+1 {
		t.Errorf("overflow value = %v, want %d", got, overflow+1)
	}
}

func TestMetricsOverflowLabelReservation(t *testing.T) {
	for _, otherFirst := range []bool{false, true} {
		t.Run(strconv.FormatBool(otherFirst), func(t *testing.T) {
			m := NewMetrics()
			if otherFirst {
				m.RecordGuardrailEvent(otherMetricLabel, otherMetricLabel)
			}
			for index := 0; index < maxMetricSeriesPerFamily-1; index++ {
				m.RecordGuardrailEvent(strconv.Itoa(index), "deny")
			}
			if !otherFirst {
				m.RecordGuardrailEvent(otherMetricLabel, otherMetricLabel)
			}
			m.RecordGuardrailEvent("overflow", "deny")
			m.RecordGuardrailEvent("overflow", "deny")
			family := m.families[guardrailsMetric]
			if len(family.series) != maxMetricSeriesPerFamily || family.series[family.overflowKey].value != 3 {
				t.Fatal("reserved overflow slot exceeded the cap or lost updates")
			}
			if family.series[formatLabels(family.labels, []string{strconv.Itoa(maxMetricSeriesPerFamily - 2), "deny"})] == nil {
				t.Fatal("existing other labels consumed an extra reserved slot")
			}
			if !strings.Contains(scrapeMetrics(t, m), `omniswitch_metrics_series_overflow_total{family="omniswitch_guardrail_events_total"} 2`+"\n") {
				t.Fatal("overflow events must count each folded update, not literal other labels")
			}
		})
	}
}

func TestMetricsCircuitSeriesCap(t *testing.T) {
	m := NewMetrics()
	states := map[string]int{otherMetricLabel: 0}
	const providers = maxMetricSeriesPerFamily + 7
	for index := 0; index < providers; index++ {
		name := "a-provider-" + strconv.Itoa(10000+index)
		states[name] = 0
		if index >= maxMetricSeriesPerFamily-1 {
			states[name] = 2
		}
	}
	m.RegisterCircuitStateSource(func() map[string]int { return states })
	for scrape := 1; scrape <= 2; scrape++ {
		text := scrapeMetrics(t, m)
		if got := strings.Count(text, circuitMetric+"{"); got != maxMetricSeriesPerFamily {
			t.Fatalf("circuit scrape has %d label sets, want %d", got, maxMetricSeriesPerFamily)
		}
		if !strings.Contains(text, `omniswitch_provider_circuit_state{provider="other"} 2`+"\n") {
			t.Fatal("circuit overflow must retain the highest state, even with a literal other provider")
		}
		if !strings.Contains(text, `omniswitch_provider_circuit_state{provider="a-provider-10000"} 0`+"\n") || strings.Contains(text, `provider="a-provider-12006"`) {
			t.Fatal("circuit cap must choose retained labels deterministically")
		}
		want := `omniswitch_metrics_series_overflow_total{family="omniswitch_provider_circuit_state"} ` + strconv.Itoa(scrape*(providers-(maxMetricSeriesPerFamily-1)))
		if !strings.Contains(text, want+"\n") {
			t.Errorf("missing scrape-time overflow count %s", want)
		}
	}
	states = map[string]int{"only": 1}
	text := scrapeMetrics(t, m)
	if strings.Contains(text, `omniswitch_provider_circuit_state{provider="other"}`) || strings.Count(text, circuitMetric+"{") != 1 {
		t.Fatal("capped circuit snapshot retained stale providers or overflow state")
	}
}

func TestMetricsConcurrentSeriesCap(t *testing.T) {
	m := NewMetrics()
	const workers, iterations = 8, 400
	var pending sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		pending.Add(1)
		go func() {
			defer pending.Done()
			for iteration := 0; iteration < iterations; iteration++ {
				recordDistinctMetricLabels(m, worker*iterations+iteration)
				if iteration%100 == 0 {
					if err := m.WritePrometheus(io.Discard); err != nil {
						t.Error(err)
					}
				}
			}
		}()
	}
	pending.Wait()
	assertMetricsSeriesCapTotals(t, m, workers*iterations)
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
