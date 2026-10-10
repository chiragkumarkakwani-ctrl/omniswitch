package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/adapter/mcp"
	"github.com/omniswitch-dev/omniswitch/internal/model"
)

var ErrInspectionTargetNotFound = errors.New("MCP target not found")

type TokenExchangeSummary struct {
	Configured   bool   `json:"configured"`
	TokenURLHost string `json:"token_url_host"`
	Audience     string `json:"audience"`
}

type TargetInspection struct {
	Name               string               `json:"name"`
	Transport          string               `json:"transport"`
	Enabled            bool                 `json:"enabled"`
	Connected          bool                 `json:"connected"`
	ToolCount          int                  `json:"tool_count"`
	Visible            int                  `json:"visible"`
	Hidden             int                  `json:"hidden"`
	LastError          string               `json:"last_error"`
	OpenAPISpec        bool                 `json:"openapi_spec"`
	ForwardBearerToken bool                 `json:"forward_bearer_token"`
	TokenExchange      TokenExchangeSummary `json:"token_exchange"`
}

type PolicyInspection struct {
	Allowed bool   `json:"allowed"`
	Rule    string `json:"rule"`
	Reason  string `json:"reason"`
}

type CallInspection struct {
	PolicyInspection
	EvaluatedWithoutArguments bool `json:"evaluated_without_arguments"`
}

type ToolInspection struct {
	Target         string           `json:"target"`
	Name           string           `json:"name"`
	NamespacedName string           `json:"namespaced_name"`
	Description    string           `json:"description"`
	List           PolicyInspection `json:"list"`
	Call           CallInspection   `json:"call"`
}

// InspectTargets probes discovery and evaluates visibility for this principal.
// These inspection paths never use h.auditor and never invoke a tool.
func (h *Handler) InspectTargets(ctx context.Context, headers http.Header, agent model.Agent) []TargetInspection {
	out := make([]TargetInspection, 0, len(h.targetOrder))
	for _, key := range h.targetOrder {
		target := h.targets[key]
		entry := TargetInspection{Name: target.name, Transport: "http", Enabled: true, OpenAPISpec: len(target.openAPITools) > 0, ForwardBearerToken: target.forwardBearerToken}
		if target.stdio != nil {
			entry.Transport = "stdio"
		}
		if target.tokenExchange != nil {
			entry.TokenExchange.Configured = true
			entry.TokenExchange.Audience = target.tokenExchange.Audience
			if parsed, err := url.Parse(target.tokenExchange.TokenURL); err == nil {
				entry.TokenExchange.TokenURLHost = parsed.Hostname()
			}
		}
		tools, err := h.inspectUpstreamTools(ctx, target, headers)
		entry.Connected = err == nil
		if err != nil {
			entry.LastError = "tool discovery failed"
			if len(target.openAPITools) > 0 && len(tools) > 0 {
				entry.LastError = "tool discovery unavailable; showing known OpenAPI tools only"
			}
		}
		for _, tool := range tools {
			name, _ := tool["name"].(string)
			if name == "" {
				continue
			}
			entry.ToolCount++
			decision := inspectPolicy(ctx, target, agent, name, "list", headers.Get("Mcp-Session-Id"))
			if decision.Allowed {
				entry.Visible++
			} else {
				entry.Hidden++
			}
		}
		out = append(out, entry)
	}
	return out
}

// InspectTools explains the same per-tool policy engine used by discovery and
// calls. Hidden tools are only included for administrators. No arguments are
// fabricated, and the call decision is explicitly marked as a dry run.
func (h *Handler) InspectTools(ctx context.Context, headers http.Header, agent model.Agent, targetName string, includeHidden bool) ([]ToolInspection, error) {
	out := []ToolInspection{}
	if targetName != "" {
		if _, exists := h.targets[strings.ToLower(targetName)]; !exists {
			return nil, ErrInspectionTargetNotFound
		}
	}
	for _, key := range h.targetOrder {
		if targetName != "" && key != strings.ToLower(targetName) {
			continue
		}
		target := h.targets[key]
		tools, err := h.inspectUpstreamTools(ctx, target, headers)
		if err != nil && len(tools) == 0 {
			return nil, fmt.Errorf("MCP tool discovery unavailable")
		}
		for _, tool := range tools {
			name, _ := tool["name"].(string)
			if name == "" {
				continue
			}
			list := inspectPolicy(ctx, target, agent, name, "list", headers.Get("Mcp-Session-Id"))
			if !list.Allowed && !includeHidden {
				continue
			}
			description, _ := tool["description"].(string)
			out = append(out, ToolInspection{
				Target: target.name, Name: name, NamespacedName: target.name + "__" + name, Description: description,
				List: list, Call: CallInspection{PolicyInspection: inspectPolicy(ctx, target, agent, name, "call", headers.Get("Mcp-Session-Id")), EvaluatedWithoutArguments: true},
			})
		}
	}
	return out, nil
}

