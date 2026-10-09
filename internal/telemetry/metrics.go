package telemetry

import (
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	requestsMetric        = "omniswitch_requests_total"
	durationMetric        = "omniswitch_request_duration_seconds"
	tokensMetric          = "omniswitch_tokens_total"
	costMetric            = "omniswitch_cost_usd_total"
	guardrailsMetric      = "omniswitch_guardrail_events_total"
	rateLimitsMetric      = "omniswitch_rate_limit_rejections_total"
	circuitMetric         = "omniswitch_provider_circuit_state"
	buildMetric           = "omniswitch_build_info"
	seriesOverflowMetric  = "omniswitch_metrics_series_overflow_total"
	prometheusContentType = "text/plain; version=0.0.4; charset=utf-8"

	// The cap includes one reserved overflow series, bounding retained labels
	// even when callers supply a different model on every request.
	maxMetricSeriesPerFamily = 2000
	otherMetricLabel         = "other"
)

var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}

// DefaultMetrics is the process-wide registry used by gateway and dashboard
// handlers unless an embedder supplies its own registry with SetMetrics.
var DefaultMetrics = NewMetrics()

// Metrics is a small, concurrency-safe registry for the gateway's fixed metric
// families. Counters and histograms accumulate since process start, not from
// request logs. Construct it with NewMetrics.
type Metrics struct {
	mu            sync.RWMutex
	families      map[string]*metricFamily
	circuitStates func() map[string]int
}

type metricFamily struct {
	name        string
	help        string
	kind        string
	labels      []string
	overflowKey string
	series      map[string]*metricSeries
}

type metricSeries struct {
	value   float64
	buckets []uint64
	count   uint64
	sum     float64
}

// NewMetrics creates an empty registry with build information for a development
// build. The gateway binary replaces those labels with its build-time values.
func NewMetrics() *Metrics {
	m := &Metrics{families: make(map[string]*metricFamily)}
	definitions := []metricFamily{
		{name: requestsMetric, help: "Completed gateway API requests since process start.", kind: "counter", labels: []string{"endpoint", "provider", "model", "status"}},
		{name: durationMetric, help: "End-to-end gateway API request duration in seconds.", kind: "histogram", labels: []string{"endpoint", "provider"}},
		{name: tokensMetric, help: "Reported upstream token usage since process start, excluding cache hits.", kind: "counter", labels: []string{"provider", "model", "direction"}},
		{name: costMetric, help: "Estimated upstream cost in USD since process start, excluding cache hits.", kind: "counter", labels: []string{"provider", "model"}},
		{name: guardrailsMetric, help: "Triggered gateway guardrail events since process start.", kind: "counter", labels: []string{"type", "action"}},
		{name: rateLimitsMetric, help: "Rate-limit rejections since process start.", kind: "counter", labels: []string{"policy"}},
		{name: circuitMetric, help: "Provider circuit state: 0 closed, 1 half-open, 2 open; overflow reports the highest state.", kind: "gauge", labels: []string{"provider"}},
		{name: buildMetric, help: "Gateway build information.", kind: "gauge", labels: []string{"version", "commit"}},
		{name: seriesOverflowMetric, help: "Metric observations folded into overflow series since process start.", kind: "counter", labels: []string{"family"}},
	}
	for _, definition := range definitions {
		definition.help += " At most " + strconv.Itoa(maxMetricSeriesPerFamily) + " label sets; overflow uses other for every family label value."
		other := make([]string, len(definition.labels))
		for index := range other {
			other[index] = otherMetricLabel
		}
		definition.overflowKey = formatLabels(definition.labels, other)
		definition.series = make(map[string]*metricSeries)
		m.families[definition.name] = &definition
	}
	m.SetBuildInfo("dev", "none")
	return m
}

// RecordRequest records one completed API request and its end-to-end duration.
func (m *Metrics) RecordRequest(endpoint, provider, model, status string, duration time.Duration) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addCounter(requestsMetric, []string{endpoint, provider, model, status}, 1)
	if duration < 0 {
		duration = 0
	}
	m.observe(durationMetric, []string{endpoint, provider}, duration.Seconds())
}

// RecordUsage records reported usage and estimated cost for an upstream call.
// Callers must not record cached completions. Invalid negative or non-finite
// values are ignored rather than making a counter decrease or become NaN.
func (m *Metrics) RecordUsage(provider, model string, input, output int, cost float64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addCounter(tokensMetric, []string{provider, model, "input"}, float64(input))
	m.addCounter(tokensMetric, []string{provider, model, "output"}, float64(output))
	m.addCounter(costMetric, []string{provider, model}, cost)
}

// RecordGuardrailEvent counts a triggered guardrail, including non-blocking
// actions, regardless of whether its audit event could be persisted.
func (m *Metrics) RecordGuardrailEvent(kind, action string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addCounter(guardrailsMetric, []string{kind, action}, 1)
}

// RecordRateLimitRejection is the integration hook for request, token, and
// spend limiters. Call it once per rejection with the rejecting policy name.
func (m *Metrics) RecordRateLimitRejection(policy string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addCounter(rateLimitsMetric, []string{policy}, 1)
}

// RegisterCircuitStateSource installs a scrape-time circuit snapshot hook. The
// callback must return an owned snapshot and be safe for concurrent calls. It
// runs outside the registry lock. States are 0 closed, 1 half-open, and 2 open;
// invalid states are omitted. A nil source unregisters the hook.
func (m *Metrics) RegisterCircuitStateSource(source func() map[string]int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.circuitStates = source
}

