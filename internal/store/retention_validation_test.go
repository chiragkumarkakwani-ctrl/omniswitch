package store

import (
	"testing"
	"time"
)

func TestRetentionPolicyValidationOrder(t *testing.T) {
	policy := DefaultRetentionPolicy()
	fields := []struct {
		name     string
		duration *time.Duration
	}{
		{"request_logs", &policy.RequestLogs},
		{"payloads", &policy.Payloads},
		{"guardrail_events", &policy.GuardrailEvents},
		{"shadow_requests", &policy.ShadowRequests},
		{"limited_and_denied", &policy.LimitedAndDenied},
	}
	for _, field := range fields {
		*field.duration = -time.Second
	}
	policy.SweepInterval = 0
	policy.BatchSize = 0
	for _, field := range fields {
		want := "retention." + field.name + " must be nonnegative"
		for range 100 {
			if err := policy.Validate(); err == nil || err.Error() != want {
				t.Fatalf("validation error = %v, want %q", err, want)
			}
		}
		*field.duration = 0
	}
	if err := policy.Validate(); err == nil || err.Error() != "retention.sweep_interval must be positive" {
		t.Fatalf("interval validation error = %v", err)
	}
	policy.SweepInterval = time.Hour
	if err := policy.Validate(); err == nil || err.Error() != "retention.batch_size must be positive" {
		t.Fatalf("batch validation error = %v", err)
	}
	policy.BatchSize = 1
	if err := policy.Validate(); err != nil {
		t.Fatalf("all-zero durations should be valid: %v", err)
	}
}
