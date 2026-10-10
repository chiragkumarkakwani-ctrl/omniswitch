package limits

import (
	"math"
	"strings"
	"testing"
	"time"
)

func validPolicy() Policy {
	return Policy{Name: "tokens", Scope: ScopeKey, Unit: TotalTokens, Limit: 1000, Window: "1m", Action: Reject}
}

func TestPolicyValidation(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Policy)
		want   string
	}{
		{"name", func(p *Policy) { p.Name = " " }, ".name is required"},
		{"reserved", func(p *Policy) { p.Name = "default-requests" }, "reserved"},
		{"scope", func(p *Policy) { p.Scope = "tenant" }, ".scope"},
		{"unit", func(p *Policy) { p.Unit = "tokens" }, ".unit"},
		{"zero", func(p *Policy) { p.Limit = 0 }, ".limit"},
		{"negative", func(p *Policy) { p.Limit = -1 }, ".limit"},
		{"nan", func(p *Policy) { p.Limit = math.NaN() }, ".limit"},
		{"infinite", func(p *Policy) { p.Limit = math.Inf(1) }, ".limit"},
		{"too large", func(p *Policy) { p.Limit = float64(maxCounter) * 2 }, ".limit"},
		{"fractional tokens", func(p *Policy) { p.Limit = 2.5 }, "whole number"},
		{"too large usd", func(p *Policy) { p.Unit = USD; p.Limit = float64(maxCounter) }, ".limit"},
		{"action", func(p *Policy) { p.Action = "allow" }, ".action"},
		{"window", func(p *Policy) { p.Window = "31d" }, "window"},
		{"key glob", func(p *Policy) { p.Match.KeyID = "[" }, "match.key_id"},
		{"workspace glob", func(p *Policy) { p.Match.WorkspaceID = "[" }, "match.workspace_id"},
		{"model glob", func(p *Policy) { p.Match.Model = "[" }, "match.model"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := validPolicy()
			test.change(&policy)
			err := ValidatePolicies([]Policy{policy})
			if err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "policies[0]") {
				t.Fatalf("ValidatePolicies() = %v, want %s", err, test.want)
			}
		})
	}
	policy := validPolicy()
	if err := ValidatePolicies([]Policy{policy, policy}); err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("duplicate policy: %v", err)
	}
	for _, scope := range []Scope{ScopeKey, ScopeWorkspace, ScopeOrganization, ScopeModel, ScopeGlobal} {
		for _, unit := range []Unit{Requests, InputTokens, OutputTokens, TotalTokens, USD} {
			policy.Scope, policy.Unit, policy.Action = scope, unit, ""
			policy.Limit = 1
			if unit == USD {
				policy.Limit = .000001
			}
			if err := ValidatePolicies([]Policy{policy}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestWindowsAndGlobs(t *testing.T) {
	for value, want := range map[string]time.Duration{"1s": time.Second, "1m": time.Minute, "24h": 24 * time.Hour, "30d": 30 * 24 * time.Hour, " 1d ": 24 * time.Hour, "0.5d": 12 * time.Hour} {
		got, err := ParseWindow(value)
		if err != nil || got != want {
			t.Fatalf("ParseWindow(%q) = %v/%v", value, got, err)
		}
	}
	for _, value := range []string{"", "0", "0s", "999ms", "31d", "-1d", "xyz", "9999999999999d", "0.000001d"} {
		if _, err := ParseWindow(value); err == nil {
			t.Fatalf("ParseWindow(%q) succeeded", value)
		}
	}
	policy := validPolicy()
	policy.Match = Match{KeyID: "key_?", WorkspaceID: "dev*", Model: "*/gpt-*"}
	identity := Identity{KeyID: "key_a", WorkspaceID: "development"}
	if !policy.matches(identity, "openai/gpt-mini", false) || policy.matches(identity, "claude", false) {
		t.Fatal("model glob mismatch")
	}
	if !policy.matches(identity, "", true) || policy.matches(Identity{KeyID: "other"}, "openai/gpt-mini", false) {
		t.Fatal("identity or snapshot matching mismatch")
	}
	if !matches("*", "accounts/fireworks/models/gpt-oss-120b") {
		t.Fatal("star must include slash")
	}
}

func TestAmountsAndUSDRounding(t *testing.T) {
	usage := Usage{InputTokens: 4, OutputTokens: 6, USD: .0000011}
	for unit, want := range map[Unit]int64{Requests: 1, InputTokens: 4, OutputTokens: 6, TotalTokens: 10, USD: 2, "unknown": 0} {
		if got := usage.amount(unit); got != want {
			t.Fatalf("amount(%s) = %d, want %d", unit, got, want)
		}
	}
	usage.TotalTokens = 11
	if usage.amount(TotalTokens) != 11 {
		t.Fatal("reported total was lost")
	}
	if (Usage{InputTokens: math.MaxInt64, OutputTokens: math.MaxInt64}).amount(TotalTokens) != maxCounter {
		t.Fatal("total must saturate safely")
	}
	for _, value := range []float64{-1, 0, math.NaN()} {
		if scaledUSD(value) != 0 {
			t.Fatal("invalid cost must not consume")
		}
	}
	if scaledUSD(math.Inf(1)) != maxCounter || unscale(USD, 25) != .000025 || unscale(Requests, 25) != 25 {
		t.Fatal("USD scale mismatch")
	}
	policy := validPolicy()
	policy.Unit, policy.Limit = USD, .25
	if policy.scaledLimit() != 250000 {
		t.Fatal("USD policy scaling mismatch")
	}
	if scopeID("unknown", Identity{}, "") != "" {
		t.Fatal("unknown scope must not create counters")
	}
}
