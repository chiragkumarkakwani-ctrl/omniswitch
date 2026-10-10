package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/store"
)

type retentionRun struct {
	StartedAt       time.Time              `json:"started_at"`
	FinishedAt      time.Time              `json:"finished_at"`
	Deleted         store.RetentionDeleted `json:"deleted"`
	PayloadsCleared int64                  `json:"payloads_cleared"`
	Error           string                 `json:"error"`
}

type retentionStatus struct {
	Policy               map[string]float64 `json:"policy"`
	SweepIntervalSeconds float64            `json:"sweep_interval_seconds"`
	PayloadLogging       bool               `json:"payload_logging"`
	LastRun              *retentionRun      `json:"last_run"`
	NextRun              *time.Time         `json:"next_run"`
}

// retentionController serializes local manual/background sweeps. The store
// provides cross-replica exclusion. Published run records are immutable, so a
// concurrent GET never races with a sweep or waits on its database work.
type retentionController struct {
	store          *store.Store
	policy         store.RetentionPolicy
	payloadLogging func() bool
	gate           chan struct{}
	mu             sync.RWMutex
	lastRun        *retentionRun
	nextTick       time.Time
}

func newRetentionController(st *store.Store, policy store.RetentionPolicy, payloadLogging func() bool) *retentionController {
	return &retentionController{store: st, policy: policy, payloadLogging: payloadLogging, gate: make(chan struct{}, 1)}
}

func retentionPolicySummary(policy store.RetentionPolicy) map[string]float64 {
	return map[string]float64{
		"request_logs":       policy.RequestLogs.Seconds(),
		"payloads":           policy.Payloads.Seconds(),
		"guardrail_events":   policy.GuardrailEvents.Seconds(),
		"shadow_requests":    policy.ShadowRequests.Seconds(),
		"limited_and_denied": policy.LimitedAndDenied.Seconds(),
	}
}

func (c *retentionController) snapshot() retentionStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()
	status := retentionStatus{
		Policy:               retentionPolicySummary(c.policy),
		SweepIntervalSeconds: c.policy.SweepInterval.Seconds(),
		PayloadLogging:       c.payloadLogging != nil && c.payloadLogging(),
	}
	if c.policy.Enabled() && c.lastRun != nil {
		status.LastRun = c.lastRun
		next := c.nextTick
		if next.IsZero() {
			next = c.lastRun.FinishedAt.Add(c.policy.SweepInterval)
		}
		status.NextRun = &next
	}
	return status
}

func (c *retentionController) sweep(ctx context.Context) error {
	if !c.policy.Enabled() {
		return nil
	}
	select {
	case c.gate <- struct{}{}:
		defer func() { <-c.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	started := time.Now().UTC()
	result, err := c.store.SweepRetention(ctx, c.policy, started)
	if result.Skipped {
		return nil
	}
	run := &retentionRun{
		StartedAt: started, FinishedAt: time.Now().UTC(),
		Deleted: result.Deleted, PayloadsCleared: result.PayloadsCleared,
	}
	if err != nil {
		run.Error = err.Error()
	}
	c.mu.Lock()
	c.lastRun = run
	c.mu.Unlock()
	return err
}

// start waits a full interval before the first run. The caller cancels ctx and
// waits on the returned channel before closing the database during shutdown.
func (c *retentionController) start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if !c.policy.Enabled() {
			return
		}
		ticker := time.NewTicker(c.policy.SweepInterval)
		defer ticker.Stop()
		c.setNextTick(time.Now().UTC().Add(c.policy.SweepInterval))
		for {
			select {
			case <-ctx.Done():
				return
			case at := <-ticker.C:
				if ctx.Err() != nil {
					return
				}
				c.setNextTick(at.UTC().Add(c.policy.SweepInterval))
				if err := c.sweep(ctx); err != nil && ctx.Err() == nil {
					log.Printf("retention sweep failed: %v", err)
				}
			}
		}
	}()
	return done
}

func (c *retentionController) setNextTick(at time.Time) {
	c.mu.Lock()
	c.nextTick = at
	c.mu.Unlock()
}

func registerRetentionRoutes(mux *http.ServeMux, retention *retentionController) {
	mux.HandleFunc("/api/retention", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		writeJSON(w, http.StatusOK, retention.snapshot())
	})
	mux.HandleFunc("/api/retention/sweep", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		status := http.StatusOK
		if err := retention.sweep(r.Context()); err != nil {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, retention.snapshot())
	})
}

func logRetentionPolicy(policy store.RetentionPolicy) {
	log.Printf("Retention: request logs %s, payloads %s, guardrail events %s, shadow requests %s, limited/denied %s; sweep every %s, batch %d (set 0 to keep forever)",
		retentionDuration(policy.RequestLogs), retentionDuration(policy.Payloads), retentionDuration(policy.GuardrailEvents),
		retentionDuration(policy.ShadowRequests), retentionDuration(policy.LimitedAndDenied), policy.SweepInterval, policy.BatchSize)
}

func retentionDuration(duration time.Duration) string {
	if duration == 0 {
		return "0"
	}
	if duration%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", duration/(24*time.Hour))
	}
	return duration.String()
}
