// Package limits implements approximate pre-checks with exact reconciliation.
// Counters use fixed windows shared by the memory and Redis backends.
package limits

import (
	"fmt"
	"math"
	"path"
	"strings"
	"time"
)

type Scope string
type Unit string
type Action string

const (
	ScopeKey          Scope = "key"
	ScopeWorkspace    Scope = "workspace"
	ScopeOrganization Scope = "organization"
	ScopeModel        Scope = "model"
	ScopeGlobal       Scope = "global"

	Requests     Unit = "requests"
	InputTokens  Unit = "input_tokens"
	OutputTokens Unit = "output_tokens"
	TotalTokens  Unit = "total_tokens"
	USD          Unit = "usd"

	Reject Action = "reject"
	Log    Action = "log"

	usdScale   = 1_000_000
	maxCounter = 1<<53 - 1 // Redis Lua represents numbers as doubles.
)

// Match filters are ANDed; * and ? match across slashes in model IDs.
type Match struct {
	KeyID       string `json:"key_id,omitempty" yaml:"key_id,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty" yaml:"workspace_id,omitempty"`
	Model       string `json:"model,omitempty" yaml:"model,omitempty"`
}

type Policy struct {
	Name   string  `json:"name" yaml:"name"`
	Scope  Scope   `json:"scope" yaml:"scope"`
	Unit   Unit    `json:"unit" yaml:"unit"`
	Limit  float64 `json:"limit" yaml:"limit"`
	Window string  `json:"window" yaml:"window"`
	Match  Match   `json:"match" yaml:"match"`
	Action Action  `json:"action" yaml:"action"`
}

// Identity contains only trusted authentication data, including the key fields
// loaded on this request. A key PATCH therefore needs no enforcer reload.
type Identity struct {
	KeyID             string
	WorkspaceID       string
	OrganizationID    string
	RateLimit         int
	RequestsPerMinute int
	TokensPerMinute   int
	USDPerDay         float64
}

// Usage is shared by chat, embeddings, rerank, and media callers. USD may be
// set from token pricing, an image size/quality price, or audio duration.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
	USD          float64
}

func (u Usage) amount(unit Unit) int64 {
	switch unit {
	case Requests:
		return 1
	case InputTokens:
		return min(maxCounter, max(0, u.InputTokens))
	case OutputTokens:
		return min(maxCounter, max(0, u.OutputTokens))
	case TotalTokens:
		if u.TotalTokens > 0 {
			return min(maxCounter, u.TotalTokens)
		}
		return min(maxCounter, min(maxCounter, max(0, u.InputTokens))+min(maxCounter, max(0, u.OutputTokens)))
	case USD:
		return scaledUSD(u.USD)
	default:
		return 0
	}
}

func scaledUSD(value float64) int64 {
	if value <= 0 || math.IsNaN(value) {
		return 0
	}
	return int64(math.Min(maxCounter, math.Ceil(value*usdScale)))
}

// ParseWindow accepts Go durations and a d suffix (one day is 24 hours).
func ParseWindow(value string) (time.Duration, error) {
	text := strings.TrimSpace(value)
	if strings.HasSuffix(text, "d") {
		text = strings.TrimSuffix(text, "d") + "h"
		duration, err := time.ParseDuration(text)
		if err != nil || duration < time.Second/24 || duration > 30*24*time.Hour/24 {
			return 0, fmt.Errorf("window %q must be between 1s and 30d", value)
		}
		return duration * 24, nil
	}
	duration, err := time.ParseDuration(text)
	if err != nil || duration < time.Second || duration > 30*24*time.Hour {
		return 0, fmt.Errorf("window %q must be between 1s and 30d", value)
	}
	return duration, nil
}

func ValidatePolicies(policies []Policy) error {
	names := make(map[string]bool, len(policies))
	for index, policy := range policies {
		prefix := fmt.Sprintf("rate_limit.policies[%d]", index)
		if strings.TrimSpace(policy.Name) == "" {
			return fmt.Errorf("%s.name is required", prefix)
		}
		switch policy.Name {
		case "default-requests", "key-requests-per-minute", "key-tokens-per-minute", "key-usd-per-day":
			return fmt.Errorf("%s.name %q is reserved for an implicit policy", prefix, policy.Name)
		}
		if names[policy.Name] {
			return fmt.Errorf("%s.name %q is duplicated", prefix, policy.Name)
		}
		names[policy.Name] = true
		switch policy.Scope {
		case ScopeKey, ScopeWorkspace, ScopeOrganization, ScopeModel, ScopeGlobal:
		default:
			return fmt.Errorf("%s.scope must be key, workspace, organization, model, or global", prefix)
		}
		switch policy.Unit {
		case Requests, InputTokens, OutputTokens, TotalTokens, USD:
		default:
			return fmt.Errorf("%s.unit must be requests, input_tokens, output_tokens, total_tokens, or usd", prefix)
		}
		maxLimit := float64(maxCounter)
		if policy.Unit == USD {
			maxLimit /= usdScale
		}
		if math.IsNaN(policy.Limit) || math.IsInf(policy.Limit, 0) || policy.Limit <= 0 || policy.Limit > maxLimit {
			return fmt.Errorf("%s.limit must be positive and at most %g", prefix, maxLimit)
		}
		if policy.Unit != USD && math.Trunc(policy.Limit) != policy.Limit {
			return fmt.Errorf("%s.limit must be a whole number for %s", prefix, policy.Unit)
		}
		if _, err := ParseWindow(policy.Window); err != nil {
			return fmt.Errorf("%s.%w", prefix, err)
		}
		if policy.Action != "" && policy.Action != Reject && policy.Action != Log {
			return fmt.Errorf("%s.action must be reject or log", prefix)
		}
		for _, filter := range []struct{ name, pattern string }{
			{"key_id", policy.Match.KeyID}, {"workspace_id", policy.Match.WorkspaceID}, {"model", policy.Match.Model},
		} {
			if _, err := path.Match(globText(filter.pattern), ""); err != nil {
				return fmt.Errorf("%s.match.%s is not a valid glob: %w", prefix, filter.name, err)
			}
		}
	}
	return nil
}

func globText(text string) string { return strings.ReplaceAll(text, "/", "\x00") }

func matches(pattern, value string) bool {
	if pattern == "" {
		return true
	}
	matched, _ := path.Match(globText(pattern), globText(value))
	return matched
}

func (p Policy) matches(identity Identity, model string, snapshot bool) bool {
	return matches(p.Match.KeyID, identity.KeyID) && matches(p.Match.WorkspaceID, identity.WorkspaceID) &&
		((snapshot && model == "") || matches(p.Match.Model, model))
}

func (p Policy) scaledLimit() int64 {
	if p.Unit == USD {
		return scaledUSD(p.Limit)
	}
	return int64(p.Limit)
}

func unscale(unit Unit, amount int64) float64 {
	if unit == USD {
		return float64(amount) / usdScale
	}
	return float64(amount)
}
