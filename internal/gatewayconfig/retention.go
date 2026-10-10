package gatewayconfig

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Retention is startup-only configuration. Pointer fields distinguish an
// omitted policy (use the default) from an explicit zero (retain forever).
type Retention struct {
	RequestLogs      *Duration `json:"request_logs,omitempty" yaml:"request_logs,omitempty"`
	Payloads         *Duration `json:"payloads,omitempty" yaml:"payloads,omitempty"`
	GuardrailEvents  *Duration `json:"guardrail_events,omitempty" yaml:"guardrail_events,omitempty"`
	ShadowRequests   *Duration `json:"shadow_requests,omitempty" yaml:"shadow_requests,omitempty"`
	LimitedAndDenied *Duration `json:"limited_and_denied,omitempty" yaml:"limited_and_denied,omitempty"`
	SweepInterval    *Duration `json:"sweep_interval,omitempty" yaml:"sweep_interval,omitempty"`
	BatchSize        *int      `json:"batch_size,omitempty" yaml:"batch_size,omitempty"`
}

// ParseRetentionDuration accepts whole days (30d), standard Go durations (1h),
// and 0. Invalid or overflowing values fail rather than silently enabling a
// potentially destructive default policy.
func ParseRetentionDuration(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if strings.HasSuffix(value, "d") {
		days, err := strconv.ParseInt(strings.TrimSuffix(value, "d"), 10, 64)
		day := int64(24 * time.Hour)
		if err != nil || days > (1<<63-1)/day || days < (-1<<63)/day {
			return 0, fmt.Errorf("invalid whole-day duration %q", value)
		}
		return time.Duration(days * day), nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse duration %q: %w", value, err)
	}
	return parsed, nil
}

func validateRetention(cfg Retention) error {
	for name, value := range map[string]*Duration{
		"request_logs": cfg.RequestLogs, "payloads": cfg.Payloads,
		"guardrail_events": cfg.GuardrailEvents, "shadow_requests": cfg.ShadowRequests,
		"limited_and_denied": cfg.LimitedAndDenied,
	} {
		if value != nil && value.Duration < 0 {
			return fmt.Errorf("retention.%s must be nonnegative", name)
		}
	}
	if cfg.SweepInterval != nil && cfg.SweepInterval.Duration <= 0 {
		return fmt.Errorf("retention.sweep_interval must be positive")
	}
	if cfg.BatchSize != nil && *cfg.BatchSize <= 0 {
		return fmt.Errorf("retention.batch_size must be positive")
	}
	return nil
}