func inspectPolicy(ctx context.Context, target target, agent model.Agent, name, action, sessionID string) PolicyInspection {
	request := model.ToolRequest{Agent: agent, Tool: model.Tool{Name: name}, Action: model.Action{Name: action}, Session: model.Session{ID: sessionID}, Metadata: map[string]string{"mcp.target": target.name, "mcp.tool": name}}
	var legacyRequest *model.ToolRequest
	if action == "call" {
		if synthetic := findOpenAPITool(target.openAPITools, name); synthetic != nil {
			request.Metadata["mcp.openapi"] = synthetic.Method + " " + synthetic.Path
		} else {
			// Match the real MCP adapter's tool/action/resource inference, but
			// supply no fabricated arguments and never forward a tools/call.
			rpc := mcp.Request{JSONRPC: "2.0", Method: "tools/call", Params: mcp.ToolCallParams{Name: name}}
			var err error
			request, err = rpc.ToToolRequestWithIdentity(agent, sessionID)
			if err != nil {
				return PolicyInspection{Allowed: false, Reason: "policy could not be evaluated without arguments"}
			}
			request.Metadata["mcp.target"] = target.name
			request.Metadata["mcp.tool"] = name
			legacy := request
			legacyRequest = &legacy
			request.Tool.Name, request.Action.Name = name, "call"
		}
	}
	decision, err := target.engine.Evaluate(ctx, request)
	// P3b's real calls enforce canonical bare-name/call policy first, then
	// retain the legacy namespace/inferred-action policy when different.
	if err == nil && decision.Allowed && legacyRequest != nil && (legacyRequest.Tool != request.Tool || legacyRequest.Action != request.Action) {
		decision, err = target.engine.Evaluate(ctx, *legacyRequest)
	}
	if err != nil {
		return PolicyInspection{Allowed: false, Rule: decision.Rule, Reason: "policy could not be evaluated without arguments"}
	}
	return PolicyInspection{Allowed: decision.Allowed, Rule: decision.Rule, Reason: decision.Reason}
}

// inspectUpstreamTools is the integration switch point for P3b's per-target
// 30-second tools/list cache. Until both streams land, it calls discovery
// directly with a deadline and a response size cap. Keep policy evaluation
// outside the cache, and keep identity-sensitive forwarding/exchange separate.
func (h *Handler) inspectUpstreamTools(ctx context.Context, target target, headers http.Header) (tools []map[string]any, err error) {
	// Known synthetic tools stay inspectable when a plain OpenAPI server does
	// not implement native MCP discovery. Targets still report that probe as
	// disconnected, rather than presenting a loaded spec as a live check.
	defer func() {
		seen := make(map[string]bool, len(tools))
		for _, tool := range tools {
			name, _ := tool["name"].(string)
			seen[name] = true
		}
		for _, synthetic := range target.openAPITools {
			if !seen[synthetic.Name] {
				tools = append(tools, map[string]any{"name": synthetic.Name, "description": synthetic.Description})
				seen[synthetic.Name] = true
			}
		}
		if len(tools) > 1000 {
			tools = nil
			err = fmt.Errorf("discovery exceeds 1000 tools")
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	body := []byte(`{"jsonrpc":"2.0","id":"omniswitch-inspect","method":"tools/list","params":{}}`)
	var payload []byte
	if target.stdio != nil {
		var err error
		payload, err = target.stdio.Call(ctx, body)
		if err != nil {
			return nil, err
		}
	} else {
		incoming := headers.Clone()
		if incoming == nil {
			incoming = make(http.Header)
		}
		incoming.Set("Accept", "application/json")
		response, err := h.targetRequest(ctx, target, body, incoming)
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return nil, fmt.Errorf("upstream discovery returned a non-success status")
		}
		payload, err = io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
		if err != nil {
			return nil, err
		}
	}
	if len(payload) > 1<<20 {
		return nil, fmt.Errorf("discovery response exceeds 1 MiB")
	}
	var response struct {
		Result *struct {
			Tools      []map[string]any `json:"tools"`
			NextCursor string           `json:"nextCursor"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(payload, &response); err != nil {
		return nil, err
	}
	if response.Result == nil || (len(response.Error) != 0 && string(response.Error) != "null") {
		return nil, fmt.Errorf("upstream discovery returned a JSON-RPC error")
	}
	if response.Result.NextCursor != "" || len(response.Result.Tools) > 1000 {
		// A partial count must not be presented as complete discovery data.
		return nil, fmt.Errorf("discovery requires pagination or exceeds 1000 tools")
	}
	return response.Result.Tools, nil
}

// TargetNames preserves configuration order without exposing upstream URLs,
// transport commands, headers, or token-exchange credentials.
func (h *Handler) TargetNames() []string {
	out := make([]string, 0, len(h.targetOrder))
	for _, key := range h.targetOrder {
		out = append(out, h.targets[key].name)
	}
	return out
}