// SetBuildInfo replaces the single build-info gauge rather than accumulating
// stale label sets when an embedder changes build information.
func (m *Metrics) SetBuildInfo(version, commit string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	family := m.families[buildMetric]
	family.series = map[string]*metricSeries{
		formatLabels(family.labels, []string{version, commit}): {value: 1},
	}
}

func (m *Metrics) addCounter(name string, labels []string, value float64) {
	if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return
	}
	series := m.getSeries(name, labels)
	if total := series.value + value; !math.IsInf(total, 0) {
		series.value = total
	}
}

func (m *Metrics) observe(name string, labels []string, value float64) {
	series := m.getSeries(name, labels)
	series.count++
	series.sum += value
	for index, bound := range durationBuckets {
		if value <= bound {
			series.buckets[index]++
		}
	}
}

// getSeries runs under the registry's exclusive lock. The overflow counter's
// family labels are fixed registry names, never caller-supplied label values.
func (m *Metrics) getSeries(name string, labels []string) *metricSeries {
	series, overflow := m.families[name].getSeries(labels)
	if overflow && name != seriesOverflowMetric {
		m.addCounter(seriesOverflowMetric, []string{name}, 1)
	}
	return series
}

func (family *metricFamily) getSeries(labels []string) (*metricSeries, bool) {
	key := formatLabels(family.labels, labels)
	if series := family.series[key]; series != nil {
		return series, false
	}
	limit := maxMetricSeriesPerFamily
	if family.series[family.overflowKey] == nil {
		// Reserve the final slot: creating overflow must not exceed the cap.
		limit--
	}
	overflow := false
	if len(family.series) >= limit {
		overflow = key != family.overflowKey
		key = family.overflowKey
	}
	series := family.series[key]
	if series == nil {
		series = &metricSeries{}
		if family.kind == "histogram" {
			series.buckets = make([]uint64, len(durationBuckets))
		}
		family.series[key] = series
	}
	return series, overflow
}

// WritePrometheus writes deterministic Prometheus text exposition format 0.0.4.
// Socket writes run without holding the registry lock, so a slow scraper does
// not block request updates. Unobserved families have HELP/TYPE but no samples.
func (m *Metrics) WritePrometheus(w io.Writer) error {
	m.mu.RLock()
	source := m.circuitStates
	circuits := *m.families[circuitMetric]
	m.mu.RUnlock()
	circuits.series = make(map[string]*metricSeries)
	overflowEvents := 0
	if source != nil {
		states := source()
		// Select retained providers consistently between scrapes.
		for _, provider := range sortedKeys(states) {
			state := states[provider]
			if state >= 0 && state <= 2 {
				series, overflow := circuits.getSeries([]string{provider})
				series.value = math.Max(series.value, float64(state))
				if overflow {
					overflowEvents++
				}
			}
		}
	}
	if overflowEvents > 0 {
		m.mu.Lock()
		m.addCounter(seriesOverflowMetric, []string{circuitMetric}, float64(overflowEvents))
		m.mu.Unlock()
	}

	var text strings.Builder
	m.mu.RLock()
	for _, name := range sortedKeys(m.families) {
		family := m.families[name]
		text.WriteString("# HELP " + name + " " + escapeHelp(family.help) + "\n")
		text.WriteString("# TYPE " + name + " " + family.kind + "\n")
		series := family.series
		if name == circuitMetric {
			series = circuits.series
		}
		for _, labels := range sortedKeys(series) {
			sample := series[labels]
			if family.kind != "histogram" {
				writeSample(&text, name, labels, formatFloat(sample.value))
				continue
			}
			for index, bound := range durationBuckets {
				writeSample(&text, name+"_bucket", withBucketLabel(labels, formatFloat(bound)), strconv.FormatUint(sample.buckets[index], 10))
			}
			writeSample(&text, name+"_bucket", withBucketLabel(labels, "+Inf"), strconv.FormatUint(sample.count, 10))
			writeSample(&text, name+"_sum", labels, formatFloat(sample.sum))
			writeSample(&text, name+"_count", labels, strconv.FormatUint(sample.count, 10))
		}
	}
	m.mu.RUnlock()
	_, err := io.WriteString(w, text.String())
	return err
}

// ServeHTTP serves the registry without applying authentication. The caller
// chooses whether to mount it inside or outside the gateway middleware chain.
func (m *Metrics) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "only GET and HEAD are supported", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", prometheusContentType)
	if r.Method == http.MethodGet {
		// A disconnected scraper cannot receive an error response after writing.
		_ = m.WritePrometheus(w)
	}
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func writeSample(text *strings.Builder, name, labels, value string) {
	text.WriteString(name + labels + " " + value + "\n")
}

func formatFloat(value float64) string {
	return strconv.FormatFloat(value, 'g', -1, 64)
}

func formatLabels(names, values []string) string {
	var labels strings.Builder
	labels.WriteByte('{')
	for index, name := range names {
		if index > 0 {
			labels.WriteByte(',')
		}
		labels.WriteString(name + "=\"" + escapeLabel(values[index]) + "\"")
	}
	labels.WriteByte('}')
	return labels.String()
}

func withBucketLabel(labels, bound string) string {
	return strings.TrimSuffix(labels, "}") + ",le=\"" + bound + "\"}"
}

func escapeLabel(value string) string {
	return strings.NewReplacer("\\", "\\\\", "\n", "\\n", "\"", "\\\"").Replace(value)
}

func escapeHelp(value string) string {
	return strings.NewReplacer("\\", "\\\\", "\n", "\\n").Replace(value)
}
