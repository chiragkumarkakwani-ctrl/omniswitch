package dashboard

import (
	"net/http"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/store"
)

func (h *Handler) getMetrics(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	_, since, _, err := timeWindow(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	metrics, err := h.store.GetMetricsForAPIKey(r.Context(), since, scopedAPIKeyID(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read metrics")
		return
	}
	writeJSON(w, http.StatusOK, metrics)
}

func (h *Handler) getProviderMetrics(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	_, since, _, err := timeWindow(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	metrics, err := h.store.GetProviderMetricsForAPIKey(r.Context(), since, scopedAPIKeyID(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read provider metrics")
		return
	}
	if metrics == nil {
		metrics = []store.ProviderMetrics{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": metrics})
}

func (h *Handler) getTimeseries(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	window, since, until, err := timeWindow(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	bucket := r.URL.Query().Get("bucket")
	if bucket == "" {
		bucket = map[string]string{"1h": "5m", "6h": "15m", "24h": "1h", "7d": "6h", "30d": "1d"}[window]
	}
	duration := 24 * time.Hour
	if bucket != "1d" {
		duration, err = time.ParseDuration(bucket)
	}
	if err != nil || duration < time.Second || duration > 30*24*time.Hour || duration%time.Second != 0 {
		writeError(w, http.StatusBadRequest, "bucket must be a positive whole-second duration (or 1d)")
		return
	}
	buckets, err := h.store.MetricsTimeseries(r.Context(), since, until, duration, scopedAPIKeyID(r))
	if err != nil {
		// Bounds are checked here rather than exposing a driver error as a
		// validation response. A too-small bucket is not silently resampled.
		count := until.Add(-time.Nanosecond).Unix()/int64(duration/time.Second) - since.Unix()/int64(duration/time.Second) + 1
		if count > 200 {
			writeError(w, http.StatusBadRequest, "bucket must yield no more than 200 buckets")
		} else {
			writeError(w, http.StatusInternalServerError, "could not read traffic buckets")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"window": window, "bucket": bucket, "buckets": buckets})
}

func (h *Handler) getLatency(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	_, since, until, err := timeWindow(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	stats, err := h.store.LatencyPercentiles(r.Context(), since, until, scopedAPIKeyID(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read latency percentiles")
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (h *Handler) getKeyUsage(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if window := r.URL.Query().Get("window"); window != "" && window != "month" {
		writeError(w, http.StatusBadRequest, "window must be month")
		return
	}
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	keys, err := h.store.BudgetUsageByKey(r.Context(), start, now, scopedAPIKeyID(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read key budget usage")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"period": map[string]time.Time{"start": start, "end": end}, "keys": keys})
}
