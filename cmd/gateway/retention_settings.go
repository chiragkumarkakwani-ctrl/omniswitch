package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/gatewayconfig"
)

func applyRetentionConfig(settings *runtimeSettings, cfg gatewayconfig.Retention) {
	for _, field := range []struct {
		target *time.Duration
		value  *gatewayconfig.Duration
	}{
		{&settings.retention.RequestLogs, cfg.RequestLogs},
		{&settings.retention.Payloads, cfg.Payloads},
		{&settings.retention.GuardrailEvents, cfg.GuardrailEvents},
		{&settings.retention.ShadowRequests, cfg.ShadowRequests},
		{&settings.retention.LimitedAndDenied, cfg.LimitedAndDenied},
		{&settings.retention.SweepInterval, cfg.SweepInterval},
	} {
		if field.value != nil {
			*field.target = field.value.Duration
		}
	}
	if cfg.BatchSize != nil {
		settings.retention.BatchSize = *cfg.BatchSize
	}
}

func loadRetentionEnv(settings *runtimeSettings) error {
	for _, field := range []struct {
		name   string
		target *time.Duration
	}{
		{"REQUEST_LOGS", &settings.retention.RequestLogs},
		{"PAYLOADS", &settings.retention.Payloads},
		{"GUARDRAIL_EVENTS", &settings.retention.GuardrailEvents},
		{"SHADOW_REQUESTS", &settings.retention.ShadowRequests},
		{"LIMITED_AND_DENIED", &settings.retention.LimitedAndDenied},
		{"SWEEP_INTERVAL", &settings.retention.SweepInterval},
	} {
		name := "OMNISWITCH_RETENTION_" + field.name
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			parsed, err := gatewayconfig.ParseRetentionDuration(value)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			*field.target = parsed
		}
	}
	if value := strings.TrimSpace(os.Getenv("OMNISWITCH_RETENTION_BATCH_SIZE")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("OMNISWITCH_RETENTION_BATCH_SIZE must be a positive integer: %w", err)
		}
		settings.retention.BatchSize = parsed
	}
	return settings.retention.Validate()
}
