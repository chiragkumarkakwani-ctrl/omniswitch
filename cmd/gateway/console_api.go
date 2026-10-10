package main

import (
	"strings"

	"github.com/omniswitch-dev/omniswitch/internal/dashboard"
	"github.com/omniswitch-dev/omniswitch/internal/provider"
)

func configureConsoleAPI(h routeHandlers) {
	h.dash.SetSessionInfo(dashboard.SessionInfo{
		AuthEnabled: h.settings.authEnabled, Version: version, Commit: commit,
		Listen: h.settings.listenAddr, MCPEnabled: h.settings.mcpEnabled, LogPayloads: h.settings.logPayloads,
	})
	// P2a supplies this getter with its atomic live-reload implementation.
	if h.gateway != nil {
		if source, ok := any(h.gateway).(interface{ LogPayloads() bool }); ok {
			h.dash.SetLogPayloadsSource(source.LogPayloads)
		}
	}
	types := map[string]string{}
	for _, account := range h.settings.providerAccounts {
		kind := strings.ToLower(strings.TrimSpace(account.Type))
		types[strings.ToLower(account.Name)] = kind
		types[strings.ToLower(provider.NormalizeAlias(account.Name))] = kind
	}
	h.dash.SetProviderInspector(h.registry, h.router, types)
	h.dash.SetMCPInspector(h.mcp, h.settings.mcpEnabled, consoleMCPTargets(h.settings))
}

func consoleMCPTargets(settings runtimeSettings) []dashboard.MCPConfiguredTarget {
	out := []dashboard.MCPConfiguredTarget{}
	if len(settings.mcpTargets) == 0 {
		if settings.mcpEnabled {
			out = append(out, dashboard.MCPConfiguredTarget{Name: "default", Transport: "http", Enabled: true, PolicyPath: settings.mcpPolicyPath})
		}
		return out
	}
	for _, configured := range settings.mcpTargets {
		enabled := configured.Enabled == nil || *configured.Enabled
		transport := firstNonEmpty(configured.Transport, "http")
		out = append(out, dashboard.MCPConfiguredTarget{
			Name: configured.Name, Transport: transport, Enabled: enabled,
			PolicyPath: firstNonEmpty(configured.Policy, settings.mcpPolicyPath),
		})
	}
	return out
}

// Summaries are newly allocated per request. Remove paths at the HTTP boundary
// so runtimeConfigSummary can remain role-independent for existing callers.
func redactConfigSummary(summary map[string]any, admin bool) {
	if admin {
		return
	}
	delete(summary, "config_path")
	mcp, ok := summary["mcp"].(map[string]any)
	if !ok {
		return
	}
	delete(mcp, "policy_path")
	if targets, ok := mcp["targets"].([]map[string]any); ok {
		for _, target := range targets {
			delete(target, "policy_path")
		}
	}
}
